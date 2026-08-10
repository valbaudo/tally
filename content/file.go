package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const mediaSniffSize = 512

// Match the standard library's bounded tolerance for misbehaving readers that
// transiently return no bytes and no error.
const maxConsecutiveEmptyReads = 100

// Repository manages semantic files whose immutable bytes are held by a Store.
type Repository struct {
	store  Store
	treeFS treeFilesystem
}

type materializationTemp interface {
	io.Writer
	Name() string
	Close() error
}

var createMaterializationTemp = func(directory, pattern string) (materializationTemp, error) {
	return os.CreateTemp(directory, pattern)
}

var removeMaterializationTemp = os.Remove

// NewRepository constructs a file repository backed by store.
func NewRepository(store Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("content: file repository store must not be nil")
	}
	return &Repository{store: store, treeFS: osTreeFilesystem{}}, nil
}

// IngestFile streams source into the repository's Store and records immutable
// semantic file facts. name is logical metadata, not a filesystem path.
func (r *Repository) IngestFile(ctx context.Context, name, explicitMedia string, source io.Reader) (File, error) {
	if r == nil || r.store == nil {
		return File{}, fmt.Errorf("content: file repository is invalid")
	}
	if name == "" {
		return File{}, fmt.Errorf("content: file name must not be empty")
	}
	if source == nil {
		return File{}, fmt.Errorf("content: file source must not be nil")
	}

	if explicitMedia != "" {
		explicit, err := canonicalConcreteMedia(explicitMedia)
		if err != nil {
			return File{}, err
		}
		return r.storeFile(ctx, name, explicit, source)
	}

	prefix, terminalErr := readMediaPrefix(source)
	media, err := canonicalConcreteMedia(http.DetectContentType(prefix))
	if err != nil {
		return File{}, fmt.Errorf("content: detect file media: %w", err)
	}
	return r.storeFile(ctx, name, media, sourceWithPrefix(prefix, source, terminalErr))
}

func (r *Repository) storeFile(ctx context.Context, name, media string, source io.Reader) (File, error) {
	object, err := r.store.Put(ctx, source)
	if err != nil {
		return File{}, fmt.Errorf("content: ingest file: %w", err)
	}
	file, err := NewFile(object.Digest(), object.Size(), name, media)
	if err != nil {
		return File{}, fmt.Errorf("content: ingest file: %w", err)
	}
	return file, nil
}

// CopyFile copies committed file bytes to destination, rejecting store data
// whose size or digest does not match file's semantic identity.
func (r *Repository) CopyFile(ctx context.Context, file File, destination io.Writer) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("content: file repository is invalid")
	}
	if !file.Valid() {
		return fmt.Errorf("content: file is invalid")
	}
	if destination == nil {
		return fmt.Errorf("content: file destination must not be nil")
	}
	return r.copyVerified(ctx, file, destination)
}

// MaterializeFile copies a file to the caller's absent runtime-owned target.
// It writes a same-directory temporary file and atomically publishes it only
// after the copied bytes match file's authoritative content identity.
func (r *Repository) MaterializeFile(ctx context.Context, file File, target string) (err error) {
	if r == nil || r.store == nil {
		return fmt.Errorf("content: file repository is invalid")
	}
	if !file.Valid() {
		return fmt.Errorf("content: file is invalid")
	}
	if target == "" {
		return fmt.Errorf("content: materialization target must not be empty")
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		return fmt.Errorf("content: materialization target already exists")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("content: inspect materialization target: %w", statErr)
	}

	temporary, err := createMaterializationTemp(filepath.Dir(target), ".dawn-materialize-*")
	if err != nil {
		return fmt.Errorf("content: create materialization temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	temporaryClosed := false
	published := false
	defer func() {
		var cleanupErr error
		if !temporaryClosed {
			temporaryClosed = true
			if closeErr := temporary.Close(); closeErr != nil {
				cleanupErr = fmt.Errorf("content: close materialization temporary file during cleanup: %w", closeErr)
			}
		}
		if temporaryName != "" {
			if removeErr := removeMaterializationTemp(temporaryName); removeErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("content: remove materialization temporary file: %w", removeErr))
			}
		}
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		if err != nil && published {
			if rollbackErr := os.Remove(target); rollbackErr != nil && !errors.Is(rollbackErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("content: remove failed materialized file: %w", rollbackErr))
			}
		}
	}()

	if err := r.copyVerified(ctx, file, temporary); err != nil {
		return fmt.Errorf("content: materialize file: %w", err)
	}
	temporaryClosed = true
	if closeErr := temporary.Close(); closeErr != nil {
		return fmt.Errorf("content: close materialization temporary file: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize file: %w", err)
	}
	if err := os.Link(temporaryName, target); err != nil {
		return fmt.Errorf("content: publish materialized file: %w", err)
	}
	published = true
	return nil
}

func (r *Repository) copyVerified(ctx context.Context, file File, destination io.Writer) error {
	hasher := sha256.New()
	size, err := r.store.Copy(ctx, file.Digest(), io.MultiWriter(destination, hasher))
	if err != nil {
		return fmt.Errorf("content: copy file: %w", err)
	}
	if size != file.Size() {
		return integrityError("file content length does not match semantic record")
	}
	if !file.Digest().Equal(digestFromHash(hasher)) {
		return integrityError("file content does not match semantic record")
	}
	return nil
}

func readMediaPrefix(source io.Reader) ([]byte, error) {
	prefix := make([]byte, mediaSniffSize)
	n := 0
	emptyReads := 0
	for n < len(prefix) {
		read, err := source.Read(prefix[n:])
		n += read
		if err != nil {
			return prefix[:n], err
		}
		if read == 0 {
			emptyReads++
			if emptyReads == maxConsecutiveEmptyReads {
				return prefix[:n], io.ErrNoProgress
			}
			continue
		}
		emptyReads = 0
	}
	return prefix, nil
}

func sourceWithPrefix(prefix []byte, source io.Reader, terminalErr error) io.Reader {
	if terminalErr == nil {
		return io.MultiReader(bytes.NewReader(prefix), source)
	}
	if errors.Is(terminalErr, io.EOF) {
		return bytes.NewReader(prefix)
	}
	return io.MultiReader(bytes.NewReader(prefix), readerError{err: terminalErr})
}

type readerError struct {
	err error
}

func (r readerError) Read([]byte) (int, error) {
	return 0, r.err
}

func canonicalConcreteMedia(media string) (string, error) {
	if media == "" {
		return "", fmt.Errorf("content: file media must not be empty")
	}
	typeName, parameters, err := mime.ParseMediaType(media)
	if err != nil {
		return "", fmt.Errorf("content: invalid file media %q: %w", media, err)
	}
	major, subtype, hasSubtype := strings.Cut(typeName, "/")
	if !hasSubtype || major == "" || subtype == "" || strings.Contains(subtype, "/") {
		return "", fmt.Errorf("content: file media %q must have a nonempty type and subtype", media)
	}
	if strings.Contains(major, "*") || strings.Contains(subtype, "*") {
		return "", fmt.Errorf("content: file media %q must be concrete", media)
	}
	formatted := mime.FormatMediaType(typeName, parameters)
	if formatted == "" {
		return "", fmt.Errorf("content: invalid file media %q", media)
	}
	return formatted, nil
}
