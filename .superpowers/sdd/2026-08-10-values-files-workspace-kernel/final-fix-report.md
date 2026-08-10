# Final review fix report — values/files/workspace kernel

Date: 2026-08-10
Branch: `codex/values-files-workspace-kernel`
Reviewed starting point: `fc52d02e48b12c7cb9b5bc4cffcb2da41f9186dd`

## Outcome

All seven accepted final-review findings are corrected without a compatibility layer, configured depth/resource limit, second artifact channel, workspace value kind, merge behavior, scheduler/runtime work, or durable-commit work. The output capability now has only the kind-specific `File`, `Tree`, and declared `Workspace` operations; `Outputs.Value` is removed.

## Commits

- `475e3c47d22cddea35d9b7f2d9cb12e1d6f23213` — `Make public finite traversals stack safe`
- `a1e53243bc9da1586df2bd4e1b404e6cd93ea327` — `Fix value and capture boundary semantics`
- `64152784c574e02f590bd0ab907821f8f6d388f0` — `Document final capture semantics`

## RED/GREEN evidence

### 1. Finite public traversal and filesystem resource safety

Observed REDs before production changes:

- The 50,000-level exported `Type`/`Contract` construction and `Valid`/`Equal` subprocess overflowed in recursive type validation.
- The 50,000-level literal-validation subprocess overflowed in recursive canonical JSON traversal.
- Public `workflow.Compile` with a 50,000-level authored type overflowed while emitting its canonical type representation.
- Preparing a 50,000-level schema did not finish within the 20-second subprocess deadline because persistent paths were repeatedly copied.
- Deep finite workspace preparation and `Environment.Close` overflowed in `filepath.WalkDir`/`fs.WalkDir` cleanup paths.
- A long descending/ascending/oscillating symlink target required 162,565 allocations in the focused regression.
- The final rollback-focused low-stack subprocess exposed one remaining `os.Root.RemoveAll` stack overflow.

GREEN corrections and regressions:

- `Type.Valid`, `Type.Equal`, `Contract.Valid`, `Contract.Equal`, assignability, ordinary-type checks, and literal validation use explicit work stacks. Public constructors use a private immutable construction invariant, so building 50,000 levels is linear rather than repeatedly revalidating the constructed subtree.
- Workflow contract cloning and canonical type emission are iterative; public `workflow.Compile` returns ordinarily under the low-stack regression.
- Value and workspace paths use persistent linked nodes where extension is frequent. Type canonical wire emission is a flat prefix traversal.
- Workspace staging/read-only traversal and cleanup are iterative handle-relative walks. Tree rollback cleanup is also iterative and handle-relative; no production `filepath.WalkDir`, `fs.WalkDir`, or `RemoveAll` remains in `content`/`workspace`.
- Tree symlink resolution uses interned linked path-node identities and is near-linear without a depth/component limit.
- Focused low-stack/complexity regressions all pass in `content/tree_test.go`, `content/tree_materialize_test.go`, `value/type_test.go`, `value/literal_test.go`, `value/value_validate_test.go`, `workflow/compile_test.go`, and `workspace/prepare_test.go`.

The final forged scalar-enum/deep-literal probe produced an additional RED stack overflow in `appendCanonicalJSON`. It is GREEN because enum validation now rejects a non-scalar decoded shape before canonicalization.

### 2. Concrete file media and output metadata

Observed REDs:

- `NewFile` retained equivalent noncanonical media spelling, and malformed media records could reach a panicking value-side split.
- The required `Outputs.File`/`Outputs.Tree` API regression initially failed to compile because only generic `Outputs.Value` existed; the physical child `value` was being used as a file's semantic name and output media came only from sniffing.

GREEN corrections:

- `canonicalConcreteMedia` is the single concrete MIME canonicalizer used by `IngestFile`, `NewFile`, and `File.Valid`. It requires a nonempty type/subtype, rejects wildcards and bare tokens, and stores `mime.FormatMediaType` canonical spelling.
- Value media relations return false, rather than panic, for malformed content records.
- `Outputs.Value` is deleted. `Outputs.File(ctx, path, logicalName, concreteMedia)` captures the fixed slot with required semantic metadata; `Outputs.Tree(ctx, path)` captures tree output; `Outputs.Workspace` remains declaration-gated.
- Tests cover exact `application/json`, custom parameterized media canonicalization, logical names different from physical `value`, equivalent repeated metadata, invalid/bare/wildcard metadata, and wrong-method errors.
- All workspace tests and the Prestige-shaped tracer use the kind-specific API.

### 3. Numeric enum kind correctness

Observed RED: an enum containing integer literal `1` accepted a runtime `NumberKind` value whose canonical JSON bytes were also `1`.

GREEN correction: enum membership now requires both scalar kind and canonical ordinary bytes in runtime and literal validation. The regression proves static integer-enum-to-number assignment, producer validation of the integer member, consumer validation after integer-to-number widening, and rejection of a `NumberKind` impostor.

### 4. Stable tree/workspace capture

Observed REDs: deterministic same-inode/same-size mutations during the first named-tree read and first workspace read both allowed `Capture` to succeed.

GREEN correction: named trees and published workspaces are captured twice through the same already-pinned root, and their semantic `Tree` identities must match before candidate publication. The mutation regressions now return only the zero candidate and an error. This is a stable-snapshot check, not isolation from continuing external mutation.

### 5. Tree manifest segment validity

Observed RED: both manifest encoding and `Repository.Entries` accepted a single encoded segment containing `/` or NUL.

GREEN correction: manifest validation rejects `/` and NUL within each segment. A POSIX regression confirms backslash remains an opaque component byte.

### 6. Materialization rollback

Observed REDs:

- File publication followed by a temporary-cleanup failure returned an error but left the newly published target, preventing retry.
- Tree destination creation followed by the first fallible post-create inspection returned an error but left the new destination.

GREEN corrections:

- File materialization tracks successful publication and removes that new target whenever a later deferred cleanup makes the operation fail; retry succeeds.
- Tree materialization arms rollback immediately after destination creation, before inspection, and removes every newly created destination on later error.
- Existing exact-name/transformed-name/final-close rollback regressions remain green, along with the new low-stack iterative rollback removal test.

### 7. Typed-nil built-in stores

Observed RED: typed-nil `*content.Memory` and `*content.FS` receivers panicked directly and through `content.NewRepository`/`value.NewStore`.

GREEN correction: exported built-in `Put`/`Copy` receiver methods return ordinary errors for nil receivers, nil contexts, and nil reader/writer interfaces. Focused tests cover direct calls, repository paths, and value-store paths. No reflection, registry, or behavior imposed on custom store implementations was added.

## Files changed

- Content: `content/file.go`, `content/fs.go`, `content/memory.go`, `content/tree.go`, `content/tree_wire.go`, `content/value.go`, and focused tests.
- Value: `value/type.go`, `value/contract.go`, `value/literal.go`, `value/path.go`, `value/value_validate.go`, and focused tests.
- Workflow: `workflow/canonical.go` and `workflow/compile_test.go`.
- Workspace: `workspace/manifest.go`, `workspace/prepare.go`, `workspace/capture.go`, and focused/conformance tests.
- Design evidence: the checked-in implementation plan and design spec now describe the kind-specific output API, concrete output metadata, kind-sensitive enums, and stable-snapshot boundary truthfully.

## Fresh verification

Focused accepted-finding regressions:

```text
go test ./content ./value ./workflow ./workspace -run '^(TestPublicTypeTraversalDeepFiniteSubprocess|TestTypeAndContractValidateDeepFiniteSubprocess|TestValidateLiteralDeepFiniteSubprocess|TestCompileDeepFinitePublicTypeSubprocess|TestPrepareDeepFiniteInputAndOutputSchemasDoNotOverflowStack|TestPrepareDeepFiniteTreeUnderLowStackSubprocess|TestEnvironmentCloseDeepFiniteTreeUnderLowStackSubprocess|TestTreeSymlinkResolutionLongOscillatingTargetIsNearLinear|TestIngestFileCanonicalizesExplicitMedia|TestNewFileCanonicalizesAndRequiresConcreteTypeSubtypeMedia|TestMediaRelationsRejectMalformedRecordsWithoutPanicking|TestCaptureKindSpecificFileMetadataAndWrongMethods|TestTypeValidateValueMatchesParsedMediaAndEnums|TestAtomicCaptureRejectsSameSizeTreeMutationDuringFirstRead|TestAtomicCaptureRejectsSameSizeWorkspaceMutationDuringFirstRead|TestTreeManifestSegmentsAreSinglePOSIXComponents|TestMaterializeFileRollsBackPublishedTargetWhenTemporaryCleanupFails|TestMaterializeTreeRollsBackWhenFirstPostCreateInspectionFails|TestTreeRollbackRemovalHandlesDeepFiniteTreeUnderLowStack|TestBuiltInStoresRejectTypedNilReceiversAndNilStreams|TestValueStoreReturnsErrorsForTypedNilBuiltInContentStores)$' -count=1
ok github.com/valbaudo/dawn/content
ok github.com/valbaudo/dawn/value
ok github.com/valbaudo/dawn/workflow
ok github.com/valbaudo/dawn/workspace
```

Required matrix:

```text
go test ./content ./value ./workflow ./workspace -count=1
ok (all four packages)

go test -race ./content ./value ./workflow ./workspace -count=1
ok (all four packages)

go test ./... -count=1
ok (all repository packages)

go vet ./...
exit 0

git diff --check
exit 0
```

Both exact plan invariant scans exited zero with no matches:

```bash
if rg -n 'github\.com/valbaudo/dawn/(store|plan|gate|backend|proc)|dawn\.(Ref|Backend|Invocation|Result)' content value workflow workspace; then exit 1; fi
if rg -n 'KindWorkspace|artifact(s)?\s+map|input_files|output_files|capture_path|destination_path|merge_policy|provider_file_id|workspace_dir|mounts:' content value workflow workspace; then exit 1; fi
```

Additional surface scans found no `Outputs.Value` reference and no production `filepath.WalkDir`, `fs.WalkDir`, or `RemoveAll` use in `content`/`workspace`.

## Concerns and boundaries

No accepted finding remains unresolved. Stable double capture is an ordinary consistency check over a pinned root; it intentionally does not promise isolation against a process that continues mutating files. Durable candidate commit/replay, scheduling, adapter execution, native process lifecycle, and later-ticket behavior remain outside this kernel.
