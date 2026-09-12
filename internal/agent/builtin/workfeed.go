package builtin

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// What happened, and what is expected of me.
//
// # The board answers "what is there"; these answer everything else
//
// `list_work_items` is about the rows that exist now. Every question about a
// CHANGE — who moved this, when did it stop being blocked, what did that bulk
// edit actually do — is about the ordered log instead, and no filter over the
// rows can reach it: a task that was reassigned twice and back looks exactly
// like one nobody touched.
//
// # And `my_work` is the call a turn opens with
//
// Seven lists in one answer rather than seven calls, because assembled
// separately a seat could see a task in `assigned` that had already moved out
// of it by the time `priorities` was read — and spend its turn on work
// somebody else had taken.

// FeedReader is the tracker read side these two need.
type FeedReader interface {
	Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
		tracker.ActivityAnswer, error)
	MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time) (
		tracker.MyWork, error)
}

type taskActivity struct{ deps WorkDeps }

var _ tools.SeatCallable = (*taskActivity)(nil)

func (t *taskActivity) Name() string { return tracker.TaskActivityTool }

func (t *taskActivity) Description() string {
	return "What HAPPENED, in the order it happened: every change to one task " +
		"or one project, with who made it and exactly which fields moved. A " +
		"board says what is there now; this is the only way to ask what " +
		"changed, and it answers at any age."
}

func (t *taskActivity) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type": "string",
				"description": "One task's whole history, by key or id — " +
					"including keys it used to have.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Everything that happened in one project.",
			},
			"kinds": map[string]any{
				"type": "string",
				"description": "Comma-separated change kinds to keep, such as " +
					"`status,assignee`. Omit for everything.",
			},
			"actor": map[string]any{
				"type": "string",
				"description": "Who made the change — which is not the same " +
					"question as whose work it is.",
			},
			"since": map[string]any{
				"type": "string",
				"description": "Resume from a position a previous answer's " +
					"`next_cursor` carried, or an RFC3339 instant.",
			},
			"q": map[string]any{
				"type": "string",
				"description": "Text to find in a change's own excerpt. It " +
					"needs either `task`, or `project` with a `since` inside " +
					"90 days — at company scope it would read every change " +
					"ever made.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "At most 200, which is also the default.",
			},
			"cursor": map[string]any{
				"type":        "string",
				"description": "The `next_cursor` of the previous page.",
			},
		},
	}
}

func (t *taskActivity) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *taskActivity) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.TaskActivityTool), nil
	}
	reader, ok := t.deps.Reader.(FeedReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(tracker.TaskActivityTool), nil
	}
	q := tracker.ActivityQuery{
		Task:    strings.TrimSpace(argString(args, "task")),
		Project: strings.TrimSpace(argString(args, "project")),
		Actor:   strings.TrimSpace(argString(args, "actor")),
		Q:       strings.TrimSpace(argString(args, "q")),
		Limit:   argInt(args, "limit", 0),
		Cursor:  strings.TrimSpace(argString(args, "cursor")),
		Level:   seatReadLevel,
	}
	for _, kind := range strings.Split(argString(args, "kinds"), ",") {
		if kind = strings.TrimSpace(kind); kind != "" {
			q.Kinds = append(q.Kinds, tracker.ChangeKind(kind))
		}
	}
	// NEITHER KEY NAMED IS THE SEAT'S OWN PROJECT, not the whole company:
	// a model asking "what has been going on" means its own work, and a
	// company-wide feed is the one answer that is both expensive and
	// almost never what was meant.
	if q.Task == "" && q.Project == "" {
		q.Project = t.deps.defaultProject(actor.Handle)
		if q.Project == "" {
			q.Workspace = true
		}
	}
	if since := strings.TrimSpace(argString(args, "since")); since != "" {
		// A POSITION FIRST, because it is unambiguous and a timestamp is
		// not — and a caller resuming a page holds a position.
		if at, err := tracker.ParseLogPosition(since); err == nil {
			q.Since = at
		} else if when, err := time.Parse(time.RFC3339, since); err == nil {
			q.SinceAt = when
		} else {
			return failed("`since` is either a position from a previous " +
				"answer's `next_cursor`, or an RFC3339 instant like " +
				"2031-04-16T14:30:00Z."), nil
		}
	}
	answer, err := reader.Activity(ctx, q, t.deps.now())
	if err != nil {
		return failed(readFailure(tracker.TaskActivityTool, err)), nil
	}
	return jsonResult(answer)
}

type myWork struct{ deps WorkDeps }

var _ tools.SeatCallable = (*myWork)(nil)

func (t *myWork) Name() string { return tracker.MyWorkTool }

func (t *myWork) Description() string {
	return "Everything you are expected to look at, in one call: your " +
		"priorities in the order somebody put them, the work you hold, the " +
		"questions waiting on your answer with the call that answers each, " +
		"the checklist items you claimed on other people's tasks, what you " +
		"were brought onto, what moved on what you follow, and what just " +
		"became workable. Call this first."
}

func (t *myWork) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *myWork) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *myWork) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	_ map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.MyWorkTool), nil
	}
	reader, ok := t.deps.Reader.(FeedReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(tracker.MyWorkTool), nil
	}
	// THE HANDLE IS THE TURN'S OWN SEAT AND TAKES NO ARGUMENT.
	//
	// A model that could name whose day to read could read anybody's —
	// which is a colleague's inbox, their priorities and the questions
	// they owe, handed to an agent nobody asked. The operator surface
	// supplies its own identity the same way, through the actor seam.
	out, err := reader.MyWork(ctx, tracker.MyWorkQuery{
		Handle: actor.Handle, Level: seatReadLevel,
	}, t.deps.now())
	if err != nil {
		return failed(readFailure(tracker.MyWorkTool, err)), nil
	}
	return jsonResult(out)
}
