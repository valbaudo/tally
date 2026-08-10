package workflow

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// This catches JSON string replacement collapsing two distinct semantic Go
// strings that contain invalid UTF-8 into the same canonical definition.
func TestCanonicalDefinitionDistinguishesInvalidUTF8Strings(t *testing.T) {
	first := string([]byte{0xff})
	second := string([]byte{0xfe})
	for _, tc := range []struct {
		name  string
		draft func(string) ProgramDraft
	}{
		{name: "node name", draft: canonicalInvalidNodeName},
		{name: "contract port", draft: canonicalInvalidPort},
		{name: "object field", draft: canonicalInvalidObjectField},
		{name: "binding path", draft: canonicalInvalidBindingPath},
		{name: "branch selector", draft: canonicalInvalidBranchSelector},
		{name: "loop termination path", draft: canonicalInvalidLoopTermination},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := mustCompileCanonical(t, tc.draft(first)).Canonical()
			right := mustCompileCanonical(t, tc.draft(second)).Canonical()
			if bytes.Equal(left, right) {
				t.Fatalf("canonical bytes collapsed distinct semantic strings: %x", left)
			}
		})
	}
}

// This catches ordering that is normalized only while encoding, leaving
// semantically identical compiled definitions observably source ordered.
func TestCompiledDefinitionNormalizesPublicInspectionOrder(t *testing.T) {
	forward := mustCompileCanonical(t, canonicalNestedFixture(t, false))
	reversed := mustCompileCanonical(t, canonicalNestedFixture(t, true))
	got := canonicalInspection(forward)
	if !reflect.DeepEqual(got, canonicalInspection(reversed)) {
		t.Fatalf("public inspection order differs:\n%v\n%v", got, canonicalInspection(reversed))
	}
	want := []string{
		"root:nodes=alpha,beta,choose,each,literal,nested,repeat",
		"root:edges=alpha->:one,alpha->:two,alpha->beta",
		"root:literals=a,b",
		"nested:nodes=nested-a,nested-b,nested-c",
		"nested:edges=nested-a->nested-b,nested-b->nested-c",
		"branch:false:nodes=false-a,false-b,false-c",
		"branch:false:edges=false-a->false-b,false-b->false-c",
		"branch:true:nodes=true-a,true-b,true-c",
		"branch:true:edges=true-a->true-b,true-b->true-c",
		"branch:cases=false,true",
		"map:nodes=map-a,map-b,map-c",
		"map:edges=map-a->map-b,map-b->map-c",
		"loop:nodes=loop-a,loop-b,work",
		"loop:edges=loop-a->loop-b,loop-b->work,work->:done",
		"finally:nodes=cleanup-a,cleanup-b,cleanup-c",
		"finally:edges=cleanup-a->cleanup-b,cleanup-b->cleanup-c",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public inspection order =\n%v\nwant\n%v", got, want)
	}
}

// This catches preserving lexical edge grouping after validation, where one
// dependency relation can be expressed as split data edges or redundant
// completion edges and therefore must normalize to one public Edge.
func TestCompiledDefinitionCoalescesSemanticEdges(t *testing.T) {
	combined := mustCompileCanonical(t, canonicalEdgeGroupingFixture(t, "combined"))
	for _, name := range []string{"split", "split reversed", "data plus completion"} {
		t.Run(name, func(t *testing.T) {
			got := mustCompileCanonical(t, canonicalEdgeGroupingFixture(t, name))
			if !bytes.Equal(combined.Canonical(), got.Canonical()) {
				t.Fatalf("canonical bytes differ for %s", name)
			}
			assertCanonicalEdgeGroup(t, got.Root())
		})
	}
	assertCanonicalEdgeGroup(t, combined.Root())

	t.Run("duplicate completion", func(t *testing.T) {
		def := mustCompileCanonical(t, canonicalCompletionGroupingFixture())
		edges := def.Root().Edges()
		if len(edges) != 1 || len(edges[0].Bindings()) != 0 {
			t.Fatalf("completion edges = %#v, want one empty edge", edges)
		}
	})

	t.Run("nested graph", func(t *testing.T) {
		combined := mustCompileCanonical(t, canonicalNestedEdgeGroupingFixture(t, "combined"))
		split := mustCompileCanonical(t, canonicalNestedEdgeGroupingFixture(t, "split reversed"))
		if !bytes.Equal(combined.Canonical(), split.Canonical()) {
			t.Fatal("nested canonical bytes differ")
		}
		scope, _ := mustNode(t, split.Root(), "nested").Scope()
		graph, _ := scope.Graph()
		assertCanonicalEdgeGroup(t, graph)
	})

	t.Run("distinct pairs remain distinct", func(t *testing.T) {
		def := mustCompileCanonical(t, canonicalDistinctEdgePairsFixture())
		if got := len(def.Root().Edges()); got != 2 {
			t.Fatalf("distinct endpoint edges = %d, want 2", got)
		}
	})
}

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
				canonicalRoot(draft).Finally.Graph.Nodes[0].Leaf.Kind = Agent
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

func TestCanonicalDeliveryIntentIgnoresAttachmentSourceOrder(t *testing.T) {
	forward := mustCompileCanonical(t, canonicalDeliveryFixture(t, false))
	reversed := mustCompileCanonical(t, canonicalDeliveryFixture(t, true))
	if !bytes.Equal(forward.Canonical(), reversed.Canonical()) {
		t.Fatalf("canonical delivery definitions differ:\n%s\n%s", forward.Canonical(), reversed.Canonical())
	}

	forwardLeaf, _ := mustNode(t, forward.Root(), "attach").Leaf()
	reversedLeaf, _ := mustNode(t, reversed.Root(), "attach").Leaf()
	if !reflect.DeepEqual(forwardLeaf.Attachments(), reversedLeaf.Attachments()) {
		t.Fatalf("attachment inspection differs: %#v != %#v", forwardLeaf.Attachments(), reversedLeaf.Attachments())
	}
}

func TestCanonicalDeliveryIntentChangesWithSemantics(t *testing.T) {
	baseline := mustCompileCanonical(t, canonicalDeliveryFixture(t, false)).Canonical()
	for _, tc := range []struct {
		name string
		edit func(*ProgramDraft)
	}{
		{"base tree", func(draft *ProgramDraft) {
			canonicalNode(canonicalRoot(draft), "workspace").Leaf.BaseTree = []string{"otherSource"}
		}},
		{"workspace publication", func(draft *ProgramDraft) {
			canonicalNode(canonicalRoot(draft), "workspace").Leaf.PublishWorkspace = []string{"otherWorkspace"}
		}},
		{"attachment fidelity", func(draft *ProgramDraft) {
			canonicalNode(canonicalRoot(draft), "attach").Leaf.Attachments[0].Fidelity = TextFidelity
		}},
		{"attachment subtree", func(draft *ProgramDraft) {
			canonicalNode(canonicalRoot(draft), "attach").Leaf.Attachments[0].Input = []string{"otherDocument"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := canonicalDeliveryFixture(t, false)
			tc.edit(&draft)
			if got := mustCompileCanonical(t, draft).Canonical(); bytes.Equal(got, baseline) {
				t.Fatal("canonical bytes did not change")
			}
		})
	}
}

func TestCanonicalDeliveryIntentClonesDrafts(t *testing.T) {
	draft := canonicalDeliveryFixture(t, false)
	definition := mustCompileCanonical(t, draft)
	wantCanonical := definition.Canonical()
	workspaceDraft := canonicalNode(canonicalRoot(&draft), "workspace").Leaf
	attachmentDraft := canonicalNode(canonicalRoot(&draft), "attach").Leaf
	workspaceDraft.BaseTree[0] = "changed"
	workspaceDraft.PublishWorkspace[0] = "changed"
	attachmentDraft.Attachments[0].Input[0] = "changed"
	attachmentDraft.Attachments[0].Fidelity = TextFidelity
	attachmentDraft.Attachments[0] = AttachmentDraft{}

	if got := definition.Canonical(); !bytes.Equal(got, wantCanonical) {
		t.Fatalf("canonical bytes changed after draft mutation: got %s, want %s", got, wantCanonical)
	}
	workspaceLeaf, _ := mustNode(t, definition.Root(), "workspace").Leaf()
	if got, want := workspaceLeaf.BaseTree(), []string{"source"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compiled base = %v, want %v", got, want)
	}
	attachmentLeaf, _ := mustNode(t, definition.Root(), "attach").Leaf()
	if got, want := attachmentLeaf.Attachments()[0].Input(), []string{"document"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compiled attachment = %v, want %v", got, want)
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
	root.Finally.Graph.Nodes[0].Name = "changed"
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
		Finally: &FinallyDraft{Graph: GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "cleanup", Leaf: scriptLeaf()}}}},
	})
	library := module("library", GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "library-work", Leaf: scriptLeaf()}}})
	modules := []ModuleDraft{root, library}
	if reverse {
		modules[0], modules[1] = modules[1], modules[0]
	}
	return ProgramDraft{Root: "root", Modules: modules}
}

func canonicalDeliveryFixture(t *testing.T, reverse bool) ProgramDraft {
	t.Helper()
	treeInputs := canonicalContract(t,
		canonicalField(t, "source", value.Tree()),
		canonicalField(t, "otherSource", value.Tree()),
	)
	treeOutputs := canonicalContract(t,
		canonicalField(t, "continued", value.Tree()),
		canonicalField(t, "otherWorkspace", value.Tree()),
	)
	file := canonicalFile(t, "application/pdf", "image/png", false)
	payload, err := value.Object(canonicalField(t, "page", file))
	if err != nil {
		t.Fatal(err)
	}
	attachmentInputs := canonicalContract(t,
		canonicalField(t, "document", file),
		canonicalField(t, "otherDocument", file),
		canonicalField(t, "payload", payload),
	)
	attachments := []AttachmentDraft{
		{Input: []string{"document"}, Fidelity: VisualFidelity},
		{Input: []string{"payload"}, Fidelity: TextFidelity},
	}
	if reverse {
		attachments[0], attachments[1] = attachments[1], attachments[0]
	}
	graphInputs := canonicalContract(t,
		canonicalField(t, "source", value.Tree()),
		canonicalField(t, "otherSource", value.Tree()),
		canonicalField(t, "document", file),
		canonicalField(t, "otherDocument", file),
		canonicalField(t, "payload", payload),
	)
	root := GraphDraft{
		Inputs: graphInputs, Outputs: treeOutputs,
		Nodes: []NodeDraft{
			{Name: "workspace", Leaf: &LeafDraft{
				Kind: Agent, Inputs: treeInputs, Outputs: treeOutputs,
				BaseTree: []string{"source"}, PublishWorkspace: []string{"continued"},
			}},
			{Name: "attach", Leaf: &LeafDraft{
				Kind: LLM, Inputs: attachmentInputs, Outputs: value.EmptyContract(), Attachments: attachments,
			}},
		},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "workspace"}, Bindings: []BindingDraft{{From: []string{"source"}, To: "source"}, {From: []string{"otherSource"}, To: "otherSource"}}},
			{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "attach"}, Bindings: []BindingDraft{{From: []string{"document"}, To: "document"}, {From: []string{"otherDocument"}, To: "otherDocument"}, {From: []string{"payload"}, To: "payload"}}},
			{From: EndpointDraft{Kind: Child, Child: "workspace"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"continued"}, To: "continued"}, {From: []string{"otherWorkspace"}, To: "otherWorkspace"}}},
		},
	}
	if reverse {
		reverseNodes(root.Nodes)
		reverseEdges(root.Edges)
		for index := range root.Edges {
			reverseBindings(root.Edges[index].Bindings)
		}
	}
	return bindingProgram(root)
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

func canonicalInvalidNodeName(name string) ProgramDraft {
	return bindingProgram(GraphDraft{Nodes: []NodeDraft{{Name: name, Leaf: scriptLeaf()}}})
}

func canonicalInvalidPort(name string) ProgramDraft {
	return bindingProgram(GraphDraft{Inputs: mustCanonicalContract(name, value.String())})
}

func canonicalInvalidObjectField(name string) ProgramDraft {
	field, err := value.Required(name, value.String())
	if err != nil {
		panic(err)
	}
	object, err := value.Object(field)
	if err != nil {
		panic(err)
	}
	return bindingProgram(GraphDraft{Inputs: mustCanonicalContract("object", object)})
}

func canonicalInvalidBindingPath(name string) ProgramDraft {
	contract := mustCanonicalContract(name, value.String())
	return bindingProgram(GraphDraft{
		Inputs: contract,
		Nodes:  []NodeDraft{bindingLeaf("child", contract, value.EmptyContract())},
		Edges: []EdgeDraft{{
			From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "child"},
			Bindings: []BindingDraft{{From: []string{name}, To: name}},
		}},
	})
}

func canonicalInvalidBranchSelector(name string) ProgramDraft {
	inputs := mustCanonicalContract(name, value.Boolean())
	cases := []CaseDraft{
		{Name: "false", Graph: GraphDraft{Inputs: inputs, Outputs: value.EmptyContract()}},
		{Name: "true", Graph: GraphDraft{Inputs: inputs, Outputs: value.EmptyContract()}},
	}
	return bindingProgram(GraphDraft{Nodes: []NodeDraft{{
		Name: "branch", Branch: &BranchDraft{Inputs: inputs, Outputs: value.EmptyContract(), Selector: name, Cases: cases},
		Literals: []LiteralBindingDraft{{Input: name, Value: mustCanonicalLiteral("true")}},
	}}})
}

func canonicalInvalidLoopTermination(name string) ProgramDraft {
	outputs := mustCanonicalContract(name, value.Boolean())
	previous, err := value.Optional("previous", outputs.ObjectType())
	if err != nil {
		panic(err)
	}
	initial, err := value.Required("initial", value.EmptyContract().ObjectType())
	if err != nil {
		panic(err)
	}
	iteration, err := value.Required("iteration", value.Integer())
	if err != nil {
		panic(err)
	}
	bodyInputs, err := value.NewContract(initial, previous, iteration)
	if err != nil {
		panic(err)
	}
	loop := LoopDraft{
		Inputs: value.EmptyContract(), Outputs: outputs, Maximum: 1, Termination: []string{name},
		Body: GraphDraft{
			Inputs: bodyInputs, Outputs: outputs,
			Nodes: []NodeDraft{bindingLeaf("work", value.EmptyContract(), outputs)},
			Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "work"}, To: EndpointDraft{Kind: Boundary},
				Bindings: []BindingDraft{{From: []string{name}, To: name}},
			}},
		},
	}
	return bindingProgram(GraphDraft{Nodes: []NodeDraft{{Name: "loop", Loop: &loop}}})
}

func mustCanonicalContract(name string, typ value.Type, rest ...any) value.Contract {
	fields := []value.Field{mustCanonicalField(name, typ)}
	for index := 0; index < len(rest); index += 2 {
		fieldName, ok := rest[index].(string)
		if !ok {
			panic("contract field name is not a string")
		}
		fieldType, ok := rest[index+1].(value.Type)
		if !ok {
			panic("contract field type is not a value.Type")
		}
		fields = append(fields, mustCanonicalField(fieldName, fieldType))
	}
	contract, err := value.NewContract(fields...)
	if err != nil {
		panic(err)
	}
	return contract
}

func mustCanonicalField(name string, typ value.Type) value.Field {
	field, err := value.Required(name, typ)
	if err != nil {
		panic(err)
	}
	return field
}

func mustCanonicalLiteral(data string) value.Literal {
	literal, err := value.ParseLiteral([]byte(data))
	if err != nil {
		panic(err)
	}
	return literal
}

func canonicalNestedFixture(t *testing.T, reverse bool) ProgramDraft {
	t.Helper()
	strings := canonicalContract(t, canonicalField(t, "a", value.String()), canonicalField(t, "b", value.String()))
	outputs := canonicalContract(t, canonicalField(t, "one", value.String()), canonicalField(t, "two", value.String()))
	emptyLeaf := func(name string) NodeDraft { return NodeDraft{Name: name, Leaf: scriptLeaf()} }
	chain := func(prefix string) GraphDraft {
		return GraphDraft{
			Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
			Nodes: []NodeDraft{emptyLeaf(prefix + "-a"), emptyLeaf(prefix + "-b"), emptyLeaf(prefix + "-c")},
			Edges: []EdgeDraft{
				{From: EndpointDraft{Kind: Child, Child: prefix + "-a"}, To: EndpointDraft{Kind: Child, Child: prefix + "-b"}},
				{From: EndpointDraft{Kind: Child, Child: prefix + "-b"}, To: EndpointDraft{Kind: Child, Child: prefix + "-c"}},
			},
		}
	}
	selector := canonicalContract(t, canonicalField(t, "selected", value.Boolean()))
	branch := BranchDraft{
		Inputs: selector, Outputs: value.EmptyContract(), Selector: "selected",
		Cases: []CaseDraft{
			{Name: "false", Graph: canonicalBranchGraph(selector, chain("false"))},
			{Name: "true", Graph: canonicalBranchGraph(selector, chain("true"))},
		},
	}
	item, err := value.Required("item", value.String())
	if err != nil {
		t.Fatal(err)
	}
	index, err := value.Required("index", value.Integer())
	if err != nil {
		t.Fatal(err)
	}
	mapInputs := canonicalContract(t, canonicalField(t, "items", mustCanonicalList(t, value.String())))
	mapResult := canonicalContract(t, canonicalField(t, "result", mustCanonicalList(t, value.EmptyContract().ObjectType())))
	mapped := MapDraft{
		Inputs: mapInputs, Outputs: mapResult, Collection: "items", Result: "result",
		Body: canonicalMapBody(chain("map"), item, index),
	}
	loop := canonicalOrderedLoop(t)
	root := GraphDraft{
		Outputs: outputs,
		Nodes: []NodeDraft{
			{Name: "alpha", Leaf: &LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: outputs}},
			emptyLeaf("beta"),
			{Name: "choose", Branch: &branch, Literals: []LiteralBindingDraft{{Input: "selected", Value: canonicalLiteral(t, "true")}}},
			{Name: "each", Map: &mapped, Literals: []LiteralBindingDraft{{Input: "items", Value: canonicalLiteral(t, "[]")}}},
			{Name: "literal", Leaf: &LeafDraft{Kind: Script, Inputs: strings, Outputs: value.EmptyContract()}, Literals: []LiteralBindingDraft{{Input: "b", Value: canonicalLiteral(t, `"b"`)}, {Input: "a", Value: canonicalLiteral(t, `"a"`)}}},
			{Name: "nested", Graph: canonicalNestedGraph(chain("nested"))},
			{Name: "repeat", Loop: &loop},
		},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Child, Child: "alpha"}, To: EndpointDraft{Kind: Child, Child: "beta"}},
			{From: EndpointDraft{Kind: Child, Child: "alpha"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"two"}, To: "two"}, {From: []string{"one"}, To: "one"}}},
		},
		Finally: &FinallyDraft{Graph: GraphDraft{
			Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
			Nodes: chain("cleanup").Nodes, Edges: chain("cleanup").Edges,
		}},
	}
	if reverse {
		reverseDraftCollections(&root)
	}
	return bindingProgram(root)
}

func canonicalBranchGraph(inputs value.Contract, graph GraphDraft) GraphDraft {
	graph.Inputs = inputs
	return graph
}

func canonicalMapBody(graph GraphDraft, item, index value.Field) GraphDraft {
	inputs, err := value.NewContract(item, index)
	if err != nil {
		panic(err)
	}
	graph.Inputs = inputs
	return graph
}

func canonicalNestedGraph(graph GraphDraft) *GraphDraft { return &graph }

func mustCanonicalList(t *testing.T, element value.Type) value.Type {
	t.Helper()
	typ, err := value.List(element)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func canonicalOrderedLoop(t *testing.T) LoopDraft {
	t.Helper()
	outputs := canonicalContract(t, canonicalField(t, "done", value.Boolean()))
	initial := canonicalField(t, "initial", value.EmptyContract().ObjectType())
	previous, err := value.Optional("previous", outputs.ObjectType())
	if err != nil {
		t.Fatal(err)
	}
	iteration := canonicalField(t, "iteration", value.Integer())
	bodyInputs := canonicalContract(t, initial, previous, iteration)
	return LoopDraft{
		Inputs: value.EmptyContract(), Outputs: outputs, Maximum: 2, Termination: []string{"done"},
		Body: GraphDraft{
			Inputs: bodyInputs, Outputs: outputs,
			Nodes: []NodeDraft{
				{Name: "loop-a", Leaf: scriptLeaf()},
				{Name: "loop-b", Leaf: scriptLeaf()},
				{Name: "work", Leaf: &LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: outputs}},
			},
			Edges: []EdgeDraft{
				{From: EndpointDraft{Kind: Child, Child: "loop-a"}, To: EndpointDraft{Kind: Child, Child: "loop-b"}},
				{From: EndpointDraft{Kind: Child, Child: "loop-b"}, To: EndpointDraft{Kind: Child, Child: "work"}},
				{From: EndpointDraft{Kind: Child, Child: "work"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"done"}, To: "done"}}},
			},
		},
	}
}

func reverseDraftCollections(graph *GraphDraft) {
	reverseNodes(graph.Nodes)
	reverseEdges(graph.Edges)
	for index := range graph.Edges {
		reverseBindings(graph.Edges[index].Bindings)
	}
	for index := range graph.Nodes {
		node := &graph.Nodes[index]
		reverseLiteralBindings(node.Literals)
		switch {
		case node.Graph != nil:
			reverseDraftCollections(node.Graph)
		case node.Branch != nil:
			reverseCases(node.Branch.Cases)
			for caseIndex := range node.Branch.Cases {
				reverseDraftCollections(&node.Branch.Cases[caseIndex].Graph)
			}
		case node.Map != nil:
			reverseDraftCollections(&node.Map.Body)
		case node.Loop != nil:
			reverseDraftCollections(&node.Loop.Body)
		}
	}
	if graph.Finally != nil {
		reverseDraftCollections(&graph.Finally.Graph)
		reverseCleanupBindings(graph.Finally.Bindings)
	}
}

func reverseLiteralBindings(bindings []LiteralBindingDraft) {
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}
}

func canonicalInspection(def Definition) []string {
	root := def.Root()
	inspection := []string{
		"root:nodes=" + canonicalNodeNames(root),
		"root:edges=" + canonicalEdgeNames(root),
		"root:literals=" + canonicalLiteralNames(canonicalNodeOf(root, "literal")),
	}
	nestedScope, _ := canonicalNodeOf(root, "nested").Scope()
	nested, _ := nestedScope.Graph()
	inspection = append(inspection, "nested:nodes="+canonicalNodeNames(nested), "nested:edges="+canonicalEdgeNames(nested))
	branchScope, _ := canonicalNodeOf(root, "choose").Scope()
	branch, _ := branchScope.Branch()
	caseNames := make([]string, len(branch.Cases()))
	for index, branchCase := range branch.Cases() {
		caseNames[index] = branchCase.Name()
		inspection = append(inspection,
			"branch:"+branchCase.Name()+":nodes="+canonicalNodeNames(branchCase.Graph()),
			"branch:"+branchCase.Name()+":edges="+canonicalEdgeNames(branchCase.Graph()),
		)
	}
	inspection = append(inspection, "branch:cases="+strings.Join(caseNames, ","))
	mappedScope, _ := canonicalNodeOf(root, "each").Scope()
	mapped, _ := mappedScope.Map()
	inspection = append(inspection, "map:nodes="+canonicalNodeNames(mapped.Body()), "map:edges="+canonicalEdgeNames(mapped.Body()))
	loopScope, _ := canonicalNodeOf(root, "repeat").Scope()
	loop, _ := loopScope.Loop()
	inspection = append(inspection, "loop:nodes="+canonicalNodeNames(loop.Body()), "loop:edges="+canonicalEdgeNames(loop.Body()))
	cleanup, _ := root.Finally()
	inspection = append(inspection, "finally:nodes="+canonicalNodeNames(cleanup.Graph()), "finally:edges="+canonicalEdgeNames(cleanup.Graph()))
	return inspection
}

func canonicalNodeNames(graph Graph) string {
	names := make([]string, len(graph.Nodes()))
	for index, node := range graph.Nodes() {
		names[index] = node.Name()
	}
	return strings.Join(names, ",")
}

func canonicalEdgeNames(graph Graph) string {
	names := make([]string, 0)
	for _, edge := range graph.Edges() {
		for _, binding := range edge.Bindings() {
			names = append(names, edge.From().Child()+"->"+edge.To().Child()+":"+binding.To())
		}
		if len(edge.Bindings()) == 0 {
			names = append(names, edge.From().Child()+"->"+edge.To().Child())
		}
	}
	return strings.Join(names, ",")
}

func canonicalLiteralNames(node Node) string {
	names := make([]string, len(node.Literals()))
	for index, literal := range node.Literals() {
		names[index] = literal.Input()
	}
	return strings.Join(names, ",")
}

func canonicalNodeOf(graph Graph, name string) Node {
	node, ok := findNode(graph, name)
	if !ok {
		panic("node is missing: " + name)
	}
	return node
}

func canonicalEdgeGroupingFixture(t *testing.T, grouping string) ProgramDraft {
	t.Helper()
	ports := canonicalContract(t, canonicalField(t, "one", value.String()), canonicalField(t, "two", value.String()))
	edges := []EdgeDraft{{
		From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"},
		Bindings: []BindingDraft{{From: []string{"one"}, To: "one"}, {From: []string{"two"}, To: "two"}},
	}}
	switch grouping {
	case "combined":
	case "split":
		edges = []EdgeDraft{
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}, Bindings: []BindingDraft{{From: []string{"one"}, To: "one"}}},
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}, Bindings: []BindingDraft{{From: []string{"two"}, To: "two"}}},
		}
	case "split reversed":
		edges = []EdgeDraft{
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}, Bindings: []BindingDraft{{From: []string{"two"}, To: "two"}}},
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}, Bindings: []BindingDraft{{From: []string{"one"}, To: "one"}}},
		}
	case "data plus completion":
		edges = append(edges, EdgeDraft{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}})
	default:
		panic("unknown edge grouping " + grouping)
	}
	return bindingProgram(GraphDraft{
		Nodes: []NodeDraft{
			bindingLeaf("source", value.EmptyContract(), ports),
			bindingLeaf("target", ports, value.EmptyContract()),
		},
		Edges: edges,
	})
}

func canonicalCompletionGroupingFixture() ProgramDraft {
	return bindingProgram(GraphDraft{
		Nodes: []NodeDraft{bindingLeaf("source", value.EmptyContract(), value.EmptyContract()), bindingLeaf("target", value.EmptyContract(), value.EmptyContract())},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}},
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "target"}},
		},
	})
}

func canonicalNestedEdgeGroupingFixture(t *testing.T, grouping string) ProgramDraft {
	t.Helper()
	draft := canonicalEdgeGroupingFixture(t, grouping)
	root := canonicalRoot(&draft)
	root.Nodes = []NodeDraft{{Name: "nested", Graph: &GraphDraft{Nodes: root.Nodes, Edges: root.Edges, Inputs: value.EmptyContract(), Outputs: value.EmptyContract()}}}
	root.Edges = nil
	return draft
}

func canonicalDistinctEdgePairsFixture() ProgramDraft {
	return bindingProgram(GraphDraft{
		Nodes: []NodeDraft{
			bindingLeaf("source", value.EmptyContract(), value.EmptyContract()),
			bindingLeaf("first", value.EmptyContract(), value.EmptyContract()),
			bindingLeaf("second", value.EmptyContract(), value.EmptyContract()),
		},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "first"}},
			{From: EndpointDraft{Kind: Child, Child: "source"}, To: EndpointDraft{Kind: Child, Child: "second"}},
		},
	})
}

func assertCanonicalEdgeGroup(t *testing.T, graph Graph) {
	t.Helper()
	edges := graph.Edges()
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want one", len(edges))
	}
	bindings := edges[0].Bindings()
	if len(bindings) != 2 {
		t.Fatalf("bindings = %d, want two", len(bindings))
	}
	if got := []string{bindings[0].To(), bindings[1].To()}; !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("binding targets = %v, want [one two]", got)
	}
}
