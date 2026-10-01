package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A NODE WHOSE BROKER CONNECTION IS CLOSED FOR GOOD STOPS, AND SAYS WHY.
//
// `crewlet run` itself, as a supervisor sees it: a real process, pointed at an
// operator's NATS server, which then stops accepting it — restarted with its
// credentials rotated, so the node's reconnect is refused, refused again on the
// attempt after it, and the connection closed by the client for good. That
// connection is the node's only one: on an external broker the coordination
// store rides it too.
//
// Nothing noticed that. The process stayed up with nothing to publish,
// consume or renew a lease over until a person restarted it. It must stop now
// by the one shutdown a signal takes — the drain, then the teardown — and
// EXIT NON-ZERO through main, printing the sentence that names the cause and
// the setting to change, so whatever supervises it restarts it and whoever
// reads the log knows what to fix first.
//
// In a CHILD, for the reason the drain probe is: the exit status is main's,
// and main exits the process.
//
// Mutation: wait on the signal context alone in runEngine and the node never
// exits; return nil after the shutdown and the exit status is the probe's own;
// skip the shutdown and the order row goes red.
func TestANodeWhoseBrokerConnectionIsClosedForGoodExitsNamingIt(t *testing.T) {
	// Not parallel, for the drain probe's reason: a whole node in a second
	// process.
	const token = "the-one-this-node-holds"
	srv := externaltest.Start(t, queue.MaxPayloadBytes, func(o *server.Options) {
		o.Authorization = token
	})

	dir := t.TempDir()
	boot := writeFile(t, dir, "crewlet.yaml", fmt.Sprintf(`node:
  id: lost-broker-probe
store:
  path: %s
stream:
  type: nats
  url: %s
  token: %s
coordination:
  type: embedded-kv
secrets:
  active_key_id: lost-broker-probe
  keys:
    - id: lost-broker-probe
      material: %s
api:
  port: 0
`, filepath.Join(dir, "crewlet.db"), srv.URL(), token, testKeyMaterial(t)))

	args, err := json.Marshal([]string{"-config", boot, "-log-format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestANodeWhoseBrokerConnectionIsClosedForGoodExitsNamingIt$")
	// THE TEMPORARY DIRECTORY, so the default -company path names nothing
	// this package's own directory might hold.
	child.Dir = dir
	child.Env = append(os.Environ(),
		lostBrokerProbeEnv+"=1",
		drainProbeArgsEnv+"="+string(args))
	var output lockedBuffer
	child.Stdout, child.Stderr = &output, &output
	death, hold, err := os.Pipe()
	if err != nil {
		t.Fatalf("parent-death pipe: %v", err)
	}
	child.ExtraFiles = []*os.File{death}
	child.WaitDelay = 5 * time.Second
	if err := child.Start(); err != nil {
		_ = death.Close()
		_ = hold.Close()
		t.Fatalf("starting the node: %v", err)
	}
	_ = death.Close()
	t.Cleanup(func() { _ = hold.Close() })
	exited := make(chan struct{})
	var waitErr error
	go func() {
		defer close(exited)
		waitErr = child.Wait()
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-exited
		if t.Failed() {
			t.Logf("the node's output:\n%s", output.String())
		}
	})
	alive := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}

	eventually(t, "the node to start", alive, func() bool {
		return logOrder(output.String(), "engine_started")[0] >= 0
	})

	// THE OPERATOR'S GESTURE: the server's credentials rotated under the
	// running node, which it then no longer admits.
	srv.RestartAs(func(o *server.Options) { o.Authorization = "rotated" })

	select {
	case <-exited:
	case <-time.After(drainProbeBudget):
		t.Fatalf("the node is still running %s after its broker connection was "+
			"closed for good", drainProbeBudget)
	}

	// NON-ZERO, AND main's OWN: 1 is what main exits with on an error, and
	// the probe's code is what a run() that returned nil reaches.
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the node ended with %v, want main to exit 1 on the error run "+
			"returned (the probe exits %d when run returns nil)", waitErr, lostBrokerProbeExit)
	}

	// THE SENTENCE, on main's own line: what closed, why, and what to change.
	var said string
	for line := range strings.SplitSeq(output.String(), "\n") {
		if strings.HasPrefix(line, "crewlet: ") {
			said = line
		}
	}
	if said == "" {
		t.Fatal("the node exited without main's `crewlet: ` line naming why")
	}
	for _, want := range []string{"the node stopped itself", "stream",
		"closed this node's connection for good", "Authorization Violation",
		"stream.token", srv.HostPort()} {
		if !strings.Contains(said, want) {
			t.Errorf("the exit line %q does not say %q", said, want)
		}
	}
	if strings.Contains(said, token) {
		t.Errorf("the exit line %q carries the token the node signs in with", said)
	}
	// AND THE DEPLOYMENT GUIDE PRINTS ITS EXAMPLE AS main DOES: everything
	// main and the engine put in front of the backend's own sentence, which
	// the jetstream suite holds to the guide's example on its own.
	if at := strings.Index(said, "jetstream: "); at < 0 {
		t.Errorf("the exit line %q does not carry the backend's sentence", said)
	} else {
		guide, err := os.ReadFile(filepath.Join(sourcetree.Root(t),
			"docs", "guides", "deployment.md"))
		if err != nil {
			t.Fatalf("read the deployment guide: %v", err)
		}
		if prefix := said[:at+len("jetstream: ")]; !strings.Contains(string(guide), "\n"+prefix) {
			t.Errorf("docs/guides/deployment.md prints no exit line starting %q, "+
				"which is how main writes one", prefix)
		}
	}

	// AND THROUGH THE ORDINARY SHUTDOWN: said, drained, stopped — the order a
	// signal takes, with the fatal line where the signal would be.
	order := logOrder(output.String(), "engine_fatal", "engine_draining",
		"drain_complete", "engine_stopped", "run_failed")
	if !slices.IsSorted(order) || slices.Contains(order, -1) {
		t.Errorf("the shutdown ran out of order or in part: engine_fatal, "+
			"engine_draining, drain_complete, engine_stopped and run_failed "+
			"logged at lines %v", order)
	}
}

const (
	// lostBrokerProbeEnv selects the child of the case above, which runs
	// main with the arguments drainProbeArgsEnv carries.
	lostBrokerProbeEnv = "CREWLET_RUN_LOST_BROKER_PROBE"

	// lostBrokerProbeExit is what the child exits with if main RETURNS,
	// which it does only when run returned nil: distinct from main's own 1
	// and from a test binary's 0, 1 and 2, as drainProbeExit is.
	lostBrokerProbeExit = 8
)

// runLostBrokerProbe is the child. It never returns.
func runLostBrokerProbe() {
	exitWithTheParent()
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(drainProbeArgsEnv)), &args); err != nil {
		fmt.Fprintln(os.Stderr, "probe: args:", err)
		os.Exit(3)
	}
	// MAIN ITSELF, rather than run: the exit status a supervisor reads is
	// main's, and it is half of what the case asserts.
	os.Args = append([]string{"crewlet", "run"}, args...)
	main()
	os.Exit(lostBrokerProbeExit)
}
