package api

import (
	"fmt"
	"time"
)

// VDH: hunt one bug class four ways, fan in, validate, repeat until dry.
//
// There is no sound oracle for this target. The only gate that exists checks
// that a citation resolves to a real, non-trivial, verbatim-quoted source line;
// it has never seen the ground truth and scores a correctly-cited safe decoy
// 1.0. So every gated stage here is format-only, nothing can reach Passed, and
// there is consequently no actuator: the findings stay in the trial's artifacts
// tree and the run record — not a published effect — is the whole product.
//
// Drive it with:
//
//	Main("vdh", Lease{Attempts: vdhAttempts, WallClock: 6 * time.Hour, AttemptWallClock: 30 * time.Minute}, VDH)
const (
	vdhEnv  Image = "dawn-vdh-env@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	vdhGate Image = "dawn-vdh-gate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// vdh seeds one bug class four times by four splicing mechanisms. The mechanism
// is the thing a hunter can specialise on, so it is the fan's axis. An array,
// not a slice, so the round's cost below is a compile-time constant.
var vdhClasses = [4]string{"concat", "percent", "fstring", "dotformat"}

const (
	// vdhDryStop: rounds that change nothing before the loop gives up.
	vdhDryStop = 2
	// vdhRoundCost: a round is two fans, one per class each.
	vdhRoundCost = 2 * len(vdhClasses)
	// vdhRounds bounds the loop in ROUNDS, because a round is what the lease
	// has to be able to pay for in one piece. Three rounds is the floor for
	// dry == 2 when the first round finds something; the fourth is slack for
	// one void round.
	vdhRounds = 4
	// vdhAttempts is the root lease the loop actually needs: recon plus four
	// whole rounds. The old declaration of 24 could not fund three rounds and
	// so could never reach its own stopping condition; the fix is the honest
	// number, not a smaller vdhDryStop.
	vdhAttempts = 1 + vdhRounds*vdhRoundCost // 33
)

const vdhReconPrompt = `Read every file under /app/repo. Note where untrusted input enters and where
SQL strings are built. Report no findings yet.`

func vdhHuntPrompt(class string) string {
	return fmt.Sprintf(`Hunt ONLY for SQL built by %s splicing in /app/repo. Earlier surviving
findings are mounted read-only; do not re-report them. Write one JSON object
per line to your declared output with keys file, line, class, evidence —
evidence is the verbatim cited source line. Write the file even if you find
nothing: an empty declared output is a result, a missing one is a broken run.`, class)
}

func vdhValidatePrompt(class string) string {
	return fmt.Sprintf(`Candidate findings are mounted read-only. Take the ones with class %q and try
to DISCONFIRM each: a bound-parameter query is safe, not a bug. Write only the
survivors of that class to your declared output, same format. Write the file
even if none survive.`, class)
}

// VDH returns Unverified: with no sound oracle, that is the best state any
// stage in this protocol can reach, and it means "the gate ran", never "these
// bugs are real". It returns recon's state instead when recon never reached a
// verdict, because a hunt seeded by a failed recon is not a hunt.
func VDH(run *Scope) State {
	// Not a runtime check — Codex.FanOut is a fixed property of the profile,
	// so branching on it would be a caveat dressed up as a condition. The
	// value is read INTO the caveat instead, which is what makes the statement
	// checkable against the profile rather than merely asserted next to it.
	run.Record("caveat", fmt.Sprintf(
		"cross-vendor decorrelation lost: the codex profile fixes FanOut=%v, so the only profile that may fan out is claude-code and every hunt in this run is claude-code; a blind spot shared by all four hunters is invisible",
		Codex.FanOut))
	run.Record("caveat", "coverage unknown: the gate checks that citations resolve, not that findings are correct; no stage can reach passed and no count of dry rounds proves the repo is clean")

	recon := run.Run(Stage{
		ID:     "recon",
		Agent:  ClaudeCode,
		Env:    vdhEnv,
		Prompt: vdhReconPrompt,
		Gate:   NoGate("orientation only: where to look is not checkable without a sound oracle"),
	})
	// Unverified is this stage's success state; anything else is dawn telling
	// us the orientation never happened. cybergym checks its own equivalent —
	// letting an infra_error recon flow into Inputs would hand every hunter an
	// empty mount and call the resulting silence a dry round.
	if recon.State != Unverified {
		return recon.State
	}

	// The seed is the EMPTY corpus. Seeding it from recon's digest compared a
	// notes artifact against a findings artifact — two different declared
	// outputs — so round one could never be dry no matter what it found.
	carry, prev, dry := []Result{recon}, "", 0
	for round := 1; round <= vdhRounds && dry < vdhDryStop && run.More(); round++ {
		// One scope per round, leased for exactly one round. More() answers
		// "can one more attempt be admitted", and a round spends eight, so it
		// cannot be the round's guard; the round count is, and vdhAttempts is
		// declared to fund it. What More() still guards here is the clock.
		rs := run.Scope(fmt.Sprintf("round-%d", round), Lease{
			Attempts:         vdhRoundCost,
			WallClock:        80 * time.Minute,
			AttemptWallClock: 30 * time.Minute,
		})

		hunts := rs.Fan(len(vdhClasses), func(i int) Stage {
			return Stage{
				ID:     fmt.Sprintf("hunt-r%d-%s", round, vdhClasses[i]),
				Agent:  ClaudeCode,
				Env:    vdhEnv,
				Prompt: vdhHuntPrompt(vdhClasses[i]),
				Inputs: carry,
				Gate:   FormatOnlyGate(vdhGate),
			}
		})
		run.Record(fmt.Sprintf("round-%d-hunt-states", round), vdhStates(hunts))

		vals := rs.Fan(len(vdhClasses), func(i int) Stage {
			return Stage{
				ID:     fmt.Sprintf("validate-r%d-%s", round, vdhClasses[i]),
				Agent:  ClaudeCode,
				Env:    vdhEnv,
				Prompt: vdhValidatePrompt(vdhClasses[i]),
				Inputs: vdhSurviving(hunts),
				Gate:   FormatOnlyGate(vdhGate),
			}
		})
		run.Record(fmt.Sprintf("round-%d-validate-states", round), vdhStates(vals))

		live := vdhSurviving(vals)
		if len(live) == 0 {
			// Every validator dropped out. The corpus is UNKNOWN, not empty:
			// overwriting carry with nil and hashing that would make total
			// failure differ from prev and so RESET dry — total failure
			// reading as progress. Keep the last corpus actually observed and
			// count the round as neither dry nor progress.
			run.Record(fmt.Sprintf("round-%d-void", round), "no validate child reached a verdict; corpus unchanged and the round counts as neither dry nor progress")
			continue
		}
		carry = live

		// A dry round changed nothing: the surviving corpus hashes to what it
		// hashed last round. The manifest already carries the digests, so no
		// agent output is parsed to decide the loop's exit.
		if d := vdhCorpus(carry); d == prev {
			dry++
		} else {
			prev, dry = d, 0
		}
	}
	return Unverified
}

// vdhSurviving keeps the children whose gate actually ran. Filtering on State,
// never on a score. The children it drops are not silent: vdhStates records
// every child's terminal state, which is what "drop out loudly" has to mean.
func vdhSurviving(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.State == Unverified {
			out = append(out, r)
		}
	}
	return out
}

// vdhStates is the loud half: every child of a drained fan, in index order.
func vdhStates(rs []Result) []State {
	out := make([]State, len(rs))
	for i, r := range rs {
		out[i] = r.State
	}
	return out
}

func vdhCorpus(rs []Result) string {
	s := ""
	for _, r := range rs {
		for _, a := range r.Manifest {
			s += a.Name + a.Digest
		}
	}
	return s
}
