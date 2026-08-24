package seat

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// sweepLoop re-evaluates placement on its own cadence, separate from the
// heartbeat because the two answer different questions: the heartbeat keeps
// what this node has, the sweep looks for what it should take.
func (h *SeatHost) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(h.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		h.safely("seat_sweep_tick_failed", func() { h.Sweep(ctx) })
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
func (h *SeatHost) Sweep(ctx context.Context) SweepResult {
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
	plan, liveNodes := h.plan(ctx, seats)

	byHandle := make(map[string]placement.Seat, len(seats))
	for _, s := range seats {
		byHandle[s.Handle] = s
	}
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
			log.Info("seat_released_role_gone", "seat", handle)
			// Only if the release was PROVEN. Release reports false when
			// teardown could not be confirmed and the seat went undead —
			// still leased, still renewed, and reported as released is
			// exactly backwards.
			if h.Release(ctx, handle, ReasonRoleGone) {
				released = append(released, handle)
			}
			continue
		}
		if _, may := eligible[handle]; !may {
			// Still a seat, no longer OURS to run: the placement selector
			// changed under a live apply, or this node's labels or roles
			// did. Released before the capacity shed, because an ineligible
			// seat is not a question of how many we hold — no amount of
			// spare capacity makes it ours again, and holding it means an
			// eligible peer cannot take it.
			log.Info("seat_released_not_placeable_here", "seat", handle, "node", h.nodeID,
				"placement", seat.Placement.String())
			if h.Release(ctx, handle, ReasonPlacement) {
				released = append(released, handle)
			}
		}
	}

	draining := h.Draining()
	if !draining {
		released = append(released, h.shedToCapacity(ctx, plan.Capacity)...)
	}

	var claimed []string
	var blocked int
	if !draining {
		h.mu.Lock()
		// Undead seats count against capacity: this process may still be
		// serving them, so taking on more work would over-subscribe a node
		// that is already in trouble.
		room := plan.Capacity - len(h.held) - len(h.undead)
		h.mu.Unlock()
		if room > h.claimLimit {
			room = h.claimLimit
		}
		if room > 0 {
			claimed, blocked = h.claimUpTo(ctx, plan.Eligible, room)
		}
	}

	if len(plan.Unplaceable) > 0 {
		log.Warn("seats_unplaceable", "node", h.nodeID, "seats", plan.Unplaceable,
			"hint", "no live node that runs seats matches these seats' placement, so nothing "+
				"is serving them. Start a node that matches, or widen the selector.")
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
	}
	// A copy, never &result: a heartbeat appends to the stored record, and
	// the value returned here would otherwise be the same object.
	stored := result.clone()
	h.last = &stored
	h.mu.Unlock()

	if len(claimed) > 0 || len(released) > 0 || blocked > 0 {
		log.Info("seat_sweep", "node", h.nodeID, "held", result.Held, "capacity", plan.Capacity,
			"live_nodes", liveNodes, "claimed", claimed, "released", released,
			"blocked_by_protocol", blocked)
	}
	return result
}

// currentSeats reads the org, converting a panicking provider into a pass
// that does nothing rather than one that reads "the company has no seats".
func (h *SeatHost) currentSeats() (seats []placement.Seat, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("seat_lookup_failed", "node", h.nodeID, "panic", r,
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
func (h *SeatHost) pruneSeatLocks(seats map[string]placement.Seat) {
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
// The give-back half of placement. It exists because the fair share is
// recomputed from a live node count: the moment a peer joins, every
// incumbent's share drops, and without this nothing ever acts on that — the
// new node computes a share it cannot reach, and the incumbent keeps serving
// seats it has already been told are not its own.
//
// VOLUNTARY, so the seat is quiesced and its in-flight turn finishes before
// the consumer detaches. A rebalance costs at most one turn boundary of
// latency on each seat that moves; nothing is abandoned and nothing goes
// dark, because the seat is served continuously — first here, then there.
//
// CONVERGENT, NOT OSCILLATING, and the ceiling is what guarantees it: shares
// are ceil(seats/nodes), so they sum to at least the seat count, and a node
// that has shed down to its share has no room to immediately re-claim what
// it just gave up. The excess moves once and stops.
func (h *SeatHost) shedToCapacity(ctx context.Context, capacity int) []string {
	h.mu.Lock()
	over := len(h.held) + len(h.undead) - capacity
	h.mu.Unlock()
	if over <= 0 {
		return nil
	}

	// Sorted, and NOT ordered by the preferred hint the way claiming is.
	// The hint records the last node to claim a seat, which for every seat
	// this node holds is this node — so it cannot discriminate among them,
	// and ordering by it would only look like it was doing something. A
	// stable order is what matters: it makes a shed reproducible, and two
	// nodes rebalancing at once hold disjoint sets, so they cannot collide.
	candidates := h.Held()
	limit := min(over, h.releaseLimit, len(candidates))

	var shed []string
	for _, handle := range candidates[:limit] {
		h.mu.Lock()
		held := len(h.held)
		h.mu.Unlock()
		log.Info("seat_released_over_capacity", "seat", handle, "held", held, "capacity", capacity)
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

// claimUpTo takes at most room seats, and reports the fleet's protocol floor
// when it took none because an older-protocol peer is live.
func (h *SeatHost) claimUpTo(ctx context.Context, eligible []string, room int) ([]string, int) {
	var claimed []string
	for _, handle := range h.claimOrder(ctx, eligible) {
		if len(claimed) >= room {
			break
		}
		took, stop := h.tryClaim(ctx, handle)
		if took {
			claimed = append(claimed, handle)
		}
		if stop {
			break
		}
	}
	if len(claimed) > 0 {
		return claimed, 0
	}
	// Nothing claimed. Distinguish "peers hold everything" (normal) from
	// "an older-protocol node is live and this build refuses to claim
	// beside it" (an upgrade that has stalled, and invisible without this).
	return claimed, h.protocolBlock(ctx)
}

// tryClaim takes one seat, reporting whether it was established and whether
// the pass must stop claiming altogether.
func (h *SeatHost) tryClaim(ctx context.Context, handle string) (took, stop bool) {
	unlock := h.lockSeat(handle)
	defer unlock()

	h.mu.Lock()
	_, alreadyHeld := h.held[handle]
	_, alreadyDead := h.undead[handle]
	h.mu.Unlock()
	if alreadyHeld || alreadyDead {
		return false, false // re-claimed under us while we waited
	}

	lease, err := h.backend.TryAcquire(ctx, coord.SeatResource(handle), coord.AcquireOptions{
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
		log.Warn("seat_claim_unavailable", "seat", handle, "error", err)
		return false, true
	}
	if lease == nil {
		return false, false
	}
	log.Info("seat_claimed", "seat", handle, "epoch", lease.Epoch)

	// Held from here so the heartbeat renews it while the hook runs, but
	// ESTABLISHING so nothing may start a turn on it yet. See heldSeat.
	h.mu.Lock()
	h.held[handle] = &heldSeat{lease: *lease, renewedAt: h.now(), establishing: true}
	h.mu.Unlock()

	if err := h.notifyAcquire(ctx, handle, *lease); err != nil {
		// A seat whose takeover pipeline failed must not stay claimed: it
		// would look owned to the fleet while nothing runs it. Give it
		// straight back so a peer can try — and back off here, because
		// retrying a config-shaped failure every 5 s spins at the cost of
		// an MCP fork each time.
		log.Error("seat_acquire_hook_failed", "seat", handle, "epoch", lease.Epoch, "error", err)
		h.mu.Lock()
		h.acquireBackoffs[handle] = h.now().Add(h.acquireBackoff)
		h.mu.Unlock()
		// Fenced, and already inside this seat's lock. The reason tells the
		// hook the seat was never fully established, so it must tolerate
		// half-spawned children and a consumer that was never attached.
		h.releaseLocked(ctx, handle, ReasonAcquireFailed)
		return false, false
	}

	h.mu.Lock()
	if entry := h.held[handle]; entry != nil {
		entry.establishing = false
	}
	h.mu.Unlock()

	// Counted as claimed only once the seat is ESTABLISHED. The hook gives
	// a failed takeover straight back — a bad MCP command, a credential
	// resolving to nothing — so counting it earlier meant a seat nothing
	// runs still burned a claim slot, still logged as claimed, and still
	// made the pass non-empty, which suppresses the protocol-block probe: a
	// stalled mixed-version upgrade then reported no block beside a claim
	// that never happened.
	return true, false
}

// claimOrder is the unheld seats this node may run, its preferred ones
// first.
//
// The caller passes the ELIGIBLE handles, already filtered by placement —
// eligibility is not a preference to be sorted, it is the difference between
// a seat this node may hold and one it may not.
//
// Stickiness, and only stickiness: a seat whose hint names this node is
// TRIED first so a restart or a rolling deploy tends to land it back where
// its MCP children were already spawned. A matching hint is never a claim
// precondition and a non-matching one is never a reason to skip — the hint
// outlives the node that set it, so gating on it would strand every seat a
// dead node used to hold.
//
// Sorted within each group so every node walks the list the same way. The
// lease decides races; a stable order stops two nodes colliding on the same
// seat over and over and making no progress.
//
// Seats this node recently failed to acquire are skipped until their backoff
// expires — negative stickiness, the mirror of the positive kind. Peers are
// unaffected.
func (h *SeatHost) claimOrder(ctx context.Context, seats []string) []string {
	now := h.now()
	h.mu.Lock()
	for handle, until := range h.acquireBackoffs {
		if !until.After(now) {
			delete(h.acquireBackoffs, handle)
		}
	}
	candidates := make([]string, 0, len(seats))
	for _, handle := range seats {
		if _, skip := h.held[handle]; skip {
			continue
		}
		if _, skip := h.undead[handle]; skip {
			continue
		}
		if _, skip := h.acquireBackoffs[handle]; skip {
			continue
		}
		candidates = append(candidates, handle)
	}
	h.mu.Unlock()
	slices.Sort(candidates)

	hinted, err := h.backend.PreferredResources(ctx, coord.SeatPrefix, h.nodeID)
	if err != nil || len(hinted) == 0 {
		return candidates
	}
	mine := make([]string, 0, len(candidates))
	rest := make([]string, 0, len(candidates))
	for _, handle := range candidates {
		if _, ok := hinted[coord.SeatResource(handle)]; ok {
			mine = append(mine, handle)
			continue
		}
		rest = append(rest, handle)
	}
	return append(mine, rest...)
}

// protocolBlock reports the fleet's protocol floor when it is what stopped
// this node claiming, and zero otherwise.
func (h *SeatHost) protocolBlock(ctx context.Context) int {
	floor, found, err := h.backend.FleetProtocolFloor(ctx)
	if err != nil || !found || floor >= h.protocol {
		return 0
	}
	log.Warn("seat_claims_blocked_by_older_protocol", "node", h.nodeID,
		"fleet_floor", floor, "this_node", h.protocol,
		"hint", "an older-protocol node still holds leases; this node will claim nothing until "+
			"it drains. Finish the rolling upgrade — do NOT roll back across a protocol bump "+
			"without stopping the fleet first.")
	return floor
}

// --- membership -----------------------------------------------------------

// plan works out what this node may claim and how much, plus the live
// seat-running node count.
//
// When the fleet cannot be read, the LAST KNOWN membership is reused rather
// than assuming a fleet of one. A partial store failure — a timeout on the
// scan while point writes still succeed — would otherwise turn every node in
// the fleet into "I should own everything" simultaneously: mutual exclusion
// still prevents double ownership, but the fleet degenerates to whoever
// sweeps first taking the claim limit every 5 s until it holds the lot,
// undoing the balance for no reason. Before the first successful read there
// is nothing to reuse, and a fleet of one — this node — is the honest
// assumption.
func (h *SeatHost) plan(ctx context.Context, seats []placement.Seat) (placement.Plan, int) {
	var live []placement.NodeProfile

	leases, err := h.backend.ListLive(ctx, coord.NodePrefix)
	if err != nil {
		h.mu.Lock()
		live = slices.Clone(h.liveProfiles)
		h.mu.Unlock()
		log.Warn("seat_capacity_unavailable", "node", h.nodeID,
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
	}

	plan := placement.Compute(seats, h.profile, live)
	h.checkFleetRoles(append(slices.Clone(live), h.profile))
	return plan, plan.SeatNodes
}

// checkFleetRoles says something when the fleet has nobody doing one of the
// jobs.
//
// A node's roles subtract a duty from THAT NODE, never from the company — so
// a fleet can be configured, node by node, into a shape where a whole job is
// done by nobody, and no single node's config is wrong. The symptoms are all
// absences: no scheduled task ever fires, no retention sweep ever runs, no
// webhook is ever received. Nothing errors, so nothing surfaces.
//
// Edge-triggered. A fleet missing workers for an hour should say so once,
// and again when it comes back — not 720 times.
func (h *SeatHost) checkFleetRoles(live []placement.NodeProfile) {
	unmanned := map[placement.NodeRole]struct{}{}
	for _, role := range []placement.NodeRole{placement.RoleIngress, placement.RoleSeats, placement.RoleWorkers} {
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

	ids := map[string]struct{}{}
	for _, node := range live {
		ids[node.ID] = struct{}{}
	}

	h.mu.Lock()
	was := h.unmannedRoles
	h.unmannedRoles = unmanned
	h.mu.Unlock()

	for _, role := range slices.Sorted(maps.Keys(unmanned)) {
		if _, known := was[role]; known {
			continue
		}
		log.Warn("fleet_role_unmanned", "node", h.nodeID, "role", string(role),
			"live_nodes", len(ids), "hint", unmannedHints[role])
	}
	for _, role := range slices.Sorted(maps.Keys(was)) {
		if _, still := unmanned[role]; !still {
			log.Info("fleet_role_manned", "node", h.nodeID, "role", string(role))
		}
	}
}

// presenceMeta is what this node advertises to its peers.
//
// The placement half — roles and labels — and the live half beside it under
// its own key, both re-sent on every heartbeat. A node with no status hook
// publishes only the placement half, which is what makes "did not say"
// distinguishable from "said zero" on the reading side.
//
// It writes into the profile's own map because [placement.NodeProfile.Meta]
// returns a fresh literal per call — the lease payload is built for this
// beat and belongs to no one else.
//
// The hook is BOUNDED. Answering may mean reading the control plane, and
// this is the path that renews presence: an unbounded read on a wedged store
// would hold the beat until the watchdog shot the process, to publish a
// display column. Overrunning it publishes the placement half alone, which
// the reading side already renders as "did not say".
func (h *SeatHost) presenceMeta(ctx context.Context) map[string]any {
	meta := h.profile.Meta()
	if h.status == nil {
		return meta
	}
	ctx, cancel := context.WithTimeout(ctx, h.statusBudget())
	defer cancel()
	status := h.status(ctx)
	if ctx.Err() != nil {
		return meta
	}
	meta[coord.StatusKey] = status.Meta()
	return meta
}

// statusBudget is how long one beat lets the status hook run.
func (h *SeatHost) statusBudget() time.Duration {
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
// NOT WHILE DRAINING. BeginDrain drops the presence lease precisely so peers
// stop reserving capacity for a node that will never claim again — and the
// heartbeat runs on regardless, so an ungated renew here would put the row
// straight back within one interval.
//
// It claims rather than renews because a claim is idempotent for a live
// self-held lease AND re-establishes the row after a lapse, which is exactly
// the behaviour presence wants.
//
// Ungated: presence is membership, not work. Running it through the
// mixed-version gate makes a newer-protocol node invisible during the exact
// rolling upgrade the gate exists for — its peers then divide the seats by a
// count that excludes it, and its own capacity excludes it too.
func (h *SeatHost) renewNodePresence(ctx context.Context) {
	if h.Draining() {
		return
	}
	lease, err := h.backend.TryAcquire(ctx, coord.NodeResource(h.nodeID), coord.AcquireOptions{
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
		Meta: h.presenceMeta(ctx),
	})
	if err != nil {
		return
	}
	if lease == nil {
		log.Warn("node_presence_refused", "node", h.nodeID,
			"hint", "another process holds this node id's presence lease; two processes "+
				"sharing one node id will miscount the fleet and each compute too small a share")
		return
	}

	h.mu.Lock()
	draining := h.draining
	if !draining {
		h.nodeLease = lease
	}
	h.mu.Unlock()
	if draining {
		// A drain started while this claim was in flight. The Python engine
		// could not reach this state — one event loop meant BeginDrain and
		// the heartbeat could not overlap — but here they are separate
		// goroutines, and a row put back after the drain dropped it leaves
		// peers reserving capacity for a node that will never claim again.
		h.giveUpLease(ctx, *lease)
	}
}

func (h *SeatHost) releaseNodePresence(ctx context.Context) {
	h.mu.Lock()
	lease := h.nodeLease
	h.nodeLease = nil
	h.mu.Unlock()
	if lease != nil {
		h.giveUpLease(ctx, *lease)
	}
}

func (h *SeatHost) giveUpLease(ctx context.Context, lease coord.Lease) {
	if _, err := h.backend.Release(ctx, lease.Resource, h.owner, lease.Epoch); err != nil {
		log.Warn("node_presence_release_unavailable", "node", h.nodeID, "error", err)
	}
}
