package cliagent

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// MessageStream reads a reply's text AS IT ARRIVES and reveals the prose of
// its envelope's message, decoded, and nothing else.
//
// It exists because text mode asks the model for an envelope ([RenderContract])
// rather than prose, so a CLI that streams its tokens streams JSON: shown raw,
// a live view would draw `{"message": "The sea st` and then a tool call's
// arguments. This is the incremental reader that turns those fragments into
// the characters of the `message` string as they land, so the person watching
// reads the answer being written rather than the envelope carrying it.
//
// A VIEW, NEVER THE AUTHORITY. [ParseEnvelope] still reads the whole reply
// once the call has finished, and what it decides is the round's content. The
// stream is deliberately narrower than that parser: it follows ONE shape (the
// reply opens with an object, bare or inside one ``` fence, after nothing but
// whitespace), reads the FIRST top-level string under one of [messageKeys],
// and stops at the end of that string. Anything else (prose before the
// object, a JSON syntax error, an escape JSON does not have) and it reveals
// nothing more, because guessing is how a view shows text the answer does
// not contain. Where its reading and the parser's still differ (a second
// fenced block the parser prefers, a higher-priority synonym later in the
// object, a duplicate key), the caller finds out by comparing the two once
// the call ends ([llm.Reconcile]) and retracts what was shown.
//
// Decoding follows encoding/json's own rules exactly, because the parser it
// is checked against IS encoding/json: the seven short escapes and `\u` with
// four hex digits, a UTF-16 surrogate pair joined into one code point, an
// unpaired surrogate or an invalid UTF-8 byte read as U+FFFD, and a raw
// control character refused. A fragment may end anywhere: inside an escape,
// between the halves of a surrogate pair, or in the middle of a multi-byte
// character, which is held until the rest of it arrives.
//
// Not safe for concurrent use: one stream reads one call's output, on the
// goroutine that reads the CLI's stdout.
type MessageStream struct {
	state streamState

	// ticks counts the backticks of an opening fence.
	ticks int

	// key is the top-level key being read, decoded, and keyLong says it
	// outgrew every synonym, so its bytes stop being kept.
	key     []byte
	keyLong bool
	// chosen says the value about to be read is the message.
	chosen bool

	// skip tracks a value that is not the message: how deep inside it the
	// reader is, and whether it is inside a string there.
	skipDepth  int
	skipString bool
	skipEscape bool

	// esc holds an escape being read, from its backslash on: `\n` is two
	// bytes, `A` six. high is the surrogate a pair opened with, waiting
	// to see whether the next escape closes it.
	esc  []byte
	high rune

	// raw holds the leading bytes of a multi-byte character whose remainder
	// is still to arrive.
	raw []byte

	// out collects what one Write reveals.
	out strings.Builder
}

// streamState is where in the reply a [MessageStream] is.
type streamState int

const (
	// Before anything but whitespace has arrived.
	streamLead streamState = iota
	// Inside the backticks of an opening fence.
	streamTicks
	// On the fence's info string, up to the end of its line.
	streamInfo
	// After the fence's line, before the object opens.
	streamFenced
	// Inside the object, where a key or its end belongs.
	streamKey
	// Inside a key's string.
	streamKeyString
	// After a key, before its colon.
	streamColon
	// After a colon, before the value.
	streamValue
	// Inside a value that is not the message.
	streamSkip
	// Inside the message string.
	streamMessage
	// After a value, before a comma or the object's end.
	streamAfter
	// Finished: the message string ended, the object closed without one,
	// or the reply took a shape this reader does not follow. Nothing more
	// is revealed.
	streamDone
)

// keyCap bounds the bytes kept of one key: the longest message synonym, so a
// longer key is known not to be one without holding all of it.
var keyCap = func() int {
	longest := 0
	for _, k := range messageKeys {
		longest = max(longest, len(k))
	}
	return longest
}()

// NewMessageStream returns a stream at the start of a reply.
func NewMessageStream() *MessageStream { return &MessageStream{} }

// Write reads the next fragment of the reply and returns the message prose it
// revealed, which may be empty. The concatenation of every return is a prefix
// of the first message string in the reply, however the reply was cut into
// fragments.
func (s *MessageStream) Write(fragment string) string {
	s.out.Reset()
	for i := 0; i < len(fragment) && s.state != streamDone; i++ {
		s.step(fragment[i])
	}
	return s.out.String()
}

// step reads one byte of the reply.
func (s *MessageStream) step(c byte) {
	switch s.state {
	case streamLead:
		switch {
		case isJSONSpace(c):
		case c == '{':
			s.state = streamKey
		case c == '`':
			s.state, s.ticks = streamTicks, 1
		default:
			s.state = streamDone
		}
	case streamTicks:
		switch {
		case c != '`':
			s.state = streamDone
		case s.ticks == 2:
			s.state = streamInfo
		default:
			s.ticks++
		}
	case streamInfo:
		// The info string is skipped whatever it says, as [fencedBlocks]
		// skips it: a fence is read by its body, not its label.
		if c == '\n' {
			s.state = streamFenced
		}
	case streamFenced:
		switch {
		case isJSONSpace(c):
		case c == '{':
			s.state = streamKey
		default:
			s.state = streamDone
		}
	case streamKey:
		switch {
		case isJSONSpace(c):
		case c == '"':
			s.state, s.key, s.keyLong = streamKeyString, s.key[:0], false
		default:
			// `}` closes an object that held no message, and anything else
			// is not JSON this reader follows. Either way there is nothing
			// left to reveal.
			s.state = streamDone
		}
	case streamKeyString:
		s.keyByte(c)
	case streamColon:
		switch {
		case isJSONSpace(c):
		case c == ':':
			s.state = streamValue
			s.chosen = !s.keyLong && slices.Contains(messageKeys, string(s.key))
		default:
			s.state = streamDone
		}
	case streamValue:
		switch {
		case isJSONSpace(c):
		case c == '"' && s.chosen:
			s.state = streamMessage
		default:
			// A synonym holding something other than a string is passed
			// over, as [fromDocument] passes over it to the next synonym.
			s.state, s.skipDepth, s.skipString, s.skipEscape = streamSkip, 0, false, false
			s.skipByte(c)
		}
	case streamSkip:
		s.skipByte(c)
	case streamMessage:
		s.messageByte(c)
	case streamAfter:
		switch {
		case isJSONSpace(c):
		case c == ',':
			s.state = streamKey
		default:
			s.state = streamDone
		}
	}
}

// keyByte reads one byte of a top-level key.
//
// Keys are compared after decoding, as encoding/json compares them, so
// `"mess\u0061ge"` is the message key. They are decoded by the same reader as
// the message itself, with its output pointed at the key buffer instead.
func (s *MessageStream) keyByte(c byte) {
	if s.decodeByte(c, s.appendKey) {
		s.state = streamColon
	}
}

// appendKey keeps a decoded key rune, until the key is too long to be a
// synonym.
func (s *MessageStream) appendKey(r rune) {
	if s.keyLong {
		return
	}
	s.key = utf8.AppendRune(s.key, r)
	if len(s.key) > keyCap {
		s.keyLong = true
	}
}

// messageByte reads one byte of the message string.
func (s *MessageStream) messageByte(c byte) {
	if s.decodeByte(c, s.reveal) {
		// The message is complete. What follows is the rest of the
		// envelope, which the parser reads once the call is over.
		s.state = streamDone
	}
}

// reveal adds one decoded rune of the message to what this Write returns.
func (s *MessageStream) reveal(r rune) { s.out.WriteRune(r) }

// decodeByte reads one byte inside a JSON string, hands every rune it
// completes to emit, in order, and reports whether the byte closed the
// string. A byte JSON does not allow there ends the stream.
func (s *MessageStream) decodeByte(c byte, emit func(rune)) (closed bool) {
	switch {
	case len(s.raw) > 0:
		return s.rawByte(c, emit)
	case s.esc != nil:
		s.escapeByte(c, emit)
		return false
	case s.high != 0:
		// A surrogate waits for a `\u` that may complete it. Anything else
		// (the closing quote included) leaves it unpaired, which
		// encoding/json reads as U+FFFD, and the byte is then read on its
		// own.
		if c == '\\' {
			s.esc = append(s.esc[:0], c)
			return false
		}
		emit(unicode.ReplacementChar)
		s.high = 0
		return s.decodeByte(c, emit)
	case c == '"':
		return true
	case c == '\\':
		s.esc = append(s.esc[:0], c)
	case c < ' ':
		// A raw control character is a syntax error in a JSON string.
		s.state = streamDone
	case c < utf8.RuneSelf:
		emit(rune(c))
	default:
		s.raw = s.raw[:0]
		return s.rawByte(c, emit)
	}
	return false
}

// rawByte adds one byte to a multi-byte character and emits it once it is
// whole, reporting whether a byte read along the way closed the string.
func (s *MessageStream) rawByte(c byte, emit func(rune)) (closed bool) {
	s.raw = append(s.raw, c)
	if !utf8.FullRune(s.raw) {
		return false
	}
	r, size := utf8.DecodeRune(s.raw)
	// An invalid byte is one U+FFFD, as encoding/json reads it, and the
	// bytes after it are read again as the start of what follows. Only the
	// byte just added can be among them unread (every earlier one left the
	// character incomplete), so a quote can only be the last of them.
	rest := slices.Clone(s.raw[size:])
	s.raw = s.raw[:0]
	emit(r)
	for _, b := range rest {
		if s.decodeByte(b, emit) {
			return true
		}
	}
	return false
}

// escapeByte reads one byte of an escape sequence, whose backslash is held.
func (s *MessageStream) escapeByte(c byte, emit func(rune)) {
	s.esc = append(s.esc, c)
	if len(s.esc) == 2 {
		if c == 'u' {
			return
		}
		if s.high != 0 {
			// A short escape after a surrogate leaves the surrogate
			// unpaired.
			emit(unicode.ReplacementChar)
			s.high = 0
		}
		s.esc = nil
		r, ok := shortEscape(c)
		if !ok {
			s.state = streamDone
			return
		}
		emit(r)
		return
	}
	if !isHex(c) {
		// `\u` with a non-hex digit is a syntax error, so the reply will not
		// parse whatever comes next.
		s.state = streamDone
		return
	}
	if len(s.esc) < 6 {
		return
	}
	r := hexRune(s.esc[2:6])
	s.esc = nil
	if s.high != 0 {
		high := s.high
		s.high = 0
		if pair := utf16.DecodeRune(high, r); pair != unicode.ReplacementChar {
			emit(pair)
			return
		}
		// Not the other half: the first surrogate stands alone, and this
		// escape is read on its own, which may open a pair of its own.
		emit(unicode.ReplacementChar)
	}
	if utf16.IsSurrogate(r) {
		s.high = r
		return
	}
	emit(r)
}

// skipByte reads one byte of a value that is not the message: an object or a
// list to its matching close, a string to its closing quote, a literal or a
// number to the delimiter after it.
func (s *MessageStream) skipByte(c byte) {
	if s.skipString {
		switch {
		case s.skipEscape:
			s.skipEscape = false
		case c == '\\':
			s.skipEscape = true
		case c == '"':
			s.skipString = false
			if s.skipDepth == 0 {
				s.state = streamAfter
			}
		}
		return
	}
	switch c {
	case '"':
		s.skipString = true
	case '{', '[':
		s.skipDepth++
	case '}', ']':
		if s.skipDepth == 0 {
			// The object's own close, after a literal or a number.
			s.state = streamDone
			return
		}
		s.skipDepth--
		if s.skipDepth == 0 {
			s.state = streamAfter
		}
	case ',':
		if s.skipDepth == 0 {
			s.state = streamKey
		}
	default:
		if s.skipDepth == 0 && isJSONSpace(c) {
			s.state = streamAfter
		}
	}
}

// shortEscape decodes the one-character escapes JSON has.
func shortEscape(c byte) (rune, bool) {
	switch c {
	case '"', '\\', '/':
		return rune(c), true
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	}
	return 0, false
}

// isJSONSpace is the whitespace JSON allows between tokens.
func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// isHex reports a hexadecimal digit.
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// hexRune reads four hexadecimal digits, already checked by [isHex].
func hexRune(digits []byte) rune {
	var r rune
	for _, c := range digits {
		switch {
		case c <= '9':
			r = r<<4 | rune(c-'0')
		case c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			r = r<<4 | rune(c-'a'+10)
		}
	}
	return r
}
