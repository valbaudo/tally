package dawn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The settled rules are only true if the compiler is the one place a task.toml
// is written, so this asserts them on the bytes it writes.
func TestWriteTaskCarriesTheSettledRules(t *testing.T) {
	dir := t.TempDir()
	s := Stage{
		ID:     "rules",
		Agent:  Agent{name: "oracle"},
		Env:    Image("rc-env@sha256:" + strings.Repeat("a", 64)),
		Prompt: "do the thing",
		Gate:   SoundGate(Image("rc-gate@sha256:" + strings.Repeat("b", 64))),
	}
	if err := writeTask(context.Background(), dir, s, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`artifacts = ["/app/outputs"]`,
		`environment_mode = "separate"`,
		`docker_image = "rc-gate@sha256:`,
		`docker_image = "rc-env@sha256:`,
		`allowed_hosts = ["api.anthropic.com", "platform.claude.com"]`,
		`network_mode = "allowlist"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("task.toml missing %q\n%s", want, got)
		}
	}
	if strings.Count(got, `network_mode = "no-network"`) != 3 {
		t.Errorf("environment, verifier and verifier.environment must all be no-network\n%s", got)
	}
	// The measured verdict-forgery vector: an artifact restored under the
	// verifier's log tree lands there before the gate runs.
	if strings.Contains(got, "/logs/verifier") {
		t.Errorf("task names a path under /logs/verifier\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "environment")); err != nil {
		t.Errorf("harbor requires environment/ to exist: %v", err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "instruction.md")), outputDir) {
		t.Error("instruction must tell the agent where the handover goes")
	}
}

// A LiveGate must open the verifier phase onto its declared hosts in BOTH
// [verifier] and [verifier.environment] — Harbor enforces egress per-process,
// so allowing only one half would still let the gate's own container phone
// out unchecked, or would block the verifier process itself from reaching the
// hosts its own environment is allowed to reach. [environment] and [agent]
// are untouched: the engagement scope is the gate's alone.
func TestWriteTaskOnLiveGateOpensBothVerifierPhases(t *testing.T) {
	dir := t.TempDir()
	s := Stage{
		ID:     "exploit",
		Agent:  Agent{name: "oracle"},
		Env:    Image("rc-env@sha256:" + strings.Repeat("a", 64)),
		Prompt: "prove it",
		Gate:   LiveGate(Image("rc-gate@sha256:"+strings.Repeat("b", 64)), "target.example.com", "10.0.0.5"),
	}
	if err := writeTask(context.Background(), dir, s, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, "task.toml"))

	verifier, verifierEnv := splitAtVerifierEnvironment(t, got)
	for _, section := range []struct{ name, body string }{{"[verifier]", verifier}, {"[verifier.environment]", verifierEnv}} {
		if !strings.Contains(section.body, `network_mode = "allowlist"`) {
			t.Errorf("%s missing allowlist network_mode\n%s", section.name, got)
		}
		if !strings.Contains(section.body, `allowed_hosts = ["target.example.com", "10.0.0.5"]`) {
			t.Errorf("%s missing the declared hosts\n%s", section.name, got)
		}
	}
	if !strings.Contains(got, `[environment]`+"\n"+`docker_image = "rc-env@sha256:`) || !strings.Contains(got, `network_mode = "no-network"`+"\n"+`os = "linux"`) {
		t.Errorf("[environment] must stay no-network for a live gate\n%s", got)
	}
	if !strings.Contains(got, `[agent]`+"\n"+`network_mode = "allowlist"`+"\n"+`allowed_hosts = ["api.anthropic.com", "platform.claude.com"]`) {
		t.Errorf("[agent] must be unchanged by a live gate\n%s", got)
	}
	if strings.Contains(got, "/logs/verifier") {
		t.Errorf("task names a path under /logs/verifier\n%s", got)
	}
}

// splitAtVerifierEnvironment splits task.toml's text at the [verifier] and
// [verifier.environment] table headers so a test can assert on each section
// without a substring match from one leaking into the other (both sections
// share every key name: network_mode, allowed_hosts).
func splitAtVerifierEnvironment(t *testing.T, taskToml string) (verifier, verifierEnv string) {
	t.Helper()
	i := strings.Index(taskToml, "[verifier]\n")
	j := strings.Index(taskToml, "[verifier.environment]\n")
	if i == -1 || j == -1 || j < i {
		t.Fatalf("task.toml missing [verifier]/[verifier.environment] in order\n%s", taskToml)
	}
	return taskToml[i:j], taskToml[j:]
}

func TestWriteTaskRejectsUnpinnedAndUnexplained(t *testing.T) {
	pinned := Image("x@sha256:" + strings.Repeat("c", 64))
	for name, s := range map[string]Stage{
		"unpinned env":  {ID: "a", Env: "rc-env:1", Gate: SoundGate(pinned)},
		"unpinned gate": {ID: "a", Env: pinned, Gate: SoundGate("rc-gate:1")},
		"silent nogate": {ID: "a", Env: pinned, Gate: NoGate("")},
		"no id":         {ID: "", Env: pinned, Gate: SoundGate(pinned)},
	} {
		if err := writeTask(context.Background(), t.TempDir(), s, time.Minute); err == nil {
			t.Errorf("%s: accepted at dispatch", name)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The live path. Needs harbor, docker and the rc-pr-ci images; opt in with
// DAWN_HARBOR_E2E=1. nop hands nothing over, so the gate's honest verdict is
// zero and dawn's declared output is absent — the two facts a classifier
// most needs to be able to tell apart.
func TestRunTrialAgainstAGeneratedTask(t *testing.T) {
	if os.Getenv("DAWN_HARBOR_E2E") != "1" {
		t.Skip("set DAWN_HARBOR_E2E=1 (needs harbor, docker, rc-pr-ci images)")
	}
	got, err := runTrial(context.Background(), Stage{
		ID:     "rc-pr-ci",
		Agent:  Agent{name: "nop"},
		Env:    Image(os.Getenv("DAWN_E2E_ENV")),
		Prompt: "Fix calc.py in /app/repo and write the diff as fix.patch.",
		Gate:   SoundGate(Image(os.Getenv("DAWN_E2E_GATE"))),
	}, 10*time.Minute, t.TempDir())
	if err != nil {
		t.Fatalf("%v (see %s/harbor.log)", err, got.Dir)
	}
	if !got.Rewarded || got.Rewards["reward"] != 0 {
		t.Errorf("gate should have written reward 0, got %v rewarded=%v", got.Rewards, got.Rewarded)
	}
	if len(got.Outputs) != 0 {
		t.Errorf("nop hands nothing over, got %v", got.Outputs)
	}
	t.Logf("dir=%s rewards=%v present=%v outputs=%v", got.Dir, got.Rewards, got.Present, got.Outputs)
}

// TestFanOverlapsRealHarborTrials is Q2 proven for real, not against a fake:
// four real Harbor trials against the shipped pr-ci target, using Harbor's
// "nop" agent — the same agent TestRunTrialAgainstAGeneratedTask already
// uses, so it costs nothing and needs no token, yet every container, every
// environment build and every gate run is genuine.
//
// NOT "oracle": Harbor's OracleAgent hardcodes its solution script's path as
// <task_dir>/solution/solve.sh (harbor/models/task/paths.py) — a HOST-side
// file dawn's compiler has no way to produce, because Stage has no
// "solution" field and never will (dawn runs real agents against real
// unknowns; baking an answer key is not in its vocabulary, on purpose). This
// was verified by trying it: harbor fails every trial with "Solution script
// not found" before ever reaching the gate.
//
// nop hands nothing over, so every child's terminal state is InfraError
// (Rule 2: a declared output that was never collected) rather than Passed —
// but that is still a REAL trial with a REAL environment build and a REAL
// gate run, and the state is irrelevant to what this test proves: overlap in
// wall time, and correct placement by index.
//
// DAWN_MAX_CONCURRENT pins the width to exactly 4 so the comparison below
// does not depend on this host's own memory or core count. Opt in with
// DAWN_HARBOR_E2E=1, same images as TestRunTrialAgainstAGeneratedTask.
func TestFanOverlapsRealHarborTrials(t *testing.T) {
	if os.Getenv("DAWN_HARBOR_E2E") != "1" {
		t.Skip("set DAWN_HARBOR_E2E=1 (needs harbor, docker, rc-pr-ci images)")
	}
	// Opting into the e2e path and then leaving the images unset used to pass
	// in 0.18s: both "trials" failed instantly, and "four fanned < 4x one"
	// is trivially true of two instant failures. A test that passes when its
	// subject never ran is worse than no test, so this fails rather than
	// skips — the caller asked for the real thing.
	if os.Getenv("DAWN_E2E_ENV") == "" || os.Getenv("DAWN_E2E_GATE") == "" {
		t.Fatal("DAWN_HARBOR_E2E=1 requires DAWN_E2E_ENV and DAWN_E2E_GATE (digest-pinned images)")
	}
	t.Setenv("DAWN_MAX_CONCURRENT", "4")

	env := Image(os.Getenv("DAWN_E2E_ENV"))
	gate := Image(os.Getenv("DAWN_E2E_GATE"))
	stage := func(id string) Stage {
		return Stage{
			ID:      id,
			Agent:   Agent{name: "nop", FanOut: true},
			Env:     env,
			Prompt:  "Fix calc.py in /app/repo and write the diff as fix.patch.",
			Outputs: []string{"fix.patch"},
			Gate:    SoundGate(gate),
		}
	}
	mk := func(i int) Stage { return stage(fmt.Sprintf("rc-pr-ci-fan-%d", i)) }

	r := &run{dir: t.TempDir(), ctx: context.Background(), dispatch: dispatcher, sleep: time.Sleep, values: map[string]any{}, actuations: map[string]string{}}

	// Baseline: one real trial, timed alone, on its own stage id so its
	// evidence directory never collides with a fan child's.
	start := time.Now()
	baseline := r.root(Dispatching(1, 10*time.Minute)).Run(stage("rc-pr-ci-baseline"))
	oneTrial := time.Since(start)
	// A real containerised trial takes tens of seconds. Anything near-instant
	// means harbor refused before starting one, and the overlap ratio below
	// would then be comparing two failures to each other.
	if oneTrial < 5*time.Second {
		t.Fatalf("baseline trial took %v: harbor did not run a real container, so the overlap measurement is meaningless", oneTrial)
	}
	if baseline.State != InfraError {
		t.Fatalf("baseline nop trial: state = %s, want infra_error (nop hands nothing back)", baseline.State)
	}

	// Four, fanned. Attempts == n exactly: every child spends its one charge
	// before any of them finishes, so nobody retries mid-fan regardless of
	// nop's InfraError outcome (see dispatchAttempt: a retry only happens
	// when the scope still admits one, and this scope is fully spent the
	// instant all four children have charged).
	start = time.Now()
	results := r.root(Dispatching(4, 10*time.Minute)).Fan(4, mk)
	fanTime := time.Since(start)

	for i, res := range results {
		if res.State != InfraError {
			t.Errorf("child %d: state = %s, want infra_error", i, res.State)
			continue
		}
		if want := attemptID(mk(i), 1); res.attemptID != want {
			t.Errorf("child %d: attemptID = %s, want %s — index order broken", i, res.attemptID, want)
		}
	}

	ratio := float64(fanTime) / float64(oneTrial)
	t.Logf("one trial = %v, four fanned = %v (%.2fx)", oneTrial, fanTime, ratio)
	if fanTime > oneTrial*3 {
		t.Errorf("fan of 4 took %v (%.2fx a single trial's %v), want well under 4x: no real overlap happened", fanTime, ratio, oneTrial)
	}
}

// The classifier is the whole of dawn's state assignment, so it is asserted as
// a table on facts rather than through Docker. The clamp is the case that
// matters most: a format-only gate that writes reward 1 is still Unverified,
// and there is no arrangement of facts that makes it Passed.
func TestClassifyAssignsStatesByFirstMatch(t *testing.T) {
	pinned := Image("x@sha256:" + strings.Repeat("d", 64))
	sound := Stage{Gate: SoundGate(pinned)}
	format := Stage{Gate: FormatOnlyGate(pinned)}
	none := Stage{Gate: NoGate("nothing to verify")}
	won := trial{Present: true, Rewarded: true, Rewards: map[string]float64{"reward": 1}}
	lost := trial{Present: true, Rewarded: true, Rewards: map[string]float64{"reward": 0}}

	for name, c := range map[string]struct {
		s    Stage
		t    trial
		want State
	}{
		"gate said yes":          {sound, won, Passed},
		"gate said no":           {sound, lost, Rejected},
		"output never collected": {sound, trial{Rewarded: true, Rewards: map[string]float64{"reward": 1}}, InfraError},
		"gate wrote no verdict":  {sound, trial{Present: true}, InfraError},
		"harbor raised":          {sound, trial{Present: true, Fault: "ValidationError: nope", Rewarded: true, Rewards: map[string]float64{"reward": 1}}, InfraError},
		// The shape a REAL clock-kill produces, taken from a measured trial
		// (attempts/pov-1/1 of vdh-adyen, 20m20s against a 20m clock): the
		// agent was still working, so its declared output is missing, and
		// Harbor was SIGTERM'd, so it finalized with a CancelledError. The
		// case this replaces hand-built {Present: true, Rewarded: true,
		// Fault: ""} with TimedOut — a trial the runner CANNOT produce, since
		// the thing that sets TimedOut is the same thing that sets Fault and
		// leaves the output unwritten. It passed over an impossible world and
		// hid the bug: Exhausted was assigned zero times in twenty-one real
		// attempts.
		"dawn's clock ended it": {sound, trial{TimedOut: true, Present: false, Rewarded: false, Fault: "CancelledError: "}, Exhausted},
		// The companion, and the reason the reorder is safe: an infra failure
		// with no clock event is still InfraError, still retryable. This is
		// the ~190s credential failure the old ordering claimed to protect —
		// it never sets TimedOut, so it falls through exactly as before.
		"infra failure, clock never fired": {sound, trial{TimedOut: false, Present: false}, InfraError},
		"format-only clamped":              {format, won, Unverified},
		"ungated":                          {none, trial{Present: true}, Unverified},
		// Rule 1 wins over everything, including a trial that otherwise looks
		// like a clean win: cancelled from outside is never a verdict.
		"externally cancelled": {sound, trial{Present: true, Rewarded: true, Rewards: map[string]float64{"reward": 1}, Cancelled: true}, Cancelled},
	} {
		if got := classify(c.s, c.t); got.State != c.want {
			t.Errorf("%s: got %s, want %s", name, got.State, c.want)
		}
	}

	// The reward rides alongside the state as a metric, never as one.
	r := classify(sound, won)
	if v, ok := r.Metric("reward"); !ok || v != 1 {
		t.Errorf("reward metric: %v %v", v, ok)
	}
	if _, ok := r.Metric("nope"); ok {
		t.Error("a missing metric must not read as zero")
	}
	// Decision 1: classify threads the trial's PublishDir onto the Result
	// unconditionally, the same pattern as metrics, so Actuate has a door
	// onto the gate's bytes by the time a Passed Result ever reaches it.
	published := classify(sound, trial{Present: true, Rewarded: true, Rewards: map[string]float64{"reward": 1}, PublishDir: "/some/trial/verifier/publish"})
	if published.publishDir != "/some/trial/verifier/publish" {
		t.Errorf("publishDir = %q, want the trial's PublishDir threaded through", published.publishDir)
	}
}

// This is the test that proves the "no classifier change" claim: a live gate
// is gated && !formatOnly, and classify's Rule 5 already sends that straight
// to Passed for a rewarded, present trial. If this ever needed a new branch
// in classify, this test would be the one to catch it.
func TestClassifyPassesALiveGatedStageOnRewardedPresentTrial(t *testing.T) {
	live := Stage{Gate: LiveGate(Image("x@sha256:"+strings.Repeat("e", 64)), "target.example.com")}
	won := trial{Present: true, Rewarded: true, Rewards: map[string]float64{"reward": 1}}
	if got := classify(live, won); got.State != Passed {
		t.Errorf("live gate, rewarded and present: got %s, want %s", got.State, Passed)
	}
}

// trial.read resolves PublishDir from the SAME trialDir that yields
// result.json — verifier/publish underneath it — without needing Harbor,
// Docker or a manifest.json: this is dawn's own arithmetic on a path, not a
// property of what the trial contained.
func TestTrialReadSetsPublishDirFromTheTrialDir(t *testing.T) {
	jobsDir := t.TempDir()
	trialDir := filepath.Join(jobsDir, "task", "trial")
	if err := os.MkdirAll(trialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trialDir, "result.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	var tr trial
	if err := tr.read(jobsDir, nil); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(trialDir, "verifier", "publish"); tr.PublishDir != want {
		t.Errorf("PublishDir = %q, want %q", tr.PublishDir, want)
	}
}

// digest looks up declared names; it must not pick up a file the agent wrote
// but never declared, however innocuous — an undeclared file is inert.
func TestDigestLooksUpDeclaredNamesOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "declared.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "undeclared.txt"), []byte("smuggled"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, present, err := digest(dir, []string{"declared.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("declared.txt exists and fits the cap: should be present")
	}
	if len(m) != 1 || m[0].Name != "declared.txt" {
		t.Fatalf("manifest = %v, want exactly the one declared name", m)
	}
}

// A declared output that was never written is not present, and infra_error
// (via classify's Rule 2) is the only way it can go — never rejected.
func TestDigestMissingDeclaredNameIsNotPresent(t *testing.T) {
	m, present, err := digest(t.TempDir(), []string{"never-written.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("a missing declared output must not be present")
	}
	if len(m) != 0 {
		t.Fatalf("manifest = %v, want none: a missing output earns no partial manifest", m)
	}
}

// Decision 3: an oversized declared output is not present either — dawn
// refusing its own collection, not a gate voting no — and "never truncate"
// means it is never even partially digested.
func TestDigestOversizedDeclaredNameIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxOutputBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	m, present, err := digest(dir, []string{"huge.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("an over-cap declared output must not be present")
	}
	if len(m) != 0 {
		t.Fatalf("manifest = %v, want none", m)
	}
}

// A file exactly at the cap is not oversized — the boundary is "over", not
// "at or over".
func TestDigestAtTheCapIsPresent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exact.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxOutputBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, present, err := digest(dir, []string{"exact.bin"}); err != nil || !present {
		t.Fatalf("present=%v err=%v, want present at exactly the cap", present, err)
	}
}

// Decision 1: the agent is told the exact filenames, not left to guess. A
// stage with no declared outputs says so instead of naming nothing.
func TestInstructionNamesEachDeclaredOutput(t *testing.T) {
	s := Stage{Prompt: "do the thing", Outputs: []string{"fix.patch", "notes.txt"}}
	got := instruction(s)
	for _, name := range s.Outputs {
		want := outputDir + "/" + name
		if !strings.Contains(got, want) {
			t.Errorf("instruction missing declared path %q\n%s", want, got)
		}
	}

	none := instruction(Stage{Prompt: "do the thing"})
	if strings.Contains(none, outputDir+"/") {
		t.Errorf("a stage with no declared outputs must not name a path under it\n%s", none)
	}
}

// clockOutcome is the pure translation from ctx.Err() dawn relies on to tell
// its own clock apart from an external cancel; classify's Cancelled-first
// rule is only as correct as this mapping.
func TestClockOutcome(t *testing.T) {
	for _, c := range []struct {
		err                error
		timedOut, canceled bool
	}{
		{nil, false, false},
		{context.DeadlineExceeded, true, false},
		{context.Canceled, false, true},
	} {
		gotTimedOut, gotCancelled := clockOutcome(c.err)
		if gotTimedOut != c.timedOut || gotCancelled != c.canceled {
			t.Errorf("clockOutcome(%v) = (%v, %v), want (%v, %v)", c.err, gotTimedOut, gotCancelled, c.timedOut, c.canceled)
		}
	}
}

// Decision 4: cmd.Cancel must send SIGTERM, not exec.CommandContext's default
// SIGKILL — the whole point being that harbor gets to run its own shutdown
// handler. Proven against a real subprocess and a real signal, not a mock.
func TestTerminateGracefullySendsSIGTERMNotSIGKILL(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "term.seen")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c",
		"trap 'touch "+marker+"; exit 0' TERM; sleep 5 & wait $!")
	terminateGracefully(cmd, 2*time.Second)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // let the trap install before cancelling
	cancel()
	cmd.Wait()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("SIGTERM trap did not fire (got SIGKILL instead?): %v", err)
	}
}

// The hard-kill backstop: a process that ignores SIGTERM entirely must still
// be gone by WaitDelay, not left to run forever.
func TestTerminateGracefullyForceKillsAfterWaitDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "trap '' TERM; sleep 30 & wait $!")
	terminateGracefully(cmd, 300*time.Millisecond)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WaitDelay did not force-kill a process ignoring SIGTERM")
	}
}

// runTrial generates its task directory with THIS exact basename, without
// exception, because it is the one thing reap.go's whole reap set depends
// on: Harbor's LocalTaskId.get_name() reads a task directory's basename
// verbatim as the seed for trial_name, so this name — not "task", not
// anything else — is what makes every container dawn creates start with
// reapPrefix. A regression here silently breaks reaping without breaking a
// single Harbor trial.
func TestGeneratedTaskDirNameIsDawn(t *testing.T) {
	if generatedTaskDirName != "dawn" {
		t.Fatalf("generatedTaskDirName = %q, want %q: reap.go's reapPrefix is derived from this constant", generatedTaskDirName, "dawn")
	}
	if want := generatedTaskDirName + "__"; reapPrefix != want {
		t.Fatalf("reapPrefix = %q, want %q", reapPrefix, want)
	}
}

// taskDirFor is the exact function runTrial calls to place the generated
// task — the constant test above proves nothing if this join ever drifts
// from it (a literal "task" snuck back in here, say).
func TestTaskDirForJoinsTheGeneratedTaskDirName(t *testing.T) {
	got := taskDirFor("/run/attempts/x/1")
	want := "/run/attempts/x/1/dawn"
	if got != want {
		t.Fatalf("taskDirFor(%q) = %q, want %q", "/run/attempts/x/1", got, want)
	}
}

// harborArgs is the one place dawn tells Harbor what to pin. ClaudeCode's
// profile carries a model, so its argv must carry -m; a profile with neither
// model nor effort set (the "nop" test agent, same as Codex today) must carry
// neither flag — dawn records that it pinned nothing rather than guess.
func TestHarborArgsSetsModelAndEffortAsFlags(t *testing.T) {
	pinned := Stage{ID: "s", Agent: ClaudeCode, Env: "e@sha256:0", Gate: NoGate("test")}
	got := harborArgs(pinned, "/task", "/jobs")
	if !slices.Contains(got, "-m") || !slices.Contains(got, "claude-sonnet-5") {
		t.Errorf("harborArgs(ClaudeCode) = %v, want -m claude-sonnet-5", got)
	}

	unpinned := Stage{ID: "s", Agent: Agent{name: "nop"}, Env: "e@sha256:0", Gate: NoGate("test")}
	got = harborArgs(unpinned, "/task", "/jobs")
	if slices.Contains(got, "-m") {
		t.Errorf("harborArgs(nop) = %v, contains -m: an unpinned profile must set neither flag", got)
	}
	if slices.Contains(got, "--ak") {
		t.Errorf("harborArgs(nop) = %v, contains --ak: an unpinned profile must set neither flag", got)
	}
}

// taskName no longer sanitises anything — it trusts the id outright (see
// harbor.go) — so the only thing left pinning Stage.ID's grammar (stageID,
// runtime.go) and Harbor's own task-name grammar, verbatim from its
// constants.py,
//
//	^[a-zA-Z0-9][a-zA-Z0-9._-]*/[a-zA-Z0-9][a-zA-Z0-9._-]*$
//
// to the same string is this test. Every candidate stageID accepts must
// produce a taskName Harbor accepts too; a candidate stageID rejects is
// skipped, not asserted on, since taskName never sees an id that never
// reaches it. Loosen stageID to admit a character Harbor's own grammar
// refuses — a space, a leading '.' or '-' — and the candidate that exercises
// it newly passes the filter and fails here, without dispatchAttempt's own
// guard ever running.
func TestTaskNameAlwaysSatisfiesHarborsGrammar(t *testing.T) {
	harborName := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*/[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	candidates := []string{
		"fix", "pov-0", "hunt.sql-injection", "prove_2", "a.b", "s",
		".draft", "-x", "_scratch", "a b", "\u00e9t\u00e9", "s/t/u/v", "",
	}
	tested := 0
	for _, id := range candidates {
		if !stageID.MatchString(id) {
			continue
		}
		tested++
		if got := taskName(id); !harborName.MatchString(got) {
			t.Errorf("taskName(%q) = %q, which Harbor refuses", id, got)
		}
	}
	if tested == 0 {
		t.Fatal("no candidate satisfied stageID — this test is vacuous")
	}
}

// The name is not identity and never has to be distinct — evidence paths and
// attempt_ids carry that — so the prefix taskName may add is free.
func TestTaskNameReachesTheGeneratedTask(t *testing.T) {
	dir := t.TempDir()
	s := Stage{
		ID:     "pov-0",
		Agent:  Agent{name: "oracle"},
		Env:    Image("rc-env@sha256:" + strings.Repeat("a", 64)),
		Prompt: "do the thing",
		Gate:   SoundGate(Image("rc-gate@sha256:" + strings.Repeat("b", 64))),
	}
	if err := writeTask(context.Background(), dir, s, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `name = "dawn/pov-0"`) {
		t.Errorf("generated task did not carry the sanitised name; task.toml:\n%s", b)
	}
}

// A gate that claims nothing proves nothing, and must not reach docker at all.
// The image name is deliberately unpullable: if proveGate dispatched it, the
// call would fail rather than return nil, so this passes for the right reason
// and needs no docker of its own.
func TestProveGateSkipsGatesThatClaimNothing(t *testing.T) {
	proven = map[Image]error{}
	for _, g := range []Gate{
		FormatOnlyGate("dawn-no-such-image-should-never-be-run"),
		NoGate("nothing to check here"),
	} {
		if err := proveGate(context.Background(), g, t.TempDir()); err != nil {
			t.Fatalf("%s gate: proveGate = %v, want nil (it must not run docker)", g.kind(), err)
		}
	}
}

// The live path: a gate proves itself, and a gate that ships no proof is
// refused BEFORE an agent is dispatched, which is the whole point of the
// check. Opt in with DAWN_HARBOR_E2E=1 (needs docker and the built images).
func TestProveGateRefusesAGateThatShipsNoProof(t *testing.T) {
	if os.Getenv("DAWN_HARBOR_E2E") != "1" {
		t.Skip("set DAWN_HARBOR_E2E=1 (needs docker and the built gate images)")
	}
	proven = map[Image]error{}
	dir := t.TempDir()

	// A real gate carries /gate/selftest.sh and passes it.
	real := Image("dawn-pr-ci-gate@sha256:fb9372925196db1af71f3a1349f1e9c3b5c7d1c44621ec83a5d3b5483d117242")
	if err := proveGate(context.Background(), SoundGate(real), dir); err != nil {
		t.Fatalf("the pr-ci gate did not prove itself: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gate-selftest.log")); err != nil {
		t.Fatalf("no transcript beside the attempt that paid for it: %v", err)
	}

	// The gates' own base image: same bytes underneath, no selftest on top.
	bare := Image("python:3.12-bookworm@sha256:581429e3df12d76e6af4be5ab7d0e7fc2013eb57dc23d2de691411c8efdbb970")
	if err := proveGate(context.Background(), SoundGate(bare), t.TempDir()); err == nil {
		t.Fatal("a gate with no /gate/selftest.sh proved itself; SoundGate would mean nothing")
	}

	// Memoised: the answer is a property of pinned bytes, not of the call.
	if _, ok := proven[bare]; !ok {
		t.Fatal("the refusal was not memoised")
	}
}

// Stage.Inputs was documented for months as "the only way anything crosses an
// attempt boundary" while writeTask emitted nothing for it — the surface
// describing behaviour that did not exist. This asserts the behaviour, not the
// sentence: the bytes land in the build context, the Dockerfile bakes them at
// the documented path, and the prebuilt image key is GONE (a docker_image
// would win over the Dockerfile and the inputs would silently vanish).
func TestStageInputsAreBakedIntoTheNextStagesImage(t *testing.T) {
	produced := t.TempDir()
	if err := os.WriteFile(filepath.Join(produced, "finding.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream := Result{
		State:        Passed,
		Manifest:     Manifest{{Name: "finding.json", Digest: "sha256:0"}},
		artifactsDir: produced,
	}
	dir := t.TempDir()
	stage := Stage{
		ID: "prove", Agent: ClaudeCode, Env: Image("e@sha256:" + strings.Repeat("a", 64)),
		Prompt: "prove it", Outputs: []string{"proof.json"},
		Inputs: []Result{upstream}, Gate: NoGate("test"),
	}
	if err := writeTask(context.Background(), dir, stage, time.Minute); err != nil {
		t.Fatalf("writeTask: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "environment", "inputs", "0", "finding.json"))
	if err != nil || string(got) != `{"x":1}` {
		t.Fatalf("input bytes did not reach the build context: %q, %v", got, err)
	}
	df, err := os.ReadFile(filepath.Join(dir, "environment", "Dockerfile"))
	if err != nil {
		t.Fatalf("no generated Dockerfile: %v", err)
	}
	if !strings.Contains(string(df), "FROM "+string(stage.Env)) {
		t.Errorf("Dockerfile lost the pin: %s", df)
	}
	if !strings.Contains(string(df), "COPY inputs "+inputDir) {
		t.Errorf("Dockerfile does not bake the inputs: %s", df)
	}
	toml, err := os.ReadFile(filepath.Join(dir, "task.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(toml), "docker_image = "+strconv.Quote(string(stage.Env))) {
		t.Error("task.toml still names the prebuilt image: it would win over the Dockerfile and the inputs would never arrive")
	}
	if !strings.Contains(instruction(stage), inputDir+"/0/finding.json") {
		t.Error("the agent is never told where its inputs are")
	}
}

// A declared name becomes a path in a build context, so it must be a plain
// relative one. Stage.Outputs is Go source and cannot be chosen by an agent —
// but three gates were broken today by a rule that held only in a comment.
func TestInputNamesCannotEscapeTheBuildContext(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b", ""} {
		if err := cleanName(bad); err == nil {
			t.Errorf("cleanName(%q) = nil, want an error", bad)
		}
	}
	for _, ok := range []string{"finding.json", "sub/finding.json"} {
		if err := cleanName(ok); err != nil {
			t.Errorf("cleanName(%q) = %v, want nil", ok, err)
		}
	}
}

// The gate must see what the agent was given. Harbor carries only the agent's
// declared ARTIFACTS into the verifier, so a gate that judges work against its
// inputs saw nothing at all — measured, when a live run lost every validate
// attempt to infra_error before this existed.
func TestTheGateSeesTheStagesInputs(t *testing.T) {
	produced := t.TempDir()
	if err := os.WriteFile(filepath.Join(produced, "finding.json"), []byte(`{"f":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned := Image("g@sha256:" + strings.Repeat("b", 64))
	stage := Stage{
		ID: "validate", Agent: ClaudeCode, Env: Image("e@sha256:" + strings.Repeat("a", 64)),
		Prompt: "disprove it", Outputs: []string{"verdict.json"},
		Inputs: []Result{{Manifest: Manifest{{Name: "finding.json", Digest: "sha256:0"}}, artifactsDir: produced}},
		Gate:   SoundGate(pinned),
	}
	dir := t.TempDir()
	tag, root, err := gateContext(dir, stage)
	if err != nil {
		t.Fatalf("gateContext: %v", err)
	}
	if root == "" || tag == pinned {
		t.Fatal("a gated stage with inputs got no derived gate: it would run blind")
	}
	got, err := os.ReadFile(filepath.Join(root, "inputs", "0", "finding.json"))
	if err != nil || string(got) != `{"f":1}` {
		t.Fatalf("the input never reached the gate's build context: %q, %v", got, err)
	}
	df, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if !strings.Contains(string(df), "FROM "+string(pinned)) {
		t.Errorf("the derived gate lost the pinned gate's bytes: %s", df)
	}
	if !strings.Contains(string(df), "COPY inputs "+inputDir) {
		t.Errorf("the derived gate does not carry the inputs: %s", df)
	}
	// Same gate + same inputs must name the same image, or every attempt
	// rebuilds and resume stops meaning anything.
	again, _, _ := gateContext(t.TempDir(), stage)
	if again != tag {
		t.Errorf("derived tag is not a function of its inputs: %s vs %s", tag, again)
	}
	// A stage with no inputs needs no derived gate.
	plain := stage
	plain.Inputs = nil
	if tag, root, _ := gateContext(t.TempDir(), plain); tag != pinned || root != "" {
		t.Errorf("an input-free stage got a derived gate: %s %s", tag, root)
	}
}
