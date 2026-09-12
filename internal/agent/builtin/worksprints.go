package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Managing a sprint — start it, close it, settle where its work went.
//
// # Lead-gated in every action, and an operator's alone
//
// A sprint is a COMMITMENT a team made together: starting one changes what
// everybody is expected to work on this fortnight, and closing one decides
// what counted. A seat that could do either would be a seat deciding its own
// team's plan, and the refusal it was working around — "this sprint is full",
// "that work is not in this sprint" — is the signal a person needs.
//
// So `manage_sprint` is on the operator surface, where a call carries a
// person's own credential, and is additionally gated on leading the project:
// somebody with a token is not automatically somebody who plans this team's
// fortnight.
//
// # And almost nothing needs it
//
// The duty mints sprints, starts them at their window under `auto_start`,
// ALWAYS closes them at their end, and rolls the unfinished work forward under
// `auto_roll`. What is left for a person is the two decisions a policy cannot
// make: ending a sprint early, and settling a spillover the policy declined to
// decide.

// SprintWriter is the tracker write side this tool needs.
type SprintWriter interface {
	StartSprint(ctx context.Context, opID, project string, number int) (
		tracker.WriteResult, error)
	CloseSprint(ctx context.Context, opID, project string, number int) (
		tracker.WriteResult, error)
	RolloverSprint(ctx context.Context, opID, project string, number int,
		target tracker.RolloverTarget) (int, bool, error)
}

// LeadsProject reports whether a handle leads the unit that owns a project.
//
// A SEAM RATHER THAN A CHART, for the reason [Leads] is one: the answer is a
// fact about the company's configuration, this package holds none, and a lead
// relation derived here would be a second opinion about the hierarchy.
//
// NIL RESOLVES FALSE, which refuses every action naming what is missing — the
// safe direction, and the one an operator can diagnose: a surface that wired
// no lookup loses the verb rather than opening it to everybody.
type LeadsProject func(ctx context.Context, actor, project string) bool

type manageSprint struct {
	deps  WorkDeps
	leads LeadsProject
}

var _ tools.Callable = (*manageSprint)(nil)

func (t *manageSprint) Name() string { return tracker.ManageSprintTool }

func (t *manageSprint) Description() string {
	return "Start a sprint, close one early, or settle where a closed " +
		"sprint's unfinished work goes. Almost nothing needs this: sprints " +
		"are minted, started at their window and ALWAYS closed at their end " +
		"by the engine itself, and the unfinished work rolls forward unless " +
		"the project's policy says a person decides. Lead-gated."
}

func (t *manageSprint) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type":        "string",
				"description": "The project key.",
			},
			"action": map[string]any{
				"type": "string",
				"enum": []string{"start", "close", "rollover"},
				"description": "`start` runs a future sprint; `close` ends one " +
					"early (a sprint always closes at its end anyway, and a " +
					"close is irreversible); `rollover` settles where a closed " +
					"sprint's unfinished work went.",
			},
			"sprint": map[string]any{
				"type":        "integer",
				"description": "Which sprint, by number.",
			},
			"rollover_to": map[string]any{
				"type": "string",
				"description": "For `rollover`, and optional on `close`: " +
					"`next` carries the unfinished work into the next sprint " +
					"(minting one if there is none), `backlog` clears each " +
					"task's sprint and leaves it in the project, `close` " +
					"cancels every open task — abandoned, not delivered — or " +
					"a sprint NUMBER names any future or active sprint.",
			},
		},
		"required": []string{"project", "action", "sprint"},
	}
}

func (t *manageSprint) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *manageSprint) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.ManageSprintTool), nil
	}
	if t.deps.SprintWriter == nil {
		return unconfigured(tracker.ManageSprintTool), nil
	}
	project := strings.TrimSpace(argString(args, "project"))
	number := argInt(args, "sprint", 0)
	switch {
	case project == "":
		return failed("Name the project this sprint belongs to — " +
			"list_projects reports the keys."), nil
	case number < 1:
		return failed("Name the sprint by its number. sprint_report lists " +
			"them with their numbers."), nil
	}
	// THE LEAD RELATION IS RESOLVED HERE and never inside the tracker,
	// which holds no chart. A surface that wired no lookup refuses, which
	// is the safe direction: a sprint decision belongs to whoever plans
	// the team's fortnight, and "everybody with a token" is not that.
	if t.leads == nil || !t.leads(ctx, actor.Handle, project) {
		return failed(fmt.Sprintf("Only %s's lead manages its sprints — "+
			"starting one changes what the whole team is expected to work on, "+
			"and closing one decides what counted.", project)), nil
	}

	writer := t.deps.SprintWriter(actor)
	action := strings.TrimSpace(argString(args, "action"))
	opID := fmt.Sprintf("sprint-%s-%s-%d-%s", action, project, number,
		turnKeyOr(turn))

	switch action {
	case "start":
		result, err := writer.StartSprint(ctx, opID, project, number)
		if err != nil {
			return failed(writeFailure(tracker.ManageSprintTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		return jsonResult(map[string]any{
			"project": project, "sprint": number, "state": "active",
			"outcome": string(result.Outcome), "version": result.Version,
		})
	case "close":
		result, err := writer.CloseSprint(ctx, opID, project, number)
		if err != nil {
			return failed(writeFailure(tracker.ManageSprintTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		out := map[string]any{
			"project": project, "sprint": number, "state": "closed",
			"outcome": string(result.Outcome), "version": result.Version,
		}
		// A CLOSE MAY CARRY ITS OWN SPILLOVER DECISION, which is what a
		// lead ending a sprint early usually means — and saves them a
		// second call at the moment the board is most confusing.
		if target := strings.TrimSpace(argString(args, "rollover_to")); target != "" {
			moved, done, err := writer.RolloverSprint(ctx, opID+"-roll",
				project, number, tracker.RolloverTarget(target))
			if err != nil {
				return failed(writeFailure(tracker.ManageSprintTool, err)), nil
			}
			out["rolled"], out["rollover_complete"] = moved, done
		}
		return jsonResult(out)
	case "rollover":
		target := strings.TrimSpace(argString(args, "rollover_to"))
		if target == "" {
			return failed("Say where the unfinished work goes: `next`, " +
				"`backlog`, `close`, or a sprint number."), nil
		}
		moved, done, err := writer.RolloverSprint(ctx, opID, project, number,
			tracker.RolloverTarget(target))
		if err != nil {
			return failed(writeFailure(tracker.ManageSprintTool, err)), nil
		}
		return jsonResult(map[string]any{
			"project": project, "sprint": number, "rollover_to": target,
			"moved": moved,
			// A FULL BATCH LEAVES MORE, and the answer says so rather
			// than reporting a finished rollover: the duty's next tick
			// continues it, and a lead told "done" would go looking for
			// work that has not moved yet.
			"complete": done,
		})
	}
	return failed("`action` is one of start, close or rollover."), nil
}
