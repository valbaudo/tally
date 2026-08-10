package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FS is a filesystem-backed Store.
type FS struct {
	root string
}

// OpenFS opens, or creates, a content store rooted at root.
func OpenFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("content: create store: %w", err)
	}
	return &FS{root: root}, nil
}

// Put streams content to a temporary file before publishing it atomically.
func (s *FS) Put(ctx context.Context, source io.Reader) (object Object, err error) {
	if err := ctx.Err(); err != nil {
		return Object{}, fmt.Errorf("content: put: %w", err)
	}
	temporary, err := os.CreateTemp(s.root, ".content-*")
	if err != nil {
		return Object{}, fmt.Errorf("content: create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		if temporaryName != "" {
			_ = os.Remove(temporaryName)
		}
	}()

	hasher := sha256.New()
	size, err := copyContext(ctx, io.MultiWriter(temporary, hasher), source)
	if err != nil {
		_ = temporary.Close()
		return Object{}, fmt.Errorf("content: put: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Object{}, fmt.Errorf("content: sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Object{}, fmt.Errorf("content: close temporary file: %w", err)
	}

	digest := digestFromHash(hasher)
	finalName := filepath.Join(s.root, digestFilename(digest))
	if _, statErr := os.Stat(finalName); statErr == nil {
		if err := verifyFile(ctx, finalName, digest); err != nil {
			return Object{}, err
		}
		return Object{digest: digest, size: size}, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Object{}, fmt.Errorf("content: inspect existing object: %w", statErr)
	}
	if err := os.Rename(temporaryName, finalName); err != nil {
		return Object{}, fmt.Errorf("content: publish content: %w", err)
	}
	temporaryName = ""
	return Object{digest: digest, size: size}, nil
}

// Copy writes a stored object to destination while checking its identity.
func (s *FS) Copy(ctx context.Context, digest Digest, destination io.Writer) (int64, error) {
	file, err := os.Open(filepath.Join(s.root, digestFilename(digest)))
	if errors.Is(err, os.ErrNotExist) {
		return 0, integrityError("missing content")
	}
	if err != nil {
		return 0, fmt.Errorf("content: open: %w", err)
	}
	defer file.Close()

	hasher := sha256.New()
	size, err := copyContext(ctx, io.MultiWriter(destination, hasher), file)
	if err != nil {
		return size, fmt.Errorf("content: copy: %w", err)
	}
	if !digest.Equal(digestFromHash(hasher)) {
		return size, integrityError("corrupt content")
	}
	return size, nil
}

func verifyFile(ctx context.Context, name string, digest Digest) error {
	file, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("content: open existing object: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := copyContext(ctx, hasher, file); err != nil {
		return fmt.Errorf("content: verify existing object: %w", err)
	}
	if !digest.Equal(digestFromHash(hasher)) {
		return integrityError("corrupt existing content")
	}
	return nil
}

func digestFilename(digest Digest) string {
	return hex.EncodeToString(digest[:])
}

func integrityError(problem string) error {
	return fmt.Errorf("content integrity error: %s", problem)
}
