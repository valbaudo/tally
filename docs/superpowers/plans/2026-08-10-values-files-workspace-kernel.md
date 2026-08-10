# Runtime Values, Files, and Workspace Kernel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Dawn vNext's immutable runtime value graph, content-addressed files and portable trees, canonical leaf delivery intent, and one private local workspace preparation/capture kernel.

**Architecture:** Add one dependency-free `content` module for immutable bytes, file metadata, and portable tree snapshots; extend `value` with immutable runtime values that contain `content.File` and `content.Tree` recursively; extend `workflow.Leaf` with only workspace-base, workspace-publication, and raw-attachment fidelity semantics; add a `workspace` module that materializes concrete values into fresh runtime-owned roots and returns one validated candidate value after all captures succeed. The new modules never import or adapt the legacy `dawn`, `store`, `plan`, `backend`, `gate`, or `proc` APIs.

**Tech Stack:** Go 1.26 standard library only; SHA-256 content identity; deterministic private binary encodings; native macOS/Linux filesystem behavior.

## Global Constraints

- Implement `docs/superpowers/specs/2026-08-09-values-contracts-workspaces-files-design.md` on top of the canonical kernel from GitHub #5.
- The clean redesign has no compatibility readers, wrappers, aliases, converters, migrations, or dual writes for the current `dawn.Ref`, `store.Trees`, workspace field, or plan runtime.
- `workspace` remains an execution role. Never add a workspace value kind.
- Files and trees remain recursive members of the one `value.Value` graph. Never add an artifact map or second result channel.
- Secrets are not `value.Value` members, file metadata, manifests, candidate outputs, or content-store records. This plan adds no secret reference or secret-producing leaf surface.
- Semantic strings are opaque data and retain exact bytes. Runtime paths use structured identities and runtime-owned physical locations; do not ban characters merely to simplify path construction.
- Finite public inputs must terminate without fatal recursion. Use explicit traversal stacks where decoded value or tree depth is user-controlled; do not replace termination with an arbitrary author-visible depth limit.
- A filesystem leaf receives one fresh private writable workspace, empty or materialized from exactly one required tree input selected in the canonical leaf.
- Every other file/tree input is staged outside the workspace under deterministic Dawn-owned slots. Every declared file/tree output uses deterministic Dawn-owned slots.
- Authors cannot configure staging paths, capture paths, workspace directories, mount layouts, copy strategies, store URIs, provider IDs, merge policies, or archive behavior.
- Captured trees preserve empty directories, regular bytes, the executable bit, and confined relative symlinks; unsupported filesystem entries fail the capture and are never skipped.
- Parallel invocations never share a live directory and trees are never implicitly overlaid or merged.
- A failed preparation or capture returns no environment or candidate. Content written before a later failure is unreachable candidate content, not a partial result.
- This plan does not implement YAML, scheduling, propagation, commits/journals/replay, adapter lifecycle, native script ABI/processes, secrets delivery, Docker orchestration, or host-output CLI syntax. GitHub #7 through #12 own those boundaries.
- Add no production dependency and no configurable resource, retry, isolation, or policy surface.

## File and Module Map

| Module | Files | Hidden knowledge |
| --- | --- | --- |
| `content` | `digest.go`, `store.go`, `memory.go`, `fs.go`, `value.go`, `file.go`, `tree.go`, `tree_wire.go` | Streaming content identity, durable byte storage, media selection, portable tree capture/materialization, safe symlinks |
| `value` | existing contract files plus `path.go`, `value.go`, `value_wire.go`, `value_validate.go`, `store.go` | Recursive immutable runtime values, structured paths, canonical value encoding/storage, contract validation |
| `workflow` | existing draft/model/compiler/canonical files | Canonical workspace base/publication and attachment-fidelity declarations only |
| `workspace` | `manifest.go`, `prepare.go`, `capture.go` | Fresh invocation roots, deterministic slots, staging, read-only input presentation, all-or-nothing candidate capture |

The dependency direction is:

```text
content <- value <- workflow
   ^         ^         ^
   +---------+--------- workspace
```

`content` knows nothing about contracts or workflows. `value` may contain immutable `content.File` and `content.Tree` facts. `workflow` declares meaning but never materializes bytes. `workspace` is the only module that joins compiled leaf meaning, concrete values, content storage, and native paths.

---

### Task 1: Streaming content identity and durable byte storage

**Files:**
- Create: `content/digest.go`
- Create: `content/store.go`
- Create: `content/memory.go`
- Create: `content/fs.go`
- Test: `content/store_test.go`
- Test: `content/fs_test.go`

**Interfaces:**
- Produces: `type Digest [sha256.Size]byte`
- Produces: `type Object struct` with `Digest() Digest`, `Size() int64`, `Valid() bool`, and `Equal(Object) bool`
- Produces: `type Store interface { Put(context.Context, io.Reader) (Object, error); Copy(context.Context, Digest, io.Writer) (int64, error) }`
- Produces: `func NewMemory() *Memory`
- Produces: `func OpenFS(root string) (*FS, error)`
- Consumes: Go standard library only.

- [ ] **Step 1: Write failing streaming-store contract tests**

Create one shared contract test in `content/store_test.go` and run it against memory and filesystem implementations:

```go
func storeContract(t *testing.T, open func(*testing.T) Store) {
	t.Helper()
	t.Run("identity and defensive reads", func(t *testing.T) {
		store := open(t)
		want := bytes.Repeat([]byte("dawn\x00"), 64*1024)
		first, err := store.Put(context.Background(), bytes.NewReader(want))
		if err != nil { t.Fatal(err) }
		second, err := store.Put(context.Background(), bytes.NewReader(want))
		if err != nil { t.Fatal(err) }
		if !first.Equal(second) || first.Size() != int64(len(want)) { t.Fatal("identity changed") }
		var got bytes.Buffer
		if _, err := store.Copy(context.Background(), first.Digest(), &got); err != nil { t.Fatal(err) }
		if !bytes.Equal(got.Bytes(), want) { t.Fatal("content changed") }
	})
}
```

Add cases for empty content, cancellation during `Put` and `Copy`, concurrent identical puts, missing content, and a writer that fails after receiving bytes. Assert a failed put never makes the final digest readable.

- [ ] **Step 2: Run the store contract and verify RED**

```bash
go test ./content -run 'Test(Memory|FS|Store)' -count=1
```

Expected: FAIL because `content` and its store contract do not exist.

- [ ] **Step 3: Implement typed content identity and the memory store**

Use a fixed digest, never a workflow string:

```go
type Digest [sha256.Size]byte

type Object struct {
	digest Digest
	size   int64
}

type Store interface {
	Put(context.Context, io.Reader) (Object, error)
	Copy(context.Context, Digest, io.Writer) (int64, error)
}
```

`Memory.Put` streams through `io.Copy` into a buffer and SHA-256 hasher, checks the context before publication, and stores a defensive copy under the typed digest. `Memory.Copy` copies from a defensive snapshot while hashing it again; missing or corrupt content returns an integrity error.

- [ ] **Step 4: Implement the filesystem store with atomic streaming writes**

`FS.Put` writes to a same-directory temporary file while hashing, calls `Sync`, closes, and atomically renames it to the private lowercase-hex digest filename. If the final file already exists, verify it and discard the temporary file. `FS.Copy` hashes while copying and rejects a file whose bytes no longer match the requested digest.

Keep the hex spelling private to `content.FS`; `Digest` exposes `Bytes()` as a defensive copy and `Equal`, but no parseable workflow URI.

- [ ] **Step 5: Run focused, race, and package tests for GREEN**

```bash
go test ./content -count=1
go test -race ./content -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the content-store foundation**

```bash
git add content/digest.go content/store.go content/memory.go content/fs.go content/store_test.go content/fs_test.go
git commit -m "feat(content): store immutable bytes by typed digest"
```

---

### Task 2: Immutable recursive runtime values and canonical storage

**Files:**
- Create: `content/value.go`
- Create: `value/path.go`
- Create: `value/value.go`
- Create: `value/value_wire.go`
- Create: `value/value_validate.go`
- Create: `value/store.go`
- Test: `value/value_test.go`
- Test: `value/value_wire_test.go`
- Test: `value/value_validate_test.go`
- Test: `value/store_test.go`

**Interfaces:**
- Consumes: `content.Object`, `content.Digest`, existing `value.Kind`, `value.Type`, `value.Contract`, and `value.Literal`.
- Produces: immutable `content.File` and `content.Tree` semantic records in `content/value.go`; constructing a record validates facts but never claims the referenced bytes are present.
- Produces: `type Value`, `type Entry`, and structured `type Path` with field, map-key, and list-index segments.
- Produces: scalar constructors, `NewObject`, `NewMap`, `NewList`, `NewFileValue`, `NewTreeValue`, and read-only accessors.
- Produces: `func ParseCanonical([]byte) (Value, error)` and `func (Value) Canonical() []byte`.
- Produces: `func (Type) Validate(Value) error` and `func (Contract) Validate(Value) error`.
- Produces: `type Store struct` with `NewStore(content.Store)`, `Put(context.Context, Value) (content.Object, error)`, and `Get(context.Context, content.Digest) (Value, error)`.

- [ ] **Step 1: Add failing recursive-value and ownership tests**

Build one value containing files and trees at every recursive position:

```go
func TestValueOwnsNestedData(t *testing.T) {
	file := testFile(t, "report.pdf", "application/pdf", []byte("%PDF"))
	tree := testTree(t)
	value, err := NewObject(
		mustEntry(t, "document", NewFileValue(file)),
		mustEntry(t, "pages", NewList(NewFileValue(file))),
		mustEntry(t, "evidence", mustMap(t, mustEntry(t, "primary", NewFileValue(file)))),
		mustEntry(t, "bundle", mustObject(t,
			mustEntry(t, "report", NewFileValue(file)),
			mustEntry(t, "sources", NewTreeValue(tree)),
		)),
	)
	if err != nil { t.Fatal(err) }
	before := value.Canonical()
	mutateEveryReturnedSliceAndPath(value)
	if !bytes.Equal(before, value.Canonical()) { t.Fatal("value mutated through accessor") }
}
```

Cover absent optional fields versus explicit null, object versus map identity, ordered lists, integer versus number, arbitrary byte strings in names/keys, duplicate fields, invalid zero values, and file/tree facts.

- [ ] **Step 2: Add failing canonical round-trip and validation tests**

Assert:

```go
encoded := original.Canonical()
decoded, err := ParseCanonical(encoded)
if err != nil { t.Fatal(err) }
if !original.Equal(decoded) { t.Fatal("round trip changed value") }
```

Reverse every non-semantic map/object input order and assert byte equality. Change one scalar, list position, file digest, file length, logical filename, media type, tree digest, map key, or nesting position and assert byte inequality.

Validate nested file/tree values against matching contracts. Reject missing required fields, undeclared object fields, map/list element mismatches, wrong media, file/tree under `any`, number-to-integer narrowing, and implicit projection/coercion.

Store the canonical value through `value.Store`, load it by typed digest, and assert equality. Corrupt or remove the underlying bytes and assert `Get` returns an integrity error rather than a partial value.

- [ ] **Step 3: Run runtime-value tests and verify RED**

```bash
go test ./value -run 'Test(Value|CanonicalValue|ContractValidateValue|TypeValidateValue)' -count=1
```

Expected: FAIL because runtime values do not exist.

- [ ] **Step 4: Implement immutable values and structured paths**

Use private state and copied canonical collections:

```go
type Value struct {
	kind    Kind
	text    string
	boolean bool
	number  string
	entries []Entry
	items   []Value
	file    *content.File
	tree    *content.Tree
}

type Entry struct {
	name  string
	value Value
}
```

`Path` stores tagged segments, not a slash-delimited string:

```go
type SegmentKind uint8
const (
	FieldSegment SegmentKind = iota + 1
	MapKeySegment
	ListIndexSegment
)
```

Every `Path` extension returns a copy. Names and keys remain opaque bytes. Diagnostic rendering may escape bytes but never becomes identity or filesystem interpretation.

- [ ] **Step 5: Implement the private canonical value wire format**

Use a domain header (`dawn.value/1`), one byte per variant, unsigned length prefixes, raw string bytes, raw 32-byte digests, and sorted object/map entries. Preserve list order. Do not use JSON for the private wire because Go strings may contain arbitrary bytes and file/tree handles are not ordinary JSON.

The decoder rejects unknown tags, duplicate or unsorted entries, invalid lengths, invalid media, zero content records, trailing bytes, and nesting that does not terminate. It never normalizes a malformed encoding into a valid value.

Encode, decode, validate, and handle-detection walks use explicit stacks for user-controlled nesting. Add a subprocess regression that parses and validates a deeply nested but finite value and returns an ordinary result rather than overflowing the Go stack; do not add a maximum-depth setting.

`value.Store` is a thin ownership boundary over `content.Store`: it stores only `Value.Canonical()` and accepts a loaded value only after `ParseCanonical` succeeds. It does not introduce a URI, cache key, journal record, or second encoding.

- [ ] **Step 6: Implement recursive contract validation**

`Type.Validate` uses one closed switch. Enum values compare against the scalar's canonical ordinary representation. Integer may validate as number, required data may satisfy optional fields through the containing contract, and `any` recursively rejects every file/tree value. Parse a concrete file media value with `mime.ParseMediaType`; an exact `text/plain` constraint matches `text/plain; charset=utf-8`, while `type/*` matches the parsed base type. No conversion occurs and concrete parameters remain part of the file's semantic metadata.

`Contract.Validate` requires an object value and distinguishes an absent optional port from a present null port.

- [ ] **Step 7: Run focused, race, and complete tests for GREEN**

```bash
go test ./value ./content -count=1
go test -race ./value ./content -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit the runtime value graph**

```bash
git add content/value.go value/path.go value/value.go value/value_wire.go value/value_validate.go value/store.go value/value_test.go value/value_wire_test.go value/value_validate_test.go value/store_test.go
git commit -m "feat(value): model immutable runtime values"
```

---

### Task 3: File ingestion, media semantics, and byte materialization

**Files:**
- Create: `content/file.go`
- Create: `content/file_test.go`

**Interfaces:**
- Consumes: `content.Store` and typed content identity from Task 1.
- Produces: `type Repository struct` and `func NewRepository(Store) (*Repository, error)`.
- Produces: `func (*Repository) IngestFile(context.Context, string, string, io.Reader) (File, error)`.
- Produces: `func (*Repository) CopyFile(context.Context, File, io.Writer) error`.
- Produces: `func (*Repository) MaterializeFile(context.Context, File, string) error`.

- [ ] **Step 1: Add failing media-selection and immutable-ingestion tests**

Table-test the exact precedence:

```go
tests := []struct {
	name, explicit string
	content        []byte
	want           string
}{
	{"explicit wins", "application/x-custom", []byte("plain"), "application/x-custom"},
	{"sniff pdf", "", []byte("%PDF-1.7\n"), "application/pdf"},
	{"sniff text", "", []byte("hello\n"), "text/plain; charset=utf-8"},
	{"honest fallback", "", []byte{0x00, 0xff, 0x00, 0xfe}, "application/octet-stream"},
}
```

Ingest from an open host file, mutate and delete the host file, then copy the committed `File` and assert the original bytes, logical filename, media, and length remain unchanged. Include a multi-megabyte reader that errors if a caller requests an unbounded all-at-once read.

- [ ] **Step 2: Run file tests and verify RED**

```bash
go test ./content -run 'Test(File|Ingest|Media)' -count=1
```

Expected: FAIL because repository file operations do not exist.

- [ ] **Step 3: Implement streaming ingestion and media choice**

`IngestFile` validates and canonically formats one concrete media type with `mime.ParseMediaType`/`mime.FormatMediaType` when supplied. Otherwise, buffer at most the first 512 bytes for `http.DetectContentType`, then stream the prefix plus remainder into `Store.Put`. Keep the logical name as opaque semantic text; never join it to a filesystem path.

`MaterializeFile` writes to a runtime-owned exact location via a same-directory temporary file, verifies the content digest while copying, closes, and renames. It never uses the logical filename as a destination.

- [ ] **Step 4: Run file, value, race, and complete tests for GREEN**

```bash
go test ./content ./value -count=1
go test -race ./content ./value -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit file lifecycle support**

```bash
git add content/file.go content/file_test.go
git commit -m "feat(content): ingest immutable file values"
```

---

### Task 4: Portable deterministic tree capture and materialization

**Files:**
- Create: `content/tree.go`
- Create: `content/tree_wire.go`
- Create: `content/tree_test.go`
- Create: `content/tree_materialize_test.go`

**Interfaces:**
- Consumes: `content.Repository`, `content.Store`, and `content.Tree` from Tasks 1–2.
- Produces: `func (*Repository) CaptureTree(context.Context, string) (Tree, error)`.
- Produces: `func (*Repository) MaterializeTree(context.Context, Tree, string) error`.
- Produces: `func (*Repository) Entries(context.Context, Tree) ([]TreeEntry, error)` for read-only diagnostics/manifests.

- [ ] **Step 1: Add failing portable tree round-trip tests**

Create a source containing:

```text
empty/
bin/tool        executable regular file
docs/report.txt regular file
links/report    -> ../docs/report.txt
```

Capture, materialize into a fresh directory, and assert byte-identical files, the executable bit, the empty directory, and the relative symlink. Change timestamps, ownership-irrelevant permission bits, and source enumeration order; assert the same tree value. Change one semantic fact and assert a different tree value.

- [ ] **Step 2: Add failing rejection and backend-honesty tests**

Add cases for absolute symlinks, lexical escapes, escaping symlink chains, symlink cycles without a fully resolved target, FIFOs, sockets, devices where the platform permits creation, unreadable entries, context cancellation, and a corrupted manifest/blob.

Make unreadable-entry and mid-walk failure tests deterministic through one unexported filesystem-operations seam on `Repository`; do not expose capture policy or test injection in the public API.

Construct two manifest entries that are distinct byte names but collide on the test filesystem when materialized. Assert materialization fails rather than overwriting one. Do not reject either name globally; the concrete backend reports that it cannot reproduce the tree exactly.

- [ ] **Step 3: Run tree tests and verify RED**

```bash
go test ./content -run 'Test(Tree|Capture|Materialize)' -count=1
```

Expected: FAIL because portable tree storage does not exist.

- [ ] **Step 4: Implement a structured, lossless tree manifest**

Store each entry as structured path segments plus one closed kind:

```go
type treeKind uint8
const (
	directoryEntry treeKind = iota + 1
	fileEntry
	executableEntry
	symlinkEntry
)

type treeEntry struct {
	segments []string
	kind     treeKind
	content  Object
	target   string
}
```

The private `dawn.tree/1` binary encoding length-prefixes raw segment/target bytes and stores entries in canonical segment order. Directories, including empty directories, are explicit. Timestamps, ownership, group, xattrs, and non-executable permission bits never enter the manifest.

- [ ] **Step 5: Implement full capture validation**

Walk without following symlinks. Every special or unsupported entry returns an error; none are skipped. Store regular bytes through `Store.Put`. After enumeration, resolve every symlink through the captured entry graph: reject absolute targets, any resolution step that leaves the root, and cycles that never produce a fully resolved target.

There is no ignore file, base diff, Git invocation, or best-effort omission. Full workspace publication means the exact workspace root; Dawn-owned staging roots are excluded structurally by living outside it.

- [ ] **Step 6: Implement exact materialization**

Decode and revalidate the manifest before writing. Create directories first, regular files atomically second, and symlinks last. Before every create, use `Lstat` to reject an existing destination; this surfaces case-folding or normalization collisions on the concrete backend without banning semantic names globally.

`MaterializeTree` requires an absent runtime-owned destination and creates it. If any step fails, remove only that newly created destination and return an error. `MaterializeFile` similarly requires an absent exact target. The committed `Tree` remains unchanged and no caller-owned directory is cleared.

- [ ] **Step 7: Run tree, race, and complete tests for GREEN**

```bash
go test ./content -count=1
go test -race ./content -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit portable tree support**

```bash
git add content/tree.go content/tree_wire.go content/tree_test.go content/tree_materialize_test.go
git commit -m "feat(content): capture portable immutable trees"
```

---

### Task 5: Canonical leaf delivery intent

**Files:**
- Modify: `workflow/draft.go`
- Modify: `workflow/model.go`
- Modify: `workflow/compile.go`
- Modify: `workflow/canonical.go`
- Create: `workflow/delivery.go`
- Test: `workflow/delivery_test.go`
- Modify: `workflow/canonical_test.go`
- Modify: `workflow/model_test.go`

**Interfaces:**
- Consumes: existing immutable `workflow.Leaf`, input/output contracts, and `value.Type` traversal.
- Produces: `type Fidelity uint8` with exactly `TextFidelity` and `VisualFidelity`.
- Produces: `type AttachmentDraft struct { Input []string; Fidelity Fidelity }`.
- Adds to `LeafDraft`: `BaseTree []string`, `PublishWorkspace []string`, and `Attachments []AttachmentDraft`.
- Produces read-only canonical accessors on `Leaf`: `BaseTree() []string`, `PublishWorkspace() []string`, and `Attachments() []Attachment`.
- Produces: `func DefaultFidelity(media string) Fidelity` for concrete media at request resolution.

- [ ] **Step 1: Add failing semantic-declaration tests**

Cover these accepted shapes:

```go
LeafDraft{
	Kind: Agent, Inputs: agentInputs, Outputs: agentOutputs,
	BaseTree: []string{"source"},
	PublishWorkspace: []string{"continued"},
}

LeafDraft{
	Kind: LLM, Inputs: llmInputs, Outputs: report,
	Attachments: []AttachmentDraft{{Input: []string{"document"}, Fidelity: VisualFidelity}},
}
```

Assert agent/script need no declaration for an empty workspace and automatic named staging/capture. Assert `DefaultFidelity("application/pdf")` and every `image/*` value are visual; `text/*` and other media default to text unless one declared override applies.

- [ ] **Step 2: Add failing rejection tests**

Reject:

- base paths that do not resolve to a required tree input;
- publication paths that do not resolve to a required tree output;
- workspace declarations on `llm` or `gate`;
- attachment declarations on `agent`, `script`, or `gate`;
- attachment paths whose selected subtree contains no file;
- duplicate or overlapping attachment overrides that would give one file two meanings;
- unknown fidelity; and
- any raw-LLM input contract containing a tree at any depth.

Do not reject file names, field names, or map keys by character content.

- [ ] **Step 3: Run delivery tests and verify RED**

```bash
go test ./workflow -run 'TestCompile.*(Workspace|Attachment|Fidelity|TreeInput)' -count=1
```

Expected: FAIL because canonical leaves carry contracts only.

- [ ] **Step 4: Implement one closed validation/lowering path**

Clone every path and declaration during compilation. Require the selected base/publication path and every segment above it to be present; absence cannot silently change an invocation from continued workspace to empty workspace.

Attachment override paths use existing static contract resolution. An override on a homogeneous list/map subtree applies uniformly to every concrete file below that schema position. Sort and deduplicate canonical attachment declarations by structured input path.

- [ ] **Step 5: Include delivery intent in immutable inspection and canonical identity**

Add the three semantic fields to the private workflow wire. Reversed attachment source order must inspect and encode identically. Changing a base path, publication path, fidelity, or attachment subtree must change canonical bytes. Returned paths/slices must be defensive copies.

- [ ] **Step 6: Run workflow, race, and complete tests for GREEN**

```bash
go test ./workflow ./value -count=1
go test -race ./workflow ./value -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit canonical delivery semantics**

```bash
git add workflow/draft.go workflow/model.go workflow/compile.go workflow/canonical.go workflow/delivery.go workflow/delivery_test.go workflow/canonical_test.go workflow/model_test.go
git commit -m "feat(workflow): declare file delivery semantics"
```

---

### Task 6: Fresh workspace preparation and deterministic manifests

**Files:**
- Create: `workspace/manifest.go`
- Create: `workspace/prepare.go`
- Create: `workspace/prepare_test.go`
- Create: `workspace/slots_test.go`

**Interfaces:**
- Consumes: `content.Repository`, compiled `workflow.Leaf`, concrete input `value.Value`, and structured `value.Path`.
- Produces: `func Prepare(context.Context, *content.Repository, workflow.Leaf, value.Value) (*Environment, error)`.
- Produces: immutable `Manifest`, `Input`, and `Output` inspection records.
- Produces: `Environment.Workspace() string`, `Environment.Manifest() Manifest`, `Environment.Output(value.Path) (Target, error)`, and idempotent `Environment.Close() error`.

- [ ] **Step 1: Add failing preparation and manifest tests**

Prepare an agent input containing a base tree, one PDF, an object of files, a list of files, and a map with adversarial keys such as `"../x"`, `"a/b"`, `"a\\b"`, empty text, invalid UTF-8 bytes, and two names that normalize alike on the host.

Assert:

- one new workspace is created and contains exactly the selected base tree;
- every other file/tree has one distinct input record and runtime-owned location outside the workspace;
- logical names, media, lengths, digests, and structured value paths are preserved in the manifest;
- input bytes are read-only presentation copies and edits never mutate committed content;
- output records exist for every static file/tree leaf and one namespace exists for each dynamic list/map file/tree schema position;
- the published-workspace path has no named output slot; and
- manifest ordering and slot locations are deterministic for the same leaf/value.

- [ ] **Step 2: Add failing independence and rollback tests**

Prepare two environments concurrently from one base tree, edit the same file differently in each workspace, and assert neither branch nor the committed tree observes the other's edit.

Inject a missing/corrupt input halfway through preparation. Assert `Prepare` returns a nil environment and removes every allocated runtime root. An LLM leaf must reject workspace preparation because it has no filesystem role.

- [ ] **Step 3: Run workspace preparation tests and verify RED**

```bash
go test ./workspace -run 'Test(Prepare|Manifest|Slots|Parallel)' -count=1
```

Expected: FAIL because `workspace` does not exist.

- [ ] **Step 4: Implement deterministic slot identities without semantic-name restrictions**

Hash the private canonical encoding of each structured `value.Path` with a domain prefix to obtain a fixed physical slot component. Keep the original path only in the manifest. Detect a duplicate physical identity as an internal integrity error.

Use fixed runtime-owned children such as `workspace`, `inputs`, and `outputs` beneath a fresh `os.MkdirTemp` root. These names are internal implementation detail, not workflow syntax. Store file content at a fixed name inside its slot directory; never use logical filenames or map keys as physical path components.

- [ ] **Step 5: Implement input traversal and output schema traversal**

Walk the concrete input value together with its contract. Materialize the selected base tree into the writable workspace and record its value path there. Materialize every other file/tree into its own input slot, then make the staging tree read-only on a best-effort presentation basis while documenting that same-user isolation is not claimed.

Walk the output contract without concrete values. Create exact slots for statically known file/tree leaves and namespace roots at homogeneous list/map positions. `Environment.Output` matches a concrete path against that schema and lazily creates one confined dynamic member slot.

- [ ] **Step 6: Implement cleanup and preparation atomicity**

Own the complete temporary root in `Environment`. On preparation failure, restore permissions needed for deletion and remove it. `Close` is idempotent and removes private workspace, input, and output state. No invocation path enters a committed value.

- [ ] **Step 7: Run workspace, content, race, and complete tests for GREEN**

```bash
go test ./workspace ./content ./value ./workflow -count=1
go test -race ./workspace ./content ./value ./workflow -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit workspace preparation**

```bash
git add workspace/manifest.go workspace/prepare.go workspace/prepare_test.go workspace/slots_test.go
git commit -m "feat(workspace): prepare private invocation roots"
```

---

### Task 7: All-or-nothing named output and workspace capture

**Files:**
- Create: `workspace/capture.go`
- Create: `workspace/capture_test.go`
- Create: `workspace/atomicity_test.go`

**Interfaces:**
- Consumes: prepared `Environment`, output contract, `content.Repository`, `value.Path`, and runtime `value.Value` constructors.
- Produces: `type Outputs` with `Value(context.Context, value.Path) (value.Value, error)` and `Workspace(context.Context) (value.Value, error)`.
- Produces: `func (*Environment) Capture(context.Context, func(Outputs) (value.Value, error)) (value.Value, error)`.

- [ ] **Step 1: Add failing exact-slot capture tests**

Use `Environment.Output(path)` to obtain runtime-owned targets. Assert:

- a file target accepts exactly one regular file at its root and derives logical filename/media/length/content from it;
- an empty tree target captures a valid empty tree;
- nested object, list, and map file/tree outputs resolve through structured concrete paths;
- missing required outputs, extra files in a file slot, wrong kinds, special entries, escaping symlinks, undeclared output-root entries, and media mismatches fail; and
- private files elsewhere in the workspace are absent unless workspace publication is declared and requested.

- [ ] **Step 2: Add failing one-candidate atomicity tests**

Build the candidate only inside `Capture`:

```go
candidate, err := env.Capture(ctx, func(outputs Outputs) (value.Value, error) {
	report, err := outputs.Value(ctx, value.RootPath().Field("report"))
	if err != nil { return value.Value{}, err }
	continued, err := outputs.Workspace(ctx)
	if err != nil { return value.Value{}, err }
	return value.NewObject(
		mustEntry(t, "summary", mustStringValue(t, "complete")),
		mustEntry(t, "report", report),
		mustEntry(t, "continued", continued),
	)
})
```

Inject failure after the first file blob, during the second tree manifest, during workspace capture, during candidate assembly, and during final contract validation. Every case must return the zero `value.Value`; no captured subvalue escapes through the API. Already stored bytes may remain unreachable for later garbage collection.

- [ ] **Step 3: Run capture tests and verify RED**

```bash
go test ./workspace -run 'Test(Capture|Candidate|Atomic)' -count=1
```

Expected: FAIL because all-or-nothing capture does not exist.

- [ ] **Step 4: Implement lazy capture through one deep boundary**

`Outputs.Value` validates the requested concrete path against the output contract and manifest, captures only its Dawn-owned slot, memoizes the immutable result, and never accepts an arbitrary host path. `Outputs.Workspace` exists only when the compiled leaf declares publication and captures exactly the workspace root.

After the callback returns, `Capture`:

1. validates the complete candidate against the leaf output contract;
2. walks every file/tree leaf and proves it is exactly one value captured by this `Outputs` instance at that semantic path;
3. proves no requested capture is placed at a different path;
4. rejects undeclared physical output entries; and
5. returns the candidate only after every check succeeds.

This is candidate atomicity, not a durable node commit. GitHub #8 later stores the returned candidate reference and journal fact atomically.

- [ ] **Step 5: Run workspace, race, and complete tests for GREEN**

```bash
go test ./workspace ./content ./value ./workflow -count=1
go test -race ./workspace ./content ./value ./workflow -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit candidate capture**

```bash
git add workspace/capture.go workspace/capture_test.go workspace/atomicity_test.go
git commit -m "feat(workspace): capture one complete candidate"
```

---

### Task 8: Prestige-shaped value-flow proof and implementation evidence

**Files:**
- Create: `workspace/conformance_test.go`
- Modify: `docs/superpowers/specs/2026-08-09-values-contracts-workspaces-files-design.md`

**Interfaces:**
- Consumes: completed `content`, `value`, `workflow`, and `workspace` kernels.
- Produces: one syntax-independent Prestige-shaped tracer and honest implementation evidence.

- [ ] **Step 1: Build the failing Prestige-shaped fixture**

Construct and execute through direct Go APIs:

1. ingest a source repository tree, PDF, validator tree, and skill-library tree;
2. prepare two agent/script branches from the same source tree and prove private writable divergence;
3. publish each branch's named JSON/report files and only the explicitly selected workspace tree;
4. fan in with one branch tree as the sole writable base and the other branch tree as an immutable named input;
5. perform an explicit merge in test code and publish a new tree;
6. pass the same PDF as a visual raw-LLM attachment requirement and as an agent workspace file input;
7. assemble validation, scoring, remediation, and final-report values as one nested contracted graph; and
8. fail one late capture and prove no candidate becomes visible.

Destroy every producer environment before preparing the final consumer, then rematerialize solely from committed file/tree/value content. Corrupt one referenced object and assert the consumer halts with an integrity error rather than substituting a live workspace or host file.

Assert there is no shared writable directory, automatic merge, author destination path, provider file ID, document node, Docker node, artifact map, or workspace value.

- [ ] **Step 2: Run the tracer and verify RED before integration fixes**

```bash
go test ./workspace -run 'TestPrestigeValueFlow|TestProductInvariant' -count=1
```

Expected: at least one cross-module mismatch. Fix only the owning module and add a regression test there; do not add a compatibility or provider layer.

- [ ] **Step 3: Record exact implementation evidence**

Append `## Implementation Evidence` to the #6 spec with the actual commit IDs and subjects for Tasks 1–7, the verification commands below, and this boundary statement:

> This implementation provides immutable values/content and local workspace preparation/candidate capture. GitHub #7 owns structured scheduling and propagation; #8 owns durable commits, replay, and reuse; #9 owns adapter preparation/run/recovery; #10 owns the native script ABI and process lifecycle; #11 owns sub-workflow linking; #12 owns author syntax.

- [ ] **Step 4: Run the complete verification matrix**

```bash
go test ./content ./value ./workflow ./workspace -count=1
go test -race ./content ./value ./workflow ./workspace -count=1
go test ./... -count=1
go vet ./...
git diff --check
```

Verify the new inward modules do not depend on legacy or later runtime packages:

```bash
if rg -n 'github\.com/valbaudo/dawn/(store|plan|gate|backend|proc)|dawn\.(Ref|Backend|Invocation|Result)' content value workflow workspace; then exit 1; fi
```

Verify no duplicate artifact/workspace/path/policy surface entered the new language:

```bash
if rg -n 'KindWorkspace|artifact(s)?\s+map|input_files|output_files|capture_path|destination_path|merge_policy|provider_file_id|workspace_dir|mounts:' content value workflow workspace; then exit 1; fi
```

Expected: every command exits zero and both scans produce no matches.

- [ ] **Step 5: Commit the tracer and evidence**

```bash
git add workspace/conformance_test.go docs/superpowers/specs/2026-08-09-values-contracts-workspaces-files-design.md
git commit -m "test(workspace): prove immutable Prestige value flow"
```

## Final Verification

```bash
go test ./content ./value ./workflow ./workspace -count=1
go test -race ./content ./value ./workflow ./workspace -count=1
go test ./... -count=1
go vet ./...
git diff --check
if rg -n 'github\.com/valbaudo/dawn/(store|plan|gate|backend|proc)|dawn\.(Ref|Backend|Invocation|Result)' content value workflow workspace; then exit 1; fi
if rg -n 'KindWorkspace|artifact(s)?\s+map|input_files|output_files|capture_path|destination_path|merge_policy|provider_file_id|workspace_dir|mounts:' content value workflow workspace; then exit 1; fi
```

Expected: every command exits zero and both searches produce no matches.

## Acceptance Mapping

| Approved #6 result | Implementation task |
| --- | --- |
| Recursive typed values and strict bindings | Task 2 |
| Immutable file facts and media semantics | Tasks 2–3 |
| Portable deterministic trees | Task 4 |
| Workspace base/publication and raw attachment intent | Task 5 |
| One fresh private writable workspace | Task 6 |
| Named read-only side inputs and fixed output slots | Task 6 |
| Arbitrary nested/list/map file paths without destination knobs | Tasks 2 and 6 |
| Atomic structured plus file/tree candidate | Task 7 |
| Parallel branching and explicit fan-in | Task 8 |
| Raw LLM versus agent delivery boundary | Tasks 5 and 8 |
| No automatic merge, artifact channel, or workspace value | Global constraints and Task 8 |
| Honest later-ticket boundary | Task 8 evidence |

## Plan Self-Review

- **Spec coverage:** Tasks 1–4 implement immutable structured/file/tree storage; Task 5 captures the only missing canonical leaf semantics; Tasks 6–7 implement one private workspace, named slots, manifests, and candidate atomicity; Task 8 proves parallel fan-in, raw attachment versus workspace delivery, and Prestige-shaped composition.
- **Subsystem boundary:** Storage, values, orchestration declarations, and ephemeral filesystem execution each hide one distinct body of knowledge. Their interfaces point inward and no module needs an adapter, scheduler, journal, script process, or legacy runtime.
- **Artificial-limitation audit:** Semantic names and keys remain unrestricted opaque bytes. Physical paths derive from structured identities. Concrete filesystem collisions fail only on a backend that cannot reproduce the value; they do not become global naming bans.
- **Guardrail audit:** The only rejections protect type truth, immutable content integrity, root confinement, exact reproduction, one-source workspace semantics, or all-or-nothing visibility. No restriction exists merely to simplify implementation.
- **Placeholder scan:** Every task names exact files, interfaces, tests, commands, expected failures, implementation behavior, and a commit boundary. No `TBD`, compatibility placeholder, generic error-handling step, or unowned future interface remains.
- **Type consistency:** `content.File`/`Tree` flow into recursive `value.Value`; compiled `workflow.Leaf` plus concrete input `value.Value` enter `workspace.Prepare`; `Environment.Capture` returns one contract-validated `value.Value`. Runtime paths and content digests never become workflow strings.
- **Complexity audit:** Four modules are justified by four independent secrets. There is no repository/service factory hierarchy, registry, provider feature matrix, generic artifact abstraction, policy object, event bus, plugin surface, or alternate dependency channel.
