package code_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// BenchmarkRetireSweepStep is ONE STEP OF THE RETENTION PASS'S SWEEP on the
// round-trip harness's real log and estate: Store.RetireSweep deciding,
// appending its record and waiting for its own apply — the duration the pass's
// pacer measures and waits twice after, and what a pass's budget is spent in —
// over a repository of its own each iteration, its retired rows seeded outside
// the timer as one earlier record's retirements:
//
//   - rows=8: a sweep of a few rows, what the pass takes of a repository one
//     small compaction retired from;
//   - rows=3999: RetireSweepRows, the most rows one step removes.
//
// BenchmarkApplyARetireSweep is the apply alone, the share of it every other
// data node's write lane pays; limits.go's RetentionPassBudget reasons from
// both.
func BenchmarkRetireSweepStep(b *testing.B) {
	code.Quiet(b)
	old := harnessAt.Add(-code.RetireGrace - time.Hour)
	for _, rows := range []int{8, code.RetireSweepRows} {
		b.Run(fmt.Sprint("rows=", rows), func(b *testing.B) {
			n := newNode(b)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				id := n.ensure(fmt.Sprintf("repo-%06d", i), described("swept"), t1).RepoID
				n.seedRetired(id, 1, old, rows)
				b.StartTimer()
				res, err := n.store.RetireSweep(b.Context(), id, harnessAt)
				b.StopTimer()
				if err != nil || res.Rows != rows || res.More || !res.Held() {
					b.Fatalf("the sweep of %d rows = %+v, %v; want them all removed by one "+
						"applied record", rows, res, err)
				}
			}
		})
	}
}
