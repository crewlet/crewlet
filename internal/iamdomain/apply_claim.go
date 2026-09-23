package iamdomain

import (
	"context"
	"database/sql"
	"fmt"
)

// THE CLAIM SUBJECTS: an address, a login and a seat binding.
//
// Each arbitrates on ITSELF, create-only at an expectation of zero, which is
// the whole of what keeps two people off one identity — there is no unique
// index anywhere in this estate and there cannot be one. What the apply does
// is write the token onto the person's row, which is what the duplicate-claim
// duty later scans.
//
// # A claim writes a column on a row another subject owns
//
// That is the one shape here that looks wrong and is not. A claim record
// arbitrates on the ADDRESS and writes `iam_people.email_blind`; a content
// record arbitrates on the PERSON and writes everything else on that row. So
// the two subjects meet on one row, exactly as a unit's content and its
// placement do in the org chart, and the rule is the org chart's rule: each
// touches only its own half, and `scoped_through` rather than `version` is
// what a claim stamps — so the row's broker expectation still matches its own
// subject's last message and cannot be poisoned into permanent unwritability.

// applyEmail dispatches the ops on [KindEmail], which is the only claim kind
// with an invitation behind it.
func (a *Applier) applyEmail(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	switch at.record.Op {
	case OpInvite:
		return a.writeInvitation(ctx, tx, at)
	case OpRedeem:
		return a.redeemInvitation(ctx, tx, at)
	case OpClaim, OpRelease:
		return a.writeToken(ctx, tx, at, KindEmail)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on an address, "+
		"which this build has no case for", at.position, at.record.Op)
}

// applyToken dispatches the ops on [KindLogin] and [KindSeat].
func (a *Applier) applyToken(ctx context.Context, tx *sql.Tx, at applyContext,
	kind ObjectKind) (int, error) {

	switch at.record.Op {
	case OpClaim, OpRelease:
		return a.writeToken(ctx, tx, at, kind)
	}
	return 0, fmt.Errorf("iamdomain: the record at %s is op %q on a %s claim, "+
		"which this build has no case for", at.position, at.record.Op, kind)
}

// writeToken puts a claimed token onto its holder's row, or takes it off.
//
// THE COLUMN IS DECIDED BY THE SUBJECT KIND, in a switch rather than by a
// caller passing a name, because a column name built at a call site is a
// string this package would be interpolating into SQL from somewhere it cannot
// see.
func (a *Applier) writeToken(ctx context.Context, tx *sql.Tx, at applyContext,
	kind ObjectKind) (int, error) {

	claim, err := DecodeClaim(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the claim record at %s: %w",
			at.position, err)
	}

	var column string
	switch kind {
	case KindEmail:
		column = "email_blind"
	case KindLogin:
		column = "login"
	case KindSeat:
		column = "seat_id"
	default:
		return 0, fmt.Errorf("iamdomain: %s is not a claim kind", kind)
	}

	// A RELEASE CLEARS THE COLUMN ON WHOEVER HOLDS THE TOKEN, and a claim
	// sets it on the person the record names. The two are one statement
	// shape with a different target, and stating them apart is what stops a
	// release needing to know who the holder was.
	if at.record.Op == OpRelease {
		// A RELEASED SEAT CLEARS ITS POSITION TOO. Left behind it would
		// say a binding that no longer exists was decided against a
		// chart this node has seen, which is a sentence about nothing —
		// and the next bind would then read one claim's handle beside
		// another's position until it overwrote both.
		clear := column + ` = ''`
		if kind == KindSeat {
			clear += ", chart_position = 0"
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE iam_people
			SET `+clear+`, updated_at = ?, scoped_through = ?
			WHERE `+column+` = ? AND scoped_through < ?`,
			at.unix(), at.packed, at.record.Subject.ID, at.packed)
		if err != nil {
			return 0, fmt.Errorf("iamdomain: release the %s claim on %s: %w",
				kind, at.record.Subject.ID, err)
		}
		written, _ := result.RowsAffected()
		if kind == KindSeat && written > 0 {
			// AN UNBIND hands the seat back to the chart, which is a
			// move in its standing — see [Reader.SeatHolders].
			a.directoryMoved = true
		}
		return int(written), nil
	}

	if at.record.Person == "" {
		return 0, fmt.Errorf("iamdomain: the claim at %s names no person — a "+
			"claim binds a token TO somebody, and one that binds it to nobody "+
			"would take the address out of circulation with nothing holding it",
			at.position)
	}

	// THE TOKEN GOES ON ONE ROW AND IS TAKEN OFF EVERY OTHER, in that
	// order. A person moving to an address they already hold elsewhere in
	// the row set is not a state the broker can refuse — the subject
	// arbitrates the ADDRESS, and a stale row carrying it is this node's
	// own residue — so the apply is what makes the column single-holder,
	// deterministically, on every node.
	stale := column + ` = ''`
	if kind == KindSeat {
		stale += ", chart_position = 0"
	}
	cleared, err := tx.ExecContext(ctx, `
		UPDATE iam_people
		SET `+stale+`, updated_at = ?, scoped_through = ?
		WHERE `+column+` = ? AND id <> ? AND scoped_through < ?`,
		at.unix(), at.packed, at.record.Subject.ID, at.record.Person, at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: clear a stale %s claim on %s: %w",
			kind, at.record.Subject.ID, err)
	}
	written, _ := cleared.RowsAffected()

	// AN UPSERT, NOT AN UPDATE, and this is the shape an enrolment's own
	// order forces. The claims are taken BEFORE the person's content is
	// written — they are what can be refused, so they go first — which
	// means the claim's apply routinely runs against a person who does not
	// exist yet.
	//
	// SO IT CREATES THE ROW, with no kind and no stage. That row is the
	// RESERVATION half of an enrolment, and it is the legal named state a
	// stopped sequence leaves behind rather than a special case: an empty
	// stage is not `active`, and only `active` may act, so a person the
	// sequence never finished can hold an address and do nothing at all.
	// The orphan duty reports them; the content record that follows fills
	// them in.
	//
	// The alternative order — person first — would leave a person nobody
	// can find, holding an address somebody else may then take, which is
	// the failure this estate has no index to refuse.
	//
	// A SEAT CLAIM ALSO WRITES THE CHART POSITION it was decided at, and
	// the two move together for a reason: the position is only meaningful
	// as a statement about the seat in the same row. A claim that set the
	// handle and left a stale position behind would tell every reader that
	// this node's view covers a decision it does not.
	assign := column + " = excluded." + column
	if kind == KindSeat {
		assign += ", chart_position = excluded.chart_position"
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_people
			(id, kind, stage, login, email_blind, seat_id, chart_position,
			 name_sealed, email_sealed, shredded, bucket,
			 created_at, updated_at, version, scoped_through, document)
		VALUES (?, '', '', ?, ?, ?, ?, x'', x'', 0, ?, ?, ?, 0, ?, x'')
		ON CONFLICT(id) DO UPDATE SET
			`+assign+`,
			updated_at     = excluded.updated_at,
			scoped_through = excluded.scoped_through
		WHERE excluded.scoped_through > iam_people.scoped_through`,
		at.record.Person,
		claimColumn(KindLogin, kind, at.record.Subject.ID),
		claimColumn(KindEmail, kind, at.record.Subject.ID),
		claimColumn(KindSeat, kind, at.record.Subject.ID),
		int64(claim.ChartPosition),
		at.bucket(), at.unix(), at.unix(), at.packed)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: bind the %s claim on %s to %s: %w",
			kind, at.record.Subject.ID, at.record.Person, err)
	}
	bound, _ := result.RowsAffected()
	written += bound
	if kind == KindSeat && bound > 0 {
		// AND IT ENDS THE SAY OF EVERY REMOVAL THAT RELEASED THIS SEAT.
		// A removal's tombstone withholds the seat it released because
		// the contact map still names the leaver; a bind is a new binding
		// with a standing of its own, and once one has landed the leaver
		// is no longer what the seat's standing is about — however many
		// holders come and go after it. See [Reader.SeatHolders].
		//
		// The FIRST bind's operation id, and the guard is what makes it the
		// first: a removal is a gate no node defers, so every node has its
		// tombstone before any later bind, and every node stamps the same
		// id.
		retired, err := tx.ExecContext(ctx, `
			UPDATE iam_removed SET seat_rebound_by = ?
			WHERE seat_rebound_by = ''
			  AND json_extract(claims_json, '$.seat_id') = ?`,
			at.record.OpID, at.record.Subject.ID)
		if err != nil {
			return int(written), fmt.Errorf("iamdomain: end the removals' say "+
				"over seat %s: %w", at.record.Subject.ID, err)
		}
		n, _ := retired.RowsAffected()
		written += n
	}
	if kind == KindSeat && written > 0 {
		// A BIND puts a person — at whatever stage they are — between
		// the seat and its contact routing.
		a.directoryMoved = true
	}

	// AND THE SEALED FORM, for the one claim whose token is not readable.
	// The blind is one-way by construction, so without this the company
	// could authenticate somebody and never show them which address it
	// authenticated them by.
	if kind == KindEmail && claim.Sealed != "" {
		sealed, err := tx.ExecContext(ctx, `
			UPDATE iam_people
			SET email_sealed = ?
			WHERE id = ? AND scoped_through <= ?`,
			[]byte(claim.Sealed), at.record.Person, at.packed)
		if err != nil {
			return int(written), fmt.Errorf("iamdomain: store %s's sealed "+
				"address: %w", at.record.Person, err)
		}
		n, _ := sealed.RowsAffected()
		written += n
	}
	return int(written), nil
}

// writeInvitation records an address spoken for by somebody who has no person
// yet.
//
// IT ARBITRATES ON THE ADDRESS, which is what makes an invitation and an
// enrolment for one address contend: they share the subject, so the second one
// is refused rather than producing a person and an invitation for the same
// person's address.
func (a *Applier) writeInvitation(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	invitation, err := DecodeInvitation(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the invitation record at %s: %w",
			at.position, err)
	}
	document, err := EncodeInvitation(invitation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the invitation at %s: %w",
			at.position, err)
	}
	grants, err := encodeGrants(invitation.Grants)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: encode an invitation's grants: %w", err)
	}
	// THE BUCKET IS THE ADDRESS'S OWN, not a person's: an invitation has no
	// person until it is redeemed, and a bucket derived from an empty id
	// would put every outstanding invitation in the company into one sweep
	// transaction.
	bucket := int64(BucketOf(at.record.Subject.ID))
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
		invitation.ID, at.record.Subject.ID, []byte(invitation.Sealed),
		invitation.InvitedBy, grants, millis(invitation.ExpiresAt),
		bucket, at.unix(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write an invitation: %w", err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// redeemInvitation marks an invitation used and binds the address it held.
//
// ONE RECORD FOR BOTH HALVES, because they arbitrate on the same subject —
// the address — and splitting them would leave an invitation redeemed with the
// address unclaimed, or the reverse, with nothing able to tell which.
func (a *Applier) redeemInvitation(ctx context.Context, tx *sql.Tx,
	at applyContext) (int, error) {

	invitation, err := DecodeInvitation(at.record.Mutation)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: the redemption record at %s: %w",
			at.position, err)
	}
	if at.record.Person == "" {
		return 0, fmt.Errorf("iamdomain: the redemption at %s names no person "+
			"— a redemption is what CREATES one, so the record must say who",
			at.position)
	}
	// REDEEMED_AT IS SET ONCE. The guard is the column rather than the
	// version, because redeeming twice is not a stale write to skip — it
	// is two people trying to use one invitation, and the second must find
	// it spent whatever position it arrived at.
	result, err := tx.ExecContext(ctx, `
		UPDATE iam_invites
		SET redeemed_at = ?, person_id = ?, version = ?
		WHERE id = ? AND redeemed_at = 0`,
		at.unix(), at.record.Person, at.packed, invitation.ID)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: redeem invitation %s: %w",
			invitation.ID, err)
	}
	written, _ := result.RowsAffected()

	bound, err := a.writeToken(ctx, tx, at, KindEmail)
	return int(written) + bound, err
}

// claimColumn is the value one column takes for a claim of a given kind: the
// token where they match, and the empty string everywhere else.
//
// A HELPER RATHER THAN THREE STATEMENTS, because the upsert has to bind all
// three columns to create the row at all — an insert naming only the claimed
// one would leave the others unbound, and they are NOT NULL. Three statements
// would be three round trips for one claim, and three places for the ON
// CONFLICT guard to drift.
func claimColumn(want, got ObjectKind, token string) string {
	if want == got {
		return token
	}
	return ""
}
