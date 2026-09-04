package coordtest

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// feedCases certify the durable change feed.
//
// A FEED IS HOW A COMMITTED RECORD BECOMES A WAKE, and every failure here is
// silent in production: a change delivered twice wakes a seat twice, one
// delivered to nobody is a person never told their item was assigned, and a
// consumer that replayed history on every restart would wake the whole
// company for everything that ever happened. None of those produce an error
// anywhere, which is why they are certified against both backends rather than
// asserted against the twin.
var feedCases = []fleetCase{
	{"a new feed starts at the head, never at the beginning", func(h *fleetHarness) {
		feeder, ok := h.f.(coord.Feeder)
		if !ok {
			h.t.Skip("this backend serves no feeds")
		}
		// Written BEFORE the feed exists. An upgrade that introduced a
		// feed must not wake every seat for every change the company ever
		// made — which is what a DeliverAll consumer would do, silently
		// and exactly once, at the worst possible moment.
		mustCreate(h, coord.DocumentKey("c", "old", "1"), `{"before":true}`)

		feed, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", h.name("history"))
		if err != nil {
			h.t.Fatalf("open the feed: %v", err)
		}
		defer feed.Stop()

		mustCreate(h, coord.DocumentKey("c", "new", "1"), `{"after":true}`)
		got := nextChange(h, feed)
		if got.Key != coord.DocumentKey("c", "new", "1") {
			h.t.Errorf("the feed delivered %q — a new consumer replayed history",
				got.Key)
		}
	}},

	{"the class filter selects one class and nothing else", func(h *fleetHarness) {
		feeder, ok := h.f.(coord.Feeder)
		if !ok {
			h.t.Skip("this backend serves no feeds")
		}
		feed, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", h.name("filter"))
		if err != nil {
			h.t.Fatalf("open the feed: %v", err)
		}
		defer feed.Stop()

		// A head, a comment and a counter, then the change. Only the last
		// may arrive: a feed over a REWRITABLE key loses wakes silently,
		// because a bucket keeps one revision per key and rewriting one
		// terminates any un-acked message already delivered for it.
		mustCreate(h, coord.DocumentKey("i", "item"), `{"head":true}`)
		mustCreate(h, coord.DocumentKey("m", "item", "c1"), `{"comment":true}`)
		mustCreate(h, coord.DocumentKey("n", "ENG"), `{"counter":true}`)
		mustCreate(h, coord.DocumentKey("c", "item", "u1"), `{"change":true}`)

		got := nextChange(h, feed)
		if got.Key != coord.DocumentKey("c", "item", "u1") {
			h.t.Fatalf("the feed delivered %q, want only the change key", got.Key)
		}
	}},

	{"an un-acked change comes back, and an acked one does not", func(h *fleetHarness) {
		feeder, ok := h.f.(coord.Feeder)
		if !ok {
			h.t.Skip("this backend serves no feeds")
		}
		feed, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", h.name("redeliver"))
		if err != nil {
			h.t.Fatalf("open the feed: %v", err)
		}
		defer feed.Stop()

		key := coord.DocumentKey("c", "item", "u1")
		mustCreate(h, key, `{"n":1}`)

		// NAKED WITH NO DELAY, which is the handler's honest answer when
		// it could not reach something it needs: the change comes back to
		// whichever node asks next. Dropping it instead would be a wake
		// nobody is ever told about.
		first := nextDelivery(h, feed)
		if first.Key != key {
			h.t.Fatalf("delivered %q", first.Key)
		}
		if err := first.Nak(0); err != nil {
			h.t.Fatalf("nak: %v", err)
		}
		again := nextDelivery(h, feed)
		if again.Key != key {
			h.t.Fatalf("a naked change came back as %q", again.Key)
		}
		if err := again.Ack(); err != nil {
			h.t.Fatalf("ack: %v", err)
		}

		// And an ACKED change does not come back. A feed that redelivered
		// after an ack would wake a seat twice for one assignment.
		mustCreate(h, coord.DocumentKey("c", "item", "u2"), `{"n":2}`)
		next := nextDelivery(h, feed)
		if next.Key == key {
			h.t.Error("an acked change was delivered again")
		}
		_ = next.Ack()
	}},

	{"a purge reaches the feed as a removal", func(h *fleetHarness) {
		feeder, ok := h.f.(coord.Feeder)
		if !ok {
			h.t.Skip("this backend serves no feeds")
		}
		feed, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", h.name("purge"))
		if err != nil {
			h.t.Fatalf("open the feed: %v", err)
		}
		defer feed.Stop()

		key := coord.DocumentKey("c", "item", "u1")
		mustCreate(h, key, `{"n":1}`)
		created := nextDelivery(h, feed)
		_ = created.Ack()

		rec, found, err := h.f.Document(h.ctx, coord.FamilyWork, key)
		if err != nil || !found {
			h.t.Fatalf("read back: found=%v err=%v", found, err)
		}
		if ok, err := h.f.PurgeDocument(h.ctx, coord.FamilyWork, key, rec.Version); err != nil || !ok {
			h.t.Fatalf("purge: ok=%v err=%v", ok, err)
		}
		got := nextDelivery(h, feed)
		if got.Op != coord.OpPurge {
			h.t.Errorf("a purge arrived as %q — a removal that did not travel "+
				"leaves the record on every peer", got.Op)
		}
		_ = got.Ack()
	}},

	{"one change goes to one consumer", func(h *fleetHarness) {
		feeder, ok := h.f.(coord.Feeder)
		if !ok {
			h.t.Skip("this backend serves no feeds")
		}
		group := h.name("shared")
		a, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", group)
		if err != nil {
			h.t.Fatalf("open feed A: %v", err)
		}
		defer a.Stop()
		b, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", group)
		if err != nil {
			h.t.Fatalf("open feed B: %v", err)
		}
		defer b.Stop()

		mustCreate(h, coord.DocumentKey("c", "item", "u1"), `{"n":1}`)

		// TWO NODES, ONE DURABLE GROUP: exactly one of them gets it. Both
		// getting it would wake every recipient twice for one change,
		// which is the failure a fleet-wide group exists to prevent.
		type result struct {
			from string
			d    *coord.Delivery
		}
		results := make(chan result, 2)
		for name, feed := range map[string]coord.Feed{"A": a, "B": b} {
			go func() {
				ctx, cancel := contextWithTimeout(h, feedBudget)
				defer cancel()
				d, _ := feed.Next(ctx)
				results <- result{from: name, d: d}
			}()
		}
		delivered := 0
		for range 2 {
			got := <-results
			if got.d != nil {
				delivered++
				_ = got.d.Ack()
			}
		}
		if delivered != 1 {
			h.t.Errorf("%d of two consumers got the same change", delivered)
		}
	}},

	{"a durable group resumes where it left off", func(h *fleetHarness) {
		feeder, ok := h.f.(coord.Feeder)
		if !ok {
			h.t.Skip("this backend serves no feeds")
		}
		group := h.name("resume")
		first, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", group)
		if err != nil {
			h.t.Fatalf("open the feed: %v", err)
		}
		mustCreate(h, coord.DocumentKey("c", "item", "u1"), `{"n":1}`)
		got := nextDelivery(h, first)
		_ = got.Ack()

		// The node goes away. A change written while it is gone must be
		// waiting when it comes back — the consumer's position belongs to
		// the FLEET, not to a process, which is the whole reason a
		// restart resumes rather than replays.
		if err := first.Stop(); err != nil {
			h.t.Fatalf("stop: %v", err)
		}
		mustCreate(h, coord.DocumentKey("c", "item", "u2"), `{"n":2}`)

		second, err := feeder.FeedDocuments(h.ctx, coord.FamilyWork, "c", group)
		if err != nil {
			h.t.Fatalf("reopen the feed: %v", err)
		}
		defer second.Stop()
		back := nextDelivery(h, second)
		if back.Key != coord.DocumentKey("c", "item", "u2") {
			h.t.Errorf("after a restart the feed delivered %q, want the change "+
				"written while it was away", back.Key)
		}
		_ = back.Ack()
	}},
}

// name scopes a durable group to this case, so parallel cases against one
// broker never share a consumer — which would make each of them see the
// other's changes and none of them see all of its own.
func (h *fleetHarness) name(suffix string) string {
	return "crewlet-test-" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '-'
	}, h.t.Name()) + "-" + suffix
}

// contextWithTimeout bounds one wait on the harness's own context.
func contextWithTimeout(h *fleetHarness, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(h.ctx, d)
}

// mustCreate writes a document or fails the case.
func mustCreate(h *fleetHarness, key, value string) {
	h.t.Helper()
	created, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(value))
	if err != nil || !created {
		h.t.Fatalf("create %s: created=%v err=%v", key, created, err)
	}
}

// nextDelivery takes the next delivery or fails the case.
func nextDelivery(h *fleetHarness, feed coord.Feed) *coord.Delivery {
	h.t.Helper()
	ctx, cancel := contextWithTimeout(h, feedBudget)
	defer cancel()
	got, err := feed.Next(ctx)
	if err != nil {
		h.t.Fatalf("feed: %v", err)
	}
	if got == nil {
		h.t.Fatalf("no change arrived in %v", feedBudget)
	}
	return got
}

// nextChange takes the next delivery and acks it.
func nextChange(h *fleetHarness, feed coord.Feed) coord.Change {
	h.t.Helper()
	got := nextDelivery(h, feed)
	_ = got.Ack()
	return got.Change
}

// feedBudget is how long a case waits for a delivery.
//
// Generous for the reason [watchBudget] is: it bounds a real broker round
// trip on one backend, and a feed that is merely slow looks exactly like one
// that is broken until it expires.
const feedBudget = 15 * time.Second
