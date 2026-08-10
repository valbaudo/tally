package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

const conformancePermutations = 128

func TestPrestigeStructuredControlTrace(t *testing.T) {
	definition := prestigeDefinition(t)
	documents := []value.Value{
		value.NewString("duplicate/\x00document"),
		value.NewString("distinct document"),
		value.NewString("duplicate/\x00document"),
	}
	input := testValue(t, map[string]value.Value{
		"documents":    value.NewList(documents...),
		"selected":     value.NewBoolean(true),
		"request":      value.NewString("prestige request /\x00\xff"),
		"cleanup_note": value.NewString("retain committed evidence"),
	})

	t.Run("success trace", func(t *testing.T) {
		trace := &eventTrace{}
		runner := newPrestigeRunner(trace)
		boundary := newPathRecordingBoundary(trace)
		execution := startPathExecution(t, definition, input, runner, boundary, Policy{Capacity: 16})

		initial := prestigeStartsByName(runner.waitStarts(t, 5))
		requirePrestigeStartCount(t, initial, "static-analysis", 1)
		requirePrestigeStartCount(t, initial, "analyze-document", 3)
		requirePrestigeStartCount(t, initial, "route-true", 1)
		if runner.inner.maximum() != 5 {
			t.Fatalf("initial static, dynamic, and conditional work did not overlap: maximum active = %d, want 5", runner.inner.maximum())
		}

		staticInput := testValue(t, map[string]value.Value{"request": value.NewString("prestige request /\x00\xff")})
		assertPrestigeInput(t, initial["static-analysis"][0], staticInput)
		assertPrestigeInput(t, initial["route-true"][0], emptyValue(t))

		mapStarts := append([]*mapTestExecution(nil), initial["analyze-document"]...)
		sort.Slice(mapStarts, func(i, j int) bool {
			return mapStartIndex(t, mapStarts[i].start) < mapStartIndex(t, mapStarts[j].start)
		})
		seenOccurrences := map[string]uint64{}
		for index, started := range mapStarts {
			wantInput := testValue(t, map[string]value.Value{
				"item":  documents[index],
				"index": mustTestInteger(t, index),
			})
			assertPrestigeInput(t, started, wantInput)
			itemBytes, occurrence, ok := pathMapItem(started.start.path)
			if !ok || string(itemBytes) != string(documents[index].Canonical()) {
				t.Fatalf("map index %d path item = %x/%v, want exact canonical bytes %x", index, itemBytes, ok, documents[index].Canonical())
			}
			key := string(documents[index].Canonical())
			if occurrence != seenOccurrences[key] {
				t.Fatalf("map index %d occurrence = %d, want %d", index, occurrence, seenOccurrences[key])
			}
			seenOccurrences[key]++
		}

		analysisOutputs := make([]value.Value, len(documents))
		completionOrder := rand.New(rand.NewSource(73)).Perm(len(mapStarts))
		for _, index := range completionOrder {
			text, _ := documents[index].Text()
			analysisOutputs[index] = testValue(t, map[string]value.Value{
				"analysis": value.NewString(fmt.Sprintf("analysis[%d]:%s", index, text)),
			})
			mapStarts[index].complete(mustLeafSuccess(t, analysisOutputs[index]))
		}
		initial["route-true"][0].complete(mustLeafSuccess(t, testValue(t, map[string]value.Value{
			"route": value.NewString("fast"),
		})))
		initial["static-analysis"][0].complete(mustLeafSuccess(t, testValue(t, map[string]value.Value{
			"finding": value.NewString("static finding"),
		})))

		orderedAnalyses := value.NewList(analysisOutputs...)
		review := prestigeStartsByName(runner.waitStarts(t, 2))
		requirePrestigeStartCount(t, review, "primary-review", 1)
		requirePrestigeStartCount(t, review, "secondary-review", 1)
		assertPrestigeInput(t, review["primary-review"][0], testValue(t, map[string]value.Value{
			"static":   value.NewString("static finding"),
			"analyses": orderedAnalyses,
		}))
		assertPrestigeInput(t, review["secondary-review"][0], testValue(t, map[string]value.Value{
			"analyses": orderedAnalyses,
		}))
		review["secondary-review"][0].complete(mustLeafSuccess(t, emptyValue(t)))
		evidence := testValue(t, map[string]value.Value{"evidence": value.NewString("approved evidence")})
		review["primary-review"][0].complete(mustLeafSuccess(t, evidence))

		innerCleanup := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, innerCleanup, "inner-cleanup", 1)
		assertPrestigeInput(t, innerCleanup["inner-cleanup"][0], testValue(t, map[string]value.Value{
			"required_copy": value.NewString("prestige request /\x00\xff"),
			"optional_copy": value.NewString("retain committed evidence"),
			"outcome":       value.NewString("succeeded"),
			"evidence":      value.NewString("approved evidence"),
		}))
		innerCleanup["inner-cleanup"][0].complete(mustLeafSuccess(t, emptyValue(t)))

		firstWorker := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, firstWorker, "worker", 1)
		loopInitial := testValue(t, map[string]value.Value{"seed": value.NewString("approved evidence")})
		assertPrestigeLoopStart(t, firstWorker["worker"][0], 1, testValue(t, map[string]value.Value{
			"initial":   loopInitial,
			"iteration": mustTestInteger(t, 1),
		}))
		firstWorker["worker"][0].complete(mustLeafSuccess(t, testValue(t, map[string]value.Value{
			"candidate": value.NewString("draft-1"),
		})))

		firstJudge := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, firstJudge, "judge", 1)
		assertPrestigeLoopStart(t, firstJudge["judge"][0], 1, testValue(t, map[string]value.Value{
			"candidate": value.NewString("draft-1"),
			"iteration": mustTestInteger(t, 1),
		}))
		firstLoopOutput := testValue(t, map[string]value.Value{
			"candidate": value.NewString("draft-1"),
			"passed":    value.NewBoolean(false),
		})
		firstJudge["judge"][0].complete(mustLeafSuccess(t, testValue(t, map[string]value.Value{
			"passed": value.NewBoolean(false),
		})))

		secondWorker := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, secondWorker, "worker", 1)
		assertPrestigeLoopStart(t, secondWorker["worker"][0], 2, testValue(t, map[string]value.Value{
			"initial":   loopInitial,
			"previous":  firstLoopOutput,
			"iteration": mustTestInteger(t, 2),
		}))
		secondWorker["worker"][0].complete(mustLeafSuccess(t, testValue(t, map[string]value.Value{
			"candidate": value.NewString("draft-2"),
		})))

		secondJudge := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, secondJudge, "judge", 1)
		assertPrestigeLoopStart(t, secondJudge["judge"][0], 2, testValue(t, map[string]value.Value{
			"candidate": value.NewString("draft-2"),
			"iteration": mustTestInteger(t, 2),
		}))
		secondJudge["judge"][0].complete(mustLeafSuccess(t, testValue(t, map[string]value.Value{
			"passed": value.NewBoolean(true),
		})))

		aggregate := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, aggregate, "aggregate", 1)
		assertPrestigeInput(t, aggregate["aggregate"][0], testValue(t, map[string]value.Value{
			"candidate": value.NewString("draft-2"),
			"route":     value.NewString("fast"),
			"evidence":  value.NewString("approved evidence"),
			"analyses":  orderedAnalyses,
		}))
		final := testValue(t, map[string]value.Value{"final": value.NewString("prestige:draft-2:fast:approved evidence")})
		aggregate["aggregate"][0].complete(mustLeafSuccess(t, final))

		outerCleanup := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, outerCleanup, "outer-cleanup", 1)
		assertPrestigeInput(t, outerCleanup["outer-cleanup"][0], testValue(t, map[string]value.Value{
			"outcome": value.NewString("succeeded"),
			"final":   value.NewString("prestige:draft-2:fast:approved evidence"),
		}))
		outerCleanup["outer-cleanup"][0].complete(mustLeafSuccess(t, emptyValue(t)))

		result := waitResult(t, execution)
		requireStatus(t, result, Succeeded)
		output, ok := result.Output()
		if !ok || !output.Equal(final) {
			t.Fatalf("Prestige output = %x/%v, want %x", output.Canonical(), ok, final.Canonical())
		}
		if runner.inner.activeCount() != 0 {
			t.Fatalf("runner retained %d active executions after success", runner.inner.activeCount())
		}

		events := boundary.snapshot()
		assertNoCaseEvents(t, events, "false")
		assertPrestigePathEvent(t, events, traceEnter, Path{}.AuthoredChild("route").BranchCase("true"))
		assertPrestigePathEvent(t, events, traceEnter, Path{}.AuthoredChild("protected").Cleanup().AuthoredChild("inner-cleanup"))
		assertPrestigePathEvent(t, events, traceEnter, Path{}.Cleanup().AuthoredChild("outer-cleanup"))
		assertPrestigeTraceBefore(t, trace, traceCommit, "static-analysis", traceStart, "primary-review")
		assertPrestigeTraceBefore(t, trace, traceCommit, "analyze-documents", traceStart, "primary-review")
		assertPrestigeTraceBefore(t, trace, traceCommit, "review", traceStart, "inner-cleanup")
		assertPrestigeTraceBefore(t, trace, traceCommit, "inner-cleanup", traceCommit, "protected")
		assertPrestigeTraceBefore(t, trace, traceCommit, "protected", traceStart, "worker")
		assertPrestigeTraceBefore(t, trace, traceCommit, "revise", traceEnter, "approve")
		assertPrestigeTraceBefore(t, trace, traceCommit, "approve", traceStart, "aggregate")
		assertPrestigeTraceBefore(t, trace, traceCommit, "aggregate", traceStart, "outer-cleanup")
		assertPrestigeTraceBefore(t, trace, traceCommit, "outer-cleanup", traceCommit, "root")

		gateOutput, committed := committedAt(events, Path{}.AuthoredChild("approve"))
		if !committed || !gateOutput.Equal(emptyValue(t)) {
			t.Fatalf("gate commit = %x/%v, want empty completion-only output", gateOutput.Canonical(), committed)
		}
		if _, exists := aggregate["approve"]; exists {
			t.Fatal("scheduler-owned gate unexpectedly reached the external runner")
		}
	})

	t.Run("same definition publishes no non-success boundary output", func(t *testing.T) {
		trace := &eventTrace{}
		runner := newPrestigeRunner(trace)
		boundary := newPathRecordingBoundary(trace)
		execution := startPathExecution(t, definition, input, runner, boundary, Policy{Capacity: 16})

		initial := prestigeStartsByName(runner.waitStarts(t, 5))
		requirePrestigeStartCount(t, initial, "static-analysis", 1)
		initial["static-analysis"][0].complete(mustLeafFailure(t, MechanicalFailure, fmt.Errorf("static analysis failed")))

		cleanup := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, cleanup, "outer-cleanup", 1)
		assertPrestigeInput(t, cleanup["outer-cleanup"][0], testValue(t, map[string]value.Value{
			"outcome": value.NewString("failed"),
		}))
		cleanup["outer-cleanup"][0].complete(mustLeafSuccess(t, emptyValue(t)))

		result := waitResult(t, execution)
		requireStatus(t, result, Failed)
		if _, ok := result.Output(); ok {
			t.Fatal("non-successful Prestige execution exposed a root output")
		}
		if runner.inner.activeCount() != 0 {
			t.Fatalf("runner retained %d active executions after fail-fast cleanup", runner.inner.activeCount())
		}
		assertSettledPathsNeverCommit(t, boundary.snapshot())
	})
}

func TestStructuredControlArbitrationStress(t *testing.T) {
	mapLow, err := Path{}.AuthoredChild("scope/\x00").MapItem([]byte{0x00, 0xff, '/', 0x00}, 1)
	if err != nil {
		t.Fatal(err)
	}
	mapHigh, err := Path{}.AuthoredChild("scope/\x00").MapItem([]byte{0xff, 0x00, '/', 0x00}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if comparePath(mapLow, mapHigh) >= 0 {
		t.Fatalf("arbitrary-byte map path order = %d, want low bytes before high bytes", comparePath(mapLow, mapHigh))
	}

	wantPrimary := Path{}.AuthoredChild("\x00failure/\xff")
	causes := []diagnostic{
		failed(mapHigh, TimeoutFailure, errors.New("timeout/\x00\xff")),
		rejected(Path{}.AuthoredChild("\x00policy/\xff"), "policy/\x00\xff"),
		parentCancelled(Path{}.AuthoredChild("parent/\x00\xff")),
		intrinsicCancelled(Path{}.AuthoredChild("intrinsic/\x00\xff")),
		failed(mapLow, ContractFailure, errors.New("contract/\x00\xff")),
		failed(wantPrimary, MechanicalFailure, errors.New("mechanical/\x00\xff")),
	}
	want := stressResultSignature(normalize(causes, nil))
	wantExternal := stressResultSignature(normalize(causes, errors.New("external/\x00\xff")))
	for seed := int64(0); seed < conformancePermutations; seed++ {
		permuted := shuffled(seed, causes)
		got := normalize(permuted, nil)
		primary, ok := got.Primary()
		failure, failed := primary.Failure()
		if !ok || !failed || failure != MechanicalFailure || comparePath(primary.Path(), wantPrimary) != 0 {
			t.Fatalf("seed %d primary = %#v, want mechanical failure at %#v", seed, primary, wantPrimary.Components())
		}
		if signature := stressResultSignature(got); signature != want {
			t.Fatalf("seed %d normalized bytes = %q, want %q", seed, signature, want)
		}

		external := normalize(permuted, errors.New("external/\x00\xff"))
		if external.Status() != Cancelled || stressResultSignature(external) != wantExternal {
			t.Fatalf("seed %d external normalization = %q, want %q", seed, stressResultSignature(external), wantExternal)
		}
	}

	for _, kind := range []FailureKind{MechanicalFailure, ContractFailure, TimeoutFailure} {
		got := normalize([]diagnostic{
			rejected(Path{}.AuthoredChild("a-rejection"), "no"),
			failed(Path{}.AuthoredChild("z-failure"), kind, errors.New("failed")),
		}, nil)
		primary, ok := got.Primary()
		failure, failed := primary.Failure()
		if !ok || got.Status() != Failed || !failed || failure != kind {
			t.Fatalf("failure kind %d did not independently outrank rejection: %#v", kind, got)
		}
	}
}

func TestStructuredControlGraphCancellationStress(t *testing.T) {
	empty := value.EmptyContract()
	type leafCase struct {
		name       string
		completion LeafCompletion
		diagnostic diagnostic
	}
	leaves := []leafCase{
		{name: "a-mechanical", completion: mustLeafFailure(t, MechanicalFailure, errors.New("mechanical/a")), diagnostic: failed(Path{}.AuthoredChild("a-mechanical"), MechanicalFailure, errors.New("mechanical/a"))},
		{name: "b-contract", completion: mustLeafFailure(t, ContractFailure, errors.New("contract/b")), diagnostic: failed(Path{}.AuthoredChild("b-contract"), ContractFailure, errors.New("contract/b"))},
		{name: "c-timeout", completion: mustLeafTimeout(t, errors.New("timeout/c")), diagnostic: failed(Path{}.AuthoredChild("c-timeout"), TimeoutFailure, errors.New("timeout/c"))},
		{name: "d-mechanical", completion: mustLeafFailure(t, MechanicalFailure, errors.New("mechanical/d")), diagnostic: failed(Path{}.AuthoredChild("d-mechanical"), MechanicalFailure, errors.New("mechanical/d"))},
		{name: "e-contract", completion: mustLeafFailure(t, ContractFailure, errors.New("contract/e")), diagnostic: failed(Path{}.AuthoredChild("e-contract"), ContractFailure, errors.New("contract/e"))},
		{name: "f-timeout", completion: mustLeafTimeout(t, errors.New("timeout/f")), diagnostic: failed(Path{}.AuthoredChild("f-timeout"), TimeoutFailure, errors.New("timeout/f"))},
		{name: "g-/\x00", completion: mustLeafFailure(t, MechanicalFailure, errors.New("mechanical/g")), diagnostic: failed(Path{}.AuthoredChild("g-/\x00"), MechanicalFailure, errors.New("mechanical/g"))},
		{name: "h-é", completion: mustLeafFailure(t, ContractFailure, errors.New("contract/h")), diagnostic: failed(Path{}.AuthoredChild("h-é"), ContractFailure, errors.New("contract/h"))},
	}
	nodes := make([]workflow.NodeDraft, len(leaves))
	causes := make([]diagnostic, len(leaves))
	for index, leaf := range leaves {
		nodes[index] = testLeaf(leaf.name, empty, empty)
		causes[index] = leaf.diagnostic
	}
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: nodes,
	})
	want := stressResultSignature(normalize(causes, nil))
	seenOrders := make(map[string]struct{})
	for seed := int64(0); seed < conformancePermutations; seed++ {
		runner := newMapTestRunner()
		boundary := newSettlementSignalBoundary(&eventTrace{}, len(leaves)+1)
		execution := startMapTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: len(leaves), CancellationGrace: testTimeout})
		starts := prestigeStartsByName(runner.waitStarts(t, len(leaves)))
		order := conformanceSchedule(len(leaves), int(seed)*313+17)
		orderedNames := make([]string, len(order))
		for position, index := range order {
			orderedNames[position] = leaves[index].name
			requirePrestigeStartCount(t, starts, leaves[index].name, 1)
		}
		seenOrders[strings.Join(orderedNames, ",")] = struct{}{}

		first := leaves[order[0]]
		starts[first.name][0].complete(first.completion)
		boundary.waitSettled(t, Path{}.AuthoredChild(first.name))
		for _, index := range order[1:] {
			name := leaves[index].name
			waitClosed(t, starts[name][0].cancelObserved, fmt.Sprintf("seed %d leaf %q did not observe fail-fast cancellation", seed, name))
		}
		for _, index := range order[1:] {
			leaf := leaves[index]
			starts[leaf.name][0].complete(leaf.completion)
			boundary.waitSettled(t, Path{}.AuthoredChild(leaf.name))
		}

		result := waitResult(t, execution)
		requireStatus(t, result, Failed)
		primary, _ := result.Primary()
		failure, ok := primary.Failure()
		if !ok || failure != MechanicalFailure || lastAuthoredChild(primary.Path()) != "a-mechanical" {
			t.Fatalf("seed %d primary = %#v, want lexical mechanical path", seed, primary)
		}
		signature := stressResultSignature(result)
		if signature != want {
			t.Fatalf("seed %d runtime arbitration = %q, want %q", seed, signature, want)
		}
		for _, started := range starts {
			if started[0].terminalizations() != 1 || started[0].forceCalls() != 0 {
				t.Fatalf("seed %d %q terminal/force counts = %d/%d, want 1/0", seed, lastAuthoredChild(started[0].start.path), started[0].terminalizations(), started[0].forceCalls())
			}
		}
		if runner.activeCount() != 0 {
			t.Fatalf("seed %d active graph executions = %d, want 0", seed, runner.activeCount())
		}
		if _, ok := result.Output(); ok {
			t.Fatalf("seed %d non-success graph exposed output", seed)
		}
		assertSettledPathsNeverCommit(t, boundary.base.snapshot())
	}
	if len(seenOrders) != conformancePermutations {
		t.Fatalf("runtime stress exercised %d distinct completion schedules, want %d", len(seenOrders), conformancePermutations)
	}
}

func TestStructuredControlFailFastCancellationConformance(t *testing.T) {
	empty := value.EmptyContract()
	definition := testDefinition(t, workflow.GraphDraft{
		Inputs: empty, Outputs: empty,
		Nodes: []workflow.NodeDraft{
			testLeaf("trigger", empty, empty),
			testLeaf("cooperative", empty, empty),
			testLeaf("noncooperative", empty, empty),
			testLeaf("later", empty, empty),
		},
		Edges: []workflow.EdgeDraft{prestigeEdge(workflow.Child, "trigger", workflow.Child, "later")},
	})
	runner := newMapTestRunner()
	boundary := newPathRecordingBoundary(&eventTrace{})
	execution := startMapTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 3, CancellationGrace: 100 * time.Millisecond})
	starts := prestigeStartsByName(runner.waitStarts(t, 3))
	for _, name := range []string{"trigger", "cooperative", "noncooperative"} {
		requirePrestigeStartCount(t, starts, name, 1)
	}
	cooperativeDone := make(chan struct{})
	cooperativeCompletion := mustLeafCancelled(context.Canceled)
	go func() {
		<-starts["cooperative"][0].cancelObserved
		starts["cooperative"][0].complete(cooperativeCompletion)
		close(cooperativeDone)
	}()

	triggerErr := errors.New("controlled fail-fast trigger")
	starts["trigger"][0].complete(mustLeafFailure(t, MechanicalFailure, triggerErr))
	waitClosed(t, starts["cooperative"][0].cancelObserved, "cooperative sibling did not observe cancellation")
	waitClosed(t, starts["noncooperative"][0].cancelObserved, "noncooperative sibling did not observe cancellation")
	waitClosed(t, cooperativeDone, "cooperative sibling did not terminalize after cancellation")
	result := waitResult(t, execution)
	waitClosed(t, starts["noncooperative"][0].forceObserved, "noncooperative sibling was not force-stopped after grace")

	want := stressResultSignature(normalize([]diagnostic{
		failed(Path{}.AuthoredChild("trigger"), MechanicalFailure, triggerErr),
		parentCancelled(Path{}.AuthoredChild("cooperative")),
		parentCancelled(Path{}.AuthoredChild("noncooperative")),
	}, nil))
	if got := stressResultSignature(result); got != want {
		t.Fatalf("fail-fast result = %q, want %q", got, want)
	}
	for name, counts := range map[string][2]int{
		"trigger":        {1, 0},
		"cooperative":    {1, 0},
		"noncooperative": {1, 1},
	} {
		started := starts[name][0]
		if started.terminalizations() != counts[0] || started.forceCalls() != counts[1] {
			t.Fatalf("%q terminal/force counts = %d/%d, want %d/%d", name, started.terminalizations(), started.forceCalls(), counts[0], counts[1])
		}
	}
	if runner.count() != 3 {
		t.Fatalf("fail-fast started %d leaves, want 3 and no dependent later leaf", runner.count())
	}
	if runner.activeCount() != 0 {
		t.Fatalf("fail-fast left %d active leaf executions, want quiescence", runner.activeCount())
	}
	if _, ok := result.Output(); ok {
		t.Fatal("fail-fast graph exposed non-success output")
	}
	assertSettledPathsNeverCommit(t, boundary.snapshot())
}

func conformanceSchedule(size, rank int) []int {
	available := make([]int, size)
	factorial := 1
	for index := range available {
		available[index] = index
		factorial *= index + 1
	}
	if factorial != 0 {
		rank %= factorial
	}
	result := make([]int, 0, size)
	for remaining := size; remaining > 0; remaining-- {
		block := factorial / remaining
		choice := rank / block
		rank %= block
		result = append(result, available[choice])
		available = append(available[:choice], available[choice+1:]...)
		factorial = block
	}
	return result
}

type settlementSignalBoundary struct {
	base    *pathRecordingBoundary
	settled chan Path
}

func newSettlementSignalBoundary(trace *eventTrace, capacity int) *settlementSignalBoundary {
	return &settlementSignalBoundary{base: newPathRecordingBoundary(trace), settled: make(chan Path, capacity)}
}

func (b *settlementSignalBoundary) Enter(ctx context.Context, instance Instance) error {
	return b.base.Enter(ctx, instance)
}

func (b *settlementSignalBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	return b.base.Commit(ctx, instance, output)
}

func (b *settlementSignalBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	if err := b.base.Settle(ctx, instance, result); err != nil {
		return err
	}
	b.settled <- instance.Path()
	return nil
}

func (b *settlementSignalBoundary) waitSettled(t *testing.T, want Path) {
	t.Helper()
	select {
	case got := <-b.settled:
		if comparePath(got, want) != 0 {
			t.Fatalf("settled path = %#v, want %#v", got.Components(), want.Components())
		}
	case <-time.After(testTimeout):
		t.Fatalf("path %#v did not settle", want.Components())
	}
}

func TestStructuredControlMapCancellationStress(t *testing.T) {
	definition := mapLeafDefinition(t, value.String(), value.EmptyContract())
	items := make([]value.Value, 8)
	for index := range items {
		items[index] = value.NewString(fmt.Sprintf("item-%d", index))
	}
	input := testValue(t, map[string]value.Value{"items": value.NewList(items...)})
	for seed := int64(0); seed < 32; seed++ {
		runner := newMapTestRunner()
		boundary := newPathRecordingBoundary(&eventTrace{})
		execution := startMapTestExecution(t, definition, input, runner, boundary, Policy{Capacity: len(items)})
		starts := runner.waitStarts(t, len(items))
		byIndex := mapStartsByIndex(t, starts)
		failureIndex := rand.New(rand.NewSource(seed)).Intn(len(items))
		byIndex[failureIndex].complete(mustLeafFailure(t, MechanicalFailure, errors.New("map item failed")))

		result := waitResult(t, execution)
		requireStatus(t, result, Failed)
		primary, _ := result.Primary()
		itemBytes, _, ok := pathMapItem(primary.Path())
		if !ok || !bytes.Equal(itemBytes, items[failureIndex].Canonical()) {
			t.Fatalf("seed %d primary path item = %x/%v, want %x", seed, itemBytes, ok, items[failureIndex].Canonical())
		}
		for index, started := range byIndex {
			wantForce := 1
			if index == failureIndex {
				wantForce = 0
			}
			if started.terminalizations() != 1 || started.forceCalls() != wantForce {
				t.Fatalf("seed %d index %d terminal/force counts = %d/%d, want 1/%d", seed, index, started.terminalizations(), started.forceCalls(), wantForce)
			}
		}
		if runner.activeCount() != 0 {
			t.Fatalf("seed %d active map executions = %d, want 0", seed, runner.activeCount())
		}
		assertSettledPathsNeverCommit(t, boundary.snapshot())
	}
}

func TestStructuredControlCleanupCancellationStress(t *testing.T) {
	definition := finallySiblingDefinition(t, false)
	for seed := 0; seed < 32; seed++ {
		runner := newMapTestRunner()
		boundary := newPathRecordingBoundary(&eventTrace{})
		execution := startMapTestExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 2})
		body := prestigeStartsByName(runner.waitStarts(t, 2))
		requirePrestigeStartCount(t, body, "protected-body", 1)
		requirePrestigeStartCount(t, body, "trigger", 1)
		body["protected-body"][0].complete(mustLeafSuccess(t, emptyValue(t)))
		cleanup := prestigeStartsByName(runner.waitStarts(t, 1))
		requirePrestigeStartCount(t, cleanup, "protected-cleanup", 1)
		body["trigger"][0].complete(mustLeafFailure(t, MechanicalFailure, fmt.Errorf("sibling failure %d", seed)))

		result := waitResult(t, execution)
		requireStatus(t, result, Failed)
		primary, _ := result.Primary()
		if lastAuthoredChild(primary.Path()) != "trigger" {
			t.Fatalf("seed %d primary = %#v, want sibling trigger", seed, primary)
		}
		for name, started := range map[string]*mapTestExecution{
			"protected-body":    body["protected-body"][0],
			"trigger":           body["trigger"][0],
			"protected-cleanup": cleanup["protected-cleanup"][0],
		} {
			wantForce := 0
			if name == "protected-cleanup" {
				wantForce = 1
			}
			if started.terminalizations() != 1 || started.forceCalls() != wantForce {
				t.Fatalf("seed %d %q terminal/force counts = %d/%d, want 1/%d", seed, name, started.terminalizations(), started.forceCalls(), wantForce)
			}
		}
		if runner.activeCount() != 0 {
			t.Fatalf("seed %d active cleanup executions = %d, want 0", seed, runner.activeCount())
		}
		if diagnostic, ok := anyCleanupFailure(result); ok {
			t.Fatalf("seed %d parent cancellation became cleanup failure %#v", seed, diagnostic)
		}
	}
}

func TestBranchBooleanFalseIsStableAcrossTimingPermutations(t *testing.T) {
	definition := branchSelectionDefinition(t, value.Boolean(), []string{"false", "true"})
	input := testValue(t, map[string]value.Value{"selected": value.NewBoolean(false)})
	want := testValue(t, map[string]value.Value{"result": value.NewString("false")})
	for seed := int64(0); seed < conformancePermutations; seed++ {
		runner := newMapTestRunner()
		boundary := newPathRecordingBoundary(&eventTrace{})
		execution := startMapTestExecution(t, definition, input, runner, boundary, Policy{Capacity: 2})
		starts := prestigeStartsByName(runner.waitStarts(t, 2))
		requirePrestigeStartCount(t, starts, "emit", 1)
		requirePrestigeStartCount(t, starts, "noise", 1)
		assertPrestigeInput(t, starts["emit"][0], input)
		assertPrestigeInput(t, starts["noise"][0], emptyValue(t))
		order := rand.New(rand.NewSource(seed)).Perm(2)
		for _, index := range order {
			if index == 0 {
				starts["emit"][0].complete(mustLeafSuccess(t, want))
			} else {
				starts["noise"][0].complete(mustLeafSuccess(t, emptyValue(t)))
			}
		}
		result := waitResult(t, execution)
		requireStatus(t, result, Succeeded)
		output, ok := result.Output()
		if !ok || !output.Equal(want) {
			t.Fatalf("seed %d false-branch output = %x/%v, want %x", seed, output.Canonical(), ok, want.Canonical())
		}
		events := boundary.snapshot()
		assertPrestigePathEvent(t, events, traceEnter, Path{}.AuthoredChild("choose").BranchCase("false"))
		assertNoCaseEvents(t, events, "true")
	}
}

func TestMapObservedFailureLeavesLaterSentinelUninstantiated(t *testing.T) {
	const itemCount = 10_000
	items := make([]value.Value, itemCount)
	items[0] = value.NewString("fail-first")
	for index := 1; index < itemCount-1; index++ {
		items[index] = value.NewString(fmt.Sprintf("filler-%05d", index))
	}
	items[itemCount-1] = value.NewString("sentinel/\x00\xff")
	definition := mapDefinition(t, value.String(), value.EmptyContract(), workflow.GraphDraft{
		Inputs: mapBodyInputs(t, value.String()), Outputs: value.EmptyContract(),
	})
	base := newPathRecordingBoundary(&eventTrace{})
	boundary := &failFirstMapBoundary{base: base, first: items[0].Canonical()}
	execution := startMapTestExecution(t, definition, testValue(t, map[string]value.Value{
		"items": value.NewList(items...),
	}), newMapTestRunner(), boundary, Policy{Capacity: 1})

	result := waitResult(t, execution)
	requireStatus(t, result, Failed)
	primary, _ := result.Primary()
	itemBytes, _, ok := pathMapItem(primary.Path())
	if !ok || !bytes.Equal(itemBytes, items[0].Canonical()) || !errors.Is(primary.Error(), errObservedMapFailure) {
		t.Fatalf("primary = %#v, want observed first-item failure", primary)
	}
	sentinelPath, err := Path{}.AuthoredChild("each").MapItem(items[itemCount-1].Canonical(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasPathEvent(base.snapshot(), traceEnter, sentinelPath) {
		t.Fatalf("sentinel path %#v was instantiated after the earlier failure was observed", sentinelPath.Components())
	}
	if hasPathEvent(base.snapshot(), traceCommit, Path{}.AuthoredChild("each")) {
		t.Fatal("failed map published a boundary output")
	}
}

var errObservedMapFailure = errors.New("observed map body failure")

type failFirstMapBoundary struct {
	base  *pathRecordingBoundary
	first []byte
}

func (b *failFirstMapBoundary) Enter(ctx context.Context, instance Instance) error {
	if err := b.base.Enter(ctx, instance); err != nil {
		return err
	}
	item, _, ok := pathMapItem(instance.Path())
	if ok && bytes.Equal(item, b.first) && len(instance.Path().Components()) == 2 {
		return errObservedMapFailure
	}
	return nil
}

func (b *failFirstMapBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	return b.base.Commit(ctx, instance, output)
}

func (b *failFirstMapBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	return b.base.Settle(ctx, instance, result)
}

func mustLeafTimeout(t *testing.T, err error) LeafCompletion {
	t.Helper()
	completion, constructErr := NewLeafTimeout(err)
	if constructErr != nil {
		t.Fatal(constructErr)
	}
	return completion
}

func stressResultSignature(result Result) string {
	diagnostics := resultDiagnostics(result)
	var signature strings.Builder
	fmt.Fprintf(&signature, "status=%d;", result.Status())
	for _, diagnostic := range diagnostics {
		failure, _ := diagnostic.Failure()
		errorText := ""
		if diagnostic.Error() != nil {
			errorText = diagnostic.Error().Error()
		}
		fmt.Fprintf(&signature, "d=%d,%d,%x,%x,%x,%t,%t,%t;",
			diagnostic.Status(), failure, stressPathBytes(diagnostic.Path()), []byte(errorText), []byte(diagnostic.reason),
			diagnostic.parentCancelled, diagnostic.cleanup, diagnostic.external)
	}
	return signature.String()
}

func stressPathBytes(path Path) []byte {
	var encoded strings.Builder
	for _, component := range path.Components() {
		fmt.Fprintf(&encoded, "%d:", component.Kind())
		switch component.Kind() {
		case AuthoredChildComponent:
			name, _ := component.AuthoredChild()
			fmt.Fprintf(&encoded, "%x", []byte(name))
		case BranchCaseComponent:
			name, _ := component.BranchCase()
			fmt.Fprintf(&encoded, "%x", []byte(name))
		case MapItemComponent:
			item, occurrence, _ := component.MapItem()
			fmt.Fprintf(&encoded, "%x:%d", item, occurrence)
		case LoopIterationComponent:
			iteration, _ := component.LoopIteration()
			fmt.Fprintf(&encoded, "%d", iteration)
		case CleanupComponent:
			encoded.WriteString("cleanup")
		}
		encoded.WriteByte(';')
	}
	return []byte(encoded.String())
}

type prestigeRunner struct {
	inner *mapTestRunner
	trace *eventTrace
}

func newPrestigeRunner(trace *eventTrace) *prestigeRunner {
	return &prestigeRunner{inner: newMapTestRunner(), trace: trace}
}

func (r *prestigeRunner) Start(ctx context.Context, request LeafRequest) (LeafExecution, error) {
	r.trace.record(traceEvent{kind: traceStart, child: lastAuthoredChild(request.Instance().Path())})
	return r.inner.Start(ctx, request)
}

func (r *prestigeRunner) waitStarts(t *testing.T, count int) []*mapTestExecution {
	t.Helper()
	return r.inner.waitStarts(t, count)
}

func prestigeStartsByName(starts []*mapTestExecution) map[string][]*mapTestExecution {
	result := make(map[string][]*mapTestExecution)
	for _, started := range starts {
		name := lastAuthoredChild(started.start.path)
		result[name] = append(result[name], started)
	}
	return result
}

func requirePrestigeStartCount(t *testing.T, starts map[string][]*mapTestExecution, name string, want int) {
	t.Helper()
	if got := len(starts[name]); got != want {
		t.Fatalf("%q starts = %d, want %d; starts = %v", name, got, want, prestigeStartNames(starts))
	}
}

func prestigeStartNames(starts map[string][]*mapTestExecution) []string {
	names := make([]string, 0, len(starts))
	for name, executions := range starts {
		names = append(names, fmt.Sprintf("%s:%d", name, len(executions)))
	}
	sort.Strings(names)
	return names
}

func assertPrestigeInput(t *testing.T, started *mapTestExecution, want value.Value) {
	t.Helper()
	if !started.start.input.Equal(want) {
		t.Fatalf("%q input = %x, want exact %x", lastAuthoredChild(started.start.path), started.start.input.Canonical(), want.Canonical())
	}
}

func assertPrestigeLoopStart(t *testing.T, started *mapTestExecution, iteration uint64, want value.Value) {
	t.Helper()
	got, ok := pathLoopIteration(started.start.path)
	if !ok || got != iteration {
		t.Fatalf("%q path = %#v, want loop iteration %d", lastAuthoredChild(started.start.path), started.start.path.Components(), iteration)
	}
	assertPrestigeInput(t, started, want)
}

func assertPrestigePathEvent(t *testing.T, events []pathBoundaryEvent, kind traceKind, path Path) {
	t.Helper()
	if !hasPathEvent(events, kind, path) {
		t.Fatalf("missing path event kind %d at %#v; events = %#v", kind, path.Components(), events)
	}
}

func assertPrestigeTraceBefore(t *testing.T, trace *eventTrace, firstKind traceKind, firstName string, secondKind traceKind, secondName string) {
	t.Helper()
	first := trace.index(firstKind, firstName)
	second := trace.index(secondKind, secondName)
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("trace (%d,%q) index %d must precede (%d,%q) index %d; trace = %#v", firstKind, firstName, first, secondKind, secondName, second, trace.snapshot())
	}
}

func assertSettledPathsNeverCommit(t *testing.T, events []pathBoundaryEvent) {
	t.Helper()
	for _, event := range events {
		if event.kind != traceSettle {
			continue
		}
		if hasPathEvent(events, traceCommit, event.path) {
			t.Fatalf("non-successful path %#v both settled and committed; events = %#v", event.path.Components(), events)
		}
	}
}

func prestigeDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	documentResults := testContract(t, testRequired(t, "analysis", value.String()))
	documentsType := mustPrestigeListType(t, value.String())
	analysesType := mustPrestigeListType(t, documentResults.ObjectType())
	rootInputs := testContract(t,
		testRequired(t, "documents", documentsType),
		testRequired(t, "selected", value.Boolean()),
		testRequired(t, "request", value.String()),
		testOptional(t, "cleanup_note", value.String()),
	)
	rootOutputs := testContract(t, testRequired(t, "final", value.String()))

	staticInputs := testContract(t, testRequired(t, "request", value.String()))
	staticOutputs := testContract(t, testRequired(t, "finding", value.String()))
	mapInputs := testContract(t, testRequired(t, "documents", documentsType))
	mapOutputs := testContract(t, testRequired(t, "analyses", analysesType))
	mapBodyInputs := mapBodyInputs(t, value.String())
	mapBody := workflow.GraphDraft{
		Inputs: mapBodyInputs, Outputs: documentResults,
		Nodes: []workflow.NodeDraft{testLeaf("analyze-document", mapBodyInputs, documentResults)},
		Edges: []workflow.EdgeDraft{
			prestigeEdge(workflow.Boundary, "", workflow.Child, "analyze-document",
				workflow.BindingDraft{From: []string{"item"}, To: "item"},
				workflow.BindingDraft{From: []string{"index"}, To: "index"}),
			prestigeEdge(workflow.Child, "analyze-document", workflow.Boundary, "",
				workflow.BindingDraft{From: []string{"analysis"}, To: "analysis"}),
		},
	}

	reviewInputs := testContract(t,
		testRequired(t, "static", value.String()),
		testRequired(t, "analyses", analysesType),
	)
	reviewOutputs := testContract(t, testRequired(t, "evidence", value.String()))
	secondaryInputs := testContract(t, testRequired(t, "analyses", analysesType))
	reviewParallel := workflow.ParallelDraft{Graph: workflow.GraphDraft{
		Inputs: reviewInputs, Outputs: reviewOutputs,
		Nodes: []workflow.NodeDraft{
			testLeaf("primary-review", reviewInputs, reviewOutputs),
			testLeaf("secondary-review", secondaryInputs, value.EmptyContract()),
		},
		Edges: []workflow.EdgeDraft{
			prestigeEdge(workflow.Boundary, "", workflow.Child, "primary-review",
				workflow.BindingDraft{From: []string{"static"}, To: "static"},
				workflow.BindingDraft{From: []string{"analyses"}, To: "analyses"}),
			prestigeEdge(workflow.Boundary, "", workflow.Child, "secondary-review",
				workflow.BindingDraft{From: []string{"analyses"}, To: "analyses"}),
			prestigeEdge(workflow.Child, "primary-review", workflow.Boundary, "",
				workflow.BindingDraft{From: []string{"evidence"}, To: "evidence"}),
		},
	}}

	protectedInputs := testContract(t,
		testRequired(t, "static", value.String()),
		testRequired(t, "analyses", analysesType),
		testRequired(t, "request", value.String()),
		testOptional(t, "note", value.String()),
	)
	innerCleanupInputs := testContract(t,
		testOptional(t, "required_copy", value.String()),
		testOptional(t, "optional_copy", value.String()),
		testOptional(t, "outcome", finallyOutcomeType(t)),
		testOptional(t, "evidence", value.String()),
	)
	protected := workflow.GraphDraft{
		Inputs: protectedInputs, Outputs: reviewOutputs,
		Nodes: []workflow.NodeDraft{{Name: "review", Parallel: &reviewParallel}},
		Edges: []workflow.EdgeDraft{
			prestigeEdge(workflow.Boundary, "", workflow.Child, "review",
				workflow.BindingDraft{From: []string{"static"}, To: "static"},
				workflow.BindingDraft{From: []string{"analyses"}, To: "analyses"}),
			prestigeEdge(workflow.Child, "review", workflow.Boundary, "",
				workflow.BindingDraft{From: []string{"evidence"}, To: "evidence"}),
		},
		Finally: &workflow.FinallyDraft{
			Graph: finallyNamedCleanupGraph(t, innerCleanupInputs, "inner-cleanup"),
			Bindings: []workflow.CleanupBindingDraft{
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupInput, Path: []string{"request"}}, To: "required_copy"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupInput, Path: []string{"note"}}, To: "optional_copy"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupOutcome}, To: "outcome"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupChild, Child: "review", Path: []string{"evidence"}}, To: "evidence"},
			},
		},
	}

	loopInputs := testContract(t, testRequired(t, "seed", value.String()))
	loopOutputs := testContract(t,
		testRequired(t, "candidate", value.String()),
		testRequired(t, "passed", value.Boolean()),
	)
	loopBodyInputs := testContract(t,
		testRequired(t, "initial", loopInputs.ObjectType()),
		testOptional(t, "previous", loopOutputs.ObjectType()),
		testRequired(t, "iteration", value.Integer()),
	)
	workerOutputs := testContract(t, testRequired(t, "candidate", value.String()))
	judgeInputs := testContract(t,
		testRequired(t, "candidate", value.String()),
		testRequired(t, "iteration", value.Integer()),
	)
	judgeOutputs := testContract(t, testRequired(t, "passed", value.Boolean()))
	loopBody := workflow.GraphDraft{
		Inputs: loopBodyInputs, Outputs: loopOutputs,
		Nodes: []workflow.NodeDraft{
			testLeaf("worker", loopBodyInputs, workerOutputs),
			testLeaf("judge", judgeInputs, judgeOutputs),
		},
		Edges: []workflow.EdgeDraft{
			prestigeEdge(workflow.Boundary, "", workflow.Child, "worker",
				workflow.BindingDraft{From: []string{"initial"}, To: "initial"},
				workflow.BindingDraft{From: []string{"previous"}, To: "previous"},
				workflow.BindingDraft{From: []string{"iteration"}, To: "iteration"}),
			prestigeEdge(workflow.Boundary, "", workflow.Child, "judge",
				workflow.BindingDraft{From: []string{"iteration"}, To: "iteration"}),
			prestigeEdge(workflow.Child, "worker", workflow.Child, "judge",
				workflow.BindingDraft{From: []string{"candidate"}, To: "candidate"}),
			prestigeEdge(workflow.Child, "worker", workflow.Boundary, "",
				workflow.BindingDraft{From: []string{"candidate"}, To: "candidate"}),
			prestigeEdge(workflow.Child, "judge", workflow.Boundary, "",
				workflow.BindingDraft{From: []string{"passed"}, To: "passed"}),
		},
	}

	routeInputs := testContract(t, testRequired(t, "selected", value.Boolean()))
	routeOutputs := testContract(t, testRequired(t, "route", value.String()))
	routeCases := []workflow.CaseDraft{
		{Name: "false", Graph: prestigeRouteCase(t, "route-false", routeInputs, routeOutputs)},
		{Name: "true", Graph: prestigeRouteCase(t, "route-true", routeInputs, routeOutputs)},
	}
	gateInputs := testContract(t,
		testRequired(t, "passed", value.Boolean()),
		testOptional(t, "reason", value.String()),
	)
	aggregateInputs := testContract(t,
		testRequired(t, "candidate", value.String()),
		testRequired(t, "route", value.String()),
		testRequired(t, "evidence", value.String()),
		testRequired(t, "analyses", analysesType),
	)
	outerCleanupInputs := testContract(t,
		testRequired(t, "outcome", finallyOutcomeType(t)),
		testOptional(t, "final", value.String()),
	)

	return testDefinition(t, workflow.GraphDraft{
		Inputs: rootInputs, Outputs: rootOutputs,
		Nodes: []workflow.NodeDraft{
			testLeaf("static-analysis", staticInputs, staticOutputs),
			{Name: "analyze-documents", Map: &workflow.MapDraft{
				Inputs: mapInputs, Outputs: mapOutputs, Collection: "documents", Result: "analyses", Body: mapBody,
			}},
			{Name: "protected", Graph: &protected},
			{Name: "revise", Loop: &workflow.LoopDraft{
				Inputs: loopInputs, Outputs: loopOutputs, Maximum: 3, Termination: []string{"passed"}, Body: loopBody,
			}},
			{Name: "approve", Leaf: &workflow.LeafDraft{Kind: workflow.Gate, Inputs: gateInputs, Outputs: value.EmptyContract()}},
			{Name: "route", Branch: &workflow.BranchDraft{
				Inputs: routeInputs, Outputs: routeOutputs, Selector: "selected", Cases: routeCases,
			}},
			testLeaf("aggregate", aggregateInputs, rootOutputs),
		},
		Edges: []workflow.EdgeDraft{
			prestigeEdge(workflow.Boundary, "", workflow.Child, "static-analysis",
				workflow.BindingDraft{From: []string{"request"}, To: "request"}),
			prestigeEdge(workflow.Boundary, "", workflow.Child, "analyze-documents",
				workflow.BindingDraft{From: []string{"documents"}, To: "documents"}),
			prestigeEdge(workflow.Child, "static-analysis", workflow.Child, "protected",
				workflow.BindingDraft{From: []string{"finding"}, To: "static"}),
			prestigeEdge(workflow.Child, "analyze-documents", workflow.Child, "protected",
				workflow.BindingDraft{From: []string{"analyses"}, To: "analyses"}),
			prestigeEdge(workflow.Boundary, "", workflow.Child, "protected",
				workflow.BindingDraft{From: []string{"request"}, To: "request"},
				workflow.BindingDraft{From: []string{"cleanup_note"}, To: "note"}),
			prestigeEdge(workflow.Child, "protected", workflow.Child, "revise",
				workflow.BindingDraft{From: []string{"evidence"}, To: "seed"}),
			prestigeEdge(workflow.Child, "revise", workflow.Child, "approve",
				workflow.BindingDraft{From: []string{"passed"}, To: "passed"}),
			prestigeEdge(workflow.Boundary, "", workflow.Child, "route",
				workflow.BindingDraft{From: []string{"selected"}, To: "selected"}),
			prestigeEdge(workflow.Child, "revise", workflow.Child, "aggregate",
				workflow.BindingDraft{From: []string{"candidate"}, To: "candidate"}),
			prestigeEdge(workflow.Child, "route", workflow.Child, "aggregate",
				workflow.BindingDraft{From: []string{"route"}, To: "route"}),
			prestigeEdge(workflow.Child, "protected", workflow.Child, "aggregate",
				workflow.BindingDraft{From: []string{"evidence"}, To: "evidence"}),
			prestigeEdge(workflow.Child, "analyze-documents", workflow.Child, "aggregate",
				workflow.BindingDraft{From: []string{"analyses"}, To: "analyses"}),
			prestigeEdge(workflow.Child, "approve", workflow.Child, "aggregate"),
			prestigeEdge(workflow.Child, "aggregate", workflow.Boundary, "",
				workflow.BindingDraft{From: []string{"final"}, To: "final"}),
		},
		Finally: &workflow.FinallyDraft{
			Graph: finallyNamedCleanupGraph(t, outerCleanupInputs, "outer-cleanup"),
			Bindings: []workflow.CleanupBindingDraft{
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupOutcome}, To: "outcome"},
				{From: workflow.CleanupSourceDraft{Kind: workflow.CleanupChild, Child: "aggregate", Path: []string{"final"}}, To: "final"},
			},
		},
	})
}

func prestigeRouteCase(t *testing.T, leafName string, inputs, outputs value.Contract) workflow.GraphDraft {
	t.Helper()
	return workflow.GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Nodes: []workflow.NodeDraft{testLeaf(leafName, value.EmptyContract(), outputs)},
		Edges: []workflow.EdgeDraft{prestigeEdge(workflow.Child, leafName, workflow.Boundary, "",
			workflow.BindingDraft{From: []string{"route"}, To: "route"})},
	}
}

func prestigeEdge(fromKind workflow.EndpointKind, fromChild string, toKind workflow.EndpointKind, toChild string, bindings ...workflow.BindingDraft) workflow.EdgeDraft {
	return workflow.EdgeDraft{
		From:     workflow.EndpointDraft{Kind: fromKind, Child: fromChild},
		To:       workflow.EndpointDraft{Kind: toKind, Child: toChild},
		Bindings: bindings,
	}
}

func mustPrestigeListType(t *testing.T, element value.Type) value.Type {
	t.Helper()
	typ, err := value.List(element)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}
