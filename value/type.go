// Package value defines immutable workflow contracts and ordinary literals.
package value

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
)

// Kind is one member of Dawn's closed contract algebra.
type Kind uint8

const (
	InvalidKind Kind = iota
	StringKind
	IntegerKind
	NumberKind
	BooleanKind
	NullKind
	EnumKind
	ObjectKind
	MapKind
	ListKind
	FileKind
	TreeKind
	AnyKind
)

// Field is a named member of an object or contract.
type Field struct {
	name     string
	typ      Type
	optional bool
}

// Name returns the field's declared name.
func (f Field) Name() string { return f.name }

// Type returns the field's declared type.
func (f Field) Type() Type { return f.typ }

// Optional reports whether an object or contract may omit this field.
func (f Field) Optional() bool { return f.optional }

// Type is an immutable recursive contract type.
type Type struct {
	kind   Kind
	elem   *Type
	fields []Field
	enum   []Literal
	media  []string
}

// String constructs the string type.
func String() Type { return Type{kind: StringKind} }

// Integer constructs the integer type.
func Integer() Type { return Type{kind: IntegerKind} }

// Number constructs the number type.
func Number() Type { return Type{kind: NumberKind} }

// Boolean constructs the boolean type.
func Boolean() Type { return Type{kind: BooleanKind} }

// Null constructs the null type.
func Null() Type { return Type{kind: NullKind} }

// Tree constructs the immutable tree-handle type.
func Tree() Type { return Type{kind: TreeKind} }

// Any constructs the ordinary untyped-data type.
func Any() Type { return Type{kind: AnyKind} }

// Kind returns this type's closed-algebra member.
func (t Type) Kind() Kind { return t.kind }

// Valid reports whether t is a constructed contract type.
func (t Type) Valid() bool { return validType(t) }

// Equal reports whether two types have the same canonical structure.
func (t Type) Equal(other Type) bool { return equalType(t, other) }

// Element returns the list or map element type.
func (t Type) Element() (Type, bool) {
	if (t.kind != ListKind && t.kind != MapKind) || t.elem == nil {
		return Type{}, false
	}
	return *t.elem, true
}

// Fields returns a copy of this object's declared fields.
func (t Type) Fields() []Field {
	return append([]Field(nil), t.fields...)
}

// EnumValues returns a copy of this enum's canonical members.
func (t Type) EnumValues() []Literal {
	return append([]Literal(nil), t.enum...)
}

// Media returns a copy of this file type's accepted media constraints.
func (t Type) Media() []string {
	return append([]string(nil), t.media...)
}

// Required constructs a required named field.
func Required(name string, typ Type) (Field, error) {
	return newField(name, typ, false)
}

// Optional constructs an optional named field.
func Optional(name string, typ Type) (Field, error) {
	return newField(name, typ, true)
}

func newField(name string, typ Type, optional bool) (Field, error) {
	if name == "" {
		return Field{}, fmt.Errorf("field name must not be empty")
	}
	if !typ.Valid() {
		return Field{}, fmt.Errorf("field %q has an invalid type", name)
	}
	return Field{name: name, typ: typ, optional: optional}, nil
}

var mediaConstraint = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/(?:[a-z0-9][a-z0-9!#$&^_.+-]*|\*)$`)

// File constructs a file-handle type constrained by optional media patterns.
func File(media ...string) (Type, error) {
	constraints := append([]string(nil), media...)
	for _, constraint := range constraints {
		if !mediaConstraint.MatchString(constraint) {
			return Type{}, fmt.Errorf("invalid media constraint %q", constraint)
		}
	}
	sort.Strings(constraints)
	constraints = compactStrings(constraints)
	return Type{kind: FileKind, media: constraints}, nil
}

// Enum constructs an enum over one or more scalar literal members.
func Enum(values ...Literal) (Type, error) {
	if len(values) == 0 {
		return Type{}, fmt.Errorf("enum must contain at least one literal")
	}
	members := append([]Literal(nil), values...)
	for _, value := range members {
		if !value.valid() || !value.scalar() {
			return Type{}, fmt.Errorf("enum members must be valid scalar literals")
		}
	}
	sort.Slice(members, func(i, j int) bool {
		return bytes.Compare(members[i].canonical, members[j].canonical) < 0
	})
	members = compactLiterals(members)
	return Type{kind: EnumKind, enum: members}, nil
}

// List constructs a homogeneous list type.
func List(element Type) (Type, error) {
	if !element.Valid() {
		return Type{}, fmt.Errorf("list element type is invalid")
	}
	copy := element
	return Type{kind: ListKind, elem: &copy}, nil
}

// Map constructs a homogeneous string-keyed map type.
func Map(element Type) (Type, error) {
	if !element.Valid() {
		return Type{}, fmt.Errorf("map element type is invalid")
	}
	copy := element
	return Type{kind: MapKind, elem: &copy}, nil
}

// Object constructs a closed object with the supplied named fields.
func Object(fields ...Field) (Type, error) {
	copy, problem := canonicalFields(fields)
	if problem.invalid {
		return Type{}, fmt.Errorf("object has an invalid field")
	}
	if problem.duplicate != "" {
		return Type{}, fmt.Errorf("object has duplicate field %q", problem.duplicate)
	}
	return Type{kind: ObjectKind, fields: copy}, nil
}

type fieldSetProblem struct {
	invalid   bool
	duplicate string
}

func canonicalFields(fields []Field) ([]Field, fieldSetProblem) {
	copy := append([]Field(nil), fields...)
	for _, field := range copy {
		if !validField(field) {
			return nil, fieldSetProblem{invalid: true}
		}
	}
	sort.Slice(copy, func(i, j int) bool { return copy[i].name < copy[j].name })
	for i := 1; i < len(copy); i++ {
		if copy[i-1].name == copy[i].name {
			return nil, fieldSetProblem{duplicate: copy[i].name}
		}
	}
	return copy, fieldSetProblem{}
}

func validField(field Field) bool {
	return field.name != "" && field.typ.Valid()
}

func validType(t Type) bool {
	switch t.kind {
	case StringKind, IntegerKind, NumberKind, BooleanKind, NullKind, TreeKind, AnyKind:
		return t.elem == nil && len(t.fields) == 0 && len(t.enum) == 0 && len(t.media) == 0
	case FileKind:
		if t.elem != nil || len(t.fields) != 0 || len(t.enum) != 0 {
			return false
		}
		for _, constraint := range t.media {
			if !mediaConstraint.MatchString(constraint) {
				return false
			}
		}
		return true
	case EnumKind:
		if t.elem != nil || len(t.fields) != 0 || len(t.media) != 0 || len(t.enum) == 0 {
			return false
		}
		for _, value := range t.enum {
			if !value.valid() || !value.scalar() {
				return false
			}
		}
		return true
	case ObjectKind:
		if t.elem != nil || len(t.enum) != 0 || len(t.media) != 0 {
			return false
		}
		for i, field := range t.fields {
			if !validField(field) || (i > 0 && t.fields[i-1].name >= field.name) {
				return false
			}
		}
		return true
	case MapKind, ListKind:
		return t.elem != nil && t.elem.Valid() && len(t.fields) == 0 && len(t.enum) == 0 && len(t.media) == 0
	default:
		return false
	}
}

func equalType(left, right Type) bool {
	if left.kind != right.kind || !left.Valid() || !right.Valid() {
		return false
	}
	switch left.kind {
	case ListKind, MapKind:
		return equalType(*left.elem, *right.elem)
	case ObjectKind:
		if len(left.fields) != len(right.fields) {
			return false
		}
		for i := range left.fields {
			if left.fields[i].name != right.fields[i].name || left.fields[i].optional != right.fields[i].optional || !equalType(left.fields[i].typ, right.fields[i].typ) {
				return false
			}
		}
	case EnumKind:
		if len(left.enum) != len(right.enum) {
			return false
		}
		for i := range left.enum {
			if !left.enum[i].Equal(right.enum[i]) {
				return false
			}
		}
	case FileKind:
		if len(left.media) != len(right.media) {
			return false
		}
		for i := range left.media {
			if left.media[i] != right.media[i] {
				return false
			}
		}
	}
	return true
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	n := 1
	for _, value := range values[1:] {
		if value != values[n-1] {
			values[n] = value
			n++
		}
	}
	return values[:n]
}

func compactLiterals(values []Literal) []Literal {
	if len(values) == 0 {
		return values
	}
	n := 1
	for _, value := range values[1:] {
		if !value.Equal(values[n-1]) {
			values[n] = value
			n++
		}
	}
	return values[:n]
}
