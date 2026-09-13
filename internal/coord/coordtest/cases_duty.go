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
		baseline := h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
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

	{"list_live_finds_duties_under_every_prefix_that_can_name_one", func(h *harness) {
		// A backend that keeps duties apart from seats has to decide which
		// store a listing reads from the PREFIX alone, and both directions
		// can name a duty: a prefix shorter than "worker:" covers every
		// duty, and one longer than it covers some. Reading only an exact
		// "worker:" as a duty listing hides a live duty from a narrower
		// read, which is a dashboard showing a running duty as unheld.
		h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL, Ungated: true})
		h.claim(coord.WorkerResource("sandbox-waiter"), coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL, Ungated: true})
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "node-a:1", TTL: LongTTL})
		h.requireResources(`ListLive("w")`, h.listLive("w"), "worker:scheduler", "worker:sandbox-waiter")
		h.requireResources(`ListLive("worker:s")`, h.listLive("worker:s"), "worker:scheduler", "worker:sandbox-waiter")
		h.requireResources(`ListLive("worker:sch")`, h.listLive("worker:sch"), "worker:scheduler")
		h.requireResources(`ListLive("seat:")`, h.listLive(coord.SeatPrefix), "seat:ceo")
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
