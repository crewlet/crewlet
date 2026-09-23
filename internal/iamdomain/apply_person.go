package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE PERSON'S OWN SUBJECT, and the five ops that arbitrate on it.
//
// Everything about somebody that is not a CLAIM contends here — their content,
// their stage, their revocation epoch and their removal — so two administrators
// editing one person contend and two editing two people never do.

// applyPerson dispatches the ops on [KindPerson].
func (a *Applier) applyPerson(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	id := at.record.Subject.ID
	switch at.record.Op {
	case OpEnrol, OpUpdate:
		return a.writePerson(ctx, tx, at, id)
	case OpStatus:
		return a.writeStage(ctx, tx, at, id)
	case OpRevoke:
		return a.writeRevocation(ctx, tx, at, id)
	case OpRemove:
		return a.writeRemoval(ctx, tx, at, id)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a person, "+
		"which this build has no case for", at.position, at.record.Op)
}

// writePerson writes the person's own row and their credentials, as FULL
// POST-STATE.
//
// ONE RECORD, TWO TABLES, and the credential rows are REPLACED rather than
// merged: the payload is the complete set, so a credential absent from it is
// one the writer removed. A merge would make deleting a credential impossible
// to express and would leave a revoked machine token live for ever.
func (a *Applier) writePerson(ctx context.Context, tx *sql.Tx, at applyContext,
	id string) (int, error) {

	person, err := DecodePerson(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the person record at %s: %w",
			at.position, err)
	}
	document, err := EncodePerson(person)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the person at %s: %w",
			at.position, err)
	}

	// THE VERSION GUARD IS A SKIP, not an error: a redelivered record is
	// ordinary traffic, and a constraint violation inside this transaction
	// would abort it identically on every node and stall the fleet's log.
	//
	// `created_at` IS NOT TOUCHED ON CONFLICT, which is what makes a
	// replay of the whole log reproduce the instant somebody was enrolled
	// rather than the instant of their last edit.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_people
			(id, kind, stage, login, email_blind, seat_id,
			 name_sealed, email_sealed, bucket,
			 created_at, updated_at, version, scoped_through, document)
		VALUES (?, ?, ?, '', '', '', ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind         = excluded.kind,
			stage        = excluded.stage,
			name_sealed  = excluded.name_sealed,
			email_sealed = excluded.email_sealed,
			updated_at   = excluded.updated_at,
			version      = excluded.version,
			document     = excluded.document
		WHERE excluded.version > iam_people.version`,
		id, string(person.Kind), string(person.Stage),
		[]byte(person.NameSealed), []byte(person.EmailSealed),
		at.bucket(), at.unix(), at.unix(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write person %s: %w", id, err)
	}
	written, _ := result.RowsAffected()
	if written > 0 {
		// A CONTENT RECORD STATES A STAGE, so a person's standing may
		// have moved with it — an enrolment arriving on a reservation
		// that already holds a seat is the ordinary case.
		a.directoryMoved = true
	}

	// THE CLAIM COLUMNS ARE NOT TOUCHED HERE, and that is the one thing
	// this statement deliberately leaves out. A claim arrives on its own
	// subject, so a content record that wrote one would be a record
	// overwriting a decision another record arbitrated — the same rule the
	// org chart states as "a content apply must never touch the structure
	// columns", in a domain where the columns are somebody's identity.

	credentials, err := a.writeCredentials(ctx, tx, at, id, person.Credentials)
	return int(written) + credentials, err
}

// writeCredentials replaces a person's credential rows from the payload.
//
// EVERY ROW BUT A PROVIDER LINK. A link is its claim's row and nobody else's
// ([KindLink]): the delete leaves it where it is, and an oidc entry a payload
// carries is skipped rather than written. SKIPPED, NOT REFUSED — the writer
// refuses such a document ([authoredPerson]), and a record that reached the
// log anyway is one every node holds identically, so failing its apply would
// stop the log everywhere over a row this apply was never going to own.
func (a *Applier) writeCredentials(ctx context.Context, tx *sql.Tx,
	at applyContext, id string, credentials []Credential) (int, error) {

	// THE DELETE IS GUARDED BY THE VERSION TOO, through the person's row:
	// a redelivered record that skipped the upsert above must not then
	// delete the credentials a newer record wrote. Reading the person's
	// committed version back is what makes the pair one decision.
	var live int64
	err := tx.QueryRowContext(ctx,
		`SELECT version FROM iam_people WHERE id = ?`, id).Scan(&live)
	switch {
	case err != nil && !errorsIsNoRows(err):
		return 0, fmt.Errorf("iamdomain: read person %s's version: %w", id, err)
	case errorsIsNoRows(err):
		// NO PERSON ROW MEANS THE UPSERT WAS SKIPPED BY THE GUARD OR THE
		// person was removed between the two statements. Either way this
		// record's credentials belong to a state that is not current.
		return 0, nil
	case live > at.packed:
		return 0, nil
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM iam_credentials WHERE person_id = ? AND method <> ?`,
		id, string(MethodOIDC))
	if err != nil {
		return 0, fmt.Errorf("iamdomain: clear person %s's credentials: %w", id, err)
	}
	removed, _ := result.RowsAffected()

	written := int(removed)
	for _, credential := range credentials {
		if credential.Method == MethodOIDC {
			continue
		}
		document, err := EncodeCredential(credential)
		if err != nil {
			return written, fmt.Errorf("iamdomain: encode a credential for "+
				"%s: %w", id, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO iam_credentials
				(id, person_id, method, verifier, subject_blind,
				 expires_at, revoked_at, bucket, created_at, version, document)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			credential.ID, id, string(credential.Method),
			[]byte(credential.Verifier), credential.SubjectBlind,
			millis(credential.ExpiresAt), millis(credential.RevokedAt),
			at.bucket(), at.unix(), at.packed, document)
		if err != nil {
			return written, fmt.Errorf("iamdomain: write a credential for "+
				"%s: %w", id, err)
		}
		written++
	}
	return written, nil
}

// writeStage moves a person between enrolment stages.
//
// ITS OWN OP AND ITS OWN NARROW WRITE, so suspending somebody does not carry
// their whole document — which matters because the op an operator reads the
// log for is this one, and a full post-state would make every suspension look
// like an edit.
//
// # The stage lives in two places on the row, and this moves BOTH
//
// The `stage` column is what every predicate reads — the stage index, the
// seat-holder checks, the directory listing — and the document is the full
// post-state a content record authors and a read-modify-write re-reads. This
// op used to move the column alone, so the document went on saying `active`
// after a suspension: every reader that decided from the document signed the
// suspended person in and validated their sessions, and the next edit of their
// grants re-published the document's `active` and quietly reactivated them.
// One fact in two places is only safe while every writer moves both.
//
// The document is re-derived from the row this transaction reads and the stage
// the record states, which is still a pure function of the log: every node
// holds the same row at this position and writes the same bytes.
func (a *Applier) writeStage(ctx context.Context, tx *sql.Tx, at applyContext,
	id string) (int, error) {

	change, err := DecodeStatus(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the status record at %s: %w",
			at.position, err)
	}
	var document []byte
	err = tx.QueryRowContext(ctx, `
		SELECT document FROM iam_people WHERE id = ? AND version < ?`,
		id, at.packed).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOBODY TO MOVE, or a record this row is already past — a
		// redelivery, or a status a later content record superseded. The
		// guard is a SKIP, for the reason every guard here is.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read person %s to move their stage: %w",
			id, err)
	}
	restaged, err := restage(document, change.Stage)
	if err != nil {
		// A DOCUMENT THIS BUILD CANNOT OPEN IS LEFT AS IT IS and the
		// column still moves. Failing here would stall the log on every
		// node holding the same bytes, over a stage the column already
		// states — and the column is what every reader decides from, and
		// what [heldPerson] hands a read-modify-write, so the stale copy
		// can neither admit the person nor be re-published over them.
		restaged = document
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE iam_people
		SET stage = ?, document = ?, updated_at = ?, version = ?
		WHERE id = ? AND version < ?`,
		string(change.Stage), restaged, at.unix(), at.packed, id, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: set person %s's stage: %w", id, err)
	}
	written, _ := result.RowsAffected()
	if written > 0 {
		a.directoryMoved = true
	}
	return int(written), nil
}

// restage is a stored person document with its stage replaced.
//
// AN EMPTY DOCUMENT IS A RESERVATION — the half of an enrolment a claim
// writes before the content record fills it in — and stays empty: there is
// no post-state to amend, and the content record that follows states its own
// stage.
func restage(document []byte, stage iam.Stage) ([]byte, error) {
	if len(document) == 0 {
		return document, nil
	}
	person, err := DecodePerson(document)
	if err != nil {
		return nil, err
	}
	person.Stage = stage
	return EncodePerson(person)
}

// writeRevocation bumps a person's revocation epoch, which ends every session
// they hold, everywhere, at once.
//
// THE NEW EPOCH IS STATED, never incremented here. An applier that did `epoch
// + 1` would be folding over an arrival order, and two nodes at one checkpoint
// have seen the same set in a different order — so the one write in this
// domain that must be identical everywhere would be the one that is not.
func (a *Applier) writeRevocation(ctx context.Context, tx *sql.Tx,
	at applyContext, id string) (int, error) {

	revocation, err := DecodeRevocation(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the revocation record at %s: %w",
			at.position, err)
	}
	// THE GUARD IS THE EPOCH ITSELF AND NOT THE VERSION, which is the one
	// place this domain guards on a payload value. An epoch is MONOTONE by
	// its own meaning — it only ever ends more sessions — so a redelivered
	// or reordered lower epoch must not take effect, and comparing versions
	// would let one that arrived later win.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_revocation_epochs
			(person_id, epoch, reason, bumped_at, bucket, version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET
			epoch     = excluded.epoch,
			reason    = excluded.reason,
			bumped_at = excluded.bumped_at,
			version   = excluded.version
		WHERE excluded.epoch > iam_revocation_epochs.epoch`,
		id, int64(revocation.Epoch), at.record.Reason, at.unix(),
		at.bucket(), at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: bump person %s's epoch: %w", id, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// writeRemoval is the one operation here with no inverse.
//
// IT DELETES THE ROWS AND LEAVES A TOMBSTONE, and the tombstone is what makes
// it permanent: without it a redelivery of any earlier record about this
// person would write them back, on one node, for ever — and in this domain
// that is somebody the company off-boarded still signing in.
//
// THE CLAIMS ARE RELEASED IN THE SAME STATEMENT LIST rather than by a cascade,
// because a cascade is a delete nobody committed: the deletion is part of this
// record's own effect and is therefore identical on every node.
//
// AND THE KEY IS DESTROYED AFTER THE COMMIT, not here. Destroying it inside
// the transaction would destroy it for a removal that then rolled back, and
// nothing could put it back.
func (a *Applier) writeRemoval(ctx context.Context, tx *sql.Tx, at applyContext,
	id string) (int, error) {

	removal, err := DecodeRemoval(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the removal record at %s: %w",
			at.position, err)
	}
	claims, err := json.Marshal(removal.Released)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: encode person %s's released claims: %w",
			id, err)
	}

	// THE TOMBSTONE FIRST, so a transaction that fails partway leaves the
	// person present rather than deleted-and-reclaimable. It is keyed on
	// the person and carries the record's OP ID rather than its position,
	// because the apply that writes it is the one that must be able to run
	// twice and a position changes under a republish.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_removed
			(person_id, at, record_id, actor, actor_kind, reason,
			 claims_json, bucket, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO NOTHING`,
		id, at.unix(), at.record.OpID, at.record.Actor,
		string(at.record.ActorKind), at.record.Reason, string(claims),
		at.bucket(), at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: record person %s's removal: %w", id, err)
	}
	written, _ := result.RowsAffected()

	// AND EVERY ROW THAT IS THEIRS. The order is deliberate: the sessions
	// go before the person, so a reader that sees the person gone can
	// never find a live session pointing at them.
	for _, statement := range []struct{ what, sql string }{
		{"sessions", `DELETE FROM iam_sessions WHERE person_id = ?`},
		{"revocation epoch", `DELETE FROM iam_revocation_epochs WHERE person_id = ?`},
		{"credentials", `DELETE FROM iam_credentials WHERE person_id = ?`},
		{"person", `DELETE FROM iam_people WHERE id = ?`},
	} {
		deleted, err := tx.ExecContext(ctx, statement.sql, id)
		if err != nil {
			return int(written), fmt.Errorf("iamdomain: delete person %s's %s: %w",
				id, statement.what, err)
		}
		n, _ := deleted.RowsAffected()
		written += n
	}

	// THE HISTORY IS NOT DELETED, and that is the whole reason the id
	// outlives the person: an authentication trail whose authors evaporate
	// is not an audit trail. What makes the removal real is that their NAME
	// and ADDRESS are unrecoverable — the key, destroyed after the commit.
	a.shred = append(a.shred, id)
	if written > 0 {
		// A REMOVAL RELEASES THE SEAT with every other claim, and the
		// tombstone is what the directory then reads as the seat's
		// standing — see [Reader.SeatHolders].
		a.directoryMoved = true
	}
	return int(written), nil
}

// errorsIsNoRows is `errors.Is(err, sql.ErrNoRows)` under a name the call
// sites read as the question they are asking.
func errorsIsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// millis is an instant as this estate stores it: ZERO for an unset time,
// rather than the epoch's own milliseconds, so "no deadline" and "1 January
// 1970" are different values in a column a predicate ranges over — which
// matters because every expiry predicate here is a range, and an unset
// deadline read as 1970 is a row that expired before it was written.
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
