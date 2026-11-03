package codeindex_test

import (
	"math/bits"
	"testing"

	"github.com/crewlet/crewlet/internal/codeindex"
)

// written is what an index written one equal segment at a time, k of them,
// asks of MergeNewest after each: the segments it keeps, oldest first, and
// the bytes its merges wrote.
func written(k int) (sizes []int64, merged int64) {
	const segment = 1
	for range k {
		sizes = append(sizes, segment)
		for codeindex.MergeNewest(sizes) {
			n := len(sizes)
			sum := sizes[n-2] + sizes[n-1]
			merged += sum
			sizes = append(sizes[:n-2], sum)
		}
	}
	return sizes, merged
}

// AN INDEX WRITTEN SEGMENT BY SEGMENT REWRITES EACH BLOB A LOGARITHMIC
// NUMBER OF TIMES, and keeps a logarithmic number of segments: asked after
// each segment is appended, MergeNewest keeps every segment more than twice
// the one after it, and a merge grows the segment a blob is in by at least
// half, so what k segments' source costs in merges is under 2·k·log2(k)
// segments' writes (log base 1.5 is 1.71 times log2). Past MaxSegments the
// newest two are merged whatever their sizes.
//
// Mutation: merge the newest two whatever their sizes (MergeNewest returns
// n >= 2) — every new segment rewrites the whole index, k²/2 writes.
func TestAnIndexWrittenSegmentBySegmentRewritesEachBlobLogarithmically(t *testing.T) {
	t.Parallel()
	for k := 1; k <= 512; k++ {
		sizes, merged := written(k)
		if bound := int64(2 * k * bits.Len(uint(k))); merged > bound {
			t.Fatalf("%d segments wrote %d segments' worth of merges, past 2·k·log2(k) = %d", k, merged, bound)
		}
		if bound := 2 * bits.Len(uint(k)); len(sizes) > bound {
			t.Fatalf("%d segments settled as %d, past 2·log2(k) = %d: %v", k, len(sizes), bound, sizes)
		}
		for i := 1; i < len(sizes); i++ {
			if 2*sizes[i] >= sizes[i-1] {
				t.Fatalf("%d segments settled as %v: segment %d is not under half the one before it", k, sizes, i)
			}
		}
	}
	// Each a third of the one before it, so the sizes alone merge nothing.
	over := make([]int64, codeindex.MaxSegments+1)
	over[len(over)-1] = 1
	for i := len(over) - 2; i >= 0; i-- {
		over[i] = 3 * over[i+1]
	}
	if !codeindex.MergeNewest(over) {
		t.Errorf("%d geometric segments are past MaxSegments %d and were not merged", len(over), codeindex.MaxSegments)
	}
	if codeindex.MergeNewest(over[1:]) {
		t.Errorf("MaxSegments geometric segments were merged")
	}
}
