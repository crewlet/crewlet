package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat"
)

// A STOP AGAINST A COORDINATION STORE THAT HAS GONE AWAY SPENDS ONE ALLOWANCE.
//
// A member that has lost its coordination store still stops in order: it
// gives its presence and its seats back, releases its duties and withdraws its
// admission — and every one of those fails. The leases fall back to lapsing on
// their TTL; the admission has no TTL and falls back to nothing, so it runs on
// a share of the allowance no step before it can spend — carved out of the
// allowance rather than added to it. Each step used to wait out a bound of its
// own, so the stop spent their sum, close to an orchestrator's kill grace and
// past it once the flushes behind them ran. Here every round trip to the lease
// and fleet stores stalls for five seconds once the stop begins, against a
// lease TTL of three — an allowance of one second, the admission's share
// included — so the old sum is past half a minute and one allowance is a
// second. (The stream stays up: the native backends this company runs are
// reached through the broker's own client, which a stand-in cannot be.)
//
// A RECONCILE PASS IS IN FLIGHT WHEN THE STOP BEGINS, under the hold on its
// surface: the integration loop's first grant is held back until then. The
// teardown waits for that pass, and the pass gives its hold back on the
// context the hold was taken on — long before the stop — so that give-back
// sat out a whole stalled round trip beside the allowance until the engine
// bound it to the stop. Held rather than left to chance, because the loop's
// first pass runs as it starts and is over by the stop on an idle machine, so
// a case that waited for luck exercised the give-back only under load.
//
// THE STORE GOES AWAY AS THE STOP BEGINS — the moment its drain is decided
// ([engine.Engine.ShuttingDown]) — rather than a moment before it, because the
// subject is the round trips the STOP makes. One begun earlier ends on its own
// client's bound like any round trip in flight when a stop begins, and a case
// that took the store away first measured whether one of those happened to
// start in between.
func TestAStopAgainstAnUnreachableStoreSpendsOneAllowance(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Coordination.LeaseTTLSeconds = 3
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	var stopped atomic.Pointer[engine.Engine]
	gone := func() bool {
		e := stopped.Load()
		return e != nil && e.ShuttingDown()
	}
	pass := &heldPass{granted: make(chan struct{})}
	back.Coord = stallingLeases{coordBackend: back.Coord, gone: gone, pass: pass}
	back.Fleet = stallingFleet{fleet: back.Fleet, gone: gone}

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	stopped.Store(e)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(e.Node().Host().Held()) == 0 {
		t.Fatal("the premise: the node holds seats for its stop to give back")
	}
	select {
	case <-pass.granted:
	case <-time.After(30 * time.Second):
		t.Fatal("the premise: the integration loop never took a surface's hold, " +
			"so no pass is in flight for the stop to wait on")
	}

	started := time.Now()
	e.Stop(context.Background())
	took := time.Since(started)
	allowance := seat.StopAllowance(3 * time.Second)
	// THE ALLOWANCE, and room for the local teardown around it: one stalled
	// round trip alone is five seconds, so a stop that left even one of its
	// steps to a bound of its own would be past this.
	if limit := allowance + 3*time.Second; took > limit {
		t.Fatalf("the stop took %v against a store that answers nothing, past "+
			"its %v allowance and %v of local teardown: its round trips each "+
			"waited out a bound of their own", took, allowance, limit-allowance)
	}
}

// errGone is what the stalled store answers once its stall is over.
var errGone = errors.New("the coordination store has lost quorum")

// stall waits out a round trip to a store that has gone away: until the
// request's context ends, or five seconds — a client's own timeout.
func stall(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errGone
	}
}

type coordBackend = coord.Backend

// stallingLeases is a lease store whose reads and releases stall once gone
// answers true, and which holds back its first grant of an integration
// surface's hold until then ([heldPass]).
type stallingLeases struct {
	coordBackend
	gone func() bool
	pass *heldPass
}

// heldPass is the reconcile pass a stop begins in the middle of: granted is
// closed when the first surface hold is taken, and that hold reaches its pass
// only once the stop has begun.
type heldPass struct {
	once    sync.Once
	granted chan struct{}
}

func (l stallingLeases) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {
	lease, refusal, err := l.coordBackend.TryAcquire(ctx, resource, opts)
	// EVERY SURFACE IS HELD UNDER `setup-provision-<kind>`, the one name a
	// reconcile pass and an operator's pass both take.
	if err != nil || lease == nil ||
		!strings.HasPrefix(resource, coord.WorkerResource("setup-provision-")) {
		return lease, refusal, err
	}
	first := false
	l.pass.once.Do(func() { first = true; close(l.pass.granted) })
	for first && !l.gone() {
		select {
		case <-ctx.Done():
			return lease, refusal, err
		case <-time.After(5 * time.Millisecond):
		}
	}
	return lease, refusal, err
}

func (l stallingLeases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if l.gone() {
		return false, stall(ctx)
	}
	return l.coordBackend.Release(ctx, resource, owner, epoch)
}

func (l stallingLeases) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if l.gone() {
		return nil, stall(ctx)
	}
	return l.coordBackend.Get(ctx, resource)
}

// stallingFleet is a fleet store whose admission withdrawal stalls once
// gone answers true.
type stallingFleet struct {
	fleet
	gone func() bool
}

func (f stallingFleet) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	if f.gone() {
		return stall(ctx)
	}
	return f.fleet.ForgetAdmission(ctx, nodeID, incarnation)
}

// A STOP ON A STREAM THAT WILL NOT ACKNOWLEDGE LEAVES NOTHING HELD.
//
// The event stream and the coordination buckets are separately replicated
// streams, so the stream can lose its quorum while the store answers. A stop's
// last events are publishes on it — the node's `org_stopped` and each seat's
// `agent_terminated` — and a publish the stream never acknowledges waits out
// whatever deadline it is handed. As steps of the stop's allowance they spent
// it, and the presence, both seats, the admission and every duty were left
// held against a store that was answering all along: the presence and the
// seats until their TTL, the duties for theirs, and the admission — which has
// none — until an operator excluded the node. Here the stream holds both kinds
// of event exactly as its client does, until the request's deadline or five
// seconds without one, and the store is untouched: after the stop, nothing of
// this node's is held.
//
// AND A SEAT'S LAST EVENT IS OVER BEFORE ITS LEASE IS GIVEN BACK, published
// beside the seat's teardown but never past it: a peer that takes the seat
// over then announces it after this node let it go, where the other order
// shows the live projection a running seat as terminated.
//
// AND IT RUNS BESIDE THE SEAT'S TEARDOWN, not in front of it. Each seat holds
// a memory row here, and the memory changelog acknowledges the release's last
// publish of it only after [teardownHold], so each seat's teardown takes
// seconds of its own. Beside it, the event costs the seat no time the teardown
// is not already spending; in front of it, every seat's lease waits for the
// sum of the two.
//
// AND THE ANNOUNCEMENT RUNS BESIDE THE DRAIN, so the stream's silence costs
// the stop one of its bounds while the seats' run, not one after the other.
//
// The lease TTL is fifteen seconds — an allowance of five — so a lease a stop
// failed to give back is still held when the case reads it, seconds later.
func TestAStopOnAStreamThatWillNotAcknowledgeLeavesNothingHeld(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		// THE BROKER'S RESERVATIONS FROM A FIXED SIZE rather than from
		// whatever this machine's disk has free, which is a fact about
		// the machine and not about the stop.
		b.Stream.StoreMaxBytes = 16 << 30
		b.Coordination.LeaseTTLSeconds = 15
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	broker, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the premise: the engine's broker is JetStream, got %T", back.Queue)
	}
	stream := &unacknowledgedLastRecords{Queue: broker}
	back.Queue = stream
	leases := &recordedReleases{coordBackend: back.Coord}
	back.Coord = leases

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	stream.engine.Store(e)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	node := e.Node().ID()
	ctx := context.Background()
	// THE PREMISE: everything the stop must give back is held first. A case
	// that stopped before a duty was claimed would find none held afterwards
	// whatever the stop did.
	waitFor(t, "both seats to be claimed", func() bool {
		return len(e.Node().Host().Held()) == 2
	})
	waitFor(t, "a fleet duty to be claimed", func() bool {
		duties, err := back.Coord.ListLive(ctx, coord.ClassWorker)
		return err == nil && len(duties) > 0
	})
	if admissions, err := back.Fleet.Admissions(ctx); err != nil || len(admissions) == 0 {
		t.Fatalf("the premise: the node is admitted, got %v (%v)", admissions, err)
	}
	// A MEMORY ROW FOR EACH SEAT, which its release publishes whole on its
	// way out — so each seat's teardown has a record of its own on the
	// silent stream for its last event to run beside.
	now := time.Now().UTC().UnixMicro()
	for _, handle := range []string{"ceo", "cto"} {
		if _, err := back.Store.SQL().ExecContext(ctx, `INSERT INTO counterparty_profiles
			(observer_handle, subject_handle, first_seen_at, last_updated_at,
			 last_corroborated_at) VALUES (?, 'founder', ?, ?, ?)`,
			handle, now, now, now); err != nil {
			t.Fatalf("give seat %s a memory row: %v", handle, err)
		}
	}

	started := time.Now()
	e.Stop(ctx)
	took := time.Since(started)

	if live, err := back.Coord.ListLive(ctx, coord.ClassNode); err != nil || len(live) != 0 {
		t.Errorf("after the stop the node's presence is %v (%v), want none: the "+
			"stream's silence spent the time its give-back needed", live, err)
	}
	for _, handle := range []string{"ceo", "cto"} {
		lease, err := back.Coord.Get(ctx, coord.SeatResource(handle))
		if err != nil {
			t.Fatalf("read seat %s: %v", handle, err)
		}
		if lease != nil {
			t.Errorf("after the stop seat %s is held by %s: the seat's last event "+
				"spent the time its lease's give-back needed", handle, lease.Owner)
		}
	}
	admissions, err := back.Fleet.Admissions(ctx)
	if err != nil {
		t.Fatalf("read the admissions: %v", err)
	}
	for _, a := range admissions {
		if a.NodeID == node {
			t.Errorf("after the stop the node's admission is still there — it has "+
				"no TTL, so it stays until an operator excludes the node: %+v", a)
		}
	}
	if duties, err := back.Coord.ListLive(ctx, coord.ClassWorker); err != nil || len(duties) != 0 {
		t.Errorf("after the stop %d duty lease(s) are held (%v): %v",
			len(duties), err, duties)
	}

	for role, handle := range map[string]string{"CEO": "ceo", "CTO": "cto"} {
		began, ended, published := stream.terminated(role)
		flushed, carried := stream.flushed(handle)
		released, gaveBack := leases.released(coord.SeatResource(handle))
		switch {
		case !published:
			t.Errorf("seat %s was released with no `agent_terminated` asked for", handle)
		case !carried:
			t.Errorf("the premise: seat %s's release published no memory, so its "+
				"teardown held nothing for its last event to run beside", handle)
		case !gaveBack:
			t.Errorf("seat %s's lease was never given back", handle)
		case released.Before(ended):
			t.Errorf("seat %s's lease was given back %v before its last event was "+
				"over: a peer taking it over could announce it first",
				handle, ended.Sub(released))
		default:
			// THE EVENT'S BOUND FROM THE SEAT'S FIRST HELD RECORD TO ITS
			// LEASE, and half the teardown's hold for the machine: beside
			// each other the teardown ends inside the event's wait, and one
			// after the other the lease waits for the sum of them.
			start := began
			if flushed.Before(start) {
				start = flushed
			}
			if took, limit := released.Sub(start), streamClientTimeout+teardownHold/2; took >= limit {
				t.Errorf("seat %s's lease was given back %v after its release began, "+
					"past %v: its last event and its teardown waited one after the "+
					"other rather than beside each other", handle, took, limit)
			}
		}
	}
	// TWO OF THE STREAM'S BOUNDS separate the outcomes: beside the drain the
	// announcement's runs while the seats' do, and in front of it the two run
	// one after the other.
	if limit := 2 * streamClientTimeout; took >= limit {
		t.Errorf("the stop took %v on a stream that answers nothing, past %v: "+
			"its events waited one after another rather than beside its "+
			"give-backs", took, limit)
	}
}

// streamClientTimeout is what the JetStream client waits for a request whose
// context carries no deadline.
const streamClientTimeout = 5 * time.Second

// errNoQuorum is what the stalled stream answers once its wait is over.
var errNoQuorum = errors.New("the event stream has lost quorum")

// teardownHold is how long the memory changelog takes to acknowledge a seat's
// last memory publish once the stop has begun.
//
// SHORTER THAN THE EVENT'S BOUND BY SECONDS, and seconds long itself, so each
// order the case tells apart misses by seconds rather than by a scheduler's
// whim: a lease given back without waiting for the seat's event comes two
// seconds before the event is over, and an event published in front of the
// teardown makes the lease three seconds late. A hold that ran to the flush's
// own deadline would end with the event, and the first of those would be told
// apart by microseconds.
const teardownHold = 3 * time.Second

// unacknowledgedLastRecords is the engine's own broker with an event stream
// that never acknowledges a stop's last events — `org_stopped` and
// `agent_terminated` — and a memory changelog that acknowledges each seat's
// last memory publish only after [teardownHold], recording when each seat's
// were asked for and when its event was over. Every other publish is the
// broker's.
type unacknowledgedLastRecords struct {
	*jetstream.Queue
	engine atomic.Pointer[engine.Engine]

	terminating sync.Map // role name → time.Time its event was asked for
	ended       sync.Map // role name → time.Time its event was over
	flushing    sync.Map // seat handle → time.Time its memory was first published
}

func (q *unacknowledgedLastRecords) Publish(ctx context.Context, subject string, ev *events.Event) error {
	if ev.Type != (types.OrgStopped{}).EventType() &&
		ev.Type != (types.AgentTerminated{}).EventType() {
		return q.Queue.Publish(ctx, subject, ev)
	}
	terminated := ev.Type == (types.AgentTerminated{}).EventType()
	if terminated {
		q.terminating.LoadOrStore(ev.Source, time.Now())
	}
	unacknowledged(ctx)
	if terminated {
		q.ended.Store(ev.Source, time.Now())
	}
	return errNoQuorum
}

// JetStream is the broker's own, but for a seat's memory publishes once the
// stop has begun — what the release's last flush makes — which are held.
func (q *unacknowledgedLastRecords) JetStream() natsjs.JetStream {
	return heldMemory{JetStream: q.Queue.JetStream(), records: q}
}

// stopping is whether the engine's stop has begun.
func (q *unacknowledgedLastRecords) stopping() bool {
	e := q.engine.Load()
	return e != nil && e.ShuttingDown()
}

// terminated is when role's last event was asked for and when it was over, if
// one was asked for.
func (q *unacknowledgedLastRecords) terminated(role string) (began, ended time.Time, ok bool) {
	b, asked := q.terminating.Load(role)
	e, over := q.ended.Load(role)
	if !asked || !over {
		return time.Time{}, time.Time{}, false
	}
	return b.(time.Time), e.(time.Time), true
}

// flushed is when handle's memory was first published once the stop began, if
// it was.
func (q *unacknowledgedLastRecords) flushed(handle string) (time.Time, bool) {
	at, ok := q.flushing.Load(handle)
	if !ok {
		return time.Time{}, false
	}
	return at.(time.Time), true
}

// heldMemory is a JetStream context whose publishes of a seat's memory are
// acknowledged only after [teardownHold] once the stop has begun.
type heldMemory struct {
	natsjs.JetStream
	records *unacknowledgedLastRecords
}

func (j heldMemory) Publish(ctx context.Context, subject string, data []byte,
	opts ...natsjs.PublishOpt) (*natsjs.PubAck, error) {
	rest, memory := strings.CutPrefix(subject, topics.MemoryPrefix)
	if memory && j.records.stopping() {
		handle, _, _ := strings.Cut(rest, ".")
		j.records.flushing.LoadOrStore(handle, time.Now())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(teardownHold):
		}
	}
	return j.JetStream.Publish(ctx, subject, data, opts...)
}

// unacknowledged waits as the client waits for an acknowledgement that never
// comes: until the request's deadline, or its own timeout for one with none.
func unacknowledged(ctx context.Context) {
	wait := streamClientTimeout
	if deadline, ok := ctx.Deadline(); ok {
		wait = time.Until(deadline)
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// recordedReleases is the lease store, recording when each lease's give-back
// was first asked for.
type recordedReleases struct {
	coordBackend
	asked sync.Map // resource → time.Time
}

func (l *recordedReleases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	l.asked.LoadOrStore(resource, time.Now())
	return l.coordBackend.Release(ctx, resource, owner, epoch)
}

// released is when resource's give-back was first asked for.
func (l *recordedReleases) released(resource string) (time.Time, bool) {
	at, ok := l.asked.Load(resource)
	if !ok {
		return time.Time{}, false
	}
	return at.(time.Time), true
}
