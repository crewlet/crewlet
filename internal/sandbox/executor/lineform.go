package executor

import (
	"bufio"
	"bytes"
	"io"
)

// A LINE'S FORM ([lineForm]) is how a harness writes a line that its readers
// and its pack's trim must find alike: matched on the line's raw bytes, never
// on a decode, so a Go reader and a pack's awk read one definition. Its
// pieces are literal, and between each two lies a JSON string's body, its
// escapes read as escapes, so a form walks the line's top-level tokens in
// their written order and no value's text can match it.

// lineFormMaxBytes is the most of a line a form reads: a whole form's line is
// no longer, and an opening form is found within it. EIGHT KIBIBYTES holds the
// opening of a Qwen Code boundary whose working directory is as long as Linux
// lets a path be (PATH_MAX, 4,096 bytes) beside its fixed fields, and is some
// fifty times the longest Gemini CLI line a form reads whole (its "Got it"
// reply, some 165 bytes). A Go reader peeks this much ahead ([peekLine]).
const lineFormMaxBytes = 8 << 10

// lineForm is one line's shape: pieces, each after the first opening with
// the quote that ends the string body before it, anchored at the line's
// start and, where whole, at its end.
type lineForm struct {
	pieces []string
	whole  bool
}

// holds reports whether line, a line's raw bytes without its break, is of
// form f within its first [lineFormMaxBytes].
func (f lineForm) holds(line []byte) bool {
	if len(line) > lineFormMaxBytes {
		if f.whole {
			return false
		}
		line = line[:lineFormMaxBytes]
	}
	rest, ok := bytes.CutPrefix(line, []byte(f.pieces[0]))
	for _, p := range f.pieces[1:] {
		end := jsonBodyEnd(rest)
		if !ok || end < 0 {
			return false
		}
		rest, ok = bytes.CutPrefix(rest[end:], []byte(p))
	}
	return ok && (!f.whole || len(rest) == 0)
}

// jsonBodyEnd is where the JSON string body s opens with ends, at its closing
// quote, a backslash escaping the byte after it; -1 where s never closes it.
func jsonBodyEnd(s []byte) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// peekLine is the line lines reads next, without reading it: its bytes up
// to its break, or [lineFormMaxBytes] and one more where it is longer, which
// no whole form holds. A last line with no break is a line. A read that fails
// is the bytes before it, which the walk's own read meets.
func peekLine(lines *bufio.Reader) []byte {
	ahead, _ := lines.Peek(lineFormMaxBytes + 1)
	if i := bytes.IndexByte(ahead, '\n'); i >= 0 {
		return ahead[:i]
	}
	return ahead
}

// lineHead reads a line through r, keeping its first [lineFormMaxBytes] and
// one more for its forms ([lineForm.holds]): no more, whatever the line's
// length.
type lineHead struct {
	r    io.Reader
	head []byte
}

// reset makes h the reader of the next line, read through r.
func (h *lineHead) reset(r io.Reader) { h.r, h.head = r, h.head[:0] }

func (h *lineHead) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if room := lineFormMaxBytes + 1 - len(h.head); room > 0 {
		h.head = append(h.head, p[:min(n, room)]...)
	}
	return n, err
}
