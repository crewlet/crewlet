package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE SUGGESTIONS' APPLY: code_apply_suggestions puts findings' suggestions
// in place of the lines they are anchored to, on the change's source branch,
// as ONE ENGINE WRITE the repository's git node builds and lands as one push
// (code.WriteApplySuggestions, ADR-0062) — each suggestion placed in the
// head's file by the one rule its finding is shown by (code.Reanchor), all of
// them or none — and then answers each finding resolved with the commit it
// made, as a SECOND write, a step of the same operation decided at or past
// the push's record.
//
// # The actor's commit
//
// The commit is the caller's (W-M1): its author the seat or the person who
// applied, its committer the engine, as every engine write's is. So the push
// that lands it makes the caller an AUTHOR of the change (the push apply's
// rule D4), whose approval of it then counts for nothing — an applier is
// writing the change, not judging it.
//
// # What it applies
//
// The findings the call names, read in the call's one read of the change's
// conversation: each must still STAND (a dismissed or superseded finding is
// nobody's to apply), carry a suggestion, and be on the NEW side of its
// revision's diff — a suggestion replaces lines a head holds, and the old
// side's are the base's. Its resolution is "resolved", an author's answer,
// which leaves a blocker standing for its raiser to judge (S40): applying a
// suggestion answers a finding, it never clears one.

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

// codeAppliedAnswer is an apply's answer: the change, what became of its
// push — its commit and the head it moved — and of its findings' answer: the
// findings resolved with the commit, or why they were not.
type codeAppliedAnswer struct {
	Repo                string            `json:"repo"`
	RenamedTo           string            `json:"renamed_to,omitempty"`
	RedirectUntil       time.Time         `json:"redirect_until,omitzero"`
	Change              string            `json:"change"`
	Branch              string            `json:"branch"`
	Outcome             statelog.Outcome  `json:"outcome"`
	Commit              string            `json:"commit,omitempty"`
	Previous            string            `json:"previous,omitempty"`
	Position            statelog.Position `json:"position,omitzero"`
	FindingsResolved    []string          `json:"findings_resolved,omitempty"`
	FindingsNotResolved string            `json:"findings_not_resolved,omitempty"`
	OpID                string            `json:"op_id,omitempty"`
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
	case len(ids) > code.MaxSuggestionOps:
		return failed(fmt.Sprintf("%s: %d findings, past the %d one apply makes: apply them in two.", tool,
			len(ids), code.MaxSuggestionOps)), nil
	case !gitref.ValidOID(head):
		return failed(fmt.Sprintf("%s needs `expected_head`: the change's head as %s answered it, a whole commit "+
			"id — the commit sits on the head you read, and on no later one.", tool, CodeGetChangeTool)), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	if !cv.change.State.Live() {
		return refusedBy(tools.RefusalConflict, fmt.Errorf("code: %s is %s", cv.g.ref(), cv.change.State),
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
	w := code.EngineWrite{Kind: code.WriteApplySuggestions, RepoID: repo.ID, Principal: writer(ctx, turn, cv.g.c),
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.SourceRef, ExpectedHead: head,
		Message: message, Suggestions: suggestions}
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	// THE SOURCE BRANCH'S HOLD, before the write is sent ([CodeDeps.branch]):
	// the commit lands on the change's source, which is often a seat's branch,
	// and an apply that moves nothing — every suggestion outdated — appends no
	// record a deletion of it would be ordered against. It names the head
	// (expected_head is required), so it is held as a read is.
	if refusal := t.deps.branch(ctx, turn, tool, cv.g.c, repo, w.Ref, true); refusal != nil {
		return *refusal, nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	out := codeAppliedAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Change: cv.g.ref(), Branch: strings.TrimPrefix(w.Ref, branchRoot), OpID: cv.g.c.actor.Operation}
	switch {
	case res.Refusal == code.WriteSuggestionOutdated:
		return refusedBy(tools.RefusalStaleVersion, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s applied nothing: %s (%s). Read the findings again with %s at the change's head, and "+
				"apply those still in place.", tool, res.Detail, res.Refusal, CodeGetChangeTool)), nil
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s applied nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		out.Outcome, out.Commit, out.Position = res.Outcome, res.Commit, res.Position
		if res.Command.Old != code.ZeroOID {
			out.Previous = res.Command.Old
		}
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(cv.g.c.actor, tool, fmt.Sprintf("the suggestions' commit to %s landed", cv.g.ref()), op,
			false, unknownNext(false, sameCall(cv.g.c.actor, tool), fmt.Sprintf("Read %s with %s", cv.g.ref(),
				CodeGetChangeTool), "the same commit would be made twice")), nil
	case res.Command.Result == code.ResultNG:
		return pushRefused(ctx, tool, repo.Name, out.Branch, res.Command, cv.g.c.actor), nil
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered an apply with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the apply: %s.", tool, faultSaid)), nil
	}
	cv.resolveApplied(ctx, ids, res, &out)
	return newForeignRead(turn, cv.g.c.asker.Seat).page(ctx, tool, out, "")
}

// suggestions are the suggestions of the findings ids names, in that order,
// read from the change's conversation a page at a time from the call's read
// of the change — or the refusal of a finding no apply may take: one the
// change does not hold, one that no longer stands, one that suggests nothing,
// or one on the old side of its revision's diff.
func (cv conversing) suggestions(ctx context.Context, ids []string) ([]code.SuggestionOp, *tools.Result) {
	g := cv.g
	want := map[string]*code.FindingRow{}
	for _, id := range ids {
		want[id] = nil
	}
	left, cursor := len(ids), ""
	for left > 0 {
		page, refusal := g.read(ctx, code.ReadQuery{Part: code.PartFindings, Cursor: cursor,
			Limit: code.MaxFindingsPerPage}, cv.at)
		if refusal != nil {
			return nil, refusal
		}
		for i := range page.Findings {
			f := &page.Findings[i]
			if held, asked := want[f.ID]; asked && held == nil {
				want[f.ID] = f
				left--
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	out := make([]code.SuggestionOp, 0, len(ids))
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
		case f.Side != code.SideNew:
			r := failed(fmt.Sprintf("%s: finding %s of %s is on the %s side, its revision's base: a suggestion "+
				"replaces lines a head holds.", g.tool, id, g.ref(), f.Side))
			return nil, &r
		}
		out = append(out, code.SuggestionOp{FindingID: f.ID, Path: f.Path, BlobOID: f.BlobOID,
			LineStart: f.LineStart, LineEnd: f.LineEnd, Text: *f.Suggestion})
	}
	return out, nil
}

// resolveApplied makes an apply's second write — every finding it applied
// answered resolved, with the commit it made — once the commit is landed
// (applied or pending), as a STEP of the apply's operation decided at or past
// the push's own record, where the caller is an author of the change: the
// answer is an author's. Whatever became of it is said on the answer beside
// the commit, never as the apply's failure: the commit WAS made, and the call
// made again is the same push, which makes it once.
func (cv conversing) resolveApplied(ctx context.Context, ids []string, res code.EngineWriteResult,
	out *codeAppliedAnswer) {

	g := cv.g
	answer := code.FindingResolve{RepoID: g.repo.ID, ChangeID: cv.change.ID}
	for _, id := range ids {
		answer.Findings = append(answer.Findings, code.FindingResolution{FindingID: id,
			Resolution: code.FindingResolved, Commit: res.Commit})
	}
	w := code.ConversationWrite{OpID: statelog.StepOpID(cv.op(), stepTwo), Seen: res.Position,
		FindingResolve: &answer}
	if _, refusal := cv.decided(ctx, w); refusal != nil {
		out.FindingsNotResolved = fmt.Sprintf("The suggestions WERE applied, as %s, but answering their findings "+
			"did not finish: %s", res.Commit, refusal.Output)
		return
	}
	out.FindingsResolved = ids
}
