package workflow

import (
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// These cases catch a compiler that treats data bindings and completion-only
// edges as separate dependency relations, or accepts a static cycle because it
// happens to be inside an already lowered scope.
func TestCompileRejectsDataDependencyCycle(t *testing.T) {
	text := validationContract(t, validationRequired(t, "value", value.String()))
	_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("b", text, text),
		bindingLeaf("a", text, text),
	}, Edges: []EdgeDraft{
		validationEdge("b", "a", "value"),
		validationEdge("a", "b", "value"),
	}}))
	assertValidationError(t, err, "a -> b -> a")
}

func TestCompileRejectsMixedDependencyCycle(t *testing.T) {
	text := validationContract(t, validationRequired(t, "value", value.String()))
	_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("c", value.EmptyContract(), value.EmptyContract()),
		bindingLeaf("a", value.EmptyContract(), text),
		bindingLeaf("b", text, value.EmptyContract()),
	}, Edges: []EdgeDraft{
		{From: validationChild("c"), To: validationChild("a")},
		validationEdge("a", "b", "value"),
		{From: validationChild("b"), To: validationChild("c")},
	}}))
	assertValidationError(t, err, "a -> b -> c -> a")
}

func TestCompileRejectsSelfDependencyCycle(t *testing.T) {
	text := validationContract(t, validationRequired(t, "value", value.String()))
	_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{
		bindingLeaf("a", text, text),
	}, Edges: []EdgeDraft{validationEdge("a", "a", "value")}}))
	assertValidationError(t, err, "a -> a")
}

func TestCompileRejectsDependencyCycleInCalledModule(t *testing.T) {
	text := validationContract(t, validationRequired(t, "value", value.String()))
	_, err := Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{
		module("root", GraphDraft{Nodes: []NodeDraft{{Name: "child", Call: &CallDraft{Module: "child"}}}}),
		module("child", GraphDraft{Nodes: []NodeDraft{
			bindingLeaf("b", text, text), bindingLeaf("a", text, text),
		}, Edges: []EdgeDraft{validationEdge("b", "a", "value"), validationEdge("a", "b", "value")}}),
	}})
	assertValidationError(t, err, "a -> b -> a")
}

// This catches treating the repeated runtime instances of a bounded loop body
// as static graph edges.
func TestCompileAllowsBoundedLoopBody(t *testing.T) {
	loop := validationLoop(t)
	if _, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{Name: "repeat", Loop: &loop}}})); err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompileRejectsMalformedBranch(t *testing.T) {
	boolSelector := validationContract(t, validationRequired(t, "selected", value.Boolean()))
	enum := validationEnum(t, `"alpha"`, `"beta"`)
	enumSelector := validationContract(t, validationRequired(t, "selected", enum))
	for _, tc := range []struct {
		name   string
		branch BranchDraft
		want   string
	}{
		{
			name: "selector is neither boolean nor enum",
			branch: validationBranch(boolSelector, []CaseDraft{
				validationCase("true", boolSelector), validationCase("false", boolSelector),
			}),
			want: "selector",
		},
		{
			name: "boolean cases are closed",
			branch: BranchDraft{Inputs: boolSelector, Selector: "selected", Cases: []CaseDraft{
				validationCase("true", boolSelector), validationCase("default", boolSelector),
			}},
			want: "cases",
		},
		{
			name: "enum cases are closed",
			branch: BranchDraft{Inputs: enumSelector, Selector: "selected", Cases: []CaseDraft{
				validationCase(`"alpha"`, enumSelector), validationCase("fallthrough", enumSelector),
			}},
			want: "cases",
		},
		{
			name: "case contracts match the branch",
			branch: BranchDraft{Inputs: boolSelector, Selector: "selected", Cases: []CaseDraft{
				validationCase("true", value.EmptyContract()), validationCase("false", boolSelector),
			}},
			want: "input contract",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := tc.branch
			if !branch.Outputs.Valid() {
				branch.Outputs = value.EmptyContract()
			}
			selector := `true`
			if _, ok := branch.Inputs.Resolve("selected"); ok && branch.Inputs.Ports()[0].Type().Kind() == value.EnumKind {
				selector = `"alpha"`
			}
			_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{
				Name: "choose", Branch: &branch,
				Literals: []LiteralBindingDraft{{Input: "selected", Value: validationLiteral(t, selector)}},
			}}}))
			assertValidationError(t, err, tc.want)
		})
	}
}

func TestCompileRejectsMalformedMap(t *testing.T) {
	stringsList := validationList(t, value.String())
	result := validationMapResult(t, value.EmptyContract())
	validBody := GraphDraft{Inputs: validationContract(t,
		validationRequired(t, "item", value.String()),
		validationRequired(t, "index", value.Integer()),
	), Outputs: value.EmptyContract()}
	for _, tc := range []struct {
		name string
		map_ MapDraft
		want string
	}{
		{
			name: "collection is the only list input",
			map_: MapDraft{Inputs: validationContract(t, validationRequired(t, "items", value.String())), Collection: "items", Result: "result", Outputs: result, Body: validBody},
			want: "collection",
		},
		{
			name: "body has fixed inputs",
			map_: MapDraft{Inputs: validationContract(t, validationRequired(t, "items", stringsList)), Collection: "items", Result: "result", Outputs: result, Body: GraphDraft{Inputs: validationContract(t, validationRequired(t, "item", value.String())), Outputs: value.EmptyContract()}},
			want: "body input",
		},
		{
			name: "result is a list of the body output object",
			map_: MapDraft{Inputs: validationContract(t, validationRequired(t, "items", stringsList)), Collection: "items", Result: "result", Outputs: validationContract(t, validationRequired(t, "other", result.Ports()[0].Type())), Body: validBody},
			want: "result",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapped := tc.map_
			collection := `[]`
			if field, ok := mapped.Inputs.Resolve(mapped.Collection); ok && field.Kind() == value.StringKind {
				collection = `"items"`
			}
			_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{
				Name: "each", Map: &mapped,
				Literals: []LiteralBindingDraft{{Input: mapped.Collection, Value: validationLiteral(t, collection)}},
			}}}))
			assertValidationError(t, err, tc.want)
		})
	}
}

func TestCompileRejectsMalformedLoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*LoopDraft)
		want string
	}{
		{
			name: "maximum is positive",
			edit: func(loop *LoopDraft) { loop.Maximum = 0 },
			want: "maximum",
		},
		{
			name: "body has fixed inputs",
			edit: func(loop *LoopDraft) {
				loop.Body.Inputs = validationContract(t, validationRequired(t, "initial", value.EmptyContract().ObjectType()))
			},
			want: "body input",
		},
		{
			name: "termination resolves to boolean",
			edit: func(loop *LoopDraft) {
				output := validationContract(t, validationRequired(t, "done", value.String()))
				loop.Body = validationLoopBody(t, output)
				loop.Outputs = output
			},
			want: "termination",
		},
		{
			name: "outputs equal body outputs",
			edit: func(loop *LoopDraft) { loop.Outputs = value.EmptyContract() },
			want: "output contract",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop := validationLoop(t)
			tc.edit(&loop)
			_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{Name: "repeat", Loop: &loop}}}))
			assertValidationError(t, err, tc.want)
		})
	}
}

func TestCompileRejectsMalformedGate(t *testing.T) {
	passed := validationRequired(t, "passed", value.Boolean())
	for _, tc := range []struct {
		name string
		leaf LeafDraft
		want string
	}{
		{
			name: "inputs are fixed",
			leaf: LeafDraft{Kind: Gate, Inputs: validationContract(t, passed, validationRequired(t, "extra", value.String())), Outputs: value.EmptyContract()},
			want: "gate inputs",
		},
		{
			name: "outputs are empty",
			leaf: LeafDraft{Kind: Gate, Inputs: validationContract(t, passed, validationOptional(t, "reason", value.String())), Outputs: validationContract(t, validationRequired(t, "output", value.String()))},
			want: "gate outputs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf := tc.leaf
			literals := []LiteralBindingDraft{{Input: "passed", Value: validationLiteral(t, `true`)}}
			if _, found := leaf.Inputs.Resolve("extra"); found {
				literals = append(literals, LiteralBindingDraft{Input: "extra", Value: validationLiteral(t, `"extra"`)})
			}
			_, err := Compile(bindingProgram(GraphDraft{Nodes: []NodeDraft{{Name: "check", Leaf: &leaf, Literals: literals}}}))
			assertValidationError(t, err, tc.want)
		})
	}
}

func TestCompileRejectsMalformedFinally(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cleanup GraphDraft
		want    string
	}{
		{
			name:    "cleanup has no outputs",
			cleanup: validationCleanupWithOutput(t),
			want:    "cleanup output",
		},
		{
			name:    "cleanup cannot contain a gate",
			cleanup: GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "check", Leaf: &LeafDraft{Kind: Gate, Inputs: validationContract(t, validationRequired(t, "passed", value.Boolean())), Outputs: value.EmptyContract()}, Literals: []LiteralBindingDraft{{Input: "passed", Value: validationLiteral(t, `true`)}}}}},
			want:    "gate",
		},
		{
			name:    "cleanup cannot contain a loop below a graph",
			cleanup: GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "nested", Graph: &GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "repeat", Loop: validationLoopPointer(t)}}}}}},
			want:    "loop",
		},
		{
			name:    "cleanup cannot contain nested finally",
			cleanup: GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "nested", Graph: &GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Finally: &GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract()}}}}},
			want:    "finally",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := tc.cleanup
			_, err := Compile(bindingProgram(GraphDraft{Finally: &cleanup}))
			assertValidationError(t, err, tc.want)
		})
	}
}

// Cleanup itself cannot publish data, but its ordinary composed scopes may use
// their own explicit boundaries to pass internal data.
func TestCompileAllowsComposedCleanupWithInternalOutputs(t *testing.T) {
	text := validationContract(t, validationRequired(t, "text", value.String()))
	selected := validationContract(t, validationRequired(t, "selected", value.Boolean()))
	items := validationContract(t, validationRequired(t, "items", validationList(t, value.String())))
	copyGraph := GraphDraft{
		Inputs: text, Outputs: text,
		Nodes: []NodeDraft{bindingLeaf("copy", text, text)},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Boundary}, To: validationChild("copy"), Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			{From: validationChild("copy"), To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
		},
	}
	caseGraph := GraphDraft{
		Inputs: selected, Outputs: text,
		Nodes: []NodeDraft{bindingLeaf("value", value.EmptyContract(), text)},
		Edges: []EdgeDraft{{From: validationChild("value"), To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}}},
	}
	bodyOutputs := text
	mapped := MapDraft{
		Inputs: items, Outputs: validationMapResult(t, bodyOutputs), Collection: "items", Result: "result",
		Body: GraphDraft{
			Inputs: validationContract(t,
				validationRequired(t, "item", value.String()),
				validationRequired(t, "index", value.Integer()),
			),
			Outputs: bodyOutputs,
			Nodes:   []NodeDraft{bindingLeaf("copy", validationContract(t, validationRequired(t, "item", value.String())), bodyOutputs)},
			Edges: []EdgeDraft{
				{From: EndpointDraft{Kind: Boundary}, To: validationChild("copy"), Bindings: []BindingDraft{{From: []string{"item"}, To: "item"}}},
				{From: validationChild("copy"), To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			},
		},
	}
	cleanupInputs := validationContract(t,
		validationRequired(t, "items", validationList(t, value.String())),
		validationRequired(t, "selected", value.Boolean()),
		validationRequired(t, "text", value.String()),
	)
	cleanup := GraphDraft{
		Inputs: cleanupInputs, Outputs: value.EmptyContract(),
		Nodes: []NodeDraft{
			{Name: "inner", Graph: &copyGraph},
			{Name: "choose", Branch: &BranchDraft{Inputs: selected, Outputs: text, Selector: "selected", Cases: []CaseDraft{{Name: "false", Graph: caseGraph}, {Name: "true", Graph: caseGraph}}}},
			{Name: "each", Map: &mapped},
		},
		Edges: []EdgeDraft{
			{From: EndpointDraft{Kind: Boundary}, To: validationChild("inner"), Bindings: []BindingDraft{{From: []string{"text"}, To: "text"}}},
			{From: EndpointDraft{Kind: Boundary}, To: validationChild("choose"), Bindings: []BindingDraft{{From: []string{"selected"}, To: "selected"}}},
			{From: EndpointDraft{Kind: Boundary}, To: validationChild("each"), Bindings: []BindingDraft{{From: []string{"items"}, To: "items"}}},
		},
	}
	root := GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Finally: &cleanup}
	if _, err := Compile(bindingProgram(root)); err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompileRejectsUnconstructedGateAndCleanupOutputs(t *testing.T) {
	gateInputs := validationContract(t,
		validationRequired(t, "passed", value.Boolean()),
		validationOptional(t, "reason", value.String()),
	)
	for _, tc := range []struct {
		name  string
		draft ProgramDraft
		want  string
	}{
		{
			name: "gate output",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{module("root", GraphDraft{
				Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
				Nodes: []NodeDraft{{Name: "check", Leaf: &LeafDraft{Kind: Gate, Inputs: gateInputs}, Literals: []LiteralBindingDraft{{Input: "passed", Value: validationLiteral(t, `true`)}}}},
			})}},
			want: "leaf outputs: contract is invalid",
		},
		{
			name: "cleanup output",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{module("root", GraphDraft{
				Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Finally: &GraphDraft{Inputs: value.EmptyContract()},
			})}},
			want: "finally: graph outputs: contract is invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.draft)
			assertValidationError(t, err, tc.want)
		})
	}
}

func TestCompileRejectsUnconstructedContractsAtOtherBoundaries(t *testing.T) {
	validLoop := validationLoop(t)
	validLoop.Inputs = value.EmptyContract()
	for _, tc := range []struct {
		name  string
		draft ProgramDraft
		want  string
	}{
		{
			name:  "root graph input",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{{Name: "root", Graph: GraphDraft{Outputs: value.EmptyContract()}}}},
			want:  "graph inputs: contract is invalid",
		},
		{
			name: "leaf input",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{{Name: "root", Graph: GraphDraft{
				Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "work", Leaf: &LeafDraft{Kind: Script, Outputs: value.EmptyContract()}}},
			}}}},
			want: "leaf inputs: contract is invalid",
		},
		{
			name: "map body output",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{{Name: "root", Graph: GraphDraft{
				Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
				Nodes: []NodeDraft{{Name: "each", Map: &MapDraft{
					Inputs:  validationContract(t, validationRequired(t, "items", validationList(t, value.String()))),
					Outputs: validationMapResult(t, value.EmptyContract()), Collection: "items", Result: "result",
					Body: GraphDraft{Inputs: validationContract(t, validationRequired(t, "item", value.String()), validationRequired(t, "index", value.Integer()))},
				}, Literals: []LiteralBindingDraft{{Input: "items", Value: validationLiteral(t, `[]`)}}}},
			}}}},
			want: "body: graph outputs: contract is invalid",
		},
		{
			name: "loop input",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{{Name: "root", Graph: GraphDraft{
				Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "repeat", Loop: &LoopDraft{Outputs: validLoop.Outputs, Maximum: validLoop.Maximum, Termination: validLoop.Termination, Body: validLoop.Body}}},
			}}}},
			want: "loop: inputs: contract is invalid",
		},
		{
			name: "ordinary graph output",
			draft: ProgramDraft{Root: "root", Modules: []ModuleDraft{{Name: "root", Graph: GraphDraft{
				Inputs: value.EmptyContract(), Outputs: value.EmptyContract(), Nodes: []NodeDraft{{Name: "nested", Graph: &GraphDraft{Inputs: value.EmptyContract()}}},
			}}}},
			want: "graph outputs: contract is invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.draft)
			assertValidationError(t, err, tc.want)
		})
	}
}

func validationBranch(inputs value.Contract, cases []CaseDraft) BranchDraft {
	return BranchDraft{Inputs: inputs, Selector: "missing", Cases: cases}
}

func validationCase(name string, inputs value.Contract) CaseDraft {
	return CaseDraft{Name: name, Graph: GraphDraft{Inputs: inputs, Outputs: value.EmptyContract()}}
}

func validationLoop(t *testing.T) LoopDraft {
	t.Helper()
	output := validationContract(t, validationRequired(t, "done", value.Boolean()))
	return LoopDraft{Inputs: value.EmptyContract(), Outputs: output, Maximum: 2, Termination: []string{"done"}, Body: validationLoopBody(t, output)}
}

func validationLoopPointer(t *testing.T) *LoopDraft {
	t.Helper()
	loop := validationLoop(t)
	return &loop
}

func validationLoopBody(t *testing.T, outputs value.Contract) GraphDraft {
	t.Helper()
	inputs := validationContract(t,
		validationRequired(t, "initial", value.EmptyContract().ObjectType()),
		validationOptional(t, "previous", outputs.ObjectType()),
		validationRequired(t, "iteration", value.Integer()),
	)
	bindings := make([]BindingDraft, 0, len(outputs.Ports()))
	for _, field := range outputs.Ports() {
		bindings = append(bindings, BindingDraft{From: []string{field.Name()}, To: field.Name()})
	}
	return GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Nodes: []NodeDraft{bindingLeaf("work", value.EmptyContract(), outputs)},
		Edges: []EdgeDraft{{From: validationChild("work"), To: EndpointDraft{Kind: Boundary}, Bindings: bindings}},
	}
}

func validationCleanupWithOutput(t *testing.T) GraphDraft {
	t.Helper()
	output := validationContract(t, validationRequired(t, "result", value.String()))
	return GraphDraft{
		Inputs:  value.EmptyContract(),
		Outputs: output,
		Nodes:   []NodeDraft{bindingLeaf("clean", value.EmptyContract(), output)},
		Edges:   []EdgeDraft{{From: validationChild("clean"), To: EndpointDraft{Kind: Boundary}, Bindings: []BindingDraft{{From: []string{"result"}, To: "result"}}}},
	}
}

func validationMapResult(t *testing.T, bodyOutputs value.Contract) value.Contract {
	t.Helper()
	list := validationList(t, bodyOutputs.ObjectType())
	return validationContract(t, validationRequired(t, "result", list))
}

func validationEdge(from, to, port string) EdgeDraft {
	return EdgeDraft{From: validationChild(from), To: validationChild(to), Bindings: []BindingDraft{{From: []string{port}, To: port}}}
}

func validationChild(name string) EndpointDraft { return EndpointDraft{Kind: Child, Child: name} }

func validationRequired(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func validationOptional(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Optional(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func validationContract(t *testing.T, ports ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(ports...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func validationList(t *testing.T, element value.Type) value.Type {
	t.Helper()
	list, err := value.List(element)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func validationEnum(t *testing.T, members ...string) value.Type {
	t.Helper()
	values := make([]value.Literal, 0, len(members))
	for _, member := range members {
		values = append(values, validationLiteral(t, member))
	}
	enum, err := value.Enum(values...)
	if err != nil {
		t.Fatal(err)
	}
	return enum
}

func validationLiteral(t *testing.T, source string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return literal
}

func assertValidationError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("Compile() succeeded, want validation error")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile() error = %q, want %q", err, want)
	}
}
