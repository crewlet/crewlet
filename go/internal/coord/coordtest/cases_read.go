package coordtest

import (
	"reflect"

	"github.com/crewlet/crewlet/internal/coord"
)

// readCases certify the read surface: what Get answers, what the two listings
// mean, the stickiness hint, and the meta payload presence carries.
var readCases = []testCase{
	{"get_of_an_unclaimed_resource_is_nil", func(h *harness) {
		h.mustBeUnheld("seat:ceo")
	}},

	{"get_returns_the_live_holder", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a", Protocol: 3,
		})
		read := h.mustHold("seat:ceo", "node-a:1")
		if read.Epoch != lease.Epoch || read.Preferred != "node-a" || read.Protocol != 3 {
			h.t.Fatalf("Get returned %+v, want the record the claim wrote (%+v)", read, lease)
		}
	}},

	{"get_excludes_a_lapsed_lease", func(h *harness) {
		// A Lease handed out by the store was live when it was read —
		// that is what lets a caller act on one without re-checking a
		// deadline against its own wall clock. So a lapsed record reads
		// as nil, exactly as an unclaimed one does. (The Python store's
		// get() returned lapsed rows too; nothing in the engine read
		// them, and the narrower answer is the one callers can trust.)
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		h.mustBeUnheld("seat:ceo")
		// ...and the record is still there underneath, which is the
		// whole reason release expires rather than deletes.
		taken := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if taken.Epoch <= lease.Epoch {
			h.t.Fatalf("epoch %d after a lapse of epoch %d — the record was dropped, "+
				"not expired", taken.Epoch, lease.Epoch)
		}
	}},

	{"get_excludes_a_released_lease", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.release("seat:ceo", "node-a", lease.Epoch)
		h.mustBeUnheld("seat:ceo")
	}},

	{"list_owned_excludes_lapsed_leases", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim("seat:cto", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.claim("seat:pm", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		h.lapse()

		h.requireResources(`ListOwned("node-a")`, h.listOwned("node-a"), "seat:ceo")
		h.requireResources(`ListOwned("node-b")`, h.listOwned("node-b"), "seat:pm")
		h.requireResources(`ListOwned("node-c")`, h.listOwned("node-c"))
	}},

	{"list_owned_excludes_released_leases", func(h *harness) {
		// A drain watches this list converge to empty. A released lease
		// that kept reporting as owned would make a graceful shutdown
		// wait out its whole TTL.
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim("seat:cto", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.release("seat:ceo", "node-a", lease.Epoch)
		h.requireResources(`ListOwned("node-a")`, h.listOwned("node-a"), "seat:cto")
	}},

	{"list_live_filters_by_prefix", func(h *harness) {
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim(coord.NodeResource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Ungated: true,
		})

		h.requireResources("live seats", h.listLive(coord.SeatPrefix), "seat:ceo")
		h.requireResources("live workers", h.listLive(coord.WorkerPrefix), "worker:scheduler")
		h.requireResources("live nodes", h.listLive(coord.NodePrefix), "node:node-a")
		h.requireResources("every live lease", h.listLive(""),
			"seat:ceo", "worker:scheduler", "node:node-a")
	}},

	{"presence_leases_are_the_membership_read", func(h *harness) {
		// Counting live presence leases is how a node learns the fleet
		// size it divides the seats by. Inferring it from SEAT ownership
		// cannot work: a fleet where nobody has claimed anything yet
		// reads as zero nodes, and every node then takes every seat.
		for _, id := range []string{"a", "b", "c"} {
			h.claim(coord.NodeResource(id), coord.AcquireOptions{
				Owner: id + ":1", TTL: LongTTL, Ungated: true,
			})
		}
		// One node dies. Its presence has to stop counting toward
		// capacity as soon as its lease runs out, or the survivors keep
		// dividing the seats by a peer that is gone and leave seats dark.
		h.claim(coord.NodeResource("c"), coord.AcquireOptions{
			Owner: "c:1", TTL: ShortTTL, Ungated: true,
		})
		h.lapse()
		h.requireResources("membership", h.listLive(coord.NodePrefix), "node:a", "node:b")
	}},

	// --- the stickiness hint -------------------------------------------

	{"the_preferred_hint_round_trips", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		if lease.Preferred != "node-a" {
			h.t.Fatalf("claim returned preferred %q", lease.Preferred)
		}
		if read := h.mustHold("seat:ceo", "node-a:1"); read.Preferred != "node-a" {
			h.t.Fatalf("Get returned preferred %q", read.Preferred)
		}
		h.requireSet("hints for node-a", h.preferred(coord.SeatPrefix, "node-a"), "seat:ceo")
		h.requireSet("hints for node-b", h.preferred(coord.SeatPrefix, "node-b"))
	}},

	{"the_preferred_hint_orders_and_never_gates", func(h *harness) {
		// The hint outlives the node that set it, so gating a claim on
		// it would strand a dead node's seats forever — every sweep
		// reading healthy while the seats sit dark. It ranks claims and
		// nothing else; a node the hint does not name still takes the
		// resource the moment it is free.
		first := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: ShortTTL, Preferred: "node-a",
		})
		h.lapse()
		taken := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b:1", TTL: LongTTL})
		if taken.Epoch <= first.Epoch {
			h.t.Fatalf("takeover kept epoch %d", taken.Epoch)
		}
		// A claim that names no node leaves the hint alone: it is the
		// last DELIBERATE placement, not a record of who happens to hold
		// the resource now.
		if taken.Preferred != "node-a" {
			h.t.Fatalf("a claim with no hint overwrote it with %q", taken.Preferred)
		}
	}},

	{"a_claim_that_names_a_node_replaces_the_hint", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: ShortTTL, Preferred: "node-a",
		})
		h.lapse()
		taken := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Preferred: "node-b",
		})
		if taken.Preferred != "node-b" {
			h.t.Fatalf("hint is %q after a claim that named node-b", taken.Preferred)
		}
		h.requireSet("hints for node-a", h.preferred(coord.SeatPrefix, "node-a"))
		h.requireSet("hints for node-b", h.preferred(coord.SeatPrefix, "node-b"), "seat:ceo")
	}},

	{"preferred_resources_include_lapsed_and_released_ones", func(h *harness) {
		// The whole point of the hint. A live-only read would answer
		// nothing in exactly the case it exists for: a node coming back
		// from a restart looking for the seats whose MCP children and
		// caches it had warm.
		h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: ShortTTL, Preferred: "node-a",
		})
		released := h.claim("seat:cto", coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		h.claim("seat:pm", coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Preferred: "node-b",
		})
		h.release("seat:cto", "node-a:1", released.Epoch)
		h.lapse()

		h.requireResources("live seats", h.listLive(coord.SeatPrefix), "seat:pm")
		h.requireSet("hints for node-a", h.preferred(coord.SeatPrefix, "node-a"),
			"seat:ceo", "seat:cto")
	}},

	{"preferred_resources_are_scoped_to_the_prefix", func(h *harness) {
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		h.requireSet("seat hints", h.preferred(coord.SeatPrefix, "node-a"), "seat:ceo")
		h.requireSet("worker hints", h.preferred(coord.WorkerPrefix, "node-a"), "worker:scheduler")
	}},

	// --- meta: what the holder IS --------------------------------------
	//
	// The payloads below stay JSON-native (strings, slices and maps of
	// them) on purpose: a backend that persists meta as JSON must not fail
	// this suite over its codec's choice of number type.

	{"meta_round_trips_through_every_read", func(h *harness) {
		payload := map[string]any{
			"roles":  []any{"seats"},
			"labels": map[string]any{"zone": "eu"},
		}
		lease := h.claim(coord.NodeResource("n1"), coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: payload,
		})
		if !reflect.DeepEqual(lease.Meta, payload) {
			h.t.Fatalf("claim returned meta %v, want %v", lease.Meta, payload)
		}
		read := h.mustHold(coord.NodeResource("n1"), "n1:a")
		if !reflect.DeepEqual(read.Meta, payload) {
			h.t.Fatalf("Get returned meta %v, want %v", read.Meta, payload)
		}
		live := h.listLive(coord.NodePrefix)
		if len(live) != 1 || !reflect.DeepEqual(live[0].Meta, payload) {
			h.t.Fatalf("ListLive returned %v, want one lease carrying %v", live, payload)
		}
		owned := h.listOwned("n1:a")
		if len(owned) != 1 || !reflect.DeepEqual(owned[0].Meta, payload) {
			h.t.Fatalf("ListOwned returned %v, want one lease carrying %v", owned, payload)
		}
	}},

	{"a_lease_written_without_meta_reads_as_empty", func(h *harness) {
		// The pre-migration record shape, and the shape of every seat
		// lease. Callers read empty as "does everything, labelled with
		// nothing" — the old behaviour, and the only safe reading of a
		// peer that never told you.
		lease := h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL,
		})
		if len(lease.Meta) != 0 {
			h.t.Fatalf("claim without meta returned %v", lease.Meta)
		}
		if read := h.mustHold(coord.SeatResource("ceo"), "n1:a"); len(read.Meta) != 0 {
			h.t.Fatalf("Get of a lease without meta returned %v", read.Meta)
		}
	}},

	{"a_claim_that_says_nothing_leaves_meta_alone", func(h *harness) {
		// An empty payload keeps what is there, as a rule about the
		// PAYLOAD rather than about which resource it is. A renew that
		// forgets to re-send the profile must not silently un-label a
		// node mid-flight — peers would read that as a node matching no
		// placement at all and quietly stop giving it seats.
		resource := coord.NodeResource("n1")
		payload := map[string]any{"roles": []any{"workers"}}
		h.claim(resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: payload,
		})
		again := h.claim(resource, coord.AcquireOptions{Owner: "n1:a", TTL: LongTTL, Ungated: true})
		if !reflect.DeepEqual(again.Meta, payload) {
			h.t.Fatalf("re-claim with no meta returned %v, want %v", again.Meta, payload)
		}
	}},

	{"meta_is_replaced_not_merged", func(h *harness) {
		// A node that drops a role must stop advertising it. Merging
		// would make a role impossible to remove without a restart AND a
		// lease expiry, and re-sending the profile on every renew is
		// worth doing precisely because it tracks the live process.
		resource := coord.NodeResource("n1")
		h.claim(resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true,
			Meta: map[string]any{"roles": []any{"seats", "workers"}, "labels": map[string]any{"zone": "eu"}},
		})
		updated := h.claim(resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true,
			Meta: map[string]any{"roles": []any{"seats"}},
		})
		want := map[string]any{"roles": []any{"seats"}}
		if !reflect.DeepEqual(updated.Meta, want) {
			h.t.Fatalf("meta %v after re-profiling, want %v (merged, not replaced?)",
				updated.Meta, want)
		}
	}},

	{"meta_is_copied_in_and_out", func(h *harness) {
		// A store that aliased the caller's map would let a later
		// mutation on either side rewrite a peer's view of this node —
		// a hazard an out-of-process backend cannot have and an
		// in-process twin gets for free unless it copies.
		resource := coord.NodeResource("n1")
		payload := map[string]any{"roles": []any{"seats"}}
		lease := h.claim(resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: payload,
		})

		payload["roles"] = []any{"nothing"}
		payload["injected"] = "yes"
		lease.Meta["injected"] = "yes"

		want := map[string]any{"roles": []any{"seats"}}
		if read := h.mustHold(resource, "n1:a"); !reflect.DeepEqual(read.Meta, want) {
			h.t.Fatalf("meta %v after the caller mutated its own maps, want %v", read.Meta, want)
		}
	}},
}
