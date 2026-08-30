package pulsar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	pulsarlog "github.com/apache/pulsar-client-go/pulsar/log"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Queue is the Pulsar implementation of queue.EventQueue.
type Queue struct {
	log    *slog.Logger
	cfg    Config
	client pulsar.Client
	admin  BrokerAdmin

	// producers are cached per topic: creating one is a network round trip
	// and the engine publishes to the same few hundred subjects for the
	// life of the process.
	prodMu    sync.Mutex
	producers map[string]pulsar.Producer

	// attachments holds one entry per (topic, group) this process
	// consumes, keyed by the PAIR. Keying by topic alone was a real bug in
	// twice over: a pause hold outlived its attachment
	// so a re-attaching node was silently deaf, and every group on a
	// shared subject got gated together.
	mu          sync.Mutex
	attachments map[attachKey][]*attachment
	holds       map[attachKey]map[string]struct{}
	streams     []*streamSub
	listeners   []queue.PublishListener
	paused      bool
	closed      bool

	// inFlight counts handler invocations, which is the number an operator
	// watches converge to zero during a drain.
	inFlight queue.Inflight
}

type attachKey struct{ topic, group string }

// Open validates a configuration, connects a client and prepares the admin
// endpoint.
//
// Connecting here rather than in Start mirrors the JetStream backend: Start
// then has nothing deferred to do, and a broker that is unreachable fails the
// call that named it rather than the first publish minutes later.
func Open(_ context.Context, cfg Config) (*Queue, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	admin, err := newRESTAdmin(cfg)
	if err != nil {
		return nil, err
	}
	client, err := pulsar.NewClient(clientOptions(cfg))
	if err != nil {
		admin.Close()
		return nil, fmt.Errorf("connect pulsar %s: %w", cfg.URL, err)
	}
	return newQueueOn(cfg, client, admin), nil
}

func newQueueOn(cfg Config, client pulsar.Client, admin BrokerAdmin) *Queue {
	return &Queue{
		log:         logging.Get("queue.pulsar"),
		cfg:         cfg,
		client:      client,
		admin:       admin,
		producers:   map[string]pulsar.Producer{},
		attachments: map[attachKey][]*attachment{},
		holds:       map[attachKey]map[string]struct{}{},
	}
}

func clientOptions(cfg Config) pulsar.ClientOptions {
	opts := pulsar.ClientOptions{
		URL: cfg.URL,
		// The client's own logs go through the engine's structured
		// logger, at WARN and above. It is chatty at INFO — one line per
		// connection lifecycle event per broker — and an operator reading
		// engine logs is not reading them to learn about TCP.
		Logger: pulsarlog.NewLoggerWithSlog(atLeast(
			logging.Get("queue.pulsar.client"), slog.LevelWarn)),
	}
	if cfg.Token != "" {
		// With token authentication the broker identifies this engine by
		// the token's `sub` claim; namespace-level grants then confine it
		// to its own tenant, which is what makes one estate safe to share.
		opts.Authentication = pulsar.NewAuthenticationToken(cfg.Token)
	}
	if cfg.TLSTrustCertsPath != "" {
		opts.TLSTrustCertsFilePath = cfg.TLSTrustCertsPath
	}
	return opts
}

// atLeast returns a logger that drops records below min.
//
// slog has no "raise the level of an existing logger" operation, and the
// alternative — handing the client the engine's root logger — floods an
// operator's log with connection lifecycle at INFO. So the client gets its own
// logger with a floor pinned at WARNING.
func atLeast(l *slog.Logger, min slog.Level) *slog.Logger {
	return slog.New(&floorHandler{inner: l.Handler(), min: min})
}

type floorHandler struct {
	inner slog.Handler
	min   slog.Level
}

func (h *floorHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.inner.Enabled(ctx, l)
}

func (h *floorHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < h.min {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *floorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &floorHandler{inner: h.inner.WithAttrs(attrs), min: h.min}
}

func (h *floorHandler) WithGroup(name string) slog.Handler {
	return &floorHandler{inner: h.inner.WithGroup(name), min: h.min}
}

// Backend names this backend for operator display. Nothing may branch on it.
func (q *Queue) Backend() string { return "pulsar" }

// Start satisfies the contract; the connection and the admin endpoint are
// established in Open, so there is nothing deferred to do here.
func (q *Queue) Start(context.Context) error { return nil }

// isClosed reports whether Stop has run.
//
// Every verb in the contract asks this, so it is one method rather than a
// hand-rolled lock in each: the eleven verbs disagreed about the answer for
// exactly as long as each one owned its own copy of the question.
func (q *Queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// InFlightCount reports handler invocations currently mid-flight.
func (q *Queue) InFlightCount() int { return q.inFlight.Count() }

// AddPublishListener registers a listener called inline on every publish.
func (q *Queue) AddPublishListener(l queue.PublishListener) {
	if l == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.listeners = append(q.listeners, l)
}

// --- publishing -----------------------------------------------------------

// Publish sends an event, returning only once the broker has acknowledged it.
// Callers rely on "published means durable" — the completion ledger, the
// event store and every terminal event a draining handler emits.
func (q *Queue) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if err := checkSubject(topic); err != nil {
		return err
	}
	q.mu.Lock()
	closed := q.closed
	listeners := append([]queue.PublishListener(nil), q.listeners...)
	q.mu.Unlock()
	if closed {
		return ErrClosed
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", ev.Type, err)
	}
	producer, err := q.producer(topic)
	if err != nil {
		return err
	}
	if err := q.send(ctx, producer, topic, data); err != nil {
		return err
	}

	// Listeners run inline, and must: the event
	// store's writer is one, and it must see the event as part of the
	// publish rather than racing a consumer. Their failures are logged and
	// never propagate — telemetry must not be able to fail a publish.
	for _, l := range listeners {
		q.callListener(ctx, l, topic, ev)
	}
	return nil
}

// send publishes one payload, retrying the transient failures.
//
// A momentarily slow or unreachable broker must neither drop the event nor
// park the caller: the producer refuses to block when its queue is full, so a
// stalled broker surfaces HERE as an error this loop backs off on. The caller
// is usually a handler holding a seat's only turn.
func (q *Queue) send(ctx context.Context, producer pulsar.Producer, topic string, data []byte) error {
	var last error
	for attempt := range publishAttempts {
		_, err := producer.Send(ctx, &pulsar.ProducerMessage{Payload: data})
		if err == nil {
			return nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
		if attempt == publishAttempts-1 {
			break
		}
		q.log.Warn("publish_retry", "topic", topic, "attempt", attempt+1, "error", err.Error())
		if sleep(ctx, publishRetryBase*(1<<attempt)) != nil {
			break
		}
	}
	return fmt.Errorf("publish %s after %d attempts: %w", topic, publishAttempts, last)
}

// producer returns the cached producer for a subject, creating one on first
// use.
func (q *Queue) producer(topic string) (pulsar.Producer, error) {
	full := q.cfg.fullTopic(topic)
	q.prodMu.Lock()
	defer q.prodMu.Unlock()
	if p, ok := q.producers[full]; ok {
		return p, nil
	}
	p, err := q.client.CreateProducer(pulsar.ProducerOptions{
		Topic: full,
		// BATCHING OFF, and this is not a throughput preference.
		//
		// With batching on, several engine events share one broker entry.
		// Acking one of them individually then needs batch-index
		// acknowledgment (a broker-side opt-in), and NAKing one
		// redelivers the WHOLE entry — which would make "a failing
		// partition redelivers only itself" false, silently, for any
		// batch whose conversations happened to be co-batched. The
		// per-partition ack that inbox coalescing rests on requires one
		// message per entry.
		DisableBatching: true,
		// Never park the caller when the producer queue backs up behind a
		// slow broker: fail here and let send() back off. A blocked
		// publish is a blocked handler, and a blocked handler is a seat
		// that has stopped answering.
		DisableBlockIfQueueFull: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create producer for %s: %w", topic, err)
	}
	q.producers[full] = p
	return p, nil
}

func (q *Queue) callListener(ctx context.Context, l queue.PublishListener, topic string, ev *events.Event) {
	defer func() {
		if r := recover(); r != nil {
			queue.LogListenerPanic(q.log, topic, ev, r)
		}
	}()
	l(ctx, topic, ev)
}

// --- subscription existence ----------------------------------------------

// EnsureSubscription creates the durable subscription if absent, with NO
// consumer attached, positioned at the earliest message.
//
// Routed through the admin API rather than through the client — see the
// restAdmin doc comment for the measurement that decided it. This is the
// operation a seat's mailbox is built on, and it must run before anything
// publishes to the subject: Pulsar deletes a message published to a topic no
// subscription covers, which is the contract's stated behaviour rather than a
// surprise.
func (q *Queue) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	// THE ADMIN ENDPOINT IS A SEPARATE CONNECTION, and that is exactly why
	// this check has to be here: closing the Pulsar client does not close
	// the REST client, so without it a stopped queue would happily create a
	// durable subscription on the broker that nothing in this process will
	// ever attach to or delete. See queue.EventQueue's Stop.
	if q.isClosed() {
		return false, ErrClosed
	}
	return q.admin.EnsureSubscription(ctx, topic, group)
}

// DeleteSubscription destroys the subscription and the mail it retains.
//
// Detaches locally first — a broker refuses to delete a subscription that
// still has a connected consumer — but does not REQUIRE a local one:
// decommissioning a role must not depend on which node happened to run the
// seat. The local wait is bounded, so a wedged handler turns into a legible
// broker error rather than a hang.
func (q *Queue) DeleteSubscription(ctx context.Context, topic, group string) (bool, error) {
	// The local detach is also this verb's LIFECYCLE GATE, and the order is
	// what makes that safe: it refuses a stopped queue before the admin call
	// — which runs over a REST client Stop does not close — can destroy a
	// subscription and the mail it retains. A second flag check here would
	// be a guard no test could distinguish from this one.
	if _, err := q.detach(topic, group, detachGrace); err != nil {
		return false, err
	}
	return q.admin.DeleteSubscription(ctx, topic, group)
}

// Subscriptions lists the subscription names on a subject. Operator- and
// test-facing: "does this seat have a mailbox at all" has no answer in the
// consumer-facing contract, and it is the first question when a seat goes
// quiet.
func (q *Queue) Subscriptions(ctx context.Context, topic string) ([]string, error) {
	return q.admin.Subscriptions(ctx, topic)
}

// --- drain and shutdown ---------------------------------------------------

// PauseDelivery stops dispatching new events while leaving Publish working,
// so in-flight handlers can still emit their terminal events. One-way: once
// paused, the engine is shutting down.
func (q *Queue) PauseDelivery(context.Context) error {
	q.mu.Lock()
	q.paused = true
	atts := q.allAttachmentsLocked()
	q.mu.Unlock()
	for _, a := range atts {
		a.paused.Store(true)
	}
	return nil
}

// WaitForHandlers waits for in-flight handlers, returning how many were still
// running when the wait ended. Zero means a clean drain; non-zero means the
// timeout expired, which is not an error — the caller owns any "too long"
// policy.
func (q *Queue) WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error) {
	return q.inFlight.Wait(ctx, timeout)
}

// Stop closes every attachment, every stream, the producers and the client.
//
// Closing the consumers is also what RETURNS this node's unacked mail: a
// graceful close hands it to whoever attaches next, in order and at
// redeliveryCount 0 (measured). A node leaving is therefore free, which
// is the property a rolling deploy rests on.
func (q *Queue) Stop(context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	atts := q.allAttachmentsLocked()
	q.attachments = map[attachKey][]*attachment{}
	// Pause holds are process-local state about attachments that no longer
	// exist. Carrying them past a stop would leave a restarted queue
	// silently deaf on subjects nothing is holding any more.
	q.holds = map[attachKey]map[string]struct{}{}
	streams := q.streams
	q.streams = nil
	q.mu.Unlock()

	// Concurrently, so one unresponsive consumer bounds this step instead
	// of stranding every attachment behind it.
	var wg sync.WaitGroup
	for _, a := range atts {
		wg.Add(1)
		go func(a *attachment) {
			defer wg.Done()
			a.stop()
			a.wait(stopGrace)
		}(a)
	}
	for _, s := range streams {
		wg.Add(1)
		go func(s *streamSub) {
			defer wg.Done()
			s.close()
		}(s)
	}
	wg.Wait()

	q.prodMu.Lock()
	for _, p := range q.producers {
		p.Close()
	}
	q.producers = map[string]pulsar.Producer{}
	q.prodMu.Unlock()

	if q.client != nil {
		q.client.Close()
	}
	if q.admin != nil {
		q.admin.Close()
	}
	return nil
}

func (q *Queue) allAttachmentsLocked() []*attachment {
	out := make([]*attachment, 0, len(q.attachments))
	for _, group := range q.attachments {
		out = append(out, group...)
	}
	return out
}

// deadLetterTopic is the fully-qualified topic a subscription's poison
// messages are routed to.
//
// Deliberately OUTSIDE the crewlet.* subject space (topics.DeadLetter): the
// dashboard streams crewlet.events.> and Pulsar's own default DLQ name
// (<topic>-<sub>-DLQ) would sit inside it, resurfacing a poison event as live
// traffic on every screen.
func (q *Queue) deadLetterTopic(topic, group string) string {
	return q.cfg.fullTopic(topics.DeadLetter(topic, group))
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

var _ queue.EventQueue = (*Queue)(nil)
