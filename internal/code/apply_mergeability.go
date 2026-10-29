package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// applyMergeability writes a change's mergeability ([Mergeability]) — its one
// row, replaced whole by every record the decide took — moving the change's
// conversation's text_bytes by what the conflicted paths weigh, less what the
// row they replace weighed, so a purge weighs the change's subject's wide rows
// by its one narrow row. The row's instant is the record's broker instant,
// which its home's next recheck is paced by (changesdue.go).
//
// PURE IN THE RECORD, its position, its broker instant and the prior row.
func applyMergeability(ctx context.Context, tx *sql.Tx, at applyContext, m Mergeability) (int, conversationCounts,
	error) {

	if !m.State.Valid() {
		return 0, conversationCounts{}, fmt.Errorf("code: the mergeability at %s says %q, which no writer forms",
			at.position, m.State)
	}
	prior, held, err := mergeabilityIn(ctx, tx, m.ChangeID)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	conflicts := strings.Join(m.Conflicts, "\x00")
	weight := int64(len(conflicts))
	if _, err := tx.ExecContext(ctx, writeMergeabilitySQL, m.ChangeID, m.Revision, m.TargetRef, m.TargetOID,
		m.TargetPosition.Packed(), string(m.State), len(m.Conflicts), m.ConflictsOmitted, m.Node, weight,
		at.packed, at.at(), conflicts); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: write %s's mergeability at %s: %w", m.ChangeID,
			at.position, err)
	}
	counts := conversationCounts{textBytes: weight}
	if held {
		counts.textBytes -= prior.bytes
	}
	return 1, counts, nil
}

// A mergeability's write: the row by the change's key, replaced whole by every
// record the decide took.
const (
	writeMergeabilitySQL = `INSERT INTO code_mergeability (change_id, revision, target_ref, target_oid,
			target_position, state, conflicts_count, conflicts_omitted, node, bytes, position, at, conflicts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id) DO UPDATE SET revision = excluded.revision, target_ref = excluded.target_ref,
			target_oid = excluded.target_oid, target_position = excluded.target_position, state = excluded.state,
			conflicts_count = excluded.conflicts_count, conflicts_omitted = excluded.conflicts_omitted,
			node = excluded.node, bytes = excluded.bytes, position = excluded.position, at = excluded.at,
			conflicts = excluded.conflicts`
)
