package seat

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// --- a seat cut while it is being taken -------------------------------------
//
// A seat enters the held set before its acquire hook runs, so the heartbeat
// renews its lease throughout ([heldSeat]), and the hook runs under the seat's
// own lock for as long as it takes. A hook has no total of its own — it spawns
// MCP children, hydrates the seat's memory and recovers an interrupted
// executor — so anything that had to wait for that lock waited for the hook:
// the heartbeat dropping a lease it lost, and a drain giving its seats back.
// These cases hold the host to the rule that replaced the wait: a lost lease
// and a drain CUT the hook through its context, the seat is never established
// once cut, and nothing treats a cut seat as held.

// TestALeaseLostWhileASeatIsEstablishingCutsItsHookAndNeverHoldsTheHeartbeat
// is the wedge: a renew answers false while the seat's hook runs.
//
// The heartbeat used to drop the seat through [Host.dropLostSeat], which takes
// the seat's lock — and the claim holds that lock for the whole hook. So the
// heartbeat goroutine waited for the hook, its beat froze, and past one TTL the
// watchdog ended the process; on the boot's first pass, before the watchdog is
// armed, every other seat that pass had attached went unrenewed while it still
// consumed its mailbox. Here the hook waits for its context, which only a cut
// ends, and the case asks the heartbeat itself: it must return, naming the
// seat lost, and the seat must then be given back by the claim that took it,
// backed off like any lost lease.
//
// Red before the cut: Heartbeat blocks on the seat's lock for as long as the
// hook runs, which is for ever. A goroutine blocked on a mutex is not durably
// blocked to a bubble, so the run hangs to its -timeout rather than failing.
//
// Mutation: drop an establishing seat through dropLostSeat, as the heartbeat
// did, and this hangs the same way.
func TestALeaseLostWhileASeatIsEstablishingCutsItsHookAndNeverHoldsTheHeartbeat(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		hook := newCutHook(t)
		h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		<-hook.entered
		lease := f.leaseOf(seatResource("ceo"))
		if lease == nil || lease.Owner != h.Owner() {
			t.Fatalf("the premise: the seat being taken is leased to %+v, want this node", lease)
		}
		f.peerTakes("ceo", "node-b:incarnation", h.Owner(), lease.Epoch)

		lost := h.Heartbeat(f.ctx)
		wantStrings(t, lost, []string{"ceo"}, "seats the heartbeat lost")
		synctest.Wait()

		if !hook.cut() {
			t.Error("the hook of a seat whose lease was lost while it established runs on: " +
				"nothing cut its context")
		}
		result := <-swept
		if slices.Contains(result.Claimed, "ceo") {
			t.Error("the pass counted as claimed a seat whose lease was lost while it established")
		}
		wantHeld(t, h)
		wantStrings(t, hook.establishedSeats(), nil, "seats established")
		wantStrings(t, hook.released(), []string{"ceo:" + ReasonAcquireFailed.String()},
			"releases: the seat cut while it was being taken is given back as never established")
		if !backedOff(h, "ceo") {
			t.Error("a seat whose lease was lost while it established is not backed off: " +
				"this node would take it straight back from the peer that holds it")
		}
	})
}

// TestADrainCutsASeatStillEstablishing is the drain's half: a drain used to
// wait for every establishing hook, because its release takes the seat's lock,
// so a stop waited out an acquisition with no total. The drain now cuts the
// hook, the seat is given back as never established, and it is NOT backed off:
// the node is leaving, and a backoff is a promise about its next claim.
//
// Red before the cut: BeginDrain cuts nothing, the hook is still waiting for its
// context once the bubble is idle, and the seat is still held.
//
// Mutation: BeginDrain cuts nothing, and this fails the same way.
func TestADrainCutsASeatStillEstablishing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		hook := newCutHook(t)
		h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)

		pass, stop := context.WithCancel(f.ctx)
		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(pass) }()
		t.Cleanup(func() { stop(); <-swept })
		<-hook.entered

		h.BeginDrain(f.ctx)
		synctest.Wait()

		if !hook.cut() {
			t.Fatal("a drain left the hook of a seat still being taken running: the drain " +
				"waits for an acquisition that has no total")
		}
		wantHeld(t, h)
		wantStrings(t, hook.establishedSeats(), nil, "seats established")
		wantStrings(t, hook.released(), []string{"ceo:" + ReasonAcquireFailed.String()},
			"releases: the seat the drain cut is given back as never established")
		if backedOff(h, "ceo") {
			t.Error("a seat a drain cut is backed off: the node is leaving, and the " +
				"backoff says something only about a claim it will not make")
		}
		if lease := f.leaseOf(seatResource("ceo")); lease != nil {
			t.Errorf("the seat the drain cut is still leased to %s", lease.Owner)
		}
	})
}

// TestACutSeatWhoseHookReturnedNilIsGivenBack holds the host to its side of a
// cut whatever the hook does with it. A hook that never reads its context is
// not shortened by a cut, and it may well finish and return nil — but the
// lease under it is lost or being given back, so the seat must not start
// admitting turns as though it had been taken.
//
// Red before the cut: the drain cuts nothing, the hook returns nil, and the
// seat is established on a node that is draining.
//
// Mutation: establish a cut seat whose hook returned nil, and this fails.
func TestACutSeatWhoseHookReturnedNilIsGivenBack(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		hook := newCutHook(t)
		hook.ignoreCut = true
		h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		<-hook.entered

		h.BeginDrain(f.ctx)
		synctest.Wait()
		close(hook.finish)
		result := <-swept

		if slices.Contains(result.Claimed, "ceo") {
			t.Error("the pass counted as claimed a seat the drain cut")
		}
		wantHeld(t, h)
		wantAdmits(t, h, "ceo", false)
		wantStrings(t, hook.establishedSeats(), nil,
			"seats established: a cut seat whose hook returned nil was taken anyway")
		wantStrings(t, hook.released(), []string{"ceo:" + ReasonAcquireFailed.String()},
			"releases: the cut seat is given back as never established")
	})
}

// TestACutSeatIsNotHeld holds the cut seat out of what the rest of the node
// reads as held. Its lease is lost or being given back, so nothing may act on
// it as held: the memory cycle would publish it, a drain's ReleaseAll would
// wait on its lock, and a sandbox apply would prepare it.
//
// Red before the cut: a drain cuts nothing, and a seat still establishing is
// listed by both.
//
// Mutation: Held lists cut seats, and this fails.
func TestACutSeatIsNotHeld(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		hook := newCutHook(t)
		hook.ignoreCut = true
		h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		t.Cleanup(func() { close(hook.finish); <-swept })
		<-hook.entered
		wantHeld(t, h, "ceo")

		h.BeginDrain(f.ctx)
		synctest.Wait()

		wantHeld(t, h)
		if seats := h.HeldSeats(); len(seats) != 0 {
			t.Errorf("HeldSeats lists %v while the drain has cut it", seats)
		}
		if h.Provable("ceo") {
			t.Error("Provable answers yes for a seat the drain has cut: a child of the " +
				"seat's would be restarted for a holding that is being given back")
		}
	})
}

// TestADrainBegunBeforeStartHoldsThroughTheFirstPass is the boot's half of the
// drain. A stop decided while the engine is still starting begins the drain
// before the host's first pass can claim — and the host's Start used to clear
// the drain, so that pass claimed the node's whole share onto a node already
// leaving, each claim's hook run to its end. A stopped host still starts again
// undrained, because Stop is what clears it now.
//
// Red before the change: Start clears the drain, and the first pass claims.
//
// Mutation: Start clears draining, and this fails.
func TestADrainBegunBeforeStartHoldsThroughTheFirstPass(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks,
		SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})

	h.BeginDrain(f.ctx)
	h.Start(f.ctx)
	if !h.Draining() {
		t.Error("the host's Start undid a drain begun before it")
	}
	wantHeld(t, h)
	wantStrings(t, hooks.acquired(), nil, "acquire hooks run by the first pass of a draining host")
	if lease := f.leaseOf(coord.NodeResource("node-a")); lease != nil {
		t.Error("a host draining before it started announced itself to its peers")
	}

	h.Stop(f.ctx)
	h.Start(f.ctx)
	defer h.Stop(f.ctx)
	if h.Draining() {
		t.Error("a stopped host started again still draining: Stop must end the drain " +
			"it was stopped in")
	}
	wantHeld(t, h, "ceo")
}

// cutHook is an acquire hook that waits for its context to end — or, when
// ignoreCut is set, for finish whatever its context does — and records what the
// host asked of it.
type cutHook struct {
	entered chan struct{}
	finish  chan struct{}

	// ignoreCut makes the hook wait for finish alone and return nil: a hook
	// that never reads its context.
	ignoreCut bool

	mu          sync.Mutex
	ctx         context.Context
	established []string
	releases    []string
}

func newCutHook(t *testing.T) *cutHook {
	t.Helper()
	return &cutHook{entered: make(chan struct{}), finish: make(chan struct{})}
}

func (c *cutHook) OnAcquire(ctx context.Context, _ placement.Seat, _ coord.Lease) error {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	close(c.entered)
	if c.ignoreCut {
		<-c.finish
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *cutHook) OnEstablished(_ context.Context, s placement.Seat, _ coord.Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.established = append(c.established, s.Handle)
}

func (c *cutHook) OnRelease(_ context.Context, s placement.Seat, _ coord.Lease, reason ReleaseReason) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releases = append(c.releases, s.Handle+":"+reason.String())
	return nil
}

func (c *cutHook) OnAdmission(context.Context, placement.Seat, bool) error { return nil }

// cut reports whether the context the hook was handed has ended.
func (c *cutHook) cut() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctx != nil && c.ctx.Err() != nil
}

func (c *cutHook) establishedSeats() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.established)
}

func (c *cutHook) released() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.releases)
}

var _ Hooks = (*cutHook)(nil)

// backedOff reports whether this node stands back from claiming a seat.
func backedOff(h *Host, handle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.acquireBackoffs[handle]
	return ok
}
