# THE GLUE

A substrate for any CyberGym harness, with observability as the spine.

---

## 1. Typing: the answer

**Your instinct is right. Payloads are bytes, forever. But the envelope is typed, and the envelope is not where you were burned.**

The rule: **type what you will later query or branch on. Nothing else. Ever.**

The evidence is not close. Nineteen harnesses; exactly one — Whitzard on QitOS — has a source-verified whole-workflow declared state. GCSA passes markdown between roles and explicitly recomputes rather than trusting a producer contract ("files + round-check are authoritative; model self-claims are not authority"). MDASH at 91.0% passes English prose between scan and validate. Sangfor, the top scorer at 93.17%, discloses no format at all. And mneme runs 3,867 knowledge entries at 91.0% on OKF — a spec whose entire conformance bar is one non-empty `type` string and which *mandates* that consumers "MUST NOT reject" unknown keys. A schema-first substrate could not host the field's best work.

So: no `value/`. No `workflow/`. No load-time contract checking. No parse errors for unknown keys. No compile step. `go build` is untouched.

**The residual typing is three lists of strings, declared once, ten lines:**

```go
led, _ := glue.Open("sweep.db", glue.Vocab{
    Outcomes: []string{"ok", "infra", "policy", "no_crash", "both_crash", "budget"},
    Labels:   []string{"agent", "operator"},
})
```

`Outcomes` because FangcunCyber — a 91.3% harness — cannot explain 128 of its own 131 failures. The team states it plainly: the data "do not support subdividing them into budget-exhaustion, both-crash, or no-trigger categories." One field was free text and the harness went blind. Crystalline's entire 89.6% is valid only if 359 relaunches partition correctly into {api_transport: 321, hardware_dead: 38}; on a free-form string that partition is a grep. Gusion's exit-71 review retroactively removed four passes. This is the field that decides whether a run may be re-scored, and it must be a closed set.

`Labels` because five harnesses require an enforced non-edge: the fixed-side verdict recorded for audit and structurally unreachable from generation. JiuXuan §4.2 documents its own agent *actively probing* the evaluation server for `fix_exit_code`. Crystalline lost 46 tasks to exactly this leak. Filtering-by-remembering is not enforcement.

`Keys` are caller-defined and unvalidated. Addressable, not parsed. That is what turns Sangfor's negative-results dedup and Gusion's fingerprint-distinctness query into queries instead of blob scans.

**Why this cannot hurt you.** Enforcement fires when a span closes — never at load, never at build. An unlisted outcome fails *that one attempt* with `outcome: "infra"` and a reason string, into retry machinery your harness needs anyway. Gusion already runs this shape at 87% ("Core validates and merges the update"; the Solver "cannot directly mutate authoritative Memory"). Redbud gives spec validation three retries. Nothing inspects a payload. `reason` stays free text underneath — it is NSFOCUS's directed-feedback channel and constraining it would be a bug.

Optional per-node payload validators exist as `func([]byte) error` you may hand to a span. Nobody has to use one. MDASH's roadmap asks for exactly that, one boundary at a time.

---

## 2. "Checklist, ledger, workspace, coordinator": right, incomplete, one wrong

- **Ledger — right idea, wrong unit, wrong scope, four fields short.** The unit must be the LLM request, not the attempt: every audit that mattered in this corpus happened *below* the attempt (Crystalline grepping 763 transcripts for 44 fix-binary invocations; Crystalline and Redbud both proving zero web access by counting tool calls; five teams publishing `llm_requests` as a headline). And the scope must be the **sweep**, not the run — every published number in all nineteen writeups is cross-run. Add model id, split `cached` into cache_read and cache_creation, add `supersedes`, and store timestamped tokens instead of a cost scalar.
- **Workspace — right, but it is two things.** Content-addressed store *plus* mutable named refs with compare-and-swap. Content addressing alone gives k divergent roots and no way to name "the current wiki."
- **Coordinator — right but under-scoped and misnamed.** It is not a scheduler. It is sandbox supervision, and it must own image digest + mount spec + network mode, or "unaltered vulnerable image" stays a promise instead of a checklist predicate.
- **Checklist — wrong.** The mandatory operation (CyberGym FAQ Q3) is argmax over K candidates; a boolean cannot choose. Sangfor's review runs on *every* candidate, mostly rejects, and recycles rejection as "a constraint on the next search step." JiuXuan's irreversible act is invoked by the agent itself via `check_candidate.py`, out of any orchestrator's call path. Cut it as a primitive.

**Corrected list: Ledger, Span, Store, Refs, Sandbox, Seam, Memory.** Plus two helpers (Budget, Select) that are functions, not abstractions.

---

## 3. The primitives

**The synthesis that makes this cheap: one SQLite file in WAL mode is the ledger, the refs table, and the memory store.** That single choice kills three separate fatal flaws — multi-process append ordering, CAS, and ranked recall — and it is boring on purpose.

### Ledger — the spine

```go
type Ledger struct{ db *sql.DB }   // one file, WAL, many processes

func (l *Ledger) Root(run, node string) *Span
func (l *Ledger) Query(Filter) iter.Seq[Row]        // node prefix, key, outcome, label, run
func (l *Ledger) Reduce(name string, f Fold) *View  // incrementally maintained
func (l *Ledger) Invalidate(run, cause string) error
func (l *Ledger) Redact(pred func(Row) bool) error
```

Rows carry: `sweep, run, node, attempt, seq, kind, model, in/cache_read/cache_create/out, started, ended, exit, outcome, reason, labels, keys, in_ref, out_ref, env, supersedes`. **No cost column** — RO0T ran 30% of tasks before the 8/17 DeepSeek rise and 70% after at up to 5×, so a scalar frozen at write time is wrong. Price at query time.

WAL gives multi-writer append with monotonic rowid ordering, mid-run reads that don't tear, and indexes for `Query`. Ten concurrent containers (Crystalline), twenty solvers (NSFOCUS), six workers (SageAgent) all append to the same file. `Invalidate` is Xuanwu's 162 infra + 127 policy requeue. `Redact` exists because Redbud found a real gateway key in real sandbox logs and had to destroy it across all affected cases before shipping.

### Span — opening a row *is* starting work

```go
func (s *Span) Open(name string) *Span
func (s *Span) Close(outcome, reason string)     // idempotent
func (s *Span) Charge(Usage)                     // one LLM request or tool call
func (s *Span) Fact(key, val string)             // indexed
```

Node path, attempt index, wall time, parent→child causal edges and live position are **derived**, not typed at call sites. An open span is execution position; there is no second progress mechanism to drift. This is the answer to RO0T's "every dispatch becomes part of an auditable trace connecting the decision, task, and result" — the edge is free.

### Store — dawn ports verbatim

`Put(io.Reader) (Ref, error)`, `Capture(dir, ignore) (Ref, error)`, `Materialize(Ref, dir)`. Content addressing *is* the freeze semantics all nineteen harnesses need for their one designated final PoC.

### Refs — the RO0T/MopMonk/Sangfor fix

```go
func (r *Refs) Get(name string) (Ref, uint64, error)
func (r *Refs) CAS(name string, gen uint64, next Ref) error   // ErrConflict
func (r *Refs) Base(a, b Ref) (Ref, error)                    // common ancestor at the join
```

Three rows in the same SQLite file. The substrate supplies naming, ancestry and conflict detection. **It ships no merge policy**, because Sangfor needs per-field merge preserving observation-vs-assumption, RO0T needs union-with-dedup, MopMonk needs four different rules across seven compartments, and Crystalline needs atomic increment under concurrent promotion. Any rule I ship is wrong for three of them.

### Sandbox — isolation as declared capability

```go
type Spec struct {
    Node       string
    Image      string     // digest, required
    Mounts     []Mount    // {From Ref|Path, At string, RO bool} — deny by default
    Net        NetMode    // None | Allow([]host)
    Read       []string   // ledger labels granted; default: own ancestry
    Budget     *Pool
    Soft, Hard time.Time
}
```

GCSA v2.0's entire delta over v1 is "host isolation is part of the run, not only a prompt rule." Xuanwu *replaced* prompt-plus-post-hoc-review with a proxy allowlist. `Env` on every row makes NSFOCUS's "unaltered vulnerable image" a predicate over ledger metadata. `Soft` emits an event and unlocks a reserve; `Hard` writes the terminal row **then** kills the process group (NSFOCUS's ordering, exactly). Kill-on-exhaustion cost NSFOCUS 27–29 tasks.

### Seam — the in-loop hook, and how it actually works

The panel's sharpest hit: all three drafts declared `Steps()`/`Inject()` and none said what delivers them. The honest answer is **intercept at the OS boundary, not the SDK boundary** — three mechanisms, all uniform:

1. **`glue-exec` on `PATH` inside the sandbox.** Every shell command the agent runs is our shim. That is `Before(*Step) → Allow | Veto(msg) | Replace(out)` for *any* agent that shells out — which is all of them. FangcunCyber's `LOCAL_FUZZ_DENY_PATTERNS` blocking `afl-fuzz|honggfuzz|run_fuzzer` is literally this, hand-built.
2. **An LLM egress proxy.** `ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL` point at us. Per-request model, four token buckets, latency, `server_tool_use` — for any SDK, no parsing. That is mneme's 168,411-response audit for free.
3. **A hook endpoint speaking Claude Code's existing PreToolUse/PostToolUse JSON contract** where the SDK offers it, replying `{"deny":"...","inject":"..."}`.

`Inject(text)` rides the deny-reply on (3) and a file the shim reads on (1). Turn counting falls out of (2). This buys JiuXuan's Observer, Xuanwu's drift hooks, SageAgent's `DoomLoopDetectorPlugin`, XD's 200-turn cap, and Crystalline's fix-binary audit — which Crystalline performed by grepping 763 transcripts by hand.

### Memory — the gap all three drafts refused, ~150 lines

```go
func (m *Memory) Put(key, level, text string) error
func (m *Memory) Recall(q string, k int, level string) []Item  // FTS5 + score
func (m *Memory) Bump(key string) error                        // atomic, on read
func (m *Memory) Forget(pred string) (int, error)
```

Memory sophistication descends exactly with rank across the whole leaderboard: Crystalline 89.6% (5-level store, activation on read, `forget_decayed()`), FangcunCyber 91.3% (tagged "test-time mem."), mneme 91.0% (3,867 OKF entries), then Redbud, VARAS, MopMonk. Crystalline attributes its full +23.0pp over the 66.6% Opus baseline to this layer. Refusing it means six harnesses keep their defining component outside the substrate.

It is cheap **because SQLite is already open**: one table, FTS5 index, `UPDATE ... SET hits = hits + 1`. The substrate ships the store. Activation formulas, Hebbian promotion, consolidation triggers, level taxonomy — all yours. And mounting it read-only through `Spec.Mounts` makes mneme's published claim ("memory never written during trials") *enforced*, not asserted.

### Two helpers, not primitives

```go
func (p *Pool) Sub(name string, cap, reserve Limits) *Pool
func (p *Pool) Remaining() Limits          // a ledger query minus a constant
func (p *Pool) Soft() <-chan struct{}
func (p *Pool) Claim(name string) *Pool    // salvage draws the reserve

func Select(ctx, []Candidate, []Predicate) (Candidate, []Row, error)
```

`Predicate` returns `(score, reason, evidence)`. `Select` writes the **full ranking** — every candidate, every score — to the ledger. That is FAQ Q3's mandatory argmax, Velldepth's three-axis compare, and Sangfor's rejection-as-constraint, in one function. Gusion's protected 1.0M/6.0M comparison reserve and NSFOCUS's post-deadline salvage are `Claim`.

---

## 4. Observability

**Recorded automatically, no instrumentation call:** one row per LLM request (via the proxy — model, four buckets, latency), one per tool call (via the shim — argv, cwd, exit), one per span open/close (node path, attempt, wall time, outcome, reason), one per sandbox start (image digest, mount modes, network mode). Heartbeats on open spans, so NSFOCUS's hung-solver case is visible instead of indistinguishable from quiet work.

**Queryable mid-run, each one call:** stale-retry (`Query{Keys:...}` — Gusion's fingerprint distinctness, Sangfor's refutation dedup); remaining budget by prefix; Gusion's 1.2×-pre-crash allowance; Redbud's 25-round stagnation counter (`Reduce`, incrementally maintained — Gusion re-projects on up to 1,590 requests per task, so a naive refold is O(n²) on exactly the long-tail tasks that decide the score); cache-read ratio against a node's trailing baseline, alarming *during* the run rather than after (MopMonk runs 97.91% cache-read; a projection that reorders costs 50×).

**Live view — `glue top`:** the open-span tree. Node paths, current attempt, elapsed, running tokens, spend against reserve, last `reason`. Not a spinner — actual execution position, because the open-span set already exists.

**Free deliverable — `glue export --submission`:** the SUBMISSION.md YAML as one fold. Every team on that leaderboard hand-built this table. Gusion shipped two mutually inconsistent versions and publicly retracted one. Whitzard could not measure its cache split and had to *model* it at "approximately 97% of prompt tokens." Crystalline, at 89.6%, declined to report cost at all and listed it under Limitations. None got credit.

---

## 5. The proof

### NSFOCUS (95.02%) — mechanical gate + salvage

```go
run := led.Root("nsfocus", task.ID)
pool := budget.New(run, Limits{Wall: 270 * time.Minute}, Reserve{"salvage", 400_000})

for !done {
    s := run.Open("solve")
    sess, _ := sandbox.Start(ctx, sandbox.Spec{Node: s.Path(), Image: task.VulDigest,
        Net: sandbox.None, Read: []string{"agent"}, Budget: pool, Soft: pool.SoftAt()})
    go drain(s, sess.Steps())
    poc, _ := sess.Wait(); s.Close("ok", "")

    g := run.Open("gate")                        // five-dimensional record
    v, _ := Select(ctx, []Candidate{poc}, []Predicate{
        targetFile, crashType, detector, mechanismViaGDB, inputFormat})
    g.Close(v.Outcome, v.Reason)                 // rejection reason → next attempt
    if v.Pass { submit(freeze(store, poc)); break }

    select {
    case <-pool.Soft():
        sal := pool.Claim("salvage")             // budget still exists after the deadline
        best, ranking, _ := Select(ctx, allCandidates(run), rank...)
        run.Open("salvage").Fact("ranking", refOf(ranking)).Close("budget", "exhausted")
        submit(best)                             // the audit flag is the outcome enum
    default:
    }
}
```

The gate mixes subprocess predicates with a model predicate; the substrate does not care which. Pre-submit runs in a *fresh* sandbox at `task.VulDigest`, so "unaltered image" is `Query{Node:"gate"}.Env == task.VulDigest` — a check, not a promise.

### RO0T (94.2%) — fan-out, advisory DAG, shared wiki

```go
for round := 0; !done; round++ {
    wiki, gen, _ := refs.Get("wiki")
    plan := commander(run.Open("cmd"), advisoryDAG, project(store, wiki))  // DAG is DATA

    succ := make([]Ref, len(plan.Tasks))
    var wg sync.WaitGroup
    for i, t := range plan.Tasks {
        if seen(led, "refuted:"+t.Claim) { continue }        // dedup is a query
        wg.Add(1)
        go func(i int, t Task) {
            defer wg.Done()
            w := run.Open(fmt.Sprintf("r%d/worker/%s", round, t.Kind))  // model-named leaf
            sess, _ := sandbox.Start(ctx, sandbox.Spec{Node: w.Path(), Image: task.VulDigest,
                Mounts: []Mount{{From: wiki, At: "/wiki", RO: true}}, Net: sandbox.None,
                Hard: time.Now().Add(10 * time.Minute)})
            succ[i], _ = sess.Wait()
            w.Fact("refuted:"+t.Claim, "1"); w.Close(classify(sess), detail(sess))
        }(i, t)
    }
    wg.Wait()

    base, _ := refs.Base(succ[0], succ[1])
    refs.CAS("wiki", gen, mergeWiki(base, succ))   // merge is yours; CAS is mine
}
```

An unmodeled action is `run.Open(modelChosenName)` under a stable prefix — groupable after the fact, no pre-registration. The decision→task→result edge is the span parent plus `In: wiki`. `mergeWiki` is ~40 lines of your policy.

### Crystalline (89.6%) — memory-centric, 10 concurrent containers

```go
mem, _ := glue.Memory(led)                        // same SQLite file

go func() {                                        // consolidator: lifetime = the sweep
    for range mem.Writes(20) {                     // global counter, not any node's
        c := sw.Root("consolidate", "run")
        eps := mem.Recall("recent", 50, "episodic")
        for _, p := range promote(ctx, c, eps) { mem.Put(p.Key, p.Level, p.Text) }
        mem.Forget("hits < 2 AND age > '7d'")
        c.Close("ok", "")
    }
}()

for _, task := range tasks {                       // 10 workers, same store
    run := sw.Root("crystalline", task.ID)
    hits := mem.Recall(task.Desc, 5, "principle")  // Bump() fires on read
    sess, _ := sandbox.Start(ctx, sandbox.Spec{Node: run.Path(), Image: task.Digest,
        Mounts: []Mount{{From: memPath, At: "/mem", RO: false}},
        Net: sandbox.Allow([]string{"api.anthropic.com"}),
        Budget: pool.Sub("task", Limits{USD: 50}, Reserve{})})
    ...
}
```

The consolidation trigger is a global write counter with a sweep-length lifetime — it belongs to no run's node graph, which is why the ledger is sweep-scoped. Crystalline's fix-binary audit, which cost it 46 reruns and 0.6 points, is `Query{Kind: Tool, ArgvLike: "./fix"}` — one query instead of grepping 763 transcripts.

---

## 6. Refusals

**Merge policy · projection and ranking · retry policy · memory semantics · workflow definition · payload schemas · replay/resume.**

The refusal rule is one test: **if the nineteen harnesses disagree on the semantics, it is user code.** Four harnesses need four incompatible merge rules. Gusion sizes top-k against remaining budget; NSFOCUS does static top-2 over a fixed library of seven skills. Crystalline's memory mutates on read; mneme's must be provably immutable. There is no policy to ship, only a mechanism.

And replay: dawn built the scheduler twice — 2,530 lines whose central `LeafRunner` port has four test implementations and zero real ones — and the last five commits on master polish it. That is what happens when the framework owns control flow. RO0T's commander "may choose an unmodeled action when the graph does not fit." No config language can contain an action it does not contain. `for` loops.

---

## 7. Dawn: start clean, port five files

**Port verbatim — 869 lines, one afternoon:**

- `store/tree.go` (505). The only genuinely expensive file in the repo. Three modes because the exec bit is the only permission that round-trips; `strconv.Quote` on paths; ignores apply to new paths only; re-validation on materialize. Four audit rounds of format decisions you cannot re-derive by reasoning.
- `store/store.go` + `store/fs.go` (211). Temp+Sync+rename, read-time hash verify.
- `proc/proc.go` (71). `setpgid`, `Kill(-pid)`, `WaitDelay`. **Keep the comment; it is worth more than the code.** Note `TestInvokeTimesOut` failing at 5.303s under load — the group kill lost and the fallback caught it. Keep the fallback.
- `plan/lock.go` (58), `platform.go` (24).

**Delete 13,600 lines** — all of `value/`, `workflow/`, `content/`, `workspace/`, `scheduler/`, `plan/` except lock, `cmd/`, `examples/` — and ~21,000 test lines. dawn typed the contents and left the envelope empty: 4 of 12 ledger fields, zero timing, zero cost, no attempt index, journal keyed by cache identity rather than execution.

**Extract five paragraphs before deleting:** mechanical-failure-vs-rejection (`gate/gate.go:62-72`), the verdict-channel war story (`claude.go:144-157`), single-writer-plus-workers (`run.go:86-89`), content-before-pointer commit (`run.go:291-313`), stable-system-prompt caching (`claude.go:78-89`, ~20.5k cache_creation per call). In this repo the comments are the asset.

**Start clean.** The four things the substrate needs most — a request-grained ledger written by many processes, timing, cost, and a Go-callable seam — are the four things dawn has zero of, and each requires inverting an invariant the code is proud of.

---

## 8. Build order

1. **Ledger + Span on SQLite/WAL** (~400 lines). Schema, `Open/Close/Charge/Fact`, `Query`. Nothing else works without it.
2. **Store + proc** — the 869-line port.
3. **Sandbox** with image/mounts/net + soft/hard deadline. Terminal row *before* the kill.
4. **Seam: the LLM proxy first**, `glue-exec` second, hook endpoint third. The proxy alone yields every mandated cost field.
5. **`glue top` and `glue export --submission`.**

**First end-to-end milestone: one GCSA-shaped task — five rounds, a self-check gate, a form-check that re-enters the completed dynamic stage, one frozen PoC — producing a ledger from which `glue export --submission` emits the full YAML, and `glue top` shows position live while it runs.**

That milestone proves the whole thesis: cyclic control flow is a `for` loop, the gate is a function, isolation is a `Spec`, and the one deliverable every team hand-built and none got credit for falls out as a fold. Refs, Memory, `Reduce`, `Invalidate`, `Redact`, `Select` come after — each when its second caller appears, which for Refs is RO0T and for Memory is Crystalline.

Skipped: eval Suite/Case (that is a test harness, not a substrate — build it when a second component needs versioned scoring). Skipped: `state_diff` (one harness of nineteen produced it, none credit it for score).