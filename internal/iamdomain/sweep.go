package iamdomain

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RETENTION SWEEP, and why it is a RECORD rather than a local delete.
//
// These rows are identity-claimed: N nodes assert their replicated tables are
// byte-identical. A node that swept on its own clock would hold different
// bytes from its peers, and the claim would quietly become a claim about how
// synchronised their clocks were — checkable by nothing, false whenever an NTP
// correction landed, and indistinguishable from an applier that had diverged.
//
// So the publisher resolves each horizon to a POSITION, once, and publishes
// it. Every node then deletes exactly the same rows, whatever its clock says.
//
// # Per bucket, and why the division is the point
//
// One record per bucket rather than one for the estate, because a sweep is
// bounded by the applier's row budget and a company-wide delete is not: at the
// horizons this domain keeps, one tick's worth of session rows for a large
// company is a single statement holding this store's only writer for as long
// as it takes. Sixty-four records spread it across sixty-four transactions,
// and the framework's own budget ends each one at a record boundary.
//
// IT STILL HAS TO BE BOUNDED WITHIN A BUCKET, which is what [MaxSweepRows] is
// for: a bucket that has been unswept for a month is not a sixty-fourth of a
// tick, it is a sixty-fourth of a month, and the budget is per transaction
// rather than per record.

// MaxSweepRows is how many rows one sweep record's apply may delete.
//
// FOUR THOUSAND, which is [statelog.ApplyTxRowBudget] — the framework's own
// per-transaction row budget, and the number every other bounded statement in
// this estate is measured against. It is a ceiling on ONE record's work rather
// than on the sweep as a whole: a bucket with more than this behind it is
// swept again on the next tick, and converges.
//
// Taking the framework's constant rather than a number of this domain's own is
// deliberate: a sweep that deleted more than the budget the applier is sized
// for would hold this store's only writer past the point every other domain's
// apply is waiting at, which is the contention the budget exists to bound.
const MaxSweepRows = statelog.ApplyTxRowBudget

// applySweep deletes one bucket's expired rows below the positions the
// publisher resolved.
func (a *Applier) applySweep(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Op != OpSweep {
		return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a sweep "+
			"subject, which this build has no case for", at.position,
			at.record.Op)
	}
	sweep, err := DecodeSweep(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the sweep record at %s: %w",
			at.position, err)
	}
	if sweep.Bucket >= Buckets {
		return 0, fmt.Errorf("iamdomain: the sweep at %s names bucket %d and "+
			"this estate has %d", at.position, sweep.Bucket, Buckets)
	}
	bucket := int64(sweep.Bucket)
	expired := millis(sweep.Expired)

	var written int64
	// THE TRAIL'S TWO HORIZONS, each a POSITION rather than an age. A zero
	// position deletes nothing, which is the correct reading of "this
	// company has no rows old enough yet" and is NOT the same as
	// "everything": a zero is below every row's version.
	for _, horizon := range []struct {
		class  HistoryClass
		before uint64
	}{
		{ClassChange, sweep.Changes},
		{ClassSession, sweep.Sessions},
	} {
		if horizon.before == 0 {
			continue
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM iam_history
			WHERE rowid IN (
				SELECT rowid FROM iam_history
				WHERE bucket = ? AND class = ? AND version < ?
				LIMIT ?)`,
			bucket, string(horizon.class), int64(horizon.before), MaxSweepRows)
		if err != nil {
			return int(written), fmt.Errorf("iamdomain: sweep the %s trail in "+
				"bucket %d: %w", horizon.class, bucket, err)
		}
		n, _ := result.RowsAffected()
		written += n
	}

	// AND THE THREE TABLES WHOSE ROWS ARE OVER RATHER THAN OLD. Each is
	// collected against the BROKER's instant the record carries, never a
	// node's own clock, for this file's whole reason.
	//
	// A LIMIT ON EACH, for MaxSweepRows' reason. The subquery is over the
	// index each table ships for exactly this predicate.
	if expired > 0 {
		for _, collect := range []struct {
			what, sql string
		}{
			{"sessions", `
				DELETE FROM iam_sessions WHERE rowid IN (
					SELECT rowid FROM iam_sessions
					WHERE bucket = ? AND ended_at > 0 AND ended_at < ?
					LIMIT ?)`},
			{"expired sessions", `
				DELETE FROM iam_sessions WHERE rowid IN (
					SELECT rowid FROM iam_sessions
					WHERE bucket = ? AND ended_at = 0
					  AND absolute_expires_at > 0 AND absolute_expires_at < ?
					LIMIT ?)`},
			{"invitations", `
				DELETE FROM iam_invites WHERE rowid IN (
					SELECT rowid FROM iam_invites
					WHERE bucket = ? AND redeemed_at = 0
					  AND expires_at > 0 AND expires_at < ?
					LIMIT ?)`},
			{"bootstrap codes", `
				DELETE FROM iam_bootstrap_codes WHERE rowid IN (
					SELECT rowid FROM iam_bootstrap_codes
					WHERE bucket = ? AND redeemed_at = 0
					  AND expires_at > 0 AND expires_at < ?
					LIMIT ?)`},
		} {
			result, err := tx.ExecContext(ctx, collect.sql, bucket, expired,
				MaxSweepRows)
			if err != nil {
				return int(written), fmt.Errorf("iamdomain: collect %s in "+
					"bucket %d: %w", collect.what, bucket, err)
			}
			n, _ := result.RowsAffected()
			written += n
		}
	}
	return int(written), nil
}
