package builtin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
	deps  RunDeps
	fleet Fleet
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
		return refused(tools.RefusalUnavailable, fmt.Sprintf("answer_run could not "+
			"read run %s right now (%v). This does NOT mean it is gone — try again.",
			clip(turnID), err)), nil
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
	if refusal := seatCanCarry(ctx, t.fleet, AnswerRunTool, run.AgentHandle,
		coord.FeatureAnswerRunByTurn); refusal != nil {
		return *refusal, nil
	}

	given := types.SandboxAnswerGiven{
		TurnID: run.TurnID, AgentHandle: run.AgentHandle, Answer: answer,
		AnsweredBy: actor.Handle, AnsweredBySeat: actor.Seat,
	}
	outcome := statelog.OutcomePending
	if err := t.deps.Desk.Deliver(ctx, given); err != nil {
		// THE PUBLISH MAY HAVE LANDED, so this is `unknown` rather than a
		// refusal — see [sandbox.AnswerDesk.Deliver]. Answering it again is
		// safe: whichever copy is consumed second finds the run no longer
		// waiting.
		log.WarnContext(ctx, "answer_run_delivery_unknown",
			"turn_id", run.TurnID, "seat", run.AgentHandle, "error", err.Error())
		outcome = statelog.OutcomeUnknown
	}
	return jsonResult(map[string]any{
		"turn_id": run.TurnID, "agent_handle": run.AgentHandle,
		"question": run.Question, "outcome": string(outcome),
	})
}
