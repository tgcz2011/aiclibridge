// Package main hosts the version constant and printer for aiclibridge.
//
// version.go is intentionally tiny: it carries the canonical version
// string stamped at release time and the Build/Commit placeholders that
// -ldflags can populate at link time. The `version` subcommand (see
// cli.go:runVersion) calls printVersion so the same formatter is reused
// by tests without going through os.Stdout.
package main

import (
	"fmt"
	"io"
)

// Version is the canonical version string for the aiclibridge binary.
// Bumped per release; printed by `aiclibridge version` and surfaced by
// the `--version` / `-v` top-level flag.
//   - v0.1.0: first releaseable cut — 6 CLIs (claude/codex/opencode/
//     openclaw/qwen/gemini), CLI subcommands, OpenAI/Anthropic/native
//     API surfaces, full docs.
//   - v0.2.0: extends to 19 CLIs from AionUi's ACP catalogue — adds
//     codebuddy (stream-json), copilot/goose/cursor/kimi/kiro/qoder/
//     hermes/auggie (ACP JSON-RPC), and droid/snow/vibe/aion (stubs).
//   - v0.3.0: token/price stats API, high-concurrency SQLite (WAL +
//     connection pool), background daemon (start/stop/restart/upgrade),
//     per-request custom_args forwarding (e.g. `run -- --pure`).
//   - v0.4.0: cross-platform daemon (Windows best-effort), concurrency cap
//     with queueing (semaphore + 503/Retry-After), pprof auth guard on
//     non-loopback listen, configurable claude permission_mode, schema
//     migration framework (schema_migrations), CLI version-check helpers,
//     bounded in-memory event slice for non-streaming responses, pricing
//     sort.Slice, daemon/server init de-duplication.
//   - v0.4.1: release workflow now ships a Windows amd64 binary
//     (aiclibridge-windows-amd64.zip) alongside the darwin/linux tarballs;
//     no code changes from v0.4.0.
//   - v0.5.0: one-line installers (scripts/install.sh for macOS/Linux,
//     scripts/install.ps1 for Windows) with multi-arch + sha256 verify;
//     `aiclibridge update` subcommand checks GitHub for a newer release
//     (supports --json / --quiet); daemon startup logs an async update
//     hint when a newer release exists.
//   - v0.5.1: installer robustness — install.sh now resolves the latest
//     tag via github.com/releases/latest 302 redirect (no API quota) with
//     api.github.com fallback; all curl calls use --http1.1 to avoid the
//     HTTP/2 framing error common behind GFW/proxies; GITHUB_MIRROR env
//     var / --mirror flag for mirror-prefixed downloads; --retry +
//     --connect-timeout for transient failures; install.ps1 gains UA
//     header + redirect-based tag resolution.
//   - v0.5.2: release archives now contain a plain 'aiclibridge' binary
//     (was 'aiclibridge-{goos}-{goarch}') so users can tar | mv without
//     renaming; install.sh/ps1 accept both names (backward compatible
//     with v0.5.0/v0.5.1 archives); install target is always 'aiclibridge'.
//   - v0.5.3: install.sh fixes — TARGET is now computed AFTER the
//     /usr/local/bin → ~/.local/bin fallback (v0.5.2 computed it before,
//     so fallback still tried to write /usr/local/bin → "Permission
//     denied"); `read ANSWER` uses /dev/tty under `curl|sh` (stdin is
//     the curl pipe, not a terminal, so plain `read` consumed script
//     bytes and corrupted execution).
//   - v0.5.4: three usability fixes —
//     1) Local commands (run/agents/models) are now quiet by default
//        (LevelError); add --debug/-d to see detect logs. Logger writes
//        to stderr (was stdout, which polluted `run` text output).
//     2) codebuddy adapter fixed: was using qwen's flags (--bare/--yolo/
//        -m/--max-session-turns) which codebuddy doesn't support; now
//        uses codebuddy's actual flags (--print/--dangerously-skip-
//        permissions/--model/--max-turns).
//     3) codebuddy catalog expanded from 2 placeholder models to all 15
//        real models from `codebuddy --help` (glm-5.2, minimax-m3,
//        kimi-k2.7, deepseek-v4-pro, etc.); pricing table updated.
const Version = "0.5.4"

// Build and Commit are populated by -ldflags at link time
// (`-X main.Build=... -X main.Commit=...`). They stay empty for local
// `go build` / `go run` invocations; printVersion omits the
// corresponding lines when unset so a local build prints a single
// `aiclibridge <version>` line. v0.1.0 ships without stamping them —
// the CI pipeline can wire them in later without touching this file.
var (
	Build  = ""
	Commit = ""
)

// printVersion writes the version banner to w. The first line is always
// `aiclibridge <version>`; when Build or Commit were injected at link
// time they follow on their own `build:` / `commit:` lines. The output
// is plain text (not JSON) because the `version` subcommand is meant
// for humans glancing at a terminal, not for machine parsing.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "aiclibridge %s\n", Version)
	if Build != "" {
		fmt.Fprintf(w, "build:  %s\n", Build)
	}
	if Commit != "" {
		fmt.Fprintf(w, "commit: %s\n", Commit)
	}
}
