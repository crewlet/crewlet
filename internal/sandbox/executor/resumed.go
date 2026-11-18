package executor

import (
	"crypto/sha256"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A count bounds from above, off a session's main thread ([resumedReading]),
// what Claude Code counts the session at before its next request.
//
// Claude Code's rule ([claudeCountFloor]'s releases): walk back to an anchor, a
// main compact boundary (0) or the last main-thread reply whose usage counts
// (input side above zero, model not [claudeSynthetic], not `isUnmetered`), taken
// at input, cache writes, cache reads and output and placed at the first record
// of its message id (a reply is a record a block, each repeating its usage);
// add an estimate of every record after it and the tokens the server cleared
// since the last boundary; compact where the sum reaches the threshold.

// The bound takes the anchor's four counts through [threadLine.Usage] and
// charges every chained line ([threadHead.Chained]) after its place at
// [sandbox.ClaudeCharge]: two bytes a token of the line and its break, no more
// than any rate the harness counts a block at, syntax bytes covering rounding;
// so it is never below the harness's count. It reads only the line layer, under
// its one set of options (head, Usage, ToolResultsCleared, UUID,
// [threadLine.Size], [threadLine.Bytes]), never a line's Message, so a reshaped
// block never costs it the count. It over-reads a prompt snapshot (the harness
// counts it as nothing) and does not skip a reply led by one of the harness's
// five placeholder texts (one anchored there carries a real request's usage).

// What the bytes cannot bound is a mark ([sandbox.CountMark]), never a figure:
// a PDF read after the anchor (3,000 tokens a page), an image or a document
// after it (2,000 flat), a server clearing since the last boundary (no line
// states the tokens cleared), a reply whose usage does not read as the harness
// counts it ([threadLine.Usage]), and a boundary with no counted reply after it
// ([resumedReading.line]). A line the layer refuses, a thread past its bound
// and a read cut short leave the thread unread: no bound at all.

// claudeCountFloor is the oldest Claude Code release whose thread a count is
// read off: the older of the two releases (2.1.310, 2.1.322) whose counting,
// anchor rule, estimate and compaction threshold were read and replicated. A
// thread from an older release, or one naming none, is [sandbox.CountRelease].
// Never below [claudeSettingsPinFloor]: the threshold is the one the run's
// settings pin holds against a repository's own settings, so the count is read
// only where the pin is in force (a test holds the two).
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

// The exact bytes, as JSON.stringify writes them, of what the count cannot
// bound: a block's or attachment's own type (so a call's input naming a key
// `image` marks nothing); the provisional record the harness never anchors on;
// and the server's clearing edit, whose cleared tokens the harness counts back
// whatever its count of cleared uses. [byteMarks] and [threadBytes.Has] read each.
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

// lineMarksOf is what b, one line's byte facts, says the line carries that the
// count cannot bound by its bytes: a PDF read, an image or a document.
func lineMarksOf(b *threadBytes) countMarks {
	var m countMarks
	if b.Has(markPDF) {
		m = m.with(sandbox.CountPDF)
	}
	if b.Has(markImage) || b.Has(markDocument) {
		m = m.with(sandbox.CountMedia)
	}
	return m
}

// resumedReading is the count's reading of one main thread ([threadReading]):
// handed every whole line in file order, it keeps the harness's anchor and what
// lies after its place, and answers the bound or the mark ([resumedReading.count]).
// What it keeps is fixed whatever a line holds: never a string the thread chose
// the length of (an id or a uuid runs to [threadLineMax], threefold where the
// decode reads U+FFFD), which kept across the walk beside every other reading
// ([threadChain]) would pass the most a read of one thread holds.
type resumedReading struct {
	// tokens is the anchor's four counts; run the charge since the first
	// record of the newest run of one message id, outside that record's own;
	// tail the charge of everything after the anchor's place. runMarks and
	// tailMarks are the line marks carried as run and tail are.
	tokens, run, tail int64
	// runID is the digest of the newest run's message id, never the id: it
	// tells ids apart as they do ([threadChain] interns reply ids alike) in 32
	// bytes. No id is empty here, so no digest is the zero a reading starts with.
	runID               [sha256.Size]byte
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

	// restarts follows the thread's restarts by the layer's rule; flagged is
	// whether it flagged any line, and newest the uuid of the newest it
	// flagged where newestReads says that uuid is an identity, else "": a uuid
	// past [sandbox.RestartIDMaxBytes] is never kept.
	restarts    threadRestarts
	newest      string
	newestReads bool
	flagged     bool
}

var _ threadReading = (*resumedReading)(nil)

// line reads one whole line.
func (r *resumedReading) line(l threadLine) {
	if r.unread {
		return
	}
	if r.restarts.See(l.Head) {
		r.flagged = true
		r.newest, r.newestReads = "", false
		if uuid, err := l.UUID(); err == nil && uuid != "" && len(uuid) <= sandbox.RestartIDMaxBytes {
			r.newest, r.newestReads = uuid, true
		}
	}
	if !l.Head.Chained() {
		return
	}
	r.chained = true
	if l.Head.Type == recordSystem && l.Head.Subtype == subtypeCompactBoundary && l.Head.Main() {
		// A main compact boundary: the harness anchors here at 0. Its preserved
		// segment sits before it in the file while the harness carries it, so
		// until a counted reply follows, the count is a mark.
		*r = resumedReading{chained: true, compacted: true, restarts: r.restarts, newest: r.newest,
			newestReads: r.newestReads, flagged: r.flagged}
		return
	}
	charge, marks := sandbox.ClaudeCharge(l.Size), lineMarksOf(&l.Bytes)
	if l.Head.Type != recordAssistant || !l.Head.Main() {
		r.after(charge, marks)
		return
	}
	if cleared, err := l.ToolResultsCleared(); cleared || err != nil || l.Bytes.Has(markClearing) {
		r.thread = r.thread.with(sandbox.CountCleared)
	}
	u, ok, err := l.Usage()
	if err != nil {
		r.thread = r.thread.with(sandbox.CountUsage)
		r.after(charge, marks)
		return
	}
	// The harness takes the id off a record with no usage too; none for a
	// reply it wrote itself.
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
	if digest := sha256.Sum256([]byte(id)); digest != r.runID {
		r.runID, r.run, r.runMarks = digest, 0, 0
	} else {
		r.run += charge
		r.runMarks |= marks
	}
	if ok && u.Input+u.CacheWrite+u.CacheRead > 0 && !l.Bytes.Has(markUnmetered) {
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
	// The identity is blind only where the thread was not read whole (unread)
	// or not by its release's rules (release). Every other mark says what the
	// bytes cannot bound, never where the thread restarts, and the walk ran the
	// restart rule over every line, so its newest restart is exact. Blind there
	// too, a marked count would leave the launch no basis, and the job would
	// read as restarted for any restart the thread already held
	// ([sandbox.RestartedSince]).
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

// restart is the uuid of the newest line the restart rule ([threadRestarts.See])
// flagged ([sandbox.ResumedCount.Restart]), which a later relaunch compares
// with the restart its launch handed it, to know whether the job restarted the
// session after its brief; "" where it flagged none, nil where that uuid does
// not read (an error, empty, or past [sandbox.RestartIDMaxBytes]). The count restarts only at a main compact
// boundary, as Claude Code counts; the identity follows every flagged restart,
// since a resume hands its model nothing before any of them.
func (r *resumedReading) restart() *string {
	switch {
	case !r.flagged:
		return new("")
	case !r.newestReads:
		return nil
	}
	return new(r.newest)
}
