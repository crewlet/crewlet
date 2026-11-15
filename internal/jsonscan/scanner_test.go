package jsonscan_test

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// readToEnd reads a whole text through s, every text read, and answers what ended it (nil at io.EOF).
func readToEnd(t *testing.T, s *jsonscan.Scanner) error {
	t.Helper()
	p := make([]byte, 64)
	for {
		k, err := s.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if hasText(k) {
			if _, err = readAll(t, s, p); err != nil {
				return err
			}
		}
	}
}

// EVERY FAULT IS MET WHERE THE TEXT STOPS BEING ONE VALUE, AND SAYS WHICH OF THE FIVE IT IS: a reader maps
// each onto a class of its own (the thread layer: no value and a refused byte its faultSyntax, a cut its
// faultPrefix, more after the value its faultTrailing, a NUL or a byte order mark first its faultLead, too
// deep its faultDepth), so each is told apart, at its offset, with the refused byte and the character it
// begins. And the fault is the scan's from then on: every call answers it again.
//
// Mutation: answer io.EOF at a byte after the value, read a text of blanks as cut, or name the byte after a
// refused one, and this fails naming the case.
func TestEveryFaultIsMetAtItsOffset(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		text    string
		fault   jsonscan.Fault
		offset  int64
		refused byte
		char    rune
	}{
		{"an empty text has no value", "", jsonscan.FaultNoValue, 0, 0, 0},
		{"blanks alone are no value", " \t\r\n", jsonscan.FaultNoValue, 4, 0, 0},
		{"an array cut after an element", "[1,2", jsonscan.FaultCut, 4, 0, 0},
		{"an object cut after a colon", `{"a":`, jsonscan.FaultCut, 5, 0, 0},
		{"a string cut", `"abc`, jsonscan.FaultCut, 4, 0, 0},
		{"a literal cut", "tru", jsonscan.FaultCut, 3, 0, 0},
		{"a number cut after its point", "1.", jsonscan.FaultCut, 2, 0, 0},
		{"a number cut after its exponent's sign", "[1e+", jsonscan.FaultCut, 4, 0, 0},
		{"a minus alone", "-", jsonscan.FaultCut, 1, 0, 0},
		{"an escape cut among its digits", `"\u12`, jsonscan.FaultCut, 5, 0, 0},
		{"a NUL first", "\x00{}", jsonscan.FaultByte, 0, 0x00, 0},
		{"a byte order mark first", "\xef\xbb\xbf{}", jsonscan.FaultByte, 0, 0xef, '\uFEFF'},
		{"a byte 0xEF first that begins no character", "\xef{}", jsonscan.FaultByte, 0, 0xef, utf8.RuneError},
		{"a NUL after a blank", " \x00", jsonscan.FaultByte, 1, 0x00, 0},
		{"two elements with no comma", "[1 2]", jsonscan.FaultByte, 3, '2', '2'},
		{"a name with no colon", `{"a" 1}`, jsonscan.FaultByte, 5, '1', '1'},
		{"a raw control byte in a string", "\"a\x01\"", jsonscan.FaultByte, 2, 0x01, 0x01},
		{"an escape JSON has not", `"\x"`, jsonscan.FaultByte, 2, 'x', 'x'},
		{"a \\u escape with a byte that is no hex digit", `"\u12G4"`, jsonscan.FaultByte, 5, 'G', 'G'},
		{"a point with no digit after it", "1.x", jsonscan.FaultByte, 2, 'x', 'x'},
		{"a number that begins with a point", ".5", jsonscan.FaultByte, 0, '.', '.'},
		{"a comma before an array's end", "[1,]", jsonscan.FaultByte, 3, ']', ']'},
		{"a comma before an object's end", `{"a":1,}`, jsonscan.FaultByte, 7, '}', '}'},
		{"a name that is no string", "{1:2}", jsonscan.FaultByte, 1, '1', '1'},
		{"a literal misspelt", "nul1", jsonscan.FaultByte, 3, '1', '1'},
		{"a byte past ASCII outside a string", "[\xff]", jsonscan.FaultByte, 1, 0xff, utf8.RuneError},
		{"a character past ASCII outside a string", "[\xc3\xa9]", jsonscan.FaultByte, 1, 0xc3, '\u00e9'},
		{"a second value", "1 2", jsonscan.FaultTrailing, 2, 0, 0},
		{"a byte after an object", "{} x", jsonscan.FaultTrailing, 3, 0, 0},
		{"a number with a leading zero, which is 0 and more", "01", jsonscan.FaultTrailing, 1, 0, 0},
		{"a literal followed by a letter", "truex", jsonscan.FaultTrailing, 4, 0, 0},
		{"a NUL after a string", "\"a\"\x00", jsonscan.FaultTrailing, 3, 0, 0},
		{"a container opened past the bound", strings.Repeat("[", jsonscan.MaxDepth+1), jsonscan.FaultDepth,
			jsonscan.MaxDepth, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := jsonscan.New(strings.NewReader(c.text))
			err := readToEnd(t, s)
			var e *jsonscan.Error
			if !errors.As(err, &e) {
				t.Fatalf("%q reads to %v, which is no *jsonscan.Error", c.text, err)
			}
			if e.Fault != c.fault || e.Offset != c.offset || e.Byte != c.refused || e.Rune != c.char {
				t.Fatalf("%q is refused with %q at byte %d (byte %#x, %q), want %q at byte %d (byte %#x, %q): %v",
					c.text, e.Fault, e.Offset, e.Byte, e.Rune, c.fault, c.offset, c.refused, c.char, err)
			}
			if !e.Fault.Valid() {
				t.Fatalf("the fault %q is none of the five", e.Fault)
			}
			if _, again := s.Next(); !errors.Is(again, err) {
				t.Fatalf("after the fault, Next answers %v, not the fault", again)
			}
			if _, again := s.Read(make([]byte, 8)); !errors.Is(again, err) {
				t.Fatalf("after the fault, Read answers %v, not the fault", again)
			}
		})
	}
}

// A VALUE NESTED MaxDepth DEEP IS READ, AND THE CONTAINER THAT WOULD OPEN ONE LEVEL MORE IS REFUSED where it
// stands: the bound both decoders and the scrub hold a value to, so a value the scanner reads is one the
// thread layer's decode reads, and one it refuses at the bound is the line the layer reads as faultDepth.
// Arrays, objects and the two by turns, each to the bound and one past it.
//
// Mutation: let the depth reach MaxDepth+1, or refuse at MaxDepth, and this fails naming the nesting.
func TestAValueNestedToTheBoundIsReadAndOnePastItIsRefused(t *testing.T) {
	t.Parallel()
	nest := func(levels int, open, shut func(int) string) (text string, last int64) {
		var b strings.Builder
		for i := range levels {
			last = int64(b.Len())
			b.WriteString(open(i))
		}
		b.WriteString("1")
		for i := levels - 1; i >= 0; i-- {
			b.WriteString(shut(i))
		}
		return b.String(), last
	}
	array := func(int) string { return "[" }
	arrayEnd := func(int) string { return "]" }
	object := func(int) string { return `{"k":` }
	objectEnd := func(int) string { return "}" }
	byTurns := func(i int) string { return [2]string{"[", `{"k":`}[i%2] }
	byTurnsEnd := func(i int) string { return [2]string{"]", "}"}[i%2] }
	for _, c := range []struct {
		name       string
		open, shut func(int) string
	}{
		{"arrays", array, arrayEnd},
		{"objects", object, objectEnd},
		{"arrays and objects by turns", byTurns, byTurnsEnd},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			at, _ := nest(jsonscan.MaxDepth, c.open, c.shut)
			s := jsonscan.New(strings.NewReader(at))
			deepest := 0
			for {
				_, err := s.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("a value nested %d deep is refused: %v", jsonscan.MaxDepth, err)
				}
				deepest = max(deepest, s.Depth())
			}
			if deepest != jsonscan.MaxDepth {
				t.Fatalf("a value nested %d deep reads %d deep at most", jsonscan.MaxDepth, deepest)
			}
			past, opener := nest(jsonscan.MaxDepth+1, c.open, c.shut)
			err := readToEnd(t, jsonscan.New(strings.NewReader(past)))
			var e *jsonscan.Error
			if !errors.As(err, &e) || e.Fault != jsonscan.FaultDepth || e.Offset != opener {
				t.Fatalf("a value nested %d deep reads to %v, want the depth fault at byte %d",
					jsonscan.MaxDepth+1, err, opener)
			}
		})
	}
}

// EVERY ESCAPE, EVERY SURROGATE AND EVERY INVALID BYTE DECODES AS BOTH DECODERS DECODE IT, the two agreeing:
// a pair of halves as its one character, any other half as U+FFFD with the escape after it read on its own,
// a high half after a high half included, and a byte that begins no character as one U+FFFD each, as
// utf8.DecodeRune reads it, a cut sequence, a surrogate written as UTF-8, an overlong form and a code point
// past U+10FFFF among them. These are the places the decoders could most easily part, and the day a release
// parts them this fails naming the text, before a reader moved onto the scanner reads one differently.
//
// Mutation: decode `\n` as `n`, never pair two halves, pair a high half with whatever escape follows it, or
// read a three-byte sequence cut short as one U+FFFD, and this fails naming the text.
func TestEscapesAndInvalidBytesDecodeAsBothDecodersDecodeThem(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ json, text string }{
		{`"\"\\\/\b\f\n\r\t"`, "\"\\/\b\f\n\r\t"},
		{`"\u0041\u00e9\u20AC\uffff\u0000"`, "A\u00e9\u20ac\uffff\x00"},
		{`"\ud83d\ude00"`, "\U0001F600"},
		{`"\uD83D\uDE00"`, "\U0001F600"},
		{`"\udbff\udfff"`, "\U0010FFFF"},
		{`"\ud800"`, "\uFFFD"},
		{`"\udc00"`, "\uFFFD"},
		{`"\ud800x"`, "\uFFFDx"},
		{`"\ud800\u0041"`, "\uFFFDA"},
		{`"\ud800\ud800"`, "\uFFFD\uFFFD"},
		{`"\ud800\ud800\udc00"`, "\uFFFD\U00010000"},
		{`"\udc00\ud800"`, "\uFFFD\uFFFD"},
		{`"\udbff\ue000"`, "\uFFFD\ue000"},
		{`"\ud800\n"`, "\uFFFD\n"},
		{"\"\xff\"", "\uFFFD"},
		{"\"\xe2\x82\"", "\uFFFD\uFFFD"},
		{"\"\xe2\x82x\"", "\uFFFD\uFFFDx"},
		{"\"\xed\xa0\x80\"", "\uFFFD\uFFFD\uFFFD"},
		{"\"\xc0\x80\"", "\uFFFD\uFFFD"},
		{"\"\xf4\x90\x80\x80\"", "\uFFFD\uFFFD\uFFFD\uFFFD"},
		{"\"\xef\xbf\xbd\"", "\uFFFD"},
		{"\"caf\xc3\xa9 \xf0\x9f\x98\x80\x7f\"", "caf\u00e9 \U0001F600\x7f"},
	} {
		dec := json.NewDecoder(strings.NewReader(c.json))
		var v1 string
		if err := dec.Decode(&v1); err != nil || v1 != c.text {
			t.Errorf("encoding/json decodes %q as %q (%v), want %q", c.json, v1, err, c.text)
		}
		tok, err := jsontext.NewDecoder(strings.NewReader(c.json), threadDecoding).ReadToken()
		if err != nil || tok.String() != c.text {
			t.Errorf("jsontext decodes %q as %q (%v), want %q", c.json, tok.String(), err, c.text)
		}
		for _, buffer := range []int{jsonscan.MinBufferSize, jsonscan.ReadBufferSize} {
			s := jsonscan.NewSized(strings.NewReader(c.json), buffer)
			if k, err := s.Next(); err != nil || k != jsonscan.String {
				t.Fatalf("%q begins with %q (%v)", c.json, k, err)
			}
			text, err := readAll(t, s, make([]byte, 64))
			if err != nil || text != c.text {
				t.Errorf("the scanner with a %d-byte buffer decodes %q as %q (%v), want %q", buffer, c.json, text,
					err, c.text)
			}
		}
	}
}

// A READ NEVER ENDS INSIDE A CHARACTER, whatever room its p has and wherever the scanner's buffer ends: each
// read holds whole characters of the decoded text, so a reader can take each read for text by itself (a
// redaction's patterns, a digest of runes), and a p too short for the next character reads none of it and
// says io.ErrShortBuffer. Over a text of characters of every width, escapes standing for them, a pair of
// halves, a lone half and invalid bytes, read through a p of every size from one byte to the whole text,
// with the buffer's edge at every byte.
//
// Mutation: copy what of a character fits p, or answer io.ErrShortBuffer where the character fits, and this
// fails naming the size.
func TestAReadNeverEndsInsideACharacter(t *testing.T) {
	t.Parallel()
	const text = `"a\u00e9` + "\u00e9\u20ac\U0001F600" + `\ud83d\ude00\ud800` + "\xff\xe2\x82" +
		`b\n\"\u20ac` + "\u20ac\u00e9z\""
	const want = "a\u00e9\u00e9\u20ac\U0001F600\U0001F600\uFFFD\uFFFD\uFFFD\uFFFDb\n\"\u20ac\u20ac\u00e9z"
	for _, buffer := range []int{jsonscan.MinBufferSize, jsonscan.MinBufferSize + 1, 17, jsonscan.ReadBufferSize} {
		for chunk := 1; chunk <= len(want)+1; chunk++ {
			s := jsonscan.NewSized(strings.NewReader(text), buffer)
			if _, err := s.Next(); err != nil {
				t.Fatal(err)
			}
			got, err := readAll(t, s, make([]byte, chunk))
			if err != nil || got != want {
				t.Fatalf("read %d at a time with a %d-byte buffer, the text is %q (%v), want %q", chunk, buffer,
					got, err, want)
			}
		}
	}
}

// NEXT ANSWERS io.EOF ONCE THE VALUE AND THE BLANKS AFTER IT ARE READ, and only then is a text known to be
// one value: every token comes first, in order, and after the text's end every call answers io.EOF, Offset
// says the text's length and Depth 0.
//
// Mutation: answer io.EOF at the value's last token, or stop answering it after the first, and this fails.
func TestNextAnswersEOFOnceTheValueAndTheBlanksAfterItAreRead(t *testing.T) {
	t.Parallel()
	const text = `{"a":[1,"b",true,false,null]} ` + "\t\r\n"
	s := jsonscan.New(strings.NewReader(text))
	for _, want := range []jsonscan.Kind{jsonscan.ObjectStart, jsonscan.Name, jsonscan.ArrayStart,
		jsonscan.Number, jsonscan.String, jsonscan.True, jsonscan.False, jsonscan.Null, jsonscan.ArrayEnd,
		jsonscan.ObjectEnd} {
		if k, err := s.Next(); k != want || err != nil || !k.Valid() {
			t.Fatalf("the next token is %q (%v), want %q", k, err, want)
		}
	}
	for range 2 {
		if k, err := s.Next(); !errors.Is(err, io.EOF) || k != "" {
			t.Fatalf("after the value, Next answers %q and %v, want io.EOF", k, err)
		}
	}
	if _, err := s.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Fatalf("after the text, Read answers %v", err)
	}
	if err := s.Skip(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the text, Skip answers %v", err)
	}
	if raw, whole, err := s.Capture(64); !errors.Is(err, io.EOF) || raw != nil || whole {
		t.Fatalf("after the text, Capture answers %q, %v and %v", raw, whole, err)
	}
	if s.Offset() != int64(len(text)) || s.Depth() != 0 {
		t.Fatalf("after the text, Offset is %d and Depth %d, want %d and 0", s.Offset(), s.Depth(), len(text))
	}
}

// readerFault is an error a reader beneath answers.
type readerFault struct{ what string }

func (e *readerFault) Error() string { return e.what }

// handing answers its text with its error, both in one read.
type handing struct {
	text string
	err  error
	done bool
}

func (r *handing) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.text), r.err
}

// stuck answers no bytes and no error, forever; misread answers a count no buffer holds.
type (
	stuck   struct{}
	misread struct{}
)

func (stuck) Read([]byte) (int, error)   { return 0, nil }
func (misread) Read([]byte) (int, error) { return -1, nil }

// AN ERROR OF THE READER BENEATH PASSES THROUGH, so errors.Is and errors.As reach it: a broken archive or a
// cut connection is never read as a text the grammar refused, nor as a text cut short. It is met where the
// scan needs the bytes it stood in the way of, after every byte handed over before it is scanned, so a
// grammar fault those bytes settle is the answer, and one they leave unsettled (a refused 0xEF, which with
// the next two bytes is a byte order mark) is not; it ends the scan as a fault does; and a reader that
// answers nothing forever, or a count no buffer holds, is an error of its own rather than a hang or a panic.
//
// Mutation: answer the reader's error as soon as it is read, before the bytes it came with, drop its
// wrapping, or read a refused byte's character from part of it, and this fails naming the case.
func TestAReaderErrorPassesThrough(t *testing.T) {
	t.Parallel()
	broken := &readerFault{what: "the archive is broken"}
	readThrough := func(t *testing.T, r io.Reader) error {
		t.Helper()
		s := jsonscan.NewSized(r, jsonscan.MinBufferSize)
		err := readToEnd(t, s)
		if err != nil {
			if _, again := s.Next(); !errors.Is(again, err) {
				t.Fatalf("after %v, Next answers %v", err, again)
			}
		}
		return err
	}
	for _, c := range []struct {
		name string
		r    io.Reader
	}{
		{"between tokens", io.MultiReader(strings.NewReader(`[1,`), iotest.ErrReader(broken))},
		{"inside a string's text", io.MultiReader(strings.NewReader(`["abcdefghijklmnop`), iotest.ErrReader(broken))},
		{"inside a literal", io.MultiReader(strings.NewReader(`[tr`), iotest.ErrReader(broken))},
		{"inside an escape", io.MultiReader(strings.NewReader(`["\u00`), iotest.ErrReader(broken))},
		{"after a surrogate's escape, where the next could pair with it",
			io.MultiReader(strings.NewReader(`["\ud800\ud`), iotest.ErrReader(broken))},
		{"with the last bytes", &handing{text: `{"a":`, err: broken}},
		{"after a refused 0xEF that could begin a byte order mark", &handing{text: "\xef", err: broken}},
		{"after a refused byte that begins a character the error cuts",
			io.MultiReader(strings.NewReader("[1,\xe2\x82"), iotest.ErrReader(broken))},
	} {
		err := readThrough(t, c.r)
		var fault *readerFault
		var e *jsonscan.Error
		if !errors.Is(err, broken) || !errors.As(err, &fault) || errors.As(err, &e) {
			t.Errorf("%s, the reader's error reads as %v", c.name, err)
		}
	}
	err := readThrough(t, &handing{text: `[1,x`, err: broken})
	var e *jsonscan.Error
	if !errors.As(err, &e) || e.Fault != jsonscan.FaultByte || e.Offset != 3 {
		t.Errorf("a refused byte handed over with the reader's error reads as %v, want the byte refused", err)
	}
	if err := readThrough(t, &handing{text: "\xef\xbb\xbf{}", err: broken}); !errors.As(err, &e) ||
		e.Fault != jsonscan.FaultByte || e.Offset != 0 || e.Rune != '\uFEFF' {
		t.Errorf("a byte order mark handed over with the reader's error reads as %v, want it refused", err)
	}
	if err := readThrough(t, &handing{text: `["\ud800" x`, err: broken}); !errors.As(err, &e) ||
		e.Fault != jsonscan.FaultByte || e.Offset != 10 {
		t.Errorf("a lone half closed before the reader's error, then a refused byte, reads as %v", err)
	}
	if err := readThrough(t, stuck{}); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("a reader that answers nothing forever reads as %v", err)
	}
	if err := readThrough(t, misread{}); err == nil || errors.As(err, &e) {
		t.Errorf("a reader that answers a count of -1 reads as %v", err)
	}
}

// SKIP PASSES OVER THE VALUE WHOLE, AND ONLY THE VALUE, wherever it is called: at a container's start to
// its matching end, at a name over the name and its member's value, at a string partly read over the rest
// of it, at a number, at a literal; the next token is the one after the value, at the depth the value left.
// It validates what it passes over: a fault inside the value is the answer. Before the first token, or at a
// container's end, there is no value to pass, and it says so.
//
// Mutation: stop a skip where it begins, or at the first end of a container inside the value, or keep an
// unread name's first byte through the skip's fills, and this fails naming the member.
func TestSkipPassesOverTheValueWhole(t *testing.T) {
	t.Parallel()
	const text = `{"a":{"b":[1,{"c":"d"}],"e":"f"},"g":[[],[[]]],"h":"x\"y","i":-12.5e3,"j":true,"k":null}`
	for _, buffer := range []int{jsonscan.MinBufferSize, jsonscan.ReadBufferSize} {
		s := jsonscan.NewSized(strings.NewReader(text), buffer)
		if k, err := s.Next(); k != jsonscan.ObjectStart || err != nil {
			t.Fatalf("the text begins with %q (%v)", k, err)
		}
		for _, name := range []string{"a", "g", "h", "i", "j", "k"} {
			k, err := s.Next()
			if k != jsonscan.Name || err != nil {
				t.Fatalf("with a %d-byte buffer, the token where %q begins is %q (%v)", buffer, name, k, err)
			}
			if got, _ := readAll(t, s, make([]byte, 8)); got != name {
				t.Fatalf("with a %d-byte buffer, the member after a skip is %q, want %q", buffer, got, name)
			}
			if err = s.Skip(); err != nil || s.Depth() != 1 {
				t.Fatalf("skipping the member %q answers %v at depth %d", name, err, s.Depth())
			}
		}
		if k, err := s.Next(); k != jsonscan.ObjectEnd || err != nil {
			t.Fatalf("after the last member, %q (%v)", k, err)
		}
	}

	// At a container's start, at a string partly read, at a number and at a literal.
	s := jsonscan.New(strings.NewReader(text))
	steps := []struct {
		kind  jsonscan.Kind
		read  int
		skip  bool
		depth int
	}{
		{jsonscan.ObjectStart, 0, false, 1}, {jsonscan.Name, 0, false, 1},
		{jsonscan.ObjectStart, 0, true, 1}, {jsonscan.Name, 0, false, 1},
		{jsonscan.ArrayStart, 0, false, 2}, {jsonscan.ArrayStart, 0, true, 2}, {jsonscan.ArrayStart, 0, true, 2},
		{jsonscan.ArrayEnd, 0, false, 1}, {jsonscan.Name, 0, false, 1},
		{jsonscan.String, 2, true, 1}, {jsonscan.Name, 0, false, 1},
		{jsonscan.Number, 0, true, 1}, {jsonscan.Name, 0, false, 1},
		{jsonscan.True, 0, true, 1}, {jsonscan.Name, 0, false, 1},
		{jsonscan.Null, 0, true, 1}, {jsonscan.ObjectEnd, 0, false, 0},
	}
	for i, step := range steps {
		k, err := s.Next()
		if k != step.kind || err != nil {
			t.Fatalf("step %d answers %q (%v), want %q", i, k, err, step.kind)
		}
		if step.read > 0 {
			if n, err := s.Read(make([]byte, step.read)); n != step.read || err != nil {
				t.Fatalf("step %d reads %d (%v)", i, n, err)
			}
		}
		if step.skip {
			if err = s.Skip(); err != nil {
				t.Fatalf("step %d, skipping the %s answers %v", i, k, err)
			}
		}
		if s.Depth() != step.depth {
			t.Fatalf("step %d leaves depth %d, want %d", i, s.Depth(), step.depth)
		}
	}

	// At a name longer than the buffer, unread: the skip's fills slide the buffer past the name's first
	// byte, which nothing can capture from then on.
	long := `{"` + strings.Repeat("n", 3*jsonscan.MinBufferSize) + `":"` +
		strings.Repeat("v", 3*jsonscan.MinBufferSize) + `","z":1}`
	s = jsonscan.NewSized(strings.NewReader(long), jsonscan.MinBufferSize)
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Skip(); err != nil {
		t.Fatalf("skipping a member whose name is longer than the buffer answers %v", err)
	}
	if k, err := s.Next(); k != jsonscan.Name || err != nil {
		t.Fatalf("after skipping a member with a long name, %q (%v)", k, err)
	}
	if got, _ := readAll(t, s, make([]byte, 8)); got != "z" {
		t.Fatalf("after skipping a member with a long name, the next member is %q", got)
	}

	// Skip validates what it passes over.
	s = jsonscan.New(strings.NewReader(`{"a":[1,{"b":x}],"c":1}`))
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	var e *jsonscan.Error
	if err := s.Skip(); !errors.As(err, &e) || e.Fault != jsonscan.FaultByte || e.Offset != 13 {
		t.Fatalf("skipping a member whose value holds a refused byte answers %v", err)
	}

	// Nowhere to skip.
	s = jsonscan.New(strings.NewReader(`[]`))
	if err := s.Skip(); !errors.Is(err, jsonscan.ErrNoValue) {
		t.Fatalf("Skip before the first token answers %v", err)
	}
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Skip(); !errors.Is(err, jsonscan.ErrNoValue) {
		t.Fatalf("Skip at an array's end answers %v", err)
	}
}

// CAPTURE ANSWERS THE VALUE AS WRITTEN, WHOLE, WHERE IT IS AT MOST ITS BOUND, AND PASSES OVER ONE BYTE MORE:
// every kind of value, from its first byte to its last (a string's quotes and escapes as written, a
// container's inner blanks), at a bound of exactly its length and at one byte less; with the buffer's edge
// inside the value or not; and the scan goes on after the value either way. A value that refuses part way
// answers the fault, not a capture.
//
// Mutation: answer a value of exactly the bound as past it, or one byte past it as whole, and this fails
// naming the value.
func TestCaptureAnswersTheValueAsWrittenWithinItsBound(t *testing.T) {
	t.Parallel()
	values := []string{`{"a" : [1, "x"]}`, `"s\u00e9\" "`, `-1.5e+3`, `true`, `false`, `null`, `[]`, `{}`,
		`"` + strings.Repeat("0123456789", 10) + `"`, `[[["deep"]],{"k":{"l":null}}]`, `0`}
	text := "[" + strings.Join(values, " , ") + "]"
	for _, buffer := range []int{jsonscan.MinBufferSize, jsonscan.MinBufferSize + 1, jsonscan.ReadBufferSize} {
		for i, value := range values {
			for _, short := range []bool{false, true} {
				s := jsonscan.NewSized(strings.NewReader(text), buffer)
				if _, err := s.Next(); err != nil {
					t.Fatal(err)
				}
				for range i {
					if _, err := s.Next(); err != nil {
						t.Fatal(err)
					}
					if err := s.Skip(); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.Next(); err != nil {
					t.Fatal(err)
				}
				limit := len(value)
				if short {
					limit--
				}
				raw, whole, err := s.Capture(limit)
				switch {
				case err != nil:
					t.Fatalf("capturing %s at %d answers %v", value, limit, err)
				case short && (whole || raw != nil):
					t.Fatalf("with a %d-byte buffer, %s captured at %d bytes, one short, answers %q whole", buffer,
						value, limit, raw)
				case !short && (!whole || string(raw) != value):
					t.Fatalf("with a %d-byte buffer, %s captured at its length answers %q (whole %v)", buffer,
						value, raw, whole)
				}
				want := jsonscan.ArrayEnd
				if i+1 < len(values) {
					want = map[byte]jsonscan.Kind{'{': jsonscan.ObjectStart, '"': jsonscan.String,
						'-': jsonscan.Number, 't': jsonscan.True, 'f': jsonscan.False, 'n': jsonscan.Null,
						'[': jsonscan.ArrayStart, '0': jsonscan.Number}[values[i+1][0]]
				}
				if k, err := s.Next(); k != want || err != nil {
					t.Fatalf("after capturing %s, the next token is %q (%v), want %q", value, k, err, want)
				}
			}
		}
	}

	// A value that refuses part way answers the fault.
	s := jsonscan.NewSized(strings.NewReader(`[{"a":[1,x]}]`), jsonscan.MinBufferSize)
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	var e *jsonscan.Error
	if raw, _, err := s.Capture(64); !errors.As(err, &e) || e.Fault != jsonscan.FaultByte || e.Offset != 9 ||
		raw != nil {
		t.Fatalf("capturing a value that refuses part way answers %q and %v", raw, err)
	}
}

// DEPTH AND OFFSET SAY WHERE EACH TOKEN SITS: the byte it begins at, blanks before it counted, and how many
// containers are open after it, which is what a reader that models a byte-level load by where a key sits
// (`{"parentUuid":` at a line's start, a key at depth one) reads.
//
// Mutation: count Offset from the byte after a token's first, or Depth before a container opens, and this
// fails naming the token.
func TestDepthAndOffsetSayWhereEachTokenSits(t *testing.T) {
	t.Parallel()
	const text = ` {"parentUuid":null, "a" :[1,{"b":"c"}]} `
	s := jsonscan.NewSized(strings.NewReader(text), jsonscan.MinBufferSize)
	for i, want := range []struct {
		kind   jsonscan.Kind
		offset int64
		depth  int
	}{
		{jsonscan.ObjectStart, 1, 1}, {jsonscan.Name, 2, 1}, {jsonscan.Null, 15, 1}, {jsonscan.Name, 21, 1},
		{jsonscan.ArrayStart, 26, 2}, {jsonscan.Number, 27, 2}, {jsonscan.ObjectStart, 29, 3},
		{jsonscan.Name, 30, 3}, {jsonscan.String, 34, 3}, {jsonscan.ObjectEnd, 37, 2}, {jsonscan.ArrayEnd, 38, 1},
		{jsonscan.ObjectEnd, 39, 0},
	} {
		k, err := s.Next()
		if k != want.kind || err != nil || s.Offset() != want.offset || s.Depth() != want.depth {
			t.Fatalf("token %d is %q at byte %d, depth %d (%v), want %q at %d, depth %d", i, k, s.Offset(),
				s.Depth(), err, want.kind, want.offset, want.depth)
		}
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) || s.Offset() != int64(len(text)) {
		t.Fatalf("the text's end answers %v at %d", err, s.Offset())
	}
}

// READ, SKIP AND CAPTURE SAY SO WHERE THERE IS NOTHING FOR THEM TO TAKE, and change nothing: Read at a token
// with no text, Skip and Capture before any token or at a container's end, Capture at a name and once a Read
// has taken part of a value's text. A text read to its end answers io.EOF, and a read of nothing answers
// nothing. A read too short for the next character takes nothing, so Capture still answers the whole value,
// wherever the buffer's edge falls (here right after the string's opening quote, so the read's fill slides
// the buffer).
//
// Mutation: let Capture answer a string a Read has begun, Read a literal, refuse Capture after a read that
// took nothing, or let that read's fill drop the string's opening quote, and this fails.
func TestReadSkipAndCaptureSayWhereThereIsNothingToTake(t *testing.T) {
	t.Parallel()
	s := jsonscan.New(strings.NewReader(`{"name":"value","n":[true]}`))
	p := make([]byte, 64)
	expect := func(what string, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("%s answers %v, want %v", what, err, want)
		}
	}
	_, err := s.Read(p)
	expect("Read before the first token", err, jsonscan.ErrNoText)
	expect("Skip before the first token", s.Skip(), jsonscan.ErrNoValue)
	_, _, err = s.Capture(64)
	expect("Capture before the first token", err, jsonscan.ErrNoValue)
	if k, _ := s.Next(); k != jsonscan.ObjectStart {
		t.Fatalf("the first token is %q", k)
	}
	_, err = s.Read(p)
	expect("Read at an object's start", err, jsonscan.ErrNoText)
	if k, _ := s.Next(); k != jsonscan.Name {
		t.Fatalf("the second token is %q", k)
	}
	_, _, err = s.Capture(64)
	expect("Capture at a name", err, jsonscan.ErrNoValue)
	if n, err := s.Read(p[:0]); n != 0 || err != nil {
		t.Fatalf("a read of nothing answers %d and %v", n, err)
	}
	if text, _ := readAll(t, s, p); text != "name" {
		t.Fatalf("after Capture at a name, the name reads %q", text)
	}
	_, err = s.Read(p)
	expect("Read at a name read to its end", err, io.EOF)
	if k, _ := s.Next(); k != jsonscan.String {
		t.Fatalf("the member's value is %q", k)
	}
	if n, err := s.Read(p[:2]); n != 2 || err != nil {
		t.Fatalf("reading two bytes of the value answers %d and %v", n, err)
	}
	_, _, err = s.Capture(64)
	expect("Capture once a Read has begun the value", err, jsonscan.ErrNoValue)
	if text, _ := readAll(t, s, p); text != "lue" {
		t.Fatalf("after Capture refused, the rest of the value reads %q", text)
	}
	for _, want := range []jsonscan.Kind{jsonscan.Name, jsonscan.ArrayStart, jsonscan.True} {
		if k, _ := s.Next(); k != want {
			t.Fatalf("the next token is %q, want %q", k, want)
		}
	}
	_, err = s.Read(p)
	expect("Read at a literal", err, jsonscan.ErrNoText)
	if k, _ := s.Next(); k != jsonscan.ArrayEnd {
		t.Fatalf("the next token is %q, want the array's end", k)
	}
	expect("Skip at an array's end", s.Skip(), jsonscan.ErrNoValue)
	_, _, err = s.Capture(64)
	expect("Capture at an array's end", err, jsonscan.ErrNoValue)
	if k, _ := s.Next(); k != jsonscan.ObjectEnd {
		t.Fatalf("the last token is %q", k)
	}

	for _, blanks := range []int{0, jsonscan.MinBufferSize - 1} {
		text := strings.Repeat(" ", blanks) + "\"\u00e9abc\""
		s := jsonscan.NewSized(strings.NewReader(text), jsonscan.MinBufferSize)
		if k, err := s.Next(); k != jsonscan.String || err != nil {
			t.Fatalf("%q begins with %q (%v)", text, k, err)
		}
		if n, err := s.Read(p[:1]); n != 0 || !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("a one-byte read of %q answers %d and %v, want io.ErrShortBuffer", text, n, err)
		}
		if raw, whole, err := s.Capture(64); string(raw) != text[blanks:] || !whole || err != nil {
			t.Fatalf("after a read that took nothing, %d blanks in, Capture answers %q (whole %v, %v)", blanks,
				raw, whole, err)
		}
		if _, err := s.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("after the capture, Next answers %v", err)
		}
	}
}

// THE WORD-AT-A-TIME RUNS STOP WHERE THE BYTE-AT-A-TIME RULE STOPS: a string holds every byte but a quote, a
// backslash and a control character as it stands, and decoding copies the ASCII ones of those as they
// stand. Every byte value at every place in a run of plain bytes and of bytes past ASCII, and random runs,
// each against the rule a byte at a time.
//
// Mutation: test the control bytes against 0x1f rather than 0x20, drop the quote's or the backslash's term,
// or let decoding copy a byte past ASCII, and this fails naming the byte.
func TestTheWordAtATimeRunsStopWhereTheByteRuleStops(t *testing.T) {
	t.Parallel()
	stringRule := func(b []byte) int {
		for i, c := range b {
			if c == '"' || c == '\\' || c < 0x20 {
				return i
			}
		}
		return len(b)
	}
	textRule := func(b []byte) int {
		for i, c := range b {
			if c == '"' || c == '\\' || c < 0x20 || c >= 0x80 {
				return i
			}
		}
		return len(b)
	}
	check := func(b []byte) {
		t.Helper()
		if got, want := jsonscan.StringRun(b), stringRule(b); got != want {
			t.Fatalf("the string run of %q is %d bytes, want %d", b, got, want)
		}
		if got, want := jsonscan.TextRun(b), textRule(b); got != want {
			t.Fatalf("the text run of %q is %d bytes, want %d", b, got, want)
		}
	}
	for _, background := range []byte{'a', 0xc3, 0x7f, 0x20, 0xff} {
		for c := range 256 {
			for at := range 20 {
				b := bytes.Repeat([]byte{background}, 20)
				b[at] = byte(c)
				check(b)
			}
		}
	}
	random := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		b := make([]byte, random.IntN(40))
		for i := range b {
			// Mostly plain bytes, so a run is long enough to cross words before it stops.
			b[i] = byte(0x20 + random.IntN(0x5f))
			if random.IntN(16) == 0 {
				b[i] = byte(random.IntN(256))
			}
		}
		check(b)
	}
}

// RESET READS THE NEXT TEXT WITH THE SAME BUFFERS, after a text read to its end and after a fault alike,
// so a reader of a thread's lines allocates its buffer once, not once a line: reading a short line again
// and again allocates nothing.
//
// Mutation: keep the fault or the grammar's place across a Reset, or allocate a buffer in it, and this
// fails.
func TestResetReadsTheNextTextWithTheSameBuffers(t *testing.T) {
	// Not parallel: testing.AllocsPerRun counts every goroutine's allocations.
	s := jsonscan.New(strings.NewReader(`{"a":1}`))
	if err := readToEnd(t, s); err != nil {
		t.Fatal(err)
	}
	s.Reset(strings.NewReader(`[true`))
	var e *jsonscan.Error
	if err := readToEnd(t, s); !errors.As(err, &e) || e.Fault != jsonscan.FaultCut || e.Offset != 5 {
		t.Fatalf("the second text reads to %v, want cut at 5", err)
	}
	s.Reset(strings.NewReader(` "b" `))
	if k, err := s.Next(); k != jsonscan.String || err != nil || s.Offset() != 1 {
		t.Fatalf("after a fault and a Reset, the text begins with %q at %d (%v)", k, s.Offset(), err)
	}
	line := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"a \"line\""}]},"n":12}`)
	r := bytes.NewReader(line)
	p := make([]byte, 64)
	allocs := testing.AllocsPerRun(100, func() {
		r.Reset(line)
		s.Reset(r)
		for {
			k, err := s.Next()
			if err != nil {
				break
			}
			if hasText(k) {
				for {
					if _, err = s.Read(p); err != nil {
						break
					}
				}
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("reading a line again after a Reset allocates %v times", allocs)
	}
}
