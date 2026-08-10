package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"testing"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

const (
	finiteExecutionCase = "DAWN_STRUCTURED_CONTROL_FINITE_EXECUTION_CASE"
	finiteDepth         = 50_000
	finiteMapItems      = 50_000
)

func TestStructuredControlFiniteExecutionSubprocess(t *testing.T) {
	if childCase := os.Getenv(finiteExecutionCase); childCase != "" {
		debug.SetMaxStack(1 << 20)
		switch childCase {
		case "depth":
			runFiniteDepthCase(t)
		case "map":
			runFiniteMapCase(t)
		default:
			t.Fatalf("unknown finite execution case %q", childCase)
		}
		return
	}

	for _, childCase := range []string{"depth", "map"} {
		t.Run(childCase, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestStructuredControlFiniteExecutionSubprocess$", "-test.count=1")
			command.Env = append(os.Environ(), finiteExecutionCase+"="+childCase)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("finite %s compilation/execution did not return ordinarily: %v\n%s", childCase, err, output)
			}
		})
	}
}

func runFiniteDepthCase(t *testing.T) {
	t.Helper()
	definition := compileFiniteDepthDefinition(t)
	input := testValue(t, map[string]value.Value{"selected": value.NewBoolean(true)})
	result := runFiniteDefinition(t, definition, input)
	requireStatus(t, result, Succeeded)
	output, ok := result.Output()
	if !ok || !output.Equal(emptyValue(t)) {
		t.Fatalf("finite depth output = %x/%v, want exact empty object", output.Canonical(), ok)
	}
}

func compileFiniteDepthDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	empty := value.EmptyContract()
	selector := testContract(t, testRequired(t, "selected", value.Boolean()))
	gateInputs := testContract(t,
		testRequired(t, "passed", value.Boolean()),
		testOptional(t, "reason", value.String()),
	)
	trueLiteral := branchLiteral(t, "true")
	current := workflow.GraphDraft{
		Inputs: selector, Outputs: empty,
		Nodes: []workflow.NodeDraft{{
			Name:     "terminal-gate",
			Leaf:     &workflow.LeafDraft{Kind: workflow.Gate, Inputs: gateInputs, Outputs: empty},
			Literals: []workflow.LiteralBindingDraft{{Input: "passed", Value: trueLiteral}},
		}},
	}

	for level := 0; level < finiteDepth; level++ {
		if level%2 == 0 {
			nested := current
			current = workflow.GraphDraft{
				Inputs: selector, Outputs: empty,
				Nodes: []workflow.NodeDraft{{Name: "nested-graph", Graph: &nested}},
				Edges: []workflow.EdgeDraft{prestigeEdge(workflow.Boundary, "", workflow.Child, "nested-graph",
					workflow.BindingDraft{From: []string{"selected"}, To: "selected"})},
			}
			continue
		}

		selected := current
		current = workflow.GraphDraft{
			Inputs: selector, Outputs: empty,
			Nodes: []workflow.NodeDraft{{
				Name: "nested-branch",
				Branch: &workflow.BranchDraft{
					Inputs: selector, Outputs: empty, Selector: "selected",
					Cases: []workflow.CaseDraft{
						{Name: "false", Graph: workflow.GraphDraft{Inputs: selector, Outputs: empty}},
						{Name: "true", Graph: selected},
					},
				},
			}},
			Edges: []workflow.EdgeDraft{prestigeEdge(workflow.Boundary, "", workflow.Child, "nested-branch",
				workflow.BindingDraft{From: []string{"selected"}, To: "selected"})},
		}
	}

	definition, err := workflow.Compile(workflow.ProgramDraft{
		Root: "root", Modules: []workflow.ModuleDraft{{Name: "root", Graph: current}},
	})
	if err != nil {
		t.Fatalf("compile finite depth definition: %v", err)
	}
	return definition
}

func runFiniteMapCase(t *testing.T) {
	t.Helper()
	empty := value.EmptyContract()
	bodyInputs := mapBodyInputs(t, value.String())
	definition := mapDefinition(t, value.String(), empty, workflow.GraphDraft{Inputs: bodyInputs, Outputs: empty})
	items := make([]value.Value, finiteMapItems)
	for index := range items {
		items[index] = value.NewString(fmt.Sprintf("item-%05d", index))
	}
	result := runFiniteDefinition(t, definition, testValue(t, map[string]value.Value{
		"items": value.NewList(items...),
	}))
	requireStatus(t, result, Succeeded)
	output, ok := result.Output()
	if !ok {
		t.Fatal("finite map omitted its exact contracted output")
	}
	results, present := selectValue(output, []string{"results"})
	if !present || results.Kind() != value.ListKind {
		t.Fatalf("finite map results = %x/%v, want list", results.Canonical(), present)
	}
	items = results.Items()
	if len(items) != finiteMapItems {
		t.Fatalf("finite map result count = %d, want %d", len(items), finiteMapItems)
	}
	wantItem := emptyValue(t)
	for index, item := range items {
		if !item.Equal(wantItem) {
			t.Fatalf("finite map result %d = %x, want exact empty object", index, item.Canonical())
		}
	}
}

func runFiniteDefinition(t *testing.T, definition workflow.Definition, input value.Value) Result {
	t.Helper()
	scheduler, err := New(forbiddenFiniteRunner{}, discardFiniteBoundary{}, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(context.Background(), definition, input)
	if err != nil {
		t.Fatal(err)
	}
	return execution.Wait()
}

type forbiddenFiniteRunner struct{}

func (forbiddenFiniteRunner) Start(context.Context, LeafRequest) (LeafExecution, error) {
	return nil, errors.New("finite empty-body proof unexpectedly invoked an external leaf")
}

type discardFiniteBoundary struct{}

func (discardFiniteBoundary) Enter(context.Context, Instance) error { return nil }

func (discardFiniteBoundary) Commit(context.Context, Instance, value.Value) error { return nil }

func (discardFiniteBoundary) Settle(context.Context, Instance, Result) error { return nil }
