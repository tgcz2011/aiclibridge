// Package pricing holds the token-pricing table used by the stats API to
// estimate run cost from token usage.
//
// Prices are per-million-tokens in USD and are keyed by `provider/model`
// (NOT by CLI — the same model served through different CLIs but the same
// upstream provider has one price). The table is sourced from each
// vendor's public pricing page as of 2025-08 and may change at any time;
// it is for estimation only, not billing. A zero price means "unknown or
// not token-billed" (e.g. image/video generation is per-call, some newer
// CLIs have no public token price).
package pricing

import (
	"sort"

	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// Price is the per-million-token USD rate for one provider/model pair.
// Any field may be zero when the vendor does not publish that component
// (e.g. non-cacheable models leave cache rates at zero) or when the whole
// pair is unpriced.
type Price struct {
	InputPerMillion      float64
	OutputPerMillion     float64
	CacheReadPerMillion  float64
	CacheWritePerMillion float64
}

// priceKey joins provider and model into the table key. The separator "/"
// matches the `CLI/provider/model` convention's middle segment so a key
// never collides with a fully-qualified model id.
func priceKey(provider, model string) string {
	return provider + "/" + model
}

// prices is the static price table. See the package doc for provenance
// and the zero convention.
//
// Anthropic (https://www.anthropic.com/pricing): claude-sonnet-4.5
// $3/$15, claude-opus-4.1 $15/$75, claude-haiku-4.5 $1/$5. Cache reads
// are billed at 10% of input; cache writes at 125% of input. Those
// multipliers are applied below so the stats API can price cache usage
// without a separate table.
//
// OpenAI (https://openai.com/api/pricing): gpt-5 $5/$15, gpt-5-mini
// $0.5/$2, o3 $10/$40. No published cache split for these; cache rates
// stay zero.
//
// Google (https://ai.google.dev/pricing): gemini-2.5-pro $1.25/$5,
// gemini-2.5-flash $0.075/$0.3.
//
// Alibaba (https://help.aliyun.com/zh/model-studio/billing): qwen3-coder-plus
// $0.5/$1.5, qwen3-max $2/$6.
//
// Bytedance doubao-seedream/seedance are image/video generation billed
// per call, not per token — zero here; the stats API reports $0 for them.
//
// Tencent codebuddy models (glm/minimax/kimi/deepseek/hy3 accessed via
// codebuddy CLI) have no public per-token price — zero here. Update as
// Tencent publishes rates.
//
// Newer v0.2 CLIs (github, block, cursor, moonshot, aws, qoder, nous,
// auggie) either resell models under opaque pricing or have no public
// token rate — zero. Update as vendors publish rates.
var prices = map[string]Price{
	// ── Anthropic ──
	priceKey("anthropic", "claude-sonnet-4.5"): {InputPerMillion: 3, OutputPerMillion: 15, CacheReadPerMillion: 0.3, CacheWritePerMillion: 3.75},
	priceKey("anthropic", "claude-opus-4.1"):   {InputPerMillion: 15, OutputPerMillion: 75, CacheReadPerMillion: 1.5, CacheWritePerMillion: 18.75},
	priceKey("anthropic", "claude-haiku-4.5"):  {InputPerMillion: 1, OutputPerMillion: 5, CacheReadPerMillion: 0.1, CacheWritePerMillion: 1.25},

	// ── OpenAI ──
	priceKey("openai", "gpt-5"):      {InputPerMillion: 5, OutputPerMillion: 15},
	priceKey("openai", "gpt-5-mini"): {InputPerMillion: 0.5, OutputPerMillion: 2},
	priceKey("openai", "o3"):         {InputPerMillion: 10, OutputPerMillion: 40},

	// ── Google ──
	priceKey("google", "gemini-2.5-pro"):   {InputPerMillion: 1.25, OutputPerMillion: 5},
	priceKey("google", "gemini-2.5-flash"): {InputPerMillion: 0.075, OutputPerMillion: 0.3},

	// ── Alibaba ──
	priceKey("alibaba", "qwen3-coder-plus"): {InputPerMillion: 0.5, OutputPerMillion: 1.5},
	priceKey("alibaba", "qwen3-max"):        {InputPerMillion: 2, OutputPerMillion: 6},

	// ── Bytedance (image/video gen: per-call, not token-billed) ──
	priceKey("bytedance", "doubao-seedream-4-0"): {},
	priceKey("bytedance", "doubao-seedance-1-0"): {},

	// ── Tencent / codebuddy (no public per-token price) ──
	priceKey("tencent", "glm-5.2"):          {},
	priceKey("tencent", "glm-5.1"):          {},
	priceKey("tencent", "glm-5.0"):          {},
	priceKey("tencent", "glm-5.0-turbo"):    {},
	priceKey("tencent", "glm-5v-turbo"):     {},
	priceKey("tencent", "glm-4.7"):          {},
	priceKey("tencent", "minimax-m3"):       {},
	priceKey("tencent", "minimax-m2.7"):     {},
	priceKey("tencent", "kimi-k2.7"):        {},
	priceKey("tencent", "kimi-k2.6"):        {},
	priceKey("tencent", "kimi-k2.5"):        {},
	priceKey("tencent", "hy3-preview"):      {},
	priceKey("tencent", "deepseek-v4-pro"):  {},
	priceKey("tencent", "deepseek-v4-flash"):{},
	priceKey("tencent", "deepseek-v3-2-volc"): {},

	// ── v0.2 CLIs (no public token price yet) ──
	priceKey("github", "gpt-5"):            {},
	priceKey("github", "claude-sonnet-4.5"): {},
	priceKey("github", "gemini-2.5-pro"):   {},
	priceKey("block", "goose-1"):           {},
	priceKey("cursor", "cursor-default"):   {},
	priceKey("moonshot", "kimi-k2"):        {},
	priceKey("aws", "kiro-default"):        {},
	priceKey("qoder", "qoder-default"):     {},
	priceKey("nous", "hermes-4"):           {},
	priceKey("auggie", "auggie-default"):   {},
}

// Lookup returns the price for a provider/model pair. The second return
// is false when the pair is not in the table at all (truly unknown);
// present-but-zero means "known but unpriced" (e.g. per-call billing).
func Lookup(provider, model string) (Price, bool) {
	p, ok := prices[priceKey(provider, model)]
	return p, ok
}

// EstimateCost computes the USD cost of a single run's usage for one
// provider/model pair. It is the per-million rate times the token count
// for each component (input, output, cache read, cache write). Unknown
// pairs return 0; the caller should treat 0 as "unpriced" rather than
// "free" when surfacing totals. The usage int fields are widened to
// float64; token counts fit comfortably in float64 for any realistic run.
func EstimateCost(provider, model string, usage protocol.TokenUsagePayload) float64 {
	p, ok := Lookup(provider, model)
	if !ok {
		return 0
	}
	const perMillion = 1_000_000
	return float64(usage.InputTokens)*p.InputPerMillion/perMillion +
		float64(usage.OutputTokens)*p.OutputPerMillion/perMillion +
		float64(usage.CacheReadTokens)*p.CacheReadPerMillion/perMillion +
		float64(usage.CacheWriteTokens)*p.CacheWritePerMillion/perMillion
}

// PriceEntry is one row of the exported price table, used by the
// /v1/stats/prices endpoint. It flattens the (provider, model, Price)
// triple so the JSON response is a flat list.
type PriceEntry struct {
	Provider             string  `json:"provider"`
	Model                string  `json:"model"`
	InputPerMillion      float64 `json:"input_per_million"`
	OutputPerMillion     float64 `json:"output_per_million"`
	CacheReadPerMillion  float64 `json:"cache_read_per_million"`
	CacheWritePerMillion float64 `json:"cache_write_per_million"`
}

// AllPrices returns every entry in the price table, sorted by
// (provider, model) for stable output. The /v1/stats/prices handler
// serialises this directly.
func AllPrices() []PriceEntry {
	out := make([]PriceEntry, 0, len(prices))
	for k, p := range prices {
		// Split "provider/model" back into components. The key was built
		// by priceKey so it always has exactly one "/".
		provider, model := splitKey(k)
		out = append(out, PriceEntry{
			Provider:             provider,
			Model:                model,
			InputPerMillion:      p.InputPerMillion,
			OutputPerMillion:     p.OutputPerMillion,
			CacheReadPerMillion:  p.CacheReadPerMillion,
			CacheWritePerMillion: p.CacheWritePerMillion,
		})
	}
	// Stable order: provider then model. Avoids map-iteration
	// nondeterminism in the HTTP response.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// splitKey reverses priceKey. A key with no "/" (never produced by
// priceKey) returns ("", key) defensively.
func splitKey(k string) (provider, model string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return "", k
}
