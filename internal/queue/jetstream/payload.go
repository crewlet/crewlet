package jetstream

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
)

// carriesTheContract refuses a connection held to a max_payload below
// [queue.MaxPayloadBytes], the largest message this build sends: an event, a
// webhook delivery, a state-log record at its log's maximum, an answer on the
// ephemeral verbs. [heldToTheContract] asks it of every connection, through
// the only two dials ([embeddedServer.connect] and [dial]), because the client
// refuses a message past the announced limit only when it is sent: a server at
// nats-server's 1 MiB default booted healthy and failed the first large write.
// The embedded broker, which [embeddedOptions] sets at the contract, is held
// to it too, so an edit dropping that option fails every boot. A member met on
// reconnect is [reconnectWatch]'s; a live reload lowering the limit closes the
// connection or reconnects it (TestALiveReloadOfALowerMaxPayloadClosesAClientForGood),
// and [connectionLoss] stops the node, whose restart meets this check.
func carriesTheContract(nc *nats.Conn, embedded bool) error {
	// Named before the round trip: a connection that drops during it no
	// longer says which member it was talking to.
	server := serverOf(nc, embedded)
	announced, err := announcedMaxPayload(nc)
	if err != nil {
		return fmt.Errorf("jetstream: read the max_payload %s holds this "+
			"connection to: %w", server, err)
	}
	return belowTheContract(server, announced, embedded)
}

// serverOf is how a sentence about nc names its server: the embedded broker as
// this node's own (an in-process URL names nothing), an operator's by
// [connectedServer], which carries no credential its URL holds.
func serverOf(nc *nats.Conn, embedded bool) string {
	if embedded {
		return "this node's embedded NATS server"
	}
	return "the NATS server at " + connectedServer(nc)
}

// announcedMaxPayload is the max_payload the server holds this connection to,
// read after one more round trip: the limit is the lowest of the server's, the
// account's and the user's, announced only in an INFO sent after the PONG that
// nats.Connect returns on, so read straight off the connect it is the server's
// own figure. That INFO precedes the flush's PONG on the socket, which the
// client reads in order. Bounded by the connection's handshake budget, since it
// finishes the handshake (the listener's when embedded, [externalHandshake]
// otherwise).
func announcedMaxPayload(nc *nats.Conn) (int64, error) {
	if err := nc.FlushTimeout(nc.Opts.Timeout); err != nil {
		return 0, err
	}
	return nc.MaxPayload(), nil
}

// belowTheContract is the refusal of a connection held to announced bytes, or
// nil where that carries the contract. server is [serverOf]'s name for it.
//
// Two sentences, because the remedies are opposite: an operator's server is
// fixed in its (and its account's) configuration, while no setting reaches the
// embedded broker, so sending that operator to stream.url names no real knob.
func belowTheContract(server string, announced int64, embedded bool) error {
	if announced >= queue.MaxPayloadBytes {
		return nil
	}
	if embedded {
		return fmt.Errorf("jetstream: %s announces a max_payload of %d bytes, "+
			"below the %d bytes this build sends at most (queue.MaxPayloadBytes): "+
			"the engine configures that server itself and no setting reaches it, so "+
			"this build is broken rather than misconfigured",
			server, announced, queue.MaxPayloadBytes)
	}
	return fmt.Errorf("jetstream: %s announces a max_payload of "+
		"%d bytes for this node's connection, below the %d bytes this build needs: "+
		"that is the largest message it sends — an event, a webhook delivery, a "+
		"state-log record at the largest its log declares — and a server that "+
		"carries less takes every smaller write and refuses the first large one. "+
		"Set max_payload: %s in the configuration of every server stream.url "+
		"reaches, and wherever the account this node signs in to, or its user, "+
		"states a payload limit of its own, raise that too: a server holds a "+
		"connection to the lowest of them",
		server, announced, queue.MaxPayloadBytes, natsSize(queue.MaxPayloadBytes))
}

// heldToTheContract hands nc back where it carries the contract, and closes
// it and answers the refusal where it does not.
func heldToTheContract(nc *nats.Conn, embedded bool) (*nats.Conn, error) {
	if err := carriesTheContract(nc, embedded); err != nil {
		nc.Close()
		return nil, err
	}
	return nc, nil
}

// reconnectWatch is every connection's reconnect handler,
// [reconnectWatch.reconnected]: it names a reconnected-to server holding the
// connection below the contract, the half of [carriesTheContract] a boot
// cannot reach.
//
// A LOG LINE AND NOT A REFUSAL: the node is serving with its seats leased over
// this connection, and closing it would turn one misconfigured member into an
// outage of every write. The line names the member out of step at once, in the
// boot's sentence so one grep finds both. A METHOD rather than a closure, so a
// case can tell a connection was given this handler: method values share code,
// while an inlined constructor's closure is a new function per call site.
type reconnectWatch struct {
	log      *slog.Logger
	embedded bool
}

// reconnected logs where the member this connection reached holds it below
// the contract. A failed round trip says nothing, deliberately: the connection
// has gone again, and the next reconnect asks whichever member it reaches.
func (w reconnectWatch) reconnected(nc *nats.Conn) {
	// Both named before the round trip, as [carriesTheContract] does; the
	// attribute is the address a reader filters on.
	address := "embedded"
	if !w.embedded {
		address = connectedServer(nc)
	}
	server := serverOf(nc, w.embedded)
	announced, err := announcedMaxPayload(nc)
	if err != nil {
		return
	}
	if err := belowTheContract(server, announced, w.embedded); err != nil {
		w.log.Error("jetstream_max_payload_below_contract",
			"server", address, "max_payload", announced,
			"needed", queue.MaxPayloadBytes, "error", err)
	}
}

// natsSize spells n bytes as a nats-server configuration file takes it: with
// the MB suffix (read as 2^20) where n is whole mebibytes, as the contract is,
// and as a plain byte count otherwise, so the value is never rounded down.
func natsSize(n int) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%dMB", n>>20)
	}
	return fmt.Sprint(n)
}
