package executor_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// THE CASES READ A PROCESS AS INTERNAL/PROCGROUP DOES: one whose main thread
// alone has exited runs on, so a call left so still runs for [runs]; once it
// is over, a zombie nothing has reaped, it runs nothing. Linux alone, where a
// main thread exits by itself.
//
// Mutation: read the process's stat line, its main thread's state, as runs
// did, and the first reading takes the call for ended.
func TestTheCasesReadAProcessAsProcgroupDoes(t *testing.T) {
	t.Parallel()
	pid := procgrouptest.StartExitedMainThread(t, nil)
	if !runs(t, pid) {
		t.Errorf("runs takes a process whose main thread alone has exited, pid %d, for ended", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	if !procgrouptest.AwaitGone(t, pid, time.Minute) {
		t.Fatalf("pid %d runs on after SIGKILL", pid)
	}
	if runs(t, pid) {
		t.Errorf("runs takes a zombie, pid %d, for running", pid)
	}
}
