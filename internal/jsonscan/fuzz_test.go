package jsonscan_test

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// seeds are the inputs the fuzzer starts from, each at an edge of the grammar or of the decoding where a
// scanner most easily parts from the two decoders.
var seeds = []string{
	// Escapes of every kind, in both cases of hex.
	`"\"\\\/\b\f\n\r\t"`, `"\u0041\u00e9\u20ac\uFFFF\u0000\u001f"`, `"\u00E9\u20AC"`, `{"\u0061":"\u0062"}`,
	`"\x"`, `"\u12G4"`, `"\u12"`, `"\u"`, `"\`, `"\u1`,
	// Surrogate pairs, lone halves, and a high half followed by a high half.
	`"\ud83d\ude00"`, `"\uD83D\uDE00"`, `"\udbff\udfff"`, `"\ud800"`, `"\ud800x"`, `"\ud800\u0041"`,
	`"\udc00"`, `"\udc00\ud800"`, `"\ud800\ud800"`, `"\ud800\ud800\udc00"`, `"\ud800\n"`, `"\ud800\\"`,
	`"\ud800\udc`, `"\ud800\u`, `"\ud800\`, `"\ud800`, `"\ud800\uZZZZ"`, `"\udbff\ue000"`,
	// Invalid UTF-8 inside strings and outside them.
	"\"\xff\"", "\"\xc3\"", "\"\xe2\x82\"", "\"\xed\xa0\x80\"", "\"\xf4\x90\x80\x80\"", "\"\xc0\x80\"",
	"\"a\x80b\"", "\"\xef\xbf\xbd\"", "\"\xe2\x82", "\xff", "[\xc3]", `{"a":1}` + "\xff", "{\"\xff\":1}",
	"\"\xf0\x9f\x98\x80\"", "\"\xf0\x9f\x98\"",
	// Raw control bytes in strings, and DEL, which a string takes as it stands.
	"\"\x00\"", "\"\x1f\"", "\"\x7f\"", "\"a\tb\"", "\"\n\"", "\"a\rb\"",
	// Numbers at every edge of the grammar.
	"-0", "0.5", "1e+5", "01", "1.", ".5", "-", "0", "-1.5e-10", "1E5", "1e", "1e+", "00", "-01", "1.5.",
	"123456789012345678901234567890", "[1.]", "[-]", "[01]", "1x", "0x10", "+1", "1e5.0", "-.5", "2E-0",
	"[1e]", "[1,-0,0.0,1E+2]", "1.0e", "9.e1",
	// Literals, whole and cut and misspelt.
	"true", "false", "null", "tru", "nul", "fals", "True", "nulL", "[true,false,null]", "tx", "n",
	// Blanks of every form, and what is no blank.
	" \t\n\r1 \t\n\r", "[ 1 , 2 ]", `{ "a" : 1 }`, "\f1", "\v1", "\xc2\xa01", "1\f",
	// A NUL or a byte order mark first, or a part of one.
	"\x00", "\x001", "\xef\xbb\xbf{}", "\xef\xbb\xbf", "\xef\xbb", " \x00", "\x00 ",
	// More after the value.
	"1 2", "{}{}", "[]x", `"a"b`, "truex", "null,", "1\x00", "{} ", "[] \n ]",
	// Structure, whole and broken.
	"{}", "[]", "[[]]", `{"a":[]}`, "[1,]", "[,1]", "{,}", `{"a"}`, `{"a":}`, "{:1}", "[}", "{]", `{"a":1]`,
	`{"a":1,}`, `{"a" 1}`, `{1:2}`, "[1 2]", `{"a":1 "b":2}`, `[[[]]`, `{"a":{"b":{}}}`,
	// Names written twice.
	`{"a":1,"a":2}`, `{"a":{"x":1},"a":{"y":2}}`, `{"a":1,"\u0061":2}`,
	// An empty text, and blanks alone.
	"", "   ", "\n", "\r\n\t ",
	// A transcript line's shape.
	`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"line one\nline \"two\"` +
		` caf\u00e9 \ud83d\ude00"}]}]},"uuid":"5f0c","timestamp":"2026-11-14T10:00:00.000Z"}`,
	// At the depth bound, and one past it.
	strings.Repeat("[", jsonscan.MaxDepth) + strings.Repeat("]", jsonscan.MaxDepth),
	strings.Repeat("[", jsonscan.MaxDepth+1) + strings.Repeat("]", jsonscan.MaxDepth+1),
	strings.Repeat(`{"a":`, jsonscan.MaxDepth) + "1" + strings.Repeat("}", jsonscan.MaxDepth),
	strings.Repeat(`{"a":`, jsonscan.MaxDepth+1) + "1" + strings.Repeat("}", jsonscan.MaxDepth+1),
}

// readings are the ways a fuzzed text is handed to a scanner and read, besides [baseline], every one of
// which reads it as the baseline does: the same tokens, at the same offsets, ending alike. Between them they
// move the reader's chunks and the scanner's buffer edge through every byte of the text: the least buffer (a
// string's quote, a surrogate's escape and the one after it) and one a byte longer slide at every few bytes,
// so every escape, character, literal and number is met cut by the buffer's edge somewhere.
var readings = []reading{
	{name: "a byte at a time", buffer: jsonscan.ReadBufferSize, chunk: 64 << 10, reader: iotest.OneByteReader},
	{name: "the least buffer", buffer: jsonscan.MinBufferSize, chunk: 64 << 10, reader: asIs},
	{name: "a buffer a byte past the least, read in halves", buffer: jsonscan.MinBufferSize + 1, chunk: 5,
		reader: iotest.HalfReader},
	{name: "the last bytes with io.EOF", buffer: jsonscan.MinBufferSize, chunk: 3, reader: iotest.DataErrReader},
}

// FuzzTheScannerReadsWhatBothDecodersRead: FOR EVERY INPUT, THE SCANNER ACCEPTS EXACTLY WHAT json.Valid AND
// jsontext (UNDER THE THREAD LAYER'S OPTIONS) ACCEPT, AND WHERE IT ACCEPTS, ITS TOKENS ARE THEIRS: each kind
// in order, each name's and string's decoded text and each number's literal equal jsontext's tokens, each
// at the offset jsontext reads it at, and the value they make equals what encoding/json's Decode
// (UseNumber) makes. Each token's InvalidUTF8, asked once its text is read, is false exactly where utf8.Valid
// takes the token's bytes as jsontext delimits them, and false at every token whose text is passed over. Where it refuses, the thread layer would class its fault as it classes the line today
// (refusedFault), so moving the layer onto it changes no line's reading. The two decoders are held to each
// other first, so a release that parts them fails here naming the input, and the package doc's account of
// their agreement is rewritten in that change.
//
// THE ANSWER DOES NOT DEPEND ON HOW THE TEXT IS READ: handed over whole, a byte at a time or in halves, with
// the buffer's edge at every byte, its texts read through a p of every size from one byte to 64 and one that
// holds the longest text whole, passed over by Next unread, skipped whole or captured, a scan ends alike.
//
// Mutation: decode `\n` as `n`, pair a high half with any escape after it, take a 10,001st container,
// accept a byte after the value, take a raw control byte in a string, end a number at a cut exponent, set
// a number's offset at its second byte, say a lone surrogate's escape held a byte that is no UTF-8, or keep
// what a text held past its token, and the seed corpus alone fails naming the input.
func FuzzTheScannerReadsWhatBothDecodersRead(f *testing.F) {
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v1ok, v1value := v1Reading(t, data)
		class := layerClass(data)
		if v1ok != (class == "") {
			t.Fatalf("encoding/json and jsontext disagree on %q: json.Valid says %v, jsontext classes it %q",
				data, v1ok, class)
		}
		got := scan(t, data, baseline)
		if c := scannerClass(t, got.err); c != class {
			t.Fatalf("the scanner reads %q as %q (%v), the thread layer as %q", data, c, got.err, class)
		}
		if class == "" {
			want, offsets := textTokens(t, data)
			if !slices.Equal(got.tokens, want) {
				t.Fatalf("the scanner's tokens of %q are\n%+v\njsontext's\n%+v", data, got.tokens, want)
			}
			if !slices.Equal(got.offsets, offsets) {
				t.Fatalf("the scanner's tokens of %q begin at %v, jsontext's at %v", data, got.offsets, offsets)
			}
			if value := tree(got.tokens); !reflect.DeepEqual(value, v1value) {
				t.Fatalf("the scanner's value of %q is %#v, encoding/json's %#v", data, value, v1value)
			}
		}
		for _, how := range readings {
			assertReadsAlike(t, data, got, how, true)
		}
		longest := 0
		for _, tok := range got.tokens {
			longest = max(longest, len(tok.Text))
		}
		for chunk := 1; chunk <= min(longest+1, 64); chunk++ {
			assertReadsAlike(t, data, got, reading{name: "chunked", buffer: jsonscan.MinBufferSize, chunk: chunk,
				reader: asIs}, true)
		}
		assertReadsAlike(t, data, got, reading{name: "the longest text whole", buffer: jsonscan.MinBufferSize,
			chunk: longest + 1, reader: asIs}, true)
		assertReadsAlike(t, data, got, reading{name: "passed over by Next", buffer: jsonscan.MinBufferSize,
			reader: asIs}, false)
		assertSkipsAndCapturesAlike(t, data, got)
	})
}

// assertReadsAlike holds the reading how of data to want, the baseline's: the same tokens (their texts where
// how reads them), at the same offsets, ending alike.
func assertReadsAlike(t *testing.T, data []byte, want scanned, how reading, texts bool) {
	t.Helper()
	got := scan(t, data, how)
	if !texts {
		// A reading that reads no text has every token Next answers, the one whose text holds the fault
		// included, which a reading of that text never finishes; its texts are the baseline's, and none of
		// them, passed over unread, says it held a byte that is no UTF-8.
		if n := len(want.tokens); want.err != nil && len(got.tokens) == n+1 && hasText(got.tokens[n].Kind) {
			got.tokens, got.offsets = got.tokens[:n], got.offsets[:n]
		}
		for i := range got.tokens {
			if got.tokens[i].Invalid {
				t.Fatalf("%s, the %s at byte %d of %q, passed over unread, says it held a byte that is no UTF-8",
					how.name, got.tokens[i].Kind, got.offsets[i], data)
			}
		}
		for i := range min(len(got.tokens), len(want.tokens)) {
			got.tokens[i].Text, got.tokens[i].Invalid = want.tokens[i].Text, want.tokens[i].Invalid
		}
	}
	if !slices.Equal(got.tokens, want.tokens) || !slices.Equal(got.offsets, want.offsets) || !sameEnd(got.err, want.err) {
		t.Fatalf("%s (read %d at a time), %q reads as %s at offsets %v; whole, as %s at %v", how.name, how.chunk,
			data, describe(got), got.offsets, describe(want), want.offsets)
	}
}

// assertSkipsAndCapturesAlike holds Skip and Capture over data to what the baseline found: skipping the
// value, or capturing it within its bound and past it, ends the scan alike, and a capture of a text that is
// one value is that value's bytes as written; and so is capturing every value one level in.
func assertSkipsAndCapturesAlike(t *testing.T, data []byte, want scanned) {
	t.Helper()
	value := bytes.Trim(data, " \t\r\n")
	for _, step := range []struct {
		name string
		do   func(*jsonscan.Scanner) error
	}{
		{"Skip", func(s *jsonscan.Scanner) error { return s.Skip() }},
		{"Capture within its bound", func(s *jsonscan.Scanner) error {
			raw, whole, err := s.Capture(len(data))
			if err == nil && want.err == nil && (!whole || !bytes.Equal(raw, value)) {
				t.Fatalf("Capture of %q within its bound answers %q (whole %v)", data, raw, whole)
			}
			return err
		}},
		{"Capture past its bound", func(s *jsonscan.Scanner) error {
			raw, whole, err := s.Capture(len(value) - 1)
			if err == nil && want.err == nil && (whole || raw != nil) {
				t.Fatalf("Capture of %q, %d bytes, at %d answers %q (whole %v)", data, len(value), len(value)-1,
					raw, whole)
			}
			return err
		}},
	} {
		s := jsonscan.NewSized(bytes.NewReader(data), jsonscan.MinBufferSize)
		_, err := s.Next()
		if err == nil {
			err = step.do(s)
		}
		if err == nil {
			_, err = s.Next()
		}
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if !sameEnd(err, want.err) {
			t.Fatalf("%s at the value of %q ends %v; read whole, %v", step.name, data, err, want.err)
		}
	}
	if want.err != nil || len(want.tokens) < 2 {
		return
	}
	// Every value one level in, captured whole, is its bytes as written where it begins.
	s := jsonscan.NewSized(bytes.NewReader(data), jsonscan.MinBufferSize)
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	for {
		k, err := s.Next()
		if err != nil {
			t.Fatalf("reading %q one level in: %v", data, err)
		}
		if k == jsonscan.ObjectEnd || k == jsonscan.ArrayEnd {
			break
		}
		if k == jsonscan.Name {
			if k, err = s.Next(); err != nil {
				t.Fatalf("reading %q one level in: %v", data, err)
			}
		}
		at := s.Offset()
		raw, whole, err := s.Capture(len(data))
		if err != nil || !whole || !bytes.Equal(raw, data[at:at+int64(len(raw))]) || !begins(k, raw[0]) {
			t.Fatalf("Capture of the %s at byte %d of %q answers %q (whole %v, %v)", k, at, data, raw, whole, err)
		}
	}
}

// hasText reports whether a token of kind k has a text Read reads.
func hasText(k jsonscan.Kind) bool {
	return k == jsonscan.Name || k == jsonscan.String || k == jsonscan.Number
}

// begins reports whether c is a byte a token of kind k begins with.
func begins(k jsonscan.Kind, c byte) bool {
	switch k {
	case jsonscan.ObjectStart:
		return c == '{'
	case jsonscan.ObjectEnd:
		return c == '}'
	case jsonscan.ArrayStart:
		return c == '['
	case jsonscan.ArrayEnd:
		return c == ']'
	case jsonscan.Name, jsonscan.String:
		return c == '"'
	case jsonscan.Number:
		return c == '-' || '0' <= c && c <= '9'
	case jsonscan.True:
		return c == 't'
	case jsonscan.False:
		return c == 'f'
	case jsonscan.Null:
		return c == 'n'
	}
	return false
}
