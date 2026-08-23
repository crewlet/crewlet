package coordtest

import "github.com/crewlet/crewlet/internal/coord"

// protocolCases certify the mixed-version gate.
//
// A rolling upgrade puts a vN and a vN+1 node on one store at once. That is
// fine while both agree on what HOLDING A LEASE MEANS, and catastrophic when
// they do not: two nodes that disagree about whether a seat's inbox consumer
// is owner-only, or about whether a claim honours the role's placement, are
// each individually correct and jointly wrong. So the newer node waits.
var protocolCases = []testCase{
	{"a_newer_node_refuses_to_claim_beside_an_older_holder", func(h *harness) {
		// The gate stated as the deploy it protects. The old node holds
		// ONE seat; the new node must claim NOTHING — not even an
		// unrelated, entirely unclaimed seat — until that hold ends. The
		// predicate is fleet-wide because the disagreement is about
		// meaning, not about a resource.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old-node:1", TTL: LongTTL, Protocol: 1})
		h.refused("seat:engineer", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Protocol: 2,
		})
	}},

	{"an_older_node_still_claims_beside_a_newer_holder", func(h *harness) {
		// Asymmetric on purpose: the old build has no check to run, and
		// nothing in the store can make it run one. Which is exactly why
		// a DOWNGRADE across a protocol bump needs a full fleet drain —
		// an older build will happily take over a newer node's expired
		// leases.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2})
		h.claim("seat:engineer", coord.AcquireOptions{Owner: "old-node:1", TTL: LongTTL, Protocol: 1})
	}},

	{"same_protocol_peers_are_unaffected", func(h *harness) {
		// The gate must not fire between peers of the same build. That
		// is every normal deployment, and firing there would be a
		// fleet-wide stall.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL, Protocol: 2})
		h.claim("seat:engineer", coord.AcquireOptions{Owner: "node-b:1", TTL: LongTTL, Protocol: 2})
	}},

	{"the_gate_lifts_when_the_old_lease_lapses", func(h *harness) {
		// A rolling deploy converges because that is what a rolling
		// deploy does: drain the old, the new take over.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old-node:1", TTL: ShortTTL, Protocol: 1})
		h.refused("seat:ceo", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2})

		h.lapse()
		lease := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Protocol: 2,
		})
		if lease.Protocol != 2 {
			h.t.Fatalf("claim recorded protocol %d, want the claiming build's 2", lease.Protocol)
		}
	}},

	{"the_gate_lifts_when_the_old_lease_is_released", func(h *harness) {
		old := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "old-node:1", TTL: LongTTL, Protocol: 1,
		})
		h.refused("seat:engineer", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Protocol: 2,
		})
		if !h.release("seat:ceo", "old-node:1", old.Epoch) {
			h.t.Fatal("release of the old hold reported failure")
		}
		h.claim("seat:engineer", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2})
	}},

	{"a_newer_node_cannot_renew_its_way_around_the_gate", func(h *harness) {
		// Re-acquire is the renew path for a live lease, so it goes
		// through the same guard — otherwise a node that claimed before
		// the old one appeared would keep extending indefinitely and the
		// mixed fleet would never converge.
		mine := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Protocol: 2,
		})
		h.claim("seat:engineer", coord.AcquireOptions{Owner: "old-node:1", TTL: LongTTL, Protocol: 1})
		h.refused("seat:ceo", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2})

		// Renew is deliberately NOT gated: it extends a hold this node
		// already has and is already acting on. Refusing it would drop a
		// seat mid-turn rather than prevent anything.
		if !h.renew("seat:ceo", "new-node:1", mine.Epoch, LongTTL) {
			h.t.Fatal("renew was refused by the mixed-version gate — a seat would be " +
				"dropped mid-turn for a claim it already holds")
		}
	}},

	{"the_gate_reads_presence_leases_too", func(h *harness) {
		// The predicate is over every live lease, presence included: an
		// old node that has registered itself is an old node, whether or
		// not it has taken a seat yet.
		h.claim(coord.NodeResource("old"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1, Ungated: true,
		})
		h.refused(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "new:1", TTL: LongTTL, Protocol: 2,
		})
	}},

	{"ungated_claims_skip_the_gate", func(h *harness) {
		// Membership is not work. A newer node that cannot register
		// itself during the very upgrade the gate exists for is
		// invisible in the membership read — its peers then divide the
		// seats by a count that excludes it and each take a larger
		// share, while its own capacity calculation also excludes
		// itself.
		h.claim(coord.NodeResource("old"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1, Ungated: true,
		})
		h.claim(coord.NodeResource("new"), coord.AcquireOptions{
			Owner: "new:1", TTL: LongTTL, Protocol: 2, Ungated: true,
		})
		h.requireResources("membership", h.listLive(coord.NodePrefix), "node:old", "node:new")
	}},

	{"an_ungated_claim_still_records_its_own_protocol", func(h *harness) {
		// Ungated skips the CHECK, never the stamp. A long-lived
		// singleton record left at protocol 1 by a build that predates
		// the gate would block every seat claim in the fleet the moment
		// the version moved; carrying this build's protocol is what
		// stops an opted-out claim from becoming the thing that blocks.
		duty := h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Protocol: 3, Ungated: true,
		})
		if duty.Protocol != 3 {
			h.t.Fatalf("ungated claim recorded protocol %d, want 3", duty.Protocol)
		}
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Protocol: 3,
		})
	}},

	{"an_unset_protocol_reads_as_the_oldest", func(h *harness) {
		// Zero is not a protocol. A record whose version is unknown has
		// to read as the OLDEST one — fail-closed, and converging: it
		// gates newer claims until it lapses, exactly as a real v1 hold
		// would. Storing the zero verbatim would instead put every
		// build in the fleet above the floor forever.
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		if lease.Protocol != 1 {
			h.t.Fatalf("a claim with no protocol recorded %d, want 1", lease.Protocol)
		}
		if floor, any := h.floor(); !any || floor != 1 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v), want (1, true)", floor, any)
		}
		h.refused("seat:engineer", coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Protocol: coord.ProtocolVersion,
		})
	}},

	// --- the observability half ----------------------------------------

	{"fleet_protocol_floor_reports_the_oldest_live_holder", func(h *harness) {
		// TryAcquire can only answer yes or no, so a node stalled by the
		// gate looks identical to one whose peers simply hold every
		// seat. This is the call that tells them apart — once per claim
		// sweep, not once per resource.
		if floor, any := h.floor(); any {
			h.t.Fatalf("FleetProtocolFloor = (%d, true) on an empty store, want (_, false)", floor)
		}

		h.claim("seat:ceo", coord.AcquireOptions{Owner: "new:1", TTL: LongTTL, Protocol: 3})
		if floor, any := h.floor(); !any || floor != 3 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v), want (3, true)", floor, any)
		}

		h.claim("seat:eng", coord.AcquireOptions{Owner: "old:1", TTL: ShortTTL, Protocol: 1})
		if floor, any := h.floor(); !any || floor != 1 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v), want (1, true)", floor, any)
		}

		h.lapse()
		if floor, any := h.floor(); !any || floor != 3 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v) after the old hold lapsed, want (3, true)",
				floor, any)
		}
	}},

	{"fleet_protocol_floor_ignores_lapsed_and_released_leases", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old:1", TTL: ShortTTL, Protocol: 1})
		released := h.claim("seat:cto", coord.AcquireOptions{
			Owner: "old:2", TTL: LongTTL, Protocol: 1,
		})
		h.release("seat:cto", "old:2", released.Epoch)
		h.lapse()
		if floor, any := h.floor(); any {
			h.t.Fatalf("FleetProtocolFloor = (%d, true) with nothing live, want (_, false)", floor)
		}
	}},
}
