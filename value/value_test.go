package value

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"github.com/valbaudo/dawn/content"
)

func testFile(t *testing.T, name, media string, data []byte) content.File {
	t.Helper()
	object, err := content.NewMemory().Put(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	file, err := content.NewFile(object.Digest(), object.Size(), name, media)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func testTree(t *testing.T) content.Tree {
	t.Helper()
	digest := content.Digest(sha256.Sum256([]byte("canonical tree manifest")))
	tree, err := content.NewTree(digest)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func mustEntry(t *testing.T, name string, value Value) Entry {
	t.Helper()
	entry, err := NewEntry(name, value)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func mustObject(t *testing.T, entries ...Entry) Value {
	t.Helper()
	value, err := NewObject(entries...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustMap(t *testing.T, entries ...Entry) Value {
	t.Helper()
	value, err := NewMap(entries...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustInteger(t *testing.T, number string) Value {
	t.Helper()
	value, err := NewInteger(number)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustNumber(t *testing.T, number string) Value {
	t.Helper()
	value, err := NewNumber(number)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestValueOwnsNestedData(t *testing.T) {
	file := testFile(t, "report.pdf", "application/pdf", []byte("%PDF"))
	tree := testTree(t)
	value := mustObject(t,
		mustEntry(t, "document", NewFileValue(file)),
		mustEntry(t, "pages", NewList(NewFileValue(file))),
		mustEntry(t, "evidence", mustMap(t, mustEntry(t, "primary", NewFileValue(file)))),
		mustEntry(t, "bundle", mustObject(t,
			mustEntry(t, "report", NewFileValue(file)),
			mustEntry(t, "sources", NewTreeValue(tree)),
		)),
	)
	before := value.Canonical()
	mutateEveryReturnedSliceAndPath(t, value)
	if !bytes.Equal(before, value.Canonical()) {
		t.Fatal("value mutated through an accessor")
	}
}

func mutateEveryReturnedSliceAndPath(t *testing.T, root Value) {
	t.Helper()
	stack := []Value{root}
	path := Path{}
	for len(stack) > 0 {
		value := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, entry := range value.Entries() {
			stack = append(stack, entry.Value())
			path = path.Field(entry.Name())
		}
		entries := value.Entries()
		if len(entries) > 0 {
			entries[0] = Entry{}
		}
		items := value.Items()
		stack = append(stack, items...)
		if len(items) > 0 {
			items[0] = Value{}
			path = path.ListIndex(0)
		}
		if file, ok := value.File(); ok {
			digest := file.Digest().Bytes()
			digest[0] ^= 0xff
		}
		if tree, ok := value.Tree(); ok {
			digest := tree.Digest().Bytes()
			digest[0] ^= 0xff
		}
	}
	segments := path.Segments()
	if len(segments) > 0 {
		segments[0] = Segment{}
	}
}

func TestValueDistinguishesRecursiveVariantsAndScalarRepresentations(t *testing.T) {
	plainEntries := []Entry{mustEntry(t, "a", NewString("x"))}
	object := mustObject(t, plainEntries...)
	mapping := mustMap(t, plainEntries...)
	if object.Equal(mapping) {
		t.Fatal("object and map have the same identity")
	}
	if mustInteger(t, "1").Equal(mustNumber(t, "1")) {
		t.Fatal("integer and number have the same identity")
	}
	if NewList(NewString("a"), NewString("b")).Equal(NewList(NewString("b"), NewString("a"))) {
		t.Fatal("list order is not semantic")
	}

	absent := mustObject(t)
	presentNull := mustObject(t, mustEntry(t, "optional", NewNull()))
	if absent.Equal(presentNull) {
		t.Fatal("absent field equals explicit null")
	}
}

func TestValueScalarAccessorsDoNotCoerce(t *testing.T) {
	if got, ok := NewString("exact").Text(); !ok || got != "exact" {
		t.Fatalf("Text() = (%q, %v)", got, ok)
	}
	if got, ok := mustInteger(t, "-12").Number(); !ok || got != "-12" {
		t.Fatalf("integer Number() = (%q, %v)", got, ok)
	}
	if got, ok := mustNumber(t, "1.20e+3").Number(); !ok || got != "1.20e+3" {
		t.Fatalf("number Number() = (%q, %v)", got, ok)
	}
	if got, ok := NewBoolean(true).Boolean(); !ok || !got {
		t.Fatalf("Boolean() = (%v, %v)", got, ok)
	}
	if _, ok := NewString("true").Boolean(); ok {
		t.Fatal("string was coerced through Boolean")
	}
	if _, ok := NewString("1").Number(); ok {
		t.Fatal("string was coerced through Number")
	}
}

func TestValueRetainsArbitraryBytesInStringsNamesAndKeys(t *testing.T) {
	text := string([]byte{'v', 0, 0xff})
	name := string([]byte{'n', '/', 0, 0xfe})
	key := string([]byte{0, 0xfd})
	value := mustObject(t,
		mustEntry(t, name, NewString(text)),
		mustEntry(t, "map", mustMap(t, mustEntry(t, key, NewString(text)))),
	)
	decoded, err := ParseCanonical(value.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	entries := decoded.Entries()
	if entries[1].Name() != name {
		t.Fatalf("object name = %q, want exact bytes %q", entries[1].Name(), name)
	}
	gotText, ok := entries[1].Value().Text()
	if !ok || gotText != text {
		t.Fatalf("string = %q, want exact bytes %q", gotText, text)
	}
	mapEntries := entries[0].Value().Entries()
	if len(mapEntries) != 1 || mapEntries[0].Name() != key {
		t.Fatalf("map key changed: %#v", mapEntries)
	}
}

func TestValueConstructorsRejectInvalidInputs(t *testing.T) {
	if (Value{}).Valid() {
		t.Fatal("zero Value is valid")
	}
	if (Entry{}).Valid() {
		t.Fatal("zero Entry is valid")
	}
	if _, err := NewEntry("bad", Value{}); err == nil {
		t.Fatal("NewEntry accepted an invalid value")
	}
	duplicate := mustEntry(t, "same", NewNull())
	if _, err := NewObject(duplicate, duplicate); err == nil {
		t.Fatal("NewObject accepted duplicate fields")
	}
	if _, err := NewMap(duplicate, duplicate); err == nil {
		t.Fatal("NewMap accepted duplicate keys")
	}
	if got := NewList(Value{}); got.Valid() {
		t.Fatal("NewList accepted an invalid item")
	}
	if got := NewFileValue(content.File{}); got.Valid() {
		t.Fatal("NewFileValue accepted a zero file")
	}
	if got := NewTreeValue(content.Tree{}); got.Valid() {
		t.Fatal("NewTreeValue accepted a zero tree")
	}
	for _, number := range []string{"", "01", "1.", "+1", "NaN"} {
		if _, err := NewNumber(number); err == nil {
			t.Errorf("NewNumber(%q) succeeded", number)
		}
	}
	for _, integer := range []string{"1.0", "1e2", "-"} {
		if _, err := NewInteger(integer); err == nil {
			t.Errorf("NewInteger(%q) succeeded", integer)
		}
	}
}

func TestContentValueRecordsValidateFactsWithoutStorePresence(t *testing.T) {
	data := []byte("facts are not stored")
	digest := content.Digest(sha256.Sum256(data))
	file, err := content.NewFile(digest, int64(len(data)), "logical/name.bin", "application/octet-stream; profile=exact")
	if err != nil {
		t.Fatal(err)
	}
	if !file.Valid() || !file.Digest().Equal(digest) || file.Size() != int64(len(data)) || file.Name() != "logical/name.bin" || file.Media() != "application/octet-stream; profile=exact" {
		t.Fatalf("file facts changed: %#v", file)
	}
	if _, err := content.NewFile(content.Digest{}, 0, "name", "application/octet-stream"); err == nil {
		t.Fatal("NewFile accepted a zero digest")
	}
	if _, err := content.NewFile(digest, int64(len(data)), "name", "not media"); err == nil {
		t.Fatal("NewFile accepted invalid media")
	}
	if _, err := content.NewFile(digest, int64(len(data)), "name", "image/*"); err == nil {
		t.Fatal("NewFile accepted a non-concrete media wildcard")
	}
	if _, err := content.NewFile(digest, int64(len(data)), "", "application/octet-stream"); err == nil {
		t.Fatal("NewFile accepted an empty logical name")
	}

	treeDigest := content.Digest(sha256.Sum256([]byte("tree facts not stored")))
	tree, err := content.NewTree(treeDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Valid() || !tree.Digest().Equal(treeDigest) {
		t.Fatalf("tree facts changed: %#v", tree)
	}
	if _, err := content.NewTree(content.Digest{}); err == nil {
		t.Fatal("NewTree accepted a zero digest")
	}
}

func TestPathUsesCopiedTaggedSegments(t *testing.T) {
	rawField := string([]byte{'f', '/', 0, 0xff})
	rawKey := string([]byte{'k', '.', 0xfe})
	root := Path{}
	fieldPath := root.Field(rawField)
	full := fieldPath.MapKey(rawKey).ListIndex(3)
	if len(root.Segments()) != 0 || len(fieldPath.Segments()) != 1 {
		t.Fatal("path extension mutated its receiver")
	}
	segments := full.Segments()
	if len(segments) != 3 || segments[0].Kind() != FieldSegment || segments[1].Kind() != MapKeySegment || segments[2].Kind() != ListIndexSegment {
		t.Fatalf("segment kinds = %#v", segments)
	}
	if name, ok := segments[0].Name(); !ok || name != rawField {
		t.Fatalf("field segment = (%q, %v)", name, ok)
	}
	if key, ok := segments[1].Name(); !ok || key != rawKey {
		t.Fatalf("map segment = (%q, %v)", key, ok)
	}
	if index, ok := segments[2].Index(); !ok || index != 3 {
		t.Fatalf("list segment = (%d, %v)", index, ok)
	}
	segments[0] = Segment{}
	if full.Segments()[0].Kind() != FieldSegment {
		t.Fatal("Segments exposed path storage")
	}
	if full.String() == rawField || full.String() == rawKey {
		t.Fatal("diagnostic rendering treated a raw name as path identity")
	}
}
