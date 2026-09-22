package iamdomain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// THE AUTHENTICATION TRAIL: one row per record that changes who somebody is or
// whether they are signed in.
//
// # The class is DERIVED, never carried
//
// A writer that stated its own class could file a suspension in the short
// horizon, and the row would simply be gone when somebody went looking. So the
// op decides, through one table beside the enum, and an op with no
// classification is a build failure rather than a record that quietly writes
// no trail.
//
// # Why the row's id is a digest and not a uuid
//
// The applier is a PURE FUNCTION of the log: a uuid minted here would differ
// on every node, in a table that claims byte-identical state. So the id is a
// digest of the record's own operation id and the position it applied at —
// both of which every node sees identically — which also makes the row
// naturally idempotent under redelivery.

// writeHistory writes the trail row for one record, if its op has one.
func (a *Applier) writeHistory(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	class, recorded := ClassOf(at.record.Op)
	if !recorded {
		return 0, nil
	}
	document, err := Encode(at.record)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: re-encode the record at %s for the "+
			"trail: %w", at.position, err)
	}
	summary := at.record.Reason
	if len(summary) > MaxReason {
		// CUT RATHER THAN REFUSED, and only here. Every other value in
		// this domain is refused past its cap naming the field, because
		// a caller can fix what it sent; this one arrives on a record
		// the broker has already committed, and refusing it would stall
		// the log over a long sentence.
		summary = summary[:MaxReason]
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_history
			(id, class, object_kind, object_id, person_id, op, actor,
			 actor_kind, reason, summary, created_at, broker_at, bucket,
			 version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		historyID(at), string(class), string(at.record.Subject.Kind),
		at.record.Subject.ID, at.record.Person, string(at.record.Op),
		at.record.Actor, string(at.record.ActorKind), at.record.Reason,
		summary, at.unix(), at.unix(), at.bucket(), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write the trail row for %s: %w",
			at.position, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// historyID is the trail row's own id: a digest every node computes the same
// way.
//
// THE OPERATION ID AND THE POSITION, both. The op id alone would collide
// across a republish, which is exactly what a reanchor performs — the same
// operation, at a new position, legitimately producing a second row that says
// when this fleet applied it. The position alone would collide with nothing
// and say nothing, and would change under a republish that must not rewrite
// history.
func historyID(at applyContext) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d",
		at.record.OpID, at.packed)))
	return hex.EncodeToString(digest[:16])
}
