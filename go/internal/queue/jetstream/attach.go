package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// fetchWait bounds one Fetch call. Short enough that quiesce, pause and
// shutdown take effect promptly; long enough that an idle seat is not
// spinning. Pull consumers make this a poll interval rather than a
// hostage window — nothing is pushed into a client-side queue, so an idle
// fetch holds no mail.
const fetchWait = time.Second

// drainWait is the fetch window used when collecting the tail of a batch
// that is already locally available.
const drainWait = 50 * time.Millisecond

// attachment is this process's consumer for one (topic, group) pair.
type attachment struct {
	q      *Queue
	topic  string
	group  string
	cons   jetstream.Consumer
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}

	// quiesced stops the loop taking NEW work while staying attached. A
	// running handler finishes; nothing new is fetched. Its inverse
	// (Unquiesce) is required, not optional: a node whose coordination
	// store blipped quiesces and must be able to come back, or it holds
	// the seat, stays attached, and consumes nothing for the rest of its
	// life — owned, attached and silently deaf.
	quiesced atomic.Bool

	// paused is the shutdown-wide delivery pause.
	paused atomic.Bool

	// holds are reason-scoped pause holds. The subscription stays paused
	// while ANY reason holds it: two independent subsystems gate one
	// inbox — the sandbox busy gate and the config-divergence shed — and
	// with one flat flag the sandbox resuming its own run would un-gate a
	// node serving a stale company.
	holdMu sync.Mutex
	holds  map[string]struct{}
}

func (a *attachment) setQuiesced(v bool) { a.quiesced.Store(v) }
func (a *attachment) setPaused(v bool)   { a.paused.Store(v) }

// blocked reports whether the loop may take new work.
func (a *attachment) blocked() bool {
	if a.quiesced.Load() || a.paused.Load() {
		return true
	}
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	return len(a.holds) > 0
}

func (a *attachment) hold(reason string) {
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	if a.holds == nil {
		a.holds = map[string]struct{}{}
	}
	a.holds[reason] = struct{}{}
}

func (a *attachment) release(reason string) {
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	delete(a.holds, reason)
}

func (a *attachment) stop() {
	if a.cancel != nil {
		a.cancel()
	}
	<-a.done
}

// attach creates or reuses the durable consumer and starts a loop.
func (q *Queue) attach(ctx context.Context, topic, group string, loop func(context.Context, *attachment)) error {
	if topic == "" || group == "" {
		return fmt.Errorf("%w: topic and group are required", ErrSubject)
	}
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		return err
	}
	stream, err := streamForSubject(topic)
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
	if _, exists := q.attachments[key]; exists {
		q.mu.Unlock()
		return fmt.Errorf("already attached to %s/%s", topic, group)
	}
	// A process-wide delivery pause must apply to an attachment created
	// during a drain, or a late subscribe would start handlers the drain
	// is waiting to finish.
	paused := q.paused
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a := &attachment{
		q: q, topic: topic, group: group, cons: cons,
		log:    q.log.With("topic", topic, "group", group),
		cancel: cancel, done: make(chan struct{}),
	}
	a.paused.Store(paused)
	q.attachments[key] = a
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
				if sleep(ctx, fetchWait) != nil {
					return
				}
				continue
			}
			msgs, err := a.cons.Fetch(1, jetstream.FetchMaxWait(fetchWait))
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
	_ = sleep(ctx, fetchWait)
}

// dispatchOne runs a handler for one message and applies its outcome.
func (a *attachment) dispatchOne(ctx context.Context, msg jetstream.Msg, h queue.Handler) {
	ev, ok := a.decode(msg)
	if !ok {
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
	queue.LogResult(a.log, a.topic, a.group, ev, res)
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
		// handoffs (see decisions/102). Never republish instead: a
		// republished event is a NEW message with a new sequence, and
		// the ledger's idempotency plus the batch layer's aging both
		// key on the identity a Nak preserves.
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
// this is where the DLQ subject is known and where the message body is in
// hand. MaxDeliver stays configured as a backstop so a bug here cannot
// produce an infinite loop.
func (a *attachment) nakOrDeadLetter(ctx context.Context, msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err == nil && md.NumDelivered >= maxDeliver {
		a.log.Error("dead_lettered", "deliveries", md.NumDelivered)
		a.q.deadLetter(ctx, a.topic, a.group, msg.Data())
		// Term, not Ack: the message is being discarded from this
		// subscription deliberately, and Term says so to anyone reading
		// consumer state.
		if err := msg.Term(); err != nil {
			a.log.Warn("term_failed", "error", err.Error())
		}
		return
	}
	// A short delay, not zero: an immediately-redelivered failing message
	// spins the loop at full speed against whatever is failing.
	if err := msg.NakWithDelay(time.Second); err != nil {
		a.log.Warn("nak_failed", "error", err.Error())
	}
}

// --- the four attachment verbs -------------------------------------------

// Quiesce stops taking new work while staying attached.
func (q *Queue) Quiesce(_ context.Context, topic, group string) (bool, error) {
	a := q.lookup(topic, group)
	if a == nil {
		return false, nil
	}
	a.setQuiesced(true)
	return true, nil
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
	a := q.lookup(topic, group)
	if a == nil {
		return false, nil
	}
	was := a.quiesced.Swap(false)
	return was, nil
}

// Detach closes this process's consumer, leaving the durable subscription.
//
// Non-destructive: the consumer, its cursor and its retained mail all
// survive, so unacked messages return to whoever attaches next and messages
// published while nothing is attached are kept. That is what makes a seat
// handoff cheap and an unowned seat safe.
//
// Releases this attachment's pause holds — a hold is state about THIS
// attachment, and one that outlived a detach would leave a node that
// re-attached later silently deaf.
func (q *Queue) Detach(_ context.Context, topic, group string) (bool, error) {
	key := attachKey{topic, group}
	q.mu.Lock()
	a, ok := q.attachments[key]
	if ok {
		delete(q.attachments, key)
	}
	q.mu.Unlock()
	if !ok {
		return false, nil
	}
	a.stop()
	return true, nil
}

func (q *Queue) lookup(topic, group string) *attachment {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.attachments[attachKey{topic, group}]
}

// PauseTopic takes a named hold on one subscription.
func (q *Queue) PauseTopic(_ context.Context, topic, group, reason string) error {
	if reason == "" {
		reason = "default"
	}
	if a := q.lookup(topic, group); a != nil {
		a.hold(reason)
	}
	return nil
}

// ResumeTopic releases one reason's hold, flushing when none remain.
func (q *Queue) ResumeTopic(_ context.Context, topic, group, reason string) error {
	if reason == "" {
		reason = "default"
	}
	if a := q.lookup(topic, group); a != nil {
		a.release(reason)
	}
	return nil
}

// --- in-flight accounting -------------------------------------------------

type atomicCounter struct{ n atomic.Int64 }

func (c *atomicCounter) add(d int64) { c.n.Add(d) }
func (c *atomicCounter) load() int   { return int(c.n.Load()) }

func (q *Queue) beginHandler() {
	q.inFlight.Add(1)
	q.inFlightN.add(1)
}

func (q *Queue) endHandler() {
	q.inFlightN.add(-1)
	q.inFlight.Done()
}

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
