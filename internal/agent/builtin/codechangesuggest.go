package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// code_apply_suggestions puts findings' suggestions in place of their lines on
// the change's source branch as ONE engine write the Gitlet node lands as one
// push (gitlet.WriteApplySuggestions, ADR-0092), each placed by the rule its
// finding is shown by (gitlet.Reanchor), all or none; then it answers each
// finding resolved with the commit, as a second write: a step of the same
// operation decided at or past the push's record. The commit's author is the
// caller (W-M1), its committer the engine, so the push makes the caller an
// AUTHOR of the change (rule D4), whose approval then counts for nothing. A
// finding applied must still stand, carry a suggestion and be on the NEW side
// (a suggestion replaces lines a head holds). "resolved" is an author's
// answer, leaving a blocker for its raiser to judge (S40).

// CodeApplySuggestionsTool is the tool's name: its authority row's action.
const CodeApplySuggestionsTool = string(authz.ActionCodeApplySuggestions)

type codeApplySuggestions struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeApplySuggestions)(nil)

func (t *codeApplySuggestions) Name() string { return CodeApplySuggestionsTool }

func (t *codeApplySuggestions) Description() string {
	return "Apply findings' suggestions to a change's branch at the head you read (`expected_head`) as ONE commit " +
		"of yours — all of them, or none where a finding's lines changed or two overlap — then answer each finding " +
		"resolved with it. Applying makes you an author of the change."
}

func (t *codeApplySuggestions) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":          changeRepoParam(),
			"change":        changeParam(),
			"findings":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"expected_head": map[string]any{"type": "string", "description": "The change's head, as you read it."},
			"message":       map[string]any{"type": "string"},
		},
		"required": []string{"change", "findings", "expected_head"},
	})
}

func (t *codeApplySuggestions) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeAppliedAnswer is an apply's answer: the change, its push's commit and
// the head it moved, and the findings resolved with the commit or why not.
type codeAppliedAnswer struct {
	Repo                string           `json:"repo"`
	RenamedTo           string           `json:"renamed_to,omitempty"`
	RedirectUntil       time.Time        `json:"redirect_until,omitzero"`
	Change              string           `json:"change"`
	Branch              string           `json:"branch"`
	Outcome             statelog.Outcome `json:"outcome"`
	Commit              string           `json:"commit,omitempty"`
	Previous            string           `json:"previous,omitempty"`
	Position            string           `json:"position,omitempty"`
	FindingsResolved    []string         `json:"findings_resolved,omitempty"`
	FindingsNotResolved string           `json:"findings_not_resolved,omitempty"`
	OpID                string           `json:"op_id,omitempty"`
}

func (t *codeApplySuggestions) CallForTurn(ctx context.Context, turn *turnctx.Turn,
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
	ids := argStrings(args, "findings")
	head := strings.TrimSpace(argString(args, "expected_head"))
	switch {
	case len(ids) == 0:
		return failed(fmt.Sprintf("%s needs `findings`: the ids of the findings whose suggestions it applies.",
			tool)), nil
	case len(ids) > gitlet.MaxSuggestionOps:
		return failed(fmt.Sprintf("%s: %d findings, past the %d one apply makes: apply them in two.", tool,
			len(ids), gitlet.MaxSuggestionOps)), nil
	case !gitref.ValidOID(head):
		return failed(fmt.Sprintf("%s needs `expected_head`: the change's head as %s answered it, a whole commit "+
			"id — the commit sits on the head you read, and on no later one.", tool, CodeGetChangeTool)), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number, false)
	if refusal != nil {
		return *refusal, nil
	}
	if !cv.change.State.Live() {
		return refusedBy(tools.RefusalConflict, fmt.Errorf("gitlet: %s is %s", cv.g.ref(), cv.change.State),
			fmt.Sprintf("%s: %s is %s: its branch is no longer what it proposes, and takes no suggestion.", tool,
				cv.g.ref(), cv.change.State)), nil
	}
	suggestions, refusal := cv.suggestions(ctx, ids)
	if refusal != nil {
		return *refusal, nil
	}
	repo := cv.g.repo
	op := cv.op()
	message := strings.TrimSpace(argString(args, "message"))
	if message == "" {
		message = fmt.Sprintf("Apply the review's suggestions to %s", cv.g.ref())
	}
	w := written(ctx, turn, cv.g.c, gitlet.EngineWrite{Kind: gitlet.WriteApplySuggestions, RepoID: repo.ID,
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.SourceRef, ExpectedHead: head,
		Message: message, Suggestions: suggestions})
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	// THE SOURCE BRANCH'S HOLD before the write ([CodeDeps.branch]): the source
	// is often a seat's branch, and an apply that moves nothing (every
	// suggestion outdated) appends no record its deletion is ordered against.
	// It names the head, so it is held as a read is.
	if refusal := t.deps.branch(ctx, turn, tool, cv.g.c, repo, true, w.Ref); refusal != nil {
		return *refusal, nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	commit(turn, res.Outcome, types.CommittedCode)
	out := codeAppliedAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Change: cv.g.ref(), Branch: strings.TrimPrefix(w.Ref, branchRoot), OpID: cv.g.c.actor.Operation}
	switch {
	case res.Refusal == gitlet.WriteSuggestionOutdated:
		return refusedBy(tools.RefusalStaleVersion, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s applied nothing: %s (%s). Read the findings again with %s at the change's head, and "+
				"apply those still in place.", tool, res.Detail, res.Refusal, CodeGetChangeTool)), nil
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s applied nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position = res.Outcome, res.Commit, positionText(res.Position)
		if res.Command.Old != gitlet.ZeroOID {
			out.Previous = res.Command.Old
		}
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(cv.g.c.actor, tool, fmt.Sprintf("the suggestions' commit to %s landed", cv.g.ref()), op,
			false, unknownNext(false, sameCall(cv.g.c.actor, tool), fmt.Sprintf("Read %s with %s", cv.g.ref(),
				CodeGetChangeTool), "the same commit would be made twice")), nil
	case res.Command.Result == gitlet.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Branch, res.Command, cv.g.c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered an apply with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the apply: %s.", tool, faultSaid)), nil
	}
	cv.resolveApplied(ctx, ids, res, &out)
	return newForeignRead(turn, cv.g.c.asker.Seat).page(ctx, tool, out, "")
}

// suggestions are the suggestions of the findings ids names, in that order,
// from ONE read narrowed to them (gitlet.ReadQuery.FindingIDs), paging on only
// where one filled: never a walk of the change's findings, which are paged by
// time-ordered id, putting the newest review's (the ones an apply names) on
// the last page of a history that only grows. Or the refusal of a finding no
// apply takes: not the change's, no longer standing, suggesting nothing, or
// on its revision's old side.
func (cv conversing) suggestions(ctx context.Context, ids []string) ([]gitlet.SuggestionOp, *tools.Result) {
	g := cv.g
	// EACH ID ASKED ONCE, AND ONLY ONE A FINDING COULD HAVE: any other is
	// refused below as not the change's, not by the read it would make invalid.
	want := map[string]*gitlet.FindingRow{}
	var asked []string
	for _, id := range ids {
		if _, seen := want[id]; !seen && gitlet.ValidFindingID(id) {
			asked = append(asked, id)
		}
		want[id] = nil
	}
	for cursor := ""; len(asked) > 0; {
		page, refusal := g.read(ctx, gitlet.ReadQuery{Part: gitlet.PartFindings, FindingIDs: asked, Cursor: cursor},
			cv.at)
		if refusal != nil {
			return nil, refusal
		}
		for i := range page.Findings {
			f := &page.Findings[i]
			if held, asked := want[f.ID]; asked && held == nil {
				want[f.ID] = f
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	out := make([]gitlet.SuggestionOp, 0, len(ids))
	for _, id := range ids {
		f := want[id]
		switch {
		case f == nil:
			r := refused(tools.RefusalNotFound, fmt.Sprintf("%s: %s holds no finding %s: its findings are read "+
				"with %s.", g.tool, g.ref(), clip(id), CodeGetChangeTool))
			return nil, &r
		case !f.State.Stands():
			r := refused(tools.RefusalNotFound, fmt.Sprintf("%s: finding %s of %s is %s, which no apply takes: it "+
				"no longer stands.", g.tool, id, g.ref(), f.State))
			return nil, &r
		case f.Suggestion == nil:
			r := failed(fmt.Sprintf("%s: finding %s of %s suggests nothing to apply.", g.tool, id, g.ref()))
			return nil, &r
		case f.Side != gitlet.SideNew:
			r := failed(fmt.Sprintf("%s: finding %s of %s is on the %s side, its revision's base: a suggestion "+
				"replaces lines a head holds.", g.tool, id, g.ref(), f.Side))
			return nil, &r
		}
		out = append(out, gitlet.SuggestionOp{FindingID: f.ID, Path: f.Path, BlobOID: f.BlobOID,
			LineStart: f.LineStart, LineEnd: f.LineEnd, Text: *f.Suggestion})
	}
	return out, nil
}

// resolveApplied makes an apply's second write, every applied finding answered
// resolved with the commit, once it landed (applied or pending), as a STEP of
// the apply's operation decided at or past the push's record, where the
// caller is an author. Its failure is said beside the commit, never as the
// apply's: the commit WAS made, and the same call again is the same push,
// which makes it once.
func (cv conversing) resolveApplied(ctx context.Context, ids []string, res gitlet.EngineWriteResult,
	out *codeAppliedAnswer) {

	g := cv.g
	answer := gitlet.FindingResolve{RepoID: g.repo.ID, ChangeID: cv.change.ID}
	for _, id := range ids {
		answer.Findings = append(answer.Findings, gitlet.FindingResolution{FindingID: id,
			Resolution: gitlet.FindingResolved, Commit: res.Commit})
	}
	w := gitlet.ConversationWrite{OpID: statelog.StepOpID(cv.op(), stepTwo), Seen: res.Position,
		FindingResolve: &answer}
	if _, refusal := cv.decided(ctx, w); refusal != nil {
		out.FindingsNotResolved = fmt.Sprintf("The suggestions WERE applied, as %s, but answering their findings "+
			"did not finish: %s", res.Commit, refusal.Output)
		return
	}
	out.FindingsResolved = ids
}
