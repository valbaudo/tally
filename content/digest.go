package content

import "crypto/sha256"

// Digest identifies immutable content by its SHA-256 bytes.
type Digest [sha256.Size]byte

// Bytes returns a copy of the digest bytes.
func (d Digest) Bytes() []byte {
	return append([]byte(nil), d[:]...)
}

// Equal reports whether two digests identify the same content.
func (d Digest) Equal(other Digest) bool {
	return d == other
}
