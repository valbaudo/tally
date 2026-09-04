# TRACE — every construct, and what forced it: a protocol, or a settled decision

Derived from four protocols drafted independently: **cybergym** (sequential
attempts, sound oracle), **mdash** (opposed-brief routing, 24-wide fan,
actuator), **vdh** (outer loop, fan-in, no sound oracle), **pr-ci** (one stage,
sound gate, external mutation).

`port_test.go` is the check: it ports all four onto this surface and is
compiled by `go test`. A missing construct breaks the build.

A construct no port uses is not automatically dead. It survives only if a
**settled decision** forces it, and then this table names that decision instead
of naming a protocol. It is deleted if neither a protocol nor a decision forces
it. Attributing an unforced construct to a protocol that never touches it is
itself a violation of the rule, so the "Forced by" column below distinguishes
the two, and says which protocol *reads* a construct as opposed to merely
carrying it.

## Kept — forced by a protocol

| Construct | Forced by | Why it survives |
|---|---|---|
| `Main(name, root Lease, protocol func(*Scope) State)` | all four | Something must own the journal and the restart sweep, so dawn calls the protocol, not the reverse. The root lease is the only bound on a run whose loop may never exit. |
| `Scope` + `Scope.Scope(id, Lease)` | cybergym (two ceilings), mdash (per-instance escalation), vdh (one scope per round) | The lease is the only thing that bounds a retry loop, and a bound that isn't nestable can't isolate one instance's blowup from the run — or fund a round of eight in one piece. |
| `Lease{Attempts, WallClock, AttemptWallClock}` | cybergym | Exactly the three numbers an author may declare. Per-attempt clock is separate because a wedged agent must not eat the scope. |
| `Scope.More() bool` | cybergym (loop guard), vdh (clock guard) | "Try again until the scope runs out" is the one question a protocol asks the admission queue. It answers for **one** attempt, which is why vdh bounds its rounds by count and uses `More()` only for the clock. |
| `Scope.Run(Stage) Result` | all four | One attempt. Dispatch goes through the scope or the lease is decorative. |
| `Scope.Fan(n, mk) []Result` | mdash (2 and 24 wide), vdh (4 wide) | Drains, never fail-fast, returns every child in index order — a fan-in that drops children is a corpus that shrank silently. Both protocols now record every child's state, which is what makes the drain observable rather than merely promised. |
| `Scope.Record(name, value any)` | mdash (rate, per-fan states, oracle verdicts), vdh (caveats, per-fan states, void rounds) | The channel for what no gate can write. `any`, not `float64`, because an undefined rate has to be able to say "undefined" rather than be omitted. |
| `Stage{ID, Agent, Env, Prompt, Inputs, Gate}` | all four | Six fields, each of them something dawn cannot know. `ID` is per-branch because it is hashed into `attempt_id`. |
| `Stage.Inputs []Result` | cybergym (notes → pov), vdh (fan-in) | Every attempt is a fresh container, so orientation must survive as bytes. dawn mounts the inputs' artifacts read-only plus their manifest. |
| `Image` (digest-pinned ref) | cybergym, mdash, pr-ci, vdh (`Stage.Env`, gate constructors) | The soundness argument is an argument about which bytes are in which image. |
| `Gate` + `SoundGate` / `FormatOnlyGate` / `NoGate(reason)` | cybergym (`SoundGate`, `NoGate`), mdash + vdh (`FormatOnlyGate`) | Soundness declared statically at the call site: no field to forget, no image without a claim, no ungated stage without a reason. |
| `Agent` + `ClaudeCode` / `Codex` | all four (`Stage.Agent`) | A profile is a value an author selects. Its name and CLI image are **unexported**: dawn's knowledge, never the author's vocabulary — no protocol read either, and a field nothing reads is not surface. |
| `Agent.FanOut` | vdh **reads it**, into the caveat string | vdh's caveat is only checkable if it quotes the profile's actual value. It is deliberately not branched on: `if !Codex.FanOut` was a constant dressed as a runtime check. |
| `Passed` | pr-ci (branches), mdash (branches) | The only state that actuates. |
| `Rejected` | cybergym (branches: the one state worth another attempt) | |
| `Unverified` | mdash, vdh (branch and return), cybergym (checks its ungated stage reached it) | The success state of an ungated or format-only stage. |
| `Exhausted` | cybergym **returns it** | A pov loop that never runs — the root clock spent during `study` — has no gate verdict to report. Exhausted is dawn's clock ending it, one step up from an attempt. Before this it returned `State("")`: a seventh state. |
| `InfraError` | pr-ci (a passed stage that could not publish), mdash (nothing anywhere voted) | |
| `Result.Metric(name) (float64, bool)` | mdash | dawn parses no CLI output, so a gate's number is the only thing a stage can say. `ok` because a false agreement between two absent metrics is a real bug. Subsumes `reward`. |
| `Manifest` / `Artifact{Name, Digest}` | cybergym (handoff), vdh (dry-round detection **reads both fields**) | The record of what crossed a boundary. Two fields, and vdh reads both. |
| `Result.Actuate(func(*Actuation) error)` | pr-ci, mdash | Fires only on `Passed`, runs in dawn's process where the credentials are. A closure, not a named endpoint. |
| `Actuation.Key` | pr-ci, mdash | The `attempt_id`. dawn dedupes its own retries; carrying the key into the branch name is how the far side gets idempotent too. |
| `Actuation.Published(name)` | pr-ci, mdash | The only door onto `/logs/verifier/publish/`. The agent's raw output is unreachable from an actuator, and that is the whole safety argument. |

## Kept — forced by a settled decision, not by a protocol

Each of these is read by **zero** protocols. Each names the decision that keeps
it, and none is justified by a protocol that does not touch it.

| Construct | Forced by | Why deleting it would break a decision |
|---|---|---|
| `Cancelled` | *Six states, exactly* — cancel is classification rule 1 | No protocol names it; cybergym, vdh and pr-ci all *propagate* it by returning a stage's state unread. Deleting it would leave external cancel with no state, which is the seventh-state bug in the other direction. |
| `Agent`'s unexported `name` / `image` | *Per-agent concurrency is fixed by the profile as a correctness fact* | dawn must know which pinned image a profile drives. It stays unexported precisely because nothing in the author's vocabulary needs it. |

## Cut

| Cut | Where it came from | Why |
|---|---|---|
| `Stage.Outputs []string` | previously claimed forced by "all four" | It was the author restating dawn's list. Every one of the four targets declares **exactly one** artifact path in its `task.toml`, while the protocols declared two logical names apiece — a declaration that could not map onto what the runner collects, and could only ever disagree with it. `Stage.Env` names the task and the task names its artifacts, so there is nothing left for the author to say. This is the same cut as `Output{Name, Path}`, carried to its conclusion. |
| `Output{Name, Path}` / the path half of a declared output | cybergym, pr-ci | dawn owns the artifacts list. The author transcribing `/app/pov.bin` from inside a baked gate image is a permanent `infra_error` waiting for a typo. |
| `Artifact.Path`, `.Size`, `.StageID`, `.Attempt` | — | Read by no protocol and forced by no decision. Paths are dawn's (already cut above); size is dawn's oversized→`rejected` rule, not the author's; the producing stage and attempt are dawn's bookkeeping. Deleted rather than justified. |
| `Agent.Name` / `Agent.Image` as exported fields | — | Set by dawn, read by nobody. Unexported, not deleted, because the profile-is-a-pinned-image fact is a settled decision — but an author-facing field nothing reads is not surface. |
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

1. **Who owns the output path.** cybergym and pr-ci wrote absolute paths; vdh wrote a logical name only. Resolved harder than either: the author writes **neither**. Naming `Env` names the task, and the task's `artifacts` list is dawn's.
2. **Where the per-attempt clock lives.** cybergym and mdash put it on the scope; vdh and pr-ci on the stage. Resolved to the scope — a stage looped eight times should not restate its timeout eight times.
3. **Environment: pinned image or Harbor task dir.** Resolved to the pinned image. A protocol that cannot name the exact bytes of its input tree is not reproducible, and pr-ci's friction note (Harbor rebuilds the env image per run) is a hole to close in the runner, not to paper over in the surface.
4. **Soundness: call-site constructor or struct field.** Resolved to cybergym's constructors: a field can be forgotten, a constructor cannot.
5. **Actuator: closure or named endpoint.** Resolved to pr-ci's closure.

## Frictions this surface does not resolve

- **Attempt lease is both an intent budget and a flake budget.** `infra_error` retries spend the same counter as deliberate attempts (cybergym #3). Left as one number on purpose: splitting it is a second ledger under another name.
- **A fan's width and its scope's `Attempts` are declared apart** (mdash #4). `More()` answers for one attempt; a fan spends `n`. vdh works around it by giving each round its own scope leased at exactly one round's cost and bounding the loop by round count — which is author arithmetic (`vdhAttempts = 1 + vdhRounds*vdhRoundCost`), readable in source but unchecked by the surface. mdash's 24-wide fan under an 80-attempt scope has the same gap and no such arithmetic. A `More(n)` would close it; nothing yet forces the second signature.
- **`attempt_id` carries no scope component.** `hash(stage_id, content digest, input digest, attempt_number)` means two stages with the same `ID` in different scopes are one stage to recovery unless their environments or inputs happen to differ. mdash previously relied on exactly that accident (`prove-%d` under both the routing and the audit scope); the stage ids are now phase-qualified so nothing depends on it. That is a discipline the surface documents on `Stage.ID` and does not enforce.
