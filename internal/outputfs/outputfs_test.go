package outputfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}

func TestValidateOutputRootRejectsVolumeRoot(t *testing.T) {
	base, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	volumeRoot := filepath.VolumeName(base)
	if volumeRoot == "" {
		volumeRoot = string(filepath.Separator)
	} else {
		volumeRoot += string(filepath.Separator)
	}
	if err := validateOutputRoot(filepath.Clean(volumeRoot)); err == nil {
		t.Fatalf("validateOutputRoot(%q) succeeded", volumeRoot)
	}
}

func TestRelativeNamesRejectUnsafeValues(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	unsafe := []string{"", ".", "./file", "dir/.", "dir/..", "../file", "/file", "C:/file", "dir\\file", "dir:file", "dir//file", "bad\x00file"}
	for _, rel := range unsafe {
		err := func() error {
			_, err := f.DisplayPath(rel)
			return err
		}()
		if err == nil {
			t.Errorf("DisplayPath(%q) accepted an unsafe name", rel)
		}
		if rel != "" && strings.Contains(err.Error(), rel) {
			t.Errorf("error for %q exposed the unsafe name: %v", rel, err)
		}
	}
}

func TestDisplayPathIsOnlyAJoinedDisplayName(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	got, err := f.DisplayPath("set/reference.jpg")
	if err != nil {
		t.Fatalf("DisplayPath() error = %v", err)
	}
	want := filepath.Join(f.rootAbs, "set", "reference.jpg")
	if got != want {
		t.Fatalf("DisplayPath() = %q, want %q", got, want)
	}
}

func TestPersistentLockAndSecondOpen(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	f, err := Open(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	lockPath := filepath.Join(dir, "output", ".pixiegrabber.lock")
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("Lstat(lock) error = %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("lock mode = %v, want a regular file", info.Mode())
	}

	second, err := Open(filepath.Join(dir, "output"))
	if second != nil {
		_ = second.Close()
		t.Fatal("second Open() returned an FS")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open() error = %v, want ErrLocked", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock after Close() error = %v", err)
	}

	third, err := Open(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatalf("Open() after Close() error = %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("third Close() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close() on first FS error = %v", err)
	}
}

func TestOpenRejectsSymlinkOutputRootWithoutChangingTarget(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatalf("Mkdir(target) error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(before) error = %v", err)
	}
	f, err := Open(link)
	if f != nil {
		_ = f.Close()
		t.Fatal("Open() accepted a symlink output root")
	}
	if err == nil {
		t.Fatal("Open() accepted a symlink output root without an FS")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(after) error = %v", err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("target mode changed from %04o to %04o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestOpenRejectsSymlinkAndDirectoryLockPaths(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	for _, test := range []struct {
		name string
		make func(string) error
	}{
		{name: "symlink", make: func(path string) error {
			target := path + "-target"
			if err := os.WriteFile(target, []byte("lock target"), 0600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
		{name: "directory", make: func(path string) error { return os.Mkdir(path, 0700) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := filepath.Join(base, test.name)
			if err := os.Mkdir(rootPath, 0700); err != nil {
				t.Fatalf("Mkdir(root) error = %v", err)
			}
			if err := test.make(filepath.Join(rootPath, lockName)); err != nil {
				if test.name == "symlink" {
					t.Skipf("symlinks are not available: %v", err)
				}
				t.Fatalf("create lock path error = %v", err)
			}
			f, err := Open(rootPath)
			if f != nil {
				_ = f.Close()
				t.Fatal("Open() accepted an invalid lock path")
			}
			if err == nil {
				t.Fatal("Open() accepted an invalid lock path")
			}
		})
	}
}

func TestMkdirAllAndInspect(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	if err := f.MkdirAll("one/two"); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	info, exists, err := f.Inspect("one/two")
	if err != nil {
		t.Fatalf("Inspect(directory) error = %v", err)
	}
	if !exists || info == nil || !info.IsDir() {
		t.Fatalf("Inspect(directory) = %v, %v, want an existing directory", info, exists)
	}

	filePath := f.DisplayPathForTest(t, "one/file")
	if err := os.WriteFile(filePath, []byte("contents"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	info, exists, err = f.Inspect("one/file")
	if err != nil {
		t.Fatalf("Inspect(file) error = %v", err)
	}
	if !exists || info == nil || !info.Mode().IsRegular() {
		t.Fatalf("Inspect(file) = %v, %v, want an existing regular file", info, exists)
	}

	info, exists, err = f.Inspect("missing")
	if err != nil || exists || info != nil {
		t.Fatalf("Inspect(missing) = %v, %v, %v, want nil, false, nil", info, exists, err)
	}
	info, exists, err = f.Inspect("missing-parent/file")
	if err != nil || exists || info != nil {
		t.Fatalf("Inspect(missing parent) = %v, %v, %v, want nil, false, nil", info, exists, err)
	}
}

func TestCleanupCreatedRejectsLinksAndSubstitutedEntries(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	linkPath := f.DisplayPathForTest(t, "link-temp")
	linkTarget := f.DisplayPathForTest(t, "link-target")
	if err := os.WriteFile(linkPath, []byte("created"), 0600); err != nil {
		t.Fatalf("WriteFile(link) error = %v", err)
	}
	createdInfo, err := os.Stat(linkPath)
	if err != nil {
		t.Fatalf("Stat(link) error = %v", err)
	}
	if err := os.WriteFile(linkTarget, []byte("target"), 0600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatalf("Remove(link source) error = %v", err)
	}
	if err := os.Symlink(linkTarget, linkPath); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := cleanupCreated(f.root, "link-temp", createdInfo); !errors.Is(err, errLink) {
		t.Fatalf("cleanupCreated(link) error = %v, want errLink", err)
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("link after cleanup error = %v", err)
	}

	oldPath := f.DisplayPathForTest(t, "substituted")
	newPath := f.DisplayPathForTest(t, "substituted-new")
	if err := os.WriteFile(oldPath, []byte("old"), 0600); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		t.Fatalf("Stat(old) error = %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0600); err != nil {
		t.Fatalf("WriteFile(new) error = %v", err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("Remove(old) error = %v", err)
	}
	if err := os.Rename(newPath, oldPath); err != nil {
		t.Fatalf("Rename(new) error = %v", err)
	}
	if err := cleanupCreated(f.root, "substituted", oldInfo); !errors.Is(err, errChanged) {
		t.Fatalf("cleanupCreated(substituted) error = %v, want errChanged", err)
	}
	data, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("ReadFile(substituted) error = %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("substituted contents = %q, want new", data)
	}
}

func TestOpenRegularValidatesTypeAndLinks(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	path := f.DisplayPathForTest(t, "regular")
	if err := os.WriteFile(path, []byte("read me"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	file, err := f.OpenRegular("regular")
	if err != nil {
		t.Fatalf("OpenRegular() error = %v", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file Close() error = %v", err)
	}
	if string(data) != "read me" {
		t.Fatalf("OpenRegular() contents = %q", data)
	}

	if err := f.MkdirAll("directory"); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if _, err := f.OpenRegular("directory"); err == nil {
		t.Fatal("OpenRegular() accepted a directory")
	}

	linkPath := f.DisplayPathForTest(t, "link")
	if err := os.Symlink(path, linkPath); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if _, err := f.OpenRegular("link"); err == nil {
		t.Fatal("OpenRegular() accepted a symlink")
	}
}

func TestIntermediateLinksAreRejected(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatalf("Mkdir(target) error = %v", err)
	}
	linkPath := f.DisplayPathForTest(t, "link")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := f.MkdirAll("link/child"); err == nil {
		t.Fatal("MkdirAll() accepted an intermediate symlink")
	}
}

func TestTempFileUsesRootRelativeNameAndPrivateMode(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	file, name, err := f.TempFile("temp/nested", "snapshot-")
	if err != nil {
		t.Fatalf("TempFile() error = %v", err)
	}
	if !strings.HasPrefix(filepath.ToSlash(name), "temp/nested/snapshot-") || strings.Contains(name, string(filepath.Separator)+string(filepath.Separator)) {
		t.Fatalf("TempFile() name = %q, want a root-relative sibling", name)
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `\\:`) {
		t.Fatalf("TempFile() returned unsafe name %q", name)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("temp Stat() error = %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("temp mode = %v, want regular", info.Mode())
	}
	if err := file.Close(); err != nil {
		t.Fatalf("temp Close() error = %v", err)
	}
}

func TestAtomicReplaceSuccessAndFailureCleanup(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	target := f.DisplayPathForTest(t, "atomic/file.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := f.AtomicReplace("atomic/file.txt", func(w io.Writer) error {
		_, err := io.WriteString(w, "new")
		return err
	}); err != nil {
		t.Fatalf("AtomicReplace(success) error = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(success) error = %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("replaced contents = %q, want %q", data, "new")
	}

	callbackErr := errors.New("callback failed")
	if err := f.AtomicReplace("atomic/file.txt", func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return callbackErr
	}); err == nil || !errors.Is(err, callbackErr) || !strings.Contains(err.Error(), "write replacement") {
		t.Fatalf("AtomicReplace(callback failure) error = %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(after failure) error = %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("contents after callback failure = %q, want %q", data, "new")
	}

	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "file.txt" {
		t.Fatalf("temporary files remain after failure: %v", entries)
	}
}

func TestAtomicReplaceRejectsWrongTargetAndLink(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	if err := f.MkdirAll("targets/directory"); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := f.AtomicReplace("targets/directory", func(w io.Writer) error {
		_, err := io.WriteString(w, "no")
		return err
	}); err == nil {
		t.Fatal("AtomicReplace() accepted a directory target")
	}

	target := f.DisplayPathForTest(t, "targets/real")
	link := f.DisplayPathForTest(t, "targets/link")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := f.AtomicReplace("targets/link", func(w io.Writer) error {
		_, err := io.WriteString(w, "no")
		return err
	}); err == nil {
		t.Fatal("AtomicReplace() accepted a symlink target")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(real target) error = %v", err)
	}
	if string(data) != "keep" {
		t.Fatalf("real target contents = %q, want %q", data, "keep")
	}
}

func TestRemoveOnlyRemovesRegularFiles(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	filePath := f.DisplayPathForTest(t, "file")
	if err := os.WriteFile(filePath, []byte("remove"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := f.Remove("file"); err != nil {
		t.Fatalf("Remove(file) error = %v", err)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("file after Remove() error = %v, want not-exist", err)
	}
	if err := f.Remove("file"); !os.IsNotExist(err) {
		t.Fatalf("second Remove() error = %v, want not-exist", err)
	}

	if err := f.MkdirAll("directory"); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := f.Remove("directory"); err == nil {
		t.Fatal("Remove() removed a directory")
	}
}

func TestMutationsRejectLockName(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	write := func(w io.Writer) error { return nil }

	if err := f.MkdirAll(lockName); !errors.Is(err, errLockName) {
		t.Errorf("MkdirAll(lockName) error = %v, want errLockName", err)
	}
	if err := f.MkdirAll("dir/" + lockName); !errors.Is(err, errLockName) {
		t.Errorf("MkdirAll(dir/lockName) error = %v, want errLockName", err)
	}

	if err := f.AtomicReplace(lockName, write); !errors.Is(err, errLockName) {
		t.Errorf("AtomicReplace(lockName) error = %v, want errLockName", err)
	}
	if err := f.AtomicReplace("dir/"+lockName, write); !errors.Is(err, errLockName) {
		t.Errorf("AtomicReplace(dir/lockName) error = %v, want errLockName", err)
	}

	if err := f.Remove(lockName); !errors.Is(err, errLockName) {
		t.Errorf("Remove(lockName) error = %v, want errLockName", err)
	}
	if err := f.Remove("dir/" + lockName); !errors.Is(err, errLockName) {
		t.Errorf("Remove(dir/lockName) error = %v, want errLockName", err)
	}

	if _, _, err := f.TempFile(lockName, "prefix-"); !errors.Is(err, errLockName) {
		t.Errorf("TempFile(lockName) error = %v, want errLockName", err)
	}
	if _, _, err := f.TempFile("dir/"+lockName, "prefix-"); !errors.Is(err, errLockName) {
		t.Errorf("TempFile(dir/lockName) error = %v, want errLockName", err)
	}
}

func TestReadDirListsRootAndNestedAndRejectsLinksFilesAndLockName(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	rootEntries, err := f.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(root) error = %v", err)
	}
	foundLock := false
	for _, entry := range rootEntries {
		if entry.Name() == lockName {
			foundLock = true
		}
	}
	if !foundLock {
		t.Fatal("root listing did not include the lock file")
	}

	if err := f.MkdirAll("one/two"); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	filePath := f.DisplayPathForTest(t, "one/file.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	nested, err := f.ReadDir("one")
	if err != nil {
		t.Fatalf("ReadDir(nested) error = %v", err)
	}
	names := map[string]bool{}
	for _, entry := range nested {
		names[entry.Name()] = true
	}
	if !names["two"] || !names["file.txt"] {
		t.Fatalf("nested listing = %#v", names)
	}

	linkPath := f.DisplayPathForTest(t, "one/link")
	if err := os.Symlink(filePath, linkPath); err != nil {
		t.Skipf("symlinks are not supported: %v", err)
	}
	if _, err := f.ReadDir("one"); !errors.Is(err, errLink) {
		t.Fatalf("ReadDir with symlink entry error = %v, want errLink", err)
	}

	if _, err := f.ReadDir("one/file.txt"); !errors.Is(err, errWrongType) {
		t.Fatalf("ReadDir(file) error = %v, want errWrongType", err)
	}

	if _, err := f.ReadDir(lockName); !errors.Is(err, errLockName) {
		t.Fatalf("ReadDir(lockName) error = %v, want errLockName", err)
	}
}

func TestCloseWaitsForBlockedOperation(t *testing.T) {
	f := openTestFS(t)

	started := make(chan struct{})
	release := make(chan struct{})
	opDone := make(chan error, 1)
	go func() {
		opDone <- f.AtomicReplace("blocked/file.txt", func(w io.Writer) error {
			close(started)
			<-release
			_, err := io.WriteString(w, "data")
			return err
		})
	}()
	<-started

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- f.Close()
	}()

	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before the blocked operation completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-opDone; err != nil {
		t.Fatalf("AtomicReplace() error = %v", err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return after the operation completed")
	}
}

func TestAtomicReplaceMaximumLengthTarget(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	leaf := strings.Repeat("a", 255)
	rel := "max/" + leaf
	if err := f.AtomicReplace(rel, func(w io.Writer) error {
		_, err := io.WriteString(w, "max")
		return err
	}); err != nil {
		t.Fatalf("AtomicReplace(max-length) error = %v", err)
	}
	data, err := os.ReadFile(f.DisplayPathForTest(t, rel))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "max" {
		t.Fatalf("contents = %q, want %q", data, "max")
	}
}

func TestAtomicReplaceRecoversCallbackPanic(t *testing.T) {
	f := openTestFS(t)
	defer f.Close()

	target := f.DisplayPathForTest(t, "panic/file.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := f.AtomicReplace("panic/file.txt", func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		panic("callback boom")
	})
	if err == nil {
		t.Fatal("AtomicReplace() recovered a panic but returned nil")
	}
	if !strings.Contains(err.Error(), "callback panic") {
		t.Fatalf("AtomicReplace() error = %v, want a callback panic error", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "keep" {
		t.Fatalf("target contents = %q, want %q", data, "keep")
	}

	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), atomicTempPrefix) {
			t.Fatalf("temporary file left behind: %v", entry.Name())
		}
	}
}

type failingLock struct {
	calls int
}

func (l *failingLock) Close() error {
	l.calls++
	if l.calls == 1 {
		return errors.New("flock close failed")
	}
	return nil
}

func TestCloseRetriesAfterFlockFailure(t *testing.T) {
	fl := &failingLock{}
	f := &FS{lock: fl}

	if err := f.Close(); err == nil {
		t.Fatal("first Close() succeeded, want an error")
	}
	if fl.calls != 1 {
		t.Fatalf("first Close() calls = %d, want 1", fl.calls)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}
	if fl.calls != 2 {
		t.Fatalf("second Close() calls = %d, want 2", fl.calls)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("third Close() error = %v, want nil", err)
	}
	if fl.calls != 2 {
		t.Fatalf("third Close() calls = %d, want 2 (idempotent)", fl.calls)
	}
}

func openTestFS(t *testing.T) *FS {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	f, err := Open(filepath.Join(base, "output"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("cleanup Close() error = %v", err)
		}
	})
	return f
}

func (f *FS) DisplayPathForTest(t *testing.T, rel string) string {
	t.Helper()
	path, err := f.DisplayPath(rel)
	if err != nil {
		t.Fatalf("DisplayPath(%q) error = %v", rel, err)
	}
	return path
}
