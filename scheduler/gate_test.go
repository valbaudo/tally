package scheduler

import (
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

func gateDefinition(t *testing.T, passed bool, reason string) workflow.Definition {
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
	return testDefinition(t, workflow.GraphDraft{
		Inputs: value.EmptyContract(), Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{{Name: "check", Leaf: &workflow.LeafDraft{Kind: workflow.Gate, Inputs: inputs, Outputs: value.EmptyContract()}, Literals: literals}},
	})
}
