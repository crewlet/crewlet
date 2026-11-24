package engine_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat"
)

// A STOP AGAINST A COORDINATION STORE THAT HAS GONE AWAY SPENDS ONE ALLOWANCE.
//
// Every give-back, duty read, status-record removal and admission withdrawal
// stalls from the moment the stop begins ([engine.Engine.ShuttingDown]; a round
// trip begun earlier ends on its client's bound), until its context's deadline
// or else a whole TTL ([stalls.ownBound]). Leases fall back to their TTL; the
// admission has none, so it runs on a share carved out of the allowance. A
// reconcile pass is held in flight across the stop ([heldPass]), so its
// give-back is a step too. The stream stays up: the native backends reach it
// through the broker's own client, which a stand-in cannot be. The verdict is
// read off the deadlines handed out ([coveredByDeadlines]) and no round trip
// ending on its own bound, never off the stop's clock, which load stretches.
func TestAStopAgainstAnUnreachableStoreSpendsOneAllowance(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Coordination.LeaseTTLSeconds = startedLeaseTTL.Seconds()
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc), nil)
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
	record := &stalls{ownBound: startedLeaseTTL}
	back.Coord = stallingLeases{coordBackend: back.Coord, gone: gone, pass: pass, stalls: record}
	back.Fleet = stallingFleet{fleet: back.Fleet, gone: gone, stalls: record}

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
	// THE PREMISE IS WAITED FOR, never read once: a node claims its seats once
	// its appliers are level, which under load or -race is after Start returns.
	waitFor(t, "both seats to be claimed", func() bool {
		return len(e.Node().Host().Held()) == 2
	})
	select {
	case <-pass.granted:
	case <-time.After(30 * time.Second):
		t.Fatal("the premise: the integration loop never took a surface's hold, " +
			"so no pass is in flight for the stop to wait on")
	}
	// AND THE NODE HAS WRITTEN ITS STATUS RECORD, which its stop removes:
	// written as the presence is claimed, waited for like the seats.
	waitFor(t, "the node's status record to be written", func() bool {
		records, err := back.Fleet.NodeStatusRecords(t.Context(), coord.NodeStatusRead{})
		return err == nil && len(records) > 0
	})

	e.Stop(context.Background())
	calls := record.all()

	// THE PREMISE: the stop made every kind of round trip the verdict is
	// about; without the held pass's give-back the case passes never reaching it.
	for _, want := range []struct {
		what string
		is   func(stalled) bool
	}{
		{"a presence or seat lease given back", func(c stalled) bool {
			return c.op == "release" && (coord.ClassNode.Holds(c.resource) ||
				coord.ClassSeat.Holds(c.resource))
		}},
		{"a duty read before its release", func(c stalled) bool {
			return c.op == "get" && coord.ClassWorker.Holds(c.resource)
		}},
		{"the admission withdrawn", func(c stalled) bool { return c.op == "forget_admission" }},
		{"the node's status record removed", func(c stalled) bool { return c.op == "delete_node_status" }},
		{"the held pass's hold given back and ended by the stop", func(c stalled) bool {
			return c.op == "release" && c.ended == endedByCaller &&
				strings.HasPrefix(c.resource, coord.WorkerResource("setup-provision-"))
		}},
	} {
		if !slices.ContainsFunc(calls, want.is) {
			t.Errorf("the premise: the stop made no round trip that is %s; it made %v",
				want.what, calls)
		}
	}
	// NO ROUND TRIP WAITED OUT A BOUND OF ITS OWN: one stalled call is a
	// whole TTL, three times the whole allowance.
	for _, c := range calls {
		if c.ended == endedByOwnBound {
			t.Errorf("%s %s carried no deadline and waited out the store's own %v: "+
				"the stop left it a bound of its own", c.op, c.resource, record.ownBound)
		}
	}
	// AND THE STEPS' DEADLINES COVER ONE ALLOWANCE BETWEEN THEM.
	allowance := seat.StopAllowance(startedLeaseTTL)
	if covered := coveredByDeadlines(calls); covered > allowance {
		t.Errorf("the stop's round trips were handed deadlines covering %v between "+
			"them, past its %v allowance: its steps did not share one: %v",
			covered, allowance, calls)
	}
}

// errGone is what the stalled store answers once its stall is over.
var errGone = errors.New("the coordination store has lost quorum")

// stallEnd is how a round trip to the gone store ended.
type stallEnd int

const (
	// endedByDeadline is a round trip whose context's deadline passed:
	// the bound the stop handed it.
	endedByDeadline stallEnd = iota
	// endedByCaller is one whose context was cancelled before any deadline
	// (a loop the teardown stopped, a give-back whose step ran out): its
	// caller's bound, not one of its own.
	endedByCaller
	// endedByOwnBound is one that carried no deadline and waited out the
	// stand-in's own bound ([stalls.ownBound]).
	endedByOwnBound
)

func (e stallEnd) String() string {
	switch e {
	case endedByDeadline:
		return "deadline"
	case endedByCaller:
		return "caller"
	default:
		return "own bound"
	}
}

// stalled is one round trip the gone store answered: what it was, when it
// began, the deadline its context carried (zero for none) and how it ended.
type stalled struct {
	op, resource string
	start        time.Time
	deadline     time.Time
	ended        stallEnd
}

func (c stalled) String() string {
	span := "no deadline"
	if !c.deadline.IsZero() {
		span = c.deadline.Sub(c.start).Round(time.Millisecond).String()
	}
	return fmt.Sprintf("%s %s (%s, ended by %s)", c.op, c.resource, span, c.ended)
}

// stalls records every round trip the gone store answers.
type stalls struct {
	// ownBound stands in for a client's own timeout on a request whose context
	// carries no deadline. A whole TTL, three allowances: far past the longest
	// wait a caller ends (a give-back cut when the allowance runs out, plus the
	// stop's local steps).
	ownBound time.Duration

	mu    sync.Mutex
	calls []stalled
}

// stall waits out a round trip to a store that has gone away and records it.
// A context with a deadline is answered only when it ends, with no timer of
// the stand-in's racing it, so a starved goroutine cannot pick
// [stalls.ownBound] over the deadline it was handed.
func (s *stalls) stall(ctx context.Context, op, resource string) error {
	c := stalled{op: op, resource: resource, start: time.Now()}
	c.deadline, _ = ctx.Deadline()
	if !c.deadline.IsZero() {
		<-ctx.Done()
		c.ended = endedByCaller
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.ended = endedByDeadline
		}
	} else {
		select {
		case <-ctx.Done():
			c.ended = endedByCaller
		case <-time.After(s.ownBound):
			c.ended = endedByOwnBound
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	if c.ended == endedByOwnBound {
		return errGone
	}
	return ctx.Err()
}

// all is every round trip recorded so far.
func (s *stalls) all() []stalled {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// coveredByDeadlines is the measure of the union of the spans from each round
// trip's start to the deadline it ended on. A step is handed the rest of one
// allowance ([seat.StopStep]), spent only while a step is in flight, so the
// measure is at most the allowance, the admission's share included. A span
// whose deadline is not after its start (a step begun with nothing left)
// covers nothing.
func coveredByDeadlines(calls []stalled) time.Duration {
	type span struct{ from, to time.Time }
	spans := make([]span, 0, len(calls))
	for _, c := range calls {
		if c.ended == endedByDeadline && c.deadline.After(c.start) {
			spans = append(spans, span{c.start, c.deadline})
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from.Compare(b.from) })
	var covered time.Duration
	var reach time.Time
	for _, sp := range spans {
		from := sp.from
		if from.Before(reach) {
			from = reach
		}
		if sp.to.After(from) {
			covered += sp.to.Sub(from)
			reach = sp.to
		}
	}
	return covered
}

type coordBackend = coord.Backend

// stallingLeases is a lease store whose reads and releases stall once gone
// answers true, and which holds back its first grant of an integration
// surface's hold until then ([heldPass]).
type stallingLeases struct {
	coordBackend
	gone   func() bool
	pass   *heldPass
	stalls *stalls
}

// heldPass is the reconcile pass a stop begins in the middle of: granted is
// closed when the first surface hold is taken, and that hold reaches its pass
// only once the stop has begun. Held, not left to chance: on an idle machine
// the loop's first pass is over before the stop.
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
		return false, l.stalls.stall(ctx, "release", resource)
	}
	return l.coordBackend.Release(ctx, resource, owner, epoch)
}

func (l stallingLeases) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if l.gone() {
		return nil, l.stalls.stall(ctx, "get", resource)
	}
	return l.coordBackend.Get(ctx, resource)
}

// stallingFleet is a fleet store whose admission withdrawal and removals of
// what a node published of itself stall once gone answers true.
type stallingFleet struct {
	fleet
	gone   func() bool
	stalls *stalls
}

func (f stallingFleet) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	if f.gone() {
		return f.stalls.stall(ctx, "forget_admission", nodeID)
	}
	return f.fleet.ForgetAdmission(ctx, nodeID, incarnation)
}

func (f stallingFleet) DeleteNodeStatus(ctx context.Context, node string, through int64) error {
	if f.gone() {
		return f.stalls.stall(ctx, "delete_node_status", node)
	}
	return f.fleet.DeleteNodeStatus(ctx, node, through)
}

func (f stallingFleet) DeleteMCPListing(ctx context.Context, server, node string, version uint64) (bool, error) {
	if f.gone() {
		return false, f.stalls.stall(ctx, "delete_mcp_listing", coord.MCPListingKey(server, node))
	}
	return f.fleet.DeleteMCPListing(ctx, server, node, version)
}

// A STOP ON A STREAM THAT WILL NOT ACKNOWLEDGE LEAVES NOTHING HELD.
//
// A stop's last events (`org_stopped`, each seat's `agent_terminated`) wait
// out whatever deadline they get on a stream that lost quorum while the store
// answers; charged to the allowance they left presence, seats, duties and the
// TTL-less admission held. Here ([unacknowledgedLastRecords]) nothing of this
// node's is held after the stop ([startedLeaseTTL] keeps a missed lease held).
// Read off the recorded waits, never the stop's clock:
//   - a seat's last event ends before its lease is given back, or a peer's
//     takeover shows in the live projection as terminated;
//   - it runs beside the seat's teardown ([teardownHold]), so a lease waits
//     for the longer, not the sum; the announcement runs beside the drain.
func TestAStopOnAStreamThatWillNotAcknowledgeLeavesNothingHeld(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Coordination.LeaseTTLSeconds = startedLeaseTTL.Seconds()
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc), nil)
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
	// THE MEMORY CHANGELOG RIDES THE COORDINATION CONNECTION, never the
	// queue's, so its hold is put on that connection's client.
	engine.WrapCoordinationJetStream(back, func(js natsjs.JetStream) natsjs.JetStream {
		return heldMemory{JetStream: js, records: stream}
	})
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

	e.Stop(ctx)

	if live, err := back.Coord.ListLive(ctx, coord.ClassNode); err != nil || len(live) != 0 {
		t.Errorf("after the stop the node's presence is %v (%v), want none: the "+
			"stream's silence spent the time its give-back needed", live, err)
	}
	for _, handle := range []string{"ceo", "cto"} {
		id, ok := seatID(e, handle)
		if !ok {
			t.Fatalf("the premise: the running company has no seat %s", handle)
		}
		lease, err := back.Coord.Get(ctx, coord.SeatResource(id))
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

	announced, said := stream.announced()
	if !said {
		t.Error("the stop asked for no `org_stopped`")
	}
	for role, handle := range map[string]string{"CEO": "ceo", "CTO": "cto"} {
		id, ok := seatID(e, handle)
		if !ok {
			t.Fatalf("the premise: the running company has no seat %s", handle)
		}
		event, published := stream.terminated(role)
		flush, carried := stream.flushed(id.String())
		released, gaveBack := leases.released(coord.SeatResource(id))
		switch {
		case !published:
			t.Errorf("seat %s was released with no `agent_terminated` asked for", handle)
			continue
		case !carried:
			t.Errorf("the premise: seat %s's release published no memory, so its "+
				"teardown held nothing for its last event to run beside", handle)
			continue
		case !gaveBack:
			t.Errorf("seat %s's lease was never given back", handle)
			continue
		}
		if released.Before(event.to) {
			t.Errorf("seat %s's lease was given back %v before its last event was "+
				"over: a peer taking it over could announce it first",
				handle, event.to.Sub(released))
		}
		// BESIDE ITS TEARDOWN: the event's wait and the seat's held memory
		// publish overlap. One in front of the other ends before the other
		// begins, and the lease waits for the sum of the two.
		if !event.overlaps(flush) {
			t.Errorf("seat %s's last event waited %v and its teardown's memory "+
				"publish %v, one after the other rather than beside each other",
				handle, event, flush)
		}
		// AND THE ANNOUNCEMENT BESIDE THE DRAIN, whose give-backs wait for
		// every seat's last event: in front of the drain, it is over before
		// any seat's release begins.
		if said && !announced.overlaps(event) {
			t.Errorf("the announcement waited %v and seat %s's last event %v, one "+
				"after the other rather than beside the drain",
				announced, handle, event)
		}
	}
}

// wait is an interval a stand-in held a request for, from when it was asked to
// when it answered.
type wait struct{ from, to time.Time }

// overlaps reports whether w and o were held at some instant together.
func (w wait) overlaps(o wait) bool { return w.from.Before(o.to) && o.from.Before(w.to) }

func (w wait) String() string {
	return fmt.Sprintf("[%s, %s]", w.from.Format("15:04:05.000"), w.to.Format("15:04:05.000"))
}

// streamClientTimeout is what the JetStream client waits for a request whose
// context carries no deadline.
const streamClientTimeout = 5 * time.Second

// errNoQuorum is what the stalled stream answers once its wait is over.
var errNoQuorum = errors.New("the event stream has lost quorum")

// teardownHold is how long the memory changelog takes to acknowledge a seat's
// last memory publish once the stop has begun.
//
// Seconds long, and seconds shorter than the event's bound, so each order the
// case tells apart misses by seconds rather than by scheduling: a hold running
// to the flush's own deadline would end with the event, so a lease given back
// without waiting for it would be told apart only by microseconds.
const teardownHold = 3 * time.Second

// unacknowledgedLastRecords is the engine's own broker with an event stream
// that never acknowledges a stop's last events — `org_stopped` and
// `agent_terminated` — and a memory changelog that acknowledges each seat's
// last memory publish only after [teardownHold], recording how long each was
// held. Every other publish is the broker's.
type unacknowledgedLastRecords struct {
	*jetstream.Queue
	engine atomic.Pointer[engine.Engine]

	mu           sync.Mutex
	announcement *wait            // the node's `org_stopped`
	lasts        map[string]wait  // role name → its seat's `agent_terminated`
	flushes      map[string]*wait // seat id → its memory publishes, first to last
}

func (q *unacknowledgedLastRecords) Publish(ctx context.Context, subject string, ev *events.Event) error {
	if ev.Type != (types.OrgStopped{}).EventType() &&
		ev.Type != (types.AgentTerminated{}).EventType() {
		return q.Queue.Publish(ctx, subject, ev)
	}
	held := wait{from: time.Now()}
	unacknowledged(ctx)
	held.to = time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	if ev.Type == (types.OrgStopped{}).EventType() {
		if q.announcement == nil {
			q.announcement = &held
		}
		return errNoQuorum
	}
	if q.lasts == nil {
		q.lasts = map[string]wait{}
	}
	if _, seen := q.lasts[ev.Source]; !seen {
		q.lasts[ev.Source] = held
	}
	return errNoQuorum
}

// stopping is whether the engine's stop has begun.
func (q *unacknowledgedLastRecords) stopping() bool {
	e := q.engine.Load()
	return e != nil && e.ShuttingDown()
}

// announced is how long the node's `org_stopped` was held, if one was asked
// for.
func (q *unacknowledgedLastRecords) announced() (wait, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.announcement == nil {
		return wait{}, false
	}
	return *q.announcement, true
}

// terminated is how long role's last event was held, if one was asked for.
func (q *unacknowledgedLastRecords) terminated(role string) (wait, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	held, ok := q.lasts[role]
	return held, ok
}

// flushed is how long the seat's memory publishes were held once the stop
// began, from the first asked for to the last answered, if there were any —
// by the seat's id, which its changelog subjects are named by.
func (q *unacknowledgedLastRecords) flushed(seat string) (wait, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	held, ok := q.flushes[seat]
	if !ok || held.to.IsZero() {
		return wait{}, false
	}
	return *held, true
}

// flushing records that a publish of seat's memory was asked for at from and
// answered at to.
func (q *unacknowledgedLastRecords) flushing(seat string, from, to time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.flushes == nil {
		q.flushes = map[string]*wait{}
	}
	held, ok := q.flushes[seat]
	if !ok {
		held = &wait{from: from}
		q.flushes[seat] = held
	}
	if to.After(held.to) {
		held.to = to
	}
}

// heldMemory is the coordination connection's JetStream client, whose
// publishes of a seat's memory are acknowledged only after [teardownHold] once
// the stop has begun.
type heldMemory struct {
	natsjs.JetStream
	records *unacknowledgedLastRecords
}

func (j heldMemory) Publish(ctx context.Context, subject string, data []byte,
	opts ...natsjs.PublishOpt) (*natsjs.PubAck, error) {
	rest, memory := strings.CutPrefix(subject, topics.MemoryPrefix)
	if memory && j.records.stopping() {
		seat, _, _ := strings.Cut(rest, ".")
		asked := time.Now()
		select {
		case <-ctx.Done():
			j.records.flushing(seat, asked, time.Now())
			return nil, ctx.Err()
		case <-time.After(teardownHold):
		}
		j.records.flushing(seat, asked, time.Now())
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
