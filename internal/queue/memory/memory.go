// Package memory implements the EventQueue contract in one process.
//
// This backend is a SEMANTIC TWIN of a real broker, not a convenience stub,
// because it is the backend every unit test runs on: a divergence here does not
// merely fail to catch a bug, it actively certifies one.
//
// Three properties carry that weight, and each replaced an incident:
//
//   - A (topic, group) pair is a DURABLE SUBSCRIPTION. It exists whether or not
//     anything is attached, retains events published while nothing is, and
//     replays them in order when a consumer attaches. Seat ownership rests
//     entirely on that — a seat between owners must hold its mail, not lose it.
//     Publishing to a topic with no subscription drops the event, which is why
//     EnsureSubscription exists.
//   - Members of a group COMPETE: each event goes to exactly one of them, in
//     rotation. Delivering always to the first-registered member made the
//     double-attach split-brain — two nodes consuming one seat — invisible, so
//     a test asserting "exactly one delivery" passed while a real Shared
//     subscription split the traffic and ran two interleaved turn streams.
//   - A BROKER AND A CLIENT ARE DIFFERENT THINGS. Subscriptions and the mail in
//     them belong to the Broker; attachments, pause holds, quiesce flags and
//     the drain pause belong to a node. For one process the conflation is
//     invisible; for two it inverts the property above — one node's Detach
//     dropped its peer's consumer, and one node's sandbox pause stopped its
//     peer serving a seat it owned. Broker.Client mints a peer.
//
// What it still does differently, deliberately: DISPATCH IS INLINE. Publish
// drains the backlogs it can reach before returning, so a test can publish and
// assert. A real broker's handler runs later, elsewhere, possibly twice —
// anything a test asserts immediately after Publish is a race in production,
// and this backend cannot tell you that. Go expresses the same property
// without Python's re-entrant recursion: a handler that publishes into the
// subscription it is draining flags another pass instead of nesting one (see
// dispatch.go).
//
// Redelivery matches the broker's shape: the budget counts redeliveries AFTER
// the first delivery (so N+1 total attempts), and an exhausted message moves to
// the dead-letter subject rather than being destroyed.
package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("queue.memory")

// ErrNotStarted is returned on a queue that has not been started, or has been
// stopped, by EVERY publish, subscription and attachment verb — not just the
// ones a caller thinks of as needing a connection. The contract requires one
// answer for all of them (see queue.EventQueue.Start), and this backend's
// answer is "Start is what makes the client live".
//
// It used to be only Publish, Subscribe and SubscribeBatch, while
// EnsureSubscription created durable state, DeleteSubscription destroyed it
// and PauseTopic took a hold — on a client whose Stop had already dropped
// every consumer. That hold survived into the next life and left a restarted
// queue reporting itself running and silently deaf.
//
// A sentinel because callers branch on it: a boot-ordering mistake looks
// exactly like a transport failure otherwise.
var ErrNotStarted = fmt.Errorf("memory: event queue is not started: %w", queue.ErrNotLive)

// ErrNilHandler is returned when a subscription is registered with no handler.
// Accepting one would attach a consumer that swallows a seat's mail and panics
// on the first delivery.
var ErrNilHandler = errors.New("memory: handler is nil")

const (
	// defaultMaxRedeliveries matches the Pulsar backend's budget in TOTAL
	// ATTEMPTS, which is the only comparison that means anything here: this
	// constant counts redeliveries AFTER the first delivery, while Pulsar's
	// DLQPolicy.MaxDeliveries and NATS MaxDeliver both count total
	// deliveries. So 9 here and 10 there are the same budget, and the 10
	// this used to hold was one attempt MORE than Pulsar's — the same
	// numeral denoting a different quantity.
	//
	// That is worth the paragraph because the repo has already been bitten
	// by it once: the suite's capability was renamed WithRedeliveryBudget
	// -> WithDeliveryAttempts precisely because "budget" never said which
	// convention it counted, and a backend then wrote MaxDeliver: budget+1
	// to satisfy the missing half. The suite was fixed; this constant kept
	// asserting "in lockstep with Pulsar" without ever naming a convention,
	// so the claim read as checked and could not be checked.
	//
	// Pulsar is the right twin and JetStream is not: both this backend and
	// Pulsar deliver a deferral for FREE (Capabilities.FreeDeferral on
	// both), whereas JetStream returns a deferral via Nak, which spends an
	// attempt — so it budgets 25 to cover handoff as well as poison. See
	// internal/queue/pulsar/pulsar.go maxDeliveries and
	// internal/queue/jetstream/stream.go maxDeliver; if either moves, this
	// tracks Pulsar's.
	defaultMaxRedeliveries = 9

	// defaultMaxHistory bounds the published-event log this backend keeps
	// for tests and diagnostics. It is not a mailbox — retention that
	// matters lives in the subscriptions.
	//
	// It is a CEILING against unbounded growth, not a capacity anyone
	// should tune to, and the two are worth telling apart because the
	// number looks like a sizing decision and is not one. Measured: the
	// high-water mark of any one broker's history across a full conformance
	// run is 5 events, so this is not sized to test traffic at all — a
	// backlog of 5 and a ceiling of 10000 are answers to different
	// questions, and the ceiling exists for a broker that outlives a test
	// binary.
	//
	// What it actually costs is the reason to keep it finite: entries are
	// live pointers, so the buffer pins whole events against GC for as long
	// as the broker lives. That is the trade to reason about if anyone
	// moves it — not how many events a test publishes.
	defaultMaxHistory = 10000
)

// subKey is the (topic, group) pair. Holds and quiesce flags are keyed by the
// PAIR, never by topic alone: keyed by topic they both outlived the attachment
// — so a node that re-acquired a seat attached into a still-paused topic — and
// gated every group on shared subjects like crewlet.events.*.
type subKey struct{ topic, group string }

// Broker is the state a fleet SHARES: subscriptions, the mail in them, the
// broadcast streams, and dead letters.
//
// Every field is guarded by mu, including the per-client state hanging off the
// Queues built from this broker. One lock rather than a lock per structure
// because a delivery reads and writes both halves in one step, and two locks
// would need an ordering rule that a future reader is guaranteed to get wrong.
//
// Two things are NEVER done while holding it, and both are the same mistake:
//
//   - running a handler (see dispatch.go), and
//   - writing a log line.
//
// A log call is a write to a handler this package does not control — it can
// block on a full pipe, a slow file, or a contended CI stderr — and this one
// mutex guards every subscription and every peer. Holding it across that write
// stops the whole company for as long as the write takes, and a goroutine
// parked in the middle of a drain is what it looks like from the outside.
// Gather what a line needs, unlock, then log.
type Broker struct {
	mu          sync.Mutex
	subs        map[subKey]*subscription
	streams     []*streamSub
	deadLetters map[string][]*events.Event

	history    []*events.Event
	maxHistory int
}

// NewBroker returns an empty broker with no clients.
func NewBroker() *Broker {
	return &Broker{
		subs:        map[subKey]*subscription{},
		deadLetters: map[string][]*events.Event{},
		maxHistory:  defaultMaxHistory,
	}
}

// Client mints another node on this broker.
//
// Returned already stopped, like any freshly constructed queue: it models a
// separate process's connection, so it starts and stops on its own schedule.
// Its attachments, pause holds and drain state are its own; the subscriptions
// and the mail in them are shared, because those live here.
func (b *Broker) Client(opts ...Option) *Queue {
	q := &Queue{
		broker:          b,
		maxRedeliveries: defaultMaxRedeliveries,
		pauses:          map[subKey]map[string]struct{}{},
		quiescing:       map[subKey]struct{}{},
	}
	// Options are applied under the broker lock because one of them
	// reaches the broker, and a peer minted while its siblings are
	// publishing must not race them. Options are plain field setters for
	// the same reason — one that called back into the queue would
	// deadlock here.
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// subscription is a durable subscription: retained mail plus whoever is
// attached. The mail outlives every attachment — that is the whole point, it
// is what a seat's inbox holds while no node owns the seat.
type subscription struct {
	topic, group string
	mail         []*events.Event
	members      []*consumer

	// cursor is the round-robin position across the members that can take
	// a delivery right now.
	cursor int

	// redeliveries mirrors the broker's per-message counter.
	redeliveries map[uuid.UUID]int

	// draining and the two flags below make inline dispatch re-entrant
	// without recursion: a handler that publishes into the subscription it
	// is draining asks the running pass to go round again rather than
	// starting a nested one. Nesting would interleave two drains over one
	// mailbox and reorder the conversation.
	draining         bool
	drainAgain       bool
	drainAgainBypass bool
}

func (s *subscription) membersOf(client *Queue) []*consumer {
	var out []*consumer
	for _, m := range s.members {
		if m.client == client {
			out = append(out, m)
		}
	}
	return out
}

// consumer is one attached consumer on a subscription.
//
// Single-event and batch consumers share a record because they share a
// subscription: on a real broker, subscribing twice against the same
// (topic, group) creates two consumers on ONE subscription and both compete for
// its messages. Modelling them as separate registries made the second one
// silently unreachable.
type consumer struct {
	// client is the node this attachment belongs to. Without the
	// back-reference Detach could only mean "drop everyone", which in a
	// fleet is one node tearing down its peer's consumer, and Attachments
	// could only answer a fleet-wide question when the operationally
	// interesting one is "what is THIS node serving?".
	client *Queue
	key    subKey

	handler      queue.Handler
	batchHandler queue.BatchHandler
	batched      bool
	batchKey     queue.BatchKeyFunc
	options      *queue.BatchOptions

	// window is the open linger window, owned by this consumer and
	// cancelled with it. Per consumer, not per subscription: two clients
	// batching the same seat linger independently, exactly as two
	// processes would.
	window *lingerWindow
}

type streamSub struct {
	owner   *Queue
	pattern string
	handler queue.StreamHandler
}

// Option configures a Queue at construction.
type Option func(*Queue)

// WithMaxRedeliveries sets how many times a message is redelivered after its
// first delivery before it is dead-lettered.
func WithMaxRedeliveries(n int) Option {
	return func(q *Queue) {
		if n >= 0 {
			q.maxRedeliveries = n
		}
	}
}

// WithMaxHistory bounds the broker's published-event log. It applies to the
// broker this queue is built on, so the last caller wins; peers share one log.
func WithMaxHistory(n int) Option {
	return func(q *Queue) {
		if n > 0 {
			q.broker.maxHistory = n
		}
	}
}

// Queue is one node's connection to a Broker.
type Queue struct {
	broker          *Broker
	maxRedeliveries int

	// Everything below is guarded by broker.mu. Node state, not broker
	// state: every gate here describes THIS process's consumer, and a
	// subscription-level answer would let one node's sandbox pause, or one
	// node's shutdown, stop a peer from serving the seat it owns.
	running   bool
	paused    bool
	pauses    map[subKey]map[string]struct{}
	quiescing map[subKey]struct{}
	listeners []queue.PublishListener

	// inFlight counts running handlers and is what a drain waits on.
	//
	// The shared [queue.Inflight] rather than a count beside the broker
	// state, because the contract has three backends and this is the one
	// place they used to differ: the twin kept a count under a mutex with
	// a channel closed on the transition to zero (correct), and both real
	// backends used a sync.WaitGroup (a documented misuse — Add may not
	// start from zero concurrently with Wait, which is exactly what a
	// dispatch loop does). One implementation is the fix; the twin's was
	// the one that was right.
	inFlight queue.Inflight
}

// New returns a queue on a fresh broker of its own — the single-process case,
// which is a fleet of one.
func New(opts ...Option) *Queue { return NewBroker().Client(opts...) }

// Client mints another node on the same broker as this one.
func (q *Queue) Client(opts ...Option) *Queue {
	return q.broker.Client(append([]Option{WithMaxRedeliveries(q.maxRedeliveries)}, opts...)...)
}

// Broker returns the broker this queue is a client of.
func (q *Queue) Broker() *Broker { return q.broker }

// Backend implements queue.EventQueue.
func (q *Queue) Backend() string { return "memory" }

var _ queue.EventQueue = (*Queue)(nil)

// --- lifecycle ------------------------------------------------------------

// Start connects this node. Idempotent, and it clears the one-way delivery
// pause so a queue reused after a drain serves again.
func (q *Queue) Start(context.Context) error {
	q.broker.mu.Lock()
	if q.running {
		q.broker.mu.Unlock()
		return nil
	}
	q.running, q.paused = true, false
	// Clear the process-local gates here as well as in Stop.
	//
	// Stop clears them because "a hold that outlived a stop left a reused
	// queue silently deaf" — but clearing on only one side leaves the window
	// between the two open, and a hold taken there survives into the next
	// life. Measured: Stop, PauseTopic, Start, Subscribe, Publish delivers
	// nothing, with holds=[sandbox] on a queue that reports itself running.
	// That is the same incident reached from the other side, and it is
	// reachable by a sandbox gate or a config shed racing a drain.
	//
	// The invariant is about the START of a life, not the end of one: a
	// queue that has been started serves, and is never silently gated by
	// state from a previous one.
	clear(q.pauses)
	clear(q.quiescing)
	q.broker.mu.Unlock()

	log.Info("memory_event_queue_started")
	return nil
}

// Stop closes THIS client. The broker, and every peer, live on.
//
// Attachments, pause holds and quiesce flags are process state; the
// subscriptions and their retained mail are not. Clearing the holds matters: a
// hold that outlived a stop left a reused queue silently deaf.
func (q *Queue) Stop(context.Context) error {
	q.broker.mu.Lock()
	if !q.running {
		q.broker.mu.Unlock()
		return nil
	}
	q.running, q.paused = false, false
	for _, sub := range q.broker.subs {
		q.dropMembersLocked(sub)
	}
	clear(q.pauses)
	clear(q.quiescing)
	q.broker.streams = slices.DeleteFunc(q.broker.streams, func(s *streamSub) bool {
		return s.owner == q
	})
	q.broker.mu.Unlock()

	log.Info("memory_event_queue_stopped")
	return nil
}

// dropMembersLocked removes this client's consumers from a subscription and
// closes any linger window they were holding open.
func (q *Queue) dropMembersLocked(sub *subscription) int {
	mine := sub.membersOf(q)
	if len(mine) == 0 {
		return 0
	}
	for _, m := range mine {
		m.closeWindowLocked()
	}
	sub.members = slices.DeleteFunc(sub.members, func(m *consumer) bool { return m.client == q })
	if len(sub.members) == 0 {
		sub.cursor = 0
	}
	return len(mine)
}

// --- publishing -----------------------------------------------------------

// Publish records the event, hands it to every subscription on the topic, and
// drains what it can reach before returning.
//
// The event is durable — in every mailbox that will hold it — before the
// best-effort fan-outs run, so a listener or a stream subscriber can never
// observe a publish that a subscriber will not.
//
// Everything retained or delivered is DECODED FROM THE WIRE, never the
// publisher's own pointer. That is the single most load-bearing thing about
// this twin: a broker is a serialization boundary, and a twin that skips it
// certifies bugs rather than catching them. Three concrete ones it would hide
// — a payload keeping a Go type it loses in transit (a value payload arrives
// as a pointer), a JSON number arriving as an int rather than a float64, and
// one group's handler mutating what another group is about to receive.
func (q *Queue) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if ev == nil {
		return errors.New("memory: nil event")
	}
	// Serialised before the lock, and once: the bytes are what every
	// consumer decodes from, so a failure here fails the publish exactly as
	// it would on a real backend rather than half-delivering.
	wire, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("memory: serialize event %s: %w", ev.Type, err)
	}
	var received events.Event
	if err := json.Unmarshal(wire, &received); err != nil {
		return fmt.Errorf("memory: deserialize event %s: %w", ev.Type, err)
	}

	q.broker.mu.Lock()
	if !q.running {
		q.broker.mu.Unlock()
		return ErrNotStarted
	}
	q.broker.recordHistoryLocked(&received)

	// Every subscription on this topic gets its own copy — that is what a
	// consumer GROUP is. Competition happens between the members of one
	// group, never across groups.
	var targets []*subscription
	for _, sub := range q.broker.subs {
		if sub.topic == topic {
			sub.mail = append(sub.mail, decodeWire(wire, &received))
			targets = append(targets, sub)
		}
	}
	listeners := slices.Clone(q.listeners)
	streams := slices.Clone(q.broker.streams)
	q.broker.mu.Unlock()

	logPublished(topic, &received)
	// Listeners see the PUBLISHER'S event, not a decoded copy, because they
	// are a local hook on the local publish path — the event store's writer
	// is one — and they run before anything reaches a wire. Real backends
	// call them the same way.
	for _, l := range listeners {
		notifyListener(ctx, l, topic, ev)
	}
	dispatchStreams(ctx, streams, topic, wire, &received)

	if len(targets) == 0 {
		// No durable subscription exists, so there is nothing to retain
		// the event — a real broker drops it too. EnsureSubscription
		// exists precisely so a seat's mail never depends on someone
		// being attached at the time.
		log.DebugContext(ctx, "event_unsubscribed", "topic", topic, "event_type", ev.Type)
		return nil
	}
	// Sorted so a multi-group topic drains in a stable order; map
	// iteration would make an interleaving of handlers depend on the hash
	// seed, and a flaky test on a shared subject is unfixable.
	slices.SortFunc(targets, func(a, b *subscription) int { return cmp.Compare(a.group, b.group) })
	for _, sub := range targets {
		q.broker.drain(ctx, sub, false)
	}
	return nil
}

// AddPublishListener registers a listener called inline on every publish
// THROUGH THIS CLIENT. Listeners are a local hook on the local publish path —
// the event store one node writes is not a broker feature — so a peer's
// publishes do not reach them.
func (q *Queue) AddPublishListener(l queue.PublishListener) {
	if l == nil {
		return
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	q.listeners = append(q.listeners, l)
}

func logPublished(topic string, ev *events.Event) {
	// A seat's inbox and an inbound webhook are the two topics an operator
	// reads a log to follow; everything else is debug volume.
	if strings.HasSuffix(topic, ".inbound") || strings.HasSuffix(topic, topics.AgentInboxSuffix) {
		log.Info("event_published", "topic", topic, "event_type", ev.Type)
		return
	}
	log.Debug("event_published", "topic", topic, "event_type", ev.Type)
}

func notifyListener(ctx context.Context, l queue.PublishListener, topic string, ev *events.Event) {
	// A listener failure must not prevent delivery: the publisher's job is
	// the event, not the observer.
	defer func() {
		if r := recover(); r != nil {
			queue.LogListenerPanic(log, topic, ev, r)
		}
	}()
	l(ctx, topic, ev)
}

func (b *Broker) recordHistoryLocked(ev *events.Event) {
	b.history = append(b.history, ev)
	if len(b.history) > b.maxHistory {
		b.history = b.history[len(b.history)-b.maxHistory:]
	}
}

// --- subscribing ----------------------------------------------------------

// Subscribe attaches a competing-consumer handler to topic/group.
func (q *Queue) Subscribe(ctx context.Context, topic, group string, h queue.Handler) error {
	if h == nil {
		return ErrNilHandler
	}
	sub, err := q.attach(topic, group, &consumer{handler: h})
	if err != nil {
		return err
	}
	log.DebugContext(ctx, "subscription_added", "topic", topic, "group", group)
	q.broker.drain(ctx, sub, false)
	return nil
}

// SubscribeBatch attaches with batched, key-partitioned delivery. opts is read
// live on every cycle, so a config reload changes linger and batch size with no
// re-subscription.
func (q *Queue) SubscribeBatch(
	ctx context.Context,
	topic, group string,
	h queue.BatchHandler,
	key queue.BatchKeyFunc,
	opts *queue.BatchOptions,
) error {
	if h == nil {
		return ErrNilHandler
	}
	if opts == nil {
		opts = queue.DefaultBatchOptions()
	}
	sub, err := q.attach(topic, group, &consumer{
		batchHandler: h,
		batched:      true,
		batchKey:     key,
		options:      opts,
	})
	if err != nil {
		return err
	}
	log.DebugContext(ctx, "batch_subscription_added", "topic", topic, "group", group)
	q.broker.drain(ctx, sub, false)
	return nil
}

// attach registers a consumer and returns the subscription it joined.
func (q *Queue) attach(topic, group string, c *consumer) (*subscription, error) {
	key := subKey{topic, group}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if !q.running {
		return nil, ErrNotStarted
	}
	// Attaching is an explicit statement of intent to consume, so it
	// clears any quiesce on this key — the same reason Detach does.
	// Without it a stale entry strands the subscription forever: a fenced
	// release detaches (clearing it), an in-flight handler abandoned by
	// that release then defers and puts the key straight back, and from
	// there every future attachment is undeliverable with nothing left to
	// un-quiesce it. A real broker quiesces per attachment, so the twin
	// would have been the only one to strand the seat.
	delete(q.quiescing, key)
	sub := q.broker.ensureLocked(topic, group)
	c.client, c.key = q, key
	sub.members = append(sub.members, c)
	return sub, nil
}

func (b *Broker) ensureLocked(topic, group string) *subscription {
	key := subKey{topic, group}
	if sub, ok := b.subs[key]; ok {
		return sub
	}
	sub := &subscription{topic: topic, group: group, redeliveries: map[uuid.UUID]int{}}
	b.subs[key] = sub
	return sub
}

// notStartedLocked reports whether this client is down, for the verbs that
// must refuse when it is. Called with broker.mu held, so the check and the
// mutation it guards are one critical section: a two-step "ask, then act"
// would let a Stop land between them and re-open the window this closes.
func (q *Queue) notStartedLocked() bool { return !q.running }

// --- the four attachment verbs --------------------------------------------

// Quiesce stops taking NEW work while staying attached; a running handler
// finishes. Reports whether an attachment existed.
func (q *Queue) Quiesce(_ context.Context, topic, group string) (bool, error) {
	key := subKey{topic, group}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return false, ErrNotStarted
	}
	sub, ok := q.broker.subs[key]
	if !ok || len(sub.membersOf(q)) == 0 {
		q.broker.mu.Unlock()
		return false, nil
	}
	q.quiescing[key] = struct{}{}
	q.broker.mu.Unlock()

	log.Info("subscription_quiesced", "topic", topic, "group", group)
	return true, nil
}

// Unquiesce resumes a quiesced attachment and drains what it held back,
// reporting whether it was quiesced.
//
// It deliberately does NOT touch pause holds: a seat resuming from a
// stale-renew window may still be legitimately paused for a running sandbox,
// and clearing that would deliver into a suspended turn.
func (q *Queue) Unquiesce(ctx context.Context, topic, group string) (bool, error) {
	key := subKey{topic, group}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return false, ErrNotStarted
	}
	if _, held := q.quiescing[key]; !held {
		q.broker.mu.Unlock()
		return false, nil
	}
	delete(q.quiescing, key)
	sub := q.broker.subs[key]
	q.broker.mu.Unlock()

	log.InfoContext(ctx, "subscription_unquiesced", "topic", topic, "group", group)
	if sub != nil {
		q.broker.drain(ctx, sub, false)
	}
	return true, nil
}

// Detach drops THIS client's consumers; the subscription and its mail stay.
//
// The non-destructive verb. Undelivered events remain for whoever attaches
// next, in order — the in-process analogue of a broker cursor surviving a
// handoff. A peer's consumer on the same subscription is untouched, which is
// the whole point of the distinction: detaching is a node saying "I have
// stopped serving this seat", never "nobody is serving it".
func (q *Queue) Detach(_ context.Context, topic, group string) (bool, error) {
	key := subKey{topic, group}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return false, ErrNotStarted
	}
	// Holds and the quiesce flag describe THIS attachment, so they are
	// released with it — unconditionally, before the attachment is even
	// looked up. One that outlived a detach would leave a node that
	// re-attached later silently deaf.
	delete(q.pauses, key)
	delete(q.quiescing, key)

	sub, ok := q.broker.subs[key]
	if !ok {
		q.broker.mu.Unlock()
		return false, nil
	}
	dropped := q.dropMembersLocked(sub)
	q.broker.mu.Unlock()
	if dropped == 0 {
		return false, nil
	}

	log.Info("subscription_detached", "topic", topic, "group", group, "consumers", dropped)
	return true, nil
}

// EnsureSubscription creates the durable subscription if absent, with no
// consumer attached and positioned at the earliest message.
func (q *Queue) EnsureSubscription(_ context.Context, topic, group string) (bool, error) {
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return false, ErrNotStarted
	}
	if _, ok := q.broker.subs[subKey{topic, group}]; ok {
		q.broker.mu.Unlock()
		return false, nil
	}
	q.broker.ensureLocked(topic, group)
	q.broker.mu.Unlock()

	log.Info("subscription_created", "topic", topic, "group", group)
	return true, nil
}

// DeleteSubscription destroys the subscription and its retained mail.
//
// It detaches this client first if it happens to hold a consumer, but never
// requires one: decommissioning a role cannot depend on which node ran the
// seat.
func (q *Queue) DeleteSubscription(ctx context.Context, topic, group string) (bool, error) {
	// Checked here as well as inside Detach, so the refusal names the verb
	// the caller actually invoked rather than surfacing as a failure of a
	// step it did not ask for.
	q.broker.mu.Lock()
	down := q.notStartedLocked()
	q.broker.mu.Unlock()
	if down {
		return false, ErrNotStarted
	}
	if _, err := q.Detach(ctx, topic, group); err != nil {
		return false, err
	}
	key := subKey{topic, group}
	q.broker.mu.Lock()
	sub, ok := q.broker.subs[key]
	if !ok {
		q.broker.mu.Unlock()
		return false, nil
	}
	delete(q.broker.subs, key)
	discarded := slices.Clone(sub.mail)
	q.broker.mu.Unlock()

	// One line per discarded event, and deliberately after the unlock:
	// this is the one log site whose volume is unbounded, so it is also
	// the one that would hold the broker mutex longest.
	for _, ev := range discarded {
		log.InfoContext(ctx, "event_discarded", "topic", topic, "group", group,
			"event_type", ev.Type, "reason", "subscription_deleted")
	}
	log.InfoContext(ctx, "subscription_deleted", "topic", topic, "group", group)
	return true, nil
}

// --- broadcast streams ----------------------------------------------------

// SubscribeStream creates an ephemeral per-caller broadcast subscription:
// every subscriber receives every matching event, and nothing is acked or
// redelivered.
//
// Registered on the BROKER, not on this client, because a broadcast is a
// broker fact: a dashboard attached to one node must see what another node
// publishes, or the twin would model a fleet in which half the traffic is
// invisible. The owning client is recorded so Stop takes down only its own.
func (q *Queue) SubscribeStream(_ context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error) {
	if h == nil {
		return nil, ErrNilHandler
	}
	sub := &streamSub{owner: q, pattern: pattern, handler: h}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return nil, ErrNotStarted
	}
	q.broker.streams = append(q.broker.streams, sub)
	q.broker.mu.Unlock()
	log.Debug("stream_subscription_added", "topic_pattern", pattern)

	return func(context.Context) error {
		q.broker.mu.Lock()
		before := len(q.broker.streams)
		q.broker.streams = slices.DeleteFunc(q.broker.streams, func(s *streamSub) bool { return s == sub })
		removed := before != len(q.broker.streams)
		q.broker.mu.Unlock()
		if removed {
			log.Debug("stream_subscription_removed", "topic_pattern", pattern)
		}
		return nil
	}, nil
}

// decodeWire returns one consumer's own event, decoded from the bytes the
// publish serialised.
//
// The caller has already decoded those same bytes once — that is how it holds
// `received` — so a failure here is unreachable.
//
// That reason is stated because "unreachable" without one is the label that
// tells the next auditor to stop looking, and it is itself a claim about our
// own code — the population nobody audits. The coordination twin has a branch
// that read identically and was NOT unreachable: it decodes bytes it had only
// ENCODED, and json.Number("1e1000") marshals fine and then fails to unmarshal
// into float64. Ours differs in exactly the way that matters, and it was
// probed rather than argued: json.Number("1e1000"), a malformed number,
// invalid UTF-8 and a 1MB string were each pushed through this path, and every
// one either failed at ENCODE — so Publish refuses and nothing reaches here —
// or decoded identically on both passes.
//
// One mechanism could still have made two decodes of one input disagree, and
// it is worth naming because the byte-identity argument does not cover it:
// Event.UnmarshalJSON is not a pure function of its input, since it consults a
// global type registry to decode the typed body. It cannot open a failure here
// regardless — the registry-dependent branch swallows its own error and falls
// through to the envelope representation rather than returning one — so the
// only error paths left are the two envelope decodes, which are byte-pure.
//
// Falling back to a clone keeps mail flowing rather than dropping a delivery
// if it ever stops being unreachable; it is a worse copy (it shares the
// payload pointer), not no copy. Nothing is logged because this runs under the
// broker lock.
func decodeWire(wire []byte, received *events.Event) *events.Event {
	var out events.Event
	if err := json.Unmarshal(wire, &out); err != nil {
		return received.Clone()
	}
	return &out
}

func dispatchStreams(
	ctx context.Context, streams []*streamSub, topic string, wire []byte, received *events.Event,
) {
	for _, s := range streams {
		// topics.Match, never a private copy: two backends with their own
		// matchers can disagree about which events a dashboard sees, and a
		// subscriber quietly receiving too few looks exactly like a quiet
		// company.
		if !topics.Match(s.pattern, topic) {
			continue
		}
		deliverStream(ctx, s, topic, decodeWire(wire, received))
	}
}

func deliverStream(ctx context.Context, s *streamSub, topic string, ev *events.Event) {
	// One browser tab closing mid-frame must not take the publisher down
	// with it, nor stop the next subscriber being served.
	defer func() {
		if r := recover(); r != nil {
			queue.LogStreamHandlerPanic(log, topic, ev, r)
		}
	}()
	s.handler(ctx, topic, ev)
}

// --- delivery gates -------------------------------------------------------

// PauseDelivery stops dispatching new events to handlers while leaving Publish
// working, so in-flight handlers can emit their terminal events. One-way: once
// paused, the node is shutting down.
func (q *Queue) PauseDelivery(context.Context) error {
	q.broker.mu.Lock()
	if q.paused {
		q.broker.mu.Unlock()
		return nil
	}
	q.paused = true
	q.broker.mu.Unlock()

	log.Info("memory_event_queue_paused")
	return nil
}

// PauseTopic takes one reason's hold on this client's attachment.
//
// It does NOT create the subscription, and used to. A hold is process-local
// state about one attachment; on a real broker it gates local dispatch and
// creates nothing remote, so a twin that minted a subscription here was
// modelling something no backend does — and being kinder than the broker is
// this file's cardinal sin, because the twin certifies what it models.
//
// It also had a consequence with real blast radius. DeleteSubscription exists
// so a decommissioned role's inbox cannot accumulate undeliverable events for
// ever; measured, a stray PauseTopic afterwards — a sandbox gate or a config
// shed racing the decommission — RESURRECTED the subscription, which then
// retained every event published to that topic for a role that no longer
// existed. Exactly the accumulation the verb exists to prevent.
//
// The old reason (a gate taken before anything attaches should still retain the
// mail it gates) was answering a question the contract already answers the
// other way: publishing to a topic with no subscription drops the event, and
// EnsureSubscription is how a caller asks for retention.
func (q *Queue) PauseTopic(_ context.Context, topic, group, reason string) error {
	key := subKey{topic, group}
	reason = normalizeReason(reason)
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return ErrNotStarted
	}
	held, ok := q.pauses[key]
	if !ok {
		held = map[string]struct{}{}
		q.pauses[key] = held
	}
	held[reason] = struct{}{}
	q.broker.mu.Unlock()

	log.Info("memory_topic_paused", "topic", topic, "group", group, "reason", reason)
	return nil
}

// ResumeTopic releases one reason's hold, flushing when none remain.
//
// A topic stays paused while ANY reason holds it. Two independent subsystems
// gate the same inbox — the sandbox busy gate and the config-divergence shed —
// and with one flat hold the sandbox resuming its own run would un-gate a node
// serving a stale company, on a completely ordinary code path.
func (q *Queue) ResumeTopic(ctx context.Context, topic, group, reason string) error {
	key := subKey{topic, group}
	reason = normalizeReason(reason)
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return ErrNotStarted
	}
	held := q.pauses[key]
	if len(held) == 0 {
		q.broker.mu.Unlock()
		return nil
	}
	delete(held, reason)
	if len(held) > 0 {
		remaining := slices.Sorted(maps.Keys(held))
		q.broker.mu.Unlock()
		log.InfoContext(ctx, "memory_topic_still_paused", "topic", topic, "group", group,
			"released", reason, "held_by", remaining)
		return nil
	}
	delete(q.pauses, key)
	sub := q.broker.subs[key]
	backlog := 0
	if sub != nil {
		backlog = len(sub.mail)
	}
	q.broker.mu.Unlock()

	log.InfoContext(ctx, "memory_topic_resumed", "topic", topic, "group", group,
		"reason", reason, "backlog", backlog)
	if sub != nil {
		q.broker.drain(ctx, sub, false)
	}
	return nil
}

// normalizeReason gives an unnamed hold a name. An empty reason would make
// every anonymous caller share one hold, so the first release would un-gate
// the rest — the exact failure reason-scoping exists to prevent.
func normalizeReason(reason string) string {
	if reason == "" {
		return "default"
	}
	return reason
}

// --- drain protocol -------------------------------------------------------

// InFlightCount reports handler invocations currently mid-flight — the number
// an operator watches converge to zero during a drain.
func (q *Queue) InFlightCount() int { return q.inFlight.Count() }

// WaitForHandlers waits for in-flight handlers, returning how many were still
// running when the wait ended. Zero means a clean drain; non-zero means the
// timeout expired, which is not an error — the caller owns any "too long"
// policy. A non-positive timeout waits until the handlers finish or ctx ends.
func (q *Queue) WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error) {
	return q.inFlight.Wait(ctx, timeout)
}

// enterHandlerLocked and exitHandlerLocked keep their names and their
// broker-lock callers; the counting itself no longer needs that lock.
func (q *Queue) enterHandlerLocked() { q.inFlight.Begin() }
func (q *Queue) exitHandlerLocked()  { q.inFlight.End() }

// --- inspection -----------------------------------------------------------
//
// These answer questions the EventQueue contract deliberately does not, and
// that the properties seat ownership rests on cannot be asserted without:
// what mail is a subscription holding, which subsystem is gating a seat, which
// seats is this node serving. They are exported for the same reason the Python
// twin exports them — a test must not have to read the backend's registry to
// ask an operationally central question.

// Backlog reports the events a subscription retains and has not delivered.
func (q *Queue) Backlog(topic, group string) []*events.Event {
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	sub, ok := q.broker.subs[subKey{topic, group}]
	if !ok {
		return nil
	}
	return slices.Clone(sub.mail)
}

// DeadLetters reports the events a subscription gave up on. They live at
// topics.DeadLetter(topic, group), deliberately OUTSIDE the crewlet.* subject
// space so the dashboard's crewlet.events.> stream cannot re-surface a poison
// event as if it were live.
func (q *Queue) DeadLetters(topic, group string) []*events.Event {
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	return slices.Clone(q.broker.deadLetters[topics.DeadLetter(topic, group)])
}

// Attachments reports every (topic, group) pair THIS client is attached to.
//
// Scoped to the client, not the broker: a peer's attachment is its own
// business, and a fleet-wide answer would make "attached to exactly the seats I
// own" untestable — the assertion that catches a double-consumer split-brain.
func (q *Queue) Attachments() [][2]string {
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	var out [][2]string
	for key, sub := range q.broker.subs {
		if len(sub.membersOf(q)) > 0 {
			out = append(out, [2]string{key.topic, key.group})
		}
	}
	slices.SortFunc(out, func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	})
	return out
}

// PauseHolds reports the reasons currently holding THIS client's attachment
// paused, sorted. Which subsystem is gating a seat is the first question when
// one goes quiet.
func (q *Queue) PauseHolds(topic, group string) []string {
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	return slices.Sorted(maps.Keys(q.pauses[subKey{topic, group}]))
}

// Quiescing reports whether THIS client has stopped taking work on a
// subscription.
//
// Distinct from a pause: a pause is reason-counted and released by the
// subsystem that took it, while a quiesce is cleared by detaching or by
// attaching again. Both look identical from outside — a seat that is owned,
// attached and silent — which is why each is separately observable.
func (q *Queue) Quiescing(topic, group string) bool {
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	_, held := q.quiescing[subKey{topic, group}]
	return held
}

// History reports every event published through this broker, bounded by
// WithMaxHistory. A debugging affordance, not a mailbox.
func (q *Queue) History() []*events.Event {
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	return slices.Clone(q.broker.history)
}
