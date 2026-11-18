package jetstream

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A HANDLER THAT SIGNALS PROGRESS KEEPS ITS DELIVERY PAST THE ACK WINDOW, and
// spends none of it: a mailbox delivery waiting in its handler for a busy seat
// can outlast the window, and while other members pull beside it the message
// is handed over exactly once.
//
// Whether a window ran out is the broker's to say, so the case asks it through
// CONTROLS: later messages held WITHOUT a signal, redelivered in stream order,
// so a control's redelivery follows the held message's, had it run out. Two
// controls, the second after the first's redelivery, hold it past two windows.
//
// Mutation: state no progress for a batch, or make the signal a no-op, and the
// message is redelivered while the first handler still holds it.
func TestAHandlerSignallingProgressKeepsItsDeliveryPastTheAckWindow(t *testing.T) {
	t.Parallel()
	const window = 2 * time.Second
	srv := startTestServer(t, testTimings(Config{AckWait: window}))
	q := clientUnderTest(t, srv, srv.Server)
	ctx := t.Context()
	topic, group := seatInbox("dana"), seatGroup("dana")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}

	// A control's first delivery is taken and held until it is let go; its
	// second is the broker's word that the first one's window ran out.
	type control struct {
		ev                        *events.Event
		taken, redelivered, letGo chan struct{}
	}
	kept := ev(1)
	controls := map[uuid.UUID]*control{}
	var order []*control
	for _, n := range []int{2, 3} {
		c := &control{ev: ev(n), taken: make(chan struct{}),
			redelivered: make(chan struct{}), letGo: make(chan struct{})}
		controls[c.ev.ID] = c
		order = append(order, c)
	}
	keptTaken, keepDone := make(chan struct{}), make(chan struct{})

	var mu sync.Mutex
	handed := map[uuid.UUID]int{}
	var signals atomic.Int32
	// ONE HANDLER FOR EVERY MEMBER, so which member takes what is the
	// broker's choice: whoever is free takes the next delivery, and a member
	// holding a delivery takes none.
	handler := func(ctx context.Context, evs []*events.Event) queue.Result {
		e := evs[0]
		mu.Lock()
		handed[e.ID]++
		n := handed[e.ID]
		mu.Unlock()
		c := controls[e.ID]
		switch {
		case e.ID == kept.ID && n == 1:
			close(keptTaken)
			every, signal, ok := queue.Progress(ctx)
			if !ok || every >= window {
				t.Errorf("the backend stated progress %v every %v, want a signal inside the %v window",
					ok, every, window)
				return queue.Ack()
			}
			ticker := time.NewTicker(every) //fixedwait:simulated a seat's wait for its turn, signalling as often as the backend asks
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := signal(); err != nil {
						t.Errorf("signal: %v", err)
					}
					signals.Add(1)
				case <-keepDone:
					return queue.Ack()
				case <-ctx.Done():
					return queue.Ack()
				}
			}
		case c != nil && n == 1:
			close(c.taken)
			select {
			case <-c.letGo:
			case <-ctx.Done():
			}
			return queue.Ack()
		case c != nil && n == 2:
			close(c.redelivered)
			return queue.Ack()
		}
		return queue.Ack()
	}
	key := func(*events.Event) string { return "one" }
	if err := q.SubscribeBatch(ctx, topic, group, handler, key, queue.NewBatchOptions(0, 1)); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	// THE OTHER MEMBERS, which a message whose window ran out is redelivered
	// to: one to hold a control, one to take what is redelivered.
	for range 2 {
		other := clientUnderTest(t, srv, srv.Server)
		if err := other.SubscribeBatch(ctx, topic, group, handler, key, queue.NewBatchOptions(0, 1)); err != nil {
			t.Fatalf("SubscribeBatch (other): %v", err)
		}
	}
	await := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatalf("%s within a minute", what)
		}
	}

	if err := q.Publish(ctx, topic, kept); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	await(keptTaken, "the held message was not taken")
	for i, c := range order {
		if err := q.Publish(ctx, topic, c.ev); err != nil {
			t.Fatalf("Publish control %d: %v", i+1, err)
		}
		await(c.taken, "a control was not taken")
		await(c.redelivered, "a control held without a signal was not redelivered")
		close(c.letGo)
	}
	mu.Lock()
	n := handed[kept.ID]
	mu.Unlock()
	close(keepDone)
	if n != 1 || signals.Load() == 0 {
		t.Fatalf("the message was handed over %d times with %d signals, want once", n, signals.Load())
	}
}
