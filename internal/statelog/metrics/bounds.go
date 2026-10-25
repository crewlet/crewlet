package metrics

import (
	"math"
	"slices"
)

// THE BOUNDARIES A HISTOGRAM COUNTS INTO ARE ITS UNIT'S, fixed here per unit
// rather than declared per instrument or configured: a boundary set that
// differs between two nodes' copies of one instrument cannot be merged, and
// merging across a fleet is most of what these are for — while one set for
// every unit cannot serve all three this catalogue records in.
//
// That was the defect these replaced. TWENTY-ONE POWERS OF TWO from 64 µs to
// 64 s counted every histogram, and the set was wrong twice over:
//
//   - A PERCENTILE IS READ AT A BOUNDARY ([Snapshot.Quantile] answers the upper
//     boundary of the bucket it falls in), so an alarm holding a percentile to
//     a budget that is not a boundary is decided at the boundary above it.
//     `prefetch_slow` and `barrier_slow` (500 ms) fired for any p95 past
//     256 ms and said it was 512 ms; `search_slow` (1 s) past 512 ms, saying
//     1 024 ms; `pool_starved` (100 ms) past 64 ms, saying 128 ms. Every budget
//     an operator states is a round figure, and a 1-2-5 series holds every
//     round figure, so with it "the percentile is past the budget" and "the
//     boundary above it is" are one statement — the estimate is still a
//     bucket wide, but the alarm is exact. statelog's
//     TestEveryPercentileAlarmIsDecidedAtABoundary holds every budget to a
//     boundary.
//   - A BYTE COUNT IS NOT A DURATION. A push's bundle runs to 1.25 GiB
//     (code.MaxPackBytes), and against boundaries ending at 65 536 every push
//     past 64 KiB — every push worth measuring — landed in the overflow bucket,
//     as did every transfer, cold sync and backup longer than 64 s.
//
// Each set resolves a value to within a factor of two and a half at every
// scale its unit is recorded at, for an answer nobody acts on more precisely
// than "is this an order of magnitude worse than yesterday".

// durationBounds are the boundaries of a histogram in milliseconds: the 1-2-5
// series from 10 µs to 5·10⁷ ms (13 h 53 min).
//
// The FLOOR is below the fastest thing timed here, a 40 µs index probe. The
// TOP is past the longest: no transfer the engine paces outlives
// objstore.RecordWithin (12 h) — a push of the largest bundle at the pacing
// floor takes 10 h 40 min — and a backup's first copy of the large model was
// measured at about five hours. internal/code's
// TestEveryPushIsCountedBelowTheTopBoundary holds the first.
var durationBounds = oneTwoFive(-2, 7)

// countBounds are the boundaries of a histogram that counts: the 1-2-5 series
// from 1 to 5·10⁵ — a hundred times the most rows one apply transaction holds
// (statelog.ApplyTxRowBudget, 4 000, which one record may overshoot by its
// own rows), the largest count recorded here.
var countBounds = oneTwoFive(0, 5)

// byteBounds are the boundaries of a histogram in bytes: every power of two
// from 1 KiB to 2 GiB — a push's smallest bundle is over a kibibyte (its
// index alone is at least 1 072 bytes), and its largest is under 2 GiB,
// which no stored pack may reach (code.MaxPackBytes). Powers of two rather
// than 1-2-5 because the sizes a byte count is read against here are binary
// — the gibibyte push ceiling, the mebibyte parts a bundle moves in — and no
// alarm holds a byte percentile to a budget it would have to be exact at.
var byteBounds = func() []float64 {
	var out []float64
	for v := float64(1 << 10); v <= 1<<31; v *= 2 {
		out = append(out, v)
	}
	return out
}()

// oneTwoFive is 1, 2 and 5 times every power of ten from 10^from to 10^to,
// ascending: the boundaries every round figure falls on.
//
// Each is the float64 nearest the decimal it spells — a multiple divided by a
// power of ten for a negative exponent, never a power multiplied, so 0.05 is
// the 0.05 a parse of "0.05" gives and a budget converted to milliseconds
// compares equal to the boundary it names.
func oneTwoFive(from, to int) []float64 {
	var out []float64
	for e := from; e <= to; e++ {
		for _, m := range []float64{1, 2, 5} {
			if e < 0 {
				out = append(out, m/math.Pow10(-e))
			} else {
				out = append(out, m*math.Pow10(e))
			}
		}
	}
	return out
}

// Bounds is the boundaries a histogram in unit counts into, ascending and in
// that unit — what [Snapshot.Counts] are against and what an exporter
// registers for the instrument — and false for a unit no histogram may be
// declared in, which [Validate] refuses.
func Bounds(unit string) ([]float64, bool) {
	b := boundsOf(unit)
	return slices.Clone(b), b != nil
}

// boundsOf is Bounds without the copy, for the recorder's own write path: the
// sets are never written after init.
func boundsOf(unit string) []float64 {
	switch unit {
	case UnitMilliseconds:
		return durationBounds
	case UnitCount:
		return countBounds
	case UnitBytes:
		return byteBounds
	default:
		return nil
	}
}

// binFor is the index of the bucket v falls in against bounds: the first
// boundary at or above it, so a bucket holds the values above the boundary
// before it and up to its own, and one past the last boundary is the
// overflow.
func binFor(bounds []float64, v float64) int {
	i, _ := slices.BinarySearch(bounds, v)
	return i
}
