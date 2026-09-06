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
	"sync"
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
// agent was told to write it; it runs as /tests/test.sh, with nothing else of
// the agent's; it publishes to /logs/verifier/publish/<name>; and it writes
// {"reward": n} to /logs/verifier/reward.json as its LAST act, never skipping
// the write because a file is already there (Harbor restores the agent's
// artifacts before the gate runs; the last writer wins).
//
// A gate is no-network by default, so its verdict is a function of pinned
// bytes alone. LiveGate is the one exception: it reaches its declared hosts,
// and its verdict is then a function of pinned bytes AND those hosts' state at
// the moment it ran — see LiveGate's own comment for what that costs.
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
// image proves it at build time by running its own selftest, and dawn runs
// that same selftest again before it will spend an agent on the gate (see
// gateSelftest). Four ways, and the fourth is the one that was missing:
//
//	against nothing                  reward 0
//	against a planted oracle         reward 1
//	with its own environment broken  NO reward.json, so infra_error
//	against a planted FORGERY        reward 0
//
// A forgery is an artifact that would score 1 while accomplishing nothing —
// not malformed input, which the first case already covers, but input the gate
// ACCEPTS for a reason that is not the reason it is meant to accept. The first
// three cases were the whole contract for months and two gates passed them
// while being forgeable: pr-ci accepted `import sys; sys.exit(0)` prepended to
// the file under test, which ended the measuring process 0 with no test run,
// and vdh-adyen accepted a 200 from a storefront homepage paired with a 401
// from an unrelated API. Both are now permanent selftest cases in their
// images, so a gate that stops discriminating fails its build rather than
// shipping (experiments/harbor-targets/*/gate).
//
// Nothing here can establish that an author's forgery case is a GOOD one; that
// is judgment, and SoundGate states the rule it should be judged against.
const outputDir = "/app/outputs"

// inputDir is where a stage finds the artifacts of the stages it declared in
// Stage.Inputs, and it is the other half of outputDir's convention: outputs
// leave at a path dawn fixed, inputs arrive at one.
//
// They arrive BAKED, not mounted. Harbor gives a task one environment, and
// that environment is either a prebuilt docker_image or a Dockerfile in the
// task directory it builds (environments/definition.py:
// "Set [environment].docker_image or add environment/Dockerfile"). dawn
// already owns the whole task directory, so a stage with inputs gets a
// generated two-line Dockerfile — FROM the stage's own pinned Env, COPY the
// inputs — and no docker_image key at all, because a prebuilt image WINS over
// a Dockerfile unless the build is forced (should_use_prebuilt_docker_image).
// The pin is not lost by this: it moves into the FROM line.
//
// Safety rests on a fact Stage.Outputs already states: the names copied here
// are the DECLARED names from Go source, never a string the agent chose, so a
// name cannot become a path an agent controls inside a container it never
// runs in. cleanName re-checks it rather than trusting the sentence.
const inputDir = "/app/inputs"

// gateSelftest is where a gate proves itself, and dawn runs it before it will
// spend an agent on that gate. It is a path, not a flag: a gate that claims
// soundness and ships no proof of it does not run at all.
//
// The order is the point. A gate is only discovered to be broken AFTER the
// agent has run — the expensive half first, then the verdict — so a bad gate
// has always cost a whole attempt to find (the README measures one at ~140k
// tokens). Running the gate's own selftest first turns that into a failure
// that costs nothing, before any agent is dispatched.
//
// It runs with NO NETWORK, which is not a restriction the gates had to be bent
// around: all three write their selftests to be provable offline, and the live
// one says so in its own header — every case it checks is refused before any
// request is made, and it states plainly that its live half is proven by a
// recorded run rather than by every build.
const gateSelftest = "/gate/selftest.sh"

// proven memoises the selftest per image, because the image is pinned by
// digest: the same bytes cannot prove themselves twice differently, and a
// search that samples ten times should pay for this once.
var (
	provenMu sync.Mutex
	proven   = map[Image]error{}
)

// proveGate runs the gate's own selftest in the pinned gate image and requires
// it to exit 0. A gate that claims nothing (NoGate, FormatOnlyGate) proves
// nothing: dawn already refuses to let those reach Passed, so there is no
// verdict for a selftest to protect.
//
// The transcript is written beside the attempt that paid for it. Later
// attempts on the same image reuse the answer and write no transcript, which
// is why only the first attempt's evidence carries one.
func proveGate(ctx context.Context, g Gate, evidence string) error {
	if k := g.kind(); k != "sound" && k != "live" {
		return nil
	}
	provenMu.Lock()
	defer provenMu.Unlock()
	if err, ok := proven[g.image]; ok {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=none",
		string(g.image), gateSelftest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("dawn: gate %s did not prove itself (%s %s): %w\n%s",
			g.image, gateSelftest, "exited non-zero", err, out)
	}
	if mkErr := os.MkdirAll(evidence, 0o755); mkErr == nil {
		os.WriteFile(filepath.Join(evidence, "gate-selftest.log"), out, 0o644)
	}
	proven[g.image] = err
	return err
}

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

// harborArgs builds runTrial's argv — pulled out to a named function, the
// same move taskDirFor already makes, so the exact flags dawn passes are
// testable without invoking Harbor.
//
// model and effort are set as CLI flags (-m, --ak reasoning_effort=), never
// as cmd.Env: exec.Cmd.Env non-nil means ONLY that environment, which would
// strip PATH, HOME, DOCKER_HOST and — the real trap — CLAUDE_CODE_OAUTH_TOKEN
// and CLAUDE_FORCE_OAUTH, which dawn relies on inheriting from its own
// process environment. Harbor's -m flag also makes Harbor record the model in
// its own result.json (config.agent.model_name), which an env var handed to
// the child process would not. Do not "improve" this into cmd.Env.
func harborArgs(s Stage, taskDir, jobsDir string) []string {
	args := []string{"run", "-p", taskDir, "-a", s.Agent.name, "-o", jobsDir,
		"--job-name", "trial", "-n", "1", "-q", "-y"}
	if s.Gate.image == "" {
		args = append(args, "--disable-verification")
	}
	if s.Agent.model != "" {
		args = append(args, "-m", s.Agent.model)
	}
	if s.Agent.effort != "" {
		args = append(args, "--ak", "reasoning_effort="+s.Agent.effort)
	}
	return args
}

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
	// ArtifactsDir is where Harbor left those collected outputs on the host.
	// It is what makes Stage.Inputs possible at all: a later stage's build
	// context is filled by copying the DECLARED names out of this directory
	// (materialiseInputs). Set by read() beside Outputs, from the same
	// collected tree, so it is non-empty exactly when Present is true.
	ArtifactsDir string
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
func writeTask(ctx context.Context, dir string, s Stage, attempt time.Duration) error {
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
	if err := materialiseInputs(dir, s); err != nil {
		return err
	}
	gateImage, err := deriveGate(ctx, dir, s)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "instruction.md"), []byte(instruction(s)), 0o644); err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = \"1.4\"\n")
	fmt.Fprintf(&b, "artifacts = [%q]\n\n", outputDir)
	// The description keeps the id verbatim — nothing parses it — and the name
	// is built to Harbor's grammar rather than hoped into it (see taskName).
	fmt.Fprintf(&b, "[task]\nname = %q\nversion = \"1.0.0\"\ndescription = %q\n\n",
		taskName(s.ID), "dawn stage "+s.ID)

	// The environment's baseline is no-network; only the agent phase opens, and
	// only onto two hosts. A stage with no inputs names its pinned image
	// directly and Harbor pulls it. A stage WITH inputs names none, because a
	// docker_image would win over the Dockerfile materialiseInputs just wrote;
	// the pin lives in that Dockerfile's FROM instead.
	if len(s.Inputs) > 0 {
		fmt.Fprintf(&b, "[environment]\nnetwork_mode = \"no-network\"\nos = \"linux\"\n\n")
	} else {
		fmt.Fprintf(&b, "[environment]\ndocker_image = %q\nnetwork_mode = \"no-network\"\nos = \"linux\"\n\n", s.Env)
	}
	fmt.Fprintf(&b, "[agent]\nnetwork_mode = \"allowlist\"\nallowed_hosts = [%s]\ntimeout_sec = %.1f\n\n",
		quoted(agentAllowedHosts), attempt.Seconds())

	if gated {
		// separate: the gate never sees the agent's filesystem, only the
		// artifacts Harbor restores into its own container. A LiveGate opens
		// onto its declared hosts and nothing else; every other gate stays
		// no-network, so its verdict is a function of bytes alone.
		if len(s.Gate.hosts) > 0 {
			fmt.Fprintf(&b, "[verifier]\nenvironment_mode = \"separate\"\nnetwork_mode = \"allowlist\"\nallowed_hosts = [%s]\ntimeout_sec = %.1f\n\n", quoted(s.Gate.hosts), gateTimeout.Seconds())
			fmt.Fprintf(&b, "[verifier.environment]\ndocker_image = %q\nnetwork_mode = \"allowlist\"\nallowed_hosts = [%s]\n", gateImage, quoted(s.Gate.hosts))
		} else {
			fmt.Fprintf(&b, "[verifier]\nenvironment_mode = \"separate\"\nnetwork_mode = \"no-network\"\ntimeout_sec = %.1f\n\n", gateTimeout.Seconds())
			fmt.Fprintf(&b, "[verifier.environment]\ndocker_image = %q\nnetwork_mode = \"no-network\"\n", gateImage)
		}
	}
	return os.WriteFile(filepath.Join(dir, "task.toml"), []byte(b.String()), 0o644)
}

// cleanName rejects any declared output name that is not a plain relative
// path. Stage.Outputs is written in Go source, so this cannot fire on an
// agent's choosing — but the names become paths in a build context here, and
// a rule that holds only because of a sentence in a doc comment is the exact
// thing three of dawn's gates were broken by.
func cleanName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || filepath.Clean(name) != name ||
		strings.HasPrefix(filepath.Clean(name), "..") {
		return fmt.Errorf("dawn: %q is not a plain relative output name", name)
	}
	return nil
}

// materialiseInputs copies each declared input's artifacts into the task's
// build context and writes the Dockerfile that bakes them in. A stage with no
// inputs writes nothing and keeps naming its prebuilt image.
//
// Inputs arrive at inputDir/<i>/<name>, indexed by position in Stage.Inputs
// rather than by stage id, because attemptID already hashes that list IN ORDER
// (inputDigest) and order is the thing a protocol chose deliberately.
func materialiseInputs(dir string, s Stage) error {
	if len(s.Inputs) == 0 {
		return nil
	}
	if err := copyInputs(filepath.Join(dir, "environment", "inputs"), s); err != nil {
		return err
	}
	df := fmt.Sprintf("# Generated by dawn: this stage declared Stage.Inputs.\n"+
		"# The pin is here rather than in task.toml's [environment] on purpose —\n"+
		"# see inputDir. Base bytes are the stage's own Env, unchanged.\n"+
		"FROM %s\nCOPY inputs %s\n", s.Env, inputDir)
	return os.WriteFile(filepath.Join(dir, "environment", "Dockerfile"), []byte(df), 0o644)
}

// copyInputs fills one build context with the declared artifacts of every
// input, at <root>/<i>/<name>.
func copyInputs(root string, s Stage) error {
	for i, in := range s.Inputs {
		if in.artifactsDir == "" {
			return fmt.Errorf("dawn: stage %s: input %d handed back no artifacts to mount", s.ID, i)
		}
		for _, a := range in.Manifest {
			if err := cleanName(a.Name); err != nil {
				return err
			}
			dst := filepath.Join(root, fmt.Sprint(i), a.Name)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			b, err := os.ReadFile(filepath.Join(in.artifactsDir, a.Name))
			if err != nil {
				return fmt.Errorf("dawn: stage %s: input %d: %w", s.ID, i, err)
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// deriveGate gives the VERIFIER the same inputs the agent got, and returns the
// image to run it in.
//
// It exists because of a measured failure: Harbor carries only the agent's
// declared ARTIFACTS into the verifier container, so a gate saw /app/outputs
// and no /app/inputs at all. Every gate that judges an agent's work AGAINST
// WHAT IT WAS GIVEN — did this adversary's verdict concern the finding it was
// handed, did this dedupe drop something it was given, does this trace cover
// the findings it was given — could not run. The first protocol that needed
// it lost every one of its validate attempts to infra_error.
//
// The copy comes from dawn's own collected artifacts on the host, NOT from the
// agent's container, and that is the whole soundness argument: the agent can
// write anywhere in its own filesystem, so inputs collected back out of it
// would be inputs it could have edited to match its answer. These bytes it
// never touched.
//
// The gate's own logic stays pinned — it is the FROM line — exactly as the
// agent environment's pin moves into its Dockerfile. The derived tag is a pure
// function of the pinned gate and the input digests, so the same inputs
// against the same gate name the same image.
func deriveGate(ctx context.Context, dir string, s Stage) (Image, error) {
	tag, root, err := gateContext(dir, s)
	if err != nil || root == "" {
		return tag, err
	}
	cmd := exec.CommandContext(ctx, "docker", "build", "-q", "-t", string(tag), root)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("dawn: stage %s: building the input-carrying gate: %w\n%s", s.ID, err, out)
	}
	return tag, nil
}

// gateContext writes the derived gate's build context and returns the tag it
// will carry plus that context's path. Split out from the build so the part
// that decides WHAT the gate will see is testable without Docker — the part
// that was silently wrong until a live run lost every validate attempt to it.
// A stage with no inputs needs no derived gate: root comes back empty.
func gateContext(dir string, s Stage) (tag Image, root string, err error) {
	if len(s.Inputs) == 0 || s.Gate.image == "" {
		return s.Gate.image, "", nil
	}
	key := struct {
		Gate   Image
		Inputs []Manifest
	}{Gate: s.Gate.image}
	for _, in := range s.Inputs {
		key.Inputs = append(key.Inputs, in.Manifest)
	}
	b, _ := json.Marshal(key)
	tag = Image("dawn-gate-derived:" + sha256hex(b)[:16])

	root = filepath.Join(dir, "verifier")
	if err := copyInputs(filepath.Join(root, "inputs"), s); err != nil {
		return "", "", err
	}
	df := fmt.Sprintf("# Generated by dawn: this stage declared Stage.Inputs, and a\n"+
		"# gate cannot judge work against inputs it cannot see. Harbor carries only\n"+
		"# the agent's declared artifacts into the verifier, so dawn bakes the same\n"+
		"# inputs here — from its OWN copy, which the agent never touched.\n"+
		"FROM %s\nCOPY inputs %s\n", s.Gate.image, inputDir)
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(df), 0o644); err != nil {
		return "", "", err
	}
	return tag, root, nil
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
	// Inputs come first: they are context the agent needs before it reads what
	// to hand back. dawn names the paths rather than the prompt doing it, for
	// the same reason it owns the output paths — the author writes the task,
	// dawn owns where bytes live.
	if len(s.Inputs) > 0 {
		fmt.Fprintf(&b, "Earlier stages of this run handed you %d input(s), "+
			"baked into this container read-only:\n\n", len(s.Inputs))
		for i, in := range s.Inputs {
			for _, a := range in.Manifest {
				fmt.Fprintf(&b, "  - `%s/%d/%s`\n", inputDir, i, a.Name)
			}
		}
		b.WriteString("\n")
	}
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
	if err := writeTask(ctx, taskDir, s, attempt); err != nil {
		return t, err
	}

	args := harborArgs(s, taskDir, filepath.Join(dir, "jobs"))

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
		collected := filepath.Join(trialDir, e.Destination)
		m, ok, err := digest(collected, outputs)
		if err != nil {
			return err
		}
		t.Outputs, t.Present, t.ArtifactsDir = m, ok, collected
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
	// Before the expensive half. A gate that cannot prove itself never gets an
	// agent run spent on it.
	if err := proveGate(ctx, s.Gate, evidence); err != nil {
		return Result{}, err
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
	r := Result{Manifest: t.Outputs, metrics: t.Rewards, publishDir: t.PublishDir,
		artifactsDir: t.ArtifactsDir, drew: t.Drew}
	gated := s.Gate.image != ""
	switch {
	// Rule 1: cancelled from outside the run. Checked first, ahead of every
	// other rule, because a trial the run itself asked to stop never reads as
	// a verdict of any kind — not a timeout, not a missing output, not a gate
	// that happened to still have voted.
	case t.Cancelled:
		r.State = Cancelled
	// Rule 2: dawn's own clock ended it.
	//
	// Second, with Cancelled, and ahead of everything below — because these
	// two are the only facts in this switch dawn owns DIRECTLY. Both are
	// ctx.Err() (see clockOutcome), mutually exclusive, and observed rather
	// than inferred. Every rule below is read out of the wreckage dawn's own
	// SIGTERM left behind, and a killed attempt is guaranteed to leave some:
	// the agent dies mid-work so its declared output is missing, and Harbor
	// finalizes with a CancelledError that arrives here as t.Fault. Asking
	// those questions first means dawn's answer to "why did this attempt end"
	// is derived from the mess it made rather than from the fact it already
	// had.
	//
	// This ordering was the other way round, and the consequence was measured
	// rather than argued: across every run ever made against this package,
	// twenty-one attempts, Exhausted was assigned ZERO times. It was not rare,
	// it was unreachable — one of six states that classify could never emit,
	// and the one naming the thing that actually happened. A real 20-minute
	// timeout on a real target was filed as infra_error, which is retryable,
	// so dawn re-dispatched into the same wall: 5.1M tokens burned, then 3.9M
	// more, for a verdict no attempt could have produced.
	//
	// The rule this replaces claimed missing-output had to come first or a
	// ~190s credential failure would be misfiled as a real result. It does
	// not follow. That failure never sets TimedOut — the clock did not fire —
	// so it falls straight through to the missing-output rule below, exactly
	// as it always did.
	//
	// Nothing else changes to stop the retry: dispatchAttempt retries on
	// InfraError alone, so an attempt that is Exhausted is simply returned.
	case t.TimedOut:
		r.State = Exhausted
	// Rule 3: a declared output that is missing or could not be collected is
	// infra_error, always — dawn cannot tell a bad attempt from a broken
	// collection, and guessing in the agent's favour is how a forged verdict
	// gets in. An empty output directory is present and valid.
	case !t.Present:
		r.State = InfraError
	// Harbor itself raised: whatever the trial was, it was not a verdict.
	case t.Fault != "":
		r.State = InfraError
	// Rule 4: the gate ran and wrote no valid reward. A crashed gate emits no
	// reward.json, and a non-numeric one Harbor rejects before dawn sees it.
	case gated && !t.Rewarded:
		r.State = InfraError
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

// taskName builds the Harbor task name for a stage. The id already satisfies
// Harbor's own segment grammar (stageID, runtime.go — refused at dispatch
// otherwise), so it is used verbatim; the "dawn/" prefix exists only because
// Harbor's grammar wants two segments, not because names must be distinct —
// two stages that land on one name are still two evidence directories and
// two attempt_ids.
func taskName(id string) string { return "dawn/" + id }
