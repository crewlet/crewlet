package builtin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE REVERT: code_revert_change undoes a MERGED change by a change of its own
// (M5-D25) — ONE ENGINE WRITE (code.WriteRevertChange) the repository's git
// node builds: what the change's merge brought into its target undone onto
// the target's tip, as one commit on a new branch — the seat's own
// (refs/heads/seats/<handle>/revert-<n>) or, for a person, revert-<n> —
// whose push's record opens "Revert <repo>!<n>" into the same target. So a
// revert is never a move of the target: it is a change, gated, reviewed and
// merged as every change is. A branch of that name already there answers
// `exists`: a revert of the change is already under way.
//
// Asked to (`merge`), the revert's change is merged as a SECOND write of the
// same operation's step ([stepTwo], statelog.StepOpID), from the commit the
// revert made — merged, queued at its place, or not merged and why, said
// beside the opened change ("opened; site!8 not merged: …"), never as a
// refusal of the revert, which landed: the revert is the call's gesture and
// its merge the consequence, as a merge's close of its work item is.

// CodeRevertChangeTool is the revert tool's name: its authority row's action.
const CodeRevertChangeTool = string(authz.ActionCodeRevertChange)

// revertVerb is the verb a revert's operation is named by (m3-w1's op rule;
// M5's verbs).
const revertVerb = "code-revert"

type codeRevertChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeRevertChange)(nil)

func (t *codeRevertChange) Name() string { return CodeRevertChangeTool }

func (t *codeRevertChange) Description() string {
	return "Revert a merged change by a change of its own: what its merge brought into its target undone on a " +
		"new branch from the target's tip, opened as \"Revert <repo>!<n>\" into the same target — never a move " +
		"of the target. `merge` also merges that change once it is open, as its gate allows."
}

func (t *codeRevertChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":   changeRepoParam(),
			"change": changeParam(),
			"merge": map[string]any{"type": "boolean", "description": "Merge the revert's change once it is " +
				"open. Default false."},
		},
		"required": []string{"change"},
	})
}

func (t *codeRevertChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeRevertChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, number, refusal := changeArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	repo := cv.g.repo
	op := opIDFor(cv.g.c.actor, tool, revertVerb, repo.ID, args)
	w := code.EngineWrite{Kind: code.WriteRevertChange, RepoID: repo.ID, Principal: writer(ctx, turn, cv.g.c),
		OpID: op, Fresh: revertFresh(t.deps.writeFresh(turn, cv.g.c, repo), cv.at),
		Ref: revertBranch(cv.g.c.asker, cv.change.Number), Change: cv.change.Number, Ceiling: cv.g.c.write}
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	// THE BRANCH IT CREATES IS HELD before the write is sent, with no read
	// ([CodeDeps.branch]): a seat's revert branch is its own namespace's.
	if refusal := t.deps.branch(ctx, turn, tool, cv.g.c, repo, w.Ref, false); refusal != nil {
		return *refusal, nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	branch := strings.TrimPrefix(w.Ref, branchRoot)
	out := codeRevertAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Reverts: cv.g.ref(), Branch: branch, OpID: cv.g.c.actor.Operation}
	switch {
	case res.Refusal == code.WriteConflict:
		return conflictedWrite(ctx, turn, cv.g.c, tool, codeBranchAnswer{Repo: repo.Name, RenamedTo: repo.Redirect,
			RedirectUntil: repo.RedirectUntil, Branch: branch, OpID: out.OpID}, res)
	case res.Refusal == code.WriteNotMerged:
		return refusedBy(tools.RefusalConflict, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s reverted nothing: %s (%s).", tool, res.Detail, res.Refusal)), nil
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s reverted nothing: %s (%s). The same call is refused again.", tool, res.Detail,
				res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position = res.Outcome, res.Commit, positionText(res.Position)
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(cv.g.c.actor, tool, fmt.Sprintf("the revert of %s landed", cv.g.ref()), op, false,
			unknownNext(false, sameCall(cv.g.c.actor, tool), fmt.Sprintf("List %s's changes with %s",
				repo.Name, CodeListChangesTool), "the same revert would be pushed twice")), nil
	case res.Command.Result == code.ResultNG && res.Command.Reason == code.RefusalStale:
		// A NEW BRANCH THAT IS THERE: this change's revert, begun already.
		return refusedBy(tools.RefusalExists, writeRefusalError(string(res.Command.Reason), res.Command.Detail),
			fmt.Sprintf("%s: branch %s exists already, so a revert of %s is under way; find its change with "+
				"%s, or delete the branch with %s to revert again.", tool, branch, cv.g.ref(), CodeListChangesTool,
				CodeDeleteBranchTool)), nil
	case res.Command.Result == code.ResultNG:
		return pushRefused(ctx, tool, repo.Name, branch, res.Command, cv.g.c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered a revert with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the revert: %s.", tool,
			faultSaid)), nil
	}
	opened := t.opened(&out, repo.Name, res.Change)
	if argBool(args, "merge") {
		t.mergeStep(ctx, turn, cv, op, res, opened, &out)
	}
	return newForeignRead(turn, cv.g.c.asker.Seat).page(ctx, tool, out, "")
}

// opened says on out which change the revert's push opened, or why none was,
// and answers its number — 0 where its record has not said it yet.
func (t *codeRevertChange) opened(out *codeRevertAnswer, repo string, ch *code.ChangeAnswer) int64 {
	switch {
	case ch == nil:
		out.ChangeNotOpened = fmt.Sprintf("the push's record said nothing of its change yet; find it with %s",
			CodeListChangesTool)
		return 0
	case ch.Refusal != "":
		out.ChangeNotOpened = fmt.Sprintf("%s (%s)", ch.Detail, ch.Refusal)
		return 0
	case ch.Number < 1:
		out.ChangeNotOpened = fmt.Sprintf("its number is told once the push's record applies; find it with %s",
			CodeListChangesTool)
		return 0
	}
	out.Opened = code.ChangeRef(repo, ch.Number)
	return ch.Number
}

// mergeStep is the revert's step two: the change it opened merged — at the
// commit the revert made, into the reverted change's target — as the caller,
// under the operation's [stepTwo], its source deleted as a merge's default
// deletes a seat's ([seatSourceDeleted]); what became of it said on out, a
// merge that did not happen as a residue beside the opened change, never as
// a refusal.
func (t *codeRevertChange) mergeStep(ctx context.Context, turn *turnctx.Turn, cv conversing, op string,
	res code.EngineWriteResult, number int64, out *codeRevertAnswer) {

	notMerged := func(why string) {
		label := out.Opened
		if label == "" {
			label = "its change"
		}
		out.Residue = fmt.Sprintf("reverted; %s not merged: %s", label, why)
	}
	switch {
	case number == 0:
		notMerged(out.ChangeNotOpened)
		return
	case res.Outcome != statelog.OutcomeApplied:
		notMerged(fmt.Sprintf("the revert's push is still pending; merge it with %s once %s shows it",
			CodeMergeChangeTool, CodeGetChangeTool))
		return
	}
	g := cv.g
	source := revertBranch(g.c.asker, cv.change.Number)
	deletes, refusal := seatSourceDeleted(ctx, g, source, cv.change.TargetRef)
	if refusal != nil {
		notMerged(fmt.Sprintf("its target's merge rules could not be read (%s)", refusal.Output))
		return
	}
	w := code.EngineWrite{Kind: code.WriteMergeChange, RepoID: g.repo.ID, Principal: writer(ctx, turn, g.c),
		OpID: statelog.StepOpID(op, stepTwo), Fresh: revertFresh(t.deps.writeFresh(turn, g.c, g.repo),
			res.Position), Ref: cv.change.TargetRef, ExpectedHead: res.Commit, Change: number,
		Method: code.MergeMethodMerge, DeleteSource: deletes, Ceiling: g.c.write}
	if err := w.Validate(); err != nil {
		notMerged(err.Error())
		return
	}
	got, err := t.deps.Writer.GitWrite(ctx, w)
	m := got.Merge
	switch {
	case err != nil:
		notMerged(fmt.Sprintf("its merge failed (%v)", err))
	case m != nil && m.Refusal == code.MergeRefusalGateUnmet:
		notMerged(fmt.Sprintf("it does not yet meet %s's merge gate, %d precondition(s) unmet; a "+
			"ready_to_merge wake follows", strings.TrimPrefix(cv.change.TargetRef, branchRoot), len(m.Unmet)))
	case m != nil && m.Refusal != "":
		notMerged(fmt.Sprintf("%s (%s)", m.Detail, m.Refusal))
	case got.Refusal != "":
		notMerged(fmt.Sprintf("%s (%s)", got.Detail, got.Refusal))
	case got.Outcome == statelog.OutcomeUnknown:
		notMerged(fmt.Sprintf("nobody can say whether its merge landed; read it with %s", CodeGetChangeTool))
	case got.Outcome != statelog.OutcomeApplied && got.Outcome != statelog.OutcomePending:
		notMerged("the code host answered its merge with no outcome")
	case m != nil && m.Queued:
		out.Merge, out.Place = mergeQueued, m.Place
	default:
		out.Merge, out.MergeCommit = mergeMerged, got.Commit
	}
}

// revertBranch is the branch a principal's revert of change number is made
// on: a seat's own namespace's, or revert-<n> for a person.
func revertBranch(asker code.Principal, number int64) string {
	leaf := "revert-" + strconv.FormatInt(number, 10)
	if asker.Kind == code.PrincipalAgent {
		return gitref.SeatsRoot + asker.Seat + "/" + leaf
	}
	return branchRoot + leaf
}

// revertFresh is a revert's freshness: the caller's write floor raised to at,
// the position its change was read merged at (or the revert's push landed
// at, for its merge), so the git node's copy holds the commit it builds from
// — a person's write is linearizable already.
func revertFresh(f statelog.Freshness, at statelog.Position) statelog.Freshness {
	if f.Level == statelog.ReadLinearizable {
		return f
	}
	return statelog.FloorAt(statelog.ReadSession, furthestOnLog(f.Floor(topics.CodeLogStream), at))
}

// codeRevertAnswer is what code_revert_change says: the change it reverts,
// the branch and commit the revert made, the change its push opened — or why
// none was — and, asked to merge it, merged, queued at its place, or the
// residue saying why not.
type codeRevertAnswer struct {
	Repo            string           `json:"repo"`
	RenamedTo       string           `json:"renamed_to,omitempty"`
	RedirectUntil   time.Time        `json:"redirect_until,omitzero"`
	Reverts         string           `json:"reverts"`
	Branch          string           `json:"branch"`
	Outcome         statelog.Outcome `json:"outcome,omitempty"`
	Commit          string           `json:"commit,omitempty"`
	Position        string           `json:"position,omitempty"`
	Opened          string           `json:"opened,omitempty"`
	ChangeNotOpened string           `json:"change_not_opened,omitempty"`
	Merge           string           `json:"merge,omitempty"`
	MergeCommit     string           `json:"merge_commit,omitempty"`
	Place           int              `json:"place,omitempty"`
	Residue         string           `json:"residue,omitempty"`
	OpID            string           `json:"op_id,omitempty"`
}
