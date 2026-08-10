package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

type treeCaptureRoot interface {
	lstat(string) (fs.FileInfo, error)
	readDir(string) ([]os.DirEntry, error)
	open(string) (treeReadFile, error)
	readlink(string) (string, error)
	openRoot(string) (treeCaptureRoot, error)
	mkdir(string, fs.FileMode) (fs.FileInfo, error)
	createTemp(string) (treeTempFile, error)
	link(string, string) (fs.FileInfo, error)
	symlink(string, string) (fs.FileInfo, error)
	remove(string) error
	removeAll(string) error
	close() error
}

type treeFilesystem interface {
	lstat(string) (fs.FileInfo, error)
	openRoot(string) (treeCaptureRoot, error)
}

type osTreeFilesystem struct{}

func (osTreeFilesystem) lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }
func (osTreeFilesystem) openRoot(name string) (treeCaptureRoot, error) {
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return osTreeCaptureRoot{root: root}, nil
}

type osTreeCaptureRoot struct {
	root *os.Root
}

func (r osTreeCaptureRoot) lstat(name string) (fs.FileInfo, error) { return r.root.Lstat(name) }
func (r osTreeCaptureRoot) readDir(name string) ([]os.DirEntry, error) {
	return fs.ReadDir(r.root.FS(), filepath.ToSlash(name))
}
func (r osTreeCaptureRoot) open(name string) (treeReadFile, error) { return r.root.Open(name) }
func (r osTreeCaptureRoot) readlink(name string) (string, error)   { return r.root.Readlink(name) }
func (r osTreeCaptureRoot) openRoot(name string) (treeCaptureRoot, error) {
	root, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return osTreeCaptureRoot{root: root}, nil
}
func (r osTreeCaptureRoot) mkdir(name string, mode fs.FileMode) (fs.FileInfo, error) {
	if err := r.root.Mkdir(name, mode); err != nil {
		return nil, err
	}
	return r.root.Lstat(name)
}
func (r osTreeCaptureRoot) createTemp(pattern string) (treeTempFile, error) {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, fmt.Errorf("generate temporary file name: %w", err)
		}
		name := pattern + hex.EncodeToString(random[:])
		file, err := r.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return treeRootTempFile{File: file, name: name}, nil
	}
}
func (r osTreeCaptureRoot) link(oldname, newname string) (fs.FileInfo, error) {
	if err := r.root.Link(oldname, newname); err != nil {
		return nil, err
	}
	return r.root.Lstat(newname)
}
func (r osTreeCaptureRoot) symlink(oldname, newname string) (fs.FileInfo, error) {
	if err := r.root.Symlink(oldname, newname); err != nil {
		return nil, err
	}
	return r.root.Lstat(newname)
}
func (r osTreeCaptureRoot) remove(name string) error    { return r.root.Remove(name) }
func (r osTreeCaptureRoot) removeAll(name string) error { return r.root.RemoveAll(name) }
func (r osTreeCaptureRoot) close() error                { return r.root.Close() }

type treeRootTempFile struct {
	*os.File
	name string
}

func (f treeRootTempFile) Name() string { return f.name }

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
	root, err := filesystem.openRoot(source)
	if err != nil {
		return Tree{}, fmt.Errorf("content: open tree source: %w", err)
	}
	openedRootInfo, err := root.lstat(".")
	if err != nil || !openedRootInfo.IsDir() || !os.SameFile(rootInfo, openedRootInfo) {
		_ = root.close()
		if err != nil {
			return Tree{}, fmt.Errorf("content: inspect opened tree source: %w", err)
		}
		return Tree{}, fmt.Errorf("content: tree source changed while being opened")
	}
	entries, err := r.captureTreeEntries(ctx, root)
	if err != nil {
		return Tree{}, err
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

type captureDirectoryFrame struct {
	root       treeCaptureRoot
	segments   []string
	children   []os.DirEntry
	nextChild  int
	enumerated bool
}

func (r *Repository) captureTreeEntries(ctx context.Context, root treeCaptureRoot) (entries []treeEntry, err error) {
	stack := []captureDirectoryFrame{{root: root}}
	defer func() {
		for index := len(stack) - 1; index >= 0; index-- {
			if stack[index].root == nil {
				continue
			}
			if closeErr := stack[index].root.close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("content: close captured tree directory: %w", closeErr))
			}
		}
	}()
	for len(stack) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("content: capture tree: %w", err)
		}
		frame := &stack[len(stack)-1]
		if !frame.enumerated {
			if err := verifyCapturedDirectory(frame.root); err != nil {
				return nil, err
			}
			children, readErr := frame.root.readDir(".")
			if readErr != nil {
				return nil, fmt.Errorf("content: read tree directory: %w", readErr)
			}
			if err := verifyCapturedDirectory(frame.root); err != nil {
				return nil, err
			}
			frame.children = children
			frame.enumerated = true
			continue
		}
		if frame.nextChild == len(frame.children) {
			root := frame.root
			frame.root = nil
			if closeErr := root.close(); closeErr != nil {
				return nil, fmt.Errorf("content: close captured tree directory: %w", closeErr)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		child := frame.children[frame.nextChild]
		frame.nextChild++
		name := child.Name()
		if name == "" || name == "." || name == ".." {
			return nil, fmt.Errorf("content: invalid filesystem entry name")
		}
		info, statErr := frame.root.lstat(name)
		if statErr != nil {
			return nil, fmt.Errorf("content: inspect tree entry: %w", statErr)
		}
		segments := append(slices.Clone(frame.segments), name)
		switch mode := info.Mode(); {
		case mode.IsDir():
			childRoot, openErr := frame.root.openRoot(name)
			if openErr != nil {
				return nil, fmt.Errorf("content: open tree directory: %w", openErr)
			}
			openedInfo, inspectErr := childRoot.lstat(".")
			if inspectErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
				_ = childRoot.close()
				if inspectErr != nil {
					return nil, fmt.Errorf("content: inspect opened tree directory: %w", inspectErr)
				}
				return nil, fmt.Errorf("content: tree directory changed while being opened")
			}
			entries = append(entries, treeEntry{segments: segments, kind: directoryEntry})
			stack = append(stack, captureDirectoryFrame{root: childRoot, segments: segments})
		case mode.IsRegular():
			object, captureErr := r.captureTreeFile(ctx, frame.root, name, info)
			if captureErr != nil {
				return nil, captureErr
			}
			kind := fileEntry
			if mode.Perm()&0o111 != 0 {
				kind = executableEntry
			}
			entries = append(entries, treeEntry{segments: segments, kind: kind, content: object})
		case mode&os.ModeSymlink != 0:
			target, readErr := frame.root.readlink(name)
			if readErr != nil {
				return nil, fmt.Errorf("content: read tree symlink: %w", readErr)
			}
			entries = append(entries, treeEntry{segments: segments, kind: symlinkEntry, target: target})
		default:
			return nil, fmt.Errorf("content: unsupported tree entry %q with mode %v", name, mode)
		}
	}
	return entries, nil
}

func verifyCapturedDirectory(root treeCaptureRoot) error {
	current, err := root.lstat(".")
	if err != nil {
		return fmt.Errorf("content: inspect tree directory: %w", err)
	}
	if !current.IsDir() {
		return fmt.Errorf("content: tree directory changed while being captured")
	}
	return nil
}

func (r *Repository) captureTreeFile(ctx context.Context, root treeCaptureRoot, name string, expected fs.FileInfo) (object Object, err error) {
	file, err := root.open(name)
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
	for _, entry := range entries {
		if _, err := treeHostPath(".", entry.segments); err != nil {
			return err
		}
	}
	filesystem := r.treeFilesystem()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	cleanDestination := filepath.Clean(destination)
	destinationName := filepath.Base(cleanDestination)
	parentPath := filepath.Dir(cleanDestination)
	parent, err := filesystem.openRoot(parentPath)
	if err != nil {
		return fmt.Errorf("content: open tree materialization parent: %w", err)
	}
	var destinationRoot treeCaptureRoot
	createdName := ""
	rollbackArmed := false
	defer func() {
		if destinationRoot != nil {
			if closeErr := destinationRoot.close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("content: close tree materialization destination: %w", closeErr))
			}
			destinationRoot = nil
		}
		removedBeforeParentClose := false
		if err != nil && rollbackArmed && createdName != "" {
			cleanupErr := parent.removeAll(createdName)
			removedBeforeParentClose = cleanupErr == nil
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("content: remove failed tree materialization: %w", cleanupErr))
			}
		}
		if closeErr := parent.close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("content: close tree materialization parent: %w", closeErr))
			if rollbackArmed && createdName != "" && !removedBeforeParentClose {
				cleanupParent, openErr := filesystem.openRoot(parentPath)
				if openErr != nil {
					err = errors.Join(err, fmt.Errorf("content: reopen tree materialization parent for cleanup: %w", openErr))
					return
				}
				if cleanupErr := cleanupParent.removeAll(createdName); cleanupErr != nil {
					err = errors.Join(err, fmt.Errorf("content: remove failed tree materialization: %w", cleanupErr))
				}
				if cleanupCloseErr := cleanupParent.close(); cleanupCloseErr != nil {
					err = errors.Join(err, fmt.Errorf("content: close tree materialization cleanup parent: %w", cleanupCloseErr))
				}
			}
		}
	}()
	if err := requireAbsentTreeName(parent, destinationName); err != nil {
		return err
	}
	createdInfo, err := parent.mkdir(destinationName, 0o700)
	if err != nil {
		return fmt.Errorf("content: create tree materialization destination: %w", err)
	}
	createdName, exact, err := identifyCreatedTreeName(parent, destinationName, createdInfo)
	if createdName != "" {
		rollbackArmed = true
	}
	if err != nil {
		return err
	}
	if !exact {
		return fmt.Errorf("content: concrete filesystem did not reproduce exact tree entry name")
	}
	destinationRoot, err = parent.openRoot(createdName)
	if err != nil {
		return fmt.Errorf("content: open created tree materialization destination: %w", err)
	}
	openedInfo, err := destinationRoot.lstat(".")
	if err != nil {
		return fmt.Errorf("content: inspect created tree materialization destination: %w", err)
	}
	if !os.SameFile(createdInfo, openedInfo) {
		return fmt.Errorf("content: created tree materialization destination identity changed")
	}
	directories := map[string]fs.FileInfo{treeSegmentsKey(nil): createdInfo}
	for _, entry := range entries {
		if entry.kind != directoryEntry {
			continue
		}
		if err := materializeTreeDirectory(ctx, destinationRoot, directories, entry); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if entry.kind != fileEntry && entry.kind != executableEntry {
			continue
		}
		if err := r.materializeTreeFile(ctx, destinationRoot, directories, entry); err != nil {
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
		if err := materializeTreeSymlink(destinationRoot, directories, entry); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	return nil
}

func materializeTreeDirectory(ctx context.Context, root treeCaptureRoot, directories map[string]fs.FileInfo, entry treeEntry) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	parent, err := openMaterializedTreeDirectory(root, directories, entry.segments[:len(entry.segments)-1])
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := parent.close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("content: close tree directory parent: %w", closeErr))
		}
	}()
	name := entry.segments[len(entry.segments)-1]
	if err := requireAbsentTreeName(parent, name); err != nil {
		return err
	}
	created, err := parent.mkdir(name, 0o700)
	if err != nil {
		return fmt.Errorf("content: create tree directory: %w", err)
	}
	_, exact, err := identifyCreatedTreeName(parent, name, created)
	if err != nil {
		return err
	}
	if !exact {
		return fmt.Errorf("content: concrete filesystem did not reproduce exact tree entry name")
	}
	child, err := parent.openRoot(name)
	if err != nil {
		return fmt.Errorf("content: open created tree directory: %w", err)
	}
	opened, inspectErr := child.lstat(".")
	closeErr := child.close()
	if inspectErr != nil {
		return errors.Join(fmt.Errorf("content: inspect created tree directory: %w", inspectErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("content: close created tree directory: %w", closeErr)
	}
	if !os.SameFile(created, opened) {
		return fmt.Errorf("content: created tree directory identity changed")
	}
	directories[treeSegmentsKey(entry.segments)] = created
	return nil
}

func (r *Repository) materializeTreeFile(ctx context.Context, root treeCaptureRoot, directories map[string]fs.FileInfo, entry treeEntry) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize tree: %w", err)
	}
	parent, err := openMaterializedTreeDirectory(root, directories, entry.segments[:len(entry.segments)-1])
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := parent.close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("content: close tree file parent: %w", closeErr))
		}
	}()
	name := entry.segments[len(entry.segments)-1]
	if err := requireAbsentTreeName(parent, name); err != nil {
		return err
	}
	temporary, err := parent.createTemp(".dawn-tree-")
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
			if removeErr := parent.remove(temporaryName); removeErr != nil {
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
	created, err := parent.link(temporaryName, name)
	if err != nil {
		return fmt.Errorf("content: publish tree file: %w", err)
	}
	if err := parent.remove(temporaryName); err != nil {
		return fmt.Errorf("content: remove published tree file temporary: %w", err)
	}
	temporaryName = ""
	_, exact, err := identifyCreatedTreeName(parent, name, created)
	if err != nil {
		return err
	}
	if !exact {
		return fmt.Errorf("content: concrete filesystem did not reproduce exact tree entry name")
	}
	return nil
}

func materializeTreeSymlink(root treeCaptureRoot, directories map[string]fs.FileInfo, entry treeEntry) (err error) {
	parent, err := openMaterializedTreeDirectory(root, directories, entry.segments[:len(entry.segments)-1])
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := parent.close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("content: close tree symlink parent: %w", closeErr))
		}
	}()
	name := entry.segments[len(entry.segments)-1]
	if err := requireAbsentTreeName(parent, name); err != nil {
		return err
	}
	created, err := parent.symlink(entry.target, name)
	if err != nil {
		return fmt.Errorf("content: create tree symlink: %w", err)
	}
	_, exact, err := identifyCreatedTreeName(parent, name, created)
	if err != nil {
		return err
	}
	if !exact {
		return fmt.Errorf("content: concrete filesystem did not reproduce exact tree entry name")
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

func requireAbsentTreeName(root treeCaptureRoot, name string) error {
	if _, err := root.lstat(name); err == nil {
		return fmt.Errorf("content: tree materialization path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("content: inspect tree materialization path: %w", err)
	}
	return nil
}

func identifyCreatedTreeName(root treeCaptureRoot, requested string, created fs.FileInfo) (actual string, exact bool, err error) {
	entries, err := root.readDir(".")
	if err != nil {
		return "", false, fmt.Errorf("content: enumerate created tree entry parent: %w", err)
	}
	matches := 0
	for _, entry := range entries {
		info, statErr := root.lstat(entry.Name())
		if statErr != nil {
			return actual, false, fmt.Errorf("content: inspect enumerated tree entry: %w", statErr)
		}
		if !os.SameFile(created, info) {
			continue
		}
		matches++
		if actual == "" {
			actual = entry.Name()
		}
		if entry.Name() == requested {
			exact = true
		}
	}
	if actual == "" {
		return "", false, fmt.Errorf("content: concrete filesystem did not retain created tree entry identity")
	}
	if matches != 1 {
		return actual, false, fmt.Errorf("content: concrete filesystem reproduced created tree entry under multiple names")
	}
	return actual, exact, nil
}

func openMaterializedTreeDirectory(root treeCaptureRoot, directories map[string]fs.FileInfo, segments []string) (treeCaptureRoot, error) {
	current, err := root.openRoot(".")
	if err != nil {
		return nil, fmt.Errorf("content: pin tree materialization destination: %w", err)
	}
	openedSegments := make([]string, 0, len(segments))
	for _, segment := range segments {
		child, openErr := current.openRoot(segment)
		closeErr := current.close()
		if openErr != nil {
			return nil, errors.Join(fmt.Errorf("content: pin tree directory: %w", openErr), closeErr)
		}
		if closeErr != nil {
			_ = child.close()
			return nil, fmt.Errorf("content: close tree directory traversal handle: %w", closeErr)
		}
		openedSegments = append(openedSegments, segment)
		info, inspectErr := child.lstat(".")
		if inspectErr != nil {
			_ = child.close()
			return nil, fmt.Errorf("content: inspect pinned tree directory: %w", inspectErr)
		}
		expected, exists := directories[treeSegmentsKey(openedSegments)]
		if !exists || !os.SameFile(expected, info) {
			_ = child.close()
			return nil, fmt.Errorf("content: pinned tree directory identity changed")
		}
		current = child
	}
	return current, nil
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
