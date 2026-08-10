package value

import (
	"bytes"
	"context"
	"fmt"

	"github.com/valbaudo/dawn/content"
)

// Store persists only the private canonical encoding of runtime values.
type Store struct {
	contents content.Store
}

// NewStore constructs a value ownership boundary over a content store.
func NewStore(contents content.Store) *Store {
	return &Store{contents: contents}
}

// Put stores a valid value's canonical bytes.
func (s *Store) Put(ctx context.Context, value Value) (content.Object, error) {
	if s == nil || s.contents == nil {
		return content.Object{}, fmt.Errorf("value store: content store is nil")
	}
	canonical := value.Canonical()
	if canonical == nil {
		return content.Object{}, fmt.Errorf("value store: invalid value")
	}
	object, err := s.contents.Put(ctx, bytes.NewReader(canonical))
	if err != nil {
		return content.Object{}, fmt.Errorf("value store: put: %w", err)
	}
	return object, nil
}

// Get loads and accepts a value only after content integrity and canonical
// parsing both succeed.
func (s *Store) Get(ctx context.Context, digest content.Digest) (Value, error) {
	if s == nil || s.contents == nil {
		return Value{}, fmt.Errorf("value store: content store is nil")
	}
	var canonical bytes.Buffer
	if _, err := s.contents.Copy(ctx, digest, &canonical); err != nil {
		return Value{}, fmt.Errorf("value store: get: %w", err)
	}
	value, err := ParseCanonical(canonical.Bytes())
	if err != nil {
		return Value{}, fmt.Errorf("value store: invalid stored value: %w", err)
	}
	return value, nil
}
