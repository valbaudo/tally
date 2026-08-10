package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"sync"
)

// Memory is an in-memory Store.
type Memory struct {
	mu       sync.RWMutex
	contents map[Digest][]byte
}

// NewMemory returns an empty in-memory content store.
func NewMemory() *Memory {
	return &Memory{contents: make(map[Digest][]byte)}
}

// Put streams content into memory and publishes it only after a complete read.
func (m *Memory) Put(ctx context.Context, source io.Reader) (Object, error) {
	if m == nil {
		return Object{}, fmt.Errorf("content: memory store is nil")
	}
	if ctx == nil {
		return Object{}, fmt.Errorf("content: put context must not be nil")
	}
	if source == nil {
		return Object{}, fmt.Errorf("content: put source must not be nil")
	}
	var data bytes.Buffer
	hasher := sha256.New()
	size, err := copyContext(ctx, io.MultiWriter(&data, hasher), source)
	if err != nil {
		return Object{}, fmt.Errorf("content: put: %w", err)
	}
	digest := digestFromHash(hasher)
	stored := append([]byte(nil), data.Bytes()...)

	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return Object{}, fmt.Errorf("content: put: %w", err)
	}
	if m.contents == nil {
		m.contents = make(map[Digest][]byte)
	}
	if _, exists := m.contents[digest]; !exists {
		m.contents[digest] = stored
	}
	m.mu.Unlock()

	return Object{digest: digest, size: size}, nil
}

// Copy writes content for digest while checking its stored identity.
func (m *Memory) Copy(ctx context.Context, digest Digest, destination io.Writer) (int64, error) {
	if m == nil {
		return 0, fmt.Errorf("content: memory store is nil")
	}
	if ctx == nil {
		return 0, fmt.Errorf("content: copy context must not be nil")
	}
	if destination == nil {
		return 0, fmt.Errorf("content: copy destination must not be nil")
	}
	m.mu.RLock()
	stored, exists := m.contents[digest]
	snapshot := append([]byte(nil), stored...)
	m.mu.RUnlock()
	if !exists {
		return 0, integrityError("missing content")
	}

	hasher := sha256.New()
	size, err := copyContext(ctx, io.MultiWriter(destination, hasher), bytes.NewReader(snapshot))
	if err != nil {
		return size, fmt.Errorf("content: copy: %w", err)
	}
	if !digest.Equal(digestFromHash(hasher)) {
		return size, integrityError("corrupt content")
	}
	return size, nil
}

func digestFromHash(hasher hash.Hash) Digest {
	var digest Digest
	copy(digest[:], hasher.Sum(nil))
	return digest
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("context must not be nil")
	}
	if destination == nil {
		return 0, fmt.Errorf("destination must not be nil")
	}
	if source == nil {
		return 0, fmt.Errorf("source must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	size, err := io.Copy(contextWriter{ctx: ctx, writer: destination}, source)
	if err != nil {
		return size, err
	}
	if err := ctx.Err(); err != nil {
		return size, err
	}
	return size, nil
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(data)
}
