package workflow

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/valbaudo/dawn/value"
)

const definitionFormat = "dawn.workflow/2"

// stringWire holds the original Go string bytes. Encoding JSON strings from
// Go strings would replace invalid UTF-8 and collapse distinct definitions.
type stringWire []byte

type cleanupBindingWire struct {
	Kind              CleanupSourceKind `json:"kind"`
	Child             stringWire        `json:"child"`
	Path              []stringWire      `json:"path"`
	To                stringWire        `json:"to"`
	RuntimeValidation bool              `json:"runtimeValidation"`
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
	var encoded bytes.Buffer
	encoded.WriteString(`{"format":"` + definitionFormat + `","root":`)
	actions := []canonicalAction{
		{kind: canonicalWriteText, text: `}`},
		{kind: canonicalWriteGraph, graph: &def.root},
	}
	for len(actions) != 0 {
		action := actions[len(actions)-1]
		actions = actions[:len(actions)-1]
		if err := action.write(&encoded, &actions); err != nil {
			return nil, err
		}
	}
	return encoded.Bytes(), nil
}

type canonicalActionKind uint8

const (
	canonicalWriteText canonicalActionKind = iota
	canonicalWriteGraph
	canonicalWriteGraphNodes
	canonicalWriteNode
	canonicalWriteScope
	canonicalWriteBranch
	canonicalWriteBranchCases
	canonicalWriteCase
	canonicalWriteMap
	canonicalWriteLoop
	canonicalWriteFinally
)

type canonicalAction struct {
	kind    canonicalActionKind
	text    string
	index   int
	graph   *Graph
	node    *Node
	scope   *Scope
	branch  *Branch
	case_   *Case
	mapped  *Map
	loop    *Loop
	cleanup *Finally
}

func (action canonicalAction) write(encoded *bytes.Buffer, actions *[]canonicalAction) error {
	switch action.kind {
	case canonicalWriteText:
		encoded.WriteString(action.text)
	case canonicalWriteGraph:
		encoded.WriteString(`{"inputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.graph.inputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"outputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.graph.outputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"nodes":[`)
		*actions = append(*actions, canonicalAction{kind: canonicalWriteGraphNodes, graph: action.graph})
	case canonicalWriteGraphNodes:
		if action.index < len(action.graph.nodes) {
			if action.index != 0 {
				encoded.WriteByte(',')
			}
			*actions = append(*actions,
				canonicalAction{kind: canonicalWriteGraphNodes, graph: action.graph, index: action.index + 1},
				canonicalAction{kind: canonicalWriteNode, node: &action.graph.nodes[action.index]},
			)
			return nil
		}
		encoded.WriteString(`],"edges":`)
		edges := make([]edgeWire, len(action.graph.edges))
		for index, edge := range action.graph.edges {
			edges[index] = encodeEdge(edge)
		}
		if err := writeCanonicalJSON(encoded, edges); err != nil {
			return err
		}
		encoded.WriteString(`,"cleanup":`)
		if action.graph.cleanup == nil {
			encoded.WriteString(`null}`)
			return nil
		}
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteText, text: `}`},
			canonicalAction{kind: canonicalWriteFinally, cleanup: action.graph.cleanup},
		)
	case canonicalWriteNode:
		encoded.WriteString(`{"name":`)
		if err := writeCanonicalJSON(encoded, encodeString(action.node.name)); err != nil {
			return err
		}
		encoded.WriteString(`,"leaf":`)
		if action.node.leaf == nil {
			encoded.WriteString(`null`)
		} else if err := writeCanonicalJSON(encoded, encodeLeaf(*action.node.leaf)); err != nil {
			return err
		}
		encoded.WriteString(`,"scope":`)
		literals := make([]literalWire, len(action.node.literals))
		for index, literal := range action.node.literals {
			literals[index] = literalWire{Input: encodeString(literal.input), Value: json.RawMessage(literal.value.Bytes())}
		}
		literalJSON, err := json.Marshal(literals)
		if err != nil {
			return err
		}
		tail := `,"literals":` + string(literalJSON) + `}`
		if action.node.scope == nil {
			encoded.WriteString(`null` + tail)
			return nil
		}
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteText, text: tail},
			canonicalAction{kind: canonicalWriteScope, scope: action.node.scope},
		)
	case canonicalWriteScope:
		encoded.WriteString(`{"kind":`)
		if err := writeCanonicalJSON(encoded, encodeString(string(action.scope.kind))); err != nil {
			return err
		}
		switch action.scope.kind {
		case GraphScope:
			encoded.WriteString(`,"graph":`)
			*actions = append(*actions,
				canonicalAction{kind: canonicalWriteText, text: `,"branch":null,"map":null,"loop":null}`},
				canonicalAction{kind: canonicalWriteGraph, graph: action.scope.graph},
			)
		case BranchScope:
			encoded.WriteString(`,"graph":null,"branch":`)
			*actions = append(*actions,
				canonicalAction{kind: canonicalWriteText, text: `,"map":null,"loop":null}`},
				canonicalAction{kind: canonicalWriteBranch, branch: action.scope.branch},
			)
		case MapScope:
			encoded.WriteString(`,"graph":null,"branch":null,"map":`)
			*actions = append(*actions,
				canonicalAction{kind: canonicalWriteText, text: `,"loop":null}`},
				canonicalAction{kind: canonicalWriteMap, mapped: action.scope.map_},
			)
		case LoopScope:
			encoded.WriteString(`,"graph":null,"branch":null,"map":null,"loop":`)
			*actions = append(*actions,
				canonicalAction{kind: canonicalWriteText, text: `}`},
				canonicalAction{kind: canonicalWriteLoop, loop: action.scope.loop},
			)
		default:
			encoded.WriteString(`,"graph":null,"branch":null,"map":null,"loop":null}`)
		}
	case canonicalWriteBranch:
		encoded.WriteString(`{"inputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.branch.inputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"outputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.branch.outputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"selector":`)
		if err := writeCanonicalJSON(encoded, encodeString(action.branch.selector)); err != nil {
			return err
		}
		encoded.WriteString(`,"cases":[`)
		*actions = append(*actions, canonicalAction{kind: canonicalWriteBranchCases, branch: action.branch})
	case canonicalWriteBranchCases:
		if action.index == len(action.branch.cases) {
			encoded.WriteString(`]}`)
			return nil
		}
		if action.index != 0 {
			encoded.WriteByte(',')
		}
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteBranchCases, branch: action.branch, index: action.index + 1},
			canonicalAction{kind: canonicalWriteCase, case_: &action.branch.cases[action.index]},
		)
	case canonicalWriteCase:
		encoded.WriteString(`{"name":`)
		if err := writeCanonicalJSON(encoded, encodeString(action.case_.name)); err != nil {
			return err
		}
		encoded.WriteString(`,"graph":`)
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteText, text: `}`},
			canonicalAction{kind: canonicalWriteGraph, graph: &action.case_.graph},
		)
	case canonicalWriteMap:
		encoded.WriteString(`{"inputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.mapped.inputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"outputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.mapped.outputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"collection":`)
		if err := writeCanonicalJSON(encoded, encodeString(action.mapped.collection)); err != nil {
			return err
		}
		encoded.WriteString(`,"result":`)
		if err := writeCanonicalJSON(encoded, encodeString(action.mapped.result)); err != nil {
			return err
		}
		encoded.WriteString(`,"body":`)
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteText, text: `}`},
			canonicalAction{kind: canonicalWriteGraph, graph: &action.mapped.body},
		)
	case canonicalWriteLoop:
		encoded.WriteString(`{"inputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.loop.inputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"outputs":`)
		if err := writeCanonicalJSON(encoded, encodeContract(action.loop.outputs)); err != nil {
			return err
		}
		encoded.WriteString(`,"maximum":`)
		if err := writeCanonicalJSON(encoded, action.loop.maximum); err != nil {
			return err
		}
		encoded.WriteString(`,"termination":`)
		if err := writeCanonicalJSON(encoded, encodeStrings(action.loop.termination)); err != nil {
			return err
		}
		encoded.WriteString(`,"body":`)
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteText, text: `}`},
			canonicalAction{kind: canonicalWriteGraph, graph: &action.loop.body},
		)
	case canonicalWriteFinally:
		bindings := make([]cleanupBindingWire, len(action.cleanup.bindings))
		for index, binding := range action.cleanup.bindings {
			bindings[index] = cleanupBindingWire{
				Kind: binding.from.kind, Child: encodeString(binding.from.child), Path: encodeStrings(binding.from.path),
				To: encodeString(binding.to), RuntimeValidation: binding.runtimeValidation,
			}
		}
		bindingJSON, err := json.Marshal(bindings)
		if err != nil {
			return err
		}
		encoded.WriteString(`{"graph":`)
		*actions = append(*actions,
			canonicalAction{kind: canonicalWriteText, text: `,"bindings":` + string(bindingJSON) + `}`},
			canonicalAction{kind: canonicalWriteGraph, graph: &action.cleanup.graph},
		)
	}
	return nil
}

func writeCanonicalJSON(encoded *bytes.Buffer, source any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	encoded.Write(data)
	return nil
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
	type graphFrame struct {
		graph    *Graph
		expanded bool
	}
	stack := []graphFrame{{graph: graph}}
	for len(stack) != 0 {
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !frame.expanded {
			stack = append(stack, graphFrame{graph: frame.graph, expanded: true})
			children := nestedGraphs(frame.graph)
			for index := len(children) - 1; index >= 0; index-- {
				stack = append(stack, graphFrame{graph: children[index]})
			}
			continue
		}
		normalizeGraphLocal(frame.graph)
	}
}

func nestedGraphs(graph *Graph) []*Graph {
	children := make([]*Graph, 0, len(graph.nodes)+1)
	for index := range graph.nodes {
		node := &graph.nodes[index]
		if node.scope == nil {
			continue
		}
		switch node.scope.kind {
		case GraphScope:
			children = append(children, node.scope.graph)
		case BranchScope:
			for caseIndex := range node.scope.branch.cases {
				children = append(children, &node.scope.branch.cases[caseIndex].graph)
			}
		case MapScope:
			children = append(children, &node.scope.map_.body)
		case LoopScope:
			children = append(children, &node.scope.loop.body)
		}
	}
	if graph.cleanup != nil {
		children = append(children, &graph.cleanup.graph)
	}
	return children
}

func normalizeGraphLocal(graph *Graph) {
	for index := range graph.nodes {
		normalizeNodeLocal(&graph.nodes[index])
	}
	sort.Slice(graph.nodes, func(i, j int) bool { return graph.nodes[i].name < graph.nodes[j].name })
	for index := range graph.edges {
		normalizeEdge(&graph.edges[index])
	}
	sort.Slice(graph.edges, func(i, j int) bool { return compareEdges(graph.edges[i], graph.edges[j]) < 0 })
	graph.edges = coalesceEdges(graph.edges)
	if graph.cleanup != nil {
		sort.Slice(graph.cleanup.bindings, func(i, j int) bool {
			return compareCleanupBindings(graph.cleanup.bindings[i], graph.cleanup.bindings[j]) < 0
		})
	}
}

func compareCleanupBindings(left, right CleanupBinding) int {
	if result := compareInt(int(left.from.kind), int(right.from.kind)); result != 0 {
		return result
	}
	if result := compareString(left.from.child, right.from.child); result != 0 {
		return result
	}
	if result := compareStrings(left.from.path, right.from.path); result != 0 {
		return result
	}
	return compareString(left.to, right.to)
}

func normalizeNodeLocal(node *Node) {
	sort.Slice(node.literals, func(i, j int) bool {
		if node.literals[i].input != node.literals[j].input {
			return node.literals[i].input < node.literals[j].input
		}
		return bytes.Compare(node.literals[i].value.Bytes(), node.literals[j].value.Bytes()) < 0
	})
	if node.leaf != nil {
		normalizeAttachments(&node.leaf.attachments)
	}
	if node.scope != nil && node.scope.kind == BranchScope {
		sort.Slice(node.scope.branch.cases, func(i, j int) bool {
			return node.scope.branch.cases[i].name < node.scope.branch.cases[j].name
		})
	}
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
