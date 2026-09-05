package dawn

// The runtime half of the surface: everything that needs the journal, the
// admission queue and the Harbor runner to mean anything. The signatures are
// final — protocols compile against them today — and the bodies land with the
// tickets each one names.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Metric reads one number the gate wrote alongside its reward — including
// "reward" itself. dawn parses no CLI output, so this is the only channel by
// which a stage's own arithmetic reaches the control flow. ok is false when
// the gate wrote no such metric; a missing metric is never silently zero.
func (r Result) Metric(name string) (value float64, ok bool) {
	// These are Harbor's own parse of /logs/verifier/reward.json, which
	// outranks reward.txt in its precedence order. dawn does not read the file
	// a second time behind Harbor's back: a string reward raises a
	// ValidationError there, and re-reading would accept a verdict Harbor
	// rejected.
	value, ok = r.metrics[name]
	return value, ok
}

// Actuate performs an external effect. It runs in dawn's own process, where
// the credentials live, and fires only on Passed — on any other state it does
// nothing and reports why. dawn deduplicates its own retries against its own
// durable record, keyed by the attempt_id.
func (r Result) Actuate(fn func(*Actuation) error) error {
	// runtime-core/actuator
	panic("not yet implemented: runtime-core/actuator")
}

// Actuation is what an actuator is handed. It is deliberately narrow: a key
// and a door onto the gate's published bytes, and nothing else.
type Actuation struct {
	// Key is the attempt_id. dawn has already deduplicated against it; carry
	// it into the remote effect (a branch name, a record id) to make the far
	// side idempotent too, which dawn cannot do for you.
	Key string
}

// Published resolves one file the GATE wrote to its publish directory. This is
// the only door: the agent's raw declared output is unreachable from here, and
// that filter is what makes publishing safe. dawn fails the actuation if the
// gate wrote no such name.
func (a *Actuation) Published(name string) string {
	// runtime-core/actuator
	panic("not yet implemented: runtime-core/actuator")
}

// runner dispatches one compiled stage and blocks until it reaches a terminal
// state. It is the whole seam between the scope arithmetic here and the Harbor
// task compiler in harbor.go: the scope decides WHETHER an attempt may run and
// for how long, the runner decides what it MEANS.
//
// The ctx carries the scope's AttemptWallClock. A returned error is dawn
// failing to obtain a verdict at all — the scope turns it into InfraError and
// retries it against its own counter. Every state assignment that needs to
// look at what the task produced (rules 2-5 in the State doc, Exhausted
// included, since only the runner knows whether the declared output was there
// when the clock ran out) belongs to the runner and rides back in the Result.
type runner interface {
	Dispatch(ctx context.Context, stage Stage) (Result, error)
}

// dispatcher is the runner every run uses. harbor.go registers the Harbor
// implementation; there is exactly one, and Main refuses to start without it.
var dispatcher runner

// run is one process-wide run: the record on disk, the runner, and the lock
// that makes the scope counters safe for a fan.
//
// ponytail: one lock for the whole run's arithmetic. Per-scope locks if a fan
// ever gets wide enough for the contention to show up, which it will not.
type run struct {
	dir      string
	dispatch runner
	sleep    func(time.Duration) // time.Sleep; a test replaces it

	mu     sync.Mutex
	values map[string]any
}

// Scope is a bounded region of a run holding one lease. Everything dispatches
// through a scope, or the lease would be decorative. infra_error backoff
// happens inside the scope that owns the attempt, so a flaky child cannot
// spend a sibling's budget.
type Scope struct {
	run      *run
	parent   *Scope
	id       string
	lease    Lease
	deadline time.Time // start + WallClock, never later than the parent's
	used     int       // dispatches charged to this scope, retries included
}

// protocolBug is a bug in the protocol, not a state of the world: dispatching
// on a spent scope, recording a name twice, dispatching from a scope with no
// per-attempt clock. dawn unwinds the run the way a cancel does rather than
// inventing a State for it.
type protocolBug struct{ msg string }

func bug(format string, args ...any) { panic(protocolBug{fmt.Sprintf(format, args...)}) }

// Main is the process entry point: it makes func main legal. dawn owns the
// journal and the restart sweep, so it must be able to re-enter the protocol
// itself — on restart a stage whose result.json exists is never re-run, and
// the recorded result is handed straight back.
//
// The State the protocol returns is the RUN's terminal state: dawn writes it
// into the run record and exits on it. That is the only consumer, and it is
// why every protocol spends care on the difference between a measurement and
// a catastrophe that produced the same artifacts.
func Main(name string, root Lease, protocol func(*Scope) State) {
	if dispatcher == nil {
		panic("dawn: no runner registered")
	}
	dir := filepath.Join(runRoot(), name+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(fmt.Sprintf("dawn: run directory: %v", err))
	}
	r := &run{dir: dir, dispatch: dispatcher, sleep: time.Sleep, values: map[string]any{}}
	state := r.protocol(root, protocol)
	r.mu.Lock()
	r.values["state"] = string(state)
	r.flush()
	r.mu.Unlock()
	fmt.Printf("dawn: %s %s %s\n", name, state, dir)
}

// runRoot is where run directories go. The default is a directory beside the
// protocol binary's working directory; DAWN_RUN_ROOT moves it.
func runRoot() string {
	if d := os.Getenv("DAWN_RUN_ROOT"); d != "" {
		return d
	}
	return "dawn-runs"
}

// protocol runs the protocol against a fresh root scope and unwinds a protocol
// bug into Cancelled — dawn never obtained a verdict, and the record says why.
func (r *run) protocol(root Lease, fn func(*Scope) State) (state State) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		b, ok := p.(protocolBug)
		if !ok {
			panic(p)
		}
		r.mu.Lock()
		r.values["protocol_bug"] = b.msg
		r.mu.Unlock()
		state = Cancelled
	}()
	return fn(r.root(root))
}

func (r *run) root(lease Lease) *Scope {
	return &Scope{run: r, id: "root", lease: lease, deadline: time.Now().Add(lease.WallClock)}
}

// Scope opens a nested scope with its own lease. Nesting is how a protocol
// says that one instance's whole escalation is bounded separately from the
// run.
func (s *Scope) Scope(id string, lease Lease) *Scope {
	// A child draws from its parent: its clock cannot outlast the parent's,
	// and every dispatch it makes is charged to the parent too (see charge),
	// so its Attempts is a ceiling on its own share and never an allowance on
	// top of the parent's. A child leased more clock than its parent has left
	// simply stops admitting when the parent's deadline arrives.
	deadline := time.Now().Add(lease.WallClock)
	if deadline.After(s.deadline) {
		deadline = s.deadline
	}
	return &Scope{run: s.run, parent: s, id: s.id + "/" + id, lease: lease, deadline: deadline}
}

// More reports whether the lease can still admit an attempt. It is the loop
// guard: "try again until the scope runs out" is the one question a protocol
// asks the admission queue rather than being told the answer to.
//
// Dispatching on a spent scope is a protocol bug, and dawn unwinds the run the
// way a cancel does — dawn never turns a spent lease into a state of its own,
// because Exhausted already means one attempt hit its clock and a best-of-N
// loop must be able to tell a single slow attempt from a spent lease. What a
// PROTOCOL calls its own run when More() was false before it dispatched
// anything is the protocol's choice, and cybergym and vdh both call it
// Exhausted.
//
// One bool for one attempt: it does not say whether the clock or the counter
// is the binding half, and a fan of n asks it n times over.
func (s *Scope) More() bool {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	return s.admits(time.Now())
}

// admits is the admission predicate, held under run.mu. The clock half is the
// scope's own deadline, which is never later than any ancestor's, so it needs
// no walk. It does NOT demand that a whole AttemptWallClock still fit: a lease
// from Dispatching is exactly attempts x per plus backoff, so the last attempt
// of an honest lease never fits with room to spare, and demanding it would
// make Dispatching(1, per) a scope that cannot dispatch at all. What bounds
// the attempt instead is the clock Run hands it — the remaining scope clock
// when that is the smaller of the two — and an attempt cut short that way is
// Exhausted, which is the state for "dawn's own clock ended it". The counter
// half walks the ancestry, because siblings spend the same parent counter.
func (s *Scope) admits(now time.Time) bool {
	if !now.Before(s.deadline) {
		return false
	}
	for a := s; a != nil; a = a.parent {
		if a.used >= a.lease.Attempts {
			return false
		}
	}
	return true
}

// charge admits one dispatch and books it against the scope and every ancestor,
// all or nothing.
func (s *Scope) charge() bool {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	if !s.admits(time.Now()) {
		return false
	}
	for a := s; a != nil; a = a.parent {
		a.used++
	}
	return true
}

// Run dispatches one attempt and blocks until it reaches a terminal state.
// Machine overcapacity is queueing, never an error.
func (s *Scope) Run(stage Stage) Result {
	// runtime-core/runner: compiles the stage into a task.toml dawn owns
	// entirely — artifacts list, separate no-network verifier, digest-pinned
	// verifier image, agent-phase allowlist — and dispatches it.
	if s.lease.AttemptWallClock <= 0 {
		bug("scope %s dispatched %q with no AttemptWallClock: a scope that dispatches is built with Dispatching", s.id, stage.ID)
	}
	// An infra_error is re-dispatched here, in the scope that owns the
	// attempt, after sleeping — and spends the same counter a deliberate
	// attempt does. A caller never sees an InfraError that still had budget.
	for retry := 0; ; retry++ {
		if !s.charge() {
			if retry == 0 {
				bug("scope %s dispatched %q on a spent lease: guard the dispatch with More()", s.id, stage.ID)
			}
			return Result{State: InfraError}
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.attemptClock())
		r, err := s.run.dispatch.Dispatch(ctx, stage)
		cancel()
		if err != nil {
			r = Result{State: InfraError}
		}
		if r.State != InfraError || !s.More() {
			return r
		}
		s.run.sleep(retryBackoffGap(retry + 1))
	}
}

// attemptClock is what one attempt gets: dawn's per-attempt clock, or the rest
// of the scope's clock when that is shorter. The scope's WallClock is a bound
// on the scope as a whole, so a late attempt is truncated rather than allowed
// to run past it.
func (s *Scope) attemptClock() time.Duration {
	if rest := time.Until(s.deadline); rest < s.lease.AttemptWallClock {
		return rest
	}
	return s.lease.AttemptWallClock
}

// Fan dispatches n attempts. It DRAINS — never fail-fast — and returns every
// child's terminal state in index order, because a fan-in that silently drops
// children is a corpus that shrank without saying so. By the time a caller
// sees an InfraError child, that branch already exhausted its own retries.
//
// There is no child limit and no concurrency argument: the agent profile fixes
// the parallelism, and n is the author's to size. Fan does NOT clamp n to the
// lease — n, plus whatever retries those children turn out to owe, has to fit
// in the scope's remaining Attempts, or a child dispatches on a spent scope,
// which is the protocol bug More() describes. Declare the lease as the fan's
// width plus retry headroom; nothing here checks that you did.
func (s *Scope) Fan(n int, mk func(i int) Stage) []Result {
	// runtime-core/scopes
	panic("not yet implemented: runtime-core/scopes")
}

// Record writes one named value into dawn's run record. It is the channel for
// what no gate can write: arithmetic dawn does across many attempts, the
// caveats a protocol with no sound oracle is obliged to state, and the external
// effects that failed after a gate had already voted yes.
//
// A name is written ONCE per run. Writing the same name twice is a protocol
// bug: the second write is not a second value, and a reader of the run cannot
// tell one caveat from two. Qualify every name by whatever varies around the
// call — round, phase, instance, branch.
//
// For a protocol whose stages cannot reach Passed there is no actuator at all,
// so the run record is not one product among several: it is the whole of it.
func (s *Scope) Record(name string, value any) {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	if _, dup := s.run.values[name]; dup {
		bug("scope %s recorded %q twice: qualify the name by whatever varies around the call", s.id, name)
	}
	s.run.values[name] = value
	s.run.flush()
}

// flush rewrites the run record. Held under run.mu. A run record dawn cannot
// write is a run with no product, so the failure is loud.
func (r *run) flush() {
	b, err := json.MarshalIndent(r.values, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("dawn: run record: %v", err))
	}
	if err := os.WriteFile(filepath.Join(r.dir, "record.json"), append(b, '\n'), 0o644); err != nil {
		panic(fmt.Sprintf("dawn: run record: %v", err))
	}
}
