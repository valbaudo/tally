package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestIngestFileSelectsMedia(t *testing.T) {
	tests := []struct {
		name, explicit string
		content        []byte
		want           string
	}{
		{"explicit wins", "application/x-custom", []byte("plain"), "application/x-custom"},
		{"sniff pdf", "", []byte("%PDF-1.7\n"), "application/pdf"},
		{"sniff text", "", []byte("hello\n"), "text/plain; charset=utf-8"},
		{"honest fallback", "", []byte{0x00, 0xff, 0x00, 0xfe}, "application/octet-stream"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, err := NewRepository(NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			file, err := repository.IngestFile(context.Background(), "logical-input", test.explicit, bytes.NewReader(test.content))
			if err != nil {
				t.Fatal(err)
			}
			if file.Media() != test.want {
				t.Fatalf("media = %q, want %q", file.Media(), test.want)
			}
		})
	}
}

func TestIngestFileCanonicalizesExplicitMedia(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	file, err := repository.IngestFile(context.Background(), "logical-input", "Application/X-Custom; Foo=bar", bytes.NewReader([]byte("plain")))
	if err != nil {
		t.Fatal(err)
	}
	if file.Media() != "application/x-custom; foo=bar" {
		t.Fatalf("media = %q, want canonical value", file.Media())
	}
	for _, media := range []string{"json", "application/", "application/*"} {
		if _, err := repository.IngestFile(context.Background(), "logical-input", media, bytes.NewReader([]byte("plain"))); err == nil {
			t.Errorf("IngestFile accepted non-concrete media %q", media)
		}
	}
}

func TestNewFileCanonicalizesAndRequiresConcreteTypeSubtypeMedia(t *testing.T) {
	digest := Digest(sha256.Sum256([]byte("semantic bytes")))
	file, err := NewFile(digest, 14, "report.json", "Application/JSON; Profile=\"custom\"; Charset=UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := file.Media(), "application/json; charset=UTF-8; profile=custom"; got != want {
		t.Fatalf("canonical media = %q, want %q", got, want)
	}
	for _, media := range []string{"json", "application/", "/json", "*/json", "application/*", "*/*"} {
		if _, err := NewFile(digest, 14, "report.json", media); err == nil {
			t.Errorf("NewFile accepted non-concrete media %q", media)
		}
	}
	forged := File{digest: digest, size: 14, name: "report.json", media: "Application/JSON"}
	if forged.Valid() {
		t.Fatal("File.Valid accepted non-canonical media storage")
	}
}

func TestIngestFilePassesExplicitMediaSourceDirectlyToStore(t *testing.T) {
	source := bytes.NewReader([]byte("plain"))
	store := &sourceCapturingStore{source: source, backing: NewMemory()}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repository.IngestFile(context.Background(), "logical-input", "application/x-custom", source); err != nil {
		t.Fatal(err)
	}
	if !store.receivedSource {
		t.Fatal("explicit-media source was buffered instead of passed directly to Store.Put")
	}
}

func TestIngestFilePreservesExactPrefixReadError(t *testing.T) {
	data := bytes.Repeat([]byte("x"), mediaSniffSize)
	store := NewMemory()
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.IngestFile(context.Background(), "logical-input", "", &exactPrefixErrorReader{data: data})
	if !errors.Is(err, errExactPrefixRead) {
		t.Fatalf("IngestFile error = %v, want exact prefix read error", err)
	}
	digest := Digest(sha256.Sum256(data))
	if _, err := store.Copy(context.Background(), digest, io.Discard); err == nil {
		t.Fatal("ingestion published bytes returned with a read error")
	}
}

func TestIngestFileToleratesTransientEmptyReadBeforeSniffing(t *testing.T) {
	data := []byte("hello\n")
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	file, err := repository.IngestFile(context.Background(), "logical-input", "", &transientEmptyReader{data: data})
	if err != nil {
		t.Fatal(err)
	}
	if file.Media() != "text/plain; charset=utf-8" {
		t.Fatalf("media = %q, want text/plain; charset=utf-8", file.Media())
	}
	var got bytes.Buffer
	if err := repository.CopyFile(context.Background(), file, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("ingested bytes = %q, want %q", got.Bytes(), data)
	}
}

func TestIngestFileCommitsBytesIndependentOfHostFile(t *testing.T) {
	sourceName := filepath.Join(t.TempDir(), "source.txt")
	want := []byte("original committed bytes")
	if err := os.WriteFile(sourceName, want, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourceName)
	if err != nil {
		t.Fatal(err)
	}

	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file, err := repository.IngestFile(context.Background(), "customer/report.txt", "", source)
	if closeErr := source.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceName, []byte("host bytes changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourceName); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := repository.CopyFile(context.Background(), file, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("copied bytes = %q, want %q", got.Bytes(), want)
	}
	if file.Name() != "customer/report.txt" || file.Media() != "text/plain; charset=utf-8" || file.Size() != int64(len(want)) {
		t.Fatalf("file facts changed: name=%q media=%q size=%d", file.Name(), file.Media(), file.Size())
	}
}

func TestIngestFileStreamsLargeReader(t *testing.T) {
	const size = 3 << 20
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	reader := &boundedLargeReader{remaining: size, maxRequest: 64 << 10}
	file, err := repository.IngestFile(context.Background(), "large.bin", "", reader)
	if err != nil {
		t.Fatal(err)
	}
	if file.Size() != size {
		t.Fatalf("size = %d, want %d", file.Size(), size)
	}
	if reader.largestRequest > 64<<10 {
		t.Fatalf("largest read request = %d, want at most %d", reader.largestRequest, 64<<10)
	}
}

func TestCopyFileCopiesCommittedBytes(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file, err := repository.IngestFile(context.Background(), "logical.txt", "text/plain", bytes.NewReader([]byte("committed")))
	if err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := repository.CopyFile(context.Background(), file, &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "committed" {
		t.Fatalf("copied content = %q, want %q", got.String(), "committed")
	}
}

func TestMaterializeFileWritesExactRuntimeTarget(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file, err := repository.IngestFile(context.Background(), "../semantic-name.txt", "", bytes.NewReader([]byte("committed")))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "runtime-owned-output")

	if err := repository.MaterializeFile(context.Background(), file, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "committed" {
		t.Fatalf("materialized content = %q, want %q", got, "committed")
	}
}

func TestMaterializeFileRequiresAbsentTarget(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file, err := repository.IngestFile(context.Background(), "logical.txt", "", bytes.NewReader([]byte("committed")))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "already-exists")
	if err := os.WriteFile(target, []byte("keep this"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := repository.MaterializeFile(context.Background(), file, target); err == nil {
		t.Fatal("MaterializeFile succeeded for an existing target")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep this" {
		t.Fatalf("existing target changed to %q", got)
	}
}

func TestMaterializeFileDoesNotOverwriteTargetCreatedDuringCopy(t *testing.T) {
	committed := []byte("committed")
	digest := Digest(sha256.Sum256(committed))
	file, err := NewFile(digest, int64(len(committed)), "logical.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "runtime-owned-output")
	store := &collidingStore{data: committed, target: target, collision: []byte("racing owner"), created: make(chan struct{}), release: make(chan struct{})}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- repository.MaterializeFile(context.Background(), file, target)
	}()
	<-store.created
	close(store.release)
	err = <-result
	if err == nil {
		t.Fatal("MaterializeFile overwrote a target created during copying")
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "racing owner" {
		t.Fatalf("racing target = %q, want %q", got, "racing owner")
	}
}

func TestMaterializeFileRejectsStoreBytesWithWrongDigest(t *testing.T) {
	committed := []byte("committed")
	digest := Digest(sha256.Sum256(committed))
	file, err := NewFile(digest, int64(len(committed)), "logical.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(&lyingStore{data: []byte("tampered")})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "runtime-owned-output")

	if err := repository.MaterializeFile(context.Background(), file, target); err == nil {
		t.Fatal("MaterializeFile accepted bytes with a wrong digest")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target after failed materialization = %v, want absent", err)
	}
}

func TestMaterializeFileJoinsTemporaryCleanupErrors(t *testing.T) {
	committed := []byte("committed")
	digest := Digest(sha256.Sum256(committed))
	file, err := NewFile(digest, int64(len(committed)), "logical.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(&lyingStore{data: []byte("tampered")})
	if err != nil {
		t.Fatal(err)
	}

	originalCreate := createMaterializationTemp
	originalRemove := removeMaterializationTemp
	createMaterializationTemp = func(string, string) (materializationTemp, error) {
		return &failingMaterializationTemp{name: filepath.Join(t.TempDir(), "temporary"), closeErr: errTemporaryClose}, nil
	}
	removeMaterializationTemp = func(string) error { return errTemporaryRemove }
	t.Cleanup(func() {
		createMaterializationTemp = originalCreate
		removeMaterializationTemp = originalRemove
	})

	err = repository.MaterializeFile(context.Background(), file, filepath.Join(t.TempDir(), "runtime-owned-output"))
	if !errors.Is(err, errTemporaryClose) {
		t.Fatalf("MaterializeFile error = %v, want joined close error", err)
	}
	if !errors.Is(err, errTemporaryRemove) {
		t.Fatalf("MaterializeFile error = %v, want joined remove error", err)
	}
}

func TestMaterializeFileRollsBackPublishedTargetWhenTemporaryCleanupFails(t *testing.T) {
	repository, err := NewRepository(NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file, err := repository.IngestFile(context.Background(), "logical.txt", "text/plain", bytes.NewReader([]byte("committed")))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "runtime-owned-output")

	originalRemove := removeMaterializationTemp
	failCleanup := true
	removeMaterializationTemp = func(name string) error {
		if failCleanup {
			return errTemporaryRemove
		}
		return os.Remove(name)
	}
	t.Cleanup(func() { removeMaterializationTemp = originalRemove })

	err = repository.MaterializeFile(context.Background(), file, target)
	if !errors.Is(err, errTemporaryRemove) {
		t.Fatalf("MaterializeFile error = %v, want temporary cleanup failure", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("published target after failed operation = %v, want absent", statErr)
	}
	failCleanup = false
	if err := repository.MaterializeFile(context.Background(), file, target); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

type boundedLargeReader struct {
	remaining      int64
	maxRequest     int
	largestRequest int
}

func (r *boundedLargeReader) Read(data []byte) (int, error) {
	if len(data) > r.largestRequest {
		r.largestRequest = len(data)
	}
	if len(data) > r.maxRequest {
		return 0, errors.New("reader requested an unbounded buffer")
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(data), int(r.remaining))
	for i := range data[:n] {
		data[i] = byte(i)
	}
	r.remaining -= int64(n)
	return n, nil
}

type lyingStore struct {
	data []byte
}

func (s *lyingStore) Put(context.Context, io.Reader) (Object, error) {
	return Object{}, errors.New("Put is not used")
}

func (s *lyingStore) Copy(_ context.Context, _ Digest, destination io.Writer) (int64, error) {
	n, err := destination.Write(s.data)
	return int64(n), err
}

type sourceCapturingStore struct {
	source         io.Reader
	backing        Store
	receivedSource bool
}

func (s *sourceCapturingStore) Put(ctx context.Context, source io.Reader) (Object, error) {
	s.receivedSource = source == s.source
	return s.backing.Put(ctx, source)
}

func (s *sourceCapturingStore) Copy(ctx context.Context, digest Digest, destination io.Writer) (int64, error) {
	return s.backing.Copy(ctx, digest, destination)
}

var errExactPrefixRead = errors.New("injected exact prefix read error")

type exactPrefixErrorReader struct {
	data []byte
	read bool
}

func (r *exactPrefixErrorReader) Read(destination []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	return copy(destination, r.data), errExactPrefixRead
}

type transientEmptyReader struct {
	data  []byte
	empty bool
}

func (r *transientEmptyReader) Read(destination []byte) (int, error) {
	if !r.empty {
		r.empty = true
		return 0, nil
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(destination, r.data)
	r.data = r.data[n:]
	return n, nil
}

type collidingStore struct {
	data      []byte
	target    string
	collision []byte
	created   chan struct{}
	release   chan struct{}
}

func (s *collidingStore) Put(context.Context, io.Reader) (Object, error) {
	return Object{}, errors.New("Put is not used")
}

func (s *collidingStore) Copy(_ context.Context, _ Digest, destination io.Writer) (int64, error) {
	n, err := destination.Write(s.data)
	if err != nil {
		return int64(n), err
	}
	if err := os.WriteFile(s.target, s.collision, 0o600); err != nil {
		return int64(n), err
	}
	close(s.created)
	<-s.release
	return int64(n), nil
}

var (
	errTemporaryClose  = errors.New("injected temporary close error")
	errTemporaryRemove = errors.New("injected temporary remove error")
)

type failingMaterializationTemp struct {
	bytes.Buffer
	name     string
	closeErr error
}

func (f *failingMaterializationTemp) Name() string { return f.name }

func (f *failingMaterializationTemp) Close() error { return f.closeErr }
