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

func TestFinallyCancellationClaimedBeforeCleanupEntryUsesCancelledOutcome(t *testing.T) {
	definition := finallySimpleDefinition(t, testLeaf("body", value.EmptyContract(), value.EmptyContract()))
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
	claimReached := make(chan struct{})
	var once sync.Once
	execution.control.beforeBodyOutcomeClaim = func(ctx context.Context, path Path) {
		if len(path.Components()) != 0 {
			return
		}
		once.Do(func() { close(claimReached) })
		<-ctx.Done()
	}

	runner.execution(t, "body").complete(mustLeafSuccess(t, emptyValue(t)))
	waitClosed(t, claimReached, "protected body did not reach its cleanup-outcome claim")
	execution.Cancel()
	cleanup := runner.execution(t, "cleanup")
	assertFinallyOutcomeInput(t, runnerInput(t, runner, "cleanup"), "cancelled")
	cleanup.complete(mustLeafSuccess(t, emptyValue(t)))

	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	if trace.index(traceCommit, "root") >= 0 {
		t.Fatal("protected graph committed after cancellation won cleanup-outcome capture")
	}
	settled := boundary.settlements("root")
	if len(settled) != 1 || !reflect.DeepEqual(settled[0], result) {
		t.Fatalf("protected settlement and Wait differ: settled = %#v, returned = %#v", settled, result)
	}
	if countExternalCancellations(result) != 1 {
		t.Fatalf("external cancellation diagnostics = %d, want exactly one: %#v", countExternalCancellations(result), result)
	}
	if diagnostics := resultDiagnostics(result); len(diagnostics) != 1 {
		t.Fatalf("cancel-first cleanup outcome produced duplicate diagnostics: %#v", diagnostics)
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
// zero-value filling, or a success that lost its terminal claim changes this
// exact closed cleanup request.
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
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, input, runner, boundary, Policy{Capacity: 2, CancellationGrace: time.Second})
	runner.waitStarts(t, 2)
	committed := runner.execution(t, "committed")
	runner.execution(t, "failed").complete(mustLeafFailure(t, MechanicalFailure, errBoom))
	waitClosed(t, committed.cancelObserved, "still-active committed child did not observe fail-fast")
	committed.complete(mustLeafSuccess(t, committedOutput))

	cleanup := runner.execution(t, "cleanup")
	want := testValue(t, map[string]value.Value{
		"original": exact,
		"outcome":  value.NewString("failed"),
	})
	if got := runnerInput(t, runner, "cleanup"); !got.Equal(want) {
		t.Fatalf("cleanup input = %x, want exact closed input %x", got.Canonical(), want.Canonical())
	}
	if trace.index(traceStart, "never") >= 0 {
		t.Fatal("unstarted child ran or contributed cleanup data")
	}
	if _, published := boundary.commits()["committed"]; published {
		t.Fatal("cancel-first child published and contributed an unclaimed success")
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

func TestFinallyPreservesEveryConcurrentCleanupFailureCause(t *testing.T) {
	empty := value.EmptyContract()
	cleanupInputs := finallyOutcomeInputs(t)
	cleanupGraph := workflow.GraphDraft{
		Inputs: cleanupInputs, Outputs: empty,
		Nodes: []workflow.NodeDraft{
			testLeaf("cleanup-failure", empty, empty),
			testLeaf("cleanup-timeout", empty, empty),
		},
	}
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("body", empty, empty)},
		Finally: &workflow.FinallyDraft{
			Graph: cleanupGraph,
			Bindings: []workflow.CleanupBindingDraft{{
				From: workflow.CleanupSourceDraft{Kind: workflow.CleanupOutcome}, To: "outcome",
			}},
		},
	})
	bodyErr := errors.New("protected body failed")
	cleanupErr := errors.New("cleanup intrinsic failure")
	timeoutErr := errors.New("cleanup timed out")

	for _, bodyFailure := range []bool{false, true} {
		name := "body success"
		if bodyFailure {
			name = "body failure"
		}
		t.Run(name, func(t *testing.T) {
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			boundary := newRecordingBoundary(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2, CancellationGrace: time.Second})
			body := runner.execution(t, "body")
			if bodyFailure {
				body.complete(mustLeafFailure(t, MechanicalFailure, bodyErr))
			} else {
				body.complete(mustLeafSuccess(t, emptyValue(t)))
			}
			cleanupNames := runner.waitStarts(t, 2)
			cleanup := make(map[string]*controlledLeafExecution, len(cleanupNames))
			for _, cleanupName := range cleanupNames {
				cleanup[cleanupName] = runner.execution(t, cleanupName)
			}
			cleanup["cleanup-failure"].complete(mustLeafFailure(t, MechanicalFailure, cleanupErr))
			waitClosed(t, cleanup["cleanup-timeout"].cancelObserved, "cleanup timeout sibling did not observe fail-fast")
			cleanup["cleanup-timeout"].complete(mustLeafTimeout(t, timeoutErr))

			result := waitResult(t, execution)
			requireStatus(t, result, Failed)
			diagnostics := resultDiagnostics(result)
			wantCount := 3
			if bodyFailure {
				wantCount = 4
				primary, _ := result.Primary()
				if !errors.Is(primary.Error(), bodyErr) || comparePath(primary.Path(), Path{}.AuthoredChild("body")) != 0 {
					t.Fatalf("body primary = %#v, want exact protected failure", primary)
				}
			}
			if len(diagnostics) != wantCount {
				t.Fatalf("terminal diagnostics = %#v, want aggregate plus both distinct cleanup causes", diagnostics)
			}

			cleanupPath := Path{}.Cleanup()
			failurePath := cleanupPath.AuthoredChild("cleanup-failure")
			timeoutPath := cleanupPath.AuthoredChild("cleanup-timeout")
			cleanupOffset := 0
			if bodyFailure {
				cleanupOffset = 1
			}
			orderedPaths := []Path{cleanupPath, failurePath, timeoutPath}
			orderedKinds := []FailureKind{CleanupFailure, MechanicalFailure, TimeoutFailure}
			for index := range orderedPaths {
				diagnostic := diagnostics[cleanupOffset+index]
				kind, failed := diagnostic.Failure()
				if !failed || kind != orderedKinds[index] || comparePath(diagnostic.Path(), orderedPaths[index]) != 0 {
					t.Fatalf("cleanup diagnostic %d = %#v, want kind %v at %#v", index, diagnostic, orderedKinds[index], orderedPaths[index].Components())
				}
			}
			aggregateFound, failureFound, timeoutFound := false, false, false
			parentCancellations := 0
			for _, diagnostic := range diagnostics {
				kind, failed := diagnostic.Failure()
				switch {
				case failed && kind == CleanupFailure && comparePath(diagnostic.Path(), cleanupPath) == 0:
					aggregateFound = errors.Is(diagnostic.Error(), cleanupErr)
				case failed && kind == MechanicalFailure && comparePath(diagnostic.Path(), failurePath) == 0:
					failureFound = diagnostic.cleanup && errors.Is(diagnostic.Error(), cleanupErr)
				case failed && kind == TimeoutFailure && comparePath(diagnostic.Path(), timeoutPath) == 0:
					timeoutFound = diagnostic.cleanup && errors.Is(diagnostic.Error(), timeoutErr)
				}
				if diagnostic.parentCancelled || diagnostic.external {
					parentCancellations++
				}
			}
			if !aggregateFound || !failureFound || !timeoutFound || parentCancellations != 0 {
				t.Fatalf("cleanup diagnostics = %#v, aggregate/failure/timeout/cancellations = %v/%v/%v/%d", diagnostics, aggregateFound, failureFound, timeoutFound, parentCancellations)
			}
			if _, output := result.Output(); output || trace.index(traceCommit, "root") >= 0 {
				t.Fatal("cleanup failures exposed or committed a protected output")
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

// Reusing the protected run's unversioned external fact inside cleanup rewrites
// the intrinsic cleanup failure to context.Canceled before precedence sees it.
func TestFinallyHistoricalExternalCancelPreservesIntrinsicCleanupFailure(t *testing.T) {
	definition := finallySimpleDefinition(t, testLeaf("body", value.EmptyContract(), value.EmptyContract()))
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	runner.cooperative["body"] = true
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})

	body := runner.execution(t, "body")
	execution.Cancel()
	waitClosed(t, body.cancelObserved, "body did not observe external cancellation")
	cleanupErr := errors.New("intrinsic cleanup failure")
	runner.execution(t, "cleanup").complete(mustLeafFailure(t, MechanicalFailure, cleanupErr))

	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	primary, _ := result.Primary()
	if !primary.external || !errors.Is(primary.Error(), context.Canceled) {
		t.Fatalf("primary = %#v, want historical external cancellation", primary)
	}
	cleanupFailure, ok := finallyCleanupDiagnostic(result, false)
	if !ok || !errors.Is(cleanupFailure.Error(), cleanupErr) || errors.Is(cleanupFailure.Error(), context.Canceled) {
		t.Fatalf("cleanup failure = %#v/%v, want intrinsic error %v and not context cancellation", cleanupFailure, ok, cleanupErr)
	}
	cleanupSettlements := boundary.settlements("cleanup")
	if len(cleanupSettlements) != 1 {
		t.Fatalf("cleanup leaf settlements = %d, want 1", len(cleanupSettlements))
	}
	requireStatus(t, cleanupSettlements[0], Failed)
	cleanupPrimary, _ := cleanupSettlements[0].Primary()
	if !errors.Is(cleanupPrimary.Error(), cleanupErr) || cleanupPrimary.external {
		t.Fatalf("cleanup leaf settlement = %#v, want intrinsic cleanup failure", cleanupSettlements[0])
	}
	rootSettlements := boundary.settlements("root")
	if len(rootSettlements) != 1 || !reflect.DeepEqual(rootSettlements[0], result) {
		t.Fatalf("protected settlement and Wait differ: settled = %#v, returned = %#v", rootSettlements, result)
	}
}

// Omitting the cleanup-entry parent snapshot leaves active cleanup insulated
// from a sibling's later fail-fast cancellation and can let that consequence
// replace the sibling's rejection through CleanupFailure precedence.
func TestFinallyLaterAncestorCancellationInterruptsCleanupWithoutChangingSiblingPrimary(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "failure"
		if rejected {
			name = "rejection"
		}
		t.Run(name, func(t *testing.T) {
			definition := finallySiblingDefinition(t, rejected)
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			runner.cooperative["protected-cleanup"] = true
			boundary := newRecordingBoundary(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2})
			runner.waitStarts(t, 2)

			runner.execution(t, "protected-body").complete(mustLeafSuccess(t, emptyValue(t)))
			cleanup := runner.execution(t, "protected-cleanup")
			siblingErr := errors.New("sibling failed")
			if rejected {
				runner.execution(t, "trigger").complete(mustLeafSuccess(t, emptyValue(t)))
			} else {
				runner.execution(t, "trigger").complete(mustLeafFailure(t, MechanicalFailure, siblingErr))
			}
			waitClosed(t, cleanup.cancelObserved, "active cleanup did not observe later ancestor cancellation")

			result := waitResult(t, execution)
			primary, _ := result.Primary()
			if rejected {
				requireStatus(t, result, Rejected)
				if reason, ok := primary.Reason(); !ok || reason != "sibling rejected" {
					t.Fatalf("primary = %#v, want sibling rejection", primary)
				}
			} else {
				requireStatus(t, result, Failed)
				if !errors.Is(primary.Error(), siblingErr) {
					t.Fatalf("primary = %#v, want sibling failure %v", primary, siblingErr)
				}
			}
			if diagnostic, ok := anyCleanupFailure(result); ok {
				t.Fatalf("ancestor cancellation became cleanup failure %#v", diagnostic)
			}
			protectedSettlements := boundary.settlements("protected")
			if len(protectedSettlements) != 1 {
				t.Fatalf("protected settlements = %d, want 1", len(protectedSettlements))
			}
			requireStatus(t, protectedSettlements[0], Cancelled)
			protectedPrimary, _ := protectedSettlements[0].Primary()
			if !protectedPrimary.parentCancelled || protectedPrimary.external {
				t.Fatalf("protected settlement = %#v, want parent-induced cancellation", protectedSettlements[0])
			}
			rootSettlements := boundary.settlements("root")
			if len(rootSettlements) != 1 || !reflect.DeepEqual(rootSettlements[0], result) {
				t.Fatalf("root settlement and Wait differ: settled = %#v, returned = %#v", rootSettlements, result)
			}
			runner.mu.Lock()
			active := runner.active
			runner.mu.Unlock()
			if active != 0 {
				t.Fatalf("active runner leaves after quiescence = %d, want 0", active)
			}
		})
	}
}

// Inferring a cleanup interruption only from the normalized cleanup primary
// lets a noncooperative cleanup's mechanical or timeout completion outrank the
// parent cancellation that first arrived while cleanup was active.
func TestFinallyLaterAncestorCancellationOutranksCleanupFailure(t *testing.T) {
	tests := []struct {
		name            string
		rejectedSibling bool
		failure         FailureKind
	}{
		{name: "mechanical failure with failed sibling", failure: MechanicalFailure},
		{name: "timeout with rejected sibling", rejectedSibling: true, failure: TimeoutFailure},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			definition := finallySiblingDefinition(t, testCase.rejectedSibling)
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			boundary := newRecordingBoundary(trace)
			execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2, CancellationGrace: time.Second})
			runner.waitStarts(t, 2)

			runner.execution(t, "protected-body").complete(mustLeafSuccess(t, emptyValue(t)))
			cleanup := runner.execution(t, "protected-cleanup")
			siblingErr := errors.New("sibling failed")
			if testCase.rejectedSibling {
				runner.execution(t, "trigger").complete(mustLeafSuccess(t, emptyValue(t)))
			} else {
				runner.execution(t, "trigger").complete(mustLeafFailure(t, MechanicalFailure, siblingErr))
			}
			waitClosed(t, cleanup.cancelObserved, "active cleanup did not observe later ancestor cancellation")

			cleanupErr := errors.New("cleanup ended after parent cancellation")
			if testCase.failure == TimeoutFailure {
				completion, err := NewLeafTimeout(cleanupErr)
				if err != nil {
					t.Fatal(err)
				}
				cleanup.complete(completion)
			} else {
				cleanup.complete(mustLeafFailure(t, testCase.failure, cleanupErr))
			}

			result := waitResult(t, execution)
			rootPrimary, _ := result.Primary()
			if testCase.rejectedSibling {
				requireStatus(t, result, Rejected)
				if reason, ok := rootPrimary.Reason(); !ok || reason != "sibling rejected" {
					t.Fatalf("root primary = %#v, want sibling rejection", rootPrimary)
				}
			} else {
				requireStatus(t, result, Failed)
				if !errors.Is(rootPrimary.Error(), siblingErr) {
					t.Fatalf("root primary = %#v, want sibling failure %v", rootPrimary, siblingErr)
				}
			}
			if diagnostic, ok := anyCleanupFailure(result); ok {
				t.Fatalf("later parent cancellation became cleanup failure %#v", diagnostic)
			}
			rootParentCancellations := 0
			for _, diagnostic := range resultDiagnostics(result) {
				if diagnostic.parentCancelled && !diagnostic.external {
					rootParentCancellations++
				}
			}
			if rootParentCancellations != 1 {
				t.Fatalf("root parent-cancellation diagnostics = %d, want 1: %#v", rootParentCancellations, result)
			}

			protectedSettlements := boundary.settlements("protected")
			if len(protectedSettlements) != 1 {
				t.Fatalf("protected settlements = %d, want 1", len(protectedSettlements))
			}
			protected := protectedSettlements[0]
			requireStatus(t, protected, Cancelled)
			protectedPrimary, _ := protected.Primary()
			if !protectedPrimary.parentCancelled || protectedPrimary.external {
				t.Fatalf("protected primary = %#v, want parent-induced cancellation", protectedPrimary)
			}
			parentCancellations := 0
			cleanupCauseSecondary := false
			for _, diagnostic := range resultDiagnostics(protected) {
				if diagnostic.parentCancelled && !diagnostic.external {
					parentCancellations++
				}
			}
			for _, diagnostic := range protected.Secondary() {
				kind, failed := diagnostic.Failure()
				if failed && kind == testCase.failure && errors.Is(diagnostic.Error(), cleanupErr) {
					cleanupCauseSecondary = true
				}
			}
			if parentCancellations != 1 {
				t.Fatalf("protected parent-cancellation diagnostics = %d, want 1: %#v", parentCancellations, protected)
			}
			if !cleanupCauseSecondary {
				t.Fatalf("protected secondaries = %#v, want cleanup %v cause %v", protected.Secondary(), testCase.failure, cleanupErr)
			}

			rootSettlements := boundary.settlements("root")
			if len(rootSettlements) != 1 || !reflect.DeepEqual(rootSettlements[0], result) {
				t.Fatalf("root settlement and Wait differ: settled = %#v, returned = %#v", rootSettlements, result)
			}
		})
	}
}

// Treating every cancelled parent as new cleanup cancellation prevents a
// protected child from unwinding after the parent's fail-fast has already
// cancelled its body.
func TestFinallyHistoricalAncestorCancellationStillRunsCleanup(t *testing.T) {
	definition := finallySiblingDefinition(t, false)
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	runner.cooperative["protected-body"] = true
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2})
	runner.waitStarts(t, 2)

	siblingErr := errors.New("sibling failed before cleanup")
	body := runner.execution(t, "protected-body")
	runner.execution(t, "trigger").complete(mustLeafFailure(t, MechanicalFailure, siblingErr))
	waitClosed(t, body.cancelObserved, "protected body did not observe parent fail-fast")
	cleanup := runner.execution(t, "protected-cleanup")
	if cleanupContextErr := finallyRunnerContextError(cleanup); cleanupContextErr != nil {
		t.Fatalf("cleanup inherited historical parent cancellation: %v", cleanupContextErr)
	}
	cleanup.complete(mustLeafSuccess(t, emptyValue(t)))

	result := waitResult(t, execution)
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	if !errors.Is(primary.Error(), siblingErr) {
		t.Fatalf("primary = %#v, want sibling failure %v", primary, siblingErr)
	}
	if trace.index(traceCommit, "protected-cleanup") < 0 {
		t.Fatal("historically cancelled protected body did not complete cleanup")
	}
	if diagnostic, ok := anyCleanupFailure(result); ok {
		t.Fatalf("historical parent cancellation became cleanup failure %#v", diagnostic)
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

func anyCleanupFailure(result Result) (Diagnostic, bool) {
	diagnostics := result.Secondary()
	if primary, ok := result.Primary(); ok {
		diagnostics = append([]Diagnostic{primary}, diagnostics...)
	}
	for _, diagnostic := range diagnostics {
		if kind, ok := diagnostic.Failure(); ok && kind == CleanupFailure {
			return diagnostic, true
		}
	}
	return Diagnostic{}, false
}

func finallySiblingDefinition(t *testing.T, rejected bool) workflow.Definition {
	t.Helper()
	empty := value.EmptyContract()
	protected := workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes:   []workflow.NodeDraft{testLeaf("protected-body", empty, empty)},
		Finally: &workflow.FinallyDraft{Graph: finallyNamedCleanupGraph(t, empty, "protected-cleanup")},
	}
	sibling := workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{testLeaf("trigger", empty, empty)},
	}
	if rejected {
		sibling.Nodes = append(sibling.Nodes, testGateNode(t, "deny", false, "sibling rejected"))
		sibling.Edges = []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Child, Child: "trigger"},
			To:   workflow.EndpointDraft{Kind: workflow.Child, Child: "deny"},
		}}
	}
	return testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{
			{Name: "protected", Graph: &protected},
			{Name: "sibling", Graph: &sibling},
		},
	})
}

func finallyRunnerContextError(execution *controlledLeafExecution) error {
	select {
	case <-execution.cancelObserved:
		return context.Canceled
	default:
		return nil
	}
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
