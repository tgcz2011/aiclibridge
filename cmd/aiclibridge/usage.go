// Package main hosts the top-level usage banner for aiclibridge.
//
// usage.go carries the single printUsage formatter that main.go calls
// for `aiclibridge`, `aiclibridge --help`, `aiclibridge -h`, and the
// `unknown command %q` fallback. It is plain text (not a help flag
// subsystem) because the binary supports both a no-arg usage form and a
// flag form, and the same body must render to either stdout (help) or
// stderr (error fallback) — printUsage takes the writer so main.go can
// pick the stream without duplicating the body.
package main

import (
	"fmt"
	"io"
)

// printUsage writes the top-level usage banner and a per-subcommand
// summary to w. The layout mirrors the AionUi web-cli subcommand surface
// (serve/run/version/help) so a user familiar with that shape finds the
// same mental model here: each verb is one line with its flags in
// brackets, so a quick scan tells the operator what they can type.
//
// The body is a single Fprintf call (not a template) so adding a
// subcommand is a one-line edit; the dispatcher in main.go is the other
// half of the contract. Per-command `-h` help is left to the flag
// package's auto-generated usage and is intentionally not duplicated
// here — printUsage is the high-level map, the flag package is the
// detailed legend.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `aiclibridge is a unified bridge for AI coding CLIs.

Usage:
  aiclibridge <command> [flags] [args]

Commands:
  serve    Start the HTTP daemon in the foreground (original behaviour).
  start    Start the HTTP daemon in the background; survives terminal close.
           (Unix: full support; Windows: best-effort, no graceful SIGTERM.)
  stop     Stop the background daemon (reads pid file, SIGTERM then SIGKILL).
           (Unix: full support; Windows: best-effort, no graceful SIGTERM.)
  restart  Stop then start the background daemon.
           (Unix: full support; Windows: best-effort, no graceful SIGTERM.)
  upgrade  Self-update via 'go install' then restart the daemon.
           (Unix: full support; Windows: best-effort, no graceful SIGTERM.)
  uninstall  Remove the aiclibridge binary. --purge also removes data dir
             and config files. --yes/-y skips the confirmation prompt.
  update   Check GitHub for a newer release; prints a notice on stderr.
           Best-effort: network/rate-limit failures exit 0 so scripting
           is never blocked. Use 'update check --json' for machine output.
  run      Run a single prompt against a CLI without a long-lived daemon.
  agents   List detected CLIs and their providers/models (local detect).
  models   List every CLI/provider/model routing key (local detect).
  cancel   Cancel a running run via the daemon's HTTP API.
  get      Fetch a run's history via the daemon's HTTP API.
  version  Print the aiclibridge version and exit.

Top-level flags:
  -h, --help       Print this usage and exit.
  -v, --version    Print the version and exit (same as 'version').

Quick reference:
  aiclibridge run --model claude/anthropic/claude-sonnet-4.5 "fix the bug"
  echo "refactor this" | aiclibridge run --model codex/openai/gpt-5
  aiclibridge run --model opencode/openai/gpt-5 "fix bug" -- --pure   # pass --pure to opencode
  aiclibridge agents
  aiclibridge start                          # background daemon on 127.0.0.1:8787
  aiclibridge stop                           # stop the background daemon
  aiclibridge serve --listen 127.0.0.1:8787  # foreground daemon

Run 'aiclibridge <command> -h' for command-specific flags.
`)
}
