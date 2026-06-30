// Package detect discovers the AI coding CLIs installed on the host and
// exposes the canonical model catalog the bridge advertises to clients.
//
// The package surfaces two complementary views:
//
//   - DefaultCatalog returns the hardcoded provider/model tables for every
//     supported CLI (claude, codex, opencode, openclaw) regardless of
//     installation state. This is the "what the bridge can know about"
//     view used by clients to preview routing options.
//   - Discover probes the host in parallel and reports, per CLI, whether
//     the binary is on PATH, its --version, and the providers/models it
//     serves. This is the "what is actually installed here" view.
//
// Both views share the `CLI/provider/model` naming convention (see
// ParseModelName / ModelName): every model identifier is a three-segment
// slash-delimited string where the first segment selects the CLI, the
// second the upstream provider, and the third the model.
//
// Fault isolation: a single CLI failing detection (binary missing,
// --version probe timing out, model discovery erroring) is recorded as
// Available=false on that CLI's entry and never propagates as an error
// from Discover. Only a wholesale failure of the discovery machinery
// (context cancellation) returns an error. The HTTP catalog endpoint
// relies on this contract to always render something, even on a host
// with zero CLIs installed.
//
// # Future extension
//
// v1 ships a hardcoded provider/model catalog (hardcodedCatalog). The
// Discoverer interface is the seam for replacing it with an adapter-backed
// implementation that queries `<cli> models list` (or per-CLI equivalent)
// at runtime — the rest of the package is shaped to accept that
// implementation without touching call sites.
package detect
