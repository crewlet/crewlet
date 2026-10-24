package jetstream

import (
	"log/slog"

	"github.com/nats-io/nats.go"
)

// clientErrors is every connection's asynchronous error handler: what the NATS
// CLIENT reports outside any call — a slow consumer, a permissions violation,
// an authorization refusal on a reconnect, a subscription the server would not
// take — logged through the engine's own logger.
//
// # Why it has to be installed
//
// A connection given none gets the client's default, which writes the error
// straight to the process's stderr with fmt — `nats: authorization violation on
// connection [5]` — past every handler the engine configured. So the line was
// never in `logging.file`, never in the format a shipper parses (a raw line in
// the middle of a `-log-format json` stream), never at a level, and never on
// the record of a node that turned stderr off. The one place it reached is the
// one place nothing read it: the authorization refusal that comes before a
// connection is closed for good ([connectionLoss]) was announced there and
// nowhere else.
//
// WARN, because every one of these is the client telling the node something
// went wrong that it has already worked around or is about to retry — the
// close that cannot be worked around is [lossWatch.closed]'s, at ERROR.
//
// A METHOD rather than a closure, for [reconnectWatch]'s reason: a case can then
// tell the handler a connection was given is this one.
type clientErrors struct {
	log *slog.Logger
}

// reported logs one asynchronous error.
//
// # Nothing is read off the subscription
//
// The client hands the subscription an error came from, and the one field that
// would name it, Subject, is the client's to REWRITE under a lock it does not
// export. A JetStream ordered consumer moves its subscription to a fresh inbox
// on every reset (nats.go's resetOrderedConsumer), and every coordination-store
// walk is one — internal/coord/kv reads a bucket through a KV watch, which the
// jetstream package builds on the client's ordered push consumer, not on its
// pull API. The client also reports that consumer's own trouble on that very
// subscription — a consumer that went quiet while the connection was down, a
// recreation that failed — from its asynchronous dispatcher, while a heartbeat
// check on another goroutine may be resetting it again. Read here, the field
// is a data race; and what it would name on such a subscription is a random
// inbox, which tells an operator nothing: the client's own default names the
// consumer's filter instead, from a field it can lock and this package cannot
// reach. So the line names no subscription. Where the subscription is the
// point, the error says so itself: a permissions violation carries the subject
// the server refused.
func (c clientErrors) reported(_ *nats.Conn, _ *nats.Subscription, err error) {
	if err == nil {
		return
	}
	c.log.Warn("jetstream_client_error", "error", err.Error())
}
