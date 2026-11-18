package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE PERSON'S OWN SUBJECT, and the four ops that arbitrate on it: everything
// about somebody that is not unique (content, stage, revocation epoch, and a
// password change moving content and epoch at once), so editing one person
// contends and editing two never does. The row is created and removed on the
// directory ([Applier.applyDirectory]), never here.

// applyPerson dispatches the ops on [KindPerson].
func (a *Applier) applyPerson(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	id := at.record.Subject.ID
	switch at.record.Op {
	case OpUpdate:
		return a.writePerson(ctx, tx, at, id)
	case OpStatus:
		return a.writeStage(ctx, tx, at, id)
	case OpRevoke:
		return a.writeRevocation(ctx, tx, at, id)
	case OpPassword:
		return a.writePassword(ctx, tx, at, id)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a person, "+
		"which this build has no case for", at.position, at.record.Op)
}

// writePerson writes the person's own document and their credentials, as FULL
// POST-STATE, onto the row the directory created.
//
// The credential rows are REPLACED, not merged: the payload is the complete
// set, and a merge could not express deleting a credential, leaving a revoked
// machine token live for ever.
//
// AN UPDATE, NEVER AN INSERT: only the directory creates a person, so a record
// for an id with no row writes nothing.
func (a *Applier) writePerson(ctx context.Context, tx *sql.Tx, at applyContext,
	id string) (int, error) {

	person, err := DecodePerson(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the person record at %s: %w",
			at.position, err)
	}
	return a.writeDocument(ctx, tx, at, id, person)
}

// writePassword writes a password change: the person's document, as a content
// record writes it, and the stated epoch, as a revocation bumps it, from one
// record so a node holds both or neither.
func (a *Applier) writePassword(ctx context.Context, tx *sql.Tx, at applyContext,
	id string) (int, error) {

	change, err := DecodePasswordChange(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the password record at %s: %w",
			at.position, err)
	}
	written, err := a.writeDocument(ctx, tx, at, id, change.Person)
	if err != nil {
		return written, err
	}
	bumped, err := a.bumpEpoch(ctx, tx, at, id, change.Epoch)
	return written + bumped, err
}

// writeDocument writes one person's document and credentials onto the row the
// directory created: what a content record and a password change share.
func (a *Applier) writeDocument(ctx context.Context, tx *sql.Tx, at applyContext,
	id string, person Person) (int, error) {

	document, err := EncodePerson(person)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the person at %s: %w",
			at.position, err)
	}

	// THE VERSION GUARD IS A SKIP, not an error: a redelivery is ordinary
	// traffic, and a constraint violation would abort this transaction
	// identically on every data node and stall the log.
	//
	// The directory's unique columns are left out: they are somebody's
	// identity, a decision only the directory makes.
	result, err := tx.ExecContext(ctx, writePersonStatement,
		string(person.Kind), string(person.Stage),
		[]byte(person.NameSealed), []byte(person.EmailSealed),
		at.unix(), at.packed, document, id, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write person %s: %w", id, err)
	}
	written, _ := result.RowsAffected()
	if written == 0 {
		// NOBODY TO WRITE, or a record this row is already past: skip, as
		// the credentials below belong to a state that is not current.
		return 0, nil
	}
	// A CONTENT RECORD STATES A STAGE, so the person's standing may have moved.
	a.moved.Seats = true

	credentials, err := a.writeCredentials(ctx, tx, at, id, person.Credentials)
	if err != nil {
		return int(written) + credentials, err
	}
	// Every credential acting for them is decided again: a session's or
	// machine token's grants are this row's, and a token this record revoked
	// or dropped is ended, named by its owner.
	if err := a.movedPerson(ctx, tx, id); err != nil {
		return int(written) + credentials, err
	}
	return int(written) + credentials, nil
}

// writePersonStatement writes a person's own content to their row, by its key
// under the version guard — every column but the directory's three.
const writePersonStatement = `
	UPDATE iam_people
	SET kind = ?, stage = ?, name_sealed = ?, email_sealed = ?,
	    updated_at = ?, version = ?, document = ?
	WHERE id = ? AND version < ?`

// personVersionStatement is the version one person's row stands at, by its
// key: what [Applier.writeCredentials] guards its delete by.
const personVersionStatement = `SELECT version FROM iam_people WHERE id = ?`

// writeCredentials replaces a person's credential rows from the payload.
func (a *Applier) writeCredentials(ctx context.Context, tx *sql.Tx,
	at applyContext, id string, credentials []Credential) (int, error) {

	// THE DELETE IS GUARDED BY THE VERSION TOO, read back from the person's
	// row, so a redelivered record whose update was skipped cannot delete the
	// credentials a newer record wrote.
	var live int64
	err := tx.QueryRowContext(ctx, personVersionStatement, id).Scan(&live)
	switch {
	case err != nil && !errorsIsNoRows(err):
		return 0, fmt.Errorf("iamdomain: read person %s's version: %w", id, err)
	case errorsIsNoRows(err):
		// No person row: this record's credentials belong to a state that is
		// not current.
		return 0, nil
	case live > at.packed:
		return 0, nil
	}

	// A CREDENTIAL KEEPS THE INSTANT IT WAS CREATED: every record restates all
	// of a person's credentials and the document holds no creation time, so one
	// the set already held keeps its row's and only a new one takes this
	// record's. Restamping all would make every change to a person read as
	// creating every credential they hold.
	created, err := credentialsCreated(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx,
		`DELETE FROM iam_credentials WHERE person_id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: clear person %s's credentials: %w", id, err)
	}
	removed, _ := result.RowsAffected()

	written := int(removed)
	for _, credential := range credentials {
		document, err := EncodeCredential(credential)
		if err != nil {
			return written, fmt.Errorf("iamdomain: encode a credential for "+
				"%s: %w", id, err)
		}
		createdAt, held := created[credential.ID]
		if !held {
			createdAt = at.unix()
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO iam_credentials
				(id, person_id, method, verifier,
				 expires_at, revoked_at, bucket, created_at, version, document)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			credential.ID, id, string(credential.Method),
			[]byte(credential.Verifier),
			millis(credential.ExpiresAt), millis(credential.RevokedAt),
			at.bucket(), createdAt, at.packed, document)
		if err != nil {
			return written, fmt.Errorf("iamdomain: write a credential for "+
				"%s: %w", id, err)
		}
		written++
	}
	return written, nil
}

// credentialsCreated is when each credential a person's rows hold was
// created, by id.
func credentialsCreated(ctx context.Context, tx *sql.Tx, id string) (
	map[string]int64, error) {

	rows, err := tx.QueryContext(ctx,
		`SELECT id, created_at FROM iam_credentials WHERE person_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read person %s's credentials: %w", id, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var (
			credential string
			at         int64
		)
		if err := rows.Scan(&credential, &at); err != nil {
			return nil, fmt.Errorf("iamdomain: scan person %s's credential: %w",
				id, err)
		}
		out[credential] = at
	}
	return out, rows.Err()
}

// The two statements [Applier.writeStage] runs, each by the person's key under
// the version guard: the document the row held, and the stage it moves to.
const (
	stageBeforeStatement = `
		SELECT document FROM iam_people WHERE id = ? AND version < ?`
	writeStageStatement = `
		UPDATE iam_people
		SET stage = ?, document = ?, updated_at = ?, version = ?
		WHERE id = ? AND version < ?`
)

// writeStage moves a person between enrolment stages.
//
// ITS OWN OP AND NARROW WRITE, so a suspension does not carry the whole
// document and does not read as an edit in the log.
//
// It moves BOTH places the stage lives: the `stage` column every predicate
// reads (the stage index, seat-holder checks, the directory listing) and the
// document a read-modify-write re-reads. A document left saying `active`
// signs a suspended person in, and the next grant edit re-publishes it,
// reactivating them. The document is re-derived from the row this
// transaction reads and the stated stage, so it stays a pure function of the
// log.
func (a *Applier) writeStage(ctx context.Context, tx *sql.Tx, at applyContext,
	id string) (int, error) {

	change, err := DecodeStatus(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the status record at %s: %w",
			at.position, err)
	}
	var document []byte
	err = tx.QueryRowContext(ctx, stageBeforeStatement,
		id, at.packed).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOBODY TO MOVE, or a record this row is already past (a redelivery,
		// or a status a later content record superseded): skip.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read person %s to move their stage: %w",
			id, err)
	}
	restaged, err := restage(document, change.Stage)
	if err != nil {
		// A DOCUMENT THIS BUILD CANNOT OPEN IS LEFT AS IT IS while the column
		// moves: failing would stall the log on every node, and the column is
		// what every reader and [heldPerson] decide from, so the stale copy
		// can neither admit the person nor be re-published over them.
		restaged = document
	}
	result, err := tx.ExecContext(ctx, writeStageStatement,
		string(change.Stage), restaged, at.unix(), at.packed, id, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: set person %s's stage: %w", id, err)
	}
	written, _ := result.RowsAffected()
	if written > 0 {
		// A stage other than `active` refuses every credential acting for
		// them; `active` admits them again.
		a.moved.Seats = true
		if err = a.movedPerson(ctx, tx, id); err != nil {
			return int(written), err
		}
	}
	if change.Epoch == 0 {
		return int(written), nil
	}
	// AND ENDS THEM whether or not the stage was this record's to write: an
	// epoch only moves forward, so a bump is never stale.
	bumped, err := a.bumpEpoch(ctx, tx, at, id, change.Epoch)
	return int(written) + bumped, err
}

// restage is a stored person document with its stage replaced.
func restage(document []byte, stage iam.Stage) ([]byte, error) {
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
// THE NEW EPOCH IS STATED, never incremented here: `epoch + 1` would fold over
// arrival order, which differs between nodes at one checkpoint, so the write
// that must be identical everywhere would not be.
func (a *Applier) writeRevocation(ctx context.Context, tx *sql.Tx,
	at applyContext, id string) (int, error) {

	revocation, err := DecodeRevocation(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the revocation record at %s: %w",
			at.position, err)
	}
	return a.bumpEpoch(ctx, tx, at, id, revocation.Epoch)
}

// bumpEpoch moves one person's revocation epoch to the value a record states:
// what a revocation and a password change share.
func (a *Applier) bumpEpoch(ctx context.Context, tx *sql.Tx, at applyContext,
	id string, epoch uint64) (int, error) {

	// THE EPOCH THIS MOVE LEAVES, read before it is replaced: a session at or
	// above it is one this move ends; one below, an earlier move ended.
	previous, err := epochOf(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	// THE GUARD IS THE EPOCH, NOT THE VERSION (this domain's one guard on a
	// payload value): an epoch is monotone, only ever ending more sessions, so
	// a redelivered or reordered lower epoch must not take effect, which a
	// version comparison would allow.
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
		id, int64(epoch), at.record.Reason, at.unix(),
		at.bucket(), at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: bump person %s's epoch: %w", id, err)
	}
	written, _ := result.RowsAffected()
	if written == 0 {
		return 0, nil
	}
	// Every session and machine token they hold is over.
	if err = a.movedPerson(ctx, tx, id); err != nil {
		return int(written), err
	}
	// EACH SESSION IT ENDED SAYS SO ON ITS OWN ROW, with this record's reason
	// and instant: the epoch's row keeps only its LAST move, so deriving the
	// why from it would describe every session an earlier move ended as the
	// last. The guard `ended_at = 0`, as a sign-out's, keeps a later move
	// from rewriting it.
	//
	// ONLY ON A SESSION LIVE AT THIS MOVE (at or above the epoch it leaves):
	// `ended_at = 0` also holds for a session an earlier move ended without
	// stamping, such as a start decided before that move whose record landed
	// after it, which this move would claim for good. Those, and sessions the
	// company's generation ended, say what the counters can ([Reader.Sessions]).
	ended, err := tx.ExecContext(ctx, `
		UPDATE iam_sessions
		SET ended_at = ?, ended_reason = ?, version = ?
		WHERE person_id = ? AND ended_at = 0 AND epoch >= ? AND epoch < ?
		  AND start_position >= COALESCE((SELECT version
		      FROM iam_session_generation WHERE singleton = 0), 0)`,
		at.unix(), at.record.Reason, at.packed, id, int64(previous), int64(epoch))
	if err != nil {
		return int(written), fmt.Errorf("iamdomain: end person %s's sessions: %w",
			id, err)
	}
	closed, _ := ended.RowsAffected()
	return int(written + closed), nil
}

// errorsIsNoRows is `errors.Is(err, sql.ErrNoRows)` under a name the call
// sites read as the question they are asking.
func errorsIsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// millis is an instant as this estate stores it: ZERO for an unset time, not
// the epoch's milliseconds, because every expiry predicate is a range and an
// unset deadline read as 1970 would have expired before it was written.
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
