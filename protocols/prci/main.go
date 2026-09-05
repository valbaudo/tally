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
// The actuator is not wired yet (runtime-core/actuator), so this program stops
// at the verdict and returns it. Nothing here pushes; nothing here ever will,
// since the push belongs to dawn's process and not to the agent's container.
package main

import (
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
	env  dawn.Image = "rc-pr-ci-env@sha256:1af66f7b408d82f983a10f262322cafe99e6408d8c33e632fc4f83bb1486dbd4"
	gate dawn.Image = "rc-pr-ci-gate@sha256:c42675a9a12badc77d667607b3704166a4a424eea9096cf34cb8346d4e874a31"
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
// exhausted, infra_error and cancelled all publish nothing.
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
	// the arithmetic dawn did across the attempt visible in the run record,
	// which for a protocol with no actuator yet is the whole product.
	if reward, ok := fix.Metric("reward"); ok {
		run.Record("fix_reward", reward)
	}
	run.Record("fix_manifest", fix.Manifest)
	return fix.State
}
