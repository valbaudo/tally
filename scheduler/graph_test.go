package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func TestGraphStartsIndependentReadyLeavesBeforeEitherCompletes(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("second", empty, empty), testLeaf("first", empty, empty)},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 2})

	if got := sortedStrings(runner.waitStarts(t, 2)); got[0] != "first" || got[1] != "second" {
		t.Fatalf("started = %v", got)
	}
	completeEmpty(t, runner.execution(t, "first"), runner.execution(t, "second"))
	requireStatus(t, waitResult(t, execution), Succeeded)
}

func TestDAGCompletionDependencyDelaysConsumerAndCommitPrecedesConsumption(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("producer", empty, empty), testLeaf("consumer", empty, empty)},
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Child, Child: "producer"},
			To:   workflow.EndpointDraft{Kind: workflow.Child, Child: "consumer"},
		}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 2})

	runner.waitStarts(t, 1)
	assertNoStart(t, runner, 25*time.Millisecond)
	runner.execution(t, "producer").complete(mustLeafSuccess(t, emptyValue(t)))
	if started := runner.waitStarts(t, 1); started[0] != "consumer" {
		t.Fatalf("started = %v, want consumer", started)
	}
	if commit, start := trace.index(traceCommit, "producer"), trace.index(traceStart, "consumer"); commit < 0 || start < 0 || commit >= start {
		t.Fatalf("trace = %#v; producer commit %d must precede consumer start %d", trace.snapshot(), commit, start)
	}
	completeEmpty(t, runner.execution(t, "consumer"))
	requireStatus(t, waitResult(t, execution), Succeeded)
}

func TestDAGBindingSuppliesExactNestedValueAndSourceOrderDoesNotChangeOutput(t *testing.T) {
	nestedType := testObject(t, testRequired(t, "exact", value.String()))
	producerOutput := testContract(t, testRequired(t, "payload", nestedType))
	consumerInput := testContract(t, testRequired(t, "selected", value.String()))
	rootOutput := testContract(t, testRequired(t, "result", value.String()))
	want := value.NewString("nested value")
	produced := testValue(t, map[string]value.Value{
		"payload": testValue(t, map[string]value.Value{"exact": want}),
	})
	consumerResult := testValue(t, map[string]value.Value{"result": want})

	build := func(nodes []workflow.NodeDraft) workflow.Definition {
		return testDefinition(t, workflow.GraphDraft{
			Inputs: value.EmptyContract(), Outputs: rootOutput, Nodes: nodes,
			Edges: []workflow.EdgeDraft{
				{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "producer"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "consumer"}, Bindings: []workflow.BindingDraft{{From: []string{"payload", "exact"}, To: "selected"}}},
				{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "consumer"}, To: workflow.EndpointDraft{Kind: workflow.Boundary}, Bindings: []workflow.BindingDraft{{From: []string{"result"}, To: "result"}}},
			},
		})
	}
	for _, nodes := range [][]workflow.NodeDraft{
		{testLeaf("producer", value.EmptyContract(), producerOutput), testLeaf("consumer", consumerInput, rootOutput)},
		{testLeaf("consumer", consumerInput, rootOutput), testLeaf("producer", value.EmptyContract(), producerOutput)},
	} {
		trace := &eventTrace{}
		runner := newControlledRunner(trace)
		execution := startTestExecution(t, build(nodes), emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 2})
		runner.execution(t, "producer").complete(mustLeafSuccess(t, produced))
		consumer := runner.execution(t, "consumer")
		requestInput := runnerInput(t, runner, "consumer")
		if !requestInput.Equal(testValue(t, map[string]value.Value{"selected": want})) {
			t.Fatalf("consumer input = %s", requestInput.Canonical())
		}
		consumer.complete(mustLeafSuccess(t, consumerResult))
		result := waitResult(t, execution)
		requireStatus(t, result, Succeeded)
		output, _ := result.Output()
		if !output.Equal(consumerResult) {
			t.Fatalf("output = %s, want %s", output.Canonical(), consumerResult.Canonical())
		}
	}
}

func TestGraphBoundaryBindingPassesInputDirectlyToOutput(t *testing.T) {
	inputs := testContract(t, testRequired(t, "source", value.String()))
	outputs := testContract(t, testRequired(t, "target", value.String()))
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: []workflow.BindingDraft{{From: []string{"source"}, To: "target"}},
		}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	input := testValue(t, map[string]value.Value{"source": value.NewString("direct")})
	execution := startTestExecution(t, definition, input, runner, newRecordingBoundary(trace), Policy{Capacity: 1})
	result := waitResult(t, execution)
	requireStatus(t, result, Succeeded)
	output, _ := result.Output()
	want := testValue(t, map[string]value.Value{"target": value.NewString("direct")})
	if !output.Equal(want) || runner.count() != 0 {
		t.Fatalf("output = %s, starts = %d", output.Canonical(), runner.count())
	}
}

func TestCapacityLimitsOnlyActiveExternalLeaves(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("a", empty, empty), testLeaf("b", empty, empty)},
	})
	for _, capacity := range []int{1, 2} {
		t.Run(string(rune('0'+capacity)), func(t *testing.T) {
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: capacity})
			started := runner.waitStarts(t, 1)
			if capacity == 1 {
				assertNoStart(t, runner, 25*time.Millisecond)
				runner.execution(t, started[0]).complete(mustLeafSuccess(t, emptyValue(t)))
				runner.waitStarts(t, 1)
			} else {
				runner.waitStarts(t, 1)
			}
			completeEmpty(t, runner.execution(t, "a"), runner.execution(t, "b"))
			requireStatus(t, waitResult(t, execution), Succeeded)
			if runner.maximum() != capacity {
				t.Fatalf("maximum active = %d, want %d", runner.maximum(), capacity)
			}
		})
	}
}

func TestGraphValidationAndBoundaryFailuresExposeNoPublicOutput(t *testing.T) {
	empty := value.EmptyContract()
	stringOutput := testContract(t, testRequired(t, "result", value.String()))
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: stringOutput,
		Nodes: []workflow.NodeDraft{testLeaf("work", empty, stringOutput)},
		Edges: []workflow.EdgeDraft{{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "work"}, To: workflow.EndpointDraft{Kind: workflow.Boundary}, Bindings: []workflow.BindingDraft{{From: []string{"result"}, To: "result"}}}},
	})
	for _, testCase := range []struct {
		name      string
		configure func(*recordingBoundary)
		complete  value.Value
	}{
		{name: "output validation", complete: emptyValue(t)},
		{name: "leaf commit", complete: testValue(t, map[string]value.Value{"result": value.NewString("ok")}), configure: func(boundary *recordingBoundary) { boundary.commitError["work"] = errors.New("commit failed") }},
		{name: "root commit", complete: testValue(t, map[string]value.Value{"result": value.NewString("ok")}), configure: func(boundary *recordingBoundary) { boundary.commitError["root"] = errors.New("root commit failed") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			boundary := newRecordingBoundary(trace)
			if testCase.configure != nil {
				testCase.configure(boundary)
			}
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
			runner.execution(t, "work").complete(mustLeafSuccess(t, testCase.complete))
			result := waitResult(t, execution)
			requireStatus(t, result, Failed)
			if _, public := result.Output(); public {
				t.Fatal("failed result exposed public output")
			}
		})
	}
}

func TestGraphStartRejectsInvalidRuntimeInputsBeforeRootEnter(t *testing.T) {
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	validDefinition := testDefinition(t, workflow.GraphDraft{Inputs: value.EmptyContract(), Outputs: value.EmptyContract()})

	if _, err := New(nil, boundary, Policy{Capacity: 1}); err == nil {
		t.Fatal("New accepted nil runner")
	}
	if _, err := New(runner, nil, Policy{Capacity: 1}); err == nil {
		t.Fatal("New accepted nil boundary")
	}
	for _, policy := range []Policy{{Capacity: 0}, {Capacity: 1, CancellationGrace: -time.Nanosecond}} {
		if _, err := New(runner, boundary, policy); err == nil {
			t.Fatalf("New accepted policy %#v", policy)
		}
	}
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name       string
		ctx        context.Context
		definition workflow.Definition
		input      value.Value
	}{
		{name: "nil context", definition: validDefinition, input: emptyValue(t)},
		{name: "zero definition", ctx: context.Background(), input: emptyValue(t)},
		{name: "invalid value", ctx: context.Background(), definition: validDefinition, input: value.NewString("not object")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := scheduler.Start(testCase.ctx, testCase.definition, testCase.input); err == nil {
				t.Fatal("Start accepted invalid input")
			}
		})
	}
	if trace.index(traceEnter, "root") != -1 {
		t.Fatalf("root entered during rejected start: %#v", trace.snapshot())
	}
}

func TestFailFastCancelsSiblingsWaitsGraceForceStopsAndQuiesces(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{
			testLeaf("failure", empty, empty), testLeaf("cooperative", empty, empty),
			testLeaf("stuck", empty, empty), testLeaf("descendant", empty, empty),
		},
		Edges: []workflow.EdgeDraft{{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "failure"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "descendant"}}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	runner.cooperative["cooperative"] = true
	grace := 40 * time.Millisecond
	execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 3, CancellationGrace: grace})
	runner.waitStarts(t, 3)
	failureAt := time.Now()
	runner.execution(t, "failure").complete(mustLeafFailure(t, MechanicalFailure, errBoom))
	cooperative := runner.execution(t, "cooperative")
	stuck := runner.execution(t, "stuck")
	waitClosed(t, cooperative.cancelObserved, "cooperative sibling did not receive cancellation")
	waitClosed(t, stuck.cancelObserved, "stuck sibling did not receive cancellation")
	result := waitResult(t, execution)
	requireStatus(t, result, Failed)
	if elapsed := stuck.forceTime().Sub(failureAt); elapsed < grace || elapsed > grace+300*time.Millisecond {
		t.Fatalf("force stop after %v, want supplied grace %v", elapsed, grace)
	}
	if trace.index(traceForceStop, "cooperative") != -1 || trace.index(traceForceStop, "stuck") == -1 {
		t.Fatalf("force-stop trace = %#v", trace.snapshot())
	}
	if trace.index(traceStart, "descendant") != -1 {
		t.Fatal("newly ready descendant started after fail-fast")
	}
	primary, _ := result.Primary()
	if primary.Path().Components()[0].Kind() != AuthoredChildComponent || primary.Error() != errBoom {
		t.Fatalf("primary = %#v, want original failure", primary)
	}
	if len(result.Secondary()) != 2 {
		t.Fatalf("secondary = %#v, want sibling cancellations", result.Secondary())
	}
}

func TestExternalCancelWinsIntrinsicFailure(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: []workflow.NodeDraft{testLeaf("failure", empty, empty), testLeaf("stuck", empty, empty)}})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 2, CancellationGrace: 30 * time.Millisecond})
	runner.waitStarts(t, 2)
	runner.execution(t, "failure").complete(mustLeafFailure(t, MechanicalFailure, errBoom))
	waitClosed(t, runner.execution(t, "stuck").cancelObserved, "fail-fast cancellation was not observed")
	execution.Cancel()
	execution.Cancel()
	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	primary, _ := result.Primary()
	if !errors.Is(primary.Error(), context.Canceled) {
		t.Fatalf("primary = %#v", primary)
	}
}

func TestExternalForceStopImmediatelyStopsActiveLeaves(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: []workflow.NodeDraft{testLeaf("a", empty, empty), testLeaf("b", empty, empty)}})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 2, CancellationGrace: time.Second})
	runner.waitStarts(t, 2)
	started := time.Now()
	execution.ForceStop()
	execution.ForceStop()
	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	for _, name := range []string{"a", "b"} {
		leaf := runner.execution(t, name)
		waitClosed(t, leaf.cancelObserved, name+" did not receive cancellation")
		waitClosed(t, leaf.forceObserved, name+" was not force-stopped")
		if elapsed := leaf.forceTime().Sub(started); elapsed > 250*time.Millisecond {
			t.Fatalf("%s force stop waited %v", name, elapsed)
		}
	}
}

func TestExternalCancelForceSignalPreservesAlreadyCompletedLeafBoundary(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: []workflow.NodeDraft{testLeaf("work", empty, empty)}})
	trace := &eventTrace{}
	runner := newCompletionOnForcePathRunner(t)
	boundary := newRecordingBoundary(trace)
	scheduler, err := New(runner, boundary, Policy{Capacity: 1, CancellationGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(context.Background(), definition, emptyValue(t))
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, runner.started, "leaf did not start")
	waitClosed(t, runner.ready, "leaf did not enter its completion select")
	execution.control.forceOnce.Do(func() { close(execution.control.force) })
	result := waitResult(t, execution)
	requireStatus(t, result, Succeeded)
	if _, committed := boundary.commits()["work"]; !committed {
		t.Fatal("already completed leaf result was not committed before cancellation normalized")
	}
	if runner.exec.wasForced() {
		t.Fatal("already completed backend execution was force-stopped")
	}
}

func runnerInput(t *testing.T, runner *controlledRunner, name string) value.Value {
	t.Helper()
	execution := runner.execution(t, name)
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if !execution.input.Valid() {
		t.Fatalf("%s has no recorded input", name)
	}
	return execution.input
}
