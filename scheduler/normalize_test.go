package scheduler

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
)

func TestNormalizeIgnoresCompletionOrder(t *testing.T) {
	causes := []diagnostic{
		failed(testPath("z"), TimeoutFailure, errors.New("late timeout")),
		rejected(testPath("a"), "policy"),
		failed(testPath("b"), MechanicalFailure, errors.New("mechanical")),
		parentCancelled(testPath("c")),
	}
	want := diagnosticPaths(normalize(causes, nil))
	for seed := int64(0); seed < 100; seed++ {
		got := normalize(shuffled(seed, causes), nil)
		primary, ok := got.Primary()
		if !ok || lastAuthoredName(primary.Path()) != "b" {
			t.Fatalf("seed %d primary = %#v", seed, primary)
		}
		if !reflect.DeepEqual(diagnosticPaths(got), want) {
			t.Fatalf("seed %d diagnostics = %#v, want %#v", seed, diagnosticPaths(got), want)
		}
	}
}

func TestNormalizeExternalCancellationOutranksEveryIntrinsicCause(t *testing.T) {
	got := normalize([]diagnostic{
		failed(testPath("a"), MechanicalFailure, errors.New("failure")),
		rejected(testPath("b"), "no"),
	}, errors.New("caller cancelled"))
	primary, ok := got.Primary()
	if !ok || got.Status() != Cancelled || primary.Error() == nil || primary.Error().Error() != "caller cancelled" {
		t.Fatalf("external cancellation did not win: %#v", got)
	}
}

func TestNormalizeRanksIntrinsicCauses(t *testing.T) {
	cases := []struct {
		name   string
		causes []diagnostic
		want   Status
		path   string
	}{
		{"failure over rejection", []diagnostic{rejected(testPath("a"), "no"), failed(testPath("b"), ContractFailure, errors.New("bad"))}, Failed, "b"},
		{"rejection over cancellation", []diagnostic{intrinsicCancelled(testPath("b")), rejected(testPath("a"), "no")}, Rejected, "a"},
		{"intrinsic cancellation", []diagnostic{intrinsicCancelled(testPath("a"))}, Cancelled, "a"},
		{"cleanup does not replace body rejection", []diagnostic{rejected(testPath("a"), "no"), cleanupFailed(testPath("b"), errors.New("cleanup"))}, Rejected, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize(tc.causes, nil)
			primary, ok := got.Primary()
			if !ok || got.Status() != tc.want || lastAuthoredName(primary.Path()) != tc.path {
				t.Fatalf("normalize = %#v", got)
			}
		})
	}
}

func TestNormalizeUsesLexicalPathForEqualPrecedence(t *testing.T) {
	got := normalize([]diagnostic{
		failed(testPath("z"), TimeoutFailure, errors.New("z")),
		failed(testPath("a"), MechanicalFailure, errors.New("a")),
		failed(testPath("m"), ContractFailure, errors.New("m")),
	}, nil)
	primary, ok := got.Primary()
	if !ok || lastAuthoredName(primary.Path()) != "a" {
		t.Fatalf("primary = %#v", primary)
	}
	if lastAuthoredName(got.Secondary()[0].Path()) != "m" || lastAuthoredName(got.Secondary()[1].Path()) != "z" {
		t.Fatalf("secondary diagnostics = %#v", got.Secondary())
	}
}

func TestNormalizeUsesParentCancellationOnlyWhenItIsTheOnlyCause(t *testing.T) {
	got := normalize([]diagnostic{
		parentCancelled(testPath("a")),
		failed(testPath("b"), MechanicalFailure, errors.New("failure")),
	}, nil)
	primary, ok := got.Primary()
	if !ok || lastAuthoredName(primary.Path()) != "b" {
		t.Fatalf("parent cancellation masked intrinsic cause: %#v", got)
	}

	onlyParent := normalize([]diagnostic{parentCancelled(testPath("a"))}, nil)
	if onlyParent.Status() != Cancelled {
		t.Fatalf("only parent cancellation = %#v", onlyParent)
	}
}

func testPath(name string) Path { return Path{}.AuthoredChild(name) }

func lastAuthoredName(path Path) string {
	components := path.Components()
	if len(components) == 0 {
		return ""
	}
	name, _ := components[len(components)-1].AuthoredChild()
	return name
}

func shuffled(seed int64, causes []diagnostic) []diagnostic {
	got := append([]diagnostic(nil), causes...)
	rand.New(rand.NewSource(seed)).Shuffle(len(got), func(i, j int) { got[i], got[j] = got[j], got[i] })
	return got
}

func diagnosticPaths(result Result) []string {
	all := result.Secondary()
	if primary, ok := result.Primary(); ok {
		all = append([]Diagnostic{primary}, all...)
	}
	paths := make([]string, len(all))
	for i, diagnostic := range all {
		paths[i] = lastAuthoredName(diagnostic.Path())
	}
	return paths
}
