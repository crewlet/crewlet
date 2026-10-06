package codingagent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// How a run's output is read back: as a STREAM, a line at a time.
//
// A coding agent's stdout is an event log that grows with every tool call, and
// its stderr is whatever it and everything it ran printed. Both used to be
// read WHOLE, and refused past [sandbox.MaxFileBytes] — so a long run was
// refused for the size of its own log: the poll returned before its liveness
// probe and kept a finished-but-hung or dead job's seat busy for good, and a
// collection lost a result its report file held. What the engine keeps of
// either is bounded anyway (a transcript, a failure read from the end), so
// nothing here needs the whole of one in memory, and nothing is refused for
// its size.

// Decoder reads one CLI's event stream a line at a time and says, at the end,
// what the stream told it — for one read of one stream, so a fresh one is
// taken per read ([CLI.Events]).
type Decoder interface {
	// Line is one complete line of the stream, without its line break. The
	// slice is reused once Line returns. A line that is not an event the
	// decoder knows is skipped, never an error: a stream read mid-write ends
	// in a partial object, and a CLI version this build has not met prints
	// event types it does not know.
	Line(line []byte)

	// Skipped is a line past [maxLineBytes], which was not read: n is its
	// size. The decoder says so where the line was, so a reader of what it
	// built is not shown a gap as continuity.
	Skipped(n int64)

	// Result is what the stream said: for a CLI whose result is its stream,
	// the whole of it; for one whose result is read apart, its transcript.
	Result() sandbox.Result

	// Entries hands over the transcript entries decoded since it was last
	// asked, in order, and FORGETS them — what a live reading needs of a
	// stream that may run for hours ([Runner.Follow]), where keeping them
	// would hold the whole transcript in memory for as long as somebody
	// watches. A decoder read this way also stops keeping what only a
	// Result needs, so it is read one way or the other, never both.
	Entries() []string
}

// maxLineBytes is the longest single line of a run's output that is read; a
// longer one is skipped and counted ([Decoder.Skipped]).
//
// A LINE IS READ IN ONE PIECE, so it is held to the one bound the engine
// already puts on a piece it reads whole ([sandbox.MaxFileBytes]): every line
// a whole read of the file could decode before, this still decodes, and what
// memory one line can cost is what one file read could. What has no bound any
// more is the number of lines.
const maxLineBytes = sandbox.MaxFileBytes

// eachLine feeds r to dec a line at a time, in memory bounded by
// [maxLineBytes]: a line longer than that is never held — its bytes are
// counted as they pass and the decoder told its size.
//
// The last line need not end in a line break: a finished stream's last line
// is complete wherever it stops, and a running one's is the decoder's to
// recognise as partial.
func eachLine(r io.Reader, dec Decoder) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var (
		line []byte
		over int64 // the size of a line already past the bound
	)
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case over > 0:
			over += int64(len(chunk))
		case len(line)+len(chunk) > maxLineBytes:
			over = int64(len(line) + len(chunk))
			// RELEASED, not truncated: a line that reached the bound has
			// grown the buffer to it, and keeping that buffer would hold
			// the bound's worth of memory for the rest of the read.
			line = nil
		default:
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if over > 0 {
			dec.Skipped(over)
			over = 0
		} else if len(line) > 0 {
			dec.Line(bytes.TrimRight(line, "\r\n"))
		}
		line = line[:0]
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// decodeAll runs a whole text through a decoder: what a CLI's Parse is for a
// stream already in hand.
func decodeAll(dec Decoder, text string) sandbox.Result {
	// A strings.Reader cannot fail, so neither can this.
	_ = eachLine(strings.NewReader(text), dec)
	return dec.Result()
}

// transcriptLines builds a run's activity transcript one entry at a time, and
// is where a skipped line becomes a note.
type transcriptLines struct {
	lines []string

	// skipped and skippedBytes are a run of consecutive lines past the
	// bound, said in ONE note where they were rather than one per line.
	skipped      int
	skippedBytes int64
}

func (t *transcriptLines) add(entry string) {
	t.flushSkipped()
	t.lines = append(t.lines, entry)
}

func (t *transcriptLines) skip(n int64) {
	t.skipped++
	t.skippedBytes += n
}

func (t *transcriptLines) flushSkipped() {
	if t.skipped == 0 {
		return
	}
	t.lines = append(t.lines, fmt.Sprintf("(%d line(s) of output, %s, not read: past the %s one "+
		"line of a run's output may hold)", t.skipped, humanSize(t.skippedBytes), humanSize(maxLineBytes)))
	t.skipped, t.skippedBytes = 0, 0
}

func (t *transcriptLines) String() string {
	t.flushSkipped()
	return strings.TrimSpace(strings.Join(t.lines, "\n"))
}

// take hands over the entries added since the last take and forgets them.
//
// A RUN OF SKIPPED LINES STILL OPEN IS NOT FLUSHED: its note is said where the
// run ends, by the next entry, exactly as a whole read says it — so a reading
// taken in pieces names the same lines as one taken whole.
func (t *transcriptLines) take() []string {
	lines := t.lines
	t.lines = nil
	return lines
}

// redactContext is how much MORE than it keeps a read from a stream's end
// takes, so a credential that began before the kept window is still
// recognised and redacted.
//
// Every credential shape the redaction pass knows fits on one line except a
// private key, whose block runs from its BEGIN line to an END line at most
// [redact.MaxKeyBlockBytes] after it. A window opening inside such a block
// would show its base64 body with no BEGIN for the rule to anchor on; with
// this much read before the window, any key that reaches into it is read
// whole and redacted as one — and the bound is the redaction's own, so the two
// cannot drift.
const redactContext = redact.MaxKeyBlockBytes

// streamEnd is the end of a stream as text a person or a model can be shown:
// at most keep bytes of WHOLE lines from the end of tail, redacted, and how
// many bytes of the file came before what is shown.
//
// REDACTED BEFORE IT IS BOUNDED, for the reason [redactContext] exists, and
// on WHOLE LINES by the one rule every stream's end is shown by
// ([sandbox.KeepEnd]).
func streamEnd(tail sandbox.FileTail, keep int) (string, int64) {
	data, partial := tail.Lines()
	text := redact.Secrets(string(data))
	kept, at := sandbox.KeepEnd(text, keep)
	return kept, tail.Before() + int64(partial) + int64(at)
}

// humanSize renders a byte count as a reader would say it.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
