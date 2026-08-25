package seat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// --- the two families ------------------------------------------------------

// Invariant 5: release has two opposite modes.
func TestReleaseReasonsSeparateLossFromDrain(t *testing.T) {
	t.Parallel()
	// Voluntary and fenced are opposites: one has time and exclusivity, the
	// other has neither.
	cases := map[ReleaseReason]bool{
		ReasonDrain:         false,
		ReasonRoleGone:      false,
		ReasonPlacement:     false,
		ReasonLeaseLost:     true,
		ReasonAcquireFailed: true,
		ReasonPosture:       true,
	}
	for reason, fenced := range cases {
		if reason.Fenced() != fenced {
			t.Errorf("%s.Fenced() = %v, want %v", reason, reason.Fenced(), fenced)
		}
	}
}

func TestTheHookIsToldWhyTheSeatIsGoing(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	seats := []string{"ceo"}
	h := f.newHost("node-a", Config{Hooks: hooks, Seats: func() []placement.Seat {
		return seatsNamed(seats...)()
	}})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	h.Release(f.ctx, "ceo", ReasonDrain)
	wantStrings(t, hooks.released(), []string{"ceo:drain"}, "releases")

	h.Sweep(f.ctx) // takes it back
	seats = nil    // the role was decommissioned
	h.Sweep(f.ctx)
	wantStrings(t, hooks.released(), []string{"ceo:drain", "ceo:role_gone"}, "releases")
}

func TestALostLeaseReleasesWithTheFencedReason(t *testing.T) {
	t.Parallel()
	// A peer may already be running this seat, so in-flight work is
	// abandoned rather than finished, and nothing is republished —
	// republishing hands the peer a second copy of work it is already
	// doing.
	f := newFleet(t)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	epoch, _ := h.EpochFor("ceo")
	f.peerTakes("ceo", "peer:1", h.Owner(), epoch)
	wantStrings(t, h.Heartbeat(f.ctx), []string{"ceo"}, "lost")
	wantStrings(t, hooks.released(), []string{"ceo:lease_lost"}, "releases")
}

func TestTheAcquireHookIsGivenTheFencingToken(t *testing.T) {
	t.Parallel()
	// Every write made on a seat's behalf carries this. A write without it
	// is a write a zombie can also make.
	f := newFleet(t)
	var seen coord.Lease
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Hooks: HookFuncs{Acquire: func(_ context.Context, _ string, lease coord.Lease) error {
			seen = lease
			return nil
		}},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	epoch, ok := h.EpochFor("ceo")
	if !ok || seen.Epoch != epoch || seen.Resource != coord.SeatResource("ceo") {
		t.Fatalf("hook saw %+v, want the live lease at epoch %d", seen, epoch)
	}
	if _, ok := h.EpochFor("nobody"); ok {
		t.Error("a seat this node does not hold reported an epoch")
	}
}

// --- fail-closed release ---------------------------------------------------

func stuckTeardown(err string) func(string, ReleaseReason, int) error {
	return func(string, ReleaseReason, int) error { return errors.New(err) }
}

// Invariant 5: a teardown that cannot be PROVEN keeps the lease.
func TestAnUnprovenTeardownKeepsTheLease(t *testing.T) {
	t.Parallel()
	// The single failure ownership exists to prevent. Releasing a lease
	// while still consuming the seat hands a peer permission to run the
	// same agent concurrently.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("the consumer would not close")}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	epoch, _ := h.EpochFor("ceo")

	if h.Release(f.ctx, "ceo", ReasonDrain) {
		t.Fatal("an unproven teardown reported the lease as released")
	}
	// Out of the held set, so nothing new starts on it...
	wantHeld(t, h)
	wantAdmits(t, h, "ceo", false)
	// ...but still leased, and still ours.
	wantStrings(t, h.Unproven(), []string{"ceo"}, "unproven")
	lease := f.leaseOf(coord.SeatResource("ceo"))
	if lease == nil || lease.Owner != h.Owner() || lease.Epoch != epoch {
		t.Fatalf("seat lease = %+v, want ours at epoch %d", lease, epoch)
	}
	// And a peer cannot take it.
	taken, err := f.store.TryAcquire(f.ctx, coord.SeatResource("ceo"), coord.AcquireOptions{
		Owner: "peer:1", TTL: time.Minute, Protocol: coord.ProtocolVersion,
	})
	if err != nil || taken != nil {
		t.Fatalf("a peer claimed a seat this process may still be consuming: %+v", taken)
	}
}

func TestAnUnprovenSeatKeepsBeingRenewed(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("still consuming")}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks, TTL: 30 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonDrain)

	before := f.leaseOf(coord.SeatResource("ceo")).ExpiresAt
	f.clock.Advance(10 * time.Second)
	h.Heartbeat(f.ctx)
	after := f.leaseOf(coord.SeatResource("ceo")).ExpiresAt
	if !after.After(before) {
		t.Fatal("an unproven seat stopped being renewed, so a peer will take a seat this " +
			"process may still be consuming")
	}
}

func TestAnUnprovenSeatIsNotReClaimedOrDoubleCounted(t *testing.T) {
	t.Parallel()
	// It counts against capacity: this process may still be serving it, so
	// taking on more work would over-subscribe a node already in trouble.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("still consuming")}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo", "eng"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonDrain)

	result := h.Sweep(f.ctx)
	if slicesContains(result.Claimed, "ceo") {
		t.Fatal("an undead seat was claimed again by the process that could not close it")
	}
	wantStrings(t, h.Unproven(), []string{"ceo"}, "unproven")
}

func TestAnUnprovenTeardownIsRetriedAndRecovers(t *testing.T) {
	t.Parallel()
	// The reason undead is a state and not a grave. The causes are
	// overwhelmingly transient — a consumer mid-delivery, an MCP child that
	// has not finished dying — and without a retry the FIRST one stranded
	// the seat for the life of the process: out of the held set so this
	// node never ran it, leased so no peer could, counted against capacity
	// forever, and announced exactly once in a line that then rotated away.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: func(_ string, _ ReleaseReason, calls int) error {
		if calls == 1 {
			return errors.New("the consumer would not close")
		}
		return nil
	}}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonPlacement)
	wantStrings(t, h.Unproven(), []string{"ceo"}, "unproven")

	h.Heartbeat(f.ctx)

	wantStrings(t, h.Unproven(), nil, "unproven after the retry")
	wantStrings(t, hooks.released(), []string{"ceo:placement", "ceo:placement"},
		"the retry must continue the SAME release, or the hook tears the seat down differently")
	// And the lease is gone, so the fleet can run the seat again.
	if lease := f.leaseOf(coord.SeatResource("ceo")); lease != nil {
		t.Fatalf("the recovered seat is still leased: %+v", lease)
	}
}

func TestARetryThatKeepsFailingKeepsTheSeatAndAges(t *testing.T) {
	t.Parallel()
	// The only safe answer is still to hold it — but visibly. UnprovenAges
	// is what an alert reads: existence is normal for a moment, duration
	// never is.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("still consuming")}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks, TTL: 30 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonDrain)

	f.clock.Advance(5 * time.Second)
	h.Heartbeat(f.ctx)
	f.clock.Advance(5 * time.Second)
	h.Heartbeat(f.ctx)

	wantStrings(t, h.Unproven(), []string{"ceo"}, "unproven")
	wantInt(t, undeadAttempts(h, "ceo"), 3, "teardown attempts (one per heartbeat)")
	if age := h.UnprovenAges()["ceo"]; age != 10*time.Second {
		t.Fatalf("stranded age = %s, want 10s", age)
	}
	if lease := f.leaseOf(coord.SeatResource("ceo")); lease == nil || lease.Owner != h.Owner() {
		t.Fatalf("seat lease = %+v, want still ours", lease)
	}
}

func TestAStrandedSeatReRaisesItsAlarmOnAnIntervalNotEveryTick(t *testing.T) {
	t.Parallel()
	// The failure itself is not news — it is the same failure as last
	// heartbeat — so logging it every tick would bury the fleet's other
	// signals under one seat. What IS news is that it is STILL happening,
	// which is why the alarm repeats at all, and why the elapsed time is
	// its payload. It used to be logged once, at the moment it happened,
	// and then never again: a seat could be out of service for a week with
	// the only evidence a single line that had long since rotated out.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("still consuming")}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks, TTL: time.Hour})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonDrain)
	first := undeadAlarm(h, "ceo")

	f.clock.Advance(UndeadAlarmInterval - time.Second)
	h.Heartbeat(f.ctx)
	if undeadAlarm(h, "ceo") != first {
		t.Fatal("a stranded seat re-alarmed inside its quiet window")
	}

	f.clock.Advance(2 * time.Second)
	h.Heartbeat(f.ctx)
	if undeadAlarm(h, "ceo") == first {
		t.Fatal("a seat stranded past the alarm interval said nothing, so it can be out of " +
			"service with no live evidence at all")
	}
	// And the retry ran on EVERY heartbeat regardless — the alarm rate
	// limits the noise, never the recovery attempt.
	wantInt(t, undeadAttempts(h, "ceo"), 3, "teardown attempts")
}

func TestARecoveredSeatCanBeClaimedAgainByThisNode(t *testing.T) {
	t.Parallel()
	// Recovery has to return the seat to the FLEET, not just to the map it
	// was parked in — which means releasing the lease.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: func(_ string, _ ReleaseReason, calls int) error {
		if calls == 1 {
			return errors.New("the consumer would not close")
		}
		return nil
	}}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonDrain)
	h.Heartbeat(f.ctx)

	result := h.Sweep(f.ctx)
	wantStrings(t, result.Claimed, []string{"ceo"}, "claimed")
	wantHeld(t, h, "ceo")
}

func TestAnUndeadSeatWhoseLeaseIsGoneStopsBeingRenewed(t *testing.T) {
	t.Parallel()
	// Teardown was never proven and the lease has now moved on regardless.
	// This process may still be consuming the seat, but there is nothing
	// left to hold: renewing a row a peer owns is not exclusion, it is
	// noise.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("still consuming")}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks, TTL: 30 * time.Second,
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	h.Release(f.ctx, "ceo", ReasonDrain)
	wantStrings(t, h.Unproven(), []string{"ceo"}, "unproven")

	f.clock.Advance(31 * time.Second) // the row lapses under us
	h.Heartbeat(f.ctx)
	wantStrings(t, h.Unproven(), nil, "unproven after the lease went")
}

func TestAPanickingReleaseHookAlsoFailsClosed(t *testing.T) {
	t.Parallel()
	// A hook that fails in an unexpected way is no more proof of teardown
	// than one that fails in an expected way — and in Go the unexpected way
	// would otherwise take the whole process, and every healthy seat on it,
	// down with the one broken seat.
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Hooks: HookFuncs{Release: func(context.Context, string, coord.Lease, ReleaseReason) error {
			panic("something else entirely")
		}},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	if h.Release(f.ctx, "ceo", ReasonDrain) {
		t.Fatal("a panicking teardown reported the lease as released")
	}
	wantStrings(t, h.Unproven(), []string{"ceo"}, "unproven")
}

// --- draining many seats ---------------------------------------------------

func TestSeatsAreReleasedTogetherNotInAQueue(t *testing.T) {
	t.Parallel()
	// A drain costs one bounded wait, not one per seat. In sequence, a node
	// holding a dozen seats pays that timeout twelve times, and the seat
	// that went idle FIRST stays dark for the whole procession —
	// unclaimable by any peer, for no reason.
	f := newFleet(t)
	var mu sync.Mutex
	entered := map[string]bool{}
	all := make(chan struct{})
	release := make(chan struct{})
	h := f.newHost("node-a", Config{
		Hooks: HookFuncs{Release: func(_ context.Context, handle string, _ coord.Lease, _ ReleaseReason) error {
			mu.Lock()
			entered[handle] = true
			if len(entered) == 3 {
				close(all)
			}
			mu.Unlock()
			<-release
			return nil
		}},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantInt(t, len(h.Held()), 3, "seats held")

	done := make(chan struct{})
	go func() { defer close(done); h.ReleaseAll(f.ctx, ReasonDrain) }()

	select {
	case <-all:
	case <-time.After(5 * time.Second):
		mu.Lock()
		in := len(entered)
		mu.Unlock()
		t.Fatalf("only %d of 3 releases had started; they ran in sequence, so the last seat "+
			"waited on the first", in)
	}
	close(release)
	<-done
	wantHeld(t, h)
}

// Invariant 5: failing closed is PER SEAT.
func TestOneStuckSeatDoesNotStrandTheOthers(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Hooks: HookFuncs{Release: func(_ context.Context, handle string, _ coord.Lease, _ ReleaseReason) error {
			if handle == "eng" {
				return errors.New("consumer will not close")
			}
			return nil
		}},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	h.ReleaseAll(f.ctx, ReasonDrain)
	wantHeld(t, h)
	wantStrings(t, h.Unproven(), []string{"eng"}, "unproven")
}

func TestAShedThatCannotBeProvenIsNotReportedAsRoomMade(t *testing.T) {
	t.Parallel()
	// An unproven teardown keeps the lease, so the seat did not move.
	// Counting it as shed would tell the fleet a rebalance made room no
	// peer can take — and this node would then shed a SECOND seat next
	// sweep on top of one it is still serving.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: stuckTeardown("consumer will not close")}
	a := f.newHost("node-a", Config{Hooks: hooks})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx)
	wantInt(t, len(a.Held()), 3, "seats held alone")

	f.present("node-b", time.Minute, placement.NodeProfile{})
	result := a.Sweep(f.ctx)
	wantInt(t, len(result.Lost), 0, "seats reported as shed")
	wantInt(t, len(a.Unproven()), 1, "unproven seats")
}

// --- the per-seat lock -----------------------------------------------------

// Invariant 6: one mutex per seat, held across a WHOLE acquire or release.
func TestAReleaseWaitsForAConcurrentAcquireOnTheSameSeat(t *testing.T) {
	t.Parallel()
	// The heartbeat and the sweep are independent goroutines with no
	// ordering between them, and both hooks are long. Without the lock, a
	// release that lands mid-acquire finds nothing held, tears nothing
	// down, and reports nothing wrong — while the consumer and MCP children
	// the claim just created stay alive under a lease this node is about to
	// give away.
	f := newFleet(t)
	inAcquire := make(chan struct{})
	finish := make(chan struct{})
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Hooks: HookFuncs{
			Acquire: func(context.Context, string, coord.Lease) error {
				note("acquire:start")
				close(inAcquire)
				<-finish
				note("acquire:end")
				return nil
			},
			Release: func(context.Context, string, coord.Lease, ReleaseReason) error {
				note("release")
				return nil
			},
		},
	})
	h.renewNodePresence(f.ctx)

	swept := make(chan struct{})
	go func() { defer close(swept); h.Sweep(f.ctx) }()
	<-inAcquire

	released := make(chan bool, 1)
	go func() { released <- h.Release(f.ctx, "ceo", ReasonDrain) }()
	// Give the release every chance to interleave. The assertion below
	// holds with or without this pause; the pause is what makes a broken
	// lock fail loudly rather than occasionally.
	time.Sleep(20 * time.Millisecond)
	close(finish)

	if !<-released {
		t.Fatal("the release ran through the middle of the acquire and tore down nothing")
	}
	<-swept
	wantHeld(t, h)
	wantStrings(t, order, []string{"acquire:start", "acquire:end", "release"}, "hook order")
}

// Invariant 9: on_acquire attaches the consumer LAST.
func TestNoWorkIsAdmittedUntilTheAcquireHookReturns(t *testing.T) {
	t.Parallel()
	// The hook establishes the seat in a known state — agent instance,
	// budget cap, per-role MCP children, sandbox recovery — and attaches
	// the inbox only at the end, because a seat that starts receiving work
	// before its MCP children are up runs its first turn with an empty tool
	// surface. The host's half of that bargain is that the seat is not
	// claimed, not admitting, and not visible until the hook returns.
	f := newFleet(t)
	inAcquire := make(chan struct{})
	finish := make(chan struct{})
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Hooks: HookFuncs{Acquire: func(context.Context, string, coord.Lease) error {
			close(inAcquire)
			<-finish
			return nil
		}},
	})
	h.renewNodePresence(f.ctx)

	swept := make(chan struct{})
	go func() { defer close(swept); h.Sweep(f.ctx) }()
	<-inAcquire

	wantAdmits(t, h, "ceo", false)
	// It IS leased, though, and the heartbeat renews it throughout: an
	// acquire spawns MCP children and recovers an interrupted sandbox run,
	// which can outlast a TTL, and a lease that lapsed mid-acquire would
	// let a peer claim a seat this node is halfway through building.
	if _, ok := h.EpochFor("ceo"); !ok {
		t.Fatal("a seat mid-acquire is not being renewed, so its lease can lapse under the " +
			"hook that is still building it")
	}
	wantStrings(t, h.Heartbeat(f.ctx), nil, "lost while establishing")

	close(finish)
	<-swept
	wantHeld(t, h, "ceo")
	wantAdmits(t, h, "ceo", true)
}

func undeadAlarm(h *Host, handle string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry := h.undead[handle]; entry != nil {
		return entry.lastAlarm
	}
	return time.Time{}
}

func undeadAttempts(h *Host, handle string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry := h.undead[handle]; entry != nil {
		return entry.attempts
	}
	return 0
}
