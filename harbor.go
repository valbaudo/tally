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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
const outputDir = "/app/outputs"

// The agent phase reaches these two hosts and no others. Verified by grepping
// the pinned CLI binary: everything else it contacts degrades quietly.
var agentAllowedHosts = []string{"api.anthropic.com", "platform.claude.com"}

// gateTimeout bounds the verifier, which is dawn's own program on dawn's own
// pinned image. It is not the attempt clock and not the author's business.
const gateTimeout = 10 * time.Minute

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
	// Outputs is dawn's declared artifacts list as it came back: the files
	// under outputDir, named and digested. Present is false when the entry was
	// missing or could not be collected, which is always infra_error.
	Outputs Manifest
	Present bool
	// TimedOut reports that dawn's own clock ended the attempt.
	TimedOut bool
	// Fault is Harbor's exception for the trial, empty when there was none.
	Fault string
	// What the attempt drew. Zero for agents that report nothing (oracle, nop).
	InputTokens, CacheTokens, OutputTokens int
	CostUSD                                float64
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
	fmt.Fprintf(&b, "[task]\nname = %q\nversion = \"1.0.0\"\ndescription = %q\n\n",
		"dawn/"+s.ID, "dawn stage "+s.ID)

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
// author state: where the handover goes. dawn owns the artifacts list, so dawn
// and not the protocol tells the agent the path it must write to.
func instruction(s Stage) string {
	return s.Prompt + fmt.Sprintf(`

---

Hand your work over by writing files into `+"`%s`"+`. That directory is the
only thing collected from this container; nothing else you do here is looked
at, and nothing outside it crosses the boundary.
`, outputDir)
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
	taskDir := filepath.Join(dir, "task")
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
	runErr := cmd.Run()
	t.TimedOut = ctx.Err() != nil

	// A non-zero harbor is not itself an error: the trial may still have
	// produced a result, and a missing result is what "no verdict" looks like.
	if err := t.read(filepath.Join(dir, "jobs")); err != nil {
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

func (t *trial) read(jobsDir string) error {
	hits, _ := filepath.Glob(filepath.Join(jobsDir, "*", "*", "result.json"))
	if len(hits) != 1 {
		return fmt.Errorf("dawn: expected one trial result under %s, found %d", jobsDir, len(hits))
	}
	trialDir := filepath.Dir(hits[0])

	var r harborResult
	if err := readJSON(hits[0], &r); err != nil {
		return err
	}
	if a := r.AgentResult; a != nil {
		t.InputTokens, t.CacheTokens, t.OutputTokens, t.CostUSD =
			a.NInputTokens, a.NCacheTokens, a.NOutputTokens, a.CostUSD
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
		t.Present = e.Status == "ok" || e.Status == "empty"
		t.Outputs, _ = digest(filepath.Join(trialDir, e.Destination))
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

// digest turns the collected output tree into a Manifest: one entry per file,
// named by its path under outputDir, digested by its bytes. The digests are
// the only thing a protocol can compare across attempts to learn whether
// anything actually changed.
func digest(root string) (Manifest, error) {
	var m Manifest
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		name, _ := filepath.Rel(root, p)
		m = append(m, Artifact{Name: filepath.ToSlash(name), Digest: "sha256:" + hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return m, err
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
	r := Result{Manifest: t.Outputs, metrics: t.Rewards}
	gated := s.Gate.image != ""
	switch {
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
