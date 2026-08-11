//go:build linux || darwin

package privatefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnixCreationModesAreOwnerOnly(t *testing.T) {
	root := testRealTempDir(t)

	file, err := OpenNew(filepath.Join(root, "new-file"))
	if err != nil {
		t.Fatalf("OpenNew() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("OpenNew Close() error = %v", err)
	}
	temp, err := CreateTemp(root, "file-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	if err := temp.Close(); err != nil {
		t.Fatalf("CreateTemp Close() error = %v", err)
	}
	for _, path := range []string{filepath.Join(root, "new-file"), temp.Name()} {
		assertMode(t, path, 0600)
	}

	if err := Mkdir(filepath.Join(root, "new-dir")); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	tempDir, err := MkdirTemp(root, "dir-*")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	if err := MkdirAll(filepath.Join(root, "one", "two")); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for _, path := range []string{filepath.Join(root, "new-dir"), tempDir, filepath.Join(root, "one"), filepath.Join(root, "one", "two")} {
		assertMode(t, path, 0700)
	}

	if err := os.Chmod(filepath.Join(root, "new-dir"), 0755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	if err := Restrict(filepath.Join(root, "new-dir"), true); err != nil {
		t.Fatalf("Restrict() error = %v", err)
	}
	assertMode(t, filepath.Join(root, "new-dir"), 0700)
}

func TestUnixRestrictRejectsFinalLinkWithoutChangingTarget(t *testing.T) {
	root := testRealTempDir(t)
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := Restrict(link, false); err == nil {
		t.Fatal("Restrict() accepted a final symlink")
	}
	assertMode(t, target, 0644)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %q = %04o, want %04o", path, got, want)
	}
}
