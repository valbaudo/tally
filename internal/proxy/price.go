package proxy

import (
	"strings"

	"github.com/valbaudo/dawn/internal/store"
)

// Rate is dollars per million tokens for one model.
//
// Verified 2026-09-02 against https://platform.claude.com/docs/en/about-claude/pricing
// (table "Model pricing", columns: Base input / 5m cache writes / 1h cache
// writes / Cache hits and refreshes / Output tokens).
type Rate struct{ In, Cache5m, Cache1h, CacheRead, Out float64 }

// prices is keyed "<provider>/<model id>". The provider prefix is not
// decoration: model ids collide across vendors (an OpenAI-compatible gateway
// will happily echo back a name it reroutes), and a bare-id map silently prices
// one vendor's traffic at another's rate. The id half is the one the API echoes
// back in `model`, not the doc's display name.
//
// This map does NOT have to be complete. A key that misses is charged at
// maxRate and flagged price_known=0 — unknown models never gate traffic and are
// never free.
//
// Non-Anthropic rows would leave Cache5m/Cache1h at zero: those wires have no
// billed cache WRITE — OpenAI, xAI and GLM all discount reads off the prompt
// total and charge nothing to populate the cache. If that stops being true the
// parsers need a cache-write field before this map does.
var prices = map[string]Rate{
	"anthropic/claude-fable-5-1":  {10, 12.50, 20, 0.25, 50},
	"anthropic/claude-mythos-5-1": {10, 12.50, 20, 0.25, 50},
	"anthropic/claude-fable-5":    {10, 12.50, 20, 1.00, 50},
	"anthropic/claude-mythos-5":   {10, 12.50, 20, 1.00, 50},
	"anthropic/claude-opus-5":     {5, 6.25, 10, 0.50, 25},
	"anthropic/claude-opus-4-8":   {5, 6.25, 10, 0.50, 25},
	"anthropic/claude-opus-4-7":   {5, 6.25, 10, 0.50, 25},
	"anthropic/claude-opus-4-6":   {5, 6.25, 10, 0.50, 25},
	"anthropic/claude-opus-4-5":   {5, 6.25, 10, 0.50, 25},
	"anthropic/claude-opus-4-1":   {15, 18.75, 30, 1.50, 75},
	"anthropic/claude-opus-4":     {15, 18.75, 30, 1.50, 75},
	"anthropic/claude-sonnet-5":   {2, 2.50, 4, 0.20, 10},
	"anthropic/claude-sonnet-4-6": {3, 3.75, 6, 0.30, 15},
	"anthropic/claude-sonnet-4-5": {3, 3.75, 6, 0.30, 15},
	"anthropic/claude-sonnet-4":   {3, 3.75, 6, 0.30, 15},
	"anthropic/claude-haiku-4-5":  {1, 1.25, 2, 0.10, 5},
	"anthropic/claude-haiku-3-5":  {0.80, 1, 1.60, 0.08, 4},

	// openai/, openai-responses/, xai/ and glm/ rows are deliberately ABSENT
	// rather than filled in from memory. A wrong price published as fact is
	// worse than the honest price_known=0 an absent key already produces. Fill
	// each from the vendor's own page the first time a sweep uses it:
	//   openai, openai-responses  platform.openai.com/docs/pricing
	//   xai                       docs.x.ai/docs/models
	//   glm                       docs.z.ai/guides/overview/pricing
}

// maxRate is the ceiling charged to an unrecognized model. It is the max over
// EVERY provider's rates, not the calling provider's — a sweep that adds one
// cheap OpenAI-compatible gateway must not thereby lower the ceiling that
// protects it from an unknown expensive model, and that ceiling is the only
// thing between an unpriced model and unbounded spend.
var maxRate = func() Rate {
	var m Rate
	for _, r := range prices {
		m.In = max(m.In, r.In)
		m.Cache5m = max(m.Cache5m, r.Cache5m)
		m.Cache1h = max(m.Cache1h, r.Cache1h)
		m.CacheRead = max(m.CacheRead, r.CacheRead)
		m.Out = max(m.Out, r.Out)
	}
	return m
}()

// rateFor resolves a provider+model to a rate. Exact match first, then longest
// prefix WITHIN the same provider, so a dated id
// ("anthropic/claude-opus-5-20260401") inherits its family's price and a new
// dated release does not silently fall to the ceiling. bestLen must exceed the
// provider prefix itself, or "xai/" would match nothing and still claim known.
func rateFor(prov, model string) (Rate, bool) {
	key := prov + "/" + model
	if r, ok := prices[key]; ok {
		return r, true
	}
	best, bestLen := Rate{}, 0
	for id, r := range prices {
		if len(id) > bestLen && strings.HasPrefix(key, id) {
			best, bestLen = r, len(id)
		}
	}
	if bestLen > len(prov)+1 {
		return best, true
	}
	return maxRate, false
}

func Cost(prov string, u Usage) (usd store.USD, known bool) {
	r, known := rateFor(prov, u.Model)
	// The single cache_w column loses the 5m/1h split, so price from the
	// breakdown when the response carried one and fall back to the 1h (higher)
	// rate when it did not. Over-charging is recoverable; under-charging is the
	// failure this ledger exists to prevent.
	w5, w1 := u.CacheW5m, u.CacheW1h
	if w5+w1 != u.CacheW {
		w5, w1 = 0, u.CacheW
	}
	cents := float64(u.In)*r.In +
		float64(u.Out)*r.Out +
		float64(u.CacheR)*r.CacheRead +
		float64(w5)*r.Cache5m +
		float64(w1)*r.Cache1h
	return store.USD(cents / 1e6), known
}
