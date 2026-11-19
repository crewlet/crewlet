package jsonscan

import (
	"encoding/binary"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// replacement is U+FFFD as UTF-8: what an invalid byte, or a surrogate's escape that makes no pair, reads as.
const replacement = "\uFFFD"

// Read reads the current Name's or String's DECODED text, or a Number's literal as written, and answers
// io.EOF at its end (once its text is read, passed or captured). A read never ends inside a UTF-8 sequence:
// the text is valid UTF-8, an invalid byte read as U+FFFD and an escape as the character it stands for, and
// each read holds whole characters, so a p of at least utf8.UTFMax bytes always takes the next one, and a
// shorter p that cannot answers io.ErrShortBuffer and reads nothing: the token is as it was, and Capture
// can still take its value. At a token that has no text, Read answers [ErrNoText]; once the scan has ended,
// what ended it.
func (s *Scanner) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.text == textNone {
		if s.kind == Name || s.kind == String || s.kind == Number {
			return 0, io.EOF
		}
		return 0, ErrNoText
	}
	if len(p) == 0 {
		return 0, nil
	}
	var (
		n   int
		err error
	)
	if s.text == textNumber {
		n, err = s.number(p, false)
	} else {
		n, err = s.readString(p)
	}
	if n > 0 || s.text == textNone {
		// The read took part of the text, or passed its end: Capture can no longer answer the value from its
		// first byte.
		s.whole = false
	}
	if err == nil {
		if tee := s.takeTee(); tee != nil {
			// The read ended a teed value whose writer failed: answered with the bytes it read.
			return n, tee
		}
	}
	// What ended the read, if anything did, is the scan's answer from now on: the next call answers it,
	// after the bytes this one read.
	err = s.stop(err)
	switch {
	case n > 0:
		return n, nil
	case err != nil:
		return 0, err
	case s.text != textNone:
		// Nothing read and nothing ended: the next character outgrows p.
		return 0, io.ErrShortBuffer
	}
	return 0, io.EOF
}

// InvalidUTF8 reports whether the text Read has handed of the current Name or String held a byte that begins
// no UTF-8 character, which Read hands as U+FFFD: once Read has answered io.EOF, utf8.Valid of the text as
// written is false exactly where this is true. An escape, a lone surrogate's among them, is no such byte, nor
// a U+FFFD written as such. False at every other token, and for text passed over unread.
//
// THE DECODED TEXT CANNOT SAY SO: a byte that begins no character, a U+FFFD written as such and an escape
// that stands for one all read as the same U+FFFD, so a reader whose rule turns on the bytes as written (the
// thread layer's skill listing, whose long name that is no UTF-8 no reader can measure) asks here.
func (s *Scanner) InvalidUTF8() bool {
	return s.invalid
}

// passText passes over what of the current token's text is still unread, validating it, and answers the
// failure of a teed value's writer where that ends the value.
func (s *Scanner) passText() error {
	var err error
	switch s.text {
	case textName, textString:
		err = s.passString()
	case textNumber:
		_, err = s.number(nil, true)
	}
	if err != nil {
		return err
	}
	return s.takeTee()
}

// closeText ends the current token's text: after a Name its colon comes, after a String or a Number
// whatever follows a value, which ends a tee of it.
func (s *Scanner) closeText() {
	if s.text == textName {
		s.expect = expectColon
	} else {
		s.afterValue()
		if s.tw != nil && s.depth == s.teeDepth {
			s.endTee()
		}
	}
	s.text = textNone
}

// readString decodes the open Name's or String's text into p, whole characters only, and answers how many
// bytes it wrote: a run of plain bytes in one copy, an escape as its character, a byte past ASCII as the
// character it begins or as U+FFFD where it begins none. At the closing quote it closes the text; it stops
// short of a character p has no room left for, which the next read begins with.
func (s *Scanner) readString(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if n > 0 {
			// Part of the text is taken, so a fill from here need not keep the token's first byte.
			s.whole = false
		}
		if s.pos == s.end {
			if err := s.fill(1); err != nil {
				return n, err
			}
			if s.pos == s.end {
				return n, s.cut()
			}
		}
		b := s.buf[s.pos:s.end]
		if i := textRun(b); i > 0 {
			k := copy(p[n:], b[:i])
			n += k
			s.pos += k
			if s.counting {
				s.textUnits += int64(k)
			}
			continue
		}
		switch c := b[0]; {
		case c == '"':
			s.pos++
			s.closeText()
			return n, nil
		case c == '\\':
			r, size, err := s.escape(true)
			if err != nil {
				return n, err
			}
			if utf8.RuneLen(r) > len(p)-n {
				return n, nil
			}
			n += utf8.EncodeRune(p[n:], r)
			s.pos += size
			if s.counting {
				// A pair's twelve bytes are two units, every other escape one.
				s.textUnits += 1 + int64(size/12)
			}
		case c < 0x20:
			return n, s.refuse(whereString)
		default:
			// A byte past ASCII: the character it begins, read whole, so a multi-byte one is never split
			// between two reads, or U+FFFD where it begins none, one for each such byte, as both decoders
			// read it (utf8.DecodeRune's width of 1).
			if !utf8.FullRune(b) {
				if err := s.fill(utf8.UTFMax); err != nil {
					return n, err
				}
				b = s.buf[s.pos:s.end]
			}
			r, width := utf8.DecodeRune(b)
			if r == utf8.RuneError && width == 1 {
				if len(replacement) > len(p)-n {
					return n, nil
				}
				n += copy(p[n:], replacement)
				s.pos++
				s.invalid = true
				if s.counting {
					s.textUnits++
				}
				continue
			}
			if width > len(p)-n {
				return n, nil
			}
			n += copy(p[n:], b[:width])
			s.pos += width
			if s.counting {
				s.countChar(width)
			}
		}
	}
	return n, nil
}

// passString passes over the open Name's or String's text to its closing quote, validating it: a string
// takes every byte but a quote, a backslash and a control character as it stands (a byte past ASCII
// included, which both decoders read even where it begins no character), so a run of them is passed in one
// step, eight bytes at a time. Counting, each run is counted a character at a time ([Scanner.countRun]).
func (s *Scanner) passString() error {
	for {
		b := s.buf[s.pos:s.end]
		i := stringRun(b)
		if s.counting && i > 0 {
			if counted := s.countRun(b[:i], i == len(b) && !s.eof); counted < i {
				// A character the next bytes may complete: read on until they say, the reader's error the
				// answer only where they never do, as readString reads one.
				s.pos += counted
				if err := s.fill(utf8.UTFMax); err != nil && !utf8.FullRune(s.buf[s.pos:s.end]) {
					return err
				}
				continue
			}
		}
		s.pos += i
		if i == len(b) {
			if err := s.fill(1); err != nil {
				return err
			}
			if s.pos == s.end {
				return s.cut()
			}
			continue
		}
		switch b[i] {
		case '"':
			s.pos++
			s.closeText()
			return nil
		case '\\':
			_, size, err := s.escape(false)
			if err != nil {
				return err
			}
			s.pos += size
			if s.counting {
				// Every escape held to the grammar alone is one unit: a pair's halves two.
				s.textUnits++
			}
		default:
			return s.refuse(whereString)
		}
	}
}

// escape reads the escape at pos, a backslash and what follows it, and answers the character it stands for
// and how many bytes it takes, refusing an escape JSON has not.
//
// WITH pair SET, A SURROGATE'S \u ESCAPE TAKES THE \u ESCAPE AFTER IT TOO WHERE THE TWO MAKE ONE CHARACTER, a
// high half and a low one; any other surrogate reads as U+FFFD and takes its own six bytes, so the escape
// after it is read on its own. That is both decoders' reading (jsonwire's AppendUnquote, which jsontext and
// encoding/json share): `\ud800\ud800\udc00` reads as U+FFFD and then U+10000. Without pair (passing over a
// text), each escape is held to the grammar alone.
func (s *Scanner) escape(pair bool) (rune, int, error) {
	ferr := s.fill(2)
	b := s.buf[s.pos:s.end]
	if len(b) < 2 {
		if ferr != nil {
			return 0, 0, ferr
		}
		return 0, 0, s.cut()
	}
	switch c := b[1]; c {
	case '"', '\\', '/':
		return rune(c), 2, nil
	case 'b':
		return '\b', 2, nil
	case 'f':
		return '\f', 2, nil
	case 'n':
		return '\n', 2, nil
	case 'r':
		return '\r', 2, nil
	case 't':
		return '\t', 2, nil
	case 'u':
	default:
		s.pos++
		return 0, 0, s.refuse(whereEscape)
	}
	high, err := s.hex()
	if err != nil {
		return 0, 0, err
	}
	if !pair || !utf16.IsSurrogate(high) {
		return high, 6, nil
	}
	ferr = s.fill(12)
	low, ok, sure := lowHalf(s.buf[s.pos+6 : s.end])
	if !sure && ferr != nil {
		// The bytes after the escape could still begin the low half's: only the reader's failure stops
		// the scan from saying.
		return 0, 0, ferr
	}
	if ok {
		// DecodeRune answers U+FFFD unless high is a high half and low a low one.
		if r := utf16.DecodeRune(high, low); r != utf8.RuneError {
			return r, 12, nil
		}
	}
	return utf8.RuneError, 6, nil
}

// hex reads the four hex digits of the \u escape at pos, refusing the first byte that is none, or cutting the
// text where it ends among them.
func (s *Scanner) hex() (rune, error) {
	ferr := s.fill(6)
	b := s.buf[s.pos:s.end]
	var v rune
	for i := 2; i < 6; i++ {
		if i == len(b) {
			if ferr != nil {
				return 0, ferr
			}
			return 0, s.cut()
		}
		d := hexDigit(b[i])
		if d < 0 {
			s.pos += i
			return 0, s.refuse(whereEscape)
		}
		v = v<<4 | d
	}
	return v, nil
}

// lowHalf reads b, the bytes after a surrogate's escape, for a \u escape after it: the code unit that
// escape holds, whether b begins with one, and whether b says either way. It does not where b ends while it
// could still begin one.
func lowHalf(b []byte) (low rune, ok bool, sure bool) {
	for i := range 6 {
		if i == len(b) {
			return 0, false, false
		}
		switch {
		case i == 0 && b[i] != '\\', i == 1 && b[i] != 'u':
			return 0, false, true
		case i >= 2:
			d := hexDigit(b[i])
			if d < 0 {
				return 0, false, true
			}
			low = low<<4 | d
		}
	}
	return low, true, true
}

// hexDigit is the value of the hex digit c, or -1 where c is none.
func hexDigit(c byte) rune {
	switch {
	case '0' <= c && c <= '9':
		return rune(c - '0')
	case 'a' <= c && c <= 'f':
		return rune(c - 'a' + 10)
	case 'A' <= c && c <= 'F':
		return rune(c - 'A' + 10)
	}
	return -1
}

// numState is where a number's literal stands in the grammar: -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
type numState uint8

const (
	numStart     numState = iota // the first byte: '-' or a digit
	numMinus                     // after '-': a digit
	numZero                      // the integer is 0: '.', 'e' or 'E', or the end
	numInt                       // in the integer's digits: a digit, '.', 'e' or 'E', or the end
	numPoint                     // after '.': a digit
	numFrac                      // in the fraction's digits: a digit, 'e' or 'E', or the end
	numExp                       // after 'e' or 'E': '+', '-' or a digit
	numExpSign                   // after the exponent's sign: a digit
	numExpDigits                 // in the exponent's digits: a digit, or the end
)

// step is the state after c, and false where the grammar takes no c here.
func (st numState) step(c byte) (numState, bool) {
	digit := '0' <= c && c <= '9'
	switch st {
	case numStart, numMinus:
		switch {
		case c == '-' && st == numStart:
			return numMinus, true
		case c == '0':
			return numZero, true
		case digit:
			return numInt, true
		}
	case numZero, numInt, numFrac:
		switch {
		case digit && st != numZero:
			return st, true
		case c == '.' && st != numFrac:
			return numPoint, true
		case c == 'e' || c == 'E':
			return numExp, true
		}
	case numPoint:
		if digit {
			return numFrac, true
		}
	case numExp:
		switch {
		case c == '+' || c == '-':
			return numExpSign, true
		case digit:
			return numExpDigits, true
		}
	case numExpSign, numExpDigits:
		if digit {
			return numExpDigits, true
		}
	}
	return st, false
}

// mayEnd reports whether the literal is a whole number where it stands, so a byte the grammar takes no
// further ends it rather than being refused.
func (st numState) mayEnd() bool {
	return st == numZero || st == numInt || st == numFrac || st == numExpDigits
}

// inDigits reports whether a run of digits keeps the state where it is, so the run is passed in one step.
func (st numState) inDigits() bool {
	return st == numInt || st == numFrac || st == numExpDigits
}

// number reads the open Number's literal into p, or passes over it (pass), and answers how many bytes it
// wrote. A NUMBER ENDS AT THE FIRST BYTE ITS GRAMMAR TAKES NO FURTHER, where it is whole: that byte is the
// next token's, or the trailing data's (`01` is the number 0 followed by more), and is left unpassed; where
// it is not whole the byte is refused, and the text's end inside it cuts it.
func (s *Scanner) number(p []byte, pass bool) (int, error) {
	n := 0
	for pass || n < len(p) {
		if n > 0 {
			// Part of the literal is taken, so a fill from here need not keep its first byte.
			s.whole = false
		}
		if s.pos == s.end {
			if err := s.fill(1); err != nil {
				return n, err
			}
			if s.pos == s.end {
				if !s.num.mayEnd() {
					return n, s.cut()
				}
				s.closeText()
				return n, nil
			}
		}
		b := s.buf[s.pos:s.end]
		if s.num.inDigits() {
			if i := digitRun(b); i > 0 {
				if !pass {
					i = copy(p[n:], b[:i])
					n += i
				}
				s.pos += i
				continue
			}
		}
		next, ok := s.num.step(b[0])
		if !ok {
			if !s.num.mayEnd() {
				return n, s.refuse(whereNumber)
			}
			s.closeText()
			return n, nil
		}
		if !pass {
			p[n] = b[0]
			n++
		}
		s.pos++
		s.num = next
	}
	return n, nil
}

// digitRun is how many of b's leading bytes are digits.
func digitRun(b []byte) int {
	i := 0
	for i < len(b) && '0' <= b[i] && b[i] <= '9' {
		i++
	}
	return i
}

// ones and highs are a byte's lowest and highest bit in each of a word's eight.
const (
	ones  = 0x0101010101010101
	highs = 0x8080808080808080
)

// stringRun is how many of b's leading bytes a string holds as they stand: every byte but a quote, a
// backslash and a control character. It tests eight bytes at a time, so passing over a long string costs a
// few operations a word rather than a branch a byte.
func stringRun(b []byte) int {
	i := 0
	for ; len(b)-i >= 8; i += 8 {
		if stops(binary.LittleEndian.Uint64(b[i:])) != 0 {
			break
		}
	}
	for i < len(b) && !stopsString(b[i]) {
		i++
	}
	return i
}

// textRun is stringRun's run of ASCII alone, what decoding copies as it stands: a byte past ASCII stops it
// too, to be read as the character it begins, or as U+FFFD where it begins none.
func textRun(b []byte) int {
	i := 0
	for ; len(b)-i >= 8; i += 8 {
		w := binary.LittleEndian.Uint64(b[i:])
		if stops(w)|w&highs != 0 {
			break
		}
	}
	for i < len(b) && b[i] < utf8.RuneSelf && !stopsString(b[i]) {
		i++
	}
	return i
}

// stops is non-zero exactly when some byte of w is a quote, a backslash or a control character: a byte
// under 0x20, or a byte that is 0 once w's bytes are flipped by a quote's or a backslash's bits.
func stops(w uint64) uint64 {
	return under(w, 0x20) | under(w^(ones*'"'), 1) | under(w^(ones*'\\'), 1)
}

// under is non-zero exactly when some byte of w is under n, for n from 1 to 0x80: the word-at-a-time test,
// EXACT FOR ANY WORD, a byte past ASCII anywhere in it included. The lowest byte under n borrows in the
// subtraction and so holds its high bit, which &^ w keeps since that byte is under 0x80; and where no byte
// is under n nothing borrows, and a byte's difference holds its high bit only where the byte's own is set,
// which &^ w clears.
func under(w, n uint64) uint64 {
	return (w - ones*n) &^ w & highs
}

// stopsString reports whether a string stops holding bytes as they stand at c: a quote, a backslash or a
// control character.
func stopsString(c byte) bool {
	return c == '"' || c == '\\' || c < 0x20
}
