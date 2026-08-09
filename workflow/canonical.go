package workflow

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/valbaudo/dawn/value"
)

const definitionFormat = "dawn.workflow/1"

// These records are the private, captured definition format. They deliberately
// mirror only semantic compiled data; diagnostics and authoring provenance do
// not affect executable workflow identity.
type definitionWire struct {
	Format string    `json:"format"`
	Root   graphWire `json:"root"`
}

type graphWire struct {
	Inputs  contractWire `json:"inputs"`
	Outputs contractWire `json:"outputs"`
	Nodes   []nodeWire   `json:"nodes"`
	Edges   []edgeWire   `json:"edges"`
	Cleanup *graphWire   `json:"cleanup"`
}

type contractWire struct {
	Fields []fieldWire `json:"fields"`
}

type fieldWire struct {
	Name     string   `json:"name"`
	Type     typeWire `json:"type"`
	Optional bool     `json:"optional"`
}

type typeWire struct {
	Kind   value.Kind        `json:"kind"`
	Elem   *typeWire         `json:"elem"`
	Fields []fieldWire       `json:"fields"`
	Enum   []json.RawMessage `json:"enum"`
	Media  []string          `json:"media"`
}

type nodeWire struct {
	Name     string        `json:"name"`
	Leaf     *leafWire     `json:"leaf"`
	Scope    *scopeWire    `json:"scope"`
	Literals []literalWire `json:"literals"`
}

type leafWire struct {
	Kind    LeafKind     `json:"kind"`
	Inputs  contractWire `json:"inputs"`
	Outputs contractWire `json:"outputs"`
}

type scopeWire struct {
	Kind   ScopeKind   `json:"kind"`
	Graph  *graphWire  `json:"graph"`
	Branch *branchWire `json:"branch"`
	Map    *mapWire    `json:"map"`
	Loop   *loopWire   `json:"loop"`
}

type branchWire struct {
	Inputs   contractWire `json:"inputs"`
	Outputs  contractWire `json:"outputs"`
	Selector string       `json:"selector"`
	Cases    []caseWire   `json:"cases"`
}

type caseWire struct {
	Name  string    `json:"name"`
	Graph graphWire `json:"graph"`
}

type mapWire struct {
	Inputs     contractWire `json:"inputs"`
	Outputs    contractWire `json:"outputs"`
	Collection string       `json:"collection"`
	Result     string       `json:"result"`
	Body       graphWire    `json:"body"`
}

type loopWire struct {
	Inputs      contractWire `json:"inputs"`
	Outputs     contractWire `json:"outputs"`
	Maximum     int          `json:"maximum"`
	Termination []string     `json:"termination"`
	Body        graphWire    `json:"body"`
}

type endpointWire struct {
	Kind  EndpointKind `json:"kind"`
	Child string       `json:"child"`
}

type edgeWire struct {
	From     endpointWire  `json:"from"`
	To       endpointWire  `json:"to"`
	Bindings []bindingWire `json:"bindings"`
}

type bindingWire struct {
	From              []string `json:"from"`
	To                string   `json:"to"`
	RuntimeValidation bool     `json:"runtimeValidation"`
}

type literalWire struct {
	Input string          `json:"input"`
	Value json.RawMessage `json:"value"`
}

func encodeDefinition(def Definition) ([]byte, error) {
	return json.Marshal(definitionWire{Format: definitionFormat, Root: encodeGraph(def.root)})
}

func encodeGraph(graph Graph) graphWire {
	wire := graphWire{
		Inputs:  encodeContract(graph.inputs),
		Outputs: encodeContract(graph.outputs),
		Nodes:   make([]nodeWire, len(graph.nodes)),
		Edges:   make([]edgeWire, len(graph.edges)),
	}
	for index, node := range graph.nodes {
		wire.Nodes[index] = encodeNode(node)
	}
	sort.Slice(wire.Nodes, func(i, j int) bool { return wire.Nodes[i].Name < wire.Nodes[j].Name })
	for index, edge := range graph.edges {
		wire.Edges[index] = encodeEdge(edge)
	}
	sort.Slice(wire.Edges, func(i, j int) bool { return compareEdges(wire.Edges[i], wire.Edges[j]) < 0 })
	if graph.cleanup != nil {
		cleanup := encodeGraph(graph.cleanup.graph)
		wire.Cleanup = &cleanup
	}
	return wire
}

func encodeContract(contract value.Contract) contractWire {
	ports := contract.Ports()
	wire := contractWire{Fields: make([]fieldWire, len(ports))}
	for index, port := range ports {
		wire.Fields[index] = fieldWire{Name: port.Name(), Type: encodeType(port.Type()), Optional: port.Optional()}
	}
	return wire
}

func encodeType(typ value.Type) typeWire {
	wire := typeWire{Kind: typ.Kind()}
	switch typ.Kind() {
	case value.ListKind, value.MapKind:
		element, _ := typ.Element()
		encoded := encodeType(element)
		wire.Elem = &encoded
	case value.ObjectKind:
		fields := typ.Fields()
		wire.Fields = make([]fieldWire, len(fields))
		for index, field := range fields {
			wire.Fields[index] = fieldWire{Name: field.Name(), Type: encodeType(field.Type()), Optional: field.Optional()}
		}
	case value.EnumKind:
		values := typ.EnumValues()
		wire.Enum = make([]json.RawMessage, len(values))
		for index, member := range values {
			wire.Enum[index] = json.RawMessage(member.Bytes())
		}
	case value.FileKind:
		wire.Media = typ.Media()
	}
	return wire
}

func encodeNode(node Node) nodeWire {
	wire := nodeWire{Name: node.name, Literals: make([]literalWire, len(node.literals))}
	for index, literal := range node.literals {
		wire.Literals[index] = literalWire{Input: literal.input, Value: json.RawMessage(literal.value.Bytes())}
	}
	sort.Slice(wire.Literals, func(i, j int) bool {
		if wire.Literals[i].Input != wire.Literals[j].Input {
			return wire.Literals[i].Input < wire.Literals[j].Input
		}
		return bytes.Compare(wire.Literals[i].Value, wire.Literals[j].Value) < 0
	})
	if node.leaf != nil {
		wire.Leaf = &leafWire{Kind: node.leaf.kind, Inputs: encodeContract(node.leaf.inputs), Outputs: encodeContract(node.leaf.outputs)}
	}
	if node.scope != nil {
		wire.Scope = encodeScope(*node.scope)
	}
	return wire
}

func encodeScope(scope Scope) *scopeWire {
	wire := &scopeWire{Kind: scope.kind}
	switch scope.kind {
	case GraphScope:
		graph := encodeGraph(*scope.graph)
		wire.Graph = &graph
	case BranchScope:
		branch := encodeBranch(*scope.branch)
		wire.Branch = &branch
	case MapScope:
		mapped := encodeMap(*scope.map_)
		wire.Map = &mapped
	case LoopScope:
		loop := encodeLoop(*scope.loop)
		wire.Loop = &loop
	}
	return wire
}

func encodeBranch(branch Branch) branchWire {
	wire := branchWire{
		Inputs: encodeContract(branch.inputs), Outputs: encodeContract(branch.outputs), Selector: branch.selector,
		Cases: make([]caseWire, len(branch.cases)),
	}
	for index, branchCase := range branch.cases {
		wire.Cases[index] = caseWire{Name: branchCase.name, Graph: encodeGraph(branchCase.graph)}
	}
	sort.Slice(wire.Cases, func(i, j int) bool { return wire.Cases[i].Name < wire.Cases[j].Name })
	return wire
}

func encodeMap(mapped Map) mapWire {
	return mapWire{
		Inputs: encodeContract(mapped.inputs), Outputs: encodeContract(mapped.outputs),
		Collection: mapped.collection, Result: mapped.result, Body: encodeGraph(mapped.body),
	}
}

func encodeLoop(loop Loop) loopWire {
	return loopWire{
		Inputs: encodeContract(loop.inputs), Outputs: encodeContract(loop.outputs), Maximum: loop.maximum,
		Termination: append([]string(nil), loop.termination...), Body: encodeGraph(loop.body),
	}
}

func encodeEdge(edge Edge) edgeWire {
	wire := edgeWire{
		From:     endpointWire{Kind: edge.from.kind, Child: edge.from.child},
		To:       endpointWire{Kind: edge.to.kind, Child: edge.to.child},
		Bindings: make([]bindingWire, len(edge.bindings)),
	}
	for index, binding := range edge.bindings {
		wire.Bindings[index] = bindingWire{
			From: append([]string(nil), binding.from...), To: binding.to, RuntimeValidation: binding.runtimeValidation,
		}
	}
	sort.Slice(wire.Bindings, func(i, j int) bool { return compareBindings(wire.Bindings[i], wire.Bindings[j]) < 0 })
	return wire
}

func compareEdges(left, right edgeWire) int {
	if result := compareEndpoints(left.From, right.From); result != 0 {
		return result
	}
	if result := compareEndpoints(left.To, right.To); result != 0 {
		return result
	}
	for index := 0; index < len(left.Bindings) && index < len(right.Bindings); index++ {
		if result := compareBindings(left.Bindings[index], right.Bindings[index]); result != 0 {
			return result
		}
	}
	return compareInt(len(left.Bindings), len(right.Bindings))
}

func compareEndpoints(left, right endpointWire) int {
	if result := compareInt(int(left.Kind), int(right.Kind)); result != 0 {
		return result
	}
	return compareString(left.Child, right.Child)
}

func compareBindings(left, right bindingWire) int {
	if result := compareString(left.To, right.To); result != 0 {
		return result
	}
	if result := compareStrings(left.From, right.From); result != 0 {
		return result
	}
	if left.RuntimeValidation == right.RuntimeValidation {
		return 0
	}
	if !left.RuntimeValidation {
		return -1
	}
	return 1
}

func compareStrings(left, right []string) int {
	for index := 0; index < len(left) && index < len(right); index++ {
		if result := compareString(left[index], right[index]); result != 0 {
			return result
		}
	}
	return compareInt(len(left), len(right))
}

func compareString(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareInt(left, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
