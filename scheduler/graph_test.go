package scheduler

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

func TestGraphExecutionIndexKeepsLexicalReadinessAndDirectDataflow(t *testing.T) {
	producerOutput := testContract(t, testRequired(t, "payload", value.String()))
	consumerInput := testContract(t, testRequired(t, "selected", value.String()))
	consumerOutput := testContract(t, testRequired(t, "result", value.String()))
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: value.EmptyContract(), Outputs: consumerOutput,
		Nodes: []workflow.NodeDraft{
			testLeaf("z-independent", value.EmptyContract(), value.EmptyContract()),
			testLeaf("m-direct", consumerInput, consumerOutput),
			testLeaf("n-unrelated", value.EmptyContract(), value.EmptyContract()),
			testLeaf("a-source", value.EmptyContract(), producerOutput),
		},
		Edges: []workflow.EdgeDraft{
			{
				From: workflow.EndpointDraft{Kind: workflow.Child, Child: "a-source"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "m-direct"},
				Bindings: []workflow.BindingDraft{{From: []string{"payload"}, To: "selected"}},
			},
			{
				From: workflow.EndpointDraft{Kind: workflow.Child, Child: "m-direct"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
				Bindings: []workflow.BindingDraft{{From: []string{"result"}, To: "result"}},
			},
		},
	})
	state, err := applyGraphInputs(definition.Root(), emptyValue(t))
	if err != nil {
		t.Fatal(err)
	}
	source, ready := state.popReady()
	if !ready || state.nodes[source].node.Name() != "a-source" {
		t.Fatalf("first ready node = %q/%v, want lexical source", state.nodes[source].node.Name(), ready)
	}
	state.nodes[source].started = true
	exact := value.NewString("direct value")
	if err := state.applyChildOutput(source, testValue(t, map[string]value.Value{"payload": exact})); err != nil {
		t.Fatal(err)
	}

	wantReady := []string{"m-direct", "n-unrelated", "z-independent"}
	for _, want := range wantReady {
		index, ok := state.popReady()
		if !ok || state.nodes[index].node.Name() != want {
			t.Fatalf("next ready node = %q/%v, want %q", state.nodes[index].node.Name(), ok, want)
		}
		if want != "m-direct" {
			continue
		}
		input, inputErr := state.childInput(index)
		wantInput := testValue(t, map[string]value.Value{"selected": exact})
		if inputErr != nil || !input.Equal(wantInput) {
			t.Fatalf("direct consumer input = %x/%v, want %x", input.Canonical(), inputErr, wantInput.Canonical())
		}
		if err := state.applyChildOutput(index, testValue(t, map[string]value.Value{"result": exact})); err != nil {
			t.Fatal(err)
		}
	}
	output, err := objectValue(state.outputs)
	wantOutput := testValue(t, map[string]value.Value{"result": exact})
	if err != nil || !output.Equal(wantOutput) {
		t.Fatalf("indexed boundary output = %x/%v, want %x", output.Canonical(), err, wantOutput.Canonical())
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

func TestFailFastForceStopErrorDoesNotReleaseQuiescenceOrCapacity(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: []workflow.NodeDraft{testLeaf("work", empty, empty)}})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	runner.forceErrors["work"] = errors.New("force request failed")
	boundary := &forceStopObservationBoundary{base: newRecordingBoundary(trace), settled: make(chan struct{})}
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := scheduler.Start(context.Background(), definition, emptyValue(t))
	if err != nil {
		t.Fatal(err)
	}
	if started := runner.waitStarts(t, 1); len(started) != 1 || started[0] != "work" {
		t.Fatalf("first starts = %v", started)
	}
	firstLeaf := runner.execution(t, "work")
	secondRun, err := scheduler.Start(context.Background(), definition, emptyValue(t))
	if err != nil {
		t.Fatal(err)
	}

	firstResult := make(chan Result, 1)
	go func() { firstResult <- firstRun.Wait() }()
	firstRun.ForceStop()
	waitClosed(t, firstLeaf.forceObserved, "backend force-stop was not attempted")

	var first Result
	waitedForDone := false
	select {
	case <-firstLeaf.doneAfterForce:
		waitedForDone = true
	case <-boundary.settled:
		first = <-firstResult
		if started := runner.waitStarts(t, 1); len(started) != 1 || started[0] != "work" {
			t.Fatalf("premature second starts = %v", started)
		}
	}

	firstLeaf.complete(mustLeafCancelled(context.Canceled))
	if waitedForDone {
		first = <-firstResult
		if started := runner.waitStarts(t, 1); len(started) != 1 || started[0] != "work" {
			t.Fatalf("second starts = %v", started)
		}
	}
	secondLeaf := runner.execution(t, "work")
	secondLeaf.complete(mustLeafSuccess(t, emptyValue(t)))
	second := waitResult(t, secondRun)

	if !waitedForDone {
		t.Error("Wait returned before the failed force-stop execution closed Done")
		t.Error("global capacity was reused before the failed force-stop execution closed Done")
	}
	requireStatus(t, first, Cancelled)
	requireStatus(t, second, Succeeded)
	workSettlements := boundary.base.settlements("work")
	if len(workSettlements) == 0 {
		t.Fatal("failed force-stop leaf did not publish settlement")
	}
	settledForce, settledOK := forceFailure(workSettlements[0])
	returnedForce, returnedOK := forceFailure(first)
	if !settledOK || !returnedOK || !reflect.DeepEqual(settledForce, returnedForce) {
		t.Fatalf("settled force diagnostic = %#v/%v, returned = %#v/%v", settledForce, settledOK, returnedForce, returnedOK)
	}
	rootSettlements := boundary.base.settlements("root")
	if len(rootSettlements) != 1 || !reflect.DeepEqual(rootSettlements[0], first) {
		t.Fatalf("root settlement and Wait differ: settled = %#v, returned = %#v", rootSettlements, first)
	}
}

type forceStopObservationBoundary struct {
	base       *recordingBoundary
	settled    chan struct{}
	settleOnce sync.Once
}

func (b *forceStopObservationBoundary) Enter(ctx context.Context, instance Instance) error {
	return b.base.Enter(ctx, instance)
}

func (b *forceStopObservationBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	return b.base.Commit(ctx, instance, output)
}

func (b *forceStopObservationBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	if instanceName(instance) == "work" {
		b.settleOnce.Do(func() { close(b.settled) })
	}
	return b.base.Settle(ctx, instance, result)
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

func TestExternalCancellationRespectsDirectRootTerminalClaimOrder(t *testing.T) {
	for _, mode := range []rootCancellationMode{rootEnterCancellation, rootCommitCancellation} {
		t.Run(string(mode), func(t *testing.T) {
			trace := &eventTrace{}
			boundary := &rootCancellationBoundary{
				mode: mode, blocked: make(chan struct{}), base: newRecordingBoundary(trace),
			}
			runner := newControlledRunner(trace)
			definition, input := rootCancellationDefinition(t, mode)
			scheduler, err := New(runner, boundary, Policy{Capacity: 1})
			if err != nil {
				t.Fatal(err)
			}
			execution, err := scheduler.Start(context.Background(), definition, input)
			if err != nil {
				t.Fatal(err)
			}
			waitClosed(t, boundary.blocked, "root terminal path did not block")
			execution.Cancel()
			result := waitResult(t, execution)
			if mode == rootCommitCancellation {
				requireStatus(t, result, Succeeded)
				if _, committed := boundary.base.commits()["root"]; !committed {
					t.Fatal("root success claimed before cancellation did not commit")
				}
				if settlements := boundary.base.settlements("root"); len(settlements) != 0 {
					t.Fatalf("late cancellation produced root settlement %#v", settlements)
				}
				return
			}
			requireStatus(t, result, Cancelled)
			primary, _ := result.Primary()
			if !errors.Is(primary.Error(), context.Canceled) {
				t.Fatalf("primary = %#v", primary)
			}
		})
	}
}

func TestExternalCancelAppearsOnceAcrossNestedGraphSettlements(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{{Name: "nested", Graph: &workflow.GraphDraft{
			Inputs: empty, Outputs: empty,
			Nodes: []workflow.NodeDraft{testLeaf("work", empty, empty)},
		}}},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
	runner.waitStarts(t, 1)
	execution.Cancel()
	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)

	for _, child := range []string{"work", "nested", "root"} {
		settlements := boundary.settlements(child)
		if len(settlements) != 1 {
			t.Fatalf("%s settlements = %d, want 1", child, len(settlements))
		}
		if countExternalCancellations(settlements[0]) != 1 {
			t.Fatalf("%s external diagnostics = %d, want 1: %#v", child, countExternalCancellations(settlements[0]), settlements[0])
		}
	}
	if countExternalCancellations(result) != 1 {
		t.Fatalf("Wait external diagnostics = %d, want 1: %#v", countExternalCancellations(result), result)
	}
	root := boundary.settlements("root")[0]
	if !reflect.DeepEqual(root, result) {
		t.Fatalf("root settlement and Wait differ: settled = %#v, returned = %#v", root, result)
	}
}

func TestExternalCancelAfterSettlementStartsDoesNotMutateTerminalResult(t *testing.T) {
	trace := &eventTrace{}
	base := newRecordingBoundary(trace)
	boundary := &blockingSettlementBoundary{base: base, entered: make(chan struct{}), release: make(chan struct{})}
	definition, input := rootCancellationDefinition(t, rootOutputSettlementCancellation)
	scheduler, err := New(newControlledRunner(trace), boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(context.Background(), definition, input)
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, boundary.entered, "root settlement did not begin")
	execution.Cancel()
	close(boundary.release)
	returned := waitResult(t, execution)
	settlements := base.settlements("root")
	if len(settlements) != 1 {
		t.Fatalf("root settlements = %d, want 1", len(settlements))
	}
	if !reflect.DeepEqual(settlements[0], returned) {
		t.Fatalf("settlement and Wait differ: settled = %#v, returned = %#v", settlements[0], returned)
	}
	requireStatus(t, returned, Failed)
	if countExternalCancellations(returned) != 0 {
		t.Fatalf("late cancellation mutated terminal result: %#v", returned)
	}
}

func TestExternalCancellationClaimBeforeRootCommitPreventsPublication(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("work", empty, empty)},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
	claimReached := make(chan struct{})
	var once sync.Once
	execution.control.beforeTerminalClaim = func(ctx context.Context, path Path) {
		if len(path.Components()) != 0 {
			return
		}
		once.Do(func() { close(claimReached) })
		<-ctx.Done()
	}

	runner.execution(t, "work").complete(mustLeafSuccess(t, emptyValue(t)))
	waitClosed(t, claimReached, "root did not reach its successful terminal claim")
	execution.Cancel()
	result := waitResult(t, execution)

	requireStatus(t, result, Cancelled)
	if _, committed := boundary.commits()["root"]; committed {
		t.Fatal("root published success after external cancellation claimed the terminal transition")
	}
	settled := boundary.settlements("root")
	if len(settled) != 1 || !reflect.DeepEqual(settled[0], result) {
		t.Fatalf("root settlement and Wait differ: settled = %#v, returned = %#v", settled, result)
	}
	if countExternalCancellations(result) != 1 {
		t.Fatalf("external cancellation diagnostics = %d, want exactly one: %#v", countExternalCancellations(result), result)
	}
	if diagnostics := resultDiagnostics(result); len(diagnostics) != 1 {
		t.Fatalf("cancel-first root claim produced duplicate diagnostics: %#v", diagnostics)
	}
}

func TestAncestorFailFastClaimBeforeProtectedCommitPreventsPublication(t *testing.T) {
	empty := value.EmptyContract()
	protected := workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("protected-work", empty, empty)},
	}
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{
			{Name: "protected", Graph: &protected},
			testLeaf("trigger", empty, empty),
		},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2})
	claimReached := make(chan struct{})
	protectedPath := Path{}.AuthoredChild("protected")
	var once sync.Once
	execution.control.beforeTerminalClaim = func(ctx context.Context, path Path) {
		if comparePath(path, protectedPath) != 0 {
			return
		}
		once.Do(func() { close(claimReached) })
		<-ctx.Done()
	}

	runner.waitStarts(t, 2)
	runner.execution(t, "protected-work").complete(mustLeafSuccess(t, emptyValue(t)))
	waitClosed(t, claimReached, "protected graph did not reach its successful terminal claim")
	triggerErr := errors.New("ancestor fail-fast trigger")
	runner.execution(t, "trigger").complete(mustLeafFailure(t, MechanicalFailure, triggerErr))
	result := waitResult(t, execution)

	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	if !errors.Is(primary.Error(), triggerErr) {
		t.Fatalf("root primary = %#v, want trigger failure", primary)
	}
	if _, committed := boundary.commits()["protected"]; committed {
		t.Fatal("protected graph published success after ancestor fail-fast claimed cancellation")
	}
	protectedSettlements := boundary.settlements("protected")
	if len(protectedSettlements) != 1 {
		t.Fatalf("protected settlements = %d, want exactly one", len(protectedSettlements))
	}
	requireStatus(t, protectedSettlements[0], Cancelled)
	protectedPrimary, _ := protectedSettlements[0].Primary()
	if !protectedPrimary.parentCancelled || protectedPrimary.external {
		t.Fatalf("protected settlement = %#v, want one parent-induced cancellation", protectedSettlements[0])
	}
	if diagnostics := resultDiagnostics(protectedSettlements[0]); len(diagnostics) != 1 {
		t.Fatalf("protected cancellation produced duplicate diagnostics: %#v", diagnostics)
	}
	rootSettlements := boundary.settlements("root")
	if len(rootSettlements) != 1 || !reflect.DeepEqual(rootSettlements[0], result) {
		t.Fatalf("root settlement and Wait differ: settled = %#v, returned = %#v", rootSettlements, result)
	}
}

func TestSuccessfulTerminalClaimWinsLaterExternalCancellation(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("work", empty, empty)},
	})
	trace := &eventTrace{}
	base := newRecordingBoundary(trace)
	boundary := &blockingSuccessfulRootCommitBoundary{
		base: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := newControlledRunner(trace)
	execution := startPathExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
	runner.execution(t, "work").complete(mustLeafSuccess(t, emptyValue(t)))
	waitClosed(t, boundary.entered, "root successful commit did not begin")
	execution.Cancel()
	close(boundary.release)
	result := waitResult(t, execution)

	requireStatus(t, result, Succeeded)
	if _, committed := base.commits()["root"]; !committed {
		t.Fatal("root terminal claim did not publish its successful output")
	}
	if settlements := base.settlements("root"); len(settlements) != 0 {
		t.Fatalf("late cancellation produced root settlements: %#v", settlements)
	}
	if repeated := execution.Wait(); !reflect.DeepEqual(repeated, result) {
		t.Fatalf("repeated Wait = %#v, first = %#v", repeated, result)
	}
}

type blockingSuccessfulRootCommitBoundary struct {
	base    *recordingBoundary
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSuccessfulRootCommitBoundary) Enter(ctx context.Context, instance Instance) error {
	return b.base.Enter(ctx, instance)
}

func (b *blockingSuccessfulRootCommitBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	if err := b.base.Commit(ctx, instance, output); err != nil {
		return err
	}
	if instanceName(instance) == "root" {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return nil
}

func (b *blockingSuccessfulRootCommitBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

type blockingSettlementBoundary struct {
	base    *recordingBoundary
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSettlementBoundary) Enter(ctx context.Context, instance Instance) error {
	return b.base.Enter(ctx, instance)
}

func (b *blockingSettlementBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	return b.base.Commit(ctx, instance, output)
}

func (b *blockingSettlementBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	if err := b.base.Settle(ctx, instance, result); err != nil {
		return err
	}
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

func countExternalCancellations(result Result) int {
	diagnostics := result.Secondary()
	if primary, ok := result.Primary(); ok {
		diagnostics = append([]Diagnostic{primary}, diagnostics...)
	}
	count := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Status() == Cancelled && !diagnostic.parentCancelled && len(diagnostic.Path().Components()) == 0 && errors.Is(diagnostic.Error(), context.Canceled) {
			count++
		}
	}
	return count
}

func forceFailure(result Result) (Diagnostic, bool) {
	for _, diagnostic := range result.Secondary() {
		if diagnostic.Status() == Failed && diagnostic.Error() != nil && strings.Contains(diagnostic.Error().Error(), "force stop leaf") {
			return diagnostic, true
		}
	}
	return Diagnostic{}, false
}

type rootCancellationMode string

const (
	rootEnterCancellation            rootCancellationMode = "enter"
	rootCommitCancellation           rootCancellationMode = "commit"
	rootOutputSettlementCancellation rootCancellationMode = "output settlement"
)

type rootCancellationBoundary struct {
	mode        rootCancellationMode
	blocked     chan struct{}
	base        *recordingBoundary
	once        sync.Once
	rootContext context.Context
}

func (b *rootCancellationBoundary) Enter(ctx context.Context, instance Instance) error {
	if instanceName(instance) == "root" {
		b.rootContext = ctx
	}
	if b.mode != rootEnterCancellation || instanceName(instance) != "root" {
		return b.base.Enter(ctx, instance)
	}
	b.signalBlocked()
	<-b.rootContext.Done()
	return errors.New("enter failed independently after cancellation")
}

func (b *rootCancellationBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	if b.mode != rootCommitCancellation || instanceName(instance) != "root" {
		return b.base.Commit(ctx, instance, output)
	}
	b.signalBlocked()
	<-b.rootContext.Done()
	return b.base.Commit(ctx, instance, output)
}

func (b *rootCancellationBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func (b *rootCancellationBoundary) signalBlocked() {
	b.once.Do(func() { close(b.blocked) })
}

func rootCancellationDefinition(t *testing.T, mode rootCancellationMode) (workflow.Definition, value.Value) {
	t.Helper()
	if mode != rootOutputSettlementCancellation {
		empty := value.EmptyContract()
		return testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty}), emptyValue(t)
	}
	inputs := testContract(t, testRequired(t, "source", value.Any()))
	outputs := testContract(t, testRequired(t, "target", value.String()))
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: []workflow.BindingDraft{{From: []string{"source"}, To: "target"}},
		}},
	})
	return definition, testValue(t, map[string]value.Value{"source": value.NewBoolean(true)})
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

func TestGraphObservesClosedCancellationSignalOnlyOnceWhileChildDrains(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("noncooperative", empty, empty)},
	})
	trace := &eventTrace{}
	runner := newMapTestRunner()
	boundary := newPathRecordingBoundary(trace)
	execution := startMapTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1, CancellationGrace: 100 * time.Millisecond})
	started := runner.waitStarts(t, 1)[0]
	var observations atomic.Int32
	execution.control.graphCancellationObserved = func(path Path) {
		if len(path.Components()) == 0 {
			observations.Add(1)
		}
	}

	execution.Cancel()
	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	if got := observations.Load(); got != 1 {
		t.Fatalf("root graph observed its permanently closed cancellation signal %d times, want exactly once", got)
	}
	if started.terminalizations() != 1 || started.forceCalls() != 1 {
		t.Fatalf("noncooperative child terminal/force counts = %d/%d, want 1/1", started.terminalizations(), started.forceCalls())
	}
	if runner.activeCount() != 0 {
		t.Fatalf("active child executions = %d, want quiescence", runner.activeCount())
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
