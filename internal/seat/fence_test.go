package seat

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// The reason the fence exists: a turn outlives its admission, and nothing
// else asks again.
func TestAFenceClosesWhenAPeerTakesTheSeat(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	fence := h.Fence("ceo")
	if fence == nil {
		t.Fatal("a held seat got no fence, so its turn runs unguarded")
	}
	if err := fence(); err != nil {
		t.Fatalf("a fence closed on the seat this node holds: %v", err)
	}

	epoch, _ := h.EpochFor("ceo")
	f.peerTakes("ceo", "peer:1", h.Owner(), epoch)
	h.Heartbeat(f.ctx)

	err := fence()
	if !errors.Is(err, ErrSeatMoved) {
		t.Fatalf("fence() = %v, want ErrSeatMoved once a peer holds the seat", err)
	}
}

// The tri-state, at the one place a two-valued answer would be an outage.
func TestAStoreBlipDoesNotCloseTheFence(t *testing.T) {
	t.Parallel()
	// An unreachable store says NOTHING about ownership, and the heartbeat
	// keeps the seat for exactly that reason. A fence that closed here
	// would abandon every in-flight turn in the company over a blip during
	// which no peer could have claimed anything anyway.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	h := f.newHost("node-a", Config{Backend: faulty, Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	fence := h.Fence("ceo")
	faulty.Break(nil)
	h.Heartbeat(f.ctx)

	if err := fence(); err != nil {
		t.Fatalf("a store blip closed the fence: %v", err)
	}
}

// A re-claim is a DIFFERENT grant, and membership alone cannot see that.
func TestAReclaimAtANewEpochClosesTheOldFence(t *testing.T) {
	t.Parallel()
	// The window dropLostSeat re-checks the epoch for: a renew fails, a
	// peer takes the seat, the peer lets it go, and this node's next sweep
	// claims it again at a higher epoch. The seat is held here at both
	// ends, so "is it in the held set" answers yes throughout — and the
	// turn still running from the first grant ran beside the peer.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	fence := h.Fence("ceo")
	first, _ := h.EpochFor("ceo")

	f.peerTakes("ceo", "peer:1", h.Owner(), first)
	h.Heartbeat(f.ctx)
	if _, err := f.store.Release(f.ctx, seatResource("ceo"), "peer:1", first+1); err != nil {
		t.Fatalf("peer release: %v", err)
	}
	// Past the backoff dropLostSeat set, so this node may claim again.
	f.clock.Advance(2 * time.Minute)
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	second, held := h.EpochFor("ceo")
	if !held {
		t.Fatal("the seat was not re-claimed, so this test proves nothing")
	}
	if second == first {
		t.Fatalf("the re-claim reused epoch %d, so there is no new grant to detect", first)
	}
	if err := fence(); !errors.Is(err, ErrSeatMoved) {
		t.Fatalf("fence() = %v, want ErrSeatMoved: the turn holds grant %d and the "+
			"node now holds grant %d", err, first, second)
	}
	if err := h.Fence("ceo")(); err != nil {
		t.Fatalf("a fence built under the NEW grant closed: %v", err)
	}
}

// An unproven teardown keeps the lease, so it has not moved.
func TestAnUndeadSeatKeepsItsFenceOpen(t *testing.T) {
	t.Parallel()
	// The seat leaves the held set but the heartbeat keeps renewing its
	// lease, precisely so no peer can take a seat this process may still
	// be consuming. Nothing is racing the in-flight turn, so closing over
	// it would abandon work for no gain.
	f := newFleet(t)
	hooks := &hookLog{releaseErr: func(string, ReleaseReason, int) error {
		return errors.New("consumer still draining")
	}}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	fence := h.Fence("ceo")
	h.Release(f.ctx, "ceo", ReasonPlacement)

	if _, held := h.EpochFor("ceo"); held {
		t.Fatal("the seat is still held, so this test is not exercising the undead set")
	}
	if err := fence(); err != nil {
		t.Fatalf("an undead seat closed its fence: %v", err)
	}
}

// A voluntary hand-back with no work in flight still closes it, because the
// lease really is gone.
func TestAReleasedSeatClosesItsFence(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	fence := h.Fence("ceo")
	h.Release(f.ctx, "ceo", ReasonPlacement)

	if err := fence(); !errors.Is(err, ErrSeatMoved) {
		t.Fatalf("fence() = %v, want ErrSeatMoved after the seat was handed back", err)
	}
}

// Nil is an open fence, and the two ways to get one are both legitimate.
func TestNoGrantMeansNoFence(t *testing.T) {
	t.Parallel()
	var none *Host
	if none.Fence("ceo") != nil {
		t.Error("a nil host produced a fence, so a hostless engine would refuse every round")
	}

	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	if h.Fence("ceo") != nil {
		t.Error("a seat this node has never claimed produced a fence")
	}
}
