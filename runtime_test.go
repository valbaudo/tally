package dawn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake stands in for the Harbor runner: it hands back states from a script,
// repeating the last one, and counts the dispatches the scope let through.
type fake struct {
	script []State
	err    error
	calls  int
	// dispatchCtxErr is ctx.Err() snapshotted SYNCHRONOUSLY inside Dispatch,
	// before Scope.Run's own deferred cancel() ever fires — checking the ctx
	// object itself after Run returns would always read Canceled, since
	// Run's cancel() mutates that same object regardless of its parent.
	dispatchCtxErr error
}

func (f *fake) Dispatch(ctx context.Context, stage Stage, evidence string) (Result, error) {
	f.calls++
	f.dispatchCtxErr = ctx.Err()
	if f.err != nil {
		return Result{}, f.err
	}
	i := f.calls - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	return Result{State: f.script[i]}, nil
}

// testRun builds a run whose sleeps are recorded instead of slept.
func testRun(t *testing.T, f *fake) (*run, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	r := &run{
		dir:        t.TempDir(),
		ctx:        context.Background(),
		dispatch:   f,
		sleep:      func(d time.Duration) { slept = append(slept, d) },
		values:     map[string]any{},
		actuations: map[string]string{},
	}
	return r, &slept
}

// caughtBug runs fn and reports the protocol bug it unwound with, if any.
func caughtBug(t *testing.T, fn func()) string {
	t.Helper()
	var msg string
	func() {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			b, ok := p.(protocolBug)
			if !ok {
				panic(p)
			}
			msg = b.msg
		}()
		fn()
	}()
	return msg
}

var okStage = Stage{ID: "s", Agent: ClaudeCode, Env: "e@sha256:0", Gate: NoGate("test")}

// The counter is the bound on dispatch, and it does not care whether an
// attempt was deliberate or a retry paying for someone else's flake.
func TestScopeAdmitsExactlyItsAttempts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  []State
		lease   Lease
		wantRun int  // dispatches the scope should let through
		wantMor bool // More() once the run loop is done
	}{
		{"unverified stops at one", []State{Unverified}, Dispatching(3, time.Minute), 1, true},
		{"rejected loop spends the counter", []State{Rejected}, Dispatching(3, time.Minute), 3, false},
		{"single attempt lease", []State{Rejected}, Dispatching(1, time.Minute), 1, false},
		{"infra retries spend the same counter", []State{InfraError}, Dispatching(4, time.Minute), 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{script: tc.script}
			r, _ := testRun(t, f)
			s := r.root(tc.lease)
			for s.More() {
				if s.Run(okStage).State != Rejected {
					break
				}
			}
			if f.calls != tc.wantRun {
				t.Errorf("dispatches = %d, want %d", f.calls, tc.wantRun)
			}
			if s.More() != tc.wantMor {
				t.Errorf("More() = %v, want %v", !tc.wantMor, tc.wantMor)
			}
		})
	}
}

// Dispatching past the counter is a protocol bug, not a state.
func TestDispatchOnSpentScopeIsAProtocolBug(t *testing.T) {
	f := &fake{script: []State{Rejected}}
	r, _ := testRun(t, f)
	s := r.root(Dispatching(1, time.Minute))
	s.Run(okStage)
	if msg := caughtBug(t, func() { s.Run(okStage) }); msg == "" {
		t.Fatal("dispatch on a spent lease did not unwind")
	}
	if f.calls != 1 {
		t.Errorf("dispatches = %d, want 1: the spent dispatch reached the runner", f.calls)
	}
	// A scope built to dispatch nothing has no per-attempt clock to hand the
	// runner, so dispatching from it is the same class of bug.
	root := r.root(Lease{Attempts: 5, WallClock: time.Hour})
	if msg := caughtBug(t, func() { root.Run(okStage) }); msg == "" {
		t.Fatal("dispatch with no AttemptWallClock did not unwind")
	}
}

// The retry schedule the clock was funded for is the schedule that is slept.
func TestRetrySleepsTheFundedSchedule(t *testing.T) {
	f := &fake{script: []State{InfraError}}
	r, slept := testRun(t, f)
	s := r.root(Dispatching(4, time.Minute))
	if got := s.Run(okStage); got.State != InfraError {
		t.Fatalf("state = %q, want infra_error once the retries are spent", got.State)
	}
	want := []time.Duration{retryBackoffGap(1), retryBackoffGap(2), retryBackoffGap(3)}
	if len(*slept) != len(want) {
		t.Fatalf("slept %v, want %v", *slept, want)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Fatalf("slept %v, want %v", *slept, want)
		}
	}
	// Nothing is slept after the last attempt: the scope was spent, not flaky.
	if f.calls != 4 {
		t.Errorf("dispatches = %d, want 4", f.calls)
	}
}

// A runner that could not obtain a verdict at all is an infra_error like any
// other, and retries against the same counter.
func TestRunnerErrorIsInfraError(t *testing.T) {
	f := &fake{err: errors.New("harbor exploded")}
	r, _ := testRun(t, f)
	s := r.root(Dispatching(2, time.Minute))
	if got := s.Run(okStage); got.State != InfraError {
		t.Fatalf("state = %q, want infra_error", got.State)
	}
	if f.calls != 2 {
		t.Errorf("dispatches = %d, want 2", f.calls)
	}
}

// Children draw FROM the parent: two siblings leased more than the parent
// holds still share the parent's counter, and the second one starves.
func TestChildrenDrawFromTheParent(t *testing.T) {
	f := &fake{script: []State{Rejected}}
	r, _ := testRun(t, f)
	root := r.root(Lease{Attempts: 3, WallClock: time.Hour})

	a := root.Scope("a", Dispatching(10, time.Minute))
	b := root.Scope("b", Dispatching(10, time.Minute))
	spend := func(s *Scope) int {
		n := 0
		for s.More() {
			s.Run(okStage)
			n++
		}
		return n
	}
	if got := spend(a); got != 3 {
		t.Errorf("first child ran %d, want 3: it may not outspend the parent", got)
	}
	if got := spend(b); got != 0 {
		t.Errorf("second child ran %d, want 0: the parent was already spent", got)
	}
	if root.used != 3 {
		t.Errorf("root charged %d, want 3: children's dispatches book against it", root.used)
	}
}

// A child leased more clock than its parent has left does not get it: its
// deadline is clamped at creation, so it stops admitting when the parent's
// clock is out even though its own counter is untouched.
func TestChildCannotOutliveTheParentsClock(t *testing.T) {
	f := &fake{script: []State{Rejected}}
	r, _ := testRun(t, f)
	root := r.root(Lease{Attempts: 100, WallClock: 0})
	child := root.Scope("child", Dispatching(10, time.Hour))
	if child.deadline.After(root.deadline) {
		t.Fatal("child deadline outlives the parent's")
	}
	if child.More() {
		t.Fatal("More() true: the child claimed an hour the parent does not have")
	}
	if msg := caughtBug(t, func() { child.Run(okStage) }); msg == "" {
		t.Fatal("dispatching past the parent's clock did not unwind")
	}
	if f.calls != 0 {
		t.Errorf("dispatches = %d, want 0", f.calls)
	}
}

// More() is the counter and the deadline, and each half flips it alone.
func TestMoreFlipsOnEitherHalfOfTheLease(t *testing.T) {
	f := &fake{script: []State{Rejected}}
	r, _ := testRun(t, f)
	for _, tc := range []struct {
		name  string
		lease Lease
		want  bool
	}{
		{"both halves left", Lease{Attempts: 1, WallClock: time.Hour, AttemptWallClock: time.Minute}, true},
		{"clock out", Lease{Attempts: 1, WallClock: 0, AttemptWallClock: time.Minute}, false},
		{"counter spent", Lease{Attempts: 0, WallClock: time.Hour, AttemptWallClock: time.Minute}, false},
		// A derived lease admits its FIRST attempt: its whole clock is that
		// attempt, so an admission rule wanting one spare would never dispatch.
		{"the tightest honest lease", Dispatching(1, time.Hour), true},
	} {
		if got := r.root(tc.lease).More(); got != tc.want {
			t.Errorf("%s: More() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An attempt gets the smaller of dawn's per-attempt clock and what is left of
// the scope's, so the scope's WallClock really does bound the scope.
func TestAttemptClockIsTruncatedByTheScope(t *testing.T) {
	f := &fake{script: []State{Unverified}}
	r, _ := testRun(t, f)
	if got := r.root(Lease{Attempts: 1, WallClock: time.Hour, AttemptWallClock: time.Minute}).attemptClock(); got != time.Minute {
		t.Errorf("attemptClock = %v, want the per-attempt clock", got)
	}
	s := r.root(Lease{Attempts: 1, WallClock: time.Minute, AttemptWallClock: time.Hour})
	if got := s.attemptClock(); got > time.Minute || got < 50*time.Second {
		t.Errorf("attemptClock = %v, want about the minute the scope has left", got)
	}
}

// Decision 4: Scope.Run must thread the run's OWN context as the parent of
// every attempt's context — never context.Background() — or an external
// cancel (Main's signal.NotifyContext) would never reach a dispatched
// attempt at all.
func TestScopeRunDerivesFromTheRunsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run's parent is already cancelled before dispatch
	f := &fake{script: []State{Unverified}}
	r, _ := testRun(t, f)
	r.ctx = ctx
	r.root(Dispatching(1, time.Minute)).Run(okStage)
	if f.dispatchCtxErr != context.Canceled {
		t.Fatalf("attempt ctx.Err() at dispatch = %v, want context.Canceled: Scope.Run is not deriving from run.ctx", f.dispatchCtxErr)
	}
}

// The run record is one named value per name, written as JSON, and a second
// write of the same name is a protocol bug rather than a second value.
func TestRecordWritesEachNameOnce(t *testing.T) {
	f := &fake{script: []State{Unverified}}
	r, _ := testRun(t, f)
	s := r.root(Lease{Attempts: 1, WallClock: time.Hour})
	s.Record("caveat", "no sound oracle for round 1")
	s.Record("round_2/score", 0.75)

	b, err := os.ReadFile(filepath.Join(r.dir, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["caveat"] != "no sound oracle for round 1" || got["round_2/score"] != 0.75 {
		t.Fatalf("record.json = %v", got)
	}
	if msg := caughtBug(t, func() { s.Record("caveat", "something else") }); msg == "" {
		t.Fatal("recording a name twice did not unwind")
	}
}

// A protocol bug unwinds the run the way a cancel does, and says so in the
// record rather than inventing a state.
func TestProtocolBugUnwindsToCancelled(t *testing.T) {
	f := &fake{script: []State{Rejected}}
	r, _ := testRun(t, f)
	state := r.protocol(Dispatching(1, time.Minute), func(s *Scope) State {
		s.Run(okStage)
		return s.Run(okStage).State // one dispatch too many
	})
	if state != Cancelled {
		t.Fatalf("state = %q, want cancelled", state)
	}
	if r.values["protocol_bug"] == nil {
		t.Error("the record does not say what the bug was")
	}
}

// Scope.Run is the only writer of attempt identity onto a Result, and it has
// to happen on the terminal Result — the one a protocol actually gets back —
// or Actuate would have no key and no run to dedup against.
func TestScopeRunThreadsAttemptIdentityIntoResult(t *testing.T) {
	f := &fake{script: []State{Passed}}
	r, _ := testRun(t, f)
	got := r.root(Dispatching(1, time.Minute)).Run(okStage)
	if got.attemptID == "" {
		t.Error("Scope.Run did not set attemptID on the terminal Result")
	}
	if got.run != r {
		t.Error("Scope.Run did not thread the run onto the Result")
	}
}

// Decision 5: Actuate calls the closure only on Passed, and says why not
// otherwise — never silently.
func TestActuateFiresOnlyOnPassed(t *testing.T) {
	for _, st := range []State{Rejected, Unverified, Exhausted, InfraError, Cancelled, State("")} {
		called := false
		err := Result{State: st}.Actuate(func(a *Actuation) error { called = true; return nil })
		if called {
			t.Errorf("state %s: Actuate called the closure", st)
		}
		if err == nil {
			t.Errorf("state %s: Actuate returned nil, want a reason it did nothing", st)
		}
	}
}

// Actuate on Passed hands the closure the attempt_id as Key and a working
// door onto the gate's own bytes — never the agent's.
func TestActuatePassedThreadsKeyAndPublishedBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fix.patch"), []byte("gate bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Result{State: Passed, attemptID: "attempt-abc", publishDir: dir, run: &run{dir: t.TempDir(), actuations: map[string]string{}}}
	var gotKey, gotPath string
	if err := r.Actuate(func(a *Actuation) error {
		gotKey = a.Key
		gotPath = a.Published("fix.patch")
		return nil
	}); err != nil {
		t.Fatalf("Actuate on Passed: %v", err)
	}
	if gotKey != "attempt-abc" {
		t.Errorf("Key = %q, want the attempt id", gotKey)
	}
	b, err := os.ReadFile(gotPath)
	if err != nil || string(b) != "gate bytes" {
		t.Errorf("Published(%q) = %q (%v), want the gate's own bytes", gotPath, b, err)
	}
}

// A Published name the gate never wrote fails the whole actuation, even when
// the actuator's own fn ignores the (still-returned) path and reports no
// error itself.
func TestPublishedMissingNameFailsTheActuation(t *testing.T) {
	r := Result{State: Passed, attemptID: "x", publishDir: t.TempDir(), run: &run{dir: t.TempDir(), actuations: map[string]string{}}}
	err := r.Actuate(func(a *Actuation) error {
		a.Published("never-written.txt")
		return nil
	})
	if err == nil {
		t.Fatal("Actuate returned nil: a missing publish must fail the actuation")
	}
}

// Decision 4, proven with a counter rather than a mock remote: a retried
// Step for the same (attempt_id, name) returns the recorded identifier
// without calling fn again.
func TestStepDedupsWithinARun(t *testing.T) {
	r, _ := testRun(t, &fake{script: []State{Unverified}})
	a := &Actuation{Key: "attempt-1", run: r}
	calls := 0
	step := func() (string, error) { calls++; return "external-id", nil }

	v1, err := a.Step("push", step)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := a.Step("push", step)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want 1: the second Step should have short-circuited", calls)
	}
	if v1 != v2 || v1 != "external-id" {
		t.Fatalf("Step returned %q then %q, want the same recorded id both times", v1, v2)
	}
	// A different name under the same attempt is a different key.
	if _, err := a.Step("tag", step); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("fn called %d times, want 2: a different step name must not dedup against \"push\"", calls)
	}
}

// TestRecordInsideStepDoesNotDeadlock pins the one thing an author will
// obviously write: recording the identifier the step just produced, from
// inside the step. Step originally held the run's mutex across fn, and Record
// takes that same mutex, so this deadlocked the whole run.
//
// It is a timeout rather than an assertion because a deadlock has no value to
// compare — the failure mode is that this test never returns.
func TestRecordInsideStepDoesNotDeadlock(t *testing.T) {
	r := &run{dir: t.TempDir(), values: map[string]any{}, actuations: map[string]string{}}
	scope := &Scope{run: r, id: "s"}
	res := Result{State: Passed, attemptID: "attempt-1", run: r, publishDir: t.TempDir()}

	done := make(chan error, 1)
	go func() {
		done <- res.Actuate(func(a *Actuation) error {
			_, err := a.Step("push", func() (string, error) {
				scope.Record("pr", 42) // the deadlock, when Step held the lock
				return "branch-1", nil
			})
			return err
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("actuate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlocked: Step held the run lock across fn while Record wanted it")
	}
}

// --- Decision 3: resume, via dispatchAttempt's before-charge() check ---

// writeCompletedTrial drops a minimal but genuine result.json (plus a
// manifest.json that collected outputDir empty-but-present) under evidence,
// in exactly the shape a prior process's real Dispatch call would have left
// it — evidence/jobs/<job>/<trial>/result.json — so trial.read's glob finds
// it. An ungated stage with no declared outputs then classifies Unverified.
func writeCompletedTrial(t *testing.T, evidence string) {
	t.Helper()
	trialDir := filepath.Join(evidence, "jobs", "job1", "trial1")
	if err := os.MkdirAll(filepath.Join(trialDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trialDir, "result.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`[{"source":%q,"destination":".","status":"ok"}]`, outputDir)
	if err := os.WriteFile(filepath.Join(trialDir, "artifacts", "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeCompletedInfraErrorTrial drops a result.json with NO manifest.json —
// trial.read still finds the one result, but Present stays false, so
// classify reads it as infra_error (Rule 2), the same as a live attempt
// whose declared output was never collected.
func writeCompletedInfraErrorTrial(t *testing.T, evidence string) {
	t.Helper()
	trialDir := filepath.Join(evidence, "jobs", "job1", "trial1")
	if err := os.MkdirAll(trialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trialDir, "result.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// This is the real work of Decision 3: a completed attempt's evidence
// directory already has a result.json — left by dispatchAttempt's own
// os.MkdirAll-then-Dispatch in a PRIOR process — and resuming into the same
// stage must reconstruct that Result via trial.read + classify without
// calling the dispatcher again and without spending the scope's lease a
// second time (it was already spent when the attempt first ran).
func TestDispatchAttemptResumesFromExistingResultWithoutRedispatchingOrSpendingTheLease(t *testing.T) {
	f := &fake{script: []State{Passed}} // wrong on purpose: must never be reached
	r, _ := testRun(t, f)
	s := r.root(Dispatching(1, time.Minute))

	evidence := filepath.Join(r.dir, "attempts", fsSafe(okStage.ID), "1")
	writeCompletedTrial(t, evidence)

	got := s.Run(okStage)
	if f.calls != 0 {
		t.Fatalf("dispatcher called %d times, want 0: a completed attempt must not be re-dispatched", f.calls)
	}
	if got.State != Unverified {
		t.Fatalf("state = %s, want unverified (reconstructed from the fixture on disk)", got.State)
	}
	if s.used != 0 {
		t.Fatalf("scope.used = %d, want 0: a resumed attempt must not spend the lease a second time", s.used)
	}
	if !s.More() {
		t.Fatal("More() = false: the lease should still show its full budget after a resumed attempt")
	}
	if got.attemptID != attemptID(okStage, 1) {
		t.Errorf("attemptID = %q, want the same id a live dispatch would have produced", got.attemptID)
	}
	if got.run != r {
		t.Error("a resumed Result must still thread the run, or Actuate has nothing to dedup against")
	}
}

// A resumed run whose first retry was already infra_error on disk, and whose
// second retry never got that far (nothing under its evidence directory),
// replays the first retry for free and then dispatches the second for real —
// proving the short-circuit does not swallow retries that never happened,
// and does not sleep a backoff for a retry that was not actually just
// dispatched.
func TestDispatchAttemptResumesInfraErrorRetryThenDispatchesTheNextRetryForReal(t *testing.T) {
	f := &fake{script: []State{Rejected}}
	r, slept := testRun(t, f)
	s := r.root(Dispatching(2, time.Minute))

	evidence1 := filepath.Join(r.dir, "attempts", fsSafe(okStage.ID), "1")
	writeCompletedInfraErrorTrial(t, evidence1)

	got := s.Run(okStage)
	if f.calls != 1 {
		t.Fatalf("dispatcher called %d times, want 1: only retry 2 (never on disk) should really dispatch", f.calls)
	}
	if got.State != Rejected {
		t.Fatalf("state = %s, want rejected (the live retry 2)", got.State)
	}
	if got.attemptID != attemptID(okStage, 2) {
		t.Errorf("attemptID = %q, want retry 2's id", got.attemptID)
	}
	if s.used != 1 {
		t.Fatalf("scope.used = %d, want 1: only the live retry may spend the lease", s.used)
	}
	if len(*slept) != 0 {
		t.Fatalf("slept %v, want none: nothing was actually dispatched for the resumed retry, so there is nothing to back off from", *slept)
	}
}

// --- Decision 2: DAWN_RESUME ---

// DAWN_RESUME reuses a literal path rather than minting a fresh
// "name-<timestamp>", mirroring DAWN_RUN_ROOT/DAWN_MAX_CONCURRENT since dawn
// owns no flag parser.
func TestResolveRunDirHonoursDawnResume(t *testing.T) {
	existing := t.TempDir()
	t.Setenv("DAWN_RESUME", existing)
	got, err := resolveRunDir("whatever")
	if err != nil {
		t.Fatalf("resolveRunDir: %v", err)
	}
	if got != existing {
		t.Fatalf("resolveRunDir() = %q, want the literal DAWN_RESUME path %q", got, existing)
	}
}

// A typo'd DAWN_RESUME must fail loud, before anything else runs, rather
// than silently minting a brand-new run under a name nobody asked for.
func TestResolveRunDirDawnResumeToMissingDirFailsLoud(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("DAWN_RESUME", missing)
	if _, err := resolveRunDir("whatever"); err == nil {
		t.Fatal("resolveRunDir did not fail for a DAWN_RESUME directory that does not exist")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("resolveRunDir must not create the directory it just refused to resume")
	}
}

// Without DAWN_RESUME, a fresh run is minted (and actually created on disk)
// under runRoot(), name-qualified and timestamped.
func TestResolveRunDirMintsAFreshRunByDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DAWN_RUN_ROOT", root)
	dir, err := resolveRunDir("myproto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(root, "myproto-")) {
		t.Fatalf("resolveRunDir() = %q, want it under %q named after the protocol", dir, root)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("resolveRunDir() must create the fresh run directory: %v", err)
	}
}

// TestResumeDoesNotRefireActuators pins the actuator's own rule across the one
// retry it was written for: a crash and resume. Main used to start with an
// empty actuation map, so a resumed stage pushed its branch a second time.
func TestResumeDoesNotRefireActuators(t *testing.T) {
	dir := t.TempDir()

	// First invocation: the effect happens once and is recorded.
	first := newRun(dir, context.Background(), nil)
	calls := 0
	res := Result{State: Passed, attemptID: "attempt-1", run: first, publishDir: t.TempDir()}
	if err := res.Actuate(func(a *Actuation) error {
		_, err := a.Step("push", func() (string, error) { calls++; return "branch-1", nil })
		return err
	}); err != nil {
		t.Fatalf("first actuate: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first run made %d calls, want 1", calls)
	}

	// Second invocation over the SAME run directory, as DAWN_RESUME does.
	second := newRun(dir, context.Background(), nil)
	res2 := Result{State: Passed, attemptID: "attempt-1", run: second, publishDir: t.TempDir()}
	got, err := "", error(nil)
	if err = res2.Actuate(func(a *Actuation) error {
		var e error
		got, e = a.Step("push", func() (string, error) { calls++; return "branch-2", nil })
		return e
	}); err != nil {
		t.Fatalf("resumed actuate: %v", err)
	}
	if calls != 1 {
		t.Errorf("resume re-fired the actuator: %d calls, want 1 — the effect already happened", calls)
	}
	if got != "branch-1" {
		t.Errorf("resume returned %q, want the recorded identifier %q", got, "branch-1")
	}
}
