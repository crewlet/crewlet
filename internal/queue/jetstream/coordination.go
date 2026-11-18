package jetstream

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Coordination is a connection to the fleet's broker for a process that needs
// its coordination store and nothing else: how a `crewlet` command reaches the
// fleet's store while this node's engine is stopped. Not a queue, because a
// queue PROVISIONS its streams ([Queue.ensureStreams]), and a command has no
// business creating them on a store whose engine never booted: the first boot
// does that, under the budgets it sizes them by.
type Coordination struct {
	conn  *nats.Conn
	js    jetstream.JetStream
	owned *embeddedServer
	where string
}

// OpenCoordination reaches the broker cfg describes for its coordination store
// alone: it dials an external one ([Config.URL]) or starts this node's embedded
// one, and waits for what that broker needs before a store on it can answer (a
// leaf's link, a clustered member's quorum).
//
// An embedded one is owned here ([Coordination.Close] stops it) and takes the
// store directory's lock ([StoreDirLock]), so it, the engine and another
// command refuse each other. A caller that is not the node clears the leaf
// listener in cfg, or the fleet's leaves would rejoin a broker about to go
// away. The connection is unwatched, as every [Server.Conn] one is: a command
// meets its close for good as its next request failing.
func OpenCoordination(ctx context.Context, cfg Config) (*Coordination, error) {
	if cfg.URL != "" {
		// A COMMAND'S DIAL, which is no node's data link: unmetered.
		conn, err := dial(cfg, watched{}, nil)
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
	// THE SAME WAITS A QUEUE MAKES: a request sent before a leaf's link is
	// up, or before the metadata group has a leader, is dropped rather than
	// refused, and costs a whole ask term.
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

// Where names the broker this reached: the external broker's addresses (never
// stream.url itself, which may carry a password) or this node's store
// directory.
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
