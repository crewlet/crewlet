package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The native tracker's tool names, re-exported from the package that owns the
// vocabulary.
//
// DECLARED IN internal/tracker rather than here, because the tracker's own
// notification prompt names them — it tells a woken seat which tool to reach
// for — and a constant defined here and read there is an import cycle. The
// domain owns its vocabulary; this package implements it.
const (
	ListWorkItemsTool  = tracker.ListWorkItemsTool
	GetWorkItemTool    = tracker.GetWorkItemTool
	CreateWorkItemTool = tracker.CreateWorkItemTool
	UpdateWorkItemTool = tracker.UpdateWorkItemTool
	CommentOnWorkTool  = tracker.CommentOnWorkTool
)

// WorkTools are the five, so a caller registering them names one thing.
func WorkTools() []string { return tracker.Tools() }

// WorkWrites are the three that count as a DELIVERY.
//
// A turn woken by an assignment answers by moving the item, commenting on it,
// or filing the follow-up work — and the delivery gate has to know that, or
// such a turn is corrected and looped for having "done nothing". Reading is
// not delivering, which is why get and list are not here: a turn that only
// read is exactly the turn the gate exists to catch.
func WorkWrites() []string { return tracker.WriteTools() }

// WorkReader is what these tools need from the tracker's read side.
//
// TWO SHAPES, because they are two questions. A board answers "what is there"
// over many rows and returns what a card renders; one task is "tell me
// everything about this" and every part of it comes from a different table.
type WorkReader interface {
	Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error)
	Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
		level statelog.ReadLevel) (tracker.TaskDetail, error)
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
}

// WorkWriter is what these tools need from the tracker's write side.
//
// EVERY WRITE TAKES AN OPERATION ID, minted once per caller-visible operation
// and stable across every round and every retry. A regenerated id defeats the
// ledger for exactly the lost-acknowledgement case the ledger exists for: a
// rejected attempt cannot dedupe against itself, so the only case a stable id
// collapses is a retry after a copy that already landed.
type WorkWriter interface {
	CreateTask(ctx context.Context, opID string, task tracker.Task,
		notify *tracker.Notify) (tracker.WriteResult, error)
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch tracker.TaskPatch, notify *tracker.Notify) (tracker.WriteResult, error)
}

// WorkDeps are the tracker halves plus what a write needs to attribute itself.
type WorkDeps struct {
	Reader WorkReader

	// Writer resolves the tracker's write side FOR ONE ACTOR.
	//
	// A function rather than a value, because the tracker's rule is that a
	// writer acts as exactly one party — a history row whose author was
	// chosen by the caller is not an audit trail. A surface serving many
	// parties takes one writer per party, derived from that surface's own
	// immutable identity: the seat bound into the turn context here, and
	// the credential on the request in the operator's surface.
	Writer func(actor Actor) WorkWriter

	// ViewWriter resolves the saved-view write side for one actor, in the
	// same shape and for the same reason [WorkDeps.Writer] is a function.
	//
	// SEPARATE from Writer because the surfaces differ: every surface has
	// a task writer and only the OPERATOR's has a view writer, so folding
	// the verb into one interface would make a seat's registration
	// implement a method nothing there may call.
	ViewWriter func(actor Actor) ViewWriter

	// Mentions resolves the handles a comment names, so a mention wakes
	// the person the author meant. Nil resolves nothing, which degrades to
	// a comment that notifies only the ordinary watchers.
	Mentions MentionResolver

	// DefaultProject is where a seat files work that names no project —
	// its unit's. Empty makes the project argument required, which is the
	// honest state for a seat whose unit owns none.
	DefaultProject func(handle string) string

	// Actor decides who a write is attributed to.
	//
	// NIL TAKES THE TURN'S SEAT, which is every in-engine caller and the
	// only correct answer there: a model that could name its own actor
	// could file work as anybody, so the seat comes from the immutable
	// turn context the tool surface bound.
	//
	// The OPERATOR surface sets it, because there is no turn there and the
	// writer is a person's credential rather than a seat. That is the
	// whole reason it is a seam and not a second copy of these five tools:
	// two implementations of "file an item" would drift on the parts that
	// matter least visibly — which fields are trimmed, which default
	// applies, what a refusal says — and only one of them would be tested.
	//
	// It takes the CONTEXT as well as the turn because the two callers
	// carry identity in different places: a turn's seat is on the turn,
	// and an operator's credential is on the request's context. A seam
	// that took only the turn would have forced the operator surface to
	// stash the caller in a package variable, which is one identity for
	// every concurrent request.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Leads resolves the two fallbacks a wake may need — a project's lead
	// and a unit's. Nil carries neither, which degrades to a change that
	// reaches the people already on the task and nobody else.
	Leads tracker.Leads

	// Now is the clock a query's relative dates resolve against, and the
	// zone they resolve in. Injected because "due this week" is a calendar
	// boundary, and a boundary read from a wall clock in the wrong zone
	// names a different week.
	Now  func() time.Time
	Zone *time.Location

	// Await blocks until this node's applier has consumed a write.
	//
	// THE READ-YOUR-WRITES SEAM, and it is what makes a tool loop
	// coherent: a write goes to the fleet's LOG while every read goes to
	// this node's own rows, so a turn that files a task and then lists its
	// project would not see what it just filed — and a model that cannot
	// see its own write files it again. It is applied after every write
	// here for that reason, never before a read.
	//
	// It takes a POSITION rather than a revision, because that is what a
	// log write answers with: a place on a stream, comparable only against
	// the same stream and the same generation.
	//
	// Nil skips the wait, which is right for a caller that has established
	// the ordering some other way. A failure is LOGGED AND IGNORED rather
	// than failing the tool: the write landed, and telling a model its
	// create failed when the task exists is the one answer that produces a
	// duplicate.
	Await func(ctx context.Context, at statelog.Position) error
}

// Actor is who a write is attributed to.
//
// THE TOOL LAYER'S OWN TYPE, small on purpose: the tracker's record carries
// the same four values as separate fields, and a struct shared with it would
// make every caller of these tools depend on the record format.
type Actor struct {
	// Handle is the record's AUTHOR, and it is never empty: every history
	// row names who wrote it, and one that does not is a row nobody can
	// attribute. For a seat it is the handle; for an operator it is the
	// token's own name, and [Actor.Kind] is what says which — a renderer
	// deciding from the string alone would show a credential as a
	// colleague.
	Handle string
	Kind   tracker.AuthorKind

	// OperatorID is the credential the write was made under, recorded
	// BESIDE the author rather than instead of it: a person acting through
	// a token is attributed to the person, and the token is how an audit
	// answers "what did this credential do".
	OperatorID string

	TurnID string
	Chain  []string
}

// settle waits for a write to reach this node's projection.
//
// Best effort by design — see [WorkDeps.Await]. The wait is bounded by the
// projector's own budget, so a wedged projection costs a tool call a couple
// of seconds rather than the turn.
func (d WorkDeps) settle(ctx context.Context, at statelog.Position) {
	if d.Await == nil || at.Seq == 0 {
		return
	}
	if err := d.Await(ctx, at); err != nil {
		log.WarnContext(ctx, "work_write_not_applied_yet",
			"position", at.String(), "error", err.Error(),
			"detail", "the write landed on the fleet's log; this node's own "+
				"applier has not consumed it, so a list in this same turn may "+
				"not show it yet")
	}
}

// MentionResolver turns the handles a comment's text names into seats.
type MentionResolver interface {
	// Mentions returns the handles this text addresses that are seats
	// here. Anything else is dropped: a comment naming an outsider must
	// not produce a notification nobody can deliver.
	Mentions(text string) []string
}

// ---- the seat identity every write is attributed to -------------------- //

// actorFor builds the write's attribution from the TURN, never from arguments.
//
// A model that could name its own actor could file work as anybody, so the
// seat comes from the immutable turn context the tool surface bound. The turn
// id and chain travel as provenance so an audit can walk from an item back to
// the turn that wrote it; they bound nothing, because a hand-off is charged
// to the item's own reassignment counter and not to the delegation depth.
func actorFor(turn *turnctx.Turn) (Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return Actor{}, err
	}
	return Actor{
		Handle: seat.Handle(),
		Kind:   tracker.AuthorAgent,
		TurnID: turn.ID,
		Chain:  turn.Chain,
	}, nil
}

// actor resolves who this call writes as — see [WorkDeps.Actor].
func (d WorkDeps) actor(ctx context.Context, turn *turnctx.Turn) (Actor, error) {
	if d.Actor != nil {
		return d.Actor(ctx, turn)
	}
	return actorFor(turn)
}

// turnKey is the idempotency key a comment carries, or "" outside a turn.
//
// NIL-SAFE, because these tools serve two callers now. A TURN's key makes a
// comment idempotent: the engine's redelivery guarantees make a re-run turn
// ordinary, and without it a seat says the same thing twice. An OPERATOR has
// no turn and no redelivery — their MCP client made one call — so there is
// nothing to deduplicate against and an invented key would be a lie about
// what produced the comment.
func turnKey(turn *turnctx.Turn) string {
	if turn == nil {
		return ""
	}
	return turn.ID
}

// notInATurn is the refusal every one of these tools gives outside a turn.
func notInATurn(name string) tools.Result {
	return failed(name + " can only be called during a turn, on behalf of a seat.")
}

// unconfigured is the refusal when the company runs no native tracker.
func unconfigured(name string) tools.Result { return failed(unconfiguredText(name)) }

func unconfiguredText(name string) string {
	return name + " is unavailable: this company does not run the native " +
		"work tracker. Use the tracker tools your company has configured."
}

// ---- list_work_items --------------------------------------------------- //

type listWorkItems struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listWorkItems)(nil)

func (t *listWorkItems) Name() string { return ListWorkItemsTool }

func (t *listWorkItems) Description() string {
	return "List work items on the company's tracker, filtered. Use this to " +
		"see what you are assigned, what is open in a project, or whether " +
		"something has already been filed before you file it again. Returns " +
		"summaries — call get_work_item for one item's full description, " +
		"comments and links."
}

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
				"description": "Statuses to include: " + statusList() + ".",
			},
			"open_only": map[string]any{
				"type": "boolean",
				"description": "True lists only items that are not done or " +
					"cancelled. Default false, which lists everything.",
			},
			"label": map[string]any{"type": "string", "description": "One label to filter on."},
			"text": map[string]any{
				"type": "string",
				"description": "Substring of the key or title. For finding an " +
					"item you half remember; use search_knowledge for a " +
					"question about the company's written knowledge.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).", tracker.PageMax, tracker.PageDefault),
			},
		},
	}
}

func (t *listWorkItems) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listWorkItems) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	if _, err := turn.RequireSeat(); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return notInATurn(ListWorkItemsTool), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(ListWorkItemsTool), nil
	}

	// THE TOOL'S ARGUMENTS ARE THE QUERY GRAMMAR'S OWN KEYS, translated
	// once here rather than parsed a second time. One grammar serves the
	// board, the socket, the REST route and this tool — and a second
	// parser for the model would be the one place a filter meant
	// something slightly different.
	params := map[string]any{}
	if v := strings.TrimSpace(argString(args, "project")); v != "" {
		params["container"] = "project:" + strings.ToUpper(v)
	}
	for _, key := range []string{"assignee", "text", "limit"} {
		if v, held := args[key]; held {
			params[key] = v
		}
	}
	if v := strings.TrimSpace(argString(args, "label")); v != "" {
		params["tag"] = v
	}
	if names := argStrings(args, "status"); len(names) > 0 {
		params["status"] = strings.Join(names, ",")
	}
	if open, held := args["open_only"].(bool); held && open {
		params["status_group"] = "not_started,in_progress,blocked"
	}

	q, err := tracker.ParseQuery(queries.FromMap(params), t.deps.now(), t.deps.zone())
	if err != nil {
		return failed(fmt.Sprintf("That filter is not one the tracker accepts: %v", err)), nil
	}
	// THIS SURFACE'S OWN DEFAULT, and it matches the detail read beside
	// it: a seat reads its own writes, so `session` is what stops a turn
	// filing a duplicate of the item it just created. A model that names a
	// level explicitly keeps it.
	if q.Level == "" {
		q.Level = statelog.ReadSession
	}
	answer, err := t.deps.Reader.Tasks(ctx, q, t.deps.now())
	if err != nil {
		return failed(readFailure(ListWorkItemsTool, err)), nil
	}
	if len(answer.Rows) == 0 && answer.Complete {
		return tools.Result{Output: "No work items match that filter."}, nil
	}
	result := map[string]any{"count": len(answer.Rows), "items": answer.Rows}
	if answer.TotalHint > len(answer.Rows) {
		result["total"] = answer.TotalHint
	}
	if !answer.Complete {
		// AN INCOMPLETE ANSWER SAYS SO, in the result the model reads.
		// The alternative is a seat concluding the company has no work
		// from a node that simply could not read some of it — and acting
		// on that by filing a duplicate.
		result["incomplete"] = incompleteNote(answer.Incomplete)
	}
	return jsonResult(result)
}

// incompleteNote is what a model is told when the answer could not account
// for everything.
//
// IN WORDS RATHER THAN A FLAG, because the model's correct response is a
// behaviour — say you could not check — and a boolean beside a list of rows
// reads as metadata rather than as a caveat about the rows.
func incompleteNote(in *tracker.Incomplete) string {
	if in == nil {
		return "This node could not account for part of the tracker, so this " +
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
	return "Read one work item in full: its description, status, assignee, " +
		"labels, links in both directions, its whole comment thread and its " +
		"recent history. Take the `revision` from the result and pass it back " +
		"as `if_match` on update_work_item to make your edit conditional."
}

func (t *getWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item key (ENG-42) or its id.",
			},
		},
		"required": []any{"item"},
	}
}

func (t *getWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *getWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	if _, err := turn.RequireSeat(); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(GetWorkItemTool), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(GetWorkItemTool), nil
	}
	id := strings.TrimSpace(argString(args, "item"))
	if id == "" {
		return failed("get_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	detail, err := t.deps.Reader.Task(ctx, id, tracker.DetailWants{
		Comments: true, History: true, Links: true,
	}, statelog.ReadSession)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q. Check the key, or "+
			"use list_work_items to find it.", clip(id))), nil
	case err != nil:
		return failed(readFailure(GetWorkItemTool, err)), nil
	}
	if !detail.Complete {
		return jsonResult(map[string]any{
			"task": detail, "incomplete": incompleteNote(detail.Incomplete),
		})
	}
	return jsonResult(detail)
}

// ---- create_work_item -------------------------------------------------- //

type createWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*createWorkItem)(nil)

func (t *createWorkItem) Name() string { return CreateWorkItemTool }

func (t *createWorkItem) Description() string {
	return "File a new work item. Search with list_work_items first — a " +
		"duplicate costs somebody a triage turn. Leave the assignee empty " +
		"and the item lands in triage, where the team's lead is told about " +
		"it; name an assignee and it goes straight to their queue."
}

func (t *createWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "One line saying what the work is.",
			},
			"body": map[string]any{
				"type": "string",
				"description": "The full description in markdown: what is " +
					"wanted, why, and how anyone would know it is done.",
			},
			"type": map[string]any{
				"type": "string",
				"description": "A task type from your workspace's own " +
					"catalogue. `task` if you are unsure.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "The project key. Defaults to your team's.",
			},
			"assignee": map[string]any{
				"type": "string",
				"description": "A seat's handle, from lookup_colleague. Omit " +
					"to leave it in triage for the lead to route.",
			},
			"priority": map[string]any{
				"type":        "string",
				"description": "One of: " + priorityList() + ". Default none.",
			},
			"parent": map[string]any{
				"type":        "string",
				"description": "The id or key of the item this belongs under.",
			},
			"labels": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []any{"title"},
	}
}

func (t *createWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *createWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(CreateWorkItemTool), nil
	}
	if t.deps.Writer == nil {
		return unconfigured(CreateWorkItemTool), nil
	}
	writer := t.deps.Writer(actor)

	now := t.deps.now()
	task := tracker.Task{
		V:           tracker.DocumentVersion,
		ID:          uuid.NewString(),
		Title:       strings.TrimSpace(argString(args, "title")),
		Body:        argString(args, "body"),
		Type:        strings.TrimSpace(argString(args, "type")),
		Project:     strings.ToUpper(strings.TrimSpace(argString(args, "project"))),
		Assignee:    strings.TrimSpace(argString(args, "assignee")),
		Reporter:    actor.Handle,
		Priority:    tracker.Priority(strings.TrimSpace(argString(args, "priority"))),
		Status:      tracker.StatusTodo,
		StatusGroup: tracker.GroupNotStarted,
		Rank:        tracker.RankOrigin,
		Tags:        argStrings(args, "labels"),
		CreatedAt:   now,
		UpdatedAt:   now,
		BodyAuthor:  actor.Handle,
		BodyAt:      now,
	}
	if task.Title == "" {
		return failed("create_work_item needs a `title` — one line saying what " +
			"the work is."), nil
	}
	if task.Type == "" {
		task.Type = tracker.DefaultType
	}
	if task.Priority == "" {
		task.Priority = tracker.PriorityNone
	}
	if !task.Priority.Valid() {
		return failed(fmt.Sprintf("%q is not a priority. The priorities are: %s.",
			clip(string(task.Priority)), priorityList())), nil
	}
	if ref := strings.TrimSpace(argString(args, "parent")); ref != "" {
		parent, refusal := t.deps.resolveRef(ctx, CreateWorkItemTool, "`parent`", ref)
		if refusal != "" {
			return failed(refusal), nil
		}
		task.Parent = &parent
	}
	if task.Project == "" {
		if t.deps.DefaultProject != nil {
			task.Project = t.deps.DefaultProject(actor.Handle)
		}
		if task.Project == "" {
			return failed("create_work_item needs a `project`: your team owns " +
				"none, so there is no default. Ask which project this belongs " +
				"in rather than guessing."), nil
		}
	}
	// THE REPORTER WATCHES WHAT THEY FILED, and so does the assignee. Set
	// at the write rather than derived at the wake: a watcher list built
	// later would be built from a row that has moved on.
	task.Watchers = handles(actor.Handle, task.Assignee)

	notify := tracker.Wake{
		Kind: tracker.ChangeCreated, After: task,
	}.Notify(t.deps.Leads)
	got, err := writer.CreateTask(ctx, opIDFor(actor, "create", task.ID), task, notify)
	if err != nil {
		return failed(writeFailure(CreateWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(map[string]any{
		"key": got.Key, "id": task.ID, "status": task.Status,
		"assignee": task.Assignee, "outcome": string(got.Outcome),
		"version": got.Version,
	})
}

// resolveRef turns what a model typed — a key like ENG-7, or an id — into the
// task ID every relation and every parent pointer is written with.
//
// # Why this is not optional plumbing
//
// [tracker.Relation.Other] and [tracker.Task.Parent] are IDs: the applier
// derives a subtree's root and depth from the parent pointer, and the relation
// duty writes a mirror edge onto the task the id names. Storing a KEY in
// either produces an edge that resolves to nothing on every node forever — the
// mirror is never written, the duty retries it until it gives up, and the item
// renders a link to a task that does not exist. A model types a key, because a
// key is what it read; the resolution is therefore the tool's job.
//
// It returns the model-facing refusal rather than an error, because every
// caller here answers a model rather than a process.
func (d WorkDeps) resolveRef(ctx context.Context, tool, field, ref string) (string, string) {
	if d.Reader == nil {
		return "", unconfiguredText(tool)
	}
	got, err := d.Reader.Task(ctx, ref, tracker.DetailWants{}, statelog.ReadSession)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return "", fmt.Sprintf("%s names %s %q and there is no such work item. "+
			"Check the key with list_work_items rather than guessing — a link "+
			"to an item that does not exist renders as a dead reference on "+
			"everybody's board.", tool, field, clip(ref))
	case err != nil:
		return "", readFailure(tool, err)
	}
	return got.Task.ID, ""
}

// handles is a de-duplicated, order-preserving list with the empties dropped.
func handles(all ...string) []string {
	var out []string
	for _, handle := range all {
		if handle = strings.TrimSpace(handle); handle != "" && !slices.Contains(out, handle) {
			out = append(out, handle)
		}
	}
	return out
}

// opIDFor is the operation id one tool call writes under.
//
// DERIVED FROM THE TURN AND THE OBJECT rather than minted fresh, so a re-run
// turn — which the engine's redelivery guarantees make ordinary — writes ONCE.
// Outside a turn there is nothing to be idempotent against and the object's own
// id is enough to make it unique.
func opIDFor(actor Actor, verb, object string) string {
	if actor.TurnID == "" {
		return verb + "-" + object
	}
	return actor.TurnID + "-" + verb + "-" + object
}

// ---- update_work_item -------------------------------------------------- //

type updateWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*updateWorkItem)(nil)

func (t *updateWorkItem) Name() string { return UpdateWorkItemTool }

func (t *updateWorkItem) Description() string {
	return "Change a work item: its status, assignee, priority, title, " +
		"description, labels, or whether you watch it. Only the fields you " +
		"pass are changed. Closing as `done` or `cancelled` takes a " +
		"`close_reason`; closing as a duplicate must name the item that " +
		"survives."
}

func (t *updateWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item key (ENG-42) or its id.",
			},
			"status":   map[string]any{"type": "string", "description": "One of: " + statusList() + "."},
			"assignee": map[string]any{"type": "string", "description": "A seat's handle, or \"\" to unassign."},
			"priority": map[string]any{"type": "string", "description": "One of: " + priorityList() + "."},
			"title":    map[string]any{"type": "string"},
			"body":     map[string]any{"type": "string", "description": "Replaces the description."},
			"labels": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Replaces the whole label set.",
			},
			"duplicate_of": map[string]any{
				"type": "string",
				"description": "Closing this as a duplicate: the item that " +
					"survives. Set the status as well — the link records WHY, " +
					"and the status records that it is closed.",
			},
			"watch": map[string]any{
				"type": "boolean",
				"description": "True to follow this item, false to stop. " +
					"Stopping sticks: you will not be re-subscribed by being " +
					"assigned it, though a direct @-mention still reaches you.",
			},
			"if_match": map[string]any{
				"type": "integer",
				"description": "The `revision` from get_work_item. Given, the " +
					"edit is REFUSED if anybody changed the item since you " +
					"read it. Omitted, your fields are merged onto the " +
					"current item — which is usually what you want.",
			},
		},
		"required": []any{"item"},
	}
}

func (t *updateWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *updateWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(UpdateWorkItemTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(UpdateWorkItemTool), nil
	}
	writer := t.deps.Writer(actor)

	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed("update_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, statelog.ReadSession)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(UpdateWorkItemTool, err)), nil
	}

	var duplicateOf string
	if ref := strings.TrimSpace(argString(args, "duplicate_of")); ref != "" {
		var refusal string
		if duplicateOf, refusal = t.deps.resolveRef(ctx, UpdateWorkItemTool,
			"`duplicate_of`", ref); refusal != "" {
			return failed(refusal), nil
		}
	}
	patch, kind, refusal := patchFromArgs(args, actor, duplicateOf)
	if refusal != "" {
		return failed(refusal), nil
	}
	// IF-MATCH IS THE MODEL'S OWN PRECONDITION, passed through rather than
	// derived: omitted it merges, which is what a model naming two fields
	// wants, and given it refuses if anybody moved the task since the read
	// the model reasoned from.
	ifMatch := uint64(max(argInt(args, "if_match", 0), 0))
	got, err := writer.UpdateTask(ctx,
		opIDFor(actor, "update", before.Task.ID), before.Task.ID,
		before.Task.Project, ifMatch, patch,
		tracker.Wake{
			Kind:   kind,
			Before: before.Task,
			After:  patched(before.Task, patch),
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(UpdateWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(map[string]any{
		"key": before.Task.Key, "outcome": string(got.Outcome),
		"version": got.Version,
	})
}

// patchFromArgs builds the patch and the change kind, or the refusal to show
// the model.
//
// THE KIND IS DECIDED HERE and not derived from the patch, because a change
// with several fields in it still has ONE thing it is about: a status move
// that also set an assignee is a status change with an assignee delta beside
// it, and a recipient told "fields changed" would have to read the deltas to
// find out what happened. The order below is that judgement, most specific
// first.
func patchFromArgs(args map[string]any, actor Actor,
	duplicateOf string) (tracker.TaskPatch, tracker.ChangeKind, string) {

	var patch tracker.TaskPatch
	kind := tracker.ChangeFields

	if v, held := args["title"]; held {
		title := strings.TrimSpace(argString(map[string]any{"v": v}, "v"))
		patch.Title = &title
	}
	if _, held := args["body"]; held {
		body := argString(args, "body")
		patch.Body = &body
	}
	if _, held := args["assignee"]; held {
		assignee := strings.TrimSpace(argString(args, "assignee"))
		patch.Assignee = &assignee
		kind = tracker.ChangeAssignee
	}
	if raw := strings.TrimSpace(argString(args, "priority")); raw != "" {
		priority := tracker.Priority(raw)
		if !priority.Valid() {
			return patch, kind, fmt.Sprintf("%q is not a priority. The "+
				"priorities are: %s.", clip(raw), priorityList())
		}
		patch.Priority = &priority
		kind = tracker.ChangePrioritised
	}
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := tracker.Status(raw)
		if !status.Valid() {
			return patch, kind, fmt.Sprintf("%q is not a status. The statuses "+
				"are: %s.", clip(raw), statusList())
		}
		patch.Status = &status
		kind = tracker.ChangeStatus
	}
	if _, held := args["labels"]; held {
		labels := argStrings(args, "labels")
		patch.Tags = &labels
		if kind == tracker.ChangeFields {
			kind = tracker.ChangeTags
		}
	}
	if watch, held := args["watch"].(bool); held {
		// WATCHING IS A COLLECTION WRITE, carried whole: the mute travels
		// with it, because a replay that saw only the watcher list could
		// not tell "not a watcher" from "watching but muted" and would
		// silently re-add every unwatched person on the next mention.
		patch.Watchers = &[]string{actor.Handle}
		patch.Muted = &[]string{}
		if !watch {
			patch.Watchers = &[]string{}
			patch.Muted = &[]string{actor.Handle}
		}
		if kind == tracker.ChangeFields {
			kind = tracker.ChangeWatchers
		}
	}
	if duplicateOf != "" {
		patch.Relations = &[]tracker.Relation{{
			Kind: tracker.RelationDuplicates, Other: duplicateOf,
			CreatedBy: actor.Handle,
		}}
		if kind == tracker.ChangeFields {
			kind = tracker.ChangeRelations
		}
	}
	return patch, kind, ""
}

// patched is the task as the write will leave it, for the wake's snapshot.
//
// APPLIED HERE RATHER THAN READ BACK, because the snapshot has to describe the
// state this change produces and the change has not landed yet — a read after
// the write would race every other writer, and on a lagging node would return
// the state before it.
func patched(task tracker.Task, patch tracker.TaskPatch) tracker.Task {
	if patch.Title != nil {
		task.Title = *patch.Title
	}
	if patch.Body != nil {
		task.Body = *patch.Body
	}
	if patch.Assignee != nil {
		task.Assignee = *patch.Assignee
	}
	if patch.Status != nil {
		task.Status = *patch.Status
		task.StatusGroup = patch.Status.Group()
	}
	if patch.Priority != nil {
		task.Priority = *patch.Priority
	}
	if patch.Tags != nil {
		task.Tags = *patch.Tags
	}
	if patch.Watchers != nil {
		task.Watchers = *patch.Watchers
	}
	if patch.Muted != nil {
		task.Muted = *patch.Muted
	}
	return task
}

// ---- comment_on_work_item ---------------------------------------------- //

type commentOnWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*commentOnWorkItem)(nil)

func (t *commentOnWorkItem) Name() string { return CommentOnWorkTool }

func (t *commentOnWorkItem) Description() string {
	return "Post a comment on a work item. Everyone following the item is " +
		"told, and anyone you @-mention by handle is woken specifically. " +
		"Post ONE substantive comment when you have something to say — " +
		"running commentary is noise on a surface other people read."
}

func (t *commentOnWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item key (ENG-42) or its id.",
			},
			"body": map[string]any{
				"type": "string",
				"description": "The comment, in markdown. @-mention a " +
					"colleague by handle to reach them specifically.",
			},
			"reply_to": map[string]any{
				"type":        "string",
				"description": "The id of the comment you are answering.",
			},
		},
		"required": []any{"item", "body"},
	}
}

func (t *commentOnWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *commentOnWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(CommentOnWorkTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(CommentOnWorkTool), nil
	}
	writer := t.deps.Writer(actor)

	ref := strings.TrimSpace(argString(args, "item"))
	body := strings.TrimSpace(argString(args, "body"))
	switch {
	case ref == "":
		return failed("comment_on_work_item needs an `item` — a key like ENG-42, or an id."), nil
	case body == "":
		return failed("comment_on_work_item needs a `body`. Say the substantive thing, once."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, statelog.ReadSession)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(CommentOnWorkTool, err)), nil
	}

	now := t.deps.now()
	comment := &tracker.Comment{
		// THE ID IS DERIVED FROM THE OPERATION, not minted fresh, so a
		// re-run turn posts once rather than saying the same thing
		// twice. The engine's redelivery guarantees make a re-run turn
		// ordinary rather than exceptional.
		ID:         commentID(actor, before.Task.ID),
		Task:       before.Task.ID,
		Author:     actor.Handle,
		AuthorKind: actor.Kind,
		Body:       body,
		CreatedAt:  now,
	}
	if reply := strings.TrimSpace(argString(args, "reply_to")); reply != "" {
		comment.ReplyTo = &reply
	}
	if t.deps.Mentions != nil {
		comment.Mentions = t.deps.Mentions.Mentions(body)
	}

	// A COMMENT RIDES THE TASK'S OWN WRITE, because a comment is a
	// mutation of the task and shares its arbitration: two people
	// commenting at once contend at the broker on one subject, and exactly
	// one wins a round.
	after := before.Task
	after.Watchers = handles(append(slices.Clone(after.Watchers), actor.Handle)...)
	got, err := writer.UpdateTask(ctx,
		opIDFor(actor, "comment", comment.ID), before.Task.ID, before.Task.Project,
		// A COMMENT NEVER CONDITIONS ON A VERSION: it adds to the thread
		// rather than replacing anybody's value, so there is nothing a
		// concurrent edit could make it clobber.
		tracker.NoIfMatch,
		tracker.TaskPatch{Comment: comment, Watchers: &after.Watchers},
		tracker.Wake{
			Kind: tracker.ChangeComment, Before: before.Task, After: after,
			Comment: comment, Mentions: comment.Mentions,
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(CommentOnWorkTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(map[string]any{
		"comment_id": comment.ID, "item": before.Task.Key,
		"mentioned": comment.Mentions, "outcome": string(got.Outcome),
		"version": got.Version,
	})
}

// commentID is the comment's own id, derived so a re-run turn posts once.
//
// A UUIDv5 over the operation and the task, because the id is a PRIMARY KEY on
// every node: two nodes applying one record must write one row, so an id
// generated at apply time would produce two.
func commentID(actor Actor, taskID string) string {
	seed := actor.TurnID
	if seed == "" {
		seed = uuid.NewString()
	}
	return uuid.NewSHA1(commentNamespace, []byte(seed+"\x00"+taskID+"\x00"+actor.Handle)).String()
}

// commentNamespace is the uuid namespace comment ids are derived under. Fixed
// for the life of the format: it is durable in every comment row.
var commentNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// ---- shared rendering -------------------------------------------------- //

// jsonResult renders a value as the tool's output.
//
// JSON rather than prose, because these results are STRUCTURED — a board, an
// item, a set of ids — and a model asked to parse a rendered table back into
// fields gets it wrong in ways that are invisible until it acts on them.
func jsonResult(v any) (tools.Result, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return failed("The result could not be rendered."), nil
	}
	return tools.Result{Output: string(data)}, nil
}

// readFailure explains a read that could not be served.
//
// IT NEVER SAYS "NOTHING FOUND". A projection that has not caught up must not
// be able to tell a seat the company has no work — it would file a duplicate,
// or abandon work it was told to do.
func readFailure(name string, err error) string {
	return fmt.Sprintf("%s could not read the tracker right now (%v). This is "+
		"NOT an empty result — do not conclude the item or the list does not "+
		"exist. Try again, or say you could not check.", name, err)
}

// writeFailure explains a write that did not land, in terms the model can act
// on: which of these it can fix by trying differently, and which it cannot.
func writeFailure(name string, err error) string {
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return fmt.Sprintf("%s: %v", name, err)
	case errors.Is(err, statelog.ErrConflict):
		return fmt.Sprintf("%s could not land: %v. Somebody else is editing "+
			"this item. Read it again with get_work_item and decide from what "+
			"it says now.", name, err)
	case errors.Is(err, statelog.ErrExists):
		return fmt.Sprintf("%s: %v", name, err)
	case errors.Is(err, tracker.ErrStaleVersion):
		// THE ONE REFUSAL WHOSE ANSWER IS "READ IT AGAIN". Nothing is
		// wrong with the patch, so a model told only "it failed" would
		// re-send the same one against the same moved item forever.
		return fmt.Sprintf("%s was refused: %v. Nothing is wrong with your "+
			"edit — somebody changed the item after you read it. Call "+
			"get_work_item again and decide from what it says now.", name, err)
	case errors.Is(err, tracker.ErrReassignmentBudget):
		// THE REFUSAL THAT MUST NOT INVITE ANOTHER ATTEMPT: this item is
		// circulating between agents, and a message that reads like a
		// transient failure is exactly what keeps it circulating.
		return fmt.Sprintf("%s was refused: %v. Do not reassign it again. "+
			"Comment on the item saying what is blocking it and who you "+
			"think should own it, and leave it where it is.", name, err)
	}
	return fmt.Sprintf("%s did not land (%v). The change was NOT made — do not "+
		"report it as done.", name, err)
}

// now and zone are the clock a query resolves against, defaulted here so a
// caller that supplied neither still gets calendar boundaries in UTC rather
// than a zero time nothing can compare.
func (d WorkDeps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now()
}

func (d WorkDeps) zone() *time.Location {
	if d.Zone == nil {
		return time.UTC
	}
	return d.Zone
}

func statusList() string   { return joinValues(tracker.Statuses) }
func priorityList() string { return joinValues(tracker.Priorities) }

// joinValues renders a closed set for a tool description.
func joinValues[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}
