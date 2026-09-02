// Package store is the disk. It owns the only SQLite handle in the system, the
// schema, and every statement anyone issues against it.
//
// It is internal on purpose. A harness that could reach a *sql.DB could write
// its own events rows, and then the ledger is no longer a record of what
// happened — it is a record of what someone claimed happened. The public API
// in package glue exposes no database handle, no statement, and no path.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// USD is dollars, as a float.
//
// Integer micro-cents were considered and rejected. The accumulated
// representation error over a whole sweep is n*eps: 10^5 calls at 2.2e-16 is
// ~1e-11 relative, i.e. 1e-8 dollars on a $1,000 sweep, and it always errs
// toward under-spending because the cap comparison is exact and binds one
// increment early. Integers would buy that back at the cost of a conversion at
// every boundary — the proxy, the price table, the cap, the report.
type USD float64

// Class is the mechanical verdict on a span or a call. It is an int, not a
// string, and the distinction is load-bearing:
//
//   - Unknown is the zero value. A span whose supervisor died before it closed
//     is reaped with class 0 and reads as "we do not know", not as a failure and
//     not as a success. With a string enum the zero value is "", which every
//     reader has to special-case, and half of them will forget.
//   - The set is closed, and closed by the compiler. `class` in the database is
//     the one column a report may branch on; `outcome` next to it is free text,
//     indexed, and never parsed. Making Class a string invites the two to blur
//     until someone writes `WHERE class = 'timeout'` and a five-way partition
//     becomes a hundred-way one.
//   - It costs 8 bytes per row instead of 8 characters, and `GROUP BY class`
//     over a million rows is an integer sort.
//
// Only supervisor-side code ever writes it. The agent cannot set its own class.
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

// Schema is the whole database. There is no migration machinery and there will
// not be one: every statement is CREATE ... IF NOT EXISTS, so Open is
// idempotent, and a sweep's database is a sweep's database. When the shape
// changes, the next sweep gets the new shape and the old file still reads with
// the old binary. A migration framework is a mechanism for evolving state you
// cannot afford to abandon; a sweep's ledger is a record, not a live store.
const Schema = `
CREATE TABLE IF NOT EXISTS events (
  id             INTEGER PRIMARY KEY,   -- rowid alias == COMMIT order, never causal order
  ts             INTEGER NOT NULL,      -- unix millis, wall clock, request start
  sweep          TEXT    NOT NULL,      -- stable across supervisor restarts; the reap scope
  run            TEXT    NOT NULL DEFAULT '',  -- one task; '' for sweep-level rows
  span           TEXT    NOT NULL,      -- 128-bit random hex; also the proxy bearer capability
  parent         TEXT    NOT NULL DEFAULT '',  -- written explicitly, NEVER derived from id
  call_id        TEXT    NOT NULL DEFAULT '',  -- logical call; shared by transport retries
  attempt        INTEGER NOT NULL DEFAULT 0,
  kind           TEXT    NOT NULL,      -- span_open|span_close|call|fact
  class          INTEGER NOT NULL DEFAULT 0,
  outcome        TEXT    NOT NULL DEFAULT '',  -- free text; on span_open it is the span NAME
  model          TEXT    NOT NULL DEFAULT '',
  in_tok         INTEGER NOT NULL DEFAULT 0,
  out_tok        INTEGER NOT NULL DEFAULT 0,
  cache_r        INTEGER NOT NULL DEFAULT 0,
  cache_w        INTEGER NOT NULL DEFAULT 0,
  usd            REAL    NOT NULL DEFAULT 0,   -- priced ON WRITE
  price_known    INTEGER NOT NULL DEFAULT 1,
  usage_complete INTEGER NOT NULL DEFAULT 1,   -- 0 => tokens are a floor, charge is the reservation
  meta           TEXT    NOT NULL DEFAULT '{}',
  last_seen      INTEGER NOT NULL       -- response finish; ts..last_seen is the call's duration
);

-- A Span is TWO rows. These make the database say so, instead of the code
-- promising it. Without them a duplicate span_open silently multiplies that
-- span's subtree in every roll-up that joins opens to descendants.
CREATE UNIQUE INDEX IF NOT EXISTS events_span_open  ON events(span) WHERE kind = 'span_open';
CREATE UNIQUE INDEX IF NOT EXISTS events_span_close ON events(span) WHERE kind = 'span_close';

-- The hot path: call_id inference reads the last call row on a span, once per
-- proxy request. (span, id) makes it a reverse seek with no sort.
CREATE INDEX IF NOT EXISTS events_span_call ON events(span, id) WHERE kind = 'call';

-- Bounds every sweep-scoped query, and serves the boot reap.
CREATE INDEX IF NOT EXISTS events_sweep_kind ON events(sweep, kind);

-- glue top descends the open-span forest by parent.
CREATE INDEX IF NOT EXISTS events_parent_open ON events(parent) WHERE kind = 'span_open';

-- outcome is indexed so the failure partition is a seek. It is still never parsed.
CREATE INDEX IF NOT EXISTS events_outcome ON events(sweep, class, outcome) WHERE kind = 'span_close';

-- The only table in the system that is ever UPDATEd, by exactly three
-- statements: the conditional reserve, the unconditional true-up, and Claim.
CREATE TABLE IF NOT EXISTS pool (
  name    TEXT PRIMARY KEY,
  -- NULL, not '', for a root: the foreign key is the guard against a typo'd
  -- parent silently truncating the reserve chain, which would fail OPEN by
  -- quietly ceasing to charge the sweep cap. NULL satisfies the FK; '' does not.
  parent  TEXT REFERENCES pool(name),
  cap     REAL NOT NULL,
  -- Held back from cap until Claim(). The reserve statement reads
  -- cap - salvage, so the salvage reserve is not a quantity anyone tracks and
  -- claiming it is not a transfer: it is one UPDATE that widens the cap.
  salvage REAL NOT NULL DEFAULT 0,
  spent   REAL NOT NULL DEFAULT 0        -- reserved-or-spent, trued up after each response
);
`

// DB is the one handle.
type DB struct {
	sql   *sql.DB
	lock  *os.File
	sweep string
}

// ErrSweepLocked means another supervisor already owns this sweep id.
var ErrSweepLocked = errors.New("sweep is already running")

// Open takes the sweep lock, opens the database, and applies the schema.
//
// The lock is an flock on a sibling file, held for the process lifetime. It
// exists because the boot reap closes every span this sweep left open, and two
// supervisors on one sweep id would reap each other's live spans. There is no
// TTL, no heartbeat and no reconciler: the kernel drops the lock when the
// process dies, which is the same lease the containers and the proxy
// connections already are. Concurrent sweeps against one database take
// different locks and reap disjoint row sets.
func Open(path, sweep string) (*DB, error) {
	lockPath := filepath.Join(filepath.Dir(path), ".glue-"+sweep+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrSweepLocked, sweep)
	}

	// _txlock=immediate takes the write lock at BEGIN instead of on the first
	// write, so a read-then-write transaction cannot upgrade and deadlock.
	d, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		f.Close()
		return nil, err
	}
	// One connection. database/sql then serializes every goroutine in the
	// supervisor before SQLite ever sees them, so SQLITE_BUSY is structurally
	// impossible in this process and the busy_timeout above is there for the
	// OTHER processes (glue top, glue table) and the WAL checkpointer.
	// Measured: 20 connections is *slower* than 1 against a single writer.
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(Schema); err != nil {
		d.Close()
		f.Close()
		return nil, err
	}
	return &DB{sql: d, lock: f, sweep: sweep}, nil
}

// OpenRead is how glue top and glue table reach the file. Read-only, its own
// connection, no lock: in WAL a reader never blocks the writer and the writer
// never blocks a reader.
func OpenRead(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", path+"?mode=ro&_busy_timeout=10000")
}

func (d *DB) Close() error {
	err := d.sql.Close()
	d.lock.Close() // releasing the flock is the point; the file itself can stay
	return err
}

func (d *DB) SQL() *sql.DB  { return d.sql }
func (d *DB) Sweep() string { return d.sweep }

// Row is one events row. Append is the only way one is created.
type Row struct {
	TS, LastSeen              time.Time
	Run, Span, Parent         string
	CallID                    string
	Attempt                   int
	Kind, Outcome, Model      string
	Class                     Class
	In, Out, CacheR, CacheW   int64
	USD                       USD
	PriceKnown, UsageComplete bool
	Meta                      string // raw JSON; '{}' when empty
}

const insertRow = `
INSERT INTO events (ts, sweep, run, span, parent, call_id, attempt, kind,
                    class, outcome, model, in_tok, out_tok, cache_r, cache_w,
                    usd, price_known, usage_complete, meta, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// Append writes one row. There is no Update anywhere in this package except the
// three pool statements below; events is append-only, and that is what makes a
// concurrent reader safe without a snapshot and a crash recoverable without a
// journal of our own.
func (d *DB) Append(r Row) error {
	if r.LastSeen.IsZero() {
		r.LastSeen = r.TS
	}
	if r.Meta == "" {
		r.Meta = "{}"
	}
	_, err := d.sql.Exec(insertRow,
		r.TS.UnixMilli(), d.sweep, r.Run, r.Span, r.Parent, r.CallID, r.Attempt, r.Kind,
		int(r.Class), r.Outcome, r.Model, r.In, r.Out, r.CacheR, r.CacheW,
		float64(r.USD), b2i(r.PriceKnown), b2i(r.UsageComplete), r.Meta,
		r.LastSeen.UnixMilli())
	return err
}

// ---- pools ---------------------------------------------------------------

// NewPool is one INSERT and it does NOT charge the parent.
//
// Charging the parent at creation would double-count (the parent pays the cap,
// then pays again as the child spends) and would forbid over-subscription — but
// over-subscription is the entire shape of a sweep: 188 task pools of $C
// against one sweep cap far below 188*C, because you do not know in advance
// which tasks are expensive. Enforcing the parent cap only where money actually
// moves also deletes the release-on-close path, the stranded-remainder problem
// and the refund arithmetic. Three edge cases that stop existing instead of
// getting three ifs.
func (d *DB) NewPool(name, parent string, cap, salvage USD) error {
	var par any
	if parent != "" {
		par = parent
	}
	_, err := d.sql.Exec(
		`INSERT OR IGNORE INTO pool(name, parent, cap, salvage) VALUES(?,?,?,?)`,
		name, par, float64(cap), float64(salvage))
	return err
}

// chain walks leaf -> root through pool.parent. Every pool statement is
// expressed against it, so a sub-pool can never be charged without its
// ancestors being charged in the same statement.
const chain = `
WITH RECURSIVE chain(name, parent) AS (
      SELECT name, parent FROM pool WHERE name = ?1
    UNION ALL
      SELECT p.name, p.parent FROM pool p JOIN chain c ON p.name = c.parent
)`

// reserveSQL is the entire budget mechanism.
//
// One statement, so the read and the write are the same statement and SQLite
// holds the write lock across both — there is no window between the guard and
// the increment for a second worker to slip through. The guard subquery is
// uncorrelated with the row being updated, which is what makes it
// all-or-nothing rather than "charge the leaf, then discover the parent is
// full": SQLite compiles it as a scalar subquery evaluated once.
//
// The effective cap is cap - salvage, not cap. That single subtraction is the
// whole salvage-reserve feature; see Claim.
const reserveSQL = chain + `
UPDATE pool SET spent = spent + ?2
 WHERE name IN (SELECT name FROM chain)
   AND (SELECT COUNT(*) FROM pool q
         WHERE q.name IN (SELECT name FROM chain)
           AND q.spent + ?2 > q.cap - q.salvage) = 0`

// Reserve charges amount against pool and every ancestor, or nothing at all.
// It returns the number of pools charged; 0 means refused.
//
// A pool name that does not exist yields an empty chain and therefore 0 rows,
// which reads as refused. Fail-closed by construction, with no extra branch.
func (d *DB) Reserve(pool string, amount USD) (int, error) {
	r, err := d.sql.Exec(reserveSQL, pool, float64(amount))
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	return int(n), err
}

const trueUpSQL = chain + `
UPDATE pool SET spent = spent + ?2 WHERE name IN (SELECT name FROM chain)`

// TrueUp swaps a reservation for the real number: delta is actual - reserved,
// usually negative. It is UNCONDITIONAL, and it is allowed to push spent past
// cap.
//
// That is not a bug to be clamped. The money is already gone — the request was
// forwarded and the tokens were generated. Three things make actual exceed
// reserved: the proxy cannot know before forwarding how the input will split
// across fresh / cache-read / cache-write (a 1-hour cache write costs 2x base
// input), server-side tools bill outside the token math entirely, and the
// pre-forward input estimate is a byte count, not a tokenizer. A ledger that
// clamps is lying about what was spent. The cap's job is to refuse the NEXT
// reserve, and it still does.
//
// Release-on-error is the same statement with actual = 0.
func (d *DB) TrueUp(pool string, delta USD) error {
	if delta == 0 {
		return nil
	}
	_, err := d.sql.Exec(trueUpSQL, pool, float64(delta))
	return err
}

// Hold sets a pool's salvage reserve. The effective cap the reserve statement
// tests against is cap - salvage, so this is the only write the feature needs.
func (d *DB) Hold(pool string, reserve USD) error {
	_, err := d.sql.Exec(`UPDATE pool SET salvage = ? WHERE name = ?`, float64(reserve), pool)
	return err
}

// Claim releases the salvage reserve. It is one UPDATE and it needs no second
// mechanism anywhere: reserveSQL already reads cap - salvage, so dropping
// salvage to zero widens the effective cap for every in-flight and future
// request atomically, with zero branches in the proxy handler.
//
// The `salvage > 0` guard makes the return value mean "this call is what
// claimed it", so a double Claim is visible instead of silent.
func (d *DB) Claim(pool string) (bool, error) {
	r, err := d.sql.Exec(`UPDATE pool SET salvage = 0 WHERE name = ? AND salvage > 0`, pool)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// Spent reports a pool's counters. It is for rendering, never for deciding —
// see the note on the absent Pool.Remaining in package glue.
func (d *DB) Spent(pool string) (spent, cap, salvage USD, err error) {
	err = d.sql.QueryRow(`SELECT spent, cap, salvage FROM pool WHERE name = ?`, pool).
		Scan(&spent, &cap, &salvage)
	return
}

// ---- call_id continuation ------------------------------------------------

// Prev is the last call row on a span.
type Prev struct {
	CallID   string
	Attempt  int
	BodyHash string
	// Retry is the verdict the proxy recorded at the one moment it could see
	// status line, headers and stream together. Reading a stored fact beats
	// re-deriving it from the outcome string, which has already lost the
	// x-should-retry header by the time it is text in a column.
	Retry bool
	// End is when the attempt finished. The retry window is measured from the
	// end, never the start: a stream can burn the client's full timeout before
	// it fails, and the retry half a second later is still the same call.
	End time.Time
}

func (d *DB) LastCall(span string) (Prev, bool) {
	var p Prev
	var retry, end int64
	err := d.sql.QueryRow(`
		SELECT call_id, attempt,
		       COALESCE(json_extract(meta,'$.body'),''),
		       COALESCE(json_extract(meta,'$.retry'),0),
		       last_seen
		  FROM events WHERE span = ? AND kind = 'call'
		 ORDER BY id DESC LIMIT 1`, span).
		Scan(&p.CallID, &p.Attempt, &p.BodyHash, &retry, &end)
	p.Retry, p.End = retry != 0, time.UnixMilli(end)
	return p, err == nil
}

// ---- boot reap -----------------------------------------------------------

// reapSQL closes every span this sweep left open. It is an INSERT, not an
// UPDATE, because events is append-only and the reap obeys that like everything
// else; the synthetic close is distinguishable from a real one by
// outcome='abandoned'. class stays 0 (Unknown) so an abandoned span reads
// honestly instead of reading as a failure.
//
// The NOT EXISTS makes it idempotent, so booting twice reaps once.
const reapSQL = `
INSERT INTO events(ts, sweep, run, span, parent, kind, class, outcome, last_seen)
SELECT ?2, o.sweep, o.run, o.span, o.parent, 'span_close', 0, 'abandoned', ?2
  FROM events o
 WHERE o.sweep = ?1 AND o.kind = 'span_open'
   AND NOT EXISTS (SELECT 1 FROM events c
                    WHERE c.span = o.span AND c.kind = 'span_close')`

// Reap is the database half of crash cleanup. The other half is one docker
// command; there is no third statement.
//
// A crash strands the reservations of in-flight requests in pool.spent, and
// they are deliberately not recovered: the stranding is bounded by (requests in
// flight) * (per-request reserve), it errs toward under-spending, and
// recovering it would mean adding a pool column to the hot write path so a
// reconciliation UPDATE could find it. Ceiling named, cost declined.
func (d *DB) Reap() (int, error) {
	r, err := d.sql.Exec(reapSQL, d.sweep, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	return int(n), err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
