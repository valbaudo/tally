package content

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFSPersistsObjectsAcrossOpen(t *testing.T) {
	root := t.TempDir()
	first, err := OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	object, err := first.Put(context.Background(), bytes.NewReader([]byte("durable bytes")))
	if err != nil {
		t.Fatal(err)
	}

	second, err := OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := second.Copy(context.Background(), object.Digest(), &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "durable bytes" {
		t.Fatalf("reopened store returned %q, want durable bytes", got.String())
	}
}

func TestFSRejectsCorruptExistingObject(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("bytes that are later corrupted")
	object, err := store.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hex.EncodeToString(object.Digest().Bytes())), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Copy(context.Background(), object.Digest(), io.Discard); err == nil {
		t.Fatal("Copy returned corrupted bytes")
	}
	if _, err := store.Put(context.Background(), bytes.NewReader(content)); err == nil {
		t.Fatal("Put accepted a corrupt existing object")
	}
}
