package value

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
	"time"

	"github.com/valbaudo/dawn/content"
)

func TestCanonicalValueRoundTrip(t *testing.T) {
	file := testFile(t, "report.pdf", "application/pdf", []byte("%PDF canonical"))
	tree := testTree(t)
	original := mustObject(t,
		mustEntry(t, "boolean", NewBoolean(true)),
		mustEntry(t, "file", NewFileValue(file)),
		mustEntry(t, "integer", mustInteger(t, "-42")),
		mustEntry(t, "list", NewList(NewNull(), mustNumber(t, "1.25e+2"))),
		mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))),
		mustEntry(t, "tree", NewTreeValue(tree)),
	)
	encoded := original.Canonical()
	if !bytes.HasPrefix(encoded, []byte("dawn.value/1")) {
		t.Fatalf("canonical header = %q", encoded)
	}
	decoded, err := ParseCanonical(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Equal(decoded) {
		t.Fatal("round trip changed value")
	}
	copy := decoded.Canonical()
	copy[len(copy)-1] ^= 0xff
	if bytes.Equal(copy, decoded.Canonical()) {
		t.Fatal("Canonical exposed value storage")
	}
}

func TestCanonicalValueIgnoresObjectAndMapInputOrder(t *testing.T) {
	one := mustEntry(t, "a", NewString("first"))
	two := mustEntry(t, "z", NewString("last"))
	if !bytes.Equal(mustObject(t, one, two).Canonical(), mustObject(t, two, one).Canonical()) {
		t.Fatal("object input order changed canonical bytes")
	}
	if !bytes.Equal(mustMap(t, one, two).Canonical(), mustMap(t, two, one).Canonical()) {
		t.Fatal("map input order changed canonical bytes")
	}
}

func TestCanonicalValueChangesForEverySemanticFact(t *testing.T) {
	baseFile := testFile(t, "a.txt", "text/plain", []byte("a"))
	baseTree := testTree(t)
	base := mustObject(t,
		mustEntry(t, "file", NewFileValue(baseFile)),
		mustEntry(t, "list", NewList(NewString("a"), NewString("b"))),
		mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))),
		mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree)))),
	)

	otherDigest, err := content.NewFile(content.Digest(sha256.Sum256([]byte("b"))), baseFile.Size(), baseFile.Name(), baseFile.Media())
	if err != nil {
		t.Fatal(err)
	}
	otherLength, err := content.NewFile(baseFile.Digest(), baseFile.Size()+1, baseFile.Name(), baseFile.Media())
	if err != nil {
		t.Fatal(err)
	}
	otherName, err := contentFileWith(baseFile, "b.txt", baseFile.Media())
	if err != nil {
		t.Fatal(err)
	}
	otherMedia, err := contentFileWith(baseFile, baseFile.Name(), "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	otherTree := testTreeFrom(t, "different tree")
	changes := []Value{
		mustObject(t, mustEntry(t, "file", NewFileValue(otherDigest)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(otherLength)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(otherName)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(otherMedia)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(baseFile)), mustEntry(t, "list", NewList(NewString("b"), NewString("a"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(baseFile)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "other", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(baseFile)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("changed")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(baseTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(baseFile)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "place", NewTreeValue(otherTree))))),
		mustObject(t, mustEntry(t, "file", NewFileValue(baseFile)), mustEntry(t, "list", NewList(NewString("a"), NewString("b"))), mustEntry(t, "map", mustMap(t, mustEntry(t, "key", NewString("value")))), mustEntry(t, "nested", mustObject(t, mustEntry(t, "other", NewTreeValue(baseTree))))),
	}
	for i, changed := range changes {
		if bytes.Equal(base.Canonical(), changed.Canonical()) {
			t.Errorf("semantic change %d retained canonical bytes", i)
		}
	}
}

func contentFileWith(file content.File, name, media string) (content.File, error) {
	return content.NewFile(file.Digest(), file.Size(), name, media)
}

func testTreeFrom(t *testing.T, text string) content.Tree {
	t.Helper()
	digest := content.Digest(sha256.Sum256([]byte(text)))
	tree, err := content.NewTree(digest)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestCanonicalValueRejectsMalformedAndNoncanonicalEncodings(t *testing.T) {
	header := []byte("dawn.value/1")
	stringTag := byte(StringKind)
	objectTag := byte(ObjectKind)
	treeTag := byte(TreeKind)
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"header only", append([]byte(nil), header...)},
		{"unknown tag", append(append([]byte(nil), header...), 0xff)},
		{"trailing data", append(append(append([]byte(nil), header...), byte(NullKind)), 0)},
		{"overlong length", append(append([]byte(nil), header...), stringTag, 0x80, 0)},
		{"truncated string", append(append([]byte(nil), header...), stringTag, 2, 'x')},
		{"invalid number", wireScalar(header, byte(NumberKind), "01")},
		{"zero tree", append(append([]byte(nil), header...), append([]byte{treeTag}, make([]byte, 32)...)...)},
		{"duplicate object", wireEntries(header, objectTag, []wireEntry{{"a", NewNull()}, {"a", NewNull()}})},
		{"unsorted object", wireEntries(header, objectTag, []wireEntry{{"z", NewNull()}, {"a", NewNull()}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCanonical(tc.data); err == nil {
				t.Fatalf("ParseCanonical(%x) succeeded", tc.data)
			}
		})
	}
}

type wireEntry struct {
	name  string
	value Value
}

func wireScalar(header []byte, tag byte, text string) []byte {
	out := append(append([]byte(nil), header...), tag)
	out = binary.AppendUvarint(out, uint64(len(text)))
	return append(out, text...)
}

func wireEntries(header []byte, tag byte, entries []wireEntry) []byte {
	out := append(append([]byte(nil), header...), tag)
	out = binary.AppendUvarint(out, uint64(len(entries)))
	for _, entry := range entries {
		out = binary.AppendUvarint(out, uint64(len(entry.name)))
		out = append(out, entry.name...)
		out = append(out, entry.value.Canonical()[len(header):]...)
	}
	return out
}

func TestCanonicalValueDeepFiniteSubprocess(t *testing.T) {
	if os.Getenv("DAWN_DEEP_VALUE_CHILD") == "1" {
		debug.SetMaxStack(1 << 20)
		value := NewNull()
		for range 50_000 {
			value = NewList(value)
		}
		encoded := value.Canonical()
		decoded, err := ParseCanonical(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := Any().Validate(decoded); err != nil {
			t.Fatal(err)
		}
		if !decoded.Valid() {
			t.Fatal("decoded deep value is invalid")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCanonicalValueDeepFiniteSubprocess$", "-test.count=1")
	command.Env = append(os.Environ(), "DAWN_DEEP_VALUE_CHILD=1")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("deep-value child timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("deep-value child failed: %v\n%s", err, output)
	}
}
