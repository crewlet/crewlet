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

// WHICH COPY OF THE REPLICATED ESTATE A FILE IS: `statelog_estate`, the one
// framework row derived from no log (replicated migration 0060). Empty on a
// file the migrations created; a random instance the [Adopter] mints for each
// snapshot it installs, before the rename. It changes exactly when a node's
// rows were replaced by something other than its own appliers, which a
// forward-only reader cannot see and the in-memory signal loses across a
// restart; such a reader records the instance it was built from and compares
// ([EstateInstance]).

// estateInstanceStatement reads the estate's instance.
const estateInstanceStatement = `SELECT instance FROM statelog_estate WHERE id = 1`

// estateStampStatement names an artefact as a new copy.
const estateStampStatement = `
	UPDATE statelog_estate SET instance = ?, stamped_at = ? WHERE id = 1`

// EstateInstance reads which copy of the replicated estate a transaction on it
// is reading.
//
// THE EMPTY NAME IS AN ANSWER: the migrations' copy, never adopted over. It is
// the same on every such file, which is safe because names are compared only
// within one node and an adopted copy's is never empty. A MISSING ROW IS AN
// ERROR: the table's migration writes it, so a file without it is of unknown
// origin and must not match whatever an index last recorded.
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

// stampEstateInstance names the artefact at path a NEW copy of the estate and
// reports the instance it minted.
//
// MINTED, never the donor's: two adoptions from one donor would otherwise
// share an instance, and a reader built from the first would miss the second.
// ON THE ARTEFACT, BEFORE THE INSTALL'S RENAME: stamped after, a crash between
// the two leaves the donor's rows live under an instance a reader may have
// recorded. AFTER every check of the artefact, since this write changes it;
// the checksum covers the donor's bytes and no check reads this row.
func stampEstateInstance(ctx context.Context, path string, at time.Time) (string, error) {
	// Opening or writing the copy fails on this node's own disk: marked
	// [ownFault], so [Adopter.Join] ends the join rather than blaming the
	// donor and meeting the same fault on every other offer. A copy with no
	// row to stamp is the offer's fault: it lacks what its migration writes.
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
