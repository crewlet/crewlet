package statelog_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AN ADOPTION THAT DID NOT COMPLETE STILL BOUNDS WHAT THE LEDGER CAN ANSWER.
//
// The row cannot say which side of the install a join stopped on — it keeps
// when the join began and whether it completed, nothing else — so an
// incomplete one has to be read as though its install happened: from its
// START, every operation minted earlier resolves `unknown` rather than being
// re-decided against a ledger that may be the donor's scrubbed one.
//
// Two tempting readings fail here, and each has its case. The newest COMPLETED
// row answers an abandoned join after a completed one with the older instant,
// and the operations minted between the two are re-decided. The newest row,
// read as "never adopted" when it is incomplete, re-decides operations minted
// before even the completed adoption.
//
// # Where the clock is read
//
// RecordAdoption stamps a completion with the clock, so each case places its
// rows around a clock read of its own, taken immediately before it records —
// a join that follows the completed adoption starts an HOUR after that read,
// and the only thing between the read and the stamp is the store's own
// inserts. Read in the parent, the same instant would be followed by every
// other parallel test in the binary: a subtest that calls t.Parallel waits
// for the parent to return and then queues behind them, and on a single CPU
// that wait was measured past a minute — past a join placed a minute on, so
// the completion stamp answered instead and a correct reader failed.
func TestAnAdoptionThatDidNotCompleteStillBoundsWhatTheLedgerCanAnswer(t *testing.T) {
	t.Parallel()
	type row struct {
		// started is when the row's join began, in hours from the case's
		// own clock read.
		started int
		phase   statelog.AdoptionPhase
	}
	cases := []struct {
		name string
		rows []row
		// want is the bound, in hours from the same read; completedLast is
		// a case whose bound is the completion instant RecordAdoption
		// stamps, which only brackets.
		want          int
		completedLast bool
		never         bool
	}{
		{name: "never adopted", never: true},
		{name: "one adoption that completed",
			rows:          []row{{-2, statelog.AdoptionComplete}},
			completedLast: true},
		{name: "one join that stopped before completing",
			rows: []row{{-2, statelog.AdoptionScrubbed}},
			want: -2},
		{name: "a join that stopped after an adoption that completed",
			rows: []row{{-2, statelog.AdoptionComplete}, {1, statelog.AdoptionInstalled}},
			want: 1},
		{name: "an adoption that completed after a join that stopped",
			rows:          []row{{-2, statelog.AdoptionScrubbed}, {-1, statelog.AdoptionComplete}},
			completedLast: true},
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
				if err := statelog.RecordAdoption(t.Context(), db, at(r.started), "donor",
					statelog.Manifest{SHA256: "sum"}, r.phase); err != nil {
					t.Fatalf("RecordAdoption: %v", err)
				}
			}
			after := time.Now().UTC()

			got, ever, err := statelog.AdoptedAt(t.Context(), db)
			if err != nil {
				t.Fatalf("AdoptedAt: %v", err)
			}
			switch {
			case c.never:
				if ever {
					t.Fatalf("a node that never began an adoption reports one at %s — "+
						"its ledger covers its whole life", got)
				}
			case !ever:
				t.Fatal("AdoptedAt reports no adoption, so every operation this " +
					"node's ledger cannot answer for is re-decided")
			case c.completedLast:
				if got.Before(base) || got.After(after) {
					t.Fatalf("AdoptedAt = %s, want the completion stamped between %s "+
						"and %s", got, base, after)
				}
			case !got.Equal(at(c.want)):
				t.Fatalf("AdoptedAt = %s, want %s — the start of the join the row "+
					"cannot place on either side of its install", got, at(c.want))
			}
		})
	}
}
