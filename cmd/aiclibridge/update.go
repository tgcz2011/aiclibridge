// Package main hosts the update-check subcommand for aiclibridge.
//
// update.go adds the `aiclibridge update` verb (and its `check` sub-verb):
// it queries GitHub for the latest non-prerelease release, compares the
// tag to the running binary's Version, and prints a one-paragraph notice
// to stderr. The check is best-effort — a network failure or rate-limit
// is logged as a warning and the subcommand exits 0 so it never blocks
// scripting or CI.
//
// `aiclibridge update` (no sub-verb) is equivalent to `update check` so
// users do not have to remember the sub-verb. A future `update install`
// may download + atomically replace the binary; for now self-update
// remains the `aiclibridge upgrade` verb's go-install path.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/update"
)

// updateCheckTimeout bounds the GitHub API call so `aiclibridge update`
// never blocks a script longer than necessary. 10s matches the default
// client timeout in internal/update; the context is the actual guard
// because the default client only enforces per-request deadlines.
const updateCheckTimeout = 10 * time.Second

// runUpdate is the dispatcher for the `update` verb. It peels the next
// arg to choose between `check` (the only sub-verb today) and the
// implicit no-arg form. An unknown sub-verb surfaces as a usage error.
//
// Exit codes:
//   - 0: check completed (regardless of whether an update is available)
//   - 1: an unexpected error occurred (e.g. bad flag)
//   - 2: usage error
//
// The check itself never returns non-zero on "no update" or "network
// failed" — that is intentional, so `aiclibridge update && ...` chains
// do not break when the host is offline.
func runUpdate(args []string) int {
	// `aiclibridge update` (no args) is equivalent to `update check`.
	if len(args) == 0 {
		return runUpdateCheck(nil)
	}
	switch args[0] {
	case "check":
		return runUpdateCheck(args[1:])
	case "-h", "--help":
		printUpdateUsage(os.Stdout)
		return 0
	default:
		// A leading flag (e.g. `update --json`) is forwarded to
		// runUpdateCheck so the sub-verb is truly optional.
		if strings.HasPrefix(args[0], "-") {
			return runUpdateCheck(args)
		}
		fmt.Fprintf(os.Stderr, "aiclibridge update: unknown subcommand %q\n", args[0])
		printUpdateUsage(os.Stderr)
		return 2
	}
}

// runUpdateCheck performs the actual GitHub release lookup. It accepts
// a --quiet flag to suppress the "up to date" line (useful in cron),
// and a --json flag to emit machine-readable output for tooling.
func runUpdateCheck(args []string) int {
	fs := flag.NewFlagSet("update check", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "suppress output when already up to date")
	jsonOut := fs.Bool("json", false, "emit JSON (for tooling) instead of human text")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()

	client := &http.Client{Timeout: updateCheckTimeout}
	info, err := update.Check(ctx, client, update.DefaultOwner, update.DefaultRepo,
		Version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		// Best-effort: a network failure, rate-limit, or 404 (no
		// releases yet) is a warning, not a hard error. Exit 0 so
		// scripting is not broken by transient GitHub issues.
		fmt.Fprintf(os.Stderr, "aiclibridge update: could not check for updates: %v\n", err)
		return 0
	}

	if *jsonOut {
		writeUpdateJSON(info)
		return 0
	}

	if !info.HasUpdate && *quiet {
		return 0
	}

	// Human-readable notice goes to stderr so stdout piping (e.g.
	// `aiclibridge update | tee`) is not polluted by the notice.
	fmt.Fprint(os.Stderr, update.FormatNotice(info))
	return 0
}

// writeUpdateJSON emits the Info as a single JSON object on stdout. The
// shape matches update.Info 1:1 so a tool can parse it without a
// separate schema.
func writeUpdateJSON(info *update.Info) {
	if info == nil {
		fmt.Fprintln(os.Stdout, "{}")
		return
	}
	fmt.Printf(`{"current":%q,"latest":%q,"latest_tag":%q,"has_update":%t,"html_url":%q,"asset_url":%q}`+"\n",
		info.Current, info.Latest, info.LatestTag, info.HasUpdate, info.HTMLURL, info.AssetURL)
}

// printUpdateUsage writes the `update` subcommand's help. It is mounted
// at `aiclibridge update -h` and at the `unknown subcommand` fallback.
func printUpdateUsage(w *os.File) {
	fmt.Fprint(w, `aiclibridge update — check for a newer aiclibridge release on GitHub.

Usage:
  aiclibridge update [check] [flags]

Subcommands:
  check   Fetch the latest release tag and compare to the running version. (default)

Flags:
  --quiet   Suppress output when already up to date (useful in cron).
  --json    Emit a JSON object on stdout instead of human text.
  -h, --help  Show this help and exit.

Exit codes:
  0   Check completed (regardless of whether an update is available).
  1   Unexpected error.
  2   Usage error.

The check is best-effort: a network failure or GitHub rate-limit is
logged as a warning and exits 0 so scripting is never blocked by
transient GitHub issues.

Examples:
  aiclibridge update                 # human notice on stderr
  aiclibridge update check --quiet   # cron-friendly; silent when up to date
  aiclibridge update check --json    # machine-readable output on stdout

To actually install the new binary, run:
  aiclibridge upgrade                # via 'go install' (requires Go)
  # or
  curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh
`)
}

// maybeAsyncUpdateCheck is the daemon-side hook: it fires the same
// check as `update check` in a background goroutine with a short
// timeout, and logs the result via the provided logger-style sink
// (logf). It is called by serveStack at daemon startup so an operator
// who starts a stale daemon sees a one-line hint in the log without
// having to run `update check` manually.
//
// The function returns immediately (non-blocking); the goroutine writes
// to logf which must be safe for concurrent use. A failure is silent —
// daemon startup must not depend on GitHub being reachable.
func maybeAsyncUpdateCheck(logf func(format string, args ...any)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		defer cancel()

		client := &http.Client{Timeout: updateCheckTimeout}
		info, err := update.Check(ctx, client, update.DefaultOwner, update.DefaultRepo,
			Version, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			// Silent: daemon startup must not depend on GitHub.
			return
		}
		if info.HasUpdate {
			logf("aiclibridge update available: %s → %s (%s)",
				info.Current, info.LatestTag, info.HTMLURL)
		}
	}()
}
