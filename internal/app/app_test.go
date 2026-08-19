package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"pixiegrabber/internal/archive"
	"pixiegrabber/internal/browsercookies"
	"pixiegrabber/internal/manifest"
	"pixiegrabber/internal/outputfs"
	"pixiegrabber/internal/runlog"
)

func TestReportCookieSource(t *testing.T) {
	tests := []struct {
		name    string
		session browsercookies.Session
		want    []string
		absent  []string
	}{
		{
			name:    "profile only",
			session: browsercookies.Session{Browser: "chrome", Profile: "Personal", SessionCookies: 5, TokenCookies: 1, Cookies: 19},
			want: []string{
				`Using chrome profile "Personal" (5 session cookies, 19 Pixieset cookies).`,
				`Override with --cookies-from-browser 'chrome:Personal'.`,
			},
			absent: []string{"Sign in to Pixieset again"},
		},
		{
			name:    "container and no token",
			session: browsercookies.Session{Browser: "firefox", Profile: "default-release", Container: "Work", SessionCookies: 2, Cookies: 4},
			want: []string{
				`container "Work"`,
				`Override with --cookies-from-browser 'firefox:default-release::Work'.`,
				"Sign in to Pixieset again",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			reportCookieSource(&out, tt.session)
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("output %q does not contain %q", out.String(), want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(out.String(), absent) {
					t.Fatalf("output %q contains %q", out.String(), absent)
				}
			}
		})
	}
}

var syntheticJPEG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// syntheticMP4 is a minimal MP4: one ftyp box of 20 bytes (big-endian size,
// "ftyp", major brand "isom", minor version 0, compatible brand "mp42"). The
// "mp42" brand is required so http.DetectContentType reports video/mp4.
var syntheticMP4 = func() []byte {
	var buf bytes.Buffer
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], 20)
	buf.Write(size[:])
	buf.WriteString("ftyp")
	buf.WriteString("isom")
	var version [4]byte
	buf.Write(version[:])
	buf.WriteString("mp42")
	return buf.Bytes()
}()

func apiServer(t *testing.T, mediaURL, videos string, videoCount int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/dashboard_listings":
			_, _ = io.WriteString(w, `{"data":{"data":{"collections":[{"id":"1","name":"Collection","description":"","photo_count":1,"video_count":0,"rank":4}]},"meta":{"current_page":1,"last_page":1}}}`)
		case "/api/v1/collections/1/galleries":
			_, _ = io.WriteString(w, `{"data":[{"id":"2","collection_id":"1","name":"Set","description":"","photo_count":1,"video_count":0,"rank":2}]}`)
		case "/api/v1/galleries/2":
			_, _ = io.WriteString(w, `{"data":{"id":"2","collection_id":"1","name":"Set","description":"","photo_count":1,"video_count":`+strconv.Itoa(videoCount)+`,"rank":2,"photos":[{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","description":"","mime_type":"image/jpeg","ext":"jpg","size":12,"width":20,"height":30,"rank":1,"path_xxlarge":"`+mediaURL+`"}],"videos":`+videos+`}}`)
		default:
			t.Errorf("unexpected API path %q", r.URL.Path)
		}
	}))
}

func testOptions(output, apiURL, mediaURL string) Options {
	jar, _ := cookiejar.New(nil)
	return Options{
		CookiesFromBrowser: "test",
		Output:             output,
		Yes:                true,
		Concurrency:        2,
		UserAgent:          "PixiegrabberTest/1",
		apiBaseURL:         apiURL,
		mediaOrigin:        mediaURL,
		session:            browsercookies.Session{Jar: jar},
	}
}

func tempOutput(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunWritesTheRunLogAndProgressLines(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]", 0)
	defer api.Close()

	output := tempOutput(t)
	var stdout bytes.Buffer
	if err := Run(context.Background(), testOptions(output, api.URL, media.URL), &stdout, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	// The person sees the Collection while a long search runs.
	if got := stdout.String(); !strings.Contains(got, "[1/1] Collection: 1 set, 1 image") {
		t.Fatalf("stdout = %q, want a Collection progress line", got)
	}

	data, err := os.ReadFile(filepath.Join(output, runlog.LogFilename))
	if err != nil {
		t.Fatalf("run log missing: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"ev":"run_start"`, `"ev":"discovery_done"`, `"ev":"collection"`,
		`"ev":"plan"`, `"ev":"run_end"`, `"outcome":"completed"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run log has no %s:\n%s", want, text)
		}
	}
	// Every line must be one JSON object, so a program can read the file.
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		if event["ev"] == nil || event["t"] == nil {
			t.Fatalf("line %q has no ev or t", line)
		}
	}
	// No URL and no secret may reach the file.
	if strings.Contains(text, "://") {
		t.Fatalf("run log holds a URL:\n%s", text)
	}
}

func TestQuietRunStillWritesTheRunLog(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]", 0)
	defer api.Close()

	output := tempOutput(t)
	options := testOptions(output, api.URL, media.URL)
	options.Quiet = true
	var stdout bytes.Buffer
	if err := Run(context.Background(), options, &stdout, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "1 set,") {
		t.Fatalf("quiet run printed progress: %q", stdout.String())
	}
	// displayPlan is not progress, so it stays.
	if !strings.Contains(stdout.String(), "Collections: 1") {
		t.Fatalf("quiet run lost the plan totals: %q", stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(output, runlog.LogFilename))
	if err != nil {
		t.Fatalf("run log missing: %v", err)
	}
	if !strings.Contains(string(data), `"ev":"collection"`) {
		t.Fatalf("quiet run lost the log events:\n%s", data)
	}
}

func TestRunLogRecordsADeclinedPlan(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]", 0)
	defer api.Close()

	output := tempOutput(t)
	options := testOptions(output, api.URL, media.URL)
	options.Yes = false
	if err := Run(context.Background(), options, io.Discard, strings.NewReader("n\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, runlog.LogFilename))
	if err != nil {
		t.Fatalf("run log missing: %v", err)
	}
	if !strings.Contains(string(data), `"outcome":"declined"`) {
		t.Fatalf("run log does not record the refusal:\n%s", data)
	}
}

func TestRunFirstRunDownloadsAndWritesManifest(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]", 0)
	defer api.Close()

	output := tempOutput(t)
	if err := Run(context.Background(), testOptions(output, api.URL, media.URL), io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	img, err := os.ReadFile(filepath.Join(output, "Collection--1", "Set--2", "Photo--3.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img, syntheticJPEG) {
		t.Fatal("downloaded image does not match the fixture")
	}

	fs, err := outputfs.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	m, err := manifest.Load(fs, "Collection--1/collection.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.Collection.ID != "1" || len(m.References) != 1 || m.References[0].DownloadState != manifest.DownloadComplete || m.References[0].Placements[0].DownloadState != manifest.DownloadComplete {
		t.Fatalf("manifest = %#v", m)
	}
}

func TestRunVideoStopWritesDiagnosticAndDoesNotDownload(t *testing.T) {
	var mediaCalls atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaCalls.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", `[{"kind":"video","safe":true}]`, 1)
	defer api.Close()

	output := tempOutput(t)
	var stdout bytes.Buffer
	err := Run(context.Background(), testOptions(output, api.URL, media.URL), &stdout, strings.NewReader(""))
	if !errors.Is(err, archive.ErrUnsupportedVideo) {
		t.Fatalf("error = %v, want ErrUnsupportedVideo", err)
	}
	if _, statErr := os.Stat(filepath.Join(output, "pixiegrabber-unsupported-video.json")); statErr != nil {
		t.Fatalf("diagnostic not written: %v", statErr)
	}
	if !strings.Contains(stdout.String(), "pixiegrabber-unsupported-video.json") {
		t.Fatalf("stdout did not print the diagnostic path: %q", stdout.String())
	}
	// The run log keeps counts and IDs only; no diagnostic path or URL.
	data, readErr := os.ReadFile(filepath.Join(output, runlog.LogFilename))
	if readErr != nil {
		t.Fatalf("run log missing: %v", readErr)
	}
	if strings.Contains(string(data), "pixiegrabber-unsupported-video") || strings.Contains(string(data), "://") {
		t.Fatalf("run log holds a diagnostic path or URL:\n%s", data)
	}
	if mediaCalls.Load() != 0 {
		t.Fatalf("media server was called %d times during video stop", mediaCalls.Load())
	}
}

func TestRunConfirmationGatesWork(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]", 0)
	defer api.Close()

	output := tempOutput(t)
	options := testOptions(output, api.URL, media.URL)
	options.Yes = false

	if err := Run(context.Background(), options, io.Discard, strings.NewReader("n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "Collection--1", "collection.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest written after 'n': %v", err)
	}

	if err := Run(context.Background(), options, io.Discard, strings.NewReader("y\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "Collection--1", "collection.json")); err != nil {
		t.Fatalf("manifest not written after 'y': %v", err)
	}
}

func TestRunResumeSkipsHealthyCollection(t *testing.T) {
	var mediaCalls atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaCalls.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]", 0)
	defer api.Close()

	output := tempOutput(t)
	options := testOptions(output, api.URL, media.URL)

	if err := Run(context.Background(), options, io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if mediaCalls.Load() != 1 {
		t.Fatalf("first run media calls = %d, want 1", mediaCalls.Load())
	}

	if err := Run(context.Background(), options, io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if mediaCalls.Load() != 1 {
		t.Fatalf("resume media calls = %d, want 1 (healthy skip)", mediaCalls.Load())
	}
}

func TestRunDownloadsVideosWhenEnabled(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/PLAYBACKID/high.mp4" {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(syntheticMP4)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	video := `{"id":"4","provider_id":3,"name":"Clip","width":1080,"height":1620,"mux_status":2,"metadata":"{\"status\":\"ready\",\"duration\":8.8,\"mp4_support\":\"standard\",\"static_renditions\":{\"status\":\"ready\",\"files\":[{\"name\":\"high.mp4\",\"ext\":\"mp4\",\"width\":1080,\"height\":1620,\"filesize\":2152769}]}}","video_source":"` + media.URL + `/PLAYBACKID.m3u8?token=synthetic-video-token"}`
	api := apiServer(t, media.URL+"/photo.jpg", "["+video+"]", 1)
	defer api.Close()

	output := tempOutput(t)
	options := testOptions(output, api.URL, media.URL)
	options.Videos = true
	if err := Run(context.Background(), options, io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	videoFile := filepath.Join(output, "Collection--1", "Set--2", "Clip--4.mp4")
	data, err := os.ReadFile(videoFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, syntheticMP4) {
		t.Fatal("downloaded video does not match the fixture")
	}

	fs, err := outputfs.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	m, err := manifest.Load(fs, "Collection--1/collection.json")
	if err != nil {
		t.Fatal(err)
	}
	var videoRef *manifest.Reference
	for i := range m.References {
		if m.References[i].ID == "4" {
			videoRef = &m.References[i]
		}
	}
	if videoRef == nil || videoRef.MediaType != "video" || videoRef.DownloadState != manifest.DownloadComplete || videoRef.SelectedQuality != "high" {
		t.Fatalf("video reference = %#v", videoRef)
	}
}

func TestRunRecordsUnavailableVideoAsMissing(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	dead := `{"id":"5","provider_id":3,"name":"Dead","width":0,"height":0,"mux_status":3,"metadata":"{\"status\":\"timed_out\"}","video_source":""}`
	api := apiServer(t, media.URL+"/photo.jpg", "["+dead+"]", 1)
	defer api.Close()

	output := tempOutput(t)
	options := testOptions(output, api.URL, media.URL)
	options.Videos = true
	if err := Run(context.Background(), options, io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	fs, err := outputfs.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	m, err := manifest.Load(fs, "Collection--1/collection.json")
	if err != nil {
		t.Fatal(err)
	}
	var deadRef *manifest.Reference
	for i := range m.References {
		if m.References[i].ID == "5" {
			deadRef = &m.References[i]
		}
	}
	if deadRef == nil || deadRef.MediaType != "video" || deadRef.PresenceState != manifest.PresenceMissing || len(deadRef.Placements) != 0 || deadRef.DownloadState != manifest.DownloadPending {
		t.Fatalf("dead video reference = %#v", deadRef)
	}
	if _, statErr := os.Stat(filepath.Join(output, "Collection--1", "Set--2", "Photo--3.jpg")); statErr != nil {
		t.Fatalf("photo not downloaded: %v", statErr)
	}
}
