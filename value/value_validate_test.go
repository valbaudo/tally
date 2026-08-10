package value

import (
	"reflect"
	"testing"
)

func TestTypeValidateValueAcceptsExactRecursiveValues(t *testing.T) {
	file := testFile(t, "notes.txt", "text/plain; charset=utf-8", []byte("notes"))
	tree := testTree(t)
	fileConstraint := fileType(t, "text/plain")
	typ := objectType(t,
		required(t, "document", fileConstraint),
		required(t, "files", listType(t, fileConstraint)),
		required(t, "named", mapType(t, fileConstraint)),
		required(t, "bundle", objectType(t,
			required(t, "tree", Tree()),
			optional(t, "note", Null()),
		)),
	)
	value := mustObject(t,
		mustEntry(t, "document", NewFileValue(file)),
		mustEntry(t, "files", NewList(NewFileValue(file))),
		mustEntry(t, "named", mustMap(t, mustEntry(t, "first", NewFileValue(file)))),
		mustEntry(t, "bundle", mustObject(t, mustEntry(t, "tree", NewTreeValue(tree)))),
	)
	if err := typ.Validate(value); err != nil {
		t.Fatalf("Validate matching value: %v", err)
	}
}

func TestContractValidateValueDistinguishesAbsentOptionalFromNull(t *testing.T) {
	contract := newContract(t,
		required(t, "name", String()),
		optional(t, "note", Null()),
	)
	if err := contract.Validate(mustObject(t, mustEntry(t, "name", NewString("dawn")))); err != nil {
		t.Fatalf("absent optional: %v", err)
	}
	if err := contract.Validate(mustObject(t, mustEntry(t, "name", NewString("dawn")), mustEntry(t, "note", NewNull()))); err != nil {
		t.Fatalf("explicit matching null: %v", err)
	}
	if err := contract.Validate(mustObject(t, mustEntry(t, "name", NewString("dawn")), mustEntry(t, "note", NewString("null")))); err == nil {
		t.Fatal("string null satisfied a null optional port")
	}
	if err := (Contract{}).Validate(mustObject(t)); err == nil {
		t.Fatal("zero contract validated a value")
	}
}

func TestTypeValidateValueRejectsMismatchWithoutProjectionOrCoercion(t *testing.T) {
	file := testFile(t, "image.png", "image/png", []byte("png"))
	tests := []struct {
		name  string
		typ   Type
		value Value
	}{
		{"missing required field", objectType(t, required(t, "name", String())), mustObject(t)},
		{"undeclared object field", objectType(t, required(t, "name", String())), mustObject(t, mustEntry(t, "name", NewString("dawn")), mustEntry(t, "extra", NewBoolean(true)))},
		{"object is not map", mapType(t, String()), mustObject(t, mustEntry(t, "key", NewString("value")))},
		{"map is not object", objectType(t, required(t, "key", String())), mustMap(t, mustEntry(t, "key", NewString("value")))},
		{"map element", mapType(t, String()), mustMap(t, mustEntry(t, "key", NewBoolean(true)))},
		{"list element", listType(t, String()), NewList(NewString("ok"), NewBoolean(true))},
		{"wrong media", fileType(t, "application/pdf"), NewFileValue(file)},
		{"integer does not accept number", Integer(), mustNumber(t, "1")},
		{"string is not parsed", Integer(), NewString("1")},
		{"closed object is not projected", objectType(t, required(t, "x", String())), mustObject(t, mustEntry(t, "x", NewString("x")), mustEntry(t, "y", NewString("y")))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.typ.Validate(tc.value); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
	if err := Number().Validate(mustInteger(t, "1")); err != nil {
		t.Fatalf("number rejected integer: %v", err)
	}
}

func TestTypeValidateValueAnyRejectsHandlesRecursively(t *testing.T) {
	file := NewFileValue(testFile(t, "report.pdf", "application/pdf", []byte("pdf")))
	tree := NewTreeValue(testTree(t))
	values := []Value{
		file,
		tree,
		mustObject(t, mustEntry(t, "file", file)),
		mustMap(t, mustEntry(t, "tree", tree)),
		NewList(NewList(mustObject(t, mustEntry(t, "file", file)))),
	}
	for i, value := range values {
		if err := Any().Validate(value); err == nil {
			t.Errorf("Any accepted handle-containing value %d", i)
		}
	}
	ordinary := mustObject(t, mustEntry(t, "data", NewList(NewString("x"), mustNumber(t, "1.0"), NewNull())))
	if err := Any().Validate(ordinary); err != nil {
		t.Fatalf("Any rejected ordinary recursive value: %v", err)
	}
}

func TestTypeValidateValueMatchesParsedMediaAndEnums(t *testing.T) {
	file := NewFileValue(testFile(t, "notes.txt", "text/plain; charset=utf-8", []byte("notes")))
	for _, constraint := range []string{"text/plain", "text/*"} {
		if err := fileType(t, constraint).Validate(file); err != nil {
			t.Errorf("constraint %q rejected parameterized media: %v", constraint, err)
		}
	}
	if err := enumType(t, mustLiteral(t, `"x"`), mustLiteral(t, `1`), mustLiteral(t, `true`)).Validate(NewString("x")); err != nil {
		t.Fatalf("enum rejected canonical string member: %v", err)
	}
	if err := enumType(t, mustLiteral(t, `1`)).Validate(mustNumber(t, "1")); err != nil {
		t.Fatalf("enum rejected matching scalar ordinary representation: %v", err)
	}
}

func TestTypeValidateValueReportsStructuredPath(t *testing.T) {
	value := mustObject(t, mustEntry(t, "items", NewList(mustMap(t, mustEntry(t, "raw/key", NewBoolean(true))))))
	typ := objectType(t, required(t, "items", listType(t, mapType(t, String()))))
	err := typ.Validate(value)
	validation, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	wantKinds := []SegmentKind{FieldSegment, ListIndexSegment, MapKeySegment}
	segments := validation.Path().Segments()
	gotKinds := make([]SegmentKind, len(segments))
	for i := range segments {
		gotKinds[i] = segments[i].Kind()
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("path kinds = %v, want %v", gotKinds, wantKinds)
	}
	segments[0] = Segment{}
	if validation.Path().Segments()[0].Kind() != FieldSegment {
		t.Fatal("ValidationError.Path exposed mutable storage")
	}
}

func TestTypeValidateValueRejectsInvalidInputs(t *testing.T) {
	if err := (Type{}).Validate(NewNull()); err == nil {
		t.Fatal("zero type validated a value")
	}
	if err := Any().Validate(Value{}); err == nil {
		t.Fatal("Any validated a zero value")
	}
}
