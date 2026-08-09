package workflow

import "github.com/valbaudo/dawn/value"

// LeafKind is one member of Dawn's closed leaf algebra.
type LeafKind string

const (
	LLM    LeafKind = "llm"
	Agent  LeafKind = "agent"
	Script LeafKind = "script"
	Gate   LeafKind = "gate"
)

func validLeafKind(kind LeafKind) bool {
	switch kind {
	case LLM, Agent, Script, Gate:
		return true
	default:
		return false
	}
}

// ScopeKind is one member of Dawn's closed scope algebra.
type ScopeKind string

const (
	GraphScope   ScopeKind = "graph"
	BranchScope  ScopeKind = "branch"
	MapScope     ScopeKind = "map"
	LoopScope    ScopeKind = "loop"
	FinallyScope ScopeKind = "finally"
)

func validScopeKind(kind ScopeKind) bool {
	switch kind {
	case GraphScope, BranchScope, MapScope, LoopScope, FinallyScope:
		return true
	default:
		return false
	}
}

// Status is one workflow terminal outcome.
type Status string

const (
	Succeeded Status = "succeeded"
	Rejected  Status = "rejected"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Origin identifies the lowering origin recorded for diagnostics.
type Origin string

const (
	OriginAuthored Origin = "authored"
	OriginCall     Origin = "call"
	OriginParallel Origin = "parallel"
)

// Provenance holds source and lowering labels used in diagnostics.
type Provenance struct {
	Source string
	Module string
	Origin Origin
}

// Definition is one immutable compiled workflow definition.
type Definition struct {
	root      Graph
	canonical []byte
}

// Root returns the definition's root graph.
func (d Definition) Root() Graph { return d.root }

// Graph is an immutable contracted hierarchical region.
type Graph struct {
	inputs, outputs value.Contract
	nodes           []Node
	edges           []Edge
	cleanup         *Finally
	provenance      Provenance
}

// Inputs returns the graph input contract.
func (g Graph) Inputs() value.Contract { return g.inputs }

// Outputs returns the graph output contract.
func (g Graph) Outputs() value.Contract { return g.outputs }

// Nodes returns a copy of the graph's immediate nodes.
func (g Graph) Nodes() []Node { return append([]Node(nil), g.nodes...) }

// Edges returns a copy of the graph's immediate edges.
func (g Graph) Edges() []Edge { return append([]Edge(nil), g.edges...) }

// Finally returns the graph's attached cleanup scope, if any.
func (g Graph) Finally() (Finally, bool) {
	if g.cleanup == nil {
		return Finally{}, false
	}
	return *g.cleanup, true
}

// Provenance returns the graph's diagnostic provenance.
func (g Graph) Provenance() Provenance { return g.provenance }

// Node is one immutable immediate graph child.
type Node struct {
	name       string
	leaf       *Leaf
	scope      *Scope
	literals   []LiteralBinding
	provenance Provenance
}

// Name returns the node's sibling-unique name.
func (n Node) Name() string { return n.name }

// Leaf returns the node's leaf variant, if it has one.
func (n Node) Leaf() (Leaf, bool) {
	if n.leaf == nil {
		return Leaf{}, false
	}
	return *n.leaf, true
}

// Scope returns the node's scope variant, if it has one.
func (n Node) Scope() (Scope, bool) {
	if n.scope == nil {
		return Scope{}, false
	}
	return *n.scope, true
}

// ScopeKind returns the node scope kind, if the node is a scope.
func (n Node) ScopeKind() ScopeKind {
	if n.scope == nil {
		return ""
	}
	return n.scope.kind
}

// Literals returns a copy of the node's direct input literal bindings.
func (n Node) Literals() []LiteralBinding { return append([]LiteralBinding(nil), n.literals...) }

// Provenance returns the node's diagnostic provenance.
func (n Node) Provenance() Provenance { return n.provenance }

// Leaf is one canonical externally observable workflow operation.
type Leaf struct {
	kind            LeafKind
	inputs, outputs value.Contract
}

// Kind returns the leaf's closed kind.
func (l Leaf) Kind() LeafKind { return l.kind }

// Inputs returns the leaf input contract.
func (l Leaf) Inputs() value.Contract { return l.inputs }

// Outputs returns the leaf output contract.
func (l Leaf) Outputs() value.Contract { return l.outputs }

// Scope is a closed tagged union over canonical structured scopes.
type Scope struct {
	kind   ScopeKind
	graph  *Graph
	branch *Branch
	map_   *Map
	loop   *Loop
}

// Kind returns the scope's closed kind.
func (s Scope) Kind() ScopeKind { return s.kind }

// Graph returns the graph scope body, if this is a graph scope.
func (s Scope) Graph() (Graph, bool) {
	if s.kind != GraphScope || s.graph == nil {
		return Graph{}, false
	}
	return *s.graph, true
}

// Branch returns the branch scope record, if this is a branch scope.
func (s Scope) Branch() (Branch, bool) {
	if s.kind != BranchScope || s.branch == nil {
		return Branch{}, false
	}
	return *s.branch, true
}

// Map returns the map scope record, if this is a map scope.
func (s Scope) Map() (Map, bool) {
	if s.kind != MapScope || s.map_ == nil {
		return Map{}, false
	}
	return *s.map_, true
}

// Loop returns the loop scope record, if this is a loop scope.
func (s Scope) Loop() (Loop, bool) {
	if s.kind != LoopScope || s.loop == nil {
		return Loop{}, false
	}
	return *s.loop, true
}

// Branch is one canonical branch scope record.
type Branch struct {
	inputs, outputs value.Contract
	selector        string
	cases           []Case
}

// Inputs returns the branch input contract.
func (b Branch) Inputs() value.Contract { return b.inputs }

// Outputs returns the branch output contract.
func (b Branch) Outputs() value.Contract { return b.outputs }

// Selector returns the branch selector input name.
func (b Branch) Selector() string { return b.selector }

// Cases returns a copy of the branch alternatives.
func (b Branch) Cases() []Case { return append([]Case(nil), b.cases...) }

// Case is one immutable named branch alternative.
type Case struct {
	name  string
	graph Graph
}

// Name returns the case name.
func (c Case) Name() string { return c.name }

// Graph returns the alternative's graph.
func (c Case) Graph() Graph { return c.graph }

// Map is one canonical collection scope record.
type Map struct {
	inputs, outputs    value.Contract
	collection, result string
	body               Graph
}

// Inputs returns the map input contract.
func (m Map) Inputs() value.Contract { return m.inputs }

// Outputs returns the map output contract.
func (m Map) Outputs() value.Contract { return m.outputs }

// Collection returns the collection input name.
func (m Map) Collection() string { return m.collection }

// Result returns the result output name.
func (m Map) Result() string { return m.result }

// Body returns the map child template.
func (m Map) Body() Graph { return m.body }

// Loop is one canonical bounded sequential scope record.
type Loop struct {
	inputs, outputs value.Contract
	maximum         int
	termination     []string
	body            Graph
}

// Inputs returns the loop input contract.
func (l Loop) Inputs() value.Contract { return l.inputs }

// Outputs returns the loop output contract.
func (l Loop) Outputs() value.Contract { return l.outputs }

// Maximum returns the loop's static iteration bound.
func (l Loop) Maximum() int { return l.maximum }

// Termination returns a copy of the body output path that terminates the loop.
func (l Loop) Termination() []string { return append([]string(nil), l.termination...) }

// Body returns the loop child template.
func (l Loop) Body() Graph { return l.body }

// Finally is the sole protected cleanup scope attached to a graph.
type Finally struct{ graph Graph }

// Kind returns FinallyScope.
func (Finally) Kind() ScopeKind { return FinallyScope }

// Graph returns the cleanup graph.
func (f Finally) Graph() Graph { return f.graph }

// Endpoint identifies one graph boundary or immediate child endpoint.
type Endpoint struct {
	kind  EndpointKind
	child string
}

// Kind returns the endpoint kind.
func (e Endpoint) Kind() EndpointKind { return e.kind }

// Child returns the immediate child name, or an empty string for a boundary.
func (e Endpoint) Child() string { return e.child }

// Binding is one validated port binding on an edge.
type Binding struct {
	from              []string
	to                string
	runtimeValidation bool
}

// From returns a copy of the source port path.
func (b Binding) From() []string { return append([]string(nil), b.from...) }

// To returns the target top-level port.
func (b Binding) To() string { return b.to }

// RuntimeValidation reports whether the binding needs boundary validation.
func (b Binding) RuntimeValidation() bool { return b.runtimeValidation }

// Edge is the sole immutable dependency relation in a graph.
type Edge struct {
	from, to Endpoint
	bindings []Binding
}

// From returns the source endpoint.
func (e Edge) From() Endpoint { return e.from }

// To returns the target endpoint.
func (e Edge) To() Endpoint { return e.to }

// Bindings returns a copy of the edge bindings.
func (e Edge) Bindings() []Binding { return append([]Binding(nil), e.bindings...) }

// LiteralBinding is one immutable direct child-input literal binding.
type LiteralBinding struct {
	input string
	value value.Literal
}

// Input returns the target input port.
func (b LiteralBinding) Input() string { return b.input }

// Value returns the bound ordinary literal.
func (b LiteralBinding) Value() value.Literal { return b.value }
