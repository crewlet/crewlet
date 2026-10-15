package procgroup

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

// A GROUP'S CPU IS WHAT ITS LEADER'S REAP REPORTS, read from the other side:
// a shell runs a loop in a subshell it waits for — so the loop's time is the
// shell's to hand on — and then sleeps as the group's only member. What
// GroupCPU reads from the kernel's records then is what wait4 reports when
// the shell is killed and reaped, to within the four clock ticks a record
// rounds its four times to: so the ticks are read from the right fields and
// turned into time at the kernel's own rate, where a rate assumed wrong by
// any factor puts the two figures that factor apart.
func TestAGroupsCPUIsWhatItsLeadersReapReports(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c",
		`(i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done); echo spent; exec sleep 300`)
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
	line := make([]byte, 16)
	if n, err := stdout.Read(line); err != nil || strings.TrimSpace(string(line[:n])) != "spent" {
		t.Fatalf("waiting for the loop to end: %q, %v", line[:n], err)
	}
	read, err := GroupCPU(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("GroupCPU: %v", err)
	}
	if err := Kill(cmd.Process.Pid); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	reaped := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	t.Logf("GroupCPU read %v; the reap reported %v", read, reaped)
	if reaped < 100*time.Millisecond {
		t.Fatalf("the loop spent %v, too little for a rate wrong by any factor to show", reaped)
	}
	if diff := read - reaped; diff < -50*time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("GroupCPU read %v and the reap reported %v: more than the four ticks a record rounds to apart",
			read, reaped)
	}
}

// A GROUP KILLED WHOLE LEAVES ITS LEADER NONE OF ITS CHILDREN'S TIME — the
// reason GroupCPU exists. A shell starts a loop that runs until it is killed
// and waits for it; the group is read while the loop runs and then killed as
// one, and the shell dies without reaping the loop, so its reap reports the
// shell alone. GroupCPU, read before the signal, counted the loop.
func TestAGroupKilledWholeLeavesItsLeaderNoneOfItsChildrensTime(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `while :; do :; done & wait`)
	Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	var read time.Duration
	for deadline := time.Now().Add(30 * time.Second); read < 200*time.Millisecond; {
		if time.Now().After(deadline) {
			t.Fatalf("the group's loop had spent %v after 30 s", read)
		}
		time.Sleep(20 * time.Millisecond)
		var err error
		if read, err = GroupCPU(cmd.Process.Pid); err != nil {
			t.Fatalf("GroupCPU: %v", err)
		}
	}
	if err := Kill(cmd.Process.Pid); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	reaped := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	t.Logf("GroupCPU read %v before the kill; the leader's reap reported %v", read, reaped)
	if reaped > read/4 {
		t.Fatalf("the leader's reap reported %v of the %v its group had spent: the premise — that a leader "+
			"killed with its group does not reap the rest — no longer holds here", reaped, read)
	}
}
