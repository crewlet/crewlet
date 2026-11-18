package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE REBASE: code_rebase_change replays a change's commits its target does
// not hold onto the target's tip and moves the source branch to the last, as
// ONE engine write (gitlet.WriteRebaseChange) the Gitlet node lands as one
// forced push: each commit keeps its author and author date (U43 a), and the
// first that conflicts stops it with nothing stored. The push is the caller's,
// held to the grant's ref rule and the source branch's protections as a
// force-push is, and, as every branch write, to the requester's grant in a
// turn somebody woke (gitlet.EngineWrite.Ceiling, [CodeDeps.writeCaller]).

// CodeRebaseChangeTool is the tool's name: its authority row's action.
const CodeRebaseChangeTool = string(authz.ActionCodeRebaseChange)

// rebaseVerb is the verb a rebase's operation is named by ([opIDFor]).
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
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number, false)
	if refusal != nil {
		return *refusal, nil
	}
	// CONFLICT, as an ended change is on every code tool ([mergeRefusalClass]):
	// the change's state refuses it until reopened, or for good once merged,
	// and the request itself is not wrong.
	if !cv.change.State.Live() {
		return refusedBy(tools.RefusalConflict, fmt.Errorf("gitlet: %s is %s", cv.g.ref(), cv.change.State),
			fmt.Sprintf("%s: %s is %s, and only a live change is rebased.", tool, cv.g.ref(),
				cv.change.State)), nil
	}
	repo := cv.g.repo
	op := opIDFor(cv.g.c.actor, tool, rebaseVerb, repo.ID, args)
	w := written(ctx, turn, cv.g.c, gitlet.EngineWrite{Kind: gitlet.WriteRebaseChange, RepoID: repo.ID,
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.SourceRef, ExpectedHead: head,
		Change: cv.change.Number})
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	// The source, often a seat's branch, is held before the write is sent, as
	// a read of it is ([CodeDeps.branch]).
	if refusal := t.deps.branch(ctx, turn, tool, cv.g.c, repo, true, w.Ref); refusal != nil {
		return *refusal, nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	commit(turn, res.Outcome, types.CommittedCode)
	out := codeBranchAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Branch: strings.TrimPrefix(w.Ref, branchRoot), OpID: cv.g.c.actor.Operation}
	switch {
	case res.Refusal == gitlet.WriteConflict:
		return conflictedWrite(ctx, turn, cv.g.c, tool, out, res)
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s rebased nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position, out.Previous = res.Outcome, res.Commit, positionText(res.Position),
			head
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(cv.g.c.actor, tool, fmt.Sprintf("the rebase of %s landed", cv.g.ref()), op, false,
			unknownNext(false, sameCall(cv.g.c.actor, tool), fmt.Sprintf("Read %s with %s", cv.g.ref(),
				CodeGetChangeTool), "the same rebase would be pushed twice")), nil
	case res.Command.Result == gitlet.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Branch, res.Command, cv.g.c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered a rebase with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the rebase: %s.", tool,
			faultSaid)), nil
	}
	return newForeignRead(turn, cv.g.c.asker.Seat).page(ctx, tool, out, "")
}
