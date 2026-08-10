package content

import (
	"fmt"
)

// File describes immutable file content and its logical metadata.
// Construction validates these facts but does not assert that the content is
// present in a Store.
type File struct {
	digest Digest
	size   int64
	name   string
	media  string
}

// NewFile constructs a semantic file record.
func NewFile(digest Digest, size int64, name, media string) (File, error) {
	if digest == (Digest{}) || size < 0 {
		return File{}, fmt.Errorf("content: file content facts are invalid")
	}
	if name == "" {
		return File{}, fmt.Errorf("content: file name must not be empty")
	}
	canonical, err := canonicalConcreteMedia(media)
	if err != nil {
		return File{}, err
	}
	return File{digest: digest, size: size, name: name, media: canonical}, nil
}

// Valid reports whether f contains valid semantic file facts.
func (f File) Valid() bool {
	if f.digest == (Digest{}) || f.size < 0 || f.name == "" || f.media == "" {
		return false
	}
	canonical, err := canonicalConcreteMedia(f.media)
	return err == nil && canonical == f.media
}

// Digest returns the file's content identity.
func (f File) Digest() Digest { return f.digest }

// Size returns the file content length.
func (f File) Size() int64 { return f.size }

// Name returns the logical file name as exact bytes in a Go string.
func (f File) Name() string { return f.name }

// Media returns the exact declared media metadata.
func (f File) Media() string { return f.media }

// Equal reports whether both records contain the same semantic facts.
func (f File) Equal(other File) bool {
	return f.Valid() && other.Valid() && f.digest.Equal(other.digest) && f.size == other.size && f.name == other.name && f.media == other.media
}

// Tree describes an immutable tree by the digest of its canonical tree data.
// Construction does not assert that the referenced bytes are in a Store.
type Tree struct {
	digest Digest
}

// NewTree constructs a semantic tree record.
func NewTree(digest Digest) (Tree, error) {
	if digest == (Digest{}) {
		return Tree{}, fmt.Errorf("content: tree digest is zero")
	}
	return Tree{digest: digest}, nil
}

// Valid reports whether t has a content identity.
func (t Tree) Valid() bool { return t.digest != (Digest{}) }

// Digest returns the tree's canonical content identity.
func (t Tree) Digest() Digest { return t.digest }

// Equal reports whether both trees have the same content identity.
func (t Tree) Equal(other Tree) bool {
	return t.Valid() && other.Valid() && t.digest.Equal(other.digest)
}
