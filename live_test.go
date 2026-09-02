package glue

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ---- call_id: the three gaps the outcome-string rule could not see ---------

// A 500 stamped `x-should-retry: false` is permanent. _should_retry checks that
// header before it looks at the status code, so the SDK will NOT resend — and
// an identical body arriving later is a new logical call, not attempt 2.
func TestShouldRetryFalseOnA500SplitsTheCall(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Should-Retry", "false")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"nope"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	post(t, f.base(), streamReq).Body.Close()

	rs := f.waitRows(2)
	if len(rs) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rs))
	}
	if rs[0].CallID == rs[1].CallID {
		t.Fatalf("x-should-retry:false was ignored: both attempts got call_id %s", rs[0].CallID)
	}
	if rs[1].Attempt != 1 {
		t.Fatalf("second call should be attempt 1, got %d", rs[1].Attempt)
	}
}

// The mirror image: a 400 stamped `x-should-retry: true`. The status code says
// permanent, the header says resend, and the header wins.
func TestShouldRetryTrueOnA400MergesTheCall(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Should-Retry", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	post(t, f.base(), streamReq).Body.Close()

	rs := f.waitRows(2)
	if len(rs) != 2 || rs[0].CallID != rs[1].CallID {
		t.Fatalf("header-forced retry did not merge: %+v", rs)
	}
	if rs[1].Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", rs[1].Attempt)
	}
}

// 409 conflict_error. _should_retry retries it ("Retry on lock timeouts"), and
// the old outcome-string list did not contain it.
func TestConflictIsRetryable(t *testing.T) {
	if !willRetry(409, http.Header{}) {
		t.Fatal("409 must be retryable: _should_retry retries it explicitly")
	}
	for _, s := range []int{408, 429, 500, 502, 529} {
		if !willRetry(s, http.Header{}) {
			t.Fatalf("%d must be retryable", s)
		}
	}
	for _, s := range []int{200, 400, 401, 402, 403, 404, 413} {
		if willRetry(s, http.Header{}) {
			t.Fatalf("%d must not be retryable", s)
		}
	}
}

// The window. An agent that fails a call, then spends twenty minutes compiling
// and running a fuzz target, then resends the identical body is starting a new
// logical call — the SDK gave up long ago. Without a clock the old rule welded
// them together and COUNT(DISTINCT call_id) under-reported forever.
func TestRetryWindowExpires(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"o"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	f.waitRows(1)

	// Age the recorded attempt past the window. last_seen is what identify
	// measures from, which is exactly why it is the column it reads.
	old := time.Now().Add(-retryWindow - time.Second).UnixMilli()
	if _, err := f.l.db.Exec(`UPDATE events SET last_seen=? WHERE kind='call'`, old); err != nil {
		t.Fatal(err)
	}

	post(t, f.base(), streamReq).Body.Close()
	rs := f.waitRows(2)
	if len(rs) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rs))
	}
	if rs[0].CallID == rs[1].CallID {
		t.Fatal("a retryable failure older than retryWindow must not be joined")
	}
}

// Inside the window the same failure does merge — the window narrows the rule,
// it does not disable it.
func TestRetryInsideWindowStillMerges(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"o"}}`))
	})
	post(t, f.base(), streamReq).Body.Close()
	f.waitRows(1)
	post(t, f.base(), streamReq).Body.Close()

	rs := f.waitRows(2)
	if len(rs) != 2 || rs[0].CallID != rs[1].CallID || rs[1].Attempt != 2 {
		t.Fatalf("529 inside the window must merge as attempt 2: %+v", rs)
	}
}

// A refused budget must not cost three round trips. We mint the 429 ourselves,
// so we also get to tell the SDK not to bother.
func TestBudgetRefusalTellsTheSDKNotToRetry(t *testing.T) {
	f := newFixture(t, 0, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached when the budget refuses")
	})
	res := post(t, f.base(), streamReq)
	defer res.Body.Close()
	if got := res.Header.Get("X-Should-Retry"); got != "false" {
		t.Fatalf("want x-should-retry: false on a budget refusal, got %q", got)
	}
}

// ---- staleness ------------------------------------------------------------

// The whole point: an in-flight request proves liveness on its own, so a call
// that runs far past staleAfter is never flagged. No threshold is tuned to
// model latency, because none has to be.
func TestInFlightNeverGoesStale(t *testing.T) {
	w := NewWatch()
	long := time.Now().Add(-20 * time.Minute) // an Opus call with extended thinking
	if st, _ := w.State("c1", long, 1, time.Now()); st != Live {
		t.Fatalf("a span with a request in flight must be Live, got %v", st)
	}
	if st, _ := w.State("c1", long, 0, time.Now()); st != Stale {
		t.Fatalf("the same span with nothing in flight must be Stale, got %v", st)
	}
}

// CPU movement is the third signal: an agent compiling a fuzz target between
// LLM calls writes no events and holds no request, and must still read alive.
func TestCPUMovementBeatsSilence(t *testing.T) {
	now := time.Now()
	w := NewWatch()
	w.cpu[container("c1")] = sample{ns: 42, moved: now.Add(-5 * time.Second)}

	quiet := now.Add(-10 * time.Minute) // nothing written for ten minutes
	st, _ := w.State("c1", quiet, 0, now)
	if st != Busy {
		t.Fatalf("recent CPU must read Busy, got %v", st)
	}

	w.cpu[container("c1")] = sample{ns: 42, moved: now.Add(-staleAfter - time.Second)}
	if st, _ := w.State("c1", quiet, 0, now); st != Stale {
		t.Fatalf("CPU stopped past the threshold must read Stale, got %v", st)
	}

	w.cpu[container("c1")] = sample{gone: true}
	if st, _ := w.State("c1", quiet, 0, now); st != Gone {
		t.Fatalf("a vanished container must read Gone, got %v", st)
	}
}

// A 404 from the stats endpoint is the container being gone, not an error.
func TestPollMarksMissingContainerGone(t *testing.T) {
	w := NewWatch()
	w.http.Transport = roundTrip(func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: http.Header{}}
	})
	w.Poll(t.Context(), []string{"c1"})
	if !w.cpu[container("c1")].gone {
		t.Fatal("404 from /stats must mark the container gone")
	}
}

func TestPollReadsTotalUsage(t *testing.T) {
	w := NewWatch()
	var ns uint64 = 1000
	w.http.Transport = roundTrip(func(r *http.Request) *http.Response {
		if q := r.URL.Query(); q.Get("stream") != "false" || q.Get("one-shot") != "true" {
			t.Errorf("want stream=false&one-shot=true, got %q", r.URL.RawQuery)
		}
		body := fmt.Sprintf(`{"cpu_stats":{"cpu_usage":{"total_usage":%d}}}`, ns)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	})

	w.Poll(t.Context(), []string{"c1"})
	first := w.cpu[container("c1")]
	if first.ns != 1000 || first.moved.IsZero() {
		t.Fatalf("first sample not recorded: %+v", first)
	}

	// A poll where the counter did not move must not refresh `moved`.
	w.Poll(t.Context(), []string{"c1"})
	if got := w.cpu[container("c1")]; !got.moved.Equal(first.moved) {
		t.Fatal("an unchanged CPU counter must not count as movement")
	}

	ns = 2000
	w.Poll(t.Context(), []string{"c1"})
	if got := w.cpu[container("c1")]; !got.moved.After(first.moved) || got.ns != 2000 {
		t.Fatalf("an advancing counter must refresh moved: %+v", got)
	}
}

type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

// ---- renderers ------------------------------------------------------------

// scratch builds a real SQLite ledger and returns it, so the renderers are
// exercised against the actual schema and the actual SQL, not a hand-built
// struct literal.
func scratch(t *testing.T) *Ledger {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "glue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	l, err := Open(db, "7f3a91c2")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLoadRollsSubtreeTotalsUpTheParentChain(t *testing.T) {
	l := scratch(t)
	now := time.Now()
	n := 0
	rec := func(span, parent, kind, outcome string, in, out int64, usd USD) {
		n++
		if err := l.Record(Event{
			TS: now, LastSeen: now, Span: span, Parent: parent, Kind: kind,
			Outcome: outcome, CallID: fmt.Sprintf("call-%d", n), Attempt: 1,
			Model: "claude-opus-5", In: in, Out: out, USD: usd,
			PriceKnown: true, UsageComplete: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec("root", "", "span_open", "w00 arvo:10400", 0, 0, 0)
	rec("kid", "root", "span_open", "salvage", 0, 0, 0)
	rec("kid", "root", "call", "", 1000, 100, 1.25)
	rec("kid", "root", "call", "", 2000, 200, 2.50)

	all, err := Load(l.db, "7f3a91c2", map[string]USD{"root": 6})
	if err != nil {
		t.Fatal(err)
	}
	var root *Node
	for _, n := range all {
		if n.Span == "root" {
			root = n
		}
	}
	if root == nil {
		t.Fatal("no root node")
	}
	if root.In != 3000 || root.Out != 300 {
		t.Fatalf("subtree tokens not rolled up: in=%d out=%d", root.In, root.Out)
	}
	if root.Spend != 3.75 {
		t.Fatalf("subtree spend not rolled up: %v", root.Spend)
	}
	if root.Calls != 2 {
		t.Fatalf("want 2 distinct call_ids in the subtree, got %d", root.Calls)
	}
	if root.Cap != 6 {
		t.Fatalf("cap not applied: %v", root.Cap)
	}
}

// Every rendered line must be exactly the frame width, or the columns lie.
func TestFrameLinesAreFlush(t *testing.T) {
	f := demoFrame()
	for _, w := range []int{100, 120, 160} {
		for i, l := range f.Lines(w) {
			if n := len([]rune(l)); n > w {
				t.Fatalf("width %d: line %d overflows at %d runes:\n%s", w, i, n, l)
			}
		}
	}
}

func TestTableCSVIsMachineReadable(t *testing.T) {
	var b strings.Builder
	if err := TableCSV(&b, demoRows()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != len(demoRows())+1 {
		t.Fatalf("want header + %d rows, got %d lines", len(demoRows()), len(lines))
	}
	want := 16
	for i, l := range lines {
		// The outcome column is quoted, so count fields outside quotes.
		n, inQ := 1, false
		for _, c := range l {
			switch {
			case c == '"':
				inQ = !inQ
			case c == ',' && !inQ:
				n++
			}
		}
		if n != want {
			t.Fatalf("line %d has %d fields, want %d:\n%s", i, n, want, l)
		}
	}
}

// Printed, not asserted: `go test -run Render -v` is how the layout gets looked
// at. A golden file would freeze columns that are still being chosen.
func TestRenderDemo(t *testing.T) {
	if os.Getenv("GLUE_DEMO") == "" && !testing.Verbose() {
		t.Skip("set -v or GLUE_DEMO=1 to print the frames")
	}
	fmt.Println(strings.Join(demoFrame().Lines(120), "\n"))
	fmt.Println()
	fmt.Println(strings.Join(TableLines("7f3a91c2", demoRows()), "\n"))
}
