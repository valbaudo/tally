package scheduler

import (
	"errors"
	"reflect"
	"testing"

	"github.com/valbaudo/dawn/value"
)

func TestPathDoesNotExposePointerIdentityEquality(t *testing.T) {
	if reflect.TypeOf(Path{}).Comparable() {
		t.Fatal("Path became comparable; independently built semantic paths would expose pointer identity through == and map keys")
	}
}

func TestPathComponentsRetainArbitraryBytesAndDefensiveCopies(t *testing.T) {
	item := []byte{'/', 0, 0xff, '.', 'x'}
	child := "/\x00\xff.alpha"
	caseName := ".case/\x00\xfe"

	path := Path{}.AuthoredChild(child).BranchCase(caseName)
	path, err := path.MapItem(item, 2)
	if err != nil {
		t.Fatal(err)
	}
	path, err = path.LoopIteration(1)
	if err != nil {
		t.Fatal(err)
	}
	path = path.Cleanup()

	components := path.Components()
	if len(components) != 5 {
		t.Fatalf("component count = %d, want 5", len(components))
	}
	if got, ok := components[0].AuthoredChild(); !ok || got != child {
		t.Fatalf("authored child = %q, %v", got, ok)
	}
	if got, ok := components[1].BranchCase(); !ok || got != caseName {
		t.Fatalf("branch case = %q, %v", got, ok)
	}
	gotItem, occurrence, ok := components[2].MapItem()
	if !ok || occurrence != 2 || string(gotItem) != string(item) {
		t.Fatalf("map item = %q, occurrence %d, ok %v", gotItem, occurrence, ok)
	}
	if got, ok := components[3].LoopIteration(); !ok || got != 1 {
		t.Fatalf("loop iteration = %d, %v", got, ok)
	}
	if !components[4].IsCleanup() {
		t.Fatal("cleanup component not retained")
	}

	gotItem[0] = 'x'
	components[0] = Component{}
	again := path.Components()
	againItem, _, _ := again[2].MapItem()
	if again[0].Kind() != AuthoredChildComponent || string(againItem) != string(item) {
		t.Fatalf("path leaked mutable components: %#v, %q", again[0], againItem)
	}
}

func TestMapItemUsesZeroBasedOccurrenceOnlyForIdenticalCanonicalBytes(t *testing.T) {
	first, err := (Path{}).MapItem([]byte{0, 0xff, '/'}, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Path{}).MapItem([]byte{0, 0xff, '/'}, 1)
	if err != nil {
		t.Fatal(err)
	}

	firstItem, firstOccurrence, _ := first.Components()[0].MapItem()
	secondItem, secondOccurrence, _ := second.Components()[0].MapItem()
	if string(firstItem) != string(secondItem) || firstOccurrence != 0 || secondOccurrence != 1 {
		t.Fatalf("map identity = (%q, %d), (%q, %d)", firstItem, firstOccurrence, secondItem, secondOccurrence)
	}
	if comparePath(first, second) >= 0 || comparePath(second, first) <= 0 {
		t.Fatal("duplicate occurrence does not produce a strict order")
	}
}

func TestLoopIterationIsOneBased(t *testing.T) {
	if _, err := (Path{}).LoopIteration(0); err == nil {
		t.Fatal("zero loop iteration was accepted")
	}
	path, err := (Path{}).LoopIteration(1)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := path.Components()[0].LoopIteration()
	if !ok || got != 1 {
		t.Fatalf("loop iteration = %d, %v", got, ok)
	}
}

func TestComparePathIsStrictStructuredLexicalOrder(t *testing.T) {
	paths := []Path{
		Path{}.AuthoredChild("a").BranchCase("x"),
		Path{}.AuthoredChild("a"),
		Path{}.AuthoredChild("a").BranchCase("y"),
		Path{}.AuthoredChild("b"),
	}
	for i := range paths {
		for j := range paths {
			got := comparePath(paths[i], paths[j])
			if i == j && got != 0 {
				t.Fatalf("path %d compared to itself = %d", i, got)
			}
			if i != j && got == 0 {
				t.Fatalf("distinct paths %d and %d compare equal", i, j)
			}
			if got != -comparePath(paths[j], paths[i]) {
				t.Fatalf("comparison is not antisymmetric for %d and %d", i, j)
			}
		}
	}
	if comparePath(paths[1], paths[0]) >= 0 || comparePath(paths[0], paths[2]) >= 0 || comparePath(paths[2], paths[3]) >= 0 {
		t.Fatal("paths are not ordered by structured lexical components")
	}
}

func TestResultOnlyExposesCommittedOutputForSuccess(t *testing.T) {
	output := value.NewString("committed")
	success, err := NewSucceededResult(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := success.Output(); !ok || got.Kind() != value.StringKind {
		t.Fatalf("success output = %#v, %v", got, ok)
	} else if text, _ := got.Text(); text != "committed" {
		t.Fatalf("success text = %q", text)
	}

	nonSuccess := normalize([]diagnostic{rejected(Path{}.AuthoredChild("gate"), "no")}, nil)
	if _, ok := nonSuccess.Output(); ok {
		t.Fatal("non-success result exposed output")
	}
	if nonSuccess.Status() != Rejected {
		t.Fatalf("status = %v, want Rejected", nonSuccess.Status())
	}
	if _, ok := nonSuccess.Primary(); !ok {
		t.Fatal("non-success result has no primary diagnostic")
	}
}

func TestLeafCompletionRejectsMalformedCombinations(t *testing.T) {
	if (LeafCompletion{}).Valid() {
		t.Fatal("zero completion is valid")
	}
	if _, err := NewLeafSucceeded(value.Value{}); err == nil {
		t.Fatal("invalid successful output was accepted")
	}
	if _, err := NewLeafFailed(CleanupFailure, errors.New("cleanup")); err == nil {
		t.Fatal("runner supplied cleanup failure was accepted")
	}
	if _, err := NewLeafFailed(MechanicalFailure, nil); err == nil {
		t.Fatal("failure without error was accepted")
	}
}

func TestLeafCompletionConstructorsProduceClosedOutcomes(t *testing.T) {
	success, err := NewLeafSucceeded(value.NewString("candidate"))
	if err != nil {
		t.Fatal(err)
	}
	if !success.Valid() || success.Status() != Succeeded {
		t.Fatalf("success = %#v", success)
	}
	if got, ok := success.Output(); !ok || got.Kind() != value.StringKind {
		t.Fatalf("success output = %#v, %v", got, ok)
	}

	failure, err := NewLeafFailed(ContractFailure, errors.New("bad candidate"))
	if err != nil {
		t.Fatal(err)
	}
	if !failure.Valid() || failure.Status() != Failed || failure.FailureKind() != ContractFailure {
		t.Fatalf("failure = %#v", failure)
	}

	timeout, err := NewLeafTimeout(errors.New("deadline"))
	if err != nil {
		t.Fatal(err)
	}
	if timeout.Status() != Failed || timeout.FailureKind() != TimeoutFailure {
		t.Fatalf("timeout = %#v", timeout)
	}

	cancelled, err := NewLeafCancelled(errors.New("backend cancelled"))
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status() != Cancelled || !cancelled.Valid() {
		t.Fatalf("cancelled = %#v", cancelled)
	}
}
