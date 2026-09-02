package glue_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	glue "github.com/valbaudo/dawn"
)

// This file is an EXTERNAL test package (glue_test, not glue), on purpose: it
// can reach exactly what a harness can reach and nothing else. If any assertion
// here needed an unexported field, the API would be missing something; if it
// could reach internal/, the layout would not be doing its job.

func upstreamOK(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.Header.Get("X-Api-Key"); k != "sk-real" {
			t.Errorf("proxy did not attach the real key, got %q", k)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-opus-5","usage":{"input_tokens":1000,"output_tokens":100}}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// open brings up a real supervisor: real SQLite, real listener, real proxy. No
// Docker, because Config.Isolate is false and a sweep that launches no
// containers creates no container network.
func open(t *testing.T, id string, budget, salvage glue.USD, up string) *glue.Sweep {
	t.Helper()
	sw, err := glue.Open(glue.Config{
		Sweep: id, DB: filepath.Join(t.TempDir(), "glue.db"), Keys: map[string]string{"anthropic": "sk-real"},
		Budget: budget, Salvage: salvage, Upstream: map[string]string{"anthropic": up},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	go sw.Serve(ctx)
	t.Cleanup(func() { stop(); sw.Close() })
	return sw
}

// peek is the only door into the ledger a harness has, and it is one-way: the
// same read-only handle `glue table` opens from another process.
func peek(t *testing.T, sw *glue.Sweep) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", sw.Path()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// call makes one request the way an agent's SDK would: it takes the span's
// ANTHROPIC_BASE_URL verbatim out of Env and appends /v1/messages, because that
// is the entire mechanism by which the span id survives into the request.
func call(t *testing.T, s *glue.Span, body string) *http.Response {
	t.Helper()
	base := strings.TrimPrefix(s.Env()[0], "ANTHROPIC_BASE_URL=")
	res, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

const req = `{"model":"claude-opus-5","max_tokens":100,"messages":[]}`

// A span is two rows, never one that gets updated, and the open row survives
// the close.
func TestSpanIsTwoRows(t *testing.T) {
	sw := open(t, "s-two", 100, 0, upstreamOK(t).URL)
	sp := sw.Span(nil, "task", nil)
	sp.Close(glue.OK, "found it")
	sp.Close(glue.Failed, "second close must be ignored")

	rows, err := peek(t, sw).Query(
		`SELECT kind, class, outcome FROM events WHERE span=? ORDER BY id`, sp.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, outcome string
		var class int
		if err := rows.Scan(&kind, &class, &outcome); err != nil {
			t.Fatal(err)
		}
		got = append(got, kind+"/"+glue.Class(class).String()+"/"+outcome)
	}
	want := []string{"span_open/unknown/task", "span_close/ok/found it"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

// The span id is a bearer capability in a URL. Two spans must not be able to
// predict each other, and Close must revoke.
func TestSpanIDIsUnguessableAndCloseRevokesIt(t *testing.T) {
	sw := open(t, "s-cap", 100, 0, upstreamOK(t).URL)
	seen := map[string]bool{}
	for range 200 {
		id := sw.Span(nil, "w", nil).ID()
		if len(id) < 20 {
			t.Fatalf("span id %q is too short to be a capability", id)
		}
		if seen[id] {
			t.Fatalf("span id collision on %q", id)
		}
		seen[id] = true
	}

	sp := sw.Span(nil, "revoke", nil)
	if res := call(t, sp, req); res.StatusCode != 200 {
		t.Fatalf("live span: status %d", res.StatusCode)
	}
	sp.Close(glue.OK, "")
	if res := call(t, sp, req); res.StatusCode != 404 {
		t.Fatalf("a closed span still spends: status %d", res.StatusCode)
	}
}

// The container is handed one variable and no credential.
func TestEnvCarriesTheCapabilityAndNoKey(t *testing.T) {
	sw := open(t, "s-env", 100, 0, upstreamOK(t).URL)
	env := sw.Span(nil, "w", nil).Env()
	if len(env) != 1 || !strings.HasPrefix(env[0], "ANTHROPIC_BASE_URL=http://") {
		t.Fatalf("env = %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "sk-real") || strings.Contains(strings.ToUpper(e), "API_KEY") {
			t.Fatalf("a credential reached the container: %q", e)
		}
	}
}

// Budget enforcement is inside the handler, so it holds under concurrency and
// the refused request never leaves the process. The parent cap binds even
// though the child's is enormous.
func TestBudgetBindsAtTheRootUnderConcurrency(t *testing.T) {
	var reached int64
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":1}}`)
	}))
	defer up.Close()

	// Root cap 0.01, child cap 1000. A non-atomic chain would charge the child
	// and let everything through.
	sw := open(t, "s-budget", 0.01, 0, up.URL)
	pool := sw.Budget().Sub("task", 1000)

	var wg sync.WaitGroup
	var ok, refused int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				res := call(t, sw.Span(nil, "w", pool), req)
				mu.Lock()
				if res.StatusCode == 200 {
					ok++
				} else if res.StatusCode == 429 {
					refused++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok == 0 || refused == 0 {
		t.Fatalf("want both outcomes, got ok=%d refused=%d", ok, refused)
	}
	if reached != ok {
		t.Fatalf("%d requests reached the upstream but only %d were allowed: a refusal leaked", reached, ok)
	}

	var spent float64
	if err := peek(t, sw).QueryRow(`SELECT SUM(usd) FROM events WHERE kind='call' AND class=?`,
		int(glue.OK)).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if spent > 0.01 {
		t.Fatalf("root cap crossed: %v spent against a 0.01 cap", spent)
	}

	// Rejected, not Failed: the substrate refused it, no gate did.
	var n int
	if err := peek(t, sw).QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind='call' AND class=? AND outcome='budget'`,
		int(glue.Rejected)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if int64(n) != refused {
		t.Fatalf("%d refusals but %d Rejected rows", refused, n)
	}
}

// Hold makes money unreachable; Claim makes it reachable, once.
func TestSalvageIsHeldThenClaimed(t *testing.T) {
	sw := open(t, "s-salvage", 100, 0, upstreamOK(t).URL)
	p := sw.Budget().Sub("task", 0.01)
	p.Hold(0.0099) // effectively nothing spendable

	sp := sw.Span(nil, "w", p)
	if res := call(t, sp, req); res.StatusCode != 429 {
		t.Fatalf("held money was spendable: status %d", res.StatusCode)
	}
	if !p.Claim() {
		t.Fatal("Claim reported nothing held")
	}
	if p.Claim() {
		t.Fatal("a second Claim must report that it claimed nothing")
	}
	if res := call(t, sw.Span(nil, "w", p), req); res.StatusCode != 200 {
		t.Fatalf("claimed reserve is still unspendable: status %d", res.StatusCode)
	}
}

// Outcomes is the harness's whole read: facts verbatim, in write order, with
// duplicate keys kept, joined to the span's spend — and no database handle
// anywhere in the harness.
//
// The duplicate-key half is the part that used to be broken. A stage that
// streams N findings wrote N rows and the only reader folded them into a map,
// so it showed the last one. Cloudflare's whole reason for streaming findings
// is that a crash costs the task in flight and nothing else; a reader that
// keeps one of forty gives that back.
func TestOutcomesKeepEveryFactInWriteOrder(t *testing.T) {
	sw := open(t, "s-report", 100, 0, upstreamOK(t).URL)
	sp := sw.Span(nil, "hunt/uaf", sw.Budget().Sub("hunt/uaf", 5))
	call(t, sp, req)
	for i := range 40 {
		sp.Fact("finding", fmt.Appendf(nil, "f%02d", i))
	}
	sp.Fact("poc.bytes", []byte("412"))
	sp.Close(glue.OK, "")

	outs, err := sw.Outcomes()
	if err != nil {
		t.Fatal(err)
	}
	var got *glue.Outcome
	for i := range outs {
		if outs[i].Span == sp.ID() {
			got = &outs[i]
		}
	}
	if got == nil {
		t.Fatal("the span that was just closed is not in Outcomes")
	}
	var findings []string
	for _, f := range got.Facts {
		if f.Key == "finding" {
			findings = append(findings, f.Value)
		}
	}
	if len(findings) != 40 {
		t.Fatalf("kept %d of 40 findings", len(findings))
	}
	if findings[0] != "f00" || findings[39] != "f39" {
		t.Fatalf("write order lost: %q .. %q", findings[0], findings[39])
	}
	if got.Name != "hunt/uaf" || got.Run != "hunt/uaf" || got.Class != glue.OK {
		t.Fatalf("span identity lost: %+v", *got)
	}
	if got.Spend <= 0 || got.Calls != 1 {
		t.Fatalf("spend did not join: %v over %d calls", got.Spend, got.Calls)
	}
	if err := sw.Err(); err != nil {
		t.Fatalf("ledger error: %v", err)
	}
}

// A second supervisor on one sweep id would reap the first one's live spans.
// The kernel refuses it; there is no lease to expire and no TTL to tune.
func TestTwoSupervisorsOnOneSweepIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := glue.Config{Sweep: "dup", DB: filepath.Join(dir, "glue.db"), Keys: map[string]string{"anthropic": "k"}}
	first, err := glue.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := glue.Open(cfg); err == nil {
		second.Close()
		t.Fatal("a second supervisor took the same sweep id")
	}
}

// The boot reap closes what a crash left open, as Unknown — not as a failure.
func TestBootReapClosesAbandonedSpans(t *testing.T) {
	dir := t.TempDir()
	cfg := glue.Config{Sweep: "crash", DB: filepath.Join(dir, "glue.db"), Keys: map[string]string{"anthropic": "k"}}
	first, err := glue.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := first.Span(nil, "task", nil).ID()
	first.Close() // die without closing the span

	second, err := glue.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	var class int
	var outcome string
	if err := peek(t, second).QueryRow(
		`SELECT class, outcome FROM events WHERE span=? AND kind='span_close'`, id).
		Scan(&class, &outcome); err != nil {
		t.Fatal(err)
	}
	if glue.Class(class) != glue.Unknown || outcome != "abandoned" {
		t.Fatalf("reaped span reads %v/%q, want unknown/abandoned", glue.Class(class), outcome)
	}
}

// A sweep that declares no container boundary launches nothing — neither an
// agent nor a validator — and says so rather than half-working.
func TestContainersWithoutIsolateAreAnError(t *testing.T) {
	sw := open(t, "s-noimage", 1, 0, upstreamOK(t).URL)
	sp := sw.Span(nil, "w", nil)
	if _, err := sw.Launch(sp, "img", nil, nil, "agent"); err == nil {
		t.Fatal("Launch: want an error")
	}
	if _, _, err := sw.Sandbox(context.Background(), sp, "img", nil, "/poc"); err == nil {
		t.Fatal("Sandbox: want an error")
	}
}
