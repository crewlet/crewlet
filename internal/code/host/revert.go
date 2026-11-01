package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MERGED CHANGE REVERTED, ON THE GIT NODE (code.WriteRevertChange, M5-D25):
// the change read in the build's copy's snapshot, its merge placed against
// the target it was merged onto, ONE three-way merge undoing it onto the
// target's tip, and ONE commit on a NEW branch, landed as one push whose
// record opens "Revert <repo>!<n>" into the same target (landing.Change) —
// so the revert is a change of its own, gated, reviewed and merged as any
// is, and never a move of the target.
//
// WHAT THE MERGE BROUGHT IN is its merge_oid against the target before it:
//
//   - a MERGE COMMIT's first parent — the target it was built onto — which is
//     git's `revert -m 1`;
//   - a SQUASH's one parent, the same target, which is a plain revert;
//   - a FAST-FORWARD, whose merge_oid is the change's own head, its RANGE:
//     from the base its head revision's facts recorded (the merge-base of its
//     target and its head, D-REVISIONS), since its head's first parent is the
//     change's own previous commit, not the target.
//
// One rule undoes all three — merge-tree with the merge_oid as the base,
// the tip as ours and the target before it as theirs — so a range reverts as
// ONE commit, the change's whole effect, as a reviewer reads a revert.
// Nothing reads which method merged it: a merge commit has two parents, and a
// fast-forward's merge_oid is its head, which the rows say alone. A change a
// person merged by a push is placed the same way.

// revertChange is a revert_change's build: the change and its merge placed
// ([revertBase]), the merge undone onto the target's tip, the change the push
// opens formed, and its commit — or a refusal, nothing stored: not_merged for
// a change with no merge to place, conflict naming every path that does not
// apply, nothing_to_change for a merge already undone.
func (b *building) revertChange(ctx context.Context) (string, code.EngineWriteResult, bool, error) {
	wr := b.wr
	s, w := wr.s, wr.w
	repo := wr.synced.Repo
	// THE COPY'S OWN SNAPSHOT OR LATER: its caller read the change merged
	// at a floor the copy was synced past, so the merge_oid is in it.
	ans, err := s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChange, Asker: w.Principal, Repo: repo.Name,
		Change: w.Change, Fresh: statelog.FloorAt(statelog.ReadSession, wr.synced.Position)})
	named := code.ChangeRef(repo.Name, w.Change)
	switch {
	case errors.Is(err, code.ErrChangeNotFound):
		return "", wr.changeRefused(code.WriteNotMerged, fmt.Sprintf("repository %s holds no change %s",
			repo.Name, named)), true, nil
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case ans.Change == nil:
		return "", code.EngineWriteResult{}, true, fmt.Errorf("host: the read of %s answered no change", named)
	}
	c := ans.Change
	if c.State != code.ChangeStateMerged || !gitref.ValidOID(c.MergeOID) {
		return "", wr.changeRefused(code.WriteNotMerged, fmt.Sprintf("%s is %s, and only a merged change is "+
			"reverted", named, c.State)), true, nil
	}
	tip, err := wr.headOf(ctx, c.TargetRef)
	switch {
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case tip == "":
		return "", wr.changeRefused(code.WriteNotMerged, fmt.Sprintf("%s merged into %s, which no longer exists",
			named, c.TargetRef)), true, nil
	}
	head, merged := &commitHeader{}, &commitHeader{}
	if err := b.readCommits(ctx, []string{tip, c.MergeOID}, []*commitHeader{head, merged}); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	before, how, refusal := revertBase(named, c, ans.Revisions, merged.parents)
	if refusal != "" {
		return "", wr.changeRefused(code.WriteNotMerged, refusal), true, nil
	}
	m, err := b.merge(ctx, c.MergeOID, tip, before)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if !m.clean {
		res, err := b.conflicted(ctx, m, fmt.Sprintf("the revert of %s does not apply cleanly on %s: %d paths "+
			"conflict; resolve them in a coding box (run_sandbox), or write the result yourself", named,
			shortID(tip), len(m.conflicts)+m.omitted))
		return "", res, true, err
	}
	if m.tree == head.tree {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("what %s brought into %s is undone "+
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
	return commit, code.EngineWriteResult{}, false, err
}

// How a merge brought a change in, as a revert's message names it.
const (
	revertedMergeCommit = "merge commit"
	revertedSquash      = "squash"
	revertedRange       = "fast-forward"
)

// revertBase is the target a change's merge was made onto — what reverting
// it restores ([revertChange]'s doc) — and how it was merged: the merge
// commit's first parent, a squash's one parent, or a fast-forward's recorded
// base; or the sentence of a merge the rows cannot place yet. parents are the
// merge_oid's, and revisions the change's first page of them, newest first.
func revertBase(named string, c *code.ChangeDetail, revisions []code.RevisionRow, parents []string) (string,
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
func revertOption(named, title, mergeOID, target string) *code.ChangeOption {
	return &code.ChangeOption{Target: target, Title: code.AsTitle("Revert " + named),
		Body: fmt.Sprintf("Reverts %s (%s), merged as %s.", named, title, mergeOID)}
}
