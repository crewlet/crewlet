package procgroup

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The exact shape the Linux CI runner failed on, built on purpose. A shell
// leads the group and its sleep is a GRANDCHILD of the test, so killing the
// group and reaping the shell leaves the sleep an orphan whose corpse belongs
// to whatever the kernel reparents it to. On the runner that was an init that
// did not reap at once; here the test makes ITSELF the child subreaper, so the
// corpse is the test's and stays a zombie until the test reaps it. The case no
// longer depends on how quickly a reaper outside the test runs.
func TestAGroupOfAnOrphanedZombieIsNotCurrent(t *testing.T) {
	// Restored in a cleanup registered first, so it runs last: every other
	// test in this binary expects an orphan to leave, not to become a zombie
	// of the test process.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatalf("becoming the child subreaper: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
			t.Errorf("ceasing to be the child subreaper: %v", err)
		}
	})

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!; wait")
	Set(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	leader, err := Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := stdout.Read(buf)
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("reading the grandchild pid from %q: %v", buf[:n], err)
	}
	// Reaped whatever happens below, so the subreaper never leaves a corpse
	// behind. The kill reaches it only while it still runs.
	t.Cleanup(func() {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(grandchild, &status, 0, nil)
	})
	if current, err := leader.Current(); err != nil || !current {
		t.Fatalf("Current of a live group = %v, %v; want true", current, err)
	}

	if err := Kill(leader.PID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	if _, found, err := Inspect(leader.PID); err != nil || found {
		t.Fatalf("Inspect of the reaped shell = found %v, %v; want no such process", found, err)
	}
	assertZombieOnlyGroupIsNotCurrent(t, leader, grandchild)

	var status syscall.WaitStatus
	if _, err := syscall.Wait4(grandchild, &status, 0, nil); err != nil {
		t.Fatalf("reaping the orphaned grandchild %d: %v", grandchild, err)
	}
	if Exists(leader.PID) {
		t.Fatal("the group still exists after its orphaned member was reaped")
	}
}
