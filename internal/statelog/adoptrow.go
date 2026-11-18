package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// statelog_adoption, in the node estate, is this node's HISTORY of the
// snapshots it adopted (donor, checksum, when the join began, whether it
// finished), for an operator asking what a node is actually running. Nothing
// on the write path reads it: the operation ledger travels inside the snapshot
// with its own watermark ([Rows.LostBefore]), so an adoption bounds nothing.

// RecordAdoption writes or advances this node's adoption row.
//
// Keyed on started_at, [Adopter.Join]'s stamp once the fleet's offers are in
// (never the caller's own clock), so one join is one row through its phases:
// [AdoptionScrubbed], [AdoptionInstalled], [AdoptionComplete]. Only the last
// stamps completed_at, so an interrupted adoption shows as one rather than as
// a node that has always been here.
//
// In the node estate because the replicated estate is the thing being
// replaced: a row there would be renamed away between the second phase and
// the third, the one that matters most.
func RecordAdoption(ctx context.Context, db *store.DB, startedAt time.Time,
	donor string, m Manifest, phase AdoptionPhase) error {

	if db == nil {
		return fmt.Errorf("statelog: no store to record an adoption in")
	}
	var completed any
	if phase == AdoptionComplete {
		completed = store.EncodeTime(time.Now().UTC())
	}
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO statelog_adoption
				(started_at, donor, manifest, completed_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (started_at) DO UPDATE SET
				donor        = excluded.donor,
				manifest     = excluded.manifest,
				-- COALESCE, so a phase written out of order cannot
				-- un-complete an adoption that finished. Nothing writes
				-- them out of order today; a retry after a partial
				-- failure is exactly what would.
				completed_at = COALESCE(excluded.completed_at, statelog_adoption.completed_at)`,
			store.EncodeTime(startedAt.UTC()), donor, m.SHA256, completed)
		return err
	})
	if err != nil {
		return fmt.Errorf("statelog: record this node's adoption of %s's "+
			"snapshot at phase %q: %w", donor, phase, err)
	}
	return nil
}
