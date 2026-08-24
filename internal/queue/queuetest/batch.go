package queuetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runBatch covers batched, key-partitioned delivery: the drain, the
// partitioning, per-partition acking, and the two places a batch loop has to
// stop early — a mid-batch quiesce and the linger window closing on a paused
// attachment.
func (s *suite) runBatch(t *testing.T) {
	ctx := context.Background()

	t.Run("zero_linger_dispatches_inline_single_event_batches", func(t *testing.T) {
		t.Parallel()
		if !s.caps.InlineDispatch {
			t.Skip("backend has fetch latency, so batch boundaries are not deterministic")
		}
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches), queue.DefaultBatchOptions())

		publish(t, q, "t", newConvEvent("a", "c1"))
		publish(t, q, "t", newConvEvent("b", "c1"))

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
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t.neg", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(-5, 20))

		publish(t, q, "t.neg", newConvEvent("a", "c1"))
		batches.await(t, "delivery under a negative linger",
			func(got [][]string) bool { return len(got) == 1 && equalStrings(got[0], []string{"a"}) })
	})

	t.Run("linger_coalesces_same_key_events_into_one_batch", func(t *testing.T) {
		t.Parallel()
		// The property inbox batching exists for: events that queued
		// while an agent was busy must arrive as ONE turn, not N.
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		publish(t, q, "t", newConvEvent("a", "c1"))
		publish(t, q, "t", newConvEvent("b", "c1"))

		batches.await(t, "one coalesced batch", func(got [][]string) bool {
			return len(got) == 1 && equalStrings(got[0], []string{"a", "b"})
		})
	})

	t.Run("linger_partitions_by_key_preserving_arrival_order", func(t *testing.T) {
		t.Parallel()
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		publish(t, q, "t", newConvEvent("a", "c1"))
		publish(t, q, "t", newConvEvent("b", "c2"))
		publish(t, q, "t", newConvEvent("c", "c1"))

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
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 2))

		for i := range 5 {
			publish(t, q, "t", newConvEvent(string(rune('0'+i)), "c1"))
		}
		batches.awaitSizes(t, "the backlog to arrive as capped batches", 2, 2, 1)
	})

	t.Run("failing_partition_redelivers_only_itself", func(t *testing.T) {
		t.Parallel()
		// A handler failure on one conversation must not replay or block
		// the other conversation from the same flush.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := startQueue(t, newQueueWithAttempts(t, 3))

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

		publish(t, q, "t", newConvEvent("a", "c1"))
		publish(t, q, "t", newConvEvent("b", "c2"))

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
		q := s.start(t)
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

		publish(t, q, "t", newEvent("a"))
		publish(t, q, "t", newEvent("b"))

		batches.awaitSizes(t, "each event to become its own partition", 1, 1)
	})

	t.Run("live_options_mutation_takes_effect_next_cycle", func(t *testing.T) {
		t.Parallel()
		// A hot config reload changes linger and batch size with no
		// re-subscription: the consume loop re-reads the options every
		// cycle.
		q := s.start(t)
		batches := newBatchJournal()
		opts := queue.NewBatchOptions(0, 20)
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches), opts)

		publish(t, q, "t", newConvEvent("a", "c1"))
		batches.awaitSizes(t, "the un-lingered first event", 1)

		opts.Set(lingerFor.Seconds(), 20)
		publish(t, q, "t", newConvEvent("b", "c1"))
		publish(t, q, "t", newConvEvent("c", "c1"))
		batches.awaitSizes(t, "the next cycle to honour the new linger", 1, 2)
	})

	t.Run("stop_cancels_pending_lingered_flush", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(5.0, 20))

		setup := time.Now()
		publish(t, q, "t", newConvEvent("a", "c1"))
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
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(racingWindow.Seconds(), 20))

		// The window must be OPEN when the pause lands and must EXPIRE while
		// it is held — no ordering removes that, so this case buys slack with
		// a longer window rather than pretending the race is gone.
		setup := time.Now()
		publish(t, q, "t", newConvEvent("a", "c1"))
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
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t.hold", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(racingWindow.Seconds(), 20))

		// Open the window, then take the hold while it is open.
		publish(t, q, "t.hold", newConvEvent("during", "c1"))
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
		publish(t, q, "t.hold", newConvEvent("after", "c1"))
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
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "topic.age", "grp", recordingBatchHandler(batches),
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
		publish(t, q, "topic.age", hot)
		publish(t, q, "topic.age", quiet)
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
		// matter how the broker handed the events over. Measured, the two
		// brokers disagree: JetStream returns a redelivered message
		// BEHIND never-delivered ones, where Pulsar replays it from the
		// head (rewrite/decisions/102-jetstream-redelivery.md). So within-
		// conversation order comes from the timestamps the engine already
		// trusts and already preserves across requeue — never from
		// delivery order. This subtest is what certifies a backend
		// against its own replay semantics, so it makes arrival order and
		// timestamp order deliberately disagree.
		q := s.start(t)
		batches := newBatchJournal()
		subscribeBatch(t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		t0 := time.Now().UTC()
		late := newConvEvent("late", "c1")
		late.Timestamp = t0.Add(time.Minute)
		early := newConvEvent("early", "c1")
		early.Timestamp = t0

		// Published newest-first: arrival order is the reverse of
		// chronological order.
		publish(t, q, "t", late)
		publish(t, q, "t", early)

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
		q := s.start(t)
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
		publish(t, q, "t", older)

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
		q := s.start(t)

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
		// That one-batch outcome is an ASSUMPTION, not a guarantee the
		// contract makes, and it is worth knowing which because this case
		// has failed intermittently on JetStream and the cause is still
		// unattributed. It was briefly written up as timing-independent
		// "by construction" — that is wrong, and stating it here rather
		// than leaving the correction in a commit message: the PUBLISH
		// side is indeed race-free, since the hold means every event is
		// in the mailbox before the attachment resumes, but the DRAIN
		// side is not, and nothing obliges a backend to hand all three
		// to one call.
		//
		// What is actually known: 22 consecutive clean runs here (12
		// isolated, 10 full-package, all under -race), and no
		// reproduction. Forcing a split with a max batch of 2 did not
		// reproduce it either, though with a zero linger that variant may
		// not exercise a split at all — so that experiment rules nothing
		// out. If it recurs, capture the BACKLOG ORDER from the final
		// assertion before anything else: a deferral pushes its partition
		// to the front, so the order distinguishes a split (which is
		// timing) from partitions handled past a deferral (which is the
		// ordering defect this case exists to catch).
		fillOneBatch(t, q, "topic.b", "grp", "a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(t, 1, "the deferral did not stop the batch")

		awaitState(t, "the whole batch to return in partition order", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.b", "grp")), []string{"a", "b", "c"})
		})
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
		q := s.start(t)

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

		fillOneBatch(t, q, "topic.c", "grp", "a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		awaitState(t, "the undispatched partitions to be spliced after the deferred one", func() bool {
			got := convsOf(backlog(q, "topic.c", "grp"))
			return len(got) >= 3 && equalStrings(got[:3], []string{"a", "b", "c"})
		})
	})
}

// fillOneBatch publishes one event per conversation while the subscription is
// held, so they arrive as a single multi-partition batch when it is released.
//
// SCOPE, because this is an assumption and not a guarantee. Holding the
// subscription makes the events available together, which is the most any
// caller can arrange through the contract — but nothing obliges a backend to
// hand them over as ONE batch. A backend that delivers them singly makes the
// cases built on this pass without exercising the multi-partition loop they are
// named for: the first single-partition batch defers, the attachment quiesces,
// and there never was a "rest of the batch" to stop. That is a vacuous pass,
// not a false one, and it is invisible from the assertions.
//
// The in-memory twin is where these genuinely exercise the loop, because it
// drains everything available into one delivery. Treat a green result on an
// asynchronous backend as "did not contradict" rather than "certified", and if
// that distinction ever needs closing it needs an observable the contract does
// not currently have — how many partitions a delivery was drawn from.
func fillOneBatch(t *testing.T, q queue.EventQueue, topic, group string, convs ...string) {
	t.Helper()
	ctx := context.Background()
	if err := q.PauseTopic(ctx, topic, group, "queuetest-fill"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	for _, conv := range convs {
		publish(t, q, topic, newConvEvent(conv, conv))
	}
	if err := q.ResumeTopic(ctx, topic, group, "queuetest-fill"); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
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
