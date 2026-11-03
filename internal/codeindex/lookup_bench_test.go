package codeindex

import (
	"regexp"
	"testing"
)

// BenchmarkLookup times what a search's lookup costs a segment's posting
// lists, the evidence for lookup.go's head: over one segment of 65 536 blobs,
// in which 99 of every 100 hold "handle" and "request" apart and the hundredth
// one word of four holding both, joined — so the lists of handle's and
// request's trigrams name every blob, and the joins' lists one in a hundred:
//
//   - decode: one list of 65 536 ordinals decoded whole (Segment.decode), the
//     loop every posting a merge reads and a lookup's least list go through;
//   - literal: the candidates of handlerequest, a search's commonest shape,
//     one literal: its least list decoded, each list of every blob narrowing
//     it read to the end, since blobs sit in id order and what it keeps runs
//     to the segment's last;
//   - alternation: the candidates of handle(request|response|record|reply),
//     four literals sharing handle's trigrams, read once for all four.
//
// It reports allocations beside time; run it bounded, as its figures were:
// -run '^$' -bench '^BenchmarkLookup$' -benchtime 200x -count 1.
func BenchmarkLookup(b *testing.B) {
	const blobs = 65536
	words := []string{"handlerequest", "handleresponse", "handlerecord", "handlereply"}
	texts := make([][]byte, blobs)
	for i := range texts {
		if i%100 == 0 {
			texts[i] = []byte("x := " + words[(i/100)%len(words)] + "\n")
		} else {
			texts[i] = []byte("x := handle(request)\n")
		}
	}
	s := segmentOf(b, texts, 1, 1)
	list, ok := s.lookup(Trigrams([]byte("han"))[0])
	if !ok {
		b.Fatal("no blob holds han: the segment is not the one the benchmark claims")
	}
	if got, err := s.decode(list, nil); err != nil || len(got) != blobs {
		b.Fatalf("han's list names %d of the %d blobs (%v): the segment is not the one the benchmark claims",
			len(got), blobs, err)
	}
	b.Run("decode", func(b *testing.B) {
		b.ReportAllocs()
		dst := make([]uint32, 0, blobs)
		for b.Loop() {
			var err error
			if dst, err = s.decode(list, dst[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, q := range []struct{ name, pattern string }{
		{"literal", "handlerequest"},
		{"alternation", "handle(request|response|record|reply)"},
	} {
		plan, err := NewPlan(regexp.MustCompile(q.pattern))
		if err != nil {
			b.Fatal(err)
		}
		got, err := plan.Candidates(s)
		if err != nil || len(got) == 0 {
			b.Fatalf("%s admits %d blobs (%v): the segment is not the one the benchmark claims", q.pattern,
				len(got), err)
		}
		b.Run(q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := plan.Candidates(s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
