package statelog

import (
	"errors"
	"testing"
)

// THE NEXT SEQUENCE IS ONE PAST THE HIGHEST RECORD IN THE RUN, never one past
// its length.
//
// A run carries records outside its contiguous prefix on purpose: a redelivery
// at or below the checkpoint is passed through so the loop can acknowledge it,
// and a second copy of a record already in hand is passed through for the same
// reason. Counting either as a step of the prefix moves the expected sequence
// past a record that has not arrived, and the record above the hole is then
// admitted as contiguous — applied on every node that hit the same
// redelivery, with the checkpoint moving over a record nothing wrote.
func TestTheReorderBufferNeverAdmitsPastAHoleAfterARedelivery(t *testing.T) {
	t.Parallel()
	cursor := Position{Stream: "s", Generation: 1, Seq: 10}
	rec := func(seq uint64) Record {
		return Record{Position: Position{Stream: "s", Generation: 1, Seq: seq},
			Payload: []byte("x"), framed: []byte("x")}
	}
	seqs := func(rs []Record) []uint64 {
		out := make([]uint64, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.Position.Seq)
		}
		return out
	}
	cases := []struct {
		name  string
		first []Record // the first fetch, admitted into an empty run
		then  []Record // the second fetch, admitted into that run
		hole  uint64   // the sequence that has not arrived
	}{
		{
			name:  "a redelivery below the checkpoint sits in the run",
			first: []Record{rec(5), rec(11), rec(12)},
			then:  []Record{rec(14), rec(15)},
			hole:  13,
		},
		{
			name:  "a second copy of a record already in the run",
			first: []Record{rec(11), rec(11), rec(12)},
			then:  []Record{rec(14)},
			hole:  13,
		},
		{
			name:  "a fetch that delivered only a redelivery, appended last",
			first: []Record{rec(11), rec(12), rec(3)},
			then:  []Record{rec(14)},
			hole:  13,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b reorderBuffer
			var run []Record
			// The first fetch may arrive in more than one batch, which is
			// how a redelivery lands at the END of a run: each batch is
			// admitted into what the previous ones left.
			for _, r := range tc.first {
				ready, err := b.admit([]Record{r}, cursor, ReplayStrict, run)
				if err != nil {
					t.Fatal(err)
				}
				run = append(run, ready...)
			}
			ready, err := b.admit(tc.then, cursor, ReplayStrict, run)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range ready {
				if r.Position.Seq > tc.hole {
					t.Fatalf("run %v then %v admitted %v — sequence %d is missing "+
						"and %d was let past it", seqs(run), seqs(tc.then), seqs(ready),
						tc.hole, r.Position.Seq)
				}
			}
			// And the hole closing releases everything above it, in order.
			ready, err = b.admit([]Record{rec(tc.hole)}, cursor, ReplayStrict, run)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.hole
			for _, r := range ready {
				if r.Position.Seq != want {
					t.Fatalf("after the hole closed the buffer released %v, want a "+
						"contiguous run from %d", seqs(ready), tc.hole)
				}
				want++
			}
			if len(ready) == 0 {
				t.Fatalf("the hole at %d closed and nothing was released", tc.hole)
			}
		})
	}
}

// AN OVERFLOWING BUFFER IS A STOP, and it is measured against a hole that is
// real rather than one the arithmetic invented.
func TestTheReorderBufferStopsWhenAHoleWillNotClose(t *testing.T) {
	t.Parallel()
	cursor := Position{Stream: "s", Generation: 1, Seq: 0}
	var b reorderBuffer
	big := make([]byte, ReorderBufferBytes/2+1)
	above := func(seq uint64) Record {
		// THE FRAMED BYTES ARE WHAT THE BUFFER HOLDS, and what it is
		// measured in: [Record.Payload] is a subslice of them, so
		// counting both would double a number that has one allocation
		// behind it.
		return Record{
			Position: Position{Stream: "s", Generation: 1, Seq: seq},
			Payload:  big, framed: big,
		}
	}
	if _, err := b.admit([]Record{above(2)}, cursor, ReplayStrict, nil); err != nil {
		t.Fatalf("the first record above a hole fits: %v", err)
	}
	_, err := b.admit([]Record{above(3)}, cursor, ReplayStrict, nil)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("past the bound the buffer answered %v, want %v", err, ErrStopped)
	}
}
