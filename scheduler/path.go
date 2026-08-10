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
	tail          *pathNode
	nonComparable [0]func()
}

type pathNode struct {
	parent    *pathNode
	component Component
	depth     int
	valid     bool
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
	if p.tail == nil {
		return nil
	}
	components := make([]Component, p.tail.depth)
	for node, index := p.tail, p.tail.depth-1; node != nil; node, index = node.parent, index-1 {
		components[index] = copyComponent(node.component)
	}
	return components
}

func (p Path) append(component Component) Path {
	parent := p.tail
	depth := 1
	valid := validComponent(component)
	if parent != nil {
		depth = parent.depth + 1
		valid = parent.valid && valid
	}
	return Path{tail: &pathNode{parent: parent, component: copyComponent(component), depth: depth, valid: valid}}
}

func (p Path) valid() bool {
	return p.tail == nil || p.tail.valid
}

func (p Path) empty() bool { return p.tail == nil }

func validComponent(component Component) bool {
	switch component.kind {
	case AuthoredChildComponent, BranchCaseComponent:
		return component.item == nil && component.occurrence == 0 && component.iteration == 0
	case MapItemComponent:
		return component.name == "" && component.iteration == 0
	case LoopIterationComponent:
		return component.name == "" && component.item == nil && component.occurrence == 0 && component.iteration != 0
	case CleanupComponent:
		return component.name == "" && component.item == nil && component.occurrence == 0 && component.iteration == 0
	default:
		return false
	}
}

func copyComponent(component Component) Component {
	component.item = append([]byte(nil), component.item...)
	return component
}

// comparePath compares stable path components lexically. It deliberately does
// not render or encode paths; representation is deferred to durable identity.
func comparePath(left, right Path) int {
	leftComponents := left.Components()
	rightComponents := right.Components()
	length := min(len(leftComponents), len(rightComponents))
	for i := 0; i < length; i++ {
		if comparison := compareComponent(leftComponents[i], rightComponents[i]); comparison != 0 {
			return comparison
		}
	}
	switch {
	case len(leftComponents) < len(rightComponents):
		return -1
	case len(leftComponents) > len(rightComponents):
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
