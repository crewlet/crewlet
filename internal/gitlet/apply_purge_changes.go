package gitlet

import (
	"context"
	"database/sql"
	"fmt"
)

// A purge part's changes: their revisions, authors, moves and bodies on the
// repository's subject, and their conversations on each change's own.

// purgedChange is one change as a purge part's walk reads it: what its body and
// conversation's text weigh, and its rows beside its own, each counted to the
// part's bound ([purgeChangeWalkSQL]).
type purgedChange struct {
	id string
	// state is counted out of the repository's counts when the change goes
	// whole (apply_counts.go).
	state           ChangeState
	bodyBytes, text int64
	// A cut takes the narrow rows first, in this order, then its notices, then
	// the wide ones by bytes as well as rows; body, mergeability and
	// conversation go only with the change, whole (a mergeability's conflicted
	// paths are bounded by MaxConflictBytes and weighed in the text).
	revisions, authors, moves, reviewers, participants, findings int
	comments, findingTexts, reviews, facts                       int
	body, mergeability, conversation                             int
	// notices is code_changes.notices ([countNotices]): what a part charges
	// for them, never the rows this node finds, which another may not.
	notices int
}

// rows is every row a part charges for c taken whole, its own and its notices'
// count included.
func (c purgedChange) rows() int {
	return c.revisions + c.authors + c.moves + c.reviewers + c.participants + c.findings + c.comments + c.findingTexts +
		c.reviews + c.facts + c.body + c.mergeability + c.conversation + c.notices + 1
}

// changesPurged is what a purge part did to a repository's changes: spent, the
// rows charged to its bound (notices by count, so the same on every node);
// deleted, every row it deleted or rewrote on this node; and weighed, what the
// part's wide rows weigh with them.
type changesPurged struct {
	spent, deleted int
	weighed        int64
}

// wideCut threads [purgeCutWide] from one wide table to the next: the part's
// rows left and weight, and what the cut deleted and freed (the bytes its
// conversation's text_bytes loses).
type wideCut struct {
	left           int
	weighed, freed int64
	deleted        int
}

// purgeChanges deletes, for a purge part, at most budget rows of a deleted
// repository's changes and their notices, while the part's wide rows (weighed,
// the push rows', then each change's body_bytes and conversation text_bytes,
// whose overflow pages no row count sees) weigh under PurgePartBytes. It
// answers the rows charged, deleted and weighed ([purgeChecks] take the rest).
//
// A CHANGE GOES WHOLE, ITS ROW LAST: only it reaches its dependents, so one
// deleted first strands them (doc.go, "A change is rows on its repository's
// subject"). Every node takes the same changes, in [changesNumberKey] order
// (numbers never repeat), set-based: one statement a table over the ids that
// fit. The change the walk stops at is CUT; its body, mergeability,
// conversation row and own row wait for a part that finds the rest gone.
func (a *Applier) purgeChanges(ctx context.Context, tx *sql.Tx, at applyContext, repoID string, budget int,
	weighed int64) (changesPurged, error) {
	// A part whose push rows reached the bound takes no change; it advanced
	// by those.
	if weighed >= PurgePartBytes {
		return changesPurged{weighed: weighed}, nil
	}
	rows, err := tx.QueryContext(ctx, purgeChangeWalkSQL, repoID, budget)
	if err != nil {
		return changesPurged{}, fmt.Errorf("gitlet: read %s's changes to purge: %w", repoID, err)
	}
	var whole []string
	left := budget
	var cut purgedChange
	var byText bool
	// counted is the whole changes' notices by their counts, gone the changes
	// by state.
	counted := 0
	var gone ChangeCounts
	for weighed < PurgePartBytes && rows.Next() {
		var c purgedChange
		if err = rows.Scan(&c.id, &c.state, &c.bodyBytes, &c.text, &c.notices, &c.revisions, &c.authors, &c.moves,
			&c.reviewers,
			&c.participants, &c.findings, &c.comments, &c.findingTexts, &c.reviews, &c.facts, &c.body,
			&c.mergeability, &c.conversation); err != nil {
			_ = rows.Close()
			return changesPurged{}, fmt.Errorf("gitlet: read %s's changes to purge: %w", repoID, err)
		}
		switch {
		case c.rows() > left:
			// STOPPED BY ITS ROWS: the cut takes its narrow rows, its notices
			// and its wide rows, up to what is left of both bounds.
			cut = c
		case weighed+c.text > PurgePartBytes:
			// STOPPED BY ITS TEXT: the cut takes its wide rows alone until the
			// part weighs PurgePartBytes. A conversation can weigh many parts
			// (a thousand comments at MaxCommentBytes are 32 MiB): taken whole
			// it would hold the write lane past the bound, and never cut it
			// would stop every part and the purge would never end.
			cut, byText = c, true
		default:
			// A BODY never stops the walk: one row of at most
			// MaxChangeBodyBytes takes the part past the bound by at most that.
			whole = append(whole, c.id)
			left -= c.rows()
			counted += c.notices
			gone.add(c.state, 1)
			weighed += c.bodyBytes + c.text
			continue
		}
		break
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return changesPurged{}, fmt.Errorf("gitlet: read %s's changes to purge: %w", repoID, err)
	}
	_ = rows.Close()

	var done changesPurged
	if len(whole) > 0 {
		// SET-BASED: one change at a time, four statements each, held a part
		// of a thousand changes past statelog.ApplyTxTimeBudget
		// (BenchmarkApplyAPurgePart; doc.go's measured section).
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		list, err := jsonList(whole)
		if err != nil {
			return changesPurged{}, err
		}
		for _, stmt := range []string{purgeChangeRevisionsSQL, purgeChangeAuthorsSQL, purgeChangeMovesSQL,
			purgeChangeReviewersSQL,
			purgeChangeParticipantsSQL, purgeChangeFindingsSQL, purgeChangeCommentsSQL, purgeChangeFindingTextsSQL,
			purgeChangeReviewsSQL, purgeChangeFactsSQL, purgeChangeBodiesSQL, purgeChangeMergeabilitySQL,
			purgeChangeConversationsSQL, purgeChangeRowsSQL} {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			res, err := tx.ExecContext(ctx, stmt, list)
			if err != nil {
				return changesPurged{}, fmt.Errorf("gitlet: purge %s's changes: %w", repoID, err)
			}
			n, _ := res.RowsAffected()
			done.spent += int(n)
			done.deleted += int(n)
		}
		// Counted out of the repository's counts row, which the part rewrites
		// anyway: deleted, never spent, which decides later steps on every node.
		tallied, err := countChanges(ctx, tx, repoID, gone.negated())
		if err != nil {
			return changesPurged{}, err
		}
		done.deleted += tallied
		// NOTICES ARE CHARGED BY COUNT, never the rows this node holds: the
		// table is DIVERGENT, but the charge decides which replicated rows go
		// next, so it must agree on every node, and no node holds more rows
		// than the count. So a part deletes at most PurgePartRows rows of both
		// tables on every node, MaxRecordRows' reason.
		removed, err := a.removeNotices(ctx, tx, at, whole)
		if err != nil {
			return changesPurged{}, err
		}
		done.spent += counted
		done.deleted += removed
	}
	if cut.id == "" {
		done.weighed = weighed
		return done, nil
	}
	// THE CUT CHANGE: its body, mergeability and own rows are never reached
	// here.
	if !byText {
		for _, stmt := range []string{purgeCutRevisionsSQL, purgeCutAuthorsSQL, purgeCutMovesSQL,
			purgeCutReviewersSQL, purgeCutParticipantsSQL, purgeCutFindingsSQL} {
			if left == 0 {
				break
			}
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			res, err := tx.ExecContext(ctx, stmt, cut.id, left)
			if err != nil {
				return changesPurged{}, fmt.Errorf("gitlet: purge change %s of %s: %w", cut.id, repoID, err)
			}
			n, _ := res.RowsAffected()
			done.spent += int(n)
			done.deleted += int(n)
			left -= int(n)
		}
		// Its notices, oldest first, charged what is left and taken off its
		// count by exactly that, so the count stays the same on every node.
		if take := min(left, cut.notices); take > 0 {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			removed, err := a.removeCutNotices(ctx, tx, at, cut.id, take)
			if err != nil {
				return changesPurged{}, err
			}
			if _, err := tx.ExecContext(ctx, uncountNoticesSQL, take, cut.id); err != nil {
				return changesPurged{}, fmt.Errorf("gitlet: count change %s's notices again: %w", cut.id, err)
			}
			done.spent += take + 1
			done.deleted += removed + 1
			left -= take
		}
	}
	// Its wide rows, each counted in its conversation's text_bytes.
	wide := wideCut{left: left, weighed: weighed}
	if wide, err = purgeCutWide(ctx, tx, purgeCutCommentWidthsSQL, purgeCutCommentsSQL, cut.id,
		wide); err != nil {
		return changesPurged{}, fmt.Errorf("gitlet: purge change %s's comments of %s: %w", cut.id, repoID, err)
	}
	if wide, err = purgeCutWide(ctx, tx, purgeCutFindingTextWidthsSQL, purgeCutFindingTextsSQL, cut.id,
		wide); err != nil {
		return changesPurged{}, fmt.Errorf("gitlet: purge change %s's findings' texts of %s: %w", cut.id, repoID, err)
	}
	if wide, err = purgeCutWide(ctx, tx, purgeCutReviewWidthsSQL, purgeCutReviewsSQL, cut.id,
		wide); err != nil {
		return changesPurged{}, fmt.Errorf("gitlet: purge change %s's reviews of %s: %w", cut.id, repoID, err)
	}
	if wide, err = purgeCutWide(ctx, tx, purgeCutFactWidthsSQL, purgeCutFactsSQL, cut.id, wide); err != nil {
		return changesPurged{}, fmt.Errorf("gitlet: purge change %s's revisions' facts of %s: %w", cut.id,
			repoID, err)
	}
	done.spent += wide.deleted
	done.deleted += wide.deleted
	if wide.freed > 0 {
		if _, err := tx.ExecContext(ctx, unweighConversationSQL, wide.freed, cut.id); err != nil {
			return changesPurged{}, fmt.Errorf("gitlet: weigh change %s's conversation again: %w", cut.id, err)
		}
		done.spent++
		done.deleted++
	}
	done.weighed = wide.weighed
	return done, nil
}

// purgeCutWide deletes a cut change's rows of one wide table, at most what is
// left of the part's rows and while the part weighs under PurgePartBytes, the
// first whatever it weighs so a cut always advances, and answers the cut with
// them taken.
//
// widthsSQL reads the widths in deleteSQL's order and only as far as it takes:
// that order is total (each comment and review at a position of its own, each
// finding's text by its id), so every node deletes the same rows and none
// reads a wide row it keeps.
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
		if err = rows.Scan(&width); err != nil {
			_ = rows.Close()
			return cut, fmt.Errorf("weigh its rows: %w", err)
		}
		take++
		freed += width
	}
	if err = rows.Err(); err != nil {
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
// registered in plan_test.go, holding the pinned driver's planner to its seek.
const (
	// purgeChangeWalkSQL is a repository's changes in number order, each
	// table's rows counted at most ?2 deep: a count only asks whether one
	// change fits, so the walk reads a part's bound and one change more,
	// whatever the repository holds. Wide rows are COUNTED from their key's
	// index, never read, so no overflow page is touched: their weight is read
	// off the narrow rows of the change and its conversation.
	purgeChangeWalkSQL = `SELECT c.id, c.state, c.body_bytes,
		COALESCE((SELECT text_bytes FROM code_conversations WHERE change_id = c.id), 0), c.notices,
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_revisions WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_change_authors WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_change_moves WHERE change_id = c.id LIMIT ?2)),
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
	// The changes taken whole, driven from the JSON list of their ids, one
	// seek an element (planner.go).
	purgeChangeRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM json_each(?) AS j CROSS JOIN code_revisions AS r
		WHERE r.change_id = CAST(j.value AS TEXT))`
	purgeChangeAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM json_each(?) AS j CROSS JOIN code_change_authors AS a
		WHERE a.change_id = CAST(j.value AS TEXT))`
	purgeChangeMovesSQL = `DELETE FROM code_change_moves WHERE rowid IN (SELECT v.rowid
		FROM json_each(?) AS j CROSS JOIN code_change_moves AS v
		WHERE v.change_id = CAST(j.value AS TEXT))`
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
	// The cut change's narrow rows, each table in its key's order.
	purgeCutRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT rowid
		FROM code_revisions WHERE change_id = ? ORDER BY n LIMIT ?)`
	purgeCutAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT rowid
		FROM code_change_authors WHERE change_id = ? ORDER BY author LIMIT ?)`
	purgeCutMovesSQL = `DELETE FROM code_change_moves WHERE rowid IN (SELECT rowid
		FROM code_change_moves WHERE change_id = ? ORDER BY position, kind LIMIT ?)`
	purgeCutReviewersSQL = `DELETE FROM code_reviewers WHERE rowid IN (SELECT rowid
		FROM code_reviewers WHERE change_id = ? ORDER BY reviewer LIMIT ?)`
	purgeCutParticipantsSQL = `DELETE FROM code_participants WHERE rowid IN (SELECT rowid
		FROM code_participants WHERE change_id = ? ORDER BY handle LIMIT ?)`
	purgeCutFindingsSQL = `DELETE FROM code_findings WHERE rowid IN (SELECT rowid
		FROM code_findings WHERE change_id = ? ORDER BY raiser, state, revision, id LIMIT ?)`
	// The cut change's wide rows: each table's widths, and the delete of the
	// first n in the same order ([purgeCutWide]).
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
	// uncountNoticesSQL takes the notices a cut charged off its change's count.
	uncountNoticesSQL = `UPDATE code_changes SET notices = notices - ? WHERE id = ?`
	// The final purge's, unbounded because the decide forms it only once at
	// most PurgePartRows rows weighing within PurgePartBytes are left: the
	// dependents first, reached through their changes, then the changes.
	purgeAllRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_revisions AS r
		WHERE c.repo_id = ? AND r.change_id = c.id)`
	purgeAllAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_authors AS a
		WHERE c.repo_id = ? AND a.change_id = c.id)`
	purgeAllMovesSQL = `DELETE FROM code_change_moves WHERE rowid IN (SELECT v.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_moves AS v
		WHERE c.repo_id = ? AND v.change_id = c.id)`
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
