// Package store defines the storage abstraction used by the Pixiegrabber
// pipeline. All paths are root-relative keys mirroring the internal/paths
// layout, so a Store can be backed by a local directory or a remote object
// store without changing the callers.
package store

import (
	"io"
	"os"

	"pixiegrabber/internal/outputfs"
)

// Store is the storage surface used by the archive, download, and manifest
// packages. Implementations must treat rel as a root-relative key.
type Store interface {
	// Open returns a reader for rel. The error wraps os.ErrNotExist when the
	// object is absent.
	Open(rel string) (io.ReadCloser, error)
	// Inspect reports a non-link object. The boolean is false when absent.
	Inspect(rel string) (os.FileInfo, bool, error)
	// ReadDir lists the entries in a directory. A rel of "" or "." lists the
	// root.
	ReadDir(rel string) ([]os.DirEntry, error)
	// Put atomically replaces rel with the contents of r. metadata is optional
	// and may be ignored by the backend.
	Put(rel string, r io.Reader, size int64, metadata map[string]string) error
	// Remove deletes one regular, non-link object.
	Remove(rel string) error
	// MkdirAll creates missing directories below the root.
	MkdirAll(rel string) error
	// DisplayPath returns a path for messages or display. It must not be used
	// for filesystem operations.
	DisplayPath(rel string) (string, error)
	// Metadata returns backend metadata for rel, or nil when none exists.
	Metadata(rel string) (map[string]string, error)
	// SameFile reports whether a and b refer to the same underlying object.
	// It is false when either is absent.
	SameFile(a, b string) (bool, error)
	// Lock acquires the store-wide lock. The returned release function must be
	// called exactly once.
	Lock() (func() error, error)
	// Close releases the store and its lock.
	Close() error
}

// LocalStore is the local-filesystem implementation of Store.
type LocalStore = *outputfs.FS

// NewLocal opens a local output root and returns it as a Store.
func NewLocal(output string) (Store, error) {
	return outputfs.Open(output)
}
