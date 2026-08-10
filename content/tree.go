package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// TreeEntry is a read-only diagnostic view of one semantic tree entry.
type TreeEntry struct {
	segments []string
	kind     treeKind
	content  Object
	target   string
}

// Segments returns a defensive copy of the entry's exact-byte path segments.
func (e TreeEntry) Segments() []string { return slices.Clone(e.segments) }

// Kind returns the diagnostic entry kind: directory, regular, executable, or symlink.
func (e TreeEntry) Kind() string {
	switch e.kind {
	case directoryEntry:
		return "directory"
	case fileEntry:
		return "regular"
	case executableEntry:
		return "executable"
	case symlinkEntry:
		return "symlink"
	default:
		return ""
	}
}

// Content returns the immutable content object for regular and executable
// entries. It returns the zero Object for directories and symlinks.
func (e TreeEntry) Content() Object { return e.content }

// Target returns the exact relative target for symlinks. It returns an empty
// string for directories and files.
func (e TreeEntry) Target() string { return e.target }

type treeReadFile interface {
	io.Reader
	Stat() (fs.FileInfo, error)
	Close() error
}

type treeTempFile interface {
	io.Writer
	Name() string
	Chmod(fs.FileMode) error
	Close() error
}

type treeFilesystem interface {
	lstat(string) (fs.FileInfo, error)
	readDir(string) ([]os.DirEntry, error)
	open(string) (treeReadFile, error)
	readlink(string) (string, error)
	mkdir(string, fs.FileMode) error
	createTemp(string, string) (treeTempFile, error)
	link(string, string) error
	symlink(string, string) error
	remove(string) error
	removeAll(string) error
}

type osTreeFilesystem struct{}

func (osTreeFilesystem) lstat(name string) (fs.FileInfo, error)     { return os.Lstat(name) }
func (osTreeFilesystem) readDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }
func (osTreeFilesystem) open(name string) (treeReadFile, error)     { return os.Open(name) }
func (osTreeFilesystem) readlink(name string) (string, error)       { return os.Readlink(name) }
func (osTreeFilesystem) mkdir(name string, mode fs.FileMode) error  { return os.Mkdir(name, mode) }
func (osTreeFilesystem) createTemp(dir, pattern string) (treeTempFile, error) {
	return os.CreateTemp(dir, pattern)
}
func (osTreeFilesystem) link(oldname, newname string) error { return os.Link(oldname, newname) }
func (osTreeFilesystem) symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}
func (osTreeFilesystem) remove(name string) error    { return os.Remove(name) }
func (osTreeFilesystem) removeAll(name string) error { return os.RemoveAll(name) }

func (r *Repository) treeFilesystem() treeFilesystem {
	if r != nil && r.treeFS != nil {
		return r.treeFS
	}
	return osTreeFilesystem{}
}

// CaptureTree captures the complete directory rooted at source as an immutable,
// portable tree. It does not follow symlinks.
func (r *Repository) CaptureTree(ctx context.Context, source string) (Tree, error) {
	if r == nil || r.store == nil {
		return Tree{}, fmt.Errorf("content: file repository is invalid")
	}
	if source == "" {
		return Tree{}, fmt.Errorf("content: tree source must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return Tree{}, fmt.Errorf("content: capture tree: %w", err)
	}
	filesystem := r.treeFilesystem()
	rootInfo, err := filesystem.lstat(source)
	if err != nil {
		return Tree{}, fmt.Errorf("content: inspect tree source: %w", err)
	}
	if !rootInfo.IsDir() {
		return Tree{}, fmt.Errorf("content: tree source must be a directory")
	}

	type pendingDirectory struct {
		host     string
		segments []string
		expected fs.FileInfo
	}
	pending := []pendingDirectory{{host: source, expected: rootInfo}}
	entries := make([]treeEntry, 0)
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return Tree{}, fmt.Errorf("content: capture tree: %w", err)
		}
		directory := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := verifyCapturedDirectory(filesystem, directory.host, directory.expected); err != nil {
			return Tree{}, err
		}
		children, err := filesystem.readDir(directory.host)
		if err != nil {
			return Tree{}, fmt.Errorf("content: read tree directory: %w", err)
		}
		if err := verifyCapturedDirectory(filesystem, directory.host, directory.expected); err != nil {
			return Tree{}, err
		}
		if err := ctx.Err(); err != nil {
			return Tree{}, fmt.Errorf("content: capture tree: %w", err)
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return Tree{}, fmt.Errorf("content: capture tree: %w", err)
			}
			name := child.Name()
			if name == "" || name == "." || name == ".." {
				return Tree{}, fmt.Errorf("content: invalid filesystem entry name")
			}
			hostName := filepath.Join(directory.host, name)
			info, err := filesystem.lstat(hostName)
			if err != nil {
				return Tree{}, fmt.Errorf("content: inspect tree entry: %w", err)
			}
			segments := append(slices.Clone(directory.segments), name)
			switch mode := info.Mode(); {
			case mode.IsDir():
				entries = append(entries, treeEntry{segments: segments, kind: directoryEntry})
				pending = append(pending, pendingDirectory{host: hostName, segments: segments, expected: info})
			case mode.IsRegular():
				object, err := r.captureTreeFile(ctx, filesystem, hostName, info)
				if err != nil {
					return Tree{}, err
				}
				kind := fileEntry
				if mode.Perm()&0o111 != 0 {
					kind = executableEntry
				}
				entries = append(entries, treeEntry{segments: segments, kind: kind, content: object})
			case mode&os.ModeSymlink != 0:
				target, err := filesystem.readlink(hostName)
				if err != nil {
					return Tree{}, fmt.Errorf("content: read tree symlink: %w", err)
				}
				entries = append(entries, treeEntry{segments: segments, kind: symlinkEntry, target: target})
			default:
				return Tree{}, fmt.Errorf("content: unsupported tree entry %q with mode %v", hostName, mode)
			}
		}
	}
	slices.SortFunc(entries, func(a, b treeEntry) int {
		return compareTreeSegments(a.segments, b.segments)
	})
	manifest, err := encodeTreeManifest(entries)
	if err != nil {
		return Tree{}, fmt.Errorf("content: capture tree: %w", err)
	}
	object, err := r.store.Put(ctx, bytes.NewReader(manifest))
	if err != nil {
		return Tree{}, fmt.Errorf("content: store tree manifest: %w", err)
	}
	tree, err := NewTree(object.Digest())
	if err != nil {
		return Tree{}, fmt.Errorf("content: store tree manifest: %w", err)
	}
	return tree, nil
}

func verifyCapturedDirectory(filesystem treeFilesystem, name string, expected fs.FileInfo) error {
	current, err := filesystem.lstat(name)
	if err != nil {
		return fmt.Errorf("content: inspect tree directory: %w", err)
	}
	if !current.IsDir() || !os.SameFile(expected, current) {
		return fmt.Errorf("content: tree directory changed while being captured")
	}
	return nil
}

func (r *Repository) captureTreeFile(ctx context.Context, filesystem treeFilesystem, name string, expected fs.FileInfo) (object Object, err error) {
	file, err := filesystem.open(name)
	if err != nil {
		return Object{}, fmt.Errorf("content: open tree file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("content: close tree file: %w", closeErr))
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return Object{}, fmt.Errorf("content: inspect opened tree file: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return Object{}, fmt.Errorf("content: tree file changed while being captured")
	}
	object, err = r.store.Put(ctx, file)
	if err != nil {
		return Object{}, fmt.Errorf("content: store tree file: %w", err)
	}
	if !object.Valid() {
		return Object{}, fmt.Errorf("content: store returned an invalid tree file object")
	}
	return object, nil
}

// Entries returns a read-only diagnostic view of a tree's canonical entries.
func (r *Repository) Entries(ctx context.Context, tree Tree) ([]TreeEntry, error) {
	entries, err := r.loadTree(ctx, tree)
	if err != nil {
		return nil, err
	}
	result := make([]TreeEntry, len(entries))
	for index, entry := range entries {
		result[index] = TreeEntry{
			segments: slices.Clone(entry.segments),
			kind:     entry.kind,
			content:  entry.content,
			target:   entry.target,
		}
	}
	return result, nil
}

func (r *Repository) loadTree(ctx context.Context, tree Tree) ([]treeEntry, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("content: file repository is invalid")
	}
	if !tree.Valid() {
		return nil, fmt.Errorf("content: tree is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("content: read tree: %w", err)
	}
	var manifest bytes.Buffer
	hasher := sha256.New()
	size, err := r.store.Copy(ctx, tree.Digest(), io.MultiWriter(&manifest, hasher))
	if err != nil {
		return nil, fmt.Errorf("content: read tree manifest: %w", err)
	}
	if size != int64(manifest.Len()) || !tree.Digest().Equal(digestFromHash(hasher)) {
		return nil, integrityError("tree manifest does not match tree identity")
	}
	entries, err := decodeTreeManifest(manifest.Bytes())
	if err != nil {
		return nil, fmt.Errorf("content: read tree manifest: %w", err)
	}
	return entries, nil
}

// MaterializeTree recreates tree at an absent runtime-owned destination.
func (r *Repository) MaterializeTree(ctx context.Context, tree Tree, destination string) (err error) {
	if destination == "" {
		return fmt.Errorf("content: tree materialization destination must not be empty")
	}
	entries, err := r.loadTree(ctx, tree)
	if err != nil {
		return err
	}
	filesystem := r.treeFilesystem()
	if _, statErr := filesystem.lstat(destination); statErr == nil {
		return fmt.Errorf("content: tree materialization destination already exists")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("content: inspect tree materialization destination: %w", statErr)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	if err := filesystem.mkdir(destination, 0o700); err != nil {
		return fmt.Errorf("content: create tree materialization destination: %w", err)
	}
	removeDestination := true
	defer func() {
		if !removeDestination {
			return
		}
		if cleanupErr := filesystem.removeAll(destination); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("content: remove failed tree materialization: %w", cleanupErr))
		}
	}()

	for _, entry := range entries {
		if entry.kind != directoryEntry {
			continue
		}
		name, pathErr := treeHostPath(destination, entry.segments)
		if pathErr != nil {
			return pathErr
		}
		if err := createTreeDirectory(ctx, filesystem, name); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if entry.kind != fileEntry && entry.kind != executableEntry {
			continue
		}
		name, pathErr := treeHostPath(destination, entry.segments)
		if pathErr != nil {
			return pathErr
		}
		if err := r.materializeTreeFile(ctx, filesystem, entry, name); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if entry.kind != symlinkEntry {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("content: materialize tree: %w", err)
		}
		name, pathErr := treeHostPath(destination, entry.segments)
		if pathErr != nil {
			return pathErr
		}
		if err := requireAbsentTreePath(filesystem, name); err != nil {
			return err
		}
		if err := filesystem.symlink(entry.target, name); err != nil {
			return fmt.Errorf("content: create tree symlink: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	removeDestination = false
	return nil
}

func createTreeDirectory(ctx context.Context, filesystem treeFilesystem, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	if err := requireAbsentTreePath(filesystem, name); err != nil {
		return err
	}
	if err := filesystem.mkdir(name, 0o700); err != nil {
		return fmt.Errorf("content: create tree directory: %w", err)
	}
	return nil
}

func (r *Repository) materializeTreeFile(ctx context.Context, filesystem treeFilesystem, entry treeEntry, name string) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	if err := requireAbsentTreePath(filesystem, name); err != nil {
		return err
	}
	temporary, err := filesystem.createTemp(filepath.Dir(name), ".dawn-tree-*")
	if err != nil {
		return fmt.Errorf("content: create tree file temporary: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		var cleanupErr error
		if !closed {
			closed = true
			if closeErr := temporary.Close(); closeErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("content: close tree file temporary during cleanup: %w", closeErr))
			}
		}
		if temporaryName != "" {
			if removeErr := filesystem.remove(temporaryName); removeErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("content: remove tree file temporary: %w", removeErr))
			}
		}
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err := r.copyTreeObject(ctx, entry.content, temporary); err != nil {
		return err
	}
	mode := fs.FileMode(0o600)
	if entry.kind == executableEntry {
		mode = 0o700
	}
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("content: set tree file mode: %w", err)
	}
	closed = true
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("content: close tree file temporary: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	if err := filesystem.link(temporaryName, name); err != nil {
		return fmt.Errorf("content: publish tree file: %w", err)
	}
	return nil
}

func (r *Repository) copyTreeObject(ctx context.Context, object Object, destination io.Writer) error {
	hasher := sha256.New()
	size, err := r.store.Copy(ctx, object.Digest(), io.MultiWriter(destination, hasher))
	if err != nil {
		return fmt.Errorf("content: copy tree file: %w", err)
	}
	if size != object.Size() || !object.Digest().Equal(digestFromHash(hasher)) {
		return integrityError("tree file does not match manifest")
	}
	return nil
}

func requireAbsentTreePath(filesystem treeFilesystem, name string) error {
	if _, err := filesystem.lstat(name); err == nil {
		return fmt.Errorf("content: tree materialization path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("content: inspect tree materialization path: %w", err)
	}
	return nil
}

func treeHostPath(root string, segments []string) (string, error) {
	name := root
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || filepath.IsAbs(segment) || filepath.VolumeName(segment) != "" || filepath.Base(segment) != segment || filepath.Clean(segment) != segment {
			return "", fmt.Errorf("content: concrete filesystem cannot reproduce tree path segment")
		}
		next := filepath.Join(name, segment)
		if filepath.Dir(next) != filepath.Clean(name) {
			return "", fmt.Errorf("content: concrete filesystem cannot reproduce tree path segment")
		}
		name = next
	}
	return name, nil
}
