package jetstream

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// delivery pairs a decoded event with the message it must be acked through.
//
// A FOLDED delivery has no message: it was read where it sits on the topic
// rather than fetched ([attachment.fold]), so the consumer holds no delivery
// of it to ack, Nak or extend, and it is settled by its stream sequence
// instead ([attachment.settleFolded]).
type delivery struct {
	msg jetstream.Msg
	ev  *events.Event
	seq uint64
}

// folded reports a delivery the drain folded rather than fetched.
func (d delivery) folded() bool { return d.msg == nil }

func eventOf(d delivery) *events.Event { return d.ev }

// SubscribeBatch attaches with batched, key-partitioned delivery.
//
// The cycle: drain what is locally available (plus a linger window),
// partition by the key [queue.BatchKeyFunc] derives, dispatch one handler
// call per partition oldest-partition-first, and ack per partition. A
// failing partition never blocks or replays a different one from the same
// drain.
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
			// A PRIORITY PARTITION IS DELIVERED WHOLE ([queue.Fold]).
			batch = a.fold(ctx, batch, key)
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
// Options are read HERE, once per cycle, which is what makes a live config
// reload take effect on the next batch with no re-subscription — and they are
// read once the cycle's FIRST MESSAGE IS IN HAND, not when the cycle starts.
//
// An idle cycle spends up to a whole poll window ([Queue.fetchWait], a second
// at the shipped value) parked in that first fetch. Read before it, the
// options belonged to whenever the loop last went round, so a reload landing
// while an idle seat waited was applied to the batch AFTER the next one — and
// the next one is exactly the burst an operator who just opened the window
// was expecting it to absorb. The window is measured from the first message,
// so the instant that opens it is the instant that says how long it is.
func (a *attachment) drain(ctx context.Context, opts *queue.BatchOptions) []delivery {
	var batch []delivery
	collect := func(msg jetstream.Msg) {
		if d, ok := a.toDelivery(msg); ok {
			batch = append(batch, d)
		}
	}
	// THE PRIORITY LANE FIRST, and all of it the cap allows, without
	// waiting: this is what puts a person's message in the drain while the
	// topic's own head is a backlog of work nobody is waiting for. What it
	// takes is dispatched ahead of the rest ([queue.OrderForDispatch]), and
	// the topic fills the drain behind it as before. The cap is read here,
	// ahead of the first message, because the lane is asked before anything
	// is known to be waiting; the window below is still read once a message
	// is in hand.
	if err := a.fetchLane(opts.EffectiveMaxBatch(), collect); err != nil {
		a.logLaneFetchErr(ctx, err)
	}
	if len(batch) == 0 {
		//nolint:contextcheck // a fetch runs on the attachment's fetch scope, never the loop's: see [attachment.fetching]
		if err := a.fetch(1, a.q.fetchWait(), collect); err != nil {
			a.logFetchErr(ctx, err)
			return nil
		}
		if len(batch) == 0 {
			return nil
		}
	}
	maxBatch := opts.EffectiveMaxBatch()
	linger := opts.EffectiveLinger()

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
		//nolint:contextcheck // the attachment's fetch scope, as above
		if err := a.fetch(maxBatch-len(batch), wait, collect); err != nil {
			break
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

// fold takes into a drain every event still pending on the topic that shares a
// partition with one of the drain's priority events ([queue.Fold]): the
// conversation a person is waiting on is delivered whole, never split between
// the turn the lane brought forward and a second one behind the backlog.
//
// READ, NOT FETCHED. The consumer serves its mail in stream order and the
// events a fold needs are anywhere behind the drain's head, so they are read
// where they sit ([Queue.pendingOf]) and settled with their partition's
// outcome: an Ack removes them from the stream ([attachment.settleFolded]),
// and a Nak or a Defer leaves them exactly where they were, never delivered
// and so never charged a delivery. Fetching to them instead would deliver,
// and then hand back, every event in front of them on every person's
// message, which spends their delivery budgets on nothing.
//
// The events this drain already holds are skipped by sequence, so nothing is
// in a delivery twice. Only a drain that took lane mail folds, so the read is
// paid on a person's message and on no other drain. A read that fails is
// reported and the drain goes ahead with what it took: the events it could
// not fold are still on the topic, and reach the seat in their turn.
func (a *attachment) fold(ctx context.Context, batch []delivery, key queue.BatchKeyFunc) []delivery {
	if a.lane == nil {
		return batch
	}
	evs := make([]*events.Event, len(batch))
	held := make(map[uint64]struct{}, len(batch))
	for i, d := range batch {
		evs[i] = d.ev
		if md, err := d.msg.Metadata(); err == nil {
			held[md.Sequence.Stream] = struct{}{}
		}
	}
	fold := queue.FoldOf(evs, key)
	if fold.Empty() {
		return batch
	}
	err := a.q.pendingOf(ctx, a.cons.CachedInfo().Stream, a.key.topic,
		consumerName(a.key.topic, a.key.group),
		func(seq uint64, ev *events.Event) {
			if _, ok := held[seq]; ok || !fold.Takes(ev) {
				return
			}
			batch = append(batch, delivery{ev: ev, seq: seq})
		})
	if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		a.log.Warn("lane_fold_failed", "error", err.Error(),
			"detail", "the topic could not be read for the rest of a priority partition; "+
				"the partition runs with what the drain took, and what it could not fold "+
				"reaches the seat in its turn")
	}
	return batch
}

// settleFolded retires a folded event once its partition was acked. It was
// read where it sat rather than fetched, so the consumer holds no delivery of
// it to ack, and deleting it from the stream is how it is settled: the
// consumer passes over a sequence the stream no longer holds.
//
// A delete the broker reports UNSUCCESSFUL is a message already gone, settled
// by an earlier fold, which is the end state this asks for: the agent stream
// refuses no delete ([streamSpec.denyDelete] is a log's), so a missing
// message is the only refusal it gives. Any other failure leaves the event on
// the topic, where it reaches the seat again in its turn, and says so.
func (a *attachment) settleFolded(ctx context.Context, seq uint64) {
	stream, err := a.q.js.Stream(ctx, a.cons.CachedInfo().Stream)
	if err == nil {
		err = stream.DeleteMsg(ctx, seq)
	}
	switch {
	case err == nil:
	case errors.Is(err, jetstream.ErrMsgNotFound), errors.Is(err, jetstream.ErrMsgDeleteUnsuccessful):
		a.log.Debug("folded_already_settled", "seq", seq)
	default:
		a.log.Warn("fold_settle_failed", "seq", seq, "error", a.q.brokerFailed(ctx, err).Error(),
			"detail", "an event folded into an acked partition could not be removed from the "+
				"topic, so it reaches the seat again when the topic does")
	}
}

func (a *attachment) toDelivery(msg jetstream.Msg) (delivery, bool) {
	ev, ok := a.decode(msg)
	if !ok {
		return delivery{}, false
	}
	return delivery{msg: msg, ev: ev}, true
}

// dispatchBatch partitions a drain and runs one handler per partition.
func (a *attachment) dispatchBatch(ctx context.Context, batch []delivery, h queue.BatchHandler, key queue.BatchKeyFunc) {
	parts := queue.OrderForDispatch(queue.PartitionByKey(batch, key, eventOf), eventOf)

	for _, part := range parts {
		// A deferral, a detach or a pause taken mid-drain stops the rest
		// of it: the remaining partitions are work this consumer has
		// equally lost the right to run. They go back unhandled so the
		// successor gets them, rather than being run by a consumer that
		// has just admitted it should not.
		//
		// UNHANDLED IS NOT UNCHARGED. These messages were FETCHED, which
		// is where this broker's counter moves, so their delivery is
		// already spent by the time the loop gets here — nakAll only
		// decides whether the last one dead-letters with a copy or is
		// discarded by MaxDeliver in silence. That is the contract's rule
		// and not this backend's shape: see [queue.DeliveriesLeft], and
		// internal/queue/memory, whose mailbox is a slice and therefore
		// has to charge a take deliberately.
		if a.blocked() {
			a.nakAll(ctx, part.Items)
			continue
		}

		evs := make([]*events.Event, len(part.Items))
		for i, d := range part.Items {
			evs[i] = d.ev
		}

		a.q.beginHandler()
		// EACH MESSAGE'S OWN HEADROOM, and the partition's folded from
		// them by the contract. Both, because they answer different
		// questions: one outcome covers the whole partition, so a
		// hand-back spends a delivery of every message in it and the
		// nearest its budget is the one that dead-letters first — while
		// a handler deciding what to do with ONE of these events has to
		// ask what THAT message has left. See [queue.DeliveriesLeft] and
		// [queue.DeliveriesLeftFor].
		res := runBatchHandler(a.withProgress(a.withHeadroom(ctx, part.Items...), part.Items...),
			a.log, evs, h)
		a.q.endHandler()

		a.applyPartition(ctx, part.Key, part.Items, evs, res)
	}
}

// applyPartition applies one outcome to every message in a partition.
//
// Per-partition rather than per-message: the handler saw the partition as a
// unit and its verdict covers all of it. Acking some and NAKing others would
// deliver a partial one to the successor.
func (a *attachment) applyPartition(ctx context.Context, batchKey string, items []delivery, evs []*events.Event, res queue.Result) {
	queue.LogBatchResult(a.log, a.key.topic, a.key.group, batchKey, evs, res)

	switch res.Outcome {
	case queue.OutcomeAck:
		for _, d := range items {
			if d.folded() {
				a.settleFolded(ctx, d.seq)
				continue
			}
			// FORGOTTEN FIRST, for the reason [attachment.apply]
			// gives on the single-message path: a partition that
			// succeeded after failing must not carry those failures
			// into a later redelivery of the same sequences, and the
			// map would otherwise hold every sequence this seat ever
			// failed for the life of the attachment. That is
			// ordinary operation here — an answer a parked coding
			// run is still owed is NAKed and acked on a later pass.
			if md, err := d.msg.Metadata(); err == nil {
				a.settled(md.Sequence.Stream)
			}
			if err := d.msg.Ack(); err != nil {
				a.log.Warn("ack_failed", "error", err.Error())
			}
		}
	case queue.OutcomeDefer:
		a.nakAll(ctx, items)
		a.setQuiesced(true)
	case queue.OutcomeNak:
		for _, d := range items {
			// A FOLDED EVENT STAYS WHERE IT WAS ([attachment.fold]).
			if d.folded() {
				continue
			}
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
//
// A FOLDED EVENT IS NOT HANDED BACK: it was never fetched, so it is still where
// it was on the topic and no delivery of it was spent ([attachment.fold]).
func (a *attachment) nakAll(ctx context.Context, items []delivery) {
	for _, d := range items {
		if d.folded() {
			continue
		}
		a.handBack(ctx, d.msg)
	}
}
