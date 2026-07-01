// Package main hosts the unit tests for aiclibridge's daemon helpers.
//
// daemon_test.go covers the pure functions only — pidFilePath,
// logFilePath, readPID, writePID, removePID, isRunning, daemonAddr —
// plus the "already running" short-circuit in runStart (exercised by
// pre-writing a pid file with the test process's own PID and asserting
// runStart returns 1 without forking). The end-to-end fork path
// (runStart with a real re-exec, runStop killing a live daemon,
// runUpgrade shelling out to `go install`) is intentionally NOT
// exercised here: those paths spawn background processes or hit the
// network, which belong in an integration suite rather than
// `go test ./...`. Every test runs without network access and uses only
// t.TempDir for filesystem fixtures.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tgcz2011/aiclibridge/internal/config"
)

// ── readPID / writePID round trip ──

// TestPIDFileRoundTrip verifies that writePID then readPID returns the
// same PID. This is the contract runStart/runStop rely on: the child
// writes its PID and the parent (or a later stop) reads it back. The
// atomic write-then-rename must leave a file readPID can parse.
func TestPIDFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.pid")

	want := 12345
	if err := writePID(path, want); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	got, err := readPID(path)
	if err != nil {
		t.Fatalf("readPID: %v", err)
	}
	if got != want {
		t.Errorf("readPID after writePID: got %d, want %d", got, want)
	}
}

// TestWritePIDOverwrites verifies that a second writePID replaces the
// first value rather than appending or failing. runStart overwrites a
// stale pid file from a crashed daemon, so the rename must clobber.
func TestWritePIDOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.pid")

	if err := writePID(path, 111); err != nil {
		t.Fatalf("writePID(111): %v", err)
	}
	if err := writePID(path, 222); err != nil {
		t.Fatalf("writePID(222): %v", err)
	}
	got, err := readPID(path)
	if err != nil {
		t.Fatalf("readPID: %v", err)
	}
	if got != 222 {
		t.Errorf("readPID after overwrite: got %d, want 222", got)
	}
}

// ── readPID missing / empty ──

// TestReadPIDMissing verifies that a missing pid file returns 0, nil —
// the "daemon is not running" signal runStart/runStop check before
// forking or signalling. A missing file must NOT be an error because
// the daemon's first start has no pid file yet.
func TestReadPIDMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.pid")

	pid, err := readPID(path)
	if err != nil {
		t.Errorf("readPID(missing): err = %v, want nil", err)
	}
	if pid != 0 {
		t.Errorf("readPID(missing): pid = %d, want 0", pid)
	}
}

// TestReadPIDEmpty verifies that an empty pid file is treated the same
// as a missing one (0, nil). A truncated or zero-length file can appear
// if a previous write was interrupted; readPID must not choke on it.
func TestReadPIDEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.pid")
	if err := os.WriteFile(path, []byte("   \n  "), 0o644); err != nil {
		t.Fatalf("write empty pid file: %v", err)
	}
	pid, err := readPID(path)
	if err != nil {
		t.Errorf("readPID(empty): err = %v, want nil", err)
	}
	if pid != 0 {
		t.Errorf("readPID(empty): pid = %d, want 0", pid)
	}
}

// ── removePID ──

// TestRemovePID verifies that removePID deletes the pid file so a
// subsequent readPID returns 0. This is the cleanup contract
// runDaemonForeground's deferred removePID and runStop's final remove
// rely on: after stop, the next start sees no pid file.
func TestRemovePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.pid")

	if err := writePID(path, 4242); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	removePID(path)

	pid, err := readPID(path)
	if err != nil {
		t.Errorf("readPID after remove: err = %v, want nil", err)
	}
	if pid != 0 {
		t.Errorf("readPID after remove: pid = %d, want 0", pid)
	}
}

// TestRemovePIDMissing verifies that removePID on a non-existent path
// does not error — it is best-effort, and runStop calls it
// unconditionally at the end even when the daemon was already gone.
func TestRemovePIDMissing(t *testing.T) {
	dir := t.TempDir()
	removePID(filepath.Join(dir, "nope.pid")) // must not panic/error
}

// ── isRunning ──

// TestIsRunning verifies the liveness check against three cases: the
// current process (alive), a reaped child process (dead), and
// non-positive PIDs (always dead). The dead-PID case spawns a child
// that exits immediately and is reaped by cmd.Run, so its PID is
// guaranteed free at check time (modulo an unrealistic immediate reuse
// race).
func TestIsRunning(t *testing.T) {
	// Self is alive.
	if !isRunning(os.Getpid()) {
		t.Errorf("isRunning(self=%d) = false, want true", os.Getpid())
	}

	// A reaped child is dead. Use `go version` (guaranteed on PATH
	// during `go test`) rather than os.Args[0] — re-invoking the test
	// binary would re-run the test harness, not our main(), and hang.
	// cmd.Run waits for exit + reaps, so the PID is free afterwards.
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go not on PATH; skipping dead-PID assertion: %v", err)
	}
	cmd := exec.Command(goPath, "version")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	deadPID := cmd.ProcessState.Pid()
	if isRunning(deadPID) {
		t.Errorf("isRunning(dead pid %d) = true, want false", deadPID)
	}

	// Non-positive PIDs are never running.
	if isRunning(0) {
		t.Errorf("isRunning(0) = true, want false")
	}
	if isRunning(-1) {
		t.Errorf("isRunning(-1) = true, want false")
	}
}

// ── daemonAddr ──

// TestDaemonAddr verifies that daemonAddr returns cfg.Listen verbatim.
// runStart uses this for the "daemon started on <addr>" banner, so the
// displayed port must match what the server actually binds.
func TestDaemonAddr(t *testing.T) {
	cfg := &config.Config{Listen: "127.0.0.1:8787"}
	if got := daemonAddr(cfg); got != "127.0.0.1:8787" {
		t.Errorf("daemonAddr = %q, want 127.0.0.1:8787", got)
	}

	cfg.Listen = "0.0.0.0:9999"
	if got := daemonAddr(cfg); got != "0.0.0.0:9999" {
		t.Errorf("daemonAddr = %q, want 0.0.0.0:9999", got)
	}
}

// TestPidAndLogFilePath verifies the path helpers compose the data dir
// with the canonical filenames. runStart/runStop agree on the pid file
// location via pidFilePath, and the parent's log redirect + the child's
// log rotation both target logFilePath, so the two must stay in sync.
func TestPidAndLogFilePath(t *testing.T) {
	cfg := &config.Config{DataDir: "/var/lib/aiclibridge"}
	if got := pidFilePath(cfg); got != "/var/lib/aiclibridge/aiclibridge.pid" {
		t.Errorf("pidFilePath = %q, want /var/lib/aiclibridge/aiclibridge.pid", got)
	}
	if got := logFilePath(cfg); got != "/var/lib/aiclibridge/aiclibridge.log" {
		t.Errorf("logFilePath = %q, want /var/lib/aiclibridge/aiclibridge.log", got)
	}
}

// ── runStart "already running" short-circuit ──

// TestRunStartAlreadyRunning verifies that runStart refuses to fork
// when a daemon appears to be running (pid file present + process
// alive). The test writes the test process's own PID into the pid file
// under a temp data dir (set via the AICLIBRIDGE_DATA_DIR env override
// that config.applyEnvOverrides reads), then calls runStart with no
// flags. runStart must hit the "already running" branch and return 1
// WITHOUT forking — so the test never spawns a real daemon.
//
// stderr is redirected to /dev/null to keep the test output clean; the
// test only asserts the exit code, not the message text.
func TestRunStartAlreadyRunning(t *testing.T) {
	dir := t.TempDir()

	// Point the data dir at the temp dir via env override. This is
	// applied by config.applyEnvOverrides inside loadConfig, so runStart
	// (which calls loadConfig) picks it up. Restore on cleanup.
	origDataDir := os.Getenv("AICLIBRIDGE_DATA_DIR")
	if err := os.Setenv("AICLIBRIDGE_DATA_DIR", dir); err != nil {
		t.Fatalf("setenv AICLIBRIDGE_DATA_DIR: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("AICLIBRIDGE_DATA_DIR", origDataDir)
	})

	// Pre-write the pid file with our own PID so isRunning sees a live
	// process. The path must match pidFilePath(cfg), which is
	// <DataDir>/aiclibridge.pid.
	pidFile := filepath.Join(dir, "aiclibridge.pid")
	if err := writePID(pidFile, os.Getpid()); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	// Silence the "already running" stderr line so it doesn't clutter
	// test output. os.Stderr is a *os.File; redirect to /dev/null.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	origStderr := os.Stderr
	os.Stderr = devNull
	t.Cleanup(func() {
		os.Stderr = origStderr
		devNull.Close()
	})

	code := runStart([]string{})
	if code != 1 {
		t.Errorf("runStart with live pid file: code = %d, want 1", code)
	}
}

// ── runStop "not running" ──

// TestRunStopNotRunning verifies that runStop returns 1 when no pid file
// exists (daemon never started or already stopped). Like the
// already-running test it uses the AICLIBRIDGE_DATA_DIR env override to
// point at a temp dir with no pid file, so runStop hits the "no pid
// file" branch and returns 1 without signalling anything.
func TestRunStopNotRunning(t *testing.T) {
	dir := t.TempDir()

	origDataDir := os.Getenv("AICLIBRIDGE_DATA_DIR")
	if err := os.Setenv("AICLIBRIDGE_DATA_DIR", dir); err != nil {
		t.Fatalf("setenv AICLIBRIDGE_DATA_DIR: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("AICLIBRIDGE_DATA_DIR", origDataDir)
	})

	// Silence the "not running" stderr line.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	origStderr := os.Stderr
	os.Stderr = devNull
	t.Cleanup(func() {
		os.Stderr = origStderr
		devNull.Close()
	})

	code := runStop([]string{})
	if code != 1 {
		t.Errorf("runStop with no pid file: code = %d, want 1", code)
	}
}
