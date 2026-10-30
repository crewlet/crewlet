package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE HOME'S DUE CHANGE WORK, READ FROM THE ROWS: a `changes_due` read
// ([ReadChangesDue]) is what a repository's home git node polls for, and is
// nudged into reading, to learn which changes it owes work. A LIVE change, on
// an ACTIVE repository, its target a live branch, owes its home:
//
//   - its head revision's FACTS, where it holds none against the branch it
//     merges into (code_revision_facts by its key — a revision with no row, or
//     one read against another branch, the change retargeted since), which
//     the home reads and records ([Revise]) — or, finding a head its target
//     holds, lands ([ChangeLanded]);
//   - its head revision's MERGE, where it holds no mergeability of that
//     revision against that branch (code_mergeability by its key), which the
//     home merges and records ([Mergeability]) — or lands, as above;
//   - a REFRESH of its readiness ([Mergeability.Refresh]), where it holds a
//     mergeability of its head revision against that branch whose readiness
//     was judged at versions its own row (code_changes.version) or its
//     conversation's readiness inputs (code_conversations.readiness_version)
//     have moved past since — a review, an ask, an answer to findings, its
//     facts, an edit of its row — which the home records over the merge the
//     row holds, running none;
//   - and a RECHECK of that merge — the change's mergeability is of its head
//     revision against its branch, and its target has moved past the tip it
//     merged into since, or the merge was undecided — once the mergeability
//     was recorded at or before [ChangesDueQuery.RecheckBefore]: its home's
//     clock less its refresh, so a target that moves on every push asks its
//     changes' merges again at the home's pace, never at its own.
//
// Every record the home makes takes its change off the list by its own row,
// so the rows are the work list and nothing else is: a merge and a refresh
// each store the versions they judged the readiness at.
//
// A CHANGE ON AN ARCHIVED OR A DELETED REPOSITORY IS NOBODY'S WORK: every write
// of it would be refused (repo_archived, repo_deleted), and listed it would be
// read again on every pass for ever. A change whose target branch is gone is
// nobody's either, until a push makes the branch again or the change is
// retargeted: there is no tip to read its facts against or merge it into.
//
// TWO SHAPES, one per caller: with no repository it pages the repositories
// holding such a change, by id — what a home's backstop asks its placement
// about, a repository at a time being no question a pass can afford — walking
// code_changes_open_idx, which holds the live changes alone, in its own order
// and grouped on its leading column, so a page is read in index order and
// never sorted; with one ([ChangesDueQuery]) it pages that repository's
// changes owing their facts, their merge or a refresh by source branch, the
// same index's range, each with its target's tip from code_refs' key, and the
// repository's path owners beside them, read in the same snapshot as the
// changes they are matched for — and on its FIRST PAGE the changes owing a
// recheck, at most as many as the home asks ([ChangesDueQuery.Rechecks]), THE
// OLDEST MERGE FIRST. Every change a page names is checked against its facts'
// key, its mergeability's and its conversation's, one seek of each a change.
//
// OLDEST FIRST, because a page cut at the home's budget in any other order
// starves the rest: in source-branch order a repository with more changes due
// a recheck than its home's budget covers in one refresh would recheck the
// same first ones at every refresh and the last ones never, while the oldest
// merge first walks them all in turn. The one sort a listing here makes, of
// one repository's live changes behind a moved target — at most one a live
// branch of it (code_changes_open_idx), so MaxRefsPerRepo bounds it — since no
// index orders a repository's changes by when their merge was recorded
// without one written by the records of two subjects.
//
// READ STALE BY DEFAULT, as every duty listing is: a change listed a moment
// behind the head costs a record the decide refuses superseded, and one a
// moment early is listed again at the next pass — the decide holds every guard
// to its own snapshot. A home nudged by a record reads at that record's
// position instead, so the change it was nudged about is in the rows it reads
// ([checkFreshness]: `session` beside a position on the code log).

// validateChangesDue is [ReadQuery.validate] for a `changes_due` read: the
// engine's own, so it names NO asker and no seat it reads for, for the
// `compaction_due` read's reason; its own query, REQUIRED, with the instant a
// recheck is due by, its one repository, where it names one, by id in its
// canonical spelling, and the rechecks it asks only beside one, 0 to
// MaxSnapshotItems; and nothing else a listing names — no repository's name,
// filter, range, floor or limit, since a page is MaxSnapshotItems rows.
func (q ReadQuery) validateChangesDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch d := q.ChangesDue; {
	case q.Asker != (Principal{}):
		return refused("asker", "a changes_due read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a changes_due read is the engine's own, which no seat makes for another")
	case d == nil:
		return refused("changes_due", "a changes_due read names the instant its home's recheck of a merge is "+
			"due by, and, for one repository's due changes, the repository")
	case d.RecheckBefore.IsZero():
		return refused("changes_due.recheck_before", "a changes_due read names the instant a merge recorded at "+
			"or before is due again: its home's clock less its refresh")
	case d.RepoID != "" && !canonicalUUID(d.RepoID):
		return refused("changes_due.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", d.RepoID))
	case d.Rechecks < 0 || d.Rechecks > MaxSnapshotItems:
		return refused("changes_due.rechecks", fmt.Sprintf("%d; a page names 0 to %d rechecks (MaxSnapshotItems)",
			d.Rechecks, MaxSnapshotItems))
	case d.RepoID == "" && d.Rechecks != 0:
		return refused("changes_due.rechecks", "the listing of repositories names the repositories alone, and a "+
			"repository's read its rechecks")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil}, {"min_packs", q.MinPacks != 0},
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"expunge_tips", q.ExpungeTips != nil},
		{"target", q.Target != ""}, {"queue_due", q.QueueDue != nil},
	} {
		if f.set {
			return refused(f.field, "a changes_due read lists the repositories a home owes change work in, or "+
				"one repository's due changes by its id, a page of MaxSnapshotItems at a time, and names "+
				"nothing else")
		}
	}
	return nil
}

// changesDueQuery is the framework read a `changes_due` read resolves to: a
// SET READ OF THE DOMAIN for the listing of repositories, as every duty listing
// is, and of the one repository for its due changes — whose records' scope
// names the repository's path, a change's conversation's among them — so a
// repository a deferred record holds back is read as its rows stand, and the
// decide that follows judges again from its own snapshot.
func (q ReadQuery) changesDueQuery(fresh statelog.Freshness) statelog.Query {
	if q.ChangesDue.RepoID == "" {
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	}
	return fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.ChangesDue.RepoID)}}, true)
}

// changesDue serves one page of a `changes_due` read — see this file's head.
func (r *Reader) changesDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.ChangesDue.RepoID == "" {
		return r.dueRepos(ctx, tx, q, out)
	}
	return r.dueChanges(ctx, tx, q, out)
}

// dueRepos serves a page of the repositories holding a due change, by id,
// after the cursor — the last id a page named.
func (r *Reader) dueRepos(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && !canonicalUUID(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, dueReposSQL, q.Cursor, r.pageRows,
		store.EncodeTime(q.ChangesDue.RecheckBefore))
	if err != nil {
		return fmt.Errorf("code: list the repositories change work is due in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("code: list the repositories change work is due in: %w", err)
		}
		out.DueRepos = append(out.DueRepos, id)
		if pg.add(stringBytes(id) + 1) {
			out.Next = id
			break
		}
	}
	return rows.Err()
}

// dueChanges serves a page of a repository's changes owing their facts or
// their merge, by source branch, after the cursor — the last source branch a
// page named — beside the repository's path owners, and on its first page the
// changes owing a recheck, oldest merge first: none at all, and no owners, for
// a repository that is not active or not there.
func (r *Reader) dueChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	repoID := q.ChangesDue.RepoID
	if q.Cursor != "" && !ValidRef(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	var status string
	switch err := tx.QueryRowContext(ctx, dueRepoSQL, repoID).Scan(&status); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("code: read repository %s's state: %w", repoID, err)
	case RepoStatus(status) != RepoStatusActive:
		return nil
	}
	owners, err := pathOwnersIn(ctx, tx, repoID)
	if err != nil {
		return err
	}
	// THE OWNERS RIDE EVERY PAGE, so every page's changes are matched against
	// the owners read beside them; their bytes come off the page's budget.
	ownersBytes := 0
	if len(owners) > 0 {
		encoded, err := json.Marshal(owners)
		if err != nil {
			return fmt.Errorf("code: measure repository %s's path owners: %w", repoID, err)
		}
		ownersBytes = len(encoded)
	}
	out.PathOwners = owners
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes - ownersBytes}
	if q.Cursor == "" && q.ChangesDue.Rechecks > 0 {
		if err := dueRechecks(ctx, tx, q, &pg, out); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, dueChangesSQL, repoID, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("code: list repository %s's due changes: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c  ChangeDue
			at int64
		)
		if err := rows.Scan(&c.ID, &c.Number, &c.Revision, &c.Head, &c.SourceRef, &c.TargetRef, &c.Tip,
			&at, &c.FactsDue, &c.MergeDue, &c.RefreshDue); err != nil {
			return fmt.Errorf("code: list repository %s's due changes: %w", repoID, err)
		}
		c.TipPosition = statelog.Unpack(topics.CodeLogStream, at)
		out.DueChanges = append(out.DueChanges, c)
		if pg.add(c.encodedBytes()) {
			out.Next = c.SourceRef
			break
		}
	}
	return rows.Err()
}

// dueRechecks is a repository's changes owing a recheck of their merge, the
// oldest merge first, at most as many as q asks — counted against the first
// page's budget, which they ride.
func dueRechecks(ctx context.Context, tx *sql.Tx, q ReadQuery, pg *page, out *ReadAnswer) error {
	repoID := q.ChangesDue.RepoID
	rows, err := tx.QueryContext(ctx, dueRechecksSQL, repoID, store.EncodeTime(q.ChangesDue.RecheckBefore),
		q.ChangesDue.Rechecks)
	if err != nil {
		return fmt.Errorf("code: list repository %s's due rechecks: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c           ChangeDue
			at, checked int64
		)
		if err := rows.Scan(&c.ID, &c.Number, &c.Revision, &c.Head, &c.SourceRef, &c.TargetRef, &c.Tip, &at,
			&checked); err != nil {
			return fmt.Errorf("code: list repository %s's due rechecks: %w", repoID, err)
		}
		c.TipPosition, c.Checked = statelog.Unpack(topics.CodeLogStream, at), store.DecodeTime(checked)
		out.Rechecks = append(out.Rechecks, c)
		// COUNTED, NEVER A REASON TO STOP: the home asks at most its pass's
		// budget, a few dozen rows, and a page always serves at least one
		// change after them, so the pages that follow start where it ends.
		pg.add(c.encodedBytes())
	}
	return rows.Err()
}

// The due listing's reads, each registered in plan_test.go. Each walks
// code_changes_open_idx, its predicate spelled as the index declares it
// ([liveChanges]), and reaches each change's target's live row by code_refs'
// key, its facts by theirs and its mergeability by its key:
//
//   - the repositories holding a due change, after a cursor, in the index's
//     order and grouped on its leading column, each repository's state by its
//     key — a change due its facts, its merge, a refresh, or a recheck of its
//     merge recorded at or before ?3;
//   - one repository's state by its key;
//   - one repository's changes owing their facts, their merge or a refresh
//     after a source branch, the index's range, each saying which it owes;
//   - one repository's changes owing a recheck — a mergeability of the head
//     revision against the branch, behind its target's tip or undecided,
//     recorded at or before ?2 — oldest first, at most ?3, the walk's one sort
//     (this file's head says why).
//
// THE MERGEABILITY IS READ IN A SUBQUERY, NEVER JOINED: its `state` beside the
// change's would make [liveChanges] — the index's predicate, spelled as the
// index declares it, unqualified — an ambiguous column, which the driver
// refuses to plan.
//
// A MERGE IS HELD, so not due, where the change's mergeability is of its head
// revision against its branch and either still of the target's tip and
// decided, or recorded after the instant a recheck is due by — [mergeHeld],
// what the listing of repositories asks of every live change it walks, where a
// repository's own read asks the two halves of it apart. A REFRESH IS DUE
// where that mergeability's readiness was judged at versions the change's row
// or its conversation have moved past — [refreshDue], its conversation's
// readiness version read by the conversations' key.
const (
	dueReposSQL = `SELECT c.repo_id FROM code_changes AS c INDEXED BY code_changes_open_idx
		WHERE c.repo_id > ?1 AND ` + liveChanges + `
			AND EXISTS (SELECT 1 FROM code_refs AS t INDEXED BY ` + refsKey + `
				WHERE t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
					AND (NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
							WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref)
						OR NOT EXISTS (SELECT 1 FROM code_mergeability AS m
							WHERE ` + mergeHeld + `)
						OR EXISTS (SELECT 1 FROM code_mergeability AS m WHERE ` + refreshDue + `)))
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = c.repo_id AND r.status = 'active')
		GROUP BY c.repo_id ORDER BY c.repo_id LIMIT ?2`
	dueRepoSQL    = `SELECT status FROM code_repos WHERE id = ?`
	dueChangesSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position,
			NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
				WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref),
			NOT EXISTS (SELECT 1 FROM code_mergeability AS m
				WHERE m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref),
			EXISTS (SELECT 1 FROM code_mergeability AS m WHERE ` + refreshDue + `)
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND c.source_ref > ?2 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND (NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
					WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref)
				OR NOT EXISTS (SELECT 1 FROM code_mergeability AS m
					WHERE m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref)
				OR EXISTS (SELECT 1 FROM code_mergeability AS m WHERE ` + refreshDue + `))
		ORDER BY c.source_ref LIMIT ?3`
	dueRechecksSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position, (SELECT m.at FROM code_mergeability AS m WHERE m.change_id = c.id) AS checked
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND EXISTS (SELECT 1 FROM code_mergeability AS m
				WHERE m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref
					AND (m.target_position < t.position OR m.state = 'unknown') AND m.at <= ?2)
		ORDER BY checked, c.id LIMIT ?3`
	// mergeHeld is a change's mergeability, m, as one its home owes no merge
	// of: of the change's head revision against its branch, and either of
	// the target's tip, t, and decided, or recorded after the instant a
	// recheck is due by, ?3.
	mergeHeld = `m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref
		AND ((m.target_position >= t.position AND m.state != 'unknown') OR m.at > ?3)`
	// refreshDue is a change's mergeability, m, as one its home owes a
	// refresh of: of the change's head revision against its branch, its
	// readiness judged at a version of the change's row, c, or of its
	// conversation's readiness inputs, that each has moved past since.
	refreshDue = `m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref
		AND (m.change_version < c.version OR m.readiness_version < (SELECT v.readiness_version
			FROM code_conversations AS v WHERE v.change_id = c.id))`
)
