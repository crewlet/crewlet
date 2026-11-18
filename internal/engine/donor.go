package engine

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/statelog"
)

// donorServer is what the donor loop asks of a donor: [statelog.Donor.Serve].
type donorServer interface {
	Serve(ctx context.Context) error
}

// redialPolicy is how soon a donor that stopped serving dials again.
type redialPolicy struct {
	base, ceiling time.Duration
}

// donorRedial doubles from a second to [snapshotSkipRetry].
//
// A SECOND FIRST: the commonest stop is the NATS client closing a serving
// donor's connection (a chunk past a lowered max_payload, a revoked
// credential), and a fresh dial either works at once or is refused, at the
// cost of one handshake.
//
// THE SNAPSHOT LOOP'S RETRY AS THE CEILING, because it answers the same
// question (can this node serve a peer that fell behind): often enough that a
// restarted peer finds a donor within its boot, rarely enough that a node
// unable to serve does not spend its life on it.
var donorRedial = redialPolicy{base: time.Second, ceiling: snapshotSkipRetry}

// after is the loop's arithmetic, apart so it is checked without a clock: the
// failures counted once err has ended a Serve, and the wait before the next.
//
// A DONOR THAT HAD BEEN SERVING STARTS AGAIN FROM THE BASE: its connection
// closing is a new outage, not one more failure of the last.
func (p redialPolicy) after(failures int, err error) (int, time.Duration) {
	if errors.Is(err, statelog.ErrDonorConnectionClosed) {
		failures = 0
	}
	failures++
	return failures, backoff.Doubling(failures, p.base, p.ceiling)
}

// keepDonorServing serves donor for as long as ctx lives, dialling again
// whenever it stops: its connection is its own ([jetstream.Queue.DialOwned])
// and its loss is not the node's, since a donor serves PEERS below the log's
// floor: the node runs on and the donor comes back by itself. Served once, a
// closed connection would leave it subscribed to nothing while the snapshot
// register still advertised its artefact.
//
// A stop is logged only when its error is NEW (the first, one that differs from
// the last, or any close under a serving donor); the same refusal on every
// redial is not news.
func keepDonorServing(ctx context.Context, donor donorServer, policy redialPolicy) {
	failures := 0
	said := ""
	for {
		err := donor.Serve(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Serve returns nil only for an ended context; a nil
			// otherwise is redialled like any other stop.
			err = errors.New("statelog: the donor stopped serving with no error")
		}
		var wait time.Duration
		failures, wait = policy.after(failures, err)
		// A CLOSE IS ALWAYS NEW: the donor had been serving, so the last
		// outage's line is about one that ended.
		if msg := err.Error(); msg != said ||
			errors.Is(err, statelog.ErrDonorConnectionClosed) {
			said = msg
			log.ErrorContext(ctx, "statelog_donor_stopped", "error", msg,
				"redial_in", wait.String(),
				"detail", "a peer below the log's floor cannot adopt from this node "+
					"until its donor is serving again; the fleet's other members "+
					"still answer, and this node goes on serving its own company")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
