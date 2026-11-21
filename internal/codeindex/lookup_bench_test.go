package codeindex

import (
	"regexp"
	"testing"
)

// BenchmarkLookup is the evidence for lookup.go's head: 65 536 blobs, 99 in 100
// holding "handle" and "request" apart, the hundredth one of four words holding
// both joined, so handle's and request's lists name every blob. decode: one
// list decoded whole (Segment.decode); literal: candidates of handlerequest,
// each narrowing list read to its end since blobs sit in id order; alternation:
// handle(request|response|record|reply), four literals reading handle's lists
// once. Run: -run '^$' -bench '^BenchmarkLookup$' -benchtime 200x -count 1.
// Reference (two E-cores): literal 2.5 ms, 7.6 KB vs 3.9 ms, 10.6 MB per
// literal; alternation 2.6 ms, 27 KB vs 9.4 ms, 24.7 MB.
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
