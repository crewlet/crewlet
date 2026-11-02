package executor

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// How a run's output is read back: as a STREAM, a line at a time.
//
// A harness's stdout is an event log that grows with every tool call, and
// its stderr is whatever it and everything it ran printed. Both used to be
// read WHOLE, and refused past [sandbox.MaxFileBytes] — so a long run was
// refused for the size of its own log: the poll returned before its liveness
// probe and kept a dead job's seat busy for good, and a
// collection lost a result its report file held. What the engine keeps of
// either is bounded anyway (a transcript, a failure read from the end), so
// nothing here needs the whole of one in memory, and nothing is refused for
// its size.

// Decoder reads one harness's event stream a line at a time: ONE STREAM per
// decoder ([Harness.Events]), read either WHOLE — every line, then [Decoder.Result]
// — or as a LIVE READING across many reads of the growing stream, its entries
// taken as they come ([Decoder.Entries], what [Runner.Follow] keeps one decoder
// for, for as long as the reading lasts).
//
// SO ITS STATE OUTLIVES A READ, and must depend only on the lines it has been
// fed — never on where one read ended and the next began. A tool call in one
// read is named by its result in the next (Claude Code's pending calls), and a
// run of skipped lines is said where it ends, whichever read that is. Two
// owners whose reads fell differently derive the same text from the same
// stream only because of this, and the live view's digest resets every owner
// move on which they do not.
type Decoder interface {
	// Line is one complete line of the stream, without its line break. The
	// slice is reused once Line returns. A line that is not an event the
	// decoder knows is skipped, never an error: a stream read mid-write ends
	// in a partial object, and a harness version this build has not met prints
	// event types it does not know.
	Line(line []byte)

	// Skipped is a line past bound — what its reader held a line to,
	// [maxLineBytes] on every read the engine makes — which was not read: n
	// is its size. The decoder says so where the line was, naming the
	// bound, so a reader of what it built is not shown a gap as continuity.
	Skipped(n int64, bound int)

	// Result is what the stream said: for a harness whose result is its stream,
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
//
// Every read the engine makes holds a line to it — a runner's through
// [Runner.lineBound], which its suite lowers to skip a line it can afford to
// build.
const maxLineBytes = sandbox.MaxFileBytes

// eachLine feeds r to dec a line at a time, in memory bounded by bound
// ([maxLineBytes] on every read the engine makes): a line longer than that is
// never held — its bytes are counted as they pass and the decoder told its
// size and the bound it was past.
//
// The last line need not end in a line break: a finished stream's last line
// is complete wherever it stops, and a running one's is the decoder's to
// recognise as partial.
func eachLine(r io.Reader, dec Decoder, bound int) error {
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
		case len(line)+len(chunk) > bound:
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
			dec.Skipped(over, bound)
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

// decodeAll runs a whole text through a decoder: what a harness's Parse is for a
// stream already in hand.
func decodeAll(dec Decoder, text string) sandbox.Result {
	// A strings.Reader cannot fail, so neither can this.
	_ = eachLine(strings.NewReader(text), dec, maxLineBytes)
	return dec.Result()
}

// transcriptLines builds a run's activity transcript one entry at a time, and
// is where a skipped line becomes a note.
type transcriptLines struct {
	lines []string

	// skipped and skippedBytes are a run of consecutive lines past the
	// bound, said in ONE note where they were rather than one per line,
	// and skippedBound is the bound they were past.
	skipped      int
	skippedBytes int64
	skippedBound int
}

func (t *transcriptLines) add(entry string) {
	t.flushSkipped()
	t.lines = append(t.lines, entry)
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
	t.lines = append(t.lines, fmt.Sprintf("(%d line(s) of output, %s, not read: past the %s one "+
		"line of a run's output may hold)", t.skipped, humanSize(t.skippedBytes), humanSize(int64(t.skippedBound))))
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

// redactContext is how much MORE than it shows a reading that begins inside a
// stream reads before what it shows, so a credential that began before what is
// shown is still recognised and redacted. What it reads there is context and
// nothing else: it is never shown ([settledEnd]).
//
// Every credential shape the redaction pass knows fits on one line except a
// private key, whose block runs at most [redact.MaxKeyBlockBytes] past its
// BEGIN line. A window opening inside such a block would show its base64 body
// with no BEGIN for the rule to anchor on, and read back from its END, the
// lines above three that part it are published (package redact's doc); with
// this much read before the window, any key that reaches into it is read
// whole and redacted as one — and the bound is the redaction's own, so the two
// cannot drift.
const redactContext = redact.MaxKeyBlockBytes

// streamEnd is the end of a stream as text a person or a model can be shown:
// at most keep bytes of WHOLE lines from the end of tail, redacted, and how
// many bytes of the stream came before what is shown. tail is the stream's
// end read [redactContext] past keep, or the whole stream.
//
// REDACTED BEFORE IT IS BOUNDED, for the reason [redactContext] exists, and
// on WHOLE LINES by the one rule every stream's end is shown by
// ([sandbox.KeepEnd]).
//
// SHOWN ONLY WHERE THE CONTEXT SETTLES IT. The first [redactContext] bytes of
// a read that began inside the stream are context, and what is shown is the
// end of what comes after them, as the whole read redacts it ([settledEnd]).
// Bounded by the redacted text's bytes alone, the end reached back into the
// context whenever the redaction shrank what came after it — a stream's last
// mebibytes holding keys enough, each block a marker of twenty-two bytes —
// and showed the lines of a key whose BEGIN came before the read.
//
// A READ THAT OPENED INSIDE A LINE RUNNING TO THE END has no line break to
// start a whole line at, and dropping the line it opened inside dropped
// everything the process said last — a failure built on it said nothing of
// the stream at all, not even that it was there. That is the one case whole
// lines cannot meet, and such a line's end is kept on a character, marked:
// past the context, as anything shown is.
func streamEnd(tail sandbox.FileTail, keep int) (string, int64) {
	ctx := 0
	if tail.Before() > 0 {
		ctx = redactContext
	}
	kept, at := settledEnd(string(tail.Data), ctx, keep)
	return kept, tail.Before() + int64(at)
}

// settledEnd is the end of text that its first ctx bytes settle, redacted: at
// most keep bytes of whole lines, kept as [sandbox.KeepEnd] keeps them. And
// how much of text came before what it keeps — counted in text's own bytes up
// to where what is settled begins, and in the redacted text's past that, which
// is what a reader would have been shown of them, as the failure that carries
// a stream's end counts what it cuts off it ([failureDetail]).
//
// WHAT IS SETTLED begins on the first line past the context ([settledStart]),
// and is shown as the WHOLE of text redacts it. Redacted alone, its start can
// read otherwise: a key whose BEGIN is in the context reads as one cut off
// from its BEGIN, and a password's value under a key in the context as no
// password's. So only what the two redactions agree on is kept, from the line
// after the last place they part — where whatever the context decided ends.
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
			// THE END OF ONE LINE, which began before what is settled: kept
			// on a character and marked, as a line longer than keep is.
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

// settledStart is where what the first ctx bytes of text settle begins, and
// whether it opens a line: the first line that starts ctx bytes in or later,
// or — where none does, the last line having begun before — the first
// character there, inside that line.
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
