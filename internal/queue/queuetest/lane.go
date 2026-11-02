package queuetest

import (
	"strconv"
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
