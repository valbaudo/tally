// Package glue is the substrate an agent harness sits on. It is the only
// package a harness imports, and it is the whole of what a harness may do.
//
// # What glue is
//
// A sweep is one ordinary Go process. It holds the only SQLite handle in the
// system, runs one HTTP listener, and launches every agent container. It is not
// a daemon, there is nothing to install, and there is no second process to keep
// alive. Kill it and the sweep is over.
//
// Every agent runs in a container that has no credential, no database, and one
// route out: the supervisor's listener. The container is handed
// ANTHROPIC_BASE_URL=http://<listener>/s/<span id> and nothing else, so the
// span id rides in the request path with zero cooperation from the SDK — every
// Anthropic client appends /v1/messages to whatever base you give it. Spend is
// therefore observed and refused in the one place it passes through, and an
// agent cannot opt out of accounting because there is nowhere else to send a
// request.
//
// # The package layout, and what it refuses to compile
//
//	glue/               this package. Ledger, Span, Pool, Sweep, Class, USD.
//	internal/store/     the SQLite handle, the schema, every statement.
//	internal/proxy/     the listener, the SSE tap, pricing, call_id inference.
//	internal/oci/       the Docker network, container launch, the boot reap.
//	internal/tui/       glue top and glue table. Read-only.
//	cmd/glue/           the CLI.
//
// Go's internal rule is not a convention here, it is the enforcement. A package
// under internal/ can only be imported from within the module rooted at its
// parent, so a harness in any other module gets a compile error, not a warning
// and not a code review comment. Concretely, a harness CANNOT:
//
//   - obtain a *sql.DB, or any handle to the ledger file, and therefore cannot
//     append an events row that did not describe something that happened. There
//     is no exported field, method or type in this package that yields one;
//   - construct or bypass the proxy, and therefore cannot make an LLM call that
//     is not metered — the only credential is inside the supervisor process and
//     the only route out of a container is the listener;
//   - launch a container, and therefore cannot launch one without the sweep
//     label the boot reap collects by, or with a network that has a default
//     route, or with the ledger bind-mounted in;
//   - read live budget state, and therefore cannot make a decision on it. See
//     the note on the absent Pool.Remaining below.
//
// The design document promises those four things. internal/ is what converts
// each promise into a build failure.
//
// # Shape of a harness
//
//	sw, err := glue.Open(glue.Config{
//		Sweep: "nsfocus-l1", DB: "glue.db", APIKey: key,
//		Image: "cybergym/agent:latest", Budget: 2000, Salvage: 400,
//	})
//	defer sw.Close()
//	go sw.Serve(ctx)
//
//	for _, task := range tasks {
//		pool := sw.Budget().Sub(task.ID, 12)
//		span := sw.Span(nil, task.ID, pool)
//		sw.Launch(span, []string{task.Dir + ":/work"}, "agent", "--task", "/work")
//		// ... wait for the container, judge it ...
//		span.Fact("cybergym.task", []byte(task.ID))
//		span.Close(glue.OK, "")
//	}
package glue

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/valbaudo/dawn/internal/proxy"
	"github.com/valbaudo/dawn/internal/store"
)

// Class is the mechanical verdict on a span. It is an int, and the fact that it
// is not a string is load-bearing in three ways.
//
// First, the zero value has to mean something honest. A supervisor that dies
// leaves spans open; the boot reap closes them with Unknown, which reads as "we
// do not know what happened here" — not as a failure, and not as a success. The
// zero value of a string enum is "", which every reader has to special-case and
// half of them will forget.
//
// Second, the set is closed by the compiler. A five-way partition that a report
// may branch on is a different thing from the free-text Outcome that sits next
// to it, and keeping them in different types is what stops them blurring.
// Outcome is indexed and never parsed; the day someone writes
// `WHERE class = 'timeout'` the five-way partition has quietly become a
// hundred-way one and every comparison across sweeps is broken.
//
// Third, GROUP BY over a million rows is an integer sort.
//
// Only supervisor-side code writes it. An agent cannot set its own class: the
// only thing that reaches the ledger from inside a container is a metered LLM
// call, and the proxy classifies those itself.
//
// It is an alias rather than a fresh type because the enum is shared by two
// packages that must not import each other — this one and internal/proxy — so
// its definition lives below both. Nothing leaks: the only method on it is
// String, and USD below is a float64. The alternative was two copies of a
// five-value enum that could drift.
type Class = store.Class

// The five outcomes. Unknown is deliberately the zero value.
const (
	// Unknown: nobody said. A reaped span, or a span still open.
	Unknown = store.Unknown
	// OK: the thing the span existed to do, happened.
	OK = store.OK
	// Rejected: the substrate refused it. A budget cap, a gate. Not a failure
	// of the agent; a decision by us. Kept distinct from Failed because the two
	// have opposite implications for whether to retry, and for whether the
	// number you publish is a measurement or an artifact of your own limits.
	Rejected = store.Rejected
	// Failed: it was attempted and it did not work.
	Failed = store.Failed
	// Cancelled: it was stopped from outside — a deadline, a kill, a client
	// that went away mid-stream.
	Cancelled = store.Cancelled
)

// USD is dollars. See store.USD for why it is a float.
type USD = store.USD

// ---------------------------------------------------------------- ledger

// Ledger is the append-only record of a sweep.
//
// Every row is an INSERT. There is no UPDATE anywhere in the events table, and
// that single rule is what buys: a reader in another process needs no snapshot
// and no lock (glue top runs at 1 Hz against a live sweep and never blocks the
// writer); a crash cannot leave a half-mutated row, only a missing one; and the
// history of a sweep is reconstructible after the fact rather than being
// overwritten by its own summary.
type Ledger struct {
	db   *store.DB
	base string // http://host:port — the listener a container is pointed at
	root *Pool

	mu    sync.RWMutex
	spans map[string]*Span
	// caps records every span's pool ceiling and is never pruned, unlike
	// spans. The report needs the cap of a span that has already closed, and
	// closing a span deliberately drops it from spans to revoke its capability.
	caps map[string]USD
	err  error
}

// Err returns the first write error the ledger hit, if any.
//
// Span, Fact and Close return nothing on purpose. A harness cannot do anything
// useful with a failed INSERT: the disk is gone, or the schema is wrong, and
// either way the sweep is over — no retry loop at the call site fixes it.
// Returning an error from every one of them would put an `if err != nil` after
// every span in every harness, and every one of those would be ignored, which
// is strictly worse than one sticky error checked once at the end.
func (l *Ledger) Err() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.err
}

func (l *Ledger) fail(err error) {
	if err == nil {
		return
	}
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
}

// ------------------------------------------------------------------ span

// Span is a unit of work with a cost, a parent, and an outcome.
//
// It is TWO rows in the ledger — span_open when it starts, span_close when it
// ends — and never one row that gets updated. Three reasons, in order of how
// much they matter:
//
//  1. An open span and a closed span are different facts, and the open one is
//     worth keeping. `glue top` shows the forest of spans that are open right
//     now, which is derivable only if "opened and never closed" leaves a trace.
//     With one mutable row you can see the current state and nothing else.
//  2. ts..last_seen across the pair is the span's wall clock, for free, with no
//     clock read at close time that has to be reconciled with the one at open.
//  3. A crashed supervisor leaves span_open rows with no span_close. That is
//     precisely the query the boot reap runs, and it is an anti-join over an
//     index, not a scan for rows in a suspicious state. Recovery falls out of
//     the representation instead of needing a status column that itself has to
//     be updated, which is the very thing we are avoiding.
//
// The database enforces it: a unique partial index on (span) WHERE
// kind='span_open' (and the same for span_close) means a duplicate is an error
// at INSERT rather than a silently doubled subtree in every roll-up.
type Span struct {
	l      *Ledger
	id     string
	parent string
	run    string
	pool   *Pool
	closed sync.Once
}

// ID is the span's identifier and, in the same 130 bits, the bearer capability
// that lets one container spend from one pool. Treat it as a secret.
func (s *Span) ID() string { return s.id }

// Span opens a child span and writes its span_open row.
//
// parent may be nil for a top-level span. pool may be nil, in which case the
// span inherits its parent's pool, or the sweep's root pool if there is no
// parent. A top-level span's name also becomes its run id, which every
// descendant inherits — "run" is the outermost span in a tree, which for a
// CyberGym sweep is exactly one task.
//
// The id comes from crypto/rand, not from the rowid, a counter, or a hash of
// the name. It is handed to exactly one container in a URL, and the proxy
// grants that URL the right to spend from this span's pool, so an id that could
// be guessed or enumerated is a way for one agent to spend another agent's
// budget and to write rows under another agent's name. rand.Text is 26 base32
// characters, 130 bits, URL-safe, and it cannot fail.
//
// The parent id is written into the row explicitly. Nothing about the tree is
// ever derived from the rowid: rowids are commit order, and commit order is not
// causal order the moment two workers are running.
func (l *Ledger) Span(parent *Span, name string, pool *Pool) *Span {
	s := &Span{l: l, id: rand.Text(), run: name, pool: pool}
	if parent != nil {
		s.parent, s.run = parent.id, parent.run
		if s.pool == nil {
			s.pool = parent.pool
		}
	}
	if s.pool == nil {
		s.pool = l.root
	}
	l.mu.Lock()
	l.spans[s.id] = s
	l.caps[s.id] = s.pool.cap
	l.mu.Unlock()
	l.fail(l.db.Append(store.Row{
		TS: time.Now(), Span: s.id, Parent: s.parent, Run: s.run,
		Kind: "span_open", Outcome: name,
		PriceKnown: true, UsageComplete: true,
	}))
	return s
}

// Env is the entire configuration a container receives.
//
// It is one variable. There is no config file to mount, no token to inject and
// no sidecar to reach: the SDK inside the container appends /v1/messages to
// whatever base URL it is given, so putting the span id in the path means the
// capability survives into every request the agent makes without the agent
// knowing it exists or being able to leave it out.
//
// Note what is NOT here: ANTHROPIC_API_KEY. The real credential lives in the
// supervisor's memory and is attached to the outbound request by the proxy. A
// container that is compromised has nothing to exfiltrate and no second route
// to exfiltrate it over.
func (s *Span) Env() []string {
	return []string{fmt.Sprintf("ANTHROPIC_BASE_URL=%s/s/%s", s.l.base, s.id)}
}

// Fact records something the harness knows and glue does not: the sha256 of a
// submitted crash input, an exit code, a task id. One row, kind='fact', the key
// in outcome and the value verbatim in meta.
//
// Facts are how the report gets built without glue growing an opinion about
// what a harness is doing. glue never derives success from an exit code — the
// class on span_close is written by the harness's own gate, and re-deriving it
// here would put a second, disagreeing judge in the pipeline.
//
// The value is stored as written, not wrapped in a JSON envelope, so a reader
// can CAST(meta AS TEXT) and parse it. Keep values small; this is a ledger, not
// a blob store, and there is deliberately nowhere in v0 to put a blob.
func (s *Span) Fact(key string, v []byte) {
	s.l.fail(s.l.db.Append(store.Row{
		TS: time.Now(), Span: s.id, Parent: s.parent, Run: s.run,
		Kind: "fact", Outcome: key, Meta: string(v),
		PriceKnown: true, UsageComplete: true,
	}))
}

// Close writes the span_close row and revokes the span's bearer capability.
//
// The revocation is the half that is easy to miss. After Close, the proxy no
// longer resolves this span id, so a container that outlives its span — because
// the harness moved on, or because docker was slow to kill it — can still make
// requests and will get 404 for every one of them. Spend cannot outlive the
// span it is attributed to, which is what makes the closed row a final number
// rather than a snapshot.
//
// outcome is free text and is never parsed. Write what happened in the words
// that are true: "no crash in 270m", "patched build also crashed". The class
// beside it is the machine-readable half, and that one is a closed set.
//
// Close is idempotent; a second call does nothing rather than tripping the
// unique index.
func (s *Span) Close(c Class, outcome string) {
	s.closed.Do(func() {
		now := time.Now()
		s.l.mu.Lock()
		delete(s.l.spans, s.id) // the capability dies here
		s.l.mu.Unlock()
		s.l.fail(s.l.db.Append(store.Row{
			TS: now, LastSeen: now, Span: s.id, Parent: s.parent, Run: s.run,
			Kind: "span_close", Class: c, Outcome: outcome,
			PriceKnown: true, UsageComplete: true,
		}))
	})
}

// target is what the proxy is allowed to know about a span. It is called on
// every request; the map lookup is why the hot path never touches the disk.
func (l *Ledger) target(id string) (proxy.Target, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.spans[id]
	if !ok {
		return proxy.Target{}, false
	}
	return proxy.Target{ID: s.id, Parent: s.parent, Run: s.run, Pool: s.pool.name}, true
}

// ------------------------------------------------------------------ pool

// Pool is a spending cap. Pools nest, and a charge against a leaf is charged
// against every ancestor in the same statement, so the sweep cap binds even
// when a task's own cap does not.
//
// Creating a sub-pool does NOT reserve anything from its parent. That is
// deliberate: 188 task pools of $12 against a $2,000 sweep cap is the normal
// shape, because you do not know in advance which tasks are expensive.
// Charging the parent at creation would both double-count and forbid that
// over-subscription — and it would drag in a release-on-close path, a
// stranded-remainder problem, and refund arithmetic. Three edge cases that stop
// existing rather than getting three ifs.
//
// There is deliberately no Remaining, no Spent and no Available method on Pool,
// and the omission is the design, not an oversight.
//
// Any such method hands back a number that is stale the instant it returns. The
// only thing a caller can do with it is decide — "if remaining > estimate, go
// ahead" — and between the read and the request, nineteen other workers have
// each spent from the same pool. That is a textbook time-of-check to
// time-of-use race, and it is worse than usual here because it fails silently
// and in the expensive direction: the cap holds in testing, then quietly does
// not hold under twenty concurrent workers, and the first evidence is the
// invoice.
//
// So there is nothing to read and nothing to race against. Enforcement lives in
// exactly one place, inside the proxy handler, as one conditional UPDATE whose
// guard and whose increment are the same statement — SQLite holds the write
// lock across both, so there is no window at all. The handler checks
// RowsAffected. Zero rows means refused, and the request never leaves the
// process.
//
// A harness that wants to know it is out of budget finds out the way everything
// else finds out: the call comes back 429 and the ledger row reads
// class=Rejected, outcome="budget". That is not an approximation of the answer,
// it IS the answer, and it cannot be wrong by a race.
type Pool struct {
	l    *Ledger
	name string
	cap  USD
}

// Sub creates a child pool. Names are global to the ledger and a repeated name
// is a no-op, so a harness that retries a task does not stack caps.
func (p *Pool) Sub(name string, cap USD) *Pool {
	p.l.fail(p.l.db.NewPool(name, p.name, cap, 0))
	return &Pool{l: p.l, name: name, cap: cap}
}

// Name is the pool's identifier, and Cap is the ceiling it was created with.
// Both are static; neither tells you anything about live spend, on purpose.
func (p *Pool) Name() string { return p.name }
func (p *Pool) Cap() USD     { return p.cap }

// Hold sets this pool's salvage reserve: money inside the cap that cannot be
// spent until Claim releases it. The effective ceiling becomes cap - reserve.
//
// There is no second counter and nothing to keep in sync, because the budget
// statement tests `spent + n <= cap - salvage` directly. Held money is
// unreachable by construction rather than by anyone remembering to subtract it,
// and a task that never reaches its endgame simply never spends it.
func (p *Pool) Hold(reserve USD) {
	p.l.fail(p.l.db.Hold(p.name, reserve))
}

// Claim releases the salvage reserve on this pool, widening its effective cap
// from (cap - salvage) to cap for everything already in flight and everything
// that follows. It reports whether this call is the one that claimed it, so a
// double claim is visible instead of silent.
//
// This is one UPDATE and it needs no second mechanism anywhere, because the
// reserve was never a quantity anyone tracked: the budget statement reads
// `cap - salvage` rather than `cap`, so setting salvage to zero is the entire
// feature. The intended shape is a soft deadline — run the sweep against the
// held-back cap, and at T-minus-whatever call Claim so the endgame has money
// that the earlier, cheaper part of the sweep could not have burned.
func (p *Pool) Claim() bool {
	claimed, err := p.l.db.Claim(p.name)
	p.l.fail(err)
	return claimed
}
