package code

import (
	"context"
	"database/sql"
	"fmt"
)

// A PURGE PART'S CHANGES: what a repository's purge does to the rows its
// changes are kept in — their revisions, authors and bodies on the
// repository's subject, and their conversations on each change's own.

// purgedChange is one change as a purge part's walk reads it: its id, what its
// wide rows weigh — its body, and its conversation's text — and its rows
// beside its own, each counted to the part's bound, in the order a cut takes
// them ([purgeChangeWalkSQL]).
type purgedChange struct {
	id              string
	bodyBytes, text int64
	// revisions, authors, reviewers, participants and findings are the
	// narrow rows a cut takes first, in this order; comments, findingTexts,
	// reviews and facts the wide ones it takes after them, by bytes as well
	// as rows; body, mergeability and conversation the rows that go only
	// with their change, whole — a mergeability one row a change, whose
	// conflicted paths are bounded (MaxConflictBytes) and weighed in its
	// conversation's text.
	revisions, authors, reviewers, participants, findings int
	comments, findingTexts, reviews, facts                int
	body, mergeability, conversation                      int
}

// rows is every row of c a part deletes taking it whole, its own included.
func (c purgedChange) rows() int {
	return c.revisions + c.authors + c.reviewers + c.participants + c.findings + c.comments + c.findingTexts +
		c.reviews + c.facts + c.body + c.mergeability + c.conversation + 1
}

// wideCut is a cut change's wide rows as [purgeCutWide] takes them, one table
// after another: what is left of a part's rows and what it weighs, and what
// the cut deleted and freed — the bytes its conversation's text_bytes loses.
type wideCut struct {
	left           int
	weighed, freed int64
	deleted        int
}

// purgeChanges deletes, for a purge part, at most budget rows of a deleted
// repository's changes, and only while what the part's wide rows weigh —
// weighed, the push rows it already took, and then each change's body and
// conversation it takes — is under PurgePartBytes; it answers how many rows it
// deleted and what the part's wide rows weigh with them, which the part's
// checks take what is left of ([purgeChecks]).
//
// A CHANGE GOES WHOLE, AND ITS ROW LAST. A change's revisions, authors and
// body, and every row of its conversation, carry no repo_id: the only way to
// them from the repository is the change's own row, so a row deleted before
// them would leave rows no later part — and no final purge — could find. The
// changes are taken in number order, and every node takes the same ones,
// because the walk reads them in the order of [changesNumberKey] and a
// repository's numbers never repeat.
//
// SET-BASED OVER THE CHANGES THAT FIT, ONE CUT AT MOST. The walk counts each
// change's rows, each table to the part's bound, and takes changes while the
// whole of one fits what is left of the budget and the part weighs under
// PurgePartBytes; their rows go in one statement a table over their ids. The
// change the walk stops at is CUT:
//
//   - stopped by its ROWS, it loses its narrow rows — its oldest revisions,
//     then its authors, its reviewers, its participants and its findings, each
//     in its key's order — and then its wide ones — its comments, oldest
//     thread first, its findings' texts and its reviews — up to what is left
//     of both bounds;
//   - stopped by its CONVERSATION'S TEXT — rows that fit, and wide rows that
//     would take the part past PurgePartBytes — it loses its wide rows alone,
//     by the same orders, until the part weighs PurgePartBytes: a
//     conversation can weigh many times a part — a thousand comments at
//     MaxCommentBytes are 32 MiB — and taken whole by its rows alone it would
//     hold the write lane for every mebibyte past the bound; never cut, it
//     would stop every part at it and the purge would never end.
//
// Its body, its mergeability, its conversation's row and its own row wait for a
// part that finds the rest gone. A change's BODY never stops the walk: it is one row of at
// most MaxChangeBodyBytes, so a change whose conversation fits is taken whole
// while the part weighs under the bound, its body taking the part past it by
// at most that much, and the next part starts with the change after it.
// Measured on a part of a thousand changes of one revision and one author
// each: deleting one change at a time, four statements a change, held the
// transaction 850 ms, over three times the 250 ms after which the framework
// ends one at the next record (statelog.ApplyTxTimeBudget); set-based it held
// it 62 ms, and a part of 3 000 refs 118 ms (one run each, under the race
// detector on four cores). BenchmarkApplyAPurgePart's arms over changes
// measure it without the detector (doc.go's measured section).
//
// AND WHILE THE BYTES LAST. A change's body and its conversation's wide rows
// are the wide values it keeps, and deleting them frees overflow pages a count
// does not see. So they share the pushes' budget, each change weighed by its
// body_bytes and its conversation's text_bytes — columns of narrow rows the
// walk reads anyway, never the wide rows themselves — and a cut's wide rows by
// their own widths, read off the rows it then deletes, so it reads no wide row
// it keeps. A part whose push rows reached the bound takes no change; it
// advanced by those.
func purgeChanges(ctx context.Context, tx *sql.Tx, repoID string, budget int,
	weighed int64) (int, int64, error) {
	if weighed >= PurgePartBytes {
		return 0, weighed, nil
	}
	rows, err := tx.QueryContext(ctx, purgeChangeWalkSQL, repoID, budget)
	if err != nil {
		return 0, 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
	}
	var whole []string
	left := budget
	var cut purgedChange
	var byText bool
	for weighed < PurgePartBytes && rows.Next() {
		var c purgedChange
		if err := rows.Scan(&c.id, &c.bodyBytes, &c.text, &c.revisions, &c.authors, &c.reviewers,
			&c.participants, &c.findings, &c.comments, &c.findingTexts, &c.reviews, &c.facts, &c.body,
			&c.mergeability, &c.conversation); err != nil {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
		}
		switch {
		case c.rows() > left:
			// THE STOP BY ITS ROWS: its cut takes what is left of both
			// bounds.
			cut = c
		case weighed+c.text > PurgePartBytes:
			// THE STOP BY ITS CONVERSATION'S TEXT: its wide rows until the
			// bytes are spent.
			cut, byText = c, true
		default:
			whole = append(whole, c.id)
			left -= c.rows()
			weighed += c.bodyBytes + c.text
			continue
		}
		break
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
	}
	_ = rows.Close()

	deleted := 0
	if len(whole) > 0 {
		list, err := jsonList(whole)
		if err != nil {
			return 0, 0, err
		}
		for _, stmt := range []string{purgeChangeRevisionsSQL, purgeChangeAuthorsSQL, purgeChangeReviewersSQL,
			purgeChangeParticipantsSQL, purgeChangeFindingsSQL, purgeChangeCommentsSQL, purgeChangeFindingTextsSQL,
			purgeChangeReviewsSQL, purgeChangeFactsSQL, purgeChangeBodiesSQL, purgeChangeMergeabilitySQL,
			purgeChangeConversationsSQL, purgeChangeRowsSQL} {
			res, err := tx.ExecContext(ctx, stmt, list)
			if err != nil {
				return 0, 0, fmt.Errorf("code: purge %s's changes: %w", repoID, err)
			}
			n, _ := res.RowsAffected()
			deleted += int(n)
		}
	}
	if cut.id == "" {
		return deleted, weighed, nil
	}
	// THE CUT CHANGE needs more than is left — of the rows, or of the bytes
	// its conversation's text needs — so its body, its mergeability and its
	// rows are never reached here.
	if !byText {
		for _, stmt := range []string{purgeCutRevisionsSQL, purgeCutAuthorsSQL, purgeCutReviewersSQL,
			purgeCutParticipantsSQL, purgeCutFindingsSQL} {
			if left == 0 {
				break
			}
			res, err := tx.ExecContext(ctx, stmt, cut.id, left)
			if err != nil {
				return 0, 0, fmt.Errorf("code: purge change %s of %s: %w", cut.id, repoID, err)
			}
			n, _ := res.RowsAffected()
			deleted += int(n)
			left -= int(n)
		}
	}
	// ITS WIDE ROWS, every one's text counted in its conversation's
	// text_bytes: its comments, oldest thread first, its findings' texts by
	// finding, its reviews in the order given, then its revisions' facts,
	// oldest revision first.
	wide := wideCut{left: left, weighed: weighed}
	if wide, err = purgeCutWide(ctx, tx, purgeCutCommentWidthsSQL, purgeCutCommentsSQL, cut.id,
		wide); err != nil {
		return 0, 0, fmt.Errorf("code: purge change %s's comments of %s: %w", cut.id, repoID, err)
	}
	if wide, err = purgeCutWide(ctx, tx, purgeCutFindingTextWidthsSQL, purgeCutFindingTextsSQL, cut.id,
		wide); err != nil {
		return 0, 0, fmt.Errorf("code: purge change %s's findings' texts of %s: %w", cut.id, repoID, err)
	}
	if wide, err = purgeCutWide(ctx, tx, purgeCutReviewWidthsSQL, purgeCutReviewsSQL, cut.id,
		wide); err != nil {
		return 0, 0, fmt.Errorf("code: purge change %s's reviews of %s: %w", cut.id, repoID, err)
	}
	if wide, err = purgeCutWide(ctx, tx, purgeCutFactWidthsSQL, purgeCutFactsSQL, cut.id, wide); err != nil {
		return 0, 0, fmt.Errorf("code: purge change %s's revisions' facts of %s: %w", cut.id, repoID, err)
	}
	deleted += wide.deleted
	if wide.freed > 0 {
		if _, err := tx.ExecContext(ctx, unweighConversationSQL, wide.freed, cut.id); err != nil {
			return 0, 0, fmt.Errorf("code: weigh change %s's conversation again: %w", cut.id, err)
		}
		deleted++
	}
	return deleted, wide.weighed, nil
}

// purgeCutWide deletes a cut change's rows of one wide table — widthsSQL its
// rows' widths in the order deleteSQL takes the first n of — at most what is
// left of the part's rows and only while what the part weighs, each row's
// width added, is under PurgePartBytes, the first of them whatever it weighs,
// so a cut always advances; it answers the cut with them taken.
//
// THE WIDTHS ARE READ IN THE ORDER THE DELETE TAKES, and only as far as it
// takes: the delete takes the first n of the same order, which is total — a
// thread's comments are each their own record's, at positions of their own; a
// finding's text is keyed by its id; a reviewer's reviews are each at a
// position of its own — so every node deletes the same rows and reads no wide
// row it keeps.
func purgeCutWide(ctx context.Context, tx *sql.Tx, widthsSQL, deleteSQL, changeID string,
	cut wideCut) (wideCut, error) {

	if cut.left <= 0 || cut.weighed >= PurgePartBytes {
		return cut, nil
	}
	rows, err := tx.QueryContext(ctx, widthsSQL, changeID, cut.left)
	if err != nil {
		return cut, fmt.Errorf("weigh its rows: %w", err)
	}
	var take int
	var freed int64
	for cut.weighed+freed < PurgePartBytes && rows.Next() {
		var width int64
		if err := rows.Scan(&width); err != nil {
			_ = rows.Close()
			return cut, fmt.Errorf("weigh its rows: %w", err)
		}
		take++
		freed += width
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return cut, fmt.Errorf("weigh its rows: %w", err)
	}
	_ = rows.Close()
	if take == 0 {
		return cut, nil
	}
	res, err := tx.ExecContext(ctx, deleteSQL, changeID, take)
	if err != nil {
		return cut, fmt.Errorf("delete its rows: %w", err)
	}
	n, _ := res.RowsAffected()
	cut.left -= int(n)
	cut.deleted += int(n)
	cut.weighed += freed
	cut.freed += freed
	return cut, nil
}

// A purge's statements over a repository's changes: every one with a WHERE is
// registered in plan_test.go, so the pinned driver's planner is held to the
// seek each is written for.
const (
	// purgeChangeWalkSQL is a repository's changes in number order, each with
	// what its body weighs and what its conversation's text does, its rows of
	// each table counted at most ?2 deep — in the order a cut takes them — and
	// its body and conversation rows: a count is asked only of whether the
	// whole of one change fits, and the walk stops at the first that does not,
	// so it reads a part's bound of dependents and one change's more, whatever
	// the repository holds. Every wide row is COUNTED from its key's index,
	// never read, so the walk never touches a wide row's overflow pages: what
	// they weigh is read off the narrow rows of the change and its
	// conversation.
	purgeChangeWalkSQL = `SELECT c.id, c.body_bytes,
		COALESCE((SELECT text_bytes FROM code_conversations WHERE change_id = c.id), 0),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_revisions WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_change_authors WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_reviewers WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_participants WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_findings WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_comments WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_finding_texts WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_reviews WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_revision_facts WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM code_change_bodies WHERE change_id = c.id),
		(SELECT COUNT(*) FROM code_mergeability WHERE change_id = c.id),
		(SELECT COUNT(*) FROM code_conversations WHERE change_id = c.id)
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
	purgeChangeReviewersSQL = `DELETE FROM code_reviewers WHERE rowid IN (SELECT r.rowid
		FROM json_each(?) AS j CROSS JOIN code_reviewers AS r
		WHERE r.change_id = CAST(j.value AS TEXT))`
	purgeChangeParticipantsSQL = `DELETE FROM code_participants WHERE rowid IN (SELECT p.rowid
		FROM json_each(?) AS j CROSS JOIN code_participants AS p
		WHERE p.change_id = CAST(j.value AS TEXT))`
	purgeChangeFindingsSQL = `DELETE FROM code_findings WHERE rowid IN (SELECT f.rowid
		FROM json_each(?) AS j CROSS JOIN code_findings AS f
		WHERE f.change_id = CAST(j.value AS TEXT))`
	purgeChangeCommentsSQL = `DELETE FROM code_comments WHERE rowid IN (SELECT m.rowid
		FROM json_each(?) AS j CROSS JOIN code_comments AS m
		WHERE m.change_id = CAST(j.value AS TEXT))`
	purgeChangeFindingTextsSQL = `DELETE FROM code_finding_texts WHERE rowid IN (SELECT x.rowid
		FROM json_each(?) AS j CROSS JOIN code_finding_texts AS x
		WHERE x.change_id = CAST(j.value AS TEXT))`
	purgeChangeReviewsSQL = `DELETE FROM code_reviews WHERE rowid IN (SELECT v.rowid
		FROM json_each(?) AS j CROSS JOIN code_reviews AS v
		WHERE v.change_id = CAST(j.value AS TEXT))`
	purgeChangeFactsSQL = `DELETE FROM code_revision_facts WHERE rowid IN (SELECT t.rowid
		FROM json_each(?) AS j CROSS JOIN code_revision_facts AS t
		WHERE t.change_id = CAST(j.value AS TEXT))`
	purgeChangeBodiesSQL = `DELETE FROM code_change_bodies WHERE rowid IN (SELECT b.rowid
		FROM json_each(?) AS j CROSS JOIN code_change_bodies AS b
		WHERE b.change_id = CAST(j.value AS TEXT))`
	purgeChangeMergeabilitySQL = `DELETE FROM code_mergeability WHERE rowid IN (SELECT g.rowid
		FROM json_each(?) AS j CROSS JOIN code_mergeability AS g
		WHERE g.change_id = CAST(j.value AS TEXT))`
	purgeChangeConversationsSQL = `DELETE FROM code_conversations WHERE rowid IN (SELECT v.rowid
		FROM json_each(?) AS j CROSS JOIN code_conversations AS v
		WHERE v.change_id = CAST(j.value AS TEXT))`
	purgeChangeRowsSQL = `DELETE FROM code_changes WHERE rowid IN (SELECT c.rowid
		FROM json_each(?) AS j CROSS JOIN code_changes AS c
		WHERE c.id = CAST(j.value AS TEXT))`
	// The cut change's narrow rows, each table in its key's order, no more
	// than the part has left: its oldest revisions, its authors, its
	// reviewers, its participants and its findings.
	purgeCutRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT rowid
		FROM code_revisions WHERE change_id = ? ORDER BY n LIMIT ?)`
	purgeCutAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT rowid
		FROM code_change_authors WHERE change_id = ? ORDER BY author LIMIT ?)`
	purgeCutReviewersSQL = `DELETE FROM code_reviewers WHERE rowid IN (SELECT rowid
		FROM code_reviewers WHERE change_id = ? ORDER BY reviewer LIMIT ?)`
	purgeCutParticipantsSQL = `DELETE FROM code_participants WHERE rowid IN (SELECT rowid
		FROM code_participants WHERE change_id = ? ORDER BY handle LIMIT ?)`
	purgeCutFindingsSQL = `DELETE FROM code_findings WHERE rowid IN (SELECT rowid
		FROM code_findings WHERE change_id = ? ORDER BY raiser, state, revision, id LIMIT ?)`
	// The cut change's wide rows, each table's widths read as far as the cut
	// takes and the delete of the first n of the same order: its comments,
	// oldest thread first; its findings' texts by finding; its reviews in the
	// order given; its revisions' facts, oldest revision first.
	purgeCutCommentWidthsSQL = `SELECT body_bytes FROM code_comments WHERE change_id = ?
		ORDER BY thread_id, position LIMIT ?`
	purgeCutCommentsSQL = `DELETE FROM code_comments WHERE rowid IN (SELECT rowid
		FROM code_comments WHERE change_id = ? ORDER BY thread_id, position LIMIT ?)`
	purgeCutFindingTextWidthsSQL = `SELECT bytes FROM code_finding_texts WHERE change_id = ?
		ORDER BY finding_id LIMIT ?`
	purgeCutFindingTextsSQL = `DELETE FROM code_finding_texts WHERE rowid IN (SELECT rowid
		FROM code_finding_texts WHERE change_id = ? ORDER BY finding_id LIMIT ?)`
	purgeCutReviewWidthsSQL = `SELECT summary_bytes FROM code_reviews WHERE change_id = ?
		ORDER BY position LIMIT ?`
	purgeCutReviewsSQL = `DELETE FROM code_reviews WHERE rowid IN (SELECT rowid
		FROM code_reviews WHERE change_id = ? ORDER BY position LIMIT ?)`
	purgeCutFactWidthsSQL = `SELECT bytes FROM code_revision_facts WHERE change_id = ?
		ORDER BY n LIMIT ?`
	purgeCutFactsSQL = `DELETE FROM code_revision_facts WHERE rowid IN (SELECT rowid
		FROM code_revision_facts WHERE change_id = ? ORDER BY n LIMIT ?)`
	unweighConversationSQL = `UPDATE code_conversations SET text_bytes = text_bytes - ? WHERE change_id = ?`
	// The final purge's, unbounded because the decide forms it only once its
	// bounded count found at most PurgePartRows rows left, and what the
	// bodies and the conversations among them weigh within PurgePartBytes —
	// the dependents first, reached through their changes, then the changes.
	purgeAllRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_revisions AS r
		WHERE c.repo_id = ? AND r.change_id = c.id)`
	purgeAllAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_authors AS a
		WHERE c.repo_id = ? AND a.change_id = c.id)`
	purgeAllReviewersSQL = `DELETE FROM code_reviewers WHERE rowid IN (SELECT r.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_reviewers AS r
		WHERE c.repo_id = ? AND r.change_id = c.id)`
	purgeAllParticipantsSQL = `DELETE FROM code_participants WHERE rowid IN (SELECT p.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_participants AS p
		WHERE c.repo_id = ? AND p.change_id = c.id)`
	purgeAllFindingsSQL = `DELETE FROM code_findings WHERE rowid IN (SELECT f.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_findings AS f
		WHERE c.repo_id = ? AND f.change_id = c.id)`
	purgeAllCommentsSQL = `DELETE FROM code_comments WHERE rowid IN (SELECT m.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_comments AS m
		WHERE c.repo_id = ? AND m.change_id = c.id)`
	purgeAllFindingTextsSQL = `DELETE FROM code_finding_texts WHERE rowid IN (SELECT x.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_finding_texts AS x
		WHERE c.repo_id = ? AND x.change_id = c.id)`
	purgeAllReviewsSQL = `DELETE FROM code_reviews WHERE rowid IN (SELECT v.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_reviews AS v
		WHERE c.repo_id = ? AND v.change_id = c.id)`
	purgeAllFactsSQL = `DELETE FROM code_revision_facts WHERE rowid IN (SELECT t.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_revision_facts AS t
		WHERE c.repo_id = ? AND t.change_id = c.id)`
	purgeAllBodiesSQL = `DELETE FROM code_change_bodies WHERE rowid IN (SELECT b.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_bodies AS b
		WHERE c.repo_id = ? AND b.change_id = c.id)`
	purgeAllMergeabilitySQL = `DELETE FROM code_mergeability WHERE rowid IN (SELECT g.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_mergeability AS g
		WHERE c.repo_id = ? AND g.change_id = c.id)`
	purgeAllConversationsSQL = `DELETE FROM code_conversations WHERE rowid IN (SELECT v.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_conversations AS v
		WHERE c.repo_id = ? AND v.change_id = c.id)`
	purgeAllChangesSQL = `DELETE FROM code_changes INDEXED BY ` + changesNumberKey + ` WHERE repo_id = ?`
)
