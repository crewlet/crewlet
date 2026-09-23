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
// # What it is for now, and what it stopped being for
//
// It is this node's HISTORY: which donor's artefact it installed, which one
// (the checksum), when the join began and whether it finished — the record an
// operator reads when a node is behaving oddly and the question is what it is
// actually running.
//
// It used to be the BOUND on this node's operation ledger as well: a donated
// snapshot arrived with the donor's ledger scrubbed, so the ledger could vouch
// for nothing minted before the latest adoption, and the publisher read that
// instant off these rows. The ledger travels now, with a watermark of its own
// in the same file ([RecordLedgerLoss]), so nothing on the write path reads
// this table. What remains of that role is the rows a build from before the
// change wrote: each of them is an adoption whose ledger WAS scrubbed, and
// [FoldLegacyAdoptions] carries them into the watermark once. `ledger_folded`
// (node migration 0030) is what says which rows that has happened to.

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
// FOLDED FROM THE START: the adopter wrote whatever the artefact's ledger had
// lost into the artefact itself before it recorded anything here, so there is
// nothing for [FoldLegacyAdoptions] to carry.
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
				(started_at, donor, manifest, completed_at, ledger_folded)
			VALUES (?, ?, ?, ?, 1)
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

// FoldLegacyAdoptions carries every adoption a build from before the ledger
// travelled recorded into the watermark of each domain's ledger, and marks the
// rows so it never carries them again. The engine runs it at boot, before any
// join and before anything publishes.
//
// # Why those rows, and why this bound
//
// Such an adoption installed a file whose ledger was SCRUBBED, and that file —
// or one derived from it by this node's own applier — is the one this node
// runs: the only thing that replaces it is another adoption, and one run by
// this build marks its rows as it records them. The bound is the one those
// builds read, the latest of each row's completion or, where it did not
// complete, its start, which follows every offer its join collected — so an
// operation minted after it was published after every artefact that join
// could have installed. An upgraded node that skipped this would read its
// scrubbed ledger's silence as conclusive, and decide again every retry of an
// operation minted before its last adoption.
//
// # Why this order
//
// The watermark first and the mark second, and the two are in different files
// with no transaction across them: a crash between them folds the same rows
// again at the next boot, which the watermark's own monotonicity makes a
// no-op. The other order would mark rows whose loss was never recorded.
func FoldLegacyAdoptions(ctx context.Context, db *store.DB, domains []Domain) error {
	if db == nil {
		return fmt.Errorf("statelog: no store to fold an adoption history in")
	}
	// ONE AGGREGATE, which answers NULL over no unfolded row rather than no
	// row: "nothing to fold" is the NULL, and there is no second shape of it.
	var bound sql.NullInt64
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT MAX(COALESCE(completed_at, started_at)) FROM statelog_adoption
			WHERE ledger_folded = 0`).Scan(&bound)
	}); err != nil {
		return fmt.Errorf("statelog: read this node's adoption history: %w", err)
	}
	if !bound.Valid {
		return nil
	}
	before := store.DecodeTime(bound.Int64)
	for _, d := range domains {
		if err := RecordLedgerLoss(ctx, db.Replicated(), d, before); err != nil {
			return err
		}
	}
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE statelog_adoption SET ledger_folded = 1 WHERE ledger_folded = 0`)
		return err
	}); err != nil {
		return fmt.Errorf("statelog: mark this node's earlier adoptions as "+
			"carried into the ledger's watermark: %w", err)
	}
	return nil
}
