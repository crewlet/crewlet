package coordtest

import (
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// dutyCases certify the ceiling coord.MaxDutyTTL states: a `worker:` lease,
// and every other lease outside seats and presence, is honoured at any TTL up
// to it, whatever TTL the backend's seat leases run on, and refused beyond it.
//
// A group of its own because the rule is the one place a lease and a seat are
// held to different TTLs, and every other case here claims a seat. A backend
// that refused a duty longer than its seat lease bucket's age went unseen
// while no case sent a TTL beyond LongTTL, and again when the cases sent the
// long TTL to duties alone, so a caller class (the tracker's walk claims) was
// still refused. These cases are the one place the suite asks for more than
// LongTTL and DEPENDS on the answer, which the LongTTL doc allows for exactly
// this reason: the contract, not a backend, fixes this ceiling.
var dutyCases = []testCase{
	{"a_duty_is_honoured_at_the_duty_ceiling", func(h *harness) {
		// The regression the group exists for. The retention sweep asks for
		// 45 minutes and the learning passes for three hours, and a backend
		// that refused either left that duty unrun on every fleet it
		// served.
		duty := coord.WorkerResource("maintenance")
		baseline := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		lease := h.claim(duty, coord.AcquireOptions{
			Owner: "node-a:1", TTL: coord.MaxDutyTTL, Ungated: true,
		})
		// An interval, not an ordering: a store clamping the duty to its
		// seat ceiling still stamps the second claim later, purely because
		// it happened second.
		if gap := lease.ExpiresAt.Sub(baseline.ExpiresAt); gap < coord.MaxDutyTTL-LongTTL-time.Minute {
			h.t.Fatalf("a %v duty landed only %v beyond a %v seat lease: the store clamped the "+
				"duty to a shorter ceiling instead of honouring coord.MaxDutyTTL",
				coord.MaxDutyTTL, gap, LongTTL)
		}
		if !h.renew(duty, "node-a:1", lease.Epoch, coord.MaxDutyTTL) {
			h.t.Fatal("renewing a live duty at the duty ceiling reported loss")
		}
		again := h.claim(duty, coord.AcquireOptions{
			Owner: "node-a:1", TTL: coord.MaxDutyTTL, Ungated: true,
		})
		if again.Epoch != lease.Epoch {
			h.t.Fatalf("a per-tick re-claim at the duty ceiling moved the epoch %d -> %d",
				lease.Epoch, again.Epoch)
		}
		h.mustHold(duty, "node-a:1")
	}},

	{"a_duty_beyond_the_ceiling_is_an_error_never_a_refusal_or_a_clamp", func(h *harness) {
		// Refused on EVERY backend, including one that could keep the
		// deadline: a twin that accepted what the embedded KV refuses is
		// exactly how the long duties passed every single-node test. An
		// error, because nobody holds the duty and a refusal would say
		// somebody does.
		duty := coord.WorkerResource("maintenance")
		over := coord.MaxDutyTTL + time.Second
		lease, refused, err := h.b.TryAcquire(h.ctx, duty, coord.AcquireOptions{
			Owner: "node-a:1", TTL: over, Ungated: true,
		})
		if !errors.Is(err, coord.ErrTTLTooLong) {
			h.t.Fatalf("TryAcquire(%q, ttl=%v) = (%v, %q, %v), want an error wrapping coord.ErrTTLTooLong",
				duty, over, lease, refused, err)
		}
		if lease != nil {
			h.t.Fatalf("TryAcquire(%q, ttl=%v) granted a lease beside its error", duty, over)
		}
		h.mustBeUnheld(duty)

		held := h.claim(duty, coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL, Ungated: true})
		ok, err := h.b.Renew(h.ctx, duty, "node-a:1", held.Epoch, over)
		if !errors.Is(err, coord.ErrTTLTooLong) || ok {
			h.t.Fatalf("Renew(%q, ttl=%v) = (%v, %v), want (false, an error wrapping coord.ErrTTLTooLong)",
				duty, over, ok, err)
		}
		h.requireUnchanged("a renew refused for its TTL", held, h.get(duty))
	}},

	{"a_caller_class_lease_is_honoured_beyond_the_seat_ceiling_up_to_the_contract_ceiling", func(h *harness) {
		// The same ceiling for a class this package does not own. A caller's
		// claim (the tracker's walk claims, `move:`, `merge:`) is sized from
		// the work it guards, never from the seat heartbeat; a backend holding
		// it to its seat lease bucket's age refuses every cross-project move
		// and merge at the shipped 45-second seat TTL. Claimed GATED, as a
		// caller's claim is: a duty is ungated, and claiming only the ungated
		// way would certify the ceiling on half the paths a claim takes.
		claim := callerClass.Resource("node-a")
		baseline := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		lease := h.claim(claim, coord.AcquireOptions{Owner: "node-a:1", TTL: coord.MaxDutyTTL})
		// An interval, not an ordering, for the reason the duty case
		// gives: a store clamping the claim still stamps it later.
		if gap := lease.ExpiresAt.Sub(baseline.ExpiresAt); gap < coord.MaxDutyTTL-LongTTL-time.Minute {
			h.t.Fatalf("a %v %s claim landed only %v beyond a %v seat lease: the store clamped "+
				"it to a shorter ceiling instead of honouring coord.MaxDutyTTL",
				coord.MaxDutyTTL, callerClass, gap, LongTTL)
		}
		if !h.renew(claim, "node-a:1", lease.Epoch, coord.MaxDutyTTL) {
			h.t.Fatalf("renewing a live %s claim at the contract ceiling reported loss", callerClass)
		}
		held := h.mustHold(claim, "node-a:1")

		// AND BEYOND IT, AN ERROR ON EVERY BACKEND: the twin could keep
		// any deadline, and accepting one the embedded KV refuses is
		// exactly how this went unseen. An error rather than a refusal,
		// because nobody holds the resource.
		over := coord.MaxDutyTTL + time.Second
		fresh := callerClass.Resource("node-b")
		got, refused, err := h.b.TryAcquire(h.ctx, fresh, coord.AcquireOptions{Owner: "node-a:1", TTL: over})
		if !errors.Is(err, coord.ErrTTLTooLong) {
			h.t.Fatalf("TryAcquire(%q, ttl=%v) = (%v, %q, %v), want an error wrapping coord.ErrTTLTooLong",
				fresh, over, got, refused, err)
		}
		if got != nil {
			h.t.Fatalf("TryAcquire(%q, ttl=%v) granted a lease beside its error", fresh, over)
		}
		h.mustBeUnheld(fresh)
		ok, err := h.b.Renew(h.ctx, claim, "node-a:1", held.Epoch, over)
		if !errors.Is(err, coord.ErrTTLTooLong) || ok {
			h.t.Fatalf("Renew(%q, ttl=%v) = (%v, %v), want (false, an error wrapping coord.ErrTTLTooLong)",
				claim, over, ok, err)
		}
		h.requireUnchanged("a renew refused for its TTL", held, h.get(claim))
	}},

	{"a_short_duty_lapses_on_its_own_deadline", func(h *harness) {
		// A duty is judged by the deadline it asked for, not by whatever
		// longer horizon the backend keeps duty records for. A dead holder's
		// scheduler duty has to move within its 30 seconds, not within the
		// three hours the learning passes need.
		duty := coord.WorkerResource("scheduler")
		first := h.claim(duty, coord.AcquireOptions{Owner: "node-a:1", TTL: ShortTTL, Ungated: true})
		h.lapse()
		h.mustBeUnheld(duty)
		if owned := h.listOwned("node-a:1"); len(owned) != 0 {
			h.t.Fatalf("ListOwned still reports %v after the duty lapsed", resources(owned))
		}
		taken := h.claim(duty, coord.AcquireOptions{Owner: "node-b:1", TTL: LongTTL, Ungated: true})
		if taken.Epoch <= first.Epoch {
			h.t.Fatalf("a duty taken over after a lapse kept epoch %d (previous %d)",
				taken.Epoch, first.Epoch)
		}
	}},

	{"every_listing_reaches_duties_wherever_a_backend_keeps_them", func(h *harness) {
		// A backend keeping duties in a store of its own decides which stores
		// a listing reads, and both wrong directions are live incidents.
		// Reading only the seat store HIDES a running duty (a second node
		// starts it). Answering a class with whatever the STORE holds, not the
		// leases whose leading segment is that class, hands node presence back
		// as a seat nobody claimed. So every assertion names its class and the
		// exact set it must get, on a fleet holding all four at once.
		//
		// Duties and the caller's claim are taken at coord.MaxDutyTTL, not
		// LongTTL: that TTL cannot live beside seats, so it forces the separate
		// store into existence; inside the seat ceiling this would certify a
		// routing decision nothing had to make.
		duties := coord.AcquireOptions{Owner: "node-a:1", TTL: coord.MaxDutyTTL, Ungated: true}
		h.claim(coord.WorkerResource("scheduler"), duties)
		h.claim(coord.WorkerResource("launch-node-watch"), duties)
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		h.claim(coord.NodeResource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Ungated: true,
		})
		h.claim(callerClass.Resource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: coord.MaxDutyTTL,
		})

		// EVERY duty, and no seat. Both halves matter: a listing that
		// reached the duty store but stopped at the first record would
		// pass a single-duty case and still leave the second duty unrun.
		h.requireResources("live duties", h.listLive(coord.ClassWorker),
			"worker:scheduler", "worker:launch-node-watch")
		h.requireResources("live seats", h.listLive(coord.ClassSeat), "seat:ceo")
		// The membership read, on a fleet where duties outnumber both the
		// seats and the nodes. It is what a node divides the seats by, so
		// a duty counted into it is every node believing the fleet is
		// larger than it is and leaving seats dark.
		h.requireResources("live nodes", h.listLive(coord.ClassNode), "node:node-a")
		// And a class a caller owns, read the same way: a duty or a
		// presence lease counted into it is a claim nobody made.
		h.requireResources("live claims", h.listLive(callerClass), "claim:node-a")

		// ListOwned narrows by nothing at all, because the owner is in the
		// record rather than in the key, so it is the one read that has to
		// visit every store a backend keeps leases in. A drain watches it
		// converge to empty: a duty it never listed is a node that reports
		// itself drained while its duty is still running.
		h.requireResources("everything node-a:1 holds", h.listOwned("node-a:1"),
			"claim:node-a", "node:node-a", "seat:ceo", "worker:scheduler",
			"worker:launch-node-watch")
	}},

	{"a_released_duty_is_free_at_once_and_its_epoch_keeps_climbing", func(h *harness) {
		// The duty's counter is the same monotonic record a seat's is,
		// wherever the backend keeps the duty's lease. A hold released at
		// the end of a provisioning pass must hand the next caller a newer
		// token, never a restarted one.
		duty := coord.WorkerResource("setup-provision-github")
		first := h.claim(duty, coord.AcquireOptions{Owner: "node-a:1", TTL: coord.MaxDutyTTL, Ungated: true})
		if !h.release(duty, "node-a:1", first.Epoch) {
			h.t.Fatal("releasing a held duty reported failure")
		}
		h.mustBeUnheld(duty)
		taken := h.claim(duty, coord.AcquireOptions{Owner: "node-b:1", TTL: coord.MaxDutyTTL, Ungated: true})
		if taken.Epoch <= first.Epoch {
			h.t.Fatalf("epoch %d after releasing a duty at epoch %d: the counter reset",
				taken.Epoch, first.Epoch)
		}
	}},
}
