package sandbox

import "slices"

// A CAPTURE'S SAVE POINT, WHERE NO READ MADE NOW CAN PLACE IT.
//
// Only a capture whose harness ended its own run is placed by a read of its
// round's log ([SessionCapture.placeAt]) after the pack: that harness answered
// or gave up on every call and wrote every message first, so its session holds
// every call the log took, bar [BridgeCall.Abandoned] and [BridgeCall.Late]
// ones (internal/agent/runner's heldAt); reads counted without listing
// ([SessionCapture.Omitted]) are told whatever the point. Any other capture — a
// save, a job still working ([readCapture]), one cut off
// ([SessionCapture.Interrupted]) — may lack calls answered before their message
// streamed whole (measured on Claude Code 2.1.310), which a count off the log
// would name and a continuation never be told, so [savePlaces] places it.

// savePlaces is what a capture whose harness may not have written every call it
// made — a save ([SessionTake.Save]; this version stamps none), the take of a
// job still working, a capture of a job cut off — is placed by
// ([SessionCapture.placeAtNewest]).
type savePlaces struct {
	// run is the run's row as the capture read it, its launch the one whose
	// session is taken; its kept capture counts only where it is that
	// launch's ([launchKept]).
	run PendingRun

	// continued is the capture whose archive the launch's box took up
	// ([LaunchRequest.Session]); nil where it continued none (a session of its
	// own, its conversation's last turn's, or the copy left in the box) and
	// where the reader cannot say: a launch records none on its row, so the
	// take of a job still working or cut off hands none and is placed lower,
	// which tells more, never less.
	continued *SessionCapture
}

// placeAtNewest places c at the NEWEST save point from.run's launch is known to
// hold ([savePlaces.held]), sound since a session's archive only grows, or zero
// where none states one: a continuation is told calls again
// ([SessionCapture.Through]), never not at all, and a reading of the archive leaves out the held ones
// ([ExactTelling], [Coordinator.HoldingsOf]). It reads no log, so a log the
// store cannot read refuses no such capture ([Coordinator.takeCapture]). On a
// fleet of this version alone the point is zero (a read-placed capture is final
// and nothing of its launch is taken after it), so the brief and
// internal/engine's continuedFloor grow by the round, within the prior-work
// ledger's budgets. BridgeSeq, BridgeOpen and Omitted come off the same capture
// as the point, so an older peer is never told less.
func (c *SessionCapture) placeAtNewest(from savePlaces) {
	var newest *SessionCapture
	point := 0
	for _, held := range from.held() {
		if at, stated := held.SavePoint(); stated && (newest == nil || at > point) {
			newest, point = held, at
		}
	}
	c.Through = new(point)
	c.BridgeSeq, c.BridgeOpen, c.Omitted = 0, nil, nil
	if newest != nil {
		c.BridgeSeq, c.BridgeOpen = newest.BridgeSeq, slices.Clone(newest.BridgeOpen)
		c.Omitted = slices.Clone(newest.Omitted)
	}
}

// held is every capture p's launch's session is known to hold: its newest
// landed capture ([launchKept]), and the capture it continued — only where the
// launch carried its round on ([PendingRun.Round] names the job that opened
// it), since a launch that opened a round numbers its calls from a new log. A
// row that names no round is read as a new round's, the safe reading.
func (p savePlaces) held() []*SessionCapture {
	var held []*SessionCapture
	run := p.run
	if kept := launchKept(run); kept != nil {
		held = append(held, kept)
	}
	if continued := p.continued; continued != nil && run.Round != "" && run.Round != run.LaunchID {
		held = append(held, continued)
	}
	return held
}

// launchKept is run's kept capture ([PendingRun.Kept]) where it is the launch's
// own, else nil: before this launch's first capture lands, the turn's kept one
// is an earlier job's, which this launch may never have continued. A launch is
// its job and its ordinal both, as [Coordinator.takeCapture] reads it.
func launchKept(run PendingRun) *SessionCapture {
	if kept := run.Kept; kept != nil && kept.Launch == run.LaunchID && kept.LaunchSeq == run.LaunchSeq {
		return kept
	}
	return nil
}
