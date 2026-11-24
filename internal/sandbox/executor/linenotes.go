package executor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A COLLECTION'S READ OF A SESSION'S RECORD for the receipts of the notes its
// run served walks the record's lines too ([lineWalk.noteLine]), as [readLines]
// hands them, bounded and plain: a line past [maxLineBytes] is passed over,
// and of each line only the words its reader takes are held ([recordWord]), so
// the read holds no line within the base tier it runs in ([baseTierReadBytes]).

// setupNotes makes w a walk of the record lines reads for the receipts of its
// notes, a bounded and plain one with no reader of entries, its lines bound
// [maxLineBytes] but in a test of its own.
func (w *lineWalk) setupNotes(lines *bufio.Reader, bound int) {
	w.lines, w.piece = lines, make([]byte, lineChunkBytes)
	w.scan = jsonscan.New(&w.src)
	w.bound, w.plain = int64(bound), true
	w.src.bound, w.src.tee = w.bound, &w.trim
	w.wordBuf = make([]byte, 0, transcriptNameMaxBytes+1+utf8.UTFMax)
}

// noteLine is how a line a walk for notes read ended: none for no line at the
// record's end, ended where the record ended with it, past where it ran past
// the bound (size its bytes, its break among them, as [readLines] sizes it),
// object where it is one JSON object, read whole, null where it is JSON's
// null alone, which a whole-line decode reads as an object of no members, and
// blank where it is white space alone, as strings.TrimSpace reads it.
type noteLine struct {
	none, ended, past, object, null, blank bool
	size                                   int64
}

// noteLine reads the record's next line, read reading a JSON object's members
// from its opening brace, and every other value and byte passed over and
// validated.
func (w *lineWalk) noteLine(ctx context.Context, read func() error) (noteLine, error) {
	if err := ctx.Err(); err != nil {
		return noteLine{}, err
	}
	w.src.reset(ctx, w.lines, math.MaxInt64)
	w.trim = lineTrim{}
	w.scan.Reset(&w.src)
	k, err := w.scan.Next()
	if w.src.n == 0 && !w.src.broke && errors.Is(w.src.cause, io.EOF) {
		return noteLine{none: true, ended: true}, nil
	}
	object, null := err == nil && k == jsonscan.ObjectStart, err == nil && k == jsonscan.Null
	switch {
	case object:
		err = read()
	case err == nil:
		err = w.scan.Skip()
	}
	if err == nil {
		_, err = w.scan.Next()
	}
	_, scanned := errors.AsType[*jsonscan.Error](err)
	if err != nil && !scanned && !errors.Is(err, io.EOF) && w.src.cause == nil {
		return noteLine{}, err
	}
	if !w.src.broke && w.src.cause == nil {
		w.src.drain()
	}
	w.src.ctx = nil
	if cause := w.src.cause; cause != nil && !errors.Is(cause, io.EOF) {
		return noteLine{}, cause
	}
	whole := errors.Is(err, io.EOF)
	line := noteLine{ended: w.src.cause != nil, past: w.src.past, object: object && whole, null: null && whole,
		blank: w.trim.blank(), size: w.src.n}
	if w.src.broke {
		line.size++
	}
	return line, nil
}

// recordWord is a word a record names (an id, a kind): its text where within
// [transcriptNameMaxBytes], else the SHA-256 of the whole, so a longer one is
// still told apart from every other, as the whole-line decode told it, without
// being held. The zero word is the empty one.
type recordWord struct {
	text string
	long bool
	sum  [sha256.Size]byte
}

// is reports whether the word is s.
func (r recordWord) is(s string) bool { return !r.long && r.text == s }

// recordWordOf is s as a [recordWord] holds it.
func recordWordOf(s string) recordWord {
	if len(s) <= transcriptNameMaxBytes {
		return recordWord{text: s}
	}
	return recordWord{long: true, sum: sha256.Sum256([]byte(s))}
}

// empty reports whether the word is "".
func (r recordWord) empty() bool { return r.is("") }

// idField is the word the string value k begins ([recordWord]), the empty one
// for null, and whether a string field takes the value, passing over one it
// does not.
func (w *lineWalk) idField(k jsonscan.Kind) (recordWord, bool, error) {
	switch k {
	case jsonscan.Null:
		return recordWord{}, true, nil
	case jsonscan.String:
	default:
		return recordWord{}, false, w.scan.Skip()
	}
	buf := w.wordBuf[:0]
	for len(buf) <= transcriptNameMaxBytes {
		n, err := w.scan.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		switch {
		case errors.Is(err, io.EOF):
			return recordWord{text: string(buf)}, true, nil
		case err != nil:
			return recordWord{}, true, err
		}
	}
	if w.idSum == nil {
		w.idSum = sha256.New()
	}
	h := w.idSum
	h.Reset()
	_, _ = h.Write(buf)
	for {
		n, err := w.scan.Read(w.piece)
		_, _ = h.Write(w.piece[:n])
		switch {
		case errors.Is(err, io.EOF):
			word := recordWord{long: true}
			h.Sum(word.sum[:0])
			return word, true, nil
		case err != nil:
			return recordWord{}, true, err
		}
	}
}

// numberAsWritten is the number value k begins as written, where within
// [transcriptNameMaxBytes] (nil for a longer one, or a value of another
// kind), passing the value over; valid until the scanner's next call.
func (w *lineWalk) numberAsWritten(k jsonscan.Kind) ([]byte, error) {
	if k != jsonscan.Number {
		return nil, w.scan.Skip()
	}
	raw, whole, err := w.scan.Capture(transcriptNameMaxBytes)
	if !whole {
		raw = nil
	}
	return raw, err
}
