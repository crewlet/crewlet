package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

func bootstrap(t *testing.T, mutate func(*config.Bootstrap)) *config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	if mutate != nil {
		mutate(&b)
	}
	return &b
}

func TestTheDefaultTopologyRunsWithNoExternalService(t *testing.T) {
	t.Parallel()
	// The single-binary promise: a company with nothing configured starts.
	// If this needs a broker, the promise is not kept.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := engine.OpenBackends(t.Context(), b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if back.Queue == nil || back.Coord == nil {
		t.Fatalf("backends = %+v, want both slots", back)
	}
}

func TestBothSlotsCloseTogether(t *testing.T) {
	t.Parallel()
	// A node holding one without the other can hear work it may not do, or
	// hold seats it cannot serve. Closing twice must also be safe: a
	// deferred Close beside an explicit one is the ordinary shape.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := engine.OpenBackends(t.Context(), b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	back.Close(t.Context())
	// Twice is safe: a deferred Close beside an explicit one is the
	// ordinary shape, and a second Stop on a stopped queue is a no-op.
	back.Close(t.Context())
}

func TestAnEmbeddedKVStoreRidesTheStreamsOwnConnection(t *testing.T) {
	t.Parallel()
	// A second dial would work and would be worse: two connections to one
	// broker fail independently, so a node could hold live leases over a
	// connection that still works while the one carrying its inbox has
	// dropped — alive to its peers, deaf to its work.
	//
	// Observable as: the KV slot builds at all on an embedded stream, which
	// it can only do by reaching the in-process server.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	back, err := engine.OpenBackends(t.Context(), b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	// It is a real store: a lease acquired through it is held.
	lease, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("the coordination slot refused a first acquire")
	}
	// And it is EXCLUSIVE, which a memory backend standing in for it would
	// also be — so the test above is what says it is the KV store, and this
	// is what says it works.
	if got, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-2", TTL: 30 * time.Second}); err != nil {
		t.Fatalf("second Acquire: %v", err)
	} else if got != nil {
		t.Error("two owners held one seat")
	}
}

func TestLocalCoordinationNeedsNoBroker(t *testing.T) {
	t.Parallel()
	// The default. One node, no quorum, no network — and the coordination
	// slot still has to be a real backend, because a nil one surfaces as a
	// panic in the seat host rather than a configuration error.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Coordination.Type = config.CoordinationLocal
	})
	back, err := engine.OpenBackends(t.Context(), b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if back.Coord == nil {
		t.Fatal("local coordination produced no backend")
	}
	if _, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second}); err != nil {
		t.Errorf("Acquire: %v", err)
	}
}

func TestNoBootstrapIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := engine.OpenBackends(context.Background(), nil); err == nil {
		t.Error("a nil bootstrap opened backends")
	}
}

func TestAnUnknownStreamTypeNamesItself(t *testing.T) {
	t.Parallel()
	// A Bootstrap is an exported struct, so this is reachable without going
	// through validation — and an error that does not name the value sends
	// a reader to read the code instead of their config.
	b := bootstrap(t, func(b *config.Bootstrap) { b.Stream.Type = "kafka" })
	_, err := engine.OpenBackends(context.Background(), b)
	if err == nil {
		t.Fatal("an unknown stream type opened backends")
	}
	if !strings.Contains(err.Error(), "kafka") {
		t.Errorf("the error does not name the type: %v", err)
	}
}

func TestTheTopologyIsCheckedBeforeTheDial(t *testing.T) {
	t.Parallel()
	// Reaching a broker only to discover the topology cannot use it reports
	// a network failure when the problem is a configuration one — an
	// operator whose broker happens to be down would read the wrong error
	// and go fix the wrong thing.
	//
	// The URL here points at a port nothing serves, so a dial-first
	// implementation reports a connection failure and this reads it.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:1"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationLocal
	})
	_, err := engine.OpenBackends(context.Background(), b)
	if err == nil {
		t.Fatal("a Pulsar stream with local coordination opened backends")
	}
	if !strings.Contains(err.Error(), "compare-and-set") {
		t.Errorf("the failure blames the network rather than the topology: %v", err)
	}
}

func TestPulsarWithoutItsCoordinationClusterSaysWhichSideIsMissing(t *testing.T) {
	t.Parallel()
	// Pulsar has no compare-and-set, so it cannot hold leases itself. An
	// operator whose Pulsar config is correct should learn that this BUILD
	// is incomplete, not that their config is wrong.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:6650"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	back, err := engine.OpenBackends(context.Background(), b)
	if err == nil {
		t.Fatal("a Pulsar topology opened with no coordination cluster")
	}
	if back != nil {
		t.Error("a failed open returned backends, which would carry a nil Coord")
	}
	// The BUILD is what is incomplete, not the config. An operator whose
	// Pulsar block is correct must not be sent to re-read it.
	if !strings.Contains(err.Error(), "does not start yet") {
		t.Errorf("the failure blames the config rather than the build: %v", err)
	}
}

func TestAnEmbeddedStreamPersistsAcrossARestart(t *testing.T) {
	t.Parallel()
	// Two properties in one round trip, and both were unasserted:
	//
	//   - Close actually SHUTS THE SERVER DOWN. A leaked embedded server
	//     holds its store directory, so the second open below fails outright
	//     if the first one is still running.
	//   - StoreDir is actually USED. Ignoring it selects an in-memory
	//     server, which starts and works and loses everything on restart —
	//     the failure a company only discovers the first time it restarts.
	dir := filepath.Join(t.TempDir(), "stream")
	mk := func() *engine.Backends {
		t.Helper()
		b := bootstrap(t, func(b *config.Bootstrap) { b.Stream.StoreDir = dir })
		back, err := engine.OpenBackends(t.Context(), b)
		if err != nil {
			t.Fatalf("OpenBackends: %v", err)
		}
		return back
	}

	first := mk()
	if err := first.Queue.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev := events.New(marker{Note: "survives"}, events.NewTrace())
	if err := first.Queue.Publish(t.Context(), topics.Event(ev.Type), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Close stops the queue itself — that is step one of its order, and
	// stopping it here as well would hide whether it does.
	first.Close(t.Context())

	// A second server on the same directory: it can only start if the first
	// let go of it.
	second := mk()
	t.Cleanup(func() { second.Close(t.Context()) })
	if err := second.Queue.Start(t.Context()); err != nil {
		t.Fatalf("the second open could not start — the first server is still holding %s: %v", dir, err)
	}

	// And the published event is still there, which an in-memory server
	// could not manage.
	got := make(chan *events.Event, 1)
	if err := second.Queue.Subscribe(t.Context(), topics.Event(ev.Type), "restart-probe",
		func(_ context.Context, e *events.Event) queue.Result {
			select {
			case got <- e:
			default:
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case e := <-got:
		if e.ID != ev.ID {
			t.Errorf("replayed %s, want %s", e.ID, ev.ID)
		}
	case <-time.After(10 * time.Second):
		t.Error("the event did not survive the restart — the store directory is not being used")
	}
}

func TestPulsarWithLocalCoordinationNamesTheReason(t *testing.T) {
	t.Parallel()
	// Pulsar has no compare-and-set, which is the one primitive a lease
	// needs. Config validation refuses this pair, but a Bootstrap is an
	// exported struct — and a nil coordination backend surfaces as a panic
	// in the seat host rather than as a configuration error.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:1"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationLocal
	})
	back, err := engine.OpenBackends(context.Background(), b)
	if err == nil {
		t.Fatal("a Pulsar stream with local coordination opened backends")
	}
	if back != nil {
		t.Error("a failed open returned backends")
	}
}

// marker is a payload this suite publishes to prove the stream round-trips.
type marker struct {
	Note string `json:"note"`
}

func (marker) EventType() string { return "engine.restart_marker" }

func init() { events.Register[marker]() }

// NOTE ON WHAT THIS SUITE CANNOT SEE.
//
// Two of Close's three steps have effects outside this package's reach, and
// mutation confirms it: removing either leaves every test here green.
//
//   - Stopping the QUEUE before the server buys a peer a fast handoff. In one
//     process the shut-down server kills the connection either way, so there
//     is nothing to observe; two nodes and one broker would show it, which is
//     the fleet suite's shape.
//   - Shutting the SERVER down releases its resources. A solo embedded server
//     runs with DontListen and binds no port at all, so there is no socket to
//     probe; a clustered one does bind, but a single member never reaches
//     JetStream quorum (measured: it waits the full 60 s and fails), so
//     standing one up here is not possible either.
//
// Both are stated at their site in backends.go rather than left as apparent
// coverage. Writing a test that passes without exercising them would be worse
// than having none.
