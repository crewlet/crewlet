package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE HOME'S DUE CHANGE WORK, READ FROM THE ROWS: a `changes_due` read
// ([ReadChangesDue]) is what a repository's home git node polls for, and is
// nudged into reading, to learn which changes it owes work: a LIVE change whose
// head revision holds no facts against the branch it merges into
// (code_revision_facts by its key — a revision with no row, or one read against
// another branch, the change retargeted since), on an ACTIVE repository, its
// target a live branch. Those are the changes whose facts the home reads and
// records ([Revise]) — or, finding a head its target holds, lands
// ([ChangeLanded]) — and every one it records drops out of the listing by its
// own row, so the rows are the work list and nothing else is.
//
// A CHANGE ON AN ARCHIVED OR A DELETED REPOSITORY IS NOBODY'S WORK: every write
// of it would be refused (repo_archived, repo_deleted), and listed it would be
// read again on every pass for ever. A change whose target branch is gone is
// nobody's either, until a push makes the branch again or the change is
// retargeted: there is no tip to read its facts against.
//
// TWO SHAPES, one per caller: with no repository it pages the repositories
// holding such a change, by id — what a home's backstop asks its placement
// about, a repository at a time being no question a pass can afford — walking
// code_changes_open_idx, which holds the live changes alone, in its own order
// and grouped on its leading column, so a page is read in index order and
// never sorted; with one ([ChangesDueQuery]) it pages that repository's due
// changes by source branch, the same index's range, each with its target's tip
// from code_refs' key, and the repository's path owners beside them, read in
// the same snapshot as the changes they are matched for. Every change a page
// names is checked against its facts' key, one seek a change.
//
// READ STALE BY DEFAULT, as every duty listing is: a change listed a moment
// behind the head costs a revise the decide refuses superseded, and one a
// moment early is listed again at the next pass — the decide holds every guard
// to its own snapshot. A home nudged by a record reads at that record's
// position instead, so the change it was nudged about is in the rows it reads
// ([checkFreshness]: `session` beside a position on the code log).

// validateChangesDue is [ReadQuery.validate] for a `changes_due` read: the
// engine's own, so it names NO asker and no seat it reads for, for the
// `compaction_due` read's reason; its one repository, where it names one, by
// id in its canonical spelling; and nothing else a listing names — no
// repository's name, filter, range, floor or limit, since a page is
// MaxSnapshotItems rows.
func (q ReadQuery) validateChangesDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a changes_due read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a changes_due read is the engine's own, which no seat makes for another")
	case q.ChangesDue != nil && !canonicalUUID(q.ChangesDue.RepoID):
		return refused("changes_due.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", q.ChangesDue.RepoID))
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
		{"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil},
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
	if q.ChangesDue == nil {
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	}
	return fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.ChangesDue.RepoID)}}, true)
}

// changesDue serves one page of a `changes_due` read — see this file's head.
func (r *Reader) changesDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.ChangesDue == nil {
		return r.dueRepos(ctx, tx, q, out)
	}
	return r.dueChanges(ctx, tx, q, q.ChangesDue.RepoID, out)
}

// dueRepos serves a page of the repositories holding a due change, by id,
// after the cursor — the last id a page named.
func (r *Reader) dueRepos(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && !canonicalUUID(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, dueReposSQL, q.Cursor, r.pageRows)
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

// dueChanges serves a page of repoID's due changes, by source branch, after
// the cursor — the last source branch a page named — beside the repository's
// path owners: none at all, and no owners, for a repository that is not
// active or not there.
func (r *Reader) dueChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, repoID string, out *ReadAnswer) error {
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
	rows, err := tx.QueryContext(ctx, dueChangesSQL, repoID, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("code: list repository %s's due changes: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes - ownersBytes}
	for rows.Next() {
		var (
			c  ChangeDue
			at int64
		)
		if err := rows.Scan(&c.ID, &c.Number, &c.Revision, &c.Head, &c.SourceRef, &c.TargetRef, &c.Tip,
			&at); err != nil {
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

// The due listing's reads, each registered in plan_test.go: the repositories
// holding a due change, after a cursor, walking code_changes_open_idx in its
// order and grouped on its leading column, its predicate spelled as the index
// declares it ([liveChanges]), each change checked against its facts' key, its
// target's live row by code_refs' key and its repository's state by its key;
// one repository's state by its key; and one repository's due changes after a
// source branch, the same index's range, each joined to its target's row by
// code_refs' key and checked against its facts' key.
const (
	dueReposSQL = `SELECT c.repo_id FROM code_changes AS c INDEXED BY code_changes_open_idx
		WHERE c.repo_id > ?1 AND ` + liveChanges + `
			AND NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
				WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref)
			AND EXISTS (SELECT 1 FROM code_refs AS t INDEXED BY ` + refsKey + `
				WHERE t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != '')
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = c.repo_id AND r.status = 'active')
		GROUP BY c.repo_id ORDER BY c.repo_id LIMIT ?2`
	dueRepoSQL    = `SELECT status FROM code_repos WHERE id = ?`
	dueChangesSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND c.source_ref > ?2 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
				WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref)
		ORDER BY c.source_ref LIMIT ?3`
)
