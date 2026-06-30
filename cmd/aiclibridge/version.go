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
// the `--version` / `-v` top-level flag. v0.1.0 is the first
// releaseable cut — the CLI surface (serve/run/agents/models/cancel/
// get/version) is feature-complete for the one-shot invocation mode.
const Version = "0.1.0"

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
