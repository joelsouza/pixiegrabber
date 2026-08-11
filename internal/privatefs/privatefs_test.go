package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenNewIsExclusiveAndDoesNotTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	first, err := OpenNew(path)
	if err != nil {
		t.Fatalf("OpenNew() error = %v", err)
	}
	if _, err := first.WriteString("keep me"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := OpenNew(path)
	if second != nil {
		_ = second.Close()
		t.Fatal("OpenNew() returned a file for an existing path")
	}
	if !os.IsExist(err) {
		t.Fatalf("OpenNew() error = %v, want an existing-path error", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(contents) != "keep me" {
		t.Fatalf("existing contents = %q, want %q", contents, "keep me")
	}
}

func TestCreateTempUsesPatternAndUniqueNames(t *testing.T) {
	dir := t.TempDir()
	first, err := CreateTemp(dir, "snapshot-*.json")
	if err != nil {
		t.Fatalf("first CreateTemp() error = %v", err)
	}
	firstName := first.Name()
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	second, err := CreateTemp(dir, "snapshot-*.json")
	if err != nil {
		t.Fatalf("second CreateTemp() error = %v", err)
	}
	secondName := second.Name()
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if firstName == secondName {
		t.Fatalf("CreateTemp() reused name %q", firstName)
	}
	for _, name := range []string{firstName, secondName} {
		base := filepath.Base(name)
		if !strings.HasPrefix(base, "snapshot-") || !strings.HasSuffix(base, ".json") {
			t.Fatalf("temp name %q does not preserve pattern", base)
		}
	}

	if _, err := CreateTemp(dir, "bad/name"); err == nil {
		t.Fatal("CreateTemp() accepted a separator in pattern")
	}
	if _, err := CreateTemp(dir, "bad\x00name"); err == nil {
		t.Fatal("CreateTemp() accepted NUL in pattern")
	}
}

func TestMkdirAndMkdirTempArePrivateDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private")
	if err := Mkdir(path); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := Mkdir(path); !os.IsExist(err) {
		t.Fatalf("second Mkdir() error = %v, want an existing-path error", err)
	}

	temp, err := MkdirTemp(dir, "staging-*")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	if temp == path || !strings.HasPrefix(filepath.Base(temp), "staging-") {
		t.Fatalf("MkdirTemp() path = %q", temp)
	}
	info, err := os.Stat(temp)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("MkdirTemp() created a non-directory")
	}
}

func TestMkdirAllCreatesMissingComponentsWithoutChangingAncestors(t *testing.T) {
	root := testRealTempDir(t)
	ancestor := filepath.Join(root, "existing")
	if err := os.Mkdir(ancestor, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	wantAncestorMode := os.FileMode(0755)
	path := filepath.Join(ancestor, "one", "two")
	if err := MkdirAll(path); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := MkdirAll(path); err != nil {
		t.Fatalf("second MkdirAll() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("MkdirAll() created a non-directory")
	}
	info, err = os.Stat(ancestor)
	if err != nil {
		t.Fatalf("ancestor Stat() error = %v", err)
	}
	if info.Mode().Perm() != wantAncestorMode {
		t.Fatalf("ancestor mode = %04o, want %04o", info.Mode().Perm(), wantAncestorMode)
	}
}

func TestMkdirAllRejectsWrongTypesAndLinks(t *testing.T) {
	root := testRealTempDir(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := MkdirAll(filepath.Join(file, "child")); err == nil {
		t.Fatal("MkdirAll() accepted a file as a directory")
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || !IsLink(info) {
		t.Fatalf("IsLink() = %v, %v; want true", info, err)
	}
	if err := MkdirAll(filepath.Join(link, "child")); err == nil {
		t.Fatal("MkdirAll() accepted a link component")
	}
}

func TestRestrictValidatesTypeAndRejectsLinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := Restrict(file, true); err == nil {
		t.Fatal("Restrict() accepted a file as a directory")
	}
	if err := Restrict(root, false); err == nil {
		t.Fatal("Restrict() accepted a directory as a file")
	}
	if err := Restrict(file, false); err != nil {
		t.Fatalf("Restrict(file) error = %v", err)
	}
	link := filepath.Join(root, "file-link")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := Restrict(link, false); err == nil {
		t.Fatal("Restrict() accepted a link")
	}
}

func TestMkdirAllRejectsEmptyPath(t *testing.T) {
	if err := MkdirAll(""); err == nil || errors.Is(err, os.ErrExist) {
		t.Fatalf("MkdirAll(\"\") error = %v, want a path error", err)
	}
}

func testRealTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	return path
}
