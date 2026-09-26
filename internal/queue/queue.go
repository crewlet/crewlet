// Package queue defines the EventQueue contract — the transport every
// inter-component message in Crewlet travels through.
//
// One interface, two backends (an in-memory twin, and NATS JetStream —
// embedded by default, external when configured), both certified by ONE
// conformance suite. A
// backend that the suite has not certified does not exist as far as the
// engine is concerned, and nothing above this package may branch on which
// backend is running. One broker carries both this stream and the fleet's
// coordination store, on one connection — ADR-0001, and the reason a broker
// without compare-and-set cannot be a backend here however good its
// messaging is.
//
// The rationale a reader should not have to re-derive:
//
//   - A durable subscription IS a seat's mailbox. It exists without a
//     consumer, retains what is published while nothing is attached, and
//     replays in order when someone attaches. Publishing to a topic with no
//     subscription drops the event silently — hence EnsureSubscription.
//   - Handlers have three outcomes, not two. Ack, Nak, and Defer (leave it
//     unacked, stop consuming) — see Result.
//   - A handler is told how many deliveries are LEFT before the backend
//     dead-letters a message, because every outcome that puts one back spends
//     one and the count rides on the message rather than on the process
//     handling it. TWO numbers, because one cannot answer both questions: the
//     PARTITION's (DeliveriesLeft, the smallest of its messages') answers
//     "will handing this batch back dead-letter something", and each
//     MESSAGE's (DeliveriesLeftFor) answers "how much is left of this one".
//     A caller that reads the first where it means the second is bounded by
//     the worst message it happens to be batched with.
//   - Attachment has four verbs with different destructiveness: Quiesce,
//     Unquiesce, Detach, DeleteSubscription.
//   - The durable subscriptions a broker holds can be LISTED
//     (ListSubscriptions). A mailbox outlives everything that knew its name,
//     a removed seat's handle included, so the broker has to be able to say
//     which mailboxes exist or a leaked one can never be found again.
package queue

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("queue.contract")

// ErrTooLarge reports that an event does not fit on the wire.
//
// THE SECOND SENTINEL THE LAYERS ABOVE MAY BRANCH ON, and for the same reason
// as ErrNotLive: they must not branch on which backend is running, so each one
// translates its own refusal into this and callers ask one question
// everywhere.
//
// It exists because "too large" is the one publish failure that is PERMANENT.
// Every other reason a publish fails — a broker restarting, a connection
// dropping, a quorum briefly unavailable — is answered by trying again, so a
// producer that cannot tell them apart has to treat all of them as transient.
// The webhook edge did exactly that and asked the provider to retry a delivery
// whose size guaranteed it would fail identically forever, releasing and
// re-taking its claim on every attempt.
//
// Sizing a producer's own input against MaxPayloadBytes cannot replace this.
// What a body costs on the wire is a property of its BYTES, not its length:
// encoding/json escapes '<', '>' and '&' to six bytes each, so one JSON
// document re-encodes at 1x and another of the same length at 6x. Any
// up-front ratio is therefore either wrong for the second or wasteful for the
// first — measured, and it is why this is an error from the transport rather
// than a division at the edge.
var ErrTooLarge = errors.New("queue: event too large for the transport")

// MaxPayloadBytes is the largest single event any backend must carry.
//
// It is a CONTRACT number rather than a backend's own, because every backend
// has to agree on it: the embedded broker is configured to accept exactly this
// (see internal/queue/jetstream) and the in-memory twin enforces the same
// ceiling, so a payload that a test accepts is one production accepts.
//
// Written down because there was no number at all, and the failure had no
// floor: the webhook edge accepted a 25 MiB delivery, verified its signature,
// claimed it, and then could not publish it through a broker left at its own
// 1 MiB default. The delivery was refused as unavailable, so the provider
// retried, and every retry failed the same way forever. Nothing in that loop
// is a transient, and nothing logged a size. Raising the ceiling is half the
// answer; ErrTooLarge, which lets a producer tell that refusal apart from a
// broker that is merely down, is the other half.
//
// 8 MiB is nats-server's own MAX_PAYLOAD_MAX_SIZE — the threshold above which
// it warns that a payload is too large to be a good idea (server/const.go).
// Sitting AT that boundary takes eight times the default without arguing with
// the broker about what it was built for: a message is buffered whole, in the
// server and again in every client that receives it, so this is memory per
// in-flight event and not a disk number.
const MaxPayloadBytes = 8 << 20

// MaxConcurrentAnswers is how many requests one [EventQueue.Serve]
// registration answers at once.
//
// SIXTY-FOUR, which is twice the default `node.max_concurrent`: every turn a
// node runs can be inside one tool call that asks a peer, so a registration
// answering one node's full complement of turns and a second node's beside it
// never queues a request behind another. The cap bounds goroutines and
// buffered payloads rather than throughput — a request past it waits for a
// slot, it is never dropped — and the answers this engine serves are
// kilobytes, far under [MaxPayloadBytes], so sixty-four in flight is memory
// nobody notices.
const MaxConcurrentAnswers = 64

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
	// config. Acking would claim work it will not perform.
	//
	// IT COSTS ONE DELIVERY, exactly as a Nak does, on every backend. No
	// broker here has a "give this back without counting it": returning a
	// message promptly means a Nak, and a Nak spends one of its deliveries
	// — so the delivery budget is sized to cover handoffs as well as
	// failures, and the backend dead-letters at the boundary rather than
	// letting the broker's own MaxDeliver backstop discard a healthy event
	// with nothing recorded. The alternative, letting the ack timer expire
	// instead, parks a seat's whole mailbox for the ack window on every
	// lease movement.
	//
	// THIS USED TO DIFFER BY BACKEND, and the contract said so: the
	// in-memory twin returned the events without touching their counters,
	// gated behind a conformance capability. That made one rule two —
	// DeliveriesLeft's doc asserted every return spends one while the
	// suite's own flag asserted a deferral spends nothing — and it made the
	// twin the only backend its own handoff case was ever run against. The
	// twin spends one now, so the sentence above is true of everything that
	// implements this contract.
	//
	// WHAT EVERY BACKEND OWES IS UNCHANGED, and it is the weaker claim the
	// capability used to protect: a deferral must not kill a HEALTHY event.
	// That is answered by sizing the budget so handoffs cannot exhaust it,
	// not by making the handoff free.
	//
	// When the consumer closes, the broker returns the message to
	// whoever attaches next, in order. Never substitute a republish: that
	// sends the event to the topic tail while its prefetched siblings
	// replay from the head, reordering the conversation.
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

// BatchHandler processes one PARTITION of a drained batch.
type BatchHandler func(ctx context.Context, evs []*events.Event) Result

// ErrNotLive reports that a verb reached a queue that is not live — never
// started, or stopped.
//
// ONE OF THE TWO SENTINELS THE LAYERS ABOVE MAY BRANCH ON — see ErrTooLarge
// for the other — because they must not branch on which backend is running:
// each backend keeps its own error (jetstream.ErrClosed,
// memory.ErrNotStarted) and wraps this, so errors.Is(err, queue.ErrNotLive)
// is the same question everywhere.
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

// AnswerFunc answers one scattered request.
//
// AN ERROR ANSWERS NOTHING. The asker learns that this server did not answer
// and nothing else — the same fact as a server that was down, slow or absent,
// and deliberately so: an asker able to tell a server's failure from its
// absence would have two cases to handle where the honest answer is one. What
// a failure is worth saying is said in the server's own log, where the server
// is.
//
// The context is the ANSWERER's, cancelled when its registration ends. The
// asker's deadline is enforced by the asker, because only the asker knows it.
type AnswerFunc func(ctx context.Context, request []byte) ([]byte, error)

// Unsubscribe cancels a stream subscription and releases backend resources.
type Unsubscribe func(ctx context.Context) error

// BatchKeyFunc derives the PARTITION KEY an event is handled under: which
// other events this one is drained, merged and dispatched WITH.
//
// NOT the conversation the event belongs to, which it was and which is a
// different question with a different answer on at least one source. A direct
// message is one conversation however it is threaded, so its identity is the
// bare channel — while a reply in a thread on that line still partitions on
// the thread, because merging it with unrelated top-level pings would hand
// one digest a reply target that names only one of them. This layer must not
// know that: internal/queue cannot import internal/notify, so the producer
// stamps both values and the caller hands the partition one down (see
// node.partitionKey, over notify.KeyOf). Prose is the only thing tying the
// two ends together, which is exactly why it has to name the right key.
type BatchKeyFunc func(ev *events.Event) string

// PublishListener is invoked inline on every publish. Listeners run in the
// publishing goroutine and must not block for long; their failures are
// logged and never prevent the publish.
type PublishListener func(ctx context.Context, topic string, ev *events.Event)

// Subscription names one durable subscription by the (topic, group) pair it
// was created with, exactly as the caller spelled both.
//
// The PAIR, because every backend keys a subscription on the pair and two
// groups on one topic are two mailboxes; see topics.AgentControlGroupSuffix for
// a pair of seats whose groups alone would collide.
type Subscription struct {
	Topic string
	Group string
}

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
	// by the key [BatchKeyFunc] derives preserving arrival order,
	// dispatch one handler call per partition oldest-partition-first,
	// and ack per partition. A failing partition never blocks or replays
	// a different one.
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
	// Does NOT touch pause holds: a seat resuming from a stale-renew
	// window may still be legitimately held by another subsystem (the
	// engine's own is the park of a node with no turn engine).
	Unquiesce(ctx context.Context, topic, group string) (bool, error)

	// Detach closes this process's consumers, leaving the subscription.
	//
	// NON-DESTRUCTIVE, WHICH IS A CLAIM ABOUT THE MAIL AND NOT ABOUT THE
	// COST. The subscription, its cursor and everything it retains all
	// survive, so nothing is lost and whoever attaches next gets the lot,
	// including what was published while nothing was attached. That is
	// what makes a seat handoff cheap and an unowned seat safe.
	//
	// WHAT THIS DOES COST is one delivery of each message the consumer
	// had taken and not settled. Those go back the way every hand-back
	// goes back, because no backend here has a "return this without
	// counting it" — see DeliveriesLeft, which is the one place that rule
	// and the blockers that trigger it are written down, a detach among
	// them. The partitions a stopped drain never dispatched are charged
	// on the same terms as the one that stopped it. A message under a
	// RUNNING handler is the exception, and not because it is cheaper:
	// the handler runs to completion and its own outcome settles it.
	//
	// A message that goes back may return BEHIND events that were never
	// delivered rather than at the head. The backends genuinely differ
	// there (queuetest.Caps.HeadReplayOnNak), so nothing above this
	// package may depend on either answer — within-conversation order
	// comes from event timestamps, which is what OrderForDispatch is for.
	//
	// Releases this attachment's pause holds — a hold that outlived a
	// detach would leave a re-attaching node silently deaf.
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
	// Creating an existing subscription is success.
	//
	// The bool reports whether this call found it absent AND then
	// provisioned it. That is exact on a backend with one writer, and on
	// a fleet it is the closest thing to an answer there is: two nodes
	// that create the same subscription in the same instant both receive
	// it, because a broker that returns the object either way gives a
	// client no way to tell "I made this" from "this was already here".
	// So on a simultaneous boot more than one node can report true.
	//
	// Deliberately NOT a three-valued answer, and nothing in the engine
	// branches on it: it is a count in a log line. A tri-state nobody
	// reads would be machinery invented to describe a race the broker
	// does not expose, rather than an answer anyone could act on.
	EnsureSubscription(ctx context.Context, topic, group string) (bool, error)

	// DeleteSubscription destroys the subscription and its retained
	// mail. Must not require a local consumer: decommissioning a role
	// cannot depend on which node happened to run the seat.
	DeleteSubscription(ctx context.Context, topic, group string) (bool, error)

	// ListSubscriptions reports every durable subscription whose topic
	// matches topicPattern (`*` one segment, `>` trailing segments, the
	// grammar SubscribeStream takes), as the exact pair it was created
	// with, in no promised order.
	//
	// EVERY one the broker holds, not this client's: one made through
	// EnsureSubscription, Subscribe or SubscribeBatch, by any client of
	// the broker, attached or not. A deleted subscription is not listed,
	// and neither is an ephemeral SubscribeStream subscription, which is
	// not a mailbox. An empty pattern is refused rather than read as
	// "everything"; ">" asks for everything.
	//
	// It exists because a durable subscription outlives every record of
	// its name. A seat's mailbox is named after its handle, a removed
	// seat's handle is gone from the org every node derives names from,
	// and a registry written beside the broker can miss one (a write that
	// failed, a mailbox older than the registry). Without a listing, a
	// mailbox that escaped the registry retains its mail for the life of
	// the deployment and nothing can ever find it.
	ListSubscriptions(ctx context.Context, topicPattern string) ([]Subscription, error)

	// SubscribeStream creates an ephemeral per-caller broadcast
	// subscription — every subscriber receives every matching event.
	// topicPattern supports `*` (one segment) and `>` (trailing
	// segments).
	SubscribeStream(ctx context.Context, topicPattern string, h StreamHandler) (Unsubscribe, error)

	// AddPublishListener registers a listener called inline on every
	// publish.
	AddPublishListener(l PublishListener)

	// Ask scatters one request to every process serving subject and
	// collects the replies, returning when want of them have arrived or
	// when ctx is done, whichever comes first. want of 0 waits out the
	// deadline, which is what a caller that does not know how many
	// answerers exist has to do.
	//
	// EPHEMERAL, and that is why it is a verb of its own rather than a
	// Publish and a Subscribe. Nothing here is retained, redelivered or
	// recorded: no stream, no consumer group, no ack, no publish
	// listener, no event-store row. A request nobody serves is a request
	// that never existed, and a reply that arrives after its asker left
	// is dropped rather than held for the next one. The durable verbs are
	// the wrong shape for a query: a search fan-out that wrote two
	// records and an audit row per keystroke would make the audit log a
	// function of how often somebody typed.
	//
	// FEWER REPLIES THAN want IS NOT AN ERROR. A peer that is slow, gone,
	// or simply not serving this subject is the ordinary case, and only
	// the caller knows what a missing answer costs it — an error would
	// force every caller to unwrap one to find out how many it got. An
	// error means the ask could not be made at all.
	//
	// THE REPLIES ARE UNORDERED and carry no sender. What a reply means
	// is inside its own bytes, because the transport cannot say: a
	// scatter has no roster, so "who did not answer" is a question only
	// the payload can answer.
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)

	// Serve makes this process one of subject's answerers.
	//
	// EVERY SERVER OF A SUBJECT SEES EVERY REQUEST — the opposite of
	// Subscribe's competing consumers, and the reason this is not a
	// consumer group: a scatter divides work by what the request NAMES,
	// so a broker that handed each request to one member would divide it
	// twice and cover a fraction.
	//
	// An answerer that returns an error answers NOTHING. Its asker sees a
	// server that did not answer, which is the same fact as a server that
	// was not running — see AnswerFunc.
	//
	// ANSWERS RUN CONCURRENTLY, up to [MaxConcurrentAnswers] per
	// registration, and a request beyond that waits for a slot rather
	// than being dropped. One slow answer must never hold the next
	// request behind it: an answerer serves every asker in the fleet, and
	// a registration that answered one at a time made each of them wait
	// for the sum of everybody else's latency.
	//
	// The returned Unsubscribe cancels the answerer's context and then
	// WAITS for the answers in flight, bounded by the context it is given
	// — so a caller that withdraws and then closes what the answerer
	// reads never closes it under an answer still reading.
	Serve(ctx context.Context, subject string, h AnswerFunc) (Unsubscribe, error)

	// Start connects the backend and begins consuming.
	//
	// WHETHER A BACKEND REQUIRES IT IS ITS OWN ANSWER, and both shipped
	// answers are legitimate: on JetStream, Open establishes the
	// connection and the streams, so Start is a no-op and a publish
	// before it works; on the in-memory twin, Start is what makes the
	// client live.
	//
	// What a backend may NOT do is answer DIFFERENTLY PER VERB. Before
	// Start, all twelve of the publish, subscription and attachment verbs
	// must give the same answer — Publish, Subscribe, SubscribeBatch,
	// Quiesce, Unquiesce, Detach, EnsureSubscription, DeleteSubscription,
	// ListSubscriptions, SubscribeStream, PauseTopic and ResumeTopic.
	// Either they all refuse, or none of them require it.
	// Capabilities.RequiresStart says which, and queuetest sends all
	// twelve to hold a backend to one answer.
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
	// PAIR, so a second subsystem gating the same inbox cannot release
	// the first one's hold by lifting its own, and a hold on one group
	// does not gate every other group on a shared subject. The engine
	// takes one reason today: a seat whose node has no turn engine
	// pauses before requeuing, so the copies buffer rather than loop.
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
	// AFTERWARDS EVERY ONE OF THE TWELVE VERBS REFUSES. A requirement
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

// Partition is one partition key's slice of a drained batch.
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
			log.Error("batch_key_failed", "event_type", eventType(ev), "panic", r,
				"stack", string(debug.Stack()))
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
// starves a quiet partition behind a hot one under deferral — the quiet
// partition's requeued copies re-enter the topic AFTER whatever arrived
// during the hot one's turn, so receive-ordered dispatch picks the hot one
// on every drain. Timestamps carry the aging signal, so the partition that
// has waited longest dispatches first.
//
// Within a partition: event timestamp, not delivery order. This is what
// makes a partition read correctly regardless of how a broker interleaves
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
		// A partition with no comparable timestamp sorts LAST, and
		// stably among its own kind — so it keeps arrival order relative
		// to the others like it, and the stamped partitions still age
		// against each other.
		//
		// Answering 0 whenever EITHER side was unstamped read as "these
		// two are equal" and was not an ordering at all: it is not
		// transitive, so one unstamped partition anywhere in a drain made
		// the sort leave unrelated stamped partitions in arrival order,
		// silently switching aging off for the whole drain. An event with
		// no timestamp is a bug upstream; the fairness policy has to keep
		// working for everything around it either way.
		case ta.IsZero() && tb.IsZero():
			return 0
		case ta.IsZero():
			return 1
		case tb.IsZero():
			return -1
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

// LogBatchResult emits the standard line for one partition's outcome.
//
// A distinct event name from LogResult, carrying the partition key and the
// partition size, because the two failures are operationally different
// things: one delivery failing is a bad event, a whole partition failing is
// a bad turn. A log consumer must be able to tell them apart without
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
// THE STACK IS CAPTURED HERE, not passed in, and that works because these are
// called from inside the deferred function that recovered: a deferred call
// runs before its frame is popped, so debug.Stack() still walks down through
// runtime.gopanic into the function that actually panicked. Capturing it at
// the recover site and threading it through would be identical minus one
// frame, and minus the chance a caller forgets.
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
		"event_type", eventType(ev), "panic", r, "stack", string(debug.Stack()))
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
		"event_type", eventType(ev), "panic", r, "stack", string(debug.Stack()))
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
