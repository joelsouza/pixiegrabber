package browsercookies

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	// No profile holds a readable cookie database, so the true default profile
	// wins. kooky marks Profile 1 as the default one, which the directory name
	// contradicts.
	if got.store != network {
		t.Fatalf("selected store = %p, want Network/Cookies store %p", got.store, network)
	}
}

func TestSelectStoreScansEveryProfileAndPrefersSessionEvidence(t *testing.T) {
	root := t.TempDir()
	profiles := []struct {
		name    string
		cookies []string
	}{
		{name: "Default", cookies: []string{"gallery_dashboard_session", "PHPSESSID", "wsid", "gd_ca", "IDP_SID"}},
		{name: "Profile 3"},
		{name: "Profile 7", cookies: []string{"GD-XSRF-TOKEN", "_ga"}},
	}
	candidates := make([]kooky.CookieStore, 0, len(profiles))
	for _, profile := range profiles {
		path := filepath.Join(root, "User Data", profile.name, "Network", "Cookies")
		writeChromiumCookieDatabase(t, path, profile.cookies)
		candidates = append(candidates, &fakeStore{browser: "chrome", profile: profile.name, filePath: path})
	}

	got, err := selectStore(Selector{Browser: "chrome"}, candidates)
	if err != nil {
		t.Fatalf("selectStore() error = %v", err)
	}
	if got.store != candidates[0] {
		t.Fatalf("selected profile = %q, want Default", got.store.Profile())
	}
	if want := (cookieScore{session: 5, total: 5}); got.evidence.score != want {
		t.Fatalf("selected score = %+v, want %+v", got.evidence.score, want)
	}
	if len(got.scanned) != len(profiles) {
		t.Fatalf("scanned candidates = %d, want %d", len(got.scanned), len(profiles))
	}
	for _, evidence := range got.scanned {
		if evidence.err != nil {
			t.Fatalf("candidate %q error = %v", evidence.candidate.store.Profile(), evidence.err)
		}
	}
}

func TestInspectCandidateKeepsFirefoxContainersApart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.sqlite")
	writeFirefoxCookieDatabase(t, path, []firefoxRow{
		{name: "PHPSESSID"},
		{name: "GD-XSRF-TOKEN", origin: "^userContextId=3"},
		{name: "gallery_dashboard_session", origin: "^userContextId=3"},
		{name: "_ga", origin: "^userContextId=4"},
		{name: "_gid", origin: "^firstPartyDomain=example.com"},
	})

	groups, err := inspectCandidate("firefox", path)
	if err != nil {
		t.Fatalf("inspectCandidate() error = %v", err)
	}
	want := candidateGroups{
		"":  {session: 1, total: 2},
		"3": {session: 1, token: 1, total: 2},
		"4": {total: 1},
	}
	if !maps.Equal(groups, want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
	}
	// The strongest group must hold its own cookies alone. A sum of two
	// containers would give four cookies and two session cookies.
	name, score := groups.best()
	if name != "3" || score != (cookieScore{session: 1, token: 1, total: 2}) {
		t.Fatalf("best group = %q %+v", name, score)
	}
}

// A reported container must carry the name that the user sees, so that the
// whole value goes straight back into --cookies-from-browser.
func TestInspectCandidateNamesFirefoxContainers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.sqlite")
	writeFirefoxCookieDatabase(t, path, []firefoxRow{
		{name: "PHPSESSID", origin: "^userContextId=2"},
		{name: "wsid", origin: "^userContextId=7"},
		{name: "_ga", origin: "^userContextId=9"},
	})
	writeRegularFileWithContents(t, filepath.Join(dir, "containers.json"), `{"identities":[
		{"userContextId":2,"l10nID":"user-context-work"},
		{"userContextId":7,"name":"Photos"},
		{"userContextId":9,"name":"userContextIdInternal.thumbnail"}
	]}`)

	groups, err := inspectCandidate("firefox", path)
	if err != nil {
		t.Fatalf("inspectCandidate() error = %v", err)
	}
	// Firefox translates a built-in container, keeps a custom name, and hides
	// its own containers. An unnamed container keeps its number, as kooky does.
	want := candidateGroups{
		"Work":   {session: 1, total: 1},
		"Photos": {session: 1, total: 1},
		"9":      {total: 1},
	}
	if !maps.Equal(groups, want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
	}
}

func TestLoadListsTheContainerOfEachCandidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.sqlite")
	writeFirefoxCookieDatabase(t, path, []firefoxRow{{name: "GD-XSRF-TOKEN", origin: "^userContextId=2"}})
	writeRegularFileWithContents(t, filepath.Join(dir, "containers.json"), `{"identities":[{"userContextId":2,"l10nID":"user-context-work"}]}`)

	_, err := load(context.Background(), Selector{Browser: "firefox"}, stores(
		&fakeStore{browser: "firefox", profile: "default-release", isDefault: true, filePath: path},
	))
	if err == nil {
		t.Fatal("load() succeeded without a session")
	}
	want := "firefox:default-release::Work — 1 Pixieset cookies, no session cookie"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("load() error = %v, want text %q", err, want)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("load() leaked a path: %v", err)
	}
}

// A container needs no browser name, because only Firefox gives a cookie a
// container.
func TestMatchesContainerAppliesWithoutABrowserName(t *testing.T) {
	selector, err := ParseSelector("::Work")
	if err != nil {
		t.Fatal(err)
	}
	work := cookie("work", "work-secret", ".pixieset.com", "Work", time.Time{})
	plain := cookie("plain", "plain-secret", ".pixieset.com", "", time.Time{})
	if !matchesContainer(selector, work) {
		t.Fatal("the named container rejected its own cookie")
	}
	if matchesContainer(selector, plain) {
		t.Fatal("the named container accepted a cookie of no container")
	}
	if !matchesContainer(Selector{}, plain) || !matchesContainer(Selector{}, work) {
		t.Fatal("an empty selector rejected a cookie")
	}
}

// immutable=1 hides the write-ahead log, so the newest cookies of a running
// browser are invisible to the live read.
func TestInspectCandidateReadsWriteAheadLogRowsFromASnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.sqlite")
	db := openFirefoxWALDatabase(t, path)
	insertFirefoxRows(t, db, []firefoxRow{{name: "PHPSESSID"}, {name: "GD-XSRF-TOKEN"}})

	live, err := inspectDatabase("firefox", immutableURI(path), nil)
	if err != nil {
		t.Fatalf("inspectDatabase() error = %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live groups = %+v, want none while the rows stay in the log", live)
	}
	groups, err := inspectCandidate("firefox", path)
	if err != nil {
		t.Fatalf("inspectCandidate() error = %v", err)
	}
	want := candidateGroups{"": {session: 1, token: 1, total: 2}}
	if !maps.Equal(groups, want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
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
	if got.store != sqliteStore {
		t.Fatalf("selected store = %p, want SQLite store %p", got.store, sqliteStore)
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

// Without a named container the score selects one container. A jar that mixed
// two containers would hold two different sessions.
func TestLoadFirefoxWithoutContainerSelectsTheStrongestContainer(t *testing.T) {
	tests := []struct {
		name          string
		cookies       []*kooky.Cookie
		wantName      string
		wantContainer string
	}{
		{
			name: "session evidence beats a token",
			cookies: []*kooky.Cookie{
				cookie("GD-XSRF-TOKEN", "token-secret", ".pixieset.com", "", time.Time{}),
				cookie("PHPSESSID", "work-secret", ".pixieset.com", "Work", time.Time{}),
			},
			wantName:      "PHPSESSID",
			wantContainer: "Work",
		},
		{
			name: "an equal score keeps the cookies without a container",
			cookies: []*kooky.Cookie{
				cookie("PHPSESSID", "plain-secret", ".pixieset.com", "", time.Time{}),
				cookie("gallery_dashboard_session", "work-secret", ".pixieset.com", "Work", time.Time{}),
			},
			wantName: "PHPSESSID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected := &fakeStore{browser: "firefox", profile: "default-release", isDefault: true, cookies: tt.cookies}
			session, err := load(context.Background(), Selector{Browser: "firefox"}, stores(selected))
			if err != nil {
				t.Fatal(err)
			}
			got := session.Jar.Cookies(mustURL(t, "https://galleries.pixieset.com/"))
			if len(got) != 1 || got[0].Name != tt.wantName {
				t.Fatalf("selected container cookies = %#v, want %q alone", got, tt.wantName)
			}
			if session.Container != tt.wantContainer {
				t.Fatalf("Session.Container = %q, want %q", session.Container, tt.wantContainer)
			}
		})
	}
}

// Every profile is a candidate, so a signed-in profile beats the default one.
func TestLoadScansEveryProfileAndKeepsTheSignedInOne(t *testing.T) {
	root := t.TempDir()
	defaultPath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
	signedInPath := filepath.Join(root, "User Data", "Profile 3", "Network", "Cookies")
	writeChromiumCookieDatabase(t, defaultPath, []string{"GD-XSRF-TOKEN"})
	writeChromiumCookieDatabase(t, signedInPath, []string{"PHPSESSID", "wsid"})
	unused := &fakeStore{
		browser:   "chrome",
		profile:   "Default",
		isDefault: true,
		filePath:  defaultPath,
		cookies:   []*kooky.Cookie{cookie("GD-XSRF-TOKEN", "token-secret", ".pixieset.com", "", time.Time{})},
	}
	signedIn := &fakeStore{
		browser:  "chrome",
		profile:  "Profile 3",
		filePath: signedInPath,
		cookies:  []*kooky.Cookie{cookie("PHPSESSID", "session-secret", ".pixieset.com", "", time.Time{})},
	}

	session, err := load(context.Background(), Selector{Browser: "chrome"}, stores(unused, signedIn))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if session.Profile != "Profile 3" {
		t.Fatalf("Session.Profile = %q, want the profile that holds the session", session.Profile)
	}
}

func TestSnapshotReportsAPrivacyControlSeparatelyFromAFullDisk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	source := filepath.Join(t.TempDir(), "Cookies")
	writeSQLiteDatabase(t, source)
	if err := os.Chmod(source, 0000); err != nil {
		t.Fatal(err)
	}
	err := snapshotSQLiteDatabase(source, filepath.Join(t.TempDir(), "Cookies"))
	if !errors.Is(err, errSnapshotPermission) {
		t.Fatalf("snapshotSQLiteDatabase() error = %v, want a permission error", err)
	}
	if strings.Contains(err.Error(), source) {
		t.Fatalf("error leaked a path: %v", err)
	}
}

func TestLoadListsEveryCandidateWhenNoProfileHoldsASession(t *testing.T) {
	root := t.TempDir()
	emptyPath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
	tokenPath := filepath.Join(root, "User Data", "Profile 3", "Network", "Cookies")
	writeChromiumCookieDatabase(t, emptyPath, nil)
	writeChromiumCookieDatabase(t, tokenPath, []string{"GD-XSRF-TOKEN"})

	_, err := load(context.Background(), Selector{Browser: "chrome"}, stores(
		&fakeStore{browser: "chrome", profile: "Empty One", isDefault: true, filePath: emptyPath},
		&fakeStore{
			browser:  "chrome",
			profile:  "Token Only",
			filePath: tokenPath,
			cookies:  []*kooky.Cookie{cookie("other", "secret-cookie-value", ".example.com", "", time.Time{})},
		},
	))
	if err == nil {
		t.Fatal("load() succeeded without a session")
	}
	for _, want := range []string{
		"no valid Pixieset cookies",
		"chrome:Empty One — 0 Pixieset cookies",
		"chrome:Token Only — 1 Pixieset cookies, no session cookie",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("load() error = %v, want text %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "secret-cookie-value") || strings.Contains(err.Error(), root) {
		t.Fatalf("load() leaked a value or a path: %v", err)
	}
}

func TestSelectStoreWithoutABrowserSearchesEveryBrowser(t *testing.T) {
	root := t.TempDir()
	chromePath := filepath.Join(root, "User Data", "Default", "Network", "Cookies")
	firefoxPath := filepath.Join(root, "Firefox", "Profiles", "default-release", "cookies.sqlite")
	if err := os.MkdirAll(filepath.Dir(firefoxPath), 0700); err != nil {
		t.Fatal(err)
	}
	// Chrome holds a token only. Firefox holds the session.
	writeChromiumCookieDatabase(t, chromePath, []string{"GD-XSRF-TOKEN"})
	writeFirefoxCookieDatabase(t, firefoxPath, []firefoxRow{{name: "PHPSESSID"}, {name: "wsid"}})

	chromeStore := &fakeStore{browser: "chrome", profile: "Default", isDefault: true, filePath: chromePath}
	firefoxStore := &fakeStore{browser: "firefox", profile: "default-release", isDefault: true, filePath: firefoxPath}

	selection, err := selectStore(Selector{}, []kooky.CookieStore{chromeStore, firefoxStore})
	if err != nil {
		t.Fatalf("selectStore() error = %v", err)
	}
	if selection.store != kooky.CookieStore(firefoxStore) {
		t.Fatalf("selectStore() chose %s, want the browser that holds the session", selection.store.Browser())
	}
	if len(selection.scanned) != 2 {
		t.Fatalf("scanned = %d candidates, want both browsers", len(selection.scanned))
	}
}

func TestSelectStoreScoresASingleCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "User Data", "Default", "Network", "Cookies")
	writeChromiumCookieDatabase(t, path, []string{"PHPSESSID", "GD-XSRF-TOKEN"})
	store := &fakeStore{browser: "chrome", profile: "Default", isDefault: true, filePath: path}

	selection, err := selectStore(Selector{Browser: "chrome"}, []kooky.CookieStore{store})
	if err != nil {
		t.Fatalf("selectStore() error = %v", err)
	}
	// A single candidate is scanned too, so the reported evidence is true.
	if selection.evidence.score.session != 1 || selection.evidence.score.token != 1 {
		t.Fatalf("single-candidate score = %+v, want one session and one token cookie", selection.evidence.score)
	}
}

func TestSelectStoreDropsAnUnclearProfileAndKeepsSearching(t *testing.T) {
	root := t.TempDir()
	// Two cookie files of equal rank in one profile make that profile unclear.
	unclearFirst := filepath.Join(root, "User Data", "Unclear", "Cookies")
	unclearSecond := filepath.Join(root, "User Data", "Unclear", "Other", "Cookies")
	usable := filepath.Join(root, "User Data", "Default", "Cookies")
	for _, path := range []string{unclearFirst, unclearSecond} {
		writeChromiumCookieDatabase(t, path, []string{"PHPSESSID"})
	}
	writeChromiumCookieDatabase(t, usable, []string{"gd_ca"})

	selection, err := selectStore(Selector{Browser: "chrome"}, []kooky.CookieStore{
		&fakeStore{browser: "chrome", profile: "Unclear", filePath: unclearFirst},
		&fakeStore{browser: "chrome", profile: "Unclear", filePath: unclearSecond},
		&fakeStore{browser: "chrome", profile: "Default", isDefault: true, filePath: usable},
	})
	if err != nil {
		t.Fatalf("selectStore() error = %v", err)
	}
	if got := selection.store.Profile(); got != "Default" {
		t.Fatalf("selectStore() chose profile %q, want the readable profile", got)
	}
}

func TestMakeStoreCandidateMatchesAProfilePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "User Data", "Profile 7", "Network", "Cookies")
	writeChromiumCookieDatabase(t, path, []string{"PHPSESSID"})
	store := &fakeStore{browser: "chrome", profile: "Some Display Name", filePath: path}

	for _, profile := range []string{
		filepath.Join(root, "User Data", "Profile 7"),
		filepath.Join(root, "User Data", "Profile 7", "Network"),
		path,
	} {
		selector, err := ParseSelector("chrome:" + profile)
		if err != nil {
			t.Fatalf("ParseSelector(%q) error = %v", profile, err)
		}
		if _, ok := makeStoreCandidate(selector, store); !ok {
			t.Fatalf("profile path %q did not match the store", profile)
		}
	}

	selector, err := ParseSelector("chrome:" + filepath.Join(root, "User Data", "Profile 9"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := makeStoreCandidate(selector, store); ok {
		t.Fatal("an unrelated profile path matched the store")
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
		name     string
		selector Selector
		stores   kooky.CookieStoreSeq
		want     string
	}{
		{name: "missing", stores: stores(&fakeStore{browser: "firefox", profile: "default-release", isDefault: true}), want: "no chrome cookie store"},
		{name: "two empty profiles of one name", selector: Selector{Browser: "chrome", Profile: "Default", hasProfile: true}, stores: stores(&fakeStore{browser: "chrome", profile: "Default", isDefault: true}, &fakeStore{browser: "chrome", profile: "Default", isDefault: true, filePath: otherDefaultPath}), want: "no valid Pixieset cookies"},
		{name: "read failure", stores: stores(&fakeStore{browser: "chrome", profile: "Default", isDefault: true, readErr: errors.New("secret-cookie-value")}), want: "could not be read"},
		{name: "no matching cookies", stores: stores(&fakeStore{browser: "chrome", profile: "Default", isDefault: true, cookies: []*kooky.Cookie{cookie("other", "secret-cookie-value", ".example.com", "", time.Time{})}}), want: "no valid Pixieset cookies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := tt.selector
			if selector.Browser == "" {
				selector = Selector{Browser: "chrome"}
			}
			_, err := load(context.Background(), selector, tt.stores)
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

// The browser cookie reader in kooky never reads a write-ahead log, so the
// snapshot must hold every row in the main file before that reader opens it.
func TestSnapshotStoreReadsWALCookiesFromALockedFirefoxProfile(t *testing.T) {
	profile := t.TempDir()
	source := filepath.Join(profile, "cookies.sqlite")
	db, err := sqlite3.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close source database: %v", err)
		}
	})
	expiry := time.Now().Add(time.Hour).Unix()
	insert := func(name string) string {
		return fmt.Sprintf("INSERT INTO moz_cookies (name, value, host, path, expiry, creationTime, isSecure, isHttpOnly, originAttributes) VALUES ('%s', 'secret', '.pixieset.com', '/', %d, 0, 1, 1, '')", name, expiry)
	}
	// Firefox holds cookies.sqlite in exclusive lock mode while it runs.
	if err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; PRAGMA locking_mode=EXCLUSIVE; CREATE TABLE moz_cookies (id INTEGER PRIMARY KEY, name TEXT, value TEXT, host TEXT, path TEXT, expiry INTEGER, creationTime INTEGER, isSecure INTEGER, isHttpOnly INTEGER, originAttributes TEXT); " + insert("from-main")); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(insert("from-wal")); err != nil {
		t.Fatal(err)
	}

	store, remove, err := snapshotStoreWith(&fakeStore{browser: "firefox", profile: "default-release", filePath: source}, openCookieStore)
	if err != nil {
		t.Fatalf("snapshotStoreWith() error = %v", err)
	}
	var names []string
	for cookie, readErr := range store.TraverseCookies() {
		if readErr != nil {
			t.Fatalf("TraverseCookies() error = %v", readErr)
		}
		names = append(names, cookie.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"from-main", "from-wal"}) {
		t.Fatalf("snapshot cookies = %v, want both the main and the log row", names)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := remove(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotSQLiteDatabaseCopiesOpenDatabaseIncludingWALState(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(t *testing.T, source string)
		wantErr  error
		wantRows []string
	}{
		{
			// The browser stays open and keeps the newest row in the log.
			name:     "open database",
			prepare:  func(t *testing.T, source string) { openWALDatabase(t, source, false) },
			wantRows: []string{"from-main", "from-wal"},
		},
		{
			// Firefox holds cookies.sqlite in exclusive lock mode. The SQLite
			// backup API fails here; a byte copy does not.
			name:     "exclusive lock",
			prepare:  func(t *testing.T, source string) { openWALDatabase(t, source, true) },
			wantRows: []string{"from-main", "from-wal"},
		},
		{
			// A log larger than the copy limit makes the snapshot fall back to
			// the main file, which stays coherent.
			name: "oversized write-ahead log",
			prepare: func(t *testing.T, source string) {
				writeValueDatabase(t, source, 1)
				writeSparseFile(t, source+"-wal", maxRegularCopyBytes+1)
			},
			wantRows: []string{"from-main"},
		},
		{
			name: "damaged main file",
			prepare: func(t *testing.T, source string) {
				writeValueDatabase(t, source, 400)
				truncateFileByHalf(t, source)
				writeSparseFile(t, source+"-wal", 32<<10)
			},
			wantErr: errSnapshotIntegrity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "Cookies")
			destination := filepath.Join(root, "snapshot", "Cookies")
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				t.Fatal(err)
			}
			tt.prepare(t, source)

			err := snapshotSQLiteDatabase(source, destination)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("snapshotSQLiteDatabase() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				// The fallback copies the main file alone, so no log copy from
				// the first attempt may remain.
				for _, suffix := range sqliteSidecarSuffixes {
					if _, statErr := os.Stat(destination + suffix); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("snapshot sidecar %q remains: %v", suffix, statErr)
					}
				}
				return
			}
			// The checkpoint must leave every row in the main file, because the
			// readers that follow open the file alone.
			for _, suffix := range sqliteSidecarSuffixes {
				if err := os.Remove(destination + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			if got := readValueRows(t, destination); !slices.Equal(got, tt.wantRows) {
				t.Fatalf("snapshot rows = %v, want %v", got, tt.wantRows)
			}
		})
	}
}

// openWALDatabase writes a database whose newest row lives only in the
// write-ahead log and keeps the connection open, as a running browser does.
func openWALDatabase(t *testing.T, filename string, exclusive bool) {
	t.Helper()
	db, err := sqlite3.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close source database: %v", err)
		}
	})
	setup := "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"
	if exclusive {
		setup += " PRAGMA locking_mode=EXCLUSIVE;"
	}
	setup += " CREATE TABLE values_table (value TEXT); INSERT INTO values_table VALUES ('from-main');"
	if err := db.Exec(setup); err != nil {
		t.Fatal(err)
	}
	// The checkpoint puts the first row in the main file. The second row then
	// stays in the log, which the snapshot must also copy.
	if err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO values_table VALUES ('from-wal')"); err != nil {
		t.Fatal(err)
	}
}

func writeValueDatabase(t *testing.T, filename string, rows int) {
	t.Helper()
	db, err := sqlite3.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE values_table (value TEXT); INSERT INTO values_table VALUES ('from-main')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for i := 1; i < rows; i++ {
		if err := db.Exec("INSERT INTO values_table VALUES ('filler')"); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeSparseFile(t *testing.T, filename string, size int64) {
	t.Helper()
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func truncateFileByHalf(t *testing.T, filename string) {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filename, info.Size()/2); err != nil {
		t.Fatal(err)
	}
}

func readValueRows(t *testing.T, filename string) []string {
	t.Helper()
	db, err := sqlite3.OpenFlags(filename, sqlite3.OPEN_READONLY|sqlite3.OPEN_URI)
	if err != nil {
		t.Fatal(err)
	}
	stmt, _, err := db.Prepare("SELECT value FROM values_table ORDER BY rowid")
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var rows []string
	for stmt.Step() {
		rows = append(rows, stmt.ColumnText(0))
	}
	if err := stmt.Err(); err != nil {
		_ = stmt.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	if err := stmt.Close(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return rows
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

// writeChromiumCookieDatabase writes a cookie database whose schema holds no
// value column at all. A scan that selected a value could not run here, which
// is what keeps the macOS Keychain quiet.
func writeChromiumCookieDatabase(t *testing.T, filename string, names []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite3.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close cookie database: %v", err)
		}
	}()
	if err := db.Exec("CREATE TABLE cookies (host_key TEXT, name TEXT, expires_utc INTEGER, is_persistent INTEGER)"); err != nil {
		t.Fatal(err)
	}
	chromiumTime := func(offset time.Duration) int64 {
		return time.Now().Add(offset).UnixMicro() + chromiumEpochOffset
	}
	// An expired session cookie and a cookie of another domain must not count.
	rows := []string{
		fmt.Sprintf("INSERT INTO cookies VALUES ('.pixieset.com', 'stale_session', %d, 1)", chromiumTime(-time.Hour)),
		fmt.Sprintf("INSERT INTO cookies VALUES ('.example.com', 'other_session', %d, 1)", chromiumTime(time.Hour)),
	}
	for _, name := range names {
		rows = append(rows, fmt.Sprintf("INSERT INTO cookies VALUES ('.pixieset.com', '%s', %d, 1)", name, chromiumTime(time.Hour)))
	}
	if err := db.Exec(strings.Join(rows, "; ")); err != nil {
		t.Fatal(err)
	}
}

type firefoxRow struct {
	name   string
	origin string
}

func writeFirefoxCookieDatabase(t *testing.T, filename string, rows []firefoxRow) {
	t.Helper()
	db, err := sqlite3.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close cookie database: %v", err)
		}
	}()
	if err := db.Exec("CREATE TABLE moz_cookies (host TEXT, name TEXT, expiry INTEGER, originAttributes TEXT)"); err != nil {
		t.Fatal(err)
	}
	insertFirefoxRows(t, db, rows)
}

// openFirefoxWALDatabase keeps the connection open under an exclusive lock, as
// a running Firefox does, and leaves the main file empty of cookies.
func openFirefoxWALDatabase(t *testing.T, filename string) *sqlite3.Conn {
	t.Helper()
	db, err := sqlite3.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close cookie database: %v", err)
		}
	})
	if err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; PRAGMA locking_mode=EXCLUSIVE; CREATE TABLE moz_cookies (host TEXT, name TEXT, expiry INTEGER, originAttributes TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertFirefoxRows(t *testing.T, db *sqlite3.Conn, rows []firefoxRow) {
	t.Helper()
	expiry := time.Now().Add(time.Hour).Unix()
	statements := []string{
		fmt.Sprintf("INSERT INTO moz_cookies VALUES ('.pixieset.com', 'stale_session', %d, '')", time.Now().Add(-time.Hour).Unix()),
		fmt.Sprintf("INSERT INTO moz_cookies VALUES ('.example.com', 'other_session', %d, '')", expiry),
	}
	for _, row := range rows {
		statements = append(statements, fmt.Sprintf("INSERT INTO moz_cookies VALUES ('.pixieset.com', '%s', %d, '%s')", row.name, expiry, row.origin))
	}
	if err := db.Exec(strings.Join(statements, "; ")); err != nil {
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
