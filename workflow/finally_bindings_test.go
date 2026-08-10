package workflow

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/value"
)

// This catches cleanup compilation losing source identity, byte-exact path
// segments, target identity, or assignability's runtime-validation decision.
func TestFinallyBindingCompilesAllSources(t *testing.T) {
	draft := finallyBindingProgram(t)
	definition, err := Compile(draft)
	if err != nil {
		t.Fatal(err)
	}

	cleanup, ok := definition.Root().Finally()
	if !ok {
		t.Fatal("compiled graph has no finally scope")
	}
	bindings := cleanup.Bindings()
	if len(bindings) != 3 {
		t.Fatalf("cleanup bindings = %d, want 3", len(bindings))
	}

	assertCleanupBinding(t, bindings[0], CleanupInput, "", []string{"request"}, "request", false)
	assertCleanupBinding(t, bindings[1], CleanupOutcome, "", nil, "outcome", false)
	assertCleanupBinding(t, bindings[2], CleanupChild, "prepare\xff", []string{"handle\xfe"}, "handle", true)
	if got := cleanup.Graph().Inputs(); !got.Equal(finallyCleanupInputs(t)) {
		t.Fatal("cleanup graph input contract changed during compilation")
	}
	if !bytes.Contains(definition.Canonical(), []byte(`"format":"dawn.workflow/2"`)) {
		t.Fatalf("canonical format = %s, want dawn.workflow/2", definition.Canonical())
	}
}

// This catches the immutable cleanup model retaining draft paths or exposing
// its binding/path slices for mutation after compilation.
func TestFinallyBindingDefensivelyCopiesState(t *testing.T) {
	draft := finallyBindingProgram(t)
	definition, err := Compile(draft)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := definition.Canonical()

	draftBinding := &draft.Modules[0].Graph.Finally.Bindings[2]
	draftBinding.From.Path[0] = "changed"
	draftBinding.From.Child = "changed"
	draftBinding.To = "changed"

	cleanup, _ := definition.Root().Finally()
	bindings := cleanup.Bindings()
	bindings[0] = CleanupBinding{}
	bindings[2].from.path[0] = "changed"
	path := cleanup.Bindings()[2].From().Path()
	path[0] = "changed"

	bindings = cleanup.Bindings()
	assertCleanupBinding(t, bindings[2], CleanupChild, "prepare\xff", []string{"handle\xfe"}, "handle", true)
	if got := definition.Canonical(); !bytes.Equal(got, wantCanonical) {
		t.Fatal("canonical bytes changed through mutable draft or accessor state")
	}
}

// This catches canonical identity depending on cleanup binding author order,
// or omitting the source path and target from executable identity.
func TestFinallyBindingCanonicalIdentity(t *testing.T) {
	forward := finallyBindingProgram(t)
	reversed := finallyBindingProgram(t)
	reverseCleanupBindings(reversed.Modules[0].Graph.Finally.Bindings)
	left := mustCompileFinallyBindings(t, forward).Canonical()
	right := mustCompileFinallyBindings(t, reversed).Canonical()
	if !bytes.Equal(left, right) {
		t.Fatalf("canonical bytes depend on cleanup binding order:\n%s\n%s", left, right)
	}

	changedSource := finallyBindingProgram(t)
	changedSource.Modules[0].Graph.Finally.Bindings[2].From.Path = []string{"spare\xfd"}
	if got := mustCompileFinallyBindings(t, changedSource).Canonical(); bytes.Equal(left, got) {
		t.Fatal("canonical bytes did not change with cleanup source")
	}

	changedTarget := finallyBindingProgram(t)
	changedTarget.Modules[0].Graph.Finally.Bindings[2].To = "spareTarget"
	if got := mustCompileFinallyBindings(t, changedTarget).Canonical(); bytes.Equal(left, got) {
		t.Fatal("canonical bytes did not change with cleanup target")
	}
}

// This catches malformed cleanup sources becoming ambiguous runtime lookups.
func TestCompileFinallyRejectsMalformedSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		from CleanupSourceDraft
		want string
	}{
		{"unknown kind", CleanupSourceDraft{Kind: CleanupSourceKind(99), Path: []string{"request"}}, "unknown cleanup source kind"},
		{"outcome path", CleanupSourceDraft{Kind: CleanupOutcome, Path: []string{"status"}}, "outcome source must not have a path"},
		{"input child", CleanupSourceDraft{Kind: CleanupInput, Child: "prepare", Path: []string{"request"}}, "input source must not name a child"},
		{"outcome child", CleanupSourceDraft{Kind: CleanupOutcome, Child: "prepare"}, "outcome source must not name a child"},
		{"empty child", CleanupSourceDraft{Kind: CleanupChild, Path: []string{"handle\xfe"}}, "child source has an empty child name"},
		{"unknown child", CleanupSourceDraft{Kind: CleanupChild, Child: "missing", Path: []string{"handle\xfe"}}, "unknown child"},
		{"missing input path", CleanupSourceDraft{Kind: CleanupInput}, "input source path is empty"},
		{"missing child path", CleanupSourceDraft{Kind: CleanupChild, Child: "prepare\xff"}, "child source path is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := finallyBindingProgram(t)
			draft.Modules[0].Graph.Finally.Bindings[2].From = tc.from
			_, err := Compile(draft)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

// This catches cleanup bindings bypassing target resolution, exact outcome
// typing, assignability, uniqueness, or required-input coverage.
func TestCompileFinallyRejectsInvalidTargetsAndTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *ProgramDraft)
		want string
	}{
		{
			name: "unknown target",
			edit: func(_ *testing.T, draft *ProgramDraft) {
				draft.Modules[0].Graph.Finally.Bindings[2].To = "missing"
			},
			want: "unknown target port",
		},
		{
			name: "incompatible types",
			edit: func(t *testing.T, draft *ProgramDraft) {
				draft.Modules[0].Graph.Finally.Graph.Inputs = finallyContract(t,
					finallyRequired(t, "request", value.Integer()),
					finallyRequired(t, "outcome", finallyOutcomeType(t)),
					finallyOptional(t, "handle", value.String()),
					finallyOptional(t, "spareTarget", value.String()),
				)
			},
			want: "cannot assign",
		},
		{
			name: "duplicate target",
			edit: func(_ *testing.T, draft *ProgramDraft) {
				draft.Modules[0].Graph.Finally.Bindings[2].To = "request"
			},
			want: "bound more than once",
		},
		{
			name: "unbound required input",
			edit: func(_ *testing.T, draft *ProgramDraft) {
				draft.Modules[0].Graph.Finally.Bindings = draft.Modules[0].Graph.Finally.Bindings[1:]
			},
			want: "required input is unbound",
		},
		{
			name: "outcome target must use exact enum",
			edit: func(t *testing.T, draft *ProgramDraft) {
				draft.Modules[0].Graph.Finally.Graph.Inputs = finallyContract(t,
					finallyRequired(t, "request", value.String()),
					finallyRequired(t, "outcome", value.String()),
					finallyOptional(t, "handle", value.String()),
					finallyOptional(t, "spareTarget", value.String()),
				)
			},
			want: "exact outcome enum",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := finallyBindingProgram(t)
			tc.edit(t, &draft)
			_, err := Compile(draft)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

// This catches conditional protected values being treated as guaranteed
// cleanup values merely because their static types are assignable.
func TestCompileFinallyRejectsConditionalSourceForRequiredTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		from CleanupSourceDraft
	}{
		{
			name: "optional protected input",
			from: CleanupSourceDraft{Kind: CleanupInput, Path: []string{"optionalRequest"}},
		},
		{
			name: "body child output",
			from: CleanupSourceDraft{Kind: CleanupChild, Child: "prepare\xff", Path: []string{"requiredText"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := finallyBindingProgram(t)
			draft.Modules[0].Graph.Finally.Bindings[0].From = tc.from
			_, err := Compile(draft)
			if err == nil || !strings.Contains(err.Error(), "cannot satisfy required target") {
				t.Fatalf("Compile() error = %v, want conditional-source rejection", err)
			}
		})
	}
}

func finallyBindingProgram(t *testing.T) ProgramDraft {
	t.Helper()
	protectedInputs := finallyContract(t,
		finallyOptional(t, "optionalRequest", value.String()),
		finallyRequired(t, "request", value.String()),
	)
	childOutputs := finallyContract(t,
		finallyRequired(t, "handle\xfe", value.Any()),
		finallyRequired(t, "requiredText", value.String()),
		finallyRequired(t, "spare\xfd", value.Any()),
	)
	return bindingProgram(GraphDraft{
		Inputs: protectedInputs,
		Nodes: []NodeDraft{{
			Name: "prepare\xff",
			Leaf: &LeafDraft{Kind: Script, Inputs: value.EmptyContract(), Outputs: childOutputs},
		}},
		Finally: &FinallyDraft{
			Graph: GraphDraft{Inputs: finallyCleanupInputs(t), Outputs: value.EmptyContract()},
			Bindings: []CleanupBindingDraft{
				{From: CleanupSourceDraft{Kind: CleanupInput, Path: []string{"request"}}, To: "request"},
				{From: CleanupSourceDraft{Kind: CleanupOutcome}, To: "outcome"},
				{From: CleanupSourceDraft{Kind: CleanupChild, Child: "prepare\xff", Path: []string{"handle\xfe"}}, To: "handle"},
			},
		},
	})
}

func finallyCleanupInputs(t *testing.T) value.Contract {
	t.Helper()
	return finallyContract(t,
		finallyRequired(t, "request", value.String()),
		finallyRequired(t, "outcome", finallyOutcomeType(t)),
		finallyOptional(t, "handle", value.String()),
		finallyOptional(t, "spareTarget", value.String()),
	)
}

func finallyOutcomeType(t *testing.T) value.Type {
	t.Helper()
	typ, err := value.Enum(
		finallyLiteral(t, `"succeeded"`),
		finallyLiteral(t, `"rejected"`),
		finallyLiteral(t, `"failed"`),
		finallyLiteral(t, `"cancelled"`),
	)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func finallyLiteral(t *testing.T, source string) value.Literal {
	t.Helper()
	literal, err := value.ParseLiteral([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return literal
}

func finallyRequired(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func finallyOptional(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Optional(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func finallyContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func assertCleanupBinding(t *testing.T, binding CleanupBinding, kind CleanupSourceKind, child string, path []string, target string, runtime bool) {
	t.Helper()
	source := binding.From()
	if source.Kind() != kind || source.Child() != child || !equalFinallyStrings(source.Path(), path) {
		t.Fatalf("cleanup source = (%d, %q, %#v), want (%d, %q, %#v)", source.Kind(), source.Child(), source.Path(), kind, child, path)
	}
	if binding.To() != target || binding.RuntimeValidation() != runtime {
		t.Fatalf("cleanup target = (%q, %v), want (%q, %v)", binding.To(), binding.RuntimeValidation(), target, runtime)
	}
}

func equalFinallyStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func reverseCleanupBindings(bindings []CleanupBindingDraft) {
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}
}

func mustCompileFinallyBindings(t *testing.T, draft ProgramDraft) Definition {
	t.Helper()
	definition, err := Compile(draft)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
