package detect

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/tgcz2011/aiclibridge/internal/adapter"
)

// Discover probes every CLI in supportedCLIs in parallel and returns a
// CLIInfo slice in the same order. It is the host-level entry point: the
// returned list reflects what is actually installed on this machine.
//
// Fault isolation: a single CLI failing to be detected (binary missing,
// --version probe erroring or timing out, model discovery erroring) is
// recorded as Available=false on that CLI's entry but never propagates as
// an error from Discover. Only a wholesale failure of the discovery
// machinery (e.g. context cancellation) returns an error. This is the
// contract the HTTP catalog endpoint relies on: it always renders
// something, even on a host with zero CLIs installed.
//
// Parallelism: each CLI is probed in its own goroutine. The version probe
// (adapter.DetectCLIVersion) carries its own 10s timeout, so a wedged CLI
// fails fast in isolation rather than stalling the batch. Results are
// written into a pre-sized slice indexed by position in supportedCLIs so
// the returned order is deterministic and matches DefaultCatalog.
func Discover(ctx context.Context) ([]CLIInfo, error) {
	return discoverWithCLIs(ctx, supportedCLIs)
}

// discoverWithCLIs is the testable core of Discover. It runs the probe
// for every name in clis in parallel, in the given order, and returns the
// assembled CLIInfo slice. Used by Discover (supportedCLIs) and by tests
// that inject a fake CLI name to verify fault isolation.
func discoverWithCLIs(ctx context.Context, clis []string) ([]CLIInfo, error) {
	results := make([]CLIInfo, len(clis))
	var wg sync.WaitGroup
	for i, name := range clis {
		wg.Add(1)
		go func(idx int, cliName string) {
			defer wg.Done()
			info, err := discoverOne(ctx, cliName, newDynamicDiscoverer(cliName, ""))
			if err != nil || info == nil {
				// Defensive: discoverOne should never error in the
				// fault-isolating path, but if it does we surface the
				// CLI as unavailable with the default catalog so the
				// HTTP catalog still renders.
				info = &CLIInfo{
					Name:      cliName,
					Available: false,
					Providers: fallbackProviders(cliName),
				}
			}
			results[idx] = *info
		}(i, name)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("detect: discover cancelled: %w", err)
	}
	return results, nil
}

// DiscoverCLI probes a single CLI by name. Unlike Discover, a failure here
// returns an error — callers using this entry point want the per-CLI
// signal, not the catalog-wide fallback. The name is matched
// case-insensitively and normalized to lowercase in the returned
// CLIInfo.Name so callers can pass "Claude" or "CLAUDE" interchangeably.
//
// An unknown name (not in supportedCLIs) returns an error rather than an
// empty CLIInfo: this entry point is for targeted lookups, and silently
// returning Available=false for a typo would mask the mistake.
func DiscoverCLI(ctx context.Context, name string) (*CLIInfo, error) {
	normalized := normalizeCLIName(name)
	if !isSupportedCLI(normalized) {
		return nil, fmt.Errorf("detect: unsupported CLI %q (supported: %s)", name, strings.Join(supportedCLIs, ", "))
	}
	info, err := discoverOne(ctx, normalized, newDynamicDiscoverer(normalized, ""))
	if err != nil {
		return nil, err
	}
	return info, nil
}

// discoverOne is the shared per-CLI probe. It performs, in order:
//
//  1. exec.LookPath — resolves the binary path. Missing binary short-
//     circuits with Available=false and the hardcoded provider catalog
//     (so the HTTP catalog still advertises what the CLI would serve).
//  2. adapter.DetectCLIVersion — runs `<cli> --version` under a 10s
//     timeout. A failure (version probe erroring or timing out) marks
//     the CLI Available=false but keeps Path populated so callers can
//     distinguish "missing" from "installed-but-broken".
//  3. Discoverer.DiscoverModels — fetches the provider/model catalog.
//     V1 uses the hardcoded catalog; a future adapter-backed
//     implementation can surface per-CLI errors here.
//
// Errors at any step are absorbed into the returned CLIInfo (Available
// drops to false, Version/Path are filled in best-effort) and the
// returned error is nil. The only path that returns an error is the
// Discoverer returning one AND the catalog having no fallback — the v1
// hardcoded Discoverer never errors, so discoverOne never errors either.
func discoverOne(ctx context.Context, name string, d Discoverer) (*CLIInfo, error) {
	info := &CLIInfo{Name: name, Available: false}

	path, err := exec.LookPath(name)
	if err != nil {
		// Binary not on PATH. Still surface the catalog so the HTTP
		// endpoint can preview what this CLI would serve once installed.
		providers, _ := d.DiscoverModels(ctx)
		info.Providers = providers
		return info, nil
	}
	info.Path = path

	version, err := adapter.DetectCLIVersion(ctx, path)
	if err != nil {
		// LookPath succeeded but --version failed (timeout, non-zero
		// exit, no parseable version). Mark unavailable, keep Path so
		// callers can distinguish from "missing".
		providers, _ := d.DiscoverModels(ctx)
		info.Providers = providers
		return info, nil
	}
	info.Version = version
	info.Available = true

	providers, _ := d.DiscoverModels(ctx)
	info.Providers = providers
	return info, nil
}

// fallbackProviders returns the hardcoded catalog for the given CLI, or an
// empty slice if the CLI is unknown. Used when discoverOne itself errors
// — the v1 path never hits this, but it keeps the fault-isolation
// invariant bulletproof.
func fallbackProviders(cli string) []ProviderInfo {
	providers, _ := newHardcodedDiscoverer(cli).DiscoverModels(context.Background())
	return providers
}

// isSupportedCLI reports whether name (already normalized to lowercase) is
// in the v1 supportedCLIs set. Used by DiscoverCLI to reject typos with
// an actionable error rather than silently returning Available=false.
func isSupportedCLI(name string) bool {
	for _, s := range supportedCLIs {
		if s == name {
			return true
		}
	}
	return false
}

// normalizeCLIName lowercases the CLI name so callers can pass "Claude",
// "CLAUDE", or "claude" interchangeably. Only ASCII case folding is
// applied — CLI names are ASCII identifiers, so unicode.ToLower would be
// overkill and could mangle non-ASCII input in surprising ways.
func normalizeCLIName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ParseModelName splits a `CLI/provider/model` identifier into its three
// components. The form is the canonical routing key used throughout
// aiclibridge: it identifies both the CLI to invoke and the model the CLI
// should select.
//
// Validation:
//
//   - Exactly three slash-separated segments. Fewer or more is an error.
//   - Every segment non-empty. Leading/trailing slashes and doubled
//     slashes (e.g. "claude//claude-sonnet-4.5") are rejected.
//   - The CLI segment is case-insensitively normalized to lowercase, so
//     "Claude/anthropic/claude-sonnet-4.5" parses identically to
//     "claude/anthropic/claude-sonnet-4.5".
//
// Provider and model segments are returned verbatim — their case is
// semantically significant to the upstream provider and the CLI, so
// "openai/GPT-5" must round-trip unchanged. Use ModelName to reassemble.
//
// ParseModelName does not validate that the CLI is in supportedCLIs or
// that the provider/model exist in the catalog; callers that need that
// check should look up the returned tuple against DefaultCatalog().
func ParseModelName(model string) (cli, provider, modelName string, err error) {
	parts := strings.Split(model, "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("detect: model name %q must be in CLI/provider/model form (got %d segments)", model, len(parts))
	}
	cli = parts[0]
	provider = parts[1]
	modelName = parts[2]
	if cli == "" || provider == "" || modelName == "" {
		return "", "", "", fmt.Errorf("detect: model name %q has an empty segment", model)
	}
	cli = normalizeCLIName(cli)
	return cli, provider, modelName, nil
}

// ModelName assembles a `CLI/provider/model` identifier from its
// components. It is the inverse of ParseModelName: the assembled string
// round-trips through ParseModelName and recovers (cli, provider, model)
// with cli normalized to lowercase.
//
// ModelName does NOT validate its inputs — it is a formatter, not a
// parser. Callers passing empty strings will get a malformed identifier
// like "claude//"; that is intentional, so a misconfigured caller shows
// up in logs as a malformed routing key rather than failing silently.
func ModelName(cli, provider, model string) string {
	return strings.Join([]string{normalizeCLIName(cli), provider, model}, "/")
}
