# Tally vNext Structured Control Flow and Propagation

**Issue:** GitHub #7, “Define structured control-flow and propagation semantics”

**Depends on:** GitHub #5, GitHub #6, `2026-08-09-canonical-workflow-semantic-model-design.md`, and `2026-08-09-values-contracts-workspaces-files-design.md`

**Status:** Design approved on 2026-08-09

**Goal:** Give `branch`, `parallel`, `map`, bounded `loop`, `gate`, and `finally` exact execution, output, failure, rejection, cancellation, and cleanup semantics without growing a second scheduler or a policy-knob language inside individual constructs.

## Product Principle

Workflow constructs describe structure. One scheduler owns execution mechanics.

Each construct has one job:

- `branch` selects exactly one typed case.
- `parallel` names a concurrent region and its all-children barrier.
- `map` creates dynamic children from a list.
- `loop` performs bounded semantic iteration.
- `gate` turns a boolean policy verdict into success or rejection.
- `finally` performs bounded cleanup during structured unwinding.

Scheduling, durability, retry, timeout, cancellation, and outcome normalization are shared runtime services. A construct cannot redefine them through `continue_on_error`, quorum, minimum-success, per-scope concurrency, custom cancellation, repair, catch, or merge settings.

## Decisions

- Every structured construct uses one scope lifecycle and one four-outcome model.
- A scope exposes public outputs only when its complete result commits successfully.
- Plain graph dependencies already produce automatic concurrency.
- `parallel` is an explicit named barrier, not a second concurrency engine.
- `map` is the only dynamic fan-out construct and accepts only a list.
- Map child identity is based on item value, not completion order or list position.
- Map results are collected in original input order.
- `loop` has a mandatory positive literal bound and always returns its last successful iteration output on exhaustion.
- A separate `gate` is required when a final negative loop verdict must reject the workflow.
- `gate` has no data outputs and performs no model invocation, repair, retry loop, or evidence aggregation.
- Fail-fast response begins immediately, but concurrent outcomes are normalized after children quiesce.
- Mechanical failure outranks policy rejection; external cancellation outranks both.
- Timeout is a failure, never cancellation.
- `finally` starts only after its body is quiescent and runs in a fresh cleanup context.
- Cleanup cannot catch, replace, or convert the body's outcome.
- Cleanup has no public data outputs and cannot contain another `finally`, `loop`, or `gate`.
- Runtime capacity and cancellation grace are operator policy, not workflow syntax.

## Alternatives Considered

### 1. Orthogonal scopes with one lifecycle — chosen

Each primitive contributes one structural meaning while the scheduler applies the same readiness, fail-fast, settlement, cancellation, and commit rules everywhere. Compound behavior is ordinary composition. This keeps the language small and makes the outcome of a concurrent failure independent of timing.

### 2. Compound workflow primitives

An AWF-style gate could own candidate generation, one or more judges, quorum, repair, and iteration. Map and parallel could similarly own per-node concurrency and failure policies. This shortens some individual workflow definitions, but duplicates lifecycle logic, couples policy to orchestration, and creates many combinations whose cancellation and replay behavior must be specified separately.

### 3. Outcomes as ordinary data with generic recovery

Failure, rejection, and cancellation could become values consumed by branch and loop constructs. This is flexible, but makes accidental recovery easy, weakens fail-closed behavior, and obscures which results may safely commit. Tally keeps terminal outcomes in the execution model and permits composition without a general catch mechanism.

## Universal Scope Lifecycle

Every structured scope follows the same lifecycle:

1. Resolve already committed inputs.
2. Validate the scope input contract.
3. Instantiate the children that the construct makes reachable.
4. Assign every child a stable hierarchical identity.
5. Schedule ready children subject to ordinary dependencies and runtime-wide capacity.
6. Record committed child results and terminal child outcomes.
7. On the first observed non-success, begin the construct's fixed fail-fast response.
8. Wait until active children have settled or have been force-stopped.
9. Normalize concurrent causes into one primary scope outcome and secondary diagnostics.
10. Run applicable `finally` cleanup after body quiescence.
11. Validate the complete scope output contract.
12. Atomically commit the scope's public result.

Only step 12 makes scope outputs visible. `Rejected`, `Failed`, and `Cancelled` scopes publish no boundary output. Internally committed child results remain durable for diagnostics and the later retry, resume, and replay design; durability does not make them visible through a failed scope boundary.

The four terminal outcomes retain the meanings defined by the canonical semantic model:

- **Succeeded:** declared work and output validation completed.
- **Rejected:** an explicit policy gate returned a valid negative verdict.
- **Failed:** execution, contract validation, capability, timeout, or cleanup failed.
- **Cancelled:** external or ancestor intent stopped the work.

The eventual retry system operates below this terminal scope view. A parent observes a child's outcome only after the child's applicable retry behavior has settled. Rejection and cancellation are not disguised as mechanical failures in order to make them retryable.

## Dependency and Output Rules

Structured constructs use the canonical dependency relation; they do not add a separate channel.

- A value binding carries typed immutable data.
- A completion dependency carries readiness without data.
- One dependency may carry both.

Every public output field has one explicit source and must satisfy the containing scope's declared contract. Tally performs no implicit object merge, list concatenation, filesystem merge, race winner selection, or last-writer-wins assignment.

Children may consume only values made available through their lexical scope and explicit bindings. A child result becomes visible outside its containing scope only through a declared output binding. Completion order never changes binding meaning or output order.

## Stable Child Identity

Child identity is hierarchical. An authored child extends its containing scope path with its authored name. Structured constructs add the following semantic components:

- `branch`: the selected case name;
- `parallel`: the named branch;
- `map`: the canonical item-value digest and duplicate occurrence;
- `loop`: the one-based semantic iteration number; and
- `finally`: a fixed cleanup component beneath the protected scope.

For a map, duplicate occurrence is counted among byte-identical canonical item values in input order. Distinct items retain the same semantic identity when reordered. Identical duplicates remain separate children without requiring an author-supplied key expression.

The exact path encoding, digest construction, attempt suffixes, and reuse checks belong to the durable identity design in GitHub #8. This specification fixes the semantic ingredients and the rule that timing cannot influence identity.

## `branch`

`branch` accepts a selector whose type is either boolean or a closed scalar enum.

- A boolean branch declares exactly `true` and `false` cases.
- An enum branch declares exactly one case for every enum member.
- Every case satisfies the same declared branch output contract.
- There is no default, fallthrough, pattern language, or embedded selector expression.

Tally evaluates and records the selector once. It instantiates exactly the selected case; unselected cases do not become skipped runtime nodes. The chosen case executes as an ordinary child graph. If it succeeds, its declared values are validated and committed as the branch result. Its non-success becomes the branch's non-success.

A compiler may simplify a statically constant selector, but must retain enough provenance to explain the authored choice in inspection and replay data.

## `parallel`

`parallel` is a named scope containing at least two named child graphs. It exists when authors want explicit grouping, encapsulation, and an all-children barrier.

Once scope inputs commit, branch roots whose ordinary dependencies are satisfied become eligible together. Tally does not promise simultaneous process start: runtime capacity may delay eligible children without changing workflow meaning. Dependencies inside the scope continue to work normally; an explicit binding from one child result necessarily delays its consumer.

The scope completes only after every child succeeds and every declared parallel output validates. Its public outputs come only from explicit bindings. It does not automatically expose a tuple of branches or merge their structured or filesystem results.

The first observed non-success starts sibling cancellation. After all active work settles, the scope uses the universal concurrent-outcome normalization rules. There are no per-parallel settings for concurrency, ordering, quorum, minimum success, continue-on-error, cancellation, or output merge.

A plain `graph` already schedules independent ready nodes concurrently. Authors do not need `parallel` merely to obtain concurrency.

## `map`

`map` accepts exactly one `list<T>` collection and creates one body instance for each item. The body receives fixed inputs:

- `item`: the current `T` value; and
- `index`: the current zero-based position in the input list.

`index` is body data, not child identity. If reordering changes the body input, the durable reuse rules in GitHub #8 determine whether existing work remains valid.

Each body instance has the same input and output contracts. Instances become independently schedulable, subject only to runtime-wide capacity and their ordinary dependencies. There is no workflow-level concurrency knob and no author-supplied identity or key expression.

An empty collection succeeds with an empty result. Otherwise, map succeeds only when every body instance succeeds and validates. It returns `list<body-output>` in original input order, never completion order.

Any rejected, failed, timed-out, or intrinsically cancelled item starts fail-fast cancellation of active siblings. The map publishes no result. Successfully committed child results remain internal and durable so a future resume does not require Tally to pretend they never happened.

## `loop`

`loop` performs bounded semantic revision. It has a body output object, a named boolean field within that object used as the termination verdict, and a compile-time positive integer maximum of at least one. Runtime-provided or unbounded iteration limits are invalid.

Each body iteration receives fixed inputs:

- `initial`: the loop's complete original input, unchanged across iterations;
- `previous`: the prior successful iteration's complete output, absent for iteration one; and
- `iteration`: the one-based semantic iteration number.

The body returns one complete value satisfying its declared output contract. If the termination field is `true`, the loop succeeds immediately with that complete output. If it is `false` and capacity remains, the complete output becomes `previous` for the next iteration.

If the maximum is reached with a false verdict, the loop still succeeds with the final iteration output. This preserves useful best-effort work. When a negative final verdict must stop downstream execution, the author binds that verdict into a separate `gate`.

A failed, rejected, timed-out, or cancelled body iteration ends the loop with the corresponding normalized non-success. It does not fall back to an older revision. Only a successfully committed body result may become `previous`; invocation attempts and retries do not create new semantic iteration numbers.

There is no carried-field mapping, shared mutable state, retry-on-negative option, break expression, or repair policy in `loop` itself.

## `gate`

`gate` is a deterministic policy leaf with exactly these inputs:

- `passed`: required boolean;
- `reason`: optional string.

When `passed` is true, the gate succeeds. When it is false, the gate returns `Rejected` and records the optional reason. A missing or non-boolean `passed` value is a contract failure, not a policy rejection.

The gate has no data outputs. Candidate data flows directly from its producer to downstream consumers; those consumers additionally completion-depend on the gate. This prevents the gate from becoming an untyped data relay.

A judge, jury, vote, evidence collector, critique, or repair agent is an ordinary composition of `llm`, `agent`, or `script` leaves before the gate. The gate itself owns no model, tool, timeout work, retry, quorum, evidence, or repair loop.

## Fail-Fast and Concurrent Outcome Normalization

Fail-fast has two distinct moments:

1. **Response:** the first observed non-success stops new work and starts cancellation of active siblings.
2. **Diagnosis:** after active children settle, Tally chooses the stable terminal outcome from all intrinsic causes.

Body causes normalize in this precedence order:

1. External or ancestor cancellation produces `Cancelled`.
2. Otherwise, any intrinsic mechanical failure or timeout produces `Failed`.
3. Otherwise, any policy rejection produces `Rejected`.
4. Otherwise, an intrinsic child cancellation produces `Cancelled`.

Parent-induced sibling cancellations are consequences, not candidate primary causes. They remain observable diagnostics but cannot hide the rejection or failure that triggered them.

If multiple candidate causes have equal precedence, the cause with the lexically smallest stable child path becomes primary. Every other cause is retained as a secondary diagnostic. Wall-clock completion order never selects the primary cause.

Timeout is represented as `Failed` with a timeout classification. `Cancelled` is reserved for external or ancestor intent, plus the exceptional case of an intrinsic backend cancellation that has no more specific failure or rejection cause.

## Cancellation and Structured Unwinding

When a scope is cancelled or begins fail-fast response, Tally:

1. stops scheduling new descendants;
2. sends cancellation to active descendants;
3. waits for the runtime-wide cancellation grace period;
4. asks the execution backend to force-stop noncooperative descendants; and
5. runs applicable cleanup from the innermost protected scope outward.

The cancellation grace period is operator/runtime policy recorded with the run. It is not a construct field. A backend must either support the required cancellation behavior or reject the workflow during capability preflight; it may not silently weaken the semantics.

Cleanup receives a fresh execution context rather than the cancelled body context. Its leaves use ordinary bounded deadlines. Only process death or a second explicit force-stop of the overall run may abort cleanup rather than allow structured unwinding to finish.

## `finally`

`finally` contains a protected body graph and a cleanup-only graph. Cleanup starts after the body is quiescent: every active body descendant has completed, acknowledged cancellation, or been force-stopped.

The cleanup guarantee begins once the `finally` scope's own inputs validate and it enters execution. From that point, cleanup runs for every body outcome, including a failure before the first body child starts. If the `finally` scope itself cannot be entered because its input contract is invalid, there is no valid execution context to clean up. Process death and a second explicit force-stop remain the only runtime interruptions that may prevent the guarantee from completing.

The cleanup graph may receive:

- the protected scope's original input values;
- a typed `outcome` enum describing the normalized body result; and
- explicitly bound, fully committed body-child outputs.

A binding from a body child is optional unless the compiler can prove that the producer always succeeds before cleanup. Failed, unstarted, or partially completed children contribute no value. Tally never captures or publishes their partial data for cleanup.

Cleanup may contain ordinary bounded leaves and structural composition, including dependency ordering, branching, parallel work, and finite map work. It cannot contain `gate`, `loop`, or another `finally`. Cleanup is operational work, not a second policy decision, recovery loop, or unbounded cleanup stack.

The cleanup graph has no public data outputs. It cannot catch, replace, downgrade, or convert the body's outcome.

- Body `Succeeded` plus intrinsic cleanup non-success produces `Failed(cleanup)`.
- Body `Failed`, `Rejected`, or `Cancelled` remains the primary outcome if cleanup also fails.
- A cleanup failure in the latter case is retained as a secondary diagnostic.

A new external or ancestor cancellation arriving while cleanup runs still has universal highest precedence and produces `Cancelled`; the interrupted cleanup is diagnostic context. This is cancellation of the enclosing run, not an intrinsic cleanup failure.

This cleanup precedence is intentionally distinct from normal sibling arbitration: a cleanup problem must not rewrite the already established reason structured unwinding began.

## Deterministic Fan-In Summary

- `branch` exposes the selected case through the shared case contract.
- `parallel` exposes only explicitly declared named bindings after all children succeed.
- `map` exposes all body outputs in input order.
- `loop` exposes the last successful semantic iteration output.
- `gate` exposes no data.
- `finally` exposes no data.

No construct publishes partial results after a non-success. Internal committed results remain durable but stay behind the failed scope boundary.

## Prestige and AWF Lowering

The representative Prestige control patterns lower into orthogonal Tally composition:

| Prestige/AWF behavior | Tally vNext expression |
| --- | --- |
| Independent stages | Ordinary `graph` dependencies and automatic concurrency |
| Explicit parallel phase | `parallel` when a named barrier or encapsulated output boundary is useful |
| Batch processing | `map` over a typed list |
| Generate, judge, repair | `loop` whose body composes worker and judge leaves |
| Reject after exhausted repair | A separate `gate` bound to the loop's final verdict |
| Multiple judges or aggregation | Ordinary leaves and explicit typed fan-in before `gate` |
| Conditional path | Exhaustive typed `branch` |
| Cleanup and report preservation | `finally` with explicit optional committed inputs |

This retains the useful behavior without adopting AWF's compound gate, implicit repair loop, catch semantics, mutable shared workspace, or per-node scheduling policies.

The complete end-to-end Prestige proof depends on later vNext designs for durable execution, adapter capabilities, native scripts, subworkflows, and author syntax. This specification guarantees that the control-flow portion has a deterministic lowering target.

## Verification Strategy

Tests must exercise runtime traces, not only parsing or IR snapshots. Child completion order should be deliberately randomized and faults injected at every boundary.

### Branch conformance

- Reject non-boolean and open or incomplete selectors.
- Instantiate exactly one exhaustive case.
- Record the selected case once.
- Produce the same public result after replay.

### Parallel conformance

- Demonstrate independent ready children can overlap.
- Preserve ordinary dependencies inside the scope.
- Wait for all successful branches before commit.
- Cancel siblings after a non-success.
- Normalize simultaneous rejection, failure, timeout, and cancellation deterministically.

### Map conformance

- Succeed for an empty list with an empty result.
- Distinguish byte-identical duplicates by occurrence.
- Retain distinct-item semantic identity across reorder.
- Collect results by input order under randomized completion.
- Publish no map result after one child fails while retaining committed internal children.

### Loop conformance

- Stop and return on a true verdict.
- Return the last output after false-verdict exhaustion.
- Pass `initial`, `previous`, and `iteration` exactly.
- Reject zero, negative, dynamic, and absent bounds.
- Keep an iteration identity stable across invocation retries.
- Stop on a mechanically failed body rather than using an older revision.

### Gate conformance

- Succeed without data outputs for true.
- Reject with the supplied reason for false.
- Fail contract validation for a malformed verdict.
- Require downstream candidate data to use a direct value binding.

### Finally conformance

- Run cleanup after body success, rejection, failure, timeout, and cancellation.
- Start only after body quiescence.
- Unwind nested cleanup innermost first.
- Expose only explicitly bound committed optional values.
- Preserve body non-success as primary if cleanup also fails.
- Convert body success plus intrinsic cleanup non-success to `Failed(cleanup)`.
- Preserve universal cancellation precedence if external cancellation arrives during cleanup.

### Cross-construct invariants

- No non-successful scope exposes a boundary result.
- Stable identity and primary diagnosis do not depend on completion timing.
- Parent-induced cancellations do not mask their cause.
- Timeout always reports failure.
- No workflow-local resource or cancellation policy changes semantic behavior.

## Deliberately Deferred

This design fixes semantic behavior but does not preempt the remaining vNext work:

- GitHub #8 defines identity encoding, commits, attempts, retry, resume, replay, and reuse validity.
- GitHub #9 defines agent roles and the adapter capability boundary.
- GitHub #10 defines native script invocation and force-stop behavior.
- GitHub #11 defines subworkflow linking and boundary propagation.
- GitHub #12 defines author-facing syntax and lowering into this canonical model.

These later designs may choose representation and mechanism. They must preserve the semantics in this document and cannot add construct-local lifecycle policies through another spelling.

## Acceptance Criteria

The design is complete when:

- every construct has one clear purpose and a fixed default behavior;
- success, rejection, failure, timeout, and cancellation propagation are deterministic;
- concurrent completion order cannot change the primary outcome or public result;
- map and loop children have stable semantic identities;
- fan-in order and scope output publication are unambiguous;
- loop exhaustion preserves its final typed result while rejection remains explicit;
- cleanup runs during structured unwinding without becoming catch or recovery;
- Prestige's control patterns lower by composition rather than special-purpose policy knobs; and
- later identity, adapter, script, subworkflow, and syntax work has explicit boundaries.

## Implementation Evidence — 2026-08-11

Task 9 conformance and final cancellation/runtime hardening are captured by the
immutable source-and-test range
`7dfe848b3d60ee688dcdc0694ebdd5f1e64a8db5..f5ff103773965ba8e1021fd3e597bdde97e836e6`.
The original conformance commit is
`92cde7d574a066a6513c3bfcdcab2fa0ad78e496`, the first focused hardening
endpoint is `c8245169bcebb7b8c26cb810a033850baa1e51df`, and the final source/test
endpoint is `f5ff103773965ba8e1021fd3e597bdde97e836e6`.
The Prestige-shaped
trace, deterministic arbitration/cancellation permutations, deferred edge
matrices, and 50,000-depth/50,000-item finite-execution subprocesses were
verified with:

```bash
go test ./scheduler -run 'TestPrestigeStructuredControlTrace' -count=1
go test ./scheduler -run 'Test(PrestigeStructuredControlTrace|StructuredControlArbitrationStress|StructuredControlGraphCancellationStress|StructuredControlMapCancellationStress|StructuredControlCleanupCancellationStress|BranchBooleanFalseIsStableAcrossTimingPermutations|MapObservedFailureLeavesLaterSentinelUninstantiated)' -count=10
go test -race ./scheduler -run 'Test(PrestigeStructuredControlTrace|StructuredControl|BranchBooleanFalse|MapObservedFailure)' -count=1
go test ./scheduler -run 'TestStructuredControlFiniteExecutionSubprocess' -count=1
go test ./scheduler -count=1
go test -race ./scheduler -count=1
go test ./value ./workflow ./workspace ./scheduler -count=1
go test -race ./value ./workflow ./workspace ./scheduler -count=1
go test ./... -count=1
go vet ./...
git diff --check
```

The final endpoint additionally proves controller-linearized cleanup-outcome
capture and success publication, claim-first immutability, one-shot closed
cancellation observation, indexed linear graph readiness/dataflow, complete
cleanup-cause retention, and a 50,000-node completion chain under the same
1 MiB maximum Go stack and runtime capacity one.

This evidence implements and hardens only the structured-control runtime in
this specification. It does not claim implementation of GitHub #8–#12.
