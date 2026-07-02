// Package main hosts the uninstall subcommand for aiclibridge.
//
// uninstall.go removes the aiclibridge binary and, optionally, its data
// directory and config files. The flow:
//
//  1. Stop any running daemon (best-effort — reuses the stop logic).
//  2. Locate the binary via exec.LookPath + common install paths
//     (/usr/local/bin, ~/.local/bin on Unix; %USERPROFILE%\bin on Windows).
//  3. Prompt for confirmation unless --yes/-y is passed.
//  4. Remove the binary (with sudo if the file is not writable by the
//     current user and sudo is available).
//  5. If --purge is passed, also remove the data directory (from config)
//     and the config files (./aiclibridge.yaml, ~/.aiclibridge/).
//
// The command never deletes anything without confirmation (or --yes),
// and it never removes directories outside the known aiclibridge paths.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runUninstall implements `aiclibridge uninstall`. It stops the daemon,
// removes the binary, and optionally (--purge) removes data + config.
//
// Flags:
//
//	--yes / -y    Skip the confirmation prompt.
//	--purge       Also remove the data directory and config files.
//	--config      Path to config file (to locate the data dir for --purge).
func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	shortYes := fs.Bool("y", false, "skip confirmation prompt (shorthand)")
	purge := fs.Bool("purge", false, "also remove data directory and config files")
	configPath := fs.String("config", "", "path to config file (to locate data dir for --purge)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	skipConfirm := *yes || *shortYes

	// ── 1. Stop daemon (best-effort) ──
	// Reuse stop logic so the pid file is cleaned up and the process
	// gets a graceful SIGTERM before we remove the binary.
	fmt.Fprintln(os.Stderr, "aiclibridge: stopping daemon if running...")
	_ = runStop([]string{})
	// Also kill any stray processes matching the binary name.
	killStrayAiclibridge()

	// ── 2. Locate binary ──
	binPath := locateBinary()
	if binPath == "" {
		fmt.Fprintln(os.Stderr, "aiclibridge: binary not found on PATH or in common install locations")
		fmt.Fprintln(os.Stderr, "aiclibridge: nothing to uninstall (already removed?)")
		return 0
	}

	// ── 3. Confirm ──
	fmt.Fprintf(os.Stderr, "aiclibridge: will remove binary: %s\n", binPath)
	if *purge {
		paths := purgePaths(*configPath)
		for _, p := range paths {
			fmt.Fprintf(os.Stderr, "aiclibridge: will also remove: %s\n", p)
		}
	}
	if !skipConfirm {
		if !promptYesNo("Proceed with uninstall?") {
			fmt.Fprintln(os.Stderr, "aiclibridge: aborted.")
			return 1
		}
	}

	// ── 4. Remove binary ──
	if err := removePathWithSudo(binPath); err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge: could not remove %s: %v\n", binPath, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "aiclibridge: removed %s\n", binPath)

	// ── 5. Purge data + config ──
	if *purge {
		for _, p := range purgePaths(*configPath) {
			if err := removePathWithSudo(p); err != nil {
				fmt.Fprintf(os.Stderr, "aiclibridge: could not remove %s: %v (continuing)\n", p, err)
			} else {
				fmt.Fprintf(os.Stderr, "aiclibridge: removed %s\n", p)
			}
		}
	}

	fmt.Fprintln(os.Stderr, "aiclibridge: uninstall complete.")
	if !*purge {
		fmt.Fprintln(os.Stderr, "aiclibridge: data directory and config files were kept. Use --purge to remove them.")
	}
	return 0
}

// locateBinary finds the aiclibridge executable. It tries exec.LookPath
// first (which searches PATH), then falls back to common install
// locations that install.sh / install.ps1 use.
func locateBinary() string {
	// exec.LookPath finds it if it's on PATH.
	if p, err := exec.LookPath("aiclibridge"); err == nil {
		return p
	}

	// Fallback: common install locations from install.sh / install.ps1.
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", "aiclibridge"),
		)
	}
	candidates = append(candidates, "/usr/local/bin/aiclibridge")

	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// purgePaths returns the data directory and config file paths that
// --purge should remove. It loads the config (best-effort) to find the
// data dir, then adds the known config file locations.
func purgePaths(configPath string) []string {
	var paths []string

	// Data directory from config.
	cfg, err := loadConfig(configPath)
	if err == nil && cfg.DataDir != "" {
		paths = append(paths, cfg.DataDir)
	} else {
		// If config load failed, still try the default.
		paths = append(paths, "./data")
	}

	// Config files: ./aiclibridge.yaml and ~/.aiclibridge/
	if fileExistsSync("./aiclibridge.yaml") {
		paths = append(paths, "./aiclibridge.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil {
		homeCfgDir := filepath.Join(home, ".aiclibridge")
		if dirExists(homeCfgDir) {
			paths = append(paths, homeCfgDir)
		}
	}

	return paths
}

// fileExistsSync reports whether path is an existing regular file.
func fileExistsSync(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// removePathWithSudo removes a file or directory (recursively). If the
// plain os.RemoveAll fails (typically permission denied on /usr/local/bin),
// it retries with sudo when available.
func removePathWithSudo(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	// Plain remove failed — try sudo if available.
	if _, lookupErr := exec.LookPath("sudo"); lookupErr == nil {
		cmd := exec.Command("sudo", "rm", "-rf", path)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("remove %s: permission denied and sudo unavailable", path)
}

// promptYesNo asks the user a yes/no question on stderr and reads the
// answer from /dev/tty (so it works under `curl | sh` where stdin is
// the curl pipe). Returns true only for explicit y/yes.
func promptYesNo(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	var answer string
	// Read from /dev/tty so `curl | sh` works (stdin is the pipe).
	tty, err := os.Open("/dev/tty")
	if err == nil {
		defer tty.Close()
		fmt.Fscanln(tty, &answer)
	} else {
		// No /dev/tty (Windows or non-interactive) — read stdin.
		fmt.Fscanln(os.Stdin, &answer)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// killStrayAiclibridge kills any remaining aiclibridge processes that
// aren't the current process. On Unix it uses pkill -f; on Windows it
// uses taskkill. Best-effort — failures are silently ignored.
func killStrayAiclibridge() {
	if _, err := exec.LookPath("pkill"); err == nil {
		_ = exec.Command("pkill", "-f", "aiclibridge").Run()
		return
	}
	if _, err := exec.LookPath("taskkill"); err == nil {
		_ = exec.Command("taskkill", "/F", "/IM", "aiclibridge.exe").Run()
	}
}
