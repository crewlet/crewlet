package queuetest

import (
	"context"
	"errors"
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

	t.Run("a_priority_partition_folds_its_mail_from_behind_the_backlog", func(t *testing.T) {
		t.Parallel()
		// THE FOLD. The person's conversation also has mail on the topic,
		// behind a backlog deeper than the drain: with a cap of two the
		// follow-up is in no drain the lane brings forward. Delivered
		// whole, the person's partition carries it, and it is never run a
		// second time when the topic's head reaches where it was.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(5), followUp(), person())...)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(0, 2))
		batches.await(t, "the person's partition first, whole", func(got [][]string) bool {
			return len(got) > 0 && sameSet(got[0], []string{"followup", "person"})
		})
		batches.await(t, "every event settled exactly once", func(got [][]string) bool {
			return equalStrings(settled(got), everyLabel(5))
		})
		if backlogOf := s.optionalBacklog(t); backlogOf != nil {
			if left := backlogOf(q, topic, "g"); len(left) != 0 {
				t.Fatalf("after every partition was acked the subscription retains %v: a "+
					"folded event was handed to an acked partition and never settled", labelsOf(left))
			}
		}
	})

	t.Run("a_requeued_priority_partition_is_folded_whole_again", func(t *testing.T) {
		t.Parallel()
		// A PARK splits what it requeues: each event is republished as it
		// is, so the person's message returns to the lane and the
		// follow-up to the topic's TAIL, behind whatever arrived since.
		// The next drain that takes the person's message folds the
		// follow-up back in, so the requeued partition runs once, whole.
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(5), followUp(), person())...)
		batches := newBatchJournal()
		var once sync.Once
		handler := func(ctx context.Context, evs []*events.Event) queue.Result {
			requeue := false
			if slices.Contains(labelsOf(evs), "person") {
				once.Do(func() { requeue = true })
			}
			if requeue {
				for _, ev := range evs {
					if err := q.Publish(ctx, topic, ev); err != nil {
						t.Errorf("requeue %s: %v", ev.Type, err)
						return queue.Nak(err)
					}
				}
				return queue.Ack()
			}
			batches.record(evs)
			return queue.Ack()
		}
		subscribeBatch(ctx, t, q, topic, "g", handler, queue.NewBatchOptions(0, 2))
		batches.await(t, "every event settled exactly once", func(got [][]string) bool {
			return equalStrings(settled(got), everyLabel(5))
		})
		for _, batch := range batches.all() {
			if slices.Contains(batch, "followup") && !slices.Contains(batch, "person") {
				t.Fatalf("the requeued follow-up ran as %v, apart from the person's message "+
					"it was requeued with", batch)
			}
		}
	})

	t.Run("a_held_priority_partition_is_folded_whole_when_the_hold_lifts", func(t *testing.T) {
		t.Parallel()
		// A seat held (paused, parked, or busy with its executor) keeps
		// both mailboxes; when the hold lifts, the person's message comes
		// first and brings its conversation's topic mail with it.
		q := s.start(ctx, t)
		topic := inbox()
		if _, err := q.EnsureSubscription(ctx, topic, "g"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if err := q.PauseTopic(ctx, topic, "g", holdReason); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, topic, "g", recordingBatchHandler(batches),
			queue.NewBatchOptions(0, 2))
		for _, ev := range append(backlog(5), followUp(), person()) {
			publish(ctx, t, q, topic, ev)
		}
		batches.staysAt(t, 0, "a delivery past the topic's hold")
		if err := q.ResumeTopic(ctx, topic, "g", holdReason); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		batches.await(t, "the person's partition first, whole", func(got [][]string) bool {
			return len(got) > 0 && sameSet(got[0], []string{"followup", "person"})
		})
		batches.await(t, "every event settled exactly once", func(got [][]string) bool {
			return equalStrings(settled(got), everyLabel(5))
		})
	})

	t.Run("a_nak_leaves_folded_mail_where_it_was_and_settles_it_once", func(t *testing.T) {
		t.Parallel()
		// A failed priority partition hands back only what the drain took.
		// The follow-up it folded was read, not delivered: the handler is
		// told no headroom for it, nothing returns it, and it is settled
		// exactly once, by whichever delivery is acked with it.
		q := s.start(ctx, t)
		topic := inbox()
		follow := followUp()
		queueBacklog(ctx, t, q, topic, "g", append(backlog(5), follow, person())...)
		batches := newBatchJournal()
		var (
			mu     sync.Mutex
			failed bool
		)
		handler := func(ctx context.Context, evs []*events.Event) queue.Result {
			mu.Lock()
			first := !failed && slices.Contains(labelsOf(evs), "person")
			if first {
				failed = true
			}
			mu.Unlock()
			if first {
				if !slices.Contains(labelsOf(evs), "followup") {
					t.Errorf("the person's first delivery was %v, want the follow-up folded "+
						"into it", labelsOf(evs))
				}
				if left, stated := queue.DeliveriesLeftFor(ctx, follow.ID); stated {
					t.Errorf("the folded follow-up was stated %d deliveries of headroom; it "+
						"was read, not delivered, so none is spent and none is stated", left)
				}
				return queue.Nak(errFoldFailed)
			}
			batches.record(evs)
			return queue.Ack()
		}
		subscribeBatch(ctx, t, q, topic, "g", handler, queue.NewBatchOptions(0, 2))
		batches.await(t, "every event settled exactly once", func(got [][]string) bool {
			return equalStrings(settled(got), everyLabel(5))
		})
		if backlogOf := s.optionalBacklog(t); backlogOf != nil {
			if left := backlogOf(q, topic, "g"); len(left) != 0 {
				t.Fatalf("after every partition was acked the subscription retains %v", labelsOf(left))
			}
		}
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
		batches.staysAt(t, 0, "a priority event delivered past its topic's hold")
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

// errFoldFailed is the failure a fold case's handler reports on purpose.
var errFoldFailed = errors.New("queuetest: the priority partition failed on purpose")

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
