package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
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

func TestCaptureTreePreservesDriveLikeRelativeSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("drive-like target syntax is absolute on Windows")
	}
	source := t.TempDir()
	target := `C:\outside`
	if err := os.WriteFile(filepath.Join(source, target), []byte("ordinary in-tree bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, target, filepath.Join(source, "link"))
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatalf("CaptureTree rejected a relative raw-byte target: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "materialized")
	if err := repository.MaterializeTree(context.Background(), tree, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("materialized symlink target = %q, want %q", got, target)
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

func TestCaptureTreeDoesNotFollowDirectoryReplacedBySymlinkDuringWalk(t *testing.T) {
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

	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := repository.Entries(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !slices.Equal(entries[0].Segments(), []string{"nested"}) || entries[0].Kind() != "directory" {
		t.Fatalf("captured entries = %#v, want only the pinned nested directory", entries)
	}
}

func TestCaptureTreeConfinesDirectorySwappedAfterEnumerationVerification(t *testing.T) {
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
	entry := filepath.Join(nested, "entry")
	if err := os.WriteFile(entry, []byte("inside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideBytes := []byte("outside bytes must never be captured")
	if err := os.WriteFile(filepath.Join(outside, "entry"), outsideBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewMemory()
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	swappingFS := &postVerificationSwapTreeFilesystem{
		treeFilesystem: repository.treeFilesystem(),
		directory:      nested,
		entry:          entry,
		target:         outside,
	}
	repository.treeFS = swappingFS

	_, captureErr := repository.CaptureTree(context.Background(), source)
	if swappingFS.swapErr != nil {
		t.Fatalf("swap fixture failed: %v", swappingFS.swapErr)
	}
	if captureErr == nil {
		t.Error("CaptureTree accepted a directory swapped to an outside symlink after verification")
	}
	outsideDigest := Digest(sha256.Sum256(outsideBytes))
	if _, err := store.Copy(context.Background(), outsideDigest, io.Discard); err == nil {
		t.Error("CaptureTree stored bytes reached only through the outside symlink")
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

func TestTreeManifestSegmentsAreSinglePOSIXComponents(t *testing.T) {
	for _, segment := range []string{"a/b", "a\x00b"} {
		if _, err := encodeTreeManifest([]treeEntry{{segments: []string{segment}, kind: directoryEntry}}); err == nil {
			t.Errorf("encodeTreeManifest accepted segment %q", segment)
		}

		encoded, err := encodeTreeManifest([]treeEntry{{segments: []string{"abc"}, kind: directoryEntry}})
		if err != nil {
			t.Fatal(err)
		}
		offset := bytes.Index(encoded, []byte("abc"))
		if offset < 0 {
			t.Fatal("valid manifest did not contain its segment bytes")
		}
		copy(encoded[offset:offset+3], segment)
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
		if _, err := repository.Entries(context.Background(), tree); err == nil {
			t.Errorf("Entries accepted encoded segment %q", segment)
		}
	}

	if runtime.GOOS != "windows" {
		encoded, err := encodeTreeManifest([]treeEntry{{segments: []string{`a\b`}, kind: directoryEntry}})
		if err != nil {
			t.Fatalf("backslash is an opaque POSIX component byte: %v", err)
		}
		entries, err := decodeTreeManifest(encoded)
		if err != nil || len(entries) != 1 || entries[0].segments[0] != `a\b` {
			t.Fatalf("backslash segment round trip = (%#v, %v)", entries, err)
		}
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

func TestTreeSymlinkResolutionLongOscillatingTargetIsNearLinear(t *testing.T) {
	const (
		depth        = 512
		oscillations = 20_000
	)
	entries := make([]treeEntry, 0, depth+1)
	segments := make([]string, 0, depth+1)
	for range depth {
		segments = append(segments, "d")
		entries = append(entries, treeEntry{segments: slices.Clone(segments), kind: directoryEntry})
	}
	segments = append(segments, "link")
	entries = append(entries, treeEntry{
		segments: slices.Clone(segments),
		kind:     symlinkEntry,
		target:   strings.Repeat("../d/", oscillations),
	})

	var validationErr error
	allocations := testing.AllocsPerRun(1, func() {
		validationErr = validateTreeEntries(entries)
	})
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if allocations > 10_000 {
		t.Fatalf("long oscillating target allocations = %.0f, want near-linear path-node reuse", allocations)
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

func (f reversingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return reversingTreeCaptureRoot{treeCaptureRoot: root}, nil
}

type reversingTreeCaptureRoot struct{ treeCaptureRoot }

func (r reversingTreeCaptureRoot) readDir(name string) ([]os.DirEntry, error) {
	entries, err := r.treeCaptureRoot.readDir(name)
	slices.Reverse(entries)
	return entries, err
}

func (r reversingTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return reversingTreeCaptureRoot{treeCaptureRoot: root}, nil
}

type failingOpenTreeFilesystem struct {
	treeFilesystem
	name string
	err  error
}

func (f failingOpenTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return failingOpenTreeCaptureRoot{treeCaptureRoot: root, host: name, name: f.name, err: f.err}, nil
}

type failingOpenTreeCaptureRoot struct {
	treeCaptureRoot
	host string
	name string
	err  error
}

func (r failingOpenTreeCaptureRoot) open(name string) (treeReadFile, error) {
	if filepath.Join(r.host, name) == r.name {
		return nil, r.err
	}
	return r.treeCaptureRoot.open(name)
}

func (r failingOpenTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return failingOpenTreeCaptureRoot{treeCaptureRoot: root, host: filepath.Join(r.host, name), name: r.name, err: r.err}, nil
}

type failingReadDirTreeFilesystem struct {
	treeFilesystem
	name string
	err  error
}

func (f failingReadDirTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return failingReadDirTreeCaptureRoot{treeCaptureRoot: root, host: name, name: f.name, err: f.err}, nil
}

type failingReadDirTreeCaptureRoot struct {
	treeCaptureRoot
	host string
	name string
	err  error
}

func (r failingReadDirTreeCaptureRoot) readDir(name string) ([]os.DirEntry, error) {
	if r.host == r.name {
		return nil, r.err
	}
	return r.treeCaptureRoot.readDir(name)
}

func (r failingReadDirTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return failingReadDirTreeCaptureRoot{treeCaptureRoot: root, host: filepath.Join(r.host, name), name: r.name, err: r.err}, nil
}

type cancelingTreeFilesystem struct {
	treeFilesystem
	cancel context.CancelFunc
}

func (f cancelingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return cancelingTreeCaptureRoot{treeCaptureRoot: root, cancel: f.cancel}, nil
}

type cancelingTreeCaptureRoot struct {
	treeCaptureRoot
	cancel context.CancelFunc
}

func (r cancelingTreeCaptureRoot) readDir(name string) ([]os.DirEntry, error) {
	entries, err := r.treeCaptureRoot.readDir(name)
	r.cancel()
	return entries, err
}

func (r cancelingTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return cancelingTreeCaptureRoot{treeCaptureRoot: root, cancel: r.cancel}, nil
}

type replacingDirectoryTreeFilesystem struct {
	treeFilesystem
	directory string
	target    string
}

func (f replacingDirectoryTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return replacingDirectoryTreeCaptureRoot{treeCaptureRoot: root, host: name, directory: f.directory, target: f.target}, nil
}

type replacingDirectoryTreeCaptureRoot struct {
	treeCaptureRoot
	host      string
	directory string
	target    string
}

func (r replacingDirectoryTreeCaptureRoot) readDir(name string) ([]os.DirEntry, error) {
	if r.host == r.directory {
		if err := os.Remove(r.host); err != nil {
			return nil, err
		}
		if err := os.Symlink(r.target, r.host); err != nil {
			return nil, err
		}
	}
	return r.treeCaptureRoot.readDir(name)
}

func (r replacingDirectoryTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return replacingDirectoryTreeCaptureRoot{treeCaptureRoot: root, host: filepath.Join(r.host, name), directory: r.directory, target: r.target}, nil
}

type postVerificationSwapTreeFilesystem struct {
	treeFilesystem
	directory  string
	entry      string
	target     string
	lstatCalls int
	swapErr    error
}

func (f *postVerificationSwapTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return &postVerificationSwapTreeCaptureRoot{treeCaptureRoot: root, host: name, filesystem: f}, nil
}

type postVerificationSwapTreeCaptureRoot struct {
	treeCaptureRoot
	host       string
	filesystem *postVerificationSwapTreeFilesystem
}

func (r *postVerificationSwapTreeCaptureRoot) lstat(name string) (os.FileInfo, error) {
	info, err := r.treeCaptureRoot.lstat(name)
	if r.host != r.filesystem.directory || name != "." || err != nil {
		return info, err
	}
	r.filesystem.lstatCalls++
	if r.filesystem.lstatCalls != 3 {
		return info, nil
	}
	if removeErr := os.Remove(r.filesystem.entry); removeErr != nil {
		r.filesystem.swapErr = removeErr
		return info, nil
	}
	if removeErr := os.Remove(r.filesystem.directory); removeErr != nil {
		r.filesystem.swapErr = removeErr
		return info, nil
	}
	if symlinkErr := os.Symlink(r.filesystem.target, r.filesystem.directory); symlinkErr != nil {
		r.filesystem.swapErr = symlinkErr
	}
	return info, nil
}

func (r *postVerificationSwapTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return &postVerificationSwapTreeCaptureRoot{treeCaptureRoot: root, host: filepath.Join(r.host, name), filesystem: r.filesystem}, nil
}

func requireErrorContains(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error = %v, want fragment %q", err, fragment)
	}
}
