package jetstream

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
)

// carriesTheContract refuses a connection held to a max_payload below
// [queue.MaxPayloadBytes], the largest message a backend of the queue contract
// must carry. [heldToTheContract] asks it of every connection this package
// opens — the queue's own, the coordination store's on an embedded broker, and
// every second connection [Queue.DialOwned] hands a subsystem, a snapshot
// donor streaming mebibyte chunks among them — through the only two dials
// there are ([embeddedServer.connect] and [dial]).
//
// # Why at the connection, before anything is provisioned
//
// This build sends messages that large. An event or a webhook delivery may be
// up to it, and a state-log record at the largest its log declares is exactly
// it: the tracker's, the knowledge base's and the vector changelog's
// declaration is the contract less what a stored record carries beside its
// payload, so one of those records signed and sent is [queue.MaxPayloadBytes]
// on the wire, and every log's stream is provisioned with a max_msg_size its
// largest record fits. The client refuses a message past what its server
// ANNOUNCED, locally, and nothing else ever compared that with what this
// build sends — so a server below the contract took every small write and
// refused the first large one: a node that booted, reported itself healthy and
// served, and then refused a webhook delivery, a long phase's telemetry or a
// state-log record — `record_too_large` — past the server's limit.
// nats-server's default is 1 MiB, which makes that the ordinary shape of an
// external cluster nobody configured for this engine.
// Refused here, where the connection has just authenticated, the same
// misconfiguration is a boot that says which setting, the value the server
// announced and the value this build needs.
//
// # Why the embedded broker is checked as well
//
// It cannot be too small. [embeddedOptions] configures it at exactly the
// contract's number, no Tier A field reaches that setting, every member of an
// embedded cluster is configured by the same function, and it defines no
// account with a limit of its own. The check runs on its connection anyway
// because it is ONE rule over every connection this backend opens, rather than
// a branch on where the broker runs — and because the embedded server's option
// is the kind that is invisible until the day it matters: an edit that dropped
// it would boot every node against nats-server's 1 MiB default and fail only
// the first large write, where it now fails every boot, and the refusal says
// the defect is this build's rather than a setting anybody can change.
//
// # What it cannot see
//
// The member this connection landed on, and only what that member announces.
// A client dialling a cluster reconnects to whichever member answers, so a
// member configured apart from the others is met after boot, and a server
// whose limit is lowered under a running node likewise: both refuse a message
// past their limit when it is written, naming the limit — see
// [reconnectWatch], which says so on the reconnect itself.
func carriesTheContract(nc *nats.Conn, embedded bool) error {
	// NAMED BEFORE THE ROUND TRIP: a connection that drops during it no
	// longer says which member it was talking to, and that member is the
	// one the failure is about.
	server := serverOf(nc, embedded)
	announced, err := announcedMaxPayload(nc)
	if err != nil {
		return fmt.Errorf("jetstream: read the max_payload %s holds this "+
			"connection to: %w", server, err)
	}
	return belowTheContract(server, announced, embedded)
}

// serverOf is how a sentence about nc names its server: the embedded broker as
// this node's own — an in-process connection's URL is the client's default
// address, which names nothing — and an operator's by [connectedServer], which
// carries no credential its URL holds.
func serverOf(nc *nats.Conn, embedded bool) string {
	if embedded {
		return "this node's embedded NATS server"
	}
	return "the NATS server at " + connectedServer(nc)
}

// announcedMaxPayload is the max_payload the server holds this connection to,
// read after one round trip rather than straight off the connect.
//
// THE INFO A CONNECT READS IS THE SERVER'S, and the limit that applies is the
// CONNECTION's: nats-server holds a client to the lowest of its own
// max_payload and the payload limits of the account it signed in to and of its
// user — an account's `limits { max_payload }` in the server's configuration
// file, or the `payload` limit an account's or a user's JWT states in operator
// mode — and announces that only in the INFO it sends once the connection has
// authenticated, straight after the PONG that completes the connect.
// nats.Connect returns on that PONG, so what the connection holds then is
// almost always the server's own figure: read there, a server at 8 MiB whose
// account allowed a quarter of it passed, and the node refused its first
// message past the account's limit instead. One more PING and PONG is the
// point by which the connection has read it, since the server sent it before
// that PONG and the client reads its socket in order.
//
// Bounded by the connection's own handshake budget, because it finishes the
// handshake: the accept budget on an embedded member, [externalHandshake] on
// an operator's server.
func announcedMaxPayload(nc *nats.Conn) (int64, error) {
	if err := nc.FlushTimeout(nc.Opts.Timeout); err != nil {
		return 0, err
	}
	return nc.MaxPayload(), nil
}

// belowTheContract is the refusal of a connection held to announced bytes, or
// nil where that carries the contract.
//
// Two sentences, because the two topologies have opposite remedies: an
// operator's server is fixed in that server's configuration — and in the
// account's, where the account states a limit of its own — while nothing an
// operator sets reaches the embedded broker, so a refusal there sending them to
// stream.url would send them after a knob that does not exist.
//
// server is [serverOf]'s name for the connection's server.
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

// reconnectWatch is every connection's reconnect handler, [reconnectWatch.
// reconnected]: it names a server the connection reconnected to that holds it
// to less than the contract, which is the half of [carriesTheContract] a boot
// cannot reach.
//
// A LOG LINE AND NOT A REFUSAL, because there is nothing left to refuse: the
// node is serving, its seats are leased over this very connection, and closing
// it would turn one misconfigured member into an outage of every write, the
// small ones included. What the line buys is the moment — without it the first
// word about a member configured apart from the one the node booted against
// was a large write's `record_too_large`, which names the server's limit and
// not that the member it came from is the one out of step. The same sentence
// as the boot's, so an operator greps one phrase for both.
//
// A METHOD rather than a closure, so a case can tell the handler a connection
// was given is this one: every method value of one method shares its code,
// while a closure returned by an inlined constructor is a new function at each
// call site.
type reconnectWatch struct {
	log      *slog.Logger
	embedded bool
}

// reconnected reads what the member this connection reached holds it to, and
// logs where that is below the contract.
//
// A round trip that fails says nothing, deliberately: it means the connection
// has gone again, and the next reconnect asks again of whichever member it
// reaches — a line about a member it has already left would name the wrong
// one.
func (w reconnectWatch) reconnected(nc *nats.Conn) {
	// The attribute is the address a reader filters on, and the sentence
	// names it the way the boot's does; both before the round trip, as
	// [carriesTheContract] reads them.
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

// natsSize spells n bytes the way a nats-server configuration file takes it:
// with the MB suffix, which nats-server reads as 2^20, where n is a whole
// number of mebibytes — the contract's is, and that is what an operator types
// — and as a plain byte count where it is not, so the value named is never
// rounded to one that carries less.
func natsSize(n int) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%dMB", n>>20)
	}
	return fmt.Sprint(n)
}
