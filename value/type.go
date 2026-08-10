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
	constructed bool
	name        string
	typ         Type
	optional    bool
}

// Name returns the field's declared name.
func (f Field) Name() string { return f.name }

// Type returns the field's declared type.
func (f Field) Type() Type { return f.typ }

// Optional reports whether an object or contract may omit this field.
func (f Field) Optional() bool { return f.optional }

// Type is an immutable recursive contract type.
type Type struct {
	constructed bool
	kind        Kind
	elem        *Type
	fields      []Field
	enum        []Literal
	media       []string
}

// String constructs the string type.
func String() Type { return Type{constructed: true, kind: StringKind} }

// Integer constructs the integer type.
func Integer() Type { return Type{constructed: true, kind: IntegerKind} }

// Number constructs the number type.
func Number() Type { return Type{constructed: true, kind: NumberKind} }

// Boolean constructs the boolean type.
func Boolean() Type { return Type{constructed: true, kind: BooleanKind} }

// Null constructs the null type.
func Null() Type { return Type{constructed: true, kind: NullKind} }

// Tree constructs the immutable tree-handle type.
func Tree() Type { return Type{constructed: true, kind: TreeKind} }

// Any constructs the ordinary untyped-data type.
func Any() Type { return Type{constructed: true, kind: AnyKind} }

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
	if !typ.constructed {
		return Field{}, fmt.Errorf("field %q has an invalid type", name)
	}
	return Field{constructed: true, name: name, typ: typ, optional: optional}, nil
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
	return Type{constructed: true, kind: FileKind, media: constraints}, nil
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
	return Type{constructed: true, kind: EnumKind, enum: members}, nil
}

// List constructs a homogeneous list type.
func List(element Type) (Type, error) {
	if !element.constructed {
		return Type{}, fmt.Errorf("list element type is invalid")
	}
	copy := element
	return Type{constructed: true, kind: ListKind, elem: &copy}, nil
}

// Map constructs a homogeneous string-keyed map type.
func Map(element Type) (Type, error) {
	if !element.constructed {
		return Type{}, fmt.Errorf("map element type is invalid")
	}
	copy := element
	return Type{constructed: true, kind: MapKind, elem: &copy}, nil
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
	return Type{constructed: true, kind: ObjectKind, fields: copy}, nil
}

type fieldSetProblem struct {
	invalid   bool
	duplicate string
}

func canonicalFields(fields []Field) ([]Field, fieldSetProblem) {
	copy := append([]Field(nil), fields...)
	for _, field := range copy {
		if !field.constructed || field.name == "" || !field.typ.constructed {
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
	stack := []Type{t}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch current.kind {
		case StringKind, IntegerKind, NumberKind, BooleanKind, NullKind, TreeKind, AnyKind:
			if current.elem != nil || len(current.fields) != 0 || len(current.enum) != 0 || len(current.media) != 0 {
				return false
			}
		case FileKind:
			if current.elem != nil || len(current.fields) != 0 || len(current.enum) != 0 {
				return false
			}
			for _, constraint := range current.media {
				if !mediaConstraint.MatchString(constraint) {
					return false
				}
			}
		case EnumKind:
			if current.elem != nil || len(current.fields) != 0 || len(current.media) != 0 || len(current.enum) == 0 {
				return false
			}
			for _, member := range current.enum {
				if !member.valid() || !member.scalar() {
					return false
				}
			}
		case ObjectKind:
			if current.elem != nil || len(current.enum) != 0 || len(current.media) != 0 {
				return false
			}
			for index, field := range current.fields {
				if field.name == "" || (index > 0 && current.fields[index-1].name >= field.name) {
					return false
				}
				stack = append(stack, field.typ)
			}
		case MapKind, ListKind:
			if current.elem == nil || len(current.fields) != 0 || len(current.enum) != 0 || len(current.media) != 0 {
				return false
			}
			stack = append(stack, *current.elem)
		default:
			return false
		}
	}
	return true
}

func equalType(left, right Type) bool {
	if !left.Valid() || !right.Valid() {
		return false
	}
	type pair struct{ left, right Type }
	stack := []pair{{left: left, right: right}}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.left.kind != current.right.kind {
			return false
		}
		switch current.left.kind {
		case ListKind, MapKind:
			stack = append(stack, pair{left: *current.left.elem, right: *current.right.elem})
		case ObjectKind:
			if len(current.left.fields) != len(current.right.fields) {
				return false
			}
			for index := range current.left.fields {
				leftField, rightField := current.left.fields[index], current.right.fields[index]
				if leftField.name != rightField.name || leftField.optional != rightField.optional {
					return false
				}
				stack = append(stack, pair{left: leftField.typ, right: rightField.typ})
			}
		case EnumKind:
			if len(current.left.enum) != len(current.right.enum) {
				return false
			}
			for index := range current.left.enum {
				if !current.left.enum[index].Equal(current.right.enum[index]) {
					return false
				}
			}
		case FileKind:
			if len(current.left.media) != len(current.right.media) {
				return false
			}
			for index := range current.left.media {
				if current.left.media[index] != current.right.media[index] {
					return false
				}
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
