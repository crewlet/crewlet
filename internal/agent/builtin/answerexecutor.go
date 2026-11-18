package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/runanswer"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// AnswerExecutorTool is the tool's wire name.
const AnswerExecutorTool = "answer_executor"

// RunDesk is what answer_executor needs: the run an answer names, delivery to
// the control topic of the seat that holds it, and who gave a run's recorded
// answer, which only its seal says ([sandbox.AnswerSeal]). [sandbox.AnswerDesk]
// implements it.
type RunDesk interface {
	Run(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
	Deliver(ctx context.Context, given types.ExecutorAnswerGiven) (uuid.UUID, error)
	// Answerer is who gave run's recorded answer, empty where the seal does
	// not open here.
	Answerer(run sandbox.PendingRun) string
}

// RunDeps are answer_executor's dependencies.
type RunDeps struct {
	// Desk reads the run and delivers the answer. Nil omits the tool.
	Desk RunDesk

	// Actor is who is answering, off the caller's credential ([WorkDeps.Actor]).
	// Required with Desk: an answer nobody can be named for is refused.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
}

// runParties is the run's identity on its row: its seat by id and handle, and
// its requester, the seat whose wake started the turn that launched it.
type runParties struct {
	seat      uuid.UUID
	handle    string
	requester string
}

// answerExecutor answers a parked executor by naming its turn, so a run with
// no conversation to reply into (launched by an assignment, a colleague's ask
// or a schedule: internal/notify's Derived) is answerable too. Its requester
// (through the rule's self arm, only when the caller IS them, never their
// lead), the seat's lead or an admin may answer ([authz.ActionExecutorAnswer]).
//
// It answers `pending`: only the node holding the seat records and resumes, so
// the run's record, read by `launch_id` and `answer_id`, says what became of it
// (`answered`, among `declined_answers`, or a new question). Naming the
// question too, an answer landing after the run moved on is announced
// `executor_answered` `not_awaiting` instead of resuming it
// ([sandbox.Coordinator.AnswerByTurn]).
type answerExecutor struct {
	deps      RunDeps
	authorize Authorizer
}

var _ tools.SeatCallable = (*answerExecutor)(nil)

func (t *answerExecutor) Name() string { return AnswerExecutorTool }

func (t *answerExecutor) Description() string {
	return "Answer a seat's executor that stopped to ask a person a question, " +
		"by naming its turn. Where the question offers options, name the one " +
		"chosen in `choice`, with any words beside it in `answer`; an open " +
		"question is answered in `answer`. The answer is recorded on the " +
		"executor's row at once, and the executor carries on with it as the " +
		"reply to the question it is waiting on now, on the node that holds its " +
		"seat, as soon as that seat is free of any other turn's executor. Use " +
		"this for any parked executor, including one started by a schedule, an " +
		"assignment or a colleague, which has no conversation to reply in. Find " +
		"parked executors and their questions on the executors board."
}

func (t *answerExecutor) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_id": map[string]any{
				"type":        "string",
				"description": "The turn id of the parked executor — its `turn_id` on the executors board.",
			},
			"launch_id": map[string]any{
				"type": "string",
				"description": "The question you are answering: the executor's `launch_id` on " +
					"the executors board, read with the question. An executor that has moved " +
					"on to another question since refuses the answer, so it never answers one " +
					"you did not see.",
			},
			"choice": map[string]any{
				"type": "string",
				"description": "The id of the option chosen, where the executor's question " +
					"offers options: one of its `decision.options` on the executors board.",
			},
			"answer": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Your answer to the executor's question, as "+
					"its harness should read it: the whole answer to an open question, "+
					"or what to add to a `choice`. At most %d KiB: put anything longer on "+
					"a page the executor can read, and say where it is.", sandbox.MaxAnswerTextBytes>>10),
			},
		},
		"required": []any{"turn_id", "launch_id"},
	}
}

func (t *answerExecutor) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *answerExecutor) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Desk == nil || t.deps.Actor == nil {
		return refusedBy(tools.RefusalUnavailable, waitless(), AnswerExecutorTool+
			" is unavailable: this surface was built without the executors' record."), nil
	}
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return refused(tools.RefusalForbidden, AnswerExecutorTool+
			" answers as the person whose credential calls it, and this call carries none."), nil
	}
	turnID := strings.TrimSpace(argString(args, "turn_id"))
	if turnID == "" {
		return failed(AnswerExecutorTool + " needs a `turn_id` — the parked executor's turn id."), nil
	}
	launchID := strings.TrimSpace(argString(args, "launch_id"))
	if launchID == "" {
		return failed(AnswerExecutorTool + " needs a `launch_id`: the question you are answering, " +
			"the executor's `launch_id` read with it on the executors board."), nil
	}
	answer := strings.TrimSpace(argString(args, "answer"))
	choice := strings.TrimSpace(argString(args, "choice"))
	switch {
	case answer == "" && choice == "":
		return failed(AnswerExecutorTool + " needs a `choice` naming the option chosen, or an " +
			"`answer` saying what the harness should be told, or both."), nil
	case len(answer) > sandbox.MaxAnswerTextBytes:
		// Measured as the coordinator measures where it records: trimmed,
		// before redaction ([sandbox.MaxAnswerTextBytes]), so an answer
		// accepted here is never refused there for a redaction marker.
		return failed(fmt.Sprintf("That answer is %d KiB and one answer may be at most %d KiB: "+
			"it becomes the executor's next message, or a fresh launch's brief where the "+
			"executor cannot carry on in its own session, and its harness reads it again "+
			"at every step after. Put the detail on a page the executor can read, and answer "+
			"with where it is.",
			kib(len(answer)), sandbox.MaxAnswerTextBytes>>10)), nil
	}

	run, found, err := t.deps.Desk.Run(ctx, turnID)
	switch {
	case err != nil:
		return nodeFailure(ctx, AnswerExecutorTool, err, fmt.Sprintf(AnswerExecutorTool+" could not "+
			"read the executor of turn %s", clip(turnID)), "This does NOT mean it is gone, and "+
			"nothing was answered."), nil
	case !found:
		// not_running, not not_found: an ended run has no record, like an id
		// that never named one, and of both it is true nothing waits.
		return refused(tools.RefusalNotRunning, fmt.Sprintf("No executor is "+
			"parked under turn %s: it has finished or ended, or the id names no turn "+
			"with one.", clip(turnID))), nil
	case !slices.Contains(sandbox.Awaiting, run.Status) && run.Answer != nil:
		// The first recorded answer wins, and a later one is told whose and
		// when. A refused oversized reply closed the question the same way,
		// and is never told as an answer the executor resumes with (ADR-0114).
		by := t.deps.Desk.Answerer(run)
		if refusal, isRefusal := run.Refused(); isRefusal {
			return toPerson(refused(tools.RefusalAlreadyAnswered, refusedAlready(run, refusal, by)),
				caveat.RefusedReplyFirst(by)), nil
		}
		return toPerson(refused(tools.RefusalAlreadyAnswered, answeredAlready(run, by)),
			caveat.AnsweredAlready(by)), nil
	case !slices.Contains(sandbox.Awaiting, run.Status):
		return refused(tools.RefusalNotRunning, fmt.Sprintf("The executor of turn %s is "+
			"%s, not waiting for an answer, so there is no question for this to answer.",
			clip(turnID), run.Status)), nil
	}
	if run.HeldCall != nil {
		// A held call is approved by its own route (ADR-0077), which asks
		// whether the caller may approve a call for this seat and writes the
		// level Always allow sets; this tool admits the run's requester, so it
		// would approve on a narrower grant.
		return toPerson(refused(tools.RefusalForbidden, fmt.Sprintf("The executor of turn %s is "+
			"waiting for a person to approve a call to %s, which is answered with POST "+
			"/mcp-approvals/%s by a person its approval admits, not by this tool. Nothing was "+
			"answered.", clip(turnID), run.HeldCall.Tool, run.HeldCall.ID)), caveat.HeldCallAnswer()), nil
	}
	// The question the person read: a run that moved on since must not take
	// an answer, or a choice by id, given to one it no longer asks.
	if launchID != run.LaunchID {
		return toPerson(refused(tools.RefusalStaleVersion, fmt.Sprintf("The executor of "+
			"turn %s has moved on to another question since the one you answered (launch "+
			"%s): read its question again on the executors board and answer that one. "+
			"Nothing was answered.", clip(turnID), clip(launchID))), caveat.QuestionChanged()), nil
	}
	// The choice is checked against the row just read as waiting: a run asks a
	// new question only under a new job, and the answer is pinned to this one.
	if _, unoffered := sandbox.AnswerText(run.Decision, choice, answer); unoffered != nil {
		return refusedBy(tools.RefusalInvalid, unoffered, fmt.Sprintf(AnswerExecutorTool+
			" was refused: %s. Nothing was answered.", unoffered.Error())), nil
	}
	parties, refusal := t.parties(ctx, run)
	if refusal != nil {
		return *refusal, nil
	}
	if refusal := t.mayAnswer(ctx, actor, parties); refusal != nil {
		return *refusal, nil
	}

	// Named as [iam.ActorFor] names them: a person bound to a seat answers AS
	// it, kind human, beside the credential used. The desk re-reads the row to
	// stamp the seat it delivers to, so it is the seat named here.
	given := types.ExecutorAnswerGiven{
		TurnID: run.TurnID, Agent: parties.seat.String(), AgentHandle: parties.handle,
		Answer: answer, Choice: choice, AnsweredBy: actor.Handle,
		AnsweredByKind: string(actor.Kind),
		OperatorID:     actor.OperatorID,
		// The run resumes with the answer only while it still waits on this.
		LaunchID: run.LaunchID,
	}
	outcome := statelog.OutcomePending
	answerID, err := t.deps.Desk.Deliver(ctx, given)
	if err != nil {
		// The publish may have landed, so `unknown`, not a refusal
		// ([sandbox.AnswerDesk.Deliver]). A retry is safe: both copies name
		// their question, so the one consumed second finds it not waiting.
		log.WarnContext(ctx, "answer_executor_delivery_unknown",
			"turn_id", run.TurnID, "seat", parties.handle, "error", err.Error())
		outcome = statelog.OutcomeUnknown
	}
	result := map[string]any{
		"turn_id": run.TurnID, "agent_handle": parties.handle,
		"question": run.Question, "outcome": string(outcome),
		// With answer_id, what the run's record is read against.
		"launch_id": run.LaunchID,
	}
	if answerID != uuid.Nil {
		result["answer_id"] = answerID.String()
	}
	if choice != "" {
		result["choice"] = choice
	}
	// The question is the harness's words, usually read through an assistant:
	// fenced by [provenance.Executor] like every door to them (ADR-0040).
	read := newForeignRead(turn, actor.Handle)
	asked := provenance.Executor(parties.handle)
	read.fenceAt(fieldPath{}.key("question"), asked.Source, asked.Trust, asked.Authors,
		run.Question)
	return read.answer(ctx, AnswerExecutorTool, result, "")
}

// refusedAlready is the model's refusal for a question an oversized reply
// reached first: whose (by, from the refusal's seal, or "someone"), when (RFC
// 3339), its figures but never its words, which no record keeps, and that the
// executor is resumed told so and asks again if it still needs an answer.
//
// Never worded as an answer, which the run did not get, and worded as what is
// recorded: the resume that tells the executor is owed, not done (a refusal
// let go of before any turn told it puts the question back to waiting).
func refusedAlready(run sandbox.PendingRun, refusal sandbox.AnswerRefusal, by string) string {
	if by == "" {
		by = "someone"
	}
	when := ""
	if !run.Answer.RecordedAt.IsZero() {
		when = " at " + run.Answer.RecordedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("A reply from %s in the conversation reached this question first%s. It came "+
		"to %s as the engine records an answer, past the %s an answer to an executor can carry, so it "+
		"was not taken: it was refused as the question's outcome, the question no longer waits, the "+
		"executor is resumed told the reply did not reach it, and your answer was not used. If the "+
		"executor still needs an answer it asks again, with a question of its own: answer that one.",
		by, when, kept.SizeUp(refusal.Bytes), kept.Size(refusal.MaxBytes))
}

// answeredAlready is the model's refusal for a question already answered: by
// whom (by, from the answer's seal, or "someone else"), when (RFC 3339, as
// recorded), and what the record says became of it. A person's surface shows
// [caveat.AnsweredAlready] instead, on the person's own clock.
func answeredAlready(run sandbox.PendingRun, by string) string {
	if by == "" {
		by = "someone else"
	}
	when := ""
	if !run.Answer.RecordedAt.IsZero() {
		when = " at " + run.Answer.RecordedAt.UTC().Format(time.RFC3339)
	}
	said := fmt.Sprintf("This question was already answered by %s%s, so your answer was not used.",
		by, when)
	switch run.Status {
	case sandbox.StatusAnswered:
		return said + " The executor resumes with that answer once its seat is free."
	case sandbox.StatusResumed:
		return said + " The executor was resumed with that answer."
	}
	return said
}

// errUnreadableSeatID is a pending run's seat id that is absent, nil or not a
// uuid.
var errUnreadableSeatID = errors.New("builtin: a pending run's seat id is unreadable")

// parties is the run's seat and requester off its row; a handle is immutable
// (ADR-0013), so the row names the same seats for as long as they exist.
//
// A row with no readable seat id is refused as a FAULT ([faulted]), never
// answered on a guess: it is this node's own row and stays unreadable, so
// "try again" would be a promise nothing keeps.
func (t *answerExecutor) parties(ctx context.Context, run sandbox.PendingRun) (runParties, *tools.Result) {
	parties, err := partiesOf(run)
	if err != nil {
		return runParties{}, refusalOf(faulted(ctx, AnswerExecutorTool, err, fmt.Sprintf(
			AnswerExecutorTool+" cannot answer the executor of turn %s: its record names no seat id "+
				"this build can read, so the seat it would wake cannot be named: %s.", clip(run.TurnID),
			faultSaid)))
	}
	return parties, nil
}

// partiesOf is [answerExecutor.parties] without the refusal: errUnreadableSeatID
// for an unreadable seat id.
func partiesOf(run sandbox.PendingRun) (runParties, error) {
	id, err := uuid.Parse(run.AgentID)
	if err != nil || id == uuid.Nil {
		return runParties{}, fmt.Errorf("pending run %s names seat id %q: %w", run.TurnID, run.AgentID,
			errUnreadableSeatID)
	}
	return runParties{seat: id, handle: run.AgentHandle, requester: run.Requester}, nil
}

// mayAnswer asks the authority about the object [runParties.answeredBy] names
// for the actor, whose handle for a bound person is their seat's
// ([iam.ActorFor]).
func (t *answerExecutor) mayAnswer(ctx context.Context, actor Actor,
	parties runParties) *tools.Result {

	return askAuthority(ctx, t.authorize, authz.ActionExecutorAnswer, parties.answeredBy(actor.Handle))
}

// answeredBy is the object answering the run asks about for the caller named
// handle: the requester's own seat when the caller is the requester (the rule's
// self arm), the run's seat otherwise. [runanswer.QuestionOf] states it, so the
// screens that offer the answer read the same object this decides on.
func (p runParties) answeredBy(handle string) authz.Object {
	return runanswer.QuestionOf(p.seat, p.handle, p.requester, handle).Object
}
