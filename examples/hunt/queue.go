package main

import (
	"database/sql"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------- the queue

// queue is the harness's work queue, and the fact that it is HERE — thirty-odd
// lines in the harness's own file, not a symbol in dawn — is the answer to the
// question this example exists to ask.
//
// # Why it is not dawn's
//
// Of the six sequencers on record, exactly one needs a queue. MDASH is a
// five-stage forward pipeline; Gcsa is a state machine over one state.json with
// one worker; VARAS is nine stages and one artifact each; RedbudAI is a single
// bounded loop; this harness's own recon/validate/dedupe are three ordinary
// function calls in main. All five need only "which cells have no span that
// closed OK", which is Sweep.Outcomes and needs no mutable row anywhere.
//
// Cloudflare's Glasswing needs one, for exactly one reason, and it is neither
// resume nor parallelism: three of its eight stages AUTHOR WORK DURING THE RUN.
// Gapfill enqueues cells nobody covered, Tracer enqueues into a consumer repo,
// Feedback rewrites the prompt of a task no worker has read yet. An unclaimed
// task is a span that has not been opened, and an append-only ledger has no row
// for a span that has not been opened. That is the whole gap, it is one of six,
// and dawn's own membership rule ("if the 19 harnesses disagree about it, it is
// your code") points at this file.
//
// # What that costs, and what it does not
//
// It costs the two-phase gap named at drain() and it costs `glue top` any view
// of queue depth. It does NOT cost the join between a work item and its money,
// which is the one thing worth having: the task key IS the span name, so "what
// did this cell cost across every attempt, including the ones a crash
// abandoned" is a group-by over Sweep.Outcomes — see report(). The same join in
// SQL is two keywords, because dawn ships the ledger as a PATH and turns WAL on
// (sweep.go:279, store.go:171):
//
//	ATTACH DATABASE 'file:hunt.db?mode=ro' AS led;
//	SELECT t.key, t.state, round(sum(e.usd),2) FROM task t
//	  JOIN led.events e ON e.outcome = t.key AND e.kind = 'span_open' ...
//
// # The lease, which is not a lease
//
// There is no lease column, no TTL and no reaper, and that is not a shortcut —
// it is dawn's own argument applied one layer up. A claimed task is abandoned
// only if the process holding it died, because a live worker always writes
// done() on its way out. This queue opens after glue.Open has taken the sweep's
// flock (store.go:157), so exactly one process can hold claims against it, and
// the kernel drops that flock when the process dies. So the requeue is one
// statement at open, like store.Reap, and a healthy worker on a twelve-minute
// extended-thinking call can never be evicted for being slow — which is the one
// failure mode a TTL would add and could not remove.
const queueSchema = `
CREATE TABLE IF NOT EXISTS task (
  key     TEXT PRIMARY KEY,             -- the cell; also the span name and the pool name
  payload TEXT NOT NULL,                -- a taxon, as JSON. Feedback rewrites THIS.
  state   TEXT NOT NULL DEFAULT 'queued',
  tries   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS task_ready ON task(state, key);`

// maxTries is the poison guard. Without it a task that kills the supervisor
// comes back through the boot requeue forever, at full worker concurrency,
// and the invoice is the first symptom.
const maxTries = 3

type queue struct{ db *sql.DB }

func openQueue(path string) (*queue, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=10000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(queueSchema); err != nil {
		db.Close()
		return nil, err
	}
	// The boot requeue. Every claim in this file was held by a process that is
	// no longer running, because the flock says so.
	if _, err := db.Exec(`UPDATE task SET state = 'queued' WHERE state = 'running'`); err != nil {
		db.Close()
		return nil, err
	}
	return &queue{db}, nil
}

func (q *queue) close() error { return q.db.Close() }

// push offers a cell, or sharpens one nobody has started yet.
//
// One statement, two of Cloudflare's published stages. The INSERT is Gapfill
// enqueueing a cell that did not exist when the run began; the DO UPDATE is
// Feedback rewriting a task's payload "to make future tasks sharper". The
// `WHERE state = 'queued'` is not decoration — it is what makes "rewrites
// QUEUED prompts" true instead of a race against a worker whose request is
// already in flight, and it also makes a re-offer of finished work a no-op, so
// Gapfill is idempotent for free.
//
// What is rewritten is the taxon, not a rendered prompt string. Fifty hunters
// sharing one template and differing by five typed fields is fifty rows to
// schedule and one template to fix; fifty rendered prompts is fifty drifting
// copies of the same threat-model clause, and two of them do not diff.
func (q *queue) push(key string, payload []byte) error {
	_, err := q.db.Exec(`INSERT INTO task(key, payload) VALUES(?, ?)
	  ON CONFLICT(key) DO UPDATE SET payload = excluded.payload WHERE state = 'queued'`,
		key, payload)
	return err
}

// take claims the next cell, or returns sql.ErrNoRows when the queue is drained.
//
// The guard and the write are the same statement, which is the entire
// concurrency argument — the same one store.Reserve makes (store.go:302).
// SQLite holds the write lock across both, so there is no window between
// "which key is queued" and "it is mine now" for a second worker to slip
// through, at eight workers or at two hundred.
func (q *queue) take() (key string, payload []byte, tries int, err error) {
	err = q.db.QueryRow(`
	  UPDATE task SET state = 'running', tries = tries + 1
	   WHERE key = (SELECT key FROM task
	                 WHERE state = 'queued' AND tries < ?
	                 ORDER BY key LIMIT 1)
	  RETURNING key, payload, tries`, maxTries).Scan(&key, &payload, &tries)
	return
}

// done retires a claim. state is the harness's word, never dawn's: whether a
// failed cell deserves another try is retry policy, and retry policy is a line
// in gapfill() or nothing at all.
func (q *queue) done(key, state string) error {
	_, err := q.db.Exec(`UPDATE task SET state = ? WHERE key = ?`, state, key)
	return err
}

// task is one row, for the report and for sizing the per-cell budget.
type task struct {
	Key, State string
	Tries      int
}

func (q *queue) list() ([]task, error) {
	rows, err := q.db.Query(`SELECT key, state, tries FROM task ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []task
	for rows.Next() {
		var t task
		if err := rows.Scan(&t.Key, &t.State, &t.Tries); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// queued is how many cells are still to run. It sizes the per-cell pool at the
// top of a drain, which is why it is a count and not a bool.
func (q *queue) queued() (int, error) {
	var n int
	err := q.db.QueryRow(`SELECT count(*) FROM task WHERE state = 'queued' AND tries < ?`,
		maxTries).Scan(&n)
	return n, err
}
