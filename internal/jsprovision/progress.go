package jsprovision

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Progress is the link a read's request and its reply cross, as a meter of
// its socket reads it (internal/queue/jetstream's LinkMeter): whether it
// carried, over the last d, what a link carries when it is busy at the floor
// every link is promised.
//
// A READ'S SILENCE IS ITS LINK'S, where it crosses one. A reply queued behind
// other bytes on a link is no reply nobody sent, and neither the broker nor
// the client sees any of it until its last byte is in, so the only thing that
// tells a read held behind a link's traffic from one asked of a broker that
// will not answer is the link itself: still, the silence is the broker's;
// moving, the reply may be among what it carries.
type Progress interface {
	Moving(d time.Duration) bool
}

// stillPeriod is how often a ceiling held by its link's movement is judged
// once it has run ([Timing.Ceiling]): a quarter of [ReadTerm], the cadence
// internal/queue/jetstream's meter reads its link at, so the ceiling ends
// within one of the meter's readings of the link falling still, and asks
// nothing a reading could not have changed.
const stillPeriod = ReadTerm / 4

// Following is t for a read across a link that progress reads: each of its
// ceilings runs only while the link is still, and is held past its span while
// the link moves, by beyond at most ([Timing.Ceiling]).
//
// beyond is what one reply may legitimately be held behind on the link: one
// message at the transport's bound crossing it at the floor. Past it, a link
// still moving is carrying other traffic, and the reply the read waits for
// was never sent.
func (t Timing) Following(progress Progress, beyond time.Duration) Timing {
	t.Progress, t.Beyond = progress, beyond
	return t
}

// Longest is the longest a ceiling of d at t can hold: d, and [Timing.Beyond]
// where a link's movement can hold it past d. What a caller that outlasts the
// read sizes itself by — the inactivity a broker reaps a consumer after, which
// a read the consumer's reader waits in must not outlast.
func (t Timing) Longest(d time.Duration) time.Duration {
	if t.Progress == nil {
		return d
	}
	return d + t.Beyond
}

// Ceiling is ctx ended at d of its link's stillness: at d for a timing that
// follows no link; for one that does ([Timing.Following]), at the first
// judgment at or past d (one every [stillPeriod] after the first) at which the
// link did not move over the last d, and at d and [Timing.Beyond] whatever the
// link does. Its error is [context.DeadlineExceeded] ([Unanswered]); ended by
// ctx, it is ctx's. Judged over the last d, not since the read began: a reply
// queued behind a message of megabytes is held as long as it takes to cross.
// Capped because movement is not an answer: a busy link moves whether or not a
// reply is on it, and a request a member dropped across an election is
// answered by nobody. One reply waits behind at most one message at the
// transport's bound, which Beyond is. The cancel returns once the judge stops.
func (t Timing) Ceiling(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if t.Progress == nil {
		return context.WithTimeout(ctx, d)
	}
	capped, cancel := context.WithTimeout(ctx, d+t.Beyond)
	c := &stillCeiling{Context: capped, done: make(chan struct{})}
	judged := make(chan struct{})
	go func() {
		defer close(judged)
		judge := time.NewTimer(d)
		defer judge.Stop()
		for {
			select {
			case <-capped.Done():
				c.end(capped.Err())
				return
			case <-judge.C:
			}
			if !t.Progress.Moving(d) {
				c.end(context.DeadlineExceeded)
				return
			}
			judge.Reset(stillPeriod)
		}
	}()
	return c, func() {
		cancel()
		<-judged
	}
}

// stillCeiling is a context a [Timing.Ceiling] ends, at its link's stillness
// or with the capped context it carries.
//
// ITS OWN CONTEXT rather than one cancelled with a cause, because a cancelled
// context's error is [context.Canceled], and a ceiling's ending is the
// silence a deadline is: [Unanswered] reads [context.DeadlineExceeded] off it
// as a read nobody answered, and the caller's own cancellation as the caller
// giving up. Its deadline is the cap's, so a request sent on it is held to
// no more than the ceiling can hold it.
type stillCeiling struct {
	context.Context
	done chan struct{}

	mu  sync.Mutex
	err error
}

func (c *stillCeiling) Done() <-chan struct{} { return c.done }

func (c *stillCeiling) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// end ends the ceiling with err, once.
func (c *stillCeiling) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// unfollowed refuses a timing that follows a link with no beyond: a ceiling
// held by a link's movement and capped at nothing past its span is one no
// caller chose, and capped at nothing it ends as an unheld one would.
func (t Timing) unfollowed() error {
	if t.Progress != nil && t.Beyond <= 0 {
		return fmt.Errorf("jsprovision: Timing.Beyond is %v with a link to follow — a read held "+
			"while its link moves is held by one message at the transport's bound crossing it, "+
			"which Timing.Following is handed", t.Beyond)
	}
	return nil
}
