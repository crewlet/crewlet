package code

import (
	"context"
	"database/sql"
	"fmt"
)

// purgeChanges deletes at most budget rows of a deleted repository's changes,
// for a purge part, and answers how many it deleted.
//
// A CHANGE GOES WHOLE, AND ITS ROW LAST. A change's revisions and authors
// carry no repo_id: the only way to them from the repository is the change's
// own row, so a row deleted before them would leave rows no later part — and
// no final purge — could find. The changes are taken in number order, and every
// node takes the same ones, because the walk reads them in the order of
// [changesNumberKey] and a repository's numbers never repeat.
//
// SET-BASED OVER THE CHANGES THAT FIT, ONE CUT AT MOST. The walk counts each
// change's revisions and authors, each to the part's bound, and takes changes
// while the whole of one fits what is left of the budget; their revisions,
// authors and rows go in three statements over their ids. The change the walk
// stops at loses only revisions and then authors — its oldest revisions first,
// its authors in key order — up to what is left, and its row waits for a part
// that finds the rest gone. Measured on a part of a thousand changes of one
// revision and one author each — the most statements a part of 3 000 rows
// could need: deleting one change at a time, four statements a change, held
// the transaction 850 ms, over three times the 250 ms after which the
// framework ends one at the next record (statelog.ApplyTxTimeBudget); this
// holds it 62 ms, and a part of 3 000 refs 118 ms (one run each, under the race
// detector on four cores). BenchmarkApplyAPurgePart's changes and cut arms
// measure it without the detector (doc.go's measured section).
func purgeChanges(ctx context.Context, tx *sql.Tx, repoID string, budget int) (int, error) {
	rows, err := tx.QueryContext(ctx, purgeChangeWalkSQL, repoID, budget)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
	}
	var whole []string
	left, cut := budget, ""
	for rows.Next() {
		var id string
		var revisions, authors int
		if err := rows.Scan(&id, &revisions, &authors); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
		}
		need := revisions + authors + 1
		if need > left {
			cut = id
			break
		}
		whole = append(whole, id)
		left -= need
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
	}
	_ = rows.Close()

	deleted := 0
	if len(whole) > 0 {
		list, err := jsonList(whole)
		if err != nil {
			return 0, err
		}
		for _, stmt := range []string{purgeChangeRevisionsSQL, purgeChangeAuthorsSQL, purgeChangeRowsSQL} {
			res, err := tx.ExecContext(ctx, stmt, list)
			if err != nil {
				return 0, fmt.Errorf("code: purge %s's changes: %w", repoID, err)
			}
			n, _ := res.RowsAffected()
			deleted += int(n)
		}
	}
	// THE CHANGE THE WALK STOPPED AT needs more than is left — its revisions
	// and authors alone fill it — so its row is never reached here.
	for _, stmt := range []string{purgeCutRevisionsSQL, purgeCutAuthorsSQL} {
		if cut == "" || left == 0 {
			break
		}
		res, err := tx.ExecContext(ctx, stmt, cut, left)
		if err != nil {
			return 0, fmt.Errorf("code: purge change %s of %s: %w", cut, repoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		left -= int(n)
	}
	return deleted, nil
}

// A purge's statements over a repository's changes: every one with a WHERE is
// registered in plan_test.go, so the pinned driver's planner is held to the
// seek each is written for.
const (
	// purgeChangeWalkSQL is a repository's changes in number order, each with
	// its revisions and its authors counted at most ?2 deep: a count is asked
	// only of whether the whole of one change fits, and the walk stops at the
	// first that does not, so it reads a part's bound of dependents and one
	// change's more, whatever the repository holds.
	purgeChangeWalkSQL = `SELECT c.id,
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_revisions WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_change_authors WHERE change_id = c.id LIMIT ?2))
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` WHERE c.repo_id = ?1
		ORDER BY c.number LIMIT ?2`
	// The changes the walk took whole, by the JSON list of their ids — each
	// driven from the list, one seek of a key an element, as planner.go says
	// a batch is.
	purgeChangeRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM json_each(?) AS j CROSS JOIN code_revisions AS r
		WHERE r.change_id = CAST(j.value AS TEXT))`
	purgeChangeAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM json_each(?) AS j CROSS JOIN code_change_authors AS a
		WHERE a.change_id = CAST(j.value AS TEXT))`
	purgeChangeRowsSQL = `DELETE FROM code_changes WHERE rowid IN (SELECT c.rowid
		FROM json_each(?) AS j CROSS JOIN code_changes AS c
		WHERE c.id = CAST(j.value AS TEXT))`
	// purgeCutRevisionsSQL and purgeCutAuthorsSQL are the cut change's
	// oldest revisions and its authors in key order, no more than the part
	// has left.
	purgeCutRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT rowid
		FROM code_revisions WHERE change_id = ? ORDER BY n LIMIT ?)`
	purgeCutAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT rowid
		FROM code_change_authors WHERE change_id = ? ORDER BY author LIMIT ?)`
	// The final purge's, unbounded because the decide forms it only once its
	// bounded count found at most PurgePartRows rows left — the dependents
	// first, reached through their changes, then the changes.
	purgeAllRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_revisions AS r
		WHERE c.repo_id = ? AND r.change_id = c.id)`
	purgeAllAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_authors AS a
		WHERE c.repo_id = ? AND a.change_id = c.id)`
	purgeAllChangesSQL = `DELETE FROM code_changes INDEXED BY ` + changesNumberKey + ` WHERE repo_id = ?`
)
