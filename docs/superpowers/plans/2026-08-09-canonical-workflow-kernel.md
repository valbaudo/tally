# Canonical Workflow Kernel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Dawn vNext's immutable, typed, hierarchical workflow definition and its one syntax-independent compilation path, so every later scheduler, ledger, adapter, script, and author-format implementation shares one closed semantic model.

**Architecture:** Add two inward packages. `value` owns the small recursive contract algebra and compile-time ordinary literals because typed ports cannot be validated without them; the later #6 implementation extends this package with immutable runtime values, files, trees, storage, and workspace materialization. `workflow` owns source-neutral drafts, static module resolution, lowering, lexical binding validation, fixed-graph cycle detection, structured-scope shape validation, provenance, and the immutable compiled definition. It exposes no scheduler, persistence, adapter, YAML, legacy-plan, or execution API.

**Tech Stack:** Go 1.26 standard library; `encoding/json` for canonical ordinary literals and compiled-definition encoding; no new dependency.

## Global Constraints

- Implement `docs/superpowers/specs/2026-08-09-canonical-workflow-semantic-model-design.md`.
- Use the already-approved contract algebra from `docs/superpowers/specs/2026-08-09-values-contracts-workspaces-files-design.md` only where typed ports, literals, and bindings require it. File bytes, tree capture, workspaces, manifests, and storage remain in the later #6 plan.
- The canonical leaf kinds are exactly `llm`, `agent`, `script`, and `gate`.
- The canonical scope kinds are exactly `graph`, `branch`, `map`, `loop`, and `finally`.
- `parallel` and `call` exist only in compilation drafts and must lower to ordinary canonical `graph` scopes with provenance.
- Preserve hierarchy. Do not flatten descendants into a global graph or expose runtime instance paths as source references.
- An edge is the only dependency relation. An edge may carry typed port bindings or no bindings for completion-only ordering.
- A literal binds directly to a child input and never becomes a fake node.
- Source order has no scheduling meaning and must not change canonical compiled bytes.
- Reject unknown kinds, unresolved or cross-scope references, multiply bound inputs, incompatible bindings, incompatible branch contracts, and fixed-graph cycles before execution.
- Add no YAML spelling, expression language, scheduler, runtime capacity, concurrency setting, retry field, attempt count, failure-policy knob, adapter registry, provider configuration, journal format, store URI, workspace path, Docker branch, or compatibility layer.
- Do not import or adapt current `dawn.Backend`, `dawn.Invocation`, `dawn.Result`, `plan.Plan`, `plan.Step`, `gate`, or AWF types.
- The old implementation may coexist only as untouched code while the vNext branch is built. Add no alias, shim, converter, dual-write path, or backward-compatible decoder; the final vNext cutover deletes it in a later integration plan.
- Use red-green-refactor and commit every task independently.
- Preserve unrelated untracked files.

## Package Boundary

| Package | One responsibility | Public surface after this plan |
| --- | --- | --- |
| `value` | Define canonical data contracts and compile-time ordinary literals | `Type`, `Field`, `Contract`, `Literal`, constructors, path lookup, validation, and assignability |
| `workflow` | Compile source-neutral workflow drafts into one immutable hierarchical definition | draft types, `Compile`, `Definition`, read-only graph/node/edge accessors, provenance, and four terminal status names |

`value` knows nothing about workflows. `workflow` depends on `value`. Later runtime packages depend inward on both. Neither package imports execution, adapters, persistence, or the current root package.

## File Structure

| File | Responsibility |
| --- | --- |
| `value/type.go` | Closed recursive type algebra and constructors |
| `value/contract.go` | Closed port contracts, object-path lookup, equality, and assignability |
| `value/literal.go` | One canonical ordinary JSON literal and contract validation |
| `workflow/draft.go` | Source-neutral compilation input, including `call` and `parallel` sugar |
| `workflow/model.go` | Immutable canonical definition, graph, node, edge, scope, provenance, and outcomes |
| `workflow/compile.go` | Module resolution, recursive lowering, deep copy, and compilation entry point |
| `workflow/bindings.go` | Lexical endpoint resolution, typed bindings, literals, and one-source validation |
| `workflow/validate.go` | Fixed-graph cycles and exact structured-scope shape checks |
| `workflow/canonical.go` | Deterministic canonical definition encoding |
| `workflow/conformance_test.go` | Prestige-shaped structural tracer and product invariants |

Keep contract knowledge in `value` and graph knowledge in `workflow`. Do not split constructors, validation phases, or individual scope kinds into shallow packages.

---

### Task 1: Establish the canonical contract kernel

**Files:**
- Create: `value/type.go`
- Create: `value/type_test.go`
- Create: `value/contract.go`
- Create: `value/contract_test.go`
- Create: `value/literal.go`
- Create: `value/literal_test.go`

**Interfaces:**
- Produces: `value.Type`, `value.Field`, `value.Contract`, `value.Literal`, and `value.Assignment`.
- Produces: scalar and composite constructors, `ParseLiteral`, `Contract.Resolve`, `Type.ValidateLiteral`, `Contract.ValidateLiteral`, and `CheckAssignable`.
- Consumes: no Dawn package.

- [ ] **Step 1: Write failing closed-algebra and construction tests**

Create table tests covering every approved kind and invalid construction:

```go
func TestTypeAlgebraIsClosed(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		kind Kind
	}{
		{"string", String(), StringKind},
		{"integer", Integer(), IntegerKind},
		{"number", Number(), NumberKind},
		{"boolean", Boolean(), BooleanKind},
		{"null", Null(), NullKind},
		{"any", Any(), AnyKind},
		{"file", mustType(t, File("application/pdf", "image/*")), FileKind},
		{"tree", Tree(), TreeKind},
		{"list", mustType(t, List(String())), ListKind},
		{"map", mustType(t, Map(Boolean())), MapKind},
		{"object", mustType(t, Object(mustField(t, Required("name", String())))), ObjectKind},
		{"enum", mustType(t, Enum(mustLiteral(t, `"low"`), mustLiteral(t, `"high"`))), EnumKind},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.typ.Kind(); got != tc.kind {
				t.Fatalf("Kind() = %v, want %v", got, tc.kind)
			}
		})
	}
}
```

Add neighboring cases proving:

- the zero `Type` and zero `Contract` are invalid;
- `EmptyContract()` is valid and distinct from a zero contract;
- fields and top-level ports reject an empty name and duplicates;
- an enum requires at least one scalar literal, rejects arrays/objects, removes exact duplicates, and keeps integer `1` distinct from number `1.0`;
- media constraints accept exact `type/subtype` and `type/*`, reject malformed values, sort, and deduplicate;
- `Object`, `List`, and `Map` reject an invalid nested type; and
- all exported getters return defensive copies.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./value -run 'Test(Type|Contract).*' -count=1
```

Expected: FAIL because package `value` does not exist.

- [ ] **Step 3: Define the exact algebra with construction-time validity**

In `value/type.go`, use an unexported representation so an accepted type cannot later be mutated:

```go
type Kind uint8

const (
	InvalidKind Kind = iota
	StringKind
	IntegerKind
	NumberKind
	BooleanKind
	NullKind
	EnumKind
	ObjectKind
	MapKind
	ListKind
	FileKind
	TreeKind
	AnyKind
)

type Field struct {
	name     string
	typ      Type
	optional bool
}

type Type struct {
	kind  Kind
	elem  *Type
	fields []Field
	enum  []Literal
	media []string
}

func String() Type
func Integer() Type
func Number() Type
func Boolean() Type
func Null() Type
func Tree() Type
func Any() Type
func File(media ...string) (Type, error)
func Enum(values ...Literal) (Type, error)
func List(element Type) (Type, error)
func Map(element Type) (Type, error)
func Object(fields ...Field) (Type, error)
func Required(name string, typ Type) (Field, error)
func Optional(name string, typ Type) (Field, error)
```

`Kind`, `Element`, `Fields`, `EnumValues`, and `Media` are read-only accessors. `Element` returns `(Type, bool)`; slice accessors allocate new slices. Constructors validate once, copy incoming data, sort fields by name, sort media lexically, and sort enum members by their canonical literal bytes.

In `value/contract.go`, make an explicit valid empty contract possible without making `Contract{}` valid:

```go
type Contract struct {
	valid  bool
	ports  []Field
}

func NewContract(ports ...Field) (Contract, error)
func EmptyContract() Contract
func (c Contract) Valid() bool
func (c Contract) Ports() []Field
func (c Contract) Resolve(path ...string) (Type, bool)
func (c Contract) Equal(other Contract) bool
func (c Contract) ObjectType() Type
```

`Resolve` accepts a top-level port followed only by statically declared object fields. It does not perform list indexing, map lookup, JSONPath, coercion, or filesystem traversal.

- [ ] **Step 4: Add failing literal and assignability tests**

```go
func TestCheckAssignable(t *testing.T) {
	tests := []struct {
		name        string
		from, to    Type
		wantRuntime bool
		wantErr     bool
	}{
		{"exact", String(), String(), false, false},
		{"integer widens", Integer(), Number(), false, false},
		{"enum widens", mustType(t, Enum(mustLiteral(t, `"a"`))), String(), false, false},
		{"concrete to any", String(), Any(), false, false},
		{"any to concrete validates later", Any(), String(), true, false},
		{"file is not ordinary any", mustType(t, File()), Any(), false, true},
		{"string is not parsed", String(), Integer(), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckAssignable(tc.from, tc.to)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckAssignable() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got.RuntimeValidation != tc.wantRuntime {
				t.Fatalf("RuntimeValidation = %v, want %v", got.RuntimeValidation, tc.wantRuntime)
			}
		})
	}
}
```

Add recursive cases for lists, maps, closed objects, required-to-optional widening, optional-to-required rejection, exact file/tree kinds, and file-media checks. A source file contract whose declared media is wholly accepted by the destination is statically assignable; an unconstrained source into a constrained destination requires boundary validation; provably disjoint exact media sets fail.

For literals, cover one JSON value, trailing data, duplicate object keys, optional versus explicit `null`, integer versus number, undeclared object fields, homogeneous lists/maps, enums, and rejection of file/tree contracts. `any` accepts only ordinary JSON data.

- [ ] **Step 5: Implement one canonical ordinary literal path**

In `value/literal.go` define:

```go
type Literal struct {
	canonical []byte
	decoded   any
}

type Assignment struct {
	RuntimeValidation bool
}

func ParseLiteral(data []byte) (Literal, error)
func (l Literal) Bytes() []byte
func (l Literal) Equal(other Literal) bool
func (t Type) ValidateLiteral(l Literal) error
func (c Contract) ValidateLiteral(l Literal) error
func CheckAssignable(from, to Type) (Assignment, error)
```

Parse with `json.Decoder.UseNumber`, require EOF, reject duplicate object keys in a token-level pass, and recursively re-encode objects with lexical key ordering. Preserve JSON number spelling after validating it with `json.Number`; the type checker treats spellings without `.` or exponent as integers and every other valid JSON number as numbers. This plan does not invent arbitrary-precision arithmetic or numeric coercion.

The recursive validator owns all approved rules in one switch. It distinguishes absent optional fields from `null`, rejects undeclared object fields, never parses strings as JSON, and refuses file/tree types because compile-time literals cannot manufacture immutable binary handles.

- [ ] **Step 6: Run the package tests for GREEN**

```bash
go test ./value -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the contract kernel**

```bash
git add value/type.go value/type_test.go value/contract.go value/contract_test.go value/literal.go value/literal_test.go
git commit -m "feat(value): define canonical workflow contracts"
```

---

### Task 2: Define drafts and the immutable canonical model

**Files:**
- Create: `workflow/draft.go`
- Create: `workflow/model.go`
- Create: `workflow/model_test.go`

**Interfaces:**
- Consumes: `value.Contract`, `value.Type`, and `value.Literal` from Task 1.
- Produces: source-neutral `ProgramDraft`, `ModuleDraft`, `GraphDraft`, and node variant drafts.
- Produces: immutable `Definition`, `Graph`, `Node`, `Edge`, scope records, `Provenance`, and `Status`.

- [ ] **Step 1: Write failing closed-kind and immutable-model tests**

```go
func TestCanonicalKindsAreClosed(t *testing.T) {
	for _, kind := range []LeafKind{LLM, Agent, Script, Gate} {
		if !validLeafKind(kind) { t.Fatalf("validLeafKind(%q) = false", kind) }
	}
	if validLeafKind(LeafKind("plugin")) {
		t.Fatal("plugin-defined leaf kind was accepted")
	}
	for _, kind := range []ScopeKind{GraphScope, BranchScope, MapScope, LoopScope, FinallyScope} {
		if !validScopeKind(kind) { t.Fatalf("validScopeKind(%q) = false", kind) }
	}
	if validScopeKind(ScopeKind("plugin")) {
		t.Fatal("plugin-defined scope kind was accepted")
	}
}
```

Add tests that pass every draft variant through a test-local model constructor and prove:

- a node contains exactly one of leaf, graph, branch, map, loop, call, or parallel;
- graph boundaries and child endpoints are different typed values rather than a reserved name;
- mutating any draft slice, contract input, or provenance byte slice after compilation cannot change the compiled definition; and
- accessor-returned slices can be mutated without changing the definition.

Task 3's observable lowering tests—not constant-presence tests—prove `call` and `parallel` are draft-only. Later scheduler conformance tests prove terminal outcome behavior; this task defines the four status names without adding tests that merely restate constants.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./workflow -run 'Test(CanonicalKinds|Model|DefinitionImmutable)' -count=1
```

Expected: FAIL because package `workflow` does not exist.

- [ ] **Step 3: Define the source-neutral draft boundary**

In `workflow/draft.go`, define plain construction records. They are compilation input, not author YAML and not durable runtime state:

```go
type ProgramDraft struct {
	Root    string
	Modules []ModuleDraft
}

type ModuleDraft struct {
	Name       string
	Graph      GraphDraft
	Provenance Provenance
}

type GraphDraft struct {
	Inputs     value.Contract
	Outputs    value.Contract
	Nodes      []NodeDraft
	Edges      []EdgeDraft
	Finally    *GraphDraft
	Provenance Provenance
}

type NodeDraft struct {
	Name       string
	Leaf       *LeafDraft
	Graph      *GraphDraft
	Branch     *BranchDraft
	Map        *MapDraft
	Loop       *LoopDraft
	Call       *CallDraft
	Parallel   *ParallelDraft
	Literals   []LiteralBindingDraft
	Provenance Provenance
}

type LeafDraft struct {
	Kind    LeafKind
	Inputs  value.Contract
	Outputs value.Contract
}

type CallDraft struct { Module string }
type ParallelDraft struct { Graph GraphDraft }
```

Define the edge construction records here as model data; Task 4 adds their semantic validation rather than introducing types after `GraphDraft` already depends on them:

```go
type EndpointKind uint8
const (
	Boundary EndpointKind = iota + 1
	Child
)

type EndpointDraft struct {
	Kind EndpointKind
	Child string
}

type BindingDraft struct {
	From []string
	To   string
}

type EdgeDraft struct {
	From, To EndpointDraft
	Bindings []BindingDraft
}

type LiteralBindingDraft struct {
	Input string
	Value value.Literal
}
```

Also define the exact structured draft shapes used for static checks:

```go
type CaseDraft struct { Name string; Graph GraphDraft }
type BranchDraft struct {
	Inputs, Outputs value.Contract
	Selector string
	Cases []CaseDraft
}
type MapDraft struct {
	Inputs, Outputs value.Contract
	Collection, Result string
	Body GraphDraft
}
type LoopDraft struct {
	Inputs, Outputs value.Contract
	Maximum int
	Termination []string
	Body GraphDraft
}
```

There is no draft `FinallyKind`: cleanup is the single `GraphDraft.Finally` attachment and compiles to a canonical `Finally` record. This prevents an unattached cleanup scope or more than one cleanup attachment.

- [ ] **Step 4: Define the immutable compiled representation**

In `workflow/model.go`, define closed tags and private state:

```go
type LeafKind string
const (
	LLM LeafKind = "llm"
	Agent LeafKind = "agent"
	Script LeafKind = "script"
	Gate LeafKind = "gate"
)

type ScopeKind string
const (
	GraphScope ScopeKind = "graph"
	BranchScope ScopeKind = "branch"
	MapScope ScopeKind = "map"
	LoopScope ScopeKind = "loop"
	FinallyScope ScopeKind = "finally"
)

type Status string
const (
	Succeeded Status = "succeeded"
	Rejected Status = "rejected"
	Failed Status = "failed"
	Cancelled Status = "cancelled"
)

type Definition struct { root Graph; canonical []byte }
type Graph struct {
	inputs, outputs value.Contract
	nodes []Node
	edges []Edge
	cleanup *Finally
	provenance Provenance
}
type Node struct {
	name string
	leaf *Leaf
	scope *Scope
	literals []LiteralBinding
	provenance Provenance
}

type Endpoint struct { kind EndpointKind; child string }
type Binding struct { from []string; to string; runtimeValidation bool }
type Edge struct { from, to Endpoint; bindings []Binding }
type LiteralBinding struct { input string; value value.Literal }
```

`Leaf` holds one closed leaf kind and its contracts. `Scope` is a closed tagged union over graph, branch, map, and loop. `Finally` owns one cleanup graph and is only reachable from its protected graph. All fields stay private; accessors return values or defensive copies. There is no exported constructor for compiled records: `Compile` is the sole creation path.

`Provenance` contains only optional source/module labels and lowering origin needed for diagnostics:

```go
type Origin string
const (
	OriginAuthored Origin = "authored"
	OriginCall Origin = "call"
	OriginParallel Origin = "parallel"
)

type Provenance struct {
	Source string
	Module string
	Origin Origin
}
```

It contains no runtime address, run ID, attempt, provider session, filesystem path, or scheduler state.

- [ ] **Step 5: Run focused tests for GREEN**

```bash
go test ./workflow -run 'Test(CanonicalKinds|Model|DefinitionImmutable)' -count=1
```

Expected: PASS using a test-local compiled fixture; `Compile` itself arrives in Task 3.

- [ ] **Step 6: Commit the model slice**

```bash
git add workflow/draft.go workflow/model.go workflow/model_test.go
git commit -m "feat(workflow): define the canonical hierarchical model"
```

---

### Task 3: Resolve modules and lower `call` and `parallel`

**Files:**
- Create: `workflow/compile.go`
- Create: `workflow/compile_test.go`

**Interfaces:**
- Consumes: all drafts and immutable records from Task 2.
- Produces: `Compile(ProgramDraft) (Definition, error)`.
- Produces: ordinary canonical graph scopes for every draft `call` and `parallel`.

- [ ] **Step 1: Add failing compilation and lowering tests**

Construct `analyze`, `report`, and `root` modules. Make root call `analyze` and contain a two-child parallel draft. Assert:

```go
func TestCompileLowersCallAndParallelToGraphs(t *testing.T) {
	def, err := Compile(loweringFixture(t))
	if err != nil { t.Fatal(err) }

	call := mustNode(t, def.Root(), "analysis")
	if call.ScopeKind() != GraphScope || call.Provenance().Origin != OriginCall {
		t.Fatalf("call = kind %q provenance %#v", call.ScopeKind(), call.Provenance())
	}
	parallel := mustNode(t, def.Root(), "reviewers")
	if parallel.ScopeKind() != GraphScope || parallel.Provenance().Origin != OriginParallel {
		t.Fatalf("parallel = kind %q provenance %#v", parallel.ScopeKind(), parallel.Provenance())
	}
}
```

Add cases for an absent root module, duplicate module names, empty module/node names, unknown calls, direct and indirect recursive calls, fewer than two parallel children, more than one node variant, and nested calls. Assert a caller sees only the called module's declared boundary contracts; no descendant becomes addressable in its parent.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./workflow -run 'TestCompile.*(Module|Call|Parallel)' -count=1
```

Expected: FAIL because `Compile` does not exist.

- [ ] **Step 3: Implement one recursive compiler**

Use one compiler object containing the module table and active call stack:

```go
type compiler struct {
	modules map[string]ModuleDraft
	active  []string
}

func Compile(draft ProgramDraft) (Definition, error) {
	c, err := newCompiler(draft.Modules)
	if err != nil { return Definition{}, err }
	root, err := c.compileModule(draft.Root, OriginAuthored)
	if err != nil { return Definition{}, err }
	return Definition{root: root}, nil
}
```

`compileModule` pushes the module name, rejects a repeated active name with the full call chain, compiles the module graph, records module provenance, and pops with `defer`. `compileNode` requires exactly one non-nil variant.

- A leaf becomes one canonical leaf.
- An authored graph becomes `GraphScope`.
- A call resolves its module, recursively compiles a fresh graph value, and overwrites only the outer graph provenance with `OriginCall` plus the called module name.
- A parallel validates at least two immediate children, recursively compiles its graph, and becomes `GraphScope` with `OriginParallel`.
- Branch, map, and loop retain their canonical scope kinds and compiled child templates.

Never retain draft pointers or slices. Clone at the owning constructor; do not expose a general-purpose clone package.

- [ ] **Step 4: Run lowering tests for GREEN**

```bash
go test ./workflow -run 'TestCompile.*(Module|Call|Parallel)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the compiler slice**

```bash
git add workflow/compile.go workflow/compile_test.go
git commit -m "feat(workflow): resolve modules into canonical graphs"
```

---

### Task 4: Validate lexical references through one edge relation

**Files:**
- Create: `workflow/bindings.go`
- Create: `workflow/bindings_test.go`
- Modify: `workflow/compile.go`

**Interfaces:**
- Consumes: graph boundary contracts, immediate child contracts, `value.Contract.Resolve`, `value.CheckAssignable`, and `value.Literal`.
- Produces: canonical `Endpoint`, `Binding`, `Edge`, and `LiteralBinding`.
- Produces: complete lexical and exactly-one-source validation during `Compile`.

- [ ] **Step 1: Add failing lexical-reference tests**

Use the explicit endpoint and binding drafts established in Task 2:

```go
type EndpointKind uint8
const (
	Boundary EndpointKind = iota + 1
	Child
)

type EndpointDraft struct {
	Kind EndpointKind
	Child string
}

type BindingDraft struct {
	From []string
	To   string
}

type EdgeDraft struct {
	From, To EndpointDraft
	Bindings []BindingDraft
}

type LiteralBindingDraft struct {
	Input string
	Value value.Literal
}
```

Use table tests for:

- graph input to immediate child input;
- immediate child output to sibling input;
- immediate child output to graph output;
- graph input directly to graph output;
- child-to-child completion-only edge with zero bindings;
- nested source selection through statically declared object fields;
- unresolved child, input, output, and object field;
- an attempted parent-to-grandchild reference expressed as `child/grandchild`;
- boundary used in an invalid direction;
- type mismatch and an `any` binding marked for runtime validation;
- two edges bound to the same child input;
- edge plus literal bound to the same child input;
- a missing required child input;
- an unbound optional child input;
- a missing required graph output; and
- an edge with neither bindings nor two child endpoints.

The failure must include the containing module/graph provenance, endpoint, port, and reason without mentioning a runtime path.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./workflow -run 'TestCompile.*(Binding|Reference|Literal|Completion)' -count=1
```

Expected: FAIL because binding validation is absent.

- [ ] **Step 3: Implement endpoint-local resolution**

In `workflow/bindings.go`, keep resolution private to one graph:

```go
type resolvedEndpoint struct {
	kind EndpointKind
	child int
	inputs, outputs value.Contract
}

func resolveEndpoint(graph Graph, endpoint EndpointDraft) (resolvedEndpoint, error)
func resolveSource(endpoint resolvedEndpoint, path []string) (value.Type, error)
func resolveTarget(endpoint resolvedEndpoint, port string) (value.Type, error)
```

Rules are directional:

- a boundary source reads graph inputs;
- a child source reads that immediate child's outputs;
- a child target writes that immediate child's inputs;
- a boundary target writes graph outputs;
- a completion-only edge is valid only from one child to another distinct child; and
- a name containing `/` is an opaque immediate child name: it resolves only by an exact sibling-name match and is never parsed as a descendant path.

For every binding, call `value.CheckAssignable`. Store its `RuntimeValidation` bit in the canonical binding; do not introduce a transform node or a runtime expression.

- [ ] **Step 4: Enforce one source for every required target**

During graph compilation, build one table keyed by `(target endpoint, top-level input port)`. Register edge bindings and literals through the same function:

```go
type targetKey struct { endpoint Endpoint; port string }

func bindTarget(bound map[targetKey]struct{}, key targetKey) error {
	if _, exists := bound[key]; exists {
		return fmt.Errorf("input %s is bound more than once", key.port)
	}
	bound[key] = struct{}{}
	return nil
}
```

Validate each literal against the complete target port type before recording it. After processing all sources, require every non-optional child input and non-optional graph output to be present. Optional targets may be absent but never multiply bound.

Keep literals on their target node in the immutable model. They do not create endpoints, edges, dependencies, or runtime instances.

- [ ] **Step 5: Run the binding suite for GREEN**

```bash
go test ./workflow -run 'TestCompile.*(Binding|Reference|Literal|Completion)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the binding slice**

```bash
git add workflow/compile.go workflow/bindings.go workflow/bindings_test.go
git commit -m "feat(workflow): validate one lexical edge model"
```

---

### Task 5: Reject cycles and malformed structured scopes

**Files:**
- Create: `workflow/validate.go`
- Create: `workflow/validate_test.go`
- Modify: `workflow/compile.go`

**Interfaces:**
- Consumes: fully resolved canonical graphs from Tasks 3–4.
- Produces: total static validation for fixed-graph acyclicity, `branch`, `map`, `loop`, `gate`, and `finally` shapes.

- [ ] **Step 1: Add failing fixed-graph cycle tests**

Cover a two-node data cycle, a mixed data/completion cycle, a self-edge, and a cycle nested inside a called module. Also prove that a bounded loop body does not become a graph cycle merely because the runtime will instantiate it repeatedly.

```go
func TestCompileRejectsMixedDependencyCycle(t *testing.T) {
	_, err := Compile(mixedCycleFixture(t))
	if err == nil || !strings.Contains(err.Error(), "a -> b -> c -> a") {
		t.Fatalf("Compile() error = %v", err)
	}
}
```

The reported cycle starts at the lexically smallest child in the cycle and follows lexically sorted neighbors, so diagnostics do not depend on source slice order.

- [ ] **Step 2: Add failing structured-scope shape tests**

Use exact approved structural rules:

- boolean branch cases are exactly `true` and `false`;
- enum branch cases are exactly the enum members, with no default or fallthrough;
- every case input contract equals the branch input contract and every case output contract equals the branch output contract;
- map has exactly one collection input, that port is a list, its body inputs are required `item` of the list element and required `index` integer, and its named result port is `list<body-output-object>`;
- loop maximum is a positive literal integer, body inputs are required `initial` object, optional `previous` body-output object, and required `iteration` integer, the termination path resolves to boolean in the body output, and loop outputs equal body outputs;
- gate inputs are required boolean `passed` and optional string `reason`, with no other inputs and an explicit empty output contract;
- a cleanup graph has an explicit empty output contract and contains no gate, loop, or nested finally at any depth; and
- every ordinary graph scope's inner input/output contracts equal its node boundary contracts.

Do not add a default branch, map concurrency, loop retry behavior, gate model/judges/quorum, finally catch behavior, or cleanup output.

- [ ] **Step 3: Run the validation tests and verify RED**

```bash
go test ./workflow -run 'TestCompileRejects.*(Cycle|Branch|Map|Loop|Gate|Finally)' -count=1
```

Expected: FAIL because structural validation is absent.

- [ ] **Step 4: Implement one recursive validator**

In `workflow/validate.go`, keep one dispatch over closed kinds:

```go
func validateGraph(graph Graph, cleanup bool) error {
	if err := validateAcyclic(graph); err != nil { return err }
	for _, node := range graph.nodes {
		if err := validateNode(node, cleanup); err != nil { return err }
	}
	if graph.cleanup != nil {
		if cleanup { return fmt.Errorf("finally cannot contain finally") }
		if err := validateGraph(graph.cleanup.graph, true); err != nil { return err }
	}
	return nil
}
```

`validateAcyclic` derives one adjacency set from child-to-child edges regardless of whether they carry bindings, then uses deterministic DFS colors. It does not build a second `depends_on` structure.

Structured checks inspect `value.Type` and `value.Contract` through read-only methods. They do not know YAML, execute a child, resolve an adapter, allocate runtime identities, or assign persistence keys.

Call `validateGraph(root, false)` inside `Compile` after recursive lowering and binding resolution and before returning the definition. A failed graph never becomes a partially accepted `Definition`.

- [ ] **Step 5: Run the validation suite for GREEN**

```bash
go test ./workflow -run 'TestCompileRejects.*(Cycle|Branch|Map|Loop|Gate|Finally)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the validation slice**

```bash
git add workflow/compile.go workflow/validate.go workflow/validate_test.go
git commit -m "feat(workflow): validate closed structured definitions"
```

---

### Task 6: Make compiled definitions deterministic and observably immutable

**Files:**
- Create: `workflow/canonical.go`
- Create: `workflow/canonical_test.go`
- Modify: `workflow/compile.go`
- Modify: `workflow/model.go`

**Interfaces:**
- Consumes: a fully validated compiled definition.
- Produces: `Definition.Canonical() []byte` and deterministic read-only inspection.
- Produces: one canonical encoding owned by `workflow`; no caller serializes private IR fields itself.

- [ ] **Step 1: Add failing canonicalization properties**

Build semantically identical drafts with:

- module slices reversed;
- sibling node slices reversed;
- edge slices reversed;
- contract fields reversed before construction; and
- enum/media declarations reversed.

Assert byte equality. Then change one edge, literal, contract, leaf kind, branch case, loop maximum, or cleanup graph and assert byte inequality.

```go
func TestCanonicalDefinitionIgnoresSourceOrder(t *testing.T) {
	a := mustCompile(t, canonicalFixture(t, forwardOrder))
	b := mustCompile(t, canonicalFixture(t, reverseOrder))
	if !bytes.Equal(a.Canonical(), b.Canonical()) {
		t.Fatalf("canonical definitions differ:\n%s\n%s", a.Canonical(), b.Canonical())
	}
}
```

Add a mutation test that changes every source slice and every slice returned by an accessor after compilation; `Canonical()` and all later reads must remain byte-identical.

- [ ] **Step 2: Run the canonical tests and verify RED**

```bash
go test ./workflow -run 'TestCanonical|TestDefinitionImmutable' -count=1
```

Expected: FAIL until compilation sorts semantic collections and owns encoding.

- [ ] **Step 3: Encode through private wire records**

In `workflow/canonical.go`, define unexported encoding-only records. Never add JSON tags to the public immutable model or expose the wire shape as an API:

```go
const definitionFormat = "dawn.workflow/1"

type definitionWire struct {
	Format string    `json:"format"`
	Root   graphWire `json:"root"`
}
```

Convert private model data to wire records, sorting nodes by name, edges by their full source/target/binding tuple, bindings by target then source path, and branch cases by case name. Preserve list order where it is semantic: source field paths, literal list values, and any later runtime collection input.

`Definition.Canonical` returns a defensive copy of the bytes produced after validation. Invalid or partial definitions never receive bytes. This encoding is the captured canonical definition format; #8 may derive work fingerprints from semantic subrecords without exposing this wire format to the scheduler.

Modify `Compile` only in this task to encode the already validated definition:

```go
def := Definition{root: root}
canonical, err := encodeDefinition(def)
if err != nil { return Definition{}, err }
def.canonical = canonical
return def, nil
```

- [ ] **Step 4: Run canonical and complete package tests for GREEN**

```bash
go test ./value ./workflow -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the canonical definition slice**

```bash
git add workflow/canonical.go workflow/canonical_test.go workflow/compile.go workflow/model.go
git commit -m "feat(workflow): canonicalize immutable definitions"
```

---

### Task 7: Prove the Prestige-shaped structure and the clean boundary

**Files:**
- Create: `workflow/conformance_test.go`
- Modify: `docs/superpowers/specs/2026-08-09-canonical-workflow-semantic-model-design.md`

**Interfaces:**
- Consumes: completed `value` contract kernel and `workflow.Compile`.
- Produces: one syntax-independent structural tracer bullet and implementation evidence.

- [ ] **Step 1: Add a failing Prestige-shaped compilation fixture**

Build a programmatic draft with:

1. structured and file-typed root inputs;
2. a called reconnaissance module;
3. an explicit parallel scope containing two independent `agent` leaves;
4. a `script` aggregation leaf;
5. an exhaustive optional-work branch;
6. a bounded revision loop whose body contains worker and judge leaves;
7. a separate deterministic gate; and
8. unconditional cleanup attached through `finally`.

Assert the compiled definition contains only the four canonical leaf kinds and five canonical scope kinds, `call` and `parallel` appear only as provenance, the reviewers have no edge between them, all external visibility goes through declared graph outputs, and no reference or record contains a runtime path.

```go
func TestPrestigeShapeCompilesIntoClosedHierarchy(t *testing.T) {
	def, err := Compile(prestigeDraft(t))
	if err != nil { t.Fatal(err) }
	assertClosedKinds(t, def.Root())
	assertLoweredOrigin(t, def.Root(), "recon", OriginCall)
	assertLoweredOrigin(t, def.Root(), "review", OriginParallel)
	assertNoDependency(t, mustGraph(t, def.Root(), "review"), "static", "dynamic")
	assertEncapsulated(t, def.Root())
}
```

- [ ] **Step 2: Add negative product-invariant fixtures**

Prove compilation rejects attempts to encode:

- a plugin-defined leaf or scope kind;
- two source drafts with opposite node order compiling to the same edge-free canonical graph, proving source order cannot express sequencing;
- a parent reference to a nested reviewer;
- an arbitrary fixed-graph cycle;
- a non-exhaustive branch;
- a runtime-selected module name;
- a fake literal-producing node;
- gate-owned model/judge/quorum configuration; and
- per-parallel concurrency/failure/merge settings, represented by the absence of such fields from `ParallelDraft`.

Do not create reflection-based blacklists or production enumeration APIs solely to test absent syntax. Positive model-shape tests and canonical-equivalence tests prove the actual surface; compiler rejection tests cover values that can enter the existing drafts.

- [ ] **Step 3: Run the tracer and verify RED, then fix only integration mismatches**

```bash
go test ./workflow -run 'TestPrestigeShape|TestProductInvariant' -count=1
```

Expected before integration fixes: at least one cross-component mismatch. Fix it in the owner—contracts in `value`, graph meaning in `workflow`—without adding an adapter, scheduler, parser, or compatibility surface.

- [ ] **Step 4: Run the complete verification matrix**

```bash
go test ./value ./workflow -count=1
go test ./... -count=1
go vet ./...
git diff --check
```

Then verify the new inward packages do not depend on the legacy runtime:

```bash
if rg -n 'github\.com/valbaudo/dawn/(plan|gate|backend|proc)|dawn\.(Backend|Invocation|Result)' value workflow; then exit 1; fi
```

Expected: every command exits zero and the dependency search produces no matches.

- [ ] **Step 5: Record honest implementation evidence**

Append `## Implementation Evidence` to the #5 design spec with:

- commit IDs for the contract kernel, immutable model, lowering, bindings, structural validation, canonical encoding, and tracer;
- the exact verification commands above; and
- one explicit boundary statement: this plan proves compile-time structure, while #6 implements runtime values/workspaces, #7 implements recursive scheduling and propagation, #8 implements durable execution, #9 implements adapters, and #10 implements native scripts.

Do not mark runtime scheduling, commit, replay, adapter, or script conformance as implemented by structural tests.

- [ ] **Step 6: Commit the tracer and evidence**

```bash
git add workflow/conformance_test.go docs/superpowers/specs/2026-08-09-canonical-workflow-semantic-model-design.md
git commit -m "test(workflow): prove the canonical semantic structure"
```

## Final Verification

```bash
go test ./value ./workflow -count=1
go test -race ./value ./workflow -count=1
go test ./... -count=1
go vet ./...
git diff --check
```

Verify the closed algebra and the absence of duplicate mechanisms:

```bash
if rg -n 'depends_on|continue_on_error|retry_count|attempts|quorum|merge_policy|concurrency:' value workflow; then exit 1; fi
if rg -n 'github\.com/valbaudo/dawn/(plan|gate|backend|proc)|dawn\.(Backend|Invocation|Result)' value workflow; then exit 1; fi
```

Expected: every command exits zero and both searches produce no matches.

## Plan Self-Review

- **Spec coverage:** Tasks 2–3 establish the closed leaf/scope algebra, hierarchical modules, definitions/instances separation, and `parallel`/`call` lowering. Task 4 implements the sole dependency relation, lexical/local references, direct literals, boundary encapsulation, exactly-one-source rules, and typed bindings. Task 5 rejects fixed cycles and fixes the compile-time shape of structured scopes. Task 6 makes the definition immutable and deterministic. Task 7 proves the general Prestige shape without provider or document-specific nodes.
- **Approved cross-ticket use:** Task 1 implements only the inward contract algebra and compile-time ordinary literals required to make #5's typed graph real. It does not implement #6 storage, binary values, trees, workspaces, manifests, capture, media ingestion, or secrets. Those remain one later deep `value` extension rather than a second model.
- **Placeholder scan:** Every task names concrete files, interfaces, test cases, commands, expected failures, implementation behavior, and a commit boundary. There are no unspecified types, generic “handle errors” steps, compatibility placeholders, or deferred code markers.
- **Type consistency:** `value.Contract` flows from drafts through canonical graph boundaries. `EndpointDraft` and `BindingDraft` lower once to immutable `Endpoint` and `Binding`. `ProgramDraft` is the only input to `Compile`; `Definition` is the only successful output. Draft-only `CallDraft` and `ParallelDraft` never acquire canonical kinds.
- **Complexity audit:** Two packages earn their boundaries. `value` hides recursive type and literal rules; `workflow` hides all hierarchical compilation knowledge. There is no parser/compiler/validator class chain, per-kind package family, registry, visitor framework, event bus, policy object, scheduler abstraction, or generalized plugin surface.
- **Software-design-philosophy score: 10/10.** Each module has one sentence; interfaces are smaller than implementations; implementation formats stay private; interface comments state semantic promises; the review checks complexity; each module hides one load-bearing body of knowledge; newcomers can read the package boundary without implementation; and the plan invests in deterministic invariants before runtime code depends on them.
