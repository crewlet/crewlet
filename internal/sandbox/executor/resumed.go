package executor

import (
	"bytes"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT CLAUDE CODE COUNTS A SESSION AT BEFORE ITS NEXT REQUEST, bounded from above
// off the session's own main thread ([resumedReading], internal/sandbox's
// resumed.go).
//
// THE HARNESS'S OWN RULE, run forward. Before every request, the first of a
// resumed run's included, Claude Code walks its conversation back to an
// ANCHOR: a main compact boundary (counted 0), or else the last main-thread
// reply whose usage counts (a usage present, its input side above zero, its
// model not `<synthetic>`, not a provisional `isUnmetered` record), counted at
// its input, cache writes, cache reads and OUTPUT and placed at the FIRST record
// of its message id (a reply is written a record a block, each repeating its
// usage). It adds an estimate of every record after that place, and the tokens
// the server cleared from old tool results since the last boundary, and
// compacts where the sum reaches its threshold (read in 2.1.310's and
// 2.1.322's shipped code; replicated to the token on both, 167,834 at a
// compaction both made).
//
// THE BOUND. This reading takes the anchor's four counts as written, through
// the line layer's one rule for a reply's usage ([threadLine.Usage]), and
// charges every chained line after the anchor's place at [sandbox.ClaudeCharge],
// two bytes a token of the line and its break, at most every rate the harness
// counts a block at (text at three or four a UTF-16 unit, a JSON-titled
// document at two, a call at its JSON over three or four), each block's syntax
// bytes covering its rounding. So the bound is never below the harness's count,
// measured so at every one of 186 real compactions and 376,937 cut points of
// real sessions, and in every probe job.
//
// WHAT ITS BYTES CANNOT BOUND IS A MARK ([sandbox.CountMark]), never a figure: a
// PDF read after the anchor (3,000 tokens a page), an image or a document after
// it (2,000 flat), a server clearing since the last boundary (the cleared
// tokens no line states), a reply whose usage the layer cannot read as the
// harness counts it, and a boundary with no counted reply after it, whose
// preserved segment sits before the boundary in the file while the harness
// carries it. A line the layer refuses, a thread past its bound and a read cut
// short leave the thread unread: no bound at all.
//
// READ THROUGH THE LAYER ALONE: the head, Usage, ToolResultsCleared, UUID and
// the line's raw bytes, under the layer's one set of options. It never asks a
// line's Message: it decides nothing from what a message says, so a reshaped
// block never costs it the count, and the raw charge covers whatever a block
// holds.
//
// Over-reading, stated: every chained line after the anchor's place is
// charged, a prompt snapshot among them, which the harness counts as nothing
// (a fresh job's second snapshot, about 95 KB, over-read by about 48,000
// tokens, only where a job ended before its second reply); the harness's skip
// of a reply whose first block is one of its five placeholder texts is not
// read (a reply anchored there carries a real request's usage); and a usage's
// own counts are read where it lists iterations whose counted entry agrees,
// else the usage is a mark ([threadLine.Usage]).
//
// AND WHERE THE THREAD RESTARTS LAST, by the line layer's restart rule
// ([threadRestarts.See]) run over every line the walk reads: the uuid of the
// newest line it flags ([sandbox.ResumedCount.Restart]), which a later relaunch
// compares with the restart the job's launch handed it, to know whether the job
// restarted the session after its brief. The COUNT restarts at a main compact
// boundary alone, Claude Code's counting rule; the IDENTITY follows every
// restart the rule flags, since a resume hands its model nothing before any of
// them, and is named beside every mark but where the thread was not read
// whole, or not by the rules of its release ([resumedReading.count]).

// claudeCountFloor is the oldest Claude Code release whose thread a count is
// read off: 2.1.310, the older of the two releases (2.1.310 and 2.1.322) whose
// count records, anchor rule, estimate and compaction threshold were read and
// replicated. A thread written by an older release, or by one that names no
// release, is [sandbox.CountRelease].
//
// NEVER BELOW [claudeSettingsPinFloor]: the threshold the count is judged
// against is the one the run's settings pin holds wherever a repository's own
// settings could move it, so the count is read only where the pin is in force
// (a test holds the two).
const claudeCountFloor = "2.1.310"

// countMarks is a set of [sandbox.CountMarks], by their places in that order.
type countMarks uint16

// with is m and mark.
func (m countMarks) with(mark sandbox.CountMark) countMarks {
	for i, known := range sandbox.CountMarks {
		if known == mark {
			return m | 1<<i
		}
	}
	return m
}

// first is the first mark of m in [sandbox.CountMarks]' order, "" where m holds
// none.
func (m countMarks) first() sandbox.CountMark {
	for i, known := range sandbox.CountMarks {
		if m&(1<<i) != 0 {
			return known
		}
	}
	return ""
}

// The exact bytes a line holds where its record carries what the count cannot
// bound, each a block's or an attachment's own type as JSON.stringify writes it,
// so a call's input naming a key `image` marks nothing; and the provisional
// record the harness never anchors on; and the server's clearing edit, whose
// cleared tokens the harness counts back whatever its count of cleared uses.
var (
	pdfReadMark  = []byte(`"type":"message_pdf_reads"`)
	imageMark    = []byte(`"type":"image"`)
	documentMark = []byte(`"type":"document"`)
	unmetered    = []byte(`"isUnmetered":true`)
	clearingEdit = []byte(`"` + editClearToolUses + `"`)
)

// claudeSynthetic is the model Claude Code names on a reply it wrote itself, as
// a resume writes one for a turn it found unanswered and an API error writes
// one: never counted, never an anchor.
const claudeSynthetic = "<synthetic>"

// lineMarksOf is what raw, one line, carries that the count cannot bound by its
// bytes: a PDF read, an image or a document.
func lineMarksOf(raw []byte) countMarks {
	var m countMarks
	if bytes.Contains(raw, pdfReadMark) {
		m = m.with(sandbox.CountPDF)
	}
	if bytes.Contains(raw, imageMark) || bytes.Contains(raw, documentMark) {
		m = m.with(sandbox.CountMedia)
	}
	return m
}

// resumedReading is the count's reading of one main thread ([threadReading]):
// handed every whole line in file order, it keeps the harness's anchor and what
// lies after its place, and answers the bound or the mark ([resumedReading.count]).
type resumedReading struct {
	// tokens is the anchor's four counts; run the charge since the first
	// record of the newest run of one message id (runID), outside that
	// record's own; tail the charge of everything after the anchor's place.
	// runMarks and tailMarks are the line marks carried as run and tail are.
	tokens, run, tail   int64
	runID               string
	runMarks, tailMarks countMarks
	// thread is the thread marks since the last boundary (a usage that does
	// not read, a clearing); compacted is a boundary with no anchoring reply
	// after it yet; chained whether any chained line was read.
	thread    countMarks
	compacted bool
	chained   bool
	// unread is a thread not read whole: a line the layer refused, or a walk
	// that ended on an error.
	unread bool

	// restarts follows the thread's restarts by the layer's rule, and newest
	// is the uuid of the newest line it flagged, and what reading it met;
	// flagged whether any was.
	restarts threadRestarts
	newest   string
	newestOf error
	flagged  bool
}

var _ threadReading = (*resumedReading)(nil)

// line reads one whole line.
func (r *resumedReading) line(l threadLine) {
	if r.unread {
		return
	}
	if r.restarts.See(l.Head) {
		r.newest, r.newestOf = l.UUID()
		r.flagged = true
	}
	if !l.Head.Chained() {
		return
	}
	r.chained = true
	if l.Head.Type == recordSystem && l.Head.Subtype == subtypeCompactBoundary && l.Head.Main() {
		// A MAIN COMPACT BOUNDARY: the harness's own walk anchors here at 0,
		// and nothing before it counts. Its preserved segment sits before it
		// in the file, so until a counted reply follows, the count is a mark.
		*r = resumedReading{chained: true, compacted: true, restarts: r.restarts, newest: r.newest,
			newestOf: r.newestOf, flagged: r.flagged}
		return
	}
	charge, marks := sandbox.ClaudeCharge(len(l.Raw)+1), lineMarksOf(l.Raw)
	if l.Head.Type != recordAssistant || !l.Head.Main() {
		r.after(charge, marks)
		return
	}
	if cleared, err := l.ToolResultsCleared(); cleared || err != nil || bytes.Contains(l.Raw, clearingEdit) {
		r.thread = r.thread.with(sandbox.CountCleared)
	}
	u, ok, err := l.Usage()
	if err != nil {
		r.thread = r.thread.with(sandbox.CountUsage)
		r.after(charge, marks)
		return
	}
	// ITS ID, which the harness takes off a record with no usage too; none for
	// a reply the harness wrote itself.
	id := u.ID
	if u.Model == claudeSynthetic {
		id = ""
	}
	if !ok && u.ID == "" {
		r.thread = r.thread.with(sandbox.CountUsage)
	}
	if id == "" {
		r.after(charge, marks)
		return
	}
	if id != r.runID {
		r.runID, r.run, r.runMarks = id, 0, 0
	} else {
		r.run += charge
		r.runMarks |= marks
	}
	if ok && u.Input+u.CacheWrite+u.CacheRead > 0 && !bytes.Contains(l.Raw, unmetered) {
		r.tokens = u.Input + u.CacheWrite + u.CacheRead + u.Output
		r.tail, r.tailMarks, r.compacted = r.run, r.runMarks, false
		return
	}
	r.tail += charge
	r.tailMarks |= marks
}

// after charges a chained line that is no record of a counted reply's message
// id to both the tail and the newest run.
func (r *resumedReading) after(charge int64, marks countMarks) {
	r.tail += charge
	r.run += charge
	r.tailMarks |= marks
	r.runMarks |= marks
}

// refused is a whole line that is no record: the thread is not read whole, and
// nothing after it is read.
func (r *resumedReading) refused(*threadLineError) { r.unread = true }

// end is how the walk ended: an error leaves the thread unread; a torn last
// piece is none of the thread, which the box cuts away before a resume.
func (r *resumedReading) end(_ bool, err error) {
	if err != nil {
		r.unread = true
	}
}

// count is the bound the reading holds, or the first mark of callers' and its
// own ([sandbox.CountMarks]' order); and, where the thread was read whole and
// the walk's caller set no unread or release mark, its newest restart's uuid.
func (r *resumedReading) count(callers countMarks) *sandbox.ResumedCount {
	marks := callers | r.thread | r.tailMarks
	if r.unread || !r.chained {
		marks = marks.with(sandbox.CountUnread)
	}
	if r.compacted {
		marks = marks.with(sandbox.CountCompacted)
	}
	restart := r.restart()
	if restart == nil {
		marks = marks.with(sandbox.CountUnread)
	}
	// THE IDENTITY IS BLIND ONLY WHERE THE THREAD WAS NOT READ WHOLE (unread)
	// OR NOT BY THE RULES OF ITS RELEASE (release). Every other mark is the
	// bound's: compacted, usage, cleared, pdf, media and a kept archive's
	// precompact say what the thread's bytes cannot bound, never where it
	// restarts, and the walk ran the restart rule over every line of it
	// ([resumedReading.line]), so its newest restart is exact. Blind there
	// too, a marked count would leave the launch handed its session no basis,
	// and that job would read as restarted for any restart the thread already
	// held ([sandbox.RestartedSince]).
	blind := countMarks(0).with(sandbox.CountUnread).with(sandbox.CountRelease)
	out := &sandbox.ResumedCount{Tokens: r.tokens + r.tail}
	if marks&blind == 0 {
		out.Restart = restart
	}
	if mark := marks.first(); mark != "" {
		out.Tokens, out.Mark = 0, mark
	}
	return out
}

// restart is the uuid of the newest line the restart rule flagged, "" where it
// flagged none, and nil where that uuid does not read: an error, empty, or
// longer than [sandbox.RestartIDMaxBytes], which is no identity.
func (r *resumedReading) restart() *string {
	switch {
	case !r.flagged:
		return new("")
	case r.newestOf != nil || r.newest == "" || len(r.newest) > sandbox.RestartIDMaxBytes:
		return nil
	}
	return new(r.newest)
}
