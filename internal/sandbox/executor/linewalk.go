package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A PERSON'S TRANSCRIPT WRITTEN AS JSON LINES is walked a line at a time
// ([lineWalk]) through the thread layer's line source ([lineSource]), one
// scanner pass a line, holding no line and no value past the read's value
// bound, which its reader streams into the entry it makes ([valueText]). A
// record's kind can be written after the values it picks a reading of, so a
// reader shapes each reading as it reads it, provisional ([entryShape]) until
// the line ends and its kind picks one. [TranscriptValueMaxBytes] says why the
// bound, [TranscriptReadLines] the lines.

// A WALK TAKES ITS LINES AS ITS HARNESS'S RECORD IS READ. Claude Code's
// bounds no line: one is one JSON text with JSON's blanks around it, and one
// that is no record is sized as written. A bounded walk ([lineWalk.setupBounded])
// takes them as [readLines] hands them: a line past [maxLineBytes], its break
// among it, is said and neither read nor counted against the lines bound; one
// of white space alone (strings.TrimSpace) is no entry; one that is no record
// is sized without the carriage returns that end it ([lineTrim]); and one of a
// JSON value its reader's decode passes over ([lineWalk.passes]) is no entry.

// lineRecords is a reader of the records a [lineWalk] walks, one a line.
type lineRecords interface {
	// start starts the record the next line holds.
	start()
	// record reads the record's members, its opening brace current.
	record() error
	// commit commits what the record's kind picks, once its line has ended
	// unrefused.
	commit() error
	// own reads a string or a name the walk read for its own words
	// ([lineWalk.word]).
	own(t *valueText) error
	// linesPast says where the read passed its lines bound, and ends it
	// ([transcriptBuilder.linesPast]).
	linesPast() error
	// said is told the walk is about to say the line just read in its place
	// (no record, or past the line bound), and commits first what a reader
	// holding a record's entries over lines holds ([geminiReading]).
	said() error
}

// said commits nothing: a reader of one record a line holds none over lines.
func (*lineWalk) said() error { return nil }

// lineWalk is a person's read of a member's lines into b: the member's line
// source and the scanner reset to each line, how many lines and elements the
// read has framed, and the reader of each line's record.
type lineWalk struct {
	b      *transcriptBuilder
	rd     lineRecords
	lines  *bufio.Reader
	src    lineSource
	scan   *jsonscan.Scanner
	framed int
	piece  []byte
	// str is a name or a string read for its own words ([lineWalk.word]).
	str valueText
	// bound is the longest line a bounded walk reads, 0 for none, and trim
	// judges its line as [readLines] hands one.
	bound int64
	trim  lineTrim
	// plain is a reader that reads no string for its own words: a word is
	// read to its bound alone, and every other value passed over.
	plain bool
	// passes reports a line of one JSON value of kind k, no object, that the
	// reader's whole-line decode passes over unsaid: no entry, where a line of
	// a value it does not pass is said as no record (nil: none is passed).
	passes func(k jsonscan.Kind) bool
	// quiet passes over a line that is no record unsaid, as a harness that
	// skips a fragment it left does (Codex, Hermes's dump); tornUnread
	// leaves a last line with no break unread, neither said nor counted.
	quiet, tornUnread bool
	wordBuf           []byte
	// idSum digests a word past the bound a walk for notes tells apart
	// ([recordWord]).
	idSum hash.Hash
	// head holds a string's opening as it is told ([lineWalk.opening]).
	head []byte
	// inner reads, through innerSrc, the JSON text a string holds
	// ([lineWalk.decoded]).
	inner    *jsonscan.Scanner
	innerSrc stringWords
}

// setup makes w rd's walk into b of the members lines reads.
func (w *lineWalk) setup(b *transcriptBuilder, rd lineRecords, lines *bufio.Reader) {
	w.b, w.rd, w.lines, w.piece = b, rd, lines, make([]byte, textDigestBytes)
	w.scan = jsonscan.New(&w.src)
}

// setupBounded makes w rd's bounded walk into b of the members lines reads,
// rd a plain reader ([lineWalk.plain]).
func (w *lineWalk) setupBounded(b *transcriptBuilder, rd lineRecords, lines *bufio.Reader) {
	w.setup(b, rd, lines)
	w.bound, w.plain = maxLineBytes, true
	w.src.bound, w.src.tee = w.bound, &w.trim
	w.wordBuf = make([]byte, 0, transcriptNameMaxBytes+1+utf8.UTFMax)
}

// member reads the member's lines to its end: A PIECE THE MEMBER ENDS WITH NO
// BREAK IS A LINE, judged as any other, since the take keeps a transcript as it
// was first read, its last line at most torn; a piece cut by the read budget or
// by its reader's own error is no line, and ends the read with that error. Past
// its bounds' ReadLines lines and elements the read ends where it is, said.
func (w *lineWalk) member(ctx context.Context) error {
	for {
		ended, err := w.line(ctx)
		switch {
		case errors.Is(err, errReadLines):
			return w.rd.linesPast()
		case err != nil:
			return err
		case ended:
			return nil
		}
	}
}

// frame counts one line or element the read frames, ending the read past
// [TranscriptReadLines].
func (w *lineWalk) frame() error {
	if w.framed++; w.framed > w.b.bounds.ReadLines {
		return errReadLines
	}
	return nil
}

// line reads the member's next line and commits the reading its record's kind
// picks, and reports whether the member ended. A LINE IS ONE JSON TEXT WITH
// JSON'S BLANKS AROUND IT, as Claude Code's own load reads one: a line of blanks
// alone is no entry, and one that is no object (unless its reader passes it,
// read whole), or that the scanner refuses anywhere, commits nothing of its
// record and is said. A bounded walk counts a line only once it has ended
// within the bound.
func (w *lineWalk) line(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	w.src.reset(ctx, w.lines, math.MaxInt64)
	w.trim = lineTrim{}
	w.scan.Reset(&w.src)
	w.rd.start()
	k, err := w.scan.Next()
	if w.src.n == 0 && !w.src.broke && errors.Is(w.src.cause, io.EOF) {
		return true, nil
	}
	if w.bound == 0 {
		if frameErr := w.frame(); frameErr != nil {
			return false, frameErr
		}
	}
	object := err == nil && k == jsonscan.ObjectStart
	passed := err == nil && !object && w.passes != nil && w.passes(k)
	switch {
	case object:
		err = w.rd.record()
	case passed:
		err = w.scan.Skip()
	}
	if (object || passed) && err == nil {
		_, err = w.scan.Next()
	}
	fault, scanned := errors.AsType[*jsonscan.Error](err)
	if errors.Is(err, errReadLines) || err != nil && !scanned && !errors.Is(err, io.EOF) && w.src.cause == nil {
		// The lines bound, or a caller who left inside the line.
		return false, err
	}
	// A FAULT STOPS THE SCANNER BEFORE THE BREAK: the rest of the line is passed
	// over, holding nothing, so a refused line's size is the whole line's.
	if !w.src.broke && w.src.cause == nil {
		w.src.drain()
	}
	w.src.ctx = nil
	ended := w.src.cause != nil
	if cause := w.src.cause; cause != nil && !errors.Is(cause, io.EOF) {
		return false, cause
	}
	if w.tornUnread && ended && !w.src.broke {
		return ended, nil
	}
	if w.bound > 0 {
		if w.src.past {
			return ended, w.say(w.pastBound)
		}
		if frameErr := w.frame(); frameErr != nil {
			return false, frameErr
		}
		if w.trim.blank() {
			return ended, nil
		}
	}
	switch {
	case scanned && fault.Fault == jsonscan.FaultNoValue:
		return ended, nil
	case (!object && !passed || scanned || err != nil && !errors.Is(err, io.EOF)) && w.quiet:
		return ended, nil
	case !object && !passed, scanned, err != nil && !errors.Is(err, io.EOF):
		return ended, w.say(w.refused)
	case passed:
		return ended, nil
	}
	return ended, w.rd.commit()
}

// say says the line just read in its place with said, once its reader has
// committed what it holds ([lineRecords.said]).
func (w *lineWalk) say(said func() error) error {
	if err := w.rd.said(); err != nil {
		return err
	}
	return said()
}

// refused says the line just read is no record, sized as its walk takes it.
func (w *lineWalk) refused() error {
	n := w.src.n
	if w.bound > 0 {
		n -= w.trim.returns
	}
	return w.b.notARecord(int(n))
}

// pastBound says the line just read ran past the bound, sized with its break.
func (w *lineWalk) pastBound() error {
	n := w.src.n
	if w.src.broke {
		n++
	}
	return w.b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
		"and was not read", humanSize(n), humanSize(w.bound)))
}

// read reads the current string or name into to, a piece at a time.
func (w *lineWalk) read(to io.Writer) error {
	for {
		n, err := w.scan.Read(w.piece)
		_, _ = to.Write(w.piece[:n])
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// asked is the caller's error, asked between the values and the notes one line
// holds: a line runs to mebibytes, and the source asks only as it takes a piece.
func (w *lineWalk) asked() error { return w.src.ctx.Err() }

// word reads the current string for its own words, and is the word, "" where
// it runs past [transcriptNameMaxBytes]: every word a reader compares or
// remembers is a kind, a time or an id, none longer, and [transcriptBuilder.called]
// refuses a longer id, so a list of such ids holds none until its line ends.
func (w *lineWalk) word() (string, error) {
	if w.plain {
		return w.plainWord()
	}
	t := w.str.begin(w.b, TranscriptEntry{Kind: EntryNote}, false)
	defer t.drop()
	if err := w.read(t); err != nil {
		return "", err
	}
	if err := w.rd.own(t); err != nil || t.past || len(t.held) > transcriptNameMaxBytes {
		return "", err
	}
	return t.text(), nil
}

// plainWord is [lineWalk.word] for a plain reader: read to just past
// [transcriptNameMaxBytes], the rest passed over by the next token's read.
func (w *lineWalk) plainWord() (string, error) {
	buf := w.wordBuf[:0]
	for len(buf) <= transcriptNameMaxBytes {
		n, err := w.scan.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		switch {
		case errors.Is(err, io.EOF):
			return string(buf), nil
		case err != nil:
			return "", err
		}
	}
	return "", nil
}

// wordValue is the word the value k begins, "" where it is no string, its own
// words read either way.
func (w *lineWalk) wordValue(k jsonscan.Kind) (string, error) {
	if k != jsonscan.String {
		return "", w.value(k)
	}
	return w.word()
}

// value walks the value k begins for its own words: every string and every
// name in it ([lineRecords.own]), holding nothing else; a plain reader's
// passes it over.
func (w *lineWalk) value(k jsonscan.Kind) error {
	if w.plain {
		return w.scan.Skip()
	}
	switch k {
	case jsonscan.String:
		_, err := w.word()
		return err
	case jsonscan.ObjectStart:
		return w.members(func(_ string, k jsonscan.Kind) error { return w.value(k) })
	case jsonscan.ArrayStart:
		return w.elements(func(jsonscan.Kind) (bool, error) { return false, nil })
	}
	return nil
}

// members walks the object whose start is the current token, handing each
// member's name, read for its own words, and its value's kind to each, which
// reads the value.
func (w *lineWalk) members(each func(name string, k jsonscan.Kind) error) error {
	for {
		if err := w.asked(); err != nil {
			return err
		}
		k, err := w.scan.Next()
		switch {
		case err != nil:
			return err
		case k == jsonscan.ObjectEnd:
			return nil
		}
		name, err := w.word()
		if err != nil {
			return err
		}
		if k, err = w.scan.Next(); err != nil {
			return err
		}
		if err = each(name, k); err != nil {
			return err
		}
	}
}

// elements walks the list whose start is the current token, handing each
// element's kind to each, which answers whether it read the element; an
// element it did not read is walked for its own words.
func (w *lineWalk) elements(each func(k jsonscan.Kind) (bool, error)) error {
	for {
		if err := w.asked(); err != nil {
			return err
		}
		k, err := w.scan.Next()
		switch {
		case err != nil:
			return err
		case k == jsonscan.ArrayEnd:
			return nil
		}
		read, err := each(k)
		if err == nil && !read {
			err = w.value(k)
		}
		if err != nil {
			return err
		}
	}
}

// notARecord says a line of the record, n bytes long, that is no record.
func (b *transcriptBuilder) notARecord(n int) error {
	return b.unread(fmt.Sprintf("a line of %s of the harness's record is not a record, "+
		"and was not read", humanSize(int64(n))))
}

// lineTrim judges a line as [readLines] hands it, a piece at a time: whether
// it is white space alone, as strings.TrimSpace reads it (a character split
// across two pieces carried to the next), and how many carriage returns end
// it, which bytes.TrimRight takes off before a line that is no record is sized.
type lineTrim struct {
	text    bool
	returns int64
	carry   [utf8.UTFMax]byte
	carried int
}

func (j *lineTrim) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	end := len(p)
	for end > 0 && p[end-1] == '\r' {
		end--
	}
	if end == 0 {
		j.returns += int64(len(p))
	} else {
		j.returns = int64(len(p) - end)
	}
	for q := p; !j.text && len(q) > 0; {
		if j.carried > 0 {
			k := copy(j.carry[j.carried:], q)
			if !utf8.FullRune(j.carry[:j.carried+k]) {
				j.carried += k
				return len(p), nil
			}
			// A space is whole and longer than what was carried; anything
			// else ends the judging.
			r, size := utf8.DecodeRune(j.carry[:j.carried+k])
			if j.text = !unicode.IsSpace(r); j.text {
				break
			}
			q, j.carried = q[size-j.carried:], 0
			continue
		}
		if !utf8.FullRune(q) {
			j.carried = copy(j.carry[:], q)
			return len(p), nil
		}
		r, size := utf8.DecodeRune(q)
		j.text = !unicode.IsSpace(r)
		q = q[size:]
	}
	return len(p), nil
}

// blank reports whether the line held white space alone: a character left
// unfinished at its end is none.
func (j *lineTrim) blank() bool { return !j.text && j.carried == 0 }
