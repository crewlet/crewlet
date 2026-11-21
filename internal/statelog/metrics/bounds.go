package metrics

import (
	"math"
	"slices"
)

// A histogram's boundaries are fixed per unit, never per instrument or by
// config: copies of one instrument on two nodes merge only when their sets
// match, and no one set serves durations, counts and bytes alike.
//   - A percentile reads as the upper boundary of its bucket
//     ([Snapshot.Quantile]), so an alarm whose budget is not a boundary is
//     decided at the one above it. Budgets are round figures and a 1-2-5
//     series holds every one, so the alarm is exact (statelog's
//     TestEveryPercentileAlarmIsDecidedAtABoundary).
//   - A byte count is not a duration: a push's bundle runs to 1.25 GiB
//     (gitlet.MaxPackBytes), a range no duration set spans.
// Each set resolves a value to within a factor of 2.5 across its unit's
// range: enough to tell an order of magnitude worse than yesterday.

// durationBounds are the boundaries of a histogram in milliseconds: the 1-2-5
// series from 10 µs to 5·10⁷ ms (13 h 53 min).
//
// The floor is below the fastest thing timed, a 40 µs index probe; the top is
// past the longest: no paced transfer outlives objstore.RecordWithin (12 h),
// the largest push at the pacing floor takes 10 h 40 min (internal/gitlet's
// TestEveryPushIsCountedBelowTheTopBoundary), and a backup's first copy about
// five hours.
var durationBounds = oneTwoFive(-2, 7)

// countBounds are the boundaries of a histogram that counts: the 1-2-5 series
// from 1 to 5·10⁵, a hundred times the largest count recorded, an apply
// transaction's rows (statelog.ApplyTxRowBudget, 4 000, which one record may
// overshoot).
var countBounds = oneTwoFive(0, 5)

// byteBounds are the boundaries of a histogram in bytes: every power of two
// from 1 KiB (a bundle's index alone is at least 1 072 bytes) to 2 GiB, past
// gitlet.MaxPackBytes. Binary, not 1-2-5: the sizes bytes are read against
// are binary (the push ceiling, a bundle's mebibyte parts) and no alarm holds
// a byte percentile to a budget.
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
// A negative exponent divides rather than multiplies, so each boundary is the
// float64 a parse of its decimal gives and a budget converted to milliseconds
// compares equal to it.
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

// Bounds is a copy of the ascending boundaries a histogram in unit counts
// into, which [Snapshot.Counts] are against and an exporter registers, and
// false for a unit no histogram may be declared in ([Validate] refuses it).
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

// binFor is the index of v's bucket, the first boundary at or above it: a
// bucket holds values above the previous boundary up to its own, and
// len(bounds) is the overflow.
func binFor(bounds []float64, v float64) int {
	i, _ := slices.BinarySearch(bounds, v)
	return i
}
