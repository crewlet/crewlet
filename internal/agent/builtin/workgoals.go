package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The goal tools, and they are the OPERATOR's alone.
//
// A goal is an outcome a PERSON commits the company to, with owners who report
// on it. A seat setting its own goals is a seat marking its own homework — and
// a founder already has a better lever over what gets done, which is the work
// itself.
//
// # The percentage is never an argument
//
// A goal is at what its targets say, computed on every read. There is
// deliberately no way to write one: a stored number is a second answer to a
// question that already has one, and the two drift the moment a task in a
// target closes without anybody editing the goal.

// GoalWriter is the tracker write side these tools need.
type GoalWriter interface {
	WriteGoal(ctx context.Context, opID string, goal tracker.Goal) (tracker.WriteResult, error)
}

// GoalReader is the read side.
type GoalReader interface {
	Goals(ctx context.Context, q tracker.GoalQuery) (tracker.GoalListing, error)
}

type listWorkGoals struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listWorkGoals)(nil)

func (t *listWorkGoals) Name() string { return tracker.ListWorkGoalsTool }

func (t *listWorkGoals) Description() string {
	return "List the company's goals with what each one is at. The progress " +
		"is computed from the work every time it is read — nothing stores a " +
		"percentage, so it can never disagree with the board."
}

func (t *listWorkGoals) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "description": "One goal, with its targets."},
			"owner": map[string]any{"type": "string", "description": "A handle that owns or contributes to the goal."},
			"group": map[string]any{"type": "string", "description": "The free-text label goals are filed under, e.g. a quarter."},
			"archived": map[string]any{
				"type":        "boolean",
				"description": "True includes archived goals. Default false.",
			},
		},
	}
}

func (t *listWorkGoals) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listWorkGoals) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	// THE IDENTITY CHECK IS ON A READ TOO, exactly as it is on
	// get_work_catalogue beside it: outside a turn a seat's registry has
	// no seat, and a tool that answered anyway would be answering as
	// nobody. The operator surface supplies its own actor, so this
	// succeeds there.
	if _, err := t.deps.actor(ctx, turn); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.ListWorkGoalsTool), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(tracker.ListWorkGoalsTool), nil
	}
	listing, err := t.deps.Reader.Goals(ctx, tracker.GoalQuery{
		ID:       strings.TrimSpace(argString(args, "id")),
		Owner:    strings.TrimSpace(argString(args, "owner")),
		Group:    strings.TrimSpace(argString(args, "group")),
		Archived: argBool(args, "archived"),
		// THE SEAT'S OWN LEVEL, like every other tool read here: a
		// caller that writes a goal and then lists sees the goal it
		// just wrote — see [seatReadLevel].
		Level: seatReadLevel,
	})
	if err != nil {
		return failed(readFailure(tracker.ListWorkGoalsTool, err)), nil
	}
	if len(listing.Goals) == 0 && listing.Complete {
		return tools.Result{Output: "No goals match that filter."}, nil
	}
	return jsonResult(map[string]any{
		"count": len(listing.Goals), "goals": listing.Goals,
		"read_level": listing.Level, "complete": listing.Complete,
	})
}

type writeWorkGoal struct{ deps WorkDeps }

var _ tools.Callable = (*writeWorkGoal)(nil)

func (t *writeWorkGoal) Name() string { return tracker.WriteWorkGoalTool }

func (t *writeWorkGoal) Description() string {
	return "Set a goal — an outcome with targets under it. Pass an existing " +
		"goal's `id` to replace it whole, or omit it to create one. There is " +
		"no progress argument: a goal is at what its targets say, computed " +
		"every time it is read."
}

func (t *writeWorkGoal) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The goal to replace. Omit to create a new one.",
			},
			"name":        map[string]any{"type": "string", "description": "What the outcome is."},
			"description": map[string]any{"type": "string", "description": "The case for it, in markdown."},
			"owners": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Handles that report on this goal. At least one — " +
					"a goal that reports to nobody is a note.",
			},
			"members": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Handles contributing without owning it.",
			},
			"group": map[string]any{
				"type":        "string",
				"description": "A free-text label goals are filed under, e.g. `H1` or `2026 Q2`.",
			},
			"update": map[string]any{
				"type": "object",
				"description": "Post a health update — the one part of a goal " +
					"written in somebody's own words, and what its owners and " +
					"members are woken with. APPENDED to the goal's history; " +
					"nothing rewrites an existing one.",
				"properties": map[string]any{
					"health": map[string]any{"type": "string", "description": "The health this update reports."},
					"text":   map[string]any{"type": "string", "description": "What is going on, in prose."},
				},
			},
			"health": map[string]any{
				"type": "string",
				"description": "The OWNER's judgement, never derived from the " +
					"targets: a goal at 90% with a week left and one at 90% " +
					"with a day left are different situations.",
				"enum": healthNames(),
			},
			"start_at": map[string]any{"type": "string", "description": "RFC3339."},
			"due_at":   map[string]any{"type": "string", "description": "RFC3339."},
			"archived": map[string]any{"type": "boolean"},
			"targets": map[string]any{
				"type":        "array",
				"description": "The measurable outcomes. Replaces the goal's targets whole.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":   map[string]any{"type": "string", "description": "Stable per goal; a progress note addresses it."},
						"name": map[string]any{"type": "string"},
						"type": map[string]any{
							"type": "string",
							"enum": tracker.TargetTypes,
							"description": "`tasks` counts finished work and is the " +
								"only one the engine moves on its own; `number` and " +
								"`percent` are a start, a goal and a current; " +
								"`binary` is done or not.",
						},
						"start":    map[string]any{"type": "number"},
						"goal":     map[string]any{"type": "number"},
						"current":  map[string]any{"type": "number"},
						"unit":     map[string]any{"type": "string"},
						"done":     map[string]any{"type": "boolean"},
						"tasks":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"projects": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"id", "name", "type"},
				},
			},
		},
		"required": []string{"name", "owners"},
	}
}

func (t *writeWorkGoal) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *writeWorkGoal) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.WriteWorkGoalTool), nil
	}
	if t.deps.GoalWriter == nil {
		return unconfigured(tracker.WriteWorkGoalTool), nil
	}
	id := strings.TrimSpace(argString(args, "id"))
	if id == "" {
		id = uuid.NewString()
	}
	// THE OWNERS AND MEMBERS THE GOAL ALREADY HAS, read before the write so
	// a save can carry a handle that no longer resolves without being
	// refused for it — see [WorkDeps.resolveHandles]. A goal whose owner
	// left the company must stay editable, not least to take them off it.
	before := t.deps.goalParties(ctx, id)
	owners, refusal := t.deps.resolveHandles(tracker.WriteWorkGoalTool,
		"`owners`", argStrings(args, "owners"), before)
	if refusal != "" {
		return failed(refusal), nil
	}
	members, refusal := t.deps.resolveHandles(tracker.WriteWorkGoalTool,
		"`members`", argStrings(args, "members"), before)
	if refusal != "" {
		return failed(refusal), nil
	}
	goal := tracker.Goal{
		ID:          id,
		Name:        strings.TrimSpace(argString(args, "name")),
		Description: argString(args, "description"),
		Owners:      owners,
		Members:     members,
		Group:       strings.TrimSpace(argString(args, "group")),
		Health:      strings.TrimSpace(argString(args, "health")),
		Archived:    argBool(args, "archived"),
	}
	for _, spec := range []struct {
		key  string
		into **time.Time
	}{{"start_at", &goal.StartAt}, {"due_at", &goal.DueAt}} {
		raw := strings.TrimSpace(argString(args, spec.key))
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return failed(fmt.Sprintf("%s is a date and time in RFC3339, like "+
				"2026-06-30T00:00:00Z — %q is not one.", spec.key, clip(raw))), nil
		}
		utc := at.UTC()
		*spec.into = &utc
	}
	targets, refusal := goalTargets(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	goal.Targets = targets
	// THE UPDATE IS APPENDED BY THE WRITER, which carries the stored
	// history forward and stamps the author and the instant: an update is
	// something somebody SAID on a date, and a caller that could set
	// either would be filing an assessment under another name.
	if raw, held := args["update"].(map[string]any); held {
		health := strings.TrimSpace(argString(raw, "health"))
		text := strings.TrimSpace(argString(raw, "text"))
		if health == "" && text == "" {
			return failed("An `update` needs a `health`, a `text` or both — " +
				"an empty one tells the goal's owners nothing."), nil
		}
		goal.Updates = []tracker.GoalUpdate{{Health: health, Text: text}}
	}

	result, err := t.deps.GoalWriter(actor).WriteGoal(ctx, "goal-"+id, goal)
	if err != nil {
		return failed(writeFailure(tracker.WriteWorkGoalTool, err)), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(map[string]any{
		"id": id, "outcome": string(result.Outcome), "version": result.Version,
	})
}

// goalParties is who a stored goal already names.
//
// BEST EFFORT: a read that fails answers nobody, which makes the save STRICTER
// rather than laxer — every handle then has to resolve. The alternative,
// failing the write because a read for a leniency check failed, would refuse a
// save that is perfectly valid.
func (d WorkDeps) goalParties(ctx context.Context, id string) []string {
	if d.Reader == nil || id == "" {
		return nil
	}
	listing, err := d.Reader.Goals(ctx, tracker.GoalQuery{
		ID: id, Level: seatReadLevel,
	})
	if err != nil || len(listing.Goals) == 0 {
		return nil
	}
	return append(append([]string{}, listing.Goals[0].Owners...),
		listing.Goals[0].Members...)
}

// goalTargets reads the targets a model sent.
//
// A REFUSAL RATHER THAN A DROPPED TARGET: a goal silently saved with three of
// its four targets is a goal reporting a percentage over the wrong set, which
// is exactly the kind of wrong number a person quotes in a review.
func goalTargets(args map[string]any) ([]tracker.GoalTarget, string) {
	raw, held := args["targets"].([]any)
	if !held {
		return nil, ""
	}
	out := make([]tracker.GoalTarget, 0, len(raw))
	for i, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Target %d is not an object.", i+1)
		}
		out = append(out, tracker.GoalTarget{
			ID:       strings.TrimSpace(argString(fields, "id")),
			Name:     strings.TrimSpace(argString(fields, "name")),
			Type:     strings.TrimSpace(argString(fields, "type")),
			Start:    argFloat(fields, "start"),
			Goal:     argFloat(fields, "goal"),
			Current:  argFloat(fields, "current"),
			Unit:     strings.TrimSpace(argString(fields, "unit")),
			Done:     argBool(fields, "done"),
			Tasks:    argStrings(fields, "tasks"),
			Projects: argStrings(fields, "projects"),
		})
	}
	return out, ""
}

// healthNames renders the closed set for the tool schema.
func healthNames() []string {
	out := make([]string, 0, len(tracker.GoalHealths))
	for _, health := range tracker.GoalHealths {
		if health == tracker.HealthUnset {
			// THE EMPTY STRING IS A REAL STATE and not an enum value a
			// model should pick: "nobody has judged this yet" is what
			// omitting the argument means.
			continue
		}
		out = append(out, string(health))
	}
	return out
}
