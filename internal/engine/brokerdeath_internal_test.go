package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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

// A NODE WHOSE EMBEDDED BROKER DECLARES ITSELF DEAD ENDS — and one whose
// teardown has begun does not.
//
// # The failure this pins
//
// nats-server's own logger exits the process on Fatalf; the engine's does not,
// because the engine owns the process. So after the boot a Fatalf was an
// Error line and nothing else, and the node went on answering /health on a
// broker that had said it was dead — holding its seats' leases, running turns
// whose results could never be acknowledged. The node now ends through the
// seat watchdog's exit; this case stands the exit in with a recorder, since
// the real one ends the test binary.
//
// # And what it must not do
//
// End a node for its own shutdown. The teardown closes the broker this engine
// owns, or hands back one it was lent — a declaration after it is about a
// broker that is no longer this node's to serve on.
func TestABrokerThatDeclaresItselfDeadEndsTheNode(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
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

	boot := func(death *declaredDeath) (*Engine, chan error) {
		lent := *back
		lent.death = death
		ended := make(chan error, 2)
		e, err := New(t.Context(), Options{
			Bootstrap: &b, Company: cfg, Backends: &lent,
			onBrokerDeath: func(cause error) { ended <- cause },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return e, ended
	}

	t.Run("a declaration ends the node", func(t *testing.T) {
		death := &declaredDeath{done: make(chan struct{}),
			cause: errors.New("nats-server: Can't start JetStream: disk gone")}
		e, ended := boot(death)
		defer e.Stop(context.Background())
		select {
		case cause := <-ended:
			t.Fatalf("the node was ended before its broker declared anything: %v", cause)
		default:
		}

		close(death.done)
		select {
		case cause := <-ended:
			if !errors.Is(cause, death.cause) {
				t.Errorf("the node was ended for %v, not for the broker's own "+
					"reason", cause)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a broker declared itself dead and the node went on serving on it")
		}
	})

	t.Run("a teardown stands the watch down", func(t *testing.T) {
		death := &declaredDeath{done: make(chan struct{}),
			cause: errors.New("nats-server: after the stop")}
		e, ended := boot(death)
		e.Stop(context.Background())

		close(death.done)
		select {
		case cause := <-ended:
			t.Fatalf("a node that had already stopped was ended for its "+
				"broker: %v", cause)
		case <-time.After(time.Second):
		}
	})
}
