package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

func bootstrap(t *testing.T, mutate func(*config.Bootstrap)) *config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	// The default store path is relative, so a test that took it would
	// create a database in the package directory and share it with every
	// other test in the run. One process owns a store file exclusively.
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	if mutate != nil {
		mutate(&b)
	}
	return &b
}

// parsedCompany is the Tier B half OpenBackends needs, for the one field it
// reads: the width of the configured embedding vectors.
func parsedCompany(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// openBackends is OpenBackends with the ordinary company, for the cases whose
// subject is the topology rather than the store.
func openBackends(t *testing.T, b *config.Bootstrap) (*engine.Backends, error) {
	t.Helper()
	return engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
}

func TestTheDefaultTopologyRunsWithNoExternalService(t *testing.T) {
	t.Parallel()
	// The single-binary promise: a company with nothing configured starts.
	// If this needs a broker, the promise is not kept.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, b)
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
	back, err := openBackends(t, b)
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
	back, err := openBackends(t, b)
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
	back, err := openBackends(t, b)
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
	if _, err := engine.OpenBackends(context.Background(), nil, parsedCompany(t, companyDoc)); err == nil {
		t.Error("a nil bootstrap opened backends")
	}
}

func TestAnUnknownStreamTypeNamesItself(t *testing.T) {
	t.Parallel()
	// A Bootstrap is an exported struct, so this is reachable without going
	// through validation — and an error that does not name the value sends
	// a reader to read the code instead of their config.
	b := bootstrap(t, func(b *config.Bootstrap) { b.Stream.Type = "kafka" })
	_, err := engine.OpenBackends(context.Background(), b, parsedCompany(t, companyDoc))
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
	_, err := engine.OpenBackends(context.Background(), b, parsedCompany(t, companyDoc))
	if err == nil {
		t.Fatal("a Pulsar stream with local coordination opened backends")
	}
	if !strings.Contains(err.Error(), "compare-and-set") {
		t.Errorf("the failure blames the network rather than the topology: %v", err)
	}
}

func TestPulsarWithoutItsCoordinationEstateSaysWhichSideIsMissing(t *testing.T) {
	t.Parallel()
	// Pulsar has no compare-and-set, so its leases live on a NATS estate
	// named separately. An empty block is refused rather than read as
	// "embed one with defaults": those are the same value, and the second
	// reading gives every node in a fleet its own private in-memory lease
	// table — so every node claims every seat, with nothing anywhere
	// reporting a problem.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:6650"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	_, err := engine.OpenBackends(context.Background(), b, parsedCompany(t, companyDoc))
	if err == nil {
		t.Fatal("a Pulsar topology opened with no coordination estate")
	}
	if !strings.Contains(err.Error(), "coordination.nats") {
		t.Errorf("the error does not name the missing block: %v", err)
	}
}

func TestTheCoordinationEstateIsOpenedBeforeThePulsarDial(t *testing.T) {
	// NOT parallel: the observable is a process-wide goroutine count.
	//
	// A node that reached its broker and then found it had nowhere to keep
	// leases would sit attached to topics it must not take work from, and
	// the operator would read a lease error while looking at a healthy
	// broker. So the estate comes up first — which makes it the thing that
	// must be torn down when the Pulsar dial fails.
	dir := t.TempDir()
	before := settledGoroutines()
	for i := range failedOpenAttempts {
		b := bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.Type = config.StreamPulsar
			// A stream configuration the Pulsar client refuses
			// SYNCHRONOUSLY. An unreachable broker would not do: the
			// client connects lazily, so a dial at a dead port
			// succeeds here and fails at the first attach.
			b.Stream.URL = "http://127.0.0.1:6650"
			b.Stream.Tenant = "acme"
			b.Stream.Namespace = "default"
			b.Coordination.Type = config.CoordinationEmbeddedKV
			b.Coordination.NATS.StoreDir = filepath.Join(dir, "coord")
		})
		if _, err := engine.OpenBackends(context.Background(), b,
			parsedCompany(t, companyDoc)); err == nil {
			t.Fatalf("attempt %d: an invalid Pulsar stream opened backends", i)
		}
	}
	if leaked := settledGoroutines() - before; leaked > leakThreshold {
		t.Errorf("%d failed Pulsar opens leaked %d goroutines: the coordination "+
			"estate was left running behind the stream's failure",
			failedOpenAttempts, leaked)
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
		back, err := openBackends(t, b)
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
	back, err := engine.OpenBackends(context.Background(), b, parsedCompany(t, companyDoc))
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

// --- the store slot -------------------------------------------------------- //

func TestTheStoreOpensWithTheOtherSlots(t *testing.T) {
	t.Parallel()
	// A node holding a queue attachment with nowhere to record what a turn
	// did is a node that works and forgets.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if back.Store == nil {
		t.Fatal("no store")
	}
	// Open, and MIGRATED — an opened file with no schema is a store every
	// ledger write fails against, which is not what "opened" should mean.
	if _, err := back.Store.SQL().ExecContext(t.Context(),
		`INSERT INTO conversation_sessions
		     (agent_handle, conversation_key, entry, created_at)
		 VALUES ('ceo', 'thread-1', '{}', 1)`); err != nil {
		t.Errorf("the store opened without its schema: %v", err)
	}
}

func TestTheStoreTakesItsVectorWidthFromTheCompany(t *testing.T) {
	t.Parallel()
	// The store is the only thing that knows how wide the packed BLOBs in
	// its vector columns are, and the model that decides is Tier B. A width
	// it was never told refuses every write from the right one, and recall
	// simply stops returning anything.
	const doc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
  embeddings:
    type: openai
    model: text-embedding-3-large
    api_key: ${K}
    dimensions: 3072
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, doc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if got := back.Store.EmbeddingDim(); got != 3072 {
		t.Errorf("embedding width = %d, want the configured 3072", got)
	}
}

func TestNoEmbeddingProviderIsAWidthOfNoneNotADefault(t *testing.T) {
	t.Parallel()
	// The counterfactual to the case above, and the one that says the width
	// is READ rather than guessed. "This company does not remember by
	// similarity" is a real answer; inventing 1536 for it would let a
	// vector of that width be written to a company that can produce none.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if got := back.Store.EmbeddingDim(); got != 0 {
		t.Errorf("embedding width = %d, want 0 for a company with no embeddings", got)
	}
}

func TestNoCompanyIsRefusedRatherThanDefaulted(t *testing.T) {
	t.Parallel()
	// Refused rather than defaulted because the default would be silent:
	// nothing fails at open, and the failure surfaces later as recall that
	// returns nothing with no reason in the log.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	_, err := engine.OpenBackends(t.Context(), b, nil)
	if err == nil {
		t.Fatal("a nil company opened backends")
	}
	if !strings.Contains(err.Error(), "embedding") {
		t.Errorf("the error does not say what was missing: %v", err)
	}
}

// settledGoroutines waits for the goroutine count to stop moving.
//
// A count taken immediately after a shutdown is meaningless: the goroutines a
// server was told to stop are still winding down. This polls until two
// consecutive readings agree, so what it returns is what stayed.
func settledGoroutines() int {
	prev := -1
	for range 40 {
		runtime.GC()
		time.Sleep(25 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	return runtime.NumGoroutine()
}

func TestAStoreThatCannotOpenTakesTheBrokerDownWithIt(t *testing.T) {
	// NOT parallel: the observable is a process-wide goroutine count.
	//
	// The store is opened LAST, so its failure is the only one with
	// something to clean up behind it. An embedded server left running
	// after a failed open is a leak nothing else reports — the process
	// carries a broker nobody holds a handle to, for its whole life.
	//
	// Goroutines are the observable, and the first thing tried was not:
	// asserting that a second open on the same stream directory succeeds
	// proves nothing, because two embedded servers share a store directory
	// without complaint (measured). That test passed with the cleanup
	// removed, which makes it worse than no test.
	//
	// The margin here is not delicate: three failed opens leak about a
	// hundred goroutines, against a residue of one when they are cleaned
	// up. The threshold sits far from both.
	dir := t.TempDir()
	before := settledGoroutines()
	for i := range failedOpenAttempts {
		b := bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.StoreDir = filepath.Join(dir, "stream")
			// A directory is not a database file.
			b.Store.Path = dir
		})
		if _, err := openBackends(t, b); err == nil {
			t.Fatalf("attempt %d: a store path that is a directory opened backends", i)
		}
	}
	if leaked := settledGoroutines() - before; leaked > leakThreshold {
		t.Errorf("%d failed opens leaked %d goroutines: the broker was left "+
			"running behind the store's failure", failedOpenAttempts, leaked)
	}
}

const (
	// failedOpenAttempts is how many times the failure is repeated, so one
	// leak becomes a multiple of itself and cannot be mistaken for noise.
	failedOpenAttempts = 3

	// leakThreshold sits between the two measured outcomes — a residue of
	// one goroutine when the broker is closed, about a hundred when it is
	// not — rather than at zero, which would make the test fail on any
	// unrelated background goroutine that happens to still be settling.
	leakThreshold = 10
)

func TestTheStoreOutlivesTheHandlersThatWriteToIt(t *testing.T) {
	t.Parallel()
	// Close order. The queue stops first and waits for in-flight handlers;
	// the store closes after, because everything writes to it. Closing it
	// first would turn the tail of a graceful drain into a run of "database
	// is closed" — the drain would still finish, and would finish having
	// recorded none of what it drained.
	//
	// The assertion is on the FILE, read back after everything is shut, not
	// on an error variable the handler set. An error captured in one
	// goroutine and read in another needs a happens-before edge to be worth
	// anything, and the edge in question — whether Close waits for the
	// handler — is the very thing under test.
	dir := t.TempDir()
	path := filepath.Join(dir, "crewlet.db")
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(dir, "stream")
		b.Store.Path = path
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := back.Queue.Subscribe(t.Context(), "t.close", "g",
		func(ctx context.Context, _ *events.Event) queue.Result {
			once.Do(func() { close(entered) })
			<-release
			// Written while Close is running, which is the whole point.
			// The context is detached because cancelling the delivery is
			// how a shutdown reaches a handler, and a write refused for
			// that reason would not be evidence about the store.
			_, _ = back.Store.SQL().ExecContext(context.WithoutCancel(ctx),
				`INSERT INTO conversation_sessions
				     (agent_handle, conversation_key, entry, created_at)
				 VALUES ('ceo', 'late', '{}', 1)`)
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := back.Queue.Publish(t.Context(), "t.close",
		&events.Event{ID: uuid.New(), Type: "notification"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		back.Close(context.Background())
	}()
	// Let Close get as far as it can with a handler still in flight, then
	// let the handler finish and write.
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned")
	}

	again, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	var n int
	if err := again.SQL().QueryRowContext(t.Context(),
		`SELECT count(*) FROM conversation_sessions WHERE conversation_key = 'late'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Error("a handler still in flight when Close began could not record " +
			"what it drained: the store closed out from under it")
	}
}

func TestCloseLeavesTheStoreClosed(t *testing.T) {
	t.Parallel()
	// The counterfactual to the ordering above. "Closes last" is satisfied
	// just as well by "never closes", and a store handle left open holds
	// the file for the life of the process — which matters because one
	// process owns it exclusively, so a restart in-process contends with
	// the corpse of the last one.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	db := back.Store
	back.Close(t.Context())
	if err := db.SQL().PingContext(t.Context()); err == nil {
		t.Error("the store is still open after Close")
	}
	if back.Store != nil {
		t.Error("Close left a handle to a closed store on the Backends")
	}
}

func TestTheStoreTakesItsDriverAndBusyTimeoutFromConfig(t *testing.T) {
	t.Parallel()
	// Both are Tier A knobs an operator sets and nothing else reports. A
	// driver silently ignored means running on a different storage engine
	// than the one configured — which the config's own validator calls a
	// data-loss shape, not a cosmetic one — and a busy timeout ignored
	// means statements giving up on lock contention far sooner than asked.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Store.Driver = config.StoreDriverSQLite
		b.Store.BusyTimeoutSeconds = 11
		b.Store.MaxOpenConns = 3
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	if got := back.Store.Driver(); got != store.DriverSQLite {
		t.Errorf("driver = %q, want the configured %q", got, store.DriverSQLite)
	}
	var busyMS int
	if err := back.Store.SQL().QueryRowContext(t.Context(),
		"PRAGMA busy_timeout").Scan(&busyMS); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyMS != 11_000 {
		t.Errorf("busy_timeout = %dms, want the configured 11000", busyMS)
	}
	if got := back.Store.SQL().Stats().MaxOpenConnections; got != 3 {
		t.Errorf("max open conns = %d, want the configured 3", got)
	}
}

func TestAnUnsetDriverAndTimeoutTakeTheStoreDefaults(t *testing.T) {
	t.Parallel()
	// The counterfactual. Passing the configured values through is only
	// half the contract: zero must reach the store as "you choose", not as
	// a literal zero, which for a busy timeout means every contended
	// statement failing immediately.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	if got := back.Store.Driver(); got != store.DriverTurso {
		t.Errorf("driver = %q, want the default %q", got, store.DriverTurso)
	}
	var busyMS int
	if err := back.Store.SQL().QueryRowContext(t.Context(),
		"PRAGMA busy_timeout").Scan(&busyMS); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyMS <= 0 {
		t.Errorf("busy_timeout = %dms: an unset timeout reached the store as a "+
			"literal zero, so every contended statement fails at once", busyMS)
	}
	// Zero must mean "the store chooses", not "one connection at a time" —
	// and certainly not database/sql's own unlimited, which on a single
	// SQLite file is how a dashboard query storm becomes lock contention.
	if got := back.Store.SQL().Stats().MaxOpenConnections; got <= 0 {
		t.Errorf("max open conns = %d: an unset bound reached the store as a "+
			"literal zero", got)
	}
}

func TestAPulsarTopologyGetsARealLeaseStore(t *testing.T) {
	t.Parallel()
	// The whole point of the coordination estate: a Pulsar deployment can
	// hold seats. Before this existed every Pulsar topology was refused
	// outright, so the slot shipped and could not be run.
	//
	// The broker is never reached — the Pulsar client connects lazily, so
	// what is under test here is the assembly and the lease store, not the
	// stream. A live-broker case belongs to the conformance suite, which
	// has one.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:6650"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationEmbeddedKV
		b.Coordination.NATS.StoreDir = filepath.Join(t.TempDir(), "coord")
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	if back.Queue == nil || back.Coord == nil || back.Store == nil {
		t.Fatalf("backends = %+v, want every slot", back)
	}
	lease, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("the coordination estate refused a first acquire")
	}
	// EXCLUSIVE, which is the property Pulsar cannot supply and this
	// estate exists to add.
	if got, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-2", TTL: 30 * time.Second}); err != nil {
		t.Fatalf("second Acquire: %v", err)
	} else if got != nil {
		t.Error("two owners held one seat on a Pulsar topology")
	}
}

func TestAnEstateWithNoURLIsEmbeddedEvenWithNoStoreDirectory(t *testing.T) {
	t.Parallel()
	// The absence of a URL is what makes an estate embedded, not the
	// presence of a store directory: an in-memory member names neither,
	// and reading it the other way round sends the node off to dial a NATS
	// server nobody configured.
	//
	// In-memory is a real shape. It loses every lease on a restart, which
	// is survivable — leases are TTL-bounded and re-claimed — and means a
	// restart sheds this node's seats rather than resuming them.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:6650"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationEmbeddedKV
		// No URL and no store directory: an embedded member holding its
		// leases in memory. Replicas is what keeps the block non-zero,
		// which validation requires so that "embed with defaults" and
		// "nothing was said" stay distinguishable.
		b.Coordination.NATS.Replicas = 1
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	if _, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second}); err != nil {
		t.Errorf("an in-memory coordination estate could not hold a lease: %v", err)
	}
}

func TestAnEstateWithAStoreDirectoryWritesToIt(t *testing.T) {
	t.Parallel()
	// The counterfactual to the in-memory case above, and the half that
	// says store_dir is read at all: an estate that quietly ran in memory
	// would pass every assertion about holding a lease, and lose every one
	// of them on a restart.
	dir := filepath.Join(t.TempDir(), "coord")
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamPulsar
		b.Stream.URL = "pulsar://127.0.0.1:6650"
		b.Stream.Tenant = "acme"
		b.Stream.Namespace = "default"
		b.Coordination.Type = config.CoordinationEmbeddedKV
		b.Coordination.NATS.StoreDir = dir
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	if _, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the configured store directory was never created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("the coordination estate wrote nothing to its store directory: " +
			"it is running in memory, and every lease is lost on a restart")
	}
}

func TestAnUnopenableCoordinationEstateIsReportedAndLeavesNothingBehind(t *testing.T) {
	// NOT parallel: the observable is a process-wide goroutine count.
	//
	// The other half of the Pulsar assembly. The embedded member starts
	// before its KV bucket is opened, so a failure between the two has a
	// running server to clean up — one a node would otherwise carry for
	// the life of the process with no handle to it.
	//
	// The failure is injected with an already-cancelled context rather
	// than a broken store directory. Both reach the same branch, and the
	// broken directory takes thirty seconds per attempt to get there:
	// StartServer accepts it and the wait happens inside the KV open,
	// which is a timeout, not a refusal.
	dir := t.TempDir()
	before := settledGoroutines()
	for i := range failedOpenAttempts {
		b := bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.Type = config.StreamPulsar
			b.Stream.URL = "pulsar://127.0.0.1:6650"
			b.Stream.Tenant = "acme"
			b.Stream.Namespace = "default"
			b.Coordination.Type = config.CoordinationEmbeddedKV
			b.Coordination.NATS.StoreDir = filepath.Join(dir, fmt.Sprint("coord", i))
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := engine.OpenBackends(ctx, b,
			parsedCompany(t, companyDoc)); err == nil {
			t.Fatalf("attempt %d: an unopenable coordination estate opened backends", i)
		}
	}
	if leaked := settledGoroutines() - before; leaked > leakThreshold {
		t.Errorf("%d failed coordination opens leaked %d goroutines",
			failedOpenAttempts, leaked)
	}
}

// THE FLEET STORE IS ALWAYS THE KV, and the default single-node topology is
// exactly the case that proves it: `coordination.type` is `local` there, and
// while the fleet store followed that slot it was an in-process map.
//
// Everything in it then reset with the process. The token counter's bucket is
// documented as having NO retention — "a cap is a ceiling for the life of a
// deployment" — and a restart put a company's spend back to zero. A detached
// sandbox run, which is a BILLED box, was forgotten by the engine that
// launched it. A turn completion no longer suppressed the redelivery it exists
// to suppress.
//
// A lease is the one thing that legitimately follows the slot: a single node
// has no peer to fence against and re-claims at boot.
func TestTheFleetStoreSurvivesARestartOnLocalCoordination(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "stream")
	mk := func() *engine.Backends {
		t.Helper()
		b := bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.StoreDir = dir
			// The default, stated rather than assumed: this test is
			// about the topology an operator gets without asking.
			b.Coordination.Type = config.CoordinationLocal
		})
		back, err := openBackends(t, b)
		if err != nil {
			t.Fatalf("OpenBackends: %v", err)
		}
		return back
	}

	first := mk()
	if _, err := first.Fleet.Charge(t.Context(), coord.AgentScope("a-1"), 900, 0, 0); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if err := first.Fleet.OpenChannel(t.Context(), coord.Channel{
		ID: "c1", Requester: "alice", Target: "bob",
		OpenedAt: time.Now().UTC(), LastAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	if _, err := first.Fleet.CreateSandboxRun(t.Context(), "turn-1", []byte(`{"status":"running"}`)); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	first.Close(t.Context())

	second := mk()
	t.Cleanup(func() { second.Close(t.Context()) })
	if used, err := second.Fleet.Used(t.Context(), coord.OrgScope); err != nil || used != 900 {
		t.Errorf("org spend after a restart = %d (err %v), want 900 — the cap is a "+
			"ceiling for the deployment's life, not for one process", used, err)
	}
	if _, found, err := second.Fleet.Channel(t.Context(), "c1"); err != nil || !found {
		t.Errorf("the open ask did not survive the restart (found=%v err=%v) — "+
			"its answer would be refused as an unknown channel", found, err)
	}
	if _, found, err := second.Fleet.SandboxRun(t.Context(), "turn-1"); err != nil || !found {
		t.Errorf("the detached run did not survive the restart (found=%v err=%v) — "+
			"its box is billing with nobody to collect it", found, err)
	}
}

// AN EXTERNAL URL IS DIALLED, not quietly replaced with a private broker.
//
// `stream.type: nats` with a URL means "the cluster somebody else runs".
// Every path here used to start an in-process member and connect to THAT,
// ignoring the URL: an operator who pointed a fleet at an external cluster
// got one private broker per node, so no node shared a stream or a
// coordination bucket with any other — and nothing said so. Every symptom
// that followed was a fleet-shared-state break wearing a different mask.
//
// Asserted as a REFUSAL against an address nothing answers on, which is the
// one shape that needs no broker: a node that starts cleanly here is a node
// that never tried to reach the URL it was given.
func TestAnExternalStreamURLIsActuallyDialled(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamNATS
		// Port 1 on loopback: reserved, and nothing binds it.
		b.Stream.URL = "nats://127.0.0.1:1"
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	back, err := openBackends(t, b)
	if err == nil {
		back.Close(t.Context())
		t.Fatal("a node started cleanly against a broker that does not " +
			"exist, so it is running a private in-process one and shares " +
			"nothing with its peers")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("err = %v, want it to name the address it could not reach", err)
	}
}

// AND ITS TLS MATERIAL TRAVELS WITH IT.
//
// The field is read by config, validated, and then has to reach the dial. A
// block that is set, accepted by `crewlet validate`, and never applied leaves
// a broker rejecting every connection for a reason no log line connects to
// the omission.
func TestAnExternalStreamsTLSMaterialReachesTheDial(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamNATS
		b.Stream.URL = "nats://127.0.0.1:1"
		b.Stream.TLS = config.NATSTLS{CA: "/nope/ca.pem"}
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	back, err := openBackends(t, b)
	if err == nil {
		back.Close(t.Context())
		t.Fatal("a node started with a CA bundle that does not exist")
	}
	if !strings.Contains(err.Error(), "/nope/ca.pem") {
		t.Errorf("err = %v, want the configured tls.ca to have reached the "+
			"dial and been refused by name", err)
	}
}
