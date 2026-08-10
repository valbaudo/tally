package value

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/dawn/content"
)

func TestValueStoreRoundTripByTypedDigest(t *testing.T) {
	store := NewStore(content.NewMemory())
	value := mustObject(t,
		mustEntry(t, "name", NewString("dawn")),
		mustEntry(t, "items", NewList(mustInteger(t, "1"), NewBoolean(true))),
	)
	object, err := store.Put(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if !object.Valid() || object.Size() != int64(len(value.Canonical())) {
		t.Fatalf("stored object = %#v", object)
	}
	loaded, err := store.Get(context.Background(), object.Digest())
	if err != nil {
		t.Fatal(err)
	}
	if !value.Equal(loaded) {
		t.Fatal("stored value changed")
	}
}

func TestValueStoreRejectsMalformedStoredBytes(t *testing.T) {
	contents := content.NewMemory()
	object, err := contents.Put(context.Background(), bytes.NewReader([]byte("not a dawn value")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(contents).Get(context.Background(), object.Digest()); err == nil {
		t.Fatal("Get accepted malformed stored bytes")
	}
}

func TestValueStorePropagatesMissingAndCorruptContentIntegrityErrors(t *testing.T) {
	root := t.TempDir()
	contents, err := content.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(contents)
	object, err := store.Put(context.Background(), NewString("stored"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, hex.EncodeToString(object.Digest().Bytes()))
	if err := os.WriteFile(path, []byte("dawn.value/1\x01\x01x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), object.Digest()); err == nil {
		t.Fatal("Get returned corrupted content")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), object.Digest()); err == nil {
		t.Fatal("Get returned missing content")
	}

	missing := content.Digest(sha256.Sum256([]byte("never stored")))
	if _, err := store.Get(context.Background(), missing); err == nil {
		t.Fatal("Get returned an unknown digest")
	}
}

func TestValueStoreRejectsInvalidValueAndNilContentStore(t *testing.T) {
	if _, err := NewStore(content.NewMemory()).Put(context.Background(), Value{}); err == nil {
		t.Fatal("Put accepted a zero value")
	}
	store := NewStore(nil)
	if _, err := store.Put(context.Background(), NewNull()); err == nil {
		t.Fatal("nil-backed Put succeeded")
	}
	if _, err := store.Get(context.Background(), content.Digest{}); err == nil {
		t.Fatal("nil-backed Get succeeded")
	}
}

func TestValueStoreReturnsErrorsForTypedNilBuiltInContentStores(t *testing.T) {
	for _, contents := range []content.Store{(*content.Memory)(nil), (*content.FS)(nil)} {
		store := NewStore(contents)
		if _, err := store.Put(context.Background(), NewNull()); err == nil {
			t.Fatal("Put through typed-nil built-in store succeeded")
		}
		if _, err := store.Get(context.Background(), content.Digest(sha256.Sum256([]byte("missing")))); err == nil {
			t.Fatal("Get through typed-nil built-in store succeeded")
		}
	}
}
