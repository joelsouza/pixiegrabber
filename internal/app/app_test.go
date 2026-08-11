package app

import (
	"bytes"
	"context"
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
	"strings"
	"sync/atomic"
	"testing"

	"pixiegrabber/internal/archive"
	"pixiegrabber/internal/browsercookies"
	"pixiegrabber/internal/manifest"
	"pixiegrabber/internal/outputfs"
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

func apiServer(t *testing.T, mediaURL, videos string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/dashboard_listings":
			_, _ = io.WriteString(w, `{"data":{"data":{"collections":[{"id":"1","name":"Collection","description":"","photo_count":1,"video_count":0,"rank":4}]},"meta":{"current_page":1,"last_page":1}}}`)
		case "/api/v1/collections/1/galleries":
			_, _ = io.WriteString(w, `{"data":[{"id":"2","collection_id":"1","name":"Set","description":"","photo_count":1,"video_count":0,"rank":2}]}`)
		case "/api/v1/galleries/2":
			_, _ = io.WriteString(w, `{"data":{"id":"2","collection_id":"1","name":"Set","description":"","photo_count":1,"video_count":0,"rank":2,"photos":[{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","description":"","mime_type":"image/jpeg","ext":"jpg","size":12,"width":20,"height":30,"rank":1,"path_xxlarge":"`+mediaURL+`"}],"videos":`+videos+`}}`)
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

func TestRunFirstRunDownloadsAndWritesManifest(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]")
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
	api := apiServer(t, media.URL+"/photo.jpg", `[{"kind":"video","safe":true}]`)
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
	api := apiServer(t, media.URL+"/photo.jpg", "[]")
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
	api := apiServer(t, media.URL+"/photo.jpg", "[]")
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
