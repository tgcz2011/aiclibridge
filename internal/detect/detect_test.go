package detect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ── ParseModelName ──

func TestParseModelName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantCLI     string
		wantProv    string
		wantModel   string
		wantErr     bool
		errContains  string // substring of the error message, when wantErr
	}{
		{
			name:      "canonical claude",
			input:     "claude/anthropic/claude-sonnet-4.5",
			wantCLI:   "claude",
			wantProv:  "anthropic",
			wantModel: "claude-sonnet-4.5",
		},
		{
			name:      "canonical codex",
			input:     "codex/openai/gpt-5",
			wantCLI:   "codex",
			wantProv:  "openai",
			wantModel: "gpt-5",
		},
		{
			name:      "canonical opencode google",
			input:     "opencode/google/gemini-2.5-pro",
			wantCLI:   "opencode",
			wantProv:  "google",
			wantModel: "gemini-2.5-pro",
		},
		{
			name:      "canonical openclaw bytedance",
			input:     "openclaw/bytedance/doubao-seedream-4-0",
			wantCLI:   "openclaw",
			wantProv:  "bytedance",
			wantModel: "doubao-seedream-4-0",
		},
		{
			name:      "CLI case-insensitive normalized to lowercase",
			input:     "Claude/anthropic/claude-sonnet-4.5",
			wantCLI:   "claude",
			wantProv:  "anthropic",
			wantModel: "claude-sonnet-4.5",
		},
		{
			name:      "CLI all-caps normalized to lowercase",
			input:     "CLAUDE/anthropic/claude-sonnet-4.5",
			wantCLI:   "claude",
			wantProv:  "anthropic",
			wantModel: "claude-sonnet-4.5",
		},
		{
			name:      "provider case preserved",
			input:     "codex/OpenAI/GPT-5",
			wantCLI:   "codex",
			wantProv:  "OpenAI",
			wantModel: "GPT-5",
		},
		{
			name:      "model with dashes preserved",
			input:     "openclaw/bytedance/doubao-seedance-1-0",
			wantCLI:   "openclaw",
			wantProv:  "bytedance",
			wantModel: "doubao-seedance-1-0",
		},
		{
			name:       "empty string",
			input:      "",
			wantErr:    true,
			errContains: "must be in CLI/provider/model form",
		},
		{
			name:       "one segment",
			input:      "claude",
			wantErr:    true,
			errContains: "must be in CLI/provider/model form",
		},
		{
			name:       "two segments",
			input:      "claude/anthropic",
			wantErr:    true,
			errContains: "must be in CLI/provider/model form",
		},
		{
			name:       "four segments",
			input:      "claude/anthropic/claude-sonnet-4.5/extra",
			wantErr:    true,
			errContains: "must be in CLI/provider/model form",
		},
		{
			name:       "empty CLI segment",
			input:      "/anthropic/claude-sonnet-4.5",
			wantErr:    true,
			errContains: "empty segment",
		},
		{
			name:       "empty provider segment (doubled slash)",
			input:      "claude//claude-sonnet-4.5",
			wantErr:    true,
			errContains: "empty segment",
		},
		{
			name:       "empty model segment (trailing slash)",
			input:      "claude/anthropic/",
			wantErr:    true,
			errContains: "empty segment",
		},
		{
			name:       "leading slash splits to four",
			input:      "/claude/anthropic/claude-sonnet-4.5",
			wantErr:    true,
			errContains: "must be in CLI/provider/model form",
		},
		{
			name:       "trailing slash splits to four",
			input:      "claude/anthropic/claude-sonnet-4.5/",
			wantErr:    true,
			errContains: "must be in CLI/provider/model form",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli, prov, model, err := ParseModelName(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseModelName(%q): expected error, got nil (cli=%q prov=%q model=%q)", tt.input, cli, prov, model)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("ParseModelName(%q): error %q does not contain %q", tt.input, err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseModelName(%q): unexpected error: %v", tt.input, err)
			}
			if cli != tt.wantCLI {
				t.Errorf("ParseModelName(%q): cli = %q, want %q", tt.input, cli, tt.wantCLI)
			}
			if prov != tt.wantProv {
				t.Errorf("ParseModelName(%q): provider = %q, want %q", tt.input, prov, tt.wantProv)
			}
			if model != tt.wantModel {
				t.Errorf("ParseModelName(%q): model = %q, want %q", tt.input, model, tt.wantModel)
			}
		})
	}
}

// ── ModelName round-trip ──

func TestModelNameRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cli, prov, model string
	}{
		{"claude", "anthropic", "claude-sonnet-4.5"},
		{"codex", "openai", "gpt-5"},
		{"opencode", "google", "gemini-2.5-pro"},
		{"openclaw", "bytedance", "doubao-seedream-4-0"},
		// Mixed-case CLI is normalized on the way out, so the round trip
		// is NOT identity — ModelName lowercases the CLI segment. The
		// post-normalization tuple must round-trip cleanly.
		{"Claude", "anthropic", "claude-sonnet-4.5"},
		{"CLAUDE", "OpenAI", "GPT-5"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(ModelName(tc.cli, tc.prov, tc.model), func(t *testing.T) {
			t.Parallel()
			assembled := ModelName(tc.cli, tc.prov, tc.model)
			cli, prov, model, err := ParseModelName(assembled)
			if err != nil {
				t.Fatalf("ParseModelName(%q): %v", assembled, err)
			}
			// ModelName lowercases the CLI; compare against the
			// normalized form, not the original input.
			wantCLI := strings.ToLower(strings.TrimSpace(tc.cli))
			if cli != wantCLI {
				t.Errorf("cli round-trip: got %q, want %q (input %q)", cli, wantCLI, tc.cli)
			}
			if prov != tc.prov {
				t.Errorf("provider round-trip: got %q, want %q", prov, tc.prov)
			}
			if model != tc.model {
				t.Errorf("model round-trip: got %q, want %q", model, tc.model)
			}
		})
	}
}

// ModelName must lowercase the CLI segment and leave provider and model
// untouched — verify the exact assembled form for a mixed-case input so
// a regression here shows up as a string diff rather than a round-trip
// mystery.
func TestModelNameFormatting(t *testing.T) {
	t.Parallel()

	got := ModelName("Claude", "Anthropic", "claude-sonnet-4.5")
	want := "claude/Anthropic/claude-sonnet-4.5"
	if got != want {
		t.Errorf("ModelName lower-cased CLI / preserved provider: got %q, want %q", got, want)
	}
}

// ── DefaultCatalog ──

func TestDefaultCatalog(t *testing.T) {
	t.Parallel()

	catalog := DefaultCatalog()
	if len(catalog) != 4 {
		t.Fatalf("DefaultCatalog: got %d CLIs, want 4", len(catalog))
	}

	wantNames := []string{"claude", "codex", "opencode", "openclaw"}
	for i, want := range wantNames {
		if catalog[i].Name != want {
			t.Errorf("DefaultCatalog[%d].Name = %q, want %q", i, catalog[i].Name, want)
		}
		// Default catalog is host-independent: nothing is marked available.
		if catalog[i].Available {
			t.Errorf("DefaultCatalog[%d] (%s): Available = true, want false", i, catalog[i].Name)
		}
		if catalog[i].Version != "" {
			t.Errorf("DefaultCatalog[%d] (%s): Version = %q, want empty", i, catalog[i].Name, catalog[i].Version)
		}
		if catalog[i].Path != "" {
			t.Errorf("DefaultCatalog[%d] (%s): Path = %q, want empty", i, catalog[i].Name, catalog[i].Path)
		}
		if len(catalog[i].Providers) == 0 {
			t.Errorf("DefaultCatalog[%d] (%s): Providers empty, want hardcoded catalog", i, catalog[i].Name)
		}
	}

	// Spot-check a couple of provider/model entries to make sure the
	// hardcoded catalog survives the clone + DefaultCatalog round trip.
	claude := catalog[0]
	if claude.Providers[0].Name != "anthropic" {
		t.Fatalf("claude provider[0]: got %q, want anthropic", claude.Providers[0].Name)
	}
	wantClaudeModels := []string{"claude-sonnet-4.5", "claude-opus-4.1", "claude-haiku-4.5"}
	if len(claude.Providers[0].Models) != len(wantClaudeModels) {
		t.Fatalf("claude anthropic models: got %d, want %d", len(claude.Providers[0].Models), len(wantClaudeModels))
	}
	for i, want := range wantClaudeModels {
		if claude.Providers[0].Models[i].Name != want {
			t.Errorf("claude anthropic model[%d]: got %q, want %q", i, claude.Providers[0].Models[i].Name, want)
		}
	}

	// codex has two providers: openai and anthropic.
	codex := catalog[1]
	if len(codex.Providers) != 2 {
		t.Fatalf("codex providers: got %d, want 2", len(codex.Providers))
	}
	if codex.Providers[0].Name != "openai" || codex.Providers[1].Name != "anthropic" {
		t.Errorf("codex providers: got %q/%q, want openai/anthropic", codex.Providers[0].Name, codex.Providers[1].Name)
	}

	// openclaw has the bytedance provider with the two doubao models.
	openclaw := catalog[3]
	if len(openclaw.Providers) != 1 || openclaw.Providers[0].Name != "bytedance" {
		t.Fatalf("openclaw providers: got %+v, want [bytedance]", openclaw.Providers)
	}
	if len(openclaw.Providers[0].Models) != 2 {
		t.Fatalf("openclaw bytedance models: got %d, want 2", len(openclaw.Providers[0].Models))
	}
}

// DefaultCatalog must not share slices with the package catalog —
// mutating a returned entry must not leak into the next caller. This
// guards the cloneProviders defensive copy.
func TestDefaultCatalogDefensiveCopy(t *testing.T) {
	t.Parallel()

	first := DefaultCatalog()
	if len(first) == 0 || len(first[0].Providers) == 0 || len(first[0].Providers[0].Models) == 0 {
		t.Fatalf("DefaultCatalog returned empty entries: %+v", first)
	}
	// Mutate the returned slice in place.
	first[0].Providers[0].Models[0].Name = "MUTATED"
	first[0].Providers[0].Name = "MUTATED_PROVIDER"

	second := DefaultCatalog()
	if second[0].Providers[0].Models[0].Name == "MUTATED" {
		t.Errorf("DefaultCatalog leaked model mutation between calls")
	}
	if second[0].Providers[0].Name == "MUTATED_PROVIDER" {
		t.Errorf("DefaultCatalog leaked provider mutation between calls")
	}
}

// ── Discover fault isolation ──

// TestDiscoverFaultIsolation feeds an unsupported CLI name through the
// internal discoverWithCLIs entry point and verifies that the missing
// binary is recorded as Available=false without surfacing an error. The
// v1 Discover function iterates supportedCLIs, so this is the closest
// in-package way to exercise the fault-isolation invariant against a
// guaranteed-absent binary without depending on the host's installation
// state.
func TestDiscoverFaultIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	results, err := discoverWithCLIs(ctx, []string{"nonexistent-cli-xyz-do-not-install"})
	if err != nil {
		t.Fatalf("discoverWithCLIs: expected no error for missing CLI, got %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("discoverWithCLIs: got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Name != "nonexistent-cli-xyz-do-not-install" {
		t.Errorf("result.Name = %q, want nonexistent-cli-xyz-do-not-install", r.Name)
	}
	if r.Available {
		t.Errorf("result.Available = true, want false (binary should not be on PATH)")
	}
	if r.Path != "" {
		t.Errorf("result.Path = %q, want empty (LookPath should have failed)", r.Path)
	}
	if r.Version != "" {
		t.Errorf("result.Version = %q, want empty", r.Version)
	}
}

// TestDiscoverNeverErrorsForAnyMissingCLIs mixes supported CLI names
// with a guaranteed-absent binary and verifies Discover's fault-isolation
// contract: every supported CLI in the list is probed in parallel and
// each missing one is reported Available=false, but the batch as a whole
// returns no error. This is the test the HTTP catalog endpoint relies on.
func TestDiscoverNeverErrorsForAnyMissingCLIs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mixed := append([]string{}, supportedCLIs...)
	mixed = append(mixed, "definitely-not-installed-cli-12345")

	results, err := discoverWithCLIs(ctx, mixed)
	if err != nil {
		t.Fatalf("discoverWithCLIs: expected no error even with missing CLIs, got %v", err)
	}
	if len(results) != len(mixed) {
		t.Fatalf("discoverWithCLIs: got %d results, want %d", len(results), len(mixed))
	}
	// Order is preserved — supportedCLIs first, then the injected name.
	for i, want := range mixed {
		if results[i].Name != want {
			t.Errorf("results[%d].Name = %q, want %q", i, results[i].Name, want)
		}
		// Providers must always be populated from the catalog, even
		// when the binary is missing — that's the preview contract.
		if want != "definitely-not-installed-cli-12345" && len(results[i].Providers) == 0 {
			t.Errorf("results[%d] (%s): Providers empty, want hardcoded catalog", i, want)
		}
	}
	// The injected unknown name has no catalog entry — Providers is
	// empty but the entry exists and is not Available.
	unknown := results[len(results)-1]
	if unknown.Available {
		t.Errorf("unknown CLI should be Available=false, got true")
	}
}

// TestDiscoverSmoke exercises the public Discover entry point on the
// real host. It must not error and must return exactly len(supportedCLIs)
// entries in the supportedCLIs order; whether each is Available depends
// on what's installed, so we only assert structure, not Available.
func TestDiscoverSmoke(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	results, err := Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: unexpected error: %v", err)
	}
	if len(results) != len(supportedCLIs) {
		t.Fatalf("Discover: got %d results, want %d", len(results), len(supportedCLIs))
	}
	for i, want := range supportedCLIs {
		if results[i].Name != want {
			t.Errorf("Discover[%d].Name = %q, want %q", i, results[i].Name, want)
		}
		// The hardcoded catalog is always populated, regardless of
		// whether the binary is installed.
		if len(results[i].Providers) == 0 {
			t.Errorf("Discover[%d] (%s): Providers empty, want hardcoded catalog", i, results[i].Name)
		}
	}
}

// ── DiscoverCLI ──

// TestDiscoverCLIUnknownNameErrors verifies that the targeted
// DiscoverCLI entry point rejects unsupported CLI names with an error
// rather than silently returning Available=false. This is the per-CLI
// counterpart to Discover's fault-isolation behavior: Discover swallows
// per-CLI failures into Available=false (catalog-wide fallback);
// DiscoverCLI surfaces them (single-CLI lookup).
func TestDiscoverCLIUnknownNameErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	info, err := DiscoverCLI(ctx, "nonexistent-cli")
	if err == nil {
		t.Fatalf("DiscoverCLI(nonexistent-cli): expected error, got nil (info=%+v)", info)
	}
	if info != nil {
		t.Errorf("DiscoverCLI(nonexistent-cli): expected nil info on error, got %+v", info)
	}
	if !strings.Contains(err.Error(), "unsupported CLI") {
		t.Errorf("DiscoverCLI error %q does not mention unsupported CLI", err.Error())
	}
}

// TestDiscoverCLICaseInsensitive verifies that callers can pass "Claude"
// or "CLAUDE" and still hit the supported-CLI path. The result's Name
// field must be the normalized lowercase form so downstream lookups
// (which key on Name) work regardless of caller casing.
func TestDiscoverCLICaseInsensitive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, name := range []string{"claude", "Claude", "CLAUDE", "ClAuDe"} {
		info, err := DiscoverCLI(ctx, name)
		if err != nil {
			t.Errorf("DiscoverCLI(%q): unexpected error: %v", name, err)
			continue
		}
		if info == nil {
			t.Errorf("DiscoverCLI(%q): nil info", name)
			continue
		}
		if info.Name != "claude" {
			t.Errorf("DiscoverCLI(%q).Name = %q, want %q", name, info.Name, "claude")
		}
	}
}

// TestDiscoverCLITrimsWhitespace covers a common caller mistake: a
// leading/trailing space on the CLI name (e.g. from a config field
// parsed without trimming). normalizeCLIName handles this so DiscoverCLI
// doesn't reject " claude " as unsupported.
func TestDiscoverCLITrimsWhitespace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	info, err := DiscoverCLI(ctx, "  claude  ")
	if err != nil {
		t.Fatalf("DiscoverCLI(%q): unexpected error: %v", "  claude  ", err)
	}
	if info.Name != "claude" {
		t.Errorf("DiscoverCLI(%q).Name = %q, want %q", "  claude  ", info.Name, "claude")
	}
}

// ── Discoverer interface ──

// fakeDiscoverer is a test double for the Discoverer seam. It lets a
// test inject a custom provider/model catalog without touching the
// hardcoded one, exercising the path a future adapter-backed
// implementation will take.
type fakeDiscoverer struct {
	providers []ProviderInfo
	err       error
}

func (f fakeDiscoverer) DiscoverModels(_ context.Context) ([]ProviderInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return cloneProviders(f.providers), nil
}

// TestDiscoverOneWithFakeDiscoverer verifies that discoverOne consults
// the injected Discoverer for the provider catalog, even when the binary
// is missing. This is the contract a future adapter-backed implementation
// depends on: it gets to drive the catalog independent of LookPath.
func TestDiscoverOneWithFakeDiscoverer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fake := fakeDiscoverer{
		providers: []ProviderInfo{
			{
				Name: "custom-provider",
				Models: []ModelInfo{
					{Name: "custom-model-1", DisplayName: "Custom 1"},
				},
			},
		},
	}
	info, err := discoverOne(ctx, "nonexistent-cli-xyz", fake)
	if err != nil {
		t.Fatalf("discoverOne: unexpected error: %v", err)
	}
	if info.Available {
		t.Errorf("discoverOne: Available = true for missing binary, want false")
	}
	if len(info.Providers) != 1 || info.Providers[0].Name != "custom-provider" {
		t.Errorf("discoverOne: Providers = %+v, want [custom-provider]", info.Providers)
	}
	if len(info.Providers[0].Models) != 1 || info.Providers[0].Models[0].Name != "custom-model-1" {
		t.Errorf("discoverOne: model = %+v, want custom-model-1", info.Providers[0].Models)
	}
}

// TestDiscoverOneDiscovererError verifies that a Discoverer returning an
// error is absorbed: the CLI is marked Available=false (or true if the
// binary is present and the version probe succeeded), the catalog is
// empty, and discoverOne itself does not return an error. This keeps the
// Discover fault-isolation invariant bulletproof against a future
// adapter-backed Discoverer that surfaces transient failures.
func TestDiscoverOneDiscovererError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fake := fakeDiscoverer{err: errors.New("discoverer unavailable")}
	info, err := discoverOne(ctx, "nonexistent-cli-xyz", fake)
	if err != nil {
		t.Fatalf("discoverOne: expected no error when Discoverer fails, got %v", err)
	}
	if info == nil {
		t.Fatal("discoverOne: nil info")
	}
	if info.Available {
		t.Errorf("discoverOne: Available = true, want false")
	}
	if len(info.Providers) != 0 {
		t.Errorf("discoverOne: Providers = %+v, want empty when Discoverer errors", info.Providers)
	}
}
