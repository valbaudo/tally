package dawn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	if err := writeTask(dir, s, 5*time.Minute); err != nil {
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

func TestWriteTaskRejectsUnpinnedAndUnexplained(t *testing.T) {
	pinned := Image("x@sha256:" + strings.Repeat("c", 64))
	for name, s := range map[string]Stage{
		"unpinned env":  {ID: "a", Env: "rc-env:1", Gate: SoundGate(pinned)},
		"unpinned gate": {ID: "a", Env: pinned, Gate: SoundGate("rc-gate:1")},
		"silent nogate": {ID: "a", Env: pinned, Gate: NoGate("")},
		"no id":         {ID: "", Env: pinned, Gate: SoundGate(pinned)},
	} {
		if err := writeTask(t.TempDir(), s, time.Minute); err == nil {
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
		"dawn's clock ended it":  {sound, trial{Present: true, Rewarded: true, Rewards: map[string]float64{"reward": 1}, TimedOut: true}, Exhausted},
		"format-only clamped":    {format, won, Unverified},
		"ungated":                {none, trial{Present: true}, Unverified},
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
