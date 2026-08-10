package scheduler

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

type pathBoundaryEvent struct {
	kind   traceKind
	path   Path
	output value.Value
	status Status
}

type pathRecordingBoundary struct {
	base *recordingBoundary

	mu     sync.Mutex
	events []pathBoundaryEvent
}

func newPathRecordingBoundary(trace *eventTrace) *pathRecordingBoundary {
	return &pathRecordingBoundary{base: newRecordingBoundary(trace)}
}

func (b *pathRecordingBoundary) Enter(ctx context.Context, instance Instance) error {
	b.record(pathBoundaryEvent{kind: traceEnter, path: instance.Path()})
	return b.base.Enter(ctx, instance)
}

func (b *pathRecordingBoundary) Commit(ctx context.Context, instance Instance, output value.Value) error {
	b.record(pathBoundaryEvent{kind: traceCommit, path: instance.Path(), output: output})
	return b.base.Commit(ctx, instance, output)
}

func (b *pathRecordingBoundary) Settle(ctx context.Context, instance Instance, result Result) error {
	b.record(pathBoundaryEvent{kind: traceSettle, path: instance.Path(), status: result.Status()})
	return b.base.Settle(ctx, instance, result)
}

func (b *pathRecordingBoundary) record(event pathBoundaryEvent) {
	b.mu.Lock()
	b.events = append(b.events, event)
	b.mu.Unlock()
}

func (b *pathRecordingBoundary) snapshot() []pathBoundaryEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]pathBoundaryEvent(nil), b.events...)
}

func TestBranchBooleanEntersOnlySelectedCaseBeforeItsChildren(t *testing.T) {
	definition := branchSelectionDefinition(t, value.Boolean(), []string{"false", "true"})
	input := testValue(t, map[string]value.Value{"selected": value.NewBoolean(true)})
	trace := &eventTrace{}
	runner := newControlledRunner(trace)
	boundary := newPathRecordingBoundary(trace)
	execution := startPathExecution(t, definition, input, runner, boundary, Policy{Capacity: 2})

	if got := sortedStrings(runner.waitStarts(t, 2)); got[0] != "emit" || got[1] != "noise" {
		t.Fatalf("started = %v, want selected case children", got)
	}
	assertSelectedCaseEnteredFirst(t, boundary.snapshot(), "true")
	assertNoCaseEvents(t, boundary.snapshot(), "false")
	if got := runnerInput(t, runner, "emit"); !got.Equal(input) {
		t.Fatalf("selected case input = %x, want exact branch input %x", got.Canonical(), input.Canonical())
	}

	want := testValue(t, map[string]value.Value{"result": value.NewString("true")})
	runner.execution(t, "emit").complete(mustLeafSuccess(t, want))
	runner.execution(t, "noise").complete(mustLeafSuccess(t, emptyValue(t)))
	result := waitResult(t, execution)
	requireStatus(t, result, Succeeded)
	output, _ := result.Output()
	if !output.Equal(want) {
		t.Fatalf("output = %x, want %x", output.Canonical(), want.Canonical())
	}
	events := boundary.snapshot()
	assertNoCaseEvents(t, events, "false")
	casePath := Path{}.AuthoredChild("choose").BranchCase("true")
	branchPath := Path{}.AuthoredChild("choose")
	caseCommit := pathEventIndex(events, traceCommit, casePath)
	branchCommit := pathEventIndex(events, traceCommit, branchPath)
	committed, ok := committedAt(events, branchPath)
	if caseCommit < 0 || branchCommit < 0 || caseCommit >= branchCommit || !ok || !committed.Equal(want) {
		t.Fatalf("case commit %d must precede exact branch commit %d (%v): %#v", caseCommit, branchCommit, ok, events)
	}
}

func TestBranchMixedScalarEnumUsesExactKindAndCanonicalScalarSelection(t *testing.T) {
	specialLiteral := branchLiteral(t, `"quoted\"/nul\u0000"`)
	integerLiteral := branchLiteral(t, `1`)
	numberLiteral := branchLiteral(t, `1.0`)
	booleanLiteral := branchLiteral(t, `true`)
	nullLiteral := branchLiteral(t, `null`)
	members := []value.Literal{specialLiteral, integerLiteral, numberLiteral, booleanLiteral, nullLiteral}
	selectorType, err := value.Enum(members...)
	if err != nil {
		t.Fatal(err)
	}
	caseNames := make([]string, len(members))
	for index, member := range members {
		caseNames[index] = string(member.Bytes())
	}
	definition := branchSelectionDefinition(t, selectorType, caseNames)
	integer, err := value.NewInteger("1")
	if err != nil {
		t.Fatal(err)
	}
	number, err := value.NewNumber("1.0")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		selector value.Value
		caseName string
	}{
		{name: "string bytes", selector: value.NewString("quoted\"/nul\x00"), caseName: string(specialLiteral.Bytes())},
		{name: "integer kind", selector: integer, caseName: string(integerLiteral.Bytes())},
		{name: "number kind", selector: number, caseName: string(numberLiteral.Bytes())},
		{name: "boolean kind", selector: value.NewBoolean(true), caseName: string(booleanLiteral.Bytes())},
		{name: "null kind", selector: value.NewNull(), caseName: string(nullLiteral.Bytes())},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := testValue(t, map[string]value.Value{"selected": testCase.selector})
			var firstSelectedPath Path
			for seed := int64(0); seed < 8; seed++ {
				trace := &eventTrace{}
				runner := newControlledRunner(trace)
				boundary := newPathRecordingBoundary(trace)
				execution := startPathExecution(t, definition, input, runner, boundary, Policy{Capacity: 2})
				runner.waitStarts(t, 2)

				events := boundary.snapshot()
				selectedPath := assertSelectedCaseEnteredFirst(t, events, testCase.caseName)
				if seed == 0 {
					firstSelectedPath = selectedPath
				} else if comparePath(firstSelectedPath, selectedPath) != 0 {
					t.Fatalf("seed %d selected path changed: %#v != %#v", seed, selectedPath.Components(), firstSelectedPath.Components())
				}
				for _, other := range caseNames {
					if other != testCase.caseName {
						assertNoCaseEvents(t, events, other)
					}
				}
				if got := runnerInput(t, runner, "emit"); !got.Equal(input) {
					t.Fatalf("seed %d selected case changed selector bytes or kind: %x != %x", seed, got.Canonical(), input.Canonical())
				}

				want := testValue(t, map[string]value.Value{"result": value.NewString(testCase.caseName)})
				emit := runner.execution(t, "emit")
				noise := runner.execution(t, "noise")
				completions := []func(){
					func() { emit.complete(mustLeafSuccess(t, want)) },
					func() { noise.complete(mustLeafSuccess(t, emptyValue(t))) },
				}
				random := rand.New(rand.NewSource(seed))
				if random.Intn(2) == 1 {
					completions[0], completions[1] = completions[1], completions[0]
				}
				time.Sleep(time.Duration(random.Intn(3)) * time.Millisecond)
				completions[0]()
				time.Sleep(time.Duration(random.Intn(3)) * time.Millisecond)
				completions[1]()

				result := waitResult(t, execution)
				requireStatus(t, result, Succeeded)
				output, _ := result.Output()
				if !output.Equal(want) {
					t.Fatalf("seed %d output = %x, want %x", seed, output.Canonical(), want.Canonical())
				}
			}
		})
	}
}

func TestBranchSelectorResolutionPreservesScalarKindAndOpaqueStringBytes(t *testing.T) {
	t.Run("kind", func(t *testing.T) {
		integerLiteral := branchLiteral(t, `1`)
		otherLiteral := branchLiteral(t, `"other"`)
		selectorType, err := value.Enum(integerLiteral, otherLiteral)
		if err != nil {
			t.Fatal(err)
		}
		definition := branchSelectionDefinition(t, selectorType, []string{string(integerLiteral.Bytes()), string(otherLiteral.Bytes())})
		scope, ok := definition.Root().Nodes()[0].Scope()
		if !ok {
			t.Fatal("compiled branch node omitted its scope")
		}
		branch, ok := scope.Branch()
		if !ok {
			t.Fatal("compiled branch scope omitted its branch")
		}
		sameJSONDifferentKind, err := value.NewNumber("1")
		if err != nil {
			t.Fatal(err)
		}
		input := testValue(t, map[string]value.Value{"selected": sameJSONDifferentKind})
		if selected, err := resolveBranchCase(branch, input); err == nil {
			t.Fatalf("number selector resolved to integer case %q despite its distinct scalar kind", selected)
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		invalidUTF8 := value.NewString(string([]byte{'a', 0xff, 'b'}))
		if ordinary, ok := scalarOrdinaryJSON(invalidUTF8); ok {
			t.Fatalf("invalid UTF-8 selector bytes were replaced during ordinary JSON selection: %x", ordinary)
		}
	})
}

func TestBranchSelectedCaseNonSuccessPropagatesWithoutBranchCommit(t *testing.T) {
	timeoutErr := errors.New("selected case timed out")
	cancelErr := errors.New("backend cancelled selected case")
	timeout, err := NewLeafTimeout(timeoutErr)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := NewLeafCancelled(cancelErr)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		mode        branchNonSuccessMode
		completion  LeafCompletion
		wantStatus  Status
		wantFailure FailureKind
		wantError   error
		wantReason  string
	}{
		{name: "rejection", mode: branchRejects, wantStatus: Rejected, wantReason: "selected denied"},
		{name: "failure", mode: branchRunsLeaf, completion: mustLeafFailure(t, MechanicalFailure, errBoom), wantStatus: Failed, wantFailure: MechanicalFailure, wantError: errBoom},
		{name: "timeout", mode: branchRunsLeaf, completion: timeout, wantStatus: Failed, wantFailure: TimeoutFailure, wantError: timeoutErr},
		{name: "cancellation", mode: branchRunsLeaf, completion: cancelled, wantStatus: Cancelled, wantError: cancelErr},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			definition := branchNonSuccessDefinition(t, testCase.mode)
			trace := &eventTrace{}
			runner := newControlledRunner(trace)
			boundary := newPathRecordingBoundary(trace)
			execution := startPathExecution(t, definition, emptyValue(t), runner, boundary, Policy{Capacity: 1})
			if testCase.mode == branchRunsLeaf {
				runner.execution(t, "selected").complete(testCase.completion)
			}
			result := waitResult(t, execution)
			requireStatus(t, result, testCase.wantStatus)
			if _, public := result.Output(); public {
				t.Fatal("non-successful branch exposed a public output")
			}
			primary, _ := result.Primary()
			wantPath := Path{}.AuthoredChild("choose").BranchCase("true").AuthoredChild("selected")
			if comparePath(primary.Path(), wantPath) != 0 {
				t.Fatalf("primary path = %#v, want %#v", primary.Path().Components(), wantPath.Components())
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
			if testCase.wantReason != "" {
				reason, ok := primary.Reason()
				if !ok || reason != testCase.wantReason {
					t.Fatalf("reason = %q/%v, want %q", reason, ok, testCase.wantReason)
				}
			}
			branchPath := Path{}.AuthoredChild("choose")
			if hasPathEvent(boundary.snapshot(), traceCommit, branchPath) {
				t.Fatal("non-successful branch committed its boundary output")
			}
		})
	}
}

func branchSelectionDefinition(t *testing.T, selectorType value.Type, caseNames []string) workflow.Definition {
	t.Helper()
	inputs := testContract(t, testRequired(t, "selected", selectorType))
	outputs := testContract(t, testRequired(t, "result", value.String()))
	cases := make([]workflow.CaseDraft, 0, len(caseNames))
	for _, caseName := range caseNames {
		cases = append(cases, workflow.CaseDraft{Name: caseName, Graph: workflow.GraphDraft{
			Inputs: inputs, Outputs: outputs,
			Nodes: []workflow.NodeDraft{
				testLeaf("emit", inputs, outputs),
				testLeaf("noise", value.EmptyContract(), value.EmptyContract()),
			},
			Edges: []workflow.EdgeDraft{
				{From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "emit"}, Bindings: []workflow.BindingDraft{{From: []string{"selected"}, To: "selected"}}},
				{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "emit"}, To: workflow.EndpointDraft{Kind: workflow.Boundary}, Bindings: []workflow.BindingDraft{{From: []string{"result"}, To: "result"}}},
			},
		}})
	}
	return testDefinition(t, workflow.GraphDraft{
		Inputs: inputs, Outputs: outputs,
		Nodes: []workflow.NodeDraft{{Name: "choose", Branch: &workflow.BranchDraft{
			Inputs: inputs, Outputs: outputs, Selector: "selected", Cases: cases,
		}}},
		Edges: []workflow.EdgeDraft{
			{From: workflow.EndpointDraft{Kind: workflow.Boundary}, To: workflow.EndpointDraft{Kind: workflow.Child, Child: "choose"}, Bindings: []workflow.BindingDraft{{From: []string{"selected"}, To: "selected"}}},
			{From: workflow.EndpointDraft{Kind: workflow.Child, Child: "choose"}, To: workflow.EndpointDraft{Kind: workflow.Boundary}, Bindings: []workflow.BindingDraft{{From: []string{"result"}, To: "result"}}},
		},
	})
}

type branchNonSuccessMode uint8

const (
	branchRunsLeaf branchNonSuccessMode = iota + 1
	branchRejects
)

func branchNonSuccessDefinition(t *testing.T, mode branchNonSuccessMode) workflow.Definition {
	t.Helper()
	inputs := testContract(t, testRequired(t, "selected", value.Boolean()))
	selected := testLeaf("selected", value.EmptyContract(), value.EmptyContract())
	if mode == branchRejects {
		selected = testGateNode(t, "selected", false, "selected denied")
	}
	trueCase := workflow.GraphDraft{Inputs: inputs, Outputs: value.EmptyContract(), Nodes: []workflow.NodeDraft{selected}}
	falseCase := workflow.GraphDraft{Inputs: inputs, Outputs: value.EmptyContract()}
	trueLiteral := branchLiteral(t, `true`)
	return testDefinition(t, workflow.GraphDraft{
		Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{{
			Name: "choose",
			Branch: &workflow.BranchDraft{
				Inputs: inputs, Outputs: value.EmptyContract(), Selector: "selected",
				Cases: []workflow.CaseDraft{{Name: "false", Graph: falseCase}, {Name: "true", Graph: trueCase}},
			},
			Literals: []workflow.LiteralBindingDraft{{Input: "selected", Value: trueLiteral}},
		}},
	})
}

func branchLiteral(t *testing.T, source string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(source))
	if err != nil {
		t.Fatalf("ParseLiteral(%q) error = %v", source, err)
	}
	return literal
}

func assertSelectedCaseEnteredFirst(t *testing.T, events []pathBoundaryEvent, caseName string) Path {
	t.Helper()
	casePath := Path{}.AuthoredChild("choose").BranchCase(caseName)
	caseEnter := -1
	firstChild := -1
	for index, event := range events {
		if event.kind == traceEnter && comparePath(event.path, casePath) == 0 {
			caseEnter = index
		}
		components := event.path.Components()
		if event.kind == traceEnter && len(components) == 3 && comparePath(Path{components: components[:2]}, casePath) == 0 && firstChild == -1 {
			firstChild = index
		}
	}
	if caseEnter < 0 || firstChild < 0 || caseEnter >= firstChild {
		t.Fatalf("selected case enter %d must precede first child enter %d; events = %#v", caseEnter, firstChild, events)
	}
	return casePath
}

func assertNoCaseEvents(t *testing.T, events []pathBoundaryEvent, caseName string) {
	t.Helper()
	for _, event := range events {
		for _, component := range event.path.Components() {
			if selected, ok := component.BranchCase(); ok && selected == caseName {
				t.Fatalf("unselected case %q produced runtime event %#v", caseName, event)
			}
		}
	}
}

func hasPathEvent(events []pathBoundaryEvent, kind traceKind, path Path) bool {
	return pathEventIndex(events, kind, path) >= 0
}

func pathEventIndex(events []pathBoundaryEvent, kind traceKind, path Path) int {
	for index, event := range events {
		if event.kind == kind && comparePath(event.path, path) == 0 {
			return index
		}
	}
	return -1
}

func startPathExecution(t *testing.T, definition workflow.Definition, input value.Value, runner LeafRunner, boundary Boundary, policy Policy) *Execution {
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
