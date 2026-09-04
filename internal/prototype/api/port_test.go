package api

// The check: all four independently drafted protocols must be expressible in
// this surface, with nothing left over. These functions are never called —
// compiling them IS the test. If a construct is missing, this file stops
// building; if a construct is here that none of them uses, it has no forcing
// protocol and must be deleted — unless a settled decision forces it, in which
// case TRACE.md names that decision instead of naming a protocol.

import (
	"fmt"
	"time"
)

const (
	envImg  Image = "dawn-env@sha256:aa"
	gateImg Image = "dawn-gate@sha256:bb"
)

// cybergym: sequential attempts against a sound proof-of-vulnerability oracle.
func portCybergym() {
	Main("cybergym", Lease{Attempts: 12, WallClock: 2 * time.Hour}, func(run *Scope) State {
		study := run.Scope("study", Lease{Attempts: 2, WallClock: 20 * time.Minute, AttemptWallClock: 10 * time.Minute})
		notes := study.Run(Stage{
			ID: "study", Agent: ClaudeCode, Env: envImg,
			Prompt: "Read /app/src/vuln.c and write findings.",
			Gate:   NoGate("orientation prose; nothing about it is machine-checkable"),
		})
		if notes.State != Unverified {
			return notes.State
		}

		pov := run.Scope("pov", Lease{Attempts: 8, WallClock: 90 * time.Minute, AttemptWallClock: 12 * time.Minute})
		stage := Stage{
			ID: "pov", Agent: ClaudeCode, Env: envImg,
			Inputs: []Result{notes},
			Prompt: "Write a proof-of-vulnerability.",
			Gate:   SoundGate(gateImg),
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
	instances := []Image{"dawn-mdash-stock@sha256:01", "dawn-mdash-patched@sha256:02"}
	briefs := [2]string{"argue it IS reachable", "argue it is NOT reachable"}

	Main("mdash", Lease{Attempts: 2000, WallClock: 24 * time.Hour}, func(run *Scope) State {
		prove := func(scope *Scope, env Image, phase string) (proved, ran bool) {
			branches := scope.Fan(24, func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("%s-prove-%d", phase, i), Agent: ClaudeCode, Env: env,
					Prompt: "Read bob's note as alice.",
					Gate:   SoundGate(gateImg),
				}
			})
			run.Record("prove-states-"+phase+"-"+string(env), states(branches))
			for _, r := range branches {
				if r.State == Passed {
					if err := r.Actuate(fileFinding); err != nil {
						run.Record("actuation_failed", err.Error())
					}
					return true, true
				}
				ran = ran || r.State == Rejected
			}
			return false, ran
		}

		type agreement struct {
			env   Image
			claim float64
		}
		var agreed []agreement
		observed := false
		for _, env := range instances {
			scope := run.Scope(string(env), Lease{Attempts: 80, WallClock: 3 * time.Hour, AttemptWallClock: 10 * time.Minute})
			routed := scope.Fan(2, func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("route-%d", i), Agent: ClaudeCode, Env: env,
					Prompt: briefs[i],
					Gate:   FormatOnlyGate(gateImg),
				}
			})
			run.Record("route-states-"+string(env), states(routed))
			a, aok := routed[0].Metric("exploitable")
			b, bok := routed[1].Metric("exploitable")
			if aok && bok {
				observed = true
				if a == b {
					agreed = append(agreed, agreement{env, a})
					continue
				}
			}
			proved, ran := prove(scope, env, "route")
			run.Record("proved-"+string(env), proved)
			observed = observed || ran
		}

		// One-sided oracle: only a "not exploitable" agreement can be shown
		// wrong, so only those are in the denominator.
		var auditable []agreement
		for _, g := range agreed {
			if g.claim == 0 {
				auditable = append(auditable, g)
			}
		}
		wrong := 0
		for _, g := range auditable {
			scope := run.Scope("audit-"+string(g.env), Lease{Attempts: 80, WallClock: 3 * time.Hour, AttemptWallClock: 20 * time.Minute})
			proved, ran := prove(scope, g.env, "audit")
			observed = observed || ran
			if proved {
				wrong++
			}
		}
		run.Record("audited", len(auditable))
		if len(auditable) == 0 {
			run.Record("false_agreement_rate", "undefined: nothing was auditable")
		} else {
			run.Record("false_agreement_rate", float64(wrong)/float64(len(auditable)))
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
	const roundCost = 2 * len(classes)
	const rounds = 4

	Main("vdh", Lease{Attempts: 1 + rounds*roundCost, WallClock: 6 * time.Hour, AttemptWallClock: 30 * time.Minute}, func(p *Scope) State {
		p.Record("caveat", fmt.Sprintf("cross-vendor decorrelation lost: codex fixes FanOut=%v, so every hunter is claude-code", Codex.FanOut))
		p.Record("caveat", "coverage unknown: no stage here can reach passed")

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
		for round := 1; round <= rounds && dry < 2 && p.More(); round++ {
			rs := p.Scope(fmt.Sprintf("round-%d", round), Lease{Attempts: roundCost, WallClock: 80 * time.Minute, AttemptWallClock: 30 * time.Minute})
			hunts := rs.Fan(len(classes), func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("hunt-r%d-%s", round, classes[i]), Agent: ClaudeCode, Env: envImg,
					Prompt: "Hunt " + classes[i], Inputs: carry,
					Gate: FormatOnlyGate(gateImg),
				}
			})
			p.Record(fmt.Sprintf("round-%d-hunt-states", round), states(hunts))

			vals := rs.Fan(len(classes), func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("validate-r%d-%s", round, classes[i]), Agent: ClaudeCode, Env: envImg,
					Prompt: "Disconfirm " + classes[i], Inputs: surviving(hunts),
					Gate: FormatOnlyGate(gateImg),
				}
			})
			p.Record(fmt.Sprintf("round-%d-validate-states", round), states(vals))

			live := surviving(vals)
			if len(live) == 0 {
				// Unknown, not empty. Clobbering carry here would make total
				// failure differ from prev and reset dry.
				p.Record(fmt.Sprintf("round-%d-void", round), "no validate child reached a verdict")
				continue
			}
			carry = live
			if d := corpus(carry); d == prev {
				dry++
			} else {
				prev, dry = d, 0
			}
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
			s += a.Name + a.Digest
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
			Gate:   SoundGate(gateImg),
		})
		if fix.State != Passed {
			return fix.State
		}
		if err := fix.Actuate(openPR); err != nil {
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
