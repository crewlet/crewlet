//go:build unix

package cliagent

import (
	"bytes"
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
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
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
	//
	// And it INHERITS the pipes, as a runtime under a launcher does. That is
	// what holds a caller's Wait open once the launcher itself is gone — the
	// hang WaitDelay bounds — and left on os/exec's default of the null
	// device, a grandchild could never witness it.
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fake CLI could not fork: %v\n", err)
		os.Exit(1)
	}
	// Holds until something ends it. os/exec's WaitDelay kill reaches this
	// process, and this process alone.
	procgrouptest.Hold()
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
	procgrouptest.Hold()
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
	p.termGrace = witnessGrace

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
	cancelled := time.Now()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Complete never returned after the caller cancelled")
	}
	assertLetGoAtTheProvidersGrace(t, "a seat's call", time.Since(cancelled))

	if !procgrouptest.AwaitGone(t, grandchild, 10*time.Second) {
		t.Fatalf("grandchild %d survived the cancelled call: the reap runs on "+
			"the deadline path only, so a shutdown leaves a runtime holding "+
			"this seat's workspace and sockets", grandchild)
	}
}

// A CANCELLED CREDENTIAL COMMAND IS LET GO when its grace runs out, even while
// a helper the CLI forked holds its output open.
//
// The one child here started without its own process group — an interactive
// login has to read the operator's terminal — so nothing signals its
// descendants, and the inherited pipes are all that is left holding Wait.
// Without a WaitDelay an operator's Ctrl+C left `crewlet llm login` hanging on
// an EOF that was never coming.
//
// The stubborn fake and its grandchild are in THIS binary's process group, so
// the cleanup ends the grandchild by its pid alone: [reapTree] would signal
// the suite itself.
func TestACancelledCredentialCommandIsLetGoAfterTheGrace(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	p := loginProvider(t)
	p.profile.StatusArgs = []string{"-test.run=TestCLIAgentFakeCLI"}
	p.env = fakeChildEnv(map[string]string{"FAKE_STUBBORN": "1", "FAKE_PIDFILE": pidFile})
	p.termGrace = witnessGrace

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		// Buffers, not nil: a nil writer is the null device, and only a
		// pipe the grandchild inherited can hold the command open.
		done <- p.Status(ctx, &bytes.Buffer{}, &bytes.Buffer{})
	}()

	grandchild := awaitPidFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	cancel()
	cancelled := time.Now()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled credential command was reported as success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Status never returned after its context was cancelled: nothing " +
			"bounds the wait on a pipe a forked helper holds open")
	}
	assertLetGoAtTheProvidersGrace(t, "a credential command", time.Since(cancelled))
}

// witnessGrace is the termination grace the cases that wait one out run at.
// Any positive figure would do — each case cancels only once its stubborn tree
// is up, its grandchild having announced itself after ignoring SIGTERM — and
// this one is short enough that a kill costs the suite nothing while far
// enough under [termGrace] to tell the two apart.
const witnessGrace = 100 * time.Millisecond

// assertLetGoAtTheProvidersGrace fails unless a call that had to wait out a
// stubborn tree was let go before the PRODUCTION grace could have elapsed.
//
// A child ignoring SIGTERM, or a helper holding the pipes its parent was
// killed with, keeps the call open for exactly the grace in force, so a call
// that took termGrace or longer was not given its provider's — and both sites
// that exec a child read the provider's, which is the only reason the cases
// above can run at [witnessGrace] at all.
func assertLetGoAtTheProvidersGrace(t *testing.T, what string, took time.Duration) {
	t.Helper()
	if took >= termGrace {
		t.Errorf("%s was let go %v after its cancel, at the production %v rather "+
			"than the provider's own %v: the exec path is not reading Provider.termGrace",
			what, took, termGrace, witnessGrace)
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
