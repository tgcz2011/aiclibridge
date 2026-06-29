//go:build !windows

package adapter

import (
	"os/exec"
	"syscall"
)

// hideAgentWindow is a no-op on non-Windows platforms.
func hideAgentWindow(cmd *exec.Cmd) {}

// configureProcessGroup puts the child into its own process group (it becomes
// the group leader, so the group id equals the child pid). This lets the
// daemon signal the entire tree — the agent CLI plus any tool subprocess it
// spawns — in one call, instead of killing only the direct child and leaking
// grandchildren that keep running (and spinning on EPIPE) after a task is
// cancelled or the daemon restarts. See killProcessGroup.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends sig to the whole process group led by pid (when the
// command was started with configureProcessGroup). Targeting the group
// (negative pid) reaches the descendants the agent spawned, not just the
// leader. The error is intentionally swallowed — the caller already has a
// failure path; signalling is best-effort cleanup on top of that.
func killProcessGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
