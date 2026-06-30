package detect

import "context"

// CLIInfo describes a single supported coding CLI as observed on the host:
// whether it is installed, where the binary lives, the reported version,
// and the providers/models it can serve through aiclibridge.
//
// A CLIInfo with Available=false still carries Providers (the hardcoded
// default catalog) so clients can preview what the CLI would expose once
// installed — the bridge's HTTP catalog endpoint surfaces this directly
// rather than gating on installation.
type CLIInfo struct {
	// Name is the lowercase CLI identifier ("claude", "codex",
	// "opencode", "openclaw"). It is also the first segment of every
	// model name this CLI can serve (see ParseModelName / ModelName).
	Name string `json:"name"`
	// Version is the trimmed version line reported by `<cli> --version`,
	// empty when the binary is missing or the version probe failed.
	Version string `json:"version,omitempty"`
	// Available reports whether the binary is on PATH and the version
	// probe returned cleanly. A CLI that is installed but whose
	// --version probe timed out is reported Available=false with the
	// Path still populated, so callers can distinguish "missing" from
	// "installed-but-broken".
	Available bool `json:"available"`
	// Path is the resolved executable path (exec.LookPath result).
	// Empty when the binary was not found on PATH.
	Path string `json:"path,omitempty"`
	// Providers lists the provider/model groupings this CLI serves.
	// For v1 this is always the hardcoded default catalog
	// (see hardcodedCatalog); a future dynamic Discoverer can populate
	// it from `<cli> models list`-style output.
	Providers []ProviderInfo `json:"providers,omitempty"`
}

// ProviderInfo groups models by their upstream provider, matching the
// `CLI/provider/model` naming convention. For example the codex CLI
// exposes models under both the openai provider (gpt-5) and the
// anthropic provider (claude-sonnet-4.5).
type ProviderInfo struct {
	// Name is the lowercase provider identifier ("anthropic", "openai",
	// "google", "bytedance"). It is the middle segment of every model
	// name under this provider (see ModelName).
	Name string `json:"name"`
	// Models lists the models this CLI exposes for the provider.
	Models []ModelInfo `json:"models,omitempty"`
}

// ModelInfo describes a single model entry within a provider. The Name
// field is the routing key used in `CLI/provider/model`; DisplayName is
// an optional human-readable label and may be empty.
type ModelInfo struct {
	// Name is the bare model identifier (the third segment of the
	// `CLI/provider/model` form), e.g. "claude-sonnet-4.5" or "gpt-5".
	// This is what gets passed to the CLI at run time.
	Name string `json:"name"`
	// DisplayName is an optional, possibly empty human-readable label.
	// Clients SHOULD fall back to Name when DisplayName is empty.
	DisplayName string `json:"display_name,omitempty"`
}

// Discoverer is the per-CLI model-discovery seam. v1 ships a single
// implementation (hardcodedCatalog) that returns the default provider/model
// tables; future versions may inject an adapter-backed implementation
// that calls `<cli> models list` (or per-CLI equivalent) to enumerate
// installed models at runtime.
//
// A Discoverer MUST be safe for concurrent use across goroutines: the
// top-level Discover function calls each CLI's Discoverer in parallel.
// Implementations that surface transient errors SHOULD return
// (nil, err) — Discover turns that into Available=false for the affected
// CLI without failing the whole batch.
type Discoverer interface {
	// DiscoverModels returns the provider/model catalog this CLI can
	// serve. Returning an error marks the CLI Available=false but does
	// not abort the surrounding Discover call.
	DiscoverModels(ctx context.Context) ([]ProviderInfo, error)
}
