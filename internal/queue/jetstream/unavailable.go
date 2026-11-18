package jetstream

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
)

// ErrConnectionLost means NATS has closed this queue's connection for good
// ([connectionLoss] has why), so nothing reaches the broker again in this
// process and the node stops for it. It wraps [queue.ErrNotLive], not
// [queue.ErrUnavailable]: no wait clears it, and a queue without a connection
// consumes nothing, which is what a seat release reads ErrNotLive as. It is
// not [ErrClosed], which says this queue's own Stop closed it.
var ErrConnectionLost = fmt.Errorf("jetstream: the broker connection is closed for good: %w",
	queue.ErrNotLive)

// brokerFailed marks err, a failed call to the broker, with the contract's word
// for what it says about the broker (package doc, "What a broker that does not
// answer looks like from above"): wrapped, never replaced, so the client's
// message and its sentinels still read. In order: an already marked error as
// it is; [ErrClosed] when Stop raced the call; [ErrConnectionLost] when the
// connection is closed for good; [queue.ErrUnavailable] while it is down,
// whatever the call met, a caller's deadline included (the connection is why
// nothing answered); [queue.ErrUnavailable] for silence on an up connection
// ([jsprovision.Unanswered], [jetstream.ErrNoStreamResponse], [reconnecting]);
// anything else (the broker's own refusal, this node's fault) unmarked. The
// state is read AFTER the failure, so a connection back in between leaves a
// caller's deadline the context's error, not an outage.
func (q *Queue) brokerFailed(ctx context.Context, err error) error {
	switch {
	case err == nil, errors.Is(err, queue.ErrNotLive), errors.Is(err, queue.ErrUnavailable),
		errors.Is(err, queue.ErrTooLarge):
		return err
	case q.isClosed():
		return fmt.Errorf("%w: %w", ErrClosed, err)
	case q.nc == nil:
		return err
	}
	switch status := q.nc.Status(); {
	case status == nats.CLOSED, errors.Is(err, nats.ErrConnectionClosed):
		return fmt.Errorf("%w: %w", ErrConnectionLost, err)
	case status != nats.CONNECTED:
		return fmt.Errorf("%w: %w", connectionDown(status), err)
	case jsprovision.Unanswered(ctx, err), errors.Is(err, jetstream.ErrNoStreamResponse),
		reconnecting(err):
		return fmt.Errorf("%w: %w", queue.ErrUnavailable, err)
	}
	return err
}

// reachable is nil while the connection is up, else the marked refusal of a
// verb that must not wait for it: [Queue.Ask], which refuses rather than write
// a request into the reconnect buffer.
func (q *Queue) reachable() error {
	if q.nc == nil {
		return nil
	}
	switch status := q.nc.Status(); status {
	case nats.CONNECTED:
		return nil
	case nats.CLOSED:
		return ErrConnectionLost
	default:
		return connectionDown(status)
	}
}

// connectionDown is [queue.ErrUnavailable] naming the connection's state, so a
// log says the broker was not reached rather than only that nothing answered.
func connectionDown(status nats.Status) error {
	return fmt.Errorf("%w: this node's connection to it is %s", queue.ErrUnavailable,
		strings.ToLower(status.String()))
}

// reconnecting reports the client's words for a connection it found down
// mid-call, each of which it follows with a reconnect.
func reconnecting(err error) bool {
	return errors.Is(err, nats.ErrDisconnected) ||
		errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, nats.ErrReconnectBufExceeded) ||
		errors.Is(err, nats.ErrStaleConnection)
}
