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
// Dropping the seat through [Host.dropLostSeat] takes the seat's lock, which
// the claim holds for the whole hook, so the heartbeat goroutine would wait
// for the hook, its beat freeze, and past one TTL the watchdog end the
// process. Here the hook waits for its context, which only a cut ends, and the
// heartbeat must return, naming the seat lost; the claim then gives it back,
// backed off like any lost lease.
//
// Mutation: drop an establishing seat through dropLostSeat, and this hangs to
// its -timeout (a mutex wait is not durably blocked to a bubble).
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

// TestACutSeatIsNotRenewed holds the heartbeat off a seat cut while it was
// being taken. Its lease is lost or being given back by the claim that took
// it, so a renew could only answer for a lease nobody here is keeping — and a
// renew that answered yes would resume the admission of a seat that is never
// to be established.
//
// The hook ignores its cut, so the seat stays in the held set, cut, for as long
// as the case reads it.
//
// Mutation: renew cut seats in heartbeatTargets, and this fails.
func TestACutSeatIsNotRenewed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		hook := newCutHook(t)
		hook.ignoreCut = true
		renews := &renewLog{Backend: f.store}
		h := f.newHost("node-a", Config{Backend: renews, Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		t.Cleanup(func() { close(hook.finish); <-swept })
		<-hook.entered

		h.BeginDrain(f.ctx)
		synctest.Wait()
		h.Heartbeat(f.ctx)

		if n := renews.of(seatResource("ceo")); n != 0 {
			t.Errorf("the heartbeat renewed a seat the drain cut %d time(s): it keeps a "+
				"lease the claim that took it is giving back", n)
		}
	})
}

// TestASeatADrainCutIsGivenBackOnTheDrainsAllowance holds the give-back of a
// seat a drain cut to the stop the drain is part of. The claim that took the
// seat gives it back on the pass's context, which carries no stop: left to
// begin an allowance of its own, a stop against an unreachable store spent
// one more allowance per seat cut, uncounted in the stop's grace.
//
// The store answers no release until its context ends: the presence and the
// cut seat must both end inside the drain's one allowance, in the bubble's
// clock.
//
// Mutation: give a drain-cut seat back on an allowance of its own, and the
// pass ends a whole allowance of the lease later.
func TestASeatADrainCutIsGivenBackOnTheDrainsAllowance(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		stuck := &stuckReleases{Backend: f.store}
		hook := newCutHook(t)
		h := f.newHost("node-a", Config{Backend: stuck, Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		<-hook.entered

		const allowance = 2 * time.Second
		stuck.block()
		began := time.Now()
		h.BeginDrain(WithStopBudget(f.ctx, NewStopBudget(allowance)))
		<-swept

		if took := time.Since(began); took > allowance {
			t.Errorf("giving back the presence and the seat the drain cut took %v against "+
				"the drain's allowance of %v: the cut seat's give-back began an "+
				"allowance of its own", took, allowance)
		}
		if got := stuck.releases(); got != 2 {
			t.Errorf("%d release(s) asked for, want the presence and the seat the drain cut", got)
		}
		wantStrings(t, hook.released(), []string{"ceo:" + ReasonAcquireFailed.String()},
			"releases: the seat the drain cut is given back as never established")
	})
}

// TestALeaseTakenAsADrainBeganIsGivenBackOnTheDrainsAllowance is the same rule
// for the lease a claim took while the drain began: the claim hands it straight
// back, with nothing attached to it, on the pass's context, which carries no
// stop — so on its own it began an allowance beside the stop's.
//
// The claim is held at the store until the drain has begun, so the lease it
// is granted is one a draining host never enters; the store then answers no
// release until the context it was asked on ends.
//
// Mutation: give the lease back on an allowance of its own, and the pass ends
// a whole allowance of the lease later.
func TestALeaseTakenAsADrainBeganIsGivenBackOnTheDrainsAllowance(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		stuck := &stuckReleases{Backend: f.store}
		claims := &heldClaim{Backend: stuck, at: make(chan struct{}), proceed: make(chan struct{})}
		hooks := &hookLog{}
		h := f.newHost("node-a", Config{Backend: claims, Seats: seatsNamed("ceo"), Hooks: hooks})
		h.renewNodePresence(f.ctx)

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		<-claims.at

		const allowance = 2 * time.Second
		stuck.block()
		began := time.Now()
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			h.BeginDrain(WithStopBudget(f.ctx, NewStopBudget(allowance)))
		}()
		synctest.Wait()
		close(claims.proceed)
		<-swept
		<-drained

		if took := time.Since(began); took > allowance {
			t.Errorf("giving back the presence and the lease taken as the drain began took "+
				"%v against the drain's allowance of %v: the lease's give-back began an "+
				"allowance of its own", took, allowance)
		}
		if got := stuck.releases(); got != 2 {
			t.Errorf("%d release(s) asked for, want the presence and the lease taken as the "+
				"drain began", got)
		}
		wantStrings(t, hooks.acquired(), nil, "acquire hooks run for a lease taken as the drain began")
	})
}

// heldClaim is the lease store with the first seat claim held until proceed,
// at closed once that claim has been asked.
type heldClaim struct {
	coord.Backend
	at      chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (c *heldClaim) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	if coord.IsSeatResource(resource) {
		held := false
		c.once.Do(func() { held = true })
		if held {
			close(c.at)
			<-c.proceed
		}
	}
	return c.Backend.TryAcquire(ctx, resource, opts)
}

// TestAwaitCutWaitsForTheSeatsADrainCut is the drain's wait for what it cut.
// A cut seat leaves the held set at once, so the drain's ReleaseAll never
// reaches it, and the claim that took it gives it back only once its hook
// returns: AwaitCut is what the drain waits on so it does not report done
// beside that give-back. It answers how many are still being given back when
// its bound ends, and none once each has been.
//
// Mutation: answer AwaitCut at once, and the bounded wait reports nothing left.
func TestAwaitCutWaitsForTheSeatsADrainCut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		hook := newCutHook(t)
		hook.ignoreCut = true
		h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hook})
		h.renewNodePresence(f.ctx)
		if left := h.AwaitCut(f.ctx); left != 0 {
			t.Fatalf("AwaitCut reports %d seat(s) cut before anything was", left)
		}

		swept := make(chan SweepResult, 1)
		go func() { swept <- h.Sweep(f.ctx) }()
		<-hook.entered
		h.BeginDrain(f.ctx)
		synctest.Wait()

		bounded, cancel := context.WithTimeout(f.ctx, time.Minute)
		defer cancel()
		if left := h.AwaitCut(bounded); left != 1 {
			t.Errorf("AwaitCut reports %d seat(s) still being given back once its bound "+
				"ended, want the one whose hook has not returned", left)
		}
		if bounded.Err() == nil {
			t.Error("AwaitCut returned before its bound while the seat the drain cut was " +
				"still being given back")
		}
		wantStrings(t, hook.released(), nil, "releases while the cut hook still runs")

		close(hook.finish)
		if left := h.AwaitCut(f.ctx); left != 0 {
			t.Errorf("AwaitCut reports %d seat(s) left once the cut hook returned", left)
		}
		wantStrings(t, hook.released(), []string{"ceo:" + ReasonAcquireFailed.String()},
			"releases: AwaitCut returned before the cut seat was given back")
		<-swept
	})
}

// TestACutIsRecordedWhateverItsCancel holds the rule that a cut seat is never
// established to every establishing entry, one that carries no cancel among
// them: the cut is the cause, and the cancel is only how a hook hears of it.
//
// Mutation: record no cause for an entry without a cancel, and this fails.
func TestACutIsRecordedWhateverItsCancel(t *testing.T) {
	t.Parallel()
	entry := &heldSeat{establishing: true}
	if cancel := entry.cutLocked(cutLeaseLost); cancel != nil {
		t.Error("an entry with no cancel handed one back")
	}
	if !entry.isCut() || entry.cutBy != cutLeaseLost {
		t.Errorf("an establishing entry with no cancel was cut as %q, want %q",
			entry.cutBy, cutLeaseLost)
	}
	if entry.cutLocked(cutDrained); entry.cutBy != cutLeaseLost {
		t.Errorf("a second cut replaced the first's cause with %q", entry.cutBy)
	}
}

// TestEveryCutCauseIsValid holds the cause type to the causes the host makes.
func TestEveryCutCauseIsValid(t *testing.T) {
	t.Parallel()
	for _, c := range []cutCause{cutDrained, cutLeaseLost} {
		if !c.Valid() {
			t.Errorf("%q is a cause the host cuts for and is not valid", c)
		}
	}
	for _, c := range []cutCause{"", "stopped"} {
		if c.Valid() {
			t.Errorf("%q is valid and the host never cuts for it", c)
		}
	}
}

// renewLog counts the renews asked of the store, by resource.
type renewLog struct {
	coord.Backend
	mu    sync.Mutex
	asked map[string]int
}

func (r *renewLog) Renew(ctx context.Context, resource, owner string, epoch int64,
	ttl time.Duration) (bool, error) {

	r.mu.Lock()
	if r.asked == nil {
		r.asked = map[string]int{}
	}
	r.asked[resource]++
	r.mu.Unlock()
	return r.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

func (r *renewLog) of(resource string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked[resource]
}

// TestAHostStoppedBeforeItRanStartsUndrained holds Stop to ending a drain on a
// host that never ran too: Start leaves a drain as it finds it, so a drain begun
// and stopped before the host started would otherwise keep a later Start from
// claiming anything, with nothing left to end it.
//
// Mutation: return from Stop on a host that is not running with the drain
// still set, and this fails.
func TestAHostStoppedBeforeItRanStartsUndrained(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: &hookLog{},
		SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})

	h.BeginDrain(f.ctx)
	h.Stop(f.ctx)
	h.Start(f.ctx)
	defer h.Stop(f.ctx)
	if h.Draining() {
		t.Error("a host stopped before it ran started still draining")
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
