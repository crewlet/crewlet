package sandbox

// A CAPTURE'S SAVE POINT, WHERE NO READ MADE NOW CAN PLACE IT.
//
// Only a capture whose harness ended its own run is placed by a read of its
// round's log ([SessionCapture.placeAt]) after the pack: that harness answered
// or gave up on every call and wrote every message first, so its session holds
// every call the log took, bar [BridgeCall.Abandoned] and [BridgeCall.Late]
// ones (internal/agent/runner's heldAt). Any other capture — a save, a job
// still working ([readCapture]), one cut off ([SessionCapture.Interrupted]) —
// may lack calls answered before their message streamed whole (measured on
// Claude Code 2.1.310), so it is reserved at zero and raised after its take.

// savePlaces is what a capture whose harness may not have written every call it
// made — a save ([Coordinator.saveTask]), the take of a job still working, a
// capture of a job cut off — is raised by once its take is counted
// ([SessionCapture.placeAtNewest]).
type savePlaces struct {
	// run is the run's row as the capture read it, its launch the one whose
	// session is taken: its kept capture counts only where it is that
	// launch's ([launchKept]), and its record names the capture the launch
	// continued ([LaunchRecord.Continued]).
	run PendingRun

	// resumeRead says the take's archive was written by a release whose
	// resume this build read ([TakenSession.ResumeRead]): only such a Claude
	// Code capture is raised.
	resumeRead bool
}

// placeAtZero reserves c, a capture no read places, at a stated zero, with no
// place an older build reads and no read left out: what tells a continuation
// every call of the round, more, never less. Its take raises it once counted
// ([SessionCapture.placeAtNewest]).
func (c *SessionCapture) placeAtZero() {
	c.Through = new(0)
	c.BridgeSeq, c.BridgeOpen, c.Omitted = 0, nil, nil
}

// placeAtNewest raises c, reserved at zero and its take's count stamped, to the
// newest save point a capture its launch's session holds states whose newest
// compaction is the one c's count names ([launchKept], [LaunchRecord.Continued]),
// Omitted the launch's [LaunchRecord.OmittedCalls] at or below it; else c stays
// at zero. The package doc's "A capture no read places" says why it is sound.
//
// THE POINT IS RAISED AFTER THE TAKE, never at the place: whether the session
// compacted since a capture it holds is known only from the take's own count,
// which only the spool's whole read states, after the place is reserved. A
// place is reserved at zero, which tells more, never less, and the outcome
// states the raised point; a standby's takeover, reserved after its own take,
// states that take's.
func (c *SessionCapture) placeAtNewest(from savePlaces) {
	newest, read := c.ResumedCount.Restarted()
	if !read || (c.Harness == ClaudeCodeHarness && !from.resumeRead) {
		return
	}
	run, point := from.run, 0
	if kept := launchKept(run); kept != nil {
		if at, stated := kept.SavePoint(); stated && sameRestart(restartOf(kept.ResumedCount), newest) {
			point = at
		}
	}
	if p := run.Launch.Continued; p != nil && p.LaunchSeq > 0 && p.LaunchSeq == run.LaunchSeq-1 &&
		run.Launch.SessionStart == SessionStartContinued && sameRestart(run.Launch.BasisRestart, newest) {
		point = max(point, p.Through)
	}
	if point <= 0 {
		return
	}
	raised := *c
	raised.Through = new(point)
	raised.Omitted = nil
	for _, place := range run.Launch.OmittedCalls() {
		if place <= point {
			raised.Omitted = append(raised.Omitted, place)
		}
	}
	if raised.Bounded() == nil {
		*c = raised
	}
}

// sameRestart reports whether seen, a newest compaction a capture or a launch
// read ([restartOf], [LaunchRecord.BasisRestart]), is newest: both read, and
// one. Never [RestartedSince]'s rule, which reads an unknown basis as no
// restart where the thread has none: a raise needs both identities read.
func sameRestart(seen *string, newest string) bool {
	return seen != nil && *seen == newest
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
