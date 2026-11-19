package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
)

// drain delivers from a subscription's mailbox while some attachment can take
// it. Call it with the broker lock NOT held; it never holds it across a handler.
//
// One pass runs at a time per subscription; a second caller (a publish from a
// handler, a linger expiring mid-drain) asks it to go round again, since two
// passes would interleave and reorder the conversation.
//
// A handler publishing into its own subscription on EVERY invocation feeds
// this loop faster than it retires work, and the publisher never regains
// control (a real broker just never catches up). No cap on purpose: it would
// be an invented constant changing delivery for every well-behaved caller.
func (b *Broker) drain(ctx context.Context, sub *subscription) {
	b.mu.Lock()
	if sub.draining {
		sub.drainAgain = true
		b.mu.Unlock()
		return
	}
	sub.draining = true
	// Counted for [Broker.Idle] from here until the last pass returns: every
	// handler call this drain makes is inside that span, and a pass a second
	// caller asks for runs inside it too.
	b.busyLocked()
	b.mu.Unlock()

	for {
		b.drainPass(ctx, sub)

		b.mu.Lock()
		if !sub.drainAgain {
			sub.draining = false
			b.restLocked()
			b.mu.Unlock()
			return
		}
		sub.drainAgain = false
		b.mu.Unlock()
	}
}

// drainPass delivers until the mailbox is empty, nothing can take a delivery,
// or a batch consumer opens a linger window: which it does for any mail but
// the events a window's flush found waiting ([subscription.lingered]).
//
// Deliverability is re-checked every iteration, so a handler that pauses its
// own subscription (which is what a seat with no turn engine does before it
// requeues) stops the drain at the next event rather than being run over.
// Events published during a pass join the tail, so the mailbox stays FIFO
// instead of being overtaken by new arrivals.
func (b *Broker) drainPass(ctx context.Context, sub *subscription) {
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

		if m.batched && !lingeredLocked(sub) {
			if linger := m.options.EffectiveLinger(); linger > 0 {
				// Hold the window open and stop draining. The window is
				// fixed from the first waiting event, not sliding: later
				// publishes join this mailbox without resetting it, so a
				// steady trickle cannot delay dispatch unboundedly.
				b.openWindowLocked(ctx, sub, m, linger)
				b.mu.Unlock()
				return
			}
		}
		sub.cursor++

		if !m.batched {
			ev := takeOneLocked(sub)
			b.mu.Unlock()
			b.deliverOne(ctx, sub, m, ev)
			continue
		}

		chunk := takeChunkLocked(sub, m)
		if sub.drained == nil {
			sub.drained = map[*events.Event]struct{}{}
		}
		for _, ev := range chunk {
			sub.drained[ev] = struct{}{}
		}
		b.mu.Unlock()
		b.deliverBatch(ctx, sub, m, chunk)
	}
}

// lingeredLocked reports whether the mailbox still holds an event a linger
// window's flush found waiting, and forgets the set once it holds none: read
// off the mailbox, whatever road its events left by (a take, a stop, a
// requeue that re-sorts it), so a mark never outlives what it marked.
func lingeredLocked(sub *subscription) bool {
	if len(sub.lingered) == 0 {
		return false
	}
	if slices.ContainsFunc(sub.mail, func(ev *events.Event) bool {
		_, ok := sub.lingered[ev]
		return ok
	}) {
		return true
	}
	sub.lingered = nil
	return false
}

// deliverableMembersLocked answers, of each CONSUMER rather than of the
// subscription, whether it can take a delivery right now. Every gate belongs to
// a process: its queue must be started and not drain-paused, and its own hold
// and quiesce flags must be clear. A subscription-level answer would let one
// node's pause hold, or one node's shutdown, stop a peer from serving the seat
// it owns.
func deliverableMembersLocked(sub *subscription) []*consumer {
	var out []*consumer
	for _, m := range sub.members {
		if deliverableLocked(m) {
			out = append(out, m)
		}
	}
	return out
}

// deliverableLocked is the ONE predicate both the pre-drain gate and the
// between-partition guard ask: every condition of [queue.DeliveriesLeft] and
// jetstream's attachment.blocked(), plus a broker that went away
// ([Broker.Fail], no decision of the node's, so the contract omits it).
//
//   - detach, or a Stop that drops the client's consumers -> m.detached
//   - a stopped client                                     -> !c.running
//   - PauseDelivery, PauseTopic                            -> c.paused, c.pauses
//   - a deferral just applied, or Quiesce                  -> c.quiescing[m.key]
//
// DETACH IS ASKED FIRST: a dropped consumer's client may still be running,
// unpaused and unheld, so the other questions must not rule it in.
func deliverableLocked(m *consumer) bool {
	if m.detached {
		return false
	}
	c := m.client
	if c == nil || !c.running || c.paused || c.broker.down {
		return false
	}
	if len(c.pauses[m.key]) > 0 {
		return false
	}
	_, quiesced := c.quiescing[m.key]
	return !quiesced
}

// blockedByLocked names what stops m taking a delivery ([deliverableLocked]):
// the reason the twin's own hand-back of what a blocked drain never dispatched
// is logged with, should that hand-back spend a delivery's budget — the words
// the JetStream backend names its own blocked hand-backs by.
func blockedByLocked(m *consumer) string {
	c := m.client
	switch {
	case m.detached:
		return "detached"
	case c == nil || !c.running || c.broker.down:
		return "stopping"
	case c.paused:
		return "paused"
	case len(c.pauses[m.key]) > 0:
		return "held"
	}
	if _, quiesced := c.quiescing[m.key]; quiesced {
		return "quiesced"
	}
	return "blocked"
}

// takeChunkLocked removes up to max_batch events from the head of the mailbox.
//
// Draining everything waiting into ONE delivery per partition is what inbox
// batching is for: events queued while an agent was busy arrive as one turn.
//
// ON A LANED TOPIC THE LANE IS TAKEN FIRST ([queue.HasLane]): every priority
// event the mailbox holds, wherever it sits, then the head of the rest. One
// slice models the broker's two mailboxes because the lane's order and the
// topic's order are both the slice's order: a hand-back to the front keeps
// either kind ahead of its own kind, as the broker's redelivery does.
func takeChunkLocked(sub *subscription, m *consumer) []*events.Event {
	n := min(m.options.EffectiveMaxBatch(), len(sub.mail))
	if !queue.HasLane(sub.topic) {
		chunk := make([]*events.Event, n)
		copy(chunk, sub.mail[:n])
		sub.mail = sub.mail[n:]
		return chunk
	}
	chunk := make([]*events.Event, 0, n)
	rest := make([]*events.Event, 0, len(sub.mail))
	for _, ev := range sub.mail {
		if len(chunk) < n && queue.Prioritized(ev) {
			chunk = append(chunk, ev)
			continue
		}
		rest = append(rest, ev)
	}
	take := n - len(chunk)
	chunk = append(chunk, rest[:take]...)
	sub.mail = rest[take:]
	return chunk
}

// takeLaneLocked removes up to max_batch of the lane's events from the mailbox,
// in the order it holds them: what a drain already under way asks the broker's
// lane for between partitions. A topic with no lane has none.
func takeLaneLocked(sub *subscription, m *consumer) []*events.Event {
	if !queue.HasLane(sub.topic) {
		return nil
	}
	n := m.options.EffectiveMaxBatch()
	var lane []*events.Event
	rest := make([]*events.Event, 0, len(sub.mail))
	for _, ev := range sub.mail {
		if len(lane) < n && queue.Prioritized(ev) {
			lane = append(lane, ev)
			continue
		}
		rest = append(rest, ev)
	}
	if len(lane) > 0 {
		sub.mail = rest
	}
	return lane
}

// takeOneLocked removes the event a single-event consumer is handed next: the
// lane's first on a laned topic, as [takeChunkLocked] takes a batch, and the
// head of the mailbox otherwise.
func takeOneLocked(sub *subscription) *events.Event {
	i := 0
	if queue.HasLane(sub.topic) {
		if j := slices.IndexFunc(sub.mail, queue.Prioritized); j >= 0 {
			i = j
		}
	}
	ev := sub.mail[i]
	sub.mail = append(sub.mail[:i:i], sub.mail[i+1:]...)
	return ev
}

func (b *Broker) deliverOne(ctx context.Context, sub *subscription, m *consumer, ev *events.Event) {
	b.invoke(ctx, sub, m, []*events.Event{ev},
		func(hctx context.Context) queue.Result { return m.handler(hctx, ev) },
		func(lctx context.Context, res queue.Result) {
			queue.LogResult(lctx, log, sub.topic, sub.group, ev, res)
		},
	)
}

// deliverBatch partitions a chunk and dispatches one handler call per
// partition, acking per partition: a failing partition never blocks or
// replays a different one from the same drain.
func (b *Broker) deliverBatch(ctx context.Context, sub *subscription, m *consumer, chunk []*events.Event) {
	if len(chunk) == 0 {
		return
	}
	parts := queue.OrderForDispatch(queue.PartitionByKey(chunk, m.batchKey, sameEvent), sameEvent)

	// The identities of this chunk's events, so a mid-batch quiesce can
	// tell what a deferral pushed back to the FRONT from what was already
	// queued behind it — and slot the undispatched partitions between the
	// two. A partition that FAILED leaves this set as soon as it does, and
	// one that PASSED with it: each goes to the BACK
	// ([Broker.retryLaterLocked]), and on a mailbox that held nothing else it
	// would otherwise read as part of the leading run a deferral made, so the
	// undispatched partitions were spliced in behind a retry the broker
	// serves after them.
	inChunk := make(map[*events.Event]struct{}, len(chunk))
	for _, ev := range chunk {
		inChunk[ev] = struct{}{}
	}

	for i := 0; i < len(parts); i++ {
		b.mu.Lock()
		if i > 0 && deliverableLocked(m) {
			// THE LANE BEFORE EVERY PARTITION, as the broker's drain asks it.
			lane := takeLaneLocked(sub, m)
			for _, ev := range lane {
				sub.drained[ev], inChunk[ev] = struct{}{}, struct{}{}
			}
			parts = append(parts[:i:i], queue.JoinLane(parts[i:], lane, m.batchKey, sameEvent)...)
		}
		part := parts[i]
		if !deliverableLocked(m) {
			// BLOCKED, by an earlier partition or by something that landed
			// between them: put the rest back for whoever attaches next, as
			// a real broker does. Carrying on would run the handler for
			// partitions 2..N on a seat this node does not own, and each
			// deferral to the front would reverse the partition order.
			// The predicate is the pre-drain gate's, whole list at
			// [deliverableLocked]; a detach lives on the MEMBERSHIP, so
			// this loop's *consumer sees it only as [consumer.detached].
			// The rest is CHARGED FOR THE HAND-BACK: [Broker.restoreLocked].
			for _, rest := range parts[i:] {
				undrainLocked(sub, rest.Items)
			}
			dead := b.restoreLocked(sub, parts[i:], inChunk, m.client.maxRedeliveries,
				blockedByLocked(m))
			b.mu.Unlock()
			b.logDeadLetters(ctx, sub, dead)
			return
		}
		// RUNNING NOW, so no longer waiting ([Queue.Waiting]).
		undrainLocked(sub, part.Items)
		b.mu.Unlock()

		evs := part.Items
		outcome := b.invoke(ctx, sub, m, evs,
			func(hctx context.Context) queue.Result { return m.batchHandler(hctx, evs) },
			// THE CONTRACT'S PARTITION LINE, the one the backend that
			// ships emits: a log consumer must not see a partition's
			// outcome under a single delivery's name on one backend and
			// under its own on the other.
			func(lctx context.Context, res queue.Result) {
				queue.LogBatchResult(lctx, log, sub.topic, sub.group, part.Key, evs, res)
			},
		)
		if outcome == queue.OutcomeNak || outcome == queue.OutcomePass {
			for _, ev := range evs {
				delete(inChunk, ev)
			}
		}
	}
}

// undrainLocked takes events out of what a drain is holding undispatched
// ([subscription.drained]): their handler is starting, or they are going back
// to the mailbox.
func undrainLocked(sub *subscription, evs []*events.Event) {
	for _, ev := range evs {
		delete(sub.drained, ev)
	}
}

// restoreLocked puts the undispatched partitions back AFTER the partition that
// deferred, never before it, which would reverse the order this guard keeps.
//
// EACH PAYS A DELIVERY, so this returns what it dead-lettered: on JetStream the
// counter moves when a message is FETCHED (jetstream.dispatchBatch naks the
// rest through the same budget check). SETTLED UNDER THE LOCK, dead letters'
// keeps included, so no drain reads it in flight
// ([queue.EventQueue.WaitForHandlers]).
//
// The splice point is found by SCANNING the leading run of chunk events, not by
// a length delta: publishing appends at the TAIL, so a handler's publish can
// land mid-loop and a delta would splice inside the pre-existing tail.
func (b *Broker) restoreLocked(
	sub *subscription,
	remaining []queue.Partition[*events.Event],
	inChunk map[*events.Event]struct{},
	budget int,
	reason string,
) []deadLetter {
	var undispatched []*events.Event
	for _, part := range remaining {
		undispatched = append(undispatched, part.Items...)
	}
	// Charged BEFORE the splice, so what is spliced back is what survived:
	// an event whose budget the hand-back just spent belongs on the
	// dead-letter subject, not at the head of a mailbox no budget can ever
	// retire it from.
	keep, dead := b.chargeLocked(sub, undispatched, budget, queue.OutcomeDefer, reason)
	restored := 0
	for _, ev := range sub.mail {
		if _, fromChunk := inChunk[ev]; !fromChunk {
			break
		}
		restored++
	}
	spliced := make([]*events.Event, 0, len(sub.mail)+len(keep))
	spliced = append(spliced, sub.mail[:restored]...)
	spliced = append(spliced, keep...)
	spliced = append(spliced, sub.mail[restored:]...)
	sub.mail = spliced
	return dead
}

// invoke runs one delivery and applies the handler's outcome.
//
// Returns put events back where JetStream does (queue.OutcomeNak, OutcomePass,
// OutcomeDefer): a NAK and a PASS go BEHIND every event waiting at the time
// (retrying at the HEAD would certify a retry before a newer message of the
// conversation), a PASS quiescing nothing; a DEFER goes at the HEAD, where a
// plain Nak lands, and quiesces the attachment. The backoff is VIRTUAL, for inline deterministic
// dispatch: mail published inside a backoff window lands behind the retry here,
// ahead of it on the broker, so nothing above may lean on either
// (queue.OrderForDispatch). A DEFER SPENDS ONE DELIVERY, like a Nak, as on
// JetStream; the budget is sized so handoffs cannot kill a healthy event.
func (b *Broker) invoke(
	ctx context.Context,
	sub *subscription,
	m *consumer,
	evs []*events.Event,
	call func(context.Context) queue.Result,
	logResult func(context.Context, queue.Result),
) queue.Outcome {
	client := m.client

	// In-flight is counted on the node that OWNS the handler, not on
	// whoever's publish happened to drive the drain: a drain waits for the
	// handlers it is responsible for, and inline dispatch is a property of
	// this twin, not of the fleet it models.
	b.mu.Lock()
	client.enterHandlerLocked()
	// EACH MESSAGE'S REMAINING HEADROOM, read under the lock the counters
	// live under and before the handler runs — see [queue.DeliveriesLeftFor]
	// for the number one message carries and [queue.DeliveriesLeft] for the
	// partition's own, which the contract folds from this same list.
	// The twin models the delivery budget, so it owes a handler the same
	// numbers the real broker gives it, in the contract's convention rather
	// than in this backend's: what is carried is deliveries LEFT, where
	// this package counts redeliveries already spent.
	perMessage := headroomLocked(sub, evs, client.maxRedeliveries)
	b.mu.Unlock()
	// THE HANDLER IS DETACHED FROM THE CONTEXT THAT DROVE THE DRAIN, which
	// is a PUBLISHER's — inline dispatch is a property of this twin, and the
	// publisher is routinely another node. Its values ride along, its
	// cancellation does not: on a real broker nothing carries a stopping
	// publisher to a consumer, and dispatched under it every redelivery
	// after the cancellation ran on a dead context and failed at its first
	// call until the message was dead-lettered.
	ctx = queue.WithHeadroom(context.WithoutCancel(ctx), perMessage)

	res := runHandler(ctx, call)

	b.mu.Lock()
	var dead []deadLetter
	switch res.Outcome {
	case queue.OutcomeDefer:
		// Quiesce the ATTACHMENT that deferred, not the subscription: a
		// seat whose lease moved is not owned by this node, and stopping
		// the subscription would also stop the peer that now owns it from
		// picking these very events up.
		client.quiescing[m.key] = struct{}{}
		// THROUGH THE SAME BOUNDARY AS A NAK, because a hand-back spends
		// a delivery whatever it meant — see the doc above. The
		// dead-letter that ends it is the contract's too: a message whose
		// deliveries went on handoffs is dead-lettered with a line rather
		// than circling a mailbox no budget can ever retire it from.
		dead = b.handBackLocked(sub, evs, client.maxRedeliveries, res.Reason)
	case queue.OutcomeNak:
		// A FAILURE OF THE MESSAGE IS REMEMBERED until the message
		// leaves, so a later return that spends its budget — a
		// hand-back, a node's own failure — keeps it with the poison
		// ([queue.RetiredTo]); a node's own failure ([queue.NakNode]) is
		// not.
		if res.FailsMessage() {
			for _, ev := range evs {
				sub.failed[ev.ID] = struct{}{}
			}
		}
		dead = b.retryLaterLocked(sub, evs, client.maxRedeliveries, queue.OutcomeNak, "")
	case queue.OutcomePass:
		// BEHIND THE WAITING MAIL, AS A FAILURE, AND NOTHING QUIESCED: the
		// broker withholds a pass for the failure path's backoff, and the
		// attachment that passed goes on taking mail — see the doc above.
		// And a pass that spends the budget keeps its copy where the broker
		// keeps it, with its group's unhandled deliveries unless the
		// message failed as itself before ([queue.RetiredTo]).
		dead = b.retryLaterLocked(sub, evs, client.maxRedeliveries, queue.OutcomePass, res.Reason)
	case queue.OutcomeAck:
		// Acked: the events left the mailbox before the call, so there
		// is nothing to put back, and nothing of them to remember.
		for _, ev := range evs {
			delete(sub.failed, ev.ID)
		}
	}
	// IN FLIGHT UNTIL WHAT THE HANDLER RETURNED IS SETTLED, a dead letter's
	// keep included ([queue.EventQueue.WaitForHandlers]): instant here,
	// under the lock, where the broker's keep is a write that can cross a
	// link.
	client.exitHandlerLocked()
	b.mu.Unlock()

	// Logging happens OUTSIDE the critical section: a log write goes to a
	// handler this package does not control and can block on a full pipe or
	// slow file, while the broker mutex guards every subscription and peer, so
	// holding it would turn a slow logger into an outage.
	//
	// AND THROUGH THE CONTRACT'S OWN LINE ([queue.LogResult],
	// [queue.LogBatchResult]), never one spelled here: a partition's deferral
	// must log as `batch_delivery_deferred`, as the shipped backend does.
	logResult(ctx, res)
	b.logDeadLetters(ctx, sub, dead)
	return res.Outcome
}

// logDeadLetters reports what a charge retired, and is called with the broker
// lock RELEASED for the reason [Broker.invoke] gives above: a log write this
// package does not control must not be made while holding the mutex that
// guards every subscription and every peer on this broker.
//
// THROUGH THE CONTRACT'S OWN LINE ([queue.LogDeadLettered]), never one spelled
// here: the twin's was `event_dead_lettered`, with its redeliveries, where the
// backend that ships logs `dead_lettered` with its deliveries.
func (b *Broker) logDeadLetters(ctx context.Context, sub *subscription, dead []deadLetter) {
	for _, d := range dead {
		queue.LogDeadLettered(ctx, log, sub.topic, sub.group, d.ev, d.deliveries, d.spent, d.reason,
			d.failed)
	}
}

// headroomLocked is how many further deliveries each of a partition's events
// has before its budget is spent, in the contract's convention.
//
// An event on its FIRST delivery has the whole budget; one redelivered budget
// times has none and the next hand-back dead-letters it, exactly where
// [Broker.chargeLocked] draws the line: the two must agree on that boundary.
//
// PER EVENT AND NOTHING ELSE: the partition's number is [queue.WithHeadroom]'s,
// derived from this list, since folding it here would certify a bound
// production does not have.
func headroomLocked(sub *subscription, evs []*events.Event, budget int) []queue.Headroom {
	perMessage := make([]queue.Headroom, 0, len(evs))
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		perMessage = append(perMessage, queue.Headroom{
			ID:   ev.ID,
			Left: budget - sub.redeliveries[ev.ID],
		})
	}
	return perMessage
}

// deadLetter is one event that exhausted its budget, carried back out of the
// critical section so it can be logged without the broker lock held.
type deadLetter struct {
	ev *events.Event
	// deliveries is how many times it was delivered, the last included.
	deliveries int
	// spent is the return that spent its budget and reason why, which its
	// line says; failed is whether the message failed as itself at any of
	// its deliveries, which decides where its copy is kept
	// ([queue.RetiredTo]).
	spent  queue.Outcome
	reason string
	failed bool
}

// runHandler recovers a panicking handler into a Nak, preserving "an
// unexpected failure redelivers". A panic must not take the drain — or the
// publishing goroutine it may be running on — down with it.
func runHandler(ctx context.Context, call func(context.Context) queue.Result) (res queue.Result) {
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeQueue, r)
			log.Error("handler_panicked", "panic", r, "stack", contained.Stack)
			res = queue.Nak(fmt.Errorf("handler panicked: %w", contained.Err()))
		}
	}()
	return call(ctx)
}

// handBackLocked charges a HAND-BACK — a deferral — and returns the events to
// the FRONT of the mailbox, handing back the ones that exhausted their budget
// for the caller to log once it has released the lock.
//
// The front, because a hand-back is a healthy delivery this consumer may not
// work right now, and the broker serves it before anything newer: see
// [Broker.invoke]. The blocked-drain path charges the same way and splices
// elsewhere — see [Broker.restoreLocked] — and a failure goes to the back
// ([Broker.retryLaterLocked]), which is why the charge itself is
// [Broker.chargeLocked] and no path owns it.
func (b *Broker) handBackLocked(sub *subscription, evs []*events.Event, budget int,
	reason string) []deadLetter {

	keep, dead := b.chargeLocked(sub, evs, budget, queue.OutcomeDefer, reason)
	sub.mail = prepend(keep, sub.mail)
	return dead
}

// retryLaterLocked charges a FAILURE, or a PASS — spent, for reason — and
// returns the events BEHIND everything already waiting, which is where the
// shipped broker's redelivery backoff puts them: see [Broker.invoke] for why,
// and for what the virtual backoff does and does not reproduce.
//
// A failure that is the only thing in the mailbox therefore comes straight
// back, exactly as it would on the broker once its backoff elapsed with
// nothing else to serve — the virtual backoff has no wall clock, so a poison
// message still reaches its dead-letter boundary inside one drain rather than
// across minutes of timers.
func (b *Broker) retryLaterLocked(sub *subscription, evs []*events.Event, budget int,
	spent queue.Outcome, reason string) []deadLetter {

	keep, dead := b.chargeLocked(sub, evs, budget, spent, reason)
	sub.mail = append(sub.mail, keep...)
	return dead
}

// chargeLocked spends one delivery on each event, splitting survivors from the
// retired.
//
// THE ONE PLACE A DELIVERY IS SPENT in this backend, as [attachment.returnMsg]
// is in the JetStream one: a hand-back costs a delivery whatever put the
// message back (nak, deferral, or a drain told mid-flight to stop), and a
// second site would be a second answer to where the dead-letter line is. It
// leaves the mailbox to the caller, where the callers differ.
//
// spent is the return charged (Nak, Defer or Pass), reason why, for the line.
// A retired copy goes where [queue.RetiredTo] says, by [subscription.failed],
// which the caller records before charging a failure.
func (b *Broker) chargeLocked(sub *subscription, evs []*events.Event, budget int,
	spent queue.Outcome, reason string) ([]*events.Event, []deadLetter) {

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
			// KEPT AS ITS WIRE BYTES, with no header, beside every other
			// dead letter and on the subject the contract keeps the copy
			// of a message that did or did not fail as itself on, as the
			// broker this twin stands for republishes the message's own
			// data there: so DeadLettersOn lists it, DeadLetterFrame reads
			// its bytes and DropDeadLetter removes it exactly as there.
			// The event was decoded from JSON, so it encodes again.
			_, failed := sub.failed[ev.ID]
			delete(sub.failed, ev.ID)
			if wire, err := json.Marshal(ev); err == nil {
				b.keepLocked(queue.RetiredTo(sub.topic, sub.group, failed), nil, wire)
			}
			dead = append(dead, deadLetter{ev: ev, deliveries: count, spent: spent, reason: reason,
				failed: failed})
			continue
		}
		sub.redeliveries[ev.ID] = count
		keep = append(keep, ev)
	}
	return keep, dead
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
	// stop cancels the window's timer, on the clock of the client that
	// opened it ([lingerClock]).
	stop func() bool
	// closed guards against the race every one-shot timer has: the
	// callback may already be running when Stop is called, so it re-reads
	// this under the broker lock before it delivers anything.
	closed bool
}

// lingerClock is what a client's linger windows are timed on: the wall clock,
// unless this package's own tests hand the client another (export_test.go).
//
// A SEAM FOR THE CONFORMANCE SUITE, and the whole of what the twin's dispatch
// keeps on a clock: a window's timer is its one timer. A case asking what a
// pause, a hold or a stop does to a window that is open when it lands needs the
// window open at the verb and ended after it, and on the wall clock nothing
// orders the two: a runner that reached the verb late met a window that had
// already flushed. On a clock the case moves, the window ends where the case
// says and never before. Nothing else reads it: the instants a dead letter is
// stamped and let go by are the BROKER's clock, as the stream's are on the
// backend that ships, and a window is the consuming client's.
type lingerClock interface {
	// AfterFunc calls f on a goroutine of its own once d has passed, unless
	// the stop it returns is called first, which reports whether it was.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// wallClock is the wall clock as a [lingerClock].
type wallClock struct{}

func (wallClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// windowClock is the clock q's linger windows are timed on, and the one place
// an unset Queue.clock is read as the wall clock.
func (q *Queue) windowClock() lingerClock {
	if q.clock != nil {
		return q.clock
	}
	return wallClock{}
}

func (b *Broker) openWindowLocked(ctx context.Context, sub *subscription, m *consumer, linger time.Duration) {
	if m.window != nil {
		return
	}
	w := &lingerWindow{}
	m.window = w
	// Counted for [Broker.Idle] until it is closed undelivered or the drain
	// its flush starts has returned: a window still open is a delivery the
	// broker owes.
	b.busyLocked()
	// The window fires outside any caller's context — the publish that
	// opened it has long returned — so the flush must not inherit that
	// caller's cancellation, or a returned request would cancel a delivery
	// it has nothing more to do with. It DOES inherit the caller's values,
	// so the trace and the logging fields the batch belongs to survive the
	// timer hop; a bare Background would drop them.
	flushCtx := context.WithoutCancel(ctx)
	w.stop = m.client.windowClock().AfterFunc(linger, func() { b.flushWindow(flushCtx, sub, m, w) })
}

// closeWindowLocked cancels m's open window without delivering. What it was
// collecting is NOT lost: those events are in the mailbox, which outlives every
// attachment.
func (b *Broker) closeWindowLocked(m *consumer) {
	w := m.window
	if w == nil {
		return
	}
	m.window = nil
	w.closed = true
	w.stop()
	// Closed undelivered, so it owes nothing: a flush already waiting on
	// the lock reads closed and delivers nothing.
	b.restLocked()
}

func (b *Broker) flushWindow(ctx context.Context, sub *subscription, m *consumer, w *lingerWindow) {
	b.mu.Lock()
	if w.closed || m.window != w {
		b.mu.Unlock()
		return
	}
	m.window = nil
	w.closed = true
	deliverable := deliverableLocked(m)
	backlog := len(sub.mail)
	if deliverable {
		// WHAT THIS WINDOW COLLECTED has waited it out: the drain below
		// takes it without opening another. Only it — mail published after
		// this point has waited for nothing, and opens a window of its own
		// (#983). An undelivered window marks nothing: its mail waits for
		// the window the next drain opens.
		if sub.lingered == nil {
			sub.lingered = make(map[*events.Event]struct{}, backlog)
		}
		for _, ev := range sub.mail {
			sub.lingered[ev] = struct{}{}
		}
	}
	b.mu.Unlock()
	// THE WINDOW STAYS COUNTED UNTIL ITS FLUSH HAS RUN ([Broker.Idle]): the
	// drain below counts itself before this lets go, so the broker never
	// reads as idle between the two.
	defer func() {
		b.mu.Lock()
		b.restLocked()
		b.mu.Unlock()
	}()

	if !deliverable {
		// A stop or a pause during the window loses nothing: the events
		// are in the mailbox, which outlives both.
		log.DebugContext(ctx, "memory_linger_window_closed_undelivered",
			"topic", sub.topic, "group", sub.group, "backlog", backlog)
		return
	}
	// The events this window collected go without re-opening a window on
	// them: they are marked lingered above.
	b.drain(ctx, sub)
}

// --- small helpers --------------------------------------------------------

func sameEvent(ev *events.Event) *events.Event { return ev }
