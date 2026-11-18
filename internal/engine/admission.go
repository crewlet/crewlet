package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

// THE SEAT-CLAIM GATE'S OPENING IS AN EVENT, and the seat host is told of it.
// The host learns whether the gate ([Engine.seatsAdmitted]) admits only by
// asking on a pass, so a gate that opens just after a refused pass (appliers
// still catching up at boot, or with the records an apply just wrote) would
// leave the node's seats unclaimed until the sweep's next tick
// ([seat.SweepInterval]). The engine owns what the gate reads, so it asks for a
// pass ([seat.Host.Resweep]) on the two openings it can see: a log whose health
// gates admission coming level ([statelog.RunnerDeps.OnLevel]), and the fleet's
// view listing for the first time ([coord.LeaseView.Listed]), without which a
// node running a native half refuses every claim. A peer's copy starting to
// admit it cannot see; the tick covers that.

// gateWatch is the seat-claim gate as the host asks it, and the watch that
// asks the host for a pass when the gate opens after refusing one.
//
// It acts only while a refusal is unanswered ([gateWatch.refused]): an
// admitted node has nothing waiting on the gate, and reading it on every burst
// of writes would cost a broker health read per log. Armed, it asks for a pass
// only once the gate admits (a pass lists the fleet, and the gate may be closed
// for a reason no event here reports cleared), and answers each refusal once:
// the ask disarms, and the pass re-arms it if refused. Armed by the gate's refusal,
// never [seat.SweepResult.Withheld], which also reads withheld on a node that
// cannot serve at all; the host honours an ask at once after a withheld pass,
// so that would make every write burst a full pass.
type gateWatch struct {
	// admits is the gate itself ([Engine.seatsAdmitted]).
	admits func(ctx context.Context) bool

	// refused records that the gate refused the last pass that asked it
	// and that nothing has acted on that refusal yet.
	refused atomic.Bool

	// nudges is told whenever something the gate reads may have opened
	// it, and whenever the gate refuses a pass. Buffered by one: a nudge
	// that finds one pending adds nothing to it.
	nudges chan struct{}
}

// resweeper is what the watch asks of the seat host.
type resweeper interface {
	Resweep()
}

func newGateWatch(admits func(ctx context.Context) bool) *gateWatch {
	return &gateWatch{admits: admits, nudges: make(chan struct{}, 1)}
}

// gate is the claim gate as the seat host asks it ([seat.Config.Ready]). An
// admission disarms the watch; a refusal arms and nudges it, because a log that
// came level while this pass decided found the watch unarmed, and with every
// log level no later event would come before the tick.
func (w *gateWatch) gate(ctx context.Context) bool {
	if w.admits(ctx) {
		w.refused.Store(false)
		return true
	}
	w.refused.Store(true)
	w.nudge()
	return false
}

// nudge tells the watch that the gate may have opened, or has just refused a
// pass, without waiting. Called on an apply loop's goroutine and on the
// sweep's, so it must never block. Nil-safe: an engine built without a watch
// tells nobody.
func (w *gateWatch) nudge() {
	if w == nil {
		return
	}
	select {
	case w.nudges <- struct{}{}:
	default:
	}
}

// readmit acts on one event: while a refusal is unanswered and the gate now
// admits, it asks the host for one pass and disarms.
//
// The disarm is a compare-and-swap made after the gate answered, so a pass the
// gate admitted meanwhile — which disarmed it first — is not followed by a
// second, and a pass refused meanwhile, which re-armed it, is still answered.
func (w *gateWatch) readmit(ctx context.Context, host resweeper) {
	if !w.refused.Load() || !w.admits(ctx) {
		return
	}
	if w.refused.CompareAndSwap(true, false) {
		host.Resweep()
	}
}

// run reads events until ctx ends, acting on each ([gateWatch.readmit]).
//
// IT ACTS ONCE AS IT STARTS, before any event: the host's first pass
// ([seat.Host.Start]) ran before the watch did, and the fleet's view may have
// listed already. listed is closed by the view's first listing, and nil where
// the gate reads no view.
func (w *gateWatch) run(ctx context.Context, host resweeper, listed <-chan struct{}) {
	for {
		w.readmit(ctx, host)
		select {
		case <-ctx.Done():
			return
		case <-w.nudges:
		case <-listed:
			// CLOSED, and so ready for ever: read once.
			listed = nil
		}
	}
}

// nudgeAdmission tells the claim gate's watch that something the gate reads
// may have opened it ([gateWatch.nudge]). Called on an apply loop's goroutine.
func (e *Engine) nudgeAdmission() { e.claims.nudge() }

// startAdmissionWatch runs the watch until [Engine.stopAdmissionWatch],
// detached from ctx like every loop a node owns. Safe to call twice.
func (e *Engine) startAdmissionWatch(ctx context.Context) {
	if e.node == nil || e.claims == nil || e.stopAdmission != nil {
		return
	}
	var listed <-chan struct{}
	if e.dataView != nil {
		listed = e.dataView.Listed()
	}
	host := e.node.Host()
	loop, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var done sync.WaitGroup
	done.Go(func() { e.claims.run(loop, host, listed) })
	e.stopAdmission = func() {
		cancel()
		done.Wait()
	}
}

// stopAdmissionWatch ends the watch and waits for it. Nil-safe, and safe to
// call twice.
func (e *Engine) stopAdmissionWatch() {
	if e.stopAdmission == nil {
		return
	}
	e.stopAdmission()
	e.stopAdmission = nil
}
