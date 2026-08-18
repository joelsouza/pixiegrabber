// Package app implements the runnable Pixiegrabber CLI flow.
package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"pixiegrabber/internal/archive"
	"pixiegrabber/internal/browsercookies"
	"pixiegrabber/internal/download"
	"pixiegrabber/internal/manifest"
	"pixiegrabber/internal/paths"
	"pixiegrabber/internal/pixieset"
	"pixiegrabber/internal/store"
	"pixiegrabber/internal/throttle"
)

const (
	defaultConcurrency = 4
	maxConcurrency     = 64
	productionAPIBase  = "https://galleries.pixieset.com"
)

// Options configures one run of the CLI flow.
type Options struct {
	CookiesFromBrowser string
	Output             string
	SyncExisting       bool
	Verify             bool
	Yes                bool
	Concurrency        int
	UserAgent          string
	Interval           time.Duration

	// S3 mode. When S3 is enabled, Output is not required and the store is
	// backed by an S3-compatible bucket instead of a local directory.
	S3          bool
	S3Endpoint  string
	S3Bucket    string
	S3Region    string
	S3PathStyle bool
	S3Secure    bool

	// Store overrides the backend. When nil, Run selects local or S3 based on
	// the S3 flag. It is a test seam.
	Store store.Store

	// apiBaseURL and mediaOrigin are unexported test seams. apiBaseURL
	// defaults to the production API origin; mediaOrigin defaults to the
	// downloader's production media origin.
	apiBaseURL  string
	mediaOrigin string
	// session is an unexported test seam that bypasses browser cookie import.
	session browsercookies.Session
}

type collectionPlan struct {
	collection pixieset.Collection
	plan       archive.Plan
}

// Run executes the CLI flow. stdout receives the plan and progress; stdin
// provides the confirmation answer. It returns an error for any failure.
func Run(ctx context.Context, options Options, stdout io.Writer, stdin io.Reader) error {
	if options.S3 {
		if options.S3Endpoint == "" {
			return errors.New("s3-endpoint is required in s3 mode")
		}
		if options.S3Bucket == "" {
			return errors.New("s3-bucket is required in s3 mode")
		}
		if os.Getenv("PIXIEGRABBER_S3_ACCESS_KEY") == "" || os.Getenv("PIXIEGRABBER_S3_SECRET_KEY") == "" {
			return errors.New("PIXIEGRABBER_S3_ACCESS_KEY and PIXIEGRABBER_S3_SECRET_KEY are required in s3 mode")
		}
	} else if options.Output == "" {
		return errors.New("output directory is required")
	}
	if options.CookiesFromBrowser == "" {
		return errors.New("cookies-from-browser is required")
	}
	concurrency := options.Concurrency
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	if concurrency < 1 || concurrency > maxConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d", maxConcurrency)
	}

	session := options.session
	if session.Jar == nil {
		var err error
		session, err = browsercookies.Load(ctx, options.CookiesFromBrowser)
		if err != nil {
			return err
		}
	}
	userAgent := options.UserAgent
	if userAgent == "" {
		userAgent = session.UserAgent
	}
	if userAgent == "" {
		return errors.New("a User-Agent is required; set --user-agent or import a browser session")
	}

	s, err := selectStore(options)
	if err != nil {
		return err
	}
	defer s.Close()
	release, err := s.Lock()
	if err != nil {
		return err
	}
	defer release()

	apiBase := options.apiBaseURL
	if apiBase == "" {
		apiBase = productionAPIBase
	}
	lim := throttle.New(options.Interval)
	client, err := pixieset.NewClient(apiBase, &http.Client{Jar: session.Jar}, pixieset.WithUserAgent(userAgent), pixieset.WithThrottle(lim))
	if err != nil {
		return err
	}

	collections, err := client.ListCollections(ctx)
	if err != nil {
		return err
	}

	discovered := make(map[string]struct{}, len(collections))
	var plans []collectionPlan
	for _, collection := range collections {
		discovered[collection.ID] = struct{}{}
		sets, err := client.ListSets(ctx, collection.ID)
		if err != nil {
			return err
		}
		fullSets := make([]pixieset.Set, 0, len(sets))
		for _, set := range sets {
			full, err := client.GetSet(ctx, collection.ID, set.ID)
			if err != nil {
				return err
			}
			fullSets = append(fullSets, full)
		}
		if err := archive.CheckVideos(s, fullSets); err != nil {
			if errors.Is(err, archive.ErrUnsupportedVideo) {
				var videoErr *archive.UnsupportedVideoError
				if errors.As(err, &videoErr) {
					fmt.Fprintf(stdout, "unsupported video detected; diagnostic: %s\n", videoErr.Path())
				}
			}
			return err
		}
		previous, err := loadPreviousManifest(s, collection)
		if err != nil {
			return err
		}
		plan, err := archive.Build(s, collection, fullSets, previous, archive.Options{
			SyncExisting: options.SyncExisting,
			Verify:       options.Verify,
			Now:          time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		plans = append(plans, collectionPlan{collection: collection, plan: plan})
	}

	// Absent Collections (only when SyncExisting) are marked source-missing
	// without deleting any local files.
	var absent []archive.Plan
	if options.SyncExisting {
		entries, err := s.ReadDir(".")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			id := collectionIDFromDir(entry.Name())
			if id == "" {
				continue
			}
			if _, ok := discovered[id]; ok {
				continue
			}
			previous, err := manifest.Load(s, path.Join(entry.Name(), manifest.ManifestFilename))
			if err != nil {
				if errors.Is(err, manifest.ErrNotFound) {
					continue
				}
				return err
			}
			plan, err := archive.MarkSourceMissing(s, *previous, time.Now().UTC())
			if err != nil {
				return err
			}
			absent = append(absent, plan)
		}
	}

	displayPlan(stdout, plans)

	if !options.Yes {
		fmt.Fprint(stdout, "Proceed? [y/N] ")
		answer, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && answer == "" {
			return nil
		}
		answer = strings.TrimSpace(answer)
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			return nil
		}
	}

	now := time.Now().UTC()
	for i := range plans {
		if err := executePlan(ctx, s, concurrency, options.mediaOrigin, lim, &plans[i].plan, now); err != nil {
			return err
		}
	}
	for _, plan := range absent {
		if err := manifest.Write(s, path.Join(plan.CollectionDir, manifest.ManifestFilename), plan.Manifest); err != nil {
			return err
		}
	}

	for _, cp := range plans {
		for _, ref := range cp.plan.Manifest.References {
			if ref.PresenceState == manifest.PresencePresent && ref.DownloadState == manifest.DownloadFailed {
				return errors.New("one or more References failed to download; run again to resume")
			}
		}
	}
	return nil
}

// selectStore returns the backend for a run. An explicit Store seam wins;
// otherwise S3 mode builds an S3 backend and local mode opens the output root.
func selectStore(options Options) (store.Store, error) {
	if options.Store != nil {
		return options.Store, nil
	}
	if options.S3 {
		return store.NewS3(store.Config{
			Endpoint:  options.S3Endpoint,
			Bucket:    options.S3Bucket,
			Region:    options.S3Region,
			AccessKey: os.Getenv("PIXIEGRABBER_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("PIXIEGRABBER_S3_SECRET_KEY"),
			PathStyle: options.S3PathStyle,
			Secure:    options.S3Secure,
		})
	}
	return store.NewLocal(options.Output)
}

func displayPlan(stdout io.Writer, plans []collectionPlan) {
	collectionCount := len(plans)
	referenceCount := 0
	placementCount := 0
	var sourceBytes int64
	for _, cp := range plans {
		for _, ref := range cp.plan.Manifest.References {
			referenceCount++
			for _, placement := range ref.Placements {
				if placement.PresenceState == manifest.PresencePresent {
					placementCount++
				}
			}
		}
		for _, work := range cp.plan.Downloads {
			sourceBytes += work.SourceBytes
		}
	}
	fmt.Fprintf(stdout, "Collections: %d\n", collectionCount)
	fmt.Fprintf(stdout, "Image references: %d\n", referenceCount)
	fmt.Fprintf(stdout, "Placement files: %d\n", placementCount)
	fmt.Fprintf(stdout, "Source bytes: %d\n", sourceBytes)
}

func loadPreviousManifest(s store.Store, collection pixieset.Collection) (*manifest.Manifest, error) {
	currentDir := paths.CollectionComponent(collection.Name, collection.ID)
	previous, err := manifest.Load(s, path.Join(currentDir, manifest.ManifestFilename))
	if err == nil {
		return previous, nil
	}
	if !errors.Is(err, manifest.ErrNotFound) {
		return nil, err
	}
	entries, err := s.ReadDir(".")
	if err != nil {
		return nil, err
	}
	suffix := "--" + collection.ID
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		previous, err := manifest.Load(s, path.Join(entry.Name(), manifest.ManifestFilename))
		if err == nil {
			return previous, nil
		}
		if !errors.Is(err, manifest.ErrNotFound) {
			return nil, err
		}
	}
	return nil, nil
}

func collectionIDFromDir(name string) string {
	idx := strings.LastIndex(name, "--")
	if idx < 0 || idx+2 >= len(name) {
		return ""
	}
	return name[idx+2:]
}

func executePlan(ctx context.Context, s store.Store, concurrency int, mediaOrigin string, limiter *throttle.Limiter, plan *archive.Plan, now time.Time) error {
	for _, rename := range plan.Renames {
		if err := applyRename(s, rename); err != nil {
			return err
		}
	}
	if len(plan.Downloads) > 0 {
		downloader, err := download.New(download.Options{
			Client:      &http.Client{},
			Concurrency: concurrency,
			MediaOrigin: mediaOrigin,
			Limiter:     limiter,
		})
		if err != nil {
			return err
		}
		results := downloader.Download(ctx, s, plan.Downloads)
		applyResults(&plan.Manifest, results, now)
	}
	if err := manifest.Write(s, path.Join(plan.CollectionDir, manifest.ManifestFilename), plan.Manifest); err != nil {
		return err
	}
	return nil
}

func applyRename(s store.Store, rename archive.Rename) error {
	source, err := s.Open(rename.From)
	if err != nil {
		return err
	}
	defer source.Close()
	info, exists, err := s.Inspect(rename.From)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("rename source is missing")
	}
	if err := s.Put(rename.To, source, info.Size(), nil); err != nil {
		return err
	}
	return s.Remove(rename.From)
}

func applyResults(m *manifest.Manifest, results []download.Result, now time.Time) {
	byID := make(map[string]*manifest.Reference, len(m.References))
	for i := range m.References {
		byID[m.References[i].ID] = &m.References[i]
	}
	for _, result := range results {
		ref, ok := byID[result.ReferenceID]
		if !ok {
			continue
		}
		if result.Failure != nil {
			ref.DownloadState = manifest.DownloadFailed
			ref.Failure = &manifest.Failure{Code: result.Failure.Code, Message: result.Failure.Message, At: &now}
			continue
		}
		ref.SHA256 = result.SHA256
		ref.SelectedQuality = result.Quality
		ref.Failure = nil
		bySet := make(map[string]*manifest.Placement, len(ref.Placements))
		for i := range ref.Placements {
			bySet[ref.Placements[i].SetID] = &ref.Placements[i]
		}
		for _, placementResult := range result.Placements {
			placement, ok := bySet[placementResult.SetID]
			if !ok {
				continue
			}
			if placementResult.Success {
				placement.DownloadState = manifest.DownloadComplete
				placement.InstalledSHA256 = result.SHA256
				placement.LastSuccessAt = &now
				placement.Failure = nil
			} else {
				placement.DownloadState = manifest.DownloadFailed
				placement.Failure = &manifest.Failure{Code: placementResult.Failure.Code, Message: placementResult.Failure.Message, At: &now}
				placement.LastAttemptAt = &now
			}
		}
		recomputeReferenceState(ref)
	}
}

func recomputeReferenceState(ref *manifest.Reference) {
	present := 0
	failed := false
	pending := false
	var firstFailure *manifest.Failure
	for _, placement := range ref.Placements {
		if placement.PresenceState != manifest.PresencePresent {
			continue
		}
		present++
		switch placement.DownloadState {
		case manifest.DownloadFailed:
			failed = true
			if firstFailure == nil && placement.Failure != nil {
				copy := *placement.Failure
				firstFailure = &copy
			}
		case manifest.DownloadComplete:
		default:
			pending = true
		}
	}
	if present == 0 {
		ref.DownloadState = manifest.DownloadComplete
		ref.Failure = nil
		return
	}
	if pending {
		ref.DownloadState = manifest.DownloadPending
		ref.Failure = nil
		return
	}
	if failed {
		ref.DownloadState = manifest.DownloadFailed
		if ref.Failure == nil {
			ref.Failure = firstFailure
		}
		return
	}
	ref.DownloadState = manifest.DownloadComplete
	ref.Failure = nil
}
