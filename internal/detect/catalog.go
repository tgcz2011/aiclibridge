package detect

import "context"

// supportedCLIs is the set of CLIs the bridge can serve. Order matters:
// DefaultCatalog and Discover both iterate this slice in this order so the
// HTTP catalog endpoint emits a stable, testable listing. Adding a CLI here
// is the single source of truth for "what does the bridge know about" —
// detect.go picks it up automatically.
//
// v0.2 extends the v0.1 six-CLI set with thirteen more surfaced from
// AionUi's ACP backend catalogue. The stub tier (droid/snow/vibe/aion)
// is listed so /v1/agents reports them as available:false rather than
// silently omitting them — clients can see what the bridge *would*
// route to once the upstream protocol is documented.
var supportedCLIs = []string{
	// v0.1
	"claude", "codex", "opencode", "openclaw", "qwen", "gemini",
	// v0.2 stream-json (Claude SDK schema)
	"codebuddy",
	// v0.2 ACP JSON-RPC family
	"copilot", "goose", "cursor", "kimi", "kiro", "qoder", "hermes", "auggie",
	// v0.2 stubs
	"droid", "snow", "vibe", "aion",
}

// hardcodedCatalog is the v1 Discoverer. It returns the static provider/model
// tables shipped in source, the same ones surfaced by DefaultCatalog for
// client preview. A future version can replace this with an adapter-backed
// Discoverer that queries `<cli> models list` at runtime.
//
// The catalog is keyed by CLI name (lowercase, matching CLIInfo.Name and the
// first segment of every `CLI/provider/model` identifier). A missing key
// means "this CLI serves no models" — DiscoverModels returns an empty slice
// rather than nil so the JSON serialization is `[]`, not `null`.
var hardcodedCatalog = map[string][]ProviderInfo{
	"claude": {
		{
			Name: "anthropic",
			Models: []ModelInfo{
				{Name: "claude-sonnet-4.5", DisplayName: "Claude Sonnet 4.5"},
				{Name: "claude-opus-4.1", DisplayName: "Claude Opus 4.1"},
				{Name: "claude-haiku-4.5", DisplayName: "Claude Haiku 4.5"},
			},
		},
	},
	"codex": {
		{
			Name: "openai",
			Models: []ModelInfo{
				{Name: "gpt-5", DisplayName: "GPT-5"},
				{Name: "gpt-5-mini", DisplayName: "GPT-5 Mini"},
				{Name: "o3", DisplayName: "o3"},
			},
		},
		{
			Name: "anthropic",
			Models: []ModelInfo{
				{Name: "claude-sonnet-4.5", DisplayName: "Claude Sonnet 4.5"},
			},
		},
	},
	"opencode": {
		{
			Name: "anthropic",
			Models: []ModelInfo{
				{Name: "claude-sonnet-4.5", DisplayName: "Claude Sonnet 4.5"},
			},
		},
		{
			Name: "openai",
			Models: []ModelInfo{
				{Name: "gpt-5", DisplayName: "GPT-5"},
			},
		},
		{
			Name: "google",
			Models: []ModelInfo{
				{Name: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro"},
			},
		},
	},
	"openclaw": {
		{
			Name: "bytedance",
			Models: []ModelInfo{
				{Name: "doubao-seedream-4-0", DisplayName: "Doubao Seedream 4.0"},
				{Name: "doubao-seedance-1-0", DisplayName: "Doubao Seedance 1.0"},
			},
		},
	},
	"qwen": {
		{
			Name: "alibaba",
			Models: []ModelInfo{
				{Name: "qwen3-coder-plus", DisplayName: "Qwen3 Coder Plus"},
				{Name: "qwen3-max", DisplayName: "Qwen3 Max"},
			},
		},
	},
	"gemini": {
		{
			Name: "google",
			Models: []ModelInfo{
				{Name: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro"},
				{Name: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash"},
			},
		},
	},
	// ── v0.2 additions ──
	"codebuddy": {
		{
			Name: "tencent",
			Models: []ModelInfo{
				{Name: "codebuddy-x1", DisplayName: "CodeBuddy X1"},
				{Name: "hunyuan-code", DisplayName: "Hunyuan Code"},
			},
		},
	},
	"copilot": {
		{
			Name: "github",
			Models: []ModelInfo{
				{Name: "gpt-5", DisplayName: "GPT-5"},
				{Name: "claude-sonnet-4.5", DisplayName: "Claude Sonnet 4.5"},
				{Name: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro"},
			},
		},
	},
	"goose": {
		{
			Name: "block",
			Models: []ModelInfo{
				{Name: "goose-1", DisplayName: "Goose 1"},
			},
		},
	},
	"cursor": {
		{
			Name: "cursor",
			Models: []ModelInfo{
				{Name: "cursor-default", DisplayName: "Cursor Default"},
			},
		},
	},
	"kimi": {
		{
			Name: "moonshot",
			Models: []ModelInfo{
				{Name: "kimi-k2", DisplayName: "Kimi K2"},
			},
		},
	},
	"kiro": {
		{
			Name: "aws",
			Models: []ModelInfo{
				{Name: "kiro-default", DisplayName: "Kiro Default"},
			},
		},
	},
	"qoder": {
		{
			Name: "qoder",
			Models: []ModelInfo{
				{Name: "qoder-default", DisplayName: "Qoder Default"},
			},
		},
	},
	"hermes": {
		{
			Name: "nous",
			Models: []ModelInfo{
				{Name: "hermes-4", DisplayName: "Hermes 4"},
			},
		},
	},
	"auggie": {
		{
			Name: "auggie",
			Models: []ModelInfo{
				{Name: "auggie-default", DisplayName: "Auggie Default"},
			},
		},
	},
	// Stubs — no known provider/model mapping; listed so /v1/agents
	// honestly reports them as known-but-unavailable.
	"droid":  {},
	"snow":   {},
	"vibe":   {},
	"aion":   {},
}

// hardcodedCatalog implements Discoverer. DiscoverModels returns a defensive
// copy of the catalog entry for the CLI so callers cannot mutate the package
// global through the returned slice. A CLI not in the catalog returns an
// empty (non-nil) slice — semantically "no models known" rather than "error".
func (h hardcodedDiscoverer) DiscoverModels(_ context.Context) ([]ProviderInfo, error) {
	providers, ok := hardcodedCatalog[h.cli]
	if !ok {
		return []ProviderInfo{}, nil
	}
	return cloneProviders(providers), nil
}

// hardcodedDiscoverer is the receiver for the v1 Discoverer implementation.
// Construction lives in newHardcodedDiscoverer so detect.go can swap in an
// adapter-backed implementation later without touching this file.
type hardcodedDiscoverer struct {
	cli string
}

func newHardcodedDiscoverer(cli string) Discoverer {
	return hardcodedDiscoverer{cli: cli}
}

// cloneProviders returns a deep copy of providers so callers cannot mutate
// the package-level catalog through the returned slice. Both the outer slice
// and each ProviderInfo.Models slice are reallocated; the string fields are
// immutable so they are shared safely.
func cloneProviders(providers []ProviderInfo) []ProviderInfo {
	if len(providers) == 0 {
		return []ProviderInfo{}
	}
	out := make([]ProviderInfo, len(providers))
	for i, p := range providers {
		out[i] = ProviderInfo{Name: p.Name}
		if len(p.Models) > 0 {
			models := make([]ModelInfo, len(p.Models))
			copy(models, p.Models)
			out[i].Models = models
		}
	}
	return out
}

// DefaultCatalog returns the hardcoded provider/model catalog for every
// supported CLI, regardless of whether the binary is installed. Each entry
// has Available=false and an empty Path/Version; Providers is populated
// from hardcodedCatalog so clients can preview what the bridge would serve
// once each CLI is installed. The returned slice is in supportedCLIs order
// so callers get a deterministic listing.
//
// This is the "what can the bridge know about" view; Discover is the "what
// is actually installed on this host" view. The HTTP catalog endpoint
// typically calls DefaultCatalog and then overlays per-CLI Available/Version
// from Discover.
func DefaultCatalog() []CLIInfo {
	out := make([]CLIInfo, 0, len(supportedCLIs))
	for _, name := range supportedCLIs {
		providers, _ := newHardcodedDiscoverer(name).DiscoverModels(context.Background())
		out = append(out, CLIInfo{
			Name:      name,
			Available: false,
			Providers: providers,
		})
	}
	return out
}
