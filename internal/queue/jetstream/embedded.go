package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
)

// The budgets for an embedded server ACCEPTING CONNECTIONS.
//
// Not to be confused with [clusterReadyTimeout] below, which bounds a
// different and later wait: this one is "the listener is up", that one is
// "this member's JetStream has caught up with the metadata group". A boot
// waits for both in that order, and they fail for different reasons.
//
// # Why two, and why the clustered one is larger
//
// A SOLO server has one thing to do before it accepts: recover its own file
// store. A CLUSTERED member does that while also standing up its route
// listener and starting to gossip with peers that are themselves booting —
// so on a host bringing several members up at once it is competing for the
// same disk and the same scheduler. One number for both is wrong for one of
// them.
//
// # And why both are generous
//
// The asymmetry decides it. Failing here fails the WHOLE BOOT, so a budget
// that is too short turns a busy host into a node that refuses to start and
// then works on the retry — which during a rolling restart is how one slow
// member takes out the restart. Too long only means a genuinely broken
// server is reported as broken later, and the wait is CANCELLABLE, so a
// person who has seen enough gets their prompt back immediately.
//
// The clustered case shared the solo 30 seconds and flaked under the full
// race suite with several clusters forming at once — which is a smaller
// version of exactly the production case it has to survive.
const (
	// acceptTimeout bounds a solo server: its own file store, and nothing
	// else.
	acceptTimeout = 30 * time.Second

	// clusterAcceptTimeout bounds a member that is also standing up a
	// route listener and gossiping with peers mid-boot.
	clusterAcceptTimeout = 2 * time.Minute
)

// acceptBudget is how long this server gets to accept connections.
func acceptBudget(clustered bool) time.Duration {
	if clustered {
		return clusterAcceptTimeout
	}
	return acceptTimeout
}

const (
	// clusterReadyTimeout bounds waiting for a clustered member's JetStream
	// to become current. Measured at ~8s for a quiet three-member cluster
	// on a loopback; the generous multiple is because this is a one-time
	// boot cost and the alternative to waiting is a node that provisions
	// into a leaderless metadata group and blocks with no diagnosis.
	clusterReadyTimeout = 60 * time.Second

	// clusterReadyPoll is how often that wait re-checks. Short enough that
	// a fast local cluster is not held back by the poll itself, and it runs
	// at most a few hundred times.
	clusterReadyPoll = 100 * time.Millisecond
)

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
	// scratch is a private directory this server owns and deletes on
	// shutdown, used when no StoreDir was configured. See startEmbedded.
	scratch string
	// clustered records whether this member has peers, which decides
	// whether anything has to wait for a metadata leader.
	clustered bool
}

// Server is an embedded broker that outlives any one client of it.
//
// The split matters for the same reason the in-memory twin is a broker plus
// N clients rather than one object: a node stopping must not take the broker
// down for its peers. Open is the production convenience that owns both,
// because the solo topology genuinely is one process owning one broker.
type Server struct {
	embedded *embeddedServer
	cfg      Config
}

// StartServer starts an embedded broker without any client attached.
//
// The context bounds the START, not the server's life: a broker that is up
// outlives it, and stopping one is [Server.Shutdown].
func StartServer(ctx context.Context, cfg Config) (*Server, error) {
	e, err := startEmbedded(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("start embedded nats: %w", err)
	}
	return &Server{embedded: e, cfg: cfg}, nil
}

// Client connects a new queue to this server. The client does NOT own the
// server: stopping it leaves the broker and every peer running.
func (s *Server) Client(ctx context.Context) (*Queue, error) {
	return newQueueOn(ctx, s.cfg, s.embedded, false)
}

// Conn returns a NATS connection to this server, for subsystems that ride
// the same broker without going through the queue contract — the KV
// coordination backend is the one that matters, and sharing the broker is
// what makes the single-binary topology one service rather than two.
//
// The caller owns the connection and must close it.
func (s *Server) Conn() (*nats.Conn, error) {
	if s.embedded == nil {
		return nil, errors.New("jetstream: server is shut down")
	}
	return s.embedded.connect()
}

// Shutdown stops the broker. Every client of it should be stopped first.
func (s *Server) Shutdown() {
	if s.embedded != nil {
		s.embedded.shutdown()
		s.embedded = nil
	}
}

// embeddedOptions is the nats-server option set a Config produces, together
// with the scratch directory the caller owns and must remove.
//
// SEPARATED FROM THE START for the same reason dialOptions is, and it earns it
// twice over here: two of these options have no observable effect until the
// day they matter. A wrong MaxPayload surfaces as one oversized publish
// failing in production, and a wrong SyncAlways surfaces only as acked data
// missing after a host loses power — which no test can stage.
func embeddedOptions(cfg Config) (*server.Options, string, error) {
	clustered := cfg.ClusterName != "" || len(cfg.ClusterURLs) > 0 || cfg.ClusterPort != 0
	name := cfg.ServerName
	if name == "" {
		if clustered {
			// Refused rather than generated. A generated name is unique,
			// which is half the requirement — the other half is that it
			// survives a restart, and a name minted at boot silently
			// orphans this member's replicas every time the process
			// comes back. See Config.ServerName.
			return nil, "", errors.New("jetstream: ServerName is required for a clustered embedded server")
		}
		name = "crewlet"
	}
	opts := &server.Options{
		ServerName: name,
		JetStream:  true,
		Port:       -1,
		// No listener in the solo case. This is a security property as
		// much as a convenience: an embedded broker with no socket
		// cannot be reached by anything but this process.
		DontListen: len(cfg.ClusterURLs) == 0 && cfg.ClusterPort == 0,
		StoreDir:   cfg.StoreDir,

		// The transport's own ceiling, from the contract's number rather
		// than nats-server's 1 MiB default — see queue.MaxPayloadBytes for
		// what that default cost. A SERVER limit rather than a per-stream
		// one, deliberately: the client reads it off the server's INFO and
		// refuses an oversized publish locally, which is what turns "this
		// delivery is too big" into an error the publisher can report
		// instead of a connection the broker closes underneath it.
		MaxPayload: queue.MaxPayloadBytes,

		// WHAT "PERSISTED" MEANS, and the one place it is decided.
		//
		// EventQueue.Publish promises that a returned publish is durable.
		// Left unset, nats-server's file store flushes on a 2-MINUTE
		// background interval (server/filestore.go defaultSyncInterval),
		// so an acked publish survives this process crashing and does NOT
		// survive the host losing power — up to two minutes of acked
		// events, and with them the coordination KV that rides this same
		// server: the completion ledger, claimed scheduled fires, a
		// detached (billed) sandbox run, a just-written secret. Losing
		// those replays a trigger already worked and forgets a box still
		// running.
		//
		// So a SOLO member fsyncs every write. There is no peer to recover
		// from, which makes its disk the only copy, and the cost is
		// affordable at this engine's write rate: events are paced by LLM
		// turns and lease renewals by a 15-second heartbeat, not by a
		// throughput workload.
		//
		// A CLUSTERED member does not, because the quorum IS the
		// durability: a publish returns once a majority holds it, so the
		// host that loses power loses nothing its peers cannot replay.
		// nats-server draws the same line itself — it enables the
		// async-flush path for a replicated stream only when SyncAlways is
		// off (server/filestore.go) — so forcing it on every member would
		// spend an fsync per write to protect a copy two others already
		// have.
		SyncAlways: cfg.Replicas <= 1,

		// THE ENGINE OWNS THE PROCESS SIGNALS. Left false, Server.Start
		// installs its own SIGINT/SIGTERM handler, which shuts the
		// broker down and then calls os.Exit(0) — from a library, inside
		// a process that is in the middle of something.
		//
		// Measured, end to end: SIGTERM to a solo node ran the drain and
		// the process vanished part way through Backends.Close, exit 0,
		// with the seat release done and nothing after it. A slower
		// drain — one with a turn still running, which is the case the
		// drain exists for — loses that turn and the lease release with
		// it, so a peer waits out the full TTL for seats a graceful
		// shutdown was meant to hand back immediately.
		//
		// The engine's own handler is the one that must run, and it must
		// run to completion.
		NoSigs: true,
	}
	var scratch string
	if opts.StoreDir == "" {
		// An in-memory server. Streams are memory-backed too (see
		// ensureStreams), which suits tests and a stateless
		// ingress-only node that materializes nothing.
		opts.JetStreamMaxStore = -1

		// It still needs somewhere to put its own metadata, and left
		// empty nats-server picks a FIXED default path. That path is
		// shared by every crewlet process and every test binary on the
		// machine, so servers meant to be isolated recover each other's
		// streams: measured as KV epochs arriving at 136 in a suite that
		// had claimed a seat five times, and as a nil-pointer panic
		// inside the server's own stream recovery when two runs
		// overlapped. A private directory, removed on shutdown, is what
		// "no store configured" was always meant to mean.
		dir, err := os.MkdirTemp("", "crewlet-jetstream-")
		if err != nil {
			return nil, "", fmt.Errorf("embedded jetstream scratch dir: %w", err)
		}
		opts.StoreDir, scratch = dir, dir
	}
	if cfg.ClusterName != "" {
		opts.Cluster = server.ClusterOpts{Name: cfg.ClusterName, Port: cfg.ClusterPort}
		opts.Routes = server.RoutesFromStr(joinURLs(cfg.ClusterURLs))
	}
	return opts, scratch, nil
}

func startEmbedded(ctx context.Context, cfg Config) (*embeddedServer, error) {
	opts, scratch, err := embeddedOptions(cfg)
	if err != nil {
		return nil, err
	}
	clustered := opts.Cluster.Name != "" || len(opts.Routes) > 0 || opts.Cluster.Port != 0

	ns, err := server.NewServer(opts)
	if err != nil {
		removeScratch(scratch)
		return nil, fmt.Errorf("configure embedded server: %w", err)
	}
	// BEFORE Start, or the boot is the one stretch that logs nowhere —
	// which is where stream recovery and a failed store directory report.
	// Trace is never enabled: it is a line per protocol message, and the
	// engine publishes every event through here.
	natsLog, natsDebug := newNATSLogger(ctx)
	ns.SetLoggerV2(natsLog, natsDebug, false, false)
	go ns.Start()

	// THE WAIT IS CANCELLABLE, which is the whole reason this function takes
	// a context. ReadyForConnections blocks for up to its budget with no way
	// to interrupt it, so a Ctrl-C during a slow cold start — a large file
	// store recovering its streams — used to sit out the full 30 seconds
	// before the process could begin its drain.
	budget := acceptBudget(clustered)
	ready := make(chan bool, 1)
	go func() { ready <- ns.ReadyForConnections(budget) }()
	select {
	case ok := <-ready:
		if !ok {
			ns.Shutdown()
			removeScratch(scratch)
			// THE BUDGET IS IN THE MESSAGE, and whether this member was
			// waiting on peers: "did not become ready" alone sends an
			// operator to the disk when a route was the problem.
			return nil, fmt.Errorf(
				"embedded nats server did not become ready within %v (clustered: %v). "+
					"A clustered member also waits for its routes to dial and for "+
					"the metadata group to elect a leader, so check that its peers "+
					"in stream.cluster.routes are reachable", budget, clustered)
		}
	case <-ctx.Done():
		// Shutdown makes the in-flight ReadyForConnections return, so the
		// goroutine above finishes into its buffered channel rather than
		// leaking.
		ns.Shutdown()
		removeScratch(scratch)
		return nil, fmt.Errorf("start embedded nats: %w", ctx.Err())
	}
	return &embeddedServer{
		ns: ns, inProcess: opts.DontListen, scratch: scratch, clustered: clustered,
	}, nil
}

// awaitClusterReady waits for this member's JetStream to catch up with the
// cluster's metadata group.
//
// Accepting connections is NOT the same as being able to serve JetStream. A
// clustered member answers its client port as soon as it is listening, while
// the metadata group takes seconds to elect a leader — measured at around
// eight on a quiet three-member cluster — and until it has one, creating a
// replicated stream BLOCKS rather than failing. A node that provisioned at
// boot therefore hung with nothing to diagnose, looking exactly like a broker
// that is up and ignoring you.
//
// A no-op for a solo member and for an external URL: solo has no metadata
// group to join, and an external cluster is somebody else's to have made
// ready before pointing an engine at it.
func (e *embeddedServer) awaitClusterReady(ctx context.Context) error {
	if e == nil || !e.clustered {
		return nil
	}
	deadline := time.Now().Add(clusterReadyTimeout)
	for time.Now().Before(deadline) {
		if e.ns.JetStreamIsCurrent() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for jetstream cluster %q: %w",
				e.ns.ClusterName(), ctx.Err())
		case <-time.After(clusterReadyPoll):
		}
	}
	if e.ns.JetStreamIsCurrent() {
		return nil
	}
	return fmt.Errorf("embedded nats server %q joined no jetstream cluster within %s",
		e.ns.Name(), clusterReadyTimeout)
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
	removeScratch(e.scratch)
}

// removeScratch deletes a scratch store directory. A failure is logged rather
// than returned: the broker is already down, and a leftover temp directory is
// not a reason to fail a shutdown the caller cannot retry.
func removeScratch(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		logging.Get("queue.jetstream").Warn("embedded_scratch_not_removed", "dir", dir, "error", err)
	}
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

// Dial opens a NATS connection to an external server with this package's own
// reconnect policy.
//
// Exported so a caller outside this package that needs a plain connection to
// the same estate gets the same reconnect-forever behaviour. Reimplementing
// the option list there would give two places to disagree about how long a
// node survives a broker blip — and the whole point of that policy is that it
// keeps its seats through one.
//
// The caller owns the connection and must close it.
func Dial(cfg Config) (*nats.Conn, error) { return dial(cfg) }

func dial(cfg Config) (*nats.Conn, error) {
	opts, err := dialOptions(cfg)
	if err != nil {
		return nil, err
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.URL, err)
	}
	return nc, nil
}

// dialOptions is the option list, separated from the dial so a test can
// assert what a config produces without a broker to connect to.
//
// The alternative was asserting on the connection, which needs a real TLS
// server with `verify: true` and a generated certificate chain — and would
// then be testing that NATS honours its own documented option rather than
// that this package passes it.
func dialOptions(cfg Config) ([]nats.Option, error) {
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
	// THE FILES ARE CHECKED HERE rather than left for nats.Connect. Its
	// own failure for an unreadable certificate arrives as a dial error,
	// which reads as "the broker is unreachable" — so an operator goes
	// looking at the network for a path that is simply not there. The
	// caller names which estate this was (engine: stream / engine:
	// coordination), so this only has to name the file and the field.
	if cfg.TLS.CA != "" {
		if _, err := os.Stat(cfg.TLS.CA); err != nil {
			return nil, fmt.Errorf("tls.ca %s: %w", cfg.TLS.CA, err)
		}
		opts = append(opts, nats.RootCAs(cfg.TLS.CA))
	}
	if cfg.TLS.Cert != "" {
		for field, path := range map[string]string{
			"tls.cert": cfg.TLS.Cert, "tls.key": cfg.TLS.Key,
		} {
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("%s %s: %w", field, path, err)
			}
		}
		opts = append(opts, nats.ClientCert(cfg.TLS.Cert, cfg.TLS.Key))
	}
	return opts, nil
}

// SubscribeStream creates an ephemeral per-caller broadcast subscription.
//
// Unlike a durable group subscription, EVERY stream subscriber receives
// every matching event — this is the dashboard's live feed, not a work
// queue. Best-effort by design: there is no ack, a slow consumer misses
// messages rather than holding them, and the authoritative path for anything
// that matters polls its own source.
func (q *Queue) SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error) {
	// The one broker-touching verb that does not go through streamFor, so
	// it carries its own copy of the same check — see there.
	if q.isClosed() {
		return nil, ErrClosed
	}
	spec, err := specForPattern(pattern, q.cfg.EventRetention)
	if err != nil {
		return nil, err
	}
	if err = q.ensureStream(ctx, spec); err != nil {
		return nil, err
	}
	stream := spec.name
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

	// stopped gates dispatch rather than relying on the consume context
	// alone: a message can already be in the client's callback path when
	// Unsubscribe lands, and a dashboard that keeps receiving after it
	// unsubscribed is a subscription that never really ended.
	var stopped atomic.Bool
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		if stopped.Load() {
			return
		}
		var ev events.Event
		if decodeErr := json.Unmarshal(msg.Data(), &ev); decodeErr != nil {
			q.log.Warn("stream_decode_failed", "pattern", pattern, "error", decodeErr.Error())
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
			stopped.Store(true)
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
			queue.LogStreamHandlerPanic(q.log, subject, ev, r)
		}
	}()
	h(ctx, subject, ev)
}
