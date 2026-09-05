// Command prci is the pr-ci protocol: the one that mutates something outside
// itself.
//
// The agent is handed a repo whose CI is red and hands back exactly one thing:
// a unified diff. Never a mutated tree — that would be a large agent-controlled
// filesystem the gate would have to trust. A diff is small, inert, filterable.
//
// The gate runs in its own container after the agent's is gone, starts from its
// OWN pristine baked repo, applies the diff behind `git apply --include=calc.py`
// and runs its OWN baked suite. Only on Passed does the actuator run, inside
// dawn's process where the credentials live, publishing the bytes the GATE
// wrote and never the agent's raw patch.
//
// The actuator pushes to a local bare git repo standing in for a real remote —
// no network, no GitHub, but a genuine `git push` all the same, so the shape of
// a real actuator (dedup by Step, publish only the gate's bytes) is exercised
// end to end without spending anything real.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/valbaudo/dawn"
)

// The two images, pinned by digest and not by tag, because the whole soundness
// argument is an argument about which bytes are in which image. Rebuilding
// either one invalidates the pin here and dawn refuses the stage at dispatch
// rather than running against bytes nobody named.
//
// The environment carries the seeded repo at /app/repo, an empty /app/outputs,
// and a baked @anthropic-ai/claude-code@2.1.259 — baked because Harbor runs the
// agent inside this container and its installer would need hosts the agent
// phase is not allowed to reach. The gate carries the pristine repo at
// /gate/repo and dawn's verdict script at /tests/test.sh.
const (
	env  dawn.Image = "dawn-pr-ci-env@sha256:b75eb2a91c251a49b58d0ff902ee60336748161d6dabe970b54496d7db509aa2"
	gate dawn.Image = "dawn-pr-ci-gate@sha256:6d995127e36488de43b2f6a10e600b6ce871228e3276a5c26c2e1348ea4477ac"
)

// The prompt is part of the integrity argument: it is the only thing that tells
// the agent what to hand back, and the filter it describes is one the gate
// actually enforces rather than a request the agent may decline.
const prompt = `The repository at /app/repo has a red CI: ./ci.sh fails. Find the bug and fix
it. Hand back exactly one thing: a file named fix.patch containing a unified
diff, produced with "git -C /app/repo diff". Do not commit. Do not push. Do not
hand back a modified tree — only that diff is read, and it is read as data,
never run. Only changes to calc.py are considered: hunks touching the test
suite or ci.sh are discarded before your fix is judged.`

// The lease. Dispatching derives the WallClock from the attempt clock and
// dawn's own retry backoff, so it is never hand-written and never short.
//
// Two attempts is work 1 plus headroom 1: the single deliberate attempt, and
// one spare that only an infra_error can spend. A Rejected patch does not get
// a second attempt on purpose — the agent cannot see why the gate said no, so
// re-running it is spend, not signal.
func main() {
	dawn.Main("pr-ci", dawn.Dispatching(2, 20*time.Minute), protocol)
}

// protocol returns the stage's state unchanged. Rejected, unverified,
// exhausted, infra_error and cancelled all publish nothing — fix.Actuate
// enforces that itself, so actuate below is called unconditionally and simply
// has nothing to do on any of those states.
func protocol(run *dawn.Scope) dawn.State {
	fix := run.Run(dawn.Stage{
		ID:      "fix",
		Agent:   dawn.ClaudeCode,
		Env:     env,
		Prompt:  prompt,
		Outputs: []string{"fix.patch"},
		Gate:    dawn.SoundGate(gate),
	})
	// The reward is a number the gate wrote, never a state. Recording it keeps
	// the arithmetic dawn did across the attempt visible in the run record.
	if reward, ok := fix.Metric("reward"); ok {
		run.Record("fix_reward", reward)
	}
	run.Record("fix_manifest", fix.Manifest)

	if fix.State == dawn.Passed {
		if err := actuate(run, fix); err != nil {
			run.Record("actuation_error", err.Error())
		}
	}
	return fix.State
}

// actuate pushes the GATE's own fix.patch — never the agent's raw declared
// output, which the gate may have filtered — to a branch in a local bare git
// repo standing in for a real remote. Two ordered sub-steps, each deduped by
// dawn against this run's own record: a retried Actuate closure within the
// same run pushes the same branch and tag at most once.
func actuate(run *dawn.Scope, fix dawn.Result) error {
	return fix.Actuate(func(a *dawn.Actuation) error {
		remote, err := os.MkdirTemp("", "dawn-prci-remote-")
		if err != nil {
			return err
		}
		if err := runGit("", "init", "--bare", "-q", remote); err != nil {
			return err
		}
		run.Record("remote", remote)

		work, err := os.MkdirTemp("", "dawn-prci-work-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		branch := "dawn/" + a.Key
		if err := runGit(work, "init", "-q", "-b", branch); err != nil {
			return err
		}
		patch, err := os.ReadFile(a.Published("fix.patch"))
		if err != nil {
			return fmt.Errorf("reading the gate's published patch: %w", err)
		}
		if err := os.WriteFile(filepath.Join(work, "fix.patch"), patch, 0o644); err != nil {
			return err
		}
		if err := runGit(work, "add", "fix.patch"); err != nil {
			return err
		}
		if err := runGit(work, "-c", "user.email=dawn@localhost", "-c", "user.name=dawn",
			"commit", "-q", "-m", "dawn: publish gate-verified fix"); err != nil {
			return err
		}

		if _, err := a.Step("push", func() (string, error) {
			return branch, runGit(work, "push", "-q", remote, branch)
		}); err != nil {
			return err
		}
		tag := "verified/" + a.Key
		_, err = a.Step("tag", func() (string, error) {
			if err := runGit(work, "tag", tag); err != nil {
				return "", err
			}
			return tag, runGit(work, "push", "-q", remote, tag)
		})
		return err
	})
}

// runGit runs one git command with dir as its working tree (ignored for a
// bare init, which names its target directory as an argument instead).
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}
