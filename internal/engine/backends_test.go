package engine_test

import (
	"context"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

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

// COORDINATION RIDES THE QUEUE'S OWN CONNECTION, on every topology.
//
// A second connection would work and would be worse: two connections to one
// broker fail independently, so a node could hold live leases over the one
// that still works while the one carrying its inbox has dropped — alive to its
// peers, deaf to its work. That is as true of the embedded broker as of an
// external one, since a connection to a server inside this process can close
// on its own like any other.
//
// OBSERVED THE ONE WAY THAT TELLS ONE CONNECTION FROM TWO: close the queue's,
// then ask the lease store and the fleet store something. Riding it, both
// fail with it. On a connection of their own they would go on answering —
// and so would an in-process lease table standing in for the KV, which is
// why the lease half is asked too.
func TestCoordinationRidesTheQueuesOwnConnection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// topology shapes the bootstrap into one of the two stream
		// branches, both with the KV holding the leases.
		topology func(t *testing.T, b *config.Bootstrap)
		// embedded is whether this node's own process runs the broker,
		// which is the one case Backends.EmbeddedConn names the
		// connection in.
		embedded bool
	}{
		{
			name: "an embedded broker",
			topology: func(t *testing.T, b *config.Bootstrap) {
				b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
			},
			embedded: true,
		},
		{
			name: "an external broker",
			topology: func(t *testing.T, b *config.Bootstrap) {
				b.Stream.Type = config.StreamNATS
				b.Stream.URL = externalBroker(t)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := bootstrap(t, func(b *config.Bootstrap) {
				tc.topology(t, b)
				b.Coordination.Type = config.CoordinationEmbeddedKV
			})
			back, err := openBackends(t, b)
			if err != nil {
				t.Fatalf("OpenBackends: %v", err)
			}
			t.Cleanup(func() { back.Close(context.Background()) })
			ctx := t.Context()

			broker, ok := back.Queue.(interface{ Conn() *nats.Conn })
			if !ok || broker.Conn() == nil {
				t.Fatalf("the stream is %T with no connection to close, and "+
					"this case needs the JetStream backend's", back.Queue)
			}
			conn := broker.Conn()

			// WHAT THE BACKUP IS HANDED. It snapshots the streams over
			// this connection, and reads nil as "the stream estate is a
			// cluster somebody else runs and backs up".
			if got := back.EmbeddedConn(); tc.embedded && got != conn {
				t.Errorf("Backends.EmbeddedConn() = %p on an embedded broker, "+
					"want the queue's own connection %p", got, conn)
			} else if !tc.embedded && got != nil {
				t.Errorf("Backends.EmbeddedConn() = %p on a dialled broker, want "+
					"nil: the backup would snapshot a cluster it does not own", got)
			}

			// A REAL LEASE STORE while the connection is open: a first
			// acquire is held, and a second owner is refused.
			lease, err := back.Coord.TryAcquire(ctx, "seat:ceo",
				coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second})
			if err != nil || lease == nil {
				t.Fatalf("a first acquire = (%v, %v), want a held lease", lease, err)
			}
			if got, err := back.Coord.TryAcquire(ctx, "seat:ceo",
				coord.AcquireOptions{Owner: "owner-2", TTL: 30 * time.Second}); err != nil {
				t.Fatalf("a second owner's acquire: %v", err)
			} else if got != nil {
				t.Fatal("two owners held one seat")
			}
			if _, err := back.Fleet.Used(ctx, coord.OrgScope); err != nil {
				t.Fatalf("the fleet store did not answer: %v", err)
			}

			conn.Close()

			if lease, err := back.Coord.TryAcquire(ctx, "seat:cto",
				coord.AcquireOptions{Owner: "owner-1", TTL: 30 * time.Second}); err == nil {
				t.Errorf("the lease store answered (%v) after the queue's "+
					"connection closed: it is not riding that connection, so "+
					"this node could go on holding seats whose inbox it can no "+
					"longer read", lease)
			}
			if used, err := back.Fleet.Used(ctx, coord.OrgScope); err == nil {
				t.Errorf("the fleet store answered (%d) after the queue's "+
					"connection closed: it holds a connection of its own", used)
			}
		})
	}
}

// externalBroker starts a NATS server this node DIALS, which is what
// `stream.type: nats` means: a broker somebody else runs, listening on a
// socket. It answers the URL to put in stream.url.
//
// Its max_payload is the contract's own, because the fleet store refuses to
// open on a server below it — and a case failing on that would be testing the
// refusal rather than the topology.
func externalBroker(t *testing.T) string {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		ServerName: "external-broker",
		Host:       "127.0.0.1",
		Port:       -1,
		JetStream:  true,
		StoreDir:   t.TempDir(),
		MaxPayload: queue.MaxPayloadBytes,
		NoSigs:     true,
	})
	if err != nil {
		t.Fatalf("configure the external broker: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the external broker did not accept connections")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns.ClientURL()
}

// A SEAT KEEPS ITS MEMORY WHEN IT MOVES BETWEEN NODES ON AN EXTERNAL BROKER.
//
// The memory changelog is a stream on the broker every node shares, dialled
// as much as embedded. Given a connection only where the broker is embedded,
// a node on `stream.type: nats` would carry nothing: its seats would flush
// nothing when released and hydrate nothing when claimed, so a seat would
// arrive on its next node with an empty memory and nothing would say so.
//
// Observed the way it fails: a seat learns something on one node, that node
// hands its seats back in a graceful stop, and a second node — its own store,
// the same broker — claims the seat and must hold what the first one learned.
func TestASeatsMemoryMovesWithItOnAnExternalBroker(t *testing.T) {
	t.Parallel()
	url := externalBroker(t)
	onTheBroker := func(nodeID string) *engine.Engine {
		t.Helper()
		return newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			b.Node.ID = nodeID
			b.Stream.Type = config.StreamNATS
			b.Stream.URL = url
			b.Coordination.Type = config.CoordinationEmbeddedKV
		})})
	}

	first := onTheBroker("node-first")
	claimed(t, first, 2)
	const episode = "episode-learned-on-the-first-node"
	at := time.Now().UTC().UnixMicro()
	if _, err := first.Backends().Store.SQL().ExecContext(t.Context(),
		`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		     ended_at, plan_summary, task_summary, review_outcome, duration_ms)
		 VALUES (?, 'ceo', 'CEO', 'turn-1', ?, ?, 'plan', 'the release train is thursdays',
		     'done', 1)`, episode, at, at); err != nil {
		t.Fatalf("seed the seat's memory on the first node: %v", err)
	}
	// A GRACEFUL STOP hands every seat back, and a seat's release is where
	// what it learned since the last cycle reaches the changelog.
	first.Stop(context.Background())

	second := onTheBroker("node-second")
	claimed(t, second, 2)
	// POLLED, because a seat counts as held while its acquire hook — where
	// it hydrates — is still running.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var summary string
		err := second.Backends().Store.SQL().QueryRowContext(t.Context(),
			`SELECT task_summary FROM episodes WHERE id = ?`, episode).Scan(&summary)
		if err == nil {
			if summary != "the release train is thursdays" {
				t.Errorf("the carried episode says %q", summary)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seat moved to a second node on the same external broker "+
				"and arrived without what it learned on the first (%v): its memory "+
				"did not travel on this topology", err)
		}
		time.Sleep(50 * time.Millisecond)
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

func TestAnEmbeddedStreamPersistsAcrossARestart(t *testing.T) {
	t.Parallel()
	// StoreDir is actually USED. Ignoring it selects an in-memory server,
	// which starts and works and loses everything on restart — the failure
	// a company only discovers the first time it restarts.
	//
	// It does NOT show that Close shuts the first server down. Two embedded
	// servers share one store directory without complaint, so the second
	// open below succeeds with the first still running — measured, by
	// taking the shutdown out of Close and watching this case stay green.
	// TestAStoreThatCannotOpenTakesTheBrokerDownWithIt is what covers it.
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

	// A second server on the same directory, which is what a restart is.
	second := mk()
	t.Cleanup(func() { second.Close(t.Context()) })
	if err := second.Queue.Start(t.Context()); err != nil {
		t.Fatalf("the second open on %s could not start: %v", dir, err)
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

// NOTE ON WHAT THIS SUITE CANNOT SEE.
//
// One effect of Close is outside this package's reach: stopping the QUEUE
// before the server hands a consumer's in-flight message back at once, rather
// than leaving it to wait out the broker's ack timeout, and that buys a PEER a
// fast handoff. In one process the shut-down server kills the connection
// either way, so there is nothing to observe; two nodes and one broker would
// show it, which is the fleet suite's shape.
//
// The rest of both steps IS seen here, and mutation confirms it: taking the
// queue stop out of Close turns TestTheStoreOutlivesTheHandlersThatWriteToIt
// red, and taking the server shutdown out turns
// TestAStoreThatCannotOpenTakesTheBrokerDownWithIt red. Each is stated at its
// site in backends.go.

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

// NO COMPANY OPENS AT WIDTH 0, because a node has to open its store before it
// can read the company out of it — and a node with no active revision has no
// company to be asked for at all.
//
// This was REFUSED while the width was fixed at open, and rightly: a store
// stuck at the wrong width refuses every write from the right one, and recall
// stops returning anything with no reason in the log. What makes it safe now
// is that the width is re-stated by every apply (see
// TestTheEmbeddingWidthFollowsAConfigApply), so the first revision this node
// applies corrects it. If that ever stops being true, this has to go back to
// being a refusal.
func TestNoCompanyOpensAtWidthZero(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := engine.OpenBackends(t.Context(), b, nil)
	if err != nil {
		t.Fatalf("a nil company was refused: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if got := back.Store.EmbeddingDim(); got != 0 {
		t.Errorf("embedding width = %d, want 0 until an apply says otherwise", got)
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
	// The margin here is not delicate: a broker left running keeps every
	// goroutine it started, which is many times the threshold, and a broker
	// that was shut down leaves the count where it began. See leakThreshold.
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

	// leakThreshold separates the two outcomes: every broker shut down,
	// which leaves the goroutine count where it began, and a broker left
	// running, which leaves its whole goroutine population behind — well
	// past this for a single broker, before failedOpenAttempts multiplies
	// it. Not zero, which would fail the test on any unrelated background
	// goroutine that happens to still be settling.
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

func TestTheStoreTakesItsPoolKnobsFromConfig(t *testing.T) {
	t.Parallel()
	// Both are Tier A knobs an operator sets and nothing else reports. A
	// busy timeout ignored means statements giving up on lock contention
	// far sooner than asked, and a pool bound ignored means the dashboard's
	// read burst landing on however many connections database/sql felt
	// like opening.
	//
	// The driver used to be the third knob here and is gone with it:
	// there is one driver, and TestTursoIsTheOnlyDriverInTheBinary in
	// internal/store is what asserts that now.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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
	if got := back.Store.SQL().Stats().MaxOpenConnections; got != 3 {
		t.Errorf("max open conns = %d, want the configured 3", got)
	}
}

func TestAnUnsetTimeoutTakesTheStoreDefault(t *testing.T) {
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

// THE WIDTH IS LEARNED ONCE AND THEN HELD.
//
// The width belongs to the vectors already in the file, not to the current
// config, so it is not something an apply may change: buildEmbedder refuses a
// revision whose width differs from the one the store was opened at and tells
// the operator to restart. A store that was never told a width is the one
// exception — a node that booted with no active revision, holding no rows —
// and it learns from its first epoch.
//
// The direction that must NOT work is the way back. Were the width simply
// re-stated on every apply, dropping the embeddings provider would set it to 0
// and turn that guard off, and re-adding the provider at a different width
// would then be accepted — putting rows of two widths in one recall pool,
// which the reader can match neither of. Zero is "no declared width", and
// EncodeVector checks nothing against it, so nothing would report this.
func TestTheEmbeddingWidthIsLearnedOnceAndHeld(t *testing.T) {
	// Not parallel: t.Setenv resolves the ${K} these documents reference.
	const noVectors = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`
	const wide = `
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
	narrow := strings.Replace(wide, "dimensions: 3072", "dimensions: 1536", 1)
	narrow = strings.Replace(narrow, "text-embedding-3-large", "text-embedding-3-small", 1)

	t.Setenv("K", "test-key")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, noVectors)})
	if got := e.Backends().Store.EmbeddingDim(); got != 0 {
		t.Fatalf("booted at width %d, want 0 for a company with no embeddings", got)
	}

	// LEARNED: the unconfigured node taking its first revision. Nothing was
	// being refused at 0 — the dimension guard was simply off.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, wide)); err != nil {
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

	// HELD: a revision that would change it is refused, by the guard that
	// already existed. This is what the learn-once rule protects.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, narrow)); err == nil {
		t.Error("a revision changing the store's width was applied")
	}
	if got := e.Backends().Store.EmbeddingDim(); got != 3072 {
		t.Errorf("a refused apply moved the width to %d", got)
	}

	// AND HELD ACROSS A REMOVAL, which is the two-step way around the guard.
	// Dropping the provider must not reset the width to "never told".
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, noVectors)); err != nil {
		t.Fatalf("apply the removal: %v", err)
	}
	if got := e.Backends().Store.EmbeddingDim(); got != 3072 {
		t.Fatalf("dropping the embeddings provider reset the width to %d; "+
			"re-adding it at another width would now be accepted and the "+
			"recall pool would hold rows of two widths", got)
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, narrow)); err == nil {
		t.Error("removing and re-adding the provider changed the width")
	}
}

// A NODE WHOSE ROUTE PORT IS ALREADY TAKEN REFUSES TO START, naming the port.
//
// # Why this needs a guard of its own
//
// Because nats-server does not fail on it. A member whose configured route
// port is held by something else logs the listener error and carries on
// serving clients: the node comes up, answers its health check, and simply
// never forms a route to a peer. What an operator sees a minute later is the
// readiness wait expiring and naming the CLUSTER — "JetStream has not
// established contact with a meta leader" — which sends them to debug peers, a
// firewall and a network path that are all fine.
//
// There is no other symptom. The port is the only thing that knows.
func TestANodeRefusesToStartWhenItsRoutePortIsTaken(t *testing.T) {
	t.Parallel()

	// SOMETHING ELSE HOLDS IT, which is the whole input. Held for the
	// length of the case, so the engine's listener cannot have it.
	var lc net.ListenConfig
	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	taken := held.Addr().(*net.TCPAddr).Port

	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Node.ID = "node-route-taken"
		b.Stream.Cluster.Name = "crewlet-route-taken"
		b.Stream.Cluster.Host = "127.0.0.1"
		b.Stream.Cluster.Port = taken
		// A PEER THAT DOES NOT EXIST, deliberately: this case is about
		// failing before the readiness wait, so the peer must never be
		// the thing that answers.
		b.Stream.Cluster.Peers = []string{"nats://127.0.0.1:1"}
	})

	backends, err := openBackends(t, b)
	if err == nil {
		backends.Close(context.Background())
		t.Fatal("a node whose route port was taken started anyway — it serves " +
			"clients, answers health checks and never forms a route, which " +
			"from outside is indistinguishable from a slow cluster")
	}
	// THE PORT IS IN THE MESSAGE, because that is the one fact an operator
	// cannot derive from anywhere else.
	if !strings.Contains(err.Error(), strconv.Itoa(taken)) {
		t.Errorf("the failure does not name the port that was taken: %v", err)
	}
	if !strings.Contains(err.Error(), "stream.cluster.port") {
		t.Errorf("the failure does not name the field to change: %v", err)
	}
}
