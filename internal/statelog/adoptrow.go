package statelog

import (
	"context"
	"database/sql"
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
// a missing column rather than reporting no adoption. And the rule the
// migration wrote down for reading it — "the newest completed one" — is not
// the one [AdoptedAt] applies, for the reason its doc gives.

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
// in, and never one the caller reads off its own clock: it is a bound only
// because it follows every offer — see [AdoptedAt].
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

// AdoptedAt is the instant before which this node's operation ledger cannot
// vouch for an operation, reporting false when there is none.
//
// # What each row says
//
// A COMPLETED adoption installed a donated file whose ledger was SCRUBBED, so
// an operation minted before it began may be one this node cannot answer for.
// An INCOMPLETE row is a join that is running or stopped partway, and the row
// cannot say which side of the install it stopped on: the table has no phase
// column — [RecordAdoption] writes the same columns at every phase and stamps
// only completed_at — so a join that failed after its rename and before it
// completed looks exactly like one that failed before it closed anything. A
// running node that reopens such an estate finds the artefact current and
// carries on over it, and so does a node restarted after a crash in that
// window: the row stays incomplete, because that is what happened.
//
// # Why the START bounds an incomplete row, on either side of the install
//
// Because of WHEN it is stamped: [Adopter.Join] stamps it after the fleet's
// offers are in and before it fetches anything, and every artefact the join
// can install is one of those offers — the checksum it verifies is the
// offer's. A donor offers the artefact it holds when it ANSWERS, so each was
// finished before its donor answered, which was before the collection ended,
// which was before the stamp. So an operation minted after the stamp was
// published after every such artefact was taken, at the log's next sequence,
// and that is above every position the artefact holds: this node's own
// applier writes its ledger row into whichever file is live, the artefact or
// the file it never replaced. An operation minted before the stamp resolves
// `unknown` rather than being re-decided, which a caller retries under the
// same operation id, and which is safe whichever side of the install the join
// stopped on. Both instants are read off this node's own wall clock, so no
// skew between nodes enters the comparison — what it does assume, as the row
// has since it was keyed on a wall-clock instant, is a clock not stepped
// backwards between a mint and the stamp.
//
// No EARLIER instant is a bound, which is why the start is not stamped when
// the join begins. A donor's snapshotter runs on its own schedule and can
// finish an artefact between the moment a join asks and the moment the donor
// answers, and an operation this node published in between is then inside
// that artefact with its ledger row scrubbed — minted after a start stamped
// before the ask, and re-decided.
//
// So the answer is the LATEST such instant over every row: completed_at where
// the adoption finished — later than its start, so never less safe —
// started_at where it did not. (zero, false) only when this node has never
// recorded an adoption, and its ledger covers its whole life: a join records
// its row before it closes anything, and one that fails before then — no
// offer, a failed fetch, a refused artefact — installed nothing.
//
// # Why not the newest completed row, or the newest row
//
// Migration 0022 says the reader takes the newest completed one. That answers
// an incomplete adoption AFTER a completed one with the older instant, and
// lets operations minted between the two be re-decided against a ledger the
// second join may already have replaced. Reading the newest row, complete or
// not, and answering (zero, false) when it is incomplete — which this did —
// is worse in the same direction: it re-decides operations minted before even
// the COMPLETED adoption. The migration is applied history and is not edited
// (schema_migrations keys on its filename), so the rule is stated here, where
// the query is.
func AdoptedAt(ctx context.Context, db *store.DB) (time.Time, bool, error) {
	if db == nil {
		return time.Time{}, false, fmt.Errorf("statelog: no store to read an " +
			"adoption record from")
	}
	// ONE AGGREGATE, which answers NULL over an empty table rather than no
	// row: "never recorded an adoption" is the NULL, and there is no second
	// shape of it to handle.
	var bound sql.NullInt64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT MAX(COALESCE(completed_at, started_at)) FROM statelog_adoption`).Scan(&bound)
	})
	switch {
	case err != nil:
		return time.Time{}, false, fmt.Errorf("statelog: read this node's "+
			"adoption record: %w", err)
	case !bound.Valid:
		return time.Time{}, false, nil
	}
	return store.DecodeTime(bound.Int64), true, nil
}
