package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
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
	return "What HAPPENED: every change to one task or one project, newest " +
		"first, with who made it and exactly which fields moved. A board says " +
		"what is there now; this is the only way to ask what changed, and it " +
		"answers at any age. An answer is one page: when it carries " +
		"`next_cursor` there are older changes, and passing it back as " +
		"`cursor` reads them. To come back later for only what is new, keep " +
		"the first page's `newest_position` and pass it as `since`."
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
				"description": "Only changes AFTER this point: the " +
					"`newest_position` of an earlier answer, or an RFC3339 " +
					"instant. Not for paging — `cursor` is.",
			},
			"q": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Text to find in a change's own excerpt. It "+
					"needs either `task`, or `project` with a `since` inside "+
					"%d days — at company scope it would read every change "+
					"ever made.", tracker.ActivityQuerySpanDays),
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("Changes per page: at most %d, which "+
					"is also the default. A smaller page costs more calls, "+
					"never any changes — `next_cursor` reads on.",
					tracker.MaxActivityRows),
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next_cursor` of the previous page, for " +
					"the older changes after it. Everything else stays as it was.",
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
		// not: the log's order is total and two authored instants can tie.
		//
		// `since` IS A LOWER BOUND and the feed is NEWEST FIRST, so it
		// takes the position of the newest change a caller has seen —
		// never a `next_cursor`, which is the OLDEST row of its page and
		// the resume point for the other direction. Handed one, `since`
		// answers the rows NEWER than that page's end — the page it came
		// from, again, rather than the one after it — and a caller reads
		// that as the rest of the history.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if at, err := tracker.ParseLogPosition(since); err == nil {
			q.Since = at
		} else if when, err := time.Parse(time.RFC3339, since); err == nil {
			q.SinceAt = when
		} else {
			return failed("`since` is either a position from a previous " +
				"answer's `newest_position`, or an RFC3339 instant like " +
				"2031-04-16T14:30:00Z. To read the page after one, pass its " +
				"`next_cursor` as `cursor` instead."), nil
		}
	}
	answer, err := reader.Activity(ctx, q, t.deps.now())
	if err != nil {
		return failed(readFailure(tracker.TaskActivityTool, err)), nil
	}
	return jsonResult(activityResult{
		ActivityAnswer: answer, NewestPosition: newestPosition(q, answer),
	})
}

// activityResult is the feed as this tool answers it: the tracker's own
// answer, plus the one position `since` takes.
type activityResult struct {
	tracker.ActivityAnswer

	// NewestPosition is where a caller resumes to read only what happened
	// AFTER this answer. See [newestPosition].
	NewestPosition string `json:"newest_position,omitempty"`
}

// newestPosition is the log position of the newest change on a FIRST page.
//
// It exists because `since` needs a position and nothing else on the answer
// is one of the right end: `next_cursor` is the oldest row of the page, and
// each row carries its position only as three separate fields. A first page
// is newest-first from the head of what matched, so its first row is the
// newest change the caller has now seen.
//
// ONLY on a first page. A later page's first row is older than the first
// page's, so offering it would let a caller skip back over changes it has
// already read and read them again as new.
func newestPosition(q tracker.ActivityQuery, answer tracker.ActivityAnswer) string {
	if q.Cursor != "" || len(answer.Records) == 0 {
		return ""
	}
	head := answer.Records[0]
	return statelog.Position{
		Stream: head.LogStream, Generation: head.LogGeneration, Seq: head.LogSeq,
	}.String()
}

type myWork struct{ deps WorkDeps }

var _ tools.SeatCallable = (*myWork)(nil)

func (t *myWork) Name() string { return tracker.MyWorkTool }

func (t *myWork) Description() string {
	return fmt.Sprintf("Everything you are expected to look at, in one call: your "+
		"priorities in the order somebody put them, the work you hold, the "+
		"questions waiting on your answer with the call that answers each, "+
		"the checklist items you claimed on other people's tasks, what you "+
		"were brought onto, what moved on what you follow, and what just "+
		"became workable. Call this first. Each block carries at most %d "+
		"rows: a block named under `truncated` holds more, and `rest` gives "+
		"`%s` arguments that list every row of it — for some blocks beside "+
		"rows the block leaves out, such as your own work among what you "+
		"watch. A question's `body` is its opening, ending in `…` where it "+
		"was cut; `%s` with its `key` as `item` and its `comment` as "+
		"`comment` reads it whole.",
		tracker.MyWorkRows, tracker.ListWorkItemsTool, GetWorkItemTool)
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
	return jsonResult(myWorkResult{MyWork: out, Rest: myWorkRest(actor.Handle, out.Truncated)})
}

// myWorkResult is `my_work` as this tool answers it: the tracker's own answer,
// plus where the rest of each cut block is.
type myWorkResult struct {
	tracker.MyWork

	// Rest is, for each block [tracker.MyWork.Truncated] names, the
	// `list_work_items` arguments whose answer holds every row of it. See
	// [myWorkRest].
	Rest map[string]map[string]any `json:"rest,omitempty"`
}

// myWorkRest names, for each cut block, `list_work_items` arguments whose
// answer holds every row the block cut.
//
// A CUT BLOCK NAMES ITS REST OR IT IS A SILENT ONE. Each block stops at
// [tracker.MyWorkRows] and says so under `truncated`; without a way to reach
// the rows past it, a seat holding thirty questions reads the twenty it was
// shown as its queue.
//
// THE LIST ANSWERS TASKS, so for the questions and the checklist claims it
// names the tasks that hold them, and `get_work_item` opens each.
//
// A SUPERSET WHERE THE LIST CANNOT NARROW AS FAR. The collaborations and the
// watches leave out the seat's own work, the watches leave out the muted ones,
// the claims leave out the items already done, and the unblocked work needs a
// dependency that was CLEARED where `has_dependencies` takes any. The list has
// a key for none of those, so for those blocks its answer holds rows the
// block does not — and every row it does.
//
// EVERY ENTRY CARRIES `archived: true`, because no block reads the archive —
// neither a task's own flag nor its project's — and the list leaves both out
// unless asked. Without it an open task the seat holds in an archived project,
// or a watched one somebody archived, can sit past a block's cut and be listed
// by no `rest` at all.
//
// `open_only: false` is there wherever the block does not stop at open work:
// an open question can sit on a finished task, the claims include finished
// tasks, and a watch outlives the work finishing.
//
// Where the list can page in the block's own order, the arguments ask for it:
// a `sort` for the blocks ordered by priority or by the last change, and the
// priorities preset, which keeps the stored order itself. The questions are
// ordered by when each was asked and the claims by their task's key, and the
// list sorts on neither.
//
// THE HANDLE IS THE SEAT'S OWN, from the same actor the answer was read for —
// never an argument, for the reason [myWork.CallForTurn] gives.
func myWorkRest(handle string, cut tracker.MyWorkTruncated) map[string]map[string]any {
	blocks := []struct {
		cut  bool
		name string
		args map[string]any
	}{
		{cut.Priorities, "priorities", map[string]any{"preset": tracker.PresetPriorities}},
		{cut.Assigned, "assigned", map[string]any{
			"assignee": handle, "sort": "-priority,due"}},
		{cut.AskedOfMe, "asked_of_me", map[string]any{
			"asked_of": handle, "open_only": false}},
		{cut.ChecklistItems, "checklist_items", map[string]any{
			"checklist_assignee": handle, "open_only": false}},
		{cut.Collaborating, "collaborating", map[string]any{
			"collaborator": handle, "sort": "-updated"}},
		{cut.WatchingRecent, "watching_recent", map[string]any{
			"watcher": handle, "open_only": false, "sort": "-updated"}},
		{cut.UnblockedRecent, "unblocked_recent", map[string]any{
			"assignee": handle, "has_dependencies": true, "blocked": false,
			"sort": "-updated"}},
	}
	var rest map[string]map[string]any
	for _, block := range blocks {
		if !block.cut {
			continue
		}
		if rest == nil {
			rest = map[string]map[string]any{}
		}
		// HERE RATHER THAN IN EACH LITERAL, because it is true of every
		// block and a block added without it is the silent cut again.
		block.args["archived"] = true
		rest[block.name] = block.args
	}
	return rest
}
