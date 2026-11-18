package jetstream

import (
	"log/slog"

	"github.com/nats-io/nats.go"
)

// clientErrors is every connection's asynchronous error handler: what the NATS
// client reports outside any call (a slow consumer, a permissions violation,
// an authorization refusal on a reconnect) logged through the engine's logger.
// Without it the client's default writes to raw stderr with fmt: outside
// logging.file, at no level, a bare line in a JSON stream, and the only place
// the refusal before a close for good ([connectionLoss]) was said.
//
// WARN: the client has worked around each or will retry; the close that
// cannot be worked around is [lossWatch.closed]'s, at ERROR. A method rather
// than a closure, for [reconnectWatch]'s reason: a case can tell the handler
// a connection was given is this one.
type clientErrors struct {
	log *slog.Logger
}

// reported logs one asynchronous error and names no subscription: the client
// rewrites a JetStream ordered consumer's Subject on every reset (nats.go's
// resetOrderedConsumer) under a lock it does not export, and reports that
// consumer's errors on the same subscription from its dispatcher. Every
// coordination-store walk is such a consumer (a KV watch), so reading the
// field is a data race, and would name only a random inbox. Where the subject
// matters the error carries it: a permissions violation names the subject.
func (c clientErrors) reported(_ *nats.Conn, _ *nats.Subscription, err error) {
	if err == nil {
		return
	}
	c.log.Warn("jetstream_client_error", "error", err.Error())
}
