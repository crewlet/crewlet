package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/work"
)

// The native tracker's tool names.
//
// BARE, not prefixed. A first-party tool may be named in a prompt — this
// build registers it under a name this build chose — and a prefix would cost
// tokens on every catalogue line for every seat to disambiguate from nothing.
// The registry refuses a duplicate, so an operator's MCP server shipping a
// tool by one of these names is reported rather than silently shadowing it.
const (
	ListWorkItemsTool  = "list_work_items"
	GetWorkItemTool    = "get_work_item"
	CreateWorkItemTool = "create_work_item"
	UpdateWorkItemTool = "update_work_item"
	CommentOnWorkTool  = "comment_on_work_item"
)

// WorkTools are the five, so a caller registering them names one thing.
func WorkTools() []string {
	return []string{ListWorkItemsTool, GetWorkItemTool, CreateWorkItemTool,
		UpdateWorkItemTool, CommentOnWorkTool}
}

// WorkWrites are the three that count as a DELIVERY.
//
// A turn woken by an assignment answers by moving the item, commenting on it,
// or filing the follow-up work — and the delivery gate has to know that, or
// such a turn is corrected and looped for having "done nothing". Reading is
// not delivering, which is why get and list are not here: a turn that only
// read is exactly the turn the gate exists to catch.
func WorkWrites() []string {
	return []string{CreateWorkItemTool, UpdateWorkItemTool, CommentOnWorkTool}
}

// WorkReader is what these tools need from the tracker's read side.
type WorkReader interface {
	List(ctx context.Context, f work.Filter) ([]work.Summary, error)
	Get(ctx context.Context, idOrKey string) (work.Detail, error)
}

// WorkWriter is what these tools need from the tracker's write side.
type WorkWriter interface {
	Create(ctx context.Context, actor work.Actor, in work.NewItem) (work.Written, error)
	Update(ctx context.Context, actor work.Actor, itemID string, ifMatch uint64, edit work.Edit) (work.Written, error)
	Comment(ctx context.Context, actor work.Actor, itemID string, in work.NewComment) (work.Comment, work.Written, error)
	Item(ctx context.Context, itemID string) (work.Item, uint64, error)
}

// WorkDeps are the tracker halves plus what a write needs to attribute itself.
type WorkDeps struct {
	Reader WorkReader
	Writer WorkWriter

	// Mentions resolves the handles a comment names, so a mention wakes
	// the person the author meant. Nil resolves nothing, which degrades to
	// a comment that notifies only the ordinary watchers.
	Mentions MentionResolver

	// DefaultProject is where a seat files work that names no project —
	// its unit's. Empty makes the project argument required, which is the
	// honest state for a seat whose unit owns none.
	DefaultProject func(handle string) string

	// Await blocks until this node's projection has applied a revision.
	//
	// THE READ-YOUR-WRITES SEAM, and it is what makes a tool loop
	// coherent: a write goes to the fleet's coordination record while
	// every read goes to this node's projection, so a turn that files an
	// item and then lists its project would not see what it just filed —
	// and a model that cannot see its own write files it again. It is
	// applied after every write here for that reason, never before a
	// read.
	//
	// Nil skips the wait, which is right for a caller that has
	// established the ordering some other way. A failure is LOGGED AND
	// IGNORED rather than failing the tool: the write landed, and telling
	// a model its create failed when the item exists is the one answer
	// that produces a duplicate.
	Await func(ctx context.Context, revision uint64) error
}

// settle waits for a write to reach this node's projection.
//
// Best effort by design — see [WorkDeps.Await]. The wait is bounded by the
// projector's own budget, so a wedged projection costs a tool call a couple
// of seconds rather than the turn.
func (d WorkDeps) settle(ctx context.Context, revision uint64) {
	if d.Await == nil || revision == 0 {
		return
	}
	if err := d.Await(ctx, revision); err != nil {
		log.WarnContext(ctx, "work_write_not_projected_yet",
			"revision", revision, "error", err.Error(),
			"detail", "the write landed on the fleet's record; this node's own "+
				"copy has not caught up, so a list in this same turn may not "+
				"show it yet")
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
func actorFor(turn *turnctx.Turn) (work.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return work.Actor{}, err
	}
	return work.Actor{
		Handle: seat.Handle(),
		Kind:   work.AuthorAgent,
		TurnID: turn.ID,
		Chain:  turn.Chain,
	}, nil
}

// notInATurn is the refusal every one of these tools gives outside a turn.
func notInATurn(name string) tools.Result {
	return failed(name + " can only be called during a turn, on behalf of a seat.")
}

// unconfigured is the refusal when the company runs no native tracker.
func unconfigured(name string) tools.Result {
	return failed(name + " is unavailable: this company does not run the native " +
		"work tracker. Use the tracker tools your company has configured.")
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
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).", work.MaxLimit, work.DefaultLimit),
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
	filter := work.Filter{
		Assignee: strings.TrimSpace(argString(args, "assignee")),
		Project:  strings.TrimSpace(argString(args, "project")),
		Label:    strings.TrimSpace(argString(args, "label")),
		Text:     strings.TrimSpace(argString(args, "text")),
		Limit:    argInt(args, "limit", 0),
	}
	for _, name := range argStrings(args, "status") {
		status := work.Status(strings.TrimSpace(name))
		if !status.Valid() {
			return failed(fmt.Sprintf("%q is not a status. The statuses are: %s.",
				clip(name), statusList())), nil
		}
		filter.Status = append(filter.Status, status)
	}
	if open, ok := args["open_only"].(bool); ok && open {
		filter.Open = &open
	}

	items, err := t.deps.Reader.List(ctx, filter)
	if err != nil {
		return failed(readFailure(ListWorkItemsTool, err)), nil
	}
	if len(items) == 0 {
		return tools.Result{Output: "No work items match that filter."}, nil
	}
	return jsonResult(map[string]any{"count": len(items), "items": items})
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
	detail, err := t.deps.Reader.Get(ctx, id)
	switch {
	case errors.Is(err, work.ErrNotFound):
		return failed(fmt.Sprintf("There is no work item %q. Check the key, or "+
			"use list_work_items to find it.", clip(id))), nil
	case err != nil:
		return failed(readFailure(GetWorkItemTool, err)), nil
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
				"type":        "string",
				"description": "One of: " + typeList() + ". Default task.",
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
	actor, err := actorFor(turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(CreateWorkItemTool), nil
	}
	if t.deps.Writer == nil {
		return unconfigured(CreateWorkItemTool), nil
	}

	in := work.NewItem{
		Title:    strings.TrimSpace(argString(args, "title")),
		Body:     argString(args, "body"),
		Type:     work.Type(strings.TrimSpace(argString(args, "type"))),
		Project:  strings.ToUpper(strings.TrimSpace(argString(args, "project"))),
		Assignee: strings.TrimSpace(argString(args, "assignee")),
		Priority: work.Priority(strings.TrimSpace(argString(args, "priority"))),
		ParentID: strings.TrimSpace(argString(args, "parent")),
		Labels:   argStrings(args, "labels"),
	}
	if in.Type == "" {
		in.Type = work.TypeTask
	}
	if in.Project == "" {
		if t.deps.DefaultProject != nil {
			in.Project = t.deps.DefaultProject(actor.Handle)
		}
		if in.Project == "" {
			return failed("create_work_item needs a `project`: your team owns " +
				"none, so there is no default. Ask which project this belongs " +
				"in rather than guessing."), nil
		}
	}

	got, err := t.deps.Writer.Create(ctx, actor, in)
	if err != nil {
		return failed(writeFailure(CreateWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Revision)
	return jsonResult(map[string]any{
		"key": got.Item.Key, "id": got.Item.ID, "status": got.Item.Status,
		"assignee": got.Item.Assignee, "revision": got.Revision,
	})
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
			"close_reason": map[string]any{
				"type":        "string",
				"description": "With a closing status: one of " + closeReasonList() + ".",
			},
			"duplicate_of": map[string]any{
				"type":        "string",
				"description": "With close_reason duplicate: the surviving item.",
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
	actor, err := actorFor(turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(UpdateWorkItemTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(UpdateWorkItemTool), nil
	}

	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed("update_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref)
	switch {
	case errors.Is(err, work.ErrNotFound):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(UpdateWorkItemTool, err)), nil
	}

	edit, refusal := editFromArgs(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	got, err := t.deps.Writer.Update(ctx, actor, detail.Item.ID,
		uint64(argInt(args, "if_match", 0)), edit)
	if err != nil {
		return failed(writeFailure(UpdateWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Revision)
	return jsonResult(map[string]any{
		"key": got.Item.Key, "status": got.Item.Status,
		"assignee": got.Item.Assignee, "revision": got.Revision,
	})
}

// editFromArgs builds the edit, or the refusal to show the model.
func editFromArgs(args map[string]any) (work.Edit, string) {
	var edit work.Edit
	if v, ok := args["title"]; ok {
		title := strings.TrimSpace(argString(map[string]any{"v": v}, "v"))
		edit.Title = &title
	}
	if _, ok := args["body"]; ok {
		body := argString(args, "body")
		edit.Body = &body
	}
	if _, ok := args["assignee"]; ok {
		assignee := strings.TrimSpace(argString(args, "assignee"))
		edit.Assignee = &assignee
	}
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := work.Status(raw)
		if !status.Valid() {
			return edit, fmt.Sprintf("%q is not a status. The statuses are: %s.",
				clip(raw), statusList())
		}
		edit.Status = &status
	}
	if raw := strings.TrimSpace(argString(args, "priority")); raw != "" {
		priority := work.Priority(raw)
		if !priority.Valid() {
			return edit, fmt.Sprintf("%q is not a priority. The priorities are: %s.",
				clip(raw), priorityList())
		}
		edit.Priority = &priority
	}
	if raw := strings.TrimSpace(argString(args, "close_reason")); raw != "" {
		reason := work.CloseReason(raw)
		if !reason.Valid() {
			return edit, fmt.Sprintf("%q is not a close reason. They are: %s.",
				clip(raw), closeReasonList())
		}
		edit.CloseReason = &reason
	}
	if raw := strings.TrimSpace(argString(args, "duplicate_of")); raw != "" {
		edit.DuplicateOf = &raw
	}
	if _, ok := args["labels"]; ok {
		labels := argStrings(args, "labels")
		edit.Labels = &labels
	}
	if watch, ok := args["watch"].(bool); ok {
		edit.Watch = &watch
	}
	return edit, ""
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
	actor, err := actorFor(turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(CommentOnWorkTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(CommentOnWorkTool), nil
	}

	ref := strings.TrimSpace(argString(args, "item"))
	body := strings.TrimSpace(argString(args, "body"))
	switch {
	case ref == "":
		return failed("comment_on_work_item needs an `item` — a key like ENG-42, or an id."), nil
	case body == "":
		return failed("comment_on_work_item needs a `body`. Say the substantive thing, once."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref)
	switch {
	case errors.Is(err, work.ErrNotFound):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(CommentOnWorkTool, err)), nil
	}

	in := work.NewComment{
		Body:    body,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
		// THE TURN'S OWN WORK KEY makes this comment idempotent: a re-run
		// turn — which the engine's redelivery guarantees make ordinary —
		// posts once rather than saying the same thing twice.
		TurnKey: turn.ID,
	}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}

	comment, written, err := t.deps.Writer.Comment(ctx, actor, detail.Item.ID, in)
	if err != nil {
		return failed(writeFailure(CommentOnWorkTool, err)), nil
	}
	t.deps.settle(ctx, written.Revision)
	return jsonResult(map[string]any{
		"comment_id": comment.ID, "item": detail.Item.Key,
		"mentioned": comment.Mentions, "revision": written.Revision,
	})
}

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
	case errors.Is(err, work.ErrInvalid):
		return fmt.Sprintf("%s refused that: %v", name, err)
	case errors.Is(err, work.ErrReassignmentBudget):
		return fmt.Sprintf("%v\n\nDo not reassign it again. Say what you know "+
			"in a comment so the person who picks it up has it.", err)
	case errors.Is(err, work.ErrStaleVersion):
		return fmt.Sprintf("%s was refused: %v. Read the item again with "+
			"get_work_item and decide from what it says now.", name, err)
	case errors.Is(err, work.ErrConflict):
		return fmt.Sprintf("%s could not land: %v. Somebody else is editing "+
			"this item. Read it again before retrying.", name, err)
	case errors.Is(err, work.ErrNotFound):
		return fmt.Sprintf("%s: %v", name, err)
	}
	return fmt.Sprintf("%s did not land (%v). The change was NOT made — do not "+
		"report it as done.", name, err)
}

func statusList() string      { return joinValues(work.Statuses()) }
func typeList() string        { return joinValues(work.Types()) }
func priorityList() string    { return joinValues(work.Priorities()) }
func closeReasonList() string { return joinValues(work.CloseReasons()) }

// joinValues renders a closed set for a tool description.
func joinValues[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}
