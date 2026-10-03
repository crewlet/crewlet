package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE THAT STOPS VOUCHING FOR ITS IDENTITY ROWS SAYS SO, as a move.
//
// What the credential listener hears ([Engine.identityMoved]) is what a
// committed identity batch moved, a batch that retained a record, and an
// estate replaced underneath the node. Each of those is a COMMIT, and the one
// thing that changes every credential's answer on this node without one is the
// applier no longer keeping up at all: halted on a record it cannot read — a
// removal or a company-wide invalidation signed under a keyring key this node
// was not restarted with, a record that fails its signature, a recreated
// stream — or frozen behind a broker it cannot reach. Neither commits a batch,
// so nothing was ever said, and the record the applier halted on is often the
// very removal or invalidation meant to end an open socket's credential.
//
// The request path already answers it. Every identity question the guard asks
// — a session's rows, a machine token's, the binding a Tier A token acts
// through — compares the identity log's lag ([runningDomain.Lag], which a
// halted or frozen applier cannot hold down: [progress.frozenFor]) against
// [statelog.StallGrace], and past it answers UNKNOWN: a REST request is 503
// and a handshake is refused before any snapshot. An open socket was decided
// only on a commit, so on the same node it went on receiving every push and
// answering every question as its last decision, for as long as the node ran.
//
// # So the crossing is the event
//
// [vouching] watches the same figure against the same grace, and the moment
// it crosses — the guard's answer for every identity this node holds turning
// from "served" to "cannot say" — hands the listener a move naming everyone.
// Every socket is decided again, reads the stall its REST requests read, and
// closes 1013; its reconnect's handshake answers 503 like every route beside
// it. ONCE PER CROSSING: a node that stays past the grace has nothing new to
// say, and every socket decided on that crossing is already closed or is a
// credential the guard serves even on a stalled node (an unbound Tier A
// token, the break-glass path). Back under the grace re-arms it, so a second
// stall is said again.
//
// # Why not the applier's stop itself
//
// A halt is one way to cross the grace and not the only one — a broker
// partition freezes the checkpoint with no stop at all — and the halt is not
// when the guard's answer changes: until the frozen checkpoint passes the
// grace, the guard still serves every credential on its rows, exactly as it
// serves a node briefly behind. A move at the halt would decide every socket
// again into the same "served", and then nothing would decide them at the
// moment the guard stopped serving. What decides a socket has to be what
// decides a request, read at the instant it changes.
//
// SEATS ARE NOT NAMED: contact routing is rebuilt from rows, and a stall wrote
// none.
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
// ONE SECOND, because the figure it reads crosses the grace on the WALL CLOCK
// rather than at any event: the frozen term ([progress.frozenFor]) grows every
// instant from the checkpoint's last move, and the guard compares it at each
// request. A poll is therefore the only way to see the crossing, and its
// interval is exactly how long past the guard's own refusal an open socket
// can still be served — one second, the figure internal/api/stream's expiry
// retry already accepts for a deadline the guard and a socket's timer disagree
// on. A read
// costs an atomic load and one mutex, so a second costs nothing a node can
// measure; longer would serve a credential the REST surface beside it already
// refuses for longer.
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
