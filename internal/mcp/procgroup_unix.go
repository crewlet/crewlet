//go:build unix

package mcp

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group.
//
// The point is the TREE, not the child. An MCP server is very often launched
// through a package runner — `uvx mcp-atlassian`, `npx @some/mcp` — which
// forks the real server underneath itself. The SDK's transport shutdown
// signals only the process it started, so the runner's child can survive its
// parent, keep the inherited stderr descriptor open, and go on holding
// whatever the server held. One group makes the whole tree addressable with
// one signal.
//
// Setpgid with a zero Pgid makes the child a group leader, so the group id
// equals its pid.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs every process in the group led by pid.
//
// Callers must have evidence the group is not already gone — see
// client.reapChild. Signalling a group whose leader has been reaped is a
// theoretical way to hit a recycled pid, and "the tree is definitely still
// there" is cheap to establish and the only honest precondition.
//
// ESRCH (nothing in the group) is not an error worth reporting: it is the
// answer we wanted.
func killProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
