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

// prices is keyed by API model id. The doc's table is keyed by display name, so
// the ids here are the ones the API actually echoes back in `model`.
//
// This map does NOT have to be complete. An id that misses is charged at
// maxRate and flagged price_known=0 — unknown models never gate traffic and are
// never free.
var prices = map[string]Rate{
	"claude-fable-5-1":  {10, 12.50, 20, 0.25, 50},
	"claude-mythos-5-1": {10, 12.50, 20, 0.25, 50},
	"claude-fable-5":    {10, 12.50, 20, 1.00, 50},
	"claude-mythos-5":   {10, 12.50, 20, 1.00, 50},
	"claude-opus-5":     {5, 6.25, 10, 0.50, 25},
	"claude-opus-4-8":   {5, 6.25, 10, 0.50, 25},
	"claude-opus-4-7":   {5, 6.25, 10, 0.50, 25},
	"claude-opus-4-6":   {5, 6.25, 10, 0.50, 25},
	"claude-opus-4-5":   {5, 6.25, 10, 0.50, 25},
	"claude-opus-4-1":   {15, 18.75, 30, 1.50, 75},
	"claude-opus-4":     {15, 18.75, 30, 1.50, 75},
	"claude-sonnet-5":   {2, 2.50, 4, 0.20, 10},
	"claude-sonnet-4-6": {3, 3.75, 6, 0.30, 15},
	"claude-sonnet-4-5": {3, 3.75, 6, 0.30, 15},
	"claude-sonnet-4":   {3, 3.75, 6, 0.30, 15},
	"claude-haiku-4-5":  {1, 1.25, 2, 0.10, 5},
	"claude-haiku-3-5":  {0.80, 1, 1.60, 0.08, 4},
}

// maxRate is the ceiling charged to an unrecognized model.
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

// rateFor resolves a model id to a rate. Exact match first, then longest
// matching prefix, because the API echoes dated ids ("claude-opus-4-5-20251101")
// that alias an undated entry.
func rateFor(model string) (Rate, bool) {
	if r, ok := prices[model]; ok {
		return r, true
	}
	best, bestLen := Rate{}, 0
	for id, r := range prices {
		if len(id) > bestLen && strings.HasPrefix(model, id) {
			best, bestLen = r, len(id)
		}
	}
	if bestLen > 0 {
		return best, true
	}
	return maxRate, false
}

// Cost prices a usage snapshot. known is false when the model was unrecognized
// and the ceiling rate was applied instead.
func Cost(u Usage) (usd store.USD, known bool) {
	r, known := rateFor(u.Model)
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
