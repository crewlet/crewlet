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

// exists probes the group without touching it.
//
// EPERM counts as alive: the group is there and belongs to somebody else,
// which under a recycled pid is exactly the case a caller checking identity
// has to handle. Reporting it as dead would be the more dangerous lie.
func exists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signal delivers sig to the whole group led by pid.
func signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
