package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/seat"
)

// declaredDeath is a broker's death the test declares.
type declaredDeath struct {
	done  chan struct{}
	cause error
}

func (d *declaredDeath) Done() <-chan struct{} { return d.done }

func (d *declaredDeath) Err() error {
	select {
	case <-d.done:
		return d.cause
	default:
		return nil
	}
}

const (
	// brokerDeathChildEnv makes this test binary the CHILD of
	// [TestABrokerThatDeclaresItselfDeadEndsTheNode]: a node whose broker
	// declares itself dead, run in a process of its own because the exit
	// under test ends the process it runs in. Its value is the case.
	brokerDeathChildEnv = "CREWLET_ENGINE_TEST_BROKER_DEATH"
	// brokerDeathDirEnv is the directory the child keeps its stores in. The
	// parent's, because the child ends by os.Exit, which runs no cleanup.
	brokerDeathDirEnv = "CREWLET_ENGINE_TEST_BROKER_DEATH_DIR"

	// The two cases a child runs.
	childDeclares     = "declares"
	childDeclaresLate = "declares-after-stop"

	// brokerDeathReason is the reason the child's broker declares.
	brokerDeathReason = "nats-server: Can't start JetStream: disk gone"
)

// A NODE WHOSE EMBEDDED BROKER DECLARES ITSELF DEAD ENDS, through the
// engine's own exit — and one whose teardown has begun does not.
//
// # The failure this pins
//
// nats-server's own logger exits the process on Fatalf; the engine's does not,
// because the engine owns the process. So after the boot a Fatalf was an
// Error line and nothing else, and the node went on answering /health on a
// broker that had said it was dead — holding its seats' leases, running turns
// whose results could never be acknowledged. The node now ends through the
// seat watchdog's exit.
//
// # Why in a process of its own
//
// That exit is the property: os.Exit with the declared-death code, the
// broker's reason on stderr, after an embedded_broker_died line through the
// log. A case that stood the exit in with a recorder certified the recorder
// and nothing that runs in production — replacing the exit with nothing left
// it green. So this test binary runs itself again as a node: real backends,
// a death the case declares, and the engine's own watch and watchdog, and the
// parent reads what the process left behind — its exit status and its stderr.
//
// # And what it must not do
//
// End a node for its own shutdown. The teardown closes the broker this engine
// owns, or hands back one it was lent — a declaration after it is about a
// broker that is no longer this node's to serve on, and that child returns.
func TestABrokerThatDeclaresItselfDeadEndsTheNode(t *testing.T) {
	if mode := os.Getenv(brokerDeathChildEnv); mode != "" {
		runBrokerDeathChild(t, mode, os.Getenv(brokerDeathDirEnv))
		return
	}
	t.Parallel()

	t.Run("a declaration ends the process", func(t *testing.T) {
		t.Parallel()
		code, stderr := runBrokerDeathParent(t, childDeclares)
		if code != seat.FailExitCode {
			t.Fatalf("a node whose broker declared itself dead exited %d, want "+
				"%d (the declared-death code); stderr:\n%s", code,
				seat.FailExitCode, stderr)
		}
		logged := strings.Index(stderr, "embedded_broker_died")
		notice := strings.Index(stderr, "crewlet: embedded broker declared it cannot go on: "+
			brokerDeathReason)
		if notice < 0 {
			t.Errorf("the exit notice on stderr does not carry the broker's own "+
				"reason; stderr:\n%s", stderr)
		}
		if !strings.Contains(stderr, "(code "+strconv.Itoa(seat.FailExitCode)+")") {
			t.Errorf("the exit notice does not name its code; stderr:\n%s", stderr)
		}
		if logged < 0 || (notice >= 0 && logged > notice) {
			t.Errorf("embedded_broker_died was not logged before the exit, so a "+
				"log file would not hold the reason; stderr:\n%s", stderr)
		}
	})

	t.Run("a teardown stands the watch down", func(t *testing.T) {
		t.Parallel()
		code, stderr := runBrokerDeathParent(t, childDeclaresLate)
		if code != 0 {
			t.Fatalf("a node that had already stopped was ended for its broker "+
				"(exit %d); stderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "declared it cannot go on") {
			t.Errorf("a node that had already stopped announced its end; "+
				"stderr:\n%s", stderr)
		}
	})
}

// runBrokerDeathParent runs this test binary as the child for mode and answers
// its exit status and its stderr.
func runBrokerDeathParent(t *testing.T, mode string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestABrokerThatDeclaresItselfDeadEndsTheNode$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		brokerDeathChildEnv+"="+mode, brokerDeathDirEnv+"="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the child for %q did not end; stdout:\n%s\nstderr:\n%s",
			mode, stdout.String(), stderr.String())
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String() + "\n--- stdout ---\n" + stdout.String()
	default:
		t.Fatalf("run the child for %q: %v", mode, err)
		return 0, ""
	}
}

// runBrokerDeathChild is the node the parent watches: a real engine on real
// backends, whose broker's death the case declares.
func runBrokerDeathChild(t *testing.T, mode, dir string) {
	if dir == "" {
		t.Fatalf("%s is set and %s is not", brokerDeathChildEnv, brokerDeathDirEnv)
	}
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	// THE EMBEDDED BROKER'S OWN DEATH IS WHAT THE ENGINE WATCHES, so the
	// backends that start one carry it — the wiring nothing else holds.
	if back.death == nil {
		t.Fatal("the backends of an embedded broker carry no way to learn it " +
			"has declared itself dead")
	}
	// LENT, with a death this case declares: nothing outside the jetstream
	// package can make a real nats-server call its Fatalf, and that half —
	// Fatalf to Death — is certified there.
	death := &declaredDeath{done: make(chan struct{}), cause: errors.New(brokerDeathReason)}
	lent := *back
	lent.death = death
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: &lent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	switch mode {
	case childDeclares:
		defer e.Stop(context.Background())
		close(death.done)
		// The process ends inside this wait. Reaching its end is the
		// failure, and the child exits 0 for the parent to read as one.
		time.Sleep(30 * time.Second)
		t.Log("the child outlived its broker's death")
	case childDeclaresLate:
		e.Stop(context.Background())
		close(death.done)
		time.Sleep(2 * time.Second)
	default:
		t.Fatalf("no such child: %q", mode)
	}
}
