package queuetest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runListing certifies ListSubscriptions: the broker's answer to "which
// mailboxes exist", which is the only thing that can find a mailbox whose name
// every other record has forgotten.
//
// Its failures are all silent in the same way. A listing that omits an
// unattached subscription, a peer's subscription or one with an awkward name
// hides exactly the mailbox a retirement sweep is looking for, and that
// mailbox then retains its mail for the life of the deployment with nothing
// anywhere reporting it. A listing that includes a stream subscription or a
// deleted one sends the sweep after something that is not a mailbox. So each
// case below is one way a plausible backend answers wrongly while every other
// case in this suite still passes.
func (s *suite) runListing(t *testing.T) {
	ctx := t.Context()

	t.Run("every_durable_subscription_is_listed_attached_or_not", func(t *testing.T) {
		t.Parallel()
		// A removed seat's mailbox is exactly the one nothing is attached
		// to, so a listing of attachments would miss the only case that
		// matters. And all three ways a subscription comes to exist count.
		q := s.start(ctx, t)
		if _, err := q.EnsureSubscription(ctx, "listed.idle.inbox", "grp-idle"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		subscribe(ctx, t, q, "listed.one.inbox", "grp-one", recordingHandler(newJournal()))
		if err := q.SubscribeBatch(ctx, "listed.batch.inbox", "grp-batch",
			func(context.Context, []*events.Event) queue.Result { return queue.Ack() },
			nil, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}
		requireListed(ctx, t, q, "listed.>",
			queue.Subscription{Topic: "listed.batch.inbox", Group: "grp-batch"},
			queue.Subscription{Topic: "listed.idle.inbox", Group: "grp-idle"},
			queue.Subscription{Topic: "listed.one.inbox", Group: "grp-one"},
		)
	})

	t.Run("a_detached_subscription_stays_listed_and_a_deleted_one_does_not", func(t *testing.T) {
		t.Parallel()
		// Detach is the non-destructive verb and DeleteSubscription the
		// destructive one; the listing has to tell them apart the way the
		// mail does.
		q := s.start(ctx, t)
		subscribe(ctx, t, q, "gone.kept.inbox", "grp", recordingHandler(newJournal()))
		if _, err := q.Detach(ctx, "gone.kept.inbox", "grp"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		if _, err := q.EnsureSubscription(ctx, "gone.doomed.inbox", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if deleted, err := q.DeleteSubscription(ctx, "gone.doomed.inbox", "grp"); err != nil || !deleted {
			t.Fatalf("DeleteSubscription = (%v, %v), want (true, nil)", deleted, err)
		}
		requireListed(ctx, t, q, "gone.>", queue.Subscription{Topic: "gone.kept.inbox", Group: "grp"})
	})

	t.Run("the_topic_pattern_filters_with_the_stream_grammar", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		for _, sub := range []queue.Subscription{
			{Topic: "pat.alice.inbox", Group: "agent-alice"},
			{Topic: "pat.alice.control", Group: "agent-alice-control"},
			{Topic: "pat.bob.inbox", Group: "agent-bob"},
			{Topic: "other.carol.inbox", Group: "agent-carol"},
		} {
			if _, err := q.EnsureSubscription(ctx, sub.Topic, sub.Group); err != nil {
				t.Fatalf("EnsureSubscription(%s, %s): %v", sub.Topic, sub.Group, err)
			}
		}
		requireListed(ctx, t, q, "pat.*.inbox",
			queue.Subscription{Topic: "pat.alice.inbox", Group: "agent-alice"},
			queue.Subscription{Topic: "pat.bob.inbox", Group: "agent-bob"},
		)
		requireListed(ctx, t, q, "pat.>",
			queue.Subscription{Topic: "pat.alice.control", Group: "agent-alice-control"},
			queue.Subscription{Topic: "pat.alice.inbox", Group: "agent-alice"},
			queue.Subscription{Topic: "pat.bob.inbox", Group: "agent-bob"},
		)
		// A pattern spanning namespaces reaches all of them: a backend that
		// keeps each namespace apart must still answer the whole pattern.
		requireListed(ctx, t, q, ">",
			queue.Subscription{Topic: "other.carol.inbox", Group: "agent-carol"},
			queue.Subscription{Topic: "pat.alice.control", Group: "agent-alice-control"},
			queue.Subscription{Topic: "pat.alice.inbox", Group: "agent-alice"},
			queue.Subscription{Topic: "pat.bob.inbox", Group: "agent-bob"},
		)
		requireListed(ctx, t, q, "nobody.>")
	})

	t.Run("the_pair_comes_back_exactly_as_it_was_created", func(t *testing.T) {
		t.Parallel()
		// The names a backend has to rewrite to store, sent on purpose. A
		// listing that handed back a rewritten group names a subscription
		// nothing can address: deleting "h_i" deletes nothing and leaves
		// "h.i" retaining its mail. And two pairs that differ only in a
		// rewritten character are two subscriptions, never one.
		q := s.start(ctx, t)
		want := []queue.Subscription{
			{Topic: "exact.shared", Group: "h.i"},
			{Topic: "exact.shared", Group: "h_i"},
			{Topic: "exact.shared", Group: "with space"},
			{Topic: "exact.a.b", Group: "g"},
			{Topic: "exact.a_b", Group: "g"},
		}
		for _, sub := range want {
			if _, err := q.EnsureSubscription(ctx, sub.Topic, sub.Group); err != nil {
				t.Skipf("backend refuses the pair (%q, %q): %v", sub.Topic, sub.Group, err)
			}
		}
		requireListed(ctx, t, q, "exact.>", want...)
	})

	t.Run("a_stream_subscription_is_not_a_mailbox", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		unsub, err := q.SubscribeStream(ctx, "eph.>", func(context.Context, string, *events.Event) {})
		if err != nil {
			t.Fatalf("SubscribeStream: %v", err)
		}
		t.Cleanup(func() { _ = unsub(context.WithoutCancel(ctx)) })
		if _, err := q.EnsureSubscription(ctx, "eph.durable", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		requireListed(ctx, t, q, "eph.>", queue.Subscription{Topic: "eph.durable", Group: "grp"})
	})

	t.Run("a_peer_lists_what_another_client_created", func(t *testing.T) {
		t.Parallel()
		// The sweep that retires a mailbox runs on whichever node holds the
		// duty, and the node that created the mailbox may be gone.
		peer := s.needPeer(t)
		a := s.start(ctx, t)
		b := startQueue(ctx, t, peer(t, a))
		if _, err := a.EnsureSubscription(ctx, "fleet.alice.inbox", "agent-alice"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		requireListed(ctx, t, b, "fleet.>", queue.Subscription{Topic: "fleet.alice.inbox", Group: "agent-alice"})
	})

	t.Run("an_empty_pattern_is_refused_not_read_as_everything", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		if _, err := q.EnsureSubscription(ctx, "blank.t", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if got, err := q.ListSubscriptions(ctx, ""); err == nil {
			t.Fatalf("ListSubscriptions(\"\") = (%v, nil), want an error: a missing argument must "+
				"not become the widest possible answer", got)
		}
	})
}

// requireListed asserts a listing is exactly want, in any order.
func requireListed(ctx context.Context, t *testing.T, q queue.EventQueue, pattern string, want ...queue.Subscription) {
	t.Helper()
	got, err := q.ListSubscriptions(ctx, pattern)
	if err != nil {
		t.Fatalf("ListSubscriptions(%q): %v", pattern, err)
	}
	sortSubscriptions(got)
	want = slices.Clone(want)
	sortSubscriptions(want)
	if !slices.Equal(got, want) {
		t.Fatalf("ListSubscriptions(%q) = %v, want %v", pattern, got, want)
	}
}

func sortSubscriptions(subs []queue.Subscription) {
	slices.SortFunc(subs, func(a, b queue.Subscription) int {
		if c := strings.Compare(a.Topic, b.Topic); c != 0 {
			return c
		}
		return strings.Compare(a.Group, b.Group)
	})
}
