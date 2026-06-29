// Package main is the aiclibridge command-line entry point.
//
// aiclibridge exposes a unified HTTP API in front of multiple AI coding
// CLIs (Claude Code, Codex, OpenCode, OpenClaw, Gemini). The daemon
// logic lives under internal/ and is wired up here once the adapters
// land in later milestones.
package main

import (
	"fmt"
	"os"
)

const usage = "aiclibridge — unified API bridge for AI coding CLIs\n" +
	"\n" +
	"Usage:\n" +
	"  aiclibridge           print this message and exit 0\n" +
	"  aiclibridge --help    print this message and exit 0\n"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Print(usage)
		return
	}
	fmt.Println("aiclibridge")
}
