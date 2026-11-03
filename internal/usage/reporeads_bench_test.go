package usage_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// BenchmarkRepoReaders is what "who read it" costs at its largest: one unit
// repository read every day of the history by four hundred readers on each of
// three nodes — 217 200 reader rows — asked over the whole history for its
// first page of readers (a tool answer's 64 KiB, which holds all four hundred)
// and its elided count, and for a page well inside the walk, which is what
// usage_repo_reads_repo_idx's shape decides: each page seeks the reader it
// resumes at and reads its own readers' rows and no one else's.
//
// Measured with `-benchtime 3x -count 1` on two cores of an Intel Core Ultra 7
// 265U: the first page 68 ms (its 400 readers' 217 200 rows), the page after
// the three hundredth reader 17 ms (the last hundred readers' rows, a quarter
// of the first's: the resumed page re-reads nobody), the elided count 0.3 ms.
func BenchmarkRepoReaders(b *testing.B) {
	const (
		readers, nodes = 400, 3
		pageBytes      = 64 << 10
	)
	db := openStore(b)
	last := period.At(period.Day, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), time.UTC)
	first := last.Shift(-(usage.HorizonDays - 1))
	seedReaders(b, db, first, usage.HorizonDays, nodes, readers)
	q := usage.RepoReadsQuery{RepoID: "repo-1", From: first.Label, To: last.Label}

	// THE DEEP PAGE resumes after the three hundredth reader.
	deep := usage.Group{"operator", fmt.Sprintf("pat:%04d", 300), "git", "maya"}
	for _, arm := range []struct {
		name  string
		after usage.Group
	}{{"first page", nil}, {"a page after 300 readers", deep}} {
		b.Run(arm.name, func(b *testing.B) {
			for b.Loop() {
				page, err := usage.RepoReadsPage(b.Context(), db.Replicated(), q, arm.after, pageBytes)
				if err != nil {
					b.Fatal(err)
				}
				if len(page.Rows) == 0 {
					b.Fatal("the page holds no reader")
				}
			}
		})
	}
	b.Run("the elided count", func(b *testing.B) {
		for b.Loop() {
			if _, err := usage.RepoReadsElided(b.Context(), db.Replicated(), q); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// seedReaders writes days days of repository repo-1's readers from first on,
// on each of nodes nodes, readers credentials of one person each day, as the
// applier writes them — in one transaction, since how the rows arrived is not
// what is measured.
func seedReaders(b *testing.B, db *store.DB, first period.Window, days, nodes, readers int) {
	b.Helper()
	at := store.EncodeTime(first.Start)
	if err := db.Replicated().Tx(b.Context(), func(tx *sql.Tx) error {
		for d := range days {
			day := first.Shift(d).Label
			for n := range nodes {
				node := fmt.Sprintf("node-%d", n)
				if _, err := tx.ExecContext(b.Context(), `INSERT INTO usage_repo_read_days
					(day, node, repo_id, repo_name, readers_elided, version) VALUES (?, ?, 'repo-1', 'platform', 0, ?)`,
					day, node, d*nodes+n+1); err != nil {
					return err
				}
				for r := range readers {
					if _, err := tx.ExecContext(b.Context(), `INSERT INTO usage_repo_reads
						(day, node, repo_id, principal_kind, principal, via, seat, reads, bytes, last_at)
						VALUES (?, ?, 'repo-1', 'operator', ?, 'git', 'maya', 3, 150000, ?)`,
						day, node, fmt.Sprintf("pat:%04d", r), at); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed the readers: %v", err)
	}
}
