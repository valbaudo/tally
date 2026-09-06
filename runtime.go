package dawn

// The runtime half of the surface: everything that needs the journal,
// concurrency admission and the Harbor runner to mean anything. The
// signatures are final — protocols compile against them today — and the
// bodies land with the tickets each one names.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
// nothing and reports why, and it never calls fn at all. An error fn returns
// (or a Published lookup that found nothing) fails the actuation but never
// touches r.State: the verdict already stands, only the effect failed.
//
// fn may run more than once per run — a retry, a resume — so everything it
// builds is scratch except a.Key, Published, and what Step returns; record
// only those, and record them AFTER Step returns, not inside fn. Recording
// inside fn is the trap: a crash between the effect landing and Step's own
// flush re-runs fn on resume, the remote hands back a second identifier for
// the same effect, and the loaded record still holds the first — a bug() on
// resume for a pattern that looked fine every time it was actually tested.
//
// dawn deduplicates its own retries against its own durable record, keyed by
// the attempt_id — see Actuation.Step. That record is PER-RUN, held in the
// run directory, not a second global store: a global dedup store is a second
// persistence layer with its own crash-recovery, growth and GC policy that
// nothing in this design has an opinion on. The honest consequence: dawn
// deduplicates a run's own retries, never two different runs. Running the
// same protocol twice over unchanged inputs WILL perform the effect twice —
// a new run is a new request that it happen.
//
// No Secret type guards the credentials an fn uses. A Secret.Reveal() method
// exported so the actuator can use a credential is callable from anywhere in
// the package, Stage-building code included, so a reveal method and a real
// Prompt-interpolation guard against credential leakage cannot coexist in one
// type. Credentials for an effect are ordinary strings in this trusted
// actuator code; "credentials live in dawn's process" means they are never
// shipped into a container, not that dawn custodies them behind a type.
func (r Result) Actuate(fn func(*Actuation) error) error {
	if r.State != Passed {
		return fmt.Errorf("dawn: actuate: state is %s, not %s: nothing to actuate", r.State, Passed)
	}
	a := &Actuation{Key: r.attemptID, publishDir: r.publishDir, run: r.run}
	err := fn(a)
	if a.err != nil {
		return a.err
	}
	return err
}

// Actuation is what an actuator is handed. It is deliberately narrow: a key,
// a door onto the gate's published bytes, and a way to dedup a sub-step —
// nothing else.
type Actuation struct {
	// Key is the attempt_id. dawn has already deduplicated against it; carry
	// it into the remote effect (a branch name, a record id) to make the far
	// side idempotent too, which dawn cannot do for you.
	Key string
	// publishDir is the gate's own publish directory for this attempt,
	// threaded from Result.publishDir. Unexported: an actuator reaches it
	// only through Published, never as a raw path it could point elsewhere.
	publishDir string
	// run is the run whose per-run actuation record Step consults, threaded
	// from Result.run.
	run *run
	// err records a Published call that found nothing. Actuate checks it
	// after fn returns, so a missing publish fails the actuation even if fn
	// ignored Published's return value instead of erroring out itself.
	err error
}

// Published resolves one file the GATE wrote to its publish directory. This is
// the only door: the agent's raw declared output is unreachable from here, and
// that filter is what makes publishing safe. dawn fails the actuation if the
// gate wrote no such name — recorded here after a stat, and surfaced by
// Actuate once fn returns, so a caller that forgets to check still fails
// rather than pushing a path to nothing.
func (a *Actuation) Published(name string) string {
	p := filepath.Join(a.publishDir, name)
	if _, err := os.Stat(p); err != nil {
		a.err = fmt.Errorf("dawn: gate published no %q", name)
	}
	return p
}

// Step performs one named external sub-effect at most once per attempt. It
// looks up (attempt_id, name) in the run's own actuation record before
// calling fn; on a first success it records fn's returned external
// identifier and returns it; on a repeat — a retried Actuate closure within
// the SAME run — it returns the recorded identifier straight back without
// calling fn again. This hoists the settled lookup-then-act rule once,
// instead of every actuator author hand-rolling their own probe of a remote
// system that answers "did this already happen?" badly or not at all.
//
// ponytail: fn runs under the run's single mutex — the same one Scope's
// arithmetic already shares — so two Steps of the same run never race for
// the same key, at the cost of stalling the rest of the run while one effect
// is in flight. Fine for a local git push; per-key locks if actuation
// concurrency ever matters.
func (a *Actuation) Step(name string, fn func() (string, error)) (string, error) {
	key := a.Key + "/" + name

	// The lock is taken twice and never held across fn. An effect is the one
	// thing in dawn that talks to the outside world and can block for as long
	// as the outside world likes, and Record takes this same lock — so holding
	// it across fn would deadlock the run the first time an author recorded
	// the identifier the step just returned, which is the obvious thing to
	// write. It would also serialise every effect in a fan behind whichever
	// one is slowest.
	a.run.mu.Lock()
	v, done := a.run.actuations[key]
	a.run.mu.Unlock()
	if done {
		return v, nil
	}

	v, err := fn()
	if err != nil {
		return "", err
	}

	a.run.mu.Lock()
	defer a.run.mu.Unlock()
	// Someone else recorded this key while fn ran. Their identifier is the one
	// already published, so it wins; ours is a duplicate effect that at-least-
	// once always allowed. Returning theirs keeps every later reader agreeing.
	if prior, ok := a.run.actuations[key]; ok {
		return prior, nil
	}
	a.run.actuations[key] = v
	a.run.flushActuations()
	return v, nil
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
	// evidence is the directory the runner must write everything it produced
	// into: the generated task, harbor's log, and the trial itself. It is
	// durable and deterministic, because recovery reads it — "result.json
	// present, trust it, never re-run" needs a path that survives the process.
	Dispatch(ctx context.Context, stage Stage, evidence string) (Result, error)
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
	ctx      context.Context // parent of every attempt's context; cancelled -> Cancelled
	dispatch runner
	sleep    func(time.Duration) // time.Sleep; a test replaces it

	mu         sync.Mutex
	values     map[string]any
	actuations map[string]string // "attempt_id/step name" -> the external identifier Step recorded
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

// Main is the process entry point: it makes func main legal. dawn owns
// resumption and the restart sweep, so it must be able to re-enter the
// protocol itself — on restart, a stage whose result.json already exists on
// disk is never re-run; dispatchAttempt hands the reconstructed Result
// straight back (see resumeResult).
//
// There is no separate journal: os.MkdirAll(evidence, …), which
// dispatchAttempt already calls before every Dispatch, IS "journal before
// dispatch" — a directory, not a file. Everything a resume needs is already
// on disk or rebuilds for free by re-entering this same deterministic
// protocol function from the top and replaying the same calls in the same
// order.
//
// The State the protocol returns is the RUN's terminal state: dawn writes it
// into the run record and exits on it. That is the only consumer, and it is
// why every protocol spends care on the difference between a measurement and
// a catastrophe that produced the same artifacts.
func Main(name string, root Lease, protocol func(*Scope) State) {
	if dispatcher == nil {
		panic("dawn: no runner registered")
	}
	dir, err := resolveRunDir(name)
	if err != nil {
		panic(fmt.Sprintf("dawn: %v", err))
	}
	// The reap is synchronous and unconditional, on every invocation — see
	// reap.go. It runs after the run directory is settled (so a bad
	// DAWN_RESUME fails loud without needing Docker at all) and before
	// anything is dispatched.
	if err := reap(); err != nil {
		panic(fmt.Sprintf("dawn: reap: %v", err))
	}
	// Captured once, here, and threaded down as the parent of every attempt's
	// context (Scope.Run) — never context.Background(). Every in-flight and
	// future attempt's ctx.Done fires the instant either signal arrives,
	// which is what lets clockOutcome (harbor.go) tell dawn's own clock
	// apart from an operator asking the whole run to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := newRun(dir, ctx, dispatcher)
	state := r.protocol(root, protocol)
	r.mu.Lock()
	r.values["state"] = string(state)
	r.flush()
	r.mu.Unlock()
	writeReport(dir)
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

// resolveRunDir picks the run directory for this Main invocation.
// DAWN_RESUME=<dir>, same family as DAWN_RUN_ROOT/DAWN_MAX_CONCURRENT since
// dawn owns no flag parser (func main belongs to the protocol author),
// reuses a literal path so a crashed run can be re-entered; otherwise a
// fresh "name-<timestamp>" is minted under runRoot(). A DAWN_RESUME naming a
// directory that does not exist is refused rather than silently minting a
// new run under that name — a typo must fail loud, not quietly start over.
func resolveRunDir(name string) (string, error) {
	if d := os.Getenv("DAWN_RESUME"); d != "" {
		info, err := os.Stat(d)
		if err != nil {
			return "", fmt.Errorf("DAWN_RESUME=%q: %w", d, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("DAWN_RESUME=%q: not a directory", d)
		}
		return d, nil
	}
	dir := filepath.Join(runRoot(), name+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("run directory: %w", err)
	}
	return dir, nil
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
	return s.dispatchAttempt(stage, true)
}

// stageID is Stage.ID's grammar: one Harbor task-name segment, verbatim from
// Harbor's constants.py. It is also exactly one path segment — no slash, and
// the leading alnum rules out "." and ".." — so an id IS its evidence
// directory and its task name, translated nowhere: attempts/<id>/ is
// injective in id by construction and "dawn/"+id is a name Harbor accepts.
// Three bugs came from leaving the id unconstrained and sanitising it per
// consumer (a double slash in the name, a leading '.', two ids on one
// evidence directory); one grammar at dispatch replaces all three.
var stageID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// dispatchAttempt is Run's body — compiles the stage into a task.toml dawn
// owns entirely (artifacts list, separate no-network verifier, digest-pinned
// verifier image, agent-phase allowlist) and dispatches it — parameterised on
// whether a spent scope on the very FIRST charge is a protocol bug.
//
// bugOnFirstCharge is true for every caller except Fan's own children. A
// plain Run against an already-spent scope is a caller that skipped More(),
// which is the protocol bug More()'s doc names. Several fan children racing
// the SAME scope's remaining Attempts down to zero is a different thing: the
// scope was sized correctly, More() was true a moment ago, and the loser of
// that race gets an ordinary InfraError — like any other spent-lease
// dispatch — instead of tearing down every sibling with it (see Fan).
func (s *Scope) dispatchAttempt(stage Stage, bugOnFirstCharge bool) Result {
	if !stageID.MatchString(stage.ID) {
		bug("scope %s dispatched stage id %q: an id is one segment, %s — qualify with '-' or '.', not '/'", s.id, stage.ID, stageID)
	}
	if s.lease.AttemptWallClock <= 0 {
		bug("scope %s dispatched %q with no AttemptWallClock: a scope that dispatches is built with Dispatching", s.id, stage.ID)
	}
	// An infra_error is re-dispatched here, in the scope that owns the
	// attempt, after sleeping — and spends the same counter a deliberate
	// attempt does. A caller never sees an InfraError that still had budget.
	for retry := 0; ; retry++ {
		evidence := filepath.Join(s.run.dir, "attempts", stage.ID, strconv.Itoa(retry+1))

		// Resume: this exact retry already ran, in an earlier process, if
		// result.json is sitting under evidence/jobs from a prior Dispatch.
		// trial.read + classify (harbor.go) is the SAME reconstruction a live
		// Dispatch performs, so a resumed attempt reads as the identical
		// Result — without spending a charge(), because this attempt's charge
		// was already booked, in the run that actually dispatched it, and
		// there is no journal recording that booking to replay. A protocol
		// that loops past a resumed short-circuit rather than returning
		// immediately would therefore see this scope's own Attempts counter
		// under-report its history; every shipped protocol dispatches once
		// per Run call and stops, so this never shows.
		r, resumed := resumeResult(evidence, stage, retry+1)
		if !resumed {
			// Concurrency admission: charge() (already atomic, already walks
			// ancestors) plus this one semaphore, acquired before
			// charge/Dispatch and released right after — see agentGates.
			// Held only across the charge+dispatch below, never across the
			// backoff sleep, so a flaky child doesn't sit on a slot while it
			// waits to retry.
			release := acquireAgentGate(stage.Agent, stage.Env)
			if !s.charge() {
				release()
				if retry == 0 && bugOnFirstCharge {
					bug("scope %s dispatched %q on a spent lease: guard the dispatch with More()", s.id, stage.ID)
				}
				return Result{State: InfraError}
			}
			if err := os.MkdirAll(evidence, 0o755); err != nil {
				bug("cannot create the evidence directory %s: %v", evidence, err)
			}
			// s.run.ctx (Main's signal.NotifyContext), never
			// context.Background(): a cancel from outside the run has to
			// reach every attempt, including ones dispatched after the
			// signal arrived.
			ctx, cancel := context.WithTimeout(s.run.ctx, s.attemptClock())
			var err error
			r, err = s.run.dispatch.Dispatch(ctx, stage, evidence)
			cancel()
			release()
			if err != nil {
				r = Result{State: InfraError}
			}
		}
		// The receipt, on both paths, with the same bytes — the resumed
		// reconstruction is classify() over the same trial, so there is
		// nothing to branch on. This is also the only place that sees every
		// DISPATCH rather than every Result: an infra_error that is retried
		// away never reaches the protocol, and it is exactly the attempt that
		// burned a draw and produced nothing.
		writeReceipt(evidence, stage, retry+1, r)
		if r.State != InfraError || !s.More() {
			// Set here, on the terminal Result, and nowhere else: this is
			// the one place both the stage and the settled attempt number
			// (retry+1, the same number evidence/ is already keyed on) are
			// both in hand.
			r.attemptID = attemptID(stage, retry+1)
			r.run = s.run
			return r
		}
		if resumed {
			// Nothing was actually dispatched just now, so there is nothing
			// to back off from — move straight to checking whether the NEXT
			// retry was also already run.
			continue
		}
		// The one gap: this sleep is plain time.Sleep, not selecting on
		// s.run.ctx.Done(). A cancel arriving mid-backoff is not noticed here
		// — only at the next dispatch's context, whose Done fires immediately
		// since the parent is already cancelled. Bounded, not unbounded: the
		// longest this can delay noticing is retryBackoffCap (5 minutes).
		s.run.sleep(retryBackoffGap(retry + 1))
	}
}

// resumeResult reconstructs a completed attempt from a PRIOR process's
// evidence directory instead of dispatching it again — the whole of "resume"
// for one attempt. os.MkdirAll(evidence, …), which the live path below still
// calls before every Dispatch, already puts a result.json where the SAME
// deterministic replay looks for one; trial.read finds it and classify
// (harbor.go) turns it into the identical Result a live Dispatch would have
// produced. ok is false the instant trial.read's glob does not find exactly
// one result.json under evidence/jobs — nothing to resume, dispatch for real.
//
// The receipt is the commit record — writeReceipt is the last thing
// dispatchAttempt does for an attempt, and its id is attemptID(s, n): stage
// id, content, inputs, attempt number. A receipt with THIS attempt's id
// proves the trial beside it is this attempt's, not merely one that landed on
// the same path. Any other receipt means the stage changed since it ran, or
// two stages share one id; adopting that trial would answer a Run with a
// verdict for work that never happened, and actuate on it. Refused, never
// re-dispatched: re-dispatching would overwrite the evidence that shows why.
// No receipt at all is today's rule unchanged — nothing finished here, or
// dawn died in the instant between Harbor's result.json and its own receipt,
// the one window this cannot see.
func resumeResult(evidence string, s Stage, attempt int) (Result, bool) {
	var rec receipt
	switch err := readJSON(filepath.Join(evidence, "receipt.json"), &rec); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		bug("receipt at %s unreadable: %v — refusing to resume over it", evidence, err)
	case rec.ID != attemptID(s, attempt):
		bug("evidence %s belongs to attempt %s (stage %q), not to stage %q's attempt %s: the stage changed since it ran, or two stages share one id",
			evidence, rec.ID, rec.Stage, s.ID, attemptID(s, attempt))
	// Model and effort are deliberately NOT folded into contentDigest
	// (dawn.go's own comment on it says why), so the rec.ID check above
	// cannot see a profile that changed between the run that left this
	// evidence and the one resuming over it: the hash is identical either
	// way. This case is the sixth field attemptID's LOUD COMMENT prescribes
	// appending alongside the hash rather than into it — it is what makes
	// the model and effort part of resume identity without touching
	// attemptID at all.
	case rec.Model != s.Agent.model || rec.Effort != s.Agent.effort:
		bug("evidence %s ran on model %q effort %q, this stage pins %q %q: the profile changed since it ran",
			evidence, rec.Model, rec.Effort, s.Agent.model, s.Agent.effort)
	}
	var t trial
	if err := t.read(filepath.Join(evidence, "jobs"), s.Outputs); err != nil {
		return Result{}, false
	}
	return classify(s, t), true
}

// fanWidth is the Q2 concurrency formula: clamp(1, NumCPU, 80% of host
// memory / one attempt's image size). Plain NumCPU has no idea an image is
// heavy: 12-wide against pr-ci's 1.94 GB image asks ~23 GB of a 9.4 GB VM.
// Verifier containers run concurrently with agent containers, and dawn's own
// process plus the Docker daemon share that same pool, so usable is 80% of
// the total, not 100% — turning the clamp into a target is how a concurrency
// knob becomes an OOM knob. An attempt OOM-killed mid-trial writes no
// result.json, so dawn classifies it infra_error and burns the lease
// retrying into the same wall — a config mistake wearing a flakiness
// costume.
//
// A docker inspect failure, or an unreadable host memory figure, fails
// CLOSED to 1 — never to NumCPU(). "Overcapacity is queueing, never an
// error" has to mean the failure mode is safe, not fast: serial-by-default
// is the only answer that cannot OOM.
//
// DAWN_MAX_CONCURRENT wins outright when set, mirroring DAWN_RUN_ROOT.
func fanWidth(env Image) int {
	return width(env, hostMemoryBytes, imageSizeBytes, runtime.NumCPU())
}

// width is fanWidth's arithmetic, parameterised on how to read host memory,
// one image's size, and the CPU count, so the formula is fully deterministic
// in a test — without Docker, a real host, or a dependency on how many cores
// happen to run the test.
func width(env Image, hostMem func() (int64, bool), imageSize func(Image) (int64, bool), cpu int) int {
	if v, ok := os.LookupEnv("DAWN_MAX_CONCURRENT"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	mem, ok := hostMem()
	if !ok {
		return 1
	}
	size, ok := imageSize(env)
	if !ok || size <= 0 {
		return 1
	}
	n := int(mem * 80 / 100 / size)
	if n < 1 {
		n = 1
	}
	if n > cpu {
		n = cpu
	}
	return n
}

// hostMemoryBytes reads total physical memory on the host dawn's own process
// runs on — the same pool the Docker daemon and every container draw from.
// ok is false on any unsupported OS or read failure, which is exactly the
// "unreadable host memory" case fanWidth fails closed on.
func hostMemoryBytes() (int64, bool) {
	// The memory that bounds concurrency is the DOCKER DAEMON's, not this
	// machine's. Containers run inside a VM on macOS, and the two numbers
	// differ by more than the safety factor: measured here, sysctl reports
	// 19.3 GB while the VM the trials actually run in has 9.4 GB. Sizing
	// against the host would allow 7 concurrent copies of a 1.94 GB image —
	// 13.6 GB into a 9.4 GB VM — which is the OOM this whole formula exists
	// to prevent, arrived at by a longer route.
	//
	// docker is already a dependency this file shells to for image size, so
	// asking it for the pool its own containers draw from adds no new
	// surface, and it is right on Linux too, where the daemon is bounded by
	// whatever cgroup it runs under rather than by /proc/meminfo.
	out, err := exec.Command("docker", "info", "--format", "{{.MemTotal}}").Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n, err == nil && n > 0
}

// imageSizeBytes shells out to docker — already a dependency Harbor drives —
// to size one pinned image. ok is false on any inspect failure: an image
// dawn cannot size is an image dawn cannot safely fan.
func imageSizeBytes(env Image) (int64, bool) {
	out, err := exec.Command("docker", "image", "inspect", string(env), "--format", "{{.Size}}").Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n, err == nil && n > 0
}

// agentGates is one semaphore per agent NAME, built lazily on first use and
// never rebuilt — the structural form of Codex's cap of 1. Capacity is 1
// when the profile cannot fan (Agent.FanOut == false), otherwise fanWidth's
// memory-aware limit, sized against whichever stage first dispatches that
// agent name: a fan is homogeneous, so that is mk(0)'s Env for a Fan call
// (Fan sizes it before spawning a single child) and simply the one Env
// there is for a plain Run.
//
// Every dispatch acquires it, in dispatchAttempt above, fanned or not — which
// is what makes the cap hold structurally rather than by convention:
// Fan(24, codexStage) still spawns 24 goroutines, but they serialise here
// instead of overlapping, and that is safe because Dispatching's WallClock
// already funds the fully-serial worst case.
//
// FanOut is a bool and can only express "1" or "the pool" — a future agent
// with, say, a cap of 3 would need the field widened past a bool.
var (
	agentGatesMu sync.Mutex
	agentGates   = map[string]chan struct{}{}
)

// acquireAgentGate blocks until a slot for a's name is free and returns the
// release func. See agentGates for why this is keyed by name alone and built
// only once.
func acquireAgentGate(a Agent, env Image) func() {
	agentGatesMu.Lock()
	g, ok := agentGates[a.name]
	if !ok {
		n := 1
		if a.FanOut {
			n = fanWidth(env)
		}
		g = make(chan struct{}, n)
		agentGates[a.name] = g
	}
	agentGatesMu.Unlock()
	g <- struct{}{}
	return func() { <-g }
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

// Fan dispatches n attempts concurrently. It DRAINS — never fail-fast — and
// returns every child's terminal state in INDEX order regardless of dispatch
// or completion order, because a fan-in that silently drops children is a
// corpus that shrank without saying so; index order is also why wait-order
// never matters. By the time a caller sees an InfraError child, that branch
// already exhausted its own retries.
//
// Concurrency is admission, not a second data structure: dispatchAttempt's
// existing charge() plus one per-agent-name semaphore (agentGates), sized
// once against mk(0)'s Env — a fan is homogeneous, so every child shares one
// image and dawn need not ask docker n times over. There is no ordering and
// no fairness beyond index order; the "admission queue" once sketched in
// CONTEXT.md never existed.
//
// A child that cannot be charged returns InfraError like any other
// spent-lease dispatch rather than unwinding the whole run: several children
// racing this SAME scope's remaining Attempts down to zero is not the
// protocol bug More()'s doc warns about, it is what a fan sized exactly to
// its lease looks like — that sizing is the author's job, not a clamp Fan
// applies (below).
//
// There is no child limit and no concurrency argument: the agent profile
// fixes the parallelism, and n is the author's to size. Fan does NOT clamp n
// to the lease — n, plus whatever retries those children turn out to owe,
// has to fit in the scope's remaining Attempts. Declare the lease as the
// fan's width plus retry headroom; nothing here checks that you did.
//
// Every child charges THIS SAME scope — s.id is identical across every one
// of them, so it cannot disambiguate a Record. A Record made from inside
// mk(i), or by the caller processing result i, MUST qualify its name by the
// child's own Stage.ID. Record's existing duplicate-name bug() is the
// correctness net, not something to route around: two children writing the
// same unqualified name is the exact prototype bug — two values silently
// overwriting one key — turned into a loud crash instead. The cost is author
// discipline with no soft failure: one unqualified Record in a wide fan
// crashes the whole run.
func (s *Scope) Fan(n int, mk func(i int) Stage) []Result {
	if n < 0 {
		bug("scope %s Fan called with negative n=%d", s.id, n)
	}
	if n > 0 {
		// Build (and size) this agent's gate against the fan's own image
		// before any child races to do it implicitly.
		first := mk(0)
		acquireAgentGate(first.Agent, first.Env)()
	}
	results := make([]Result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = s.dispatchAttempt(mk(i), false)
		}(i)
	}
	wg.Wait()
	return results
}

// Record writes one named value into dawn's run record. It is the channel for
// what no gate can write: arithmetic dawn does across many attempts, the
// caveats a protocol with no sound oracle is obliged to state, and the external
// effects that failed after a gate had already voted yes.
//
// A name is written once per run — recording it again with an identical
// value is a no-op, not a second write, because that is exactly what a
// resumed pass looks like when it recomputes a fact it already recorded
// before crashing: newRun loaded the same record, and the pass is not
// wrong to reach the same answer twice. "Identical" compares values after a
// JSON round-trip, since a Go struct and the loaded map[string]any it
// decodes into on the next resume are never DeepEqual as raw values even
// when they carry the same data.
//
// Recording a name again with a DIFFERENT value is a protocol bug, whether
// that is the same pass writing it twice or a resume recording something
// the pass it resumed did not: a reader of the run cannot tell which of two
// answers is true. Qualify every name by whatever varies around the call —
// round, phase, instance, branch.
//
// For a protocol whose stages cannot reach Passed there is no actuator at all,
// so the run record is not one product among several: it is the whole of it.
func (s *Scope) Record(name string, value any) {
	norm := normalizeJSON(value)
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	if prior, dup := s.run.values[name]; dup {
		if reflect.DeepEqual(prior, norm) {
			return
		}
		bug("scope %s recorded %q twice with different values: qualify the name by whatever varies around the call, or a resume recorded something the pass it resumed did not", s.id, name)
	}
	s.run.values[name] = norm
	s.run.flush()
}

// normalizeJSON round-trips value through JSON so two representations of the
// same data — a freshly-built struct and the map[string]any that decoding
// record.json produced from an earlier flush of that same struct — compare
// and store identically. Without this, Record's identical-value check never
// fires for anything but primitives: a Manifest built this pass and the
// []any{map[string]any{...}} form loaded from a previous pass's record.json
// are never reflect.DeepEqual as raw values, so every resumed run would bug
// on its first re-recorded struct.
func normalizeJSON(value any) any {
	b, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("dawn: run record: %v", err))
	}
	var norm any
	if err := json.Unmarshal(b, &norm); err != nil {
		panic(fmt.Sprintf("dawn: run record: %v", err))
	}
	return norm
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

// flushActuations rewrites the run's own actuation dedup record — a sibling
// file to record.json, not a field inside it, because Record's one-write-
// per-name rule and Step's whole point (the SAME key answered twice on
// purpose) are different invariants and would be confusing sharing one map.
// Held under run.mu, same as flush: a dedup record dawn cannot write is a
// dedup record nothing can trust on the next retry.
// loadActuations reads back the dedup record a previous invocation of this
// same run directory wrote.
//
// Without it, resume re-runs every actuator: Main always started with an empty
// map, so a stage that already pushed a branch pushed it again. That directly
// contradicts the actuator's own rule — dawn deduplicates a run's OWN retries,
// and a crash-and-resume is exactly the retry that rule was written for. The
// per-run scope of the record is deliberate and unchanged; this only makes the
// record survive the process, which is what "durable" was always supposed to
// mean.
//
// A missing file is the normal case for a fresh run, not an error. A corrupt
// one is: silently continuing with an empty map would re-fire effects that
// already happened, which is the single thing this record exists to prevent.
// newRun is the only way a run is built, so the dedup record is read back as
// part of construction rather than as a call Main has to remember. The first
// version of this was a separate r.loadActuations() line in Main, and a test
// that called loadActuations itself passed happily when that line was deleted
// — the check could not fail. Construction is the honest place for it: a run
// without its record is not a half-built run, it is a run that will re-fire
// effects that already happened.
func newRun(dir string, ctx context.Context, d runner) *run {
	r := &run{dir: dir, ctx: ctx, dispatch: d, sleep: time.Sleep,
		values: map[string]any{}, actuations: map[string]string{}}
	r.loadActuations()
	r.loadRecord()
	return r
}

func (r *run) loadActuations() {
	b, err := os.ReadFile(filepath.Join(r.dir, "actuations.json"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		panic(fmt.Sprintf("dawn: actuation record unreadable at %s: %v", r.dir, err))
	}
	if err := json.Unmarshal(b, &r.actuations); err != nil {
		panic(fmt.Sprintf("dawn: actuation record corrupt at %s: %v — refusing to resume, because an empty record re-fires effects that already happened", r.dir, err))
	}
}

// loadRecord reads back record.json a previous invocation of this same run
// directory wrote — same reason as loadActuations, same shape of bug without
// it. newRun used to start r.values empty on every invocation, so a resumed
// run's first flush rewrote record.json from only what THAT invocation had
// recorded so far, discarding everything an earlier invocation wrote before
// it crashed or was resumed. Measured: pr-ci's actuator re-provisioned a
// remote on resume (a bug of its own, fixed alongside this one) and recorded
// its fresh path; with r.values loaded here, that becomes a bug() — Record
// sees a name already holding a DIFFERENT value — instead of a silent lie.
// Without the load, it was a silent lie: the resumed record.json and
// report.md named a remote with 0 branches and 0 tags, because dawn had no
// memory of the one Step had actually pushed to.
//
// state and protocol_bug are deleted immediately after loading: they are
// THIS invocation's verdict to write, never the previous invocation's to
// repeat. Left in, a resumed run that crashes before reaching Main's own
// write would flush "state": "passed" from the attempt before on every
// intermediate Record, and a protocol bug fixed and then resumed would carry
// a stale protocol_bug forever.
//
// A missing file is the normal case for a fresh run, not an error. A corrupt
// one panics, mirroring loadActuations and for the same reason: rewriting
// record.json from an empty map is exactly the truncation this fixes.
func (r *run) loadRecord() {
	switch err := readJSON(filepath.Join(r.dir, "record.json"), &r.values); {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
		panic(fmt.Sprintf("dawn: run record unreadable at %s: %v", r.dir, err))
	}
	delete(r.values, "state")
	delete(r.values, "protocol_bug")
}

func (r *run) flushActuations() {
	b, err := json.MarshalIndent(r.actuations, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("dawn: actuation record: %v", err))
	}
	if err := os.WriteFile(filepath.Join(r.dir, "actuations.json"), append(b, '\n'), 0o644); err != nil {
		panic(fmt.Sprintf("dawn: actuation record: %v", err))
	}
}
