package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pixiegrabber/internal/manifest"
	"pixiegrabber/internal/outputfs"
	"pixiegrabber/internal/paths"
	"pixiegrabber/internal/pixieset"
)

var testNow = time.Date(2026, time.August, 10, 15, 0, 0, 0, time.FixedZone("test", -5*60*60))

func testCollection() pixieset.Collection {
	created := testNow.Add(-24 * time.Hour)
	return pixieset.Collection{ID: "100", Name: "Collection", Description: "source", Rank: 1, CreatedAt: &created}
}

func testSet(collectionID, id, name string, rank int, photos ...pixieset.Photo) pixieset.Set {
	return pixieset.Set{ID: id, CollectionID: collectionID, Name: name, Rank: rank, Photos: photos}
}

func testPhoto(collectionID, setID, id, name string, rank int) pixieset.Photo {
	return pixieset.Photo{
		ID: id, CollectionID: collectionID, SetID: setID, Name: name, Description: "description",
		MIMEType: "image/jpeg", Extension: "jpg", Size: 3, Width: 10, Height: 20, Rank: rank,
		ImageVariants: []pixieset.ImageVariant{{Quality: "xxlarge", URL: "https://images.pixieset.com/private/" + id + "/xxlarge"}, {Quality: "large", URL: "https://images.pixieset.com/private/" + id + "/large"}},
	}
}

func displayPath(t *testing.T, fs *outputfs.FS, rel string) string {
	t.Helper()
	path, err := fs.DisplayPath(rel)
	if err != nil {
		t.Fatalf("DisplayPath(%q) error = %v", rel, err)
	}
	return path
}

func buildTest(t *testing.T, fs *outputfs.FS, source pixieset.Collection, sets []pixieset.Set, previous *manifest.Manifest, options Options) Plan {
	t.Helper()
	plan, err := Build(fs, source, sets, previous, options)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if err := plan.Manifest.Validate(); err != nil {
		t.Fatalf("planned manifest is invalid: %v", err)
	}
	return plan
}

func completePlanFile(t *testing.T, fs *outputfs.FS, plan *Plan, content string) {
	t.Helper()
	placement := plan.Manifest.References[0].Placements[0]
	filename := displayPath(t, fs, path.Join(plan.CollectionDir, placement.Path))
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	plan.Manifest.References[0].Placements[0].InstalledSHA256 = hex.EncodeToString(digest[:])
	plan.Manifest.References[0].Placements[0].DownloadState = manifest.DownloadComplete
	plan.Manifest.References[0].Placements[0].Failure = nil
	plan.Manifest.References[0].PresenceState = manifest.PresencePresent
	plan.Manifest.References[0].DownloadState = manifest.DownloadComplete
	plan.Manifest.Collection.RunState = manifest.RunComplete
}

func TestBuildNewEmptyCollection(t *testing.T) {
	plan := buildTest(t, openTestFS(t), testCollection(), nil, nil, Options{Now: testNow})
	if plan.Classification != ClassificationNew || plan.Manifest.Collection.RunState != manifest.RunComplete {
		t.Fatalf("classification/run state = %q/%q", plan.Classification, plan.Manifest.Collection.RunState)
	}
	if len(plan.Manifest.References) != 0 || len(plan.Downloads) != 0 || len(plan.Renames) != 0 {
		t.Fatalf("empty plan contains references, downloads, or renames: %#v", plan)
	}
	want := paths.CollectionComponent(testCollection().Name, testCollection().ID)
	if filepath.IsAbs(plan.CollectionDir) || plan.CollectionDir != want {
		t.Fatalf("CollectionDir = %q, want root-relative component %q", plan.CollectionDir, want)
	}
}

func TestBuildAggregatesRepeatedPhotoAndKeepsPortablePaths(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	photo1 := testPhoto(source.ID, "11", "501", "Reference.jpg", 2)
	photo2 := photo1
	photo2.SetID = "12"
	photo2.Rank = 99
	photo2.ImageVariants = []pixieset.ImageVariant{{Quality: "xxlarge", URL: "https://images.pixieset.com/other/501/xxlarge"}, {Quality: "large", URL: "https://images.pixieset.com/other/501/large"}}
	sets := []pixieset.Set{testSet(source.ID, "12", "Second", 2, photo2), testSet(source.ID, "11", "First", 1, photo1)}
	plan := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	if len(plan.Manifest.References) != 1 || len(plan.Manifest.References[0].Placements) != 2 || len(plan.Downloads) != 1 {
		t.Fatalf("repeated photo plan = %#v", plan)
	}
	if got := plan.Manifest.References[0].SelectedQuality; got != "path_xxlarge" {
		t.Fatalf("selected quality = %q", got)
	}
	if got := plan.Downloads[0].SourceBytes; got != photo1.Size {
		t.Fatalf("source bytes = %d, want %d", got, photo1.Size)
	}
	if got := plan.Downloads[0].Variants[0].URL; got != photo1.ImageVariants[0].URL {
		t.Fatalf("canonical variant URL = %q, want first Set URL", got)
	}
	for _, placement := range plan.Manifest.References[0].Placements {
		if strings.Contains(placement.Path, "\\") || filepath.IsAbs(placement.Path) {
			t.Fatalf("placement path is not portable relative: %q", placement.Path)
		}
	}
	data, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "images.pixieset.com") {
		t.Fatalf("manifest contains an image URL: %s", data)
	}
}

func TestBuildResumesHealthyAndRestoresFiles(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	if first.Classification != ClassificationNew || len(first.Downloads) != 1 {
		t.Fatalf("new plan = %#v", first)
	}
	incomplete := first.Manifest
	resumed := buildTest(t, fs, source, sets, &incomplete, Options{Now: testNow})
	if resumed.Classification != ClassificationIncomplete || len(resumed.Downloads) != 1 {
		t.Fatalf("resume plan classification/downloads = %q/%d", resumed.Classification, len(resumed.Downloads))
	}

	completePlanFile(t, fs, &first, "abc")
	healthyManifest := first.Manifest
	healthy := buildTest(t, fs, source, sets, &healthyManifest, Options{Now: testNow})
	if healthy.Classification != ClassificationHealthy || len(healthy.Downloads) != 0 {
		t.Fatalf("healthy plan classification/downloads = %q/%d", healthy.Classification, len(healthy.Downloads))
	}
	filename := displayPath(t, fs, path.Join(healthy.CollectionDir, healthy.Manifest.References[0].Placements[0].Path))
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	restore := buildTest(t, fs, source, sets, &healthyManifest, Options{Now: testNow})
	if restore.Classification != ClassificationMissingFiles || len(restore.Downloads) != 1 || restore.Manifest.References[0].Placements[0].DownloadState != manifest.DownloadPending || restore.Manifest.References[0].Placements[0].InstalledSHA256 != "" {
		t.Fatalf("restore plan = %#v", restore)
	}
}

func TestHealthyDefaultSkipsDiscoveredSourceChanges(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	previous.References[0].SHA256 = strings.Repeat("c", 64)
	changed := source
	changed.Name = "Changed collection"
	changedSets := []pixieset.Set{testSet(source.ID, "12", "New set", 2, testPhoto(source.ID, "12", "502", "New photo", 2))}
	// The malformed relationship is not read on a healthy default skip.
	changedSets[0].CollectionID = "other"
	skipped := buildTest(t, fs, changed, changedSets, &previous, Options{Now: testNow})
	if skipped.Classification != ClassificationHealthy || len(skipped.Downloads) != 0 || len(skipped.Renames) != 0 {
		t.Fatalf("healthy default did not skip: %#v", skipped)
	}
	if skipped.Manifest.Collection.Name != previous.Collection.Name || !reflect.DeepEqual(skipped.Manifest.Sets, previous.Sets) || !reflect.DeepEqual(skipped.Manifest.References, previous.References) {
		t.Fatalf("healthy default changed prior records: %#v", skipped.Manifest)
	}
	if skipped.CollectionDir != first.CollectionDir {
		t.Fatalf("healthy default changed CollectionDir from %q to %q", first.CollectionDir, skipped.CollectionDir)
	}
}

func TestMissingCollectionReappearsAsIncomplete(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	missing, err := MarkSourceMissing(fs, first.Manifest, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	reappeared := buildTest(t, fs, source, sets, &missing.Manifest, Options{Now: testNow})
	if reappeared.Classification != ClassificationIncomplete || reappeared.Manifest.Collection.PresenceState != manifest.PresencePresent {
		t.Fatalf("reappeared Collection = %#v", reappeared)
	}
	if reappeared.Manifest.References[0].PresenceState != manifest.PresencePresent || reappeared.Manifest.References[0].Placements[0].PresenceState != manifest.PresencePresent {
		t.Fatalf("reappeared records = %#v", reappeared.Manifest)
	}
}

func TestBuildVerifyAndSyncExisting(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	previous.References[0].SHA256 = strings.Repeat("c", 64)
	verified := buildTest(t, fs, source, sets, &previous, Options{Now: testNow, Verify: true})
	if verified.Classification != ClassificationHealthy || len(verified.Downloads) != 0 {
		t.Fatalf("verified matching plan = %#v", verified)
	}
	filename := displayPath(t, fs, path.Join(first.CollectionDir, first.Manifest.References[0].Placements[0].Path))
	if err := os.WriteFile(filename, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	mismatch := buildTest(t, fs, source, sets, &previous, Options{Now: testNow, Verify: true})
	if mismatch.Classification != ClassificationMissingFiles || len(mismatch.Downloads) != 1 {
		t.Fatalf("verified mismatch plan = %#v", mismatch)
	}
	if got := mismatch.Manifest.References[0].Placements[0].InstalledSHA256; got != "" {
		t.Fatalf("mismatched checksum = %q, want empty", got)
	}
	noChecksum := cloneManifest(previous)
	noChecksum.References[0].Placements[0].InstalledSHA256 = ""
	withoutChecksum := buildTest(t, fs, source, sets, &noChecksum, Options{Now: testNow, Verify: true})
	if withoutChecksum.Classification != ClassificationMissingFiles || len(withoutChecksum.Downloads) != 1 {
		t.Fatalf("unverifiable file plan = %#v", withoutChecksum)
	}
	sync := buildTest(t, fs, source, sets, &previous, Options{Now: testNow, SyncExisting: true})
	if len(sync.Downloads) != 1 || len(sync.Downloads[0].Destinations) != 1 || sync.Manifest.References[0].DownloadState != manifest.DownloadPending {
		t.Fatalf("sync plan = %#v", sync)
	}
	if got := sync.Manifest.References[0].Placements[0].InstalledSHA256; got == "" {
		t.Fatal("SyncExisting cleared checksum for an existing regular file")
	}
	if got := sync.Manifest.References[0].SHA256; got != strings.Repeat("c", 64) {
		t.Fatalf("SyncExisting changed historical Reference checksum to %q", got)
	}
}

func TestBuildRenameWithAndWithoutOldFile(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	oldSets := []pixieset.Set{testSet(source.ID, "11", "Old set", 1, testPhoto(source.ID, "11", "501", "Old name", 1))}
	first := buildTest(t, fs, source, oldSets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	newSets := []pixieset.Set{testSet(source.ID, "11", "New set", 1, testPhoto(source.ID, "11", "501", "New name", 1))}
	renamed := buildTest(t, fs, source, newSets, &previous, Options{Now: testNow, SyncExisting: true})
	if len(renamed.Renames) != 1 || len(renamed.Downloads) != 1 || renamed.Manifest.References[0].Placements[0].DownloadState != manifest.DownloadPending {
		t.Fatalf("rename plan = %#v", renamed)
	}
	if renamed.Manifest.References[0].Placements[0].InstalledSHA256 == "" {
		t.Fatal("rename lost the checksum of the existing old file")
	}
	if renamed.Renames[0].From == renamed.Renames[0].To || filepath.IsAbs(renamed.Renames[0].From) || filepath.IsAbs(renamed.Renames[0].To) || strings.Contains(renamed.Renames[0].From, "\\") {
		t.Fatalf("rename paths = %#v", renamed.Renames)
	}
	normalPrevious := previous
	normalPrevious.Collection.RunState = manifest.RunIncomplete
	normal := buildTest(t, fs, source, newSets, &normalPrevious, Options{Now: testNow})
	if len(normal.Renames) != 1 || len(normal.Downloads) != 0 || normal.Manifest.References[0].Placements[0].DownloadState != manifest.DownloadComplete {
		t.Fatalf("normal rename plan = %#v", normal)
	}

	if err := os.Remove(displayPath(t, fs, path.Join(first.CollectionDir, first.Manifest.References[0].Placements[0].Path))); err != nil {
		t.Fatal(err)
	}
	withoutOld := buildTest(t, fs, source, newSets, &previous, Options{Now: testNow, SyncExisting: true})
	if len(withoutOld.Renames) != 0 || len(withoutOld.Downloads) != 1 || withoutOld.Manifest.References[0].Placements[0].DownloadState != manifest.DownloadPending {
		t.Fatalf("rename without old file = %#v", withoutOld)
	}
}

func TestBuildMoveRetainsOldPlacementAndHandlesAdditions(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	photo1 := testPhoto(source.ID, "11", "501", "Photo", 1)
	first := buildTest(t, fs, source, []pixieset.Set{testSet(source.ID, "11", "One", 1, photo1)}, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	movedPhoto := photo1
	movedPhoto.SetID = "12"
	moved := buildTest(t, fs, source, []pixieset.Set{testSet(source.ID, "12", "Two", 2, movedPhoto)}, &previous, Options{Now: testNow, SyncExisting: true})
	if len(moved.Renames) != 0 || len(moved.Downloads) != 1 {
		t.Fatalf("move plan = %#v", moved)
	}
	if len(moved.Manifest.References[0].Placements) != 2 {
		t.Fatalf("move placements = %#v", moved.Manifest.References[0].Placements)
	}
	for _, placement := range moved.Manifest.References[0].Placements {
		if placement.SetID == "11" && (placement.PresenceState != manifest.PresenceMissing || placement.DownloadState != manifest.DownloadComplete) {
			t.Fatalf("old move placement = %#v", placement)
		}
		if placement.SetID == "12" && placement.DownloadState != manifest.DownloadPending {
			t.Fatalf("new move placement = %#v", placement)
		}
	}

	additionalPhoto := testPhoto(source.ID, "11", "502", "Additional", 2)
	added := buildTest(t, fs, source, []pixieset.Set{testSet(source.ID, "11", "One", 1, photo1, additionalPhoto)}, &previous, Options{Now: testNow, SyncExisting: true})
	if len(added.Manifest.References) != 2 || len(added.Downloads) != 2 {
		t.Fatalf("additional placement/reference plan = %#v", added)
	}
	removed := buildTest(t, fs, source, []pixieset.Set{testSet(source.ID, "11", "One", 1, photo1)}, &added.Manifest, Options{Now: testNow})
	for _, reference := range removed.Manifest.References {
		if reference.ID == "502" && reference.PresenceState != manifest.PresenceMissing {
			t.Fatalf("disappeared reference = %#v", reference)
		}
	}
}

func TestBuildRetainsDisappearedSetAndMarkSourceMissing(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{
		testSet(source.ID, "11", "One", 1, testPhoto(source.ID, "11", "501", "One", 1)),
		testSet(source.ID, "12", "Two", 2, testPhoto(source.ID, "12", "502", "Two", 2)),
	}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	missingSet := buildTest(t, fs, source, sets[:1], &first.Manifest, Options{Now: testNow})
	if len(missingSet.Manifest.Sets) != 2 || missingSet.Manifest.Sets[1].PresenceState != manifest.PresenceMissing {
		t.Fatalf("disappeared set = %#v", missingSet.Manifest.Sets)
	}
	sourceMissing, err := MarkSourceMissing(fs, missingSet.Manifest, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceMissing.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if sourceMissing.Classification != ClassificationSourceMissing || len(sourceMissing.Downloads) != 0 || len(sourceMissing.Renames) != 0 {
		t.Fatalf("source missing plan = %#v", sourceMissing)
	}
	if sourceMissing.Manifest.Collection.PresenceState != manifest.PresenceMissing {
		t.Fatalf("Collection presence = %q", sourceMissing.Manifest.Collection.PresenceState)
	}
	for _, set := range sourceMissing.Manifest.Sets {
		if set.PresenceState != manifest.PresenceMissing {
			t.Fatalf("source missing Set = %#v", set)
		}
	}
}

func TestBuildRejectsRelationshipsConflictsAndUnsafeFiles(t *testing.T) {
	source := testCollection()
	fs := openTestFS(t)
	badRelationship := testSet("999", "11", "Bad", 1, testPhoto("999", "11", "501", "Photo", 1))
	if _, err := Build(fs, source, []pixieset.Set{badRelationship}, nil, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted a Set from another Collection")
	}
	left := testPhoto(source.ID, "11", "501", "Photo", 1)
	right := left
	right.SetID = "12"
	right.Name = "different"
	conflictSets := []pixieset.Set{testSet(source.ID, "11", "One", 1, left), testSet(source.ID, "12", "Two", 2, right)}
	_, err := Build(fs, source, conflictSets, nil, Options{Now: testNow})
	if err == nil || strings.Contains(err.Error(), left.ImageVariants[0].URL) {
		t.Fatalf("conflict error = %v", err)
	}

	first := buildTest(t, fs, source, []pixieset.Set{testSet(source.ID, "11", "One", 1, left)}, nil, Options{Now: testNow})
	previous := first.Manifest
	filename := displayPath(t, fs, path.Join(first.CollectionDir, previous.References[0].Placements[0].Path))
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(displayPath(t, fs, "target"), filename); err != nil {
		t.Fatal(err)
	}
	previous.Collection.RunState = manifest.RunComplete
	previous.References[0].DownloadState = manifest.DownloadComplete
	previous.References[0].Placements[0].DownloadState = manifest.DownloadComplete
	if _, err := Build(fs, source, []pixieset.Set{testSet(source.ID, "11", "One", 1, left)}, &previous, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted a symbolic link expected file")
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filename, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs, source, []pixieset.Set{testSet(source.ID, "11", "One", 1, left)}, &previous, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted a non-regular expected file")
	}
}

func TestBuildDeterministicOrderingAndSafeManifest(t *testing.T) {
	source := testCollection()
	sets := []pixieset.Set{
		testSet(source.ID, "12", "Second", 2, testPhoto(source.ID, "12", "503", "Three", 1), testPhoto(source.ID, "12", "501", "One", 1)),
		testSet(source.ID, "11", "First", 1, testPhoto(source.ID, "11", "502", "Two", 1)),
	}
	fs := openTestFS(t)
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	second := buildTest(t, fs, source, []pixieset.Set{sets[1], sets[0]}, nil, Options{Now: testNow})
	if !reflect.DeepEqual(first.Manifest, second.Manifest) || !reflect.DeepEqual(first.Downloads, second.Downloads) || !reflect.DeepEqual(first.Renames, second.Renames) {
		t.Fatalf("plans differ for input order:\nfirst=%#v\nsecond=%#v", first, second)
	}
	for i := range first.Downloads {
		if !reflect.DeepEqual(first.Downloads[i].Destinations, second.Downloads[i].Destinations) {
			t.Fatalf("destinations differ for input order: %#v vs %#v", first.Downloads[i].Destinations, second.Downloads[i].Destinations)
		}
	}
	if first.Manifest.References[0].ID != "501" || first.Manifest.References[1].ID != "502" || first.Manifest.References[2].ID != "503" {
		t.Fatalf("reference order = %#v", first.Manifest.References)
	}
	if data, err := json.Marshal(first.Manifest); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(data), "https://") {
		t.Fatalf("manifest contains a URL: %s", data)
	}
}

func TestMarkSourceMissingPreservesDownloadState(t *testing.T) {
	source := testCollection()
	fs := openTestFS(t)
	first := buildTest(t, fs, source, []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}, nil, Options{Now: testNow})
	first.Manifest.References[0].DownloadState = manifest.DownloadComplete
	first.Manifest.References[0].Placements[0].DownloadState = manifest.DownloadComplete
	first.Manifest.References[0].Placements[0].InstalledSHA256 = strings.Repeat("a", 64)
	missing, err := MarkSourceMissing(fs, first.Manifest, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if missing.Manifest.References[0].DownloadState != manifest.DownloadComplete || missing.Manifest.References[0].Placements[0].InstalledSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("local state was not preserved: %#v", missing.Manifest.References[0])
	}
}

func TestBuildRetriesFailedPlacementAndClearsFailure(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	previous.References[0].SHA256 = strings.Repeat("b", 64)
	previous.References[0].DownloadState = manifest.DownloadFailed
	previous.References[0].Failure = &manifest.Failure{Code: "download_failed", Message: "temporary failure"}
	previous.References[0].Placements[0].DownloadState = manifest.DownloadFailed
	previous.References[0].Placements[0].Failure = &manifest.Failure{Code: "download_failed", Message: "temporary failure"}
	retry := buildTest(t, fs, source, sets, &previous, Options{Now: testNow})
	if len(retry.Downloads) != 1 || retry.Manifest.References[0].DownloadState != manifest.DownloadPending || retry.Manifest.References[0].Failure != nil {
		t.Fatalf("retry reference state = %#v", retry.Manifest.References[0])
	}
	placement := retry.Manifest.References[0].Placements[0]
	if placement.DownloadState != manifest.DownloadPending || placement.Failure != nil {
		t.Fatalf("retry placement state = %#v", placement)
	}
	if placement.InstalledSHA256 != "" || retry.Manifest.References[0].SHA256 != "" {
		t.Fatalf("retry retained stale checksums: placement=%q reference=%q", placement.InstalledSHA256, retry.Manifest.References[0].SHA256)
	}
	missing, err := MarkSourceMissing(fs, previous, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if missing.Manifest.References[0].DownloadState != manifest.DownloadFailed || missing.Manifest.References[0].Placements[0].DownloadState != manifest.DownloadFailed || missing.Manifest.References[0].Placements[0].Failure == nil {
		t.Fatalf("source-missing retry state was not retained: %#v", missing.Manifest.References[0])
	}
}

func TestBuildRejectsSymlinkAndNonDirectoryParents(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	setDir := displayPath(t, fs, path.Join(first.CollectionDir, "Set--11"))
	filename := displayPath(t, fs, path.Join(first.CollectionDir, previous.References[0].Placements[0].Path))
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(setDir); err != nil {
		t.Fatal(err)
	}
	target := displayPath(t, fs, "real-set")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, setDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs, source, sets, &previous, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted a symbolic-link parent")
	}
	if err := os.Remove(setDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs, source, sets, &previous, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted a non-directory parent")
	}
}

func TestBuildRejectsExistingNewAndRenameTargets(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	initial := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	newTarget := displayPath(t, fs, path.Join(initial.CollectionDir, initial.Manifest.References[0].Placements[0].Path))
	if err := os.MkdirAll(filepath.Dir(newTarget), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTarget, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs, source, sets, nil, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted an existing new-placement target")
	}

	fs2 := openTestFS(t)
	oldSets := []pixieset.Set{testSet(source.ID, "11", "Old", 1, testPhoto(source.ID, "11", "501", "Old photo", 1))}
	first := buildTest(t, fs2, source, oldSets, nil, Options{Now: testNow})
	completePlanFile(t, fs2, &first, "abc")
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	newSets := []pixieset.Set{testSet(source.ID, "11", "New", 1, testPhoto(source.ID, "11", "501", "New photo", 1))}
	newPath := portablePlacementPath(newSets[0], newSets[0].Photos[0])
	target := displayPath(t, fs2, path.Join(first.CollectionDir, newPath))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("collision"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs2, source, newSets, &previous, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted an existing rename target")
	}
}

func TestBuildPlansWholeCollectionRenameBeforeFileRenames(t *testing.T) {
	fs := openTestFS(t)
	oldSource := testCollection()
	oldSource.Name = "Old collection"
	oldSets := []pixieset.Set{
		testSet(oldSource.ID, "11", "Old set", 1, testPhoto(oldSource.ID, "11", "501", "Old photo", 1)),
		testSet(oldSource.ID, "12", "Retained", 2, testPhoto(oldSource.ID, "12", "502", "Retained photo", 2)),
	}
	first := buildTest(t, fs, oldSource, oldSets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	// Keep a local record that is no longer in the discovered source.
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	previous.References[1].PresenceState = manifest.PresenceMissing
	previous.References[1].Placements[0].PresenceState = manifest.PresenceMissing
	newSource := oldSource
	newSource.Name = "New collection"
	newSets := []pixieset.Set{testSet(newSource.ID, "11", "New set", 1, testPhoto(newSource.ID, "11", "501", "New photo", 1))}
	planned := buildTest(t, fs, newSource, newSets, &previous, Options{Now: testNow, SyncExisting: true})
	if len(planned.Renames) != 2 || planned.Renames[0].From != first.CollectionDir || planned.Renames[0].To != planned.CollectionDir {
		t.Fatalf("Collection rename operations = %#v", planned.Renames)
	}
	if planned.Renames[1].From != path.Join(planned.CollectionDir, "Old set--11", "Old photo--501.jpg") || planned.Renames[1].To != path.Join(planned.CollectionDir, "New set--11", "New photo--501.jpg") {
		t.Fatalf("dependent rename paths = %#v", planned.Renames)
	}
	if planned.Manifest.References[1].PresenceState != manifest.PresenceMissing || planned.Manifest.References[1].Placements[0].PresenceState != manifest.PresenceMissing {
		t.Fatalf("retained missing records = %#v", planned.Manifest.References[1])
	}

	emptySource := testCollection()
	emptySource.Name = "Empty old"
	empty := buildTest(t, fs, emptySource, nil, nil, Options{Now: testNow})
	if err := os.MkdirAll(displayPath(t, fs, empty.CollectionDir), 0700); err != nil {
		t.Fatal(err)
	}
	emptyPrevious := empty.Manifest
	emptyPrevious.Collection.RunState = manifest.RunIncomplete
	emptyNewSource := emptySource
	emptyNewSource.Name = "Empty new"
	emptyMoved := buildTest(t, fs, emptyNewSource, nil, &emptyPrevious, Options{Now: testNow, SyncExisting: true})
	if len(emptyMoved.Renames) != 1 || emptyMoved.Renames[0].From != empty.CollectionDir || emptyMoved.Renames[0].To != emptyMoved.CollectionDir {
		t.Fatalf("empty Collection rename = %#v", emptyMoved.Renames)
	}
}

func TestBuildRejectsCollectionRenameCollisionsAndUnsafeRoots(t *testing.T) {
	fs := openTestFS(t)
	oldSource := testCollection()
	oldSource.Name = "Old"
	first := buildTest(t, fs, oldSource, nil, nil, Options{Now: testNow})
	if err := os.MkdirAll(displayPath(t, fs, first.CollectionDir), 0700); err != nil {
		t.Fatal(err)
	}
	newSource := oldSource
	newSource.Name = "New"
	newDir := collectionComponent(newSource.Name, newSource.ID)
	if err := os.MkdirAll(displayPath(t, fs, newDir), 0700); err != nil {
		t.Fatal(err)
	}
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	if _, err := Build(fs, newSource, nil, &previous, Options{Now: testNow, SyncExisting: true}); err == nil {
		t.Fatal("Build accepted a Collection rename collision")
	}

	fs2 := openTestFS(t)
	first = buildTest(t, fs2, oldSource, nil, nil, Options{Now: testNow})
	if err := os.Symlink(displayPath(t, fs2, "target"), displayPath(t, fs2, first.CollectionDir)); err != nil {
		t.Fatal(err)
	}
	previous = first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	if _, err := Build(fs2, newSource, nil, &previous, Options{Now: testNow, SyncExisting: true}); err == nil {
		t.Fatal("Build accepted a symbolic-link old Collection root")
	}
}

func TestBuildKeepsPriorCollectionRootWhenSourceRootIsAbsent(t *testing.T) {
	fs := openTestFS(t)
	oldSource := testCollection()
	oldSource.Name = "Old"
	first := buildTest(t, fs, oldSource, nil, nil, Options{Now: testNow})
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	newSource := oldSource
	newSource.Name = "New"
	planned := buildTest(t, fs, newSource, nil, &previous, Options{Now: testNow, SyncExisting: true})
	if planned.CollectionDir != first.CollectionDir || planned.Manifest.Collection.Name != oldSource.Name || len(planned.Renames) != 0 {
		t.Fatalf("unsafe missing-root fallback = %#v", planned)
	}
}

func TestBuildRejectsRootMigrationPlacementAndDownloadCollisions(t *testing.T) {
	fs := openTestFS(t)
	oldSource := testCollection()
	oldSource.Name = "Old"
	oldSets := []pixieset.Set{testSet(oldSource.ID, "11", "Old set", 1, testPhoto(oldSource.ID, "11", "501", "Old photo", 1))}
	first := buildTest(t, fs, oldSource, oldSets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	newSource := oldSource
	newSource.Name = "New"
	newSets := []pixieset.Set{testSet(newSource.ID, "11", "New set", 1, testPhoto(newSource.ID, "11", "501", "New photo", 1))}
	placementTarget := displayPath(t, fs, path.Join(first.CollectionDir, portablePlacementPath(newSets[0], newSets[0].Photos[0])))
	if err := os.MkdirAll(filepath.Dir(placementTarget), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placementTarget, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs, newSource, newSets, &previous, Options{Now: testNow, SyncExisting: true}); err == nil {
		t.Fatal("Build planned a placement rename over an old-root collision")
	}

	fs2 := openTestFS(t)
	first = buildTest(t, fs2, oldSource, oldSets, nil, Options{Now: testNow})
	completePlanFile(t, fs2, &first, "abc")
	previous = first.Manifest
	newSet := testSet(newSource.ID, "12", "Added set", 2, testPhoto(newSource.ID, "12", "502", "Added photo", 2))
	downloadTarget := displayPath(t, fs2, path.Join(first.CollectionDir, portablePlacementPath(newSet, newSet.Photos[0])))
	if err := os.MkdirAll(filepath.Dir(downloadTarget), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(downloadTarget, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(fs2, newSource, append(newSets[:1], newSet), &previous, Options{Now: testNow, SyncExisting: true}); err == nil {
		t.Fatal("Build planned a download over an old-root collision")
	}
}

func TestBuildValidatesOutputAndCollectionRootsForEmptyPlans(t *testing.T) {
	source := testCollection()
	t.Run("output root symlink", func(t *testing.T) {
		realRoot := t.TempDir()
		link := filepath.Join(realRoot, "link")
		if err := os.Symlink(realRoot, link); err != nil {
			t.Skipf("symlinks are not supported: %v", err)
		}
		if _, err := outputfs.Open(link); err == nil {
			t.Fatal("outputfs.Open accepted a symbolic-link output root")
		}
	})
	t.Run("output root wrong type", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "root-file")
		if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := outputfs.Open(root); err == nil {
			t.Fatal("outputfs.Open accepted a non-directory output root")
		}
	})
	t.Run("Collection root symlink on every return path", func(t *testing.T) {
		fs := openTestFS(t)
		initial := buildTest(t, fs, source, nil, nil, Options{Now: testNow})
		target := displayPath(t, fs, "real-Collection")
		if err := os.MkdirAll(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, displayPath(t, fs, initial.CollectionDir)); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(fs, source, nil, &initial.Manifest, Options{Now: testNow}); err == nil {
			t.Fatal("healthy empty plan accepted a symbolic-link Collection root")
		}
		if _, err := MarkSourceMissing(fs, initial.Manifest, testNow); err == nil {
			t.Fatal("source-missing empty plan accepted a symbolic-link Collection root")
		}
	})
	t.Run("Collection root wrong type", func(t *testing.T) {
		fs := openTestFS(t)
		collectionDir := collectionComponent(source.Name, source.ID)
		if err := os.WriteFile(displayPath(t, fs, collectionDir), []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(fs, source, nil, nil, Options{Now: testNow}); err == nil {
			t.Fatal("Build accepted a non-directory Collection root")
		}
	})
}

func TestBuildMergesRepeatedReferenceVariantsDeterministically(t *testing.T) {
	source := testCollection()
	firstPhoto := testPhoto(source.ID, "11", "501", "Photo", 9)
	firstPhoto.ImageVariants = []pixieset.ImageVariant{
		{Quality: "xxlarge", URL: "https://images.pixieset.com/low-rank/xxlarge"},
		{Quality: "large", URL: "https://images.pixieset.com/low-rank/large"},
	}
	secondPhoto := testPhoto(source.ID, "12", "501", "Photo", 1)
	secondPhoto.ImageVariants = []pixieset.ImageVariant{
		{Quality: "xxlarge", URL: "https://images.pixieset.com/high-rank/xxlarge"},
		{Quality: "medium", URL: "https://images.pixieset.com/high-rank/medium"},
	}
	sets := []pixieset.Set{
		testSet(source.ID, "12", "Second", 2, secondPhoto),
		testSet(source.ID, "11", "First", 1, firstPhoto),
	}
	fs := openTestFS(t)
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	second := buildTest(t, fs, source, []pixieset.Set{sets[1], sets[0]}, nil, Options{Now: testNow})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated-reference plans differ:\nfirst=%#v\nsecond=%#v", first, second)
	}
	wantVariants := []pixieset.ImageVariant{
		{Quality: "xxlarge", URL: "https://images.pixieset.com/low-rank/xxlarge"},
		{Quality: "large", URL: "https://images.pixieset.com/low-rank/large"},
		{Quality: "medium", URL: "https://images.pixieset.com/high-rank/medium"},
	}
	if got := first.Downloads[0].Variants; !reflect.DeepEqual(got, wantVariants) {
		t.Fatalf("merged variants = %#v", got)
	}
}

func TestBuildPlansCaseOnlyCollectionAndPlacementRenames(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	source.Name = "Collection"
	oldSets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, oldSets, nil, Options{Now: testNow})
	completePlanFile(t, fs, &first, "abc")
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	changed := source
	changed.Name = "collection"
	newSets := []pixieset.Set{testSet(changed.ID, "11", "set", 1, testPhoto(changed.ID, "11", "501", "photo", 1))}
	newCollectionDir := collectionComponent(changed.Name, changed.ID)
	oldCollectionInfo, err := os.Stat(displayPath(t, fs, first.CollectionDir))
	if err != nil {
		t.Fatal(err)
	}
	newCollectionInfo, newCollectionErr := os.Stat(displayPath(t, fs, newCollectionDir))
	caseInsensitive := newCollectionErr == nil && os.SameFile(oldCollectionInfo, newCollectionInfo)
	if caseInsensitive {
		oldPlacementPath := displayPath(t, fs, path.Join(first.CollectionDir, previous.References[0].Placements[0].Path))
		newPlacementPath := displayPath(t, fs, path.Join(first.CollectionDir, portablePlacementPath(newSets[0], newSets[0].Photos[0])))
		oldPlacementInfo, err := os.Stat(oldPlacementPath)
		if err != nil {
			t.Fatal(err)
		}
		newPlacementInfo, err := os.Stat(newPlacementPath)
		if err != nil || !os.SameFile(oldPlacementInfo, newPlacementInfo) {
			t.Fatalf("case-insensitive placement collision is not the same inode: err=%v", err)
		}
	} else {
		t.Log("case-only same-inode collision branch skipped on a case-sensitive filesystem")
	}
	planned := buildTest(t, fs, changed, newSets, &previous, Options{Now: testNow})
	wantRenames := []Rename{
		{From: first.CollectionDir, To: planned.CollectionDir},
		{From: path.Join(planned.CollectionDir, "Set--11", "Photo--501.jpg"), To: path.Join(planned.CollectionDir, "set--11", "photo--501.jpg")},
	}
	if !reflect.DeepEqual(planned.Renames, wantRenames) {
		t.Fatalf("case-only rename operations = %#v, want %#v", planned.Renames, wantRenames)
	}
}

func TestBuildRejectsPriorPlacementOutsideExactLayout(t *testing.T) {
	fs := openTestFS(t)
	source := testCollection()
	sets := []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}
	first := buildTest(t, fs, source, sets, nil, Options{Now: testNow})
	if err := os.MkdirAll(displayPath(t, fs, first.CollectionDir), 0700); err != nil {
		t.Fatal(err)
	}
	collectionFile := displayPath(t, fs, path.Join(first.CollectionDir, manifest.ManifestFilename))
	if err := os.WriteFile(collectionFile, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := first.Manifest
	previous.Collection.RunState = manifest.RunIncomplete
	previous.References[0].Placements[0].Path = manifest.ManifestFilename
	if _, err := Build(fs, source, sets, &previous, Options{Now: testNow}); err == nil {
		t.Fatal("Build accepted a prior Placement targeting collection.json")
	}
}

func TestManifestNeedsWorkScansPresentPlacementsOfMissingReferences(t *testing.T) {
	source := testCollection()
	plan := buildTest(t, openTestFS(t), source, []pixieset.Set{testSet(source.ID, "11", "Set", 1, testPhoto(source.ID, "11", "501", "Photo", 1))}, nil, Options{Now: testNow})
	m := plan.Manifest
	m.References[0].PresenceState = manifest.PresenceMissing
	if !manifestNeedsWork(m) {
		t.Fatal("manifestNeedsWork skipped a present Placement under a missing Reference")
	}
}
