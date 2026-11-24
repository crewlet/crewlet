package engine_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// bootstrap is a node's Tier A for a case, its store in the case's own
// directory and already MIGRATED from the image this binary builds once, while
// the store is where this put it: a boot here is about the engine, never the
// migration chain, which internal/store certifies. A case that moves the store
// (onto a directory, or a path two boots share) seeds it itself
// ([engine.SeedStore]).
func bootstrap(t *testing.T, mutate func(*config.Bootstrap)) *config.Bootstrap {
	t.Helper()
	b, chosen := unseededBootstrap(t, mutate)
	if b.Store.Path == chosen {
		engine.SeedStore(t, b)
	}
	return b
}

// unseededBootstrap is [bootstrap] with nothing written at the store's path,
// for a case whose subject is what a node does before anything exists there,
// and the path it chose for the store.
func unseededBootstrap(t *testing.T, mutate func(*config.Bootstrap)) (*config.Bootstrap, string) {
	t.Helper()
	b := config.DefaultBootstrap()
	// The default store path is relative: a test taking it would create a
	// database in the package directory shared with every other test, and one
	// process owns a store file exclusively.
	chosen := filepath.Join(t.TempDir(), "crewlet.db")
	b.Store.Path = chosen
	engine.PinGitLanes(&b)
	// AND A KEYRING: every state-log record is signed and a node with none
	// refuses to start. Never in [config.DefaultBootstrap]: a default key is
	// one every deployment shares, the opposite of what signing buys.
	b.Secrets.ActiveKeyID = "test"
	b.Secrets.Keys = []config.SecretKey{{ID: "test", Material: testKeyMaterial(t)}}
	if mutate != nil {
		mutate(&b)
	}
	return &b, chosen
}

// testKeyMaterial mints one through the minter `crewlet secrets keygen` uses,
// so size, randomness and encoding come from the production definition; per
// call, so parallel cases share no key.
func testKeyMaterial(t *testing.T) string {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("no randomness for the test keyring: %v", err)
	}
	return secrets.EncodeKey(key)
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
	return engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc), nil)
}

func TestTheDefaultTopologyRunsWithNoExternalService(t *testing.T) {
	t.Parallel()
	// The single-binary promise: a company with nothing configured starts
	// without an external broker.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
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
	// A node holding one slot without the other can hear work it may not do,
	// or hold seats it cannot serve.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	back.Close(t.Context())
	// Twice is safe: a deferred Close beside an explicit one is the ordinary
	// shape.
	back.Close(t.Context())
}

func TestAnEmbeddedKVStoreLivesOnTheStreamsOwnBroker(t *testing.T) {
	t.Parallel()
	// A second estate would fail independently: a node could hold live leases
	// on one while the other lost its inbox, alive to its peers and deaf to its
	// work (ADR-0001). Observable as the KV slot building at all on an embedded
	// stream, which it can only do by reaching the in-process server.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	// It is a real store: a lease acquired through it is held.
	lease, _, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil {
		t.Fatal("the coordination slot refused a first acquire")
	}
	// And EXCLUSIVE, which a memory stand-in would also be: the build above
	// says it is the KV store, this says it works.
	if got, _, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-2", TTL: 30 * time.Second}); err != nil {
		t.Fatalf("second Acquire: %v", err)
	} else if got != nil {
		t.Error("two owners held one seat")
	}
}

// THE COORDINATION STORE'S CONNECTION IS ONE THE NODE HEARS LOSING.
//
// On an embedded broker the store rides its own connection and holds every
// lease this node renews, so NATS closing it for good is a node with no
// leases, reported with the queue's own loss (jetstream.Queue.Lost). Only a
// connection dialled watched does that: told to call a closed handler for a
// close NATS made, and not for the node's own Close at shutdown. This is the
// engine's half of ADR-0001's gate; the jetstream package holds that
// DialWatched is watched. A DialOwned (donor-style) connection's loss would be
// heard by nothing.
//
// Mutation: open it with DialOwned and both halves go red.
func TestTheCoordinationConnectionIsWatched(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Coordination.Type = config.CoordinationEmbeddedKV
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	conn := back.Conn()
	if conn == nil {
		t.Fatal("an embedded topology answered no coordination connection")
	}
	if conn.Opts.ClosedCB == nil {
		t.Error("the coordination store's connection has no closed handler, so " +
			"NATS closing it for good leaves the node holding leases it cannot renew")
	}
	if !conn.Opts.NoCallbacksAfterClientClose {
		t.Error("the coordination store's connection reports the node's own " +
			"Close at shutdown as a lost broker")
	}
}

func TestLocalCoordinationNeedsNoBroker(t *testing.T) {
	t.Parallel()
	// The default: one node, no quorum, no network, and still a real backend,
	// because a nil one surfaces as a panic in the seat host rather than a
	// configuration error.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
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
	if _, _, err := back.Coord.TryAcquire(t.Context(), "seat:ceo",
		coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second}); err != nil {
		t.Errorf("Acquire: %v", err)
	}
}

func TestNoBootstrapIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := engine.OpenBackends(context.Background(), nil, parsedCompany(t, companyDoc), nil); err == nil {
		t.Error("a nil bootstrap opened backends")
	}
}

func TestAnUnknownStreamTypeNamesItself(t *testing.T) {
	t.Parallel()
	// A Bootstrap is an exported struct, so this is reachable past validation,
	// and an error not naming the value sends a reader to the code instead of
	// their config.
	b := bootstrap(t, func(b *config.Bootstrap) { b.Stream.Type = "kafka" })
	_, err := engine.OpenBackends(context.Background(), b, parsedCompany(t, companyDoc), nil)
	if err == nil {
		t.Fatal("an unknown stream type opened backends")
	}
	if !strings.Contains(err.Error(), "kafka") {
		t.Errorf("the error does not name the type: %v", err)
	}
}

func TestAnEmbeddedStreamPersistsAcrossARestart(t *testing.T) {
	t.Parallel()
	// Two properties in one round trip:
	//
	//   - Close SHUTS THE SERVER DOWN: a leaked server holds its store
	//     directory, so the second open below fails.
	//   - StoreDir is USED: ignoring it selects an in-memory server, which
	//     loses everything at the first restart.
	dir := filepath.Join(brokertest.TempDir(t), "stream")
	mk := func() *engine.Backends {
		t.Helper()
		b := bootstrap(t, func(b *config.Bootstrap) { brokertest.StoreIn(&b.Stream, dir) })
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
	// Close stops the queue itself (step one of its order); stopping it here
	// too would hide whether it does.
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

// marker is a payload this suite publishes to prove the stream round-trips.
type marker struct {
	Note string `json:"note"`
}

func (marker) EventType() string { return "engine.restart_marker" }

func init() { events.Register[marker]() }

// NOTE ON WHAT THIS SUITE CANNOT SEE: two of Close's three steps act outside
// this package's reach, and removing either leaves every test here green.
//
//   - Stopping the QUEUE before the server buys a peer a fast handoff, which
//     only two nodes on one broker show (the fleet suite's shape).
//   - Shutting the SERVER down releases its resources: a solo embedded server
//     binds no port (DontListen), and a single clustered member never reaches
//     JetStream quorum, so neither can be probed here.
//
// Both are stated at their site in backends.go; a test that passed without
// exercising them would be worse than none.

// --- the store slot -------------------------------------------------------- //

func TestTheStoreOpensWithTheOtherSlots(t *testing.T) {
	t.Parallel()
	// A node holding a queue attachment with nowhere to record what a turn
	// did is a node that works and forgets.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
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
		`INSERT INTO scheduled_runs
		     (scope_type, scope_id, schedule_name, fire_label, scheduled_at, fired_at)
		 VALUES ('role', 'ceo', 'standup', '20260101T0900', 1, 1)`); err != nil {
		t.Errorf("the store opened without its schema: %v", err)
	}
}

func TestTheStoreTakesItsVectorWidthFromTheCompany(t *testing.T) {
	t.Parallel()
	// The store alone knows how wide its packed vector BLOBs are, and Tier B's
	// model decides it: a width it was never told refuses every write from the
	// right one, and recall stops returning anything.
	const doc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, doc), nil)
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
	// The counterfactual: the width is READ, not guessed. "No similarity
	// recall" is a real answer; inventing 1536 would let a vector of that width
	// be written to a company that can produce none.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
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

// NO COMPANY OPENS AT WIDTH 0, because a node opens its store before it can
// read the company out of it, and a node with no active revision has none.
//
// Safe because a store never told a width LEARNS it from the first revision it
// applies and holds it from then on (TestTheEmbeddingWidthIsLearnedOnceAndHeld):
// 0 is "not yet told", never a width a write is checked against. If that stops
// being true this must become a refusal again, since a store stuck at the
// wrong width refuses every write from the right one.
func TestNoCompanyOpensAtWidthZero(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, nil, nil)
	if err != nil {
		t.Fatalf("a nil company was refused: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if got := back.Store.EmbeddingDim(); got != 0 {
		t.Errorf("embedding width = %d, want 0 until an apply says otherwise", got)
	}
}

// settledGoroutines polls until two consecutive goroutine counts agree, so
// what it returns is what stayed after a shutdown, not what is still winding
// down.
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
	// The store opens LAST, so its failure alone has something to clean up
	// behind it: an embedded server left running after a failed open is a
	// broker nobody holds a handle to, for the life of the process. Goroutines,
	// because a second open on the same stream directory succeeds either way
	// (two embedded servers share one without complaint). Three failed opens
	// leak about a hundred goroutines against a residue of one when cleaned up;
	// the threshold sits far from both.
	dir := brokertest.TempDir(t)
	before := settledGoroutines()
	for i := range failedOpenAttempts {
		b := bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
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
	// failedOpenAttempts repeats the failure so one leak becomes a multiple of
	// itself and cannot be mistaken for noise.
	failedOpenAttempts = 3

	// leakThreshold sits between the measured outcomes (a residue of one, about
	// a hundred when leaked) rather than at zero, which would fail on any
	// unrelated goroutine still settling.
	leakThreshold = 10
)

func TestTheStoreOutlivesTheHandlersThatWriteToIt(t *testing.T) {
	t.Parallel()
	// Close order: the queue stops first and waits for in-flight handlers, and
	// the store closes after, because everything writes to it; closing it first
	// turns the tail of a graceful drain into "database is closed", a drain that
	// records none of what it drained.
	//
	// Asserted on the FILE read back after shutdown, not on an error the
	// handler set: reading that across goroutines needs the very happens-before
	// edge (Close waiting for the handler) under test.
	dir := brokertest.TempDir(t)
	path := filepath.Join(dir, "crewlet.db")
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
		b.Store.Path = path
	})
	engine.SeedStore(t, b)
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
			// Written while Close runs, which is the point. Detached, because
			// cancelling the delivery is how shutdown reaches a handler, and a
			// write refused for that is no evidence about the store.
			_, _ = back.Store.SQL().ExecContext(context.WithoutCancel(ctx),
				`INSERT INTO scheduled_runs
				     (scope_type, scope_id, schedule_name, fire_label, scheduled_at, fired_at)
				 VALUES ('role', 'ceo', 'late', '20260101T0900', 1, 1)`)
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

	again, err := store.OpenNode(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	var n int
	if err := again.SQL().QueryRowContext(t.Context(),
		`SELECT count(*) FROM scheduled_runs WHERE schedule_name = 'late'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Error("a handler still in flight when Close began could not record " +
			"what it drained: the store closed out from under it")
	}
}

func TestCloseLeavesTheStoreClosed(t *testing.T) {
	t.Parallel()
	// The counterfactual: "closes last" is also satisfied by "never closes", and
	// an open handle holds the file, which one process owns exclusively, so an
	// in-process restart contends with the last one.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
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

func TestTheStoreTakesItsPoolKnobsFromConfig(t *testing.T) {
	t.Parallel()
	// Tier A knobs nothing else reports: an ignored busy timeout gives up on
	// lock contention far sooner than asked, and an ignored pool bound lets the
	// dashboard's read burst open however many connections database/sql likes.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Store.BusyTimeoutSeconds = 11
		b.Store.MaxOpenConns = 3
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	var busyMS int
	if err := back.Store.SQL().QueryRowContext(t.Context(),
		"PRAGMA busy_timeout").Scan(&busyMS); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyMS != 11_000 {
		t.Errorf("busy_timeout = %dms, want the configured 11000", busyMS)
	}
	// THE CONFIGURED READERS plus what the store keeps beside them: the
	// connection it reserves for resolving who is acting is added to
	// `max_open_conns`, not taken from it. Read off a store opened with the same
	// knob, so the engine is held to passing it through and the store to what
	// it adds.
	reference := storetest.OpenNode(t, filepath.Join(t.TempDir(), "reference.db"),
		store.Options{MaxOpenConns: 3})
	t.Cleanup(func() { _ = reference.Close() })
	want := reference.SQL().Stats().MaxOpenConnections
	if got := back.Store.SQL().Stats().MaxOpenConnections; got != want {
		t.Errorf("max open conns = %d, want the %d a store makes of the configured 3",
			got, want)
	}
}

func TestAnUnsetTimeoutTakesTheStoreDefault(t *testing.T) {
	t.Parallel()
	// The counterfactual: zero must reach the store as "you choose", not as a
	// literal zero, which for a busy timeout fails every contended statement
	// at once.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	var busyMS int
	if err := back.Store.SQL().QueryRowContext(t.Context(),
		"PRAGMA busy_timeout").Scan(&busyMS); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyMS <= 0 {
		t.Errorf("busy_timeout = %dms: an unset timeout reached the store as a "+
			"literal zero, so every contended statement fails at once", busyMS)
	}
	// Zero means "the store chooses": not one connection at a time, and not
	// database/sql's unlimited, which on one SQLite file turns a dashboard
	// query storm into lock contention.
	if got := back.Store.SQL().Stats().MaxOpenConnections; got <= 0 {
		t.Errorf("max open conns = %d: an unset bound reached the store as a "+
			"literal zero", got)
	}
}

func TestTheFleetStoreSurvivesARestartOnLocalCoordination(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(brokertest.TempDir(t), "stream")
	mk := func() *engine.Backends {
		t.Helper()
		b := bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, dir)
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
	windows := coord.WindowsAt(time.Now(), time.UTC)
	if _, err := first.Fleet.PostCharge(t.Context(), coord.AgentScope("a-1"), 900, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if err := first.Fleet.OpenChannel(t.Context(), coord.Channel{
		ID: "c1", Requester: "alice", Target: "bob",
		OpenedAt: time.Now().UTC(), LastAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	if _, err := first.Fleet.CreateSandboxRun(t.Context(), "turn-1", []byte(`{"status":"running"}`), nil); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	first.Close(t.Context())

	second := mk()
	t.Cleanup(func() { second.Close(t.Context()) })
	if used, err := second.Fleet.Used(t.Context(), coord.OrgScope, windows); err != nil ||
		used.In(period.Month).Used != 900 {
		t.Errorf("org spend after a restart = %+v (err %v), want 900 this month — a "+
			"window's spend belongs to the window, not to the process that charged it",
			used, err)
	}
	if _, found, err := second.Fleet.Channel(t.Context(), "c1"); err != nil || !found {
		t.Errorf("the open ask did not survive the restart (found=%v err=%v) — "+
			"its answer would be refused as an unknown channel", found, err)
	}
	if _, found, err := second.Fleet.SandboxRun(t.Context(), "turn-1"); err != nil || !found {
		t.Errorf("the detached run did not survive the restart (found=%v err=%v) — "+
			"its box runs on, holding its node, with nobody to collect it", found, err)
	}
}

// AN EXTERNAL URL IS DIALLED, not quietly replaced with a private broker.
//
// `stream.type: nats` with a URL means "the cluster somebody else runs"; a
// node that started an in-process member instead would share no stream or
// coordination bucket with its peers, and nothing would say so. Asserted as a
// REFUSAL against an address nothing answers on, the one shape needing no
// broker: a node that starts cleanly here never tried the URL it was given.
func TestAnExternalStreamURLIsActuallyDialled(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamNATS
		// Port 1 on loopback: reserved, and nothing binds it.
		b.Stream.URL = "nats://127.0.0.1:1"
		b.Stream.Token = externaltest.Token
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

// AND ITS TLS MATERIAL TRAVELS WITH IT: a block set, accepted by `crewlet
// validate` and never applied leaves a broker rejecting every connection for
// a reason no log line connects to the omission.
func TestAnExternalStreamsTLSMaterialReachesTheDial(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamNATS
		b.Stream.URL = "nats://127.0.0.1:1"
		b.Stream.Token = externaltest.Token
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

// A NODE WILL NOT BOOT AGAINST A NATS SERVER THAT CANNOT CARRY WHAT IT WRITES.
//
// nats-server defaults max_payload to 1 MiB; a node on such a server would
// serve and then refuse its first record past it (`record_too_large`) when
// nobody looks at the broker. So boot refuses by a sentence naming the
// setting, the value the server announced and the one this build needs, which
// is also the sentence deployment.md prints, so the example an operator greps
// for cannot drift. A server carrying the contract boots: the control.
//
// Mutation: drop the check from the jetstream backend's dial and the refusal
// row goes red; compare with `>` and the control does; edit the guide's
// example and the refusal row does.
func TestABootAgainstAServerBelowTheContractIsRefusedByName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		maxPayload int32
		announced  int
		refused    bool
	}{
		{"nats-server's own default", 0, natsserver.MAX_PAYLOAD_SIZE, true},
		{"the contract's own number", queue.MaxPayloadBytes, queue.MaxPayloadBytes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url := externaltest.Start(t, tc.maxPayload).URL()
			b := bootstrap(t, func(b *config.Bootstrap) {
				b.Stream.Type = config.StreamNATS
				b.Stream.URL = url
				b.Stream.Token = externaltest.Token
				b.Coordination.Type = config.CoordinationEmbeddedKV
			})
			back, err := openBackends(t, b)
			if !tc.refused {
				if err != nil {
					t.Fatalf("a node refused a server announcing a max_payload of "+
						"%d: %v", tc.announced, err)
				}
				back.Close(t.Context())
				return
			}
			if err == nil {
				back.Close(t.Context())
				t.Fatalf("a node booted against a server announcing a max_payload "+
					"of %d, and would refuse its first record past it only when it "+
					"was written", tc.announced)
			}
			for _, want := range []string{"max_payload", strconv.Itoa(tc.announced),
				strconv.Itoa(queue.MaxPayloadBytes), "max_payload: 8MB", "stream.url"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
			// AND IT IS THE SENTENCE THE DEPLOYMENT GUIDE PRINTS, for a boot
			// against nats-server's own default at the guide's address.
			guide, rerr := os.ReadFile(filepath.Join(sourcetree.Root(t),
				"docs", "guides", "deployment.md"))
			if rerr != nil {
				t.Fatalf("read the deployment guide: %v", rerr)
			}
			printed := strings.ReplaceAll(err.Error(), url, "nats://nats-1.internal:4222")
			if !strings.Contains(string(guide), "\n"+printed+"\n") {
				t.Errorf("docs/guides/deployment.md does not print the refusal a "+
					"boot writes; it should carry, on a line of its own:\n%s", printed)
			}
		})
	}
}

// THE WIDTH IS LEARNED ONCE AND THEN HELD.
//
// The width belongs to the vectors in the file, not to the config, so
// buildEmbedder refuses a revision whose width differs and tells the operator
// to restart; a store never told one (no active revision at boot, no rows)
// learns it from its first epoch. The way back must NOT work: were it
// re-stated on every apply, dropping the provider would set 0 and turn the
// guard off, and re-adding it at another width would put two widths in one
// recall pool, which the reader matches neither of and EncodeVector,
// checking nothing against 0, would never report.
func TestTheEmbeddingWidthIsLearnedOnceAndHeld(t *testing.T) {
	t.Parallel()
	const noVectors = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`
	const wide = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
	narrow := strings.Replace(wide, "dimensions: 3072", "dimensions: 1536", 1)
	narrow = strings.Replace(narrow, "text-embedding-3-large", "text-embedding-3-small", 1)

	e := newEngine(t, engine.Options{Company: parsedCompany(t, noVectors),
		Environment: config.MapSource{"K": "test-key"}})
	if got := e.Backends().Store.EmbeddingDim(); got != 0 {
		t.Fatalf("booted at width %d, want 0 for a company with no embeddings", got)
	}

	// LEARNED: the unconfigured node's first revision turns the dimension
	// guard on.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, wide), time.Now()); err != nil {
		t.Fatalf("apply the embedding provider: %v", err)
	}
	if got := e.Backends().Store.EmbeddingDim(); got != 3072 {
		t.Fatalf("after the first apply the width is %d, want 3072 — the "+
			"dimension guard stays off for the life of the process", got)
	}
	if _, err := e.Backends().Store.EncodeVector(make([]float32, 3072)); err != nil {
		t.Errorf("a vector from the applied provider was refused: %v", err)
	}
	if _, err := e.Backends().Store.EncodeVector(make([]float32, 1536)); err == nil {
		t.Error("a mis-sized vector was accepted: the guard did not come on")
	}

	// HELD: a revision changing it is refused, by the guard that already
	// existed.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, narrow), time.Now()); err == nil {
		t.Error("a revision changing the store's width was applied")
	}
	if got := e.Backends().Store.EmbeddingDim(); got != 3072 {
		t.Errorf("a refused apply moved the width to %d", got)
	}

	// AND HELD ACROSS A REMOVAL, the two-step way around the guard: dropping
	// the provider must not reset the width to "never told".
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, noVectors), time.Now()); err != nil {
		t.Fatalf("apply the removal: %v", err)
	}
	if got := e.Backends().Store.EmbeddingDim(); got != 3072 {
		t.Fatalf("dropping the embeddings provider reset the width to %d; "+
			"re-adding it at another width would now be accepted and the "+
			"recall pool would hold rows of two widths", got)
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, narrow), time.Now()); err == nil {
		t.Error("removing and re-adding the provider changed the width")
	}
}

// A NODE WHOSE ROUTE PORT IS ALREADY TAKEN REFUSES TO START, naming the port.
//
// nats-server reports a route listener that cannot bind through its fatal log
// call and carries on, so without the engine acting on that call the member
// comes up healthy and never forms a route, surfacing a minute later as the
// readiness wait naming the CLUSTER and sending an operator after peers and a
// network that are fine. The refusal names the port and the setting, the one
// fact an operator cannot derive elsewhere.
func TestANodeRefusesToStartWhenItsRoutePortIsTaken(t *testing.T) {
	t.Parallel()

	// SOMETHING ELSE HOLDS IT for the length of the case, so the engine's
	// listener cannot.
	var lc net.ListenConfig
	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	taken := held.Addr().(*net.TCPAddr).Port

	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Node.ID = "node-route-taken"
		b.Stream.Cluster.Name = "crewlet-route-taken"
		b.Stream.Cluster.Host = "127.0.0.1"
		b.Stream.Cluster.Port = taken
		// A PEER THAT DOES NOT EXIST: this case fails before the readiness
		// wait, so a peer must never be what answers.
		b.Stream.Cluster.Peers = []string{"nats://127.0.0.1:1"}
	})

	backends, err := openBackends(t, b)
	if err == nil {
		backends.Close(context.Background())
		t.Fatal("a node whose route port was taken started anyway — a member " +
			"that cannot route is, from outside, indistinguishable from a slow " +
			"cluster")
	}
	// THE PORT AND THE FIELD ARE IN THE MESSAGE.
	if !strings.Contains(err.Error(), strconv.Itoa(taken)) {
		t.Errorf("the failure does not name the port that was taken: %v", err)
	}
	if !strings.Contains(err.Error(), "stream.cluster.port") {
		t.Errorf("the failure does not name the field to change: %v", err)
	}
}
