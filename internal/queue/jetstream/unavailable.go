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

// ErrConnectionLost means NATS has closed this queue's own connection for good
// — see [connectionLoss] for the reasons the client reaches that on its own —
// so nothing this queue does will reach the broker again in this process, and
// the node stops for it.
//
// It wraps [queue.ErrNotLive], and not [queue.ErrUnavailable], for that
// reason: no wait clears it, and a queue whose connection is gone consumes
// nothing, which is the fact a seat release reads ErrNotLive as. It is also
// not [ErrClosed], which says this queue's own Stop closed it.
var ErrConnectionLost = fmt.Errorf("jetstream: the broker connection is closed for good: %w",
	queue.ErrNotLive)

// brokerFailed is err as a verb of this queue answers a call to the broker that
// failed: marked with the contract's word for what the failure says about the
// broker — wrapped, never replaced, so the client's own message still reads in
// a log and errors.Is on the client's sentinels still answers — and returned
// unchanged when it says nothing about the broker at all.
//
// # The answers, in the order they are asked
//
//   - ALREADY MARKED: returned as it is. A verb built on another verb (an
//     attach is an EnsureSubscription first) passes its inner answer up, and
//     a second mark would only repeat the first.
//   - THIS QUEUE WAS STOPPED while the call was out: [ErrClosed], which is
//     the answer every verb gives a stopped queue at its door, so a verb that
//     raced the Stop answers as one that arrived after it.
//   - THE CONNECTION IS CLOSED FOR GOOD: [ErrConnectionLost].
//   - THE CONNECTION IS DOWN — reconnecting, or disconnected between
//     attempts: [queue.ErrUnavailable], WHATEVER the call met on the way. The
//     connection is why nothing answered, so a deadline the caller set that
//     ran out while it was down is the connection's doing and is marked as
//     well, the one place a caller's own deadline is ([queue.ErrUnavailable]
//     says why).
//   - THE BROKER DID NOT ANSWER on a connection that is up:
//     [queue.ErrUnavailable] — [jsprovision.Unanswered], the rule this
//     package's provisioning already reads silence by (a client timeout, no
//     responders, a deadline the client imposed while the caller was still
//     waiting); a publish no stream leader acknowledged
//     ([jetstream.ErrNoStreamResponse], which is no responders after the
//     client's own retries); and the client's words for a connection it has
//     just found down ([reconnecting]).
//   - ANYTHING ELSE is returned unmarked: the broker's own answer — a stream
//     that does not exist, a limit the account has reached — or a failure of
//     this node's, neither of which waiting changes.
//
// # Why the connection's state is read and not only the error
//
// Because the error a call meets while the connection is down is whatever the
// wait it was in ended with: a publish buffered for the reconnect waits for an
// acknowledgement and ends on a deadline, which read alone is a caller's
// deadline on a broker that was merely slow. The state is read AFTER the
// failure, so a connection that came back in between reads as up and the
// error is judged on its own words — a deadline the caller set then stays the
// context's error, which every caller already reads as a wait that ran out.
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

// reachable is nil while this queue's connection is up, and otherwise the
// marked answer a verb that must not wait for it gives — [Queue.Ask], which
// refuses rather than write a request into the reconnect buffer.
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

// connectionDown is [queue.ErrUnavailable] naming the state this queue's
// connection is in, so a log line says the broker was not reached rather than
// only that nothing answered.
func connectionDown(status nats.Status) error {
	return fmt.Errorf("%w: this node's connection to it is %s", queue.ErrUnavailable,
		strings.ToLower(status.String()))
}

// reconnecting reports the client's own words for a connection it found down
// in the middle of a call: disconnected, reconnecting, a reconnect buffer that
// filled before the connection came back, or a connection the server stopped
// answering pings on — each of which the client follows with a reconnect.
func reconnecting(err error) bool {
	return errors.Is(err, nats.ErrDisconnected) ||
		errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, nats.ErrReconnectBufExceeded) ||
		errors.Is(err, nats.ErrStaleConnection)
}
