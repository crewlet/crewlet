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

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error)
	Goals(ctx context.Context, q tracker.GoalQuery) (tracker.GoalListing, error)
	Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error)
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
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
	// UpdateTask takes the change KIND beside the notification because
	// they are two facts: what happened, and who is told about it. A tool
	// that stated only the second left the record's kind to be guessed
	// from the operation whenever nobody was listening.
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch tracker.TaskPatch, kind tracker.ChangeKind,
		notify *tracker.Notify) (tracker.WriteResult, error)
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

	// GoalWriter resolves the goal write side for one actor, and is the
	// operator surface's alone for the same reason ViewWriter is.
	GoalWriter func(actor Actor) GoalWriter

	// CatalogueWriter resolves the workspace catalogue write side, and is
	// the operator surface's alone for the reason the two above are.
	CatalogueWriter func(actor Actor) CatalogueWriter

	// PersonWriter resolves the person write side for one actor. The
	// operator surface's alone, because a seat is not a human: it has a
	// mailbox rather than an inbox, and nothing on a person's record
	// describes one.
	PersonWriter func(actor Actor) PersonWriter

	// ProjectWriter resolves a project's own settings for one actor.
	//
	// EVERY SURFACE HAS ONE, unlike the five above it, because one of its
	// facets — declaring a tag — is open to every seat: a seat that could
	// not declare a label could never use the `labels` argument on the
	// create and update tools it already holds. The authority for the rest
	// is resolved per call rather than per surface.
	ProjectWriter func(actor Actor) ProjectWriter

	// SprintWriter resolves the sprint side for one actor, and is the
	// operator surface's alone: a sprint is a commitment a team made
	// together, so starting or closing one is a lead's decision rather
	// than a seat's.
	SprintWriter func(actor Actor) SprintWriter

	// TrashWriter resolves the removal and restore side for one actor, and
	// is the operator surface's alone: a removal hides a task from every
	// list in the company, and a seat that could hide work it did not want
	// to do would be marking its own homework in the one way that leaves
	// no trace — the board simply has one fewer item on it.
	TrashWriter func(actor Actor) TrashWriter

	// Seats is the company's roster, for validating a handle a caller
	// typed. Nil admits every handle, which is the honest state for a
	// surface with no chart loaded.
	//
	// A FUNCTION, resolved per call, for the reason [WorkDeps.Units] and
	// [WorkDeps.DefaultProject] are: a seat's tools are cloned into its
	// lease, an apply does not rebuild the clone, and a captured roster
	// would validate against an org that has since moved — refusing a
	// colleague who joined this morning and admitting one who left.
	Seats func() []colleague.Seat

	// Units resolves a project's chart-owned unit at READ time — the
	// tracker holds no org, because the applier may not read one. Nil
	// renders every unit unresolved, which is honest rather than empty.
	Units tracker.Units

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
			"preset": map[string]any{
				"type": "string",
				"enum": tracker.Presets,
				"description": "A saved question. `my_queue` is YOUR open " +
					"work, most important first; `blocked` is open work that " +
					"cannot move; `overdue` is open work past its due date. " +
					"Any other argument you pass overrides the preset's own.",
			},
			"label": map[string]any{"type": "string", "description": "One label to filter on."},
			"sprint": map[string]any{
				"type": "string",
				"description": "A sprint number, a sprint name, or one of " +
					"`active`, `next`, `future`, `closed` — or `none` for the " +
					"backlog, which is unfinished work in no sprint. Comma " +
					"separate to ask for several. Everything but `none` names " +
					"a sprint of ONE project, so pass `project` with it.",
			},
			"removed": map[string]any{
				"type": "boolean",
				"description": "True lists the TRASH — items somebody removed " +
					"— and nothing else. This is the only way to see them: a " +
					"removed item is out of every other list. They are not " +
					"destroyed and an operator can restore one at any age.",
			},
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
	// THE IDENTITY CHECK IS [WorkDeps.actor]'s, never turn.RequireSeat()
	// directly. A seat with no turn still refuses — the nil-Actor fallback
	// requires the seat itself — and the OPERATOR surface, which has no
	// turn and supplies its own actor, is answered rather than told it is
	// not in one. Asking the turn first made every read on /operator/mcp
	// refuse while the writes beside them worked.
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
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
	for _, key := range []string{"assignee", "limit", "sprint", "removed"} {
		if v, held := args[key]; held {
			params[key] = v
		}
	}
	// THE GRAMMAR'S TEXT KEY IS `q`, and the tool's argument is `text`
	// because that is the word a model reaches for. The two are translated
	// here rather than renamed at either end: the argument is the model's
	// vocabulary and `q` is the query string's, and copying `text` through
	// under its own name is how this tool spent its early life returning
	// an unfiltered list to every model that asked for one.
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
	if open, held := args["open_only"].(bool); held && open {
		// THE TWO OPEN GROUPS, named from the constants rather than
		// typed: there are FOUR status groups and neither `in_progress`
		// nor `blocked` is one of them, so a literal naming either is a
		// filter the parser refuses — which made `open_only` fail the
		// whole call rather than narrow it.
		params["status_group"] = strings.Join(openGroups(), ",")
	}

	// THROUGH THE EXPANSION, with the SEAT as the viewer — which is what
	// makes `preset=my_queue` mean this seat's own queue and not a queue
	// it could name. The seat comes from the immutable turn context for
	// the same reason every write here does: a model that could name its
	// own viewer could read as anybody.
	// THE VIEWER IS THE SEAT AND ITS OWN PROJECT, because `preset=my_queue`
	// asks two things about the reader: what they hold, and what is
	// unclaimed in THEIR container.
	q, err := t.deps.Reader.ExpandedQuery(ctx, params, tracker.Viewer{
		Handle: actor.Handle, Project: t.deps.defaultProject(actor.Handle),
	},
		t.deps.now(), t.deps.zone())
	if err != nil {
		return failed(fmt.Sprintf("That filter is not one the tracker accepts: %v", err)), nil
	}
	// THIS SURFACE'S OWN LEVEL, and it OVERRULES rather than fills in.
	// One query grammar serves the board, the socket, the REST route and
	// this tool, so a level can arrive here from a path that knew nothing
	// about which surface would answer — and the surface is the authority.
	// A guard that only filled an empty field would enforce nothing the
	// day something populated it; see [statelog.LevelFor].
	q.Level = statelog.LevelFor(statelog.SurfaceSeat, q.Level)
	answer, err := t.deps.Reader.Tasks(ctx, q, t.deps.now())
	switch {
	case errors.Is(err, tracker.ErrTooBroad):
		// NOT [readFailure], whose entire advice is "try again": this
		// refusal is about the QUERY rather than about the node, and the
		// same keys refuse again for ever. What the model needs is the
		// refusal's own sentence, which names what would narrow it.
		return failed(fmt.Sprintf("%s: %v", ListWorkItemsTool, err)), nil
	case err != nil:
		return failed(readFailure(ListWorkItemsTool, err)), nil
	}
	// A GROUPED ANSWER HAS NO FLAT ROWS BY CONSTRUCTION, so the empty
	// message has to ask about the groups too — a board with five columns
	// reported as "no work items match" is a seat about to file the
	// duplicate.
	if len(answer.Rows) == 0 && len(answer.Groups) == 0 && answer.Complete {
		return tools.Result{Output: "No work items match that filter."}, nil
	}
	result := map[string]any{"count": len(answer.Rows), "items": answer.Rows}
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
	// [WorkDeps.actor] rather than the turn — see [listWorkItems.CallForTurn].
	if _, err := t.deps.actor(ctx, turn); err != nil {
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
	}, seatReadLevel)
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
				"type": "array",
				"description": "Tags this project declares — read them with " +
					"describe_project. A label the project has not declared " +
					"is refused, because a typo would otherwise become a " +
					"grouping nobody can filter on twice.",
				"items": map[string]any{"type": "string"},
			},
			"labels_create_missing": map[string]any{
				"type": "boolean",
				"description": "Declare any label in `labels` this project " +
					"does not have, then file. Say true only when you MEANT " +
					"to add a grouping — the answer lists what it created.",
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
	// THE ASSIGNEE IS RESOLVED BEFORE THE WATCHERS, so the canonical
	// handle is what gets watched: a task watched under one spelling and
	// assigned under another is a task whose assignee is not following it.
	assignee, refusal := t.deps.resolveHandle(CreateWorkItemTool,
		"`assignee`", task.Assignee)
	if refusal != "" {
		return failed(refusal), nil
	}
	task.Assignee = assignee
	// THE REPORTER WATCHES WHAT THEY FILED, and so does the assignee. Set
	// at the write rather than derived at the wake: a watcher list built
	// later would be built from a row that has moved on.
	task.Watchers = handles(actor.Handle, task.Assignee)

	// THE LABELS BEFORE THE TASK, because the create refuses one the
	// project has not declared and the declare is a separate record on a
	// separate subject: doing it after would file the task that already
	// failed.
	declared, labelRefusal := t.deps.declareLabels(ctx, actor, args,
		task.Project, task.Tags)
	if refusal := labelRefusal; refusal != "" {
		return failed(refusal), nil
	}
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
		"labels_created": declared, "version": got.Version,
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
	got, err := d.Reader.Task(ctx, ref, tracker.DetailWants{}, seatReadLevel)
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

// resolveHandle turns a handle a caller typed into one the company has.
//
// # Why a handle is validated AT ALL, when nothing used to
//
// Because an unknown one fails silently and permanently. It is stored, it
// rides the routing snapshot, it becomes a candidate — and [tracker.Route]
// drops it against the live roster with no error, no per-candidate log and no
// metric. The write answers `outcome: applied`; the person it named never
// hears anything; and on a goal there is not even a lead to catch the fall,
// because `goal_updated` is deliberately outside the fallback set. A
// misspelling is indistinguishable from a colleague who is simply quiet.
//
// # Why HERE and not in the tracker
//
// The tracker holds no org chart, deliberately: the applier may not read one,
// because two nodes briefly on different epochs would write different rows for
// one record. So the chart is the CALLER's, and this is the caller.
//
// It resolves rather than merely checking, through the same machinery
// `lookup_colleague` uses — so a caller that typed a role's NAME, or a handle
// in the wrong shape, gets the canonical handle rather than a refusal it
// cannot act on.
//
// A NIL ROSTER ADMITS EVERYTHING. A surface with no chart loaded cannot tell a
// typo from a colleague, and refusing every handle there would be worse than
// the hole this closes.
func (d WorkDeps) resolveHandle(tool, field, arg string) (string, string) {
	handle := strings.TrimSpace(arg)
	if handle == "" || d.Seats == nil {
		return handle, ""
	}
	seats := d.Seats()
	if len(seats) == 0 {
		return handle, ""
	}
	found := colleague.Resolve(handle, seats)
	switch {
	case len(found) == 1:
		return found[0].Seat.Handle, ""
	case len(found) == 0:
		return "", fmt.Sprintf("%s names %s %q and there is nobody here by "+
			"that name. Look them up with %s rather than guessing — a handle "+
			"nobody has is stored, and then every notification to it is "+
			"dropped in silence. The seats are: %s.",
			tool, field, clip(handle), LookupColleagueTool,
			strings.Join(allHandles(seats), ", "))
	}
	return "", fmt.Sprintf("%s names %s %q and it matches %s. Name one of them "+
		"exactly.", tool, field, clip(handle), strings.Join(matchHandles(found), " or "))
}

// resolveHandles is the same for a LIST, and the difference is the one that
// makes a whole-post-state write safe.
//
// A SAVE MAY NOT GROW THE UNRESOLVABLE SET, rather than refusing any save that
// carries one. `write_work_goal` replaces the whole document, so a flat
// refusal would make a goal whose owner LEFT THE COMPANY permanently
// unsaveable — including the one edit that removes them. A departure is
// repaired by editing the object, never blocked by it; a typo adds a name that
// was not there before, and that is what is refused.
func (d WorkDeps) resolveHandles(tool, field string, args, before []string) (
	[]string, string) {

	known := map[string]bool{}
	for _, h := range before {
		known[strings.TrimSpace(h)] = true
	}
	out := make([]string, 0, len(args))
	for _, arg := range args {
		handle, refusal := d.resolveHandle(tool, field, arg)
		if refusal == "" {
			out = append(out, handle)
			continue
		}
		if known[strings.TrimSpace(arg)] {
			// ALREADY ON THE OBJECT. Somebody left, or was renamed;
			// the save carries them because it carries everything, and
			// refusing it would leave nobody able to take them off.
			out = append(out, strings.TrimSpace(arg))
			continue
		}
		return nil, refusal
	}
	return out, ""
}

// matchHandles renders an ambiguous resolution's candidates.
func matchHandles(found []colleague.Candidate) []string {
	out := make([]string, 0, len(found))
	for _, c := range found {
		out = append(out, c.Seat.Handle)
	}
	return out
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
				"type": "array",
				"description": "REPLACES the item's labels. Each must be a " +
					"tag this project declares — describe_project lists them.",
				"items": map[string]any{"type": "string"},
			},
			"labels_create_missing": map[string]any{
				"type": "boolean",
				"description": "Declare any label in `labels` this project " +
					"does not have, then write. Say true only when you MEANT " +
					"to add a grouping.",
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
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatReadLevel)
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
	if patch.Assignee != nil {
		// THE SAME CHECK THE CREATE MAKES, and it belongs here rather
		// than in patchFromArgs for the reason that function is a free
		// one: the roster is on the deps, and a patch builder that
		// reached for it would need the whole surface threaded through
		// it to validate one field.
		assignee, refusal := t.deps.resolveHandle(UpdateWorkItemTool,
			"`assignee`", *patch.Assignee)
		if refusal != "" {
			return failed(refusal), nil
		}
		patch.Assignee = &assignee
	}
	var declared []string
	if patch.Tags != nil {
		// AGAINST THE TASK'S HOME PROJECT, which is the one whose set
		// the write is checked against — never the caller's default.
		if declared, refusal = t.deps.declareLabels(ctx, actor, args,
			before.Task.Project, *patch.Tags); refusal != "" {
			return failed(refusal), nil
		}
	}
	got, err := writer.UpdateTask(ctx,
		opIDFor(actor, "update", before.Task.ID), before.Task.ID,
		before.Task.Project, ifMatch, patch, kind,
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
		"labels_created": declared, "version": got.Version,
	})
}

// declareLabels declares the labels a write is about to use that its project
// does not have — but only if the caller SAID SO.
//
// # Why an opt-in rather than a create-on-use
//
// Because the two failures look identical at the call and are opposite in
// effect: a seat that meant `regression` and a seat that typo'd `regresion`
// both send one unknown label, and declaring whichever arrived would fill a
// board's filter strip with every misspelling anybody ever typed. The flag is
// the difference between them, and it is the only thing that can be.
//
// It returns what it created, so a caller that set the flag by habit still
// sees a typo in the answer rather than on a board three weeks later.
func (d WorkDeps) declareLabels(ctx context.Context, actor Actor,
	args map[string]any, project string, labels []string) ([]string, string) {

	if len(labels) == 0 || !argBool(args, "labels_create_missing") {
		return nil, ""
	}
	if d.ProjectWriter == nil {
		// THE REFUSAL NAMES THE OTHER ROUTE rather than failing the
		// write: a build with no project writer still has a lead who
		// can declare the tag, and the create that follows will say
		// which label is missing.
		return nil, "This build cannot declare labels at a write. Ask the " +
			"project lead to declare it, or file without the label."
	}
	created, warnings, err := d.ProjectWriter(actor).EnsureTags(ctx,
		"tags-"+uuid.NewString(), project, labels)
	if err != nil {
		return nil, writeFailure(tracker.WriteProjectTool, err)
	}
	if len(warnings) > 0 {
		// THE WARNINGS RIDE THE CREATED LIST, because they are about
		// exactly these slugs and the caller has one answer to read.
		created = append(created, warnings...)
	}
	return created, ""
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
		// AND THE KIND STAYS `fields`, which is what a task's priority
		// moving IS. `prioritised` is a different event entirely — it is
		// somebody writing YOUR OWN priority list, on the person object,
		// and it is the one kind whose recipient is read from
		// Snapshot.Person rather than from the task.
		//
		// Setting it here was silently worse than picking the wrong
		// word: ChangePrioritised is not in [tracker.ChangeKind.TaskCommit],
		// so a task's priority change woke no assignee, no collaborator
		// and no watcher — while Snapshot.Person was empty on this path,
		// so the one arm that does route it added nobody either. A
		// priority change reached NOBODY AT ALL.
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
		// A GESTURE ABOUT ONE PERSON, never the set. This tool cannot
		// form the watcher list: [tracker.TaskPatch]'s collections are
		// carried whole, so writing `watchers: [me]` here removed
		// everybody else — and, the change kind being ChangeWatchers,
		// announced their removal in the wake. The writer resolves it
		// against the task's current sets inside its own snapshot, which
		// is the only place a single consistent read of them exists.
		patch.Watch = &tracker.WatchIntent{Handle: actor.Handle, Watch: watch}
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
// remove is a handle set minus one handle, order preserved.
func remove(all []string, handle string) []string {
	out := make([]string, 0, len(all))
	for _, one := range all {
		if one != handle {
			out = append(out, one)
		}
	}
	return out
}

// appendMissing adds the handle when `add`, and otherwise leaves the set.
func appendMissing(all []string, handle string, add bool) []string {
	if !add {
		return all
	}
	return append(all, handle)
}

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
	if patch.Watch != nil {
		// THE GESTURE APPLIED TO THE SNAPSHOT THIS TOOL READ. The
		// durable sets are the WRITER's, settled inside its own
		// transaction, and this is only the wake's recipient list — so
		// it is the best answer the tool has rather than the authority.
		// Leaving it out would be worse than approximating it: a person
		// who just started watching would be absent from the very wake
		// announcing that they did.
		task.Watchers = appendMissing(remove(task.Watchers, patch.Watch.Handle),
			patch.Watch.Handle, patch.Watch.Watch)
		task.Muted = appendMissing(remove(task.Muted, patch.Watch.Handle),
			patch.Watch.Handle, !patch.Watch.Watch)
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
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatReadLevel)
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
	//
	// THE COMMENTER'S WATCH IS A GESTURE, NOT A SET. Sending the whole
	// watcher set — read here, OUTSIDE the writer's own decide snapshot —
	// made a comment a last-write-wins over that collection: somebody who
	// watched or unwatched between this read and the append had their
	// change silently discarded by a comment that was not about them. It
	// could also push the set past [tracker.MaxWatchers] with nothing
	// checking, after which the cap refused every unwatch and nobody
	// could leave. The gesture is resolved against the CURRENT row inside
	// the decide, and it is `Auto` because nobody pressed watch: at the
	// cap the comment lands and the watch is skipped.
	patch := tracker.TaskPatch{
		Comment: comment,
		Watch: &tracker.WatchIntent{
			Handle: actor.Handle, Watch: true, Auto: true,
		},
	}
	got, err := writer.UpdateTask(ctx,
		opIDFor(actor, "comment", comment.ID), before.Task.ID, before.Task.Project,
		// A COMMENT NEVER CONDITIONS ON A VERSION: it adds to the thread
		// rather than replacing anybody's value, so there is nothing a
		// concurrent edit could make it clobber.
		tracker.NoIfMatch, patch, tracker.ChangeComment,
		tracker.Wake{
			Kind:   tracker.ChangeComment,
			Before: before.Task,
			// THROUGH THE SAME SIMULATION EVERY OTHER WRITE USES, so the
			// gesture is applied once rather than here and again in
			// [patched]: a second copy is how the two stop agreeing,
			// and this one had already drifted — it appended the
			// commenter to the watcher set unconditionally, which is
			// wrong at the cap now that an automatic watch is skipped
			// there rather than refused.
			After:   patched(before.Task, patch),
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

// defaultProject is the seat's own, or empty where its unit owns none.
//
// ONE SPELLING, because three tools fall back to it and a second copy would
// be the one that stopped matching — a create filing into the seat's project
// while a describe answered about another is the shape that teaches a model
// the wrong vocabulary for the container it is writing to.
func (d WorkDeps) defaultProject(handle string) string {
	if d.DefaultProject == nil {
		return ""
	}
	return d.DefaultProject(handle)
}

func (d WorkDeps) zone() *time.Location {
	if d.Zone == nil {
		return time.UTC
	}
	return d.Zone
}

func statusList() string   { return joinValues(tracker.Statuses) }
func priorityList() string { return joinValues(tracker.Priorities) }

// openGroups is the status groups where work has not finished, DERIVED from
// the closed set rather than listed.
//
// A literal here is a filter that goes on parsing after somebody adds a fifth
// group and stops meaning what it says — and, before that, it is how
// `open_only` came to name two groups that do not exist.
func openGroups() []string {
	out := make([]string, 0, len(tracker.StatusGroups))
	for _, group := range tracker.StatusGroups {
		if group.Open() {
			out = append(out, string(group))
		}
	}
	return out
}

// joinValues renders a closed set for a tool description.
func joinValues[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// seatReadLevel is what every read on this surface uses.
//
// # Why it is a name and not the literal it replaced
//
// Because the literal was `session` at every one of these call sites, each
// plausible on its own — "the caller sees its own writes" is what a tool
// wants — and what it produced was the opposite. A session read waits for the
// caller's own high-water mark, NOTHING here ever populated one, so the wait
// target was the zero position: the read served this node's committed prefix
// immediately and labelled the answer `session`. The state-log reader's own
// comment names that shape — "a stale read wearing a stronger name".
//
// A seat asked for the strongest guarantee the engine has, was handed the
// weakest, and could not tell. It has no screen on which to notice, and its
// reads DECIDE things: a create refuses a project the company does not have,
// a hand-off names a colleague, a turn reports what it found. A seat that
// reads a stale task and tells a colleague "nobody is assigned to this" has
// produced a wrong answer no broker refuses.
//
// # The operator MCP reads through the same value
//
// It serves these same tool implementations, and the design gives it the same
// level for the same reason: a person deciding something about their own
// company is not helped by a faster wrong answer. Neither surface lets the
// caller choose — see [statelog.LevelSettable].
var seatReadLevel = statelog.DefaultReadLevel(statelog.SurfaceSeat)
