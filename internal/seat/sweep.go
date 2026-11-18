package seat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// sweepLoop re-evaluates placement on its own cadence, separate from the
// heartbeat because the two answer different questions: the heartbeat keeps
// what this node has, the sweep looks for what it should take — and at once
// when [Host.Resweep] asks, at most once per interval.
//
// THE INTERVAL STARTS AGAIN after an asked-for pass, so it stands in for the
// tick and the claim limit still bounds the rate however many applies ask. An
// ask inside the last honoured one's interval waits for the tick. Wall-clock,
// never the injected clock the leases are judged by.
//
// A WITHHELD PASS SPENDS NOTHING when it gave nothing back ([SweepResult.idle]),
// or the ask that says the gate has opened ([Config.Ready]) would wait.
func (h *Host) sweepLoop(ctx context.Context, asks <-chan struct{}) {
	ticker := time.NewTicker(h.sweepEvery)
	defer ticker.Stop()
	var asked time.Time
	for {
		honoured := false
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-asks:
			if !asked.IsZero() && time.Since(asked) < h.sweepEvery {
				continue
			}
			asked, honoured = time.Now(), true
			ticker.Reset(h.sweepEvery)
		}
		var result SweepResult
		h.safely("seat_sweep_tick_failed", func() { result = h.Sweep(ctx) })
		if honoured && result.idle() {
			asked = time.Time{}
		}
	}
}

// Resweep asks for a placement pass now rather than at the sweep's next tick,
// without waiting for it. For a change to the seats [Config.Seats] answers
// (a role added or removed, a placement moved, a node's first company), which
// the tick would see up to [SweepInterval] later, and for the claim gate
// opening ([Config.Ready]), which only the gate's owner can see.
//
// DROPPED BY A HOST THAT IS NOT RUNNING, whose [Host.Start] begins with a pass
// of its own. Kept, an ask made before the start would run a second pass with
// nothing new to see and spend the one ask an interval honours, so the first
// apply after boot would wait for the tick. Coalesced while running: an ask
// that finds one pending adds nothing, and one inside the interval of the last
// honoured waits for the tick ([Host.sweepLoop]).
func (h *Host) Resweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resweep == nil {
		return
	}
	select {
	case h.resweep <- struct{}{}:
	default:
	}
}

// --- the sweep ------------------------------------------------------------

// Sweep runs one placement pass: converge on a fair share, rate-limited,
// in BOTH directions.
//
// Claiming alone is only half of placement, and the half that does nothing
// for a fleet that grows: a node that booted alone holds every seat, and
// every peer that joins later computes a share it can never reach because
// the seats it should take are already held by a node with no reason to let
// go. Scaling out would then do nothing at all until something died.
func (h *Host) Sweep(ctx context.Context) SweepResult {
	h.sweepMu.Lock()
	defer h.sweepMu.Unlock()

	seats, ok := h.currentSeats()
	if !ok {
		// The org could not be read. Returning an empty seat list here
		// would decommission every role on this node — the one misreading
		// with a catastrophic direction — so the pass does nothing at all
		// and the next one tries again.
		last, _ := h.LastSweep()
		return last
	}

	draining := h.Draining()

	// WHETHER THIS NODE CAN SERVE ITS SEATS AT ALL, asked BEFORE the plan,
	// because the answer changes the plan: a node that cannot serve its
	// seats' work gives every one of them back and claims none, so it is not
	// placing seats, and it says so to its peers ([placement.NodeProfile.Withdrawn])
	// rather than leaving them to keep its share free for it.
	//
	// It is a separate question from the readiness gate lower down because
	// the two have opposite directions: readiness withholds CLAIMS and
	// deliberately keeps what is held, and this gives back what is held
	// whether or not anything is claimable. See [Config.Serviceable]. Not
	// asked while draining, which gives everything back anyway.
	var unfit bool
	var unfitReason string
	if !draining {
		unfit, unfitReason = h.unserviceable(ctx)
	}
	h.setWithdrawn(ctx, unfit)

	plan, liveNodes, peers := h.plan(ctx, seats, unfit)

	byHandle := make(map[string]placement.Seat, len(seats))
	placeable := make(map[string]struct{}, len(seats))
	for _, s := range seats {
		byHandle[s.Handle] = s
		placeable[s.Handle] = struct{}{}
	}
	for _, handle := range plan.Unplaceable {
		delete(placeable, handle)
	}
	h.mu.Lock()
	h.placeable = placeable
	h.mu.Unlock()
	eligible := make(map[string]struct{}, len(plan.Eligible))
	for _, handle := range plan.Eligible {
		eligible[handle] = struct{}{}
	}

	var released []string
	for _, handle := range h.Held() {
		seat, still := byHandle[handle]
		if !still {
			// A role the org no longer has: a live config apply deleted it,
			// and holding its lease afterwards would look like ownership of
			// something that no longer exists.
			log.InfoContext(ctx, "seat_released_role_gone", "seat", handle)
			// Only if the release was PROVEN. Release reports false when
			// teardown could not be confirmed and the seat went undead —
			// still leased, still renewed, and reported as released is
			// exactly backwards.
			if h.Release(ctx, handle, ReasonRoleGone) {
				released = append(released, handle)
			}
			continue
		}
		if unfit {
			// Eligible for nothing while withdrawn, which is not a
			// placement change: the unserviceable shed below gives it back
			// under its own reason.
			continue
		}
		if _, may := eligible[handle]; !may {
			// Still a seat, no longer OURS to run: the placement selector
			// changed under a live apply, or this node's labels or roles
			// did. Released before the capacity shed, because an ineligible
			// seat is not a question of how many we hold — no amount of
			// spare capacity makes it ours again, and holding it means an
			// eligible peer cannot take it.
			log.InfoContext(ctx, "seat_released_not_placeable_here", "seat", handle, "node", h.nodeID,
				"placement", seat.Placement.String())
			if h.Release(ctx, handle, ReasonPlacement) {
				released = append(released, handle)
			}
		}
	}

	// THE UNSERVICEABLE SHED, BEFORE the capacity one. A node that cannot
	// serve its seats' work gives back EVERY seat, so there is no share
	// left to converge on and the capacity pass below has nothing to
	// divide.
	if unfit {
		for _, handle := range h.Held() {
			if h.Release(ctx, handle, ReasonUnserviceable) {
				released = append(released, handle)
			}
		}
		// EVERY PASS while it holds, at WARN: this is a node running no
		// work at all, and an operator looking at an idle node needs the
		// reason on the node rather than in a fleet-wide alarm they have to
		// go and correlate.
		log.WarnContext(ctx, "seats_shed_unserviceable", "node", h.nodeID,
			"reason", unfitReason, "released", len(released),
			"hint", "this node cannot serve its seats' work at all, so its "+
				"seats move to a peer that can; it reclaims them when this "+
				"clears")
	}

	if !draining {
		released = append(released, h.shedToCapacity(ctx, plan)...)
	}

	var claimed []string
	var blocked int
	var fleetFull bool
	// AFTER the shed above, deliberately: a node that is not ready must
	// still give back seats it holds beyond its share, or a fleet whose
	// newest member is mid-hydration cannot rebalance onto it and its
	// oldest member stays over-subscribed for as long as that takes.
	withheld := !draining && (unfit || !h.admits(ctx))
	if !draining && !withheld {
		// ONE BOUND PER GROUP, and nothing else: room in one group is never
		// room in another, or a node matching a pinned seat's group and the
		// unpinned one fills up on unpinned seats and strands the pinned one.
		// The undead seats are already in it — [placement.Plan.Room]
		// charges them first and squeezes the least constrained groups —
		// so there is no total beside it to disagree with (see the
		// placement package doc).
		h.mu.Lock()
		groupRoom := h.roomLocked(plan)
		h.mu.Unlock()
		room := 0
		for _, r := range groupRoom {
			room += max(r, 0)
		}
		room = min(room, h.claimLimit)
		switch {
		case room <= 0:
		case h.fleetHoldsEverySeat(seats, plan, peers):
			// NOTHING IS FREE, and the fleet's own counts say so: see
			// [Host.fleetHoldsEverySeat]. Trying every seat to learn it
			// is the standing cost this skips.
			fleetFull = true
		default:
			claimed, blocked = h.claimUpTo(ctx, byHandle, plan.Groups, groupRoom, room)
		}
	}

	// WHAT THIS PASS TOOK, ADVERTISED NOW rather than at the next
	// heartbeat, so a peer deciding whether anything is free counts it:
	// otherwise that peer goes on trying every seat for up to a heartbeat.
	// What a pass GIVES BACK is advertised by the release itself (see
	// [Host.finishRelease]), whichever path releases it.
	if len(claimed) > 0 {
		h.renewNodePresence(ctx)
	}

	if withheld {
		// EVERY PASS, at debug: this is a state a node sits in for
		// minutes on a large company, and an operator asking why a fresh
		// node is holding nothing needs one line that says so rather
		// than a healthy-looking sweep with an empty claim list.
		//
		// UNDER THE LOCK, like every other read of this map. It is only a
		// log field, which is exactly why it was easy to write without
		// one — and `len` on a map is a read the detector counts: a
		// release running concurrently deletes from it, and the two
		// raced in CI.
		h.mu.Lock()
		holding := len(h.held)
		h.mu.Unlock()
		log.DebugContext(ctx, "seat_claims_withheld", "node", h.nodeID,
			"held", holding, "capacity", plan.Capacity,
			"hint", "this node is not ready to run new seats yet; it keeps what "+
				"it holds and claims nothing until it is")
	}

	if len(plan.Unplaceable) > 0 {
		log.WarnContext(ctx, "seats_unplaceable", "node", h.nodeID, "seats", plan.Unplaceable,
			"hint", "no live node placing seats matches these seats' placement, so nothing "+
				"is serving them. Start a node that matches (or bring back the one that "+
				"matches and has withdrawn), or widen the selector.")
	}

	h.pruneSeatLocks(byHandle)

	h.mu.Lock()
	result := SweepResult{
		Held:              len(h.held),
		Capacity:          plan.Capacity,
		LiveNodes:         liveNodes,
		Claimed:           claimed,
		Lost:              released,
		Unplaceable:       plan.Unplaceable,
		BlockedByProtocol: blocked,
		Withheld:          withheld,
		FleetFull:         fleetFull,
	}
	// A copy, never &result: a heartbeat appends to the stored record, and
	// the value returned here would otherwise be the same object.
	stored := result.clone()
	h.last = &stored
	h.mu.Unlock()

	if len(claimed) > 0 || len(released) > 0 || blocked > 0 {
		log.InfoContext(ctx, "seat_sweep", "node", h.nodeID, "held", result.Held, "capacity", plan.Capacity,
			"live_nodes", liveNodes, "claimed", claimed, "released", released,
			"blocked_by_protocol", blocked)
	}
	return result
}

// setWithdrawn records whether this node is placing seats, and tells its peers
// the moment that changes rather than at the next heartbeat.
//
// THE PEERS ARE WHO NEED IT. A node that cannot serve its seats gives them all
// back and claims none, but its presence lease stays live — it is still a
// member of the fleet — and a peer that went on counting it would leave its
// share of every group free for it: those seats would sit unclaimed with no
// node reporting anything, and a seat only it matches would never be called
// unplaceable. Advertised, its peers divide by the nodes actually placing
// seats and take the seats up, and a group only it matches is reported.
func (h *Host) setWithdrawn(ctx context.Context, withdrawn bool) {
	h.mu.Lock()
	changed := h.withdrawn != withdrawn
	h.withdrawn = withdrawn
	h.mu.Unlock()
	if changed {
		h.renewNodePresence(ctx)
	}
}

// currentSeats reads the org, converting a panicking provider into a pass
// that does nothing rather than one that reads "the company has no seats".
func (h *Host) currentSeats() (seats []placement.Seat, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeSeat, r)
			log.Error("seat_lookup_failed", "node", h.nodeID, "panic", r,
				"stack", contained.Stack,
				"hint", "the org could not be read, so this pass changed nothing; releasing "+
					"every seat on an unreadable org would decommission the whole node")
			seats, ok = nil, false
		}
	}()
	return h.seats(), true
}

// pruneSeatLocks drops locks for seats this node neither holds nor could
// claim. One mutex accumulated per handle ever seen, including every seat
// removed by a live config apply — unbounded growth in a long-lived process
// that reconfigures often.
func (h *Host) pruneSeatLocks(seats map[string]placement.Seat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for handle, lock := range h.seatLocks {
		if _, keep := seats[handle]; keep {
			continue
		}
		if _, keep := h.held[handle]; keep {
			continue
		}
		if _, keep := h.undead[handle]; keep {
			continue
		}
		// Never drop one somebody is queued on: the next caller would mint
		// a second mutex for the same seat and the two would not exclude
		// each other, which is the whole reason the lock exists.
		if lock.waiters == 0 {
			delete(h.seatLocks, handle)
		}
	}
}

// shedToCapacity hands back seats held above this node's fair share.
//
// When a peer joins every incumbent's share drops, and only this acts on it.
// PER GROUP, like claiming: room is not fungible across groups (placement
// package doc), so a node over its share of one group gives back seats of THAT
// group whatever room it has in another.
//
// AND THE UNDEAD, through the same number: a seat whose teardown could not be
// proven still counts, and [placement.Plan.Room] charges it first. VOLUNTARY:
// the in-flight turn finishes before the consumer detaches. CONVERGENT: shares
// are ceil(group size / eligible nodes), so a shed node cannot re-claim.
func (h *Host) shedToCapacity(ctx context.Context, plan placement.Plan) []string {
	h.mu.Lock()
	candidates := h.overCapacityLocked(plan)
	h.mu.Unlock()

	limit := min(h.releaseLimit, len(candidates))
	var shed []string
	for _, handle := range candidates[:limit] {
		h.mu.Lock()
		held := len(h.held)
		h.mu.Unlock()
		log.InfoContext(ctx, "seat_released_over_capacity", "seat", handle, "held", held, "capacity", plan.Capacity)
		h.Release(ctx, handle, ReasonDrain)

		// A release whose teardown could not be proven keeps the lease —
		// the seat leaves the held set but lands among the undead, so no
		// peer can take it. Reporting that as shed would claim the
		// rebalance made room it did not make, and this node would shed a
		// second seat next sweep on top of one it is still serving.
		h.mu.Lock()
		_, stillHeld := h.held[handle]
		_, stillDead := h.undead[handle]
		h.mu.Unlock()
		if !stillHeld && !stillDead {
			shed = append(shed, handle)
		}
	}
	return shed
}

// overCapacityLocked is every running seat this node holds beyond what
// [placement.Plan.Room] lets it keep, in the order they are given back:
// least constrained groups first, so when the release limit cuts a pass
// short a pinned seat — the one with the fewest other homes — is the last
// to go. The caller holds h.mu.
//
// Sorted within a group, and NOT ordered by the preferred hint the way
// claiming is. The hint records the last node to claim a seat, which for
// every seat this node holds is this node — so it cannot discriminate among
// them, and ordering by it would only look like it was doing something. A
// stable order is what matters: it makes a shed reproducible, and two nodes
// rebalancing at once hold disjoint sets, so they cannot collide.
func (h *Host) overCapacityLocked(plan placement.Plan) []string {
	running := make([][]string, len(plan.Groups))
	for i, g := range plan.Groups {
		for _, handle := range g.Handles {
			if _, ok := h.held[handle]; ok {
				running[i] = append(running[i], handle)
			}
		}
		slices.Sort(running[i])
	}

	room := h.roomLocked(plan)
	var out []string
	for i := len(room) - 1; i >= 0; i-- {
		// Room only ever reads negative by running seats of the group —
		// the undead ones are what it charged first, not what it counts
		// down — so the surplus is always theirs to give.
		out = append(out, running[i][:max(-room[i], 0)]...)
	}
	return out
}

// roomLocked is [placement.Plan.Room] over what this node holds: the seats it
// runs, and the undead ones it cannot give back. The caller holds h.mu.
func (h *Host) roomLocked(plan placement.Plan) []int {
	return plan.Room(func(handle string) bool {
		_, ok := h.held[handle]
		return ok
	}, slices.Collect(maps.Keys(h.undead)))
}

// claimUpTo takes at most room seats in all and at most groupRoom[i] of
// groups[i], and reports the fleet's protocol floor when the mixed-version
// gate stopped it.
//
// A SEAT IS TAKEN ONLY AGAINST ITS OWN GROUP'S ROOM, never another group's
// spare share, or a pinned seat would be stranded. THE FLOOR IS ASKED ONLY WHEN
// THE GATE REFUSED A CLAIM, never because a pass took nothing: it spans every
// live lease, so asked each sweep the node took in the fleet's heartbeats (about
// 670 messages a second at ten thousand seats). A GATE REFUSAL ENDS THE PASS:
// the gate is fleet-wide (coord.Refusal).
//
// byHandle maps handle to seat: a claim needs the id its lease is named by.
func (h *Host) claimUpTo(ctx context.Context, byHandle map[string]placement.Seat,
	groups []placement.Group, groupRoom []int, room int) ([]string, int) {

	var claimed []string
	for _, c := range h.claimOrder(ctx, byHandle, groups, groupRoom) {
		if len(claimed) >= room {
			break
		}
		if groupRoom[c.group] <= 0 {
			continue
		}
		took, refused, stop := h.tryClaim(ctx, c.seat)
		if took {
			claimed = append(claimed, c.seat.Handle)
			groupRoom[c.group]--
		}
		if refused == coord.RefusedProtocol {
			// "An older-protocol node is live and this build refuses to
			// claim beside it" — an upgrade that has stalled, and
			// invisible without this — rather than "peers hold
			// everything", which is normal.
			return claimed, h.protocolBlock(ctx)
		}
		if stop {
			break
		}
	}
	return claimed, 0
}

// tryClaim takes one seat, reporting whether it was established, why it was
// refused if it was, and whether the pass must stop claiming altogether.
func (h *Host) tryClaim(ctx context.Context, seat placement.Seat) (took bool, refused coord.Refusal, stop bool) {
	handle := seat.Handle
	// A SEAT WITH NO ID CANNOT BE CLAIMED, and refusing here is what keeps
	// that loud: its lease resource, its mailbox subject and its consumer
	// group are all built from the id, so claiming it would take a lease
	// nothing can name and attach a consumer to a subject nothing publishes
	// to — a seat that reads as served and receives nothing.
	if handle == "" || seat.ID == uuid.Nil {
		log.WarnContext(ctx, "seat_claim_unidentified", "seat", handle,
			"hint", "this seat has no derivable id, so it has no lease and no "+
				"mailbox; a human seat is never claimed, and an agent seat "+
				"without one is a company whose name or handle derives nothing")
		return false, "", false
	}
	unlock := h.lockSeat(handle)
	defer unlock()

	h.mu.Lock()
	_, alreadyHeld := h.held[handle]
	_, alreadyDead := h.undead[handle]
	draining := h.draining
	h.mu.Unlock()
	// A DRAIN THAT BEGAN AFTER THIS PASS READ THE HOST ends the pass. The
	// pass decided to claim from a host that was not draining, and a drain
	// begun since has handed its seats back — under this same seat lock, so
	// a claim waiting on it would take straight back the seat the drain had
	// just let go, onto a node that is leaving: attached after the drain's
	// quiesce, held through the teardown, and, when the stop cancelled the
	// pass mid-acquire, left to lapse on its TTL.
	if draining {
		return false, "", true
	}
	if alreadyHeld || alreadyDead {
		return false, "", false // re-claimed under us while we waited
	}

	// BEFORE THE CALL, as a renew and the presence claim take theirs: the
	// store counts the lease's TTL from the moment it grants it, somewhere
	// inside this call, so a stamp read once a slow store has answered is
	// later than the store's by the round trip — and turn admission
	// ([Host.MayStart]), [Host.Provable] and the unreachable-store grace
	// would all credit this node with that much time it never held, on the
	// one stamp nothing has renewed since.
	asked := h.now()
	lease, refused, err := h.backend.TryAcquire(ctx, coord.SeatResource(seat.ID), coord.AcquireOptions{
		Owner: h.owner,
		TTL:   h.ttl,
		// The STABLE node id, not the incarnation: the hint has to survive
		// this process to be worth anything.
		Preferred: h.nodeID,
		Protocol:  h.protocol,
		// No meta. A seat lease says nothing about what a node IS, and a
		// payload sent here would be a second, staler answer to the
		// question the presence row already answers.
	})
	if err != nil {
		// The store is unreachable, which says nothing about who owns what.
		// Stop claiming — never take a seat on unknown state — and report
		// what this pass actually got. Distinct from a nil lease, which is
		// a real refusal by a peer and only skips THIS seat.
		log.WarnContext(ctx, "seat_claim_unavailable", "seat", handle, "error", err)
		return false, "", true
	}
	if lease == nil {
		return false, refused, false
	}

	// Held from here so the heartbeat renews it while the hook runs, but
	// ESTABLISHING so nothing may start a turn on it yet. See heldSeat.
	//
	// UNLESS A DRAIN BEGAN WHILE THE LEASE WAS BEING TAKEN, decided under
	// the lock [Host.BeginDrain] sets its flag under: a seat entered here
	// before the flag is one the drain's release reads in the held set and
	// gives back, and once the flag is set nothing is entered — so no claim
	// can slip between the drain's decision and its release. The lease
	// just taken goes straight back, with nothing attached to it, as a step
	// of the drain's stop ([Host.drainStop]).
	h.mu.Lock()
	if h.draining {
		drainStop := h.drainStop
		h.mu.Unlock()
		h.giveBackUnattached(WithStopBudget(ctx, drainStop), handle, *lease)
		return false, "", true
	}

	// ON A CONTEXT OF ITS OWN, which the heartbeat and a drain can end without
	// this seat's lock ([heldSeat.cut]): a lease lost or a drain begun while
	// the hook runs CUTS it, where they used to wait on the lock for a hook
	// with no total. SETTLED once this claim has established the seat or
	// given it back, which is what a drain that cut it waits for
	// ([Host.AwaitCut]).
	hookCtx, cut := context.WithCancel(ctx)
	defer cut()
	settled := make(chan struct{})
	defer close(settled)
	entry := &heldSeat{seat: seat, lease: *lease, renewedAt: asked, establishing: true,
		cut: cut, settled: settled}
	h.held[handle] = entry
	h.mu.Unlock()
	log.InfoContext(ctx, "seat_claimed", "seat", handle, "epoch", lease.Epoch)

	err = h.notifyAcquire(hookCtx, seat, *lease)
	// THE CUT IS READ AND THE SEAT ESTABLISHED UNDER ONE LOCK, the one a cut is
	// made under, so a seat is either cut or established and never both. A
	// CUT SEAT IS NEVER ESTABLISHED, whatever its hook returned: a hook that
	// ignored its context and finished is building on a lease that is lost or
	// being given back.
	h.mu.Lock()
	cutBy, drainStop := entry.cutBy, h.drainStop
	if cutBy == "" && err == nil {
		entry.establishing = false
		entry.cut = nil
	}
	h.mu.Unlock()
	if cutBy != "" && err == nil {
		err = fmt.Errorf("%w: %s", errSeatCut, cutBy)
	}

	if err != nil {
		// A seat whose takeover pipeline failed must not stay claimed: it
		// would look owned to the fleet while nothing runs it. Give it
		// straight back so a peer can try — and back off here, because
		// retrying a config-shaped failure every 5 s spins at the cost of
		// an MCP fork each time.
		//
		// A CUT IS SAID AS ONE, not as a hook that failed: a drain's is the
		// node leaving, and a lost lease's is a peer holding the seat. The
		// drain's alone is not backed off, because the node is leaving and a
		// backoff speaks only about a claim it will not make; a lost lease
		// is, as [Host.dropLostSeat] backs off any lease it lost.
		switch cutBy {
		case cutDrained:
			log.InfoContext(ctx, "seat_acquire_cut", "seat", handle, "epoch", lease.Epoch,
				"cause", string(cutBy), "error", err)
		case cutLeaseLost:
			log.WarnContext(ctx, "seat_acquire_cut", "seat", handle, "epoch", lease.Epoch,
				"cause", string(cutBy), "error", err,
				"hint", "the seat's lease was lost while it was being taken, so a peer may "+
					"hold it; this node gives it back and stands back for one backoff")
		default:
			log.ErrorContext(ctx, "seat_acquire_hook_failed", "seat", handle, "epoch", lease.Epoch, "error", err)
		}
		if cutBy != cutDrained {
			h.mu.Lock()
			h.acquireBackoffs[handle] = h.now().Add(h.acquireBackoff)
			h.mu.Unlock()
		}
		// Fenced, and already inside this seat's lock. The reason tells the
		// hook the seat was never fully established, so it must tolerate
		// half-spawned children and a consumer that was never attached.
		//
		// NOT ON THE PASS'S CONTEXT, for the reason [Host.Stop] gives: a hook
		// commonly fails because the host is stopping, and a give-back that
		// inherits the cancelled pass releases nothing.
		//
		// A SEAT A DRAIN CUT is given back as a step of the drain's stop
		// ([Host.drainStop]): on an allowance of its own, a stop against an
		// unreachable store would spend one more per seat cut. Any other
		// give-back here is bounded as one that begins a stop.
		stop := context.WithoutCancel(ctx)
		if cutBy == cutDrained {
			stop = WithStopBudget(stop, drainStop)
		}
		h.releaseLocked(WithinStop(stop, h.ttl), handle, ReasonAcquireFailed)
		return false, "", false
	}

	// THE EDGE INTO ADMISSION, said out loud: nothing else reports it, and
	// whatever the acquire hook was refused while the seat established is
	// waiting on exactly this. See [Hooks.OnEstablished].
	h.notifyEstablished(ctx, seat, *lease)

	// Counted as claimed only once the seat is ESTABLISHED. The hook gives
	// a failed takeover straight back — a bad MCP command, a credential
	// resolving to nothing — so counting it earlier meant a seat nothing
	// runs still burned a claim slot and still logged as claimed.
	return true, "", false
}

// giveBackUnattached hands back a seat lease this node took and never
// entered in its held set, so nothing — no hook, no consumer — was attached to
// it. The caller holds the seat's lock.
//
// On a context free of the pass's cancellation, bounded as every give-back of
// a stop is ([WithinStop]), because the pass that took it is one a stopping
// host cancels; past the bound the lease lapses on its TTL, the outcome of not
// trying.
func (h *Host) giveBackUnattached(ctx context.Context, handle string, lease coord.Lease) {
	stepCtx, done := StopStep(WithinStop(context.WithoutCancel(ctx), h.ttl))
	defer done()
	if _, err := h.backend.Release(stepCtx, lease.Resource, h.owner, lease.Epoch); err != nil {
		log.WarnContext(ctx, "seat_release_unavailable", "seat", handle, "error", err)
	}
}

// candidate is a seat a pass may try, and the index of its group in the
// plan — the room it is claimed against.
type candidate struct {
	seat  placement.Seat
	group int
}

// claimOrder is the unheld seats this node may run, in the groups it has room
// in, most constrained group first and its preferred ones first within each.
//
// The plan's groups arrive already filtered by placement: eligibility is not a
// preference to sort. A group with no room contributes nothing.
//
// GROUPS BEFORE HINTS: fewest eligible nodes first, so a claim limit that cuts a
// pass short has taken the seats with the fewest other homes. The hint orders
// seats WITHIN a group, stickiness only: never a claim precondition or a reason
// to skip, since it outlives the node that set it. Sorted so every node walks
// the list alike and none collides forever; recent failures wait out a backoff.
func (h *Host) claimOrder(ctx context.Context, byHandle map[string]placement.Seat,
	groups []placement.Group, groupRoom []int) []candidate {

	now := h.now()
	h.mu.Lock()
	for handle, until := range h.acquireBackoffs {
		if !until.After(now) {
			delete(h.acquireBackoffs, handle)
		}
	}
	perGroup := make([][]string, len(groups))
	total := 0
	orderable := false
	for i, g := range groups {
		if groupRoom[i] <= 0 {
			continue
		}
		for _, handle := range g.Handles {
			if _, skip := h.held[handle]; skip {
				continue
			}
			if _, skip := h.undead[handle]; skip {
				continue
			}
			if _, skip := h.acquireBackoffs[handle]; skip {
				continue
			}
			perGroup[i] = append(perGroup[i], handle)
		}
		slices.Sort(perGroup[i])
		total += len(perGroup[i])
		orderable = orderable || len(perGroup[i]) >= 2
	}
	h.mu.Unlock()

	// NOTHING TO ORDER, so nothing to read. The hint orders seats within a
	// group, so a pass where no group has two candidates has exactly one
	// ordering, and the hint read below is a walk of the epochs
	// bucket — which has no TTL and is never pruned, so it holds a record
	// for every resource the deployment has ever leased. Paying that on the
	// five-second sweep to sort a list that cannot be sorted is waste in the
	// one state it is most likely to be in: a node one seat short of
	// capacity, or one whose every remaining candidate is in acquire
	// backoff. The second is the case the backoff exists to calm, so
	// spending a full-bucket read there works directly against it.
	var hinted map[string]struct{}
	if orderable {
		got, err := h.backend.PreferredResources(ctx, coord.ClassSeat, h.nodeID)
		if err == nil {
			hinted = got
		}
	}

	out := make([]candidate, 0, total)
	for i, handles := range perGroup {
		var rest []candidate
		for _, handle := range handles {
			c := candidate{seat: byHandle[handle], group: i}
			// The hint is on the LEASE, which is named by the seat's id.
			if _, ok := hinted[coord.SeatResource(c.seat.ID)]; ok {
				out = append(out, c)
				continue
			}
			rest = append(rest, c)
		}
		out = append(out, rest...)
	}
	return out
}

// protocolBlock reports the fleet's protocol floor once the mixed-version gate
// has refused one of this pass's claims, and zero when the floor it names is no
// longer below this node's.
//
// Asked only then, which is what makes it cheap: the claim the gate refused
// has just judged the same view, so the floor is read from a view that is
// already running rather than one started to answer it.
func (h *Host) protocolBlock(ctx context.Context) int {
	const hint = "an older-protocol node is still live or still holds seats; this node will claim nothing " +
		"until it drains. Finish the rolling upgrade — do NOT roll back across a protocol " +
		"bump without stopping the fleet first."
	floor, found, err := h.backend.FleetProtocolFloor(ctx)
	switch {
	case err != nil:
		// The gate DID refuse — that much the claim said — so the stall
		// is reported even when the floor to name could not be read.
		log.WarnContext(ctx, "seat_claims_blocked_by_older_protocol", "node", h.nodeID,
			"fleet_floor", "unknown", "this_node", h.protocol, "error", err, "hint", hint)
		return 0
	case !found || floor >= h.protocol:
		// The older lease went between the refusal and this read: the
		// next pass claims.
		return 0
	}
	log.WarnContext(ctx, "seat_claims_blocked_by_older_protocol", "node", h.nodeID,
		"fleet_floor", floor, "this_node", h.protocol, "hint", hint)
	return floor
}

// --- membership -----------------------------------------------------------

// plan works out what this node may claim and how much, plus the live
// seat-running node count.
//
// When the fleet cannot be read, the LAST KNOWN membership is reused rather
// than assuming a fleet of one: a partial store failure would otherwise turn
// every node into "I should own everything", and whoever sweeps first would
// take the claim limit each pass until it holds the lot. Before the first
// successful read a fleet of one, this node, is the honest assumption.
//
// The third result is the PEERS AS READ BY THIS PASS, and nil when the read
// failed: a stale roster is good enough to size a share by, and not good
// enough to conclude from that nothing is free ([Host.fleetHoldsEverySeat]).
func (h *Host) plan(ctx context.Context, seats []placement.Seat,
	withdrawn bool) (placement.Plan, int, []placement.NodeProfile) {

	var live, peers []placement.NodeProfile

	leases, err := h.backend.ListLive(ctx, coord.ClassNode)
	if err != nil {
		h.mu.Lock()
		live = slices.Clone(h.liveProfiles)
		h.mu.Unlock()
		log.WarnContext(ctx, "seat_capacity_unavailable", "node", h.nodeID,
			"assumed_live_nodes", max(1, len(live)), "error", err)
	} else {
		live = make([]placement.NodeProfile, 0, len(leases))
		for _, lease := range leases {
			if profile, ok := placement.FromLease(lease); ok {
				live = append(live, profile)
			}
		}
		h.mu.Lock()
		h.liveProfiles = live
		h.mu.Unlock()
		peers = live
	}

	me := h.profile
	me.Withdrawn = withdrawn
	plan := placement.Compute(seats, me, live)
	h.checkFleetRoles(append(slices.Clone(live), me))
	return plan, plan.SeatNodes, peers
}

// fleetHoldsEverySeat reports whether the live seat-running nodes, by their
// own counts, hold every seat any of them may run — in which case a pass with
// room has nothing to claim and tries nothing.
//
// It asks the advertised counts rather than trying every seat (about four
// thousand messages per pass at two thousand seats). Safe to be wrong: a claim
// is still the lease's compare-and-set, and it errs toward TRYING. It reads FULL
// with a seat free only while a count outlives what its node holds, which a
// renewal or release ([Host.finishRelease]) corrects. Only leases on placeable
// seats count, or an undead seat whose role is gone would stand in for a free
// one ([Host.placedCount]). OWN COUNT FROM MEMORY, never this node's own row, a
// renewal old.
func (h *Host) fleetHoldsEverySeat(seats []placement.Seat, plan placement.Plan,
	peers []placement.NodeProfile) bool {

	if peers == nil {
		return false
	}
	held := h.placedCount()
	for _, p := range peers {
		if p.ID == h.nodeID || !p.RunsSeats() {
			continue
		}
		held += p.Held
	}
	return held >= len(seats)-len(plan.Unplaceable)
}

// placedCount is how many seat leases this node holds on seats some live node
// may run ([Host.placeable]): the seats it runs and the undead ones it is
// still renewing, less any lease on a seat outside that set.
func (h *Host) placedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for handle := range h.placeable {
		if _, ok := h.held[handle]; ok {
			n++
		} else if _, ok := h.undead[handle]; ok {
			n++
		}
	}
	return n
}

// checkFleetRoles says something when the fleet has nobody doing one of the
// jobs.
//
// A node's roles subtract a duty from THAT NODE, never the company, so node by
// node a fleet can reach a shape where a whole job is done by nobody and no
// config is wrong; the symptoms are silent absences. Edge-triggered: said once,
// and again when it comes back.
//
// ONLY A ROLE THE COMPANY NEEDS ([Config.RoleNeeded]). One that stops being
// needed while unmanned says fleet_role_unneeded, never fleet_role_manned: an
// operator who turned code off must not read that a Gitlet node came back.
func (h *Host) checkFleetRoles(live []placement.NodeProfile) {
	edges := h.fleetRoleEdges(live)
	ids := map[string]struct{}{}
	for _, node := range live {
		ids[node.ID] = struct{}{}
	}
	for _, role := range edges.unmanned {
		log.Warn("fleet_role_unmanned", "node", h.nodeID, "role", string(role),
			"live_nodes", len(ids), "hint", unmannedHints[role])
	}
	for _, role := range edges.unneeded {
		log.Info("fleet_role_unneeded", "node", h.nodeID, "role", string(role),
			"detail", "the company no longer needs this role")
	}
	for _, role := range edges.manned {
		log.Info("fleet_role_manned", "node", h.nodeID, "role", string(role))
	}
}

// roleEdges is what one sweep of the unmanned-role check has to say, each list
// sorted: the roles that became unmanned, the roles that were unmanned and now
// have a node, and the roles that were unmanned and are no longer needed.
type roleEdges struct {
	unmanned, manned, unneeded []placement.NodeRole
}

// fleetRoleEdges is [Host.checkFleetRoles]' decision, apart from its lines: the
// roles the company needs that no live node performs, compared with the last
// sweep's — the edge detector's state, which it moves.
func (h *Host) fleetRoleEdges(live []placement.NodeProfile) roleEdges {
	unmanned := map[placement.NodeRole]struct{}{}
	needed := map[placement.NodeRole]bool{}
	for _, role := range placement.Vocabulary() {
		needed[role] = h.NeedsRole(role)
		if !needed[role] {
			// NEITHER MANNED NOR UNMANNED: nothing is missing.
			continue
		}
		manned := false
		for _, node := range live {
			if node.Roles.Has(role) {
				manned = true
				break
			}
		}
		if !manned {
			unmanned[role] = struct{}{}
		}
	}

	h.mu.Lock()
	was := h.unmannedRoles
	h.unmannedRoles = unmanned
	h.mu.Unlock()

	var out roleEdges
	for _, role := range slices.Sorted(maps.Keys(unmanned)) {
		if _, known := was[role]; !known {
			out.unmanned = append(out.unmanned, role)
		}
	}
	for _, role := range slices.Sorted(maps.Keys(was)) {
		switch _, still := unmanned[role]; {
		case still:
		case !needed[role]:
			out.unneeded = append(out.unneeded, role)
		default:
			out.manned = append(out.manned, role)
		}
	}
	return out
}

// PresenceMeta is what a node's presence lease carries: the placement half —
// its profile's roles, labels and broker, how many seats it holds
// (placement.HeldKey) and, only while it is, its withdrawal
// (placement.WithdrawnKey) — then, on a Gitlet node, what its git cache gives
// its background work ([coord.GitBackgroundKey]; nil on a node without the
// gitlet role, which writes no key), and its status core under its own key
// ([coord.StatusKey]; nil where the node did not say, which writes none).
//
// PURE, AND THE ONE ASSEMBLER: the beat builds its presence through it, and so
// does internal/engine's TestThePresenceLeaseAtItsWorstFitsItsBound, which
// derives [coord.MaxLeaseMetaBytes] from every input at its limit — so the
// bound and the beat cannot assemble a presence two ways.
func PresenceMeta(profile placement.NodeProfile, held int, withdrawn bool, gitBackground *int64,
	status *coord.NodeStatus) map[string]any {
	// [placement.NodeProfile.Meta] returns a fresh map per call, so the
	// payload is built for this beat and belongs to no one else.
	meta := profile.Meta()
	meta[placement.HeldKey] = held
	if withdrawn {
		meta[placement.WithdrawnKey] = true
	}
	if gitBackground != nil {
		meta[coord.GitBackgroundKey] = *gitBackground
	}
	if status != nil {
		meta[coord.StatusKey] = status.Meta()
	}
	return meta
}

// presenceMeta is what this node advertises to its peers ([PresenceMeta]),
// both halves re-sent on every heartbeat. A node with no status hook publishes
// only the placement half, so readers tell "did not say" from "said zero".
// The seat count ([Host.fleetHoldsEverySeat]), the withdrawal and a Gitlet
// node's budget, zero included ([coord.GitBackgroundKey]), are written
// unconditionally: the host's own facts. The hook is BOUNDED, since a wedged
// store would hold this presence-renewing beat until the watchdog shot the
// process; one that does not answer ([Config].Status) publishes the placement
// half alone. AND THE WHOLE IS MEASURED: the store refuses a Meta past
// [coord.MaxLeaseMetaBytes] and a refused beat renews nothing, so such a status
// is dropped like an unanswered hook, said at ERROR; only the column is lost.
func (h *Host) presenceMeta(ctx context.Context, logger *slog.Logger) map[string]any {
	h.mu.Lock()
	withdrawn := h.withdrawn
	h.mu.Unlock()
	var git *int64
	if h.gitBackground != nil {
		budget := h.gitBackground()
		git = &budget
	}
	held := h.placedCount()
	placed := PresenceMeta(h.profile, held, withdrawn, git, nil)
	if h.status == nil {
		return placed
	}
	hookCtx, cancel := context.WithTimeout(ctx, h.statusBudget())
	defer cancel()
	// NOT ctx.Err() after the call. Whether the hook answered is the
	// hook's fact: the clock read here cannot tell a hook the deadline
	// cut short from one that had already returned its whole answer when
	// the goroutine was descheduled past the deadline.
	status, err := h.status(hookCtx)
	if err != nil {
		return placed
	}
	whole := PresenceMeta(h.profile, held, withdrawn, git, &status)
	size, err := coord.LeaseMetaBytes(whole)
	if err == nil && size <= coord.MaxLeaseMetaBytes {
		return whole
	}
	// Each attribute only where it says something: a size for a presence
	// that measured, an error for one that did not.
	why := slog.Int("bytes", size)
	if err != nil {
		why = slog.Any("error", err)
	}
	logger.ErrorContext(ctx, "node_presence_status_dropped", "node", h.nodeID,
		why, "bound", coord.MaxLeaseMetaBytes,
		"hint", "this node's presence was published without its status, which would carry it "+
			"past what a lease holds; shrinking node.labels makes room, and a status this "+
			"large beside labels Tier A allows is an engine defect worth reporting")
	return placed
}

// statusBudget is how long one beat lets the status hook run.
func (h *Host) statusBudget() time.Duration {
	d := h.heartbeat / StatusBudgetRatio
	if d <= 0 {
		// A host configured with a heartbeat under the divisor still has
		// to give the hook a turn: a zero budget expires before the call
		// starts, so the column would never be published at all.
		d = time.Millisecond
	}
	return d
}

// renewNodePresence keeps this node counted in the fleet size.
//
// NOT WHILE DRAINING: BeginDrain drops the presence lease so peers stop
// reserving capacity for a node that will never claim again, and the heartbeat
// runs on regardless, so a renew here would put the row back within a beat.
//
// It claims rather than renews because a claim is idempotent for a live
// self-held lease AND re-establishes the row after a lapse.
//
// Ungated: presence is membership, not work. Through the mixed-version gate a
// newer-protocol node would be invisible during the rolling upgrade the gate
// exists for, and its peers would divide the seats by a count excluding it.
func (h *Host) renewNodePresence(ctx context.Context) {
	if h.Draining() {
		return
	}
	// BEFORE THE CALL, as a seat's renew takes it: what a successful claim
	// proves is the row's TTL from the moment it was asked for, and a stamp
	// taken after a slow store answered would credit this node with time it
	// never held.
	asked := h.now()
	lease, _, err := h.backend.TryAcquire(ctx, coord.NodeResource(h.nodeID), coord.AcquireOptions{
		Owner:     h.owner,
		TTL:       h.ttl,
		Preferred: h.nodeID,
		Protocol:  h.protocol,
		Ungated:   true,
		// Re-sent on EVERY renew, not written once at claim. The profile
		// describes the LIVE process, so a node that restarts with
		// different roles or labels has to be able to correct what its
		// peers believe about it without waiting for its presence lease to
		// lapse first.
		Meta: h.presenceMeta(ctx, log),
	})
	if errors.Is(err, coord.ErrValueTooLarge) {
		// A DEFINITE REFUSAL, unlike the outage every other error is, which
		// the next beat retries: this one the next beat repeats, and this
		// node is gone from its peers' count until somebody reads why.
		// presenceMeta keeps the status from reaching it, so only a
		// placement half past what Tier A allows does.
		log.ErrorContext(ctx, "node_presence_refused_too_large", "node", h.nodeID, "error", err,
			"hint", "this node's presence is past what a lease holds and was not renewed; "+
				"shrink node.labels to what Tier A allows")
		return
	}
	if err != nil {
		return
	}
	if lease == nil {
		log.WarnContext(ctx, "node_presence_refused", "node", h.nodeID,
			"hint", "another process holds this node id's presence lease; two processes "+
				"sharing one node id will miscount the fleet and each compute too small a share")
		return
	}

	h.mu.Lock()
	draining := h.draining
	claimed := false
	if !draining {
		// A NEW EPOCH, not merely a lease in hand where none was: a
		// lapsed lease stays in hand until the claim that replaces it,
		// at the epoch the store mints for a lease that lapsed.
		claimed = h.nodeLease == nil || h.nodeLease.Epoch != lease.Epoch
		h.nodeLease = lease
		h.presenceRenewedAt = asked
	}
	h.mu.Unlock()
	if draining {
		// A drain started while this claim was in flight. A
		// single-threaded scheduler cannot reach this state, because
		// BeginDrain and the heartbeat cannot overlap; here they are
		// separate goroutines, and a row put back after the drain
		// dropped it leaves
		// peers reserving capacity for a node that will never claim again.
		h.giveUpLease(ctx, *lease)
		return
	}
	if claimed && h.presenceClaimed != nil {
		h.presenceClaimed(lease.Epoch)
	}
}

func (h *Host) releaseNodePresence(ctx context.Context) {
	h.mu.Lock()
	lease := h.nodeLease
	h.nodeLease = nil
	h.mu.Unlock()
	if lease != nil {
		h.giveUpLease(ctx, *lease)
	}
}

func (h *Host) giveUpLease(ctx context.Context, lease coord.Lease) {
	// ONE STEP OF A STOP, when a stop is what asks ([StopStep]).
	ctx, done := StopStep(ctx)
	defer done()
	if _, err := h.backend.Release(ctx, lease.Resource, h.owner, lease.Epoch); err != nil {
		log.WarnContext(ctx, "node_presence_release_unavailable", "node", h.nodeID, "error", err)
	}
}

// admits reports whether this node may claim new seats.
//
// A nil gate admits, and a PANICKING one admits too: the gate is a
// convenience for the caller, and a node that stopped claiming for ever
// because a status function paniced once would be a worse failure than the
// one the gate was added to prevent. The panic is logged where it can be
// found.
func (h *Host) admits(ctx context.Context) (ok bool) {
	if h.ready == nil {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeSeat, r)
			log.Error("seat_ready_check_failed", "node", h.nodeID, "panic", r,
				"stack", contained.Stack,
				"hint", "the readiness gate panicked, so this pass claimed as though "+
					"the node were ready; a gate that cannot answer must not stop a "+
					"fleet placing seats for ever")
			ok = true
		}
	}()
	return h.ready(ctx)
}

// unserviceable asks [Config.Serviceable] whether held seats may stay.
//
// A nil gate keeps them, and a PANICKING one keeps them too — the same rule
// [Host.admits] states for the opposite gate, and it matters more here: a
// status function that panicked once would otherwise tear down every seat on
// the node, which is a far worse failure than the stale rows the gate exists
// to stop serving.
func (h *Host) unserviceable(ctx context.Context) (unfit bool, reason string) {
	if h.serviceable == nil {
		return false, ""
	}
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeSeat, r)
			log.ErrorContext(ctx, "seat_serviceable_check_failed", "node", h.nodeID,
				"panic", r, "stack", contained.Stack,
				"hint", "the serviceability gate panicked, so this pass kept every "+
					"seat; a gate that cannot answer must not shed a fleet's work")
			unfit, reason = false, ""
		}
	}()
	ok, why := h.serviceable(ctx)
	return !ok, why
}
