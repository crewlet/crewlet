package sandbox

import "slices"

// A SAVE'S SAVE POINT, WHERE NO READ MADE NOW CAN PLACE IT.
//
// A capture is placed by one read of its round's log made before its pack
// ([SessionCapture.placeAt]): every call the log had taken by then had been
// answered before the pack began, and a job that is OVER wrote every call it
// made before it ended, so its session holds each. A SAVE is packed while its
// job still works, and a harness writes a call to its record only once the
// model's whole message has streamed: measured on Claude Code 2.1.310 against
// a scripted model, a call is made — and can be answered — before its
// tool_use line is in the transcript (eleven of thirteen timed calls started
// with no line there, and five quick ones were answered with neither their
// call nor their result written). So a count read off the log while the job
// works can name calls its session does not hold yet, and a continuation told
// only the calls after that count would never be told those.
//
// So a save of a running session is placed at the NEWEST SAVE POINT ITS LAUNCH
// IS ALREADY KNOWN TO HOLD, each one sound by itself, since what a running
// session holds only grows:
//
//   - the save point of the launch's newest landed save: that session's
//     archive only grows, so every call it held, this one holds;
//   - the save point of the capture the launch continued, where it carried its
//     round on: the brief that continued it told the run every call after it;
//   - zero, for a launch that continued nothing of its round — a session of
//     its own, the one its conversation's last turn left, or a capture of the
//     round before, a send-back's, whose point counts another round's calls:
//     its brief handed it what it needed, and every call of its round is then
//     told again.
//
// A continuation from such a save is told every call after that point, a
// subagent's included, and the ones its harness had already recorded are told
// twice, as a continuation already accepts ([SessionCapture.Through]): told
// twice, never not at all. The places an older build reads a save point by
// come from the same capture as the point ([SessionCapture.BridgeSeq],
// [SessionCapture.BridgeOpen]), so a peer reading them is never told less.

// savePlaces is what a save of the session a launch is still working in is
// placed by ([SessionCapture.placeAtNewest]).
type savePlaces struct {
	// run is the run's row as the save read it, its launch the one still
	// working: its kept capture ([PendingRun.Kept]) is that launch's newest
	// landed save where it is one of that launch's.
	run PendingRun

	// continued is the capture of the job before that the launch's box took
	// up and continued, as the launch was asked to ([LaunchRequest.Session]);
	// nil where it continued none — a session of its own, the one its
	// conversation's last turn left, or the copy its job before left in the
	// box, continued in place, which no capture places.
	continued *SessionCapture
}

// placeAtNewest places c, a save of the session from.run's launch is still
// working in, at the NEWEST save point that launch is known to hold (see the
// file comment): its save point ([SessionCapture.Through]) and, off the same
// capture, the places an older build reads it by — or the round's start, zero,
// where the launch is known to hold none of its round.
func (c *SessionCapture) placeAtNewest(from savePlaces) {
	var newest *SessionCapture
	point := 0
	for _, held := range from.held() {
		if at, stated := held.SavePoint(); stated && (newest == nil || at > point) {
			newest, point = held, at
		}
	}
	c.Through = new(point)
	c.BridgeSeq, c.BridgeOpen = 0, nil
	if newest != nil {
		c.BridgeSeq, c.BridgeOpen = newest.BridgeSeq, slices.Clone(newest.BridgeOpen)
	}
}

// held is every capture p's launch's running session is known to hold, each of
// a save point counted in its own round's log.
//
// THE KEPT CAPTURE ONLY WHERE IT IS THIS LAUNCH'S: the row's kept capture is
// the turn's newest landed one, which before this launch's first save lands is
// an earlier job's — one this launch may never have continued. THE CONTINUED
// ONE ONLY WHERE THE LAUNCH CARRIED ITS ROUND ON ([PendingRun.Round], which
// names the job that opened the round): a launch that opened a round began a
// log that numbers its calls from its start, and a capture of the round
// before counts another log's. A row that names no round says neither, and is
// read as the safe one, a new round's.
func (p savePlaces) held() []*SessionCapture {
	var held []*SessionCapture
	run := p.run
	if kept := run.Kept; kept != nil && kept.Launch == run.LaunchID && kept.LaunchSeq == run.LaunchSeq {
		held = append(held, kept)
	}
	if continued := p.continued; continued != nil && run.Round != "" && run.Round != run.LaunchID {
		held = append(held, continued)
	}
	return held
}
