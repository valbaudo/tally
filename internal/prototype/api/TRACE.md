# TRACE — every construct, and what forced it: a protocol, or a settled decision

Derived from four protocols drafted independently: **cybergym** (sequential
attempts, sound oracle), **mdash** (opposed-brief routing, 24-wide fan,
actuator), **vdh** (outer loop, fan-in, no sound oracle), **pr-ci** (one stage,
sound gate, external mutation).

`port_test.go` is the check: it ports all four onto this surface and is
compiled by `go test` and `go vet` — never by `go build`, which skips
`_test.go` files. A missing construct breaks that compile. It can prove
sufficiency only; nothing in Go fails because a construct went unused, so
minimality is checked by this table and by nothing else.

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
| `Main(name, root Lease, protocol func(*Scope) State)` | all four | Something must own the journal and the restart sweep, so dawn calls the protocol, not the reverse. The root lease is the only bound on a run whose loop may never exit. The returned `State` is the run's own terminal state, which dawn records and exits on — the one consumer, and the reason all four spend care on telling a measurement from a catastrophe. |
| `Scope` + `Scope.Scope(id, Lease)` | cybergym (two ceilings), mdash (per-instance escalation), vdh (one scope per round) | The lease is the only thing that bounds a retry loop, and a bound that isn't nestable can't isolate one instance's blowup from the run — or fund a round of eight, plus the flakes those eight will owe, in one piece. |
| `Lease{Attempts, WallClock, AttemptWallClock}` | cybergym | Exactly the three numbers an author may declare. Per-attempt clock is separate because a wedged agent must not eat the scope. Every lease in the package is now declared as **work plus headroom** and says which is which in source: an `infra_error` retry spends the same counter and the same clock as a deliberate attempt, so a lease sized to exactly its work cannot pay for one flake. |
| `Scope.More() bool` | cybergym (loop guard), vdh (clock guard), mdash (guards both loops against the root lease) | "Try again until the scope runs out" is the one question a protocol asks the admission queue. It answers for **one** attempt, which is why vdh bounds its rounds by count and uses `More()` only for the clock, and why mdash records how many instances it got through when it stops short. |
| `Scope.Run(Stage) Result` | cybergym, vdh (recon), pr-ci | One attempt. Dispatch goes through the scope or the lease is decorative. mdash does not read it — every mdash dispatch is a fan — and saying "all four" here was attributing a construct to a protocol that never touches it. |
| `Scope.Fan(n, mk) []Result` | mdash (2 and 24 wide), vdh (4 wide) | Drains, never fail-fast, returns every child in index order — a fan-in that drops children is a corpus that shrank silently. Both protocols now record every child's state, which is what makes the drain observable rather than merely promised. |
| `Scope.Record(name, value any)` | mdash (rate and its populations, per-fan states, oracle verdicts, failed actuations), vdh (two caveats, per-fan states, incomplete rounds, why the loop stopped), pr-ci (failed actuation) | The channel for what no gate can write. `any`, not `float64`, because an undefined rate has to be able to say "undefined" rather than be omitted — and because an oracle verdict is a `State`, not a bool: `false` said both "voted no" and "never voted". A name is written **once per run**; vdh's two caveats and mdash's per-branch actuation failures are the reason that is now stated on the method and every name is qualified by whatever varies. |
| `Stage{ID, Agent, Env, Prompt, Inputs, Gate}` | all four | Six fields, each of them something dawn cannot know. `ID` is per-branch because it is hashed into `attempt_id`. |
| `Stage.Inputs []Result` | cybergym (notes → pov), vdh (fan-in) | Every attempt is a fresh container, so orientation must survive as bytes. dawn mounts the inputs' artifacts read-only plus their manifest. |
| `Image` (digest-pinned ref) | cybergym, mdash, pr-ci, vdh (`Stage.Env`, gate constructors) | The soundness argument is an argument about which bytes are in which image. |
| `Gate` + `SoundGate` / `FormatOnlyGate` / `NoGate(reason)` | cybergym (`SoundGate`, `NoGate`), mdash + vdh (`FormatOnlyGate`) | Soundness declared statically at the call site: no field to forget, no image without a claim, no ungated stage without a reason. |
| `Agent` + `ClaudeCode` / `Codex` | all four (`Stage.Agent`) | A profile is a value an author selects. Its name and CLI image are **unexported**: dawn's knowledge, never the author's vocabulary — no protocol read either, and a field nothing reads is not surface. |
| `Agent.FanOut` | vdh **reads it**, into the caveat string | vdh's caveat quotes the profile's actual value rather than asserting it beside the code, and the "one profile hunts here" half is read from the same `hunter` variable that configures both fans — so changing the profile changes the caveat. It is deliberately not branched on: `if !Codex.FanOut` was a constant dressed as a runtime check. |
| `Passed` | pr-ci (branches), mdash (branches) | The only state that actuates. |
| `Cancelled` | mdash and vdh **branch on it**; cybergym and pr-ci propagate it by returning a stage's state unread | A fan that comes back cancelled must stop the run, not be read as a round that found nothing: vdh would otherwise dispatch five more rounds into a cancelled run and report its success state, and mdash would fire 96 more prove attempts and report `infra_error`. |
| `Rejected` | cybergym (branches: the one state worth another attempt), mdash (the oracle voted and found nothing — distinct from never having voted) | |
| `Unverified` | mdash, vdh (branch and return), cybergym (checks its ungated stage reached it) | The success state of an ungated or format-only stage. |
| `Exhausted` | cybergym **returns it**, vdh **returns it** | A loop that never runs — cybergym's pov because `study` spent the root clock, vdh's rounds because `recon` did — has no gate verdict to report. Exhausted is dawn's clock ending it, one step up from an attempt. dawn itself assigns it only to a dispatched attempt that hit its `AttemptWallClock`; the one-step-up reading is the protocol's, and `More()` says so. |
| `InfraError` | pr-ci (a passed stage that could not publish), mdash (nothing anywhere voted), vdh (rounds ran and no hunter ever reached a verdict) | The word for a run that produced the same artifacts as a measurement and measured nothing. |
| `Result.Metric(name) (float64, bool)` | mdash | dawn parses no CLI output, so a gate's number is the only thing a stage can say. `ok` because a false agreement between two absent metrics is a real bug. Subsumes `reward`. |
| `Manifest` / `Artifact{Name, Digest}` | cybergym (handoff), vdh (dry-round detection **reads both fields**) | The record of what crossed a boundary. Two fields, and vdh reads both — separated when hashed, since a name's tail running into a digest's head is a false match that calls a live repo dry. |
| `Result.Actuate(func(*Actuation) error)` | pr-ci, mdash | Fires only on `Passed`, runs in dawn's process where the credentials are. A closure, not a named endpoint. |
| `Actuation.Key` | pr-ci, mdash | The `attempt_id`. dawn dedupes its own retries; carrying the key into the branch name is how the far side gets idempotent too. |
| `Actuation.Published(name)` | pr-ci, mdash | The only door onto `/logs/verifier/publish/`. The agent's raw output is unreachable from an actuator, and that is the whole safety argument. |

## Kept — forced by a settled decision, not by a protocol

Each of these is read by **zero** protocols. Each names the decision that keeps
it, and none is justified by a protocol that does not touch it.

| Construct | Forced by | Why deleting it would break a decision |
|---|---|---|
| `Agent`'s unexported `name` / `image` | *Per-agent concurrency is fixed by the profile as a correctness fact* | dawn must know which pinned image a profile drives. It stays unexported precisely because nothing in the author's vocabulary needs it. |
| `Gate`'s unexported `reason` | *No ungated stage without a stated reason* | Written by `NoGate`, read by dawn: it is rejected when empty at dispatch and carried into the run record, which is the only place a reader can see that a stage stopped checking anything. No protocol reads it, and none should. |

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
| `Result.attempt` (unexported) | — | Read by no protocol, written by nothing, and named by no row of this table — the counterexample to the rule above, surviving by omission from it. `Actuation.Key` already carries the `attempt_id`, and it is the construct a protocol actually reads. Deleted. |
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
- **A fan's width and its scope's `Attempts` are declared apart** (mdash #4). `More()` answers for one attempt; a fan spends `n`, and the retries those children owe spend it again. A scope leased at exactly one round's cost does not fund a round: the first flake makes the second fan dispatch on a spent scope, which unwinds the run. vdh therefore leases each round at `vdhRoundCost + 2` and the root at `2 + vdhRounds*vdhRoundAttempts`; mdash's 24-wide fans sit under 80-attempt scopes. That headroom is author arithmetic, readable in source and unchecked by the surface — `Fan` now says so instead of claiming the lease bounds the width. A `More(n)` would close the attempt half; nothing closes the clock half, and nothing yet forces either signature.
- **`attempt_id` carries no scope component.** `hash(stage_id, content digest, input digest, attempt_number)` means two stages with the same `ID` in different scopes are one stage to recovery unless their environments or inputs happen to differ. mdash relied on that accident twice over — `prove-%d` across the two phases, and `route-%d` and `prove-%d` across the four instances, whose env digests a run may pin identically. Its stage ids now carry both the phase and the env, so nothing depends on the accident; the port pins its two instances to the same digest to keep it that way. That is a discipline the surface documents on `Stage.ID` and does not enforce.

- **A protocol's own arithmetic is unchecked.** Every count a protocol publishes
  — mdash's rate and its populations, vdh's dry rounds — is author code over
  `Result`s, so the surface cannot tell a denominator of audits *performed*
  from one of audits *dispatched*. What the surface does supply is the
  vocabulary to tell them apart: `Metric`'s `ok`, a fan that drains and returns
  every child, and six states in which "the oracle voted no" and "the oracle
  never voted" are different words. Every fix in this class was spending a
  signal the surface already gave and the protocol was throwing away.
