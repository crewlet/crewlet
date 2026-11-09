package queuetest

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// runLane covers a laned topic's priority lane ([queue.HasLane]): a marked
// event published behind a backlog is handed to the subscription before the
// backlog, on every verb that consumes, and the lane is one subscription with
// its topic for every verb that manages one.
//
// THE TOPIC IS A SEAT'S INBOX, because that is the only topic the contract
// lanes; a case on any other name would certify nothing. Each case takes a
// fresh seat id, so no two share a mailbox on a backend whose broker outlives
// the case.
func (s *suite) runLane(t *testing.T) {
	ctx := t.Context()
	inbox := func() string { return topics.AgentInbox(uuid.New()) }
	// backlog is n background events on n conversations of their own, so
	// no merge can carry the priority event along with any of them.
	backlog := func(n int) []*events.Event {
		out := make([]*events.Event, 0, n)
		for i := range n {
			label := "background-" + strconv.Itoa(i)
			out = append(out, newConvEvent(label, label))
		}
		return out
	}
	person := func() *events.Event {
		ev := newConvEvent("person", "direct")
		queue.Prioritize(ev)
		return ev
	}
	// followUp is background mail on the person's own conversation: a
	// colleague's remark in the same thread, say, which rides the topic.
	followUp := func() *events.Event { return newConvEvent("followup", "direct") }
	// everyLabel is the labels a case publishes, each settled exactly once.
	everyLabel := func(background int) []string {
		out := []string{"followup", "person"}
		for i := range background {
			out = append(out, "background-"+strconv.Itoa(i))
		}
		slices.Sort(out)
		return out
	}

	// waitingIs polls a subscription's waiting count until it is want: the
	// count is allowed to trail a publish or a settlement by a moment.
	waitingIs := func(t *testing.T, q queue.EventQueue, topic string, want queue.Waiting) {
		t.Helper()
		var got queue.Waiting
		awaitState(t, fmt.Sprintf("the subscription waiting %+v", want), func() bool {
			w, err := q.Waiting(ctx, topic, "g")
			if err != nil {
				t.Errorf("Waiting: %v", err)
				return true
			}
			got = w
			return got == want
		})
		if got != want {
			t.Fatalf("waiting %+v, want %+v", got, want)
		}
	}

	t.Run("a_priority_event_is_taken_before_the_backlog_it_arrived_behind", func(t *testing.T) {
		t.Parallel()
		// The failure the lane exists for: a person's message published
		// behind a seat's backlog. With a cap of two the message is in the
		// third drain at the earliest by arrival order; the lane puts it in
		// the first, and the dispatch order puts it ahead of the background
		// partition the drain fills in behind it.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(5), person())...)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(0, 2))
		batches.await(t, "the person's message dispatched first", func(got [][]string) bool {
			return len(got) > 0 && equalStrings(got[0], []string{"person"})
		})
	})

	t.Run("a_partition_wake_deeper_than_the_drain_runs_once_in_its_own_turn", func(t *testing.T) {
		t.Parallel()
		// THE DRAIN WINDOW IS THE BOUND of what one turn is handed. The
		// person's conversation also has mail on the topic, behind a
		// backlog deeper than the drain: with a cap of two the follow-up
		// is in no drain the lane brings forward. It is not reached for:
		// the person's turn runs on what the drain took, and the follow-up
		// runs once, as a partition of its own, when the topic's head
		// reaches it. Nothing lost, nothing run twice.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(5), followUp(), person())...)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(0, 2))
		batches.await(t, "every event settled exactly once", func(got [][]string) bool {
			return equalStrings(settled(got), everyLabel(5))
		})
		got := batches.all()
		if !equalStrings(got[0], []string{"person"}) {
			t.Fatalf("the first delivery was %v, want the person's message alone: the "+
				"follow-up was deeper than the drain", got[0])
		}
		for _, batch := range got {
			if slices.Contains(batch, "followup") && len(batch) != 1 {
				t.Fatalf("the follow-up ran as %v, want a partition of its own", batch)
			}
		}
		// POLLED: the journal records a batch inside its handler, before
		// the backend applies the Ack, so the last batch's settlement may
		// still be in flight the moment every label has been seen.
		if backlogOf := s.optionalBacklog(t); backlogOf != nil {
			awaitState(t, "every partition settled, the subscription retaining nothing",
				func() bool { return len(backlogOf(q, topic, "g")) == 0 })
		}
	})

	t.Run("a_requeued_priority_partition_returns_whole", func(t *testing.T) {
		t.Parallel()
		// A PARK REQUEUES WHAT IT WAS HANDED AS ONE UNIT (queue.Together):
		// the person's message and the follow-up the drain took with it
		// both return to the lane and are delivered together again, ahead
		// of the backlog, rather than the marked one alone to the lane and
		// the follow-up to the topic's tail.
		//
		// PARKED AS THE ENGINE PARKS: the inbox is held before the requeue
		// (internal/engine's dispatcher pauses a seat's inbox before every
		// park), and the hold lifts once the copies wait on the lane.
		// Requeued with no hold, the drain after the handler can ask the
		// lane before a broker that signals its consumers asynchronously has
		// counted the copies — a no-wait pull on JetStream answers "no
		// messages" until it has — and the backlog goes first on a race the
		// engine never runs.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(append([]*events.Event{followUp()},
			backlog(5)...), person())...)
		batches := newBatchJournal()
		var once sync.Once
		requeued := make(chan struct{})
		handler := func(ctx context.Context, evs []*events.Event) queue.Result {
			requeue := false
			if slices.Contains(labelsOf(evs), "person") {
				once.Do(func() { requeue = true })
			}
			if !requeue {
				batches.record(evs)
				return queue.Ack()
			}
			if !sameSet(labelsOf(evs), []string{"followup", "person"}) {
				t.Errorf("the person's first delivery was %v, want the follow-up the drain "+
					"took with it", labelsOf(evs))
			}
			if err := q.PauseTopic(ctx, topic, "g", holdReason); err != nil {
				t.Errorf("PauseTopic: %v", err)
				return queue.Nak(err)
			}
			for _, ev := range queue.Together(evs) {
				if err := q.Publish(ctx, topic, ev); err != nil {
					t.Errorf("requeue %s: %v", ev.Type, err)
					return queue.Nak(err)
				}
			}
			close(requeued)
			return queue.Ack()
		}
		subscribeBatch(ctx, t, q, topic, "g", handler, queue.NewBatchOptions(0, 2))
		awaitSignal(t, requeued, "the person's partition requeued under a hold", nil)
		// BOTH COPIES ON THE LANE, the backlog untouched on the topic: what
		// the drain after the hold will see, and the first half of the
		// claim, since a follow-up requeued apart would wait on the topic.
		waitingIs(t, q, topic, queue.Waiting{Priority: 2, Background: 5})
		if err := q.ResumeTopic(ctx, topic, "g", holdReason); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		batches.await(t, "every event settled exactly once", func(got [][]string) bool {
			return equalStrings(settled(got), everyLabel(5))
		})
		got := batches.all()
		if !sameSet(got[0], []string{"followup", "person"}) {
			t.Fatalf("after the requeue the first delivery was %v, want the person's "+
				"partition whole, ahead of the backlog", got[0])
		}
	})

	t.Run("waiting_counts_both_mailboxes_of_a_subscription", func(t *testing.T) {
		t.Parallel()
		// A seat with nothing attached waits for everything it holds, the
		// person's message on the lane counted apart from the rest.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(3), person())...)
		waitingIs(t, q, topic, queue.Waiting{Priority: 1, Background: 3})
		if w, err := q.Waiting(ctx, inbox(), "g"); err != nil || w != (queue.Waiting{}) {
			t.Fatalf("a subscription that does not exist waits %+v (%v), want nothing", w, err)
		}
		other := "lane.none"
		queueBacklog(ctx, t, q, other, "g", backlog(1)[0], person())
		waitingIs(t, q, other, queue.Waiting{Background: 2})
	})

	t.Run("waiting_leaves_out_what_a_handler_is_running_on", func(t *testing.T) {
		t.Parallel()
		// THE TURN'S OWN MAIL IS NOT WAITING. While the person's partition
		// runs, the two background events the drain took with it are still
		// waiting for a handler, and once everything settles nothing is.
		//
		// HELD, then resumed off the test's goroutine: the whole backlog is
		// in one drain, and a backend that dispatches inline runs the
		// blocking handler on whoever resumed, never on the test. The
		// release is a cleanup as well, so a failing case cannot leave a
		// handler blocked under the queue's own stop.
		q := s.start(ctx, t)
		topic := inbox()
		if _, err := q.EnsureSubscription(ctx, topic, "g"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if err := q.PauseTopic(ctx, topic, "g", holdReason); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		started := make(chan struct{})
		release := make(chan struct{})
		var startOnce, releaseOnce sync.Once
		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseAll)
		subscribeBatch(ctx, t, q, topic, "g", func(_ context.Context, evs []*events.Event) queue.Result {
			if slices.Contains(labelsOf(evs), "person") {
				startOnce.Do(func() { close(started) })
				<-release
			}
			return queue.Ack()
		}, queue.NewBatchOptions(0, 10))
		for _, ev := range append(backlog(2), person()) {
			publish(ctx, t, q, topic, ev)
		}
		go func() {
			if err := q.ResumeTopic(context.WithoutCancel(ctx), topic, "g", holdReason); err != nil {
				t.Errorf("ResumeTopic: %v", err)
			}
		}()
		awaitSignal(t, started, "the person's partition running", nil)
		waitingIs(t, q, topic, queue.Waiting{Background: 2})
		releaseAll()
		waitingIs(t, q, topic, queue.Waiting{})
	})

	t.Run("a_single_event_consumer_takes_the_lane_first", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(3), person())...)
		seen := newJournal()
		subscribe(ctx, t, q, topic, "g", recordingHandler(seen))
		seen.await(t, "the person's message delivered first", func(got []string) bool {
			return len(got) > 0 && got[0] == "person"
		})
	})

	t.Run("a_hold_on_the_topic_holds_its_lane", func(t *testing.T) {
		t.Parallel()
		// ONE SUBSCRIPTION: a seat paused, parked or held by its executor
		// holds a person's message with the rest of its mail, and a lane
		// consumed past the hold would start a turn on a stopped seat.
		q := s.start(ctx, t)
		topic := inbox()
		if err := q.PauseTopic(ctx, topic, "g", holdReason); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, "g", recordingBatchHandler(batches),
			queue.DefaultBatchOptions())
		publish(ctx, t, q, topic, person())
		batches.staysAt(ctx, t, 0, "a priority event delivered past its topic's hold")
		if err := q.ResumeTopic(ctx, topic, "g", holdReason); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		batches.await(t, "the held message after the hold lifts", func(got [][]string) bool {
			return len(got) == 1 && equalStrings(got[0], []string{"person"})
		})
	})

	t.Run("the_mark_does_not_reorder_a_topic_without_a_lane", func(t *testing.T) {
		t.Parallel()
		// Every other topic is one mailbox: a marked event waits behind the
		// mail published before it, exactly as an unmarked one does.
		q := s.start(ctx, t)
		topic := "lane.none"
		queueBacklog(ctx, t, q, topic, "g", append(backlog(2), person())...)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(0, 1))
		batches.await(t, "arrival order on a topic with no lane", func(got [][]string) bool {
			return len(got) == 3 &&
				equalStrings(got[0], []string{"background-0"}) &&
				equalStrings(got[1], []string{"background-1"}) &&
				equalStrings(got[2], []string{"person"})
		})
	})

	t.Run("the_lane_is_part_of_its_subscription", func(t *testing.T) {
		t.Parallel()
		// Listed once, as the topic's pair and complete; its mail is the
		// subscription's mail; and a delete takes it with the rest.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", person())
		subs, err := q.ListSubscriptions(ctx, topic)
		if err != nil {
			t.Fatalf("ListSubscriptions: %v", err)
		}
		if len(subs) != 1 || subs[0] != (queue.Subscription{Topic: topic, Group: "g"}) {
			t.Fatalf("listed %+v, want the topic's one complete subscription", subs)
		}
		backlogOf := s.optionalBacklog(t)
		if backlogOf != nil {
			if got := labelsOf(backlogOf(q, topic, "g")); !equalStrings(got, []string{"person"}) {
				t.Fatalf("the subscription retains %v, want the lane's message", got)
			}
		}
		if _, err := q.DeleteSubscription(ctx, topic, "g"); err != nil {
			t.Fatalf("DeleteSubscription: %v", err)
		}
		if subs, err := q.ListSubscriptions(ctx, topic); err != nil || len(subs) != 0 {
			t.Fatalf("after a delete the listing is %+v (%v), want nothing", subs, err)
		}
		if backlogOf != nil {
			if got := backlogOf(q, topic, "g"); len(got) != 0 {
				t.Fatalf("a deleted subscription still retains %v", labelsOf(got))
			}
		}
	})
}

// settled is every label the acked batches carried, sorted: a label twice is an
// event settled twice, and a label missing is one never settled.
func settled(batches [][]string) []string {
	var out []string
	for _, batch := range batches {
		out = append(out, batch...)
	}
	slices.Sort(out)
	return out
}
