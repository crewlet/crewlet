package sandbox

import (
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A WAIT TIMED BY AN INSTANT ON THE ROW is judged by whichever node handles the
// run next, whose clock may stand [RowClockAllowance] from the one that
// stamped it. So each such instant names its node, and the judge is exact for
// either reading: the node that stamped it judges it on its own clock against
// the bound; any other, or an instant naming none, against the bound plus the
// allowance, which never comes before the bound has passed in real time. Where
// a node other than the one named records the wait again, its window restarts
// on that node's clock ([RowWindow]), the count and the first instant kept. See
// the package doc, "Instants another node stamped".

// RowClockAllowance is what a node adds to a bound it judges against an
// instant another node's clock stamped: twice [objstore.AssumedClockSkew], the
// most two clocks the bound holds stand apart while no node raises
// `clock_skew`.
const RowClockAllowance = 2 * objstore.AssumedClockSkew

// RowWindow is where a wait on the row is timed on one clock: Node's, which
// read Since, when the wait began or was first recorded there. Of is the first
// instant of the wait it times ([rowWait]); a window of another is a former
// wait's, which a build carrying keys it does not know (wirekeys.go) kept
// while it restarted the wait, and is not read.
type RowWindow struct {
	Node  string    `json:"node"`
	Since time.Time `json:"since"`
	Of    time.Time `json:"of"`
}

// rowWait is a wait timed on the row: first, when it began, on whichever clock
// stamped it, and window, where it is timed on one node's clock, nil for none.
type rowWait struct {
	first  time.Time
	window *RowWindow
}

// windowOn is the wait's window where it is node's and of this wait, nil
// otherwise.
func (w rowWait) windowOn(node string) *RowWindow {
	if win := w.window; win != nil && win.Node == node && win.Of.Equal(w.first) && !win.Since.IsZero() {
		return win
	}
	return nil
}

// lasted reports whether the wait has lasted bound by node's clock at now: its
// window on node's clock past bound, or its first instant past bound and
// [RowClockAllowance], whoever stamped it. A wait not begun has lasted nothing.
func (w rowWait) lasted(node string, now time.Time, bound time.Duration) bool {
	if w.first.IsZero() {
		return false
	}
	if win := w.windowOn(node); win != nil && now.Sub(win.Since) >= bound {
		return true
	}
	return now.Sub(w.first) >= bound+RowClockAllowance
}

// recorded is the wait's window once node records it again at at, by its own
// clock: kept where it is already node's, restarted at at where it is another
// node's, of another wait or none. first is the wait's first instant as the
// same write leaves it.
func (w rowWait) recorded(node string, at, first time.Time) *RowWindow {
	w.first = first
	if win := w.windowOn(node); win != nil {
		return win
	}
	return &RowWindow{Node: node, Since: at.UTC(), Of: first.UTC()}
}

// collectWait is the record's run of failed collections as a wait
// ([LaunchRecord.CollectFailingOn]).
func (l LaunchRecord) collectWait() rowWait {
	return rowWait{first: l.CollectFailingSince, window: l.CollectFailingOn}
}

// resumeWait is the record's run of failed resumes as a wait
// ([LaunchRecord.ResumeFailingOn]).
func (l LaunchRecord) resumeWait() rowWait {
	return rowWait{first: l.ResumeFailingSince, window: l.ResumeFailingOn}
}

// lostWait is the answer's lost claims as a wait ([RecordedAnswer.LostOn]).
func (a RecordedAnswer) lostWait() rowWait { return rowWait{first: a.FirstLostAt, window: a.LostOn} }

// lostWait is the wait's lost claims as a wait ([WaitOver.LostOn]).
func (w WaitOver) lostWait() rowWait { return rowWait{first: w.FirstLostAt, window: w.LostOn} }

// until is the instant past which node, by its own clock, waits no longer for
// the call in flight: [BridgeCall.Until] where node's clock stamped it, else
// [RowClockAllowance] past it. Zero for a call with no bound.
func (c BridgeCall) until(node string) time.Time {
	switch {
	case c.Until.IsZero() || c.UntilOn == node:
		return c.Until
	}
	return c.Until.Add(RowClockAllowance)
}
