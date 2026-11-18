package seat

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// --- the loops ------------------------------------------------------------

// heartbeatLoop renews this node's leases, and is the duty the watchdog
// watches.
//
// It beats far faster than it renews. The beat proves the goroutine is
// turning: a pass hung in a hook or on a lock freezes the stamp, and past the
// lease TTL this node's seats have moved to a peer while its queue client
// still holds their mail. See [Watchdog].
//
// TWO TICKERS, the renew pass on one of its own ([Host.renewPeriod]): a ticker
// drops ticks that come due while its receiver is busy, so counting received
// beats would delay the pass after a long one.
func (h *Host) heartbeatLoop(ctx context.Context) {
	beats := time.NewTicker(h.beatInterval())
	defer beats.Stop()
	passes := time.NewTicker(h.renewPeriod())
	defer passes.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-beats.C:
			h.stampBeat()
		case <-passes.C:
			h.stampBeat()
			h.safely("seat_heartbeat_tick_failed", func() { h.Heartbeat(ctx) })
			h.stampBeat()
		}
	}
}

// renewPeriod is how often the heartbeat loop renews: ONE BEAT INSIDE THE
// HEARTBEAT.
//
// [Host.MayStart] refuses a seat whose last renew is more than one heartbeat
// old. The stamp is the instant a renew was SENT ([Host.Heartbeat] reads the
// clock before the store call), recorded once it succeeded, so a pass every
// heartbeat would send each renew just as the proof it replaces stops
// admitting, and the seat would refuse its own work for the round trip
// (internal/node defers the delivery). Sent a beat early, the renew has the
// beat to land in. A function so [TestAHealthyNodeNeverRefusesItsOwnSeat]
// calls it over a range of TTLs.
func (h *Host) renewPeriod() time.Duration {
	// A BEAT is the watchdog's resolution of lateness ([Watchdog.Lag]); a renew
	// landing later is what [Host.NoteDeliveryDeferred] and the resume are for.
	// Stamping at the answer would credit a round trip the node never held, and
	// widening MayStart's window would admit on a proof older than its
	// ttl - heartbeat promise. Shorter than the heartbeat renews MORE often,
	// never less.
	//
	// Positive whatever a host was built with, because a ticker panics on
	// anything else: beatInterval is at most half the heartbeat, so this is
	// at least a beat for every heartbeat of two nanoseconds or more.
	return max(h.heartbeat-h.beatInterval(), time.Nanosecond)
}

// beatInterval is how often the heartbeat goroutine proves it is turning.
//
// A CEILING scaled to the lease TTL, the threshold the watchdog fires at: five
// beats must fit inside it so a healthy pass never looks stalled and a node
// does not shoot itself at a tight TTL. Invisible at the shipped values (45 s
// vs 1 s), so it is enforced rather than documented.
//
// And never more than HALF the heartbeat: the renew pass runs a beat inside
// the heartbeat ([Host.renewPeriod]), so a longer beat would leave the pass
// less than a beat after the one before. That binds only below a 6 s TTL,
// where it also keeps the beat from lagging the work it proves.
func (h *Host) beatInterval() time.Duration {
	d := beatCeiling(h.ttl)
	if half := h.heartbeat / 2; half < d {
		d = half
	}
	// A ticker panics on a non-positive interval, which only a heartbeat
	// of a nanosecond would leave here.
	return max(d, time.Nanosecond)
}

// --- the heartbeat --------------------------------------------------------

// heartbeatTarget is one seat to renew, snapshotted so the store call never
// runs while holding a lock.
type heartbeatTarget struct {
	seat      placement.Seat
	handle    string
	entry     *heldSeat
	lease     coord.Lease
	renewedAt time.Time
	undead    bool
}

// renewConcurrency is how many of a pass's renewals are in flight at once.
//
// Renewed one at a time, seventy-five seats over a 100 ms link outran the
// fifteen-second TTL; sent together, a pass takes ⌈N/64⌉ round trips.
//
// 64 IS ABOVE WHAT THE SLOWEST LINK THE ENGINE ALLOWS CAN CARRY AT ONCE, so the
// link sets the pace, never this bound: at the object store's floor (34,952.5
// bytes a second) and a 0.6 s round trip it holds about 36 renewals of some
// 573 bytes. It is FAR BELOW WHAT THE REQUEST TERM COULD CARRY: a batch's last
// renewal waits behind 63 × 573 ≈ 36 KB, about 1.03 s at the floor, against
// the 4 s room (jsapi.RequestTerm − jsprovision.ReadTerm).
const renewConcurrency = 64

// renewal is one seat's renewal as the store answered it.
type renewal struct {
	// sent is the instant it was asked, the stamp a success records.
	sent  time.Time
	alive bool
	err   error
}

// Heartbeat renews every held lease and returns the seats that were LOST.
//
// A lost seat is dropped immediately and locally: a peer may already be
// running it, so anything this node does for it is a zombie's work. Fencing
// catches the writes; this stops the rest.
//
// The renewals go together, up to [renewConcurrency] in flight, each stamped
// at its own send ([Host.renewAll]); the answers are applied one at a time in
// the order [Host.heartbeatTargets] gives, so a decision about a seat does not
// depend on which answered first. THE BEAT IS STAMPED AS EACH ANSWER IS
// APPLIED: a release hook can wait out its own bound (five seconds), and a
// pass that dropped ten seats at the default TTL read as a wedge otherwise.
func (h *Host) Heartbeat(ctx context.Context) []string {
	h.beatMu.Lock()
	defer h.beatMu.Unlock()

	targets := h.heartbeatTargets()
	var lost []string

	// THE PRESENCE BESIDE THE SEATS, never after them: its claim is two
	// round trips and its status hook may spend a fifth of the heartbeat
	// ([StatusBudgetRatio]), and neither is a reason for a seat's renewal
	// to wait. It counts the seats the node holds as it is sent, so a pass
	// that takes a seat out of the count renews it once more below.
	var presence sync.WaitGroup
	presence.Go(func() {
		h.safely("seat_presence_renew_failed", func() { h.renewNodePresence(ctx) })
	})
	defer presence.Wait()
	// recounted is whether this pass took a seat out of what the presence
	// advertises (placedCount): lost it, or released or stopped renewing an
	// undead one.
	recounted := false

	answers, wait := h.renewAll(ctx, targets)
	defer wait()

	for i, t := range targets {
		r := <-answers[i]
		h.stampBeat()
		now, alive, err := r.sent, r.alive, r.err
		if err != nil {
			// Unknown is not lost — the row is untouched and still held, so
			// shedding on a blip would tear down a healthy node while no
			// peer could claim the seats anyway.
			//
			// But it is only "not lost" until the row's TTL runs out. Past
			// that the lease HAS lapsed, whatever this node can or cannot
			// see, and a peer may already be running the agent. Keeping the
			// seat on faith from there is how one unreachable store turns
			// into two nodes serving one seat, so the grace is bounded by
			// the same TTL the lease was granted with.
			age := elapsed(now, t.renewedAt)
			if age < h.ttl {
				log.WarnContext(ctx, "seat_heartbeat_unavailable", "seat", t.handle,
					"seconds_since_renew", age.Seconds(), "ttl_seconds", h.ttl.Seconds(),
					"error", err)
				h.noteAdmission(ctx, t.seat, false)
				continue
			}
			log.ErrorContext(ctx, "seat_dropped_unrenewable", "seat", t.handle,
				"seconds_since_renew", age.Seconds(), "ttl_seconds", h.ttl.Seconds(),
				"error", err,
				"hint", "the lease store has been unreachable for longer than the TTL, so this "+
					"lease has lapsed whether or not we can see it; dropping the seat rather "+
					"than risk running an agent a peer now owns")
			alive = false
		}

		if alive {
			h.recordRenew(t, now)
			if t.undead {
				// Still ours, so try again to close it. This is the only way
				// an undead seat ever returns to the fleet short of
				// restarting the process.
				if h.retryUndeadTeardown(ctx, t.handle) {
					recounted = true
				}
				continue
			}
			h.noteAdmission(ctx, t.seat, true)
			continue
		}

		if t.undead {
			// An undead seat whose lease is genuinely gone: the row has
			// moved on and a peer owns it now. Stop renewing.
			h.mu.Lock()
			if cur := h.undead[t.handle]; cur != nil && cur.held == t.entry {
				delete(h.undead, t.handle)
				recounted = true
			}
			h.mu.Unlock()
			log.ErrorContext(ctx, "seat_undead_lease_lost", "seat", t.handle, "epoch", t.lease.Epoch,
				"hint", "teardown was never proven and the lease has now moved to a peer; "+
					"this process may still be consuming the seat")
			continue
		}

		// A SEAT STILL BEING TAKEN IS CUT, never dropped here: dropping
		// takes the seat's lock, and the claim running its acquire hook
		// holds that lock for as long as the hook runs, which has no total.
		// Waiting on it froze this loop's beat — the watchdog ended the
		// process past a TTL, and on the boot's first pass, before the
		// watchdog is armed, every seat that pass had already attached went
		// unrenewed. The claim gives the cut seat back once its hook
		// returns ([Host.tryClaim]).
		if h.cutLostEstablishing(ctx, t) {
			lost = append(lost, t.handle)
			continue
		}
		if h.dropLostSeat(ctx, t) {
			lost = append(lost, t.handle)
		}
	}

	if len(lost) > 0 {
		// Amend the standing record rather than replace its list: a pass
		// that shed two seats followed by a heartbeat that lost a third
		// has lost three, and overwriting reported one.
		h.mu.Lock()
		if h.last != nil {
			for _, handle := range lost {
				if !slices.Contains(h.last.Lost, handle) {
					h.last.Lost = append(h.last.Lost, handle)
				}
			}
			h.last.Held = len(h.held)
		}
		h.mu.Unlock()
	}
	presence.Wait()
	if recounted || len(lost) > 0 {
		// THE COUNT THIS PASS LEAVES, advertised now rather than at the
		// next pass: the claim beside the seats may have read the count
		// before this pass took a seat out of it, and while the presence
		// still counted a seat this node gave back or lost, a peer with
		// room read the fleet as full and the seat sat unclaimed for a
		// renew period ([Host.fleetHoldsEverySeat]). Once, and only when
		// the count moved, so a pass that changed nothing costs nothing
		// more.
		h.stampBeat()
		h.safely("seat_presence_recount_failed", func() { h.renewNodePresence(ctx) })
	}
	h.stampBeat()
	return lost
}

// renewAll asks the store to renew every target, at most [renewConcurrency] at
// once, and hands back one answer channel per target, in the targets' order;
// wait returns once every renewal has answered and its goroutines have ended.
//
// THE CLOCK IS READ PER SEAT, AS IT IS SENT, never once for the pass, so a
// later batch's seat is not credited with an earlier renewal. THE BEAT IS
// STAMPED AS EACH RENEWAL IS ASKED AND ANSWERS: an unanswered one is held to
// its read's whole bound (five seconds), so a pass on a node that cannot
// reach its store spends that bound per batch, and stamped only at the end it
// read as a wedge once the bounds summed past the TTL. A renewal that hangs
// still freezes the beat, since [Host.Heartbeat] waits on each answer in
// order. A renewal that panics is answered as unknown, costing its own seat.
func (h *Host) renewAll(ctx context.Context, targets []heartbeatTarget) ([]chan renewal, func()) {
	answers := make([]chan renewal, len(targets))
	for i := range answers {
		answers[i] = make(chan renewal, 1)
	}
	var (
		next    atomic.Int64
		workers sync.WaitGroup
	)
	for range min(renewConcurrency, len(targets)) {
		workers.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(targets) {
					return
				}
				h.stampBeat()
				answers[i] <- h.renewOne(ctx, targets[i])
				h.stampBeat()
			}
		})
	}
	return answers, workers.Wait
}

// renewOne asks the store to renew one target's lease, stamped at its send.
func (h *Host) renewOne(ctx context.Context, t heartbeatTarget) (r renewal) {
	r.sent = h.now()
	defer func() {
		if p := recover(); p != nil {
			contained := panics.Recovered(panics.ScopeSeat, p)
			log.Error("seat_renew_panicked", "node", h.nodeID, "seat", t.handle, "panic", p,
				"stack", contained.Stack)
			r.alive, r.err = false, fmt.Errorf("renew of seat %q panicked: %v", t.handle, p)
		}
	}()
	r.alive, r.err = h.backend.Renew(ctx, t.lease.Resource, h.owner, t.lease.Epoch, h.ttl)
	return r
}

// heartbeatTargets snapshots what to renew: the living and the undead, in a
// stable order. The undead are renewed alongside the living because their
// teardown was never proven, so a peer must not be able to claim them.
//
// A seat cut while it was being taken is not renewed: its lease is lost, or
// the claim that took it is giving it back ([heldSeat.cut]).
func (h *Host) heartbeatTargets() []heartbeatTarget {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]heartbeatTarget, 0, len(h.held)+len(h.undead))
	for _, handle := range slices.Sorted(maps.Keys(h.held)) {
		e := h.held[handle]
		if e.isCut() {
			continue
		}
		out = append(out, heartbeatTarget{seat: e.seat, handle: handle, entry: e, lease: e.lease, renewedAt: e.renewedAt})
	}
	for _, handle := range slices.Sorted(maps.Keys(h.undead)) {
		e := h.undead[handle].held
		out = append(out, heartbeatTarget{seat: e.seat, handle: handle, entry: e, lease: e.lease, renewedAt: e.renewedAt, undead: true})
	}
	return out
}

// recordRenew stamps a successful renew, but only if this is still the live
// record: a sweep may have re-claimed the seat at a new epoch while the
// store call was in flight, and writing here would stamp an orphaned object
// while the live one kept the older timestamp.
func (h *Host) recordRenew(t heartbeatTarget, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.held[t.handle]
	if current == nil {
		if entry := h.undead[t.handle]; entry != nil {
			current = entry.held
		}
	}
	if current == t.entry {
		current.renewedAt = now
	}
}

// cutLostEstablishing cuts a seat whose lease is definitively gone while it is
// still being taken, reporting whether it was one: the seat this target names
// is still the one in the held set, and its acquire hook is still running.
//
// It never takes the seat's lock, which the claim holds for the whole hook:
// it ends the hook's context instead ([heldSeat.cut]), and the claim gives the
// seat back as never established and backs off it once the hook returns. A
// seat a drain cut already keeps that cause and is reported lost all the same,
// since its lease is.
func (h *Host) cutLostEstablishing(ctx context.Context, t heartbeatTarget) bool {
	h.mu.Lock()
	current := h.held[t.handle]
	if current != t.entry || !current.establishing {
		h.mu.Unlock()
		return false
	}
	cut := current.cutLocked(cutLeaseLost)
	h.mu.Unlock()
	if cut != nil {
		cut()
	}
	log.WarnContext(ctx, "seat_lease_lost", "seat", t.handle, "epoch", t.lease.Epoch,
		"establishing", true,
		"hint", "a peer may already own this seat; its acquire hook is cut, and the claim "+
			"that was taking it gives it back and stands back for one backoff")
	return true
}

// dropLostSeat gives up a seat whose lease is definitively gone, reporting
// whether this node actually held it at that epoch.
func (h *Host) dropLostSeat(ctx context.Context, t heartbeatTarget) bool {
	unlock := h.lockSeat(t.handle)
	defer unlock()

	h.mu.Lock()
	// Re-check under the lock: a sweep may have re-claimed this seat at a
	// NEW epoch between the failed renew and here, and tearing that down
	// would kill a claim this node legitimately holds.
	current := h.held[t.handle]
	if current == nil || current.lease.Epoch != t.lease.Epoch {
		h.mu.Unlock()
		return false
	}
	delete(h.held, t.handle)
	delete(h.unprovenAdmission, t.handle)
	// Stand back from this seat, the negative stickiness a failed acquire
	// gets: this node has just shown it cannot hold it, so a peer should get
	// the next attempt. Without it a node whose renews fail while its claims
	// succeed (a degraded store, as a timeout under load looks) re-takes the
	// seat on its next sweep and loses it a TTL on, an unbroken claim/lose
	// cycle that respawns the seat's runtime each time and abandons its
	// in-flight work while a healthy peer never wins a race.
	//
	// One backoff is AcquireBackoff, defaulting to the lease TTL: long enough
	// for a peer to claim and prove it can renew. A blip shorter than the TTL
	// never reaches here, because an unreachable store KEEPS the seat.
	h.acquireBackoffs[t.handle] = h.now().Add(h.acquireBackoff)
	h.mu.Unlock()

	log.WarnContext(ctx, "seat_lease_lost", "seat", t.handle, "epoch", t.lease.Epoch,
		"hint", "a peer may already own this seat; dropping it locally, and standing back "+
			"for one backoff so a peer gets the next attempt")

	// Fenced: a peer may already be running this seat, so in-flight work is
	// abandoned rather than finished. There is nothing to fail closed ON —
	// the lease is already gone — so an unproven teardown is logged, not
	// retained.
	if err := h.notifyRelease(ctx, t.seat, t.lease, ReasonLeaseLost); err != nil {
		log.ErrorContext(ctx, "seat_lost_release_unproven", "seat", t.handle, "epoch", t.lease.Epoch, "error", err,
			"hint", "the lease is already gone, so there is nothing to keep; this process may "+
				"still be consuming a seat a peer now owns")
	}
	return true
}
