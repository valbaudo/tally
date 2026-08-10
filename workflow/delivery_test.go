package workflow

import (
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

func TestCompileWorkspaceDeclarations(t *testing.T) {
	tree := value.Tree()
	file := deliveryFile(t, "text/plain")
	inputs := deliveryContract(t,
		deliveryRequired(t, "source", tree),
		deliveryRequired(t, "notes", file),
	)
	outputs := deliveryContract(t,
		deliveryRequired(t, "continued", tree),
		deliveryRequired(t, "report", file),
	)

	for _, kind := range []LeafKind{Agent, Script} {
		t.Run(string(kind)+" explicit base and publication", func(t *testing.T) {
			leaf := deliveryCompileLeaf(t, LeafDraft{
				Kind: kind, Inputs: inputs, Outputs: outputs,
				BaseTree: []string{"source"}, PublishWorkspace: []string{"continued"},
			})
			if got, want := leaf.BaseTree(), []string{"source"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("BaseTree() = %v, want %v", got, want)
			}
			if got, want := leaf.PublishWorkspace(), []string{"continued"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("PublishWorkspace() = %v, want %v", got, want)
			}
		})

		t.Run(string(kind)+" automatic named slots and empty workspace", func(t *testing.T) {
			leaf := deliveryCompileLeaf(t, LeafDraft{Kind: kind, Inputs: inputs, Outputs: outputs})
			if leaf.BaseTree() != nil || leaf.PublishWorkspace() != nil {
				t.Fatalf("undeclared workspace intent = base %v publication %v", leaf.BaseTree(), leaf.PublishWorkspace())
			}
		})
	}
}

func TestCompileWorkspacePathsTreatSegmentsAsExactStrings(t *testing.T) {
	fieldName := "../source/with.dot\x00"
	outputName := "capture\\not-a-path"
	inputs := deliveryContract(t, deliveryRequired(t, fieldName, value.Tree()))
	outputs := deliveryContract(t, deliveryRequired(t, outputName, value.Tree()))
	leaf := deliveryCompileLeaf(t, LeafDraft{
		Kind: Script, Inputs: inputs, Outputs: outputs,
		BaseTree: []string{fieldName}, PublishWorkspace: []string{outputName},
	})
	if got := leaf.BaseTree(); len(got) != 1 || got[0] != fieldName {
		t.Fatalf("BaseTree() = %q, want exact field name %q", got, fieldName)
	}
	if got := leaf.PublishWorkspace(); len(got) != 1 || got[0] != outputName {
		t.Fatalf("PublishWorkspace() = %q, want exact field name %q", got, outputName)
	}
}

func TestCompileRejectsInvalidWorkspaceDeclarations(t *testing.T) {
	file := deliveryFile(t, "text/plain")
	requiredTree := deliveryContract(t, deliveryRequired(t, "source", value.Tree()))
	requiredTreeOutput := deliveryContract(t, deliveryRequired(t, "continued", value.Tree()))
	optionalTree := deliveryContract(t, deliveryOptional(t, "source", value.Tree()))
	optionalAncestor := deliveryObject(t, deliveryOptional(t, "nested", deliveryObject(t, deliveryRequired(t, "source", value.Tree()))))
	optionalSelected := deliveryObject(t, deliveryRequired(t, "nested", deliveryObject(t, deliveryOptional(t, "source", value.Tree()))))

	tests := []struct {
		name string
		leaf LeafDraft
		want string
	}{
		{"base missing", LeafDraft{Kind: Agent, Inputs: requiredTree, Outputs: value.EmptyContract(), BaseTree: []string{"missing"}}, "base tree"},
		{"base is file", LeafDraft{Kind: Agent, Inputs: deliveryContract(t, deliveryRequired(t, "source", file)), Outputs: value.EmptyContract(), BaseTree: []string{"source"}}, "tree input"},
		{"base top level optional", LeafDraft{Kind: Agent, Inputs: optionalTree, Outputs: value.EmptyContract(), BaseTree: []string{"source"}}, "required"},
		{"base ancestor optional", LeafDraft{Kind: Agent, Inputs: deliveryContract(t, deliveryRequired(t, "payload", optionalAncestor)), Outputs: value.EmptyContract(), BaseTree: []string{"payload", "nested", "source"}}, "required"},
		{"base selected field optional", LeafDraft{Kind: Agent, Inputs: deliveryContract(t, deliveryRequired(t, "payload", optionalSelected)), Outputs: value.EmptyContract(), BaseTree: []string{"payload", "nested", "source"}}, "required"},
		{"publication missing", LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: requiredTreeOutput, PublishWorkspace: []string{"missing"}}, "workspace publication"},
		{"publication is file", LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: deliveryContract(t, deliveryRequired(t, "continued", file)), PublishWorkspace: []string{"continued"}}, "tree output"},
		{"publication optional", LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: deliveryContract(t, deliveryOptional(t, "continued", value.Tree())), PublishWorkspace: []string{"continued"}}, "required"},
		{"llm base", LeafDraft{Kind: LLM, Inputs: requiredTree, Outputs: value.EmptyContract(), BaseTree: []string{"source"}}, "workspace"},
		{"llm publication", LeafDraft{Kind: LLM, Inputs: value.EmptyContract(), Outputs: requiredTreeOutput, PublishWorkspace: []string{"continued"}}, "workspace"},
		{"gate base", LeafDraft{Kind: Gate, Inputs: deliveryGateInputs(t), Outputs: value.EmptyContract(), BaseTree: []string{"passed"}}, "workspace"},
		{"gate publication", LeafDraft{Kind: Gate, Inputs: deliveryGateInputs(t), Outputs: value.EmptyContract(), PublishWorkspace: []string{"continued"}}, "workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := deliveryCompileLeafError(tc.leaf)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("Compile() error = %v, want text %q", err, tc.want)
			}
		})
	}
}

func TestCompileAttachmentDeclarations(t *testing.T) {
	file := deliveryFile(t, "application/pdf")
	page := deliveryObject(t, deliveryRequired(t, "page", file))
	pages := deliveryList(t, page)
	filesByName := deliveryMap(t, deliveryFile(t, "application/octet-stream"))
	inputs := deliveryContract(t,
		deliveryRequired(t, "document", file),
		deliveryRequired(t, "pages", pages),
		deliveryRequired(t, "files", filesByName),
	)
	leaf := deliveryCompileLeaf(t, LeafDraft{
		Kind: LLM, Inputs: inputs, Outputs: value.EmptyContract(),
		Attachments: []AttachmentDraft{
			{Input: []string{"pages"}, Fidelity: TextFidelity},
			{Input: []string{"document"}, Fidelity: VisualFidelity},
			{Input: []string{"files"}, Fidelity: TextFidelity},
		},
	})
	attachments := leaf.Attachments()
	if len(attachments) != 3 {
		t.Fatalf("Attachments() length = %d, want 3", len(attachments))
	}
	wantPaths := [][]string{{"document"}, {"files"}, {"pages"}}
	wantFidelity := []Fidelity{VisualFidelity, TextFidelity, TextFidelity}
	for index, attachment := range attachments {
		if !reflect.DeepEqual(attachment.Input(), wantPaths[index]) || attachment.Fidelity() != wantFidelity[index] {
			t.Fatalf("attachment %d = (%v, %v), want (%v, %v)", index, attachment.Input(), attachment.Fidelity(), wantPaths[index], wantFidelity[index])
		}
	}
}

func TestCompileFidelityDefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct {
		media string
		want  Fidelity
	}{
		{"application/pdf", VisualFidelity},
		{"application/pdf; version=1.7", VisualFidelity},
		{"image/png", VisualFidelity},
		{"image/svg+xml", VisualFidelity},
		{"text/plain", TextFidelity},
		{"text/html; charset=utf-8", TextFidelity},
		{"application/json", TextFidelity},
		{"audio/mpeg", TextFidelity},
	} {
		t.Run(tc.media, func(t *testing.T) {
			if got := DefaultFidelity(tc.media); got != tc.want {
				t.Fatalf("DefaultFidelity(%q) = %v, want %v", tc.media, got, tc.want)
			}
		})
	}

	inputs := deliveryContract(t, deliveryRequired(t, "document", deliveryFile(t, "application/pdf")))
	leaf := deliveryCompileLeaf(t, LeafDraft{
		Kind: LLM, Inputs: inputs, Outputs: value.EmptyContract(),
		Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: TextFidelity}},
	})
	if got := leaf.Attachments()[0].Fidelity(); got != TextFidelity {
		t.Fatalf("declared PDF override = %v, want TextFidelity", got)
	}
}

func TestCompileAttachmentPathsTreatSegmentsAsExactStrings(t *testing.T) {
	name := "../document/with.dot\x00"
	inputs := deliveryContract(t, deliveryRequired(t, name, deliveryMap(t, deliveryFile(t, "text/plain"))))
	leaf := deliveryCompileLeaf(t, LeafDraft{
		Kind: LLM, Inputs: inputs, Outputs: value.EmptyContract(),
		Attachments: []AttachmentDraft{{Input: []string{name}, Fidelity: TextFidelity}},
	})
	if got := leaf.Attachments()[0].Input(); len(got) != 1 || got[0] != name {
		t.Fatalf("attachment path = %q, want exact schema segment %q", got, name)
	}
}

func TestCompileAttachmentDuplicatesAndNonconflictingOverlaps(t *testing.T) {
	file := deliveryFile(t, "text/plain")
	payload := deliveryObject(t,
		deliveryRequired(t, "document", file),
		deliveryRequired(t, "thumbnail", file),
	)
	inputs := deliveryContract(t, deliveryRequired(t, "payload", payload))
	leaf := deliveryCompileLeaf(t, LeafDraft{
		Kind: LLM, Inputs: inputs, Outputs: value.EmptyContract(),
		Attachments: []AttachmentDraft{
			{Input: []string{"payload", "document"}, Fidelity: TextFidelity},
			{Input: []string{"payload"}, Fidelity: TextFidelity},
			{Input: []string{"payload", "document"}, Fidelity: TextFidelity},
		},
	})
	if got := len(leaf.Attachments()); got != 2 {
		t.Fatalf("canonical attachment declarations = %d, want 2", got)
	}

	leaf = deliveryCompileLeaf(t, LeafDraft{
		Kind: LLM, Inputs: inputs, Outputs: value.EmptyContract(),
		Attachments: []AttachmentDraft{
			{Input: []string{"payload", "document"}, Fidelity: VisualFidelity},
			{Input: []string{"payload", "thumbnail"}, Fidelity: TextFidelity},
		},
	})
	if got := len(leaf.Attachments()); got != 2 {
		t.Fatalf("disjoint attachment declarations = %d, want 2", got)
	}
}

func TestCompileRejectsInvalidAttachmentDeclarations(t *testing.T) {
	file := deliveryFile(t, "text/plain")
	fileInputs := deliveryContract(t, deliveryRequired(t, "document", file))
	payload := deliveryObject(t, deliveryRequired(t, "document", file))
	payloadInputs := deliveryContract(t, deliveryRequired(t, "payload", payload))

	tests := []struct {
		name string
		leaf LeafDraft
		want string
	}{
		{"agent", LeafDraft{Kind: Agent, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: TextFidelity}}}, "attachment"},
		{"script", LeafDraft{Kind: Script, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: TextFidelity}}}, "attachment"},
		{"gate", LeafDraft{Kind: Gate, Inputs: deliveryGateInputs(t), Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"passed"}, Fidelity: TextFidelity}}}, "attachment"},
		{"unknown path", LeafDraft{Kind: LLM, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"missing"}, Fidelity: TextFidelity}}}, "file"},
		{"empty path", LeafDraft{Kind: LLM, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Fidelity: TextFidelity}}}, "file"},
		{"scalar subtree", LeafDraft{Kind: LLM, Inputs: deliveryContract(t, deliveryRequired(t, "text", value.String())), Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"text"}, Fidelity: TextFidelity}}}, "file"},
		{"tree subtree", LeafDraft{Kind: LLM, Inputs: deliveryContract(t, deliveryRequired(t, "source", value.Tree())), Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"source"}, Fidelity: TextFidelity}}}, "file"},
		{"zero fidelity", LeafDraft{Kind: LLM, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: Fidelity(0)}}}, "fidelity"},
		{"unknown fidelity", LeafDraft{Kind: LLM, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: Fidelity(3)}}}, "fidelity"},
		{"duplicate conflict", LeafDraft{Kind: LLM, Inputs: fileInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: TextFidelity}, {Input: []string{"document"}, Fidelity: VisualFidelity}}}, "conflict"},
		{"overlap conflict", LeafDraft{Kind: LLM, Inputs: payloadInputs, Outputs: value.EmptyContract(), Attachments: []AttachmentDraft{{Input: []string{"payload"}, Fidelity: TextFidelity}, {Input: []string{"payload", "document"}, Fidelity: VisualFidelity}}}, "conflict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := deliveryCompileLeafError(tc.leaf)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("Compile() error = %v, want text %q", err, tc.want)
			}
		})
	}
}

func TestCompileRejectsRawLLMTreeInputAtAnyDepth(t *testing.T) {
	treeObject := deliveryObject(t, deliveryRequired(t, "source", value.Tree()))
	tests := []struct {
		name string
		typ  value.Type
	}{
		{"direct", value.Tree()},
		{"object", treeObject},
		{"list", deliveryList(t, value.Tree())},
		{"map", deliveryMap(t, treeObject)},
		{"list map object", deliveryList(t, deliveryMap(t, treeObject))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inputs := deliveryContract(t, deliveryRequired(t, "input", tc.typ))
			_, err := deliveryCompileLeafError(LeafDraft{Kind: LLM, Inputs: inputs, Outputs: value.EmptyContract()})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "tree") {
				t.Fatalf("Compile() error = %v, want raw-LLM tree rejection", err)
			}
		})
	}
}

func TestCompileDeliveryDeepRawLLMTreeInputDoesNotOverflowStack(t *testing.T) {
	const childCase = "DAWN_DEEP_DELIVERY_TYPE_CASE"
	if os.Getenv(childCase) != "" {
		typ := value.Tree()
		for range 2048 {
			typ = deliveryList(t, typ)
		}
		inputs := deliveryContract(t, deliveryRequired(t, "input", typ))
		outputs := value.EmptyContract()
		debug.SetMaxStack(64 << 10)
		result := make(chan error, 1)
		go func() {
			_, err := compileDelivery(LeafDraft{Kind: LLM}, inputs, outputs)
			result <- err
		}()
		err := <-result
		if err == nil || !strings.Contains(err.Error(), "tree") {
			t.Fatalf("compileDelivery() error = %v, want ordinary raw-LLM tree rejection", err)
		}
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestCompileDeliveryDeepRawLLMTreeInputDoesNotOverflowStack$")
	command.Env = append(os.Environ(), childCase+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("deep delivery validation did not return an ordinary result: %v\n%s", err, output)
	}
}

func TestCompileAttachmentAllowsRawLLMTreeOutput(t *testing.T) {
	outputs := deliveryContract(t, deliveryRequired(t, "tree", value.Tree()))
	deliveryCompileLeaf(t, LeafDraft{Kind: LLM, Inputs: value.EmptyContract(), Outputs: outputs})
}

func deliveryCompileLeaf(t *testing.T, leaf LeafDraft) Leaf {
	t.Helper()
	definition, err := deliveryCompileLeafError(leaf)
	if err != nil {
		t.Fatal(err)
	}
	compiled, ok := mustNode(t, definition.Root(), "delivery").Leaf()
	if !ok {
		t.Fatal("compiled node is not a leaf")
	}
	return compiled
}

func deliveryCompileLeafError(leaf LeafDraft) (Definition, error) {
	graph := GraphDraft{
		Inputs: leaf.Inputs, Outputs: leaf.Outputs,
		Nodes: []NodeDraft{{Name: "delivery", Leaf: &leaf}},
	}
	if ports := leaf.Inputs.Ports(); len(ports) > 0 {
		bindings := make([]BindingDraft, len(ports))
		for index, port := range ports {
			bindings[index] = BindingDraft{From: []string{port.Name()}, To: port.Name()}
		}
		graph.Edges = append(graph.Edges, EdgeDraft{
			From: EndpointDraft{Kind: Boundary}, To: EndpointDraft{Kind: Child, Child: "delivery"}, Bindings: bindings,
		})
	}
	if ports := leaf.Outputs.Ports(); len(ports) > 0 {
		bindings := make([]BindingDraft, len(ports))
		for index, port := range ports {
			bindings[index] = BindingDraft{From: []string{port.Name()}, To: port.Name()}
		}
		graph.Edges = append(graph.Edges, EdgeDraft{
			From: EndpointDraft{Kind: Child, Child: "delivery"}, To: EndpointDraft{Kind: Boundary}, Bindings: bindings,
		})
	}
	return Compile(ProgramDraft{Root: "root", Modules: []ModuleDraft{module("root", graph)}})
}

func deliveryGateInputs(t *testing.T) value.Contract {
	t.Helper()
	return deliveryContract(t,
		deliveryRequired(t, "passed", value.Boolean()),
		deliveryOptional(t, "reason", value.String()),
	)
}

func deliveryFile(t *testing.T, media ...string) value.Type {
	t.Helper()
	typ, err := value.File(media...)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func deliveryList(t *testing.T, element value.Type) value.Type {
	t.Helper()
	typ, err := value.List(element)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func deliveryMap(t *testing.T, element value.Type) value.Type {
	t.Helper()
	typ, err := value.Map(element)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func deliveryObject(t *testing.T, fields ...value.Field) value.Type {
	t.Helper()
	typ, err := value.Object(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func deliveryRequired(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func deliveryOptional(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Optional(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func deliveryContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}
