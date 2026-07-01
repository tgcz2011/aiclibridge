package api

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/tgcz2011/aiclibridge/internal/store"
)

// TestStatsUsage verifies /v1/stats/usage returns the aggregated rows with
// an estimated_cost_usd per row. The fake facade supplies pre-aggregated
// rows (store aggregation is covered by store_test.go); here we check the
// handler prices them and shapes the JSON. claude/claude-sonnet-4.5 with
// 1000 input + 500 output → $0.003 + $0.0075 = $0.0105.
func TestStatsUsage(t *testing.T) {
	fc := newFakeFacade()
	fc.statsRows = []store.UsageStatRow{
		{Adapter: "claude", Model: "claude-sonnet-4.5", Status: "completed", RunCount: 2,
			InputTokens: 1000, OutputTokens: 500},
		{Adapter: "codex", Model: "gpt-5", Status: "completed", RunCount: 1,
			InputTokens: 2000, OutputTokens: 1000},
	}
	s := newTestServer(fc, nil)

	rec := doRequest(t, s, "GET", "/v1/stats/usage", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Rows []statsUsageRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(body.Rows))
	}

	// Find the claude row and check its cost.
	var claudeRow *statsUsageRow
	for i := range body.Rows {
		if body.Rows[i].Adapter == "claude" {
			claudeRow = &body.Rows[i]
			break
		}
	}
	if claudeRow == nil {
		t.Fatalf("claude row missing: %+v", body.Rows)
	}
	want := 0.0105
	if math.Abs(claudeRow.EstimatedCostUSD-want) > 1e-9 {
		t.Errorf("claude cost: got %.6f, want %.6f", claudeRow.EstimatedCostUSD, want)
	}
	if claudeRow.RunCount != 2 {
		t.Errorf("claude run_count: got %d, want 2", claudeRow.RunCount)
	}
}

// TestStatsUsageBadParam: a non-integer since yields 400, not a 500.
func TestStatsUsageBadParam(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/stats/usage?since=notanint", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestStatsPrices verifies /v1/stats/prices returns the full price table
// non-empty, with anthropic/claude-sonnet-4.5 priced at $3/$15.
func TestStatsPrices(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/stats/prices", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var env statsPricesEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Prices) == 0 {
		t.Fatal("prices is empty")
	}
	var sonnet struct {
		Provider         string  `json:"provider"`
		Model            string  `json:"model"`
		InputPerMillion  float64 `json:"input_per_million"`
		OutputPerMillion float64 `json:"output_per_million"`
	}
	for _, p := range env.Prices {
		if p.Provider == "anthropic" && p.Model == "claude-sonnet-4.5" {
			sonnet.Provider = p.Provider
			sonnet.Model = p.Model
			sonnet.InputPerMillion = p.InputPerMillion
			sonnet.OutputPerMillion = p.OutputPerMillion
			break
		}
	}
	if sonnet.Model == "" {
		t.Fatalf("anthropic/claude-sonnet-4.5 missing from prices: %+v", env.Prices)
	}
	if sonnet.InputPerMillion != 3 || sonnet.OutputPerMillion != 15 {
		t.Errorf("sonnet price: got in=%v out=%v, want 3/15",
			sonnet.InputPerMillion, sonnet.OutputPerMillion)
	}
}

// TestStatsSummary verifies /v1/stats/summary totals tokens and cost
// across rows and breaks cost down by adapter. Two adapters (claude,
// codex) each contribute one row; the response should have TotalRuns=3
// and a ByAdapter slice with two entries.
func TestStatsSummary(t *testing.T) {
	fc := newFakeFacade()
	fc.statsRows = []store.UsageStatRow{
		{Adapter: "claude", Model: "claude-sonnet-4.5", Status: "completed", RunCount: 2,
			InputTokens: 1000, OutputTokens: 500},
		{Adapter: "codex", Model: "gpt-5", Status: "completed", RunCount: 1,
			InputTokens: 2000, OutputTokens: 1000},
	}
	s := newTestServer(fc, nil)

	rec := doRequest(t, s, "GET", "/v1/stats/summary", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var sum statsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sum.TotalRuns != 3 {
		t.Errorf("TotalRuns: got %d, want 3", sum.TotalRuns)
	}
	if sum.TotalInputTokens != 3000 {
		t.Errorf("TotalInputTokens: got %d, want 3000", sum.TotalInputTokens)
	}
	if sum.TotalOutputTokens != 1500 {
		t.Errorf("TotalOutputTokens: got %d, want 1500", sum.TotalOutputTokens)
	}
	// claude cost: 1000*3/1M + 500*15/1M = 0.0105
	// codex cost:  2000*5/1M + 1000*15/1M = 0.025
	// total: 0.0355
	wantTotal := 0.0355
	if math.Abs(sum.TotalEstimatedCostUSD-wantTotal) > 1e-9 {
		t.Errorf("TotalEstimatedCostUSD: got %.6f, want %.6f", sum.TotalEstimatedCostUSD, wantTotal)
	}
	if len(sum.ByAdapter) != 2 {
		t.Fatalf("ByAdapter: got %d entries, want 2", len(sum.ByAdapter))
	}
	// ByAdapter is sorted by adapter name: claude first, then codex.
	if sum.ByAdapter[0].Adapter != "claude" {
		t.Errorf("ByAdapter[0]: got %q, want claude", sum.ByAdapter[0].Adapter)
	}
	if sum.ByAdapter[1].Adapter != "codex" {
		t.Errorf("ByAdapter[1]: got %q, want codex", sum.ByAdapter[1].Adapter)
	}
	// claude adapter cost = 0.0105.
	if math.Abs(sum.ByAdapter[0].EstimatedCostUSD-0.0105) > 1e-9 {
		t.Errorf("claude adapter cost: got %.6f, want 0.0105", sum.ByAdapter[0].EstimatedCostUSD)
	}
}

// TestStatsSummaryEmpty: no rows yields zero totals and an empty (not nil)
// ByAdapter slice so the JSON serialises as [].
func TestStatsSummaryEmpty(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/stats/summary", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var sum statsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sum.TotalRuns != 0 {
		t.Errorf("TotalRuns: got %d, want 0", sum.TotalRuns)
	}
	if sum.ByAdapter == nil {
		t.Error("ByAdapter should be non-nil empty slice, got nil")
	}
	if len(sum.ByAdapter) != 0 {
		t.Errorf("ByAdapter: got %d entries, want 0", len(sum.ByAdapter))
	}
}
