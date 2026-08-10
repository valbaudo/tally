package scheduler

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func TestLoopRejectsInvalidRuntimeInputBeforeBoundaryEntry(t *testing.T) {
	definition := loopDefinition(t, 2, false)
	scope, ok := definition.Root().Nodes()[0].Scope()
	if !ok {
		t.Fatal("compiled loop node has no scope")
	}

	trace := &eventTrace{}
	runner := newLoopTestRunner()
	boundary := newPathRecordingBoundary(trace)
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &runState{scheduler: scheduler, control: newRunController(cancel)}
	result, handled := run.runScope(ctx, Path{}.AuthoredChild("repeat"), scope, value.NewString("not a loop input object"))

	if !handled {
		t.Fatal("loop scope was not dispatched")
	}
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	if kind, ok := primary.Failure(); !ok || kind != ContractFailure {
		t.Fatalf("primary = %#v, want contract failure", primary)
	}
	if len(boundary.snapshot()) != 0 || runner.count() != 0 {
		t.Fatalf("events = %#v, starts = %d; invalid input must fail before loop entry", boundary.snapshot(), runner.count())
	}
}

func TestLoopCarriesExactSemanticDataAndStopsOnTrueVerdict(t *testing.T) {
	definition := loopDefinition(t, 5, false)
	initial := loopInitialValue(t)
	runner := newLoopTestRunner()
	boundary := newPathRecordingBoundary(&eventTrace{})
	execution := startLoopTestExecution(t, context.Background(), definition, initial, runner, boundary)

	first := runner.waitStart(t)
	assertLoopStart(t, first, 1, testValue(t, map[string]value.Value{
		"initial":   initial,
		"iteration": mustTestInteger(t, 1),
	}))
	assertNoLoopStart(t, runner, 25*time.Millisecond)
	firstOutput := loopOutput(t, "first complete revision", false, true)
	first.execution.complete(mustLeafSuccess(t, firstOutput))

	second := runner.waitStart(t)
	assertLoopStart(t, second, 2, testValue(t, map[string]value.Value{
		"initial":   initial,
		"previous":  firstOutput,
		"iteration": mustTestInteger(t, 2),
	}))
	secondOutput := loopOutput(t, "accepted complete revision", true, true)
	second.execution.complete(mustLeafSuccess(t, secondOutput))

	result := waitResult(t, execution)
	requireStatus(t, result, Succeeded)
	output, ok := result.Output()
	if !ok || !output.Equal(secondOutput) {
		t.Fatalf("loop output = %x/%v, want true-verdict output %x", output.Canonical(), ok, secondOutput.Canonical())
	}
	if runner.count() != 2 {
		t.Fatalf("runner starts = %d, want exactly 2 semantic iterations", runner.count())
	}
	loopPath := Path{}.AuthoredChild("repeat")
	committed, ok := committedAt(boundary.snapshot(), loopPath)
	if !ok || !committed.Equal(secondOutput) || countPathEvents(boundary.snapshot(), traceCommit, loopPath) != 1 {
		t.Fatalf("loop commit = %x/%v, count %d; want one exact commit %x", committed.Canonical(), ok, countPathEvents(boundary.snapshot(), traceCommit, loopPath), secondOutput.Canonical())
	}
}

func TestLoopFalseVerdictExhaustionCommitsFinalSuccessfulOutput(t *testing.T) {
	t.Run("exact maximum", func(t *testing.T) {
		definition := loopDefinition(t, 3, false)
		initial := loopInitialValue(t)
		runner := newLoopTestRunner()
		boundary := newPathRecordingBoundary(&eventTrace{})
		execution := startLoopTestExecution(t, context.Background(), definition, initial, runner, boundary)

		var previous value.Value
		for iteration := 1; iteration <= 3; iteration++ {
			start := runner.waitStart(t)
			wantFields := map[string]value.Value{
				"initial":   initial,
				"iteration": mustTestInteger(t, iteration),
			}
			if iteration > 1 {
				wantFields["previous"] = previous
			}
			assertLoopStart(t, start, uint64(iteration), testValue(t, wantFields))
			assertNoLoopStart(t, runner, 15*time.Millisecond)
			previous = loopOutput(t, "revision "+strconv.Itoa(iteration), false, true)
			start.execution.complete(mustLeafSuccess(t, previous))
		}

		result := waitResult(t, execution)
		requireStatus(t, result, Succeeded)
		output, ok := result.Output()
		if !ok || !output.Equal(previous) {
			t.Fatalf("exhausted loop output = %x/%v, want final false output %x", output.Canonical(), ok, previous.Canonical())
		}
		if runner.count() != 3 {
			t.Fatalf("runner starts = %d, want authored maximum 3", runner.count())
		}
		loopPath := Path{}.AuthoredChild("repeat")
		if committed, ok := committedAt(boundary.snapshot(), loopPath); !ok || !committed.Equal(previous) {
			t.Fatalf("loop commit = %x/%v, want exhausted final output %x", committed.Canonical(), ok, previous.Canonical())
		}
	})

	t.Run("maximum one", func(t *testing.T) {
		definition := loopDefinition(t, 1, false)
		initial := loopInitialValue(t)
		runner := newLoopTestRunner()
		boundary := newPathRecordingBoundary(&eventTrace{})
		execution := startLoopTestExecution(t, context.Background(), definition, initial, runner, boundary)

		start := runner.waitStart(t)
		assertLoopStart(t, start, 1, testValue(t, map[string]value.Value{
			"initial":   initial,
			"iteration": mustTestInteger(t, 1),
		}))
		final := loopOutput(t, "one and done", false, true)
		start.execution.complete(mustLeafSuccess(t, final))

		result := waitResult(t, execution)
		requireStatus(t, result, Succeeded)
		output, ok := result.Output()
		if !ok || !output.Equal(final) || runner.count() != 1 {
			t.Fatalf("maximum-one result = %x/%v with %d starts, want %x and one start", output.Canonical(), ok, runner.count(), final.Canonical())
		}
	})
}

func TestLoopSecondIterationNonSuccessPropagatesWithoutOlderRevisionOrNextIteration(t *testing.T) {
	timeoutErr := errors.New("iteration timed out")
	timeout, err := NewLeafTimeout(timeoutErr)
	if err != nil {
		t.Fatal(err)
	}
	intrinsicCancelErr := errors.New("backend cancelled iteration")
	intrinsicCancelled, err := NewLeafCancelled(intrinsicCancelErr)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		completion  LeafCompletion
		wantStatus  Status
		wantFailure FailureKind
		wantError   error
		passed      bool
	}{
		{name: "failure", completion: mustLeafFailure(t, MechanicalFailure, errBoom), wantStatus: Failed, wantFailure: MechanicalFailure, wantError: errBoom, passed: true},
		{name: "rejection", completion: mustLeafSuccess(t, loopOutput(t, "rejected revision", false, false)), wantStatus: Rejected, passed: false},
		{name: "timeout", completion: timeout, wantStatus: Failed, wantFailure: TimeoutFailure, wantError: timeoutErr, passed: true},
		{name: "intrinsic cancellation", completion: intrinsicCancelled, wantStatus: Cancelled, wantError: intrinsicCancelErr, passed: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			definition := loopDefinition(t, 4, true)
			initial := loopInitialValue(t)
			runner := newLoopTestRunner()
			boundary := newPathRecordingBoundary(&eventTrace{})
			execution := startLoopTestExecution(t, context.Background(), definition, initial, runner, boundary)

			first := runner.waitStart(t)
			assertLoopStart(t, first, 1, testValue(t, map[string]value.Value{
				"initial":   initial,
				"iteration": mustTestInteger(t, 1),
			}))
			prior := loopOutput(t, "durable but private prior revision", false, true)
			first.execution.complete(mustLeafSuccess(t, prior))

			second := runner.waitStart(t)
			assertLoopStart(t, second, 2, testValue(t, map[string]value.Value{
				"initial":   initial,
				"previous":  prior,
				"iteration": mustTestInteger(t, 2),
			}))
			second.execution.complete(testCase.completion)

			result := waitResult(t, execution)
			requireStatus(t, result, testCase.wantStatus)
			if _, public := result.Output(); public {
				t.Fatal("non-successful loop exposed an older successful revision")
			}
			if runner.count() != 2 {
				t.Fatalf("runner starts = %d, want no semantic iteration after the non-success", runner.count())
			}
			primary, _ := result.Primary()
			if iteration, ok := pathLoopIteration(primary.Path()); !ok || iteration != 2 {
				t.Fatalf("primary path = %#v, want semantic iteration 2", primary.Path().Components())
			}
			if testCase.wantFailure != 0 {
				failure, ok := primary.Failure()
				if !ok || failure != testCase.wantFailure {
					t.Fatalf("failure = %v/%v, want %v", failure, ok, testCase.wantFailure)
				}
			}
			if testCase.wantError != nil && !errors.Is(primary.Error(), testCase.wantError) {
				t.Fatalf("primary error = %v, want %v", primary.Error(), testCase.wantError)
			}
			loopPath := Path{}.AuthoredChild("repeat")
			firstPath := mustLoopPath(t, loopPath, 1)
			if !hasPathEvent(boundary.snapshot(), traceCommit, firstPath) {
				t.Fatal("the first successful body revision was not internally committed")
			}
			if hasPathEvent(boundary.snapshot(), traceCommit, loopPath) || hasPathEvent(boundary.snapshot(), traceCommit, Path{}) {
				t.Fatal("non-successful loop or containing graph published an output")
			}
			if hasLoopIterationEvent(boundary.snapshot(), 3) {
				t.Fatal("loop instantiated a semantic iteration after non-success")
			}
		})
	}
}

func TestLoopMalformedTerminationVerdictIsContractFailure(t *testing.T) {
	definition := loopDefinition(t, 2, false)
	runner := newLoopTestRunner()
	boundary := newPathRecordingBoundary(&eventTrace{})
	execution := startLoopTestExecution(t, context.Background(), definition, loopInitialValue(t), runner, boundary)

	start := runner.waitStart(t)
	malformed := testValue(t, map[string]value.Value{
		"candidate": value.NewString("malformed"),
		"passed":    value.NewBoolean(true),
		"review": testValue(t, map[string]value.Value{
			"done": value.NewString("not a boolean"),
			"note": value.NewString("runner claimed success"),
		}),
	})
	start.execution.complete(mustLeafSuccess(t, malformed))

	result := waitResult(t, execution)
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	if failure, ok := primary.Failure(); !ok || failure != ContractFailure {
		t.Fatalf("primary = %#v, want contract failure", primary)
	}
	if runner.count() != 1 || hasPathEvent(boundary.snapshot(), traceCommit, Path{}.AuthoredChild("repeat")) {
		t.Fatalf("starts = %d, events = %#v; malformed verdict must stop without loop commit", runner.count(), boundary.snapshot())
	}
}

func TestLoopExternalCancellationOutranksIterationFailure(t *testing.T) {
	definition := loopDefinition(t, 3, false)
	runner := newLoopTestRunner()
	boundary := newPathRecordingBoundary(&eventTrace{})
	execution := startLoopTestExecution(t, context.Background(), definition, loopInitialValue(t), runner, boundary)

	first := runner.waitStart(t)
	first.execution.complete(mustLeafSuccess(t, loopOutput(t, "first", false, true)))
	second := runner.waitStart(t)
	execution.Cancel()
	waitClosed(t, second.execution.cancelObserved, "second loop iteration did not observe external cancellation")
	second.execution.complete(mustLeafFailure(t, MechanicalFailure, errors.New("failure during cancellation")))

	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	primary, _ := result.Primary()
	if comparePath(primary.Path(), Path{}) != 0 {
		t.Fatalf("primary path = %#v, want external root cancellation", primary.Path().Components())
	}
	if runner.count() != 2 || hasPathEvent(boundary.snapshot(), traceCommit, Path{}.AuthoredChild("repeat")) {
		t.Fatalf("starts = %d, events = %#v; cancellation must stop the loop without publication", runner.count(), boundary.snapshot())
	}
	foundIterationFailure := false
	for _, secondary := range result.Secondary() {
		failure, failed := secondary.Failure()
		iteration, iterated := pathLoopIteration(secondary.Path())
		if failed && failure == MechanicalFailure && iterated && iteration == 2 {
			foundIterationFailure = true
		}
	}
	if !foundIterationFailure {
		t.Fatalf("secondary diagnostics = %#v, want iteration-two mechanical failure", result.Secondary())
	}
}

func TestLoopCancellationAfterFinalBodyCommitPublishesNoLoopRevision(t *testing.T) {
	definition := loopDefinition(t, 1, false)
	initial := loopInitialValue(t)
	trace := &eventTrace{}
	base := newPathRecordingBoundary(trace)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boundary := &cancelAfterLoopBodyCommitBoundary{base: base, cancel: cancel}
	runner := newLoopTestRunner()
	execution := startLoopTestExecution(t, ctx, definition, initial, runner, boundary)

	start := runner.waitStart(t)
	final := loopOutput(t, "completed before cancellation", false, true)
	start.execution.complete(mustLeafSuccess(t, final))

	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	loopPath := Path{}.AuthoredChild("repeat")
	bodyPath := mustLoopPath(t, loopPath, 1)
	if !hasPathEvent(base.snapshot(), traceCommit, bodyPath) {
		t.Fatal("final body did not commit before the injected cancellation")
	}
	if hasPathEvent(base.snapshot(), traceCommit, loopPath) || hasPathEvent(base.snapshot(), traceCommit, Path{}) {
		t.Fatal("loop or root published after cancellation became visible during final fan-in")
	}
	settled := base.base.settlements("repeat")
	if len(settled) != 1 || settled[0].Status() != Cancelled {
		t.Fatalf("loop settlements = %#v, want one cancelled result", settled)
	}
	if _, published := settled[0].Output(); published {
		t.Fatal("cancelled loop settlement exposed an output")
	}
}

type loopStart struct {
	path      Path
	input     value.Value
	execution *loopTestExecution
}

type loopTestRunner struct {
	mu      sync.Mutex
	started chan loopStart
	starts  []loopStart
}

func newLoopTestRunner() *loopTestRunner {
	return &loopTestRunner{started: make(chan loopStart, 32)}
}

func (r *loopTestRunner) Start(ctx context.Context, request LeafRequest) (LeafExecution, error) {
	execution := &loopTestExecution{
		done: make(chan LeafCompletion, 1), cancelObserved: make(chan struct{}), forceObserved: make(chan struct{}),
	}
	start := loopStart{path: request.Instance().Path(), input: request.Input(), execution: execution}
	r.mu.Lock()
	r.starts = append(r.starts, start)
	r.mu.Unlock()
	r.started <- start
	go func() {
		<-ctx.Done()
		execution.cancelOnce.Do(func() { close(execution.cancelObserved) })
	}()
	return execution, nil
}

func (r *loopTestRunner) waitStart(t *testing.T) loopStart {
	t.Helper()
	select {
	case start := <-r.started:
		return start
	case <-time.After(testTimeout):
		t.Fatal("loop body leaf did not start")
		return loopStart{}
	}
}

func (r *loopTestRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

type loopTestExecution struct {
	done           chan LeafCompletion
	cancelObserved chan struct{}
	forceObserved  chan struct{}
	finishOnce     sync.Once
	cancelOnce     sync.Once
	forceOnce      sync.Once
}

func (e *loopTestExecution) Done() <-chan LeafCompletion { return e.done }

func (e *loopTestExecution) ForceStop() error {
	e.forceOnce.Do(func() {
		close(e.forceObserved)
		e.finishOnce.Do(func() { close(e.done) })
	})
	return nil
}

func (e *loopTestExecution) complete(completion LeafCompletion) {
	e.finishOnce.Do(func() {
		e.done <- completion
		close(e.done)
	})
}

type cancelAfterLoopBodyCommitBoundary struct {
	base   *pathRecordingBoundary
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelAfterLoopBodyCommitBoundary) Enter(ctx context.Context, instance Instance) error {
	return b.base.Enter(ctx, instance)
}

func (b *cancelAfterLoopBodyCommitBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	if err := b.base.Commit(ctx, instance, output); err != nil {
		return err
	}
	components := instance.Path().Components()
	if len(components) > 0 && components[len(components)-1].Kind() == LoopIterationComponent {
		b.once.Do(b.cancel)
		<-ctx.Done()
	}
	return nil
}

func (b *cancelAfterLoopBodyCommitBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func loopDefinition(t *testing.T, maximum int, withGate bool) workflow.Definition {
	t.Helper()
	inputs := loopInputContract(t)
	outputs := loopOutputContract(t)
	bodyInputs := testContract(t,
		testRequired(t, "initial", inputs.ObjectType()),
		testOptional(t, "previous", outputs.ObjectType()),
		testRequired(t, "iteration", value.Integer()),
	)
	body := workflow.GraphDraft{
		Inputs: bodyInputs, Outputs: outputs,
		Nodes: []workflow.NodeDraft{testLeaf("revise", bodyInputs, outputs)},
		Edges: []workflow.EdgeDraft{
			{
				From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "revise"},
				Bindings: []workflow.BindingDraft{
					{From: []string{"initial"}, To: "initial"},
					{From: []string{"previous"}, To: "previous"},
					{From: []string{"iteration"}, To: "iteration"},
				},
			},
			{
				From: workflow.EndpointDraft{Kind: workflow.Child, Child: "revise"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
				Bindings: contractBindings(outputs),
			},
		},
	}
	if withGate {
		gateInputs := testContract(t, testRequired(t, "passed", value.Boolean()), testOptional(t, "reason", value.String()))
		body.Nodes = append(body.Nodes, workflow.NodeDraft{Name: "judge", Leaf: &workflow.LeafDraft{
			Kind: workflow.Gate, Inputs: gateInputs, Outputs: value.EmptyContract(),
		}})
		body.Edges = append(body.Edges, workflow.EdgeDraft{
			From: workflow.EndpointDraft{Kind: workflow.Child, Child: "revise"}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "judge"},
			Bindings: []workflow.BindingDraft{{From: []string{"passed"}, To: "passed"}},
		})
	}
	return testDefinition(t, workflow.GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Nodes: []workflow.NodeDraft{{Name: "repeat", Loop: &workflow.LoopDraft{
			Inputs: inputs, Outputs: outputs, Maximum: maximum, Termination: []string{"review", "done"}, Body: body,
		}}},
		Edges: []workflow.EdgeDraft{
			{
				From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "repeat"},
				Bindings: contractBindings(inputs),
			},
			{
				From: workflow.EndpointDraft{Kind: workflow.Child, Child: "repeat"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
				Bindings: contractBindings(outputs),
			},
		},
	})
}

func loopInputContract(t *testing.T) value.Contract {
	t.Helper()
	options := testObject(t, testRequired(t, "tone", value.String()))
	return testContract(t, testRequired(t, "prompt", value.String()), testRequired(t, "options", options))
}

func loopOutputContract(t *testing.T) value.Contract {
	t.Helper()
	review := testObject(t, testRequired(t, "done", value.Boolean()), testRequired(t, "note", value.String()))
	return testContract(t,
		testRequired(t, "candidate", value.String()),
		testRequired(t, "passed", value.Boolean()),
		testRequired(t, "review", review),
	)
}

func loopInitialValue(t *testing.T) value.Value {
	t.Helper()
	return testValue(t, map[string]value.Value{
		"prompt":  value.NewString("keep these original bytes /\x00\xff"),
		"options": testValue(t, map[string]value.Value{"tone": value.NewString("precise")}),
	})
}

func loopOutput(t *testing.T, candidate string, done, passed bool) value.Value {
	t.Helper()
	return testValue(t, map[string]value.Value{
		"candidate": value.NewString(candidate),
		"passed":    value.NewBoolean(passed),
		"review": testValue(t, map[string]value.Value{
			"done": value.NewBoolean(done),
			"note": value.NewString("complete output for " + candidate),
		}),
	})
}

func contractBindings(contract value.Contract) []workflow.BindingDraft {
	bindings := make([]workflow.BindingDraft, 0, len(contract.Ports()))
	for _, port := range contract.Ports() {
		bindings = append(bindings, workflow.BindingDraft{From: []string{port.Name()}, To: port.Name()})
	}
	return bindings
}

func startLoopTestExecution(t *testing.T, ctx context.Context, definition workflow.Definition, input value.Value, runner LeafRunner, boundary Boundary) *Execution {
	t.Helper()
	scheduler, err := New(runner, boundary, Policy{Capacity: 1, CancellationGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(ctx, definition, input)
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func assertLoopStart(t *testing.T, start loopStart, iteration uint64, wantInput value.Value) {
	t.Helper()
	gotIteration, ok := pathLoopIteration(start.path)
	if !ok || gotIteration != iteration {
		t.Fatalf("start path = %#v, want semantic loop iteration %d", start.path.Components(), iteration)
	}
	if !start.input.Equal(wantInput) {
		t.Fatalf("iteration %d input = %x, want %x", iteration, start.input.Canonical(), wantInput.Canonical())
	}
}

func assertNoLoopStart(t *testing.T, runner *loopTestRunner, duration time.Duration) {
	t.Helper()
	select {
	case start := <-runner.started:
		t.Fatalf("unexpected concurrent/extra loop start at path %#v", start.path.Components())
	case <-time.After(duration):
	}
}

func pathLoopIteration(path Path) (uint64, bool) {
	for _, component := range path.Components() {
		if iteration, ok := component.LoopIteration(); ok {
			return iteration, true
		}
	}
	return 0, false
}

func mustLoopPath(t *testing.T, path Path, iteration uint64) Path {
	t.Helper()
	result, err := path.LoopIteration(iteration)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustTestInteger(t *testing.T, integer int) value.Value {
	t.Helper()
	result, err := value.NewInteger(strconv.Itoa(integer))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func countPathEvents(events []pathBoundaryEvent, kind traceKind, path Path) int {
	count := 0
	for _, event := range events {
		if event.kind == kind && comparePath(event.path, path) == 0 {
			count++
		}
	}
	return count
}

func hasLoopIterationEvent(events []pathBoundaryEvent, iteration uint64) bool {
	for _, event := range events {
		if got, ok := pathLoopIteration(event.path); ok && got == iteration {
			return true
		}
	}
	return false
}
