package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
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
// position it was judged at, and nothing of its merge: the instant a recheck
// is paced by stays the merge's, and the conflicted paths are the row's own.
// It writes the row only while the row is still the merge the decide stamped
// on the record — which the arbitration on the change's subject keeps true,
// since no other record writes the row's merge (a close's or a landing's
// clear writes its answer alone) — and applies as nothing otherwise.
//
// A CHANGE THAT IS NO LONGER LIVE STORES NO ANSWER: ready and waits_person
// are written false whatever the record stamped, where the change's row is
// closed or merged by the time the record applies. The record was decided
// over a live change on the change's OWN subject, and the close or the
// landing that ended it is a record on the REPOSITORY'S, which that
// arbitration does not order it against — so the two can land either way
// round, and a stored answer of an ended change would sit in
// code_mergeability_waits_idx, which needs=person walks, until its
// repository's purge. The record that ends a change clears what it stored
// before ([clearReadiness]); this is the same rule for the record that lands
// after it.
//
// PURE IN THE RECORD, its position, its broker instant, the prior row and the
// change's state.
func applyMergeability(ctx context.Context, tx *sql.Tx, at applyContext, m Mergeability) (int, conversationCounts,
	error) {

	if !m.State.Valid() {
		return 0, conversationCounts{}, fmt.Errorf("code: the mergeability at %s says %q, which no writer forms",
			at.position, m.State)
	}
	change, found, err := changeRowIn(ctx, tx, m.ChangeID)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	if !found || !change.state.Live() {
		m.Ready, m.WaitsPerson = false, false
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

// applyMergeabilityKept moves what a re-evaluation that kept the row's outcome
// moved ([MergeabilityKept]): a recheck's tip, its position and the row's
// instant — the one the home's next recheck is paced from — and the versions
// and the checks position a readiness was judged at, each part only where the
// record carries it, and nothing else: the state, the conflicted paths, the
// stored answer, the node and the position of the record that recorded them
// stay the row's. A refresh alone leaves the instant, as a refresh's full
// record does, so a review never postpones a moved target's recheck.
//
// It writes the row only while it is of the record's revision and the record
// moves it FORWARD — a tip no earlier than the row's, versions and a checks
// position no lower — the guard the merge's own record is decided by, which
// the arbitration on the change's subject keeps true since no other record
// writes the row; and applies as nothing otherwise.
//
// PURE IN THE RECORD, its position, its broker instant and the prior row.
func applyMergeabilityKept(ctx context.Context, tx *sql.Tx, at applyContext, k MergeabilityKept) (int, error) {
	retarget, rejudge := k.TargetOID != "", k.ChangeVersion != 0
	if !retarget && !rejudge {
		return 0, fmt.Errorf("code: the kept mergeability at %s moves neither a tip nor a judgement, which no "+
			"writer forms", at.position)
	}
	res, err := tx.ExecContext(ctx, keptMergeabilitySQL, retarget, k.TargetOID, k.TargetPosition.Packed(), at.at(),
		rejudge, k.ChangeVersion, k.ReadinessVersion, k.ChecksPosition.Packed(), k.ChangeID, k.Revision)
	if err != nil {
		return 0, fmt.Errorf("code: move %s's mergeability at %s: %w", k.ChangeID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return conversationNotApplied(ctx, at, k.RepoID, k.ChangeID, "the mergeability it keeps is not the "+
			"change's, or has moved past it")
	}
	return 1, nil
}

// clearReadiness clears the stored readiness of the changes ids names — each
// one a record has just closed or merged — and answers the rows it wrote: one
// for each whose row held an answer, ready or waiting on a person, and none
// for the rest, which the statement does not write. An ended change merges
// nothing, so it is ready for nobody and waits on nobody; and a row left
// waiting would stay in code_mergeability_waits_idx, which needs=person walks
// and whose partial predicate keeps it to the changes in front of the
// company's persons, until its repository's purge — since no record of its
// home's refreshes an ended change. A reopen moves the change's version, so
// its home's next pass judges it again (changesdue.go's refresh term).
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
		return 0, fmt.Errorf("code: clear the stored readiness of %d ended changes: %w", len(ids), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("code: clear the stored readiness of %d ended changes: %w", len(ids), err)
	}
	return int(n), nil
}

// A mergeability's writes: the row by the change's key, replaced whole by
// every merge the decide took; a refresh's readiness, written by the same key
// only while the row is the merge the record names; a kept re-evaluation's
// moves, by the same key only while they move the row forward; and the
// readiness of a list of ended changes cleared, each by its key, only where
// the row holds an answer — a write naming waits_person rewrites the row's
// entry in code_mergeability_waits_idx whatever the value, so a row with
// nothing to clear is not written.
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
	keptMergeabilitySQL = `UPDATE code_mergeability SET
			target_oid = CASE WHEN ?1 THEN ?2 ELSE target_oid END,
			target_position = CASE WHEN ?1 THEN ?3 ELSE target_position END,
			at = CASE WHEN ?1 THEN ?4 ELSE at END,
			change_version = CASE WHEN ?5 THEN ?6 ELSE change_version END,
			readiness_version = CASE WHEN ?5 THEN ?7 ELSE readiness_version END,
			checks_position = CASE WHEN ?5 THEN ?8 ELSE checks_position END
		WHERE change_id = ?9 AND revision = ?10 AND (NOT ?1 OR target_position <= ?3)
			AND (NOT ?5 OR (change_version <= ?6 AND readiness_version <= ?7 AND checks_position <= ?8))`
	clearReadinessSQL = `UPDATE code_mergeability SET ready = 0, waits_person = 0 WHERE rowid IN (SELECT g.rowid
		FROM json_each(?) AS j CROSS JOIN code_mergeability AS g
		WHERE g.change_id = CAST(j.value AS TEXT) AND (g.ready = 1 OR g.waits_person = 1))`
)
