// Package outputfs provides link-resistant access to one private output root.
package outputfs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	"pixiegrabber/internal/privatefs"
)

const (
	lockName         = ".pixiegrabber.lock"
	atomicTempPrefix = ".pixiegrabber-tmp-"
	privateFileMode  = 0600
	privateDirMode   = 0700
	maxTempAttempts  = 32
)

var (
	ErrLocked        = errors.New("output root is locked")
	errInvalidRoot   = errors.New("invalid output root")
	errInvalidRel    = errors.New("invalid relative path")
	errInvalidPrefix = errors.New("invalid temporary prefix")
	errLink          = errors.New("link is not allowed")
	errWrongType     = errors.New("path has the wrong type")
	errChanged       = errors.New("path changed during operation")
	errTempCollision = errors.New("temporary name collision limit reached")
	errLockName      = errors.New("lock name is reserved")
)

// lockCloser is the subset of *flock.Flock used by FS. It is an interface so
// tests can simulate a failing flock close.
type lockCloser interface {
	Close() error
}

// FS owns a private absolute root and its persistent process lock.
type FS struct {
	rootAbs string
	root    *os.Root
	lock    lockCloser

	// mu serializes public operations against Close. Public operations hold
	// the shared lock for their whole duration; Close holds the exclusive
	// lock so it waits for in-flight operations before releasing the flock.
	mu     sync.RWMutex
	closed bool
}

// Open creates or restricts path and acquires its persistent lock.
func Open(path string) (*FS, error) {
	if path == "" {
		return nil, opError("open output root", errInvalidRoot)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, opError("open output root", err)
	}
	abs = filepath.Clean(abs)
	if err := validateOutputRoot(abs); err != nil {
		return nil, opError("validate output root", err)
	}

	if err := privatefs.MkdirAll(abs); err != nil {
		return nil, opError("create output root", err)
	}
	if err := verifyAbsoluteComponents(abs); err != nil {
		return nil, opError("verify output root", err)
	}
	if err := privatefs.Restrict(abs, true); err != nil {
		return nil, opError("restrict output root", err)
	}

	preOpen, err := os.Lstat(abs)
	if err != nil {
		return nil, opError("stat output root", err)
	}
	if privatefs.IsLink(preOpen) || !preOpen.IsDir() {
		return nil, opError("verify output root", errWrongType)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, opError("open output root", err)
	}
	actual, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, opError("verify output root", err)
	}
	if !os.SameFile(preOpen, actual) {
		_ = root.Close()
		return nil, opError("verify output root", errChanged)
	}

	f := &FS{rootAbs: abs, root: root}
	lock, err := f.acquireLock()
	if err != nil {
		return nil, openCleanup(root, lock, err)
	}
	f.lock = lock
	return f, nil
}

// Close releases the lock and closes the root. It is safe to call more than
// once. It waits for any in-flight public operation to finish, closes the
// rooted handles, and releases the flock last. If it fails, a later Close
// retries without losing the lock handle.
func (f *FS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	// Close the rooted handles first, then release the flock last so no
	// operation can touch the root after the lock is gone.
	var rootErr, lockErr error
	if f.root != nil {
		rootErr = f.root.Close()
		if errors.Is(rootErr, os.ErrClosed) {
			rootErr = nil
		}
	}
	if f.lock != nil {
		lockErr = f.lock.Close()
	}
	if err := errors.Join(rootErr, lockErr); err != nil {
		// Keep the handles so a later Close can retry.
		return err
	}
	f.closed = true
	return nil
}

// DisplayPath returns a path for messages or display. It must not be used for
// filesystem operations.
func (f *FS) DisplayPath(rel string) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.displayPath(rel)
}

func (f *FS) displayPath(rel string) (string, error) {
	if _, err := validateRelative(rel); err != nil {
		return "", opError("display path", err)
	}
	return filepath.Join(f.rootAbs, filepath.FromSlash(rel)), nil
}

// MkdirAll creates missing directories below the root.
func (f *FS) MkdirAll(rel string) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.mkdirAll(rel)
}

func (f *FS) mkdirAll(rel string) error {
	parts, err := validateRelative(rel)
	if err != nil {
		return opError("mkdir all", err)
	}
	if slices.Contains(parts, lockName) {
		return opError("mkdir all", errLockName)
	}
	if err := f.ensureDirectories(parts); err != nil {
		return opError("mkdir all", err)
	}
	return nil
}

// Inspect reports a non-link object. The boolean is false when the final
// object does not exist.
func (f *FS) Inspect(rel string) (info os.FileInfo, exists bool, result error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.inspect(rel)
}

func (f *FS) inspect(rel string) (info os.FileInfo, exists bool, result error) {
	parts, err := validateRelative(rel)
	if err != nil {
		return nil, false, opError("inspect", err)
	}
	parent, pinned, err := f.pinParent(parts[:len(parts)-1])
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, opError("inspect", err)
	}
	defer func() {
		if closeErr := closePinned(pinned); closeErr != nil {
			result = errors.Join(result, opError("inspect", closeErr))
		}
	}()
	info, err = parent.Lstat(parts[len(parts)-1])
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, opError("inspect", err)
	}
	if privatefs.IsLink(info) {
		return nil, false, opError("inspect", errLink)
	}
	return info, true, nil
}

// Open returns a reader for a regular, non-link file. It satisfies the
// store.Store interface and delegates to OpenRegular.
func (f *FS) Open(rel string) (io.ReadCloser, error) {
	return f.OpenRegular(rel)
}

// Put atomically replaces rel with the contents of r. It satisfies the
// store.Store interface and reuses the AtomicReplace machinery, so the
// no-overwrite and atomic semantics of media and manifest writes are
// preserved. metadata is ignored for the local backend.
func (f *FS) Put(rel string, r io.Reader, size int64, metadata map[string]string) error {
	if r == nil {
		return opError("put", errors.New("nil reader"))
	}
	return f.AtomicReplace(rel, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// Metadata returns backend metadata for rel. The local backend stores none, so
// it always returns nil.
func (f *FS) Metadata(rel string) (map[string]string, error) {
	return nil, nil
}

// SameFile reports whether a and b refer to the same underlying object. It is
// false when either is absent.
func (f *FS) SameFile(a, b string) (bool, error) {
	aInfo, aExists, err := f.Inspect(a)
	if err != nil {
		return false, err
	}
	bInfo, bExists, err := f.Inspect(b)
	if err != nil {
		return false, err
	}
	if !aExists || !bExists {
		return false, nil
	}
	return os.SameFile(aInfo, bInfo), nil
}

// Lock returns a no-op release function. The persistent flock is already held
// by Open and released by Close.
func (f *FS) Lock() (func() error, error) {
	return func() error { return nil }, nil
}

// OpenRegular opens a regular, non-link file for reading.
func (f *FS) OpenRegular(rel string) (*os.File, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.openRegular(rel)
}

func (f *FS) openRegular(rel string) (*os.File, error) {
	parts, err := validateRelative(rel)
	if err != nil {
		return nil, opError("open regular", err)
	}
	parent, pinned, err := f.pinParent(parts[:len(parts)-1])
	if err != nil {
		return nil, opError("open regular", err)
	}
	leaf := parts[len(parts)-1]
	initial, err := parent.Lstat(leaf)
	if err != nil {
		_ = closePinned(pinned)
		return nil, opError("open regular", err)
	}
	if err := verifyRegular(initial); err != nil {
		_ = closePinned(pinned)
		return nil, opError("open regular", err)
	}
	file, err := parent.OpenFile(leaf, os.O_RDONLY, 0)
	if err != nil {
		_ = closePinned(pinned)
		return nil, opError("open regular", err)
	}
	opened, err := file.Stat()
	if err == nil {
		err = verifyRegular(opened)
	}
	if err == nil && !os.SameFile(initial, opened) {
		err = errChanged
	}
	second, secondErr := parent.Lstat(leaf)
	if err == nil && secondErr != nil {
		err = secondErr
	}
	if err == nil {
		if privatefs.IsLink(second) {
			err = errLink
		} else if !os.SameFile(initial, second) {
			err = errChanged
		}
	}
	if err != nil {
		closeErr := file.Close()
		_ = closePinned(pinned)
		return nil, errors.Join(opError("open regular", err), closeErr)
	}
	if err := closePinned(pinned); err != nil {
		_ = file.Close()
		return nil, opError("open regular", err)
	}
	return file, nil
}

// TempFile creates a private randomized file and returns its root-relative
// name.
func (f *FS) TempFile(dir, prefix string) (*os.File, string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.tempFile(dir, prefix)
}

func (f *FS) tempFile(dir, prefix string) (*os.File, string, error) {
	if dir != "" {
		if _, err := validateRelative(dir); err != nil {
			return nil, "", opError("create temp", err)
		}
		if err := f.mkdirAll(dir); err != nil {
			return nil, "", opError("create temp", err)
		}
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, "", opError("create temp", err)
	}
	dirParts := []string{}
	if dir != "" {
		var err error
		dirParts, err = validateRelative(dir)
		if err != nil {
			return nil, "", opError("create temp", err)
		}
	}
	if slices.Contains(dirParts, lockName) {
		return nil, "", opError("create temp", errLockName)
	}
	parent, pinned, err := f.pinParent(dirParts)
	if err != nil {
		return nil, "", opError("create temp", err)
	}
	file, leaf, info, err := createSibling(parent, prefix)
	if err != nil {
		_ = closePinned(pinned)
		return nil, "", opError("create temp", err)
	}
	closeErr := closePinned(pinned)
	if closeErr != nil {
		fileCloseErr := file.Close()
		cleanupErr := f.cleanupCreatedRelative(joinRelative(dir, leaf), info)
		return nil, "", errors.Join(opError("create temp", closeErr), fileCloseErr, cleanupErr)
	}
	return file, joinRelative(dir, leaf), nil
}

// AtomicReplace writes a new regular file and renames it over rel.
func (f *FS) AtomicReplace(rel string, write func(io.Writer) error) (result error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.atomicReplace(rel, write)
}

func (f *FS) atomicReplace(rel string, write func(io.Writer) error) (result error) {
	if write == nil {
		return opError("atomic replace", errors.New("nil writer"))
	}
	parts, err := validateRelative(rel)
	if err != nil {
		return opError("atomic replace", err)
	}
	if slices.Contains(parts, lockName) {
		return opError("atomic replace", errLockName)
	}
	parentParts := parts[:len(parts)-1]
	if len(parentParts) != 0 {
		if err := f.mkdirAll(strings.Join(parentParts, "/")); err != nil {
			return opError("atomic replace", err)
		}
	}
	parent, pinned, err := f.pinParent(parentParts)
	if err != nil {
		return opError("atomic replace", err)
	}
	defer func() {
		if closeErr := closePinned(pinned); closeErr != nil {
			result = errors.Join(result, opError("atomic replace", closeErr))
		}
	}()

	targetLeaf := parts[len(parts)-1]
	file, tempLeaf, info, err := createSibling(parent, atomicTempPrefix)
	if err != nil {
		return opError("atomic replace", err)
	}
	open := true
	cleanup := func(base error) error {
		var closeErr error
		if open {
			closeErr = file.Close()
			open = false
		}
		return errors.Join(base, closeErr, cleanupCreated(parent, tempLeaf, info))
	}
	// Recover a panic from the callback so it cannot crash the process, then
	// clean up the temporary file and return an error instead.
	writeErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("callback panic: %v", r)
			}
		}()
		return write(file)
	}()
	if writeErr != nil {
		return cleanup(opError("write replacement", writeErr))
	}
	if err := file.Sync(); err != nil {
		return cleanup(opError("sync replacement", err))
	}
	if err := file.Close(); err != nil {
		open = false
		return errors.Join(opError("close replacement", err), cleanupCreated(parent, tempLeaf, info))
	}
	open = false

	if err := verifyReplaceTarget(parent, targetLeaf); err != nil {
		return cleanup(opError("atomic replace", err))
	}
	if err := parent.Rename(tempLeaf, targetLeaf); err != nil {
		return cleanup(opError("atomic replace", err))
	}
	syncDirectory(parent)
	return nil
}

// Remove removes one regular, non-link file.
func (f *FS) Remove(rel string) (result error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.remove(rel)
}

func (f *FS) remove(rel string) (result error) {
	parts, err := validateRelative(rel)
	if err != nil {
		return opError("remove", err)
	}
	if slices.Contains(parts, lockName) {
		return opError("remove", errLockName)
	}
	parent, pinned, err := f.pinParent(parts[:len(parts)-1])
	if err != nil {
		return opError("remove", err)
	}
	defer func() {
		if closeErr := closePinned(pinned); closeErr != nil {
			result = errors.Join(result, opError("remove", closeErr))
		}
	}()
	leaf := parts[len(parts)-1]
	initial, err := parent.Lstat(leaf)
	if err != nil {
		return opError("remove", err)
	}
	if err := verifyRegular(initial); err != nil {
		return opError("remove", err)
	}
	second, err := parent.Lstat(leaf)
	if err != nil {
		return opError("remove", err)
	}
	if privatefs.IsLink(second) {
		return opError("remove", errLink)
	}
	if !os.SameFile(initial, second) {
		return opError("remove", errChanged)
	}
	return opError("remove", parent.Remove(leaf))
}

// ReadDir lists the non-link entries in a directory below the root. A rel of
// "." or "" lists the root itself.
func (f *FS) ReadDir(rel string) ([]os.DirEntry, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.readDir(rel)
}

func (f *FS) readDir(rel string) (entries []os.DirEntry, result error) {
	if rel == "" || rel == "." {
		dir, err := f.root.Open(".")
		if err != nil {
			return nil, opError("read dir", err)
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if readErr != nil {
			return nil, opError("read dir", readErr)
		}
		if closeErr != nil {
			return nil, opError("read dir", closeErr)
		}
		return rejectLinkEntries(entries)
	}
	parts, err := validateRelative(rel)
	if err != nil {
		return nil, opError("read dir", err)
	}
	if slices.Contains(parts, lockName) {
		return nil, opError("read dir", errLockName)
	}
	parent, pinned, err := f.pinParent(parts[:len(parts)-1])
	if err != nil {
		return nil, opError("read dir", err)
	}
	defer func() {
		if closeErr := closePinned(pinned); closeErr != nil {
			result = errors.Join(result, opError("read dir", closeErr))
		}
	}()
	leaf := parts[len(parts)-1]
	initial, err := parent.Lstat(leaf)
	if err != nil {
		return nil, opError("read dir", err)
	}
	if privatefs.IsLink(initial) {
		return nil, opError("read dir", errLink)
	}
	if !initial.IsDir() {
		return nil, opError("read dir", errWrongType)
	}
	dir, err := parent.Open(leaf)
	if err != nil {
		return nil, opError("read dir", err)
	}
	opened, statErr := dir.Stat()
	if statErr == nil {
		if privatefs.IsLink(opened) || !opened.IsDir() {
			statErr = errWrongType
		} else if !os.SameFile(initial, opened) {
			statErr = errChanged
		}
	}
	second, secondErr := parent.Lstat(leaf)
	if statErr == nil && secondErr != nil {
		statErr = secondErr
	}
	if statErr == nil {
		if privatefs.IsLink(second) {
			statErr = errLink
		} else if !os.SameFile(initial, second) {
			statErr = errChanged
		}
	}
	if statErr != nil {
		closeErr := dir.Close()
		return nil, errors.Join(opError("read dir", statErr), closeErr)
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return nil, opError("read dir", readErr)
	}
	if closeErr != nil {
		return nil, opError("read dir", closeErr)
	}
	return rejectLinkEntries(entries)
}

func rejectLinkEntries(entries []os.DirEntry) ([]os.DirEntry, error) {
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errLink
		}
	}
	return entries, nil
}

func (f *FS) acquireLock() (*flock.Flock, error) {
	for {
		info, err := f.root.Lstat(lockName)
		if err == nil {
			if err := verifyRegular(info); err != nil {
				return nil, opError("verify output lock", err)
			}
			lockPath := filepath.Join(f.rootAbs, lockName)
			if err := privatefs.Restrict(lockPath, false); err != nil {
				return nil, opError("restrict output lock", err)
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, opError("inspect output lock", err)
		}
		file, createErr := f.root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
		if createErr != nil {
			if os.IsExist(createErr) {
				continue
			}
			return nil, opError("create output lock", createErr)
		}
		lockErr := file.Chmod(privateFileMode)
		if lockErr == nil {
			created, statErr := file.Stat()
			if statErr != nil {
				lockErr = statErr
			} else if err := verifyRegular(created); err != nil {
				lockErr = err
			} else {
				current, lstatErr := f.root.Lstat(lockName)
				if lstatErr != nil {
					lockErr = lstatErr
				} else if privatefs.IsLink(current) || !os.SameFile(created, current) {
					lockErr = errChanged
				}
			}
		}
		closeErr := file.Close()
		if lockErr != nil {
			return nil, errors.Join(opError("create output lock", lockErr), closeErr)
		}
		if closeErr != nil {
			return nil, opError("close output lock", closeErr)
		}
		break
	}

	lock := flock.New(filepath.Join(f.rootAbs, lockName), flock.SetFlag(os.O_RDONLY), flock.SetPermissions(privateFileMode))
	ok, err := lock.TryLock()
	if err != nil {
		return lock, opError("lock output root", err)
	}
	if !ok {
		return lock, opError("lock output root", ErrLocked)
	}
	flockInfo, err := lock.Stat()
	if err != nil {
		return lock, opError("stat output lock", err)
	}
	rootInfo, exists, err := f.inspect(lockName)
	if err != nil {
		return lock, opError("verify output lock", err)
	}
	if !exists || !os.SameFile(flockInfo, rootInfo) {
		return lock, opError("verify output lock", errChanged)
	}
	return lock, nil
}

func (f *FS) ensureDirectories(parts []string) error {
	current := f.root
	var pinned []*os.Root
	for _, part := range parts {
		info, err := current.Lstat(part)
		if os.IsNotExist(err) {
			err = current.Mkdir(part, privateDirMode)
			if err != nil && !os.IsExist(err) {
				_ = closePinned(pinned)
				return err
			}
			if err == nil {
				// We created this directory. A restrictive umask may have
				// produced a mode without owner access, so set it exactly
				// before pinning it.
				if chmodErr := current.Chmod(part, privateDirMode); chmodErr != nil {
					_ = closePinned(pinned)
					return chmodErr
				}
			}
			info, err = current.Lstat(part)
		}
		if err != nil {
			_ = closePinned(pinned)
			return err
		}
		child, err := pinChild(current, part, info)
		if err != nil {
			_ = closePinned(pinned)
			return err
		}
		pinned = append(pinned, child)
		current = child
	}
	return closePinned(pinned)
}

func (f *FS) pinParent(parts []string) (*os.Root, []*os.Root, error) {
	current := f.root
	var pinned []*os.Root
	for _, part := range parts {
		info, err := current.Lstat(part)
		if err != nil {
			_ = closePinned(pinned)
			return nil, nil, err
		}
		child, err := pinChild(current, part, info)
		if err != nil {
			_ = closePinned(pinned)
			return nil, nil, err
		}
		pinned = append(pinned, child)
		current = child
	}
	return current, pinned, nil
}

func pinChild(current *os.Root, name string, initial os.FileInfo) (*os.Root, error) {
	if privatefs.IsLink(initial) {
		return nil, errLink
	}
	if !initial.IsDir() {
		return nil, errWrongType
	}
	child, err := current.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	childInfo, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if !os.SameFile(initial, childInfo) {
		_ = child.Close()
		return nil, errChanged
	}
	second, err := current.Lstat(name)
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if privatefs.IsLink(second) {
		_ = child.Close()
		return nil, errLink
	}
	if !os.SameFile(initial, second) {
		_ = child.Close()
		return nil, errChanged
	}
	return child, nil
}

func createSibling(parent *os.Root, prefix string) (*os.File, string, os.FileInfo, error) {
	for attempt := 0; attempt < maxTempAttempts; attempt++ {
		leaf := prefix + rand.Text()
		file, err := parent.OpenFile(leaf, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, "", nil, err
		}
		info, statErr := file.Stat()
		if statErr == nil {
			statErr = verifyRegular(info)
		}
		if statErr == nil {
			statErr = file.Chmod(privateFileMode)
		}
		if statErr == nil {
			info, statErr = file.Stat()
		}
		if statErr == nil {
			current, lstatErr := parent.Lstat(leaf)
			if lstatErr != nil {
				statErr = lstatErr
			} else if privatefs.IsLink(current) || !os.SameFile(info, current) {
				statErr = errChanged
			}
		}
		if statErr != nil {
			cleanupErr := failCreated(parent, leaf, file, info, statErr)
			return nil, "", nil, cleanupErr
		}
		return file, leaf, info, nil
	}
	return nil, "", nil, errTempCollision
}

func failCreated(parent *os.Root, leaf string, file *os.File, info os.FileInfo, reason error) error {
	if info == nil {
		info, _ = file.Stat()
	}
	closeErr := file.Close()
	return errors.Join(reason, closeErr, cleanupCreated(parent, leaf, info))
}

func cleanupCreated(parent *os.Root, leaf string, created os.FileInfo) error {
	if created == nil {
		return nil
	}
	current, err := parent.Lstat(leaf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if privatefs.IsLink(current) {
		return errLink
	}
	if !os.SameFile(created, current) {
		return errChanged
	}
	return parent.Remove(leaf)
}

func (f *FS) cleanupCreatedRelative(rel string, created os.FileInfo) error {
	parts, err := validateRelative(rel)
	if err != nil {
		return err
	}
	parent, pinned, err := f.pinParent(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	cleanupErr := cleanupCreated(parent, parts[len(parts)-1], created)
	return errors.Join(cleanupErr, closePinned(pinned))
}

func verifyReplaceTarget(parent *os.Root, leaf string) error {
	initial, err := parent.Lstat(leaf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := verifyRegular(initial); err != nil {
		return err
	}
	second, err := parent.Lstat(leaf)
	if err != nil {
		return err
	}
	if privatefs.IsLink(second) {
		return errLink
	}
	if !os.SameFile(initial, second) {
		return errChanged
	}
	return nil
}

func syncDirectory(parent *os.Root) {
	directory, err := parent.Open(".")
	if err != nil {
		return
	}
	_ = directory.Sync()
	_ = directory.Close()
}

func verifyRegular(info os.FileInfo) error {
	if privatefs.IsLink(info) || info == nil {
		if privatefs.IsLink(info) {
			return errLink
		}
		return errWrongType
	}
	if !info.Mode().IsRegular() {
		return errWrongType
	}
	return nil
}

func validateRelative(rel string) ([]string, error) {
	if rel == "" || rel == "." || strings.IndexByte(rel, 0) >= 0 || strings.ContainsAny(rel, `\\:`) || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return nil, errInvalidRel
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errInvalidRel
		}
	}
	return parts, nil
}

func validatePrefix(prefix string) error {
	if strings.IndexByte(prefix, 0) >= 0 || strings.ContainsAny(prefix, `/\:`) || prefix == "." || prefix == ".." {
		return errInvalidPrefix
	}
	return nil
}

func validateOutputRoot(abs string) error {
	if abs == "" || !filepath.IsAbs(abs) {
		return errInvalidRoot
	}
	volume := filepath.VolumeName(abs)
	volumeRoot := volume + string(filepath.Separator)
	if volume == "" {
		volumeRoot = string(filepath.Separator)
	}
	rel, err := filepath.Rel(volumeRoot, abs)
	if err != nil || rel == "." || rel == "" {
		return errInvalidRoot
	}
	leaf := filepath.Base(abs)
	if leaf == "" || leaf == "." || leaf == string(filepath.Separator) || leaf == volume {
		return errInvalidRoot
	}
	return nil
}

func verifyAbsoluteComponents(abs string) error {
	volume := filepath.VolumeName(abs)
	base := volume + string(filepath.Separator)
	if volume == "" {
		base = string(filepath.Separator)
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := base
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if privatefs.IsLink(info) {
			return errLink
		}
		if !info.IsDir() {
			return errWrongType
		}
	}
	return nil
}

func closePinned(pinned []*os.Root) error {
	var joined error
	for index := len(pinned) - 1; index >= 0; index-- {
		joined = errors.Join(joined, pinned[index].Close())
	}
	return joined
}

func openCleanup(root *os.Root, lock *flock.Flock, err error) error {
	if lock != nil {
		err = errors.Join(err, lock.Close())
	}
	if root != nil {
		err = errors.Join(err, root.Close())
	}
	return err
}

func joinRelative(dir, leaf string) string {
	if dir == "" {
		return leaf
	}
	return dir + "/" + leaf
}

func opError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return &os.PathError{Op: operation, Err: err}
}
