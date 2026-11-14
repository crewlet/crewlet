//go:build unix

package procgroup

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// The property this package exists for: signalling the process Go started
// must reach what that process forked. A harness or an MCP server is nearly
// always a launcher over a runtime that forks helpers, and killing only the
// launcher leaves those holding memory, sockets and the inherited stderr.
//
// The shape: sh starts cat in the background and exits. cat's parent is
// therefore gone — the only thing that can still reach it is the process
// GROUP. cat reads a pipe whose write end this case holds open, and writes to
// a pipe whose write end nothing else holds, so it cannot end on its own while
// the case runs: only a signal ends it, and once the case is over its input's
// end of file does.
//
// GONE IS READ OFF ITS OUTPUT, never off its pid. The kernel closes every
// descriptor a process holds as it exits, before anything reaps it, so the
// end of file on cat's output is the moment it has gone, zombie or not.
// kill(pid, 0) answers yes for a zombie until its adopter reaps it, and an
// orphan's adopter is whatever the machine runs as its first process: in a
// container whose first process reaps nothing, never (#1080). A build whose
// Kill misses cat leaves the read below blocked, and the test binary's own
// timeout ends it, naming this case.
func TestSignallingTheGroupReachesAnOrphanedGrandchild(t *testing.T) {
	input, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Closed last, as the case ends: a cat this case failed to kill reads end
	// of file on its input and exits, so a failure leaves no process behind.
	t.Cleanup(func() { _ = feed.Close() })
	output, witness, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "cat <&3 >&4 2>/dev/null 3<&- 4>&- &")
	cmd.ExtraFiles = []*os.File{input, witness}
	Set(cmd)
	err = cmd.Run()
	// cmd.Run waits, so the launcher itself has exited by now and cat is
	// orphaned. Its ends of the two pipes are cat's alone once this process
	// lets go of its own copies: the end of file below waits on cat and
	// nothing else.
	_ = input.Close()
	_ = witness.Close()
	if err != nil {
		t.Fatalf("starting the fake launcher: %v", err)
	}
	const ping = "ping\n"
	if _, err := feed.Write([]byte(ping)); err != nil {
		t.Fatalf("writing to the grandchild: %v", err)
	}
	echo := make([]byte, len(ping))
	if n, err := io.ReadFull(output, echo); err != nil || string(echo) != ping {
		t.Fatalf("the grandchild echoed %q (%v), not %q, so it was not running and the test proves nothing",
			echo[:n], err, ping)
	}

	if err := Kill(cmd.Process.Pid); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	rest, err := io.ReadAll(output)
	if err != nil {
		t.Fatalf("reading the grandchild's output to its end: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the grandchild wrote %q after its echo, which nothing asked it to", rest)
	}
}

// Signalling a group that is already gone is the outcome the caller wanted,
// not an error to report — every teardown path would otherwise log one.
func TestSignallingAnEmptyGroupIsNotAnError(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	Set(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := Kill(cmd.Process.Pid); err != nil {
		t.Errorf("Kill on a reaped group: %v", err)
	}
	if err := Terminate(cmd.Process.Pid); err != nil {
		t.Errorf("Terminate on a reaped group: %v", err)
	}
}

// A zero or negative pid would be a signal to the CALLER's own group — every
// process in this test binary's session — so it must be refused outright.
func TestAZeroPidIsRefusedRatherThanSignallingOurselves(t *testing.T) {
	if err := Kill(0); err != nil {
		t.Errorf("Kill(0): %v", err)
	}
	if err := Terminate(-1); err != nil {
		t.Errorf("Terminate(-1): %v", err)
	}
}

// Pid 1 names no group this package could have made, and -1 is kill(2)'s
// broadcast address: a signal "to the group led by pid 1" reaches every process
// the engine user may signal, the engine and this test binary among them.
//
// Asserted through Exists, whose probe is the signal-0 form of the very call a
// Kill would make, so this can fail without a broken guard ever delivering a
// real signal: kill(-1, 0) succeeds for any caller that can signal itself,
// which reads as a live group led by init.
func TestPidOneIsRefusedBecauseKillReadsItAsEveryProcess(t *testing.T) {
	if Exists(1) {
		t.Fatal("Exists(1) = true: the probe went out as kill(-1, 0), the broadcast form, " +
			"so a Kill(1) would have sent SIGKILL to every process this user owns")
	}
}

// Terminate must send a catchable termination rather than an uncatchable
// kill, or a tree gets no chance to flush its work and close its sockets.
// The child's own exit status is the unambiguous witness: SIGTERM, not
// SIGKILL.
func TestTerminateSendsACatchableSignal(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 30")
	Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := Terminate(cmd.Process.Pid); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Wait = %v, want a signalled exit", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("no wait status on %v", exit)
	}
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("the child died of %v, want SIGTERM — Terminate must be catchable", status.Signal())
	}
}
