package jsonscan_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// teeTexts hold a value of every kind at every depth: escapes, a pair, invalid UTF-8 and blanks inside
// values as written, a string and a number longer than the least buffer, and a number the text ends on.
var teeTexts = []string{
	`{"a" : [1, "x"], "s":"s\u00e9\" \ud83d\ude00", "n":-1.5e+3, "t":true, "f":false, "z":null, "e":[ ],` +
		` "o":{}, "d":[[["deep"]],{"k":{"l":null}}]}`,
	` [ "\u00e9\ud800x` + "\xff\xc3" + `", 0 , {"\u0061" : "caf` + "\xc3\xa9" + `"}, ` +
		`"` + strings.Repeat("0123456789", 6) + `", 12345678901234567890.5e-10 ] `,
	`-12345678901234567890.5e-10`,
	`"` + strings.Repeat(`ab\n\u20ac`, 9) + `"`,
	`true`,
}

// valueAt is a value of a text as Capture answers it, and what the scan answers after it: what a tee of the
// value writes, and where the scan stands once the tee has ended.
type valueAt struct {
	kind   jsonscan.Kind
	raw    string
	at     int64
	next   jsonscan.Kind
	nextAt int64
}

// beginsAValue reports whether a token of kind k is a value's first.
func beginsAValue(k jsonscan.Kind) bool {
	return k != jsonscan.Name && k != jsonscan.ObjectEnd && k != jsonscan.ArrayEnd
}

// toValue moves s to the first token of the text's i-th value, in the order values begin.
func toValue(s *jsonscan.Scanner, i int) (jsonscan.Kind, error) {
	for {
		k, err := s.Next()
		if err != nil {
			return k, err
		}
		if beginsAValue(k) {
			if i == 0 {
				return k, nil
			}
			i--
		}
	}
}

// valuesOf is every value of text, captured whole by a scan at the baseline.
func valuesOf(t *testing.T, text string) []valueAt {
	t.Helper()
	var out []valueAt
	for i := 0; ; i++ {
		s := jsonscan.New(strings.NewReader(text))
		k, err := toValue(s, i)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading %q to its value %d: %v", text, i, err)
		}
		v := valueAt{kind: k, at: s.Offset()}
		raw, whole, err := s.Capture(len(text))
		if err != nil || !whole {
			t.Fatalf("capturing the value at byte %d of %q: whole %v, %v", v.at, text, whole, err)
		}
		v.raw = string(raw)
		if v.next, err = s.Next(); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("after the value at byte %d of %q: %v", v.at, text, err)
		}
		v.nextAt = s.Offset()
		out = append(out, v)
	}
}

// takes are the ways a teed value is taken: its tokens read, every text read through a p of utf8.UTFMax
// bytes; read in part, every other text read and the rest passed by Next; passed by Next alone; skipped;
// and captured.
var takes = []string{"read", "read in part", "passed by Next", "skipped", "captured"}

// take takes the value whose first token, of kind k, is current, as take says, up to its last token, and
// answers the first error a call answers. A scalar passed by Next is left to the Next after it.
func take(s *jsonscan.Scanner, k jsonscan.Kind, take string) error {
	switch take {
	case "skipped":
		return s.Skip()
	case "captured":
		_, _, err := s.Capture(1 << 20)
		return err
	}
	whole := s.Depth()
	if k == jsonscan.ObjectStart || k == jsonscan.ArrayStart {
		whole--
	}
	p := make([]byte, 4)
	for i := 0; ; i++ {
		if hasText(k) && (take == "read" || take == "read in part" && i%2 == 0) {
			for {
				_, err := s.Read(p)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
			}
		}
		if s.Depth() == whole && k != jsonscan.Name {
			return nil
		}
		var err error
		if k, err = s.Next(); err != nil {
			return err
		}
	}
}

// teeReadings move the buffer's edge through every byte of the texts: every buffer from the least to a
// byte past the longest escape's reach, one fed a byte at a time, and New's own.
func teeReadings() []reading {
	out := []reading{
		{name: "a byte at a time", buffer: jsonscan.MinBufferSize, chunk: 64 << 10, reader: iotest.OneByteReader},
		baseline,
	}
	for buffer := jsonscan.MinBufferSize; buffer <= 2*jsonscan.MinBufferSize; buffer++ {
		out = append(out, reading{name: "a small buffer", buffer: buffer, chunk: 64 << 10, reader: asIs})
	}
	return out
}

// A TEE WRITES THE VALUE AS WRITTEN, AND NOTHING ELSE: whatever the value, wherever the read buffer's edge
// falls in it, and however it is taken (read, read in part, passed by Next, skipped or captured), the bytes
// its writer takes are the value's as Capture answers them, from its first byte to its last, and the scan
// goes on after it as a scan without the tee. Only a value whose first byte is at hand is teed, and one tee
// writes at a time.
//
// Mutation: write nothing as the buffer slides, write what was read past pos, or end a container's tee at
// its first inner value, and this fails naming the value.
func TestATeeWritesTheValueAsWritten(t *testing.T) {
	t.Parallel()
	for _, text := range teeTexts {
		for i, v := range valuesOf(t, text) {
			for _, how := range teeReadings() {
				for _, how2 := range takes {
					s := jsonscan.NewSized(how.reader(strings.NewReader(text)), how.buffer)
					k, err := toValue(s, i)
					if err != nil {
						t.Fatal(err)
					}
					var w bytes.Buffer
					if err = s.Tee(&w); err != nil {
						t.Fatalf("teeing the %s at byte %d of %q: %v", k, v.at, text, err)
					}
					if err = take(s, k, how2); err != nil {
						t.Fatalf("%s, the %s at byte %d of %q %s: %v", how.name, k, v.at, text, how2, err)
					}
					next, err := s.Next()
					if err != nil && !errors.Is(err, io.EOF) {
						t.Fatalf("after the teed value at byte %d of %q: %v", v.at, text, err)
					}
					if w.String() != v.raw || next != v.next || s.Offset() != v.nextAt {
						t.Fatalf("%s (%d-byte buffer), the %s at byte %d of %q %s tees %q and then reads %q at %d;"+
							" want %q, then %q at %d", how.name, how.buffer, k, v.at, text, how2, w.String(), next,
							s.Offset(), v.raw, v.next, v.nextAt)
					}
				}
			}
		}
	}

	// Nowhere to tee: before the first token, at a name, at a container's end, once a Read has begun a
	// value; and a second tee inside the first.
	s := jsonscan.New(strings.NewReader(`{"a":["b", "cd"], "e":[]}`))
	if err := s.Tee(io.Discard); !errors.Is(err, jsonscan.ErrNoValue) {
		t.Fatalf("Tee before the first token answers %v", err)
	}
	for i, step := range []struct {
		read int
		want error
	}{
		{0, nil}, {0, jsonscan.ErrNoValue}, {0, jsonscan.ErrTeeing}, {1, jsonscan.ErrNoValue},
		{0, jsonscan.ErrTeeing}, {0, jsonscan.ErrNoValue}, {0, jsonscan.ErrNoValue},
	} {
		k, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		if step.read > 0 {
			if _, err := s.Read(make([]byte, step.read)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Tee(io.Discard); !errors.Is(err, step.want) {
			t.Fatalf("step %d: Tee at the %s at byte %d answers %v, want %v", i, k, s.Offset(), err, step.want)
		}
	}
}

// failingWriter takes the first after bytes it is handed, then answers err, and counts the writes it is
// handed once it has.
type failingWriter struct {
	after  int
	err    error
	took   bytes.Buffer
	failed bool
	late   int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.failed {
		w.late++
		return 0, w.err
	}
	room := w.after - w.took.Len()
	if len(p) <= room {
		return w.took.Write(p)
	}
	w.took.Write(p[:room])
	w.failed = true
	return room, w.err
}

// shortWriter takes all but a byte of each write and says nothing failed.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(len(p)-1, 0), nil }

// A TEE WHOSE WRITER FAILS ENDS ITS WRITING, NEVER THE SCAN: wherever the writer fails, it is handed nothing
// more; the call that ends the value, and no other, answers a TeeError that reaches the writer's error and
// says where the value begins; and the scan goes on after the value as a scan without the tee, to the
// text's end. A fault inside the value past the failure is still met, and ends the scan. A write that takes
// less than it is handed and says nothing is a short write.
//
// Mutation: let the writer's error end the scan, answer it where the writer fails, or write on past it, and
// this fails naming the value and where its writer failed.
func TestATeeWhoseWriterFailsReadsOn(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	least := reading{name: "the least buffer", buffer: jsonscan.MinBufferSize, chunk: 64 << 10, reader: asIs}
	for _, text := range teeTexts {
		for i, v := range valuesOf(t, text) {
			for after := range len(v.raw) {
				for _, how := range []reading{least, baseline} {
					for _, how2 := range takes {
						s := jsonscan.NewSized(how.reader(strings.NewReader(text)), how.buffer)
						k, err := toValue(s, i)
						if err != nil {
							t.Fatal(err)
						}
						w := &failingWriter{after: after, err: refused}
						// The call that ends the value: Tee itself at a literal, a call of the take, or the
						// Next past a scalar the take leaves to it.
						ended := s.Tee(w)
						if ended == nil {
							ended = take(s, k, how2)
						}
						next, err := s.Next()
						if ended == nil && err != nil && !errors.Is(err, io.EOF) {
							ended = err
							next, err = s.Next()
						}
						var tee *jsonscan.TeeError
						switch {
						case err != nil && !errors.Is(err, io.EOF):
							t.Fatalf("%s, after the %s at byte %d of %q %s, its writer failing at %d, the scan "+
								"answers %v", how.name, k, v.at, text, how2, after, err)
						case !errors.As(ended, &tee) || !errors.Is(ended, refused) || tee.Offset != v.at:
							t.Fatalf("%s, the %s at byte %d of %q %s, its writer failing at %d, ends with %v",
								how.name, k, v.at, text, how2, after, ended)
						case w.late > 0 || w.took.String() != v.raw[:after]:
							t.Fatalf("%s, the %s at byte %d of %q %s: its writer took %q and was handed %d writes"+
								" after failing at %d", how.name, k, v.at, text, how2, w.took.String(), w.late, after)
						case next != v.next || s.Offset() != v.nextAt:
							t.Fatalf("%s, after the %s at byte %d of %q %s, its writer failing at %d, the scan "+
								"reads %q at %d, want %q at %d", how.name, k, v.at, text, how2, after, next,
								s.Offset(), v.next, v.nextAt)
						}
						if err := readToEnd(t, s); err != nil {
							t.Fatalf("reading %q on past the %s at byte %d: %v", text, k, v.at, err)
						}
					}
				}
			}
		}
	}

	// A fault inside the value, past where its writer failed, ends the scan.
	s := jsonscan.NewSized(strings.NewReader(`[{"a":"xxxxxxxx","b":x}]`), jsonscan.MinBufferSize)
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Tee(&failingWriter{after: 2, err: refused}); err != nil {
		t.Fatal(err)
	}
	var e *jsonscan.Error
	if err := s.Skip(); !errors.As(err, &e) || e.Fault != jsonscan.FaultByte || e.Offset != 21 {
		t.Fatalf("passing a teed value with a refused byte past its writer's failure answers %v", err)
	}
	if _, err := s.Next(); !errors.As(err, &e) || e.Offset != 21 {
		t.Fatalf("after the fault, Next answers %v, want the fault again", err)
	}

	// A short write.
	s = jsonscan.New(strings.NewReader(`["abc", 1]`))
	for range 2 {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Tee(shortWriter{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Skip(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("a tee whose writer takes less and says nothing answers %v, want a short write", err)
	}
}
