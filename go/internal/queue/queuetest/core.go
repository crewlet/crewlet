package queuetest

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runCore covers the properties every producer and consumer depends on:
// delivery, competition inside a group, isolation between groups, the durable
// mailbox, redelivery and its dead-letter floor, and the graceful-drain
// protocol.
func (s *suite) runCore(t *testing.T) {
	ctx := context.Background()

	t.Run("publish_subscribe", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		j := newJournal()
		subscribe(t, q, "topic.a", "grp1", recordingHandler(j))

		ev := newEvent("test_event")
		publish(t, q, "topic.a", ev)

		j.awaitLabels(t, "the published event", "test_event")
	})

	t.Run("multiple_subscribers_different_topics", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		a, b := newJournal(), newJournal()
		subscribe(t, q, "topic.a", "g", recordingHandler(a))
		subscribe(t, q, "topic.b", "g", recordingHandler(b))

		publish(t, q, "topic.a", newEvent("a"))
		publish(t, q, "topic.b", newEvent("b"))

		a.awaitLabels(t, "topic.a's subscriber", "a")
		b.awaitLabels(t, "topic.b's subscriber", "b")
	})

	t.Run("multiple_groups_receive_same_event", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		g1, g2 := newJournal(), newJournal()
		subscribe(t, q, "topic.y", "g1", recordingHandler(g1))
		subscribe(t, q, "topic.y", "g2", recordingHandler(g2))

		publish(t, q, "topic.y", newEvent("t"))

		// A copy each: competition happens BETWEEN the members of one
		// group, never across groups.
		g1.awaitLabels(t, "group g1's copy", "t")
		g2.awaitLabels(t, "group g2's copy", "t")
	})

	t.Run("consumer_group_competing", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		shared := newJournal()
		subscribe(t, q, "topic.x", "grp", recordingHandler(shared))
		subscribe(t, q, "topic.x", "grp", recordingHandler(shared))

		publish(t, q, "topic.x", newEvent("t"))

		shared.await(t, "exactly one member to take the event",
			func(seen []string) bool { return len(seen) == 1 })
		shared.staysAt(t, 1, "one event delivered to a two-member group")
	})

	t.Run("members_of_a_group_compete", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		// Each member journals its own name, so the sequence records WHO
		// was chosen. Delivering always to the first-registered member
		// made a double-attach split-brain invisible: two nodes consuming
		// one seat looked like one, so "exactly one delivery" passed
		// while a real Shared subscription split the traffic and ran two
		// interleaved turn streams.
		by := newJournal()
		subscribe(t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			by.record("a")
			return queue.Ack()
		})
		subscribe(t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			by.record("b")
			return queue.Ack()
		})

		for range 4 {
			publish(t, q, "topic", newEvent("t"))
		}

		if s.caps.StrictRoundRobin {
			by.awaitLabels(t, "strict round-robin across the group", "a", "b", "a", "b")
			return
		}
		by.await(t, "the group to share four events", func(seen []string) bool {
			var a, b int
			for _, who := range seen {
				switch who {
				case "a":
					a++
				case "b":
					b++
				}
			}
			return a+b == 4 && a > 0 && b > 0
		})
	})

	t.Run("subscribe_after_start", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		j := newJournal()
		subscribe(t, q, "topic.late", "grp", recordingHandler(j))
		publish(t, q, "topic.late", newEvent("late"))
		j.awaitLabels(t, "a subscription added after start", "late")
	})

	t.Run("an_unowned_subscription_holds_its_mail", func(t *testing.T) {
		t.Parallel()
		// The property seat ownership rests on. A subscription with
		// nothing attached retains what is published to it and replays it
		// on attach; without this a seat between owners loses every event
		// published in the gap.
		q := s.start(t)
		const topic, group = "crewlet.agent.alice.inbox", "agent-alice"
		created, err := q.EnsureSubscription(ctx, topic, group)
		if err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if !created {
			t.Fatalf("EnsureSubscription reported the subscription already existed")
		}
		for _, label := range []string{"e0", "e1", "e2"} {
			publish(t, q, topic, newEvent(label))
		}
		if backlog := s.caps.Backlog; backlog != nil {
			awaitState(t, "three retained events", func() bool {
				return len(backlog(q, topic, group)) == 3
			})
		}

		j := newJournal()
		subscribe(t, q, topic, group, recordingHandler(j))
		j.awaitLabels(t, "the retained mail to replay in order", "e0", "e1", "e2")
	})

	t.Run("publishing_to_no_subscription_retains_nothing", func(t *testing.T) {
		t.Parallel()
		// Which is exactly why EnsureSubscription exists.
		backlog := s.needBacklog(t)
		q := s.start(t)
		publish(t, q, "nobody.listening", newEvent("lost"))
		if got := backlog(q, "nobody.listening", "grp"); len(got) != 0 {
			t.Fatalf("a topic with no subscription retained %v", labelsOf(got))
		}
	})

	t.Run("ensure_subscription_reports_creation", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
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
		q := s.start(t)
		if _, err := q.EnsureSubscription(ctx, "topic.gone", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(t, q, "topic.gone", newEvent("e1"))
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

		publish(t, q, "topic.gone", newEvent("e2"))
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
		q := s.start(t)
		j := newJournal()
		var attempts int
		subscribe(t, q, "topic.flaky", "grp", func(context.Context, *events.Event) queue.Result {
			attempts++
			j.record("attempt")
			if attempts < 3 {
				return queue.Nak(errors.New("transient failure"))
			}
			return queue.Ack()
		})

		publish(t, q, "topic.flaky", newEvent("t"))

		j.await(t, "two failures and a success", func(seen []string) bool { return len(seen) == 3 })
		j.staysAt(t, 3, "an acknowledged event")
	})

	t.Run("nak_returns_the_event_to_the_front_of_the_mailbox", func(t *testing.T) {
		t.Parallel()
		if !s.caps.HeadReplayOnNak {
			t.Skip("backend redelivers behind never-delivered events")
		}
		q := s.start(t)
		seen := newJournal()
		var failed bool
		subscribe(t, q, "topic.head", "grp", func(_ context.Context, ev *events.Event) queue.Result {
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
		publish(t, q, "topic.head", newEvent("e1"))
		publish(t, q, "topic.head", newEvent("e2"))
		if err := q.ResumeTopic(ctx, "topic.head", "grp", "test"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}

		seen.awaitLabels(t, "the redelivery to come back ahead of e2", "e1", "e1", "e2")
	})

	t.Run("redelivery_rotates_across_members", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		by := newJournal()
		subscribe(t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			by.record("a")
			return queue.Nak(errors.New("fail"))
		})
		subscribe(t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			by.record("b")
			return queue.Ack()
		})

		publish(t, q, "topic", newEvent("t"))

		if s.caps.StrictRoundRobin {
			// First delivery goes to member 0 (fails); the redelivery
			// advances the cursor to member 1, which succeeds.
			by.awaitLabels(t, "redelivery to move to the next member", "a", "b")
			return
		}
		by.await(t, "a healthy member to take the redelivery", func(seen []string) bool {
			for _, who := range seen {
				if who == "b" {
					return true
				}
			}
			return false
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
		newBudgeted := s.needBudget(t)
		deadLetters := s.needDeadLetters(t)
		backlog := s.needBacklog(t)
		q := startQueue(t, newBudgeted(t, 2))

		j := newJournal()
		subscribe(t, q, "topic", "grp", func(context.Context, *events.Event) queue.Result {
			j.record("attempt")
			return queue.Nak(errors.New("permanent failure"))
		})
		publish(t, q, "topic", newEvent("t"))

		j.await(t, "first delivery plus two redeliveries",
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
		q := s.start(t)

		j := newJournal()
		subscribe(t, q, "topic.d", "grp", func(_ context.Context, ev *events.Event) queue.Result {
			j.record(labelOf(ev))
			return queue.Defer("lease moved")
		})
		publish(t, q, "topic.d", newEvent("e1"))
		publish(t, q, "topic.d", newEvent("e2"))

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
		q := s.start(t)
		a, b := newJournal(), newJournal()
		subscribe(t, q, "topic.a", "grp", recordingHandler(a))
		subscribe(t, q, "topic.b", "grp", recordingHandler(b))

		if err := q.PauseTopic(ctx, "topic.a", "grp", "test"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		publish(t, q, "topic.a", newEvent("a1"))
		publish(t, q, "topic.a", newEvent("a2"))
		// A different topic is unaffected while topic.a is held.
		publish(t, q, "topic.b", newEvent("b1"))

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
		q := s.start(t)
		j := newJournal()
		subscribe(t, q, "seat.inbox", "grp", recordingHandler(j))

		for _, reason := range []string{"sandbox", "config-divergence"} {
			if err := q.PauseTopic(ctx, "seat.inbox", "grp", reason); err != nil {
				t.Fatalf("PauseTopic(%s): %v", reason, err)
			}
		}
		publish(t, q, "seat.inbox", newEvent("work"))

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
		q := s.start(t)
		held, free := newJournal(), newJournal()
		subscribe(t, q, "crewlet.events.task_created", "held-grp", recordingHandler(held))
		subscribe(t, q, "crewlet.events.task_created", "free-grp", recordingHandler(free))

		if err := q.PauseTopic(ctx, "crewlet.events.task_created", "held-grp", "sandbox"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		publish(t, q, "crewlet.events.task_created", newEvent("t"))

		free.awaitLabels(t, "the unheld group's copy", "t")
		held.staysAt(t, 0, "the held group")
	})

	t.Run("resume_topic_unpaused_is_noop", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
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
		q := s.start(t)
		j := newJournal()
		subscribe(t, q, "topic.p", "grp", recordingHandler(j))

		publish(t, q, "topic.p", newEvent("before_pause"))
		j.awaitLabels(t, "the pre-pause delivery", "before_pause")

		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		publish(t, q, "topic.p", newEvent("after_pause"))
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
		q := s.start(t)
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
		q := s.start(t)
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
		subscribe(t, q, "topic.r", "grp", recordingHandler(j))
		publish(t, q, "topic.r", newEvent("resumed"))
		j.awaitLabels(t, "a restarted queue to deliver again", "resumed")
	})

	t.Run("wait_for_handlers_no_op_when_idle", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		remaining, err := q.WaitForHandlers(ctx, quietFor)
		if err != nil {
			t.Fatalf("WaitForHandlers: %v", err)
		}
		if remaining != 0 {
			t.Fatalf("WaitForHandlers on an idle queue = %d, want 0", remaining)
		}
	})

	t.Run("wait_for_handlers_drains_a_running_handler", func(t *testing.T) {
		t.Parallel()
		// The drain protocol an operator watches converge: pause, then
		// wait. A non-zero return is a timeout, not an error — the caller
		// owns any "too long" policy.
		q := s.start(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		var once bool
		subscribe(t, q, "topic.drain", "grp", func(context.Context, *events.Event) queue.Result {
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

	t.Run("start_stop_lifecycle", func(t *testing.T) {
		t.Parallel()
		if !s.caps.RejectsPublishBeforeStart {
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
		q := s.start(t)
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
		q := s.start(t)
		seen := newJournal()
		q.AddPublishListener(func(_ context.Context, topic string, ev *events.Event) {
			seen.record(topic + "/" + labelOf(ev))
		})
		publish(t, q, "topic.listen", newEvent("e1"))
		// A topic with no subscription still reaches listeners: the event
		// store must record what was published, not what was consumed.
		publish(t, q, "nobody.listening", newEvent("e2"))

		seen.awaitLabels(t, "the listener to see both publishes",
			"topic.listen/e1", "nobody.listening/e2")
	})

	t.Run("publish_listener_failure_does_not_prevent_delivery", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		j := newJournal()
		q.AddPublishListener(func(context.Context, string, *events.Event) {
			panic("listener crashed")
		})
		subscribe(t, q, "topic.listen", "grp", recordingHandler(j))
		publish(t, q, "topic.listen", newEvent("e1"))

		j.awaitLabels(t, "delivery to survive a broken listener", "e1")
	})
}
