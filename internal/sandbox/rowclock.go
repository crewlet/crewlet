package sandbox

import (
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A WAIT TIMED BY AN INSTANT ON THE ROW (failed collections or resumes, lost
// claims, a call in flight's bound, a held call's deadline) is judged by
// whichever process handles the run next, whose clock may stand
// [RowClockAllowance] from the stamper's. So each such instant names its
// clock, by the stamping process's incarnation, never its node id, which a
// rescheduled pod or rebuilt machine keeps on another clock. That process
// judges it on its own clock against the bound; any other, or an instant naming
// none (an older build's), against the bound plus the allowance, never before
// the bound has passed in real time; an empty clock is nobody's, never matched.
// A process recording the wait again
// restarts its window on its own clock ([RowWindow]), count and first kept. A
// kept box's age is read on its host's clock ([RelaunchTarget.At]).

// RowClockAllowance is what a process adds to a bound it judges against an
// instant another clock stamped: twice [objstore.AssumedClockSkew], the
// most two clocks the bound holds stand apart while no node raises
// `clock_skew`.
const RowClockAllowance = 2 * objstore.AssumedClockSkew

// RowWindow is where a wait on the row is timed on one clock: that of the
// process whose incarnation Clock names, which read Since, when the wait began
// or was first recorded there. Of is the first instant of the wait it times
// ([rowWait]); a window of another is a former wait's, which an older build
// kept in a section carrying keys it does not know (wirekeys.go) while it
// restarted the wait, and is not read. Where a section carries none, an older
// build's write drops the window, leaving the bound plus the allowance.
type RowWindow struct {
	Clock string    `json:"clock"`
	Since time.Time `json:"since"`
	Of    time.Time `json:"of"`
}

// rowWait is a wait timed on the row: first, when it began, on whichever clock
// stamped it, and window, where it is timed on one node's clock, nil for none.
type rowWait struct {
	first  time.Time
	window *RowWindow
}

// windowOn is the wait's window where it is clock's and of this wait, nil
// otherwise.
func (w rowWait) windowOn(clock string) *RowWindow {
	if win := w.window; win != nil && clock != "" && win.Clock == clock && win.Of.Equal(w.first) &&
		!win.Since.IsZero() {
		return win
	}
	return nil
}

// lasted reports whether the wait has lasted bound by clock's reading now: its
// window on that clock past bound, or its first instant past bound and
// [RowClockAllowance], whoever stamped it. A wait not begun has lasted nothing.
func (w rowWait) lasted(clock string, now time.Time, bound time.Duration) bool {
	if w.first.IsZero() {
		return false
	}
	if win := w.windowOn(clock); win != nil && now.Sub(win.Since) >= bound {
		return true
	}
	return now.Sub(w.first) >= bound+RowClockAllowance
}

// recorded is the wait's window once the process clock names records it again
// at at, by its own clock: kept where it is already that clock's, restarted at
// at where it is another's, of another wait or none. first is the wait's
// first instant as the same write leaves it.
func (w rowWait) recorded(clock string, at, first time.Time) *RowWindow {
	w.first = first
	if win := w.windowOn(clock); win != nil {
		return win
	}
	return &RowWindow{Clock: clock, Since: at.UTC(), Of: first.UTC()}
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

// until is the instant past which the process clock names, by its own clock,
// waits no longer for the call in flight: [BridgeCall.Until] where that clock
// stamped it, else [RowClockAllowance] past it. Zero for a call with no bound.
func (c BridgeCall) until(clock string) time.Time {
	if c.Until.IsZero() || (clock != "" && c.UntilClock == clock) {
		return c.Until
	}
	return c.Until.Add(RowClockAllowance)
}
