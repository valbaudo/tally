# Native Script Execution Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Dawn's clean vNext native `script` executor: literal commands enter through one typed ABI, run in an owned native execution group, and either return one fully captured candidate value or one honest terminal outcome.

**Architecture:** Add one deep `script.Executor` that owns invocation roots, ABI files, environment construction, native execution, output validation, capture, and cleanup. Put reusable operating-system process lifecycle in `nativeexec`, below both scripts and future CLI-backed agent adapters; expose no current-`dawn.Backend` or AWF compatibility layer. The vNext value runtime supplies immutable artifact materialization and atomic candidate construction through one narrow port.

**Tech Stack:** Go 1.26 standard library, macOS/Linux process groups and Unix file descriptors, existing Dawn content-addressed storage through the vNext value boundary.

## Global Constraints

- Implement `docs/superpowers/specs/2026-08-09-native-script-execution-contract-design.md`.
- Treat GitHub #5–#9 implementations as prerequisites for scheduler, value, workspace, ledger, effect-key, cancellation, and commit semantics.
- Do not adapt `script.Executor` to current `dawn.Backend`, `plan.Step`, map-shaped `dawn.Result`, current workspace-only refs, or AWF fields.
- Canonical commands are literal argv. `/bin/sh -c` lowering belongs to compilation; the executor never interpolates values or invokes a shell implicitly.
- The author surface remains command, requested external environment names, and an optional wall deadline.
- Stdin is closed. Stdout and stderr are diagnostics only.
- Add no default script timeout, idle timeout, attempt count, exit-code policy, retry loop, provider-error parser, sandbox knob, Docker branch, or exactly-once claim.
- A successful process must leave its owned execution group quiet before validation and capture start.
- Coordinator loss must not leave an owned process group running while a replacement attempt starts.
- The local backend states its non-isolating policy; stronger backends fail instead of silently degrading.
- Use red-green-refactor and commit every task independently.
- Preserve unrelated untracked files and make no backward-compatibility change.

## Required vNext Port

The #6 implementation supplies one value boundary with these semantics. If final package names differ, update imports and signatures in this plan before execution; do not add a translation layer solely to preserve an abandoned API.

```go
type ImmutableValues interface {
	MaterializeFile(ctx context.Context, value Value, destination string) error
	MaterializeTree(ctx context.Context, value Value, destination string) error
	CaptureFile(ctx context.Context, slot string, sensitive [][]byte) (Value, error)
	CaptureTree(ctx context.Context, root string, sensitive [][]byte) (Value, error)
}

type CandidateBuilder interface {
	BuildScriptCandidate(ctx context.Context, contract Contract, outputJSON []byte,
		manifest script.Manifest, captured map[string]Value, workspace *Value,
		sensitive [][]byte) (Value, error)
}

type AttemptContext struct {
	EffectKey string
	Grace     time.Duration
}
```

`Value` is #6's one recursive immutable value type, `Contract` is its exact output contract, and the returned `Value` is the sole candidate presented to #8's commit boundary. `script` must not define a second durable artifact or result model.

The vNext value runtime resolves an input `Value` into `Request.InputJSON` before dispatch, replacing each file/tree leaf with its exact `input:...` manifest reference. `script` owns the physical manifestation of those references, not a second recursive value encoder.

## File Structure

| File | Responsibility |
| --- | --- |
| `nativeexec/types.go` | Owned-process request, diagnostics, and terminal outcome |
| `nativeexec/local.go` | Parent-side runner and cancellation protocol |
| `nativeexec/supervisor_unix.go` | Process group, TERM/KILL sequence, leak detection, reaping |
| `nativeexec/supervisor_protocol.go` | Private control/result protocol for Dawn re-exec |
| `script/types.go` | Resolved request, artifact/slot descriptors, executor outcomes |
| `script/abi.go` | Exact `dawn.script/1` manifest and reserved references |
| `script/invocation.go` | Private roots, materialization, fixed ABI files, cleanup |
| `script/environment.go` | Native baseline, requested names, diagnostic redaction |
| `script/executor.go` | Prepare/run/quiesce/validate/capture orchestration |
| `script/candidate.go` | Output decoding, slot capture, workspace publication |
| `cmd/dawn/main.go` | Early private re-exec entry for the supervisor |

Keep process mechanics in `nativeexec` and typed workflow mechanics in `script`. Do not split ABI preparation, validation, and capture into public packages; they share one invariant and change together.

---

### Task 1: Lock the resolved script and owned-process interfaces

**Files:**
- Create: `nativeexec/types.go`
- Create: `script/types.go`
- Test: `nativeexec/types_test.go`
- Test: `script/types_test.go`

**Interfaces:**
- Produces: `nativeexec.Request`, `nativeexec.Outcome`, `nativeexec.DiagnosticSink`, `nativeexec.Runner`.
- Produces: `script.Request`, `script.InputArtifact`, `script.OutputSlot`, `script.Executor`, `script.Outcome`, `script.Failure`.
- Consumes: prerequisite `Value`, `Contract`, `ImmutableValues`, `CandidateBuilder`, and `AttemptContext`.

- [ ] **Step 1: Write failing request and interface tests**

```go
func TestRequestRequiresResolvedExecutionFacts(t *testing.T) {
	tests := []struct {
		name string
		mutate func(*Request)
		want string
	}{
		{"argv", func(r *Request) { r.Argv = nil }, "argv"},
		{"input JSON", func(r *Request) { r.InputJSON = nil }, "input JSON"},
		{"output contract", func(r *Request) { r.OutputContract = Contract{} }, "output contract"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := validRequest()
			tc.mutate(&r)
			if err := r.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}
```

Add a neighboring test proving `Deadline == 0` is valid and means no deadline. Add compile-time assertions that fakes satisfy `DiagnosticSink` and `Runner`.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./nativeexec ./script -run 'TestRequest|TestInterfaces' -count=1
```

Expected: FAIL because neither package nor its types exists.

- [ ] **Step 3: Define minimal process and script types**

In `nativeexec/types.go` define:

```go
type Stream string
const ( Stdout Stream = "stdout"; Stderr Stream = "stderr" )

type DiagnosticSink interface {
	WriteDiagnostic(stream Stream, payload []byte) error
}

type Request struct {
	Argv []string
	Dir string
	Env []string
	Deadline time.Duration // zero means none
	Grace time.Duration    // runtime policy, never leaf syntax
}

type State string
const (
	Exited State = "exited"
	Signaled State = "signaled"
	Cancelled State = "cancelled"
	TimedOut State = "timed_out"
	Leaked State = "leaked_processes"
	LaunchFailed State = "launch_failed"
)

type Provenance struct { Backend string; Policy string }
type Outcome struct {
	State State
	ExitCode int
	Signal string
	Err error
	Provenance Provenance
}
type Runner interface { Run(context.Context, Request, DiagnosticSink) Outcome }
```

In `script/types.go` define:

```go
type ArtifactKind string
const ( File ArtifactKind = "file"; Tree ArtifactKind = "tree" )

type InputArtifact struct {
	ID, ValuePath string
	Kind ArtifactKind
	Value Value
	LogicalName, MediaType, Digest string
	Bytes int64
}

type OutputSlot struct { ID, ValuePath string; Kind ArtifactKind; Dynamic bool }

type Request struct {
	Argv []string
	InputJSON []byte
	Inputs []InputArtifact
	Outputs []OutputSlot
	Base *InputArtifact
	PublishWorkspace bool
	OutputContract Contract
	Environment []string
	Deadline time.Duration
}

type Status string
const ( Succeeded Status = "succeeded"; Failed Status = "failed"; Cancelled Status = "cancelled" )

type FailureKind string
const (
	FailureLaunch FailureKind = "launch"
	FailureExit FailureKind = "exit"
	FailureSignal FailureKind = "signal"
	FailureTimeout FailureKind = "timeout"
	FailureLeakedProcesses FailureKind = "leaked_processes"
	FailureOutput FailureKind = "output"
	FailureCapture FailureKind = "capture"
)

type Outcome struct { Status Status; Candidate Value; Failure *Failure }
```

`Failure` implements `error` and carries kind, exit code or signal, cause, and non-secret detail. `Executor` contains only values, candidate builder, native runner, runtime root, environment lookup, and baseline provider. It has no retry or attempt-limit field.

- [ ] **Step 4: Run focused tests for GREEN**

```bash
go test ./nativeexec ./script -run 'TestRequest|TestInterfaces' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the contract slice**

```bash
git add nativeexec/types.go nativeexec/types_test.go script/types.go script/types_test.go
git commit -m "feat(script): define the native execution boundary"
```

---

### Task 2: Materialize one invocation and emit the exact ABI

**Files:**
- Create: `script/abi.go`
- Create: `script/invocation.go`
- Test: `script/abi_test.go`
- Test: `script/invocation_test.go`

**Interfaces:**
- Consumes: `script.Request`, `ImmutableValues.MaterializeFile`, `ImmutableValues.MaterializeTree`.
- Produces: `Manifest`, `Layout`, `prepareInvocation(context.Context, string, Request) (*Invocation, error)`; the string is the ledger-owned effect key.

- [ ] **Step 1: Add failing ABI golden tests**

Construct input containing one PDF and a two-item file list. Assert ABI `dawn.script/1`, workspace and effect key, exact input IDs, static output slots, and dynamic namespace `output:/pages/*`. Compare decoded `DAWN_INPUT` and `DAWN_MANIFEST` with golden values and assert `DAWN_OUTPUT` does not yet exist.

Add cases for empty workspace, one base tree, file/tree outputs, logical filenames with spaces, duplicate manifest IDs, and a base of kind `file`. Invalid cases must fail before materialization.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./script -run 'TestPrepareInvocation|TestManifest' -count=1
```

Expected: FAIL because no ABI or invocation allocator exists.

- [ ] **Step 3: Implement roots and manifest records**

Allocate one `0700` attempt root beneath `Executor.RuntimeRoot` with sibling `control`, `inputs`, `outputs`, `tmp`, and `workspace` directories. Define:

```go
type Manifest struct {
	ABI string `json:"abi"`
	Workspace string `json:"workspace"`
	EffectKey string `json:"effect_key"`
	Inputs map[string]ManifestInput `json:"inputs"`
	Outputs map[string]ManifestOutput `json:"outputs"`
}

type ManifestOutput struct {
	ValuePath string `json:"value_path"`
	Kind ArtifactKind `json:"kind"`
	Slot string `json:"slot,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}
```

Use the full SHA-256 of each manifest ID for physical directories; the manifest remains path authority. Materialize the optional base into `workspace`. Materialize a file input as one file retaining its validated basename and a tree input as one directory.

Write canonical input and manifest JSON to temporary files, sync, close, and rename inside `control`. Do not create output. `Invocation.Close` removes only its exact allocated root.

- [ ] **Step 4: Run focused tests for GREEN**

```bash
go test ./script -run 'TestPrepareInvocation|TestManifest' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the ABI slice**

```bash
git add script/abi.go script/abi_test.go script/invocation.go script/invocation_test.go
git commit -m "feat(script): materialize the dawn script ABI"
```

---

### Task 3: Construct the native environment and redact diagnostics

**Files:**
- Create: `script/environment.go`
- Test: `script/environment_test.go`

**Interfaces:**
- Consumes: prepared `Layout`, `Request.Environment`, and the attempt context's effect key.
- Produces: `buildEnvironment(Layout, Request, string) (env []string, sensitive [][]byte, redactor *Redactor, err error)`.
- Produces: `Redactor`, a chunk-safe wrapper around `nativeexec.DiagnosticSink`.

- [ ] **Step 1: Add failing environment tests**

Use injected maps rather than the test process environment. Assert the result contains only the documented baseline, attempt-local `TMPDIR`/`TMP`/`TEMP`, the four fixed Dawn variables, and explicitly requested `API_TOKEN`. Assert missing requested names fail before execution, names are sorted/deduplicated, and values never appear in the manifest.

Add a redactor test that splits `secret-value` across three writes to both streams. After `Close`, neither stream may contain the value and both must contain `[REDACTED]`. With no external values, bytes must pass unchanged.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./script -run 'TestBuildEnvironment|TestRedactor' -count=1
```

Expected: FAIL because environment construction and chunk-safe redaction do not exist.

- [ ] **Step 3: Implement one baseline and one external-value mechanism**

Read the local baseline only through injected `LookupEnv`; production requests `PATH`, `HOME`, `LANG`, `LC_ALL`, and `LC_CTYPE` when present. Replace temp variables with the attempt temp root. Append fixed Dawn variables and requested names. Reject empty names, names containing `=` or NUL, and missing values as malformed process environment.

Return one defensive copy of every non-empty injected value as `sensitive`. Implement streaming redaction by retaining the final `maxSecretBytes-1` bytes between writes, replacing every exact value before forwarding, and flushing the suffix on `Close`. Empty external values are valid but are not redaction patterns. Do not claim to detect encodings, hashes, or deliberate exfiltration.

- [ ] **Step 4: Run focused tests for GREEN**

```bash
go test ./script -run 'TestBuildEnvironment|TestRedactor' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the environment slice**

```bash
git add script/environment.go script/environment_test.go
git commit -m "feat(script): build and redact the native environment"
```

---

### Task 4: Own the process group through a crash-safe supervisor

**Files:**
- Create: `nativeexec/supervisor_protocol.go`
- Create: `nativeexec/supervisor_unix.go`
- Create: `nativeexec/local.go`
- Test: `nativeexec/supervisor_test.go`
- Modify: `cmd/dawn/main.go`
- Test: `cmd/dawn/main_test.go`

**Interfaces:**
- Produces: `nativeexec.Local.Run(context.Context, Request, DiagnosticSink) Outcome`.
- Produces: `nativeexec.RunSupervisorFromEnvironment() (handled bool, exitCode int)`.
- Consumes: runtime-wide `Request.Grace` and optional `Request.Deadline`.

- [ ] **Step 1: Add failing real-process lifecycle tests**

Use the test binary helper-process pattern. Cover exit 0, exit 7, self-`TERM`, context cancellation, a 100 ms deadline, `sleep 30 & exit 0`, immediate stdin read, and concurrent 2 MiB stdout/stderr. Expected states are respectively `Exited(0)`, `Exited(7)`, `Signaled`, `Cancelled`, `TimedOut`, `Leaked`, `Exited(0)` with immediate EOF, and `Exited(0)` with both streams complete.

For cancellation, record the grandchild PID and assert `syscall.Kill(pid, 0)` returns `ESRCH` after `Run` returns.

Assert every outcome records backend `native-local` and policy `workspace-and-process-group-v1`. The policy fixture must explicitly make no filesystem, credential, PID-namespace, network, or hostile-containment claim.

- [ ] **Step 2: Add a failing coordinator-loss test**

Start a coordinator helper that calls `Local.Run` on `sleep 30`, records the workload PID, then blocks. Kill the coordinator with `SIGKILL`. Poll for at most `Grace + 2s` and require the workload to disappear before starting a replacement.

- [ ] **Step 3: Run lifecycle tests and verify RED**

```bash
go test ./nativeexec -run 'TestLocal|TestCoordinatorLoss' -count=1
```

Expected: FAIL because no supervisor protocol or owned runner exists.

- [ ] **Step 4: Implement the private re-exec protocol**

Use the Dawn executable as a supervisor. Parent and supervisor exchange exact-version JSON:

```go
const protocol = "dawn.nativeexec/1"
type processSpec struct {
	Protocol string `json:"protocol"`
	Argv []string `json:"argv"`
	Dir string `json:"dir"`
	Env []string `json:"env"`
	DeadlineNanos int64 `json:"deadline_nanos,omitempty"`
	GraceNanos int64 `json:"grace_nanos"`
}
type control struct { Cause string `json:"cause"` }
type processResult struct {
	State State `json:"state"`
	ExitCode int `json:"exit_code,omitempty"`
	Signal string `json:"signal,omitempty"`
	Detail string `json:"detail,omitempty"`
}
```

Pass spec, control, result, stdout, and stderr over inherited Unix file descriptors. The supervisor starts the workload with `Setpgid`, `/dev/null` stdin, and only the supplied directory/environment.

The supervisor sends `SIGTERM` to `-pgid`, waits `Grace`, sends `SIGKILL`, reaps the direct child, then waits for group absence and pipe EOF. After normal parent exit, `kill(-pgid, 0)` detects descendants; terminate them and return `Leaked`. If the group is gone but inherited pipes do not reach EOF within the same grace, close the parent read descriptors and return `Leaked`; an intentionally detached service must close them. Control EOF means coordinator loss and triggers cleanup. Deadline is measured in the supervisor. A control message identifies external cancellation.

Call `RunSupervisorFromEnvironment` before normal CLI parsing. Protect the private selector with a per-launch random token inherited through a dedicated descriptor; it is not a user CLI or workflow surface.

- [ ] **Step 5: Run focused and CLI tests for GREEN**

```bash
go test ./nativeexec -run 'TestLocal|TestCoordinatorLoss' -count=1
go test ./cmd/dawn -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the supervisor slice**

```bash
git add nativeexec/local.go nativeexec/supervisor_protocol.go nativeexec/supervisor_unix.go nativeexec/supervisor_test.go cmd/dawn/main.go cmd/dawn/main_test.go
git commit -m "feat(nativeexec): own process groups across cancellation and crash"
```

---

### Task 5: Execute a prepared script and classify every terminal outcome

**Files:**
- Create: `script/executor.go`
- Test: `script/executor_test.go`

**Interfaces:**
- Consumes: `prepareInvocation`, `buildEnvironment`, `nativeexec.Runner`.
- Produces: `Executor.Run(context.Context, AttemptContext, Request, DiagnosticSink) Outcome`.
- Calls candidate construction from Task 6 only after native success.

- [ ] **Step 1: Add failing outcome tests**

Use a fake runner returning every `nativeexec.State`. Assert exact mappings: nonzero `Exited` to `FailureExit`, `Signaled` to `FailureSignal`, `TimedOut` to `FailureTimeout`, `Leaked` to `FailureLeakedProcesses`, `LaunchFailed` to `FailureLaunch`, and `Cancelled` to script `Cancelled` with no failure.

Assert exit zero advances to candidate building, but leak never does. Output bytes written before every non-success must be ignored. `Deadline == 0` must reach `nativeexec` unchanged.

- [ ] **Step 2: Add failing ordering tests**

Record events from values, runner, diagnostics, candidate builder, and cleanup. Assert:

```text
prepare → run → diagnostics-close → process-quiescent → candidate → cleanup
```

For launch failure, cancellation, timeout, and nonzero exit, `candidate` must be absent and cleanup present.

- [ ] **Step 3: Run tests and verify RED**

```bash
go test ./script -run 'TestExecutor' -count=1
```

Expected: FAIL because executor orchestration does not exist.

- [ ] **Step 4: Implement orchestration with one native call**

Validate the resolved request and attempt context, prepare roots with `attempt.EffectKey`, defer exact-root cleanup, build environment/redactor/sensitive values from that same key, and call `Runner.Run` exactly once. Map the native outcome through one total switch. Only `Exited` with code 0 calls `buildCandidate` with the sensitive values; every other outcome discards output and slots. Never inspect diagnostics, loop, or parse provider errors.

Pass attempt grace, optional deadline, workspace directory, literal argv, and constructed environment. Reject an empty effect key before preparation because the ledger must durably assign it first.

- [ ] **Step 5: Run focused tests for GREEN**

```bash
go test ./script -run 'TestExecutor' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the executor slice**

```bash
git add script/executor.go script/executor_test.go
git commit -m "feat(script): execute and classify native scripts"
```

---

### Task 6: Build one complete candidate or publish nothing

**Files:**
- Create: `script/candidate.go`
- Test: `script/candidate_test.go`

**Interfaces:**
- Consumes: `DAWN_OUTPUT`, `Manifest`, `ImmutableValues.CaptureFile`, `ImmutableValues.CaptureTree`, `CandidateBuilder.BuildScriptCandidate`.
- Produces: `Executor.buildCandidate(context.Context, *Invocation, Request) (Value, error)`.

- [ ] **Step 1: Add failing JSON and static-slot tests**

Cover absent output, empty output, malformed JSON, two concatenated values, a valid top-level string, valid `null`, and a valid object. Dawn must parse one value and canonicalize through the candidate builder.

For static file output, create one slot containing `report.pdf`; assert `CaptureFile` receives the slot and the returned `Value` is supplied under `output:/report`. Zero files and two files must fail `FailureCapture`. Add an empty tree and a tree containing an executable file and safe internal symlink. Put an exact injected value into JSON, a file, and a tree in three neighboring cases; each must fail before candidate publication under #6's secret boundary.

- [ ] **Step 2: Add failing dynamic and atomicity tests**

Write:

```json
{"pages":[
  {"$dawn":"output:/pages/*","member":"0"},
  {"$dawn":"output:/pages/*","member":"1"}
]}
```

Populate both member slots and assert both values reach the builder at their exact result paths. Add traversal, absolute member, wrong ID, file/tree mismatch, and overlapping incompatible roots; each must fail before publication.

Inject capture failure on the second of three outputs. Assert the builder is never called and prior captured content remains uncommitted/unreachable according to the value store.

- [ ] **Step 3: Run candidate tests and verify RED**

```bash
go test ./script -run 'TestCandidate|TestDynamicOutput|TestCaptureAtomicity' -count=1
```

Expected: FAIL because decoding and capture do not exist.

- [ ] **Step 4: Implement exact decoding and capture**

Use `json.Decoder` with `UseNumber`, decode once, then require EOF. Reserved objects are recognized only at contracted file/tree positions through `CandidateBuilder`; an ordinary object field named `$dawn` stays ordinary.

Resolve static references to `ManifestOutput.Slot`. Resolve dynamic members by joining `Namespace` with a normalized relative member and verifying confinement. Pass the exact non-empty injected values from Task 3 into ordinary-output validation and every file/tree capture. Capture through `ImmutableValues`, collecting invocation-local values by resolved result path.

When workspace publication is declared, capture workspace as a tree after named captures succeed. Call `BuildScriptCandidate` only after all capture succeeds. Return its one `Value`; never expose the intermediate map to the scheduler. Map malformed/contract-invalid output to `FailureOutput` and slot/media/tree/store failures to `FailureCapture`.

- [ ] **Step 5: Run focused tests for GREEN**

```bash
go test ./script -run 'TestCandidate|TestDynamicOutput|TestCaptureAtomicity' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the candidate slice**

```bash
git add script/candidate.go script/candidate_test.go
git commit -m "feat(script): capture one atomic candidate value"
```

---

### Task 7: Prove the contract with a Prestige-shaped native chain

**Files:**
- Create: `script/integration_test.go`
- Create: `script/testdata/validator/main.go`
- Create: `script/testdata/report/main.go`
- Modify: `docs/superpowers/specs/2026-08-09-native-script-execution-contract-design.md`

**Interfaces:**
- Consumes: completed `script.Executor`, real `nativeexec.Local`, and in-memory vNext value ports.
- Produces: one executable Prestige-shaped tracer bullet and recorded conformance evidence.

- [ ] **Step 1: Add a failing typed-file chain**

Build test helpers with `go build`. Validator reads `DAWN_INPUT`, resolves a named PDF/text fixture through `DAWN_MANIFEST`, and writes a structured score. Report consumes that score and named files, writes `report.md` into its slot, and returns:

```json
{"summary":"2 findings validated","report":{"$dawn":"output:/report"}}
```

Run both through real subprocesses. Assert each helper's `os.Getwd()` equals the manifest workspace, the second receives only committed immutable values, report content round-trips, effect key reaches each child, and neither stdout nor stderr becomes output. Interrupt a first attempt, wait for its group to die, then run a second attempt with the same `AttemptContext.EffectKey`; assert a new workspace path with byte-identical committed inputs and the same child-visible key.

- [ ] **Step 2: Add shell, cancellation, and external-tool cases**

Run literal shell argv containing pipes/redirection. Put shell metacharacters in input and prove they remain data.

Run a fake executable named `docker` through `PATH`; assert ordinary argv/environment and no Docker callback. Add an `integration` build-tag test that, when Docker is available, starts a labeled disposable container with the effect key, returns its ID, and removes it in test cleanup.

Cancel a helper with grandchildren. Require `Cancelled`, no candidate, and no surviving PID.

- [ ] **Step 3: Run tracer tests and verify RED**

```bash
go test ./script -run 'TestPrestigeTracer|TestShellDataIsNotCode|TestExternalDockerCLI|TestCancellationPublishesNothing' -count=1
```

Expected: at least one cross-component contract remains incomplete until the executor is fully wired.

- [ ] **Step 4: Make only integration fixes required by tests**

Connect completed components through `Executor.Run`. Add no source syntax, alternate result channel, retry, cwd override, environment mapping, Docker branch, or test-only production switch. Fix mismatches at the owning module: ABI in `script`, process lifecycle in `nativeexec`, immutable content in the vNext value boundary.

- [ ] **Step 5: Run the verification matrix**

```bash
go test ./nativeexec ./script -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS. Where Docker is available:

```bash
go test -tags=integration ./script -run TestRealDockerExternalEffect -count=1
```

Expected: PASS, or SKIP with `docker CLI or daemon unavailable`.

- [ ] **Step 6: Record implementation evidence**

Add `## Implementation Evidence` to the design spec with commit IDs and exact verification commands for ABI, files, environment, lifecycle, crash recovery, isolation honesty, and the Prestige tracer. Do not weaken the spec to match a shortcut.

- [ ] **Step 7: Commit the tracer**

```bash
git add script/integration_test.go script/testdata/validator/main.go script/testdata/report/main.go docs/superpowers/specs/2026-08-09-native-script-execution-contract-design.md
git commit -m "test(script): prove the native workflow contract end to end"
```

---

### Task 8: Cut over with no legacy process API

**Files:**
- Delete: `proc/proc.go`
- Delete: `proc/proc_test.go`
- Modify: `backend/claude/claude.go`
- Modify: `backend/claude/workspace.go`
- Test: `backend/claude/claude_test.go`
- Test: `backend/claude/workspace_test.go`

**Interfaces:**
- Consumes: `nativeexec.Runner` and `nativeexec.Local`.
- Removes: `proc.Command`, `proc.WaitDelay`, direct adapter-owned `exec.CommandContext`, and all compatibility callers.
- Produces: one native lifecycle shared by scripts and CLI-backed vNext adapters.

- [ ] **Step 1: Enumerate every legacy caller**

```bash
rg -n 'proc\.Command|proc\.WaitDelay|github\.com/valbaudo/dawn/proc' --glob '*.go'
```

Expected before cutover: only pre-vNext Claude files and `proc` tests. Any additional caller must move to its proper vNext boundary; do not preserve `proc.Command` as a wrapper.

- [ ] **Step 2: Add failing adapter lifecycle tests**

For every CLI-backed vNext adapter, inject `nativeexec.Runner` and assert cancellation kills a grandchild, stdin is closed, dual streams drain, and recognized transient continuation stays adapter-owned. Assert `nativeexec` returns only process outcomes and has no provider classification.

- [ ] **Step 3: Run adapter tests and verify RED**

```bash
go test ./backend/... -run 'Test.*(Cancellation|Grandchild|TransientContinuation)' -count=1
```

Expected: FAIL while adapters still call legacy `proc.Command`.

- [ ] **Step 4: Move adapters and delete `proc`**

Pass literal adapter argv, adapter environment, context, runtime grace, and diagnostics into `nativeexec.Runner`. Keep provider session recovery and transient classification inside the #9 adapter. Delete `proc` after `rg` reports no caller. Leave no alias, forwarding function, deprecated shim, old test, or compatibility documentation.

- [ ] **Step 5: Run clean-cutover verification**

```bash
test ! -d proc
if rg -n 'proc\.Command|proc\.WaitDelay|github\.com/valbaudo/dawn/proc' --glob '*.go'; then exit 1; fi
go test ./... -count=1
go vet ./...
```

Expected: no legacy matches; tests and vet pass.

- [ ] **Step 6: Commit the cutover**

```bash
git add -A proc backend nativeexec script
git commit -m "refactor: remove the legacy process runner"
```

## Final Verification

```bash
go test ./... -count=1
go test -race ./nativeexec ./script -count=1
go vet ./...
git diff --check
```

Then verify product constraints mechanically:

```bash
if rg -n 'retry_count|retryable_exit|backoff|stdout_as|capture_path|working_directory|sandbox:|docker:' script nativeexec; then exit 1; fi
if rg -n 'proc\.Command|proc\.WaitDelay|github\.com/valbaudo/dawn/proc' --glob '*.go'; then exit 1; fi
```

Expected: every command exits zero and both searches produce no matches.

## Plan Self-Review

- **Spec coverage:** Tasks 1–2 cover command, cwd, ABI, named files, and workspace roots. Task 3 covers baseline environment, named external values, effect key, and redaction. Tasks 4–5 cover stdin, streams, deadline, cancellation, process groups, leaked descendants, coordinator loss, classification, and no retry. Task 6 covers JSON, captures, workspace publication, and one atomic candidate. Task 7 proves Prestige-shaped and arbitrary native execution. Task 8 removes the legacy API without compatibility.
- **Placeholder scan:** Every task names concrete files, interfaces, test assertions, commands, expected failures, implementation behavior, and a commit boundary; no work is deferred through an unnamed marker or generic error-handling instruction.
- **Type consistency:** `nativeexec.Request`/`Outcome` flow from Task 1 through Tasks 4–5. `script.Request`, `Manifest`, `Invocation`, and `Outcome` retain their names through Tasks 2–7. Only prerequisite vNext `Value`, `Contract`, and attempt context cross the package boundary.
- **Complexity audit:** Two modules earn boundaries: `nativeexec` hides OS ownership shared by scripts and CLI adapters; `script` hides the entire typed process ABI. No registry, policy object, mode, retry abstraction, or Docker layer is introduced.
