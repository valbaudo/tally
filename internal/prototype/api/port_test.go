package api

// The check: all four independently drafted protocols must be expressible in
// this surface, with nothing left over. These functions are never called —
// compiling them IS the test. If a construct is missing, this file stops
// building; if a construct is here that none of them uses, it has no forcing
// protocol and must be deleted.

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
			Prompt:  "Read /app/src/vuln.c and write findings.",
			Outputs: []string{"notes"},
			Gate:    NoGate("orientation prose; nothing about it is machine-checkable"),
		})
		if notes.State != Unverified {
			return notes.State
		}

		pov := run.Scope("pov", Lease{Attempts: 8, WallClock: 90 * time.Minute, AttemptWallClock: 12 * time.Minute})
		stage := Stage{
			ID: "pov", Agent: ClaudeCode, Env: envImg,
			Inputs:  []Result{notes},
			Prompt:  "Write a proof-of-vulnerability.",
			Outputs: []string{"pov"},
			Gate:    SoundGate(gateImg),
		}
		var last Result
		for pov.More() {
			if last = pov.Run(stage); last.State != Rejected {
				return last.State
			}
		}
		return last.State
	})
}

// mdash: an opposed-brief router, a 24-wide prove fan on disagreement, and an
// audited sample of the agreements.
func portMDASH() {
	instances := []Image{"dawn-mdash-stock@sha256:01", "dawn-mdash-patched@sha256:02"}
	briefs := [2]string{"argue it IS reachable", "argue it is NOT reachable"}

	Main("mdash", Lease{Attempts: 2000, WallClock: 24 * time.Hour}, func(run *Scope) State {
		prove := func(scope *Scope, env Image) bool {
			branches := scope.Fan(24, func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("prove-%d", i), Agent: ClaudeCode, Env: env,
					Prompt: "Read bob's note as alice.", Outputs: []string{"exploit"},
					Gate: SoundGate(gateImg),
				}
			})
			for _, r := range branches {
				if r.State == Passed {
					if err := r.Actuate(fileFinding); err != nil {
						run.Record("actuation_failed", err.Error())
					}
					return true
				}
			}
			return false
		}

		var agreed []Image
		for _, env := range instances {
			scope := run.Scope(string(env), Lease{Attempts: 80, WallClock: 3 * time.Hour, AttemptWallClock: 10 * time.Minute})
			routed := scope.Fan(2, func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("route-%d", i), Agent: ClaudeCode, Env: env,
					Prompt: briefs[i], Outputs: []string{"claim"},
					Gate: FormatOnlyGate(gateImg),
				}
			})
			a, aok := routed[0].Metric("exploitable")
			b, bok := routed[1].Metric("exploitable")
			if aok && bok && a == b {
				agreed = append(agreed, env)
				continue
			}
			prove(scope, env)
		}

		wrong := 0
		for _, env := range agreed {
			scope := run.Scope("audit-"+string(env), Lease{Attempts: 80, WallClock: 3 * time.Hour, AttemptWallClock: 20 * time.Minute})
			if prove(scope, env) {
				wrong++
			}
		}
		if len(agreed) > 0 {
			run.Record("false_agreement_rate", float64(wrong)/float64(len(agreed)))
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
	classes := []string{"concat", "percent", "fstring", "dotformat"}

	Main("vdh", Lease{Attempts: 24, WallClock: 6 * time.Hour, AttemptWallClock: 30 * time.Minute}, func(p *Scope) State {
		if !Codex.FanOut {
			p.Record("caveat", "cross-vendor decorrelation lost: every hunter is claude-code")
		}
		p.Record("caveat", "coverage unknown: no stage here can reach passed")

		recon := p.Run(Stage{
			ID: "recon", Agent: ClaudeCode, Env: envImg,
			Prompt: "Read everything under /app/repo.", Outputs: []string{"notes"},
			Gate: NoGate("orientation only: where to look is not checkable without a sound oracle"),
		})

		carry := []Result{recon}
		prev := corpus(carry)
		for round, dry := 1, 0; p.More() && dry < 2; round++ {
			hunts := p.Fan(len(classes), func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("hunt-r%d-%s", round, classes[i]), Agent: ClaudeCode, Env: envImg,
					Prompt: "Hunt " + classes[i], Inputs: carry, Outputs: []string{"findings"},
					Gate: FormatOnlyGate(gateImg),
				}
			})
			found := surviving(hunts)

			vals := p.Fan(len(classes), func(i int) Stage {
				return Stage{
					ID: fmt.Sprintf("validate-r%d-%s", round, classes[i]), Agent: ClaudeCode, Env: envImg,
					Prompt: "Disconfirm " + classes[i], Inputs: found, Outputs: []string{"findings"},
					Gate: FormatOnlyGate(gateImg),
				}
			})
			carry = surviving(vals)

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
			Prompt:  "Fix the red CI. Hand back a unified diff and nothing else.",
			Outputs: []string{"fix.patch"},
			Gate:    SoundGate(gateImg),
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
