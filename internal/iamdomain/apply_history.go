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
// THE CLASS IS DERIVED from the op through one table beside the enum, never
// carried: a writer stating its own could file a suspension in the short
// horizon, gone when somebody looked. An unclassified op fails the build.
//
// THE ROW'S ID IS A DIGEST of the record's op id and position, not a uuid,
// which would differ per node in a table claiming byte-identical state; it is
// also idempotent under redelivery.

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
		// CUT, NOT REFUSED, here alone: every other value is refused past its
		// cap so the caller can fix it, but this record is already committed,
		// and refusing it would stall the log over a long sentence.
		summary = summary[:MaxReason]
	}
	// THE OBJECT IS WHAT THE RECORD WAS ABOUT: its subject, or for a
	// directory record the person or address ([applyContext.aboutKind]).
	objectKind, objectID := string(at.record.Subject.Kind), at.record.Subject.ID
	if at.aboutKind != "" {
		objectKind, objectID = at.aboutKind, at.aboutID
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO iam_history
			(id, class, object_kind, object_id, person_id, op, actor,
			 actor_kind, operator_id, reason, summary, created_at, broker_at,
			 bucket, version, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		historyID(at), string(class), objectKind, objectID,
		at.record.Person, string(at.record.Op),
		at.record.Actor, string(at.record.ActorKind), at.record.OperatorID,
		at.record.Reason, summary, at.unix(), at.unix(), at.bucket(),
		at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("iamdomain: write the trail row for %s: %w",
			at.position, err)
	}
	written, _ := result.RowsAffected()
	return int(written), nil
}

// historyID is the trail row's id, a digest every node computes alike of the
// op id AND the position: the op id alone collides across a republish, which a
// reanchor performs, legitimately adding a row for when this fleet applied it.
func historyID(at applyContext) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d",
		at.record.OpID, at.packed)))
	return hex.EncodeToString(digest[:16])
}
