package sandbox

import "slices"

// WHAT CLAUDE CODE COUNTS A SESSION AT BEFORE ITS NEXT REQUEST
// ([ResumedCount]). Before every request, a resumed run's first included,
// Claude Code compares its count with its window less its reply reserve and a
// 13,000-token buffer, and there COMPACTS the session to a summary (read in
// 2.1.310 and 2.1.322). The engine's floor continues a session whose last input
// and brief fit the window less 20,000 tokens, so in that band what it counts
// as held may be gone. So the count is bounded FROM ABOVE wherever a copy is
// read whole ([SessionCapture.ResumedCount], [LaunchRecord.ResumedCount]),
// judged by [ExecutorWindow.SendsWhole], a continuation not proven whole told
// as cut off (internal/agent/runner's firstRequest); what the bound cannot see
// is a [CountMark]. NEVER CARRIED, unlike [LaunchRecord.Context]: a carried
// count could under-state a grown copy.

// CountMark is why a [ResumedCount] holds no bound: what a whole read of the
// main thread could not bound by its bytes, or why it was not read whole. Every
// mark reads as a session that may compact at its first request.
type CountMark string

const (
	// CountUnread is a thread not read whole: no session or no file for its
	// id, a job not over, no main thread or no regular file, an archive member
	// named absolutely, with `..` or twice, a line the line layer refuses, a
	// thread past its bound or a line past the layer's bounds, a read or look
	// error, the wait spent, no chained line, a newest restart whose uuid does
	// not read, or a runner reading none.
	CountUnread CountMark = "unread"
	// CountRelease is a thread from a Claude Code release the count's rules were
	// not read in: none named, or one below internal/sandbox/executor's
	// claudeCountFloor (2.1.310).
	CountRelease CountMark = "release"
	// CountPrecompact is a session holding a saved compaction Claude Code swaps
	// in for a summary where its threshold fires
	// (`projects/<project>/<session>/precompact.json`, or the sibling
	// `<session>.precompact.json`).
	CountPrecompact CountMark = "precompact"
	// CountCompacted is a thread whose last main compact boundary has no
	// counted reply after it: the segment the compaction preserved sits before
	// the boundary, off the chain the count reads, yet the harness sends it.
	CountCompacted CountMark = "compacted"
	// CountUsage is a reply since the last boundary whose usage does not read
	// as Claude Code counts it, or one with no usage and no message id.
	CountUsage CountMark = "usage"
	// CountCleared is a reply since the last boundary saying the server cleared
	// old tool results: Claude Code counts those tokens back, and no line
	// states them.
	CountCleared CountMark = "cleared"
	// CountPDF is a PDF read after the counted reply, which Claude Code counts
	// at 3,000 tokens a page, unbounded by its bytes.
	CountPDF CountMark = "pdf"
	// CountMedia is an image or a document after the counted reply, which
	// Claude Code counts at a flat 2,000 tokens, unbounded by its bytes.
	CountMedia CountMark = "media"
)

// CountMarks are the marks this build writes, IN PRECEDENCE ORDER: a count
// that is marked keeps the first of them that applies, one word.
var CountMarks = []CountMark{CountUnread, CountRelease, CountPrecompact, CountCompacted, CountUsage,
	CountCleared, CountPDF, CountMedia}

// Valid reports whether this build knows m. A word it does not know, a later
// build's, is still a mark: it reads as a session that may compact.
func (m CountMark) Valid() bool { return slices.Contains(CountMarks, m) }

// maxCountMarkBytes is the longest mark a record carries ([ResumedCount.Mark]):
// this build's longest is ten (`precompact`), the rest room for a later build's.
// A capture's stamp naming a longer one is refused ([SessionCapture.Bounded]) and
// a job's record writes it as [CountUnread] ([LaunchRecord.counted]), so the
// row's widest count (TestTheWholeRowFitsItsBudget) is a bound.
const maxCountMarkBytes = 32

// RestartIDMaxBytes is the longest restart uuid a count names
// ([ResumedCount.Restart]): a uuid's thirty-six, as every chained record carries.
// A newest restart whose uuid is longer, empty, absent or unreadable is no
// identity, and its count is [CountUnread].
const RestartIDMaxBytes = 36

// ResumedCount is at least what Claude Code counts a resumed session at before
// the brief and the resume's own attachments (window.go's compaction
// arithmetic), read whole off its main thread (internal/sandbox/executor's
// resumedReading), or why no bound could be read; the least window the run that
// left the copy reported; and the thread's newest restart. CLOSED: a later build
// adds a key beside it on its record (wirekeys.go's other way), never a field,
// so it carries no Extra.
type ResumedCount struct {
	// Tokens is the bound; 0 where Mark says there is none.
	Tokens int64 `json:"tokens,omitempty"`
	// Mark is what the bound could not see, "" where Tokens is one.
	Mark CountMark `json:"mark,omitempty"`
	// Window is the least contextWindow the run that left the copy reported
	// for any model (Claude Code's result line), 0 for none. It only ever
	// lowers the window a continuation is judged under
	// ([ExecutorWindow.SendsWhole]).
	Window int64 `json:"window,omitempty"`
	// Restart is the uuid of the main thread's newest record where the session
	// its resume hands its model starts over (the line layer's restart rule),
	// "" where it restarts nowhere. A job whose newest restart differs from its
	// launch's restarted after its brief ([LaunchRecord.BasisRestart]).
	//
	// NIL ONLY WHERE THE THREAD WAS NOT READ WHOLE, OR NOT BY ITS RELEASE'S
	// RULES ([ResumedCount.Restarted]): [CountUnread], [CountRelease], and the
	// box's [CountPrecompact] found before it opened the thread. Every other
	// mark says what the bytes cannot bound, never where the thread restarts,
	// and the restart rule ran over every line, so its restart is exact and a
	// launch records it as a known basis: read as none, a restart the thread
	// already held would read as the job's own ([RestartedSince]).
	Restart *string `json:"restart,omitempty"`
}

// Bound is the count's bound, false where there is none: no count (nil), a
// mark, or a figure that is no count of a session (at or below 0).
func (c *ResumedCount) Bound() (int64, bool) {
	if c == nil || c.Mark != "" || c.Tokens <= 0 {
		return 0, false
	}
	return c.Tokens, true
}

// Restarted is the uuid of the newest restart of the thread the count read, ""
// where it restarts nowhere, and whether that is known: false for no count, and
// for one whose read did not see the thread whole, or not by the rules of its
// release ([ResumedCount.Restart]).
func (c *ResumedCount) Restarted() (string, bool) {
	if c == nil || c.Restart == nil {
		return "", false
	}
	return *c.Restart, true
}

// Worse is the count a continuation of c's or o's copy, whichever its harness
// resumes, is judged by: none where either is none, since either may be
// resumed; else the first mark in precedence, since a mark is no bound; else
// the larger bound, beside the smaller window either reported. The restart is
// c's: of a capture and the copy in its job's box, the capture names the
// restart the launch is compared with.
func (c *ResumedCount) Worse(o *ResumedCount) *ResumedCount {
	if c == nil || o == nil {
		return nil
	}
	out := *c
	switch {
	case c.Mark != "" || o.Mark != "":
		out.Tokens, out.Mark = 0, firstMark(c.Mark, o.Mark)
	default:
		out.Tokens = max(c.Tokens, o.Tokens)
	}
	if o.Window > 0 && (out.Window <= 0 || o.Window < out.Window) {
		out.Window = o.Window
	}
	return &out
}

// firstMark is whichever of a and b comes first in [CountMarks], "" where
// neither is a mark; a word this build does not know comes after every one it
// knows, and is kept where it is the only mark.
func firstMark(a, b CountMark) CountMark {
	rank := func(m CountMark) int {
		if i := slices.Index(CountMarks, m); i >= 0 {
			return i
		}
		return len(CountMarks)
	}
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case rank(b) < rank(a):
		return b
	}
	return a
}

// bounded is c as a record carries it: a mark longer than [maxCountMarkBytes]
// as [CountUnread], which reads the same; a restart uuid longer than
// [RestartIDMaxBytes] as none read and the count [CountUnread] WHATEVER ITS
// MARK, as a thread read with an unreadable restart uuid is, so no record keeps
// a mark that leaves the identity known beside no restart ([ResumedCount.Restart]).
func (c *ResumedCount) bounded() *ResumedCount {
	if c == nil {
		return nil
	}
	out := *c
	if len(out.Mark) > maxCountMarkBytes {
		out.Tokens, out.Mark = 0, CountUnread
	}
	if out.Restart != nil && len(*out.Restart) > RestartIDMaxBytes {
		out.Tokens, out.Mark, out.Restart = 0, CountUnread, nil
	}
	return &out
}

// same reports whether c and o say the same of a session's count, a nil one
// none at all.
func (c *ResumedCount) same(o *ResumedCount) bool {
	switch {
	case c == nil || o == nil:
		return c == o
	case (c.Restart == nil) != (o.Restart == nil):
		return false
	case c.Restart != nil && *c.Restart != *o.Restart:
		return false
	}
	return c.Tokens == o.Tokens && c.Mark == o.Mark && c.Window == o.Window
}

// RestartedSince reports whether a session whose thread last restarted at
// basis, as the job's launch knew it ([LaunchRecord.RestartBasis]), restarted
// again in that job, as count, a whole read of the job's copy, finds it. A
// restart the job appended lies after its brief, its first record, and a resume
// hands its model nothing before a restart but what it left, so the next run
// may lack the brief.
//
// BY IDENTITY, NEVER BY COUNT: the harness trims a long thread to its last
// compaction on, so a thread can hold fewer restarts than its basis and still
// have restarted; the trim keeps the newest restart. TRUE WHEREVER IT CANNOT
// READ: no count, one not read whole or not by its release's rules
// ([ResumedCount.Restarted]), or an unknown basis and any restart at all.
func RestartedSince(basis *string, count *ResumedCount) bool {
	newest, read := count.Restarted()
	switch {
	case !read:
		return true
	case basis == nil:
		return newest != ""
	}
	return newest != *basis
}

// RestartBasis is the newest restart of the session l's job took up, as its
// launch knew it ([LaunchRecord.BasisRestart]): a thread that restarts nowhere
// where the job's box said it started a session of its own, whatever the launch
// handed it, and else what the launch recorded, nil where that is not known.
func (l LaunchRecord) RestartBasis() *string {
	if l.SessionStart == SessionStartFresh {
		return new("")
	}
	return l.BasisRestart
}

// restartOf is the newest restart a count read, as a basis records it
// ([PriorBox.Restart]): nil where it was not read.
func restartOf(c *ResumedCount) *string {
	newest, read := c.Restarted()
	if !read {
		return nil
	}
	return &newest
}
