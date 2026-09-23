package iamdomain

import (
	"context"
	"database/sql"
	"errors"
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
//
// ONE BUDGET FOR THE WHOLE RECORD, shared by every statement below and spent
// in order. It used to be a LIMIT per statement, which made [MaxSweepRows] a
// bound on each of six deletes rather than on the transaction they share: a
// bucket back from a lapse with every class overdue deleted six budgets' worth
// in one apply, holding this store's only writer six times as long as the
// framework sizes a transaction for. What one record leaves unspent the next
// one collects, which is the convergence the cap already promised.
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
	budget := int64(MaxSweepRows)

	var written int64
	// spend runs one bounded delete against what is left of the budget,
	// which is the LIMIT its subquery is handed as the last argument.
	spend := func(what, query string, args ...any) error {
		if budget <= 0 {
			return nil
		}
		result, err := tx.ExecContext(ctx, query, append(args, budget)...)
		if err != nil {
			return fmt.Errorf("iamdomain: sweep %s in bucket %d: %w", what,
				bucket, err)
		}
		n, _ := result.RowsAffected()
		written += n
		budget -= n
		return nil
	}

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
		if err := spend("the "+string(horizon.class)+" trail", `
			DELETE FROM iam_history
			WHERE rowid IN (
				SELECT rowid FROM iam_history
				WHERE bucket = ? AND class = ? AND version < ?
				LIMIT ?)`,
			bucket, string(horizon.class), int64(horizon.before)); err != nil {
			return int(written), err
		}
	}

	// AND THE THREE TABLES WHOSE ROWS ARE OVER RATHER THAN OLD. Each is
	// collected against the ONE instant the record carries, never a node's
	// own clock, for this file's whole reason. The subquery is over the
	// index each table ships for exactly this predicate, so every statement
	// is a seek into one bucket's range.
	if expired > 0 {
		for _, collect := range []struct {
			what, sql string
		}{
			{"ended sessions", `
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
			if err := spend(collect.what, collect.sql, bucket, expired); err != nil {
				return int(written), err
			}
		}
	}
	if expired <= 0 || at.record.V < SweepRecordVersion {
		// A VERSION-1 SWEEP collects what version 1 said and nothing
		// more — replay meets records written before version 2 existed,
		// and applying them with a clause they never stated would delete
		// rows the nodes that applied them live never deleted.
		return int(written), nil
	}

	// VERSION 2: WHAT WAS SPENT, and not only what lapsed. A redeemed
	// invitation and a redeemed bootstrap code are as unpresentable as
	// expired ones, and version 1 kept them for ever — every address
	// anybody was ever invited at, sealed, in every node's estate.
	for _, collect := range []struct {
		what, sql string
	}{
		{"redeemed invitations", `
			DELETE FROM iam_invites WHERE rowid IN (
				SELECT rowid FROM iam_invites
				WHERE bucket = ? AND redeemed_at > 0 AND redeemed_at < ?
				LIMIT ?)`},
		{"redeemed bootstrap codes", `
			DELETE FROM iam_bootstrap_codes WHERE rowid IN (
				SELECT rowid FROM iam_bootstrap_codes
				WHERE bucket = ? AND redeemed_at > 0 AND redeemed_at < ?
				LIMIT ?)`},
	} {
		if err := spend(collect.what, collect.sql, bucket, expired); err != nil {
			return int(written), err
		}
	}
	if budget > 0 {
		n, err := collectCredentials(ctx, tx, at, bucket, expired, budget)
		written += n
		if err != nil {
			return int(written), err
		}
	}
	return int(written), nil
}

// collectCredentials removes one bucket's credentials that stopped being usable
// before `expired` — revoked, or past their own expiry — spending at most
// budget rows.
//
// # From the document as well as from the rows
//
// A person's credentials are carried WHOLE on their document, and the rows are
// derived from it: every content record rewrites them, and the read-modify-write
// that changes somebody's second factor forms the new set from the document. A
// sweep that deleted only rows would be undone by the next such write, which
// republishes the document's copy. So each owner's document is rewritten
// without the collected credentials, in the same transaction, and stamped with
// this record's position in `scoped_through` rather than `version` — a sweep
// arbitrates on its bucket's subject, not the person's, and the person
// subject's own expectation must stay the last record on it.
//
// # One ordered pick, spent person by person
//
// The pick is ordered by (person, credential), so every node takes the same
// subset under the budget, and it is spent a whole person at a time — the
// person's document and every row collected for them — because a person half
// collected would hold rows its document no longer lists. A person with more
// due than the whole budget is collected in part, first to last, or they could
// never be collected at all.
//
// A DOCUMENT THIS BUILD CANNOT READ IS LEFT ALONE, credentials and all: failing
// the apply would stop this domain's log on every node at once, and the rows it
// derived are the ones every node of this build keeps identically.
func collectCredentials(ctx context.Context, tx *sql.Tx, at applyContext,
	bucket, expired, budget int64) (int64, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, person_id FROM iam_credentials
		 WHERE bucket = ? AND revoked_at > 0 AND revoked_at < ?
		UNION
		SELECT id, person_id FROM iam_credentials
		 WHERE bucket = ? AND expires_at > 0 AND expires_at < ?
		ORDER BY 2, 1
		LIMIT ?`, bucket, expired, bucket, expired, budget)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: sweep credentials in bucket %d: %w",
			bucket, err)
	}
	type owned struct {
		person string
		ids    []string
	}
	var due []owned
	for rows.Next() {
		var id, person string
		if err := rows.Scan(&id, &person); err != nil {
			rows.Close()
			return 0, fmt.Errorf("iamdomain: sweep credentials in bucket %d: %w",
				bucket, err)
		}
		if n := len(due); n > 0 && due[n-1].person == person {
			due[n-1].ids = append(due[n-1].ids, id)
			continue
		}
		due = append(due, owned{person: person, ids: []string{id}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iamdomain: sweep credentials in bucket %d: %w",
			bucket, err)
	}

	var written int64
	for i, owner := range due {
		// A WHOLE PERSON OR NONE: their rows and their document, which
		// is one row more than the credentials. The first person is the
		// exception, so a person with more due than the budget is not
		// stuck for ever.
		cost := int64(len(owner.ids)) + 1
		if remaining := budget - written; cost > remaining {
			if i > 0 || remaining < 2 {
				break
			}
			owner.ids = owner.ids[:remaining-1]
		}
		n, err := collectOwned(ctx, tx, at, owner.person, owner.ids)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// collectOwned removes one person's collected credentials from their document
// and their rows.
func collectOwned(ctx context.Context, tx *sql.Tx, at applyContext,
	person string, ids []string) (int64, error) {

	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM iam_people WHERE id = ?`, person).Scan(&document)
	var written int64
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// ROWS NOBODY OWNS: a removal deletes a person's credentials with
		// them, so there is no document to keep in step — the rows are
		// simply collected.
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read person %s to sweep their "+
			"credentials: %w", person, err)
	default:
		held, err := DecodePerson(document)
		if err != nil {
			applyLog.WarnContext(ctx, "iam_sweep_person_unreadable",
				"person", person, "error", err.Error(),
				"detail", "this person's swept credentials are kept, rows and "+
					"document alike, until a build that reads the document sweeps them")
			return 0, nil
		}
		gone := make(map[string]bool, len(ids))
		for _, id := range ids {
			gone[id] = true
		}
		kept := make([]Credential, 0, len(held.Credentials))
		for _, credential := range held.Credentials {
			if !gone[credential.ID] {
				kept = append(kept, credential)
			}
		}
		held.Credentials = kept
		rewritten, err := EncodePerson(held)
		if err != nil {
			return 0, fmt.Errorf("iamdomain: re-encode person %s without their "+
				"swept credentials: %w", person, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE iam_people
			   SET document = ?, scoped_through = MAX(scoped_through, ?)
			 WHERE id = ?`, rewritten, at.packed, person); err != nil {
			return 0, fmt.Errorf("iamdomain: rewrite person %s without their "+
				"swept credentials: %w", person, err)
		}
		written++
	}
	for _, id := range ids {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM iam_credentials WHERE id = ? AND person_id = ?`, id, person)
		if err != nil {
			return written, fmt.Errorf("iamdomain: sweep credential %s: %w", id, err)
		}
		n, _ := result.RowsAffected()
		written += n
	}
	return written, nil
}
