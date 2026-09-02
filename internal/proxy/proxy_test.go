package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/internal/store"
)

// The SSE bodies below are the verbatim examples from
// platform.claude.com/docs/en/build-with-claude/streaming, with the cache
// fields added to message_start and repeated cumulatively in message_delta the
// way the current API sends them. If we ever sum instead of overwrite, the
// cache assertions here fail.
const sseBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100}}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":25,"output_tokens":15,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248}}

event: message_stop
data: {"type":"message_stop"}

`

type callRow struct {
	CallID                  string
	Attempt                 int
	Class                   store.Class
	Outcome, Model          string
	In, Out, CacheR, CacheW int64
	USD                     store.USD
	PriceKnown, Complete    bool
}

type fixture struct {
	t    *testing.T
	p    *Proxy
	db   *store.DB
	span Target
	fe   *httptest.Server // the supervisor listener
	up   *httptest.Server // stands in for api.anthropic.com
	seen chan *http.Request

	mu    sync.Mutex
	spans map[string]Target
}

func newFixture(t *testing.T, cap store.USD, upstream http.HandlerFunc) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "glue.db"), "sweep-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.NewPool("root", "", cap, 0); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, db: db, seen: make(chan *http.Request, 8),
		spans: map[string]Target{}}
	f.span = f.mint("worker-0", "root")
	f.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body.Close()
		f.seen <- r
		upstream(w, r)
	}))
	t.Cleanup(f.up.Close)

	f.p = New(db, map[string]string{"anthropic": "sk-ant-REAL-KEY"}, f.lookup)
	prov := f.p.Providers["anthropic"]
	prov.Upstream, _ = url.Parse(f.up.URL)
	f.p.Providers["anthropic"] = prov
	mux := http.NewServeMux()
	mux.Handle("/s/", f.p)
	f.fe = httptest.NewServer(mux)
	t.Cleanup(f.fe.Close)
	return f
}

// mint stands in for glue.Ledger.Span: it registers a bearer id the proxy will
// accept. The proxy resolves through this func and nothing else, which is what
// keeps it from importing the ledger.
func (f *fixture) mint(name, pool string) Target {
	f.t.Helper()
	var b [16]byte
	rand.Read(b[:])
	tg := Target{ID: hex.EncodeToString(b[:]), Run: name, Pool: pool,
		Providers: []string{"anthropic"}}
	if err := f.db.Append(store.Row{TS: time.Now(), Span: tg.ID, Run: tg.Run,
		Kind: "span_open", Outcome: name}); err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.spans[tg.ID] = tg
	f.mu.Unlock()
	return tg
}

func (f *fixture) lookup(id string) (Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tg, ok := f.spans[id]
	return tg, ok
}

func (f *fixture) spent(pool string) store.USD {
	sp, _, _, err := f.db.Spent(pool)
	if err != nil {
		f.t.Fatal(err)
	}
	return sp
}

func (f *fixture) base() string { return f.fe.URL + "/s/" + f.span.ID + "/anthropic" }

// rows returns every call callRow on the span, oldest first.
func (f *fixture) rows() []callRow {
	f.t.Helper()
	rs, err := f.db.SQL().Query(`SELECT call_id, attempt, class, outcome, model,
		in_tok, out_tok, cache_r, cache_w, usd, price_known, usage_complete
		FROM events WHERE kind='call' ORDER BY id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rs.Close()
	var out []callRow
	for rs.Next() {
		var r callRow
		if err := rs.Scan(&r.CallID, &r.Attempt, &r.Class, &r.Outcome, &r.Model,
			&r.In, &r.Out, &r.CacheR, &r.CacheW, &r.USD, &r.PriceKnown, &r.Complete); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// waitRows polls for n call rows. The callRow is written from ReverseProxy's
// goroutine when it closes the upstream body, which happens just after the
// client's final Read returns -- so a test that queries immediately races it.
func (f *fixture) waitRows(n int) []callRow {
	f.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; {
		rs := f.rows()
		if len(rs) >= n || time.Now().After(deadline) {
			return rs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) only() callRow {
	f.t.Helper()
	rs := f.waitRows(1)
	if len(rs) != 1 {
		f.t.Fatalf("want 1 call callRow, got %d: %+v", len(rs), rs)
	}
	return rs[0]
}

func post(t *testing.T, base, body string) *http.Response {
	t.Helper()
	res, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

const streamReq = `{"model":"claude-opus-5","max_tokens":1024,"stream":true,` +
	`"messages":[{"role":"user","content":"hi"}]}`

func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, body)
	}
}

func TestStreamingUsage(t *testing.T) {
	f := newFixture(t, 100, sseHandler(sseBody))

	res := post(t, f.base(), streamReq)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(got) != sseBody {
		t.Fatalf("body was not passed through verbatim:\n%q", got)
	}

	// The upstream must see our key and never the container's headers.
	up := <-f.seen
	if up.URL.Path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages (span prefix must be stripped)", up.URL.Path)
	}
	if up.Header.Get("X-Api-Key") != "sk-ant-REAL-KEY" {
		t.Errorf("x-api-key = %q", up.Header.Get("X-Api-Key"))
	}
	if up.Header.Get("Authorization") != "" {
		t.Errorf("Authorization leaked through: %q", up.Header.Get("Authorization"))
	}
	if up.Header.Get("Anthropic-Version") != AnthropicVersion {
		t.Errorf("anthropic-version = %q", up.Header.Get("Anthropic-Version"))
	}

	r := f.only()
	want := callRow{Attempt: 1, Class: store.OK, Model: "claude-opus-5",
		In: 25, Out: 15, CacheR: 1800, CacheW: 248, PriceKnown: true, Complete: true}
	want.CallID, want.USD = r.CallID, r.USD
	if r != want {
		t.Fatalf("row = %+v\nwant %+v", r, want)
	}
	// 25*5 + 15*25 + 1800*0.50 + 148*6.25 + 100*10 per million.
	const exp = (25*5 + 15*25 + 1800*0.50 + 148*6.25 + 100*10) / 1e6
	if d := float64(r.USD) - exp; d > 1e-12 || d < -1e-12 {
		t.Fatalf("usd = %v, want %v", r.USD, exp)
	}
	// Reservation must have been given back.
	if spent := f.spent("root"); float64(spent)-exp > 1e-12 {
		t.Fatalf("pool spent = %v, want %v (reservation not trued up)", spent, exp)
	}
}

// The agent must see tokens as they arrive. The upstream here holds the
// connection open after the first event; if the proxy buffered, the read below
// would block until the test's deadline.
func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})

	res := post(t, f.base(), streamReq)
	defer res.Body.Close()

	first := make(chan string, 1)
	go func() {
		br := bufio.NewReader(res.Body)
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.HasPrefix(line, "event: message_start") {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bytes reached the client before the upstream finished: the stream is buffered")
	}
	close(release)
	io.Copy(io.Discard, res.Body)
}

// A killed run must never read as zero spend.
func TestClientDisconnectMidStreamStillWritesRow(t *testing.T) {
	upstreamDone := make(chan struct{})
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the container died; never send message_delta
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", f.base()+"/v1/messages", strings.NewReader(streamReq))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	res.Body.Read(buf) // wait until the stream is genuinely flowing
	cancel()
	res.Body.Close()
	<-upstreamDone

	var r callRow
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if rs := f.rows(); len(rs) == 1 {
			r = rs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Outcome != "stream_incomplete" || r.Class != store.Cancelled {
		t.Fatalf("row = %+v, want Cancelled/stream_incomplete", r)
	}
	if r.Complete {
		t.Error("usage_complete must be 0 when message_stop never arrived")
	}
	if r.In != 25 {
		t.Errorf("in_tok = %d, want 25 from message_start", r.In)
	}
	// The reservation stands as the charge, and it is not zero.
	reserved := reservation(f.p.Providers["anthropic"], wireRequest{Model: "claude-opus-5", MaxTokens: 1024}, len(streamReq))
	if r.USD < reserved {
		t.Fatalf("usd = %v, want >= the reservation %v: a killed run must not read as free", r.USD, reserved)
	}
}

func TestNonStreamingUsage(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",`+
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":2048,"output_tokens":503,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248}}`)
	})
	res := post(t, f.base(), `{"model":"claude-sonnet-5","max_tokens":512}`)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	r := f.only()
	if r.In != 2048 || r.Out != 503 || r.CacheR != 1800 || r.CacheW != 248 {
		t.Fatalf("usage = %+v", r)
	}
	if !r.Complete || r.Class != store.OK || r.Model != "claude-sonnet-5" {
		t.Fatalf("row = %+v", r)
	}
}

func TestProviderErrorRowAndReleasedReservation(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		outcome string
	}{
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`, "rate_limit_error"},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "overloaded_error"},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`, "invalid_request_error"},
		{500, ``, "api_error"}, // unusable body: fall back to the documented type
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			res := post(t, f.base(), streamReq)
			got, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != tc.status || string(got) != tc.body {
				t.Fatalf("status=%d body=%q, want %d %q", res.StatusCode, got, tc.status, tc.body)
			}
			r := f.only()
			if r.Class != store.Failed || r.Outcome != tc.outcome {
				t.Fatalf("row = %+v, want Failed/%s", r, tc.outcome)
			}
			if r.USD != 0 || !r.Complete {
				t.Fatalf("a refused request generated nothing: want usd=0 usage_complete=1, got %+v", r)
			}
			if spent := f.spent("root"); spent != 0 {
				t.Fatalf("reservation not released: pool spent = %v", spent)
			}
		})
	}
}

// Budget enforcement lives inside the handler: the request must not reach the
// upstream at all.
func TestBudgetRefusalNeverLeavesTheProcess(t *testing.T) {
	f := newFixture(t, 0.0001, sseHandler(sseBody)) // reservation for 1024 out tokens is ~$0.026
	res := post(t, f.base(), streamReq)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	var e struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if json.Unmarshal(got, &e) != nil || e.Error.Type != "rate_limit_error" {
		t.Fatalf("body = %q, want the documented error envelope", got)
	}
	if !strings.Contains(e.Error.Message, "over budget") {
		t.Errorf("message = %q, want a reason", e.Error.Message)
	}
	select {
	case up := <-f.seen:
		t.Fatalf("request reached the upstream anyway: %v", up.URL)
	default:
	}
	r := f.only()
	if r.Class != store.Rejected || r.Outcome != "budget" || r.USD != 0 {
		t.Fatalf("row = %+v, want Rejected/budget/0", r)
	}
	if spent := f.spent("root"); spent != 0 {
		t.Fatalf("a refused reservation must leave nothing behind, got %v", spent)
	}
}

// A sub-pool's spend counts against its parent, so the parent cap binds even
// when the child's does not.
func TestSubPoolChargesParent(t *testing.T) {
	f := newFixture(t, 0.001, sseHandler(sseBody))
	if err := f.db.NewPool("child", "root", 1000, 0); err != nil { // huge child cap, tiny root cap
		t.Fatal(err)
	}
	span := f.mint("w1", "child")
	res := post(t, f.fe.URL+"/s/"+span.ID+"/anthropic", streamReq)
	res.Body.Close()
	if res.StatusCode != 429 {
		t.Fatalf("status = %d, want 429: the root cap must bind", res.StatusCode)
	}
	if spent := f.spent("child"); spent != 0 {
		t.Fatalf("failed reserve left %v on the child pool", spent)
	}
}

func TestUnknownModelIsChargedAtTheCeiling(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-something-unreleased","usage":{"input_tokens":1000000,"output_tokens":0}}`)
	})
	res := post(t, f.base(), `{"model":"claude-something-unreleased","max_tokens":1}`)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	r := f.only()
	if r.PriceKnown {
		t.Error("price_known must be 0 for an unrecognized model")
	}
	if r.USD != store.USD(maxRate.In) { // 1M input tokens at the ceiling input rate
		t.Fatalf("usd = %v, want the ceiling %v: unknown models are never free", r.USD, maxRate.In)
	}
}

func TestDatedModelIdResolves(t *testing.T) {
	if r, ok := rateFor("anthropic", "claude-opus-4-5-20251101"); !ok || r.Out != 25 {
		t.Fatalf("dated id resolved to %+v known=%v, want the claude-opus-4-5 rate", r, ok)
	}
	if r, ok := rateFor("anthropic", "claude-opus-4-1-20250805"); !ok || r.Out != 75 {
		t.Fatalf("longest-prefix match failed: %+v %v", r, ok)
	}
}

// An id the supervisor never minted is not a capability.
func TestUnknownSpanIsRefusedAndWritesNothing(t *testing.T) {
	f := newFixture(t, 100, sseHandler(sseBody))
	res := post(t, f.fe.URL+"/s/deadbeef/anthropic", streamReq)
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if rs := f.rows(); len(rs) != 0 {
		t.Fatalf("an unminted span id created rows: %+v", rs)
	}
}

// A retried identical body after a retryable failure joins the same logical
// call. COUNT(DISTINCT call_id) is 1; COUNT(*) is 2.
func TestRetryJoinsTheSameCallID(t *testing.T) {
	fail := true
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			fail = false
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		sseHandler(sseBody)(w, r)
	})
	for range 2 {
		res := post(t, f.base(), streamReq)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	rs := f.waitRows(2)
	if len(rs) != 2 {
		t.Fatalf("want 2 attempt rows, got %d", len(rs))
	}
	if rs[0].CallID != rs[1].CallID {
		t.Fatalf("retry did not join: %q vs %q", rs[0].CallID, rs[1].CallID)
	}
	if rs[0].Attempt != 1 || rs[1].Attempt != 2 {
		t.Fatalf("attempts = %d,%d", rs[0].Attempt, rs[1].Attempt)
	}
}

// A different body after the same failure is a different logical call.
func TestDifferentBodyIsANewCall(t *testing.T) {
	f := newFixture(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
	})
	for _, b := range []string{streamReq, `{"model":"claude-opus-5","max_tokens":1024,"stream":true}`} {
		res := post(t, f.base(), b)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	rs := f.waitRows(2)
	if len(rs) != 2 || rs[0].CallID == rs[1].CallID {
		t.Fatalf("rows = %+v, want two distinct call_ids", rs)
	}
}

// Split the stream at every byte offset: line reassembly across Read
// boundaries must not change a single number.
func TestSSEReassemblyAcrossChunkBoundaries(t *testing.T) {
	for split := 1; split < len(sseBody); split += 7 {
		var u Usage
		tp := &tap{sse: true, parse: &anthropicParser{}}
		tp.observe([]byte(sseBody[:split]))
		tp.observe([]byte(sseBody[split:]))
		u = tp.parse.Usage()
		if u.In != 25 || u.Out != 15 || u.CacheR != 1800 || u.CacheW != 248 || !u.Complete {
			t.Fatalf("split at %d: %+v", split, u)
		}
	}
}

// message_delta repeats input and cache cumulatively. Summing double-counts.
func TestCumulativeDeltaIsNotSummed(t *testing.T) {
	tp := &tap{sse: true, parse: &anthropicParser{}}
	tp.observe([]byte(sseBody))
	if tp.parse.Usage().CacheR != 1800 {
		t.Fatalf("cache_read = %d, want 1800 (3600 means message_delta was summed)", tp.parse.Usage().CacheR)
	}
	if tp.parse.Usage().In != 25 {
		t.Fatalf("input = %d, want 25 (50 means message_start and message_delta were summed)", tp.parse.Usage().In)
	}
}

// A 200 that turns into an error partway: tokens were generated, so the charge
// must not be zero and the usage must not read as final.
func TestMidStreamErrorEvent(t *testing.T) {
	f := newFixture(t, 100, sseHandler(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n"+
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	res := post(t, f.base(), streamReq)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	r := f.only()
	if r.Class != store.Failed || r.Outcome != "overloaded_error" {
		t.Fatalf("row = %+v", r)
	}
	if r.Complete {
		t.Error("usage_complete must be 0: message_stop never arrived")
	}
	if r.USD == 0 {
		t.Error("tokens were generated before the break; the charge must not be zero")
	}
}
