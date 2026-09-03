// Command hunt is a vulnerability-discovery harness in the shape Cloudflare's
// Project Glasswing and Microsoft's MDASH independently converged on, built on
// dawn. It exists to answer one question with code instead of prose: what does
// a harness that is NOT CyberGym have to write itself, and what does the
// substrate actually carry?
//
// # The pipeline
//
//	RECON     one driver agent reads the repo and writes a per-codebase attack
//	          taxonomy. Cloudflare: "a custom taxonomy tailored specifically to
//	          that codebase, which is used to more tightly scope the Hunters."
//	HUNT      one concurrent agent per attack class, narrow scope. A hunter that
//	          does not state a threat model files nothing — enforced here, in Go,
//	          not asked for in a prompt.
//	VALIDATE  pass 1 is plain Go: the cited file must exist and the cited line
//	          range must be inside it. No model is involved and none can be
//	          talked around. Pass 2 is a jury on DIFFERENT vendors whose only
//	          job is to disprove; a juror cannot file a finding of its own.
//	DEDUPE    deterministic pre-cluster, then one model call per multi-member
//	          cluster. The model's answer is RECORDED, never used to silently
//	          re-partition the set.
//	REPORT    the harness's own store for the findings, joined to Sweep.Outcomes
//	          for what each stage cost. There is no PROVE stage here; if there
//	          were, it would be Sweep.Sandbox — a container with no network at
//	          all, which is what you run attacker-controlled code in.
//
// # Heterogeneity, which is the point
//
// Cloudflare runs validation on "a completely different model", so a finding is
// "evaluated by an entirely different set of logical weights and training
// data". MDASH goes further: a distilled model absorbs ~90% of the debate
// volume and a second SOTA model is reserved for the hardest ~10%, and model
// DISAGREEMENT is treated as evidence rather than noise. Neither is a prompt
// choice. Both are a second vendor, under one budget.
//
// Four vendors here, three CLIs and one raw endpoint:
//
//	role            transport                       dawn provider
//	driver          claude 2.1.247   (container)    anthropic
//	juror, cheap    droid  0.138.0   (container)    xai
//	juror, cheap    raw HTTP from the supervisor    glm
//	juror, heavy    codex  0.145.0   (container)    openai-responses
//
// claude, codex and droid are the three agent CLIs actually installed on the
// machine this was written on, and on 2026-09-03 each was pointed at a local
// listener to see what it actually sends. All three spoke plain HTTP to a base
// URL carrying a two-segment path prefix, which is the assumption the whole
// routing scheme rests on:
//
//	claude 2.1.247   POST <base>/v1/messages?beta=true   anthropic-version: 2023-06-01
//	codex  0.145.0   POST <base>/responses               Accept: text/event-stream
//	droid  0.138.0   POST <base>/chat/completions        stream_options.include_usage already set
//
// Cursor is NOT installed here and nothing in this file claims anything about
// it. Untested and unfixable from this side: a CLI that pinned a certificate
// would fail before reaching the proxy — none of these three did, because none
// of them was speaking TLS at all.
//
// The fourth voice is not a CLI at all. Z.AI serves GLM on the Anthropic
// Messages wire with bearer auth (internal/proxy/provider.go:76), so the
// supervisor calls it directly with the same 40-line client it would use for
// any Anthropic call — no image, no config file, no container. That is
// deliberate, and it is the cheapest thing in this file: the fourth vendor
// costs one URL.
//
// # The images
//
// Three CLIs, three images, one per role — Launch takes the image per call, so
// which CLI answers this span is a harness decision and nothing is shared
// between them but the network:
//
//	FROM node:22-slim
//	RUN npm i -g @anthropic-ai/claude-code   # or @openai/codex, or @factory-ai/droid
//	ENTRYPOINT ["/bin/sh"]
//
// Untested: no image was built. What it costs if a pin is wrong is one Launch
// that exits non-zero and one span closed Failed, which is visible in the
// ledger rather than silent.
//
// # Running it
//
//	hunt -repo /path/to/target -budget 200 \
//	     -image-claude glue/claude:latest \
//	     -image-codex  glue/codex:latest \
//	     -image-droid  glue/droid:latest
//
// Needs Docker and a Linux host: the bridge gateway the supervisor binds lives
// inside a VM on macOS. Keys come from the environment of this process and are
// never inside any container.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	glue "github.com/valbaudo/dawn"
)

// ---------------------------------------------------------------- domain

// TODO(you): the seed attack classes. dawn refuses workflow definition, and the
// taxonomy IS the workflow: it is the list of hunters, the coverage
// cross-product Gapfill would consume, and the scope sentence in every hunter's
// prompt. RECON refines this per codebase; this is the catalogue it starts
// from. Nothing about it is generic — Cloudflare's is per-codebase and MDASH's
// is per-bug-class with a plugin behind each cell.
var seed = []string{
	"memory-safety",
	"authz-bypass",
	"injection",
	"deserialization",
	"race-condition",
	"resource-exhaustion",
}

// taxon is one row of the taxonomy RECON writes and HUNT renders. It is the
// artifact stage one actually produces: everything domain-specific in a
// hunter's instructions arrives through these five fields, and huntPrompt is a
// fmt.Sprintf over one of them.
//
// This is why the recon prompt below is long and the hunt prompt is a template.
// A harness tunes the taxonomy, not the prose. Two taxonomy.json files diff
// cleanly and a coverage query runs over them; two 400-word prompts do neither,
// and MDASH's per-domain knowledge lives in plugins behind a stable interface
// for exactly that reason. A one-line prompt looks like a missing string; it is
// usually a missing data structure.
//
// TODO(you): the fields. dawn refuses payload schemas — see Finding.
type taxon struct {
	Class    string   `json:"class"`
	Scope    []string `json:"scope"`    // repo-relative; the hunter reads these and no others
	Why      string   `json:"why"`      // file:line in THIS code that makes the class live
	Attacker string   `json:"attacker"` // who they are and what they control
	Sink     string   `json:"sink"`     // file:line where exploitation lands
}

// Finding is the harness's payload schema. dawn refuses payload schemas: it
// stores a key and verbatim bytes (glue.go:315) and has no opinion about the
// shape, which is why this struct lives here and why the JSONL file below is
// the harness's own store rather than a dawn table.
//
// TODO(you): every field. Threat is required by policy, not by the compiler —
// see hunter(), where a finding without one is dropped in Go.
type Finding struct {
	ID     string `json:"id"`
	Class  string `json:"class"`
	Title  string `json:"title"`
	File   string `json:"file"` // repo-relative
	Lo     int    `json:"lo"`   // 1-based, inclusive
	Hi     int    `json:"hi"`
	Threat string `json:"threat"`
	Detail string `json:"detail"`

	// Filled by the pipeline, not by the hunter.
	Pass1   string            `json:"pass1,omitempty"`
	Jury    map[string]string `json:"jury,omitempty"`
	Split   bool              `json:"jury_disagreed,omitempty"`
	Verdict string            `json:"verdict,omitempty"`
	Cluster string            `json:"cluster,omitempty"`
	Note    string            `json:"cluster_note,omitempty"`
}

// id is content-addressed so a hunter re-run under a resumed sweep produces the
// same id, which is what makes the append-only store idempotent. Note what is
// NOT in it: the span id. A span id is a live bearer capability
// (glue.go:221 — "Treat it as a secret"), and a findings database keyed by span
// id is a findings database full of spend capabilities. The join runs the other
// way: the span records the finding id as a Fact.
func (f *Finding) id() string {
	h := sha256.Sum256([]byte(f.Class + "\x00" + f.File + "\x00" +
		fmt.Sprint(f.Lo, f.Hi) + "\x00" + f.Title))
	return hex.EncodeToString(h[:6])
}

// ---------------------------------------------------------------- the store

// store is the harness's findings database. It is a file, it is append-only,
// and every finding is durable the moment it is filed — Cloudflare's "findings
// stream and are saved as they happen, so a crash costs you the task in flight
// and nothing else."
//
// ponytail: JSONL, not SQLite. One writer, append-only, no query beyond a scan
// over a few thousand rows. Upgrade path when Gapfill needs coverage queries
// mid-run: same file, same struct, an index in front.
type store struct {
	mu   sync.Mutex
	w    *os.File
	all  []*Finding
	seen map[string]bool
}

func newStore(path string) (*store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &store{w: f, seen: map[string]bool{}}, nil
}

// add files a finding, or re-emits one whose verdict changed. The in-memory
// list holds pointers and holds each finding once; the file gets a line every
// time, so the JSONL is a log and the last line for an id is its final state.
// fsync per line is the durability Cloudflare's "a crash costs you the task in
// flight and nothing else" actually requires.
func (s *store) add(f *Finding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen[f.ID] {
		s.seen[f.ID] = true
		s.all = append(s.all, f)
	}
	b, _ := json.Marshal(f)
	s.w.Write(append(b, '\n'))
	s.w.Sync()
}

func (s *store) list() []*Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Finding(nil), s.all...)
}

// ---------------------------------------------------------------- harness

type harness struct {
	sw   *glue.Sweep
	repo string // host path to the target checkout
	work string // host scratch root, one subdir per span
	find *store

	// Model ids per role. Four vendors is a config fact, which is exactly the
	// shape Config.Keys has (sweep.go:40).
	driver, cheapA, cheapB, heavy string

	// One image per CLI. Launch takes the image per call, so the jury does not
	// have to share a container image with the driver it is meant to be
	// independent of.
	imgDriver, imgCodex, imgDroid string

	// architecture.md, as RECON wrote it. Every span gets its OWN /work
	// (container(), below), so a hunter cannot see recon's scratch dir unless
	// the harness carries the bytes across — and Cloudflare's validation
	// rejection rate fell from 40% to 11% on "better recon context injection",
	// which is this field and the four lines that write it.
	arch []byte
}

func main() {
	var (
		repo    = flag.String("repo", "", "path to the target checkout (required)")
		work    = flag.String("work", "", "scratch root (default: a temp dir)")
		db      = flag.String("db", "hunt.db", "ledger path")
		sweepID = flag.String("sweep", "hunt", "sweep id; names the reap scope")
		isolate = flag.Bool("isolate", true, "run agents in containers on the sweep's private network")
		imgD    = flag.String("image-claude", "glue/claude:latest", "image carrying claude")
		imgC    = flag.String("image-codex", "glue/codex:latest", "image carrying codex")
		imgX    = flag.String("image-droid", "glue/droid:latest", "image carrying droid")
		budget  = flag.Float64("budget", 200, "sweep cap, USD")
		workers = flag.Int("workers", 8, "concurrent hunters")
		verify  = flag.Bool("verify", true, "prove the network boundary at boot")
	)
	flag.Parse()
	if *repo == "" {
		log.Fatal("hunt: -repo is required")
	}
	abs, err := filepath.Abs(*repo)
	if err != nil {
		log.Fatal(err)
	}
	if *work == "" {
		if *work, err = os.MkdirTemp("", "hunt-"); err != nil {
			log.Fatal(err)
		}
	} else if err := os.MkdirAll(*work, 0o755); err != nil {
		log.Fatal(err)
	}

	// Every credential this sweep can spend, in one map, in this process's
	// memory. No container ever sees one: the proxy strips whatever the agent
	// sent and attaches the real key on the outbound leg
	// (internal/proxy/proxy.go:524).
	keys := map[string]string{}
	for prov, env := range map[string]string{
		"anthropic":        "ANTHROPIC_API_KEY",
		"xai":              "XAI_API_KEY",
		"glm":              "ZAI_API_KEY",
		"openai-responses": "OPENAI_API_KEY",
	} {
		if k := os.Getenv(env); k != "" {
			keys[prov] = k
		}
	}

	// Both overrides are load-bearing and neither is guessable. droid appends
	// "/chat/completions" to its baseUrl and Codex appends "/responses"; the
	// proxy joins upstream.Path + that remainder (internal/proxy/proxy.go:520),
	// and the registered defaults carry no "/v1"
	// (internal/proxy/provider.go:80). Without these two entries every juror
	// call is a 404 at the vendor.
	//
	// They are added only for providers this sweep actually holds a key for:
	// Open refuses an Upstream naming a provider it did not register, which is
	// the right call — a silently ignored override is a sweep pointed at the
	// wrong endpoint — but it means the override table follows the key table.
	upstream := map[string]string{}
	for prov, u := range map[string]string{
		"xai":              "https://api.x.ai/v1",
		"openai-responses": "https://api.openai.com/v1",
	} {
		if _, ok := keys[prov]; ok {
			upstream[prov] = u
		}
	}

	sw, err := glue.Open(glue.Config{
		Sweep: *sweepID, DB: *db, Keys: keys, Isolate: *isolate,
		Budget: glue.USD(*budget), Salvage: glue.USD(*budget / 10),
		Verify: *verify, Upstream: upstream,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer sw.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		if err := sw.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	}()

	fs, err := newStore(filepath.Join(*work, "findings.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	h := &harness{
		sw: sw, repo: abs, work: *work, find: fs,
		imgDriver: *imgD, imgCodex: *imgC, imgDroid: *imgX,
		// TODO(you): model ids. dawn refuses routing policy; which model a role
		// gets is the harness's call and it is the whole MDASH thesis.
		driver: "claude-sonnet-5",
		cheapA: "grok-4-fast",
		cheapB: "glm-4.6",
		heavy:  "gpt-5.6-sol",
	}

	// The run id. Cloudflare keys everything on (run_id, repo, stage); the
	// mapping onto dawn is sweep=run_id, run=repo, stage=child span name, and
	// "run" is the name of the outermost span (glue.go:242).
	//
	// GAP(dawn): a root span's name IS its run id and there is no other way to
	// set one, so the same repo hunted again in a later sweep is a second,
	// unrelated run with no query that unions them. Cloudflare's "any stage can
	// be pulled into a later run without redoing work" is not expressible.
	root := sw.Span(nil, filepath.Base(abs), sw.Budget())

	// Where the budget goes. Sub does NOT reserve from the parent
	// (glue.go:410), so these five deliberately over-subscribe: the sweep cap
	// binds in the same statement as every leaf charge, so over-subscription is
	// safe and is the designed shape.
	//
	// The cheap/heavy split is MDASH's volume routing, expressed as two pools
	// rather than as a rule. jury.cheap pays for two jurors on every surviving
	// finding; jury.heavy pays for one SOTA call only where the cheap jurors
	// disagreed. A pool cannot tell two models apart inside one span, so the
	// routing is a span per juror per finding — a span is two rows and costs
	// nothing.
	b := sw.Budget()
	pools := struct{ recon, hunt, cheap, heavy, dedupe *glue.Pool }{
		recon:  b.Sub("recon", glue.USD(*budget*0.10)),
		hunt:   b.Sub("hunt", glue.USD(*budget*0.60)),
		cheap:  b.Sub("jury.cheap", glue.USD(*budget*0.20)),
		heavy:  b.Sub("jury.heavy", glue.USD(*budget*0.05)),
		dedupe: b.Sub("dedupe", glue.USD(*budget*0.05)),
	}

	// What a previous run of this sweep id already finished. Cloudflare: "any
	// stage can resume, retry, or be pulled into a later run without redoing
	// work."
	//
	// TODO(you): this policy. Sweep.Outcomes hands back closed spans and stops
	// there; deciding that "hunt.injection closed OK last Tuesday" means do not
	// run it again is a judgement about staleness that dawn refuses, and
	// rightly — it is a for loop in main.go like every other control decision.
	done := h.done()

	tax, err := h.recon(ctx, root, pools.recon, done)
	if err != nil {
		root.Close(glue.Failed, "recon: "+err.Error())
		log.Fatal(err)
	}
	h.hunt(ctx, root, pools.hunt, tax, *workers, done)
	h.validate(ctx, root, pools.cheap, pools.heavy, *workers)
	h.dedupe(ctx, root, pools.dedupe)
	root.Close(glue.OK, fmt.Sprintf("%d findings", len(h.find.list())))

	h.report(os.Stdout, root)
	if err := sw.Err(); err != nil {
		log.Fatal("ledger: ", err)
	}
}

// ---------------------------------------------------------------- RECON

// reconPrompt is the whole instruction to the recon agent: role, the physics of
// the container it woke up in, and the two artifacts. It is an ordinary Go
// string. dawn cannot see it, cannot template it and has no type for it —
// Launch takes an image, env, mounts and argv (sweep.go:307) and that is the
// entire interface. Edit the text, run the sweep, and `git log -S` on this
// constant joins the version to the findings it produced through the
// prompt.sha256 Fact.
//
// TODO(you): all of it. Every clause below is a policy decision, and the
// annotation on each one is what it costs when it is missing.
//
//   - "You produce two artifacts and no opinions." The stage has a machine
//     consumer, not a human one. PREVENTS: a well-written security essay in
//     /work/out.txt and an empty taxonomy.json, which fails recon and costs the
//     whole run.
//   - The container physics — read-only /src, no default route, one invocation.
//     None of it is guessable from inside and all of it is enforced (the bridge
//     is created --internal; there is no route out except the proxy).
//     PREVENTS: the agent spending a third of its turns on `pip install
//     semgrep`, `git clone`, and retrying curl against a black hole.
//   - "architecture.md before taxonomy.json." Cloudflare: findings "stream and
//     are saved as they happen, so a crash costs you the task in flight and
//     nothing else" — the same rule applied inside one agent's turn budget.
//     PREVENTS: one non-zero exit costing the entire recon stage.
//   - "Grep for the symbol, read the forty lines around the hit." Cloudflare
//     keeps context "below 25% of the total window" and treats the model as a
//     stateless compute engine; JiuXuan keeps a 6KB working set. This is that
//     discipline stated where the agent can act on it. PREVENTS: context
//     exhaustion at 60% of the repo read, with the taxonomy never written.
//   - The four fixed headings in architecture.md. Named because HUNT consumes
//     this file (h.arch, copied into every hunter's /work) and MDASH's domain
//     plugins inject precisely these — "kernel calling conventions, IRP rules,
//     lock invariants, IPC trust boundaries". A recon agent cannot ship a
//     plugin; it can name the invariants a plugin would encode. PREVENTS:
//     race-condition and authz hunters with nothing to compare a call site
//     against.
//   - "scope: 1 to 40 paths... more than 40 means this is two classes."
//     Cloudflare's hunters take "one attack class each, narrow scope". Scope is
//     the only thing that makes a hunter's context budget achievable. PREVENTS:
//     a taxonomy of six generic nouns each scoped to the whole repo — which is
//     exactly what the one-line prompt this replaced produced.
//   - "why: one sentence naming the construct in THIS code, with a file:line.
//     Not a definition of the class." Cloudflare's taxonomy is "tailored
//     specifically to that codebase", and specificity is what moved their
//     validation rejection rate from 40% to 11%. PREVENTS: an OWASP Top 10
//     restatement, where recon returns the seed list unchanged and stage two
//     learns nothing from stage one.
//   - "DELETE any seed that does not apply." Explicit permission to return
//     fewer than six. PREVENTS: padding to match the list it was handed, which
//     creates an empty coverage cell that Gapfill then re-hunts forever.
//   - The closing sentence about money. The agent has no other way to know that
//     each line of its output becomes a process with a budget. PREVENTS: a
//     long, hedged, comprehensive taxonomy — the default shape of the answer.
//
// Not here, deliberately: the merge of three parallel recon agents (Cloudflare
// runs three; divergence is the product and unioning them on class is Go's
// job), the concurrency limit, and the budget. All workflow, all refused by
// dawn, and a prompt that restates them creates a second drifting copy of a
// rule Go already owns.
const reconPrompt = `You are a static-analysis recon agent. You produce two artifacts and no opinions.

/src is mounted read-only. A write there is a bug in your plan, not a permission problem.
Bash has no network: there is no default route out of this container, so git clone, curl
and every package manager fail. Everything you need is already in /src.

You get ONE invocation. When you exit the process is gone and nothing outside /work
survives. Write architecture.md before taxonomy.json, so a crash costs the tail of the
work rather than the run.

Never read a file whole to find one symbol. Grep for the symbol, then read the forty
lines around the hit. If you have spent a quarter of your context window you have
already lost: stop exploring and write.

Produce exactly two files.

1. /work/architecture.md - at most 1200 words, no code listings, these four headings:

## Entry points
Every place attacker-influenced bytes enter. For each: the file:line where the bytes are
first read, the transport (HTTP body, argv, env, file, DB row, IPC, deserialized
message), and whether authentication happens before or after parsing.

## Trust boundaries
Each crossing from less-trusted to more-trusted. Name both sides and the exact function
that straddles them.

## Invariants
Rules the code assumes but does not check at every call site - "already canonicalized",
"the lock is held", "the handle is owned by the caller". Cite one site that enforces the
rule and one site that only assumes it.

## Privileged sinks
exec, SQL, file writes, deserialization, reflection, memory copies with a computed
length, anything that mutates authorization state.

2. /work/taxonomy.json - a JSON array and nothing else, no prose around it:

[{"class":"...","scope":["path", ...],"why":"...","attacker":"...","sink":"..."}]

class     lowercase, hyphenated, unique. Seeded from: %s
          DELETE any seed that does not apply to this codebase and say nothing about it.
          ADD the classes this codebase earns.
scope     the files a hunter for this class must read, and no others. 1 to 40
          repo-relative paths, each of which exists in /src. More than 40 means this is
          two classes: split it.
why       one sentence naming the construct in THIS code that makes the class live, with
          a file:line. Not a definition of the class.
attacker  who they are and what they control, in this system's own nouns.
sink      the file:line where exploitation lands.

A class you cannot ground in a file:line is not a class. Drop it. Six vague classes are
worse than two grounded ones, because every class you write becomes an agent that spends
money looking for it.
`

// recon runs one driver agent over the whole repo and takes its taxonomy. The
// repo is mounted read-only; the only writable path is the span's own scratch
// dir, so a recon agent cannot edit the code it is describing.
func (h *harness) recon(ctx context.Context, parent *glue.Span, pool *glue.Pool, done map[string][]glue.Fact) ([]taxon, error) {
	// The resumable artifact is the Fact, not the span: a span id is a live
	// capability that Close revoked, but "taxonomy" is bytes on disk that
	// outlive the process that wrote them. Re-running recon on a repo whose
	// architecture has not changed is the single most wasteful thing this
	// pipeline can do.
	if facts, ok := done["recon"]; ok {
		var tax []taxon
		for _, f := range facts {
			switch f.Key {
			case "architecture":
				h.arch = []byte(f.Value)
			case "taxonomy":
				if json.Unmarshal([]byte(f.Value), &tax) != nil {
					tax = nil
				}
			}
		}
		if len(tax) > 0 && len(h.arch) > 0 {
			return tax, nil
		}
	}
	span := h.sw.Span(parent, "recon", pool).Only("anthropic")
	ws, err := h.workspace(span)
	if err != nil {
		span.Close(glue.Failed, err.Error())
		return nil, err
	}
	prompt := fmt.Sprintf(reconPrompt, strings.Join(seed, ", "))
	// dawn never sees this string. Launch takes argv, env and mounts
	// (sweep.go:307) and there is nowhere in the API to put a prompt. What the
	// ledger records is its hash, which is the whole point: a finding is
	// attributable to the prompt VERSION that produced it, the text stays in
	// git, and `git log -S` on the constant below joins the two.
	span.Fact("prompt.sha256", hash(prompt))

	if err := h.container(ctx, span, ws, driverAgent(h.imgDriver, h.driver, prompt)); err != nil {
		span.Close(glue.Failed, err.Error())
		return nil, err
	}
	// architecture.md is a required artifact, not a nice-to-have: the hunt
	// prompt tells every hunter to read it first, so a recon that did not write
	// one has not finished and the failure belongs here rather than fifty
	// hunters later. This is also why the prompt says "architecture.md before
	// taxonomy.json" — Cloudflare's "a crash costs you the task in flight and
	// nothing else" applied inside a single agent's turn budget.
	arch, err := os.ReadFile(filepath.Join(ws, "architecture.md"))
	if err != nil {
		span.Close(glue.Failed, "no architecture.md: "+err.Error())
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(ws, "taxonomy.json"))
	if err != nil {
		span.Close(glue.Failed, "no taxonomy: "+err.Error())
		return nil, err
	}
	var tax []taxon
	if err := json.Unmarshal(raw, &tax); err != nil {
		span.Close(glue.Failed, "taxonomy unparseable")
		return nil, fmt.Errorf("taxonomy unparseable: %v", err)
	}
	// The prompt says every scope path must exist in /src. This is where that
	// stops being a request. Same move as pass1(): a model can be argued out of
	// a rule, os.Stat cannot — and a hunter scoped to a hallucinated path burns
	// a whole agent run discovering it.
	tax = ground(h.repo, tax)
	if len(tax) == 0 {
		span.Close(glue.Failed, "taxonomy has no class grounded in a real path")
		return nil, fmt.Errorf("taxonomy has no class grounded in a real path")
	}
	span.Fact("architecture", arch)
	span.Fact("taxonomy", raw)
	h.arch = arch
	span.Close(glue.OK, fmt.Sprintf("%d classes", len(tax)))
	return tax, nil
}

// ground drops the parts of a taxonomy that do not correspond to files on disk:
// a scope path that does not exist, and then any class left with no scope at
// all. It keeps the RAW taxonomy bytes as the Fact, not the grounded slice, so
// the ledger records what the model actually said and this function's opinion
// stays out of the record.
func ground(repo string, tax []taxon) []taxon {
	var out []taxon
	for _, t := range tax {
		if strings.TrimSpace(t.Class) == "" {
			continue
		}
		var keep []string
		for _, p := range t.Scope {
			if _, err := os.Stat(filepath.Join(repo, filepath.Clean("/"+p))); err == nil {
				keep = append(keep, p)
			}
		}
		if t.Scope = keep; len(t.Scope) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------- HUNT

// huntPrompt renders one taxon. The template is the stable half — harness
// physics, which do not change between a Rust kernel driver and a Django app —
// and the five interpolated fields are the entire domain payload, which is why
// RECON exists and why it is a compiler from a codebase to this struct rather
// than a preamble.
//
// TODO(you): all of it.
//
//   - "You hunt exactly one attack class. Other agents cover everything else."
//     Cloudflare runs ~50 concurrent hunters, one class each. PREVENTS: fifty
//     agents each rediscovering the same SQL injection and fifty jury calls
//     paid to refute forty-nine duplicates.
//   - The five interpolated lines. This is the context injection Cloudflare
//     credits for taking validation rejection from 40% to 11%. PREVENTS: a
//     hunter that has to re-derive from scratch what recon already knows, on a
//     smaller context budget than recon had.
//   - "Read /work/architecture.md first." True only because hunter() copies it
//     in; every span's /work is its own. PREVENTS: a threat model invented from
//     the cited function's local shape, with no idea which boundary it sits
//     behind.
//   - "You MAY copy fragments into /work, compile them and run them."
//     Cloudflare's hunters "build small versions and attack them in sandboxes".
//     Without the sentence the agent reads "/src is read-only" as "look, do not
//     touch". PREVENTS: the highest-value tool in the container going unused —
//     a claim you executed beats a claim you argued.
//   - "Append AS YOU CONFIRM EACH ONE. Do not batch to the end." PREVENTS: a
//     turn-limit or budget kill on the last turn taking every finding with it.
//   - "lo,hi must be the lines holding the defect - not the function, not the
//     import block." pass1() is arithmetic: a function-scoped range passes it
//     and then wastes a juror call on lines containing nothing. PREVENTS: a
//     citation that is technically valid and evidentially empty.
//   - "threat: three sentences — who, what they control, what they get." The Go
//     gate in the loop below only tests for emptiness, so the definition has to
//     live here; the gate is what makes it true. Cloudflare: "a Hunter has to
//     state the threat model before it's allowed to file anything." PREVENTS:
//     "an attacker could exploit this", which satisfies the checker and is not
//     a threat model.
//   - "detail as file:line hops." MDASH's debaters argue reachability and
//     exploitability; hops are the only form in which reachability survives the
//     model boundary into a juror that shares no context. PREVENTS: a plausible
//     paragraph the juror must re-derive, which doubles cost and inflates
//     disagreement — and disagreement is what buys the expensive model.
//   - "State the assumption that would make this a false positive, then check
//     it." PREVENTS: filing the sanitized-upstream false positive that three
//     jurors are then paid to kill.
//   - "A different model on a different vendor, whose only job is to destroy
//     this finding, reads it next." Not a threat — a specification of the bar,
//     and it is literally true (jury(), below). PREVENTS: detail written for a
//     sympathetic reader.
//   - "Zero is a correct outcome." PREVENTS: the fabrication that appears when
//     an agent believes an empty result is a failed run.
//
// Not here: the class name is ASSIGNED by the harness after the file is read,
// so nothing tells the hunter it could file into another cell. The sibling
// hunters' scopes are not here either — a hunter that knows what its neighbours
// cover starts reasoning about coverage instead of its own class.
func huntPrompt(t taxon) string {
	return fmt.Sprintf(huntTemplate, t.Class, t.Why, t.Attacker, t.Sink,
		strings.Join(t.Scope, "\n  "))
}

const huntTemplate = `You hunt exactly one attack class. Other agents cover everything else, and work outside
your scope is duplicated effort at best and noise at worst.

Attack class:      %s
Live here because: %s
Attacker:          %s
Sink:              %s
Read only these files:
  %s

Read /work/architecture.md first. It is 1200 words, written by an agent that saw the
whole repo, and it names the trust boundaries and invariants you are about to test.

/src is read-only; /work is yours. Bash has no network - no installs, no clones, no
fetches. You MAY copy fragments of /src into /work, write a small harness around them,
compile it and run it. That is what the sandbox is for: a claim you executed beats a
claim you argued.

Grep, then read the hit and its neighbours. Do not read whole files. Do not read files
outside your scope. Past a quarter of your context window, write what you have and keep
hunting from the file.

Append one JSON object per line to /work/findings.jsonl AS YOU CONFIRM EACH ONE. Do not
batch to the end; a crash then costs you everything. Fields, all required:

{"class","title","file","lo","hi","threat","detail"}

title   one line. The bug, not the file.
file    repo-relative, must exist in /src.
lo,hi   1-based inclusive, must exist in that file, and must be the lines holding the
        defect - not the enclosing function, not the import block. A range a checker
        cannot resolve is discarded unread.
threat  three sentences: who the attacker is, what they control that reaches the cited
        code, and what they get. "An attacker could..." with no named entry point is not
        a threat model, and it is dropped before a human sees it.
detail  the path from the entry point named in threat to file:lo, as a list of file:line
        hops. If you cannot write the hops you have a suspicion, not a finding. Do not
        file suspicions.

Before you file, once per finding: state the assumption that would make this a false
positive, then go and check it. If a caller sanitizes, if a length is bounded upstream,
if every path holds the lock - find that out yourself. A different model on a different
vendor, whose only job is to destroy this finding, reads it next, and it will find the
caller you did not check.

If this class has no instances here, write nothing and say so. Zero is a correct
outcome, and it is cheaper than one finding that gets refuted.
`

// hunt runs one agent per attack class, concurrently. Cloudflare runs 50-200 of
// these; the semaphore is the entire scheduler and it is the harness's, because
// dawn serializes nothing and correctly refuses to own a work queue.
func (h *harness) hunt(ctx context.Context, parent *glue.Span, pool *glue.Pool, tax []taxon, workers int, done map[string][]glue.Fact) {
	per := glue.USD(float64(pool.Cap()) / float64(len(tax)))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, t := range tax {
		if _, ok := done["hunt."+t.Class]; ok {
			continue // already closed OK in an earlier run of this sweep id
		}
		wg.Add(1)
		go func(t taxon) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h.hunter(ctx, parent, pool.Sub("hunt."+t.Class, per), t)
		}(t)
	}
	wg.Wait()
}

func (h *harness) hunter(ctx context.Context, parent *glue.Span, pool *glue.Pool, t taxon) {
	span := h.sw.Span(parent, "hunt."+t.Class, pool).Only("anthropic")
	span.Fact("attack.class", []byte(t.Class))
	ws, err := h.workspace(span)
	if err != nil {
		span.Close(glue.Failed, err.Error())
		return
	}
	// The recon context injection. It is a file copy because each span's /work
	// is its own; nothing else connects stage one to stage two.
	if err := os.WriteFile(filepath.Join(ws, "architecture.md"), h.arch, 0o644); err != nil {
		span.Close(glue.Failed, err.Error())
		return
	}
	prompt := huntPrompt(t)
	span.Fact("prompt.sha256", hash(prompt))

	if err := h.container(ctx, span, ws, driverAgent(h.imgDriver, h.driver, prompt)); err != nil {
		span.Close(glue.Failed, err.Error())
		return
	}

	found, bad, err := readJSONL(filepath.Join(ws, "findings.jsonl"))
	if err != nil {
		// No file. The agent exited 0 without honouring its output contract —
		// muzzled by a permission mode, out of turns, or it ignored the format.
		// Failed, not OK, so done() re-queues this cell instead of retiring it.
		span.Fact("hunt.no_output", []byte(err.Error()))
		span.Close(glue.Failed, "no findings.jsonl: "+err.Error())
		return
	}

	filed, dropped := 0, 0
	for _, f := range found {
		// "A Hunter must state its threat model before it is allowed to file
		// anything." Enforced here, in Go. A prompt that says "threat is
		// mandatory" is a request; this is the gate.
		if strings.TrimSpace(f.Threat) == "" {
			dropped++
			continue
		}
		// The class is ASSIGNED, not taken from the finding: a hunter scoped to
		// one cell cannot file into another one and inflate its own coverage.
		f.Class, f.ID = t.Class, f.id()
		h.find.add(f)
		// One Fact per finding, N rows under one span, and Outcome.Facts hands
		// all N back in write order. This is the join key between dawn's ledger
		// and the store above, and it runs ledger -> finding rather than
		// finding -> span, so no capability lands in the findings file.
		span.Fact("finding", []byte(f.ID))
		filed++
	}
	span.Fact("hunt.dropped_no_threat", []byte(fmt.Sprint(dropped)))
	span.Fact("hunt.unparseable", []byte(fmt.Sprint(bad)))
	span.Close(glue.OK, fmt.Sprintf("%d filed, %d dropped, %d unparseable", filed, dropped, bad))
}

// done reads the ledger and returns the facts of every span that closed OK,
// keyed by span name. This is Sweep.Outcomes (sweep.go:442), dawn's one read, and it
// is exactly enough for the two things Glasswing needs it for: skipping
// finished work, and the coverage query — which (repo, attack-class) cells have
// no span that closed OK — that Gapfill would consume.
func (h *harness) done() map[string][]glue.Fact {
	m := map[string][]glue.Fact{}
	outs, err := h.sw.Outcomes()
	if err != nil {
		return m // a first run has no ledger to read; that is not an error
	}
	for _, o := range outs {
		if o.Class == glue.OK {
			m[o.Name] = o.Facts
		}
	}
	return m
}

// ---------------------------------------------------------------- VALIDATE

// validate is two passes and they are not the same kind of thing. Pass 1 is
// arithmetic. Pass 2 is a jury that can only say no.
func (h *harness) validate(ctx context.Context, parent *glue.Span, cheap, heavy *glue.Pool, workers int) {
	all := h.find.list()
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, f := range all {
		// Pass 1: plain Go, no model, cannot be argued with. Cloudflare's first
		// validation pass "checks schema and that cited files/paths actually
		// exist"; their rejection rate was 40% before context work, so this
		// pass is not a formality.
		if why := pass1(h.repo, f); why != "" {
			f.Pass1, f.Verdict = why, "rejected"
			h.find.add(f) // durable before we move on, like every other verdict
			continue
		}
		f.Pass1 = "ok"
		wg.Add(1)
		go func(f *Finding) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h.jury(ctx, parent, cheap, heavy, f)
		}(f)
	}
	wg.Wait()
}

// pass1 is the deterministic pass. It answers exactly one question — does the
// cited code exist — and it answers it by reading the disk.
//
// TODO(you): everything past line existence. Cloudflare also parses the patch
// and the test, and checks the finding against the schema; MDASH's plugins go
// further and construct a triggering artifact. That is the acceptance predicate
// for one taxonomy cell, it is 35% of the only harness in this repo
// (cmd/nsfocus/main.go:346), and dawn refuses it by design.
func pass1(repo string, f *Finding) string {
	if f.File == "" || f.Lo < 1 || f.Hi < f.Lo {
		return "bad citation"
	}
	clean := filepath.Clean(f.File)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "path escapes repo"
	}
	fh, err := os.Open(filepath.Join(repo, clean))
	if err != nil {
		return "no such file"
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "not a regular file"
	}
	n := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		n++
	}
	if sc.Err() != nil {
		return "unreadable"
	}
	if f.Hi > n {
		return fmt.Sprintf("line %d past EOF (%d lines)", f.Hi, n)
	}
	return ""
}

// disprovePrompt is the single highest-leverage string in this file, and not
// because it is well worded: it is the one place where what the harness wants
// and what the transport can carry are the same artifact. It is ONE
// self-contained user string because two of the three juror transports cannot
// carry a system prompt at all — codex exec 0.145.0 has no system-prompt flag,
// and the GLM juror is a raw Messages call with no system field (ask(), below).
//
// TODO(you): all of it.
//
//   - "You cannot file findings" as the first three sentences. Cloudflare's
//     validator "cannot log findings of its own". Three mechanical facts
//     already enforce it (jury's doc comment); this is the prompt telling the
//     agent so it does not waste turns trying. PREVENTS: a juror helpfully
//     filing an adjacent bug into a /work that is thrown away — pure spend.
//   - "Your job is to destroy the claim. Assume it is wrong." MDASH: "an
//     auditor does not reason like a debater, which does not reason like a
//     prover", and each stage gets "its own role, prompt regime, tools, and
//     stop criteria". PREVENTS: the agreeable-reviewer failure, where a model
//     asked to "evaluate" a finding ratifies whatever it is shown — which
//     collapses the jury to a rubber stamp and makes tally() meaningless.
//   - "You have never seen this codebase and share no context." True by
//     construction: different vendor, different weights, Only() (glue.go:298)
//     narrowing the span so it cannot even reach the driver's model. PREVENTS:
//     the juror assuming a convention the hunter saw and it did not.
//   - Five numbered checks, cheapest first, stop at the first failure. Step 1
//     is one file read; step 4 is a caller analysis. PREVENTS: paying output
//     tokens for exploitability analysis of code that does not exist.
//   - Step 5, "a crash is not code execution, a read is not a write."
//     PREVENTS: the impact inflation that survives every other check because
//     the vulnerability is real and only the consequence is wrong.
//   - "First line: exactly one word." verdictOf() reads the first line and
//     discards the rest. PREVENTS: an unparseable first line landing as
//     "error", which closeJuror() classes Failed and which silently changes the
//     vote.
//   - "A STANDS with no attempted disproof is a failure to do your job."
//     PREVENTS: the cheap STANDS, which is indistinguishable from a diligent
//     one in tally() and costs the same.
//   - "Do not hedge. Possibly exploitable is STANDS." PREVENTS: "needs further
//     investigation", which is not a verdict and which no downstream code can
//     act on.
//
// Not here: the tally rule, the routing between cheap and heavy jurors, and the
// fact that a second juror is voting on the same claim. All ranking policy,
// which dawn refuses and tally() owns. A juror that knew it was one of three
// would start reasoning about the panel.
func disprovePrompt(f *Finding) string {
	return fmt.Sprintf(disproveTemplate,
		f.Class, f.Title, f.File, f.Lo, f.Hi, f.Threat, f.Detail)
}

const disproveTemplate = `You are a referee. You cannot file findings. You cannot add findings. You cannot improve
this one, restate it more convincingly, or suggest a variant that would work. Any output
that is not a verdict on THIS claim is discarded unread.

Your job is to destroy the claim below. Assume it is wrong and find out how.

You have never seen this codebase and you share no context with whoever wrote the claim.
Everything you need is here; there is no earlier conversation. /src is mounted
read-only. Read it.

CLAIM
  class:        %s
  title:        %s
  location:     %s:%d-%d
  threat model: %s
  reachability: %s

Check in this order. Stop at the first failure.

1. Does the cited location contain what the claim says? Open it. A claim about code that
   is not there is REFUTED whatever else is true.
2. Is the entry point in the threat model reachable by that attacker, in this build,
   without a credential they do not have?
3. Does every hop in the reachability path exist, and does control actually flow along
   it? Follow each hop in /src. One broken hop refutes the claim.
4. Is there an upstream check - a bound, a canonicalization, a held lock, a type that
   cannot represent the bad value - that makes the bad state unreachable? Look in the
   callers, not only at the cited lines.
5. Is the asserted consequence the one that actually follows? A crash is not code
   execution. A read is not a write.

First line: exactly one word, REFUTED or STANDS.
Then the file:line evidence for that word. If REFUTED, name which numbered step failed
and the code that refutes it. If STANDS, name the strongest disproof you attempted and
why it failed - a STANDS with no attempted disproof is a failure to do your job.

Do not hedge. "Possibly exploitable" is STANDS. "Needs more investigation" is not a
verdict. Pick one.
`

// jury is pass 2. Two cheap jurors on two vendors argue against every finding;
// a third, expensive, different-vendor juror is called ONLY where they
// disagreed. That is MDASH's routing — a distilled debater absorbing ~90% of
// the volume, a second SOTA model reserved for the hardest ~10% — and here it
// is two pools and one if.
//
// Three mechanical facts make a juror unable to file a finding of its own,
// none of which is a prompt instruction:
//   - /src is read-only and its /work is a throwaway nothing downstream reads,
//     so there is no path from a juror to the findings store;
//   - the harness reads one token from its output and discards the rest;
//   - Only() (glue.go:298) narrows its span to one provider, so it cannot even
//     reach the driver's model to launder an opinion through it.
func (h *harness) jury(ctx context.Context, parent *glue.Span, cheap, heavy *glue.Pool, f *Finding) {
	span := h.sw.Span(parent, "validate."+f.ID, cheap)
	span.Fact("finding", []byte(f.ID))
	f.Jury = map[string]string{}

	prompt := disprovePrompt(f)
	span.Fact("prompt.sha256", hash(prompt))

	f.Jury["xai/"+h.cheapA] = h.jurorContainer(ctx, span, cheap, "xai", droidAgent(h.imgDroid, h.cheapA, prompt))
	f.Jury["glm/"+h.cheapB] = h.jurorAsk(ctx, span, cheap, "glm", h.cheapB, prompt)

	a, b := f.Jury["xai/"+h.cheapA], f.Jury["glm/"+h.cheapB]
	f.Split = a != b
	if f.Split {
		// Disagreement is EVIDENCE, not noise to average away. It is recorded on
		// the span and on the finding, and it is what buys the expensive call.
		span.Fact("jury.disagreed", []byte(a+"|"+b))
		f.Jury[heavyProvider+"/"+h.heavy] = h.jurorContainer(
			ctx, span, heavy, heavyProvider, codexAgent(h.imgCodex, h.heavy, prompt))
	}

	f.Verdict = tally(f.Jury)
	span.Fact("jury.verdict", []byte(f.Verdict))
	h.find.add(f) // a second JSONL line; the store is append-only, last line wins on read
	if f.Verdict == "stands" {
		span.Close(glue.OK, "stands")
		return
	}
	span.Close(glue.Rejected, "refuted")
}

// tally is the verdict rule, and it is deliberately not an average. Any REFUTED
// from the heavy juror is final; otherwise a finding survives only if no cheap
// juror refuted it. MDASH: "when an auditor flags something the debater cannot
// refute, that finding's posterior credibility goes up" — the credibility here
// is the recorded set of votes, which stays readable, not a number that hides
// which model said what.
//
// TODO(you): this rule. It is ranking policy, which dawn refuses; a harness
// that wants unanimity, or that weights the heavy juror differently, changes
// exactly this function and nothing else.
func tally(votes map[string]string) string {
	for k, v := range votes {
		if strings.HasPrefix(k, heavyProvider+"/") {
			return v
		}
	}
	for _, v := range votes {
		if v == "refuted" {
			return "refuted"
		}
	}
	return "stands"
}

// heavyProvider is the tiebreaker's vendor. It is a constant because it appears
// in three places that must agree: the pool the call is charged to, the Only()
// that keeps the tiebreaker off every other vendor, and tally above.
const heavyProvider = "openai-responses"

// jurorContainer runs one juror CLI in a container on its own span, so the
// juror's spend is attributed to it and to nothing else.
func (h *harness) jurorContainer(ctx context.Context, parent *glue.Span, pool *glue.Pool, prov string, a agent) string {
	span := h.sw.Span(parent, "juror."+prov, pool).Only(prov)
	ws, err := h.workspace(span)
	if err != nil {
		span.Close(glue.Failed, err.Error())
		return "error"
	}
	if err := h.container(ctx, span, ws, a); err != nil {
		span.Close(glue.Failed, err.Error())
		return "error"
	}
	out, _ := os.ReadFile(filepath.Join(ws, "out.txt"))
	return closeJuror(span, verdictOf(string(out)))
}

// closeJuror keeps "the juror ran" and "the juror answered" apart. An unparsed
// answer is a Failed span, not an OK one: done() skips work that closed OK, and
// a juror recorded as OK having said nothing is how a resumed run silently
// inherits a verdict nobody reached.
func closeJuror(span *glue.Span, v string) string {
	if v == "unparsed" {
		span.Close(glue.Failed, "unparsed")
		return v
	}
	span.Close(glue.OK, v)
	return v
}

// jurorAsk is the fourth voice: one HTTP call from the supervisor, through the
// supervisor's own proxy, on a span the supervisor minted for it. Nothing is
// privileged about it — it is reserved before it leaves, trued up when it
// lands, priced on write, and refused with a 429 if the pool is out. No CLI, no
// container, no image; the whole integration is a base URL.
func (h *harness) jurorAsk(ctx context.Context, parent *glue.Span, pool *glue.Pool, prov, model, prompt string) string {
	span := h.sw.Span(parent, "juror."+prov, pool).Only(prov)
	out, err := ask(ctx, span, prov, model, prompt)
	if err != nil {
		span.Close(glue.Failed, err.Error())
		return "error"
	}
	return closeJuror(span, verdictOf(out))
}

// verdictOf reads exactly one token. Everything else the juror produced is
// discarded unread, which is the mechanical half of "a juror cannot file".
func verdictOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		switch strings.ToUpper(strings.TrimSpace(line)) {
		case "REFUTED":
			return "refuted"
		case "STANDS":
			return "stands"
		}
	}
	return "unparsed"
}

// ---------------------------------------------------------------- DEDUPE

// dedupe clusters surviving findings by root cause, in Cloudflare's two halves:
// deterministic code first, then an agent over the short list.
func (h *harness) dedupe(ctx context.Context, parent *glue.Span, pool *glue.Pool) {
	span := h.sw.Span(parent, "dedupe", pool).Only("glm")
	groups := map[string][]*Finding{}
	for _, f := range h.find.list() {
		if f.Verdict != "stands" {
			continue
		}
		k := clusterKey(f)
		f.Cluster = k
		h.find.add(f)
		groups[k] = append(groups[k], f)
	}
	asked := 0
	for k, g := range groups {
		if len(g) < 2 {
			continue
		}
		asked++
		var b strings.Builder
		fmt.Fprintf(&b, "Do these %d reports share ONE root cause? Answer YES or NO, then why.\n", len(g))
		for _, f := range g {
			fmt.Fprintf(&b, "- %s (%s:%d-%d) %s\n", f.Title, f.File, f.Lo, f.Hi, f.Detail)
		}
		note, err := ask(ctx, span, "glm", h.cheapB, b.String())
		if err != nil {
			note = "error: " + err.Error()
		}
		// TODO(you): what to DO with that answer. Splitting the cluster on a
		// NO is merge policy, which dawn refuses at runtime and which this
		// example therefore refuses too: the answer is recorded beside the
		// cluster and the partition is left as the deterministic pass computed
		// it. A harness that wants the model to re-partition writes that here,
		// deliberately, and owns the result.
		for _, f := range g {
			f.Note = firstLine(note)
			h.find.add(f)
		}
		span.Fact("cluster."+k, []byte(firstLine(note)))
	}
	span.Close(glue.OK, fmt.Sprintf("%d clusters, %d asked", len(groups), asked))
}

// TODO(you): the equivalence relation. "Same root cause" is domain knowledge —
// Cloudflare uses inverted indexes over files, functions, trust boundaries and
// rare tokens; MDASH groups by the patch that would fix them. This is the
// laziest key that is not wrong, and it is wrong often.
//
// ponytail: (class, directory). Cost: two bugs in the same directory from the
// same class collapse even when unrelated, and one bug reachable from two
// directories splits. Upgrade path: patch-based grouping, which needs a patch,
// which needs the PATCH stage this example does not have.
func clusterKey(f *Finding) string {
	return f.Class + ":" + filepath.Dir(filepath.Clean(f.File))
}

// ---------------------------------------------------------------- REPORT

// report joins the two halves. The findings are the harness's, because dawn has
// no opinion about what a finding is; the money is dawn's, because that is one
// of the four facts an agent would profit from forging.
//
// Note which direction the join runs. The findings store holds no span id — a
// span id is a live bearer capability — so the link is the finding id recorded
// as a Fact against the span, read back here out of Outcome.Facts.
func (h *harness) report(w io.Writer, root *glue.Span) {
	byCluster := map[string][]*Finding{}
	n := 0
	for _, f := range h.find.list() {
		if f.Verdict != "stands" {
			continue
		}
		n++
		byCluster[f.Cluster] = append(byCluster[f.Cluster], f)
	}
	keys := make([]string, 0, len(byCluster))
	for k := range byCluster {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintf(w, "%d cluster%s, %d finding%s survived\n\n",
		len(keys), plural(len(keys)), n, plural(n))
	for _, k := range keys {
		fmt.Fprintf(w, "== %s\n", k)
		for _, f := range byCluster[k] {
			split := ""
			if f.Split {
				split = "  [jury split]"
			}
			fmt.Fprintf(w, "  %s  %s:%d-%d  %s%s\n", f.ID, f.File, f.Lo, f.Hi, f.Title, split)
			for _, j := range sortedKeys(f.Jury) {
				fmt.Fprintf(w, "      %-34s %s\n", j, f.Jury[j])
			}
		}
		if n := byCluster[k][0].Note; n != "" {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
		fmt.Fprintln(w)
	}
	// Outcome.Spend is a subtree rollup, so summing the direct children of the
	// root gives the cost of each stage including every agent under it, not the
	// cost of its own bookkeeping. Pool has no Remaining and should not — any
	// live number is stale before it returns — but this is the retrospective,
	// and the retrospective is a fact.
	outs, err := h.sw.Outcomes()
	if err != nil {
		fmt.Fprintf(w, "spend: unreadable: %v\n", err)
		return
	}
	stage := map[string]glue.USD{}
	var total glue.USD
	unpriced := 0
	for _, o := range outs {
		if o.Parent != root.ID() {
			continue
		}
		name, _, _ := strings.Cut(o.Name, ".")
		stage[name] += o.Spend
		total += o.Spend
		if !o.PriceKnown {
			unpriced++
		}
	}
	fmt.Fprintln(w, "spend")
	for _, k := range []string{"recon", "hunt", "validate", "dedupe"} {
		fmt.Fprintf(w, "  %-10s $%.4f\n", k, float64(stage[k]))
	}
	fmt.Fprintf(w, "  %-10s $%.4f\n", "TOTAL", float64(total))
	if unpriced > 0 {
		// Honest column: internal/proxy/price.go carries Anthropic rates only,
		// so every xai, glm and openai-responses call lands price_known=0 and
		// is charged at the global ceiling. That is a BOUND, not a number,
		// until someone fills those rows from the vendors' own pages.
		fmt.Fprintf(w, "  (calls priced at the ceiling in %d stage%s)\n",
			unpriced, plural(unpriced))
	}
}

// ---------------------------------------------------------------- agents

// launch is one container: which image, what environment, what argv. It maps
// one-to-one onto Sweep.Launch, and the mapping is the whole integration
// surface for an agent CLI.
type launch struct {
	Image string
	Env   []string
	Argv  []string
}

// agent produces that, after writing whatever config file the CLI reads into
// the span's own scratch dir. The three CLIs differ only in that file and in
// two environment variables; dawn mints the one string all three need,
// Span.BaseURL(provider) (glue.go:288). Building the file is the harness's job
// — dawn does not know what agent is in the image, and should not.
//
// The shell is here for redirection and nothing else. Every variable these CLIs
// need is passed through Launch's own env argument, where docker appends the
// span's ANTHROPIC_BASE_URL last so a caller cannot shadow it.
type agent func(span *glue.Span, ws string) (launch, error)

// driverArgv is Claude Code. Sweep.Launch already injects ANTHROPIC_BASE_URL
// with the span and the provider in the path (internal/oci/net.go:99), so the
// driver needs no config file at all.
//
// ANTHROPIC_AUTH_TOKEN has to be set or the CLI looks for a login store that
// does not exist in a container; its value is irrelevant because the proxy
// discards whatever the container sent. Verified 2026-09-03: Claude Code 2.1.247 sent
// an Authorization: Bearer of its own from the host login store even with
// ANTHROPIC_AUTH_TOKEN set, which is why the proxy's unconditional
// Header.Del("Authorization") (internal/proxy/proxy.go:524) is load-bearing and
// not defensive.
func driverAgent(image, model, prompt string) agent {
	return func(*glue.Span, string) (launch, error) {
		return launch{
			Image: image,
			// ANTHROPIC_BASE_URL is NOT here: Launch appends the span's own
			// last, and docker takes last-wins on a repeated -e, so metering
			// cannot be shadowed by anything the harness passes.
			// UNVERIFIED: that Claude Code honours ANTHROPIC_MODEL. The probe
			// on 2026-09-03 only exercised `claude -p`. If it is ignored the
			// driver silently runs the image's default model, which the ledger
			// still records truthfully in Outcome.Model — so the cost of being
			// wrong is a surprise in the report, not a silent overspend.
			Env:  []string{"ANTHROPIC_AUTH_TOKEN=glue", "ANTHROPIC_MODEL=" + model},
			Argv: []string{"sh", "-c", `exec claude -p "$0" >/work/out.txt 2>/work/err.txt`, prompt},
		}, nil
	}
}

// codexArgv is Codex. Verified 2026-09-03 against codex 0.145.0: wire_api
// "chat" is refused by the binary ("no longer supported"), so Responses is the
// only wire and internal/proxy/provider.go's responsesParser is not optional.
// CODEX_HOME points at a config the harness writes into the span's own scratch
// dir, so two jurors in one sweep cannot share state through ~/.codex.
func codexAgent(image, model, prompt string) agent {
	return func(span *glue.Span, ws string) (launch, error) {
		dir := filepath.Join(ws, "codex")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return launch{}, err
		}
		cfg := fmt.Sprintf(`model = %q
model_provider = "glue"

[model_providers.glue]
name = "glue"
base_url = %q
wire_api = "responses"
env_key = "GLUE_KEY"
requires_openai_auth = false
`, model, span.BaseURL("openai-responses"))
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644); err != nil {
			return launch{}, err
		}
		return launch{
			Image: image,
			// CODEX_HOME points into the span's own scratch dir, so two jurors
			// in one sweep cannot share state through ~/.codex. GLUE_KEY is the
			// placeholder env_key above names; the proxy discards it unread and
			// attaches the real credential on the outbound leg.
			Env: []string{"CODEX_HOME=/work/codex", "GLUE_KEY=glue"},
			Argv: []string{"sh", "-c",
				`exec codex exec --skip-git-repo-check "$0" >/work/out.txt 2>/work/err.txt`, prompt},
		}, nil
	}
}

// droidArgv is Factory droid. Verified 2026-09-03 against droid 0.138.0: it
// POSTs baseUrl + "/chat/completions" and already sets
// stream_options.include_usage itself, which is why the proxy's injection of
// the same field (internal/proxy/provider.go:256) is idempotent rather than a
// conflict. GLUE_KEY equivalent is the literal apiKey below; it is a
// placeholder the proxy strips before the request leaves the process.
func droidAgent(image, model, prompt string) agent {
	return func(span *glue.Span, ws string) (launch, error) {
		dir := filepath.Join(ws, "droid")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return launch{}, err
		}
		cfg, err := json.Marshal(map[string]any{"customModels": []map[string]any{{
			"model":           model,
			"displayName":     "glue",
			"provider":        "generic-chat-completion-api",
			"baseUrl":         span.BaseURL("xai"),
			"apiKey":          "glue",
			"maxOutputTokens": 8192,
		}}})
		if err != nil {
			return launch{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), cfg, 0o644); err != nil {
			return launch{}, err
		}
		return launch{
			Image: image,
			Argv: []string{"sh", "-c",
				`exec droid exec --settings /work/droid/settings.json -m "$0" "$1" >/work/out.txt 2>/work/err.txt`,
				model, prompt},
		}, nil
	}
}

// container writes the agent's config, launches it, and waits. /src is
// read-only for every agent kind including the driver — nothing in this
// pipeline edits the code it is describing — and /work is the span's own
// throwaway scratch dir, so two agents cannot see each other's state.
//
// Cancellation is Wait's problem, not this function's: it kills the container
// before returning, because the container is the lease on the span. A harness
// that shells out to `docker wait` gets the happy path right and that part
// wrong, and the symptom is a cancelled sweep that keeps spending.
func (h *harness) container(ctx context.Context, span *glue.Span, ws string, a agent) error {
	l, err := a(span, ws)
	if err != nil {
		return err
	}
	id, err := h.sw.Launch(span, l.Image, l.Env,
		[]string{h.repo + ":/src:ro", ws + ":/work"}, l.Argv...)
	if err != nil {
		return err
	}
	defer h.sw.Kill(span)
	code, err := h.sw.Wait(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		e, _ := os.ReadFile(filepath.Join(ws, "err.txt"))
		return fmt.Errorf("agent exit %d: %s", code, tail(string(e), 300))
	}
	return nil
}

// ask makes one Anthropic-wire call from the supervisor through the
// supervisor's own proxy. Used for the GLM juror and for dedupe.
//
// GAP(dawn): every harness writes this function. dawn hands out an address and
// a span and nothing else, so the client is yours — and once the jury is
// heterogeneous it is your client per wire, not one client. This one covers the
// Anthropic wire, which GLM also speaks (internal/proxy/provider.go:76); a
// supervisor-side OpenAI juror would need a second one.
func ask(ctx context.Context, span *glue.Span, prov, model, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		span.BaseURL(prov)+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", res.Status, tail(string(raw), 200))
	}
	var m struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
	}
	return b.String(), nil
}

// ---------------------------------------------------------------- plumbing

func (h *harness) workspace(span *glue.Span) (string, error) {
	// Named by span id, which is fine on the host: the id is a capability
	// against the proxy, and the host filesystem is already inside the trust
	// boundary. It is never written into the findings store.
	d := filepath.Join(h.work, span.ID())
	return d, os.MkdirAll(d, 0o755)
}

// readJSONL returns the findings, the number of unparseable lines, and an
// error if the file is ABSENT.
//
// The three outcomes must stay distinguishable. An agent that ran and found
// nothing writes an empty file; an agent that was muzzled, crashed, or never
// understood its output contract writes no file at all. Collapsing those into
// a nil slice — which the first version of this function did — makes a silent
// zero look exactly like an honest zero, and hunter then closes OK on it. Since
// done() keys resumption on OK, that permanently retires the attack class: the
// cell reads covered on every later run of this sweep and is never hunted
// again. A coverage harness that quietly stops covering things is worse than
// one that crashes.
func readJSONL(path string) ([]*Finding, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []*Finding
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var v Finding
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			bad++
			continue
		}
		out = append(out, &v)
	}
	return out, bad, sc.Err()
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return []byte(hex.EncodeToString(h[:]))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}
