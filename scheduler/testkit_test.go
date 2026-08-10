package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

const testTimeout = 2 * time.Second

type traceKind uint8

const (
	traceEnter traceKind = iota + 1
	traceStart
	traceCancel
	traceForceStop
	traceCommit
	traceSettle
)

type traceEvent struct {
	kind   traceKind
	child  string
	status Status
}

type eventTrace struct {
	mu     sync.Mutex
	events []traceEvent
}

func (t *eventTrace) record(event traceEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}

func (t *eventTrace) snapshot() []traceEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]traceEvent(nil), t.events...)
}

func (t *eventTrace) index(kind traceKind, child string) int {
	for index, event := range t.snapshot() {
		if event.kind == kind && event.child == child {
			return index
		}
	}
	return -1
}

type recordingBoundary struct {
	trace       *eventTrace
	mu          sync.Mutex
	enterErrors map[string]error
	commitError map[string]error
	committed   map[string]value.Value
}

func newRecordingBoundary(trace *eventTrace) *recordingBoundary {
	return &recordingBoundary{
		trace:       trace,
		enterErrors: make(map[string]error),
		commitError: make(map[string]error),
		committed:   make(map[string]value.Value),
	}
}

func (b *recordingBoundary) Enter(_ context.Context, instance Instance) error {
	child := instanceName(instance)
	b.trace.record(traceEvent{kind: traceEnter, child: child})
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enterErrors[child]
}

func (b *recordingBoundary) Commit(_ context.Context, instance Instance, output value.Value) error {
	child := instanceName(instance)
	b.trace.record(traceEvent{kind: traceCommit, child: child})
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.commitError[child]; err != nil {
		return err
	}
	b.committed[child] = output
	return nil
}

func (b *recordingBoundary) Settle(_ context.Context, instance Instance, result Result) error {
	b.trace.record(traceEvent{kind: traceSettle, child: instanceName(instance), status: result.Status()})
	return nil
}

func (b *recordingBoundary) commits() map[string]value.Value {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make(map[string]value.Value, len(b.committed))
	for name, output := range b.committed {
		result[name] = output
	}
	return result
}

type controlledRunner struct {
	trace *eventTrace

	mu           sync.Mutex
	executions   map[string]*controlledLeafExecution
	startErrors  map[string]error
	cooperative  map[string]bool
	started      chan string
	startCount   int
	active       int
	maximumAlive int
}

func newControlledRunner(trace *eventTrace) *controlledRunner {
	return &controlledRunner{
		trace:       trace,
		executions:  make(map[string]*controlledLeafExecution),
		startErrors: make(map[string]error),
		cooperative: make(map[string]bool),
		started:     make(chan string, 64),
	}
}

func (r *controlledRunner) Start(ctx context.Context, request LeafRequest) (LeafExecution, error) {
	name := instanceName(request.Instance())
	r.mu.Lock()
	if err := r.startErrors[name]; err != nil {
		r.mu.Unlock()
		return nil, err
	}
	execution := &controlledLeafExecution{
		name: name, trace: r.trace, done: make(chan LeafCompletion, 1),
		cancelObserved: make(chan struct{}), forceObserved: make(chan struct{}),
		onQuiescent: r.quiescent, input: request.Input(),
	}
	r.executions[name] = execution
	r.startCount++
	r.active++
	if r.active > r.maximumAlive {
		r.maximumAlive = r.active
	}
	cooperative := r.cooperative[name]
	r.mu.Unlock()

	r.trace.record(traceEvent{kind: traceStart, child: name})
	r.started <- name
	go func() {
		<-ctx.Done()
		execution.observeCancel()
		if cooperative {
			execution.complete(mustLeafCancelled(ctx.Err()))
		}
	}()
	return execution, nil
}

func (r *controlledRunner) execution(t *testing.T, name string) *controlledLeafExecution {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		r.mu.Lock()
		execution := r.executions[name]
		r.mu.Unlock()
		if execution != nil {
			return execution
		}
		select {
		case <-r.started:
		case <-deadline:
			t.Fatalf("leaf %q did not start", name)
		}
	}
}

func (r *controlledRunner) waitStarts(t *testing.T, count int) []string {
	t.Helper()
	seen := make([]string, 0, count)
	deadline := time.After(testTimeout)
	for len(seen) < count {
		select {
		case name := <-r.started:
			seen = append(seen, name)
		case <-deadline:
			t.Fatalf("started %v leaves, want %d", seen, count)
		}
	}
	return seen
}

func (r *controlledRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startCount
}

func (r *controlledRunner) maximum() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maximumAlive
}

func (r *controlledRunner) quiescent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
}

type controlledLeafExecution struct {
	name           string
	trace          *eventTrace
	done           chan LeafCompletion
	cancelObserved chan struct{}
	forceObserved  chan struct{}
	onQuiescent    func()

	completeOnce sync.Once
	cancelOnce   sync.Once
	forceOnce    sync.Once
	mu           sync.Mutex
	input        value.Value
	forceAt      time.Time
}

// completionOnForcePathRunner makes the completion channel become ready only
// after runLeaf has selected the run-level force signal. It exercises the
// already-quiescent completion check without a timing race in the test.
type completionOnForcePathRunner struct {
	started chan struct{}
	ready   chan struct{}
	exec    *completionOnForcePathExecution
}

func newCompletionOnForcePathRunner(t *testing.T) *completionOnForcePathRunner {
	t.Helper()
	completion := mustLeafSuccess(t, emptyValue(t))
	return &completionOnForcePathRunner{
		started: make(chan struct{}), ready: make(chan struct{}),
		exec: &completionOnForcePathExecution{
			blocked: make(chan LeafCompletion), completed: make(chan LeafCompletion, 1), completion: completion,
		},
	}
}

func (r *completionOnForcePathRunner) Start(context.Context, LeafRequest) (LeafExecution, error) {
	r.exec.completed <- r.exec.completion
	close(r.exec.completed)
	r.exec.onSelect = r.ready
	close(r.started)
	return r.exec, nil
}

type completionOnForcePathExecution struct {
	mu         sync.Mutex
	calls      int
	blocked    chan LeafCompletion
	completed  chan LeafCompletion
	completion LeafCompletion
	onSelect   chan struct{}
	selectOnce sync.Once
	forced     bool
}

func (e *completionOnForcePathExecution) Done() <-chan LeafCompletion {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.calls >= 3 {
		return e.completed
	}
	if e.calls == 2 && e.onSelect != nil {
		e.selectOnce.Do(func() { close(e.onSelect) })
	}
	return e.blocked
}

func (e *completionOnForcePathExecution) ForceStop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forced = true
	return nil
}

func (e *completionOnForcePathExecution) wasForced() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.forced
}

func (e *controlledLeafExecution) Done() <-chan LeafCompletion { return e.done }

func (e *controlledLeafExecution) ForceStop() error {
	e.forceOnce.Do(func() {
		e.mu.Lock()
		e.forceAt = time.Now()
		e.mu.Unlock()
		e.trace.record(traceEvent{kind: traceForceStop, child: e.name})
		close(e.forceObserved)
		e.completeOnce.Do(func() { e.onQuiescent() })
	})
	return nil
}

func (e *controlledLeafExecution) complete(completion LeafCompletion) {
	e.completeOnce.Do(func() {
		e.done <- completion
		close(e.done)
		e.onQuiescent()
	})
}

func (e *controlledLeafExecution) observeCancel() {
	e.cancelOnce.Do(func() {
		e.trace.record(traceEvent{kind: traceCancel, child: e.name})
		close(e.cancelObserved)
	})
}

func (e *controlledLeafExecution) forceTime() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.forceAt
}

func instanceName(instance Instance) string {
	components := instance.Path().Components()
	if len(components) == 0 {
		return "root"
	}
	if name, ok := components[len(components)-1].AuthoredChild(); ok {
		return name
	}
	return fmt.Sprintf("component-%d", components[len(components)-1].Kind())
}

func testDefinition(t *testing.T, graph workflow.GraphDraft) workflow.Definition {
	t.Helper()
	definition, err := workflow.Compile(workflow.ProgramDraft{
		Root:    "root",
		Modules: []workflow.ModuleDraft{{Name: "root", Graph: graph}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return definition
}

func testLeaf(name string, inputs, outputs value.Contract) workflow.NodeDraft {
	return workflow.NodeDraft{Name: name, Leaf: &workflow.LeafDraft{Kind: workflow.Script, Inputs: inputs, Outputs: outputs}}
}

func testContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatalf("NewContract() error = %v", err)
	}
	return contract
}

func testRequired(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatalf("Required() error = %v", err)
	}
	return field
}

func testOptional(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Optional(name, typ)
	if err != nil {
		t.Fatalf("Optional() error = %v", err)
	}
	return field
}

func testObject(t *testing.T, fields ...value.Field) value.Type {
	t.Helper()
	typ, err := value.Object(fields...)
	if err != nil {
		t.Fatalf("Object() error = %v", err)
	}
	return typ
}

func testValue(t *testing.T, fields map[string]value.Value) value.Value {
	t.Helper()
	entries := make([]value.Entry, 0, len(fields))
	for name, fieldValue := range fields {
		entry, err := value.NewEntry(name, fieldValue)
		if err != nil {
			t.Fatalf("NewEntry(%q) error = %v", name, err)
		}
		entries = append(entries, entry)
	}
	result, err := value.NewObject(entries...)
	if err != nil {
		t.Fatalf("NewObject() error = %v", err)
	}
	return result
}

func emptyValue(t *testing.T) value.Value { return testValue(t, nil) }

func mustLeafSuccess(t *testing.T, output value.Value) LeafCompletion {
	t.Helper()
	completion, err := NewLeafSucceeded(output)
	if err != nil {
		t.Fatalf("NewLeafSucceeded() error = %v", err)
	}
	return completion
}

func mustLeafFailure(t *testing.T, kind FailureKind, err error) LeafCompletion {
	t.Helper()
	completion, constructErr := NewLeafFailed(kind, err)
	if constructErr != nil {
		t.Fatalf("NewLeafFailed() error = %v", constructErr)
	}
	return completion
}

func mustLeafCancelled(err error) LeafCompletion {
	completion, constructErr := NewLeafCancelled(err)
	if constructErr != nil {
		panic(constructErr)
	}
	return completion
}

func startTestExecution(t *testing.T, definition workflow.Definition, input value.Value, runner *controlledRunner, boundary *recordingBoundary, policy Policy) *Execution {
	t.Helper()
	scheduler, err := New(runner, boundary, policy)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	execution, err := scheduler.Start(context.Background(), definition, input)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return execution
}

func waitResult(t *testing.T, execution *Execution) Result {
	t.Helper()
	done := make(chan Result, 1)
	go func() { done <- execution.Wait() }()
	select {
	case result := <-done:
		return result
	case <-time.After(testTimeout):
		t.Fatal("execution did not settle")
		return Result{}
	}
}

func requireStatus(t *testing.T, result Result, status Status) {
	t.Helper()
	if !result.Valid() || result.Status() != status {
		t.Fatalf("result = %#v (valid %v, status %v), want %v", result, result.Valid(), result.Status(), status)
	}
}

func completeEmpty(t *testing.T, executions ...*controlledLeafExecution) {
	t.Helper()
	for _, execution := range executions {
		execution.complete(mustLeafSuccess(t, emptyValue(t)))
	}
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func waitClosed(t *testing.T, channel <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(testTimeout):
		t.Fatal(message)
	}
}

func assertNoStart(t *testing.T, runner *controlledRunner, duration time.Duration) {
	t.Helper()
	select {
	case name := <-runner.started:
		t.Fatalf("unexpected leaf start %q", name)
	case <-time.After(duration):
	}
}

var errBoom = errors.New("boom")
