package executor

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// qwenSummaryCapture reads a Qwen Code chat file's newest summary on the walk
// a read takes of the file anyway, its count's ([qwenCounting.summarize]) or
// its notes' ([qwenNotesHeld]): each line's system payload by its
// transcript's own steps ([qwenCompaction]), taken as that walk reads the
// payload for its own ([lineWalk.qwenPayload], [qwenPreserved.read]), so the
// read holds the summary's words and nothing else of the line, to
// [summaryReadMaxBytes] as every harness's read holds them; the newest
// compaction record's summary, or why it cannot be said, once its line read
// whole.
type qwenSummaryCapture struct {
	compaction qwenCompaction
	read       *summaryRead
}

// newQwenSummaryCapture is a capture of the newest summary into read.
func newQwenSummaryCapture(read *summaryRead) *qwenSummaryCapture {
	s := &qwenSummaryCapture{read: read}
	s.compaction.reset(summaryReadMaxBytes)
	return s
}

// summarize makes c read the newest summary on its walk.
func (c *qwenCounting) summarize() { c.sum = newQwenSummaryCapture(new(summaryRead)) }

// summary is the newest summary the walk read, the walk ended with err:
// unread where it did not read the file to its end.
func (c *qwenCounting) summary(err error) sandbox.ThreadSummary {
	if err != nil {
		c.sum.read.fail("the session's chat file was not read to its end: " + err.Error())
	}
	return c.sum.read.reading()
}

// payload is where a line's system payload is read for the summary, nil for
// a walk that reads none.
func (s *qwenSummaryCapture) payload() *qwenCompaction {
	if s == nil {
		return nil
	}
	return &s.compaction
}

// line takes the line l the count's walk just read, object where it is one
// JSON object read whole: a compaction record's summary ([qwenSummaryCapture.take]).
func (s *qwenSummaryCapture) line(object bool, l *qwenCountLine) {
	if s != nil && object && l.kind == "system" && l.subtype == "chat_compression" {
		s.take()
	}
}

// noteLine takes the line l the notes' walk just read: a compaction record's
// summary ([qwenSummaryCapture.take]), and one past the bound, which may have
// been a compaction, unsaid.
func (s *qwenSummaryCapture) noteLine(line noteLine, l *qwenNoteLine) {
	switch {
	case s == nil:
	case line.past:
		s.read.unshaped(fmt.Sprintf("a line of the chat file runs past the %d bytes a line is read to", maxLineBytes))
	case line.object && l.typ == "system" && l.subtype == "chat_compression":
		s.take()
	}
}

// take makes the compaction record's summary the newest, or one that cannot
// be said: a payload of no shape, or a summary past the bound.
func (s *qwenSummaryCapture) take() {
	switch words, shaped, past := s.compaction.words(); {
	case !shaped:
		s.read.unshaped(qwenCompactionUnread)
	case past:
		s.read.unshaped(errSummaryPast.Error())
	default:
		s.read.take(words)
	}
}
