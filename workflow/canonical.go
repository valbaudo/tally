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

// stringWire holds the original Go string bytes. Encoding JSON strings from
// Go strings would replace invalid UTF-8 and collapse distinct definitions.
type stringWire []byte

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
	Name     stringWire `json:"name"`
	Type     typeWire   `json:"type"`
	Optional bool       `json:"optional"`
}

type typeWire struct {
	Nodes []typeNodeWire `json:"nodes"`
}

// typeNodeWire is one node in a prefix-ordered type tree. Keeping the private
// canonical representation flat makes both construction and JSON encoding
// stack-safe for deeply nested finite public types.
type typeNodeWire struct {
	Kind   value.Kind        `json:"kind"`
	Fields []typeFieldWire   `json:"fields"`
	Enum   []json.RawMessage `json:"enum"`
	Media  []stringWire      `json:"media"`
}

type typeFieldWire struct {
	Name     stringWire `json:"name"`
	Optional bool       `json:"optional"`
}

type nodeWire struct {
	Name     stringWire    `json:"name"`
	Leaf     *leafWire     `json:"leaf"`
	Scope    *scopeWire    `json:"scope"`
	Literals []literalWire `json:"literals"`
}

type leafWire struct {
	Kind             stringWire       `json:"kind"`
	Inputs           contractWire     `json:"inputs"`
	Outputs          contractWire     `json:"outputs"`
	BaseTree         []stringWire     `json:"baseTree"`
	PublishWorkspace []stringWire     `json:"publishWorkspace"`
	Attachments      []attachmentWire `json:"attachments"`
}

type attachmentWire struct {
	Input    []stringWire `json:"input"`
	Fidelity Fidelity     `json:"fidelity"`
}

type scopeWire struct {
	Kind   stringWire  `json:"kind"`
	Graph  *graphWire  `json:"graph"`
	Branch *branchWire `json:"branch"`
	Map    *mapWire    `json:"map"`
	Loop   *loopWire   `json:"loop"`
}

type branchWire struct {
	Inputs   contractWire `json:"inputs"`
	Outputs  contractWire `json:"outputs"`
	Selector stringWire   `json:"selector"`
	Cases    []caseWire   `json:"cases"`
}

type caseWire struct {
	Name  stringWire `json:"name"`
	Graph graphWire  `json:"graph"`
}

type mapWire struct {
	Inputs     contractWire `json:"inputs"`
	Outputs    contractWire `json:"outputs"`
	Collection stringWire   `json:"collection"`
	Result     stringWire   `json:"result"`
	Body       graphWire    `json:"body"`
}

type loopWire struct {
	Inputs      contractWire `json:"inputs"`
	Outputs     contractWire `json:"outputs"`
	Maximum     int          `json:"maximum"`
	Termination []stringWire `json:"termination"`
	Body        graphWire    `json:"body"`
}

type endpointWire struct {
	Kind  EndpointKind `json:"kind"`
	Child stringWire   `json:"child"`
}

type edgeWire struct {
	From     endpointWire  `json:"from"`
	To       endpointWire  `json:"to"`
	Bindings []bindingWire `json:"bindings"`
}

type bindingWire struct {
	From              []stringWire `json:"from"`
	To                stringWire   `json:"to"`
	RuntimeValidation bool         `json:"runtimeValidation"`
}

type literalWire struct {
	Input stringWire      `json:"input"`
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
	for index, edge := range graph.edges {
		wire.Edges[index] = encodeEdge(edge)
	}
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
		wire.Fields[index] = fieldWire{Name: encodeString(port.Name()), Type: encodeType(port.Type()), Optional: port.Optional()}
	}
	return wire
}

func encodeType(typ value.Type) typeWire {
	wire := typeWire{}
	pending := []value.Type{typ}
	for len(pending) != 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		node := typeNodeWire{Kind: current.Kind()}
		switch current.Kind() {
		case value.ListKind, value.MapKind:
			element, _ := current.Element()
			pending = append(pending, element)
		case value.ObjectKind:
			fields := current.Fields()
			node.Fields = make([]typeFieldWire, len(fields))
			for index, field := range fields {
				node.Fields[index] = typeFieldWire{Name: encodeString(field.Name()), Optional: field.Optional()}
			}
			for index := len(fields) - 1; index >= 0; index-- {
				pending = append(pending, fields[index].Type())
			}
		case value.EnumKind:
			values := current.EnumValues()
			node.Enum = make([]json.RawMessage, len(values))
			for index, member := range values {
				node.Enum[index] = json.RawMessage(member.Bytes())
			}
		case value.FileKind:
			media := current.Media()
			node.Media = make([]stringWire, len(media))
			for index, constraint := range media {
				node.Media[index] = encodeString(constraint)
			}
		}
		wire.Nodes = append(wire.Nodes, node)
	}
	return wire
}

func encodeNode(node Node) nodeWire {
	wire := nodeWire{Name: encodeString(node.name), Literals: make([]literalWire, len(node.literals))}
	for index, literal := range node.literals {
		wire.Literals[index] = literalWire{Input: encodeString(literal.input), Value: json.RawMessage(literal.value.Bytes())}
	}
	if node.leaf != nil {
		wire.Leaf = encodeLeaf(*node.leaf)
	}
	if node.scope != nil {
		wire.Scope = encodeScope(*node.scope)
	}
	return wire
}

func encodeLeaf(leaf Leaf) *leafWire {
	wire := &leafWire{
		Kind: encodeString(string(leaf.kind)), Inputs: encodeContract(leaf.inputs), Outputs: encodeContract(leaf.outputs),
		BaseTree: encodeStrings(leaf.baseTree), PublishWorkspace: encodeStrings(leaf.publishWorkspace),
	}
	if len(leaf.attachments) != 0 {
		wire.Attachments = make([]attachmentWire, len(leaf.attachments))
		for index, attachment := range leaf.attachments {
			wire.Attachments[index] = attachmentWire{Input: encodeStrings(attachment.input), Fidelity: attachment.fidelity}
		}
	}
	return wire
}

func encodeScope(scope Scope) *scopeWire {
	wire := &scopeWire{Kind: encodeString(string(scope.kind))}
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
		Inputs: encodeContract(branch.inputs), Outputs: encodeContract(branch.outputs), Selector: encodeString(branch.selector),
		Cases: make([]caseWire, len(branch.cases)),
	}
	for index, branchCase := range branch.cases {
		wire.Cases[index] = caseWire{Name: encodeString(branchCase.name), Graph: encodeGraph(branchCase.graph)}
	}
	return wire
}

func encodeMap(mapped Map) mapWire {
	return mapWire{
		Inputs: encodeContract(mapped.inputs), Outputs: encodeContract(mapped.outputs),
		Collection: encodeString(mapped.collection), Result: encodeString(mapped.result), Body: encodeGraph(mapped.body),
	}
}

func encodeLoop(loop Loop) loopWire {
	return loopWire{
		Inputs: encodeContract(loop.inputs), Outputs: encodeContract(loop.outputs), Maximum: loop.maximum,
		Termination: encodeStrings(loop.termination), Body: encodeGraph(loop.body),
	}
}

func encodeEdge(edge Edge) edgeWire {
	wire := edgeWire{
		From:     endpointWire{Kind: edge.from.kind, Child: encodeString(edge.from.child)},
		To:       endpointWire{Kind: edge.to.kind, Child: encodeString(edge.to.child)},
		Bindings: make([]bindingWire, len(edge.bindings)),
	}
	for index, binding := range edge.bindings {
		wire.Bindings[index] = bindingWire{
			From: encodeStrings(binding.from), To: encodeString(binding.to), RuntimeValidation: binding.runtimeValidation,
		}
	}
	return wire
}

func encodeString(value string) stringWire { return stringWire([]byte(value)) }

func encodeStrings(values []string) []stringWire {
	wire := make([]stringWire, len(values))
	for index, value := range values {
		wire[index] = encodeString(value)
	}
	return wire
}

func normalizeDefinition(definition *Definition) { normalizeGraph(&definition.root) }

func normalizeGraph(graph *Graph) {
	for index := range graph.nodes {
		normalizeNode(&graph.nodes[index])
	}
	sort.Slice(graph.nodes, func(i, j int) bool { return graph.nodes[i].name < graph.nodes[j].name })
	for index := range graph.edges {
		normalizeEdge(&graph.edges[index])
	}
	sort.Slice(graph.edges, func(i, j int) bool { return compareEdges(graph.edges[i], graph.edges[j]) < 0 })
	graph.edges = coalesceEdges(graph.edges)
	if graph.cleanup != nil {
		normalizeGraph(&graph.cleanup.graph)
	}
}

func normalizeNode(node *Node) {
	sort.Slice(node.literals, func(i, j int) bool {
		if node.literals[i].input != node.literals[j].input {
			return node.literals[i].input < node.literals[j].input
		}
		return bytes.Compare(node.literals[i].value.Bytes(), node.literals[j].value.Bytes()) < 0
	})
	if node.leaf != nil {
		normalizeAttachments(&node.leaf.attachments)
	}
	if node.scope == nil {
		return
	}
	switch node.scope.kind {
	case GraphScope:
		normalizeGraph(node.scope.graph)
	case BranchScope:
		normalizeBranch(node.scope.branch)
	case MapScope:
		normalizeGraph(&node.scope.map_.body)
	case LoopScope:
		normalizeGraph(&node.scope.loop.body)
	}
}

func normalizeBranch(branch *Branch) {
	for index := range branch.cases {
		normalizeGraph(&branch.cases[index].graph)
	}
	sort.Slice(branch.cases, func(i, j int) bool { return branch.cases[i].name < branch.cases[j].name })
}

func normalizeEdge(edge *Edge) {
	sort.Slice(edge.bindings, func(i, j int) bool { return compareBindings(edge.bindings[i], edge.bindings[j]) < 0 })
}

func coalesceEdges(edges []Edge) []Edge {
	if len(edges) == 0 {
		return edges
	}
	coalesced := make([]Edge, 0, len(edges))
	for _, edge := range edges {
		if len(coalesced) == 0 || compareEndpoints(coalesced[len(coalesced)-1].from, edge.from) != 0 || compareEndpoints(coalesced[len(coalesced)-1].to, edge.to) != 0 {
			coalesced = append(coalesced, edge)
			continue
		}
		coalesced[len(coalesced)-1].bindings = append(coalesced[len(coalesced)-1].bindings, edge.bindings...)
	}
	for index := range coalesced {
		normalizeEdge(&coalesced[index])
	}
	return coalesced
}

func compareEdges(left, right Edge) int {
	if result := compareEndpoints(left.from, right.from); result != 0 {
		return result
	}
	if result := compareEndpoints(left.to, right.to); result != 0 {
		return result
	}
	for index := 0; index < len(left.bindings) && index < len(right.bindings); index++ {
		if result := compareBindings(left.bindings[index], right.bindings[index]); result != 0 {
			return result
		}
	}
	return compareInt(len(left.bindings), len(right.bindings))
}

func compareEndpoints(left, right Endpoint) int {
	if result := compareInt(int(left.kind), int(right.kind)); result != 0 {
		return result
	}
	return compareString(left.child, right.child)
}

func compareBindings(left, right Binding) int {
	if result := compareString(left.to, right.to); result != 0 {
		return result
	}
	if result := compareStrings(left.from, right.from); result != 0 {
		return result
	}
	if left.runtimeValidation == right.runtimeValidation {
		return 0
	}
	if !left.runtimeValidation {
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
