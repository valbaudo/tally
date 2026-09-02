package proxy

import (
	"encoding/json"
	"testing"
)

// One check per wire fact this file claims. Each fails if the claim is wrong.
func TestParsers(t *testing.T) {
	// OpenAI Chat Completions, streaming with stream_options.include_usage:
	// usage arrives in a final chunk whose choices array is empty.
	// prompt_tokens INCLUDES cached_tokens, so In must be the difference.
	t.Run("chat stream", func(t *testing.T) {
		p := &chatParser{}
		p.Event([]byte(`data: {"model":"gpt-5.6","choices":[{"delta":{"content":"hi"}}],"usage":null}`))
		if u := p.Usage(); u.Complete {
			t.Fatal("a content chunk must not look terminal")
		}
		p.Event([]byte(`data: {"model":"gpt-5.6","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":900}}}`))
		p.Event([]byte(`data: [DONE]`))
		u := p.Usage()
		if u.In != 100 || u.CacheR != 900 || u.Out != 40 || !u.Complete || u.Model != "gpt-5.6" {
			t.Fatalf("%+v: want In=100 (1000-900), CacheR=900, Out=40, Complete", u)
		}
	})

	// OpenAI Responses: usage arrives once, inside the whole response object on
	// the terminal event, under DIFFERENT names than the chat wire.
	t.Run("responses stream", func(t *testing.T) {
		p := &responsesParser{}
		p.Event([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}`))
		p.Event([]byte(`data: {"type":"response.completed","response":{"model":"gpt-5.6-sol","usage":{"input_tokens":1000,"output_tokens":40,"input_tokens_details":{"cached_tokens":900},"output_tokens_details":{"reasoning_tokens":12}}}}`))
		u := p.Usage()
		if u.In != 100 || u.CacheR != 900 || u.Out != 40 || !u.Complete {
			t.Fatalf("%+v: want In=100, CacheR=900, Out=40, Complete", u)
		}
	})

	// Anthropic is the OTHER convention: cache_read_input_tokens is disjoint
	// from input_tokens, and message_delta repeats both cumulatively.
	t.Run("anthropic stream", func(t *testing.T) {
		p := &anthropicParser{}
		p.Event([]byte(`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":25,"cache_read_input_tokens":1800}}}`))
		p.Event([]byte(`data: {"type":"message_delta","usage":{"input_tokens":25,"cache_read_input_tokens":1800,"output_tokens":15}}`))
		p.Event([]byte(`data: {"type":"message_stop"}`))
		u := p.Usage()
		if u.In != 25 || u.CacheR != 1800 || u.Out != 15 || !u.Complete {
			t.Fatalf("%+v: want In=25 (NOT netted against cache), CacheR=1800, Out=15", u)
		}
	})
}

// Without this injection an agent's streaming Chat Completions call reports no
// usage at all, and every row lands usage_complete=0 charged at the ceiling.
func TestIncludeUsageInjection(t *testing.T) {
	out := includeUsage([]byte(`{"model":"gpt-5.6","stream":true,"messages":[]}`))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["stream_options"]) != `{"include_usage":true}` {
		t.Fatalf("stream_options = %s", m["stream_options"])
	}
	if string(m["messages"]) != "[]" {
		t.Fatal("injection must not disturb the rest of the body")
	}
	// A non-streaming body is left byte-identical: there is nothing to opt into.
	in := []byte(`{"model":"gpt-5.6","messages":[]}`)
	if string(includeUsage(in)) != string(in) {
		t.Fatal("non-streaming body was rewritten")
	}
}

func TestSplitSpanPath(t *testing.T) {
	span, prov, rest, ok := splitSpanPath("/s/abc123/openai/v1/chat/completions")
	if !ok || span != "abc123" || prov != "openai" || rest != "/v1/chat/completions" {
		t.Fatalf("%q %q %q %v", span, prov, rest, ok)
	}
	// A span id with no provider segment is not a route. It used to be one, and
	// silently defaulting it to Anthropic would send an OpenAI body upstream
	// with an Anthropic key attached.
	if _, _, _, ok := splitSpanPath("/s/abc123/v1"); ok {
		t.Fatal("/s/<id>/<one segment> must not resolve; the segment is the provider")
	}
}

// An unpriced model on ANY provider is charged the global ceiling, never zero.
func TestUnknownProviderModelChargesCeiling(t *testing.T) {
	usd, known := Cost("xai", Usage{Model: "grok-5", In: 1_000_000})
	if known {
		t.Fatal("xai/grok-5 has no row; known must be false")
	}
	if float64(usd) != maxRate.In {
		t.Fatalf("usd = %v, want the global ceiling %v", usd, maxRate.In)
	}
}
