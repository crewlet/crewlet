package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AskAndWaitTool is the tool's wire name, declared at [toolname.AskAndWait]:
// no authority row names the run's own ending, so the walk holding every tool
// name to one declaration reads it there. The run record's call-log bound
// ([sandbox.AskAndWaitTool]) and the bridge's nudge ([sandbox.EndsRun]) read
// the same constant.
const AskAndWaitTool = toolname.AskAndWait

// RunAskDesk is the run record an executor asks through: the write recording
// its question on the job's row ([sandbox.PendingStore.RecordAsk]) and the
// read of that row ([sandbox.PendingStore.Get]) a submission is held to
// ([AskAndWait.Gate]), handed over through the engine's sandbox runtime.
type RunAskDesk interface {
	RecordAsk(ctx context.Context, turnID, launch string, ask sandbox.Ask) (sandbox.AskOutcome, error)
	Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
}

// RunAskDeps are ask_and_wait's dependencies.
type RunAskDeps struct {
	// Desk records the ask. Required: [NewAskAndWait] builds no tool
	// without one.
	Desk RunAskDesk

	// Work and Pages check an ask's evidence as a work item's decision is
	// checked ([worklet.ValidateDecision]): a task resolves to its id, a page
	// must exist. Either nil refuses evidence of its kind and tells the run to
	// cite a url: a company without Worklet or Doclet.
	Work  WorkDeps
	Pages PageReader
}

// NewAskAndWait is the tool a seat's executor run asks a person through and
// parks on, or nil without a desk. A headless run cannot wait inside its
// harness, so the ask is recorded on the job's row, the launch ends, and the
// answer resumes the same executor. A tool rather than a shell argument, so
// the ask is validated whole before anything is written (ADR-0059) and its
// options are a structure, not a paragraph. Offered only on the run's
// surface, beside `submit_work`: no other phase has a run to park; a
// question that only shapes what comes next goes on the work item. The
// choice is a [decision.Decision], read with [choiceProperties] and
// checked as a work item's is (ADR-0068), always with a recommended option
// (ADR-0100), since a person can still answer in words. No `inform`: the
// answer resumes the run itself.
func NewAskAndWait(deps RunAskDeps) *AskAndWait {
	if deps.Desk == nil {
		return nil
	}
	return &AskAndWait{deps: deps}
}

// AskAndWait is the `ask_and_wait` tool ([NewAskAndWait]).
type AskAndWait struct{ deps RunAskDeps }

var _ tools.SeatCallable = (*AskAndWait)(nil)

// Name is the tool's wire name, [AskAndWaitTool].
func (t *AskAndWait) Name() string { return AskAndWaitTool }

// Description is what the run is told the tool is for and when to use it.
func (t *AskAndWait) Description() string {
	return "Ask a person a question you cannot go on without, and park this run " +
		"until they answer. Use it only when the work cannot proceed without a " +
		"person's decision or information; a question that only shapes what comes " +
		"next goes on the work item with `" + CommentOnWorkTool + "` and `ask`, and " +
		"you finish as usual. Before you call it, commit and push your work in " +
		"progress and name the branch. Every question offers " +
		fmt.Sprintf("%d to %d", decision.MinOptions, decision.MaxOptions) + " `options` " +
		"the person answers with one press, the one you `recommended` and your " +
		"`rationale`, never options written into the context: a question you would " +
		"ask in words still has answers you expect, and the person can always answer " +
		"in their own words instead. After it is recorded, END YOUR TURN at once: do not call " +
		"`" + toolname.SubmitWork + "`. Their answer arrives as your next prompt in this session, " +
		"on this machine or, if it is gone by then, on a fresh one from your branch; only " +
		"where this session cannot be continued does a fresh run start from your branch " +
		"with your question and their answer."
}

// Parameters is the ask's schema: the question, its context and the choice.
func (t *AskAndWait) Parameters() map[string]any {
	properties := choiceProperties()
	properties["question"] = map[string]any{
		"type": "string",
		"description": fmt.Sprintf("What you need answered, in one specific, "+
			"self-contained sentence of at most %d bytes: the title the person "+
			"reads first. Everything else goes in `context`.", decision.MaxQuestion),
	}
	properties["context"] = map[string]any{
		"type": "string",
		"description": fmt.Sprintf("What the person needs to answer well, in "+
			"Markdown: what you found, what is at stake, where your work stands. "+
			"Link what you cite. At most %d KiB. Never list the options here: "+
			"give them in `options`.", sandbox.MaxAskContextBytes>>10),
	}
	properties["to"] = map[string]any{
		"type": "string",
		"description": fmt.Sprintf("Who should answer: `%s` (the person who "+
			"asked for this work, for a clarification of what they asked), `%s` "+
			"(a design or technical decision), `%s`, or a teammate's handle as "+
			"your standing instructions list them.", sandbox.AskToRequester,
			sandbox.AskToTeam, sandbox.AskToManager),
	}
	properties["branch"] = map[string]any{
		"type": "string",
		"description": "The branch you pushed your work in progress to, exactly " +
			"as you pushed it. Leave it out only if you pushed nothing.",
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []any{"question", "to", "options", "recommended", "rationale", "role"},
	}
}

// askArgs is the arguments as the tool reads them. Unknown keys are refused
// by name ([readAsk]): a misspelled `recommendation` would otherwise read as
// an ask with no recommendation, and be refused for the wrong reason.
type askArgs struct {
	Question    string              `json:"question"`
	Context     string              `json:"context"`
	To          string              `json:"to"`
	Branch      string              `json:"branch"`
	Options     []decision.Option   `json:"options"`
	Recommended string              `json:"recommended"`
	Rationale   string              `json:"rationale"`
	Evidence    []decision.Evidence `json:"evidence"`
	Role        decision.Role       `json:"role"`
}

// Call is [AskAndWait.CallForTurn] with no turn, which no run makes.
func (t *AskAndWait) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// CallForTurn records the ask a run's job makes over its bridge, or refuses
// it with every fault at once.
func (t *AskAndWait) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	runID, launch := "", turnctx.JobOf(ctx)
	if turn != nil {
		runID = turn.RunID
	}
	if runID == "" || launch == "" {
		// NOT AN EXECUTOR'S CALL: nothing names the launch to park. The tool is
		// on the executor's surface alone, so this is a wiring fault, said as one.
		return refusedBy(tools.RefusalUnavailable, waitless(), AskAndWaitTool+
			" is unavailable here: it parks a seat's executor in its box, and this call "+
			"was not made by one. Ask on the work item with `"+CommentOnWorkTool+"` and "+
			"`ask` instead."), nil
	}
	ask, refusal := t.readAsk(ctx, args)
	if refusal != nil {
		return *refusal, nil
	}
	outcome, err := t.deps.Desk.RecordAsk(ctx, runID, launch, ask)
	onCall := ""
	if err == nil && outcome == sandbox.AskAlreadyAsked {
		onCall = t.parkedOnCall(ctx, runID, launch)
	}
	switch {
	case err != nil:
		return nodeFailure(ctx, AskAndWaitTool, err, AskAndWaitTool+" could not record "+
			"your question", "Nothing was asked: call it again."), nil
	case onCall != "":
		// PARKED ON ITS OWN CALLS, which it never asked about: said as that.
		return refused(tools.RefusalNotRunning, onCall+" still running, so this run is parked on it and "+
			"asks nothing now. END YOUR TURN NOW: you will be resumed with its result, and can ask then if "+
			"you still need to."), nil
	case outcome == sandbox.AskAlreadyAsked:
		// NOT RUNNING, not a conflict: a conflict clears on a retry, and this
		// never does, since the job stays parked on the question it asked.
		return refused(tools.RefusalNotRunning, "You already asked a question in this run, "+
			"and it stands: one answer resumes the run, so it asks one question at a "+
			"time. End your turn now; ask the next question once you have the answer."), nil
	case outcome != sandbox.AskRecorded:
		return refused(tools.RefusalNotRunning, "This run is no longer running, so "+
			"nothing can wait on a question here. End your turn."), nil
	}
	return tools.Result{Output: "Your question is recorded and has been put to a person. " +
		"END YOUR TURN NOW: stop working and do not call `" + toolname.SubmitWork + "`. Their " +
		"answer arrives as your next prompt in this session, on this machine or, if it is " +
		"gone by then, on a fresh one from your branch; only where this session cannot be " +
		"continued does a fresh run start from your branch with your question and their answer."}, nil
}

// parkedOnCall names the calls job of run runID is parked on alone
// (internal/sandbox's CallWait), "Your call to X is" ([callsParked]), "" where
// it asked anything else or its row cannot be read.
func (t *AskAndWait) parkedOnCall(ctx context.Context, runID, job string) string {
	run, found, err := t.deps.Desk.Get(ctx, runID)
	if err != nil || !found || !run.AskedBy(job) || !run.WaitsOnOwnCalls() {
		return ""
	}
	return callsParked(run)
}

// callsParked names the calls run's job parked on, as the subject of a
// sentence: "Your call to X is", or "Your calls to X and Y are".
func callsParked(run sandbox.PendingRun) string {
	called := make([]words.Said, len(run.ParkedCalls))
	for i, c := range run.ParkedCalls {
		called[i] = words.Said("`" + c.Tool + "`")
	}
	if len(called) == 1 {
		return "Your call to " + string(called[0]) + " is"
	}
	return "Your calls to " + words.And(called) + " are"
}

// readAsk reads and checks the arguments into the ask to record, or returns
// the refusal: every fault at once (ADR-0059), the world's checks of the
// evidence after the shape's.
func (t *AskAndWait) readAsk(ctx context.Context, args map[string]any) (sandbox.Ask, *tools.Result) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return sandbox.Ask{}, refusalOf(failed(fmt.Sprintf("%s: the arguments are not "+
			"a JSON object (%v). Nothing was asked.", AskAndWaitTool, err)))
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var in askArgs
	if err = decoder.Decode(&in); err != nil {
		return sandbox.Ask{}, refusalOf(failed(fmt.Sprintf("%s: the arguments do not have "+
			"the shape its schema describes (%v). It takes question, context, to, branch, "+
			"options [{id, label, detail}], recommended, rationale, evidence [{kind, ref, "+
			"label}] and role. Nothing was asked.", AskAndWaitTool, err)))
	}
	// EVERY QUESTION IS A CHOICE (ADR-0100): the decision is built even with
	// no options, so their absence is refused in the decision's own words.
	ask := sandbox.Ask{Question: in.Question, Context: in.Context, To: in.To, Branch: in.Branch,
		Decision: &decision.Decision{Question: in.Question, Options: in.Options,
			Recommended: in.Recommended, Rationale: in.Rationale, Evidence: in.Evidence,
			Role: in.Role}}
	// THE ASK'S FIELDS, THE DECISION'S SHAPE AND ITS EVIDENCE IN ONE PASS,
	// named as the arguments carry them (no `decision.` prefix), so one
	// refusal names every fault (ADR-0059, ADR-0068).
	var said []string
	if err = ask.ValidateOwn(); err != nil {
		said = append(said, err.Error())
	}
	// A RUN IS AN AGENT'S, so its decision recommends one option and says why.
	checked, err := worklet.ValidateDecisionAt(ctx, *ask.Decision, worklet.AuthorAgent,
		evidenceLookup{work: t.deps.Work, pages: t.deps.Pages}, "")
	switch {
	case errors.Is(err, worklet.ErrInvalid):
		said = append(said, worklet.Sentence(err))
	case err != nil:
		return sandbox.Ask{}, refusalOf(readFailure(ctx, AskAndWaitTool, err))
	default:
		ask.Decision = &checked
	}
	if len(said) > 0 {
		refusal := decision.Refuse("%s", strings.Join(said, "; "))
		return sandbox.Ask{}, refusalOf(refusedBy(tools.RefusalInvalid, refusal, fmt.Sprintf(
			"%s was refused: %s. Nothing was asked.", AskAndWaitTool, refusal.Error())))
	}
	return ask, nil
}

// Gate is submit as an executor run's surface offers it beside this tool:
// refused to a job whose question is on its row ([sandbox.PendingRun.AskedBy]).
// Such a job is parked on the answer, and its round submits the work done
// after it; accepted, a `submit_work` made after the question would tell the
// harness its work was delivered while the turn waits on a person. The
// collection also never replays such a call (the runner's resume keeps only
// calls after the asking job): the gate is what the run is told, the cut what
// the round reports, and either holds without the other.
//
// The row is read before the submission is decoded, so an unreadable row
// refuses the call as the node's condition and the run calls again. A call
// with no run id or job on its context passes: it could have asked nothing.
func (t *AskAndWait) Gate(submit tools.Callable) tools.Callable {
	return &submitGate{submit: submit, desk: t.deps.Desk}
}

// submitGate is [AskAndWait.Gate]'s submission: a FORWARDER, reached only
// through [tools.Invoke], which checks the call against the submission's
// schema before the gate decides it (internal/tools' forwarders list holds
// it to that).
type submitGate struct {
	submit tools.Callable
	desk   RunAskDesk
}

var (
	_ tools.SeatCallable = (*submitGate)(nil)
	_ tools.Wrapper      = (*submitGate)(nil)
)

func (g *submitGate) Name() string               { return g.submit.Name() }
func (g *submitGate) Description() string        { return g.submit.Description() }
func (g *submitGate) Parameters() map[string]any { return g.submit.Parameters() }
func (g *submitGate) Unwrap() tools.Callable     { return g.submit }

func (g *submitGate) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return g.CallForTurn(ctx, nil, args)
}

func (g *submitGate) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if turn != nil && turn.RunID != "" {
		if launch := turnctx.JobOf(ctx); launch != "" {
			run, found, err := g.desk.Get(ctx, turn.RunID)
			switch {
			case err != nil:
				return nodeFailure(ctx, g.submit.Name(), err, g.submit.Name()+" could not "+
					"read whether this run is waiting on a person", "Nothing was submitted: "+
					"call it again."), nil
			case found && run.AskedBy(launch) && run.HeldCall != nil:
				// A CALL HELD FOR APPROVAL parks the job as its own question
				// does (ADR-0077), and is said as the call it is.
				return refused(tools.RefusalNotRunning, "Your call to "+run.HeldCall.Tool+
					" waits for a person's approval, so this run is parked on their answer and "+
					"takes no submission. END YOUR TURN NOW: their answer, and what the call "+
					"returned, arrive as your next prompt, and you submit your work once you "+
					"have finished it."), nil
			case found && run.AskedBy(launch) && run.WaitsOnOwnCalls():
				// A CALL ANSWERED STILL RUNNING before its harness gave it up
				// parks the job on it (internal/sandbox's CallWait).
				return refused(tools.RefusalNotRunning, callsParked(run)+" still running, so this run is "+
					"parked on it and takes no submission. END YOUR TURN NOW: you will be resumed with its "+
					"result, and you submit your work once you have finished it."), nil
			case found && run.AskedBy(launch) && run.WaitsFor != nil:
				// A WAIT ON ANOTHER RUN parks the job as a question does
				// (ADR-0176), and is said as the wait it is.
				return refused(tools.RefusalNotRunning, "You are waiting for run "+run.WaitsFor.Turn+
					" to leave its seat, so this run is parked and takes no submission. END YOUR TURN "+
					"NOW: how that run went arrives as your next prompt, and you submit your work once "+
					"you have finished it."), nil
			case found && run.AskedBy(launch):
				return refused(tools.RefusalNotRunning, "You asked a person a question in "+
					"this run, so it is parked on their answer and takes no submission. "+
					"END YOUR TURN NOW: their answer arrives as your next prompt, and you "+
					"submit your work once you have finished it."), nil
			}
		}
	}
	if seat, ok := g.submit.(tools.SeatCallable); ok {
		return seat.CallForTurn(ctx, turn, args)
	}
	return g.submit.Call(ctx, args)
}
