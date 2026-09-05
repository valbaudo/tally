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
//	Main("vdh", Lease{Attempts: vdhAttempts, WallClock: vdhRootClock, AttemptWallClock: vdhAttemptClock}, VDH)
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
	// vdhNoCorpus is the round-zero corpus: a value vdhCorpus cannot return,
	// since every corpus it builds is either empty or ends in a newline.
	// Seeding prev with "" compared round one against a round that never ran,
	// so a first round whose four survivors declared nothing counted as dry
	// before anything had been hunted twice.
	vdhNoCorpus = "no round has completed"
	// vdhRoundCost: a round is two fans, one per class each.
	vdhRoundCost = 2 * len(vdhClasses)
	// vdhRoundAttempts leases a round at its width PLUS two, because an
	// infra_error retry spends the same counter as a deliberate dispatch: at
	// exactly vdhRoundCost the first flake in the hunt fan makes the validate
	// fan dispatch on a spent scope, which unwinds the whole run.
	vdhRoundAttempts = vdhRoundCost + 2
	// vdhRounds bounds the loop in ROUNDS, because a round is what the lease
	// has to be able to pay for in one piece. THREE rounds is the floor for
	// dry == vdhDryStop: round one only establishes the corpus, with nothing
	// yet to compare it against, so the earliest two consecutive unchanged
	// rounds are two and three. (The old claim of four assumed round two MUST
	// find something new because it is told not to re-report — a hope about the
	// agent, not a bound.) Every incomplete round advances nothing while still
	// costing a round, so six is that floor plus slack for three.
	vdhRounds = 6

	// Clocks, on the serial-floor model cybergym and pr-ci already use: a
	// scope's clock must cover every dispatch running one after another,
	// because dawn admits against ITS concurrency, not the author's. A scope
	// clocked below its floor makes its own later attempts unreachable, and
	// they return Exhausted — which then feeds the guards that ask whether a
	// gate voted. A 2h round against vdhRoundAttempts x 30m did exactly that.
	vdhAttemptClock = 30 * time.Minute
	vdhRoundClock   = time.Duration(vdhRoundAttempts) * vdhAttemptClock
	vdhReconClock   = vdhAttemptClock
	vdhRootClock    = vdhReconClock + time.Duration(vdhRounds)*vdhRoundClock
	// vdhAttempts is the root lease the loop actually needs: recon, one recon
	// retry, and six fully funded rounds. Nested scopes draw from the root, so
	// the root has to hold every round's whole lease, headroom included.
	vdhAttempts = 2 + vdhRounds*vdhRoundAttempts // 62
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
// bugs are real". Unverified is therefore this protocol's SUCCESS state, so it
// is returned only when some gate somewhere actually ran — Exhausted when the
// root clock left no room for a single round, InfraError when rounds ran and
// no hunter ever reached a verdict, recon's own state when recon did not,
// because a hunt seeded by a failed recon is not a hunt.
func VDH(run *Scope) State {
	// One profile drives every hunter and validator below: the caveat's claim
	// that this run is single-vendor is read from the same variable that
	// configures the stages, not asserted beside them. Codex.FanOut is quoted
	// rather than branched on — it is a fixed property of the profile, so
	// branching on it would be a caveat dressed up as a condition.
	hunter := ClaudeCode
	run.Record("caveat-vendor", fmt.Sprintf(
		"cross-vendor decorrelation lost: one profile runs every hunt and every validation in this run, and the codex profile fixes FanOut=%v, so that profile can only be claude-code; a blind spot shared by all four hunters is invisible",
		Codex.FanOut))
	// Two caveats, two names. Record writes a name once per run, so writing
	// both under "caveat" published one of them and silently dropped the
	// other — in a protocol whose entire product is the run record.
	run.Record("caveat-coverage", "coverage unknown: the gate checks that citations resolve, not that findings are correct; no stage can reach passed and no count of dry rounds proves the repo is clean")

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

	// The round-zero corpus is a value no corpus can equal. recon's artifact is
	// orientation notes, not findings, so hashing it as round zero would make
	// round one permanently wet — but "" was no better: it is exactly what
	// vdhCorpus returns for four survivors that declared nothing.
	carry, prev, dry := []Result{recon}, vdhNoCorpus, 0
	// Exhausted seeds the loop's own outcome: the loop can run zero times —
	// recon may have spent the root clock — and "no hunt was ever dispatched"
	// is not the state a completed hunt returns. observed is the other half:
	// rounds that ran but in which no gate ever voted are a catastrophe, and
	// returning this protocol's success state for them would make catastrophe
	// and measurement identical. cybergym carries the first guard, mdash the
	// second; this loop needs both.
	rounds, observed := 0, false
	for round := 1; round <= vdhRounds && dry < vdhDryStop && run.More(); round++ {
		// One scope per round, leased for one round's width plus two flakes.
		// More() answers "can one more attempt be admitted", and a round
		// spends eight, so it cannot be the round's guard; the round count is,
		// and vdhAttempts is declared to fund it. What More() still guards
		// here is the root clock.
		//
		// 2h: two fans run in sequence, each 30m of attempt clock when its
		// four children run in parallel, so 60m of work — plus one 30m retry
		// and half an hour of backoff. At 80m one retried hunt child left the
		// validate fan less than its own attempt clock to finish in.
		rs := run.Scope(fmt.Sprintf("round-%d", round), Lease{
			Attempts:         vdhRoundAttempts,
			WallClock:        vdhRoundClock,
			AttemptWallClock: vdhAttemptClock,
		})
		rounds++

		hunts := rs.Fan(len(vdhClasses), func(i int) Stage {
			return Stage{
				ID:     fmt.Sprintf("hunt-r%d-%s", round, vdhClasses[i]),
				Agent:  hunter,
				Env:    vdhEnv,
				Prompt: vdhHuntPrompt(vdhClasses[i]),
				Inputs: carry,
				Gate:   FormatOnlyGate(vdhGate),
			}
		})
		run.Record(fmt.Sprintf("round-%d-hunt-states", round), vdhStates(hunts))
		if vdhCancelled(hunts) {
			return Cancelled
		}

		// A round is comparable only if all four classes were hunted and all
		// four validated. Attrition is not progress: a round that lost one
		// class hashes differently from the same findings a round earlier, so
		// counting it would RESET dry and a repo that went dry would burn
		// every round it has. And a round that lost them all is a corpus that
		// is UNKNOWN, not empty — overwriting carry with nil would file total
		// failure as either progress or convergence, depending only on what
		// the last round happened to hold. So an incomplete round changes
		// nothing, counts as neither dry nor progress, and says so.
		found := vdhSurviving(hunts)
		// Observed asks whether anything was learned, which is not the same as
		// whether anything SURVIVED: a round whose hunts all voted Rejected
		// learned something and found nothing.
		observed = observed || anyDecided(hunts)
		if len(found) < len(vdhClasses) {
			run.Record(fmt.Sprintf("round-%d-incomplete", round), fmt.Sprintf("only %d of %d hunt children reached a verdict; corpus unchanged and the round counts as neither dry nor progress", len(found), len(vdhClasses)))
			continue
		}

		vals := rs.Fan(len(vdhClasses), func(i int) Stage {
			return Stage{
				ID:     fmt.Sprintf("validate-r%d-%s", round, vdhClasses[i]),
				Agent:  hunter,
				Env:    vdhEnv,
				Prompt: vdhValidatePrompt(vdhClasses[i]),
				Inputs: found,
				Gate:   FormatOnlyGate(vdhGate),
			}
		})
		run.Record(fmt.Sprintf("round-%d-validate-states", round), vdhStates(vals))
		if vdhCancelled(vals) {
			return Cancelled
		}

		live := vdhSurviving(vals)
		if len(live) < len(vdhClasses) {
			run.Record(fmt.Sprintf("round-%d-incomplete", round), fmt.Sprintf("only %d of %d validate children reached a verdict; corpus unchanged and the round counts as neither dry nor progress", len(live), len(vdhClasses)))
			continue
		}
		carry = live

		// A dry round changed nothing: the surviving corpus hashes to what it
		// hashed last round, over the same four classes both times. The
		// manifest already carries the digests, so no agent output is parsed
		// to decide the loop's exit.
		if d := vdhCorpus(carry); d == prev {
			dry++
		} else {
			prev, dry = d, 0
		}
	}
	// Why the loop stopped. "Went dry" and "ran out of rounds" are different
	// findings about the repo and the loop returns the same state for both.
	run.Record("stop", fmt.Sprintf("%d of %d rounds ran, %d consecutive dry of %d needed, converged=%v", rounds, vdhRounds, dry, vdhDryStop, dry >= vdhDryStop))
	switch {
	case rounds == 0:
		return Exhausted
	case !observed:
		return InfraError
	}
	return Unverified
}

// vdhCancelled reports whether a drained fan contains a cancelled child. Every
// other state is the round's business; a cancel is the run's, and continuing
// would dispatch five more rounds into a cancelled run and then call the
// result unverified.
func vdhCancelled(rs []Result) bool {
	for _, r := range rs {
		if r.State == Cancelled {
			return true
		}
	}
	return false
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

// vdhCorpus fingerprints a round's surviving artifacts. The separator is
// load-bearing: without it a name's tail and a digest's head are one string,
// and two different corpora can fingerprint alike — a false match is a live
// repo called dry two rounds early.
func vdhCorpus(rs []Result) string {
	s := ""
	for _, r := range rs {
		for _, a := range r.Manifest {
			s += a.Name + "@" + a.Digest + "\n"
		}
	}
	return s
}
