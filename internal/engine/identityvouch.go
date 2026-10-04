package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE THAT STOPS VOUCHING FOR ITS IDENTITY ROWS SAYS SO, as a move naming
// everyone — and that is all this watch does.
//
// An identity applier HALTED on a record it cannot read — a removal or a
// company-wide invalidation signed under a keyring key this node was not
// restarted with, a record that fails its signature, a recreated stream — or
// frozen behind a broker it cannot reach commits no batch, so the credential
// listener ([Engine.identityMoved]) never hears the record meant to end an
// open socket. What does move is the identity log's lag ([runningDomain.Lag],
// which a halted applier cannot hold down: [progress.frozenFor]), and past
// [statelog.StallGrace] every identity read on the node answers unknown, so a
// REST request is 503 and a handshake is refused before any snapshot.
//
// So the CROSSING is the event: the reading that finds the lag past the grace
// hands the listener a move naming everyone, which closes every open socket
// for its reconnect's handshake to decide. ONCE PER CROSSING — a node that
// stays past the grace has nothing new to say — and re-armed when it catches
// up. Not at the halt itself: inside the grace the guard still serves the rows
// this node has, as it serves a node briefly behind. SEATS ARE NOT NAMED:
// contact routing is rebuilt from rows, and a stall wrote none.
type vouching struct {
	// lag is the identity log's lag as of an instant — the figure every
	// identity read on this node carries, [runningDomain.lagAt].
	lag func(time.Time) time.Duration

	// moved is the credential listener's hand-off,
	// [Engine.identityMoved].
	moved func(iamdomain.Moved)

	// stalled is whether the last observation found the lag past the
	// grace. Read and written by [vouching.run]'s goroutine alone.
	stalled bool
}

// vouchInterval is how often [vouching] reads the identity log's lag.
//
// ONE SECOND, because the figure crosses the grace on the WALL CLOCK rather
// than at any event — the frozen term grows every instant from the
// checkpoint's last move — so a poll is the only way to see the crossing, and
// its interval is how long past the guard's own refusal an open socket can
// still be served. A read is an atomic load and one mutex.
const vouchInterval = time.Second

// observe takes one reading at now and hands the listener a move naming
// everyone if it is the reading that crossed the grace.
//
// PAST, as every reader of the figure compares it ([session.RowStalled], a
// token's check, a binding's vouch): a lag of exactly the grace is still
// served, so it is not yet a crossing.
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
// sees to e's credential listener.
//
// THE LAG IS THE RUNNING DOMAIN'S OWN, the function the identity reader is
// built with ([core.openIAM]), so the crossing this sees is the one every
// identity read on this node answers by — never a second reading of a stall
// that could cross at another instant.
func identityVouching(e *Engine, running *runningDomain) *vouching {
	return &vouching{lag: running.lagAt, moved: e.identityMoved}
}
