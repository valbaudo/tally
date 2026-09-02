package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valbaudo/dawn/internal/store"
)

// The proxy is the only route out of the container's network namespace, so it
// is the only place spend can be observed, and the only place it can be
// refused. Nothing in here asks the agent for anything.
//
// Wire facts below were verified 2026-09-02 against:
//   platform.claude.com/docs/en/build-with-claude/streaming   (event names, usage paths)
//   platform.claude.com/docs/en/build-with-claude/prompt-caching (cache fields, cache_creation split)
//   platform.claude.com/docs/en/api/errors                     (error body shape, status->type)
//   platform.claude.com/docs/en/api/overview                   (x-api-key, anthropic-version)
// and against $GOROOT/src/net/http/httputil/reverseproxy.go (go1.27.0) for the
// flush and body-close guarantees this file leans on.

const (
	// AnthropicVersion is required on every request (api/overview, "Authentication").
	AnthropicVersion = "2023-06-01"

	// Request bodies are capped at 32 MB by the API itself (api/errors,
	// "Request size limits"). We buffer the request — never the response —
	// because max_tokens drives the reservation and the body hash drives
	// call_id continuation.
	maxRequestBody = 32 << 20

	// Error bodies are the documented {"type":"error","error":{...}} envelope.
	maxErrorBody = 64 << 10

	// Non-streaming Message bodies. Larger than any real one; a body past this
	// is simply not parsed and lands as usage_complete=0.
	maxJSONBody = 8 << 20

	// Resync guard on a single SSE data: line.
	maxSSELine = 1 << 20

	// bytesPerToken estimates input tokens at reservation time, before
	// message_start tells us the truth. Deliberately below the ~4 rule of thumb
	// so the reservation over-covers; the true-up releases the slack about a
	// second later.
	// ponytail: base64 image payloads over-reserve ~4x for the life of one
	// call. Upgrade path is POST /v1/messages/count_tokens, at the price of a
	// second round trip on every request.
	bytesPerToken = 3
)

// Usage is what the proxy extracts from an upstream response.
type Usage struct {
	Model  string
	In     int64 // usage.input_tokens
	Out    int64 // usage.output_tokens
	CacheR int64 // usage.cache_read_input_tokens
	CacheW int64 // usage.cache_creation_input_tokens

	// Split of CacheW, present only when the response carried
	// usage.cache_creation. Priced separately; not stored (one cache_w column).
	CacheW5m int64 // usage.cache_creation.ephemeral_5m_input_tokens
	CacheW1h int64 // usage.cache_creation.ephemeral_1h_input_tokens

	// Complete is true only when the terminal event was seen: message_stop for
	// a stream, a parsed body for a non-stream. False means the numbers are a
	// floor and the reservation stands as the charge.
	Complete bool

	// Err is the provider's own error type when a 200 turned into an error
	// partway through. It lives here rather than on the tap because only the
	// per-provider parser can recognise one.
	Err string
}

// apply folds one wire usage object in. It OVERWRITES; it never sums.
//
// message_delta.usage is CUMULATIVE, not incremental (streaming doc, the
// warning under "Event types"), and on the current API it repeats
// input_tokens and both cache fields that message_start already sent. Adding
// them is the double-count that langchainjs #10249 and litellm #25517 both
// shipped. Older responses omit those fields from message_delta, where they
// unmarshal to 0 — hence "overwrite when non-zero", which is correct under
// both shapes.
func (u *Usage) apply(w usageWire) {
	if w.InputTokens > 0 {
		u.In = w.InputTokens
	}
	if w.OutputTokens > 0 {
		u.Out = w.OutputTokens
	}
	if w.CacheReadInputTokens > 0 {
		u.CacheR = w.CacheReadInputTokens
	}
	if w.CacheCreationInputTokens > 0 {
		u.CacheW = w.CacheCreationInputTokens
	}
	if w.CacheCreation != nil {
		u.CacheW5m = w.CacheCreation.Ephemeral5m
		u.CacheW1h = w.CacheCreation.Ephemeral1h
	}
}

type usageWire struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// sseEvent is every streaming event we care about, in one shape. Unknown types
// fall through untouched — the versioning policy says new ones will appear.
type sseEvent struct {
	Type    string    `json:"type"`
	Message *struct { // message_start
		Model string    `json:"model"`
		Usage usageWire `json:"usage"`
	} `json:"message"`
	Usage *usageWire `json:"usage"` // message_delta
	Error *struct {  // mid-stream error, after a 200
		Type string `json:"type"`
	} `json:"error"`
}

// wireMessage is the non-streaming response: usage is one object at the top
// level, alongside model.
type wireMessage struct {
	Model string    `json:"model"`
	Usage usageWire `json:"usage"`
}

// wireRequest is every provider's request in one shape. The three output-cap
// keys are the only real divergence: Anthropic REQUIRES max_tokens, OpenAI Chat
// Completions takes max_completion_tokens (max_tokens is its deprecated alias),
// and Responses takes max_output_tokens — and all three may be absent on the
// OpenAI wires, where cap() returns 0 and the provider's ceiling applies.
type wireRequest struct {
	Model         string `json:"model"`
	MaxTokens     int64  `json:"max_tokens"`
	MaxCompletion int64  `json:"max_completion_tokens"`
	MaxOutput     int64  `json:"max_output_tokens"`
	Stream        bool   `json:"stream"`
}

func (r wireRequest) cap() int64 { return max(r.MaxTokens, r.MaxCompletion, r.MaxOutput) }

// ---- proxy ---------------------------------------------------------------

// Target is everything the proxy needs to know about a span, which is
// deliberately not much: an id to attribute rows to, its place in the tree, and
// the name of the pool that pays. The proxy has no Span type, no Ledger and no
// Pool — package glue owns those, and if the proxy could name them the import
// would run the wrong way and the harness could reach the metering path.
type Target struct {
	ID, Parent, Run string
	Pool            string

	// Providers is the set of provider names this span may reach. The span id
	// in the path is a bearer capability; the provider segment beside it is
	// not, so without this a container could spend the sweep's OpenAI key on a
	// span the harness minted for Anthropic alone. Empty means none.
	Providers []string
}

type Proxy struct {
	db      *store.DB
	resolve func(spanID string) (Target, bool)

	// Providers is the routing table, keyed by the path segment that selects
	// it. Every credential this sweep holds lives in here and nowhere else.
	Providers map[string]Provider

	rp *httputil.ReverseProxy

	// live counts the attempts currently open per span. It is the second of the
	// three liveness signals in live.go, and it is free: the supervisor is
	// already holding the request. Nothing persists it — if this process dies,
	// the containers die with it and there is nothing left to be live.
	mu   sync.Mutex
	live map[string]int
}

// InFlight reports how many upstream attempts are open on a span right now.
// A span with a request in flight is alive BY CONSTRUCTION, for as long as the
// call runs — which is what keeps a twelve-minute Opus call with extended
// thinking from being reaped as stale, with no threshold tuned to model
// latency and no heartbeat sidecar in any agent image.
func (p *Proxy) InFlight(span string) int {
	// nil is the out-of-process reader: `glue top` running beside the sweep
	// cannot see the supervisor's open connections, and 0 is the honest answer
	// there. Liveness then rests on last_seen and the container CPU counter,
	// which is exactly what a second process CAN observe.
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live[span]
}

func (p *Proxy) enter(span string) {
	p.mu.Lock()
	p.live[span]++
	p.mu.Unlock()
}

// leave is called from finish, which is sync.Once-guarded, so every attempt
// that entered leaves exactly once on every terminal path.
func (p *Proxy) leave(span string) {
	p.mu.Lock()
	if n := p.live[span] - 1; n > 0 {
		p.live[span] = n
	} else {
		delete(p.live, span)
	}
	p.mu.Unlock()
}

// NewProxy builds the handler. Mount it at /s/ on the supervisor's listener.
//
// httputil.ReverseProxy, not a hand-rolled handler, for four things that are
// already written and already correct in go1.27.0:
//
//  1. flushInterval() returns -1 for Content-Type: text/event-stream
//     (reverseproxy.go:671) and for ContentLength == -1 (line 676), and
//     maxLatencyWriter.Write flushes on every single write when the latency is
//     negative (line 764). SSE reaches the agent token by token with no
//     Flusher code of our own.
//  2. res.Body.Close() runs on EVERY exit path — after a successful copy
//     (line 601), deferred when the copy errors, which is what a client
//     disconnect looks like (line 592), and inside modifyResponse when the
//     hook errors (line 395). That makes a body wrapper's Close the single
//     guaranteed place to write the ledger row.
//  3. removeHopByHopHeaders per RFC 9110 6.1, on both legs.
//  4. Client disconnect cancels the request context, which tears down the
//     upstream RoundTrip instead of leaking it.
//
// Hand-rolling those is ~150 lines to re-derive stdlib. The Rewrite hook (not
// Director) is used because Rewrite drops inbound X-Forwarded-* by default,
// and the container is not trusted to set them.
func New(db *store.DB, keys map[string]string, resolve func(spanID string) (Target, bool)) *Proxy {
	p := &Proxy{
		db:        db,
		resolve:   resolve,
		Providers: Providers(keys),
		live:      map[string]int{},
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
	}
	return p
}

// call is the per-request state, carried in the context so the Rewrite,
// ModifyResponse and body-Close hooks can all reach it.
type call struct {
	span     Target
	prov     Provider
	path     string
	body     []byte
	hash     string
	model    string
	stream   bool
	reserved store.USD
	callID   string
	attempt  int
	started  time.Time
	once     sync.Once

	// retry is "will the client send this same body again?", decided on each
	// terminal path at the moment it can see the whole answer, and persisted in
	// meta so the NEXT attempt's identify() reads a fact instead of guessing
	// from a string. Zero value false means "do not merge", so a path that
	// forgets to set it splits a call in two — the recoverable direction.
	retry bool
}

type ctxKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanID, provName, path, ok := splitSpanPath(r.URL.Path)
	if !ok {
		http.Error(w, "want /s/<span>/<provider>/<upstream path>", http.StatusNotFound)
		return
	}
	// The span id is a bearer capability minted by the supervisor. An id it did
	// not mint gets nothing, and writes nothing: there is no span to attribute
	// a row to, and letting an unknown id create rows is a write primitive
	// handed to whoever guessed it.
	span, ok := p.resolve(spanID)
	if !ok {
		http.Error(w, "unknown span", http.StatusNotFound)
		return
	}

	// The provider is resolved BEFORE anything is read, because the reservation
	// below has to price the call and the price map is keyed by provider. The
	// alternative — recording one provider per span, or sniffing the body
	// shape — cannot answer that question until the response echoes a model
	// back, which is after the money is already committed.
	prov, ok := p.Providers[provName]
	if !ok || !allowed(span.Providers, provName) {
		http.Error(w, "unknown provider for span", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req wireRequest
	json.Unmarshal(body, &req) // a body we cannot parse still gets forwarded; the API judges it

	if req.Stream && prov.InjectUsage != nil {
		body = prov.InjectUsage(body)
	}

	c := &call{
		span: span, prov: prov, path: path, body: body, model: req.Model,
		stream: req.Stream, started: time.Now(),
	}
	sum := sha256.Sum256(body)
	c.hash = hex.EncodeToString(sum[:])
	p.identify(c)
	p.enter(span.ID)

	// Reserve at max_tokens BEFORE the request leaves. This is the entire
	// budget mechanism: one conditional UPDATE per pool, RowsAffected checked.
	// c.reserved is assigned only on success, so the refusal path below has
	// nothing to settle and cannot drive pool.spent negative.
	want := reservation(prov, req, len(body))
	charged, err := p.db.Reserve(c.span.Pool, want)
	if err != nil || charged == 0 {
		// Rejected, not Failed: the substrate refused this, no gate did.
		p.finish(c, Usage{Model: req.Model, Complete: true}, store.Rejected, "budget", 0)
		// The refusal is spoken in the wire dialect the client is expecting;
		// an Anthropic error envelope handed to an OpenAI SDK is an unparsed
		// 429 that the SDK retries on its own schedule.
		w.Header().Set("Content-Type", "application/json")
		// 429 is the only honest status for "you may not spend", but the SDK
		// retries every 429 twice by default, and a budget that is out will
		// still be out 8 seconds later. `x-should-retry: false` is checked
		// first in _should_retry and short-circuits it, which turns three
		// pointless round trips into one and makes the merge question moot.
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "rate_limit_error",
				"message": "glue: over budget, pool " + c.span.Pool,
			},
		})
		return
	}
	c.reserved = want

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
}

// splitSpanPath turns /s/<id>/anthropic/v1/messages into
// <id>, anthropic, /v1/messages.
//
// The provider lives in the path rather than in a per-span field because the
// base URL is a string dawn mints anyway — ANTHROPIC_BASE_URL for Claude Code,
// base_url in ~/.codex/config.toml for Codex, baseUrl in a droid settings file
// — so a second segment costs nothing at the mint site and buys the case both
// reference harnesses actually need: ONE span that calls two providers, because
// droid can be handed several customModels at once and picks between them
// per-turn. A per-span field would have to be re-derived from the model string,
// which is a second mapping that goes stale every model launch.
func splitSpanPath(p string) (span, prov, rest string, ok bool) {
	if !strings.HasPrefix(p, "/s/") {
		return "", "", "", false
	}
	p = p[3:]
	i := strings.IndexByte(p, '/')
	if i <= 0 {
		return "", "", "", false
	}
	span, p = p[:i], p[i+1:]
	j := strings.IndexByte(p, '/')
	if j <= 0 {
		return "", "", "", false
	}
	return span, p[:j], p[j:], true
}

func allowed(set []string, name string) bool {
	for _, s := range set {
		if s == name {
			return true
		}
	}
	return false
}

// reservation prices the worst case: every output token the caller allowed,
// plus an estimate of the input it just handed us.
func reservation(prov Provider, req wireRequest, bodyLen int) store.USD {
	out := req.cap()
	if out == 0 {
		out = prov.DefaultMaxOut
	}
	u := Usage{
		Model: req.Model,
		In:    int64(bodyLen / bytesPerToken),
		Out:   out,
	}
	usd, _ := Cost(prov.Name, u)
	return usd
}

// identify assigns call_id. The SDKs retry internally without stamping a
// logical id, so the proxy infers one: a new attempt joins the previous
// call_id iff the previous attempt on this span failed retryably and the
// bodies hash identically. That also merges an agent that legitimately resends
// an identical body after a real error, which is the correct answer anyway.
//
// ponytail: reads the last row without a lock, so two genuinely concurrent
// calls on ONE span could both claim attempt N+1. One span is one agent turn,
// which is sequential by construction; if a harness ever fans out inside a
// single span, this becomes a transaction.
func (p *Proxy) identify(c *call) {
	if prev, ok := p.db.LastCall(c.span.ID); ok &&
		prev.Retry && prev.BodyHash == c.hash &&
		c.started.Sub(prev.End) <= retryWindow {
		c.callID, c.attempt = prev.CallID, prev.Attempt+1
		return
	}
	// A fresh logical call gets a fresh random id. The obvious cheaper thing —
	// hashing span+body with a timestamp mixed in — was here and was wrong: it
	// took only the low byte of UnixNano, so two genuinely distinct calls with
	// the same body on the same span collided 1 time in 256 and silently
	// merged. COUNT(DISTINCT call_id) is a published number; a 0.4% quiet
	// under-count is worse than no number. rand.Text cannot fail and cannot
	// collide.
	c.callID, c.attempt = rand.Text(), 1
}

// retryWindow bounds how long after a failed attempt its retry may still join
// the same logical call. Without it the rule has no clock at all: an agent that
// fails a call, spends twenty minutes building and running a fuzz target, then
// resends the identical body is welded onto the old call_id and the logical
// call count silently under-reports.
//
// The bound is the client's own, read out of anthropic-sdk-python on
// 2026-09-02 rather than recalled:
//
//	_constants.py    DEFAULT_MAX_RETRIES = 2   -> 3 attempts per call, at most
//	                 INITIAL_RETRY_DELAY = 0.5
//	                 MAX_RETRY_DELAY     = 8.0 -> backoff never sleeps past 8s
//	_base_client.py  _calculate_retry_timeout obeys `retry-after` when
//	                 0 < retry_after <= 60
//
// 60s is therefore the widest gap the SDK will ever leave between one attempt
// ending and the next starting; 90 is that plus slop for scheduling.
//
// Measured from the END of the previous attempt (last_seen), never its start: a
// streaming attempt can burn the client's full 10-minute timeout before it
// fails, and the retry half a second later is still the same call.
const retryWindow = 90 * time.Second

// willRetry answers the only question the merge rule actually asks: is this
// client about to send the same body again? Not "was this error retryable in
// principle" — so it reproduces the SDK's own predicate instead of inventing a
// second taxonomy beside it.
//
// From _base_client.py _should_retry, in its order:
//
//	x-should-retry: true   -> retry, whatever the status says
//	x-should-retry: false  -> do not retry, whatever the status says
//	408 request timeout, 409 lock timeout, 429 rate limit, >= 500  -> retry
//	anything else          -> do not
//
// 529 overloaded_error needs no case of its own; it is >= 500. The header is
// checked first and overrides in BOTH directions, which is the half the
// outcome-string version could not express: a 500 stamped `x-should-retry:
// false` is permanent, and merging the next identical body onto it is wrong.
func willRetry(status int, h http.Header) bool {
	switch h.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return status == 408 || status == 409 || status == 429 || status >= 500
}

func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	c := r.In.Context().Value(ctxKey{}).(*call)
	up := c.prov.Upstream
	r.Out.URL.Scheme = up.Scheme
	r.Out.URL.Host = up.Host
	r.Out.Host = up.Host                                       // Host header and TLS SNI both follow this
	r.Out.URL.Path = strings.TrimSuffix(up.Path, "/") + c.path // /s/<id>/<prov> stripped

	// The container never holds a credential for ANY provider. Whatever it sent
	// is discarded unread, and the real key is attached here and only here.
	r.Out.Header.Del("Authorization")
	r.Out.Header.Del("X-Api-Key")
	c.prov.Auth(r.Out.Header)

	// Drop the client's Accept-Encoding so http.Transport adds its own gzip and
	// therefore transparently decodes the body for us (Transport doc: "If the
	// Transport requests gzip on its own and gets a gzipped response, it's
	// transparently decoded"). We keep compression on the expensive leg and
	// still see plaintext SSE in the tap. Forwarding the container's header
	// instead would hand us gzip bytes and silently zero every usage row.
	r.Out.Header.Del("Accept-Encoding")
}

func (p *Proxy) modifyResponse(res *http.Response) error {
	c := res.Request.Context().Value(ctxKey{}).(*call)

	if res.StatusCode != http.StatusOK {
		// Small, bounded, and worth reading: the outcome string should be the
		// provider's own error.type, not a status code we made up.
		b, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(b))
		res.ContentLength = int64(len(b))
		res.Header.Set("Content-Length", strconv.Itoa(len(b)))

		// A refused request generates nothing, so the true charge is zero and
		// we know it exactly: usage_complete=1 with zeros, not an unknown.
		// Anything else makes a retry storm look like unmeasured spend.
		c.retry = willRetry(res.StatusCode, res.Header)
		p.finish(c, Usage{Model: c.model, Complete: true}, store.Failed,
			errorType(res.StatusCode, b), 0)
		return nil
	}

	ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	t := &tap{src: res.Body, sse: ct == "text/event-stream", c: c, p: p, parse: c.prov.Parse()}
	if !t.sse {
		t.buf = new(bytes.Buffer)
	}
	res.Body = t // pass-through: bytes and Content-Length are untouched
	return nil
}

// errorHandler covers the paths where no response ever arrives: DNS, TLS,
// connection reset, or the client vanishing before the upstream answered.
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	c, ok := r.Context().Value(ctxKey{}).(*call)
	if ok {
		class, outcome := store.Failed, "transport_error"
		// APIConnectionError / APITimeoutError are retried unconditionally by
		// _should_retry_exception. A client that has gone away is not going to
		// retry anything.
		c.retry = true
		if errors.Is(err, context.Canceled) {
			class, outcome, c.retry = store.Cancelled, "client_gone", false
		}
		// Nothing was generated: release the whole reservation.
		p.finish(c, Usage{Model: c.model, Complete: true}, class, outcome, 0)
	}
	w.WriteHeader(http.StatusBadGateway)
}

// ---- the tap -------------------------------------------------------------

// tap sits between the upstream body and ReverseProxy's copy loop. It parses
// usage out of bytes on their way past and never holds them up.
//
// The obvious shape — io.TeeReader into a bufio.Scanner — needs an io.Pipe and
// a goroutine, which buys three failure modes (the scanner's backpressure
// stalling the agent, a leaked goroutine when the client disconnects and
// nobody drains the pipe, and a race between the copy finishing and the
// scanner finishing) for zero benefit. Parsing inline in Read is synchronous,
// allocation-free per event, and needs no synchronisation at all: Read and
// Close are called by exactly one goroutine, ReverseProxy's.
type tap struct {
	src   io.ReadCloser
	sse   bool
	c     *call
	p     *Proxy
	parse Parser
	line  []byte        // partial SSE line carried between Reads
	buf   *bytes.Buffer // non-SSE only
	once  sync.Once
}

func (t *tap) Read(b []byte) (int, error) {
	n, err := t.src.Read(b)
	if n > 0 {
		t.observe(b[:n])
	}
	return n, err
}

func (t *tap) observe(b []byte) {
	if !t.sse {
		if t.buf.Len() < maxJSONBody {
			t.buf.Write(b)
		}
		return
	}
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if len(t.line)+len(b) <= maxSSELine {
				t.line = append(t.line, b...)
			}
			return
		}
		line := b[:i]
		if len(t.line) > 0 {
			t.line = append(t.line, line...)
			line = t.line
		}
		t.parse.Event(bytes.TrimSuffix(line, []byte("\r")))
		t.line = t.line[:0] // after Event(), which does not retain the slice
		b = b[i+1:]
	}
}

// Close is the single write point for the row, and ReverseProxy guarantees it
// runs exactly once on every path including client disconnect.
func (t *tap) Close() error {
	err := t.src.Close()
	t.once.Do(func() {
		if !t.sse {
			t.parse.Body(t.buf.Bytes())
		}
		u := t.parse.Usage()
		if u.Model == "" {
			u.Model = t.c.model
		}
		usd, _ := Cost(t.c.prov.Name, u)
		switch {
		case u.Err != "":
			// Partial generation then a mid-stream error. Charge the
			// reservation: what was produced before the break is unknown.
			//
			// The HTTP status here is 200, so willRetry cannot help — classify
			// by the frame's error.type instead. And note what the SDK does
			// with this: _streaming.py raises out of the ITERATOR, long after
			// _request returned its 200 and the retry loop went away. So the
			// SDK does not retry a mid-stream error at all; whatever comes next
			// is the harness's own retry, on its own clock. retryWindow is the
			// only thing bounding that, and this is the merge case most likely
			// to be wrong in either direction.
			t.c.retry = u.Err == "overloaded_error" ||
				u.Err == "api_error" || u.Err == "timeout_error"
			t.p.finish(t.c, u, store.Failed, u.Err, maxUSD(usd, t.c.reserved))
		case !u.Complete:
			// The client went away, or the stream broke, before message_stop.
			// The row is written anyway with usage_complete=0 and the
			// reservation as the charge. A killed run reading as zero spend is
			// the exact failure this ledger exists to prevent; over-charging a
			// rare abandoned stream is the recoverable direction, and
			// reconciliation filters on usage_complete=1 for the strict compare.
			//
			// c.retry stays false. A broken stream surfaces as a read error
			// inside the SDK's iterator, again after the retry loop has
			// returned, so the SDK does not retry it either. Leaving it false
			// means a harness that resends after a truncated stream gets a new
			// call_id: attempts and calls both go up by one, which over-counts
			// calls rather than hiding a real second call inside the first.
			t.p.finish(t.c, u, store.Cancelled, "stream_incomplete", maxUSD(usd, t.c.reserved))
		default:
			t.p.finish(t.c, u, store.OK, "", usd)
		}
	})
	return err
}

func maxUSD(a, b store.USD) store.USD {
	if a > b {
		return a
	}
	return b
}

// finish trues the reservation up to the real charge and appends the row.
// Once per call, whichever hook gets there first.
func (p *Proxy) finish(c *call, u Usage, class store.Class, outcome string, usd store.USD) {
	c.once.Do(func() {
		p.leave(c.span.ID)
		if u.Model == "" {
			u.Model = c.model
		}
		_, known := rateFor(c.prov.Name, u.Model)
		// The true-up is unconditional and may push the pool past its cap; see
		// store.TrueUp. c.reserved is zero on the refusal path, which is why
		// that path cannot drive spent negative.
		p.db.TrueUp(c.span.Pool, usd-c.reserved)
		meta, _ := json.Marshal(map[string]any{
			"body":   c.hash, // request body hash; never the body itself
			"stream": c.stream,
			"path":   c.path,
			"retry":  c.retry, // will the client resend? read back by identify()
			"prov":   c.prov.Name,
		})
		p.db.Append(store.Row{
			TS:            c.started,
			LastSeen:      time.Now(), // ts..last_seen is the call's wall clock, no UPDATE needed
			Span:          c.span.ID,
			Parent:        c.span.Parent,
			Run:           c.span.Run,
			CallID:        c.callID,
			Attempt:       c.attempt,
			Kind:          "call",
			Class:         class,
			Outcome:       outcome,
			Model:         u.Model,
			In:            u.In,
			Out:           u.Out,
			CacheR:        u.CacheR,
			CacheW:        u.CacheW,
			USD:           usd,
			PriceKnown:    known,
			UsageComplete: u.Complete,
			Meta:          string(meta),
		})
	})
}

// errorType pulls error.type out of the documented envelope
// {"type":"error","error":{"type":..., "message":...},"request_id":...} and
// falls back to the status code's documented type when the body is unusable.
func errorType(status int, body []byte) string {
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Type != "" {
		return e.Error.Type
	}
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 500:
		return "api_error"
	case 504:
		return "timeout_error"
	case 529:
		return "overloaded_error"
	}
	return fmt.Sprintf("http_%d", status)
}
