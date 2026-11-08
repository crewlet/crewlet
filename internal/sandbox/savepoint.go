package sandbox

import "slices"

// A SAVE'S SAVE POINT, WHERE NO READ MADE NOW CAN PLACE IT.
//
// A capture is placed by one read of its round's log made before its pack
// ([SessionCapture.placeAt]) ONLY WHERE ITS HARNESS ENDED ITS OWN RUN — the job
// over, its harness gone cleanly by itself and its script packing it: every
// call the log had taken by then had been answered before the pack began, and
// a harness that ends its own run writes every message it made before it
// exits, so its session holds each. Every other capture is packed where its
// harness may not have written every call it made, and a harness writes a call
// to its record only once the model's whole message has streamed: measured on
// Claude Code 2.1.310 against a scripted model, a call is made — and can be
// answered — before its tool_use line is in the transcript (eleven of thirteen
// timed calls started with no line there, and five quick ones were answered
// with neither their call nor their result written). Those captures are:
//
//   - a SAVE, and the TAKE OF A JOB STILL WORKING (one its box would not end,
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
// count would never be told those. Each is placed at the NEWEST SAVE POINT ITS
// LAUNCH IS ALREADY KNOWN TO HOLD, each one sound by itself, since what a
// session holds only grows:
//
//   - the save point of the launch's newest landed capture: that session's
//     archive only grows, so every call it held, this one holds;
//   - the save point of the capture the launch continued, where it carried its
//     round on: the brief that continued it told the run every call after it;
//   - zero, for a launch that continued nothing of its round — a session of
//     its own, the one its conversation's last turn left, or a capture of the
//     round before, a send-back's, whose point counts another round's calls:
//     its brief handed it what it needed, and every call of its round is then
//     told again.
//
// A continuation from such a capture is told every call after that point, a
// subagent's included, and the ones its harness had already recorded are told
// twice, as a continuation already accepts ([SessionCapture.Through]): told
// twice, never not at all. The places an older build reads a save point by
// come from the same capture as the point ([SessionCapture.BridgeSeq],
// [SessionCapture.BridgeOpen]), so a peer reading them is never told less.

// savePlaces is what a capture whose harness may not have written every call it
// made — a save, the take of a job still working, a capture of a job cut off —
// is placed by ([SessionCapture.placeAtNewest]).
type savePlaces struct {
	// run is the run's row as the capture read it, its launch the one whose
	// session is taken: its kept capture ([PendingRun.Kept]) is that launch's
	// newest landed capture where it is one of that launch's.
	run PendingRun

	// continued is the capture of the job before that the launch's box took
	// up and continued, as the launch was asked to ([LaunchRequest.Session]);
	// nil where it continued none — a session of its own, the one its
	// conversation's last turn left, or the copy its job before left in the
	// box, continued in place, which no capture places — and nil where the
	// reader cannot say which it continued: a launch records none on its row,
	// so the take of a job still working, and of one cut off, hands none, and
	// is placed at its launch's own newest landed capture or at zero, which
	// tells more, never less.
	continued *SessionCapture
}

// placeAtNewest places c, a capture of the session from.run's launch worked in
// that its harness may not have written whole, at the NEWEST save point that
// launch is known to hold (see the file comment): its save point
// ([SessionCapture.Through]) and, off the same capture, the places an older
// build reads it by — or the round's start, zero, where the launch is known to
// hold none of its round.
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

// held is every capture p's launch's session is known to hold, each of a save
// point counted in its own round's log.
//
// THE KEPT CAPTURE ONLY WHERE IT IS THIS LAUNCH'S: the row's kept capture is
// the turn's newest landed one, which before this launch's first capture lands
// is an earlier job's — one this launch may never have continued. THE CONTINUED
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
