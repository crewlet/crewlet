package memory_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// THE BROKER READS AS IDLE ONLY ONCE NOTHING ON IT CAN STILL HAND A HANDLER A
// DELIVERY — the conformance suite's Settle on this backend, which every case
// asserting that nothing more was delivered reads behind.
//
// Each case runs in a synctest bubble: the twin's one timer is a linger
// window's, and in a bubble it fires only once the case is blocked, so "the
// window is still open" is a fact of the case rather than a bet that a runner
// reaches the check inside the window.
//
// Mutation: leave the drain uncounted, and a running handler reads as idle;
// leave the window uncounted, and Idle returns before the flush delivers; leave
// a window closed undelivered counted, and the broker never reads as idle again.
func TestTheBrokerIsIdleOnlyOnceNothingCanStillDeliver(t *testing.T) {
	t.Parallel()

	t.Run("an_open_linger_window_until_the_drain_its_flush_starts_returns", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			q := startedTwin(t)
			var mu sync.Mutex
			var batches int
			subscribeLingering(t, q, func(context.Context, []*events.Event) queue.Result {
				mu.Lock()
				batches++
				mu.Unlock()
				return queue.Ack()
			})
			publishProbe(t, q)

			select {
			case <-q.Broker().Idle():
				t.Fatal("the broker read as idle with a linger window still open")
			default:
			}
			<-q.Broker().Idle()
			mu.Lock()
			defer mu.Unlock()
			if batches != 1 {
				t.Fatalf("the broker went idle with %d batches delivered, want the window's 1", batches)
			}
		})
	})

	t.Run("a_drain_whose_handler_is_running", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			q := startedTwin(t)
			entered, release := make(chan struct{}), make(chan struct{})
			if err := q.Subscribe(t.Context(), "idle.t", "g",
				func(context.Context, *events.Event) queue.Result {
					close(entered)
					<-release
					return queue.Ack()
				}); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			published := make(chan struct{})
			go func() {
				defer close(published)
				// The publish drains inline, so it is inside the handler
				// until the case releases it.
				publishProbe(t, q)
			}()
			<-entered

			select {
			case <-q.Broker().Idle():
				t.Fatal("the broker read as idle with a handler running")
			default:
			}
			close(release)
			<-q.Broker().Idle()
			<-published
		})
	})

	t.Run("a_window_closed_undelivered", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			q := startedTwin(t)
			subscribeLingering(t, q, func(context.Context, []*events.Event) queue.Result {
				t.Error("a detached consumer's window delivered")
				return queue.Ack()
			})
			publishProbe(t, q)
			if _, err := q.Detach(t.Context(), "idle.t", "g"); err != nil {
				t.Fatalf("Detach: %v", err)
			}

			select {
			case <-q.Broker().Idle():
			default:
				t.Fatal("a window closed undelivered kept the broker from reading as idle")
			}
		})
	})
}

// startedTwin is a started twin of its own broker, stopped when the case ends.
func startedTwin(t *testing.T) *memory.Queue {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return q
}

// subscribeLingering attaches h to the case's pair with a linger window of
// half a minute, inside queue.MaxLingerSeconds: a bubble's clock reaches it the
// moment the case blocks, and nothing outside one is ever asked to wait it out.
func subscribeLingering(t *testing.T, q *memory.Queue, h queue.BatchHandler) {
	t.Helper()
	if err := q.SubscribeBatch(t.Context(), "idle.t", "g", h,
		func(*events.Event) string { return "" },
		queue.NewBatchOptions((30*time.Second).Seconds(), 10)); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
}

// publishProbe publishes one event to the case's pair.
func publishProbe(t *testing.T, q *memory.Queue) {
	t.Helper()
	if err := q.Publish(t.Context(), "idle.t", &events.Event{Type: "idle.probe"}); err != nil {
		t.Errorf("Publish: %v", err)
	}
}
