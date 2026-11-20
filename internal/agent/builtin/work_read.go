package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// queryAlias is one argument this surface spells differently from the query
// grammar it compiles into.
//
// Written down because a saved view stores the GRAMMAR's keys
// ([worklet.ParseQuery]) while these arguments are the model's vocabulary, and a
// model saving the query it just ran must be told which to write. The
// translation stays in [listWorkItems.CallForTurn]; a test holds both against
// what the tool sends.
type queryAlias struct {
	// Arg is this surface's argument, Key the grammar's parameter.
	Arg, Key string

	// Shape is what the grammar's value looks like where it is not simply
	// the argument's own — empty for a plain rename.
	Shape string
}

var queryAliases = []queryAlias{
	{Arg: "project", Key: "container", Shape: "`project:ENG`, or `workspace` for the whole company"},
	{Arg: "text", Key: "q"},
	{Arg: "label", Key: "tag"},
	{Arg: "open_only", Key: "status_group", Shape: "`not_started,active`"},
}

// AliasSentence names every rename, for a tool description that has to tell a
// model which vocabulary to write in.
func AliasSentence() string {
	parts := make([]string, 0, len(queryAliases))
	for _, a := range queryAliases {
		part := fmt.Sprintf("`%s` is `%s`", a.Arg, a.Key)
		if a.Shape != "" {
			part += ", as " + a.Shape
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// ---- list_work_items --------------------------------------------------- //

type listWorkItems struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listWorkItems)(nil)

func (t *listWorkItems) Name() string { return ListWorkItemsTool }

func (t *listWorkItems) Description() string {
	return "List work items in Worklet, the company's tracker, filtered. Use " +
		"this to see what you are assigned, what is open in a project, or " +
		"whether something has already been filed before you file it again. " +
		"Returns summaries; call " + GetWorkItemTool + " for one item's full " +
		"description, comments and links. " + flowSentence
}

// flowSentence is what each tool reading items tells a model an item's flow is
// (ADR-0162).
const flowSentence = "An item in progress carries `flow`: why it is or is " +
	"not moving now (working, blocked, waiting, held, due, stuck or moving), " +
	"its cause, since when and, where known, until when, such as when its " +
	"assignee's model limit resets. `flow_live: false` means this node could " +
	"not read the seats' live state: no item reads working, and none reads " +
	"held for a paused, unplaced or limited assignee."

func (t *listWorkItems) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"assignee": map[string]any{
				"type": "string",
				"description": "A seat's handle. Pass your own handle for " +
					"your queue; omit for everybody's.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "A project key, e.g. ENG. Omit for every project.",
			},
			"status": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Statuses to include: " + words.Or(worklet.Statuses) + ".",
			},
			"open_only": map[string]any{
				"type": "boolean",
				"description": "True lists only items that are not done or " +
					"cancelled. Default false, which lists everything.",
			},
			"preset": map[string]any{
				"type": "string",
				"enum": worklet.Presets,
				"description": "A saved question. `my_queue` is YOUR open " +
					"work, most important first; `blocked` is open work that " +
					"cannot move; `overdue` is open work past its due date. " +
					"Any other argument you pass overrides the preset's own.",
			},
			"view": map[string]any{
				"type": "string",
				"description": "A saved view's `id`, from " + worklet.ListWorkViewsTool + ": " +
					"runs the query somebody arranged. Any other argument " +
					"you pass overrides the view's own.",
			},
			"label": map[string]any{"type": "string", "description": "One label to filter on."},
			"removed": map[string]any{
				"type": "boolean",
				"description": "True lists the TRASH — items somebody removed " +
					"— and nothing else. This is the only way to see them: a " +
					"removed item is out of every other list. They are not " +
					"destroyed and an operator can restore one at any age.",
			},
			"text": map[string]any{
				"type": "string",
				"description": "Substring of the KEY or TITLE only — it does " +
					"not see descriptions. For an item whose key you are half " +
					"sure of. To find work by what it is ABOUT, use " +
					SearchWorkItemsTool + ", which ranks over descriptions too; " +
					SearchKnowledgeTool + " is for the company's written knowledge.",
			},
			"type": map[string]any{
				"type": "string",
				"description": "A work type — `bug`, `story`, `task`, and " +
					"whatever else this company declares. " + worklet.GetWorkCatalogueTool + " " +
					"lists them. Comma separate for several.",
			},
			"priority": map[string]any{
				"type":        "string",
				"description": "One of " + words.Or(worklet.Priorities) + ". Comma separate for several.",
			},
			"due": map[string]any{
				"type": "string",
				"description": "When it is due. A comparison — `lt:today`, " +
					"`gte:+3d`, `range:sow..eow` — or one of the shorthands " +
					"`overdue`, `next7`, `last7`, `thisweek`, `thismonth`, " +
					"`lastmonth`, `earlier`.",
			},
			"updated": map[string]any{
				"type": "string",
				"description": "When it last changed, in the same shapes as " +
					"`due`. `gte:-1d` is what moved since yesterday.",
			},
			"created": map[string]any{
				"type":        "string",
				"description": "When it was filed, in the same shapes as `due`.",
			},
			"parent": map[string]any{
				"type": "string",
				"description": "An item, to list its SUBTASKS — for reading " +
					"a piece of work broken down. " + itemRef,
			},
			"reporter": map[string]any{
				"type":        "string",
				"description": "Who filed it. Your own handle is what you filed.",
			},
			"watcher": map[string]any{
				"type":        "string",
				"description": "Who is following it. Your own handle is what you follow.",
			},
			"unit": map[string]any{
				"type": "string",
				"description": "A team, by its id or its name: the work FILED " +
					"into that team, whoever holds it. Where it routes NOW is " +
					"`routing_unit`, which a re-route moves and this does not.",
			},
			"field_filters": map[string]any{
				"type": "object",
				"description": "Filter on this company's own custom fields, " +
					"keyed by field SLUG — {\"impact\": \"high\"}. " +
					worklet.DescribeProjectTool + " lists what a project declares and what " +
					"each may hold. An operator is available per type, e.g. " +
					"`>3` on a number or `a..b` on a date.",
				"additionalProperties": true,
			},
			"sort": map[string]any{
				"type": "string",
				"description": "How to order: `rank` (the board's own order), " +
					"`updated`, `created`, `due`, `priority`, `key`, `status`, " +
					"`type`, `assignee` (by the seat's name), or `f.<slug>` " +
					"for a custom field. Prefix with `-` to reverse.",
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next_cursor` from a previous call, for " +
					"the page after it. Everything else stays as it was.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).", worklet.PageMax, worklet.PageDefault),
			},
		},
	}
}

func (t *listWorkItems) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listWorkItems) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	// [WorkDeps.actor], never turn.RequireSeat(): the OPERATOR surface has no
	// turn and supplies its own actor.
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(ListWorkItemsTool, err), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(ListWorkItemsTool), nil
	}

	// Arguments translate once into the query grammar's own keys: one grammar
	// serves the board, the socket, the REST route and this tool, and a second
	// parser would let a filter mean something different.
	params := map[string]any{}
	if v := strings.TrimSpace(argString(args, "project")); v != "" {
		params["container"] = "project:" + strings.ToUpper(v)
	}
	// Same-word keys forward as-is. Board furniture (group, subgroup,
	// group_limit, totals) is left out: a model reads rows, and grouping costs it
	// a shape to unpack.
	for _, key := range []string{
		"assignee", "limit", "removed",
		"type", "priority", "due", "updated", "created",
		"reporter", "watcher", "unit",
		"sort", "cursor", "view",
	} {
		if v, held := args[key]; held {
			params[key] = v
		}
	}
	// The parent is resolved: `parent` matches `parent_id` raw, so a key passed
	// through answers an empty list, no error.
	if ref := strings.TrimSpace(argString(args, "parent")); ref != "" {
		id, refused := t.deps.resolveRef(ctx, ListWorkItemsTool, "`parent`", ref)
		if refused != nil {
			return *refused, nil
		}
		params["parent"] = id
	}
	// Custom-field filters (`f.<slug>`). An object, not `slug=value` strings, so
	// a value containing `=` stays writable.
	if raw, held := args["field_filters"].(map[string]any); held {
		for slug, value := range raw {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			params[worklet.FieldKeyPrefix+slug] = value
		}
	}
	// The grammar's text key is `q`; copied under `text` it would not filter.
	if v := strings.TrimSpace(argString(args, "text")); v != "" {
		params["q"] = v
	}
	if v := strings.TrimSpace(argString(args, "label")); v != "" {
		params["tag"] = v
	}
	if names := argStrings(args, "status"); len(names) > 0 {
		params["status"] = strings.Join(names, ",")
	}
	if v := strings.TrimSpace(argString(args, "preset")); v != "" {
		params["preset"] = v
	}
	if open, _ := argval.Bool(args["open_only"]); open {
		// `in_progress` and `blocked` are not status groups; the parser refuses them.
		params["status_group"] = strings.Join(openGroups(), ",")
	}

	// The SEAT and its own project are the viewer, from the immutable turn
	// context, so `preset=my_queue` is this seat's; a model naming its viewer
	// could read as anybody.
	q, err := t.deps.Reader.ExpandedQuery(ctx, params, worklet.Viewer{
		// A person bound to a seat writes AS that seat ([PrincipalActor]), so
		// my_queue is theirs, not the credential's.
		Handle: actor.Handle, Project: t.deps.defaultProject(actor),
	},
		t.deps.now(), t.deps.zone())
	if err != nil {
		return failed(fmt.Sprintf("That filter is not one Worklet accepts: %s.",
			worklet.Sentence(err))), nil
	}
	// This surface's level OVERRULES rather than fills in: a level can arrive
	// from a grammar path that knew nothing of this surface, and a guard that
	// only filled an empty field would enforce nothing ([statelog.LevelFor]).
	q.Level = statelog.LevelFor(statelog.SurfaceSeat, q.Level)
	// A model types the team name, the rows hold the chart's key
	// ([worklet.Units]).
	q.Units = t.deps.Units
	// The grammar's default, [worklet.SubtasksCollapsed], filters the ROOT and
	// carries its subtree unfiltered: right for a board's tree, wrong for a list a
	// model counts, where others' subtasks would read as its own queue. Overruled
	// since a saved `view` carries a board's shape; a subtree is asked with
	// `parent`.
	q.Subtasks = worklet.SubtasksSeparate
	// `fields=` and `around` are a board card's and a task page's opt-ins, which
	// a saved `view` may carry and which would grow every row in a model's
	// context.
	q.RowFields, q.Around = nil, ""
	answer, err := t.deps.Reader.Tasks(ctx, q, t.deps.now())
	switch {
	case errors.Is(err, worklet.ErrTooBroad):
		// Not [readFailure]'s "try again": the QUERY is at fault and repeats
		// forever.
		return failed(fmt.Sprintf("%s: %s.", ListWorkItemsTool, worklet.Sentence(err))), nil
	case errors.Is(err, worklet.ErrBadCursor):
		// Not [readFailure]'s "change the arguments": the cursor is the one to drop.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused the "+
			"cursor: %s. Call it again without `cursor` to start from the first "+
			"page.", ListWorkItemsTool, ReadRefusalSentence(err))), nil
	case err != nil:
		return readFailure(ctx, ListWorkItemsTool, err), nil
	}
	// A grouped answer has no flat rows, so emptiness asks the groups too; a
	// closed axis carries every admitted column (internal/worklet's grouping
	// doc), so empty means no column COUNTS anything. A board wrongly reported
	// empty invites a duplicate.
	if len(answer.Rows) == 0 && boardEmpty(answer.Groups) && answer.Complete {
		return tools.Result{Output: "No work items match that filter."}, nil
	}
	// FALSE IS SAID: else a flow read with no live fact reads as every seat free.
	result := map[string]any{"count": len(answer.Rows), "items": answer.Rows,
		"flow_live": answer.FlowLive}
	if len(answer.Groups) > 0 {
		result["groups"] = answer.Groups
		result["groups_overlap"] = answer.GroupsOverlap
		if answer.GroupsDropped > 0 {
			result["groups_dropped"] = answer.GroupsDropped
		}
	}
	if len(answer.Totals) > 0 {
		result["totals"] = answer.Totals
	}
	if answer.TotalHint > len(answer.Rows) {
		result["total"] = answer.TotalHint
		// The count stops at a ceiling: a `total` at it is no exact number.
		if answer.TotalCapped {
			result["total_is_at_least"] = true
		}
	}
	if answer.NextCursor != "" {
		result["next_cursor"] = answer.NextCursor
	}
	if !answer.Complete {
		// Else a seat concludes the work does not exist and files a duplicate.
		result["incomplete"] = incompleteNote(answer.Incomplete)
	}
	// EVERY TITLE IS SOMEBODY'S WORDS and no row says whose: neutralised, the
	// answer labelled as the company's writers' (ADR-0040). A group's label is a
	// value somebody gave the rows it gathers.
	read := newForeignRead(turn, actor.Handle)
	rowNames(read, answer.Rows)
	if len(answer.Groups) > 0 {
		namesBy(read)
	}
	return read.answer(ctx, ListWorkItemsTool, result, "Narrow what you asked for.")
}

// incompleteNote is what a model is told when the answer could not account for
// everything: words, not a flag, since the right response is a behaviour (say
// you could not check) and a boolean reads as metadata.
func incompleteNote(in *worklet.Incomplete) string {
	if in == nil {
		return "This node could not account for part of Worklet, so this " +
			"list may be missing items. Do not conclude something has not been " +
			"filed."
	}
	return fmt.Sprintf("This node holds %d record(s) it cannot read, affecting "+
		"this answer. The list may be missing items or showing stale ones — do "+
		"NOT conclude something has not been filed. Say you could not check.",
		in.Records)
}

// ---- get_work_item ----------------------------------------------------- //

type getWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*getWorkItem)(nil)

func (t *getWorkItem) Name() string { return GetWorkItemTool }

func (t *getWorkItem) Description() string {
	return "Read one work item: its description, status, assignee, labels, " +
		"links in both directions, the most recent comments and its recent " +
		"history. Comment bodies in the thread are EXCERPTS, ending in `…` " +
		"where one was cut — pass `comment` with that comment's id to read " +
		"it whole. Take `task.version` from the result and pass it back as " +
		"`if_match` on " + UpdateWorkItemTool + " to make your edit conditional. The " +
		"description and each comment arrive fenced as text somebody wrote; " +
		"the two fence lines are a label, not part of the item. " + flowSentence
}

func (t *getWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to read. " + itemRef,
			},
			"include": map[string]any{
				"type": "array",
				"description": "Which parts to read beside the item itself: " +
					"`body` (its whole description), `comments`, `history`, " +
					"`links`, `fields`. All five by default — name fewer when " +
					"you only need one, and the answer is smaller. Every part " +
					"comes back whole; an item too large to answer at once is " +
					"refused with a request to name fewer, never cut. `fields` " +
					"are the custom-field values with the slug, name and type " +
					"that explain each, which is what you write back with.",
				"items": map[string]any{
					"type": "string",
					"enum": stringsToAny(includeParts),
				},
			},
			"comments_cursor": map[string]any{
				"type": "string",
				"description": "The `comments_cursor` from a previous read, " +
					"to see the comments before that page.",
			},
			"comment": map[string]any{
				"type": "string",
				"description": "One comment's id, to read that comment ALONE. " +
					"Every comment on a thread page is already whole; this " +
					"is for going straight to one you were pointed at. On " +
					"its own it answers the item and that comment and " +
					"nothing else — name `include` as well if you also want " +
					"the history, the links or the fields.",
			},
		},
		"required": []any{"item"},
	}
}

// includeParts are the parts `include` names, in its schema and its reading
// ([argval.Choice]).
var includeParts = []string{"body", "comments", "history", "links", "fields"}

// detailWants reads the `include` argument; with none, every part.
//
// The description is a part like the others so each stays reachable WHOLE: a
// description and a comment are each capped at 32 KiB, and a read always
// carrying the description could refuse the narrowed call its refusal advised.
// A named `comment` is its own default (the item and that comment only); an
// explicit `include` still wins.
func detailWants(args map[string]any) (worklet.DetailWants, bool, string) {
	want := worklet.DetailWants{
		CommentCursor: strings.TrimSpace(argString(args, "comments_cursor")),
		Comment:       strings.TrimSpace(argString(args, "comment")),
	}
	raw, held := args["include"]
	if !held || raw == nil {
		if want.Comment != "" {
			return want, false, ""
		}
		want.Comments, want.History = true, true
		want.Links, want.Fields = true, true
		return want, true, ""
	}
	body := false
	parts, ok := refList(raw)
	if !ok {
		return want, false, GetWorkItemTool + " takes `include` as a list of parts: body, " +
			"comments, history, links, fields."
	}
	for _, part := range parts {
		// The one rule for a closed set ([argval.Choice]).
		member, _ := argval.Choice(part, includeParts)
		switch member {
		case "body":
			body = true
		case "comments":
			want.Comments = true
		case "history":
			want.History = true
		case "links":
			want.Links = true
		case "fields":
			want.Fields = true
		default:
			return want, false, fmt.Sprintf(GetWorkItemTool+" has no %q to "+
				"include. The parts are: body, comments, history, links, fields.",
				clip(part))
		}
	}
	return want, body, ""
}

func (t *getWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *getWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	// [WorkDeps.actor] rather than the turn — see [listWorkItems.CallForTurn].
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(GetWorkItemTool, err), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(GetWorkItemTool), nil
	}
	id := strings.TrimSpace(argString(args, "item"))
	if id == "" {
		return failed(GetWorkItemTool + " needs an `item` — a key like ENG-42, or an id."), nil
	}
	want, withBody, refusal := detailWants(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	// Who may read it is decided on the item: a tracker holder, or the person an
	// open ask on it names (workasked.go), known only once it is read; so a
	// caller the table does not admit has their asks probed BEFORE any part is
	// read. After the arguments, whose refusals reveal nothing about the board.
	holder, refused := t.deps.itemGate(ctx, authz.ActionWorkRead)
	if refused != nil {
		return *refused, nil
	}
	if !holder {
		if _, refused := t.deps.askedOn(ctx, authz.ActionWorkRead, id, actor.Handle,
			nil); refused != nil {
			return *refused, nil
		}
	}
	// The chart, so the answer carries the team's NAME beside its key. Passed
	// wherever a chart is held: an unresolved unit is a FINDING, and a surface
	// that could resolve and did not would report every task orphaned.
	want.Units = t.deps.Units
	// The company's clock, which the item's flow is cut on: without one the
	// detail carries no flow.
	want.Clock = &worklet.DayClock{Now: t.deps.now(), Zone: t.deps.zone()}
	detail, err := t.deps.Reader.Task(ctx, id, want, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q. Check the key, or "+
			"use "+ListWorkItemsTool+" to find it.", clip(id))), nil
	case errors.Is(err, worklet.ErrNoComment):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("Work item %q has no comment %q. Comment ids "+
			"come from the `comments` in a read of the item itself — drop "+
			"`comment` to see the thread.", clip(id), clip(want.Comment))), nil
	case errors.Is(err, worklet.ErrBadCursor):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused the "+
			"comments cursor: %s. Call it again without `comments_cursor` to read "+
			"the newest comments.", GetWorkItemTool, ReadRefusalSentence(err))), nil
	case err != nil:
		return readFailure(ctx, GetWorkItemTool, err), nil
	}
	if !t.deps.readsAtlas(ctx) {
		// THE ATLAS IS ITS HOLDERS': counted, never named (ADR-0140).
		detail = detail.WithoutEntries()
	}
	// The description is whole or absent, never cut: a model acts on a truncated
	// spec as the spec.
	answer := map[string]any{"task": detail}
	if !withBody && detail.Task.Body != "" {
		answer["body_not_included"] = fmt.Sprintf("The description (%d bytes) is "+
			"not in this answer; read it with include=[\"body\"].", len(detail.Task.Body))
		detail.Task.Body = ""
		answer["task"] = detail
	}
	if !detail.Complete {
		answer["incomplete"] = incompleteNote(detail.Incomplete)
	}
	narrow := "Ask for less with `include`: the parts are body, comments, " +
		"history, links and fields, and each on its own always fits."
	// The description and thread are somebody's words, fenced.
	read := newForeignRead(turn, actor.Handle)
	if len(answer) == 1 {
		fenceTask(read, nil, detail)
		return read.answer(ctx, GetWorkItemTool, detail, narrow)
	}
	fenceTask(read, fieldPath{}.key("task"), detail)
	return read.answer(ctx, GetWorkItemTool, answer, narrow)
}

// fenceTask fences what a work item read holds that somebody wrote: the
// description under its author, each comment under its own. The description's
// author is its last writer, so a body past its first version is labelled as
// having unnamed writers too ([fencePage]'s reason).
func fenceTask(read *foreignRead, at fieldPath, d worklet.TaskDetail) {
	var writers []provenance.Author
	partial := d.Task.BodyVersion > 1
	if author := strings.TrimSpace(d.Task.BodyAuthor); author != "" {
		writers = append(writers, read.writer(author, ""))
	} else {
		partial = true
	}
	read.fence(at.key("task").key("body"), provenance.SourceWorkDescription, writers,
		partial, d.Task.Body)
	for i, c := range d.Comments {
		author := read.writer(c.Author, iam.ActorKind(c.AuthorKind))
		read.fence(at.key("comments").index(i).key("body"), provenance.SourceWorkComment,
			[]provenance.Author{author}, false, c.Body)
		if c.Decision != nil {
			// The ask's options are its author's.
			read.attribute([]provenance.Author{author}, false)
		}
	}
	// History excerpts are the changes' own text (a comment, a hand-off's
	// reason), each under who made it.
	for i, h := range d.History {
		fenceExcerpt(read, at.key("history").index(i).key("excerpt"),
			worklet.ChangeKind(h.Kind), h.Actor, h.ActorKind, h.Excerpt)
	}
	// Names: the title, linked titles and notes, checklists, custom field values
	// and what history changes moved; no row records their writer.
	if d.Task.Title != "" {
		namesBy(read)
	}
}
