package jetstream

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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
// CHILD: the test binary re-executes itself, the child starts a broker,
// signals itself, and has to still be alive afterwards to say so.
const (
	signalProbeEnv  = "CREWLET_JETSTREAM_SIGNAL_PROBE"
	signalProbeMark = "PROBE-SURVIVED-SIGTERM"

	// Distinctive, and specifically not 0 or 1: nats-server's handler
	// exits 0 and an ordinary test failure exits 1, so an exit code that
	// could be either would not tell them apart.
	signalProbeExit = 7

	// How long the child stays alive after the signal before reporting.
	// It stands in for a drain, which is the thing being protected — the
	// bug is not that the process exits, it is that it exits EARLY, so a
	// probe that reported instantly would race the very handler it is
	// testing for.
	signalProbeDrain = 500 * time.Millisecond
)

func TestMain(m *testing.M) {
	if os.Getenv(signalProbeEnv) == "1" {
		runSignalProbe()
	}
	os.Exit(m.Run())
}

// runSignalProbe is the child. It never returns.
func runSignalProbe() {
	server, err := StartServer(Config{ServerName: "signal-probe"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: start:", err)
		os.Exit(2)
	}

	ours := make(chan os.Signal, 1)
	signal.Notify(ours, syscall.SIGTERM)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "probe: kill:", err)
		os.Exit(3)
	}

	select {
	case <-ours:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "probe: no signal arrived")
		os.Exit(4)
	}

	// The window a hijacked signal would have used.
	time.Sleep(signalProbeDrain)

	server.Shutdown()
	fmt.Println(signalProbeMark)
	os.Exit(signalProbeExit)
}

func TestTheEmbeddedBrokerDoesNotHijackTheProcessSignals(t *testing.T) {
	// Not parallel: it forks a process that signals itself.
	cmd := exec.CommandContext(t.Context(),
		os.Args[0], "-test.run=TestTheEmbeddedBrokerDoesNotHijackTheProcessSignals")
	cmd.Env = append(os.Environ(), signalProbeEnv+"=1")
	out, err := cmd.CombinedOutput()

	code := 0
	if err != nil {
		// errors.As, not a bare assertion: the probe's own exit code is
		// the assertion below, and a wrapped ExitError read as "could
		// not run the probe" would report a passing child as a broken
		// harness.
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running the probe: %v (output %s)", err, out)
		}
		code = exit.ExitCode()
	}

	if !strings.Contains(string(out), signalProbeMark) {
		t.Errorf("the child did not survive its own SIGTERM (exit %d):\n%s", code, out)
	}
	if code != signalProbeExit {
		t.Errorf("child exit = %d, want %d: something other than the child's own "+
			"code ended the process — nats-server's handler exits 0",
			code, signalProbeExit)
	}
}
