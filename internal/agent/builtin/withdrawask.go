package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WithdrawAskTool is the tool's wire name.
const WithdrawAskTool = toolname.WithdrawAsk

// MaxWithdrawReasonBytes bounds why a question is withdrawn: the page an
// answer to a parked run is held to ([MaxRunAnswerBytes]), since a run's
// withdrawal travels as its answer, and far inside a comment's own cap.
const MaxWithdrawReasonBytes = MaxRunAnswerBytes

// AskResolver resolves a comment as its author: a question its asker
// withdraws ([worklet.Writer.ResolveComment]).
type AskResolver interface {
	ResolveComment(ctx context.Context, opID, taskID, project, commentID string,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// WithdrawDeps are withdraw_ask's dependencies: the tracker's, for a question
// on a work item, and the run desk, for a question or a held call the seat's
// executor waits on. Either half alone offers the tool for that half.
type WithdrawDeps struct {
	// Work reads the item and posts the reason in the question's thread.
	Work WorkDeps
	// Resolver resolves the question as its author, one per actor as the
	// tracker's writers are.
	Resolver func(actor Actor) AskResolver
	// Desk reads a run and delivers the withdrawal as its answer.
	Desk RunDesk
}

// tracker reports whether the work-item half is wired.
func (d WithdrawDeps) tracker() bool {
	return d.Work.Reader != nil && d.Work.Writer != nil && d.Resolver != nil
}

// serves reports whether either half is wired.
func (d WithdrawDeps) serves() bool { return d.tracker() || d.Desk != nil }

// withdrawAsk withdraws a question the calling seat asked, or a call its
// executor holds for a person's approval (ADR-0111).
//
// # Only the seat's own
//
// A question on a work item is resolved by its author alone, which the
// tracker's writer decides in its own snapshot ([worklet.ErrNotAuthor]); a
// run is the seat's own when its row names the seat, read here. Nobody else's
// question is withdrawn through this tool, and a person's is never: a person
// resolves their own from the item.
//
// # It says why, where the person reads it
//
// On a work item the reason is posted in the question's thread before the
// question is resolved, so the person asked reads why it closed. A run is told
// its question was withdrawn, with the reason, as its answer: a held call is
// answered Deny by the seat itself, so the call is never made, and the run
// carries on under the access it holds now.
type withdrawAsk struct {
	deps WithdrawDeps
}

var _ tools.SeatCallable = (*withdrawAsk)(nil)

func (t *withdrawAsk) Name() string { return WithdrawAskTool }

func (t *withdrawAsk) Description() string { return withdrawAskDescription }

// withdrawAskDescription is the tool's contract as a model reads it.
var withdrawAskDescription = "Withdraw a question you asked that is still open, or a call your " +
	"executor holds for a person's approval, when it no longer fits: your access changed, or " +
	"the work moved on. Name a question on a work item with `item` and `ask` (the question's " +
	"comment id), or a question or held call your executor waits on with `run` (its turn id). " +
	"`reason` is required and is what the person reads: say why, in one or two sentences. A " +
	"question's options never change once asked, so to re-word a question or change its " +
	"options, withdraw it and ask again with `" + toolname.CommentOnWorkItem + "`."

func (t *withdrawAsk) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The work item the question is on. " + itemRef,
			},
			"ask": map[string]any{
				"type":        "string",
				"description": "The question's comment id on that item. It must be yours and still open.",
			},
			"run": map[string]any{
				"type": "string",
				"description": "The turn id of your executor run that waits on a person for a " +
					"question or a held call. Instead of `item` and `ask`.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Why you withdraw it, as the person who reads it should hear it.",
			},
		},
		"required": []any{"reason"},
	}
}

func (t *withdrawAsk) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *withdrawAsk) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	seat := turn.Handle()
	if seat == "" {
		return notInATurn(WithdrawAskTool), nil
	}
	reason := strings.TrimSpace(argString(args, "reason"))
	item := strings.TrimSpace(argString(args, "item"))
	ask := strings.TrimSpace(argString(args, "ask"))
	run := strings.TrimSpace(argString(args, "run"))
	switch {
	case reason == "":
		return failed(WithdrawAskTool + " needs a `reason`: the person reads why the question " +
			"closed."), nil
	case len(reason) > MaxWithdrawReasonBytes:
		return failed(fmt.Sprintf(WithdrawAskTool+": `reason` is %d bytes and the most is %d. "+
			"Say why in a sentence or two.", len(reason), MaxWithdrawReasonBytes)), nil
	case run != "" && (item != "" || ask != ""):
		return failed(WithdrawAskTool + " withdraws one thing: a question on a work item " +
			"(`item` and `ask`) or what your executor waits on (`run`), never both."), nil
	case run != "":
		return t.withdrawRun(ctx, seat, run, reason)
	case item != "" && ask != "":
		return t.withdrawQuestion(ctx, turn, item, ask, reason, args)
	}
	return failed(WithdrawAskTool + " needs `item` and `ask` (a question on a work item) or " +
		"`run` (what your executor waits on)."), nil
}

// withdrawRun withdraws the question or held call seat's own run waits on, as
// that run's answer.
func (t *withdrawAsk) withdrawRun(ctx context.Context, seat, turnID, reason string) (tools.Result, error) {
	if t.deps.Desk == nil {
		return unconfigured(WithdrawAskTool), nil
	}
	run, found, err := t.deps.Desk.Run(ctx, turnID)
	if err != nil {
		return readFailure(ctx, WithdrawAskTool, err), nil
	}
	if !found || run.AgentHandle != seat {
		// ANOTHER SEAT'S RUN READS AS NONE: what a run of somebody else's
		// waits on is not this seat's to withdraw, nor to learn about.
		return refused(tools.RefusalNotFound, fmt.Sprintf("No run %s of yours waits on a "+
			"person. Name one of your own runs by its turn id.", clip(turnID))), nil
	}
	if run.Ending != nil || !slices.Contains(sandbox.Awaiting, run.Status) ||
		(run.HeldCall == nil && run.Question == "") {
		return refused(tools.RefusalNotRunning, fmt.Sprintf("Run %s does not wait on a person "+
			"now: it was answered, or it moved on. Nothing is withdrawn.", run.TurnID)), nil
	}
	given := types.ExecutorAnswerGiven{
		TurnID: run.TurnID, Agent: run.AgentID, AgentHandle: run.AgentHandle,
		Answer: reason, AnsweredBy: seat, AnsweredByKind: string(iam.ActorAgent),
		// THE QUESTION THIS WITHDRAWS, off the row just read as waiting:
		// the withdrawal carries the run on only while it still waits on
		// that question, and is spent otherwise.
		LaunchID: run.LaunchID,
	}
	what := "question"
	if held := run.HeldCall; held != nil {
		// A HELD CALL IS WITHDRAWN AS DENY, by the seat itself: the call is
		// never made, and the run is told it withdrew it.
		given.Choice, given.Approval, what = sandbox.HeldDeny, held.ID, "held call"
	}
	outcome := statelog.OutcomePending
	answerID, err := t.deps.Desk.Deliver(ctx, given)
	if err != nil {
		log.WarnContext(ctx, "withdraw_ask_delivery_unknown", "turn_id", run.TurnID,
			"seat", seat, "error", err.Error())
		outcome = statelog.OutcomeUnknown
	}
	result := map[string]any{
		"withdrawn": what, "run": run.TurnID, "launch_id": run.LaunchID,
		"outcome": string(outcome),
		"note": "The run carries on, told you withdrew it and why, once your seat is free; " +
			"the person no longer sees it waiting.",
	}
	if answerID != uuid.Nil {
		result["answer_id"] = answerID.String()
	}
	if outcome == statelog.OutcomeUnknown {
		result["note"] = "The withdrawal may or may not have reached the run. Calling this again " +
			"is safe: a run withdrawn once ignores a second withdrawal."
	}
	return jsonResult(result)
}

// withdrawQuestion withdraws seat's own open question on a work item: the
// reason posted in its thread, then the question resolved as its author.
func (t *withdrawAsk) withdrawQuestion(ctx context.Context, turn *turnctx.Turn, ref, askID,
	reason string, args map[string]any) (tools.Result, error) {

	if !t.deps.tracker() {
		return unconfigured(WithdrawAskTool), nil
	}
	work := t.deps.Work
	actor, err := work.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(WithdrawAskTool), nil
	}
	actor, denied := work.bindOperation(actor, WithdrawAskTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	// A REPLY IN A THREAD IS A COMMENT, decided as one: the tracker's app,
	// for the seat and for whoever woke the turn (ADR-0109).
	holder, refusal := work.itemGate(ctx, authz.ActionWorkComment)
	if refusal != nil {
		return *refusal, nil
	}
	if !holder {
		return refused(tools.RefusalForbidden, WithdrawAskTool+" posts the reason on the item, "+
			"which takes Worklet."), nil
	}
	before, err := work.Reader.Task(ctx, ref, worklet.DetailWants{Comment: askID}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, WithdrawAskTool, err), nil
	}
	var question *worklet.Comment
	for i := range before.Comments {
		if before.Comments[i].ID == askID {
			question = &before.Comments[i]
		}
	}
	switch {
	case question == nil || question.Removed:
		return refused(tools.RefusalNotFound, fmt.Sprintf("There is no comment %s on %s.",
			clip(askID), before.Named())), nil
	case question.Author != actor.Handle:
		return refused(tools.RefusalForbidden, fmt.Sprintf("Comment %s on %s is not yours: only "+
			"its author withdraws a question.", question.ID, before.Named())), nil
	case question.Ask == "":
		return failed(fmt.Sprintf("Comment %s on %s asks nobody anything, so there is no "+
			"question to withdraw.", question.ID, before.Named())), nil
	case question.Resolved:
		return refused(tools.RefusalAlreadyAnswered, fmt.Sprintf("Your question %s on %s is "+
			"already closed. Nothing is withdrawn.", question.ID, before.Named())), nil
	}
	writer := work.Writer(actor)
	reply := &worklet.Comment{
		ID:   commentID(actor, WithdrawAskTool, before.Task.ID, args),
		Task: before.Task.ID, Author: actor.Handle, AuthorKind: actor.Kind,
		Body: reason, CreatedAt: work.now(), ReplyTo: &question.ID,
	}
	patch := worklet.TaskPatch{Comment: reply}
	replyOp := opIDFor(actor, WithdrawAskTool, "reason", reply.ID, args)
	notify := worklet.Wake{Kind: worklet.ChangeComment, Before: before.Task,
		After: patched(before.Task, patch), Comment: reply}.Notify(work.Leads)
	posted, err := writer.UpdateTask(ctx, replyOp, before.Task.ID, before.Task.Project,
		worklet.NoIfMatch, patch, worklet.ChangeComment, notify)
	if err != nil {
		return writeFailure(ctx, actor, WithdrawAskTool, err), nil
	}
	if posted.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, WithdrawAskTool,
			fmt.Sprintf("the reason on %s was posted", before.Named()), replyOp,
			posted.Unvouched, unknownNext(posted.Unvouched, sameCall(actor, WithdrawAskTool),
				fmt.Sprintf("Read %s's thread with "+GetWorkItemTool, before.Address()),
				"that is a second reason")), nil
	}
	resolved := *question
	resolved.Resolved, resolved.ResolvedBy = true, actor.Handle
	resolveOp := opIDFor(actor, WithdrawAskTool, "resolve", question.ID, args)
	got, err := t.deps.Resolver(actor).ResolveComment(ctx, resolveOp, before.Task.ID,
		before.Task.Project, question.ID, worklet.Wake{Kind: worklet.ChangeCommentResolved,
			Before: before.Task, After: before.Task, Comment: &resolved}.Notify(work.Leads))
	if err != nil {
		return writeFailure(ctx, actor, WithdrawAskTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, WithdrawAskTool,
			fmt.Sprintf("your question %s on %s was closed", question.ID, before.Named()),
			resolveOp, got.Unvouched, unknownNext(got.Unvouched, sameCall(actor, WithdrawAskTool),
				fmt.Sprintf("Read %s with "+GetWorkItemTool, before.Address()),
				"that changes nothing")), nil
	}
	work.settle(ctx, got.Position)
	return jsonResult(withOperation(ReceiptOf(map[string]any{
		"withdrawn": "question", "comment_id": question.ID, "reason_comment_id": reply.ID,
		"outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version": got.Version,
	}, before), actor))
}
