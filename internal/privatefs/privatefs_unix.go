//go:build linux || darwin

package privatefs

import (
	"os"
	"path/filepath"
	"slices"
)

// CreateTemp creates a new owner-only temporary regular file in dir. The last
// '*' in pattern is replaced as in os.CreateTemp. An empty dir uses the
// operating system temporary directory.
func CreateTemp(dir, pattern string) (*os.File, error) {
	if err := validatePath(dir); err != nil {
		return nil, wrap("create temp", err)
	}
	if err := validatePattern(pattern); err != nil {
		return nil, wrap("create temp", err)
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, wrap("create temp", err)
	}
	if err := file.Chmod(privateFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, wrap("restrict temp file", err)
	}
	return file, nil
}

// Mkdir creates one new owner-only directory. It returns an existing-path
// error if path already exists and never changes an existing directory.
func Mkdir(path string) error {
	if err := validatePath(path); err != nil {
		return wrap("mkdir", err)
	}
	if err := os.Mkdir(path, privateDirMode); err != nil {
		if os.IsExist(err) {
			return wrap("mkdir", os.ErrExist)
		}
		return wrap("mkdir", err)
	}
	if err := restrictCreatedDirectory(path); err != nil {
		return wrap("restrict directory", err)
	}
	return nil
}

// MkdirAll creates missing owner-only directories in path. Existing
// components must be real directories, and their permissions are unchanged.
func MkdirAll(path string) error {
	if path == "" {
		return wrap("mkdir all", os.ErrNotExist)
	}
	if err := validatePath(path); err != nil {
		return wrap("mkdir all", err)
	}
	for _, component := range pathComponents(path) {
		if err := ensureUnixDirectory(component); err != nil {
			return wrap("mkdir all", err)
		}
	}
	return nil
}

// MkdirTemp creates a new owner-only temporary directory in dir. The last
// '*' in pattern is replaced as in os.MkdirTemp. An empty dir uses the
// operating system temporary directory.
func MkdirTemp(dir, pattern string) (string, error) {
	if err := validatePath(dir); err != nil {
		return "", wrap("mkdir temp", err)
	}
	if err := validatePattern(pattern); err != nil {
		return "", wrap("mkdir temp", err)
	}
	path, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", wrap("mkdir temp", err)
	}
	if err := restrictCreatedDirectory(path); err != nil {
		return "", wrap("restrict temp directory", err)
	}
	return path, nil
}

// OpenNew creates an exclusive owner-only read/write regular file. It never
// truncates an existing path.
func OpenNew(path string) (*os.File, error) {
	if err := validatePath(path); err != nil {
		return nil, wrap("open new", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return nil, wrap("open new", err)
	}
	if err := file.Chmod(privateFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, wrap("restrict new file", err)
	}
	return file, nil
}

// Restrict applies owner-only permissions to an existing regular file or
// directory. It rejects links and objects with a type different from
// directory.
func Restrict(path string, directory bool) error {
	if err := validatePath(path); err != nil {
		return wrap("restrict", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return wrap("restrict", err)
	}
	if IsLink(info) {
		return wrap("restrict", errLink)
	}
	if err := verifyUnixObject(info, directory); err != nil {
		return wrap("restrict", err)
	}
	file, err := openPrivate(path, directory)
	if err != nil {
		return wrap("restrict", err)
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return wrap("restrict", err)
	}
	if !os.SameFile(info, actual) {
		return wrap("restrict", errChanged)
	}
	if err := verifyUnixObject(actual, directory); err != nil {
		return wrap("restrict", err)
	}
	if err := file.Chmod(modeFor(directory)); err != nil {
		return wrap("restrict", err)
	}
	return nil
}

// IsLink reports whether info describes a symbolic link.
func IsLink(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink != 0
}

func modeFor(directory bool) os.FileMode {
	if directory {
		return privateDirMode
	}
	return privateFileMode
}

func ensureUnixDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		return verifyUnixDirectory(info)
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(path, privateDirMode); err != nil {
		if os.IsExist(err) {
			return ensureUnixDirectory(path)
		}
		return err
	}
	if err := restrictCreatedDirectory(path); err != nil {
		return err
	}
	return nil
}

func verifyUnixDirectory(info os.FileInfo) error {
	if IsLink(info) {
		return errLink
	}
	if !info.IsDir() {
		return errWrongType
	}
	return nil
}

func verifyUnixObject(info os.FileInfo, directory bool) error {
	if IsLink(info) {
		return errLink
	}
	if directory {
		if !info.IsDir() {
			return errWrongType
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return errWrongType
	}
	return nil
}

// restrictCreatedDirectory uses the directory handle for chmod and removes
// the directory only when its identity is still the identity created here.
// The no-follow open protects the final component; a parent substitution
// between Lstat and open remains a platform limitation without openat-style
// traversal.
func restrictCreatedDirectory(path string) error {
	created, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := verifyUnixDirectory(created); err != nil {
		cleanupCreatedDirectory(path, created)
		return err
	}
	file, err := openPrivate(path, true)
	if err != nil {
		cleanupCreatedDirectory(path, created)
		return err
	}
	actual, statErr := file.Stat()
	if statErr == nil && !os.SameFile(created, actual) {
		statErr = errChanged
	}
	if statErr == nil {
		statErr = verifyUnixDirectory(actual)
	}
	if statErr == nil {
		statErr = file.Chmod(privateDirMode)
	}
	closeErr := file.Close()
	if statErr == nil {
		statErr = closeErr
	}
	if statErr != nil {
		cleanupCreatedDirectory(path, created)
	}
	return statErr
}

func cleanupCreatedDirectory(path string, created os.FileInfo) {
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(created, current) {
		_ = os.Remove(path)
	}
}

func pathComponents(path string) []string {
	clean := filepath.Clean(path)
	var components []string
	for {
		components = append(components, clean)
		parent := filepath.Dir(clean)
		if parent == clean {
			break
		}
		clean = parent
	}
	slices.Reverse(components)
	return components
}
