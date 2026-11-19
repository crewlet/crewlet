package executor

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's output is read back as a STREAM, a line at a time, never whole:
// stdout and stderr grow without bound over a long run, while what the engine
// keeps of either (a transcript, a failure's end) is bounded, so nothing is
// refused for its size.

// Decoder reads one harness's event stream a line at a time: ONE STREAM per
// decoder ([Harness.Events]), read either WHOLE (every line, then
// [Decoder.Result]) or LIVE across many reads ([Decoder.Entries],
// [Runner.Follow]).
//
// ITS STATE MUST DEPEND ONLY ON THE LINES FED, never on where one read ended:
// a call is named by its result in a later read, and skipped lines are said
// where they end. Otherwise two owners whose reads fell differently derive
// different text, and the live view's digest resets on every owner move.
type Decoder interface {
	// Line is one complete line, without its line break; the slice is reused
	// once Line returns. An unknown line is skipped, never an error: a stream
	// read mid-write ends in a partial object, and a newer harness prints
	// unknown event types.
	Line(line []byte)

	// Skipped is a line of n bytes past bound, not read. The decoder says so
	// where it was, naming the bound, so a gap never reads as continuity.
	Skipped(n int64, bound int)

	// Result is what the stream said: for a harness whose result is its stream,
	// the whole of it; for one whose result is read apart, its transcript.
	Result() sandbox.Result

	// Entries hands over the entries decoded since it was last asked, in
	// order, and FORGETS them, or an hours-long live reading would hold the
	// whole transcript. A decoder read this way stops keeping what Result
	// needs, so it is read one way or the other, never both: a live reading
	// asks once before its first line, so the decoder never holds for a
	// Result, however much that line's read hands it.
	Entries() []string
}

// maxLineBytes is the longest line of a run's output that is read; a longer
// one is skipped and counted ([Decoder.Skipped]).
//
// A LINE IS READ IN ONE PIECE, so it takes the bound on a piece read whole
// ([sandbox.MaxFileBytes]): one line costs at most what one file read could.
// A runner reads it through [Runner.lineBound], which its suite lowers.
const maxLineBytes = sandbox.MaxFileBytes

// eachLine feeds r to dec a line at a time, in memory bounded by bound; a
// longer line is counted, never held, and reported to [Decoder.Skipped].
//
// The last line need not end in a line break: it is handed on, and a running
// stream's partial last line is the decoder's to recognise.
func eachLine(r io.Reader, dec Decoder, bound int) error {
	return readLines(bufio.NewReaderSize(r, lineChunkBytes), bound,
		func(line []byte) error {
			dec.Line(line)
			return nil
		},
		func(n int64) error {
			dec.Skipped(n, bound)
			return nil
		})
}

// lineChunkBytes is the read buffer; a longer line is gathered across chunks,
// so it bounds how many reads a line takes, not how long one may be.
const lineChunkBytes = 64 << 10

// readLines hands line every line br reads up to bound bytes, without its
// line break (the slice reused), and skipped the size of every longer line,
// which is counted, never held. The first error either returns ends the read
// (how [ReadTranscript] stops early), as does br's own failure, and the line
// it cut short is handed to neither.
//
// THE BUFFER IS THE CALLER'S, so a reader of many records (a transcript per
// subagent) resets one. ONE IMPLEMENTATION for [eachLine] and a captured
// session's transcript, so there is one answer to how long a line may be.
func readLines(br *bufio.Reader, bound int, line func([]byte) error, skipped func(int64) error) error {
	var (
		held []byte
		over int64 // the size of a line already past the bound
	)
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case over > 0:
			over += int64(len(chunk))
		case len(held)+len(chunk) > bound:
			over = int64(len(held) + len(chunk))
			// RELEASED, not truncated: the buffer has grown to the bound,
			// and keeping it would hold that much for the rest of the read.
			held = nil
		default:
			held = append(held, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		// A READ THAT FAILED MID-LINE HANDS NOTHING ON: a record cut in two
		// (by a budget, a store that stopped answering) would pass for whole.
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		var stop error
		if over > 0 {
			stop = skipped(over)
			over = 0
		} else if len(held) > 0 {
			stop = line(bytes.TrimRight(held, "\r\n"))
		}
		held = held[:0]
		switch {
		case stop != nil:
			return stop
		case errors.Is(err, io.EOF):
			return nil
		}
	}
}

// decodeAll runs a whole text through a decoder: a harness's Parse for a
// stream already in hand.
func decodeAll(dec Decoder, text string) sandbox.Result {
	// A strings.Reader cannot fail, so neither can this.
	_ = eachLine(strings.NewReader(text), dec, maxLineBytes)
	return dec.Result()
}

// transcriptHoldBytes is how much of a run's transcript a whole reading holds
// raw: the most the engine reads any one text whole, so below it a transcript
// costs what it always has. Past it the transcript is redacted as it arrives
// and only what the run's record keeps is held ([sandbox.TranscriptHalves]).
const transcriptHoldBytes = sandbox.MaxFileBytes

// transcriptLines builds a run's activity transcript one entry at a time, and
// is where a skipped line becomes a note. Each entry is trimmed and a blank one
// dropped, so the transcript is its entries joined by line breaks.
//
// A WHOLE READING PAST THE HOLD STREAMS: its entries so far, then each one
// after, are written to a redacting stream into the halves the record keeps,
// a piece at a time. A live reading takes its entries before its first line
// and after every read ([Decoder.Entries]), so it never streams.
type transcriptLines struct {
	lines []string
	// held is the bytes of lines and the breaks joining them.
	held int
	// live is set once a live reading took entries: held raw always.
	live bool
	// hold is [transcriptHoldBytes] where zero; a suite lowers it.
	hold int
	// newStream is [redact.NewStream] where nil; a suite injects a refusal.
	newStream func(io.Writer) transcriptStream
	// stream is set once a whole reading passed the hold; halves is what it
	// writes, and fedBytes and fedBreaks are what it was fed.
	stream              transcriptStream
	halves              sandbox.TranscriptHalves
	fedBytes, fedBreaks int

	// skipped, skippedBytes and skippedBound are a run of consecutive lines
	// past the bound, said in ONE note where they were, not one per line.
	skipped      int
	skippedBytes int64
	skippedBound int
}

// transcriptStream is a [redact.Stream].
type transcriptStream interface {
	io.Writer
	Close() error
}

func (t *transcriptLines) add(entry string) {
	t.flushSkipped()
	t.push(entry)
}

func (t *transcriptLines) push(entry string) {
	entry = strings.TrimSpace(entry)
	switch {
	case entry == "":
		return
	case t.stream != nil:
		t.feed(entry)
		return
	case t.live || t.held+len(entry)+1 <= cmp.Or(t.hold, transcriptHoldBytes):
		if len(t.lines) > 0 {
			t.held++
		}
		t.held += len(entry)
		t.lines = append(t.lines, entry)
		return
	}
	newStream := t.newStream
	if newStream == nil {
		newStream = func(w io.Writer) transcriptStream { return redact.NewStream(w) }
	}
	t.stream = newStream(&t.halves)
	// Entry by entry, never the whole join at once.
	for _, line := range t.lines {
		t.feed(line)
	}
	t.lines, t.held = nil, 0
	t.feed(entry)
}

// feed writes entry to the stream, after the line break joining it to the
// entry before.
func (t *transcriptLines) feed(entry string) {
	piece := make([]byte, 0, len(entry)+1)
	if t.fedBytes > 0 {
		piece = append(piece, '\n')
	}
	piece = append(piece, entry...)
	t.fedBytes += len(piece)
	t.fedBreaks += bytes.Count(piece, []byte("\n"))
	// A refusal is answered again by Close, which result reads.
	_, _ = t.stream.Write(piece)
}

func (t *transcriptLines) skip(n int64, bound int) {
	t.skipped++
	t.skippedBytes += n
	t.skippedBound = bound
}

func (t *transcriptLines) flushSkipped() {
	if t.skipped == 0 {
		return
	}
	t.push(activityline.Words(activityline.TagNote, fmt.Sprintf("%d line(s) of output, %s, "+
		"not read: past the %s one line of a run's output may hold", t.skipped, humanSize(t.skippedBytes),
		humanSize(int64(t.skippedBound)))))
	t.skipped, t.skippedBytes = 0, 0
}

// result is the transcript and how much of its middle was left out: its
// entries joined, or, once it streamed, what the record keeps of its
// redaction ([sandbox.TranscriptHalves]). A transcript the stream refused is
// WITHHELD: a note saying so stands for it, counted whole, and nothing the
// stream wrote is kept.
func (t *transcriptLines) result() (text string, elidedLines, elidedBytes int) {
	t.flushSkipped()
	if t.stream == nil {
		return strings.Join(t.lines, "\n"), 0, 0
	}
	if t.stream.Close() == nil {
		return t.halves.Bounded()
	}
	lines := t.fedBreaks + 1
	note := activityline.Words(activityline.TagNote, fmt.Sprintf("the run's transcript, %d line(s), %s, is "+
		"not kept: its text could not be redacted in one pass", lines, humanSize(int64(t.fedBytes))))
	return note, lines, t.fedBytes
}

// take hands over the entries added since the last take and forgets them,
// and marks the reading live.
//
// A RUN OF SKIPPED LINES STILL OPEN IS NOT FLUSHED: the next entry says it,
// so a reading in pieces names the same lines as one taken whole.
func (t *transcriptLines) take() []string {
	t.live = true
	lines := t.lines
	t.lines, t.held = nil, 0
	return lines
}

// redactContext is how much a reading that begins inside a stream reads
// BEFORE what it shows, never shown ([settledEnd]), so a credential that began
// earlier is still redacted.
//
// Every credential shape fits on one line but a private key, whose block runs
// at most [redact.MaxKeyBlockBytes] past BEGIN; a window opening inside one
// would show its body with no BEGIN to anchor on (package redact's doc). It is
// the redaction's own bound, so such a key is read whole and the two cannot
// drift.
const redactContext = redact.MaxKeyBlockBytes

// streamEnd is the end of a stream a person or model can be shown: at most
// keep bytes of WHOLE lines ([sandbox.KeepEnd]) from tail's end, redacted
// first, and how many stream bytes came before it. tail is the stream's end
// read [redactContext] past keep, or the whole stream.
//
// SHOWN ONLY WHERE THE CONTEXT SETTLES IT ([settledEnd]): bounded by redacted
// bytes alone, the end could reach back into the context when redaction shrank
// what followed, and show a key whose BEGIN came before the read. A read
// opened inside a line running to the end has no whole line to show, so that
// line's end past the context is kept on a character, marked, rather than
// showing nothing.
func streamEnd(tail sandbox.FileTail, keep int) (string, int64) {
	ctx := 0
	if tail.Before() > 0 {
		ctx = redactContext
	}
	kept, at := settledEnd(string(tail.Data), ctx, keep)
	return kept, tail.Before() + int64(at)
}

// settledEnd is the redacted end of text that its first ctx bytes settle (at
// most keep bytes, as [sandbox.KeepEnd] keeps them), and how much came before
// it: in text's bytes up to the settled start, in redacted bytes past it, as
// [failureDetail] counts what it cuts.
//
// WHAT IS SETTLED ([settledStart]) is shown as the WHOLE text redacts it:
// redacted alone, a key whose BEGIN is in the context reads as cut off, and a
// password whose key is there as no password. So only what both redactions agree on is kept, from
// the line after the last place they part.
func settledEnd(text string, ctx, keep int) (string, int) {
	from, opensLine := settledStart(text, ctx)
	part := redact.Secrets(text[from:])
	start := 0
	if from > 0 {
		start = len(part) - sharedSuffix(redact.Secrets(text), part)
	}
	if start == len(part) {
		return "", from + start
	}
	onLine := start == 0 && opensLine || start > 0 && part[start-1] == '\n'
	if !onLine {
		i := strings.IndexByte(part[start:], '\n')
		if i < 0 || start+i+1 == len(part) {
			// THE END OF ONE LINE begun before what is settled: kept on a
			// character and marked, as a line longer than keep is.
			kept, at := sandbox.KeepEnd(part[start:], keep)
			if at == 0 {
				kept = sandbox.KeepEndMark + kept
			}
			return kept, from + start + at
		}
		start += i + 1
	}
	kept, at := sandbox.KeepEnd(part[start:], keep)
	return kept, from + start + at
}

// settledStart is where what text's first ctx bytes settle begins, and
// whether it opens a line: the first line starting ctx bytes in or later, else
// the first character there, inside the last line.
func settledStart(text string, ctx int) (int, bool) {
	switch {
	case ctx <= 0:
		return 0, true
	case ctx >= len(text):
		return len(text), true
	case text[ctx-1] == '\n':
		return ctx, true
	}
	if i := strings.IndexByte(text[ctx:], '\n'); i >= 0 && ctx+i+1 < len(text) {
		return ctx + i + 1, true
	}
	for ctx < len(text) && !utf8.RuneStart(text[ctx]) {
		ctx++
	}
	return ctx, false
}

// sharedSuffix is how many bytes a and b end with alike.
func sharedSuffix(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			return i
		}
	}
	return n
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
