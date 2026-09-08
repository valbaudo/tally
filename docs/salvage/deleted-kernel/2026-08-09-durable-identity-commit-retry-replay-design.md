# Tally vNext Durable Identity, Commit, Recovery, and Replay

**Issue:** GitHub #8, “Define durable identity, commit, retry, and replay semantics”

**Depends on:** GitHub #5–#7 and:

- `2026-08-09-canonical-workflow-semantic-model-design.md`
- `2026-08-09-values-contracts-workspaces-files-design.md`
- `2026-08-09-structured-control-flow-propagation-design.md`

**Status:** Design approved on 2026-08-09

**Goal:** Let Tally resume, derive, and replay arbitrary structured agent workflows without repeating committed work, confusing a run with a global cache, silently changing prior decisions, or exposing journal, identity, provider-recovery, and persistence machinery as workflow-language knobs.

## Product Principle

Durability should make workflows more capable without making workflow authors think about durability.

Tally therefore exposes four execution intents:

- `fresh`: execute a new run without importing prior results;
- `continue`: continue one captured run from its durable boundary;
- `derive`: create a new run from an earlier run and reuse compatible committed work;
- `replay`: reconstruct recorded history without executing work.

Cancellation remains an operator action on a run. Everything else in this specification—runtime addresses, fingerprints, attempts, effect keys, commit markers, journal facts, provider handles, lineage, and reuse matching—is runtime machinery derived automatically by Tally.

There is no generic retry system. In Tally, **retry** means only baked-in continuation of the same provider session after an adapter positively recognizes a transient upstream provider failure.

## Decisions

- One deep run-ledger module owns durable execution truth.
- Every run captures its canonical workflow and immutable input values.
- A run's journal is append-only history; projections and indices are rebuildable.
- Runtime address, work fingerprint, attempt identity, and effect key are distinct concepts.
- Dynamic instance addresses are hierarchical and independent of completion timing.
- A node result becomes visible only through one commit written after all values validate and become durable.
- Successful leaf and structured-scope commits are reusable; failures, rejections, and cancellations are durable facts but not result commits.
- `continue` uses the original run's captured definition, committed results, and recorded control decisions.
- `derive` accepts arbitrary workflow and input changes, creates a child run, and imports every reachable compatible commit one-to-one.
- Compatibility depends on effective work and exact inputs, not node name or graph position.
- Results imported into a changed contract are revalidated against that contract.
- `fresh` is the explicit way to resample nondeterministic work, observe changed external state, or intentionally repeat all effects.
- `replay` performs no execution and never regenerates missing values.
- Provider session state may recover only the same node instance; it never becomes a cross-node result.
- Generic automatic retry, author retry counts, and retry-policy matrices do not exist.
- Recognized upstream throttling or overload continues the same provider session with baked-in backoff; it never starts a replacement session or replays the prompt.
- Private workspaces are attempt state, not durable workflow values.
- External effects remain at-least-once around uncertain interruption; a stable effect key improves deduplication but is not a transaction.
- The redesign has no persistence backward compatibility, legacy readers, migration shims, or dual writes.

## Alternatives Considered

### 1. Lineage, semantic reuse, and one run ledger — chosen

Each run has immutable journaled history and commits. Operator intent distinguishes continuation, derivation, fresh execution, and replay. Runtime address identifies an occurrence; a separate work fingerprint identifies reusable work. One deep module hides addressing, journaling, commit, recovery, lineage, and matching behind a small semantic interface.

This design gives Tally exact continuation and broad reuse while keeping workflow YAML free of persistence concerns.

### 2. Global content cache

The runtime could hash a node and its inputs and reuse matching entries everywhere. This resembles current Tally's “rerun is resume” model. It is initially small but conflates same-run recovery with cross-run reuse, cannot explain lineage cleanly, struggles with intentional duplicates and dynamic instances, and gives cancellation and replay no precise history.

### 3. Mutable run plus manual invalidation

The runtime could keep one run record, allow its definition to change, and require path-oriented invalidation such as `--from`. This resembles AWF's pinned definition plus invalidation machinery. It offers manual control but leaks internal paths, spreads durable knowledge across special-purpose maps, mutates the meaning of historical state, and makes users reason about the persistence implementation.

## Architecture and Ownership

Tally adds one internal **run ledger**. Its one-sentence responsibility is:

> Preserve and interpret the durable semantic history of one execution lineage.

The run ledger owns:

- run identity and parent lineage;
- captured workflow and input references;
- runtime instance addressing;
- effective-work fingerprints;
- dynamic discovery and control-decision facts;
- attempt and provider-session recovery state;
- node and scope commits;
- cancellation and continuation history;
- journal folding and run reconstruction;
- derivation matching and import provenance; and
- replay projections.

This is one deep module, not a collection of `BranchJournal`, `MapState`, `RetryPolicy`, `CacheMatcher`, `ResumeManager`, and `ReplayService` wrappers. Those concerns share one body of knowledge: what execution facts mean durably.

The surrounding boundaries are:

| Owner | Knows | Does not know |
|---|---|---|
| Workflow language/compiler | Canonical nodes, scopes, bindings, contracts | Journal formats, attempt numbers, storage keys, provider handles |
| Scheduler | Readiness, structured execution, outcome propagation | Persistence representation, reuse hashing, provider-specific recovery protocols |
| Run ledger | Durable execution semantics and lineage | Provider commands, file upload APIs, workspace implementation, database format |
| Immutable value store | Structured values, files, trees, digests, reachability | Workflow control flow, attempts, provider sessions |
| Leaf adapter/backend | Execute one leaf, expose capabilities, return result/checkpoints | Run history, derivation matching, journal representation |
| Persistence implementation | Atomically preserve ledger facts and content references | Workflow-language meaning and provider behavior |

The scheduler interacts with the run ledger through semantic operations such as opening a run by intent, recording a discovered instance, beginning or checkpointing an invocation, committing a complete result, recording a terminal outcome, cancelling, and reconstructing state. Exact API spelling belongs to implementation planning. Callers never append raw journal records or calculate storage keys.

Physical journal order is not workflow semantics. Concurrent facts identify their semantic instance, while the stable outcome and cancellation rules from GitHub #7 determine the result independently of arrival timing.

## Durable Vocabulary

### Run

A run is one immutable execution history with:

- a unique run ID;
- one captured canonical workflow;
- one immutable root input value set;
- recorded resolved runtime behavior as nodes execute;
- zero or one parent-run reference;
- semantic journal facts; and
- reachable commits and values.

Continuing a run appends history to that same run. Deriving creates another run and never rewrites its parent.

### Runtime instance address

An instance address identifies one occurrence in one run's execution tree. It is hierarchical and automatically derived from semantic structure:

- an authored child extends its containing scope with its authored name;
- a branch adds the selected case;
- a parallel scope adds its named branch;
- a map adds the canonical item-value digest and the duplicate occurrence among byte-identical item values in input order;
- a loop adds its one-based semantic iteration number;
- a sub-workflow adds its call-site instance followed by its internal path; and
- `finally` adds a fixed cleanup component below its protected scope.

Map identity follows GitHub #7: it is based on item value, not list position or completion order. Distinct items retain their addresses when reordered. Identical duplicates remain distinct through their duplicate occurrence.

Every dynamic discovery fact is durable before its child is launched. Exact byte encoding of an address is private to the run ledger.

### Work fingerprint

A work fingerprint identifies the effective request whose prior result may be reusable. It is independent of run ID and runtime address.

For a leaf it covers all declared and resolved behavior Tally can know, including:

- canonical leaf kind and operation;
- prompt, instruction, or script content;
- exact immutable input values and file/tree digests;
- ordered collection position when position is a declared input;
- selected role, skills, tools, adapter, backend, model, and behavior-affecting configuration; and
- declared execution semantics that can change the produced result.

For a structured scope it covers the canonical scope semantics, its effective child graph, and exact inputs. An unchanged scope may therefore be imported as a unit. When the scope changes, Tally reevaluates the structure and may still import compatible descendants.

The fingerprint excludes:

- run ID, parent ID, runtime address, and node display name;
- attempt and effect identities;
- timestamps, logs, metrics, token streams, and progress;
- physical storage locations and provider file IDs;
- cancellation history and baked-in provider backoff; and
- output values.

The output contract is checked separately. A prior result is reusable only when it validates against the current contract. This permits compatible contract edits instead of invalidating work merely because contract text changed.

Secret bytes never enter a fingerprint, journal, trace, or committed value, as required by GitHub #6. Credential rotation is not treated as a semantic input change by default. A semantic data choice encoded only in a secret is therefore outside Tally's compatibility knowledge; an ordinary declared version or selector value can make that distinction visible when desired.

A fingerprint describes declared behavior, not the entire mutable outside world. A provider can change a model behind the same model name, a network service can change, and a script can read undeclared external state. Tally records what it can know and never presents compatibility as proof of referential transparency. Use `fresh` when current external state or new sampling is the purpose.

### Attempt identity

An attempt identity is the run ID, instance address, and monotonically increasing invocation ordinal. A new ordinal is created only when Tally starts another logical leaf execution: another native script process, raw model request, or provider session. Adapter transport subprocesses used to poll or continue an existing provider session do not create attempts.

- Polling or retrieving an already-submitted provider operation stays in the same attempt.
- Same-session continuation after a recognized transient upstream failure stays in the same attempt.
- Reinvoking an uncommitted leaf after process-state loss creates another attempt.
- Explicitly continuing a failed or cancelled run may create another attempt for unfinished work.

An attempt start without a later terminal fact is reconstructed as interrupted. Attempt identities exist for recovery and diagnosis; they never change semantic loop iteration numbers or result compatibility.

### Logical effect key

Before the first executable attempt begins, Tally durably assigns the logical instance an effect key. The key is:

- stable across attempts, process restart, and `continue` for that instance;
- stable through same-session transient provider continuation;
- distinct for intentional duplicate instances;
- new for work actually executed in a fresh or derived run; and
- unnecessary when a derived run imports an existing commit and performs no effect.

The executor context receives the key automatically. Provider adapters use it when an upstream interface accepts an idempotency key. Native scripts receive it through one fixed runtime channel defined by the script-execution ticket. Agent tools may receive it when their interface supports idempotent operations. Workflow authors do not generate, route, or configure it.

An external system may ignore the key. Tally therefore promises at-least-once execution around uncertain interruption, never exactly-once effects or a transaction spanning Tally and another system.

### Commit

A commit is the immutable visibility marker for one successful node or structured scope result. It records:

- run and instance address;
- effective work fingerprint;
- complete normalized result envelope;
- references to all immutable structured, file, and tree values;
- the successful terminal outcome;
- behavior provenance needed to explain compatibility; and
- source run and source commit when imported.

Failures, rejections, and cancellations are durable journal facts but do not create result commits. Successfully committed descendants remain durable even when their containing scope later fails, rejects, or cancels, but #7 prevents their values from leaking through an uncommitted scope boundary.

## Universal Node Boundary Protocol

Every executable leaf and structured scope uses one protocol:

1. Resolve already committed inputs.
2. Validate the node's input contract.
3. Derive or restore its runtime instance address.
4. Resolve its effective execution behavior and work fingerprint.
5. Ask the run ledger whether the current run intent supplies a compatible commit.
6. If executing a leaf, durably assign its effect key and begin an attempt before invoking the adapter.
7. Checkpoint an opaque provider recovery handle as soon as the adapter supplies one.
8. Receive and normalize the complete result.
9. Validate the output contract and every declared media/fidelity constraint.
10. Capture every declared named output and optional published workspace into the immutable value store.
11. Durably write the commit that references the complete stored result.
12. Make the result visible to downstream scheduling.

Structured scopes do not perform steps 6–10 as a leaf invocation. They journal their dynamic children and control decisions, wait for the #7 scope lifecycle to settle, validate their boundary output, and then commit that complete scope result.

Values are always stored before the commit. A crash may leave unreachable stored content, but no consumer can observe it. A crash after the commit reuses the complete result. Storage reclamation may later remove unreachable content without affecting journal meaning.

## Semantic Journal

The journal contains only facts that can affect recovery, reuse, replay, or explanation:

- run created, continued, derived, cancelled, or terminally settled;
- parent lineage and operator intent;
- captured canonical workflow and immutable root inputs;
- resolved behavior provenance needed by executed instances;
- dynamic instance discovered;
- branch case selected, map item instantiated, loop iteration created, or sub-workflow called;
- attempt started;
- provider recovery handle checkpointed or updated;
- recognized transient upstream state entered or left;
- attempt failed or cancelled;
- node or scope commit written;
- prior commit imported into a derived instance;
- cancellation requested, propagated, and settled; and
- structured scope settled after its children and applicable `finally` work.

Ordinary logs, model token streams, stdout/stderr chunks, metrics, traces, and progress notifications are observable diagnostics, not semantic journal facts.

A successful attempt becomes durable through the node commit itself; Tally does not first write a separate “attempt succeeded” fact that could survive without a committed result. Replay is read-only and likewise appends no semantic fact to the run it reads.

The journal is append-only. A conforming persistence implementation must provide these behavioral guarantees:

- a semantic transition can be retried by the runtime without being recorded twice;
- contradictory terminal transitions for one attempt or commit cannot both become durable;
- a commit cannot reference values that were not already stored durably;
- acknowledged facts survive process termination;
- the complete run projection is reconstructible solely from durable facts and committed values; and
- concurrent physical append order cannot change the normalized workflow outcome.

The specification does not choose JSONL, SQLite, a relational schema, a log service, or a snapshot format. Rebuildable snapshots and indices may accelerate folding, but the append-only semantic history remains authoritative.

## Execution Intents

### `fresh`

`fresh` captures the supplied workflow and inputs into a new run. It imports no commits and executes every reachable node. It is the explicit operation for:

- resampling an agent or model;
- observing changed undeclared external state;
- repeating external effects intentionally; or
- obtaining an execution with no inherited work.

Every executed instance receives a new effect key.

### `continue`

`continue` reopens one existing run using its captured workflow and inputs. It does not depend on the current working directory or silently substitute edited files.

It reconstructs:

- committed node and scope results;
- dynamic instances and control decisions already recorded;
- active or interrupted attempts;
- cancellation propagation and pending `finally` work; and
- the run's last terminal outcome.

It then handles reachable unfinished work in this order:

1. Reattach to or retrieve the same provider operation when the checkpointed handle supports it.
2. Continue the same provider session when the narrowly defined transient-upstream recovery rules apply.
3. Otherwise start the uncommitted node again at its node boundary.

The logical instance and effect key remain stable. A failed or cancelled run can later be continued. Its prior failure or cancellation stays in the journal; continuation does not erase history or require an exposed execution-epoch concept.

### `derive` — rerun with reuse

`derive` creates a child run from any prior run while accepting arbitrary workflow and input changes. Tally does not reject those changes as drift. The parent remains immutable and independently replayable.

The derived run evaluates the new workflow and imports reachable compatible commits as follows:

1. Prefer a source commit at the same instance address when its work fingerprint matches.
2. Group remaining source commits and current instances by compatible work fingerprint.
3. Revalidate every candidate result against the current output contract.
4. Pair duplicate candidates one-to-one using canonical source and destination address order.
5. Write a local import commit that references the source commit and values without duplicating their bytes.
6. Execute every unmatched reachable instance normally.

One source commit can satisfy at most one instance in a derived run. If the new graph has more compatible duplicate instances than the parent, Tally imports the available commits and executes only the additional instances. If it has fewer, unused parent commits remain historical parent data.

All successful leaf kinds are eligible, including `agent`, `llm`, and `script`. Structured-scope commits are eligible when the entire scope remains compatible. When a scope changes, Tally reevaluates its control flow and can still reuse compatible descendant commits.

Derived branch selections, map expansions, loop progress, and sub-workflow structure come from the new definition and inputs. Tally never forces an old control decision into an incompatible new structure merely because both runs share lineage.

### `replay`

`replay` folds the selected run's journal and committed values to reproduce:

- captured workflow and inputs;
- dynamic execution tree;
- attempts and recovery checkpoints;
- control decisions;
- imported-commit provenance;
- cancellation and continuation history;
- node and scope outcomes; and
- final visible outputs.

Replay never invokes an adapter, model, agent, script, tool, or external service. Missing or corrupt committed content is an integrity failure. Replay does not regenerate or repair it.

## Retry Means Same-Session Upstream Recovery Only

Tally has no generic retry policy and no default attempt count.

The runtime calls an operation a retry only when all of these are true:

1. An `agent` or `llm` adapter positively recognizes a transient upstream provider failure such as throttling (`429`), model overload, or temporary model unavailability.
2. A resumable session handle for that same invocation has already been checkpointed durably.
3. The adapter can continue that same session without replaying the original prompt or creating another session.

The adapter then:

1. waits for provider `Retry-After` guidance or its built-in provider-specific backoff;
2. sends `continue` to the same session;
3. keeps the existing attempt identity and effect key; and
4. repeats only this recovery operation until the session succeeds, returns a definitive non-transient failure, or the run is cancelled.

There is no workflow `attempts` field, retry-count default, backoff configuration, retry-condition expression, or maximum imposed by Tally. Provider-specific classification and continuation stay inside the adapter and are tested by adapter conformance. Generic engine code does not parse arbitrary error strings or know Claude/Codex command syntax.

If a resumable handle was never obtained, this retry is impossible. Tally reports the upstream failure rather than secretly starting another session.

The following are explicitly not retries:

- a script exiting nonzero;
- a result violating its contract;
- an ordinary agent or model failure not positively classified as transient upstream failure;
- gate rejection;
- cancellation;
- feedback-driven revision with changed context;
- another semantic loop iteration;
- crash recovery that reinvokes an uncommitted node; or
- operator-requested continuation of a failed run.

Feedback and strategy changes are ordinary workflow control flow. Process and operator recovery are journaled as new invocations when they actually invoke a leaf again.

## Cancellation and Recovery

Cancellation records intent and stops execution according to GitHub #7; it does not permanently poison a run.

When cancellation is requested, Tally durably records it before propagating the request. It stops new descendants, settles active work, runs applicable `finally` scopes, and records the resulting normalized outcome.

If the user later chooses `continue`:

- committed results remain committed;
- prior dynamic discoveries and control decisions remain fixed for that run;
- a recoverable active provider operation is recovered as the same attempt;
- uncommitted work without recoverable state begins again at its node boundary;
- completed `finally` work remains committed;
- interrupted `finally` work continues through the same universal protocol; and
- the earlier cancellation and later continuation both remain visible in replay.

The race between completion, failure, rejection, and cancellation is resolved by the stable scope-outcome rules in GitHub #7, not by whichever journal append happens to reach storage first.

## Workflow and Input Changes

Run history is immutable; user capability is not.

At run creation, Tally snapshots the canonical workflow, root structured inputs, and every supplied file or tree. Later host edits cannot mutate the run's meaning.

- `continue` faithfully uses that captured material.
- `derive` accepts current edited material and reuses compatible commits.
- `fresh` accepts current edited material and imports nothing.

Tally does not reject an edited workflow as drift. The operation chosen by the user states whether they want faithful continuation, evolution with reuse, or a completely new execution.

Resolved adapter and backend behavior is recorded as execution reaches each node rather than pretending every external detail can be known at initial admission. The adapter-capability ticket defines the exact behavior signature supplied to the run ledger.

## Workspaces and Provider State

This specification preserves the GitHub #6 boundary:

- committed structured values, named files, trees, and explicitly published workspaces are durable;
- a leaf's private workspace is execution state, not a value;
- a raw `llm` receives contracted file values through adapter attachment translation, not a workspace;
- a filesystem-capable `agent` or `script` receives a private workspace materialized from zero or one committed base tree plus named immutable inputs;
- while the same provider session remains recoverable, its private remote state may remain part of that attempt;
- when that session is not recoverable, the node begins with a fresh private workspace from committed inputs;
- leftover local directories, provider file IDs, sessions, and remote containers are never evidence of completion; and
- Tally adds no generic private-workspace checkpoint, diff, merge, or restoration system.

Anything that must cross a node or survive independently of a live recoverable attempt must return through declared outputs or explicit full-workspace publication and commit.

## Integrity and Retention

A commit is Tally's evidence that work completed. Reconstruction verifies that every referenced value exists and matches its recorded digest.

- Corrupt or missing committed content is an integrity failure, not an ordinary node failure.
- `continue` does not silently replace a corrupt commit by re-executing its node.
- `replay` never regenerates missing values.
- Repair tooling, if later designed, must produce explicit auditable history rather than rewrite facts invisibly.
- Imported commits preserve source-run and source-commit provenance.
- A derived run keeps its imported values reachable even if the parent run is later archived or removed from ordinary listings.

Physical retention policy and garbage-collection algorithms are implementation details. They must never collect values still reachable from a live commit.

## Failure Semantics

Failures, rejections, cancellation, and success keep the four-outcome meanings from GitHub #5 and #7.

- A successful node may commit its complete result.
- A failed, rejected, or cancelled node commits no result.
- Its terminal outcome and diagnostics remain durable journal history.
- A failed containing scope exposes no child output through its boundary.
- Successfully committed descendants remain available for a later `continue` or compatible `derive`.
- A timeout is a failure, not cancellation and not an automatic retry trigger.
- Missing capability or invalid configuration is a failure and cannot enter provider transient recovery.

Explicit operator continuation may invoke unfinished work again. That is a new attempt under explicit user intent, not a hidden automatic retry policy.

## Storage-Agnostic Atomicity Invariants

The storage implementation may use transactions, compare-and-swap records, an append log, or another mechanism, but it must preserve these invariants:

1. An effect key and attempt start are durable before the executable invocation begins.
2. A provider recovery handle acknowledged to the scheduler is durable before recovery may depend on it.
3. All result values are durable before the commit referencing them becomes visible.
4. One instance has at most one visible successful commit in one run.
5. A derived import binds one source commit to at most one destination instance.
6. Acknowledged journal facts survive restart.
7. Partial writes cannot decode as valid facts or commits.
8. Folding the same facts produces the same semantic run projection.
9. Replaying facts never causes external execution.
10. Physical concurrency cannot change deterministic outcome arbitration.

These are semantic requirements, not a prescribed database schema.

## Inspection Model

Inspection should explain, for every runtime instance:

- how its address was formed;
- whether it executed, recovered, imported, failed, rejected, or cancelled;
- its effective-work provenance without exposing secrets;
- every invocation ordinal and provider-session recovery transition;
- its stable logical effect key in an appropriately protected operator view;
- its commit and immutable outputs when successful;
- the source commit when imported; and
- why a candidate parent commit was or was not compatible when derivation diagnostics are requested.

Ordinary users should see concise states such as “continued existing session,” “reused from run X,” or “executed because input changed.” Internal digests and journal encodings remain optional diagnostic detail.

## Verification Strategy

### Universal lifecycle conformance

Run one lifecycle suite against `agent`, `llm`, `script`, branch, parallel, map, loop, gate, sub-workflow, and `finally` scopes:

- discover;
- start;
- checkpoint;
- commit;
- interrupt;
- continue;
- cancel;
- continue after cancellation; and
- replay.

### Crash-boundary injection

Terminate the process before and after every durable transition:

- before and after dynamic discovery;
- before executable invocation;
- before and after provider-handle checkpoint;
- during same-session upstream recovery;
- after values are stored but before commit;
- immediately after commit;
- during cancellation propagation;
- during `finally`; and
- while importing a derived commit.

Every recovery must either recover the same provider attempt or begin exactly the uncommitted node again. It may never expose partial output, repeat committed work, or invent an unrecorded control decision.

### Identity and derivation properties

Property tests prove that:

- rename and movement do not change work compatibility;
- changed declared inputs or behavior do change it;
- result revalidation permits compatible contract edits;
- identical duplicate instances map one-to-one;
- reordering distinct map items preserves their runtime identity;
- changing one map item does not invalidate unrelated item commits;
- one source commit is never imported twice into one derived run;
- an unchanged structured scope can import as a unit;
- a changed scope can still reuse compatible descendants;
- `fresh` imports nothing; and
- effect keys remain stable only within one logical run instance.

### Adapter recovery conformance

Fake and real adapter fixtures verify that:

- recognized `429`, overload, and temporary-unavailable responses continue the same session;
- the original prompt is never replayed;
- a replacement session is never created under retry semantics;
- the attempt identity and effect key remain stable;
- provider guidance controls waiting when supplied;
- a definitive error or cancellation exits recovery;
- absence of a session handle makes recovery unavailable; and
- every non-upstream failure bypasses this path.

### Concurrent determinism

Randomized scheduling proves that different physical fact order produces the same normalized workflow outcome, child addresses, visible outputs, and replay projection.

### Value and workspace integration

Tests verify that:

- declared files and trees are durable before commit;
- a provider file ID alone cannot satisfy a result;
- a private workspace never becomes recovery evidence;
- published workspaces rematerialize byte-identically;
- orphaned pre-commit content remains invisible; and
- missing or corrupt committed values halt replay and continuation as integrity failures.

### Prestige acceptance fixture

A representative Prestige workflow must demonstrate:

- long multi-agent execution continuing after repeated Tally process interruption;
- committed PDF, image, report, and source-tree values surviving executor loss;
- transient Claude/Codex upstream errors continuing the same session;
- a changed final-report step using `derive` to reuse expensive upstream agent work;
- a reordered or extended map reusing compatible item work;
- `fresh` executing the full workflow again;
- deterministic reconstruction after concurrent branches; and
- complete replay while every executable adapter is disabled.

No test preserves legacy Tally persistence behavior.

## Language and Runtime Surface

| Boundary | Exposed | Hidden |
|---|---|---|
| Workflow YAML | No durability or retry constructs | IDs, addresses, attempts, fingerprints, effect keys, commits, journal, cache policy |
| Operator API/CLI | Fresh run, continue run, derive from run, replay run, cancel run | Storage format, provider commands, matching algorithm encoding |
| Scheduler ↔ run ledger | Semantic lifecycle transitions and reconstructed state | Raw persistence reads/writes |
| Run ledger ↔ adapter | Opaque recovery handle, effective-behavior signature, normalized result and outcome | Claude/Codex command syntax and error parsing |
| Run ledger ↔ value store | Immutable value references and reachability | Filesystem staging and provider upload/download details |

There are no author knobs for attempt count, retry condition, retry delay, backoff curve, journal destination, cache key, runtime address, map identity, effect key, provider session ID, commit policy, snapshot interval, invalidation path, or workspace recovery.

## Software-Design Diagnostic

The design scores **10/10** against the eight software-design-philosophy diagnostics:

| Diagnostic | Result |
|---|---|
| Each module has a one-sentence purpose | Pass: the run ledger preserves and interprets durable execution history |
| Interfaces are simpler than implementations | Pass: four operator intents and semantic scheduler transitions hide matching, recovery, folding, and atomicity |
| Implementations can change without caller changes | Pass: persistence format and provider recovery protocols sit behind owned boundaries |
| Interface documentation states the abstraction | Pass: this specification defines meanings and invariants without prescribing storage methods |
| Design discussion is part of review | Pass: implementation review must check the storage invariants and no-leak surface above |
| Each module hides an important decision | Pass: ledger, value store, and adapters each own non-overlapping knowledge |
| Newcomers can understand boundaries without internals | Pass: the ownership and language/runtime tables define the complete responsibility map |
| Strategic design work is explicit | Pass: identity, commit, lineage, recovery, and replay are fixed before implementation |

The score falls below 10 if implementation splits ledger knowledge into shallow temporal wrappers, exposes raw journal records to the scheduler, duplicates provider classification in generic runtime code, or adds workflow knobs for internal recovery behavior. Those are review failures, not extension points.

## Rejected Complexity

This design deliberately rejects:

- one global cache masquerading as run history;
- mutable historical workflow definitions;
- path-oriented manual invalidation as the core reuse model;
- author-assigned runtime or map-item IDs;
- generic automatic retries;
- retry counts and policy matrices;
- new sessions disguised as retries;
- prompt replay disguised as same-session continuation;
- commits containing partial results;
- provider IDs or remote containers used as committed outputs;
- private-workspace snapshot machinery;
- database or journal-format requirements in the language contract;
- exactly-once external-effect claims;
- silent commit repair or regeneration during replay;
- legacy persistence readers and migration shims; and
- node-kind-specific durability lifecycles.

## Out of Scope

- Exact YAML and CLI spelling for run intents
- Concrete run-ledger API names and package layout
- Persistence engine and physical schema
- Content garbage-collection implementation
- Protected inspection and secret-redaction UI
- Exact provider command lines and error-recognition implementations
- Native script environment-variable names
- General operator tooling for explicit historical repair
- Remote deployment, distributed scheduling, and multi-host consensus

These later designs may choose mechanisms but may not weaken the semantics fixed here.

## Acceptance Criterion

The design is complete when a Prestige-class structured workflow can:

1. commit every successful leaf and scope through one universal boundary;
2. survive process termination at any point without exposing a partial result, restarting uncommitted work only from its defined recovery boundary;
3. continue from recorded commits and same-provider recovery state;
4. continue the same session after a recognized transient upstream provider error without a replacement session;
5. derive an arbitrarily edited child run while importing every reachable compatible agent, LLM, script, and scope commit one-to-one;
6. execute fresh when the user wants no reuse;
7. cancel and later continue without erasing history;
8. reconstruct its complete semantic history with all executors disabled;
9. survive loss of every private workspace and provider environment once declared outputs commit; and
10. do all of this without durability IDs, retry policies, storage paths, invalidation paths, or workspace checkpoints in workflow YAML.

## Evidence Base

This design was checked against:

- Tally's current content-key plan identity, journal, run, and store implementation;
- Tally vNext semantic, value/workspace, and structured-control-flow specifications;
- AWF's node-key, commit, run-state, definition-snapshot, rerun, retry-loop, and conformance code;
- Prestige's watchdog, continuation, validation, and multi-agent retry behavior;
- `docs/research/tally-adapter-capability-contract.md`; and
- `docs/research/tally-native-workspace-isolation.md`.

AWF demonstrates why node-boundary commits, definition capture, and stable dynamic identities matter. It also demonstrates the complexity cost of path-keyed mutable state and manual invalidation. Prestige demonstrates that expensive agent work must survive orchestration-process failure and that provider overload recovery must stay with the same session. Current Tally demonstrates useful content-addressed values but conflates run continuation with cross-run cache reuse. The chosen design keeps the proven capabilities while separating their meanings behind one deeper module.
