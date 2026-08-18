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
	"strconv"
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
	Container string
	UserAgent string

	// SessionCookies, TokenCookies and Cookies count the imported cookies by
	// class. They hold counts only, never a cookie value.
	SessionCookies int
	TokenCookies   int
	Cookies        int
}

// Selector returns the --cookies-from-browser value that selects this session
// again. A profile name can hold a space, so quote it in a printed command.
func (s Session) Selector() string {
	value := s.Browser
	if s.Profile != "" {
		value += ":" + s.Profile
	}
	if s.Container != "" {
		value += "::" + s.Container
	}
	return value
}

var (
	errSnapshotSchema = errors.New("selected browser cookie schema could not be inspected; choose another profile and retry")
	errSnapshotCopy   = errors.New("selected browser cookie store could not be copied; make sure the file exists and the disk has free space")
	// macOS keeps the Safari cookie file behind a privacy control, so a copy
	// fails until the terminal gets Full Disk Access.
	errSnapshotPermission       = errors.New("selected browser cookie store could not be read; give your terminal Full Disk Access in System Settings and retry")
	errSnapshotIntegrity        = errors.New("selected browser cookie data is damaged; choose another profile and retry")
	errFirefoxContainerMetadata = errors.New("firefox container metadata is required for container cookies; restore containers.json and retry")
)

// sqliteSidecarSuffixes names the write-ahead log files that hold the newest
// rows of a live SQLite database.
var sqliteSidecarSuffixes = [...]string{"-wal", "-shm"}

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

	selection, err := selectStore(selector, stores)
	if err != nil {
		return Session{}, err
	}
	selected := selection.store
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

	imported := make(map[string][]*http.Cookie)
	scores := make(candidateGroups)
	for cookie, readErr := range cookies {
		if readErr != nil {
			return Session{}, errors.New("read selected browser cookie store: browser data could not be read; choose another profile")
		}
		if err := ctx.Err(); err != nil {
			return Session{}, err
		}
		if !isPixiesetCookie(cookie) || !matchesContainer(selector, cookie) || !kooky.Valid.Filter(cookie) {
			continue
		}
		entry := cookie.Cookie
		// SetCookies receives galleriesURL, so an empty Domain makes this
		// cookie host-only for galleries.pixieset.com.
		entry.Domain = ""
		imported[cookie.Container] = append(imported[cookie.Container], &entry)
		scores[cookie.Container] = scores[cookie.Container].add(cookie.Name)
	}
	if len(imported) == 0 {
		return Session{}, noPixiesetCookiesError(selection)
	}
	// One container alone fills the jar. Cookies from two containers belong to
	// two different sessions, and a mix of them is a broken session.
	container, score := scores.best()
	jar.SetCookies(galleriesURL, imported[container])
	return Session{
		Jar:            jar,
		Browser:        selected.Browser(),
		Profile:        selected.Profile(),
		Container:      container,
		UserAgent:      userAgent,
		SessionCookies: score.session,
		TokenCookies:   score.token,
		Cookies:        score.total,
	}, nil
}

// noPixiesetCookiesError names every profile that the search examined, so the
// user can see what was found and can select one profile without a guess. It
// gives counts only, and no file path.
func noPixiesetCookiesError(selection storeSelection) error {
	var message strings.Builder
	message.WriteString("no valid Pixieset cookies found; sign in to Pixieset and retry")
	scanned := selection.scanned
	if len(scanned) == 0 {
		scanned = []candidateEvidence{selection.evidence}
	}
	for _, evidence := range scanned {
		if isNilStore(evidence.candidate.store) {
			continue
		}
		message.WriteString("\n  ")
		message.WriteString(evidence.candidate.store.Browser())
		if profile := evidence.candidate.store.Profile(); profile != "" {
			message.WriteString(":" + profile)
		}
		// The container completes the value, so the whole name can go straight
		// back into --cookies-from-browser.
		if evidence.container != "" {
			message.WriteString("::" + evidence.container)
		}
		message.WriteString(" — ")
		switch {
		case evidence.err != nil:
			message.WriteString("the cookie database could not be read")
		case evidence.score.total == 0:
			message.WriteString("0 Pixieset cookies")
		case evidence.score.session == 0:
			fmt.Fprintf(&message, "%d Pixieset cookies, no session cookie", evidence.score.total)
		default:
			fmt.Fprintf(&message, "%d Pixieset cookies", evidence.score.total)
		}
	}
	return errors.New(message.String())
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
	case errors.Is(err, errSnapshotCopy):
		return errSnapshotCopy
	case errors.Is(err, errSnapshotPermission):
		return errSnapshotPermission
	case errors.Is(err, errSnapshotIntegrity):
		return errSnapshotIntegrity
	case errors.Is(err, errFirefoxContainerMetadata):
		return errFirefoxContainerMetadata
	default:
		return errors.New("read selected browser cookie store: browser data could not be opened; choose another profile")
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
		// The sentinels name a cause the user can act on and hold no path.
		if errors.Is(err, errSnapshotCopy) || errors.Is(err, errSnapshotPermission) || errors.Is(err, errSnapshotIntegrity) {
			return nil, "", nil, err
		}
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
		// rows. Its requirement is checked after the snapshot.
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
		if err := snapshotSQLiteDatabase(source, destination); err != nil {
			return "", err
		}
	case "safari":
		if err := copyFile(source, destination); err != nil {
			return "", err
		}
	}
	return destination, nil
}

// snapshotSQLiteDatabase copies a live SQLite database to destination. A byte
// copy takes no lock, so the browser can stay open. The SQLite backup API
// cannot do this: Firefox holds cookies.sqlite in exclusive lock mode.
func snapshotSQLiteDatabase(source, destination string) error {
	if !isRegularFile(source) {
		return errors.New("source is not a regular SQLite database")
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("destination is not fresh")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := copySQLiteSnapshot(source, destination, true); err == nil {
		return nil
	}
	// The browser can write while the copy runs, which leaves a write-ahead log
	// that does not agree with the main file. The main file alone is coherent
	// and holds almost every cookie.
	if err := removeSQLiteSnapshot(destination); err != nil {
		return errSnapshotCopy
	}
	return copySQLiteSnapshot(source, destination, false)
}

// snapshotCopyError names the cause of a failed copy, so that a privacy
// control is not reported as a full disk.
func snapshotCopyError(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return errSnapshotPermission
	}
	return errSnapshotCopy
}

func copySQLiteSnapshot(source, destination string, sidecars bool) error {
	if err := copyFile(source, destination); err != nil {
		return snapshotCopyError(err)
	}
	if err := privatefs.Restrict(destination, false); err != nil {
		return errSnapshotCopy
	}
	if sidecars {
		for _, suffix := range sqliteSidecarSuffixes {
			if err := copySidecarFile(source+suffix, destination+suffix); err != nil {
				return errSnapshotCopy
			}
		}
	}
	if err := recoverSQLiteSnapshot(destination); err != nil {
		return errSnapshotIntegrity
	}
	if err := validateSQLiteIntegrity(destination); err != nil {
		return errSnapshotIntegrity
	}
	return nil
}

func copySidecarFile(source, destination string) error {
	if err := copyOptionalFile(source, destination); err != nil {
		return err
	}
	if !isRegularFile(destination) {
		return nil
	}
	return privatefs.Restrict(destination, false)
}

func removeSQLiteSnapshot(destination string) error {
	names := make([]string, 0, len(sqliteSidecarSuffixes)+1)
	names = append(names, destination)
	for _, suffix := range sqliteSidecarSuffixes {
		names = append(names, destination+suffix)
	}
	var errs []error
	for _, name := range names {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverSQLiteSnapshot folds the copied write-ahead log into the copied main
// file. The connection must be read-write, because SQLite cannot replay a
// write-ahead log on a read-only connection. The later read-only readers then
// see every row.
func recoverSQLiteSnapshot(filename string) error {
	db, err := sqlite3.Open(filename)
	if err != nil {
		return err
	}
	if err := db.BusyTimeout(sqliteBusyTimeout); err != nil {
		_ = db.Close()
		return err
	}
	if err := checkSQLiteIntegrity(db); err != nil {
		_ = db.Close()
		return err
	}
	checkpointErr := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return errors.Join(checkpointErr, db.Close())
}

func validateSQLiteIntegrity(filename string) error {
	db, err := openInspectionDatabase(filename)
	if err != nil {
		return err
	}
	if err := checkSQLiteIntegrity(db); err != nil {
		_ = db.Close()
		return err
	}
	return db.Close()
}

func checkSQLiteIntegrity(db *sqlite3.Conn) error {
	stmt, _, err := db.Prepare("PRAGMA integrity_check")
	if err != nil {
		return err
	}
	ok := stmt.Step() && stmt.ColumnText(0) == "ok" && stmt.Err() == nil
	stepErr := stmt.Err()
	closeErr := stmt.Close()
	if !ok {
		if stepErr != nil {
			return stepErr
		}
		return errors.New("SQLite integrity check failed")
	}
	return closeErr
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
	_, contained := firefoxUserContext(origin)
	return contained
}

// firefoxUserContext reads the container number from one originAttributes
// value. The format is ^key=value&key=value. Container zero is the ordinary
// browsing context, which holds no container.
func firefoxUserContext(origin string) (int, bool) {
	attributes, prefixed := strings.CutPrefix(origin, "^")
	if !prefixed {
		return 0, false
	}
	for _, attribute := range strings.Split(attributes, "&") {
		key, value, separated := strings.Cut(attribute, "=")
		if !separated || key != "userContextId" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			return 0, false
		}
		return number, true
	}
	return 0, false
}

// inspectCandidate counts the Pixieset cookies of one candidate and groups them
// by container. It selects no value column, so macOS asks for no Keychain
// permission before one profile wins.
func inspectCandidate(browser, path string) (candidateGroups, error) {
	if strings.EqualFold(browser, "safari") {
		return inspectSafariCandidate(path)
	}
	// A snapshot holds no containers.json, so the container names always come
	// from the original profile directory.
	containers := firefoxContainerNames(browser, path)
	// The live file needs no copy and takes no lock, so this path stays fast.
	// immutable=1 hides the write-ahead log, so a store whose newest rows live
	// only in that log, or a store that changes during the read, is read again
	// from a snapshot.
	groups, err := inspectDatabase(browser, immutableURI(path), containers)
	if err == nil && (len(groups) > 0 || !hasWriteAheadLog(path)) {
		return groups, nil
	}
	return inspectSnapshotCandidate(browser, path, containers)
}

// immutableURI names one live database as a SQLite URI. The escape keeps a
// space, a question mark or a number sign in the path out of the query.
func immutableURI(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	name := filepath.ToSlash(absolute)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	uri := url.URL{Scheme: "file", Path: name, RawQuery: "immutable=1"}
	return uri.String()
}

func hasWriteAheadLog(path string) bool {
	info, err := os.Stat(path + sqliteSidecarSuffixes[0])
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func inspectSnapshotCandidate(browser, path string, containers map[int]string) (candidateGroups, error) {
	root, err := makeSnapshotTempDir("", "pixiegrabber-cookies-")
	if err != nil {
		return nil, errSnapshotCopy
	}
	defer removeSnapshotDir(root)
	snapshot := filepath.Join(root, filepath.Base(path))
	if err := snapshotSQLiteDatabase(path, snapshot); err != nil {
		return nil, err
	}
	return inspectDatabase(browser, snapshot, containers)
}

// inspectSafariCandidate reads Cookies.binarycookies, which holds no SQLite
// database. Safari keeps its cookie values in the clear, so this read asks for
// no Keychain permission either.
func inspectSafariCandidate(path string) (candidateGroups, error) {
	root, err := makeSnapshotTempDir("", "pixiegrabber-cookies-")
	if err != nil {
		return nil, errSnapshotCopy
	}
	defer removeSnapshotDir(root)
	snapshot := filepath.Join(root, filepath.Base(path))
	if err := copyFile(path, snapshot); err != nil {
		return nil, snapshotCopyError(err)
	}
	store, err := openSnapshotCookieStore("safari", snapshot)
	if err != nil || isNilStore(store) {
		return nil, errSnapshotSchema
	}
	defer store.Close()
	groups := make(candidateGroups)
	for cookie, readErr := range store.TraverseCookies() {
		if readErr != nil {
			return nil, errSnapshotSchema
		}
		if !isPixiesetCookie(cookie) || !kooky.Valid.Filter(cookie) {
			continue
		}
		groups[cookie.Container] = groups[cookie.Container].add(cookie.Name)
	}
	return groups, nil
}

func inspectDatabase(browser, filename string, containers map[int]string) (candidateGroups, error) {
	db, err := openInspectionDatabase(filename)
	if err != nil {
		return nil, errSnapshotSchema
	}
	defer db.Close()
	switch strings.ToLower(browser) {
	case "brave", "chrome", "chromium", "edge":
		return inspectChromiumRows(db, time.Now())
	case "firefox":
		return inspectFirefoxRows(db, time.Now(), containers)
	default:
		return nil, errSnapshotSchema
	}
}

// chromiumEpochOffset converts the Chromium clock, which counts microseconds
// from 1601-01-01, to the Unix clock.
const chromiumEpochOffset = 11644473600000000

func inspectChromiumRows(db *sqlite3.Conn, now time.Time) (candidateGroups, error) {
	columns, err := sqliteTableColumns(db, "cookies")
	if err != nil || !columns["host_key"] || !columns["name"] {
		return nil, errSnapshotSchema
	}
	// The query names no value column, so the store decrypts nothing.
	stmt, _, err := db.Prepare("SELECT host_key, name, " + numberColumn(columns, "expires_utc") + ", " + numberColumn(columns, "is_persistent") + " FROM cookies")
	if err != nil {
		return nil, errSnapshotSchema
	}
	groups := make(candidateGroups)
	for stmt.Step() {
		if !isPixiesetHost(stmt.ColumnText(0)) || chromiumExpired(stmt.ColumnInt64(2), stmt.ColumnInt64(3) != 0, now) {
			continue
		}
		groups[""] = groups[""].add(stmt.ColumnText(1))
	}
	return groups, finishInspection(stmt)
}

func inspectFirefoxRows(db *sqlite3.Conn, now time.Time, containers map[int]string) (candidateGroups, error) {
	columns, err := sqliteTableColumns(db, "moz_cookies")
	if err != nil || !columns["host"] || !columns["name"] {
		return nil, errSnapshotSchema
	}
	stmt, _, err := db.Prepare("SELECT host, name, " + numberColumn(columns, "expiry") + ", " + textColumn(columns, "originAttributes") + " FROM moz_cookies")
	if err != nil {
		return nil, errSnapshotSchema
	}
	groups := make(candidateGroups)
	for stmt.Step() {
		if !isPixiesetHost(stmt.ColumnText(0)) || firefoxExpired(stmt.ColumnInt64(2), now) {
			continue
		}
		container := ""
		if number, contained := firefoxUserContext(stmt.ColumnText(3)); contained {
			container = firefoxContainerName(containers, number)
		}
		groups[container] = groups[container].add(stmt.ColumnText(1))
	}
	return groups, finishInspection(stmt)
}

// firefoxContainerName names one container as the user sees it, so that a
// reported container can go straight back into the selector. kooky keeps the
// number when the metadata names no container, and this rule agrees with it.
func firefoxContainerName(containers map[int]string, number int) string {
	if name := containers[number]; name != "" {
		return name
	}
	return strconv.Itoa(number)
}

func finishInspection(stmt *sqlite3.Stmt) error {
	stepErr := stmt.Err()
	closeErr := stmt.Close()
	if stepErr != nil || closeErr != nil {
		return errSnapshotSchema
	}
	return nil
}

// numberColumn and textColumn keep an older schema readable. The name comes
// from the schema itself, so the query holds no outside text.
func numberColumn(columns map[string]bool, name string) string {
	if columns[name] {
		return name
	}
	return "0"
}

func textColumn(columns map[string]bool, name string) string {
	if columns[name] {
		return name
	}
	return "''"
}

func chromiumExpired(expires int64, persistent bool, now time.Time) bool {
	if !persistent || expires <= 0 {
		return false
	}
	return time.UnixMicro(expires - chromiumEpochOffset).Before(now)
}

// firefoxExpired reads moz_cookies.expiry. Firefox 142 changed the unit from
// seconds to milliseconds, so a row counts as expired only when both readings
// are in the past.
func firefoxExpired(expiry int64, now time.Time) bool {
	if expiry <= 0 {
		return false
	}
	return time.Unix(expiry, 0).Before(now) && time.UnixMilli(expiry).Before(now)
}

func firefoxContainersReadable(snapshotPath string) bool {
	_, err := parseFirefoxContainers(filepath.Join(filepath.Dir(snapshotPath), "containers.json"))
	return err == nil
}

// firefoxContainerLabels names the four built-in containers. Firefox stores
// them as a localization identifier and translates them when it runs, so these
// English labels are the fallback, exactly as kooky does.
var firefoxContainerLabels = map[string]string{
	"user-context-personal": "Personal",
	"user-context-work":     "Work",
	"user-context-banking":  "Banking",
	"user-context-shopping": "Shopping",
}

// firefoxContainerNames reads the container names of one Firefox profile. It
// gives no names for another browser and no names when the metadata is absent
// or damaged, and the container number is then the name.
func firefoxContainerNames(browser, path string) map[int]string {
	if !strings.EqualFold(browser, "firefox") {
		return nil
	}
	names, err := parseFirefoxContainers(filepath.Join(filepath.Dir(path), "containers.json"))
	if err != nil {
		return nil
	}
	return names
}

func parseFirefoxContainers(filename string) (map[int]string, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("firefox container metadata is not a regular file")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		Identities []struct {
			L10nID        *string `json:"l10nID"`
			Name          *string `json:"name"`
			UserContextID int     `json:"userContextId"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	names := make(map[int]string, len(metadata.Identities))
	for _, identity := range metadata.Identities {
		var name string
		// Firefox keeps its own hidden containers in the same list.
		if identity.Name != nil && !strings.HasPrefix(*identity.Name, "userContextIdInternal.") {
			name = *identity.Name
		}
		if name == "" && identity.L10nID != nil {
			name = firefoxContainerLabels[*identity.L10nID]
		}
		if name != "" {
			names[identity.UserContextID] = name
		}
	}
	return names, nil
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
	store     kooky.CookieStore
	key       string
	rank      int
	path      string
	isDefault bool
}

// candidateEvidence holds what one scan found in one candidate. It holds no
// cookie value and no path outside the package.
type candidateEvidence struct {
	candidate storeCandidate
	container string
	score     cookieScore
	modified  time.Time
	err       error
}

// storeSelection names the selected store and keeps the evidence of every
// candidate, so that the caller can report the choice.
type storeSelection struct {
	store    kooky.CookieStore
	evidence candidateEvidence
	scanned  []candidateEvidence
}

func selectStore(selector Selector, stores []kooky.CookieStore) (storeSelection, error) {
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
			return storeSelection{}, fmt.Errorf("no %s cookie store matched the selected profile; check the profile name", browserLabel(selector))
		}
		return storeSelection{}, fmt.Errorf("no %s cookie store was found; sign in to Pixieset in a supported browser and retry", browserLabel(selector))
	}
	candidates := make([]storeCandidate, 0, len(groups))
	for _, group := range groups {
		best, ok := preferredStore(group)
		if !ok {
			// The profile keeps two cookie files of equal age. Drop it and let
			// the other profiles answer, because one strange profile must not
			// stop the whole search.
			continue
		}
		candidates = append(candidates, best)
	}
	if len(candidates) == 0 {
		return storeSelection{}, fmt.Errorf("no %s cookie store could be read; name one profile with BROWSER:PROFILE", browserLabel(selector))
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].path < candidates[j].path })
	// Every candidate is scanned, including a single one, so that the reported
	// evidence is always true.
	return scanCandidates(candidates), nil
}

// browserLabel names the selected browser for a message. It stays empty-safe,
// because the selector does not always name a browser.
func browserLabel(selector Selector) string {
	if selector.hasBrowser() {
		return selector.Browser
	}
	return "browser"
}

// preferredStore keeps one store for each profile. Chromium moved its cookies
// from the profile directory to the Network subdirectory and can leave both
// files in place, so the newer path wins. It returns false when two files of
// equal age make the choice unclear.
func preferredStore(group []storeCandidate) (storeCandidate, bool) {
	best := group[0]
	for _, candidate := range group[1:] {
		if candidate.rank < best.rank || (candidate.rank == best.rank && candidate.path < best.path) {
			best = candidate
		}
	}
	for _, candidate := range group {
		if candidate.rank == best.rank && candidate.path != best.path {
			return storeCandidate{}, false
		}
	}
	return best, true
}

// scanCandidates reads the evidence of every candidate and keeps the strongest
// one. The scan reads no cookie value.
func scanCandidates(candidates []storeCandidate) storeSelection {
	scanned := make([]candidateEvidence, 0, len(candidates))
	for _, candidate := range candidates {
		scanned = append(scanned, inspectStoreCandidate(candidate))
	}
	best := 0
	for index := 1; index < len(scanned); index++ {
		if betterEvidence(scanned[index], scanned[best]) {
			best = index
		}
	}
	return storeSelection{store: scanned[best].candidate.store, evidence: scanned[best], scanned: scanned}
}

func inspectStoreCandidate(candidate storeCandidate) candidateEvidence {
	evidence := candidateEvidence{candidate: candidate}
	if info, err := os.Stat(candidate.path); err == nil {
		evidence.modified = info.ModTime()
	}
	groups, err := inspectCandidate(candidate.store.Browser(), candidate.path)
	if err != nil {
		evidence.err = err
		return evidence
	}
	evidence.container, evidence.score = groups.best()
	return evidence
}

// betterEvidence compares two candidates. The score decides first. A tie keeps
// the store that changed last, then a group with no container, then the default
// profile, then the first path in alphabetical order.
func betterEvidence(evidence, other candidateEvidence) bool {
	if result := evidence.score.compare(other.score); result != 0 {
		return result > 0
	}
	// A store that holds no Pixieset cookie gives no useful time, so the stable
	// rules below decide instead.
	if !evidence.score.empty() && !other.score.empty() && !evidence.modified.Equal(other.modified) {
		return evidence.modified.After(other.modified)
	}
	if (evidence.container == "") != (other.container == "") {
		return evidence.container == ""
	}
	if evidence.candidate.isDefault != other.candidate.isDefault {
		return evidence.candidate.isDefault
	}
	return evidence.candidate.path < other.candidate.path
}

// matchesProfile compares the selected profile with one store. A profile that
// holds a path separator names a directory or a file. Every other profile is a
// profile name, as the browser shows it.
func matchesProfile(selector Selector, store kooky.CookieStore, path string) bool {
	if !selector.profileIsPath {
		return store.Profile() == selector.Profile
	}
	want := filepath.Clean(selector.Profile)
	if want == filepath.Clean(path) || want == filepath.Clean(filepath.Dir(path)) {
		return true
	}
	// Chromium keeps its cookies in a Network subdirectory, so the profile
	// directory is one level higher than the file.
	if profileDir, _, ok := chromiumProfileDirectory(path); ok {
		return want == filepath.Clean(profileDir)
	}
	return false
}

func makeStoreCandidate(selector Selector, store kooky.CookieStore) (storeCandidate, bool) {
	if isNilStore(store) {
		return storeCandidate{}, false
	}
	// An empty browser in the selector searches every supported browser.
	if selector.hasBrowser() && !strings.EqualFold(store.Browser(), selector.Browser) {
		return storeCandidate{}, false
	}
	path := store.FilePath()
	if !isRegularFile(path) {
		return storeCandidate{}, false
	}
	if selector.hasProfile && !matchesProfile(selector, store, path) {
		return storeCandidate{}, false
	}
	candidate := storeCandidate{store: store, path: filepath.Clean(path), rank: 0}
	switch strings.ToLower(store.Browser()) {
	case "brave", "chrome", "chromium", "edge":
		profileDir, rank, ok := chromiumProfileDirectory(path)
		if !ok {
			return storeCandidate{}, false
		}
		candidate.key = filepath.Clean(profileDir)
		candidate.rank = rank
		// Chromium names the directory of its first profile "Default". kooky can
		// mark another profile as the default one, so the directory decides.
		candidate.isDefault = filepath.Base(profileDir) == "Default"
	case "firefox":
		if filepath.Base(path) != "cookies.sqlite" {
			return storeCandidate{}, false
		}
		candidate.key = filepath.Clean(filepath.Dir(path))
		candidate.isDefault = store.IsDefaultProfile()
	case "safari":
		candidate.key = candidate.path
		candidate.isDefault = store.IsDefaultProfile()
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
	return isPixiesetHost(cookie.Domain)
}

func isPixiesetHost(host string) bool {
	switch strings.ToLower(host) {
	case ".pixieset.com", ".galleries.pixieset.com", galleriesURL.Host:
		return true
	default:
		return false
	}
}

// matchesContainer accepts every container when the user names none, because
// the score then selects one container. The name "none" keeps its meaning of
// "no container". A named container also applies when the selector names no
// browser, because only Firefox then gives a cookie a container.
func matchesContainer(selector Selector, cookie *kooky.Cookie) bool {
	if !selector.hasContainer {
		return true
	}
	if cookie == nil {
		return false
	}
	if strings.EqualFold(selector.Container, "none") {
		return cookie.Container == ""
	}
	return cookie.Container == selector.Container
}

// cookieFilters runs before the store decrypts a value, so a cookie of another
// domain never reaches the macOS Keychain. matchesContainer removes a container
// cookie only when the user names a container, so every container reaches the
// score.
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
