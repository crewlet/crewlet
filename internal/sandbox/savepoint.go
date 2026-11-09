package sandbox

import "slices"

// A CAPTURE'S SAVE POINT, WHERE NO READ MADE NOW CAN PLACE IT.
//
// A capture is placed by one read of its round's log ([SessionCapture.placeAt])
// ONLY WHERE ITS HARNESS ENDED ITS OWN RUN — the job over, its harness gone
// cleanly by itself and its script packing it. That read is made once the job
// is over, so after its harness exited and its script packed the archive,
// never before the pack: a harness ends its own run only once every call it
// made has been answered, and writes every message it made before it exits,
// so every call the log had taken by the read is one its session holds. Every
// other capture is packed where its harness may not have written every call
// it made, and a harness writes a call to its record only once the model's
// whole message has streamed: measured on Claude Code 2.1.310 against a
// scripted model, a call is made — and can be answered — before its tool_use
// line is in the transcript (eleven of thirteen timed calls started with no
// line there, and five quick ones were answered with neither their call nor
// their result written). Those captures are:
//
//   - the TAKE OF A JOB STILL WORKING (one its box would not end,
//     [readCapture]), packed while the job still works, its next message
//     perhaps still streaming;
//   - a capture of a job CUT OFF ([SessionCapture.Interrupted]): its harness
//     killed — past its end's grace, by its memory limit, by a crash — which
//     never writes a message still streaming; told to stop by its end, which
//     2.1.310 answers by recording the call it was cut off in (measured only
//     with that call's message already written, never with one still
//     streaming); ended before its harness started, its session the one it
//     was handed, which never read the calls its brief told; or gone with a
//     loss, whose read cannot say how it went.
//
// So a count read off the log as such a capture is taken can name calls its
// session does not hold, and a continuation told only the calls after that
// count would never be told those. None of them reads the log, so a log the
// store cannot read refuses none of them ([Coordinator.takeCapture]): each is
// placed off its row, at the NEWEST SAVE POINT ITS LAUNCH IS ALREADY KNOWN TO
// HOLD, each one sound by itself, since what a session holds only grows:
//
//   - the save point of the launch's newest landed capture: that session's
//     archive only grows, so every call it held, this one holds;
//   - zero, for a launch with no landed capture that states one: a launch
//     records on its row no capture it continued, so whatever it began from —
//     a session of its own, the one its conversation's last turn left, or a
//     capture of its round or of the round before — every call of its round is
//     then told again.
//
// ON A FLEET OF THIS VERSION ALONE THAT IS ZERO: a capture placed by its read
// is final, and nothing of its launch is taken after one
// ([Coordinator.takeCapture]), so the landed captures of a launch before one
// placed here were placed here too; a point above zero is one another build
// stamped, in a fleet mid-upgrade. So a continuation of such a capture is told
// every call of its round, within the prior-work ledger's budgets, and its
// brief, and the floor its launch is held to (internal/engine's
// continuedFloor), grow with them.
//
// A continuation from such a capture is told every call after that point, a
// subagent's included, and the ones its harness had already recorded are told
// again, as a continuation already accepts ([SessionCapture.Through]): told
// more than once, never not at all. The places an older build reads a save
// point by come from the same capture as the point ([SessionCapture.BridgeSeq],
// [SessionCapture.BridgeOpen]), so a peer reading them is never told less.

// placeAtNewest places c, a capture of the session run's launch worked in
// that its harness may not have written whole, at the NEWEST save point that
// launch is known to hold (see the file comment): its save point
// ([SessionCapture.Through]) and, off the same capture, the places an older
// build reads it by — or the round's start, zero, where no landed capture of
// that launch states one. run is the run's row as the capture read it, its
// launch the one whose session is taken.
func (c *SessionCapture) placeAtNewest(run PendingRun) {
	var newest *SessionCapture
	point := 0
	if kept := launchKept(run); kept != nil {
		if at, stated := kept.SavePoint(); stated {
			newest, point = kept, at
		}
	}
	c.Through = new(point)
	c.BridgeSeq, c.BridgeOpen = 0, nil
	if newest != nil {
		c.BridgeSeq, c.BridgeOpen = newest.BridgeSeq, slices.Clone(newest.BridgeOpen)
	}
}

// launchKept is the newest landed capture run's launch's session is known to
// hold: the row's kept capture ([PendingRun.Kept]) where it is one of that
// launch's, and nil where it is not.
//
// THE KEPT CAPTURE ONLY WHERE IT IS THIS LAUNCH'S: the row's kept capture is
// the turn's newest landed one, which before this launch's first capture lands
// is an earlier job's — one this launch may never have continued. A stamp
// names its launch by its job and its ordinal both, as the rule that takes a
// job once reads it ([Coordinator.takeCapture]), and one that differs in
// either is another launch's.
func launchKept(run PendingRun) *SessionCapture {
	if kept := run.Kept; kept != nil && kept.Launch == run.LaunchID && kept.LaunchSeq == run.LaunchSeq {
		return kept
	}
	return nil
}
