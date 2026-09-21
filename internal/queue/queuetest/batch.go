package queuetest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runBatch covers batched, key-partitioned delivery: the drain, the
// partitioning, per-partition acking, and the places a batch loop has to stop
// early.
//
// THE EARLY STOPS ARE AN ENUMERATION, not a pair, and this comment said "the
// two" for as long as only two of them were asked about. The contract names
// four conditions a partition loop must answer BETWEEN partitions — a deferral
// it just applied, a hold, a drain pause, a detach (see queue.DeliveriesLeft)
// — and there is one case per condition below, plus the linger window closing
// on an attachment that has since been paused or held, which is a different
// gate in both backends. A fifth thing every one of them shares is what the
// undispatched remainder COSTS, which is its own case again.
func (s *suite) runBatch(t *testing.T) {
	ctx := t.Context()

	t.Run("zero_linger_dispatches_inline_single_event_batches", func(t *testing.T) {
		t.Parallel()
		if !s.caps.InlineDispatch {
			t.Skip("backend has fetch latency, so batch boundaries are not deterministic")
		}
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches), queue.DefaultBatchOptions())

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		publish(ctx, t, q, "t", newConvEvent("b", "c1"))

		// Zero linger keeps publishes synchronous: each event arrives as
		// its own one-element batch, before Publish returns.
		batches.awaitSizes(t, "one batch per publish", 1, 1)
	})

	t.Run("a_negative_linger_behaves_as_no_linger", func(t *testing.T) {
		t.Parallel()
		// The linger clamp has two halves and exactly one is reachable from
		// here. The CEILING (60s) is not: this suite may never depend on a
		// window above it, so no behaviour above it is observable, and unlike
		// a lease deadline a linger has no reported value to compare — the
		// only observable is elapsed time, so probing it would cost a test
		// that sleeps for a minute. That half stays a documented gap.
		//
		// The FLOOR is reachable and costs nothing: a negative window must
		// behave as no window at all. A backend handing a negative duration
		// straight to a broker API is entitled to nothing in particular from
		// it, which is exactly why the contract clamps and why a backend that
		// forwards the raw value should be caught here rather than in
		// production.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t.neg", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(-5, 20))

		publish(ctx, t, q, "t.neg", newConvEvent("a", "c1"))
		batches.await(t, "delivery under a negative linger",
			func(got [][]string) bool { return len(got) == 1 && equalStrings(got[0], []string{"a"}) })
	})

	t.Run("linger_coalesces_same_key_events_into_one_batch", func(t *testing.T) {
		t.Parallel()
		// The property inbox batching exists for: events that queued
		// while an agent was busy must arrive as ONE turn, not N.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		publish(ctx, t, q, "t", newConvEvent("b", "c1"))

		batches.await(t, "one coalesced batch", func(got [][]string) bool {
			return len(got) == 1 && equalStrings(got[0], []string{"a", "b"})
		})
	})

	t.Run("linger_partitions_by_key_preserving_arrival_order", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		publish(ctx, t, q, "t", newConvEvent("b", "c2"))
		publish(ctx, t, q, "t", newConvEvent("c", "c1"))

		// Two partitions, dispatched oldest-constituent-first (here the
		// same as first arrival, since timestamps follow publish order),
		// and c1 keeps both its events in publish order.
		batches.await(t, "two partitions in arrival order", func(got [][]string) bool {
			return len(got) == 2 &&
				equalStrings(got[0], []string{"a", "c"}) &&
				equalStrings(got[1], []string{"b"})
		})
	})

	t.Run("max_batch_chunks_oversized_buffers", func(t *testing.T) {
		t.Parallel()
		// A pathological backlog is delivered as successive capped
		// batches rather than one unbounded one.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 2))

		for i := range 5 {
			publish(ctx, t, q, "t", newConvEvent(string(rune('0'+i)), "c1"))
		}
		batches.awaitSizes(t, "the backlog to arrive as capped batches", 2, 2, 1)
	})

	t.Run("a_batch_handler_is_told_how_many_deliveries_are_left", func(t *testing.T) {
		t.Parallel()
		// The same number a single delivery carries — see
		// a_handler_is_told_how_many_deliveries_are_left — because a
		// partition's handler spends the budget the same way: one
		// outcome, applied to every message it was handed.
		//
		// A partition whose messages sit at DIFFERENT counts is the next
		// case, which is where the two numbers a handler is told stop
		// being the same number.
		newQueueWithAttempts := s.needAttempts(t)
		q := startQueue(ctx, t, newQueueWithAttempts(t, 3))

		j := newJournal()
		var attempts int
		subscribeBatch(ctx, t, q, "t.left", "g",
			func(hctx context.Context, _ []*events.Event) queue.Result {
				attempts++
				left, known := queue.DeliveriesLeft(hctx)
				if !known {
					j.record("unknown")
					return queue.Ack()
				}
				j.record(strconv.Itoa(left))
				if attempts < 3 {
					return queue.Nak(errors.New("still failing"))
				}
				return queue.Ack()
			}, queue.DefaultBatchOptions())
		publish(ctx, t, q, "t.left", newConvEvent("a", "c1"))

		j.awaitLabels(t, "the partition's headroom to count down", "2", "1", "0")
	})

	t.Run("a_partition_at_mixed_counts_tells_each_message_its_own_headroom", func(t *testing.T) {
		t.Parallel()
		// TWO NUMBERS, BECAUSE ONE CANNOT ANSWER BOTH QUESTIONS.
		//
		// A partition's messages sit at different delivery counts as a
		// matter of course: a conversation whose first message has been
		// handed back keeps collecting fresh replies, and each of those
		// arrives with a whole budget. So a handler is told BOTH — the
		// partition's own headroom ([queue.DeliveriesLeft], the smallest,
		// which answers "will handing this batch back dead-letter
		// something") and each message's ([queue.DeliveriesLeftFor],
		// which answers "how much is left of THIS one").
		//
		// Reading the first where the second is meant is a defect with a
		// name: the engine's sandbox answer route gated its offer on the
		// partition's number and so refused a person's clarification
		// reply that was on its FIRST delivery, because an older message
		// on the same conversation was near its own budget — and kept
		// refusing every later reply on that conversation, since the
		// spent message stays in the partition. See
		// internal/sandbox.MayOfferAnswer.
		//
		// HOW THE MIXED PARTITION IS BUILT, since the previous round of
		// this work recorded that it could not be. It needs a redelivery
		// and a never-delivered message to meet in ONE drain, and the two
		// halves that make that deterministic are:
		//
		//   - the fresh message is published BY THE HANDLER, on its first
		//     invocation, before that invocation hands the first message
		//     back. On an inline-dispatch twin that is the only ordering
		//     that works at all: nothing else runs between the nak and
		//     the next chunk, so a publish from the test goroutine can
		//     never land in between. On a fetching backend it puts the
		//     fresh message in the stream before the nak, so it is
		//     already available when the next drain opens its window.
		//   - the linger window is [mixedCountLinger] rather than
		//     lingerFor, so it outlasts the nak spacing the redelivered
		//     half comes back on instead of racing it.
		//
		// The assertion is RELATIONAL rather than two literals, because
		// a backend is entitled to an extra redelivery on the way here
		// and the rule does not depend on how many: the two counts must
		// DIFFER, and the partition's must be the smaller. On the common
		// path the numbers are attempts-2 and attempts-1.
		newQueueWithAttempts := s.needAttempts(t)
		const attempts = 6
		q := startQueue(ctx, t, newQueueWithAttempts(t, attempts))

		const topic, group, conv = "t.mixed", "g", "c1"
		older := newConvEvent("older", conv)
		fresher := newConvEvent("fresher", conv)

		var (
			mu                 sync.Mutex
			seeded             bool
			rounds             int
			readings           map[string]int
			partLeft           int
			partKnown, missing bool
		)
		j := newJournal()
		subscribeBatch(ctx, t, q, topic, group,
			func(hctx context.Context, evs []*events.Event) queue.Result {
				mu.Lock()
				first := !seeded
				seeded = true
				rounds++
				round := rounds
				mu.Unlock()

				if first {
					// Published from INSIDE the handler; see above.
					// A failure here is recorded rather than
					// fataled: this is not the test goroutine.
					if err := q.Publish(hctx, topic, fresher); err != nil {
						j.record("publish failed: " + err.Error())
						return queue.Ack()
					}
					return queue.Nak(errors.New("hand the first one back"))
				}
				if len(evs) < 2 {
					// They have not met yet. Hand it back and let
					// the next drain try, bounded by the budget so
					// a backend that never pairs them fails loudly
					// instead of hanging.
					if round < attempts-1 {
						return queue.Nak(errors.New("still waiting for the pair"))
					}
					mu.Lock()
					missing = true
					mu.Unlock()
					j.record("never met")
					return queue.Ack()
				}

				got := make(map[string]int, len(evs))
				var unstated bool
				for _, ev := range evs {
					left, known := queue.DeliveriesLeftFor(hctx, ev.ID)
					if !known {
						unstated = true
						continue
					}
					got[labelOf(ev)] = left
				}
				left, known := queue.DeliveriesLeft(hctx)

				mu.Lock()
				readings, partLeft, partKnown, missing = got, left, known, unstated
				mu.Unlock()
				j.record("paired")
				return queue.Ack()
			}, queue.NewBatchOptions(mixedCountLinger.Seconds(), 20))

		publish(ctx, t, q, topic, older)
		j.await(t, "a redelivery and a fresh publish to reach one handler together",
			func(seen []string) bool { return len(seen) == 1 })

		mu.Lock()
		defer mu.Unlock()
		if missing {
			t.Fatalf("the partition never carried both messages with both counts "+
				"stated (saw %v): a handler cannot ask what one message has left "+
				"if the backend states nothing for it", j.all())
		}
		olderLeft, olderOK := readings["older"]
		fresherLeft, fresherOK := readings["fresher"]
		if !olderOK || !fresherOK {
			t.Fatalf("the handler read %v, want a count for each of the two "+
				"messages in the partition", readings)
		}
		if olderLeft >= fresherLeft {
			t.Errorf("the redelivered message reported %d deliveries left and the "+
				"never-delivered one %d: a per-message count that cannot tell them "+
				"apart is the partition's number wearing another name, which is what "+
				"refused a reply with a whole budget in hand",
				olderLeft, fresherLeft)
		}
		if !partKnown || partLeft != olderLeft {
			t.Errorf("the partition reported (%d, %v), want (%d, true): one outcome "+
				"covers every message, so the partition's headroom is its NEAREST "+
				"message's", partLeft, partKnown, olderLeft)
		}
		if fresherLeft > attempts-1 {
			t.Errorf("a never-delivered message reported %d deliveries left on a "+
				"%d-attempt budget: the count is the headroom AFTER the delivery in "+
				"hand", fresherLeft, attempts)
		}
	})

	t.Run("failing_partition_redelivers_only_itself", func(t *testing.T) {
		t.Parallel()
		// A handler failure on one conversation must not replay or block
		// the other conversation from the same flush.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := startQueue(ctx, t, newQueueWithAttempts(t, 3))

		attempts := newJournal()
		if err := q.SubscribeBatch(ctx, "t", "g",
			func(_ context.Context, evs []*events.Event) queue.Result {
				conv := firstConv(t, evs)
				attempts.record(conv)
				if conv == "c1" {
					return queue.Nak(errors.New("boom"))
				}
				return queue.Ack()
			}, convKey, queue.NewBatchOptions(lingerFor.Seconds(), 20)); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		publish(ctx, t, q, "t", newConvEvent("b", "c2"))

		attempts.await(t, "c1 to exhaust its budget while c2 runs once", func(seen []string) bool {
			var c1, c2 int
			for _, conv := range seen {
				switch conv {
				case "c1":
					c1++
				case "c2":
					c2++
				}
			}
			return c1 == 3 && c2 == 1
		})
		attempts.staysAt(t, 4, "a dead-lettered partition kept being redelivered")
		awaitState(t, "c1's event to reach the dead-letter subject", func() bool {
			return len(deadLetters(q, "t", "g")) == 1
		})
	})

	t.Run("batch_key_failure_falls_back_to_unique_key", func(t *testing.T) {
		t.Parallel()
		// Key derivation must never block delivery — a key function that
		// blows up degrades to per-event partitions.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		if err := q.SubscribeBatch(ctx, "t", "g",
			func(_ context.Context, evs []*events.Event) queue.Result {
				batches.record(evs)
				return queue.Ack()
			},
			func(*events.Event) string { panic("no key for you") },
			queue.NewBatchOptions(lingerFor.Seconds(), 20)); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		publish(ctx, t, q, "t", newEvent("a"))
		publish(ctx, t, q, "t", newEvent("b"))

		batches.awaitSizes(t, "each event to become its own partition", 1, 1)
	})

	t.Run("live_options_mutation_takes_effect_next_cycle", func(t *testing.T) {
		t.Parallel()
		// A hot config reload changes linger and batch size with no
		// re-subscription: the consume loop re-reads the options every
		// cycle.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		opts := queue.NewBatchOptions(0, 20)
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches), opts)

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		batches.awaitSizes(t, "the un-lingered first event", 1)

		opts.Set(lingerFor.Seconds(), 20)
		publish(ctx, t, q, "t", newConvEvent("b", "c1"))
		publish(ctx, t, q, "t", newConvEvent("c", "c1"))
		batches.awaitSizes(t, "the next cycle to honour the new linger", 1, 2)
	})

	t.Run("stop_cancels_pending_lingered_flush", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(5.0, 20))

		setup := time.Now()
		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		setupTook := time.Since(setup)
		batches.staysAtRacing(t, 0, "a stopped queue flushed its linger window",
			5*time.Second, setupTook)

		// RETAINED, not dropped: the linger buffer IS the backlog, and a
		// subscription's mail outlives every attachment.
		if got := labelsOf(backlog(q, "t", "g")); !equalStrings(got, []string{"a"}) {
			t.Fatalf("backlog after Stop = %v, want [a]", got)
		}
	})

	t.Run("pause_during_linger_retains_pending", func(t *testing.T) {
		t.Parallel()
		// Dropping them meant a graceful drain silently destroyed
		// whatever was mid-window.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(racingWindow.Seconds(), 20))

		// The window must be OPEN when the pause lands and must EXPIRE while
		// it is held — no ordering removes that, so this case buys slack with
		// a longer window rather than pretending the race is gone.
		setup := time.Now()
		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		setupTook := time.Since(setup)
		time.Sleep(racingWindow + quietFor)
		batches.staysAtRacing(t, 0, "a paused queue flushed its linger window",
			racingWindow, setupTook)

		if got := labelsOf(backlog(q, "t", "g")); !equalStrings(got, []string{"a"}) {
			t.Fatalf("backlog after a pause mid-window = %v, want [a]", got)
		}
	})

	t.Run("a_hold_during_linger_is_released_in_place", func(t *testing.T) {
		t.Parallel()
		// The reversible half of the case above, and the half that was
		// missing. PauseDelivery is one-way — the node is shutting down —
		// so a backend may legitimately stop consuming for good. A
		// PauseTopic HOLD is the opposite: ResumeTopic clears it and the
		// attachment is expected to carry on.
		//
		// A backend that ends its consume loop on a hold taken during a
		// linger window leaves the seat ATTACHED, owning its lease, and
		// reading nothing for the rest of the process's life. Nothing
		// detects that — the seat looks owned and healthy while its mail
		// accumulates — which is why it survived: every other assertion
		// about holds pauses BEFORE anything is in the window, and the
		// single-event path answers the same condition correctly.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t.hold", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(racingWindow.Seconds(), 20))

		// Open the window, then take the hold while it is open.
		publish(ctx, t, q, "t.hold", newConvEvent("during", "c1"))
		if err := q.PauseTopic(ctx, "t.hold", "g", "test"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		time.Sleep(racingWindow + quietFor)
		batches.staysAt(t, 0, "a held attachment dispatched its window")

		if err := q.ResumeTopic(ctx, "t.hold", "g", "test"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		batches.awaitSizes(t, "the held batch once the hold lifts", 1)

		// And the attachment is still ALIVE, not merely flushed once:
		// something published after the resume must arrive too.
		publish(ctx, t, q, "t.hold", newConvEvent("after", "c1"))
		batches.awaitSizes(t, "an event published after the hold lifted", 1, 1)
	})

	t.Run("an_aged_conversation_dispatches_before_a_fresher_one", func(t *testing.T) {
		t.Parallel()
		// Between-partition fairness, asserted against the BACKEND rather
		// than against the ordering function.
		//
		// Receive order alone starves a quiet conversation behind a hot
		// one under deferral: the quiet conversation's requeued copies
		// re-enter the topic AFTER whatever arrived during the hot
		// conversation's turn, so receive-ordered dispatch picks the hot
		// one on every drain, for ever. Timestamps carry the aging signal
		// and survive requeue, so the conversation that has waited longest
		// must go first.
		//
		// Every other batch subtest publishes in timestamp order, which
		// makes oldest-first and receive-order indistinguishable — so this
		// deliberately puts the two in conflict: the hot conversation
		// ARRIVES first and the quiet one is OLDER. Measured, a backend
		// that partitions correctly and dispatches in receive order passes
		// the whole suite without this case.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "topic.age", "grp", recordingBatchHandler(batches),
			queue.DefaultBatchOptions())

		now := time.Now().UTC()
		hot := newConvEvent("hot", "hot")
		hot.Timestamp = now
		quiet := newConvEvent("quiet", "quiet")
		quiet.Timestamp = now.Add(-time.Minute)

		// Held so both land in one batch as two partitions; delivered one
		// at a time there is no dispatch order to observe.
		if err := q.PauseTopic(ctx, "topic.age", "grp", "queuetest-fill"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		publish(ctx, t, q, "topic.age", hot)
		publish(ctx, t, q, "topic.age", quiet)
		if err := q.ResumeTopic(ctx, "topic.age", "grp", "queuetest-fill"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}

		batches.await(t, "the conversation that has waited longest to go first",
			func(got [][]string) bool {
				return len(got) == 2 &&
					equalStrings(got[0], []string{"quiet"}) &&
					equalStrings(got[1], []string{"hot"})
			})
	})

	t.Run("within_a_partition_events_are_ordered_by_timestamp", func(t *testing.T) {
		t.Parallel()
		// A conversation must read in its own chronological order no
		// matter how the broker handed the events over. Measured, the
		// backends disagree: JetStream returns a redelivered message
		// BEHIND never-delivered ones, where the twin replays it from the
		// head. So within-
		// conversation order comes from the timestamps the engine already
		// trusts and already preserves across requeue — never from
		// delivery order. This subtest is what certifies a backend
		// against its own replay semantics, so it makes arrival order and
		// timestamp order deliberately disagree.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		t0 := time.Now().UTC()
		late := newConvEvent("late", "c1")
		late.Timestamp = t0.Add(time.Minute)
		early := newConvEvent("early", "c1")
		early.Timestamp = t0

		// Published newest-first: arrival order is the reverse of
		// chronological order.
		publish(ctx, t, q, "t", late)
		publish(ctx, t, q, "t", early)

		batches.await(t, "the conversation to arrive in timestamp order", func(got [][]string) bool {
			return len(got) == 1 && equalStrings(got[0], []string{"early", "late"})
		})
	})

	t.Run("a_redelivered_event_rejoins_its_conversation_in_timestamp_order", func(t *testing.T) {
		t.Parallel()
		// The scenario the measurement came from: an event is redelivered
		// while a newer one from the same conversation is already
		// waiting. Whichever end of the mailbox the backend returns it to,
		// the handler must see them oldest-first.
		q := s.start(ctx, t)
		batches := newBatchJournal()
		var deliveries int
		if err := q.SubscribeBatch(ctx, "t", "g",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				deliveries++
				if deliveries == 1 {
					// A newer event for the same conversation lands
					// while this one is in flight, so the backend has to
					// interleave a redelivery with a fresh arrival.
					newer := newConvEvent("newer", "c1")
					newer.Timestamp = time.Now().UTC().Add(time.Minute)
					_ = q.Publish(hctx, "t", newer)
					return queue.Nak(errors.New("redeliver me"))
				}
				batches.record(evs)
				return queue.Ack()
			}, convKey, queue.NewBatchOptions(lingerFor.Seconds(), 20)); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		older := newConvEvent("older", "c1")
		older.Timestamp = time.Now().UTC()
		publish(ctx, t, q, "t", older)

		batches.await(t, "the redelivered event to lead its conversation", func(got [][]string) bool {
			return len(got) >= 1 && equalStrings(got[0], []string{"older", "newer"})
		})
	})

	t.Run("a_deferral_stops_the_rest_of_a_batch", func(t *testing.T) {
		t.Parallel()
		// The quiesce check belongs at the top of EVERY partition, not
		// only between batches. Without it, once one partition deferred
		// the loop went on invoking the handler for partitions 2..N on a
		// seat this node had just been told it does not own — and each
		// deferral pushed its partition to the FRONT of the backlog, so
		// the mail came back in REVERSE partition order. That is the
		// exact reordering a deferral exists to prevent.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		fill := holdForOneBatch(ctx, t, q, "topic.b", "grp")

		seen := newJournal()
		if err := q.SubscribeBatch(ctx, "topic.b", "grp",
			func(_ context.Context, evs []*events.Event) queue.Result {
				seen.record(firstConv(t, evs))
				return queue.Defer("seat is not owned here")
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		// Held while publishing so all three land in ONE batch as three
		// partitions: delivered one at a time, the multi-partition loop
		// this covers never runs.
		//
		// THE PUBLISH SIDE IS NOW RACE-FREE AND WAS NOT. This case failed
		// intermittently on JetStream, and the cause was recorded here as
		// unattributed alongside the claim that the hold put every event
		// in the mailbox before the attachment could read one. That claim
		// was false: the hold used to be taken AFTER the subscription, so
		// an attachment already sitting in a long poll was served the
		// first publish and handed it straight back — which on this
		// backend returns it BEHIND the events published after it, so the
		// partition this case expects first arrived second.
		// holdForOneBatch takes the hold before the attachment exists and
		// states the mechanism in full.
		//
		// The DRAIN side is still an assumption: nothing obliges a backend
		// to hand all three partitions to one handler call. If this case
		// recurs, capture the BACKLOG ORDER from the final assertion
		// before anything else — a deferral pushes its partition to the
		// front, so the order distinguishes a split (which is timing) from
		// partitions handled past a deferral (which is the ordering defect
		// this case exists to catch).
		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(t, 1, "the deferral did not stop the batch")

		awaitState(t, "the whole batch to return in partition order", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.b", "grp")), []string{"a", "b", "c"})
		})
	})

	t.Run("a_hold_taken_mid_batch_stops_the_rest", func(t *testing.T) {
		t.Parallel()
		// A DEFERRAL IS NOT THE ONLY WAY A BATCH HAS TO STOP.
		//
		// The contract names four conditions a partition loop must answer —
		// detached, quiesced, drain-paused, and a per-topic HOLD — and the
		// case above stops the loop through the quiesce flag a Defer sets
		// itself. That is the one condition the in-memory twin happened to
		// check, so a hold stopped the real broker and did not stop the
		// twin: the loop went on running turns for partitions 2..N on a seat
		// whose inbox had just been held for a detached coding run, which is
		// exactly the "no turn starts" the hold exists to buy. A suite that
		// covers only the deferral certifies a behaviour production does not
		// have.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		fill := holdForOneBatch(ctx, t, q, "topic.hold", "grp")

		seen := newJournal()
		if err := q.SubscribeBatch(ctx, "topic.hold", "grp",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				seen.record(firstConv(t, evs))
				// The seat parks mid-conversation, so the hold lands while
				// the rest of this very batch is still waiting to run.
				if err := q.PauseTopic(hctx, "topic.hold", "grp", "queuetest-park"); err != nil {
					t.Errorf("PauseTopic: %v", err)
				}
				return queue.Ack()
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(t, 1, "the hold did not stop the batch")
		awaitState(t, "the undispatched partitions to return", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.hold", "grp")), []string{"b", "c"})
		})
	})

	t.Run("a_detach_taken_mid_batch_stops_the_rest", func(t *testing.T) {
		t.Parallel()
		// THE FOURTH CONDITION, and the one that was stated in two
		// backend comments and certified on neither.
		//
		// The contract names four things that stop a partition loop
		// between partitions — a deferral just applied, a hold, a pause,
		// a DETACH (see queue.DeliveriesLeft). The two cases above cover
		// the first two, and the twin's guard was written to cover all
		// four; its comment said so. It did not cover this one, because a
		// detach is the only one that is not a FLAG: it is a consumer
		// leaving the subscription's member list, and the partition loop
		// holds the consumer directly and never re-reads that list. So
		// the twin ran the remaining partitions, ACKED them, and reported
		// the seat's mail consumed — on a consumer this node had already
		// given up.
		//
		// WHICH IS THE ONE THAT MATTERS MOST. A detach is the fenced
		// release: internal/node detaches when a seat's lease is lost,
		// precisely to abandon in-flight work rather than finish it on a
		// seat a peer already owns. An engine or node test asserting that
		// path against the twin was certifying the exact opposite of what
		// the shipped broker does — and this suite, which exists to stop
		// that, had no case to ask with.
		//
		// The undispatched partitions come back like any other stopped
		// drain's: charged one delivery each and left in the
		// subscription's retained mail, which outlives every attachment,
		// so whoever attaches next gets them. What that costs is
		// an_undispatched_partition_pays_for_its_hand_back's subject and
		// is deliberately not re-asserted here — this case is about the
		// loop STOPPING.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		fill := holdForOneBatch(ctx, t, q, "topic.detach", "grp")

		seen := newJournal()
		if err := q.SubscribeBatch(ctx, "topic.detach", "grp",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				seen.record(firstConv(t, evs))
				// The seat's lease moves while the rest of this very
				// batch is still waiting to run, and the node gives the
				// seat up at once rather than when the drain happens to
				// end. Detach does NOT join this handler — that is the
				// contract — so this call returns and the loop is left
				// holding partitions it no longer has any claim to.
				if _, err := q.Detach(hctx, "topic.detach", "grp"); err != nil {
					t.Errorf("Detach: %v", err)
				}
				return queue.Ack()
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(t, 1, "the detach did not stop the batch")
		awaitState(t, "the undispatched partitions to return", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.detach", "grp")), []string{"b", "c"})
		})
	})

	t.Run("a_pause_taken_mid_batch_stops_the_rest", func(t *testing.T) {
		t.Parallel()
		// AND THE LAST OF THE FOUR, added with the detach case above
		// rather than after the next finding, because the detach one was
		// found by reading the enumeration and this is what reading the
		// rest of it produced.
		//
		// MEASURED, not assumed. Deleting the process-wide delivery
		// pause from the between-partition guard fails THIS case and
		// nothing else, on either backend — so before it existed, that
		// condition was held by no case at all. Both backends answer it
		// correctly and always have; it rides the same predicate as the
		// hold and the quiesce. But "rides the same predicate" is exactly
		// the argument that was made for the detach condition, in a
		// comment, while that condition was not in the predicate at all.
		// A condition the contract names and no case asks about is one
		// delete away from being gone, whether or not anything is wrong
		// with it today.
		//
		// THE DRAIN PAUSE IS A DIFFERENT GATE FROM THE TWO NEAR IT, which
		// is why it cannot borrow their coverage. A hold is per
		// (topic, group) and reversible; a quiesce is per attachment and
		// reversible; this is per PROCESS and one-way, because it is the
		// shutdown drain — the node has stopped taking work and is
		// waiting out its in-flight handlers. Landing mid-batch is its
		// ordinary case rather than an exotic one: a drain that began
		// while a seat was working arrives exactly here.
		//
		// pause_during_linger_retains_pending covers the same verb at the
		// OTHER gate — a window open when the pause lands — which is a
		// different branch in both backends.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		fill := holdForOneBatch(ctx, t, q, "topic.drain", "grp")

		seen := newJournal()
		if err := q.SubscribeBatch(ctx, "topic.drain", "grp",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				seen.record(firstConv(t, evs))
				// The node begins its shutdown drain while the rest of
				// this batch is still waiting to run. The handler in
				// flight finishes — that is what a drain waits for —
				// and nothing new starts.
				if err := q.PauseDelivery(hctx); err != nil {
					t.Errorf("PauseDelivery: %v", err)
				}
				return queue.Ack()
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(t, 1, "the drain pause did not stop the batch")
		awaitState(t, "the undispatched partitions to return", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.drain", "grp")), []string{"b", "c"})
		})
	})

	t.Run("an_undispatched_partition_pays_for_its_hand_back", func(t *testing.T) {
		t.Parallel()
		// WHAT THE REST OF A STOPPED DRAIN COSTS, which is the same as
		// what the partition that stopped it costs: one delivery each.
		//
		// The two cases above certify that the loop STOPS and that the
		// undispatched partitions come back in order. Neither says what
		// coming back is charged, and the two backends answered that
		// differently for as long as nothing asked. JetStream FETCHES a
		// drain, so its counter has already moved on every message in it
		// before the loop discovers it may not finish — the remainder
		// goes back through the same budget check as a failure. The twin
		// spliced its undispatched partitions into the mailbox untouched,
		// so on a seat whose drains are stopped over and over — a lease
		// that keeps moving, an inbox held for one parked coding run
		// after another — the same message rode round for ever on a
		// counter that never moved, while the identical message on the
		// shipped broker was walking through its twenty-five.
		//
		// That is the LAST HALF of the divergence the deferral's own cost
		// had: one partition of a drain paying the broker's price and
		// every other partition of the same drain paying nothing. The
		// contract states one rule for all of it — see
		// queue.DeliveriesLeft — so this certifies the rule rather than
		// either backend's mechanism.
		//
		// A HOLD RATHER THAN A DEFERRAL, because the hold is released in
		// place: ResumeTopic clears it and the same attachment carries
		// on, so the held-back partition comes back to a handler that can
		// read what it has left. A deferral would quiesce the attachment,
		// and un-quiescing it is a different verb testing a different
		// thing.
		//
		// OBSERVED AGAINST BOTH LITERALS, and it used to be a bare
		// comparison of the two readings — handed back must be LESS than
		// never-delivered — on the stated ground that an extra redelivery
		// on the way here could not then turn the rule into a timing test.
		// That reasoning was inverted, and it cost a CI failure: a
		// comparison of two DIFFERENT messages presumes they entered the
		// drain at the same delivery count, and nothing here established
		// that. An early delivery of the stopping partition and nothing
		// else — which is exactly what holdForOneBatch now prevents, and
		// what the hold used to allow — lowers the baseline by one and
		// makes the two readings EQUAL while the hand-back was charged
		// correctly. Reported, of course, as a hand-back that was free.
		//
		// So the precondition is asserted rather than presumed. The
		// stopping partition must read a full budget less one, which says
		// it was delivered exactly once and therefore that the fill held
		// everything back; and the handed-back one must read exactly one
		// less than that, which is the rule — ONE delivery, not two. Both
		// numbers follow from queue.DeliveriesLeft's contract and from the
		// attempts configured here, not from either backend's shape, and
		// each failure names which half it is: a broken fill and a
		// mischarged hand-back send a reader to opposite places.
		newQueueWithAttempts := s.needAttempts(t)
		const attempts = 5
		q := startQueue(ctx, t, newQueueWithAttempts(t, attempts))

		const topic, group = "topic.charged", "grp"
		fill := holdForOneBatch(ctx, t, q, topic, group)
		var (
			mu     sync.Mutex
			first  = map[string]int{}
			parked bool
		)
		j := newJournal()
		subscribeBatch(ctx, t, q, topic, group,
			func(hctx context.Context, evs []*events.Event) queue.Result {
				conv := firstConv(t, evs)
				left, known := queue.DeliveriesLeft(hctx)

				mu.Lock()
				if _, seen := first[conv]; !seen && known {
					first[conv] = left
				}
				park := !parked
				parked = true
				mu.Unlock()

				if park {
					// The seat parks while the REST of this batch is
					// still waiting to run, so the second partition
					// goes back having been drained and never
					// dispatched — the state this case is about.
					if err := q.PauseTopic(hctx, topic, group, "queuetest-charged"); err != nil {
						t.Errorf("PauseTopic: %v", err)
					}
				}
				j.record(conv)
				return queue.Ack()
			}, queue.NewBatchOptions(lingerFor.Seconds(), 20))

		fill("a", "b")
		j.awaitLabels(t, "only the first partition to be handled", "a")
		j.staysAt(t, 1, "the hold did not stop the batch")

		if err := q.ResumeTopic(ctx, topic, group, "queuetest-charged"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		j.awaitLabels(t, "the held-back partition to come back", "a", "b")

		mu.Lock()
		defer mu.Unlock()
		fresh, freshOK := first["a"]
		handed, handedOK := first["b"]
		if !freshOK || !handedOK {
			t.Fatalf("the handler read %v, want a headroom for each partition: "+
				"a backend that states nothing cannot be asked what a hand-back "+
				"cost", first)
		}
		if fresh != attempts-1 {
			t.Fatalf("the partition that stopped the drain reported %d deliveries "+
				"left of %d attempts, want %d: it is dispatched on its FIRST "+
				"delivery, so anything lower means it had already been delivered "+
				"and handed back before this batch — a fill that did not hold "+
				"everything back, not a charge this case can read. See "+
				"holdForOneBatch.", fresh, attempts, attempts-1)
		}
		if handed != fresh-1 {
			t.Errorf("the partition handed back undispatched reported %d deliveries "+
				"left and the one that stopped the drain %d, want exactly one "+
				"less: a drain the consumer stopped is handed back, and every "+
				"hand-back spends one delivery — the partition that stopped it is "+
				"charged, so the rest of it is neither free nor charged twice. (At "+
				"%d it was free, which is also what a backend that split the fill "+
				"into separate batches reports: there was then no undispatched "+
				"remainder to charge and this case never ran the loop it is named "+
				"for. See holdForOneBatch.)", handed, fresh, fresh)
		}
	})

	t.Run("a_publish_during_the_batch_does_not_move_the_splice", func(t *testing.T) {
		t.Parallel()
		// The restore point for the undispatched partitions is found by
		// IDENTITY, not by arithmetic. The obvious implementation is a
		// length delta — how much longer is the mailbox than before the
		// loop — which assumes it only grew at the FRONT. Publishing
		// appends at the TAIL: one landing mid-batch makes the delta
		// over-count and the splice lands inside the pre-existing tail,
		// reordering the very partitions the guard protects.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		fill := holdForOneBatch(ctx, t, q, "topic.c", "grp")

		seen := newJournal()
		var grown bool
		if err := q.SubscribeBatch(ctx, "topic.c", "grp",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				seen.record(firstConv(t, evs))
				// Grow the tail under the loop, exactly as a concurrent
				// producer would — ONCE.
				//
				// A handler that published on every invocation would be
				// an unbounded work generator: on a backend that drains
				// inline it feeds its own drain loop, which then never
				// reaches an empty mailbox and never returns. The test
				// only needs the tail to grow once, so it must not be
				// the thing that wedges a suite it shares a binary with.
				if !grown {
					grown = true
					_ = q.Publish(hctx, "topic.c", newConvEvent("late", "late"))
				}
				return queue.Defer("seat is not owned here")
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		awaitState(t, "the undispatched partitions to be spliced after the deferred one", func() bool {
			got := convsOf(backlog(q, "topic.c", "grp"))
			return len(got) >= 3 && equalStrings(got[:3], []string{"a", "b", "c"})
		})
	})
}

// holdForOneBatch holds a subscription's deliveries so several events can be
// put in ONE batch, and returns the function that publishes them and releases
// the hold.
//
// TWO CALLS RATHER THAN ONE, and their order is the whole of it: the hold has
// to be in place BEFORE the caller attaches. So a case takes the hold, then
// subscribes, then fills — and a single pause/publish/resume helper called
// after SubscribeBatch, which is what this was, cannot give the guarantee
// while reading exactly as though it does.
//
// A HOLD CANNOT RETRACT A FETCH THAT IS ALREADY IN FLIGHT. An idle attachment
// on a pull backend sits in a long poll, and a hold is a flag on the client:
// an event published a moment after the flag is set is served straight into
// that outstanding request, delivered, and then handed back by the very guard
// the hold exists for. It reaches the next drain having ALREADY SPENT a
// delivery — and, on a backend where a redelivery returns behind
// never-delivered mail (which is measured true of JetStream), behind the
// events published after it.
//
// That is not hypothetical and it is not one case's problem. It was
// reproduced by letting the attachment reach its first fetch before the hold
// lands, and it accounts for two intermittent failures in this group, with
// two different symptoms:
// an_undispatched_partition_pays_for_its_hand_back read the STOPPING
// partition's headroom one lower than the remainder's and so reported a
// hand-back as free, and a_deferral_stops_the_rest_of_a_batch had its first
// partition arrive other than first.
//
// Holding before the attachment exists removes it BY CONSTRUCTION rather than
// by a wait: the attachment's own first look at the flag already sees the
// hold, so nothing is ever outstanding while the fill publishes. A wait would
// have had to outlast a poll window the suite cannot see — the backend's, not
// the contract's — which is how a suite acquires a race it cannot measure.
//
// WHAT IT STILL DOES NOT PROMISE IS THE DRAIN SIDE, and that half is an
// assumption rather than a guarantee. Holding the subscription makes the
// events available together, which is the most any caller can arrange through
// the contract — but nothing obliges a backend to hand them over as ONE batch.
// A backend that delivers them singly makes the cases built on this pass
// without exercising the multi-partition loop they are named for: the first
// single-partition batch defers, the attachment quiesces, and there never was
// a "rest of the batch" to stop. That is a vacuous pass, not a false one, and
// it is invisible from most of the assertions —
// an_undispatched_partition_pays_for_its_hand_back is the exception, because
// it can tell a split from a hand-back that cost nothing and says which it
// saw.
//
// The in-memory twin is where these genuinely exercise the loop, because it
// drains everything available into one delivery. Treat a green result on an
// asynchronous backend as "did not contradict" rather than "certified", and if
// that distinction ever needs closing it needs an observable the contract does
// not currently have — how many partitions a delivery was drawn from.
func holdForOneBatch(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string) func(convs ...string) {
	t.Helper()
	if err := q.PauseTopic(ctx, topic, group, "queuetest-fill"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	return func(convs ...string) {
		t.Helper()
		for _, conv := range convs {
			publish(ctx, t, q, topic, newConvEvent(conv, conv))
		}
		if err := q.ResumeTopic(ctx, topic, group, "queuetest-fill"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
	}
}

// runContractPolicy exercises the pure policy functions in the contract
// package — NOT any backend's use of them.
//
// SCOPE, stated because this group's line in the output sits under
// TestConformance and a PASS there reads like backend coverage. Every case
// below calls a function in internal/queue directly, so its result is identical
// for every backend by construction and it CANNOT certify that a backend calls
// that function, or calls it correctly. Measured: a backend that partitions
// correctly but dispatches partitions in receive order passed all six of these
// while ignoring the aging policy outright.
//
// What owns the backend side, by name:
//   - between-partition aging  -> Batch/an_aged_conversation_dispatches_before_a_fresher_one
//   - within-partition order   -> Batch/within_a_partition_events_are_ordered_by_timestamp
//   - partition membership     -> Batch/linger_partitions_by_key_preserving_arrival_order
//   - the max_batch cap        -> Batch/max_batch_chunks_oversized_buffers
//
// The linger ceiling has no backend counterpart: nothing here checks that a
// backend honours the clamp rather than waiting the unclamped window, because
// asserting it costs a test that sleeps for a minute.
//
// These run inside the conformance suite only because internal/queue has no
// test binary of its own. When it gets one they belong there, and this group
// should go rather than be duplicated.
func (s *suite) runContractPolicy(t *testing.T) {
	t.Run("effective_linger_clamps_to_ceiling", func(t *testing.T) {
		t.Parallel()
		// The ceiling is enforced where the value is consumed:
		// programmatic construction bypasses config validation, and an
		// unbounded linger would re-open the ack-window overlap the
		// dispatch budget exists to close.
		ceiling := time.Duration(queue.MaxLingerSeconds * float64(time.Second))
		for _, tc := range []struct {
			name  string
			given float64
			want  time.Duration
		}{
			{"above the ceiling", 300, ceiling},
			{"negative", -5, 0},
			{"in range", 10, 10 * time.Second},
		} {
			if got := queue.NewBatchOptions(tc.given, 20).EffectiveLinger(); got != tc.want {
				t.Errorf("%s: EffectiveLinger(%v) = %v, want %v", tc.name, tc.given, got, tc.want)
			}
		}
		if got := queue.NewBatchOptions(0, 0).EffectiveMaxBatch(); got != 1 {
			t.Errorf("EffectiveMaxBatch(0) = %d, want 1", got)
		}
	})

	t.Run("order_partitions_oldest_first", func(t *testing.T) {
		t.Parallel()
		// A deferred (older) conversation wins priority over fresh
		// arrivals, so steady inflow on a hot conversation cannot starve
		// a quiet one whose requeued copies re-enter behind it.
		t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		hotNew := newConvEvent("a", "hot")
		hotNew.Timestamp = t0.Add(60 * time.Second)
		quietOld := newConvEvent("b", "quiet")
		quietOld.Timestamp = t0

		// Receive order puts the hot conversation first.
		got := orderedKeys([]*events.Event{hotNew, quietOld})
		if !equalStrings(got, []string{"quiet", "hot"}) {
			t.Fatalf("dispatch order = %v, want [quiet hot]", got)
		}
	})

	t.Run("one_unstamped_partition_does_not_disable_aging", func(t *testing.T) {
		t.Parallel()
		// A COMPARATOR THAT IS NOT AN ORDERING breaks the partitions it
		// was never asked about. Answering "equal" whenever EITHER side
		// carried no timestamp is not transitive, so one unstamped
		// partition anywhere in a drain left every stamped one in arrival
		// order — aging silently off for the whole drain, on the exact
		// input a fairness policy has to survive.
		t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		hotNew := newConvEvent("a", "hot")
		hotNew.Timestamp = t0.Add(60 * time.Second)
		unstamped := newConvEvent("b", "nostamp")
		unstamped.Timestamp = time.Time{}
		quietOld := newConvEvent("c", "quiet")
		quietOld.Timestamp = t0

		// Receive order is hot, unstamped, quiet. The two stamped
		// conversations must still age against each other, and the one
		// that cannot be compared goes last rather than anywhere.
		got := orderedKeys([]*events.Event{hotNew, unstamped, quietOld})
		if !equalStrings(got, []string{"quiet", "hot", "nostamp"}) {
			t.Fatalf("dispatch order = %v, want [quiet hot nostamp]", got)
		}
	})

	t.Run("order_within_a_partition_is_by_timestamp", func(t *testing.T) {
		t.Parallel()
		// The second level OrderForDispatch establishes, and the one that
		// removed the engine's dependency on a broker's replay semantics.
		t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		late := newConvEvent("late", "c1")
		late.Timestamp = t0.Add(time.Minute)
		early := newConvEvent("early", "c1")
		early.Timestamp = t0

		parts := queue.OrderForDispatch(
			queue.PartitionByKey([]*events.Event{late, early}, convKey, sameEvent), sameEvent)
		if len(parts) != 1 {
			t.Fatalf("partitions = %d, want 1", len(parts))
		}
		if got := labelsOf(parts[0].Items); !equalStrings(got, []string{"early", "late"}) {
			t.Fatalf("within-partition order = %v, want [early late]", got)
		}
	})

	t.Run("order_partitions_tie_keeps_arrival_order", func(t *testing.T) {
		t.Parallel()
		t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		first := newConvEvent("a", "c1")
		first.Timestamp = t0
		second := newConvEvent("b", "c2")
		second.Timestamp = t0

		if got := orderedKeys([]*events.Event{first, second}); !equalStrings(got, []string{"c1", "c2"}) {
			t.Fatalf("dispatch order on a tie = %v, want [c1 c2]", got)
		}
	})

	t.Run("order_partitions_falls_back_on_incomparable_timestamps", func(t *testing.T) {
		t.Parallel()
		// Ordering is a fairness policy and must never block delivery, so
		// an event carrying no usable timestamp degrades to arrival order.
		stamped := newConvEvent("a", "c1")
		stamped.Timestamp = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		unstamped := newConvEvent("b", "c2")
		unstamped.Timestamp = time.Time{}

		if got := orderedKeys([]*events.Event{stamped, unstamped}); !equalStrings(got, []string{"c1", "c2"}) {
			t.Fatalf("dispatch order with an unstamped event = %v, want [c1 c2]", got)
		}
	})

	t.Run("partition_by_key_preserves_arrival_order", func(t *testing.T) {
		t.Parallel()
		evs := []*events.Event{
			newConvEvent("a", "c1"),
			newConvEvent("b", "c2"),
			newConvEvent("c", "c1"),
		}
		parts := queue.PartitionByKey(evs, convKey, sameEvent)
		if len(parts) != 2 {
			t.Fatalf("partitions = %d, want 2", len(parts))
		}
		if parts[0].Key != "c1" || !equalStrings(labelsOf(parts[0].Items), []string{"a", "c"}) {
			t.Fatalf("first partition = %s/%v, want c1/[a c]", parts[0].Key, labelsOf(parts[0].Items))
		}
		if parts[1].Key != "c2" || !equalStrings(labelsOf(parts[1].Items), []string{"b"}) {
			t.Fatalf("second partition = %s/%v, want c2/[b]", parts[1].Key, labelsOf(parts[1].Items))
		}
	})
}

func orderedKeys(evs []*events.Event) []string {
	parts := queue.OrderForDispatch(queue.PartitionByKey(evs, convKey, sameEvent), sameEvent)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Key)
	}
	return out
}

func sameEvent(ev *events.Event) *events.Event { return ev }
