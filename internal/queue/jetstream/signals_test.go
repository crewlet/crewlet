package jetstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The embedded broker must not touch the process's signals.
//
// nats-server's Server.Start installs its own SIGINT/SIGTERM handler unless
// Options.NoSigs is set, and that handler shuts the broker down and then calls
// os.Exit(0) — from a library, inside a process that is in the middle of
// something. The something, here, is the engine's graceful drain: it hands
// every seat back and waits for in-flight turns, and a turn is the whole
// reason it exists. Measured before the fix: SIGTERM to a solo node ran the
// drain and the process vanished part way through, exit 0, with everything
// after it simply absent.
//
// A signal handler cannot be observed from inside the process that installed
// it — os.Exit takes the test binary with it — so this runs the check in a
// CHILD: the test binary re-executes itself, and the child starts a broker
// and sends itself SIGTERM with no handler of its own.
//
// # Decided by who took the signal, never by how long the child lived
//
// A SIGTERM nobody asked for is the Go runtime's to act on, and it ends the
// process BY that signal, which the parent reads off the exit status. A handler
// anybody installed takes the signal instead — nats-server's shuts the broker
// down and exits 0 — and the status says so however long that shutdown takes.
// The child used to keep a handler of its own and report itself alive half a
// second after the signal, a stand-in for a drain: a broker whose shutdown took
// longer than that exited after the report, and the case passed with
// nats-server's handler installed.
const (
	signalProbeEnv  = "CREWLET_JETSTREAM_SIGNAL_PROBE"
	signalProbeMark = "PROBE-BROKER-STARTED"

	// Distinctive, and specifically not 0 or 1: nats-server's handler
	// exits 0 and an ordinary test failure exits 1, so an exit code that
	// could be either would not tell them apart. The child exits with it
	// only if nothing has ended it by signalProbeBound.
	signalProbeExit = 7

	// signalProbeBound is how long the child waits to be ended by the
	// SIGTERM it sent itself. A bound and no more: the signal untouched ends
	// the child as it is delivered, a handler that took it ends the child
	// however long its shutdown takes, and a child still alive at the bound
	// exits with signalProbeExit — none of them an exit by SIGTERM.
	signalProbeBound = time.Minute
)

func TestMain(m *testing.M) {
	if os.Getenv(signalProbeEnv) == "1" {
		runSignalProbe()
	}
	os.Exit(m.Run())
}

// runSignalProbe is the child. It never returns, and a build that leaves the
// process's signals alone never reaches the end of it either.
func runSignalProbe() {
	server, err := StartServer(context.Background(), Config{ServerName: "signal-probe"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: start:", err)
		os.Exit(2)
	}
	fmt.Println(signalProbeMark)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "probe: kill:", err)
		os.Exit(3)
	}

	<-time.After(signalProbeBound) //fixedwait:liveness
	server.Shutdown()
	fmt.Fprintln(os.Stderr, "probe: still alive", signalProbeBound, "after its own SIGTERM")
	os.Exit(signalProbeExit)
}

func TestTheEmbeddedBrokerDoesNotHijackTheProcessSignals(t *testing.T) {
	// Not parallel: it forks a process that signals itself.
	cmd := exec.CommandContext(t.Context(),
		os.Args[0], "-test.run=TestTheEmbeddedBrokerDoesNotHijackTheProcessSignals")
	// ITS TEMPORARY FILES IN THE CASE'S OWN DIRECTORY: a child the signal
	// ends runs no shutdown, so the scratch store its broker made is left
	// for this case's cleanup rather than in the machine's temp directory.
	cmd.Env = append(os.Environ(), signalProbeEnv+"=1", "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()

	if !strings.Contains(string(out), signalProbeMark) {
		t.Fatalf("the child never got as far as its broker (%v), so it signalled "+
			"nothing:\n%s", err, out)
	}
	if err == nil {
		t.Fatalf("the child exited 0 after the SIGTERM it sent itself: something "+
			"took the signal — nats-server's handler shuts the broker down and "+
			"exits 0:\n%s", out)
	}
	// errors.As, not a bare assertion: a wrapped ExitError read as "could not
	// run the probe" would report the child's own ending as a broken harness.
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("running the probe: %v (output %s)", err, out)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() ||
		status.Signal() != syscall.SIGTERM {
		t.Errorf("the child ended %v, where the SIGTERM it sent itself should have "+
			"ended it: something else took the signal, or nothing ended it:\n%s",
			exit, out)
	}
}
