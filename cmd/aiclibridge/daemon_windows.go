//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows process creation flags used by forkChild. These are the
// numeric values of CREATE_NEW_PROCESS_GROUP (0x00000200) and
// DETACHED_PROCESS (0x00000008) from the Windows API; they are written
// as literals so the syscall.SysProcAttr.CreationFlags field can be set
// without importing a separate constants package just for two flags.
const (
	windowsCreateNewProcessGroup = 0x00000200
	windowsDetachedProcess       = 0x00000008
)

// forkChild re-execs the binary as a detached daemon child on Windows.
// CREATE_NEW_PROCESS_GROUP lets the parent later send a CTRL+BREAK to the
// child's process group (best-effort graceful shutdown); DETACHED_PROCESS
// detaches the child from the parent's console so closing the launching
// terminal does not cascade-kill it. HideWindow suppresses any console
// window the child might pop up. stdout/stderr are wired to logFile so
// the child's output is captured after the parent exits.
//
// This is the Windows analogue of the Unix Setsid fork: it approximates
// "new session, no controlling tty" with "new process group, no console".
func forkChild(cmdArgs []string, logFile *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windowsCreateNewProcessGroup | windowsDetachedProcess,
		HideWindow:    true,
	}
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// processSignal sends sig to pid on Windows. Windows has no SIGTERM/
// SIGKILL distinction; the only graceful option for a process group
// started with CREATE_NEW_PROCESS_GROUP is CTRL+BREAK, which the child's
// Go runtime maps to os.Interrupt — if the child installed a signal
// handler (serveStack does), it can shut down gracefully. SIGKILL falls
// through to os.FindProcess + Kill (TerminateProcess), which is
// ungraceful but always works. Other signals are unsupported on Windows.
//
// The daemon verbs are documented as best-effort on Windows: runStop
// sends SIGTERM (CTRL+BREAK) first, polls isRunning, and falls back to
// SIGKILL (TerminateProcess) if the child does not exit in time.
func processSignal(pid int, sig syscall.Signal) error {
	switch sig {
	case syscall.SIGTERM:
		// Best-effort graceful: send CTRL+BREAK to the child's process
		// group. Errors are ignored; runStop polls isRunning and falls
		// back to SIGKILL (Kill) if the child does not exit in time.
		_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
		return nil
	case syscall.SIGKILL:
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Kill()
	default:
		return fmt.Errorf("unsupported signal %v on windows", sig)
	}
}

// isRunning reports whether a process with the given PID is alive. Windows
// has no signal-0 probe; we OpenProcess with a query-only access mask —
// a failure means the process is gone (or we lack permission, which we
// treat as not-ours, matching the Unix EPERM behaviour).
func isRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(handle)
	return true
}
