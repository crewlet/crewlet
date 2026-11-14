package chromiumtest

import (
	"os/exec"
	"syscall"
)

// tie has the kernel kill a launched Chromium the moment the test binary that
// started it dies, however it dies, before its pipe's end would (pipes): the
// parent-death signal, SIGKILL. The kernel sends it when the THREAD that
// started the browser ends, which in a Go program is the process's end: the
// runtime ends a thread only under a goroutine that locked itself to it and
// exited still locked, which nothing in a binary that drives a browser does.
func tie(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
