package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Errors this backend reports. Callers branch on these; everything else is
// wrapped transport failure.
var (
	// ErrSubject means a subject belongs to no stream — a publish that
	// would land where nobody consumes.
	ErrSubject = errors.New("jetstream: unroutable subject")
	// ErrClosed means the queue has been stopped.
	ErrClosed = errors.New("jetstream: queue closed")
)

// Config configures a JetStream-backed queue.
type Config struct {
	// URL is the NATS server to dial. Empty runs an EMBEDDED server in
	// this process, which is the default single-binary topology; a URL
	// points at an external cluster. Same client code either way.
	URL string

	// StoreDir is where an embedded server persists its streams. Empty
	// selects an in-memory embedded server, which is what tests want and
	// what a stateless ingress-only node can use.
	StoreDir string

	// ClusterName, ClusterURLs and ClusterPort configure an embedded
	// server that joins peers, which is the fleet topology: every node
	// embeds a member of one cluster and streams replicate between them.
	ClusterName string
	ClusterURLs []string
	ClusterPort int

	// Replicas is the stream replica count. 1 for solo; 3 for a fleet,
	// where it is what makes a publish quorum-durable before Publish
	// returns.
	Replicas int

	// EventRetention bounds the audit/event stream.
	EventRetention time.Duration

	// Credentials authenticates against an external server.
	Credentials string
	Token       string
}

// Queue is the JetStream implementation of queue.EventQueue.
type Queue struct {
	log *slog.Logger
	cfg Config

	embedded *embeddedServer // nil when dialling an external server
	nc       *nats.Conn
	js       jetstream.JetStream

	// attachments holds one entry per (topic, group) this process
	// consumes, keyed by the PAIR. Keying by topic alone was a real bug
	// in the Python engine twice over: a pause hold outlived its
	// attachment so a re-attaching node was silently deaf, and every
	// group on a shared subject got gated together.
	mu          sync.Mutex
	attachments map[attachKey]*attachment
	listeners   []queue.PublishListener
	paused      bool
	closed      bool

	// inFlight counts handler invocations, which is the number an
	// operator watches converge to zero during a drain.
	inFlight  sync.WaitGroup
	inFlightN atomicCounter
}

type attachKey struct{ topic, group string }

// Open connects (or starts an embedded server) and prepares the streams.
func Open(ctx context.Context, cfg Config) (*Queue, error) {
	q := &Queue{
		log:         logging.Get("queue.jetstream"),
		cfg:         cfg,
		attachments: map[attachKey]*attachment{},
	}

	var err error
	if cfg.URL == "" {
		if q.embedded, err = startEmbedded(cfg); err != nil {
			return nil, fmt.Errorf("start embedded nats: %w", err)
		}
		q.nc, err = q.embedded.connect()
	} else {
		q.nc, err = dial(cfg)
	}
	if err != nil {
		q.shutdownServer()
		return nil, fmt.Errorf("connect nats: %w", err)
	}

	if q.js, err = jetstream.New(q.nc); err != nil {
		q.nc.Close()
		q.shutdownServer()
		return nil, fmt.Errorf("open jetstream: %w", err)
	}
	if err := q.ensureStreams(ctx); err != nil {
		q.nc.Close()
		q.shutdownServer()
		return nil, err
	}
	return q, nil
}

func (q *Queue) ensureStreams(ctx context.Context) error {
	replicas := max(q.cfg.Replicas, 1)
	storage := jetstream.FileStorage
	if q.cfg.StoreDir == "" && q.cfg.URL == "" {
		storage = jetstream.MemoryStorage
	}
	for _, spec := range streamSpecs(q.cfg.EventRetention) {
		_, err := q.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name:      spec.name,
			Subjects:  spec.subjects,
			Retention: spec.retention,
			Storage:   storage,
			Replicas:  replicas,
			MaxAge:    spec.maxAge,
		})
		if err != nil {
			return fmt.Errorf("ensure stream %s: %w", spec.name, err)
		}
	}
	return nil
}

// Backend names this backend for operator display. Nothing may branch on it.
func (q *Queue) Backend() string {
	if q.embedded != nil {
		return "jetstream-embedded"
	}
	return "jetstream"
}

// Start satisfies the contract; streams and connection are established in
// Open, so there is nothing deferred to do here.
func (q *Queue) Start(context.Context) error { return nil }

// Publish sends an event, returning only once the broker has acknowledged
// it — with replicas configured, that acknowledgement is a quorum commit, so
// "published" means "survives losing this node".
func (q *Queue) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if topic == "" {
		// An empty subject is what an unroutable handle produces. It must
		// not become a real subject nobody reads.
		return fmt.Errorf("%w: empty topic", ErrSubject)
	}
	if _, err := streamForSubject(topic); err != nil {
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
	if _, err := q.js.Publish(ctx, topic, data); err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}

	// Listeners run inline, exactly as the Python engine's do: the event
	// store's writer is one, and it must see the event as part of the
	// publish rather than racing a consumer. Their failures are logged and
	// never propagate — telemetry must not be able to fail a publish.
	for _, l := range listeners {
		q.callListener(ctx, l, topic, ev)
	}
	return nil
}

func (q *Queue) callListener(ctx context.Context, l queue.PublishListener, topic string, ev *events.Event) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("publish_listener_panicked", "topic", topic, "panic", r)
		}
	}()
	l(ctx, topic, ev)
}

// AddPublishListener registers a listener called inline on every publish.
func (q *Queue) AddPublishListener(l queue.PublishListener) {
	if l == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.listeners = append(q.listeners, l)
}

// EnsureSubscription creates the durable consumer with NO consumer attached,
// positioned at the earliest message.
//
// This is the operation a seat's mailbox is built on, and on JetStream it is
// an ordinary API call taking about a millisecond — no admin endpoint, no
// risk of stealing a live peer's traffic. It must run before anything
// publishes to the subject: the agent and notification streams use interest
// retention, so a message published to a subject no consumer covers is
// dropped, which is the contract's stated behaviour rather than a surprise.
func (q *Queue) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	stream, err := streamForSubject(topic)
	if err != nil {
		return false, err
	}
	name := consumerName(topic, group)

	// Reporting whether THIS call created it is part of the contract, so
	// look first rather than inferring from an upsert.
	_, getErr := q.js.Consumer(ctx, stream, name)
	existed := getErr == nil
	if getErr != nil && !errors.Is(getErr, jetstream.ErrConsumerNotFound) {
		return false, fmt.Errorf("inspect consumer %s: %w", name, getErr)
	}

	if _, err := q.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: topic,
		AckPolicy:     jetstream.AckExplicitPolicy,
		// Earliest, always. A consumer created at "latest" exists and
		// still discards everything published before its first
		// consumer — which is the whole failure this call prevents.
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
	}); err != nil {
		return false, fmt.Errorf("ensure consumer %s: %w", name, err)
	}
	return !existed, nil
}

// DeleteSubscription destroys the durable consumer and the mail it retains.
//
// Deliberately does not require a local attachment: decommissioning a role
// must not depend on which node happened to be running the seat.
func (q *Queue) DeleteSubscription(ctx context.Context, topic, group string) (bool, error) {
	if _, err := q.Detach(ctx, topic, group); err != nil {
		return false, err
	}
	stream, err := streamForSubject(topic)
	if err != nil {
		return false, err
	}
	err = q.js.DeleteConsumer(ctx, stream, consumerName(topic, group))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		// Deleting an absent subscription is success, not an error: the
		// caller's intent is "this must not exist", and it does not.
		return false, nil
	default:
		return false, fmt.Errorf("delete consumer for %s/%s: %w", topic, group, err)
	}
}

// InFlightCount reports handler invocations currently mid-flight.
func (q *Queue) InFlightCount() int { return q.inFlightN.load() }

// PauseDelivery stops dispatching new events while leaving Publish working,
// so in-flight handlers can still emit their terminal events. One-way: once
// paused, the engine is shutting down.
func (q *Queue) PauseDelivery(context.Context) error {
	q.mu.Lock()
	q.paused = true
	atts := q.attachmentsLocked()
	q.mu.Unlock()
	for _, a := range atts {
		a.setPaused(true)
	}
	return nil
}

// WaitForHandlers waits for in-flight handlers, returning how many were
// still running when the wait ended. Zero means a clean drain; non-zero
// means the timeout expired, which is not an error — the caller owns any
// "too long" policy.
func (q *Queue) WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error) {
	done := make(chan struct{})
	go func() {
		q.inFlight.Wait()
		close(done)
	}()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-done:
		return 0, nil
	case <-timer:
		return q.inFlightN.load(), nil
	case <-ctx.Done():
		return q.inFlightN.load(), ctx.Err()
	}
}

// Stop closes every attachment and the connection, and shuts down the
// embedded server when this process is running one.
func (q *Queue) Stop(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	atts := q.attachmentsLocked()
	q.attachments = map[attachKey]*attachment{}
	q.mu.Unlock()

	// Stop attachments concurrently so one unresponsive consumer bounds
	// this step instead of stranding every attachment behind it.
	var wg sync.WaitGroup
	for _, a := range atts {
		wg.Add(1)
		go func(a *attachment) {
			defer wg.Done()
			a.stop()
		}(a)
	}
	wg.Wait()

	if q.nc != nil {
		q.nc.Close()
	}
	q.shutdownServer()
	return nil
}

func (q *Queue) shutdownServer() {
	if q.embedded != nil {
		q.embedded.shutdown()
		q.embedded = nil
	}
}

func (q *Queue) attachmentsLocked() []*attachment {
	out := make([]*attachment, 0, len(q.attachments))
	for _, a := range q.attachments {
		out = append(out, a)
	}
	return out
}

// deadLetter publishes a message the delivery budget has exhausted, to a
// subject outside the crewlet.* space so the dashboard's crewlet.events.>
// stream cannot resurface poison as live traffic.
func (q *Queue) deadLetter(ctx context.Context, topic, group string, data []byte) {
	if _, err := q.js.Publish(ctx, topics.DeadLetter(topic, group), data); err != nil {
		q.log.Error("dead_letter_failed", "topic", topic, "group", group, "error", err.Error())
	}
}

var _ queue.EventQueue = (*Queue)(nil)
