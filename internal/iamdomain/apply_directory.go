package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// THE DIRECTORY SUBJECT: every record that sets or frees a person's login,
// address or seat, or creates or removes their row (package doc, "Two
// subjects meet on one row, and never on one column").
//
// A directory record's subject names nobody, so its trail row is filed under
// the PERSON it is about, or, for an invitation's issue, under the ADDRESS's
// blind, where a removal's erasure finds every invitation sent there
// ([eraseSealed]) even once its row is swept.

// The trail objects a directory record is filed under.
const (
	historyObjectPerson  = "person"
	historyObjectAddress = "email"
)

// applyDirectory dispatches the ops on [KindDirectory], and names what each
// is about for the trail row [Applier.writeHistory] writes after it.
func (a *Applier) applyDirectory(ctx context.Context, tx *sql.Tx,
	at *applyContext) (int, error) {

	if at.record.Person != "" {
		at.aboutKind, at.aboutID = historyObjectPerson, at.record.Person
	}
	switch at.record.Op {
	case OpEnrol:
		return a.writeEnrolment(ctx, tx, *at)
	case OpInvite:
		invitation, err := DecodeInvitation(at.record.Mutation)
		if err != nil {
			return 0, fmt.Errorf("iamdomain: the invitation record at %s: %w",
				at.position, err)
		}
		at.aboutKind, at.aboutID = historyObjectAddress, invitation.EmailBlind
		at.blind = invitation.EmailBlind
		return a.writeInvitation(ctx, tx, *at, invitation)
	case OpCancel:
		cancelled, err := DecodeCancellation(at.record.Mutation)
		if err != nil {
			return 0, fmt.Errorf("iamdomain: the cancellation record at %s: %w",
				at.position, err)
		}
		// FILED WHERE THE ISSUE WAS, under the address and in its bucket, so
		// an address's invitations are one object's trail and a removal's
		// erasure finds both rows.
		at.aboutKind, at.aboutID = historyObjectAddress, cancelled.EmailBlind
		at.blind = cancelled.EmailBlind
		return a.writeCancellation(ctx, tx, cancelled)
	case OpIdentity:
		return a.writeIdentity(ctx, tx, *at)
	case OpRemove:
		return a.writeRemoval(ctx, tx, *at)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on the "+
		"directory, which this build has no case for", at.position,
		at.record.Op)
}

// writeEnrolment creates a person's row whole, with their first credentials —
// and, for a redemption, spends the invitation in the same transaction.
//
// AN INSERT THAT DOES NOTHING ON AN EXISTING ROW, never an upsert: the decide
// refused an existing person, so meeting one here is this record's
// redelivery. `created_at` is the broker's instant, so a replay reproduces it.
func (a *Applier) writeEnrolment(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	id := at.record.Person
	if id == "" {
		return 0, fmt.Errorf("iamdomain: the enrolment at %s names no person "+
			"— an enrolment is what creates one, so the record must say who",
			at.position)
	}
	enrolled, err := DecodeEnrolled(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the enrolment record at %s: %w",
			at.position, err)
	}
	document, err := EncodePerson(enrolled.Person)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the person at %s: %w",
			at.position, err)
	}
	held := enrolled.Holds
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_people
			(id, kind, stage, login, email_blind, seat_id,
			 name_sealed, email_sealed, bucket,
			 created_at, updated_at, version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		id, string(enrolled.Person.Kind), string(enrolled.Person.Stage),
		held.Login, held.EmailBlind, held.SeatID,
		[]byte(enrolled.Person.NameSealed), []byte(enrolled.Person.EmailSealed),
		at.bucket(), at.unix(), at.unix(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: enrol person %s: %w", id, err)
	}
	written, _ := result.RowsAffected()
	if written == 0 {
		// A REDELIVERY: this record already created the row and all below.
		return 0, nil
	}
	credentials, err := a.writeCredentials(ctx, tx, at, id,
		enrolled.Person.Credentials)
	written += int64(credentials)
	if err != nil {
		return int(written), err
	}
	if enrolled.Invitation != "" {
		// SPENT BY THE RECORD THAT CREATED ITS PERSON, so no node holds the
		// person beside a link that still opens. Guarded by the column, not
		// the version: a second redemption must find it spent wherever it
		// lands.
		spent, err := tx.ExecContext(ctx, spendInvitationStatement,
			at.unix(), id, at.packed, enrolled.Invitation)
		if err != nil {
			return int(written), fmt.Errorf("iamdomain: spend invitation %s: %w",
				enrolled.Invitation, err)
		}
		n, _ := spent.RowsAffected()
		written += n
	}
	if held.SeatID != "" {
		stamped, err := a.reboundBy(ctx, tx, at, held.SeatID)
		written += stamped
		if err != nil {
			return int(written), err
		}
	}
	// A NEW PERSON moves their seat's standing, and every credential acting
	// as their login is decided again: a Tier A token's `token:<id>` row
	// binds the moment it lands.
	a.moved.Seats = true
	a.moved.person(id, held.Login)
	return int(written), nil
}

// spendInvitationStatement marks one invitation redeemed by the person its
// redemption created, by the invitation's key, and only while it is unspent.
const spendInvitationStatement = `
	UPDATE iam_invites
	SET redeemed_at = ?, person_id = ?, version = ?
	WHERE id = ? AND redeemed_at = 0`

// The two statements [Applier.writeIdentity] runs, each by the person's key
// under the version guard: what the row held, and what the record states.
const (
	identityBeforeStatement = `
		SELECT login, seat_id FROM iam_people WHERE id = ? AND version < ?`
	writeIdentityStatement = `
		UPDATE iam_people
		SET login = ?, seat_id = ?, updated_at = ?, version = ?
		WHERE id = ? AND version < ?`
)

// writeIdentity sets an enrolled person's login and seat to what the record
// states.
//
// THE SAME STATEMENT FREES THE OLD VALUES and sets the new, so a move is one
// record; a credential bound through either login changes standing, so the
// move names both.
func (a *Applier) writeIdentity(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	id := at.record.Person
	change, err := DecodeIdentity(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the identity record at %s: %w",
			at.position, err)
	}
	var login, seat string
	err = tx.QueryRowContext(ctx, identityBeforeStatement,
		id, at.packed).Scan(&login, &seat)
	switch {
	case errorsIsNoRows(err):
		// NOBODY TO CHANGE, or a redelivery this row is already past:
		// skipped, as every guard here is ([Applier]).
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read person %s to change their "+
			"login or seat: %w", id, err)
	}
	result, err := tx.ExecContext(ctx, writeIdentityStatement,
		change.Login, change.SeatID, at.unix(), at.packed, id, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: change person %s's login or seat: %w",
			id, err)
	}
	written, _ := result.RowsAffected()
	if written == 0 {
		return 0, nil
	}
	a.moved.person(id, login)
	a.moved.person(id, change.Login)
	if change.SeatID != seat {
		// A BIND OR AN UNBIND puts a person between the seat and its
		// contact routing, or takes them out of it.
		a.moved.Seats = true
		if change.SeatID != "" {
			stamped, err := a.reboundBy(ctx, tx, at, change.SeatID)
			written += stamped
			if err != nil {
				return int(written), err
			}
		}
	}
	return int(written), nil
}

// reboundBy ends the say of every removal that released seat: a tombstone
// withholds its seat because the contact map still names the leaver, and once
// a bind lands the seat's standing is no longer about the leaver, whoever
// holds it later ([Reader.SeatHolders]).
//
// It stamps the FIRST bind's op id, which the guard makes the first; every
// node stamps the same one because a removal is a gate no node defers, so each
// holds the tombstone before any later bind.
func (a *Applier) reboundBy(ctx context.Context, tx *sql.Tx, at applyContext,
	seat string) (int64, error) {

	result, err := tx.ExecContext(ctx, `
		UPDATE iam_removed SET seat_rebound_by = ? WHERE `+reboundRemovals,
		at.record.OpID, seat)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: end the removals' say over seat %s: %w",
			seat, err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// writeInvitation records an address and a seat spoken for by somebody who has
// no person yet.
//
// THE BUCKET IS THE ADDRESS'S: with no person yet, a bucket from an empty id
// would put every open invitation in one sweep transaction.
//
// THE SEAT IS A COLUMN beside the blind, so a decide's hold ([heldByInvitation])
// and the seat listing are a column lookup, not a decode of every open
// invitation. It holds the document's value whatever that is: the writer, not
// the applier, refuses an invitation naming no seat (package doc, "Every
// person holds a HUMAN seat for as long as they are here").
// [Applier.Rederive] recomputes it.
func (a *Applier) writeInvitation(ctx context.Context, tx *sql.Tx,
	at applyContext, invitation Invitation) (int, error) {

	document, err := EncodeInvitation(invitation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the invitation at %s: %w",
			at.position, err)
	}
	grants, err := encodeGrants(invitation.Grants)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: encode an invitation's grants: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_invites
			(id, email_blind, email_sealed, invited_by, grants_json, seat_id,
			 expires_at, redeemed_at, person_id, bucket, created_at, version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			email_blind  = excluded.email_blind,
			email_sealed = excluded.email_sealed,
			invited_by   = excluded.invited_by,
			grants_json  = excluded.grants_json,
			seat_id      = excluded.seat_id,
			expires_at   = excluded.expires_at,
			version      = excluded.version,
			document     = excluded.document
		WHERE excluded.version > iam_invites.version`,
		invitation.ID, invitation.EmailBlind, []byte(invitation.Sealed),
		invitation.InvitedBy, grants, invitation.Seat,
		millis(invitation.ExpiresAt),
		at.bucket(), at.unix(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write an invitation: %w", err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// writeCancellation deletes an invitation nobody redeemed.
//
// A DELETE, NOT A MARK: the row is what holds the address and seat
// ([heldByInvitation]), its sealed copy goes with it, and a link to it must
// then look like one nobody issued. A redeemed invitation stays until the
// sweep. NO TOMBSTONE: the log applies in order, so the issue always precedes
// it, and the framework passes a redelivery below its checkpoint unapplied.
func (a *Applier) writeCancellation(ctx context.Context, tx *sql.Tx,
	cancelled Cancellation) (int, error) {

	result, err := tx.ExecContext(ctx, cancelInvitationStatement,
		cancelled.Invitation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: cancel invitation %s: %w",
			cancelled.Invitation, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// cancelInvitationStatement deletes one unredeemed invitation by its key.
const cancelInvitationStatement = `DELETE FROM iam_invites WHERE id = ? AND redeemed_at = 0`

// removePersonStatement deletes one person's row by its key — the last of the
// rows a removal deletes ([Applier.writeRemoval]).
const removePersonStatement = `DELETE FROM iam_people WHERE id = ?`

// writeRemoval is the one operation here with no inverse.
//
// IT LEAVES A TOMBSTONE, which makes it permanent: without one a redelivered
// earlier record would write an off-boarded person back, able to sign in.
// Identifiers are released by these statements, not a cascade, so the deletion
// is this record's own effect, identical on every node. Every sealed value of
// theirs is erased in the same transaction, from the rows that outlive them
// too ([eraseSealed]).
func (a *Applier) writeRemoval(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	id := at.record.Person
	if id == "" {
		return 0, fmt.Errorf("iamdomain: the removal at %s names no person",
			at.position)
	}
	removal, err := DecodeRemoval(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the removal record at %s: %w",
			at.position, err)
	}
	released, err := json.Marshal(removal.Released)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: encode person %s's released "+
			"identifiers: %w", id, err)
	}
	// THE LOGIN THEY HOLD, read before its row goes: a credential bound
	// through it is bound through nothing afterwards.
	login, err := heldLogin(ctx, tx, id)
	if err != nil {
		return 0, err
	}

	// THE TOMBSTONE FIRST, so a transaction failing partway leaves the person
	// present rather than deleted-and-reusable. It carries the record's OP ID,
	// not its position, which a republish changes ([Applier.Gated]). The
	// released seat is also a column (migration 0122), so one seat's standing
	// is a seek ([Reader.Requester], [reboundRemovals]), not a decode of every
	// tombstone's claims; [Applier.Rederive] recomputes it.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_removed
			(person_id, at, record_id, actor, actor_kind, reason,
			 claims_json, seat_id, bucket, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO NOTHING`,
		id, at.unix(), at.record.OpID, at.record.Actor,
		string(at.record.ActorKind), at.record.Reason, string(released),
		removal.Released.SeatID, at.bucket(), at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: record person %s's removal: %w", id, err)
	}
	written, _ := result.RowsAffected()

	// SESSIONS BEFORE THE PERSON, so a reader that sees the person gone
	// never finds a live session pointing at them.
	for _, statement := range []struct{ what, sql string }{
		{"sessions", `DELETE FROM iam_sessions WHERE person_id = ?`},
		{"revocation epoch", `DELETE FROM iam_revocation_epochs WHERE person_id = ?`},
		{"credentials", `DELETE FROM iam_credentials WHERE person_id = ?`},
		{"person", removePersonStatement},
	} {
		deleted, execErr := tx.ExecContext(ctx, statement.sql, id)
		if execErr != nil {
			return int(written), fmt.Errorf("iamdomain: delete person %s's %s: %w",
				id, statement.what, execErr)
		}
		n, _ := deleted.RowsAffected()
		written += n
	}

	// THE TRAIL IS KEPT, which is why the id outlives the person: an audit
	// trail whose authors evaporate is none. Their NAME, ADDRESS and SEEDS
	// are erased from every row instead, here.
	erased, err := eraseSealed(ctx, tx, id, removal.Released.EmailBlind)
	written += int64(erased)
	if err != nil {
		return int(written), fmt.Errorf("iamdomain: erase person %s's "+
			"sealed values: %w", id, err)
	}
	if written > 0 {
		// A REMOVAL RELEASES THE SEAT, whose standing the tombstone then is
		// ([Reader.SeatHolders]), and ends every credential they held.
		a.moved.Seats = true
		a.moved.person(id, login)
		a.moved.removed(id)
	}
	return int(written), nil
}

// reboundRemovals selects the removal tombstones a bind of one seat speaks
// for: every removal that released the seat and that no bind since has.
//
// ONE CLAUSE FOR BOTH READERS: the bind's apply, which stamps them, and its
// writer, which declares their people's buckets in the scope ([leaversOf]). A
// tombstone is in the LEAVER's bucket, so a scope naming only the bound person
// would not wait behind a deferred earlier record about the leaver.
//
// ONE SEEK: `iam_removed_seat_idx` (0122) is `(seat_id, seat_rebound_by,
// person_id)`, so the clause is its prefix and [leaversOf]'s order its third
// column.
const reboundRemovals = `seat_rebound_by = ''
	AND seat_id = ?`

// leaversStatement is [leaversOf]'s read: the people [reboundRemovals] names,
// in the order of the index's third column.
const leaversStatement = `SELECT person_id FROM iam_removed WHERE ` + reboundRemovals + `
	ORDER BY person_id`

// leaversOf is the people whose removal tombstones a bind of seatID would
// stamp ([reboundRemovals]), sorted.
func leaversOf(ctx context.Context, tx *sql.Tx, seatID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, leaversStatement, seatID)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read the removals that released "+
			"seat %s: %w", seatID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var person string
		if err := rows.Scan(&person); err != nil {
			return nil, fmt.Errorf("iamdomain: read the removals that released "+
				"seat %s: %w", seatID, err)
		}
		out = append(out, person)
	}
	return out, rows.Err()
}
