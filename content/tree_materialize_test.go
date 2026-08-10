package content

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

type transformingTreeFilesystem struct {
	treeFilesystem
	kind string
}

func (f transformingTreeFilesystem) mkdir(name string, mode os.FileMode) error {
	if f.kind == "directory" && filepath.Base(name) == "requested" {
		name = filepath.Join(filepath.Dir(name), "transformed")
	}
	return f.treeFilesystem.mkdir(name, mode)
}

func (f transformingTreeFilesystem) link(oldname, newname string) error {
	if f.kind == "file" && filepath.Base(newname) == "requested" {
		newname = filepath.Join(filepath.Dir(newname), "transformed")
	}
	return f.treeFilesystem.link(oldname, newname)
}

func (f transformingTreeFilesystem) symlink(oldname, newname string) error {
	if f.kind == "symlink" && filepath.Base(newname) == "requested" {
		newname = filepath.Join(filepath.Dir(newname), "transformed")
	}
	return f.treeFilesystem.symlink(oldname, newname)
}

func (f caseFoldingTreeFilesystem) lstat(name string) (os.FileInfo, error) {
	if name == filepath.Join(f.destination, "a") {
		return f.treeFilesystem.lstat(filepath.Join(f.destination, "A"))
	}
	return f.treeFilesystem.lstat(name)
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
