package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	storeContract(t, func(t *testing.T) Store {
		t.Helper()
		return NewMemory()
	})
}

func TestMemoryPutChecksCancellationBeforePublication(t *testing.T) {
	store := NewMemory()
	content := []byte("canceled after streaming")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store.mu.Lock()
	locked := true
	defer func() {
		if locked {
			store.mu.Unlock()
		}
	}()
	streamed := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, &signalEOFReader{reader: bytes.NewReader(content), streamed: streamed})
		result <- err
	}()
	<-streamed
	cancel()
	store.mu.Unlock()
	locked = false

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Put cancellation error = %v, want context.Canceled", err)
	}
	digest := Digest(sha256.Sum256(content))
	if _, err := store.Copy(context.Background(), digest, io.Discard); err == nil {
		t.Fatal("canceled Put published content after streaming")
	}
}

func TestFSStore(t *testing.T) {
	storeContract(t, func(t *testing.T) Store {
		t.Helper()
		store, err := OpenFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestBuiltInStoresRejectTypedNilReceiversAndNilStreams(t *testing.T) {
	ctx := context.Background()
	digest := Digest(sha256.Sum256([]byte("missing")))
	for _, test := range []struct {
		name  string
		store Store
	}{
		{"Memory", (*Memory)(nil)},
		{"FS", (*FS)(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.store.Put(ctx, bytes.NewReader([]byte("bytes"))); err == nil {
				t.Fatal("typed-nil Put succeeded")
			}
			if _, err := test.store.Copy(ctx, digest, io.Discard); err == nil {
				t.Fatal("typed-nil Copy succeeded")
			}

			repository, err := NewRepository(test.store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.IngestFile(ctx, "logical.bin", "application/octet-stream", bytes.NewReader([]byte("bytes"))); err == nil {
				t.Fatal("repository backed by typed nil store ingested a file")
			}
			file, err := NewFile(digest, 7, "logical.bin", "application/octet-stream")
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.CopyFile(ctx, file, io.Discard); err == nil {
				t.Fatal("repository backed by typed nil store copied a file")
			}
		})
	}

	memory := NewMemory()
	if _, err := memory.Put(ctx, nil); err == nil {
		t.Fatal("Memory.Put accepted a nil reader")
	}
	object, err := memory.Put(ctx, bytes.NewReader([]byte("stored")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Copy(ctx, object.Digest(), nil); err == nil {
		t.Fatal("Memory.Copy accepted a nil writer")
	}
	filesystem, err := OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Put(ctx, nil); err == nil {
		t.Fatal("FS.Put accepted a nil reader")
	}
	object, err = filesystem.Put(ctx, bytes.NewReader([]byte("stored")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Copy(ctx, object.Digest(), nil); err == nil {
		t.Fatal("FS.Copy accepted a nil writer")
	}
}

func storeContract(t *testing.T, open func(*testing.T) Store) {
	t.Helper()
	t.Run("identity and defensive reads", func(t *testing.T) {
		store := open(t)
		want := bytes.Repeat([]byte("dawn\x00"), 64*1024)
		first, err := store.Put(context.Background(), bytes.NewReader(want))
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.Put(context.Background(), bytes.NewReader(want))
		if err != nil {
			t.Fatal(err)
		}
		if !first.Equal(second) || first.Size() != int64(len(want)) || !first.Valid() {
			t.Fatal("identity changed")
		}

		bytesFromDigest := first.Digest().Bytes()
		bytesFromDigest[0] ^= 0xff
		if bytes.Equal(bytesFromDigest, first.Digest().Bytes()) {
			t.Fatal("Digest.Bytes returned public storage")
		}

		var got bytes.Buffer
		if _, err := store.Copy(context.Background(), first.Digest(), &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatal("content changed")
		}
	})

	t.Run("stores empty content", func(t *testing.T) {
		store := open(t)
		object, err := store.Put(context.Background(), bytes.NewReader(nil))
		if err != nil {
			t.Fatal(err)
		}
		if !object.Valid() || object.Size() != 0 {
			t.Fatalf("empty object = %#v, want a valid zero-byte object", object)
		}
		var got bytes.Buffer
		n, err := store.Copy(context.Background(), object.Digest(), &got)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 || got.Len() != 0 {
			t.Fatalf("empty Copy = (%d, %q), want (0, empty)", n, got.Bytes())
		}
	})

	t.Run("cancellation during put does not publish content", func(t *testing.T) {
		store := open(t)
		ctx, cancel := context.WithCancel(context.Background())
		prefix := []byte("unpublished partial content")
		_, err := store.Put(ctx, &cancelAfterRead{data: prefix, cancel: cancel})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Put cancellation error = %v, want context.Canceled", err)
		}
		digest := Digest(sha256.Sum256(prefix))
		if _, err := store.Copy(context.Background(), digest, io.Discard); err == nil {
			t.Fatal("failed Put made its partial digest readable")
		}
	})

	t.Run("cancellation during copy reports cancellation", func(t *testing.T) {
		store := open(t)
		object, err := store.Put(context.Background(), bytes.NewReader(bytes.Repeat([]byte("copy"), 32*1024)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		_, err = store.Copy(ctx, object.Digest(), &cancelAfterWrite{cancel: cancel})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Copy cancellation error = %v, want context.Canceled", err)
		}
	})

	t.Run("concurrent identical puts have one identity", func(t *testing.T) {
		store := open(t)
		want := bytes.Repeat([]byte("same"), 16*1024)
		objects := make([]Object, 16)
		errs := make([]error, len(objects))
		var group sync.WaitGroup
		for i := range objects {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				objects[i], errs[i] = store.Put(context.Background(), bytes.NewReader(want))
			}(i)
		}
		group.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent Put %d: %v", i, err)
			}
			if !objects[0].Equal(objects[i]) {
				t.Fatalf("concurrent Put %d returned a different object", i)
			}
		}
	})

	t.Run("missing content is rejected", func(t *testing.T) {
		store := open(t)
		missing := Digest(sha256.Sum256([]byte("never stored")))
		if _, err := store.Copy(context.Background(), missing, io.Discard); err == nil {
			t.Fatal("Copy of missing content succeeded")
		}
	})

	t.Run("writer failure after bytes is returned", func(t *testing.T) {
		store := open(t)
		object, err := store.Put(context.Background(), bytes.NewReader([]byte("writer failure must propagate")))
		if err != nil {
			t.Fatal(err)
		}
		writer := failAfterWrite{remaining: 7}
		if _, err := store.Copy(context.Background(), object.Digest(), &writer); !errors.Is(err, errWriteFailed) {
			t.Fatalf("Copy writer error = %v, want injected failure", err)
		}
		if writer.written == 0 {
			t.Fatal("failing writer received no bytes")
		}
	})

	t.Run("reader failure does not publish content", func(t *testing.T) {
		store := open(t)
		prefix := []byte("reader failed after this")
		_, err := store.Put(context.Background(), &failAfterRead{data: prefix})
		if !errors.Is(err, errReadFailed) {
			t.Fatalf("Put reader error = %v, want injected failure", err)
		}
		digest := Digest(sha256.Sum256(prefix))
		if _, err := store.Copy(context.Background(), digest, io.Discard); err == nil {
			t.Fatal("failed Put made its partial digest readable")
		}
	})
}

type cancelAfterRead struct {
	data   []byte
	cancel context.CancelFunc
	read   bool
}

type signalEOFReader struct {
	reader   io.Reader
	streamed chan<- struct{}
}

func (r *signalEOFReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		close(r.streamed)
	}
	return n, err
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	n := copy(p, r.data)
	r.cancel()
	return n, nil
}

type cancelAfterWrite struct {
	cancel context.CancelFunc
	wrote  bool
}

func (w *cancelAfterWrite) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.cancel()
	}
	return len(p), nil
}

var (
	errReadFailed  = errors.New("injected read failure")
	errWriteFailed = errors.New("injected write failure")
)

type failAfterRead struct {
	data []byte
	read bool
}

func (r *failAfterRead) Read(p []byte) (int, error) {
	if r.read {
		return 0, errReadFailed
	}
	r.read = true
	return copy(p, r.data), nil
}

type failAfterWrite struct {
	remaining int
	written   int
}

func (w *failAfterWrite) Write(p []byte) (int, error) {
	n := min(len(p), w.remaining)
	w.remaining -= n
	w.written += n
	return n, errWriteFailed
}
