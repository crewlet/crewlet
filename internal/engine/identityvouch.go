package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE THAT STOPS VOUCHING FOR ITS IDENTITY ROWS SAYS SO, as a move naming
// everyone, and that is all this watch does.
//
// An identity applier halted on a record it cannot read (a key this node
// lacks, a bad signature, a recreated stream) or frozen behind an unreachable
// broker commits no batch, so [Engine.identityMoved] never hears the record
// meant to end an open socket. Its lag still grows ([progress.frozenFor]), and
// past [statelog.StallGrace] every identity read here answers unknown. So the
// crossing is the event: it closes every open socket for its reconnect's
// handshake to decide, once per crossing, re-armed on catching up. Not at the
// halt: inside the grace the guard still serves this node's rows. Seats are not
// named: contact routing is rebuilt from rows, and a stall wrote none.
type vouching struct {
	// lag is the identity log's lag as of an instant — the figure every
	// identity read on this node carries, [runningLog.lagAt].
	lag func(time.Time) time.Duration

	// moved is the credential listener's hand-off,
	// [Engine.identityMoved].
	moved func(iamdomain.Moved)

	// stalled is whether the last observation found the lag past the
	// grace. Read and written by [vouching.run]'s goroutine alone.
	stalled bool
}

// vouchInterval is how often [vouching] reads the identity log's lag: the
// figure crosses the grace on the wall clock, not at any event, so only a poll
// sees it, and the interval bounds how long past the guard's refusal an open
// socket is still served. A read is an atomic load and one mutex.
const vouchInterval = time.Second

// observe takes one reading at now and hands the listener a move naming
// everyone if it is the reading that crossed the grace. PAST, as every reader
// of the figure compares it ([session.RowStalled]): a lag of exactly the grace
// is still served.
func (v *vouching) observe(now time.Time) {
	stalled := v.lag(now) > statelog.StallGrace
	if stalled && !v.stalled {
		v.moved(iamdomain.Moved{Everyone: true})
	}
	v.stalled = stalled
}

// run observes every [vouchInterval] until ctx ends.
func (v *vouching) run(ctx context.Context) {
	tick := time.NewTicker(vouchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			v.observe(now)
		}
	}
}

// identityVouching is the watch over this node's identity log, handing what it
// sees to e's credential listener. The lag is the running log's own
// ([core.openIAM] builds the identity reader with it), so it crosses at the
// instant every identity read here answers by.
func identityVouching(e *Engine, running *runningLog) *vouching {
	return &vouching{lag: running.lagAt, moved: e.identityMoved}
}
