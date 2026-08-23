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
		// The old hold is claimed LONG for the part that must happen while
		// it is live, then shortened at the last moment. The refusal and
		// the lapse both still mean what they did, and neither is racing
		// a wall clock any more.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old-node:1", TTL: LongTTL, Protocol: 1})
		h.refused("seat:ceo", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2})

		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old-node:1", TTL: ShortTTL, Protocol: 1})
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

	{"a_gate_refused_claim_leaves_the_record_untouched", func(h *harness) {
		// The gate's refusal is the one a backend is most likely to
		// implement as an afterthought — checked in a different place
		// from the ownership predicate, and easy to reach only after the
		// record has already been written. During a rolling upgrade the
		// newer half of the fleet is refused on every sweep, so a gated
		// refusal that writes would rewrite the OLD node's live records
		// continuously, for as long as the upgrade takes: the exact
		// window the gate exists to make safe.
		//
		// SCOPE, because the corpus permits one exception and this case
		// must not be read as forbidding it. The violation here is
		// already visible when the claim arrives, so a backend sees it
		// on its check and must write nothing. d-201 §3 records the
		// other path deliberately: a KV cannot express the gate as a
		// predicate inside a compare-and-swap, so it does check → claim
		// → RE-CHECK → release on violation. A violation that appears
		// between a backend's check and its claim therefore leaves a
		// touched record — a burned epoch and a tombstone — because the
		// claim was made and given back rather than never made. That is
		// a recorded degradation, not a defect, and a concurrent gate
		// test must assert the claim is surrendered, never that the
		// record is pristine.
		old := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "old-node:1", TTL: LongTTL, Preferred: "old-node", Protocol: 1,
		})
		before := h.mustHold("seat:ceo", "old-node:1")

		h.refused("seat:ceo", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL / 2, Preferred: "new-node", Protocol: 2,
		})
		h.requireUnchanged("a claim refused by the gate", before, h.mustHold("seat:ceo", "old-node:1"))
		if floor, any := h.floor(); !any || floor != old.Protocol {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v) after a gated refusal, want (%d, true)",
				floor, any, old.Protocol)
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

	{"an_omitted_protocol_claims_at_this_build", func(h *harness) {
		// Go moves the danger, so the contract moves the default.
		//
		// Python's Protocol was a keyword argument defaulting to 1, so
		// OMITTING it was harmless. Go's is a struct zero, so omitting
		// it is the case that happens by accident — and read as
		// "oldest", one AcquireOptions{Owner, TTL} anywhere in the
		// engine would hold a live lease below every newer node's floor
		// and stall the whole fleet's claims, looking exactly like a
		// rolling upgrade that never finishes.
		//
		// So the zero value is SAFE: an omitted protocol claims at this
		// build's version, which is what the caller meant. The opposite
		// case — a STORED record with no protocol — still reads as the
		// oldest, because that record genuinely predates the concept;
		// coord.StoredProtocol is the read-side half.
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		if lease.Protocol != coord.ProtocolVersion {
			h.t.Fatalf("a claim with no protocol recorded %d, want %d (this build)",
				lease.Protocol, coord.ProtocolVersion)
		}
		if floor, any := h.floor(); !any || floor != coord.ProtocolVersion {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v), want (%d, true)",
				floor, any, coord.ProtocolVersion)
		}
		// And crucially it does NOT gate a peer running this same build.
		h.claim("seat:engineer", coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Protocol: coord.ProtocolVersion,
		})
	}},

	{"a_stored_record_with_no_protocol_reads_as_the_oldest", func(h *harness) {
		// The read-side half, and the fail-closed one: a record written
		// before the field existed must gate newer claims until it
		// lapses, exactly as a real v1 hold would.
		//
		// READ THE SCOPE BEFORE TRUSTING THIS CASE. Now that a claim
		// normalises to this build, NOTHING reachable through
		// coord.Backend can store a protocol of zero — so the suite
		// cannot plant the record this rule is about, and what follows
		// checks the shared helper plus the behaviour the rule
		// reproduces, NOT that this backend calls the helper on its own
		// decode path. A durable backend reading the field raw passes
		// here and still lets an ancient record read as current. That
		// obligation belongs to each durable backend's own tests, where
		// the record can be written out of band (internal/coord/kv does
		// it in record.go); an in-memory store has no records that
		// predate its own process and nothing to check.
		if got := coord.StoredProtocol(0); got != 1 {
			h.t.Fatalf("StoredProtocol(0) = %d, want 1", got)
		}
		if got := coord.StoredProtocol(3); got != 3 {
			h.t.Fatalf("StoredProtocol(3) = %d, want 3", got)
		}
		// An explicit older claim still gates, which is the behaviour
		// the stored reading exists to reproduce.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old:1", TTL: LongTTL, Protocol: 1})
		h.refused("seat:engineer", coord.AcquireOptions{
			Owner: "new:1", TTL: LongTTL, Protocol: coord.ProtocolVersion,
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

		// Long while the floor is read, shortened immediately before the
		// lapse: an unbroken same-owner re-claim keeps the epoch and just
		// moves the deadline in. A full fleet scan inside a 100ms window
		// is how this case used to fail on a real broker under load.
		h.claim("seat:eng", coord.AcquireOptions{Owner: "old:1", TTL: LongTTL, Protocol: 1})
		if floor, any := h.floor(); !any || floor != 1 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v), want (1, true)", floor, any)
		}

		h.claim("seat:eng", coord.AcquireOptions{Owner: "old:1", TTL: ShortTTL, Protocol: 1})
		h.lapse()
		if floor, any := h.floor(); !any || floor != 3 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v) after the old hold lapsed, want (3, true)",
				floor, any)
		}
	}},

	{"fleet_protocol_floor_ignores_lapsed_and_released_leases", func(h *harness) {
		// Short-TTL claim last — see the same reorder in the read cases.
		released := h.claim("seat:cto", coord.AcquireOptions{
			Owner: "old:2", TTL: LongTTL, Protocol: 1,
		})
		h.release("seat:cto", "old:2", released.Epoch)
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old:1", TTL: ShortTTL, Protocol: 1})
		h.lapse()
		if floor, any := h.floor(); any {
			h.t.Fatalf("FleetProtocolFloor = (%d, true) with nothing live, want (_, false)", floor)
		}
	}},
}
