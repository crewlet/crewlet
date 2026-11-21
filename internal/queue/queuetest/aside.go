package queuetest

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// asideJournal records each partition a batch handler is handed, how many
// deliveries each event had left at each delivery, and the room the attachment
// stated for aside mail, and sets aside every partition decide names a key for.
type asideJournal struct {
	*batchJournal
	mu    sync.Mutex
	left  map[string][]int
	rooms []int
	seen  map[string]int
}

func newAsideJournal() *asideJournal {
	return &asideJournal{batchJournal: newBatchJournal(), left: map[string][]int{}, seen: map[string]int{}}
}

// handler is a batch handler that journals each partition and answers what
// decide says for it: a key sets it aside, "" acknowledges it. decide is told
// how many times the partition's first event was handed over before.
func (j *asideJournal) handler(decide func(ctx context.Context, evs []*events.Event, before int) string) queue.BatchHandler {
	return func(ctx context.Context, evs []*events.Event) queue.Result {
		room, known := queue.AsideRoom(ctx)
		j.mu.Lock()
		before := j.seen[labelOf(evs[0])]
		for _, ev := range evs {
			j.seen[labelOf(ev)]++
			n, ok := queue.DeliveriesLeftFor(ctx, ev.ID)
			if !ok {
				n = -1
			}
			j.left[labelOf(ev)] = append(j.left[labelOf(ev)], n)
		}
		if !known {
			room = -1
		}
		j.rooms = append(j.rooms, room)
		j.mu.Unlock()
		key := decide(ctx, evs, before)
		j.record(evs)
		if key != "" {
			return queue.Aside(key, "the case sets it aside")
		}
		return queue.Ack()
	}
}

// leftOf is what each delivery of label had left, in order.
func (j *asideJournal) leftOf(label string) []int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]int(nil), j.left[label]...)
}

// asideFirst sets aside, under key, every partition on its first delivery.
func asideFirst(key string) func(context.Context, []*events.Event, int) string {
	return func(_ context.Context, _ []*events.Event, before int) string {
		if before == 0 {
			return key
		}
		return ""
	}
}

// runAside covers [queue.OutcomeAside] and [queue.EventQueue.Readmit]: a
// delivery set aside waits in flight, spending nothing, while the mail behind
// it flows, until a Readmit of its key hands it back at the head of the next
// drain; a release or a stop hands it back at one delivery; and a Readmit
// racing the handler that sets it aside is not lost (a busy seat's mail
// waiting on one of its runs, internal/engine, ADR-0169).
func (s *suite) runAside(t *testing.T) {
	ctx := t.Context()

	t.Run("an_aside_spends_nothing_and_the_mail_behind_it_flows", func(t *testing.T) {
		t.Parallel()
		// Mutation: set aside by handing back, or let the aside lapse into a
		// redelivery, and a comes back before its Readmit or with less left.
		q := s.start(ctx, t)
		j := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", j.handler(func(_ context.Context, evs []*events.Event, before int) string {
			if labelOf(evs[0]) == "a" && before == 0 {
				return "k"
			}
			return ""
		}), nil)
		publish(ctx, t, q, "aside.t", newConvEvent("a", "ca"))
		j.awaitSizes(t, "a, set aside", 1)
		publish(ctx, t, q, "aside.t", newConvEvent("b", "cb"))
		j.awaitSizes(t, "b, past the aside a", 1, 1)
		j.staysAt(ctx, t, 2, "an aside delivery came back before its Readmit")
		n, err := q.Readmit(ctx, "aside.t", "g", "k")
		if err != nil || n != 1 {
			t.Fatalf("Readmit = %d, %v, want 1 and no error", n, err)
		}
		j.awaitSizes(t, "a, readmitted", 1, 1, 1)
		if got := j.all()[2]; !equalStrings(got, []string{"a"}) {
			t.Fatalf("the readmitted partition is %v, want [a]", got)
		}
		if left := j.leftOf("a"); len(left) != 2 || left[0] != left[1] {
			t.Fatalf("a's deliveries left = %v, want the same at both: an aside spends nothing", left)
		}
	})

	t.Run("a_readmit_hands_back_its_key_at_the_head_in_one_partition_in_order", func(t *testing.T) {
		t.Parallel()
		// What a seat's run leaving leans on: the conversation's waiting
		// wakes run as one turn, ahead of the mail that came after them, and
		// another key's stay aside.
		//
		// Mutation: readmit at the tail, or readmit every key, and x runs
		// first or b runs too.
		q := s.start(ctx, t)
		j := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", j.handler(func(_ context.Context, evs []*events.Event, before int) string {
			switch {
			case before > 0:
				return ""
			case labelOf(evs[0]) == "b":
				return "other"
			case labelOf(evs[0]) == "x":
				return ""
			}
			return "k"
		}), nil)
		publish(ctx, t, q, "aside.t", newConvEvent("a1", "c"))
		j.awaitSizes(t, "a1, set aside", 1)
		publish(ctx, t, q, "aside.t", newConvEvent("a2", "c"))
		j.awaitSizes(t, "a2, set aside", 1, 1)
		publish(ctx, t, q, "aside.t", newConvEvent("b", "cb"))
		j.awaitSizes(t, "b, set aside under its own key", 1, 1, 1)
		s.holdTakenIn(ctx, t, q, "aside.t", "g", holdReason)
		publish(ctx, t, q, "aside.t", newConvEvent("x", "cx"))
		n, err := q.Readmit(ctx, "aside.t", "g", "k")
		if err != nil || n != 2 {
			t.Fatalf("Readmit = %d, %v, want 2 and no error", n, err)
		}
		if err := q.ResumeTopic(ctx, "aside.t", "g", attachmentOf(ctx, t, q, "aside.t", "g"), holdReason); err != nil {
			t.Fatalf("ResumeTopic: %v", err)
		}
		j.awaitSizes(t, "the readmitted partition, then x", 1, 1, 1, 2, 1)
		got := j.all()[3:]
		if !equalStrings(got[0], []string{"a1", "a2"}) || !equalStrings(got[1], []string{"x"}) {
			t.Fatalf("after the readmit the handler saw %v, want [[a1 a2] [x]]", got)
		}
		j.staysAt(ctx, t, 5, "a delivery aside under another key was readmitted")
	})

	t.Run("a_readmit_with_nothing_aside_hands_back_nothing", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		if n, err := q.Readmit(ctx, "aside.none", "g", "k"); err != nil || n != 0 {
			t.Fatalf("Readmit of a pair nothing attaches = %d, %v, want 0 and no error", n, err)
		}
		j := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", j.handler(asideFirst("k")), nil)
		if n, err := q.Readmit(ctx, "aside.t", "g", "k"); err != nil || n != 0 {
			t.Fatalf("Readmit with nothing aside = %d, %v, want 0 and no error", n, err)
		}
	})

	t.Run("a_readmit_while_the_handler_runs_readmits_what_it_sets_aside", func(t *testing.T) {
		t.Parallel()
		// The race a seat's run leaving meets: the run leaves, and its
		// conversation is readmitted, between the screen finding it busy and
		// the backend setting the delivery aside.
		//
		// Mutation: decide the readmit only on what is aside when it lands,
		// and a waits aside for ever.
		q := s.start(ctx, t)
		j := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", j.handler(func(ctx context.Context, _ []*events.Event, before int) string {
			if before > 0 {
				return ""
			}
			if _, err := q.Readmit(ctx, "aside.t", "g", "k"); err != nil {
				t.Errorf("Readmit inside the handler: %v", err)
			}
			return "k"
		}), nil)
		publish(ctx, t, q, "aside.t", newConvEvent("a", "ca"))
		j.awaitSizes(t, "a, set aside under a key readmitted while it ran, and handed back", 1, 1)
		if left := j.leftOf("a"); len(left) != 2 || left[0] != left[1] {
			t.Fatalf("a's deliveries left = %v, want the same at both", left)
		}
	})

	t.Run("a_release_hands_aside_mail_back_at_one_delivery", func(t *testing.T) {
		t.Parallel()
		// Mutation: forget aside mail at the detach, and a waits out its ack
		// window; hand it back without charging it, and it arrives unspent.
		q := s.start(ctx, t)
		first := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", first.handler(asideFirst("k")), nil)
		publish(ctx, t, q, "aside.t", newConvEvent("a", "ca"))
		first.awaitSizes(t, "a, set aside", 1)
		if _, err := q.Detach(ctx, "aside.t", "g"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		next := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", next.handler(func(context.Context, []*events.Event, int) string {
			return ""
		}), nil)
		next.awaitSizes(t, "a, handed back by the release", 1)
		was, now := first.leftOf("a"), next.leftOf("a")
		if len(was) != 1 || len(now) != 1 || now[0] != was[0]-1 {
			t.Fatalf("a had %v left aside and %v after the release, want one fewer", was, now)
		}
	})

	t.Run("a_stopped_client_hands_aside_mail_to_its_peer_at_one_delivery", func(t *testing.T) {
		t.Parallel()
		if s.caps.Peer == nil {
			t.Skip("backend declares no Peer")
		}
		// THE PEER HOLDS IT AND STOPS, never the queue the suite started: on
		// the backend that ships, that queue owns its broker and stops it.
		q := s.start(ctx, t)
		peer := s.startQueue(ctx, t, s.caps.Peer(t, q))
		first := newAsideJournal()
		subscribeBatch(ctx, t, peer, "aside.t", "g", first.handler(asideFirst("k")), nil)
		publish(ctx, t, q, "aside.t", newConvEvent("a", "ca"))
		first.awaitSizes(t, "a, set aside", 1)
		if err := peer.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		next := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", next.handler(func(context.Context, []*events.Event, int) string {
			return ""
		}), nil)
		next.awaitSizes(t, "a, handed back by the stop", 1)
		was, now := first.leftOf("a"), next.leftOf("a")
		if len(was) != 1 || len(now) != 1 || now[0] != was[0]-1 {
			t.Fatalf("a had %v left aside and %v after the stop, want one fewer", was, now)
		}
	})

	t.Run("an_aside_past_the_bound_is_handed_back", func(t *testing.T) {
		t.Parallel()
		// The bound that keeps a handler that never readmits from pinning
		// every unacknowledged slot of its mailbox. The room each handler is
		// told falls by one per aside, and the one set aside past it comes back.
		//
		// Mutation: keep every aside, and the last never comes back.
		q := s.start(ctx, t)
		j := newAsideJournal()
		var (
			mu   sync.Mutex
			past string
		)
		subscribeBatch(ctx, t, q, "aside.t", "g", j.handler(func(ctx context.Context, evs []*events.Event, before int) string {
			mu.Lock()
			defer mu.Unlock()
			if room, _ := queue.AsideRoom(ctx); room == 0 && past == "" {
				past = labelOf(evs[0])
			}
			if labelOf(evs[0]) == past && before > 0 {
				return ""
			}
			return "k"
		}), nil)
		for i := range queue.MaxAside + 1 {
			label := "e" + strconv.Itoa(i)
			publish(ctx, t, q, "aside.t", newConvEvent(label, label))
		}
		j.await(t, "every event once, and the one past the bound again", func(batches [][]string) bool {
			return len(batches) == queue.MaxAside+2
		})
		j.mu.Lock()
		rooms := append([]int(nil), j.rooms...)
		j.mu.Unlock()
		for i := range queue.MaxAside + 1 {
			if rooms[i] != queue.MaxAside-i {
				t.Fatalf("the rooms handlers were told = %v, want %d down to 0", rooms, queue.MaxAside)
			}
		}
		mu.Lock()
		label := past
		mu.Unlock()
		if left := j.leftOf(label); len(left) != 2 || left[1] != left[0]-1 {
			t.Fatalf("%s, set aside past the bound, had %v left, want one fewer the second time", label, left)
		}
		j.staysAt(ctx, t, queue.MaxAside+2, "an aside within the bound came back")
	})

	t.Run("waiting_counts_aside_mail_and_its_holders_backlog_skips_it", func(t *testing.T) {
		t.Parallel()
		// What a seat's router leans on: it reads the inbox through the client
		// that holds it, and mail already decided aside is not read again.
		//
		// Mutation: read aside mail into the holder's backlog, or leave it
		// out of Waiting.
		q := s.start(ctx, t)
		j := newAsideJournal()
		subscribeBatch(ctx, t, q, "aside.t", "g", j.handler(asideFirst("k")), nil)
		publish(ctx, t, q, "aside.t", newConvEvent("a", "ca"))
		j.awaitSizes(t, "a, set aside", 1)
		s.holdTakenIn(ctx, t, q, "aside.t", "g", holdReason)
		publish(ctx, t, q, "aside.t", newConvEvent("b", "cb"))
		awaitState(t, "Waiting to count the aside a and the held b", func() bool {
			w, err := q.Waiting(ctx, "aside.t", "g")
			return err == nil && w.Total() == 2
		})
		held, err := q.Backlog(ctx, "aside.t", "g", backlogRead)
		if err != nil {
			t.Fatalf("Backlog: %v", err)
		}
		if got := labelsOf(held); !equalStrings(got, []string{"b"}) {
			t.Fatalf("the holder's backlog = %v, want [b]: mail it holds aside is decided", got)
		}
	})
}
