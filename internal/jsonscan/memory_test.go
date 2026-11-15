package jsonscan_test

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// segment is a stretch of a spelled text: text once, or pattern repeated to n bytes.
type segment struct {
	text    string
	pattern string
	n       int
}

// spelled is a text read as it is spelled out and held nowhere: each segment in turn, a pattern copied out
// of one block of it, made before the reading begins.
type spelled struct {
	segments []segment
	blocks   [][]byte
	i, off   int
}

func spell(segments ...segment) *spelled {
	r := &spelled{segments: segments, blocks: make([][]byte, len(segments))}
	for i, seg := range segments {
		if seg.pattern != "" {
			r.blocks[i] = bytes.Repeat([]byte(seg.pattern), (64<<10)/len(seg.pattern))
		}
	}
	return r
}

func (r *spelled) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && r.i < len(r.segments) {
		seg, block := r.segments[r.i], r.blocks[r.i]
		var k int
		if block == nil {
			k = copy(p[n:], seg.text[r.off:])
		} else {
			// The block holds whole patterns, so the stream's byte r.off is the block's r.off % len(block).
			start := r.off % len(block)
			k = copy(p[n:], block[start:min(len(block), start+seg.n-r.off)])
		}
		n += k
		r.off += k
		if r.off == len(seg.text) && block == nil || block != nil && r.off == seg.n {
			r.i, r.off = r.i+1, 0
		}
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

// mib64 is the length the long values are spelled at: twice the 32 MiB a session's archive may hold, and
// past every bound a reader of one held a value to before this package.
const mib64 = 64 << 20

// A SCAN HOLDS ITS BUFFER AND NOTHING THAT GROWS WITH ITS INPUT, the figure the package doc states: a 64 MiB
// string, one of escapes and characters past ASCII, a 64 MiB name, a 64 MiB number and 64 MiB of blanks,
// each read in full through a 32 KiB p, and a value nested MaxDepth deep, each allocate no more than New's
// buffer and the scanner's few KiB of state, from New to io.EOF; so do a 64 MiB string passed over by Skip,
// and one captured past a 1 MiB bound, beside what growing its capture to the bound takes (at most twice
// the bound, by doubling, of which the bound at most is held). Measured as the bytes allocated in all,
// which a held value cannot pass under, however briefly it was held.
//
// Mutation: hold a string's text until its closing quote, keep a growing stack of containers, or let a
// capture grow past its bound, and this fails naming the input.
func TestAScanHoldsItsBufferWhateverTheInput(t *testing.T) {
	// Not parallel: runtime.MemStats counts every goroutine's allocations, and a parallel case's would be
	// counted as the scan's.
	readEvery := func(s *jsonscan.Scanner, p []byte) error {
		for {
			k, err := s.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if !hasText(k) {
				continue
			}
			for {
				if _, err = s.Read(p); errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
			}
		}
	}
	stateCeiling := uint64(jsonscan.ReadBufferSize + 4<<10)
	const bound = 1 << 20
	for _, c := range []struct {
		name    string
		text    func() io.Reader
		read    func(*jsonscan.Scanner, []byte) error
		ceiling uint64
	}{
		{"a 64 MiB string read in full", func() io.Reader {
			return spell(segment{text: `{"data":"`}, segment{pattern: "A", n: mib64}, segment{text: `"}`})
		}, readEvery, stateCeiling},
		{"a 64 MiB string of escapes and characters past ASCII read in full", func() io.Reader {
			// Ten bytes a pattern, spelled to a whole number of them so no escape is cut at the end.
			return spell(segment{text: `["`}, segment{pattern: "\u00e9\\n\\u20ac", n: mib64 - mib64%10},
				segment{text: `"]`})
		}, readEvery, stateCeiling},
		{"a 64 MiB name read in full", func() io.Reader {
			return spell(segment{text: `{"`}, segment{pattern: "n", n: mib64}, segment{text: `":1}`})
		}, readEvery, stateCeiling},
		{"a 64 MiB number read in full", func() io.Reader {
			return spell(segment{text: "-1"}, segment{pattern: "9", n: mib64}, segment{text: ".5e-7"})
		}, readEvery, stateCeiling},
		{"64 MiB of blanks around a value", func() io.Reader {
			return spell(segment{pattern: " \t\r\n", n: mib64 / 2}, segment{text: "1"},
				segment{pattern: "\n", n: mib64 / 2})
		}, readEvery, stateCeiling},
		{"a value nested MaxDepth deep", func() io.Reader {
			return spell(segment{pattern: `{"k":[`, n: 6 * jsonscan.MaxDepth / 2}, segment{text: "1"},
				segment{pattern: "]}", n: 2 * jsonscan.MaxDepth / 2})
		}, readEvery, stateCeiling},
		{"a 64 MiB string passed over by Skip", func() io.Reader {
			return spell(segment{text: `{"data":"`}, segment{pattern: "A", n: mib64}, segment{text: `"}`})
		}, func(s *jsonscan.Scanner, _ []byte) error {
			if _, err := s.Next(); err != nil {
				return err
			}
			if err := s.Skip(); err != nil {
				return err
			}
			_, err := s.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}, stateCeiling},
		{"a 64 MiB string captured past a 1 MiB bound", func() io.Reader {
			return spell(segment{text: `["`}, segment{pattern: "A", n: mib64}, segment{text: `"]`})
		}, func(s *jsonscan.Scanner, _ []byte) error {
			for range 2 {
				if _, err := s.Next(); err != nil {
					return err
				}
			}
			raw, whole, err := s.Capture(bound)
			if err != nil || whole || raw != nil {
				return errors.New("the capture past its bound answered a value")
			}
			_, err = s.Next()
			return err
		}, stateCeiling + 2*bound},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.text()
			p := make([]byte, 32<<10)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err := c.read(jsonscan.New(r), p)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatalf("reading it: %v", err)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated %d bytes", allocated)
			if allocated > c.ceiling {
				t.Fatalf("the scan allocated %d bytes, past the %d its doc states", allocated, c.ceiling)
			}
		})
	}
}
