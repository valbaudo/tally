# Structured Control Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Dawn vNext canonical definitions with deterministic DAG concurrency, branch, explicit parallel barriers, map, bounded loop, gate, fail-fast cancellation, outcome normalization, and unconditional finally cleanup.

**Architecture:** Add one inward `scheduler` package. It owns structured execution and nothing else: a global external-leaf capacity, graph readiness, stable semantic paths, cancellation/force-stop coordination, boundary publication, and the four terminal outcomes. A narrow `Boundary` port records semantic entry, successful commit, and non-success settlement so #8 can later supply durability; a narrow `LeafRunner` port starts and force-stops external work so #9 and #10 can later supply adapters and scripts. Before the scheduler is added, extend the canonical model with explicit cleanup bindings and add typed literal-to-runtime-value materialization—the two pieces #7 needs that #5/#6 intentionally deferred.

**Tech Stack:** Go 1.26 standard library; existing `value` and `workflow` packages; goroutines, contexts, channels, and one global semaphore for active external leaves; no new dependency.

## Global Constraints

- Implement `docs/superpowers/specs/2026-08-09-structured-control-flow-propagation-design.md` exactly.
- Preserve the approved canonical model and immutable value/workspace kernels. Extend them only where #7 requires runtime literal materialization and explicit `finally` input bindings.
- The clean redesign has no compatibility reader, alias, converter, migration, dual path, or adapter for the current `plan`, `gate`, root `dawn`, legacy backend, AWF, or prior canonical workflow bytes.
- `llm`, `agent`, and `script` are external leaves. `gate` is a deterministic scheduler-owned leaf and never reaches `LeafRunner`.
- `parallel` remains compiler sugar lowered to an ordinary `graph` scope with `OriginParallel`; it does not acquire a second scheduler.
- Every graph and structured scope validates its complete input before entry and commits its complete output before downstream visibility. A non-success publishes no boundary value.
- `Boundary.Enter` happens before any child launch. `Boundary.Commit` is the successful terminal fact. `Boundary.Settle` records only rejected, failed, or cancelled outcomes.
- Use one global positive external-leaf capacity and one non-negative cancellation grace supplied by the operator. Add no workflow-local concurrency, cancellation, timeout, retry, attempt, quorum, merge, or failure-policy setting.
- The scheduler performs no automatic retry. In particular, it never interprets provider errors, sends `continue`, counts invocations, or restarts a leaf.
- Timeout is `Failed` with `TimeoutFailure`, never `Cancelled`. Only an external/ancestor cancellation or an otherwise-unclassified intrinsic backend cancellation is `Cancelled`.
- Fail-fast response stops new descendants and cooperatively cancels active siblings immediately. After the one runtime grace, noncooperative active external leaves are force-stopped.
- Normalize all quiescent intrinsic causes deterministically: external cancellation, mechanical/contract/timeout failure, rejection, intrinsic cancellation; lexical semantic path breaks equal-precedence ties. Parent-induced cancellations never mask their cause.
- Stable semantic paths contain structured components, never slash-delimited strings. Map identity uses canonical item bytes plus duplicate occurrence; list index is data only. Loop identity uses the one-based semantic iteration.
- `finally` starts only after body quiescence, receives a fresh context, and has explicit bindings from original protected inputs, normalized outcome, or fully committed immediate body-child outputs. Body-child cleanup targets are optional because a child may not commit.
- Cleanup cannot contain `gate`, `loop`, or another `finally`; the compiler already owns this restriction. Cleanup cannot expose outputs or replace the body result.
- Finite public definitions and values must not fail through recursive Go stack growth. Nested structured evaluation must cross an asynchronous `runNested` boundary rather than recurse on one Go stack; value traversal uses explicit stacks.
- Add no production dependency and no author-facing syntax. #8 still owns durable reuse/replay and exact encoded addresses; #9 owns provider capability preparation and recovery; #10 owns native process execution; #11 owns sub-workflow runtime linking; #12 owns YAML.
- Use strict red-green-refactor. Every production behavior begins with a focused failing test whose failure is recorded in the task report.
- Preserve unrelated user files and other worktrees.

## File and Module Map

| Module | Files | One responsibility |
| --- | --- | --- |
| `value` | `literal_value.go` | Materialize one already-validated ordinary literal as an immutable runtime value according to its exact target type |
| `workflow` | existing draft/model/compiler/bindings/validation/canonical files | Express, validate, normalize, and canonically encode explicit cleanup inputs |
| `scheduler` | `types.go`, `path.go`, `normalize.go` | Closed execution outcomes, semantic paths, runner/boundary ports, deterministic diagnosis |
| `scheduler` | `control.go`, `engine.go`, `binding.go`, `graph.go`, `leaf.go` | Run lifecycle, global leaf capacity, DAG readiness, value propagation, gate, fail-fast quiescence |
| `scheduler` | `branch.go`, `map.go`, `loop.go`, `finally.go` | One implementation for each structured semantic contribution |
| `scheduler` | focused tests plus `conformance_test.go` | Runtime traces, injected boundary faults, randomized timing, Prestige-shaped composition |

The dependency direction remains inward:

```text
content <- value <- workflow <- scheduler
                             ^
                 Boundary + LeafRunner ports
```

`scheduler` does not import `workspace`: an adapter/script executor returns one already assembled immutable candidate through `LeafRunner`. The scheduler validates and commits that candidate through the universal node boundary.

---

### Task 1: Materialize typed literals as runtime values

**Files:**
- Create: `value/literal_value.go`
- Create: `value/literal_value_test.go`

**Interfaces:**
- Consumes: existing `value.Literal`, `value.Type`, runtime value constructors, and private literal decoded data.
- Produces: `func MaterializeLiteral(literal Literal, target Type) (Value, error)`.
- Guarantees: exact scalar kind, object/map distinction from `target`, ordered lists, no file/tree literals, defensive immutable output, iterative finite traversal.

- [ ] **Step 1: Write failing typed literal materialization tests**

Cover every ordinary target kind and the ambiguity JSON alone cannot resolve:

```go
func TestMaterializeLiteralUsesTargetStructure(t *testing.T) {
	objectType := mustObjectType(t, mustRequired(t, "name", String()))
	mapType := mustMapType(t, String())
	literal := mustLiteral(t, `{"name":"dawn"}`)

	object, err := MaterializeLiteral(literal, objectType)
	if err != nil { t.Fatal(err) }
	mapped, err := MaterializeLiteral(literal, mapType)
	if err != nil { t.Fatal(err) }
	if object.Kind() != ObjectKind || mapped.Kind() != MapKind {
		t.Fatalf("kinds = %v, %v", object.Kind(), mapped.Kind())
	}
}
```

Add table cases for string, integer, number with fractional/exponent spelling, boolean, null, enum members of each scalar kind, nested object/map/list, and `any` using map for JSON objects. Reject an invalid literal, invalid target, target mismatch, file/tree target, undeclared object member, and absent required field. Add a subprocess that materializes 50,000 nested list members under a 1 MiB maximum stack.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./value -run 'TestMaterializeLiteral' -count=1
```

Expected: build failure because `MaterializeLiteral` does not exist.

- [ ] **Step 3: Implement target-directed iterative materialization**

Use explicit construction frames, not recursive calls:

```go
func MaterializeLiteral(literal Literal, target Type) (Value, error) {
	if !target.Valid() || !literal.valid() {
		return Value{}, fmt.Errorf("materialize literal: invalid literal or target")
	}
	if err := target.ValidateLiteral(literal); err != nil {
		return Value{}, fmt.Errorf("materialize literal: %w", err)
	}
	return materializeDecoded(target, literal.decoded)
}
```

`materializeDecoded` uses a task stack and a result stack. `ObjectKind` follows declared fields and constructs `NewObject`; `MapKind` sorts arbitrary JSON keys and constructs `NewMap`; `ListKind` preserves positions; `AnyKind` uses ordinary scalar values, lists, and maps; `EnumKind` uses the member's actual scalar kind. A JSON number without `.`, `e`, or `E` becomes `IntegerKind`; all other JSON numbers become `NumberKind`.

- [ ] **Step 4: Run focused, package, and race tests for GREEN**

```bash
go test ./value -run 'TestMaterializeLiteral' -count=1
go test ./value -count=1
go test -race ./value -count=1
```

Expected: PASS with pristine output.

- [ ] **Step 5: Commit typed literal materialization**

```bash
git add value/literal_value.go value/literal_value_test.go
git commit -m "feat(value): materialize typed runtime literals"
```

---

### Task 2: Make finally inputs explicit in the canonical model

**Files:**
- Modify: `workflow/draft.go`
- Modify: `workflow/model.go`
- Modify: `workflow/compile.go`
- Modify: `workflow/bindings.go`
- Modify: `workflow/validate.go`
- Modify: `workflow/canonical.go`
- Modify: affected `workflow/*_test.go`
- Create: `workflow/finally_bindings_test.go`

**Interfaces:**
- Replaces: `GraphDraft.Finally *GraphDraft` with `GraphDraft.Finally *FinallyDraft` without a compatibility field.
- Produces: `FinallyDraft{Graph GraphDraft, Bindings []CleanupBindingDraft}`.
- Produces: `CleanupSourceKind`, `CleanupInput`, `CleanupOutcome`, `CleanupChild`, `CleanupSourceDraft`, and `CleanupBindingDraft`.
- Produces immutable `Finally.Bindings() []CleanupBinding` and read-only source/target accessors.
- Canonical format becomes `dawn.workflow/2`; no `/1` decoder or dual encoding is added.

- [ ] **Step 1: Write failing cleanup binding model tests**

Compile a protected graph with all three allowed sources:

```go
Finally: &FinallyDraft{
	Graph: cleanupGraph,
	Bindings: []CleanupBindingDraft{
		{From: CleanupSourceDraft{Kind: CleanupInput, Path: []string{"request"}}, To: "request"},
		{From: CleanupSourceDraft{Kind: CleanupOutcome}, To: "outcome"},
		{From: CleanupSourceDraft{Kind: CleanupChild, Child: "prepare", Path: []string{"handle"}}, To: "handle"},
	},
},
```

Assert exact source kind, exact byte-preserving child/path segments, target, runtime-validation bit, defensive copies, source-order-independent canonical bytes, and a changed source/target changing canonical bytes.

Add rejection cases for unknown source kind, a path on `CleanupOutcome`, a child name on input/outcome, missing child, missing source path, incompatible types, duplicate target, unbound required cleanup input, optional protected input feeding required cleanup input, and every body-child binding into a required cleanup target.

- [ ] **Step 2: Run the cleanup model tests and verify RED**

```bash
go test ./workflow -run 'Test(FinallyBinding|CompileFinally)' -count=1
```

Expected: build failure because the cleanup binding types do not exist.

- [ ] **Step 3: Add the source-neutral and immutable cleanup records**

Use a closed source union:

```go
type CleanupSourceKind uint8

const (
	CleanupInput CleanupSourceKind = iota + 1
	CleanupOutcome
	CleanupChild
)

type FinallyDraft struct {
	Graph    GraphDraft
	Bindings []CleanupBindingDraft
}

type CleanupSourceDraft struct {
	Kind  CleanupSourceKind
	Child string
	Path  []string
}

type CleanupBindingDraft struct {
	From CleanupSourceDraft
	To   string
}
```

The immutable counterparts retain only compiled semantic data. The normalized outcome source type is exactly the enum over JSON string literals `"succeeded"`, `"rejected"`, `"failed"`, and `"cancelled"`.

- [ ] **Step 4: Compile and validate cleanup bindings**

Add `compileFinally(protected Graph, draft *FinallyDraft) (Finally, error)`. It compiles the cleanup graph, resolves each source against the protected graph input contract or immediate child output contract, resolves the target against cleanup inputs, calls `value.CheckAssignable`, and enforces one source per target.

Required target rules are exact:

- protected required input may feed required or optional target;
- protected optional input may feed only optional target;
- outcome may feed a required or optional exact outcome-enum target;
- body-child output may feed only an optional target;
- every required cleanup input has exactly one binding.

The cleanup graph still has an explicitly empty output contract and the existing recursive prohibition on gate, loop, and finally.

- [ ] **Step 5: Canonicalize cleanup bindings and migrate all current drafts**

Change `graphWire.Cleanup` to:

```go
type finallyWire struct {
	Graph    graphWire            `json:"graph"`
	Bindings []cleanupBindingWire `json:"bindings"`
}
```

Sort bindings by source kind, child bytes, source path bytes, and target bytes. Update every existing test fixture from `Finally: &GraphDraft{...}` to `Finally: &FinallyDraft{Graph: GraphDraft{...}}`. Keep all previous cleanup restrictions and canonical-order tests meaningful.

- [ ] **Step 6: Run workflow verification for GREEN**

```bash
go test ./workflow -count=1
go test -race ./workflow -count=1
go test ./value ./workflow ./workspace -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit explicit cleanup bindings**

```bash
git add workflow value workspace
git commit -m "feat(workflow): bind explicit cleanup inputs"
```

---

### Task 3: Define scheduler paths, outcomes, ports, and normalization

**Files:**
- Create: `scheduler/types.go`
- Create: `scheduler/path.go`
- Create: `scheduler/normalize.go`
- Create: `scheduler/types_test.go`
- Create: `scheduler/normalize_test.go`

**Interfaces:**
- Produces immutable `Path`, `Component`, `ComponentKind`, and component accessors for authored child, branch case, map item+occurrence, loop iteration, and cleanup.
- Produces `FailureKind` values `MechanicalFailure`, `ContractFailure`, `TimeoutFailure`, and `CleanupFailure`.
- Produces immutable `Diagnostic`, `Result`, and accessors for status, committed output, primary diagnostic, and ordered secondary diagnostics.
- Produces `LeafRequest`, `LeafCompletion`, `LeafExecution`, `LeafRunner`, `Instance`, `Boundary`, and `Policy`.
- `Boundary` exposes only `Enter(context.Context, Instance) error`, `Commit(context.Context, Instance, value.Value) error`, and `Settle(context.Context, Instance, Result) error`.

- [ ] **Step 1: Write failing semantic path tests**

Construct paths whose strings and map items contain `/`, NUL, invalid UTF-8, dots, and equal prefixes. Assert accessors return defensive copies and `comparePath` is a strict deterministic lexical order over structured components. Prove map components with the same canonical item bytes differ only by duplicate occurrence and loop iteration is one-based.

- [ ] **Step 2: Write failing outcome normalization tests**

For randomized permutations of the same causes, assert the same primary and secondary order:

```go
func TestNormalizeIgnoresCompletionOrder(t *testing.T) {
	causes := []diagnostic{
		failed(path("z"), TimeoutFailure, errors.New("late timeout")),
		rejected(path("a"), "policy"),
		failed(path("b"), MechanicalFailure, errors.New("mechanical")),
		parentCancelled(path("c")),
	}
	for seed := int64(0); seed < 100; seed++ {
		got := normalize(shuffled(seed, causes), nil)
		if got.primary.path.lastAuthoredName() != "b" { t.Fatalf("seed %d", seed) }
	}
}
```

Cover external cancellation outranking failure, failure/timeout/contract outranking rejection, rejection outranking intrinsic cancellation, parent-induced cancellation never becoming primary when an intrinsic cause exists, and lexical path breaking equal precedence. Secondary diagnostics must also be stable.

- [ ] **Step 3: Run scheduler type tests and verify RED**

```bash
go test ./scheduler -run 'Test(Path|Normalize|Result|LeafCompletion)' -count=1
```

Expected: build failure because `scheduler` does not exist.

- [ ] **Step 4: Implement closed immutable execution records**

Use constructors rather than permitting invalid result combinations:

```go
type LeafRunner interface {
	Start(context.Context, LeafRequest) (LeafExecution, error)
}

type LeafExecution interface {
	Done() <-chan LeafCompletion
	ForceStop() error
}

type Boundary interface {
	Enter(context.Context, Instance) error
	Commit(context.Context, Instance, value.Value) error
	Settle(context.Context, Instance, Result) error
}

type Policy struct {
	Capacity          int
	CancellationGrace time.Duration
}
```

`LeafCompletion` has exported constructors for success, failure, timeout, and intrinsic cancellation. It has no rejected constructor because only the scheduler-owned gate rejects. A malformed completion received from a runner becomes a mechanical protocol failure.

- [ ] **Step 5: Implement deterministic diagnosis**

Keep parent-induced and cleanup flags private. `normalize` filters consequence cancellations from primary arbitration, ranks intrinsic causes by the approved precedence, and sorts equal-rank candidates by `comparePath`. The returned `Result` exposes output only for `Succeeded`; all other statuses return no output.

- [ ] **Step 6: Run focused and race tests for GREEN**

```bash
go test ./scheduler -run 'Test(Path|Normalize|Result|LeafCompletion)' -count=1
go test -race ./scheduler -run 'Test(Path|Normalize|Result|LeafCompletion)' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit scheduler semantic types**

```bash
git add scheduler/types.go scheduler/path.go scheduler/normalize.go scheduler/types_test.go scheduler/normalize_test.go
git commit -m "feat(scheduler): define deterministic execution outcomes"
```

---

### Task 4: Execute graph DAGs, external leaves, and deterministic gates

**Files:**
- Create: `scheduler/control.go`
- Create: `scheduler/engine.go`
- Create: `scheduler/binding.go`
- Create: `scheduler/graph.go`
- Create: `scheduler/leaf.go`
- Create: `scheduler/testkit_test.go`
- Create: `scheduler/graph_test.go`
- Create: `scheduler/gate_test.go`

**Interfaces:**
- Produces: `func New(runner LeafRunner, boundary Boundary, policy Policy) (*Scheduler, error)`.
- Produces: `func (*Scheduler) Start(context.Context, workflow.Definition, value.Value) (*Execution, error)`.
- Produces idempotent `Execution.Cancel()`, `Execution.ForceStop()`, and `Execution.Wait() Result`.
- Consumes: Task 3 ports, canonical graph edges/literals/contracts, and `value.MaterializeLiteral`.

- [ ] **Step 1: Write failing graph concurrency and dataflow tests**

Use a controllable real fake runner whose started executions expose channels. Prove:

- two independent ready leaves start before either completes;
- a completion-only edge delays its consumer;
- a binding edge supplies the exact nested source value to its target port;
- a boundary-to-boundary binding passes input directly to output;
- source order does not change start eligibility or output;
- capacity `1` permits only one external leaf at a time and capacity `2` permits two;
- `Boundary.Commit` happens before a result becomes available to its consumer;
- an input/output validation or boundary failure produces `Failed` and no public output.

The fake boundary records typed events, not mock call expectations; assertions inspect the scheduler's observable event trace and final result.

- [ ] **Step 2: Write failing fail-fast and force-stop tests**

Start three independent leaves. Complete one as failed, leave one cooperatively cancellable, and leave one noncooperative. Assert no newly ready descendant starts, cancellation reaches both active siblings, the scheduler waits the exact supplied grace, calls `ForceStop` only for the noncooperative execution, waits for quiescence, and reports the original failure as primary with sibling cancellations secondary.

Add external cancel during an intrinsic failure and assert `Cancelled` wins. Add an external `ForceStop` and assert active leaves are force-stopped immediately.

- [ ] **Step 3: Write failing gate tests**

Compile a gate with true and false input. True commits an empty object and succeeds; false settles as `Rejected`, retains the optional reason, commits nothing, and never calls `LeafRunner`. A malformed runner rejection for an ordinary external leaf becomes `Failed(MechanicalFailure)`.

- [ ] **Step 4: Run graph and gate tests and verify RED**

```bash
go test ./scheduler -run 'Test(Graph|DAG|Capacity|FailFast|ExternalCancel|Gate)' -count=1
```

Expected: build failure because `Scheduler` and `Execution` do not exist.

- [ ] **Step 5: Implement run control and the external-leaf boundary**

`Scheduler` owns one semaphore of `Policy.Capacity`. `Start` rejects nil context, invalid dependencies, non-positive capacity, negative grace, zero definition, and a root input that fails the root contract. Root input failure happens before `Boundary.Enter`, so root cleanup is not entered.

`Execution` owns one run controller. The caller context and `Cancel` produce the single external cancellation intent; `ForceStop` is a distinct stronger signal. A leaf gets a derived cooperative-cancellation context. After cancellation, `runLeaf` waits for completion or the grace timer, calls `LeafExecution.ForceStop`, and treats successful force-stop as quiescence. It always releases the global capacity token.

- [ ] **Step 6: Implement immutable value propagation**

`binding.go` provides private iterative helpers:

```go
func selectValue(root value.Value, path []string) (value.Value, bool)
func objectValue(fields map[string]value.Value) (value.Value, error)
func applyGraphInputs(graph workflow.Graph, input value.Value) (*graphState, error)
```

Resolve literals with their declared target types through `value.MaterializeLiteral`. Build each node's complete input object only when all predecessor child endpoints have committed. Omit an optional target when its optional source is absent. Validate the complete target contract before starting the node.

- [ ] **Step 7: Implement one DAG event loop**

`runGraph` calls `Boundary.Enter`, applies boundary bindings/literals, launches every ready child through `runNested`, and consumes child completions on one channel. On first observed non-success it marks fail-fast, cancels active children, and launches no further child. It still drains every active child before normalization. On all-success, it assembles and validates the graph output, calls `Boundary.Commit`, and only then returns success.

Every nested node evaluation crosses this helper so structured depth does not consume one Go call stack:

```go
func (r *runState) runNested(ctx context.Context, fn func(context.Context) Result) <-chan Result {
	done := make(chan Result, 1)
	go func() { done <- fn(ctx); close(done) }()
	return done
}
```

- [ ] **Step 8: Implement gate as a local leaf**

`runGate` validates its exact input contract, reads `passed` and optional `reason`, and either commits the empty object or settles a rejection. It never acquires external capacity and never invokes the runner.

- [ ] **Step 9: Run focused, package, and race verification for GREEN**

```bash
go test ./scheduler -run 'Test(Graph|DAG|Capacity|FailFast|ExternalCancel|Gate)' -count=1
go test ./scheduler -count=1
go test -race ./scheduler -count=1
go test ./value ./workflow ./workspace ./scheduler -count=1
```

Expected: PASS.

- [ ] **Step 10: Commit the graph scheduler**

```bash
git add scheduler
git commit -m "feat(scheduler): execute canonical graph DAGs"
```

---

### Task 5: Execute exhaustive branch and explicit parallel scopes

**Files:**
- Create: `scheduler/branch.go`
- Create: `scheduler/branch_test.go`
- Create: `scheduler/parallel_test.go`
- Modify: `scheduler/engine.go`

**Interfaces:**
- Consumes: canonical `Branch`, graph scopes, semantic paths, and `runNested`.
- Produces: branch case selection from the exact runtime scalar and ordinary graph execution for `OriginParallel` scopes.

- [ ] **Step 1: Write failing branch conformance tests**

For boolean and mixed scalar enum selectors, prove exactly one case enters, the selected case path is recorded before its first child, and the unselected case has no runtime event. Re-run the same definition/input under randomized runner timing and assert the identical selected path and output. Inject selected-case rejection, failure, timeout, and cancellation and assert exact propagation with no branch commit.

Use string enum members containing quotes, slash, NUL, and invalid UTF-8 bytes where the value model permits them; selection compares scalar semantics rather than parsing a path string.

- [ ] **Step 2: Write failing explicit parallel tests**

Compile an author `ParallelDraft` with two named branches. Prove both eligible branches overlap, the parent waits for both, explicit output bindings alone determine the result, and no tuple/object/filesystem merge is synthesized. Fail one branch while another returns rejection and assert deterministic failure precedence after both quiesce.

- [ ] **Step 3: Run branch/parallel tests and verify RED**

```bash
go test ./scheduler -run 'Test(Branch|Parallel)' -count=1
```

Expected: FAIL because structured branch dispatch is absent.

- [ ] **Step 4: Implement exact selector resolution**

`runBranch` enters the branch scope, validates input, extracts the selector, and maps it to a case name: booleans use `false`/`true`; enum selectors compare the runtime scalar's ordinary canonical JSON bytes and exact scalar kind with the compiled enum members. It enters only the selected case graph at `path.BranchCase(caseName)`, then validates/commits the case output as the branch output.

- [ ] **Step 5: Route graph scopes through the same graph engine**

An authored graph and lowered explicit parallel both call `runGraph`. `OriginParallel` changes provenance and semantic grouping only; it adds no branch-specific goroutine pool, settings, output collection, or normalization path. Immediate named child paths already supply the parallel branch identity.

- [ ] **Step 6: Run focused and race tests for GREEN**

```bash
go test ./scheduler -run 'Test(Branch|Parallel)' -count=1
go test -race ./scheduler -run 'Test(Branch|Parallel)' -count=1
go test ./workflow ./scheduler -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit branch and parallel execution**

```bash
git add scheduler/branch.go scheduler/branch_test.go scheduler/parallel_test.go scheduler/engine.go
git commit -m "feat(scheduler): execute branch and parallel scopes"
```

---

### Task 6: Execute dynamic map with stable identity and ordered fan-in

**Files:**
- Create: `scheduler/map.go`
- Create: `scheduler/map_test.go`
- Modify: `scheduler/engine.go`

**Interfaces:**
- Consumes: canonical `Map`, runtime list values, map semantic path components, global external-leaf capacity, and `runNested`.
- Produces: one body graph per item and a single declared list result in original input order.

- [ ] **Step 1: Write failing map success and identity tests**

Cover:

- empty input commits the declared empty list without runner calls;
- randomized reverse completion still collects output in original input order;
- reordering distinct items preserves each item's semantic path;
- byte-identical duplicates receive occurrences `0`, `1`, `2` in input order;
- changing only `index` data can change the work input but never the semantic item component;
- nested map items and arbitrary string/map bytes use canonical value bytes, not rendered JSON or filesystem paths.

- [ ] **Step 2: Write failing map fail-fast tests**

Make one item reject, another fail, one complete successfully before fail-fast, and one require force-stop. Assert response stops further item instantiation as soon as the scheduler observes non-success, active siblings settle, failure outranks rejection, the already committed child remains in the boundary trace, and the map publishes no result.

- [ ] **Step 3: Run map tests and verify RED**

```bash
go test ./scheduler -run 'TestMap' -count=1
```

Expected: FAIL because map dispatch is absent.

- [ ] **Step 4: Implement stable item discovery and concurrent execution**

`runMap` enters the map scope, validates that the sole collection input is a runtime list, and counts duplicate occurrences by `string(item.Canonical())`. For each item it constructs exactly:

```go
bodyInput := objectValue(map[string]value.Value{
	"item": item,
	"index": mustInteger(strconv.Itoa(index)),
})
```

Before launching a body, append a map component containing copied canonical item bytes and the current duplicate occurrence. Launch via `runNested`. Between launches, non-blockingly consume already available completions so an observed failure prevents later discovery.

- [ ] **Step 5: Implement ordered collection and fail-fast settlement**

Store each successful body object at its original index. On success, construct `value.NewList(results...)`, wrap it under the map's declared result field, validate, commit, and return. On non-success, cancel active items, drain them, normalize all intrinsic causes, settle the map, and return no output.

- [ ] **Step 6: Run focused, race, and package tests for GREEN**

```bash
go test ./scheduler -run 'TestMap' -count=1
go test -race ./scheduler -run 'TestMap' -count=1
go test ./value ./workflow ./scheduler -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit map execution**

```bash
git add scheduler/map.go scheduler/map_test.go scheduler/engine.go
git commit -m "feat(scheduler): execute stable dynamic maps"
```

---

### Task 7: Execute bounded semantic loops

**Files:**
- Create: `scheduler/loop.go`
- Create: `scheduler/loop_test.go`
- Modify: `scheduler/engine.go`

**Interfaces:**
- Consumes: canonical positive literal maximum and termination path, runtime input/output objects, semantic loop iteration components.
- Produces: sequential body execution with exact `initial`, optional `previous`, and one-based `iteration` values.

- [ ] **Step 1: Write failing loop dataflow and exhaustion tests**

Prove iteration one omits `previous`, every iteration receives byte-identical `initial`, later iterations receive the entire prior successful output as `previous`, and `iteration` is the exact one-based integer. A true verdict returns and commits immediately. An all-false loop commits the final successful output at the maximum. A maximum of one executes exactly once.

- [ ] **Step 2: Write failing loop non-success tests**

After one successful false iteration, make the next iteration fail, reject, time out, or cancel. Assert the loop propagates that outcome, publishes no older revision, and never creates the next semantic iteration. Assert invocation-level runner behavior does not alter the one-based iteration path; the scheduler has no retry code.

- [ ] **Step 3: Run loop tests and verify RED**

```bash
go test ./scheduler -run 'TestLoop' -count=1
```

Expected: FAIL because loop dispatch is absent.

- [ ] **Step 4: Implement sequential semantic iteration**

`runLoop` enters the loop scope and keeps the original input object unchanged. For `iteration := 1; iteration <= loop.Maximum(); iteration++`, construct the exact body input, append a loop component, execute one body through `runNested`, and stop immediately on non-success. Extract the required termination boolean through the compiled path. A malformed value is a contract failure even if the body runner claimed success.

- [ ] **Step 5: Commit the last successful output correctly**

On true verdict or false exhaustion, validate the complete body result against loop outputs, commit it once at the loop scope path, and return it. Never synthesize rejection from a false final verdict; a separate downstream gate owns that policy decision.

- [ ] **Step 6: Run focused and race tests for GREEN**

```bash
go test ./scheduler -run 'TestLoop' -count=1
go test -race ./scheduler -run 'TestLoop' -count=1
go test ./workflow ./scheduler -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit bounded loop execution**

```bash
git add scheduler/loop.go scheduler/loop_test.go scheduler/engine.go
git commit -m "feat(scheduler): execute bounded semantic loops"
```

---

### Task 8: Run finally during structured unwinding

**Files:**
- Create: `scheduler/finally.go`
- Create: `scheduler/finally_test.go`
- Modify: `scheduler/control.go`
- Modify: `scheduler/graph.go`
- Modify: `scheduler/binding.go`

**Interfaces:**
- Consumes: Task 2 cleanup bindings, protected input, committed immediate body-child outputs, normalized body result, and run control.
- Produces: fresh cleanup contexts, cleanup input construction, exact precedence, and innermost-first unwinding.

- [ ] **Step 1: Write failing cleanup guarantee tests**

Run cleanup after body success, rejection, mechanical failure, timeout, intrinsic cancellation, and external cancellation. Assert cleanup starts only after every active body descendant reports completion or force-stop. Inject a failure before the first body child starts after graph entry and assert cleanup still runs. Inject invalid root input before entry and assert cleanup does not run.

- [ ] **Step 2: Write failing cleanup input tests**

Bind original protected input, normalized outcome, one committed body-child output, and one uncommitted body-child output. Assert cleanup receives exact original bytes, one of the four exact enum values, the committed optional value, and omission—not zero data—for the unavailable optional child. Assert no other body value leaks into cleanup.

- [ ] **Step 3: Write failing precedence and nested-unwind tests**

Cover:

- body success plus cleanup failure becomes `Failed(CleanupFailure)`;
- body failure/rejection/cancellation remains primary when cleanup also fails;
- cleanup failure becomes a secondary diagnostic in those cases;
- a later external cancel during cleanup becomes primary `Cancelled`;
- nested protected scopes run cleanup innermost first;
- external force-stop during cleanup stops active cleanup leaves and may end the guarantee.

- [ ] **Step 4: Run finally tests and verify RED**

```bash
go test ./scheduler -run 'TestFinally' -count=1
```

Expected: FAIL because graph execution does not run canonical cleanup.

- [ ] **Step 5: Implement fresh cleanup control**

The run controller can create a context that ignores an already-recorded cancellation but still observes a cancellation that first arrives after cleanup starts. It always observes `Execution.ForceStop`. Use `context.WithoutCancel` to preserve context values without inheriting the cancelled body signal. Do not reset the external outcome fact.

- [ ] **Step 6: Assemble only declared cleanup inputs**

For every `CleanupBinding`:

- `CleanupInput` selects from the protected graph's original input;
- `CleanupOutcome` supplies `value.NewString(string(body.Status()))`;
- `CleanupChild` selects only from the named immediate child's committed result and omits the optional target if unavailable.

Validate the assembled object against cleanup inputs before entering cleanup. A construction or validation problem is an intrinsic cleanup failure.

- [ ] **Step 7: Integrate cleanup before parent publication**

`runGraph` holds a successful body candidate without committing the graph scope, waits for body quiescence, runs cleanup at `path.Cleanup()`, then applies cleanup precedence. Only body success plus cleanup success permits the protected graph commit. Non-success settles after cleanup. Child scopes already complete their own cleanup before returning, giving innermost-first unwinding naturally.

- [ ] **Step 8: Run focused, race, and cross-package tests for GREEN**

```bash
go test ./scheduler -run 'TestFinally' -count=1
go test -race ./scheduler -run 'TestFinally' -count=1
go test ./value ./workflow ./workspace ./scheduler -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit finally execution**

```bash
git add scheduler/finally.go scheduler/finally_test.go scheduler/control.go scheduler/graph.go scheduler/binding.go
git commit -m "feat(scheduler): unwind unconditional cleanup"
```

---

### Task 9: Prove full #7 conformance and harden finite execution

**Files:**
- Create: `scheduler/conformance_test.go`
- Create: `scheduler/stack_test.go`
- Modify: scheduler production files only when the tracer exposes a real mismatch; modify iterative `workflow` traversal only if the finite structured-depth subprocess first proves the existing compiler path unsafe
- Update: `docs/superpowers/specs/2026-08-09-structured-control-flow-propagation-design.md` with an implementation-evidence section

**Interfaces:**
- Consumes: Tasks 1–8 public APIs and the approved #7 verification strategy.
- Produces: one Prestige-shaped runtime trace and direct evidence for every branch/parallel/map/loop/gate/finally/cross-construct bullet.

- [ ] **Step 1: Write the complete Prestige-shaped runtime tracer**

Compile and execute one definition containing:

- independent static and dynamic analysis branches overlapping automatically;
- an explicit named parallel review barrier;
- map fan-out over duplicated and distinct documents with randomized completion;
- a worker/judge loop that exhausts false once and then passes;
- a separate gate consuming the loop verdict while candidate data bypasses the gate;
- a conditional branch;
- nested finally cleanup receiving optional committed evidence;
- a final aggregation output.

The fake runner uses exact typed inputs and immutable outputs. The fake boundary records entry, commit, and settlement events. Assert the final result, stable semantic paths, map result order, loop inputs, gate isolation, commit-before-visibility, cleanup order, and absence of any scope output after non-success.

- [ ] **Step 2: Run the tracer before changing production**

```bash
go test ./scheduler -run 'TestPrestigeStructuredControlTrace' -count=1
```

Expected: PASS. If it fails, record the exact mismatch as RED, make only the smallest production correction that restores the approved semantics, and record the matching GREEN run.

- [ ] **Step 3: Add randomized arbitration and cancellation stress**

Run the same fault set across at least 100 deterministic completion permutations. Assert byte-identical primary path/status/failure kind and secondary order. Repeat the fail-fast graph, map, and cleanup cancellation cases under `-race` and assert every started external execution reaches completion or force-stop exactly once.

- [ ] **Step 4: Add finite-depth and finite-breadth subprocess tests**

Under a 1 MiB maximum Go stack, execute a 50,000-level nested graph/branch chain ending in one gate. Execute a 50,000-item empty-body map with capacity one. Neither test may impose or discover an author-visible depth/item limit; both must terminate with the exact contracted output. A resource failure from the operating system is reported as mechanical failure, never converted into a language limit.

- [ ] **Step 5: Run the complete verification matrix**

```bash
go test ./scheduler -count=1
go test -race ./scheduler -count=1
go test ./value ./workflow ./workspace ./scheduler -count=1
go test -race ./value ./workflow ./workspace ./scheduler -count=1
go test ./... -count=1
go vet ./...
git diff --check
```

Expected: every command exits zero with pristine output.

- [ ] **Step 6: Record implementation evidence in the approved spec**

Append a short dated section naming the implementation commit range and the exact conformance commands. Do not rewrite approved product decisions and do not mark #8–#12 implemented.

- [ ] **Step 7: Commit the conformance proof**

```bash
git add scheduler/conformance_test.go scheduler/stack_test.go scheduler docs/superpowers/specs/2026-08-09-structured-control-flow-propagation-design.md
git commit -m "test(scheduler): prove structured control conformance"
```

---

## Plan Self-Review

- **Spec coverage:** Tasks 4–8 directly own universal lifecycle, DAG concurrency, branch, explicit parallel, map, loop, gate, normalization, cancellation, and finally. Task 9 traces every verification section and the Prestige lowering.
- **Deferred boundaries:** `Boundary` deliberately supplies no reuse, invocation counters, journal encoding, or replay; `LeafRunner` deliberately supplies no provider capability schema or transient recovery. Those remain #8/#9 rather than hidden partial implementations here.
- **Type consistency:** Every scope consumes and produces one immutable `value.Value` object satisfying its canonical contract. Successful `LeafCompletion` is the only external candidate input; successful `Boundary.Commit` is the only downstream visibility point.
- **No artificial limitations:** Only operator external-leaf capacity, runtime cancellation grace, and the authored positive loop maximum exist. There is no construct-local capacity, retry count, depth limit, map limit, default timeout, or arbitrary syntax restriction.
- **No placeholders:** Every task has exact files, interfaces, failure cases, commands, and commit boundaries.
