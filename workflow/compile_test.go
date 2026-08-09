package workflow

import (
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// This catches a compiler regression that leaves author-facing call or parallel
// nodes in the compiled definition instead of turning them into graph scopes.
func TestCompileLowersCallAndParallelToGraphs(t *testing.T) {
	def, err := Compile(loweringFixture(t))
	if err != nil {
		t.Fatal(err)
	}

	call := mustNode(t, def.Root(), "analysis")
	if call.ScopeKind() != GraphScope || call.Provenance().Origin != OriginCall {
		t.Fatalf("call = kind %q provenance %#v", call.ScopeKind(), call.Provenance())
	}
	parallel := mustNode(t, def.Root(), "reviewers")
	if parallel.ScopeKind() != GraphScope || parallel.Provenance().Origin != OriginParallel {
		t.Fatalf("parallel = kind %q provenance %#v", parallel.ScopeKind(), parallel.Provenance())
	}
}

// This catches a compiler regression that flattens a called module's descendants
// into its caller or exposes contracts other than the module boundary.
func TestCompileCallKeepsModuleBoundary(t *testing.T) {
	def, err := Compile(loweringFixture(t))
	if err != nil {
		t.Fatal(err)
	}

	if len(def.Root().Nodes()) != 2 {
		t.Fatalf("root nodes = %d, want only its two immediate children", len(def.Root().Nodes()))
	}
	if _, found := findNode(def.Root(), "write"); found {
		t.Fatal("called module descendant is addressable in its caller")
	}
	call, ok := mustNode(t, def.Root(), "analysis").Scope()
	if !ok {
		t.Fatal("analysis is not a scope")
	}
	graph, ok := call.Graph()
	if !ok {
		t.Fatal("analysis is not a graph scope")
	}
	if got := graph.Inputs().Ports()[0].Name(); got != "request" {
		t.Fatalf("call input = %q, want module boundary request", got)
	}
	if got := graph.Outputs().Ports()[0].Name(); got != "report" {
		t.Fatalf("call output = %q, want module boundary report", got)
	}
	if _, found := findNode(graph, "write"); !found {
		t.Fatal("called module graph is missing its declared child")
	}
}

// This catches a compiler regression that resolves only the first call level.
func TestCompileLowersNestedCall(t *testing.T) {
	def, err := Compile(ProgramDraft{
		Root: "root",
		Modules: []ModuleDraft{
			module("root", GraphDraft{Nodes: []NodeDraft{{Name: "first", Call: &CallDraft{Module: "middle"}}}}),
			module("middle", GraphDraft{Nodes: []NodeDraft{{Name: "second", Call: &CallDraft{Module: "leaf"}}}}),
			module("leaf", GraphDraft{Nodes: []NodeDraft{{Name: "work", Leaf: scriptLeaf()}}}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := mustNode(t, def.Root(), "first").Scope()
	firstGraph, _ := first.Graph()
	second := mustNode(t, firstGraph, "second")
	if second.ScopeKind() != GraphScope || second.Provenance().Origin != OriginCall {
		t.Fatalf("nested call = kind %q provenance %#v", second.ScopeKind(), second.Provenance())
	}
}

// This catches accepting a program whose entry module cannot be resolved.
func TestCompileRejectsAbsentRootModule(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "missing", Modules: []ModuleDraft{module("other", GraphDraft{})}})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Compile() error = %v, want missing root module", err)
	}
}

// This catches ambiguous static module resolution.
func TestCompileRejectsDuplicateModuleNames(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{}),
		module("root", GraphDraft{}),
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate module") {
		t.Fatalf("Compile() error = %v, want duplicate module rejection", err)
	}
}

// This catches nameless module and child declarations, which cannot form stable
// lexical module or graph scopes.
func TestCompileRejectsEmptyModuleAndNodeNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		draft ProgramDraft
	}{
		{
			name:  "module",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{module("", GraphDraft{})}},
		},
		{
			name: "node",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{
				module("root", GraphDraft{Nodes: []NodeDraft{{Leaf: scriptLeaf()}}}),
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(tc.draft); err == nil {
				t.Fatal("Compile() accepted an empty name")
			}
		})
	}
}

// This catches calls that remain unresolved until a scheduler runs them.
func TestCompileRejectsUnknownCall(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{{Name: "missing", Call: &CallDraft{Module: "nope"}}}}),
	}})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Compile() error = %v, want unknown call rejection", err)
	}
}

// This catches a direct recursive module call, which cannot lower to a finite
// static graph.
func TestCompileRejectsDirectRecursiveCall(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{{Name: "again", Call: &CallDraft{Module: "root"}}}}),
	}})
	if err == nil || !strings.Contains(err.Error(), "root -> root") {
		t.Fatalf("Compile() error = %v, want direct call chain", err)
	}
}

// This catches cycle detection that checks only a call's immediate parent.
func TestCompileRejectsIndirectRecursiveCall(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{{Name: "alpha", Call: &CallDraft{Module: "alpha"}}}}),
		module("alpha", GraphDraft{Nodes: []NodeDraft{{Name: "beta", Call: &CallDraft{Module: "beta"}}}}),
		module("beta", GraphDraft{Nodes: []NodeDraft{{Name: "again", Call: &CallDraft{Module: "alpha"}}}}),
	}})
	if err == nil || !strings.Contains(err.Error(), "root -> alpha -> beta -> alpha") {
		t.Fatalf("Compile() error = %v, want complete indirect call chain", err)
	}
}

// This catches lowering a one-child author parallel into a graph, where it
// would silently erase the requirement for actual parallel grouping.
func TestCompileRejectsParallelWithFewerThanTwoChildren(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{{
			Name: "parallel",
			Parallel: &ParallelDraft{Graph: GraphDraft{Nodes: []NodeDraft{{
				Name: "only", Leaf: scriptLeaf(),
			}}}},
		}}}),
	}})
	if err == nil || !strings.Contains(err.Error(), "at least two") {
		t.Fatalf("Compile() error = %v, want parallel child count rejection", err)
	}
}

// This catches an invalid draft node being arbitrarily lowered according to one
// of its variants.
func TestCompileRejectsNodeWithMultipleVariants(t *testing.T) {
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{{
			Name: "ambiguous", Leaf: scriptLeaf(), Graph: &GraphDraft{},
		}}}),
	}})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("Compile() error = %v, want node variant rejection", err)
	}
}

// This catches retaining any draft-owned graph, node, edge, binding, or literal
// slice in the compiled definition.
func TestCompileCopiesDraftOwnedState(t *testing.T) {
	fixed, err := value.Required("fixed", value.String())
	if err != nil {
		t.Fatal(err)
	}
	other, err := value.Required("other", value.String())
	if err != nil {
		t.Fatal(err)
	}
	groupInputs, err := value.NewContract(fixed, other)
	if err != nil {
		t.Fatal(err)
	}
	nested := GraphDraft{Inputs: groupInputs, Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "work", Leaf: scriptLeaf()}}}
	cleanup := GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "clean", Leaf: scriptLeaf()}}}
	draft := ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{
			Inputs: contract(t, "input"),
			Nodes: []NodeDraft{{
				Name: "group", Graph: &nested,
				Literals: []LiteralBindingDraft{{Input: "fixed", Value: mustLiteral(t, `"value"`)}},
			}},
			Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "group"},
				Bindings: []BindingDraft{{From: []string{"input"}, To: "other"}},
			}},
			Finally: &cleanup,
		}),
	}}
	def, err := Compile(draft)
	if err != nil {
		t.Fatal(err)
	}

	draft.Modules[0].Graph.Nodes[0].Name = "changed"
	draft.Modules[0].Graph.Nodes[0].Literals[0].Input = "changed"
	draft.Modules[0].Graph.Edges[0].Bindings[0].From[0] = "changed"
	nested.Nodes[0].Name = "changed"
	cleanup.Nodes[0].Name = "changed"

	root := def.Root()
	group := mustNode(t, root, "group")
	if group.Literals()[0].Input() != "fixed" {
		t.Fatalf("literal input = %q, want fixed", group.Literals()[0].Input())
	}
	if root.Edges()[0].Bindings()[0].From()[0] != "input" {
		t.Fatalf("binding source = %q, want input", root.Edges()[0].Bindings()[0].From()[0])
	}
	scope, _ := group.Scope()
	graph, _ := scope.Graph()
	if mustNode(t, graph, "work").Name() != "work" {
		t.Fatal("nested graph changed through its draft")
	}
	finally, ok := root.Finally()
	if !ok || mustNode(t, finally.Graph(), "clean").Name() != "clean" {
		t.Fatal("finally graph changed through its draft")
	}
}

// This catches recursive pointers in public graph drafts causing Compile to
// exhaust the process stack instead of rejecting malformed input.
func TestCompileRejectsRecursiveDraftGraphPointers(t *testing.T) {
	const recursiveCase = "DAWN_COMPILE_RECURSIVE_GRAPH_CASE"
	if name := os.Getenv(recursiveCase); name != "" {
		debug.SetMaxStack(64 << 10)
		if _, err := Compile(recursiveGraphFixture(name)); err == nil {
			t.Fatal("Compile() accepted a recursive graph pointer")
		}
		return
	}

	for _, name := range []string{"finally", "inline"} {
		t.Run(name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestCompileRejectsRecursiveDraftGraphPointers$")
			command.Env = append(os.Environ(), recursiveCase+"="+name)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("Compile() did not return an ordinary error for a recursive graph pointer: %v\n%s", err, output)
			}
		})
	}
}

// This catches recursive structured drafts being copied on each compilation
// frame, which previously evaded the active graph-pointer guard and overflowed
// the process stack.
func TestCompileRejectsRecursiveStructuredDraftPointers(t *testing.T) {
	const recursiveCase = "DAWN_COMPILE_RECURSIVE_STRUCTURED_CASE"
	if name := os.Getenv(recursiveCase); name != "" {
		debug.SetMaxStack(64 << 10)
		if _, err := Compile(recursiveStructuredFixture(name)); err == nil {
			t.Fatal("Compile() accepted a recursive structured draft pointer")
		}
		return
	}

	for _, name := range []string{"branch", "map", "loop"} {
		t.Run(name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestCompileRejectsRecursiveStructuredDraftPointers$")
			command.Env = append(os.Environ(), recursiveCase+"="+name)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("Compile() did not return an ordinary error for a recursive %s draft pointer: %v\n%s", name, err, output)
			}
		})
	}
}

// This catches an active-pointer guard that rejects legitimate reuse after the
// first independent occurrence has finished compiling.
func TestCompileAllowsReusedNonRecursiveDraftGraph(t *testing.T) {
	reused := GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "work", Leaf: scriptLeaf()}}}
	def, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{
			{Name: "first", Graph: &reused},
			{Name: "second", Graph: &reused},
		}}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		scope, ok := mustNode(t, def.Root(), name).Scope()
		if !ok {
			t.Fatalf("%s is not a graph scope", name)
		}
		graph, _ := scope.Graph()
		if _, found := findNode(graph, "work"); !found {
			t.Fatalf("%s did not compile its reused graph", name)
		}
	}
}

// This catches active-pointer protection that mistakes separate uses of an
// already-finished branch, map, or loop draft for recursive expansion.
func TestCompileAllowsReusedNonRecursiveStructuredDraft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []NodeDraft
	}{
		{
			name:  "branch",
			nodes: reusedBranchNodes(t),
		},
		{
			name:  "map",
			nodes: reusedMapNodes(t),
		},
		{
			name:  "loop",
			nodes: reusedLoopNodes(t),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
				module("root", GraphDraft{Nodes: tc.nodes}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(def.Root().Nodes()) != 2 {
				t.Fatalf("compiled nodes = %d, want 2 reused occurrences", len(def.Root().Nodes()))
			}
		})
	}
}

// This catches authored drafts forging lowering provenance or a module name
// other than the module currently being compiled.
func TestCompileOwnsAuthoredProvenance(t *testing.T) {
	inner := GraphDraft{
		Inputs:     value.EmptyContract(),
		Outputs:    value.EmptyContract(),
		Provenance: Provenance{Source: "inner.dawn", Module: "forged", Origin: OriginCall},
		Nodes: []NodeDraft{{
			Name: "work", Leaf: scriptLeaf(),
			Provenance: Provenance{Source: "work.dawn", Module: "forged", Origin: OriginParallel},
		}},
	}
	def, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{
			Provenance: Provenance{Source: "root.dawn", Module: "forged", Origin: OriginParallel},
			Nodes: []NodeDraft{{
				Name: "group", Graph: &inner,
				Provenance: Provenance{Source: "group.dawn", Module: "forged", Origin: OriginCall},
			}},
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}

	assertAuthoredProvenance(t, def.Root().Provenance(), "root.dawn", "root")
	group := mustNode(t, def.Root(), "group")
	assertAuthoredProvenance(t, group.Provenance(), "group.dawn", "root")
	scope, _ := group.Scope()
	graph, _ := scope.Graph()
	assertAuthoredProvenance(t, graph.Provenance(), "inner.dawn", "root")
	assertAuthoredProvenance(t, mustNode(t, graph, "work").Provenance(), "work.dawn", "root")
}

func recursiveGraphFixture(name string) ProgramDraft {
	graph := GraphDraft{}
	switch name {
	case "finally":
		graph.Finally = &graph
	case "inline":
		graph.Nodes = []NodeDraft{{Name: "self", Graph: &graph}}
	default:
		panic("unknown recursive graph fixture " + name)
	}
	return ProgramDraft{Root: "root", Modules: []ModuleDraft{module("root", graph)}}
}

func recursiveStructuredFixture(name string) ProgramDraft {
	empty := value.EmptyContract()
	var node NodeDraft
	switch name {
	case "branch":
		branch := &BranchDraft{Inputs: empty, Outputs: empty}
		branch.Cases = []CaseDraft{{Name: "again", Graph: GraphDraft{
			Inputs: empty, Outputs: empty,
			Nodes: []NodeDraft{{Name: "again", Branch: branch}},
		}}}
		node = NodeDraft{Name: "structured", Branch: branch}
	case "map":
		mapped := &MapDraft{Inputs: empty, Outputs: empty}
		mapped.Body = GraphDraft{
			Inputs: empty, Outputs: empty,
			Nodes: []NodeDraft{{Name: "again", Map: mapped}},
		}
		node = NodeDraft{Name: "structured", Map: mapped}
	case "loop":
		loop := &LoopDraft{Inputs: empty, Outputs: empty}
		loop.Body = GraphDraft{
			Inputs: empty, Outputs: empty,
			Nodes: []NodeDraft{{Name: "again", Loop: loop}},
		}
		node = NodeDraft{Name: "structured", Loop: loop}
	default:
		panic("unknown recursive structured fixture " + name)
	}
	return ProgramDraft{Root: "root", Modules: []ModuleDraft{module("root", GraphDraft{Nodes: []NodeDraft{node}})}}
}

func reusedBranchNodes(t *testing.T) []NodeDraft {
	t.Helper()
	selected := contractOf(t, requiredPort(t, "selected", value.Boolean()))
	branch := &BranchDraft{
		Inputs: selected, Outputs: value.EmptyContract(), Selector: "selected",
		Cases: []CaseDraft{
			{Name: "false", Graph: GraphDraft{Inputs: selected, Outputs: value.EmptyContract()}},
			{Name: "true", Graph: GraphDraft{Inputs: selected, Outputs: value.EmptyContract()}},
		},
	}
	literal := mustLiteral(t, "true")
	return []NodeDraft{
		{Name: "first", Branch: branch, Literals: []LiteralBindingDraft{{Input: "selected", Value: literal}}},
		{Name: "second", Branch: branch, Literals: []LiteralBindingDraft{{Input: "selected", Value: literal}}},
	}
}

func reusedMapNodes(t *testing.T) []NodeDraft {
	t.Helper()
	items := requiredPort(t, "items", listType(t, value.String()))
	mapInputs := contractOf(t, items)
	item := requiredPort(t, "item", value.String())
	index := requiredPort(t, "index", value.Integer())
	bodyInputs := contractOf(t, item, index)
	resultType := listType(t, value.EmptyContract().ObjectType())
	mapOutputs := contractOf(t, requiredPort(t, "result", resultType))
	mapped := &MapDraft{
		Inputs: mapInputs, Outputs: mapOutputs, Collection: "items", Result: "result",
		Body: GraphDraft{Inputs: bodyInputs, Outputs: value.EmptyContract()},
	}
	literal := mustLiteral(t, `["one"]`)
	return []NodeDraft{
		{Name: "first", Map: mapped, Literals: []LiteralBindingDraft{{Input: "items", Value: literal}}},
		{Name: "second", Map: mapped, Literals: []LiteralBindingDraft{{Input: "items", Value: literal}}},
	}
}

func reusedLoopNodes(t *testing.T) []NodeDraft {
	t.Helper()
	empty := value.EmptyContract()
	stop := requiredPort(t, "stop", value.Boolean())
	bodyOutputs := contractOf(t, stop)
	initial := requiredPort(t, "initial", empty.ObjectType())
	previous := optionalPort(t, "previous", bodyOutputs.ObjectType())
	iteration := requiredPort(t, "iteration", value.Integer())
	loop := &LoopDraft{
		Inputs: empty, Outputs: bodyOutputs, Maximum: 1, Termination: []string{"stop"},
		Body: GraphDraft{
			Inputs: contractOf(t, initial, previous, iteration), Outputs: bodyOutputs,
			Nodes: []NodeDraft{{Name: "produce", Leaf: &LeafDraft{Kind: Script, Inputs: empty, Outputs: bodyOutputs}}},
			Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "produce"}, To: EndpointDraft{Kind: Boundary},
				Bindings: []BindingDraft{{From: []string{"stop"}, To: "stop"}},
			}},
		},
	}
	return []NodeDraft{{Name: "first", Loop: loop}, {Name: "second", Loop: loop}}
}

func requiredPort(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	port, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func optionalPort(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	port, err := value.Optional(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func contractOf(t *testing.T, ports ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(ports...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func listType(t *testing.T, element value.Type) value.Type {
	t.Helper()
	typ, err := value.List(element)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func assertAuthoredProvenance(t *testing.T, got Provenance, source, module string) {
	t.Helper()
	if got.Source != source || got.Module != module || got.Origin != OriginAuthored {
		t.Fatalf("provenance = %#v, want source %q module %q origin %q", got, source, module, OriginAuthored)
	}
}

func loweringFixture(t *testing.T) ProgramDraft {
	t.Helper()
	request := contract(t, "request")
	report := contract(t, "report")
	return ProgramDraft{
		Root: "root",
		Modules: []ModuleDraft{
			module("root", GraphDraft{Inputs: request, Nodes: []NodeDraft{
				{Name: "analysis", Call: &CallDraft{Module: "analyze"}},
				{Name: "reviewers", Parallel: &ParallelDraft{Graph: GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{
					{Name: "first", Leaf: scriptLeaf()},
					{Name: "second", Leaf: scriptLeaf()},
				}}}},
			}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "analysis"},
				Bindings: []BindingDraft{{From: []string{"request"}, To: "request"}},
			}}}),
			module("analyze", GraphDraft{Inputs: request, Outputs: report, Nodes: []NodeDraft{
				{Name: "write", Call: &CallDraft{Module: "report"}},
			}, Edges: []EdgeDraft{
				{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"request"}, To: "request"}}},
				{From: EndpointDraft{Kind: Child, Child: "write"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"report"}, To: "report"}}},
			}}),
			module("report", GraphDraft{Inputs: request, Outputs: report, Nodes: []NodeDraft{
				{Name: "render", Leaf: &LeafDraft{Kind: Script, Inputs: request, Outputs: report}},
			}, Edges: []EdgeDraft{
				{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "render"}, Bindings: []BindingDraft{{From: []string{"request"}, To: "request"}}},
				{From: EndpointDraft{Kind: Child, Child: "render"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"report"}, To: "report"}}},
			}}),
		},
	}
}

func module(name string, graph GraphDraft) ModuleDraft {
	if !graph.Inputs.Valid() {
		graph.Inputs = value.EmptyContract()
	}
	if !graph.Outputs.Valid() {
		graph.Outputs = value.EmptyContract()
	}
	return ModuleDraft{Name: name, Graph: graph, Provenance: Provenance{Source: name + ".dawn"}}
}

func scriptLeaf() *LeafDraft {
	return &LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: value.EmptyContract()}
}

func contract(t *testing.T, name string) value.Contract {
	t.Helper()
	port, err := value.Required(name, value.String())
	if err != nil {
		t.Fatal(err)
	}
	contract, err := value.NewContract(port)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func mustNode(t *testing.T, graph Graph, name string) Node {
	t.Helper()
	node, ok := findNode(graph, name)
	if !ok {
		t.Fatalf("node %q not found", name)
	}
	return node
}

func findNode(graph Graph, name string) (Node, bool) {
	for _, node := range graph.Nodes() {
		if node.Name() == name {
			return node, true
		}
	}
	return Node{}, false
}
