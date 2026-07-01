package pricing

import (
	"math"
	"testing"

	"github.com/tgcz2011/aiclibridge/internal/detect"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// TestPriceTableCoversCatalog asserts every (provider, model) the detect
// catalog advertises has an entry in the price table — even if the price
// is zero (unknown / per-call). A model surfaced to clients but missing
// from the table would silently report $0 with no "unknown" signal, which
// is misleading; this test guards against catalog growth outpacing the
// price table.
func TestPriceTableCoversCatalog(t *testing.T) {
	for _, cli := range detect.DefaultCatalog() {
		for _, p := range cli.Providers {
			for _, m := range p.Models {
				if _, ok := Lookup(p.Name, m.Name); !ok {
					t.Errorf("price table missing %s/%s (cli=%s)", p.Name, m.Name, cli.Name)
				}
			}
		}
	}
}

// TestEstimateCost checks the cost arithmetic for a known-priced model.
// claude-sonnet-4.5: $3/M input, $15/M output. 1000 input + 500 output
// → 0.003 + 0.0075 = $0.0105. A small epsilon guards float rounding.
func TestEstimateCost(t *testing.T) {
	usage := protocol.TokenUsagePayload{
		InputTokens:  1000,
		OutputTokens: 500,
	}
	got := EstimateCost("anthropic", "claude-sonnet-4.5", usage)
	want := 0.0105
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost: got %.6f, want %.6f", got, want)
	}
}

// TestEstimateCostCache verifies cache components are priced too, so a
// future table edit that drops a cache rate is caught. claude-sonnet-4.5
// cache read $0.3/M, cache write $3.75/M.
func TestEstimateCostCache(t *testing.T) {
	usage := protocol.TokenUsagePayload{
		InputTokens:      1_000_000,
		CacheReadTokens:  1_000_000,
		CacheWriteTokens: 1_000_000,
	}
	got := EstimateCost("anthropic", "claude-sonnet-4.5", usage)
	// 1M input @ $3 + 1M cache-read @ $0.3 + 1M cache-write @ $3.75 = $7.05
	want := 7.05
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost with cache: got %.6f, want %.6f", got, want)
	}
}

// TestLookupUnknown: a provider/model pair absent from the table returns
// ok=false, and EstimateCost for it is 0.
func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("nobody", "ghost-model"); ok {
		t.Error("Lookup should return false for an unknown provider/model")
	}
	got := EstimateCost("nobody", "ghost-model", protocol.TokenUsagePayload{InputTokens: 1000})
	if got != 0 {
		t.Errorf("EstimateCost for unknown model: got %v, want 0", got)
	}
}

// TestAllPricesNonEmpty ensures the prices endpoint will render rows.
func TestAllPricesNonEmpty(t *testing.T) {
	entries := AllPrices()
	if len(entries) == 0 {
		t.Fatal("AllPrices returned no entries")
	}
	// Spot-check: claude-sonnet-4.5 under anthropic is present and priced.
	var sonnet PriceEntry
	found := false
	for _, e := range entries {
		if e.Provider == "anthropic" && e.Model == "claude-sonnet-4.5" {
			sonnet = e
			found = true
			break
		}
	}
	if !found {
		t.Fatal("anthropic/claude-sonnet-4.5 missing from AllPrices")
	}
	if sonnet.InputPerMillion != 3 || sonnet.OutputPerMillion != 15 {
		t.Errorf("sonnet price: got in=%v out=%v, want 3/15",
			sonnet.InputPerMillion, sonnet.OutputPerMillion)
	}
}
