# TRACE — every construct, and the protocol that forced it

Derived from four protocols drafted independently: **cybergym** (sequential
attempts, sound oracle), **mdash** (opposed-brief routing, 24-wide fan,
actuator), **vdh** (outer loop, fan-in, no sound oracle), **pr-ci** (one stage,
sound gate, external mutation).

`port_test.go` is the check: it ports all four onto this surface and is
compiled by `go test`. A missing construct breaks the build; a construct no
port uses has no forcing protocol and is deleted, not justified.

## Kept

| Construct | Forced by | Why it survives |
|---|---|---|
| `Main(name, root Lease, protocol func(*Scope) State)` | all four | Something must own the journal and the restart sweep, so dawn calls the protocol, not the reverse. The root lease is the only bound on a run whose loop may never exit. |
| `Scope` + `Scope.Scope(id, Lease)` | cybergym (two ceilings), mdash (per-instance escalation) | The lease is the only thing that bounds a retry loop, and a bound that isn't nestable can't isolate one instance's blowup from the run. |
| `Lease{Attempts, WallClock, AttemptWallClock}` | cybergym | Exactly the three numbers an author may declare. Per-attempt clock is separate because a wedged agent must not eat the scope. |
| `Scope.More() bool` | cybergym, vdh | "Try again until the scope runs out" is the one question a protocol asks the admission queue. It also keeps a spent lease from being spelled `Exhausted`, which already means one attempt hit its clock. |
| `Scope.Run(Stage) Result` | all four | One attempt. Dispatch goes through the scope or the lease is decorative. |
| `Scope.Fan(n, mk) []Result` | mdash (2 and 24 wide), vdh (4 wide) | Drains, never fail-fast, returns every child in index order — a fan-in that drops children is a corpus that shrank silently. |
| `Scope.Record(name, value)` | mdash (false-agreement rate), vdh (mandatory caveats) | The channel for what no gate can write: dawn's arithmetic across attempts, and the statements a protocol with no sound oracle owes its reader. Unions mdash's `Record` and vdh's `Caveat`. |
| `Stage{ID, Agent, Env, Prompt, Inputs, Outputs, Gate}` | all four | Seven fields, each of them something dawn cannot know. `ID` is per-branch because it is hashed into `attempt_id`. |
| `Stage.Inputs []Result` | cybergym (notes → pov), vdh (fan-in) | Every attempt is a fresh container, so orientation must survive as bytes. dawn mounts the inputs' artifacts read-only plus their manifest. |
| `Stage.Outputs []string` | all four | Logical names from dawn's list — **names only**. Paths are dawn's. |
| `Agent` + `ClaudeCode` / `Codex` | all four; `FanOut` by vdh | A profile is a fact about a pinned image, not a knob. `FanOut` is read so a protocol can say in source that its fan is single-vendor. |
| `Image` (digest-pinned ref) | cybergym, pr-ci | The soundness argument is an argument about which bytes are in which image. |
| `Gate` + `SoundGate` / `FormatOnlyGate` / `NoGate(reason)` | cybergym (`SoundGate`, `NoGate`), mdash + vdh (`FormatOnlyGate`) | Soundness declared statically at the call site: no field to forget, no image without a claim, no ungated stage without a reason. |
| `State` + the six constants | all four | The closed set is the entire branching vocabulary. cybergym branches on `Rejected`, mdash and vdh on `Unverified`, pr-ci on `Passed`. |
| `Result.Metric(name) (float64, bool)` | mdash | dawn parses no CLI output, so a gate's number is the only thing a stage can say. `ok` because a false agreement between two absent metrics is a real bug. Subsumes `reward`. |
| `Manifest` / `Artifact` | cybergym (handoff), vdh (dry-round detection) | The record of what crossed a boundary. Its digests are what let vdh decide a round changed nothing without reading agent output. |
| `Result.Actuate(func(*Actuation) error)` | pr-ci, mdash | Fires only on `Passed`, runs in dawn's process where the credentials are. A closure, not a named endpoint. |
| `Actuation.Key` | pr-ci | The `attempt_id`. dawn dedupes its own retries; carrying the key into the branch name is how the far side gets idempotent too. |
| `Actuation.Published(name)` | pr-ci | The only door onto `/logs/verifier/publish/`. The agent's raw output is unreachable from an actuator, and that is the whole safety argument. |

## Cut

| Cut | Where it came from | Why |
|---|---|---|
| `Output{Name, Path}` / the path half of a declared output | cybergym, pr-ci | dawn owns the artifacts list. The author transcribing `/app/pov.bin` from inside a baked gate image is a permanent `infra_error` waiting for a typo. Names only. |
| `dawn.Target(path)` / `target.CoW()` | mdash, vdh | Two names for "what the agent starts from", and mdash itself flagged CoW as probably a no-op — Harbor already gives every trial a fresh container. Folded into `Stage.Env Image`, which is also the more reproducible half of the disagreement. |
| `Stage.Attempts`, `Stage.Timeout` | pr-ci, vdh | The same two numbers the `Lease` already holds. A one-stage protocol writes a one-stage scope. |
| `MaxAttempts(n)` / `MaxWallClock(d)` options | mdash | Functional options for a three-field struct. Struct wins. |
| `Soundness` field + `Sound` / `FormatOnly` constants + a separate `NoGate` field | mdash, vdh, pr-ci | Three constructors express the same thing and make the illegal states unrepresentable. |
| `dawn.InputDigest([]Result)` | vdh | The `Manifest` already carries the digests; comparing them is four lines of author arithmetic, not API. |
| `Result.Score` / `.Reward` | — | `Metric("reward")`. cybergym never read a score at all: the strongest evidence that score-as-metric costs the surface nothing. |
| `run.Seed` | mdash | A run-varying seed is not forced: any author constant gives restart determinism, which is the property recovery actually needs. |
| A separate `Run` / `Protocol` type | cybergym, mdash, vdh, pr-ci (four different spellings) | The run *is* the root scope. One type instead of two, and the run-wide lease stops being a special case. |
| Named actuator registry (`Actuate(r, "finding-tracker")`) | mdash | A closure is the registry, and it needs no second construct to look names up in. |
| `MaxChildren`, a budget ledger, a token or dollar ceiling, an `artifacts` list, an exit classifier | forbidden by the settled decisions | None was reinvented here, and none is expressible. |

## Where the four disagreed

1. **Who owns the output path.** cybergym and pr-ci wrote absolute paths; vdh wrote a logical name only. Resolved for vdh: the author names, dawn maps. The gate image's expected path belongs to the image.
2. **Where the per-attempt clock lives.** cybergym and mdash put it on the scope; vdh and pr-ci on the stage. Resolved to the scope — a stage looped eight times should not restate its timeout eight times.
3. **Environment: pinned image or Harbor task dir.** Resolved to the pinned image. A protocol that cannot name the exact bytes of its input tree is not reproducible, and pr-ci's friction note (Harbor rebuilds the env image per run) is a hole to close in the runner, not to paper over in the surface.
4. **Soundness: call-site constructor or struct field.** Resolved to cybergym's constructors: a field can be forgotten, a constructor cannot.
5. **Actuator: closure or named endpoint.** Resolved to pr-ci's closure.

## Two frictions this surface does not resolve

- **Attempt lease is both an intent budget and a flake budget.** `infra_error` retries spend the same counter as deliberate attempts (cybergym #3). Left as one number on purpose: splitting it is a second ledger under another name.
- **A fan's width and its scope's `Attempts` are declared apart** (mdash #4). `More()` guards a loop; nothing guards a 24-wide `Fan` against a 20-attempt lease except a loud abort at dispatch.
