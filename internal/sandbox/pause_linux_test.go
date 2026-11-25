package sandbox

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// A TREE RUNS WHILE ANY THREAD OF IT DOES, as [groupThreads] reads it: a member
// whose main thread alone has exited, its stat line reading Z, runs on, so a
// tree whose leader is stopped is not; once a stop reaches the member too,
// each thread of it stopped or exited, the tree is. Linux alone, where each
// thread is read.
//
// Mutation: read each process off its own stat line, its main thread's state,
// and the member reads over, its tree stopped while it runs.
func TestATreeRunsWhileAnyThreadOfItDoes(t *testing.T) {
	t.Parallel()
	leader := exec.CommandContext(t.Context(), "sleep", "300")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("starting the tree's leader: %v", err)
	}
	pgid := leader.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = leader.Wait()
	})
	if err := syscall.Kill(pgid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stopping the tree's leader: %v", err)
	}
	awaitStopped(t, pgid, "the tree's leader never stopped")
	member := procgrouptest.StartExitedMainThread(t, &syscall.SysProcAttr{Setpgid: true, Pgid: pgid})
	if group := groupThreads(t, pgid); stoppedTree(group, pgid) {
		t.Fatalf("a tree whose member, pid %d, runs reads stopped: its threads read %v", member, group)
	}
	if err := syscall.Kill(member, syscall.SIGSTOP); err != nil {
		t.Fatalf("stopping the member: %v", err)
	}
	awaitStopped(t, pgid, "a member stopped beside its exited main thread never read stopped")
}
