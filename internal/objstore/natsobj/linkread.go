package natsobj

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// A READ ACROSS A LINK, ONE PULL AT A TIME, EACH INSIDE THE LINK'S WINDOW
// ([Config.Window]). A pull is the one thing a reader can admit, so it asks
// every pull itself ([jsapi.Pulls]), its bytes taken from the window first and
// given back once the pull has ended: a download's pull is one piece
// ([pieceBound]), a walk's the window, and a message larger than either is
// pulled again alone at the transport's bound. A slow reader holds nothing of
// the window: what it has not handed over has already crossed.
//
// THE SILENCE IS THE LINK'S ([reading.judge]), as a put's is ([Backend.held]):
// the consumer is made again when its heartbeats stop on a still link, or once
// the link has read a message at the transport's bound and a window since the
// read last heard a word.

// pulled starts r's reading of cons, made across a link, and answers its first
// message; fresh says a consumer that never had its first pull is made again
// ([errUnpulled]).
func (r *reading) pulled(ctx context.Context, cons jetstream.Consumer, fresh bool) (*message, error) {
	p, err := jsapi.NewPulls(r.b.js.Conn(), r.b.api, Stream, cons, r.b.timing)
	if err != nil {
		r.b.drop(ctx, cons)
		return nil, err
	}
	r.cons, r.pulls, r.hand, r.fresh, r.lost = cons, p, nil, fresh, false
	return r.handed(ctx)
}

// handed is the next message r's pulls hand over, a pull read whole whenever
// r holds none ([reading.fill]); its consumer made again past what it handed
// over once it went quiet ([reading.again]).
func (r *reading) handed(ctx context.Context) (*message, error) {
	for len(r.hand) == 0 {
		if r.lost {
			return r.again(ctx)
		}
		err := r.fill(ctx)
		switch {
		case errors.Is(err, errLost):
			r.lost = true
		case err != nil:
			return nil, err
		case len(r.hand) == 0:
			// A PULL THAT ENDED WITH NOTHING, its expiry or its bytes: the
			// read's stall still runs, across as many pulls as it takes.
			if since := time.Since(r.word); r.stall > 0 && since >= r.stall && !r.b.held(since) {
				return nil, fmt.Errorf("no message arrived for %v: %w", since.Round(time.Second),
					context.DeadlineExceeded)
			}
		}
	}
	msg := r.hand[0]
	r.hand = r.hand[1:]
	r.seq = msg.seq
	return msg, nil
}

// pieceBound is what a pull of one piece of want bytes is bounded at, and
// takes of the window: the piece and what the broker counts beside it.
func pieceBound(want int) int { return want + deliveryOverhead }

// fill asks r's next pull, its bytes taken from the link's window first and
// given back once the pull has ended, and reads it whole into r's hand: one
// message of the piece a get reads next ([reading.want]), up to the batch of a
// walk inside the window, or one message at the transport's bound alone where
// the last pull ended past its bytes with none. The take waits its turn under
// ctx alone, before the pull's silence is counted: a read waiting its turn has
// asked the broker nothing.
func (r *reading) fill(ctx context.Context) error {
	n, bound := r.batch, r.b.window.Size()
	switch {
	case r.alone:
		n, bound, r.alone = 1, r.b.transport(), false
	case r.want > 0:
		n, bound = 1, pieceBound(r.want)
	}
	if err := r.b.window.Take(ctx, bound); err != nil {
		return err
	}
	defer r.b.window.Give(bound)
	if err := r.pulls.Ask(n, bound, linkExpiry, linkHeartbeat); err != nil {
		return err
	}
	heard := time.Now()
	var read uint64
	if r.b.link != nil {
		_, read = r.b.link.Carried()
	}
	delivered := 0
	for {
		wait, cancel := context.WithDeadline(ctx, r.judgedAt(heard))
		ev, err := r.pulls.Next(wait)
		cancel()
		switch {
		case ctx.Err() != nil:
			return context.Cause(ctx)
		case err != nil && wait.Err() != nil:
			if jerr := r.judge(ctx, heard, read); jerr != nil {
				return jerr
			}
			continue
		case err != nil:
			return err
		}
		heard = time.Now()
		switch ev.Kind {
		case jsapi.Delivered:
			r.hand = append(r.hand, fromPull(ev))
			r.fresh, r.word = false, heard
			delivered++
			if ev.Ended {
				return nil
			}
		case jsapi.Heartbeat:
		case jsapi.PastMaxBytes:
			// A MESSAGE PAST THE BOUND, with none delivered ahead of it:
			// pulled again alone at the transport's.
			r.alone = delivered == 0
			return nil
		case jsapi.Expired, jsapi.BatchCompleted:
			return nil
		default:
			// REFUSED: the consumer deleted under the pull, its leader
			// moving, nobody answering. Made again.
			return fmt.Errorf("%w: %w", errLost, ev.Refusal())
		}
	}
}

// judgedAt is when a pull r has heard nothing of since heard is next judged
// ([reading.judge]): at its first quiet second while its consumer may never
// have had the pull, at two heartbeats, and at its stall, whichever comes
// first and has not passed; a [quietWait] from now once all have.
func (r *reading) judgedAt(heard time.Time) time.Time {
	now := time.Now()
	next := now.Add(quietWait)
	at := []time.Time{heard.Add(2 * linkHeartbeat)}
	if r.fresh {
		at = append(at, heard.Add(quietWait))
	}
	if r.stall > 0 {
		at = append(at, r.word.Add(r.stall))
	}
	for _, t := range at {
		if t.After(now) && t.Before(next) {
			next = t
		}
	}
	return next
}

// judge decides a pull r has heard nothing of since heard, its link having
// read read bytes by then:
//
//   - a consumer that never had its first pull is made again ([errUnpulled]);
//   - no message for r's stall, the link having carried nothing at the floor
//     over the stall or the read having had none for [Backend.heldCap], is
//     the read stalled;
//   - two heartbeats missed on a link that carried nothing at the floor over
//     them, or as much read off the link as a message at the transport's
//     bound and a window, is the consumer lost, and it is made again
//     ([errLost]);
//   - otherwise the pull is waited on.
func (r *reading) judge(ctx context.Context, heard time.Time, read uint64) error {
	quiet := time.Since(heard)
	if r.fresh && quiet >= quietWait {
		if r.unpulled(ctx) {
			return errUnpulled
		}
		r.fresh = false
	}
	if since := time.Since(r.word); r.stall > 0 && since >= r.stall && !r.b.held(since) {
		return fmt.Errorf("no message arrived for %v: %w", since.Round(time.Second), context.DeadlineExceeded)
	}
	if quiet < 2*linkHeartbeat {
		return nil
	}
	if r.b.link == nil || !r.b.link.Moving(2*linkHeartbeat) {
		return fmt.Errorf("%w for %v on a link that carried nothing at the floor", errLost, quiet.Round(time.Second))
	}
	if _, now := r.b.link.Carried(); now-read >= uint64(r.b.transport()+r.b.window.Size()) {
		return fmt.Errorf("%w while the link read %d bytes", errLost, now-read)
	}
	return nil
}

// again makes r's consumer again past the last message it handed over, or
// from where it began where it handed over none, once it went quiet — asked
// again a [quietWait] apart while nobody answers the making and the read is
// within its stall. A listing's consumer cannot be made again past a sequence
// ([reading.resume]), so a listing whose consumer went quiet fails, to be
// asked again.
func (r *reading) again(ctx context.Context) (*message, error) {
	if r.cfg.DeliverPolicy == jetstream.DeliverLastPerSubjectPolicy {
		return nil, fmt.Errorf("%w across the link; list again", errLost)
	}
	for {
		var msg *message
		var err error
		if r.seq > 0 {
			msg, err = r.resume(ctx, r.seq)
		} else {
			r.stop(ctx)
			msg, err = r.open(ctx)
		}
		if err == nil || !jsprovision.Unanswered(ctx, err) || (r.stall > 0 && time.Since(r.word) >= r.stall) {
			return msg, err
		}
		pause := time.NewTimer(quietWait)
		select {
		case <-ctx.Done():
			pause.Stop()
			return nil, context.Cause(ctx)
		case <-pause.C:
		}
	}
}
