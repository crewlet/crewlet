package jetstream

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Coordination is a connection to the fleet's broker for a process that needs
// its coordination store and nothing else: no queue, no stream provisioned, no
// consumer. It is how a `crewlet` command reaches the fleet's store while this
// node's engine is stopped.
//
// # Why not a queue
//
// [Open] and [Server.Client] build a queue, and a queue PROVISIONS: it creates
// the engine's streams before it hands itself over ([Queue.ensureStreams]). A
// command that only reads and writes the coordination store has no business
// creating the event and mailbox streams on a store whose engine has never
// booted — the engine's first boot is what does that, under the budgets it
// sizes them by.
type Coordination struct {
	conn  *nats.Conn
	js    jetstream.JetStream
	owned *embeddedServer
	where string
}

// OpenCoordination reaches the broker cfg describes for its coordination store
// alone: it dials an external one ([Config.URL]), or starts this node's
// embedded one on its store directory, and waits for whatever that broker needs
// before a store on it can answer — a leaf's link, a clustered member's quorum.
//
// AN EMBEDDED ONE IS STARTED HERE AND OWNED HERE: [Coordination.Close] stops
// it. It takes the store directory's lock like every other start
// ([StoreDirLock]), so it is refused while the engine — or another command —
// runs a broker there, and the engine is refused while it runs. What listeners
// it opens is cfg's to say: a caller that is not the node clears the leaf
// listener, or the fleet's leaves would rejoin a broker that is about to go
// away.
//
// UNWATCHED: the connection's close for good is the caller's to notice, as it
// is for every connection [Server.Conn] hands out — a short-lived command
// meets it as its next request failing, which is the right place.
func OpenCoordination(ctx context.Context, cfg Config) (*Coordination, error) {
	if cfg.URL != "" {
		conn, err := dial(cfg, watched{})
		if err != nil {
			return nil, fmt.Errorf("connect nats: %w", err)
		}
		js, err := cfg.API().Client(conn)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("open jetstream: %w", err)
		}
		return &Coordination{conn: conn, js: js,
			where: "the external broker at " + serverAddresses(cfg.URL)}, nil
	}

	e, err := startEmbedded(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("start embedded nats: %w", err)
	}
	c := &Coordination{owned: e, where: "this node's broker on " + cfg.StoreDir}
	if c.conn, err = e.connect(watched{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	if c.js, err = cfg.API().Client(c.conn); err != nil {
		c.Close()
		return nil, fmt.Errorf("open jetstream: %w", err)
	}
	// THE SAME WAITS A QUEUE MAKES before it provisions anything, for the
	// same reason: a request sent before a leaf's link is up, or before a
	// clustered member's metadata group has a leader, is not refused but
	// dropped, and costs a whole ask term before anything asks again.
	if err := e.awaitLeafReady(ctx, c.js); err != nil {
		c.Close()
		return nil, err
	}
	if err := e.awaitClusterReady(ctx, c.js, cfg.Replicas); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// JetStream is the client the coordination store is built over, in the API
// the broker speaks ([Config.API]).
func (c *Coordination) JetStream() jetstream.JetStream { return c.js }

// Where names the broker this reached, for a reader deciding whether it is
// the one they meant: the external broker's addresses — never stream.url
// itself, which may carry a password — or this node's store directory.
func (c *Coordination) Where() string { return c.where }

// Close closes the connection and stops the broker this started, if it
// started one. Safe on nil and safe twice.
func (c *Coordination) Close() {
	if c == nil {
		return
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	if c.owned != nil {
		c.owned.shutdown()
		c.owned = nil
	}
}
