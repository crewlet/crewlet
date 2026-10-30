package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// applyMergeability writes a change's mergeability ([Mergeability]) — its one
// row, replaced whole by every merge the decide took, with the readiness the
// decide stamped beside it — moving the change's conversation's text_bytes by
// what the conflicted paths weigh, less what the row they replace weighed, so
// a purge weighs the change's subject's wide rows by its one narrow row. The
// row's instant is the record's broker instant, which its home's next recheck
// is paced by (changesdue.go).
//
// A REFRESH rewrites the row's readiness and the versions and the checks
// position it was judged at, and nothing of its merge: the instant a recheck is paced by stays the
// merge's, and the conflicted paths are the row's own. It writes the row only
// while the row is still the merge the decide stamped on the record — which
// the arbitration on the change's subject keeps true, since no other record
// writes the row — and applies as nothing otherwise.
//
// PURE IN THE RECORD, its position, its broker instant and the prior row.
func applyMergeability(ctx context.Context, tx *sql.Tx, at applyContext, m Mergeability) (int, conversationCounts,
	error) {

	if !m.State.Valid() {
		return 0, conversationCounts{}, fmt.Errorf("code: the mergeability at %s says %q, which no writer forms",
			at.position, m.State)
	}
	if m.Refresh {
		res, err := tx.ExecContext(ctx, refreshMergeabilitySQL, m.ChangeVersion, m.ReadinessVersion,
			m.ChecksPosition.Packed(), m.Ready, m.WaitsPerson, m.ChangeID, m.Revision, m.TargetRef,
			m.TargetPosition.Packed())
		if err != nil {
			return 0, conversationCounts{}, fmt.Errorf("code: refresh %s's readiness at %s: %w", m.ChangeID,
				at.position, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			n, err := conversationNotApplied(ctx, at, m.RepoID, m.ChangeID, "the merge it refreshes is not the "+
				"change's mergeability")
			return n, conversationCounts{}, err
		}
		return 1, conversationCounts{}, nil
	}
	prior, held, err := mergeabilityIn(ctx, tx, m.ChangeID)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	conflicts := strings.Join(m.Conflicts, "\x00")
	weight := int64(len(conflicts))
	if _, err := tx.ExecContext(ctx, writeMergeabilitySQL, m.ChangeID, m.Revision, m.TargetRef, m.TargetOID,
		m.TargetPosition.Packed(), string(m.State), len(m.Conflicts), m.ConflictsOmitted, m.Node, weight,
		at.packed, at.at(), m.ChangeVersion, m.ReadinessVersion, m.ChecksPosition.Packed(), m.Ready, m.WaitsPerson,
		conflicts); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: write %s's mergeability at %s: %w", m.ChangeID,
			at.position, err)
	}
	counts := conversationCounts{textBytes: weight}
	if held {
		counts.textBytes -= prior.bytes
	}
	return 1, counts, nil
}

// A mergeability's writes: the row by the change's key, replaced whole by
// every merge the decide took; and a refresh's readiness, written by the same
// key only while the row is the merge the record names.
const (
	writeMergeabilitySQL = `INSERT INTO code_mergeability (change_id, revision, target_ref, target_oid,
			target_position, state, conflicts_count, conflicts_omitted, node, bytes, position, at, change_version,
			readiness_version, checks_position, ready, waits_person, conflicts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id) DO UPDATE SET revision = excluded.revision, target_ref = excluded.target_ref,
			target_oid = excluded.target_oid, target_position = excluded.target_position, state = excluded.state,
			conflicts_count = excluded.conflicts_count, conflicts_omitted = excluded.conflicts_omitted,
			node = excluded.node, bytes = excluded.bytes, position = excluded.position, at = excluded.at,
			change_version = excluded.change_version, readiness_version = excluded.readiness_version,
			checks_position = excluded.checks_position, ready = excluded.ready,
			waits_person = excluded.waits_person, conflicts = excluded.conflicts`
	refreshMergeabilitySQL = `UPDATE code_mergeability SET change_version = ?, readiness_version = ?,
			checks_position = ?, ready = ?, waits_person = ?
		WHERE change_id = ? AND revision = ? AND target_ref = ? AND target_position = ?`
)
