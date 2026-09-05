package dawn

// The Harbor runner: one Stage in, one Harbor trial out, one struct of raw
// facts back. Nothing here decides a State — classification reads these facts
// and is another ticket's. What IS here is every settled rule about the task
// dawn emits, because those rules are only true if the compiler is the single
// place a task.toml is ever written.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// dawn's fixed output convention, and the whole of the artifacts list it
// writes into every task. It is a directory and not a file so that a Manifest
// can have more than one entry without the author ever naming a path; the
// logical names in the Manifest are the file names under it.
//
// It is deliberately NOT under /logs/verifier. Harbor restores declared
// artifacts into the verifier container BEFORE the gate runs, so an artifact
// declared there would let the agent drop its own reward.json and forge the
// verdict outright. Nothing dawn emits may name a path under that tree.
// It is also the gate's side of the contract, and the one place it is written:
// a gate finds each declared output at outputDir/<name>, exactly where the
// agent was told to write it; it runs as /tests/test.sh, no-network, with
// nothing else of the agent's; it publishes to /logs/verifier/publish/<name>;
// and it writes {"reward": n} to /logs/verifier/reward.json as its LAST act,
// never skipping the write because a file is already there (Harbor restores
// the agent's artifacts before the gate runs; the last writer wins).
//
// That number is a claim about the artifact and about nothing else. A gate
// that cannot make the claim — its own baked app will not start, its own
// binaries or repo are missing, its own control run fails — writes no
// reward.json and exits. The missing file is the whole of "could not check":
// classify reads it as infra_error and the scope retries. It is never a 0,
// which is a rejection of an agent nobody checked. So a gate does not catch
// its own failures to score them; a crash and a deliberate exit mean the same
// thing, and dawn reads neither an exit code nor a line of output to know it.
//
// None of this reaches a gate at runtime — it is baked in — so every gate
// image proves it at build time by running its own test.sh three ways:
// against nothing (0), against a planted oracle artifact (1), and with its
// own environment sabotaged (no reward.json) (experiments/harbor-targets/*/gate).
const outputDir = "/app/outputs"

// generatedTaskDirName is the basename runTrial gives the task directory it
// generates for Harbor. It is load-bearing, not cosmetic: Harbor's
// LocalTaskId.get_name() reads a task directory's basename verbatim as the
// seed for trial_name, and reap.go's whole reap set is every compose project
// whose name starts with this string plus "__" (see reap.go for the rest of
// the chain). This is a STRING MATCH, not a cryptographic tag — see reap.go.
const generatedTaskDirName = "dawn"

// taskDirFor is the exact join runTrial uses to place the generated task
// under one attempt's evidence directory — pulled out to a named function so
// the naming decision itself is testable without invoking Harbor.
func taskDirFor(evidence string) string { return filepath.Join(evidence, generatedTaskDirName) }

// The agent phase reaches these two hosts and no others. Verified by grepping
// the pinned CLI binary: everything else it contacts degrades quietly.
var agentAllowedHosts = []string{"api.anthropic.com", "platform.claude.com"}

// gateTimeout bounds the verifier, which is dawn's own program on dawn's own
// pinned image. It is not the attempt clock and not the author's business.
const gateTimeout = 10 * time.Minute

// maxOutputBytes bounds a single declared output. 64 MiB: comfortably above
// anything this package's own protocols hand back today (a unified diff, a
// small report, a JSON manifest — all measured in KB), while still small
// enough that hashing it, holding it, and shipping it to a later stage never
// becomes the bottleneck an agent could weaponise by dumping arbitrary bulk
// into outputDir. Raise it the day a protocol legitimately needs to hand
// back something bigger.
//
// An oversized declared output is dawn refusing to trust its own collection,
// not a gate voting no: digest flips trial.Present to false and lets it ride
// the SAME infra_error rule a missing output already uses (Rule 2 in the
// State doc) rather than adding a seventh state, or a "rejected" no gate
// ever voted for. "Never truncate" means dawn never accepts partial bytes as
// the real file — a file over the cap is taken whole or not at all.
//
// The honest cost: infra_error implies transience and spends retry budget
// the same way a flaky collection does. A stage that is SYSTEMATICALLY
// oversized — not flaky, wrong every time — burns its whole Attempts counter
// looking exactly like bad luck before the lease gives out. That is the
// accepted price of not inventing a seventh state for one classifier rule.
const maxOutputBytes = 64 << 20 // 64 MiB

// harborShutdownGrace bounds how long dawn waits, after asking harbor to
// stop, before forcing it. Measured, not guessed
// (docs/research/harbor-crash-cancel-behaviour.md): a SIGTERM'd harbor runs
// its own handler, tears down Docker cleanly and writes a reconstructable
// result.json with exception_type=CancelledError in ~14s; SIGKILL — what
// exec.CommandContext sends by default — orphans the container and its
// network forever with no result.json at all. 30s gives roughly 2x headroom
// over the observed worst case, and is still short against an attempt clock
// measured in minutes.
const harborShutdownGrace = 30 * time.Second

// terminateGracefully wires cmd so that a cancelled context asks the process
// to stop the way harbor itself expects to be asked — SIGTERM, not
// exec.CommandContext's default SIGKILL — and bounds how long dawn waits for
// that before forcing it. grace is a parameter (not always harborShutdownGrace)
// so the real timing can be exercised by a test without a 30-second wait.
func terminateGracefully(cmd *exec.Cmd, grace time.Duration) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = grace
}

// clockOutcome reads what ended an attempt's context: dawn's own per-attempt
// clock (context.DeadlineExceeded, Rule 4 — Exhausted) or the run's parent
// context firing from outside (context.Canceled — Main's SIGINT/SIGTERM
// handler, Rule 1 — Cancelled), or neither. A pure function of ctx.Err() so
// the two are testable without a subprocess.
func clockOutcome(err error) (timedOut, cancelled bool) {
	switch err {
	case context.DeadlineExceeded:
		return true, false
	case context.Canceled:
		return false, true
	default:
		return false, false
	}
}

// trial is what one Harbor trial amounted to, as facts and not as a verdict.
// Everything a classifier needs to reach one of the six states is here, and
// nothing here has already reached one.
type trial struct {
	// Dir is the run directory: generated task, harbor log, job output. Kept
	// after the trial because the gate's published bytes live under it.
	Dir string
	// Rewards is what the gate wrote to /logs/verifier/reward.json, as Harbor
	// parsed it. Empty and Rewarded false when no valid reward was written —
	// a crashed gate emits no verdict, which is the honest answer.
	Rewards  map[string]float64
	Rewarded bool
	// Outputs is the Manifest of the stage's DECLARED outputs, looked up by
	// name under the collected outputDir — never a walk of whatever else the
	// agent left there (see digest). Present is false the instant any
	// declared name was missing, uncollectable, or over maxOutputBytes;
	// there is no such thing as half a Manifest, which is always infra_error.
	Outputs Manifest
	Present bool
	// PublishDir is the gate's own publish directory on the host — the
	// verifier's trial dir plus "publish" — where a SoundGate hands the
	// actuator the bytes it verified rather than the bytes the agent sent.
	// Set unconditionally by read() from the same trialDir that yields
	// reward.json; meaningless when no gate ran, exactly like Rewards above.
	PublishDir string
	// TimedOut reports that dawn's own per-attempt clock ended the attempt
	// (ctx.Err() == context.DeadlineExceeded).
	TimedOut bool
	// Cancelled reports that the RUN's parent context ended the attempt from
	// outside dawn's own clock (ctx.Err() == context.Canceled) — Main's
	// signal.NotifyContext firing on SIGINT/SIGTERM. classify checks this
	// first, ahead of every other rule: an attempt cancelled from outside
	// never reads as a verdict of any kind.
	Cancelled bool
	// Fault is Harbor's exception for the trial, empty when there was none.
	Fault string
	// Drew is what the attempt drew. nil for agents that report nothing
	// (oracle, nop) and for a trial that never got far enough to report.
	Drew *draw
}

// draw is what one attempt drew, as Harbor normalized it. A receipt of what
// was spent, never a forecast of what remains: there is no denominator to
// forecast against — provider quota is an undocumented dual-cadence pool with
// no published token counts.
//
// A pointer, and nil when Harbor wrote no agent_result, because unknown is
// not zero. An attempt that died before reporting drew something; printing 0
// for it would be a claim dawn cannot make.
type draw struct {
	InputTokens  int     `json:"input_tokens"`
	CacheTokens  int     `json:"cache_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// pinned reports whether an image reference names bytes rather than a moving
// tag. The whole soundness argument is an argument about which bytes are in
// which image, so this is checked at dispatch and never assumed.
func (i Image) pinned() bool { return strings.Contains(string(i), "@sha256:") }

// writeTask compiles a Stage into a Harbor task directory. Harbor needs
// task.toml, instruction.md and an environment/ directory to exist; with a
// pinned [environment] docker_image that directory stays empty, and with a
// pinned [verifier.environment] no tests/ is needed at all — a custom verifier
// image never receives Harbor's injected /tests, so the gate is baked in.
func writeTask(dir string, s Stage, attempt time.Duration) error {
	if s.ID == "" {
		return fmt.Errorf("dawn: stage has no ID")
	}
	if !s.Env.pinned() {
		return fmt.Errorf("dawn: stage %s: env image %q is not digest-pinned", s.ID, s.Env)
	}
	gated := s.Gate.image != ""
	switch {
	case gated && !s.Gate.image.pinned():
		return fmt.Errorf("dawn: stage %s: gate image %q is not digest-pinned", s.ID, s.Gate.image)
	case !gated && s.Gate.reason == "":
		return fmt.Errorf("dawn: stage %s: ungated stage must state why (use NoGate)", s.ID)
	}

	if err := os.MkdirAll(filepath.Join(dir, "environment"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "instruction.md"), []byte(instruction(s)), 0o644); err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = \"1.4\"\n")
	fmt.Fprintf(&b, "artifacts = [%q]\n\n", outputDir)
	// fsSafe on the NAME as well as the path. Stage ids carry slashes — the
	// Stage.ID doc tells authors to qualify them, and a loop that samples
	// "pov/0", "pov/1" is doing exactly that — but Harbor refuses a task name
	// with more than the one slash this prefix adds: measured, "dawn/pov/0" is
	// rejected with "Either datasets or tasks must be provided" while
	// "dawn/pov-0" is accepted. dawn already sanitizes the id for the evidence
	// path and simply did not for the name, so the first protocol to qualify an
	// id the documented way spent its whole lease on infra_error before any
	// agent ran. The description keeps the id verbatim: nothing parses it.
	fmt.Fprintf(&b, "[task]\nname = %q\nversion = \"1.0.0\"\ndescription = %q\n\n",
		"dawn/"+fsSafe(s.ID), "dawn stage "+s.ID)

	// The environment is the task: the input tree is baked into this image, so
	// there is nothing to build and no Dockerfile to ship. Its baseline is
	// no-network; only the agent phase opens, and only onto two hosts.
	fmt.Fprintf(&b, "[environment]\ndocker_image = %q\nnetwork_mode = \"no-network\"\nos = \"linux\"\n\n", s.Env)
	fmt.Fprintf(&b, "[agent]\nnetwork_mode = \"allowlist\"\nallowed_hosts = [%s]\ntimeout_sec = %.1f\n\n",
		quoted(agentAllowedHosts), attempt.Seconds())

	if gated {
		// separate: the gate never sees the agent's filesystem, only the
		// artifacts Harbor restores into its own container. no-network: it
		// cannot phone anything, so its verdict is a function of bytes.
		fmt.Fprintf(&b, "[verifier]\nenvironment_mode = \"separate\"\nnetwork_mode = \"no-network\"\ntimeout_sec = %.1f\n\n", gateTimeout.Seconds())
		fmt.Fprintf(&b, "[verifier.environment]\ndocker_image = %q\nnetwork_mode = \"no-network\"\n", s.Gate.image)
	}
	return os.WriteFile(filepath.Join(dir, "task.toml"), []byte(b.String()), 0o644)
}

func quoted(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, ", ")
}

// instruction is the author's prose plus the one thing dawn will not let the
// author state: exactly which files it expects back, and where. dawn owns
// the declared list, so dawn — never the protocol, never the agent's own
// judgement — names every path the agent must write, so the agent is never
// guessing what to call its handover.
func instruction(s Stage) string {
	var b strings.Builder
	b.WriteString(s.Prompt)
	b.WriteString("\n\n---\n\n")
	if len(s.Outputs) == 0 {
		fmt.Fprintf(&b, "This stage declares no output files. Nothing written "+
			"into `%s`, or anywhere else in this container, crosses the boundary.\n", outputDir)
		return b.String()
	}
	b.WriteString("Hand your work over by writing exactly these file(s):\n\n")
	for _, name := range s.Outputs {
		fmt.Fprintf(&b, "  - `%s`\n", outputDir+"/"+name)
	}
	fmt.Fprintf(&b, "\nThose paths, and only those paths, are collected from this "+
		"container. Any other file left under `%s` is ignored; nothing outside "+
		"it crosses the boundary.\n", outputDir)
	return b.String()
}

// runTrial generates the task, runs exactly one Harbor trial against it, and
// reads back what happened. attempt is dawn's own per-attempt clock.
func runTrial(ctx context.Context, s Stage, attempt time.Duration, dir string) (trial, error) {
	// dir is the run's own evidence directory for this attempt. It is NOT a
	// temp dir: a trial that vanishes with the process cannot be inspected
	// afterwards, and "result.json present, trust it, never re-run" is exactly
	// an inspection. Everything this attempt produced stays here — the task
	// dawn generated, harbor's log, and the trial Harbor wrote.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return trial{}, err
	}
	t := trial{Dir: dir}
	taskDir := taskDirFor(dir)
	if err := writeTask(taskDir, s, attempt); err != nil {
		return t, err
	}

	args := []string{"run", "-p", taskDir, "-a", s.Agent.name, "-o", filepath.Join(dir, "jobs"),
		"--job-name", "trial", "-n", "1", "-q", "-y"}
	if s.Gate.image == "" {
		args = append(args, "--disable-verification")
	}

	ctx, cancel := context.WithTimeout(ctx, attempt)
	defer cancel()

	// Redirect, never pipe: a pipe in the middle would mask harbor's exit
	// status, and the exit status is the only thing that distinguishes "the
	// run failed" from "the run produced a verdict of zero".
	log, err := os.Create(filepath.Join(dir, "harbor.log"))
	if err != nil {
		return t, err
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, "harbor", args...)
	cmd.Stdout, cmd.Stderr = log, log
	terminateGracefully(cmd, harborShutdownGrace)
	runErr := cmd.Run()
	t.TimedOut, t.Cancelled = clockOutcome(ctx.Err())

	// A non-zero harbor is not itself an error: the trial may still have
	// produced a result, and a missing result is what "no verdict" looks like.
	if err := t.read(filepath.Join(dir, "jobs"), s.Outputs); err != nil {
		if runErr != nil {
			return t, fmt.Errorf("dawn: harbor failed (%w) and wrote no readable result: %v", runErr, err)
		}
		return t, err
	}
	return t, nil
}

// Harbor's per-trial result.json and artifact manifest, only the fields dawn
// reads. reward.json is Harbor's own highest-precedence source for these
// rewards; reading the file again behind Harbor's back would accept a verdict
// Harbor rejected, so this is the single reading of it.
type harborResult struct {
	AgentResult *struct {
		NInputTokens  int     `json:"n_input_tokens"`
		NCacheTokens  int     `json:"n_cache_tokens"`
		NOutputTokens int     `json:"n_output_tokens"`
		CostUSD       float64 `json:"cost_usd"`
	} `json:"agent_result"`
	VerifierResult *struct {
		Rewards map[string]float64 `json:"rewards"`
	} `json:"verifier_result"`
	ExceptionInfo *struct {
		Type    string `json:"exception_type"`
		Message string `json:"exception_message"`
	} `json:"exception_info"`
}

type manifestEntry struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Status      string `json:"status"`
}

func (t *trial) read(jobsDir string, outputs []string) error {
	hits, _ := filepath.Glob(filepath.Join(jobsDir, "*", "*", "result.json"))
	if len(hits) != 1 {
		return fmt.Errorf("dawn: expected one trial result under %s, found %d", jobsDir, len(hits))
	}
	trialDir := filepath.Dir(hits[0])
	t.PublishDir = filepath.Join(trialDir, "verifier", "publish")

	var r harborResult
	if err := readJSON(hits[0], &r); err != nil {
		return err
	}
	if a := r.AgentResult; a != nil {
		t.Drew = &draw{a.NInputTokens, a.NCacheTokens, a.NOutputTokens, a.CostUSD}
	}
	if v := r.VerifierResult; v != nil && len(v.Rewards) > 0 {
		t.Rewards, t.Rewarded = v.Rewards, true
	}
	if e := r.ExceptionInfo; e != nil {
		t.Fault = e.Type + ": " + e.Message
	}

	var entries []manifestEntry
	if err := readJSON(filepath.Join(trialDir, "artifacts", "manifest.json"), &entries); err != nil {
		return nil // no manifest: nothing was collected, Present stays false
	}
	for _, e := range entries {
		if e.Source != outputDir {
			continue
		}
		if e.Status != "ok" && e.Status != "empty" {
			continue // outputDir itself was not collected; Present stays false
		}
		m, ok, err := digest(filepath.Join(trialDir, e.Destination), outputs)
		if err != nil {
			return err
		}
		t.Outputs, t.Present = m, ok
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// digest looks up each of the stage's declared names under root — it does
// NOT walk root. An undeclared file left in the output directory is
// invisible to it: "declared" only means something if an undeclared file is
// inert, and a file that never enters the Manifest is never mounted anywhere
// downstream, so it cannot smuggle bytes into a later stage. The Manifest is
// therefore a record of contract compliance — did the agent write what dawn
// told it to write — never a filesystem audit of everything it left behind.
//
// present is false the instant any declared name is missing, is a directory,
// or exceeds maxOutputBytes; there is no partial credit; the whole trial's
// declared output set is either fully and honestly collected or it isn't
// (Rule 2 in the State doc, via trial.Present).
func digest(root string, names []string) (Manifest, bool, error) {
	m := make(Manifest, 0, len(names))
	for _, name := range names {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.IsDir() || info.Size() > maxOutputBytes {
			return m, false, nil
		}
		d, err := digestFile(filepath.Join(root, name))
		if err != nil {
			return m, false, err
		}
		m = append(m, Artifact{Name: name, Digest: d})
	}
	return m, true, nil
}

// digestFile is the sha256 of one file's bytes, prefixed the way every
// Artifact.Digest is.
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// harborRunner is dawn's one runner. It is registered as THE dispatcher at
// package init because there is exactly one — a second implementation would be
// a second answer to "what does a trial mean", and the six states are only
// trustworthy while that answer has one author.
type harborRunner struct{}

func init() { dispatcher = harborRunner{} }

// Dispatch runs one trial and classifies it. The attempt clock arrives on the
// ctx — the scope decided it — and is also what the task's [agent] timeout_sec
// is written from, so Harbor stops the agent at the same instant dawn would.
func (harborRunner) Dispatch(ctx context.Context, s Stage, evidence string) (Result, error) {
	d, ok := ctx.Deadline()
	if !ok {
		return Result{}, fmt.Errorf("dawn: stage %s dispatched with no attempt clock", s.ID)
	}
	t, err := runTrial(ctx, s, time.Until(d), evidence)
	if err != nil {
		// dawn failed to obtain a verdict at all. The scope turns this into
		// InfraError and retries it against its own counter; saying so here
		// as well would be a second opinion on the same fact.
		return Result{}, err
	}
	return classify(s, t), nil
}

// classify is the whole of dawn's state assignment: the five rules in the
// State doc, by first match, over the facts of the trial and over no line of
// agent output. It is a pure function of a Stage and a trial precisely so that
// the rules can be read in one place and tested without Docker.
func classify(s Stage, t trial) Result {
	r := Result{Manifest: t.Outputs, metrics: t.Rewards, publishDir: t.PublishDir, drew: t.Drew}
	gated := s.Gate.image != ""
	switch {
	// Rule 1: cancelled from outside the run. Checked first, ahead of every
	// other rule, because a trial the run itself asked to stop never reads as
	// a verdict of any kind — not a timeout, not a missing output, not a gate
	// that happened to still have voted.
	case t.Cancelled:
		r.State = Cancelled
	// Rule 2: a declared output that is missing or could not be collected is
	// infra_error, always — dawn cannot tell a bad attempt from a broken
	// collection, and guessing in the agent's favour is how a forged verdict
	// gets in. An empty output directory is present and valid.
	case !t.Present:
		r.State = InfraError
	// Harbor itself raised: whatever the trial was, it was not a verdict.
	case t.Fault != "":
		r.State = InfraError
	// Rule 3: the gate ran and wrote no valid reward. A crashed gate emits no
	// reward.json, and a non-numeric one Harbor rejects before dawn sees it.
	case gated && !t.Rewarded:
		r.State = InfraError
	// Rule 4: dawn's own clock ended it. Not the agent's doing and not a
	// gate's, so it is never Rejected.
	case t.TimedOut:
		r.State = Exhausted
	// Rule 5, first half: nothing established a verdict. A format-only gate is
	// CLAMPED here — it is above the reward branch, so there is no code path
	// from a format-only stage to Passed, whatever number the gate wrote.
	case !gated || s.Gate.formatOnly:
		r.State = Unverified
	case t.Rewards["reward"] > 0:
		r.State = Passed
	default:
		r.State = Rejected
	}
	return r
}
