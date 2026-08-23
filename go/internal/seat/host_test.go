package seat

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// --- construction ----------------------------------------------------------

func TestNewRefusesAHostThatCannotIdentifyItself(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	base := Config{Backend: f.store, Owner: "node-a:1", NodeID: "node-a", Seats: seatsNamed("ceo")}

	cases := map[string]func(*Config){
		"no backend": func(c *Config) { c.Backend = nil },
		"no owner":   func(c *Config) { c.Owner = "" },
		"no node id": func(c *Config) { c.NodeID = "" },
		"no seats":   func(c *Config) { c.Seats = nil },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			mangle(&cfg)
			if _, err := New(cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestTheProfileAlwaysNamesThisNode(t *testing.T) {
	t.Parallel()
	// The presence row is keyed by the node id, so a profile naming a
	// different node would make this node invisible to itself: it would
	// find zero eligible nodes for every group and report every seat
	// unplaceable.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Profile: placement.NodeProfile{ID: "somebody-else"}})
	if h.profile.ID != "node-a" {
		t.Fatalf("profile id = %q, want node-a", h.profile.ID)
	}
}

// --- the fair share --------------------------------------------------------

func TestASingleNodeTakesEverySeat(t *testing.T) {
	t.Parallel()
	// The degenerate case is the common one and must stay boring.
	f := newFleet(t)
	h := f.newHost("node-a", Config{})
	h.renewNodePresence(f.ctx)

	result := h.Sweep(f.ctx)
	wantInt(t, result.LiveNodes, 1, "live nodes")
	wantInt(t, result.Capacity, 3, "capacity")
	wantHeld(t, h, "ceo", "eng", "ops")
}

func TestTwoNodesSplitTheSeats(t *testing.T) {
	t.Parallel()
	// ceil(seats / live nodes), computed identically by both, from the same
	// table, with no coordinator and no gossip.
	f := newFleet(t)
	a := f.newHost("node-a", Config{})
	b := f.newHost("node-b", Config{})
	a.renewNodePresence(f.ctx)
	b.renewNodePresence(f.ctx)

	ra, rb := a.Sweep(f.ctx), b.Sweep(f.ctx)
	wantInt(t, ra.LiveNodes, 2, "node-a live nodes")
	wantInt(t, rb.LiveNodes, 2, "node-b live nodes")
	wantInt(t, ra.Capacity, 2, "node-a capacity")
	wantInt(t, rb.Capacity, 2, "node-b capacity")

	held := append(a.Held(), b.Held()...)
	wantInt(t, len(held), 3, "seats held across the fleet")
	seen := map[string]bool{}
	for _, handle := range held {
		if seen[handle] {
			t.Fatalf("seat %q is held twice", handle)
		}
		seen[handle] = true
	}
}

// Invariant 2: presence leases ARE membership.
func TestAnUnclaimedFleetStillCountsItsNodes(t *testing.T) {
	t.Parallel()
	// Inferring the fleet size from SEAT ownership cannot work: a fleet
	// where nobody has claimed anything yet reads as zero nodes, and every
	// node then believes it should take every seat. Nothing here holds a
	// seat, and the answer must still be two.
	f := newFleet(t)
	a := f.newHost("node-a", Config{})
	a.renewNodePresence(f.ctx)
	f.present("node-b", time.Minute, placement.NodeProfile{})

	plan, live := a.plan(f.ctx, a.seats())
	wantInt(t, live, 2, "live seat-running nodes")
	wantInt(t, plan.Capacity, 2, "capacity with nothing claimed anywhere")
}

func TestCapacityWidensWhenAPeerDies(t *testing.T) {
	t.Parallel()
	// A dead node stops counting toward the divisor as soon as its presence
	// lease lapses — which is what lets survivors widen instead of leaving
	// the seats dark.
	f := newFleet(t)
	a := f.newHost("node-a", Config{})
	a.renewNodePresence(f.ctx)
	f.present("node-b", 5*time.Second, placement.NodeProfile{})

	wantInt(t, a.Sweep(f.ctx).Capacity, 2, "capacity with a peer")
	f.clock.Advance(6 * time.Second)

	result := a.Sweep(f.ctx)
	wantInt(t, result.LiveNodes, 1, "live nodes after the peer died")
	wantInt(t, result.Capacity, 3, "capacity after the peer died")
	wantHeld(t, a, "ceo", "eng", "ops")
}

func TestANodeNeverExceedsItsShare(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	a := f.newHost("node-a", Config{Seats: numberedSeats(10)})
	a.renewNodePresence(f.ctx)
	f.present("node-b", time.Minute, placement.NodeProfile{})
	f.present("node-c", time.Minute, placement.NodeProfile{})

	// Sweep repeatedly; the cap must hold regardless of how many passes.
	for range 6 {
		a.Sweep(f.ctx)
	}
	wantInt(t, len(a.Held()), 4, "seats held") // ceil(10 / 3)
}

// Invariant 3: a membership read that fails reuses the last known fleet.
func TestAMembershipReadFailureReusesTheLastKnownFleet(t *testing.T) {
	t.Parallel()
	// Assuming a fleet of one on an unreadable node count turns every node
	// into "I should own everything" simultaneously. Mutual exclusion still
	// prevents double ownership, but the fleet degenerates to whoever
	// sweeps first taking the claim limit every 5 s until it holds the lot,
	// undoing the balance for no reason.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	a := f.newHost("node-a", Config{Backend: faulty, Seats: numberedSeats(4)})
	a.renewNodePresence(f.ctx)
	f.present("node-b", time.Minute, placement.NodeProfile{})

	plan, live := a.plan(f.ctx, a.seats())
	wantInt(t, plan.Capacity, 2, "capacity while the store answers")
	wantInt(t, live, 2, "live nodes while the store answers")

	faulty.Break(nil)
	plan, live = a.plan(f.ctx, a.seats())
	wantInt(t, plan.Capacity, 2, "capacity during a blip")
	wantInt(t, live, 2, "live nodes during a blip")
}

func TestBeforeAnyReadTheHonestAssumptionIsAFleetOfOne(t *testing.T) {
	t.Parallel()
	// There is nothing to reuse yet, and a node that cannot see itself
	// finds zero eligible nodes for every group, claims nothing, and
	// reports every seat unplaceable.
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	faulty.Break(nil)
	a := f.newHost("node-a", Config{Backend: faulty})

	plan, live := a.plan(f.ctx, a.seats())
	wantInt(t, plan.Capacity, 3, "capacity with no membership at all")
	wantInt(t, live, 1, "live nodes with no membership at all")
	wantInt(t, len(plan.Unplaceable), 0, "unplaceable seats")
}

// Invariant 2: the profile rides on presence, and a seat claim must not
// blank it.
func TestASeatClaimDoesNotBlankThePresenceProfile(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats:   seatsNamed("ceo"),
		Profile: placement.NodeProfile{Labels: map[string]string{"zone": "eu"}},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo")

	presence := f.leaseOf(coord.NodeResource("node-a"))
	if presence == nil {
		t.Fatal("presence lease is gone")
	}
	profile, ok := placement.FromLease(*presence)
	if !ok || profile.Labels["zone"] != "eu" {
		t.Fatalf("presence profile = %+v, want zone=eu; a seat claim carrying an empty "+
			"payload must not un-label the node", profile)
	}
	// And the seat row says nothing about what this node IS: a second,
	// staler answer to the question presence already answers.
	seat := f.leaseOf(coord.SeatResource("ceo"))
	if seat == nil || len(seat.Meta) != 0 {
		t.Fatalf("seat lease meta = %v, want none", seat.Meta)
	}
}

func TestTheProfileIsResentOnEveryPresenceRenew(t *testing.T) {
	t.Parallel()
	// Written once at claim, a label or role change would not reach peers
	// until the presence lease lapsed. The process describes itself afresh
	// every heartbeat instead.
	f := newFleet(t)
	first := f.newHost("node-a", Config{
		Owner:   "node-a:incarnation",
		Profile: placement.NodeProfile{Labels: map[string]string{"zone": "eu"}},
	})
	first.renewNodePresence(f.ctx)

	relabelled := f.newHost("node-a", Config{
		Owner:   "node-a:incarnation", // the same process, reconfigured
		Profile: placement.NodeProfile{Labels: map[string]string{"zone": "us"}},
	})
	relabelled.renewNodePresence(f.ctx)

	profile, _ := placement.FromLease(*f.leaseOf(coord.NodeResource("node-a")))
	if profile.Labels["zone"] != "us" {
		t.Fatalf("presence labels = %v, want zone=us", profile.Labels)
	}
}

// --- draining --------------------------------------------------------------

// Invariant 2: presence is released on drain and NOT renewed while draining.
func TestADrainingNodeStaysOutOfTheFleetCount(t *testing.T) {
	t.Parallel()
	// A draining node in the divisor makes peers reserve capacity for a
	// node that will never claim again. On the posture-shed path — which
	// can last minutes, not seconds — that is real stranded work: with 3
	// nodes and 10 seats the healthy peers each compute ceil(10/3) = 4, and
	// 2 of the drained node's seats are claimable by nobody until it comes
	// back.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	if f.leaseOf(coord.NodeResource("node-a")) == nil {
		t.Fatal("the node never registered presence")
	}

	h.BeginDrain(f.ctx)
	if f.leaseOf(coord.NodeResource("node-a")) != nil {
		t.Fatal("BeginDrain kept presence")
	}
	// The heartbeat loop keeps ticking through a drain — that is what keeps
	// held seats alive while their turns finish.
	h.Heartbeat(f.ctx)
	if f.leaseOf(coord.NodeResource("node-a")) != nil {
		t.Fatal("the heartbeat re-announced a draining node, putting it back in every peer's " +
			"capacity divisor")
	}
	wantHeld(t, h, "ceo")
}

func TestDrainingStopsClaimingButKeepsHolding(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{ClaimLimit: 1})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	held := h.Held()
	wantInt(t, len(held), 1, "seats held before the drain")

	h.BeginDrain(f.ctx)
	result := h.Sweep(f.ctx)
	wantInt(t, len(result.Claimed), 0, "seats claimed while draining")
	wantStrings(t, h.Held(), held, "held after a draining sweep")
	wantInt(t, len(h.Heartbeat(f.ctx)), 0, "seats lost while draining")
}

func TestResumingFromAShedAnnouncesTheNodeAgain(t *testing.T) {
	t.Parallel()
	// A node that shed its seats on config divergence and then converged
	// has to rejoin, or it sits out until it restarts.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.BeginDrain(f.ctx)
	if f.leaseOf(coord.NodeResource("node-a")) != nil {
		t.Fatal("BeginDrain kept presence")
	}

	h.ResumeClaiming(f.ctx)
	if f.leaseOf(coord.NodeResource("node-a")) == nil {
		t.Fatal("a resumed node never re-announced itself")
	}
}

func TestADrainingNodeDoesNotShedTwice(t *testing.T) {
	t.Parallel()
	// ReleaseAll is already giving every seat back; a capacity shed on top
	// of it only slows the drain down and logs a rebalance that is not
	// happening.
	f := newFleet(t)
	a := f.newHost("node-a", Config{})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx)
	f.present("node-b", time.Minute, placement.NodeProfile{})

	a.BeginDrain(f.ctx)
	result := a.Sweep(f.ctx)
	wantInt(t, len(result.Lost), 0, "seats shed while draining")
	wantInt(t, len(a.Held()), 3, "seats still held while draining")
}

// --- claiming: rate, order, backoff ----------------------------------------

// Invariant 8: claims are rate-limited per sweep.
func TestClaimsAreRateLimitedPerSweep(t *testing.T) {
	t.Parallel()
	// The limiter is MCP spawn cost, not the lease. A node absorbing a dead
	// peer's seats all at once would fork that many subprocess trees in a
	// single tick.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: numberedSeats(10), ClaimLimit: 3})
	h.renewNodePresence(f.ctx)

	for i, want := range []int{3, 3, 3, 1} {
		got := len(h.Sweep(f.ctx).Claimed)
		if got != want {
			t.Fatalf("sweep %d claimed %d seats, want %d", i, got, want)
		}
	}
	wantInt(t, len(h.Held()), 10, "seats held after four sweeps")
}

// Invariant 7: preferred ORDERS the claim.
func TestPreferredSeatsAreTriedFirst(t *testing.T) {
	t.Parallel()
	// A restart should land seats back where their MCP children were
	// already spawned. The hinted seats here sort LAST, so a host that
	// ignored the hint would visibly take the other two.
	f := newFleet(t)
	f.seedHint("gamma", "node-a")
	f.seedHint("delta", "node-a")

	h := f.newHost("node-a", Config{
		Seats:      seatsNamed("alpha", "beta", "gamma", "delta"),
		ClaimLimit: 2,
	})
	h.renewNodePresence(f.ctx)

	wantStrings(t, h.Sweep(f.ctx).Claimed, []string{"delta", "gamma"}, "claimed")
}

// Invariant 7: preferred NEVER gates the claim.
func TestAStaleHintNeverBlocksAClaim(t *testing.T) {
	t.Parallel()
	// The hint outlives the node that set it. Treating a foreign preferred
	// as a reason to wait would strand every seat a dead node used to hold
	// — permanently.
	f := newFleet(t)
	f.seedHint("alpha", "dead-node")
	f.seedHint("beta", "dead-node")

	survivor := f.newHost("node-b", Config{Seats: seatsNamed("alpha", "beta")})
	survivor.renewNodePresence(f.ctx)
	survivor.Sweep(f.ctx)
	wantHeld(t, survivor, "alpha", "beta")
}

// Invariant 8: a failed acquire backs this node off the seat for one TTL.
func TestAFailedAcquireIsNotRetriedEverySweep(t *testing.T) {
	t.Parallel()
	// Retrying a config-shaped failure every 5 s spins at the cost of an
	// MCP fork each time, and the seat is dark throughout either way.
	f := newFleet(t)
	hooks := &hookLog{acquireErr: func(string, int) error { return errors.New("bad mcp command") }}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)

	h.Sweep(f.ctx)
	wantStrings(t, hooks.acquired(), []string{"ceo"}, "acquire attempts")
	wantHeld(t, h)

	h.Sweep(f.ctx)
	h.Sweep(f.ctx)
	wantStrings(t, hooks.acquired(), []string{"ceo"}, "acquire attempts while backed off")
}

func TestTheAcquireBackoffIsOneTTLAndExpires(t *testing.T) {
	t.Parallel()
	// Deliberately not permanent: the cause is often config, and config
	// changes.
	f := newFleet(t)
	hooks := &hookLog{acquireErr: func(string, int) error { return errors.New("bad mcp command") }}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks, TTL: 30 * time.Second,
	})
	if h.acquireBackoff != 30*time.Second {
		t.Fatalf("acquire backoff = %s, want one TTL", h.acquireBackoff)
	}
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	f.clock.Advance(29 * time.Second)
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantStrings(t, hooks.acquired(), []string{"ceo"}, "attempts inside the backoff")

	f.clock.Advance(2 * time.Second)
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantStrings(t, hooks.acquired(), []string{"ceo", "ceo"}, "attempts after the backoff")
}

// Invariant 8: a claim counts only once the acquire hook succeeded.
func TestASeatWhoseAcquireFailedIsNotReportedAsClaimed(t *testing.T) {
	t.Parallel()
	// Claimed is the pass's ledger, and it counted seats nothing runs.
	// Appending before the hook meant a failed takeover still burned a
	// claim slot, still logged as claimed, and still made the pass
	// non-empty — which suppresses the protocol-block probe, so a stalled
	// mixed-version upgrade reported no block beside a claim that never
	// happened.
	f := newFleet(t)
	hooks := &hookLog{acquireErr: func(string, int) error { return errors.New("no such MCP command") }}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks})
	h.renewNodePresence(f.ctx)

	result := h.Sweep(f.ctx)
	wantHeld(t, h)
	wantInt(t, len(result.Claimed), 0, "seats reported as claimed")
	wantStrings(t, hooks.released(), []string{"ceo:acquire_failed"}, "releases")
}

// --- giving seats back: the other half of placement ------------------------

func TestANodeThatBootedAloneMakesRoomWhenAPeerJoins(t *testing.T) {
	t.Parallel()
	// Claiming alone converges only for a fleet that SHRINKS. A node that
	// booted first holds every seat, and a peer joining later computes a
	// share it can never reach — every seat it wants is held by a node with
	// no reason to let go. Without the give-back, adding a node to a
	// running company does nothing at all until something dies.
	f := newFleet(t)
	a := f.newHost("node-a", Config{})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx)
	wantHeld(t, a, "ceo", "eng", "ops")

	b := f.newHost("node-b", Config{})
	b.renewNodePresence(f.ctx)

	result := a.Sweep(f.ctx)
	wantInt(t, result.Capacity, 2, "capacity once a peer joined")
	wantInt(t, len(a.Held()), 2, "seats held after the give-back")
	wantInt(t, len(result.Lost), 1, "seats handed back")

	b.Sweep(f.ctx)
	wantInt(t, len(b.Held()), 1, "seats the peer could take")
	all := append(a.Held(), b.Held()...)
	wantInt(t, len(all), 3, "seats served across the fleet")
}

func TestTheGiveBackSettlesInsteadOfPingPonging(t *testing.T) {
	t.Parallel()
	// Shares are ceil(seats / nodes), so they sum to at least the seat
	// count and a node at its share has no room to re-claim what it just
	// shed. A rebalance that oscillated would respawn MCP children on every
	// tick forever.
	f := newFleet(t)
	a := f.newHost("node-a", Config{Seats: numberedSeats(5)})
	b := f.newHost("node-b", Config{Seats: numberedSeats(5)})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx) // alone: takes all five
	b.renewNodePresence(f.ctx)

	for range 8 {
		a.Sweep(f.ctx)
		b.Sweep(f.ctx)
	}
	held := append(a.Held(), b.Held()...)
	wantInt(t, len(held), 5, "seats served across the fleet")
	if len(a.Held()) > 3 || len(b.Held()) > 3 {
		t.Fatalf("a node exceeded its share: a=%v b=%v", a.Held(), b.Held())
	}

	beforeA, beforeB := a.Held(), b.Held()
	a.Sweep(f.ctx)
	b.Sweep(f.ctx)
	wantStrings(t, a.Held(), beforeA, "node-a after a settled round")
	wantStrings(t, b.Held(), beforeB, "node-b after a settled round")
}

// Invariant 8: give-backs are rate-limited too.
func TestTheGiveBackIsRateLimitedLikeClaiming(t *testing.T) {
	t.Parallel()
	// A release is an MCP teardown here and a spawn on whoever takes it.
	// Shedding eight seats in one tick would fork the whole company's
	// subprocess trees twice inside a second, for a rebalance nothing is
	// waiting on — no seat is dark while it is held by the wrong node.
	f := newFleet(t)
	a := f.newHost("node-a", Config{Seats: numberedSeats(10)})
	a.renewNodePresence(f.ctx)
	for range 4 {
		a.Sweep(f.ctx) // alone, claim-limited: ends up holding all ten
	}
	wantInt(t, len(a.Held()), 10, "seats held alone")

	for _, node := range []string{"node-b", "node-c", "node-d"} {
		f.present(node, time.Minute, placement.NodeProfile{})
	}

	result := a.Sweep(f.ctx) // capacity is now ceil(10/4) = 3
	wantInt(t, result.Capacity, 3, "capacity")
	wantInt(t, len(result.Lost), ReleaseLimitPerSweep, "seats shed in one pass")
	wantInt(t, len(a.Held()), 10-ReleaseLimitPerSweep, "seats still held")
}

// Invariant 7: the shed order is plain sorted.
func TestTheShedOrderIsPlainSorted(t *testing.T) {
	t.Parallel()
	// Deliberately NOT ordered by the preferred hint, the way claiming is:
	// the hint records the last node to claim a seat, so for every seat
	// this node holds it names this node. Ordering by it would look like
	// stickiness and do nothing.
	f := newFleet(t)
	a := f.newHost("node-a", Config{})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx)
	wantHeld(t, a, "ceo", "eng", "ops")
	for _, handle := range a.Held() {
		if hint := f.leaseOf(coord.SeatResource(handle)).Preferred; hint != "node-a" {
			t.Fatalf("hint for %q is %q; the hint names the last claimer, so it cannot rank "+
				"a node's own seats", handle, hint)
		}
	}

	f.present("node-b", time.Minute, placement.NodeProfile{})
	wantStrings(t, a.Sweep(f.ctx).Lost, []string{"ceo"}, "shed")
}

// --- the org changing underneath -------------------------------------------

func TestADecommissionedRoleIsReleased(t *testing.T) {
	t.Parallel()
	// Seats are read fresh each sweep: a live config apply can delete a
	// role, and holding its lease afterwards would look like ownership of
	// something that no longer exists.
	f := newFleet(t)
	var live atomic.Value
	live.Store([]string{"ceo", "eng"})
	h := f.newHost("node-a", Config{Seats: func() []placement.Seat {
		return seatsNamed(live.Load().([]string)...)()
	}})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantAdmits(t, h, "eng", true)

	live.Store([]string{"ceo"})
	result := h.Sweep(f.ctx)
	wantAdmits(t, h, "eng", false)
	wantAdmits(t, h, "ceo", true)
	wantStrings(t, result.Lost, []string{"eng"}, "released")
}

func TestAnUnreadableOrgChangesNothing(t *testing.T) {
	t.Parallel()
	// Reading the org as "no seats" is the one misreading with a
	// catastrophic direction: every role on this node would be
	// decommissioned at once.
	f := newFleet(t)
	var broken atomic.Bool
	h := f.newHost("node-a", Config{Seats: func() []placement.Seat {
		if broken.Load() {
			panic("the org is mid-apply")
		}
		return seatsNamed("ceo", "eng", "ops")()
	}})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo", "eng", "ops")

	broken.Store(true)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo", "eng", "ops")
}

func TestSeatLocksDoNotAccumulateForever(t *testing.T) {
	t.Parallel()
	// One mutex per handle EVER seen, including every seat a live config
	// apply removed — unbounded growth in a long-lived process.
	f := newFleet(t)
	var live atomic.Value
	live.Store([]string{"ceo", "eng", "ops"})
	h := f.newHost("node-a", Config{Seats: func() []placement.Seat {
		return seatsNamed(live.Load().([]string)...)()
	}})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantInt(t, len(h.seatLocks), 3, "seat locks")

	live.Store([]string{"ceo"})
	h.Sweep(f.ctx)
	wantInt(t, len(h.seatLocks), 1, "seat locks after the org shrank")
}

// --- placement -------------------------------------------------------------

func placed(pinned map[string]placement.SeatPlacement, handles ...string) func() []placement.Seat {
	return func() []placement.Seat {
		out := make([]placement.Seat, 0, len(handles))
		for _, h := range handles {
			out = append(out, placement.Seat{Handle: h, Placement: pinned[h]})
		}
		return out
	}
}

func TestANodeNeverClaimsASeatPinnedElsewhere(t *testing.T) {
	t.Parallel()
	// The whole point of a pin, and the reason the protocol version moved:
	// a build that ignores placement claims it and SUCCEEDS, because the
	// lease is only a mutex.
	f := newFleet(t)
	seats := placed(map[string]placement.SeatPlacement{"eng": {Node: "node-b"}}, "ceo", "eng")
	a := f.newHost("node-a", Config{Seats: seats})
	b := f.newHost("node-b", Config{Seats: seats})
	a.renewNodePresence(f.ctx)
	b.renewNodePresence(f.ctx)

	a.Sweep(f.ctx)
	if slicesContains(a.Held(), "eng") {
		t.Fatal("node-a claimed a seat pinned to node-b")
	}
	b.Sweep(f.ctx)
	if !slicesContains(b.Held(), "eng") {
		t.Fatal("node-b never claimed the seat pinned to it")
	}
}

func TestALabelSelectorMatchesTheNodesOwnLabels(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	seats := placed(map[string]placement.SeatPlacement{
		"gpu": {Labels: map[string]string{"gpu": "true"}},
	}, "gpu")
	big := f.newHost("big", Config{
		Seats: seats, Profile: placement.NodeProfile{Labels: map[string]string{"gpu": "true"}},
	})
	small := f.newHost("small", Config{
		Seats: seats, Profile: placement.NodeProfile{Labels: map[string]string{"gpu": "false"}},
	})
	big.renewNodePresence(f.ctx)
	small.renewNodePresence(f.ctx)

	small.Sweep(f.ctx)
	wantHeld(t, small)
	big.Sweep(f.ctx)
	wantHeld(t, big, "gpu")
}

func TestASeatNobodyMatchesIsReportedAndLeftAlone(t *testing.T) {
	t.Parallel()
	// Not widened. Widening the selector is precisely what the operator
	// asked the engine not to do — so the seat goes unserved and the sweep
	// says so, which is the only way anyone finds out.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: placed(map[string]placement.SeatPlacement{
		"gpu": {Labels: map[string]string{"gpu": "true"}},
	}, "ceo", "gpu")})
	h.renewNodePresence(f.ctx)

	result := h.Sweep(f.ctx)
	wantStrings(t, result.Unplaceable, []string{"gpu"}, "unplaceable")
	wantHeld(t, h, "ceo")
}

func TestASeatThatStopsMatchingIsHandedBack(t *testing.T) {
	t.Parallel()
	// A live apply narrows the selector under a node that is holding the
	// seat. Voluntary, so the in-flight turn finishes — but it must happen,
	// or an eligible peer can never take it.
	f := newFleet(t)
	var pin atomic.Value
	pin.Store(placement.SeatPlacement{})
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{
		Hooks: hooks,
		Seats: func() []placement.Seat {
			return []placement.Seat{{Handle: "ceo", Placement: pin.Load().(placement.SeatPlacement)}}
		},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo")

	pin.Store(placement.SeatPlacement{Node: "node-b"})
	result := h.Sweep(f.ctx)
	wantHeld(t, h)
	wantStrings(t, hooks.released(), []string{"ceo:placement"}, "releases")
	wantStrings(t, result.Lost, []string{"ceo"}, "lost")
}

func TestAnIngressOnlyNodeClaimsNothingButIsStillPresent(t *testing.T) {
	t.Parallel()
	// It must be visible to the fleet (the dashboard lists it, and it holds
	// worker duties if it has that role) while never appearing in the seat
	// denominator.
	f := newFleet(t)
	api := f.newHost("api", Config{
		Seats:   seatsNamed("ceo", "eng"),
		Profile: placement.NodeProfile{Roles: placement.Roles(placement.RoleIngress)},
	})
	api.renewNodePresence(f.ctx)
	result := api.Sweep(f.ctx)
	wantHeld(t, api)
	wantInt(t, result.Capacity, 0, "capacity of a node that runs no seats")

	worker := f.newHost("node-b", Config{Seats: seatsNamed("ceo", "eng")})
	worker.renewNodePresence(f.ctx)
	result = worker.Sweep(f.ctx)
	// Two live nodes, but only one runs seats — so the share is both seats,
	// not one. Counting the API node would strand the other.
	wantInt(t, result.Capacity, 2, "capacity")
	wantHeld(t, worker, "ceo", "eng")
}

// --- the mixed-version gate ------------------------------------------------

func TestAnOlderProtocolPeerIsReportedNotJustSilent(t *testing.T) {
	t.Parallel()
	// A node stalled by the upgrade gate must be distinguishable from one
	// whose peers simply hold every seat — otherwise a rolling upgrade that
	// has wedged looks exactly like a healthy full fleet.
	f := newFleet(t)
	old := f.newHost("old-node", Config{Seats: seatsNamed("ceo"), Protocol: 1})
	old.renewNodePresence(f.ctx)
	old.Sweep(f.ctx)

	fresh := f.newHost("new-node", Config{Seats: seatsNamed("ceo", "eng"), Protocol: 2})
	result := fresh.Sweep(f.ctx)

	wantInt(t, len(result.Claimed), 0, "claims beside an older peer")
	if !result.Blocked() || result.BlockedByProtocol != 1 {
		t.Fatalf("blocked_by_protocol = %d (blocked=%v), want 1", result.BlockedByProtocol, result.Blocked())
	}
}

func TestNothingToClaimIsNotReportedAsBlocked(t *testing.T) {
	t.Parallel()
	// The other half: peers holding everything is normal, not a stall.
	f := newFleet(t)
	a := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx)

	b := f.newHost("node-b", Config{Seats: seatsNamed("ceo")})
	b.renewNodePresence(f.ctx)
	result := b.Sweep(f.ctx)
	wantInt(t, len(result.Claimed), 0, "claims")
	if result.Blocked() {
		t.Fatal("peers holding everything was reported as a protocol block")
	}
}

func TestPresenceSurvivesAnOlderProtocolPeer(t *testing.T) {
	t.Parallel()
	// The gate exists for the rolling upgrade; it must not make the
	// upgrading node invisible during one. A node that cannot register
	// presence is missing from the membership read, so its peers divide the
	// seats by a count that excludes it and each take a larger share — and
	// its own capacity excludes it too.
	f := newFleet(t)
	if _, err := f.store.TryAcquire(f.ctx, coord.SeatResource("ceo"), coord.AcquireOptions{
		Owner: "old-node:1", TTL: time.Minute, Protocol: 1,
	}); err != nil {
		t.Fatalf("stage an old peer: %v", err)
	}
	h := f.newHost("node-new", Config{Seats: seatsNamed("ceo"), Protocol: 2})

	h.renewNodePresence(f.ctx)
	if f.leaseOf(coord.NodeResource("node-new")) == nil {
		t.Fatal("the mixed-version gate refused a presence lease")
	}
	// Seat claims are still refused — that half of the gate stands.
	result := h.Sweep(f.ctx)
	wantInt(t, len(result.Claimed), 0, "claims")
	wantInt(t, result.BlockedByProtocol, 1, "protocol floor")
}

func TestSweepResultReportsBlockedOnlyWhenAFloorIsSet(t *testing.T) {
	t.Parallel()
	if (SweepResult{Capacity: 1, LiveNodes: 1}).Blocked() {
		t.Error("an unset floor read as blocked")
	}
	if !(SweepResult{BlockedByProtocol: 1}).Blocked() {
		t.Error("a set floor did not read as blocked")
	}
}

// --- lifecycle -------------------------------------------------------------

func TestStartClaimsBeforeTheLoopsSpin(t *testing.T) {
	t.Parallel()
	// Boot must not be eventually-consistent: the first sweep runs
	// synchronously inside Start, so a node is useful the moment it reports
	// started.
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	wantAdmits(t, h, "ceo", true)

	h.Stop(f.ctx)
	wantHeld(t, h)
}

func TestStopReleasesPresenceSoPeersReDivide(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	a := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	b := f.newHost("node-b", Config{Seats: seatsNamed("ceo")})
	a.Start(f.ctx)
	b.renewNodePresence(f.ctx)
	wantInt(t, b.Sweep(f.ctx).LiveNodes, 2, "live nodes")

	a.Stop(f.ctx)
	wantInt(t, b.Sweep(f.ctx).LiveNodes, 1, "live nodes after node-a stopped")
}

func TestStartAndStopAreIdempotent(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	h.Start(f.ctx) // a second start is a no-op, not a second pair of loops
	h.Stop(f.ctx)
	h.Stop(f.ctx)
	wantHeld(t, h)
}

func TestStartStopsWithItsContext(t *testing.T) {
	t.Parallel()
	// The loops are owned: cancelling the context that started them ends
	// them, so an engine that drops a host does not leak two goroutines.
	f := newFleet(t)
	ctx, cancel := context.WithCancel(f.ctx)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), SweepInterval: time.Millisecond, HeartbeatInterval: time.Millisecond,
	})
	h.Start(ctx)
	cancel()
	h.wg.Wait() // returns only when both loops have exited
	h.Stop(f.ctx)
}

// --- bookkeeping -----------------------------------------------------------

func TestSweepReportsWhatItReleased(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	var live atomic.Value
	live.Store([]string{"ceo", "eng"})
	h := f.newHost("node-a", Config{Seats: func() []placement.Seat {
		return seatsNamed(live.Load().([]string)...)()
	}})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	live.Store([]string{"eng"})
	wantStrings(t, h.Sweep(f.ctx).Lost, []string{"ceo"}, "lost")
}

func TestHeartbeatLossesReachTheLastSweep(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)

	epoch, _ := h.EpochFor("ceo")
	f.peerTakes("ceo", "peer:1", h.Owner(), epoch)
	wantStrings(t, h.Heartbeat(f.ctx), []string{"ceo"}, "lost")

	last, ok := h.LastSweep()
	if !ok {
		t.Fatal("there was no last sweep to update")
	}
	wantStrings(t, last.Lost, []string{"ceo"}, "last sweep lost")
}

func TestHeartbeatLossesDoNotErasePassLosses(t *testing.T) {
	t.Parallel()
	// Two writers, one list. The heartbeat used to REPLACE it, so a node
	// that shed seats to rebalance and then lost another to a peer
	// reported only the last thing that happened — the shed seats
	// vanished from the record entirely, on the one surface an operator
	// reads to find out where a company's seats went.
	f := newFleet(t)
	a := f.newHost("node-a", Config{Seats: seatsNamed("ceo", "eng")})
	a.renewNodePresence(f.ctx)
	a.Sweep(f.ctx)
	wantHeld(t, a, "ceo", "eng")

	// A peer arrives, so the fair share halves and this pass sheds one.
	f.present("node-b", time.Minute, placement.NodeProfile{})
	shed := a.Sweep(f.ctx).Lost
	if len(shed) != 1 {
		t.Fatalf("the rebalancing pass shed %v, want exactly one seat", shed)
	}

	// Then the seat it kept is taken out from under it.
	kept := a.Held()[0]
	epoch, _ := a.EpochFor(kept)
	f.peerTakes(kept, "peer:1", a.Owner(), epoch)
	wantStrings(t, a.Heartbeat(f.ctx), []string{kept}, "lost to the peer")

	last, ok := a.LastSweep()
	if !ok {
		t.Fatal("there was no last sweep to amend")
	}
	want := append(slices.Clone(shed), kept)
	slices.Sort(want)
	got := slices.Clone(last.Lost)
	slices.Sort(got)
	wantStrings(t, got, want, "everything this node stopped holding")
}

func TestAStoredSweepDoesNotAliasTheReturnedOne(t *testing.T) {
	t.Parallel()
	// The aliasing that caused the race: Sweep stored a POINTER to the
	// same value it returned, so a heartbeat appending to the host's copy
	// wrote into a slice the caller was still reading. Provoked here
	// single-threaded — a race detector only sees it when the two
	// genuinely overlap, which needs a store failure to arrange.
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	returned := h.Sweep(f.ctx)

	epoch, _ := h.EpochFor("ceo")
	f.peerTakes("ceo", "peer:1", h.Owner(), epoch)
	h.Heartbeat(f.ctx)

	if len(returned.Lost) != 0 {
		t.Errorf("the value Sweep returned grew to %v — the host is writing "+
			"through the caller's copy", returned.Lost)
	}
	if returned.Held != 1 {
		t.Errorf("the value Sweep returned now reports Held=%d, want the 1 it "+
			"returned with", returned.Held)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// renewCut fails only Renew, leaving claims and reads working. A store that
// is degraded rather than down — a renew timing out under load while a claim
// on a different code path still answers — which is the shape that produces
// the thrash below. coordtest.Faulty breaks everything at once and cannot
// express it.
type renewCut struct {
	coord.Backend
	cut atomic.Bool
}

func (r *renewCut) Renew(
	ctx context.Context, resource, owner string, epoch int64, ttl time.Duration,
) (bool, error) {
	if r.cut.Load() {
		return false, coord.ErrUnavailable
	}
	return r.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

func TestASeatLostToAnUnrenewableLeaseIsNotImmediatelyRetaken(t *testing.T) {
	t.Parallel()
	// The thrash: a node whose renews fail while its claims succeed drops
	// the seat when the TTL runs out and re-takes it on its very next
	// sweep, roughly a hundred milliseconds later. It then loses it again
	// one TTL on, forever — tearing down and respawning that seat's whole
	// runtime each cycle and abandoning its in-flight work, while a healthy
	// peer never wins a race for it. Every log line reads like a node doing
	// its job.
	//
	// Measured in the fleet suite before the backoff existed: node-a
	// claimed at epoch 107, 108, 109 in six seconds and node-b, idle and
	// healthy throughout, never got the seat.
	f := newFleet(t)
	cut := &renewCut{Backend: f.store}
	h := f.newHost("node-a", Config{Backend: cut, Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo")

	// The store degrades and the TTL runs out, so the seat is dropped:
	// this node can no longer prove it owns it.
	cut.cut.Store(true)
	f.clock.Advance(SeatLeaseTTL + time.Second)
	wantStrings(t, h.Heartbeat(f.ctx), []string{"ceo"}, "dropped")
	wantHeld(t, h)

	// The next sweep must NOT take it straight back, even though the claim
	// would succeed — the seat is unclaimed and this node's TryAcquire
	// still works.
	h.Sweep(f.ctx)
	if held := h.Held(); len(held) != 0 {
		t.Fatalf("the seat was retaken immediately (held=%v); a node that just proved it "+
			"cannot renew must stand back so a peer gets the next attempt", held)
	}

	// And it is a backoff, not a ban: once it expires the node may serve
	// the seat again, which matters when there is no peer to take it.
	f.clock.Advance(AcquireBackoff + time.Second)
	cut.cut.Store(false)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo")
}
