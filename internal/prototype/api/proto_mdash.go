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
// only when at least one gate anywhere ran and wrote something dawn could
// read. Unverified is this protocol's SUCCESS state, so returning it after a
// run in which every stage was infra_error would make catastrophe and
// measurement identical. A cancel from outside is propagated as itself.
func MDASH(run *Scope) State {
	type agreement struct {
		env   Image
		claim float64
	}
	var agreed []agreement
	observed := false

	routed, claimed := 0, 0
	for _, env := range mdashInstances {
		// The root lease is the run's only bound, and dispatching past it
		// unwinds the run — destroying the rate already measured. Stopping
		// short is recorded, because a truncated denominator that says nothing
		// is a smaller number reported as the same measurement.
		if !run.More() {
			run.Record("routing-stopped-early", fmt.Sprintf("root lease spent after %d of %d instances", routed, len(mdashInstances)))
			break
		}
		scope := run.Scope(string(env), Lease{
			Attempts:         80,
			WallClock:        3 * time.Hour,
			AttemptWallClock: 10 * time.Minute,
		})
		branches := scope.Fan(2, func(i int) Stage {
			return Stage{
				ID:     fmt.Sprintf("route-%s-%d", env, i),
				Agent:  ClaudeCode,
				Env:    env,
				Prompt: mdashBriefs[i],
				Gate:   FormatOnlyGate(mdashClaimGate),
			}
		})
		run.Record("route-states-"+string(env), mdashStates(branches))
		routed++
		if mdashCancelled(branches) {
			return Cancelled
		}
		// dawn parses no CLI output, so the router's answer can only travel as
		// a number its gate wrote. A missing metric means no claim came back;
		// the oracle then decides instead of the router.
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
		// Disagreement, or a router that produced no claim at all. The
		// oracle's answer is this instance's only real finding, so it is
		// recorded — as the oracle's own state, because "the oracle voted no"
		// and "the oracle never voted" are not the same finding and a bare
		// false said both.
		v := mdashProve(run, scope, env, "route")
		if v == Cancelled {
			return Cancelled
		}
		run.Record("oracle-route-"+string(env), v)
		observed = observed || v != InfraError
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
	// Every count the rate hangs off, with the population it was drawn from:
	// "2 agreements" over four declared instances and over two that actually
	// produced a pair of claims are different measurements.
	run.Record("instances_routed", routed)
	run.Record("instances_with_both_claims", claimed)
	run.Record("agreements", len(agreed))
	run.Record("unauditable_agreements", len(agreed)-len(auditable))

	sample := auditable
	if len(sample) > mdashAudit {
		sample = sample[:mdashAudit]
	}
	wrong, audited := 0, 0
	for _, g := range sample {
		if !run.More() {
			run.Record("audit-stopped-early", fmt.Sprintf("root lease spent after %d of %d sampled agreements", audited, len(sample)))
			break
		}
		scope := run.Scope("audit-"+string(g.env), Lease{
			Attempts:         80,
			WallClock:        3 * time.Hour,
			AttemptWallClock: 20 * time.Minute,
		})
		v := mdashProve(run, scope, g.env, "audit")
		if v == Cancelled {
			return Cancelled
		}
		run.Record("oracle-audit-"+string(g.env), v)
		observed = observed || v != InfraError
		// An agreement the oracle never tested is not an agreement the oracle
		// failed to contradict. Counting it would publish the strongest
		// possible claim about the router out of a fan that never voted, so
		// the denominator is audits PERFORMED, not audits dispatched.
		if v == InfraError {
			continue
		}
		audited++
		if v == Passed {
			wrong++
		}
	}
	// The name is always in the record. An omitted rate is indistinguishable
	// from a rate of zero to anyone reading the run afterwards, so an
	// undefined one says so in the value — and says only what it measured,
	// since "nothing was auditable" and "nothing got audited" are different
	// runs and the counts above tell them apart.
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
}

// mdashProve is the expensive oracle: 24 independent attempts under the only
// profile allowed to fan out. The fan drains and hands back every branch's
// terminal state; one passed branch is a proof, and only that branch's gate
// output is ever published.
//
// It returns the ORACLE's state, from the same six: Passed (a branch proved
// it), Rejected (the oracle voted and found nothing), Cancelled (the run was
// cancelled underneath it), InfraError (no branch ever voted). A bool pair
// could not say the fourth without the caller discarding half of it, and the
// caller always did.
//
// Exhausted is not a vote: dawn's clock ended that attempt before a verdict
// was recorded, which is the same evidence as no attempt at all.
//
// Both phase AND env qualify the stage ids. attempt_id hashes no scope path,
// so "prove-7" is one stage to recovery everywhere it appears — across the two
// phases and, worse, across the four instances, whose env digests are the only
// other thing in that hash and which a run may pin identically.
func mdashProve(run *Scope, scope *Scope, env Image, phase string) State {
	branches := scope.Fan(24, func(i int) Stage {
		return Stage{
			ID:     fmt.Sprintf("%s-%s-prove-%d", phase, env, i),
			Agent:  ClaudeCode,
			Env:    env,
			Prompt: mdashProvePrompt,
			Gate:   SoundGate(mdashProveGate),
		}
	})
	run.Record("prove-states-"+phase+"-"+string(env), mdashStates(branches))
	verdict := InfraError
	for i, r := range branches {
		switch r.State {
		case Passed:
			if err := r.Actuate(mdashFileFinding); err != nil {
				// Qualified by everything that varies: this runs up to seven
				// times a run, and one name would leave every failure but the
				// last unrecorded — the one thing that cannot be re-derived
				// from the states above.
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

// mdashCancelled reports whether a drained fan contains a cancelled child, so
// the protocol can stop rather than spend the rest of the lease dispatching
// into a cancelled run and then reporting infra_error for it.
func mdashCancelled(rs []Result) bool {
	for _, r := range rs {
		if r.State == Cancelled {
			return true
		}
	}
	return false
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
