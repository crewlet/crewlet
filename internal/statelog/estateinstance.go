package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
)

// WHICH COPY OF THE REPLICATED ESTATE A FILE IS — `statelog_estate`, the one
// framework row that is not derived from any log (replicated migration 0060).
//
// Every other row in the replicated file is the same on every data node at one
// position. This one names the FILE: empty on a file the migrations created,
// and a random instance the [Adopter] mints for every snapshot it installs,
// before the rename. So it changes exactly when the rows under a node were
// replaced by something other than its own appliers — which is the one change a
// reader deriving state from those rows by a forward-only walk cannot see for
// itself, and the one the engine's in-memory signal for it loses across a
// restart. Such a reader records the instance it was built from and compares
// ([EstateInstance]); a mismatch is a replacement, whenever it happened.

// estateInstanceStatement reads the estate's instance.
const estateInstanceStatement = `SELECT instance FROM statelog_estate WHERE id = 1`

// estateStampStatement names an artefact as a new copy.
const estateStampStatement = `
	UPDATE statelog_estate SET instance = ?, stamped_at = ? WHERE id = 1`

// EstateInstance reads which copy of the replicated estate a transaction on it
// is reading.
//
// THE EMPTY NAME IS AN ANSWER: the copy the migrations created and no adoption
// has replaced. It is the same on every such file, which is safe because a name
// is only compared within one node, and an adopted copy's is never empty.
//
// A MISSING ROW IS AN ERROR, never the empty name: the migration that makes the
// table writes the row, so a file without it was not migrated by this binary,
// and reading its absence as "created here" would let a file of unknown origin
// match whatever an index last recorded.
func EstateInstance(ctx context.Context, tx *sql.Tx) (string, error) {
	var instance string
	err := tx.QueryRowContext(ctx, estateInstanceStatement).Scan(&instance)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("statelog: the replicated estate names no instance " +
			"of itself: statelog_estate has no row, which its migration writes")
	case err != nil:
		return "", fmt.Errorf("statelog: read which copy the replicated estate is: %w", err)
	}
	return instance, nil
}

// stampEstateInstance names the artefact at path as a NEW copy of the estate,
// and reports the instance it minted.
//
// MINTED, never the donor's: the artefact arrives carrying the instance of the
// file it was copied from, and a node that adopted twice from one donor would
// otherwise come back to an instance it had already recorded — and a reader
// that rebuilt from the first adoption would read the second as no change.
//
// ON THE ARTEFACT, BEFORE THE INSTALL'S RENAME, so the instant the new rows are
// at the live path they already say they are new. Stamped after the rename, a
// crash between the two would leave the donor's rows in place under an instance
// some reader may already have recorded, which is the failure this exists to
// prevent. And AFTER every check of the artefact, which this changes: the
// checksum is of the bytes the donor sent and the positions and scrub are read
// from the file, none of which this row is part of.
func stampEstateInstance(ctx context.Context, path string, at time.Time) (string, error) {
	// A FAILURE OPENING OR WRITING THE COPY IS THIS NODE'S, its own file on its
	// own disk, and is marked as one ([ownFault]) so [Adopter.Join] ends the
	// join there rather than blaming the donor and fetching every other offer
	// only to meet the same fault. A copy with no row to stamp is the offer's:
	// the artefact lacks what its own migration writes.
	db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})
	if err != nil {
		return "", ownFault(fmt.Errorf("statelog: open %s to name it a new copy: %w", path, err))
	}
	defer func() { _ = db.Close() }()

	instance := uuid.NewString()
	var unstamped error
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		res, execErr := tx.ExecContext(ctx, estateStampStatement, instance,
			store.EncodeTime(at.UTC()))
		if execErr != nil {
			return execErr
		}
		n, countErr := res.RowsAffected()
		if countErr != nil {
			return countErr
		}
		if n != 1 {
			unstamped = fmt.Errorf("statelog_estate has %d rows to stamp, want the one "+
				"its migration writes", n)
			return unstamped
		}
		return nil
	})
	if err != nil {
		err = fmt.Errorf("statelog: name %s a new copy of the estate: %w", path, err)
		if unstamped != nil {
			return "", err
		}
		return "", ownFault(err)
	}
	return instance, nil
}
