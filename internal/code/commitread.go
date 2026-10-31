package code

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A COMMIT'S CHANGES — the `commit` read, what a commit's page names beside
// the commit itself: the repository in detail, as a read of one repository
// answers it, and in the same snapshot every change the commit belongs to —
// each whose merge it is (code_changes_merge_idx) and each revision it heads
// (code_revisions_head_idx), "revision 2 of web!42" — held to the asker's
// grant on the repository as that read is. The commit itself, and the push
// that introduced it, are the repository's content and its git node's index,
// never these rows: a question asks them by object id beside this read.
//
// A commit belongs to a handful of changes in practice — two branches at one
// head, a change reopened from the same commit — and to any number in the
// worst case, every one a branch somebody opened a change from, so each list
// is read to [MaxCommitChanges] and says when it holds more (More): a commit's
// page names the changes it belongs to, never a listing of them, which
// `changes` pages.

// MaxCommitChanges is the most changes a `commit` read names a commit merged
// and the most revisions it names the commit heading, each list read one row
// further to say it holds more: the page size every listing of a change's
// conversation defaults to (defaultReviewLimit), past which a commit's page
// is no longer naming what the commit belongs to but listing it.
const MaxCommitChanges = 50

// CommitChanges is a `commit` read's answer beside the repository's detail
// (ReadAnswer.Commit): the changes one commit of the repository belongs to.
//
// A READ'S ANSWER, NOT A RECORD'S, so it takes no versionedFields row; the
// `commit` kind is new to a build that predates it, which refuses it whole
// ([ReadKinds]).
type CommitChanges struct {
	// Commit is the commit, its whole id.
	Commit string `json:"commit"`
	// Merged is every change whose merge is the commit — the merge record's
	// commit, or the head a push into its target merged it at — by its
	// number; at most MaxCommitChanges.
	Merged []ChangeName `json:"merged,omitempty"`
	// Revisions is every revision the commit is the head of, at most
	// MaxCommitChanges, in the order code_revisions_head_idx keeps them.
	Revisions []CommitRevision `json:"revisions,omitempty"`
	// More is set where either list holds more than it names.
	More bool `json:"more,omitempty"`
}

// CommitRevision is one revision a commit heads: the change and its number.
type CommitRevision struct {
	Change ChangeName `json:"change"`
	N      int64      `json:"n"`
}

// commitKeys are the two indexes a `commit` read seeks — GUARDS as planner.go
// says, each held by plan_test.go to its seek.
const (
	commitMergedKey = "code_changes_merge_idx"
	commitHeadsKey  = "code_revisions_head_idx"
)

// A commit's changes: those whose merge is the commit, one range of
// commitMergedKey on the repository and the commit, its partial predicate
// repeated; and the revisions it heads, one range of commitHeadsKey on the
// commit — which every repository's revisions share, so each is kept to this
// repository by its change, read by its key. Each read one row past
// MaxCommitChanges. Registered in plan_test.go.
const (
	commitMergedSQL = `SELECT number, title, state FROM code_changes INDEXED BY ` + commitMergedKey + `
		WHERE repo_id = ? AND merge_oid = ? AND merge_oid != '' LIMIT ?`
	commitHeadsSQL = `SELECT r.n, c.number, c.title, c.state
		FROM code_revisions AS r INDEXED BY ` + commitHeadsKey + ` CROSS JOIN code_changes AS c
		WHERE r.head_oid = ? AND c.id = r.change_id AND c.repo_id = ? LIMIT ?`
)

// commit serves a `commit` read: the repository in detail, judged as a read
// of one repository is ([Reader.repo]), and the changes q.Commit belongs to,
// in the same transaction.
func (r *Reader) commit(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {

	if err := r.repo(ctx, tx, q, chart, cp, out); err != nil {
		return err
	}
	repo := out.Repo.RepoRef
	c := &CommitChanges{Commit: q.Commit}
	name := func(number int64, title, state string) ChangeName {
		return ChangeName{Repo: repo.Name, Ref: ChangeRef(repo.Name, number), Title: title,
			State: ChangeState(state)}
	}
	rows, err := tx.QueryContext(ctx, commitMergedSQL, repo.ID, q.Commit, MaxCommitChanges+1)
	if err != nil {
		return fmt.Errorf("code: read the changes %s merged in %s: %w", q.Commit, repo.Name, err)
	}
	err = eachRow(rows, func(s scanner) error {
		var (
			number       int64
			title, state string
		)
		if err := s.Scan(&number, &title, &state); err != nil {
			return err
		}
		c.Merged = append(c.Merged, name(number, title, state))
		return nil
	})
	if err != nil {
		return fmt.Errorf("code: read the changes %s merged in %s: %w", q.Commit, repo.Name, err)
	}
	if rows, err = tx.QueryContext(ctx, commitHeadsSQL, q.Commit, repo.ID, MaxCommitChanges+1); err != nil {
		return fmt.Errorf("code: read the revisions %s heads in %s: %w", q.Commit, repo.Name, err)
	}
	err = eachRow(rows, func(s scanner) error {
		var (
			n, number    int64
			title, state string
		)
		if err := s.Scan(&n, &number, &title, &state); err != nil {
			return err
		}
		c.Revisions = append(c.Revisions, CommitRevision{Change: name(number, title, state), N: n})
		return nil
	})
	if err != nil {
		return fmt.Errorf("code: read the revisions %s heads in %s: %w", q.Commit, repo.Name, err)
	}
	if len(c.Merged) > MaxCommitChanges || len(c.Revisions) > MaxCommitChanges {
		c.More = true
		c.Merged = c.Merged[:min(len(c.Merged), MaxCommitChanges)]
		c.Revisions = c.Revisions[:min(len(c.Revisions), MaxCommitChanges)]
	}
	out.Commit = c
	return nil
}

// validateCommit is [ReadQuery.validate] for a `commit` read: the commit it
// names, a WHOLE id — the rows record whole ids, and an abbreviation is the
// repository's content to expand, which no row read can — and no page: a
// commit's changes are named whole, to MaxCommitChanges.
func (q ReadQuery) validateCommit() error {
	switch {
	case !gitref.ValidOID(q.Commit) || q.Commit == ZeroOID:
		return fmt.Errorf("%w: commit: %q is not a whole commit id", ErrInvalid, q.Commit)
	case q.Limit != 0 || q.Cursor != "":
		return fmt.Errorf("%w: limit: a commit's changes are named whole, never paged", ErrInvalid)
	}
	return nil
}

// eachRow calls each for every row of rows, and closes them.
func eachRow(rows *sql.Rows, each func(scanner) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
