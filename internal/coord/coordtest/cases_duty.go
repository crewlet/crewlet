package coordtest

import (
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// dutyCases certify the duty ceiling coord.MaxDutyTTL states: a `worker:`
// lease is honoured at any TTL up to it, whatever TTL the backend's seat
// leases run on, and refused beyond it.
//
// A group of its own because the rule is the one place a duty and a seat are
// held to different TTLs, and because every other case here claims a seat.
// That is how the original defect went unseen: the embedded KV refused every
// duty longer than its seat lease bucket's age, the twin honoured any TTL, and
// no case ever sent a duty a TTL longer than LongTTL, so the suite certified
// both backends while no fleet ran a single long duty.
//
// These cases are the one place the suite asks for more than LongTTL and
// DEPENDS on the answer, which the LongTTL doc allows for exactly this
// reason: the contract, not a backend, fixes this ceiling.
var dutyCases = []testCase{
	{"a_duty_is_honoured_at_the_duty_ceiling", func(h *harness) {
		// The regression the group exists for. The retention sweep asks for
		// 45 minutes and the skill curator for three hours, and a backend
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
		// error, because nobody holds the duty and (nil, nil) would say
		// somebody does.
		duty := coord.WorkerResource("maintenance")
		over := coord.MaxDutyTTL + time.Second
		lease, err := h.b.TryAcquire(h.ctx, duty, coord.AcquireOptions{
			Owner: "node-a:1", TTL: over, Ungated: true,
		})
		if !errors.Is(err, coord.ErrTTLTooLong) {
			h.t.Fatalf("TryAcquire(%q, ttl=%v) = (%v, %v), want an error wrapping coord.ErrTTLTooLong",
				duty, over, lease, err)
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

	{"a_short_duty_lapses_on_its_own_deadline", func(h *harness) {
		// A duty is judged by the deadline it asked for, not by whatever
		// longer horizon the backend keeps duty records for. A dead holder's
		// scheduler duty has to move within its 30 seconds, not within the
		// three hours the curator needs.
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
		// A backend that keeps duties in a store of its own decides which
		// stores a listing reads, and both directions of getting that
		// wrong are live incidents. Reading only the seat store for a duty
		// listing HIDES a running duty, which is a dashboard reporting it
		// unheld and a second node starting it. Answering a class with
		// whatever the STORE it was read out of holds, rather than with
		// the leases whose own leading segment is that class, hands the
		// seat listing this fleet's node presence back as a seat, which is
		// a seat nobody claimed counted into capacity.
		//
		// So every assertion below names the class it asked for and the
		// exact set it must get, on a fleet holding all three at once.
		//
		// The duties are claimed at coord.MaxDutyTTL rather than at
		// LongTTL deliberately. That is the TTL a backend cannot keep
		// beside its seats, so it is what forces the separate store into
		// existence at all; inside the seat ceiling a backend is free to
		// hold everything in one place and this case would certify a
		// routing decision nothing had to make.
		duties := coord.AcquireOptions{Owner: "node-a:1", TTL: coord.MaxDutyTTL, Ungated: true}
		h.claim(coord.WorkerResource("scheduler"), duties)
		h.claim(coord.WorkerResource("sandbox-waiter"), duties)
		h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		h.claim(coord.NodeResource("node-a"), coord.AcquireOptions{
			Owner: "node-a:1", TTL: LongTTL, Ungated: true,
		})

		// EVERY duty, and no seat. Both halves matter: a listing that
		// reached the duty store but stopped at the first record would
		// pass a single-duty case and still leave the second duty unrun.
		h.requireResources("live duties", h.listLive(coord.ClassWorker),
			"worker:scheduler", "worker:sandbox-waiter")
		h.requireResources("live seats", h.listLive(coord.ClassSeat), "seat:ceo")
		// The membership read, on a fleet where duties outnumber both the
		// seats and the nodes. It is what a node divides the seats by, so
		// a duty counted into it is every node believing the fleet is
		// larger than it is and leaving seats dark.
		h.requireResources("live nodes", h.listLive(coord.ClassNode), "node:node-a")

		// ListOwned narrows by nothing at all, because the owner is in the
		// record rather than in the key, so it is the one read that has to
		// visit every store a backend keeps leases in. A drain watches it
		// converge to empty: a duty it never listed is a node that reports
		// itself drained while its duty is still running.
		h.requireResources("everything node-a:1 holds", h.listOwned("node-a:1"),
			"node:node-a", "seat:ceo", "worker:scheduler", "worker:sandbox-waiter")
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
