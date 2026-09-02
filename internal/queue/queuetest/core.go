package queuetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runCore covers the properties every producer and consumer depends on:
// delivery, competition inside a group, isolation between groups, the durable
// mailbox, redelivery and its dead-letter floor, and the graceful-drain
// protocol.
func (s *suite) runCore(t *testing.T) {
	ctx := t.Context()

	t.Run("publish_subscribe", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "topic.a", "grp1", recordingHandler(j))

		ev := newEvent("test_event")
		publish(ctx, t, q, "topic.a", ev)

		j.awaitLabels(t, "the published event", "test_event")
	})

	t.Run("multiple_subscribers_different_topics", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		a, b := newJournal(), newJournal()
		subscribe(ctx, t, q, "topic.a", "g", recordingHandler(a))
		subscribe(ctx, t, q, "topic.b", "g", recordingHandler(b))

		publish(ctx, t, q, "topic.a", newEvent("a"))
		publish(ctx, t, q, "topic.b", newEvent("b"))

		a.awaitLabels(t, "topic.a's subscriber", "a")
		b.awaitLabels(t, "topic.b's subscriber", "b")
	})

	t.Run("multiple_groups_receive_same_event", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		g1, g2 := newJournal(), newJournal()
		subscribe(ctx, t, q, "topic.y", "g1", recordingHandler(g1))
		subscribe(ctx, t, q, "topic.y", "g2", recordingHandler(g2))

		publish(ctx, t, q, "topic.y", newEvent("t"))

		// A copy each: competition happens BETWEEN the members of one
		// group, never across groups.
		g1.awaitLabels(t, "group g1's copy", "t")
		g2.awaitLabels(t, "group g2's copy", "t")
	})

	t.Run("distinct_pairs_never_share_a_subscription", func(t *testing.T) {
		t.Parallel()
		// Two pairs that differ only in a character a backend is tempted to
		// rewrite when it flattens (topic, group) into one broker-side name.
		//
		// Found by enumerating what this suite SENDS rather than what it
		// asserts: every topic and group in it was a plain lowercase dotted
		// identifier, so no mutation of any backend could ever have revealed
		// this — the input never arrived. Measured on a shipped backend
		// whose consumer name is safe(group)+"__"+safe(topic) with "."
		// rewritten to "_": topic `a.b` and topic `a_b` in one group land on
		// ONE consumer. For real engine subjects that is two seats sharing an
		// inbox, reachable from two operator handles differing only by a dot,
		// and it defeats the mutual exclusion seat ownership rests on.
		//
		// A backend MAY REFUSE a name it cannot represent — the same latitude
		// it has to refuse a linger it cannot honour — and this skips if it
		// does. What it may not do is accept two distinct pairs and quietly
		// alias them.
		q := s.start(ctx, t)

		// Topic side: one group, two topics differing only by . vs _.
		dotted, under := newJournal(), newJournal()
		if err := q.Subscribe(ctx, "coll.a.b", "g", recordingHandler(dotted)); err != nil {
			t.Skipf("backend refuses the topic name coll.a.b: %v", err)
		}
		if err := q.Subscribe(ctx, "coll.a_b", "g", recordingHandler(under)); err != nil {
			t.Skipf("backend refuses the topic name coll.a_b: %v", err)
		}
		publish(ctx, t, q, "coll.a.b", newEvent("to-dotted"))
		publish(ctx, t, q, "coll.a_b", newEvent("to-under"))

		dotted.awaitLabels(t, "the dotted topic's own event", "to-dotted")
		under.awaitLabels(t, "the underscored topic's own event", "to-under")
		dotted.staysAt(t, 1, "two topics collapsed onto one subscription")
		under.staysAt(t, 1, "two topics collapsed onto one subscription")

		// Group side: one topic, two groups differing only by . vs _. Distinct
		// groups each get a copy; aliased ones would COMPETE for one.
		gd, gu := newJournal(), newJournal()
		if err := q.Subscribe(ctx, "coll.shared", "h.i", recordingHandler(gd)); err != nil {
			t.Skipf("backend refuses the group name h.i: %v", err)
		}
		if err := q.Subscribe(ctx, "coll.shared", "h_i", recordingHandler(gu)); err != nil {
			t.Skipf("backend refuses the group name h_i: %v", err)
		}
		publish(ctx, t, q, "coll.shared", newEvent("fanout"))

		gd.awaitLabels(t, "the dotted group's copy", "fanout")
		gu.awaitLabels(t, "the underscored group's copy", "fanout")
	})

	t.Run("consumer_group_competing", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		shared := newJournal()
		subscribe(ctx, t, q, "topic.x", "grp", recordingHandler(shared))
		subscribe(ctx, t, q, "topic.x", "grp", recordingHandler(shared))

		publish(ctx, t, q, "topic.x", newEvent("t"))

		shared.await(t, "exactly one member to take the event",
			func(seen []string) bool { return len(seen) == 1 })
		shared.staysAt(t, 1, "one event delivered to a two-member group")
	})

	t.Run("members_of_a_group_compete", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		// Each member journals its own name, so the sequence records WHO
		// was chosen. Delivering always to the first-registered member
		// made a double-attach split-brain invisible: two nodes consuming
		// one seat looked like one, so "exactly one delivery" passed
		// while a real Shared subscription split the traffic and ran two
		// interleaved turn streams.
		by := newJournal()
		for _, who := range []string{"a", "b"} {
			subscribe(ctx, t, q, "topic", "grp", func(_ context.Context, ev *events.Event) queue.Result {
				by.record(who + "|" + labelOf(ev))
				return queue.Ack()
			})
		}

		for i := range 4 {
			publish(ctx, t, q, "topic", newEvent("e"+string(rune('0'+i))))
		}

		if s.caps.StrictRoundRobin {
			by.awaitLabels(t, "strict round-robin across the group",
				"a|e0", "b|e1", "a|e2", "b|e3")
			return
		}
		// Without strict rotation the group owes exactly one thing: every
		// event reaches exactly ONE member. It does NOT owe the sharing of a
		// burst — measured, a broker dispatching a shared subscription by
		// available permits hands one consumer as many entries as it has
		// room for, so a single member legitimately takes all four.
		// Requiring both to participate asserted the twin's dispatch
		// strategy as though it were a broker requirement.
		//
		// Each event carries its own label so this checks per-EVENT delivery
		// rather than counting deliveries, which a backend could satisfy by
		// handling two events twice and losing two.
		by.await(t, "every event to reach exactly one member", func(seen []string) bool {
			handled := map[string]int{}
			for _, entry := range seen {
				_, label, _ := strings.Cut(entry, "|")
				handled[label]++
			}
			if len(handled) != 4 {
				return false
			}
			for _, n := range handled {
				if n != 1 {
					return false
				}
			}
			return true
		})
	})

	t.Run("subscribe_after_start", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "topic.late", "grp", recordingHandler(j))
		publish(ctx, t, q, "topic.late", newEvent("late"))
		j.awaitLabels(t, "a subscription added after start", "late")
	})

	t.Run("an_unowned_subscription_holds_its_mail", func(t *testing.T) {
		t.Parallel()
		// The property seat ownership rests on. A subscription with
		// nothing attached retains what is published to it and replays it
		// on attach; without this a seat between owners loses every event
		// published in the gap.
		q := s.start(ctx, t)
		const topic, group = "crewlet.agent.alice.inbox", "agent-alice"
		created, err := q.EnsureSubscription(ctx, topic, group)
		if err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if !created {
			t.Fatalf("EnsureSubscription reported the subscription already existed")
		}
		for _, label := range []string{"e0", "e1", "e2"} {
			publish(ctx, t, q, topic, newEvent(label))
		}
		if backlog := s.caps.Backlog; backlog != nil {
			awaitState(t, "three retained events", func() bool {
				return len(backlog(q, topic, group)) == 3
			})
		}

		j := newJournal()
		subscribe(ctx, t, q, topic, group, recordingHandler(j))
		j.awaitLabels(t, "the retained mail to replay in order", "e0", "e1", "e2")
	})

	t.Run("publishing_to_no_subscription_retains_nothing", func(t *testing.T) {
		t.Parallel()
		// Which is exactly why EnsureSubscription exists.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		publish(ctx, t, q, "nobody.listening", newEvent("lost"))
		if got := backlog(q, "nobody.listening", "grp"); len(got) != 0 {
			t.Fatalf("a topic with no subscription retained %v", labelsOf(got))
		}
	})

	t.Run("ensure_subscription_reports_creation", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		created, err := q.EnsureSubscription(ctx, "topic.ensure", "grp")
		if err != nil || !created {
			t.Fatalf("first EnsureSubscription = (%v, %v), want (true, nil)", created, err)
		}
		// Creating one that exists is success, not an error — a boot that
		// re-declares every seat's inbox must be a no-op.
		created, err = q.EnsureSubscription(ctx, "topic.ensure", "grp")
		if err != nil || created {
			t.Fatalf("second EnsureSubscription = (%v, %v), want (false, nil)", created, err)
		}
	})

	t.Run("delete_subscription_discards_retained_mail", func(t *testing.T) {
		t.Parallel()
		// The destructive half: a decommissioned role's inbox must not
		// accumulate undeliverable events forever, and deleting must not
		// require a local consumer — role removal cannot depend on which
		// node happened to run the seat.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		if _, err := q.EnsureSubscription(ctx, "topic.gone", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(ctx, t, q, "topic.gone", newEvent("e1"))
		awaitState(t, "the event to be retained", func() bool {
			return len(backlog(q, "topic.gone", "grp")) == 1
		})

		deleted, err := q.DeleteSubscription(ctx, "topic.gone", "grp")
		if err != nil || !deleted {
			t.Fatalf("DeleteSubscription = (%v, %v), want (true, nil)", deleted, err)
		}
		if got := backlog(q, "topic.gone", "grp"); len(got) != 0 {
			t.Fatalf("deleting the subscription left %v behind", labelsOf(got))
		}

		publish(ctx, t, q, "topic.gone", newEvent("e2"))
		if got := backlog(q, "topic.gone", "grp"); len(got) != 0 {
			t.Fatalf("publishing into a deleted subscription retained %v", labelsOf(got))
		}
		// Already gone is the desired end state, not an error.
		deleted, err = q.DeleteSubscription(ctx, "topic.gone", "grp")
		if err != nil || deleted {
			t.Fatalf("second DeleteSubscription = (%v, %v), want (false, nil)", deleted, err)
		}
	})

	t.Run("handler_failure_triggers_redelivery", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		var attempts int
		subscribe(ctx, t, q, "topic.flaky", "grp", func(context.Context, *events.Event) queue.Result {
			attempts++
			j.record("attempt")
			if attempts < 3 {
				return queue.Nak(errors.New("transient failure"))
			}
			return queue.Ack()
		})

		publish(ctx, t, q, "topic.flaky", newEvent("t"))

		j.await(t, "two failures and a success", func(seen []string) bool { return len(seen) == 3 })
		j.staysAt(t, 3, "an acknowledged event")
	})

	t.Run("nak_returns_the_event_to_the_front_of_the_mailbox", func(t *testing.T) {
		t.Parallel()
		if !s.caps.HeadReplayOnNak {
			t.Skip("backend redelivers behind never-delivered events")
		}
		q := s.start(ctx, t)
		seen := newJournal()
		var failed bool
		subscribe(ctx, t, q, "topic.head", "grp", func(_ context.Context, ev *events.Event) queue.Result {
			seen.record(labelOf(ev))
			if !failed {
				failed = true
				return queue.Nak(errors.New("once"))
			}
			return queue.Ack()
		})

		// Held while publishing, so both events are waiting when the
		// first one fails and there is something for the redelivery to
		// come back ahead of.
		if err := q.PauseTopic(ctx, "topic.head", "grp", "test"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		publish(ctx, t, q, "topic.head", newEvent("e1"))
		publish(ctx, t, q, "topic.head", newEvent("e2"))
		if err := q.ResumeTopic(ctx, "topic.head", "grp", "test"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}

		seen.awaitLabels(t, "the redelivery to come back ahead of e2", "e1", "e1", "e2")
	})

	t.Run("redelivery_rotates_across_members", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		by := newJournal()
		subscribe(ctx, t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			by.record("a")
			return queue.Nak(errors.New("fail"))
		})
		subscribe(ctx, t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			by.record("b")
			return queue.Ack()
		})

		publish(ctx, t, q, "topic", newEvent("t"))

		if s.caps.StrictRoundRobin {
			// First delivery goes to member 0 (fails); the redelivery
			// advances the cursor to member 1, which succeeds.
			by.awaitLabels(t, "redelivery to move to the next member", "a", "b")
			return
		}
		by.await(t, "a healthy member to take the redelivery", func(seen []string) bool {
			return slices.Contains(seen, "b")
		})
	})

	t.Run("exhausted_redeliveries_dead_letter_the_event", func(t *testing.T) {
		t.Parallel()
		// A poison message is PRESERVED, not destroyed. The budget counts
		// redeliveries AFTER the first delivery — N+1 total attempts —
		// and the exhausted message moves to the dead-letter subject.
		// Counting total attempts and then dropping made every retry-count
		// assertion off by one and every "poison message is recoverable"
		// claim false.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		backlog := s.needBacklog(t)
		// Three attempts total, however this backend's broker counts them.
		q := startQueue(ctx, t, newQueueWithAttempts(t, 3))

		j := newJournal()
		subscribe(ctx, t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			j.record("attempt")
			return queue.Nak(errors.New("permanent failure"))
		})
		publish(ctx, t, q, "topic", newEvent("t"))

		j.await(t, "the configured number of delivery attempts",
			func(seen []string) bool { return len(seen) == 3 })
		j.staysAt(t, 3, "a dead-lettered event")

		awaitState(t, "the event to reach the dead-letter subject", func() bool {
			return len(deadLetters(q, "topic", "grp")) == 1
		})
		if got := backlog(q, "topic", "grp"); len(got) != 0 {
			t.Fatalf("a dead-lettered event stayed in the backlog: %v", labelsOf(got))
		}
		if got := labelsOf(deadLetters(q, "topic", "grp")); !equalStrings(got, []string{"t"}) {
			t.Fatalf("dead letters = %v, want [t]", got)
		}
	})

	t.Run("defer_delivery_leaves_the_event_and_stops_consuming", func(t *testing.T) {
		t.Parallel()
		// The third handler outcome, and the one a lost seat needs. It
		// neither claims the work (ack) nor spends the message's
		// dead-letter budget (nak).
		backlog := s.needBacklog(t)
		deadLetters := s.needDeadLetters(t)
		q := s.start(ctx, t)

		j := newJournal()
		subscribe(ctx, t, q, "topic.d", "grp", func(_ context.Context, ev *events.Event) queue.Result {
			j.record(labelOf(ev))
			return queue.Defer("lease moved")
		})
		publish(ctx, t, q, "topic.d", newEvent("e1"))
		publish(ctx, t, q, "topic.d", newEvent("e2"))

		j.awaitLabels(t, "the first delivery", "e1")
		j.staysAt(t, 1, "a deferral must stop the attachment taking new work")

		awaitState(t, "both events to be back in order", func() bool {
			return equalStrings(labelsOf(backlog(q, "topic.d", "grp")), []string{"e1", "e2"})
		})
		if got := deadLetters(q, "topic.d", "grp"); len(got) != 0 {
			t.Fatalf("a deferral spent dead-letter budget: %v", labelsOf(got))
		}
	})

	t.Run("pause_topic_buffers_then_resume_flushes", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		a, b := newJournal(), newJournal()
		subscribe(ctx, t, q, "topic.a", "grp", recordingHandler(a))
		subscribe(ctx, t, q, "topic.b", "grp", recordingHandler(b))

		if err := q.PauseTopic(ctx, "topic.a", "grp", "test"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		publish(ctx, t, q, "topic.a", newEvent("a1"))
		publish(ctx, t, q, "topic.a", newEvent("a2"))
		// A different topic is unaffected while topic.a is held.
		publish(ctx, t, q, "topic.b", newEvent("b1"))

		b.awaitLabels(t, "an unpaused topic to keep flowing", "b1")
		a.staysAt(t, 0, "a paused topic buffers rather than delivering")

		if err := q.ResumeTopic(ctx, "topic.a", "grp", "test"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		a.awaitLabels(t, "the held events to flush in publish order", "a1", "a2")
	})

	t.Run("pause_topic_holds_are_reason_scoped", func(t *testing.T) {
		t.Parallel()
		// Two independent subsystems gate the same inbox — the sandbox
		// busy gate and the config-divergence shed. With one flat hold the
		// sandbox resuming its own run would un-gate a node serving a
		// stale company, on a completely ordinary code path.
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "seat.inbox", "grp", recordingHandler(j))

		for _, reason := range []string{"sandbox", "config-divergence"} {
			if err := q.PauseTopic(ctx, "seat.inbox", "grp", reason); err != nil {
				t.Fatalf("PauseTopic(%s): %v", reason, err)
			}
		}
		publish(ctx, t, q, "seat.inbox", newEvent("work"))

		if err := q.ResumeTopic(ctx, "seat.inbox", "grp", "sandbox"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		j.staysAt(t, 0, "one subsystem released its hold and un-gated another's")

		if err := q.ResumeTopic(ctx, "seat.inbox", "grp", "config-divergence"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		j.awaitLabels(t, "the last hold to release the topic", "work")
	})

	t.Run("pause_topic_holds_are_keyed_by_the_pair", func(t *testing.T) {
		t.Parallel()
		// Keyed by topic alone, a hold gated every group on a shared
		// subject like crewlet.events.* — one seat's sandbox pause
		// silenced the fleet's routing.
		q := s.start(ctx, t)
		held, free := newJournal(), newJournal()
		subscribe(ctx, t, q, "crewlet.events.task_created", "held-grp", recordingHandler(held))
		subscribe(ctx, t, q, "crewlet.events.task_created", "free-grp", recordingHandler(free))

		if err := q.PauseTopic(ctx, "crewlet.events.task_created", "held-grp", "sandbox"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		publish(ctx, t, q, "crewlet.events.task_created", newEvent("t"))

		free.awaitLabels(t, "the unheld group's copy", "t")
		held.staysAt(t, 0, "the held group")
	})

	t.Run("resume_topic_unpaused_is_noop", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		// Releasing a hold that was never taken must not error.
		if err := q.ResumeTopic(ctx, "never.paused", "grp", "test"); err != nil {
			t.Fatalf("ResumeTopic on an unpaused subscription: %v", err)
		}
	})

	t.Run("pause_delivery_blocks_dispatch_but_accepts_publish", func(t *testing.T) {
		t.Parallel()
		// The graceful-shutdown contract: no new turns start (handlers
		// are paused) while in-flight ones complete and emit their
		// terminal events (publish still works).
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "topic.p", "grp", recordingHandler(j))

		publish(ctx, t, q, "topic.p", newEvent("before_pause"))
		j.awaitLabels(t, "the pre-pause delivery", "before_pause")

		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		publish(ctx, t, q, "topic.p", newEvent("after_pause"))
		j.staysAt(t, 1, "a paused queue dispatched")

		if history := s.caps.History; history != nil {
			// Accepted, not dispatched: the distinction is the whole
			// point of the pause being one-way and publish staying open.
			var found bool
			for _, ev := range history(q) {
				if labelOf(ev) == "after_pause" {
					found = true
				}
			}
			if !found {
				t.Fatalf("a publish after PauseDelivery was not accepted")
			}
		}
	})

	t.Run("pause_is_idempotent", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("second PauseDelivery: %v", err)
		}
	})

	t.Run("stop_clears_pause", func(t *testing.T) {
		t.Parallel()
		if !s.caps.Restartable {
			t.Skip("backend treats Stop as terminal; a restart needs a fresh queue")
		}
		q := s.start(ctx, t)
		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if err := q.Start(ctx); err != nil {
			t.Fatalf("restart: %v", err)
		}
		j := newJournal()
		subscribe(ctx, t, q, "topic.r", "grp", recordingHandler(j))
		publish(ctx, t, q, "topic.r", newEvent("resumed"))
		j.awaitLabels(t, "a restarted queue to deliver again", "resumed")
	})

	t.Run("a_hold_taken_while_stopped_does_not_survive_a_restart", func(t *testing.T) {
		t.Parallel()
		if !s.caps.Restartable {
			t.Skip("backend treats Stop as terminal; a restart needs a fresh queue")
		}
		// stop_clears_pause covers a hold taken BEFORE the stop. This covers
		// the window the suite never visited: a hold taken while the queue is
		// stopped, by a sandbox gate or a config shed racing a drain.
		//
		// Found by asking at which points in the queue's own lifecycle each
		// verb is sent — a different axis from what the suite sends. After a
		// Stop this suite sent exactly two things, Start and Publish, and
		// never the other nine verbs. Measured on the twin before the fix:
		// the hold survived, and the restarted seat was silently deaf while
		// reporting itself running, which is the incident Stop's own doc
		// exists to prevent, reached from the other side.
		q := s.start(ctx, t)
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := q.PauseTopic(ctx, "seat.restart", "grp", "sandbox"); err != nil {
			// Refusing while stopped is a legitimate answer and closes the
			// window just as well: the contract does not say what the verbs
			// other than Start and Stop do on a stopped queue, and both
			// readings are real — on JetStream, Open establishes the
			// connection and the streams, so Start is a no-op, while on the
			// twin Start is what makes the client live. What a backend may
			// NOT do is answer differently per verb, which is how this window
			// opened: the twin refuses Publish and Subscribe while
			// EnsureSubscription, DeleteSubscription and PauseTopic still
			// mutate broker state on a stopped client.
			t.Skipf("backend refuses PauseTopic while stopped: %v", err)
		}
		if err := q.Start(ctx); err != nil {
			t.Fatalf("restart: %v", err)
		}

		j := newJournal()
		subscribe(ctx, t, q, "seat.restart", "grp", recordingHandler(j))
		publish(ctx, t, q, "seat.restart", newEvent("work"))
		j.awaitLabels(t, "a restarted queue to serve rather than stay gated", "work")
	})

	t.Run("wait_for_handlers_no_op_when_idle", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		// quietFor here is only "a timeout that is not zero": an idle queue
		// returns without consuming any of it, so this assertion does not
		// depend on the length. It is not the positive-half dependency that
		// constant's doc warns against.
		remaining, err := q.WaitForHandlers(ctx, quietFor)
		if err != nil {
			t.Fatalf("WaitForHandlers: %v", err)
		}
		if remaining != 0 {
			t.Fatalf("WaitForHandlers on an idle queue = %d, want 0", remaining)
		}
	})

	t.Run("a_handler_may_start_while_a_drain_is_waiting", func(t *testing.T) {
		t.Parallel()
		// A DISPATCH LOOP STARTS HANDLERS AT MOMENTS NOBODY CHOSE.
		//
		// Two of the three backends counted with a sync.WaitGroup, whose
		// contract forbids exactly this: "calls with a positive delta that
		// start when the counter is zero must happen before a Wait". A
		// queue cannot honour that — a message arrives when it arrives —
		// and the consequence is not theoretical: Wait may return on a
		// momentary zero while a handler is starting, so a drain reports
		// clean and the process shuts down through a running handler.
		//
		// The suite could not see it because every other case here waits
		// on a QUIET queue. This one overlaps the two on purpose, and its
		// real assertion is the RACE DETECTOR: under -race, which CI runs
		// on everything, a WaitGroup implementation reports a data race on
		// itself here. Without -race it still exercises the interleaving
		// and asserts the counts stay sane.
		q := s.start(ctx, t)
		var handled atomic.Int64
		subscribe(ctx, t, q, "topic.overlap", "grp",
			func(context.Context, *events.Event) queue.Result {
				handled.Add(1)
				return queue.Ack()
			})

		var wg sync.WaitGroup
		wg.Go(func() {
			for i := range overlapRounds {
				_ = q.Publish(ctx, "topic.overlap", newEvent(fmt.Sprintf("m%d", i)))
			}
		})
		wg.Go(func() {
			for range overlapRounds {
				// A drain against a queue that is mostly idle, so most of
				// these start their wait at zero — the case the WaitGroup
				// contract rules out and a dispatch loop cannot avoid.
				if _, err := q.WaitForHandlers(ctx, 0); err != nil && ctx.Err() == nil {
					t.Errorf("WaitForHandlers during publishing: %v", err)
					return
				}
			}
		})
		wg.Wait()

		if got := q.InFlightCount(); got < 0 {
			t.Fatalf("the in-flight count went negative (%d), so a drain would "+
				"never converge", got)
		}
		remaining, err := q.WaitForHandlers(ctx, settleFor)
		if err != nil {
			t.Fatalf("WaitForHandlers after the overlap: %v", err)
		}
		if remaining != 0 {
			t.Fatalf("WaitForHandlers after everything finished = %d, want 0", remaining)
		}
	})

	t.Run("wait_for_handlers_drains_a_running_handler", func(t *testing.T) {
		t.Parallel()
		// The drain protocol an operator watches converge: pause, then
		// wait. A non-zero return is a timeout, not an error — the caller
		// owns any "too long" policy.
		q := s.start(ctx, t)
		entered := make(chan struct{})
		release := make(chan struct{})
		var once bool
		subscribe(ctx, t, q, "topic.drain", "grp", func(context.Context, *events.Event) queue.Result {
			if !once {
				once = true
				close(entered)
				<-release
			}
			return queue.Ack()
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = q.Publish(ctx, "topic.drain", newEvent("slow"))
		}()
		awaitSignal(t, entered, "the handler to start", func() { close(release) })

		if got := q.InFlightCount(); got != 1 {
			t.Fatalf("InFlightCount during a handler = %d, want 1", got)
		}
		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		remaining, err := q.WaitForHandlers(ctx, quietFor)
		if err != nil {
			t.Fatalf("WaitForHandlers: %v", err)
		}
		if remaining != 1 {
			t.Fatalf("WaitForHandlers with a handler mid-flight = %d, want 1", remaining)
		}

		close(release)
		awaitSignal(t, done, "the publish to return", func() {})
		remaining, err = q.WaitForHandlers(ctx, settleFor)
		if err != nil {
			t.Fatalf("WaitForHandlers after release: %v", err)
		}
		if remaining != 0 {
			t.Fatalf("WaitForHandlers after the handler returned = %d, want 0", remaining)
		}
		if got := q.InFlightCount(); got != 0 {
			t.Fatalf("InFlightCount after the drain = %d, want 0", got)
		}
	})

	t.Run("idempotent_start_stop", func(t *testing.T) {
		t.Parallel()
		q := s.newQueue(t)
		for range 2 {
			if err := q.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
		}
		for range 2 {
			if err := q.Stop(ctx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		}
	})

	// THE ELEVEN VERBS, AT THE TWO POINTS A QUEUE IS NOT LIVE.
	//
	// Nine of them had never been sent at either point by any case in this
	// suite, and both backends were internally inconsistent there. The twin
	// refused Publish and Subscribe while EnsureSubscription created durable
	// broker state, DeleteSubscription destroyed it and PauseTopic took a
	// hold that outlived the stop and left the next life silently deaf.
	// JetStream refused everything that touched the broker and returned
	// SUCCESS from Quiesce, Unquiesce, Detach, PauseTopic and ResumeTopic on
	// a closed connection.
	//
	// The two points get different rules, and the asymmetry is deliberate —
	// see queue.EventQueue's Start and Stop.

	// BEFORE START a backend picks its answer and applies it to all eleven.
	t.Run("an_unstarted_queue_answers_the_same_way_for_every_verb", func(t *testing.T) {
		t.Parallel()
		q := s.newQueue(t)
		for _, verb := range lifecycleVerbs(ctx, q, "unstarted") {
			switch {
			case s.caps.RequiresStart && verb.err == nil:
				t.Errorf("%s accepted before Start; this backend requires "+
					"Start, so every verb must refuse — a caller cannot "+
					"reason about a lifecycle whose rules change per method",
					verb.name)
			case !s.caps.RequiresStart && verb.err != nil:
				t.Errorf("%s refused before Start (%v); this backend does "+
					"not require Start, so no verb may demand it",
					verb.name, verb.err)
			}
		}
	})

	// AFTER STOP there is no choice: a closed client mutates nothing.
	t.Run("a_stopped_queue_refuses_every_verb", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		for _, verb := range lifecycleVerbs(ctx, q, "stopped") {
			if verb.err == nil {
				t.Errorf("%s succeeded on a stopped queue; whatever it did "+
					"was done through a closed client, and any state it left "+
					"behind outlives the queue that took it", verb.name)
				continue
			}
			// AND IT REFUSES WITH THE CONTRACT'S SENTINEL, not only its
			// own. A seat release reads this to tell "the mailbox is
			// already down" from "the detach failed" — and it may not ask
			// which backend is running to find out. A backend that wrapped
			// its own error alone would send the seat host down the second
			// reading, which KEEPS THE LEASE.
			if !errors.Is(verb.err, queue.ErrNotLive) {
				t.Errorf("%s refused with %v, which is not queue.ErrNotLive; "+
					"a caller above the queue cannot tell a torn-down mailbox "+
					"from a failed teardown without branching on the backend",
					verb.name, verb.err)
			}
		}
	})

	t.Run("start_stop_lifecycle", func(t *testing.T) {
		t.Parallel()
		if !s.caps.RequiresStart {
			t.Skip("backend accepts a publish before Start")
		}
		q := s.newQueue(t)
		if err := q.Publish(ctx, "t", newEvent("t")); err == nil {
			t.Fatalf("Publish before Start returned no error")
		}
		if err := q.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := q.Publish(ctx, "t", newEvent("t")); err != nil {
			t.Fatalf("Publish after Start: %v", err)
		}
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := q.Publish(ctx, "t", newEvent("t")); err == nil {
			t.Fatalf("Publish after Stop returned no error")
		}
	})

	t.Run("backend_name_is_stable_and_lowercase", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		name := q.Backend()
		if name == "" {
			t.Fatalf("Backend() is empty; operators have nothing to display")
		}
		for _, r := range name {
			if r >= 'A' && r <= 'Z' {
				t.Fatalf("Backend() = %q, want lowercase", name)
			}
		}
	})

	t.Run("publish_listener_sees_every_publish", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		seen := newJournal()
		q.AddPublishListener(func(_ context.Context, topic string, ev *events.Event) {
			seen.record(topic + "/" + labelOf(ev))
		})
		publish(ctx, t, q, "topic.listen", newEvent("e1"))
		// A topic with no subscription still reaches listeners: the event
		// store must record what was published, not what was consumed.
		publish(ctx, t, q, "nobody.listening", newEvent("e2"))

		seen.awaitLabels(t, "the listener to see both publishes",
			"topic.listen/e1", "nobody.listening/e2")
	})

	t.Run("publish_listener_failure_does_not_prevent_delivery", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		j := newJournal()
		q.AddPublishListener(func(context.Context, string, *events.Event) {
			panic("listener crashed")
		})
		subscribe(ctx, t, q, "topic.listen", "grp", recordingHandler(j))
		publish(ctx, t, q, "topic.listen", newEvent("e1"))

		j.awaitLabels(t, "delivery to survive a broken listener", "e1")
	})
}

// verbResult is one contract method's answer at one point in a queue's life.
type verbResult struct {
	name string
	err  error
}

// second returns the error half of a (bool, error) verb, so the lifecycle
// table can hold every shape of the contract's methods in one list.
func second(_ bool, err error) error { return err }

// lifecycleVerbs sends every publish, subscription and attachment verb once
// and reports what each answered.
//
// ALL ELEVEN, in one list, because the property under test is that they AGREE
// — a helper that took a subset would be certifying the subset the author
// happened to think of, which is exactly how nine of them went unsent.
//
// The drain trio is absent on purpose: PauseDelivery, WaitForHandlers and
// InFlightCount exist to run around a stop, so they answer on a stopped queue
// and must. Backend and AddPublishListener return no error to refuse with.
func lifecycleVerbs(ctx context.Context, q queue.EventQueue, ns string) []verbResult {
	topic := ns + ".t"
	unsub, streamErr := q.SubscribeStream(ctx, ns+".>", func(context.Context, string, *events.Event) {})
	if unsub != nil {
		// Released immediately: a subscription this probe left behind would
		// deliver into a handler the case has already returned from.
		_ = unsub(ctx)
	}
	return []verbResult{
		{"Publish", q.Publish(ctx, topic, newEvent(topic))},
		{"Subscribe", q.Subscribe(ctx, topic, "g", func(context.Context, *events.Event) queue.Result {
			return queue.Ack()
		})},
		{"SubscribeBatch", q.SubscribeBatch(ctx, topic, "g", func(context.Context, []*events.Event) queue.Result {
			return queue.Ack()
		}, nil, nil)},
		{"Quiesce", second(q.Quiesce(ctx, topic, "g"))},
		{"Unquiesce", second(q.Unquiesce(ctx, topic, "g"))},
		{"Detach", second(q.Detach(ctx, topic, "g"))},
		{"EnsureSubscription", second(q.EnsureSubscription(ctx, topic, "g"))},
		{"DeleteSubscription", second(q.DeleteSubscription(ctx, topic, "g"))},
		{"SubscribeStream", streamErr},
		{"PauseTopic", q.PauseTopic(ctx, topic, "g", "test")},
		{"ResumeTopic", q.ResumeTopic(ctx, topic, "g", "test")},
	}
}
