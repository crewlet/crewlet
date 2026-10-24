package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

// THE SEAT-CLAIM GATE'S OPENING IS AN EVENT, and the seat host is told of it.
//
// The host claims a seat only while its gate admits ([Engine.seatsAdmitted]:
// a mode that publishes, and [Engine.StateLogHydrated]), and it cannot see the
// gate change — it learns the answer only by asking, which it does on each
// pass. Its first pass is [seat.Host.Start]'s, and on the commonest boot the
// answer there is no: a data node's appliers are still working through their
// logs, so nothing is claimed. Nothing asked again until the sweep's next
// tick, up to [seat.SweepInterval] later, so a node that caught up a moment
// after it started ran none of its company for the rest of the interval — and
// a node handed its first company, whose apply's own pass met appliers still
// applying the records that apply had just written, waited the same way.
//
// So the engine, which owns what the gate reads, watches for what can open it
// and asks the host for a pass ([seat.Host.Resweep]) when it has. The events
// are the two it can see:
//
//   - A LOG WHOSE HEALTH GATES ADMISSION COMING LEVEL with its log
//     ([statelog.RunnerDeps.OnLevel]): the strict half of the gate is that
//     every such log is level now, and it turns true at the moment one of
//     them comes level after a backlog, a burst of writes or an outage.
//   - THE FLEET'S VIEW LISTING FOR THE FIRST TIME ([coord.LeaseView.Listed]),
//     without which a node whose company runs a native half refuses every
//     claim: its seats' calls could not be routed.
//
// What it cannot see is a PEER'S copy becoming one that admits: a node whose
// own copy does not serve — one holding no data, or one whose copy is out of
// service — is admitted on a data node's word, which it learns by asking, and
// its sweep asks every interval.
//
// # Armed by the gate's own refusal
//
// An event is acted on only while the gate's last answer to a pass was a
// refusal nothing has acted on ([gateWatch.refused]). A node whose last pass
// was admitted has nothing waiting on the gate — a seat free since is the
// fleet's news, which the tick is sized for — and evaluating the gate on every
// burst of writes a healthy node applies would cost the broker a health read
// per log for nothing. Armed, the watch evaluates the gate on its own
// goroutine and asks for a pass only when it admits: a pass costs a listing of
// the fleet, and the gate may still be closed for a reason no event here says
// has cleared. Each refusal is acted on ONCE — the ask disarms the watch, and
// the pass it brings asks the gate again, which re-arms it if it refuses.
//
// THE REFUSAL ITSELF IS AN EVENT TOO. A log can come level while a pass is
// deciding, after the pass read its lag and before its refusal was recorded:
// the watch told of it then found nothing armed, the refusal was recorded
// after, and with every log level no further event came — the seats waited for
// the tick, the latency this file exists to remove. So a refusal nudges the
// watch as it is recorded, and the watch evaluates the gate after it: an
// opening that raced the pass is found either way.
//
// NEVER THE HOST'S RECORD OF ITS LAST PASS ([seat.SweepResult.Withheld]),
// which also reads withheld for a pass on a node that cannot serve its seats
// at all — a pass that never asks the gate. Armed by that, every event on such
// a node found the gate open and asked for a pass that withheld itself again,
// and the host honours the next ask at once after a withheld pass that changed
// nothing — a full pass for every burst of writes the node applied, for as
// long as it could not serve.
//
// The host honours the watch's ask even inside the interval of the ask before
// it when that ask's pass changed nothing because the gate withheld it
// ([seat.Host.Resweep]) — an apply's, made while the node was still catching
// up with the records it had just written.

// gateWatch is the seat-claim gate as the host asks it, and the watch that
// asks the host for a pass when the gate opens after refusing one.
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

// gate is the claim gate as the seat host asks it ([seat.Config.Ready]): the
// gate's answer, a refusal recorded and announced to the watch, and an
// admission disarming it — a pass the gate admitted left nothing waiting.
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
// IT ACTS ONCE AS IT STARTS, before waiting for any event, because what came
// before it is not all still pending: the host's first pass ran in
// [seat.Host.Start] before the watch did, and the fleet's view may have
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
