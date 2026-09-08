# Tally and Prestige: workflow capability review

**Date:** 2026-08-08

**Scope:** Tally at `de67a26`; Prestige pipeline in `/Users/vabbb/Downloads/prestige-adyen-magento2-export/pipeline`; AWF source in `/Users/vabbb/Documents/GitHub/AgentWorkflowFormat`

**Method:** source inspection, pipeline tracing, `go test ./...`, `go vet ./...`, and validation of the root Prestige workflow with the current AWF source. I did **not** execute Prestige: doing so would launch paid agents and mutate repositories and external services.

## Executive answer

Tally is not missing a bag of agent-specific features. It is missing a small workflow algebra around an unusually good existing nucleus.

That nucleus should stay: strict plans, typed references, content-addressed state, a sole durable-state writer, independent judges, fail-closed gates, and rerun-as-resume. The expensive design decision is to add composition **around** `Invocation → Backend → Result`, not to inflate that interface.

To run Prestige—and to become a general native workflow runner—Tally needs these capabilities:

1. **Workflow contracts and real values:** top-level input/output contracts; JSON objects, arrays, numbers, and booleans; named file artifacts; immutable assets.
2. **Native script steps:** deterministic host processes with structured input/output and captured files.
3. **Sub-workflows:** imported, typed `call` boundaries with private internals and digest-pinned definitions.
4. **Runtime-sized composition:** bounded conditions plus keyed `map`/`reduce`; static parallelism should continue to fall out of data dependencies.
5. **General gates:** generate and evaluate subgraphs, deterministic evaluators as well as LLM judges, typed pass conditions, automatic feedback, bounded repair.
6. **Failure and effect policy:** retry distinct from repair, configurable timeouts, typed rejection, `try`/`catch`/`finally`, cache policy, and idempotency for external effects.
7. **Agent adapters and roles:** Claude, Droid, Codex, HTTP/direct-LLM adapters; reusable role profiles; opaque adapter-owned configuration.
8. **Skill libraries:** content-addressed corpora and a router interface, with deterministic BM25 as the first built-in router.
9. **Operational truth:** hierarchical node paths, durable map/gate decisions, concurrency pools, token/time accounting, and explicit environment-name allowlists.

Prestige does **not** require Docker in Tally. Docker, Podman, PDF tools, browsers, document converters, and similar programs are native process dependencies that a script or agent adapter may call. Tally must stage inputs, capture outputs, cancel the process tree, and report the outcome; it should not own those tools.

The product's one sentence should be:

> **Tally turns a typed workflow into accepted, resumable artifacts, regardless of whether a native agent or script produced them.**

## What Tally already gets right

Tally's current constraints are intentional, not accidental:

- `tally.Backend` is a deep two-method seam for one agent call. Its comments explicitly keep orchestration out of `Invocation` (`tally.go:42-55, 106-114`).
- The runner is a strict static DAG, with dependencies carried by data references rather than a second `needs` graph (`plan/plan.go:30-40`).
- State is committed content-first, pointer-second, so a completed journal entry cannot point at missing bytes (`plan/run.go:291-312`).
- Rerunning is resuming; there is no special recovery path to rot (`plan/run.go:72-77`).
- Workers execute expensive calls, while one coordinator goroutine owns durable mutations (`plan/run.go:86-103`).
- Gates distinguish mechanical failure from a quality rejection, preserve judge independence, and commit only the accepted attempt (`plan/run.go:524-610`).
- Backends are resolved before tokens are spent, and schema validation remains local even when a provider offers structured output (`plan/run.go:355-369, 499-516`).

These are the difficult invariants. The next version should generalize them, not replace them.

## Where Tally stands today

| Capability | Tally today | Consequence for Prestige |
|---|---|---|
| Leaf execution | Agent invocation only | Cannot clone, route, merge, score, publish, or generate reports with deterministic scripts |
| Agent adapters | `claude` and `claude-ws` in the CLI factory | Cannot express Claude Code/Droid/Codex roles and adapter-specific controls |
| Values | String or string enum only | Cannot carry task arrays, finding objects, boolean verdicts, counts, scores, or CVSS structures |
| Files | A single magical field name, `workspace`, represents a tree | Cannot name and independently pass reports, JSONL, patches, PDFs, images, or bundles |
| Workflow boundary | One `steps:` map; no public contract | Cannot import the five Prestige phases as typed black boxes |
| Composition | Fixed DAG; unknown `if`, `loop`, and `map` keys fail | Cannot branch on findings or fan out over runtime-produced tasks/findings/groups |
| Concurrency | Independent static steps run under `--jobs` | Already sufficient for author-known parallel branches; insufficient for runtime-sized worklists |
| Gate | One generator step plus homogeneous boolean LLM jury; fixed three repairs | Strong foundation, but cannot run a generator/evaluator subgraph or a deterministic aggregate after heterogeneous judges |
| Retry/timeout | Fixed policy; one 30-minute invocation bound and three repair attempts | Cannot tune provider retry, wall/idle liveness, or long setup operations |
| Resume | Identity-keyed accepted results and content-addressed trees | Strong foundation; dynamic addresses and external effects still need defined replay semantics |
| Skills | None | Cannot route a small relevant subset of 76 Prestige skills |
| Environment | Ambient backend/process setup | No declarative secret-name allowlist or stable plan identity for required environment names |
| Cleanup/errors | Fail-stop DAG | Cannot guarantee teardown or deliberately absorb a rejected remediation gate |
| Observability | Status, call count, token journal, `show` | No hierarchical view of calls, map items, gate attempts, votes, timings, or effectful scripts |

The current language really is narrow: `Plan` contains only `steps`; a step contains `agent`, `prompt`, `inputs`, `outputs`, `expect`, and `gate` (`plan/plan.go:23-41`). Output types explicitly exclude numbers, booleans, arrays, and nested records (`plan/plan.go:64-100`). The current artifact distinction is inferred from the **field name** `workspace`, not from the reference kind (`plan/run.go:323-350`). That convention is the first boundary that must be retired.

## What Prestige actually does

The root workflow imports five phase workflows and uses a `try/finally` envelope. It validates a five-field input object, clones and reconnoiters the target, runs static analysis and dynamic setup concurrently, joins them for live validation, runs scoring and remediation concurrently, and writes the final report. The source makes the native posture explicit (`prestige.yaml:1-12`) and uses ordinary scripts for repository, Podman/Docker, browser, GitHub, and reporting work.

### End-to-end chain

| Phase | What it does | Semantics Tally must supply |
|---|---|---|
| Root setup | Clone/update repo; emit typed reconnaissance | Native `run`, typed output, external-effect/cache policy |
| Static analysis | Analyze architecture; index skills; plan 15–25 hunts; map up to 7 at once; tolerate partial failure; BM25-route 3 skills; prune to 12; reduce; deduplicate | Arrays/objects, map, stable item identity, concurrency, `min_success`, optional frontier pruning, reduce, skills/assets, agent adapter |
| Dynamic setup | Three generate/verify repair gates configure the app, enable payment methods, and prove flows; writes a runbook and setup context | General gate, deterministic/agent evaluators, bounded repair, live external resource semantics, named artifacts |
| Validation | Route deterministic vs AI findings; run deterministic validators; map AI findings serially; per finding, generate evidence, run three heterogeneous reviewers, deterministically aggregate 2-of-3, repair once; merge records | Conditions, nested map/gate, fixed panel fan-out, scripts inside evaluation, typed verdicts, accepted-attempt artifact forwarding |
| Scoring | Map confirmed findings; use a CVSS skill for metric selection; calculate CVSS v4 in a script; reduce | Condition, map/reduce, skills, script output contract |
| Remediation | Group findings; serial map; isolated worktree patch generation; three-reviewer gate; catch rejection; publish PR or local artifacts; reduce and backfill | Map, typed rejection, selective catch, general gate, effects/idempotency, named artifacts |
| Finalization | Join score/remediation files into a report; always run teardown/run report | Sub-workflow exports, named file inputs, `finally`, output files |

The active core is 2,977 lines across six YAML files. It draws on 59 hunting skills, 13 validation skills, 3 remediation skills, and 1 CVSS skill. This is why passing whole libraries to every agent would be wasteful and noisy.

### Verification against AWF

The current AWF source validates the root Prestige workflow and all imports to digest:

`awf-d1:sha256:2aacaa8ea05d4144102b7d803306b092b8a1ca4f37326198770d63162d1c0d0d`

It emits 11 warnings, not errors:

- 9 `AWF3002` warnings for agent output schemas that are captured or consumed indirectly rather than referenced as typed fields.
- 2 `AWF3013` warnings in `validate.awf.yaml` for unquoted string substitutions into shell source, a CWE-78 risk.

Those warnings matter to Tally's design. General text templating is not a harmless convenience; it creates a second, injection-prone data path. Tally should pass structured values to scripts through a JSON input file and pass large/untrusted content through staged artifacts.

## AWF answers to the explicit questions

### Does AWF have BM25 and skill libraries?

Yes. A top-level `skills` corpus points at a snapshotted directory asset and selects `router: bm25` (`man/awf-workflow.5.md:39-45, 140-154`). AWF's current router weights `SKILL.md` body tokens 4×, path tokens 2×, and nested text 1×; it uses `k1=1.2`, `b=0.75`, deterministic tie-breaking, and journals `skills.selected` before dispatch (`man/awf-workflow.5.md:896-932`).

Tally should copy the **separation**, not the incidental container staging:

- language: “this step wants skills from this corpus, queried by this typed value”;
- library: BM25 implementation;
- runtime: snapshot corpus, record selection, stage selected directories into the native step workdir;
- adapter: tell the agent where/read how the selected skills were delivered.

BM25 is a built-in router, not a core language primitive. A router interface earns its existence immediately because lexical BM25 is one implementation and semantic retrieval/reranking is a plausible second.

### More agents?

Two separate things are needed:

1. **More adapters:** Claude Code, Factory Droid, Codex, direct HTTP/LLM, and test fakes. This is runtime work behind an existing good seam.
2. **Reusable agent roles:** named, digest-pinned adapter configurations for repeated model, effort, tool, and system-prompt settings.

The core must not learn every harness option. AWF's `with:` is opaque to the adapter (`man/awf-workflow.5.md:728-776`); Tally should use the same information-hiding boundary. Keep `agent: claude/sonnet` for the simple case, and allow `agent: reviewer` to resolve a declared role when repetition earns the indirection.

### Input/output contracts, scripts, gates, and LLM judges?

- **Contracts:** Tally has per-step schemas internally, but the author-facing type system is string-only and there is no workflow contract. This is partial, not absent.
- **Scripts:** absent and essential.
- **Gates:** present and unusually sound, but narrow. The current independent LLM jury should remain as shorthand.
- **LLM judges:** present. What is missing is a general evaluator subgraph, heterogeneous reviewers, and deterministic aggregation.
- **Sub-workflows:** absent and essential.

## The target language: five semantic ideas

The language should grow to five ideas, not mirror every AWF key.

### 1. Contract

A workflow and every executable leaf accept typed values plus named artifacts and return typed values plus named artifacts.

- Use JSON Schema 2020-12 rather than inventing a Tally type language.
- Preserve today's `string` and enum syntax as shorthand compiled to JSON Schema.
- Allow an external schema file to keep plans readable.
- Make artifact kind/media explicit; remove the `workspace` field-name heuristic.
- Treat a workspace as a directory-tree artifact, not the artifact channel itself.

Conceptually:

```yaml
inputs:
  schema: ./schemas/prestige-input.schema.json
outputs:
  schema: ./schemas/prestige-output.schema.json
  files:
    report: final_report.report
```

### 2. Leaf

There are two leaf executors:

- `agent:` invokes an agent backend.
- `run:` invokes a native process.

Both receive the same orchestration-owned task contract and produce the same result envelope. `tally.Backend` should **not** become a script runner; add a sibling process-runner seam and adapt both into a workflow-level leaf dispatcher.

For scripts, Tally should provide stable files such as:

- `TALLY_INPUT` — path to canonical JSON containing typed inputs;
- `TALLY_OUTPUT` — path where the process writes schema-validated JSON;
- `TALLY_FILES` — directory containing named input artifacts and expected output locations;
- `TALLY_IDEMPOTENCY_KEY` — stable key for a declared external effect.

No agent-authored string is interpolated into shell source. The plan's `run:` is trusted code; runtime values are data.

### 3. Composition

- `call` runs an imported workflow behind its public contract.
- `when` conditionally runs a node using a bounded typed expression.
- `map` fans out over a runtime array, with a stable per-item key, a concurrency bound, a success threshold, and optional `reduce`.
- `try` scopes typed failure handling and `finally` cleanup.

Do **not** add an authored `parallel` primitive initially. Tally already launches independent nodes concurrently. Root Prestige's two parallel pairs become ordinary steps whose references express the join. One less construct, identical semantics.

A map item must not be addressed only by array index. Use an author-declared `key` such as `finding.id`, falling back to a canonical content hash. That prevents an inserted first item from invalidating or, worse, misaddressing every later cached result.

`prune` can follow after map/reduce. Prestige's output can be reproduced by a reducer that selects the top 12, but matching its cost-saving cancellation behavior requires a durable frontier policy. That is performance parity, not the first semantic milestone.

### 4. Acceptance

Generalize the existing gate from “one agent + boolean jury” to:

```yaml
validate:
  gate:
    attempts: 2
    generate:
      steps: { ... }
    evaluate:
      steps: { ... }
      result: panel_verdict
    until: evaluate.passed
    feedback: evaluate.feedback
```

The evaluator's last typed output supplies the pass condition and repair feedback. Generate and evaluate may each contain scripts and agents. The runtime preserves today's invariants:

- every LLM evaluator starts fresh;
- mechanical retry is not a quality repair;
- only a completed false verdict consumes the repair budget;
- evaluator internals stay private;
- only the accepted generate attempt crosses the gate boundary;
- artifacts are committed transactionally, but external side effects are **not** falsely described as rollbackable.

The current `judges`/`quorum` form should lower to this general machinery as convenience syntax. “LLM judge” does not need another core node type.

### 5. Policy

Policy is metadata on a leaf or scope, not another executor:

- `retry` for identical replay after transient mechanical faults;
- wall timeout, plus idle timeout only when an adapter exposes honest liveness;
- `cache: true|false` so service probes/setup and cleanup do not reuse stale success;
- `effect: external` to require/emit an idempotency key;
- typed catch filters such as `on: [rejected]` so Prestige can absorb a declined patch without swallowing infrastructure failure;
- concurrency pools by adapter/resource class, separate from map fan-out width;
- environment variable **names** in the definition, with values read at run time and never logged or hashed.

## A Prestige-shaped Tally plan

This is illustrative syntax, not a committed format. It shows how the five ideas keep the common path small.

```yaml
inputs:
  schema: ./schemas/prestige-input.schema.json

imports:
  static: ./static.tally.yaml
  setup: ./setup.tally.yaml
  validate: ./validate.tally.yaml
  score: ./score.tally.yaml
  remediate: ./remediate.tally.yaml

env: [CLAUDE_CODE_OAUTH_TOKEN, FACTORY_API_KEY, GITHUB_TOKEN]

agents:
  analyst:
    uses: anthropic/claude-code
    with: {model: claude-opus-4-8, effort: high}
  adversary:
    uses: factory/droid
    with: {model: glm-5.2, reasoning_effort: high}

steps:
  clone:
    run: ./scripts/clone.sh
    inputs: {repo_url: input.repo_url}
    cache: false
    effect: external
    files: {repo: repo/}

  recon:
    run: ./scripts/recon.sh
    files: {repo: clone.repo}
    outputs: {schema: ./schemas/recon.schema.json}

  static:
    call: static
    inputs: {project: recon.project}
    files: {repo: clone.repo}

  setup:
    call: setup
    inputs: {project: recon.project}
    files: {repo: clone.repo}

  validation:
    when: static.finding_count > 0
    call: validate
    inputs:
      findings: static.findings
      target: setup.target

  scoring:
    when: static.finding_count > 0
    call: score
    files: {findings: validation.validated}

  remediation:
    when: static.finding_count > 0
    call: remediate
    files: {findings: validation.validated, repo: clone.repo}

  final_report:
    when: static.finding_count > 0
    agent: analyst
    prompt: Produce the final security report from the supplied evidence.
    files:
      scored: scoring.scored
      remediation: remediation.results
    produces: {report: report.md}
```

`static` and `setup` run concurrently because neither consumes the other. So do `scoring` and `remediation`. The language describes data; the scheduler discovers parallelism.

## Architecture that keeps Tally deep

```text
YAML source
   │
   ▼
Compiler ── validates contracts, scopes, capabilities, imports
   │         lowers shorthands; assigns canonical hierarchical paths
   ▼
Canonical workflow IR
   │
   ▼
Interpreter ── sole writer of durable state
   ├── coordinator nodes: call / when / map / gate / try
   └── leaf dispatcher
         ├── agent adapter ── existing tally.Backend
         └── process runner ── native command + process-tree cancellation
   │
   ▼
Value + artifact store ── canonical JSON and content-addressed files/trees
```

### Boundaries to preserve

**Keep `tally.Backend` small.** Adapter configuration is validated while constructing the backend; a single invocation remains unaware of node paths, attempts, calls, and journals.

**Create one canonical IR.** Imports should compile into hierarchical addresses and execute in the same interpreter. Do not build a second “sub-workflow engine.” Child internals remain private at the source/contract boundary even though the runtime sees one graph.

**Make values and artifacts first-class.** A result is `{values, artifacts, usage}`. A file, directory tree, PDF, image, patch, or bundle differs by media/kind and materialization—not by a magical output name.

**Keep the interpreter the only state writer.** Map workers, agents, and scripts return candidate results. The coordinator records selection, verdict, and commit decisions in deterministic order.

**Separate definition from run state.** The identity of a node includes its canonical definition, adapter/runtime version, canonical typed inputs, artifact digests, and immutable selected assets. It excludes wall clock, PID, retry count, and provider usage.

**Journal dynamic decisions.** Persist the map worklist snapshot, item keys, skill selection, prune disposition, evaluator verdict, accepted gate attempt, and any external-effect receipt. Resume replays decisions; it does not silently decide again from a different completion order.

## What not to add

The cut list is part of the design:

- **No Docker/DinD subsystem.** Scripts can call Docker or Podman. Tally owns process semantics, not container lifecycle syntax.
- **No PDF/image/document step types.** They are named artifacts processed by scripts or capable agents.
- **No general templating language.** Structured inputs and artifacts cross boundaries; bounded expressions choose control flow.
- **No arbitrary expression evaluator.** References, literals, comparisons, and boolean operators are enough.
- **No explicit static `parallel` node at first.** Data dependencies already express it.
- **No separate LLM-judge engine.** A judge is an agent leaf inside `evaluate`; a jury is sugar over fan-out plus reduction.
- **No global shared mutable workspace as the data model.** A workspace is an artifact. Live services are external effects described by typed endpoints/context and rechecked by uncached steps.
- **No built-in catalog of MCP servers or tool permissions.** Those are adapter-owned configuration.
- **No remote/dynamic imports in the first version.** Local, digest-pinned imports are enough and dramatically easier to reason about.

## Delivery sequence

### Milestone 1 — General leaf contract

1. Add canonical JSON values and first-class named artifacts; migrate `workspace` to a directory-tree artifact while keeping old plans readable.
2. Add workflow input/output contracts and external schema files.
3. Add the native process runner with `TALLY_INPUT`, `TALLY_OUTPUT`, and artifact staging/capture.
4. Add adapter registry, capabilities, opaque config, named roles, and environment-name allowlists.

**Proof:** a native workflow clones a repo, runs a script, asks one agent to analyze it, validates the typed result, and emits a named Markdown/PDF artifact; rerun reuses only correctly cacheable work.

### Milestone 2 — Composition

1. Compile local imports and typed calls into hierarchical IR.
2. Add bounded `when` conditions.
3. Add keyed `map`, concurrency, `min_success`, typed aggregate output, and a script/quorum reducer.
4. Infer static concurrency exactly as Tally does today.

**Proof:** the Prestige static phase plans a runtime task array, routes and runs hunts concurrently, reduces them, and resumes per item without index churn.

### Milestone 3 — Trust and failure

1. Generalize gates to generate/evaluate subgraphs; lower the current jury syntax into it.
2. Add separate retry and repair policy, typed outcome classes, wall timeout, and capability-gated idle timeout.
3. Add `try` with filtered `catch` and unconditional `finally`.
4. Add cache/effect/idempotency semantics and record effect receipts.

**Proof:** one Prestige validation item runs the full three-agent panel plus deterministic aggregate, repairs once, commits only the accepted artifact, and catches only a final `rejected` outcome.

### Milestone 4 — Skills and operation

1. Snapshot assets and skill corpora into the definition digest.
2. Add a router interface and deterministic BM25; journal selection before dispatch.
3. Add hierarchical trace/status/cost views for calls, map items, gate attempts, and judges.
4. Add resource-class pools and optional persistent-session capability after a real second workflow demands it.
5. Add optional durable prune/frontier policy for Prestige cost parity.

**Proof:** the full Prestige definition validates and runs natively with no Tally container management, and a killed run resumes without repeating committed agent work or changing a previous skill/frontier decision.

## Required tests before claiming parity

- Contract round trips for nested JSON, optional branch outputs, and schema failures.
- Artifact traversal/symlink/size limits; executable bit and directory-tree fidelity.
- Script process-tree cancellation on macOS and Linux.
- Import cycle detection, path containment, private child internals, and definition drift.
- Stable map addressing under insertion/reordering; partial success and deterministic reduce ordering.
- Gate tests proving evaluator freshness, feedback isolation, mechanical-failure propagation, and accepted-attempt-only commit.
- Crash points before blob write, between blob and journal pointer, during a map, during jury fan-out, and after an external effect but before receipt commit.
- Secret values absent from definition digests, logs, traces, and outputs.
- Compatibility fixtures for existing Tally plans and `tally show`.
- A reduced local Prestige fixture using fake agents and real scripts; full paid/invasive Prestige remains an explicit integration run.

## Design diagnostics

### Software design philosophy score: 6.3/10

| Diagnostic | Result | Evidence / fix |
|---|---|---|
| Each module can be described in one sentence | Pass | Packages have clear purposes in README and package comments |
| Interfaces are simpler than implementations | Pass | `Backend` and `Blobs` are small, deep seams |
| Implementation can change without caller edits | Partial | Backend/store pass; field-name-based workspace and string-only values leak plan assumptions into binder, preflight, schema, and CLI. Fix with first-class value/artifact contracts |
| Interface comments describe abstraction, not mechanics | Pass | `tally.go` documents responsibilities and exclusions unusually well |
| Design discussion is visibly part of review | Not evidenced | No architecture decision record was found. Record the canonical IR, artifact model, dynamic addressing, and effect semantics before implementation |
| Each module hides an important decision | Partial | Store/gate do; `plan` currently spans syntax, validation, binding, scheduling, caching, and gate orchestration. Split compiler/IR/interpreter without creating thin temporal wrappers |
| A newcomer can understand boundaries without implementation | Pass | README/SPEC are excellent, but only for the intentionally narrow product |
| 10–20% strategic design investment is evidenced | Not evidenced | Use the four milestone proofs and an explicit complexity budget before feature work |

The score is not a criticism of code quality. Tally's current narrow design is coherent. It measures readiness for the broader promise: arbitrary native agent workflows.

# Design Review: Tally as a general agent-workflow runner
**Verdict:** NOT DONE (score 6/10)

**The One Thing:** Turn a typed workflow into accepted, resumable artifacts, independent of the native agent or script that produced them.

**Keeps its promise?** Tally keeps its current, narrow static-agent-DAG promise. It cannot yet keep the broader promise because Prestige needs scripts, contracts, calls, runtime-sized fan-out, general evaluation blocks, effects, and artifacts.

**Cut list:** Docker/DinD orchestration; special PDF/image node types; unrestricted templating; arbitrary expressions; an explicit static-parallel construct; a separate LLM-judge engine; remote imports; global mutable workspace semantics.

**Fix list:** First-class values/artifacts; native script runner; local typed calls; keyed map/reduce and bounded conditions; general gates; retry/effect/finally policy; adapter roles; assets/skills/BM25; hierarchical journal and trace.

**Back of the fence:** The existing explainer is visually strong and makes current refusals legible, but its saved artifact includes a large frame wrapper and the mobile rendering clips/forces wide diagram scrolling. More importantly, the attractive simplicity rests on assumptions—string-only values, `workspace` by field name, agent-only leaves—that become change amplification under the new product promise. The hard work is to replace those assumptions once, behind deep modules, before adding surface syntax.

## Bottom line

Tally should not become “AWF without Docker.” It should become the smaller language that Prestige proves is necessary:

> **typed contracts + native leaves + composition + acceptance + policy**

Scripts make the universe of work open-ended. Artifacts make files first-class. Calls and maps make large workflows composable. Gates make stochastic work trustworthy. The journal makes all of it resumable. Skills and BM25 are then a clean library layered on top—not a special case in the interpreter.

That is enough to run Prestige and broad enough for repository work, data/file processing, browser automation, Docker-driving scripts, PDF generation, document analysis, releases, research, and future agent harnesses without teaching Tally what any of those domains are.

## Source map

- Tally core: `tally.go:1-114`, `plan/plan.go:1-168`, `plan/run.go:18-120, 252-365, 490-610`, `cmd/tally/main.go:265-281`, `SPEC.md:1-110, 182-292, 624-687`.
- AWF format: `/Users/vabbb/Documents/GitHub/AgentWorkflowFormat/man/awf-workflow.5.md:26-177, 646-985, 1164-1670, 1852-2056, 2274-2345`.
- Prestige root: `/Users/vabbb/Downloads/prestige-adyen-magento2-export/pipeline/prestige.yaml:1-454`.
- Prestige phases: `static-analysis.awf.yaml:1-501`, `dynamic-setup.awf.yaml:1-509`, `validate.awf.yaml:1-575`, `score.awf.yaml:1-298`, `remediate.awf.yaml:1-640`.
- Native launcher: `/Users/vabbb/Downloads/prestige-adyen-magento2-export/pipeline/run-native.sh:1-290`.
