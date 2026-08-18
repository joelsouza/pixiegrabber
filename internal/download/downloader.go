// Package download executes the image work produced by archive planning.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"pixiegrabber/internal/archive"
	"pixiegrabber/internal/store"
	"pixiegrabber/internal/throttle"
)

const (
	productionMediaOrigin = "https://images.pixieset.com"
	defaultConcurrency    = 4
	defaultMaxAttempts    = 3
	maxConcurrency        = 64
	maxAttempts           = 8
	// Retry-After and exponential waits never block a worker for more than
	// this small bounded delay.
	maxRetryDelay     = 2 * time.Second
	initialRetryDelay = 100 * time.Millisecond
	maxDrainBytes     = 64 * 1024
	maxSniffBytes     = 512
	copyBufferSize    = 32 * 1024

	// Image validation bounds. maxEncodedBytes is the hard cap on a single
	// encoded media response; minResponseSlack is the smallest per-reference
	// allowance so a tiny source hint cannot starve a valid image.
	maxEncodedBytes   int64 = 1 << 30
	minResponseSlack  int64 = 1 << 20
	maxImageDimension       = 65535
	maxImagePixels          = 100_000_000
	maxDecodedBytes         = maxImagePixels * 4
)

// Failure is safe to persist in a manifest. It does not contain source URLs,
// request data, transport errors, or response bodies.
type Failure struct {
	Code    string
	Message string
}

// PlacementResult reports one independently installed Placement.
type PlacementResult struct {
	SetID        string
	RelativePath string
	Success      bool
	Failure      *Failure
}

// Result reports one Collection-scoped Reference. Results are returned in the
// same order as the input work.
type Result struct {
	ReferenceID string
	Quality     string
	SHA256      string
	Placements  []PlacementResult
	Failure     *Failure
}

// Stable failure codes are intentionally small so Task 6 can map them to
// manifest state without parsing free-form text.
const (
	CodeCanceled          = "canceled"
	CodeOutputRoot        = "output_root_invalid"
	CodeInvalidSource     = "invalid_source"
	CodeSourceUnavailable = "source_unavailable"
	CodeSourceNotFound    = "source_not_found"
	CodeSourceAuth        = "source_authentication"
	CodeSourceHTTPStatus  = "source_http_status"
	CodeRedirect          = "source_redirect"
	CodeMalformedMedia    = "malformed_media"
	CodeRetryExhausted    = "retry_exhausted"
	CodeStaging           = "staging_failed"
	CodePlacement         = "placement_failed"
)

var errRedirect = errors.New("redirect rejected")

type sleeperFunc func(context.Context, time.Duration) error

// Options controls the downloader. Client is required and is shallow-cloned
// by New. A zero Concurrency uses four workers; a zero MaxAttempts uses three
// attempts per variant. Non-zero values have strict upper bounds.
type Options struct {
	Client      *http.Client
	Concurrency int
	MaxAttempts int

	// Limiter spaces out media requests. A nil limiter disables throttling.
	Limiter *throttle.Limiter

	// MediaOrigin overrides the fixed production media origin. It is a test
	// seam; production callers leave it empty.
	MediaOrigin string

	// These fields are private test seams. Production callers always use the
	// fixed Pixieset media origin and the real clock/sleeper.
	mediaOrigin string
	sleeper     sleeperFunc
	clock       func() time.Time
}

// Downloader downloads and installs planned image References.
type Downloader struct {
	client      *http.Client
	concurrency int
	maxAttempts int
	mediaOrigin string
	limiter     *throttle.Limiter
	sleeper     sleeperFunc
	clock       func() time.Time
}

// New validates options and clones the supplied HTTP client. The clone has no
// cookie jar and rejects every redirect, including redirects to the same host.
func New(options Options) (*Downloader, error) {
	if options.Client == nil {
		return nil, errors.New("download client is required")
	}
	concurrency := options.Concurrency
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	if concurrency < 1 || concurrency > maxConcurrency {
		return nil, fmt.Errorf("download concurrency must be between 1 and %d", maxConcurrency)
	}
	attempts := options.MaxAttempts
	if attempts == 0 {
		attempts = defaultMaxAttempts
	}
	if attempts < 1 || attempts > maxAttempts {
		return nil, fmt.Errorf("download max attempts must be between 1 and %d", maxAttempts)
	}

	origin := options.MediaOrigin
	if origin == "" {
		origin = options.mediaOrigin
	}
	if origin == "" {
		origin = productionMediaOrigin
	} else if err := validateTestOrigin(origin); err != nil {
		return nil, err
	}

	client := *options.Client
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return errRedirect }
	sleep := options.sleeper
	if sleep == nil {
		sleep = sleepContext
	}
	clock := options.clock
	if clock == nil {
		clock = time.Now
	}
	return &Downloader{
		client:      &client,
		concurrency: concurrency,
		maxAttempts: attempts,
		mediaOrigin: origin,
		limiter:     options.Limiter,
		sleeper:     sleep,
		clock:       clock,
	}, nil
}

// Download processes bounded concurrent work and returns one result for every
// input Reference, including work that could not start after cancellation. The
// caller must hold the output-root lock.
func (d *Downloader) Download(ctx context.Context, s store.Store, work []archive.DownloadWork) []Result {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]Result, len(work))
	rootOK := s != nil
	for i := range work {
		results[i] = initialResult(work[i])
		if !rootOK {
			results[i].Failure = failure(CodeOutputRoot)
		} else {
			// This default is replaced by a worker for every submitted job. It
			// is also the safe result for jobs left in the queue by cancellation.
			results[i].Failure = failure(CodeCanceled)
		}
	}
	if !rootOK || ctx.Err() != nil || len(work) == 0 {
		return results
	}

	jobs := make(chan int)
	workers := d.concurrency
	if workers > len(work) {
		workers = len(work)
	}
	var group sync.WaitGroup
	group.Add(workers)
	for n := 0; n < workers; n++ {
		go func() {
			defer group.Done()
			for index := range jobs {
				results[index] = d.process(ctx, s, work[index])
			}
		}()
	}
	for i := range work {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return results
		}
	}
	close(jobs)
	group.Wait()
	return results
}

func initialResult(work archive.DownloadWork) Result {
	placements := make([]PlacementResult, len(work.Destinations))
	for i, destination := range work.Destinations {
		placements[i] = PlacementResult{SetID: destination.SetID, RelativePath: destination.RelativePath}
	}
	return Result{ReferenceID: work.ReferenceID, Placements: placements}
}

func (d *Downloader) process(ctx context.Context, s store.Store, work archive.DownloadWork) Result {
	result := initialResult(work)
	if err := ctx.Err(); err != nil {
		result.Failure = failure(CodeCanceled)
		return result
	}
	stage, err := os.CreateTemp("", ".pixiegrabber-download-")
	if err != nil {
		result.Failure = failure(CodeStaging)
		return result
	}
	removeStage := true
	defer func() {
		_ = stage.Close()
		if removeStage {
			_ = os.Remove(stage.Name())
		}
	}()
	if err := stage.Chmod(0600); err != nil {
		result.Failure = failure(CodeStaging)
		return result
	}

	quality, digest, sourceFailure := d.fetchSource(ctx, stage, work)
	if sourceFailure != nil {
		result.Failure = sourceFailure
		return result
	}
	if err := stage.Sync(); err != nil {
		result.Failure = failure(CodeStaging)
		return result
	}
	result.Quality = quality
	result.SHA256 = digest
	result.Failure = nil

	info, err := stage.Stat()
	if err != nil {
		result.Failure = failure(CodeStaging)
		return result
	}
	size := info.Size()

	for i, destination := range work.Destinations {
		if err := ctx.Err(); err != nil {
			result.Placements[i].Failure = failure(CodeCanceled)
			continue
		}
		if err := installPlacement(s, stage, size, digest, destination); err != nil {
			result.Placements[i].Failure = failure(CodePlacement)
			continue
		}
		result.Placements[i].Success = true
	}
	return result
}

type fetchKind uint8

const (
	fetchSuccess fetchKind = iota
	fetchFallback
	fetchFailure
	fetchRetry
	fetchCanceled
	fetchLocal
)

type fetchResult struct {
	kind    fetchKind
	quality string
	digest  string
	failure *Failure
}

func (d *Downloader) fetchSource(ctx context.Context, stage *os.File, work archive.DownloadWork) (string, string, *Failure) {
	limit := mediaLimit(work.SourceBytes)
	sawVariant := false
	for _, variant := range work.Variants {
		if variant.URL == "" {
			continue
		}
		sawVariant = true
		source, err := d.validateMediaURL(variant.URL)
		if err != nil {
			return "", "", failure(CodeInvalidSource)
		}
		attempt := d.fetchVariant(ctx, stage, source, limit)
		switch attempt.kind {
		case fetchSuccess:
			return variant.Quality, attempt.digest, nil
		case fetchFallback:
			continue
		default:
			return "", "", attempt.failure
		}
	}
	if !sawVariant {
		return "", "", failure(CodeSourceUnavailable)
	}
	return "", "", failure(CodeSourceNotFound)
}

func mediaLimit(sourceBytes int64) int64 {
	limit := maxEncodedBytes
	if sourceBytes > 0 {
		limit = min(maxEncodedBytes, max(sourceBytes*2, minResponseSlack))
	}
	return limit
}

func (d *Downloader) fetchVariant(ctx context.Context, stage *os.File, source *url.URL, limit int64) fetchResult {
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
		}
		if d.limiter != nil {
			if err := d.limiter.Wait(ctx); err != nil {
				return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.String(), nil)
		if err != nil {
			return fetchResult{kind: fetchFailure, failure: failure(CodeInvalidSource)}
		}
		response, err := d.client.Do(request)
		if err != nil {
			if response != nil && response.Body != nil {
				closeResponse(response)
			}
			if errors.Is(err, errRedirect) {
				return fetchResult{kind: fetchFailure, failure: failure(CodeRedirect)}
			}
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
			}
			if attempt == d.maxAttempts {
				return fetchResult{kind: fetchFailure, failure: failure(CodeRetryExhausted)}
			}
			if !d.wait(ctx, initialBackoff(attempt)) {
				return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
			}
			continue
		}

		if ctx.Err() != nil {
			drainAndClose(response.Body)
			return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
		}
		status := response.StatusCode
		if status == http.StatusNotFound || status == http.StatusGone {
			drainAndClose(response.Body)
			return fetchResult{kind: fetchFallback}
		}
		if status >= 300 && status <= 399 {
			drainAndClose(response.Body)
			return fetchResult{kind: fetchFailure, failure: failure(CodeRedirect)}
		}
		if status < 200 || status >= 300 {
			if isRetryableStatus(status) {
				delay := retryDelay(response.Header.Get("Retry-After"), d.clock(), attempt)
				drainAndClose(response.Body)
				if attempt == d.maxAttempts {
					return fetchResult{kind: fetchFailure, failure: failure(CodeRetryExhausted)}
				}
				if !d.wait(ctx, delay) {
					return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
				}
				continue
			}
			drainAndClose(response.Body)
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				return fetchResult{kind: fetchFailure, failure: failure(CodeSourceAuth)}
			}
			return fetchResult{kind: fetchFailure, failure: failure(CodeSourceHTTPStatus)}
		}

		if response.ContentLength == 0 {
			drainAndClose(response.Body)
			return fetchResult{kind: fetchFailure, failure: failure(CodeMalformedMedia)}
		}
		if response.Body == nil {
			return fetchResult{kind: fetchFailure, failure: failure(CodeMalformedMedia)}
		}
		contentType, _, typeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if typeErr != nil || !strings.HasPrefix(strings.ToLower(contentType), "image/") {
			drainAndClose(response.Body)
			return fetchResult{kind: fetchFailure, failure: failure(CodeMalformedMedia)}
		}
		kind, digest := writeMediaResponse(ctx, response.Body, stage, limit)
		if kind == fetchSuccess {
			if err := validateImage(stage); err != nil {
				return fetchResult{kind: fetchFailure, failure: failure(CodeMalformedMedia)}
			}
			return fetchResult{kind: fetchSuccess, digest: digest}
		}
		if kind == fetchLocal {
			return fetchResult{kind: fetchFailure, failure: failure(CodeStaging)}
		}
		if kind == fetchCanceled || ctx.Err() != nil {
			return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
		}
		if kind == fetchFailure {
			return fetchResult{kind: fetchFailure, failure: failure(CodeMalformedMedia)}
		}
		if attempt == d.maxAttempts {
			return fetchResult{kind: fetchFailure, failure: failure(CodeRetryExhausted)}
		}
		if !d.wait(ctx, initialBackoff(attempt)) {
			return fetchResult{kind: fetchCanceled, failure: failure(CodeCanceled)}
		}
	}
	return fetchResult{kind: fetchFailure, failure: failure(CodeRetryExhausted)}
}

func writeMediaResponse(ctx context.Context, body io.ReadCloser, stage *os.File, limit int64) (fetchKind, string) {
	defer body.Close()
	prefix := make([]byte, maxSniffBytes)
	n, readErr := io.ReadFull(body, prefix)
	if n == 0 {
		if readErr != nil && readErr != io.EOF {
			return fetchRetry, ""
		}
		return fetchFailure, ""
	}
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return fetchRetry, ""
	}
	detected := http.DetectContentType(prefix[:n])
	detectedType, _, detectErr := mime.ParseMediaType(detected)
	if detectErr != nil || !strings.HasPrefix(strings.ToLower(detectedType), "image/") {
		return fetchFailure, ""
	}
	if err := stage.Truncate(0); err != nil {
		return fetchLocal, ""
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return fetchLocal, ""
	}
	hash := sha256.New()
	if err := writeAndHash(stage, hash, prefix[:n]); err != nil {
		return fetchLocal, ""
	}
	total := int64(n)
	if total > limit {
		return fetchFailure, ""
	}
	if readErr == nil {
		buffer := make([]byte, copyBufferSize)
		for {
			if ctx.Err() != nil {
				return fetchCanceled, ""
			}
			count, err := body.Read(buffer)
			if count > 0 {
				if writeErr := writeAndHash(stage, hash, buffer[:count]); writeErr != nil {
					return fetchLocal, ""
				}
				total += int64(count)
				if total > limit {
					return fetchFailure, ""
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fetchRetry, ""
			}
		}
	}
	return fetchSuccess, hex.EncodeToString(hash.Sum(nil))
}

// validateImage decodes the staged media and enforces image dimension and
// decoded-size bounds. It returns an error for any malformed or oversized
// image so the caller can fail with CodeMalformedMedia.
func validateImage(stage *os.File) error {
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	config, _, err := image.DecodeConfig(stage)
	if err != nil {
		return err
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxImageDimension || config.Height > maxImageDimension {
		return errors.New("image dimensions are out of range")
	}
	pixels := int64(config.Width) * int64(config.Height)
	if pixels > maxImagePixels {
		return errors.New("image has too many pixels")
	}
	if pixels*4 > maxDecodedBytes {
		return errors.New("image decodes to too many bytes")
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decoded, _, err := image.Decode(stage)
	if err != nil {
		return err
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != config.Width || bounds.Dy() != config.Height {
		return errors.New("image config and decoded bounds differ")
	}
	return nil
}

func writeAndHash(destination io.Writer, hash io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := destination.Write(data)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		if _, err := hash.Write(data[:count]); err != nil {
			return err
		}
		data = data[count:]
	}
	return nil
}

func (d *Downloader) validateMediaURL(raw string) (*url.URL, error) {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Path == "" {
		return nil, errors.New("invalid media source")
	}
	if !safeURLPath(parsed.Path) {
		return nil, errors.New("invalid media source")
	}
	if d.mediaOrigin == productionMediaOrigin {
		if !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Host, "images.pixieset.com") {
			return nil, errors.New("invalid media source")
		}
		return parsed, nil
	}
	origin, err := url.Parse(d.mediaOrigin)
	if err != nil || !strings.EqualFold(parsed.Scheme, origin.Scheme) || !strings.EqualFold(parsed.Hostname(), origin.Hostname()) || parsed.Port() != origin.Port() {
		return nil, errors.New("invalid media source")
	}
	return parsed, nil
}

func safeURLPath(value string) bool {
	if value == "" || value == "/" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "." || component == ".." {
			return false
		}
		if !utf8.ValidString(component) {
			return false
		}
	}
	return true
}

func validateTestOrigin(value string) error {
	origin, err := url.Parse(value)
	if err != nil || origin.User != nil || origin.Host == "" || origin.Path != "" && origin.Path != "/" || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("invalid media test origin")
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return errors.New("invalid media test origin")
	}
	host := strings.ToLower(origin.Hostname())
	if host != "localhost" && net.ParseIP(host) == nil {
		return errors.New("invalid media test origin")
	}
	return nil
}

func installPlacement(s store.Store, stage *os.File, size int64, digest string, destination archive.Destination) error {
	if err := validateDestination(destination); err != nil {
		return err
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return s.Put(destination.RelativePath, stage, size, map[string]string{"sha256": digest})
}

func validateDestination(destination archive.Destination) error {
	if destination.SetID == "" || !portableRelativePath(destination.RelativePath) {
		return errors.New("invalid placement destination")
	}
	components := strings.Split(destination.RelativePath, "/")
	if len(components) != 3 {
		return errors.New("invalid placement destination")
	}
	setSuffix := "--" + destination.SetID
	if len(components[1]) <= len(setSuffix) || !strings.HasSuffix(components[1], setSuffix) {
		return errors.New("invalid placement destination")
	}
	return nil
}

func portableRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	components := strings.Split(value, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return false
		}
		if !utf8.ValidString(component) || strings.ContainsAny(component, `<>:"|?*`) {
			return false
		}
		for _, char := range component {
			if char < 0x20 || char == 0x7f || !utf8.ValidRune(char) {
				return false
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return false
		}
	}
	return true
}

func failure(code string) *Failure {
	messages := map[string]string{
		CodeCanceled:          "download was canceled",
		CodeOutputRoot:        "output root is not a real directory",
		CodeInvalidSource:     "media source is not allowed",
		CodeSourceUnavailable: "no usable media source was provided",
		CodeSourceNotFound:    "no media variant was found",
		CodeSourceAuth:        "media source requires authentication",
		CodeSourceHTTPStatus:  "media source returned an unsupported response",
		CodeRedirect:          "media source redirect was rejected",
		CodeMalformedMedia:    "media response is not a valid image",
		CodeRetryExhausted:    "temporary media failures exceeded the attempt limit",
		CodeStaging:           "local staging write failed",
		CodePlacement:         "local Placement write failed",
	}
	return &Failure{Code: code, Message: messages[code]}
}

func (d *Downloader) wait(ctx context.Context, delay time.Duration) bool {
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return d.sleeper(ctx, delay) == nil && ctx.Err() == nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func initialBackoff(attempt int) time.Duration {
	delay := initialRetryDelay
	for n := 1; n < attempt && delay < maxRetryDelay; n++ {
		delay *= 2
	}
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func retryDelay(value string, now time.Time, attempt int) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			delay := time.Duration(seconds) * time.Second
			if delay < 0 || delay > maxRetryDelay {
				return maxRetryDelay
			}
			return delay
		}
		if retryAt, err := http.ParseTime(value); err == nil {
			delay := retryAt.Sub(now)
			if delay <= 0 {
				return 0
			}
			if delay > maxRetryDelay {
				return maxRetryDelay
			}
			return delay
		}
	}
	return initialBackoff(attempt)
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, 425, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, body, maxDrainBytes)
	_ = body.Close()
}

func closeResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
