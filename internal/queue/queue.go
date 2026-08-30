// Package queue defines the EventQueue contract — the transport every
// inter-component message in Crewlet travels through.
//
// One interface, two backends (an in-memory twin, and NATS JetStream —
// embedded by default, external when configured), both certified by ONE
// conformance suite. A
// backend that the suite has not certified does not exist as far as the
// engine is concerned, and nothing above this package may branch on which
// backend is running.
//
// The rationale a reader should not have to re-derive:
//
//   - A durable subscription IS a seat's mailbox. It exists without a
//     consumer, retains what is published while nothing is attached, and
//     replays in order when someone attaches. Publishing to a topic with no
//     subscription drops the event silently — hence EnsureSubscription.
//   - Handlers have three outcomes, not two. Ack, Nak, and Defer (leave it
//     unacked, stop consuming) — see Result.
//   - Attachment has four verbs with different destructiveness: Quiesce,
//     Unquiesce, Detach, DeleteSubscription.
package queue

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("queue.contract")

// MaxPayloadBytes is the largest single event any backend must carry.
//
// It is a CONTRACT number rather than a backend's own, because both ends need
// it and they sit in different packages: the broker is configured to accept
// this much (see internal/queue/jetstream, which sets the embedded server's
// max_payload from it), and every producer that buffers caller-supplied bytes
// has to refuse anything that would not fit BEFORE it accepts the work — see
// internal/api/webhooks, whose body cap is derived from this.
//
// Written down because the two drifted, and the failure had no floor: the
// webhook edge accepted a 25 MiB delivery, verified its signature, claimed it,
// and then could not publish it through a broker whose default payload limit
// is 1 MiB. The delivery was refused with a 503, so the provider retried, and
// every retry failed the same way forever. Nothing in that loop is a
// transient, and nothing logged a size.
//
// 8 MiB is nats-server's own MAX_PAYLOAD_MAX_SIZE — the threshold above which
// it warns that a payload is too large to be a good idea (server/const.go).
// Sitting AT that boundary takes eight times the default without arguing with
// the broker about what it was built for: a message is buffered whole, in the
// server and again in every client that receives it, so this is memory per
// in-flight event and not a disk number.
const MaxPayloadBytes = 8 << 20

// MaxLingerSeconds is the hard ceiling on a batch linger window.
//
// The linger counts against the broker ack-timeout budget that bounds a
// drained message's unacked lifetime: collection plus ONE handler run must
// fit inside it. Config validation mirrors this cap, but programmatic
// construction bypasses that, so the invariant is enforced here regardless
// of who set the field.
const MaxLingerSeconds = 60.0

// Outcome is what a handler decided about its delivery.
type Outcome int

const (
	// OutcomeAck acknowledges the delivery. It is the zero value, so a
	// handler returning Result{} acknowledges: the quiet path is the safe
	// one.
	OutcomeAck Outcome = iota

	// OutcomeNak negatively acknowledges: the handler failed and the
	// message should be redelivered, spending one unit of its
	// dead-letter budget.
	OutcomeNak

	// OutcomeDefer leaves the delivery unacked AND quiesces the
	// attachment. It is the outcome for work this process has lost the
	// right to do — a seat whose lease moved, a node serving a stale
	// config. Acking would claim work it will not perform; NAKing would
	// spend dead-letter budget on a message nothing is wrong with, and a
	// healthy event eventually dies after enough handoffs.
	//
	// When the consumer closes, the broker returns the message to
	// whoever attaches next, in order and at zero accrued redeliveries.
	// Never substitute a republish: that sends the event to the topic
	// tail while its prefetched siblings replay from the head,
	// reordering the conversation.
	OutcomeDefer
)

// String renders an outcome for logs.
func (o Outcome) String() string {
	switch o {
	case OutcomeNak:
		return "nak"
	case OutcomeDefer:
		return "defer"
	default:
		return "ack"
	}
}

// Result is a handler's decision about its delivery.
type Result struct {
	Outcome Outcome
	// Reason is logged. For a deferral it is the deferral reason, which
	// is the only record of why a message went back to the broker.
	Reason string
	// Err is the failure that caused a Nak.
	Err error
}

// Ack acknowledges the delivery.
func Ack() Result { return Result{} }

// Nak negatively acknowledges, requesting redelivery.
func Nak(err error) Result { return Result{Outcome: OutcomeNak, Err: err} }

// Defer leaves the delivery unacked and quiesces the attachment.
func Defer(reason string) Result { return Result{Outcome: OutcomeDefer, Reason: reason} }

// Handler processes one event.
type Handler func(ctx context.Context, ev *events.Event) Result

// BatchHandler processes one conversation partition of a drained batch.
type BatchHandler func(ctx context.Context, evs []*events.Event) Result

// ErrNotLive reports that a verb reached a queue that is not live — never
// started, or stopped.
//
// THE ONE SENTINEL THE LAYERS ABOVE MAY BRANCH ON for this, because they must
// not branch on which backend is running: each backend keeps its own error
// (jetstream.ErrClosed, memory.ErrNotStarted) and wraps this, so
// errors.Is(err, queue.ErrNotLive) is the same question everywhere.
//
// It exists because "the queue is down" is not a failure for every caller. A
// seat release detaches the mailbox and the seat host KEEPS THE LEASE if that
// detach cannot be proven — a seat this process may still be consuming must
// not go to a peer. But a queue that is not live consumes nothing, so a
// refusal there is proof of teardown rather than a failure to prove it, and
// reading it as a failure would strand the seat for a full TTL on the one
// path where the node is trying to hand it back.
var ErrNotLive = errors.New("queue: not live")

// StreamHandler receives broadcast events. Stream delivery is best-effort:
// there is no ack, and a handler's failure is logged, never redelivered.
type StreamHandler func(ctx context.Context, topic string, ev *events.Event)

// Unsubscribe cancels a stream subscription and releases backend resources.
type Unsubscribe func(ctx context.Context) error

// BatchKeyFunc derives the conversation key an event partitions under.
type BatchKeyFunc func(ev *events.Event) string

// PublishListener is invoked inline on every publish. Listeners run in the
// publishing goroutine and must not block for long; their failures are
// logged and never prevent the publish.
type PublishListener func(ctx context.Context, topic string, ev *events.Event)

// Publisher is the write half of the queue.
//
// Declared here rather than in each consumer because several subsystems
// publish without ever subscribing, and a per-package copy of a one-method
// interface is one more place for the signature to drift from the thing it
// describes. EventQueue satisfies it, asserted below.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// EventQueue is the transport contract.
type EventQueue interface {
	// Publish sends an event to a topic. It must not return until the
	// event is persisted: callers rely on "published means durable".
	Publish(ctx context.Context, topic string, ev *events.Event) error

	// Subscribe attaches a competing-consumer handler to topic/group.
	// Exactly one member of a group receives each message.
	Subscribe(ctx context.Context, topic, group string, h Handler) error

	// SubscribeBatch attaches with batched, key-partitioned delivery:
	// drain what is locally available (plus a linger window), partition
	// by key preserving arrival order, dispatch one handler call per
	// partition oldest-conversation-first, and ack per partition. A
	// failing partition never blocks or replays a different one.
	//
	// opts is read live on every cycle, so a config reload changes
	// linger and batch size with no re-subscription.
	SubscribeBatch(ctx context.Context, topic, group string, h BatchHandler, key BatchKeyFunc, opts *BatchOptions) error

	// Quiesce stops taking NEW work on topic/group while staying
	// attached; a running handler finishes. Reports whether an
	// attachment existed. Idempotent.
	Quiesce(ctx context.Context, topic, group string) (bool, error)

	// Unquiesce resumes a quiesced attachment, reporting whether it was
	// quiesced. Required, not optional: a node whose lease store blipped
	// quiesces and must be able to come back, or it holds the seat,
	// stays attached, and consumes nothing for the rest of its life.
	//
	// Does NOT touch pause holds — a seat resuming from a stale-renew
	// window may still be legitimately paused for a running sandbox.
	Unquiesce(ctx context.Context, topic, group string) (bool, error)

	// Detach closes this process's consumers, leaving the subscription.
	// Non-destructive: cursor and retained mail survive, so unacked
	// messages return to the next attacher in order with no accrued
	// redeliveries. Releases this attachment's pause holds — a hold that
	// outlived a detach would leave a re-attaching node silently deaf.
	//
	// Detach does NOT wait for a running handler. It stops the
	// subscription taking new work and returns; a handler already
	// in-flight runs to completion and its outcome still applies.
	//
	// This is not a performance choice, it is the fenced-release path.
	// A node that has LOST a seat's lease must detach FIRST and abandon
	// whatever is in flight — the successor already owns the seat, and
	// blocking the release until a multi-minute turn finishes would keep
	// the seat unclaimable for exactly as long as the wrong node keeps
	// working on it. The VOLUNTARY path gets the other behaviour by
	// composing verbs it already has: quiesce, wait for in-flight, then
	// detach.
	Detach(ctx context.Context, topic, group string) (bool, error)

	// EnsureSubscription creates the durable subscription if absent,
	// with NO consumer attached, positioned at the earliest message.
	// Reports whether this call created it; creating an existing
	// subscription is success.
	EnsureSubscription(ctx context.Context, topic, group string) (bool, error)

	// DeleteSubscription destroys the subscription and its retained
	// mail. Must not require a local consumer: decommissioning a role
	// cannot depend on which node happened to run the seat.
	DeleteSubscription(ctx context.Context, topic, group string) (bool, error)

	// SubscribeStream creates an ephemeral per-caller broadcast
	// subscription — every subscriber receives every matching event.
	// topicPattern supports `*` (one segment) and `>` (trailing
	// segments).
	SubscribeStream(ctx context.Context, topicPattern string, h StreamHandler) (Unsubscribe, error)

	// AddPublishListener registers a listener called inline on every
	// publish.
	AddPublishListener(l PublishListener)

	// Start connects the backend and begins consuming.
	//
	// WHETHER A BACKEND REQUIRES IT IS ITS OWN ANSWER, and both shipped
	// answers are legitimate: on JetStream, Open establishes the
	// connection and the streams, so Start is a no-op and a publish
	// before it works; on the in-memory twin, Start is what makes the
	// client live.
	//
	// What a backend may NOT do is answer DIFFERENTLY PER VERB. Before
	// Start, all eleven of the publish, subscription and attachment verbs
	// must give the same answer — Publish, Subscribe, SubscribeBatch,
	// Quiesce, Unquiesce, Detach, EnsureSubscription, DeleteSubscription,
	// SubscribeStream, PauseTopic and ResumeTopic. Either they all refuse,
	// or none of them require it. Capabilities.RequiresStart says which,
	// and queuetest sends all eleven to hold a backend to one answer.
	//
	// A caller cannot reason about a lifecycle whose rules change per
	// method, and this is not hypothetical: a twin that refused Publish
	// and Subscribe while PauseTopic still took a hold left that hold to
	// survive into the next life, and the restarted queue reported itself
	// running and was silently deaf.
	//
	// AFTER Stop the answer is not a capability at all — see Stop, which
	// is where the asymmetry is written up.
	//
	// The DRAIN protocol is exempt along with Start and Stop, because it
	// exists to run around a stop rather than in spite of one:
	// PauseDelivery, WaitForHandlers and InFlightCount answer on a
	// stopped queue, and must — a drain that could not report its own
	// in-flight count once the queue was down would have no way to say
	// whether it finished. Backend and AddPublishListener return no error
	// and so have nothing to refuse with.
	Start(ctx context.Context) error

	// InFlightCount reports handler invocations currently mid-flight —
	// the number an operator watches converge to zero during a drain.
	InFlightCount() int

	// Backend is a stable lowercase backend name for operator display.
	// Nothing may branch behaviour on it.
	Backend() string

	// PauseDelivery stops dispatching new events to handlers while
	// leaving Publish working, so in-flight handlers can emit terminal
	// events. One-way: once paused, the engine is shutting down.
	PauseDelivery(ctx context.Context) error

	// PauseTopic pauses ONE subscription's delivery under a named
	// reason. Holds are reason-scoped and keyed by the (topic, group)
	// PAIR: two independent subsystems gate the same inbox — the
	// sandbox busy gate and the config-divergence shed — and with one
	// flat set the sandbox resuming its own run would un-gate a node
	// serving a stale company.
	PauseTopic(ctx context.Context, topic, group, reason string) error

	// ResumeTopic releases one reason's hold, flushing when none remain.
	ResumeTopic(ctx context.Context, topic, group, reason string) error

	// WaitForHandlers waits for in-flight handlers, returning how many
	// were still running when the wait ended. Zero means a clean drain;
	// non-zero means the timeout expired, which is not an error — the
	// caller owns any "too long" policy. Always PauseDelivery first.
	WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error)

	// Stop closes the connection.
	//
	// AFTERWARDS EVERY ONE OF THE ELEVEN VERBS REFUSES. A requirement
	// rather than a capability, and the asymmetry with the pre-Start rule
	// is the point: "not connected yet" is a state a backend may
	// legitimately not have — JetStream's Open connects, so there is
	// nothing before Start — while "closed" is a state every backend
	// genuinely reaches, and a verb that mutates through a closed client
	// is wrong on all of them.
	//
	// It is wrong in two different ways, and both were measured. On the
	// twin, PauseTopic after Stop took a hold that outlived the client and
	// left the next life silently deaf. On JetStream, Quiesce, Unquiesce,
	// Detach, PauseTopic and ResumeTopic all returned success on a closed
	// connection while Publish and Subscribe returned "nats: connection
	// closed" — so a shutdown path could believe it had gated a
	// subscription that no longer existed.
	//
	// Whether Start can revive a stopped queue is a separate question the
	// contract does not answer; see Capabilities.Restartable.
	Stop(ctx context.Context) error
}

// --- batch options --------------------------------------------------------

// BatchOptions carries the live-mutable knobs for batched delivery.
//
// The consume loop re-reads these at the start of every collection cycle, so
// a hot config reload takes effect on the next batch with no
// re-subscription. Mutable and read concurrently is a data race unless it is
// guarded, so it is: this is safe to write from another goroutine while a loop
// is reading it.
type BatchOptions struct {
	mu            sync.RWMutex
	lingerSeconds float64
	maxBatch      int
}

// NewBatchOptions builds options with the given window and cap, clamped on
// read through the Effective accessors.
func NewBatchOptions(lingerSeconds float64, maxBatch int) *BatchOptions {
	return &BatchOptions{lingerSeconds: lingerSeconds, maxBatch: maxBatch}
}

// DefaultBatchOptions is the engine's default: no linger (still drains
// everything already locally available, so backlog accumulated while a
// previous handler ran coalesces at zero added latency) and 20 events.
func DefaultBatchOptions() *BatchOptions { return NewBatchOptions(0, 20) }

// Set replaces both knobs atomically.
func (o *BatchOptions) Set(lingerSeconds float64, maxBatch int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lingerSeconds, o.maxBatch = lingerSeconds, maxBatch
}

// EffectiveLinger is the linger window clamped to [0, MaxLingerSeconds].
//
// The window is fixed — measured from the first message of a drain, not
// sliding — so a steady trickle cannot delay dispatch unboundedly.
func (o *BatchOptions) EffectiveLinger() time.Duration {
	o.mu.RLock()
	defer o.mu.RUnlock()
	v := o.lingerSeconds
	if v < 0 {
		v = 0
	}
	if v > MaxLingerSeconds {
		v = MaxLingerSeconds
	}
	return time.Duration(v * float64(time.Second))
}

// EffectiveMaxBatch is the per-cycle cap, at least 1. A pathological backlog
// is delivered as successive capped batches rather than one unbounded one.
func (o *BatchOptions) EffectiveMaxBatch() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.maxBatch < 1 {
		return 1
	}
	return o.maxBatch
}

// --- partitioning and dispatch ordering -----------------------------------

// Partition is one conversation's slice of a drained batch.
type Partition[T any] struct {
	Key   string
	Items []T
}

// PartitionByKey groups items by key, preserving arrival order both between
// partitions (first-arrival order) and within each.
//
// DISPATCH order is a separate policy — see OrderPartitionsOldestFirst. A
// key function that panics falls back to a unique per-event key, because key
// derivation must never block delivery.
func PartitionByKey[T any](items []T, key BatchKeyFunc, eventOf func(T) *events.Event) []Partition[T] {
	index := map[string]int{}
	var out []Partition[T]
	for _, item := range items {
		ev := eventOf(item)
		k := safeKey(ev, key)
		if i, ok := index[k]; ok {
			out[i].Items = append(out[i].Items, item)
			continue
		}
		index[k] = len(out)
		out = append(out, Partition[T]{Key: k, Items: []T{item}})
	}
	return out
}

func safeKey(ev *events.Event, key BatchKeyFunc) (k string) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("batch_key_failed", "event_type", eventType(ev), "panic", r)
			k = fallbackKey(ev)
		}
	}()
	if key == nil {
		return fallbackKey(ev)
	}
	if k = key(ev); k == "" {
		return fallbackKey(ev)
	}
	return k
}

func fallbackKey(ev *events.Event) string {
	if ev == nil {
		return "event:"
	}
	return "event:" + ev.ID.String()
}

func eventType(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	return ev.Type
}

// OrderForDispatch establishes BOTH levels of ordering a drained batch
// needs, and is the one place either is decided.
//
// Between partitions: oldest constituent event first. Receive order alone
// starves a quiet conversation behind a hot one under deferral — the quiet
// conversation's requeued copies re-enter the topic AFTER whatever arrived
// during the hot conversation's turn, so receive-ordered dispatch picks the
// hot one on every drain. Timestamps carry the aging signal, so the
// conversation that has waited longest dispatches first.
//
// Within a partition: event timestamp, not delivery order. This is what
// makes a conversation read correctly regardless of how a broker interleaves
// redeliveries with fresh arrivals — measured, JetStream returns a
// redelivered message BEHIND never-delivered ones, where the in-memory twin
// replays it from the head. Relying
// on the timestamps the engine already trusts, rather than on one broker's
// replay semantics, removes a correctness dependency that would otherwise
// have to be re-verified for every backend.
//
// Both levels are stable sorts, so ties keep arrival order, and both fall
// back to arrival order rather than failing: ordering is a fairness and
// readability policy, and must never block delivery.
func OrderForDispatch[T any](parts []Partition[T], eventOf func(T) *events.Event) []Partition[T] {
	out := slices.Clone(parts)
	oldest := make(map[string]time.Time, len(out))
	for i, p := range out {
		items := slices.Clone(p.Items)
		slices.SortStableFunc(items, func(a, b T) int {
			ea, eb := eventOf(a), eventOf(b)
			if ea == nil || eb == nil {
				return 0
			}
			return ea.Timestamp.Compare(eb.Timestamp)
		})
		out[i].Items = items

		var min time.Time
		for _, item := range items {
			ev := eventOf(item)
			if ev == nil {
				continue
			}
			if min.IsZero() || ev.Timestamp.Before(min) {
				min = ev.Timestamp
			}
		}
		oldest[p.Key] = min
	}
	slices.SortStableFunc(out, func(a, b Partition[T]) int {
		ta, tb := oldest[a.Key], oldest[b.Key]
		switch {
		// A partition with no comparable timestamp keeps arrival order
		// rather than sorting to an arbitrary end: ordering is a
		// fairness policy and must never block delivery.
		case ta.IsZero() || tb.IsZero():
			return 0
		case ta.Before(tb):
			return -1
		case tb.Before(ta):
			return 1
		default:
			return 0
		}
	})
	return out
}

// LogResult emits the standard structured line for a single delivery's
// outcome. An Ack logs nothing: the normal case is the overwhelming majority
// of deliveries, and a line per success buries the two that matter.
func LogResult(l *slog.Logger, topic, group string, ev *events.Event, r Result) {
	switch r.Outcome {
	case OutcomeNak:
		l.Warn("handler_failed", "topic", topic, "group", group,
			"event_type", eventType(ev), "error", errText(r.Err))
	case OutcomeDefer:
		l.Info("delivery_deferred", "topic", topic, "group", group,
			"event_type", eventType(ev), "reason", r.Reason)
	}
}

// LogBatchResult emits the standard line for one conversation partition's
// outcome.
//
// A distinct event name from LogResult, carrying the conversation key and
// the partition size, because the two failures are operationally different
// things: one delivery failing is a bad event, a whole conversation failing
// is a bad turn. A log consumer must be able to tell them apart without
// parsing, and every backend must emit the same name for the same situation
// — which is why this lives in the contract rather than in each backend.
func LogBatchResult(l *slog.Logger, topic, group, batchKey string, evs []*events.Event, r Result) {
	var head *events.Event
	if len(evs) > 0 {
		head = evs[0]
	}
	switch r.Outcome {
	case OutcomeNak:
		l.Warn("batch_handler_failed", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "error", errText(r.Err))
	case OutcomeDefer:
		l.Info("batch_delivery_deferred", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "reason", r.Reason)
	}
}

// LogListenerPanic emits the standard line for a publish listener that
// panicked and was recovered, and [LogStreamHandlerPanic] the one for a
// stream handler.
//
// # Why these live in the contract
//
// The same reason [LogBatchResult] does, and here it is not hypothetical:
// the backends had drifted into different spellings of one situation.
// `memory` logged `publish_listener_failed` with the recovered value under
// `error`; the broker-backed side logged `publish_listener_panicked` with
// it under `panic`. The stream side was worse — it keyed the topic as
// `subject` while `memory` keyed it as `topic` and added a `topic_pattern`
// nobody else emitted. An operator grepping `publish_listener_panicked` saw
// every backend but the in-memory twin, and the twin is what the tests run
// on, so nothing caught it.
//
// # `panic` rather than `error`
//
// A recovered value is not an error. It is whatever was passed to panic(),
// carries no Error() method in general, and the distinction is exactly what
// tells an operator the callback CRASHED rather than returned badly — two
// different bugs in two different places.
//
// Error level, not Warn: unlike a Nak there is no redelivery behind this.
// The listener's work for this event is simply gone.
func LogListenerPanic(l *slog.Logger, topic string, ev *events.Event, r any) {
	l.Error("publish_listener_panicked", "topic", topic,
		"event_type", eventType(ev), "panic", r)
}

// LogStreamHandlerPanic emits the standard line for a stream handler that
// panicked and was recovered. See [LogListenerPanic] for why it is here.
//
// The topic key is `topic`, matching [StreamHandler]'s own parameter name
// and every other line in this file. Two backends called it `subject`,
// which is NATS's word for the same thing and a word this contract does not
// use.
func LogStreamHandlerPanic(l *slog.Logger, topic string, ev *events.Event, r any) {
	l.Error("stream_handler_panicked", "topic", topic,
		"event_type", eventType(ev), "panic", r)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// EventQueue is a Publisher. A compile-time assertion rather than a
// convention, because the narrow interface exists only to be satisfied by
// this one.
var _ Publisher = (EventQueue)(nil)
