package api

// PRCI: the one protocol that mutates something outside itself.
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
// Drive it with:
//
//	Main("pr-ci", Lease{Attempts: 2, WallClock: 45 * time.Minute, AttemptWallClock: 20 * time.Minute}, PRCI)
const (
	prciEnv  Image = "dawn-pr-ci-env@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	prciGate Image = "dawn-pr-ci-gate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

const prciPrompt = `The repository at /app/repo has a red CI: ./ci.sh fails. Find the bug and fix
it. Hand back exactly one thing: a unified diff, produced with
"git -C /app/repo diff", as your declared output. Do not commit. Do not push.
Do not hand back a modified tree — only that diff is read, and it is read as
data, never run. Only changes to calc.py are considered: hunks touching the
test suite or ci.sh are discarded before your fix is judged.`

// PRCI returns the stage's state unchanged. Rejected, unverified, exhausted,
// infra_error and cancelled all publish nothing. A rejected patch is terminal
// on purpose: the agent cannot see why the gate said no, so a second attempt is
// spend, not signal.
func PRCI(run *Scope) State {
	fix := run.Run(Stage{
		ID:     "fix",
		Agent:  ClaudeCode,
		Env:    prciEnv,
		Prompt: prciPrompt,
		Gate:   SoundGate(prciGate),
	})
	if fix.State != Passed {
		return fix.State
	}
	// A stage that passed and then failed to publish is not a rejected stage;
	// the mutation simply did not happen. infra_error is the nearest of the six
	// — the one place the state set feels short, and only in the protocol that
	// mutates.
	if err := fix.Actuate(prciOpenPR); err != nil {
		run.Record("actuation_failed", err.Error())
		return InfraError
	}
	return Passed
}

// prciOpenPR runs in dawn's process after the gate voted. dawn has already
// deduplicated its own retries against a.Key; the branch name carries the same
// key so a re-push lands on the same branch upstream, which is idempotency dawn
// cannot do on the far side for us.
func prciOpenPR(a *Actuation) error {
	branch := "dawn/ci-fix-" + a.Key
	_ = branch
	_ = a.Published("applied.patch") // the filtered patch, as it actually applied
	_ = a.Published("pr-body.md")    // the CI output that justified reward 1
	return nil
}
