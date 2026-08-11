//go:build linux || darwin

package outputfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenRestrictsRootAndLockModes(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	path := filepath.Join(base, "output")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer f.Close()

	rootInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(root) error = %v", err)
	}
	if got := rootInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("root mode = %04o, want 0700", got)
	}
	lockInfo, err := os.Stat(filepath.Join(path, lockName))
	if err != nil {
		t.Fatalf("Stat(lock) error = %v", err)
	}
	if got := lockInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("lock mode = %04o, want 0600", got)
	}
}

func TestVolumeRootValidationDoesNotChangeFilesystemRootMode(t *testing.T) {
	root := string(filepath.Separator)
	before, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat(before) error = %v", err)
	}
	if err := validateOutputRoot(root); err == nil {
		t.Fatal("validateOutputRoot(volume root) succeeded")
	}
	after, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat(after) error = %v", err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("filesystem root mode changed from %04o to %04o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestRestrictiveUmaskNestedDirectoryCreation(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	old := syscall.Umask(0o777)
	defer syscall.Umask(old)

	if err := f.MkdirAll("a/b/c"); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for _, rel := range []string{"a", "a/b", "a/b/c"} {
		info, err := os.Stat(filepath.Join(f.rootAbs, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("Stat(%q) error = %v", rel, err)
		}
		if got := info.Mode().Perm(); got != 0700 {
			t.Fatalf("mode of %q = %04o, want 0700", rel, got)
		}
	}
}
