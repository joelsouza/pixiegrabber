// Package archive builds side-effect-free plans for a Collection archive.
package archive

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"pixiegrabber/internal/manifest"
	"pixiegrabber/internal/paths"
	"pixiegrabber/internal/pixieset"
	"pixiegrabber/internal/store"
)

// Classification is the reason a plan was created.
type Classification string

const (
	ClassificationNew           Classification = "new"
	ClassificationIncomplete    Classification = "incomplete"
	ClassificationHealthy       Classification = "healthy"
	ClassificationMissingFiles  Classification = "missing_files"
	ClassificationSourceMissing Classification = "source_missing"
)

// Options controls classification and the amount of work in a plan.
type Options struct {
	SyncExisting bool
	Verify       bool

	// Now is used for manifest timestamps. A zero value uses the current UTC
	// time. The value is converted to UTC before it is stored.
	Now time.Time
}

// Plan contains the next manifest and transient work for one Collection.
// Download URLs exist only in Downloads and are never copied to Manifest.
type Plan struct {
	Classification Classification
	CollectionDir  string
	Manifest       manifest.Manifest
	Downloads      []DownloadWork
	Renames        []Rename
}

// DownloadWork is the work for one Collection-scoped Reference.
type DownloadWork struct {
	ReferenceID  string
	Variants     []pixieset.ImageVariant
	SourceBytes  int64
	Destinations []Destination
}

// Destination is a present Placement that needs a local write. RelativePath is
// the root-relative slash-separated path with Collection/Set/Reference
// components.
type Destination struct {
	SetID        string
	RelativePath string
}

// Rename describes a local path move. The planner does not perform it. The
// executor must use a unique temporary sibling for case-only renames and keep
// no-overwrite checks in place.
type Rename struct {
	From string
	To   string
}

// Build makes a plan for source and its discovered Sets. It does not create
// directories, rename files, download media, or write a manifest. The caller
// must hold the output-root lock.
func Build(s store.Store, source pixieset.Collection, sets []pixieset.Set, previous *manifest.Manifest, options Options) (Plan, error) {
	now := optionTime(options)
	collectionDir := collectionComponent(source.Name, source.ID)
	if err := validInputID("Collection", source.ID); err != nil {
		return Plan{}, err
	}

	classification, prior, err := classify(s, source, previous, options)
	if err != nil {
		return Plan{}, err
	}
	if prior != nil && classification == ClassificationHealthy && !options.SyncExisting {
		// A healthy default run is a discovery skip. Do not validate or merge
		// newly discovered Sets because the caller did not request a sync.
		prior.Collection.LastDiscoveryAt = timePointer(now)
		prior.Normalize()
		if err := prior.Validate(); err != nil {
			return Plan{}, fmt.Errorf("validate healthy manifest: %w", err)
		}
		priorDir := collectionComponent(prior.Collection.Name, prior.Collection.ID)
		if err := validateCollectionRoot(s, priorDir); err != nil {
			return Plan{}, err
		}
		return Plan{Classification: classification, CollectionDir: priorDir, Manifest: *prior}, nil
	}
	if prior == nil {
		if err := validateCollectionRoot(s, collectionDir); err != nil {
			return Plan{}, err
		}
	}

	orderedSets, setByID, err := validateSets(source, sets)
	if err != nil {
		return Plan{}, err
	}
	currentReferences, err := aggregateReferences(orderedSets)
	if err != nil {
		return Plan{}, err
	}

	next, downloads, renames, err := merge(source, orderedSets, setByID, currentReferences, prior, collectionDir, s, options)
	if err != nil {
		return Plan{}, err
	}
	next.Collection.LastDiscoveryAt = timePointer(now)
	next.Normalize()
	if err := next.Validate(); err != nil {
		return Plan{}, fmt.Errorf("validate planned manifest: %w", err)
	}

	plannedDir := collectionComponent(next.Collection.Name, next.Collection.ID)
	return Plan{Classification: classification, CollectionDir: plannedDir, Manifest: next, Downloads: downloads, Renames: renames}, nil
}

// MarkSourceMissing makes a no-work plan for a Collection that was not in
// discovery. Local records and download state are retained. The caller must
// hold the output-root lock.
func MarkSourceMissing(s store.Store, previous manifest.Manifest, now time.Time) (Plan, error) {
	previous = cloneManifest(previous)
	previous.Normalize()
	if err := previous.Validate(); err != nil {
		return Plan{}, fmt.Errorf("validate previous manifest: %w", err)
	}
	collectionDir := collectionComponent(previous.Collection.Name, previous.Collection.ID)
	if err := validateCollectionRoot(s, collectionDir); err != nil {
		return Plan{}, err
	}

	next := cloneManifest(previous)
	next.Collection.PresenceState = manifest.PresenceMissing
	next.Collection.LastDiscoveryAt = timePointer(now)
	for i := range next.Sets {
		next.Sets[i].PresenceState = manifest.PresenceMissing
	}
	for i := range next.References {
		next.References[i].PresenceState = manifest.PresenceMissing
		for j := range next.References[i].Placements {
			next.References[i].Placements[j].PresenceState = manifest.PresenceMissing
		}
	}
	next.Normalize()
	if err := next.Validate(); err != nil {
		return Plan{}, fmt.Errorf("validate source-missing manifest: %w", err)
	}
	return Plan{
		Classification: ClassificationSourceMissing,
		CollectionDir:  collectionDir,
		Manifest:       next,
		Downloads:      []DownloadWork{},
		Renames:        []Rename{},
	}, nil
}

type currentReference struct {
	photo  pixieset.Photo
	setIDs []string
}

func validateSets(source pixieset.Collection, sets []pixieset.Set) ([]pixieset.Set, map[string]pixieset.Set, error) {
	if err := validInputID("Collection", source.ID); err != nil {
		return nil, nil, err
	}
	ordered := append([]pixieset.Set(nil), sets...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Rank != ordered[j].Rank {
			return ordered[i].Rank < ordered[j].Rank
		}
		return ordered[i].ID < ordered[j].ID
	})
	byID := make(map[string]pixieset.Set, len(ordered))
	for _, set := range ordered {
		if err := validInputID("Set", set.ID); err != nil {
			return nil, nil, err
		}
		if set.CollectionID != source.ID {
			return nil, nil, fmt.Errorf("set %q does not belong to collection %q", set.ID, source.ID)
		}
		if _, exists := byID[set.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate Set ID %q", set.ID)
		}
		byID[set.ID] = set
		seenPhotos := make(map[string]struct{}, len(set.Photos))
		for _, photo := range set.Photos {
			if err := validInputID("photo", photo.ID); err != nil {
				return nil, nil, err
			}
			if photo.CollectionID != source.ID || photo.SetID != set.ID {
				return nil, nil, fmt.Errorf("photo %q has an invalid Collection or Set relationship", photo.ID)
			}
			if _, exists := seenPhotos[photo.ID]; exists {
				return nil, nil, fmt.Errorf("duplicate photo ID %q in Set %q", photo.ID, set.ID)
			}
			seenPhotos[photo.ID] = struct{}{}
		}
	}
	return ordered, byID, nil
}

func aggregateReferences(sets []pixieset.Set) ([]currentReference, error) {
	byID := make(map[string]*currentReference)
	// Sets are already ordered by source rank and ID. The first photo is the
	// canonical source for rank and transient variant URLs.
	for _, set := range sets {
		for _, photo := range set.Photos {
			if existing, ok := byID[photo.ID]; ok {
				if !sameIntrinsicPhotoMetadata(existing.photo, photo) {
					return nil, fmt.Errorf("photo %q has conflicting normalized metadata", photo.ID)
				}
				existing.photo.ImageVariants = mergeVariants(existing.photo.ImageVariants, photo.ImageVariants)
				existing.setIDs = append(existing.setIDs, set.ID)
				continue
			}
			byID[photo.ID] = &currentReference{photo: clonePhoto(photo), setIDs: []string{set.ID}}
		}
	}
	result := make([]currentReference, 0, len(byID))
	for _, reference := range byID {
		result = append(result, *reference)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].photo.Rank != result[j].photo.Rank {
			return result[i].photo.Rank < result[j].photo.Rank
		}
		return result[i].photo.ID < result[j].photo.ID
	})
	return result, nil
}

func classify(s store.Store, source pixieset.Collection, previous *manifest.Manifest, options Options) (Classification, *manifest.Manifest, error) {
	if previous == nil {
		return ClassificationNew, nil, nil
	}
	prior := cloneManifest(*previous)
	prior.Normalize()
	if err := prior.Validate(); err != nil {
		return "", nil, fmt.Errorf("validate previous manifest: %w", err)
	}
	if prior.Collection.ID != source.ID {
		return "", nil, fmt.Errorf("previous manifest is for Collection %q, not %q", prior.Collection.ID, source.ID)
	}
	oldDir := collectionComponent(prior.Collection.Name, prior.Collection.ID)
	missing, err := inspectManifestFiles(s, oldDir, prior, options.Verify)
	if err != nil {
		return "", nil, err
	}
	if prior.Collection.PresenceState != manifest.PresencePresent {
		return ClassificationIncomplete, &prior, nil
	}
	if prior.Collection.RunState != manifest.RunComplete || manifestNeedsWork(prior) {
		return ClassificationIncomplete, &prior, nil
	}
	if missing {
		return ClassificationMissingFiles, &prior, nil
	}
	return ClassificationHealthy, &prior, nil
}

func inspectManifestFiles(s store.Store, collectionDir string, m manifest.Manifest, verify bool) (bool, error) {
	missing := false
	for _, reference := range m.References {
		for _, placement := range reference.Placements {
			if placement.PresenceState != manifest.PresencePresent {
				continue
			}
			exists, checksumOK, err := inspectFile(s, collectionDir, placement.Path, placement.InstalledSHA256, verify)
			if err != nil {
				return false, err
			}
			if !exists || (verify && !checksumOK) {
				missing = true
			}
		}
	}
	return missing, nil
}

type collectionRootPlan struct {
	directory          string
	rootRename         *Rename
	preserveCollection bool
}

func planCollectionRoot(s store.Store, currentDir string, prior *manifest.Manifest) (collectionRootPlan, error) {
	if prior == nil {
		return collectionRootPlan{directory: currentDir}, nil
	}
	oldDir := collectionComponent(prior.Collection.Name, prior.Collection.ID)
	if oldDir == currentDir {
		if err := validateCollectionRoot(s, currentDir); err != nil {
			return collectionRootPlan{}, err
		}
		return collectionRootPlan{directory: currentDir}, nil
	}
	oldInfo, oldExists, err := s.Inspect(oldDir)
	if err != nil {
		return collectionRootPlan{}, err
	}
	if oldExists && !oldInfo.IsDir() {
		return collectionRootPlan{}, fmt.Errorf("inspect Collection directory: old Collection root is not a directory")
	}
	_, newExists, err := s.Inspect(currentDir)
	if err != nil {
		return collectionRootPlan{}, err
	}
	if newExists {
		sameSource, samePath, err := sameExistingPath(s, oldDir, currentDir)
		if err != nil {
			return collectionRootPlan{}, err
		}
		if !oldExists || !sameSource || !samePath {
			return collectionRootPlan{}, fmt.Errorf("rename Collection directory: destination already exists")
		}
	}
	if !oldExists {
		// A Rename cannot safely represent a missing source root. Keep the
		// prior root and Collection name instead of partially migrating it.
		return collectionRootPlan{directory: oldDir, preserveCollection: true}, nil
	}
	rename := Rename{From: oldDir, To: currentDir}
	return collectionRootPlan{directory: currentDir, rootRename: &rename}, nil
}

func merge(source pixieset.Collection, orderedSets []pixieset.Set, setByID map[string]pixieset.Set, currentReferences []currentReference, prior *manifest.Manifest, collectionDir string, s store.Store, options Options) (manifest.Manifest, []DownloadWork, []Rename, error) {
	rootPlan, err := planCollectionRoot(s, collectionDir, prior)
	if err != nil {
		return manifest.Manifest{}, nil, nil, err
	}
	var next manifest.Manifest
	if prior == nil {
		next = manifest.Manifest{SchemaVersion: manifest.CurrentSchemaVersion}
	} else {
		next = cloneManifest(*prior)
	}
	next.SchemaVersion = manifest.CurrentSchemaVersion
	next.Collection.ID = source.ID
	if !rootPlan.preserveCollection {
		next.Collection.Name = source.Name
		next.Collection.Description = source.Description
		next.Collection.SourceCreated = cloneTime(source.CreatedAt)
		next.Collection.SourceUpdated = nil
	}
	next.Collection.PresenceState = manifest.PresencePresent

	oldCollectionDir := collectionDir
	if prior != nil {
		oldCollectionDir = collectionComponent(prior.Collection.Name, prior.Collection.ID)
	}

	oldSets := make(map[string]manifest.Set, len(next.Sets))
	for _, set := range next.Sets {
		oldSets[set.ID] = set
	}
	next.Sets = make([]manifest.Set, 0, len(oldSets)+len(orderedSets))
	for _, sourceSet := range orderedSets {
		set, exists := oldSets[sourceSet.ID]
		if !exists {
			set = manifest.Set{ID: sourceSet.ID}
		}
		set.Name = sourceSet.Name
		set.Description = sourceSet.Description
		set.SourceOrder = sourceSet.Rank
		set.SourceCreated = nil
		set.SourceUpdated = nil
		set.PresenceState = manifest.PresencePresent
		next.Sets = append(next.Sets, set)
	}
	for _, set := range oldSets {
		if _, exists := setByID[set.ID]; exists {
			continue
		}
		set.PresenceState = manifest.PresenceMissing
		next.Sets = append(next.Sets, set)
	}
	sort.SliceStable(next.Sets, func(i, j int) bool {
		if next.Sets[i].SourceOrder != next.Sets[j].SourceOrder {
			return next.Sets[i].SourceOrder < next.Sets[j].SourceOrder
		}
		return next.Sets[i].ID < next.Sets[j].ID
	})

	oldReferences := make(map[string]manifest.Reference, len(next.References))
	if prior != nil {
		for _, reference := range next.References {
			oldReferences[reference.ID] = reference
		}
	}
	next.References = make([]manifest.Reference, 0, len(oldReferences)+len(currentReferences))
	var downloads []DownloadWork
	var renames []Rename
	for _, current := range currentReferences {
		reference, existed := oldReferences[current.photo.ID]
		delete(oldReferences, current.photo.ID)
		if !existed {
			reference = manifest.Reference{ID: current.photo.ID, Placements: []manifest.Placement{}}
		}
		applyPhotoMetadata(&reference, current.photo)
		reference.PresenceState = manifest.PresencePresent

		oldPlacements := make(map[string]manifest.Placement, len(reference.Placements))
		for _, placement := range reference.Placements {
			oldPlacements[placement.SetID] = placement
		}
		reference.Placements = make([]manifest.Placement, 0, len(current.setIDs)+len(oldPlacements))
		work := DownloadWork{ReferenceID: reference.ID, Variants: cloneVariants(current.photo.ImageVariants), SourceBytes: current.photo.Size}
		trustedPresent := 0
		untrustedReset := false
		for _, setID := range current.setIDs {
			sourceSet := setByID[setID]
			relativePath := portablePlacementPath(sourceSet, current.photo)
			placement, existed := oldPlacements[setID]
			delete(oldPlacements, setID)
			if !existed {
				currentExists, _, err := inspectFile(s, rootPlan.directory, relativePath, "", false)
				if err != nil {
					return manifest.Manifest{}, nil, nil, err
				}
				if currentExists {
					return manifest.Manifest{}, nil, nil, fmt.Errorf("plan download destination %q: destination already exists", relativePath)
				}
				if rootPlan.rootRename != nil {
					eventualExists, _, err := inspectFile(s, oldCollectionDir, relativePath, "", false)
					if err != nil {
						return manifest.Manifest{}, nil, nil, err
					}
					if eventualExists {
						return manifest.Manifest{}, nil, nil, fmt.Errorf("plan download destination %q: destination already exists after Collection rename", relativePath)
					}
				}
				placement = manifest.Placement{SetID: setID, Path: relativePath, PresenceState: manifest.PresencePresent, DownloadState: manifest.DownloadPending}
				untrustedReset = true
			} else {
				oldRelativePath := placement.Path
				priorDownloadState := placement.DownloadState
				oldExists, oldChecksumOK, err := inspectFile(s, oldCollectionDir, oldRelativePath, placement.InstalledSHA256, options.Verify)
				if err != nil {
					return manifest.Manifest{}, nil, nil, err
				}
				currentExists, _, err := inspectFile(s, rootPlan.directory, relativePath, "", false)
				if err != nil {
					return manifest.Manifest{}, nil, nil, err
				}
				if rootPlan.rootRename != nil {
					eventualExists, _, err := inspectFile(s, oldCollectionDir, relativePath, "", false)
					if err != nil {
						return manifest.Manifest{}, nil, nil, err
					}
					if eventualExists {
						sameSource, err := sameRegularFile(s, oldCollectionDir, oldRelativePath, relativePath)
						if err != nil {
							return manifest.Manifest{}, nil, nil, err
						}
						if !sameSource || (oldRelativePath != relativePath && !sameCaseOnlyPath(path.Join(oldCollectionDir, oldRelativePath), path.Join(oldCollectionDir, relativePath))) {
							return manifest.Manifest{}, nil, nil, fmt.Errorf("rename placement %q: destination already exists after Collection rename", relativePath)
						}
					}
				}
				pathChanged := oldRelativePath != relativePath
				checksumInvalid := options.Verify && oldExists && !oldChecksumOK
				trustedExisting := priorDownloadState == manifest.DownloadComplete && oldExists && !checksumInvalid
				if pathChanged && currentExists {
					sameSource, err := sameRegularFile(s, rootPlan.directory, oldRelativePath, relativePath)
					if err != nil {
						return manifest.Manifest{}, nil, nil, err
					}
					if !sameSource || !sameCaseOnlyPath(path.Join(rootPlan.directory, oldRelativePath), path.Join(rootPlan.directory, relativePath)) {
						return manifest.Manifest{}, nil, nil, fmt.Errorf("rename placement %q: destination already exists", relativePath)
					}
				}
				if pathChanged && trustedExisting {
					fromRoot := oldCollectionDir
					if rootPlan.rootRename != nil {
						fromRoot = rootPlan.directory
					}
					renames = append(renames, Rename{From: path.Join(fromRoot, oldRelativePath), To: path.Join(rootPlan.directory, relativePath)})
				}
				placement.Path = relativePath
				placement.PresenceState = manifest.PresencePresent
				if trustedExisting {
					trustedPresent++
				}
				if options.SyncExisting {
					setPending(&placement)
					if !trustedExisting {
						placement.InstalledSHA256 = ""
						untrustedReset = true
					}
				} else if placement.DownloadState == manifest.DownloadFailed {
					setPending(&placement)
					placement.InstalledSHA256 = ""
					untrustedReset = true
				} else if placement.DownloadState != manifest.DownloadComplete || !oldExists || checksumInvalid {
					setPending(&placement)
					placement.InstalledSHA256 = ""
					untrustedReset = true
				} else {
					placement.Failure = nil
				}
			}
			reference.Placements = append(reference.Placements, placement)
			if placement.PresenceState == manifest.PresencePresent && placement.DownloadState != manifest.DownloadComplete {
				work.Destinations = append(work.Destinations, Destination{
					SetID: setID, RelativePath: path.Join(rootPlan.directory, relativePath),
				})
			}
		}
		for _, oldPlacement := range oldPlacements {
			oldPlacement.PresenceState = manifest.PresenceMissing
			reference.Placements = append(reference.Placements, oldPlacement)
		}
		sortPlacements(&reference, setRanks(next.Sets))
		setReferenceDownloadState(&reference, existed)
		if untrustedReset && trustedPresent == 0 {
			// Reference SHA256 is historical executor state. Clear it only when
			// no current Placement still proves that historical content locally.
			reference.SHA256 = ""
		}
		if len(work.Destinations) != 0 {
			downloads = append(downloads, work)
		}
		next.References = append(next.References, reference)
	}
	for _, oldReference := range oldReferences {
		oldReference.PresenceState = manifest.PresenceMissing
		for i := range oldReference.Placements {
			oldReference.Placements[i].PresenceState = manifest.PresenceMissing
		}
		next.References = append(next.References, oldReference)
	}
	sort.SliceStable(next.References, func(i, j int) bool {
		if next.References[i].SourceOrder != next.References[j].SourceOrder {
			return next.References[i].SourceOrder < next.References[j].SourceOrder
		}
		return next.References[i].ID < next.References[j].ID
	})
	sortRenames(renames)
	if rootPlan.rootRename != nil {
		renames = append([]Rename{*rootPlan.rootRename}, renames...)
	}
	next.Collection.RunState = runState(next)
	return next, downloads, renames, nil
}

func applyPhotoMetadata(reference *manifest.Reference, photo pixieset.Photo) {
	// SHA256 is historical executor state. The planner only establishes local
	// Placement trust and leaves the Reference checksum for the executor.
	reference.ID = photo.ID
	reference.Name = photo.Name
	reference.Description = photo.Description
	reference.SourceOrder = photo.Rank
	reference.SourceCreated = nil
	reference.SourceUpdated = nil
	reference.CapturedAt = cloneTime(photo.CaptureDate)
	reference.MediaType = "image"
	reference.OriginalFilename = photo.Name
	reference.Width = photo.Width
	reference.Height = photo.Height
	reference.DurationSeconds = nil
	reference.MIMEType = photo.MIMEType
	reference.SelectedQuality = selectedQuality(photo.ImageVariants)
}

func selectedQuality(variants []pixieset.ImageVariant) string {
	if len(variants) == 0 {
		return ""
	}
	switch variants[0].Quality {
	case "xxlarge":
		return "path_xxlarge"
	case "xlarge":
		return "path_xlarge"
	case "large":
		return "path_large"
	case "medium":
		return "path_medium"
	default:
		return ""
	}
}

func portablePlacementPath(set pixieset.Set, photo pixieset.Photo) string {
	return path.Join(paths.SetComponent(set.Name, set.ID), paths.ReferenceComponent(photo.Name, photo.ID, photo.Extension))
}

func setReferenceDownloadState(reference *manifest.Reference, existed bool) {
	preserveFailure := existed && reference.DownloadState == manifest.DownloadFailed && validFailure(reference.Failure)
	present := 0
	pending := false
	failed := false
	for _, placement := range reference.Placements {
		if placement.PresenceState != manifest.PresencePresent {
			continue
		}
		present++
		switch placement.DownloadState {
		case manifest.DownloadFailed:
			failed = true
		case manifest.DownloadComplete:
		default:
			pending = true
		}
	}
	if present == 0 {
		if !existed {
			reference.DownloadState = manifest.DownloadComplete
		}
		return
	}
	if pending {
		reference.DownloadState = manifest.DownloadPending
		reference.Failure = nil
		return
	}
	if failed && preserveFailure {
		reference.DownloadState = manifest.DownloadFailed
		return
	}
	if failed {
		reference.DownloadState = manifest.DownloadPending
		reference.Failure = nil
		return
	}
	reference.DownloadState = manifest.DownloadComplete
	reference.Failure = nil
}

func validFailure(failure *manifest.Failure) bool {
	return failure != nil && validFailureText(failure.Code) && validFailureText(failure.Message)
}

func validFailureText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.Contains(strings.ToLower(value), "://")
}

func runState(m manifest.Manifest) manifest.RunState {
	for _, reference := range m.References {
		for _, placement := range reference.Placements {
			if placement.PresenceState == manifest.PresencePresent && placement.DownloadState != manifest.DownloadComplete {
				return manifest.RunIncomplete
			}
		}
	}
	return manifest.RunComplete
}

func manifestNeedsWork(m manifest.Manifest) bool {
	for _, reference := range m.References {
		if reference.PresenceState == manifest.PresencePresent && reference.DownloadState != manifest.DownloadComplete {
			return true
		}
		for _, placement := range reference.Placements {
			if placement.PresenceState == manifest.PresencePresent && placement.DownloadState != manifest.DownloadComplete {
				return true
			}
		}
	}
	return false
}

func inspectFile(s store.Store, collectionDir, relativePath, checksum string, verify bool) (bool, bool, error) {
	rel := path.Join(collectionDir, relativePath)
	info, exists, err := s.Inspect(rel)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, nil
	}
	if !info.Mode().IsRegular() {
		return false, false, fmt.Errorf("inspect placement %q: expected a regular file", relativePath)
	}
	if !verify {
		return true, true, nil
	}
	if checksum == "" {
		return true, false, nil
	}
	// S3 fast path: trust the stored sha256 metadata instead of re-reading the
	// object. LocalStore.Metadata returns nil, so local behavior is unchanged.
	if metadata, err := s.Metadata(rel); err == nil {
		if stored, ok := metadata["sha256"]; ok && strings.EqualFold(stored, checksum) {
			return true, true, nil
		}
	}
	got, err := fileSHA256(s, rel, relativePath)
	if err != nil {
		return false, false, err
	}
	return true, strings.EqualFold(got, checksum), nil
}

func sameRegularFile(s store.Store, collectionDir, sourcePath, destinationPath string) (bool, error) {
	sourceInfo, sourceExists, err := s.Inspect(path.Join(collectionDir, sourcePath))
	if err != nil {
		return false, err
	}
	destinationInfo, destinationExists, err := s.Inspect(path.Join(collectionDir, destinationPath))
	if err != nil {
		return false, err
	}
	if !sourceExists || !destinationExists {
		return false, nil
	}
	if !sourceInfo.Mode().IsRegular() || !destinationInfo.Mode().IsRegular() {
		return false, nil
	}
	same, err := s.SameFile(path.Join(collectionDir, sourcePath), path.Join(collectionDir, destinationPath))
	if err != nil {
		return false, err
	}
	return same, nil
}

func sameExistingPath(s store.Store, sourcePath, destinationPath string) (bool, bool, error) {
	_, sourceExists, err := s.Inspect(sourcePath)
	if err != nil {
		return false, false, err
	}
	_, destinationExists, err := s.Inspect(destinationPath)
	if err != nil {
		return false, false, err
	}
	if !sourceExists || !destinationExists {
		return false, false, nil
	}
	same, err := s.SameFile(sourcePath, destinationPath)
	if err != nil {
		return false, false, err
	}
	return same, sameCaseOnlyPath(sourcePath, destinationPath), nil
}

func sameCaseOnlyPath(sourcePath, destinationPath string) bool {
	return sourcePath != destinationPath && strings.EqualFold(path.Clean(sourcePath), path.Clean(destinationPath))
}

func fileSHA256(s store.Store, rel, relativePath string) (string, error) {
	file, err := s.Open(rel)
	if err != nil {
		return "", fmt.Errorf("open for checksum placement %q: %v", relativePath, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("read for checksum placement %q: %v", relativePath, err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func sortPlacements(reference *manifest.Reference, ranks map[string]int) {
	sort.SliceStable(reference.Placements, func(i, j int) bool {
		leftRank, leftOK := ranks[reference.Placements[i].SetID]
		rightRank, rightOK := ranks[reference.Placements[j].SetID]
		if leftOK && rightOK && leftRank != rightRank {
			return leftRank < rightRank
		}
		if leftOK != rightOK {
			return leftOK
		}
		return reference.Placements[i].SetID < reference.Placements[j].SetID
	})
}

func setRanks(sets []manifest.Set) map[string]int {
	ranks := make(map[string]int, len(sets))
	for _, set := range sets {
		ranks[set.ID] = set.SourceOrder
	}
	return ranks
}

func sortRenames(renames []Rename) {
	sort.SliceStable(renames, func(i, j int) bool {
		if renames[i].To != renames[j].To {
			return renames[i].To < renames[j].To
		}
		return renames[i].From < renames[j].From
	})
}

func sameIntrinsicPhotoMetadata(left, right pixieset.Photo) bool {
	if left.ID != right.ID || left.CollectionID != right.CollectionID || left.Name != right.Name || left.Description != right.Description || left.MIMEType != right.MIMEType || left.Extension != right.Extension || left.Size != right.Size || left.Width != right.Width || left.Height != right.Height {
		return false
	}
	return sameTime(left.CaptureDate, right.CaptureDate)
}

var variantOrder = map[string]int{
	"xxlarge": 0,
	"xlarge":  1,
	"large":   2,
	"medium":  3,
}

func mergeVariants(existing, incoming []pixieset.ImageVariant) []pixieset.ImageVariant {
	byQuality := make(map[string]pixieset.ImageVariant, len(existing)+len(incoming))
	add := func(variants []pixieset.ImageVariant) {
		for _, variant := range variants {
			if _, known := variantOrder[variant.Quality]; !known || variant.URL == "" {
				continue
			}
			if current, exists := byQuality[variant.Quality]; !exists || current.URL == "" {
				byQuality[variant.Quality] = variant
			}
		}
	}
	// The caller visits Sets in rank/ID order, so keeping the first non-empty
	// URL makes same-quality selection independent of discovery input order.
	add(existing)
	add(incoming)
	merged := make([]pixieset.ImageVariant, 0, len(byQuality))
	for _, quality := range []string{"xxlarge", "xlarge", "large", "medium"} {
		if variant, ok := byQuality[quality]; ok {
			merged = append(merged, variant)
		}
	}
	return merged
}

func clonePhoto(photo pixieset.Photo) pixieset.Photo {
	photo.ImageVariants = cloneVariants(photo.ImageVariants)
	photo.CaptureDate = cloneTime(photo.CaptureDate)
	return photo
}

func cloneVariants(variants []pixieset.ImageVariant) []pixieset.ImageVariant {
	return append([]pixieset.ImageVariant(nil), variants...)
}

func cloneManifest(input manifest.Manifest) manifest.Manifest {
	output := input
	output.Collection = cloneCollection(input.Collection)
	output.Sets = append([]manifest.Set(nil), input.Sets...)
	output.References = make([]manifest.Reference, len(input.References))
	for i, reference := range input.References {
		output.References[i] = reference
		output.References[i].SourceCreated = cloneTime(reference.SourceCreated)
		output.References[i].SourceUpdated = cloneTime(reference.SourceUpdated)
		output.References[i].CapturedAt = cloneTime(reference.CapturedAt)
		output.References[i].Failure = cloneFailure(reference.Failure)
		output.References[i].Placements = append([]manifest.Placement(nil), reference.Placements...)
		for j := range output.References[i].Placements {
			output.References[i].Placements[j].LastAttemptAt = cloneTime(reference.Placements[j].LastAttemptAt)
			output.References[i].Placements[j].LastSuccessAt = cloneTime(reference.Placements[j].LastSuccessAt)
			output.References[i].Placements[j].Failure = cloneFailure(reference.Placements[j].Failure)
		}
	}
	return output
}

func cloneCollection(collection manifest.Collection) manifest.Collection {
	collection.SourceCreated = cloneTime(collection.SourceCreated)
	collection.SourceUpdated = cloneTime(collection.SourceUpdated)
	collection.LastDiscoveryAt = cloneTime(collection.LastDiscoveryAt)
	collection.LastAttemptAt = cloneTime(collection.LastAttemptAt)
	collection.LastSuccessAt = cloneTime(collection.LastSuccessAt)
	collection.LastVerifiedAt = cloneTime(collection.LastVerifiedAt)
	return collection
}

func cloneFailure(failure *manifest.Failure) *manifest.Failure {
	if failure == nil {
		return nil
	}
	copy := *failure
	copy.At = cloneTime(failure.At)
	return &copy
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func timePointer(value time.Time) *time.Time {
	value = normalizeTime(value)
	return &value
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func optionTime(options Options) time.Time {
	if !options.Now.IsZero() {
		return options.Now.UTC()
	}
	return time.Now().UTC()
}

func collectionComponent(name, id string) string {
	return paths.CollectionComponent(name, id)
}

func validateCollectionRoot(s store.Store, collectionDir string) error {
	info, exists, err := s.Inspect(collectionDir)
	if err != nil {
		return err
	}
	if exists && !info.IsDir() {
		return errors.New("inspect Collection directory: Collection root is not a directory")
	}
	return nil
}

func validInputID(subject, value string) error {
	if value == "" || strings.TrimSpace(value) != value || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("%s ID is empty or malformed", subject)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s ID contains a control character", subject)
		}
	}
	return nil
}

func setPending(placement *manifest.Placement) {
	placement.DownloadState = manifest.DownloadPending
	placement.Failure = nil
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}
