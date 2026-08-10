package content

import (
	"context"
	"io"
)

// Object describes immutable content held by a Store.
type Object struct {
	digest Digest
	size   int64
}

// Digest returns the content identity.
func (o Object) Digest() Digest {
	return o.digest
}

// Size returns the number of content bytes.
func (o Object) Size() int64 {
	return o.size
}

// Valid reports whether the object has a content identity.
func (o Object) Valid() bool {
	return o.size >= 0 && o.digest != (Digest{})
}

// Equal reports whether two objects describe the same content.
func (o Object) Equal(other Object) bool {
	return o.digest.Equal(other.digest) && o.size == other.size
}

// Store holds immutable content addressed by a Digest.
type Store interface {
	Put(context.Context, io.Reader) (Object, error)
	Copy(context.Context, Digest, io.Writer) (int64, error)
}
