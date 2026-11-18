package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// applyMergeability writes a change's mergeability ([Mergeability]): its one
// row, replaced whole by every merge the decide took, with the stamped
// readiness, moving the conversation's text_bytes by the conflicted paths'
// weight less the replaced row's, so a purge weighs the subject's wide rows by
// its one narrow row. The row's instant is the broker instant, which its
// home's next recheck is paced by (changesdue.go).
//
// A REFRESH rewrites only the readiness, the versions, the checks position and
// the chart, keeping the merge, its instant and its conflicted paths, and only
// while the row is still the merge the record names (no other record writes a
// row's merge), else applies as nothing. Pure in the record, its position and
// instant, the prior row and the change's state.
func applyMergeability(ctx context.Context, tx *sql.Tx, at applyContext, m Mergeability) (int, conversationCounts,
	error) {

	if !m.State.Valid() {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: the mergeability at %s says %q, which no writer forms",
			at.position, m.State)
	}
	change, found, err := changeRowIn(ctx, tx, m.ChangeID)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	// An ended change stores no answer: this record, on the change's subject,
	// is not ordered against the close or landing on the repository's, so it
	// can land after that record's [clearReadiness] and would leave the change
	// in code_mergeability_ready_idx or code_mergeability_waits_idx until its
	// repository's purge.
	if !found || !change.state.Live() {
		m.Ready, m.WaitsPerson = false, false
	}
	if m.Refresh {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, refreshMergeabilitySQL, m.ChangeVersion, m.ReadinessVersion,
			m.ChecksPosition.Packed(), m.ChartEpoch, m.Ready, m.WaitsPerson, m.ChangeID, m.Revision, m.TargetRef,
			m.TargetPosition.Packed())
		if err != nil {
			return 0, conversationCounts{}, fmt.Errorf("gitlet: refresh %s's readiness at %s: %w", m.ChangeID,
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
		at.packed, at.at(), m.ChangeVersion, m.ReadinessVersion, m.ChecksPosition.Packed(), m.ChartEpoch, m.Ready,
		m.WaitsPerson, conflicts); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("gitlet: write %s's mergeability at %s: %w", m.ChangeID,
			at.position, err)
	}
	counts := conversationCounts{textBytes: weight}
	if held {
		counts.textBytes -= prior.bytes
	}
	return 1, counts, nil
}

// applyMergeabilityKept moves what a re-evaluation that kept the row's outcome
// moved ([MergeabilityKept]): a recheck's tip, its position and the row's
// instant, and a judgement's versions, checks position and chart, each only
// where the record carries it; the state, conflicts, stored answer and node
// stay. A refresh alone leaves the instant, so a review never postpones a
// moved target's recheck.
//
// It writes only while the row is of the record's revision with no later tip
// or version (versions only grow), else nothing. The checks position and chart
// are written as stamped, even lower: [CheckHistory] removes a head's check
// rows and a chart may be an activation behind, so holding them forward would
// leave the change due a refresh that applies as nothing at every pass.
func applyMergeabilityKept(ctx context.Context, tx *sql.Tx, at applyContext, k MergeabilityKept) (int, error) {
	retarget, rejudge := k.TargetOID != "", k.ChangeVersion != 0
	if !retarget && !rejudge {
		return 0, fmt.Errorf("gitlet: the kept mergeability at %s moves neither a tip nor a judgement, which no "+
			"writer forms", at.position)
	}
	res, err := tx.ExecContext(ctx, keptMergeabilitySQL, retarget, k.TargetOID, k.TargetPosition.Packed(), at.at(),
		rejudge, k.ChangeVersion, k.ReadinessVersion, k.ChecksPosition.Packed(), k.ChangeID, k.Revision,
		k.ChartEpoch)
	if err != nil {
		return 0, fmt.Errorf("gitlet: move %s's mergeability at %s: %w", k.ChangeID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return conversationNotApplied(ctx, at, k.RepoID, k.ChangeID, "the mergeability it keeps is not the "+
			"change's, or has moved past it")
	}
	return 1, nil
}

// clearReadiness clears the stored readiness of the changes ids names, each
// just closed or merged, answering one row for each that held an answer. An
// ended change is ready for and waits on nobody, and no refresh reaches it, so
// a row left answering would stay in code_mergeability_ready_idx or
// code_mergeability_waits_idx (which readiness=ready and needs=person walk)
// until its repository's purge. A reopen moves the change's version, so its
// home judges it again (changesdue.go's refresh term).
func clearReadiness(ctx context.Context, tx store.Execer, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, clearReadinessSQL, list)
	if err != nil {
		return 0, fmt.Errorf("gitlet: clear the stored readiness of %d ended changes: %w", len(ids), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("gitlet: clear the stored readiness of %d ended changes: %w", len(ids), err)
	}
	return int(n), nil
}

// A mergeability's writes, each by the change's key: the row replaced whole; a
// refresh's readiness only while the row is the record's merge; a kept
// re-evaluation only while the row holds no later tip or version; and ended
// changes' readiness cleared only where the row holds an answer, since writing
// ready or waits_person rewrites its partial index entry whatever the value.
const (
	writeMergeabilitySQL = `INSERT INTO code_mergeability (change_id, revision, target_ref, target_oid,
			target_position, state, conflicts_count, conflicts_omitted, node, bytes, position, at, change_version,
			readiness_version, checks_position, chart_epoch, ready, waits_person, conflicts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id) DO UPDATE SET revision = excluded.revision, target_ref = excluded.target_ref,
			target_oid = excluded.target_oid, target_position = excluded.target_position, state = excluded.state,
			conflicts_count = excluded.conflicts_count, conflicts_omitted = excluded.conflicts_omitted,
			node = excluded.node, bytes = excluded.bytes, position = excluded.position, at = excluded.at,
			change_version = excluded.change_version, readiness_version = excluded.readiness_version,
			checks_position = excluded.checks_position, chart_epoch = excluded.chart_epoch, ready = excluded.ready,
			waits_person = excluded.waits_person, conflicts = excluded.conflicts`
	refreshMergeabilitySQL = `UPDATE code_mergeability SET change_version = ?, readiness_version = ?,
			checks_position = ?, chart_epoch = ?, ready = ?, waits_person = ?
		WHERE change_id = ? AND revision = ? AND target_ref = ? AND target_position = ?`
	keptMergeabilitySQL = `UPDATE code_mergeability SET
			target_oid = CASE WHEN ?1 THEN ?2 ELSE target_oid END,
			target_position = CASE WHEN ?1 THEN ?3 ELSE target_position END,
			at = CASE WHEN ?1 THEN ?4 ELSE at END,
			change_version = CASE WHEN ?5 THEN ?6 ELSE change_version END,
			readiness_version = CASE WHEN ?5 THEN ?7 ELSE readiness_version END,
			checks_position = CASE WHEN ?5 THEN ?8 ELSE checks_position END,
			chart_epoch = CASE WHEN ?5 THEN ?11 ELSE chart_epoch END
		WHERE change_id = ?9 AND revision = ?10 AND (NOT ?1 OR target_position <= ?3)
			AND (NOT ?5 OR (change_version <= ?6 AND readiness_version <= ?7))`
	clearReadinessSQL = `UPDATE code_mergeability SET ready = 0, waits_person = 0 WHERE rowid IN (SELECT g.rowid
		FROM json_each(?) AS j CROSS JOIN code_mergeability AS g
		WHERE g.change_id = CAST(j.value AS TEXT) AND (g.ready = 1 OR g.waits_person = 1))`
)
