// Package main is the aiclibridge command-line entry point.
//
// aiclibridge ships as a single binary with subcommand dispatch (the
// AionUi web-cli model): serve / run / agents / models / cancel / get /
// version. main() is intentionally thin — it peels the first arg off
// os.Args, dispatches to the matching run<Name> function in cli.go, and
// exits with the returned code. Every subcommand owns its own flag
// parsing and lifecycle, so adding a verb is a one-file edit (a new
// function in cli.go plus a case here) rather than a restructure.
//
// The dispatcher also honours two top-level flag forms so the binary
// behaves like a conventional Unix tool: `aiclibridge` with no args or
// `-h` / `--help` prints usage to stdout and exits 0, and `--version`
// / `-v` is an alias for the `version` subcommand.
package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printUsage(os.Stdout)
		os.Exit(0)
	}

	var cmd string
	var rest []string
	// Support --version / -v as a top-level flag (no subcommand prefix)
	// so `aiclibridge --version` works alongside `aiclibridge version`.
	if args[0] == "--version" || args[0] == "-v" {
		cmd = "version"
	} else {
		cmd = args[0]
		rest = args[1:]
	}

	var code int
	switch cmd {
	case "serve":
		code = runServe(rest)
	case "start":
		code = runStart(rest)
	case "stop":
		code = runStop(rest)
	case "restart":
		code = runRestart(rest)
	case "upgrade":
		code = runUpgrade(rest)
	case "run":
		code = runRun(rest)
	case "agents":
		code = runAgents(rest)
	case "models":
		code = runModels(rest)
	case "cancel":
		code = runCancel(rest)
	case "get":
		code = runGet(rest)
	case "version", "--version", "-v":
		code = runVersion(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		printUsage(os.Stderr)
		code = 2
	}
	os.Exit(code)
}
