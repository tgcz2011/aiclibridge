package api

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/detect"
	"github.com/tgcz2011/aiclibridge/internal/pricing"
	"github.com/tgcz2011/aiclibridge/internal/store"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// ── Stats API handlers ──
//
// Three read-only endpoints back the usage/cost dashboard:
//   - GET /v1/stats/usage   — per (adapter, model, status) token buckets
//     with an estimated USD cost per bucket.
//   - GET /v1/stats/prices  — the full pricing table (provider/model →
//     per-million rates) so clients can re-price locally.
//   - GET /v1/stats/summary — totals + a per-adapter cost breakdown.
//
// All three require auth (registered with chain(..., true) in server.go).
// The store supplies aggregated rows; the pricing table + the catalog's
// adapter→provider mapping turn tokens into USD. usage_json keys are the
// adapter's internal model names (e.g. "claude-sonnet-4.5"), NOT the
// "provider/model" form, so the provider is resolved per row from the
// catalog before pricing.

// statsUsageRow is one (adapter, model, status) bucket in the usage
// response. It mirrors store.UsageStatRow plus the estimated USD cost
// computed via the pricing table.
type statsUsageRow struct {
	Adapter           string  `json:"adapter"`
	Model             string  `json:"model"`
	Status            string  `json:"status"`
	RunCount          int64   `json:"run_count"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheWriteTokens  int64   `json:"cache_write_tokens"`
	EstimatedCostUSD  float64 `json:"estimated_cost_usd"`
}

// statsPricesEnvelope wraps the price table for /v1/stats/prices.
type statsPricesEnvelope struct {
	Prices []pricing.PriceEntry `json:"prices"`
}

// statsSummary is the /v1/stats/summary response: window bounds, token
// totals, total estimated cost, and a per-adapter breakdown.
type statsSummary struct {
	Since                int64                  `json:"since"`
	Until                int64                  `json:"until"`
	TotalRuns            int64                  `json:"total_runs"`
	TotalInputTokens     int64                  `json:"total_input_tokens"`
	TotalOutputTokens    int64                  `json:"total_output_tokens"`
	TotalCacheReadTokens int64                  `json:"total_cache_read_tokens"`
	TotalCacheWriteTokens int64                 `json:"total_cache_write_tokens"`
	TotalEstimatedCostUSD float64               `json:"total_estimated_cost_usd"`
	ByAdapter            []statsSummaryByAdapter `json:"by_adapter"`
}

// statsSummaryByAdapter is one adapter's contribution to the summary.
type statsSummaryByAdapter struct {
	Adapter             string  `json:"adapter"`
	RunCount            int64   `json:"run_count"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheWriteTokens    int64   `json:"cache_write_tokens"`
	EstimatedCostUSD    float64 `json:"estimated_cost_usd"`
}

// statsDefaultWindowDays is the default lookback when `since` is omitted.
const statsDefaultWindowDays = 7

// handleStatsUsage returns per-(adapter, model, status) token buckets for
// the requested window, each priced via the catalog + pricing table.
func (s *Server) handleStatsUsage(w http.ResponseWriter, r *http.Request) {
	since, until, ok := parseStatsWindow(r)
	if !ok {
		writeError(w, http.StatusBadRequest,
			"invalid since/until: must be unix seconds", "invalid_request_error", nil)
		return
	}
	rows, err := s.fc.GetUsageStats(r.Context(), since, until)
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"usage stats failed: "+err.Error(), "upstream_error", err)
		return
	}
	providerMap := buildAdapterModelProviderMap()
	out := make([]statsUsageRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, statsUsageRow{
			Adapter:          row.Adapter,
			Model:            row.Model,
			Status:           row.Status,
			RunCount:         row.RunCount,
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
			EstimatedCostUSD: estimateRowCost(providerMap, row),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out})
}

// handleStatsPrices returns the full pricing table so clients can re-price
// locally without re-implementing the rate lookup.
func (s *Server) handleStatsPrices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, statsPricesEnvelope{Prices: pricing.AllPrices()})
}

// handleStatsSummary returns window totals plus a per-adapter cost
// breakdown, suitable for a dashboard header.
func (s *Server) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	since, until, ok := parseStatsWindow(r)
	if !ok {
		writeError(w, http.StatusBadRequest,
			"invalid since/until: must be unix seconds", "invalid_request_error", nil)
		return
	}
	rows, err := s.fc.GetUsageStats(r.Context(), since, until)
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"usage stats failed: "+err.Error(), "upstream_error", err)
		return
	}
	providerMap := buildAdapterModelProviderMap()

	summary := statsSummary{Since: since, Until: until}
	byAdapter := make(map[string]*statsSummaryByAdapter)
	for _, row := range rows {
		cost := estimateRowCost(providerMap, row)
		summary.TotalRuns += row.RunCount
		summary.TotalInputTokens += row.InputTokens
		summary.TotalOutputTokens += row.OutputTokens
		summary.TotalCacheReadTokens += row.CacheReadTokens
		summary.TotalCacheWriteTokens += row.CacheWriteTokens
		summary.TotalEstimatedCostUSD += cost

		ba := byAdapter[row.Adapter]
		if ba == nil {
			ba = &statsSummaryByAdapter{Adapter: row.Adapter}
			byAdapter[row.Adapter] = ba
		}
		ba.RunCount += row.RunCount
		ba.InputTokens += row.InputTokens
		ba.OutputTokens += row.OutputTokens
		ba.CacheReadTokens += row.CacheReadTokens
		ba.CacheWriteTokens += row.CacheWriteTokens
		ba.EstimatedCostUSD += cost
	}
	for _, ba := range byAdapter {
		summary.ByAdapter = append(summary.ByAdapter, *ba)
	}
	// Stable order by adapter name for predictable output.
	sort.Slice(summary.ByAdapter, func(i, j int) bool {
		return summary.ByAdapter[i].Adapter < summary.ByAdapter[j].Adapter
	})
	if summary.ByAdapter == nil {
		summary.ByAdapter = []statsSummaryByAdapter{}
	}
	writeJSON(w, http.StatusOK, summary)
}

// parseStatsWindow extracts the since/until unix-second query params.
// Missing `since` defaults to now-7d; missing `until` defaults to now.
// Returns ok=false (and writes nothing) if a present value fails to parse
// as an integer; the caller surfaces a 400.
func parseStatsWindow(r *http.Request) (since, until int64, ok bool) {
	now := time.Now().Unix()
	since = now - int64(statsDefaultWindowDays)*86400
	until = now
	if v := r.URL.Query().Get("since"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		since = parsed
	}
	if v := r.URL.Query().Get("until"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		until = parsed
	}
	return since, until, true
}

// buildAdapterModelProviderMap derives an adapter→model→provider mapping
// from the default catalog. The usage_json stored on a run keys tokens by
// the adapter's internal model name (e.g. "claude-sonnet-4.5"), and the
// pricing table is keyed by provider/model, so the provider must be
// resolved per (adapter, model) before Lookup. The catalog is static, so
// this map is rebuilt per call (cheap: ~20 CLIs).
func buildAdapterModelProviderMap() map[string]map[string]string {
	m := make(map[string]map[string]string)
	for _, cli := range detect.DefaultCatalog() {
		inner := make(map[string]string)
		for _, p := range cli.Providers {
			for _, mod := range p.Models {
				inner[mod.Name] = p.Name
			}
		}
		m[cli.Name] = inner
	}
	return m
}

// estimateRowCost prices one usage bucket. The provider is looked up from
// the adapter→model→provider map; if the adapter or model is unknown the
// cost is 0 (treated as "unpriced", not "free"). The row's token counts
// (int64) are narrowed to int for the protocol.TokenUsagePayload; this is
// safe because a single bucket's token sum fits in int on any platform
// Go supports (int is ≥32-bit; even 2^31 tokens ≪ realistic run volumes).
func estimateRowCost(providerMap map[string]map[string]string, row store.UsageStatRow) float64 {
	inner, ok := providerMap[row.Adapter]
	if !ok {
		return 0
	}
	provider, ok := inner[row.Model]
	if !ok {
		return 0
	}
	return pricing.EstimateCost(provider, row.Model, protocol.TokenUsagePayload{
		InputTokens:      int(row.InputTokens),
		OutputTokens:     int(row.OutputTokens),
		CacheReadTokens:  int(row.CacheReadTokens),
		CacheWriteTokens: int(row.CacheWriteTokens),
	})
}
