package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT A GEMINI CLI OR QWEN CODE CONTINUATION'S FIRST REQUEST CARRIES, read off
// the one chat file a capture's archive holds, or a job's box at its
// collection, a line at a time (geminicount.go, qwencount.go): an upper bound
// of the figure each harness compares with its point before the brief, judged
// in the box the launch gets (chatpoint.go).

// errChatArchive is an archive that holds no one chat file the meter reads.
var errChatArchive = errors.New("executor: the archive holds no one chat file")

// errChatLine is a chat file line past [codexRolloutLineMaxBytes], which no
// count reads past.
var errChatLine = errors.New("executor: a chat file line is past the bound a count reads")

// chatMemberLines walks the one regular member of the gzipped tar r that
// member names into c under large ([chatFileLines]); errChatArchive where the
// archive holds none of it or more than one.
func chatMemberLines(ctx context.Context, r io.Reader, member func(string) bool, c chatCounter,
	large *passBudget) error {

	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %w", errNotGzip, err)
	}
	tr := tar.NewReader(zr)
	read := false
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF) && read:
			return nil
		case errors.Is(err, io.EOF):
			return errChatArchive
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !member(h.Name) {
			continue
		}
		if read || h.Typeflag != tar.TypeReg {
			return errChatArchive
		}
		read = true
		if err := chatFileLines(ctx, tr, c, large, nil); err != nil {
			return err
		}
	}
}

// chatFileLines walks r, one chat file, into c a line at a time on the notes
// walk ([lineWalk.setupNotes]), holding no line: a blank line passed over, a
// torn last one passed over as the gate cuts it before a resume
// ([chatSessionGate]), and one past [codexRolloutLineMaxBytes] failing the
// read, as the whole-line read took each ([codexLines]). ONCE C KEEPS MORE
// THAN ITS BASE it holds large's slot to the walk's end, waiting within ctx
// ([errLargeWait] where the wait ends), as a collection's pass keeping a
// thread's records does ([walkThread]). Each line's bytes, its break among
// them, go to follow too where it is not nil ([chatFileFollowed]).
func chatFileLines(ctx context.Context, r io.Reader, c chatCounter, large *passBudget,
	follow io.Writer) (err error) {
	held := false
	defer func() {
		if held {
			large.drop()
		}
	}()
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes), codexWalkBound)
	w.src.tee = c.tee(&w.trim)
	if follow != nil {
		w.src.tee = teeTwo{w.src.tee, follow}
	}
	for n := 1; ; n++ {
		c.start()
		line, err := w.noteLine(ctx, func() error { return c.record(&w) })
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			return fmt.Errorf("executor: reading chat file line %d: %w", n, err)
		case line.none, line.torn:
			return nil
		case line.past:
			return fmt.Errorf("%w: line %d", errChatLine, n)
		case !line.blank:
			c.line(line.object)
			if !held && c.pastBase() {
				if err := large.hold(ctx); err != nil {
					return err
				}
				held = true
			}
		}
		if line.ended {
			return nil
		}
		if follow != nil {
			_, _ = follow.Write(lineBreak)
		}
	}
}

// chatCounter is a harness's count of its chat file, a line at a time on the
// walk ([chatFileLines]; [geminiCounting], [qwenCounting]): under marks, with
// taken bytes a take's scrub took out of the file put back where its rules
// read a size ([meterReading.unscrubbed]), 0 for a file as its box holds it.
type chatCounter interface {
	// tee is what the walk writes each line's bytes to, trim among them.
	tee(trim *lineTrim) io.Writer
	// start starts the record the next line holds.
	start()
	// record reads the line's record, its opening brace current, holding
	// what it reads until the line ends.
	record(w *lineWalk) error
	// line takes the line just read, no blank one: object says it is one
	// JSON object, read whole.
	line(object bool)
	// pastBase reports whether the count keeps more than a read at its base
	// may hold ([meterBaseHeldBytes], [baseTierReadBytes]).
	pastBase() bool
	count(marks countMarks, taken int64) *sandbox.ResumedCount
}

// chatMeter is the take's meter of a chat archive r: c's count of the one
// chat file member names, by the rules of measured alone ([countChat]), under
// the take's large, and its count again with what the take's scrub took out
// ([meterReading.recount]).
func chatMeter(ctx context.Context, c chatCounter, r io.Reader, member func(string) bool,
	release, measured string, large *passBudget) (meterReading, error) {

	marks, err := countChat(release, measured, func() error { return chatMemberLines(ctx, r, member, c, large) })
	return meterReading{count: c.count(marks, 0), recount: &chatRecount{c: c, marks: marks}}, err
}

// chatRecount is a chat file's count, to be taken again with what a take's
// scrub took out ([meterReading.unscrubbed]): its counter, and its marks.
type chatRecount struct {
	c     chatCounter
	marks countMarks
}

// countChat walks a chat file into its counter with walk and is the marks of
// its count, by the rules of measured, the one release it was read in, alone:
// [sandbox.CountRelease] for a chat file release wrote, [sandbox.CountUnread]
// with walk's error where it did not read every line (ctx's end among them).
func countChat(release, measured string, walk func() error) (countMarks, error) {
	var marks countMarks
	if release != measured {
		marks = marks.with(sandbox.CountRelease)
	}
	if err := walk(); err != nil {
		return marks.with(sandbox.CountUnread), err
	}
	return marks, nil
}

// chatCopyCount is what a job's harness counts the copy of its session its box
// holds at, read at the job's collection ([sandbox.MainThreadAsk.Count]) by
// the rules the take's meter counts a capture by: the chat file file names
// ("" for none, [sandbox.CountUnread]) read whole within the read's wait. A box
// whose job continues it in place resumes that file, unscrubbed, so a relaunch
// continuing the copy, or a capture of it taken whole, is judged by this count
// ([sandbox.LaunchRecord.ResumedCount]); a release but measured counts nothing.
// summary, where it is not nil, follows the same walk ([chatFileFollowed]),
// and err is why it did not read the file to its end; c nil counts nothing.
func chatCopyCount(ctx context.Context, box sandbox.Sandbox, file string, c chatCounter,
	release, measured string, summary *chatSummaryLines) (count *sandbox.ResumedCount, err error) {

	marked := func(mark sandbox.CountMark) *sandbox.ResumedCount {
		if c == nil {
			return nil
		}
		return &sandbox.ResumedCount{Mark: mark}
	}
	if release != measured {
		return marked(sandbox.CountRelease), fmt.Errorf("the job ran release %q, and this build reads %s alone",
			release, measured)
	}
	if file == "" {
		return marked(sandbox.CountUnread), errChatFile
	}
	r, err := box.OpenFile(ctx, file)
	if err != nil {
		return marked(sandbox.CountUnread), err
	}
	defer func() { _ = r.Close() }()
	marks, err := countChat(release, measured, func() error { return chatFileFollowed(ctx, r, c, summary) })
	if c == nil {
		return nil, err
	}
	return c.count(marks, 0), err
}

// errChatFile is a session no one chat file in its box names.
var errChatFile = errors.New("no one chat file in the box names the session")

// chatFileFollowed walks r, one chat file, into c ([chatFileLines]; c nil
// counts nothing) as summary follows the same walk where it is not nil
// ([chatSummaryLines.follow]): the walk's bytes handed on through a pipe as
// it takes them, each line's break among them, so the summary is read off
// the one walk and neither holds a line. The pipe ends with the walk's error.
func chatFileFollowed(ctx context.Context, r io.Reader, c chatCounter, summary *chatSummaryLines) error {
	if c == nil {
		c = chatSkip{}
	}
	if summary == nil {
		return chatFileLines(ctx, r, c, new(passBudget), nil)
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		summary.follow(ctx, pr)
		// A reading that ended early lets the walk's writes go by unread.
		_ = pr.Close()
	}()
	err := chatFileLines(ctx, r, c, new(passBudget), pw)
	_ = pw.CloseWithError(err)
	<-done
	return err
}

// lineBreak is the break a followed walk hands on after each line it took.
var lineBreak = []byte{'\n'}

// chatSkip is a count of nothing, a walk's lines passed over
// ([chatFileFollowed]).
type chatSkip struct{}

func (chatSkip) tee(trim *lineTrim) io.Writer                  { return trim }
func (chatSkip) start()                                        {}
func (chatSkip) record(w *lineWalk) error                      { return w.scan.Skip() }
func (chatSkip) line(bool)                                     {}
func (chatSkip) pastBase() bool                                { return false }
func (chatSkip) count(countMarks, int64) *sandbox.ResumedCount { return nil }

// lineSeparators judges a chat file line, a piece at a time as the walk
// takes it, for U+2028 or U+2029, raw or escaped: a JavaScript reader may
// split the line there, so the history it restores is not the one the line
// reads as. The take's meter reads the scrub's output, which writes both
// escaped (scrubjson.go), so the escaped form is the one a take meets; a text
// holding the six characters themselves reads as one too, which only tells
// more. A separator across two pieces is read off its last bytes, carried.
type lineSeparators struct {
	found       bool
	carry, edge []byte
}

// lineSeparatorMaxBytes is the longest form a separator takes, `\u2028`.
const lineSeparatorMaxBytes = 6

func (s *lineSeparators) Write(p []byte) (int, error) {
	if s.found {
		return len(p), nil
	}
	keep := lineSeparatorMaxBytes - 1
	s.edge = append(append(s.edge[:0], s.carry...), p[:min(len(p), keep)]...)
	s.found = separated(s.edge) || separated(p)
	if len(p) >= keep {
		s.carry = append(s.carry[:0], p[len(p)-keep:]...)
	} else {
		s.carry = append(s.carry[:0], s.edge[max(0, len(s.edge)-keep):]...)
	}
	return len(p), nil
}

// reset starts a line.
func (s *lineSeparators) reset() { s.found, s.carry = false, s.carry[:0] }

// separated reports whether b holds a separator whole: raw, or a backslash, a
// u in either case and 2028 or 2029, as bytes.ToLower leaves no other byte
// that could make one.
func separated(b []byte) bool {
	if bytes.Contains(b, []byte("\u2028")) || bytes.Contains(b, []byte("\u2029")) {
		return true
	}
	for i := bytes.IndexByte(b, '\\'); i >= 0 && i+lineSeparatorMaxBytes <= len(b); {
		if e := b[i : i+lineSeparatorMaxBytes]; (e[1] == 'u' || e[1] == 'U') && string(e[2:5]) == "202" &&
			(e[5] == '8' || e[5] == '9') {
			return true
		}
		next := bytes.IndexByte(b[i+1:], '\\')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}

// teeTwo writes each piece to both writers, neither of which fails.
type teeTwo [2]io.Writer

func (t teeTwo) Write(p []byte) (int, error) {
	_, _ = t[0].Write(p)
	_, _ = t[1].Write(p)
	return len(p), nil
}

// twice marks bit in seen, a reader's set of the members it read of one
// object, and reports whether it was set: A KEY WRITTEN TWICE leaves a
// count unread, since its whole-line decode merged an object written twice
// where the walk keeps the last, and no harness writes one.
func twice(seen *uint16, bit uint16) bool {
	again := *seen&bit != 0
	*seen |= bit
	return again
}

// byteTally counts the bytes a tee writes it.
type byteTally int64

func (t *byteTally) Write(p []byte) (int, error) {
	*t += byteTally(len(p))
	return len(p), nil
}

// sizedValue walks the value k begins with read, nil to pass it over, and is
// its size as written, as the whole-line decode held it raw: a container's
// from its opening token to its closing one; any other value's teed, which
// none may be while the value is read.
func (w *lineWalk) sizedValue(k jsonscan.Kind, read func() error) (int64, error) {
	if read == nil {
		read = w.scan.Skip
	}
	if k == jsonscan.ObjectStart || k == jsonscan.ArrayStart {
		start := w.scan.Offset()
		if err := read(); err != nil {
			return 0, err
		}
		return w.scan.Offset() + 1 - start, nil
	}
	var n byteTally
	if err := w.scan.Tee(&n); err != nil {
		return 0, err
	}
	err := w.scan.Skip()
	return int64(n), err
}

// chatText is what a count reads of the current string as it streams,
// holding none past its first [transcriptNameMaxBytes] bytes: its UTF-8
// bytes, its length as JSON.stringify writes it, quotes included
// ([lineWalk.jsValue]), Gemini CLI's units and quarters of it and whether it
// holds [redact.Marker] ([chatText.estimate]), whether past its leading white
// space it opens with a compression's summary ([geminiStateSnapshot]), and
// the word it is ([recordWord]), digested where longer only if asked.
type chatText struct {
	bytes, js, units, quarters int64
	marked, snapshot           bool
	word                       recordWord
}

// estimate is Gemini CLI 0.70.0's estimate of the text, in quarter tokens: a
// quarter for each UTF-16 unit of an ASCII character, a token and a half for
// each other unit (an unreadable byte one unit); a text past 100,000 units a
// quarter a unit, unless it holds [redact.Marker].
//
// A TEXT HOLDING A MARKER IS CHARGED BY THE SHORT RULE WHATEVER ITS LENGTH:
// the rule is no sum of its characters, and a marker the scrub wrote in place
// of a shorter value can carry a kept text past the line the box's own copy
// stays under, at a sixth of its charge. Every text the scrub changes holds a
// marker and every other is kept as it was, so with what the scrub took out
// ([scrubTally]) the count never falls below the copy's.
func (t chatText) estimate() int64 {
	if t.units > 100_000 && !t.marked {
		return t.units
	}
	return t.quarters
}

// name is the text where within [transcriptNameMaxBytes], else "".
func (t chatText) name() string {
	if t.word.long {
		return ""
	}
	return t.word.text
}

// chatText reads the current string ([chatText]), its word digested past the
// bound where word.
func (w *lineWalk) chatText(word bool) (chatText, error) {
	var t chatText
	t.js = 2
	head := w.wordBuf[:0]
	var tail [len(redact.Marker) - 1]byte
	var edge [2 * len(tail)]byte
	tailN := 0
	// lead is how far past its leading white space the summary's opening is
	// matched: -1 while still in the white space, past its length on a
	// mismatch.
	lead := -1
	var h hash.Hash
	for {
		n, err := w.scan.Read(w.piece)
		p := w.piece[:n]
		// EVERY BYTE BEFORE P IS IN HEAD until it first passes the bound,
		// where a word asked for starts its digest.
		before := len(head)
		if len(head) < cap(head) {
			head = append(head, p[:min(len(p), cap(head)-len(head))]...)
		}
		if word && h == nil && len(head) > transcriptNameMaxBytes {
			if w.idSum == nil {
				w.idSum = sha256.New()
			}
			h = w.idSum
			h.Reset()
			_, _ = h.Write(head[:before])
		}
		if h != nil {
			_, _ = h.Write(p)
		}
		if !t.marked && n > 0 {
			e := append(append(edge[:0], tail[:tailN]...), p[:min(len(p), len(tail))]...)
			t.marked = bytes.Contains(e, []byte(redact.Marker)) || bytes.Contains(p, []byte(redact.Marker))
			if len(p) >= len(tail) {
				tailN = copy(tail[:], p[len(p)-len(tail):])
			} else {
				tailN = copy(tail[:], e[max(0, len(e)-len(tail)):])
			}
		}
		lead = t.take(p, lead)
		switch {
		case errors.Is(err, io.EOF):
			t.word = recordWord{text: string(head)}
			if len(head) > transcriptNameMaxBytes {
				t.word = recordWord{long: true}
				if h != nil {
					h.Sum(t.word.sum[:0])
				}
			}
			t.snapshot = lead == len(geminiStateSnapshot)
			return t, nil
		case err != nil:
			return chatText{}, err
		}
	}
}

// take counts p, whole characters of the text, and matches the summary's
// opening past lead ([lineWalk.chatText]).
func (t *chatText) take(p []byte, lead int) int {
	for len(p) > 0 {
		if lead >= len(geminiStateSnapshot) {
			// A RUN OF PRINTABLE ASCII but a quote and a backslash costs a
			// byte, a unit, a quarter and a JSON byte each.
			n := 0
			for n < len(p) && p[n] >= 0x20 && p[n] < 0x7f && p[n] != '"' && p[n] != '\\' {
				n++
			}
			t.bytes, t.units, t.quarters, t.js = t.bytes+int64(n), t.units+int64(n), t.quarters+int64(n), t.js+int64(n)
			if p = p[n:]; len(p) == 0 {
				break
			}
		}
		r, size := rune(p[0]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRune(p)
		}
		if lead < 0 && !unicode.IsSpace(r) {
			lead = 0
		}
		for i := range size {
			if lead >= 0 && lead < len(geminiStateSnapshot) {
				if p[i] == geminiStateSnapshot[lead] {
					lead++
				} else {
					lead = len(geminiStateSnapshot) + 1
				}
			}
		}
		t.bytes += int64(size)
		switch {
		case r <= 0x7f:
			t.units, t.quarters = t.units+1, t.quarters+1
		case r >= 0x10000:
			t.units, t.quarters = t.units+2, t.quarters+12
		default:
			t.units, t.quarters = t.units+1, t.quarters+6
		}
		switch {
		case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
			t.js += 2
		case r < 0x20 || r == utf8.RuneError:
			t.js += 6
		default:
			t.js += int64(size)
		}
		p = p[size:]
	}
	return lead
}

// jsValue walks the value k begins and is the length of JSON.stringify of it,
// as UTF-8 bytes, AT LEAST what JavaScript writes: a string's characters
// escaped as it escapes them (a quote, a backslash or one of five controls in
// two, another control in six, and an unreadable byte as a lone surrogate's
// six), a number at its own length where it has no exponent and at 25 where it
// has one (no double's shortest form is longer), and a name given twice
// counted twice, though JavaScript keeps one.
func (w *lineWalk) jsValue(k jsonscan.Kind) (int64, error) {
	switch k {
	case jsonscan.ObjectStart:
		return w.jsMembers(func(_ string, k jsonscan.Kind) (int64, error) { return w.jsValue(k) })
	case jsonscan.ArrayStart:
		return w.jsElements(w.jsValue)
	case jsonscan.String:
		t, err := w.chatText(false)
		return t.js, err
	case jsonscan.Number:
		return w.jsNumber()
	case jsonscan.False:
		return 5, nil
	}
	return 4, nil
}

// jsNumber reads the current number and is its length as JSON.stringify
// writes it: as written where it has no exponent, else 25, which no double's
// shortest form passes.
func (w *lineWalk) jsNumber() (int64, error) {
	var n int64
	exp := false
	for {
		m, err := w.scan.Read(w.piece)
		n += int64(m)
		exp = exp || bytes.ContainsAny(w.piece[:m], "eE")
		switch {
		case errors.Is(err, io.EOF) && exp:
			return 25, nil
		case errors.Is(err, io.EOF):
			return n, nil
		case err != nil:
			return 0, err
		}
	}
}

// jsMembers walks the object whose start is the current token, handing each
// member's name ([chatText.name]) and its value's kind to each, which reads
// the value and is its JSON.stringify length; and is the object's.
func (w *lineWalk) jsMembers(each func(name string, k jsonscan.Kind) (int64, error)) (int64, error) {
	n := int64(2)
	for first := true; ; first = false {
		if err := w.asked(); err != nil {
			return 0, err
		}
		k, err := w.scan.Next()
		switch {
		case err != nil:
			return 0, err
		case k == jsonscan.ObjectEnd:
			return n, nil
		}
		name, err := w.chatText(false)
		if err != nil {
			return 0, err
		}
		if k, err = w.scan.Next(); err != nil {
			return 0, err
		}
		v, err := each(name.name(), k)
		if err != nil {
			return 0, err
		}
		if !first {
			n++
		}
		n += name.js + 1 + v
	}
}

// jsElements walks the list whose start is the current token, handing each
// element's kind to each, which reads it and is its JSON.stringify length;
// and is the list's.
func (w *lineWalk) jsElements(each func(k jsonscan.Kind) (int64, error)) (int64, error) {
	n := int64(2)
	for first := true; ; first = false {
		if err := w.asked(); err != nil {
			return 0, err
		}
		k, err := w.scan.Next()
		switch {
		case err != nil:
			return 0, err
		case k == jsonscan.ArrayEnd:
			return n, nil
		}
		v, err := each(k)
		if err != nil {
			return 0, err
		}
		if !first {
			n++
		}
		n += v
	}
}
