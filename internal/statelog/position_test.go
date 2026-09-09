package statelog_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A POSITION IS A TRIPLE AND ORDERS BY GENERATION FIRST.
//
// The whole point of the generation is that a sequence from before a reanchor
// is comparable and SAFELY STALE rather than plausible: on the rebuilt stream
// it is a number in a space it does not belong to. An ordering that compared
// sequences first would rank a large old sequence above a small current one
// and hand a writer an expectation the broker will refuse for ever.
func TestAPositionOrdersByGenerationBeforeSequence(t *testing.T) {
	t.Parallel()
	const s = "CREWLET_PROBE_LOG"
	for name, tc := range map[string]struct {
		a, b statelog.Position
		want bool
	}{
		"a huge old sequence is below a small new one": {
			a:    statelog.Position{Stream: s, Generation: 0, Seq: 4_000_000},
			b:    statelog.Position{Stream: s, Generation: 1, Seq: 1},
			want: true,
		},
		"and never the other way round": {
			a:    statelog.Position{Stream: s, Generation: 1, Seq: 1},
			b:    statelog.Position{Stream: s, Generation: 0, Seq: 4_000_000},
			want: false,
		},
		"within one generation it is the sequence": {
			a:    statelog.Position{Stream: s, Generation: 3, Seq: 10},
			b:    statelog.Position{Stream: s, Generation: 3, Seq: 11},
			want: true,
		},
		"equal is not before": {
			a:    statelog.Position{Stream: s, Generation: 3, Seq: 10},
			b:    statelog.Position{Stream: s, Generation: 3, Seq: 10},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.a.Before(tc.b)
			if err != nil {
				t.Fatalf("Before: %v", err)
			}
			if got != tc.want {
				t.Errorf("%s.Before(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// The packed form must give the SAME order for free, since
			// it is what every durable column is compared with and
			// nothing in SQL knows a generation exists.
			if packed := tc.a.Packed() < tc.b.Packed(); packed != tc.want {
				t.Errorf("packed order = %v, want %v — the packed form and "+
					"Before must not be able to disagree", packed, tc.want)
			}
		})
	}
}

// TWO STREAMS ARE NOT COMPARABLE, and that is an ERROR rather than a false.
//
// "This position is not before that one" and "these two are not comparable at
// all" lead to different code, and a bool collapses the second into the
// first — which every caller reads as "already caught up".
func TestComparingAcrossStreamsIsAnErrorAndNotAFalse(t *testing.T) {
	t.Parallel()
	a := statelog.Position{Stream: "CREWLET_PROBE_LOG", Generation: 1, Seq: 1}
	b := statelog.Position{Stream: "CREWLET_OTHER_LOG", Generation: 1, Seq: 9}
	got, err := a.Before(b)
	if !errors.Is(err, statelog.ErrWrongStream) {
		t.Fatalf("Before across streams = (%v, %v), want ErrWrongStream", got, err)
	}
	if got {
		t.Error("a refused comparison must not also answer true")
	}
}

// THE PACKED FORM'S BOUNDS ARE CHECKED WHERE A POSITION IS MINTED.
//
// (generation << 40) inside an int64 leaves 2^23 generations and one
// generation's whole space for the sequence. A value past either would go
// negative in a durable column with nothing saying so, so the mint points
// refuse it instead — which is why Packed() itself can stay a plain
// conversion at every comparison in the tree.
func TestAPositionOutsideThePackedRangeIsRefusedWhereItIsMinted(t *testing.T) {
	t.Parallel()
	const s = "CREWLET_PROBE_LOG"
	for name, tc := range map[string]struct {
		p    statelog.Position
		want bool
	}{
		"the highest generation fits": {
			p:    statelog.Position{Stream: s, Generation: statelog.MaxGeneration, Seq: 1},
			want: true,
		},
		"one generation past it does not": {
			p:    statelog.Position{Stream: s, Generation: statelog.MaxGeneration + 1, Seq: 1},
			want: false,
		},
		"the highest sequence fits": {
			p:    statelog.Position{Stream: s, Generation: 1, Seq: statelog.MaxSeq},
			want: true,
		},
		"one sequence past it does not": {
			p:    statelog.Position{Stream: s, Generation: 1, Seq: statelog.MaxSeq + 1},
			want: false,
		},
		"and a position with no stream names no number space": {
			p:    statelog.Position{Generation: 1, Seq: 1},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.p.Valid()
			if tc.want && err != nil {
				t.Fatalf("Valid() = %v, want nil", err)
			}
			if !tc.want && err == nil {
				t.Fatalf("Valid() accepted %v", tc.p)
			}
			if !tc.want && err != nil && !errors.Is(err, statelog.ErrPositionRange) &&
				tc.p.Stream != "" {
				t.Errorf("error = %v, want ErrPositionRange", err)
			}
		})
	}

	// AND THE HIGHEST LEGAL POSITION IS STILL POSITIVE, which is the
	// property the whole split exists for: a packed position is compared
	// with a plain `>` in SQL, and a negative one would sort below every
	// row it is meant to be above.
	top := statelog.Position{Stream: s, Generation: statelog.MaxGeneration, Seq: statelog.MaxSeq}
	if top.Packed() <= 0 {
		t.Fatalf("the highest legal position packs to %d, which is not positive",
			top.Packed())
	}
}
