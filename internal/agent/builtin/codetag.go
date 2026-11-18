package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE TAG WRITES: a tag created (annotated where a message is given) or deleted
// at the value read, each an engine write landed as one push like the branch
// writes, so the grant, tag protections and push history treat it as a pushed tag.

// The tag write tools' names: each is its authority row's action.
const (
	CodeCreateTagTool = string(authz.ActionCodeCreateTag)
	CodeDeleteTagTool = string(authz.ActionCodeDeleteTag)
)

// tagRoot is where a tag's ref begins.
const tagRoot = "refs/tags/"

// codeTagWrite is create_tag or delete_tag, by its kind.
type codeTagWrite struct {
	deps CodeDeps
	kind gitlet.EngineWriteKind
}

var _ tools.SeatCallable = (*codeTagWrite)(nil)

func (t *codeTagWrite) Name() string {
	if t.kind == gitlet.WriteCreateTag {
		return CodeCreateTagTool
	}
	return CodeDeleteTagTool
}

func (t *codeTagWrite) Description() string {
	if t.kind == gitlet.WriteCreateTag {
		return "Create a tag at a commit: annotated when given a `message`, lightweight otherwise. A tag never " +
			"moves."
	}
	return "Delete a tag at the value you read; the commits it named stay in the history."
}

func (t *codeTagWrite) Parameters() map[string]any {
	props := map[string]any{
		"repo": codeRepoArg(),
		"tag":  map[string]any{"type": "string", "description": "Its name under refs/tags/."},
	}
	required := []string{"repo", "tag"}
	if t.kind == gitlet.WriteCreateTag {
		props["at"] = codeRevArg("The commit it names")
		props["message"] = map[string]any{"type": "string", "description": "Makes it an annotated tag."}
		required = append(required, "at")
	} else {
		props["expected_target"] = map[string]any{"type": "string",
			"description": "Its value, as " + CodeResolveTool + " answered it."}
		required = append(required, "expected_target")
	}
	return t.deps.ops.param(map[string]any{"type": "object", "properties": props, "required": required})
}

func (t *codeTagWrite) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// CallForTurn is [codeBranchWrite.CallForTurn]'s pipeline over a tag: every
// argument first, then the repository, the commit `at` names and the write.
func (t *codeTagWrite) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	w, at, refusal := t.args(args)
	if refusal != nil {
		return *refusal, nil
	}
	actor, bound := t.deps.bindOperation(c.actor, tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	repo, refusal := t.deps.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	if at != "" {
		resolved, refusal := t.deps.resolveRevs(ctx, turn, tool, c, repo, at)
		if refusal != nil {
			return *refusal, nil
		}
		w.Commit = resolved[0].oid
	}
	if refusal := t.deps.branch(ctx, turn, tool, c, repo, w.ExpectedHead != "", w.Ref); refusal != nil {
		return *refusal, nil
	}
	w = written(ctx, turn, c, w)
	w.RepoID, w.Fresh = repo.ID, t.deps.writeFresh(turn, c, repo)
	w.OpID = opIDFor(actor, tool, codeWriteVerb, repo.ID, args)
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, actor, repo.Name, err), nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, c, repo, w.OpID, err), nil
	}
	return t.answer(ctx, turn, c, repo, w, res)
}

// args is the write a call's arguments ask for, and the revision `at` names,
// or the refusal of an argument no write could take.
func (t *codeTagWrite) args(args map[string]any) (gitlet.EngineWrite, string, *tools.Result) {
	tool := t.Name()
	tag := strings.TrimPrefix(strings.TrimSpace(argString(args, "tag")), tagRoot)
	if !gitref.ValidTag(tag) {
		r := failed(fmt.Sprintf("%s needs `tag`: a tag's name as refs/tags/ holds it (`v1.2.0`); %q is not "+
			"one git takes.", tool, clip(tag)))
		return gitlet.EngineWrite{}, "", &r
	}
	w := gitlet.EngineWrite{Kind: t.kind, Ref: tagRoot + tag,
		ExpectedHead: strings.TrimSpace(argString(args, "expected_target"))}
	if t.kind != gitlet.WriteCreateTag {
		return w, "", nil
	}
	w.Message = argString(args, "message")
	at, refusal := revArg(tool, "at", args)
	if refusal != nil {
		return gitlet.EngineWrite{}, "", refusal
	}
	return w, at, nil
}

// codeTagAnswer is a tag write's answer.
type codeTagAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Tag           string           `json:"tag"`
	Outcome       statelog.Outcome `json:"outcome,omitempty"`
	// Target is the tag's value, an annotated tag's object; Commit the
	// commit it names; Previous a deleted tag's value.
	Target   string `json:"target,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Previous string `json:"previous,omitempty"`
	Position string `json:"position,omitempty"`
	OpID     string `json:"op_id,omitempty"`
}

// answer is what a tag write came to, as [codeBranchWrite.answer] says a
// branch write's: the tag, a refusal in its class, or an outcome unknown.
func (t *codeTagWrite) answer(ctx context.Context, turn *turnctx.Turn, c codeCaller, repo gitlet.RepoRef,
	w gitlet.EngineWrite, res gitlet.EngineWriteResult) (tools.Result, error) {

	tool := t.Name()
	out := codeTagAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Tag: strings.TrimPrefix(w.Ref, tagRoot), OpID: c.actor.Operation}
	switch {
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s changed nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Target, out.Commit, out.Position = res.Outcome, res.Commit, w.Commit,
			positionText(res.Position)
		if w.Kind == gitlet.WriteDeleteTag {
			out.Previous = res.Command.Old
		}
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(c.actor, tool, fmt.Sprintf("the write to tag %s on %s landed", out.Tag, repo.Name),
			w.OpID, false, unknownNext(false, sameCall(c.actor, tool),
				fmt.Sprintf("Read the tag with %s", CodeResolveTool), "the same change would be made twice")), nil
	case res.Command.Result == gitlet.ResultNoop:
		out.Outcome, out.Target, out.Commit = statelog.OutcomeApplied, res.Command.New, w.Commit
	case res.Command.Result == gitlet.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Tag, res.Command, c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered a write with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the write: %s.", tool, faultSaid)), nil
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}
