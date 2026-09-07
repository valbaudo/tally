package dawn

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// --- how wide a fanning agent may go (runtime.go's maxConcurrent) ---

// The memory-aware formula this replaces is gone; see maxConcurrent's own
// comment for why. What is left has exactly two answers, and both are pinned
// here because "unset means 1" is a real behavioural claim, not a default
// nobody exercises.
func TestMaxConcurrent(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"", "1"}, {"7", "7"}, {"1", "1"},
		// A value dawn cannot use is not a licence to guess a better one.
		{"0", "1"}, {"-3", "1"}, {"lots", "1"},
	} {
		name := tc.set
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			if tc.set == "" {
				t.Setenv("DAWN_MAX_CONCURRENT", "")
				os.Unsetenv("DAWN_MAX_CONCURRENT")
			} else {
				t.Setenv("DAWN_MAX_CONCURRENT", tc.set)
			}
			want, _ := strconv.Atoi(tc.want)
			if got := maxConcurrent(); got != want {
				t.Errorf("maxConcurrent() = %d, want %d", got, want)
			}
		})
	}
}

// --- Fan and concurrency admission ---

// constFake returns a fixed state for every call. calls is incremented
// atomically because Fan dispatches its children from multiple goroutines.
type constFake struct {
	state State
	calls int32
}

func (f *constFake) Dispatch(ctx context.Context, stage Stage, evidence string) (Result, error) {
	atomic.AddInt32(&f.calls, 1)
	return Result{State: f.state}, nil
}

// concurrencyFake tracks the highest number of Dispatch calls ever in flight
// at once, so a test can prove two never overlap without relying on
// wall-clock luck to observe a collision.
type concurrencyFake struct {
	sleep    time.Duration
	inFlight int32
	maxSeen  int32
	calls    int32
}

func (f *concurrencyFake) Dispatch(ctx context.Context, stage Stage, evidence string) (Result, error) {
	n := atomic.AddInt32(&f.inFlight, 1)
	defer atomic.AddInt32(&f.inFlight, -1)
	for {
		old := atomic.LoadInt32(&f.maxSeen)
		if n <= old || atomic.CompareAndSwapInt32(&f.maxSeen, old, n) {
			break
		}
	}
	atomic.AddInt32(&f.calls, 1)
	time.Sleep(f.sleep)
	return Result{State: Unverified}, nil
}

func testFanRun(t *testing.T, d runner) *run {
	t.Helper()
	return &run{dir: t.TempDir(), ctx: context.Background(), dispatch: d, sleep: time.Sleep, values: map[string]any{}, actuations: map[string]string{}}
}

// Decision 3, proven with a counter rather than a mock CLI: Codex's cap of 1
// is structural. Fan spawns 4 goroutines that would all overlap if the
// per-agent gate did not serialise them.
func TestFanSerialisesCodex(t *testing.T) {
	f := &concurrencyFake{sleep: 20 * time.Millisecond}
	root := testFanRun(t, f).root(Dispatching(4, time.Minute))

	mk := func(i int) Stage {
		return Stage{ID: fmt.Sprintf("codex-child-%d", i), Agent: Codex, Env: "e@sha256:0", Gate: NoGate("test")}
	}
	results := root.Fan(4, mk)

	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4", len(results))
	}
	for i, r := range results {
		if r.State != Unverified {
			t.Errorf("child %d: state = %s, want unverified", i, r.State)
		}
	}
	if got := atomic.LoadInt32(&f.calls); got != 4 {
		t.Fatalf("dispatcher calls = %d, want 4", got)
	}
	if got := atomic.LoadInt32(&f.maxSeen); got > 1 {
		t.Fatalf("max concurrent Codex dispatches = %d, want at most 1: fanOut==false must be structural", got)
	}
}

// reorderingFake finishes children in the OPPOSITE of dispatch order (child 0
// sleeps longest, the last child sleeps least) and returns a state that
// encodes the child's own index, so a test can tell whether Fan wrote each
// result to the right slot rather than in whatever order the goroutines
// happened to finish.
type reorderingFake struct{ n int }

func (f reorderingFake) Dispatch(ctx context.Context, stage Stage, evidence string) (Result, error) {
	var i int
	fmt.Sscanf(stage.ID, "order-%d", &i)
	time.Sleep(time.Duration(f.n-i) * 5 * time.Millisecond)
	state := Passed
	if i%2 == 1 {
		state = Rejected
	}
	return Result{State: state}, nil
}

// Fan DRAINS and returns every child in INDEX order regardless of dispatch or
// completion order (decision: "Fan returns results in index order regardless
// of dispatch order, so wait-order never matters"). DAWN_MAX_CONCURRENT
// forces real overlap on a synthetic fanning agent without touching Docker.
func TestFanReturnsResultsInIndexOrderRegardlessOfCompletionOrder(t *testing.T) {
	t.Setenv("DAWN_MAX_CONCURRENT", "5")
	const n = 10
	f := reorderingFake{n: n}
	root := testFanRun(t, f).root(Dispatching(n, time.Minute))

	mk := func(i int) Stage {
		return Stage{ID: fmt.Sprintf("order-%d", i), Agent: Agent{name: "fan-order-test", fanOut: true}, Env: "e@sha256:0", Gate: NoGate("test")}
	}
	results := root.Fan(n, mk)

	if len(results) != n {
		t.Fatalf("len(results) = %d, want %d", len(results), n)
	}
	for i, r := range results {
		want := Passed
		if i%2 == 1 {
			want = Rejected
		}
		if r.State != want {
			t.Errorf("results[%d].State = %s, want %s: Fan must place each child at its own index", i, r.State, want)
		}
	}
}

// Decision (Fan semantics): a child that cannot be charged returns
// InfraError like any other spent-lease dispatch instead of unwinding the
// run — several children legitimately race the scope's remaining Attempts
// down to zero, and losing that race is not the protocol bug Scope.Run
// guards against for a plain, un-fanned Run.
func TestFanChildThatCannotChargeReturnsInfraErrorWithoutAbortingSiblings(t *testing.T) {
	t.Setenv("DAWN_MAX_CONCURRENT", "8")
	f := &constFake{state: Unverified}
	root := testFanRun(t, f).root(Lease{Attempts: 2, WallClock: time.Hour, AttemptWallClock: time.Minute})

	mk := func(i int) Stage {
		return Stage{ID: fmt.Sprintf("spent-%d", i), Agent: Agent{name: "fan-spent-test", fanOut: true}, Env: "e@sha256:0", Gate: NoGate("test")}
	}
	results := root.Fan(5, mk)

	if len(results) != 5 {
		t.Fatalf("len(results) = %d, want 5", len(results))
	}
	var dispatched, infra int
	for i, r := range results {
		switch r.State {
		case Unverified:
			dispatched++
		case InfraError:
			infra++
		default:
			t.Errorf("child %d: unexpected state %s", i, r.State)
		}
	}
	if dispatched != 2 {
		t.Errorf("dispatched = %d, want 2: the scope's whole Attempts budget", dispatched)
	}
	if infra != 3 {
		t.Errorf("infra_error = %d, want 3: a lost charge race must not abort its siblings", infra)
	}
	if got := atomic.LoadInt32(&f.calls); got != 2 {
		t.Errorf("dispatcher calls = %d, want 2", got)
	}
	if root.used != 2 {
		t.Errorf("root.used = %d, want 2: a failed charge must not book against the scope", root.used)
	}
}
