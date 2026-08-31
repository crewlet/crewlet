package engine_test

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
			b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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

func TestNoBootstrapIsRefusedByNew(t *testing.T) {
	t.Parallel()
	if _, err := engine.New(t.Context(), engine.Options{
		Company: parsedCompany(t, companyDoc),
	}); err == nil {
		t.Error("an engine built with no bootstrap config")
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
				res := e.Dispatch(context.Background(), "ceo",
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
	got := e.Dispatch(context.Background(), "ceo", []*events.Event{ev("external_notification")})
	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a defer on an unowned seat", got.Outcome)
	}
}

func TestAwaitingSandboxParksRatherThanRunning(t *testing.T) {
	t.Parallel()
	// A detached coding run outlasts any broker ack window, so its seat's
	// mail is requeued rather than held against the ack deadline. Nil
	// answers no — a build with no sandbox provider has no seat waiting on
	// one — so this is the half that says the hook is read at all.
	var parked [][]*events.Event
	d := &engine.Dispatcher{
		// Ownership is the guard above this one; pin it so what is under
		// test is the sandbox branch and not the lease table.
		Conditions: nil,
		Park: func(_ context.Context, _ string, evs []*events.Event) error {
			parked = append(parked, evs)
			return nil
		},
	}
	e := newEngine(t, engine.Options{
		Dispatch:        d,
		AwaitingSandbox: func(handle string) bool { return handle == "ceo" },
	})
	// The seat host has claimed nothing, so ownership would defer first.
	// Override just that answer, keeping the engine's own sandbox wiring.
	inner := d.Conditions
	d.Conditions = func(h string) inbox.Conditions {
		c := inner(h)
		c.Owned = true
		return c
	}

	if got := e.Dispatch(context.Background(), "ceo",
		[]*events.Event{ev("external_notification")}); got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack after a park", got.Outcome)
	}
	if len(parked) != 1 {
		t.Fatalf("parked %d partitions, want 1", len(parked))
	}
	if got := e.Dispatch(context.Background(), "cto",
		[]*events.Event{ev("external_notification")}); got.Outcome == queue.OutcomeAck &&
		len(parked) != 1 {
		t.Error("a seat waiting on nothing was parked too")
	}
}

// --- lifetime -------------------------------------------------------------- //

func TestBorrowedBackendsOutliveTheEngine(t *testing.T) {
	t.Parallel()
	// The merged topology: the API process and the engine share one
	// broker, and the API outlives the engine's own shutdown. An engine
	// that closed what it was lent would take the API's broker with it.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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

func TestOwnedBackendsCloseWithTheEngine(t *testing.T) {
	t.Parallel()
	// The counterfactual. "Does not close what it was lent" is satisfied
	// just as well by "never closes anything", which leaks a broker and a
	// database file per engine.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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

	if err := e.Backends().Queue.Publish(t.Context(), topics.AgentInbox("ceo"),
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

func TestCancellingTheStartContextDoesNotStopTheEngine(t *testing.T) {
	t.Parallel()
	// Stop is what ends an engine. The seat host derives its heartbeat and
	// sweep loops from whatever Start is given, so an engine started on a
	// signal context would stop renewing its leases the instant SIGTERM
	// arrived — before the drain began. The drain then waits for in-flight
	// turns while every lease lapses at the TTL and peers claim seats this
	// node is still running turns on.
	//
	// The TTL is shortened so the lapse would happen inside a test rather
	// than in forty-five seconds. The heartbeat follows it, which is what
	// makes a short TTL workable at all.
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Coordination.LeaseTTLSeconds = 0.9
	})
	e := newEngine(t, engine.Options{Bootstrap: b})

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the seats to be claimed", func() bool {
		return len(e.Node().Host().Held()) == 2
	})

	cancel()
	// Comfortably past the lease TTL: an engine whose loops died with the
	// context has stopped renewing, so the leases have expired.
	time.Sleep(2 * time.Second)

	if got := e.Node().Host().Held(); len(got) != 2 {
		t.Errorf("held seats = %v after the start context was cancelled, want both: "+
			"the engine stopped renewing its leases without being stopped", got)
	}
	if _, ok := e.Node().Host().MayStart("ceo"); !ok {
		t.Error("the seat host no longer considers a held seat fresh: " +
			"its heartbeat died with the start context")
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
