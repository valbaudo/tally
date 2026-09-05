package dawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fake stands in for the Harbor runner: it hands back states from a script,
// repeating the last one, and counts the dispatches the scope let through.
type fake struct {
	script []State
	err    error
	calls  int
}

func (f *fake) Dispatch(ctx context.Context, stage Stage, evidence string) (Result, error) {
	f.calls++
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
		dir:      t.TempDir(),
		dispatch: f,
		sleep:    func(d time.Duration) { slept = append(slept, d) },
		values:   map[string]any{},
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
