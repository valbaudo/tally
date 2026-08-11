package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
		case "dependency":
			runFiniteDependencyCase(t)
		case "compile-worklist":
			runFiniteCompileWorklistCase(t)
		case "graph-chain":
			runFiniteGraphChainCase(t)
		default:
			t.Fatalf("unknown finite execution case %q", childCase)
		}
		return
	}

	for _, childCase := range []string{"depth", "map", "dependency", "compile-worklist", "graph-chain"} {
		t.Run(childCase, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestStructuredControlFiniteExecutionSubprocess$", "-test.count=1")
			command.Env = append(os.Environ(), finiteExecutionCase+"="+childCase)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("finite %s compilation/execution did not return ordinarily: %v\n%s", childCase, err, output)
			}
		})
	}
}

func runFiniteDependencyCase(t *testing.T) {
	t.Helper()
	definition := compileFiniteDependencyDefinition(t)
	if len(definition.Canonical()) == 0 {
		t.Fatal("finite dependency definition omitted its canonical form")
	}
}

func runFiniteGraphChainCase(t *testing.T) {
	t.Helper()
	definition := compileFiniteDependencyDefinition(t)
	result := runFiniteDefinition(t, definition, emptyValue(t))
	requireStatus(t, result, Succeeded)
	output, ok := result.Output()
	if !ok || !output.Equal(emptyValue(t)) {
		t.Fatalf("finite graph-chain output = %x/%v, want exact empty object", output.Canonical(), ok)
	}
}

func compileFiniteDependencyDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	empty := value.EmptyContract()
	gateInputs := testContract(t,
		testRequired(t, "passed", value.Boolean()),
		testOptional(t, "reason", value.String()),
	)
	passed := branchLiteral(t, "true")
	nodes := make([]workflow.NodeDraft, finiteDepth)
	edges := make([]workflow.EdgeDraft, 0, finiteDepth-1)
	for index := range nodes {
		name := fmt.Sprintf("gate-%05d", index)
		nodes[index] = workflow.NodeDraft{
			Name:     name,
			Leaf:     &workflow.LeafDraft{Kind: workflow.Gate, Inputs: gateInputs, Outputs: empty},
			Literals: []workflow.LiteralBindingDraft{{Input: "passed", Value: passed}},
		}
		if index != 0 {
			edges = append(edges, prestigeEdge(workflow.Child, fmt.Sprintf("gate-%05d", index-1), workflow.Child, name))
		}
	}
	definition, err := workflow.Compile(workflow.ProgramDraft{
		Root: "root", Modules: []workflow.ModuleDraft{{
			Name: "root", Graph: workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: nodes, Edges: edges},
		}},
	})
	if err != nil {
		t.Fatalf("compile finite dependency chain: %v", err)
	}
	return definition
}

func runFiniteCompileWorklistCase(t *testing.T) {
	t.Helper()
	program := finiteDepthProgram(t)
	baseline := runtime.NumGoroutine()
	stop := make(chan struct{})
	ready := make(chan struct{})
	observed := make(chan int, 1)
	go func() {
		peak := runtime.NumGoroutine()
		close(ready)
		for {
			select {
			case <-stop:
				observed <- peak
				return
			default:
				if current := runtime.NumGoroutine(); current > peak {
					peak = current
				}
				runtime.Gosched()
			}
		}
	}()
	<-ready
	definition, err := workflow.Compile(program)
	close(stop)
	peak := <-observed
	if err != nil {
		t.Fatalf("compile finite-depth worklist: %v", err)
	}
	if len(definition.Canonical()) == 0 {
		t.Fatal("finite-depth worklist definition omitted its canonical form")
	}
	if peak > baseline+8 {
		t.Fatalf("finite-depth compilation used %d concurrent goroutines above baseline, want at most 8", peak-baseline)
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
	definition, err := workflow.Compile(finiteDepthProgram(t))
	if err != nil {
		t.Fatalf("compile finite depth definition: %v", err)
	}
	return definition
}

func finiteDepthProgram(t *testing.T) workflow.ProgramDraft {
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

	return workflow.ProgramDraft{
		Root: "root", Modules: []workflow.ModuleDraft{{Name: "root", Graph: current}},
	}
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
