package memory_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A HANDLER NEVER RUNS BESIDE ITSELF ON THE TWIN: a listener is handed one
// broadcast at a time, and a handler publishing into its own subscription is
// handed that event only after it returns, neither beside it nor inside its
// own publish. The conformance suite states both and decides the halves every
// backend can show; the rest is an absence a quiet window cannot decide, so
// JetStream's is decided by where its code sits and the twin's here, in a
// bubble: the handler is held inside the first delivery and what it was handed
// is read once every goroutine is durably blocked, and again after the bubble's
// clock passes every timer the twin arms (pastEveryTimer), catching a delivery
// that waited on that clock. Mutation: deliver on a goroutine of its own, or
// start a nested drain from a handler's publish, and the held handler is
// handed a second event.
func TestAHandlerNeverRunsBesideItself(t *testing.T) {
	t.Parallel()

	t.Run("one_listener_hears_one_message_at_a_time", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			q := startedTwin(t)
			held := holdFirst(t)
			var did steps
			stop, err := q.Listen(t.Context(), "crewlet.room", func(_ string, payload []byte) {
				did.note("enter " + string(payload))
				if string(payload) == "first" {
					held.inside()
				}
				did.note("leave " + string(payload))
			})
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			// FROM THE CASE'S OWN GOROUTINE: a listener is handed its
			// broadcasts on a goroutine of its own, never the publisher's,
			// so both return with the first one held.
			for _, payload := range []string{"first", "second"} {
				if err := q.Broadcast(t.Context(), "crewlet.room", []byte(payload)); err != nil {
					t.Fatalf("Broadcast %s: %v", payload, err)
				}
			}
			held.entered()
			stillHeld := func(when string) {
				t.Helper()
				if got := did.all(); !slices.Equal(got, []string{"enter first"}) {
					t.Fatalf("with the listener's handler held inside the first of two "+
						"broadcasts, %s it had done %v, want [enter first] alone: a second "+
						"broadcast handed over beside the first races a handler that takes its "+
						"own lock", when, got)
				}
			}
			synctest.Wait()
			stillHeld("once every goroutine had blocked")
			// THEN THE TWIN'S CLOCK, past every timer it arms: a delivery that
			// waited on it first is still asleep at the read above.
			time.Sleep(pastEveryTimer)
			synctest.Wait()
			stillHeld("once its clock had then moved past every timer the twin arms")

			held.release()
			// THE TWIN'S OWN BARRIER, which passes once every listener has
			// handled what it was handed: the second broadcast, now.
			if err := q.ListenBarrier(t.Context()); err != nil {
				t.Fatalf("ListenBarrier: %v", err)
			}
			want := []string{"enter first", "leave first", "enter second", "leave second"}
			if got := did.all(); !slices.Equal(got, want) {
				t.Fatalf("once the handler held inside the first broadcast was let go, the "+
					"listener had done %v, want %v: the second is handed over once the first "+
					"has returned", got, want)
			}
			if err := stop(t.Context()); err != nil {
				t.Fatalf("stop listening: %v", err)
			}
		})
	})

	t.Run("a_handler_that_publishes_to_its_own_subscription_is_not_re_entered", func(t *testing.T) {
		t.Parallel()
		// BOTH SUBSCRIBING VERBS, because a drain pass hands a single
		// event and a batch to the handler at two places of its own.
		for _, verb := range []struct {
			name string
			// attach makes h the handler of the case's pair, called once
			// per event it is handed.
			attach func(t *testing.T, q *memory.Queue, h func(context.Context, *events.Event))
		}{
			{"subscribe", func(t *testing.T, q *memory.Queue, h func(context.Context, *events.Event)) {
				t.Helper()
				if err := q.Subscribe(t.Context(), reentryTopic, "g",
					func(ctx context.Context, ev *events.Event) queue.Result {
						h(ctx, ev)
						return queue.Ack()
					}); err != nil {
					t.Fatalf("Subscribe: %v", err)
				}
			}},
			{"subscribe_batch", func(t *testing.T, q *memory.Queue, h func(context.Context, *events.Event)) {
				t.Helper()
				if err := q.SubscribeBatch(t.Context(), reentryTopic, "g",
					func(ctx context.Context, evs []*events.Event) queue.Result {
						for _, ev := range evs {
							h(ctx, ev)
						}
						return queue.Ack()
					},
					func(*events.Event) string { return "" }, queue.DefaultBatchOptions()); err != nil {
					t.Fatalf("SubscribeBatch: %v", err)
				}
			}},
		} {
			t.Run(verb.name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					q := startedTwin(t)
					held := holdFirst(t)
					var did steps
					var nested error
					verb.attach(t, q, func(ctx context.Context, ev *events.Event) {
						did.note("enter " + ev.Type)
						if ev.Type == "first" {
							// FROM INSIDE THE HANDLER, on its own context,
							// which is what a tool call in a turn does.
							nested = q.Publish(ctx, reentryTopic, &events.Event{Type: "second"})
							did.note("published second")
							held.inside()
						}
						did.note("leave " + ev.Type)
					})
					published := make(chan error, 1)
					// FROM A GOROUTINE OF ITS OWN: the twin dispatches on the
					// publisher, so this publish is inside the handler until
					// the case lets it go.
					go func() {
						published <- q.Publish(t.Context(), reentryTopic, &events.Event{Type: "first"})
					}()
					held.entered()
					if nested != nil {
						t.Fatalf("a publish from inside the handler failed: %v", nested)
					}
					stillHeld := func(when string) {
						t.Helper()
						if got := did.all(); !slices.Equal(got, []string{"enter first", "published second"}) {
							t.Fatalf("with the handler held after it published into its own "+
								"subscription, %s it had done %v, want [enter first published "+
								"second] alone: a seat woken by its own publish while its turn "+
								"still runs is a seat running two turns", when, got)
						}
						// AND THE EVENT WAITS IN THE MAILBOX, behind the pass
						// running the handler: the twin's own record of it,
						// which a delivery made beside the handler takes out.
						if got := q.Backlog(reentryTopic, "g"); len(got) != 1 || got[0].Type != "second" {
							t.Fatalf("with the handler held, %s its pair's mailbox held %v, want "+
								"the one event it published, waiting behind it", when, typesOf(got))
						}
					}
					synctest.Wait()
					stillHeld("once every goroutine had blocked")
					// THEN THE TWIN'S CLOCK, as for the listener: a pass that
					// waited on it before taking the event leaves the event in
					// the mailbox at the read above, and has run by the next.
					time.Sleep(pastEveryTimer)
					synctest.Wait()
					stillHeld("once its clock had then moved past every timer the twin arms")

					held.release()
					if err := <-published; err != nil {
						t.Fatalf("Publish: %v", err)
					}
					want := []string{"enter first", "published second", "leave first",
						"enter second", "leave second"}
					if got := did.all(); !slices.Equal(got, want) {
						t.Fatalf("once the handler was let go it had done %v, want %v: the event it "+
							"published is handed over once it has returned", got, want)
					}
				})
			})
		}
	})
}

// reentryTopic is the topic of the re-entry cases' pair.
const reentryTopic = "t.reentry"

// pastEveryTimer is how far a case moves the bubble's clock with its handler
// held: past the longest timer the twin arms, a linger window at its ceiling
// (queue.MaxLingerSeconds). These cases open no window, and the twin's
// redelivery is virtual, so the advance fires no timer of the twin's own. What
// it lets run is a delivery that waited on the clock first, which a read behind
// synctest.Wait alone would miss. It costs nothing: the bubble's clock jumps
// the moment every goroutine is blocked.
const pastEveryTimer = time.Duration(queue.MaxLingerSeconds*float64(time.Second)) + time.Second

// hold is a handler held inside its first delivery until the case lets it go.
type hold struct {
	in, out     chan struct{}
	enter, free sync.Once
}

// holdFirst makes the case's hold, let go on every exit of the case, the
// failing ones included. Registered after the queue's Stop, so it runs first:
// a stop waits for a listener's handler still running, and a bubble ends only
// once every goroutine in it has.
func holdFirst(t *testing.T) *hold {
	t.Helper()
	h := &hold{in: make(chan struct{}), out: make(chan struct{})}
	t.Cleanup(h.release)
	return h
}

// inside is the held handler's own: it says it is in, then waits to be let go.
func (h *hold) inside() {
	h.enter.Do(func() { close(h.in) })
	<-h.out
}

// entered waits until the handler is inside.
func (h *hold) entered() { <-h.in }

// release lets the handler go. Idempotent.
func (h *hold) release() { h.free.Do(func() { close(h.out) }) }

// steps is what a case's handlers did, in the order they did it.
type steps struct {
	mu  sync.Mutex
	did []string
}

func (s *steps) note(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.did = append(s.did, step)
}

func (s *steps) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.did)
}

// typesOf names events for a failure message.
func typesOf(evs []*events.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}
