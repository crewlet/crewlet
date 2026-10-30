package engine_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/seat"
)

// newEngine stands up a real engine on an embedded stream in a temp directory.
//
// Real, not a fake: what this file is about is the WIRING — which concrete
// thing ends up behind which seam — and a fake node with a fake queue would
// assert that the test's own wiring is what the test wrote.
func newEngine(t *testing.T, opts engine.Options) *engine.Engine {
	t.Helper()
	if opts.Bootstrap == nil {
		opts.Bootstrap = bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		})
	}
	if opts.Company == nil {
		opts.Company = parsedCompany(t, companyDoc)
	}
	e, err := engine.New(t.Context(), opts)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// unconfiguredEngine is [newEngine] for a node that boots with no company at
// all, which newEngine cannot build: it hands a nil company the ordinary one.
func unconfiguredEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		}),
	})
	if err != nil {
		t.Fatalf("an engine with no company was refused: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// THE SHARED FIXTURES' BROKER IS SIZED BY DECLARATION, never by the runner's
// disk.
//
// Every case here boots through these two, so a fixture that dropped the size
// would pass on any roomy runner and fail every boot on a small one — the
// regression nothing else in a passing run would show (#343). Read off the
// running broker, as the boot's own sizing reads it.
//
// Mutation: drop brokertest.StoreIn from either fixture for a bare store_dir
// and its row reads three quarters of this machine's free disk.
func TestTheSharedFixturesBrokerIsSizedByDeclaration(t *testing.T) {
	t.Parallel()
	for name, boot := range map[string]func(*testing.T) *engine.Engine{
		"newEngine": func(t *testing.T) *engine.Engine {
			return newEngine(t, engine.Options{})
		},
		"unconfiguredEngine": unconfiguredEngine,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, ok := boot(t).Backends().Queue.(*jetstream.Queue)
			if !ok {
				t.Fatal("the fixture's broker is not the embedded JetStream one")
			}
			budget, err := q.StreamBudget(t.Context())
			if err != nil {
				t.Fatalf("StreamBudget: %v", err)
			}
			if budget.Source != jetstream.BudgetServerStore ||
				budget.Limit != brokertest.StoreMaxBytes {
				t.Errorf("the fixture's broker budget = %+v, want its declared "+
					"cap of %d (brokertest.StoreMaxBytes)", budget,
					brokertest.StoreMaxBytes)
			}
		})
	}
}

func TestAnEngineBuildsEverySeam(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if e.Company() == nil || e.Node() == nil {
		t.Fatal("engine built with a missing half")
	}
	if got := len(e.Company().Seats()); got != 2 {
		t.Errorf("seats = %d, want the two agent roles", got)
	}
}

func TestABadConfigFailsBeforeAnythingIsClaimed(t *testing.T) {
	t.Parallel()
	// A node that boots on a bad config and discovers it at the first turn
	// has already told its peers it owns seats.
	_, err := engine.New(t.Context(), engine.Options{
		Bootstrap: bootstrap(t, nil),
		Company:   &config.Company{Name: ""},
	})
	if err == nil {
		t.Fatal("an invalid company built an engine")
	}
}

// A NODE WITH NO KEYRING IS REFUSED BEFORE ANYTHING OPENS — THE UNCONFIGURED
// ONE INCLUDED.
//
// The unconfigured node is the case, because it is the one nothing else
// caught: it used to start no state log until it had a company, so it met no
// signer and booted keyless, and its first applied revision was read in
// whatever shape the coordination store held. It runs the core runtime from
// boot now, and the refusal is still the keyring's own rather than a signer's
// that happens to come first. And "before anything opens" is the other
// half: the keyring is built from Tier A alone, so a refusal that came after
// the store was open would leave the file behind for nothing.
//
// Mutation: let openCipher answer nil for an empty keyring and this engine
// builds; move it back below OpenBackends and the store file exists.
func TestAKeylessNodeIsRefusedBeforeAnythingOpens(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "crewlet.db")
	_, err := engine.New(t.Context(), engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			b.Store.Path = storePath
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
			b.Secrets = config.Secrets{}
		}),
	})
	if err == nil {
		t.Fatal("an unconfigured node with no keyring was built")
	}
	if !strings.Contains(err.Error(), "secrets.keys") {
		t.Errorf("the refusal does not name the keyring: %v", err)
	}
	if _, statErr := os.Stat(storePath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the store was opened before the keyring was refused: %v", statErr)
	}
}

func TestNoBootstrapIsRefusedByNew(t *testing.T) {
	t.Parallel()
	if _, err := engine.New(t.Context(), engine.Options{
		Company: parsedCompany(t, companyDoc),
	}); err == nil {
		t.Error("an engine built with no bootstrap config")
	}
}

// AN INVALID TIER A IS REFUSED BY THE ENGINE ITSELF, before anything is opened
// — not only by the loader, which is the one caller that validates and the one
// path a bootstrap built in code, or edited after loading, never takes.
//
// The first shape is the one the e2e fleet harness actually ran on, with
// nothing to say so: a clustered stream on local coordination, where every
// member keeps its leases in its own process and claims every seat and every
// duty for itself. The second is the one it moved to next, and Tier A refuses
// it too: two embedded members, which have no coordination quorum without each
// other. Each refusal names the field an operator changes, and leaves no store
// file behind — a node that opened its store first would have migrated a
// database for a config it was always going to refuse.
func TestAnInvalidBootstrapIsRefusedBeforeAnythingIsOpened(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*config.Bootstrap)
		field  string
	}{
		{
			name: "a clustered stream on local coordination",
			mutate: func(b *config.Bootstrap) {
				b.Stream.Cluster.Name = "acme"
				b.Coordination.Type = config.CoordinationLocal
			},
			field: "coordination.type",
		},
		{
			name: "two embedded members on the embedded KV",
			mutate: func(b *config.Bootstrap) {
				b.Stream.Cluster.Name = "acme"
				b.Stream.Cluster.Port = 6222
				b.Stream.Cluster.Peers = []string{"nats://10.0.0.2:6222"}
				b.Stream.Replicas = 2
				b.Coordination.Type = config.CoordinationEmbeddedKV
			},
			field: "stream.cluster.peers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// UNSEEDED: that nothing is at the store's path afterwards
			// is the assertion.
			boot, _ := unseededBootstrap(t, func(b *config.Bootstrap) {
				brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
				tc.mutate(b)
			})
			_, err := engine.New(t.Context(), engine.Options{
				Bootstrap: boot, Company: parsedCompany(t, companyDoc),
			})
			if err == nil {
				t.Fatal("the engine built a node on a bootstrap Tier A refuses")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal does not name %s: %v", tc.field, err)
			}
			for _, path := range []string{boot.Store.Path, boot.Stream.StoreDir} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("%s exists after the refusal (%v): the engine opened "+
						"something before it checked the config", path, statErr)
				}
			}
		})
	}
}

// --- the dispatcher wiring ------------------------------------------------- //

func TestTheDispatcherIsWiredFromTheEngine(t *testing.T) {
	t.Parallel()
	// Every seam the dispatcher documents as required must be filled, and
	// filled with the real thing. A nil Ledgered makes every work key
	// derive from an empty id list, so the completion ledger records
	// nothing and drops nothing — present, and inert.
	d := &engine.Dispatcher{}
	newEngine(t, engine.Options{Dispatch: d})

	if d.Conditions == nil {
		t.Error("Conditions unwired: nothing answers whether this node owns the seat")
	}
	if d.Ledgered == nil {
		t.Error("Ledgered unwired: the completion ledger records nothing")
	}
	if d.Turn == nil {
		t.Error("Turn unwired")
	}
	if d.Park == nil {
		t.Error("Park unwired: every parking branch NAKs instead of requeuing")
	}
	if d.Pause == nil {
		t.Error("Pause unwired")
	}
	if d.NoteDeferred == nil {
		t.Error("NoteDeferred unwired: a deferred seat never resumes consuming")
	}
	if d.Completions == nil || d.Conversations == nil {
		t.Error("the ledgers were not wired to the local store")
	}
	if d.Ledgered != nil && !d.Ledgered("task_assigned") {
		t.Error("Ledgered is wired to something other than the inbox's own set")
	}
}

func TestASuppliedSeamIsKeptNotOverwritten(t *testing.T) {
	t.Parallel()
	// The regression this exists for: the wiring used to be reapplied on
	// every delivery, which both raced across seats and silently replaced
	// whatever a caller had supplied. A caller that wants to pin ownership
	// answers must still get the real ledgers.
	pinned := func(string) inbox.Conditions {
		return inbox.Conditions{Owned: false}
	}
	d := &engine.Dispatcher{Conditions: pinned}
	newEngine(t, engine.Options{Dispatch: d})

	if got := d.Conditions("ceo"); got.Owned {
		t.Error("the engine overwrote a supplied Conditions")
	}
	if d.Completions == nil {
		t.Error("supplying one seam dropped the rest")
	}
}

func TestTheWiringSurvivesConcurrentDeliveries(t *testing.T) {
	t.Parallel()
	// The node runs one consume loop per seat, so every attached seat can
	// be inside Dispatch at once. Under -race, a dispatcher whose fields
	// are assigned on the way in fails here; without -race it still fails
	// as a torn read on a loaded box, which is worse because it is rare.
	var ran atomic.Int64
	d := &engine.Dispatcher{
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		Turn: func(context.Context, engine.Request) (turn.Result, error) {
			return turn.Result{}, nil
		},
	}
	e := newEngine(t, engine.Options{Dispatch: d})

	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for range 25 {
				res := engine.MailboxTurn(context.Background(), e, "ceo",
					[]*events.Event{ev("external_notification")})
				if res.Outcome == queue.OutcomeAck {
					ran.Add(1)
				}
			}
		}(i)
	}
	for range 8 {
		<-done
	}
	if got := ran.Load(); got != 200 {
		t.Errorf("acked %d of 200 dispatches", got)
	}
}

// --- ownership ------------------------------------------------------------- //

func TestASeatThisNodeDoesNotOwnIsDeferred(t *testing.T) {
	t.Parallel()
	// Conditions reads the seat host's freshness answer, not a constant. A
	// node that has claimed nothing owns nothing, so every delivery defers
	// — and asks the host to resume the consumer when it does claim.
	e := newEngine(t, engine.Options{})
	got := engine.MailboxTurn(context.Background(), e, "ceo", []*events.Event{ev("external_notification")})
	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a defer on an unowned seat", got.Outcome)
	}
}

func TestASeatHeldBySandboxDefersRatherThanRunning(t *testing.T) {
	t.Parallel()
	// A detached coding run holds its seat for as long as it runs, so the
	// seat starts no turn: the coordinator holds its inbox, and a delivery
	// that reaches it anyway is deferred under that hold. Nil answers no — a
	// build with no sandbox provider has no seat waiting on one — so this is
	// the half that says the hook is read at all.
	var parked [][]*events.Event
	var held []string
	d := &engine.Dispatcher{
		// Ownership is the guard above this one; pin it so what is under
		// test is the sandbox branch and not the lease table.
		Conditions: nil,
		Park: func(_ context.Context, _ string, evs []*events.Event) error {
			parked = append(parked, evs)
			return nil
		},
		HoldSandbox: func(_ context.Context, handle string) { held = append(held, handle) },
	}
	e := newEngine(t, engine.Options{
		Dispatch:    d,
		SandboxRuns: func(handle string) (bool, bool) { return handle == "ceo", false },
	})
	// The seat host has claimed nothing, so ownership would defer first.
	// Override just that answer, keeping the engine's own sandbox wiring.
	inner := d.Conditions
	d.Conditions = func(h string) inbox.Conditions {
		c := inner(h)
		c.Owned = true
		return c
	}

	if got := engine.MailboxTurn(context.Background(), e, "ceo",
		[]*events.Event{ev("external_notification")}); got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a deferral under the sandbox hold", got.Outcome)
	}
	if len(parked) != 0 || len(held) != 1 {
		t.Fatalf("parked %d partitions and asked for %d holds, want no park and one hold",
			len(parked), len(held))
	}
	if got := engine.MailboxTurn(context.Background(), e, "cto",
		[]*events.Event{ev("external_notification")}); got.Outcome == queue.OutcomeDefer &&
		len(held) != 1 {
		t.Error("a seat waiting on nothing was held too")
	}
}

// --- lifetime -------------------------------------------------------------- //

func TestBorrowedBackendsOutliveTheEngine(t *testing.T) {
	t.Parallel()
	// A caller that opened the backends keeps their lifetime: it needs the
	// estate after the engine stops (a failed boot's leftovers, a peer's
	// record read back). An engine that closed what it was lent would take
	// the caller's broker with it.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	e.Stop(context.Background())

	if err := back.Queue.Publish(context.Background(), "t.after",
		ev("external_notification")); err != nil {
		t.Errorf("the engine closed a broker it was lent: %v", err)
	}
	if back.Store == nil {
		t.Error("the engine closed a store it was lent")
	}
}

// A STOPPED ENGINE APPLIES NOTHING.
//
// The reconcile loop can be mid-tick when the process is told to stop, and an
// apply that ran on after the teardown started again what the teardown had
// ended: the scheduler it had stopped, the background passes, and on a node's
// first company the inbound edge, on a node that was leaving. Lent backends
// outlive the engine's stop, which is the case where such an apply otherwise
// goes through to the end.
func TestAStoppedEngineAppliesNothing(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: b, Backends: back})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	e.Stop(context.Background())

	if _, _, err := e.Apply(t.Context(), scheduledCompany(t), time.Now()); err == nil {
		t.Fatal("a stopped engine applied a revision")
	}
	if e.Company() != nil {
		t.Error("a stopped engine published an epoch")
	}
	if e.RoutedSources() != nil {
		t.Error("a stopped engine started an inbound edge on a node that is leaving")
	}
	if e.SchedulerRunning() {
		t.Error("a stopped engine armed a scheduler after its teardown stopped it")
	}
}

// A SECOND STOP DOES NOTHING. It ran the whole teardown again, against what
// the first one had closed: the admission withdrawal among it, which warned an
// operator to exclude a node whose admission the first Stop had already
// removed. A failed start's cleanup is the ordinary way to reach a second Stop.
// Lent backends keep the coordination store open across both calls, so a
// second teardown would reach it and be counted here.
func TestASecondStopTearsNothingDownAgain(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	withdrawals := &countingWithdrawals{fleet: back.Fleet}
	back.Fleet = withdrawals

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	e.Stop(context.Background())
	if got := withdrawals.n.Load(); got != 1 {
		t.Fatalf("the first Stop withdrew the admission %d times, want once", got)
	}
	e.Stop(context.Background())
	if got := withdrawals.n.Load(); got != 1 {
		t.Errorf("a second Stop withdrew the admission again (%d withdrawals): it "+
			"ran the teardown a second time", got)
	}
}

// countingWithdrawals is a fleet that counts the admission withdrawals made
// through it.
type countingWithdrawals struct {
	fleet
	n atomic.Int32
}

// fleet names [coord.Fleet] for embedding: embedded under its own name, the
// field would collide with the interface's Fleet method.
type fleet = coord.Fleet

func (c *countingWithdrawals) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	c.n.Add(1)
	return c.fleet.ForgetAdmission(ctx, nodeID, incarnation)
}

// A PARTIAL SET IS REFUSED, NAMING WHAT IS MISSING. It used to be run: the
// engine skipped whatever needed the absent piece, so an engine lent a queue
// and no store served a company with no conversation ledger and no Worklet,
// and nothing said so. Every node `crewlet run` builds holds all four.
func TestBackendsSuppliedWithoutEveryEstateAreRefused(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	for missing, strip := range map[string]func(*engine.Backends){
		"Queue": func(p *engine.Backends) { p.Queue = nil },
		"Coord": func(p *engine.Backends) { p.Coord = nil },
		"Fleet": func(p *engine.Backends) { p.Fleet = nil },
		"Store": func(p *engine.Backends) { p.Store = nil },
	} {
		partial := *back
		strip(&partial)
		e, err := engine.New(t.Context(), engine.Options{
			Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: &partial,
		})
		if err == nil {
			e.Stop(context.Background())
			t.Errorf("backends with no %s built an engine", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the refusal does not name the missing %s: %v", missing, err)
		}
	}
}

func TestOwnedBackendsCloseWithTheEngine(t *testing.T) {
	t.Parallel()
	// The counterfactual. "Does not close what it was lent" is satisfied
	// just as well by "never closes anything", which leaks a broker and a
	// database file per engine.
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	back := e.Backends()
	db := back.Store
	e.Stop(context.Background())

	if err := db.SQL().PingContext(context.Background()); err == nil {
		t.Error("the engine left its own store open")
	}
	if err := back.Queue.Publish(context.Background(), "t.after",
		ev("external_notification")); err == nil {
		t.Error("the engine left its own broker running")
	}
}

func TestStartAndStopAreSurvivable(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The seat host claims what it can; with local coordination and no
	// peers that is every seat.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.Node().Host().Held()) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.Node().Host().Held(); len(got) != 2 {
		t.Errorf("held seats = %v, want both", got)
	}
	e.Stop(context.Background())
	if got := e.Node().Host().Held(); len(got) != 0 {
		t.Errorf("a stopped engine still holds %v", got)
	}
}

func TestStopLetsAnInFlightTurnFinish(t *testing.T) {
	t.Parallel()
	// The drain is the difference between a restart that resumes cleanly
	// and one that abandons work: it quiesces every held seat so no NEW
	// work is taken, then waits — bounded only by the caller's context —
	// for the turns already running.
	//
	// Without it the only wait left is the queue's own stop grace, a
	// quarter of a second sized for a consume loop to notice its
	// cancellation, not for a turn to finish. A turn parked mid-LLM-round
	// is making progress no timer can see.
	entered := make(chan struct{})
	var once sync.Once
	var finished, finishedBeforeStopReturned atomic.Bool

	d := &engine.Dispatcher{
		Turn: func(context.Context, engine.Request) (turn.Result, error) {
			once.Do(func() { close(entered) })
			time.Sleep(turnLength)
			finished.Store(true)
			return turn.Result{}, nil
		},
	}
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Dispatch: d,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(e.Node().Host().Held(), "ceo")
	})

	if err := e.Backends().Queue.Publish(t.Context(), seatInbox(e, "ceo"),
		ev("external_notification")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never started")
	}

	e.Stop(context.Background())
	finishedBeforeStopReturned.Store(finished.Load())

	if !finishedBeforeStopReturned.Load() {
		t.Error("Stop returned while a turn was still running: the shutdown " +
			"abandoned work that was already under way")
	}
}

// THE DRAIN IS ITS OWN HALF, and the HTTP surface lives in the gap after it.
//
// A process serves /health and /ready through the drain and closes its
// listener between Drain and Stop, which only works if the drain says so from
// its first moment, leaves every backend the probes read open when it
// returns, and happens once however many callers ask. Each of those is
// checked against a turn held in flight, because a drain with nothing to wait
// for finishes before anything could observe it.
func TestADrainSaysSoAtOnceAndLeavesTheBackendsOpen(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d := &engine.Dispatcher{
		Turn: func(context.Context, engine.Request) (turn.Result, error) {
			once.Do(func() { close(entered) })
			<-release
			return turn.Result{}, nil
		},
	}
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Dispatch: d,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Released on every path, so a failed assertion below cannot leave the
	// cleanup's Stop waiting on a turn nobody will let go.
	var released sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	t.Cleanup(func() { letGo(); e.Stop(context.Background()) })

	var stops atomic.Int32
	e.Backends().Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if ev.Type == (types.OrgStopped{}).EventType() {
			stops.Add(1)
		}
	})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e.ShuttingDown() {
		t.Fatal("a running engine reports that it is shutting down")
	}
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(e.Node().Host().Held(), "ceo")
	})
	if err := e.Backends().Queue.Publish(t.Context(), seatInbox(e, "ceo"),
		ev("external_notification")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never started")
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		e.Drain(context.Background())
	}()
	// FROM THE FIRST MOMENT, while the turn it waits for is still running:
	// this is the flag the HTTP surface refuses new work on, and a flag set
	// once the drain was over would refuse nothing.
	waitFor(t, "the engine to say it is shutting down", e.ShuttingDown)
	select {
	case <-drained:
		t.Fatal("the drain returned while a turn was still running")
	default:
	}

	letGo()
	select {
	case <-drained:
	case <-time.After(15 * time.Second):
		t.Fatal("the drain did not return once the turn finished")
	}
	// DRAINED, NOT STOPPED: the probes a process serves in this gap read
	// the broker, and a drain that closed it would leave them nothing.
	if err := e.Backends().Queue.Publish(context.Background(), "t.after",
		ev("external_notification")); err != nil {
		t.Errorf("the broker was closed by the drain rather than by Stop: %v", err)
	}
	if held := e.Node().Host().Held(); len(held) != 0 {
		t.Errorf("a drained engine still holds %v", held)
	}

	e.Stop(context.Background())
	if err := e.Backends().Queue.Publish(context.Background(), "t.after",
		ev("external_notification")); err == nil {
		t.Error("Stop after a drain left the engine's own broker running")
	}
	if got := stops.Load(); got != 1 {
		t.Errorf("the stop was announced %d times for one shutdown, want once", got)
	}
}

// A DRAIN REFUSES THE NEXT APPLY, and it is the drain rather than the teardown
// that refuses it.
//
// The gate is one lock and a flag, so what a test can pin is WHERE it sits: an
// apply attempted after Drain has returned, with every backend still open and
// Stop not yet called, must already be refused. The reconcile tick is
// synchronous and can be mid-apply when a signal lands, and an apply admitted
// during a drain re-arms the scheduler, the background passes and a first
// company's inbound edge on a node that is handing its seats back. It is also
// the only gate that sees an apply arriving over the STREAM from a peer, which
// the API's own drain gate never does.
func TestADrainRefusesAnApplyBeforeTheTeardownDoes(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc),
		Dispatch: &engine.Dispatcher{},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// SERVING FIRST, so the refusal below is the drain's and not a node
	// that was never able to apply.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("an apply on a serving node was refused: %v", err)
	}

	e.Drain(context.Background())

	// DRAINED, NOT STOPPED: teardown has not run, the broker is open, and
	// the apply must already be refused.
	if err := e.Backends().Queue.Publish(context.Background(), "t.after",
		ev("external_notification")); err != nil {
		t.Fatalf("the drain closed the broker, so this case is not testing "+
			"what it claims: %v", err)
	}
	if _, _, err := e.Apply(context.Background(), parsedCompany(t, companyDoc), time.Now()); err == nil {
		t.Error("a drained node applied a revision: the apply gate is on the " +
			"teardown rather than on the drain, so the reconcile tick can " +
			"re-arm everything the drain is handing back")
	}
}

// turnLength is how long the in-flight turn above takes.
//
// Comfortably longer than the queue's own stop grace, which is a quarter of a
// second: the point is that the drain waits for the TURN, not that it happens
// to outlast a wait sized for something else.
const turnLength = 2 * time.Second

// waitFor polls until cond holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startedLeaseTTL is the lease TTL a case here STARTS an engine at when it
// shortens the lease: the shortest a deployment may set ([config.MinLeaseTTL]),
// because [engine.New] holds every bootstrap to it.
//
// THE TTL IS ALSO THE WATCHDOG'S THRESHOLD. [engine.New] arms the seat
// watchdog at the host's own TTL ([seat.Watchdog] says why the two are one
// number) and [engine.Engine.Start] starts it; once the host goes longer than
// that without a beat it ends the PROCESS with status 75 — here the test
// binary, and every case running in it. So a TTL shortened to make a lapse or
// an allowance quick inside one case is also how close the whole binary sits
// to that exit on a loaded runner, and the floor's margin, measured on this
// package's own engines under load, is the doc of [config.MinLeaseTTL]: it
// used to be stated here, as the shortest TTL a case started an engine at,
// before the floor existed and two cases here started theirs at three seconds.
//
// So no case waits out what it needs a short TTL for: it reads the renew or
// the deadline it is about off the host or its store. A case whose subject is
// a fraction of a shorter TTL builds an Engine literal rather than booting one
// (TestAHoldGivenBackDuringTheTeardownIsAStepOfItsAllowance runs at 1.5 s).
const startedLeaseTTL = config.MinLeaseTTL

// CANCELLING THE START CONTEXT DOES NOT STOP THE ENGINE.
//
// Stop is what ends an engine. The seat host derives its heartbeat and sweep
// loops from whatever Start is given, so an engine started on a signal context
// would stop renewing its leases the instant SIGTERM arrived — before the drain
// began. The drain then waits for in-flight turns while every lease lapses at
// the TTL and peers claim seats this node is still running turns on.
//
// A RENEW BEGUN AFTER THE CANCEL IS THE EVIDENCE, read where the host judges
// one: [seat.Host.MayStart] admits a seat only while its last renew is within
// one heartbeat, measured from a stamp read before the store call it records.
// So a yes more than a heartbeat past the cancel is a renew begun after it. A
// host whose loops died with the context began its last renew no later than
// the cancel and refuses every seat from one heartbeat past it, for good; a
// live one renews every heartbeat and admits again within the next. The case
// polls each seat from one heartbeat past the cancel for one heartbeat more.
//
// IT WAITS FOR NO LEASE TO LAPSE. It slept two TTLs and then read the host,
// which kept it at a TTL of three seconds — and the TTL is also the threshold
// at which the engine's watchdog ends the whole test binary ([startedLeaseTTL]
// has the measurement). So against the regression it names it never failed:
// measured, the watchdog ended the binary with status 75, its host 3.3 s
// without a beat, three seconds before the case would have read anything, and
// took every case running beside it down too. Read off the renews, the window
// closes two heartbeats past the cancel, and a host whose loops died beat last
// no earlier than one beat before it, so its watchdog fires no sooner than a
// TTL less a beat past the cancel — later, which the case checks rather than
// assumes. The regression is reported here, and the cleanup's Stop disarms the
// watchdog before it can end the binary.
//
// Mutation: start the node on the caller's context in [engine.Engine.Start],
// and this fails naming both seats while the binary lives on.
func TestCancellingTheStartContextDoesNotStopTheEngine(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		b.Coordination.LeaseTTLSeconds = startedLeaseTTL.Seconds()
	})
	e := newEngine(t, engine.Options{Bootstrap: b})

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	host := e.Node().Host()
	// THE PREMISE: both seats held AND admitted, so a refusal after the
	// cancel can only be a renew that never came, never a seat still
	// establishing.
	waitFor(t, "both seats to be claimed and admitted", func() bool {
		held := host.Held()
		return len(held) == 2 && !slices.ContainsFunc(held, func(handle string) bool {
			_, fresh := host.MayStart(handle)
			return !fresh
		})
	})
	seats := host.Held()
	heartbeat := host.HeartbeatInterval()
	// THE PREMISE, checked rather than commented: the window closes before
	// the watchdog could end the binary over a host whose loops died, whose
	// last beat is at most one beat older than the cancel.
	if reach := host.TTL() - seat.WatchdogBeatInterval; 2*heartbeat >= reach {
		t.Fatalf("the premise: the window closes %v past the cancel, past the %v "+
			"at which the watchdog may end the binary over a dead heartbeat",
			2*heartbeat, reach)
	}

	cancel()
	cancelled := time.Now()
	// FROM ONE HEARTBEAT PAST THE CANCEL: before it, a yes may be a renew
	// begun before the cancel, which proves nothing.
	time.Sleep(time.Until(cancelled.Add(heartbeat)))
	until := cancelled.Add(2 * heartbeat)
	stale := slices.Clone(seats)
	for len(stale) > 0 && time.Now().Before(until) {
		stale = slices.DeleteFunc(stale, func(handle string) bool {
			_, fresh := host.MayStart(handle)
			return fresh
		})
		time.Sleep(20 * time.Millisecond)
	}
	if len(stale) > 0 {
		t.Errorf("no renew of %v began after the start context was cancelled: from "+
			"one heartbeat (%v) past the cancel to two, the seat host refused them as "+
			"stale, so its heartbeat died with the context it was started on",
			stale, heartbeat)
	}
	if got := host.Held(); !slices.Equal(got, seats) {
		t.Errorf("held seats = %v after the start context was cancelled, want %v: "+
			"the engine stopped holding its seats without being stopped", got, seats)
	}
}

// THE POSTURE GATE REACHES A SEAT'S OWN INBOX.
//
// The regression this exists for: conditionsFor asserted AdmitsTriggers as a
// hardcoded true, so a seat's own mailbox was the one trigger path a shed
// could never reach — the screening's "config posture refuses new work" branch
// existed and was unreachable. Both halves are checked, because a gate that is
// always closed is as broken as one that is always open.
func TestThePostureGateReachesTriggerAdmission(t *testing.T) {
	t.Parallel()
	d := &engine.Dispatcher{}
	e := newEngine(t, engine.Options{Dispatch: d})

	if !d.Conditions("ceo").AdmitsTriggers {
		t.Fatal("a node with no posture reporter refused its own seat's triggers")
	}

	e.SetAdmits(func() bool { return false })
	if d.Conditions("ceo").AdmitsTriggers {
		t.Error("a shedding node still admitted its own seat's triggers")
	}

	e.SetAdmits(func() bool { return true })
	if !d.Conditions("ceo").AdmitsTriggers {
		t.Error("a node that converged back never reopened trigger admission")
	}
}

// AN ENGINE RUNS WITH NO COMPANY AT ALL.
//
// The store is authoritative at runtime and a fleet's first revision may
// arrive over the API, so a node has to be able to start with nothing: serving
// its HTTP surface, holding no seats, and waiting for the control plane to
// hand it an epoch. This was refused — engine.New required a company — which
// made the whole documented "unconfigured" state unreachable from the binary:
// /health never said it, /ready never went 503 for it, and the flag that
// reported it was set to true unconditionally with no caller ever setting it
// back.
//
// Start as well as New, because the two failed at different places: New built
// the epoch, and Start read the company's name to log that it had started.
func TestAnEngineRunsUnconfigured(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if e.Company() != nil {
		t.Error("an unconfigured engine reports an epoch")
	}
	// The placement sweep asks on every tick, and an unconfigured node has
	// an empty seat set rather than an unanswerable question.
	if got := len(e.Company().Seats()); got != 0 {
		t.Errorf("seats = %d, want none", got)
	}
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("an unconfigured engine failed to start: %v", err)
	}
}

// AND ITS FIRST APPLY BRINGS IT UP. The unconfigured state is only useful if
// the node leaves it without a restart, which is the whole point of being able
// to bootstrap a fleet through PUT /config.
func TestAnUnconfiguredEngineTakesItsFirstEpoch(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("the first apply onto an unconfigured node failed: %v", err)
	}
	company := e.Company()
	if company == nil {
		t.Fatal("the node is still unconfigured after applying a revision")
	}
	if len(company.Seats()) == 0 {
		t.Error("the applied company contributed no seats")
	}
}

// A NODE ON AN IN-MEMORY STREAM IS REFUSED AT BOOT, COMPANY OR NONE, BEFORE
// ANYTHING OPENS.
//
// An in-memory stream is not "queued events do not survive a restart": the
// first restart recreates every log empty, and a node whose rows are ahead of
// it stops serving for good. Measured on a company with no Worklet
// whose pages were the engine's own: it booted, logged an error, and after one
// restart its pages applier stopped on the recreated stream and the node never
// admitted a seat again.
//
// # The node with no company is the case
//
// The rule was a cross-tier one, asked of the company a node booted with or
// was handed — so a node started with NONE passed it, and it runs the core
// runtime from boot: the identity estate its first person is invited into, on
// a log its first restart emptied. The vendor pairing is the second row
// because it is the case the rule before that one missed: a company on Jira
// and Confluence keeps no tracker or pages log, and still keeps its people.
//
// Mutation: ask the stream only when there is a company, and the first row
// builds an engine; move the question below OpenBackends, and the store file
// exists.
func TestANodeOnAnInMemoryStreamIsRefusedAtBoot(t *testing.T) {
	t.Parallel()
	vendors := parsedCompany(t, companyDoc)
	vendors.Tracker.Backend = config.TrackerNone
	vendors.Knowledge.Backend = config.KnowledgeNone
	for name, company := range map[string]*config.Company{
		"with no company":                     nil,
		"with a company on a vendor for both": vendors,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			storePath := filepath.Join(t.TempDir(), "crewlet.db")
			_, err := engine.New(t.Context(), engine.Options{
				Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
					b.Store.Path = storePath
					b.Stream.StoreDir = ""
				}),
				Company: company,
			})
			if err == nil {
				t.Fatal("a node booted on an in-memory stream — its first " +
					"restart recreates every log it runs empty")
			}
			for _, want := range []string{"stream.store_dir", "identity estate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if _, statErr := os.Stat(storePath); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the store was opened before the stream was refused: %v", statErr)
			}
		})
	}
}
