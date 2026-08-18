package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
)

const testBucket = "pixiegrabber-test"

// newTestS3 starts an in-memory S3 server and returns a configured S3Store.
func newTestS3(t *testing.T, backend *s3mem.Backend) *S3Store {
	t.Helper()
	return newTestS3Config(t, backend, Config{})
}

// newTestS3Config is newTestS3 with a caller-supplied config override.
func newTestS3Config(t *testing.T, backend *s3mem.Backend, override Config) *S3Store {
	t.Helper()
	if backend == nil {
		backend = s3mem.New()
	}
	faker := gofakes3.New(backend)
	server := httptest.NewServer(faker.Server())
	t.Cleanup(server.Close)

	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
		Creds:        nil,
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	if err := client.MakeBucket(context.Background(), testBucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	cfg := Config{
		Endpoint:  strings.TrimPrefix(server.URL, "http://"),
		Bucket:    testBucket,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
		PathStyle: true,
		Secure:    false,
	}
	if override.LockStaleAfter != 0 {
		cfg.LockStaleAfter = override.LockStaleAfter
	}
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s
}

func TestS3RoundTrip(t *testing.T) {
	s := newTestS3(t, nil)
	rel := "Collection--1/Set--2/Photo--3.jpg"
	payload := []byte("hello s3")
	if err := s.Put(rel, bytes.NewReader(payload), int64(len(payload)), map[string]string{"sha256": "abc"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reader, err := s.Open(rel)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip bytes = %q, want %q", got, payload)
	}

	meta, err := s.Metadata(rel)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta["sha256"] != "abc" {
		t.Fatalf("Metadata sha256 = %q, want abc", meta["sha256"])
	}

	info, exists, err := s.Inspect(rel)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !exists || !info.Mode().IsRegular() || info.IsDir() {
		t.Fatalf("Inspect exists=%v IsRegular=%v IsDir=%v", exists, info.Mode().IsRegular(), info.IsDir())
	}
	if info.Size() != int64(len(payload)) {
		t.Fatalf("Inspect size = %d, want %d", info.Size(), len(payload))
	}
}

func TestS3ReadDirListsFilesAndDirs(t *testing.T) {
	s := newTestS3(t, nil)
	put := func(rel string) {
		t.Helper()
		if err := s.Put(rel, strings.NewReader("x"), 1, nil); err != nil {
			t.Fatalf("Put %q: %v", rel, err)
		}
	}
	put("Collection--1/Set--2/Photo--3.jpg")
	put("Collection--1/Set--2/Photo--4.jpg")
	put("Collection--1/Set--5/Photo--6.jpg")

	entries, err := s.ReadDir("Collection--1")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var dirs, files []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	if len(dirs) != 2 || dirs[0] != "Set--2" || dirs[1] != "Set--5" {
		t.Fatalf("dirs = %v, want [Set--2 Set--5]", dirs)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none at Collection level", files)
	}

	entries, err = s.ReadDir("Collection--1/Set--2")
	if err != nil {
		t.Fatalf("ReadDir Set: %v", err)
	}
	files = nil
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	if len(files) != 2 || files[0] != "Photo--3.jpg" || files[1] != "Photo--4.jpg" {
		t.Fatalf("files = %v, want [Photo--3.jpg Photo--4.jpg]", files)
	}
}

func TestS3InspectDirectoryPrefix(t *testing.T) {
	s := newTestS3(t, nil)
	if err := s.Put("Collection--1/Set--2/Photo--3.jpg", strings.NewReader("x"), 1, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, exists, err := s.Inspect("Collection--1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !exists || !info.IsDir() {
		t.Fatalf("Inspect dir exists=%v IsDir=%v", exists, info.IsDir())
	}
}

func TestS3RemoveAndOpenMissing(t *testing.T) {
	s := newTestS3(t, nil)
	rel := "Collection--1/Set--2/Photo--3.jpg"
	if err := s.Put(rel, strings.NewReader("x"), 1, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Remove(rel); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Open(rel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open after Remove error = %v, want os.ErrNotExist", err)
	}
	if _, exists, err := s.Inspect(rel); err != nil || exists {
		t.Fatalf("Inspect after Remove exists=%v err=%v", exists, err)
	}
}

func TestS3SameFile(t *testing.T) {
	s := newTestS3(t, nil)
	a := "Collection--1/Set--2/a.jpg"
	b := "Collection--1/Set--2/b.jpg"
	c := "Collection--1/Set--2/c.jpg"
	if err := s.Put(a, strings.NewReader("same"), 4, map[string]string{"sha256": "digest"}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := s.Put(b, strings.NewReader("same"), 4, map[string]string{"sha256": "digest"}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := s.Put(c, strings.NewReader("diff"), 4, map[string]string{"sha256": "other"}); err != nil {
		t.Fatalf("Put c: %v", err)
	}
	same, err := s.SameFile(a, b)
	if err != nil || !same {
		t.Fatalf("SameFile(a,b) = %v, %v; want true", same, err)
	}
	same, err = s.SameFile(a, c)
	if err != nil || same {
		t.Fatalf("SameFile(a,c) = %v, %v; want false", same, err)
	}
	// Missing object is false, not an error.
	same, err = s.SameFile(a, "Collection--1/Set--2/missing.jpg")
	if err != nil || same {
		t.Fatalf("SameFile(a,missing) = %v, %v; want false", same, err)
	}
}

func TestS3Lock(t *testing.T) {
	s := newTestS3(t, nil)
	release, err := s.Lock()
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if _, err := s.Lock(); err == nil {
		t.Fatal("second Lock succeeded, want failure")
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	release, err = s.Lock()
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	_ = release()
}

func TestS3LockStaleIsStolen(t *testing.T) {
	// A tiny stale threshold makes any existing lock immediately stale.
	s := newTestS3Config(t, nil, Config{LockStaleAfter: time.Nanosecond})
	if err := s.Put(".pixiegrabber.lock", strings.NewReader("stale"), 5, nil); err != nil {
		t.Fatalf("Put stale lock: %v", err)
	}
	release, err := s.Lock()
	if err != nil {
		t.Fatalf("Lock on stale lock: %v", err)
	}
	_ = release
}

func TestS3DisplayPath(t *testing.T) {
	s := newTestS3(t, nil)
	got, err := s.DisplayPath("Collection--1/Set--2/Photo--3.jpg")
	if err != nil {
		t.Fatalf("DisplayPath: %v", err)
	}
	want := "s3://" + testBucket + "/Collection--1/Set--2/Photo--3.jpg"
	if got != want {
		t.Fatalf("DisplayPath = %q, want %q", got, want)
	}
}

func TestS3NewRequiresExistingBucket(t *testing.T) {
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	server := httptest.NewServer(faker.Server())
	defer server.Close()
	_, err := NewS3(Config{
		Endpoint:  strings.TrimPrefix(server.URL, "http://"),
		Bucket:    "missing-bucket",
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
		PathStyle: true,
		Secure:    false,
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("NewS3 missing bucket error = %v, want 'does not exist'", err)
	}
	if strings.Contains(err.Error(), "test") {
		t.Fatalf("NewS3 error leaked the secret key: %v", err)
	}
}
