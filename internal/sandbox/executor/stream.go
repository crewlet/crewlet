package executor

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"

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

	// Skipped is a line past bound, not read whole. The decoder says so
	// where it was, naming the bound, so a gap never reads as continuity, and
	// reads its ends for a model request ([LongLine]), so a skipped line
	// never hides one.
	Skipped(line LongLine)

	// Undecided is how many skipped lines may hold a model request whose
	// ends showed none the decoder could count: a run with a turn cap and any
	// such line is never judged within it ([Runner.Collect]).
	Undecided() int

	// Result is what the stream said: for a harness whose result is its stream,
	// the whole of it; for one whose result is read apart, its transcript.
	Result() sandbox.Result

	// Ended reports whether the lines fed held the harness's own account of
	// how its run ended, its terminal result whether a success or a failure.
	// A run whose harness exited without one never collects as a success,
	// whatever it wrote, its findings included ([Runner.Collect]).
	Ended() bool

	// Requests is how many model requests the lines fed show, counted by the
	// engine off the stream in the unit its runner's cap is held to: what a
	// run's collection holds to the cap its launch handed it ([Runner.Collect],
	// [sandbox.Result.TurnCapExceeded]). Each decoder says at its count what
	// it counts and where that was measured.
	Requests() int

	// InFlight is the requests the runner's own enforcer cannot stop once it
	// has fired, as measured on the harness's release: a run past its cap by
	// no more than these, whose stream shows that enforcer's stop, collects
	// as stopped at its cap rather than past it ([capVerdict]). It and the stop
	// only choose which failure a run past its cap collects as, never a
	// success.
	InFlight() int

	// Entries hands over the entries decoded since it was last asked, in
	// order, and FORGETS them, or an hours-long live reading would hold the
	// whole transcript. A decoder read this way stops keeping what Result
	// needs, so it is read one way or the other, never both: a live reading
	// asks once before its first line, so the decoder never holds for a
	// Result, however much that line's read hands it.
	//
	// With end (every line was fed and the run is done), a run of skipped
	// lines still open is said too, as Result says it, so a reading ends on
	// the note the record ends on.
	Entries(end bool) []string
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
	l := streamLines{bound: bound, ends: true}
	return l.read(bufio.NewReaderSize(r, lineChunkBytes), true,
		func(line []byte) error {
			dec.Line(line)
			return nil
		},
		func(n int64, head, tail []byte) error {
			dec.Skipped(LongLine{Size: n, Bound: bound, Head: head, Tail: tail})
			return nil
		})
}

// LongLine is a line of the stream past its bound ([Decoder.Skipped]): its
// size, the bound, and its first and last bytes ([skipHeadBytes],
// [skipTailBytes]), the line's break left out. Head and Tail are reused once
// Skipped returns.
type LongLine struct {
	Size       int64
	Bound      int
	Head, Tail []byte
}

// skipHeadBytes and skipTailBytes are the ends of a skipped line handed to its
// decoder, which reads its request marker in one or the other. SIZED FROM
// MEASUREMENT against each runner's pinned release: a marker-first runner's
// lies within 196 bytes of the start (OpenCode's messageID; Claude Code's id
// at 31), and a marker-last runner's within 136 bytes of the end, whatever the
// payload before it (Pi's stopReason 135 on a 300 KB line, Qwen Code's usage
// 123, Cursor's model_call_id 136 on a 900 KB one). Each window holds many
// times that, so a field a later release adds beside the marker still fits.
const (
	skipHeadBytes = 4 << 10
	skipTailBytes = 16 << 10
)

// undecidedLines is a decoder's count of skipped lines that may hold a model
// request it could not read ([Decoder.Undecided]).
type undecidedLines struct{ undecided int }

// Undecided implements [Decoder].
func (u *undecidedLines) Undecided() int { return u.undecided }

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
// session's transcript, so there is one answer to how long a line may be: a
// [streamLines] used for one whole read.
func readLines(br *bufio.Reader, bound int, line func([]byte) error, skipped func(int64) error) error {
	l := streamLines{bound: bound}
	return l.read(br, true, line, func(n int64, _, _ []byte) error { return skipped(n) })
}

// streamLines is [readLines] with its place kept between reads that may end
// anywhere, so a stream read in pieces is read as one read whole.
type streamLines struct {
	bound int
	// held is the line so far while within bound, and over the size so far
	// of one already past it.
	held []byte
	over int64
	// head and tail are the first and last bytes of a line past bound
	// ([skipHeadBytes], [skipTailBytes]), all it keeps of one, and only for a
	// decoder ([eachLine], ends): a record's other readers hold none.
	head, tail []byte
	ends       bool
	// taken is how many bytes of the stream the reads consumed: handed on,
	// held or counted.
	taken int64
	// broken is whether the line last handed on, or counted as skipped, ended
	// in a break rather than at the stream's end.
	broken bool
}

// read hands line and skipped every line br completes, as [readLines] says.
// At br's end with end set (the stream is whole), an unfinished last line is
// handed on; without it, the line waits for the next read. br's own failure
// returns with the place kept, and hands on nothing it cut short.
func (l *streamLines) read(br *bufio.Reader, end bool, line func([]byte) error,
	skipped func(n int64, head, tail []byte) error) error {
	for {
		chunk, err := br.ReadSlice('\n')
		l.taken += int64(len(chunk))
		switch {
		case l.over > 0:
			l.over += int64(len(chunk))
			if l.ends {
				l.keepTail(chunk)
			}
		case len(l.held)+len(chunk) > l.bound:
			l.over = int64(len(l.held) + len(chunk))
			if l.ends {
				l.head = append(l.head[:0], l.held[:min(len(l.held), skipHeadBytes)]...)
				l.head = append(l.head, chunk[:min(len(chunk), skipHeadBytes-len(l.head))]...)
				l.tail = l.tail[:0]
				l.keepTail(l.held)
				l.keepTail(chunk)
			}
			// RELEASED, not truncated: the buffer has grown to the bound,
			// and keeping it would hold that much for the rest of the read.
			l.held = nil
		default:
			l.held = append(l.held, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		// A READ THAT FAILED MID-LINE HANDS NOTHING ON: a record cut in two
		// (by a budget, a store that stopped answering) would pass for whole.
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		eof := errors.Is(err, io.EOF)
		if eof && !end {
			return nil
		}
		var stop error
		if l.over > 0 {
			l.broken = !eof
			stop = skipped(l.over, l.head, bytes.TrimRight(l.tail, "\r\n"))
			l.over = 0
		} else if len(l.held) > 0 {
			l.broken = !eof
			stop = line(bytes.TrimRight(l.held, "\r\n"))
		}
		// A long line's buffer serves the next line only while a read with end
		// ([readLines]' one read) goes on; otherwise it goes with its line, so
		// a reader kept between reads holds only a line in transit.
		if cap(l.held) > lineChunkBytes && (!end || stop != nil || eof) {
			l.held = nil
		}
		l.held = l.held[:0]
		switch {
		case stop != nil:
			return stop
		case eof:
			return nil
		}
	}
}

// keepTail keeps the last [skipTailBytes] of the line past bound, b appended.
func (l *streamLines) keepTail(b []byte) {
	if len(b) >= skipTailBytes {
		l.tail = append(l.tail[:0], b[len(b)-skipTailBytes:]...)
		return
	}
	if cut := len(l.tail) + len(b) - skipTailBytes; cut > 0 {
		l.tail = append(l.tail[:0], l.tail[cut:]...)
	}
	l.tail = append(l.tail, b...)
}

// decodeAll runs a whole text through a decoder: a harness's Parse for a
// stream already in hand.
func decodeAll(dec Decoder, text []byte) sandbox.Result {
	// A bytes.Reader cannot fail, so neither can this.
	_ = eachLine(bytes.NewReader(text), dec, maxLineBytes)
	return dec.Result()
}

// transcriptHoldBytes is how much of a run's transcript a whole reading holds
// raw: what the run's record keeps of one ([sandbox.MaxRunTextBytes]). Past it
// the transcript is redacted as it arrives and only what the record keeps is
// held ([sandbox.TranscriptHalves]), the same text and counts as its whole
// redaction cut where the box is read, or a note withholding one the stream
// cannot redact in one pass ([transcriptLines.result]): held raw any further,
// it is held only to be redacted whole, another copy, and cut
// ([sandbox.BoxReadHeldBytes]).
const transcriptHoldBytes = sandbox.MaxRunTextBytes

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
	// chunk is the buffer each piece is written to the stream through ([transcriptLines.write]).
	chunk []byte

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

// add adds entry in its own words, trimmed. One with NO WORDS IS NONE: it
// leaves an open run of skipped lines open, as a line that adds no entry does.
func (t *transcriptLines) add(entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return
	}
	t.flushSkipped()
	t.push(entry)
}

func (t *transcriptLines) push(entry string) {
	switch {
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
	if t.fedBytes > 0 {
		t.write("\n")
	}
	t.write(entry)
}

// write writes s to the stream through one buffer of [lineChunkBytes] it
// reuses: the stream writes what Secrets does however its text is cut, and a
// copy of a long entry whole would hold the entry twice.
func (t *transcriptLines) write(s string) {
	t.fedBytes += len(s)
	t.fedBreaks += strings.Count(s, "\n")
	for len(s) > 0 {
		n := min(len(s), lineChunkBytes)
		t.chunk = append(t.chunk[:0], s[:n]...)
		// A refusal is answered again by Close, which result reads.
		_, _ = t.stream.Write(t.chunk)
		s = s[n:]
	}
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
// A RUN OF SKIPPED LINES STILL OPEN IS NOT FLUSHED until end ([Decoder.Entries]):
// the next entry says it, so a reading in pieces names the same lines as one
// taken whole, and at the stream's end nothing follows to say it.
func (t *transcriptLines) take(end bool) []string {
	t.live = true
	if end {
		t.flushSkipped()
	}
	lines := t.lines
	t.lines, t.held = nil, 0
	return lines
}

// streamEnd is the end of a stream a person or model can be shown: at most
// keep bytes of WHOLE lines ([sandbox.KeepEnd]) from tail's end, redacted
// first, and how many stream bytes came before it. tail is the stream's end
// read [redact.LookBack] past keep, or the whole stream.
//
// SHOWN ONLY WHERE THE CONTEXT SETTLES IT ([settledEnd]): bounded by redacted
// bytes alone, the end could reach back into the context when redaction shrank
// what followed, and be redacted without what decides it. A read opened inside
// a line running to the end has no line it opens to show, so it shows nothing,
// and every byte is counted.
func streamEnd(tail sandbox.FileTail, keep int) (string, int64) {
	ctx := 0
	if tail.Before() > 0 {
		ctx = redact.LookBack
	}
	kept, at := settledEnd(string(tail.Data), ctx, keep)
	return kept, tail.Before() + int64(at)
}

// settledEnd is the redacted end of text that its first ctx bytes settle
// ([redact.SettledEnd]), at most keep bytes of it as [sandbox.KeepEnd] keeps
// them, and how much came before it: in text's bytes up to the settled start,
// in redacted bytes past it, as [failureDetail] counts what it cuts.
func settledEnd(text string, ctx, keep int) (string, int) {
	end, at := redact.SettledEnd(text, ctx)
	if end == "" {
		return "", at
	}
	kept, k := sandbox.KeepEnd(end, keep)
	return kept, at + k
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

// boundedEvent is line as a result carries an event, or "" past
// [sandbox.MaxRunEventBytes], which the record would drop.
func boundedEvent(line []byte) string {
	if len(line) > sandbox.MaxRunEventBytes {
		return ""
	}
	return string(line)
}
