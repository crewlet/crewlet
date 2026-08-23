package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// readyTimeout bounds waiting for an embedded server to accept connections.
// Generous because a cold file store on a slow disk legitimately takes a
// moment, and failing here fails the whole boot.
const readyTimeout = 30 * time.Second

// embeddedServer is a nats-server running inside this process.
//
// The single-binary topology: no listener, no port, no service to operate.
// In a fleet the same server joins a cluster, so the difference between
// "solo" and "fleet" is cluster configuration rather than a different
// architecture — and the client code above it is identical either way.
type embeddedServer struct {
	ns *server.Server
	// inProcess is true when nothing listens on a socket, which is the
	// solo case: connections are made through an in-memory pipe, so the
	// broker cannot be reached from outside the process at all.
	inProcess bool
}

func startEmbedded(cfg Config) (*embeddedServer, error) {
	opts := &server.Options{
		ServerName: "crewlet",
		JetStream:  true,
		Port:       -1,
		// No listener in the solo case. This is a security property as
		// much as a convenience: an embedded broker with no socket
		// cannot be reached by anything but this process.
		DontListen: len(cfg.ClusterURLs) == 0 && cfg.ClusterPort == 0,
		StoreDir:   cfg.StoreDir,
	}
	if opts.StoreDir == "" {
		// An in-memory server. Streams are memory-backed too (see
		// ensureStreams), which suits tests and a stateless
		// ingress-only node that materializes nothing.
		opts.JetStreamMaxStore = -1
	}
	if cfg.ClusterName != "" {
		opts.Cluster = server.ClusterOpts{Name: cfg.ClusterName, Port: cfg.ClusterPort}
		opts.Routes = server.RoutesFromStr(joinURLs(cfg.ClusterURLs))
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("configure embedded server: %w", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(readyTimeout) {
		ns.Shutdown()
		return nil, errors.New("embedded nats server did not become ready")
	}
	return &embeddedServer{ns: ns, inProcess: opts.DontListen}, nil
}

func (e *embeddedServer) connect() (*nats.Conn, error) {
	if e.inProcess {
		return nats.Connect("", nats.InProcessServer(e.ns))
	}
	return nats.Connect(e.ns.ClientURL())
}

func (e *embeddedServer) shutdown() {
	e.ns.Shutdown()
	e.ns.WaitForShutdown()
}

func joinURLs(urls []string) string {
	out := ""
	for i, u := range urls {
		if i > 0 {
			out += ","
		}
		out += u
	}
	return out
}

func dial(cfg Config) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name("crewlet"),
		// Reconnect forever. A broker blip must not become a node
		// restart: the coordination layer already distinguishes
		// "unreachable" from "not mine", and a node that keeps its
		// seats through a two-second outage is the entire point of that
		// distinction.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	}
	if cfg.Credentials != "" {
		opts = append(opts, nats.UserCredentials(cfg.Credentials))
	}
	if cfg.Token != "" {
		opts = append(opts, nats.Token(cfg.Token))
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.URL, err)
	}
	return nc, nil
}

// SubscribeStream creates an ephemeral per-caller broadcast subscription.
//
// Unlike a durable group subscription, EVERY stream subscriber receives
// every matching event — this is the dashboard's live feed, not a work
// queue. Best-effort by design: there is no ack, a slow consumer misses
// messages rather than holding them, and the authoritative path for anything
// that matters polls its own source.
func (q *Queue) SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error) {
	stream, err := streamForPattern(pattern)
	if err != nil {
		return nil, err
	}
	consCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	cons, err := q.js.CreateOrUpdateConsumer(consCtx, stream, jetstream.ConsumerConfig{
		FilterSubject: pattern,
		// From here on, not from the beginning: a live feed that
		// replayed a month of history on every browser refresh would be
		// unusable, and the durable half of every such question is
		// answered by a query rather than by this stream.
		DeliverPolicy: jetstream.DeliverNewPolicy,
		// No acks: a dashboard must never be able to hold a message.
		AckPolicy: jetstream.AckNonePolicy,
		// Ephemeral — cleaned up by the server shortly after the client
		// goes away, even if the caller never invokes Unsubscribe.
		InactiveThreshold: time.Minute,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create stream consumer on %s: %w", pattern, err)
	}

	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		var ev events.Event
		if err := json.Unmarshal(msg.Data(), &ev); err != nil {
			q.log.Warn("stream_decode_failed", "pattern", pattern, "error", err.Error())
			return
		}
		q.runStreamHandler(consCtx, h, msg.Subject(), &ev)
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("consume stream %s: %w", pattern, err)
	}

	var once sync.Once
	return func(context.Context) error {
		once.Do(func() {
			consumeCtx.Stop()
			cancel()
		})
		return nil
	}, nil
}

// runStreamHandler isolates a stream handler's failure. A dashboard callback
// that panics must not take down the feed for every other subscriber.
func (q *Queue) runStreamHandler(ctx context.Context, h queue.StreamHandler, subject string, ev *events.Event) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("stream_handler_panicked", "subject", subject, "panic", r)
		}
	}()
	h(ctx, subject, ev)
}
