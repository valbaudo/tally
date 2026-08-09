package value

import (
	"bytes"
	"testing"
)

func TestParseLiteralCanonicalizesObjectsAndRejectsInvalidJSONShapes(t *testing.T) {
	literal := mustLiteral(t, ` { "z" : 1.0, "a" : [true, null] } `)
	if got, want := string(literal.Bytes()), `{"a":[true,null],"z":1.0}`; got != want {
		t.Fatalf("Bytes() = %s, want %s", got, want)
	}
	copy := literal.Bytes()
	copy[0] = '['
	if bytes.Equal(copy, literal.Bytes()) {
		t.Fatal("Bytes exposed mutable canonical data")
	}
	if !literal.Equal(mustLiteral(t, `{"a":[true,null],"z":1.0}`)) {
		t.Fatal("equal canonical literals are not equal")
	}
	for _, data := range []string{`1 2`, `{"a":1,"a":2}`, ``, `{`} {
		if _, err := ParseLiteral([]byte(data)); err == nil {
			t.Errorf("ParseLiteral(%q) succeeded", data)
		}
	}
}

func TestValidateLiteralEnforcesTypesAndObjectPresence(t *testing.T) {
	object := objectType(t,
		required(t, "name", String()),
		optional(t, "note", Null()),
	)
	valid := []struct {
		name string
		typ  Type
		json string
	}{
		{"integer", Integer(), "1"},
		{"number accepts integer", Number(), "1"},
		{"number accepts decimal", Number(), "1.0"},
		{"optional absent", object, `{"name":"dawn"}`},
		{"optional explicit null", object, `{"name":"dawn","note":null}`},
		{"homogeneous list", listType(t, Boolean()), "[true,false]"},
		{"homogeneous map", mapType(t, Integer()), `{"a":1,"b":2}`},
		{"enum", enumType(t, mustLiteral(t, `"low"`)), `"low"`},
		{"any accepts json", Any(), `{"anything":[1,false]}`},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.typ.ValidateLiteral(mustLiteral(t, tc.json)); err != nil {
				t.Fatalf("ValidateLiteral(%s): %v", tc.json, err)
			}
		})
	}
	invalid := []struct {
		name string
		typ  Type
		json string
	}{
		{"integer rejects number", Integer(), "1.0"},
		{"string is not parsed", Integer(), `"1"`},
		{"required absent", object, `{}`},
		{"null does not satisfy string", objectType(t, optional(t, "name", String())), `{"name":null}`},
		{"undeclared field", object, `{"name":"dawn","extra":true}`},
		{"list wrong member", listType(t, Boolean()), "[true,1]"},
		{"map wrong member", mapType(t, Integer()), `{"a":1,"b":1.5}`},
		{"enum wrong member", enumType(t, mustLiteral(t, `"low"`)), `"high"`},
		{"file cannot be literal", fileType(t), `"x"`},
		{"tree cannot be literal", Tree(), `{}`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.typ.ValidateLiteral(mustLiteral(t, tc.json)); err == nil {
				t.Fatalf("ValidateLiteral(%s) succeeded", tc.json)
			}
		})
	}
}

func TestContractValidateLiteralUsesPortsAsAClosedObject(t *testing.T) {
	contract := newContract(t,
		required(t, "name", String()),
		optional(t, "count", Integer()),
	)
	if err := contract.ValidateLiteral(mustLiteral(t, `{"name":"dawn"}`)); err != nil {
		t.Fatalf("ValidateLiteral valid contract object: %v", err)
	}
	for _, data := range []string{`{}`, `{"name":"dawn","extra":true}`, `{"name":"dawn","count":1.5}`} {
		if err := contract.ValidateLiteral(mustLiteral(t, data)); err == nil {
			t.Errorf("Contract.ValidateLiteral(%s) succeeded", data)
		}
	}
}

func TestCheckAssignable(t *testing.T) {
	tests := []struct {
		name        string
		from, to    Type
		wantRuntime bool
		wantErr     bool
	}{
		{"exact", String(), String(), false, false},
		{"integer widens", Integer(), Number(), false, false},
		{"enum widens", enumType(t, mustLiteral(t, `"a"`)), String(), false, false},
		{"concrete to any", String(), Any(), false, false},
		{"any to concrete validates later", Any(), String(), true, false},
		{"file is not ordinary any", fileType(t), Any(), false, true},
		{"object with file is not ordinary any", objectType(t, required(t, "document", fileType(t))), Any(), false, true},
		{"list with tree is not ordinary any", listType(t, Tree()), Any(), false, true},
		{"map with file is not ordinary any", mapType(t, fileType(t)), Any(), false, true},
		{"ordinary any cannot satisfy object file", Any(), objectType(t, required(t, "document", fileType(t))), false, true},
		{"ordinary any cannot satisfy list tree", Any(), listType(t, Tree()), false, true},
		{"ordinary any cannot satisfy map file", Any(), mapType(t, fileType(t)), false, true},
		{"string is not parsed", String(), Integer(), false, true},
		{"recursive list", listType(t, Integer()), listType(t, Number()), false, false},
		{"recursive map", mapType(t, Integer()), mapType(t, Number()), false, false},
		{"closed object", objectType(t, required(t, "x", Integer())), objectType(t, required(t, "x", Number())), false, false},
		{"required widens to optional", objectType(t, required(t, "x", String())), objectType(t, optional(t, "x", String())), false, false},
		{"optional cannot satisfy required", objectType(t, optional(t, "x", String())), objectType(t, required(t, "x", String())), false, true},
		{"closed object is not projected", objectType(t, required(t, "x", String()), required(t, "y", String())), objectType(t, required(t, "x", String())), false, true},
		{"tree exact", Tree(), Tree(), false, false},
		{"tree not file", Tree(), fileType(t), false, true},
		{"file media accepted", fileType(t, "image/png"), fileType(t, "image/*"), false, false},
		{"file unconstrained needs validation", fileType(t), fileType(t, "image/*"), true, false},
		{"file disjoint exact media", fileType(t, "image/jpeg"), fileType(t, "image/png"), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckAssignable(tc.from, tc.to)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckAssignable() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got.RuntimeValidation != tc.wantRuntime {
				t.Fatalf("RuntimeValidation = %v, want %v", got.RuntimeValidation, tc.wantRuntime)
			}
		})
	}
}
