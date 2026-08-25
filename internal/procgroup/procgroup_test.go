//go:build unix

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

// The property this package exists for: signalling the process Go started
// must reach what that process forked. A CLI or an MCP server is nearly
// always a launcher over a runtime that forks helpers, and killing only the
// launcher leaves those holding memory, sockets and the inherited stderr.
//
// The shape: sh starts a long sleep in the background, prints its pid, and
// exits. The sleep's parent is therefore gone — the only thing that can still
// reach it is the process GROUP.
func TestSignallingTheGroupReachesAnOrphanedGrandchild(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	Set(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("starting the fake launcher: %v", err)
	}
	// cmd.Output waits, so the launcher itself has exited by now and the
	// grandchild is orphaned.
	pgid := cmd.Process.Pid
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("reading the grandchild pid from %q: %v", out, err)
	}
	if !alive(grandchild) {
		t.Fatalf("the grandchild %d was not running, so the test proves nothing", grandchild)
	}

	if err := Kill(pgid); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if waitGone(grandchild) {
		return
	}
	// Clean up whatever survived, so a failure does not leak a sleeper.
	_ = syscall.Kill(grandchild, syscall.SIGKILL)
	t.Fatalf("the orphaned grandchild %d survived the group kill", grandchild)
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

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
