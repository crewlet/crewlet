package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// A2AAskTool is the tool's wire name.
const A2AAskTool = toolname.A2AAsk

// Asker is what this tool needs from the A2A service: an interface, so the
// tool is tested without a broker and builtin never learns how a channel opens.
type Asker interface {
	Open(ctx context.Context, ask a2a.Ask) (string, error)
}

// a2aAsk asks one colleague one question.
//
// ONE ASK, ONE ANSWER, THEN CLOSED: two agents that can keep replying to each
// other will, leaving no bound on what one trigger costs. The ask charges
// delegation depth; the reply does not.
//
// It does NOT wait for the answer: the target answers in its own turn, on
// whichever node owns its seat, so blocking would hold a seat open across a
// network hop for minutes. The answer is a later wake on this seat's inbox.
type a2aAsk struct {
	svc Asker
	// env resolves the chart's `${VAR}` contacts ([Deps.Env]).
	env org.EnvLookup
	// requesters composes who the asking turn works for where no gate
	// bound the call's ceiling ([ceilingOf], [Deps.Requesters]).
	requesters Requesters
}

var _ tools.SeatCallable = (*a2aAsk)(nil)

func (t *a2aAsk) Name() string { return A2AAskTool }

func (t *a2aAsk) Description() string {
	return "Ask one AI colleague one question, in their own turn. Use " +
		LookupColleagueTool + " first to get an exact handle. The colleague is " +
		"woken with your brief and answers asynchronously — this call " +
		"returns as soon as they have been asked, NOT with their answer, " +
		"so finish your turn rather than waiting. AGENTS ONLY: a human " +
		"teammate is reached by mentioning them on a shared surface."
}

func (t *a2aAsk) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "The colleague's exact handle, from " + LookupColleagueTool,
			},
			"brief": map[string]any{
				"type": "string",
				"description": "What you need from them, in full. They see " +
					"only this — not your plan, not your conversation — so " +
					"a question that needs context must carry it.",
			},
		},
		"required": []any{"target", "brief"},
	}
}

func (t *a2aAsk) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *a2aAsk) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refused(tools.RefusalForbidden, A2AAskTool+" can only be called during a turn, on behalf of a seat."), nil
	}
	if t.svc == nil {
		return refusedBy(tools.RefusalUnavailable, waitless(), "Agent-to-agent messaging is not configured on this deployment."), nil
	}

	target := strings.TrimSpace(argString(args, "target"))
	brief := strings.TrimSpace(argString(args, "brief"))
	switch {
	case target == "":
		return failed(A2AAskTool + " needs a `target` handle. Use " + LookupColleagueTool + " to find one."), nil
	case brief == "":
		return failed(A2AAskTool + " needs a `brief`. The colleague sees only this — " +
			"not your plan and not your conversation — so say what you need in full."), nil
	}

	// RESOLVED, not trusted: an ask to a handle nobody holds waits for an
	// answer that never comes, with nothing failing.
	resolved, refusal := t.resolve(turn, target)
	if refusal != nil {
		return *refusal, nil
	}
	if resolved.Kind == string(org.KindHuman) {
		// A human seat is never spawned, so no turn would answer: say so,
		// and what to do instead, or the agent waits forever.
		return failed(fmt.Sprintf(
			"%s (%s) is a human teammate, so "+A2AAskTool+" cannot reach them — "+
				"no turn answers on their behalf. Mention them on a shared "+
				"surface instead and continue without waiting.",
			resolved.Name, resolved.Handle)), nil
	}
	if resolved.Handle == seat.Handle() {
		// The answering side decides who replies by comparing the woken seat
		// with the requester, so a self-ask is read as an incoming ANSWER
		// and never replied to.
		return failed(A2AAskTool + " cannot address your own seat."), nil
	}

	// WHOEVER THIS TURN WORKS FOR, from the call's bound ceiling: the
	// colleague works for them too (ADR-0109). A turn whose ceiling cannot be
	// composed asks nobody: an ask carrying nobody is worked for this seat,
	// every app.
	ceiling, err := ceilingOf(ctx, turn, seat.Handle(), t.requesters)
	if err != nil {
		return refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not establish who this turn's work is for, which a colleague's ask "+
				"carries to them. Nobody was asked. Try again shortly.", A2AAskTool)), nil
	}

	id, err := t.svc.Open(ctx, a2a.Ask{
		Requester: seat.Handle(), Target: resolved.Handle, Brief: brief,
		SenderRole: seat.Name,
		// The chain travels so the answering seat refuses past the cap
		// rather than two agents asking each other until a budget runs out.
		// THIS TURN'S OWN DEPTH, unincremented: a2a.Service.Open, the
		// chokepoint every ask passes, adds the hop, and adding it here too
		// would charge two per hop.
		DelegationDepth: turn.Depth,
		DelegationChain: append(append([]string(nil), turn.Chain...), seat.Handle()),
		// THIS RUN: the ask's three audit records are read back by its id
		// (`WHERE turn_id = ?`) and the target's wake names it as parent;
		// the work key lets the work-key filter answer with the run's whole
		// record rather than only its phases (ADR-0017).
		TurnID:  turn.RunID,
		WorkKey: turn.WorkKey,
		// AND THE ITEM THIS TURN IS ON, which the answering turn inherits:
		// help on a task is work on it, and only this ask knows which task.
		// Nil when this turn is on nothing.
		WorkItem: turn.WorkItem, WorkItemBasis: turn.WorkItemBasis,
		// From the ceiling above, never from this call's arguments.
		WorkingFor: workingForOf(ceiling),
	})
	switch {
	case errors.Is(err, a2a.ErrSelfChannel), errors.Is(err, a2a.ErrNotAnAgent):
		// THE TARGET'S FAULT: the checks above catch both first, so this arm
		// is a chart that moved between the resolve and the open.
		why := "they do not hold an agent's seat"
		if errors.Is(err, a2a.ErrSelfChannel) {
			why = "an ask has two parties, and that seat is yours"
		}
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("Could not ask %s: %s.",
			resolved.Handle, why)), nil
	case err != nil:
		return nodeFailure(ctx, A2AAskTool, err, fmt.Sprintf("Could not reach %s",
			resolved.Handle), ""), nil
	}
	return tools.Result{Output: fmt.Sprintf(
		"Asked %s (%s). They answer in their own turn — their reply arrives "+
			"as a new message to you, so finish this turn rather than waiting. "+
			"Channel: %s",
		resolved.Handle, resolved.Name, id)}, nil
}

// resolve turns a model's spelling into a real seat, or explains why not.
//
// Two classes: a spelling that names nobody, or several, is the `target`
// argument's fault; a turn with no organization in scope is this node's.
func (t *a2aAsk) resolve(turn *turnctx.Turn, target string) (colleague.Seat, *tools.Result) {
	if turn.Org == nil {
		return colleague.Seat{}, refusalOf(refusedBy(tools.RefusalUnavailable, waitless(),
			"No organization is in scope, so there is nobody to ask."))
	}
	found := colleague.Resolve(target, Corpus(turn.Org, t.env, turn.WithholdsContacts))
	switch len(found) {
	case 0:
		return colleague.Seat{}, refusalOf(failed(fmt.Sprintf(
			"No colleague matches %q. Call "+LookupColleagueTool+" to find the right handle.",
			clip(target))))
	case 1:
		return found[0].Seat, nil
	}
	// EVERY candidate: the wanted handle is as likely to sort past a cap,
	// leaving the model to choose among options that exclude it.
	var names []string
	for _, c := range found {
		names = append(names, c.Seat.Handle)
	}
	return colleague.Seat{}, refusalOf(failed(fmt.Sprintf(
		"%q is ambiguous — it could be %s. Ask again with one exact handle.",
		clip(target), strings.Join(names, ", "))))
}
