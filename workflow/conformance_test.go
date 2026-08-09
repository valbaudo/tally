package workflow

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// This catches a compiler regression that admits an alternate execution
// hierarchy for a general agent workflow instead of lowering author-facing
// calls and parallel work into the closed canonical model.
func TestPrestigeShapeCompilesIntoClosedHierarchy(t *testing.T) {
	def, err := Compile(prestigeDraft(t, false))
	if err != nil {
		t.Fatal(err)
	}

	root := def.Root()
	assertClosedKinds(t, root)
	assertLoweredOrigin(t, root, "recon", OriginCall)
	assertLoweredOrigin(t, root, "review", OriginParallel)
	assertNoDependency(t, mustGraph(t, root, "review"), "static", "dynamic")
	assertEncapsulated(t, root)
	assertNoRuntimePaths(t, root)
	assertOptionalWorkTopology(t, root)
	assertRevisionTopology(t, root)

	aggregate := mustNode(t, root, "aggregate")
	if !hasEdge(root, "aggregate", "", "result") {
		t.Fatal("root result does not come from the declared aggregate output")
	}
	aggregateLeaf, ok := aggregate.Leaf()
	if !ok || aggregateLeaf.Kind() != Script {
		t.Fatal("aggregate is not a script leaf")
	}
}

// This catches optional work being represented as two identical branch bodies
// rather than one zero-work path and one explicit work path with a shared
// contract.
func assertOptionalWorkTopology(t *testing.T, root Graph) {
	t.Helper()
	scope, ok := mustNode(t, root, "optional").Scope()
	if !ok || scope.Kind() != BranchScope {
		t.Fatal("optional work is not a branch scope")
	}
	branch, ok := scope.Branch()
	if !ok {
		t.Fatal("optional work has no branch record")
	}
	wantInputs := prestigeContract(t, prestigeRequired(t, "selected", value.Boolean()))
	if !branch.Inputs().Equal(wantInputs) || !branch.Outputs().Equal(value.EmptyContract()) {
		t.Fatalf("optional branch contracts = %v -> %v, want selected boolean -> empty", branch.Inputs(), branch.Outputs())
	}
	if len(branch.Cases()) != 2 {
		t.Fatalf("optional branch cases = %d, want exactly two", len(branch.Cases()))
	}
	falseCase, trueCase := mustCase(t, branch, "false"), mustCase(t, branch, "true")
	for _, branchCase := range []Case{falseCase, trueCase} {
		if !branchCase.Graph().Inputs().Equal(branch.Inputs()) || !branchCase.Graph().Outputs().Equal(branch.Outputs()) {
			t.Fatalf("optional case %q does not preserve the branch contract", branchCase.Name())
		}
	}
	if len(falseCase.Graph().Nodes()) != 0 {
		t.Fatal("false optional case contains work")
	}
	work := mustNode(t, trueCase.Graph(), "work")
	leaf, ok := work.Leaf()
	if !ok || leaf.Kind() != Script || len(trueCase.Graph().Nodes()) != 1 {
		t.Fatal("true optional case does not contain exactly the script work leaf")
	}
}

// This catches a loop whose worker creates its own verdict or whose parent
// gate is disconnected from the loop result instead of consuming that verdict
// through the one ordinary edge relation.
func assertRevisionTopology(t *testing.T, root Graph) {
	t.Helper()
	scope, ok := mustNode(t, root, "revise").Scope()
	if !ok || scope.Kind() != LoopScope {
		t.Fatal("revision is not a loop scope")
	}
	loop, ok := scope.Loop()
	if !ok {
		t.Fatal("revision has no loop record")
	}
	if !samePath(loop.Termination(), []string{"passed"}) {
		t.Fatalf("loop termination = %v, want [passed]", loop.Termination())
	}
	body := loop.Body()
	worker := mustNode(t, body, "worker")
	if leaf, ok := worker.Leaf(); !ok || leaf.Kind() != Agent {
		t.Fatal("revision worker is not an agent leaf")
	}
	judge := mustNode(t, body, "judge")
	judgeLeaf, ok := judge.Leaf()
	if !ok || (judgeLeaf.Kind() != LLM && judgeLeaf.Kind() != Agent) {
		t.Fatal("revision judge is not an llm or agent leaf")
	}
	if !judgeLeaf.Outputs().Equal(loop.Outputs()) {
		t.Fatal("revision judge does not produce the loop verdict contract")
	}
	if !hasEdge(body, "worker", "judge", "draft") {
		t.Fatal("revision worker does not feed the judge")
	}
	if !hasEdge(body, "judge", "", "passed") || !hasEdge(body, "judge", "", "reason") {
		t.Fatal("revision judge verdict does not drive the loop body outputs")
	}

	gate := mustNode(t, root, "gate")
	gateLeaf, ok := gate.Leaf()
	if !ok || gateLeaf.Kind() != Gate {
		t.Fatal("parent deterministic gate is not a gate leaf")
	}
	if len(gate.Literals()) != 0 {
		t.Fatal("parent deterministic gate is supplied by a literal")
	}
	if !hasEdge(root, "revise", "gate", "passed") || !hasEdge(root, "revise", "gate", "reason") {
		t.Fatal("loop verdict does not feed the parent deterministic gate")
	}
}

// This catches source ordering accidentally becoming a second dependency
// mechanism: reversing independent sibling declarations must not add an edge
// or alter the compiled definition.
func TestProductInvariantSourceOrderDoesNotSequenceIndependentWork(t *testing.T) {
	forward, err := Compile(prestigeDraft(t, false))
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := Compile(prestigeDraft(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forward.Canonical(), reversed.Canonical()) {
		t.Fatal("reversing independent reviewer source order changed the canonical definition")
	}
	assertNoDependency(t, mustGraph(t, forward.Root(), "review"), "static", "dynamic")
}

// These cases catch invalid values that can enter the public source-neutral
// draft surface. Plugin-defined scopes and parallel policy knobs have no
// public representation, so they intentionally have no source-text or
// reflection blacklist test here.
func TestProductInvariantRejectsInvalidDrafts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		draft func(*testing.T) ProgramDraft
		want  string
	}{
		{
			name: "plugin-defined leaf", want: "unknown leaf kind",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				mustDraftNode(t, &draft, "aggregate").Leaf.Kind = LeafKind("plugin")
				return draft
			},
		},
		{
			name: "fake literal-producing node", want: "unknown leaf kind",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				mustDraftNode(t, &draft, "aggregate").Leaf.Kind = LeafKind("literal")
				return draft
			},
		},
		{
			name: "runtime-selected module name", want: "unknown module",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				mustDraftNode(t, &draft, "recon").Call.Module = "inputs.selected_module"
				return draft
			},
		},
		{
			name: "undeclared opaque name cannot reference nested reviewer", want: "unknown child",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				root := mustDraftRoot(t, &draft)
				root.Edges = append(root.Edges, EdgeDraft{
					From:     EndpointDraft{Kind: Child, Child: "review/static"},
					To:       EndpointDraft{Kind: Child, Child: "aggregate"},
					Bindings: []BindingDraft{{From: []string{"report"}, To: "static"}},
				})
				return draft
			},
		},
		{
			name: "fixed graph cycle", want: "dependency cycle",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				root := mustDraftRoot(t, &draft)
				root.Edges = append(root.Edges,
					EdgeDraft{From: childEndpoint("aggregate"), To: childEndpoint("gate")},
					EdgeDraft{From: childEndpoint("gate"), To: childEndpoint("aggregate")},
				)
				return draft
			},
		},
		{
			name: "non-exhaustive branch", want: "branch cases must be exactly false, true",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				mustDraftNode(t, &draft, "optional").Branch.Cases = mustDraftNode(t, &draft, "optional").Branch.Cases[:1]
				return draft
			},
		},
		{
			name: "gate-owned decision configuration", want: "gate inputs",
			draft: func(t *testing.T) ProgramDraft {
				draft := prestigeDraft(t, false)
				gate := mustDraftNode(t, &draft, "gate").Leaf
				configuration := "qu" + "orum"
				gate.Inputs = prestigeContract(t,
					prestigeRequired(t, "passed", value.Boolean()),
					prestigeOptional(t, "reason", value.String()),
					prestigeRequired(t, configuration, value.Integer()),
				)
				mustDraftNode(t, &draft, "gate").Literals = append(mustDraftNode(t, &draft, "gate").Literals,
					LiteralBindingDraft{Input: configuration, Value: prestigeLiteral(t, "2")})
				return draft
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.draft(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func prestigeDraft(t *testing.T, reverseReviewers bool) ProgramDraft {
	t.Helper()
	document, err := value.File("application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	brief, err := value.Object(prestigeRequired(t, "title", value.String()))
	if err != nil {
		t.Fatal(err)
	}
	rootInputs := prestigeContract(t,
		prestigeRequired(t, "brief", brief),
		prestigeRequired(t, "document", document),
	)
	summary := prestigeContract(t, prestigeRequired(t, "summary", value.String()))
	review := prestigeContract(t,
		prestigeRequired(t, "dynamic", value.String()),
		prestigeRequired(t, "static", value.String()),
	)
	result := prestigeContract(t, prestigeRequired(t, "result", value.String()))
	selected := prestigeContract(t, prestigeRequired(t, "selected", value.Boolean()))

	reviewNodes := []NodeDraft{
		{Name: "static", Leaf: &LeafDraft{Kind: Agent, Inputs: summary, Outputs: prestigeContract(t, prestigeRequired(t, "static", value.String()))}},
		{Name: "dynamic", Leaf: &LeafDraft{Kind: Agent, Inputs: summary, Outputs: prestigeContract(t, prestigeRequired(t, "dynamic", value.String()))}},
	}
	if reverseReviewers {
		reviewNodes[0], reviewNodes[1] = reviewNodes[1], reviewNodes[0]
	}
	reviewGraph := GraphDraft{
		Inputs: summary, Outputs: review, Nodes: reviewNodes,
		Edges: []EdgeDraft{
			{From: boundaryEndpoint(), To: childEndpoint("static"), Bindings: []BindingDraft{{From: []string{"summary"}, To: "summary"}}},
			{From: boundaryEndpoint(), To: childEndpoint("dynamic"), Bindings: []BindingDraft{{From: []string{"summary"}, To: "summary"}}},
			{From: childEndpoint("static"), To: boundaryEndpoint(), Bindings: []BindingDraft{{From: []string{"static"}, To: "static"}}},
			{From: childEndpoint("dynamic"), To: boundaryEndpoint(), Bindings: []BindingDraft{{From: []string{"dynamic"}, To: "dynamic"}}},
		},
	}

	loopInputs := prestigeContract(t, prestigeRequired(t, "prompt", value.String()))
	loopOutputs := prestigeContract(t,
		prestigeRequired(t, "passed", value.Boolean()),
		prestigeOptional(t, "reason", value.String()),
	)
	workerOutputs := prestigeContract(t, prestigeRequired(t, "draft", value.String()))
	loopBodyInputs := prestigeContract(t,
		prestigeRequired(t, "initial", loopInputs.ObjectType()),
		prestigeOptional(t, "previous", loopOutputs.ObjectType()),
		prestigeRequired(t, "iteration", value.Integer()),
	)
	loop := LoopDraft{
		Inputs: loopInputs, Outputs: loopOutputs, Maximum: 3, Termination: []string{"passed"},
		Body: GraphDraft{
			Inputs: loopBodyInputs, Outputs: loopOutputs,
			Nodes: []NodeDraft{
				{Name: "worker", Leaf: &LeafDraft{Kind: Agent, Inputs: prestigeContract(t,
					prestigeRequired(t, "initial", loopInputs.ObjectType()),
					prestigeRequired(t, "iteration", value.Integer()),
				), Outputs: workerOutputs}},
				{Name: "judge", Leaf: &LeafDraft{Kind: LLM, Inputs: workerOutputs, Outputs: loopOutputs}},
			},
			Edges: []EdgeDraft{
				{From: boundaryEndpoint(), To: childEndpoint("worker"), Bindings: []BindingDraft{{From: []string{"initial"}, To: "initial"}, {From: []string{"iteration"}, To: "iteration"}}},
				{From: childEndpoint("worker"), To: childEndpoint("judge"), Bindings: []BindingDraft{{From: []string{"draft"}, To: "draft"}}},
				{From: childEndpoint("judge"), To: boundaryEndpoint(), Bindings: []BindingDraft{{From: []string{"passed"}, To: "passed"}, {From: []string{"reason"}, To: "reason"}}},
			},
		},
	}

	mapInputs := prestigeContract(t, prestigeRequired(t, "items", prestigeList(t, value.String())))
	mapBodyOutputs := value.EmptyContract()
	mapped := MapDraft{
		Inputs: mapInputs, Outputs: prestigeMapOutputs(t, "results", mapBodyOutputs), Collection: "items", Result: "results",
		Body: GraphDraft{
			Inputs: prestigeContract(t, prestigeRequired(t, "item", value.String()), prestigeRequired(t, "index", value.Integer())), Outputs: mapBodyOutputs,
			Nodes: []NodeDraft{{Name: "index", Leaf: &LeafDraft{Kind: Script, Inputs: prestigeContract(t, prestigeRequired(t, "item", value.String()), prestigeRequired(t, "index", value.Integer())), Outputs: value.EmptyContract()}}},
			Edges: []EdgeDraft{{From: boundaryEndpoint(), To: childEndpoint("index"), Bindings: []BindingDraft{{From: []string{"item"}, To: "item"}, {From: []string{"index"}, To: "index"}}}},
		},
	}

	optionalCase := func(withWork bool) GraphDraft {
		graph := GraphDraft{Inputs: selected, Outputs: value.EmptyContract()}
		if !withWork {
			return graph
		}
		graph.Nodes = []NodeDraft{{Name: "work", Leaf: &LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: value.EmptyContract()}}}
		return graph
	}
	root := GraphDraft{
		Inputs: rootInputs, Outputs: result,
		Nodes: []NodeDraft{
			{Name: "recon", Call: &CallDraft{Module: "recon"}},
			{Name: "review", Parallel: &ParallelDraft{Graph: reviewGraph}},
			{Name: "aggregate", Leaf: &LeafDraft{Kind: Script, Inputs: review, Outputs: result}},
			{Name: "optional", Branch: &BranchDraft{Inputs: selected, Outputs: value.EmptyContract(), Selector: "selected", Cases: []CaseDraft{{Name: "false", Graph: optionalCase(false)}, {Name: "true", Graph: optionalCase(true)}}}, Literals: []LiteralBindingDraft{{Input: "selected", Value: prestigeLiteral(t, "true")}}},
			{Name: "revise", Loop: &loop, Literals: []LiteralBindingDraft{{Input: "prompt", Value: prestigeLiteral(t, `"revise"`)}}},
			{Name: "index", Map: &mapped, Literals: []LiteralBindingDraft{{Input: "items", Value: prestigeLiteral(t, "[]")}}},
			{Name: "gate", Leaf: &LeafDraft{Kind: Gate, Inputs: prestigeGateInputs(t), Outputs: value.EmptyContract()}},
		},
		Edges: []EdgeDraft{
			{From: boundaryEndpoint(), To: childEndpoint("recon"), Bindings: []BindingDraft{{From: []string{"brief"}, To: "brief"}, {From: []string{"document"}, To: "document"}}},
			{From: childEndpoint("recon"), To: childEndpoint("review"), Bindings: []BindingDraft{{From: []string{"summary"}, To: "summary"}}},
			{From: childEndpoint("review"), To: childEndpoint("aggregate"), Bindings: []BindingDraft{{From: []string{"static"}, To: "static"}, {From: []string{"dynamic"}, To: "dynamic"}}},
			{From: childEndpoint("aggregate"), To: boundaryEndpoint(), Bindings: []BindingDraft{{From: []string{"result"}, To: "result"}}},
			{From: childEndpoint("revise"), To: childEndpoint("gate"), Bindings: []BindingDraft{{From: []string{"passed"}, To: "passed"}, {From: []string{"reason"}, To: "reason"}}},
		},
		Finally: &GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "cleanup", Leaf: &LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: value.EmptyContract()}}}},
	}
	return ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", root),
		module("recon", GraphDraft{
			Inputs: rootInputs, Outputs: summary,
			Nodes: []NodeDraft{{Name: "research", Leaf: &LeafDraft{Kind: LLM, Inputs: rootInputs, Outputs: summary}}},
			Edges: []EdgeDraft{
				{From: boundaryEndpoint(), To: childEndpoint("research"), Bindings: []BindingDraft{{From: []string{"brief"}, To: "brief"}, {From: []string{"document"}, To: "document"}}},
				{From: childEndpoint("research"), To: boundaryEndpoint(), Bindings: []BindingDraft{{From: []string{"summary"}, To: "summary"}}},
			},
		}),
	}}
}

func assertClosedKinds(t *testing.T, graph Graph) {
	t.Helper()
	leaves := make(map[LeafKind]bool)
	scopes := make(map[ScopeKind]bool)
	var visit func(Graph)
	visit = func(current Graph) {
		for _, node := range current.Nodes() {
			if leaf, ok := node.Leaf(); ok {
				if !validLeafKind(leaf.Kind()) {
					t.Fatalf("node %q has non-canonical leaf kind %q", node.Name(), leaf.Kind())
				}
				leaves[leaf.Kind()] = true
				continue
			}
			scope, ok := node.Scope()
			if !ok || !validScopeKind(scope.Kind()) {
				t.Fatalf("node %q has non-canonical scope", node.Name())
			}
			scopes[scope.Kind()] = true
			switch scope.Kind() {
			case GraphScope:
				inner, _ := scope.Graph()
				visit(inner)
			case BranchScope:
				branch, _ := scope.Branch()
				for _, branchCase := range branch.Cases() {
					visit(branchCase.Graph())
				}
			case MapScope:
				mapped, _ := scope.Map()
				visit(mapped.Body())
			case LoopScope:
				loop, _ := scope.Loop()
				visit(loop.Body())
			}
		}
		if cleanup, ok := current.Finally(); ok {
			if cleanup.Kind() != FinallyScope {
				t.Fatalf("cleanup kind = %q, want %q", cleanup.Kind(), FinallyScope)
			}
			scopes[cleanup.Kind()] = true
			visit(cleanup.Graph())
		}
	}
	visit(graph)
	for _, kind := range []LeafKind{LLM, Agent, Script, Gate} {
		if !leaves[kind] {
			t.Fatalf("fixture did not compile canonical leaf kind %q", kind)
		}
	}
	for _, kind := range []ScopeKind{GraphScope, BranchScope, MapScope, LoopScope, FinallyScope} {
		if !scopes[kind] {
			t.Fatalf("fixture did not compile canonical scope kind %q", kind)
		}
	}
}

func assertLoweredOrigin(t *testing.T, graph Graph, name string, origin Origin) {
	t.Helper()
	node := mustNode(t, graph, name)
	if node.ScopeKind() != GraphScope || node.Provenance().Origin != origin {
		t.Fatalf("%s = kind %q provenance %#v", name, node.ScopeKind(), node.Provenance())
	}
	scope, _ := node.Scope()
	inner, _ := scope.Graph()
	if inner.Provenance().Origin != origin {
		t.Fatalf("%s inner graph provenance = %#v", name, inner.Provenance())
	}
}

func assertNoDependency(t *testing.T, graph Graph, left, right string) {
	t.Helper()
	for _, edge := range graph.Edges() {
		if (edge.From().Child() == left && edge.To().Child() == right) || (edge.From().Child() == right && edge.To().Child() == left) {
			t.Fatalf("independent siblings %q and %q have edge %#v", left, right, edge)
		}
	}
}

func assertEncapsulated(t *testing.T, graph Graph) {
	t.Helper()
	for _, edge := range graph.Edges() {
		for _, endpoint := range []Endpoint{edge.From(), edge.To()} {
			if endpoint.Kind() == Child {
				if strings.Contains(endpoint.Child(), "/") {
					t.Fatalf("graph edge crosses a scope boundary through %q", endpoint.Child())
				}
				if _, ok := findNode(graph, endpoint.Child()); !ok {
					t.Fatalf("graph edge references non-immediate child %q", endpoint.Child())
				}
			}
		}
	}
	for _, node := range graph.Nodes() {
		if scope, ok := node.Scope(); ok {
			switch scope.Kind() {
			case GraphScope:
				inner, _ := scope.Graph()
				assertEncapsulated(t, inner)
			case BranchScope:
				branch, _ := scope.Branch()
				for _, branchCase := range branch.Cases() {
					assertEncapsulated(t, branchCase.Graph())
				}
			case MapScope:
				mapped, _ := scope.Map()
				assertEncapsulated(t, mapped.Body())
			case LoopScope:
				loop, _ := scope.Loop()
				assertEncapsulated(t, loop.Body())
			}
		}
	}
}

func assertNoRuntimePaths(t *testing.T, graph Graph) {
	t.Helper()
	for _, edge := range graph.Edges() {
		for _, endpoint := range []Endpoint{edge.From(), edge.To()} {
			if strings.Contains(endpoint.Child(), "/") {
				t.Fatalf("endpoint contains runtime path %q", endpoint.Child())
			}
		}
		for _, binding := range edge.Bindings() {
			for _, part := range binding.From() {
				if strings.Contains(part, "/") {
					t.Fatalf("binding contains runtime path %q", part)
				}
			}
		}
	}
	if cleanup, ok := graph.Finally(); ok {
		assertNoRuntimePaths(t, cleanup.Graph())
	}
	for _, node := range graph.Nodes() {
		if scope, ok := node.Scope(); ok {
			switch scope.Kind() {
			case GraphScope:
				inner, _ := scope.Graph()
				assertNoRuntimePaths(t, inner)
			case BranchScope:
				branch, _ := scope.Branch()
				for _, branchCase := range branch.Cases() {
					assertNoRuntimePaths(t, branchCase.Graph())
				}
			case MapScope:
				mapped, _ := scope.Map()
				assertNoRuntimePaths(t, mapped.Body())
			case LoopScope:
				loop, _ := scope.Loop()
				assertNoRuntimePaths(t, loop.Body())
			}
		}
	}
}

func mustGraph(t *testing.T, graph Graph, name string) Graph {
	t.Helper()
	scope, ok := mustNode(t, graph, name).Scope()
	if !ok {
		t.Fatalf("node %q is not a scope", name)
	}
	inner, ok := scope.Graph()
	if !ok {
		t.Fatalf("node %q is not a graph scope", name)
	}
	return inner
}

func mustCase(t *testing.T, branch Branch, name string) Case {
	t.Helper()
	for _, branchCase := range branch.Cases() {
		if branchCase.Name() == name {
			return branchCase
		}
	}
	t.Fatalf("branch case %q not found", name)
	return Case{}
}

func hasEdge(graph Graph, from, to, targetPort string) bool {
	for _, edge := range graph.Edges() {
		if edge.From().Child() != from || edge.To().Child() != to {
			continue
		}
		for _, binding := range edge.Bindings() {
			if binding.To() == targetPort {
				return true
			}
		}
	}
	return false
}

func mustDraftRoot(t *testing.T, draft *ProgramDraft) *GraphDraft {
	t.Helper()
	for index := range draft.Modules {
		if draft.Modules[index].Name == draft.Root {
			return &draft.Modules[index].Graph
		}
	}
	t.Fatalf("root module %q not found", draft.Root)
	return nil
}

func mustDraftNode(t *testing.T, draft *ProgramDraft, name string) *NodeDraft {
	t.Helper()
	root := mustDraftRoot(t, draft)
	for index := range root.Nodes {
		if root.Nodes[index].Name == name {
			return &root.Nodes[index]
		}
	}
	t.Fatalf("draft node %q not found", name)
	return nil
}

func boundaryEndpoint() EndpointDraft { return EndpointDraft{Kind: Boundary} }

func childEndpoint(name string) EndpointDraft { return EndpointDraft{Kind: Child, Child: name} }

func prestigeRequired(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func prestigeOptional(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Optional(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func prestigeContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func prestigeLiteral(t *testing.T, source string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return literal
}

func prestigeGateInputs(t *testing.T) value.Contract {
	t.Helper()
	return prestigeContract(t, prestigeRequired(t, "passed", value.Boolean()), prestigeOptional(t, "reason", value.String()))
}

func prestigeList(t *testing.T, element value.Type) value.Type {
	t.Helper()
	list, err := value.List(element)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func prestigeMapOutputs(t *testing.T, result string, body value.Contract) value.Contract {
	t.Helper()
	list := prestigeList(t, body.ObjectType())
	return prestigeContract(t, prestigeRequired(t, result, list))
}
