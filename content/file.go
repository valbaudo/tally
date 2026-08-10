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

// Repository manages semantic files whose immutable bytes are held by a Store.
type Repository struct {
	store Store
}

// NewRepository constructs a file repository backed by store.
func NewRepository(store Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("content: file repository store must not be nil")
	}
	return &Repository{store: store}, nil
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

	explicit, err := canonicalMedia(explicitMedia)
	if err != nil {
		return File{}, err
	}
	prefix, err := readMediaPrefix(source)
	if err != nil {
		return File{}, fmt.Errorf("content: read file prefix: %w", err)
	}
	media := explicit
	if media == "" {
		media, err = canonicalMedia(http.DetectContentType(prefix))
		if err != nil {
			return File{}, fmt.Errorf("content: detect file media: %w", err)
		}
	}

	object, err := r.store.Put(ctx, io.MultiReader(bytes.NewReader(prefix), source))
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
// It writes a same-directory temporary file and atomically renames it only
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

	temporary, err := os.CreateTemp(filepath.Dir(target), ".dawn-materialize-*")
	if err != nil {
		return fmt.Errorf("content: create materialization temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		if temporaryName != "" {
			_ = temporary.Close()
			_ = os.Remove(temporaryName)
		}
	}()

	if err := r.copyVerified(ctx, file, temporary); err != nil {
		return fmt.Errorf("content: materialize file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("content: close materialization temporary file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("content: materialize file: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("content: publish materialized file: %w", err)
	}
	temporaryName = ""
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
	n, err := io.ReadFull(source, prefix)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return prefix[:n], nil
}

func canonicalMedia(media string) (string, error) {
	if media == "" {
		return "", nil
	}
	typeName, parameters, err := mime.ParseMediaType(media)
	if err != nil {
		return "", fmt.Errorf("content: invalid file media %q: %w", media, err)
	}
	if strings.Contains(typeName, "*") {
		return "", fmt.Errorf("content: file media %q must be concrete", media)
	}
	formatted := mime.FormatMediaType(typeName, parameters)
	if formatted == "" {
		return "", fmt.Errorf("content: invalid file media %q", media)
	}
	return formatted, nil
}
