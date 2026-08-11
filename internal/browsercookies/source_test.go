package browsercookies

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/browserutils/kooky"
	sqlite3 "github.com/ncruces/go-sqlite3"
)

func TestMain(m *testing.M) {
	status := m.Run()
	if fakeStoreRoot != "" {
		_ = os.RemoveAll(fakeStoreRoot)
	}
	os.Exit(status)
}

func TestLoadSelectsDefaultStoreFiltersCookiesAndClosesAllStores(t *testing.T) {
	selected := &fakeStore{
		browser:   "chrome",
		profile:   "Default",
		isDefault: true,
		cookies: []*kooky.Cookie{
			cookie("session", "secret", ".pixieset.com", "", time.Time{}),
			cookie("host", "host-secret", "galleries.pixieset.com", "", time.Now().Add(time.Hour)),
			cookie("expired", "expired-secret", ".pixieset.com", "", time.Now().Add(-time.Hour)),
			cookie("other", "other-secret", ".example.com", "", time.Time{}),
			cookie("root-host", "root-secret", "pixieset.com", "", time.Time{}),
			cookie("partitioned", "partitioned-secret", ".pixieset.com", "", time.Time{}),
		},
	}
	selected.cookies = append(selected.cookies, cookie("nested", "nested-secret", ".galleries.pixieset.com", "", time.Time{}), cookie("host-only", "host-only-secret", "galleries.pixieset.com", "", time.Time{}))
	selected.cookies[len(selected.cookies)-3].Partitioned = true
	unmatched := &fakeStore{browser: "firefox", profile: "default-release", isDefault: true}

	session, err := load(context.Background(), Selector{Browser: "chrome"}, stores(selected, unmatched))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if !selected.closed || !unmatched.closed {
		t.Fatalf("stores closed = %v, %v; want both closed", selected.closed, unmatched.closed)
	}
	if session.Browser != "chrome" || session.Profile != "Default" {
		t.Fatalf("session metadata = %#v", session)
	}

	galleryURL := mustURL(t, "https://galleries.pixieset.com/api/v1/dashboard_listings")
	got := session.Jar.Cookies(galleryURL)
	if len(got) != 4 || got[0].Name != "session" || got[1].Name != "host" || got[2].Name != "nested" || got[3].Name != "host-only" {
		t.Fatalf("gallery cookies = %#v", got)
	}
	if leaked := session.Jar.Cookies(mustURL(t, "https://store.pixieset.com/")); len(leaked) != 0 {
		t.Fatalf("cookies leaked to another Pixieset host: %#v", leaked)
	}
}

func TestLoadPopulatesUserAgentAndImportsCookiesWithoutMetadata(t *testing.T) {
	t.Run("detected", func(t *testing.T) {
		root := t.TempDir()
		cookiePath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
		writeRegularFile(t, cookiePath)
		writeRegularFileWithContents(t, filepath.Join(root, "User Data", "Last Version"), "123.45.6.7")
		selected := &fakeStore{
			browser:   "chrome",
			profile:   "Default",
			isDefault: true,
			filePath:  cookiePath,
			cookies:   []*kooky.Cookie{cookie("session", "secret", ".pixieset.com", "", time.Time{})},
		}
		session, err := load(context.Background(), Selector{Browser: "chrome"}, stores(selected))
		if err != nil {
			t.Fatal(err)
		}
		want := "Mozilla/5.0 (" + userAgentPlatform(runtime.GOOS) + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.45.6.7 Safari/537.36"
		if session.UserAgent != want {
			t.Fatalf("Session.UserAgent = %q, want %q", session.UserAgent, want)
		}
		if strings.Contains(session.UserAgent, root) || !strings.Contains(session.UserAgent, "123.45.6.7") {
			t.Fatalf("Session.UserAgent metadata = %q", session.UserAgent)
		}
	})

	t.Run("metadata unavailable", func(t *testing.T) {
		root := t.TempDir()
		cookiePath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
		writeRegularFile(t, cookiePath)
		selected := &fakeStore{
			browser:   "chrome",
			profile:   "Default",
			isDefault: true,
			filePath:  cookiePath,
			cookies:   []*kooky.Cookie{cookie("session", "secret", ".pixieset.com", "", time.Time{})},
		}
		session, err := load(context.Background(), Selector{Browser: "chrome"}, stores(selected))
		if err != nil {
			t.Fatal(err)
		}
		if session.UserAgent != "" {
			t.Fatalf("Session.UserAgent = %q, want empty", session.UserAgent)
		}
		if len(session.Jar.Cookies(mustURL(t, "https://galleries.pixieset.com/"))) != 1 {
			t.Fatal("cookie import failed when User-Agent detection was unavailable")
		}
	})
}

func TestLoadDoesNotExposeProfilePathsOrMetadataContents(t *testing.T) {
	root := t.TempDir()
	secret := "profile-metadata-secret"
	cookiePath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
	writeRegularFile(t, cookiePath)
	writeRegularFileWithContents(t, filepath.Join(root, "User Data", "Last Version"), secret)
	selected := &fakeStore{
		browser:   "chrome",
		profile:   "Default",
		isDefault: true,
		filePath:  cookiePath,
		readErr:   errors.New(cookiePath + ": " + secret),
	}
	_, err := load(context.Background(), Selector{Browser: "chrome"}, stores(selected))
	if err == nil || strings.Contains(err.Error(), cookiePath) || strings.Contains(err.Error(), secret) {
		t.Fatalf("load() exposed profile metadata: %v", err)
	}
}

func TestLoadContinuesAfterSanitizedDiscoveryError(t *testing.T) {
	selected := &fakeStore{
		browser: "chrome",
		profile: "Default",
		cookies: []*kooky.Cookie{cookie("session", "secret", ".pixieset.com", "", time.Time{})},
	}
	unmatched := &fakeStore{browser: "firefox", profile: "default-release"}
	sequence := func(yield func(kooky.CookieStore, error) bool) {
		if !yield(nil, errors.New("missing /private/source/profile")) {
			return
		}
		if !yield(selected, nil) || !yield(unmatched, nil) {
			return
		}
	}
	if _, err := load(context.Background(), Selector{Browser: "chrome"}, sequence); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if !selected.closed || !unmatched.closed {
		t.Fatalf("stores were not all closed: selected=%v unmatched=%v", selected.closed, unmatched.closed)
	}

	badSequence := func(yield func(kooky.CookieStore, error) bool) {
		yield(nil, errors.New("raw discovery path"))
	}
	_, err := load(context.Background(), Selector{Browser: "chrome"}, badSequence)
	if err == nil || !strings.Contains(err.Error(), "no chrome cookie store") || strings.Contains(err.Error(), "raw discovery path") {
		t.Fatalf("sanitized discovery error = %v", err)
	}
}

func TestSelectStoreGroupsPhysicalStoresAndUsesTrueChromiumDefault(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, "User Data", "Default", "Cookies")
	networkPath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
	otherPath := filepath.Join(root, "User Data", "Profile 1", "Network", "Cookies")
	for _, path := range []string{legacyPath, networkPath, otherPath} {
		writeRegularFile(t, path)
	}
	legacy := &fakeStore{browser: "chrome", profile: "Default", isDefault: false, filePath: legacyPath}
	network := &fakeStore{browser: "chrome", profile: "Default", isDefault: false, filePath: networkPath}
	other := &fakeStore{browser: "chrome", profile: "Profile 1", isDefault: true, filePath: otherPath}
	got, err := selectStore(Selector{Browser: "chrome"}, []kooky.CookieStore{other, legacy, network})
	if err != nil {
		t.Fatalf("selectStore() error = %v", err)
	}
	if got != network {
		t.Fatalf("selected store = %p, want Network/Cookies store %p", got, network)
	}
}

func TestSelectStoreIgnoresFirefoxSessionStore(t *testing.T) {
	root := t.TempDir()
	sqlitePath := filepath.Join(root, "cookies.sqlite")
	sessionPath := filepath.Join(root, "sessionstore.jsonlz4")
	writeSQLiteDatabase(t, sqlitePath)
	writeRegularFile(t, sessionPath)
	sqliteStore := &fakeStore{browser: "firefox", profile: "work", filePath: sqlitePath}
	sessionStore := &fakeStore{browser: "firefox", profile: "work", filePath: sessionPath}
	got, err := selectStore(Selector{Browser: "firefox", Profile: "work", hasProfile: true}, []kooky.CookieStore{sessionStore, sqliteStore})
	if err != nil {
		t.Fatalf("selectStore() error = %v", err)
	}
	if got != sqliteStore {
		t.Fatalf("selected store = %p, want SQLite store %p", got, sqliteStore)
	}
}

func TestLoadSelectsExplicitProfileAndFirefoxContainer(t *testing.T) {
	wrong := &fakeStore{browser: "firefox", profile: "default-release", isDefault: true}
	selected := &fakeStore{
		browser: "firefox",
		profile: "work",
		cookies: []*kooky.Cookie{
			cookie("personal", "personal-secret", ".pixieset.com", "Personal", time.Time{}),
			cookie("work", "work-secret", ".pixieset.com", "Work", time.Time{}),
			cookie("plain", "plain-secret", ".pixieset.com", "", time.Time{}),
		},
	}
	selector, err := ParseSelector("firefox:work::Work")
	if err != nil {
		t.Fatal(err)
	}
	session, err := load(context.Background(), selector, stores(wrong, selected))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	got := session.Jar.Cookies(mustURL(t, "https://galleries.pixieset.com/"))
	if len(got) != 1 || got[0].Name != "work" {
		t.Fatalf("container cookies = %#v", got)
	}
}

func TestLoadTreatsFirefoxNoneAsNoContainer(t *testing.T) {
	selected := &fakeStore{
		browser:   "firefox",
		profile:   "default-release",
		isDefault: true,
		cookies: []*kooky.Cookie{
			cookie("plain", "plain-secret", ".pixieset.com", "", time.Time{}),
			cookie("work", "work-secret", ".pixieset.com", "Work", time.Time{}),
		},
	}
	selector, err := ParseSelector("firefox::none")
	if err != nil {
		t.Fatal(err)
	}
	session, err := load(context.Background(), selector, stores(selected))
	if err != nil {
		t.Fatal(err)
	}
	got := session.Jar.Cookies(mustURL(t, "https://galleries.pixieset.com/"))
	if len(got) != 1 || got[0].Name != "plain" {
		t.Fatalf("no-container cookies = %#v", got)
	}
}

func TestLoadFirefoxWithoutContainerSelectsOnlyUncontainedCookies(t *testing.T) {
	selected := &fakeStore{
		browser:   "firefox",
		profile:   "default-release",
		isDefault: true,
		cookies: []*kooky.Cookie{
			cookie("plain", "plain-secret", ".pixieset.com", "", time.Time{}),
			cookie("work", "work-secret", ".pixieset.com", "Work", time.Time{}),
		},
	}
	session, err := load(context.Background(), Selector{Browser: "firefox"}, stores(selected))
	if err != nil {
		t.Fatal(err)
	}
	got := session.Jar.Cookies(mustURL(t, "https://galleries.pixieset.com/"))
	if len(got) != 1 || got[0].Name != "plain" {
		t.Fatalf("default Firefox cookies = %#v", got)
	}
}

func TestLoadPassesDomainAndContainerFiltersBeforeValueDecryption(t *testing.T) {
	selected := &filterAwareStore{fakeStore: fakeStore{
		browser: "chrome",
		profile: "Default",
		cookies: []*kooky.Cookie{
			cookie("other", "other-secret", ".example.com", "", time.Time{}),
			cookie("wanted", "wanted-secret", ".pixieset.com", "", time.Time{}),
		},
	}}
	if _, err := load(context.Background(), Selector{Browser: "chrome"}, stores(selected)); err != nil {
		t.Fatal(err)
	}
	if selected.decryptions != 1 {
		t.Fatalf("value decryptions = %d, want one accepted cookie", selected.decryptions)
	}
}

func TestLoadRejectsMissingAmbiguousAndUnreadableStoresWithoutSecrets(t *testing.T) {
	otherDefaultPath := filepath.Join(t.TempDir(), "Other User Data", "Default", "Network", "Cookies")
	tests := []struct {
		name   string
		stores kooky.CookieStoreSeq
		want   string
	}{
		{name: "missing", stores: stores(&fakeStore{browser: "firefox", profile: "default-release", isDefault: true}), want: "no chrome cookie store"},
		{name: "ambiguous defaults", stores: stores(&fakeStore{browser: "chrome", profile: "Default", isDefault: true}, &fakeStore{browser: "chrome", profile: "Profile 1", isDefault: true, filePath: otherDefaultPath}), want: "multiple chrome cookie stores"},
		{name: "read failure", stores: stores(&fakeStore{browser: "chrome", profile: "Default", isDefault: true, readErr: errors.New("secret-cookie-value")}), want: "could not be read"},
		{name: "no matching cookies", stores: stores(&fakeStore{browser: "chrome", profile: "Default", isDefault: true, cookies: []*kooky.Cookie{cookie("other", "secret-cookie-value", ".example.com", "", time.Time{})}}), want: "no valid Pixieset cookies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(context.Background(), Selector{Browser: "chrome"}, tt.stores)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("load() error = %v, want text %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret-cookie-value") {
				t.Fatalf("load() leaked cookie data: %v", err)
			}
		})
	}
}

func TestLoadReturnsContextAndCloseErrorsAndClosesEveryStore(t *testing.T) {
	selected := &fakeStore{browser: "chrome", profile: "Default", isDefault: true, cookies: []*kooky.Cookie{cookie("session", "secret", ".pixieset.com", "", time.Time{})}}
	unmatched := &fakeStore{browser: "firefox", profile: "default-release", isDefault: true, closeErr: errors.New("close failed")}
	_, err := load(context.Background(), Selector{Browser: "chrome"}, stores(selected, unmatched))
	if err == nil || !strings.Contains(err.Error(), "close browser cookie stores") {
		t.Fatalf("load() error = %v", err)
	}
	if !selected.closed || !unmatched.closed {
		t.Fatalf("stores closed = %v, %v", selected.closed, unmatched.closed)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = load(canceled, Selector{Browser: "chrome"}, stores())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("load() error = %v, want context.Canceled", err)
	}
}

func TestSnapshotStoreCopiesChromiumLayoutAndRemovesIt(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "User Data", "Profile 2", "Network")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(profile, "Cookies")
	writeSQLiteDatabase(t, source)
	if err := os.WriteFile(filepath.Join(root, "User Data", "Local State"), []byte("local state"), 0600); err != nil {
		t.Fatal(err)
	}
	discovered := &fakeStore{browser: "chrome", profile: "Profile 2", filePath: source}
	var snapshotPath string
	store, remove, err := snapshotStoreWith(discovered, func(browser, filename string) (kooky.CookieStore, error) {
		snapshotPath = filename
		if browser != "chrome" || filename == source {
			t.Fatalf("snapshot opener got browser=%q filename=%q", browser, filename)
		}
		if err := validateSQLiteIntegrity(filename); err != nil {
			t.Fatalf("snapshot database integrity = %v", err)
		}
		statePath := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filename))), "Local State")
		data, err := os.ReadFile(statePath)
		if err != nil || string(data) != "local state" {
			t.Fatalf("snapshot Local State = %q, %v", data, err)
		}
		return &fakeStore{browser: browser}, nil
	})
	if err != nil {
		t.Fatalf("snapshotStoreWith() error = %v", err)
	}
	if store == nil {
		t.Fatal("snapshotStoreWith() returned a nil store")
	}
	if err := remove(); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if _, err := os.Stat(snapshotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists: %v", err)
	}
}

func TestSnapshotStoreCopiesFirefoxContainers(t *testing.T) {
	profile := t.TempDir()
	source := filepath.Join(profile, "cookies.sqlite")
	writeSQLiteDatabase(t, source)
	if err := os.WriteFile(filepath.Join(profile, "containers.json"), []byte("containers"), 0600); err != nil {
		t.Fatal(err)
	}
	discovered := &fakeStore{browser: "firefox", profile: "default-release", filePath: source}
	store, remove, err := snapshotStoreWith(discovered, func(_ string, filename string) (kooky.CookieStore, error) {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "containers.json"))
		if err != nil || string(data) != "containers" {
			t.Fatalf("snapshot containers = %q, %v", data, err)
		}
		return &fakeStore{browser: "firefox"}, nil
	})
	if err != nil {
		t.Fatalf("snapshotStoreWith() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := remove(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupSQLiteDatabaseIncludesWALStateAndValidatesSnapshot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Cookies")
	destination := filepath.Join(root, "snapshot", "Cookies")
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite3.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE values_table (value TEXT); INSERT INTO values_table VALUES ('from-wal')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := backupSQLiteDatabase(source, destination); err != nil {
		_ = db.Close()
		t.Fatalf("backupSQLiteDatabase() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := sqlite3.OpenFlags(destination, sqlite3.OPEN_READONLY|sqlite3.OPEN_URI)
	if err != nil {
		t.Fatal(err)
	}
	stmt, _, err := snapshot.Prepare("SELECT value FROM values_table")
	if err != nil {
		_ = snapshot.Close()
		t.Fatal(err)
	}
	if !stmt.Step() || stmt.ColumnText(0) != "from-wal" || stmt.Err() != nil {
		t.Fatalf("snapshot WAL row missing: value=%q err=%v", stmt.ColumnText(0), stmt.Err())
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotTempDirUsesPrivateBoundary(t *testing.T) {
	path, err := makeSnapshotTempDir("", `pixiegrabber\unsafe-*`)
	if err == nil {
		_ = removeSnapshotDir(path)
		t.Fatal("makeSnapshotTempDir accepted a path separator in its pattern")
	}
}

func TestSnapshotFilesArePrivateUnderPermissiveUmask(t *testing.T) {
	if os.Getenv("PIXIEGRABBER_UMASK_HELPER") != "1" {
		sh, err := exec.LookPath("sh")
		if err != nil {
			t.Skipf("permissive-umask shell is unavailable: %v", err)
		}
		cmd := exec.Command(sh, "-c", `umask 000; exec "$1" -test.run=^TestSnapshotFilesArePrivateUnderPermissiveUmask$ -test.v`, "browsercookies-umask", os.Args[0])
		cmd.Env = append(os.Environ(), "PIXIEGRABBER_UMASK_HELPER=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("permissive-umask child failed: %v\n%s", err, output)
		}
		return
	}

	root := t.TempDir()
	profile := filepath.Join(root, "User Data", "Default", "Network")
	source := filepath.Join(profile, "Cookies")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	writeSQLiteDatabase(t, source)
	writeRegularFileWithContents(t, filepath.Join(root, "User Data", "Local State"), "local state")

	store, remove, err := snapshotStoreWith(&fakeStore{browser: "chrome", filePath: source}, func(_ string, filename string) (kooky.CookieStore, error) {
		snapshotRoot := filepath.Dir(filepath.Dir(filepath.Dir(filename)))
		for _, item := range []struct {
			path      string
			directory bool
		}{
			{snapshotRoot, true},
			{filepath.Join(snapshotRoot, "Profile"), true},
			{filepath.Dir(filename), true},
			{filepath.Join(snapshotRoot, "Local State"), false},
			{filename, false},
		} {
			assertPermissionMode(t, item.path, item.directory)
		}
		return &fakeStore{browser: "chrome"}, nil
	})
	if err != nil {
		t.Fatalf("snapshotStoreWith() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := remove(); err != nil {
		t.Fatal(err)
	}
}

func assertPermissionMode(t *testing.T, path string, directory bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	want := os.FileMode(0600)
	if directory {
		want = 0700
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %q = %04o, want %04o", path, got, want)
	}
}

func TestSnapshotFailureRemovesTemporaryDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshot-root")
	oldMake := makeSnapshotTempDir
	oldRemove := removeSnapshotDir
	makeSnapshotTempDir = func(string, string) (string, error) {
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", err
		}
		return root, nil
	}
	removeSnapshotDir = os.RemoveAll
	defer func() {
		makeSnapshotTempDir = oldMake
		removeSnapshotDir = oldRemove
	}()
	source := filepath.Join(t.TempDir(), "Default", "Network", "Cookies")
	writeRegularFile(t, source)
	_, _, err := snapshotStoreWith(&fakeStore{browser: "chrome", filePath: source}, func(string, string) (kooky.CookieStore, error) {
		return nil, nil
	})
	if err == nil || strings.Contains(err.Error(), source) || strings.Contains(err.Error(), "not a database") {
		t.Fatalf("snapshot failure = %v", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary snapshot directory remains: %v", statErr)
	}
}

func TestLoadSnapshotReaderRejectsChromiumPartitionedIdentityAndCleansUp(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite3.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE cookies (host_key TEXT, name TEXT, path TEXT, top_frame_site_key TEXT); INSERT INTO cookies VALUES ('.pixieset.com', 'blocked', '/', 'https://example.com'); INSERT INTO cookies VALUES ('.pixieset.com', 'blocked', '/', '')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	opened := &filterAwareStore{fakeStore: fakeStore{
		browser: "chrome",
		profile: "Default",
		cookies: []*kooky.Cookie{
			cookie("blocked", "blocked-secret", ".pixieset.com", "", time.Time{}),
			cookie("wanted", "wanted-secret", ".pixieset.com", "", time.Time{}),
		},
	}}
	var snapshotPath string
	oldOpen := openSnapshotCookieStore
	openSnapshotCookieStore = func(browser, filename string) (kooky.CookieStore, error) {
		if browser != "chrome" {
			t.Fatalf("snapshot browser = %q", browser)
		}
		snapshotPath = filename
		return opened, nil
	}
	defer func() { openSnapshotCookieStore = oldOpen }()

	_, err = loadWith(context.Background(), Selector{Browser: "chrome"}, stores(&fakeStore{browser: "chrome", profile: "Default", filePath: source}), snapshotReader)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if opened.decryptions != 1 {
		t.Fatalf("value decryptions = %d, want one unpartitioned identity", opened.decryptions)
	}
	if !opened.closed {
		t.Fatal("snapshot store was not closed")
	}
	if snapshotPath == "" {
		t.Fatal("snapshot opener did not receive a path")
	}
	if _, statErr := os.Stat(snapshotPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("snapshot remains after Load: %v", statErr)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFirefoxContainerRowsRequireReadableMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "profile", "cookies.sqlite")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite3.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE moz_cookies (originAttributes TEXT); INSERT INTO moz_cookies VALUES ('^userContextId=1')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	opened := &filterAwareStore{fakeStore: fakeStore{
		browser: "firefox",
		profile: "default-release",
		cookies: []*kooky.Cookie{cookie("plain", "plain-secret", ".pixieset.com", "", time.Time{})},
	}}
	var snapshotPath string
	oldOpen := openSnapshotCookieStore
	openSnapshotCookieStore = func(_, filename string) (kooky.CookieStore, error) {
		snapshotPath = filename
		return opened, nil
	}
	defer func() { openSnapshotCookieStore = oldOpen }()

	_, err = loadWith(context.Background(), Selector{Browser: "firefox"}, stores(&fakeStore{browser: "firefox", profile: "default-release", isDefault: true, filePath: source}), snapshotReader)
	if err == nil || !strings.Contains(err.Error(), "container metadata") || strings.Contains(err.Error(), source) {
		t.Fatalf("container metadata error = %v", err)
	}
	if !opened.closed {
		t.Fatal("snapshot store was not closed after metadata failure")
	}
	if _, statErr := os.Stat(snapshotPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("snapshot remains after metadata failure: %v", statErr)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFirefoxWithoutContainerRowsAllowsMissingMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "profile", "cookies.sqlite")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite3.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE moz_cookies (originAttributes TEXT); INSERT INTO moz_cookies VALUES ('')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	opened := &filterAwareStore{fakeStore: fakeStore{
		browser: "firefox",
		profile: "default-release",
		cookies: []*kooky.Cookie{cookie("plain", "plain-secret", ".pixieset.com", "", time.Time{})},
	}}
	oldOpen := openSnapshotCookieStore
	openSnapshotCookieStore = func(_, _ string) (kooky.CookieStore, error) { return opened, nil }
	defer func() { openSnapshotCookieStore = oldOpen }()

	if _, err := loadWith(context.Background(), Selector{Browser: "firefox"}, stores(&fakeStore{browser: "firefox", profile: "default-release", isDefault: true, filePath: source}), snapshotReader); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if !opened.closed {
		t.Fatal("snapshot store was not closed")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func cookie(name, value, domain, container string, expires time.Time) *kooky.Cookie {
	return &kooky.Cookie{Cookie: http.Cookie{Name: name, Value: value, Domain: domain, Path: "/", Expires: expires, Secure: true, HttpOnly: true}, Container: container}
}

func stores(values ...kooky.CookieStore) kooky.CookieStoreSeq {
	return func(yield func(kooky.CookieStore, error) bool) {
		for _, value := range values {
			if !yield(value, nil) {
				return
			}
		}
	}
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

type fakeStore struct {
	browser   string
	profile   string
	filePath  string
	isDefault bool
	cookies   []*kooky.Cookie
	readErr   error
	closeErr  error
	closed    bool
}

var fakeStoreRootOnce sync.Once
var fakeStoreRoot string

func (s *fakeStore) Browser() string        { return s.browser }
func (s *fakeStore) Profile() string        { return s.profile }
func (s *fakeStore) IsDefaultProfile() bool { return s.isDefault }
func (s *fakeStore) FilePath() string {
	if s.filePath != "" {
		if err := os.MkdirAll(filepath.Dir(s.filePath), 0700); err == nil {
			file, openErr := os.OpenFile(s.filePath, os.O_CREATE, 0600)
			if openErr == nil {
				_ = file.Close()
			}
		}
		return s.filePath
	}
	fakeStoreRootOnce.Do(func() {
		fakeStoreRoot = filepath.Join(os.TempDir(), fmt.Sprintf("pixiegrabber-test-stores-%d", os.Getpid()))
	})
	profile := s.profile
	if profile == "" {
		profile = "Default"
	}
	dir := filepath.Join(fakeStoreRoot, s.browser, profile)
	var filename string
	switch strings.ToLower(s.browser) {
	case "brave", "chrome", "chromium", "edge":
		filename = filepath.Join(dir, "Network", "Cookies")
	case "firefox":
		filename = filepath.Join(dir, "cookies.sqlite")
	default:
		filename = filepath.Join(dir, "Cookies")
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err == nil {
		file, openErr := os.OpenFile(filename, os.O_CREATE, 0600)
		if openErr == nil {
			_ = file.Close()
		}
	}
	return filename
}
func (s *fakeStore) Cookies(*url.URL) []*http.Cookie     { return nil }
func (s *fakeStore) SetCookies(*url.URL, []*http.Cookie) {}
func (s *fakeStore) SubJar(context.Context, ...kooky.Filter) (http.CookieJar, error) {
	return cookiejar.New(nil)
}
func (s *fakeStore) TraverseCookies(filters ...kooky.Filter) kooky.CookieSeq {
	return func(yield func(*kooky.Cookie, error) bool) {
		if s.readErr != nil {
			yield(nil, s.readErr)
			return
		}
		ctx := context.Background()
		for _, value := range s.cookies {
			if kooky.FilterCookie(ctx, value, filters...) && !yield(value, nil) {
				return
			}
		}
	}
}
func (s *fakeStore) Close() error {
	s.closed = true
	return s.closeErr
}

type filterAwareStore struct {
	fakeStore
	decryptions int
}

func (s *filterAwareStore) TraverseCookies(filters ...kooky.Filter) kooky.CookieSeq {
	return func(yield func(*kooky.Cookie, error) bool) {
		if s.readErr != nil {
			yield(nil, s.readErr)
			return
		}
		for _, value := range s.cookies {
			passed := true
			for _, filter := range filters {
				if _, isValueFilter := filter.(kooky.ValueFilterFunc); isValueFilter {
					continue
				}
				if !filter.Filter(value) {
					passed = false
					break
				}
			}
			if !passed {
				continue
			}
			s.decryptions++
			for _, filter := range filters {
				if _, isValueFilter := filter.(kooky.ValueFilterFunc); isValueFilter && !filter.Filter(value) {
					passed = false
					break
				}
			}
			if passed && !yield(value, nil) {
				return
			}
		}
	}
}

var _ kooky.CookieStore = (*fakeStore)(nil)

func writeSQLiteDatabase(t *testing.T, filename string) {
	t.Helper()
	db, err := sqlite3.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE IF NOT EXISTS synthetic (value TEXT)"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRegularFile(t *testing.T, filename string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeRegularFileWithContents(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
