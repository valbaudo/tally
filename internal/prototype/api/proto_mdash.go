package api

import (
	"fmt"
	"time"
)

// MDASH: a cheap opposed-brief router over mdash instances, an expensive
// 24-wide prove fan whenever the briefs disagree, and an audited sample of the
// agreements so the router's false-agreement rate is measured, not assumed.
//
// Drive it with:
//
//	Main("mdash", Lease{Attempts: 2000, WallClock: 24 * time.Hour}, MDASH)
//
// Two baked gates. mdashClaimGate validates the router's claim file and emits
// the claim as a metric; it can establish nothing about the world, so it is
// format-only and its stages are clamped to Unverified. mdashProveGate is the
// real one: it stands up its own copy of the service on loopback and re-derives
// bob's note before believing anything.
const (
	mdashClaimGate Image = "dawn-mdash-claimgate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	mdashProveGate Image = "dawn-mdash-gate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// One digest-pinned environment image per instance: the instance IS the image,
// since the service's source is baked into it.
var mdashInstances = []Image{
	"dawn-mdash-stock@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	"dawn-mdash-patched@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	"dawn-mdash-notes-v2@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	"dawn-mdash-multitenant@sha256:0000000000000000000000000000000000000000000000000000000000000000",
}

// Opposed briefs: same evidence, opposite burden of proof.
var mdashBriefs = [2]string{
	`Audit the service at /srv/mdash. Make the strongest case you can that
GET /notes/{id} IS reachable across users. Then state your verdict as
{"exploitable": 1} or {"exploitable": 0} in your declared output.`,
	`Audit the service at /srv/mdash. Make the strongest case you can that
GET /notes/{id} is NOT reachable across users. Then state your verdict as
{"exploitable": 1} or {"exploitable": 0} in your declared output.`,
}

const mdashProvePrompt = `Log in to the service at /srv/mdash as alice (alice / alice-pw), then read
bob's private note (user id 2) using alice's own token, over HTTP. Write the
note verbatim into your declared output as {"secret": "..."}.`

// mdashAudit is how many agreements are re-checked against the oracle. The
// sample is the first N in instance order: a fixed rule, so a restart audits
// the same instances without a seed.
const mdashAudit = 3

// MDASH returns Unverified: the run's product is a rate in the run record and
// whatever findings the prove fan actuated, never a verdict of its own.
func MDASH(run *Scope) State {
	type agreement struct {
		env   Image
		claim float64
	}
	var agreed []agreement

	for _, env := range mdashInstances {
		scope := run.Scope(string(env), Lease{
			Attempts:         80,
			WallClock:        3 * time.Hour,
			AttemptWallClock: 10 * time.Minute,
		})
		routed := scope.Fan(2, func(i int) Stage {
			return Stage{
				ID:      fmt.Sprintf("route-%d", i),
				Agent:   ClaudeCode,
				Env:     env,
				Prompt:  mdashBriefs[i],
				Outputs: []string{"claim"},
				Gate:    FormatOnlyGate(mdashClaimGate),
			}
		})
		// dawn parses no CLI output, so the router's answer can only travel as
		// a number its gate wrote. A missing metric means no claim came back;
		// the oracle then decides instead of the router.
		a, aok := routed[0].Metric("exploitable")
		b, bok := routed[1].Metric("exploitable")
		if aok && bok && a == b {
			agreed = append(agreed, agreement{env, a})
			continue
		}
		mdashProve(run, scope, env)
	}

	sample := agreed
	if len(sample) > mdashAudit {
		sample = sample[:mdashAudit]
	}
	wrong := 0
	for _, g := range sample {
		scope := run.Scope("audit-"+string(g.env), Lease{
			Attempts:         80,
			WallClock:        3 * time.Hour,
			AttemptWallClock: 20 * time.Minute,
		})
		// A proof is one-sided: it can contradict an agreement that said "not
		// exploitable", and can never confirm one that said it is.
		if mdashProve(run, scope, g.env) && g.claim == 0 {
			wrong++
		}
	}
	if len(sample) > 0 {
		run.Record("false_agreement_rate", float64(wrong)/float64(len(sample)))
	}
	return Unverified
}

// mdashProve is the expensive oracle: 24 independent attempts under the only
// profile allowed to fan out. The fan drains and hands back every branch's
// terminal state; one passed branch is a proof, and only that branch's gate
// output is ever published.
func mdashProve(run *Scope, scope *Scope, env Image) bool {
	branches := scope.Fan(24, func(i int) Stage {
		return Stage{
			ID:      fmt.Sprintf("prove-%d", i),
			Agent:   ClaudeCode,
			Env:     env,
			Prompt:  mdashProvePrompt,
			Outputs: []string{"exploit"},
			Gate:    SoundGate(mdashProveGate),
		}
	})
	for _, r := range branches {
		if r.State == Passed {
			if err := r.Actuate(mdashFileFinding); err != nil {
				run.Record("actuation_failed", err.Error())
			}
			return true
		}
	}
	return false
}

// mdashFileFinding runs in dawn's process, after the gate voted, and can reach
// nothing but the bytes that gate published.
func mdashFileFinding(a *Actuation) error {
	_ = a.Key // carry into the tracker's own id: dedup upstream too
	_ = a.Published("finding.json")
	return nil
}
