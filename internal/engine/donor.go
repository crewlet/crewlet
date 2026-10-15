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
// A SECOND FIRST, because the commonest stop is a connection the NATS client
// closed under a donor that was serving — a server that refused one chunk past
// a lowered max_payload, a credential it stopped accepting — and the redial is
// a fresh connection that either works at once or is refused at the dial. The
// dial reconnects nothing itself, so a refusal is the server's present answer
// and asking again in a second costs one handshake.
//
// THE SNAPSHOT LOOP'S OWN RETRY AS THE CEILING, because the two are the same
// question asked of the same fleet — can this node serve a peer that fell
// behind — and the snapshot loop already answers how often a node that cannot
// should ask: often enough that a restarted peer finds a donor within its boot,
// rarely enough that a node genuinely unable to serve does not spend its life
// on it.
var donorRedial = redialPolicy{base: time.Second, ceiling: snapshotSkipRetry}

// after is the loop's arithmetic, apart from the loop so it is checked without
// a clock: the failures counted so far once err has ended a Serve, and how
// long to wait before the next.
//
// A DONOR THAT HAD BEEN SERVING STARTS AGAIN FROM THE BASE. Its connection
// closing under it is a new outage, not one more failure of the last, so the
// redial after it is the prompt one even on a node whose donor failed to dial
// a hundred times last week.
func (p redialPolicy) after(failures int, err error) (int, time.Duration) {
	if errors.Is(err, statelog.ErrDonorConnectionClosed) {
		failures = 0
	}
	failures++
	return failures, backoff.Doubling(failures, p.base, p.ceiling)
}

// keepDonorServing serves donor for as long as ctx lives, dialling again
// whenever it stops.
//
// # Why the node does not stop with it
//
// A donor's connection is its own ([jetstream.Queue.DialOwned]) and its loss is
// not the node's: a donor serves PEERS that fell below the log's floor, and a
// node whose donor is down still serves its own company — the reason a donor
// that cannot be armed at all does not gate the boot. So a donor's connection
// closing for good stops nothing but the donor, and the donor has to come back
// by itself. It used to be served once: a connection the client closed left
// the donor subscribed to nothing for the rest of the node's life while the
// snapshot register went on advertising its artefact, and a peer that needed
// it found nobody answering on this node.
//
// # What it says
//
// A stop is logged when its error is NEW — the first, each that differs from
// the last, and every connection closed under a donor that was serving — and a
// repeat of the same refusal on every redial is not, for
// the snapshot loop's reason: a state that has not changed is not news, and a
// donor refused at the dial is redialled for as long as it is refused.
func keepDonorServing(ctx context.Context, donor donorServer, policy redialPolicy) {
	failures := 0
	said := ""
	for {
		err := donor.Serve(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Serve returns nil only for a context that ended, which is
			// handled above; anything else would be a donor stopping
			// with nothing to say, and is redialled like any other stop.
			err = errors.New("statelog: the donor stopped serving with no error")
		}
		var wait time.Duration
		failures, wait = policy.after(failures, err)
		// A CLOSE IS ALWAYS NEW: the donor had been serving, so whatever
		// was said about the last outage is about one that ended.
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
