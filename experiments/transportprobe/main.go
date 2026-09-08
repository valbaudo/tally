// Command transportprobe is the smallest two-stage protocol that can exist:
// stage one writes a word, stage two must read it back out of Stage.Inputs.
// It exists to answer one question with a real Harbor run rather than a unit
// test — does Harbor actually BUILD tally's generated environment/Dockerfile
// and does the earlier stage's artifact arrive in the later stage's container?
package main

import (
	"fmt"
	"time"

	"github.com/valbaudo/tally"
)

const env tally.Image = "tally-pr-ci-env@sha256:be5db68167d51bed314197f7aaf7015adbb037c56bf4e84c138daafab959d9d9"

func main() {
	tally.Main("transport-probe", tally.Dispatching(4, 6*time.Minute), protocol)
}

func protocol(run *tally.Scope) tally.State {
	make := run.Run(tally.Stage{
		ID: "make", Agent: tally.ClaudeCode, Env: env,
		Prompt:  "Write the single word 'quokka' (no quotes, no newline, nothing else) to the output file. Do nothing else.",
		Outputs: []string{"note.txt"},
		Gate:    tally.NoGate("transport probe: this stage only needs to produce bytes"),
	})
	run.Record("make_state", string(make.State))
	run.Record("make_manifest", make.Manifest)

	read := run.Run(tally.Stage{
		ID: "read", Agent: tally.ClaudeCode, Env: env,
		Prompt:  "Read the input file you were given and copy its exact contents into the output file. Do not invent content: if the input file does not exist, write the literal text NO-INPUT instead.",
		Inputs:  []tally.Result{make},
		Outputs: []string{"echo.txt"},
		Gate:    tally.NoGate("transport probe: the answer is read off disk, not gated"),
	})
	run.Record("read_state", string(read.State))
	run.Record("read_manifest", read.Manifest)
	fmt.Println("probe done")
	return read.State
}
