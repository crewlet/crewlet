package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// defaultFetchWait bounds one Fetch call. Short enough that quiesce, pause
// and shutdown take effect promptly; long enough that an idle seat is not
// spinning. Pull consumers make this a poll interval rather than a hostage
// window — nothing is pushed into a client-side queue, so an idle fetch
// holds no mail.
const defaultFetchWait = time.Second

// drainWait is the fetch window used when collecting the tail of a batch
// that is already locally available.
const drainWait = 50 * time.Millisecond

// defaultNakDelay spaces out the redelivery of a FAILING message. Not zero:
// an immediately-redelivered failure spins the loop at full speed against
// whatever is broken. A deferral takes a different path and is immediate.
const defaultNakDelay = time.Second

func (q *Queue) fetchWait() time.Duration {
	if q.cfg.FetchWait > 0 {
		return q.cfg.FetchWait
	}
	return defaultFetchWait
}

func (q *Queue) nakDelay() time.Duration {
	if q.cfg.NakDelay > 0 {
		return q.cfg.NakDelay
	}
	return defaultNakDelay
}

// attachment is ONE consumer this process runs for a (topic, group) pair.
//
// A process may run several for one pair — the contract's "close this
// process's consumer(s)" is plural on purpose. A seat inbox has exactly one,
// because there membership IS ownership, but a fleet-wide worker group may
// legitimately run more than one member on a node.
type attachment struct {
	q      *Queue
	key    attachKey
	cons   jetstream.Consumer
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}

	// quiesced stops this consumer taking NEW work while staying
	// attached. A running handler finishes; nothing new is fetched. Its
	// inverse is required, not optional: a node whose coordination store
	// blipped quiesces and must be able to come back, or it holds the
	// seat, stays attached, and consumes nothing for the rest of its life
	// — owned, attached and silently deaf.
	quiesced atomic.Bool

	// paused is the process-wide shutdown delivery pause.
	paused atomic.Bool

	// detached is set the moment Detach or Stop is called. It is what
	// makes the contract's two halves both true at once: Detach returns
	// WITHOUT joining a running handler, and yet no NEW handler starts
	// after it returns. The consume loop may still be parked in a fetch
	// when Detach lands, so cancelling the context alone would let one
	// more message through — the delivery a successor is already entitled
	// to.
	detached atomic.Bool
}

func (a *attachment) setQuiesced(v bool) { a.quiesced.Store(v) }
func (a *attachment) setPaused(v bool)   { a.paused.Store(v) }

// blocked reports whether this consumer may take new work.
func (a *attachment) blocked() bool {
	return a.detached.Load() || a.quiesced.Load() || a.paused.Load() || a.q.held(a.key)
}

// close stops this consumer taking new work and returns immediately.
//
// It deliberately does NOT join a running handler: Detach is the
// fenced-release path, and a node that has lost a seat must give it up now
// rather than when the turn it should not be running finally ends.
func (a *attachment) close() {
	a.detached.Store(true)
	if a.cancel != nil {
		a.cancel()
	}
}

// wait blocks until the consume loop has exited, or the deadline passes.
//
// Used by Stop, where the caller has already drained in-flight handlers and
// wants the goroutines actually gone. Bounded, because a handler that never
// returns must not turn shutdown into a hang — the process is leaving
// anyway.
func (a *attachment) wait(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.done:
	case <-t.C:
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
// busy pauses first and attaches after. A hold stored on the attachment
// would simply be dropped in that window, and the seat would take exactly
// the work it was told not to.

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
	// A hold on a closed client gates nothing: Stop dropped every
	// attachment and cleared the hold map on the way out. Returning nil
	// here told a shutdown path it had gated a subscription that no longer
	// existed. See queue.EventQueue's Stop.
	if q.closed {
		return ErrClosed
	}
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
	if q.closed {
		return ErrClosed
	}
	delete(q.holds[key], reason)
	if len(q.holds[key]) == 0 {
		delete(q.holds, key)
	}
	return nil
}

// PauseHolds reports the reasons currently holding a subscription paused.
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

// attach creates or reuses the durable consumer and starts a loop.
func (q *Queue) attach(ctx context.Context, topic, group string, loop func(context.Context, *attachment)) error {
	if topic == "" || group == "" {
		return fmt.Errorf("%w: topic and group are required", ErrSubject)
	}
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		return err
	}
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return err
	}
	cons, err := q.js.Consumer(ctx, stream, consumerName(topic, group))
	if err != nil {
		return fmt.Errorf("open consumer %s/%s: %w", topic, group, err)
	}

	key := attachKey{topic, group}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrClosed
	}
	// A process-wide delivery pause must apply to an attachment created
	// during a drain, or a late subscribe would start handlers the drain
	// is waiting to finish.
	paused := q.paused
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a := &attachment{
		q: q, key: key, cons: cons,
		log:    q.log.With("topic", topic, "group", group),
		cancel: cancel, done: make(chan struct{}),
	}
	a.paused.Store(paused)
	q.attachments[key] = append(q.attachments[key], a)
	q.mu.Unlock()

	go func() {
		defer close(a.done)
		loop(loopCtx, a)
	}()
	return nil
}

// Subscribe attaches a competing-consumer handler.
func (q *Queue) Subscribe(ctx context.Context, topic, group string, h queue.Handler) error {
	return q.attach(ctx, topic, group, func(ctx context.Context, a *attachment) {
		for {
			if ctx.Err() != nil {
				return
			}
			if a.blocked() {
				// Nothing is fetched while blocked, so nothing is held
				// hostage — the pull model's quiet advantage over a
				// pushed prefetch.
				if sleep(ctx, a.q.fetchWait()) != nil {
					return
				}
				continue
			}
			msgs, err := a.cons.Fetch(1, jetstream.FetchMaxWait(a.q.fetchWait()))
			if err != nil {
				a.logFetchErr(ctx, err)
				continue
			}
			for msg := range msgs.Messages() {
				a.dispatchOne(ctx, msg, h)
			}
		}
	})
}

func (a *attachment) logFetchErr(ctx context.Context, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	a.log.Warn("fetch_failed", "error", err.Error())
	_ = sleep(ctx, a.q.fetchWait())
}

// dispatchOne runs a handler for one message and applies its outcome.
func (a *attachment) dispatchOne(ctx context.Context, msg jetstream.Msg, h queue.Handler) {
	ev, ok := a.decode(msg)
	if !ok {
		return
	}
	// A hold taken while this message was in flight must still stop it.
	// The engine pauses a seat's inbox precisely to keep a fresh turn from
	// starting, and a message already fetched is exactly what would start
	// one. Returning it costs nothing: this consumer picks it up again
	// when the hold lifts, or a successor does.
	if a.blocked() {
		if err := msg.Nak(); err != nil {
			a.log.Warn("hold_nak_failed", "error", err.Error())
		}
		return
	}
	a.q.beginHandler()
	res := runHandler(ctx, ev, h)
	a.q.endHandler()
	a.apply(ctx, msg, ev, res)
}

// decode parses a message, acking-and-dropping a corrupt payload.
//
// A corrupt payload is never NAKed: redelivering it cannot make it parse, so
// a NAK just spends its delivery budget until it dead-letters, having
// blocked the subscription for every retry in between.
func (a *attachment) decode(msg jetstream.Msg) (*events.Event, bool) {
	var ev events.Event
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		a.log.Error("event_decode_failed", "error", err.Error())
		_ = msg.Ack()
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

// apply turns a handler outcome into broker operations.
func (a *attachment) apply(ctx context.Context, msg jetstream.Msg, ev *events.Event, res queue.Result) {
	queue.LogResult(a.log, a.key.topic, a.key.group, ev, res)
	switch res.Outcome {
	case queue.OutcomeAck:
		if err := msg.Ack(); err != nil {
			a.log.Warn("ack_failed", "error", err.Error())
		}

	case queue.OutcomeDefer:
		// Defer means: this process has lost the right to do this work.
		// Nak returns it immediately — measured at about a millisecond,
		// where letting the ack timer expire would park a seat's mail
		// for the whole AckWait window on every lease movement.
		//
		// It costs one delivery count, which is why the budget covers
		// handoffs (decisions/102). Never republish instead: a
		// republished event is a NEW message, and the completion
		// ledger's idempotency plus the batch layer's aging both key on
		// the identity a Nak preserves.
		if err := msg.Nak(); err != nil {
			a.log.Warn("defer_nak_failed", "error", err.Error())
		}
		// Quiescing is the other half of a deferral: continuing to fetch
		// would hand this process more work it has equally lost the
		// right to do.
		a.setQuiesced(true)

	case queue.OutcomeNak:
		a.nakOrDeadLetter(ctx, msg)
	}
}

// nakOrDeadLetter redelivers a failed message until its budget is spent,
// then dead-letters it.
//
// The decision lives here rather than relying on MaxDeliver alone because
// this is where the dead-letter subject is known and the message body is in
// hand. MaxDeliver stays configured as a backstop so a bug here cannot
// produce an infinite loop.
func (a *attachment) nakOrDeadLetter(ctx context.Context, msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err == nil && md.NumDelivered >= uint64(budgetFor(a.q.cfg)) {
		a.log.Error("dead_lettered", "deliveries", md.NumDelivered)
		a.q.deadLetter(ctx, a.key.topic, a.key.group, msg.Data())
		// Term, not Ack: the message is being discarded from this
		// subscription deliberately, and Term says so to anyone reading
		// consumer state.
		if err := msg.Term(); err != nil {
			a.log.Warn("term_failed", "error", err.Error())
		}
		return
	}
	if err := msg.NakWithDelay(a.q.nakDelay()); err != nil {
		a.log.Warn("nak_failed", "error", err.Error())
	}
}

// --- the four attachment verbs -------------------------------------------

// Quiesce stops taking new work while staying attached.
func (q *Queue) Quiesce(_ context.Context, topic, group string) (bool, error) {
	if q.isClosed() {
		return false, ErrClosed
	}
	atts := q.lookup(topic, group)
	for _, a := range atts {
		a.setQuiesced(true)
	}
	return len(atts) > 0, nil
}

// Unquiesce resumes a quiesced attachment.
//
// Does NOT touch pause holds: a seat resuming from a stale-renew window may
// still be legitimately paused for a running sandbox, and clearing that
// would deliver into a suspended turn.
//
// Unlike the Pulsar backend this needs no prefetch reclamation — pull
// consumers never pushed anything into a client-side queue, so resuming is
// simply fetching again.
func (q *Queue) Unquiesce(_ context.Context, topic, group string) (bool, error) {
	if q.isClosed() {
		return false, ErrClosed
	}
	var was bool
	for _, a := range q.lookup(topic, group) {
		if a.quiesced.Swap(false) {
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
// Releases this pair's pause holds — a hold is state about THIS attachment,
// and one that outlived a detach would leave a node that re-attached later
// silently deaf.
func (q *Queue) Detach(_ context.Context, topic, group string) (bool, error) {
	key := attachKey{topic, group}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false, ErrClosed
	}
	atts := q.attachments[key]
	delete(q.attachments, key)
	delete(q.holds, key)
	q.mu.Unlock()

	for _, a := range atts {
		a.close()
	}
	return len(atts) > 0, nil
}

// isClosed reports whether Stop has run, for the verbs that must refuse
// afterwards rather than report success against a connection that is gone.
func (q *Queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *Queue) lookup(topic, group string) []*attachment {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.attachments[attachKey{topic, group}])
}

// --- in-flight accounting -------------------------------------------------

func (q *Queue) beginHandler() { q.inFlight.Begin() }
func (q *Queue) endHandler()   { q.inFlight.End() }

// sleep waits for d or until ctx is done, reporting the context error so a
// caller can return promptly on shutdown.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
