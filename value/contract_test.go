package value

import (
	"reflect"
	"testing"
)

func TestContractsAreExplicitAndResolveDeclaredObjectPaths(t *testing.T) {
	if (Contract{}).Valid() {
		t.Fatal("zero Contract is valid")
	}
	empty := EmptyContract()
	if !empty.Valid() {
		t.Fatal("EmptyContract is invalid")
	}
	if empty.Equal(Contract{}) {
		t.Fatal("EmptyContract equals a zero Contract")
	}

	child := objectType(t, required(t, "name", String()))
	contract := newContract(t, required(t, "result", child))
	if got, ok := contract.Resolve("result", "name"); !ok || got.Kind() != StringKind {
		t.Fatalf("Resolve(result, name) = (%v, %v), want string, true", got.Kind(), ok)
	}
	for _, path := range [][]string{{}, {"missing"}, {"result", "missing"}, {"result", "name", "extra"}} {
		if _, ok := contract.Resolve(path...); ok {
			t.Errorf("Resolve(%v) unexpectedly succeeded", path)
		}
	}
	if got := contract.ObjectType(); got.Kind() != ObjectKind {
		t.Fatalf("ObjectType().Kind() = %v, want ObjectKind", got.Kind())
	}
}

func mustContract(t *testing.T, contract Contract, err error) Contract {
	t.Helper()
	if err != nil {
		t.Fatalf("NewContract: %v", err)
	}
	return contract
}

func newContract(t *testing.T, ports ...Field) Contract {
	t.Helper()
	contract, err := NewContract(ports...)
	return mustContract(t, contract, err)
}

func TestNewContractRejectsInvalidAndDuplicatePorts(t *testing.T) {
	if _, err := NewContract(Field{}); err == nil {
		t.Fatal("NewContract accepted invalid port")
	}
	port := required(t, "value", String())
	if _, err := NewContract(port, port); err == nil {
		t.Fatal("NewContract accepted duplicate ports")
	}
}

func TestContractPortsDefensivelyCopyAndCompareSemantically(t *testing.T) {
	alpha := required(t, "alpha", String())
	beta := optional(t, "beta", Boolean())
	contract := newContract(t, beta, alpha)
	ports := contract.Ports()
	if got, want := []string{ports[0].Name(), ports[1].Name()}, []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Ports() = %v, want %v", got, want)
	}
	ports[0] = beta
	if got := contract.Ports()[0].Name(); got != "alpha" {
		t.Fatalf("Ports exposed mutable data: got %q", got)
	}
	if !contract.Equal(newContract(t, alpha, beta)) {
		t.Fatal("equivalent contracts are not equal")
	}
}
