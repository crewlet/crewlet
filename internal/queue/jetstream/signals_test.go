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

// The embedded broker must not touch the process's signals: nats-server's
// Server.Start, unless Options.NoSigs is set, installs a SIGINT/SIGTERM handler
// that shuts the broker down and calls os.Exit(0), mid-way through the
// engine's graceful drain of seats and in-flight turns.
//
// os.Exit would take the test binary with it, so a CHILD runs the check: the
// binary re-executes itself, starts a broker and sends itself SIGTERM with no
// handler of its own. Decided by who took the signal, never by how long the
// child lived: an untouched SIGTERM ends the process BY that signal, which the
// parent reads off the exit status, and a handler that took it shows there
// however long its shutdown takes.
const (
	signalProbeEnv  = "CREWLET_JETSTREAM_SIGNAL_PROBE"
	signalProbeMark = "PROBE-BROKER-STARTED"

	// Neither 0 (nats-server's handler) nor 1 (an ordinary test
	// failure). The child exits with it only if nothing has ended it by
	// signalProbeBound.
	signalProbeExit = 7

	// signalProbeBound is how long the child waits to be ended by its own
	// SIGTERM. Only a liveness bound: the untouched signal ends the child
	// on delivery, a handler that took it ends it however long its
	// shutdown takes, and a child alive at the bound exits with
	// signalProbeExit — none of them an exit by SIGTERM.
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
