package statelog

import (
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// refusalWatch is how long one domain's reads have been refused for a fault,
// which is the `read_refusals` alarm's input.
//
// # Why a run rather than a count
//
// The alarm's condition is written in TIME — reads refused for longer than one
// reconcile interval — because a single refusal during an election is not a
// fault and a sustained one is. A refusal counter can say a fault-class
// refusal happened and cannot say when it started or whether it has stopped,
// so an alarm read off it either never fires or, once one refusal lands in its
// window, fires for as long as the window holds it — a day's alarm about a
// no_quorum during a rolling restart. So the reader that makes each refusal
// remembers when the current run of them began.
//
// # Why per level
//
// A fault costs levels differently: a barrier that did not commit refuses
// `linearizable` while `stale` goes on answering. A stale read served says
// nothing about whether the barrier commits now, so a run ends only at a read
// SERVED AT ITS OWN LEVEL — or once nothing at that level has been refused for
// a fault for [refusalQuiet], because a caller that stopped asking is not
// evidence the fault continues, and a run nothing ever ends would latch the
// alarm exactly as a count does.
//
// # How long a run is
//
// From its first refusal to its LAST, never to now: the run is what was
// observed, and a level nobody has asked about since is not one whose reads
// are still being refused. So a run shows its length as its refusals arrive,
// and stops growing when they do.
type refusalWatch struct {
	mu   sync.Mutex
	runs map[ReadLevel]refusalRun
}

// refusalRun is one level's current run of fault-class refusals.
type refusalRun struct {
	// since is the first refusal of the run, and last the most recent.
	since, last time.Time
}

// refusalQuiet is how long a run outlives its last refusal with nothing at its
// level refused or served.
//
// FOUR RECONCILE INTERVALS, the span [FloorCacheStale] gives a coordination
// fact before it stops counting, and longer than the interval at which a
// polling reader asks: a screen refreshing every twenty seconds meets a
// sustained fault as one refusal per refresh, and a run that ended between two
// of them would restart at zero on each and never reach the alarm's threshold
// however long every read was refused.
const refusalQuiet = 4 * coord.ReconcileInterval

// refused records one refusal. A refusal that is ordinary lag is not a fault
// and does not start or extend a run.
func (w *refusalWatch) refused(level ReadLevel, code ReadRefusal, now time.Time) {
	if code.OrdinaryLag() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	run, current := w.runs[level]
	if !current || now.Sub(run.last) > refusalQuiet {
		run = refusalRun{since: now}
	}
	run.last = now
	if w.runs == nil {
		w.runs = map[ReadLevel]refusalRun{}
	}
	w.runs[level] = run
}

// served ends the run at a level: a read was answered there.
func (w *refusalWatch) served(level ReadLevel) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.runs, level)
}

// longest is the longest current run as of now, and false when none is.
func (w *refusalWatch) longest(now time.Time) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out time.Duration
	var current bool
	for level, run := range w.runs {
		if now.Sub(run.last) > refusalQuiet {
			// QUIET FOR TOO LONG: the run is over.
			delete(w.runs, level)
			continue
		}
		out, current = max(out, run.last.Sub(run.since)), true
	}
	return out, current
}

// FaultRefusingSince is how long this domain's reads at one level have been
// refused for something other than ordinary lag with none served at that
// level — the longest such run across the levels, from its first refusal to
// its last — and false when no run is current as of now. See [refusalWatch]
// for what begins and ends one.
func (r *Reader) FaultRefusingSince(now time.Time) (time.Duration, bool) {
	return r.watch.longest(now)
}
