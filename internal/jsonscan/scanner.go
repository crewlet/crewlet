package jsonscan

import (
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxDepth is how many containers may be open at once: a value nested MaxDepth deep is read, and the bracket
// or brace that would open one more is refused ([FaultDepth]). It is encoding/json/jsontext's own bound, and
// so encoding/json's, which this toolchain builds on jsontext: a value the scanner reads is one both decoders
// read, and the take's scrub, which reads on this scanner, rewrites no value nested deeper.
const MaxDepth = 10000

// ReadBufferSize is the read buffer [New] gives a Scanner, 64 KiB: what a pipe holds at once on Linux, so one
// read of a pipe or a file fills it, and a long string is copied out in runs of up to that length. Smaller,
// a 256 MiB session costs a read call per few KiB; larger buys nothing one read can hand over.
const ReadBufferSize = 64 << 10

// minBufferSize is the least buffer a Scanner reads with: the most it must see at once, a surrogate's \u
// escape and the escape after it, which it reads together to say whether the two make one character, and
// before them a string's opening quote, which the buffer keeps while Capture may still take the string
// ([Scanner.slide]).
const minBufferSize = 13

// maxEmptyReads is how many reads in a row may answer no bytes and no error before the reader is taken for
// stuck (io.ErrNoProgress): io.Reader discourages such a read, and bufio gives up after the same hundred.
const maxEmptyReads = 100

// Scanner reads one JSON text a token at a time ([New]). It is not safe for concurrent use.
type Scanner struct {
	r io.Reader
	// buf holds what is read of the text and not yet passed: buf[pos:end] is unscanned, and buf[0] sits at
	// base in the text. A fill moves the unscanned bytes to the front ([Scanner.slide]) and never drops one.
	buf      []byte
	pos, end int
	base     int64
	// eof says r has answered io.EOF; rerr is an error r answered, met once the bytes it came with are
	// scanned and more are needed.
	eof  bool
	rerr error
	// err ends the scan: an *Error, a reader's error, or io.EOF once the text is read. Every call answers it
	// from then on.
	err error

	// kind is the current token, the last one passed ("" before the first), and at where it begins.
	kind Kind
	at   int64
	// text is what of the current token's text is still unread, and num the number's grammar while a
	// Number's is.
	text textState
	num  numState
	// whole says nothing of the current token is taken past what Next read of it, so Capture can still
	// answer it from its first byte, which a fill keeps in buf while it holds ([Scanner.slide]).
	whole bool
	// invalid says the text Read has handed of the current Name or String held a byte that begins no UTF-8
	// character ([Scanner.InvalidUTF8]).
	invalid bool
	// counting says the unit count is on ([Scanner.CountUnits]).
	counting bool

	// expect is what the grammar takes next; depth is how many containers are open, and objects holds one
	// bit for each, set for an object, from the outermost.
	expect  expect
	depth   int
	objects [(MaxDepth + 7) / 8]byte

	// Capture's recording, while it passes over a value: held is what it has recorded from earlier fills of
	// buf (spilled says one happened), buf[recFrom:pos] the rest, and over says the value ran past limit,
	// so nothing more is held.
	recording bool
	spilled   bool
	over      bool
	recFrom   int
	limit     int
	held      []byte

	// The unit count ([Scanner.Units]): units what it counted until it last went off, onAt the offset it
	// last went on at, wide the bytes taken since then past the units they count, and textUnits the current
	// text's decoded units ([Scanner.TextUnits]).
	units, onAt, wide, textUnits int64

	// The tee ([Scanner.Tee]): tw writes the value while it is on, buf[teeFrom:pos] is what it has not yet
	// written, the value began at teeAt and is whole once depth is back at teeDepth, and teeErr is its writer's
	// first error, kept past the value's end until the call that ended it answers it.
	tw       io.Writer
	teeErr   error
	teeAt    int64
	teeFrom  int
	teeDepth int
}

// textState is what of the current token's text is still unread.
type textState uint8

const (
	textNone   textState = iota // no text, or none left: the token is passed
	textName                    // inside a Name: its closing quote not yet read
	textString                  // inside a String: its closing quote not yet read
	textNumber                  // inside a Number: the byte that ends it not yet seen
)

// expect is what the grammar takes next.
type expect uint8

const (
	expectTop          expect = iota // the text's value
	expectFirstElement               // after '[': a value or ']'
	expectElement                    // after an array's ',': a value
	expectElementEnd                 // after an array's element: ',' or ']'
	expectFirstName                  // after '{': a name or '}'
	expectName                       // after an object's ',': a name
	expectColon                      // after a name: ':'
	expectMemberValue                // after ':': a value
	expectMemberEnd                  // after a member's value: ',' or '}'
	expectEnd                        // after the text's value: blanks, then the text's end
)

// What the grammar takes where a byte is refused, for [Error]'s message.
const (
	whereTop          = "where the text's value begins"
	whereFirstElement = "where an array's first element or its ']' begins"
	whereElement      = "where an array element begins"
	whereElementEnd   = "after an array element (expecting ',' or ']')"
	whereFirstName    = "where an object's first member name or its '}' begins"
	whereName         = "where an object member's name begins"
	whereColon        = "after an object member's name (expecting ':')"
	whereMemberValue  = "where an object member's value begins"
	whereMemberEnd    = "after an object member (expecting ',' or '}')"
	whereString       = "in a string, which holds a control character only escaped"
	whereEscape       = "in an escape"
	whereNumber       = "in a number"
)

// New reads one JSON text from r: exactly one value, with blanks (space, tab, line feed, carriage return)
// around it. It reads r as the scan needs it, a 64 KiB buffer at a time.
func New(r io.Reader) *Scanner {
	return newScanner(r, ReadBufferSize)
}

// newScanner is New with a buffer of size bytes, at least [minBufferSize]: what a test scans with to move
// the buffer's edge through every byte of a short text.
func newScanner(r io.Reader, size int) *Scanner {
	return &Scanner{r: r, buf: make([]byte, max(size, minBufferSize))}
}

// Reset makes s read a new text from r, as New(r) would, keeping its buffers: a reader of many short texts,
// a thread's lines, allocates them once.
func (s *Scanner) Reset(r io.Reader) {
	*s = Scanner{r: r, buf: s.buf, held: s.held[:0]}
}

// Next advances to the next token and answers its kind: ObjectStart, ObjectEnd, ArrayStart, ArrayEnd, Name
// (an object member's name), String, Number, True, False or Null; or io.EOF once the value and the blanks
// after it are read, which is when the text is known to be one JSON value. A Name's, a String's or a
// Number's text is read with Read before the next Next; what of it is not read, Next passes over, validating
// it. A text the grammar refuses answers an [*Error]; an error of the reader beneath passes through,
// wrapped, so errors.Is and errors.As reach it. Either ends the scan: every call answers it from then on. A
// [*TeeError] ends nothing ([Scanner.Tee]).
func (s *Scanner) Next() (Kind, error) {
	if s.err != nil {
		return "", s.err
	}
	k, err := s.next()
	return k, s.stop(err)
}

func (s *Scanner) next() (Kind, error) {
	// The current token is passed from here, so a fill need keep none of it, and what its text held is no
	// longer what the current token's holds.
	s.whole, s.invalid = false, false
	// A text passed that ends a teed value whose writer failed answers that, the scan standing past it.
	if err := s.passText(); err != nil {
		return "", err
	}
	s.textUnits = 0
	for {
		c, ok, err := s.peekNonBlank()
		if err != nil {
			return "", err
		}
		if !ok {
			return "", s.ended()
		}
		switch s.expect {
		case expectTop:
			return s.value(c, whereTop)
		case expectFirstElement:
			if c == ']' {
				return s.close(ArrayEnd)
			}
			return s.value(c, whereFirstElement)
		case expectElement:
			return s.value(c, whereElement)
		case expectMemberValue:
			return s.value(c, whereMemberValue)
		case expectFirstName, expectName:
			switch {
			case c == '"':
				return s.name(), nil
			case c == '}' && s.expect == expectFirstName:
				return s.close(ObjectEnd)
			case s.expect == expectFirstName:
				return "", s.refuse(whereFirstName)
			}
			return "", s.refuse(whereName)
		case expectColon:
			if c != ':' {
				return "", s.refuse(whereColon)
			}
			s.pos++
			s.expect = expectMemberValue
		case expectElementEnd:
			switch c {
			case ',':
				s.pos++
				s.expect = expectElement
			case ']':
				return s.close(ArrayEnd)
			default:
				return "", s.refuse(whereElementEnd)
			}
		case expectMemberEnd:
			switch c {
			case ',':
				s.pos++
				s.expect = expectName
			case '}':
				return s.close(ObjectEnd)
			default:
				return "", s.refuse(whereMemberEnd)
			}
		default:
			// expectEnd: the value is whole, and this byte is no blank.
			return "", s.fault(&Error{Offset: s.offset(), Fault: FaultTrailing})
		}
	}
}

// value begins the value whose first byte is c, at pos, or refuses c with where.
func (s *Scanner) value(c byte, where string) (Kind, error) {
	at := s.offset()
	var k Kind
	switch {
	case c == '{' || c == '[':
		// THE BOUND IS CHECKED BEFORE THE CONTAINER OPENS, as jsontext checks it: the bracket that would
		// make MaxDepth+1 open is the refused one, and nothing past it is read.
		if s.depth == MaxDepth {
			return "", s.fault(&Error{Offset: at, Fault: FaultDepth})
		}
		s.pos++
		s.push(c == '{')
		k, s.expect = ArrayStart, expectFirstElement
		if c == '{' {
			k, s.expect = ObjectStart, expectFirstName
		}
	case c == '"':
		s.pos++
		k, s.text = String, textString
	case c == '-' || '0' <= c && c <= '9':
		k, s.text, s.num = Number, textNumber, numStart
	case c == 't':
		return s.literal(True, "true")
	case c == 'f':
		return s.literal(False, "false")
	case c == 'n':
		return s.literal(Null, "null")
	default:
		return "", s.refuse(where)
	}
	s.kind, s.at, s.whole = k, at, true
	return k, nil
}

// name begins the object member's name whose opening quote is at pos.
func (s *Scanner) name() Kind {
	s.kind, s.at, s.whole = Name, s.offset(), true
	s.pos++
	s.text = textName
	return Name
}

// literal reads the literal lit, whose first byte is at pos, refusing the first byte that differs from it.
func (s *Scanner) literal(k Kind, lit string) (Kind, error) {
	ferr := s.fill(len(lit))
	at := s.offset()
	b := s.buf[s.pos:s.end]
	for i := range len(lit) {
		if i == len(b) {
			// Fewer bytes than the literal, each matching it: the text ended inside it, or the reader
			// failed before saying.
			if ferr != nil {
				return "", ferr
			}
			return "", s.cut()
		}
		if b[i] != lit[i] {
			s.pos += i
			return "", s.refuse("in the literal " + lit)
		}
	}
	s.pos += len(lit)
	s.kind, s.at, s.whole = k, at, true
	s.afterValue()
	return k, nil
}

// close passes the closing bracket or brace at pos, of the container it closes, and answers its kind, or
// the failure of a teed container's writer in its place where it closes that container.
func (s *Scanner) close(k Kind) (Kind, error) {
	s.kind, s.at, s.whole = k, s.offset(), false
	s.pos++
	s.depth--
	s.afterValue()
	if s.tw != nil && s.depth == s.teeDepth {
		s.endTee()
		if err := s.teeEnded(); err != nil {
			return "", err
		}
	}
	return k, nil
}

// push opens a container, an object or an array, one level deeper.
func (s *Scanner) push(object bool) {
	i := s.depth
	if object {
		s.objects[i/8] |= 1 << (i % 8)
	} else {
		s.objects[i/8] &^= 1 << (i % 8)
	}
	s.depth++
}

// inObject reports whether the innermost open container is an object.
func (s *Scanner) inObject() bool {
	i := s.depth - 1
	return s.objects[i/8]&(1<<(i%8)) != 0
}

// afterValue sets what the grammar takes once a value is whole.
func (s *Scanner) afterValue() {
	switch {
	case s.depth == 0:
		s.expect = expectEnd
	case s.inObject():
		s.expect = expectMemberEnd
	default:
		s.expect = expectElementEnd
	}
}

// ended answers the text's end where the grammar stands: io.EOF once the value is whole, a fault anywhere
// else.
func (s *Scanner) ended() error {
	at := s.base + int64(s.end)
	switch s.expect {
	case expectEnd:
		s.kind, s.at, s.err = "", at, io.EOF
		return io.EOF
	case expectTop:
		return s.fault(&Error{Offset: at, Fault: FaultNoValue})
	}
	return s.cut()
}

// peekNonBlank passes the blanks at pos and answers the byte after them, unpassed, or false where the text
// ends first.
func (s *Scanner) peekNonBlank() (byte, bool, error) {
	for {
		for ; s.pos < s.end; s.pos++ {
			switch c := s.buf[s.pos]; c {
			case ' ', '\t', '\n', '\r':
			default:
				return c, true, nil
			}
		}
		if err := s.fill(1); err != nil {
			return 0, false, err
		}
		if s.pos == s.end {
			return 0, false, nil
		}
	}
}

// Skip passes over the current value whole: at a container's start, to its matching end; at a Name, the
// rest of the name and the member's whole value; at a String or a Number, the rest of its text; at a
// literal, nothing. It validates every byte and holds none. After it, the current token is the value's last
// (a container's end), and Next answers the token after the value. Before the first token, or at a
// container's end, it answers [ErrNoValue].
func (s *Scanner) Skip() error {
	if s.err != nil {
		return s.err
	}
	if s.kind != Name && !s.kind.beginsValue() {
		return ErrNoValue
	}
	return s.stop(s.skip())
}

// skip passes over the current value, or at a Name, the name and its member's value.
func (s *Scanner) skip() error {
	s.whole = false
	if s.kind == Name {
		if err := s.passText(); err != nil {
			return err
		}
		if _, err := s.next(); err != nil {
			return err
		}
	}
	return s.skipValue()
}

// skipValue passes over the rest of the value the current token begins.
func (s *Scanner) skipValue() error {
	s.whole = false
	if s.kind != ObjectStart && s.kind != ArrayStart {
		return s.passText()
	}
	// The container is open, at depth: the token that closes it leaves one fewer.
	for until := s.depth - 1; s.depth > until; {
		if _, err := s.next(); err != nil {
			return err
		}
	}
	return nil
}

// Capture answers the current value's bytes as written, from its first byte (a string's opening quote
// included) to its last, with whole true, where they are at most limit bytes; else it passes over the value
// as Skip does, holding none of it past limit, and answers whole false. Either way the scan goes on after
// the value, as after Skip. The current token must begin a value, and none of its text may be read yet:
// anywhere else Capture answers [ErrNoValue]. The bytes are valid until the scanner's next call, and the
// caller does not change them: a reader that keeps them copies them.
func (s *Scanner) Capture(limit int) (raw []byte, whole bool, err error) {
	if s.err != nil {
		return nil, false, s.err
	}
	if !s.whole || !s.kind.beginsValue() {
		return nil, false, ErrNoValue
	}
	// The token's first byte is still in buf: Next reads no further than the token once it has its first
	// byte, and a fill since, a Read's that took nothing, kept it ([Scanner.slide]).
	s.recording, s.spilled, s.over = true, false, false
	s.recFrom, s.limit, s.held = int(s.at-s.base), limit, s.held[:0]
	err = s.stop(s.skipValue())
	s.recording = false
	if err != nil {
		return nil, false, err
	}
	last := s.buf[s.recFrom:s.pos]
	switch {
	case s.over || len(last) > limit-len(s.held):
		s.held = s.held[:0]
		return nil, false, nil
	case !s.spilled:
		return last, true, nil
	}
	s.hold(last)
	return s.held, true, nil
}

// Depth is how many containers are open after the current token: a top-level object's opening brace and
// its member names are at 1, and its closing brace at 0.
func (s *Scanner) Depth() int {
	return s.depth
}

// Offset is where the current token begins in the text, from 0; once Next has answered io.EOF, the text's
// length.
func (s *Scanner) Offset() int64 {
	return s.at
}

// offset is where pos sits in the text.
func (s *Scanner) offset() int64 {
	return s.base + int64(s.pos)
}

// fill reads from r until at least need bytes are unscanned or the text has ended. It answers a reader's
// error alone, wrapped, where the reader failed before need bytes came: at the text's end it answers nil
// with fewer bytes unscanned, so a caller tells the end by the count. Bytes the reader answered with its
// error are scanned before the error is met, and the error is the scan's only once a caller that needs the
// bytes answers it ([Scanner.stop]): a caller whose question the bytes already in hand settle goes on.
func (s *Scanner) fill(need int) error {
	for empty := 0; s.end-s.pos < need && !s.eof; {
		if s.rerr != nil {
			return fmt.Errorf("jsonscan: reading the text at byte %d: %w", s.base+int64(s.end), s.rerr)
		}
		if s.pos == s.end || s.end == len(s.buf) {
			s.slide()
		}
		n, err := s.r.Read(s.buf[s.end:])
		if n < 0 || n > len(s.buf)-s.end {
			s.rerr = fmt.Errorf("the reader answered %d bytes for a buffer of %d", n, len(s.buf)-s.end)
			continue
		}
		s.end += n
		switch {
		case errors.Is(err, io.EOF):
			s.eof = true
		case err != nil:
			s.rerr = err
		case n > 0:
			empty = 0
		default:
			if empty++; empty == maxEmptyReads {
				s.rerr = io.ErrNoProgress
			}
		}
	}
	return nil
}

// slide moves the unscanned bytes to the front of buf, so a read can fill the rest. What a Capture records
// of the bytes before them goes into held first, and what a tee has not written to its writer, since they
// are about to be written over.
//
// WHILE NOTHING OF THE CURRENT TOKEN IS TAKEN (whole), ITS FIRST BYTE MOVES WITH THEM, so Capture can still
// answer the token from it: a Read whose p is too short for the next character fills to learn so and takes
// nothing, and the token is then as Next left it. Nothing taken, that byte is at pos or, a string's or a
// name's opening quote, the byte before it, so a slide keeps at most one byte more.
func (s *Scanner) slide() {
	if s.recording {
		s.hold(s.buf[s.recFrom:s.pos])
		s.spilled = true
	}
	if s.tw != nil {
		s.teeWrite(s.buf[s.teeFrom:s.pos])
	}
	from := s.pos
	if s.whole {
		from = int(s.at - s.base)
	}
	n := copy(s.buf, s.buf[from:s.end])
	s.base += int64(from)
	s.pos, s.end, s.recFrom, s.teeFrom = s.pos-from, n, 0, s.pos-from
}

// hold adds chunk, the next bytes of the value Capture records, to held, or gives up holding where the
// value runs past the limit.
//
// HELD GROWS TO THE LIMIT AND NO FURTHER, doubling until it would pass half the limit and then taking the
// limit itself, so what a capture allocates on its way is at most twice the limit, of which the limit at
// most is held. Doubled without that last step, a size a few bytes short of the limit is followed by one of
// the limit (a capture of 1 MiB allocated 3 MiB so); doubled by append, it would pass the limit.
func (s *Scanner) hold(chunk []byte) {
	if s.over {
		return
	}
	if len(chunk) > s.limit-len(s.held) {
		s.over, s.held = true, s.held[:0]
		return
	}
	if need := len(s.held) + len(chunk); need > cap(s.held) {
		size := max(need, 2*cap(s.held))
		if size > s.limit/2 {
			size = s.limit
		}
		grown := make([]byte, len(s.held), size)
		copy(grown, s.held)
		s.held = grown
	}
	s.held = append(s.held, chunk...)
}

// stop answers err, what a call met as it ended: [errTeeEnded] as the teed value's writer's [*TeeError],
// which ends no scan; else err, which becomes the scan's answer to every call after it (a fault is already;
// a reader's error becomes it here), whatever its chain holds.
func (s *Scanner) stop(err error) error {
	if errors.Is(err, errTeeEnded) {
		return s.takeTee()
	}
	if err != nil && s.err == nil {
		s.err = err
	}
	return err
}

// fault ends the scan with e.
func (s *Scanner) fault(e *Error) error {
	s.err = e
	return e
}

// cut ends the scan at the text's end, inside its value.
func (s *Scanner) cut() error {
	return s.fault(&Error{Offset: s.base + int64(s.end), Fault: FaultCut})
}

// refuse ends the scan at the byte at pos, which the grammar refuses where it stands (where says how).
func (s *Scanner) refuse(where string) error {
	at := s.offset()
	// THE CHARACTER THE BYTE BEGINS, which tells a reader a byte order mark from another byte 0xEF, read from
	// what the reader hands over. Where the reader fails before the bytes in hand settle it (0xEF, and then
	// the reader's error where 0xBB 0xBF could have come), the reader's error is the answer, as it is in an
	// escape: a Rune read from part of a character would class a byte order mark as any other byte. A failure
	// past bytes that settle it changes nothing, since the text is refused there whatever follows.
	ferr := s.fill(utf8.UTFMax)
	b := s.buf[s.pos:s.end]
	if ferr != nil && !utf8.FullRune(b) {
		return ferr
	}
	r, _ := utf8.DecodeRune(b)
	return s.fault(&Error{Offset: at, Fault: FaultByte, Byte: b[0], Rune: r, where: where})
}
