package workflow

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/valbaudo/dawn/value"
)

func TestCanonicalKindsAreClosed(t *testing.T) {
	for _, kind := range []LeafKind{LLM, Agent, Script, Gate} {
		if !validLeafKind(kind) {
			t.Fatalf("validLeafKind(%q) = false", kind)
		}
	}
	if validLeafKind(LeafKind("plugin")) {
		t.Fatal("plugin-defined leaf kind was accepted")
	}
	for _, kind := range []ScopeKind{GraphScope, BranchScope, MapScope, LoopScope, FinallyScope} {
		if !validScopeKind(kind) {
			t.Fatalf("validScopeKind(%q) = false", kind)
		}
	}
	if validScopeKind(ScopeKind("plugin")) {
		t.Fatal("plugin-defined scope kind was accepted")
	}
}

func TestModelAcceptsExactlyOneDraftVariant(t *testing.T) {
	contract := value.EmptyContract()
	graph := GraphDraft{Inputs: contract, Outputs: contract}
	variants := []struct {
		name  string
		draft NodeDraft
	}{
		{"leaf", NodeDraft{Leaf: &LeafDraft{Kind: Script, Inputs: contract, Outputs: contract}}},
		{"graph", NodeDraft{Graph: &graph}},
		{"branch", NodeDraft{Branch: &BranchDraft{Inputs: contract, Outputs: contract}}},
		{"map", NodeDraft{Map: &MapDraft{Inputs: contract, Outputs: contract, Body: graph}}},
		{"loop", NodeDraft{Loop: &LoopDraft{Inputs: contract, Outputs: contract, Body: graph}}},
		{"call", NodeDraft{Call: &CallDraft{Module: "child"}}},
		{"parallel", NodeDraft{Parallel: &ParallelDraft{Graph: graph}}},
	}
	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			draft := tc.draft
			draft.Name = tc.name
			if _, err := modelNodeForTest(draft); err != nil {
				t.Fatalf("modelNodeForTest(%s) error = %v", tc.name, err)
			}
		})
	}

	if _, err := modelNodeForTest(NodeDraft{
		Name:  "invalid",
		Leaf:  &LeafDraft{Kind: Script, Inputs: contract, Outputs: contract},
		Graph: &graph,
	}); err == nil {
		t.Fatal("modelNodeForTest accepted two node variants")
	}
}

func TestModelEndpointKindsKeepBoundaryAndChildrenDistinct(t *testing.T) {
	boundary := Endpoint{kind: Boundary}
	child := Endpoint{kind: Child, child: "worker"}
	if boundary.Kind() != Boundary {
		t.Fatalf("boundary Kind() = %v, want Boundary", boundary.Kind())
	}
	if child.Kind() != Child || child.Child() != "worker" {
		t.Fatalf("child = kind %v name %q, want Child worker", child.Kind(), child.Child())
	}
	if boundary == child {
		t.Fatal("boundary and child endpoints are indistinguishable")
	}
}

func TestDefinitionImmutableAfterDraftMutation(t *testing.T) {
	input, err := value.Required("input", value.String())
	if err != nil {
		t.Fatal(err)
	}
	contract, err := value.NewContract(input)
	if err != nil {
		t.Fatal(err)
	}
	literal, err := value.ParseLiteral([]byte(`"fixed"`))
	if err != nil {
		t.Fatal(err)
	}
	draft := GraphDraft{
		Inputs:  contract,
		Outputs: value.EmptyContract(),
		Nodes: []NodeDraft{{
			Name:       "worker",
			Leaf:       &LeafDraft{Kind: Script, Inputs: contract, Outputs: value.EmptyContract()},
			Literals:   []LiteralBindingDraft{{Input: "input", Value: literal}},
			Provenance: Provenance{Source: "node", Module: "root", Origin: OriginAuthored},
		}},
		Edges:      []EdgeDraft{{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "worker"}, Bindings: []BindingDraft{{From: []string{"input"}, To: "input"}}}},
		Provenance: Provenance{Source: "graph", Module: "root", Origin: OriginAuthored},
	}
	definition, err := modelDefinitionForTest(draft)
	if err != nil {
		t.Fatal(err)
	}

	draft.Nodes[0].Name = "changed"
	draft.Nodes[0].Literals[0].Input = "changed"
	draft.Edges[0].Bindings[0].From[0] = "changed"
	draft.Provenance.Source = "changed"
	ports := contract.Ports()
	ports[0] = value.Field{}

	root := definition.Root()
	if root.Provenance().Source != "graph" {
		t.Fatalf("Root().Provenance().Source = %q, want graph", root.Provenance().Source)
	}
	node := root.Nodes()[0]
	if node.Name() != "worker" {
		t.Fatalf("Node().Name() = %q, want worker", node.Name())
	}
	if node.Literals()[0].Input() != "input" {
		t.Fatalf("literal input = %q, want input", node.Literals()[0].Input())
	}
	if root.Edges()[0].Bindings()[0].From()[0] != "input" {
		t.Fatalf("binding source = %q, want input", root.Edges()[0].Bindings()[0].From()[0])
	}
	leaf, ok := node.Leaf()
	if !ok {
		t.Fatal("node leaf was changed through its draft contract")
	}
	if got := leaf.Inputs().Ports()[0].Name(); got != "input" {
		t.Fatalf("leaf input name = %q, want input", got)
	}
}

func TestDefinitionImmutableThroughAccessors(t *testing.T) {
	contract := value.EmptyContract()
	definition, err := modelDefinitionForTest(GraphDraft{
		Inputs: contract,
		Nodes: []NodeDraft{{
			Name:     "worker",
			Leaf:     &LeafDraft{Kind: Script, Inputs: contract, Outputs: contract},
			Literals: []LiteralBindingDraft{{Input: "constant", Value: mustLiteral(t, `"fixed"`)}},
		}},
		Edges: []EdgeDraft{{
			From: EndpointDraft{Kind: Boundary},
			To:   EndpointDraft{Kind: Child, Child: "worker"},
			Bindings: []BindingDraft{{
				From: []string{"input"},
				To:   "constant",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	root := definition.Root()
	nodes := root.Nodes()
	nodes[0] = Node{}
	edges := root.Edges()
	edges[0] = Edge{}
	bindings := root.Edges()[0].Bindings()
	bindings[0] = Binding{}
	path := root.Edges()[0].Bindings()[0].From()
	path[0] = "changed"
	literals := root.Nodes()[0].Literals()
	literals[0] = LiteralBinding{}

	stable := definition.Root()
	if stable.Nodes()[0].Name() != "worker" {
		t.Fatalf("Node().Name() = %q, want worker", stable.Nodes()[0].Name())
	}
	if stable.Edges()[0].Bindings()[0].From()[0] != "input" {
		t.Fatalf("binding source = %q, want input", stable.Edges()[0].Bindings()[0].From()[0])
	}
	if stable.Nodes()[0].Literals()[0].Input() != "constant" {
		t.Fatalf("literal input = %q, want constant", stable.Nodes()[0].Literals()[0].Input())
	}
}

func TestDefinitionImmutableThroughScopeAccessors(t *testing.T) {
	contract := value.EmptyContract()
	definition, err := modelDefinitionForTest(GraphDraft{Nodes: []NodeDraft{
		{
			Name: "choose",
			Branch: &BranchDraft{
				Inputs: contract, Outputs: contract,
				Cases: []CaseDraft{{Name: "yes", Graph: GraphDraft{Inputs: contract, Outputs: contract}}},
			},
		},
		{
			Name: "repeat",
			Loop: &LoopDraft{
				Inputs: contract, Outputs: contract, Termination: []string{"done"}, Body: GraphDraft{Inputs: contract, Outputs: contract},
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	branchScope, ok := definition.Root().Nodes()[0].Scope()
	if !ok {
		t.Fatal("choose node is not a scope")
	}
	branch, ok := branchScope.Branch()
	if !ok {
		t.Fatal("choose scope is not a branch")
	}
	cases := branch.Cases()
	cases[0] = Case{}

	loopScope, ok := definition.Root().Nodes()[1].Scope()
	if !ok {
		t.Fatal("repeat node is not a scope")
	}
	loop, ok := loopScope.Loop()
	if !ok {
		t.Fatal("repeat scope is not a loop")
	}
	termination := loop.Termination()
	termination[0] = "changed"

	stableBranch, _ := definition.Root().Nodes()[0].Scope()
	stableCases, _ := stableBranch.Branch()
	if stableCases.Cases()[0].Name() != "yes" {
		t.Fatalf("case name = %q, want yes", stableCases.Cases()[0].Name())
	}
	stableLoop, _ := definition.Root().Nodes()[1].Scope()
	stableTermination, _ := stableLoop.Loop()
	if stableTermination.Termination()[0] != "done" {
		t.Fatalf("termination = %q, want done", stableTermination.Termination()[0])
	}
}

func TestLeafDeliveryAccessorsAreDefensive(t *testing.T) {
	leaf := Leaf{
		baseTree:         []string{"source"},
		publishWorkspace: []string{"continued"},
		attachments: []Attachment{{
			input: []string{"payload", "document"}, fidelity: VisualFidelity,
		}},
	}

	base := leaf.BaseTree()
	base[0] = "changed"
	publication := leaf.PublishWorkspace()
	publication[0] = "changed"
	attachments := leaf.Attachments()
	attachmentPath := attachments[0].Input()
	attachmentPath[0] = "changed"
	attachments[0] = Attachment{}

	if got, want := leaf.BaseTree(), []string{"source"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("BaseTree() = %v, want %v", got, want)
	}
	if got, want := leaf.PublishWorkspace(), []string{"continued"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PublishWorkspace() = %v, want %v", got, want)
	}
	stable := leaf.Attachments()
	if got, want := stable[0].Input(), []string{"payload", "document"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attachment Input() = %v, want %v", got, want)
	}
	if got := stable[0].Fidelity(); got != VisualFidelity {
		t.Fatalf("attachment Fidelity() = %v, want VisualFidelity", got)
	}
}

func mustLiteral(t *testing.T, source string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return literal
}

// modelDefinitionForTest deliberately remains test-local until Compile owns
// lowering and validation. It builds enough canonical records to exercise the
// model's copy boundaries without becoming a second production entry point.
func modelDefinitionForTest(draft GraphDraft) (Definition, error) {
	root, err := modelGraphForTest(draft)
	if err != nil {
		return Definition{}, err
	}
	return Definition{root: root}, nil
}

func modelGraphForTest(draft GraphDraft) (Graph, error) {
	graph := Graph{
		inputs:     cloneContractForTest(draft.Inputs),
		outputs:    cloneContractForTest(draft.Outputs),
		provenance: draft.Provenance,
	}
	for _, nodeDraft := range draft.Nodes {
		node, err := modelNodeForTest(nodeDraft)
		if err != nil {
			return Graph{}, err
		}
		graph.nodes = append(graph.nodes, node)
	}
	for _, edgeDraft := range draft.Edges {
		edge := Edge{
			from: Endpoint{kind: edgeDraft.From.Kind, child: edgeDraft.From.Child},
			to:   Endpoint{kind: edgeDraft.To.Kind, child: edgeDraft.To.Child},
		}
		for _, bindingDraft := range edgeDraft.Bindings {
			edge.bindings = append(edge.bindings, Binding{
				from: append([]string(nil), bindingDraft.From...),
				to:   bindingDraft.To,
			})
		}
		graph.edges = append(graph.edges, edge)
	}
	if draft.Finally != nil {
		cleanup, err := modelGraphForTest(*draft.Finally)
		if err != nil {
			return Graph{}, err
		}
		graph.cleanup = &Finally{graph: cleanup}
	}
	return graph, nil
}

func modelNodeForTest(draft NodeDraft) (Node, error) {
	variants := 0
	for _, present := range []bool{
		draft.Leaf != nil,
		draft.Graph != nil,
		draft.Branch != nil,
		draft.Map != nil,
		draft.Loop != nil,
		draft.Call != nil,
		draft.Parallel != nil,
	} {
		if present {
			variants++
		}
	}
	if variants != 1 {
		return Node{}, fmt.Errorf("node %q has %d variants, want exactly one", draft.Name, variants)
	}
	node := Node{name: draft.Name, provenance: draft.Provenance}
	for _, literal := range draft.Literals {
		node.literals = append(node.literals, LiteralBinding{
			input: literal.Input,
			value: cloneLiteralForTest(literal.Value),
		})
	}
	switch {
	case draft.Leaf != nil:
		node.leaf = &Leaf{
			kind:    draft.Leaf.Kind,
			inputs:  cloneContractForTest(draft.Leaf.Inputs),
			outputs: cloneContractForTest(draft.Leaf.Outputs),
		}
	case draft.Graph != nil:
		graph, err := modelGraphForTest(*draft.Graph)
		if err != nil {
			return Node{}, err
		}
		node.scope = &Scope{kind: GraphScope, graph: &graph}
	case draft.Branch != nil:
		branch, err := modelBranchForTest(*draft.Branch)
		if err != nil {
			return Node{}, err
		}
		node.scope = &Scope{kind: BranchScope, branch: &branch}
	case draft.Map != nil:
		body, err := modelGraphForTest(draft.Map.Body)
		if err != nil {
			return Node{}, err
		}
		mapped := Map{
			inputs:     cloneContractForTest(draft.Map.Inputs),
			outputs:    cloneContractForTest(draft.Map.Outputs),
			collection: draft.Map.Collection,
			result:     draft.Map.Result,
			body:       body,
		}
		node.scope = &Scope{kind: MapScope, map_: &mapped}
	case draft.Loop != nil:
		body, err := modelGraphForTest(draft.Loop.Body)
		if err != nil {
			return Node{}, err
		}
		loop := Loop{
			inputs:      cloneContractForTest(draft.Loop.Inputs),
			outputs:     cloneContractForTest(draft.Loop.Outputs),
			maximum:     draft.Loop.Maximum,
			termination: append([]string(nil), draft.Loop.Termination...),
			body:        body,
		}
		node.scope = &Scope{kind: LoopScope, loop: &loop}
	case draft.Call != nil:
		// Task 3 owns module resolution and observable call lowering.
		node.scope = &Scope{kind: GraphScope, graph: &Graph{}}
	case draft.Parallel != nil:
		// Task 3 owns parallel lowering and its provenance assignment.
		graph, err := modelGraphForTest(draft.Parallel.Graph)
		if err != nil {
			return Node{}, err
		}
		node.scope = &Scope{kind: GraphScope, graph: &graph}
	}
	return node, nil
}

func modelBranchForTest(draft BranchDraft) (Branch, error) {
	branch := Branch{
		inputs:   cloneContractForTest(draft.Inputs),
		outputs:  cloneContractForTest(draft.Outputs),
		selector: draft.Selector,
	}
	for _, caseDraft := range draft.Cases {
		graph, err := modelGraphForTest(caseDraft.Graph)
		if err != nil {
			return Branch{}, err
		}
		branch.cases = append(branch.cases, Case{name: caseDraft.Name, graph: graph})
	}
	return branch, nil
}

func cloneContractForTest(contract value.Contract) value.Contract {
	copy, err := value.NewContract(contract.Ports()...)
	if err != nil {
		return value.Contract{}
	}
	return copy
}

func cloneLiteralForTest(literal value.Literal) value.Literal {
	copy, err := value.ParseLiteral(literal.Bytes())
	if err != nil {
		return value.Literal{}
	}
	return copy
}
