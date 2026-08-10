package scheduler

import "bytes"

// ComponentKind identifies one stable semantic step in an execution path.
type ComponentKind uint8

const (
	AuthoredChildComponent ComponentKind = iota + 1
	BranchCaseComponent
	MapItemComponent
	LoopIterationComponent
	CleanupComponent
)

// Component is one immutable stable semantic path step.
type Component struct {
	kind       ComponentKind
	name       string
	item       []byte
	occurrence uint64
	iteration  uint64
}

// Kind returns the component's closed-algebra variant.
func (c Component) Kind() ComponentKind { return c.kind }

// AuthoredChild returns the authored child name for an authored-child component.
func (c Component) AuthoredChild() (string, bool) {
	if c.kind != AuthoredChildComponent {
		return "", false
	}
	return c.name, true
}

// BranchCase returns the selected case name for a branch-case component.
func (c Component) BranchCase() (string, bool) {
	if c.kind != BranchCaseComponent {
		return "", false
	}
	return c.name, true
}

// MapItem returns copied canonical item bytes and its zero-based duplicate
// occurrence for a map-item component.
func (c Component) MapItem() ([]byte, uint64, bool) {
	if c.kind != MapItemComponent {
		return nil, 0, false
	}
	return append([]byte(nil), c.item...), c.occurrence, true
}

// LoopIteration returns the one-based semantic iteration for a loop component.
func (c Component) LoopIteration() (uint64, bool) {
	if c.kind != LoopIterationComponent {
		return 0, false
	}
	return c.iteration, true
}

// IsCleanup reports whether c is the fixed cleanup component.
func (c Component) IsCleanup() bool { return c.kind == CleanupComponent }

// Path is an immutable sequence of stable structured execution components.
// The zero Path is the root path.
type Path struct {
	components []Component
}

// AuthoredChild returns a copied path extended with an authored child name.
func (p Path) AuthoredChild(name string) Path {
	return p.append(Component{kind: AuthoredChildComponent, name: name})
}

// BranchCase returns a copied path extended with a selected branch case name.
func (p Path) BranchCase(name string) Path {
	return p.append(Component{kind: BranchCaseComponent, name: name})
}

// MapItem returns a copied path extended with canonical map item bytes and a
// zero-based occurrence among byte-identical input items.
func (p Path) MapItem(item []byte, occurrence uint64) (Path, error) {
	return p.append(Component{kind: MapItemComponent, item: append([]byte(nil), item...), occurrence: occurrence}), nil
}

// LoopIteration returns a copied path extended with a one-based semantic loop
// iteration.
func (p Path) LoopIteration(iteration uint64) (Path, error) {
	if iteration == 0 {
		return Path{}, errInvalidLoopIteration
	}
	return p.append(Component{kind: LoopIterationComponent, iteration: iteration}), nil
}

// Cleanup returns a copied path extended with the fixed cleanup component.
func (p Path) Cleanup() Path { return p.append(Component{kind: CleanupComponent}) }

// Components returns a defensive copy of p's structured components.
func (p Path) Components() []Component {
	components := make([]Component, len(p.components))
	for i, component := range p.components {
		components[i] = copyComponent(component)
	}
	return components
}

func (p Path) append(component Component) Path {
	components := make([]Component, len(p.components)+1)
	for i, existing := range p.components {
		components[i] = copyComponent(existing)
	}
	components[len(p.components)] = copyComponent(component)
	return Path{components: components}
}

func (p Path) valid() bool {
	for _, component := range p.components {
		switch component.kind {
		case AuthoredChildComponent, BranchCaseComponent:
			if component.item != nil || component.occurrence != 0 || component.iteration != 0 {
				return false
			}
		case MapItemComponent:
			if component.name != "" || component.iteration != 0 {
				return false
			}
		case LoopIterationComponent:
			if component.name != "" || component.item != nil || component.occurrence != 0 || component.iteration == 0 {
				return false
			}
		case CleanupComponent:
			if component.name != "" || component.item != nil || component.occurrence != 0 || component.iteration != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func copyComponent(component Component) Component {
	component.item = append([]byte(nil), component.item...)
	return component
}

// comparePath compares stable path components lexically. It deliberately does
// not render or encode paths; representation is deferred to durable identity.
func comparePath(left, right Path) int {
	length := min(len(left.components), len(right.components))
	for i := 0; i < length; i++ {
		if comparison := compareComponent(left.components[i], right.components[i]); comparison != 0 {
			return comparison
		}
	}
	switch {
	case len(left.components) < len(right.components):
		return -1
	case len(left.components) > len(right.components):
		return 1
	default:
		return 0
	}
}

func compareComponent(left, right Component) int {
	if left.kind < right.kind {
		return -1
	}
	if left.kind > right.kind {
		return 1
	}
	switch left.kind {
	case AuthoredChildComponent, BranchCaseComponent:
		if left.name < right.name {
			return -1
		}
		if left.name > right.name {
			return 1
		}
	case MapItemComponent:
		if comparison := bytes.Compare(left.item, right.item); comparison != 0 {
			return comparison
		}
		if left.occurrence < right.occurrence {
			return -1
		}
		if left.occurrence > right.occurrence {
			return 1
		}
	case LoopIterationComponent:
		if left.iteration < right.iteration {
			return -1
		}
		if left.iteration > right.iteration {
			return 1
		}
	}
	return 0
}
