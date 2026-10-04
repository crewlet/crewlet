package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// How far back an operation ledger may have lost rows, and the three things
// that say so.
//
// A domain's ledger is what makes a retry safe: a retry of an operation whose
// row is present is answered with the first copy, and one whose row is absent
// is decided afresh. That reading of absence is sound only where the ledger
// never lost a row it once held, so every way it can lose one records the
// instant before which it may have — the ledger's WATERMARK, in
// `statelog_ops_lost`, beside the ledger in the same file ([Rows.LostBefore],
// read by [Publisher.vouches]). Every write moves it only forward.
//
// THE SWEEP deletes rows applied more than [OpsRetention] ago and records its
// cutoff in the transaction that deletes them ([tables.purgeOps]).
//
// AN ADOPTION LOSES NOTHING. The ledger TRAVELS inside a snapshot
// ([Domain.OpsTable]) and so does the watermark, so the adopter holds the
// donor's rows and inherits how far back the donor had lost any.

// ledgerWriter is the one thing [RecordLedgerLoss] needs of an estate.
type ledgerWriter interface {
	Tx(ctx context.Context, fn func(*sql.Tx) error) error
}

// RecordLedgerLoss records, in the estate db, that d's operation ledger may
// have lost every row applied before before. A domain with no ledger has
// nothing to lose and nothing is written.
//
// Monotone: an earlier instant than the one already recorded changes nothing,
// because a loss once recorded is never un-lost.
func RecordLedgerLoss(ctx context.Context, db ledgerWriter, d Domain, before time.Time) error {
	t, err := newTables(d)
	if err != nil {
		return err
	}
	if t.ops == "" {
		return nil
	}
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		return t.markLost(ctx, tx, before)
	}); err != nil {
		return fmt.Errorf("statelog: record that %s's operation ledger may have "+
			"lost rows: %w", d.Name(), err)
	}
	return nil
}
