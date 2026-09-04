package api

// The check: all four independently drafted protocols must be expressible in
// this surface, with nothing left over. These functions are never called —
// compiling them is the test, and it is `go test` / `go vet` that compiles
// them, never `go build`, which skips _test.go files entirely.
//
// Compilation can prove SUFFICIENCY only: if a construct is missing, this file
// stops compiling. It cannot prove minimality, because nothing in Go fails
// because a construct went unused — a construct no port here uses is caught by
// the TRACE.md audit and by nothing else.

import (
	"fmt"
	"time"
)

// Full-length digests: an Image is a digest-pinned reference and dawn rejects
// anything else at dispatch, so a port that pins "sha256:aa" is demonstrating
// a stage no run would accept. Two gate images, because a sound gate and a
// format-only gate make opposite claims and one image cannot make both.
const (
	envImg        Image = "dawn-env@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	soundGateImg  Image = "dawn-sound-gate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	formatGateImg Image = "dawn-format-gate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// cybergym: sequential attempts against a sound proof-of-vulnerability oracle.
func portCybergym() {
	Main("cybergym", Lease{Attempts: 12, WallClock: 3 * time.Hour}, func(run *Scope) State {
		// Every lease here is work plus headroom: the retry the second attempt
		// exists to pay for needs a slot AND a clock to run in.
		study := run.Scope("study", Lease{Attempts: 2, WallClock: 30 * time.Minute, AttemptWallClock: 10 * time.Minute})
		notes := study.Run(Stage{
			ID: "study", Agent: ClaudeCode, Env: envImg,
			Prompt: "Read /app/src/vuln.c and write findings.",
			Gate:   NoGate("orientation prose; nothing about it is machine-checkable"),
		})
		if notes.State != Unverified {
			return notes.State
		}

		pov := run.Scope("pov", Lease{Attempts: 8, WallClock: 2 * time.Hour, AttemptWallClock: 12 * time.Minute})
		stage := Stage{
			ID: "pov", Agent: ClaudeCode, Env: envImg,
			Inputs: []Result{notes},
			Prompt: "Write a proof-of-vulnerability.",
			Gate:   SoundGate(soundGateImg),
		}
		// Seeded with a state, not a zero Result: the loop may run zero times,
		// and Result{}.State is a seventh state.
		state := Exhausted
		for pov.More() {
			if r := pov.Run(stage); r.State != Rejected {
				return r.State
			}
			state = Rejected
		}
		return state
	})
}

// mdash: an opposed-brief router, a 24-wide prove fan on disagreement, and an
// audited sample of the agreements that a proof can actually contradict.
func portMDASH() {
	instances := []Image{
		"dawn-mdash-stock@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"dawn-mdash-patched@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
	briefs := [2]string{"argue it IS reachable", "argue it is NOT reachable"}
	const auditCap = 3

	Main("mdash", Lease{Attempts: 2000, WallClock: 24 * time.Hour}, func(run *Scope) State {
		// Returns the oracle's own state: passed (proved), rejected (voted and
		// found nothing), cancelled, or infra_error (never voted). A bool pair
		// could not say the last one without the caller dropping half of it.
		// Stage ids carry phase AND env: attempt_id hashes no scope path, so
		// "prove-7" is one stage to recovery everywhere it appears.
		prove := func(scope *Scope, env Image, phase string) State {
			branches := scope.Fan(24, func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("%s-%s-prove-%d", phase, env, i), Agent: ClaudeCode, Env: env,
					Prompt: "Read bob's note as alice.",
					Gate:   SoundGate(soundGateImg),
				}
			})
			run.Record("prove-states-"+phase+"-"+string(env), states(branches))
			verdict := InfraError
			for i, r := range branches {
				switch r.State {
				case Passed:
					if err := r.Actuate(fileFinding); err != nil {
						run.Record(fmt.Sprintf("actuation-failed-%s-%s-%d", phase, env, i), err.Error())
					}
					return Passed
				case Cancelled:
					verdict = Cancelled
				case Rejected:
					if verdict == InfraError {
						verdict = Rejected
					}
				}
			}
			return verdict
		}

		type agreement struct {
			env   Image
			claim float64
		}
		var agreed []agreement
		observed := false
		routed, claimed := 0, 0
		for _, env := range instances {
			if !run.More() {
				run.Record("routing-stopped-early", fmt.Sprintf("root lease spent after %d of %d instances", routed, len(instances)))
				break
			}
			scope := run.Scope(string(env), Lease{Attempts: 80, WallClock: 3 * time.Hour, AttemptWallClock: 10 * time.Minute})
			branches := scope.Fan(2, func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("route-%s-%d", env, i), Agent: ClaudeCode, Env: env,
					Prompt: briefs[i],
					Gate:   FormatOnlyGate(formatGateImg),
				}
			})
			run.Record("route-states-"+string(env), states(branches))
			routed++
			if cancelled(branches) {
				return Cancelled
			}
			a, aok := branches[0].Metric("exploitable")
			b, bok := branches[1].Metric("exploitable")
			if aok && bok {
				observed = true
				claimed++
				if a == b {
					agreed = append(agreed, agreement{env, a})
					continue
				}
			}
			v := prove(scope, env, "route")
			if v == Cancelled {
				return Cancelled
			}
			run.Record("oracle-route-"+string(env), v)
			observed = observed || v != InfraError
		}

		// One-sided oracle: only a "not exploitable" agreement can be shown
		// wrong, so only those are in the denominator.
		var auditable []agreement
		for _, g := range agreed {
			if g.claim == 0 {
				auditable = append(auditable, g)
			}
		}
		// Every population the rate hangs off, so a reader can see the gap
		// between agreements and the subset a one-sided oracle can check.
		run.Record("instances_routed", routed)
		run.Record("instances_with_both_claims", claimed)
		run.Record("agreements", len(agreed))
		run.Record("unauditable_agreements", len(agreed)-len(auditable))

		sample := auditable
		if len(sample) > auditCap {
			sample = sample[:auditCap]
		}
		wrong, audited := 0, 0
		for _, g := range sample {
			if !run.More() {
				run.Record("audit-stopped-early", fmt.Sprintf("root lease spent after %d of %d sampled agreements", audited, len(sample)))
				break
			}
			scope := run.Scope("audit-"+string(g.env), Lease{Attempts: 80, WallClock: 3 * time.Hour, AttemptWallClock: 20 * time.Minute})
			v := prove(scope, g.env, "audit")
			if v == Cancelled {
				return Cancelled
			}
			run.Record("oracle-audit-"+string(g.env), v)
			observed = observed || v != InfraError
			// An agreement the oracle never tested is not one it failed to
			// contradict: the denominator is audits performed.
			if v == InfraError {
				continue
			}
			audited++
			if v == Passed {
				wrong++
			}
		}
		run.Record("audit_sample", len(sample))
		run.Record("audited", audited)
		if audited == 0 {
			run.Record("false_agreement_rate", "undefined: no auditable agreement was ever tested by the oracle")
		} else {
			run.Record("false_agreement_rate", float64(wrong)/float64(audited))
		}
		if !observed {
			return InfraError
		}
		return Unverified
	})
}

func fileFinding(a *Actuation) error {
	_ = a.Key
	_ = a.Published("finding.json")
	return nil
}

// vdh: an outer loop with fan-in and no sound oracle.
func portVDH() {
	classes := [4]string{"concat", "percent", "fstring", "dotformat"}
	const (
		dryStop = 2
		// Width plus two flakes: an infra_error retry spends the same counter.
		roundCost     = 2 * len(classes)
		roundAttempts = roundCost + 2
		// Four rounds is the floor for dryStop=2 when round two is told to
		// find something new; six is that floor plus slack for two rounds that
		// lose a class and so advance nothing.
		rounds = 6
	)

	Main("vdh", Lease{Attempts: 2 + rounds*roundAttempts, WallClock: 14 * time.Hour, AttemptWallClock: 30 * time.Minute}, func(p *Scope) State {
		// One profile drives every hunter and validator, read from the same
		// variable the caveat quotes.
		hunter := ClaudeCode
		// Two caveats, two names: Record writes a name once per run.
		p.Record("caveat-vendor", fmt.Sprintf("cross-vendor decorrelation lost: one profile runs every hunt here, and codex fixes FanOut=%v, so it can only be claude-code", Codex.FanOut))
		p.Record("caveat-coverage", "coverage unknown: no stage here can reach passed")

		recon := p.Run(Stage{
			ID: "recon", Agent: ClaudeCode, Env: envImg,
			Prompt: "Read everything under /app/repo.",
			Gate:   NoGate("orientation only: where to look is not checkable without a sound oracle"),
		})
		if recon.State != Unverified {
			return recon.State
		}

		// Empty seed: recon's notes are a different declared output from the
		// findings the rounds produce, so hashing them together made round one
		// permanently wet.
		carry, prev, dry := []Result{recon}, "", 0
		// Exhausted seeds the loop's outcome: zero rounds is not what a
		// completed hunt returns. observed is the other half — rounds that ran
		// with no gate ever voting are a catastrophe, not a measurement.
		ran, observed := 0, false
		for round := 1; round <= rounds && dry < dryStop && p.More(); round++ {
			rs := p.Scope(fmt.Sprintf("round-%d", round), Lease{Attempts: roundAttempts, WallClock: 2 * time.Hour, AttemptWallClock: 30 * time.Minute})
			ran++
			hunts := rs.Fan(len(classes), func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("hunt-r%d-%s", round, classes[i]), Agent: hunter, Env: envImg,
					Prompt: "Hunt " + classes[i], Inputs: carry,
					Gate: FormatOnlyGate(formatGateImg),
				}
			})
			p.Record(fmt.Sprintf("round-%d-hunt-states", round), states(hunts))
			if cancelled(hunts) {
				return Cancelled
			}

			// A round is comparable only if all four classes were hunted and
			// all four validated. Attrition hashes differently from the same
			// findings a round earlier, so it would reset dry; total loss
			// would clobber a real corpus with nothing. Neither is progress.
			found := surviving(hunts)
			observed = observed || len(found) > 0
			if len(found) < len(classes) {
				p.Record(fmt.Sprintf("round-%d-incomplete", round), fmt.Sprintf("only %d of %d hunt children reached a verdict; corpus unchanged", len(found), len(classes)))
				continue
			}

			vals := rs.Fan(len(classes), func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("validate-r%d-%s", round, classes[i]), Agent: hunter, Env: envImg,
					Prompt: "Disconfirm " + classes[i], Inputs: found,
					Gate: FormatOnlyGate(formatGateImg),
				}
			})
			p.Record(fmt.Sprintf("round-%d-validate-states", round), states(vals))
			if cancelled(vals) {
				return Cancelled
			}

			live := surviving(vals)
			if len(live) < len(classes) {
				p.Record(fmt.Sprintf("round-%d-incomplete", round), fmt.Sprintf("only %d of %d validate children reached a verdict; corpus unchanged", len(live), len(classes)))
				continue
			}
			carry = live
			if d := corpus(carry); d == prev {
				dry++
			} else {
				prev, dry = d, 0
			}
		}
		p.Record("stop", fmt.Sprintf("%d of %d rounds ran, %d consecutive dry of %d needed, converged=%v", ran, rounds, dry, dryStop, dry >= dryStop))
		switch {
		case ran == 0:
			return Exhausted
		case !observed:
			return InfraError
		}
		return Unverified
	})
}

// surviving keeps the children whose gate actually ran. Filtering on State,
// never on a score.
func surviving(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.State == Unverified {
			out = append(out, r)
		}
	}
	return out
}

// cancelled reports whether a drained fan contains a cancelled child: a cancel
// is the run's business, not the round's.
func cancelled(rs []Result) bool {
	for _, r := range rs {
		if r.State == Cancelled {
			return true
		}
	}
	return false
}

// states is what makes a dropped child loud: every branch of a drained fan.
func states(rs []Result) []State {
	out := make([]State, len(rs))
	for i, r := range rs {
		out[i] = r.State
	}
	return out
}

// corpus: a round is dry iff the surviving artifacts hash to what they hashed
// last round. The manifest already carries the digests, so this is author
// arithmetic — no agent output is parsed to decide it.
func corpus(rs []Result) string {
	s := ""
	for _, r := range rs {
		for _, a := range r.Manifest {
			s += a.Name + "@" + a.Digest + "\n"
		}
	}
	return s
}

// pr-ci: one agent stage, one sound gate, one actuator that opens the PR from
// the gate's own bytes.
func portPRCI() {
	Main("pr-ci", Lease{Attempts: 2, WallClock: 45 * time.Minute, AttemptWallClock: 20 * time.Minute}, func(run *Scope) State {
		fix := run.Run(Stage{
			ID: "fix", Agent: ClaudeCode, Env: envImg,
			Prompt: "Fix the red CI. Hand back a unified diff and nothing else.",
			Gate:   SoundGate(soundGateImg),
		})
		if fix.State != Passed {
			return fix.State
		}
		if err := fix.Actuate(openPR); err != nil {
			// Recorded, or "the gate said yes and the PR never opened" is
			// indistinguishable from "no verdict was ever obtained".
			run.Record("actuation_failed", err.Error())
			return InfraError
		}
		return Passed
	})
}

func openPR(a *Actuation) error {
	_ = "dawn/ci-fix-" + a.Key
	_ = a.Published("applied.patch")
	_ = a.Published("pr-body.md")
	return nil
}

var _ = []func(){portCybergym, portMDASH, portVDH, portPRCI}
