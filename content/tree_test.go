package content

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCaptureTreeRoundTripIsPortableAndDeterministic(t *testing.T) {
	source := makePortableTree(t)
	store := NewMemory()
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := repository.Entries(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := []struct {
		segments []string
		kind     string
		target   string
	}{
		{[]string{"bin"}, "directory", ""},
		{[]string{"bin", "tool"}, "executable", ""},
		{[]string{"docs"}, "directory", ""},
		{[]string{"docs", "report.txt"}, "regular", ""},
		{[]string{"empty"}, "directory", ""},
		{[]string{"links"}, "directory", ""},
		{[]string{"links", "report"}, "symlink", "../docs/report.txt"},
	}
	if len(entries) != len(wantEntries) {
		t.Fatalf("Entries length = %d, want %d", len(entries), len(wantEntries))
	}
	for i, want := range wantEntries {
		if !slices.Equal(entries[i].Segments(), want.segments) || entries[i].Kind() != want.kind || entries[i].Target() != want.target {
			t.Fatalf("entry %d = (%q, %q, %q), want (%q, %q, %q)", i, entries[i].Segments(), entries[i].Kind(), entries[i].Target(), want.segments, want.kind, want.target)
		}
		if (want.kind == "regular" || want.kind == "executable") != entries[i].Content().Valid() {
			t.Fatalf("entry %d content validity = %v for kind %q", i, entries[i].Content().Valid(), want.kind)
		}
	}
	segments := entries[0].Segments()
	segments[0] = "mutated"
	if entries[0].Segments()[0] != "bin" {
		t.Fatal("TreeEntry.Segments exposed mutable internal storage")
	}

	old := time.Unix(1, 0)
	if err := filepath.Walk(source, func(name string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chtimes(name, old, old)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "docs", "report.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "bin", "tool"), 0o711); err != nil {
		t.Fatal(err)
	}
	repository.treeFS = reversingTreeFilesystem{treeFilesystem: repository.treeFilesystem()}
	metadataChanged, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Equal(metadataChanged) {
		t.Fatal("timestamps, non-executable mode bits, or enumeration order changed tree identity")
	}

	if err := os.WriteFile(filepath.Join(source, "docs", "report.txt"), []byte("changed semantic bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Equal(changed) {
		t.Fatal("changed file bytes did not change tree identity")
	}
}

func TestCaptureTreePreservesOpaqueByteNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames must be valid Unicode")
	}
	source := t.TempDir()
	name := "raw-\n"
	if runtime.GOOS == "linux" {
		name = string([]byte{'r', 'a', 'w', '-', 0xff})
	}
	if err := os.WriteFile(filepath.Join(source, name), []byte("opaque"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := repository.Entries(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Segments()) != 1 || entries[0].Segments()[0] != name {
		t.Fatalf("opaque entry = %#v, want exact bytes %v", entries, []byte(name))
	}
	destination := filepath.Join(t.TempDir(), "materialized")
	if err := repository.MaterializeTree(context.Background(), tree, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "opaque" {
		t.Fatalf("opaque file bytes = %q", got)
	}
}

func TestCaptureTreeRejectsInvalidSymlinks(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"absolute", func(t *testing.T, root string) { mustSymlink(t, "/outside", filepath.Join(root, "link")) }},
		{"portable Windows absolute", func(t *testing.T, root string) {
			target := `C:\outside`
			if err := os.WriteFile(filepath.Join(root, target), []byte("same bytes, unsafe target syntax"), 0o600); err != nil {
				t.Skipf("host cannot create portable absolute-target fixture: %v", err)
			}
			mustSymlink(t, target, filepath.Join(root, "link"))
		}},
		{"lexical root escape", func(t *testing.T, root string) { mustSymlink(t, "../outside", filepath.Join(root, "link")) }},
		{"dangling", func(t *testing.T, root string) { mustSymlink(t, "missing", filepath.Join(root, "link")) }},
		{"file with directory suffix", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			mustSymlink(t, "file/", filepath.Join(root, "link"))
		}},
		{"escaping chain", func(t *testing.T, root string) {
			mustSymlink(t, "second", filepath.Join(root, "first"))
			mustSymlink(t, "../outside", filepath.Join(root, "second"))
		}},
		{"cycle", func(t *testing.T, root string) {
			mustSymlink(t, "second", filepath.Join(root, "first"))
			mustSymlink(t, "first", filepath.Join(root, "second"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			test.setup(t, source)
			repository, err := NewRepository(NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.CaptureTree(context.Background(), source); err == nil {
				t.Fatalf("CaptureTree accepted %s symlink", test.name)
			}
		})
	}
}

func TestCaptureTreeAcceptsResolvedSymlinkChains(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "dir", "file"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "dir", filepath.Join(source, "alias"))
	mustSymlink(t, "alias/../alias/file", filepath.Join(source, "report"))
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repository.CaptureTree(context.Background(), source); err != nil {
		t.Fatalf("CaptureTree rejected a fully resolved symlink chain: %v", err)
	}
}

func TestCaptureTreeRejectsUnsupportedEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix special-file test")
	}
	t.Run("fifo", func(t *testing.T) {
		source := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
			t.Skipf("FIFO creation unavailable: %v", err)
		}
		assertCaptureRejected(t, source)
	})
	t.Run("socket", func(t *testing.T) {
		source := t.TempDir()
		listener, err := net.Listen("unix", filepath.Join(source, "socket"))
		if err != nil {
			t.Skipf("Unix socket creation unavailable: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		assertCaptureRejected(t, source)
	})
}

func TestCaptureTreeReportsUnreadableAndMidWalkFailures(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(source, "unreadable")
	if err := os.WriteFile(unreadable, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		fs   func(treeFilesystem) treeFilesystem
		want error
	}{
		{"unreadable entry", func(base treeFilesystem) treeFilesystem {
			return failingOpenTreeFilesystem{treeFilesystem: base, name: unreadable, err: errTreeUnreadable}
		}, errTreeUnreadable},
		{"mid-walk", func(base treeFilesystem) treeFilesystem {
			return failingReadDirTreeFilesystem{treeFilesystem: base, name: filepath.Join(source, "nested"), err: errTreeMidWalk}
		}, errTreeMidWalk},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, err := NewRepository(NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			repository.treeFS = test.fs(repository.treeFilesystem())
			_, err = repository.CaptureTree(context.Background(), source)
			if !errors.Is(err, test.want) {
				t.Fatalf("CaptureTree error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCaptureTreeHonorsCancellationDuringWalk(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	repository.treeFS = cancelingTreeFilesystem{treeFilesystem: repository.treeFilesystem(), cancel: cancel}

	_, err = repository.CaptureTree(ctx, source)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CaptureTree error = %v, want context.Canceled", err)
	}
}

func TestCaptureTreeRejectsDirectoryReplacedBySymlinkDuringWalk(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside-file"), []byte("must not be followed"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	repository.treeFS = replacingDirectoryTreeFilesystem{
		treeFilesystem: repository.treeFilesystem(),
		directory:      nested,
		target:         outside,
	}

	if _, err := repository.CaptureTree(context.Background(), source); err == nil {
		t.Fatal("CaptureTree followed a directory replaced by a symlink")
	}
}

func TestTreeManifestHasExactPrivateHeader(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var manifest bytes.Buffer
	if _, err := repository.store.Copy(context.Background(), tree.Digest(), &manifest); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(manifest.Bytes(), []byte("dawn.tree/1")) {
		t.Fatalf("manifest header = %q, want exact dawn.tree/1", manifest.Bytes())
	}
}

func TestTreeManifestProcessingHandlesDeepSymlinkGraphsIteratively(t *testing.T) {
	const depth = 12000
	entries := make([]treeEntry, 0, depth+1)
	contentObject := putTestObject(t, NewMemory(), []byte("end"))
	entries = append(entries, treeEntry{segments: []string{"end"}, kind: fileEntry, content: contentObject})
	for i := 0; i < depth; i++ {
		entries = append(entries, treeEntry{segments: []string{deepTreeName(i)}, kind: symlinkEntry, target: deepTreeName(i + 1)})
	}
	entries[len(entries)-1].target = "end"
	slices.SortFunc(entries, func(a, b treeEntry) int { return compareTreeSegments(a.segments, b.segments) })
	encoded, err := encodeTreeManifest(entries)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemory()
	manifest := putTestObject(t, store, encoded)
	tree, err := NewTree(manifest.Digest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	got, err := repository.Entries(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != depth+1 {
		t.Fatalf("Entries length = %d, want %d", len(got), depth+1)
	}
}

func makePortableTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"empty", "bin", "docs", "links"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "tool"), []byte("#!/bin/sh\necho tool\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "report.txt"), []byte("portable report\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "../docs/report.txt", filepath.Join(root, "links", "report"))
	return root
}

func assertCaptureRejected(t *testing.T, source string) {
	t.Helper()
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CaptureTree(context.Background(), source); err == nil {
		t.Fatal("CaptureTree accepted unsupported entry")
	}
}

func mustSymlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

func putTestObject(t *testing.T, store Store, data []byte) Object {
	t.Helper()
	object, err := store.Put(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func deepTreeName(index int) string {
	const digits = "0123456789"
	data := []byte("link-00000")
	for position := len(data) - 1; position >= len("link-"); position-- {
		data[position] = digits[index%10]
		index /= 10
	}
	return string(data)
}

var (
	errTreeUnreadable = errors.New("injected unreadable entry")
	errTreeMidWalk    = errors.New("injected mid-walk failure")
)

type reversingTreeFilesystem struct{ treeFilesystem }

func (f reversingTreeFilesystem) readDir(name string) ([]os.DirEntry, error) {
	entries, err := f.treeFilesystem.readDir(name)
	slices.Reverse(entries)
	return entries, err
}

type failingOpenTreeFilesystem struct {
	treeFilesystem
	name string
	err  error
}

func (f failingOpenTreeFilesystem) open(name string) (treeReadFile, error) {
	if name == f.name {
		return nil, f.err
	}
	return f.treeFilesystem.open(name)
}

type failingReadDirTreeFilesystem struct {
	treeFilesystem
	name string
	err  error
}

func (f failingReadDirTreeFilesystem) readDir(name string) ([]os.DirEntry, error) {
	if name == f.name {
		return nil, f.err
	}
	return f.treeFilesystem.readDir(name)
}

type cancelingTreeFilesystem struct {
	treeFilesystem
	cancel context.CancelFunc
}

func (f cancelingTreeFilesystem) readDir(name string) ([]os.DirEntry, error) {
	entries, err := f.treeFilesystem.readDir(name)
	f.cancel()
	return entries, err
}

type replacingDirectoryTreeFilesystem struct {
	treeFilesystem
	directory string
	target    string
}

func (f replacingDirectoryTreeFilesystem) readDir(name string) ([]os.DirEntry, error) {
	if name == f.directory {
		if err := os.Remove(name); err != nil {
			return nil, err
		}
		if err := os.Symlink(f.target, name); err != nil {
			return nil, err
		}
	}
	return f.treeFilesystem.readDir(name)
}

func requireErrorContains(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error = %v, want fragment %q", err, fragment)
	}
}
