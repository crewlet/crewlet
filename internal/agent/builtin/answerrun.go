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
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// AnswerRunTool is the tool's wire name.
const AnswerRunTool = "answer_run"

// MaxRunAnswerBytes bounds one answer to a parked coding run.
//
// HALF OF [ToolAnswerBytes], because the answer is not delivered alone: it is
// spliced into the resumed turn as the result of the suspended `run_sandbox`
// call, framed by the question it answers and the instructions to continue,
// and that whole message is one tool answer in the turn's context. The other
// half is the room the question and the framing take. A person's answer to a
// coding agent's question is a sentence or a paragraph; one this size is a
// document, and belongs on a page the run can be pointed at.
const MaxRunAnswerBytes = ToolAnswerBytes / 2

// RunDesk is what answer_run needs: the run an answer names, and the way to put
// the answer on the inbox of the seat that holds it. [sandbox.AnswerDesk] is
// the implementation.
type RunDesk interface {
	Run(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
	Deliver(ctx context.Context, given types.SandboxAnswerGiven) error
}

// RunDeps are answer_run's dependencies.
type RunDeps struct {
	// Desk reads the run and delivers the answer. Nil omits the tool.
	Desk RunDesk

	// Actor is who is answering, off the caller's own credential — the same
	// resolution every other operator write takes ([WorkDeps.Actor]).
	// Required with Desk: an answer nobody can be named for is refused.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
}

// The run's identity on its row, as a decision and the answer read it: the
// seat that holds it, by id and by its handle, and the seat whose
// wake started the turn that launched it.
type runParties struct {
	seat      uuid.UUID
	handle    string
	requester string
}

// answerRun answers a coding run that stopped to ask a person something, by
// naming the run.
//
// # Why by turn
//
// A reply on the conversation the question was asked in is matched to the run
// by that conversation — and a run launched by a schedule, a task assignment
// or a colleague's ask has no conversation at all. Its question could not be
// answered: there was nowhere to reply, and it waited out its pause TTL. This
// tool names the run instead, so any parked run is answerable whatever woke
// the turn that launched it.
//
// # Whose question it is
//
// Its REQUESTER's — the person whose message, notice or ask woke the turn that
// launched the run, recorded on its row by the handle their seat was created
// under — or whoever leads the run's seat, or the deployment's grant
// ([authz.ActionRunAnswer]). The requester is admitted through the rule's SELF
// arm, asked about their own seat, so it is the table that says so; and only
// when the caller IS that person, so nobody who merely leads them answers a
// question the run put to them.
//
// # What it promises, and what it does not
//
// It answers `pending`: the answer is on the seat's inbox and the node holding
// the seat will resume the run with it — but that node is the only one that
// can, and it may be paused, restarting or mid-upgrade. What the answer became
// is announced there as `sandbox_run_answered` (`resumed`, `not_awaiting` or
// `gone`). What it refuses up front is what this node can know: a run that is
// not waiting for an answer (`not_running`), and a fleet whose node holding the
// seat runs a build that cannot route the answer (`peer_upgrading`) — an older
// build would read it as an ordinary wake and run a turn about nothing while
// the run waited on.
type answerRun struct {
	deps      RunDeps
	fleet     Fleet
	authorize Authorizer
}

var _ tools.SeatCallable = (*answerRun)(nil)

func (t *answerRun) Name() string { return AnswerRunTool }

func (t *answerRun) Description() string {
	return "Answer a coding run that stopped to ask a person a question, by " +
		"naming the run's turn. The run resumes with your answer as the reply " +
		"to its question, on the node that holds its seat. Use this for any " +
		"parked run — including one started by a schedule, an assignment or a " +
		"colleague, which has no conversation to reply in. Find parked runs " +
		"and their questions on the sandbox-runs board."
}

func (t *answerRun) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_id": map[string]any{
				"type":        "string",
				"description": "The turn id of the parked run — its `turn_id` on the sandbox-runs board.",
			},
			"answer": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Your answer to the run's question, as the "+
					"coding agent should read it. At most %d KiB.", MaxRunAnswerBytes>>10),
			},
		},
		"required": []any{"turn_id", "answer"},
	}
}

func (t *answerRun) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *answerRun) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Desk == nil || t.deps.Actor == nil {
		return refused(tools.RefusalUnavailable, AnswerRunTool+" is unavailable: "+
			"this surface was built without the run record."), nil
	}
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return refused(tools.RefusalForbidden, AnswerRunTool+
			" answers as the person whose credential calls it, and this call carries none."), nil
	}
	turnID := strings.TrimSpace(argString(args, "turn_id"))
	if turnID == "" {
		return failed("answer_run needs a `turn_id` — the parked run's turn id."), nil
	}
	answer := strings.TrimSpace(argString(args, "answer"))
	switch {
	case answer == "":
		return failed("answer_run needs an `answer` — what the coding agent should be told."), nil
	case len(answer) > MaxRunAnswerBytes:
		return failed(fmt.Sprintf("That answer is %d KiB and one answer may be at most %d KiB: "+
			"it is spliced into the run as a single tool reply. Put the detail on a page "+
			"and answer with where it is.", len(answer)>>10, MaxRunAnswerBytes>>10)), nil
	}

	run, found, err := t.deps.Desk.Run(ctx, turnID)
	switch {
	case err != nil:
		return nodeFailure(ctx, AnswerRunTool, err, fmt.Sprintf("answer_run could not "+
			"read run %s", clip(turnID)), "This does NOT mean it is gone, and "+
			"nothing was answered."), nil
	case !found:
		// NOT_RUNNING RATHER THAN NOT_FOUND: a run that has ended has no
		// record, exactly like a turn id that never named one, and a
		// person holding the id of a run that finished is told the true
		// thing about both — nothing there is waiting for them.
		return refused(tools.RefusalNotRunning, fmt.Sprintf("No coding run is "+
			"parked under turn %s: it has finished or ended, or the id names no run.",
			clip(turnID))), nil
	case !slices.Contains(sandbox.Awaiting, run.Status):
		return refused(tools.RefusalNotRunning, fmt.Sprintf("Run %s is %s, not "+
			"waiting for an answer — there is no question for this to answer.",
			clip(turnID), run.Status)), nil
	}
	parties, refusal := t.parties(ctx, run)
	if refusal != nil {
		return *refusal, nil
	}
	if refusal := t.mayAnswer(ctx, actor, parties); refusal != nil {
		return *refusal, nil
	}
	if refusal := seatCanCarry(ctx, t.fleet, AnswerRunTool, parties.seat, parties.handle,
		coord.FeatureAnswerRunByTurn); refusal != nil {
		return *refusal, nil
	}

	// WHO ANSWERED, as [iam.ActorFor] names them: a person bound to a seat
	// answers AS that seat, kind human, with the credential they acted
	// through beside it. The desk reads the run's row again to stamp the
	// seat it delivers to, so a seat named here is only ever the same one.
	given := types.SandboxAnswerGiven{
		TurnID: run.TurnID, Agent: parties.seat.String(), AgentHandle: parties.handle,
		Answer: answer, AnsweredBy: actor.Handle, AnsweredByKind: string(actor.Kind),
		OperatorID: actor.OperatorID,
	}
	outcome := statelog.OutcomePending
	if err := t.deps.Desk.Deliver(ctx, given); err != nil {
		// THE PUBLISH MAY HAVE LANDED, so this is `unknown` rather than a
		// refusal — see [sandbox.AnswerDesk.Deliver]. Answering it again is
		// safe: whichever copy is consumed second finds the run no longer
		// waiting.
		log.WarnContext(ctx, "answer_run_delivery_unknown",
			"turn_id", run.TurnID, "seat", parties.handle, "error", err.Error())
		outcome = statelog.OutcomeUnknown
	}
	return jsonResult(map[string]any{
		"turn_id": run.TurnID, "agent_handle": parties.handle,
		"question": run.Question, "outcome": string(outcome),
	})
}

// errUnreadableSeatID is the error a pending run's row carries when the seat
// id on it is not one: absent, the nil id, or not a uuid at all.
var errUnreadableSeatID = errors.New("builtin: a pending run's seat id is unreadable")

// parties is the run's seat and requester as its row records them: the seat by
// its id and its handle, the requester by handle. A handle is immutable
// (ADR-0013), so the row names the same seats for as long as they exist.
//
// A row with no seat id this build can read names no seat a gate can ask
// about, and is refused rather than answered on a guess — as a FAULT
// ([faulted]): the row is this node's own and stays unreadable however long
// anybody waits, so "try again" would be a promise nothing keeps.
func (t *answerRun) parties(ctx context.Context, run sandbox.PendingRun) (runParties, *tools.Result) {
	id, err := uuid.Parse(run.AgentID)
	if err != nil || id == uuid.Nil {
		return runParties{}, refusalOf(faulted(ctx, AnswerRunTool,
			fmt.Errorf("pending run %s names seat id %q: %w", run.TurnID, run.AgentID,
				errUnreadableSeatID), fmt.Sprintf("answer_run cannot answer run %s: its "+
				"record names no seat id this build can read, so the seat it would wake "+
				"cannot be named — %s.", clip(run.TurnID), faultSaid)))
	}
	return runParties{seat: id, handle: run.AgentHandle, requester: run.Requester}, nil
}

// mayAnswer asks the authority about this run: about the REQUESTER's own seat
// when the caller is that requester — the rule's self arm — and about the
// run's seat otherwise, where its lead and the deployment's grant are admitted.
// The actor is named by [iam.ActorFor], whose name for a bound person is their
// seat's handle.
func (t *answerRun) mayAnswer(ctx context.Context, actor Actor,
	parties runParties) *tools.Result {

	owner := parties.handle
	if parties.requester != "" && actor.Handle == parties.requester {
		owner = parties.requester
	}
	return askAuthority(ctx, t.authorize, authz.ActionRunAnswer,
		authz.Object{Kind: authz.KindPerson, ID: parties.seat.String(), Owner: owner})
}
