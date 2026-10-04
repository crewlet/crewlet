package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// This node's own record of the snapshots it has adopted — `statelog_adoption`,
// in the node estate, written through every phase of a join.
//
// It is this node's HISTORY: which donor's artefact it installed, which one
// (the checksum), when the join began and whether it finished — the record an
// operator reads when a node is behaving oddly and the question is what it is
// actually running. Nothing on the write path reads it: the operation ledger
// travels inside the artefact with its own watermark ([RecordLedgerLoss]), so
// an adoption bounds nothing.

// RecordAdoption writes or advances this node's adoption row.
//
// KEYED ON started_at, so one join writes one row through all three of its
// phases: [AdoptionScrubbed] when the artefact is verified, [AdoptionInstalled]
// when the file is in place, [AdoptionComplete] when the node may serve. Only
// the last stamps `completed_at`, which is what makes an interrupted adoption
// visible as an interrupted adoption rather than as a node that has always
// been here.
//
// startedAt is the instant [Adopter.Join] stamps once the fleet's offers are
// in, and never one the caller reads off its own clock.
//
// IN THE NODE ESTATE. The replicated estate is the thing being replaced, so a
// row written there would be renamed away between the second phase and the
// third — and the phase that matters most is the one that would vanish.
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
