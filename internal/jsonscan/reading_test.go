package jsonscan_test

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// threadDecoding is the Claude Code thread layer's options (internal/sandbox/executor/claudethread.go's
// threadDecoding), under which the scanner is held to jsontext: duplicate names allowed, invalid UTF-8 read
// as U+FFFD, names matched exactly.
var threadDecoding = jsonv2.JoinOptions(
	jsonv2.MatchCaseInsensitiveNames(false),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// seenToken is one token as a reader sees it: its kind, a Name's or a String's decoded text or a Number's
// literal, and how many containers are open after it.
type seenToken struct {
	Kind  jsonscan.Kind
	Text  string
	Depth int
}

// scanned is what one reading of a text yields: its tokens and where each begins, and what ended the
// reading, nil where Next answered io.EOF.
type scanned struct {
	tokens  []seenToken
	offsets []int64
	err     error
}

// reading is one way of handing a text to a scanner and reading it.
type reading struct {
	name string
	// buffer is the scanner's read buffer (jsonscan.NewSized).
	buffer int
	// reader wraps the text's reader: as it is, a byte at a time, or in halves.
	reader func(io.Reader) io.Reader
	// chunk is Read's p; 0 reads no text, so Next passes over every one.
	chunk int
}

// baseline is the reading every other is compared with: New's buffer, the text handed over whole, each text
// read through a p that holds it.
var baseline = reading{name: "baseline", buffer: jsonscan.ReadBufferSize, chunk: 64 << 10, reader: asIs}

// asIs hands a text's reader over as it is.
func asIs(r io.Reader) io.Reader { return r }

// scan reads data the way how says, every token to the end.
func scan(t *testing.T, data []byte, how reading) scanned {
	t.Helper()
	s := jsonscan.NewSized(how.reader(bytes.NewReader(data)), how.buffer)
	var out scanned
	var p []byte
	if how.chunk > 0 {
		p = make([]byte, how.chunk)
	}
	for {
		k, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			out.err = err
			return out
		}
		tok := seenToken{Kind: k, Depth: s.Depth()}
		at := s.Offset()
		if p != nil && hasText(k) {
			text, readErr := readAll(t, s, p)
			if readErr != nil {
				out.err = readErr
				return out
			}
			tok.Text = text
		}
		out.tokens = append(out.tokens, tok)
		out.offsets = append(out.offsets, at)
	}
}

// readAll reads the current token's text to its end through p, as a reader of it would, and holds every read
// to Read's contract: a read ends on a character's boundary (the text is valid UTF-8, so a read that is not
// split one), and answers io.ErrShortBuffer only where p cannot take the next character, reading nothing.
func readAll(t *testing.T, s *jsonscan.Scanner, p []byte) (string, error) {
	t.Helper()
	var text []byte
	for {
		n, err := s.Read(p)
		if !utf8.Valid(p[:n]) {
			t.Fatalf("a read of %d bytes ends inside a character: %q (after %q)", len(p), p[:n], text)
		}
		text = append(text, p[:n]...)
		switch {
		case errors.Is(err, io.EOF):
			return string(text), nil
		case errors.Is(err, io.ErrShortBuffer):
			var wide [utf8.UTFMax]byte
			m, wideErr := s.Read(wide[:])
			if n != 0 || wideErr != nil {
				t.Fatalf("a read of %d bytes says its buffer is short having read %d, and a read of %d after it "+
					"answers %d bytes and %v", len(p), n, len(wide), m, wideErr)
			}
			if _, size := utf8.DecodeRune(wide[:m]); size <= len(p) {
				t.Fatalf("a read of %d bytes says its buffer is short where the next character, %q, fits it",
					len(p), wide[:size])
			}
			text = append(text, wide[:m]...)
		case err != nil:
			return string(text), err
		}
	}
}

// v1Reading is what encoding/json reads of data: whether json.Valid takes it, and where it does, the value a
// Decoder with UseNumber decodes into an any.
func v1Reading(t *testing.T, data []byte) (bool, any) {
	t.Helper()
	if !json.Valid(data) {
		return false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("json.Valid takes %q and a Decoder refuses it: %v", data, err)
	}
	return true, v
}

// layerClass is how the thread layer classes data as a line, under its options, as its refusedFault does
// (claudethread.go): "lead" for a NUL or a byte order mark first, then by one jsontext.Decoder, "prefix" for a
// value cut at the end, "depth" for one nested too deep, "syntax" for any other failure before a whole value,
// "trailing" for a whole value followed by more; and "" for one whole value.
func layerClass(data []byte) string {
	if len(data) > 0 && data[0] == 0 || bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		return "lead"
	}
	dec := jsontext.NewDecoder(bytes.NewReader(data), threadDecoding)
	_, err := dec.ReadValue()
	var syntax *jsontext.SyntacticError
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "prefix"
	case errors.As(err, &syntax) && syntax.Err != nil && syntax.Err.Error() == "exceeded max depth":
		return "depth"
	case err != nil:
		return "syntax"
	}
	if _, err = dec.ReadToken(); !errors.Is(err, io.EOF) {
		return "trailing"
	}
	return ""
}

// scannerClass is how the thread layer would class the scanner's answer, by the mapping the package doc
// states: a refused NUL or byte order mark at offset 0 is "lead", any other refused byte or a text with no
// value "syntax", a cut "prefix", too deep "depth", more after the value "trailing"; "" where the text was
// read to its end.
func scannerClass(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var e *jsonscan.Error
	if !errors.As(err, &e) {
		t.Fatalf("a text read from memory ends with %v, which is no *jsonscan.Error", err)
	}
	switch e.Fault {
	case jsonscan.FaultByte:
		if e.Offset == 0 && (e.Rune == 0 || e.Rune == '\uFEFF') {
			return "lead"
		}
		return "syntax"
	case jsonscan.FaultNoValue:
		return "syntax"
	case jsonscan.FaultCut:
		return "prefix"
	case jsonscan.FaultDepth:
		return "depth"
	case jsonscan.FaultTrailing:
		return "trailing"
	}
	t.Fatalf("an *Error with the fault %q, which is none of the five", e.Fault)
	return ""
}

// textTokens is jsontext's tokens of data, which it reads as one value, under the thread layer's options, and
// where each begins: a string token is a Name where its object expects a name.
//
// A TOKEN BEGINS WHERE JSONTEXT'S LAST ONE ENDED, past the blanks and the one ',' or ':' between them:
// jsontext says where a token ends (InputOffset after ReadToken), and in a text it reads as one value only
// those bytes lie between two tokens, so the offsets are jsontext's own, never the scanner's.
func textTokens(t *testing.T, data []byte) ([]seenToken, []int64) {
	t.Helper()
	dec := jsontext.NewDecoder(bytes.NewReader(data), threadDecoding)
	var (
		tokens  []seenToken
		offsets []int64
	)
	for {
		k, n := dec.StackIndex(dec.StackDepth())
		name := k == '{' && n%2 == 0
		at := dec.InputOffset()
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return tokens, offsets
		}
		if err != nil {
			t.Fatalf("jsontext reads %q as one value and then refuses a token of it: %v", data, err)
		}
		next := seenToken{Depth: dec.StackDepth()}
		switch tok.Kind() {
		case '{':
			next.Kind = jsonscan.ObjectStart
		case '}':
			next.Kind = jsonscan.ObjectEnd
		case '[':
			next.Kind = jsonscan.ArrayStart
		case ']':
			next.Kind = jsonscan.ArrayEnd
		case '"':
			next.Kind, next.Text = jsonscan.String, tok.String()
			if name {
				next.Kind = jsonscan.Name
			}
		case '0':
			next.Kind, next.Text = jsonscan.Number, tok.String()
		case 't':
			next.Kind = jsonscan.True
		case 'f':
			next.Kind = jsonscan.False
		case 'n':
			next.Kind = jsonscan.Null
		default:
			t.Fatalf("jsontext answers a token of kind %v", tok.Kind())
		}
		for at < int64(len(data)) && bytes.IndexByte([]byte(" \t\r\n,:"), data[at]) >= 0 {
			at++
		}
		tokens = append(tokens, next)
		offsets = append(offsets, at)
	}
}

// tree is the value tokens make, as encoding/json's Decode with UseNumber makes it into an any: an object a
// map whose name written twice holds its last value, an array a slice, a number its literal.
func tree(tokens []seenToken) any {
	i := 0
	var value func() any
	value = func() any {
		tok := tokens[i]
		i++
		switch tok.Kind {
		case jsonscan.ObjectStart:
			members := map[string]any{}
			for tokens[i].Kind != jsonscan.ObjectEnd {
				name := tokens[i].Text
				i++
				members[name] = value()
			}
			i++
			return members
		case jsonscan.ArrayStart:
			elements := []any{}
			for tokens[i].Kind != jsonscan.ArrayEnd {
				elements = append(elements, value())
			}
			i++
			return elements
		case jsonscan.String:
			return tok.Text
		case jsonscan.Number:
			return json.Number(tok.Text)
		case jsonscan.True:
			return true
		case jsonscan.False:
			return false
		}
		return nil
	}
	return value()
}

// sameEnd reports whether two readings ended alike: both at the text's end, or both with the same error,
// the fault's every field and its message included.
func sameEnd(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var ea, eb *jsonscan.Error
	if errors.As(a, &ea) != errors.As(b, &eb) {
		return false
	}
	if ea != nil && (ea.Offset != eb.Offset || ea.Fault != eb.Fault || ea.Byte != eb.Byte || ea.Rune != eb.Rune) {
		return false
	}
	return a.Error() == b.Error()
}

// describe says what a reading yielded, for a failure's message.
func describe(r scanned) string {
	return fmt.Sprintf("%d tokens %+v ending %v", len(r.tokens), r.tokens, r.err)
}
