// Package dawn runs agents against verifiers and believes only the verifier.
//
// A protocol is a Go program written against this package: it declares stages,
// bounds them with leases, reads back a closed set of six states, and — only on
// passed — actuates. dawn compiles each stage into a Harbor task it owns
// entirely, dispatches it, and assigns a state by first match without parsing a
// single line of agent output. Everything that makes the verdict trustworthy is
// dawn's, not the author's.
//
// This is the whole vocabulary an author has. Nothing else is expressible, on
// purpose: every construct here was forced by at least one of the four
// independently drafted protocols (cybergym, mdash, vdh, pr-ci), or by a
// settled decision that TRACE.md names instead. See TRACE.md for the
// derivation, including what was cut.
//
// Everything the settled decisions already fix is absent from this surface:
// the verifier always runs separate, no-network, after the agent's container
// is gone, and always writes reward.json; the artifacts list, the output
// paths, the retry backoff, the per-agent concurrency cap and the idempotency
// key are dawn's, not the author's. An author cannot restate them and so
// cannot contradict them — which is why a Stage has no output field at all.
// Naming the Env image is naming the task, and the task's declared artifacts
// are already dawn's list.
package dawn

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

// Decided reports whether this outcome came from the work rather than from a
// failure to run it: a gate voted (Passed/Rejected), or a stage that has no
// sound gate completed (Unverified).
//
// It is deliberately the ONLY definition of that question in the package.
// Every earlier pass of the prototype re-derived it inline at each call site
// — "v != InfraError" here, "len(found) > 0" there, "State == Unverified"
// somewhere else — and each pass fixed one site and missed another, because
// local reasoning at N sites reliably produces N-1 correct sites. Exhausted,
// InfraError and Cancelled are all "dawn never obtained a verdict", and any
// site that wants to know whether anything was learned must call this.
func (s State) Decided() bool {
	return s == Passed || s == Rejected || s == Unverified
}

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
// same clock as a deliberate attempt. Every lease a protocol writes is
// therefore declared as work PLUS headroom, and says in source how much of
// each — nothing in the surface checks the arithmetic (TRACE.md, frictions).
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

// dawn's retry schedule. Harbor's max_retries is 0, so dawn owns retry: an
// infra_error is re-dispatched inside the scope that owns the attempt, after
// sleeping. The delay before the nth retry of a scope is the base doubled n-1
// times and clamped — 15s, 30s, 1m, 2m, 4m, 5m, 5m, ... — so a scope that
// spends its whole counter on one wedged dependency backs off to a fixed
// 5m poll instead of growing without bound.
//
// These are constants and not knobs on purpose: the backoff is one of the
// things the package doc lists as dawn's rather than the author's, and
// WallClock is derived from them (see Dispatching) rather than declared
// against them.
const (
	retryBackoffBase = 15 * time.Second
	retryBackoffCap  = 5 * time.Minute
)

// retryBackoffGap is the delay before the nth retry within one scope: the base
// doubled n-1 times, clamped. It is the schedule itself, and the only place it
// is written down — Dispatching funds it ahead of time, Scope.Run sleeps it.
func retryBackoffGap(n int) time.Duration {
	d := retryBackoffBase
	for i := 1; i < n && d < retryBackoffCap; i++ {
		d *= 2
	}
	if d > retryBackoffCap {
		d = retryBackoffCap
	}
	return d
}

// retryBackoffTotal is the worst-case time a scope of n attempts spends
// asleep: every attempt after the first waited out a full backoff, which is
// exactly what happens when a scope burns its whole counter retrying one
// infra_error. n-1 gaps, because nothing is slept before the first dispatch.
func retryBackoffTotal(attempts int) time.Duration {
	var total time.Duration
	for n := 1; n < attempts; n++ {
		total += retryBackoffGap(n)
	}
	return total
}

// Dispatching builds the lease of a scope that dispatches attempts itself.
//
// Its WallClock is DERIVED rather than chosen, because dawn admits against ITS
// concurrency and not the author's. A scope clocked below the serial floor
// makes its own later attempts unreachable; they return Exhausted, which then
// feeds the guards that ask whether a gate voted. Hand-written clocks got this
// wrong at three levels of two protocols across five passes, so the field is
// no longer hand-written: the invariant holds by construction instead of by
// review.
//
// The floor is attempts x perAttempt PLUS dawn's own retry backoff, not
// attempts x perAttempt alone. The prototype derived only the running time and
// so under-clocked every scope by the sleeping time: Attempts funds retries,
// each retry sleeps before it dispatches, and that sleep is charged to the
// same WallClock. The backoff model is worst-case and closed-form — every
// attempt after the first waited out a full clamped-exponential gap
// (retryBackoffTotal) — because the clock has to hold when the scope spends
// its whole counter on flakes, which is the case that was silently unfunded.
//
// A scope that only opens sub-scopes dispatches nothing and declares a plain
// Lease with no AttemptWallClock.
func Dispatching(attempts int, perAttempt time.Duration) Lease {
	return Lease{
		Attempts:         attempts,
		WallClock:        time.Duration(attempts)*perAttempt + retryBackoffTotal(attempts),
		AttemptWallClock: perAttempt,
	}
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
	// metrics is what the gate wrote alongside its reward, read through
	// Metric. Unexported because it is not vocabulary: a protocol asks for a
	// name and is told whether the gate wrote one, and can neither enumerate
	// nor forge the set.
	metrics map[string]float64
}

// anyDecided reports whether any child of a fan produced an outcome from the
// work. Unexported: it is a loop over State.Decided, not surface.
func anyDecided(rs []Result) bool {
	for _, r := range rs {
		if r.State.Decided() {
			return true
		}
	}
	return false
}
