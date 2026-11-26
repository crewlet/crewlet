package coordtest

import (
	"context"
	"fmt"
	"reflect"
	"time"

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
		// as nil, exactly as an unclaimed one does. (A read that returned
		// lapsed rows too would have no caller, and the narrower answer
		// is the one callers can trust.)
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

	{"a_store_lease_is_live_on_the_stores_word_whatever_the_readers_clock", func(h *harness) {
		// A reader whose clock runs an hour ahead of the store's, inside the
		// two hours a fleet allows between machines, sees every deadline an
		// hour in its past, and each lease Get and ListLive hand out is still
		// its owner's (coord.Holder). The control: on that clock each lease
		// reads lapsed. A resource nobody holds is still nobody's.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		h.claim("seat:cto", coord.AcquireOptions{Owner: "node-b:1", TTL: LongTTL})
		ahead := readerAhead{b: h.b, skew: time.Hour}
		listed, err := ahead.ListLive(h.ctx, coord.ClassSeat)
		if err != nil {
			h.t.Fatalf("ListLive(seat): unexpected error: %v", err)
		}
		h.requireResources("ListLive(seat)", listed, "seat:ceo", "seat:cto")
		for _, lease := range listed {
			h.requireLapsedAhead(ahead, coord.ClassSeat, lease)
			owner, err := coord.Holder(h.ctx, ahead, lease.Resource)
			if err != nil || owner != lease.Owner {
				h.t.Fatalf("Holder(%q) read on a clock an hour ahead = %q (%v), want %q, the owner "+
					"the store named", lease.Resource, owner, err, lease.Owner)
			}
		}
		if owner, err := coord.Holder(h.ctx, ahead, "seat:pm"); err != nil || owner != "" {
			h.t.Fatalf("Holder of a resource nobody claimed = %q (%v), want nobody", owner, err)
		}
	}},

	{"a_leases_time_left_is_read_on_the_stores_clock_whatever_the_readers", func(h *harness) {
		// The same reader, an hour ahead, would read every lease as run out on
		// its own clock (the control). On the store's clock for the lease's
		// class (coord.TimeLeft), each lease a listing hands it has time left,
		// never more than its TTL, in each bucket a store may keep: a seat's,
		// a duty's. A store whose clock the suite can move takes the time left
		// down with it.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		h.claim("worker:scheduler", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL, Ungated: true})
		ahead := readerAhead{b: h.b, skew: time.Hour}
		read := func(want time.Duration) {
			h.t.Helper()
			for class, resource := range map[coord.Class]string{
				coord.ClassSeat: "seat:ceo", coord.ClassWorker: "worker:scheduler",
			} {
				listed, err := ahead.ListLive(h.ctx, class)
				if err != nil {
					h.t.Fatalf("ListLive(%s): unexpected error: %v", class, err)
				}
				h.requireResources("ListLive("+string(class)+")", listed, resource)
				h.requireLapsedAhead(ahead, class, listed[0])
				if left := h.timeLeft(ahead, class)(listed[0]); left <= 0 || left > want {
					h.t.Fatalf("TimeLeft(%s) read on a clock an hour ahead = %v, want time left, "+
						"at most %v", resource, left, want)
				}
			}
		}
		read(LongTTL)
		if a, ok := h.b.(Advancer); ok {
			a.Advance(LongTTL / 2)
			h.travelled = true
			read(LongTTL / 2)
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
		// A STIMULUS gap: every other case names a resource like "seat:ceo",
		// so none reaches a backend's key encoding, and no mutation can reveal
		// an input that never arrives. A resource name is "seat:" plus a
		// HANDLE, which nothing in coord.go restricts; on a subject-addressed
		// store a dot separates and * and > are wildcards, so an unescaped name
		// is a COLLISION, two seats sharing one record, which defeats the
		// mutual exclusion this primitive exists to provide.
		//
		// The assertion is a property, never an encoding: a backend that
		// cannot represent a name may REFUSE it (as it may refuse a TTL it
		// cannot honour), but one that accepts it must return it unchanged and
		// never let two names land on one record.
		type claimed struct {
			resource string
			owner    string
		}
		var accepted []claimed
		for i, handle := range []string{
			"alice.smith", "a/b", "a b", "ünïcødé", "a=b", "*", ">", "a:b", "A.B",
		} {
			name := coord.ClassSeat.Resource(handle)
			owner := fmt.Sprintf("owner-%02d:1", i)
			lease, _, err := h.b.TryAcquire(h.ctx, name, coord.AcquireOptions{
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
		live := h.listLive(coord.ClassSeat)
		if len(live) != len(accepted) {
			h.t.Fatalf("ListLive(%q) returned %d of %d accepted names: %v",
				coord.ClassSeat, len(live), len(accepted), resources(live))
		}
	}},

	{"list_live_answers_one_class", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.claim(coord.NodeResource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Ungated: true,
		})
		// A caller's own class names the SAME node as its presence — so
		// a listing that matched on the node id rather than on the class
		// would count one node twice in each.
		h.claim(callerClass.Resource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Ungated: true,
		})

		// Each class sees its own and NOTHING else. On the native backend
		// the broker is what narrows this — a class is the leading subject
		// token of a resource's key — and on the twin it is a prefix test,
		// so the two agree only if both select exactly one class.
		h.requireResources("live seats", h.listLive(coord.ClassSeat), "seat:ceo")
		h.requireResources("live workers", h.listLive(coord.ClassWorker), "worker:scheduler")
		h.requireResources("live nodes", h.listLive(coord.ClassNode), "node:node-a")
		h.requireResources("live claims", h.listLive(callerClass), "claim:node-a")
	}},

	{"a_class_that_cannot_address_a_key_is_refused", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})

		// REFUSED, NEVER ANSWERED EMPTY. A class is one segment of a
		// resource name, so one that is empty or carries the separator
		// builds a filter matching no key at all — and "no leases" is
		// indistinguishable from "that class has no members" at every
		// caller. There is deliberately no all-classes listing: the empty
		// class used to mean one, which is why this case replaced it.
		for _, class := range []coord.Class{"", "seat:", "se.at"} {
			if leases, err := h.b.ListLive(h.ctx, class); err == nil {
				h.t.Fatalf("ListLive(%q) answered %v; a class that cannot address "+
					"a key must be refused, because an empty answer reads as an "+
					"empty class", class, resources(leases))
			}
			if hints, err := h.b.PreferredResources(h.ctx, class, "node-a"); err == nil {
				h.t.Fatalf("PreferredResources(%q) answered %v, want a refusal", class, hints)
			}
			if now, err := h.b.Now(h.ctx, class); err == nil {
				h.t.Fatalf("Now(%q) answered %v, want a refusal: no lease is on that class's clock",
					class, now)
			}
		}
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
		h.requireResources("membership", h.listLive(coord.ClassNode), "node:a", "node:b")
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
		h.requireSet("hints for node-a", h.preferred(coord.ClassSeat, "node-a"), "seat:ceo")
		h.requireSet("hints for node-b", h.preferred(coord.ClassSeat, "node-b"))
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
		h.requireSet("hints for node-a", h.preferred(coord.ClassSeat, "node-a"))
		h.requireSet("hints for node-b", h.preferred(coord.ClassSeat, "node-b"), "seat:ceo")
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

		h.requireResources("live seats", h.listLive(coord.ClassSeat), "seat:pm")
		h.requireSet("hints for node-a", h.preferred(coord.ClassSeat, "node-a"),
			"seat:ceo", "seat:cto")
	}},

	{"ever_present_names_every_node_that_held_presence_lapsed_and_released_ones_included", func(h *harness) {
		// The media gate's roll of every node that may settle a run: a node
		// offline when it reads (a lapsed lease, or one released at a stop)
		// is exactly the one it must still count, and no other class's
		// resource is a node. The short-TTL claim goes last, before the lapse.
		released := h.claim(coord.NodeResource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Ungated: true,
		})
		h.release(coord.NodeResource("node-a"), "node-a:1", released.Epoch)
		h.claim(coord.NodeResource("node-b"), coord.AcquireOptions{
			Owner: "node-b:1", TTL: LongTTL, Ungated: true,
		})
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b:1", TTL: LongTTL, Preferred: "node-b"})
		h.claim(coord.NodeResource("node-c"), coord.AcquireOptions{
			Owner: "node-c:1", TTL: ShortTTL, Ungated: true,
		})
		h.lapse()

		h.requireResources("live nodes", h.listLive(coord.ClassNode), coord.NodeResource("node-b"))
		got, err := h.b.EverPresent(h.ctx)
		if err != nil || !reflect.DeepEqual(got, []string{"node-a", "node-b", "node-c"}) {
			h.t.Fatalf("EverPresent = %v, %v, want node-a, node-b and node-c", got, err)
		}
	}},

	{"preferred_resources_are_scoped_to_the_prefix", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
		})
		h.requireSet("seat hints", h.preferred(coord.ClassSeat, "node-a"), "seat:ceo")
		h.requireSet("worker hints", h.preferred(coord.ClassWorker, "node-a"), "worker:scheduler")
	}},

	// --- meta: what the holder IS ---
	//
	// Meta crosses a wire as JSON, so the suite requires its VALUE to survive
	// (meta_values_survive_the_round_trip compares canonical JSON; peers place
	// seats from it) but not its TYPE: an int comes back float64 from the NATS
	// store and coord.go promises only map[string]any. The case is mutation-
	// checked BOTH ways (fails a lossy codec, passes a Go-native one), so it
	// never becomes a type requirement. Unsettled: a caller relying on a Go
	// type panics against the NATS store, so read meta as
	// placement.rolesFromMeta does, accepting either shape.

	{"meta_values_survive_the_round_trip", func(h *harness) {
		// A backend picks its wire format, so the Go TYPE of a value is its
		// business; what none may do is LOSE what a holder advertised, since a
		// peer decides whether this node may run a seat from it and a codec
		// silently dropping entries leaves it deciding on a profile never
		// published.
		//
		// So this compares canonical JSON: int(3) and float64(3) are one
		// value, []string{"a"} and []any{"a"} one list, and a dropped or
		// mangled entry still differs. It is the only case writing NON-JSON-
		// native Go types; the pre-shaped fixtures cannot see a lossy codec.
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

		live := h.listLive(coord.ClassNode)
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
		live := h.listLive(coord.ClassNode)
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
		lease := h.claim("seat:ceo", coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL,
		})
		if len(lease.Meta) != 0 {
			h.t.Fatalf("claim without meta returned %v", lease.Meta)
		}
		if read := h.mustHold("seat:ceo", "n1:a"); len(read.Meta) != 0 {
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

// readerAhead is a backend as seen by a reader whose clock runs skew ahead of
// the store's. That reader judging a store's instant on its own clock judges as
// a reader in step with the store would judge the instant moved skew earlier;
// the suite can move no clock back (an Advancer moves a store's forward), so
// readerAhead moves the instants.
type readerAhead struct {
	b    coord.Backend
	skew time.Duration
}

func (r readerAhead) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	lease, err := r.b.Get(ctx, resource)
	if lease == nil {
		return nil, err
	}
	seen := r.seen(*lease)
	return &seen, err
}

func (r readerAhead) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	leases, err := r.b.ListLive(ctx, class)
	for i := range leases {
		leases[i] = r.seen(leases[i])
	}
	return leases, err
}

// Now is the store's clock, an instant of it moved as every other.
func (r readerAhead) Now(ctx context.Context, class coord.Class) (time.Time, error) {
	now, err := r.b.Now(ctx, class)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-r.skew), nil
}

// seen is lease with its instants as the reader's clock stands to them.
func (r readerAhead) seen(lease coord.Lease) coord.Lease {
	lease.ExpiresAt = lease.ExpiresAt.Add(-r.skew)
	lease.AcquiredAt = lease.AcquiredAt.Add(-r.skew)
	return lease
}

// requireLapsedAhead is the control of a case claiming a reader takes the
// store's word whatever its clock: on ahead's own clock, lease, as ahead was
// handed it, reads lapsed. It is handed the store's deadline ahead's lead early,
// and has less than that lead left on the store's clock. Said in the store's
// instants: the reader's clock is this process's, and a test holds no deadline
// against the wall clock (internal/fixedwait).
func (h *harness) requireLapsedAhead(ahead readerAhead, class coord.Class, lease coord.Lease) {
	h.t.Helper()
	stored := h.mustHold(lease.Resource, lease.Owner)
	left := h.timeLeft(h.b, class)(*stored)
	if !lease.ExpiresAt.Equal(stored.ExpiresAt.Add(-ahead.skew)) || left >= ahead.skew {
		h.t.Fatalf("%s is handed to a reader %v ahead with the deadline %v, the store's %v with %v "+
			"left, so on that reader's clock it is not lapsed and this case shows nothing",
			lease.Resource, ahead.skew, lease.ExpiresAt, stored.ExpiresAt, left)
	}
}

// timeLeft is coord.TimeLeft read through clock, failing the case on an error.
func (h *harness) timeLeft(clock coord.StoreClock, class coord.Class) func(coord.Lease) time.Duration {
	h.t.Helper()
	left, err := coord.TimeLeft(h.ctx, clock, class)
	if err != nil {
		h.t.Fatalf("TimeLeft(%s): unexpected error: %v", class, err)
	}
	return left
}
