//go:build linux || darwin

package procgroup

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startLeader starts cmd as its own group leader and kills the group when the
// test ends, so a failure never leaks a sleeper.
func startLeader(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %q: %v", script, err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// The identity of a live group is current, and stops being current once the
// group is gone. The baseline the recycled-pid case below is measured against.
func TestAnIdentifiedGroupIsCurrentUntilItIsGone(t *testing.T) {
	cmd := startLeader(t, "sleep 300")
	leader, err := Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if current, err := leader.Current(); err != nil || !current {
		t.Fatalf("Current of a live group = %v, %v; want true", current, err)
	}
	if err := Kill(leader.PID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	if current, err := leader.Current(); err != nil || current {
		t.Fatalf("Current of a killed and reaped group = %v, %v; want false", current, err)
	}
}

// awaitZombie waits for pid, a process this test holds unreaped, to have
// exited, and returns its record. Nothing here hopes for a reaper to be slow:
// the caller is the only thing that can reap pid, so once it has exited it
// stays a zombie until the caller says otherwise, and the wait is only for the
// signal to land.
func awaitZombie(t *testing.T, pid int) Process {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		proc, found, err := Inspect(pid)
		if err != nil || !found {
			t.Fatalf("Inspect of the unreaped pid %d = %+v, found %v, %v; want its record", pid, proc, found, err)
		}
		if proc.Zombie {
			return proc
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never exited", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertZombieOnlyGroupIsNotCurrent is the check both zombie-member cases
// make: the signal-0 probe still finds the group (so the case is really the
// one a probe gets wrong), and Current does not.
func assertZombieOnlyGroupIsNotCurrent(t *testing.T, leader Leader, zombie int) {
	t.Helper()
	if proc := awaitZombie(t, zombie); proc.Group != leader.PID {
		t.Fatalf("the zombie %d is in group %d, want %d: the case under test was not built", zombie, proc.Group, leader.PID)
	}
	if !Exists(leader.PID) {
		t.Fatalf("Exists(%d) = false with a zombie member held: the probe no longer sees corpses, "+
			"so this test no longer reproduces the case it guards", leader.PID)
	}
	if current, err := leader.Current(); err != nil || current {
		t.Fatalf("Current of a group whose only member is a zombie = %v, %v; want false: a finished "+
			"job would keep its box and read as running for as long as nobody reaps the corpse", current, err)
	}
}

// A group whose leader has been reaped and whose last member has exited is
// over, although that member has not been reaped yet. This is the shape the
// Linux CI runner produced by accident, where a killed grandchild is an orphan
// nothing reaps at once; here the member is the test's own child, held
// unreaped on purpose, so the case is built on every platform rather than
// depending on who the kernel reparents an orphan to.
func TestAGroupWhoseOnlyMemberIsAZombieIsNotCurrent(t *testing.T) {
	leaderCmd := startLeader(t, "sleep 300")
	leader, err := Identify(leaderCmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	member := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 300")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.PID}
	if err := member.Start(); err != nil {
		t.Fatalf("starting a member of group %d: %v", leader.PID, err)
	}
	t.Cleanup(func() {
		_ = member.Process.Kill()
		_ = member.Wait()
	})
	if current, err := leader.Current(); err != nil || !current {
		t.Fatalf("Current of a live group = %v, %v; want true", current, err)
	}

	if err := Kill(leader.PID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = leaderCmd.Wait()
	assertZombieOnlyGroupIsNotCurrent(t, leader, member.Process.Pid)

	_ = member.Wait()
	if Exists(leader.PID) {
		t.Fatal("the group still exists after its last member was reaped")
	}
	if current, err := leader.Current(); err != nil || current {
		t.Fatalf("Current of a reaped group = %v, %v; want false", current, err)
	}
}

// A leader that has exited unreaped, with nothing else in its group, is the
// same case with the corpse in the leader's place: its record still carries the
// recorded start time, so the identity matches, and there is still no job.
func TestAGroupOfOnlyAnUnreapedLeaderIsNotCurrent(t *testing.T) {
	cmd := startLeader(t, "exit 0")
	leader, err := Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if proc := awaitZombie(t, leader.PID); proc.Start != leader.Start {
		t.Fatalf("the zombie leader's start time %q is not the one identified, %q", proc.Start, leader.Start)
	}
	assertZombieOnlyGroupIsNotCurrent(t, leader, leader.PID)
}

// A zombie leader does not end a group a living member still runs in: the
// liveness rule counts members, not the leader.
func TestAGroupWithAZombieLeaderAndALivingMemberIsCurrent(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
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
	if _, err := strconv.Atoi(strings.TrimSpace(string(buf[:n]))); err != nil {
		t.Fatalf("reading the member pid from %q: %v", buf[:n], err)
	}
	awaitZombie(t, leader.PID)
	if current, err := leader.Current(); err != nil || !current {
		t.Fatalf("Current of a group with a zombie leader and a living member = %v, %v; want true", current, err)
	}
}

// THE PROPERTY THE TYPE EXISTS FOR. A recorded pid that now leads somebody
// else's group is not this group, however alive that group is. A live group
// with a start time that is not its own is exactly what a recycled pid looks
// like from the outside.
func TestALiveGroupWithAnotherStartTimeIsNotCurrent(t *testing.T) {
	cmd := startLeader(t, "sleep 300")
	own, err := Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	recycled := Leader{PID: own.PID, Start: own.Start + "0"}
	if current, err := recycled.Current(); err != nil || current {
		t.Fatalf("Current of a live group under another start time = %v, %v; want false: "+
			"a recycled pid would keep a dead job's state alive and take its signals", current, err)
	}
	// And the group was only probed, never touched.
	if current, _ := own.Current(); !current {
		t.Fatal("probing the recycled identity disturbed the live group")
	}
}

// A group whose leader has exited while a member runs on is still the group it
// was recorded for: the kernel does not reissue a pid a live group carries. A
// launcher that forks its worker and exits is the ordinary shape of a coding
// job, and reading its group as gone would leave the worker unsignalled.
func TestAGroupOutlivingItsLeaderIsStillCurrent(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	Set(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Read before Wait: an unreaped leader keeps its record, and its pid
	// cannot be anybody else's.
	leader, err := Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := stdout.Read(buf)
	member, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("reading the member pid from %q: %v", buf[:n], err)
	}
	t.Cleanup(func() { _ = syscall.Kill(member, syscall.SIGKILL) })
	_ = cmd.Wait()

	// The kernel guarantee the answer below rests on, checked rather than
	// assumed: the reaped leader's pid is not reissued while its group lives.
	if proc, found, err := Inspect(leader.PID); err != nil || found {
		t.Fatalf("Inspect of the reaped leader's pid = %+v, found %v, %v: the pid was reissued "+
			"while a live group still carries it", proc, found, err)
	}
	if current, err := leader.Current(); err != nil || !current {
		t.Fatalf("Current of a group whose leader exited while a member runs = %v, %v; want true", current, err)
	}
	if err := Kill(leader.PID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if current, _ := leader.Current(); !current {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the group is still current after its last member was killed")
}

// An exited, unreaped process is a zombie: it still has its record and its
// start time, and it runs nothing. Reaping it removes the record.
func TestInspectSeesAZombieAndThenNothing(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	before, err := Identify(pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var proc Process
	for time.Now().Before(deadline) {
		var found bool
		proc, found, err = Inspect(pid)
		if err != nil || !found {
			t.Fatalf("Inspect of an unreaped child = %v, %v, %v; want its record", proc, found, err)
		}
		if proc.Zombie {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !proc.Zombie {
		t.Fatal("an exited, unreaped child was never reported as a zombie")
	}
	if proc.Start != before.Start {
		t.Fatalf("the start time moved when the process exited: %q then %q", before.Start, proc.Start)
	}
	_ = cmd.Wait()
	if _, found, err := Inspect(pid); err != nil || found {
		t.Fatalf("Inspect after the reap = found %v, %v; want no such process", found, err)
	}
}

func TestIdentifyRefusesWhatIsNotAGroupThisPackageAddresses(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if _, err := Identify(pid); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("Identify(%d) = %v, want ErrNoIdentity", pid, err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := Identify(cmd.Process.Pid); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("Identify of a reaped child = %v, want ErrNoIdentity", err)
	}
}

// A recorded leader is often read back from somewhere another program can
// write. Only the exact two-field form is an identity; a bare pid is the very
// shape this type replaces.
func TestALeaderRoundTripsAndNothingElseParses(t *testing.T) {
	want := Leader{PID: 4242, Start: "boot-id:1234"}
	got, err := ParseLeader(want.String())
	if err != nil || got != want {
		t.Fatalf("ParseLeader(%q) = %+v, %v; want %+v", want.String(), got, err, want)
	}
	for _, bad := range []string{
		"", "4242", "4242 ", "1 boot:1", "0 boot:1", "-7 boot:1",
		"pid boot:1", "4242 boot:1 extra",
	} {
		if leader, err := ParseLeader(bad); err == nil {
			t.Errorf("ParseLeader(%q) = %+v, want a refusal", bad, leader)
		}
	}
	for _, leader := range []Leader{{PID: 4242}, {PID: 1, Start: "boot:1"}, {PID: 4242, Start: "a b"}} {
		if current, err := leader.Current(); err != nil || current {
			t.Errorf("Current of the malformed %+v = %v, %v; want false", leader, current, err)
		}
	}
}
