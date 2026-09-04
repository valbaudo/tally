// Package api is dawn's protocol surface: the whole vocabulary an author has.
//
// A protocol is a Go program. It declares stages, bounds them with leases,
// reads back a closed set of states, and — only on passed — actuates. Nothing
// else is expressible, on purpose: every construct here was forced by at least
// one of the four independently drafted protocols (cybergym, mdash, vdh,
// pr-ci), or by a settled decision that TRACE.md names instead. See TRACE.md
// for the derivation, including what was cut.
//
// Everything the settled decisions already fix is absent from this surface:
// the verifier always runs separate, no-network, after the agent's container
// is gone, and always writes reward.json; the artifacts list, the output
// paths, the retry backoff, the per-agent concurrency cap and the idempotency
// key are dawn's, not the author's. An author cannot restate them and so
// cannot contradict them — which is why a Stage has no output field at all.
// Naming the Env image is naming the task, and the task's declared artifacts
// are already dawn's list.
//
// Prototype: signatures are load-bearing, bodies are not.
package api

import "time"

// State is a stage's outcome. Six values, and dawn assigns no other; the
// numeric reward is never one of them — it rides alongside as a metric
// (Result.Metric). The zero State is not one of the six, which is why a
// protocol seeds a loop variable with a state rather than with a zero Result.
//
// dawn assigns a state by first match, parsing no agent output at all:
//
//  1. external cancel                                    -> Cancelled
//  2. declared output missing or invalid                 -> InfraError (always)
//  3. output present, gate wrote no valid reward         -> InfraError
//  4. output present, gate ran, dawn's own timeout hit   -> Exhausted
//  5. output present, gate ran, clean exit               -> Passed / Rejected,
//     or Unverified for a format-only gate or no gate at all
type State string

const (
	// Passed: a sound gate ran and said yes. The only state that actuates.
	Passed State = "passed"
	// Rejected: a sound gate ran and said no. Terminal for the attempt; only
	// the protocol decides whether that is worth another one.
	Rejected State = "rejected"
	// Unverified: nothing established a verdict — a format-only gate, or no
	// gate. It is the SUCCESS state of those stages, not a failure.
	Unverified State = "unverified"
	// Exhausted: dawn's own clock ended it, not the agent and not a gate. dawn
	// assigns it to a dispatched attempt that hit its AttemptWallClock, and to
	// nothing else (rule 4 above).
	//
	// A protocol may also RETURN it for a loop that never got to dispatch —
	// More() false at the first turn — because that is the only honest word
	// for "no gate ever voted because the lease ran out". That is the
	// protocol's own reading of More(), never a state dawn assigned to a
	// stage; see More.
	Exhausted State = "exhausted"
	// InfraError: dawn could not obtain a verdict. Retried with backoff inside
	// the attempt's own scope, against that scope's attempt lease.
	InfraError State = "infra_error"
	// Cancelled: cancelled from outside the run.
	Cancelled State = "cancelled"
)

// Image is a digest-pinned OCI reference ("name@sha256:..."). Tags are
// rejected at dispatch: the whole soundness argument is an argument about
// which bytes are in which image.
type Image string

// Agent is an agent profile: dawn's knowledge of how to drive one CLI, over a
// digest-pinned image. It is a fact about that image, never a knob — an author
// picks a profile and declares nothing else about it, least of all its
// concurrency, which is a correctness property dawn fixes.
//
// The name and the CLI image are dawn's own knowledge, so they are unexported:
// a protocol selects a profile, it never reads one apart. FanOut is the single
// exception, and only because the per-agent cap is a correctness fact a
// protocol is obliged to state in its caveats.
type Agent struct {
	name  string
	image Image
	// FanOut reports whether this profile may run more than one attempt at a
	// time. Readable so a protocol can say in source that a fan is
	// single-vendor and its blind spots are therefore correlated.
	FanOut bool
}

// The profiles dawn ships. Only ClaudeCode may fan out.
var (
	ClaudeCode = Agent{name: "claude-code", image: "dawn-claude-code@sha256:0000000000000000000000000000000000000000000000000000000000000000", FanOut: true}
	Codex      = Agent{name: "codex", image: "dawn-codex@sha256:0000000000000000000000000000000000000000000000000000000000000000", FanOut: false}
)

// Gate is a stage's verifier. Build one with SoundGate, FormatOnlyGate or
// NoGate: soundness is declared statically, at the call site, as part of
// naming the gate, so there is no soundness field to forget and no way to pin
// a verifier image without saying what it establishes.
//
// The constructors make the illegal states unnameable, not unrepresentable: a
// zero Gate, SoundGate("") and NoGate("") are one value, and dawn rejects it
// at dispatch for the same reason it rejects an unpinned Image — a gate is
// either a digest-pinned image or a stated reason there is none.
type Gate struct {
	image      Image
	formatOnly bool
	reason     string
}

// SoundGate is a gate whose passed verdict is a claim about the world. It is
// the only kind of gate a stage can reach Passed through.
func SoundGate(img Image) Gate { return Gate{image: img} }

// FormatOnlyGate is a gate that can check shape but establish nothing. dawn
// clamps such a stage to Unverified mechanically: there is no code path from
// here to Passed. Its metrics are still the only way a number leaves a stage
// dawn cannot verify.
func FormatOnlyGate(img Image) Gate { return Gate{image: img, formatOnly: true} }

// NoGate is the plain absence of a verifier plus the reason it is absent. dawn
// disables the runner's verifier; the stage is Unverified by construction. The
// reason is required — an empty one is rejected at dispatch — because an
// ungated stage is the one place a protocol can quietly stop checking
// anything, and it is written into the run record where a reader can see it.
func NoGate(reason string) Gate { return Gate{reason: reason} }

// Lease is everything an author may declare about scarcity: three numbers, all
// time or count. There is no token ceiling and no dollar ceiling — provider
// quota has no published bound and its exhaustion may be unobservable, so dawn
// tracks draw rate and never pretends to hold a balance.
//
// A lease sized to exactly the work it funds is a lease that cannot pay for
// one flake: an infra_error retry spends the same Attempts counter and the
// same clock as a deliberate attempt. Every lease below is therefore declared
// as work PLUS headroom, and says in source how much of each — nothing in the
// surface checks the arithmetic (TRACE.md, frictions).
type Lease struct {
	// Attempts bounds every dispatch in the scope, including fan children and
	// including infra_error retries. It is the only bound on a nested fan.
	Attempts int
	// WallClock bounds the scope as a whole. It has to cover the attempts
	// Attempts funds, at AttemptWallClock apiece, plus their retry backoff.
	WallClock time.Duration
	// AttemptWallClock is dawn's own per-attempt clock: the only thing dawn
	// turns into Exhausted, and the only thing that stops one wedged agent
	// from eating the whole scope.
	AttemptWallClock time.Duration
}

// Artifact is one declared output that crossed a stage boundary. Two fields,
// because two are all a protocol can act on: the logical name from dawn's
// list, and the digest of the bytes that carried it. The path, the size and
// the producing attempt are dawn's bookkeeping, not the author's vocabulary.
type Artifact struct {
	Name   string
	Digest string
}

// Manifest is what a stage's declared outputs amounted to — the task's
// artifacts list, which dawn owns and the author never restates. A declared
// output that is missing or invalid is infra_error, always; an empty one is
// present and valid. A receiving stage gets those files read-only at a fixed
// path plus this record; a protocol reads the digests to decide whether
// anything actually changed.
type Manifest []Artifact

// Stage is one runner call: one agent, one environment, one instruction, one
// gate. Six fields, and every one of them is something dawn cannot know.
type Stage struct {
	// ID is stable across attempts and must be unique within the RUN, not
	// merely within its scope: attempt_id hashes the stage id and no scope
	// path, so two stages sharing an id are one stage to recovery unless
	// something else in that hash — the content or input digest — differs.
	// Relying on that difference is relying on an accident; qualify the id.
	ID string
	// Agent is the profile that drives the attempt.
	Agent Agent
	// Env is the digest-pinned environment image. The task's input tree is
	// baked into it, so naming the image is naming the task.
	Env Image
	// Prompt is the prose instruction. It is the only thing that tells the
	// agent what to hand back, which makes it part of the integrity argument.
	Prompt string
	// Inputs are earlier results whose artifacts this stage reads. dawn mounts
	// them read-only at a fixed path together with their manifest. This is the
	// only way anything crosses an attempt boundary: every attempt is a fresh
	// container, so what survives, survives as bytes.
	Inputs []Result
	// Gate is the verifier. The zero value is not a gate — use NoGate.
	Gate Gate
}

// Result is a stage's terminal outcome.
type Result struct {
	// State is the verdict. It is the whole branching vocabulary a protocol
	// has; there is nothing else to switch on.
	State State
	// Manifest records what this attempt's declared outputs were.
	Manifest Manifest
}

// Metric reads one number the gate wrote alongside its reward — including
// "reward" itself. dawn parses no CLI output, so this is the only channel by
// which a stage's own arithmetic reaches the control flow. ok is false when
// the gate wrote no such metric; a missing metric is never silently zero.
func (r Result) Metric(name string) (value float64, ok bool) { panic("prototype") }

// Actuate performs an external effect. It runs in dawn's own process, where
// the credentials live, and fires only on Passed — on any other state it does
// nothing and reports why. dawn deduplicates its own retries against its own
// durable record, keyed by the attempt_id.
func (r Result) Actuate(fn func(*Actuation) error) error { panic("prototype") }

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
func (a *Actuation) Published(name string) string { panic("prototype") }

// Scope is a bounded region of a run holding one lease. Everything dispatches
// through a scope, or the lease would be decorative. infra_error backoff
// happens inside the scope that owns the attempt, so a flaky child cannot
// spend a sibling's budget.
type Scope struct{}

// Main is the process entry point: it makes func main legal. dawn owns the
// journal and the restart sweep, so it must be able to re-enter the protocol
// itself — on restart a stage whose result.json exists is never re-run, and
// the recorded result is handed straight back.
//
// The State the protocol returns is the RUN's terminal state: dawn writes it
// into the run record and exits on it. That is the only consumer, and it is
// why every protocol here spends care on the difference between a measurement
// and a catastrophe that produced the same artifacts.
func Main(name string, root Lease, protocol func(*Scope) State) { panic("prototype") }

// Scope opens a nested scope with its own lease. Nesting is how a protocol
// says that one instance's whole escalation is bounded separately from the
// run.
func (s *Scope) Scope(id string, lease Lease) *Scope { panic("prototype") }

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
func (s *Scope) More() bool { panic("prototype") }

// Run dispatches one attempt and blocks until it reaches a terminal state.
// Machine overcapacity is queueing, never an error.
func (s *Scope) Run(stage Stage) Result { panic("prototype") }

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
func (s *Scope) Fan(n int, mk func(i int) Stage) []Result { panic("prototype") }

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
func (s *Scope) Record(name string, value any) { panic("prototype") }
