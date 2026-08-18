package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pixiegrabber/internal/archive"
	"pixiegrabber/internal/outputfs"
	"pixiegrabber/internal/pixieset"
	"pixiegrabber/internal/throttle"
)

var syntheticJPEG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

func loopbackDownloader(t *testing.T, handler http.Handler, options Options) (*Downloader, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	if options.Client == nil {
		options.Client = server.Client()
	}
	options.mediaOrigin = server.URL
	if options.Concurrency == 0 {
		options.Concurrency = 2
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 3
	}
	downloader, err := New(options)
	if err != nil {
		server.Close()
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(server.Close)
	return downloader, server
}

func productionDownloader(t *testing.T, transport http.RoundTripper) *Downloader {
	t.Helper()
	downloader, err := New(Options{
		Client:      &http.Client{Transport: transport},
		Concurrency: 2,
		MaxAttempts: 3,
		sleeper:     noWait,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return downloader
}

func noWait(context.Context, time.Duration) error { return nil }

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

func displayPath(t *testing.T, fs *outputfs.FS, rel string) string {
	t.Helper()
	path, err := fs.DisplayPath(rel)
	if err != nil {
		t.Fatalf("DisplayPath(%q) error = %v", rel, err)
	}
	return path
}

func oneWork(id string, variants []pixieset.ImageVariant, relative string) archive.DownloadWork {
	return archive.DownloadWork{
		ReferenceID: id,
		Variants:    variants,
		Destinations: []archive.Destination{{
			SetID:        setIDFromPath(relative),
			RelativePath: relative,
		}},
	}
}

func setIDFromPath(relative string) string {
	parts := strings.Split(relative, "/")
	idx := strings.LastIndex(parts[1], "--")
	return parts[1][idx+2:]
}

func variant(quality, source string) pixieset.ImageVariant {
	return pixieset.ImageVariant{Quality: quality, URL: source}
}

func assertSuccess(t *testing.T, result Result, quality string) {
	t.Helper()
	if result.Failure != nil {
		t.Fatalf("result failure = %#v", result.Failure)
	}
	if result.Quality != quality || result.SHA256 == "" {
		t.Fatalf("result source = quality %q hash %q", result.Quality, result.SHA256)
	}
	for _, placement := range result.Placements {
		if !placement.Success || placement.Failure != nil {
			t.Fatalf("placement = %#v", placement)
		}
	}
}

func assertFailureCode(t *testing.T, result Result, code string) {
	t.Helper()
	if result.Failure == nil || result.Failure.Code != code {
		t.Fatalf("failure = %#v, want code %q", result.Failure, code)
	}
	if result.Failure.Message == "" {
		t.Fatal("failure message is empty")
	}
}

func TestDownloadThrottleSpacesFetches(t *testing.T) {
	fs := openTestFS(t)
	const interval = 30 * time.Millisecond
	var fetches atomic.Int32
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{
		Concurrency: 1,
		Limiter:     throttle.New(interval),
	})
	work := make([]archive.DownloadWork, 4)
	for i := range work {
		id := fmt.Sprintf("ref-%d", i)
		work[i] = oneWork(id, []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/" + id}}, "Collection--100/Set--11/"+id+".jpg")
	}
	start := time.Now()
	results := d.Download(context.Background(), fs, work)
	elapsed := time.Since(start)
	for _, result := range results {
		assertSuccess(t, result, "large")
	}
	if fetches.Load() != int32(len(work)) {
		t.Fatalf("fetches = %d, want %d", fetches.Load(), len(work))
	}
	minExpected := time.Duration(len(work)-1) * interval
	if elapsed < minExpected {
		t.Fatalf("elapsed = %v, want at least %v", elapsed, minExpected)
	}
}

func TestDownloadLargestVariantSuccessAndResultOrder(t *testing.T) {
	fs := openTestFS(t)
	var requested []string
	var mu sync.Mutex
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{})

	work := []archive.DownloadWork{
		oneWork("first", []pixieset.ImageVariant{variant("xxlarge", "http://unused.invalid/first")}, "Collection--100/Set--11/first.jpg"),
		oneWork("second", []pixieset.ImageVariant{variant("xxlarge", "http://unused.invalid/second")}, "Collection--100/Set--11/second.jpg"),
	}
	for i := range work {
		work[i].Variants[0].URL = d.mediaOrigin + "/" + work[i].ReferenceID
	}
	results := d.Download(context.Background(), fs, work)
	if len(results) != 2 || results[0].ReferenceID != "first" || results[1].ReferenceID != "second" {
		t.Fatalf("result order = %#v", results)
	}
	assertSuccess(t, results[0], "xxlarge")
	assertSuccess(t, results[1], "xxlarge")
	if got := string(mustReadFile(t, displayPath(t, fs, "Collection--100/Set--11/first.jpg"))); got != string(syntheticJPEG) {
		t.Fatalf("first file = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, path := range requested {
		seen[path] = true
	}
	if len(requested) != 2 || !seen["/first"] || !seen["/second"] {
		t.Fatalf("requested paths = %#v", requested)
	}
}

func TestDownloadEmpty404And410FallBackInPlannerOrder(t *testing.T) {
	fs := openTestFS(t)
	var paths []string
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/missing", "/gone":
			w.WriteHeader(map[string]int{"/missing": http.StatusNotFound, "/gone": http.StatusGone}[r.URL.Path])
		default:
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(syntheticJPEG)
		}
	}), Options{})

	work := oneWork("ref", []pixieset.ImageVariant{
		variant("empty", ""),
		variant("xxlarge", d.mediaOrigin+"/missing"),
		variant("xlarge", d.mediaOrigin+"/gone"),
		variant("large", d.mediaOrigin+"/chosen"),
	}, "Collection--100/Set--11/ref.jpg")
	result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
	assertSuccess(t, result, "large")
	if !reflect.DeepEqual(paths, []string{"/missing", "/gone", "/chosen"}) {
		t.Fatalf("fallback paths = %#v", paths)
	}
}

func TestDownloadPreservesQueryForRequestButNeverResult(t *testing.T) {
	fs := openTestFS(t)
	const secret = "signed-token-for-test"
	var gotQuery string
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if gotQuery != "sig="+secret {
			t.Errorf("query = %q", gotQuery)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{})
	result := d.Download(context.Background(), fs, []archive.DownloadWork{
		oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/media?sig=" + secret}}, "Collection--100/Set--11/ref.jpg"),
	})[0]
	assertSuccess(t, result, "large")
	if strings.Contains(fmt.Sprintf("%#v", result), secret) || strings.Contains(fmt.Sprintf("%#v", result), "media?") {
		t.Fatalf("result leaked source data: %#v", result)
	}
}

func TestDownloadNormalizesProtocolRelativeProductionURL(t *testing.T) {
	fs := openTestFS(t)
	var requested *url.URL
	d := productionDownloader(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := *r.URL
		requested = &copy
		return imageResponse(http.StatusOK, syntheticJPEG), nil
	}))
	result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: "//images.pixieset.com/media?sig=synthetic"}}, "Collection--100/Set--11/ref.jpg")})[0]
	assertSuccess(t, result, "large")
	if requested == nil || requested.Scheme != "https" || requested.Host != "images.pixieset.com" || requested.RawQuery != "sig=synthetic" {
		t.Fatalf("normalized request URL = %v", requested)
	}
}

func TestDownloadDoesNotFallBackForProtectedRedirectMalformedOtherStatusOrExhausted(t *testing.T) {
	tests := []struct {
		name        string
		firstStatus int
		body        []byte
		contentType string
		wantCode    string
		redirect    bool
	}{
		{name: "unauthorized", firstStatus: http.StatusUnauthorized, wantCode: CodeSourceAuth},
		{name: "forbidden", firstStatus: http.StatusForbidden, wantCode: CodeSourceAuth},
		{name: "other status", firstStatus: http.StatusBadRequest, wantCode: CodeSourceHTTPStatus},
		{name: "malformed", firstStatus: http.StatusOK, body: []byte("not an image"), contentType: "image/jpeg", wantCode: CodeMalformedMedia},
		{name: "empty", firstStatus: http.StatusOK, body: nil, contentType: "image/jpeg", wantCode: CodeMalformedMedia},
		{name: "exhausted", firstStatus: http.StatusServiceUnavailable, wantCode: CodeRetryExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fs := openTestFS(t)
			var fallbackCalls atomic.Int32
			d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fallback" {
					fallbackCalls.Add(1)
					w.Header().Set("Content-Type", "image/jpeg")
					_, _ = w.Write(syntheticJPEG)
					return
				}
				if test.redirect {
					http.Redirect(w, r, "/fallback", http.StatusFound)
					return
				}
				if test.firstStatus != http.StatusOK {
					w.WriteHeader(test.firstStatus)
					return
				}
				w.Header().Set("Content-Type", test.contentType)
				_, _ = w.Write(test.body)
			}), Options{MaxAttempts: 2})
			if test.name == "redirect" {
				return
			}
			result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{
				variant("large", d.mediaOrigin+"/first"), variant("small", d.mediaOrigin+"/fallback"),
			}, "Collection--100/Set--11/ref.jpg")})[0]
			assertFailureCode(t, result, test.wantCode)
			if fallbackCalls.Load() != 0 {
				t.Fatalf("fallback called %d times", fallbackCalls.Load())
			}
		})
	}

	t.Run("redirect", func(t *testing.T) {
		fs := openTestFS(t)
		var fallbackCalls atomic.Int32
		d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/fallback" {
				fallbackCalls.Add(1)
			}
			http.Redirect(w, r, "/fallback", http.StatusFound)
		}), Options{})
		result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{
			variant("large", d.mediaOrigin+"/first"), variant("small", d.mediaOrigin+"/fallback"),
		}, "Collection--100/Set--11/ref.jpg")})[0]
		assertFailureCode(t, result, CodeRedirect)
		if fallbackCalls.Load() != 0 {
			t.Fatalf("redirect fallback called %d times", fallbackCalls.Load())
		}
	})
}

func TestDownloadRejectsInvalidOriginsWithoutFallback(t *testing.T) {
	fs := openTestFS(t)
	var calls atomic.Int32
	d := productionDownloader(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return imageResponse(http.StatusOK, syntheticJPEG), nil
	}))
	urls := []string{
		"http://images.pixieset.com/path",
		"https://images.pixieset.com:443/path",
		"https://images.pixieset.com:/path",
		"https://images.pixieset.com.evil.test/path",
		"https://images.pixieset.com/path#fragment",
		"https://user:pass@images.pixieset.com/path",
		"https://images.pixieset.com",
	}
	for i, source := range urls {
		result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork(fmt.Sprintf("ref-%d", i), []pixieset.ImageVariant{
			variant("large", source), variant("small", "https://images.pixieset.com/fallback"),
		}, fmt.Sprintf("Collection--100/Set--11/ref-%d.jpg", i))})[0]
		assertFailureCode(t, result, CodeInvalidSource)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid origins reached transport %d times", calls.Load())
	}
}

func TestDownloadRetriesTransientStatusesWithBoundedRetryAfter(t *testing.T) {
	fs := openTestFS(t)
	var calls atomic.Int32
	var delays []time.Duration
	var mu sync.Mutex
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{
		MaxAttempts: 3,
		sleeper: func(ctx context.Context, delay time.Duration) error {
			mu.Lock()
			delays = append(delays, delay)
			mu.Unlock()
			return nil
		},
	})
	result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/retry"}}, "Collection--100/Set--11/ref.jpg")})[0]
	assertSuccess(t, result, "large")
	if calls.Load() != 3 {
		t.Fatalf("attempts = %d", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) != 2 || delays[0] > maxRetryDelay || delays[1] > maxRetryDelay {
		t.Fatalf("retry delays = %#v", delays)
	}
}

func TestDownloadHonorsRetryAfterHTTPDateAndContextCancellation(t *testing.T) {
	fs := openTestFS(t)
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	var delays []time.Duration
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", now.Add(1500*time.Millisecond).Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}), Options{
		MaxAttempts: 2,
		clock:       func() time.Time { return now },
		sleeper: func(ctx context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	})
	result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("date", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/date"}}, "Collection--100/Set--11/date.jpg")})[0]
	assertFailureCode(t, result, CodeRetryExhausted)
	if len(delays) != 1 || delays[0] <= 0 || delays[0] > maxRetryDelay {
		t.Fatalf("date retry delay = %#v", delays)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := d.Download(ctx, fs, []archive.DownloadWork{
		oneWork("a", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/a"}}, "Collection--100/Set--11/a.jpg"),
		oneWork("b", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/b"}}, "Collection--100/Set--11/b.jpg"),
	})
	if len(canceled) != 2 {
		t.Fatalf("canceled results = %#v", canceled)
	}
	for _, item := range canceled {
		assertFailureCode(t, item, CodeCanceled)
	}
}

func TestNewClonesClientRejectsRedirectsAndStripsCookies(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	originalRedirects := func(*http.Request, []*http.Request) error { return errors.New("original") }
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return imageResponse(http.StatusOK, syntheticJPEG), nil
	})
	client := &http.Client{Transport: transport, Jar: jar, CheckRedirect: originalRedirects, Timeout: time.Second}
	d, err := New(Options{Client: client, Concurrency: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if d.client == client || d.client.Transport == nil || reflect.ValueOf(d.client.Transport).Pointer() != reflect.ValueOf(client.Transport).Pointer() || d.client.Jar != nil {
		t.Fatalf("client clone = %#v original jar = %#v", d.client, client.Jar)
	}
	if client.Jar != jar || client.CheckRedirect == nil {
		t.Fatal("New mutated supplied client")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); err == nil || err.Error() != "original" {
		t.Fatalf("supplied redirect policy changed: %v", err)
	}
	if d.client.CheckRedirect == nil {
		t.Fatal("redirect rejection was not installed")
	}
	if err := d.client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, errRedirect) {
		t.Fatalf("CheckRedirect error = %v", err)
	}
}

func TestDownloadSendsOnlyBareGETWithoutSensitiveHeaders(t *testing.T) {
	fs := openTestFS(t)
	var request *http.Request
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r.Clone(r.Context())
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{})
	result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/ref"}}, "Collection--100/Set--11/ref.jpg")})[0]
	assertSuccess(t, result, "large")
	if request.Method != http.MethodGet || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("X-CSRF-Token") != "" || request.Header.Get("Origin") != "" || request.Header.Get("Referer") != "" {
		t.Fatalf("request headers = %#v", request.Header)
	}
}

func TestDownloadValidatesContentTypeMagicHashAndStreamsBody(t *testing.T) {
	fs := openTestFS(t)
	wantHash := sha256.Sum256(syntheticJPEG)
	var maxRead int
	d := productionDownloader(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"image/jpeg; charset=binary"}},
			Body:          &chunkBody{data: syntheticJPEG, maxRead: &maxRead},
			Request:       r,
			ContentLength: -1,
		}, nil
	}))
	result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: "https://images.pixieset.com/ref?sig=synthetic"}}, "Collection--100/Set--11/ref.jpg")})[0]
	assertSuccess(t, result, "large")
	if result.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("hash = %q", result.SHA256)
	}
	if maxRead > 32*1024 {
		t.Fatalf("body was buffered in a large read: %d", maxRead)
	}
}

func TestDownloadBoundsInFlightReferencesAndPreservesOrder(t *testing.T) {
	fs := openTestFS(t)
	var inFlight, maxInFlight atomic.Int32
	release := make(chan struct{})
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if current <= old || maxInFlight.CompareAndSwap(old, current) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{Concurrency: 2})
	work := make([]archive.DownloadWork, 5)
	for i := range work {
		id := fmt.Sprintf("ref-%d", i)
		work[i] = oneWork(id, []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/" + id}}, "Collection--100/Set--11/"+id+".jpg")
	}
	done := make(chan []Result, 1)
	go func() { done <- d.Download(context.Background(), fs, work) }()
	deadline := time.After(2 * time.Second)
	for maxInFlight.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("workers did not reach concurrency bound")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if maxInFlight.Load() > 2 {
		t.Fatalf("max in-flight = %d", maxInFlight.Load())
	}
	close(release)
	results := <-done
	for i, result := range results {
		if result.ReferenceID != work[i].ReferenceID {
			t.Fatalf("result %d = %#v", i, result)
		}
		assertSuccess(t, result, "large")
	}
}

func TestDownloadInstallsMultiplePlacementsAndContinuesAfterPlacementFailure(t *testing.T) {
	fs := openTestFS(t)
	d, _ := loopbackDownloader(t, imageHandler(), Options{})
	goodRelative := "Collection--100/Set--a/ref.jpg"
	badParent := displayPath(t, fs, "Collection--100/Set--bad")
	if err := os.MkdirAll(filepath.Dir(badParent), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badParent, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	work := archive.DownloadWork{
		ReferenceID: "ref",
		Variants:    []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/ref"}},
		Destinations: []archive.Destination{
			{SetID: "a", RelativePath: goodRelative},
			{SetID: "bad", RelativePath: "Collection--100/Set--bad/ref.jpg"},
		},
	}
	results := d.Download(context.Background(), fs, []archive.DownloadWork{work, oneWork("other", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/other"}}, "Collection--100/Set--11/other.jpg")})
	if results[0].Failure != nil || len(results[0].Placements) != 2 || !results[0].Placements[0].Success {
		t.Fatalf("partial placement result = %#v", results[0])
	}
	if results[0].Placements[1].Failure == nil {
		t.Fatal("bad placement has no failure")
	}
	assertSuccess(t, results[1], "large")
	if got := mustReadFile(t, displayPath(t, fs, goodRelative)); !reflect.DeepEqual(got, syntheticJPEG) {
		t.Fatal("successful placement was not retained")
	}
}

func TestDownloadLocalFailureDoesNotFallBackAndKeepsOtherReferences(t *testing.T) {
	fs := openTestFS(t)
	var requests atomic.Int32
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}), Options{})
	badParent := displayPath(t, fs, "Collection--100/Set--bad")
	if err := os.MkdirAll(filepath.Dir(badParent), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badParent, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := archive.DownloadWork{
		ReferenceID: "bad",
		Variants: []pixieset.ImageVariant{
			{Quality: "large", URL: d.mediaOrigin + "/bad-large"},
			{Quality: "small", URL: d.mediaOrigin + "/bad-small"},
		},
		Destinations: []archive.Destination{{SetID: "bad", RelativePath: "Collection--100/Set--bad/ref.jpg"}},
	}
	results := d.Download(context.Background(), fs, []archive.DownloadWork{bad, oneWork("good", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/good"}}, "Collection--100/Set--11/good.jpg")})
	if results[0].Failure != nil || results[0].Placements[0].Failure == nil {
		t.Fatalf("local failure result = %#v", results[0])
	}
	assertSuccess(t, results[1], "large")
	if requests.Load() != 2 {
		t.Fatalf("request count = %d, want one request per Reference", requests.Load())
	}
}

func TestDownloadRejectsRootRelativeAbsoluteTraversalSymlinkAndWrongTypes(t *testing.T) {
	// Root validation now lives in outputfs.Open.
	rootLink := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(t.TempDir(), rootLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := outputfs.Open(rootLink); err == nil {
		t.Fatal("outputfs.Open accepted a symlink output root")
	}
	fileRoot := filepath.Join(t.TempDir(), "file-root")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outputfs.Open(fileRoot); err == nil {
		t.Fatal("outputfs.Open accepted a non-directory output root")
	}

	// The downloader rejects malformed root-relative destinations.
	fs := openTestFS(t)
	d, _ := loopbackDownloader(t, imageHandler(), Options{})
	tests := []struct {
		name  string
		setID string
		rel   string
	}{
		{name: "traversal", setID: "11", rel: "../ref.jpg"},
		{name: "backslash traversal", setID: "11", rel: `dir\\..\\ref.jpg`},
		{name: "wrong component count", setID: "11", rel: "Set--11/ref.jpg"},
		{name: "missing set suffix", setID: "99", rel: "Collection--100/Set--11/ref.jpg"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			work := archive.DownloadWork{ReferenceID: test.name, Variants: []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/ref"}}, Destinations: []archive.Destination{{SetID: test.setID, RelativePath: test.rel}}}
			result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
			if result.Failure == nil && (len(result.Placements) == 0 || result.Placements[0].Failure == nil) {
				t.Fatalf("unsafe destination succeeded: %#v", result)
			}
		})
	}
}

func TestDownloadRejectsSymlinkParentTargetAndWrongTarget(t *testing.T) {
	fs := openTestFS(t)
	d, _ := loopbackDownloader(t, imageHandler(), Options{})
	realSet := displayPath(t, fs, "Collection--100/Set--real")
	if err := os.MkdirAll(realSet, 0700); err != nil {
		t.Fatal(err)
	}
	linkedParent := displayPath(t, fs, "Collection--100/Set--11")
	if err := os.Symlink(realSet, linkedParent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	linkedTarget := displayPath(t, fs, "Collection--100/Set--12/ref.jpg")
	if err := os.MkdirAll(filepath.Dir(linkedTarget), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(displayPath(t, fs, "Collection--100/Set--12/elsewhere"), linkedTarget); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	dirTarget := displayPath(t, fs, "Collection--100/Set--13/ref.jpg")
	if err := os.MkdirAll(filepath.Dir(dirTarget), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dirTarget, 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		setID string
		rel   string
	}{
		{name: "symlink parent", setID: "11", rel: "Collection--100/Set--11/ref.jpg"},
		{name: "symlink target", setID: "12", rel: "Collection--100/Set--12/ref.jpg"},
		{name: "directory target", setID: "13", rel: "Collection--100/Set--13/ref.jpg"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := d.Download(context.Background(), fs, []archive.DownloadWork{{ReferenceID: test.name, Variants: []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/ref"}}, Destinations: []archive.Destination{{SetID: test.setID, RelativePath: test.rel}}}})[0]
			if result.Placements[0].Failure == nil {
				t.Fatalf("unsafe destination succeeded: %#v", result)
			}
		})
	}
}

func TestDownloadAtomicReplacementPermissionsAndCleanup(t *testing.T) {
	fs := openTestFS(t)
	d, _ := loopbackDownloader(t, imageHandler(), Options{})
	relative := "Collection--100/Set--11/ref.jpg"
	target := displayPath(t, fs, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	result := d.Download(context.Background(), fs, []archive.DownloadWork{oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/ref"}}, relative)})[0]
	assertSuccess(t, result, "large")
	if got := mustReadFile(t, target); !reflect.DeepEqual(got, syntheticJPEG) {
		t.Fatal("existing target was not atomically replaced")
	}
	if mode := fileMode(t, target); mode.Perm() != 0600 {
		t.Fatalf("target mode = %o", mode.Perm())
	}
	for _, name := range []string{"Collection--100", "Collection--100/Set--11"} {
		if mode := fileMode(t, displayPath(t, fs, name)); mode.Perm() != 0700 {
			t.Fatalf("directory %q mode = %o", name, mode.Perm())
		}
	}
	root := filepath.Dir(displayPath(t, fs, "Collection--100"))
	downloadTemps, err := filepath.Glob(filepath.Join(root, ".pixiegrabber-download-*"))
	if err != nil || len(downloadTemps) != 0 {
		t.Fatalf("download temp files remain: %v, %v", downloadTemps, err)
	}
	setDir := displayPath(t, fs, "Collection--100/Set--11")
	placementTemps, err := filepath.Glob(filepath.Join(setDir, ".pixiegrabber-tmp-*"))
	if err != nil || len(placementTemps) != 0 {
		t.Fatalf("placement temp files remain: %v, %v", placementTemps, err)
	}
}

func TestDownloadRelativeOutputRootAndPreReplacementTargetPreservation(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "relative-root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDir) })
	fs, err := outputfs.Open(filepath.Base(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	d, _ := loopbackDownloader(t, imageHandler(), Options{})
	work := oneWork("relative", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/relative"}}, "Collection--100/Set--11/relative.jpg")
	result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
	assertSuccess(t, result, "large")

	preserved := displayPath(t, fs, "Collection--100/Set--11/preserved.jpg")
	if err := os.WriteFile(preserved, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	stage, _, err := fs.TempFile("", ".pixiegrabber-download-")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := stage.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	dirTarget := displayPath(t, fs, "Collection--100/Set--11/stage-directory")
	if err := os.Mkdir(dirTarget, 0700); err != nil {
		t.Fatal(err)
	}
	if err := installPlacement(fs, stage, 3, "new", archive.Destination{SetID: "11", RelativePath: "Collection--100/Set--11/stage-directory"}); err == nil {
		t.Fatal("directory staging unexpectedly replaced regular target")
	}
	if got := string(mustReadFile(t, preserved)); got != "keep" {
		t.Fatalf("pre-replacement failure changed target: %q", got)
	}
}

func TestDownloadFailuresAreSanitizedAndPreReplacementFailurePreservesTarget(t *testing.T) {
	fs := openTestFS(t)
	const secret = "transport-secret-token"
	d := productionDownloader(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New(secret)
	}))
	work := oneWork("ref", []pixieset.ImageVariant{{Quality: "large", URL: "https://images.pixieset.com/private?sig=" + secret}}, "Collection--100/Set--11/ref.jpg")
	result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
	assertFailureCode(t, result, CodeRetryExhausted)
	if text := fmt.Sprintf("%#v", result); strings.Contains(text, secret) || strings.Contains(text, "images.pixieset.com") || strings.Contains(text, "sig=") {
		t.Fatalf("failure leaked sensitive source data: %s", text)
	}

	target := displayPath(t, fs, "Collection--100/Set--11/existing.jpg")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	local := d.Download(context.Background(), fs, []archive.DownloadWork{archive.DownloadWork{
		ReferenceID: "local", Variants: []pixieset.ImageVariant{{Quality: "large", URL: "https://images.pixieset.com/local"}}, Destinations: []archive.Destination{{SetID: "11", RelativePath: "Collection--100/Set--11/existing.jpg"}},
	}})[0]
	assertFailureCode(t, local, CodeRetryExhausted)
	if got := string(mustReadFile(t, target)); got != "keep" {
		t.Fatalf("target changed after source failure: %q", got)
	}
}

func TestDownloadRejectsResponseOverPerReferenceLimit(t *testing.T) {
	fs := openTestFS(t)
	big := make([]byte, minResponseSlack+1)
	copy(big, syntheticJPEG)
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(big)
	}), Options{})
	work := archive.DownloadWork{
		ReferenceID:  "big",
		SourceBytes:  1,
		Variants:     []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/big"}},
		Destinations: []archive.Destination{{SetID: "11", RelativePath: "Collection--100/Set--11/big.jpg"}},
	}
	result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
	assertFailureCode(t, result, CodeMalformedMedia)
}

func TestDownloadRejectsImageDimensionsOverLimits(t *testing.T) {
	fs := openTestFS(t)
	oversized := pngWithDimensions(70000, 1)
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(oversized)
	}), Options{})
	work := oneWork("big", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/big"}}, "Collection--100/Set--11/big.png")
	result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
	assertFailureCode(t, result, CodeMalformedMedia)
}

func TestDownloadRejectsTruncatedImage(t *testing.T) {
	fs := openTestFS(t)
	truncated := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	d, _ := loopbackDownloader(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(truncated)
	}), Options{})
	work := oneWork("bad", []pixieset.ImageVariant{{Quality: "large", URL: d.mediaOrigin + "/bad"}}, "Collection--100/Set--11/bad.jpg")
	result := d.Download(context.Background(), fs, []archive.DownloadWork{work})[0]
	assertFailureCode(t, result, CodeMalformedMedia)
}

func TestNewRejectsInvalidBounds(t *testing.T) {
	client := &http.Client{}
	for _, options := range []Options{
		{Client: nil, Concurrency: 1, MaxAttempts: 1},
		{Client: client, Concurrency: -1, MaxAttempts: 1},
		{Client: client, Concurrency: 65, MaxAttempts: 1},
		{Client: client, Concurrency: 1, MaxAttempts: -1},
		{Client: client, Concurrency: 1, MaxAttempts: 9},
	} {
		if _, err := New(options); err == nil {
			t.Fatalf("New(%#v) accepted invalid options", options)
		}
	}
}

func pngWithDimensions(width, height uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 6  // color type RGBA
	ihdr[10] = 0 // compression
	ihdr[11] = 0 // filter
	ihdr[12] = 0 // interlace
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], 13)
	buf.Write(length[:])
	buf.WriteString("IHDR")
	buf.Write(ihdr)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
	buf.Write(crc[:])
	return buf.Bytes()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func imageResponse(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body))}
}

type chunkBody struct {
	data    []byte
	maxRead *int
}

func (body *chunkBody) Read(p []byte) (int, error) {
	if len(body.data) == 0 {
		return 0, io.EOF
	}
	n := 3
	if n > len(body.data) {
		n = len(body.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, body.data[:n])
	body.data = body.data[n:]
	if n > *body.maxRead {
		*body.maxRead = n
	}
	return n, nil
}

func (body *chunkBody) Close() error { return nil }

func imageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	})
}

func mustReadFile(t *testing.T, filename string) []byte {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fileMode(t *testing.T, filename string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}
