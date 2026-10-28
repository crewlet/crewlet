package jetstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A HANDLER THAT SIGNALS PROGRESS KEEPS ITS DELIVERY PAST THE ACK WINDOW, and
// spends none of it. A mailbox delivery that meets a seat busy with another of
// its turns waits for the seat inside its handler, and that turn can outlast
// the window: a window that ran out redelivered the message and spent one of
// its deliveries on nothing but the wait. The handler here holds its delivery
// for twice the window, signalling as often as the backend asks, while a
// second member of the group pulls beside it, as the seat's next holder would:
// the message is handed over exactly once.
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
	if err := q.Publish(ctx, topic, ev(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	var handed, signals atomic.Int32
	done := make(chan struct{})
	if err := q.SubscribeBatch(ctx, topic, group,
		func(ctx context.Context, _ []*events.Event) queue.Result {
			if handed.Add(1) > 1 {
				return queue.Ack()
			}
			every, signal, ok := queue.Progress(ctx)
			if !ok || every >= window {
				t.Errorf("the backend stated progress %v every %v, want a signal inside the %v window",
					ok, every, window)
				return queue.Ack()
			}
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			wait := time.After(2 * window)
			for {
				select {
				case <-ticker.C:
					if err := signal(); err != nil {
						t.Errorf("signal: %v", err)
					}
					signals.Add(1)
				case <-wait:
					close(done)
					return queue.Ack()
				}
			}
		},
		func(*events.Event) string { return "one" },
		queue.NewBatchOptions(0, 1),
	); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	// THE OTHER MEMBER, which a message whose window ran out is redelivered to.
	other := clientUnderTest(t, srv, srv.Server)
	if err := other.SubscribeBatch(ctx, topic, group,
		func(context.Context, []*events.Event) queue.Result {
			handed.Add(1)
			return queue.Ack()
		},
		func(*events.Event) string { return "one" },
		queue.NewBatchOptions(0, 1),
	); err != nil {
		t.Fatalf("SubscribeBatch (other): %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the handler never finished its wait")
	}
	time.Sleep(window)
	if n := handed.Load(); n != 1 || signals.Load() == 0 {
		t.Fatalf("the message was handed over %d times with %d signals, want once", n, signals.Load())
	}
}
