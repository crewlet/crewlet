package statelog_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AN ADOPTION A BUILD FROM BEFORE THE LEDGER TRAVELLED RECORDED IS CARRIED INTO
// THE LEDGER'S WATERMARK — once, at its bound, and never an adoption of this
// build's.
//
// Such a row is an adoption whose ledger was scrubbed, on a node whose
// watermark does not say so, and the node runs the file it installed. Left
// alone after an upgrade, the scrubbed ledger's silence reads as conclusive,
// and every retry of an operation minted before that adoption is decided a
// second time.
//
// The bound is the one those builds read, and each tempting alternative has
// its case: the newest COMPLETED row answers an abandoned join after a
// completed one with the older instant, and the newest row read as "never
// adopted" when it is incomplete loses the completed adoption before it.
//
// # Where the clock is read
//
// RecordAdoption stamps a completion with the clock, and so does the legacy
// row's own completion here, so each case places its rows around a clock read
// of its own, taken immediately before it writes — for the reason the parent
// cannot: a subtest that calls t.Parallel waits for the parent to return and
// then queues behind every other parallel test in the binary.
func TestAnAdoptionAnEarlierBuildRecordedIsFoldedIntoTheWatermark(t *testing.T) {
	t.Parallel()
	type row struct {
		// started is when the row's join began, in hours from the case's
		// own clock read; completed says whether it finished, an hour
		// after it began; legacy says an earlier build wrote it.
		started   int
		completed bool
		legacy    bool
	}
	cases := []struct {
		name string
		rows []row
		// want is the watermark, in hours from the same read; none is a
		// case that must leave the ledger losing nothing.
		want int
		none bool
	}{
		{name: "never adopted", none: true},
		{name: "one adoption of this build", rows: []row{{-2, true, false}}, none: true},
		{name: "one earlier adoption that completed",
			rows: []row{{-3, true, true}}, want: -2},
		{name: "one earlier join that stopped before completing",
			rows: []row{{-2, false, true}}, want: -2},
		{name: "an earlier join that stopped after one that completed",
			rows: []row{{-3, true, true}, {1, false, true}}, want: 1},
		{name: "an earlier adoption that completed after a join that stopped",
			rows: []row{{-3, false, true}, {-2, true, true}}, want: -1},
		{name: "an adoption of this build after an earlier one",
			rows: []row{{-3, true, true}, {-1, true, false}}, want: -2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			// Microsecond instants, which is what the store keeps, so a
			// round trip compares equal.
			base := time.Now().UTC().Truncate(time.Microsecond)
			at := func(hours int) time.Time { return base.Add(time.Duration(hours) * time.Hour) }
			for _, r := range c.rows {
				if !r.legacy {
					if err := statelog.RecordAdoption(t.Context(), db, at(r.started), "donor",
						statelog.Manifest{SHA256: "sum"}, statelog.AdoptionComplete); err != nil {
						t.Fatalf("RecordAdoption: %v", err)
					}
					continue
				}
				writeLegacyAdoption(t, db, at(r.started), r.completed, at(r.started+1))
			}

			domains := []statelog.Domain{probeDomain{}}
			if err := statelog.FoldLegacyAdoptions(t.Context(), db, domains); err != nil {
				t.Fatalf("FoldLegacyAdoptions: %v", err)
			}
			before, lost := lostBefore(t, db)
			switch {
			case c.none && lost:
				t.Fatalf("the ledger may have lost rows before %s, and no adoption "+
					"here scrubbed it — every first attempt minted before that is "+
					"refused", before)
			case c.none:
			case !lost:
				t.Fatal("the fold recorded no loss, so every operation minted " +
					"before the earlier adoption is decided against a scrubbed ledger")
			case !before.Equal(at(c.want)):
				t.Fatalf("the watermark is %s, want %s", before, at(c.want))
			}

			// AND ONCE: a second boot folds nothing, because every row is
			// marked — even with the watermark moved back behind it.
			var unfolded int
			if err := db.Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRowContext(t.Context(), `
					SELECT COUNT(*) FROM statelog_adoption WHERE ledger_folded = 0`).Scan(&unfolded)
			}); err != nil {
				t.Fatalf("count the unfolded rows: %v", err)
			}
			if unfolded != 0 {
				t.Fatalf("%d adoption row(s) are still unfolded after the fold", unfolded)
			}
		})
	}
}

// writeLegacyAdoption writes an adoption row the way a build from before the
// ledger travelled did: every column it knew about, and nothing else — so the
// column that marks a row folded takes its default.
func writeLegacyAdoption(t *testing.T, db *store.DB, started time.Time, completed bool,
	completedAt time.Time) {

	t.Helper()
	var done any
	if completed {
		done = store.EncodeTime(completedAt)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_adoption (started_at, donor, manifest, completed_at)
			VALUES (?, 'donor', 'sum', ?)`, store.EncodeTime(started), done)
		return err
	}); err != nil {
		t.Fatalf("write an earlier build's adoption row: %v", err)
	}
}

// lostBefore is the probe ledger's watermark in db's replicated estate.
func lostBefore(t *testing.T, db *store.DB) (time.Time, bool) {
	t.Helper()
	rows, err := statelog.NewRows(db, probeDomain{}, nil)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	before, lost, err := rows.LostBefore(t.Context())
	if err != nil {
		t.Fatalf("read the ledger's watermark: %v", err)
	}
	return before, lost
}
