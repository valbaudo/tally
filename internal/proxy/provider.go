package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
)

// A Provider is one upstream API: where it lives, how it is authenticated, and
// how to read usage out of its answers. It is a struct with func fields rather
// than an interface because only Parse is stateful — Auth and Upstream are
// configuration, and an interface for configuration is an interface with one
// implementation per value.
//
// Wire facts verified 2026-09-03 against:
//
//	openai-python src/openai/types/completion_usage.py          (chat usage fields)
//	openai-python src/openai/types/responses/response_usage.py  (responses usage fields)
//	openai-python .../chat_completion_stream_options_param.py   (include_usage semantics)
//	docs.x.ai/docs/api-reference                                (xAI = chat usage + prompt_tokens_details)
//	docs.z.ai/api-reference/llm/chat-completion                 (GLM = chat usage + cached_tokens)
//	docs.z.ai/devpack/tool/claude                               (GLM also serves the Anthropic wire)
type Provider struct {
	Name     string
	Upstream *url.URL

	// Auth attaches the real credential to the OUTBOUND request. Whatever the
	// container sent has already been stripped by the caller.
	Auth func(h http.Header)

	// Parse returns a fresh parser for one call.
	Parse func() Parser

	// DefaultMaxOut is the reservation ceiling in output tokens when the
	// request names none. Anthropic requires max_tokens, so this is never used
	// there; OpenAI-shaped APIs let you omit every output cap, and a request
	// with no ceiling still has to be priced BEFORE it is forwarded.
	DefaultMaxOut int64

	// InjectUsage rewrites a streaming request body so usage is reported. This
	// is the proxy's only mutation of a payload, and it exists because OpenAI
	// Chat Completions reports NO usage on a stream unless the caller opted in
	// (stream_options.include_usage), and the caller is the untrusted agent.
	InjectUsage func(body []byte) []byte
}

// Parser reads one response. Event is called once per raw SSE line (the
// "data:" prefix included); Body once with a complete non-streaming body; both
// are called from ReverseProxy's single copy goroutine, so no parser needs a
// lock.
type Parser interface {
	Event(line []byte)
	Body(b []byte)
	Usage() Usage
}

// Providers builds the routing table. keys is provider name -> credential; a
// provider with no key is not registered, so a span can never be pointed at an
// upstream this sweep cannot pay for.
func Providers(keys map[string]string) map[string]Provider {
	m := map[string]Provider{}
	add := func(name string, build func(key string) Provider) {
		if k := keys[name]; k != "" {
			p := build(k)
			p.Name = name
			m[name] = p
		}
	}
	add("anthropic", func(k string) Provider {
		return anthropicProvider("https://api.anthropic.com", apiKeyHeader, k)
	})
	// Z.AI serves GLM on the Anthropic Messages wire at /api/anthropic, with a
	// bearer token instead of x-api-key. Same parser, different auth: this is
	// exactly why Auth is a field and not implied by the wire format.
	add("glm", func(k string) Provider {
		return anthropicProvider("https://api.z.ai/api/anthropic", bearerHeader, k)
	})
	add("openai", func(k string) Provider { return chatProvider("https://api.openai.com", k) })
	add("xai", func(k string) Provider { return chatProvider("https://api.x.ai", k) })
	add("openai-responses", func(k string) Provider {
		return responsesProvider("https://api.openai.com", k)
	})
	return m
}

func apiKeyHeader(h http.Header, key string) {
	h.Set("X-Api-Key", key)
	if h.Get("Anthropic-Version") == "" {
		h.Set("Anthropic-Version", AnthropicVersion)
	}
}

func bearerHeader(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) }

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic("proxy: bad upstream " + s)
	}
	return u
}

func anthropicProvider(upstream string, auth func(http.Header, string), key string) Provider {
	return Provider{
		Upstream: mustURL(upstream),
		Auth:     func(h http.Header) { auth(h, key) },
		Parse:    func() Parser { return &anthropicParser{} },
	}
}

func chatProvider(upstream, key string) Provider {
	return Provider{
		Upstream:      mustURL(upstream),
		Auth:          func(h http.Header) { bearerHeader(h, key) },
		Parse:         func() Parser { return &chatParser{} },
		DefaultMaxOut: defaultMaxOut,
		InjectUsage:   includeUsage,
	}
}

func responsesProvider(upstream, key string) Provider {
	return Provider{
		Upstream:      mustURL(upstream),
		Auth:          func(h http.Header) { bearerHeader(h, key) },
		Parse:         func() Parser { return &responsesParser{} },
		DefaultMaxOut: defaultMaxOut,
	}
}

// defaultMaxOut is the reservation ceiling for an OpenAI-shaped request that
// names no output cap at all. Chosen above any current model's max output so
// the reservation over-covers; the true-up releases the slack when the
// response lands.
const defaultMaxOut = 128 << 10

// ---- Anthropic ------------------------------------------------------------

// anthropicParser is the original tap logic, unchanged and now behind the
// interface. message_delta.usage is CUMULATIVE — apply overwrites, never sums.
type anthropicParser struct{ u Usage }

func (p *anthropicParser) Usage() Usage { return p.u }

func (p *anthropicParser) Event(line []byte) {
	data, ok := sseData(line)
	if !ok {
		return
	}
	var ev sseEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			p.u.Model = ev.Message.Model
			p.u.apply(ev.Message.Usage)
		}
	case "message_delta":
		if ev.Usage != nil {
			p.u.apply(*ev.Usage)
		}
	case "message_stop":
		p.u.Complete = true
	case "error":
		if ev.Error != nil {
			p.u.Err = ev.Error.Type
		}
	}
}

func (p *anthropicParser) Body(b []byte) {
	var m wireMessage
	if json.Unmarshal(b, &m) == nil && m.Usage.InputTokens+m.Usage.OutputTokens > 0 {
		p.u.Model = m.Model
		p.u.apply(m.Usage)
		p.u.Complete = true
	}
}

// ---- OpenAI Chat Completions (also xAI, also GLM's OpenAI-compatible port) --

// chatUsage is the shape shared by OpenAI Chat Completions, xAI and Zhipu GLM.
// All three publish exactly these names; the details objects are absent on some
// responses and unmarshal to zero, which is the correct reading.
//
// The trap this struct exists to record: prompt_tokens_details is a BREAKDOWN
// of prompt_tokens, so cached_tokens is already inside prompt_tokens. Anthropic
// is the other way round — cache_read_input_tokens is disjoint from
// input_tokens. Charging OpenAI's prompt_tokens at the full input rate AND
// cached_tokens at the cache rate double-charges the cached prefix, which on a
// long-context hunt agent is most of the bill.
type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptDetails    struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (c chatUsage) fold(u *Usage) {
	u.CacheR = c.PromptDetails.CachedTokens
	u.In = c.PromptTokens - c.PromptDetails.CachedTokens
	if u.In < 0 {
		u.In = c.PromptTokens
	}
	u.Out = c.CompletionTokens
}

type chatWire struct {
	Model string     `json:"model"`
	Usage *chatUsage `json:"usage"`
}

type chatParser struct{ u Usage }

func (p *chatParser) Usage() Usage { return p.u }

// Event reads one chunk. With stream_options.include_usage the final chunk
// before data: [DONE] carries usage and an EMPTY choices array; every earlier
// chunk carries usage: null. Seeing usage IS the terminal signal — there is no
// message_stop on this wire, and [DONE] is not JSON.
func (p *chatParser) Event(line []byte) {
	data, ok := sseData(line)
	if !ok || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var w chatWire
	if json.Unmarshal(data, &w) != nil {
		return
	}
	if w.Model != "" {
		p.u.Model = w.Model
	}
	if w.Usage != nil {
		w.Usage.fold(&p.u)
		p.u.Complete = true
	}
}

func (p *chatParser) Body(b []byte) {
	var w chatWire
	if json.Unmarshal(b, &w) == nil && w.Usage != nil {
		p.u.Model = w.Model
		w.Usage.fold(&p.u)
		p.u.Complete = true
	}
}

// includeUsage is the one place the proxy edits an agent's request. Without it
// a streaming Chat Completions call reports nothing at all and every row lands
// usage_complete=0 charged at the reservation — the ledger degrades to a
// guess. The added final chunk is documented behaviour that every OpenAI SDK
// already tolerates, so this cannot break a client that did not ask for it.
func includeUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["stream"]; !ok {
		return body
	}
	var stream bool
	if json.Unmarshal(m["stream"], &stream) != nil || !stream {
		return body
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// ---- OpenAI Responses ------------------------------------------------------

// responsesUsage is the Responses API shape. Note the names are NOT the chat
// names: input_tokens / output_tokens, and the cached count again lives inside
// the input total.
type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type responsesEvent struct {
	Type     string `json:"type"`
	Response *struct {
		Model string          `json:"model"`
		Usage *responsesUsage `json:"usage"`
	} `json:"response"`
}

type responsesParser struct{ u Usage }

func (p *responsesParser) Usage() Usage { return p.u }

// Event: usage arrives once, on the terminal event, inside the whole response
// object. response.failed and response.incomplete carry it too — tokens were
// generated and must be charged.
func (p *responsesParser) Event(line []byte) {
	data, ok := sseData(line)
	if !ok {
		return
	}
	var ev responsesEvent
	if json.Unmarshal(data, &ev) != nil || ev.Response == nil {
		return
	}
	if ev.Response.Model != "" {
		p.u.Model = ev.Response.Model
	}
	switch ev.Type {
	case "response.completed", "response.failed", "response.incomplete":
		if u := ev.Response.Usage; u != nil {
			p.u.In = u.InputTokens - u.InputDetails.CachedTokens
			if p.u.In < 0 {
				p.u.In = u.InputTokens
			}
			p.u.CacheR = u.InputDetails.CachedTokens
			p.u.Out = u.OutputTokens
			p.u.Complete = true
		}
		if ev.Type == "response.failed" {
			p.u.Err = "response_failed"
		}
	}
}

func (p *responsesParser) Body(b []byte) {
	var r struct {
		Model string          `json:"model"`
		Usage *responsesUsage `json:"usage"`
	}
	if json.Unmarshal(b, &r) != nil || r.Usage == nil {
		return
	}
	p.u.Model = r.Model
	p.u.In = r.Usage.InputTokens - r.Usage.InputDetails.CachedTokens
	if p.u.In < 0 {
		p.u.In = r.Usage.InputTokens
	}
	p.u.CacheR = r.Usage.InputDetails.CachedTokens
	p.u.Out = r.Usage.OutputTokens
	p.u.Complete = true
}

// ---- shared ---------------------------------------------------------------

var dataPrefix = []byte("data:")

// sseData strips the "data:" prefix. Only data lines carry JSON on every wire
// here; "event:" name lines are redundant because each payload repeats its own
// type, and Responses relies on that.
func sseData(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, dataPrefix) {
		return nil, false
	}
	return bytes.TrimSpace(line[len(dataPrefix):]), true
}
