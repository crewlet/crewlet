package coordtest

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// watchBound is how long a case waits for a write a watch owes it.
//
// A DELIVERY, NOT A POLL: what a case waits for is one message the store has
// already applied reaching a consumer the broker created before the write, so
// the bound is a stall detector rather than a latency the contract promises.
// Ten seconds is far past a delivery on the slowest topology the suite runs —
// a leaf, where the write and the delivery each cross the link — and far
// inside stallBudget, so a watch that never speaks fails as this case.
const watchBound = 10 * time.Second

// watchCases certify [coord.Backend.WatchLive]: that a write to a lease
// reaches a watch of its class as the lease it left, and that a watch which
// cannot be started says so.
//
// A WATCH IS A HINT, and the cases hold it to what the hint is for — that a
// claim, a renewal, a takeover and a release are each heard, in order, by
// the class that was watched and by no other. They do not hold it to what the
// contract leaves out: that a lapse is delivered (it is no write), or that a
// watch says anything about what was held before it began.
var watchCases = []testCase{
	{"a_claim_after_a_watch_began_is_heard_with_its_holder", func(h *harness) {
		writes := h.watch(coord.ClassNode)
		lease := h.claim(coord.NodeResource("a"), coord.AcquireOptions{
			Owner: "node-a/1", TTL: LongTTL, Ungated: true,
		})
		h.requireWrite(writes, "the claim", coord.LeaseWrite{
			Resource: lease.Resource, Owner: "node-a/1", Epoch: lease.Epoch,
		})
	}},

	{"a_renewal_and_a_release_are_heard_in_order", func(h *harness) {
		// THE ORDER IS THE POINT for a consumer that compares each write
		// with what it holds: a release heard before the renewal it
		// followed would read as a node that came back.
		writes := h.watch(coord.ClassNode)
		resource := coord.NodeResource("a")
		lease := h.claim(resource, coord.AcquireOptions{Owner: "node-a/1", TTL: LongTTL, Ungated: true})
		held := coord.LeaseWrite{Resource: resource, Owner: "node-a/1", Epoch: lease.Epoch}
		h.requireWrite(writes, "the claim", held)

		if !h.renew(resource, "node-a/1", lease.Epoch, LongTTL) {
			h.t.Fatal("the holder's renewal was refused")
		}
		h.requireWrite(writes, "the renewal, which keeps its epoch", held)
		// A LIVE HOLDER'S RE-CLAIM IS A RENEWAL TOO — the presence
		// heartbeat's own path — and keeps the epoch as Renew does.
		h.claim(resource, coord.AcquireOptions{Owner: "node-a/1", TTL: LongTTL, Ungated: true})
		h.requireWrite(writes, "the holder's re-claim", held)

		if !h.release(resource, "node-a/1", lease.Epoch) {
			h.t.Fatal("the holder's release was refused")
		}
		got := h.nextWrite(writes, "the release")
		if got.Resource != resource || got.Owner != "" {
			h.t.Fatalf("the release was heard as %+v, want %s held by nobody", got, resource)
		}
	}},

	{"a_takeover_is_heard_with_its_new_holder_and_epoch", func(h *harness) {
		writes := h.watch(coord.ClassNode)
		resource := coord.NodeResource("a")
		first := h.claim(resource, coord.AcquireOptions{Owner: "node-a/1", TTL: ShortTTL, Ungated: true})
		h.requireWrite(writes, "the first claim", coord.LeaseWrite{
			Resource: resource, Owner: "node-a/1", Epoch: first.Epoch,
		})
		h.lapse()
		second := h.claim(resource, coord.AcquireOptions{Owner: "node-a/2", TTL: LongTTL, Ungated: true})
		if second.Epoch <= first.Epoch {
			h.t.Fatalf("the takeover's epoch %d is not past the lapsed tenure's %d",
				second.Epoch, first.Epoch)
		}
		// A LAPSE IS NO WRITE and may or may not be heard; the takeover
		// is a write and must be. So the case reads past anything
		// between the two that names nobody, and holds the first write
		// that names a holder to the new one.
		for {
			got := h.nextWrite(writes, "the takeover")
			if got.Owner == "" && got.Resource == resource {
				continue
			}
			want := coord.LeaseWrite{Resource: resource, Owner: "node-a/2", Epoch: second.Epoch}
			if got != want {
				h.t.Fatalf("the takeover was heard as %+v, want %+v", got, want)
			}
			return
		}
	}},

	{"a_watch_hears_its_class_and_no_other", func(h *harness) {
		// NARROWED AT THE SOURCE, as a listing is: a presence watch handed
		// every seat's heartbeat would cost a node the whole fleet's lease
		// traffic to hear a join. The seat is written FIRST, so a watch
		// that heard it would hand it over first.
		writes := h.watch(coord.ClassNode)
		h.claim(coord.SeatResource(seatID("ceo")), coord.AcquireOptions{Owner: "node-a/1", TTL: LongTTL})
		h.claim(coord.WorkerResource("scheduler"), coord.AcquireOptions{
			Owner: "node-a/1", TTL: LongTTL, Ungated: true,
		})
		lease := h.claim(coord.NodeResource("a"), coord.AcquireOptions{
			Owner: "node-a/1", TTL: LongTTL, Ungated: true,
		})
		h.requireWrite(writes, "the presence claim, after two of other classes",
			coord.LeaseWrite{Resource: lease.Resource, Owner: "node-a/1", Epoch: lease.Epoch})
	}},

	{"a_watch_ends_with_its_context", func(h *harness) {
		ctx, cancel := context.WithCancel(h.ctx)
		writes, err := h.b.WatchLive(ctx, coord.ClassNode)
		if err != nil {
			cancel()
			h.t.Fatalf("WatchLive: %v", err)
		}
		cancel()
		deadline := time.After(watchBound)
		for {
			select {
			case _, open := <-writes:
				if !open {
					return
				}
			case <-deadline:
				h.t.Fatalf("a watch whose context ended was still open %v later — a "+
					"consumer that stops its watch would leak it", watchBound)
			}
		}
	}},

	{"a_watch_that_cannot_start_is_unknown_never_a_silent_channel", func(h *harness) {
		// A channel that never speaks reads as a class nobody writes to,
		// which is the empty answer the tri-state forbids. A context
		// already ended is the one way every backend can be made unable
		// to start one.
		ctx, cancel := context.WithCancel(h.ctx)
		cancel()
		writes, err := h.b.WatchLive(ctx, coord.ClassNode)
		if err == nil {
			h.t.Fatalf("WatchLive under an ended context answered a channel (%v) and no "+
				"error", writes != nil)
		}
		if !errors.Is(err, coord.ErrUnavailable) {
			h.t.Fatalf("WatchLive under an ended context answered %v, which does not wrap "+
				"coord.ErrUnavailable", err)
		}
	}},

	{"a_class_that_cannot_address_a_key_cannot_be_watched", func(h *harness) {
		// REFUSED rather than answered, for the listing's reason: a filter
		// that matches no key is a watch that never speaks.
		for _, class := range []coord.Class{"", "seat:", "se.at"} {
			if writes, err := h.b.WatchLive(h.ctx, class); err == nil {
				h.t.Fatalf("WatchLive(%q) answered a channel (%v); a class that cannot "+
					"address a key must be refused", class, writes != nil)
			}
		}
	}},
}

// watch starts a watch of class that ends with the case.
func (h *harness) watch(class coord.Class) <-chan coord.LeaseWrite {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	writes, err := h.b.WatchLive(ctx, class)
	if err != nil {
		h.t.Fatalf("WatchLive(%q): %v", class, err)
	}
	return writes
}

// nextWrite is the next write the watch hands over, failing the case if none
// arrives within watchBound or the watch closes.
func (h *harness) nextWrite(writes <-chan coord.LeaseWrite, what string) coord.LeaseWrite {
	h.t.Helper()
	select {
	case w, open := <-writes:
		if !open {
			h.t.Fatalf("the watch closed before %s was heard", what)
		}
		return w
	case <-time.After(watchBound):
		h.t.Fatalf("%s was not heard within %v", what, watchBound)
	}
	return coord.LeaseWrite{}
}

// requireWrite asserts the next write the watch hands over is want.
func (h *harness) requireWrite(writes <-chan coord.LeaseWrite, what string, want coord.LeaseWrite) {
	h.t.Helper()
	if got := h.nextWrite(writes, what); got != want {
		h.t.Fatalf("%s was heard as %+v, want %+v", what, got, want)
	}
}
