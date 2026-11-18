package gitlet_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// BenchmarkRetireSweepStep is ONE STEP OF THE RETENTION PASS'S SWEEP on the
// round-trip harness's real log and estate: Store.RetireSweep deciding,
// appending and waiting for its own apply (the duration the pass's pacer
// measures and its budget is spent in), over a fresh repository each
// iteration whose retired rows are seeded, untimed, as one record's:
//
//   - rows=8: a few rows, a repository one small compaction retired from;
//   - rows=3999: RetireSweepRows, the most one step removes.
//
// BenchmarkApplyARetireSweep is the apply alone, the share every other data
// node's write lane pays; limits.go's RetentionPassBudget reasons from both.
func BenchmarkRetireSweepStep(b *testing.B) {
	gitlet.Quiet(b)
	old := harnessAt.Add(-gitlet.RetireGrace - time.Hour)
	for _, rows := range []int{8, gitlet.RetireSweepRows} {
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
