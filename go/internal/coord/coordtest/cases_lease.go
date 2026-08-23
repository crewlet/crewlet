package coordtest

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// leaseCases certify the three rules the package doc opens with: the epoch is
// monotonic for the resource's LIFETIME, a lapsed lease is re-acquired rather
// than renewed, and owner names a process incarnation.
var leaseCases = []testCase{
	{"acquire_unclaimed_starts_at_epoch_one", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if lease.Epoch != 1 {
			h.t.Fatalf("first claim minted epoch %d, want 1", lease.Epoch)
		}
	}},

	{"epochs_are_per_resource", func(h *harness) {
		// The counter fences writes on ONE resource. Sharing it across
		// resources would make every seat claim in the fleet advance
		// every other seat's token, so a node that renewed nothing
		// would still see its own epoch age out from under it.
		ceo := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		cto := h.claim("seat:cto", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if ceo.Epoch != 1 || cto.Epoch != 1 {
			h.t.Fatalf("epochs %d/%d, want each resource to start its own counter at 1",
				ceo.Epoch, cto.Epoch)
		}
	}},

	{"a_second_owner_cannot_take_a_live_lease", func(h *harness) {
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.refused("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
	}},

	{"two_incarnations_under_one_node_id_do_not_both_win", func(h *harness) {
		// Owner is a process INCARNATION, not a machine. Two processes
		// sharing an owner string — which a default node id would do
		// across two engines on one host — would both hold the resource
		// at the same epoch, and the fencing token would then identify
		// nothing.
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-0:aaaa", TTL: LongTTL})
		h.refused("seat:ceo", coord.AcquireOptions{Owner: "node-0:bbbb", TTL: LongTTL})
	}},

	{"an_unbroken_same_owner_reacquire_keeps_the_epoch", func(h *harness) {
		// A live holder re-acquiring is a renewal: nothing was ever
		// unowned, so its in-flight work stayed covered and the fencing
		// token must not move under it.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		again := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if again.Epoch != first.Epoch {
			h.t.Fatalf("unbroken re-acquire moved the epoch %d -> %d", first.Epoch, again.Epoch)
		}
	}},

	{"an_unbroken_same_owner_reacquire_extends_the_deadline", func(h *harness) {
		// Re-acquire doubles as renew, which is what lets a heartbeat
		// use one call for "still mine, still wanted".
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		again := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if again.Epoch != first.Epoch {
			h.t.Fatalf("unbroken re-acquire moved the epoch %d -> %d", first.Epoch, again.Epoch)
		}
		h.lapse()
		h.mustHold("seat:ceo", "node-a")
	}},

	{"takeover_after_a_lapse_bumps_the_epoch", func(h *harness) {
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		taken := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if taken.Epoch <= first.Epoch {
			h.t.Fatalf("takeover kept epoch %d (previous %d) — the previous owner's "+
				"in-flight writes would still pass the new owner's fence",
				taken.Epoch, first.Epoch)
		}
	}},

	{"the_same_owner_reacquiring_after_a_lapse_also_bumps_the_epoch", func(h *harness) {
		// The subtle one, and the reason rule 2 says "even for the same
		// owner". During the gap this owner's in-flight work was covered
		// by nothing, so it has to be fenced against its own past self —
		// otherwise a zombie from before the gap still matches the
		// current epoch and writes straight through.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		again := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if again.Epoch <= first.Epoch {
			h.t.Fatalf("re-acquire after a lapse kept epoch %d — the gap is unfenced", again.Epoch)
		}
	}},

	// --- renew ---------------------------------------------------------

	{"renew_extends_a_live_lease", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		if !h.renew("seat:ceo", "node-a", lease.Epoch, LongTTL) {
			h.t.Fatal("renew of a live lease reported loss")
		}
		// The renewed TTL has to reach the store's own clock, not just
		// the returned struct: this is the call a heartbeat survives on.
		h.lapse()
		h.mustHold("seat:ceo", "node-a")
	}},

	{"renew_rejects_a_lapsed_lease", func(h *harness) {
		// A lapsed lease cannot be renewed, only re-acquired — and
		// re-acquiring mints a new epoch. Renewing across a gap would
		// silently re-cover work that ran unprotected.
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		if h.renew("seat:ceo", "node-a", lease.Epoch, LongTTL) {
			h.t.Fatal("renew resurrected a lapsed lease")
		}
	}},

	{"renew_rejects_a_stale_epoch", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if h.renew("seat:ceo", "node-a", lease.Epoch, LongTTL) {
			h.t.Fatal("renew accepted the previous tenure's epoch")
		}
	}},

	{"renew_rejects_the_owners_own_stale_epoch", func(h *harness) {
		// The zombie, exactly: a heartbeat left over from before the gap
		// still carries the previous tenure's token, and the owner
		// string alone cannot tell it apart from the live one. Only the
		// epoch can, which is why the predicate is owner AND epoch.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		current := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if h.renew("seat:ceo", "node-a", first.Epoch, LongTTL) {
			h.t.Fatal("renew accepted this owner's own superseded epoch")
		}
		if !h.renew("seat:ceo", "node-a", current.Epoch, LongTTL) {
			h.t.Fatal("renew rejected the current tenure's epoch")
		}
	}},

	{"renew_rejects_another_owner", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if h.renew("seat:ceo", "node-b", lease.Epoch, LongTTL) {
			h.t.Fatal("renew accepted an owner that never held the lease")
		}
	}},

	{"a_rejected_renew_leaves_the_lease_untouched", func(h *harness) {
		// Answering "no" is only half of a rejection. A backend that
		// writes the fresh deadline and THEN checks the predicate
		// answers correctly and still extends the lease it just
		// refused — and the caller doing the refusing is a zombie from
		// the previous tenure, heartbeating on a stale epoch because
		// nothing has told it to stop. Its renews would then keep the
		// CURRENT owner's lease alive indefinitely, so when that owner
		// dies its seats never lapse and no peer can ever claim them.
		// The seat goes dark permanently, and every sweep reads healthy.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		before := h.mustHold("seat:ceo", "node-a")

		if h.renew("seat:ceo", "node-a", first.Epoch, LongTTL) {
			h.t.Fatal("renew accepted the previous tenure's epoch")
		}
		h.requireUnchanged("a renew rejected on a stale epoch", before, h.mustHold("seat:ceo", "node-a"))

		if h.renew("seat:ceo", "node-b", before.Epoch, LongTTL) {
			h.t.Fatal("renew accepted an owner that never held the lease")
		}
		h.requireUnchanged("a renew rejected on the owner", before, h.mustHold("seat:ceo", "node-a"))
	}},

	{"a_refused_claim_leaves_the_holders_lease_untouched", func(h *harness) {
		// The same half-rejection, on the path a fleet walks constantly:
		// every node sweeps for claimable seats on every tick, so in a
		// healthy company a REFUSED claim is the single most common
		// thing this store does. A refusal that writes anything is
		// therefore not a rare corruption but a continuous one.
		//
		// Three fields, three different failures. The deadline: peers
		// refusing each other keep a dead holder's seats alive forever.
		// The hint: it comes to name the last node that TRIED rather
		// than the last that HELD, so a restarted node no longer finds
		// the seats whose children it had warm. The meta: two processes
		// racing one node id would let the loser's profile overwrite the
		// winner's, and peers then make placement decisions against
		// roles and labels no running process has.
		resource := coord.NodeResource("n1")
		held := map[string]any{"roles": []any{"seats"}, "labels": map[string]any{"zone": "eu"}}
		h.claim(resource, coord.AcquireOptions{
			Owner: "n1:winner", TTL: LongTTL, Preferred: "n1", Ungated: true, Meta: held,
		})
		before := h.mustHold(resource, "n1:winner")

		h.refused(resource, coord.AcquireOptions{
			Owner:     "n1:loser",
			TTL:       LongTTL / 2,
			Preferred: "n2",
			Ungated:   true,
			Meta:      map[string]any{"roles": []any{"workers"}},
		})
		h.requireUnchanged("a claim refused by a live holder", before, h.mustHold(resource, "n1:winner"))
	}},

	{"renew_of_an_unclaimed_resource_reports_loss", func(h *harness) {
		if h.renew("seat:ceo", "node-a", 1, LongTTL) {
			h.t.Fatal("renew invented a lease for a resource nobody ever claimed")
		}
	}},

	// --- release -------------------------------------------------------

	{"release_is_owner_and_epoch_predicated", func(h *harness) {
		// The defect this primitive generalises away: an unqualified
		// release lets a departing owner clear its SUCCESSOR's live
		// lease.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})

		if h.release("seat:ceo", "node-a", first.Epoch) {
			h.t.Fatal("a departing owner released its successor's lease")
		}
		h.mustHold("seat:ceo", "node-b")
	}},

	{"release_rejects_the_owners_own_stale_epoch", func(h *harness) {
		// The same zombie on the way out, and the more damaging half: a
		// straggler from the previous tenure cleaning up would hand the
		// resource away while the CURRENT tenure is mid-turn. Matching
		// on the owner alone cannot see the difference — the owner
		// string is identical.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		current := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if h.release("seat:ceo", "node-a", first.Epoch) {
			h.t.Fatal("release accepted this owner's own superseded epoch")
		}
		held := h.mustHold("seat:ceo", "node-a")
		if held.Epoch != current.Epoch {
			h.t.Fatalf("the live tenure is at epoch %d after a stale release, want %d",
				held.Epoch, current.Epoch)
		}
	}},

	{"release_frees_the_resource_for_immediate_takeover", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if !h.release("seat:ceo", "node-a", lease.Epoch) {
			h.t.Fatal("release of a held lease reported failure")
		}
		if owned := h.listOwned("node-a"); len(owned) != 0 {
			h.t.Fatalf("ListOwned still reports %v after release", resources(owned))
		}
		taken := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if taken.Owner != "node-b" {
			h.t.Fatalf("takeover after release landed on %q", taken.Owner)
		}
	}},

	{"release_keeps_the_epoch_monotonic", func(h *harness) {
		// Release must EXPIRE the record, never delete it. A deleted
		// record restarts the counter at 1 — the exact token a zombie
		// from the released tenure is still stamping its writes with, so
		// it would sail through the next owner's fence.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.release("seat:ceo", "node-a", first.Epoch)
		taken := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if taken.Epoch <= first.Epoch {
			h.t.Fatalf("epoch %d after release of epoch %d — the counter reset with the record",
				taken.Epoch, first.Epoch)
		}
	}},

	{"release_then_reacquire_by_the_same_owner_also_advances", func(h *harness) {
		// The same hazard without a second node: a process that
		// releases, restarts and re-claims must not inherit its
		// predecessor's token.
		first := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.release("seat:ceo", "node-a", first.Epoch)
		again := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if again.Epoch <= first.Epoch {
			h.t.Fatalf("re-acquire after release kept epoch %d", again.Epoch)
		}
	}},

	{"the_epoch_never_rewinds_across_many_tenures", func(h *harness) {
		// Monotonic for the LIFETIME of the resource, not of a record:
		// releases, lapses and takeovers all advance it and nothing
		// walks it back.
		var last int64
		owners := []string{"node-a", "node-b", "node-a", "node-c", "node-c"}
		for i, owner := range owners {
			lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: owner, TTL: LongTTL})
			if lease.Epoch <= last {
				h.t.Fatalf("tenure %d (owner %q) got epoch %d, not greater than %d",
					i, owner, lease.Epoch, last)
			}
			last = lease.Epoch
			h.release("seat:ceo", owner, lease.Epoch)
		}
	}},

	{"a_released_lease_cannot_be_renewed", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.release("seat:ceo", "node-a", lease.Epoch)
		if h.renew("seat:ceo", "node-a", lease.Epoch, LongTTL) {
			h.t.Fatal("renew resurrected a released lease — release must expire it, " +
				"and an expired lease is re-acquired, never renewed")
		}
	}},

	{"releasing_twice_reports_the_second_as_not_held", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if !h.release("seat:ceo", "node-a", lease.Epoch) {
			h.t.Fatal("first release reported failure")
		}
		if h.release("seat:ceo", "node-a", lease.Epoch) {
			h.t.Fatal("second release reported success — a caller retrying a release " +
				"would read that as still owning the tenure")
		}
	}},

	{"release_of_an_unclaimed_resource_reports_not_held", func(h *harness) {
		if h.release("seat:ceo", "node-a", 1) {
			h.t.Fatal("release succeeded against a resource nobody ever claimed")
		}
	}},

	// --- the deadline --------------------------------------------------

	{"expires_at_is_a_utc_deadline_on_the_stores_clock", func(h *harness) {
		// A heartbeat computes its next tick from this field, and a
		// dashboard renders it. A backend that answered in local time,
		// or with a zero value, passes every ownership test and then
		// puts a lease an hour in the past on an operator's screen.
		short := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		long := h.claim("seat:cto", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		for _, l := range []*coord.Lease{short, long} {
			if l.ExpiresAt.IsZero() {
				h.t.Fatalf("%s: zero ExpiresAt", l.Resource)
			}
			// Both halves matter. A non-zero offset is the semantic
			// failure — a deadline in local time is a different instant.
			// The location check is what keeps a UTC-hosted CI from
			// certifying a backend that answers in the host's zone: it
			// looks identical here and moves an hour the first time a
			// node runs somewhere else.
			if _, offset := l.ExpiresAt.Zone(); offset != 0 {
				h.t.Fatalf("%s: ExpiresAt %v is not UTC (offset %ds)", l.Resource, l.ExpiresAt, offset)
			}
			if l.ExpiresAt.Location() != time.UTC {
				h.t.Fatalf("%s: ExpiresAt is in %v, want time.UTC — the contract's deadline is "+
					"the store's, rendered the same way by every backend",
					l.Resource, l.ExpiresAt.Location())
			}
		}
		if !long.ExpiresAt.After(short.ExpiresAt) {
			h.t.Fatalf("a %v TTL did not outlast a %v one: %v vs %v",
				LongTTL, ShortTTL, long.ExpiresAt, short.ExpiresAt)
		}
		if got := long.ExpiresAt.Sub(short.ExpiresAt); got < LongTTL-ShortTTL-time.Minute {
			h.t.Fatalf("deadlines %v apart for TTLs %v apart — the TTL is not reaching the record",
				got, LongTTL-ShortTTL)
		}
	}},
}
