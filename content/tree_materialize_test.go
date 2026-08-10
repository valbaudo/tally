package content

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestMaterializeTreeWritesExactPortableTree(t *testing.T) {
	source := makePortableTree(t)
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "fresh-tree")

	if err := repository.MaterializeTree(context.Background(), tree, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin/tool", "docs/report.txt"} {
		want, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s bytes = %q, want %q", name, got, want)
		}
	}
	info, err := os.Stat(filepath.Join(destination, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("tool mode = %o, want executable", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(destination, "docs", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("report mode = %o, want non-executable", info.Mode().Perm())
	}
	if entries, err := os.ReadDir(filepath.Join(destination, "empty")); err != nil || len(entries) != 0 {
		t.Fatalf("empty directory = (%v, %v), want present and empty", entries, err)
	}
	target, err := os.Readlink(filepath.Join(destination, "links", "report"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "../docs/report.txt" {
		t.Fatalf("symlink target = %q, want ../docs/report.txt", target)
	}
}

func TestMaterializeTreeRequiresAbsentDestination(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "caller-owned")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "keep")
	if err := os.WriteFile(marker, []byte("caller bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := repository.MaterializeTree(context.Background(), tree, destination); err == nil {
		t.Fatal("MaterializeTree accepted an existing destination")
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "caller bytes" {
		t.Fatalf("existing destination marker changed to %q", got)
	}
}

func TestMaterializeTreeRemovesOnlyNewDestinationAfterFailure(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("committed"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	store.mu.Lock()
	store.contents[entries[0].Content().Digest()] = []byte("corrupt")
	store.mu.Unlock()
	parent := t.TempDir()
	sibling := filepath.Join(parent, "keep")
	if err := os.WriteFile(sibling, []byte("sibling"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "new-tree")

	if err := repository.MaterializeTree(context.Background(), tree, destination); err == nil {
		t.Fatal("MaterializeTree accepted a corrupt content blob")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed destination status = %v, want absent", err)
	}
	got, err := os.ReadFile(sibling)
	if err != nil || string(got) != "sibling" {
		t.Fatalf("sibling after cleanup = (%q, %v)", got, err)
	}
}

func TestMaterializeTreeRollsBackAfterFinalParentCloseFailure(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "new-tree")
	sibling := filepath.Join(parent, "keep")
	if err := os.WriteFile(sibling, []byte("sibling"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository.treeFS = &finalParentCloseFailingTreeFilesystem{
		treeFilesystem: repository.treeFilesystem(),
		parent:         parent,
	}

	err = repository.MaterializeTree(context.Background(), tree, destination)
	if err == nil || !strings.Contains(err.Error(), "injected final parent close failure") {
		t.Fatalf("MaterializeTree error = %v, want final parent close failure", err)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination after final parent close failure = %v, want absent", statErr)
	}
	got, err := os.ReadFile(sibling)
	if err != nil || string(got) != "sibling" {
		t.Fatalf("sibling after close-failure cleanup = (%q, %v)", got, err)
	}
}

func TestMaterializeTreeRollsBackWhenFirstPostCreateInspectionFails(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "new-tree")
	failing := &postCreateInspectionFailingTreeFilesystem{
		treeFilesystem: repository.treeFilesystem(),
		parent:         parent,
	}
	repository.treeFS = failing

	err = repository.MaterializeTree(context.Background(), tree, destination)
	if err == nil || !strings.Contains(err.Error(), "injected post-create inspection failure") {
		t.Fatalf("MaterializeTree error = %v, want post-create inspection failure", err)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination after post-create inspection failure = %v, want absent", statErr)
	}
}

func TestTreeRollbackRemovalHandlesDeepFiniteTreeUnderLowStack(t *testing.T) {
	const childCase = "DAWN_DEEP_TREE_ROLLBACK_REMOVE_CASE"
	if os.Getenv(childCase) == "" {
		command := exec.Command(os.Args[0], "-test.run=^TestTreeRollbackRemovalHandlesDeepFiniteTreeUnderLowStack$", "-test.count=1")
		command.Env = append(os.Environ(), childCase+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("deep tree rollback removal did not return ordinarily: %v\n%s", err, output)
		}
		return
	}

	parent := t.TempDir()
	treePath := filepath.Join(parent, "tree")
	if err := os.Mkdir(treePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createDeepTreeMaterializationFixture(treePath, 512); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	debug.SetMaxStack(64 << 10)
	result := make(chan error, 1)
	go func() {
		removeErr := (osTreeCaptureRoot{root: root}).removeAll("tree")
		result <- errors.Join(removeErr, root.Close())
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(treePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deep rollback target status = %v, want absent", err)
	}
}

func createDeepTreeMaterializationFixture(path string, depth int) (err error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for range depth {
		if err := root.Mkdir("d", 0o700); err != nil {
			return err
		}
		child, err := root.OpenRoot("d")
		if err != nil {
			return err
		}
		if err := root.Close(); err != nil {
			_ = child.Close()
			return err
		}
		root = child
	}
	return nil
}

func TestMaterializeTreeRejectsCorruptManifestBeforeCreatingDestination(t *testing.T) {
	store := NewMemory()
	manifest := putTestObject(t, store, []byte("not dawn tree data"))
	tree, err := NewTree(manifest.Digest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "must-stay-absent")

	if err := repository.MaterializeTree(context.Background(), tree, destination); err == nil {
		t.Fatal("MaterializeTree accepted a corrupt manifest")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination status = %v, want absent", err)
	}
}

func TestMaterializeTreeRejectsNoncanonicalManifest(t *testing.T) {
	store := NewMemory()
	one := putTestObject(t, store, []byte("one"))
	two := putTestObject(t, store, []byte("two"))
	encoded, err := encodeTreeManifest([]treeEntry{
		{segments: []string{"b"}, kind: fileEntry, content: one},
		{segments: []string{"a"}, kind: fileEntry, content: two},
	})
	if err == nil {
		t.Fatal("encoder accepted noncanonical entries")
	}
	if encoded != nil {
		t.Fatalf("failed encoding returned %d bytes", len(encoded))
	}
}

func TestMaterializeTreeSurfacesConcreteNameCollision(t *testing.T) {
	store := NewMemory()
	upper := putTestObject(t, store, []byte("upper"))
	lower := putTestObject(t, store, []byte("lower"))
	entries := []treeEntry{
		{segments: []string{"A"}, kind: fileEntry, content: upper},
		{segments: []string{"a"}, kind: fileEntry, content: lower},
	}
	encoded, err := encodeTreeManifest(entries)
	if err != nil {
		t.Fatal(err)
	}
	manifest := putTestObject(t, store, encoded)
	tree, err := NewTree(manifest.Digest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "collision")
	repository.treeFS = caseFoldingTreeFilesystem{treeFilesystem: repository.treeFilesystem(), destination: destination}

	err = repository.MaterializeTree(context.Background(), tree, destination)
	if err == nil {
		t.Fatal("MaterializeTree overwrote or aliased two distinct byte names")
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination after collision = %v, want absent", statErr)
	}
}

func TestMaterializeTreeRejectsConcreteByteNameTransformation(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			switch kind {
			case "directory":
				if err := os.Mkdir(filepath.Join(source, "requested"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(filepath.Join(source, "requested"), []byte("file bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.WriteFile(filepath.Join(source, "target"), []byte("target bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				mustSymlink(t, "target", filepath.Join(source, "requested"))
			}
			repository, err := NewRepository(NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			tree, err := repository.CaptureTree(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "transformed")
			repository.treeFS = transformingTreeFilesystem{
				treeFilesystem: repository.treeFilesystem(),
				kind:           kind,
			}

			if err := repository.MaterializeTree(context.Background(), tree, destination); err == nil {
				t.Fatal("MaterializeTree accepted a backend-transformed entry name")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination after name transformation = %v, want absent", err)
			}
		})
	}
}

func TestMaterializeTreeRemovesTransformedDestinationRootByActualName(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "requested-root")
	actual := filepath.Join(parent, "transformed-root")
	repository.treeFS = rootTransformingTreeFilesystem{
		treeFilesystem: repository.treeFilesystem(),
		requested:      destination,
		actual:         actual,
	}

	if err := repository.MaterializeTree(context.Background(), tree, destination); err == nil {
		t.Fatal("MaterializeTree accepted a transformed destination root name")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("parent entries after rollback = %q, want none", treeDirEntryNames(entries))
	}
}

func TestMaterializeTreeRejectsExactNameDecoyForCreatedObject(t *testing.T) {
	for _, kind := range []string{"directory", "file", "file alias", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			switch kind {
			case "directory":
				if err := os.Mkdir(filepath.Join(source, "requested"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "requested", "child"), []byte("must stay confined"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "file", "file alias":
				if err := os.WriteFile(filepath.Join(source, "requested"), []byte("file bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.WriteFile(filepath.Join(source, "target"), []byte("target bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				mustSymlink(t, "target", filepath.Join(source, "requested"))
			}
			repository, err := NewRepository(NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			tree, err := repository.CaptureTree(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "decoy")
			decoyFS := &decoyTransformingTreeFilesystem{
				treeFilesystem: repository.treeFilesystem(),
				kind:           kind,
			}
			repository.treeFS = decoyFS

			if err := repository.MaterializeTree(context.Background(), tree, destination); err == nil {
				t.Fatal("MaterializeTree accepted an exact-name decoy for a transformed created object")
			}
			if decoyFS.descendantCreated {
				t.Fatal("MaterializeTree wrote a descendant through the exact-name decoy directory")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination after decoy failure = %v, want absent", err)
			}
		})
	}
}

func TestMaterializeTreeHonorsCancellationBeforeCreation(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	destination := filepath.Join(t.TempDir(), "canceled")

	err = repository.MaterializeTree(ctx, tree, destination)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MaterializeTree error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled destination status = %v, want absent", statErr)
	}
}

type caseFoldingTreeFilesystem struct {
	treeFilesystem
	destination string
}

type finalParentCloseFailingTreeFilesystem struct {
	treeFilesystem
	parent string
	failed bool
}

type postCreateInspectionFailingTreeFilesystem struct {
	treeFilesystem
	parent  string
	created bool
	failed  bool
}

func (f *postCreateInspectionFailingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	if name == f.parent {
		return &postCreateInspectionFailingTreeRoot{treeCaptureRoot: root, filesystem: f}, nil
	}
	return root, nil
}

type postCreateInspectionFailingTreeRoot struct {
	treeCaptureRoot
	filesystem *postCreateInspectionFailingTreeFilesystem
}

func (r *postCreateInspectionFailingTreeRoot) mkdir(name string, mode os.FileMode) (os.FileInfo, error) {
	created, err := r.treeCaptureRoot.mkdir(name, mode)
	if err == nil {
		r.filesystem.created = true
	}
	return created, err
}

func (r *postCreateInspectionFailingTreeRoot) readDir(name string) ([]os.DirEntry, error) {
	if r.filesystem.created && !r.filesystem.failed {
		r.filesystem.failed = true
		return nil, errors.New("injected post-create inspection failure")
	}
	return r.treeCaptureRoot.readDir(name)
}

func (f *finalParentCloseFailingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	if name == f.parent && !f.failed {
		return finalParentCloseFailingTreeRoot{treeCaptureRoot: root, filesystem: f}, nil
	}
	return root, nil
}

type finalParentCloseFailingTreeRoot struct {
	treeCaptureRoot
	filesystem *finalParentCloseFailingTreeFilesystem
}

func (r finalParentCloseFailingTreeRoot) close() error {
	closeErr := r.treeCaptureRoot.close()
	if !r.filesystem.failed {
		r.filesystem.failed = true
		return errors.Join(closeErr, errors.New("injected final parent close failure"))
	}
	return closeErr
}

type transformingTreeFilesystem struct {
	treeFilesystem
	kind string
}

func (f transformingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return transformingTreeRoot{treeCaptureRoot: root, kind: f.kind}, nil
}

type transformingTreeRoot struct {
	treeCaptureRoot
	kind string
}

func (r transformingTreeRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return transformingTreeRoot{treeCaptureRoot: root, kind: r.kind}, nil
}

func (r transformingTreeRoot) mkdir(name string, mode os.FileMode) (os.FileInfo, error) {
	if r.kind == "directory" && name == "requested" {
		name = "transformed"
	}
	return r.treeCaptureRoot.mkdir(name, mode)
}

func (r transformingTreeRoot) link(oldname, newname string) (os.FileInfo, error) {
	if r.kind == "file" && newname == "requested" {
		newname = "transformed"
	}
	return r.treeCaptureRoot.link(oldname, newname)
}

func (r transformingTreeRoot) symlink(oldname, newname string) (os.FileInfo, error) {
	if r.kind == "symlink" && newname == "requested" {
		newname = "transformed"
	}
	return r.treeCaptureRoot.symlink(oldname, newname)
}

type rootTransformingTreeFilesystem struct {
	treeFilesystem
	requested string
	actual    string
}

func (f rootTransformingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return rootTransformingTreeRoot{treeCaptureRoot: root, host: name, requested: f.requested, actual: f.actual}, nil
}

type rootTransformingTreeRoot struct {
	treeCaptureRoot
	host      string
	requested string
	actual    string
}

func (r rootTransformingTreeRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return rootTransformingTreeRoot{treeCaptureRoot: root, host: filepath.Join(r.host, name), requested: r.requested, actual: r.actual}, nil
}

func (r rootTransformingTreeRoot) mkdir(name string, mode os.FileMode) (os.FileInfo, error) {
	if filepath.Join(r.host, name) == r.requested {
		name = filepath.Base(r.actual)
	}
	return r.treeCaptureRoot.mkdir(name, mode)
}

type decoyTransformingTreeFilesystem struct {
	treeFilesystem
	kind              string
	descendantCreated bool
}

func (f *decoyTransformingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return &decoyTransformingTreeRoot{treeCaptureRoot: root, host: name, filesystem: f}, nil
}

type decoyTransformingTreeRoot struct {
	treeCaptureRoot
	host       string
	filesystem *decoyTransformingTreeFilesystem
}

func (r *decoyTransformingTreeRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return &decoyTransformingTreeRoot{treeCaptureRoot: root, host: filepath.Join(r.host, name), filesystem: r.filesystem}, nil
}

func (r *decoyTransformingTreeRoot) mkdir(name string, mode os.FileMode) (os.FileInfo, error) {
	if r.filesystem.kind != "directory" || name != "requested" {
		return r.treeCaptureRoot.mkdir(name, mode)
	}
	created, err := r.treeCaptureRoot.mkdir("transformed", mode)
	if err != nil {
		return nil, err
	}
	if _, err := r.treeCaptureRoot.mkdir(name, mode); err != nil {
		return nil, err
	}
	return created, nil
}

func (r *decoyTransformingTreeRoot) link(oldname, newname string) (os.FileInfo, error) {
	if r.filesystem.kind == "directory" && newname == "child" && filepath.Base(r.host) == "requested" {
		r.filesystem.descendantCreated = true
	}
	if (r.filesystem.kind != "file" && r.filesystem.kind != "file alias") || newname != "requested" {
		return r.treeCaptureRoot.link(oldname, newname)
	}
	created, err := r.treeCaptureRoot.link(oldname, "transformed")
	if err != nil {
		return nil, err
	}
	if r.filesystem.kind == "file alias" {
		if _, err := r.treeCaptureRoot.link(oldname, newname); err != nil {
			return nil, err
		}
		return created, nil
	}
	temporary, err := r.treeCaptureRoot.createTemp(".decoy-")
	if err != nil {
		return nil, err
	}
	temporaryName := temporary.Name()
	if _, err := temporary.Write([]byte("decoy")); err != nil {
		_ = temporary.Close()
		_ = r.treeCaptureRoot.remove(temporaryName)
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		_ = r.treeCaptureRoot.remove(temporaryName)
		return nil, err
	}
	if _, err := r.treeCaptureRoot.link(temporaryName, newname); err != nil {
		_ = r.treeCaptureRoot.remove(temporaryName)
		return nil, err
	}
	if err := r.treeCaptureRoot.remove(temporaryName); err != nil {
		return nil, err
	}
	return created, nil
}

func (r *decoyTransformingTreeRoot) symlink(oldname, newname string) (os.FileInfo, error) {
	if r.filesystem.kind != "symlink" || newname != "requested" {
		return r.treeCaptureRoot.symlink(oldname, newname)
	}
	created, err := r.treeCaptureRoot.symlink(oldname, "transformed")
	if err != nil {
		return nil, err
	}
	if _, err := r.treeCaptureRoot.symlink(oldname, newname); err != nil {
		return nil, err
	}
	return created, nil
}

func treeDirEntryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func (f caseFoldingTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := f.treeFilesystem.openRoot(name)
	if err != nil {
		return nil, err
	}
	return caseFoldingTreeRoot{treeCaptureRoot: root}, nil
}

type caseFoldingTreeRoot struct{ treeCaptureRoot }

func (r caseFoldingTreeRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.treeCaptureRoot.openRoot(name)
	if err != nil {
		return nil, err
	}
	return caseFoldingTreeRoot{treeCaptureRoot: root}, nil
}

func (r caseFoldingTreeRoot) lstat(name string) (os.FileInfo, error) {
	if name == "a" {
		return r.treeCaptureRoot.lstat("A")
	}
	return r.treeCaptureRoot.lstat(name)
}

func TestTreeEntriesRejectsManifestWithTrailingData(t *testing.T) {
	store := NewMemory()
	encoded, err := encodeTreeManifest(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, []byte("trailing")...)
	manifest := putTestObject(t, store, encoded)
	tree, err := NewTree(manifest.Digest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.Entries(context.Background(), tree)
	requireErrorContains(t, err, "manifest")
}

func TestTreeEntriesRejectsHugeDeclaredLengthsOrdinarily(t *testing.T) {
	store := NewMemory()
	manifestBytes := append([]byte("dawn.tree/1"), bytes.Repeat([]byte{0xff}, 8)...)
	manifest := putTestObject(t, store, manifestBytes)
	tree, err := NewTree(manifest.Digest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.Entries(context.Background(), tree)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("Entries error = %v, want bounded manifest rejection", err)
	}
}
