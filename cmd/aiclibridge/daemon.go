// Package main hosts the daemon lifecycle subcommands for aiclibridge.
//
// daemon.go adds four verbs — start / stop / restart / upgrade — that
// wrap `serve` with Unix daemon management: `start` forks a detached
// child (new session, no controlling tty) that runs the HTTP server with
// stdout/stderr redirected to a log file and its PID recorded in a pid
// file, so closing the launching terminal leaves the daemon running.
// `stop` reads the pid file and SIGTERM/SIGKILLs the process. `restart`
// is stop+start. `upgrade` runs `go install ...@latest` then restarts.
//
// The foreground server startup (runDaemonForeground) shares its server
// wiring with runServe via the serveStack helper in cli.go: both build
// the same (data dir → SQLite store → detect → facade → HTTP server)
// stack and run the same signal-driven graceful shutdown. runDaemonForeground
// adds pid-file management on top; runServe keeps its foreground-only,
// no-pid-file semantics.
//
// Platform split: forkChild / processSignal / isRunning live in
// daemon_unix.go (Setsid fork + syscall.Kill) and daemon_windows.go
// (CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS + CTRL_BREAK/TerminateProcess).
// The daemon verbs are fully supported on Unix; on Windows they are
// best-effort (no graceful SIGTERM — CTRL+BREAK is sent but the fallback
// is TerminateProcess).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/config"
)

// ── PID / log file helpers ──

// pidFilePath returns the daemon PID file path under the data dir. The
// path is derived from cfg.DataDir so a user pointing the data dir at a
// dedicated location (e.g. /var/lib/aiclibridge) gets the pid file there
// too — one place to look, one place to clean up.
func pidFilePath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "aiclibridge.pid")
}

// logFilePath returns the daemon stdout/stderr log path under the data
// dir. The forked child's os.Stdout/os.Stderr are redirected here by the
// parent (cmd.Stdout = logFile), so every line the child prints — slog
// JSON logs, startup banner, panic traces — lands in this single file.
func logFilePath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "aiclibridge.log")
}

// readPID reads the PID from the pid file. Returns 0, nil if the file is
// missing so callers can treat "no file" and "empty file" identically:
// the daemon is not running. A parse failure or non-numeric content is
// an error (the file was corrupted); callers surface it rather than
// silently guessing.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse pid file %q: %w", path, err)
	}
	return pid, nil
}

// writePID writes the PID atomically (write temp + rename) so a reader
// never observes a half-written file. The temp file lives next to the
// target so the rename is on the same filesystem (rename across devices
// fails). The parent dir is assumed to exist — callers (runDaemonForeground)
// MkdirAll the data dir before writing.
func writePID(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removePID removes the pid file (best effort). Used during shutdown and
// when a stale pid is detected; a failure is silently ignored because
// the process is already going away and a leftover file is harmless
// (runStart overwrites it and runStop treats a missing file as "not
// running").
func removePID(path string) {
	_ = os.Remove(path)
}

// daemonAddr returns the listen address for display. It exists as a
// named helper so call sites read as "show me the daemon address" rather
// than reaching into cfg.Listen directly, and so a test can exercise the
// mapping without constructing a full server.
func daemonAddr(cfg *config.Config) string {
	return cfg.Listen
}

// ── runStart ──

// runStart launches the daemon in the background. The parent process
// forks a detached child (Unix: Setsid — new session, no controlling tty;
// Windows: CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS) that re-execs
// itself as `aiclibridge start --foreground [--config ...]`, redirects
// the child's stdin from /dev/null and stdout+stderr to the log file,
// and exits 0 immediately after the fork succeeds. The child writes its
// own PID to the pid file (in runDaemonForeground) so the parent can
// poll for it to confirm the child reached server startup.
//
// If a daemon is already running (pid file present + process alive),
// runStart refuses with exit 1 rather than forking a second child that
// would fail to bind the listen port.
//
// The --foreground flag is internal: it is set only by the parent when
// re-execing the child. A user typing `start --foreground` runs the
// server in the foreground with pid-file management — useful for
// debugging under a process supervisor that handles detachment itself.
func runStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	foreground := fs.Bool("foreground", false, "run in foreground (internal: used by the forked child)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge start: %v\n", err)
		return 1
	}

	pidFile := pidFilePath(cfg)

	// Refuse if a daemon is already running. A stale pid file (process
	// gone) is silently overwritten below; only a live pid blocks start.
	if pid, _ := readPID(pidFile); pid != 0 && isRunning(pid) {
		fmt.Fprintf(os.Stderr, "aiclibridge: daemon already running (pid %d, %s)\n", pid, daemonAddr(cfg))
		return 1
	}

	if *foreground {
		// We ARE the daemon child. Run the server, write the pid file,
		// and remove it on exit via runDaemonForeground's deferred
		// removePID.
		return runDaemonForeground(cfg, pidFile)
	}

	// We are the parent. Fork a detached child that re-execs self with
	// --foreground so the child runs the server path above. forkChild
	// (daemon_unix.go / daemon_windows.go) sets the platform-appropriate
	// detach attributes; stdin is nil (no input); stdout+stderr are
	// appended to the log file so the child's output is captured after
	// the parent exits.
	logFile, err := os.OpenFile(logFilePath(cfg), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge start: open log file: %v\n", err)
		return 1
	}

	cmdArgs := []string{"start", "--foreground"}
	if *configPath != "" {
		cmdArgs = append(cmdArgs, "--config", *configPath)
	}
	cmd, err := forkChild(cmdArgs, logFile)
	if err != nil {
		_ = logFile.Close()
		fmt.Fprintf(os.Stderr, "aiclibridge start: fork daemon: %v\n", err)
		return 1
	}
	// The child has its own dup'd fds; close the parent's copy so we
	// don't leak a file descriptor.
	_ = logFile.Close()

	// Don't wait — the parent exits immediately. Poll the pid file for
	// up to 2 seconds to confirm the child reached the point of writing
	// its PID (i.e. server startup proceeded past config load). The
	// child's PID is also available as cmd.Process.Pid, but reading it
	// back from the file proves the child is actually running, not just
	// forked.
	childPID := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pid, rerr := readPID(pidFile); rerr == nil && pid != 0 {
			childPID = pid
			break
		}
		// If the child already exited (e.g. bind failure), bail out
		// early rather than polling the full 2s.
		if !isRunning(childPID) {
			fmt.Fprintf(os.Stderr, "aiclibridge start: daemon exited immediately; see log: %s\n", logFilePath(cfg))
			return 1
		}
		time.Sleep(10 * time.Millisecond)
	}

	fmt.Printf("aiclibridge daemon started (pid %d) on %s\n", childPID, daemonAddr(cfg))
	fmt.Printf("logs: %s\n", logFilePath(cfg))
	fmt.Printf("pid file: %s\n", pidFile)
	fmt.Println("you can close this terminal; the daemon will keep running.")
	fmt.Println("stop with: aiclibridge stop")
	return 0
}

// runDaemonForeground is the in-child server path: it writes the pid
// file, delegates to serveStack for the (data dir → SQLite store →
// detect → facade → HTTP server → signal-driven graceful shutdown)
// lifecycle, and removes the pid file on exit via the deferred
// removePID. The deferred removePID guarantees the pid file is cleaned
// up on every exit path — signal, server failure, or panic-recovered
// return.
//
// The server wiring is shared with runServe via serveStack (cli.go);
// runDaemonForeground adds only the pid-file management that runServe
// deliberately omits.
func runDaemonForeground(cfg *config.Config, pidFile string) int {
	if err := writePID(pidFile, os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge start: write pid file: %v\n", err)
		return 1
	}
	defer removePID(pidFile)

	logger := setupLogger(cfg.LogLevel)
	return serveStack(cfg, logger, "start")
}

// ── runStop ──

// runStop terminates the daemon by reading the pid file and sending
// SIGTERM (Unix: syscall.Kill; Windows: CTRL+BREAK, best-effort). If the
// process does not exit within 10 seconds it is SIGKILLed (Unix) /
// TerminateProcess'd (Windows). A stale pid file (process gone) is
// removed and reported as "not running" rather than treated as an error
// condition.
//
// The pid file is removed after the process is confirmed dead, so a
// subsequent `start` sees no live daemon and forks cleanly. Removing it
// unconditionally at the end (even after SIGKILL) matches the daemon's
// own deferred removePID — whichever side wins the race, the file is
// gone.
//
// Windows has no graceful SIGTERM: processSignal(SIGTERM) sends
// CTRL+BREAK to the child's process group, which the Go runtime maps to
// os.Interrupt — if the child installed a handler (serveStack does) it
// can shut down gracefully, but this is best-effort. The SIGKILL fallback
// (TerminateProcess) is unconditional.
func runStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge stop: %v\n", err)
		return 1
	}

	pidFile := pidFilePath(cfg)
	pid, err := readPID(pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge stop: read pid file: %v\n", err)
		return 1
	}
	if pid == 0 {
		fmt.Fprintln(os.Stderr, "aiclibridge: daemon not running (no pid file)")
		return 1
	}
	if !isRunning(pid) {
		removePID(pidFile)
		fmt.Fprintln(os.Stderr, "aiclibridge: daemon not running (stale pid file removed)")
		return 1
	}

	// SIGTERM first for a graceful shutdown (drain in-flight requests).
	// processSignal is platform-dispatched: syscall.Kill on Unix,
	// CTRL+BREAK on Windows.
	if err := processSignal(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge stop: signal: %v\n", err)
		return 1
	}

	// Wait up to 10s for the process to exit.
	for i := 0; i < 100; i++ {
		if !isRunning(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill if still alive.
	if isRunning(pid) {
		_ = processSignal(pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	}

	removePID(pidFile)
	fmt.Println("aiclibridge daemon stopped")
	return 0
}

// ── runRestart ──

// runRestart stops the daemon (if running) and starts it again. The stop
// step's exit code is deliberately ignored: if the daemon was not
// running, stop returns 1 but restart should still bring it up. The
// original args (typically --config) are forwarded to both stop and
// start so a non-default config is honoured across the restart.
func runRestart(args []string) int {
	// Ignore stop's exit code — "not running" is not a restart failure.
	_ = runStop(args)
	return runStart(args)
}

// ── runUpgrade ──

// runUpgrade updates the aiclibridge binary and restarts the daemon.
//
// v0.3 implements the `go install` strategy: it runs
// `go install github.com/tgcz2011/aiclibridge/cmd/aiclibridge@latest`,
// which requires Go to be installed and the module to be go-installable
// (public, or with GOPRIVATE/GOFLAGS configured for a private module).
// This is the simplest path and matches how a Go developer already
// upgrades CLI tools.
//
// Binary self-update (download the latest release asset for the current
// GOOS/GOARCH and atomically replace os.Args[0]) is a TODO for a future
// release — it would let users upgrade on hosts without Go installed.
//
// The flow: stop any running daemon (so the old binary's file can be
// replaced on hosts where open files are unlink-safe), run go install,
// print the new version, then start the daemon again with the original
// args.
func runUpgrade(args []string) int {
	goPath, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "aiclibridge upgrade: 'go' not found on PATH; binary self-update not yet implemented (requires Go installed)")
		return 1
	}

	// Stop the daemon if running so the old binary can be replaced.
	// Ignore errors — "not running" is fine.
	_ = runStop([]string{})

	fmt.Println("upgrading aiclibridge via go install...")
	cmd := exec.Command(goPath, "install", "github.com/tgcz2011/aiclibridge/cmd/aiclibridge@latest")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade failed: %v\n", err)
		return 1
	}

	// Show the new version. Prefer the freshly installed binary on PATH
	// (go install writes to GOBIN/GOPATH/bin); fall back to os.Args[0]
	// if it's not on PATH (e.g. run via a relative path).
	newBin, err := exec.LookPath("aiclibridge")
	if err != nil || newBin == "" {
		newBin = os.Args[0]
	}
	out, _ := exec.Command(newBin, "version").CombinedOutput()
	fmt.Printf("upgraded: %s", out)

	// Re-start the daemon with the original args (e.g. --config).
	return runStart(args)
}
