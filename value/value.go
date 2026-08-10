package value

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/dawn/content"
)

// Value is one immutable recursive runtime value.
type Value struct {
	kind    Kind
	text    string
	boolean bool
	number  string
	entries []Entry
	items   []Value
	file    *content.File
	tree    *content.Tree
}

// Entry is one immutable named object field or map member.
type Entry struct {
	name  string
	value Value
}

// NewString constructs a string value, retaining every input byte.
func NewString(text string) Value { return Value{kind: StringKind, text: text} }

// NewInteger constructs an integer from its exact canonical JSON-number form.
func NewInteger(number string) (Value, error) {
	if !validJSONNumber(number) || strings.ContainsAny(number, ".eE") {
		return Value{}, fmt.Errorf("invalid integer %q", number)
	}
	return Value{kind: IntegerKind, number: number}, nil
}

// NewNumber constructs a number from its exact canonical JSON-number form.
func NewNumber(number string) (Value, error) {
	if !validJSONNumber(number) {
		return Value{}, fmt.Errorf("invalid number %q", number)
	}
	return Value{kind: NumberKind, number: number}, nil
}

// NewBoolean constructs a boolean value.
func NewBoolean(boolean bool) Value { return Value{kind: BooleanKind, boolean: boolean} }

// NewNull constructs the null value.
func NewNull() Value { return Value{kind: NullKind} }

// NewEntry constructs a named member. Empty names are retained for maps;
// NewObject rejects them as invalid object field names.
func NewEntry(name string, value Value) (Entry, error) {
	if !shallowValidValue(value) {
		return Entry{}, fmt.Errorf("entry %q has an invalid value", name)
	}
	return Entry{name: name, value: value}, nil
}

// NewObject constructs a closed object with canonical field order.
func NewObject(entries ...Entry) (Value, error) {
	copy, err := canonicalEntries(entries, true)
	if err != nil {
		return Value{}, err
	}
	return Value{kind: ObjectKind, entries: copy}, nil
}

// NewMap constructs a string-keyed map with canonical key order.
func NewMap(entries ...Entry) (Value, error) {
	copy, err := canonicalEntries(entries, false)
	if err != nil {
		return Value{}, err
	}
	return Value{kind: MapKind, entries: copy}, nil
}

func canonicalEntries(entries []Entry, object bool) ([]Entry, error) {
	copy := append([]Entry(nil), entries...)
	for _, entry := range copy {
		if !shallowValidValue(entry.value) || (object && entry.name == "") {
			return nil, fmt.Errorf("invalid entry")
		}
	}
	sort.Slice(copy, func(i, j int) bool { return copy[i].name < copy[j].name })
	for i := 1; i < len(copy); i++ {
		if copy[i-1].name == copy[i].name {
			return nil, fmt.Errorf("duplicate entry %q", copy[i].name)
		}
	}
	return copy, nil
}

// NewList constructs an ordered list. An invalid member produces an invalid
// zero Value.
func NewList(items ...Value) Value {
	copy := append([]Value(nil), items...)
	for _, item := range copy {
		if !shallowValidValue(item) {
			return Value{}
		}
	}
	return Value{kind: ListKind, items: copy}
}

// NewFileValue inserts a semantic file record into the value graph.
func NewFileValue(file content.File) Value {
	if !file.Valid() {
		return Value{}
	}
	copy := file
	return Value{kind: FileKind, file: &copy}
}

// NewTreeValue inserts a semantic tree record into the value graph.
func NewTreeValue(tree content.Tree) Value {
	if !tree.Valid() {
		return Value{}
	}
	copy := tree
	return Value{kind: TreeKind, tree: &copy}
}

// Kind returns the value's closed-algebra variant.
func (v Value) Kind() Kind { return v.kind }

// Valid reports whether the complete finite value graph is well formed.
func (v Value) Valid() bool {
	stack := []Value{v}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !shallowValidValue(current) {
			return false
		}
		switch current.kind {
		case ObjectKind, MapKind:
			for i, entry := range current.entries {
				if (current.kind == ObjectKind && entry.name == "") || (i > 0 && current.entries[i-1].name >= entry.name) {
					return false
				}
				stack = append(stack, entry.value)
			}
		case ListKind:
			stack = append(stack, current.items...)
		}
	}
	return true
}

func shallowValidValue(v Value) bool {
	switch v.kind {
	case StringKind:
		return v.number == "" && len(v.entries) == 0 && len(v.items) == 0 && v.file == nil && v.tree == nil
	case IntegerKind:
		return validJSONNumber(v.number) && !strings.ContainsAny(v.number, ".eE") && len(v.entries) == 0 && len(v.items) == 0 && v.file == nil && v.tree == nil
	case NumberKind:
		return validJSONNumber(v.number) && len(v.entries) == 0 && len(v.items) == 0 && v.file == nil && v.tree == nil
	case BooleanKind, NullKind:
		return v.text == "" && v.number == "" && len(v.entries) == 0 && len(v.items) == 0 && v.file == nil && v.tree == nil
	case ObjectKind, MapKind:
		return v.text == "" && v.number == "" && len(v.items) == 0 && v.file == nil && v.tree == nil
	case ListKind:
		return v.text == "" && v.number == "" && len(v.entries) == 0 && v.file == nil && v.tree == nil
	case FileKind:
		return v.file != nil && v.file.Valid() && v.tree == nil && len(v.entries) == 0 && len(v.items) == 0 && v.text == "" && v.number == ""
	case TreeKind:
		return v.tree != nil && v.tree.Valid() && v.file == nil && len(v.entries) == 0 && len(v.items) == 0 && v.text == "" && v.number == ""
	default:
		return false
	}
}

// Valid reports whether e has a valid runtime value.
func (e Entry) Valid() bool { return shallowValidValue(e.value) }

// Name returns the exact field-name or map-key bytes.
func (e Entry) Name() string { return e.name }

// Value returns the immutable member value.
func (e Entry) Value() Value { return e.value }

// Text returns the exact string bytes when v is a string.
func (v Value) Text() (string, bool) {
	if v.kind != StringKind {
		return "", false
	}
	return v.text, true
}

// Number returns the exact numeric representation for an integer or number
// value.
func (v Value) Number() (string, bool) {
	if v.kind != IntegerKind && v.kind != NumberKind {
		return "", false
	}
	return v.number, true
}

// Boolean returns the boolean payload when v is a boolean.
func (v Value) Boolean() (bool, bool) {
	if v.kind != BooleanKind {
		return false, false
	}
	return v.boolean, true
}

// Entries returns a copy of object fields or map members.
func (v Value) Entries() []Entry {
	if v.kind != ObjectKind && v.kind != MapKind {
		return nil
	}
	return append([]Entry(nil), v.entries...)
}

// Items returns a copy of ordered list members.
func (v Value) Items() []Value {
	if v.kind != ListKind {
		return nil
	}
	return append([]Value(nil), v.items...)
}

// File returns a copy of the file record when v is a file.
func (v Value) File() (content.File, bool) {
	if v.kind != FileKind || v.file == nil {
		return content.File{}, false
	}
	return *v.file, true
}

// Tree returns a copy of the tree record when v is a tree.
func (v Value) Tree() (content.Tree, bool) {
	if v.kind != TreeKind || v.tree == nil {
		return content.Tree{}, false
	}
	return *v.tree, true
}

// SameRuntimeHandle reports whether two file or tree values are copies of the
// same in-memory handle construction. Runtime orchestration uses this identity
// to track provenance; it is deliberately absent from canonical bytes and
// semantic Equal comparisons.
func (v Value) SameRuntimeHandle(other Value) bool {
	if v.kind != other.kind {
		return false
	}
	switch v.kind {
	case FileKind:
		return v.file != nil && v.file == other.file
	case TreeKind:
		return v.tree != nil && v.tree == other.tree
	default:
		return false
	}
}

// Equal reports whether two values have identical semantics.
func (v Value) Equal(other Value) bool {
	return v.Valid() && other.Valid() && bytes.Equal(v.Canonical(), other.Canonical())
}
