package sandbox

import "slices"

// WHAT CLAUDE CODE COUNTS A SESSION AT BEFORE ITS NEXT REQUEST, read off the
// session's own main thread ([ResumedCount]).
//
// A relaunch that continues a session is told what the engine counts the
// session as holding: of the copy a job's box holds whose harness ended its own
// run, every call of the round but the late ones, every note it read and its
// brief; of a kept capture, the calls at or below its save point. Claude Code
// may not hand its model that session. Before every request, the first of a
// resumed run included, it compares its own count of the conversation (its last
// counted reply's input and output, an estimate of everything after it, and
// what the server cleared) with its window less its reply reserve and a
// 13,000-token buffer, and where the count reaches that point it COMPACTS the
// session first, leaving the model a summary of everything before its last
// reply (read in 2.1.310's and 2.1.322's shipped code, and replicated to the
// token on both). The engine's floor continues a session wherever its last
// request's input and the brief fit the window less 20,000 tokens, so a
// continuation inside that band reached its model as a summary, and whatever
// the engine counted as held could be gone from it.
//
// So the engine bounds that count FROM ABOVE, at both places a session's copy
// is read whole: as a capture is taken, over the kept archive
// ([SessionCapture.ResumedCount]), and as the read that collects a job reads the
// copy its box holds, in one read of the job's main thread
// ([LaunchRecord.ResumedCount], mainthread.go). The relaunch judges the bound
// against Claude Code's own threshold ([ExecutorWindow.SendsWhole]), and a
// continuation it does not prove whole is told as a cut-off one
// (internal/agent/runner's firstRequest). What the bound cannot see is a MARK,
// kept apart from the figure and naming why ([CountMark]), every one of which
// reads as a session that may compact.
//
// NEVER CARRIED ALONG A LINEAGE, unlike the measure of a job's copy
// ([LaunchRecord.Context]): a count is the read of THIS job's copy at its
// collection, or of THIS archive at its take, and a carried count could
// under-state a copy that grew. The next launch's record starts with none.

// CountMark is why a [ResumedCount] holds no bound: what a whole read of the
// session's main thread could not bound by its bytes, or why the thread was not
// read whole. Every mark reads as a session that may compact at its first
// request.
type CountMark string

const (
	// CountUnread is a thread not read whole: no session or one whose id names
	// no file, a job not over, no main thread or one that is no regular file,
	// an archive member named absolutely, with a `..` element or twice, a line
	// the line layer refuses, a thread or a line past its bound, a reader's or
	// a look's error, the read's wait spent, no chained line at all, a newest
	// restart whose uuid does not read, and a runner that reads no thread.
	CountUnread CountMark = "unread"
	// CountRelease is a thread written by a Claude Code release the count's
	// rules were not read in: none named, or one below the count's floor
	// (internal/sandbox/executor's claudeCountFloor, 2.1.310).
	CountRelease CountMark = "release"
	// CountPrecompact is a session holding a saved compaction Claude Code
	// swaps in for a summary where its threshold fires
	// (`projects/<project>/<session>/precompact.json`, or the sibling
	// `<session>.precompact.json`).
	CountPrecompact CountMark = "precompact"
	// CountCompacted is a thread whose last main compact boundary has no
	// counted reply after it: the segment the compaction preserved sits
	// before the boundary in the file, off the chain the count reads, while
	// the harness carries it into its next request.
	CountCompacted CountMark = "compacted"
	// CountUsage is a reply since the last boundary whose usage does not read
	// as Claude Code counts it, or one with no usage that names no message
	// id.
	CountUsage CountMark = "usage"
	// CountCleared is a reply since the last boundary that says the server
	// cleared old tool results, whose cleared tokens Claude Code counts back
	// and no line states.
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

// maxCountMarkBytes is the longest mark a record carries ([ResumedCount.Mark]).
//
// THIRTY-TWO BYTES: this build's longest word is ten (`precompact`), and the
// rest is room for a later build's. A capture's stamp naming a longer one is
// refused ([SessionCapture.Bounded]), and the store writes a longer one on a
// job's record as [CountUnread] ([LaunchRecord.counted]), so the row's model
// of its widest count (TestTheWholeRowFitsItsBudget) is a bound.
const maxCountMarkBytes = 32

// RestartIDMaxBytes is the longest restart uuid a count names
// ([ResumedCount.Restart]): a uuid's thirty-six, as every chained record
// measured carries one. A newest restart whose uuid is longer, empty or absent,
// or does not read, is no identity, and its count is [CountUnread].
const RestartIDMaxBytes = 36

// ResumedCount is at least what Claude Code counts a session it resumes at
// before the brief and what the resume's own turn attaches (window.go's
// compaction arithmetic), read whole off the session's own main thread
// (internal/sandbox/executor's resumedReading), or why no such bound could be
// read; the least context window the run that left the copy reported holding a
// model to; and the newest place the thread restarts at. CLOSED: a later build
// that knows more adds a key beside it on the record holding it (wirekeys.go's
// other way), never a field to it, so it carries no Extra.
type ResumedCount struct {
	// Tokens is the bound; 0 where Mark says there is none.
	Tokens int64 `json:"tokens,omitempty"`
	// Mark is what the bound could not see, "" where Tokens is one.
	Mark CountMark `json:"mark,omitempty"`
	// Window is the least contextWindow the run that left the copy reported
	// for any model it used (Claude Code's result line), 0 where it reported
	// none. It only ever lowers the window a continuation is judged under
	// ([ExecutorWindow.SendsWhole]).
	Window int64 `json:"window,omitempty"`
	// Restart is the uuid of the newest record of the main thread where the
	// session its resume hands its model starts over (the line layer's restart
	// rule: a compaction's boundary, a microcompaction's, a span replaced, a
	// later root, a first chained record that roots nothing), "" where the
	// thread restarts nowhere. A job whose newest restart differs from the one
	// its launch's session held restarted after its brief
	// ([LaunchRecord.BasisRestart]).
	//
	// NIL ONLY WHERE THE THREAD WAS NOT READ WHOLE, OR NOT BY THE RULES OF ITS
	// RELEASE ([ResumedCount.Restarted]): [CountUnread], a newest restart whose
	// uuid does not read (no identity) among its causes; the box's read that
	// found a saved compaction before it opened the thread ([CountPrecompact]
	// there); and [CountRelease]. EVERY OTHER MARK LEAVES IT KNOWN: compacted,
	// usage, cleared, pdf, media and a kept archive's precompact say what a
	// whole thread's bytes cannot bound, never where it restarts, and the
	// restart rule ran over every line of it, so its newest restart is exact.
	// A launch handed such a session records that restart as a known basis, so
	// its job reads as restarted only for a restart of its own; read as no
	// basis, any restart the thread already held would read as the job's
	// ([RestartedSince]).
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

// Worse is the count a continuation of a copy that is c or o, whichever its
// harness resumes, is judged by: none where either is none, since either may
// be the copy resumed; else either's mark, the first in precedence, since a
// mark is no bound; else the larger bound, beside the smaller window either
// reported. The restart is c's: where the two are a capture and the copy in
// its job's box, the capture names the restart the job's launch is compared
// with.
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
// written as [CountUnread], which reads the same way as the word it replaces;
// and a restart uuid longer than [RestartIDMaxBytes] as none read, the count
// [CountUnread] with it WHATEVER ITS MARK, as the reading of a thread whose
// newest restart's uuid does not read writes it (unread comes first in
// precedence), so no record keeps a mark that leaves the identity known beside
// a restart it does not name ([ResumedCount.Restart]).
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

// RestartedSince reports whether a session whose thread restarted last at
// basis, as the launch of a job knew it ([LaunchRecord.RestartBasis]), restarted
// again in that job, as count, a whole read of the job's copy (its
// collection's, or a capture's of it), finds it; or whether it did cannot be
// read. A restart the job appended lies after its brief, the first record a job
// appends, and a resume hands its model nothing before a restart but what the
// restart left (a compaction's summary, or nothing at all), so a job that
// restarted may hand its next run a session that lacks its brief.
//
// BY IDENTITY, NEVER BY COUNT: the harness's trim of a long thread after a
// compaction keeps only the history from its last compaction on, so a thread
// can hold fewer restarts than its basis did and still have restarted. The trim
// keeps the newest restart, so a trim with no restart of the job's own changes
// nothing here.
//
// TRUE ON EVERY SIDE IT CANNOT READ: a count that did not read the thread
// whole, or not by the rules of its release, or no count
// ([ResumedCount.Restarted]); and a basis not known, where the thread restarts
// anywhere at all.
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
