package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	glue "github.com/valbaudo/dawn"
)

// There is no key in this file and no network call leaves the machine. The
// upstream is an httptest server handing back canned Anthropic responses, so
// the loop runs against a script and every branch in it is reachable from a
// test: a rejected citation, an accepted one, each stop_reason, the turn cap,
// and the budget refusal.

// ------------------------------------------------------------- canned wire

func msg(stop, content string) string {
	return `{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-5",` +
		`"content":[` + content + `],"stop_reason":"` + stop + `",` +
		`"usage":{"input_tokens":1000,"output_tokens":50}}`
}

func use(id, name, input string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}`
}

func say(s string) string { return `{"type":"text","text":` + strconv.Quote(s) + `}` }

// upstream is the stub vendor. It replays one canned response per request and
// keeps every request body, which is how a test asserts what the model was
// told after its citation was rejected.
type upstream struct {
	mu      sync.Mutex
	replies []string
	bodies  []string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	n := len(u.bodies)
	u.bodies = append(u.bodies, string(b))
	u.mu.Unlock()
	// Past the end of the script the last reply repeats, so a test can drive
	// the loop until the turn cap stops it.
	if n >= len(u.replies) {
		n = len(u.replies) - 1
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, u.replies[n])
}

func (u *upstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies...)
}

// ------------------------------------------------------------------ the rig

type rig struct {
	h    *hunter
	sw   *glue.Sweep
	up   *upstream
	span *glue.Span
}

func newRig(t *testing.T, root string, budget glue.USD, replies ...string) *rig {
	t.Helper()
	up := &upstream{replies: replies}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	sw, err := glue.Open(glue.Config{
		Sweep:    "loop-test",
		DB:       filepath.Join(t.TempDir(), "loop.db"),
		Keys:     map[string]string{"anthropic": "sk-anthropic-not-real"},
		Budget:   budget,
		Upstream: map[string]string{"anthropic": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	go sw.Serve(ctx)
	t.Cleanup(func() { stop(); sw.Close() })

	span := sw.Span(nil, "hunt test", sw.Budget())
	return &rig{h: &hunter{root: root, sw: sw, span: span}, sw: sw, up: up, span: span}
}

func (r *rig) run(t *testing.T, maxTurns int) (glue.Class, string) {
	t.Helper()
	c, why := r.h.hunt(context.Background(),
		anthropic.NewClient(option.WithAPIKey("dawn")),
		"claude-sonnet-5", "path traversal", maxTurns)
	r.span.Close(c, why)
	if err := r.sw.Err(); err != nil {
		t.Fatal(err)
	}
	return c, why
}

func srcTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Four lines exactly, so a cited range of 1-99 is provably wrong.
	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package a\n\nfunc open(p string) {}\n// end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// ------------------------------------------------------------------ the loop

// The whole shape in one test: the model reads a file, cites a line that does
// not exist, is told so in a tool_result, cites a real one, and stops. The
// assertion that matters is the third request body — the correction has to
// reach the model as data, on the next turn, or the loop's only conditional
// does nothing.
func TestLoopRejectsABadCitationThenAcceptsAGoodOne(t *testing.T) {
	root := srcTree(t)
	r := newRig(t, root, 10,
		msg("tool_use", use("t1", "read_file", `{"path":"a.go"}`)),
		msg("tool_use", use("t2", "file_finding", `{"file":"a.go","lo":1,"hi":99,`+
			`"threat":"An unauthenticated caller controls p and reaches the filesystem with it.","claim":"no bound"}`)),
		msg("tool_use", use("t3", "file_finding", `{"file":"a.go","lo":3,"hi":3,`+
			`"threat":"An unauthenticated caller controls p and reaches the filesystem with it.","claim":"no bound"}`)),
		msg("end_turn", say("One finding. Done.")),
	)

	c, why := r.run(t, 24)
	if c != glue.OK {
		t.Fatalf("class = %s (%s), want ok", c, why)
	}
	if len(r.h.filed) != 1 {
		t.Fatalf("filed %d findings, want 1: %+v", len(r.h.filed), r.h.filed)
	}
	if f := r.h.filed[0]; f.Lo != 3 || f.Hi != 3 {
		t.Fatalf("the wrong finding got through: %+v", f)
	}

	bodies := r.up.seen()
	if len(bodies) != 4 {
		t.Fatalf("%d requests, want 4", len(bodies))
	}
	// Turn 3's request carries the rejection of turn 2's citation.
	if !strings.Contains(bodies[2], `"is_error":true`) {
		t.Fatalf("the rejection did not reach the model as an error:\n%s", bodies[2])
	}
	if !strings.Contains(bodies[2], "a.go has 4 lines; you cited 1-99") {
		t.Fatalf("the rejection was not specific enough to act on:\n%s", bodies[2])
	}
	// And turn 4's carries the acceptance, so the model knows to stop.
	if !strings.Contains(bodies[3], "accepted: a.go:3-3") {
		t.Fatalf("the acceptance did not reach the model:\n%s", bodies[3])
	}
}

// The span-per-turn is the claim that dawn attributes cost per turn, and it is
// only true if the proxy — not this process — put the numbers there. Every turn
// span must carry exactly one metered call.
func TestTheLedgerPricesEveryTurnSeparately(t *testing.T) {
	root := srcTree(t)
	r := newRig(t, root, 10,
		msg("tool_use", use("t1", "read_file", `{"path":"a.go"}`)),
		msg("tool_use", use("t2", "read_file", `{"path":"a.go","from":3}`)),
		msg("end_turn", say("Nothing here.")),
	)
	if c, why := r.run(t, 24); c != glue.OK {
		t.Fatalf("class = %s (%s)", c, why)
	}

	outs, err := r.sw.Outcomes()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]glue.Outcome{}
	for _, o := range outs {
		byName[o.Name] = o
	}
	var total glue.USD
	for turn := 1; turn <= 3; turn++ {
		o, ok := byName[fmt.Sprintf("turn %d", turn)]
		if !ok {
			t.Fatalf("no span for turn %d; ledger has %v", turn, byName)
		}
		if o.Calls != 1 {
			t.Fatalf("turn %d: %d calls, want 1", turn, o.Calls)
		}
		if o.In != 1000 || o.Out != 50 {
			t.Fatalf("turn %d: in=%d out=%d, want the upstream's own numbers", turn, o.In, o.Out)
		}
		if !o.PriceKnown || o.Spend <= 0 {
			t.Fatalf("turn %d: spend=%v priceKnown=%v", turn, o.Spend, o.PriceKnown)
		}
		total += o.Spend
	}
	// The parent is a subtree rollup, so it must equal the turns beneath it.
	if got := byName["hunt test"].Spend; got != total {
		t.Fatalf("parent spend %v != sum of turns %v", got, total)
	}
	if byName["hunt test"].Calls != 3 {
		t.Fatalf("parent calls = %d, want 3", byName["hunt test"].Calls)
	}
}

// The accepted finding reaches the ledger as a fact, and the rejected one does
// not. This is the line between "the model said it" and "the harness recorded
// it", and it is drawn by cite().
func TestOnlyAcceptedFindingsBecomeFacts(t *testing.T) {
	root := srcTree(t)
	r := newRig(t, root, 10,
		msg("tool_use", use("t1", "file_finding", `{"file":"nope.go","lo":1,"hi":1,`+
			`"threat":"An unauthenticated caller controls p and reaches the filesystem with it.","claim":"x"}`)),
		msg("tool_use", use("t2", "file_finding", `{"file":"a.go","lo":3,"hi":3,`+
			`"threat":"An unauthenticated caller controls p and reaches the filesystem with it.","claim":"x"}`)),
		msg("end_turn", say("done")),
	)
	r.run(t, 24)

	outs, err := r.sw.Outcomes()
	if err != nil {
		t.Fatal(err)
	}
	var facts []glue.Fact
	for _, o := range outs {
		if o.Name == "hunt test" {
			facts = o.Facts
		}
	}
	if len(facts) != 1 {
		t.Fatalf("%d facts, want 1: %+v", len(facts), facts)
	}
	var f finding
	if err := json.Unmarshal([]byte(facts[0].Value), &f); err != nil {
		t.Fatalf("fact is not a finding: %q", facts[0].Value)
	}
	if f.File != "a.go" {
		t.Fatalf("the rejected finding was recorded: %+v", f)
	}
}

// Every stop_reason the API can end a turn with, and the class dawn records for
// it. Rejected versus Failed is the distinction that costs money when it is
// wrong: Rejected means re-running this unchanged buys the same answer twice.
func TestStopReasonsMapOntoClasses(t *testing.T) {
	cases := []struct {
		stop string
		want glue.Class
		says string
	}{
		{"end_turn", glue.OK, "0 filed in 1 turns"},
		{"refusal", glue.Rejected, "refusal on turn 1"},
		{"max_tokens", glue.Failed, "raise it"},
		{"model_context_window_exceeded", glue.Failed, "context window exhausted"},
		{"stop_sequence", glue.Failed, `stop_reason "stop_sequence"`},
	}
	for _, c := range cases {
		t.Run(c.stop, func(t *testing.T) {
			r := newRig(t, srcTree(t), 10, msg(c.stop, say("...")))
			got, why := r.run(t, 24)
			if got != c.want {
				t.Fatalf("%s -> %s, want %s (%s)", c.stop, got, c.want, why)
			}
			if !strings.Contains(why, c.says) {
				t.Fatalf("outcome %q does not say %q", why, c.says)
			}
		})
	}
}

// No CLI on this machine has a --max-turns. In this shape the cap is a for
// loop, and a model that never stops asking for tools stops here.
func TestTurnCapStopsAModelThatNeverStops(t *testing.T) {
	r := newRig(t, srcTree(t), 10,
		msg("tool_use", use("t1", "read_file", `{"path":"a.go"}`)))

	c, why := r.run(t, 3)
	if c != glue.Failed || !strings.Contains(why, "turn cap") {
		t.Fatalf("class = %s (%s), want failed / turn cap", c, why)
	}
	if n := len(r.up.seen()); n != 3 {
		t.Fatalf("%d requests past a cap of 3", n)
	}
}

// A budget that cannot cover the reservation is refused inside the proxy before
// the request is forwarded, and the SDK is told not to retry it. The loop has
// to read that as Rejected — the run did not break, it was not permitted.
func TestBudgetRefusalIsRejectedNotFailed(t *testing.T) {
	r := newRig(t, srcTree(t), 0.001, msg("end_turn", say("...")))

	c, why := r.run(t, 24)
	if c != glue.Rejected {
		t.Fatalf("class = %s (%s), want rejected", c, why)
	}
	if !strings.Contains(why, "429") {
		t.Fatalf("outcome %q does not name the refusal", why)
	}
	if n := len(r.up.seen()); n != 0 {
		t.Fatalf("%d requests reached the vendor after a refusal", n)
	}
}

// ---------------------------------------------------------------- the gate

func TestCite(t *testing.T) {
	root := srcTree(t)
	const good = "An unauthenticated caller controls p and reaches the filesystem with it."

	cases := []struct {
		name string
		f    finding
		want string // "" means accepted
	}{
		{"ok", finding{File: "a.go", Lo: 1, Hi: 4, Threat: good}, ""},
		{"one line", finding{File: "a.go", Lo: 3, Hi: 3, Threat: good}, ""},
		{"past eof", finding{File: "a.go", Lo: 1, Hi: 99, Threat: good}, "a.go has 4 lines; you cited 1-99"},
		{"zero", finding{File: "a.go", Lo: 0, Hi: 1, Threat: good}, "you cited 0-1"},
		{"inverted", finding{File: "a.go", Lo: 4, Hi: 2, Threat: good}, "you cited 4-2"},
		{"missing", finding{File: "b.go", Lo: 1, Hi: 1, Threat: good}, "b.go does not exist"},
		{"escape", finding{File: "../../etc/passwd", Lo: 1, Hi: 1, Threat: good}, "outside the source tree"},
		{"absolute", finding{File: "/etc/passwd", Lo: 1, Hi: 1, Threat: good}, "/etc/passwd does not exist"},
		{"threat is the class name", finding{File: "a.go", Lo: 1, Hi: 1, Threat: "path traversal"}, "threat model is 2 words"},
		{"no threat", finding{File: "a.go", Lo: 1, Hi: 1}, "threat model is 0 words"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := cite(root, c.f)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("rejected a good citation: %v", err)
			case c.want == "":
			case err == nil:
				t.Fatalf("accepted %+v, want %q", c.f, c.want)
			case !strings.Contains(err.Error(), c.want):
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// read_file numbers its lines because a model cannot cite what it cannot
// count, and it bounds its output because the conversation is the context.
func TestReadNumbersAndBounds(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= maxRead+10; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(b.String()), 0o644)
	h := &hunter{root: root}

	out, isErr := h.read("big.txt", 0)
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if !strings.Contains(out, "     1  line 1\n") {
		t.Fatalf("lines are not numbered:\n%s", out[:80])
	}
	if strings.Contains(out, "line "+strconv.Itoa(maxRead+1)+"\n") {
		t.Fatal("read returned past its own bound")
	}
	if !strings.Contains(out, "call read_file again with from="+strconv.Itoa(maxRead+1)) {
		t.Fatalf("truncation did not tell the model how to continue:\n%s", out[len(out)-200:])
	}

	if out, isErr := h.read("../etc/passwd", 0); !isErr || !strings.Contains(out, "outside the source tree") {
		t.Fatalf("read escaped the tree: %q", out)
	}
	if out, isErr := h.read("big.txt", 9999); !isErr || !strings.Contains(out, "you asked to start at 9999") {
		t.Fatalf("read past EOF: %q", out)
	}
}

func TestLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{{"", 0}, {"\n", 0}, {"a", 1}, {"a\n", 1}, {"a\nb", 2}, {"a\nb\n", 2}, {"a\n\n", 2}} {
		if got := len(lines([]byte(c.in))); got != c.want {
			t.Fatalf("lines(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
