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
		}, coord.RefusedProtocol)
	}},

	{"a_peer_holding_the_resource_is_the_refusal_whatever_the_gate_says", func(h *harness) {
		// Both rules refuse this claim: the old node holds the very seat
		// asked for, and it holds it at an older protocol. The answer is
		// the HOLD, because a claim that cannot write has nothing for a
		// gate to stop — and it is what a seat host reads to decide
		// whether a pass that took nothing was stalled by an upgrade.
		// Read as a gate, every seat its peers hold would send the host
		// to judge one; read the other way round, a sweep would have to
		// judge a gate for every seat it cannot have.
		h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "old-node:1", TTL: LongTTL, Preferred: "old-node", Protocol: 1,
		})
		before := h.mustHold("seat:ceo", "old-node:1")
		h.refused("seat:ceo", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL / 2, Preferred: "new-node", Protocol: 2,
		}, coord.RefusedHeld)
		h.requireUnchanged("a claim refused by a live holder at an older protocol", before,
			h.mustHold("seat:ceo", "old-node:1"))
	}},

	{"an_older_node_still_claims_beside_a_newer_holder", func(h *harness) {
		// Asymmetric on purpose: the check only ever looks DOWN, so a
		// lower-protocol claim is never gated by a higher lease. Which is
		// exactly why a DOWNGRADE across a protocol bump needs a full
		// fleet drain — a lower-protocol build takes over a higher node's
		// expired leases unchecked.
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
		h.refused("seat:engineer", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2},
			coord.RefusedProtocol)

		h.claim("seat:ceo", coord.AcquireOptions{Owner: "old-node:1", TTL: ShortTTL, Protocol: 1})
		h.lapse()
		lease := h.claim("seat:engineer", coord.AcquireOptions{
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
		}, coord.RefusedProtocol)
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
		h.refused("seat:ceo", coord.AcquireOptions{Owner: "new-node:1", TTL: LongTTL, Protocol: 2},
			coord.RefusedProtocol)

		// Renew is deliberately NOT gated: it extends a hold this node
		// already has and is already acting on. Refusing it would drop a
		// seat mid-turn rather than prevent anything.
		if !h.renew("seat:ceo", "new-node:1", mine.Epoch, LongTTL) {
			h.t.Fatal("renew was refused by the mixed-version gate — a seat would be " +
				"dropped mid-turn for a claim it already holds")
		}
	}},

	{"a_gate_refused_claim_writes_nothing", func(h *harness) {
		// The gate's refusal is the one a backend is most likely to
		// implement as an afterthought — checked in a different place
		// from the ownership predicate, and easy to reach only after the
		// record has already been written. During a rolling upgrade the
		// newer half of the fleet is refused on every sweep, so a gated
		// refusal that writes would burn an epoch and move a hint on every
		// seat a newer node reaches for, continuously, for as long as the
		// upgrade takes: the exact window the gate exists to make safe.
		//
		// The resource refused here is one nobody holds but which has a
		// history — a tenure at epoch 1 whose hint names the node that
		// held it — because a pristine resource has nothing a stray write
		// could visibly move. (A claim on a resource a PEER holds never
		// reaches the gate at all; see
		// a_peer_holding_the_resource_is_the_refusal_whatever_the_gate_says.)
		//
		// SCOPE, because the contract permits one exception and this
		// case must not be read as forbidding it. The violation here is
		// already visible when the claim arrives, so a backend sees it
		// on its check and must write nothing. The other path is
		// deliberate: a KV cannot express the gate as a predicate
		// inside a compare-and-swap, so it does check → claim →
		// RE-CHECK → release on violation. A violation that appears
		// between a backend's check and its claim therefore leaves a
		// touched record — a burned epoch and a tombstone — because the
		// claim was made and given back rather than never made. That is
		// a recorded degradation, not a defect, and a concurrent gate
		// test must assert the claim is surrendered, never that the
		// record is pristine.
		first := h.claim("seat:engineer", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Preferred: "new-node", Protocol: 2,
		})
		if !h.release("seat:engineer", "new-node:1", first.Epoch) {
			h.t.Fatal("release of the first tenure reported failure")
		}
		old := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "old-node:1", TTL: LongTTL, Protocol: 1,
		})

		h.refused("seat:engineer", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Preferred: "elsewhere", Protocol: 2,
		}, coord.RefusedProtocol)
		if _, moved := h.preferred(coord.ClassSeat, "elsewhere")["seat:engineer"]; moved {
			h.t.Fatal("a claim the gate refused moved the resource's hint")
		}
		if floor, any := h.floor(); !any || floor != old.Protocol {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v) after a gated refusal, want (%d, true)",
				floor, any, old.Protocol)
		}

		if !h.release("seat:ceo", "old-node:1", old.Epoch) {
			h.t.Fatal("release of the old hold reported failure")
		}
		next := h.claim("seat:engineer", coord.AcquireOptions{
			Owner: "new-node:1", TTL: LongTTL, Protocol: 2,
		})
		if next.Epoch != first.Epoch+1 {
			h.t.Fatalf("the tenure after a gated refusal is at epoch %d, want %d — the refused "+
				"claim minted a token", next.Epoch, first.Epoch+1)
		}
	}},

	{"the_gate_reads_presence_leases_too", func(h *harness) {
		// The predicate is over presence as well as seats: an old node
		// that has registered itself is an old node, whether or not it
		// has taken a seat yet.
		h.claim(coord.NodeResource("old"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1, Ungated: true,
		})
		h.refused(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "new:1", TTL: LongTTL, Protocol: 2,
		}, coord.RefusedProtocol)
	}},

	{"an_older_duty_lease_does_not_hold_a_newer_claim_back", func(h *harness) {
		// THE CRASH MID-UPGRADE. The last node of the older build holds
		// its presence, a seat and a duty — the learning duty's TTL runs
		// to hours — and dies. Its presence and its seat lapse within a
		// seat lease TTL; its duty stays live for the rest of its own.
		// Counted, that duty refused every newer node's seat claim for
		// all of it: a fleet that could not place a seat for hours after
		// a crash, over a lease that says nothing the older node's
		// presence did not already say while it lived. So the gate
		// counts presence and seats and never a duty — and each of the
		// two still refuses on its own, a duty beside it or not.
		duty := h.claim(coord.WorkerResource("learning"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1, Ungated: true,
		})
		// Nor any other class a claim takes: a tracker walk's claim
		// follows the walk's length and is not the seat-host protocol.
		// (On the KV store it shares the seat lease bucket, so this is
		// what holds the gate's view to the classes rather than to the
		// bucket.)
		h.claim("move:task-1", coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1,
		})
		presence := h.claim(coord.NodeResource("old"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1, Ungated: true,
		})
		newer := coord.AcquireOptions{Owner: "new:1", TTL: LongTTL, Protocol: 2}

		// Alive and holding no seat yet: its PRESENCE refuses.
		h.refused(coord.SeatResource("engineer"), newer, coord.RefusedProtocol)

		// Draining — presence given up at the first step, the seat still
		// served until it is handed over: its SEAT refuses.
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Protocol: 1,
		})
		if !h.release(coord.NodeResource("old"), "old:1", presence.Epoch) {
			h.t.Fatal("release of the older node's presence reported failure")
		}
		h.refused(coord.SeatResource("engineer"), newer, coord.RefusedProtocol)

		// It crashes: the seat lapses, and the duty and the walk claim
		// are all it left.
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "old:1", TTL: ShortTTL, Protocol: 1,
		})
		h.lapse()
		if still := h.mustHold(coord.WorkerResource("learning"), "old:1"); still.Epoch != duty.Epoch {
			h.t.Fatalf("the older duty moved from epoch %d to %d", duty.Epoch, still.Epoch)
		}
		if lease := h.claim(coord.SeatResource("engineer"), newer); lease.Protocol != 2 {
			h.t.Fatalf("the newer claim recorded protocol %d, want 2", lease.Protocol)
		}

		// And the floor names what the gate counts: the newer seat, not
		// the older leases no claim is refused over.
		if floor, any := h.floor(); !any || floor != 2 {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v) with only an older duty and walk claim live beside "+
				"a newer seat, want (2, true): it must count what the gate counts", floor, any)
		}
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
		h.requireResources("membership", h.listLive(coord.ClassNode), "node:old", "node:new")
	}},

	{"an_ungated_claim_still_records_its_own_protocol", func(h *harness) {
		// Ungated skips the CHECK, never the stamp: the lease carries
		// the claiming build's protocol like any other. A duty's stamp
		// holds no claim back at all (see
		// an_older_duty_lease_does_not_hold_a_newer_claim_back), and no
		// lease holds a claim at its own protocol back.
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
		// A named argument defaulting to 1 makes OMITTING it harmless.
		// A struct zero makes omitting it the case that happens by
		// accident — and read as
		// "oldest", one AcquireOptions{Owner, TTL} anywhere in the
		// engine would hold a live lease below every newer node's floor
		// and stall the whole fleet's claims, looking exactly like a
		// rolling upgrade that never finishes.
		//
		// So the zero value is SAFE: an omitted protocol claims at this
		// build's version, which is what the caller meant.
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

	// --- the observability half ----------------------------------------

	{"fleet_protocol_floor_reports_the_oldest_live_holder", func(h *harness) {
		// A claim refused RefusedProtocol says the gate stopped it;
		// this is the call that names the protocol behind that
		// refusal, asked after one and never once per sweep.
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
