package value

import (
	"reflect"
	"testing"
)

func mustType(t *testing.T, typ Type, err error) Type {
	t.Helper()
	if err != nil {
		t.Fatalf("constructor error: %v", err)
	}
	return typ
}

func mustField(t *testing.T, field Field, err error) Field {
	t.Helper()
	if err != nil {
		t.Fatalf("field error: %v", err)
	}
	return field
}

func mustLiteral(t *testing.T, text string) Literal {
	t.Helper()
	literal, err := ParseLiteral([]byte(text))
	if err != nil {
		t.Fatalf("ParseLiteral(%q): %v", text, err)
	}
	return literal
}

func required(t *testing.T, name string, typ Type) Field {
	t.Helper()
	field, err := Required(name, typ)
	return mustField(t, field, err)
}

func optional(t *testing.T, name string, typ Type) Field {
	t.Helper()
	field, err := Optional(name, typ)
	return mustField(t, field, err)
}

func fileType(t *testing.T, media ...string) Type {
	t.Helper()
	typ, err := File(media...)
	return mustType(t, typ, err)
}

func enumType(t *testing.T, values ...Literal) Type {
	t.Helper()
	typ, err := Enum(values...)
	return mustType(t, typ, err)
}

func listType(t *testing.T, element Type) Type {
	t.Helper()
	typ, err := List(element)
	return mustType(t, typ, err)
}

func mapType(t *testing.T, element Type) Type {
	t.Helper()
	typ, err := Map(element)
	return mustType(t, typ, err)
}

func objectType(t *testing.T, fields ...Field) Type {
	t.Helper()
	typ, err := Object(fields...)
	return mustType(t, typ, err)
}

func TestTypeAlgebraIsClosed(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		kind Kind
	}{
		{"string", String(), StringKind},
		{"integer", Integer(), IntegerKind},
		{"number", Number(), NumberKind},
		{"boolean", Boolean(), BooleanKind},
		{"null", Null(), NullKind},
		{"any", Any(), AnyKind},
		{"file", fileType(t, "application/pdf", "image/*"), FileKind},
		{"tree", Tree(), TreeKind},
		{"list", listType(t, String()), ListKind},
		{"map", mapType(t, Boolean()), MapKind},
		{"object", objectType(t, required(t, "name", String())), ObjectKind},
		{"enum", enumType(t, mustLiteral(t, `"low"`), mustLiteral(t, `"high"`)), EnumKind},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.typ.Kind(); got != tc.kind {
				t.Fatalf("Kind() = %v, want %v", got, tc.kind)
			}
			if !tc.typ.Valid() {
				t.Fatal("constructed type is invalid")
			}
		})
	}
	if (Type{}).Valid() {
		t.Fatal("zero Type is valid")
	}
}

func TestTypeConstructorsRejectInvalidInputs(t *testing.T) {
	if _, err := Required("", String()); err == nil {
		t.Fatal("Required accepted an empty name")
	}
	if _, err := Optional("", String()); err == nil {
		t.Fatal("Optional accepted an empty name")
	}
	if _, err := Required("broken", Type{}); err == nil {
		t.Fatal("Required accepted an invalid type")
	}
	if _, err := List(Type{}); err == nil {
		t.Fatal("List accepted an invalid element")
	}
	if _, err := Map(Type{}); err == nil {
		t.Fatal("Map accepted an invalid element")
	}
	valid := required(t, "name", String())
	if _, err := Object(valid, valid); err == nil {
		t.Fatal("Object accepted duplicate fields")
	}
	if _, err := Object(Field{}); err == nil {
		t.Fatal("Object accepted an invalid field")
	}
}

func TestEnumNormalizesAndLimitsLiterals(t *testing.T) {
	one := mustLiteral(t, "1")
	onePointZero := mustLiteral(t, "1.0")
	typ := enumType(t, onePointZero, one, one)
	values := typ.EnumValues()
	if len(values) != 2 {
		t.Fatalf("EnumValues length = %d, want 2", len(values))
	}
	if !values[0].Equal(one) || !values[1].Equal(onePointZero) {
		t.Fatalf("EnumValues = %q, want distinct ordered 1 and 1.0", values)
	}
	if _, err := Enum(); err == nil {
		t.Fatal("Enum accepted no members")
	}
	if _, err := Enum(mustLiteral(t, "[]")); err == nil {
		t.Fatal("Enum accepted an array")
	}
	if _, err := Enum(mustLiteral(t, `{}`)); err == nil {
		t.Fatal("Enum accepted an object")
	}
}

func TestFileNormalizesMediaConstraints(t *testing.T) {
	typ := fileType(t, "image/*", "application/pdf", "image/*")
	if got, want := typ.Media(), []string{"application/pdf", "image/*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Media() = %v, want %v", got, want)
	}
	for _, media := range []string{"", "image", "image/", "*/png", "image/**", "image/png/extra", "Image/png"} {
		if _, err := File(media); err == nil {
			t.Errorf("File(%q) accepted malformed media", media)
		}
	}
}

func TestTypeAccessorsDefensivelyCopy(t *testing.T) {
	field := required(t, "name", String())
	object := objectType(t, field)
	fields := object.Fields()
	fields[0] = optional(t, "other", Boolean())
	if got := object.Fields()[0].Name(); got != "name" {
		t.Fatalf("Fields exposed mutable data: got %q", got)
	}

	enum := enumType(t, mustLiteral(t, `"a"`))
	values := enum.EnumValues()
	values[0] = mustLiteral(t, `"b"`)
	if !enum.EnumValues()[0].Equal(mustLiteral(t, `"a"`)) {
		t.Fatal("EnumValues exposed mutable data")
	}

	file := fileType(t, "image/*")
	media := file.Media()
	media[0] = "application/pdf"
	if got := file.Media()[0]; got != "image/*" {
		t.Fatalf("Media exposed mutable data: got %q", got)
	}

	list := listType(t, String())
	element, ok := list.Element()
	if !ok || element.Kind() != StringKind {
		t.Fatalf("Element() = (%v, %v), want string, true", element.Kind(), ok)
	}
	if _, ok := String().Element(); ok {
		t.Fatal("scalar type returned an element")
	}
}

func TestFieldAccessorsExposeDeclaredProperties(t *testing.T) {
	field := optional(t, "maybe", String())
	if field.Name() != "maybe" || field.Type().Kind() != StringKind || !field.Optional() {
		t.Fatalf("Field accessors = (%q, %v, %v)", field.Name(), field.Type().Kind(), field.Optional())
	}
}
