package pulsar

import (
	"context"
	"errors"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// delivery pairs a decoded event with the message it must be acked through.
type delivery struct {
	msg pulsar.Message
	ev  *events.Event
}

func eventOf(d delivery) *events.Event { return d.ev }

// THE BATCH DISPATCH BUDGET IS DELIBERATELY NOT PORTED.
//
// The Python engine capped how long one batch drain could keep dispatching
// partition handlers (_BATCH_DISPATCH_BUDGET_MS = 60 s) and requeued the rest
// by republishing them. The entire reason was the C++ client's ack timeout:
// every drained message's 30-minute clock started at receive, so a long tail
// of multi-minute turns would blow through it mid-drain and produce
// redelivered duplicate turns.
//
// github.com/apache/pulsar-client-go has NO ack timeout — no
// ConsumerOptions.AckTimeout, no client-side unacked tracker — and Pulsar has
// no broker-side one for a connected consumer. A fetched message stays this
// consumer's until it acks, naks, or the consumer closes. So the clock the
// budget was racing does not exist. The budget existed ONLY because the
// previous engine believed Pulsar's ack clock started at receive; measured
// against this client, it does not, and the budget is deleted.
//
// Deleting it also removes the requeue-by-republish path, which adr-101 §1
// forbids anyway ("Never substitute a republish: that sends the event to the
// topic tail while its prefetched siblings replay from the head, reordering
// the conversation").
//
// What the absence costs, stated so nobody has to rediscover it: a batch of N
// slow partitions holds N messages unacked for the sum of the turns. Pulsar's
// maxUnackedMessagesPerConsumer (50 000 by default) is the only ceiling, and
// a batch is capped at max_batch — twenty by default — so the margin is four
// orders of magnitude.

// SubscribeBatch attaches with batched, key-partitioned delivery.
//
// The cycle: drain what is locally available (plus a linger window),
// partition by conversation key, dispatch one handler call per partition
// oldest-conversation-first, and ack per partition. A failing partition never
// blocks or replays a different one from the same drain.
//
// This is what makes ten comments on one issue cost ONE agent turn instead of
// ten. It is also why an agent that was busy does not wake to a thundering
// herd: everything that queued up while it worked coalesces into one batch.
func (q *Queue) SubscribeBatch(
	ctx context.Context,
	topic, group string,
	h queue.BatchHandler,
	key queue.BatchKeyFunc,
	opts *queue.BatchOptions,
) error {
	if h == nil {
		return errNilHandler(topic, group)
	}
	if opts == nil {
		opts = queue.DefaultBatchOptions()
	}
	// The prefetch is sized from the batch cap in force at SUBSCRIBE time,
	// because a receiver queue is a broker-side flow-control grant and
	// cannot be resized live. A later Set() that raises max_batch still
	// takes effect on the drain — it just cannot enlarge what is already
	// locally available, which is the honest limit of a live reload here.
	return q.attach(ctx, topic, group, opts.EffectiveMaxBatch(), func(ctx context.Context, a *attachment) {
		for {
			if ctx.Err() != nil {
				return
			}
			cons, ok := a.resume(ctx)
			if !ok {
				continue
			}
			batch := a.drain(ctx, cons, opts)
			if len(batch) == 0 {
				continue
			}
			// A stop or a hold that landed DURING the linger window must
			// not be flushed past: the whole point of pausing a seat's
			// inbox is that no turn starts, and a batch collected a
			// moment earlier would start one. Hand the whole drain back —
			// one close covers every message in it — and keep looping,
			// because a hold is released in place and ending the loop
			// here would leave the seat permanently deaf.
			if a.blocked() || ctx.Err() != nil {
				a.handBack()
				if ctx.Err() != nil {
					return
				}
				continue
			}
			a.dispatchBatch(ctx, cons, batch, h, key)
		}
	})
}

// drain collects one cycle's worth of deliveries.
//
// Options are read HERE, at the start of every cycle, which is what makes a
// live config reload take effect on the next batch with no re-subscription.
func (a *attachment) drain(ctx context.Context, cons pulsar.Consumer, opts *queue.BatchOptions) []delivery {
	maxBatch := opts.EffectiveMaxBatch()
	linger := opts.EffectiveLinger()

	first, err := receive(ctx, cons, a.q.cfg.receiveWait())
	if err != nil {
		a.idleOrDone(ctx, err)
		return nil
	}
	batch := make([]delivery, 0, maxBatch)
	if d, ok := a.toDelivery(cons, first); ok {
		batch = append(batch, d)
	}
	if len(batch) == 0 {
		return nil
	}

	// The linger window is measured from the FIRST message and is fixed,
	// not sliding: a steady trickle must not be able to delay dispatch
	// unboundedly. With linger 0 (the default) this is a single drain pass
	// over whatever the consumer already prefetched, ending at the first
	// empty poll — so a backlog that accumulated while a previous handler
	// ran coalesces at zero added latency.
	deadline := time.Now().Add(linger)
	for len(batch) < maxBatch {
		if ctx.Err() != nil || a.blocked() {
			break
		}
		// At least drainWait, so the zero-linger pass gives the local
		// queue a moment to yield what it already holds; never more than
		// one poll interval at a stretch, so a hold or a stop taken
		// mid-window is noticed rather than waited out.
		wait := drainWait
		if remaining := time.Until(deadline); remaining > wait {
			wait = min(remaining, max(a.q.cfg.receiveWait(), drainWait))
		}
		msg, err := receive(ctx, cons, wait)
		if err != nil {
			if !isIdle(err) {
				break
			}
			if linger <= 0 || !time.Now().Before(deadline) {
				break
			}
			continue
		}
		if d, ok := a.toDelivery(cons, msg); ok {
			batch = append(batch, d)
		}
	}
	return batch
}

// isIdle reports whether a receive simply found nothing before its window
// closed. The client returns the context's own error, so this is the same
// question idleOrDone asks — kept as one predicate so a drain and a poll can
// never disagree about what "no message" looks like.
func isIdle(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

func (a *attachment) toDelivery(cons pulsar.Consumer, msg pulsar.Message) (delivery, bool) {
	ev, ok := a.decode(cons, msg)
	if !ok {
		return delivery{}, false
	}
	return delivery{msg: msg, ev: ev}, true
}

// dispatchBatch partitions a drain and runs one handler per conversation.
func (a *attachment) dispatchBatch(ctx context.Context, cons pulsar.Consumer, batch []delivery, h queue.BatchHandler, key queue.BatchKeyFunc) {
	parts := queue.OrderForDispatch(queue.PartitionByKey(batch, key, eventOf), eventOf)

	for _, part := range parts {
		// A deferral, a detach or a pause taken mid-drain stops the rest
		// of it: every remaining partition is work this consumer has
		// equally lost the right to run, and running one after admitting
		// that is how a seat's mail comes back in reverse partition
		// order. One close returns THIS partition and every one after it,
		// unacked and in order — so there is nothing left to iterate.
		if a.blocked() {
			a.handBack()
			return
		}

		evs := make([]*events.Event, len(part.Items))
		for i, d := range part.Items {
			evs[i] = d.ev
		}

		a.q.beginHandler()
		res := runBatchHandler(ctx, evs, h)
		a.q.endHandler()

		a.applyPartition(cons, part.Key, part.Items, evs, res)
	}
}

// applyPartition applies one outcome to every message in a partition.
//
// Per-partition rather than per-message: the handler saw the conversation as
// a unit and its verdict covers all of it. Acking some and NAKing others
// would deliver a partial conversation to the successor.
func (a *attachment) applyPartition(cons pulsar.Consumer, batchKey string, items []delivery, evs []*events.Event, res queue.Result) {
	queue.LogBatchResult(a.log, a.key.topic, a.key.group, batchKey, evs, res)
	action := actionFor(res.Outcome)
	for _, d := range items {
		a.act(cons, action, d.msg)
	}
	if res.Outcome == queue.OutcomeDefer {
		a.quiesced.Store(true)
	}
}
