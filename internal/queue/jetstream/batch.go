package jetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// delivery pairs a decoded event with the message it must be acked through,
// its stream sequence (zero where its metadata will not parse), and whether a
// Readmit handed it back ([queue.OutcomeAside]).
type delivery struct {
	msg        jetstream.Msg
	ev         *events.Event
	seq        uint64
	readmitted bool
}

func eventOf(d delivery) *events.Event { return d.ev }

// SubscribeBatch attaches with batched, key-partitioned delivery.
//
// The cycle: drain what is locally available (plus a linger window),
// partition by the key [queue.BatchKeyFunc] derives, dispatch one handler
// call per partition oldest-partition-first, and ack per partition. A
// failing partition never blocks or replays a different one from the same
// drain. Ten comments on one issue thus cost one agent turn, and what queued
// while an agent was busy coalesces into one batch.
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
			// THE LOOP TOP [Queue.Loops] reports: the gate is read once,
			// and what it said is what the loop then does.
			blocked := a.blocked()
			a.reachedTop(blocked)
			if blocked {
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
			// not be flushed past: a paused inbox starts no turn. Hand the
			// batch straight back. A cancelled context is teardown, so the
			// loop ends; a hold is released in place (ResumeTopic), so the
			// loop must `continue` as at its top — ending it would leave
			// the seat attached, owning its lease, and silently reading
			// nothing for the rest of the process's life.
			if ctx.Err() != nil {
				a.handBackUndispatched(ctx, batch, handBackStopping)
				return
			}
			if a.blocked() {
				a.setBack(ctx, batch, a.blockedBy())
				continue
			}
			a.dispatchBatch(ctx, batch, h, key, opts)
		}
	})
}

// drain collects one cycle's worth of deliveries, then sends the spaced returns
// the loop owes behind them, or behind none waiting; a failed fetch keeps them
// owed ([attachment.sendSpaced]).
//
// Options are read HERE, once per cycle, so a live config reload takes effect
// on the next batch with no re-subscription — and once the cycle's FIRST
// MESSAGE IS IN HAND, not when the cycle starts: an idle cycle parks up to a
// poll ([Queue.fetchWait]) in that first fetch, and options read before it
// would apply a reload landing meanwhile only to the batch after the burst it
// was meant for. The window is measured from the first message, so that
// instant also says how long it is.
func (a *attachment) drain(ctx context.Context, opts *queue.BatchOptions) []delivery {
	var batch []delivery
	// Counted for [Queue.InWindow], and cleared on every way out: what a
	// returned drain holds is no window's.
	defer a.windowed.Store(0)
	collect := func(msg jetstream.Msg) {
		if d, ok := a.toDelivery(msg); ok {
			batch = append(batch, d)
			a.windowed.Store(int64(len(batch)))
		}
	}
	// THE PRIORITY LANE FIRST, all the cap allows, without waiting: a
	// person's message gets into the drain while the topic's head is a
	// backlog nobody waits for, and is dispatched ahead of it
	// ([queue.OrderForDispatch]). The cap is read ahead of the first message
	// because the lane is asked before anything is known to wait; the window
	// is still read once a message is in hand. Then what a Readmit handed
	// back ([queue.OutcomeAside]), fetched before anything the topic holds.
	if err := a.fetchLane(opts.EffectiveMaxBatch(), collect); err != nil {
		a.logLaneFetchErr(ctx, err)
	}
	for _, d := range a.takeReadmitted(opts.EffectiveMaxBatch() - len(batch)) {
		batch = append(batch, d)
		a.windowed.Store(int64(len(batch)))
	}
	if len(batch) == 0 {
		//nolint:contextcheck // a fetch runs on the attachment's fetch scope, never the loop's: see [attachment.fetching]
		answered, err := a.fetch(1, a.q.fetchWait(), collect)
		if err != nil {
			a.logFetchErr(ctx, err)
			return nil
		}
		if len(batch) == 0 {
			if answered {
				a.sendSpaced()
			}
			return nil
		}
	}
	maxBatch := opts.EffectiveMaxBatch()
	linger := opts.EffectiveLinger()

	// The linger window is measured from the FIRST message and is fixed,
	// not sliding: a steady trickle must not delay dispatch unboundedly.
	// With linger 0 (the default) this still drains everything already
	// available, at zero added latency. Its end is read off [lingerClock].
	clock := a.q.windowClock()
	deadline := clock.Now().Add(linger)
	defer a.lingering.Store(false)
	for len(batch) < maxBatch {
		if ctx.Err() != nil || a.blocked() {
			break
		}
		wait := a.q.drainFetchWait()
		if remaining := deadline.Sub(clock.Now()); remaining > wait {
			wait = min(remaining, a.q.fetchWait())
		}
		//nolint:contextcheck // the attachment's fetch scope, as above
		if _, err := a.fetch(maxBatch-len(batch), wait, collect); err != nil {
			break
		}
		// THE WINDOW IS THE BOUND, and only the window: also requiring an
		// empty fetch to end would let any arrival slide it, to maxBatch ×
		// drainWait, spent from the ack budget a whole turn needs too. An
		// empty fetch is NOT an ending on its own, since a positive window
		// waits for arrivals not yet made. With linger 0 exactly one tail
		// fetch runs, asking for the whole remaining cap.
		if !clock.Now().Before(deadline) {
			break
		}
		// THE WINDOW HAS OUTLIVED A FETCH, so what is collected from here is
		// collected because the window is open. Only a window longer than the
		// poll ([Queue.fetchWait]) gets here; the conformance harness polls
		// every 25 ms.
		a.lingering.Store(true)
	}
	a.sendSpaced()
	return batch
}

// lingerClock is what a drain's linger window ends on: the wall clock, unless
// this package's own tests hand the queue another (Config.clock), so a
// conformance case can land a verb inside an open window and only then end it.
// It decides the window's end and nothing else; each fetch keeps its expiry on
// the wall clock (package doc, "A linger window ends on a clock the
// conformance suite can move").
type lingerClock interface {
	Now() time.Time
}

// wallClock is the wall clock as a [lingerClock].
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// windowClock is the clock q's drains end their linger windows on, and the one
// place an unset Config.clock is read as the wall clock.
func (q *Queue) windowClock() lingerClock {
	if q.cfg.clock != nil {
		return q.cfg.clock
	}
	return wallClock{}
}

func (a *attachment) toDelivery(msg jetstream.Msg) (delivery, bool) {
	ev, ok := a.decode(msg)
	if !ok {
		return delivery{}, false
	}
	d := delivery{msg: msg, ev: ev}
	if md, err := msg.Metadata(); err == nil {
		d.seq = md.Sequence.Stream
	}
	return d, true
}

// dispatchBatch partitions a drain and runs one handler per partition, asking
// the priority lane again before each one after the first, so lane mail that
// landed while a partition ran is the next turn ([queue.JoinLane]).
func (a *attachment) dispatchBatch(ctx context.Context, batch []delivery, h queue.BatchHandler,
	key queue.BatchKeyFunc, opts *queue.BatchOptions) {

	parts := queue.OrderForDispatch(queue.PartitionByKey(batch, key, eventOf), eventOf)

	for first := true; len(parts) > 0; first = false {
		if !first && !a.blocked() {
			var lane []delivery
			if err := a.fetchLane(opts.EffectiveMaxBatch(), func(msg jetstream.Msg) {
				if d, ok := a.toDelivery(msg); ok {
					lane = append(lane, d)
				}
			}); err != nil {
				a.logLaneFetchErr(ctx, err)
			}
			parts = queue.JoinLane(parts, lane, key, eventOf)
		}
		part := parts[0]
		parts = parts[1:]
		// A deferral, a detach or a pause taken mid-drain stops the rest:
		// the remaining partitions go back unhandled, to the successor.
		//
		// UNHANDLED IS NOT UNCHARGED: a fetch already spent their
		// delivery, so the hand-back only decides whether the last one
		// dead-letters with a copy or is dropped by MaxDeliver in silence.
		// That is the contract's rule ([queue.DeliveriesLeft]).
		if a.blocked() {
			a.setBack(ctx, part.Items, a.blockedBy())
			continue
		}

		evs := make([]*events.Event, len(part.Items))
		for i, d := range part.Items {
			evs[i] = d.ev
		}

		a.q.beginHandler()
		done := a.runningOn(evs...)
		// EACH MESSAGE'S OWN HEADROOM, and the partition's folded from
		// them: one outcome spends a delivery of every message, so the
		// nearest its budget dead-letters first, while a handler deciding
		// about ONE event asks what that message has left
		// ([queue.DeliveriesLeft], [queue.DeliveriesLeftFor]).
		a.beginDispatch()
		res := runBatchHandler(a.withDelivery(ctx, part.Items...), a.log, evs, h)

		a.applyPartition(ctx, part.Key, part.Items, evs, res)
		// IN FLIGHT UNTIL THE OUTCOME IS SETTLED, a dead letter's keep
		// included ([attachment.dispatchOne]).
		a.q.endHandler()
		// AFTER THE OUTCOME IS APPLIED: only then does an acked message
		// leave the consumer's unfinished count.
		done()
	}
}

// applyPartition applies one outcome to every message in a partition.
//
// The handler saw the partition as a unit and its verdict covers all of it;
// acking some and NAKing others would hand the successor a partial one.
func (a *attachment) applyPartition(ctx context.Context, batchKey string, items []delivery, evs []*events.Event, res queue.Result) {
	queue.LogBatchResult(ctx, a.log, a.key.topic, a.key.group, batchKey, evs, res)

	switch res.Outcome {
	case queue.OutcomeAck:
		for _, d := range items {
			// FORGOTTEN FIRST, for [attachment.apply]'s reason: a
			// later redelivery of the same sequence must not carry
			// these failures, and the map must not grow for the
			// attachment's life.
			var mark uint64
			if md, err := d.msg.Metadata(); err == nil {
				mark = a.settled(md.Sequence.Stream)
			}
			if err := d.msg.Ack(); err != nil {
				a.log.Warn("ack_failed", "error", err.Error())
			}
			a.clearMark(ctx, mark)
		}
	case queue.OutcomeDefer:
		a.nakAll(ctx, items, res.Reason)
		a.setQuiesced(true)
	case queue.OutcomeNak:
		for _, d := range items {
			a.nakOrDeadLetter(ctx, d.msg, d.ev, res.FailsMessage())
		}
	case queue.OutcomePass:
		// Each message by its own delivery count, under the partition's
		// reason, and the attachment goes on taking mail ([attachment.pass]).
		for _, d := range items {
			a.pass(ctx, d.msg, d.ev, res.Reason)
		}
	case queue.OutcomeAside:
		a.setAside(ctx, items, res.Key)
	}
}

// nakAll hands a whole batch back — a deferral, a hold or a pause that landed
// mid-drain, or teardown — for reason. Nothing is wrong with any of these
// messages.
//
// THROUGH THE BUDGET CHECK ([attachment.handBack]), because a Nak spends a
// delivery whatever it means: a bare msg.Nak() lets a message whose deliveries
// lease moves and sandbox parks spent be discarded by MaxDeliver in silence,
// with no dead-letter copy and no `dead_lettered` line.
func (a *attachment) nakAll(ctx context.Context, items []delivery, reason string) {
	for _, d := range items {
		a.handBack(ctx, d.msg, d.ev, reason)
	}
}

// handBackUndispatched hands back what a drain fetched and this consumer will
// not dispatch — a stop, a hold, a pause, a quiesce or a detach met before a
// handler ran — for reason ([attachment.nakAll]).
//
// IN FLIGHT UNTIL IT IS SETTLED, though no handler runs
// ([attachment.dispatchOne]): a hand-back may dead-letter, and that keep
// crosses a link at its bytes' pace under a drain that would otherwise read
// the queue as idle and stop it.
func (a *attachment) handBackUndispatched(ctx context.Context, items []delivery, reason string) {
	a.q.beginHandler()
	defer a.q.endHandler()
	a.nakAll(ctx, items, reason)
}
