package glue

// ledger.go is the SEAM the proxy writes through, not the finished ledger.
// It contains exactly the schema, queries and types proxy.go needs, so that
// package glue compiles and proxy_test.go exercises real SQL against a real
// SQLite file. The Span/Pool lifecycle (Span, Sub, Fact, Close, boot reap,
// glue top, glue table) belongs to ledger.go's own task and replaces this.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Class is written only by supervisor-side code. Unknown is the zero value so
// an unclosed span reads honestly instead of reading as a failure.
type Class int

const (
	Unknown Class = iota
	OK
	Rejected
	Failed
	Cancelled
)

func (c Class) String() string {
	switch c {
	case OK:
		return "ok"
	case Rejected:
		return "rejected"
	case Failed:
		return "failed"
	case Cancelled:
		return "cancelled"
	}
	return "unknown"
}

const Schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA synchronous=NORMAL;

CREATE TABLE IF NOT EXISTS events (
  id             INTEGER PRIMARY KEY,
  ts             INTEGER NOT NULL,
  sweep          TEXT    NOT NULL,
  run            TEXT    NOT NULL DEFAULT '',
  span           TEXT    NOT NULL,
  parent         TEXT    NOT NULL DEFAULT '',
  call_id        TEXT    NOT NULL DEFAULT '',
  attempt        INTEGER NOT NULL DEFAULT 0,
  kind           TEXT    NOT NULL,
  class          INTEGER NOT NULL DEFAULT 0,
  outcome        TEXT    NOT NULL DEFAULT '',
  model          TEXT    NOT NULL DEFAULT '',
  in_tok         INTEGER NOT NULL DEFAULT 0,
  out_tok        INTEGER NOT NULL DEFAULT 0,
  cache_r        INTEGER NOT NULL DEFAULT 0,
  cache_w        INTEGER NOT NULL DEFAULT 0,
  usd            REAL    NOT NULL DEFAULT 0,
  price_known    INTEGER NOT NULL DEFAULT 1,
  usage_complete INTEGER NOT NULL DEFAULT 1,
  meta           TEXT    NOT NULL DEFAULT '{}',
  last_seen      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_span    ON events(span, id);
CREATE INDEX IF NOT EXISTS events_outcome ON events(outcome);

-- The only table in the system that is UPDATEd, and only by the one
-- conditional statement in Pool.Reserve plus the unconditional true-up.
CREATE TABLE IF NOT EXISTS pool (
  name   TEXT PRIMARY KEY,
  parent TEXT NOT NULL DEFAULT '',
  cap    REAL NOT NULL,
  spent  REAL NOT NULL DEFAULT 0,
  -- Held back from cap until Claim(). The reserve statement reads
  -- cap - salvage, so the reserve is not a quantity anyone tracks.
  salvage REAL NOT NULL DEFAULT 0
);
`

type Ledger struct {
	db    *sql.DB
	sweep string

	mu    sync.RWMutex
	spans map[string]*Span
}

func Open(db *sql.DB, sweep string) (*Ledger, error) {
	if _, err := db.Exec(Schema); err != nil {
		return nil, err
	}
	return &Ledger{db: db, sweep: sweep, spans: map[string]*Span{}}, nil
}

// Span is two rows (span_open, span_close). The proxy only ever reads one.
type Span struct {
	l      *Ledger
	ID     string
	Parent string
	Run    string
	Pool   *Pool
}

// NewSpan mints a span id. The id is a bearer capability handed to exactly one
// container in ANTHROPIC_BASE_URL, so it must be unguessable.
func (l *Ledger) NewSpan(parent *Span, name string, p *Pool) (*Span, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	s := &Span{l: l, ID: hex.EncodeToString(b), Pool: p}
	if parent != nil {
		s.Parent, s.Run = parent.ID, parent.Run
	}
	if err := l.Record(Event{
		TS: time.Now(), Span: s.ID, Parent: s.Parent, Run: s.Run,
		Kind: "span_open", Outcome: name, PriceKnown: true, UsageComplete: true,
	}); err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.spans[s.ID] = s
	l.mu.Unlock()
	return s, nil
}

// Env is what goes into the container. The SDK appends /v1/messages to it.
func (s *Span) Env(listener string) []string {
	return []string{fmt.Sprintf("ANTHROPIC_BASE_URL=http://%s/s/%s", listener, s.ID)}
}

// LookupSpan resolves a bearer capability. In-memory: the supervisor minted
// every live span itself, so this never touches the disk on the hot path.
func (l *Ledger) LookupSpan(id string) *Span {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.spans[id]
}

type Event struct {
	TS, LastSeen              time.Time
	Span, Parent, Run         string
	CallID                    string
	Attempt                   int
	Kind, Outcome, Model      string
	Class                     Class
	In, Out, CacheR, CacheW   int64
	USD                       USD
	PriceKnown, UsageComplete bool
	Meta                      map[string]any

	// MetaRaw writes the meta column verbatim instead of JSON-encoding Meta.
	// Fact needs it: glue table reads a fact's value with CAST(meta AS TEXT)
	// and ParseInt's it, so wrapping "412" in an object breaks the reader.
	MetaRaw string
}

func (l *Ledger) Record(e Event) error {
	if e.LastSeen.IsZero() {
		e.LastSeen = e.TS
	}
	meta := []byte("{}")
	switch {
	case e.MetaRaw != "":
		meta = []byte(e.MetaRaw)
	case e.Meta != nil:
		meta, _ = json.Marshal(e.Meta)
	}
	_, err := l.db.Exec(`
		INSERT INTO events (ts, sweep, run, span, parent, call_id, attempt, kind,
		                    class, outcome, model, in_tok, out_tok, cache_r, cache_w,
		                    usd, price_known, usage_complete, meta, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TS.UnixMilli(), l.sweep, e.Run, e.Span, e.Parent, e.CallID, e.Attempt, e.Kind,
		int(e.Class), e.Outcome, e.Model, e.In, e.Out, e.CacheR, e.CacheW,
		float64(e.USD), boolInt(e.PriceKnown), boolInt(e.UsageComplete), string(meta),
		e.LastSeen.UnixMilli())
	return err
}

// PrevCall is the last call row on a span, for call_id continuation.
type PrevCall struct {
	CallID   string
	Attempt  int
	Outcome  string
	BodyHash string

	// Retry is the verdict the proxy recorded at the one moment it could see
	// the whole answer — status line, headers and stream together. Reading it
	// back beats re-deriving it from Outcome, which has already lost the
	// `x-should-retry` header by the time it is a string in a column.
	Retry bool

	// End is when the attempt finished (last_seen), which is what the retry
	// window is measured from.
	End time.Time
}

func (l *Ledger) LastCall(span string) (PrevCall, bool) {
	var p PrevCall
	var retry, end int64
	err := l.db.QueryRow(`
		SELECT call_id, attempt, outcome,
		       COALESCE(json_extract(meta,'$.body'),''),
		       COALESCE(json_extract(meta,'$.retry'),0),
		       last_seen
		FROM events WHERE span=? AND kind='call' ORDER BY id DESC LIMIT 1`,
		span).Scan(&p.CallID, &p.Attempt, &p.Outcome, &p.BodyHash, &retry, &end)
	p.Retry, p.End = retry != 0, time.UnixMilli(end)
	return p, err == nil
}

// ---- pool ----------------------------------------------------------------

type Pool struct {
	l      *Ledger
	Name   string
	parent *Pool
}

var ErrOverBudget = errors.New("over budget")

func (l *Ledger) Root(name string, cap USD) (*Pool, error) {
	_, err := l.db.Exec(`INSERT OR IGNORE INTO pool(name,parent,cap) VALUES(?,'',?)`, name, float64(cap))
	return &Pool{l: l, Name: name}, err
}

func (p *Pool) Sub(name string, cap USD) (*Pool, error) {
	_, err := p.l.db.Exec(`INSERT OR IGNORE INTO pool(name,parent,cap) VALUES(?,?,?)`, name, p.Name, float64(cap))
	return &Pool{l: p.l, Name: name, parent: p}, err
}

// Reserve is the whole of budget enforcement: one conditional UPDATE per pool
// in the chain, checked by RowsAffected. There is no Remaining() to race
// against, and no lease to expire.
func (p *Pool) Reserve(amount USD) error {
	if amount <= 0 {
		return nil
	}
	var done []*Pool
	for cur := p; cur != nil; cur = cur.parent {
		r, err := cur.l.db.Exec(
			`UPDATE pool SET spent = spent + ? WHERE name = ? AND spent + ? <= cap - salvage`,
			float64(amount), cur.Name, float64(amount))
		n := int64(0)
		if err == nil {
			n, err = r.RowsAffected()
		}
		if err != nil || n == 0 {
			for _, d := range done {
				d.settleOne(amount, 0)
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: pool %q", ErrOverBudget, cur.Name)
		}
		done = append(done, cur)
	}
	return nil
}

// Settle swaps a reservation for the real number. It is unconditional: the
// money is already spent, so the cap cannot veto the correction.
func (p *Pool) Settle(reserved, actual USD) {
	for cur := p; cur != nil; cur = cur.parent {
		cur.settleOne(reserved, actual)
	}
}

func (p *Pool) settleOne(reserved, actual USD) {
	if reserved == actual {
		return
	}
	p.l.db.Exec(`UPDATE pool SET spent = spent - ? + ? WHERE name = ?`,
		float64(reserved), float64(actual), p.Name)
}

func (p *Pool) Spent() (spent, cap USD) {
	p.l.db.QueryRow(`SELECT spent, cap FROM pool WHERE name=?`, p.Name).Scan(&spent, &cap)
	return
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
