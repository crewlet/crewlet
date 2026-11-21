package queuetest

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// runBacklog covers [queue.EventQueue.Backlog], the read a seat's router makes
// of its held inbox while the seat's run works (internal/engine): it consumes
// nothing and spends no delivery, reads a laned topic's lane first, and stops
// at its limit. THE TOPIC IS A SEAT'S INBOX, the one topic the contract lanes.
func (s *suite) runBacklog(t *testing.T) {
	ctx := t.Context()
	inbox := func() string { return topics.AgentInbox(uuid.New()) }
	person := func() *events.Event {
		ev := newConvEvent("person", "direct")
		queue.Prioritize(ev)
		return ev
	}
	read := func(t *testing.T, q queue.EventQueue, topic string, limit int) []string {
		t.Helper()
		evs, err := s.caps.Inspect(t, q).Backlog(ctx, topic, "g", limit)
		if err != nil {
			t.Fatalf("Backlog(%s, limit %d): %v", topic, limit, err)
		}
		return labelsOf(evs)
	}

	t.Run("a_held_inbox_is_read_without_consuming_it_or_spending_a_delivery", func(t *testing.T) {
		t.Parallel()
		// What a router leans on: it reads a held seat's mail as often as it
		// is nudged, and the mail reaches the seat after the hold exactly as
		// if nobody had looked. Each delivery's budget is compared with one
		// published after the reads, which no read could have spent.
		//
		// Mutation: read through a consumer that acks, or one that counts a
		// delivery, and the held mail is gone or arrives with less budget.
		q := s.start(ctx, t)
		topic := inbox()
		var (
			mu   sync.Mutex
			left = map[string]int{}
		)
		seen := newJournal()
		subscribe(ctx, t, q, topic, "g", func(hctx context.Context, ev *events.Event) queue.Result {
			n, known := queue.DeliveriesLeft(hctx)
			mu.Lock()
			if !known {
				n = -1
			}
			left[labelOf(ev)] = n
			mu.Unlock()
			seen.record(labelOf(ev))
			return queue.Ack()
		})
		s.holdTakenIn(ctx, t, q, topic, "g", holdReason)
		publish(ctx, t, q, topic, newConvEvent("a", "c-a"))
		publish(ctx, t, q, topic, newConvEvent("b", "c-b"))
		publish(ctx, t, q, topic, person())
		want := []string{"person", "a", "b"}
		for i := range 2 {
			if got := read(t, q, topic, backlogRead); !equalStrings(got, want) {
				t.Fatalf("read %d of the held inbox = %v, want %v: the lane's mail first, then the "+
					"inbox's in order, and every read the same", i+1, got, want)
			}
		}
		seen.staysAt(ctx, t, 0, "a read of a held inbox delivered its mail")
		publish(ctx, t, q, topic, newConvEvent("late", "c-late"))
		if err := q.ResumeTopic(ctx, topic, "g", attachmentOf(ctx, t, q, topic, "g"), holdReason); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		seen.awaitLabelsInAnyOrder(t, "the held mail after the hold lifts", "person", "a", "b", "late")
		mu.Lock()
		defer mu.Unlock()
		for _, label := range want {
			if left[label] != left["late"] {
				t.Errorf("%s arrived with %d deliveries left, and mail no read saw with %d: a read "+
					"spent a delivery", label, left[label], left["late"])
			}
		}
	})

	t.Run("a_read_stops_at_its_limit_lane_first", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		topic := inbox()
		evs := []*events.Event{person()}
		for i := range 3 {
			label := "background-" + strconv.Itoa(i)
			evs = append(evs, newConvEvent(label, label))
		}
		queueBacklog(ctx, t, q, topic, "g", evs...)
		for limit, want := range map[int][]string{
			1: {"person"},
			2: {"person", "background-0"},
			9: {"person", "background-0", "background-1", "background-2"},
		} {
			if got := read(t, q, topic, limit); !equalStrings(got, want) {
				t.Errorf("Backlog with limit %d = %v, want %v", limit, got, want)
			}
		}
	})

	t.Run("a_limit_below_one_is_refused", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		topic := inbox()
		queueBacklog(ctx, t, q, topic, "g", person())
		if evs, err := q.Backlog(ctx, topic, "g", 0); err == nil {
			t.Fatalf("Backlog with limit 0 answered %v and no error: a read of nothing is a caller's "+
				"bug, never an empty mailbox", labelsOf(evs))
		}
	})

	t.Run("a_subscription_that_does_not_exist_holds_nothing", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		if got := read(t, q, inbox(), backlogRead); len(got) != 0 {
			t.Fatalf("a mailbox nobody made holds %v", got)
		}
	})
}
