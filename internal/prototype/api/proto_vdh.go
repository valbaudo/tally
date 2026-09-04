package api

import (
	"fmt"
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
//	Main("vdh", Lease{Attempts: 24, WallClock: 6 * time.Hour, AttemptWallClock: 30 * time.Minute}, VDH)
const (
	vdhEnv  Image = "dawn-vdh-env@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	vdhGate Image = "dawn-vdh-gate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// vdh seeds one bug class four times by four splicing mechanisms. The mechanism
// is the thing a hunter can specialise on, so it is the fan's axis.
var vdhClasses = []string{"concat", "percent", "fstring", "dotformat"}

// vdhDryStop: rounds that change nothing before the loop gives up.
const vdhDryStop = 2

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
// bugs are real".
func VDH(run *Scope) State {
	// Only claude-code may fan out, so no second vendor can mirror the hunt.
	// The author reads that fact in source rather than discovering it at
	// dispatch.
	if !Codex.FanOut {
		run.Record("caveat", "cross-vendor decorrelation lost: every hunt in this run is claude-code, so a blind spot shared by all four hunters is invisible")
	}
	run.Record("caveat", "coverage unknown: the gate checks that citations resolve, not that findings are correct; no stage can reach passed and no count of dry rounds proves the repo is clean")

	recon := run.Run(Stage{
		ID:      "recon",
		Agent:   ClaudeCode,
		Env:     vdhEnv,
		Prompt:  vdhReconPrompt,
		Outputs: []string{"notes"},
		Gate:    NoGate("orientation only: where to look is not checkable without a sound oracle"),
	})

	carry := []Result{recon}
	prev := vdhCorpus(carry)
	for round, dry := 1, 0; run.More() && dry < vdhDryStop; round++ {
		hunts := run.Fan(len(vdhClasses), func(i int) Stage {
			return Stage{
				ID:      fmt.Sprintf("hunt-r%d-%s", round, vdhClasses[i]),
				Agent:   ClaudeCode,
				Env:     vdhEnv,
				Prompt:  vdhHuntPrompt(vdhClasses[i]),
				Inputs:  carry,
				Outputs: []string{"findings"},
				Gate:    FormatOnlyGate(vdhGate),
			}
		})

		vals := run.Fan(len(vdhClasses), func(i int) Stage {
			return Stage{
				ID:      fmt.Sprintf("validate-r%d-%s", round, vdhClasses[i]),
				Agent:   ClaudeCode,
				Env:     vdhEnv,
				Prompt:  vdhValidatePrompt(vdhClasses[i]),
				Inputs:  vdhSurviving(hunts),
				Outputs: []string{"findings"},
				Gate:    FormatOnlyGate(vdhGate),
			}
		})
		carry = vdhSurviving(vals)

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
// never on a score — an infra_error or exhausted child drops out loudly instead
// of silently shrinking the corpus.
func vdhSurviving(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.State == Unverified {
			out = append(out, r)
		}
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
