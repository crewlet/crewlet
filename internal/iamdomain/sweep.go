package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RETENTION SWEEP is a RECORD rather than a local delete: these rows are
// identity-claimed (N nodes assert byte-identical tables), and a node sweeping on
// its own clock would turn that into a claim about clock synchronisation that
// nothing checks. So the publisher resolves each horizon to a POSITION once, and
// every data node deletes exactly the same rows.
//
// One record per bucket, not one for the estate: sixty-four records spread a
// tick's deletes across sixty-four transactions rather than one statement holding
// this store's only writer. [MaxSweepRows] still bounds each, since a bucket
// unswept for a month holds a month's rows.

// MaxSweepRows is how many rows one sweep record's apply may delete:
// [statelog.ApplyTxRowBudget], the per-transaction budget the applier is sized
// for, since a sweep past it would hold this store's only writer while every
// other domain's apply waits. It caps ONE record; a bucket with more behind it is
// swept again next tick, and converges.
const MaxSweepRows = statelog.ApplyTxRowBudget

// sweepTrailStatement deletes one class of one bucket's trail below a position,
// a seek of the index migration 0041 ships for this range delete.
const sweepTrailStatement = `
	DELETE FROM iam_history
	WHERE rowid IN (
		SELECT rowid FROM iam_history
		WHERE bucket = ? AND class = ? AND version < ?
		LIMIT ?)`

// A sweep's two deletes of one bucket's invitations, aged out unredeemed and
// redeemed, each a seek of the partial index 0041 ships for its predicate.
const (
	sweepOpenInvitationsStatement = `
		DELETE FROM iam_invites WHERE rowid IN (
			SELECT rowid FROM iam_invites
			WHERE bucket = ? AND redeemed_at = 0 AND expires_at < ?
			LIMIT ?)`
	sweepSpentInvitationsStatement = `
		DELETE FROM iam_invites WHERE rowid IN (
			SELECT rowid FROM iam_invites
			WHERE bucket = ? AND redeemed_at > 0 AND redeemed_at < ?
			LIMIT ?)`
)

// applySweep deletes one bucket's expired rows below the positions the
// publisher resolved.
//
// ONE BUDGET FOR THE WHOLE RECORD, shared by every statement and spent in order,
// so [MaxSweepRows] bounds the transaction rather than each of its deletes. What
// one record leaves unspent the next one collects.
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
	// spend runs one delete whose LIMIT, its last argument, is what is left of
	// the budget.
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

	// THE TRAIL'S TWO HORIZONS are POSITIONS, not ages. A zero means "no rows
	// old enough yet" and deletes nothing, never everything.
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
		if err := spend("the "+string(horizon.class)+" trail", sweepTrailStatement,
			bucket, string(horizon.class), int64(horizon.before)); err != nil {
			return int(written), err
		}
	}

	// AND THE TABLES WHOSE ROWS ARE OVER RATHER THAN OLD, collected against the
	// ONE instant the record carries, never a node's clock; each subquery seeks
	// its table's index for this predicate within one bucket.
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
			// AN INVITATION WITH NO EXPIRY IS COLLECTED like one that aged out, as
			// every reader reads it so ([InvitationRow.Spent], [openInvitation]);
			// kept, its sealed address would stay in every data node's estate for
			// good. Issuing refuses one, so every build sweeps the same rows.
			{"invitations", sweepOpenInvitationsStatement},
			// WHAT WAS SPENT too: a redeemed invitation is as unpresentable as an
			// expired one, and kept it would leave every invited address, sealed, in
			// every data node's estate.
			{"redeemed invitations", sweepSpentInvitationsStatement},
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
	}
	return int(written), nil
}

// collectCredentials removes one bucket's credentials revoked or expired before
// `expired`, spending at most budget rows.
//
// Credentials travel WHOLE on the person's document and the rows derive from it,
// so a row-only delete would be undone by the next credential write: each
// owner's document is rewritten without them in the same transaction, its
// `version` raised to this record's position so a redelivered earlier record
// cannot write them back. The pick is ordered by (person, credential), so every
// data node takes the same subset, and spent a whole person at a time, so no
// person keeps rows their document no longer lists; only the first may be cut
// short, or one with more due than the budget never would be. A document this
// build cannot read is left alone: failing would stop the log on every node.
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
		// A WHOLE PERSON OR NONE: their rows plus their document. Only the first
		// person may be cut short, so one with more due than the budget is not
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

// The two statements [collectOwned] keeps a person's document in step with
// the credentials a sweep collected by, each by the person's key.
const (
	personDocumentStatement = `SELECT document FROM iam_people WHERE id = ?`
	sweptDocumentStatement  = `
		UPDATE iam_people
		   SET document = ?, version = MAX(version, ?)
		 WHERE id = ?`
)

// collectOwned removes one person's collected credentials from their document
// and their rows.
func collectOwned(ctx context.Context, tx *sql.Tx, at applyContext,
	person string, ids []string) (int64, error) {

	var document []byte
	err := tx.QueryRowContext(ctx, personDocumentStatement, person).Scan(&document)
	var written int64
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// ROWS NOBODY OWNS (a removal deletes a person's credentials with them):
		// there is no document to keep in step.
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
		if _, err := tx.ExecContext(ctx, sweptDocumentStatement,
			rewritten, at.packed, person); err != nil {
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
