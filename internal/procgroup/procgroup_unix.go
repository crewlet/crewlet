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
