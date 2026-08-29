package seat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// --- the tri-state ---------------------------------------------------------

// Invariant 1: renew reporting FALSE means the lease is definitively gone.
func TestADefiniteFalseDropsTheSeatImmediately(t *testing.T) {
	t.Parallel()
	// Lost means gone: a peer may already be running it, so everything this
	// node does for it from here is a zombie's work.
	f := newFleet(t)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantAdmits(t, h, "ceo", true)

	epoch, _ := h.EpochFor("ceo")
	f.peerTakes("ceo", "peer:1", h.Owner(), epoch)

	wantStrings(t, h.Heartbeat(f.ctx), []string{"ceo"}, "lost")
	wantAdmits(t, h, "ceo", false)
	if _, ok := h.EpochFor("ceo"); ok {
		t.Error("a dropped seat still reported a fencing token")
	}
	wantStrings(t, hooks.released(), []string{"ceo:lease_lost"}, "releases")
}

// Invariant 1: an ERROR is not a loss.
func TestAStoreBlipDoesNotDropSeats(t *testing.T) {
	t.Parallel()
	// The distinction the whole design turns on. An error says NOTHING
	// about ownership — the row is untouched and still held. Conflating it
	// with loss tears a healthy node's whole company down over a two-second
	// blip, during which no peer could claim the seats anyway.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{
		Backend: faulty, Seats: seatsNamed("ceo", "eng"), Hooks: hooks,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	before := h.Held()
	wantInt(t, len(before), 2, "seats held before the blip")

	faulty.Break(nil)
	wantStrings(t, h.Heartbeat(f.ctx), nil, "seats lost to a blip")
	wantStrings(t, h.Held(), before, "seats held during a blip")
	wantStrings(t, hooks.released(), nil, "releases during a blip")
}

// Invariant 1: the grace is bounded by the TTL.
func TestAnUnreachableStoreDropsTheSeatOnceTheTTLHasLapsed(t *testing.T) {
	t.Parallel()
	// Keeping a seat through a blip is right; keeping it forever is not.
	// The row's TTL runs out on the STORE's clock whether or not this node
	// can see it — past that the lease HAS lapsed and a peer may already be
	// running the agent. Holding on from there is how one unreachable store
	// becomes two nodes serving one seat.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{
		Backend: faulty, Seats: seatsNamed("ceo"), Hooks: hooks, TTL: 30 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantAdmits(t, h, "ceo", true)

	faulty.Break(nil)
	// Inside the TTL: hold on.
	wantStrings(t, h.Heartbeat(f.ctx), nil, "seats lost inside the TTL")
	wantHeld(t, h, "ceo")

	// Past the TTL: the lease has lapsed regardless of what we can see.
	f.clock.Advance(31 * time.Second)
	wantStrings(t, h.Heartbeat(f.ctx), []string{"ceo"}, "seats dropped past the TTL")
	wantAdmits(t, h, "ceo", false)
	wantStrings(t, hooks.released(), []string{"ceo:lease_lost"}, "releases")
}

func TestTheGraceIsMeasuredFromTheLastSuccessfulRenew(t *testing.T) {
	t.Parallel()
	// A node that keeps renewing never accumulates its way into a false
	// drop: 58 seconds of wall clock, none of it more than 29 seconds from
	// a renew that succeeded, and the seat stays.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	h := f.newHost("node-a", Config{
		Backend: faulty, Seats: seatsNamed("ceo"), TTL: 30 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	faulty.Break(nil)
	f.clock.Advance(29 * time.Second)
	wantStrings(t, h.Heartbeat(f.ctx), nil, "seats lost inside the grace")

	faulty.Heal()
	h.Heartbeat(f.ctx) // a successful renew resets the clock

	faulty.Break(nil)
	f.clock.Advance(29 * time.Second)
	wantStrings(t, h.Heartbeat(f.ctx), nil, "seats lost after the grace was refreshed")
	wantHeld(t, h, "ceo")
}

// --- admission is freshness, not membership --------------------------------

// Invariant 4.
func TestMayStartReturnsTheEpochWhileTheRenewIsFresh(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	epoch, ok := h.MayStart("ceo")
	if !ok || epoch < 1 {
		t.Fatalf("MayStart = (%d, %v), want a live epoch", epoch, ok)
	}
	if _, ok := h.MayStart("nobody"); ok {
		t.Error("a seat this node does not hold admitted a turn")
	}
}

// Invariant 4.
func TestMayStartRefusesOnceTheLastRenewIsStale(t *testing.T) {
	t.Parallel()
	// A membership check ("is this seat in the held map?") reads a snapshot
	// refreshed on a 15 s heartbeat against a 45 s TTL, so it can be a full
	// TTL stale — precisely the window it exists to close, which means it
	// cannot meet its own exit criterion. Freshness CAN be proven: a renew
	// at t bought exclusivity through t+ttl.
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), HeartbeatInterval: 5 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantAdmits(t, h, "ceo", true)

	f.clock.Advance(6 * time.Second) // one heartbeat interval, and change
	wantAdmits(t, h, "ceo", false)

	// The seat is still HELD — in-flight work finishes, only NEW turns are
	// refused — and its epoch is still available for fencing the writes
	// that belong to that work.
	wantHeld(t, h, "ceo")
	if _, ok := h.EpochFor("ceo"); !ok {
		t.Fatal("a stale-but-held seat lost its fencing token, so in-flight work cannot " +
			"finish its writes")
	}
}

func TestAFailedRenewStopsNewTurnsBeforeTheSeatIsDropped(t *testing.T) {
	t.Parallel()
	// The grace keeps the seat so in-flight work can finish, but it must
	// not keep admitting NEW work on faith.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	h := f.newHost("node-a", Config{
		Backend: faulty, Seats: seatsNamed("ceo"),
		TTL: 30 * time.Second, HeartbeatInterval: 5 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	faulty.Break(nil)
	h.Heartbeat(f.ctx) // unknown: the seat is kept, the stamp is NOT refreshed
	wantHeld(t, h, "ceo")

	f.clock.Advance(6 * time.Second)
	wantAdmits(t, h, "ceo", false)
	wantHeld(t, h, "ceo")
}

// --- the admission edge ----------------------------------------------------

// Invariant 10.
func TestABlipReportsAdmissionLostAndTheRecoveryReportsItBack(t *testing.T) {
	t.Parallel()
	// The signal the consumer needs, in BOTH directions. A blip freezes the
	// last-renew stamp, so admission stops being provable within one
	// heartbeat while the seat itself is correctly kept. The consumer has
	// to stop reading, and then it has to start again: without the second
	// edge the node comes back healthy, still owning the seat, and never
	// serves it.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Backend: faulty, Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	faulty.Break(nil)
	wantStrings(t, h.Heartbeat(f.ctx), nil, "lost")
	wantHeld(t, h, "ceo")
	wantStrings(t, hooks.admissioned(), []string{"ceo:false"}, "admission edges")

	faulty.Heal()
	h.Heartbeat(f.ctx)
	wantStrings(t, hooks.admissioned(), []string{"ceo:false", "ceo:true"}, "admission edges")
}

// Invariant 10: the EDGE, not the tick.
func TestAdmissionIsReportedOnTheEdgeNotEveryHeartbeat(t *testing.T) {
	t.Parallel()
	// An hour-long outage is one call, not two hundred and forty. The hook
	// stops and starts a consumer; running it per tick would reattach-storm
	// a seat for the whole outage.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{
		Backend: faulty, Seats: seatsNamed("ceo"), Hooks: hooks, TTL: time.Hour,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	faulty.Break(nil)
	for range 4 {
		h.Heartbeat(f.ctx)
	}
	wantStrings(t, hooks.admissioned(), []string{"ceo:false"}, "admission edges during the outage")

	faulty.Heal()
	for range 4 {
		h.Heartbeat(f.ctx)
	}
	wantStrings(t, hooks.admissioned(), []string{"ceo:false", "ceo:true"}, "admission edges after it")
}

// Invariant 10: a deferred delivery must be RECORDED.
func TestADeferredDeliveryIsResumedByTheNextHealthyRenew(t *testing.T) {
	t.Parallel()
	// A seat that defers on freshness must not go deaf forever, and this
	// needs no failure at all to reach — which is what makes it worth a
	// test of its own. MayStart refuses once the last renew is older than
	// one heartbeat interval, and consecutive renews are one heartbeat
	// apart PLUS the duration of the pass, so every cycle has a real window
	// where a healthy, successfully-renewing seat answers false. A batch
	// landing in that window defers, and the queue quiesces the attachment.
	//
	// Nothing resumed it: the admission signal is edge-triggered, the
	// deferral never entered the set, so the next successful renew reported
	// "still admitted", short-circuited, and never called the hook. The
	// node kept the lease, kept renewing, stayed attached, and read nothing
	// from that inbox again until the process restarted.
	f := newFleet(t)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	// The ordinary window: the last renew is older than one heartbeat.
	f.clock.Advance(2 * HeartbeatInterval)
	wantAdmits(t, h, "ceo", false) // the premise — a healthy seat refused
	h.NoteDeliveryDeferred("ceo")

	h.Heartbeat(f.ctx) // a perfectly ordinary, SUCCESSFUL heartbeat
	wantHeld(t, h, "ceo")
	wantAdmits(t, h, "ceo", true)
	wantStrings(t, hooks.admissioned(), []string{"ceo:true"},
		"the deferred consumer was never resumed, so the seat is deaf on a healthy node "+
			"for the life of the process")
}

// Invariant 10: only the path that STOPS a consumer may record one.
func TestTheInTurnFenceDoesNotClaimAConsumerStopped(t *testing.T) {
	t.Parallel()
	// MayStart has two callers with opposite consequences. The delivery
	// path defers and quiesces; the in-turn fence abandons a turn and stops
	// no consumer. Marking the second would leave the set claiming a seat
	// is quiesced while it is still happily consuming — and the next real
	// store outage would then short-circuit the very call that stops it,
	// because that transition is edge-triggered too.
	f := newFleet(t)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	f.clock.Advance(2 * HeartbeatInterval)
	wantAdmits(t, h, "ceo", false) // the fence trips; no deferral is recorded
	h.Heartbeat(f.ctx)
	wantStrings(t, hooks.admissioned(), nil, "a fence trip must not fake a consumer stop")
	wantStrings(t, h.Unproven(), nil, "unproven")
}

func TestASeatThatLeavesForgetsItWasUnproven(t *testing.T) {
	t.Parallel()
	// A stale flag would suppress the resume after a re-claim, leaving a
	// seat this node legitimately owns attached and silent.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Backend: faulty, Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	faulty.Break(nil)
	h.Heartbeat(f.ctx)
	wantStrings(t, hooks.admissioned(), []string{"ceo:false"}, "admission edges")

	faulty.Heal()
	h.Release(f.ctx, "ceo", ReasonDrain)
	h.Sweep(f.ctx) // re-claimed, fresh attachment
	wantHeld(t, h, "ceo")
	wantStrings(t, hooks.admissioned(), []string{"ceo:false"}, "the release already reset the flag")

	faulty.Break(nil)
	h.Heartbeat(f.ctx)
	wantStrings(t, hooks.admissioned(), []string{"ceo:false", "ceo:false"},
		"the re-claimed seat could not report losing admission again")
}

func TestAnAdmissionHookFailureDoesNotAbortTheHeartbeat(t *testing.T) {
	t.Parallel()
	// The heartbeat is what keeps every OTHER seat on this node alive.
	// Letting one seat's gate take it down turns a queue hiccup into a
	// node-wide lease lapse.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	h := f.newHost("node-a", Config{
		Backend: faulty, Seats: seatsNamed("ceo", "eng"),
		Hooks: HookFuncs{Admission: func(context.Context, string, bool) error {
			panic("the broker will not answer")
		}},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	faulty.Break(nil)
	wantStrings(t, h.Heartbeat(f.ctx), nil, "lost")
	wantHeld(t, h, "ceo", "eng")
}

// --- the heartbeat's own bookkeeping ---------------------------------------

// slowRenew makes every renew cost wall-clock time, the way a slow store
// does.
type slowRenew struct {
	coord.Backend
	cost  time.Duration
	clock *fakeClock
}

func (b slowRenew) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	ok, err := b.Backend.Renew(ctx, resource, owner, epoch, ttl)
	b.clock.Advance(b.cost)
	return ok, err
}

// Invariant 11: the clock is read PER SEAT.
func TestTheHeartbeatReadsTheClockPerSeat(t *testing.T) {
	t.Parallel()
	// A single pre-loop timestamp is assigned to every seat's last-renew
	// stamp, so with many seats and a slow store the LATER ones record a
	// renew earlier than it happened — narrowing their grace, and their
	// admission window, as the seat count grows. Here three seats and a
	// ten-second store would leave the last seat looking thirty seconds
	// stale the instant its renew succeeded.
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo", "eng", "ops"),
		TTL:   60 * time.Second, HeartbeatInterval: 15 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	h.backend = slowRenew{Backend: f.store, cost: 10 * time.Second, clock: f.clock}
	wantStrings(t, h.Heartbeat(f.ctx), nil, "lost")

	// ceo genuinely aged past a heartbeat interval while the pass ground on.
	wantAdmits(t, h, "ceo", false)
	// ops did not, and only a per-seat clock read can tell.
	wantAdmits(t, h, "ops", true)
}

// reclaimOnRenew re-claims a seat while the heartbeat is suspended inside its
// renew — the window the per-seat lock and the epoch re-check exist for.
type reclaimOnRenew struct {
	coord.Backend
	once sync.Once
	on   func()
}

func (b *reclaimOnRenew) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	if coord.IsSeatResource(resource) {
		b.once.Do(b.on)
		return false, nil // the OLD epoch is genuinely gone
	}
	return b.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

// Invariant 11: the epoch is re-checked under the lock before acting.
func TestAReclaimBetweenHeartbeatAndReleaseIsNotTornDown(t *testing.T) {
	t.Parallel()
	// The heartbeat/sweep race, pinned at the point it actually happens.
	// The window is INSIDE the renew: the heartbeat is carrying a lease it
	// read before the call, and a sweep can re-claim the same seat at a new
	// epoch while it is suspended there. Tearing that down would leave the
	// seat owned in the lease table and dead in this process, with nothing
	// to notice.
	f := newFleet(t)
	stub := &reclaimOnRenew{Backend: f.store}
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Backend: stub, Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	epoch, _ := h.EpochFor("ceo")

	stub.on = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		lease := h.held["ceo"].lease
		lease.Epoch++
		h.held["ceo"] = &heldSeat{lease: lease, renewedAt: h.now()}
	}

	wantStrings(t, h.Heartbeat(f.ctx), nil, "the heartbeat tore down a newer claim it did not own")
	wantAdmits(t, h, "ceo", true)
	got, _ := h.EpochFor("ceo")
	wantInt(t, int(got), int(epoch+1), "epoch after the re-claim")
	wantStrings(t, hooks.released(), nil, "releases")
}
