package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// THE DIRECTORY SUBJECT: every record that sets or frees a person's login,
// address or seat, and every record that creates or removes a person's row.
//
// # The directory owns a row's existence and its three unique columns
//
// An enrolment CREATES the row whole — the document and every column — and is
// the only record that does; an identity change sets the login and the seat; a
// removal deletes the row. A record on the person's own subject owns the
// document and the columns derived from it, and touches none of the three, so
// the two subjects meet on one row and never on one column. Every record about
// one person is applied in log order — a retained record holds back every
// later record its scope meets — so one `version` guards both halves.
//
// # And the trail row names what the record was about
//
// A directory record's subject names nobody, so its trail row is filed under
// the PERSON it is about — or, for an invitation's issue, which is about
// nobody yet, under the ADDRESS's blind, where a removal's erasure finds every
// invitation an address was sent ([eraseSealed]) whether or not the
// invitation's own row has been swept since.

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
// AN INSERT THAT DOES NOTHING ON A ROW THAT EXISTS, never an upsert: the
// record's decide refused a person who already exists, so the only way to meet
// one here is a redelivery of this very record, and the guard against it is
// the row itself.
//
// `created_at` IS THE BROKER'S instant for this record, which is what makes a
// replay of the whole log reproduce the instant somebody was enrolled.
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
		// A REDELIVERY: this record already created the row, and wrote
		// everything below with it.
		return 0, nil
	}
	credentials, err := a.writeCredentials(ctx, tx, at, id,
		enrolled.Person.Credentials)
	written += int64(credentials)
	if err != nil {
		return int(written), err
	}
	if enrolled.Invitation != "" {
		// THE LINK IS SPENT BY THE RECORD THAT CREATED ITS PERSON, so no
		// node ever holds the person and a link that still opens. The
		// guard is the column rather than the version: redeeming twice is
		// not a stale write to skip, and the second must find it spent
		// whatever position it arrived at.
		spent, err := tx.ExecContext(ctx, `
			UPDATE iam_invites
			SET redeemed_at = ?, person_id = ?, version = ?
			WHERE id = ? AND redeemed_at = 0`,
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
	// A NEW PERSON — at whatever stage, holding whatever seat — is a move
	// in that seat's standing, and every credential acting as their login
	// is to be decided again: a Tier A token's `token:<id>` row binds that
	// token the moment it lands.
	a.moved.Seats = true
	a.moved.person(id, held.Login)
	return int(written), nil
}

// writeIdentity sets an enrolled person's login and seat to what the record
// states.
//
// THE OLD VALUES ARE FREED BY THE SAME STATEMENT that sets the new, which is
// what makes a move one record: a credential bound through the login given up
// is bound through nothing once it lands, and one bound through the new one is
// bound from now — so the move names both.
func (a *Applier) writeIdentity(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	id := at.record.Person
	change, err := DecodeIdentity(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the identity record at %s: %w",
			at.position, err)
	}
	var login, seat string
	err = tx.QueryRowContext(ctx, `
		SELECT login, seat_id FROM iam_people WHERE id = ? AND version < ?`,
		id, at.packed).Scan(&login, &seat)
	switch {
	case errorsIsNoRows(err):
		// NOBODY TO CHANGE, or a record this row is already past — a
		// redelivery. The guard is a SKIP, for the reason every guard
		// here is.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read person %s to change their "+
			"login or seat: %w", id, err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE iam_people
		SET login = ?, seat_id = ?, updated_at = ?, version = ?
		WHERE id = ? AND version < ?`,
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

// reboundBy ends the say of every removal that released seat: a removal's
// tombstone withholds the seat it released because the contact map still names
// the leaver, and a bind is a new binding with a standing of its own — once
// one has landed the leaver is no longer what the seat's standing is about,
// however many holders come and go after it. See [Reader.SeatHolders].
//
// The FIRST bind's operation id, and the guard is what makes it the first: a
// removal is a gate no node defers, so every node has its tombstone before any
// later bind, and every node stamps the same id.
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

// writeInvitation records an address spoken for by somebody who has no person
// yet.
//
// THE BUCKET IS THE ADDRESS'S OWN, not a person's: an invitation has no person
// until it is redeemed, and a bucket derived from an empty id would put every
// outstanding invitation in the company into one sweep transaction.
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
			(id, email_blind, email_sealed, invited_by, grants_json,
			 expires_at, redeemed_at, person_id, bucket, created_at, version, document)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			email_blind  = excluded.email_blind,
			email_sealed = excluded.email_sealed,
			invited_by   = excluded.invited_by,
			grants_json  = excluded.grants_json,
			expires_at   = excluded.expires_at,
			version      = excluded.version,
			document     = excluded.document
		WHERE excluded.version > iam_invites.version`,
		invitation.ID, invitation.EmailBlind, []byte(invitation.Sealed),
		invitation.InvitedBy, grants, millis(invitation.ExpiresAt),
		at.bucket(), at.unix(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write an invitation: %w", err)
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
// THE IDENTIFIERS ARE RELEASED IN THE SAME STATEMENT LIST rather than by a
// cascade, because a cascade is a delete nobody committed: the deletion is
// part of this record's own effect and is therefore identical on every node.
//
// AND EVERY SEALED VALUE OF THEIRS IS ERASED, in the same transaction: the
// rows it deletes take their name, address and seeds with them, and the rows
// that outlive them — an invitation addressed to them, their authentication
// trail — keep every column but the sealed ones ([eraseSealed]).
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
	// THE LOGIN THEY HOLD, read before the row that holds it is deleted:
	// a credential bound through it is bound through nothing afterwards.
	login, err := heldLogin(ctx, tx, id)
	if err != nil {
		return 0, err
	}

	// THE TOMBSTONE FIRST, so a transaction that fails partway leaves the
	// person present rather than deleted-and-reusable. It is keyed on the
	// person and carries the record's OP ID rather than its position,
	// because the apply that writes it is the one that must be able to run
	// twice and a position changes under a republish.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_removed
			(person_id, at, record_id, actor, actor_kind, reason,
			 claims_json, bucket, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO NOTHING`,
		id, at.unix(), at.record.OpID, at.record.Actor,
		string(at.record.ActorKind), at.record.Reason, string(released),
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
		deleted, execErr := tx.ExecContext(ctx, statement.sql, id)
		if execErr != nil {
			return int(written), fmt.Errorf("iamdomain: delete person %s's %s: %w",
				id, statement.what, execErr)
		}
		n, _ := deleted.RowsAffected()
		written += n
	}

	// THE HISTORY IS NOT DELETED, and that is the whole reason the id
	// outlives the person: an authentication trail whose authors evaporate
	// is not an audit trail. What makes the removal real is that their
	// NAME, ADDRESS and SEEDS are gone from every row — erased here, in the
	// transaction that deletes the rest.
	erased, err := eraseSealed(ctx, tx, id, removal.Released.EmailBlind)
	written += int64(erased)
	if err != nil {
		return int(written), fmt.Errorf("iamdomain: erase person %s's "+
			"sealed values: %w", id, err)
	}
	if written > 0 {
		// A REMOVAL RELEASES THE SEAT with everything else they held, and
		// the tombstone is what the directory then reads as the seat's
		// standing — see [Reader.SeatHolders]. And it ends every
		// credential they held, with the rows it deleted.
		a.moved.Seats = true
		a.moved.person(id, login)
	}
	return int(written), nil
}

// reboundRemovals selects the removal tombstones a bind of one seat speaks
// for: every removal that released the seat and that no bind since has.
//
// ONE CLAUSE FOR BOTH ITS READERS — the bind's apply, which stamps them, and
// its writer, which declares their people's buckets in the record's scope
// ([leaversOf]) — because a tombstone is filed under the LEAVER's bucket, and a
// bind whose scope named only its own person wrote where no node holding back
// an earlier record about the leaver would wait for it.
const reboundRemovals = `seat_rebound_by = ''
	AND json_extract(claims_json, '$.seat_id') = ?`

// leaversOf is the people whose removal tombstones a bind of seatID would
// stamp ([reboundRemovals]), sorted.
func leaversOf(ctx context.Context, tx *sql.Tx, seatID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT person_id FROM iam_removed WHERE `+reboundRemovals+`
		 ORDER BY person_id`, seatID)
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
