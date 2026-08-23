package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat"
)

// Backends are the two infrastructure slots a node runs on.
//
// TWO SLOTS, chosen independently and validated together: the STREAM carries
// events, and COORDINATION carries seat ownership. The combinations that make
// sense are enumerated in config's topology validation — a Pulsar stream has no
// compare-and-set, so it cannot also be the coordination store, and a
// multi-node fleet cannot coordinate locally.
//
// Both are opened here and closed together, because a node holding one without
// the other is a node that can hear work it may not do, or hold seats it cannot
// serve.
type Backends struct {
	Queue queue.EventQueue
	Coord coord.Backend

	// stopServer shuts down an embedded broker this node started. Nil when
	// the stream is external — a node must never take down a broker it
	// merely dialled.
	stopServer func()

	// conn is the NATS connection the coordination store rides when it
	// shares the stream's broker. Held so Close can release it; the store
	// does not own it, because on an embedded topology the SERVER owns the
	// lifetime and closing the connection from underneath it would take
	// the stream down with the leases.
	conn *nats.Conn
}

// Close releases both slots, in the reverse order of acquisition.
//
// THE ORDER IS THE POINT and each step depends on the one before:
//
//  1. Stop the QUEUE. That closes every attachment and waits briefly for
//     in-flight handlers, so consumers release their unacked messages
//     cleanly instead of having them time out. Skipping it and going
//     straight to the server shutdown works, in the sense that the process
//     exits — and leaves every prefetched message to wait out the broker's
//     full ack timeout before a peer can have it.
//
//     NOT COVERED BY A TEST IN THIS PACKAGE, and it is worth saying so
//     rather than implying otherwise: the whole effect is on a PEER's
//     handoff latency, and in one process a shut-down server kills the
//     connection either way. What would cover it is the fleet suite —
//     two nodes, one broker, measuring how long a successor waits for a
//     departed node's messages.
//
//  2. Close the COORDINATION connection, if the store rides one. After the
//     queue, because a node that released its broker while still holding
//     leases looks alive to its peers: renewals keep succeeding against a
//     store it can no longer reach work through, and its seats stay
//     unclaimable for a full TTL.
//
//  3. Shut down the embedded SERVER, if this node started one. Last,
//     because everything above it is a client of it.
//
//     Also not covered here, and for a plainer reason: a solo embedded
//     server runs with DontListen and binds no port at all, so there is no
//     socket to probe after the fact. A clustered one does bind, but a
//     single member never reaches JetStream quorum — measured, it waits
//     the full 60 s and fails — so a one-node probe cannot be stood up.
//
// A node that merely DIALLED an external broker never reaches step 3: it
// must not take down a broker its peers are using.
func (b *Backends) Close(ctx context.Context) {
	if b.Queue != nil {
		if err := b.Queue.Stop(ctx); err != nil {
			log.Warn("queue_stop_failed", "error", err)
		}
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	if b.stopServer != nil {
		b.stopServer()
		b.stopServer = nil
	}
}

// OpenBackends builds both slots from Tier A config.
//
// It does NOT re-validate the topology: config.Bootstrap.Validate already
// refuses the incoherent combinations, and duplicating those rules here would
// give an operator two places to read and two chances to disagree. What this
// adds is the construction, and one rule validation cannot express — an
// embedded-KV coordination store rides the stream's own NATS connection, so the
// two slots are not independent at runtime even though they are in config.
func OpenBackends(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	if b == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	switch b.Stream.Type {
	case config.StreamPulsar:
		return openPulsar(ctx, b)
	case config.StreamNATS, config.StreamEmbedded, "":
		return openNATS(ctx, b)
	default:
		return nil, fmt.Errorf("engine: unknown stream type %q", b.Stream.Type)
	}
}

// openNATS builds a JetStream stream, embedded or external, and the
// coordination store that may ride its connection.
func openNATS(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	cfg := jetstream.Config{
		URL:         b.Stream.URL,
		StoreDir:    b.Stream.StoreDir,
		ClusterName: b.Stream.Cluster.Name,
		ClusterURLs: b.Stream.Cluster.Peers,
		ClusterPort: b.Stream.Cluster.Port,
		ServerName:  b.Node.ID,
		Replicas:    b.Stream.Replicas,
		Credentials: b.Stream.Credentials,
		Token:       b.Stream.Token,
	}
	if b.Stream.EventRetentionHours > 0 {
		cfg.EventRetention = time.Duration(b.Stream.EventRetentionHours * float64(time.Hour))
	}

	server, err := jetstream.StartServer(cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: stream: %w", err)
	}
	q, err := server.Client(ctx)
	if err != nil {
		server.Shutdown()
		return nil, fmt.Errorf("engine: stream client: %w", err)
	}
	out := &Backends{Queue: q, stopServer: server.Shutdown}

	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		out.Coord = coordmem.New()
		return out, nil
	}

	// The coordination store rides the STREAM'S OWN connection. A second
	// dial would work and would be worse: two connections to one broker
	// fail independently, so a node could hold live leases over a
	// connection that still works while the one carrying its inbox has
	// dropped — alive to its peers, deaf to its work.
	conn, err := server.Conn()
	if err != nil {
		out.Close(ctx)
		return nil, fmt.Errorf("engine: coordination connection: %w", err)
	}
	store, err := kv.Open(ctx, conn, kv.Config{
		TTL:      leaseTTL(b),
		Replicas: b.Stream.Replicas,
	})
	if err != nil {
		conn.Close()
		out.Close(ctx)
		return nil, fmt.Errorf("engine: coordination: %w", err)
	}
	out.Coord = store
	out.conn = conn
	return out, nil
}

// openPulsar builds a Pulsar stream.
//
// Coordination is NOT built here: config validation refuses Pulsar with local
// coordination, and Pulsar cannot be the coordination store itself — it has no
// compare-and-set, which is the one primitive a lease needs. On this topology
// the coordination slot is an embedded KV cluster the nodes run among
// themselves, which is a separate NATS connection by construction rather than
// by choice.
func openPulsar(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	// CHECKED BEFORE THE DIAL. Reaching a broker only to discover the
	// topology cannot use it wastes a connection attempt and, worse,
	// reports a network failure when the problem is a configuration one —
	// an operator whose broker happens to be down would read the wrong
	// error and go fix the wrong thing.
	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		return nil, fmt.Errorf("engine: a %q stream needs %q coordination: "+
			"Pulsar has no compare-and-set, so it cannot hold leases",
			config.StreamPulsar, config.CoordinationEmbeddedKV)
	}
	// The embedded KV cluster that carries leases on a Pulsar topology is a
	// separate NATS estate the nodes run among themselves. Naming the gap
	// explicitly is the point: an operator whose Pulsar config is correct
	// should learn that this BUILD is incomplete, not that their config is
	// wrong — and learning it before the dial means they learn it even when
	// the broker is unreachable.
	return nil, fmt.Errorf("engine: a %q stream needs a coordination cluster "+
		"this build does not start yet", config.StreamPulsar)
}

// leaseTTL is the lease TTL both slots must agree on.
//
// Not free to choose independently of the coordination backend: the KV store's
// expiry is bucket-wide, so it can only honour one — and a backend that
// silently accepted a different per-call value would be lying about when a
// lease expires. That is also why the store REQUIRES it rather than defaulting
// one: a bucket created with the wrong expiry is wrong for its lifetime, and a
// silent default would decide that at the moment nobody was looking.
//
// So the default is supplied HERE, from the seat layer's own measured constant
// — 45 s, three heartbeat intervals, which tolerates two consecutive missed
// renewals with a full interval left to recover in. Shorter drops healthy
// nodes' seats on ordinary jitter, and each spurious handoff costs a real MCP
// respawn; longer is time a dead node's seats sit dark.
func leaseTTL(b *config.Bootstrap) time.Duration {
	if b.Coordination.LeaseTTLSeconds <= 0 {
		return seat.SeatLeaseTTL
	}
	return time.Duration(b.Coordination.LeaseTTLSeconds * float64(time.Second))
}
