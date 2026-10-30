package code_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// seedEndedLogged writes count failed check rows of repoID straight into a
// node's estate, each ended at the instant at and each with the log a failed
// run keeps — in two statements, so a benchmark seeds a step's worth of rows
// in two writes rather than one a row.
func (n *node) seedEndedLogged(repoID string, at time.Time, count int) {
	n.t.Helper()
	ended, overdue := store.EncodeTime(at), store.EncodeTime(at.Add(600*time.Second))
	n.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
			revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds,
			reason, runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
			duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
			url, summary, def, failures, tail)
		SELECT ?, printf('%040d', i), 'refs/heads/main', 'unit', 1, 'failed', 'tip', '', 0, '', '', '', '', 0,
			'', 600, 'exit', 'node-a', 'container', '', 0, '', ?, ?, ?, 1000, 1, 0, 1, 2, 6, ?, '', 'failed', '',
			'', '' FROM k`,
		count, repoID, ended, ended, ended, overdue)
	n.exec(`INSERT INTO code_check_logs (repo_id, commit_oid, target_ref, name, attempt, object, hash, size)
		SELECT repo_id, commit_oid, target_ref, name, attempt, 'logs/' || commit_oid, 'sha256:' || commit_oid,
			4096
		FROM code_checks WHERE repo_id = ?`, repoID)
}

// BenchmarkChecksSweepStep is ONE STEP OF THE RETENTION PASS'S CHECKS SWEEP on
// the round-trip harness's real log and estate: Store.ChecksSweep deciding,
// appending its record and waiting for its own apply — the duration the pass's
// pacer measures and waits twice after, and what a pass's budget is spent in —
// over a repository of its own each iteration, its failed checks seeded
// outside the timer, each with its log, ended past the history by the grace:
//
//   - rows=8: a sweep of a few rows, what a pass takes of a repository whose
//     few checks stopped ending;
//   - rows=500: CheckHorizonRows, the most rows one step removes, and their
//     500 logs.
//
// limits.go's RetentionPassBudget reasons from it beside
// BenchmarkRetireSweepStep, and doc.go's measured section records the
// figures.
func BenchmarkChecksSweepStep(b *testing.B) {
	code.Quiet(b)
	old := harnessAt.Add(-code.CheckHistory - code.CheckSweepGrace - time.Hour)
	for _, rows := range []int{8, code.CheckHorizonRows} {
		b.Run(fmt.Sprint("rows=", rows), func(b *testing.B) {
			n := newNode(b)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				id := n.ensure(fmt.Sprintf("repo-%06d", i), described("swept"), t1).RepoID
				n.seedEndedLogged(id, old, rows)
				// THE BROKER'S INSTANT IS THE DUTY HOLDER'S CLOCK, as a
				// pass's sweep's cutoff falls before its record's own.
				n.storedAt = func(uint64, time.Time) time.Time { return harnessAt }
				b.StartTimer()
				res, err := n.store.ChecksSweep(b.Context(), id, harnessAt)
				b.StopTimer()
				n.storedAt = nil
				if err != nil || res.Rows != rows || res.More || !res.Held() {
					b.Fatalf("the sweep of %d rows = %+v, %v; want them all removed by one applied record",
						rows, res, err)
				}
				if left := n.count(`SELECT COUNT(*) FROM code_check_logs WHERE repo_id = ?`, id); left != 0 {
					b.Fatalf("the sweep left %d of %s's %d logs", left, id, rows)
				}
			}
		})
	}
}
