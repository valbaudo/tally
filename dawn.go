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
// is gone, and always writes reward.json; the collected root, the retry
// backoff, the per-agent concurrency cap and the idempotency key are dawn's,
// not the author's. An author cannot restate them and so cannot contradict
// them. Naming the Env image is naming the task, but the ROOT being dawn's
// (outputDir) does not make the NAMES inside it dawn's too — nothing invents
// a name for a file the agent has not written yet. That is why a Stage does
// have an output field, Outputs: the one thing dawn cannot know in advance
// and must still fix before dispatch, in Go source, rather than leave to
// whatever the agent decides to call its own handover.
package dawn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// State is a stage's outcome. Six values, and dawn assigns no other; the
// numeric reward is never one of them — it rides alongside as a metric
// (Result.Metric). The zero State is not one of the six, which is why a
// protocol seeds a loop variable with a state rather than with a zero Result.
//
// dawn assigns a state by first match, parsing no agent output at all:
//
//  1. external cancel                                    -> Cancelled
//  2. dawn's own timeout hit                             -> Exhausted
//  3. declared output missing or invalid                 -> InfraError (always)
//  4. output present, gate wrote no valid reward         -> InfraError
//  5. output present, gate ran, clean exit               -> Passed / Rejected,
//     or Unverified for a format-only gate or no gate at all
//
// The first two are the facts dawn owns directly — both are ctx.Err() — and
// they are read before anything inferred from what a killed attempt left
// behind. Rules 3 and 4 used to come first, which made Exhausted unreachable:
// a clock-kill always leaves a missing output and a Harbor fault, so the
// timeout was filed as infra_error and retried into the same clock.
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
	// nothing else (rule 2 above). It says nothing about whether the declared
	// output was written or the gate ran — a clock-kill usually means neither,
	// because the agent was still working when the clock fired. That is the
	// point: the clock firing is a complete explanation on its own, and it is
	// not retried, because retrying into the same clock cannot end differently.
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

// Agent is an agent profile: dawn's knowledge of how to drive one CLI — its
// name, its model and its effort. It is a fact about that CLI, never a knob —
// an author picks a profile and declares nothing else about it, least of all
// its concurrency, which is a correctness property dawn fixes.
//
// It does NOT carry an image. The image a stage runs in is Stage.Env, and the
// agent CLI is baked into it: Harbor installs the CLI before the agent phase
// begins, so on dawn's no-network environments an unbaked CLI cannot reach a
// package mirror and the trial dies in setup. A profile therefore names a CLI,
// and the environment supplies its bytes. Agent once carried an image field
// of its own, pinned to a literal sha256:0000…; nothing ever read it, so it
// pinned nothing while claiming to — the exact lie Image exists to prevent.
//
// model and effort are facts dawn pins because leaving either unpinned does
// not mean the CLI runs with none. Harbor's adapter finds ANTHROPIC_MODEL (or
// --effort) unset and the CLI resolves it from ambient account state instead,
// so the identical protocol can silently run a different model tomorrow, on a
// host whose account state changed under it, with nothing in dawn's own
// receipt to say so. Pinning them here is what lets the receipt record what
// dawn told Harbor to run rather than nothing at all — see harborArgs.
//
// EVERY field is dawn's own knowledge, so all of them are unexported: a
// protocol selects a profile, it never reads one apart and it certainly never
// writes one. fanOut was exported once, on the theory that a protocol would
// read it to state in its caveats that a fan is single-vendor. No protocol
// ever did. What the export bought instead was the opposite of the paragraph
// above: ClaudeCode and Codex are package-level vars, so `dawn.Codex.fanOut =
// true` — or a copy with the name kept and the bool flipped — resized codex's
// semaphore from 1 to the whole pool. A field exported so an author could
// STATE a correctness fact let the author OVERRIDE it.
type Agent struct {
	name   string
	model  string // verbatim to `harbor -m`; "" means dawn pins nothing (oracle, nop)
	effort string // verbatim to `--ak reasoning_effort=`; same rule
	// fanOut reports whether this profile may run more than one attempt at a
	// time. Read only by acquireAgentGate, which sizes the per-agent
	// semaphore from it.
	fanOut bool
}

// The profiles dawn ships. Only ClaudeCode may fan out. ClaudeCode's model is
// pinned to what every run has actually been doing — measured off Harbor's
// raw agent logs, since nothing before this recorded it — so the pin is
// behaviour-neutral. Codex's model stays "": the CLI prints no effective
// default anywhere dawn can read, and guessing one would be inventing a fact
// dawn has not measured; dawn will honestly record that it pinned nothing.
// Neither profile pins an effort yet, for the identical reason.
var (
	ClaudeCode = Agent{name: "claude-code", model: "claude-sonnet-5", fanOut: true}
	Codex      = Agent{name: "codex", fanOut: false}
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
	hosts      []string // non-empty ⟹ a live gate; the enforced engagement scope
}

// SoundGate is a gate whose passed verdict is a claim about the world. It is
// the only kind of gate a stage can reach Passed through.
//
// Soundness is the AUTHOR's claim, and dawn believed three gates that did not
// have it. The rule that would have caught all three by inspection, in the
// order they were found:
//
//	A gate is sound only if the artifact supplies DATA to a procedure it
//	cannot alter.
//
// There are exactly two ways to break it, and dawn's own targets managed both.
// The artifact must not get to CHOOSE THE TEST: vdh-adyen let a finding name
// both the exploit request and its control, so two unrelated endpoints — a
// storefront homepage that answers 200 and an API that answers 401 — satisfied
// "served where the control was refused" with no vulnerability anywhere near
// it. And the artifact must not RUN INSIDE THE TEST: pr-ci measured a patch by
// the exit code of a process that imports the patched file, so `sys.exit(0)`
// at import ended that process 0 having run no tests at all. Its first repair
// compared a value the same process printed, which the patched file could
// print first, and was forged the same afternoon.
//
// cybergym obeys the rule and is the one gate no forgery was found against:
// the agent hands over argv bytes, and the two builds, the runner and the
// comparison are all the gate's own.
//
// dawn cannot check the rule — it is a property of a gate's reasoning, not of
// any bytes dawn can read. What dawn does check is that the gate PROVES ITSELF
// before dawn will spend an agent on it: see gateSelftest in harbor.go, which
// runs the gate's own selftest in the pinned image and refuses to dispatch if
// it does not pass. What that selftest must contain is the contract at
// outputDir, and the case that matters is the last one — an artifact that
// scores 1 while accomplishing nothing.
func SoundGate(img Image) Gate { return Gate{image: img} }

// LiveGate is a sound gate that reaches the named hosts. Its passed verdict is
// a real claim about the world AND a function of those hosts' state at the
// moment it ran — not of pinned bytes alone, which is what every other gate's
// verdict is. That is exactly right for proving an exploit against a running
// system and exactly wrong for the claim a SoundGate makes, so it is a
// separate constructor rather than an optional argument: `grep LiveGate`
// enumerates every stage in every protocol whose verdict touched a live
// system, and an empty-versus-non-empty argument list would bury that.
//
// hosts are the engagement scope, and they are enforced by the container's
// egress policy rather than by the gate's good behaviour. dawn is therefore
// entitled to say where the gate COULD reach; it observes no traffic and
// never claims where it did.
//
// The hosts are deliberately NOT part of attempt identity — see contentDigest,
// whose comment says why nothing may be folded into that hash after the fact.
// Two stages that differ only in gate hosts must differ in their Stage.ID, by
// the same "qualify the id by whatever varies" rule Stage.ID already states.
func LiveGate(img Image, hosts ...string) Gate { return Gate{image: img, hosts: hosts} }

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

// kind is the gate's soundness as one word, for the receipt. It reads the
// same constructors an author already chose between, so a receipt can never
// disagree with the source about what a stage established.
func (g Gate) kind() string {
	switch {
	case g.image == "":
		return "none"
	case g.formatOnly:
		return "format_only"
	case len(g.hosts) > 0:
		return "live"
	default:
		return "sound"
	}
}

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

// Manifest is what a stage's DECLARED outputs (Stage.Outputs) amounted to,
// looked up by name under the collected root — never a walk of whatever else
// the agent left there (see digest in harbor.go). A declared output that is
// missing, uncollectable, or over maxOutputBytes is infra_error, always; a
// stage that declares no outputs at all gets an empty Manifest, and that is
// present and valid. A receiving stage gets the declared files read-only at
// a fixed path plus this record; a protocol reads the digests to decide
// whether anything actually changed.
type Manifest []Artifact

// Stage is one runner call: one agent, one environment, one instruction, one
// set of declared outputs, one gate. Seven fields, and every one of them is
// something dawn cannot know.
type Stage struct {
	// ID is stable across attempts and must be unique within the RUN, not
	// merely within its scope: attempt_id hashes the stage id and no scope
	// path, so two stages sharing an id are one stage to recovery unless
	// something else in that hash — the content or input digest — differs.
	// Relying on that difference is relying on an accident; qualify the id.
	// An id is one segment, [a-zA-Z0-9][a-zA-Z0-9._-]* (Harbor's own
	// task-name grammar, refused at dispatch otherwise), so attempts/<id>/ is
	// the id itself and no two ids share a directory. Qualify with '-' or
	// '.': "pov-0", "hunt.sql-injection".
	ID string
	// Agent is the profile that drives the attempt.
	Agent Agent
	// Env is the digest-pinned environment image. The task's input tree is
	// baked into it, so naming the image is naming the task.
	Env Image
	// Prompt is the prose instruction. It is the only thing that tells the
	// agent what to hand back, which makes it part of the integrity argument.
	Prompt string
	// Outputs is the exact set of relative paths under dawn's fixed output
	// directory that the agent must write. Each entry is both the logical
	// name AND the path — there is no separate Path field, because the root
	// is already dawn's and only the name inside it was ever in question.
	// writeTask puts these exact paths into the instruction, so the agent is
	// never guessing what to call its handover; digest looks up each one by
	// name instead of walking the output tree, so a file the agent invented
	// and never declared cannot enter the Manifest.
	//
	// This is not pedantry. Today Artifact.Name is filepath.Rel's echo of
	// whatever the agent chose to call its file — harmless only because
	// nothing yet treats that string as a path. The moment Stage.Inputs
	// materialises a PRIOR stage's manifest into a new container, that
	// string becomes a mount path, and an agent in a container it never
	// runs in would control a path inside it. A declared name is fixed here,
	// in Go source, before either container exists.
	Outputs []string
	// Inputs are earlier results whose artifacts this stage reads. This is the
	// only way anything crosses an attempt boundary: every attempt is a fresh
	// container, so what survives, survives as bytes.
	//
	// They are BAKED, not mounted, and the difference is worth stating because
	// this doc claimed the wrong one for months while emitting nothing at all.
	// Harbor gives a task one environment — a prebuilt image OR a Dockerfile in
	// the task directory — so dawn generates that Dockerfile: FROM the stage's
	// own pinned Env, COPY the inputs. See inputDir in harbor.go for why the
	// docker_image key then has to be absent.
	//
	// Each input arrives at /app/inputs/<its Stage>/<name>, and dawn writes
	// those paths into the instruction so the prompt never has to. Order is
	// still hashed — attemptID folds in inputDigest, so two stages differing
	// only in which results they consume are different attempts — but order no
	// longer decides where anything lands. See Result.Stage.
	//
	// Measured end to end (experiments/transportprobe): a stage wrote a file,
	// the next stage read it back, and the two manifests carry the same digest.
	Inputs []Result
	// Gate is the verifier. The zero value is not a gate — use NoGate.
	Gate Gate
}

// Result is a stage's terminal outcome.
type Result struct {
	// State is the verdict. It is the whole branching vocabulary a protocol
	// has; there is nothing else to switch on.
	State State
	// Stage is the Stage.ID that produced this result, and downstream it is
	// this result's DIRECTORY NAME under /app/inputs.
	//
	// It is exported because it is the only thing that makes a multi-input
	// stage legible. Inputs used to arrive at /app/inputs/<position>, so a
	// prompt had to say "the queue, followed by the verdicts" and a gate had
	// to be keyed on the order of a Go slice — a coupling paid for in prose,
	// invisible in the evidence tree, and hopeless at a fan of fifty. Named by
	// stage, the filesystem is the query: /app/inputs/hunt-r1-*/finding.json.
	//
	// Slice order still feeds inputDigest, so it remains part of attempt
	// identity; it just no longer decides where anything lands.
	Stage string
	// Manifest records what this attempt's declared outputs were.
	Manifest Manifest
	// metrics is what the gate wrote alongside its reward, read through
	// Metric. Unexported because it is not vocabulary: a protocol asks for a
	// name and is told whether the gate wrote one, and can neither enumerate
	// nor forge the set.
	metrics map[string]float64
	// publishDir is the gate's own publish directory for this trial — the
	// same trial dir trial.read() already resolves to find reward.json, one
	// level under it (verifier/publish). Set by classify unconditionally,
	// the same pattern as metrics above: it is only MEANINGFUL when a gate
	// ran and voted, but Actuate only ever calls the closure on Passed, and
	// classify only reaches Passed for a gated, rewarded trial (harbor.go's
	// Rule 5) — so by the time anything reads it, it is always valid.
	publishDir string
	// artifactsDir is where this attempt's declared outputs actually sit on
	// the host. Unexported for the same reason publishDir is: a protocol names
	// a Result in Stage.Inputs and dawn resolves the bytes — an author never
	// handles a path, so an author can never point one somewhere else.
	artifactsDir string
	// drew is what this attempt drew, for the receipt. Unexported for the
	// same reason metrics is: consumption is something dawn reports, never
	// something a protocol branches on. There is no denominator to compare
	// it against, so there is nothing here for control flow to do.
	drew *draw
	// attemptID and run exist so Actuate can find a durable, per-run,
	// per-attempt dedup record without a caller ever naming one: Scope.Run
	// is the only writer, right before it hands the Result back.
	attemptID string
	run       *run
}

// attemptID names one dispatch attempt: stage.ID, a digest of the stage's own
// content, a digest of its resolved inputs, and the attempt number Scope.Run
// already counts (the nth dispatch of this scope's retry loop).
//
// LOUD COMMENT, because this hash is append-only vocabulary the instant
// anything durable is keyed by it (the per-run actuation record, Step's
// dedup): if a later ticket changes what feeds contentDigest or inputDigest,
// every existing dedup record silently starts meaning something else — same
// key, different attempt, and dawn would skip an effect that never actually
// ran. Recovery, whenever it lands, must layer run/lineage identity
// ALONGSIDE this hash — a sixth field appended elsewhere — never fold a new
// ingredient INTO this hash after the fact.
func attemptID(s Stage, attemptNumber int) string {
	type key struct {
		StageID       string
		Content       string
		Input         string
		AttemptNumber int
	}
	// Marshal of plain strings, slices and ints never errors.
	b, _ := json.Marshal(key{s.ID, contentDigest(s), inputDigest(s.Inputs), attemptNumber})
	return sha256hex(b)
}

// contentDigest hashes the Stage fields that determine the generated task —
// deliberately NOT the attempt clock (writeTask's timeout_sec, decided by the
// scope, not the Stage), which varies per retry without changing the work a
// retry repeats. Also deliberately NOT Gate.hosts: adding a field to this
// struct changes the marshalled bytes for every existing stage — a nil slice
// becomes "GateHosts":null — which would silently re-key every dedup and
// resume record keyed on attemptID, exactly what attemptID's LOUD COMMENT
// forbids. A LiveGate's hosts are therefore not attempt identity; see
// LiveGate's own comment for what that means for Stage.ID. Also deliberately
// NOT Agent.model or Agent.effort, for the same reason: resumeResult enforces
// both instead, as the sixth field alongside the hash that attemptID's LOUD
// COMMENT prescribes rather than folding a new ingredient into it.
func contentDigest(s Stage) string {
	type content struct {
		Agent   string
		Env     Image
		Prompt  string
		Outputs []string
		Gate    Image
	}
	b, _ := json.Marshal(content{s.Agent.name, s.Env, s.Prompt, s.Outputs, s.Gate.image})
	return sha256hex(b)
}

// inputDigest hashes the ordered Manifest of each Result in Stage.Inputs —
// already content digests, so this is a digest of digests. Order matters:
// Inputs is a list a protocol wrote in a deliberate order.
func inputDigest(inputs []Result) string {
	type artifact struct{ Name, Digest string }
	manifests := make([][]artifact, len(inputs))
	for i, r := range inputs {
		for _, a := range r.Manifest {
			manifests[i] = append(manifests[i], artifact{a.Name, a.Digest})
		}
	}
	b, _ := json.Marshal(manifests)
	return sha256hex(b)
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
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
