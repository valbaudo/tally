package tui

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/dawn/internal/store"
)

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
func scratch(t *testing.T) *store.DB {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), "glue.db"), "7f3a91c2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestLoadRollsSubtreeTotalsUpTheParentChain(t *testing.T) {
	l := scratch(t)
	now := time.Now()
	n := 0
	rec := func(span, parent, kind, outcome string, in, out int64, usd store.USD) {
		n++
		if err := l.Append(store.Row{
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

	all, err := Load(l.SQL(), "7f3a91c2", map[string]store.USD{"root": 6})
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

// Printed, not asserted: `go test -run Render -v` is how the layout gets looked
// at. A golden file would freeze columns that are still being chosen.
func TestRenderDemo(t *testing.T) {
	if os.Getenv("GLUE_DEMO") == "" && !testing.Verbose() {
		t.Skip("set -v or GLUE_DEMO=1 to print the frames")
	}
	fmt.Println(strings.Join(demoFrame().Lines(120), "\n"))
}

// Verdicts are counted on leaves, not on roots. Under repo -> stage -> hunters
// the old reader showed 0/0/0/0 until an entire repo closed, so fifty failed
// hunters were invisible for hours. And an Unknown span — a reaped one —
// incremented Tasks without landing in any bucket, so the counters silently
// stopped summing.
func TestSnapshotCountsLeavesAndHasABucketForUnknown(t *testing.T) {
	closed := demoNow.Add(-time.Minute)
	repo := &Node{Span: "repo", Closed: closed, Class: store.OK, Spend: 9, Calls: 6}
	stage := &Node{Span: "hunt", Parent: "repo", Closed: closed, Class: store.OK}
	kids := []*Node{
		{Span: "h1", Parent: "hunt", Closed: closed, Class: store.OK},
		{Span: "h2", Parent: "hunt", Closed: closed, Class: store.Failed},
		{Span: "h3", Parent: "hunt", Closed: closed, Class: store.Unknown},
		{Span: "h4", Parent: "hunt"}, // still open: not counted at all
	}
	stage.kids = kids
	repo.kids = []*Node{stage}
	all := append([]*Node{repo, stage}, kids...)

	f := Snapshot(Frame{}, all, NewWatch(), nil, demoNow)
	if f.Tasks != 3 || f.OK != 1 || f.Failed != 1 || f.Unk != 1 {
		t.Fatalf("leaves: tasks=%d ok=%d fail=%d unk=%d", f.Tasks, f.OK, f.Failed, f.Unk)
	}
	if f.OK+f.Rejected+f.Failed+f.Cancelled+f.Unk != f.Tasks {
		t.Fatalf("counters do not sum to tasks: %+v", f)
	}
	// Money still rolls up from roots only, or the repo's spend is counted
	// once per level of the tree.
	if f.Spend != 9 || f.Calls != 6 {
		t.Fatalf("rollup double-counted: spend=%v calls=%d", f.Spend, f.Calls)
	}
}
