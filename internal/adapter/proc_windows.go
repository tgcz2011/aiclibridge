//go:build windows

package adapter

import (
	"os/exec"
	"strconv"
	"syscall"
)

// createNewConsole allocates a fresh console for the child process. Combined
// with HideWindow=true (STARTF_USESHOWWINDOW + SW_HIDE) the console window
// stays off-screen, and — critically — any grandchildren the agent spawns
// (tool subprocesses like bash, cmd, netstat, findstr) inherit this hidden
// console instead of each allocating their own visible one.
//
// Using CREATE_NO_WINDOW here instead would strip the console entirely,
// which forces Windows to allocate a new visible console per grandchild
// when the grandchild is a console-subsystem program that doesn't itself
// pass CREATE_NO_WINDOW.
const createNewConsole = 0x00000010

// hideAgentWindow configures cmd to suppress the console window on Windows
// while still giving descendant processes a hidden console to inherit.
// Stdio pipes set via cmd.StdoutPipe/StdinPipe keep working because
// STARTF_USESTDHANDLES takes precedence over the new console's stdio.
func hideAgentWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNewConsole
}

// configureProcessGroup is a no-op on Windows: there is no Setpgid/process-group
// signalling. Descendant cleanup relies on the hidden console group set up by
// hideAgentWindow plus exec.CommandContext / WaitDelay terminating the child.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup terminates the process tree on Windows via taskkill /T,
// which best-effort reaps the agent CLI plus the tool subprocesses it
// spawned. Windows has no SIGTERM/SIGKILL distinction or native process-
// group signalling, so the signal argument is ignored and the entire tree
// is asked to terminate in one shot. Errors are intentionally swallowed —
// the caller already has a failure path; tree cleanup is best-effort on
// top of that.
func killProcessGroup(pid int, _ syscall.Signal) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T").Run()
}
