package value

import (
	"context"
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
	"time"
)

func TestMaterializeLiteralUsesTargetStructure(t *testing.T) {
	objectType := objectType(t, required(t, "name", String()))
	mapType := mapType(t, String())
	literal := mustLiteral(t, `{"name":"dawn"}`)

	object, err := MaterializeLiteral(literal, objectType)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := MaterializeLiteral(literal, mapType)
	if err != nil {
		t.Fatal(err)
	}
	if object.Kind() != ObjectKind || mapped.Kind() != MapKind {
		t.Fatalf("kinds = %v, %v", object.Kind(), mapped.Kind())
	}
}

func TestMaterializeLiteralMaterializesOrdinaryTargets(t *testing.T) {
	stringEnum := enumType(t, mustLiteral(t, `"dawn"`))
	integerEnum := enumType(t, mustLiteral(t, `12`))
	numberEnum := enumType(t, mustLiteral(t, `1.5`))
	booleanEnum := enumType(t, mustLiteral(t, `true`))
	nullEnum := enumType(t, mustLiteral(t, `null`))
	nestedType := objectType(t,
		required(t, "items", listType(t, objectType(t, required(t, "name", String())))),
		required(t, "labels", mapType(t, Boolean())),
	)

	tests := []struct {
		name    string
		literal string
		target  Type
		want    Value
	}{
		{"string", `"dawn"`, String(), NewString("dawn")},
		{"integer", `-12`, Integer(), mustInteger(t, "-12")},
		{"fractional number", `1.50`, Number(), mustNumber(t, "1.50")},
		{"exponent number", `1e+6`, Number(), mustNumber(t, "1e+6")},
		{"boolean", `true`, Boolean(), NewBoolean(true)},
		{"null", `null`, Null(), NewNull()},
		{"string enum", `"dawn"`, stringEnum, NewString("dawn")},
		{"integer enum", `12`, integerEnum, mustInteger(t, "12")},
		{"number enum", `1.5`, numberEnum, mustNumber(t, "1.5")},
		{"boolean enum", `true`, booleanEnum, NewBoolean(true)},
		{"null enum", `null`, nullEnum, NewNull()},
		{
			"nested object map list",
			`{"labels":{"alpha":true,"beta":false},"items":[{"name":"first"},{"name":"second"}]}`,
			nestedType,
			mustObject(t,
				mustEntry(t, "items", NewList(
					mustObject(t, mustEntry(t, "name", NewString("first"))),
					mustObject(t, mustEntry(t, "name", NewString("second"))),
				)),
				mustEntry(t, "labels", mustMap(t,
					mustEntry(t, "alpha", NewBoolean(true)),
					mustEntry(t, "beta", NewBoolean(false)),
				)),
			),
		},
		{
			"any object is map",
			`{"members":[1,2.5],"name":"dawn"}`,
			Any(),
			mustMap(t,
				mustEntry(t, "members", NewList(mustInteger(t, "1"), mustNumber(t, "2.5"))),
				mustEntry(t, "name", NewString("dawn")),
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MaterializeLiteral(mustLiteral(t, tc.literal), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("value = %v, want %v", got.Canonical(), tc.want.Canonical())
			}
		})
	}
}

func TestMaterializeLiteralRejectsInvalidOrIncompatibleInput(t *testing.T) {
	file := fileType(t, "application/pdf")
	tests := []struct {
		name    string
		literal Literal
		target  Type
	}{
		{"invalid literal", Literal{}, String()},
		{"invalid target", mustLiteral(t, `"dawn"`), Type{}},
		{"target mismatch", mustLiteral(t, `"dawn"`), Integer()},
		{"file target", mustLiteral(t, `null`), file},
		{"tree target", mustLiteral(t, `null`), Tree()},
		{"undeclared object member", mustLiteral(t, `{"extra":true}`), objectType(t)},
		{"absent required field", mustLiteral(t, `{}`), objectType(t, required(t, "name", String()))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MaterializeLiteral(tc.literal, tc.target); err == nil {
				t.Fatal("MaterializeLiteral accepted invalid input")
			}
		})
	}
}

func TestMaterializeLiteralReturnsDefensiveImmutableValue(t *testing.T) {
	value, err := MaterializeLiteral(mustLiteral(t, `{"items":["dawn"]}`), Any())
	if err != nil {
		t.Fatal(err)
	}
	before := value.Canonical()
	entries := value.Entries()
	entries[0] = Entry{}
	items := value.Entries()[0].Value().Items()
	items[0] = Value{}
	if got := value.Canonical(); string(got) != string(before) {
		t.Fatalf("value mutated through accessor: %v, want %v", got, before)
	}
}

func TestMaterializeLiteralDeepFiniteSubprocess(t *testing.T) {
	if os.Getenv("DAWN_DEEP_LITERAL_MATERIALIZATION_CHILD") == "1" {
		debug.SetMaxStack(1 << 20)
		target := Null()
		var decoded any
		for range 50_000 {
			child := target
			target = Type{kind: ListKind, elem: &child}
			decoded = []any{decoded}
		}
		literal := Literal{canonical: []byte("deep"), decoded: decoded}
		value, err := MaterializeLiteral(literal, target)
		if err != nil {
			t.Fatal(err)
		}
		if !value.Valid() || value.Kind() != ListKind {
			t.Fatalf("deep materialization = (%v, valid=%v)", value.Kind(), value.Valid())
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMaterializeLiteralDeepFiniteSubprocess$", "-test.count=1")
	command.Env = append(os.Environ(), "DAWN_DEEP_LITERAL_MATERIALIZATION_CHILD=1")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("deep literal-materialization child timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("deep literal-materialization child failed: %v\n%s", err, output)
	}
}
