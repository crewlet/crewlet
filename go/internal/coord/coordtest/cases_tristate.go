package coordtest

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// tristateCases certify that the three answers stay three answers — and that
// an argument a caller got wrong is never dressed up as one of them.
var tristateCases = []testCase{
	{"an_unreachable_store_is_an_error_never_a_refusal", func(h *harness) {
		// Both answers mean "you are not the owner", so both fail
		// closed. Only one of them means SOMEBODY ELSE IS, and a caller
		// that cannot tell them apart reports the wrong thing and takes
		// the wrong recovery.
		f := NewFaulty(h.b)
		f.Break(nil)

		lease, err := f.TryAcquire(context.Background(), "seat:ceo",
			coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if lease != nil {
			h.t.Fatal("TryAcquire granted a lease from an unreachable store")
		}
		if err == nil {
			h.t.Fatal("TryAcquire answered (nil, nil) — an unreachable store reads as a peer " +
				"holding the resource, which is the conflation the tri-state exists to prevent")
		}
		if !errors.Is(err, coord.ErrUnavailable) {
			h.t.Fatalf("TryAcquire error %v does not wrap coord.ErrUnavailable", err)
		}
	}},

	{"a_real_refusal_is_never_an_error", func(h *harness) {
		// The other half. A peer genuinely holding the resource is a
		// definite, actionable answer, and turning it into an error
		// would make a node quiesce over a race it simply lost.
		f := NewFaulty(h.b)
		if _, err := f.TryAcquire(context.Background(), "seat:ceo",
			coord.AcquireOptions{Owner: "node-a", TTL: LongTTL}); err != nil {
			h.t.Fatalf("healthy claim errored: %v", err)
		}
		lease, err := f.TryAcquire(context.Background(), "seat:ceo",
			coord.AcquireOptions{Owner: "node-b", TTL: LongTTL})
		if err != nil {
			h.t.Fatalf("a peer holding the resource reported an error: %v", err)
		}
		if lease != nil {
			h.t.Fatal("second owner took a live lease")
		}
	}},

	{"renew_reports_unknown_as_an_error_and_the_lease_survives_it", func(h *harness) {
		// False from renew instructs the caller to shed everything the
		// lease covered. A blip changed nothing about ownership — the
		// record is untouched and still held — so answering false would
		// tear a healthy node's seats down for a whole TTL during which
		// no peer could claim them either. The heartbeat's job is to
		// retry; it has until the deadline to succeed.
		ctx := context.Background()
		f := NewFaulty(h.b)
		lease, err := f.TryAcquire(ctx, "seat:ceo",
			coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		if err != nil || lease == nil {
			h.t.Fatalf("setup claim failed: %v %v", lease, err)
		}

		f.Break(nil)
		ok, err := f.Renew(ctx, "seat:ceo", "node-a", lease.Epoch, LongTTL)
		if ok {
			h.t.Fatal("Renew reported success from an unreachable store")
		}
		if err == nil {
			h.t.Fatal("Renew answered (false, nil) — the caller would shed every seat it holds " +
				"over a store blip that changed nothing")
		}

		f.Heal()
		if ok, err := f.Renew(ctx, "seat:ceo", "node-a", lease.Epoch, LongTTL); !ok || err != nil {
			h.t.Fatalf("Renew after the outage = (%v, %v), want the lease still held", ok, err)
		}
	}},

	{"a_genuinely_lost_lease_reports_false_with_no_error", func(h *harness) {
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: ShortTTL})
		h.lapse()
		ok, err := h.b.Renew(h.ctx, "seat:ceo", "node-a", lease.Epoch, LongTTL)
		if err != nil {
			h.t.Fatalf("a definitively lapsed lease reported an error: %v", err)
		}
		if ok {
			h.t.Fatal("renew of a lapsed lease reported success")
		}
	}},

	{"every_read_reports_unknown_as_an_error", func(h *harness) {
		// An empty answer from any of these is load-bearing: "this owner
		// holds nothing" drives a takeover pass, "nobody is live under
		// this prefix" drives the capacity split, and "no live lease at
		// all" tells a stalled node the gate is not what is blocking it.
		// None of those may be what an unreachable store looks like.
		ctx := context.Background()
		f := NewFaulty(h.b)
		f.Break(nil)

		if _, err := f.Get(ctx, "seat:ceo"); err == nil {
			h.t.Fatal("Get answered (nil, nil) from an unreachable store — callers trust " +
				"nil to mean no such lease")
		}
		if _, err := f.Release(ctx, "seat:ceo", "node-a", 1); err == nil {
			h.t.Fatal("Release answered (false, nil) from an unreachable store")
		}
		if leases, err := f.ListOwned(ctx, "node-a"); err == nil {
			h.t.Fatalf("ListOwned answered %v with no error from an unreachable store", leases)
		}
		if leases, err := f.ListLive(ctx, coord.NodePrefix); err == nil {
			h.t.Fatalf("ListLive answered %v with no error — a capacity split would divide "+
				"the seats by a fleet it could not see", leases)
		}
		if hints, err := f.PreferredResources(ctx, coord.SeatPrefix, "node-a"); err == nil {
			h.t.Fatalf("PreferredResources answered %v with no error", hints)
		}
	}},

	{"an_unknown_protocol_floor_does_not_read_as_an_empty_fleet", func(h *harness) {
		// (0, false, nil) says "nothing older is out there", which is
		// the one thing an unreachable store must never say during a
		// rolling upgrade.
		f := NewFaulty(h.b)
		f.Break(nil)
		floor, any, err := f.FleetProtocolFloor(context.Background())
		if err == nil {
			h.t.Fatalf("FleetProtocolFloor = (%d, %v, nil) from an unreachable store", floor, any)
		}
		if any {
			h.t.Fatalf("FleetProtocolFloor reported a floor of %d alongside an error", floor)
		}
	}},

	// --- arguments a caller got wrong ----------------------------------

	{"blank_arguments_are_errors_not_refusals", func(h *harness) {
		// (nil, nil) means "somebody else holds this". A blank owner has
		// not lost a race to anybody, so answering it that way sends the
		// caller hunting for a peer that does not exist. An error is the
		// honest shape — the third answer is "the store did not give you
		// an answer", and a caller retrying a programming error retries
		// it loudly and forever instead of proceeding on a lie.
		ctx := context.Background()
		bad := []struct {
			name     string
			resource string
			opts     coord.AcquireOptions
		}{
			{"blank resource", "", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL}},
			{"blank owner", "seat:ceo", coord.AcquireOptions{Owner: "", TTL: LongTTL}},
			{"zero TTL", "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: 0}},
			{"negative TTL", "seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: -time.Second}},
		}
		for _, c := range bad {
			lease, err := h.b.TryAcquire(ctx, c.resource, c.opts)
			if err == nil {
				h.t.Fatalf("TryAcquire with a %s answered (%v, nil)", c.name, lease)
			}
			if lease != nil {
				h.t.Fatalf("TryAcquire with a %s granted a lease", c.name)
			}
		}

		if ok, err := h.b.Renew(ctx, "seat:ceo", "", 1, LongTTL); err == nil || ok {
			h.t.Fatalf("Renew with a blank owner = (%v, %v), want an error", ok, err)
		}
		if ok, err := h.b.Renew(ctx, "seat:ceo", "node-a", 1, 0); err == nil || ok {
			h.t.Fatalf("Renew with a zero TTL = (%v, %v), want an error", ok, err)
		}
	}},

	{"a_blank_owner_cannot_release_a_released_record", func(h *harness) {
		// Release expires the record in place, and a store that blanks
		// the owner field while doing so leaves a record whose owner is
		// the empty string. Validating the argument is what stops a
		// caller with an unset owner from matching it and releasing —
		// or worse, from being told it succeeded.
		lease := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a", TTL: LongTTL})
		h.release("seat:ceo", "node-a", lease.Epoch)

		ok, err := h.b.Release(context.Background(), "seat:ceo", "", lease.Epoch)
		if err == nil {
			h.t.Fatalf("Release with a blank owner = (%v, nil), want an error", ok)
		}
		if ok {
			h.t.Fatal("Release with a blank owner reported success against a released record")
		}
	}},
}
