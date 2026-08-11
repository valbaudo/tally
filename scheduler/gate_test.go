package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func TestGateTrueCommitsEmptyObjectWithoutCallingRunner(t *testing.T) {
	definition := gateDefinition(t, true, "")
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
	result := waitResult(t, execution)
	requireStatus(t, result, Succeeded)
	commits := boundary.commits()
	if runner.count() != 0 || len(commits) != 2 || !commits["check"].Equal(emptyValue(t)) || !commits["root"].Equal(emptyValue(t)) {
		t.Fatalf("starts = %d, commits = %#v", runner.count(), commits)
	}
}

func TestGateFalseRejectsWithReasonAndCommitsNothing(t *testing.T) {
	definition := gateDefinition(t, false, "policy denied")
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newRecordingBoundary(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
	result := waitResult(t, execution)
	requireStatus(t, result, Rejected)
	primary, _ := result.Primary()
	reason, ok := primary.Reason()
	if !ok || reason != "policy denied" || runner.count() != 0 || len(boundary.commits()) != 0 {
		t.Fatalf("reason = %q/%v, starts = %d, commits = %#v", reason, ok, runner.count(), boundary.commits())
	}
}

func TestGateOrdinaryLeafMalformedRejectionBecomesMechanicalFailure(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: []workflow.NodeDraft{testLeaf("work", empty, empty)}})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	execution := startTestExecution(t, definition, emptyValue(t), runner, newRecordingBoundary(trace), Policy{Capacity: 1})
	runner.execution(t, "work").complete(LeafCompletion{status: Rejected})
	result := waitResult(t, execution)
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	kind, ok := primary.Failure()
	if !ok || kind != MechanicalFailure {
		t.Fatalf("primary = %#v", primary)
	}
}

func TestFailFastGateRejectionPreservesPrimaryAcrossCancellationResponsiveExits(t *testing.T) {
	for _, phase := range []cancellationPhase{
		cancelLeafEnter,
		cancelGraphEnter,
		cancelRunnerStart,
		cancelLeafCommit,
		cancelGraphCommit,
		cancelGateEnter,
		cancelGateCommit,
	} {
		t.Run(string(phase), func(t *testing.T) {
			trace := &eventTrace{}
			blocked := make(chan struct{})
			boundary := &cancellationResponsiveBoundary{
				phase: phase, blocked: blocked, base: newRecordingBoundary(trace),
			}
			runner := &cancellationResponsiveRunner{phase: phase, blocked: blocked}
			definition := cancellationDefinition(t, phase)
			execution := startCancellationExecution(t, definition, runner, boundary)
			result := waitResult(t, execution)
			if !result.Valid() || result.Status() != Rejected {
				t.Fatalf("status = %v (valid %v), want %v", result.Status(), result.Valid(), Rejected)
			}
			primary, _ := result.Primary()
			if reason, ok := primary.Reason(); !ok || reason != "denied" {
				t.Fatalf("primary = %#v", primary)
			}
			for _, diagnostic := range append([]Diagnostic{primary}, result.Secondary()...) {
				if diagnostic.Status() == Failed {
					t.Fatalf("cancellation-responsive %s became a mechanical failure: %#v", phase, diagnostic)
				}
			}
		})
	}
}

type cancellationPhase string

const (
	cancelLeafEnter   cancellationPhase = "leaf enter"
	cancelGraphEnter  cancellationPhase = "graph enter"
	cancelRunnerStart cancellationPhase = "runner start"
	cancelLeafCommit  cancellationPhase = "leaf commit"
	cancelGraphCommit cancellationPhase = "graph commit"
	cancelGateEnter   cancellationPhase = "gate enter"
	cancelGateCommit  cancellationPhase = "gate commit"
)

type cancellationResponsiveBoundary struct {
	phase          cancellationPhase
	blocked        chan struct{}
	base           *recordingBoundary
	once           sync.Once
	claimedContext context.Context
}

func (b *cancellationResponsiveBoundary) Enter(ctx context.Context, instance Instance) error {
	name := instanceName(instance)
	if name == "deny" {
		<-b.blocked
		return b.base.Enter(ctx, instance)
	}
	workBlocked := (b.phase == cancelLeafEnter || b.phase == cancelGraphEnter) && name == "work"
	gateBlocked := b.phase == cancelGateEnter && name == "allow"
	if workBlocked || gateBlocked {
		b.base.trace.record(traceEvent{kind: traceEnter, child: name})
		b.signalBlocked()
		<-ctx.Done()
		return ctx.Err()
	}
	workCommit := (b.phase == cancelLeafCommit || b.phase == cancelGraphCommit) && name == "work"
	gateCommit := b.phase == cancelGateCommit && name == "allow"
	if workCommit || gateCommit {
		b.claimedContext = ctx
	}
	return b.base.Enter(ctx, instance)
}

func (b *cancellationResponsiveBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	name := instanceName(instance)
	workBlocked := (b.phase == cancelLeafCommit || b.phase == cancelGraphCommit) && name == "work"
	gateBlocked := b.phase == cancelGateCommit && name == "allow"
	if workBlocked || gateBlocked {
		b.signalBlocked()
		// The scope claimed success before entering Commit. Observe the
		// original scope cancellation to prove fail-fast linearized later,
		// then let the protected commit finish successfully.
		<-b.claimedContext.Done()
	}
	return b.base.Commit(ctx, instance, output)
}

func (b *cancellationResponsiveBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func (b *cancellationResponsiveBoundary) signalBlocked() {
	b.once.Do(func() { close(b.blocked) })
}

type cancellationResponsiveRunner struct {
	phase   cancellationPhase
	blocked chan struct{}
	once    sync.Once
}

func (r *cancellationResponsiveRunner) Start(ctx context.Context, _ LeafRequest) (LeafExecution, error) {
	if r.phase == cancelRunnerStart {
		r.once.Do(func() { close(r.blocked) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r.phase != cancelLeafCommit {
		return nil, errors.New("unexpected runner start")
	}
	completion, err := NewLeafSucceeded(mustEmptyValue())
	if err != nil {
		return nil, err
	}
	done := make(chan LeafCompletion, 1)
	done <- completion
	close(done)
	return staticLeafExecution{done: done}, nil
}

type staticLeafExecution struct{ done <-chan LeafCompletion }

func (e staticLeafExecution) Done() <-chan LeafCompletion { return e.done }
func (staticLeafExecution) ForceStop() error              { return nil }

func cancellationDefinition(t *testing.T, phase cancellationPhase) workflow.Definition {
	t.Helper()
	empty := value.EmptyContract()
	deny := testGateNode(t, "deny", false, "denied")
	var sibling workflow.NodeDraft
	switch phase {
	case cancelLeafEnter, cancelRunnerStart, cancelLeafCommit:
		sibling = testLeaf("work", empty, empty)
	case cancelGraphEnter, cancelGraphCommit:
		sibling = workflow.NodeDraft{Name: "work", Graph: &workflow.GraphDraft{Inputs: empty, Outputs: empty}}
	case cancelGateEnter, cancelGateCommit:
		sibling = testGateNode(t, "allow", true, "")
	default:
		t.Fatalf("unknown cancellation phase %q", phase)
	}
	return testDefinition(t, workflow.GraphDraft{Inputs: empty, Outputs: empty, Nodes: []workflow.NodeDraft{deny, sibling}})
}

func startCancellationExecution(t *testing.T, definition workflow.Definition, runner LeafRunner, boundary Boundary) *Execution {
	t.Helper()
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(context.Background(), definition, emptyValue(t))
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func mustEmptyValue() value.Value {
	result, err := value.NewObject()
	if err != nil {
		panic(err)
	}
	return result
}

func gateDefinition(t *testing.T, passed bool, reason string) workflow.Definition {
	t.Helper()
	return testDefinition(t, workflow.GraphDraft{
		Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{testGateNode(t, "check", passed, reason)},
	})
}

func testGateNode(t *testing.T, name string, passed bool, reason string) workflow.NodeDraft {
	t.Helper()
	inputs := testContract(t,
		testRequired(t, "passed", value.Boolean()),
		testOptional(t, "reason", value.String()),
	)
	passedLiteral, err := value.ParseLiteral([]byte(map[bool]string{true: "true", false: "false"}[passed]))
	if err != nil {
		t.Fatal(err)
	}
	literals := []workflow.LiteralBindingDraft{{Input: "passed", Value: passedLiteral}}
	if reason != "" {
		reasonLiteral, parseErr := value.ParseLiteral([]byte(`"` + reason + `"`))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		literals = append(literals, workflow.LiteralBindingDraft{Input: "reason", Value: reasonLiteral})
	}
	return workflow.NodeDraft{Name: name, Leaf: &workflow.LeafDraft{Kind: workflow.Gate, Inputs: inputs, Outputs: value.EmptyContract()}, Literals: literals}
}
