package queuetest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		// Only one half of the linger clamp is reachable. The CEILING (60s)
		// is not: the suite may not depend on a window above it, and a linger
		// has no reported value to compare, so probing it would mean sleeping
		// a minute. That half stays a documented gap.
		//
		// The FLOOR costs nothing: a negative window must behave as no
		// window. A backend forwarding the raw negative duration to a broker
		// API gets nothing defined from it, so it must be caught here.
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
		// A burst reaching an IDLE seat joins its first event's batch. The
		// second event is published only AFTER the backend reports the window
		// open (Capabilities.Lingering): back to back, a pull drain's one
		// tail fetch collects both at ANY window, so a backend ignoring the
		// linger would pass. The window ends on the case's clock
		// (Capabilities.LingerClock), since wall-clock loses that publish on
		// a loaded runner; InWindow holds it, then the clock passes the end.
		// Mutation: a linger of nothing, or ending the window at its first
		// fetch, leaves it unreported; never counting what it holds fails
		// the wait for the second event.
		lingering := s.needLingering(t)
		inWindow := s.needInWindow(t)
		onClock := s.needLingerClock(t)
		clock := &Clock{}
		q := s.startQueue(ctx, t, onClock(t, clock))
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(clockedWindow.Seconds(), 20))

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		// LIVENESS: a backend that ignores the linger never reports a
		// window, so stallFor only ends that case's wait.
		awaitState(t, "the first event's window to outlast what no window collects", func() bool {
			return lingering(q, "t", "g")
		})
		publish(ctx, t, q, "t", newConvEvent("b", "c1"))
		// LIVENESS, as above: a window that never holds the second event
		// fails here.
		awaitState(t, "the window to hold the second event", func() bool {
			return inWindow(q, "t", "g") == 2
		})
		clock.Advance(clockedWindow)

		batches.await(t, "one coalesced batch", func(got [][]string) bool {
			return len(got) == 1 && equalStrings(got[0], []string{"a", "b"})
		})
	})

	t.Run("linger_partitions_by_key_preserving_arrival_order", func(t *testing.T) {
		t.Parallel()
		// What a drain does with the mail it holds: one partition per key,
		// each in arrival order, dispatched oldest-constituent-first.
		//
		// QUEUED BEFORE THE ATTACHMENT, never published to one, because
		// nothing in that depends on WHEN the mail arrived. Published to an
		// attached consumer, the three events had to land inside one
		// lingerFor window — three round trips on a loaded runner — and
		// the split the batcher is right to make then read as a
		// partitioning defect. See queueBacklog.
		q := s.start(ctx, t)
		queueBacklog(ctx, t, q, "t", "g",
			newConvEvent("a", "c1"), newConvEvent("b", "c2"), newConvEvent("c", "c1"))
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

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
		//
		// The backlog is a real one: it queues in the mailbox BEFORE the
		// handler attaches. Published to an attached consumer instead,
		// the five publishes race the 50 ms linger, and a loaded runner
		// closed the window after the first ([0] [1 2] [3 4]) — a split
		// the batcher is right to make and this case wrongly refused.
		q := s.start(ctx, t)
		backlog := make([]*events.Event, 0, 5)
		for i := range 5 {
			backlog = append(backlog, newConvEvent(string(rune('0'+i)), "c1"))
		}
		queueBacklog(ctx, t, q, "t", "g", backlog...)

		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 2))
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
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, 3))

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
		// TWO NUMBERS, BECAUSE ONE CANNOT ANSWER BOTH QUESTIONS. A
		// conversation's handed-back message keeps collecting fresh replies,
		// each with a whole budget. A handler is told the partition's
		// headroom ([queue.DeliveriesLeft], the smallest: "will handing this
		// back dead-letter something") and each message's
		// ([queue.DeliveriesLeftFor]: "how much is left of THIS one").
		// Reading the first for the second refuses a reply on its FIRST
		// delivery, and every later one. See internal/sandbox.MayOfferAnswer.
		// The assertion is RELATIONAL, since a backend may add a redelivery:
		// the counts must DIFFER, the partition's smaller (on the common
		// path attempts-2 and attempts-1).
		newQueueWithAttempts := s.needAttempts(t)
		quiescing := s.needQuiescing(t)
		const attempts = 6
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, attempts))

		const topic, group, conv = "t.mixed", "g", "c1"
		older := newConvEvent("older", conv)
		fresher := newConvEvent("fresher", conv)

		var (
			mu                  sync.Mutex
			seeded, came        bool
			carried             []string
			readings            map[string]int
			partLeft            int
			partKnown, unstated bool
		)
		j := newJournal()
		subscribeBatch(ctx, t, q, topic, group,
			func(hctx context.Context, evs []*events.Event) queue.Result {
				mu.Lock()
				first := !seeded
				seeded = true
				mu.Unlock()

				if first {
					// The mixed partition: a handed-back message and a
					// never-delivered one must meet in ONE drain, as in
					// a_redelivered_event_rejoins_its_conversation_in_timestamp_order.
					// So the fresh one is published FROM the handler, before
					// the first goes back by DEFERRAL (queue.OutcomeDefer):
					// it spends a delivery as a failure does, returns at
					// once and stops the attachment until the case resumes
					// it. A failure return waits on the redelivery backoff,
					// a race with the case's window.
					// A failure here is recorded: not the test goroutine.
					if err := q.Publish(hctx, topic, fresher); err != nil {
						j.record("publish failed: " + err.Error())
						return queue.Ack()
					}
					j.record("handed back")
					return queue.Defer(queue.DeferSeatMoved, "hand the first one back")
				}

				got := make(map[string]int, len(evs))
				var missing bool
				for _, ev := range evs {
					left, known := queue.DeliveriesLeftFor(hctx, ev.ID)
					if !known {
						missing = true
						continue
					}
					got[labelOf(ev)] = left
				}
				left, known := queue.DeliveriesLeft(hctx)

				// The FIRST call after the hand-back is the one this
				// case is about; a later one would mean the two came
				// round apart, which the assertions below report.
				mu.Lock()
				if !came {
					came = true
					carried = labelsOf(evs)
					readings, partLeft, partKnown, unstated = got, left, known, missing
				}
				mu.Unlock()
				j.record("came round")
				return queue.Ack()
			}, queue.NewBatchOptions(handBackLinger.Seconds(), 20))

		publish(ctx, t, q, topic, older)
		j.awaitLabels(t, "the first message to be handed back", "handed back")
		awaitState(t, "the deferral to quiesce the attachment", func() bool {
			return quiescing(q, topic, group)
		})
		if resumed, err := q.Unquiesce(ctx, topic, group); err != nil || !resumed {
			t.Fatalf("Unquiesce = (%v, %v), want (true, nil)", resumed, err)
		}
		j.await(t, "the partition to come round",
			func(seen []string) bool { return len(seen) >= 2 })

		mu.Lock()
		defer mu.Unlock()
		if len(carried) != 2 {
			t.Fatalf("the first handler call after the hand-back carried %v, want both "+
				"messages: they were both in the mailbox before the attachment could "+
				"fetch again, so either the drain left behind mail the broker already "+
				"held, or the broker took longer than handBackLinger (%s) to take back "+
				"a message it had already been sent", carried, handBackLinger)
		}
		if unstated {
			t.Fatalf("the partition carried %v without a count for each message: a "+
				"handler cannot ask what one message has left if the backend states "+
				"nothing for it", carried)
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
		//
		// QUEUED BEFORE THE ATTACHMENT, so the two conversations ARE one
		// flush: published to an attached consumer, a late second publish
		// splits them into two drains and the case passes with no flush to
		// share. See queueBacklog.
		//
		// Mutation: let a failed partition fail the rest of its drain —
		// the other conversation handled, and handed back with the failure
		// all the same — and c2 runs again with every redelivery of c1.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, 3))
		queueBacklog(ctx, t, q, "t", "g", newConvEvent("a", "c1"), newConvEvent("b", "c2"))

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
		attempts.staysAt(ctx, t, 4, "a dead-lettered partition kept being redelivered")
		awaitState(t, "c1's event to reach the dead-letter subject", func() bool {
			return len(deadLetters(q, "t", "g")) == 1
		})
	})

	// A PASSED PARTITION GOES BACK FOR A PEER, BEHIND THE WAITING MAIL, AND THE
	// REST OF ITS DRAIN RUNS, as on the single path (Core's
	// a_pass_returns_behind_waiting_mail_keeps_consuming_and_dead_letters_once_spent):
	// it returns until its budget is spent, then is dead-lettered with the
	// group's unhandled deliveries; the flush's other conversation is
	// handled once; the attachment is never quiesced. QUEUED BEFORE THE
	// ATTACHMENT, so c1 and c2 are one flush.
	// Mutation: return a passed partition at the head, and c1 comes back
	// ahead of c3 on the twin; leave the pass out of a backend's outcomes,
	// and c1 is never returned or dead-lettered; quiesce on it, and c2 is
	// never handled; count it as a failure, and its copy is kept with the
	// group's poison.
	t.Run("a_passed_partition_returns_for_a_peer_and_the_rest_of_its_drain_runs", func(t *testing.T) {
		t.Parallel()
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		quiescing := s.needQuiescing(t)
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, 3))
		queueBacklog(ctx, t, q, "t", "g", newConvEvent("a", "c1"), newConvEvent("b", "c2"))

		opts := queue.NewBatchOptions(lingerFor.Seconds(), 20)
		var passedOnce atomic.Bool
		attempts := newJournal()
		if err := q.SubscribeBatch(ctx, "t", "g",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				conv := firstConv(t, evs)
				attempts.record(conv)
				if conv != "c1" {
					return queue.Ack()
				}
				if passedOnce.CompareAndSwap(false, true) {
					opts.Set(0, 1)
					// c3 is the waiting mail: published after the flush was
					// taken, before c1 is passed. A drain now takes ONE
					// event (Set is read at the next cycle), so holds c3 or
					// the returned c1, never both: inside a drain,
					// timestamps would put the older c1 first however it
					// was returned. On the twin the order is the return's
					// place in the mailbox; on JetStream the spacing's,
					// pinned in internal/queue/jetstream by
					// TestAPassedPartitionIsSpacedByEachMessagesDeliveries.
					// Recorded rather than fataled: not the test goroutine.
					if err := q.Publish(hctx, "t", newConvEvent("c", "c3")); err != nil {
						attempts.record("publish failed: " + err.Error())
					}
				}
				return queue.Pass("newer_record", errors.New("a peer on a newer build reads it"))
			}, convKey, opts); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		attempts.await(t, "c1 to spend its budget on passes while c2 and c3 run once", func(seen []string) bool {
			counts := map[string]int{}
			for _, conv := range seen {
				counts[conv]++
			}
			return counts["c1"] == 3 && counts["c2"] == 1 && counts["c3"] == 1
		})
		attempts.staysAt(ctx, t, 5, "a dead-lettered partition kept being redelivered")
		seen := attempts.all()
		var c1s int
		for _, conv := range seen {
			if conv == "c3" {
				break
			}
			if conv == "c1" {
				c1s++
			}
		}
		if c1s != 1 {
			t.Errorf("the conversations were handled as %v: c1 came back %d time(s) before c3, "+
				"want none — a passed partition returns behind the mail that was waiting when "+
				"it was passed", seen, c1s-1)
		}
		if quiescing(q, "t", "g") {
			t.Error("a passed partition quiesced the attachment: nothing is wrong with a " +
				"node that passes a message, and a quiesced one takes none of its other mail")
		}
		awaitState(t, "c1's event to reach the dead-letter subject", func() bool {
			return equalStrings(labelsOf(deadLetters(q, "t", "g")), []string{"a"})
		})
		keptUnhandled(ctx, t, q, "t", "g", "a")
	})

	t.Run("batch_key_failure_falls_back_to_unique_key", func(t *testing.T) {
		t.Parallel()
		// Key derivation must never block delivery — a key function that
		// blows up degrades to per-event partitions.
		//
		// QUEUED BEFORE THE ATTACHMENT, so both events are in ONE drain,
		// where per-event partitions are a question: two drains of one
		// event each give one partition apiece whatever the fallback does.
		// See queueBacklog.
		//
		// Mutation: fall back to one key for every event, and the drain's
		// two events become one partition.
		q := s.start(ctx, t)
		queueBacklog(ctx, t, q, "t", "g", newEvent("a"), newEvent("b"))
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

		batches.awaitSizes(t, "each event to become its own partition", 1, 1)
	})

	t.Run("live_options_mutation_takes_effect_next_cycle", func(t *testing.T) {
		t.Parallel()
		// A hot reload changes linger and batch size with no re-subscription:
		// the consume loop re-reads the options for every batch, including
		// the next one on a subscription IDLE when the reload landed (a pull
		// backend's idle cycle waits in a fetch, so options read BEFORE it
		// open the window at the old length; see queue.BatchOptions). The
		// third event waits as in linger_coalesces_same_key_events_into_one_batch
		// (back-to-back events share one tail fetch). ORDER: first event, no
		// window; reload; the second opens the reloaded window (Lingering);
		// the third is published, the window holds both (InWindow); the
		// clock passes the end. Mutation: read the options before the idle
		// wait, or once at subscribe, and the window is never reported;
		// never counting what it holds fails the wait for the third event.
		lingering := s.needLingering(t)
		inWindow := s.needInWindow(t)
		onClock := s.needLingerClock(t)
		clock := &Clock{}
		q := s.startQueue(ctx, t, onClock(t, clock))
		batches := newBatchJournal()
		opts := queue.NewBatchOptions(0, 20)
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches), opts)

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		batches.awaitSizes(t, "the un-lingered first event", 1)

		opts.Set(clockedWindow.Seconds(), 20)
		publish(ctx, t, q, "t", newConvEvent("b", "c1"))
		// LIVENESS: a backend that keeps the old window, of nothing, never
		// reports one, so stallFor only ends that case's wait.
		awaitState(t, "the reloaded window to outlast what no window collects", func() bool {
			return lingering(q, "t", "g")
		})
		publish(ctx, t, q, "t", newConvEvent("c", "c1"))
		// LIVENESS, as above: a window that never holds the third event
		// fails here.
		awaitState(t, "the reloaded window to hold the second and third events", func() bool {
			return inWindow(q, "t", "g") == 2
		})
		clock.Advance(clockedWindow)
		batches.awaitSizes(t, "the next cycle to honour the new linger", 1, 2)
	})

	// THE THREE CASES BELOW END THEIR WINDOW ON A CLOCK THEY MOVE
	// (Capabilities.LingerClock): each needs a window open when its verb lands
	// and ended after it, and on the wall clock nothing orders the two: a
	// loaded runner lands the verb late and fails a healthy build. So the
	// window is opened (the backend reports it holds the mail,
	// Capabilities.Lingering), the verb lands, the consumer takes it in
	// (Capabilities.Parked, or Settle behind a stop, which leaves no
	// consumer to park), and only then is the window ended, at clockedWindow:
	// the clock moves behind a signal, never elapsed time.

	t.Run("stop_cancels_pending_lingered_flush", func(t *testing.T) {
		t.Parallel()
		// A stop landing while a window holds mail ends the window
		// undelivered; the window's end afterwards delivers nothing.
		// ORDER: the window opens (Lingering), the stop lands, the queue
		// settles (Settle: on JetStream its consume loop has ended), then
		// the clock passes the window's end.
		// Mutation: a Stop that leaves the client's consumers attached (the
		// window still open on the twin, which never settles; a batch
		// dispatched on JetStream past every gate the stop shuts) and the
		// batch is delivered, not retained.
		backlog := s.needBacklog(t)
		lingering := s.needLingering(t)
		onClock := s.needLingerClock(t)
		clock := &Clock{}
		q := s.startQueue(ctx, t, onClock(t, clock))
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(clockedWindow.Seconds(), 20))

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		// LIVENESS: a backend that ignores the linger never reports a
		// window, so stallFor only ends that case's wait.
		awaitState(t, "the window to hold the published event", func() bool {
			return lingering(q, "t", "g")
		})
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := settleStarted(ctx, t); err != nil {
			t.Fatalf("the stopped queue did not settle before its window's end: %v; handlers saw %v",
				err, batches.all())
		}
		clock.Advance(clockedWindow)
		batches.staysAt(ctx, t, 0, "a stopped queue flushed its linger window")

		// RETAINED, not dropped: the linger buffer IS the backlog, and a
		// subscription's mail outlives every attachment.
		if got := labelsOf(backlog(q, "t", "g")); !equalStrings(got, []string{"a"}) {
			t.Fatalf("backlog after Stop = %v, want [a]", got)
		}
	})

	t.Run("pause_during_linger_retains_pending", func(t *testing.T) {
		t.Parallel()
		// A graceful drain must keep whatever was mid-window, not destroy it.
		// ORDER: the window opens (Lingering), the pause lands, the consumer
		// parks on it (Parked), then the clock passes the window's end, so
		// what holds the mail is the pause, never the clock.
		// Mutation: let a paused consumer dispatch (on the twin, drop the
		// pause from the gate a window's flush reads; on JetStream, drop the
		// gates behind the drain, after it and before each partition) and
		// the batch is delivered. Drop JetStream's gate from the drain too,
		// or from the drain alone, and the consumer never parks; keep the
		// twin's window on the wall clock, whatever clock it was handed, and
		// it never settles.
		backlog := s.needBacklog(t)
		lingering := s.needLingering(t)
		onClock := s.needLingerClock(t)
		clock := &Clock{}
		q := s.startQueue(ctx, t, onClock(t, clock))
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(clockedWindow.Seconds(), 20))

		publish(ctx, t, q, "t", newConvEvent("a", "c1"))
		// LIVENESS, as in the stop case above.
		awaitState(t, "the window to hold the published event", func() bool {
			return lingering(q, "t", "g")
		})
		if err := q.PauseDelivery(ctx); err != nil {
			t.Fatalf("PauseDelivery: %v", err)
		}
		s.awaitParked(ctx, t, q, "t", "g")
		clock.Advance(clockedWindow)
		batches.staysAt(ctx, t, 0, "a paused queue flushed its linger window")

		if got := labelsOf(backlog(q, "t", "g")); !equalStrings(got, []string{"a"}) {
			t.Fatalf("backlog after a pause mid-window = %v, want [a]", got)
		}
	})

	t.Run("a_hold_during_linger_is_released_in_place", func(t *testing.T) {
		t.Parallel()
		// The reversible half of the case above: a PauseTopic HOLD is lifted
		// by ResumeTopic and the attachment must carry on. A backend ending
		// its consume loop on a hold taken mid-window leaves the seat
		// ATTACHED, owning its lease, reading nothing, looking healthy;
		// other hold assertions pause BEFORE the window. ORDER: window opens
		// (Lingering), hold lands, consumer parks (Parked), clock passes the
		// end, nothing delivered; hold lifts, the batch then arrives.
		// Mutation: let a held consumer dispatch (the twin's gate without
		// its holds; JetStream's gates behind the drain), and the batch is
		// delivered; end JetStream's loop on the hold, or read its gate only
		// at the window's end, and it never parks; a wall-clock window
		// never ends.
		lingering := s.needLingering(t)
		onClock := s.needLingerClock(t)
		clock := &Clock{}
		q := s.startQueue(ctx, t, onClock(t, clock))
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t.hold", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(clockedWindow.Seconds(), 20))
		// LIVENESS: a backend whose window never opens fails each of these
		// waits, at stallFor.
		awaitWindow := func(what string) {
			t.Helper()
			awaitState(t, what, func() bool { return lingering(q, "t.hold", "g") })
		}

		// Open the window, then take the hold while it is open.
		publish(ctx, t, q, "t.hold", newConvEvent("during", "c1"))
		awaitWindow("the window to hold the published event")
		if err := q.PauseTopic(ctx, "t.hold", "g", attachmentOf(ctx, t, q, "t.hold", "g"), "test"); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		s.awaitParked(ctx, t, q, "t.hold", "g")
		clock.Advance(clockedWindow)
		batches.staysAt(ctx, t, 0, "a held attachment dispatched its window")

		if err := q.ResumeTopic(ctx, "t.hold", "g", attachmentOf(ctx, t, q, "t.hold", "g"), "test"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		awaitWindow("the held event to open a window once the hold lifts")
		clock.Advance(clockedWindow)
		batches.awaitSizes(t, "the held batch once the hold lifts", 1)

		// And the attachment is still ALIVE, not merely flushed once:
		// something published after the resume must arrive too.
		publish(ctx, t, q, "t.hold", newConvEvent("after", "c1"))
		awaitWindow("an event published after the hold lifted to open a window")
		clock.Advance(clockedWindow)
		batches.awaitSizes(t, "an event published after the hold lifted", 1, 1)
	})

	t.Run("an_event_published_while_a_window_is_delivered_waits_its_own", func(t *testing.T) {
		t.Parallel()
		// A window's flush delivers what the window collected, and only that
		// goes without a window: an event published after the flush began has
		// waited for nothing, so it opens a window of its own. Its handler
		// publishes it while the flushed batch is being delivered, so it
		// lands inside the flush's own delivery — where the twin's flush let
		// its skip run on over the whole drain and delivered it at once, the
		// race behind the hold case's last step (#983).
		//
		// Mutation: skip the linger for the whole drain a flush starts rather
		// than for the events its window collected (the twin's flushWindow),
		// and the event published inside the delivery never lingers.
		lingering := s.needLingering(t)
		onClock := s.needLingerClock(t)
		clock := &Clock{}
		q := s.startQueue(ctx, t, onClock(t, clock))
		batches := newBatchJournal()
		record := recordingBatchHandler(batches)
		published := make(chan error, 1)
		var once sync.Once
		subscribeBatch(ctx, t, q, "t.inflush", "g", func(hctx context.Context, evs []*events.Event) queue.Result {
			res := record(hctx, evs)
			once.Do(func() { published <- q.Publish(ctx, "t.inflush", newConvEvent("published-in-delivery", "c1")) })
			return res
		}, queue.NewBatchOptions(clockedWindow.Seconds(), 20))

		publish(ctx, t, q, "t.inflush", newConvEvent("first", "c1"))
		awaitState(t, "the window to hold the first event", func() bool { return lingering(q, "t.inflush", "g") })
		clock.Advance(clockedWindow)
		batches.awaitSizes(t, "the first window's batch", 1)
		if err := <-published; err != nil {
			t.Fatalf("Publish from inside the delivery: %v", err)
		}
		// LIVENESS: a backend that delivered it with no window fails here,
		// at stallFor; on the case's clock a window cannot end on its own.
		awaitState(t, "the event published inside the delivery to open a window of its own", func() bool {
			return lingering(q, "t.inflush", "g")
		})
		clock.Advance(clockedWindow)
		batches.awaitSizes(t, "the event published inside the delivery, once its own window ends", 1, 1)
	})

	t.Run("an_aged_conversation_dispatches_before_a_fresher_one", func(t *testing.T) {
		t.Parallel()
		// Between-partition fairness, asserted against the BACKEND, not the
		// ordering function. Receive order alone starves a quiet
		// conversation behind a hot one under deferral: its requeued copies
		// re-enter AFTER whatever arrived during the hot one's turn.
		// Timestamps carry the aging signal and survive requeue, so the
		// conversation that has waited longest must go first. Every other
		// batch subtest publishes in timestamp order, hiding the difference,
		// so this puts them in conflict: the hot conversation ARRIVES first
		// and the quiet one is OLDER.
		q := s.start(ctx, t)

		now := time.Now().UTC()
		hot := newConvEvent("hot", "hot")
		hot.Timestamp = now
		quiet := newConvEvent("quiet", "quiet")
		quiet.Timestamp = now.Add(-time.Minute)

		// Queued before anything attaches, so both are in one drain as two
		// partitions; delivered one at a time there is no dispatch order
		// to observe. See queueBacklog. A hold taken AFTER subscribing (the
		// shape holdForOneBatch documents) would let `hot` race its own
		// return and `quiet` go alone, ordering nothing.
		queueBacklog(ctx, t, q, "topic.age", "grp", hot, quiet)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "topic.age", "grp", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		batches.await(t, "the conversation that has waited longest to go first",
			func(got [][]string) bool {
				return len(got) == 2 &&
					equalStrings(got[0], []string{"quiet"}) &&
					equalStrings(got[1], []string{"hot"})
			})
	})

	t.Run("within_a_partition_events_are_ordered_by_timestamp", func(t *testing.T) {
		t.Parallel()
		// A partition reads in chronological order however the broker handed
		// the events over: a message that went back returns at the head on
		// one backend and behind fresh mail on the other
		// (a_redelivered_event_rejoins_its_conversation_in_timestamp_order).
		// So order inside a partition comes from the timestamps, which
		// survive requeue, never delivery order; this case makes the two
		// disagree. QUEUED BEFORE THE ATTACHMENT: the subject is what a
		// backend does with two events it holds; see queueBacklog.
		q := s.start(ctx, t)

		t0 := time.Now().UTC()
		late := newConvEvent("late", "c1")
		late.Timestamp = t0.Add(time.Minute)
		early := newConvEvent("early", "c1")
		early.Timestamp = t0

		// Queued newest-first: arrival order is the reverse of
		// chronological order.
		queueBacklog(ctx, t, q, "t", "g", late, early)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "t", "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(lingerFor.Seconds(), 20))

		batches.await(t, "the conversation to arrive in timestamp order", func(got [][]string) bool {
			return len(got) == 1 && equalStrings(got[0], []string{"early", "late"})
		})
	})

	t.Run("a_redelivered_event_rejoins_its_conversation_in_timestamp_order", func(t *testing.T) {
		t.Parallel()
		// A message that went back to the broker takes its CHRONOLOGICAL
		// place among its partition's fresh mail, whichever end of the
		// mailbox it returned to, with its published stamp.
		// THREE EVENTS, SO IT BELONGS IN THE MIDDLE: two more arrive, one
		// stamped before it and one after, all in the past so a redelivery
		// stamped afresh sorts LAST; with two the twin's head return was
		// already in order. A FAILED delivery waits out its backoff
		// (queue.OrderForDispatch), so the message goes back by DEFERRAL:
		// returned at once on every backend, it quiesces the attachment
		// until the case resumes it with both fresh events in the mailbox.
		// The window bounds only the broker TAKING the return (handBackLinger).
		quiescing := s.needQuiescing(t)
		q := s.start(ctx, t)
		const topic, group, conv = "t.rejoin", "g", "c1"

		t0 := time.Now().UTC()
		before := newConvEvent("before", conv)
		before.Timestamp = t0.Add(-2 * time.Minute)
		returned := newConvEvent("returned", conv)
		returned.Timestamp = t0.Add(-time.Minute)
		after := newConvEvent("after", conv)
		after.Timestamp = t0.Add(-30 * time.Second)

		var handedBack atomic.Bool
		first := newJournal()
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, group,
			func(hctx context.Context, evs []*events.Event) queue.Result {
				if !handedBack.CompareAndSwap(false, true) {
					batches.record(evs)
					return queue.Ack()
				}
				// The rest of the conversation lands while this one is
				// in flight. A failure is recorded rather than fataled:
				// this is not the test goroutine.
				for _, ev := range []*events.Event{before, after} {
					if err := q.Publish(hctx, topic, ev); err != nil {
						first.record("publish failed: " + err.Error())
						return queue.Ack()
					}
				}
				first.record(strings.Join(labelsOf(evs), ","))
				return queue.Defer(queue.DeferSeatMoved, "hand it back while its conversation moves on")
			}, queue.NewBatchOptions(handBackLinger.Seconds(), 20))

		publish(ctx, t, q, topic, returned)
		first.awaitLabels(t, "the first delivery to be handed back", "returned")
		awaitState(t, "the deferral to quiesce the attachment", func() bool {
			return quiescing(q, topic, group)
		})
		if resumed, err := q.Unquiesce(ctx, topic, group); err != nil || !resumed {
			t.Fatalf("Unquiesce = (%v, %v), want (true, nil)", resumed, err)
		}

		batches.await(t, "the conversation to come round again", func(got [][]string) bool {
			return len(got) >= 1
		})
		got := batches.all()[0]
		want := []string{"before", "returned", "after"}
		switch {
		case equalStrings(got, want):
		case len(got) != len(want):
			t.Fatalf("the first handler call after the hand-back carried %v, want all of "+
				"%v: the handed-back event and the two that arrived while it was in "+
				"flight were all in the mailbox before the attachment could fetch "+
				"again, so either the drain left behind mail the broker already held, "+
				"or the broker took longer than handBackLinger (%s) to take back a "+
				"message it had already been sent", got, want, handBackLinger)
		default:
			t.Fatalf("the conversation came round as %v, want %v: a partition is ordered "+
				"by event timestamp, and the handed-back event belongs between the two "+
				"that arrived while it was in flight — this is the order the backend "+
				"returned them in, or a redelivery stamped as though it were new",
				got, want)
		}
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
				return queue.Defer(queue.DeferSeatMoved, "seat is not owned here")
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		// Held while publishing so all three land in ONE batch as three
		// partitions: delivered one at a time, the multi-partition loop
		// this covers never runs. The hold must precede the attachment, as
		// holdForOneBatch states: a first publish handed straight back
		// returns BEHIND later ones on JetStream.
		//
		// The DRAIN side is an assumption: nothing obliges a backend to hand
		// all three partitions to one handler call. If this recurs, read the
		// BACKLOG ORDER first: a deferral pushes its partition to the front,
		// so it tells a split (timing) from partitions handled past a
		// deferral (the defect this case catches).
		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(ctx, t, 1, "the deferral did not stop the batch")

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
		// whose inbox had just been held for a detached executor, which is
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
				if err := q.PauseTopic(hctx, "topic.hold", "grp", attachmentOf(hctx, t, q, "topic.hold", "grp"), "queuetest-park"); err != nil {
					t.Errorf("PauseTopic: %v", err)
				}
				return queue.Ack()
			}, convKey, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}

		fill("a", "b", "c")

		seen.awaitLabels(t, "only the first partition to be handled", "a")
		seen.staysAt(ctx, t, 1, "the hold did not stop the batch")
		awaitState(t, "the undispatched partitions to return", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.hold", "grp")), []string{"b", "c"})
		})
	})

	t.Run("a_detach_taken_mid_batch_stops_the_rest", func(t *testing.T) {
		t.Parallel()
		// THE FOURTH CONDITION. Four things stop a partition loop between
		// partitions: a deferral, a hold, a pause, a DETACH (see
		// queue.DeliveriesLeft). A detach is the only one that is not a
		// FLAG: a consumer leaves the member list, which a loop holding the
		// consumer directly never re-reads, so the twin would run and ACK
		// the remaining partitions on a consumer already given up. That
		// matters most because a detach is the fenced release: internal/node
		// detaches on a lost seat lease to abandon in-flight work.
		// Undispatched partitions come back charged one delivery each, in
		// retained mail that outlives every attachment; that cost is
		// an_undispatched_partition_pays_for_its_hand_back's subject, not
		// this case's: it is about the loop STOPPING.
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
		seen.staysAt(ctx, t, 1, "the detach did not stop the batch")
		awaitState(t, "the undispatched partitions to return", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.detach", "grp")), []string{"b", "c"})
		})
	})

	t.Run("a_pause_taken_mid_batch_stops_the_rest", func(t *testing.T) {
		t.Parallel()
		// THE LAST OF THE FOUR. Deleting the process-wide delivery pause
		// from the between-partition guard fails only this case, on either
		// backend: a condition the contract names and no case asks about is
		// one delete away from gone, however it rides the hold's predicate.
		//
		// THE DRAIN PAUSE IS A DIFFERENT GATE from the hold (per topic and
		// group, reversible) and the quiesce (per attachment, reversible):
		// it is per PROCESS and one-way, the shutdown drain waiting out
		// in-flight handlers, and landing mid-batch is its ordinary case.
		// pause_during_linger_retains_pending covers the same verb at the
		// OTHER gate, a window open when the pause lands.
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
		seen.staysAt(ctx, t, 1, "the drain pause did not stop the batch")
		awaitState(t, "the undispatched partitions to return", func() bool {
			return equalStrings(convsOf(backlog(q, "topic.drain", "grp")), []string{"b", "c"})
		})
	})

	t.Run("an_undispatched_partition_pays_for_its_hand_back", func(t *testing.T) {
		t.Parallel()
		// WHAT THE REST OF A STOPPED DRAIN COSTS: one delivery each, as the
		// partition that stopped it (the cases above leave the charge
		// unasserted). JetStream FETCHES a drain, so its counter has moved on
		// every message before the loop stops, and the remainder goes back
		// through the failure budget check. The twin spliced undispatched
		// partitions back untouched, so a message on a seat whose drains
		// keep stopping rode round for ever on a counter that never moved.
		// queue.DeliveriesLeft states one rule; this certifies it, not
		// either mechanism.
		//
		// A HOLD, NOT A DEFERRAL: ResumeTopic clears it in place, so the
		// held-back partition returns to a handler that can read its count.
		newQueueWithAttempts := s.needAttempts(t)
		const attempts = 5
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, attempts))

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
					if err := q.PauseTopic(hctx, topic, group, attachmentOf(hctx, t, q, topic, group), "queuetest-charged"); err != nil {
						t.Errorf("PauseTopic: %v", err)
					}
				}
				j.record(conv)
				return queue.Ack()
			}, queue.NewBatchOptions(lingerFor.Seconds(), 20))

		fill("a", "b")
		j.awaitLabels(t, "only the first partition to be handled", "a")
		j.staysAt(ctx, t, 1, "the hold did not stop the batch")

		if err := q.ResumeTopic(ctx, topic, group, attachmentOf(ctx, t, q, topic, group), "queuetest-charged"); err != nil {
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
		// The precondition is asserted, not presumed: comparing two
		// DIFFERENT messages presumes the same delivery count on entry, and
		// an early delivery of the stopping partition (what holdForOneBatch
		// prevents) makes the readings EQUAL though the charge was right.
		// So it reads a full budget less one (delivered once, the fill held
		// everything back) and the handed-back one exactly one less (ONE
		// delivery, not two). Each failure names its half.
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

	t.Run("a_drain_the_backend_hands_back_itself_is_kept_with_the_unhandled_deliveries", func(t *testing.T) {
		t.Parallel()
		// WHERE THE BACKEND'S OWN HAND-BACK KEEPS A MESSAGE IT SPENT. A
		// partition the consumer may not run mid-drain (here, after the
		// deferral of the one before it) is handed back by the backend, no
		// handler run, spending a delivery as every return does
		// (an_undispatched_partition_pays_for_its_hand_back). On the last
		// delivery it retires the message, but a hand-back is no failure:
		// the copy is kept with the group's unhandled deliveries, beside the
		// deferred partition's own. A BUDGET OF ONE makes the drain's one
		// delivery its last; filled while held so the two are one drain.
		// Mutation: charge the backend's own hand-back of an undispatched
		// partition as a failure (the twin's restoreLocked, JetStream's
		// handBackUndispatched), and b's copy is kept with the poison.
		newQueueWithAttempts := s.needAttempts(t)
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, 1))

		const topic, group = "topic.handed_drain", "grp"
		fill := holdForOneBatch(ctx, t, q, topic, group)
		j := newJournal()
		subscribeBatch(ctx, t, q, topic, group,
			func(_ context.Context, evs []*events.Event) queue.Result {
				j.record(firstConv(t, evs))
				return queue.Defer(queue.DeferShedding, "the node sheds")
			}, queue.NewBatchOptions(lingerFor.Seconds(), 20))

		fill("a", "b")
		j.awaitLabels(t, "the first partition to be deferred", "a")
		keptAs(ctx, t, q, topic, group, false, "a", "b")
		j.staysAt(ctx, t, 1, "a partition the backend handed back was run")
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
				// Grow the tail under the loop like a concurrent producer,
				// but ONCE: publishing on every invocation feeds the drain
				// loop of a backend that drains inline, which then never
				// returns and wedges the suite's binary.
				if !grown {
					grown = true
					_ = q.Publish(hctx, "topic.c", newConvEvent("late", "late"))
				}
				return queue.Defer(queue.DeferSeatMoved, "seat is not owned here")
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
// the hold. The hold must precede the attachment: a case holds, subscribes,
// then fills. One taken after SubscribeBatch cannot retract a fetch in
// flight: an idle pull attachment sits in a long poll, so an event published
// just after is delivered and handed back by the hold's own guard, reaching
// the next drain with a delivery SPENT (on JetStream behind later events).
// THE DRAIN SIDE IS AN ASSUMPTION: nothing obliges a backend to hand the events
// over as ONE batch, and one delivering them singly passes the cases built on
// this vacuously; only an_undispatched_partition_pays_for_its_hand_back can
// tell. Read a green asynchronous backend as "did not contradict".
func holdForOneBatch(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string) func(convs ...string) {
	t.Helper()
	if err := q.PauseTopic(ctx, topic, group, attachmentOf(ctx, t, q, topic, group), "queuetest-fill"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	return func(convs ...string) {
		t.Helper()
		for _, conv := range convs {
			publish(ctx, t, q, topic, newConvEvent(conv, conv))
		}
		if err := q.ResumeTopic(ctx, topic, group, attachmentOf(ctx, t, q, topic, group), "queuetest-fill"); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
	}
}

// queueBacklog puts events in a subscription's mailbox before anything is
// attached to it, so the first drain finds them ALREADY AVAILABLE.
//
// For a case whose subject is what a drain does with the mail it holds (the
// partitioning, the order, the cap), not how an arrival joins an open window.
// Published to an attached consumer, such a case asserts that every publish
// lands inside one linger window, a property of the runner's round trips that
// a loaded runner loses. The subscription is created first because a publish
// to a topic with no subscription is dropped (queue.EventQueue.EnsureSubscription).
// How fast a backend collects the held mail is unobservable here: the case's
// window bounds it.
func queueBacklog(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string, evs ...*events.Event) {
	t.Helper()
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription(%s, %s): %v", topic, group, err)
	}
	for _, ev := range evs {
		publish(ctx, t, q, topic, ev)
	}
}

// runContractPolicy exercises the pure policy functions in the contract
// package — NOT any backend's use of them.
//
// A PASS under TestConformance reads like backend coverage, but every case
// calls internal/queue directly, so it CANNOT certify that a backend calls
// that function, or correctly. The backend side is owned by Batch/
// an_aged_conversation_dispatches_before_a_fresher_one (aging),
// within_a_partition_events_are_ordered_by_timestamp (order),
// linger_partitions_by_key_preserving_arrival_order (membership) and
// max_batch_chunks_oversized_buffers (the cap). The linger ceiling has none:
// asserting it costs a minute's sleep. These run here only because
// internal/queue has no test binary; when it gets one, this group moves there.
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

	t.Run("a_priority_partition_dispatches_before_an_older_one", func(t *testing.T) {
		t.Parallel()
		// A drain that took a laned topic's priority mail and filled in
		// behind it with older background mail dispatches the priority
		// partition first: the lane is taken first so a person waiting is
		// answered first, and aging alone would hand that back.
		t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		older := newConvEvent("a", "background")
		older.Timestamp = t0
		person := newConvEvent("b", "direct")
		person.Timestamp = t0.Add(time.Hour)
		queue.Prioritize(person)
		newer := newConvEvent("c", "news")
		newer.Timestamp = t0.Add(2 * time.Hour)

		got := orderedKeys([]*events.Event{older, newer, person})
		if !equalStrings(got, []string{"direct", "background", "news"}) {
			t.Fatalf("dispatch order = %v, want [direct background news]", got)
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
