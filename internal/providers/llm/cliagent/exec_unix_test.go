//go:build unix

package cliagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// fakeStubborn is the fake CLI's SIGTERM-survivor mode.
//
// Unix only, because the survivor is built out of the two things that only
// exist here: a signal it can decline, and a process group that reaches past
// it. It re-executes this binary as a grandchild in the SAME group — the shape
// a coding CLI has in production, where the binary is a launcher and the
// runtime doing the work is its child.
func fakeStubborn() {
	signal.Ignore(syscall.SIGTERM)

	child := exec.Command(os.Args[0], "-test.run=TestCLIAgentFakeCLI")
	child.Env = append(os.Environ(), "FAKE_STUBBORN=", "FAKE_GRANDCHILD=1")
	// Deliberately NOT procgroup.Set: the grandchild stays in its parent's
	// group, which is the only reason a group signal can reach it and the
	// reason a per-process kill cannot.
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fake CLI could not fork: %v\n", err)
		os.Exit(1)
	}
	// Never returns. os/exec's WaitDelay kill reaches this process, and this
	// process alone.
	select {}
}

// fakeGrandchild is the descendant the reap has to reach.
//
// It IGNORES SIGTERM as well, which is what makes this test able to fail: the
// group is signalled politely by cmd.Cancel on every ending, timeout and
// cancellation alike, so a grandchild that merely dies of SIGTERM proves
// nothing about the SIGKILL that follows. Only a survivor distinguishes the
// path that reaps from the path that does not.
func fakeGrandchild() {
	signal.Ignore(syscall.SIGTERM)
	// The pid goes to a FILE rather than to stderr: the engine captures a
	// child's stderr into a capped buffer it returns only once the call
	// completes, and the call this witnesses is cancelled — so nothing ever
	// reads it.
	_ = os.WriteFile(os.Getenv("FAKE_PIDFILE"), fmt.Appendf(nil, "%d", os.Getpid()), 0o600)
	select {}
}

// A CANCELLED CALL REAPS THE WHOLE TREE, not just the process it started.
//
// The reap used to run only on the DEADLINE path. cmd.Cancel and WaitDelay
// fire identically on cancellation, so both leave the same survivor — but only
// one of them was followed by a group kill. On the one path where nothing else
// comes back for it, shutdown, a forking Node or Bun subtree holding this
// seat's workspace and sockets outlived the engine.
//
// The grandchild is the witness: os/exec's WaitDelay kill reaches the
// immediate process, so a run that ends without the group kill leaves it
// running with its parent already gone.
func TestCancellingACallReapsTheWholeTree(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_STUBBORN": "1", "FAKE_PIDFILE": pidFile,
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Complete(ctx, llm.Request{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		})
	}()

	grandchild := awaitPidFile(t, pidFile)
	t.Cleanup(func() { reapTree(grandchild) })

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Complete never returned after the caller cancelled")
	}

	if !waitGoneUnix(grandchild) {
		t.Fatalf("grandchild %d survived the cancelled call: the reap runs on "+
			"the deadline path only, so a shutdown leaves a runtime holding "+
			"this seat's workspace and sockets", grandchild)
	}
}

// awaitPidFile waits for the fake CLI's grandchild to announce itself.
func awaitPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake CLI never announced its grandchild")
	return 0
}

// reapTree leaves nothing behind on the machine, however the case ended.
//
// Through the GROUP, resolved from the grandchild rather than assumed to be
// its own pid: it is deliberately not a group leader — that is the whole point
// of the case — so procgroup.Kill(grandchild) would name a group that does not
// exist and silently strand both processes. The group reaches the stubborn
// parent too, which the failing path also leaves alive.
func reapTree(grandchild int) {
	if pgid, err := syscall.Getpgid(grandchild); err == nil {
		_ = procgroup.Kill(pgid)
	}
	_ = syscall.Kill(grandchild, syscall.SIGKILL)
}

func aliveUnix(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGoneUnix(pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !aliveUnix(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
