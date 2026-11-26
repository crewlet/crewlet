package executor

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// claudeSummaryReleases are the Claude Code releases this build pins its
// summary reading to, those its summary shape was read on
// (testdata/claude/<release>/holdings/s1): a set, as the resume's model is
// ([claudeResumeRead]).
var claudeSummaryReleases = []string{"2.1.310", "2.1.322"}

// claudeSummaryLineMaxBytes bounds the line a summary is read off: twice what
// a read holds of a summary ([summaryReadMaxBytes]), so a summary near that
// bound, written with every line break escaped, still fits its record. A
// longer line's summary is past the bound, its words never kept.
const claudeSummaryLineMaxBytes = 2 * summaryReadMaxBytes

// claudeSummaryReading is a [threadReading] finding a Claude Code main
// thread's newest compaction summary: the user record marked
// `isCompactSummary`, not the harness's own (`isMeta`), after a
// compact_boundary, as the transcript reads one ([claudeReading.commit]). Its
// words are read on the walk itself: once a boundary passes, the walk's reader
// streams each record's content into text ([threadReader.Summarizing]) until
// the summary is found, so no second reader reads its line again.
type claudeSummaryReading struct {
	reader *threadReader
	text   summaryText
	// armed says a boundary came since the newest summary; noText that the
	// newest's content was no text, which fails the read at its end.
	armed, noText bool
	read          summaryRead
}

// ask keeps the walk's reader, which reads a summary's words while armed.
func (r *claudeSummaryReading) ask(reader *threadReader) { r.reader = reader }

// arm makes the walk read each record's content as a summary's words, or no more.
func (r *claudeSummaryReading) arm(on bool) {
	r.armed = on
	if on {
		r.reader.Summarizing(&r.text)
	} else {
		r.reader.Summarizing(nil)
	}
}

func (r *claudeSummaryReading) line(l threadLine) {
	switch h := l.Head; {
	case h.Type == recordSystem && h.Subtype == subtypeCompactBoundary:
		r.arm(true)
	case r.armed && h.Type == recordUser:
		summary, err := l.IsCompactSummary()
		meta, metaErr := l.IsMeta()
		switch {
		case err != nil || metaErr != nil:
			r.read.unshaped("a user record after the newest compaction's boundary does not say whether it is " +
				"its summary")
		case summary && !meta:
			r.arm(false)
			r.found(int64(l.Size))
		}
	}
}

// found makes the words the walk read off the summary's line, size bytes with
// its break, the newest summary.
func (r *claudeSummaryReading) found(size int64) {
	r.noText = false
	r.read.unsaid = ""
	switch {
	case size > claudeSummaryLineMaxBytes, r.text.past:
		r.read.unshaped(errSummaryPast.Error())
	case !r.text.set:
		r.noText = true
	default:
		r.read.take(string(r.text.buf))
	}
}

// reading is the newest summary the walk read.
func (r *claudeSummaryReading) reading() sandbox.ThreadSummary {
	if r.noText {
		r.read.fail("the summary's content is no text, the one shape this read holds")
	}
	return r.read.reading()
}

// refused is a line that is no record: after a boundary, it may be its summary.
func (r *claudeSummaryReading) refused(err *threadLineError) {
	if r.armed {
		r.read.unshaped(fmt.Sprintf("line %d, after the newest compaction's boundary, is no record", err.N))
	}
}

func (r *claudeSummaryReading) end(_ bool, err error) {
	if err != nil {
		r.read.fail("the thread was not read to its end: " + err.Error())
	}
}
