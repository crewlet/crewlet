package builtin

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
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
	Activity(ctx context.Context, q worklet.ActivityQuery, now time.Time) (
		worklet.ActivityAnswer, error)
	MyWork(ctx context.Context, q worklet.MyWorkQuery, now time.Time,
		loc *time.Location) (worklet.MyWork, error)
}

type taskActivity struct{ deps WorkDeps }

var _ tools.SeatCallable = (*taskActivity)(nil)

func (t *taskActivity) Name() string { return worklet.TaskActivityTool }

func (t *taskActivity) Description() string {
	return "What HAPPENED, in the order it happened: every change to one task " +
		"or one project, with who made it and exactly which fields moved. A " +
		"board says what is there now; this is the only way to ask what " +
		"changed, and it answers at any age. Each change's text arrives " +
		"fenced as text its maker wrote; the two fence lines are a label, " +
		"not part of the change."
}

func (t *taskActivity) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type": "string",
				"description": "One task's whole history, by any key it " +
					"has had or by its id. " + itemRef,
			},
			"project": map[string]any{
				"type": "string",
				"description": "Everything that happened in one project. " +
					"Beside `task` it is only checked: the task's whole history " +
					"is answered, and a project the task is not in is refused.",
			},
			"kinds": map[string]any{
				"type": "string",
				"description": "Comma-separated change kinds to keep, such as " +
					"`status,assignee`, from: " + changeKindNames() +
					". Omit for everything.",
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
		return notInATurn(worklet.TaskActivityTool), nil
	}
	reader, ok := t.deps.Reader.(FeedReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(worklet.TaskActivityTool), nil
	}
	q := worklet.ActivityQuery{
		Task:    strings.TrimSpace(argString(args, "task")),
		Project: strings.TrimSpace(argString(args, "project")),
		Actor:   strings.TrimSpace(argString(args, "actor")),
		Q:       strings.TrimSpace(argString(args, "q")),
		Limit:   argInt(args, "limit", 0),
		Cursor:  strings.TrimSpace(argString(args, "cursor")),
		Level:   seatReadLevel,
	}
	// A KIND IS PASSED AS IT CAME and the tracker's read refuses one it
	// does not have, naming the set ([worklet.ErrBadQuery], which
	// [readFailure] words as an argument to change): built from any string
	// and never checked, a mistyped kind answered an empty feed that reads
	// exactly like a project where nothing happened.
	for _, kind := range strings.Split(argString(args, "kinds"), ",") {
		if kind = strings.TrimSpace(kind); kind != "" {
			q.Kinds = append(q.Kinds, worklet.ChangeKind(kind))
		}
	}
	// NEITHER KEY NAMED IS THE SEAT'S OWN PROJECT, not the whole company:
	// a model asking "what has been going on" means its own work, and a
	// company-wide feed is the one answer that is both expensive and
	// almost never what was meant.
	if q.Task == "" && q.Project == "" {
		q.Project = t.deps.defaultProject(actor)
		if q.Project == "" {
			q.Workspace = true
		}
	}
	if since := strings.TrimSpace(argString(args, "since")); since != "" {
		// A POSITION FIRST, because it is unambiguous and a timestamp is
		// not — and a caller resuming a page holds a position.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if at, err := statelog.ParsePosition(since); err == nil {
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
		return readFailure(ctx, worklet.TaskActivityTool, err), nil
	}
	// EACH CHANGE'S TEXT IS ITS MAKER'S WORDS — a comment, a description, a
	// hand-off's reason — fenced under them (ADR-0040); what a change moved
	// and the ask it was about are names its row records no writer of.
	read := newForeignRead(turn, actor.Handle)
	for i, rec := range answer.Records {
		fenceExcerpt(read, fieldPath{}.key("records").index(i).key("excerpt"),
			rec.Kind, rec.Actor, rec.ActorKind, rec.Excerpt)
		if len(rec.Fields) > 0 || rec.Ask != nil {
			namesBy(read)
		}
	}
	return read.answer(ctx, worklet.TaskActivityTool, answer,
		"Ask for fewer with `limit`, or narrow with `kinds`, `actor` or `since`.")
}

// changeKindNames is every change kind, for the schema that offers them: the
// set a model picks from is the one the read refuses anything outside of.
func changeKindNames() string {
	names := make([]string, len(worklet.ChangeKinds))
	for i, kind := range worklet.ChangeKinds {
		names[i] = "`" + string(kind) + "`"
	}
	return strings.Join(names, ", ")
}

type myWork struct{ deps WorkDeps }

var _ tools.SeatCallable = (*myWork)(nil)

func (t *myWork) Name() string { return worklet.MyWorkTool }

func (t *myWork) Description() string {
	return "Everything you are expected to look at, in one call: your " +
		"priorities in the order somebody put them, the work you hold, the " +
		"questions waiting on your answer with the call that answers each, " +
		"the checklist items you claimed on other people's tasks, what you " +
		"were brought onto, what moved on what you follow, and what just " +
		"became workable. Call this first. Each question arrives fenced as " +
		"text its asker wrote; the two fence lines are a label, not part of " +
		"the question."
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
		return notInATurn(worklet.MyWorkTool), nil
	}
	reader, ok := t.deps.Reader.(FeedReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(worklet.MyWorkTool), nil
	}
	// THE HANDLE IS THE TURN'S OWN SEAT AND TAKES NO ARGUMENT.
	//
	// A model that could name whose day to read could read anybody's —
	// which is a colleague's inbox, their priorities and the questions
	// they owe, handed to an agent nobody asked. The operator surface
	// supplies its own identity the same way, through the actor seam.
	//
	// AND IT IS THE ACTOR'S OWN RECORD, which is its handle: a person the
	// identity directory binds to a seat writes AS that seat
	// ([PrincipalActor]), so their day is the seat's and never the
	// credential's.
	out, err := reader.MyWork(ctx, worklet.MyWorkQuery{
		Handle: actor.Handle, Level: seatReadLevel,
	}, t.deps.now(), t.deps.zone())
	if err != nil {
		return readFailure(ctx, worklet.MyWorkTool, err), nil
	}
	// AN ASK IS ITS ASKER'S WORDS, fenced under them, its decision theirs
	// too; every title and checklist line is a name no row records a writer
	// of (ADR-0040).
	read := newForeignRead(turn, actor.Handle)
	for _, rows := range [][]worklet.TaskRow{out.Priorities, out.Assigned,
		out.Collaborating, out.WatchingRecent, out.UnblockedRecent} {
		rowNames(read, rows)
	}
	for i, ask := range out.AskedOfMe {
		asker := []provenance.Author{read.writer(ask.AskedBy, "")}
		read.fence(fieldPath{}.key("asked_of_me").index(i).key("body"),
			provenance.SourceWorkComment, asker, false, ask.Body)
		if ask.Decision != nil {
			read.attribute(asker, false)
		}
		namesBy(read)
	}
	if len(out.ChecklistItems) > 0 {
		namesBy(read)
	}
	return read.answer(ctx, worklet.MyWorkTool, out,
		"Read the blocks one at a time: "+ListWorkItemsTool+" for the tasks, "+
			worklet.WorkInboxTool+" for what is waiting on you.")
}
