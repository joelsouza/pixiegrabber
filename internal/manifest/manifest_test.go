package manifest

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pixiegrabber/internal/outputfs"
)

func openTestFS(t *testing.T) *outputfs.FS {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	f, err := outputfs.Open(filepath.Join(base, "output"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("cleanup Close() error = %v", err)
		}
	})
	return f
}

func testManifest() Manifest {
	created := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.FixedZone("local", -5*60*60))
	return Manifest{
		SchemaVersion: CurrentSchemaVersion,
		Collection: Collection{
			ID:            "collection-1",
			Name:          "References",
			Description:   "A local collection",
			SourceCreated: &created,
			PresenceState: PresencePresent,
			RunState:      RunComplete,
			LastSuccessAt: &created,
		},
		Sets: []Set{{
			ID:            "set-1",
			Name:          "Summer",
			SourceOrder:   1,
			PresenceState: PresencePresent,
		}, {
			ID:            "set-2",
			Name:          "Other",
			SourceOrder:   2,
			PresenceState: PresencePresent,
		}},
		References: []Reference{{
			ID:              "media-1",
			Name:            "Beach",
			MediaType:       "image",
			MIMEType:        "image/jpeg",
			SelectedQuality: "path_xxlarge",
			PresenceState:   PresencePresent,
			DownloadState:   DownloadComplete,
			Placements: []Placement{
				{SetID: "set-1", Path: "Summer--set-1/Beach--media-1.jpg", PresenceState: PresencePresent, DownloadState: DownloadComplete, InstalledSHA256: strings.Repeat("a", 64)},
				{SetID: "set-2", Path: "Other--set-2/Beach--media-1.jpg", PresenceState: PresencePresent, DownloadState: DownloadComplete, InstalledSHA256: strings.Repeat("b", 64)},
			},
		}},
	}
}

func TestPlacementChecksumMustBe64HexCharacters(t *testing.T) {
	tests := []string{"not-hex", strings.Repeat("a", 63), strings.Repeat("g", 64), strings.Repeat("a", 65)}
	for _, checksum := range tests {
		t.Run(checksum, func(t *testing.T) {
			m := testManifest()
			m.References[0].Placements[0].InstalledSHA256 = checksum
			if err := m.Validate(); err == nil {
				t.Fatalf("Validate() accepted checksum %q", checksum)
			}
		})
	}
	valid := testManifest()
	valid.References[0].Placements[0].InstalledSHA256 = ""
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() rejected absent checksum: %v", err)
	}
	valid.References[0].Placements[0].InstalledSHA256 = strings.Repeat("A", 64)
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() rejected uppercase checksum: %v", err)
	}
}

func TestNormalizeConvertsTransientStatesAndValidationRequiresExplicitStates(t *testing.T) {
	m := testManifest()
	m.Collection.RunState = RunRunning
	m.References[0].DownloadState = DownloadInProgress
	m.References[0].Placements[0].DownloadState = DownloadInProgress
	m.Normalize()
	if m.Collection.RunState != RunIncomplete {
		t.Fatalf("normalized run state = %q, want %q", m.Collection.RunState, RunIncomplete)
	}
	if m.References[0].DownloadState != DownloadPending || m.References[0].Placements[0].DownloadState != DownloadPending {
		t.Fatalf("normalized download states = %q and %q", m.References[0].DownloadState, m.References[0].Placements[0].DownloadState)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate() rejected normalized transient states: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "empty presence", edit: func(m *Manifest) { m.Collection.PresenceState = "" }},
		{name: "unknown presence", edit: func(m *Manifest) { m.Collection.PresenceState = "unknown" }},
		{name: "empty run", edit: func(m *Manifest) { m.Collection.RunState = "" }},
		{name: "running run", edit: func(m *Manifest) { m.Collection.RunState = "running" }},
		{name: "skipped download", edit: func(m *Manifest) { m.References[0].DownloadState = "skipped" }},
		{name: "empty download", edit: func(m *Manifest) { m.References[0].DownloadState = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			tt.edit(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("Validate() accepted non-explicit state")
			}
		})
	}
}

func TestLoadNormalizesLegacyTransientStates(t *testing.T) {
	fs := openTestFS(t)
	m := testManifest()
	m.Collection.RunState = RunRunning
	m.References[0].DownloadState = DownloadInProgress
	m.References[0].Placements[0].DownloadState = DownloadInProgress
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.AtomicReplace("collection.json", func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(fs, "collection.json")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Collection.RunState != RunIncomplete || got.References[0].DownloadState != DownloadPending || got.References[0].Placements[0].DownloadState != DownloadPending {
		t.Fatalf("Load() states = %q, %q, %q", got.Collection.RunState, got.References[0].DownloadState, got.References[0].Placements[0].DownloadState)
	}
}

func TestFailureMustMatchDownloadStateAndContainSanitizedDetails(t *testing.T) {
	tests := []struct {
		name    string
		state   DownloadState
		failure *Failure
		valid   bool
	}{
		{name: "failed without failure", state: DownloadFailed},
		{name: "failed without code", state: DownloadFailed, failure: &Failure{Message: "network error"}},
		{name: "failed without message", state: DownloadFailed, failure: &Failure{Code: "download_failed"}},
		{name: "failed with URL", state: DownloadFailed, failure: &Failure{Code: "download_failed", Message: "https://example.test/signed"}},
		{name: "pending with failure", state: DownloadPending, failure: &Failure{Code: "download_failed", Message: "network error"}},
		{name: "complete with failure", state: DownloadComplete, failure: &Failure{Code: "download_failed", Message: "network error"}},
		{name: "valid failed state", state: DownloadFailed, failure: &Failure{Code: "download_failed", Message: "network error"}, valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			m.References[0].DownloadState = tt.state
			m.References[0].Failure = tt.failure
			if err := m.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestLoadRejectsManifestLargerThan64MiB(t *testing.T) {
	fs := openTestFS(t)
	path, err := fs.DisplayPath("collection.json")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxManifestBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Load(fs, "collection.json")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load() error = %v, want ErrInvalid for oversized manifest", err)
	}
}

func TestWriteLoadRoundTripAndUTCNormalization(t *testing.T) {
	fs := openTestFS(t)
	want := testManifest()
	if err := Write(fs, "collection.json", want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := Load(fs, "collection.json")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Collection.SourceCreated == nil || got.Collection.SourceCreated.Location() != time.UTC {
		t.Fatalf("source timestamp location = %v, want UTC", got.Collection.SourceCreated.Location())
	}
	if got.Collection.SourceCreated.Hour() != 17 {
		t.Fatalf("source timestamp = %v, want UTC conversion", got.Collection.SourceCreated)
	}
	want.Normalize()
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("round trip changed manifest:\n got: %#v\nwant: %#v", *got, want)
	}

	second := openTestFS(t)
	if err := Write(second, "collection.json", *got); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	firstPath, err := fs.DisplayPath("collection.json")
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := second.DisplayPath("collection.json")
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("manifest output is not deterministic")
	}
	if strings.Contains(string(firstBytes), "https://") {
		t.Fatal("manifest contains a source URL")
	}
	info, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotPerms := info.Mode().Perm(); gotPerms != 0600 {
		t.Fatalf("manifest permissions = %04o, want 0600", gotPerms)
	}
}

func TestLoadMissingIsDistinguishable(t *testing.T) {
	fs := openTestFS(t)
	_, err := Load(fs, "missing.json")
	if !errors.Is(err, ErrNotFound) || !IsNotFound(err) {
		t.Fatalf("missing error = %v, want ErrNotFound", err)
	}
}

func TestLoadRejectsUnknownVersion(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.AtomicReplace("collection.json", func(w io.Writer) error {
		_, err := w.Write([]byte(`{"schema_version":99,"collection":{"id":"c"},"sets":[],"references":[]}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := Load(fs, "collection.json")
	if err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("Load() error = %v, want unknown schema version", err)
	}
}

func TestValidateRejectsDuplicateIDsPlacementsAndTraversal(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "duplicate set ID", edit: func(m *Manifest) { m.Sets = append(m.Sets, m.Sets[0]) }},
		{name: "duplicate reference ID", edit: func(m *Manifest) { m.References = append(m.References, m.References[0]) }},
		{name: "duplicate placement", edit: func(m *Manifest) {
			m.References[0].Placements = append(m.References[0].Placements, m.References[0].Placements[0])
		}},
		{name: "traversal", edit: func(m *Manifest) { m.References[0].Placements[0].Path = "../outside.jpg" }},
		{name: "unknown set", edit: func(m *Manifest) { m.References[0].Placements[0].SetID = "missing" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			tt.edit(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
		})
	}
}

func TestValidateRequiresExactPlacementLayoutAndStableIDs(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		valid bool
	}{
		{name: "collection manifest", path: "collection.json"},
		{name: "unrelated set", path: "Other--set-2/Beach--media-1.jpg"},
		{name: "unrelated reference", path: "Summer--set-1/Other--media-2.jpg"},
		{name: "extra component", path: "Summer--set-1/nested/Beach--media-1.jpg"},
		{name: "missing extension", path: "Summer--set-1/Beach--media-1"},
		{name: "bad set suffix", path: "Summer--set-1-extra/Beach--media-1.jpg"},
		{name: "bad reference suffix", path: "Summer--set-1/Beach--media-1-extra.jpg"},
		{name: "valid names containing marker", path: "Summer--set--1--set-1/Beach--reference--media-1.jpg", valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			m.References[0].Placements[0].Path = tt.path
			if err := m.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestValidateRejectsInvalidPresenceHierarchy(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "missing reference with present placement", edit: func(m *Manifest) {
			m.References[0].PresenceState = PresenceMissing
		}},
		{name: "present reference without present placement", edit: func(m *Manifest) {
			for i := range m.References[0].Placements {
				m.References[0].Placements[i].PresenceState = PresenceMissing
			}
		}},
		{name: "present placement under missing set", edit: func(m *Manifest) {
			m.Sets[0].PresenceState = PresenceMissing
		}},
		{name: "missing Collection with present Set", edit: func(m *Manifest) {
			m.Collection.PresenceState = PresenceMissing
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			tt.edit(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("Validate() succeeded for invalid presence hierarchy")
			}
		})
	}

	emptySet := testManifest()
	emptySet.References = nil
	if err := emptySet.Validate(); err != nil {
		t.Fatalf("Validate() rejected an empty present Set: %v", err)
	}
}

func TestWriteValidationFailurePreservesPreviousFileAndCleansTemp(t *testing.T) {
	fs := openTestFS(t)
	if err := Write(fs, "collection.json", testManifest()); err != nil {
		t.Fatal(err)
	}
	path, err := fs.DisplayPath("collection.json")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := testManifest()
	invalid.References[0].Placements[0].Path = "../../escape"
	if err := Write(fs, "collection.json", invalid); err == nil {
		t.Fatal("Write() succeeded for invalid manifest")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(previous) {
		t.Fatal("validation failure replaced the previous manifest")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pixiegrabber-tmp-") {
			t.Fatalf("temporary files remain: %#v", entries)
		}
	}
}
