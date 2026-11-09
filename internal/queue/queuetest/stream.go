package queuetest

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runStream covers broadcast delivery — the dashboard's fan-out. Unlike a
// consumer group, every stream subscriber receives every matching event, and
// nothing is redelivered: a stream handler's failure is the subscriber's
// problem, never the publisher's.
func (s *suite) runStream(t *testing.T) {
	ctx := t.Context()

	t.Run("stream_broadcasts_to_every_subscriber", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		a, b := newJournal(), newJournal()
		streamTo(ctx, t, q, "crewlet.events.>", a)
		streamTo(ctx, t, q, "crewlet.events.>", b)

		publish(ctx, t, q, "crewlet.events.agent_phase_started", newEvent("agent_phase_started"))

		a.awaitLabels(t, "the first subscriber's copy", "crewlet.events.agent_phase_started/agent_phase_started")
		b.awaitLabels(t, "the second subscriber's copy", "crewlet.events.agent_phase_started/agent_phase_started")
	})

	t.Run("stream_filters_by_topic_pattern", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		streamTo(ctx, t, q, "crewlet.events.>", j)

		publish(ctx, t, q, "crewlet.events.agent_phase_started", newEvent("agent_phase_started"))
		publish(ctx, t, q, "crewlet.notifications.inbound", newEvent("raw_webhook"))

		// READ BEHIND A FENCE on the subscriber's own stream, published
		// after both. The non-matching subject lives on another stream on
		// the backend that ships, which the subscriber's consumer cannot
		// read at all, so nothing there could reach it behind the fence
		// that had not before it; the twin hands a feed its events inside
		// each Publish.
		//
		// Mutation: match every subject in the twin's stream dispatch, and
		// the webhook is in the journal ahead of the fence.
		j.fenced(ctx, t, q, "a non-matching subject reached a stream subscriber",
			"crewlet.events.fence", "crewlet.events.agent_phase_started/agent_phase_started")
	})

	t.Run("stream_star_matches_exactly_one_segment", func(t *testing.T) {
		t.Parallel()
		// `*` is one segment and `>` is the trailing remainder. A backend
		// that treats them alike turns the dashboard's per-domain filters
		// into a firehose, silently.
		q := s.start(ctx, t)
		j := newJournal()
		streamTo(ctx, t, q, "crewlet.events.*", j)

		publish(ctx, t, q, "crewlet.events.agent_phase_started", newEvent("match"))

		// The near-miss subjects are published WITHOUT requiring the
		// publish to succeed. They exist only to be rejected by the
		// pattern, and neither is a subject the engine's own grammar can
		// produce — topics.Event always appends exactly one segment. A
		// backend is entitled to refuse a subject it has no stream for
		// (measured: JetStream rejects a bare `crewlet.events` as
		// overlapping the `crewlet.events.>` stream), and a refusal
		// satisfies the property under test just as well as accepting it
		// and not matching: either way the subscriber must not see it.
		// Making these fatal tested the backend's subject topology, not
		// its wildcard matching.
		tryPublish(ctx, q, "crewlet.events.task.created", newEvent("too_deep"))
		tryPublish(ctx, q, "crewlet.events", newEvent("too_shallow"))

		// READ BEHIND A FENCE published after the near-misses, on the same
		// stream: the subscriber is handed that stream in its own order,
		// so a near-miss its pattern let through arrives ahead of the
		// fence, however late.
		//
		// Mutation: filter the feed's consumer on `crewlet.events.>`
		// rather than on its own pattern, and too_deep arrives ahead of
		// the fence.
		j.fenced(ctx, t, q, "`*` matched something other than exactly one segment",
			"crewlet.events.fence", "crewlet.events.agent_phase_started/match")
	})

	t.Run("stream_unsubscribe_stops_dispatch", func(t *testing.T) {
		t.Parallel()
		feeds := s.needStreamSubscriptions(t)
		q := s.start(ctx, t)
		j := newJournal()
		unsubscribe := streamTo(ctx, t, q, "crewlet.events.>", j)

		publish(ctx, t, q, "crewlet.events.agent_phase_started", newEvent("agent_phase_started"))
		j.awaitLabels(t, "the pre-unsubscribe event", "crewlet.events.agent_phase_started/agent_phase_started")

		// THE COUNT HAS TO HAVE MATCHED ITS SUBJECT before it is read as
		// gone: a backend that never counted this feed would read as one
		// whose unsubscribe worked.
		if got := feeds(q); got != 1 {
			t.Fatalf("StreamSubscriptions = %d with one feed open, want 1", got)
		}
		if err := unsubscribe(ctx); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
		// WAITED UNTIL THE FEED HAS STOPPED CONSUMING, which on the
		// backend that ships comes after its Unsubscribe returns. A feed
		// is no consume loop Settle could wait out, and once it is gone
		// there is nothing to fence behind; what is left is that nothing
		// stands that could hand it anything. Bounded as liveness by
		// awaitState: a feed that never stops consuming is what fails it.
		awaitState(t, "the unsubscribed feed to stop consuming", func() bool { return feeds(q) == 0 })
		publish(ctx, t, q, "crewlet.events.turn.guard_breach", newEvent("turn.guard_breach"))
		// READ AT ONCE, with nothing to wait for: nothing that could
		// deliver it is left.
		//
		// Mutation: have the unsubscribe leave the feed's consumption
		// running, and it never stops.
		if got := j.all(); len(got) != 1 {
			t.Fatalf("an unsubscribed stream kept receiving: %v", got)
		}
	})

	t.Run("stream_handler_failure_does_not_break_publish", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		healthy := newJournal()
		if _, err := q.SubscribeStream(ctx, "crewlet.events.>",
			func(context.Context, string, *events.Event) { panic("stream client crashed") }); err != nil {
			t.Fatalf("SubscribeStream: %v", err)
		}
		streamTo(ctx, t, q, "crewlet.events.>", healthy)

		// The publish itself must complete, and the healthy subscriber
		// must still be served: one browser tab closing mid-frame cannot
		// be allowed to take the publisher down with it.
		publish(ctx, t, q, "crewlet.events.agent_phase_started", newEvent("agent_phase_started"))
		healthy.awaitLabels(t, "the healthy subscriber's copy",
			"crewlet.events.agent_phase_started/agent_phase_started")
	})

	t.Run("stream_does_not_consume_from_durable_subscriptions", func(t *testing.T) {
		t.Parallel()
		// A broadcast subscriber is not a member of any group. If it took
		// deliveries from one, opening a dashboard would start stealing a
		// seat's mail.
		q := s.start(ctx, t)
		stream, durable := newJournal(), newJournal()
		streamTo(ctx, t, q, "crewlet.events.>", stream)
		subscribe(ctx, t, q, "crewlet.events.agent_phase_started", "grp", recordingHandler(durable))

		publish(ctx, t, q, "crewlet.events.agent_phase_started", newEvent("agent_phase_started"))

		stream.awaitLabels(t, "the stream copy", "crewlet.events.agent_phase_started/agent_phase_started")
		durable.awaitLabels(t, "the durable subscriber's copy", "agent_phase_started")
	})
}

// streamTo subscribes j to every event matching pattern, labelled by the
// subject it arrived on, and returns the subscription's Unsubscribe.
//
// It marks j as a stream subscriber's, which staysAt refuses: a feed is no
// consume loop, so Settle does not wait it out. Read one behind a fence
// (journal.fenced) instead.
func streamTo(ctx context.Context, t *testing.T, q queue.EventQueue, pattern string, j *journal) queue.Unsubscribe {
	t.Helper()
	j.streamed = true
	unsubscribe, err := q.SubscribeStream(ctx, pattern,
		func(_ context.Context, topic string, ev *events.Event) {
			j.record(topic + "/" + labelOf(ev))
		})
	if err != nil {
		t.Fatalf("SubscribeStream(%s): %v", pattern, err)
	}
	return unsubscribe
}
