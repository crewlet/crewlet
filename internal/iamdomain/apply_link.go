package iamdomain

import (
	"context"
	"database/sql"
	"fmt"
)

// THE LINK SUBJECT: a provider subject pinned to one person, held as that
// person's oidc CREDENTIAL ROW — see link.go for why the row, and why this
// apply is the only thing that ever writes one.

// applyLink dispatches the ops on [KindLink].
func (a *Applier) applyLink(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	switch at.record.Op {
	case OpClaim:
		return a.writeLink(ctx, tx, at)
	case OpRelease:
		return a.clearLink(ctx, tx, at)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a provider "+
		"link, which this build has no case for", at.position, at.record.Op)
}

// writeLink pins the subject to the person the claim names.
//
// THE COLUMN SEMANTICS EVERY OTHER CLAIM HAS, on a row rather than a column:
// the subject goes on ONE person and comes off every other, and the person
// holds ONE subject — pinning a second revokes the first. Both are the apply's
// to make deterministic, because neither is a state the broker can refuse: the
// subject arbitrates the SUBJECT, not the person, so two administrators
// pinning two different subjects to one person at once each pass their
// decide ([ErrLinked] reads a snapshot neither record is in yet), and a stale
// row carrying the subject is this node's own residue. The later record wins,
// on every node, because the log orders them.
//
// REVOKED RATHER THAN DELETED, and a version guard on every statement, because
// a record deferred on one person's bucket can be reprocessed after a later one
// on another's: a deleted row would let the older claim put the link back with
// nothing newer to compare against, where a revoked row carries the position
// that superseded it. The trail is the other half — "this link was withdrawn
// on the 3rd" is a row somebody can read rather than one that vanished.
func (a *Applier) writeLink(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	claim, err := DecodeClaim(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the link record at %s: %w",
			at.position, err)
	}
	person, blind := at.record.Person, at.record.Subject.ID
	if person == "" {
		return 0, fmt.Errorf("iamdomain: the link at %s names no person — a "+
			"link pins a subject TO somebody, and one pinned to nobody would "+
			"take the subject out of circulation with nothing holding it",
			at.position)
	}
	superseded, err := tx.ExecContext(ctx, `
		UPDATE iam_credentials
		SET revoked_at = ?, version = ?
		WHERE method = ? AND revoked_at = 0 AND version < ?
		  AND ((subject_blind = ? AND person_id <> ?)
		    OR (person_id = ? AND subject_blind <> ?))`,
		at.unix(), at.packed, string(MethodOIDC), at.packed,
		blind, person, person, blind)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: supersede the links beside the one "+
			"at %s: %w", at.position, err)
	}
	written, _ := superseded.RowsAffected()

	id := linkCredentialID(person, blind)
	document, err := EncodeCredential(Credential{
		V: DocumentVersion, ID: id, Method: MethodOIDC,
		SubjectBlind: blind, Issuer: claim.Issuer,
	})
	if err != nil {
		return int(written), fmt.Errorf("iamdomain: encode the link at %s: %w",
			at.position, err)
	}
	// AN UPSERT, for the address claim's reason: an enrolment through the
	// provider claims the link BEFORE the person's content record, so this
	// routinely runs for somebody who is only a reservation yet. A re-link
	// of the same pair revives the row it left, and keeps its creation
	// instant only while it was never revoked — a link withdrawn and made
	// again is a new link, and "linked since" should say so.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_credentials
			(id, person_id, method, verifier, subject_blind, expires_at,
			 revoked_at, bucket, created_at, version, document)
		VALUES (?, ?, ?, x'', ?, 0, 0, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			created_at = CASE WHEN iam_credentials.revoked_at = 0
			                  THEN iam_credentials.created_at
			                  ELSE excluded.created_at END,
			revoked_at = 0,
			version    = excluded.version,
			document   = excluded.document
		WHERE excluded.version > iam_credentials.version`,
		id, person, string(MethodOIDC), blind, at.bucket(), at.unix(),
		at.packed, document)
	if err != nil {
		return int(written), fmt.Errorf("iamdomain: pin the link at %s: %w",
			at.position, err)
	}
	pinned, _ := result.RowsAffected()
	return int(written + pinned), nil
}

// clearLink takes the subject off WHOEVER holds it, for the release shape every
// claim has: a release clears the token wherever it is, and the decide already
// confirmed the holder it names.
func (a *Applier) clearLink(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE iam_credentials
		SET revoked_at = ?, version = ?
		WHERE method = ? AND subject_blind = ? AND revoked_at = 0
		  AND version < ?`,
		at.unix(), at.packed, string(MethodOIDC), at.record.Subject.ID,
		at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: release the link at %s: %w",
			at.position, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}
