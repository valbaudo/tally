package api

import (
	"fmt"
	"time"
)

// MDASH: a cheap opposed-brief router over mdash instances, an expensive
// 24-wide prove fan whenever the briefs disagree, and an audited sample of the
// agreements the oracle can actually contradict, so the router's
// false-agreement rate is measured, not assumed.
//
// The oracle is one-sided — it can only ever show that something IS reachable
// — so only the agreements claiming "not exploitable" are auditable, and only
// those are in the rate's denominator.
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

// mdashAudit is how many auditable agreements are re-checked against the
// oracle. The sample is the first N in instance order: a fixed rule, so a
// restart audits the same instances without a seed.
const mdashAudit = 3

// MDASH returns Unverified — the run's product is a rate in the run record and
// whatever findings the prove fan actuated, never a verdict of its own — but
// only when at least one gate anywhere actually voted. Unverified is this
// protocol's SUCCESS state, so returning it after a run in which every stage
// was infra_error would make catastrophe and measurement identical.
func MDASH(run *Scope) State {
	type agreement struct {
		env   Image
		claim float64
	}
	var agreed []agreement
	observed := false

	for _, env := range mdashInstances {
		scope := run.Scope(string(env), Lease{
			Attempts:         80,
			WallClock:        3 * time.Hour,
			AttemptWallClock: 10 * time.Minute,
		})
		routed := scope.Fan(2, func(i int) Stage {
			return Stage{
				ID:     fmt.Sprintf("route-%d", i),
				Agent:  ClaudeCode,
				Env:    env,
				Prompt: mdashBriefs[i],
				Gate:   FormatOnlyGate(mdashClaimGate),
			}
		})
		run.Record("route-states-"+string(env), mdashStates(routed))
		// dawn parses no CLI output, so the router's answer can only travel as
		// a number its gate wrote. A missing metric means no claim came back;
		// the oracle then decides instead of the router.
		a, aok := routed[0].Metric("exploitable")
		b, bok := routed[1].Metric("exploitable")
		if aok && bok {
			observed = true
			if a == b {
				agreed = append(agreed, agreement{env, a})
				continue
			}
		}
		// Disagreement, or a router that produced no claim at all. The
		// oracle's answer is this instance's only real finding, so it is
		// recorded: discarding it left the expensive half of the protocol
		// with no trace in the run record.
		proved, ran := mdashProve(run, scope, env, "route")
		run.Record("proved-"+string(env), proved)
		observed = observed || ran
	}

	// A proof is one-sided: it can contradict an agreement that said "not
	// exploitable" and can never confirm one that said it is. So an agreement
	// with claim 1 is not auditable at all, and counting it in the denominator
	// while it can never enter the numerator only dilutes the rate. The
	// denominator is the auditable agreements, and the rest are reported as
	// what they are.
	var auditable []agreement
	for _, g := range agreed {
		if g.claim == 0 {
			auditable = append(auditable, g)
		}
	}
	run.Record("agreements", len(agreed))
	run.Record("unauditable_agreements", len(agreed)-len(auditable))

	sample := auditable
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
		proved, ran := mdashProve(run, scope, g.env, "audit")
		observed = observed || ran
		if proved {
			wrong++
		}
	}
	// The name is always in the record. An omitted rate is indistinguishable
	// from a rate of zero to anyone reading the run afterwards, so an
	// undefined one says so in the value.
	run.Record("audited", len(sample))
	if len(sample) == 0 {
		run.Record("false_agreement_rate", "undefined: no agreement claimed not-exploitable, so nothing was auditable")
	} else {
		run.Record("false_agreement_rate", float64(wrong)/float64(len(sample)))
	}

	if !observed {
		return InfraError
	}
	return Unverified
}

// mdashProve is the expensive oracle: 24 independent attempts under the only
// profile allowed to fan out. The fan drains and hands back every branch's
// terminal state; one passed branch is a proof, and only that branch's gate
// output is ever published.
//
// phase qualifies the stage ids. attempt_id hashes no scope path, so
// "prove-7" under the routing scope and "prove-7" under the audit scope are
// one stage to recovery; today they merely happen never to run against the
// same env. "route-prove-7" and "audit-prove-7" do not depend on that.
//
// ran reports whether any branch's gate voted at all, so the caller can tell
// "the oracle found nothing" from "the oracle never ran".
func mdashProve(run *Scope, scope *Scope, env Image, phase string) (proved, ran bool) {
	branches := scope.Fan(24, func(i int) Stage {
		return Stage{
			ID:     fmt.Sprintf("%s-prove-%d", phase, i),
			Agent:  ClaudeCode,
			Env:    env,
			Prompt: mdashProvePrompt,
			Gate:   SoundGate(mdashProveGate),
		}
	})
	run.Record("prove-states-"+phase+"-"+string(env), mdashStates(branches))
	for _, r := range branches {
		if r.State == Passed {
			if err := r.Actuate(mdashFileFinding); err != nil {
				run.Record("actuation_failed", err.Error())
			}
			return true, true
		}
		ran = ran || r.State == Rejected
	}
	return false, ran
}

// mdashStates is every child of a drained fan, in index order: the record of
// what the fan-in dropped, without which a shrinking corpus is invisible.
func mdashStates(rs []Result) []State {
	out := make([]State, len(rs))
	for i, r := range rs {
		out[i] = r.State
	}
	return out
}

// mdashFileFinding runs in dawn's process, after the gate voted, and can reach
// nothing but the bytes that gate published.
func mdashFileFinding(a *Actuation) error {
	_ = a.Key // carry into the tracker's own id: dedup upstream too
	_ = a.Published("finding.json")
	return nil
}
