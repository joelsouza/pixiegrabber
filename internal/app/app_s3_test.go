package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"

	"pixiegrabber/internal/archive"
	"pixiegrabber/internal/browsercookies"
	"pixiegrabber/internal/manifest"
	"pixiegrabber/internal/runlog"
)

const s3TestBucket = "pixiegrabber-e2e"

// s3Fixture starts an in-memory S3 server, creates a bucket, and returns the
// endpoint (host[:port] without a scheme) and a client for assertions.
func s3Fixture(t *testing.T) (endpoint string, client *minio.Client) {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	server := httptest.NewServer(faker.Server())
	t.Cleanup(server.Close)

	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	if err := client.MakeBucket(context.Background(), s3TestBucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	return strings.TrimPrefix(server.URL, "http://"), client
}

// s3Options builds S3-mode Options reusing the fixture session seam. The
// access and secret keys are supplied through the environment, matching how
// Run reads them.
func s3Options(apiURL, mediaURL, endpoint string) Options {
	jar, _ := cookiejar.New(nil)
	return Options{
		CookiesFromBrowser: "test",
		Yes:                true,
		Concurrency:        2,
		UserAgent:          "PixiegrabberTest/1",
		S3:                 true,
		S3Endpoint:         endpoint,
		S3Bucket:           s3TestBucket,
		S3Region:           "us-east-1",
		S3PathStyle:        true,
		S3Secure:           false,
		apiBaseURL:         apiURL,
		mediaOrigin:        mediaURL,
		session:            browsercookies.Session{Jar: jar},
	}
}

// listKeys returns every object key in the bucket.
func listKeys(t *testing.T, client *minio.Client) map[string]struct{} {
	t.Helper()
	keys := make(map[string]struct{})
	for object := range client.ListObjects(context.Background(), s3TestBucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			t.Fatalf("ListObjects: %v", object.Err)
		}
		keys[object.Key] = struct{}{}
	}
	return keys
}

// getObject returns the bytes of one object, failing if it is absent.
func getObject(t *testing.T, client *minio.Client, key string) []byte {
	t.Helper()
	object, err := client.GetObject(context.Background(), s3TestBucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject %q: %v", key, err)
	}
	defer object.Close()
	data, err := io.ReadAll(object)
	if err != nil {
		t.Fatalf("ReadAll %q: %v", key, err)
	}
	return data
}

func TestRunS3ModeWritesTheRunLogToTheBucket(t *testing.T) {
	endpoint, client := s3Fixture(t)
	t.Setenv("PIXIEGRABBER_S3_ACCESS_KEY", "test-access")
	t.Setenv("PIXIEGRABBER_S3_SECRET_KEY", "test-secret")
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]")
	defer api.Close()

	if err := Run(context.Background(), s3Options(api.URL, media.URL, endpoint), io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	if _, ok := listKeys(t, client)[runlog.LogFilename]; !ok {
		t.Fatalf("bucket has no %s: %v", runlog.LogFilename, listKeys(t, client))
	}
	text := string(getObject(t, client, runlog.LogFilename))
	for _, want := range []string{`"ev":"run_start"`, `"mode":"s3"`, `"outcome":"completed"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("run log has no %s:\n%s", want, text)
		}
	}
	// The bucket name may appear, but no key and no URL.
	if strings.Contains(text, "://") {
		t.Fatalf("run log holds a URL:\n%s", text)
	}
	if strings.Contains(text, "test-access") || strings.Contains(text, "test-secret") {
		t.Fatalf("run log holds an S3 credential:\n%s", text)
	}
}

func TestRunS3ModeDownloadsToBucket(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", "[]")
	defer api.Close()

	endpoint, client := s3Fixture(t)
	t.Setenv("PIXIEGRABBER_S3_ACCESS_KEY", "test-access")
	t.Setenv("PIXIEGRABBER_S3_SECRET_KEY", "test-secret")

	if err := Run(context.Background(), s3Options(api.URL, media.URL, endpoint), io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	keys := listKeys(t, client)
	for _, want := range []string{
		"Collection--1/collection.json",
		"Collection--1/Set--2/Photo--3.jpg",
	} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("bucket missing object %q; keys = %v", want, keys)
		}
	}

	// The reference file must match the fixture bytes exactly.
	if got := getObject(t, client, "Collection--1/Set--2/Photo--3.jpg"); !bytes.Equal(got, syntheticJPEG) {
		t.Fatalf("reference bytes = %d bytes, want the fixture JPEG", len(got))
	}

	// The manifest must record the collection, the reference checksum, and the
	// placement path.
	manifestData := getObject(t, client, "Collection--1/collection.json")
	var m manifest.Manifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Collection.ID != "1" {
		t.Fatalf("collection ID = %q, want 1", m.Collection.ID)
	}
	if len(m.References) != 1 {
		t.Fatalf("references = %d, want 1", len(m.References))
	}
	ref := m.References[0]
	if ref.SHA256 == "" {
		t.Fatal("reference SHA-256 is empty")
	}
	if ref.DownloadState != manifest.DownloadComplete {
		t.Fatalf("reference download state = %q, want complete", ref.DownloadState)
	}
	if len(ref.Placements) != 1 {
		t.Fatalf("placements = %d, want 1", len(ref.Placements))
	}
	if ref.Placements[0].Path != "Set--2/Photo--3.jpg" {
		t.Fatalf("placement path = %q, want Set--2/Photo--3.jpg", ref.Placements[0].Path)
	}
	if ref.Placements[0].DownloadState != manifest.DownloadComplete {
		t.Fatalf("placement download state = %q, want complete", ref.Placements[0].DownloadState)
	}
}

func TestRunS3ModeVideoStopWritesDiagnosticToBucket(t *testing.T) {
	var mediaCalls atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaCalls.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(syntheticJPEG)
	}))
	defer media.Close()
	api := apiServer(t, media.URL+"/photo.jpg", `[{"kind":"video","safe":true}]`)
	defer api.Close()

	endpoint, client := s3Fixture(t)
	t.Setenv("PIXIEGRABBER_S3_ACCESS_KEY", "test-access")
	t.Setenv("PIXIEGRABBER_S3_SECRET_KEY", "test-secret")

	var stdout bytes.Buffer
	err := Run(context.Background(), s3Options(api.URL, media.URL, endpoint), &stdout, strings.NewReader(""))
	if !errors.Is(err, archive.ErrUnsupportedVideo) {
		t.Fatalf("error = %v, want ErrUnsupportedVideo", err)
	}

	keys := listKeys(t, client)
	if _, ok := keys["pixiegrabber-unsupported-video.json"]; !ok {
		t.Fatalf("bucket missing diagnostic; keys = %v", keys)
	}
	if data := getObject(t, client, "pixiegrabber-unsupported-video.json"); len(data) == 0 {
		t.Fatal("diagnostic object is empty")
	}
	if !strings.Contains(stdout.String(), "pixiegrabber-unsupported-video.json") {
		t.Fatalf("stdout did not print the diagnostic path: %q", stdout.String())
	}
	if mediaCalls.Load() != 0 {
		t.Fatalf("media server was called %d times during video stop", mediaCalls.Load())
	}
}
