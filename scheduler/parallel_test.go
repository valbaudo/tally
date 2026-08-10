package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func TestParallelUsesOrdinaryGraphConcurrencyBarrierAndExplicitOutputs(t *testing.T) {
	alphaOutput := testContract(t,
		testRequired(t, "chosen", value.String()),
		testRequired(t, "unbound", value.String()),
	)
	betaOutput := testContract(t, testRequired(t, "ignored", value.String()))
	parallelOutput := testContract(t, testRequired(t, "answer", value.String()))
	rootOutput := testContract(t, testRequired(t, "result", value.String()))
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: value.EmptyContract(), Outputs: rootOutput,
		Nodes: []workflow.NodeDraft{{
			Name: "review",
			Parallel: &workflow.ParallelDraft{Graph: workflow.GraphDraft{
				Inputs: value.EmptyContract(), Outputs: parallelOutput,
				Nodes: []workflow.NodeDraft{
					testLeaf("alpha", value.EmptyContract(), alphaOutput),
					testLeaf("beta", value.EmptyContract(), betaOutput),
				},
				Edges: []workflow.EdgeDraft{{
					From: workflow.EndpointDraft{Kind: workflow.Child, Child: "alpha"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
					Bindings: []workflow.BindingDraft{{From: []string{"chosen"}, To: "answer"}},
				}},
			}},
		}},
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Child, Child: "review"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: []workflow.BindingDraft{{From: []string{"answer"}, To: "result"}},
		}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newPathRecordingBoundary(trace)
	execution := startPathExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2})

	if got := sortedStrings(runner.waitStarts(t, 2)); got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("parallel starts = %v, want both named branches", got)
	}
	if runner.maximum() != 2 {
		t.Fatalf("maximum active parallel branches = %d, want overlap of 2", runner.maximum())
	}
	alphaValue := testValue(t, map[string]value.Value{
		"chosen":  value.NewString("explicit"),
		"unbound": value.NewString("must not merge"),
	})
	runner.execution(t, "alpha").complete(mustLeafSuccess(t, alphaValue))

	waited := make(chan Result, 1)
	go func() { waited <- execution.Wait() }()
	select {
	case result := <-waited:
		t.Fatalf("parallel completed before beta quiesced: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	parallelPath := Path{}.AuthoredChild("review")
	if hasPathEvent(boundary.snapshot(), traceCommit, parallelPath) {
		t.Fatal("parallel committed before every named branch succeeded")
	}

	betaValue := testValue(t, map[string]value.Value{"ignored": value.NewString("also must not merge")})
	runner.execution(t, "beta").complete(mustLeafSuccess(t, betaValue))
	var result Result
	select {
	case result = <-waited:
	case <-time.After(testTimeout):
		t.Fatal("parallel did not finish after both branches quiesced")
	}
	requireStatus(t, result, Succeeded)
	wantRoot := testValue(t, map[string]value.Value{"result": value.NewString("explicit")})
	output, _ := result.Output()
	if !output.Equal(wantRoot) {
		t.Fatalf("root output = %x, want only explicit binding %x", output.Canonical(), wantRoot.Canonical())
	}
	wantParallel := testValue(t, map[string]value.Value{"answer": value.NewString("explicit")})
	parallelCommit, ok := committedAt(boundary.snapshot(), parallelPath)
	if !ok || !parallelCommit.Equal(wantParallel) {
		t.Fatalf("parallel output = %x/%v, want only explicit binding %x", parallelCommit.Canonical(), ok, wantParallel.Canonical())
	}
}

func TestParallelRetainsOrdinaryDependenciesAtRuntime(t *testing.T) {
	payload := testContract(t, testRequired(t, "payload", value.String()))
	selected := testContract(t, testRequired(t, "selected", value.String()))
	consumerOutput := testContract(t, testRequired(t, "answer", value.String()))
	rootOutput := testContract(t, testRequired(t, "result", value.String()))
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: value.EmptyContract(), Outputs: rootOutput,
		Nodes: []workflow.NodeDraft{{
			Name: "review",
			Parallel: &workflow.ParallelDraft{Graph: workflow.GraphDraft{
				Inputs: value.EmptyContract(), Outputs: consumerOutput,
				Nodes: []workflow.NodeDraft{
					testLeaf("producer", value.EmptyContract(), payload),
					testLeaf("consumer", selected, consumerOutput),
					testLeaf("after", value.EmptyContract(), value.EmptyContract()),
					testLeaf("independent", value.EmptyContract(), value.EmptyContract()),
				},
				Edges: []workflow.EdgeDraft{
					{
						From: workflow.EndpointDraft{Kind: workflow.Child, Child: "producer"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "consumer"},
						Bindings: []workflow.BindingDraft{{From: []string{"payload"}, To: "selected"}},
					},
					{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "consumer"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "after"}},
					{
						From: workflow.EndpointDraft{Kind: workflow.Child, Child: "consumer"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
						Bindings: []workflow.BindingDraft{{From: []string{"answer"}, To: "answer"}},
					},
				},
			}},
		}},
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Child, Child: "review"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: []workflow.BindingDraft{{From: []string{"answer"}, To: "result"}},
		}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newPathRecordingBoundary(trace)
	execution := startPathExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 4})

	if got := sortedStrings(runner.waitStarts(t, 2)); got[0] != "independent" || got[1] != "producer" {
		t.Fatalf("initial starts = %v, want producer and independent", got)
	}
	if runner.maximum() != 2 {
		t.Fatalf("initial maximum active = %d, want independent overlap", runner.maximum())
	}
	assertNoStart(t, runner, 25*time.Millisecond)

	wantPayload := value.NewString("exact bound data")
	producerOutput := testValue(t, map[string]value.Value{"payload": wantPayload})
	runner.execution(t, "producer").complete(mustLeafSuccess(t, producerOutput))
	if got := runner.waitStarts(t, 1); len(got) != 1 || got[0] != "consumer" {
		t.Fatalf("start after producer = %v, want consumer", got)
	}
	wantConsumerInput := testValue(t, map[string]value.Value{"selected": wantPayload})
	if got := runnerInput(t, runner, "consumer"); !got.Equal(wantConsumerInput) {
		t.Fatalf("consumer input = %x, want exact binding %x", got.Canonical(), wantConsumerInput.Canonical())
	}
	assertNoStart(t, runner, 25*time.Millisecond)

	wantConsumerOutput := testValue(t, map[string]value.Value{"answer": value.NewString("bound result")})
	runner.execution(t, "consumer").complete(mustLeafSuccess(t, wantConsumerOutput))
	if got := runner.waitStarts(t, 1); len(got) != 1 || got[0] != "after" {
		t.Fatalf("start after consumer = %v, want completion-dependent child", got)
	}
	runner.execution(t, "after").complete(mustLeafSuccess(t, emptyValue(t)))

	waited := make(chan Result, 1)
	go func() { waited <- execution.Wait() }()
	select {
	case result := <-waited:
		t.Fatalf("parallel barrier returned before independent work quiesced: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	parallelPath := Path{}.AuthoredChild("review")
	if hasPathEvent(boundary.snapshot(), traceCommit, parallelPath) {
		t.Fatal("parallel committed before all ordinary graph children succeeded")
	}

	runner.execution(t, "independent").complete(mustLeafSuccess(t, emptyValue(t)))
	var result Result
	select {
	case result = <-waited:
	case <-time.After(testTimeout):
		t.Fatal("parallel did not finish after every child quiesced")
	}
	requireStatus(t, result, Succeeded)
	wantRoot := testValue(t, map[string]value.Value{"result": value.NewString("bound result")})
	output, _ := result.Output()
	if !output.Equal(wantRoot) {
		t.Fatalf("root output = %x, want %x", output.Canonical(), wantRoot.Canonical())
	}
	wantParallel := testValue(t, map[string]value.Value{"answer": value.NewString("bound result")})
	parallelCommit, ok := committedAt(boundary.snapshot(), parallelPath)
	if !ok || !parallelCommit.Equal(wantParallel) {
		t.Fatalf("parallel commit = %x/%v, want %x", parallelCommit.Canonical(), ok, wantParallel.Canonical())
	}
}

func TestParallelWaitsForQuiescenceThenFailureOutranksRejection(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{{
			Name: "review",
			Parallel: &workflow.ParallelDraft{Graph: workflow.GraphDraft{
				Inputs: empty, Outputs: empty,
				Nodes: []workflow.NodeDraft{
					testLeaf("a-failure", empty, empty),
					testGateNode(t, "z-reject", false, "policy rejected"),
				},
			}},
		}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	gateEntered := make(chan struct{})
	releaseGate := make(chan struct{})
	paths := newPathRecordingBoundary(trace)
	boundary := &blockingGateBoundary{base: paths, entered: gateEntered, release: releaseGate}
	execution := startPathExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1, CancellationGrace: time.Second})

	waitClosed(t, gateEntered, "parallel rejection branch did not enter")
	failure := runner.execution(t, "a-failure")
	close(releaseGate)
	waitClosed(t, failure.cancelObserved, "parallel rejection did not cancel its active sibling")
	waited := make(chan Result, 1)
	go func() { waited <- execution.Wait() }()
	select {
	case result := <-waited:
		t.Fatalf("parallel returned before active failure branch quiesced: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}

	failure.complete(mustLeafFailure(t, MechanicalFailure, errBoom))
	var result Result
	select {
	case result = <-waited:
	case <-time.After(testTimeout):
		t.Fatal("parallel did not normalize after both branches quiesced")
	}
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	wantFailurePath := Path{}.AuthoredChild("review").AuthoredChild("a-failure")
	if comparePath(primary.Path(), wantFailurePath) != 0 || !errors.Is(primary.Error(), errBoom) {
		t.Fatalf("primary = %#v, want failure at %#v", primary, wantFailurePath.Components())
	}
	secondary := result.Secondary()
	if len(secondary) != 1 || secondary[0].Status() != Rejected {
		t.Fatalf("secondary = %#v, want one rejection", secondary)
	}
	wantRejectPath := Path{}.AuthoredChild("review").AuthoredChild("z-reject")
	reason, reasonOK := secondary[0].Reason()
	if comparePath(secondary[0].Path(), wantRejectPath) != 0 || !reasonOK || reason != "policy rejected" {
		t.Fatalf("rejection = %#v, want path %#v and exact reason", secondary[0], wantRejectPath.Components())
	}
	if len(paths.base.commits()) != 0 {
		t.Fatalf("non-successful parallel published commits: %#v", paths.base.commits())
	}
}

type blockingGateBoundary struct {
	base    Boundary
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingGateBoundary) Enter(ctx context.Context, instance Instance) error {
	components := instance.Path().Components()
	if len(components) > 0 {
		if name, ok := components[len(components)-1].AuthoredChild(); ok && name == "z-reject" {
			b.once.Do(func() { close(b.entered) })
			select {
			case <-b.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return b.base.Enter(ctx, instance)
}

func (b *blockingGateBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	return b.base.Commit(ctx, instance, output)
}

func (b *blockingGateBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func committedAt(events []pathBoundaryEvent, path Path) (value.Value, bool) {
	for _, event := range events {
		if event.kind == traceCommit && comparePath(event.path, path) == 0 {
			return event.output, true
		}
	}
	return value.Value{}, false
}
