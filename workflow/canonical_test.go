package workflow

import (
	"bytes"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// This catches canonical encodings that preserve authoring order for sets of
// modules, graph children, edges, bindings, or branch cases.
func TestCanonicalDefinitionIgnoresSourceOrder(t *testing.T) {
	forward := mustCompileCanonical(t, canonicalFixture(t, false))
	reversed := mustCompileCanonical(t, canonicalFixture(t, true))
	if !bytes.Equal(forward.Canonical(), reversed.Canonical()) {
		t.Fatalf("canonical definitions differ:\n%s\n%s", forward.Canonical(), reversed.Canonical())
	}
}

// This catches an encoding that omits an executable definition property, which
// would make two distinct compiled definitions indistinguishable.
func TestCanonicalDefinitionChangesWithSemantics(t *testing.T) {
	baseline := mustCompileCanonical(t, canonicalFixture(t, false)).Canonical()
	for _, tc := range []struct {
		name string
		edit func(*ProgramDraft)
	}{
		{
			name: "edge",
			edit: func(draft *ProgramDraft) {
				root := canonicalRoot(draft)
				root.Edges[0].Bindings[0].From[0] = "second"
			},
		},
		{
			name: "literal",
			edit: func(draft *ProgramDraft) {
				root := canonicalRoot(draft)
				canonicalNode(root, "alpha").Literals[0].Value = canonicalLiteral(t, `"changed"`)
			},
		},
		{
			name: "contract",
			edit: func(draft *ProgramDraft) {
				root := canonicalRoot(draft)
				changed := canonicalContract(t, canonicalField(t, "choice", value.Boolean()), canonicalField(t, "left", value.String()), canonicalField(t, "right", value.String()), canonicalField(t, "upload", canonicalFile(t, "application/json", "text/plain", false)))
				root.Inputs = changed
			},
		},
		{
			name: "leaf kind",
			edit: func(draft *ProgramDraft) {
				canonicalNode(canonicalRoot(draft), "alpha").Leaf.Kind = Agent
			},
		},
		{
			name: "branch case",
			edit: func(draft *ProgramDraft) {
				branch := canonicalNode(canonicalRoot(draft), "choose").Branch
				branch.Cases[0].Graph.Nodes[0].Leaf.Kind = Agent
			},
		},
		{
			name: "loop maximum",
			edit: func(draft *ProgramDraft) {
				canonicalNode(canonicalRoot(draft), "repeat").Loop.Maximum++
			},
		},
		{
			name: "cleanup graph",
			edit: func(draft *ProgramDraft) {
				canonicalRoot(draft).Finally.Nodes[0].Leaf.Kind = Agent
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := canonicalFixture(t, false)
			tc.edit(&draft)
			got := mustCompileCanonical(t, draft).Canonical()
			if bytes.Equal(baseline, got) {
				t.Fatal("canonical bytes did not change")
			}
		})
	}
}

// This catches aliases from source drafts and accessor results that let a
// caller change an already compiled definition or its canonical bytes.
func TestDefinitionImmutable(t *testing.T) {
	draft := canonicalFixture(t, false)
	def := mustCompileCanonical(t, draft)
	wantCanonical := def.Canonical()
	wantReads := canonicalReads(def)

	root := canonicalRoot(&draft)
	root.Nodes[0], root.Nodes[1] = root.Nodes[1], root.Nodes[0]
	root.Nodes[0].Name = "changed"
	root.Edges[0], root.Edges[1] = root.Edges[1], root.Edges[0]
	root.Edges[1].Bindings[0].From[0] = "changed"
	canonicalNode(root, "alpha").Literals[0].Input = "changed"
	canonicalNode(root, "choose").Branch.Cases[0], canonicalNode(root, "choose").Branch.Cases[1] = canonicalNode(root, "choose").Branch.Cases[1], canonicalNode(root, "choose").Branch.Cases[0]
	canonicalNode(root, "choose").Branch.Cases[0].Graph.Nodes[0].Name = "changed"
	canonicalNode(root, "repeat").Loop.Termination[0] = "changed"
	canonicalNode(root, "repeat").Loop.Body.Nodes[0].Name = "changed"
	canonicalNode(root, "repeat").Loop.Body.Edges[0].Bindings[0].From[0] = "changed"
	root.Finally.Nodes[0].Name = "changed"
	draft.Modules[0], draft.Modules[1] = draft.Modules[1], draft.Modules[0]

	canonical := def.Canonical()
	canonical[0] = 'x'
	graph := def.Root()
	nodes := graph.Nodes()
	nodes[0], nodes[1] = nodes[1], nodes[0]
	edges := graph.Edges()
	edges[0], edges[1] = edges[1], edges[0]
	ports := graph.Inputs().Ports()
	ports[0] = value.Field{}
	outputPorts := graph.Outputs().Ports()
	outputPorts[0] = value.Field{}
	enumValues := graph.Inputs().Ports()[0].Type().EnumValues()
	enumValues[0] = value.Literal{}
	media := graph.Inputs().Ports()[3].Type().Media()
	media[0] = "changed/type"
	alpha := mustNode(t, graph, "alpha")
	literals := alpha.Literals()
	literals[0] = LiteralBinding{}
	leaf, _ := alpha.Leaf()
	leafInputs := leaf.Inputs().Ports()
	leafInputs[0] = value.Field{}
	leafOutputs := leaf.Outputs().Ports()
	leafOutputs[0] = value.Field{}
	bindings := graph.Edges()[0].Bindings()
	bindings[0] = Binding{}
	path := graph.Edges()[0].Bindings()[0].From()
	path[0] = "changed"
	choose := mustNode(t, graph, "choose")
	branch, _ := choose.Scope()
	cases, _ := branch.Branch()
	caseSlice := cases.Cases()
	caseSlice[0], caseSlice[1] = caseSlice[1], caseSlice[0]
	caseNodes := caseSlice[0].Graph().Nodes()
	caseNodes[0] = Node{}
	repeat := mustNode(t, graph, "repeat")
	loopScope, _ := repeat.Scope()
	loop, _ := loopScope.Loop()
	termination := loop.Termination()
	termination[0] = "changed"
	loopNodes := loop.Body().Nodes()
	loopNodes[0] = Node{}
	loopBindings := loop.Body().Edges()[0].Bindings()
	loopBindings[0] = Binding{}
	library := mustNode(t, graph, "library")
	libraryScope, _ := library.Scope()
	libraryGraph, _ := libraryScope.Graph()
	libraryNodes := libraryGraph.Nodes()
	libraryNodes[0] = Node{}
	finally, _ := graph.Finally()
	cleanupNodes := finally.Graph().Nodes()
	cleanupNodes[0] = Node{}

	if got := def.Canonical(); !bytes.Equal(got, wantCanonical) {
		t.Fatalf("canonical bytes changed: got %s, want %s", got, wantCanonical)
	}
	if got := canonicalReads(def); got != wantReads {
		t.Fatalf("later reads changed: got %q, want %q", got, wantReads)
	}
}

func mustCompileCanonical(t *testing.T, draft ProgramDraft) Definition {
	t.Helper()
	definition, err := Compile(draft)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func canonicalFixture(t *testing.T, reverse bool) ProgramDraft {
	t.Helper()
	choice := canonicalEnum(t, `"alpha"`, `"beta"`, reverse)
	upload := canonicalFile(t, "application/json", "text/plain", reverse)
	left := canonicalField(t, "left", value.String())
	right := canonicalField(t, "right", value.String())
	choiceField := canonicalField(t, "choice", choice)
	uploadField := canonicalField(t, "upload", upload)
	inputFields := []value.Field{left, right, choiceField, uploadField}
	if reverse {
		reverseFields(inputFields)
	}
	inputs := canonicalContract(t, inputFields...)
	output := canonicalContract(t, canonicalField(t, "first", value.String()), canonicalField(t, "second", value.String()))
	text := canonicalContract(t, canonicalField(t, "text", value.String()))
	valueOutput := canonicalContract(t, canonicalField(t, "first", value.String()), canonicalField(t, "second", value.String()))
	selector := canonicalContract(t, canonicalField(t, "selected", value.Boolean()))

	branchCases := []CaseDraft{
		{Name: "false", Graph: GraphDraft{Inputs: selector, Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "false-work", Leaf: scriptLeaf()}}}},
		{Name: "true", Graph: GraphDraft{Inputs: selector, Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "true-work", Leaf: scriptLeaf()}}}},
	}
	if reverse {
		reverseCases(branchCases)
	}
	loop := validationLoop(t)
	nodes := []NodeDraft{
		{Name: "alpha", Leaf: &LeafDraft{Kind: Script, Inputs: text, Outputs: valueOutput}, Literals: []LiteralBindingDraft{{Input: "text", Value: canonicalLiteral(t, `"alpha"`)}}},
		{Name: "beta", Leaf: &LeafDraft{Kind: Agent, Inputs: text, Outputs: value.EmptyContract()}, Literals: []LiteralBindingDraft{{Input: "text", Value: canonicalLiteral(t, `"beta"`)}}},
		{Name: "choose", Branch: &BranchDraft{Inputs: selector, Outputs: value.EmptyContract(), Selector: "selected", Cases: branchCases}, Literals: []LiteralBindingDraft{{Input: "selected", Value: canonicalLiteral(t, "true")}}},
		{Name: "library", Call: &CallDraft{Module: "library"}},
		{Name: "repeat", Loop: &loop},
	}
	edges := []EdgeDraft{
		{From: EndpointDraft{Kind: Child, Child: "alpha"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"first"}, To: "first"}, {From: []string{"second"}, To: "second"}}},
		{From: EndpointDraft{Kind: Child, Child: "alpha"}, To: EndpointDraft{Kind: Child, Child: "beta"}},
	}
	if reverse {
		reverseNodes(nodes)
		reverseEdges(edges)
		reverseBindings(edges[1].Bindings)
	}
	root := module("root", GraphDraft{
		Inputs: inputs, Outputs: output, Nodes: nodes, Edges: edges,
		Finally: &GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "cleanup", Leaf: scriptLeaf()}}},
	})
	library := module("library", GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "library-work", Leaf: scriptLeaf()}}})
	modules := []ModuleDraft{root, library}
	if reverse {
		modules[0], modules[1] = modules[1], modules[0]
	}
	return ProgramDraft{Root: "root", Modules: modules}
}

func canonicalReads(def Definition) string {
	root := def.Root()
	names := make([]byte, 0)
	for _, node := range root.Nodes() {
		names = append(names, node.Name()...)
		for _, literal := range node.Literals() {
			names = append(names, literal.Input()...)
		}
		scope, scoped := node.Scope()
		if !scoped {
			continue
		}
		if branch, ok := scope.Branch(); ok {
			for _, branchCase := range branch.Cases() {
				names = append(names, branchCase.Name()...)
				for _, caseNode := range branchCase.Graph().Nodes() {
					names = append(names, caseNode.Name()...)
				}
			}
		}
		if loop, ok := scope.Loop(); ok {
			for _, termination := range loop.Termination() {
				names = append(names, termination...)
			}
			for _, bodyNode := range loop.Body().Nodes() {
				names = append(names, bodyNode.Name()...)
			}
		}
		if nested, ok := scope.Graph(); ok {
			for _, nestedNode := range nested.Nodes() {
				names = append(names, nestedNode.Name()...)
			}
		}
	}
	for _, edge := range root.Edges() {
		names = append(names, edge.From().Child()...)
		names = append(names, edge.To().Child()...)
		for _, binding := range edge.Bindings() {
			names = append(names, binding.To()...)
			names = append(names, binding.From()[0]...)
		}
	}
	if cleanup, ok := root.Finally(); ok {
		for _, cleanupNode := range cleanup.Graph().Nodes() {
			names = append(names, cleanupNode.Name()...)
		}
	}
	return string(names)
}

func canonicalRoot(draft *ProgramDraft) *GraphDraft {
	for index := range draft.Modules {
		if draft.Modules[index].Name == "root" {
			return &draft.Modules[index].Graph
		}
	}
	panic("root module is missing")
}

func canonicalNode(graph *GraphDraft, name string) *NodeDraft {
	for index := range graph.Nodes {
		if graph.Nodes[index].Name == name {
			return &graph.Nodes[index]
		}
	}
	panic("node is missing: " + name)
}

func canonicalField(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func canonicalContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func canonicalLiteral(t *testing.T, data string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return literal
}

func canonicalEnum(t *testing.T, first, second string, reverse bool) value.Type {
	t.Helper()
	values := []value.Literal{canonicalLiteral(t, first), canonicalLiteral(t, second)}
	if reverse {
		values[0], values[1] = values[1], values[0]
	}
	typ, err := value.Enum(values...)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func canonicalFile(t *testing.T, first, second string, reverse bool) value.Type {
	t.Helper()
	if reverse {
		first, second = second, first
	}
	typ, err := value.File(first, second)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func reverseFields(fields []value.Field) {
	for left, right := 0, len(fields)-1; left < right; left, right = left+1, right-1 {
		fields[left], fields[right] = fields[right], fields[left]
	}
}

func reverseNodes(nodes []NodeDraft) {
	for left, right := 0, len(nodes)-1; left < right; left, right = left+1, right-1 {
		nodes[left], nodes[right] = nodes[right], nodes[left]
	}
}

func reverseEdges(edges []EdgeDraft) {
	for left, right := 0, len(edges)-1; left < right; left, right = left+1, right-1 {
		edges[left], edges[right] = edges[right], edges[left]
	}
}

func reverseBindings(bindings []BindingDraft) {
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}
}

func reverseCases(cases []CaseDraft) {
	for left, right := 0, len(cases)-1; left < right; left, right = left+1, right-1 {
		cases[left], cases[right] = cases[right], cases[left]
	}
}
