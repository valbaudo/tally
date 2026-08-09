package workflow

import (
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// These route cases catch regressions that either reject a valid immediate
// lexical binding or store a binding differently from the declared edge.
func TestCompileBindingRoutes(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	for _, tc := range []struct {
		name  string
		graph GraphDraft
	}{
		{
			name: "graph input to child input",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"},
				Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
		},
		{
			name: "child output to sibling input",
			graph: GraphDraft{Nodes: []NodeDraft{
				bindingLeaf("read", value.EmptyContract(), text),
				bindingLeaf("write", text, value.EmptyContract()),
			}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Child, Child: "write"},
				Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
		},
		{
			name: "child output to graph output",
			graph: GraphDraft{Outputs: text, Nodes: []NodeDraft{bindingLeaf("read", value.EmptyContract(), text)}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Boundary},
				Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
		},
		{
			name: "graph input to graph output",
			graph: GraphDraft{Inputs: text, Outputs: text, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Boundary},
				Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Compile(bindingProgram(tc.graph))
			if err != nil {
				t.Fatal(err)
			}
			edge := def.Root().Edges()[0]
			if got := edge.Bindings()[0]; got.To() != "text" || !samePath(got.From(), []string{"text"}) {
				t.Fatalf("binding = from %v to %q, want text -> text", got.From(), got.To())
			}
		})
	}
}

// This catches source traversal that stops at the first port instead of using
// the statically declared object fields in the contract.
func TestCompileBindingResolvesObjectField(t *testing.T) {
	name := requiredField(t, "name", value.String())
	person := mustObject(t, name)
	people := bindingContract(t, false, "person", person)
	text := bindingContract(t, false, "text", value.String())
	def, err := Compile(bindingProgram(GraphDraft{
		Inputs: people,
		Nodes:  []NodeDraft{bindingLeaf("write", text, value.EmptyContract())},
		Edges: []EdgeDraft{{
			From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"},
			Bindings: []BindingDraft{{From: []string{"person", "name"}, To: "text"}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Root().Edges()[0].Bindings()[0].From(); !samePath(got, []string{"person", "name"}) {
		t.Fatalf("source path = %v, want [person name]", got)
	}
}

// This catches resolution leaking through a child boundary, accepting an
// invalid endpoint direction, or accepting absent declared ports.
func TestCompileReferenceRejectsInvalidLexicalReferences(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	for _, tc := range []struct {
		name  string
		graph GraphDraft
		wants []string
	}{
		{
			name:  "unresolved child",
			graph: GraphDraft{Inputs: text, Edges: []EdgeDraft{{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "missing"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}}}},
			wants: []string{"root", "workflow.dawn", "missing", "text", "unknown child"},
		},
		{
			name: "unresolved input",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"absent"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "boundary", "absent", "unknown source"},
		},
		{
			name: "unresolved output",
			graph: GraphDraft{Nodes: []NodeDraft{bindingLeaf("read", value.EmptyContract(), text), bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"absent"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "read", "absent", "unknown source"},
		},
		{
			name: "unresolved object field",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"text", "absent"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "boundary", "absent", "unknown source"},
		},
		{
			name: "descendant child name",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write/grandchild"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "write/grandchild", "text", "unknown child"},
		},
		{
			name: "literal slash-bearing child name",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("write/grandchild", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write/grandchild"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "write/grandchild", "text", "unknown child"},
		},
		{
			name: "boundary as source with output-only port",
			graph: GraphDraft{Outputs: text, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "boundary", "text", "unknown source"},
		},
		{
			name: "boundary as target with input-only port",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("read", value.EmptyContract(), text)}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "boundary", "text", "unknown target"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(bindingProgram(tc.graph))
			assertBindingError(t, err, tc.wants...)
		})
	}
}

// This catches type checks being deferred to execution and ensures an any
// source keeps the only runtime validation bit represented in the model.
func TestCompileBindingChecksTypes(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	number := bindingContract(t, false, "number", value.Number())
	_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("read", value.EmptyContract(), number),
		bindingLeaf("write", text, value.EmptyContract()),
	}, Edges: []EdgeDraft{{
		From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Child, Child: "write"},
		Bindings: []BindingDraft{{From: []string{"number"}, To: "text"}},
	}}}))
	assertBindingError(t, err, "root", "workflow.dawn", "read", "text", "not assignable")

	any := bindingContract(t, false, "value", value.Any())
	def, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("read", value.EmptyContract(), any),
		bindingLeaf("write", text, value.EmptyContract()),
	}, Edges: []EdgeDraft{{
		From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Child, Child: "write"},
		Bindings: []BindingDraft{{From: []string{"value"}, To: "text"}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	if !def.Root().Edges()[0].Bindings()[0].RuntimeValidation() {
		t.Fatal("any source binding did not retain runtime validation")
	}
}

// This catches source accounting that lets edge and literal bindings each bind
// the same target or allows a required target to remain unsupplied.
func TestCompileBindingRequiresExactlyOneSource(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	optionalText := bindingContract(t, true, "text", value.String())
	for _, tc := range []struct {
		name  string
		graph GraphDraft
		wants []string
	}{
		{
			name: "two edges to child input",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{
				{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
				{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			}},
			wants: []string{"root", "workflow.dawn", "write", "text", "bound more than once"},
		},
		{
			name: "edge and literal to child input",
			graph: GraphDraft{Inputs: text, Nodes: []NodeDraft{{Name: "write", Leaf: &LeafDraft{Kind: Script, Inputs: text, Outputs: value.EmptyContract()}, Literals: []LiteralBindingDraft{{Input: "text", Value: bindingLiteral(t, `"fixed"`)}}}}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "write", "text", "bound more than once"},
		},
		{
			name:  "missing required child input",
			graph: GraphDraft{Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}},
			wants: []string{"root", "workflow.dawn", "write", "text", "required input"},
		},
		{
			name:  "missing required graph output",
			graph: GraphDraft{Outputs: text},
			wants: []string{"root", "workflow.dawn", "boundary", "text", "required output"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(bindingProgram(tc.graph))
			assertBindingError(t, err, tc.wants...)
		})
	}

	def, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{bindingLeaf("write", optionalText, value.EmptyContract())}}))
	if err != nil {
		t.Fatalf("optional child input was rejected: %v", err)
	}
	if got := mustNode(t, def.Root(), "write").Literals(); len(got) != 0 {
		t.Fatalf("optional node literals = %v, want none", got)
	}
}

// This catches duplicate sibling names letting one lexical edge bind several
// distinct child declarations through the same endpoint key.
func TestCompileBindingRejectsDuplicateSiblingNames(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	_, err := Compile(bindingProgram(GraphDraft{
		Inputs: text,
		Nodes: []NodeDraft{
			bindingLeaf("write", text, value.EmptyContract()),
			bindingLeaf("write", text, value.EmptyContract()),
		},
		Edges: []EdgeDraft{{
			From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"},
			Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
		}},
	}))
	assertBindingError(t, err, "root", "workflow.dawn", "write", "duplicate child name")
}

// This catches treating an optional contract field as a guaranteed source for
// a required target while allowing the same conditional source for an optional
// target that can remain absent.
func TestCompileBindingRequiresPresentSourceForRequiredTarget(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	optionalText := bindingContract(t, true, "text", value.String())
	optionalName, err := value.Optional("name", value.String())
	if err != nil {
		t.Fatal(err)
	}
	person := bindingContract(t, false, "person", mustObject(t, optionalName))

	for _, tc := range []struct {
		name  string
		graph GraphDraft
		wants []string
	}{
		{
			name: "optional top-level source",
			graph: GraphDraft{Inputs: optionalText, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"},
				Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "boundary", "write", "text", "optional source", "required target"},
		},
		{
			name: "optional nested object field",
			graph: GraphDraft{Inputs: person, Nodes: []NodeDraft{bindingLeaf("write", text, value.EmptyContract())}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"},
				Bindings: []BindingDraft{{From: []string{"person", "name"}, To: "text"}},
			}}},
			wants: []string{"root", "workflow.dawn", "boundary", "write", "person.name", "text", "optional source", "required target"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(bindingProgram(tc.graph))
			assertBindingError(t, err, tc.wants...)
		})
	}

	def, err := Compile(bindingProgram(GraphDraft{Inputs: optionalText, Nodes: []NodeDraft{bindingLeaf("write", optionalText, value.EmptyContract())}, Edges: []EdgeDraft{{
		From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "write"},
		Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
	}}}))
	if err != nil {
		t.Fatalf("optional source to optional target was rejected: %v", err)
	}
	if got := len(def.Root().Edges()); got != 1 {
		t.Fatalf("edges = %d, want one optional binding edge", got)
	}
}

// This catches literal validation being skipped or literals being represented
// as edges rather than remaining direct child-input bindings.
func TestCompileLiteralValidatesTargetAndStaysOnNode(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	def, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{
		Name: "write", Leaf: &LeafDraft{Kind: Script, Inputs: text, Outputs: value.EmptyContract()},
		Literals: []LiteralBindingDraft{{Input: "text", Value: bindingLiteral(t, `"fixed"`)}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(def.Root().Edges()); got != 0 {
		t.Fatalf("edges = %d, want no literal edge", got)
	}
	if got := mustNode(t, def.Root(), "write").Literals(); len(got) != 1 || got[0].Input() != "text" {
		t.Fatalf("literals = %#v, want one text literal", got)
	}

	_, err = Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{
		Name: "write", Leaf: &LeafDraft{Kind: Script, Inputs: text, Outputs: value.EmptyContract()},
		Literals: []LiteralBindingDraft{{Input: "text", Value: bindingLiteral(t, "3")}},
	}}}))
	assertBindingError(t, err, "root", "workflow.dawn", "write", "text", "want string")
}

// This catches completion edges that accidentally create arbitrary boundary
// dependencies or allow a node to complete itself.
func TestCompileCompletionOnlyEdgeRequiresDistinctChildren(t *testing.T) {
	empty := value.EmptyContract()
	def, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("first", empty, empty), bindingLeaf("second", empty, empty),
	}, Edges: []EdgeDraft{{From: EndpointDraft{Kind: Child, Child: "first"}, To: EndpointDraft{Kind: Child, Child: "second"}}}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(def.Root().Edges()[0].Bindings()); got != 0 {
		t.Fatalf("completion bindings = %d, want zero", got)
	}

	for _, tc := range []EdgeDraft{
		{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "first"}},
		{From: EndpointDraft{Kind: Child, Child: "first"}, To: EndpointDraft{Kind: Boundary}},
		{From: EndpointDraft{Kind: Child, Child: "first"}, To: EndpointDraft{Kind: Child, Child: "first"}},
	} {
		_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{bindingLeaf("first", empty, empty)}, Edges: []EdgeDraft{tc}}))
		assertBindingError(t, err, "root", "workflow.dawn", "completion-only")
	}
}

// This catches endpoint-resolution diagnostics inventing a port for a
// completion-only edge, which has no binding port to report.
func TestCompileCompletionReferenceErrorHasNoPort(t *testing.T) {
	empty := value.EmptyContract()
	_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("worker", empty, empty),
	}, Edges: []EdgeDraft{{
		From: EndpointDraft{Kind: Child, Child: "missing"}, To: EndpointDraft{Kind: Child, Child: "worker"},
	}}}))
	assertBindingError(t, err, "root", "workflow.dawn", "missing", "unknown child")
	if strings.Contains(err.Error(), `port ""`) {
		t.Fatalf("completion endpoint error = %q, must not invent an empty port", err)
	}
}

// This catches ParallelDraft lowering retaining dependency edges between its
// immediate branches, whether they carry data or only completion ordering.
func TestCompileParallelRejectsChildOrderingEdges(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	empty := value.EmptyContract()
	for _, tc := range []struct {
		name  string
		graph GraphDraft
		wants []string
	}{
		{
			name: "data edge",
			graph: GraphDraft{Nodes: []NodeDraft{{Name: "parallel", Parallel: &ParallelDraft{Graph: GraphDraft{Inputs: empty, Outputs: empty, Nodes: []NodeDraft{
				bindingLeaf("read", empty, text),
				bindingLeaf("write", text, empty),
			}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Child, Child: "write"},
				Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
			}}}}}}},
			wants: []string{"root", "workflow.dawn", "read", "write", "text", "parallel", "ordering"},
		},
		{
			name: "completion edge",
			graph: GraphDraft{Nodes: []NodeDraft{{Name: "parallel", Parallel: &ParallelDraft{Graph: GraphDraft{Inputs: empty, Outputs: empty, Nodes: []NodeDraft{
				bindingLeaf("first", empty, empty),
				bindingLeaf("second", empty, empty),
			}, Edges: []EdgeDraft{{
				From: EndpointDraft{Kind: Child, Child: "first"}, To: EndpointDraft{Kind: Child, Child: "second"},
			}}}}}}},
			wants: []string{"root", "workflow.dawn", "first", "second", "parallel", "ordering"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(bindingProgram(tc.graph))
			assertBindingError(t, err, tc.wants...)
		})
	}
}

// This catches treating parallel branches as unable to receive shared boundary
// data or collectively supply distinct boundary outputs.
func TestCompileParallelAllowsBoundaryFanOutAndFanIn(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	first := bindingContract(t, false, "first", value.String())
	second := bindingContract(t, false, "second", value.String())
	outputs := bindingFieldsContract(t, requiredField(t, "first", value.String()), requiredField(t, "second", value.String()))
	parallel := GraphDraft{
		Inputs: text, Outputs: outputs,
		Nodes: []NodeDraft{
			bindingLeaf("first", text, first),
			bindingLeaf("second", text, second),
		},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "first"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "second"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			{From: EndpointDraft{Kind: Child, Child: "first"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"first"}, To: "first"}}},
			{From: EndpointDraft{Kind: Child, Child: "second"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"second"}, To: "second"}}},
		},
	}
	def, err := Compile(bindingProgram(GraphDraft{
		Inputs: text, Outputs: outputs,
		Nodes: []NodeDraft{{Name: "parallel", Parallel: &ParallelDraft{Graph: parallel}}},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "parallel"}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			{From: EndpointDraft{Kind: Child, Child: "parallel"}, To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"first"}, To: "first"}, {From: []string{"second"}, To: "second"}}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	parallelNode := mustNode(t, def.Root(), "parallel")
	scope, ok := parallelNode.Scope()
	if !ok {
		t.Fatal("parallel node is not a graph scope")
	}
	graph, ok := scope.Graph()
	if !ok || len(graph.Edges()) != 4 {
		t.Fatalf("parallel graph edges = %d, want four boundary edges", len(graph.Edges()))
	}
}

// This catches applying ParallelDraft's no-ordering rule to ordinary graphs.
func TestCompileOrdinaryGraphAllowsChildOrdering(t *testing.T) {
	text := bindingContract(t, false, "text", value.String())
	def, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("read", value.EmptyContract(), text),
		bindingLeaf("write", text, value.EmptyContract()),
	}, Edges: []EdgeDraft{{
		From: EndpointDraft{Kind: Child, Child: "read"}, To: EndpointDraft{Kind: Child, Child: "write"},
		Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(def.Root().Edges()); got != 1 {
		t.Fatalf("ordinary graph edges = %d, want one child ordering edge", got)
	}
}

func bindingProgram(graph GraphDraft) ProgramDraft {
	if !graph.Inputs.Valid() {
		graph.Inputs = value.EmptyContract()
	}
	if !graph.Outputs.Valid() {
		graph.Outputs = value.EmptyContract()
	}
	return ProgramDraft{Root: "root", Modules: []ModuleDraft{{
		Name: "root", Provenance: Provenance{Source: "workflow.dawn"}, Graph: graph,
	}}}
}

func bindingLeaf(name string, inputs, outputs value.Contract) NodeDraft {
	return NodeDraft{Name: name, Leaf: &LeafDraft{Kind: Script, Inputs: inputs, Outputs: outputs}}
}

func bindingContract(t *testing.T, optional bool, name string, typ value.Type) value.Contract {
	t.Helper()
	field := requiredField(t, name, typ)
	if optional {
		var err error
		field, err = value.Optional(name, typ)
		if err != nil {
			t.Fatal(err)
		}
	}
	contract, err := value.NewContract(field)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func bindingFieldsContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func requiredField(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func mustObject(t *testing.T, fields ...value.Field) value.Type {
	t.Helper()
	object, err := value.Object(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func bindingLiteral(t *testing.T, data string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return literal
}

func assertBindingError(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("Compile() succeeded, want binding validation error")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Compile() error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "runtime path") {
		t.Fatalf("Compile() error = %q, must not mention a runtime path", err)
	}
}

func samePath(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
