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
