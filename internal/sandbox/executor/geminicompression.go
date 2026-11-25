package executor

import "bufio"

// A GEMINI CLI COMPRESSION IS FOUND BY ITS LINES, never by its summary's
// words: 0.70.0 writes one in a single sync of its history
// (updateMessagesFromHistory) that opens `{"$set":{"sessionId":…}}`, then the
// summary as a user entry, then its own "Got it" reply (measured). The summary
// is the model's own text and may open with anything (a `<scratchpad>` before
// its `<state_snapshot>`), and a `$set` of the session alone also opens every
// resume, so the compression is the three lines together, by their forms
// below: every reader of the chat file (the transcript, the take's confirm,
// the count, the holdings walk) and the pack's trim find it by them alone.

// geminiAck is the reply 0.70.0 writes after a compression's summary.
const geminiAck = "Got it. Thanks for the additional context!"

// The three lines of a compression, as 0.70.0 writes them: the `$set` that
// opens its sync, the summary's user entry (its opening), and the reply.
var (
	geminiCompressionSet  = lineForm{whole: true, pieces: []string{`{"$set":{"sessionId":"`, `"}}`}}
	geminiCompressionUser = lineForm{pieces: []string{`{"id":"`, `","timestamp":"`, `","type":"user","content":[`}}
	geminiCompressionAck  = lineForm{whole: true, pieces: []string{`{"id":"`, `","timestamp":"`,
		`","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`}}
)

// geminiCompressionWatch follows a chat file a reader streams for its
// compressions' summaries ([geminiCompressionSet]), peeking at each line before
// it is read and at the next once it has been ([peekLine]).
type geminiCompressionWatch struct {
	// set says the line before the current one opens a compression's sync;
	// opens that the current one opens as its summary would, after one.
	set, opens bool
}

// next is told the line lines reads next, before any of it is read.
func (w *geminiCompressionWatch) next(lines *bufio.Reader) {
	line := peekLine(lines)
	w.opens = w.set && geminiCompressionUser.holds(line)
	w.set = geminiCompressionSet.holds(line)
}

// summary reports whether the line [next] was last told, read whole now, is a
// compression's summary: it opened as one, after its sync's `$set`, and the
// line after it is the reply.
func (w *geminiCompressionWatch) summary(lines *bufio.Reader) bool {
	return w.opens && geminiCompressionAck.holds(peekLine(lines))
}

// geminiCompressionLines follows a chat file a reader takes whole line by
// line for its compressions' summaries, which it learns of a line late: the
// summary is known once the reply after it is read. ID is the reader's id of
// a record.
type geminiCompressionLines[ID comparable] struct {
	set, opened bool
	opens       ID
}

// line is told each line, raw, and the id of the user entry it holds, where
// user says it holds one; it is the id of the summary the line before it was,
// where summary says it was one.
func (c *geminiCompressionLines[ID]) line(raw []byte, id ID, user bool) (prior ID, summary bool) {
	if c.opened && geminiCompressionAck.holds(raw) {
		prior, summary = c.opens, true
	}
	c.opened = c.set && user && geminiCompressionUser.holds(raw)
	c.opens = id
	c.set = geminiCompressionSet.holds(raw)
	return prior, summary
}
