package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE REBASE: code_rebase_change replays a change's commits its target does
// not hold onto the target's tip and moves the change's source branch to the
// last of them, as ONE ENGINE WRITE the repository's git node builds and
// lands as one forced push (code.WriteRebaseChange) — each commit keeping its
// author and its author date (U43 a), the first that conflicts stopping it
// with nothing stored. The push is the caller's, held to the grant's ref rule
// and the protections of the source branch as a force-push of it is, and its
// apply records the change's new revision.

// CodeRebaseChangeTool is the tool's name: its authority row's action.
const CodeRebaseChangeTool = string(authz.ActionCodeRebaseChange)

// rebaseVerb is the verb a rebase's operation is named by (m3-w1's op rule).
const rebaseVerb = "code-rebase"

type codeRebaseChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeRebaseChange)(nil)

func (t *codeRebaseChange) Name() string { return CodeRebaseChangeTool }

func (t *codeRebaseChange) Description() string {
	return "Rebase a change onto its target's tip: its commits replayed in order, each keeping its author, and " +
		"its branch moved to them from the head you read (`expected_head`) by one forced push. The first commit " +
		"that conflicts stops it with nothing changed; a change holding a merge commit is refused."
}

func (t *codeRebaseChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":          changeRepoParam(),
			"change":        changeParam(),
			"expected_head": map[string]any{"type": "string", "description": "The change's head, as you read it."},
		},
		"required": []string{"change", "expected_head"},
	})
}

func (t *codeRebaseChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeRebaseChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
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
	head := strings.TrimSpace(argString(args, "expected_head"))
	if !gitref.ValidOID(head) {
		return failed(fmt.Sprintf("%s needs `expected_head`: the change's head as %s answered it, a whole commit "+
			"id — the rebase replays the commits you read, and no later ones.", tool, CodeGetChangeTool)), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	if !cv.change.State.Live() {
		return refusedBy(tools.RefusalConflict, fmt.Errorf("code: %s is %s", cv.g.ref(), cv.change.State),
			fmt.Sprintf("%s: %s is %s, and only a live change is rebased.", tool, cv.g.ref(),
				cv.change.State)), nil
	}
	repo := cv.g.repo
	op := opIDFor(cv.g.c.actor, tool, rebaseVerb, repo.ID, args)
	w := code.EngineWrite{Kind: code.WriteRebaseChange, RepoID: repo.ID, Principal: writer(ctx, turn, cv.g.c),
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.SourceRef, ExpectedHead: head,
		Change: cv.change.Number}
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	// THE SOURCE BRANCH'S HOLD, before the write is sent ([CodeDeps.branch]):
	// the rebase moves the change's source, often a seat's branch, at the head
	// it names, so it is held as a read of it is.
	if refusal := t.deps.branch(ctx, turn, tool, cv.g.c, repo, w.Ref, true); refusal != nil {
		return *refusal, nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	out := codeBranchAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Branch: strings.TrimPrefix(w.Ref, branchRoot), OpID: cv.g.c.actor.Operation}
	switch {
	case res.Refusal == code.WriteConflict:
		return conflictedWrite(ctx, turn, cv.g.c, tool, out, res)
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s rebased nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position, out.Previous = res.Outcome, res.Commit, res.Position, head
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(cv.g.c.actor, tool, fmt.Sprintf("the rebase of %s landed", cv.g.ref()), op, false,
			unknownNext(false, sameCall(cv.g.c.actor, tool), fmt.Sprintf("Read %s with %s", cv.g.ref(),
				CodeGetChangeTool), "the same rebase would be pushed twice")), nil
	case res.Command.Result == code.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Branch, res.Command, cv.g.c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered a rebase with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the rebase: %s.", tool,
			faultSaid)), nil
	}
	return newForeignRead(turn, cv.g.c.asker.Seat).page(ctx, tool, out, "")
}
