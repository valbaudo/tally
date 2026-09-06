# dawn

dawn is the mechasuit around an agent CLI: it gives a general-purpose coding agent everything it needs to carry a long-running task to a verified result, following an author-written protocol. It orchestrates agents; it is not one, and it is not a workflow language.

## Language

**dawn**:
The orchestration system — the mechasuit around any agent CLI.
_Avoid_: the harness, the framework, AWF.

**protocol**:
What an author writes to make dawn perform a class of task ("how to audit any codebase"): a Go control-flow program plus prose per stage plus a verifier image per stage. A protocol generates attempts; it is not one instance of a task.

**stage**:
One step of a protocol: a single runner call with its inputs, its prose instruction, and its verifier.

**runner**:
The component that executes one attempt. Today it wraps one Harbor trial.

**attempt**:
One execution of one agent against one environment — the unit Harbor provides. One trial runs exactly one agent; dawn owns all sequencing above it.

**harness**:
An agent's own internal loop (as in CyberGym's "top harnesses"). dawn never owns one.
_Avoid_: using "harness" for dawn or for Harbor.

**Harbor**:
The Terminal-Bench 2.0 runner dawn adopts for one isolated attempt per trial.

**verifier** / **gate**:
The check that decides a stage's outcome, run in a separate pinned no-network image after the agent's container is gone. "gate" is the same thing named for its role at a decision boundary.

**agent profile**:
dawn's knowledge of how to drive one agent CLI: a Go struct over a digest-pinned image, fixing the launch command, prompt delivery, gateway pointing, and config paths. It also pins the model and reasoning effort dawn tells Harbor to run — an unpinned one lets the CLI resolve it from ambient account state instead, so the same protocol could silently run a different model tomorrow. Admitted only by a build-time self-test proving a failed run leaves no declared output.
_Avoid_: adapter (that is Harbor's word for its own).

**soundness**:
A stage's statically declared property, one of `sound` or `format_only`, written in the protocol source beside the control flow. A `format_only` stage can never reach `passed`. Not a cost claim — cost is measured, not declared.
_Avoid_: "cheap" as a soundness value.

**oracle**:
What a protocol's verifier can actually establish — sound-and-cheap, real-but-expensive, or none. The oracle decides a protocol's shape.

**scope**:
A bounded region of a protocol run with its own budget lease. There is one per run: scopes were nestable until four protocols were written and none nested.

**lease**:
An atomic reservation a scope holds before dispatch, counted in wall clock, attempts, or container concurrency — never in money or tokens. Provider quota is not leasable: it has no published ceiling and its exhaustion may be unobservable, so dawn tracks draw rate, never a balance. Leases are booked by Scope.charge(), which is atomic; there is no separate ledger.

**admission**:
charge() — not a separate component, and no longer a semaphore either. It was charge() plus one memory-sized semaphore per agent name, which existed to bound how many attempts of one agent a fan could put in flight at once. Scope.Fan was deleted after four protocols used it zero times, and with one dispatch at a time there is nothing left to admit against.
_Avoid_: admission queue (an earlier sketch of a separate component, never built), scheduler as a second component, budget authority.

**actuator**:
The trusted component that performs an external effect (push a branch, open a PR, call a scoring endpoint) after verification, so the agent never holds production credentials. Runs in dawn's own process, fires only on `passed`, and publishes only bytes the gate itself wrote.

**State**:
A stage's outcome, one of a closed set: `passed`, `rejected`, `unverified`, `exhausted`, `infra_error`, `cancelled`. The numeric score rides alongside as a metric, never as the verdict.

**artifact**:
A declared output of a stage: bytes carrying a logical name from dawn's declared list. Never a path chosen by the agent, never executed.

**manifest**:
The record of what crossed a stage boundary — per artifact: logical name and content digest. Nothing else: path is outputDir + name, a pure function; which attempt produced it is already the evidence layout (`<run>/attempts/<stage>/<attempt>/`). A second representation of either would only have to agree with the first by convention.

**declared output**:
An output a protocol names in advance for a stage. Only declared outputs cross a boundary, and declaring them before dispatch is what makes an interrupted attempt inspectable.
