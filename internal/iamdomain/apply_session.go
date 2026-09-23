package iamdomain

import (
	"context"
	"database/sql"
	"fmt"
)

// SESSIONS AND THE BOOTSTRAP: the two kinds whose rows are about getting in.

// applySession dispatches the ops on [KindSession].
//
// TWO OPS AND NO MORE, which is what keeps this log's volume proportional to
// SIGN-INS rather than to requests: a rotation id is an HMAC over the lineage
// and the session's age, so the busiest thing a signed-in person does writes
// nothing here at all.
func (a *Applier) applySession(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	switch at.record.Op {
	case OpOpen:
		return a.writeSessionOpen(ctx, tx, at)
	case OpClose:
		return a.writeSessionClose(ctx, tx, at)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a session, "+
		"which this build has no case for", at.position, at.record.Op)
}

// writeSessionOpen records one session beginning.
func (a *Applier) writeSessionOpen(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	session, err := DecodeSession(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the session record at %s: %w",
			at.position, err)
	}
	if session.Person == "" {
		return 0, fmt.Errorf("iamdomain: the session opened at %s names no "+
			"person — a bearer minted against it would validate as nobody",
			at.position)
	}
	document, err := EncodeSession(session)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the session at %s: %w",
			at.position, err)
	}
	// `start_position` IS THIS RECORD'S OWN, which is what a bearer
	// carries so a node can tell a session opened before its own
	// checkpoint from one it has simply not seen yet — the difference
	// between a 401 and a 503, and the reason a stalled applier does not
	// sign the whole company out.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_sessions
			(lineage, person_id, epoch, start_position, absolute_expires_at,
			 ended_at, ended_reason, bucket, created_at, version, document)
		VALUES (?, ?, ?, ?, ?, 0, '', ?, ?, ?, ?)
		ON CONFLICT(lineage) DO UPDATE SET
			version  = excluded.version,
			document = excluded.document
		WHERE excluded.version > iam_sessions.version`,
		at.record.Subject.ID, session.Person, int64(session.Epoch), at.packed,
		millis(session.AbsoluteExpiresAt), at.bucket(), at.unix(), at.packed,
		document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: open session %s: %w",
			at.record.Subject.ID, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// writeSessionClose records one session ending, and keeps the row.
//
// THE ROW IS KEPT UNTIL THE SWEEP COLLECTS IT, rather than deleted, because
// "this session was ended by reuse detection" is the sentence an investigation
// is looking for — and a delete would leave the investigation with a session
// that simply is not there, which reads as one that never existed.
func (a *Applier) writeSessionClose(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	session, err := DecodeSession(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the session record at %s: %w",
			at.position, err)
	}
	// ENDED_AT IS SET ONCE. The guard is the column rather than the
	// version, for the redemption's reason: two requests closing one
	// session is not a stale write to skip, it is a race whose loser must
	// see the reason the winner recorded rather than overwrite it.
	result, err := tx.ExecContext(ctx, `
		UPDATE iam_sessions
		SET ended_at = ?, ended_reason = ?, version = ?
		WHERE lineage = ? AND ended_at = 0`,
		at.unix(), session.EndedReason, at.packed, at.record.Subject.ID)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: close session %s: %w",
			at.record.Subject.ID, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// applyBootstrap writes the company's one way in before it has anybody.
//
// ONE SUBJECT FOR THE WHOLE DOMAIN, so two nodes minting a code contend and
// exactly one wins: two live bootstraps is two ways into an engine that has no
// other way in.
func (a *Applier) applyBootstrap(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Op != OpBootstrap {
		return 0, fmt.Errorf("iamdomain: the record at %s is op %q on the "+
			"bootstrap, which this build has no case for", at.position,
			at.record.Op)
	}
	bootstrap, err := DecodeBootstrapDoc(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the bootstrap record at %s: %w",
			at.position, err)
	}
	document, err := EncodeBootstrapDoc(bootstrap)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the bootstrap at %s: %w",
			at.position, err)
	}
	// A MINT, A REDEMPTION AND A WITHDRAWAL ARE ONE STATEMENT,
	// distinguished by what the payload carries: a mint names a verifier
	// and no person, a redemption names a person, a withdrawal says so.
	// The whole life of the one bootstrap object sits consecutively on
	// one subject, so an operator reads it in order whatever each record
	// is called.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_bootstrap_codes
			(id, verifier, expires_at, redeemed_at, person_id, minted_by,
			 bucket, created_at, version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			redeemed_at = excluded.redeemed_at,
			person_id   = excluded.person_id,
			version     = excluded.version,
			document    = excluded.document
		WHERE excluded.version > iam_bootstrap_codes.version`,
		bootstrap.ID, []byte(bootstrap.Verifier), millis(bootstrap.ExpiresAt),
		redeemedAt(at, bootstrap), bootstrap.Person, bootstrap.MintedBy,
		int64(BootstrapBucket()), at.unix(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write the bootstrap code: %w", err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// redeemedAt is the broker instant a bootstrap was spent, or zero.
//
// DERIVED FROM THE PAYLOAD rather than carried as its own field, because the
// two would then be able to disagree: a record naming a person with no
// redemption instant, or an instant with nobody behind it, are both states the
// estate has no reading for.
//
// SPENT COVERS BOTH WAYS A CODE STOPS WORKING — somebody used it, or an
// operator superseded it — and the row keeps WHICH: `person_id` is set on the
// first and empty on the second, and the document says `withdrawn` either way.
func redeemedAt(at applyContext, bootstrap Bootstrap) int64 {
	if bootstrap.Person == "" && !bootstrap.Withdrawn {
		return 0
	}
	return at.unix()
}
