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
	"github.com/crewlet/crewlet/internal/worklet"
)

// AskAndWaitTool is the tool's wire name, declared in internal/toolname: no
// authority row names the run's own ending, so the walk that holds every tool
// name to one declaration reads it there ([toolname.AskAndWait]). The run
// record reads the same constant ([sandbox.AskAndWaitTool]): its call log's
// bound keeps the call by that name, and the bridge's nudge reads it there
// ([sandbox.EndsRun]).
const AskAndWaitTool = toolname.AskAndWait

// RunAskDesk is the run record a coding run asks through: the write that
// records its question onto the job's row ([sandbox.PendingStore.RecordAsk])
// and the read of that row ([sandbox.PendingStore.Get]) a submission is held to
// ([AskAndWait.Gate]), which the engine hands over through the sandbox runtime
// it holds now.
type RunAskDesk interface {
	RecordAsk(ctx context.Context, turnID, launch string, ask sandbox.Ask) (sandbox.AskOutcome, error)
	Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
}

// RunAskDeps are ask_and_wait's dependencies.
type RunAskDeps struct {
	// Desk records the ask. Required: [NewAskAndWait] builds no tool
	// without one.
	Desk RunAskDesk

	// Work and Pages check the evidence an ask cites, as a work item's
	// decision is checked ([worklet.ValidateDecision]): a task resolves
	// to its id and a page must exist. Either nil refuses evidence of its
	// kind, telling the run to cite it as a url, which is what a company
	// that runs neither Worklet nor Doclet has.
	Work  WorkDeps
	Pages PageReader
}

// NewAskAndWait is the tool a seat's executor run asks a person through and
// parks on, or nil without a desk.
//
// # Why a tool, and why only on the run's surface
//
// A coding run cannot wait on a person inside its CLI: a headless run goes on
// until it ends. So a question the work cannot go on without PARKS the run:
// the ask is recorded on its row, the run ends, a person answers, and the
// answer resumes the same run. Asking through a tool over the run's bridge
// means the ask is validated whole BEFORE anything is written, every fault
// named at once while the run can still fix it (ADR-0059), and the options a
// person chooses between are a structure rather than prose: a shell command's
// one free-text argument carried them as a paragraph, and the person answering
// got a wall of text with the options somewhere in it.
//
// It is offered on the executor's run surface alone, beside `submit_work`,
// because those are the two ways that run ends, and it means something only
// there: no other phase has a run to park. A question that only shapes what
// comes next is asked on the work item instead (`comment_on_work_item` with
// `ask`), and the run finishes as usual.
//
// # One shape for every ask a person answers
//
// The choice is a [decision.Decision], the shape a work item's decision has,
// read with the same schema ([choiceProperties]) and checked against the
// world the same way, so the person answering reads either in one layout and
// the run learns one structure (ADR-0068). A run's ask carries no `inform`:
// the answer resumes the run itself, and there is no answered turn of the
// seat's to hold to a promise.
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
		"progress and name the branch. When the person has to CHOOSE, give 2 to " +
		fmt.Sprintf("%d", decision.MaxOptions) + " `options`, the one you " +
		"`recommended` and your `rationale`, never options written into the " +
		"context. After it is recorded, END YOUR TURN at once: do not call " +
		"`" + toolname.SubmitWork + "`. Their answer arrives as your next prompt in this session, " +
		"or, if this machine is gone by then, a fresh run starts from your branch " +
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
		"required":   []any{"question", "to"},
	}
}

// askArgs is the arguments as the tool reads them. Unknown keys are refused
// by name ([readAsk]): a misspelled `recommendation` would otherwise park the
// run on an ask with no recommendation, reported as recorded.
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
		// NOT A RUN'S CALL: nothing names the job to park. The tool is
		// offered on an executor run's surface alone, so this is a
		// wiring fault, said as one rather than as the model's mistake.
		return refusedBy(tools.RefusalUnavailable, waitless(), AskAndWaitTool+
			" is unavailable here: it parks a coding run, and this call was not made by "+
			"one. Ask on the work item with `"+CommentOnWorkTool+"` and `ask` instead."), nil
	}
	ask, refusal := t.readAsk(ctx, args)
	if refusal != nil {
		return *refusal, nil
	}
	outcome, err := t.deps.Desk.RecordAsk(ctx, runID, launch, ask)
	switch {
	case err != nil:
		return nodeFailure(ctx, AskAndWaitTool, err, AskAndWaitTool+" could not record "+
			"your question", "Nothing was asked: call it again."), nil
	case outcome == sandbox.AskAlreadyAsked:
		// NOT RUNNING rather than a conflict: a conflict clears on a retry,
		// and this never does. The job is parked on the question it asked,
		// so it is not waiting for another.
		return refused(tools.RefusalNotRunning, "You already asked a question in this run, "+
			"and it stands: one answer resumes the run, so it asks one question at a "+
			"time. End your turn now; ask the next question once you have the answer."), nil
	case outcome != sandbox.AskRecorded:
		return refused(tools.RefusalNotRunning, "This run is no longer running, so "+
			"nothing can wait on a question here. End your turn."), nil
	}
	return tools.Result{Output: "Your question is recorded and has been put to a person. " +
		"END YOUR TURN NOW: stop working and do not call `" + toolname.SubmitWork + "`. Their " +
		"answer arrives as your next prompt in this session, or, if this machine is gone " +
		"by then, a fresh run starts from your branch with your question and their answer."}, nil
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
			"and for a choice options [{id, label, detail}], recommended, rationale, "+
			"evidence [{kind, ref, label}] and role. Nothing was asked.", AskAndWaitTool, err)))
	}
	ask := sandbox.Ask{Question: in.Question, Context: in.Context, To: in.To, Branch: in.Branch}
	var found []error
	if len(in.Options) > 0 {
		ask.Decision = &decision.Decision{Question: in.Question, Options: in.Options,
			Recommended: in.Recommended, Rationale: in.Rationale, Evidence: in.Evidence,
			Role: in.Role}
	} else {
		// THE CHOICE'S PARTS NEED ITS OPTIONS: a recommendation, a
		// rationale or a role on an open question would be dropped where
		// nothing draws them, and evidence goes in the context as links.
		for _, part := range []struct {
			field string
			set   bool
		}{
			{"recommended", in.Recommended != ""}, {"rationale", in.Rationale != ""},
			{"evidence", len(in.Evidence) > 0}, {"role", in.Role != ""},
		} {
			if part.set {
				found = append(found, decision.Refuse("%s is set and `options` is not: it "+
					"belongs to a choice; give the options, or link what you cite in "+
					"`context` for an open question", part.field))
			}
		}
	}
	if err = ask.ValidateOwn(); err != nil {
		found = append(found, err)
	}
	// THE DECISION'S SHAPE AND ITS EVIDENCE IN THE SAME PASS, named as the
	// arguments carry them (at their top, with no `decision.`), so one
	// refusal names every fault the run has to fix (ADR-0059, ADR-0068).
	var said []string
	if err = decision.Join(found); err != nil {
		said = append(said, err.Error())
	}
	if ask.Decision != nil {
		checked, err := worklet.ValidateDecisionAt(ctx, *ask.Decision, evidenceLookup{
			work: t.deps.Work, pages: t.deps.Pages,
		}, "")
		switch {
		case errors.Is(err, worklet.ErrInvalid):
			said = append(said, worklet.Sentence(err))
		case err != nil:
			return sandbox.Ask{}, refusalOf(readFailure(ctx, AskAndWaitTool, err))
		default:
			ask.Decision = &checked
		}
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
//
// # Why the bridge refuses a submission once the job has asked
//
// A job that asked a person is parked on their answer: its collection carries
// the run on with that answer rather than reporting it, and the work the run
// goes on to do is what its round submits. A `submit_work` made after the
// question is a run that did not stop when it was told to, and accepted, it
// read as success to the CLI that made it, which then ended believing its work
// was delivered while the turn waited on a person. Refused, the CLI is told
// what is true: the run is parked, and its next prompt is the answer.
//
// The collection never replays such a call either (the replay keeps only the
// calls made after the asking job, read off the row by the runner's resume): the gate is
// what the run is told, the cut is what the round reports, and either holds
// without the other.
//
// A READ OF THE ROW, before the submission is decoded, so a row that cannot be
// read refuses the call as the node's condition, and the run calls again. A
// call no run makes (no run id or job on its context) passes untouched: there
// is no question it could have asked.
func (t *AskAndWait) Gate(submit tools.Callable) tools.Callable {
	return &submitGate{submit: submit, desk: t.deps.Desk}
}

// submitGate is [AskAndWait.Gate]'s submission: a FORWARDER, reached only
// through [tools.Invoke] on the run's surface, which has checked the call
// against the submission's own schema before the gate decides it (the
// forwarders list in internal/tools holds it to that).
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
