package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	glue "github.com/valbaudo/dawn"
)

// A verbatim-shaped ASAN report. The four subprocess dimensions all read one of
// these, so if the parse is wrong every mechanical dimension is wrong at once.
const report = `=================================================================
==12==ERROR: AddressSanitizer: heap-buffer-overflow on address 0x60300000ef95
READ of size 1 at 0x60300000ef95 thread T0
    #0 0x4f1a2a in png_handle_iCCP /src/libpng/pngrutil.c:1447:7
    #1 0x4e1b3c in png_read_info /src/libpng/pngread.c:148:10
SUMMARY: AddressSanitizer: heap-buffer-overflow /src/libpng/pngrutil.c:1447:7
`

// primed builds a run whose repro() is already cached, so the mechanical
// dimensions can be tested without a docker daemon.
func primed(c Claim) *run {
	r := &run{claim: c}
	r.once.Do(func() { r.report = report })
	return r
}

func TestMechanicalDimensionsReadTheReport(t *testing.T) {
	honest := Claim{TargetFile: "pngrutil.c", CrashType: "heap buffer overflow"}

	d, err := targetFile(context.Background(), primed(honest), nil)
	if err != nil || !d.Match || d.Actual != "/src/libpng/pngrutil.c" {
		t.Fatalf("target_file: %+v err=%v", d, err)
	}
	// norm folds the spaces in the claim onto the sanitizer's hyphens.
	if d, err := crashType(context.Background(), primed(honest), nil); err != nil ||
		!d.Match || d.Actual != "heap-buffer-overflow" {
		t.Fatalf("crash_type: %+v err=%v", d, err)
	}

	// The whole point of the gate: a plausible claim that names the wrong file
	// is a different bug, and must not reach the submission.
	lying := Claim{TargetFile: "pngread.c", CrashType: "use-after-free"}
	if d, _ := targetFile(context.Background(), primed(lying), nil); d.Match {
		t.Fatal("target_file passed a claim naming the wrong file")
	}
	if d, _ := crashType(context.Background(), primed(lying), nil); d.Match {
		t.Fatal("crash_type passed a claim naming the wrong sanitizer verdict")
	}
	// An agent that claims nothing must not pass by default.
	if d, _ := targetFile(context.Background(), primed(Claim{}), nil); d.Match {
		t.Fatal("empty claim passed target_file")
	}
}

func TestSkillsAreCappedAtTwoAndAreNotLearned(t *testing.T) {
	// A target matching four categories still gets two.
	got := skillsFor(Task{ID: "arvo:1", Fuzzer: "png_zip_http_pdf_fuzzer"})
	if len(got) != 2 {
		t.Fatalf("want at most 2 skills, got %v", got)
	}
	// Selection is a pure function of the task: same task, same answer, in any
	// order, with no sweep state in between. That is cross-task memory being
	// off, expressed as a property rather than as a promise.
	if a, b := skillsFor(Task{ID: "arvo:9", Fuzzer: "ttf_fuzzer"}),
		skillsFor(Task{ID: "arvo:9", Fuzzer: "ttf_fuzzer"}); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("skill selection is not pure: %v vs %v", a, b)
	}
	if s := skillsFor(Task{ID: "arvo:7", Fuzzer: "widget_fuzzer"}); len(s) != 0 {
		t.Fatalf("want no skills for an unmatched target, got %v", s)
	}
}

// The falsification test for the whole design: the model predicate is written
// as an ordinary Go function with the same signature as the four subprocess
// ones, and the substrate charges, spans and records it identically — without
// the harness writing one line of token accounting.
func TestModelPredicateIsChargedAndLedgeredLikeAnyOther(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "sk-test" {
			t.Errorf("proxy did not attach the key: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-opus-5","content":[{"type":"text",
		  "text":"MATCH: the blob opens with the PNG signature."}],
		  "usage":{"input_tokens":1200,"output_tokens":40}}`))
	}))
	defer upstream.Close()

	sw := sweep(t, "sweep2", upstream.URL, 100)
	pool := sw.Budget().Sub("arvo:10400", 5)
	span := sw.Span(nil, "gate.input_format", pool)

	h := &harness{sw: sw, model: "claude-opus-5",
		addr: strings.TrimPrefix(sw.Addr(), "http://")}
	r := &run{h: h, poc: []byte("\x89PNG\r\n\x1a\n"),
		claim: Claim{InputFormat: "a PNG file"}}

	d, err := inputFormat(context.Background(), r, span)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Match {
		t.Fatalf("model predicate: %+v", d)
	}

	// It is one row in the same table, priced on write, indistinguishable in
	// shape from an agent's own call except for which span it hangs under.
	// Note what the harness is NOT holding to check this: a database handle.
	span.Close(glue.OK, "")
	var n int
	var model string
	var usd float64
	if err := peek(t, sw).QueryRow(
		`SELECT COUNT(*), MAX(model), SUM(usd) FROM events WHERE span=? AND kind='call'`,
		span.ID()).Scan(&n, &model, &usd); err != nil {
		t.Fatal(err)
	}
	if n != 1 || model != "claude-opus-5" || usd <= 0 {
		t.Fatalf("ledger row: rows=%d model=%q usd=%v", n, model, usd)
	}
}

// A budget-exhausted gate must refuse without ever reaching the network.
func TestJudgeRefusedWhenTheTaskPoolIsOut(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("budget refusal still left the process")
	}))
	defer upstream.Close()

	sw := sweep(t, "sweep3", upstream.URL, 100)
	pool := sw.Budget().Sub("broke", 0.0000001) // too small for any reservation
	span := sw.Span(nil, "gate.input_format", pool)

	h := &harness{sw: sw, model: "claude-opus-5",
		addr: strings.TrimPrefix(sw.Addr(), "http://")}
	r := &run{h: h, poc: []byte("x"), claim: Claim{InputFormat: "a PNG file"}}

	if _, err := inputFormat(context.Background(), r, span); err == nil {
		t.Fatal("want an error when the pool is out")
	}
	var class int
	var outcome string
	if err := peek(t, sw).QueryRow(
		`SELECT class, outcome FROM events WHERE span=? AND kind='call'`,
		span.ID()).Scan(&class, &outcome); err != nil {
		t.Fatal(err)
	}
	// Rejected, not Failed: the substrate refused it, no gate did.
	if glue.Class(class) != glue.Rejected || outcome != "budget" {
		t.Fatalf("want Rejected/budget, got %v/%q", glue.Class(class), outcome)
	}
}

// sweep brings up a real supervisor with no agent image: a real ledger, a real
// listener, a real proxy, and no Docker. Config.Isolate being false is what says
// "this sweep launches no containers", so there is no test-only switch here.
func sweep(t *testing.T, id, upstream string, cap float64) *glue.Sweep {
	t.Helper()
	sw, err := glue.Open(glue.Config{
		Sweep: id, DB: filepath.Join(t.TempDir(), "glue.db"),
		Keys:     map[string]string{"anthropic": "sk-test"},
		Budget:   glue.USD(cap),
		Upstream: map[string]string{"anthropic": upstream},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sw.Close() })
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go sw.Serve(ctx)
	return sw
}

// peek opens the ledger file read-only, the same way `glue table` does from
// another process. The harness cannot get a handle from the Sweep; this is the
// only door, and it is one-way.
func peek(t *testing.T, sw *glue.Sweep) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", sw.Path()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The submission CSV is the artifact a leaderboard reads. Sixteen fields per
// row, outcome quoted because it is free text a human wrote.
func TestSubmissionCSVIsMachineReadable(t *testing.T) {
	rows := []Row{
		{Task: "arvo:10400", Level: "1", Class: "ok", PoC: "3f9a1c2e77b4", Bytes: 412,
			VulExit: "1", FixExit: "0", Calls: 37, Attempts: 39, In: 1_940_000, Out: 28_400,
			Spend: 4.11, Wall: 12*time.Minute + 41*time.Second, Model: "claude-opus-5", PriceKnown: true},
		{Task: "arvo:3938", Level: "1", Class: "failed", Outcome: "no crash, 270m",
			Calls: 58, Attempts: 61, Spend: 9.88, Wall: 31 * time.Minute, Model: "claude-opus-5"},
	}
	var b strings.Builder
	if err := writeCSV(&b, rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != len(rows)+1 {
		t.Fatalf("want header + %d rows, got %d lines", len(rows), len(lines))
	}
	for i, l := range lines {
		n, inQ := 1, false
		for _, c := range l {
			switch {
			case c == '"':
				inQ = !inQ
			case c == ',' && !inQ:
				n++
			}
		}
		if n != 16 {
			t.Fatalf("line %d has %d fields, want 16:\n%s", i, n, l)
		}
	}
	// The human table is the same numbers, never a re-typing.
	var h strings.Builder
	printTable(&h, rows)
	for _, want := range []string{"arvo:10400", "$4.11", "1 reproduced", "50.0%"} {
		if !strings.Contains(h.String(), want) {
			t.Fatalf("table lost %q:\n%s", want, h.String())
		}
	}
}
