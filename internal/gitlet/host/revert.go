package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A merged change is reverted on the Gitlet node (gitlet.WriteRevertChange,
// M5-D25) as ONE commit on a NEW branch, landed as one push whose record opens
// "Revert <repo>!<n>" into the same target (landing.Change): a change of its
// own, gated, reviewed and merged as any is, never a move of the target.
//
// One rule undoes a merge commit, a squash and a fast-forward alike:
// merge-tree with the merge_oid as base, the target's tip as ours and the
// target before the merge as theirs (revertBase places it). So a
// fast-forward's whole range reverts as one commit; a plain revert of its head
// would leave its earlier commits in, its first parent being the change's own.

// revertChange is a revert_change's build: the change and its merge placed
// ([revertBase]), the merge undone onto the target's tip, the change the push
// opens formed, and its commit — or a refusal, nothing stored: not_merged for
// a change with no merge to place, conflict naming every path that does not
// apply, nothing_to_change for a merge already undone.
func (b *building) revertChange(ctx context.Context) (string, gitlet.EngineWriteResult, bool, error) {
	wr := b.wr
	s, w := wr.s, wr.w
	repo := wr.synced.Repo
	// The copy holds merge_oid: the change was read merged at a floor the copy
	// is synced past. Limit 1: only the head revision's facts are read
	// (revertBase), and a whole page is up to 200 revisions' touched paths.
	ans, err := s.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChange, Asker: w.Principal, Repo: repo.Name,
		Change: w.Change, Limit: 1, Fresh: statelog.FloorAt(statelog.ReadSession, wr.synced.Position)})
	named := gitlet.ChangeRef(repo.Name, w.Change)
	switch {
	case errors.Is(err, gitlet.ErrChangeNotFound):
		return "", wr.changeRefused(gitlet.WriteNotMerged, fmt.Sprintf("repository %s holds no change %s",
			repo.Name, named)), true, nil
	case err != nil:
		return "", gitlet.EngineWriteResult{}, true, err
	case ans.Change == nil:
		return "", gitlet.EngineWriteResult{}, true, fmt.Errorf("host: the read of %s answered no change", named)
	}
	c := ans.Change
	if c.State != gitlet.ChangeStateMerged || !gitref.ValidOID(c.MergeOID) {
		return "", wr.changeRefused(gitlet.WriteNotMerged, fmt.Sprintf("%s is %s, and only a merged change is "+
			"reverted", named, c.State)), true, nil
	}
	tip, err := wr.headOf(ctx, c.TargetRef)
	switch {
	case err != nil:
		return "", gitlet.EngineWriteResult{}, true, err
	case tip == "":
		return "", wr.changeRefused(gitlet.WriteNotMerged, fmt.Sprintf("%s merged into %s, which no longer exists",
			named, c.TargetRef)), true, nil
	}
	head, merged := &commitHeader{}, &commitHeader{}
	if err = b.readCommits(ctx, []string{tip, c.MergeOID}, []*commitHeader{head, merged}); err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	before, how, refusal := revertBase(named, c, ans.Revisions, merged.parents)
	if refusal != "" {
		return "", wr.changeRefused(gitlet.WriteNotMerged, refusal), true, nil
	}
	m, err := b.merge(ctx, c.MergeOID, tip, before)
	if err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	if !m.clean {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := b.conflicted(ctx, m, fmt.Sprintf("the revert of %s does not apply cleanly on %s: %d paths "+
			"conflict; resolve them in a checkout of the repository ("+toolname.CodeCheckout+"), or write the "+
			"result yourself", named,
			shortID(tip), len(m.conflicts)+m.omitted))
		return "", res, true, err
	}
	if m.tree == head.tree {
		return "", wr.changeRefused(gitlet.WriteNothingChanged, fmt.Sprintf("what %s brought into %s is undone "+
			"on %s already: no commit is made", named, c.TargetRef, shortID(tip))), true, nil
	}
	message := w.Message
	if message == "" {
		message = gitref.WrittenCommitText(revertChangeMessage(named, c.Title, c.MergeOID, before, how))
	} else if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	wr.opens = revertOption(named, c.Title, c.MergeOID, c.TargetRef)
	commit, err := b.commit(ctx, m.tree, tip, head.committed, message)
	return commit, gitlet.EngineWriteResult{}, false, err
}

// How a merge brought a change in, as a revert's message names it.
const (
	revertedMergeCommit = "merge commit"
	revertedSquash      = "squash"
	revertedRange       = "fast-forward"
)

// revertBase is the target a change's merge was made onto, which reverting it
// restores, and how it was merged: a merge commit's or a squash's first
// parent, or a fast-forward's base as its head revision's facts recorded it
// (D-REVISIONS); else the sentence refusing a merge the rows cannot place yet.
// parents are the merge_oid's; revisions the change's first page, newest first.
// The method is read from the merge_oid's shape, never recorded, so a change
// a person merged by a push is placed the same way.
func revertBase(named string, c *gitlet.ChangeDetail, revisions []gitlet.RevisionRow, parents []string) (string,
	string, string) {

	if c.MergeOID != c.Head {
		switch len(parents) {
		case 0:
			return emptyTreeID, revertedSquash, ""
		case 1:
			return parents[0], revertedSquash, ""
		}
		return parents[0], revertedMergeCommit, ""
	}
	for _, r := range revisions {
		if r.N != c.Revision {
			continue
		}
		if r.Facts == nil || !gitref.ValidOID(r.Facts.BaseOID) {
			break
		}
		return r.Facts.BaseOID, revertedRange, ""
	}
	return "", "", fmt.Sprintf("%s fast-forwarded %s to its head, and the base its head revision is measured "+
		"from is not recorded yet, so its range cannot be placed; ask again shortly", named, c.TargetRef)
}

// revertChangeMessage is a revert_change's commit message: git's own form
// (`Revert "<title>"`), saying which change it reverts, the commit its target
// was merged at, how, and the target it restores.
func revertChangeMessage(named, title, mergeOID, before, how string) string {
	return fmt.Sprintf("Revert \"%s\"\n\nThis reverts %s, merged as %s (%s), back to %s.\n", title, named, mergeOID,
		how, before)
}

// revertOption is the change a revert_change's push opens: "Revert
// <repo>!<n>" into the target the reverted change merged into, its
// description naming what it reverts.
func revertOption(named, title, mergeOID, target string) *gitlet.ChangeOption {
	return &gitlet.ChangeOption{Target: target, Title: gitlet.AsTitle("Revert " + named),
		Body: fmt.Sprintf("Reverts %s (%s), merged as %s.", named, title, mergeOID)}
}
