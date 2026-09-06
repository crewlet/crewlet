package jetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// delivery pairs a decoded event with the message it must be acked through.
type delivery struct {
	msg jetstream.Msg
	ev  *events.Event
}

func eventOf(d delivery) *events.Event { return d.ev }

// SubscribeBatch attaches with batched, key-partitioned delivery.
//
// The cycle: drain what is locally available (plus a linger window),
// partition by conversation key, dispatch one handler call per partition
// oldest-conversation-first, and ack per partition. A failing partition
// never blocks or replays a different one from the same drain.
//
// This is what makes ten comments on one issue cost ONE agent turn instead
// of ten. It is also why an agent that was busy does not wake to a thundering
// herd: everything that queued up while it worked coalesces into one batch.
func (q *Queue) SubscribeBatch(
	ctx context.Context,
	topic, group string,
	h queue.BatchHandler,
	key queue.BatchKeyFunc,
	opts *queue.BatchOptions,
) error {
	if opts == nil {
		opts = queue.DefaultBatchOptions()
	}
	return q.attach(ctx, topic, group, func(ctx context.Context, a *attachment) {
		for {
			if ctx.Err() != nil {
				return
			}
			if a.blocked() {
				if sleep(ctx, a.q.fetchWait()) != nil {
					return
				}
				continue
			}
			batch := a.drain(ctx, opts)
			if len(batch) == 0 {
				continue
			}
			// A stop or a hold that landed DURING the linger window must
			// not be flushed past: the whole point of pausing a seat's
			// inbox is that no turn starts, and a batch collected a
			// moment earlier would start one. Hand it straight back.
			//
			// The two are handled DIFFERENTLY and conflating them was a
			// seat-killing bug. A cancelled context is teardown, so the
			// loop ends. A hold is RELEASED IN PLACE — ResumeTopic clears
			// it and the attachment is expected to carry on — so ending
			// the loop there left the seat attached, owning its lease,
			// and reading nothing for the rest of the process's life.
			// Nothing detects that: the seat looks owned and healthy and
			// its mail simply accumulates. It is the same condition the
			// top of this loop already answers with `continue`, ten lines
			// up, which is what it should have said here too.
			if ctx.Err() != nil {
				a.nakAll(ctx, batch)
				return
			}
			if a.blocked() {
				a.nakAll(ctx, batch)
				continue
			}
			a.dispatchBatch(ctx, batch, h, key)
		}
	})
}

// drain collects one cycle's worth of deliveries.
//
// Options are read HERE, at the start of every cycle, which is what makes a
// live config reload take effect on the next batch with no re-subscription.
func (a *attachment) drain(ctx context.Context, opts *queue.BatchOptions) []delivery {
	maxBatch := opts.EffectiveMaxBatch()
	linger := opts.EffectiveLinger()

	first, err := a.cons.Fetch(1, jetstream.FetchMaxWait(a.q.fetchWait()))
	if err != nil {
		a.logFetchErr(ctx, err)
		return nil
	}
	var batch []delivery
	for msg := range first.Messages() {
		if d, ok := a.toDelivery(msg); ok {
			batch = append(batch, d)
		}
	}
	if len(batch) == 0 {
		return nil
	}

	// The linger window is measured from the FIRST message and is fixed,
	// not sliding: a steady trickle must not be able to delay dispatch
	// unboundedly. With linger 0 (the default) this still drains
	// everything already available, so a backlog that accumulated while a
	// previous handler ran coalesces at zero added latency.
	deadline := time.Now().Add(linger)
	for len(batch) < maxBatch {
		if ctx.Err() != nil || a.blocked() {
			break
		}
		wait := a.q.drainFetchWait()
		if remaining := time.Until(deadline); remaining > wait {
			wait = min(remaining, a.q.fetchWait())
		}
		more, err := a.cons.Fetch(maxBatch-len(batch), jetstream.FetchMaxWait(wait))
		if err != nil {
			break
		}
		for msg := range more.Messages() {
			if d, ok := a.toDelivery(msg); ok {
				batch = append(batch, d)
			}
		}
		// THE WINDOW IS THE BOUND, and only the window.
		//
		// This used to end the drain on a fetch that produced nothing AND a
		// passed deadline, which made the window SLIDE: any arrival kept
		// collection going, so the real bound was maxBatch × drainWait
		// rather than the linger — a second of collection at the default
		// cap, fifty at a large one, spent out of the same ack budget that
		// has to hold a whole turn as well. The comment above the deadline
		// already said fixed; this is what makes it true.
		//
		// An empty fetch is deliberately NOT an ending on its own: a
		// positive window exists precisely to wait for arrivals that have
		// not happened yet. With linger 0 the deadline has already passed
		// on the first check, so exactly one tail fetch runs — which asks
		// for the whole remaining cap and is what drains a backlog that
		// accumulated while the previous handler ran.
		if !time.Now().Before(deadline) {
			break
		}
	}
	return batch
}

func (a *attachment) toDelivery(msg jetstream.Msg) (delivery, bool) {
	ev, ok := a.decode(msg)
	if !ok {
		return delivery{}, false
	}
	return delivery{msg: msg, ev: ev}, true
}

// dispatchBatch partitions a drain and runs one handler per conversation.
func (a *attachment) dispatchBatch(ctx context.Context, batch []delivery, h queue.BatchHandler, key queue.BatchKeyFunc) {
	parts := queue.OrderForDispatch(queue.PartitionByKey(batch, key, eventOf), eventOf)

	for _, part := range parts {
		// A deferral, a detach or a pause taken mid-drain stops the rest
		// of it: the remaining partitions are work this consumer has
		// equally lost the right to run. They go back unhandled so the
		// successor gets them, rather than being run by a consumer that
		// has just admitted it should not.
		if a.blocked() {
			a.nakAll(ctx, part.Items)
			continue
		}

		evs := make([]*events.Event, len(part.Items))
		for i, d := range part.Items {
			evs[i] = d.ev
		}

		a.q.beginHandler()
		res := runBatchHandler(ctx, a.log, evs, h)
		a.q.endHandler()

		a.applyPartition(ctx, part.Key, part.Items, evs, res)
	}
}

// applyPartition applies one outcome to every message in a partition.
//
// Per-partition rather than per-message: the handler saw the conversation as
// a unit and its verdict covers all of it. Acking some and NAKing others
// would deliver a partial conversation to the successor.
func (a *attachment) applyPartition(ctx context.Context, batchKey string, items []delivery, evs []*events.Event, res queue.Result) {
	queue.LogBatchResult(a.log, a.key.topic, a.key.group, batchKey, evs, res)

	switch res.Outcome {
	case queue.OutcomeAck:
		for _, d := range items {
			if err := d.msg.Ack(); err != nil {
				a.log.Warn("ack_failed", "error", err.Error())
			}
		}
	case queue.OutcomeDefer:
		a.nakAll(ctx, items)
		a.setQuiesced(true)
	case queue.OutcomeNak:
		for _, d := range items {
			a.nakOrDeadLetter(ctx, d.msg)
		}
	}
}

// nakAll hands a whole batch back — a deferral, a hold or a pause that landed
// mid-drain, or teardown. Nothing is wrong with any of these messages.
//
// THROUGH THE BUDGET CHECK, because a Nak spends a delivery whatever it means.
// Handing a batch back is not a failure, but JetStream counts it as one
// redelivery all the same, and this used to call msg.Nak() bare: a message
// whose 25 deliveries were spent by lease movements and sandbox parks was then
// discarded by the broker's own MaxDeliver backstop — no dead-letter copy, no
// `dead_lettered` line, no advisory consumer anywhere in this package to
// notice. A healthy inbox event, silently gone. The boundary is the same one
// the failure path already honours; what differs is only how it got there.
func (a *attachment) nakAll(ctx context.Context, items []delivery) {
	for _, d := range items {
		a.handBack(ctx, d.msg)
	}
}
