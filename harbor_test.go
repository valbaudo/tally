package dawn

import (
	"context"
	"os"
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
