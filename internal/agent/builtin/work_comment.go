package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- comment_on_work_item ---------------------------------------------- //

type commentOnWorkItem struct {
	itemTarget
	deps WorkDeps

	// pages checks a decision's page evidence — see [evidenceLookup]. Nil
	// on a company with no Doclet.
	pages PageReader
}

var _ tools.SeatCallable = (*commentOnWorkItem)(nil)

func (t *commentOnWorkItem) Name() string { return CommentOnWorkTool }

func (t *commentOnWorkItem) Description() string {
	return commentToolDescription
}

// commentToolDescription is the comment tool's contract as a model reads it,
// the wake rule included: another agent's comment wakes a colleague only when it
// names them ([worklet.CandidateFor]). A constant so a test can hold the words.
const commentToolDescription = "Post a comment on a work item. Everyone " +
	"following the item finds it in their inbox, but your comment wakes a " +
	"colleague to act on it only when it asks them: an @-mention with " +
	"`asks: true`, a question put to them with `ask`, or a reply (`reply_to`) " +
	"in a thread where they asked or were asked something still open. An " +
	"@-mention with `asks: false` is a reference and wakes nobody, and a " +
	"comment that asks nothing wakes nobody, not even the item's assignee or " +
	"its reporter: it waits until they next open the item. So name whoever " +
	"you need to act, and say what you need from them. Post ONE " +
	"substantive comment when you have something to say: running commentary " +
	"is noise on a surface other people read, and an acknowledgement that " +
	"asks nothing needs no comment at all. `ask` puts a question to somebody, " +
	"and `decision` offers it as options with the one you recommend, which " +
	"every question an agent puts to a person carries; `answers` with `choice` " +
	"answers one."

func (t *commentOnWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to comment on. " + itemRef,
			},
			"body": map[string]any{
				"type": "string",
				"description": "The comment, in markdown. @-mention a " +
					"colleague by handle, with `asks: true`, to wake them to act on it.",
			},
			"asks": asksParam(workAsks),
			"ask": map[string]any{
				"type": "string",
				"description": "A colleague's handle: this comment is a " +
					"QUESTION they owe an answer to. They are woken asking " +
					"for one and start following the item. It does not hand " +
					"the item over, and it does not stop anybody closing it. " +
					"An agent always asks a person with a `decision`.",
			},
			"decision": decisionSchema(),
			"answers": map[string]any{
				"type": "string",
				"description": "The comment id of the question this answers, " +
					"which closes it. Omitted, it is inferred when exactly " +
					"one open question on the item is addressed to you.",
			},
			"choice": map[string]any{
				"type": "string",
				"description": "When the question you answer carries a " +
					"decision: the id of the option you choose. The comment " +
					"then opens with the option's label (`Chose “…”`), and " +
					"`body` is optional: why, which follows it. Leave it out " +
					"to answer in prose when none of the options is right.",
			},
			"reply_to": map[string]any{
				"type": "string",
				"description": "The id of the comment you are replying to. " +
					"This threads the conversation and wakes everybody " +
					"already in that thread; it does not close a question — " +
					"`answers` is what does.",
			},
		},
		"required": []any{"item"},
	})
}

func (t *commentOnWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *commentOnWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(CommentOnWorkTool, err), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(CommentOnWorkTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, CommentOnWorkTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	writer := t.deps.Writer(actor)

	ref := strings.TrimSpace(argString(args, "item"))
	body := strings.TrimSpace(argString(args, "body"))
	choice := strings.TrimSpace(argString(args, "choice"))
	rawDecision, decided := args["decision"]
	agent := actor.Kind == worklet.AuthorAgent
	asks, refused := asksOf(CommentOnWorkTool, args, agent)
	switch {
	case refused != nil:
		return *refused, nil
	case ref == "":
		return failed(worklet.CommentOnWorkTool + " needs an `item` — a key like ENG-42, or an id."), nil
	case body == "" && choice == "":
		// A choice is an answer on its own; a reason is not owed.
		return failed(worklet.CommentOnWorkTool + " needs a `body`. Say the substantive " +
			"thing, once — or, answering a decision, a `choice`."), nil
	case decided && strings.TrimSpace(argString(args, "ask")) == "":
		// Before any read, whatever the item says.
		return failed(worklet.CommentOnWorkTool + ": `decision` needs `ask` — the " +
			"handle of the person who has to choose. A decision asked of " +
			"nobody wakes nobody."), nil
	case decided && choice != "":
		return failed(worklet.CommentOnWorkTool + ": a comment either asks a decision " +
			"(`decision`) or answers one (`choice`), never both — they are two " +
			"comments, by two people."), nil
	}
	// Who may post is decided on the item (workasked.go): a tracker holder, or
	// anybody else only as THE ANSWER to an open ask naming them, so what can
	// never be an answer is refused before the item is read.
	holder, refused := t.deps.itemGate(ctx, authz.ActionWorkComment)
	if refused != nil {
		return *refused, nil
	}
	var before worklet.TaskDetail
	if holder {
		before, err = t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
		switch {
		case errors.Is(err, worklet.ErrNoTask):
			return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
				clip(ref))), nil
		case err != nil:
			return readFailure(ctx, CommentOnWorkTool, err), nil
		}
	} else {
		// A question of your own is not an answer.
		if decided || strings.TrimSpace(argString(args, "ask")) != "" {
			return *t.deps.answerOnly(ctx, "`ask` and `decision` put a new "+
				"question to somebody, which takes Worklet."), nil
		}
		// With the id this comment will carry, so the answer made again after
		// it landed (which closed the ask that admitted it) is still admitted,
		// and answered from its row.
		if before, refused = t.deps.askedOn(ctx, authz.ActionWorkComment, ref,
			actor.Handle, func(task string) string {
				return commentID(actor, t.Name(), task, args)
			}); refused != nil {
			return *refused, nil
		}
	}

	now := t.deps.now()
	comment := &worklet.Comment{
		ID:         commentID(actor, t.Name(), before.Task.ID, args),
		Task:       before.Task.ID,
		Author:     actor.Handle,
		AuthorKind: actor.Kind,
		Body:       body,
		Choice:     choice,
		CreatedAt:  now,
	}
	if decided {
		decision, refusal := t.deps.readDecision(ctx, CommentOnWorkTool,
			actor, rawDecision, t.pages)
		if refusal != nil {
			return *refusal, nil
		}
		comment.Decision = decision
		// Never read from the caller's arguments (ADR-0125).
		if act, ok := delegated.ActOf(ctx); ok {
			comment.Act = &act
		}
	}
	if reply := strings.TrimSpace(argString(args, "reply_to")); reply != "" {
		comment.ReplyTo = &reply
	}
	if t.deps.Mentions != nil {
		comment.Mentions = t.deps.Mentions.Mentions(body)
	}
	// One naming nobody leaves a question open forever.
	ask := strings.TrimSpace(argString(args, "ask"))
	if ask != "" {
		resolved, refusal := t.deps.resolveHandle(ctx, CommentOnWorkTool, "`ask`", ask)
		if refusal != "" {
			return failed(refusal), nil
		}
		if refused := t.deps.checkAskOfPerson(CommentOnWorkTool, actor, resolved, decided); refused != nil {
			return *refused, nil
		}
		ask = resolved
	}
	if refused := t.deps.checkAsksOfNamed(CommentOnWorkTool, actor, asks, ask, comment.Mentions); refused != nil {
		return *refused, nil
	}
	query := worklet.ThreadQuery{
		Task:    before.Task.ID,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
		Ask:     ask,
		Answers: strings.TrimSpace(argString(args, "answers")),
		// A seat, for a bound person: an ask names a seat.
		Author: actor.Handle,
		// So a re-run whose first answer landed is not told somebody (itself)
		// already answered.
		Comment: comment.ID,
		Choice:  comment.Choice,
	}
	var thread worklet.ResolvedThread
	var threadRefusal *tools.Result
	if holder {
		thread, threadRefusal = t.deps.resolveThread(ctx, query)
	} else {
		// Strict: the answer is all that admits this caller, so an inference
		// this node could not make is a failed read.
		thread, threadRefusal = t.deps.readThread(ctx, query)
	}
	if threadRefusal != nil {
		return *threadRefusal, nil
	}
	if !holder && thread.Answers == "" {
		return *t.deps.answerOnly(ctx, "this comment answers no open question "+
			"addressed to you. Name it with `answers` (its comment id)."), nil
	}
	comment.Ask = thread.Asked
	if thread.Answers != "" {
		comment.Answers = &thread.Answers
	}
	if comment.Choice != "" && comment.Answers == nil {
		// No `answers` was named and no open question is addressed to this
		// caller: no decision for the option to belong to.
		return toPerson(failed(fmt.Sprintf(worklet.CommentOnWorkTool+": `choice` %q "+
			"answers a decision, and no open question on %s is addressed to "+
			"you. Name the question with `answers` (its comment id), or answer "+
			"in `body` without a choice.", clip(comment.Choice),
			before.Named())), caveat.NoQuestionForChoice()), nil
	}
	// AN ANSWER THAT CHOSE SAYS WHAT IT CHOSE, the `body` after it: read by id
	// alone it was an empty comment to everybody but the dashboard. Mentions
	// stay the caller's own words', since the label is the asker's.
	if comment.Choice != "" && thread.AnswersDecision != nil {
		if option, ok := thread.AnswersDecision.Option(comment.Choice); ok {
			comment.Body = worklet.ChoiceWords(option.Label, body)
		}
	}
	if refused := t.deps.grantChosen(ctx, comment.Choice, thread); refused != nil {
		return *refused, nil
	}
	// An answer approving an act is that act's approval: whether this person may
	// give one, at its proof, is decided here and carried to the tracker's check
	// of the choice against its ask (ADR-0125).
	if comment.Choice != "" && t.deps.ApprovesAct != nil {
		for _, kind := range delegated.DelegableKinds() {
			if t.deps.ApprovesAct(ctx, kind) {
				ctx = delegated.WithApproval(ctx, kind)
			}
		}
	}

	// A comment rides the task's own write, sharing its arbitration. The
	// commenter's watch is a GESTURE, not a set: a set read outside the writer's
	// decide would be last-write-wins and could pass [worklet.MaxWatchers]. It
	// resolves inside the decide, and `Auto` since nobody pressed watch: at the
	// cap the comment lands and the watch is skipped.
	patch := worklet.TaskPatch{Comment: comment}
	if holder {
		// Not for somebody admitted only to answer: following would wake them on
		// every later change to a board they do not use.
		patch.Watch = &worklet.WatchIntent{
			Handle: actor.Handle, Watch: true, Auto: true,
		}
	}
	opID := opIDFor(actor, t.Name(), "comment", comment.ID, args)
	notify := worklet.Wake{
		Kind:   worklet.ChangeComment,
		Before: before.Task,
		// Through [patched], so the gesture is applied once, cap included.
		After:   patched(before.Task, patch),
		Comment: comment, Mentions: comment.Mentions,
		Asks:            t.deps.statedAsks(agent, asks || comment.Ask != ""),
		MentionedAgents: t.deps.agentsAmong(agent, comment.Mentions),
		Thread:          thread.ThreadParties,
		AnswersDecision: thread.AnswersDecision,
	}.Notify(t.deps.Leads)
	got, err := writer.UpdateTask(ctx, opID, before.Task.ID, before.Task.Project,
		// A comment replaces nobody's value: no version condition.
		worklet.NoIfMatch, patch, worklet.ChangeComment, notify)
	if err != nil {
		return writeFailure(ctx, actor, CommentOnWorkTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// Its id and ask beside "unknown" would read as a posted remark.
		return unknownWrite(actor, CommentOnWorkTool,
			fmt.Sprintf("the comment on %s was posted", before.Named()), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, CommentOnWorkTool),
				fmt.Sprintf("Read %s's thread with "+GetWorkItemTool, before.Address()),
				"that is a second comment")), nil
	}
	if got.Posted != nil {
		// A repeat whose first attempt posted: the writer answered from that
		// row (later than the thread read), which says what it asked and
		// answered.
		comment.Ask = got.Posted.Asked
		comment.Answers = nil
		if got.Posted.Answers != "" {
			answered := got.Posted.Answers
			comment.Answers = &answered
		}
	}
	t.deps.settle(ctx, got.Position)
	t.deps.requestDeclined(ctx, comment.Choice, actor.Handle, thread)
	answer := withOperation(ReceiptOf(map[string]any{
		"comment_id": comment.ID,
		"mentioned":  comment.Mentions, "outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version": got.Version,
	}, before), actor)
	if comment.Ask != "" {
		answer["asked"] = comment.Ask
	}
	if comment.Answers != nil {
		answer["answered"] = *comment.Answers
	}
	if comment.Choice != "" {
		answer["choice"] = comment.Choice
	}
	if comment.Decision != nil {
		// As stored: a task named by key in the evidence comes back as its id.
		answer["decision"] = comment.Decision
	}
	// A non-assignee's comment naming nobody wakes nobody, so an expected answer
	// may never come.
	var caveats []caveat.Caveat
	if unaddressed := unaddressedCaveat(before.Task, actor, comment, notify); !unaddressed.IsZero() {
		caveats = append(caveats, unaddressed)
	}
	return ownReceipt(ctx, turn, actor, CommentOnWorkTool, answer, caveats)
}

// ownReceipt answers a write's receipt, which may echo what the caller just
// wrote (an ask's question and options), through a [foreignRead] labelling those
// words as the caller's own (ADR-0040). Both paths carry the caveats cs and
// partials through [caveated] ([caveatedBy]).
func ownReceipt(ctx context.Context, turn *turnctx.Turn, actor Actor, tool string,
	answer map[string]any, cs []caveat.Caveat, partials ...caveat.Caveat) (tools.Result, error) {

	declared, err := declaredIn(answer)
	if err != nil || len(declared) == 0 {
		return caveated(answer, cs, partials...)
	}
	read := newForeignRead(turn, actor.Handle)
	read.attribute([]provenance.Author{read.writer(actor.Handle, iam.ActorKind(actor.Kind))}, false)
	return caveatedBy(func(v any) (tools.Result, error) {
		return read.answer(ctx, tool, v, "")
	}, answer, cs, partials...)
}

// unaddressedCaveat says when a comment leaves the item's assignee unasked: not
// woken at all, or woken without being asked anything. notify is the record the
// comment publishes, so what it says is the routing's own answer.
func unaddressedCaveat(task worklet.Task, actor Actor, comment *worklet.Comment,
	notify *worklet.Notify) caveat.Caveat {

	switch {
	// Against the author, a bound person's seat: a person commenting on their
	// own task is not waking themselves.
	case task.Assignee == "" || task.Assignee == actor.Handle:
		return caveat.Caveat{}
	case comment.Ask != "" || (len(comment.Mentions) > 0 && notify.MentionsWake()):
		return caveat.Caveat{}
	// An answer expects no reply: it closes a question and wakes the asker.
	case comment.Answers != nil:
		return caveat.Caveat{}
	}
	// The routing's answer for this record: a person's comment on the task is
	// the ask and needs no warning; another agent's reaches the assignee's inbox
	// and starts no turn, unless it is a reply in a thread where they still owe
	// an answer, which wakes them without asking anything (ADR-0113).
	heard, ok := worklet.CandidateFor(notify, task.Assignee)
	if !ok || heard.Addressed {
		return caveat.Caveat{}
	}
	return caveat.UnaddressedComment(task.Assignee, heard.Wakes)
}

// commentID is the comment's id, derived so a re-run turn posts once: a UUIDv5
// (package doc, "What the tracker's tools derive from a call's key"), since the
// id is a PRIMARY KEY on every node.
//
// From the call's key ([Actor.callKey]) and digest, as [opIDFor], so every
// different call is a different comment. The key is as the work began
// ([Actor.asBegun]): a rebase re-mints operations for the ledger, while the
// comment's row remembers, so a re-post is answered from it and wakes nobody
// twice. A caller with no turn derives it from [Actor.Operation] alone.
func commentID(actor Actor, tool, taskID string, args map[string]any) string {
	if actor.Operation != "" {
		return uuid.NewSHA1(commentNamespace, []byte(actor.Operation+"\x00"+
			taskID+"\x00"+actor.Handle)).String()
	}
	key := actor.asBegun().callKey(tool, nil, args, "")
	if key == "" {
		key = uuid.NewString()
	}
	return uuid.NewSHA1(commentNamespace, []byte(key+"\x00"+taskID+"\x00"+
		actor.Handle+"\x00"+turnctx.ArgsDigest(args))).String()
}

// commentNamespace is the uuid namespace comment ids derive under, FIXED for the
// life of the format (package doc).
var commentNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")
