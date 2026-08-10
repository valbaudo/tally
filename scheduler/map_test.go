package scheduler

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func TestMapRejectsNonListRuntimeInputBeforeBoundaryEntry(t *testing.T) {
	definition := mapLeafDefinition(t, value.String(), value.EmptyContract())
	node := definition.Root().Nodes()[0]
	scope, ok := node.Scope()
	if !ok {
		t.Fatal("compiled map node has no scope")
	}

	trace := &eventTrace{}
	runner := newMapTestRunner()
	boundary := newPathRecordingBoundary(trace)
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &runState{scheduler: scheduler, control: newRunController(cancel)}
	result, handled := run.runScope(ctx, Path{}.AuthoredChild("each"), scope, value.NewString("not a map input object"))

	if !handled {
		t.Fatal("map scope was not dispatched")
	}
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	if kind, ok := primary.Failure(); !ok || kind != ContractFailure {
		t.Fatalf("primary = %#v, want contract failure", primary)
	}
	if len(boundary.snapshot()) != 0 || runner.count() != 0 {
		t.Fatalf("events = %#v, starts = %d; invalid input must fail before map entry", boundary.snapshot(), runner.count())
	}
}

func TestMapEmptyInputCommitsDeclaredEmptyListWithoutRunnerCalls(t *testing.T) {
	definition := mapLeafDefinition(t, value.String(), mapStringOutputContract(t))
	input := testValue(t, map[string]value.Value{"items": value.NewList()})
	trace := &eventTrace{}
	runner := newMapTestRunner()
	boundary := newPathRecordingBoundary(trace)

	result := waitResult(t, startMapTestExecution(t, definition, input, runner, boundary, Policy{Capacity: 2}))
	requireStatus(t, result, Succeeded)
	want := testValue(t, map[string]value.Value{"results": value.NewList()})
	output, ok := result.Output()
	if !ok || !output.Equal(want) {
		t.Fatalf("output = %x/%v, want %x", output.Canonical(), ok, want.Canonical())
	}
	if runner.count() != 0 {
		t.Fatalf("runner starts = %d, want 0", runner.count())
	}
	mapPath := Path{}.AuthoredChild("each")
	if committed, ok := committedAt(boundary.snapshot(), mapPath); !ok || !committed.Equal(want) {
		t.Fatalf("map commit = %x/%v, want %x", committed.Canonical(), ok, want.Canonical())
	}
}

func TestMapEmptyInputCancelledAfterEntryPublishesNothing(t *testing.T) {
	definition := mapLeafDefinition(t, value.String(), mapStringOutputContract(t))
	input := testValue(t, map[string]value.Value{"items": value.NewList()})
	trace := &eventTrace{}
	base := newPathRecordingBoundary(trace)
	ctx, cancel := context.WithCancel(context.Background())
	boundary := &cancelOnMapEnterBoundary{base: base, cancel: cancel}
	runner := newMapTestRunner()
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(ctx, definition, input)
	if err != nil {
		t.Fatal(err)
	}

	result := waitResult(t, execution)
	requireStatus(t, result, Cancelled)
	mapPath := Path{}.AuthoredChild("each")
	if hasPathEvent(base.snapshot(), traceCommit, mapPath) {
		t.Fatal("empty map committed after cancellation was observed at its entered boundary")
	}
	settled := base.base.settlements("each")
	if len(settled) != 1 || settled[0].Status() != Cancelled {
		t.Fatalf("map settlements = %#v, want one cancelled result", settled)
	}
	if runner.count() != 0 {
		t.Fatalf("runner starts = %d, want 0", runner.count())
	}
}

func TestMapNonEmptyInputCancelledDuringFinalFanInPublishesNothing(t *testing.T) {
	bodyInputs := mapBodyInputs(t, value.String())
	definition := mapDefinition(t, value.String(), value.EmptyContract(), workflow.GraphDraft{
		Inputs: bodyInputs, Outputs: value.EmptyContract(),
	})
	node := definition.Root().Nodes()[0]
	scope, ok := node.Scope()
	if !ok {
		t.Fatal("compiled map node has no scope")
	}

	trace := &eventTrace{}
	base := newPathRecordingBoundary(trace)
	runner := newMapTestRunner()
	ctx := newFinalFanInCancellationContext(context.Background())
	boundary := &finalFanInCancellationBoundary{base: base, cancellation: ctx, bodyCount: 2}
	scheduler, err := New(runner, boundary, Policy{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, controlCancel := context.WithCancel(context.Background())
	defer controlCancel()
	run := &runState{scheduler: scheduler, control: newRunController(controlCancel)}
	input := testValue(t, map[string]value.Value{"items": value.NewList(value.NewString("alpha"), value.NewString("beta"))})
	mapPath := Path{}.AuthoredChild("each")

	result, handled := run.runScope(ctx, mapPath, scope, input)
	if !handled {
		t.Fatal("map scope was not dispatched")
	}
	requireStatus(t, result, Cancelled)
	if _, ok := result.Output(); ok || hasPathEvent(base.snapshot(), traceCommit, mapPath) {
		t.Fatal("non-empty map published output after cancellation became visible during final fan-in")
	}
	for _, item := range []value.Value{value.NewString("alpha"), value.NewString("beta")} {
		bodyPath, pathErr := mapPath.MapItem(item.Canonical(), 0)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if !hasPathEvent(base.snapshot(), traceCommit, bodyPath) {
			t.Fatalf("body %x did not complete before final fan-in cancellation", item.Canonical())
		}
	}
	settled := base.base.settlements("each")
	if len(settled) != 1 || !reflect.DeepEqual(settled[0], result) {
		t.Fatalf("map settlement and returned result differ: settled = %#v, returned = %#v", settled, result)
	}
	if runner.count() != 0 {
		t.Fatalf("runner starts = %d, want 0 for empty body graphs", runner.count())
	}
}

func TestMapCollectsBodyOutputsInInputOrderAfterReverseCompletion(t *testing.T) {
	definition := mapLeafDefinition(t, value.String(), mapStringOutputContract(t))
	input := testValue(t, map[string]value.Value{"items": value.NewList(
		value.NewString("alpha"), value.NewString("beta"), value.NewString("gamma"),
	)})
	runner := newMapTestRunner()
	boundary := newPathRecordingBoundary(&eventTrace{})
	execution := startMapTestExecution(t, definition, input, runner, boundary, Policy{Capacity: 3})
	starts := runner.waitStarts(t, 3)
	byIndex := mapStartsByIndex(t, starts)

	byIndex[2].complete(mustLeafSuccess(t, mapStringOutput(t, "gamma:2")))
	byIndex[1].complete(mustLeafSuccess(t, mapStringOutput(t, "beta:1")))
	byIndex[0].complete(mustLeafSuccess(t, mapStringOutput(t, "alpha:0")))

	result := waitResult(t, execution)
	requireStatus(t, result, Succeeded)
	want := testValue(t, map[string]value.Value{"results": value.NewList(
		mapStringOutput(t, "alpha:0"), mapStringOutput(t, "beta:1"), mapStringOutput(t, "gamma:2"),
	)})
	output, ok := result.Output()
	if !ok || !output.Equal(want) {
		t.Fatalf("output = %x/%v, want original-order fan-in %x", output.Canonical(), ok, want.Canonical())
	}
}

func TestMapInheritsGlobalExternalLeafCapacity(t *testing.T) {
	definition := mapLeafDefinition(t, value.String(), value.EmptyContract())
	input := testValue(t, map[string]value.Value{"items": value.NewList(
		value.NewString("zero"), value.NewString("one"), value.NewString("two"),
	)})
	runner := newMapTestRunner()
	execution := startMapTestExecution(t, definition, input, runner, newPathRecordingBoundary(&eventTrace{}), Policy{Capacity: 2})
	first := runner.waitStarts(t, 2)
	select {
	case unexpected := <-runner.started:
		t.Fatalf("third map leaf started before capacity was released: index %d", mapStartIndex(t, unexpected.start))
	case <-time.After(25 * time.Millisecond):
	}

	first[0].complete(mustLeafSuccess(t, emptyValue(t)))
	third := runner.waitStarts(t, 1)[0]
	first[1].complete(mustLeafSuccess(t, emptyValue(t)))
	third.complete(mustLeafSuccess(t, emptyValue(t)))
	requireStatus(t, waitResult(t, execution), Succeeded)
	if runner.maximum() != 2 {
		t.Fatalf("maximum active external leaves = %d, want 2", runner.maximum())
	}
}

func TestMapStableIdentityUsesCanonicalItemBytesAndOccurrences(t *testing.T) {
	t.Run("distinct reorder changes index data but not identity", func(t *testing.T) {
		alpha := value.NewString("alpha")
		beta := value.NewString("beta")
		forward := runMapCapture(t, value.String(), []value.Value{alpha, beta})
		reversed := runMapCapture(t, value.String(), []value.Value{beta, alpha})
		forwardByItem := mapStartsByItem(t, forward)
		reversedByItem := mapStartsByItem(t, reversed)

		for _, item := range []value.Value{alpha, beta} {
			key := string(item.Canonical())
			left, right := forwardByItem[key], reversedByItem[key]
			if comparePath(left.path, right.path) != 0 {
				t.Fatalf("item %x path changed across reorder: %#v != %#v", item.Canonical(), left.path, right.path)
			}
			canonical, occurrence, ok := pathMapItem(left.path)
			if !ok || !bytes.Equal(canonical, item.Canonical()) || occurrence != 0 {
				t.Fatalf("item path component = %x/%d/%v, want canonical %x occurrence 0", canonical, occurrence, ok, item.Canonical())
			}
		}
		if mapStartIndex(t, forwardByItem[string(alpha.Canonical())]) != 0 || mapStartIndex(t, reversedByItem[string(alpha.Canonical())]) != 1 {
			t.Fatal("reordering alpha did not change its index body data from 0 to 1")
		}
	})

	t.Run("byte-identical duplicates count occurrence in input order", func(t *testing.T) {
		duplicate := value.NewString("same")
		starts := runMapCapture(t, value.String(), []value.Value{duplicate, duplicate, duplicate})
		sort.Slice(starts, func(i, j int) bool { return mapStartIndex(t, starts[i]) < mapStartIndex(t, starts[j]) })
		for index, start := range starts {
			canonical, occurrence, ok := pathMapItem(start.path)
			if !ok || !bytes.Equal(canonical, duplicate.Canonical()) || occurrence != uint64(index) {
				t.Fatalf("index %d map component = %x/%d/%v, want %x/%d", index, canonical, occurrence, ok, duplicate.Canonical(), index)
			}
		}
	})

	t.Run("nested and arbitrary bytes remain private canonical value bytes", func(t *testing.T) {
		weird := value.NewString("/tmp/not-an-identity\x00\xff{\"json\":false}")
		key, err := value.NewEntry("/\x00\xff", value.NewList(value.NewString("nested/one"), value.NewBoolean(true)))
		if err != nil {
			t.Fatal(err)
		}
		nested, err := value.NewMap(key)
		if err != nil {
			t.Fatal(err)
		}
		items := []value.Value{weird, nested}
		starts := runMapCapture(t, value.Any(), items)
		startsByIndex := mapStartRecordsByIndex(t, starts)
		for index, item := range items {
			canonical, occurrence, ok := pathMapItem(startsByIndex[index].path)
			if !ok || occurrence != 0 || !bytes.Equal(canonical, item.Canonical()) {
				t.Fatalf("index %d map component = %x/%d/%v, want exact canonical bytes %x/0", index, canonical, occurrence, ok, item.Canonical())
			}
		}
	})
}

func TestMapFailFastDrainsActiveItemsNormalizesCausesAndPublishesNothing(t *testing.T) {
	definition := mapFailFastDefinition(t)
	items := value.NewList(
		value.NewString("success"), value.NewString("force"), value.NewString("fail"), value.NewString("reject"),
	)
	input := testValue(t, map[string]value.Value{"items": items})
	runner := newMapTestRunner()
	base := newPathRecordingBoundary(&eventTrace{})
	boundary := newMapFailFastBoundary(base, value.NewString("success"), value.NewString("reject"))
	grace := 75 * time.Millisecond
	execution := startMapTestExecution(t, definition, input, runner, boundary, Policy{Capacity: 3, CancellationGrace: grace})

	waitClosed(t, boundary.rejectEntered, "rejecting gate did not enter")
	starts := runner.waitStarts(t, 3)
	byItem := mapStartsByItem(t, mapStartRecords(starts))
	success := byItem[string(value.NewString("success").Canonical())].execution
	failing := byItem[string(value.NewString("fail").Canonical())].execution
	forced := byItem[string(value.NewString("force").Canonical())].execution

	success.complete(mustLeafSuccess(t, emptyValue(t)))
	waitClosed(t, boundary.successBodyCommitted, "successful item did not commit before fail-fast")
	close(boundary.releaseReject)
	waitClosed(t, failing.cancelObserved, "failing sibling did not receive fail-fast cancellation")
	waitClosed(t, forced.cancelObserved, "noncooperative sibling did not receive fail-fast cancellation")
	failing.complete(mustLeafFailure(t, MechanicalFailure, errors.New("item failed")))
	waitClosed(t, forced.forceObserved, "noncooperative sibling was not force-stopped after grace")

	result := waitResult(t, execution)
	requireStatus(t, result, Failed)
	if _, ok := result.Output(); ok {
		t.Fatal("failed map exposed a boundary output")
	}
	primary, _ := result.Primary()
	primaryItem, _, ok := pathMapItem(primary.Path())
	if kind, failed := primary.Failure(); !failed || kind != MechanicalFailure || !ok || !bytes.Equal(primaryItem, value.NewString("fail").Canonical()) {
		t.Fatalf("primary = %#v, want mechanical failure from fail item", primary)
	}
	if !hasRejectedMapItem(result.Secondary(), value.NewString("reject")) {
		t.Fatalf("secondary diagnostics = %#v, want rejecting item", result.Secondary())
	}
	mapPath := Path{}.AuthoredChild("each")
	if hasPathEvent(base.snapshot(), traceCommit, mapPath) || hasPathEvent(base.snapshot(), traceCommit, Path{}) {
		t.Fatal("non-successful map or root committed a boundary output")
	}
	successBody, _ := mapPath.MapItem(value.NewString("success").Canonical(), 0)
	if !hasPathEvent(base.snapshot(), traceCommit, successBody) {
		t.Fatal("already committed successful child disappeared from boundary trace")
	}
	settled := base.base.settlements("each")
	if len(settled) != 1 || !reflect.DeepEqual(settled[0], result) {
		t.Fatalf("map settlement and returned result differ: settled = %#v, returned = %#v", settled, result)
	}
	if runner.count() != 3 || runner.activeCount() != 0 {
		t.Fatalf("runner starts/active = %d/%d, want 3/0", runner.count(), runner.activeCount())
	}
}

type mapStart struct {
	path      Path
	input     value.Value
	execution *mapTestExecution
}

type mapTestRunner struct {
	mu           sync.Mutex
	started      chan *mapTestExecution
	starts       []mapStart
	active       int
	maximumAlive int
}

func newMapTestRunner() *mapTestRunner {
	return &mapTestRunner{started: make(chan *mapTestExecution, 128)}
}

func (r *mapTestRunner) Start(ctx context.Context, request LeafRequest) (LeafExecution, error) {
	execution := &mapTestExecution{
		done: make(chan LeafCompletion, 1), cancelObserved: make(chan struct{}), forceObserved: make(chan struct{}),
	}
	execution.onQuiescent = func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}
	start := mapStart{path: request.Instance().Path(), input: request.Input(), execution: execution}
	execution.start = start

	r.mu.Lock()
	r.starts = append(r.starts, start)
	r.active++
	if r.active > r.maximumAlive {
		r.maximumAlive = r.active
	}
	r.mu.Unlock()
	r.started <- execution
	go func() {
		<-ctx.Done()
		execution.cancelOnce.Do(func() { close(execution.cancelObserved) })
	}()
	return execution, nil
}

func (r *mapTestRunner) waitStarts(t *testing.T, count int) []*mapTestExecution {
	t.Helper()
	starts := make([]*mapTestExecution, 0, count)
	deadline := time.After(testTimeout)
	for len(starts) < count {
		select {
		case started := <-r.started:
			starts = append(starts, started)
		case <-deadline:
			t.Fatalf("started %d map leaves, want %d", len(starts), count)
		}
	}
	return starts
}

func (r *mapTestRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *mapTestRunner) maximum() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maximumAlive
}

func (r *mapTestRunner) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *mapTestRunner) snapshot() []mapStart {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mapStart(nil), r.starts...)
}

type mapTestExecution struct {
	start          mapStart
	done           chan LeafCompletion
	cancelObserved chan struct{}
	forceObserved  chan struct{}
	onQuiescent    func()

	finishOnce     sync.Once
	cancelOnce     sync.Once
	forceOnce      sync.Once
	terminalCount  atomic.Int32
	forceCallCount atomic.Int32
}

func (e *mapTestExecution) Done() <-chan LeafCompletion { return e.done }

func (e *mapTestExecution) ForceStop() error {
	e.forceCallCount.Add(1)
	e.forceOnce.Do(func() {
		close(e.forceObserved)
		e.finishOnce.Do(func() {
			e.terminalCount.Add(1)
			e.onQuiescent()
		})
	})
	return nil
}

func (e *mapTestExecution) complete(completion LeafCompletion) {
	e.finishOnce.Do(func() {
		e.terminalCount.Add(1)
		e.done <- completion
		close(e.done)
		e.onQuiescent()
	})
}

func (e *mapTestExecution) terminalizations() int { return int(e.terminalCount.Load()) }

func (e *mapTestExecution) forceCalls() int { return int(e.forceCallCount.Load()) }

type mapFailFastBoundary struct {
	base                 *pathRecordingBoundary
	successItem          []byte
	rejectItem           []byte
	rejectEntered        chan struct{}
	releaseReject        chan struct{}
	successBodyCommitted chan struct{}
	rejectOnce           sync.Once
	successOnce          sync.Once
}

type cancelOnMapEnterBoundary struct {
	base   *pathRecordingBoundary
	cancel context.CancelFunc
	once   sync.Once
}

type finalFanInCancellationContext struct {
	context.Context
	done         chan struct{}
	armed        atomic.Bool
	observations atomic.Int64
	once         sync.Once
}

func newFinalFanInCancellationContext(parent context.Context) *finalFanInCancellationContext {
	return &finalFanInCancellationContext{Context: parent, done: make(chan struct{})}
}

func (c *finalFanInCancellationContext) Done() <-chan struct{} { return c.done }

func (c *finalFanInCancellationContext) Err() error {
	if c.armed.Load() && c.observations.Add(1) >= 2 {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

type finalFanInCancellationBoundary struct {
	base         *pathRecordingBoundary
	cancellation *finalFanInCancellationContext
	bodyCount    int64
	bodyCommits  atomic.Int64
}

func (b *finalFanInCancellationBoundary) Enter(ctx context.Context, instance Instance) error {
	return b.base.Enter(ctx, instance)
}

func (b *finalFanInCancellationBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	if err := b.base.Commit(ctx, instance, output); err != nil {
		return err
	}
	components := instance.Path().Components()
	if len(components) > 0 && components[len(components)-1].Kind() == MapItemComponent && b.bodyCommits.Add(1) == b.bodyCount {
		b.cancellation.armed.Store(true)
	}
	return nil
}

func (b *finalFanInCancellationBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func (b *cancelOnMapEnterBoundary) Enter(ctx context.Context, instance Instance) error {
	if err := b.base.Enter(ctx, instance); err != nil {
		return err
	}
	if comparePath(instance.Path(), Path{}.AuthoredChild("each")) == 0 {
		b.once.Do(b.cancel)
		<-ctx.Done()
	}
	return nil
}

func (b *cancelOnMapEnterBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	return b.base.Commit(ctx, instance, output)
}

func (b *cancelOnMapEnterBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func newMapFailFastBoundary(base *pathRecordingBoundary, success, reject value.Value) *mapFailFastBoundary {
	return &mapFailFastBoundary{
		base: base, successItem: success.Canonical(), rejectItem: reject.Canonical(),
		rejectEntered: make(chan struct{}), releaseReject: make(chan struct{}), successBodyCommitted: make(chan struct{}),
	}
}

func (b *mapFailFastBoundary) Enter(ctx context.Context, instance Instance) error {
	if item, _, ok := pathMapItem(instance.Path()); ok && bytes.Equal(item, b.rejectItem) && lastAuthoredChild(instance.Path()) == "deny" {
		b.rejectOnce.Do(func() { close(b.rejectEntered) })
		select {
		case <-b.releaseReject:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.base.Enter(ctx, instance)
}

func (b *mapFailFastBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	if err := b.base.Commit(ctx, instance, output); err != nil {
		return err
	}
	components := instance.Path().Components()
	if len(components) > 0 && components[len(components)-1].Kind() == MapItemComponent {
		item, _, _ := components[len(components)-1].MapItem()
		if bytes.Equal(item, b.successItem) {
			b.successOnce.Do(func() { close(b.successBodyCommitted) })
		}
	}
	return nil
}

func (b *mapFailFastBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func mapLeafDefinition(t *testing.T, itemType value.Type, bodyOutputs value.Contract) workflow.Definition {
	t.Helper()
	bodyInputs := mapBodyInputs(t, itemType)
	body := workflow.GraphDraft{
		Inputs: bodyInputs, Outputs: bodyOutputs,
		Nodes: []workflow.NodeDraft{testLeaf("emit", bodyInputs, bodyOutputs)},
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "emit"},
			Bindings: []workflow.BindingDraft{{From: []string{"item"}, To: "item"}, {From: []string{"index"}, To: "index"}},
		}},
	}
	for _, port := range bodyOutputs.Ports() {
		body.Edges = append(body.Edges, workflow.EdgeDraft{
			From: workflow.EndpointDraft{Kind: workflow.Child, Child: "emit"}, To: workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: []workflow.BindingDraft{{From: []string{port.Name()}, To: port.Name()}},
		})
	}
	return mapDefinition(t, itemType, bodyOutputs, body)
}

func mapDefinition(t *testing.T, itemType value.Type, bodyOutputs value.Contract, body workflow.GraphDraft) workflow.Definition {
	t.Helper()
	itemsType, err := value.List(itemType)
	if err != nil {
		t.Fatal(err)
	}
	resultsType, err := value.List(bodyOutputs.ObjectType())
	if err != nil {
		t.Fatal(err)
	}
	inputs := testContract(t, testRequired(t, "items", itemsType))
	outputs := testContract(t, testRequired(t, "results", resultsType))
	return testDefinition(t, workflow.GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Nodes: []workflow.NodeDraft{{Name: "each", Map: &workflow.MapDraft{
			Inputs: inputs, Outputs: outputs, Collection: "items", Result: "results", Body: body,
		}}},
		Edges: []workflow.EdgeDraft{
			{From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "each"}, Bindings: []workflow.BindingDraft{{From: []string{"items"}, To: "items"}}},
			{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "each"}, To: workflow.EndpointDraft{Kind: workflow.Boundary}, Bindings: []workflow.BindingDraft{{From: []string{"results"}, To: "results"}}},
		},
	})
}

func mapFailFastDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	members := []string{"success", "force", "fail", "reject"}
	literals := make([]value.Literal, 0, len(members))
	for _, member := range members {
		literals = append(literals, branchLiteral(t, strconv.Quote(member)))
	}
	itemType, err := value.Enum(literals...)
	if err != nil {
		t.Fatal(err)
	}
	bodyInputs := mapBodyInputs(t, itemType)
	cases := make([]workflow.CaseDraft, 0, len(members))
	for _, member := range members {
		var graph workflow.GraphDraft
		if member == "reject" {
			graph = workflow.GraphDraft{Inputs: bodyInputs, Outputs: value.EmptyContract(), Nodes: []workflow.NodeDraft{testGateNode(t, "deny", false, "item rejected")}}
		} else {
			graph = workflow.GraphDraft{
				Inputs: bodyInputs, Outputs: value.EmptyContract(), Nodes: []workflow.NodeDraft{testLeaf(member, bodyInputs, value.EmptyContract())},
				Edges: []workflow.EdgeDraft{{
					From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: member},
					Bindings: []workflow.BindingDraft{{From: []string{"item"}, To: "item"}, {From: []string{"index"}, To: "index"}},
				}},
			}
		}
		cases = append(cases, workflow.CaseDraft{Name: string(literals[len(cases)].Bytes()), Graph: graph})
	}
	body := workflow.GraphDraft{
		Inputs: bodyInputs, Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{{Name: "dispatch", Branch: &workflow.BranchDraft{
			Inputs: bodyInputs, Outputs: value.EmptyContract(), Selector: "item", Cases: cases,
		}}},
		Edges: []workflow.EdgeDraft{{
			From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "dispatch"},
			Bindings: []workflow.BindingDraft{{From: []string{"item"}, To: "item"}, {From: []string{"index"}, To: "index"}},
		}},
	}
	return mapDefinition(t, itemType, value.EmptyContract(), body)
}

func mapBodyInputs(t *testing.T, itemType value.Type) value.Contract {
	t.Helper()
	return testContract(t, testRequired(t, "item", itemType), testRequired(t, "index", value.Integer()))
}

func mapStringOutputContract(t *testing.T) value.Contract {
	t.Helper()
	return testContract(t, testRequired(t, "value", value.String()))
}

func mapStringOutput(t *testing.T, text string) value.Value {
	t.Helper()
	return testValue(t, map[string]value.Value{"value": value.NewString(text)})
}

func startMapTestExecution(t *testing.T, definition workflow.Definition, input value.Value, runner LeafRunner, boundary Boundary, policy Policy) *Execution {
	t.Helper()
	scheduler, err := New(runner, boundary, policy)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := scheduler.Start(context.Background(), definition, input)
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func runMapCapture(t *testing.T, itemType value.Type, items []value.Value) []mapStart {
	t.Helper()
	definition := mapLeafDefinition(t, itemType, value.EmptyContract())
	runner := newMapTestRunner()
	input := testValue(t, map[string]value.Value{"items": value.NewList(items...)})
	execution := startMapTestExecution(t, definition, input, runner, newPathRecordingBoundary(&eventTrace{}), Policy{Capacity: max(1, len(items))})
	started := runner.waitStarts(t, len(items))
	for _, leaf := range started {
		leaf.complete(mustLeafSuccess(t, emptyValue(t)))
	}
	requireStatus(t, waitResult(t, execution), Succeeded)
	return runner.snapshot()
}

func mapStartRecords(executions []*mapTestExecution) []mapStart {
	starts := make([]mapStart, 0, len(executions))
	for _, execution := range executions {
		starts = append(starts, execution.start)
	}
	return starts
}

func mapStartsByIndex(t *testing.T, executions []*mapTestExecution) map[int]*mapTestExecution {
	t.Helper()
	result := make(map[int]*mapTestExecution, len(executions))
	for _, execution := range executions {
		result[mapStartIndex(t, execution.start)] = execution
	}
	return result
}

func mapStartRecordsByIndex(t *testing.T, starts []mapStart) map[int]mapStart {
	t.Helper()
	result := make(map[int]mapStart, len(starts))
	for _, start := range starts {
		result[mapStartIndex(t, start)] = start
	}
	return result
}

func mapStartsByItem(t *testing.T, starts []mapStart) map[string]mapStart {
	t.Helper()
	result := make(map[string]mapStart, len(starts))
	for _, start := range starts {
		item, ok := selectValue(start.input, []string{"item"})
		if !ok {
			t.Fatalf("map leaf input %x omitted item", start.input.Canonical())
		}
		result[string(item.Canonical())] = start
	}
	return result
}

func mapStartIndex(t *testing.T, start mapStart) int {
	t.Helper()
	indexed, ok := selectValue(start.input, []string{"index"})
	if !ok {
		t.Fatalf("map leaf input %x omitted index", start.input.Canonical())
	}
	number, ok := indexed.Number()
	if !ok {
		t.Fatalf("map leaf index = %x, want integer", indexed.Canonical())
	}
	index, err := strconv.Atoi(number)
	if err != nil {
		t.Fatalf("map leaf index %q: %v", number, err)
	}
	return index
}

func pathMapItem(path Path) ([]byte, uint64, bool) {
	for _, component := range path.Components() {
		if item, occurrence, ok := component.MapItem(); ok {
			return item, occurrence, true
		}
	}
	return nil, 0, false
}

func lastAuthoredChild(path Path) string {
	components := path.Components()
	for index := len(components) - 1; index >= 0; index-- {
		if name, ok := components[index].AuthoredChild(); ok {
			return name
		}
	}
	return ""
}

func hasRejectedMapItem(diagnostics []Diagnostic, item value.Value) bool {
	want := item.Canonical()
	for _, diagnostic := range diagnostics {
		got, _, ok := pathMapItem(diagnostic.Path())
		if ok && bytes.Equal(got, want) && diagnostic.Status() == Rejected {
			return true
		}
	}
	return false
}
