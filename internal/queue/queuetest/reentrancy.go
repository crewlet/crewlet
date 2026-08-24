package queuetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runReentrancy certifies that a subscription dispatches ONE handler at a time.
//
// This is the property the engine's inbox guards are allowed to assume, and it
// is worth a contract group of its own because assuming it wrongly is silent.
// A seat's handler runs a whole turn. If a publish made from inside that turn —
// an A2A ask that wakes the asker, a task the seat assigns itself, a
// notification it emits about its own work — could be delivered back to the
// same seat before the turn returned, the second delivery would be handled by
// a seat that is already busy, and on a backend that dispatches inline it
// would run inside the first turn's own stack.
//
// The Python this replaces had exactly that problem and carried a guard for
// it: its inline dispatch re-entered the handler within one asyncio task, so
// awaiting the agent there waited on the task doing the awaiting. Every Go
// backend here forecloses it structurally instead — the pull loops fetch again
// only after a handler returns, and the in-process twin defers a nested drain
// to the loop already running rather than starting a second one — which is why
// no such guard exists in this codebase.
//
// So this group is not testing a feature. It is pinning the reason a guard was
// not written, in the one place a backend change would break it.
func (s *suite) runReentrancy(t *testing.T) {
	ctx := t.Context()

	t.Run("a_handler_that_publishes_to_its_own_subscription_is_not_re_entered", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		batches := newBatchJournal()

		var mu sync.Mutex
		var depth, maxDepth int
		var publishErr error

		subscribeBatch(ctx, t, q, "t.reentry", "g",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				mu.Lock()
				depth++
				if depth > maxDepth {
					maxDepth = depth
				}
				first := maxDepth == 1 && len(batches.all()) == 0
				mu.Unlock()

				if first {
					// Published from INSIDE the handler, with the
					// handler's own context, which is what a tool
					// call in a turn does.
					err := q.Publish(hctx, "t.reentry", newConvEvent("second", "c1"))
					mu.Lock()
					publishErr = err
					mu.Unlock()
					// Widen the window. A backend that dispatches
					// concurrently needs somewhere to do it, and
					// without this the assertion could pass on
					// timing rather than on structure.
					time.Sleep(reentryWindow)
				}

				batches.record(evs)
				mu.Lock()
				depth--
				mu.Unlock()
				return queue.Ack()
			},
			queue.DefaultBatchOptions())

		publish(ctx, t, q, "t.reentry", newConvEvent("first", "c1"))

		batches.await(t, "both events handled", func(got [][]string) bool {
			n := 0
			for _, b := range got {
				n += len(b)
			}
			return n >= 2
		})

		mu.Lock()
		gotDepth, gotErr := maxDepth, publishErr
		mu.Unlock()
		if gotErr != nil {
			t.Fatalf("publish from inside a handler failed: %v", gotErr)
		}
		if gotDepth != 1 {
			t.Errorf("max concurrent handlers on one subscription = %d, want 1: "+
				"a seat can be woken while its own turn is still running",
				gotDepth)
		}
	})

	t.Run("a_handlers_publish_returns_without_waiting_for_the_delivery_it_causes", func(t *testing.T) {
		t.Parallel()
		// The counterfactual to the case above, and the half that says
		// WHY not re-entering is safe rather than merely different. A
		// backend could satisfy "never two handlers at once" by having
		// the nested publish block until the earlier handler drained —
		// which on the in-process twin means blocking the turn on
		// itself, the deadlock this whole group exists to rule out.
		q := s.start(ctx, t)
		batches := newBatchJournal()

		var mu sync.Mutex
		var published, returned bool

		subscribeBatch(ctx, t, q, "t.reentry.nb", "g",
			func(hctx context.Context, evs []*events.Event) queue.Result {
				mu.Lock()
				already := published
				mu.Unlock()
				if !already {
					mu.Lock()
					published = true
					mu.Unlock()
					_ = q.Publish(hctx, "t.reentry.nb", newConvEvent("second", "c1"))
					mu.Lock()
					returned = true
					mu.Unlock()
				}
				batches.record(evs)
				return queue.Ack()
			},
			queue.DefaultBatchOptions())

		publish(ctx, t, q, "t.reentry.nb", newConvEvent("first", "c1"))

		batches.await(t, "both events handled", func(got [][]string) bool {
			n := 0
			for _, b := range got {
				n += len(b)
			}
			return n >= 2
		})
		mu.Lock()
		defer mu.Unlock()
		if !returned {
			t.Error("a publish made from inside a handler never returned: " +
				"a turn that wakes its own seat would block on itself")
		}
	})
}

// reentryWindow is how long the first handler stays on the stack after
// publishing, so a backend that would dispatch concurrently has room to.
//
// Small: it is paid once per backend and only has to outlast a dispatch
// handoff, not a fetch cycle — a backend that polls will simply deliver after
// the handler returns, which is the passing case either way.
const reentryWindow = 50 * time.Millisecond
