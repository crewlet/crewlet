package metrics

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// EACH UNIT'S BOUNDARIES RESOLVE A VALUE TO WITHIN A FACTOR OF TWO AND A HALF,
// ascending, over the range the unit's doc states: the millisecond and count
// sets the 1-2-5 series from their floors to their tops, the byte set every
// power of two from a kibibyte to two gibibytes. Bounds hands out a copy, so a
// caller that writes into it changes no histogram.
//
// Mutation: put the twenty-one powers of two back as one set for every unit,
// or let Bounds hand out the package's own slice, and this fails.
func TestEachUnitCountsIntoItsOwnBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		unit          string
		first, last   float64
		round, binary bool
	}{
		{UnitMilliseconds, 0.01, 5e7, true, false},
		{UnitCount, 1, 5e5, true, false},
		{UnitBytes, 1 << 10, 1 << 31, false, true},
	} {
		bounds, ok := Bounds(tc.unit)
		if !ok || len(bounds) == 0 {
			t.Errorf("%q has no boundaries", tc.unit)
			continue
		}
		if bounds[0] != tc.first || bounds[len(bounds)-1] != tc.last {
			t.Errorf("%q runs %v to %v, want %v to %v", tc.unit, bounds[0], bounds[len(bounds)-1], tc.first,
				tc.last)
		}
		for i := 1; i < len(bounds); i++ {
			if ratio := bounds[i] / bounds[i-1]; ratio <= 1 || ratio > 2.5 {
				t.Errorf("%q steps from %v to %v, a factor of %v: want ascending, within a factor of two and "+
					"a half", tc.unit, bounds[i-1], bounds[i], ratio)
			}
		}
		for _, b := range bounds {
			mantissa := b / math.Pow10(int(math.Floor(math.Log10(b)+1e-9)))
			isRound := slices.ContainsFunc([]float64{1, 2, 5}, func(m float64) bool {
				return math.Abs(mantissa-m) < 1e-9
			})
			isBinary := b == math.Exp2(math.Round(math.Log2(b)))
			if tc.round && !isRound || tc.binary && !isBinary {
				t.Errorf("%q has a boundary at %v, which is not the series its doc states", tc.unit, b)
			}
		}
		bounds[0] = -1
		if again, _ := Bounds(tc.unit); again[0] != tc.first {
			t.Errorf("writing into %q's boundaries changed the next caller's: Bounds must hand out a copy",
				tc.unit)
		}
	}
	if _, ok := Bounds(UnitSeconds); ok {
		t.Errorf("%q has boundaries, though no histogram here is recorded in it", UnitSeconds)
	}
	// The metrics reference says what the sets are, in the same figures.
	blurb := sectionBlurb(KindHistogram)
	for _, want := range []string{"from 10 µs to 5·10⁷ ms", "the same series from 1 to 500 000",
		"every power of two from 1 KiB to 2 GiB"} {
		if !strings.Contains(blurb, want) {
			t.Errorf("the metrics reference's histogram section does not say %q: %s", want, blurb)
		}
	}
}

// A PERCENTILE IS READ AGAINST ITS OWN UNIT'S BOUNDARIES, from the cumulative
// series and from the rolling window alike: a push's 100 MiB bundle reads as
// the 128 MiB boundary above it rather than as the overflow of a set that
// ended at 65 536, a five-hour backup as the 2·10⁷ ms above it, and a 300 ms
// scan as the 500 ms boundary — the budget of the alarm that reads it, which
// it is therefore not past.
//
// Mutation: count every histogram into one set, or read a snapshot's quantile
// against any boundaries but its own, and this fails.
func TestAPercentileIsReadAgainstItsUnitsBoundaries(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.ObserveValue(CodePushBytes, 100<<20, nil)
	r.Observe(BackupDuration, 5*time.Hour, nil)
	r.Observe(TrackerSearchScanDuration, 300*time.Millisecond, Attrs{"path": "prefetch", "rung": "hybrid"})
	r.ObserveValue(CodeCommitBatchSize, 64, nil)
	want := map[string]float64{
		CodePushBytes:             128 << 20,
		BackupDuration:            2e7,
		TrackerSearchScanDuration: 500,
		CodeCommitBatchSize:       100,
	}
	for reader, read := range map[string]func() []Snapshot{"cumulative": r.Read, "windowed": r.ReadWindow} {
		seen := 0
		for _, s := range read() {
			w, ok := want[s.Name]
			if !ok {
				continue
			}
			seen++
			if got := s.Quantile(0.95); got != w {
				t.Errorf("%s p95 of %s = %v %s, want the boundary above the observation, %v", reader, s.Name,
					got, s.Unit, w)
			}
			if b, _ := Bounds(s.Unit); !slices.Equal(s.Bounds, b) {
				t.Errorf("%s snapshot of %s is against %v, want its unit's boundaries", reader, s.Name, s.Bounds)
			}
		}
		if seen != len(want) {
			t.Errorf("%s reading held %d of the %d histograms observed", reader, seen, len(want))
		}
	}
}
