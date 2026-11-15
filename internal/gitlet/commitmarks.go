package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/gitref"
)

// A HISTORY PAGE'S MARKS, the `commit_marks` read: for each commit the caller
// names, its checks' rollup per branch and the changes it landed with, read
// in one snapshot from the rows alone and bounded to what a row draws.

// The bounds of a history page's marks, each a list read one row past it to
// say more.
const (
	// MaxCommitMarks is the most commits one read marks: a history page's,
	// MaxLogLimit, the most one log page lists.
	MaxCommitMarks = MaxLogLimit
	// MaxMarkTargets is the most branches one commit's checks are rolled up
	// for: a commit is checked for the target of each change it heads and,
	// landed, as its branch's tip; four holds a release branch, its main and
	// two changes, and a row draws one.
	MaxMarkTargets = 4
	// MaxMarkChanges is the most changes each of a mark's two lists names: a
	// row draws one pill; four holds two branches at one head and a change
	// reopened at it. code_commit names them all, to MaxCommitChanges.
	MaxMarkChanges = 4
)

// CommitMark is what a history page draws beside one commit.
//
// A READ'S ANSWER, NOT A RECORD'S, so it takes no versionedFields row: only
// the `commit_marks` kind answers it, which a build that predates it refuses
// whole ([ReadKinds]).
type CommitMark struct {
	// Commit is the commit, its whole id, as the query named it.
	Commit string `json:"commit"`
	// Checks is, for each branch the commit was checked for, in branch
	// order, where its checks stand; ChecksMore that more branches may follow
	// (past MaxMarkTargets, or past the walk's bound). None where no attempt
	// is kept: never checked, or its attempts ended past CheckHistory.
	Checks     []TargetChecks `json:"checks,omitempty"`
	ChecksMore bool           `json:"checks_more,omitempty"`
	// Merged is each change whose merge commit it is, and Heads each
	// revision it heads, at most MaxMarkChanges of each; More that either
	// holds more. Neither carries an override reason or a gate, which
	// code_commit answers.
	Merged []ChangeLanding  `json:"merged,omitempty"`
	Heads  []CommitRevision `json:"heads,omitempty"`
	More   bool             `json:"more,omitempty"`
}

// TargetChecks is where a commit's checks stand for one branch.
type TargetChecks struct {
	// Target is the branch, in full: refs/heads/main.
	Target string `json:"target"`
	CheckRollup
}

// validateCommitMarks is [ReadQuery.validate] for a `commit_marks` read: the
// commits it names, each a WHOLE id (the rows record whole ids, and an
// abbreviation is the repository's content to expand), once, at most
// MaxCommitMarks of them, and no page: a history page's marks are its rows.
func (q ReadQuery) validateCommitMarks() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case len(q.CommitMarks) == 0:
		return refused("commit_marks", "a commit_marks read names the commits whose marks it reads")
	case len(q.CommitMarks) > MaxCommitMarks:
		return refused("commit_marks", fmt.Sprintf("%d commits named, past the %d one history page lists",
			len(q.CommitMarks), MaxCommitMarks))
	case q.Limit != 0 || q.Cursor != "":
		return refused("limit", "a history page's marks are read whole, never paged")
	}
	for i, oid := range q.CommitMarks {
		switch {
		case !gitref.ValidOID(oid) || oid == ZeroOID:
			return refused("commit_marks", fmt.Sprintf("%q is not a whole commit id", oid))
		case slices.Contains(q.CommitMarks[:i], oid):
			return refused("commit_marks", fmt.Sprintf("%s is named twice", oid))
		}
	}
	return nil
}

// commitMarks serves a `commit_marks` read ([ReadCommitMarks]): the
// repository q names, judged for the asker as a read of its changes is
// ([readableIn], no detail answered: a hundred commits' marks need none of
// it), then each commit's mark in the query's order, three walks a commit in
// one transaction.
func (r *Reader) commitMarks(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	marks := make([]CommitMark, 0, len(q.CommitMarks))
	for _, commit := range q.CommitMarks {
		mark := CommitMark{Commit: commit}
		if mark.Checks, mark.ChecksMore, err = commitChecks(ctx, tx, repo.ref.ID, commit); err != nil {
			return err
		}
		merged, err := commitMerged(ctx, tx, repo.ref, commit, MaxMarkChanges+1)
		if err != nil {
			return err
		}
		heads, err := commitHeads(ctx, tx, repo.ref, commit, MaxMarkChanges+1)
		if err != nil {
			return err
		}
		mark.More = len(merged) > MaxMarkChanges || len(heads) > MaxMarkChanges
		for _, l := range merged[:min(len(merged), MaxMarkChanges)] {
			mark.Merged = append(mark.Merged, l.marked())
		}
		for _, h := range heads[:min(len(heads), MaxMarkChanges)] {
			mark.Heads = append(mark.Heads, CommitRevision{Change: h.change.marked(), N: h.n})
		}
		marks = append(marks, mark)
	}
	out.CommitMarks = marks
	return nil
}

// commitChecks is where commit's checks stand for each branch, in branch
// order: one walk of latestChecksOfCommitSQL to a bound, a branch it cut, or
// one past a rollup's rows, rolled up from those read with More set.
func commitChecks(ctx context.Context, tx *sql.Tx, repoID, commit string) ([]TargetChecks, bool, error) {
	const bound = MaxMarkTargets*maxRollupChecks + 1
	rows, err := tx.QueryContext(ctx, latestChecksOfCommitSQL, repoID, commit, bound)
	if err != nil {
		return nil, false, fmt.Errorf("gitlet: read the checks of %s: %w", commit, err)
	}
	type branch struct {
		target string
		rows   []rollupRow
	}
	var (
		branches []branch
		read     int
		more     bool
	)
	err = eachRow(rows, func(s scanner) error {
		var target string
		row, err := scanRollupRow(s, &target)
		if err != nil {
			return err
		}
		read++
		switch {
		case more:
		case len(branches) > 0 && branches[len(branches)-1].target == target:
			branches[len(branches)-1].rows = append(branches[len(branches)-1].rows, row)
		case len(branches) == MaxMarkTargets:
			more = true
		default:
			branches = append(branches, branch{target: target, rows: []rollupRow{row}})
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("gitlet: read the checks of %s: %w", commit, err)
	}
	var out []TargetChecks
	for i, b := range branches {
		// The walk's bound cuts only the last branch read.
		cut := len(b.rows) > maxRollupChecks || read == bound && !more && i == len(branches)-1
		rollup := rollUp(b.rows[:min(len(b.rows), maxRollupChecks)], cut)
		out = append(out, TargetChecks{Target: b.target, CheckRollup: *rollup})
	}
	// A walk its bound cut cannot tell whether another branch follows.
	return out, more || read == bound, nil
}
