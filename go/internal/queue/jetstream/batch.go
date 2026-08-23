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
				if sleep(ctx, fetchWait) != nil {
					return
				}
				continue
			}
			batch := a.drain(ctx, opts)
			if len(batch) == 0 {
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

	first, err := a.cons.Fetch(1, jetstream.FetchMaxWait(fetchWait))
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
		wait := drainWait
		if remaining := time.Until(deadline); remaining > wait {
			wait = min(remaining, fetchWait)
		}
		more, err := a.cons.Fetch(maxBatch-len(batch), jetstream.FetchMaxWait(wait))
		if err != nil {
			break
		}
		before := len(batch)
		for msg := range more.Messages() {
			if d, ok := a.toDelivery(msg); ok {
				batch = append(batch, d)
			}
		}
		if len(batch) == before && !time.Now().Before(deadline) {
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
		// A deferral stops the whole drain: the remaining partitions are
		// work this process has equally lost the right to do. They are
		// Nak'd unhandled so the successor gets them, rather than being
		// run by a node that just admitted it should not.
		if a.quiesced.Load() {
			a.nakAll(part.Items)
			continue
		}

		evs := make([]*events.Event, len(part.Items))
		for i, d := range part.Items {
			evs[i] = d.ev
		}

		a.q.beginHandler()
		res := runBatchHandler(ctx, evs, h)
		a.q.endHandler()

		a.applyPartition(ctx, part.Items, res)
	}
}

// applyPartition applies one outcome to every message in a partition.
//
// Per-partition rather than per-message: the handler saw the conversation as
// a unit and its verdict covers all of it. Acking some and NAKing others
// would deliver a partial conversation to the successor.
func (a *attachment) applyPartition(ctx context.Context, items []delivery, res queue.Result) {
	var head *events.Event
	if len(items) > 0 {
		head = items[0].ev
	}
	queue.LogResult(a.log, a.topic, a.group, head, res)

	switch res.Outcome {
	case queue.OutcomeAck:
		for _, d := range items {
			if err := d.msg.Ack(); err != nil {
				a.log.Warn("ack_failed", "error", err.Error())
			}
		}
	case queue.OutcomeDefer:
		a.nakAll(items)
		a.setQuiesced(true)
	case queue.OutcomeNak:
		for _, d := range items {
			a.nakOrDeadLetter(ctx, d.msg)
		}
	}
}

func (a *attachment) nakAll(items []delivery) {
	for _, d := range items {
		if err := d.msg.Nak(); err != nil {
			a.log.Warn("defer_nak_failed", "error", err.Error())
		}
	}
}
