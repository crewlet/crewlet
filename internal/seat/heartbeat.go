package seat

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// --- the loops ------------------------------------------------------------

// heartbeatLoop renews this node's leases, and is the duty the watchdog
// watches.
//
// It ticks far faster than it renews. The tick is what proves the goroutine
// is turning — a pass hung inside a hook or deadlocked on a lock freezes the
// stamp, and past the lease TTL that means this node's seats have moved to a
// peer while its queue client is still attached and still holding their
// mail. See [Watchdog] and adrs/301-watchdog.md.
func (h *Host) heartbeatLoop(ctx context.Context) {
	beat := h.beatInterval()
	ticksPerPass := int((h.heartbeat + beat - 1) / beat)
	if ticksPerPass < 1 {
		ticksPerPass = 1
	}
	ticker := time.NewTicker(beat)
	defer ticker.Stop()

	for ticks := 0; ; {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		h.stampBeat()
		ticks++
		if ticks < ticksPerPass {
			continue
		}
		ticks = 0
		h.safely("seat_heartbeat_tick_failed", func() { h.Heartbeat(ctx) })
		h.stampBeat()
	}
}

// beatInterval is how often the heartbeat goroutine proves it is turning.
//
// A CEILING scaled to the threshold the watchdog fires at, which is the
// lease TTL. Five beats must fit inside it so a healthy pass can never look
// stalled — at a tight TTL, a beat sized for production would have a
// perfectly healthy node report itself a whole threshold behind and shoot
// itself. Invisible at the shipped values (45 s vs 1 s) and lethal to anyone
// who lowers the TTL, so it is enforced rather than documented.
func (h *Host) beatInterval() time.Duration {
	d := WatchdogBeatInterval
	if scaled := h.ttl / WatchdogBeatsPerThreshold; scaled < d {
		d = scaled
	}
	// Never slower than the pass itself: a beat that lags the work it is
	// meant to prove would report a stall the node has not had.
	if h.heartbeat < d {
		d = h.heartbeat
	}
	if d <= 0 {
		d = time.Millisecond
	}
	return d
}

// --- the heartbeat --------------------------------------------------------

// heartbeatTarget is one seat to renew, snapshotted so the store call never
// runs while holding a lock.
type heartbeatTarget struct {
	handle    string
	entry     *heldSeat
	lease     coord.Lease
	renewedAt time.Time
	undead    bool
}

// Heartbeat renews every held lease and returns the seats that were LOST.
//
// A lost seat is dropped immediately and locally: its lease is gone and a
// peer may already be running it, so anything this node does for it from
// here is a zombie's work. Fencing catches the writes; this is what stops
// the rest.
func (h *Host) Heartbeat(ctx context.Context) []string {
	h.beatMu.Lock()
	defer h.beatMu.Unlock()

	targets := h.heartbeatTargets()
	var lost []string

	for _, t := range targets {
		// Read the clock PER SEAT, not once before the loop. A single
		// pre-loop timestamp is assigned to every seat's last-renew stamp,
		// so with many seats and a slow store the later ones record a renew
		// earlier than it happened — narrowing their grace, and their
		// admission window, as the seat count grows.
		now := h.now()

		alive, err := h.backend.Renew(ctx, t.lease.Resource, h.owner, t.lease.Epoch, h.ttl)
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
			elapsed := now.Sub(t.renewedAt)
			if elapsed < h.ttl {
				log.WarnContext(ctx, "seat_heartbeat_unavailable", "seat", t.handle,
					"seconds_since_renew", elapsed.Seconds(), "ttl_seconds", h.ttl.Seconds(),
					"error", err)
				h.noteAdmission(ctx, t.handle, false)
				continue
			}
			log.ErrorContext(ctx, "seat_dropped_unrenewable", "seat", t.handle,
				"seconds_since_renew", elapsed.Seconds(), "ttl_seconds", h.ttl.Seconds(),
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
				h.retryUndeadTeardown(ctx, t.handle)
				continue
			}
			h.noteAdmission(ctx, t.handle, true)
			continue
		}

		if t.undead {
			// An undead seat whose lease is genuinely gone: the row has
			// moved on and a peer owns it now. Stop renewing.
			h.mu.Lock()
			if cur := h.undead[t.handle]; cur != nil && cur.held == t.entry {
				delete(h.undead, t.handle)
			}
			h.mu.Unlock()
			log.ErrorContext(ctx, "seat_undead_lease_lost", "seat", t.handle, "epoch", t.lease.Epoch,
				"hint", "teardown was never proven and the lease has now moved to a peer; "+
					"this process may still be consuming the seat")
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
	h.renewNodePresence(ctx)
	h.stampBeat()
	return lost
}

// heartbeatTargets snapshots what to renew: the living and the undead, in a
// stable order. The undead are renewed alongside the living because their
// teardown was never proven, so a peer must not be able to claim them.
func (h *Host) heartbeatTargets() []heartbeatTarget {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]heartbeatTarget, 0, len(h.held)+len(h.undead))
	for _, handle := range slices.Sorted(maps.Keys(h.held)) {
		e := h.held[handle]
		out = append(out, heartbeatTarget{handle: handle, entry: e, lease: e.lease, renewedAt: e.renewedAt})
	}
	for _, handle := range slices.Sorted(maps.Keys(h.undead)) {
		e := h.undead[handle].held
		out = append(out, heartbeatTarget{handle: handle, entry: e, lease: e.lease, renewedAt: e.renewedAt, undead: true})
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
	// Stand back from this seat, the same negative stickiness a failed
	// acquire gets and for the same reason: this node has just demonstrated
	// it cannot hold this seat, so a peer should get the next attempt.
	//
	// Without it a node whose renews fail while its claims succeed — a
	// store degraded rather than down, which is what a timeout under load
	// looks like — re-takes the seat on its next sweep, roughly a hundred
	// milliseconds later, and loses it again one TTL on. Measured in the
	// fleet suite: an unbroken claim/lose cycle for as long as the
	// degradation lasts, tearing down and respawning that seat's whole
	// runtime every TTL and abandoning its in-flight work each time, while
	// a healthy peer that could serve it never wins a race. The seat is
	// nominally served the entire time.
	//
	// One backoff is AcquireBackoff, which defaults to the lease TTL —
	// exactly long enough for a peer to claim and prove it can renew. The
	// cost when there is no peer is one TTL of darkness on a seat this node
	// could not prove it owned anyway; a blip shorter than the TTL never
	// reaches here, because an unreachable store KEEPS the seat.
	h.acquireBackoffs[t.handle] = h.now().Add(h.acquireBackoff)
	h.mu.Unlock()

	log.WarnContext(ctx, "seat_lease_lost", "seat", t.handle, "epoch", t.lease.Epoch,
		"hint", "a peer may already own this seat; dropping it locally, and standing back "+
			"for one backoff so a peer gets the next attempt")

	// Fenced: a peer may already be running this seat, so in-flight work is
	// abandoned rather than finished. There is nothing to fail closed ON —
	// the lease is already gone — so an unproven teardown is logged, not
	// retained.
	if err := h.notifyRelease(ctx, t.handle, t.lease, ReasonLeaseLost); err != nil {
		log.ErrorContext(ctx, "seat_lost_release_unproven", "seat", t.handle, "epoch", t.lease.Epoch, "error", err,
			"hint", "the lease is already gone, so there is nothing to keep; this process may "+
				"still be consuming a seat a peer now owns")
	}
	return true
}
