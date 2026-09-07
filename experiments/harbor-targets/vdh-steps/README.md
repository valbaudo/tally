# vdh, translated to Harbor `[[steps]]`

The experiment behind Open Question 1 of
`docs/designs/dawn-as-the-it-agentic-mechasuit.md`: *is Harbor a dependency or a
competitor?* The doc assumed multi-stage sequencing was something Harbor might
one day grow. It already has it, in 0.22.0, the version dawn pins.

This directory is `protocols/vdh/main.go` expressed as one Harbor multi-step
task, as faithfully as the schema allows. `task.toml` is **validated against
Harbor's own `TaskConfig`** (`harbor.models.task.config`), not hand-checked.

## What survives — more than premise 2 claims

Harbor accepts all 24 steps and resolves **five distinct verifier images**, one
per gate kind:

| dawn concept | Harbor equivalent | Verified by |
|---|---|---|
| sequential stages | `[[steps]]`, `TaskConfig.steps` | schema accepts 24 |
| a different gate image per stage | `steps.verifier.environment.docker_image` | `resolve_effective_verifier_env_config` returns 5 distinct images |
| gate in a separate container | `environment_mode = "separate"` | `resolve_step_verifier_mode` |
| only a passing stage continues | `min_reward` → "abort remaining steps" | 21 of 24 steps |
| artifact transport | per-step `artifacts` → `steps/{name}/artifacts/` | schema |
| per-stage timeout, network policy | `steps.agent`, `steps.verifier` | schema |

Premise 2 says "multi-stage, per-stage isolated verification where only a sound
gate reaches success, plus artifact transport. Nobody ships the combination."
The dependency ships all four.

## What does not survive

**1. Fan.** No concurrency primitive anywhere in `trial/multi_step.py`. dawn's
8 stage kinds unroll to **24 static steps**, and they run strictly in series.

**2. Runtime-dependent width.** `validate` is `run.Fan(len(live), ...)` — as wide
as the number of hunters that actually handed bytes back. TOML hardcodes 3, so
**6 of the 24 steps are speculative**: they exist whether or not there is a
finding for them.

**3. Loops.** `for round := 0; round < rounds && run.More()` unrolled by hand.
Changing `hunters` or `rounds` in Go is one constant; here it regenerates the
file.

**4. Conditional stages.** The feedback round runs only
`if feedback.State.Decided() || len(feedback.Manifest) > 0`, and only
`if run.More()`. In TOML it always runs.

**5. Per-stage vendor — the one with no workaround.** `StepConfig.agent` carries
exactly `['network_mode', 'allowed_hosts', 'timeout_sec', 'user']`. No vendor
field, and `Trial` builds `self.agent` once (`trial/trial.py:967`). One Harbor
trial is one agent CLI. Running `validate` under a different model than `hunt`
is not expressible at any width.

**6. Ungated stages degrade to an UNSOUND verifier.** dawn marks recon, gapfill
and feedback `NoGate(reason)` and clamps them to `unverified`. Harbor has no
per-step "no verifier": `verifier.disable` lives on the *trial*. So those three
steps resolve to `environment_mode = "shared"` — a verifier running **inside the
agent's own container** (confirmed: `task_has_any_shared_verifier` is True, for
`['recon', 'gapfill-r0', 'feedback']`). That is exactly the arrangement dawn's
rule forbids: the artifact would be running inside the test. The alternative,
trial-level `verifier.disable`, would also disable the five sound gates.

**7. Selective input wiring.** `Stage.Inputs` names *which* prior results mount —
gapfill reads the adversaries' verdicts and not the hunters' raw findings, and
`protocols/vdh/main.go:126-131` argues that distinction is load-bearing. Harbor
steps share one environment and accumulate; there is no per-step selection from
named predecessors.

**8. Gate metrics driving control flow.** `if p, ok := v.Metric("proves"); ok && p == 1`
decides which findings survive validate. `min_reward` can only abort everything
downstream.

**9.** Early exhaustion (`if len(confirmed) == 0 { return dawn.Exhausted }`), the
attempt lease (`Dispatching(26, 20*time.Minute)` and `run.More()`), the six-state
model, resume across process restarts, and actuation. None have an analogue.

## Verdict

Harbor is a dependency, not a competitor, but for a narrower reason than the doc
assumed. It is not that Harbor lacks sequencing — it has it, with per-step
isolated verification, which is most of premise 2. It is that **a Harbor task is
a static, single-vendor, straight-line list**, and dawn's protocols are Go
programs: they fan, loop, branch on gate metrics, wire inputs by name, and mix
vendors across stages.

Premise 6's second half is the whole moat, and item 5 above is why.

## Reproduce

```sh
cd experiments/harbor-targets/vdh-steps
"$(dirname "$(command -v harbor)")/python" - <<'PY'
import tomllib
from harbor.models.task.config import TaskConfig
cfg = TaskConfig.model_validate(tomllib.load(open("task.toml","rb")))
print(len(cfg.steps), "steps accepted")
print("StepConfig.agent fields:", list(type(cfg.steps[1].agent).model_fields))
PY
```

Regenerate the tree with `scratch/gen_steps.py` semantics: 24 steps from
`hunters = 3`, `rounds = 2`, unrolled at worst-case fan width.

## Not run

This is a static-expressibility result, not a runtime one. No agent was
dispatched and no quota was spent. Every claim above is either the schema
accepting or rejecting a construct, or a cited line of installed Harbor 0.22.0
source. Whether the 24 steps would also *behave* like vdh is a separate
question this does not answer.
