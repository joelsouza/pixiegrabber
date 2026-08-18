package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	defaultRegion         = "us-east-1"
	defaultLockStaleAfter = 10 * time.Minute
	lockObjectKey         = ".pixiegrabber.lock"
)

// Config configures an S3-compatible backend. Endpoint is host[:port] without
// a scheme. The secret key is never logged or included in returned errors.
type Config struct {
	Endpoint       string
	Bucket         string
	Region         string
	AccessKey      string
	SecretKey      string
	PathStyle      bool
	Secure         bool
	LockStaleAfter time.Duration
}

// S3Store is the S3-compatible implementation of Store. All paths are treated
// as root-relative object keys.
type S3Store struct {
	client         *minio.Client
	bucket         string
	lockStaleAfter time.Duration
}

// NewS3 validates cfg and opens an S3-compatible backend. The bucket must
// already exist; it is never created automatically.
func NewS3(cfg Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3 endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3 bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3 access key and secret key are required")
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}
	stale := cfg.LockStaleAfter
	if stale <= 0 {
		stale = defaultLockStaleAfter
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.Secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	exists, err := client.BucketExists(context.Background(), cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3 bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		return nil, fmt.Errorf("s3 bucket %q does not exist; create it before running", cfg.Bucket)
	}
	return &S3Store{client: client, bucket: cfg.Bucket, lockStaleAfter: stale}, nil
}

// Open returns a reader for rel. A missing object maps to os.ErrNotExist.
func (s *S3Store) Open(rel string) (io.ReadCloser, error) {
	// StatObject surfaces NoSuchKey synchronously so callers can rely on
	// errors.Is(err, os.ErrNotExist) without reading the body.
	if _, err := s.client.StatObject(context.Background(), s.bucket, rel, minio.StatObjectOptions{}); err != nil {
		return nil, notExistError("open", rel, err)
	}
	object, err := s.client.GetObject(context.Background(), s.bucket, rel, minio.GetObjectOptions{})
	if err != nil {
		return nil, notExistError("open", rel, err)
	}
	return object, nil
}

// Inspect reports a non-link object. A missing object with children is
// reported as an existing directory.
func (s *S3Store) Inspect(rel string) (os.FileInfo, bool, error) {
	info, err := s.client.StatObject(context.Background(), s.bucket, rel, minio.StatObjectOptions{})
	if err == nil {
		return s3FileInfo{info: info}, true, nil
	}
	if !isNoSuchKey(err) {
		return nil, false, err
	}
	// Probe for a directory prefix: any child under rel/ means rel is a
	// directory.
	children := s.client.ListObjectsIter(context.Background(), s.bucket, minio.ListObjectsOptions{
		Prefix:  rel + "/",
		MaxKeys: 1,
	})
	next, stop := iter.Pull(children)
	defer stop()
	child, ok := next()
	if !ok {
		return nil, false, nil
	}
	if child.Err != nil {
		return nil, false, child.Err
	}
	return s3FileInfo{dir: true, name: rel}, true, nil
}

// ReadDir lists the entries in a directory. A rel of "" or "." lists the root.
func (s *S3Store) ReadDir(rel string) ([]os.DirEntry, error) {
	prefix := ""
	if rel != "" && rel != "." {
		prefix = strings.TrimSuffix(rel, "/") + "/"
	}
	// Recursive=false uses "/" as the delimiter, so CommonPrefixes arrive as
	// directory entries and Contents as regular files.
	objects := s.client.ListObjects(context.Background(), s.bucket, minio.ListObjectsOptions{
		Prefix: prefix,
	})
	var entries []os.DirEntry
	for object := range objects {
		if object.Err != nil {
			return nil, object.Err
		}
		entries = append(entries, s3DirEntry{info: object})
	}
	return entries, nil
}

// Put atomically replaces rel with the contents of r.
func (s *S3Store) Put(rel string, r io.Reader, size int64, metadata map[string]string) error {
	_, err := s.client.PutObject(context.Background(), s.bucket, rel, r, size, minio.PutObjectOptions{
		UserMetadata: metadata,
	})
	return err
}

// Remove deletes one regular object.
func (s *S3Store) Remove(rel string) error {
	return s.client.RemoveObject(context.Background(), s.bucket, rel, minio.RemoveObjectOptions{})
}

// MkdirAll is a no-op because S3 has no directories.
func (s *S3Store) MkdirAll(rel string) error {
	return nil
}

// DisplayPath returns a display path for messages. It must not be used for
// filesystem operations.
func (s *S3Store) DisplayPath(rel string) (string, error) {
	return "s3://" + s.bucket + "/" + rel, nil
}

// Metadata returns backend metadata for rel, or nil when none exists. Keys are
// lowercased so callers can rely on "sha256" regardless of S3 header casing.
func (s *S3Store) Metadata(rel string) (map[string]string, error) {
	info, err := s.client.StatObject(context.Background(), s.bucket, rel, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
	}
	return lowerMetadata(info.UserMetadata), nil
}

// SameFile reports whether a and b refer to the same underlying object. It is
// false when either is absent or lacks a sha256 metadata value.
func (s *S3Store) SameFile(a, b string) (bool, error) {
	aInfo, err := s.statOrAbsent(a)
	if err != nil {
		return false, err
	}
	bInfo, err := s.statOrAbsent(b)
	if err != nil {
		return false, err
	}
	if aInfo == nil || bInfo == nil {
		return false, nil
	}
	if aInfo.Size != bInfo.Size {
		return false, nil
	}
	aSHA := lowerMetadata(aInfo.UserMetadata)["sha256"]
	bSHA := lowerMetadata(bInfo.UserMetadata)["sha256"]
	if aSHA == "" || bSHA == "" {
		return false, nil
	}
	return strings.EqualFold(aSHA, bSHA), nil
}

// Lock acquires the store-wide lock. The returned release function must be
// called exactly once. Because minio-go v7.3.0 does not expose a conditional
// PUT on PutObjectOptions, the lock uses a check-then-create sequence with
// stale-lock stealing.
func (s *S3Store) Lock() (func() error, error) {
	token := randomToken()
	release := func() error {
		return s.client.RemoveObject(context.Background(), s.bucket, lockObjectKey, minio.RemoveObjectOptions{})
	}
	for attempt := 0; attempt < 2; attempt++ {
		info, err := s.client.StatObject(context.Background(), s.bucket, lockObjectKey, minio.StatObjectOptions{})
		if err == nil {
			if time.Since(info.LastModified) > s.lockStaleAfter {
				if err := s.client.RemoveObject(context.Background(), s.bucket, lockObjectKey, minio.RemoveObjectOptions{}); err != nil {
					return nil, err
				}
				continue
			}
			return nil, errors.New("store is locked")
		}
		if !isNoSuchKey(err) {
			return nil, err
		}
		_, err = s.client.PutObject(context.Background(), s.bucket, lockObjectKey, strings.NewReader(token), int64(len(token)), minio.PutObjectOptions{})
		if err != nil {
			return nil, err
		}
		return release, nil
	}
	return nil, errors.New("store is locked")
}

// Close is a no-op for the S3 backend.
func (s *S3Store) Close() error {
	return nil
}

func (s *S3Store) statOrAbsent(rel string) (*minio.ObjectInfo, error) {
	info, err := s.client.StatObject(context.Background(), s.bucket, rel, minio.StatObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, nil
		}
		return nil, err
	}
	return &info, nil
}

func isNoSuchKey(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey"
}

// notExistError maps a NoSuchKey error to a *os.PathError wrapping
// os.ErrNotExist. A PathError is required so both errors.Is and os.IsNotExist
// recognize the absence (os.IsNotExist does not unwrap fmt.wrapError).
func notExistError(op, rel string, err error) error {
	if !isNoSuchKey(err) {
		return err
	}
	return &os.PathError{Op: op, Path: rel, Err: os.ErrNotExist}
}

// lowerMetadata returns a copy of meta with lowercased keys. S3 user metadata
// header names are canonicalized to title case by the HTTP layer, so callers
// cannot rely on the exact key casing.
func lowerMetadata(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]string, len(meta))
	for key, value := range meta {
		out[strings.ToLower(key)] = value
	}
	return out
}

func randomToken() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}

// s3FileInfo implements os.FileInfo for a single object or a directory prefix.
type s3FileInfo struct {
	info minio.ObjectInfo
	dir  bool
	name string
}

func (f s3FileInfo) Name() string {
	if f.dir {
		return f.name
	}
	return path.Base(f.info.Key)
}

func (f s3FileInfo) Size() int64 { return f.info.Size }

func (f s3FileInfo) Mode() os.FileMode {
	if f.dir {
		return os.ModeDir | 0755
	}
	return 0644
}

func (f s3FileInfo) ModTime() time.Time { return f.info.LastModified }

func (f s3FileInfo) IsDir() bool { return f.dir }

func (f s3FileInfo) Sys() any { return nil }

// s3DirEntry implements os.DirEntry for a listing entry.
type s3DirEntry struct {
	info minio.ObjectInfo
}

func (e s3DirEntry) Name() string {
	return path.Base(strings.TrimSuffix(e.info.Key, "/"))
}

func (e s3DirEntry) IsDir() bool {
	return strings.HasSuffix(e.info.Key, "/")
}

func (e s3DirEntry) Type() os.FileMode {
	if e.IsDir() {
		return os.ModeDir
	}
	return 0
}

func (e s3DirEntry) Info() (os.FileInfo, error) {
	return s3FileInfo{info: e.info, dir: e.IsDir()}, nil
}
