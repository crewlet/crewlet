package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// This node's own record of the snapshots it has adopted — the WRITER and the
// READER of `statelog_adoption`, in one file.
//
// # Why both halves are here
//
// The row answers one question and it is a subtle one: which operation ids
// this node's ledger can be trusted to have an opinion about. A donated
// snapshot arrives with the donor's ledger SCRUBBED out of it, so an operation
// minted before the adoption completed is one this node cannot answer for at
// all — and reading the ledger's silence as "somebody else won" would make a
// writer re-decide against a row that moved because of its own write.
//
// Written in one place and read in another, that question gets two answers
// eventually. It already had: the reader looked for a row keyed `id =
// 'current'` on a table keyed on `started_at`, which fails on every call with
// a missing column rather than reporting no adoption. The table's own rule —
// rows ACCUMULATE, and the newest one is what counts — was written in the
// migration and nowhere in the code.

// RecordAdoption writes or advances this node's adoption row.
//
// KEYED ON started_at, so one join writes one row through all three of its
// phases: [AdoptionScrubbed] when the artefact is verified, [AdoptionInstalled]
// when the file is in place, [AdoptionComplete] when the node may serve. Only
// the last stamps `completed_at`, which is what makes an interrupted adoption
// visible as an interrupted adoption rather than as a node that has always
// been here.
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

// AdoptedAt is when this node's most recent adoption COMPLETED.
//
// THE NEWEST ROW, complete or not, and the difference is the whole point:
//
//   - No rows at all — this node has never adopted, so its ledger covers its
//     whole life and every operation id can be answered for. (zero, false)
//   - The newest row is incomplete — a join is running or one died partway.
//     The ledger is scrubbed and there is no instant to compare against, so
//     the honest answer is that nothing can be answered for. (zero, false)
//   - The newest row is complete — operations minted after that instant are
//     this node's own and the ones before it are not. (instant, true)
//
// The first two answer identically here and are read differently upstream: an
// incomplete adoption also blocks serving, through the phase the row carries,
// so a node in that state is not asking this question yet.
func AdoptedAt(ctx context.Context, db *store.DB) (time.Time, bool, error) {
	if db == nil {
		return time.Time{}, false, fmt.Errorf("statelog: no store to read an " +
			"adoption record from")
	}
	var completed sql.NullInt64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT completed_at FROM statelog_adoption
			ORDER BY started_at DESC LIMIT 1`).Scan(&completed)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("statelog: read this node's "+
			"adoption record: %w", err)
	case !completed.Valid:
		return time.Time{}, false, nil
	}
	return store.DecodeTime(completed.Int64), true, nil
}
