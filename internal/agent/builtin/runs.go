package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// The tools an executor's run sees the runs around it with (ADR-0176):
// `list_runs`, `message_run` and `wait_for_run`, offered on the run's surface
// of its own beside `ask_and_wait` while other runs are in flight around it.
// Each reads and writes through the engine's [RunsDesk], which decides every
// refusal; the tools word the call and its answer.

// The tools' wire names, each declared once in internal/toolname.
const (
	ListRunsTool   = toolname.ListRuns
	MessageRunTool = toolname.MessageRun
	WaitForRunTool = toolname.WaitForRun
)

// RunsDesk is the engine's view of the runs around an executor's run
// (internal/engine's runsDesk). Declared here, by the consumer.
type RunsDesk interface {
	// Around is every other run in flight around turnID's run, in the order
	// they started. An error is a store that could not say.
	Around(ctx context.Context, turnID string) ([]prompts.RunAround, error)

	// Message files text from turnID's job on target's run, read at its next
	// step. A [*RunRefused] is the engine's refusal; any other error a filing
	// that may not have landed.
	Message(ctx context.Context, turnID, job, target, text string) error

	// Wait records turnID's job parking until target leaves its seat's slots,
	// with the run's reason and the branch it pushed. A [*RunRefused] is the
	// engine's refusal; any other error a record that may not have landed.
	Wait(ctx context.Context, turnID, job, target, reason, branch string) error
}

// RunRefusal is why the engine refused a call naming another run.
type RunRefusal string

const (
	// RunRefusedSelf is a run named that is the caller's own.
	RunRefusedSelf RunRefusal = "self"
	// RunRefusedUnknown is a turn no run in flight around the caller has.
	RunRefusedUnknown RunRefusal = "unknown_run"
	// RunRefusedRequester is a run working for another requester, or for one
	// nobody can tell, so no words pass between the two (ADR-0109).
	RunRefusedRequester RunRefusal = "requester"
	// RunRefusedNotWorking is a run not working now: a message reaches only
	// a run launching or running, a wait only a run holding its seat.
	RunRefusedNotWorking RunRefusal = "not_working"
	// RunRefusedHarness is a run whose harness reads no message between its steps.
	RunRefusedHarness RunRefusal = "harness"
	// RunRefusedBound is a bound on messages, sent or taken, that is spent.
	RunRefusedBound RunRefusal = "bound"
	// RunRefusedCycle is a run that waits, directly or through others, on the caller.
	RunRefusedCycle RunRefusal = "cycle"
	// RunRefusedCallerDone is a caller whose own job is no longer running, or has
	// asked or waited already.
	RunRefusedCallerDone RunRefusal = "caller_done"
	// RunRefusedInvalid is an argument the engine checks against the record (a
	// wait's branch or reason) that is not one it takes.
	RunRefusedInvalid RunRefusal = "invalid"
	// RunRefusedUnreachable is a run of another seat, which the chart does
	// not let the caller's seat reach as a colleague's ask does.
	RunRefusedUnreachable RunRefusal = "unreachable"
)

// RunRefusals is the closed set.
var RunRefusals = []RunRefusal{RunRefusedSelf, RunRefusedUnknown, RunRefusedRequester, RunRefusedNotWorking,
	RunRefusedHarness, RunRefusedBound, RunRefusedCycle, RunRefusedCallerDone, RunRefusedInvalid,
	RunRefusedUnreachable}

// Valid reports whether r is one of [RunRefusals].
func (r RunRefusal) Valid() bool {
	for _, known := range RunRefusals {
		if r == known {
			return true
		}
	}
	return false
}

// class is the refusal class a model and a person's surface read r as.
func (r RunRefusal) class() tools.Refusal {
	switch r {
	case RunRefusedRequester, RunRefusedUnreachable:
		return tools.RefusalForbidden
	case RunRefusedNotWorking, RunRefusedHarness, RunRefusedCallerDone:
		return tools.RefusalNotRunning
	case RunRefusedBound:
		return tools.RefusalNotesSpent
	case RunRefusedCycle:
		return tools.RefusalConflict
	}
	return tools.RefusalInvalid
}

// RunRefused is a call naming another run that the engine refused: why, and
// the sentence the run is told, which names the bound or the rule.
type RunRefused struct {
	Reason RunRefusal
	Said   string
}

func (e *RunRefused) Error() string { return "builtin: refused (" + string(e.Reason) + "): " + e.Said }

// NewRunTools is the three tools over desk, nil without one.
func NewRunTools(desk RunsDesk) []tools.SeatCallable {
	if desk == nil {
		return nil
	}
	return []tools.SeatCallable{&listRuns{desk: desk}, &messageRun{desk: desk}, &waitForRun{desk: desk}}
}

// runCall is the run and job a call is made in, and the refusal of one made
// outside an executor's run, which no surface but the run's offers.
func runCall(ctx context.Context, turn *turnctx.Turn, tool string) (run, job string, refusal *tools.Result) {
	if turn != nil {
		run = turn.RunID
	}
	job = turnctx.JobOf(ctx)
	if run == "" || job == "" {
		r := refusedBy(tools.RefusalUnavailable, waitless(), tool+" is unavailable here: it acts for a "+
			"seat's executor in its box, and this call was not made by one.")
		return "", "", &r
	}
	return run, job, nil
}

// runRefusal is err as the model is told it: the engine's refusal in its own
// words, else the node's failure to decide.
func runRefusal(ctx context.Context, tool string, err error, stopped string) tools.Result {
	if refused, ok := errors.AsType[*RunRefused](err); ok {
		return refusedBy(refused.Reason.class(), err, refused.Said)
	}
	return nodeFailure(ctx, tool, err, tool+" could not reach the runs around this one", stopped)
}

// turnArg is the turn id argument, or its refusal.
func turnArg(args map[string]any, tool string) (string, *tools.Result) {
	turn := strings.TrimSpace(argString(args, "turn_id"))
	if _, err := uuid.Parse(turn); err != nil {
		r := failedArg("turn_id", tool+" needs `turn_id`, a run's turn id as `"+ListRunsTool+"` shows it.")
		return "", &r
	}
	return turn, nil
}

// ---- list_runs --------------------------------------------------------- //

// listRuns is `list_runs`: the runs in flight around the caller's.
type listRuns struct{ desk RunsDesk }

var _ tools.SeatCallable = (*listRuns)(nil)

func (t *listRuns) Name() string { return ListRunsTool }

func (t *listRuns) Description() string {
	return "List the other runs in flight around this one, your seat's and other seats' on the same work item, " +
		"in the same conversation, on a work item linked to yours, on a work item or conversation you filed or " +
		"wrote, or that filed or wrote yours: each one's turn id, seat and how it " +
		"relates to your work (`relation`), where it stands (launching, running, parked on a question or a held call, " +
		"waiting for another run), its work item, and, for a " +
		"run working for the same requester as you, its conversation, the branch it named and its task's " +
		"title. A run for another requester shows its stage and item only. Use it to tell whether a run " +
		"works on the same item, branch or conversation as you before you duplicate or collide with its " +
		"work; then `" + MessageRunTool + "` or `" + WaitForRunTool + "`."
}

func (t *listRuns) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *listRuns) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// listedRun is one run as `list_runs` answers it.
type listedRun struct {
	Turn             string `json:"turn_id"`
	Seat             string `json:"seat"`
	ThisSeat         bool   `json:"this_seat"`
	Relation         string `json:"relation"`
	Stage            string `json:"stage"`
	WorkItem         string `json:"work_item"`
	SameRequester    bool   `json:"same_requester"`
	Conversation     string `json:"conversation"`
	ThisConversation bool   `json:"this_conversation"`
	Branch           string `json:"branch"`
	Started          string `json:"started"`
	Title            string `json:"title" untrusted:"fence"`
	TakesMessages    bool   `json:"takes_messages"`
}

// listedRuns is `list_runs`' answer.
type listedRuns struct {
	Runs []listedRun `json:"runs"`
}

func (t *listRuns) CallForTurn(ctx context.Context, turn *turnctx.Turn, _ map[string]any) (tools.Result, error) {
	// A READ NEEDS THE RUN ALONE, never the job that asks.
	if turn == nil || turn.RunID == "" {
		return refusedBy(tools.RefusalUnavailable, waitless(), ListRunsTool+" is unavailable here: it reads "+
			"the runs around a seat's executor run, and this call was not made by one."), nil
	}
	runID := turn.RunID
	around, err := t.desk.Around(ctx, runID)
	if err != nil {
		return readFailure(ctx, ListRunsTool, err), nil
	}
	self := ""
	if turn.Seat != nil {
		self = turn.Seat.Handle()
	}
	read := newForeignRead(turn, self)
	out := listedRuns{Runs: make([]listedRun, 0, len(around))}
	for i, r := range around {
		listed := listedRun{Turn: r.Turn, Seat: r.Seat, ThisSeat: r.OwnSeat, Relation: string(r.Relation),
			Stage: r.Stage, WorkItem: r.Item, SameRequester: r.SameRequester, TakesMessages: r.TakesMessages}
		if r.SameRequester {
			listed.Conversation, listed.ThisConversation, listed.Branch = r.Conversation, r.SameConversation, r.Branch
			if !r.Started.IsZero() {
				listed.Started = r.Started.UTC().Format(time.RFC3339)
			}
			listed.Title = r.Title
			read.fenceLabelled(fieldPath{}.key("runs").index(i).key("title"), r.Label, r.Title)
		}
		out.Runs = append(out.Runs, listed)
	}
	return read.answer(ctx, ListRunsTool, out, "")
}

// ---- message_run ------------------------------------------------------- //

// messageRun is `message_run`: a message to another run, read at its next step.
type messageRun struct{ desk RunsDesk }

var _ tools.SeatCallable = (*messageRun)(nil)

func (t *messageRun) Name() string { return MessageRunTool }

func (t *messageRun) Description() string {
	return fmt.Sprintf("Send another run in flight a short message, which it reads at its next step as "+
		"information from you, never as a request it works for: what you are doing that overlaps its work, "+
		"what you found, or an answer to a message it sent you. Only a run working for the same requester as "+
		"you takes one, only while it is working, and another seat's only where your seat may ask that seat as "+
		"a colleague. At most %d characters, and %d messages a launch sent "+
		"and %d taken: say it in one go. It does not wait for an answer; one arrives as a message at your "+
		"next step.", steer.MaxWakeRunes, sandbox.MaxJobRunSends, sandbox.MaxJobRunMessages)
}

func (t *messageRun) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_id": map[string]any{"type": "string",
				"description": "The run to message, by its turn id as `" + ListRunsTool + "` shows it."},
			"text": map[string]any{"type": "string",
				"description": fmt.Sprintf("Your message, at most %d characters.", steer.MaxWakeRunes)},
		},
		"required": []any{"turn_id", "text"},
	}
}

func (t *messageRun) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *messageRun) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	runID, job, refusal := runCall(ctx, turn, MessageRunTool)
	if refusal != nil {
		return *refusal, nil
	}
	target, refusal := turnArg(args, MessageRunTool)
	if refusal != nil {
		return *refusal, nil
	}
	text := strings.TrimSpace(argString(args, "text"))
	switch n := utf8.RuneCountInString(text); {
	case text == "" || !utf8.ValidString(text):
		return failedArg("text", MessageRunTool+" needs `text`, the message, in UTF-8."), nil
	case n > steer.MaxWakeRunes:
		// NEVER CUT: the run reads all of what is sent or nothing.
		return failedArg("text", fmt.Sprintf("%s: `text` is %d characters, past the %d one message to a "+
			"run carries. Nothing was sent: say it in fewer words.", MessageRunTool, n, steer.MaxWakeRunes)), nil
	}
	if err := t.desk.Message(ctx, runID, job, target, text); err != nil {
		return runRefusal(ctx, MessageRunTool, err, "Nothing may have been sent: call it again, and the run "+
			"reads one message however often it was sent."), nil
	}
	return tools.Result{Output: "Your message is filed for run " + target + ": it reads it at its next step, as " +
		"information from you. Carry on with your own work; an answer, if it sends one, reaches you the same " +
		"way."}, nil
}

// ---- wait_for_run ------------------------------------------------------ //

// waitForRun is `wait_for_run`: the caller's run parks until another leaves
// its seat's slots, as `ask_and_wait` parks on a question.
type waitForRun struct{ desk RunsDesk }

var _ tools.SeatCallable = (*waitForRun)(nil)

func (t *waitForRun) Name() string { return WaitForRunTool }

func (t *waitForRun) Description() string {
	return "Park this run until another run in flight leaves its seat (it finishes, fails, or stops to wait " +
		"on a person), when its work and yours would collide: the same branch, the same work item, or work " +
		"that has to come after it. Before you call it, commit and push your work in progress and name the " +
		"branch. After it is recorded, END YOUR TURN at once: do not call `" + toolname.SubmitWork + "`. How " +
		"the other run went (how it ended, what it pushed and, for a run working for the same requester, what " +
		"its reviewer said landed) arrives as your next prompt. Another seat's run is waited for only where it " +
		"works for the same requester and your seat may ask that seat as a colleague. A wait that would close " +
		"a cycle of runs waiting on each other is refused."
}

func (t *waitForRun) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_id": map[string]any{"type": "string",
				"description": "The run to wait for, by its turn id as `" + ListRunsTool + "` shows it."},
			"reason": map[string]any{"type": "string",
				"description": fmt.Sprintf("Why this run waits for it, in one sentence of at most %d bytes: "+
					"what of its work yours has to come after.", sandbox.MaxRunWaitReasonBytes)},
			"branch": map[string]any{"type": "string",
				"description": "The branch you pushed your work in progress to, exactly as you pushed it. " +
					"Leave it out only if you pushed nothing."},
		},
		"required": []any{"turn_id", "reason"},
	}
}

func (t *waitForRun) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *waitForRun) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	runID, job, refusal := runCall(ctx, turn, WaitForRunTool)
	if refusal != nil {
		return *refusal, nil
	}
	target, refusal := turnArg(args, WaitForRunTool)
	if refusal != nil {
		return *refusal, nil
	}
	reason := strings.TrimSpace(argString(args, "reason"))
	branch := strings.TrimSpace(argString(args, "branch"))
	if reason == "" || len(reason) > sandbox.MaxRunWaitReasonBytes {
		return failedArg("reason", fmt.Sprintf("%s needs `reason`, one sentence of at most %d bytes on why "+
			"this run waits.", WaitForRunTool, sandbox.MaxRunWaitReasonBytes)), nil
	}
	if err := t.desk.Wait(ctx, runID, job, target, reason, branch); err != nil {
		return runRefusal(ctx, WaitForRunTool, err, "Nothing may have been recorded: call it again."), nil
	}
	return tools.Result{Output: "Your wait for run " + target + " is recorded. END YOUR TURN NOW: stop working " +
		"and do not call `" + toolname.SubmitWork + "`. How that run went arrives as your next prompt in this " +
		"session once it leaves its seat, on this machine or, if it is gone by then, on a fresh one from your " +
		"branch."}, nil
}
