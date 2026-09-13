//go:build unix

package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
)

// The two signals this package sends, named here so the portable file does
// not have to import syscall.
const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
	sigStop = syscall.SIGSTOP
	sigCont = syscall.SIGCONT
)

// set makes the child a group leader. A zero Pgid with Setpgid means "your
// own group", so the group id equals the child's pid and the caller needs no
// second lookup to address it.
func set(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// detach makes the child a SESSION leader, which implies its own process
// group. Setsid and Setpgid are mutually exclusive in SysProcAttr — the
// kernel refuses both — so this sets only the one that subsumes the other.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// addressable reports whether kill(2) reads -pid as ONE process group.
//
// Two values do not, and both are refused rather than signalled. Zero and
// below would address the caller's own group, which is every process in the
// engine's session. One is the sharper case: kill(-1, sig) is the BROADCAST
// form, delivered to every process the caller has permission to signal, so a
// SIGKILL "to the group led by pid 1" takes down the engine, every coding job
// and, on a workstation, the operator's whole login. No child this package
// addresses can be pid 1: init holds it for the life of the pid namespace,
// the engine's own included when it runs as a container's first process.
func addressable(pid int) bool { return pid > 1 }

// exists probes the group without touching it.
//
// EPERM counts as alive: the group is there and belongs to somebody else,
// which under a recycled pid is exactly the case a caller checking identity
// has to handle. Reporting it as dead would be the more dangerous lie.
func exists(pid int) bool {
	if !addressable(pid) {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signal delivers sig to the whole group led by pid.
func signal(pid int, sig syscall.Signal) error {
	if !addressable(pid) {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
