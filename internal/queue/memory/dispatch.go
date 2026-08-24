package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// drain delivers from a subscription's mailbox while some attachment can take
// it. It must be called with the broker lock NOT held, and never holds it
// across a handler.
//
// Only one pass runs at a time per subscription. A second caller — a publish
// from inside a running handler, or a linger window expiring under an
// in-progress drain — asks the running pass to go round again instead of
// starting a nested one. Two passes over one mailbox would interleave, and
// interleaving is exactly the conversation reordering the whole batch layer
// exists to prevent.
//
// One liveness property callers should know, and the price of dispatching
// inline: a handler that publishes into the subscription it is draining feeds
// this loop. Doing so on EVERY invocation generates work faster than the loop
// retires it, and the loop does not return until the mailbox empties — so the
// publishing goroutine never gets control back. A real broker does not hang
// there, it just never catches up, because its dispatch is asynchronous. There
// is no cap here on purpose: a limit would be an invented constant that changes
// delivery semantics for every well-behaved caller in order to bound one that
// is asking for infinite work.
func (b *Broker) drain(ctx context.Context, sub *subscription, bypassLinger bool) {
	b.mu.Lock()
	if sub.draining {
		sub.drainAgain = true
		sub.drainAgainBypass = sub.drainAgainBypass || bypassLinger
		b.mu.Unlock()
		return
	}
	sub.draining = true
	b.mu.Unlock()

	for {
		b.drainPass(ctx, sub, bypassLinger)

		b.mu.Lock()
		if !sub.drainAgain {
			sub.draining = false
			b.mu.Unlock()
			return
		}
		bypassLinger = sub.drainAgainBypass
		sub.drainAgain, sub.drainAgainBypass = false, false
		b.mu.Unlock()
	}
}

// drainPass delivers until the mailbox is empty, nothing can take a delivery,
// or a batch consumer opens a linger window.
//
// Deliverability is re-checked every iteration, so a handler that re-pauses its
// own subscription — the sandbox busy gate's whole purpose — stops the drain at
// the next event rather than being run over. Events published during a pass
// join the tail, so the mailbox stays FIFO instead of being overtaken by new
// arrivals.
func (b *Broker) drainPass(ctx context.Context, sub *subscription, bypassLinger bool) {
	for {
		b.mu.Lock()
		if len(sub.mail) == 0 {
			b.mu.Unlock()
			return
		}
		// The round robin runs over the members that CAN take a delivery,
		// so one paused node does not stall a seat another node is
		// serving — and a subscription with no deliverable member simply
		// retains its mail, which is the unowned-seat case.
		members := deliverableMembersLocked(sub)
		if len(members) == 0 {
			b.mu.Unlock()
			return
		}
		m := members[sub.cursor%len(members)]

		if m.batched && !bypassLinger {
			if linger := m.options.EffectiveLinger(); linger > 0 {
				// Hold the window open and stop draining. The window is
				// fixed from the first waiting event, not sliding: later
				// publishes join this mailbox without resetting it, so a
				// steady trickle cannot delay dispatch unboundedly.
				b.openWindowLocked(sub, m, linger)
				b.mu.Unlock()
				return
			}
		}
		sub.cursor++

		if !m.batched {
			ev := sub.mail[0]
			sub.mail = sub.mail[1:]
			b.mu.Unlock()
			b.deliverOne(ctx, sub, m, ev)
			continue
		}

		chunk := takeChunkLocked(sub, m)
		b.mu.Unlock()
		b.deliverBatch(ctx, sub, m, chunk)
	}
}

// deliverableMembersLocked answers, of each CONSUMER rather than of the
// subscription, whether it can take a delivery right now. Every gate belongs to
// a process: its queue must be started and not drain-paused, and its own hold
// and quiesce flags must be clear. A subscription-level answer would let one
// node's sandbox pause, or one node's shutdown, stop a peer from serving the
// seat it owns.
func deliverableMembersLocked(sub *subscription) []*consumer {
	var out []*consumer
	for _, m := range sub.members {
		if deliverableLocked(m) {
			out = append(out, m)
		}
	}
	return out
}

func deliverableLocked(m *consumer) bool {
	c := m.client
	if c == nil || !c.running || c.paused {
		return false
	}
	if len(c.pauses[m.key]) > 0 {
		return false
	}
	_, quiesced := c.quiescing[m.key]
	return !quiesced
}

// takeChunkLocked removes up to max_batch events from the head of the mailbox.
//
// Draining everything already waiting into ONE delivery per conversation is the
// property inbox batching exists for: events that queued while an agent was
// busy must arrive as one turn, not N.
func takeChunkLocked(sub *subscription, m *consumer) []*events.Event {
	n := min(m.options.EffectiveMaxBatch(), len(sub.mail))
	chunk := make([]*events.Event, n)
	copy(chunk, sub.mail[:n])
	sub.mail = sub.mail[n:]
	return chunk
}

func (b *Broker) deliverOne(ctx context.Context, sub *subscription, m *consumer, ev *events.Event) {
	b.invoke(ctx, sub, m, []*events.Event{ev},
		func(hctx context.Context) queue.Result { return m.handler(hctx, ev) },
		"handler_failed",
		[]any{"topic", sub.topic, "group", sub.group, "event_type", ev.Type},
	)
}

// deliverBatch partitions a chunk and dispatches one handler call per
// conversation, acking per partition: a failing partition never blocks or
// replays a different one from the same drain.
func (b *Broker) deliverBatch(ctx context.Context, sub *subscription, m *consumer, chunk []*events.Event) {
	if len(chunk) == 0 {
		return
	}
	parts := queue.OrderForDispatch(queue.PartitionByKey(chunk, m.batchKey, sameEvent), sameEvent)

	// The identities of this chunk's events, so a mid-batch quiesce can
	// tell what a deferral pushed back to the FRONT from what was already
	// queued behind it — and slot the undispatched partitions between the
	// two.
	inChunk := make(map[*events.Event]struct{}, len(chunk))
	for _, ev := range chunk {
		inChunk[ev] = struct{}{}
	}

	for i, part := range parts {
		b.mu.Lock()
		if m.client != nil && m.client.isQuiescedLocked(m.key) {
			// Quiesced — by an earlier partition in this very batch, or
			// between batches. Stop dispatching and put the rest back for
			// whoever attaches next. A real broker checks exactly here,
			// and the twin not doing so was worse than a missing guard:
			// after one partition deferred, the loop went on invoking the
			// handler for partitions 2..N on a seat this node had just
			// been told it does not own, and each deferral pushed its
			// partition to the front — so the mailbox came back in
			// REVERSE partition order, which is precisely the reordering
			// a deferral exists to prevent.
			b.restoreLocked(sub, parts[i:], inChunk)
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()

		evs := part.Items
		b.invoke(ctx, sub, m, evs,
			func(hctx context.Context) queue.Result { return m.batchHandler(hctx, evs) },
			// The same machine-parsable failure event a real backend
			// emits for batch partitions: log consumers must not see
			// different names per backend.
			"batch_handler_failed",
			[]any{
				"topic", sub.topic, "group", sub.group,
				// firstType, not evs[0].Type: this is a LOG FIELD, and
				// indexing here happens outside the recover that contains
				// a handler panic, so an empty partition would take the
				// process down to decorate a log line. PartitionByKey
				// cannot produce one today — which is exactly why the
				// index looked safe, and why it would only ever fire on a
				// day something upstream was already wrong.
				"batch_key", part.Key, "event_type", firstType(evs),
				"event_count", len(evs),
			},
		)
	}
}

// restoreLocked puts the undispatched partitions back AFTER the partition that
// deferred, never before it: applying the deferral has already pushed that
// partition to the front, so restoring ahead of it would reverse the very order
// this guard exists to keep.
//
// The splice point is found by SCANNING the leading run of chunk events, not by
// a length delta. A delta assumes the mailbox only grew at the front, and
// publishing appends at the TAIL — a handler that publishes lets one land
// mid-loop, and the splice point then lands inside the pre-existing tail and
// reorders exactly what this is protecting.
func (b *Broker) restoreLocked(sub *subscription, remaining []queue.Partition[*events.Event], inChunk map[*events.Event]struct{}) {
	var undispatched []*events.Event
	for _, part := range remaining {
		undispatched = append(undispatched, part.Items...)
	}
	restored := 0
	for _, ev := range sub.mail {
		if _, fromChunk := inChunk[ev]; !fromChunk {
			break
		}
		restored++
	}
	spliced := make([]*events.Event, 0, len(sub.mail)+len(undispatched))
	spliced = append(spliced, sub.mail[:restored]...)
	spliced = append(spliced, undispatched...)
	spliced = append(spliced, sub.mail[restored:]...)
	sub.mail = spliced
}

func (q *Queue) isQuiescedLocked(key subKey) bool {
	_, held := q.quiescing[key]
	return held
}

// invoke runs one delivery and applies the handler's outcome.
//
// Ack drops the events — they were removed from the mailbox before the call.
// Nak returns them to the FRONT (order is what a conversation depends on) with
// their redelivery counters bumped, and a message past the budget moves to the
// dead-letter subject instead of being destroyed. Defer returns them without
// bumping anything and quiesces the attachment: a seat whose lease moved is not
// a failed handler and must not spend the message's dead-letter budget.
func (b *Broker) invoke(
	ctx context.Context,
	sub *subscription,
	m *consumer,
	evs []*events.Event,
	call func(context.Context) queue.Result,
	failureEvent string,
	attrs []any,
) {
	client := m.client

	// In-flight is counted on the node that OWNS the handler, not on
	// whoever's publish happened to drive the drain: a drain waits for the
	// handlers it is responsible for, and inline dispatch is a property of
	// this twin, not of the fleet it models.
	b.mu.Lock()
	client.enterHandlerLocked()
	b.mu.Unlock()

	res := runHandler(ctx, call)

	b.mu.Lock()
	client.exitHandlerLocked()
	var dead []deadLetter
	switch res.Outcome {
	case queue.OutcomeDefer:
		// Quiesce the ATTACHMENT that deferred, not the subscription: a
		// seat whose lease moved is not owned by this node, and stopping
		// the subscription would also stop the peer that now owns it from
		// picking these very events up.
		client.quiescing[m.key] = struct{}{}
		sub.mail = prepend(evs, sub.mail)
	case queue.OutcomeNak:
		dead = b.redeliverOrDeadLetterLocked(sub, evs, client.maxRedeliveries)
	case queue.OutcomeAck:
		// Acked: the events left the mailbox before the call, so there
		// is nothing to put back.
	}
	b.mu.Unlock()

	// Logging happens OUTSIDE the critical section. A log call is a write
	// to a handler this package does not control: it can block on a full
	// pipe, a slow file, or a contended CI stderr, and the broker mutex
	// guards every subscription and every peer on this broker. Holding it
	// across that write stops the whole company — a drain, every publish,
	// every attachment change — for as long as one write takes. It is the
	// oldest way to turn a slow logger into an outage, and the reason
	// nothing here logs while holding the lock.
	switch res.Outcome {
	case queue.OutcomeDefer:
		log.Info("delivery_deferred", append(attrs, "reason", deferralReason(res))...)
	case queue.OutcomeNak:
		log.Warn(failureEvent, append(attrs, "error", errText(res.Err))...)
	case queue.OutcomeAck:
	}
	for _, d := range dead {
		log.Error("event_dead_lettered", "topic", sub.topic, "group", sub.group,
			"event_type", d.ev.Type, "redeliveries", d.redeliveries)
	}
}

// deadLetter is one event that exhausted its budget, carried back out of the
// critical section so it can be logged without the broker lock held.
type deadLetter struct {
	ev           *events.Event
	redeliveries int
}

// runHandler recovers a panicking handler into a Nak, preserving "an
// unexpected failure redelivers". A panic must not take the drain — or the
// publishing goroutine it may be running on — down with it.
func runHandler(ctx context.Context, call func(context.Context) queue.Result) (res queue.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = queue.Nak(fmt.Errorf("handler panicked: %v", r))
		}
	}()
	return call(ctx)
}

// redeliverOrDeadLetterLocked returns the events that exhausted their budget,
// for the caller to log once it has released the lock.
func (b *Broker) redeliverOrDeadLetterLocked(sub *subscription, evs []*events.Event, budget int) []deadLetter {
	var keep []*events.Event
	var dead []deadLetter
	for _, ev := range evs {
		// The budget counts redeliveries AFTER the first delivery, so a
		// budget of N means N+1 total attempts. Counting total attempts
		// and then dropping made every retry-count assertion off by one
		// and every "poison message is recoverable" claim false.
		count := sub.redeliveries[ev.ID] + 1
		if count > budget {
			delete(sub.redeliveries, ev.ID)
			subject := topics.DeadLetter(sub.topic, sub.group)
			b.deadLetters[subject] = append(b.deadLetters[subject], ev)
			dead = append(dead, deadLetter{ev: ev, redeliveries: count - 1})
			continue
		}
		sub.redeliveries[ev.ID] = count
		keep = append(keep, ev)
	}
	sub.mail = prepend(keep, sub.mail)
	return dead
}

// prepend returns head followed by tail, allocating rather than shifting in
// place: the caller's slices are read after this returns.
func prepend(head, tail []*events.Event) []*events.Event {
	if len(head) == 0 {
		return tail
	}
	out := make([]*events.Event, 0, len(head)+len(tail))
	out = append(out, head...)
	return append(out, tail...)
}

// --- the linger window ----------------------------------------------------

// lingerWindow is one batch consumer's open collection window. It is owned by
// the consumer that opened it and torn down with the attachment, so a detach or
// a stop can never leave a timer that flushes into a seat this node no longer
// serves.
type lingerWindow struct {
	timer *time.Timer
	// closed guards against the race every one-shot timer has: the
	// callback may already be running when Stop is called, so it re-reads
	// this under the broker lock before it delivers anything.
	closed bool
}

func (b *Broker) openWindowLocked(sub *subscription, m *consumer, linger time.Duration) {
	if m.window != nil {
		return
	}
	w := &lingerWindow{}
	m.window = w
	// The window fires outside any caller's context — the publish that
	// opened it has long returned — so the flush runs on a background
	// context rather than one that may already be cancelled.
	w.timer = time.AfterFunc(linger, func() { b.flushWindow(sub, m, w) })
}

// closeWindowLocked cancels an open window without delivering. What it was
// collecting is NOT lost: those events are in the mailbox, which outlives every
// attachment.
func (m *consumer) closeWindowLocked() {
	w := m.window
	if w == nil {
		return
	}
	m.window = nil
	w.closed = true
	w.timer.Stop()
}

func (b *Broker) flushWindow(sub *subscription, m *consumer, w *lingerWindow) {
	b.mu.Lock()
	if w.closed || m.window != w {
		b.mu.Unlock()
		return
	}
	m.window = nil
	w.closed = true
	deliverable := deliverableLocked(m)
	backlog := len(sub.mail)
	b.mu.Unlock()

	if !deliverable {
		// A stop or a pause during the window loses nothing: the events
		// are in the mailbox, which outlives both.
		log.Debug("memory_linger_window_closed_undelivered",
			"topic", sub.topic, "group", sub.group, "backlog", backlog)
		return
	}
	// Bypass the linger check that would otherwise re-open the window
	// immediately on the events this one collected.
	b.drain(context.Background(), sub, true)
}

// --- small helpers --------------------------------------------------------

func sameEvent(ev *events.Event) *events.Event { return ev }

// firstType names a batch for a log line, tolerating a batch with nothing in
// it. Diagnostics must not be able to fail louder than what they describe.
func firstType(evs []*events.Event) string {
	if len(evs) == 0 || evs[0] == nil {
		return ""
	}
	return evs[0].Type
}

func deferralReason(res queue.Result) string {
	if res.Reason == "" {
		return "unspecified"
	}
	return res.Reason
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
