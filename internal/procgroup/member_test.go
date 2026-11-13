//go:build linux || darwin

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// startTrapper starts a shell in process group pgid — a group of its own where
// pgid is zero — that writes "heard" into path when it is sent SIGUSR1, and
// exits; sent SIGUSR2, it writes path's ".witness" instead ([unsignalled]). It
// says it is ready, in path's ".ready", once its traps are set, which this
// waits for: a SIGUSR1 sent before that would end it by the signal's default
// instead, and the case would be measuring the shell's start.
func startTrapper(t *testing.T, path string, pgid int) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c",
		`trap 'echo heard > "$1"; exit 0' USR1; trap ': > "$1.witness"' USR2; : > "$1.ready"; `+
			`while :; do sleep 0.05; done`, "sh", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a shell that traps SIGUSR1: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if pgid == 0 {
		// ITS OWN GROUP, whose sleeps go with it.
		t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	}
	awaitFile(t, path+".ready", "the shell to set its trap")
	return cmd
}

// unsignalled fails the case, saying what, if the trapper cmd
// ([startTrapper]), whose heard file is path, was sent SIGUSR1 before now. It
// sends the trapper SIGUSR2 and waits for what that leaves: its witness, or,
// where a SIGUSR1 came first, the heard file that trap wrote as it ended the
// shell. Of two signals pending, the kernel delivers the lower-numbered first,
// and the shell runs the traps of the signals it caught in the same order, so
// a SIGUSR1 sent before the witness's signal has written path by the time the
// witness could be written.
//
// NOT A QUIET WINDOW, of any length: a trap that has not run yet is no
// evidence that nothing was sent, and a loaded runner delays a trap past any
// window, so a window only sees a signal that is quick to show.
func unsignalled(t *testing.T, cmd *exec.Cmd, path, what string) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("sending the trapper its witness signal: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			t.Fatal(what)
		}
		if _, err := os.Stat(path + ".witness"); err == nil {
			return
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("waited 5s for the trapper to write %s.witness", path)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// awaitFile waits for path to exist, failing the case after five seconds.
func awaitFile(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("waited 5s for %s", what)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// A SIGNAL REACHES ONLY A LIVING MEMBER OF THE RECORDED GROUP. A run's script
// tells its engine which pid to ask to end by writing it in its box, and a
// direct box's job runs as the engine's own user beside that file — so the pid
// is the job's word, and the group the engine recorded for the job when it
// started it is what holds the word to the job. A member is signalled; a
// process outside the group, a member that has exited, pid 1, and a member of
// a group that is no longer the one recorded are refused, and none of them is
// signalled.
//
// Mutation: drop the group check, and the outsider's trap writes its file;
// drop the zombie check, and the exited member is "signalled" with no error;
// drop the check that the group is still the recorded one, and the stale
// identity reaches the member.
func TestASignalReachesOnlyALivingMemberOfTheGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaderCmd := startLeader(t, "sleep", "300")
	leader, err := Identify(leaderCmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	member := startTrapper(t, filepath.Join(dir, "member"), leader.PID)
	outsider := startTrapper(t, filepath.Join(dir, "outsider"), 0)

	// A GROUP THAT IS NO LONGER THE ONE RECORDED: the same pid under another
	// start time, which is what a recycled group id looks like.
	stale := Leader{PID: leader.PID, Start: leader.Start + "0"}
	if err := stale.SignalMember(member.Process.Pid, syscall.SIGUSR1); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a member of a group no longer the recorded one was answered %v, want ErrNotMember", err)
	}

	// AN EXITED MEMBER, held unreaped so it keeps its record and its group.
	zombie := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.PID}
	if err := zombie.Start(); err != nil {
		t.Fatalf("starting a member that exits: %v", err)
	}
	t.Cleanup(func() { _ = zombie.Wait() })
	awaitZombie(t, zombie.Process.Pid)
	if err := leader.SignalMember(zombie.Process.Pid, syscall.SIGUSR1); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a member that has exited was answered %v, want ErrNotMember", err)
	}

	for _, pid := range []int{1, 0, -leader.PID} {
		if err := leader.SignalMember(pid, syscall.SIGUSR1); !errors.Is(err, ErrNotMember) {
			t.Fatalf("pid %d was answered %v, want ErrNotMember", pid, err)
		}
	}
	if err := leader.SignalMember(outsider.Process.Pid, syscall.SIGUSR1); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a process outside the group was answered %v, want ErrNotMember", err)
	}

	if err := leader.SignalMember(member.Process.Pid, syscall.SIGUSR1); err != nil {
		t.Fatalf("SignalMember of a living member: %v", err)
	}
	awaitFile(t, filepath.Join(dir, "member"), "the member to hear its signal")

	// THE OUTSIDER HEARD NOTHING, as its witness tells: the witness's signal
	// goes after every request the case made.
	unsignalled(t, outsider, filepath.Join(dir, "outsider"), "the process outside the group was signalled")
}
