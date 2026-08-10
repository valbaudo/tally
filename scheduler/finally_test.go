package scheduler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

// Removing the unconditional post-entry unwind, or starting cleanup before the
// body has settled, makes at least one of these terminal-body cases fail.
func TestFinallyRunsAfterEveryQuiescentBodyOutcome(t *testing.T) {
	timeoutErr := errors.New("body timed out")
	timeoutCompletion, err := NewLeafTimeout(timeoutErr)
	if err != nil {
		t.Fatal(err)
	}
	intrinsicErr := errors.New("backend cancelled body")
	intrinsicCompletion, err := NewLeafCancelled(intrinsicErr)
	if err != nil {
		t.Fatal(err)
	}
	requiredOutput := testContract(t, testRequired(t, "required", value.String()))

	tests := []struct {
		name        string
		body        workflow.NodeDraft
		completion  LeafCompletion
		bodyRunner  bool
		wantStatus  Status
		wantOutcome string
		bodyEvent   traceKind
	}{
		{name: "success", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: mustLeafSuccess(t, emptyValue(t)), bodyRunner: true, wantStatus: Succeeded, wantOutcome: "succeeded", bodyEvent: traceCommit},
		{name: "rejection", body: testGateNode(t, "body", false, "denied"), wantStatus: Rejected, wantOutcome: "rejected", bodyEvent: traceSettle},
		{name: "mechanical failure", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: mustLeafFailure(t, MechanicalFailure, errBoom), bodyRunner: true, wantStatus: Failed, wantOutcome: "failed", bodyEvent: traceSettle},
		{name: "contract failure", body: testLeaf("body", value.EmptyContract(), requiredOutput), completion: mustLeafSuccess(t, emptyValue(t)), bodyRunner: true, wantStatus: Failed, wantOutcome: "failed", bodyEvent: traceSettle},
		{name: "timeout", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: timeoutCompletion, bodyRunner: true, wantStatus: Failed, wantOutcome: "failed", bodyEvent: traceSettle},
		{name: "intrinsic cancellation", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: intrinsicCompletion, bodyRunner: true, wantStatus: Cancelled, wantOutcome: "cancelled", bodyEvent: traceSettle},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			definition := finallySimpleDefinition(t, testCase.body)
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			boundary := newRecordingBoundary(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})

			if testCase.bodyRunner {
				runner.execution(t, "body").complete(testCase.completion)
			}
			cleanup := runner.execution(t, "cleanup")
			if bodyDone, cleanupStart := trace.index(testCase.bodyEvent, "body"), trace.index(traceStart, "cleanup"); bodyDone < 0 || cleanupStart < 0 || bodyDone >= cleanupStart {
				t.Fatalf("body terminal event %d must precede cleanup start %d: %#v", bodyDone, cleanupStart, trace.snapshot())
			}
			assertFinallyOutcomeInput(t, runnerInput(t, runner, "cleanup"), testCase.wantOutcome)
			if trace.index(traceCommit, "root") >= 0 {
				t.Fatal("protected graph committed before cleanup")
			}

			cleanup.complete(mustLeafSuccess(t, emptyValue(t)))
			result := waitResult(t, execution)
			requireStatus(t, result, testCase.wantStatus)
			if testCase.wantStatus == Succeeded {
				if cleanupCommit, rootCommit := trace.index(traceCommit, "cleanup"), trace.index(traceCommit, "root"); cleanupCommit < 0 || rootCommit < 0 || cleanupCommit >= rootCommit {
					t.Fatalf("cleanup commit %d must precede protected commit %d: %#v", cleanupCommit, rootCommit, trace.snapshot())
				}
			} else if _, committed := boundary.commits()["root"]; committed {
				t.Fatal("non-successful protected graph published an output")
			}
		})
	}
}

// Removing the distinction between pre-entry validation and post-entry body
// failure either skips required cleanup or runs cleanup without a valid scope.
func TestFinallyGuaranteeBeginsOnlyAfterProtectedEntry(t *testing.T) {
	t.Run("post-entry pre-runner failure cleans up", func(t *testing.T) {
		definition := finallySimpleDefinition(t, testLeaf("body", value.EmptyContract(), value.EmptyContract()))
		trace := &eventTrace{}
		runner := newControlledRunner(trace)
		boundary := newRecordingBoundary(trace)
		boundary.enterErrors["body"] = errors.New("body enter failed")
		execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})

		cleanup := runner.execution(t, "cleanup")
		if runner.count() != 1 {
			t.Fatalf("runner starts = %d, want cleanup only", runner.count())
		}
		if rootEnter, bodyEnter, cleanupStart := trace.index(traceEnter, "root"), trace.index(traceEnter, "body"), trace.index(traceStart, "cleanup"); rootEnter < 0 || bodyEnter <= rootEnter || cleanupStart <= bodyEnter {
			t.Fatalf("entry/failure/cleanup order is wrong: %#v", trace.snapshot())
		}
		assertFinallyOutcomeInput(t, runnerInput(t, runner, "cleanup"), "failed")
		cleanup.complete(mustLeafSuccess(t, emptyValue(t)))
		requireStatus(t, waitResult(t, execution), Failed)
	})

	t.Run("invalid protected input never enters or cleans up", func(t *testing.T) {
		inputs := testContract(t, testRequired(t, "request", value.String()))
		definition := finallyDefinition(t, inputs, testLeaf("body", value.EmptyContract(), value.EmptyContract()), finallyOutcomeInputs(t), []workflow.CleanupBindingDraft{{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupOutcome}, To: "outcome"}})
		trace := &eventTrace{}
		runner := newControlledRunner(trace)
		boundary := newRecordingBoundary(trace)
		scheduler, err := New(runner, boundary, Policy{Capacity: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scheduler.Start(context.Background(), definition, emptyValue(t)); err == nil {
			t.Fatal("Start accepted invalid protected input")
		}
		if len(trace.snapshot()) != 0 || runner.count() != 0 {
			t.Fatalf("events = %#v, starts = %d; pre-entry failure ran cleanup", trace.snapshot(), runner.count())
		}
	})
}

// Replacing the declared binding walk with captured state, transitive lookup,
// or zero-value filling changes this exact closed cleanup request.
func TestFinallyCleanupInputContainsOnlyDeclaredCommittedValues(t *testing.T) {
	requestType := testObject(t, testRequired(t, "payload", value.String()))
	protectedInputs := testContract(t, testRequired(t, "request", requestType))
	bodyOutputs := testContract(t,
		testRequired(t, "handle", value.String()),
		testRequired(t, "secret", value.String()),
	)
	cleanupInputs := testContract(t,
		testRequired(t, "original", value.String()),
		testRequired(t, "outcome", finallyOutcomeType(t)),
		testOptional(t, "handle", value.String()),
		testOptional(t, "absent", value.String()),
		testOptional(t, "leak", value.String()),
	)
	body := testLeaf("committed", value.EmptyContract(), bodyOutputs)
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: protectedInputs, Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{
			body,
			testLeaf("failed", value.EmptyContract(), value.EmptyContract()),
			testLeaf("never", value.EmptyContract(), bodyOutputs),
		},
		Edges: []workflow.EdgeDraft{{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "failed"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "never"}}},
		Finally: &workflow.FinallyDraft{
			Graph: finallyCleanupGraph(t, cleanupInputs),
			Bindings: []workflow.CleanupBindingDraft{
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupInput, Path: []string{"request", "payload"}}, To: "original"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupOutcome}, To: "outcome"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupChild, Child: "committed", Path: []string{"handle"}}, To: "handle"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupChild, Child: "never", Path: []string{"handle"}}, To: "absent"},
			},
		},
	})
	exact := value.NewString("original\x00\xff")
	input := testValue(t, map[string]value.Value{"request": testValue(t, map[string]value.Value{"payload": exact})})
	committedOutput := testValue(t, map[string]value.Value{
		"handle": value.NewString("committed handle"),
		"secret": value.NewString("must not leak"),
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	execution := startTestExecution(t, definition, input, runner, newRecordingBoundary(trace), Policy{Capacity: 2, CancellationGrace: time.Second})
	runner.waitStarts(t, 2)
	committed := runner.execution(t, "committed")
	runner.execution(t, "failed").complete(mustLeafFailure(t, MechanicalFailure, errBoom))
	waitClosed(t, committed.cancelObserved, "still-active committed child did not observe fail-fast")
	committed.complete(mustLeafSuccess(t, committedOutput))
	waitForTrace(t, trace, traceCommit, "committed")

	cleanup := runner.execution(t, "cleanup")
	want := testValue(t, map[string]value.Value{
		"original": exact,
		"outcome":  value.NewString("failed"),
		"handle":   value.NewString("committed handle"),
	})
	if got := runnerInput(t, runner, "cleanup"); !got.Equal(want) {
		t.Fatalf("cleanup input = %x, want exact closed input %x", got.Canonical(), want.Canonical())
	}
	if trace.index(traceStart, "never") >= 0 {
		t.Fatal("unstarted child ran or contributed cleanup data")
	}
	cleanup.complete(mustLeafSuccess(t, emptyValue(t)))
	requireStatus(t, waitResult(t, execution), Failed)
}

// Reusing ordinary sibling normalization here would let cleanup failure replace
// an already-established rejection or cancellation.
func TestFinallyCleanupFailureUsesUnwindingPrecedenceAndExactSettlement(t *testing.T) {
	intrinsicErr := errors.New("intrinsic body cancellation")
	intrinsic, err := NewLeafCancelled(intrinsicErr)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		body       workflow.NodeDraft
		completion LeafCompletion
		runnerBody bool
		wantStatus Status
		wantError  error
		wantReason string
	}{
		{name: "successful body", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: mustLeafSuccess(t, emptyValue(t)), runnerBody: true, wantStatus: Failed},
		{name: "failed body", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: mustLeafFailure(t, MechanicalFailure, errBoom), runnerBody: true, wantStatus: Failed, wantError: errBoom},
		{name: "rejected body", body: testGateNode(t, "body", false, "body denied"), wantStatus: Rejected, wantReason: "body denied"},
		{name: "cancelled body", body: testLeaf("body", value.EmptyContract(), value.EmptyContract()), completion: intrinsic, runnerBody: true, wantStatus: Cancelled, wantError: intrinsicErr},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			definition := finallySimpleDefinition(t, testCase.body)
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			boundary := newRecordingBoundary(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
			if testCase.runnerBody {
				runner.execution(t, "body").complete(testCase.completion)
			}
			cleanupErr := errors.New("cleanup failed")
			runner.execution(t, "cleanup").complete(mustLeafFailure(t, MechanicalFailure, cleanupErr))

			result := waitResult(t, execution)
			requireStatus(t, result, testCase.wantStatus)
			primary, _ := result.Primary()
			if testCase.wantError != nil && !errors.Is(primary.Error(), testCase.wantError) {
				t.Fatalf("primary = %#v, want body error %v", primary, testCase.wantError)
			}
			if testCase.wantReason != "" {
				if reason, ok := primary.Reason(); !ok || reason != testCase.wantReason {
					t.Fatalf("primary rejection = %#v, want %q", primary, testCase.wantReason)
				}
			}
			cleanupDiagnostic, ok := finallyCleanupDiagnostic(result, testCase.wantStatus == Failed && testCase.wantError == nil)
			if !ok {
				t.Fatalf("result has no cleanup failure diagnostic: %#v", result)
			}
			if kind, ok := cleanupDiagnostic.Failure(); !ok || kind != CleanupFailure || !errors.Is(cleanupDiagnostic.Error(), cleanupErr) {
				t.Fatalf("cleanup diagnostic = %#v", cleanupDiagnostic)
			}
			if _, output := result.Output(); output || trace.index(traceCommit, "root") >= 0 {
				t.Fatal("cleanup failure allowed protected publication")
			}
			settled := boundary.settlements("root")
			if len(settled) != 1 || !reflect.DeepEqual(settled[0], result) {
				t.Fatalf("protected settlement and Wait differ: settled = %#v, returned = %#v", settled, result)
			}
		})
	}
}

// Inheriting the cancelled body context would cancel cleanup immediately;
// dropping the parent entirely would lose values required by cleanup leaves.
func TestFinallyFreshContextPreservesValuesAndIgnoresEarlierExternalCancel(t *testing.T) {
	definition := finallySimpleDefinition(t, testLeaf("body", value.EmptyContract(), value.EmptyContract()))
	trace := &eventTrace{}
	baseRunner := newControlledRunner(trace)
	key := finallyContextKey{}
	runner := &finallyContextRunner{base: baseRunner, key: key, observed: make(chan any, 1)}
	boundary := newRecordingBoundary(trace)
	scheduler, err := New(runner, boundary, Policy{Capacity: 1, CancellationGrace: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), key, "preserved")
	execution, err := scheduler.Start(ctx, definition, emptyValue(t))
	if err != nil {
		t.Fatal(err)
	}
	body := baseRunner.execution(t, "body")
	execution.Cancel()
	waitClosed(t, body.cancelObserved, "body did not observe external cancellation")
	cleanup := baseRunner.execution(t, "cleanup")
	if force, start := trace.index(traceForceStop, "body"), trace.index(traceStart, "cleanup"); force < 0 || start < 0 || force >= start {
		t.Fatalf("cleanup started before noncooperative body was force-stopped: %#v", trace.snapshot())
	}
	select {
	case got := <-runner.observed:
		if got != "preserved" {
			t.Fatalf("cleanup context value = %#v, want preserved", got)
		}
	case <-time.After(testTimeout):
		t.Fatal("cleanup context was not observed")
	}
	assertFinallyOutcomeInput(t, runnerInput(t, baseRunner, "cleanup"), "cancelled")
	assertNotClosed(t, cleanup.cancelObserved, 25*time.Millisecond, "earlier external cancellation leaked into fresh cleanup context")
	cleanup.complete(mustLeafSuccess(t, emptyValue(t)))
	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	if trace.index(traceCommit, "cleanup") < 0 {
		t.Fatal("cleanup did not complete after earlier external cancellation")
	}
	if trace.index(traceCommit, "root") >= 0 {
		t.Fatal("cleanup reset the externally cancelled protected outcome")
	}
}

// Failing to subscribe the fresh context to post-start cancellation or to the
// stronger force signal lets cleanup outlive an explicit later stop.
func TestFinallyLaterExternalCancelAndForceStopInterruptCleanup(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "cancel"
		if force {
			name = "force stop"
		}
		t.Run(name, func(t *testing.T) {
			definition := finallySimpleDefinition(t, testLeaf("body", value.EmptyContract(), value.EmptyContract()))
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			if !force {
				runner.cooperative["cleanup"] = true
			}
			boundary := newRecordingBoundary(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1, CancellationGrace: time.Second})
			runner.execution(t, "body").complete(mustLeafSuccess(t, emptyValue(t)))
			cleanup := runner.execution(t, "cleanup")
			assertFinallyOutcomeInput(t, runnerInput(t, runner, "cleanup"), "succeeded")

			if force {
				execution.ForceStop()
				waitClosed(t, cleanup.forceObserved, "active cleanup leaf was not force-stopped")
			} else {
				execution.Cancel()
				waitClosed(t, cleanup.cancelObserved, "fresh cleanup context missed later cancellation")
			}
			result := waitResult(t, execution)
			requireStatus(t, result, Cancelled)
			primary, _ := result.Primary()
			if !primary.external || !errors.Is(primary.Error(), context.Canceled) {
				t.Fatalf("primary = %#v, want later external cancellation", primary)
			}
			if trace.index(traceCommit, "root") >= 0 {
				t.Fatal("protected graph committed after cleanup interruption")
			}
			settled := boundary.settlements("root")
			if len(settled) != 1 || !reflect.DeepEqual(settled[0], result) {
				t.Fatalf("protected settlement and Wait differ: settled = %#v, returned = %#v", settled, result)
			}
		})
	}
}

// Running outer cleanup when a nested protected child merely returns its body
// result reverses structured stack order.
func TestFinallyNestedScopesUnwindInnermostFirst(t *testing.T) {
	empty := value.EmptyContract()
	inner := workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes:   []workflow.NodeDraft{testLeaf("inner-body", empty, empty)},
		Finally: &workflow.FinallyDraft{Graph: finallyNamedCleanupGraph(t, empty, "inner-cleanup")},
	}
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes:   []workflow.NodeDraft{{Name: "nested", Graph: &inner}},
		Finally: &workflow.FinallyDraft{Graph: finallyNamedCleanupGraph(t, empty, "outer-cleanup")},
	})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newPathRecordingBoundary(trace)
	execution := startPathExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})

	runner.execution(t, "inner-body").complete(mustLeafSuccess(t, emptyValue(t)))
	innerCleanup := runner.execution(t, "inner-cleanup")
	assertNoStart(t, runner, 20*time.Millisecond)
	innerCleanup.complete(mustLeafSuccess(t, emptyValue(t)))
	outerCleanup := runner.execution(t, "outer-cleanup")
	outerCleanup.complete(mustLeafSuccess(t, emptyValue(t)))
	requireStatus(t, waitResult(t, execution), Succeeded)

	events := boundary.snapshot()
	innerPath := Path{}.AuthoredChild("nested")
	innerCleanupPath := innerPath.Cleanup()
	outerCleanupPath := Path{}.Cleanup()
	if innerCleanupCommit, innerCommit, outerCleanupCommit, rootCommit :=
		pathEventIndex(events, traceCommit, innerCleanupPath),
		pathEventIndex(events, traceCommit, innerPath),
		pathEventIndex(events, traceCommit, outerCleanupPath),
		pathEventIndex(events, traceCommit, Path{}); innerCleanupCommit < 0 || innerCommit <= innerCleanupCommit || outerCleanupCommit <= innerCommit || rootCommit <= outerCleanupCommit {
		t.Fatalf("nested unwind/commit order is wrong: %#v", events)
	}
}

type finallyContextKey struct{}

type finallyContextRunner struct {
	base     *controlledRunner
	key      any
	observed chan any
	once     sync.Once
}

func (r *finallyContextRunner) Start(ctx context.Context, request LeafRequest) (LeafExecution, error) {
	if pathContainsCleanup(request.Instance().Path()) {
		r.once.Do(func() { r.observed <- ctx.Value(r.key) })
	}
	return r.base.Start(ctx, request)
}

func pathContainsCleanup(path Path) bool {
	for _, component := range path.Components() {
		if component.IsCleanup() {
			return true
		}
	}
	return false
}

func finallySimpleDefinition(t *testing.T, body workflow.NodeDraft) workflow.Definition {
	t.Helper()
	return finallyDefinition(t, value.EmptyContract(), body, finallyOutcomeInputs(t), []workflow.CleanupBindingDraft{{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupOutcome}, To: "outcome"}})
}

func finallyDefinition(t *testing.T, inputs value.Contract, body workflow.NodeDraft, cleanupInputs value.Contract, bindings []workflow.CleanupBindingDraft) workflow.Definition {
	t.Helper()
	return testDefinition(t, workflow.GraphDraft{
		Inputs: inputs, Outputs: value.EmptyContract(), Nodes: []workflow.NodeDraft{body},
		Finally: &workflow.FinallyDraft{Graph: finallyCleanupGraph(t, cleanupInputs), Bindings: bindings},
	})
}

func finallyCleanupGraph(t *testing.T, inputs value.Contract) workflow.GraphDraft {
	t.Helper()
	return finallyNamedCleanupGraph(t, inputs, "cleanup")
}

func finallyNamedCleanupGraph(t *testing.T, inputs value.Contract, name string) workflow.GraphDraft {
	t.Helper()
	bindings := make([]workflow.BindingDraft, 0, len(inputs.Ports()))
	for _, port := range inputs.Ports() {
		bindings = append(bindings, workflow.BindingDraft{From: []string{port.Name()}, To: port.Name()})
	}
	graph := workflow.GraphDraft{
		Inputs: inputs, Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{testLeaf(name, inputs, value.EmptyContract())},
	}
	if len(bindings) > 0 {
		graph.Edges = []workflow.EdgeDraft{{
			From:     workflow.EndpointDraft{Kind: workflow.Boundary},
			To:       workflow.EndpointDraft{Kind: workflow.Child, Child: name},
			Bindings: bindings,
		}}
	}
	return graph
}

func finallyOutcomeInputs(t *testing.T) value.Contract {
	t.Helper()
	return testContract(t, testRequired(t, "outcome", finallyOutcomeType(t)))
}

func finallyOutcomeType(t *testing.T) value.Type {
	t.Helper()
	literals := make([]value.Literal, 0, 4)
	for _, status := range []string{"succeeded", "rejected", "failed", "cancelled"} {
		literal, err := value.ParseLiteral([]byte(`"` + status + `"`))
		if err != nil {
			t.Fatal(err)
		}
		literals = append(literals, literal)
	}
	typ, err := value.Enum(literals...)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func assertFinallyOutcomeInput(t *testing.T, input value.Value, want string) {
	t.Helper()
	outcome, ok := selectValue(input, []string{"outcome"})
	if !ok {
		t.Fatalf("cleanup input %x omitted outcome", input.Canonical())
	}
	got, ok := outcome.Text()
	if !ok || got != want {
		t.Fatalf("cleanup outcome = %q/%v, want %q", got, ok, want)
	}
}

func finallyCleanupDiagnostic(result Result, primary bool) (Diagnostic, bool) {
	if primary {
		diagnostic, ok := result.Primary()
		return diagnostic, ok
	}
	for _, diagnostic := range result.Secondary() {
		if kind, ok := diagnostic.Failure(); ok && kind == CleanupFailure {
			return diagnostic, true
		}
	}
	return Diagnostic{}, false
}

func waitForTrace(t *testing.T, trace *eventTrace, kind traceKind, child string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for trace.index(kind, child) < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("trace never recorded (%v, %q): %#v", kind, child, trace.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, duration time.Duration, message string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatal(message)
	case <-time.After(duration):
	}
}
