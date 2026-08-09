package value

import (
	"fmt"
	"sort"
)

// Contract is a valid, closed set of top-level ports.
type Contract struct {
	valid bool
	ports []Field
}

// NewContract constructs a closed contract from declared top-level ports.
func NewContract(ports ...Field) (Contract, error) {
	copy := append([]Field(nil), ports...)
	for _, port := range copy {
		if !validField(port) {
			return Contract{}, fmt.Errorf("contract has an invalid port")
		}
	}
	sort.Slice(copy, func(i, j int) bool { return copy[i].name < copy[j].name })
	for i := 1; i < len(copy); i++ {
		if copy[i-1].name == copy[i].name {
			return Contract{}, fmt.Errorf("contract has duplicate port %q", copy[i].name)
		}
	}
	return Contract{valid: true, ports: copy}, nil
}

// EmptyContract constructs an explicitly valid contract with no ports.
func EmptyContract() Contract { return Contract{valid: true} }

// Valid reports whether c was constructed as a contract.
func (c Contract) Valid() bool {
	if !c.valid {
		return false
	}
	for i, port := range c.ports {
		if !validField(port) || (i > 0 && c.ports[i-1].name >= port.name) {
			return false
		}
	}
	return true
}

// Ports returns a copy of the contract's declared ports.
func (c Contract) Ports() []Field { return append([]Field(nil), c.ports...) }

// Resolve finds a top-level port followed by statically declared object fields.
func (c Contract) Resolve(path ...string) (Type, bool) {
	if !c.Valid() || len(path) == 0 {
		return Type{}, false
	}
	current, ok := findField(c.ports, path[0])
	if !ok {
		return Type{}, false
	}
	for _, part := range path[1:] {
		if current.kind != ObjectKind {
			return Type{}, false
		}
		var found bool
		current, found = findField(current.fields, part)
		if !found {
			return Type{}, false
		}
	}
	return current, true
}

func findField(fields []Field, name string) (Type, bool) {
	index := sort.Search(len(fields), func(i int) bool { return fields[i].name >= name })
	if index == len(fields) || fields[index].name != name {
		return Type{}, false
	}
	return fields[index].typ, true
}

// Equal reports whether two contracts have the same ports and port types.
func (c Contract) Equal(other Contract) bool {
	if !c.Valid() || !other.Valid() || len(c.ports) != len(other.ports) {
		return false
	}
	for i := range c.ports {
		if c.ports[i].name != other.ports[i].name || c.ports[i].optional != other.ports[i].optional || !c.ports[i].typ.Equal(other.ports[i].typ) {
			return false
		}
	}
	return true
}

// ObjectType returns the contract as a closed object type.
func (c Contract) ObjectType() Type {
	if !c.Valid() {
		return Type{}
	}
	typ, err := Object(c.ports...)
	if err != nil {
		return Type{}
	}
	return typ
}
