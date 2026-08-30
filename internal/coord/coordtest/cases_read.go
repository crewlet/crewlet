package coordtest

import (
	"fmt"
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
		// Short-TTL claim last: everything after it is real time against
		// that TTL on a backend the suite cannot fast-forward.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim("seat:pm", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		h.claim("seat:cto", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
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

	{"resources_that_need_encoding_round_trip_and_stay_distinct", func(h *harness) {
		// A STIMULUS gap, found by reading what the suite sends rather
		// than by mutating what a backend answers: every other case here
		// names a resource like "seat:ceo", so no case could ever reach a
		// backend's key encoding. No mutation can reveal that — the input
		// simply never arrives.
		//
		// It is not hypothetical. A resource name is "seat:" plus a
		// HANDLE, handles come from the org, and nothing in coord.go
		// restricts them. On a subject-addressed store a dot is a
		// separator and * and > are wildcards, so an unescaped name is
		// not a lookup failure but a COLLISION: two seats sharing one
		// record, which is the mutual exclusion this whole primitive
		// exists to provide. internal/coord/kv carries its own key
		// round-trip test because its author knew that; the contract
		// suite gave the next backend no such warning.
		//
		// The assertion is a property, never an encoding. A backend that
		// cannot represent a name may REFUSE it — the same latitude a
		// store has to refuse a TTL it cannot honour rather than clamp it
		// silently — but one that accepts it must return it unchanged and
		// must never let two names land on one record.
		type claimed struct {
			resource string
			owner    string
		}
		var accepted []claimed
		for i, handle := range []string{
			"alice.smith", "a/b", "a b", "ünïcødé", "a=b", "*", ">", "a:b", "A.B",
		} {
			name := coord.SeatResource(handle)
			owner := fmt.Sprintf("owner-%02d:1", i)
			lease, err := h.b.TryAcquire(h.ctx, name, coord.AcquireOptions{
				Owner: owner, TTL: LongTTL,
			})
			if err != nil {
				h.t.Logf("backend refused %q (allowed — refusing beats mangling): %v", name, err)
				continue
			}
			if lease == nil {
				h.t.Fatalf("%q was refused as already held, but nothing else claimed it — "+
					"an earlier name in this list encoded onto the same record", name)
			}
			if lease.Resource != name {
				h.t.Fatalf("claim on %q returned a lease for %q", name, lease.Resource)
			}
			accepted = append(accepted, claimed{name, owner})
		}
		if len(accepted) == 0 {
			h.t.Fatal("the backend accepted none of these names — this case certified nothing")
		}

		// Every accepted name is still held by ITS OWN owner. Two names
		// sharing a record shows up here as one owner wearing another's
		// lease, which is the failure the encoding exists to prevent.
		for _, c := range accepted {
			read := h.get(c.resource)
			if read == nil {
				h.t.Fatalf("%q reads as unheld right after being claimed", c.resource)
			}
			if read.Resource != c.resource || read.Owner != c.owner {
				h.t.Fatalf("%q reads back as resource %q held by %q, want %q held by %q — "+
					"two names collided onto one record",
					c.resource, read.Resource, read.Owner, c.resource, c.owner)
			}
		}

		// And the prefix read still finds them, so whatever encoding a
		// backend applies has not moved them out of their own namespace.
		live := h.listLive(coord.SeatPrefix)
		if len(live) != len(accepted) {
			h.t.Fatalf("ListLive(%q) returned %d of %d accepted names: %v",
				coord.SeatPrefix, len(live), len(accepted), resources(live))
		}
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
		// The short-TTL claim goes LAST, immediately before the lapse.
		// On a backend whose clock the suite cannot move, everything
		// between the two is real time spent against that TTL, and this
		// case's setup is three store round trips — so ordering it this
		// way removes the dependency on how fast the store is rather
		// than merely tolerating it.
		released := h.claim("seat:cto", coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		h.claim("seat:pm", coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Preferred: "node-b",
		})
		h.release("seat:cto", "node-a:1", released.Epoch)
		h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: ShortTTL, Preferred: "node-a",
		})
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
	// Meta is the one field that crosses a wire — adr-201 §2 records the
	// ownership key carrying it as JSON — so the suite separates the two
	// properties it could assert about a round trip and requires exactly
	// one of them.
	//
	// The VALUE must survive: meta_values_survive_the_round_trip writes a
	// number, a bool, a []string and a nested map and compares canonical
	// JSON. A codec that drops what it cannot encode fails it, because a
	// peer reads this payload to decide whether a node may run a seat.
	//
	// The TYPE need not: measured, an int comes back float64 from the
	// embedded-NATS store and int from one that keeps Go types, and both
	// are correct — coord.go promises only map[string]any. Requiring one
	// shape would write a codec choice into the contract, so the case is
	// mutation-checked in BOTH directions: it fails a lossy codec AND
	// passes a Go-native one, which is what stops it from becoming a type
	// requirement wearing a value requirement's name.
	//
	// The remaining payloads below are pre-shaped and cannot see either
	// property on their own; the one case above is what covers them.
	//
	// What stays unsettled is the contract's own wording: coord.go says
	// only that Meta is map[string]any, so nothing tells a caller whether
	// it may depend on the Go type of a value it reads back. The silence
	// has teeth — reverting the memory twin to hand the caller its own map
	// straight back passes every case here, so a backend that preserves Go
	// types is certified today, and a caller writing meta["replicas"].(int)
	// is right against that backend and panics against the embedded-NATS
	// store, which returns float64. Until Lease.Meta says one way or the
	// other, read meta the way placement.rolesFromMeta does: accept either
	// shape.

	{"meta_values_survive_the_round_trip", func(h *harness) {
		// The property the free-form payload actually owes, separated
		// from the one it does not.
		//
		// A backend picks its own wire format, so the Go TYPE carrying a
		// value is its business — measured, a number written here comes
		// back float64 from a JSON store and int from one that keeps Go
		// types, and both are correct. What no backend may do is LOSE
		// what a holder advertised about itself, because a peer reads it
		// to decide whether this node may run a seat, and a codec that
		// silently drops the entries it cannot encode leaves that peer
		// deciding against a profile the node never published.
		//
		// So this compares canonical JSON: int(3) and float64(3) are one
		// value, []string{"a"} and []any{"a"} are one list, and a dropped
		// or mangled entry is still a difference. Deliberately the only
		// case here that writes NON-JSON-native Go types — every other
		// meta fixture is pre-shaped, which is precisely why none of them
		// can see a codec that loses things.
		payload := map[string]any{
			"replicas": 3,
			"ratio":    1.5,
			"on":       true,
			"roles":    []string{"seats", "workers"},
			"labels":   map[string]any{"zone": "eu"},
		}
		lease := h.claim(coord.NodeResource("n1"), coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: payload,
		})
		h.requireSameValues("meta returned by the claim", payload, lease.Meta)
		h.requireSameValues("meta read back", payload, h.mustHold(coord.NodeResource("n1"), "n1:a").Meta)

		live := h.listLive(coord.NodePrefix)
		if len(live) != 1 {
			h.t.Fatalf("ListLive returned %d leases, want 1", len(live))
		}
		h.requireSameValues("meta from the membership read", payload, live[0].Meta)
	}},

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

		// Checked before writing to it, because a suite must never panic
		// on what a backend hands back. A backend whose codec drops the
		// payload returns a nil map here, and "assignment to entry in
		// nil map" takes the whole test BINARY down — aborting every
		// other case, including the ones whose failure names the actual
		// defect. Measured: a lossy-codec mutation produced exactly that,
		// and the case that would have diagnosed it never ran.
		if lease.Meta == nil {
			h.t.Fatalf("claim returned no meta at all for the payload %v — a backend that "+
				"drops what a holder advertises leaves peers deciding placement against "+
				"a profile the node never published", payload)
		}

		payload["roles"] = []any{"nothing"}
		payload["injected"] = "yes"
		lease.Meta["injected"] = "yes"

		want := map[string]any{"roles": []any{"seats"}}
		if read := h.mustHold(resource, "n1:a"); !reflect.DeepEqual(read.Meta, want) {
			h.t.Fatalf("meta %v after the caller mutated its own maps, want %v", read.Meta, want)
		}
	}},
}
