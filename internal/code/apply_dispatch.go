package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// A HEAD'S DISPATCH, APPLIED: the join of a check_dispatched record into its
// target's reading and its heads' dispatches, and a purge part's removal of
// both — the applier's writes of code_check_targets and code_check_dispatches,
// and no other code's (the replicated estate is written by its log's applier
// alone). checksdispatch.go has the record, its decide and why the home's work
// list is these rows.

// laterReading reports whether a record that read a target's tip at the
// position tip, itself at position at, is kept over the one a row holds
// (heldTip, heldAt — heldAt zero for no row): the later tip, then the later
// record. A total order — no two records share a position — so a join of any
// of them in any order keeps one.
func laterReading(tip, at, heldTip, heldAt int64) bool {
	return heldAt == 0 || tip > heldTip || (tip == heldTip && at > heldAt)
}

// applyCheckDispatched joins a dispatch into each of its attempts' rows — as a
// check_queued record of each joins its one ([queuedPart], M5-D18) — and into
// its target's row and each of its heads' rows, answering the rows it wrote.
func applyCheckDispatched(ctx context.Context, tx *sql.Tx, at applyContext, d CheckDispatched) (int, error) {
	tip, recorded := d.TipPosition.Packed(), store.EncodeTime(at.brokerAt)
	rows := 0
	for _, a := range d.Attempts {
		if a.RepoID != d.RepoID {
			return 0, fmt.Errorf("code: the dispatch at %s of repository %s carries an attempt of %s — no "+
				"writer forms it", at.position, d.RepoID, a.RepoID)
		}
		joined, err := joinCheck(ctx, tx, at, a.CheckKey, queuedPart(a, at))
		if err != nil {
			return 0, err
		}
		rows += joined
	}
	heldTip, heldAt, err := readingIn(ctx, tx, readCheckTargetSQL, d.RepoID, d.Target)
	if err != nil {
		return 0, fmt.Errorf("code: read target %s's checks file at %s: %w", d.Target, at.position, err)
	}
	if laterReading(tip, at.packed, heldTip, heldAt) {
		if _, err := tx.ExecContext(ctx, writeCheckTargetSQL, d.RepoID, d.Target, d.Tip, tip, d.DefsOID, recorded,
			at.packed); err != nil {
			return 0, fmt.Errorf("code: write target %s's checks file at %s: %w", d.Target, at.position, err)
		}
		rows++
	}
	for _, h := range d.Heads {
		heldTip, heldAt, err := readingIn(ctx, tx, readCheckDispatchSQL, d.RepoID, h.OID, d.Target)
		if err != nil {
			return 0, fmt.Errorf("code: read head %s's dispatch at %s: %w", h.OID, at.position, err)
		}
		if !laterReading(tip, at.packed, heldTip, heldAt) {
			continue
		}
		if _, err := tx.ExecContext(ctx, writeCheckDispatchSQL, d.RepoID, h.OID, d.Target, d.DefsOID, h.ChangeID,
			h.Revision, tip, recorded, at.packed); err != nil {
			return 0, fmt.Errorf("code: write head %s's dispatch at %s: %w", h.OID, at.position, err)
		}
		rows++
	}
	return rows, nil
}

// readingIn is the (tip position, record position) a row of the targets' or
// the dispatches' holds by query's key, zeros for none.
func readingIn(ctx context.Context, tx *sql.Tx, query string, key ...any) (tip, at int64, err error) {
	switch err := tx.QueryRowContext(ctx, query, key...).Scan(&tip, &at); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, nil
	case err != nil:
		return 0, 0, err
	}
	return tip, at, nil
}

// purgeDispatches deletes, for a purge part, a deleted repository's dispatch
// rows and then its targets' rows, at most budget of them, answering the rows
// it deleted: a few dozen bytes each, so a part weighs them by their count
// alone. A part reaches them after the checks and before the merge queues'
// rows ([purgeQueues]).
func purgeDispatches(ctx context.Context, tx *sql.Tx, repoID string, budget int) (int, error) {
	deleted := 0
	for _, stmt := range []string{purgeSomeDispatchesSQL, purgeSomeCheckTargetsSQL} {
		if budget-deleted <= 0 {
			break
		}
		res, err := tx.ExecContext(ctx, stmt, repoID, budget-deleted)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's dispatches: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	return deleted, nil
}

// The apply's statements, each registered in plan_test.go: a target's row and
// a head's dispatch row, each by its whole key — read for the join — and
// written whole; and a purge's, by the repository's prefix of each key.
const (
	// purgeSomeDispatchesSQL and purgeSomeCheckTargetsSQL delete a purge
	// part's share, the rowid idiom; the final purge's two delete the rest.
	purgeSomeDispatchesSQL = `DELETE FROM code_check_dispatches WHERE rowid IN (SELECT rowid
		FROM code_check_dispatches INDEXED BY ` + checkDispatchesKey + ` WHERE repo_id = ? LIMIT ?)`
	purgeSomeCheckTargetsSQL = `DELETE FROM code_check_targets WHERE rowid IN (SELECT rowid
		FROM code_check_targets INDEXED BY ` + checkTargetsKey + ` WHERE repo_id = ? LIMIT ?)`
	purgeAllDispatchesSQL   = `DELETE FROM code_check_dispatches WHERE repo_id = ?`
	purgeAllCheckTargetsSQL = `DELETE FROM code_check_targets WHERE repo_id = ?`

	// checkTargetsKey and checkDispatchesKey are the two tables' PRIMARY
	// KEY indexes, (repo_id, target_ref) and (repo_id, commit_oid,
	// target_ref).
	checkTargetsKey    = "sqlite_autoindex_code_check_targets_1"
	checkDispatchesKey = "sqlite_autoindex_code_check_dispatches_1"

	readCheckTargetSQL = `SELECT tip_position, position FROM code_check_targets INDEXED BY ` + checkTargetsKey + `
		WHERE repo_id = ? AND target_ref = ?`
	writeCheckTargetSQL = `INSERT INTO code_check_targets (repo_id, target_ref, tip_oid, tip_position, defs_oid,
			at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, target_ref) DO UPDATE SET tip_oid = excluded.tip_oid,
			tip_position = excluded.tip_position, defs_oid = excluded.defs_oid, at = excluded.at,
			position = excluded.position`
	readCheckDispatchSQL = `SELECT tip_position, position FROM code_check_dispatches INDEXED BY ` +
		checkDispatchesKey + ` WHERE repo_id = ? AND commit_oid = ? AND target_ref = ?`
	writeCheckDispatchSQL = `INSERT INTO code_check_dispatches (repo_id, commit_oid, target_ref, defs_oid,
			change_id, revision, tip_position, at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, commit_oid, target_ref) DO UPDATE SET defs_oid = excluded.defs_oid,
			change_id = excluded.change_id, revision = excluded.revision, tip_position = excluded.tip_position,
			at = excluded.at, position = excluded.position`
)
