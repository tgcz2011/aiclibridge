package detect

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/adapter"
)

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
	// codebuddy v2.114.1 exposes 15 models via `--model` (verified against
	// `codebuddy --help`). All are served through Tencent's CodeBuddy
	// product, so they share a single "tencent" provider. The previous
	// placeholders (codebuddy-x1 / hunyuan-code) are not accepted by
	// `--model` and have been removed; pricing.go still carries their
	// (zero) entries harmlessly.
	"codebuddy": {
		{
			Name: "tencent",
			Models: []ModelInfo{
				{Name: "glm-5.2", DisplayName: "GLM 5.2"},
				{Name: "glm-5.1", DisplayName: "GLM 5.1"},
				{Name: "glm-5.0", DisplayName: "GLM 5.0"},
				{Name: "glm-5.0-turbo", DisplayName: "GLM 5.0 Turbo"},
				{Name: "glm-5v-turbo", DisplayName: "GLM 5V Turbo"},
				{Name: "glm-4.7", DisplayName: "GLM 4.7"},
				{Name: "minimax-m3", DisplayName: "MiniMax M3"},
				{Name: "minimax-m2.7", DisplayName: "MiniMax M2.7"},
				{Name: "kimi-k2.7", DisplayName: "Kimi K2.7"},
				{Name: "kimi-k2.6", DisplayName: "Kimi K2.6"},
				{Name: "kimi-k2.5", DisplayName: "Kimi K2.5"},
				{Name: "hy3-preview", DisplayName: "HY3 Preview"},
				{Name: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro"},
				{Name: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash"},
				{Name: "deepseek-v3-2-volc", DisplayName: "DeepSeek V3.2 Volc"},
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

// ── dynamic discoverer ──

// dynamicDiscoverer probes the CLI at runtime to enumerate available
// models. It wraps the hardcoded discoverer as a fallback: if the
// dynamic probe errors or returns zero models, the hardcoded catalog
// is returned so the HTTP endpoint always advertises something.
//
// Supported dynamic paths:
//   - opencode: `opencode models --verbose` (adapter.DiscoverOpenCodeModels)
//   - openclaw: `openclaw agents list --json` (adapter.DiscoverOpenclawAgents)
//   - codebuddy: `codebuddy --help` regex on --model description
//
// All other CLIs (claude, codex, qwen, gemini, copilot, stubs) have no
// CLI-exposed model-listing command and fall straight through to the
// hardcoded catalog.
type dynamicDiscoverer struct {
	cli  string
	path string // resolved binary path (empty if not on PATH)
	hard Discoverer
}

func newDynamicDiscoverer(cli, path string) Discoverer {
	return &dynamicDiscoverer{cli: cli, path: path, hard: newHardcodedDiscoverer(cli)}
}

// DiscoverModels runs the CLI-specific model probe, falling back to the
// hardcoded catalog on any error or empty result.
func (d *dynamicDiscoverer) DiscoverModels(ctx context.Context) ([]ProviderInfo, error) {
	models, err := d.probeModels(ctx)
	if err != nil || len(models) == 0 {
		// Dynamic probe failed or found nothing — fall back to the
		// hardcoded catalog so the endpoint still advertises models.
		return d.hard.DiscoverModels(ctx)
	}
	return groupAdapterModels(models), nil
}

// probeModels dispatches to the per-CLI discovery function. Returns
// (nil, error) for CLIs without a dynamic path — the caller falls back
// to the hardcoded catalog.
func (d *dynamicDiscoverer) probeModels(ctx context.Context) ([]adapter.Model, error) {
	switch d.cli {
	case "opencode":
		return adapter.DiscoverOpenCodeModels(ctx, d.path)
	case "openclaw":
		return adapter.DiscoverOpenclawAgents(ctx, d.path)
	case "codebuddy":
		return discoverCodebuddyModels(ctx, d.path)
	default:
		// No dynamic discovery path — caller falls back to hardcoded.
		return nil, fmt.Errorf("no dynamic discovery for %s", d.cli)
	}
}

// groupAdapterModels converts the adapter package's flat []Model list
// into the detect package's []ProviderInfo grouping (models grouped by
// their Provider field). Models with an empty Provider go under
// "unknown". Duplicate model IDs within a provider are deduplicated
// (first wins) so a chatty CLI output doesn't inflate the catalog.
func groupAdapterModels(models []adapter.Model) []ProviderInfo {
	byProvider := make(map[string][]ModelInfo)
	var order []string // preserve first-seen provider order
	for _, m := range models {
		p := m.Provider
		if p == "" {
			p = "unknown"
		}
		// Strip the "provider/" prefix from the model ID if present.
		// opencode's adapter returns IDs like "opencode/big-pickle" with
		// Provider="opencode"; without stripping, the routing key would
		// be "opencode/opencode/opencode/big-pickle" (triple-stacked).
		name := m.ID
		if p != "" && strings.HasPrefix(name, p+"/") {
			name = strings.TrimPrefix(name, p+"/")
		}
		if _, ok := byProvider[p]; !ok {
			order = append(order, p)
		}
		// Skip duplicate model IDs within the same provider.
		dup := false
		for _, ex := range byProvider[p] {
			if ex.Name == name {
				dup = true
				break
			}
		}
		if !dup {
			byProvider[p] = append(byProvider[p], ModelInfo{
				Name:        name,
				DisplayName: m.Label,
			})
		}
	}
	out := make([]ProviderInfo, 0, len(order))
	for _, p := range order {
		out = append(out, ProviderInfo{Name: p, Models: byProvider[p]})
	}
	return out
}

// discoverCodebuddyModels runs `codebuddy --help` and regex-parses the
// model list from the --model flag description, which looks like:
//
//	--model <model>  Model for the current session. Currently supported:
//	                (glm-5.2, glm-5.1, glm-5.0, ...)
//
// The 15 model IDs are comma-separated inside the parentheses. This is
// the only discovery path codebuddy exposes — it has no `models list`
// subcommand.
func discoverCodebuddyModels(ctx context.Context, execPath string) ([]adapter.Model, error) {
	if execPath == "" {
		execPath = "codebuddy"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, execPath, "--help")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseCodebuddyHelpModels(string(out)), nil
}

// parseCodebuddyHelpModels extracts model IDs from the --model flag
// description in codebuddy --help output. The list is inside
// parentheses after "Currently supported:". Returns an empty slice if
// the pattern is not found (e.g. codebuddy changed its help format).
func parseCodebuddyHelpModels(help string) []adapter.Model {
	// Find the parenthesized list after "Currently supported:".
	idx := strings.Index(help, "Currently supported:")
	if idx < 0 {
		return nil
	}
	rest := help[idx:]
	open := strings.Index(rest, "(")
	close := strings.Index(rest, ")")
	if open < 0 || close < 0 || close <= open {
		return nil
	}
	list := rest[open+1 : close]
	parts := strings.Split(list, ",")
	models := make([]adapter.Model, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if id == "" {
			continue
		}
		models = append(models, adapter.Model{ID: id, Provider: "tencent"})
	}
	return models
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
