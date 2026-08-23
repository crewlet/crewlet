package pulsar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// attachment is ONE consumer this process runs for a (topic, group) pair.
//
// A process may run several for one pair — the contract's "close this
// process's consumer(s)" is plural on purpose. A seat inbox has exactly one,
// because there membership IS ownership, but a fleet-wide worker group may
// legitimately run more than one member on a node.
//
// The consume loop OWNS the consumer's lifecycle. Everything else flips
// flags. That is what lets Detach return without joining a running handler
// while still guaranteeing the handler's ack reaches a live consumer: the
// loop finishes its dispatch, then closes.
type attachment struct {
	q   *Queue
	key attachKey
	log *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	// consMu guards cons, which the loop replaces on a recycle and clears
	// on exit. Read by the loop, and by nothing else — but a race detector
	// does not know that, and the loop is not the only goroutine that can
	// observe it during shutdown.
	consMu sync.Mutex
	cons   pulsar.Consumer
	opts   pulsar.ConsumerOptions

	// quiesced stops this consumer taking NEW work while staying attached.
	// A running handler finishes; nothing new is fetched. Its inverse is
	// required, not optional: a node whose coordination store blipped
	// quiesces and must be able to come back, or it holds the seat, stays
	// attached, and consumes nothing for the rest of its life — owned,
	// attached and silently deaf.
	quiesced atomic.Bool

	// recycle asks the loop to close and re-open the consumer.
	//
	// This is how Unquiesce RECLAIMS THE PREFETCH, which d-101 §3 requires
	// of any prefetching backend. Pulsar pushes up to ReceiverQueueSize
	// messages into a client-side queue whether or not anyone is reading,
	// and this client exposes no RedeliverUnacknowledgedMessages — so the
	// only way to hand that prefetch back is to close the consumer, which
	// on Pulsar returns everything unacked at redeliveryCount 0 in about
	// 9 ms. Re-opening immediately re-fetches it.
	//
	// Set by Unquiesce rather than done by Quiesce, because a quiesce must
	// let a RUNNING handler finish and that handler's ack has to reach a
	// live consumer.
	recycle atomic.Bool

	// paused is the process-wide shutdown delivery pause.
	paused atomic.Bool

	// detached is set the moment Detach or Stop is called. It is what
	// makes the contract's two halves both true at once: Detach returns
	// WITHOUT joining a running handler, and yet no NEW handler starts
	// after it returns.
	detached atomic.Bool
}

// blocked reports whether this consumer may take new work.
func (a *attachment) blocked() bool {
	return a.detached.Load() || a.quiesced.Load() || a.paused.Load() || a.q.held(a.key)
}

// closing reports whether this attachment's consumer is about to be closed or
// recycled — which is what decides how undispatched work is handed back. See
// handBack.
func (a *attachment) closing() bool {
	return a.detached.Load() || a.quiesced.Load() || a.paused.Load()
}

func (a *attachment) current() pulsar.Consumer {
	a.consMu.Lock()
	defer a.consMu.Unlock()
	return a.cons
}

// stop makes this attachment take no further work and unblocks its loop.
//
// It deliberately does NOT close the consumer or join a running handler: the
// loop closes on its way out, so an in-flight handler's ack still lands. That
// is the fenced-release path — a node that has LOST a seat's lease must give
// it up now rather than when the turn it should not be running finally ends.
func (a *attachment) stop() {
	a.detached.Store(true)
	if a.cancel != nil {
		a.cancel()
	}
}

// wait blocks until the consume loop has exited, or the deadline passes.
func (a *attachment) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.done:
		return true
	case <-t.C:
		return false
	}
}

// --- pause holds ----------------------------------------------------------
//
// Holds live on the QUEUE, keyed by the (topic, group) pair, not on an
// attachment. Two reasons, both learned the hard way.
//
// Keyed by the PAIR rather than by topic alone, because two subsystems gate
// one inbox — the sandbox busy gate and the config-divergence shed — and a
// topic-keyed hold also gated every other group on a shared subject.
//
// Owned by the QUEUE rather than by an attachment, because a hold is
// routinely taken BEFORE anything attaches: an engine that knows a seat is
// busy pauses first and attaches after. A hold stored on the attachment would
// simply be dropped in that window, and the seat would take exactly the work
// it was told not to.

func (q *Queue) held(key attachKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.holds[key]) > 0
}

// PauseTopic takes a named hold on one subscription.
func (q *Queue) PauseTopic(_ context.Context, topic, group, reason string) error {
	if reason == "" {
		reason = "default"
	}
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.holds[key] == nil {
		q.holds[key] = map[string]struct{}{}
	}
	q.holds[key][reason] = struct{}{}
	return nil
}

// ResumeTopic releases one reason's hold, flushing when none remain.
func (q *Queue) ResumeTopic(_ context.Context, topic, group, reason string) error {
	if reason == "" {
		reason = "default"
	}
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.holds[key], reason)
	if len(q.holds[key]) == 0 {
		delete(q.holds, key)
	}
	return nil
}

// PauseHolds reports the reasons currently holding a subscription paused.
//
// Operator- and test-facing: which subsystem is gating a seat is the first
// question when one goes quiet, and a hold is invisible from outside
// otherwise.
func (q *Queue) PauseHolds(topic, group string) []string {
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.holds[key]))
	for r := range q.holds[key] {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// --- attaching ------------------------------------------------------------

// attach ensures the subscription exists, opens a consumer and starts a loop.
func (q *Queue) attach(ctx context.Context, topic, group string, maxBatch int, loop func(context.Context, *attachment)) error {
	if err := checkSubject(topic); err != nil {
		return err
	}
	if group == "" {
		return fmt.Errorf("%w: empty group", ErrSubject)
	}
	// Through the admin API, always, and BEFORE the consumer: a
	// subscription that this consumer created would have been created by
	// joining, which is the traffic-stealing hazard restAdmin exists to
	// avoid — and one created at Latest would silently drop everything
	// published before now.
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		return err
	}

	opts := q.consumerOptions(topic, group, maxBatch)
	cons, err := q.client.Subscribe(opts)
	if err != nil {
		return fmt.Errorf("subscribe %s/%s: %w", topic, group, err)
	}

	key := attachKey{topic, group}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		cons.Close()
		return ErrClosed
	}
	// A process-wide delivery pause must apply to an attachment created
	// during a drain, or a late subscribe would start handlers the drain
	// is waiting to finish.
	paused := q.paused
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a := &attachment{
		q: q, key: key, cons: cons, opts: opts,
		log:    q.log.With("topic", topic, "group", group),
		cancel: cancel, done: make(chan struct{}),
	}
	a.paused.Store(paused)
	q.attachments[key] = append(q.attachments[key], a)
	q.mu.Unlock()

	go func() {
		defer close(a.done)
		defer a.closeConsumer()
		loop(loopCtx, a)
	}()
	return nil
}

// consumerOptions is the ONE construction path for a durable consumer, so the
// broker-side policy can never diverge between Subscribe and SubscribeBatch.
func (q *Queue) consumerOptions(topic, group string, maxBatch int) pulsar.ConsumerOptions {
	return pulsar.ConsumerOptions{
		Topic:            q.cfg.fullTopic(topic),
		SubscriptionName: group,
		// SHARED: every consumer that joins one subscription shares one
		// cursor, so each message reaches exactly one member. It is also
		// why a seat's subscription must never be created by joining —
		// see restAdmin.
		Type: pulsar.Shared,
		// The subscription already exists (attach ensures it at
		// earliest), so this only matters on the path where it somehow
		// does not — and there, Latest would silently discard everything
		// published before this consumer connected.
		SubscriptionInitialPosition: pulsar.SubscriptionPositionEarliest,
		SubscriptionMode:            pulsar.Durable,
		NackRedeliveryDelay:         q.cfg.nakDelay(),
		ReceiverQueueSize:           q.cfg.prefetchFor(maxBatch),
		DLQ: &pulsar.DLQPolicy{
			MaxDeliveries:   uint32(q.cfg.deliveryBudget()), //nolint:gosec // validated non-negative
			DeadLetterTopic: q.deadLetterTopic(topic, group),
			// Without an initial subscription the dead-letter topic has
			// none, and Pulsar deletes a message published where no
			// subscription covers it — destroying the poison message
			// this budget just spent ten deliveries establishing.
			InitialSubscriptionName: dlqRetainSubscription,
			ProducerOptions:         pulsar.ProducerOptions{DisableBatching: true},
		},
		// Acks go to the broker IMMEDIATELY rather than being grouped.
		//
		// The client's default buffers up to 1000 acks for up to 100 ms.
		// An ack sitting in that buffer when the process dies is a
		// COMPLETED turn that gets redelivered and run again — the
		// same-event-twice loop, bought for a throughput saving nothing
		// here needs: turns are serialized per seat and last minutes, so
		// the ack rate is a handful per minute per seat.
		AckGroupingOptions: &pulsar.AckGroupingOptions{MaxSize: 1},
	}
}

// closeConsumer closes and forgets this attachment's consumer.
//
// Closing is the free hand-back: unacked messages return to whoever attaches
// next, in order and at redeliveryCount 0 (measured, 9 ms). That is what
// makes a seat handoff cost nothing against the delivery budget.
func (a *attachment) closeConsumer() {
	a.consMu.Lock()
	cons := a.cons
	a.cons = nil
	a.consMu.Unlock()
	if cons != nil {
		cons.Close()
	}
}

// reopen recycles the consumer: close (handing back everything unacked and
// everything prefetched) and subscribe again to the same subscription.
//
// Re-joining is safe HERE and nowhere else: this node already owns the seat
// and is re-attaching its own consumer, whereas the hazard restAdmin exists
// for is joining a subscription a PEER is serving.
func (a *attachment) reopen() error {
	a.closeConsumer()
	cons, err := a.q.client.Subscribe(a.opts)
	if err != nil {
		return fmt.Errorf("re-open consumer %s/%s: %w", a.key.topic, a.key.group, err)
	}
	a.consMu.Lock()
	a.cons = cons
	a.consMu.Unlock()
	return nil
}

// prepare runs the per-cycle housekeeping every consume loop shares: honour a
// pending recycle, and report whether there is a consumer to fetch from.
func (a *attachment) prepare(ctx context.Context) (pulsar.Consumer, bool) {
	if a.recycle.Swap(false) {
		if err := a.reopen(); err != nil {
			a.log.Warn("consumer_reopen_failed", "error", err.Error())
			_ = sleep(ctx, consumeErrorBackoff)
			// Ask again next cycle: a seat whose consumer failed to come
			// back is a seat that reads nothing, and giving up here would
			// leave it that way for the life of the process.
			a.recycle.Store(true)
			return nil, false
		}
	}
	cons := a.current()
	return cons, cons != nil
}

// errNilHandler refuses a subscription with no handler. Accepting one would
// attach a consumer that swallows a seat's mail and panics on the first
// delivery — the seat reads as served and answers nothing.
func errNilHandler(topic, group string) error {
	return fmt.Errorf("%w: nil handler for %s/%s", ErrSubject, topic, group)
}

// Subscribe attaches a competing-consumer handler.
func (q *Queue) Subscribe(ctx context.Context, topic, group string, h queue.Handler) error {
	if h == nil {
		return errNilHandler(topic, group)
	}
	return q.attach(ctx, topic, group, 0, func(ctx context.Context, a *attachment) {
		for {
			if ctx.Err() != nil {
				return
			}
			cons, ok := a.prepare(ctx)
			if !ok {
				continue
			}
			if a.blocked() {
				// Nothing is fetched while blocked. What was already
				// prefetched stays in the client's local queue, which is
				// why Unquiesce recycles rather than merely resuming.
				if sleep(ctx, a.q.cfg.receiveWait()) != nil {
					return
				}
				continue
			}
			msg, err := receive(ctx, cons, a.q.cfg.receiveWait())
			if err != nil {
				if a.idleOrDone(ctx, err) {
					return
				}
				continue
			}
			a.dispatchOne(ctx, cons, msg, h)
		}
	})
}

// receive fetches one message, bounded so the loop re-checks its flags
// promptly. A timed-out receive consumes nothing: the message, if any, stays
// where it was.
func receive(ctx context.Context, cons pulsar.Consumer, wait time.Duration) (pulsar.Message, error) {
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return cons.Receive(rctx)
}

// idleOrDone classifies a receive failure, reporting whether the loop should
// exit.
//
// An idle poll is the overwhelming majority and logs nothing. A cancelled
// context means shutdown. Anything else is backed off: the loop is guarded so
// one bad message cannot end a seat's inbox, and a guard with no pause turns
// a recurring fault into a hot loop with a log line per turn of it.
func (a *attachment) idleOrDone(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	a.log.Warn("receive_failed", "error", err.Error())
	return sleep(ctx, consumeErrorBackoff) != nil
}

// dispatchOne runs a handler for one message and applies its outcome.
func (a *attachment) dispatchOne(ctx context.Context, cons pulsar.Consumer, msg pulsar.Message, h queue.Handler) {
	ev, ok := a.decode(cons, msg)
	if !ok {
		return
	}
	// A hold or a quiesce taken while this message was in flight must
	// still stop it. The engine pauses a seat's inbox precisely to keep a
	// fresh turn from starting, and a message already fetched is exactly
	// what would start one.
	if a.blocked() {
		a.handBack(cons, []delivery{{msg: msg, ev: ev}})
		return
	}
	a.q.beginHandler()
	res := runHandler(ctx, ev, h)
	a.q.endHandler()
	a.apply(cons, msg, ev, res)
}

// decode parses a message, acking-and-dropping a corrupt payload.
//
// A corrupt payload is never NAKed: redelivering it cannot make it parse, so
// a NAK just spends its delivery budget until it dead-letters, having blocked
// the subscription for every retry in between.
func (a *attachment) decode(cons pulsar.Consumer, msg pulsar.Message) (*events.Event, bool) {
	var ev events.Event
	if err := json.Unmarshal(msg.Payload(), &ev); err != nil {
		a.log.Error("event_decode_failed", "error", err.Error())
		if err := cons.Ack(msg); err != nil {
			a.log.Warn("ack_failed", "error", err.Error())
		}
		return nil, false
	}
	return &ev, true
}

// runHandler invokes a handler, converting a panic into a Nak so an
// unexpected failure redelivers instead of killing the loop.
func runHandler(ctx context.Context, ev *events.Event, h queue.Handler) (res queue.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = queue.Nak(fmt.Errorf("handler panicked: %v", r))
		}
	}()
	return h(ctx, ev)
}

func runBatchHandler(ctx context.Context, evs []*events.Event, h queue.BatchHandler) (res queue.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = queue.Nak(fmt.Errorf("batch handler panicked: %v", r))
		}
	}()
	return h(ctx, evs)
}

// --- outcomes -------------------------------------------------------------

// brokerAction is what an outcome does to a delivery.
type brokerAction int

const (
	// actionAck acknowledges: the work is done.
	actionAck brokerAction = iota
	// actionNak asks for redelivery and SPENDS one of the message's
	// deliveries against the dead-letter budget.
	actionNak
	// actionLeave leaves the message unacked and does nothing else. On
	// Pulsar this is the free hand-back: the consumer's close (or its
	// recycle) returns it to the next attacher in order, at
	// redeliveryCount 0.
	actionLeave
)

// actionFor maps a handler outcome onto the broker operation.
//
// The Defer row is the one that matters and the one this backend does
// differently from JetStream. Defer means "this process has lost the right to
// do this work" — a lease that moved, a node serving a stale config. Acking
// would claim work it will not perform; NAKing would spend dead-letter budget
// on a message nothing is wrong with, and a healthy event that changed hands
// often enough would eventually die having never failed. On Pulsar there is a
// third option and it costs nothing, so this backend takes it: leave it
// unacked and let the close hand it back
// (rewrite/decisions/102-jetstream-redelivery.md — the free handoff JetStream
// does not have, which is why Capabilities.FreeDeferral is a capability).
func actionFor(outcome queue.Outcome) brokerAction {
	switch outcome {
	case queue.OutcomeNak:
		return actionNak
	case queue.OutcomeDefer:
		return actionLeave
	default:
		return actionAck
	}
}

// handBackAction decides how work this consumer must NOT run is returned.
//
// The two mechanisms are not interchangeable, and picking the wrong one is
// silent either way:
//
//   - A quiesce, a detach or a shutdown pause all end with this consumer
//     CLOSING or being RECYCLED, and that is what returns unacked messages —
//     free, in order, at redeliveryCount 0. Leaving them is correct.
//   - A topic HOLD is released in place: the same consumer keeps serving the
//     seat and nothing ever closes it, so a message left unacked would never
//     come back at all. This client has no ack timeout to rescue it. Those
//     have to be NAKed, which costs one delivery and returns them after
//     nakDelay.
func handBackAction(closing bool) brokerAction {
	if closing {
		return actionLeave
	}
	return actionNak
}

// apply turns a handler outcome into broker operations.
func (a *attachment) apply(cons pulsar.Consumer, msg pulsar.Message, ev *events.Event, res queue.Result) {
	queue.LogResult(a.log, a.key.topic, a.key.group, ev, res)
	a.act(cons, actionFor(res.Outcome), msg)
	if res.Outcome == queue.OutcomeDefer {
		// Quiescing is the other half of a deferral: continuing to fetch
		// would hand this process more work it has equally lost the right
		// to do.
		a.quiesced.Store(true)
	}
}

func (a *attachment) act(cons pulsar.Consumer, action brokerAction, msg pulsar.Message) {
	switch action {
	case actionAck:
		if err := cons.Ack(msg); err != nil {
			a.log.Warn("ack_failed", "error", err.Error())
		}
	case actionNak:
		// Exhaustion is the client's DLQ router's job, not this loop's:
		// it reads the BROKER's redeliveryCount (authoritative across
		// every node that ever held the message) and routes to the
		// dead-letter topic before the message is handed over again.
		cons.Nack(msg)
	case actionLeave:
	}
}

// handBack returns deliveries this consumer must not run. See
// handBackAction for why the mechanism depends on what happens next.
func (a *attachment) handBack(cons pulsar.Consumer, items []delivery) {
	action := handBackAction(a.closing())
	for _, d := range items {
		a.act(cons, action, d.msg)
	}
}

// --- the four attachment verbs -------------------------------------------

// Quiesce stops taking new work while staying attached.
//
// The consumer is deliberately left OPEN: a quiesce lets a running handler
// finish, and that handler's ack has to reach a live consumer or a completed
// turn is redelivered and run twice. Reclaiming the prefetch is Unquiesce's
// job, which is where nothing is in flight any more.
func (q *Queue) Quiesce(_ context.Context, topic, group string) (bool, error) {
	atts := q.lookup(topic, group)
	for _, a := range atts {
		a.quiesced.Store(true)
	}
	return len(atts) > 0, nil
}

// Unquiesce resumes a quiesced attachment, RECYCLING its consumer.
//
// The recycle is not an optimisation, it is the whole operation. Quiescing
// stops the loop reading, but Pulsar keeps pushing up to ReceiverQueueSize
// messages into the client's local queue, and this client exposes no
// redeliver-unacknowledged command. Without the recycle those messages stay
// hostage to a consumer nothing is reading — and with no ack timeout in this
// client, hostage for as long as the process lives. Closing hands them back
// at redeliveryCount 0 and re-opening fetches them again immediately, so a
// two-second store blip costs a round trip rather than a seat's mail.
//
// Does NOT touch pause holds: a seat resuming from a stale-renew window may
// still be legitimately paused for a running sandbox, and clearing that would
// deliver into a suspended turn.
func (q *Queue) Unquiesce(_ context.Context, topic, group string) (bool, error) {
	var was bool
	for _, a := range q.lookup(topic, group) {
		if a.quiesced.Swap(false) {
			a.recycle.Store(true)
			was = true
		}
	}
	return was, nil
}

// Detach closes this process's consumers, leaving the durable subscription.
//
// Non-destructive: the subscription, its cursor and its retained mail all
// survive, so unacked messages return to whoever attaches next and messages
// published while nothing is attached are kept. That is what makes a seat
// handoff cheap and an unowned seat safe.
//
// Does NOT wait for a running handler. This is the fenced-release path: a
// node that has LOST a seat's lease must give it up now, because the
// successor already owns the seat and blocking the release until a
// multi-minute turn finishes would keep it unclaimable for exactly as long as
// the wrong node keeps working on it. The voluntary path composes the
// behaviour it wants out of verbs it already has: quiesce, wait for in-flight,
// then detach.
//
// Releases this pair's pause holds — a hold is state about THIS attachment,
// and one that outlived a detach would leave a node that re-attached later
// silently deaf.
func (q *Queue) Detach(_ context.Context, topic, group string) (bool, error) {
	return q.detach(topic, group, 0)
}

// detach removes an attachment, optionally waiting for its loop to exit.
//
// The wait exists for DeleteSubscription alone: the broker refuses to delete
// a subscription that still has a connected consumer, so decommissioning has
// to know the local consumer is really gone. Detach itself passes zero.
func (q *Queue) detach(topic, group string, grace time.Duration) (bool, error) {
	key := attachKey{topic, group}
	q.mu.Lock()
	atts := q.attachments[key]
	delete(q.attachments, key)
	delete(q.holds, key)
	q.mu.Unlock()

	for _, a := range atts {
		a.stop()
	}
	if grace > 0 {
		for _, a := range atts {
			if !a.wait(grace) {
				a.log.Warn("detach_wait_timed_out", "grace", grace.String())
			}
		}
	}
	return len(atts) > 0, nil
}

func (q *Queue) lookup(topic, group string) []*attachment {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.attachments[attachKey{topic, group}])
}

// --- in-flight accounting -------------------------------------------------

func (q *Queue) beginHandler() {
	q.inFlight.Add(1)
	q.inFlightN.Add(1)
}

func (q *Queue) endHandler() {
	q.inFlightN.Add(-1)
	q.inFlight.Done()
}
