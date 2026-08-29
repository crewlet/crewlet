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

		publish(ctx, t, q, "crewlet.events.task_created", newEvent("task_created"))

		a.awaitLabels(t, "the first subscriber's copy", "crewlet.events.task_created/task_created")
		b.awaitLabels(t, "the second subscriber's copy", "crewlet.events.task_created/task_created")
	})

	t.Run("stream_filters_by_topic_pattern", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		streamTo(ctx, t, q, "crewlet.events.>", j)

		publish(ctx, t, q, "crewlet.events.task_created", newEvent("task_created"))
		publish(ctx, t, q, "crewlet.notifications.inbound", newEvent("raw_webhook"))

		j.awaitLabels(t, "only the matching subject",
			"crewlet.events.task_created/task_created")
		j.staysAt(t, 1, "a non-matching subject reached a stream subscriber")
	})

	t.Run("stream_star_matches_exactly_one_segment", func(t *testing.T) {
		t.Parallel()
		// `*` is one segment and `>` is the trailing remainder. A backend
		// that treats them alike turns the dashboard's per-domain filters
		// into a firehose, silently.
		q := s.start(ctx, t)
		j := newJournal()
		streamTo(ctx, t, q, "crewlet.events.*", j)

		publish(ctx, t, q, "crewlet.events.task_created", newEvent("match"))

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

		j.awaitLabels(t, "the single-segment match", "crewlet.events.task_created/match")
		j.staysAt(t, 1, "`*` matched something other than exactly one segment")
	})

	t.Run("stream_unsubscribe_stops_dispatch", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		unsubscribe, err := q.SubscribeStream(ctx, "crewlet.events.>",
			func(_ context.Context, topic string, ev *events.Event) {
				j.record(topic + "/" + labelOf(ev))
			})
		if err != nil {
			t.Fatalf("SubscribeStream: %v", err)
		}

		publish(ctx, t, q, "crewlet.events.task_created", newEvent("task_created"))
		j.awaitLabels(t, "the pre-unsubscribe event", "crewlet.events.task_created/task_created")

		if err := unsubscribe(ctx); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
		publish(ctx, t, q, "crewlet.events.task_failed", newEvent("task_failed"))
		j.staysAt(t, 1, "an unsubscribed stream kept receiving")
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
		publish(ctx, t, q, "crewlet.events.task_created", newEvent("task_created"))
		healthy.awaitLabels(t, "the healthy subscriber's copy",
			"crewlet.events.task_created/task_created")
	})

	t.Run("stream_does_not_consume_from_durable_subscriptions", func(t *testing.T) {
		t.Parallel()
		// A broadcast subscriber is not a member of any group. If it took
		// deliveries from one, opening a dashboard would start stealing a
		// seat's mail.
		q := s.start(ctx, t)
		stream, durable := newJournal(), newJournal()
		streamTo(ctx, t, q, "crewlet.events.>", stream)
		subscribe(ctx, t, q, "crewlet.events.task_created", "grp", recordingHandler(durable))

		publish(ctx, t, q, "crewlet.events.task_created", newEvent("task_created"))

		stream.awaitLabels(t, "the stream copy", "crewlet.events.task_created/task_created")
		durable.awaitLabels(t, "the durable subscriber's copy", "task_created")
	})
}

func streamTo(ctx context.Context, t *testing.T, q queue.EventQueue, pattern string, j *journal) {
	t.Helper()
	if _, err := q.SubscribeStream(ctx, pattern,
		func(_ context.Context, topic string, ev *events.Event) {
			j.record(topic + "/" + labelOf(ev))
		}); err != nil {
		t.Fatalf("SubscribeStream(%s): %v", pattern, err)
	}
}
