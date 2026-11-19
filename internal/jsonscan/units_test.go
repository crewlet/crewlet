package jsonscan_test

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// unitPieces are the pieces a counted text is built of: every escape, pairs and lone and reversed halves,
// invalid, overlong, CESU-8 and cut sequences, U+FFFD written, escaped and as the bytes of a cut character,
// CJK, emoji, and plain runs long enough to cross a fill.
var unitPieces = []string{
	`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`, `\u0041`, `\u00e9`, `\u20AC`, `\uffff`, `\u0000`,
	`\ud83d\ude00`, `\uDBFF\uDFFF`, `\ud800`, `\udc00`, `\ude00\ud83d`, `\ud800\ud800\udc00`,
	"\xff", "\xc3", "\xe2\x82", "\xed\xa0\x80", "\xc0\x80", "\xe0\x80\x80", "\xf4\x90\x80\x80",
	"\xed\xa0\xbd\xed\xb8\x80", "\xf0\x9f\x98", "\x80", "\xf5", "\xef\xbf\xbd", `\ufffd`, "\xe2",
	"é", "€", "日本語", "😀", "\U0010FFFF", "a", "plain words ", strings.Repeat("x", 40), strings.Repeat("é", 9),
}

// unitTexts is texts built of [unitPieces], drawn from a seeded source: strings and names of every piece
// alone, then mixed, inside objects and lists beside numbers and literals.
func unitTexts() []string {
	var texts []string
	for _, p := range unitPieces {
		texts = append(texts, `"`+p+`"`, `{"`+p+`":"`+p+p+`"}`)
	}
	rng := rand.New(rand.NewPCG(5, 8))
	str := func() string {
		var b strings.Builder
		for range rng.IntN(12) {
			b.WriteString(unitPieces[rng.IntN(len(unitPieces))])
		}
		return `"` + b.String() + `"`
	}
	for range 200 {
		texts = append(texts, `{`+str()+`: [`+str()+`, 12.5e-3, true, `+str()+`], "n": null, `+str()+`:{}}`,
			` [`+str()+` , `+str()+`, -0, false] `)
	}
	return texts
}

// writtenUnits is how long text is in UTF-16 code units as written: utf8.DecodeRune's reading, a character
// past the basic plane two units and every other one, each byte that begins none one.
func writtenUnits(text []byte) int64 {
	var n int64
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		text = text[size:]
		n += int64(utf16Len(r))
	}
	return n
}

// utf16Len is how many UTF-16 code units r is.
func utf16Len(r rune) int {
	if r > 0xffff {
		return 2
	}
	return 1
}

// decodedUnits is how long text, valid UTF-8, is in UTF-16 code units.
func decodedUnits(text string) int64 {
	var n int64
	for _, r := range text {
		n += int64(utf16Len(r))
	}
	return n
}

// counted is what a counting scan of a text read: each token's TextUnits once its text is whole, Units at
// the scan's end, and how it ended.
type counted struct {
	text  []int64
	units int64
	err   error
}

// countedTake is how a counting scan takes a token's text before asking its TextUnits.
type countedTake struct {
	name string
	take func(s *jsonscan.Scanner, k jsonscan.Kind, p []byte) error
}

// readText reads the current token's text to its end through p.
func readText(s *jsonscan.Scanner, p []byte) error {
	for {
		_, err := s.Read(p)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case errors.Is(err, io.ErrShortBuffer):
			var wide [utf8.UTFMax]byte
			if _, err = s.Read(wide[:]); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		case err != nil:
			return err
		}
	}
}

// countedTakes is every way a counting scan takes a string: read whole, passed over by Skip, captured, and
// read a character then passed over; a name is always read, and a number read or passed over by Next.
var countedTakes = []countedTake{
	{"read", func(s *jsonscan.Scanner, k jsonscan.Kind, p []byte) error { return readText(s, p) }},
	{"skipped", func(s *jsonscan.Scanner, k jsonscan.Kind, p []byte) error {
		if k == jsonscan.String {
			return s.Skip()
		}
		return readText(s, p)
	}},
	{"captured", func(s *jsonscan.Scanner, k jsonscan.Kind, p []byte) error {
		if k == jsonscan.String {
			_, _, err := s.Capture(1 << 20)
			return err
		}
		return readText(s, p)
	}},
	{"read a character, then skipped", func(s *jsonscan.Scanner, k jsonscan.Kind, p []byte) error {
		if k != jsonscan.String {
			return readText(s, p)
		}
		var one [utf8.UTFMax]byte
		if _, err := s.Read(one[:]); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return s.Skip()
	}},
}

// countScan reads data as how hands it, counting from before its first token, each string taken as take
// takes it.
func countScan(data []byte, how reading, take countedTake) counted {
	s := jsonscan.NewSized(how.reader(bytes.NewReader(data)), how.buffer)
	s.CountUnits(true)
	p := make([]byte, max(how.chunk, 1))
	var out counted
	for {
		k, err := s.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				out.err = err
			}
			out.units = s.Units()
			return out
		}
		if hasText(k) {
			if err := take.take(s, k, p); err != nil {
				out.err = err
				return out
			}
		}
		out.text = append(out.text, s.TextUnits())
	}
}

// unitReadings is every way [countScan] is handed a text: the fuzz's [readings], the baseline, and buffers
// from the least up, so every character and escape is met cut by the buffer's edge.
func unitReadings() []reading {
	all := append([]reading{baseline}, readings...)
	for size := jsonscan.MinBufferSize; size < jsonscan.MinBufferSize+6; size++ {
		all = append(all, reading{name: fmt.Sprintf("a buffer of %d", size), buffer: size, chunk: 7, reader: asIs})
	}
	return all
}

// validUnitTexts is every text of [unitTexts] and of the fuzz's seeds that is one JSON value.
func validUnitTexts() []string {
	var texts []string
	for _, text := range append(unitTexts(), seeds...) {
		if jsontext.Value(text).IsValid(threadDecoding) {
			texts = append(texts, text)
		}
	}
	return texts
}

// UNITS COUNT A TEXT AS WRITTEN: at a scan's end, Units is the text's UTF-16 length as utf8.DecodeRune reads
// its bytes, a character past the basic plane two units, an escape its bytes, each byte that begins no
// character one, every blank, comma and colon among them, however its strings are taken (read, skipped,
// captured, read in part and skipped) and wherever the buffer's edge cuts a character; and so for the whole
// value skipped or captured at its first token.
//
// Mutation: count a 4-byte character one unit, drop wide in readString, read a character cut by the buffer's
// edge as invalid bytes, or count while off, and this fails naming the text.
func TestUnitsCountATextAsWritten(t *testing.T) {
	t.Parallel()
	for _, text := range validUnitTexts() {
		want := writtenUnits([]byte(text))
		for _, how := range unitReadings() {
			for _, take := range countedTakes {
				if got := countScan([]byte(text), how, take); got.err != nil || got.units != want {
					t.Fatalf("%q, %s, its strings %s: Units %d (%v), want %d", text, how.name, take.name, got.units,
						got.err, want)
				}
			}
			for _, whole := range []string{"Skip", "Capture"} {
				s := jsonscan.NewSized(how.reader(bytes.NewReader([]byte(text))), how.buffer)
				s.CountUnits(true)
				_, err := s.Next()
				if err == nil && whole == "Skip" {
					err = s.Skip()
				} else if err == nil {
					_, _, err = s.Capture(len(text))
				}
				if err == nil {
					_, err = s.Next()
				}
				if !errors.Is(err, io.EOF) || s.Units() != want {
					t.Fatalf("%q, %s, its value passed by %s: Units %d (%v), want %d", text, how.name, whole,
						s.Units(), err, want)
				}
			}
		}
	}
}

// TEXT UNITS COUNT A DECODED TEXT: once a Name's or a String's text is whole, TextUnits is the UTF-16 length
// of the text jsontext decodes, an escape its character's units (a pair two, a lone half one), an invalid
// byte one, however it is taken; and zero at every other token, a number read or not, a literal and a
// container.
//
// Mutation: count an escape as its bytes, a pair as one, or leave textUnits standing at the next token, or
// zero it at a String's alone, and this fails naming the text.
func TestTextUnitsCountADecodedText(t *testing.T) {
	t.Parallel()
	for _, text := range validUnitTexts() {
		tokens, _ := textTokens(t, []byte(text))
		want := make([]int64, len(tokens))
		for i, tok := range tokens {
			if tok.Kind == jsonscan.Name || tok.Kind == jsonscan.String {
				want[i] = decodedUnits(tok.Text)
			}
		}
		for _, how := range unitReadings() {
			for _, take := range countedTakes {
				if got := countScan([]byte(text), how, take); got.err != nil || !slices.Equal(got.text, want) {
					t.Fatalf("%q, %s, its strings %s: TextUnits %v (%v), want %v", text, how.name, take.name,
						got.text, got.err, want)
				}
			}
		}
	}
}

// UNITS COUNT ONLY WHILE ON, and keep their count while off: a value's length is the difference across it,
// whatever was counted before; and a scan never counted answers zero.
func TestUnitsCountOnlyWhileOn(t *testing.T) {
	t.Parallel()
	text := `{"a":"é😀x","b":[1,"\u00e9"],"c":true}`
	s := jsonscan.New(strings.NewReader(text))
	for range 3 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	// At "é😀x": its quote taken; on for its value, off at the next name.
	if s.Units() != 0 || s.TextUnits() != 0 {
		t.Fatalf("a scan never counted answers %d units and %d text units, want none", s.Units(), s.TextUnits())
	}
	s.CountUnits(true)
	if err := s.Skip(); err != nil {
		t.Fatal(err)
	}
	if got := s.Units(); got != 5 || s.TextUnits() != 4 {
		t.Fatalf(`the rest of "é😀x" counts %d units, %d of text; want 5 and 4`, got, s.TextUnits())
	}
	s.CountUnits(false)
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	if got := s.Units(); got != 5 {
		t.Fatalf("a name passed while off counts: %d units, want 5", got)
	}
	k, err := s.Next()
	if err != nil || k != jsonscan.ArrayStart {
		t.Fatal(k, err)
	}
	before := s.Units()
	s.CountUnits(true)
	if err := s.Skip(); err != nil {
		t.Fatal(err)
	}
	if got := s.Units() - before; got != int64(len(`1,"\u00e9"]`)) {
		t.Fatalf(`the rest of [1,"\u00e9"] counts %d units, want %d`, got, len(`1,"\u00e9"]`))
	}
	s.Reset(strings.NewReader(text))
	if s.Units() != 0 || s.TextUnits() != 0 {
		t.Fatalf("a reset scanner answers %d units and %d text units, want none", s.Units(), s.TextUnits())
	}
	// Off, a string passed over whole counts nothing of its text either.
	for range 3 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Skip(); err != nil || s.Units() != 0 || s.TextUnits() != 0 {
		t.Fatalf("a string skipped while off counts %d units and %d text units (%v), want none", s.Units(),
			s.TextUnits(), err)
	}
}

// failing hands text and then fails with err: with its last bytes where with says so, else on the read
// after them.
func failing(text []byte, err error, with bool) io.Reader {
	if with {
		return &failingWith{text: text, err: err}
	}
	return io.MultiReader(bytes.NewReader(text), iotest.ErrReader(err))
}

// failingWith hands text as a reader asks, and err with its last bytes.
type failingWith struct {
	text []byte
	err  error
}

func (r *failingWith) Read(p []byte) (int, error) {
	n := copy(p, r.text)
	if r.text = r.text[n:]; len(r.text) == 0 {
		return n, r.err
	}
	return n, nil
}

// COUNTING CHANGES NO TOKEN: a scan counting reads every text of the fuzz's seeds and of [unitTexts] as one
// not counting reads it, every token, text and offset, ending alike, a fault included, under every reading;
// so do Skip and Capture of the whole value; and a reader that fails at any byte, its error handed with the
// bytes before it or after them, ends both scans with the same error, at the same token.
//
// Mutation: hold back a character the reader's failure leaves cut past the bytes that settle it, or answer
// the reader's error before the bytes handed with it, and this fails naming the text.
func TestCountingChangesNoToken(t *testing.T) {
	t.Parallel()
	fault := &readerFault{"the reader broke"}
	// A character's first byte ends the least buffer, and the reader fails
	// with the bytes that settle it, a fault among them: the fault is the
	// answer, counting or not.
	settled := "[\"" + strings.Repeat("a", jsonscan.MinBufferSize-3) + "\xe2\"x"
	for _, whole := range []bool{false, true} {
		off := wholeScan(failing([]byte(settled), fault, true), whole, false)
		on := wholeScan(failing([]byte(settled), fault, true), whole, true)
		var e *jsonscan.Error
		if !sameEnd(on, off) || !errors.As(off, &e) {
			t.Fatalf("%q, its reader failing with the bytes that settle a cut character, whole %t: counting ends "+
				"%v, not counting %v; want both the fault", settled, whole, on, off)
		}
	}
	for _, text := range append(unitTexts(), seeds...) {
		data := []byte(text)
		for _, how := range unitReadings() {
			want := scan(t, data, how)
			got := countingScan(t, data, how)
			if !slices.Equal(got.tokens, want.tokens) || !slices.Equal(got.offsets, want.offsets) ||
				!sameEnd(got.err, want.err) {
				t.Fatalf("%q, %s, counting reads as %s at %v; not counting, %s at %v", data, how.name, describe(got),
					got.offsets, describe(want), want.offsets)
			}
		}
		if len(data) > 512 {
			// The texts at the depth bound: their every byte is a bracket, met above.
			continue
		}
		for at := range len(data) + 1 {
			for _, with := range []bool{false, true} {
				for _, whole := range []bool{false, true} {
					off := wholeScan(failing(data[:at], fault, with), whole, false)
					on := wholeScan(failing(data[:at], fault, with), whole, true)
					if !sameEnd(on, off) {
						t.Fatalf("%q failing after byte %d (with the bytes %t), whole %t: counting ends %v, not "+
							"counting %v", data, at, with, whole, on, off)
					}
				}
			}
		}
	}
}

// countingScan is [scan] counting from before the first token.
func countingScan(t *testing.T, data []byte, how reading) scanned {
	t.Helper()
	s := jsonscan.NewSized(how.reader(bytes.NewReader(data)), how.buffer)
	s.CountUnits(true)
	var out scanned
	p := make([]byte, how.chunk)
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
		if hasText(k) {
			text, readErr := readAll(t, s, p)
			if readErr != nil {
				out.err = readErr
				return out
			}
			tok.Text = text
		}
		tok.Invalid = s.InvalidUTF8()
		out.tokens = append(out.tokens, tok)
		out.offsets = append(out.offsets, at)
	}
}

// wholeScan scans r with the least buffer, counting or not: its value skipped whole where whole, else every
// token passed by Next; and answers how the scan ended, nil at io.EOF.
func wholeScan(r io.Reader, whole, counting bool) error {
	s := jsonscan.NewSized(r, jsonscan.MinBufferSize)
	s.CountUnits(counting)
	_, err := s.Next()
	if err == nil && whole {
		err = s.Skip()
	}
	for err == nil {
		_, err = s.Next()
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// FuzzUnits holds Units, TextUnits and every token to the claims above for any text, read with a buffer
// its seed draws.
func FuzzUnits(f *testing.F) {
	for i, seed := range append(seeds, unitTexts()[:40]...) {
		f.Add([]byte(seed), uint8(i))
	}
	f.Fuzz(func(t *testing.T, data []byte, size uint8) {
		how := reading{name: "sized", buffer: jsonscan.MinBufferSize + int(size%64), chunk: 1 + int(size%9),
			reader: iotest.HalfReader}
		want := scan(t, data, how)
		got := countingScan(t, data, how)
		if !slices.Equal(got.tokens, want.tokens) || !slices.Equal(got.offsets, want.offsets) ||
			!sameEnd(got.err, want.err) {
			t.Fatalf("%q counting reads as %s; not counting, %s", data, describe(got), describe(want))
		}
		if !jsontext.Value(data).IsValid(threadDecoding) {
			return
		}
		tokens, _ := textTokens(t, data)
		for _, take := range countedTakes {
			c := countScan(data, how, take)
			if c.err != nil || c.units != writtenUnits(data) || len(c.text) != len(tokens) {
				t.Fatalf("%q, its strings %s: Units %d (%v) of %d tokens, want %d of %d", data, take.name, c.units,
					c.err, len(c.text), writtenUnits(data), len(tokens))
			}
			for i, tok := range tokens {
				want := int64(0)
				if tok.Kind == jsonscan.Name || tok.Kind == jsonscan.String {
					want = decodedUnits(tok.Text)
				}
				if c.text[i] != want {
					t.Fatalf("%q, its strings %s: token %d's TextUnits %d, want %d", data, take.name, i, c.text[i],
						want)
				}
			}
		}
	})
}
