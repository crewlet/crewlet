//go:build unix

package cliagent

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
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

// A CALL WITH NO ENVIRONMENT IS NOT STARTED.
//
// os/exec reads a nil Cmd.Env as "inherit the parent's", so a caller that
// forgot to build one would not run a CLI with nothing — it would run it with
// the engine's keyring, its Tier A tokens and every provider key, which is
// what the version probe once did. The refusal has to come before the fork,
// so the proof is that the child never ran at all.
//
// WITH A GRACE, so the environment is the one thing missing: [run] refuses a
// call with no termination grace too, and a case missing both would pass on
// that refusal with the environment check deleted.
func TestACallWithNoEnvironmentIsNotStarted(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := run(t.Context(), invocation{
		binary:  "/bin/sh",
		args:    []string{"-c", `: > "$0"`, marker},
		timeout: 10 * time.Second,
		grace:   witnessGrace,
	})
	if err == nil {
		t.Fatal("a call with no environment of its own was run")
	}
	if !strings.Contains(err.Error(), "no environment") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the child ran before the refusal, holding the engine's environment")
	}
}

// fakeLeaveHolder is the fake CLI's leave-a-helper mode: it starts a
// descendant that holds ONE of its streams — "stdout", "stderr" or "stdin" —
// and returns, so the CLI goes on to answer and exit 0 with the stream still
// open behind it.
//
// The shape a launcher has when it exits before a helper it forked with that
// stream inherited, and the one thing that tells the streams apart: a helper
// holding all three, as [fakeStubborn]'s grandchild does, could not show that
// a held stderr leaves the answer standing. The others go to the null device.
// It stays in the CLI's group, as a runtime's helper does, and announces its
// pid ([fakeGrandchild]) so a case can end it — or see it ended.
//
// The CLI WAITS FOR THAT ANNOUNCEMENT before it answers. Exiting at once would
// let the call's grace run out while the holder is still starting, and a
// holder reaped before it wrote its pid is one no case can account for: it
// would read as a holder that never ran.
func fakeLeaveHolder(stream string) {
	holder := exec.Command(os.Args[0], "-test.run=TestCLIAgentFakeCLI")
	holder.Env = append(os.Environ(), "FAKE_HOLD_STREAM=", "FAKE_GRANDCHILD=1")
	switch stream {
	case "stdout":
		holder.Stdout = os.Stdout
	case "stderr":
		holder.Stderr = os.Stderr
	case "stdin":
		holder.Stdin = os.Stdin
	default:
		fmt.Fprintf(os.Stderr, "fake CLI: no stream %q to leave held\n", stream)
		os.Exit(1)
	}
	if err := holder.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fake CLI could not fork: %v\n", err)
		os.Exit(1)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if raw, err := os.ReadFile(os.Getenv("FAKE_PIDFILE")); err == nil && len(raw) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "fake CLI: the holder never announced itself")
	os.Exit(1)
}

// fakeExitZeroOnTerm is a CLI that ends CLEANLY when it is asked to stop: on
// SIGTERM it exits 0 having printed nothing, as a Node CLI with a shutdown
// handler does. Until then it holds.
func fakeExitZeroOnTerm() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	go func() {
		<-stop
		os.Exit(0)
	}()
	procgrouptest.Hold()
}

// holderOf leaves nothing behind once a case that made a holder ends: the
// holder outlives the call that made it — that is what it is for — and holds
// for ten minutes unless something ends it.
func holderOf(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() { reapTree(awaitPidFile(t, pidFile)) })
}

// A STDOUT A HELPER STILL HELD IS NOT AN ANSWER, AND NOT A FATAL EITHER.
//
// The CLI printed and exited 0, but something it started kept the stream open
// past the grace, so what was read is cut wherever the grace ran out: a
// launcher whose runtime was still writing the answer looks exactly like
// this. No reading of it can be shown to be whole, so it is refused, as a
// clipped stdout is, and as a SERVER failure, retryable and not sat out —
// nothing about it says the engine could not make the call. It used to come
// back from run as os/exec's ErrWaitDelay, which Complete read as a CLI that
// could not start: fatal, the seat's chain stopped at the head.
//
// Mutation: report a held stdout as run's error again, or stop refusing it in
// completion, and the kind is wrong or the answer comes back.
func TestAStdoutAHelperHeldIsAServerFailure(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdout", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
	}, nil)
	p.termGrace = witnessGrace
	holderOf(t, pidFile)

	comp, err := ask(t, p, llm.Request{})
	if err == nil {
		t.Fatalf("a stdout a helper held was read as the answer %q", comp.Content)
	}
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Fatalf("kind = %v, want server (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "held its stdout open") {
		t.Errorf("err = %v, want it to say the stdout was held", err)
	}
	if err := p.SittingOut(); err != nil {
		t.Errorf("a held stdout sat the entry out: %v", err)
	}
}

// A STDERR A HELPER STILL HELD COSTS ONLY DIAGNOSIS: the answer stands.
//
// The stdout reached its end when the CLI exited, so the answer on it is
// whole; what the grace cut was the CLI's own commentary. Refusing it would
// throw away a completion the operator paid for over a stream nothing reads
// from a call that succeeded.
//
// Mutation: treat a held stderr as a held stdout, and the answer is refused.
func TestAStderrAHelperHeldLeavesTheAnswerStanding(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stderr", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
	}, nil)
	p.termGrace = witnessGrace
	holderOf(t, pidFile)

	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("a call whose stderr a helper held failed: %v", err)
	}
	if comp.Content != "the answer" {
		t.Errorf("Content = %q, want the answer", comp.Content)
	}
}

// THE CHAIN CARRIES THE SEAT PAST A HEAD WHOSE STDOUT A HELPER HELD. When the
// held stream was a fatal, the walk stopped there, and every call of the seat
// failed against a CLI that had exited 0.
//
// Mutation: report a held stdout as run's error again, and the backup is never
// called.
func TestAChainWalksPastAHeadWhoseStdoutAHelperHeld(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	head := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdout", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "from the head",
	}, nil)
	head.termGrace = witnessGrace
	holderOf(t, pidFile)
	backup := fakeProvider(t, map[string]string{"FAKE_STDOUT": "from the backup"}, nil)
	var seen []chain.Fallback
	walk, err := chain.New([]chain.Member{{Key: "plan", Provider: head}, {Key: "key", Provider: backup}},
		chain.Options{OnFallback: func(f chain.Fallback) { seen = append(seen, f) }})
	if err != nil {
		t.Fatalf("chain.New: %v", err)
	}
	comp, err := walk.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("the chain stopped at the head: %v", err)
	}
	if comp.Content != "from the backup" || comp.ProviderKey != "key" {
		t.Errorf("answered %q from %q, want the backup's answer", comp.Content, comp.ProviderKey)
	}
	if len(seen) != 1 || seen[0].From != "plan" || seen[0].To != "key" || seen[0].Kind != llm.KindServer {
		t.Errorf("fallbacks = %+v, want one from plan to key, of kind server", seen)
	}
}

// A WAIT THAT FAILED IS A SERVER FAILURE. A helper holding the CLI's stdin
// with the prompt unread past the grace — more of it than a pipe buffers —
// leaves os/exec's own copy of the prompt blocked, and Wait reports
// ErrWaitDelay for a CLI that exited 0: a run that did not finish as one,
// whose answer was given to part of a prompt at most.
//
// Mutation: report a wait error as run's error again, and the kind is fatal.
func TestAPromptAHelperHeldUnreadIsAServerFailure(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdin", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
	}, nil)
	p.termGrace = witnessGrace
	holderOf(t, pidFile)

	_, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("a prompt longer than a pipe holds ", 1<<15)},
	}})
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Fatalf("kind = %v, want server (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "waiting for it to finish failed") {
		t.Errorf("err = %v, want it to say the wait failed", err)
	}
}

// A CLI THAT EXITS 0 WHEN ITS DEADLINE ARRIVES TIMED OUT. Its SIGTERM handler
// ends it cleanly, so Wait reports the deadline rather than an exit status —
// and run handed that back as its error, which Complete read as a CLI that
// could not start: fatal, a call that had simply run out of time stopping the
// seat's chain instead of moving to the next member.
//
// Mutation: report a wait error as run's error again, and the kind is fatal.
func TestACLIThatEndsCleanlyAtItsDeadlineTimedOut(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_EXIT_0_ON_TERM": "1"}, nil)
	p.termGrace = witnessGrace
	// Long enough for the fake to have its handler in before the deadline;
	// one that arrives first kills it, which reads as a timeout as well.
	p.timeout = 2 * time.Second

	_, err := ask(t, p, llm.Request{})
	if got := llm.KindOf(err); got != llm.KindTimeout {
		t.Fatalf("kind = %v, want timeout (err %v)", got, err)
	}
}

// AN AUXILIARY COMMAND WHOSE STDOUT A HELPER HELD IS NOT READ. What it printed
// may be a login document with its end missing, and an export read as whole
// would carry that out over the shared login. It fails as the SERVER failure
// an exit is — not as a refused login, since the CLI refused nothing — and
// repeats none of what it printed.
//
// Mutation: accept a held stdout as a finished command, and the document comes
// back.
func TestAnAuxiliaryCommandWhoseStdoutAHelperHeldIsNotRead(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	document := exampleDocument(t)
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdout", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": document,
	}, nil)
	p.termGrace = witnessGrace
	holderOf(t, pidFile)

	out, err := p.runAuxiliary(t.Context(), []string{"-test.run=TestCLIAgentFakeCLI"},
		t.TempDir(), p.probeEnv(), "")
	if err == nil {
		t.Fatalf("an export whose stdout a helper held was read (%d bytes)", len(out))
	}
	var unfinished *auxiliaryUnfinishedError
	if !errors.As(err, &unfinished) {
		t.Errorf("err is %T, want an auxiliaryUnfinishedError: %v", err, err)
	}
	if kind := acquireFailureKind(err); kind != llm.KindServer {
		t.Errorf("kind = %v, want server", kind)
	}
	assertNoSecret(t, err, document)
}

// NO VERSION IS READ OFF A PROBE WHOSE STDOUT A HELPER HELD: the first line of
// a cut stream is not known to be a whole version.
//
// Mutation: read the probe's stdout whether or not it was held, and the
// version comes back.
func TestTheVersionProbeReadsNoHeldStdout(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdout", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "fake-cli 1.2.3",
	}, map[string]any{"version_args": []any{"-test.run=TestCLIAgentFakeCLI"}})
	p.termGrace = witnessGrace
	holderOf(t, pidFile)

	if got := p.probeVersion(t.Context()); got != "" {
		t.Errorf("version = %q, want none from a stdout a helper held", got)
	}
}

// A HELPER THAT HELD THE CALL'S PIPES PAST THE GRACE IS ENDED WITH THE CALL.
//
// Closing a pipe under it ends nothing: the helper is attached to a call that
// is over, and a Node runtime ignores the SIGPIPE its next write raises, so it
// held the seat's memory and sockets for as long as it liked — the leak the
// group kill on a timeout exists to prevent, reached by a CLI that exited 0.
// Every stream a helper can hold is a case, stdin among them: os/exec reports
// that one as a failed wait rather than as a held stream.
//
// Mutation: drop the reap after a held stream, and each holder outlives the
// call.
func TestAHelperThatHeldTheCallsPipesIsReapedWithIt(t *testing.T) {
	t.Parallel()
	for _, stream := range []string{"stdout", "stderr", "stdin"} {
		t.Run(stream, func(t *testing.T) {
			t.Parallel()
			pidFile := filepath.Join(t.TempDir(), "holder.pid")
			p := fakeProvider(t, map[string]string{
				"FAKE_HOLD_STREAM": stream, "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
			}, nil)
			p.termGrace = witnessGrace
			holderOf(t, pidFile)

			// More prompt than a pipe holds, so a holder of stdin is one
			// that kept os/exec's copy of it waiting.
			_, _ = ask(t, p, llm.Request{Messages: []llm.Message{
				{Role: llm.RoleUser, Content: strings.Repeat("a prompt longer than a pipe holds ", 1<<15)},
			}})
			holder := awaitPidFile(t, pidFile)
			if !procgrouptest.AwaitGone(t, holder, 10*time.Second) {
				t.Fatalf("the helper %d that held the call's %s outlived the call", holder, stream)
			}
		})
	}
}
