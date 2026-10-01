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

// reported logs one asynchronous error, naming the subscription it came from
// where it came from one.
func (c clientErrors) reported(_ *nats.Conn, sub *nats.Subscription, err error) {
	if err == nil {
		return
	}
	if sub != nil {
		// READ WITHOUT THE SUBSCRIPTION'S LOCK, which is unexported. The
		// one writer of Subject after a subscription is made is the
		// client's legacy push ordered consumer, which re-points it at a
		// new inbox on a reset; this tree reaches JetStream through the
		// jetstream package's pull API alone, whose subscriptions keep
		// the subject they were made with.
		c.log.Warn("jetstream_client_error", "subject", sub.Subject, "error", err.Error())
		return
	}
	c.log.Warn("jetstream_client_error", "error", err.Error())
}
