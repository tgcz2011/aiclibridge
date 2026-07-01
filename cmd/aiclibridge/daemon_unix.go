//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// forkChild re-execs the binary as a detached daemon child: a new session
// (Setsid) detaches from the controlling tty so closing the launching
// terminal leaves the child running. stdin is nil (no input); stdout and
// stderr are wired to logFile so the child's output is captured after the
// parent exits. Returns the started cmd (whose Process.Pid is the child).
func forkChild(cmdArgs []string, logFile *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// processSignal sends sig to pid. Used by runStop for SIGTERM (graceful)
// and SIGKILL (forced). A signal of 0 is a liveness probe (no signal
// delivered); ESRCH means the process is gone.
func processSignal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// isRunning reports whether a process with the given PID is alive. signal
// 0 delivers no signal but performs the existence/permission check: nil
// means the process exists and we can signal it; ESRCH means it's gone;
// EPERM means it exists but is owned by another user (still "running"
// from the daemon's perspective, but we treat EPERM as not-ours and
// return false so stop/start don't wedge on a foreign process).
func isRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if err == syscall.EPERM {
		// Process exists but is not ours; treat as foreign/not-running
		// so we don't try to manage a process we can't control.
		return false
	}
	return false
}
