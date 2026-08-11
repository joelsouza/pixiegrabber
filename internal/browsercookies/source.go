package browsercookies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/browserutils/kooky"
	"github.com/browserutils/kooky/browser/brave"
	"github.com/browserutils/kooky/browser/chrome"
	"github.com/browserutils/kooky/browser/chromium"
	"github.com/browserutils/kooky/browser/edge"
	"github.com/browserutils/kooky/browser/firefox"
	"github.com/browserutils/kooky/browser/safari"
	sqlite3 "github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/embed"
	"pixiegrabber/internal/privatefs"
)

var galleriesURL = &url.URL{Scheme: "https", Host: "galleries.pixieset.com", Path: "/"}

const (
	sqliteBusyTimeout   = 5 * time.Second
	maxRegularCopyBytes = 256 << 20
)

var (
	makeSnapshotTempDir = privatefs.MkdirTemp
	removeSnapshotDir   = os.RemoveAll
)

// Session contains an in-memory cookie jar and non-secret source metadata.
type Session struct {
	Jar       http.CookieJar
	Browser   string
	Profile   string
	UserAgent string
}

var (
	errSnapshotSchema           = errors.New("selected browser cookie schema could not be inspected; close the browser and retry")
	errFirefoxContainerMetadata = errors.New("Firefox container metadata is required for container cookies; restore containers.json and retry")
)

// Load imports valid Pixieset cookies from one selected browser profile.
func Load(ctx context.Context, value string) (Session, error) {
	selector, err := ParseSelector(value)
	if err != nil {
		return Session{}, err
	}
	return loadWith(ctx, selector, kooky.TraverseCookieStores(ctx), snapshotReader)
}

func load(ctx context.Context, selector Selector, sequence kooky.CookieStoreSeq) (session Session, err error) {
	return loadWith(ctx, selector, sequence, liveReader)
}

type cookieReader func(kooky.CookieStore, ...kooky.Filter) (kooky.CookieSeq, func() error, error)

func loadWith(ctx context.Context, selector Selector, sequence kooky.CookieStoreSeq, reader cookieReader) (session Session, err error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	stores, err := collectStores(ctx, sequence)
	defer func() {
		if closeErr := closeStores(stores); closeErr != nil {
			session = Session{}
			err = errors.Join(err, closeErr)
		}
	}()
	if err != nil {
		return Session{}, err
	}

	selected, err := selectStore(selector, stores)
	if err != nil {
		return Session{}, err
	}
	userAgent := detectUserAgent(ctx, selected)
	cookies, cleanup, err := reader(selected, cookieFilters(selector)...)
	if err != nil {
		return Session{}, sanitizeReaderError(err)
	}
	if cleanup == nil {
		return Session{}, errors.New("read selected browser cookie store: cleanup was unavailable")
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			session = Session{}
			err = errors.Join(err, errors.New("remove temporary browser cookie snapshot: cleanup failed"))
		}
	}()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return Session{}, errors.New("create in-memory cookie jar: initialization failed")
	}

	count := 0
	for cookie, readErr := range cookies {
		if readErr != nil {
			return Session{}, errors.New("read selected browser cookie store: browser data could not be read; close the browser or choose another profile")
		}
		if err := ctx.Err(); err != nil {
			return Session{}, err
		}
		if !isPixiesetCookie(cookie) || !matchesContainer(selector, cookie) || !kooky.Valid.Filter(cookie) {
			continue
		}
		copy := cookie.Cookie
		// SetCookies receives galleriesURL, so an empty Domain makes this
		// cookie host-only for galleries.pixieset.com.
		copy.Domain = ""
		jar.SetCookies(galleriesURL, []*http.Cookie{&copy})
		count++
	}
	if count == 0 {
		return Session{}, errors.New("no valid Pixieset cookies found in the selected profile; sign in to Pixieset and retry")
	}
	return Session{Jar: jar, Browser: selected.Browser(), Profile: selected.Profile(), UserAgent: userAgent}, nil
}

func liveReader(store kooky.CookieStore, filters ...kooky.Filter) (kooky.CookieSeq, func() error, error) {
	return store.TraverseCookies(filters...), func() error { return nil }, nil
}

func snapshotReader(source kooky.CookieStore, filters ...kooky.Filter) (kooky.CookieSeq, func() error, error) {
	store, snapshotPath, remove, err := snapshotStoreWithPath(source, openSnapshotCookieStore)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error {
		closeErr := store.Close()
		removeErr := remove()
		if closeErr != nil || removeErr != nil {
			return errors.New("remove temporary browser cookie snapshot: cleanup failed")
		}
		return nil
	}

	snapshotFilters := append([]kooky.Filter(nil), filters...)
	switch strings.ToLower(source.Browser()) {
	case "brave", "chrome", "chromium", "edge":
		partitionFilter, inspectErr := chromiumPartitionFilter(snapshotPath)
		if inspectErr != nil {
			_ = cleanup()
			return nil, nil, inspectErr
		}
		if partitionFilter != nil {
			snapshotFilters = append([]kooky.Filter{partitionFilter}, snapshotFilters...)
		}
	case "firefox":
		containerRows, inspectErr := firefoxContainerRows(snapshotPath)
		if inspectErr != nil {
			_ = cleanup()
			return nil, nil, inspectErr
		}
		if containerRows && !firefoxContainersReadable(snapshotPath) {
			_ = cleanup()
			return nil, nil, errFirefoxContainerMetadata
		}
	}
	return store.TraverseCookies(snapshotFilters...), cleanup, nil
}

func sanitizeReaderError(err error) error {
	switch {
	case errors.Is(err, errSnapshotSchema):
		return errSnapshotSchema
	case errors.Is(err, errFirefoxContainerMetadata):
		return errFirefoxContainerMetadata
	default:
		return errors.New("read selected browser cookie store: browser data could not be opened; close the browser or choose another profile")
	}
}

type storeOpener func(browser, filename string) (kooky.CookieStore, error)

func snapshotStoreWith(source kooky.CookieStore, open storeOpener) (kooky.CookieStore, func() error, error) {
	store, _, remove, err := snapshotStoreWithPath(source, open)
	return store, remove, err
}

func snapshotStoreWithPath(source kooky.CookieStore, open storeOpener) (kooky.CookieStore, string, func() error, error) {
	if isNilStore(source) || open == nil {
		return nil, "", nil, errors.New("create temporary browser cookie snapshot: snapshot source is unavailable")
	}
	root, err := makeSnapshotTempDir("", "pixiegrabber-cookies-")
	if err != nil {
		return nil, "", nil, errors.New("create temporary browser cookie snapshot: temporary directory creation failed")
	}
	remove := func() error { return removeSnapshotDir(root) }
	snapshotPath, err := copyCookieStore(source.Browser(), source.FilePath(), root)
	if err != nil {
		_ = remove()
		return nil, "", nil, errors.New("copy selected browser cookie store: snapshot could not be created")
	}
	store, err := open(source.Browser(), snapshotPath)
	if err != nil {
		_ = remove()
		return nil, "", nil, errors.New("open temporary browser cookie snapshot: copied data could not be opened")
	}
	if store == nil {
		_ = remove()
		return nil, "", nil, errors.New("open temporary browser cookie snapshot: copied data could not be opened")
	}
	return store, snapshotPath, remove, nil
}

func copyCookieStore(browser, source, root string) (string, error) {
	if source == "" {
		return "", errors.New("cookie store path is empty")
	}
	var destinationDir string
	switch strings.ToLower(browser) {
	case "brave", "chrome", "chromium", "edge":
		destinationDir = filepath.Join(root, "Profile")
		sourceProfileDir := filepath.Dir(source)
		if filepath.Base(sourceProfileDir) == "Network" {
			destinationDir = filepath.Join(destinationDir, "Network")
			sourceProfileDir = filepath.Dir(sourceProfileDir)
		}
		localState := filepath.Join(filepath.Dir(sourceProfileDir), "Local State")
		if err := copyOptionalFile(localState, filepath.Join(root, "Local State")); err != nil {
			return "", err
		}
	case "firefox":
		destinationDir = filepath.Join(root, "Profile")
		containers := filepath.Join(filepath.Dir(source), "containers.json")
		// Container metadata is optional when the database has no container
		// rows. Its requirement is checked after the coherent backup.
		_ = copyOptionalFile(containers, filepath.Join(destinationDir, "containers.json"))
	case "safari":
		destinationDir = root
	default:
		return "", errors.New("unsupported browser")
	}
	if err := os.MkdirAll(destinationDir, 0700); err != nil {
		return "", err
	}
	destination := filepath.Join(destinationDir, filepath.Base(source))
	switch strings.ToLower(browser) {
	case "brave", "chrome", "chromium", "edge", "firefox":
		if err := backupSQLiteDatabase(source, destination); err != nil {
			return "", err
		}
	case "safari":
		if err := copyFile(source, destination); err != nil {
			return "", err
		}
	}
	return destination, nil
}

func backupSQLiteDatabase(source, destination string) error {
	if !isRegularFile(source) {
		return errors.New("source is not a regular SQLite database")
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("destination is not fresh")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	db, err := sqlite3.OpenFlags(source, sqlite3.OPEN_READONLY|sqlite3.OPEN_URI)
	if err != nil {
		return err
	}
	if err := db.BusyTimeout(sqliteBusyTimeout); err != nil {
		_ = db.Close()
		return err
	}
	backupErr := db.Backup("main", destination)
	closeErr := db.Close()
	if backupErr != nil {
		return backupErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := privatefs.Restrict(destination, false); err != nil {
		return err
	}
	return validateSQLiteIntegrity(destination)
}

func validateSQLiteIntegrity(filename string) error {
	db, err := sqlite3.OpenFlags(filename, sqlite3.OPEN_READONLY|sqlite3.OPEN_URI)
	if err != nil {
		return err
	}
	if err := db.BusyTimeout(sqliteBusyTimeout); err != nil {
		_ = db.Close()
		return err
	}
	stmt, _, err := db.Prepare("PRAGMA integrity_check")
	if err != nil {
		_ = db.Close()
		return err
	}
	ok := stmt.Step() && stmt.ColumnText(0) == "ok" && stmt.Err() == nil
	stepErr := stmt.Err()
	closeStmtErr := stmt.Close()
	closeDBErr := db.Close()
	if !ok {
		if stepErr != nil {
			return stepErr
		}
		return errors.New("SQLite integrity check failed")
	}
	return errors.Join(closeStmtErr, closeDBErr)
}

type cookieIdentity struct {
	host string
	name string
	path string
}

func chromiumPartitionFilter(filename string) (kooky.Filter, error) {
	db, err := openInspectionDatabase(filename)
	if err != nil {
		return nil, errSnapshotSchema
	}
	defer db.Close()
	columns, err := sqliteTableColumns(db, "cookies")
	if err != nil {
		return nil, errSnapshotSchema
	}
	if !columns["top_frame_site_key"] {
		return nil, nil
	}
	stmt, _, err := db.Prepare("SELECT host_key, name, path, top_frame_site_key FROM cookies")
	if err != nil {
		return nil, errSnapshotSchema
	}
	partitioned := make(map[cookieIdentity]struct{})
	for stmt.Step() {
		if stmt.ColumnText(3) == "" {
			continue
		}
		partitioned[cookieIdentity{
			host: stmt.ColumnText(0),
			name: stmt.ColumnText(1),
			path: stmt.ColumnText(2),
		}] = struct{}{}
	}
	stepErr := stmt.Err()
	closeErr := stmt.Close()
	if stepErr != nil || closeErr != nil {
		return nil, errSnapshotSchema
	}
	if len(partitioned) == 0 {
		return nil, nil
	}
	return kooky.FilterFunc(func(cookie *kooky.Cookie) bool {
		if cookie == nil {
			return false
		}
		_, reject := partitioned[cookieIdentity{host: cookie.Domain, name: cookie.Name, path: cookie.Path}]
		return !reject
	}), nil
}

func firefoxContainerRows(filename string) (bool, error) {
	db, err := openInspectionDatabase(filename)
	if err != nil {
		return false, errSnapshotSchema
	}
	defer db.Close()
	columns, err := sqliteTableColumns(db, "moz_cookies")
	if err != nil {
		return false, errSnapshotSchema
	}
	if !columns["originAttributes"] {
		return false, nil
	}
	stmt, _, err := db.Prepare("SELECT originAttributes FROM moz_cookies")
	if err != nil {
		return false, errSnapshotSchema
	}
	for stmt.Step() {
		if firefoxUserContextOrigin(stmt.ColumnText(0)) {
			if err := stmt.Close(); err != nil {
				return false, errSnapshotSchema
			}
			return true, nil
		}
	}
	stepErr := stmt.Err()
	closeErr := stmt.Close()
	if stepErr != nil || closeErr != nil {
		return false, errSnapshotSchema
	}
	return false, nil
}

func firefoxUserContextOrigin(origin string) bool {
	return origin != "" && strings.Contains(origin, "userContextId")
}

func firefoxContainersReadable(snapshotPath string) bool {
	filename := filepath.Join(filepath.Dir(snapshotPath), "containers.json")
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(filename)
	if err != nil || !json.Valid(data) {
		return false
	}
	var metadata struct {
		Identities []struct {
			UserContextID int `json:"userContextId"`
		} `json:"identities"`
	}
	return json.Unmarshal(data, &metadata) == nil
}

func openInspectionDatabase(filename string) (*sqlite3.Conn, error) {
	db, err := sqlite3.OpenFlags(filename, sqlite3.OPEN_READONLY|sqlite3.OPEN_URI)
	if err != nil {
		return nil, err
	}
	if err := db.BusyTimeout(sqliteBusyTimeout); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func sqliteTableColumns(db *sqlite3.Conn, table string) (map[string]bool, error) {
	if table != "cookies" && table != "moz_cookies" {
		return nil, errors.New("unsupported SQLite table")
	}
	stmt, _, err := db.Prepare("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	columns := make(map[string]bool)
	for stmt.Step() {
		columns[stmt.ColumnText(1)] = true
	}
	stepErr := stmt.Err()
	closeErr := stmt.Close()
	if stepErr != nil || closeErr != nil || len(columns) == 0 {
		return nil, errors.New("SQLite table schema is unavailable")
	}
	return columns, nil
}

func copyOptionalFile(source, destination string) error {
	_, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	return copyFile(source, destination)
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxRegularCopyBytes {
		return errors.New("source is not a regular file")
	}
	output, err := privatefs.OpenNew(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(output, input, info.Size())
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func openCookieStore(browser, filename string) (kooky.CookieStore, error) {
	switch strings.ToLower(browser) {
	case "brave":
		return brave.CookieStore(filename)
	case "chrome":
		return chrome.CookieStore(filename)
	case "chromium":
		return chromium.CookieStore(filename)
	case "edge":
		return edge.CookieStore(filename)
	case "firefox":
		return firefox.CookieStore(filename)
	case "safari":
		return safari.CookieStore(filename)
	default:
		return nil, errors.New("unsupported browser")
	}
}

var openSnapshotCookieStore storeOpener = openCookieStore

func collectStores(ctx context.Context, sequence kooky.CookieStoreSeq) ([]kooky.CookieStore, error) {
	if sequence == nil {
		return nil, nil
	}
	stores := make([]kooky.CookieStore, 0)
	for store, err := range sequence {
		if err != nil {
			// Kooky discovers independent browser roots concurrently. One missing
			// root must not hide a usable profile from another root.
			continue
		}
		if err := ctx.Err(); err != nil {
			return stores, err
		}
		if !isNilStore(store) {
			stores = append(stores, store)
		}
	}
	if err := ctx.Err(); err != nil {
		return stores, err
	}
	return stores, nil
}

type storeCandidate struct {
	store kooky.CookieStore
	key   string
	rank  int
	path  string
}

func selectStore(selector Selector, stores []kooky.CookieStore) (kooky.CookieStore, error) {
	groups := make(map[string][]storeCandidate)
	for _, store := range stores {
		candidate, ok := makeStoreCandidate(selector, store)
		if !ok {
			continue
		}
		groups[candidate.key] = append(groups[candidate.key], candidate)
	}
	if len(groups) == 0 {
		if selector.hasProfile {
			return nil, fmt.Errorf("no %s cookie store matched the selected profile; check the profile name", selector.Browser)
		}
		return nil, fmt.Errorf("no %s cookie store has a usable default profile; specify one with %s:PROFILE", selector.Browser, selector.Browser)
	}
	if len(groups) > 1 {
		return nil, fmt.Errorf("multiple %s cookie stores matched; specify one profile with %s:PROFILE", selector.Browser, selector.Browser)
	}

	var group []storeCandidate
	for _, candidates := range groups {
		group = candidates
	}
	bestRank := group[0].rank
	for _, candidate := range group[1:] {
		if candidate.rank < bestRank {
			bestRank = candidate.rank
		}
	}
	best := make([]storeCandidate, 0, len(group))
	paths := make(map[string]struct{})
	for _, candidate := range group {
		if candidate.rank == bestRank {
			best = append(best, candidate)
			paths[candidate.path] = struct{}{}
		}
	}
	if len(paths) > 1 {
		return nil, fmt.Errorf("multiple %s cookie stores matched; specify one profile with %s:PROFILE", selector.Browser, selector.Browser)
	}
	sort.Slice(best, func(i, j int) bool { return best[i].path < best[j].path })
	return best[0].store, nil
}

func makeStoreCandidate(selector Selector, store kooky.CookieStore) (storeCandidate, bool) {
	if isNilStore(store) || !strings.EqualFold(store.Browser(), selector.Browser) {
		return storeCandidate{}, false
	}
	path := store.FilePath()
	if !isRegularFile(path) {
		return storeCandidate{}, false
	}
	candidate := storeCandidate{store: store, path: filepath.Clean(path), rank: 0}
	switch strings.ToLower(selector.Browser) {
	case "brave", "chrome", "chromium", "edge":
		profileDir, rank, ok := chromiumProfileDirectory(path)
		if !ok {
			return storeCandidate{}, false
		}
		if selector.hasProfile {
			if store.Profile() != selector.Profile {
				return storeCandidate{}, false
			}
		} else if filepath.Base(profileDir) != "Default" {
			return storeCandidate{}, false
		}
		candidate.key = filepath.Clean(profileDir)
		candidate.rank = rank
	case "firefox":
		if filepath.Base(path) != "cookies.sqlite" {
			return storeCandidate{}, false
		}
		if selector.hasProfile {
			if store.Profile() != selector.Profile {
				return storeCandidate{}, false
			}
		} else if !store.IsDefaultProfile() {
			return storeCandidate{}, false
		}
		candidate.key = filepath.Clean(filepath.Dir(path))
	case "safari":
		if selector.hasProfile {
			if store.Profile() != selector.Profile {
				return storeCandidate{}, false
			}
		} else if !store.IsDefaultProfile() {
			return storeCandidate{}, false
		}
		candidate.key = candidate.path
	default:
		return storeCandidate{}, false
	}
	return candidate, true
}

func chromiumProfileDirectory(path string) (string, int, bool) {
	if filepath.Base(path) != "Cookies" {
		return "", 0, false
	}
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "Network" {
		return filepath.Dir(dir), 0, true
	}
	return dir, 1, true
}

func isRegularFile(filename string) bool {
	info, err := os.Stat(filename)
	return err == nil && info.Mode().IsRegular()
}

func isPixiesetCookie(cookie *kooky.Cookie) bool {
	if cookie == nil || cookie.MaxAge < 0 || cookie.Partitioned {
		return false
	}
	switch strings.ToLower(cookie.Domain) {
	case ".pixieset.com", ".galleries.pixieset.com", galleriesURL.Host:
		return true
	default:
		return false
	}
}

func matchesContainer(selector Selector, cookie *kooky.Cookie) bool {
	if !strings.EqualFold(selector.Browser, "firefox") {
		return true
	}
	if !selector.hasContainer {
		return cookie != nil && cookie.Container == ""
	}
	if strings.EqualFold(selector.Container, "none") {
		return cookie != nil && cookie.Container == ""
	}
	return cookie != nil && cookie.Container == selector.Container
}

func cookieFilters(selector Selector) []kooky.Filter {
	return []kooky.Filter{
		kooky.FilterFunc(func(cookie *kooky.Cookie) bool {
			return isPixiesetCookie(cookie) && matchesContainer(selector, cookie)
		}),
		kooky.Valid,
	}
}

func closeStores(stores []kooky.CookieStore) error {
	failed := false
	seen := make(map[storeIdentity]struct{})
	for _, store := range stores {
		if isNilStore(store) {
			continue
		}
		identity, identifiable := identifyStore(store)
		if identifiable {
			if _, alreadyClosed := seen[identity]; alreadyClosed {
				continue
			}
			seen[identity] = struct{}{}
		}
		if err := store.Close(); err != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("close browser cookie stores: one or more stores could not be closed")
	}
	return nil
}

type storeIdentity struct {
	typeOf reflect.Type
	ptr    uintptr
}

func isNilStore(store kooky.CookieStore) bool {
	if store == nil {
		return true
	}
	value := reflect.ValueOf(store)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func identifyStore(store kooky.CookieStore) (storeIdentity, bool) {
	if isNilStore(store) {
		return storeIdentity{}, false
	}
	value := reflect.ValueOf(store)
	if value.Kind() != reflect.Pointer {
		return storeIdentity{}, false
	}
	return storeIdentity{typeOf: value.Type(), ptr: value.Pointer()}, true
}
