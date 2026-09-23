package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/crewlet/crewlet/internal/textcut"
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
	ListWorkItemsTool   = tracker.ListWorkItemsTool
	GetWorkItemTool     = tracker.GetWorkItemTool
	CreateWorkItemTool  = tracker.CreateWorkItemTool
	UpdateWorkItemTool  = tracker.UpdateWorkItemTool
	CommentOnWorkTool   = tracker.CommentOnWorkTool
	MergeWorkItemTool   = tracker.MergeWorkItemTool
	SearchWorkItemsTool = tracker.SearchWorkItemsTool
)

// WorkTools are the whole native catalogue, so a caller registering them names
// one thing.
func WorkTools() []string { return tracker.Tools() }

// WorkWrites are the four that count as a DELIVERY.
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
		fresh statelog.Freshness) (tracker.TaskDetail, error)
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error)
	Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error)
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
	Thread(ctx context.Context, q tracker.ThreadQuery,
		fresh statelog.Freshness) (tracker.ResolvedThread, error)
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

// WorkDepender writes a dependency, which is the one gesture here that is a
// SEQUENCE rather than a patch.
//
// ITS OWN INTERFACE, declared beside [WorkWriter] rather than folded into it,
// because the two are satisfied by different surfaces: every surface that can
// write a task can patch one, and only a surface holding the replicated estate
// can read the counterparties a dependency has to have checked before its
// first append. A build without one refuses the dependency arguments by name
// and still serves every other edit.
type WorkDepender interface {
	Depend(ctx context.Context, opID string, change tracker.DependencyChange,
		leads tracker.Leads) (tracker.DependencyResult, error)
}

// WorkMerger folds one item into another, which is the second gesture here
// that is a SEQUENCE rather than a patch — and the reason it is not folded
// into [WorkWriter] is [WorkDepender]'s: it reads a subtree before its first
// append and takes a fleet claim, neither of which a surface holding no
// replicated estate can do.
type WorkMerger interface {
	MergeDuplicates(ctx context.Context, opID, duplicate, into string,
		reparent bool, notify *tracker.Notify) (tracker.WriteResult, error)
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

	// Dependencies resolves the dependency sequence FOR ONE ACTOR, in the
	// same shape and for the same reason [WorkDeps.Writer] is a function.
	//
	// Nil on a build whose writer holds no replicated estate, which is
	// what the dependency arguments are refused by name against.
	Dependencies func(actor Actor) WorkDepender

	// Merges resolves the duplicate-merge sequence FOR ONE ACTOR, in the
	// same shape and for the same reason [WorkDeps.Dependencies] is.
	//
	// Nil on a build whose writer holds no replicated estate, and the merge
	// tool is then OMITTED rather than refusing at the call: it is a whole
	// verb rather than an argument on one, and a catalogue advertising a
	// tool that always fails is how a model learns to distrust all of them.
	Merges func(actor Actor) WorkMerger

	// Search ranks work items by text — see worksearch.go for why that is
	// a verb of its own beside the board.
	//
	// A VALUE rather than a function of the actor, unlike every writer
	// above: a search reads, and what it reads is the same corpus for
	// everybody. There is nothing here to attribute.
	Search WorkSearcher

	// ViewWriter resolves the saved-view write side for one actor, in the
	// same shape and for the same reason [WorkDeps.Writer] is a function.
	//
	// SEPARATE from Writer because the surfaces differ: every surface has
	// a task writer and only the OPERATOR's has a view writer, so folding
	// the verb into one interface would make a seat's registration
	// implement a method nothing there may call.
	ViewWriter func(actor Actor) ViewWriter

	// CatalogueWriter resolves the workspace catalogue write side, and is
	// the operator surface's alone for the reason the two above are.
	CatalogueWriter func(actor Actor) CatalogueWriter

	// PersonWriter resolves the person write side for one actor. The
	// operator surface's alone, because a seat is not a human: it has a
	// mailbox rather than an inbox, and nothing on a person's record
	// describes one.
	PersonWriter func(actor Actor) PersonWriter

	// Inbox reads what the company asked of somebody — the applier's own
	// notification rows.
	//
	// A VALUE rather than a function of the actor, for the reason
	// [WorkDeps.Search] is: it reads, and the authority over WHOSE inbox
	// is the surface's rather than this seam's. The operator surface's
	// alone, beside [WorkDeps.PersonWriter] and for the same reason.
	Inbox InboxReader

	// ProjectWriter resolves a project's own settings for one actor.
	//
	// EVERY SURFACE HAS ONE, unlike the five above it, because one of its
	// facets — declaring a tag — is open to every seat: a seat that could
	// not declare a label could never use the `labels` argument on the
	// create and update tools it already holds. The authority for the rest
	// is resolved per call rather than per surface.
	ProjectWriter func(actor Actor) ProjectWriter

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

	// Party resolves a handle to the two identities that person's own
	// records may be filed under — their seat, and the `api.auth` token
	// bound to it with `contact.crewlet_operator_id`.
	//
	// A SEAM RATHER THAN A FIELD ON THE ROSTER, because [Corpus]
	// deliberately leaves an operator id out: that map is both the
	// exact-id index and what `lookup_colleague` renders, and a credential
	// listed beside somebody's Slack id reads as somewhere an agent could
	// mention them. This is the attribution key, asked for by name.
	//
	// A FUNCTION OF THE HANDLE rather than of the caller, because the
	// party belongs to the person ASKED ABOUT: an operator reading a
	// report's inbox is handed THAT person's two names from the chart,
	// never the credential in their own hand.
	//
	// Nil answers the handle alone, which is the honest state for a
	// surface with no chart loaded and the whole truth for every seat.
	Party func(handle string) tracker.Party

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

	// Seat is the chart seat that credential is BOUND to with
	// `contact.crewlet_operator_id` — empty for an actor that already IS
	// a seat, and for a token nobody bound.
	//
	// IT CHANGES NO ATTRIBUTION. [Actor.Handle] stays the author and
	// [Actor.Kind] stays `operator`, because a tracker whose author field
	// is chosen by the writer is not an audit trail. What it answers is
	// the OTHER question a person surface asks — whose inbox, whose pins,
	// whose queue, whose day — and that is the person rather than the
	// credential in their hand. See [Actor.Record] and [Actor.Party].
	//
	// Resolved by the surface, because this package holds no chart: the
	// operator MCP walks it with `org.Organization.SeatByOperatorID`.
	Seat string

	// TurnID is the RUN that produced this write — provenance, so an
	// audit can walk from an item back to the execution that wrote it.
	TurnID string

	// WorkKey is the unit of work behind that run, and it is what the
	// derived operation and comment ids are seeded from. It has to be the
	// one that SURVIVES a re-run: a redelivered trigger runs again with a
	// new TurnID, and an id seeded from that would post the same comment
	// twice. See [Actor.OperationSeed] and ADR-0017.
	WorkKey string

	// WorkSince is when that unit of work BEGAN — see
	// [turnctx.Turn.WorkSince] — and it travels beside WorkKey for the
	// reason the key does: a re-run must reproduce it exactly, because it
	// is the instant every operation id derived from the key carries. See
	// [Actor.OperationSince].
	WorkSince time.Time

	Chain []string
}

// OperationSeed is what a derived, idempotent id is built from.
//
// THE WORK KEY WHERE THERE IS ONE, because that is the identity a redelivery
// reproduces and therefore the only one that can collapse a re-run's writes
// into the first attempt's.
//
// THE RUN WHERE THERE IS NOT. A turn with no ledgerable trigger — a scheduled
// fire, a resumed run whose dispatch is long gone — has no cross-run duplicate
// to collapse, but it still has ROUNDS: an executor that calls the same update
// twice in one run should write once, and seeding from "" made every such call
// mint a fresh id and write again. Falling back to the run keeps the
// within-run guarantee without inventing a cross-run one.
//
// EMPTY OUTSIDE A TURN, which is the operator surface: their MCP client made
// one call, nothing will redeliver it, and an invented key would be a lie
// about what produced the write.
func (a Actor) OperationSeed() string {
	if a.WorkKey != "" {
		return a.WorkKey
	}
	return a.TurnID
}

// OperationSince is the instant an operation id derived from
// [Actor.OperationSeed] carries: when the identity it is seeded from BEGAN.
//
// THE SAME CHOICE AS THE SEED, made in the same order, because the two are one
// identity: the unit of work's own start where the seed is the work key, and
// the run's where it is the run — which its id carries, run ids being minted
// time-ordered for exactly this reader.
//
// NEVER THE INSTANT OF THE CALL. The ledger that collapses a retry cannot
// vouch for an operation minted before its watermark — the point before which
// it may have lost rows — whose row it no longer holds, and a re-run's call is
// always after the loss it has to be judged against — so an id stamped with
// its call's clock is one a re-run after a loss decides a second time. See
// [statelog.OpMintedAt].
//
// The zero instant where neither is known — a run parked by a build whose run
// ids carried none — which reads as older than every loss: a node whose ledger
// ever lost a row answers such a write `unknown`, unless it holds its row,
// rather than risking it twice.
func (a Actor) OperationSince() time.Time {
	if a.WorkKey != "" {
		return a.WorkSince
	}
	at, _ := statelog.OpMintedAt(a.TurnID)
	return at
}

// Record is WHOSE OWN STATE this actor writes and reads: the seat the
// credential is bound to, or the actor itself where nothing is bound.
//
// A seat answers its own handle, which is every in-engine caller. A bound
// operator answers the person they are, so their assistant's marks and pins
// land on that person's record rather than on a second one named after a
// credential. An unbound token answers itself, which is an ordinary state —
// an operator outside the org chart — and not an error.
func (a Actor) Record() string {
	if seat := strings.TrimSpace(a.Seat); seat != "" {
		return seat
	}
	return a.Handle
}

// Party is who a personal READ is about: [Actor.Record] and, behind it, the
// credential their older rows may be filed under.
//
// The alias is matched against and never rendered — see [tracker.Party] — so
// a screen is never handed a token where a colleague's handle goes.
func (a Actor) Party() tracker.Party {
	return tracker.Party{Handle: a.Record(), OperatorID: a.OperatorID}
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
		Handle:    seat.Handle(),
		Kind:      tracker.AuthorAgent,
		TurnID:    turn.RunID,
		WorkKey:   turn.WorkKey,
		WorkSince: turn.WorkSince,
		Chain:     turn.Chain,
	}, nil
}

// actor resolves who this call writes as — see [WorkDeps.Actor].
func (d WorkDeps) actor(ctx context.Context, turn *turnctx.Turn) (Actor, error) {
	if d.Actor != nil {
		return d.Actor(ctx, turn)
	}
	return actorFor(turn)
}

// partyOf is who a personal read about `handle` is about — see
// [WorkDeps.Party].
//
// NO SEAM RESOLVES TO THE HANDLE ALONE, which is exactly what this read did
// before the seam existed and is still the whole truth for a seat.
func (d WorkDeps) partyOf(handle string) tracker.Party {
	if d.Party == nil {
		return tracker.PartyOf(handle)
	}
	return d.Party(handle)
}

// turnKey is the idempotency key a comment carries, or "" outside a turn.
//
// NIL-SAFE, because these tools serve two callers now. A TURN's key makes a
// comment idempotent: the engine's redelivery guarantees make a re-run turn
// ordinary, and without it a seat says the same thing twice. An OPERATOR has
// no turn and no redelivery — their MCP client made one call — so there is
// nothing to deduplicate against and an invented key would be a lie about
// what produced the comment.
//
// THROUGH [Actor.OperationSeed] rather than reading a field, because which of
// a turn's two identities an idempotent id is built from is one rule and this
// is its second caller. Written out here it would be the same rule spelled
// twice, free to disagree — the shape internal/whsec and internal/textcut
// exist because of.
func turnKey(turn *turnctx.Turn) string {
	return turnIdentity(turn).OperationSeed()
}

// turnSince is the instant an id derived from [turnKey] carries — see
// [Actor.OperationSince] — and the zero instant outside a turn, where nothing
// is derived.
func turnSince(turn *turnctx.Turn) time.Time {
	if turn == nil {
		return time.Time{}
	}
	return turnIdentity(turn).OperationSince()
}

// turnIdentity is the part of a turn a derived id is built from, as an
// [Actor], so the seed and its instant are chosen by the one rule the actor
// states rather than by a second copy of it here.
func turnIdentity(turn *turnctx.Turn) Actor {
	if turn == nil {
		return Actor{}
	}
	return Actor{TurnID: turn.RunID, WorkKey: turn.WorkKey, WorkSince: turn.WorkSince}
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

// queryAlias is one argument this surface spells differently from the query
// grammar it compiles into.
//
// # Why the four renames are written down rather than only performed
//
// Because a SECOND reader needs them and it is prose: a saved view stores the
// GRAMMAR's keys — that is what [tracker.ParseQuery] validates and what the
// expansion reads back — while this tool's arguments are the model's
// vocabulary. save_work_view told a model the parameters were "list_work_items'
// own", so a model saving the query it had just run wrote `project`, `text`,
// `label` and `open_only` into a view, and every one of them was refused as
// not a query parameter at all.
//
// The translation itself stays in [listWorkItems.CallForTurn], where each arm
// carries the reason for its own rename. What this table is for is the
// SENTENCE, built from it rather than written beside it, and the test that
// holds both against what the tool actually sends.
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
			"view": map[string]any{
				"type": "string",
				"description": "A saved view's `id`, from list_work_views: " +
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
					"search_work_items, which ranks over descriptions too; " +
					"search_knowledge is for the company's written knowledge.",
			},
			"type": map[string]any{
				"type": "string",
				"description": "A work type — `bug`, `story`, `task`, and " +
					"whatever else this company declares. get_work_catalogue " +
					"lists them. Comma separate for several.",
			},
			"priority": map[string]any{
				"type":        "string",
				"description": "One of: " + priorityList() + ". Comma separate for several.",
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
				"description": "A key or id: lists that item's SUBTASKS. For " +
					"reading a piece of work broken down.",
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
					"describe_project lists what a project declares and what " +
					"each may hold. An operator is available per type, e.g. " +
					"`>3` on a number or `a..b` on a date.",
				"additionalProperties": true,
			},
			"sort": map[string]any{
				"type": "string",
				"description": "How to order: `rank` (the board's own order), " +
					"`updated`, `created`, `due`, `priority`, or `f.<slug>` " +
					"for a custom field. Prefix with `-` to reverse.",
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next_cursor` from a previous call, for " +
					"the page after it. Everything else stays as it was.",
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
	// THE KEYS WHOSE TOOL ARGUMENT AND GRAMMAR KEY ARE THE SAME WORD, which
	// is most of them: the tool's vocabulary was deliberately built from
	// the grammar's, so forwarding is the whole translation. The three that
	// differ are below, each with its own reason.
	//
	// THE SET IS WHAT A SEAT CANNOT ASK ANOTHER WAY. The grammar has
	// forty-odd keys and this tool is deliberately few, so what earns a
	// place here is a question a seat actually has and no other argument
	// answers — "the bugs", "what is due this week", "what moved since
	// yesterday", "the subtasks of ENG-4", "what I filed", "what I follow",
	// "that team's board", and the next page of any of them. Board
	// FURNITURE — group, subgroup, group_limit, totals — is not on it: a
	// model reads rows, and a grouped answer costs it a shape to unpack for
	// a heading nobody renders.
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
	// THE CUSTOM-FIELD FILTERS, which are the one part of the grammar
	// whose keys a company invents: `f.<slug>` is how the whole
	// declaration machinery is reached, and with no way to name one a seat
	// could set a field and never filter on it again. An object rather
	// than a list of strings, because the value is the model's and the
	// slug is the company's, and splitting a typed string on `=` would
	// make a value containing one unwritable.
	// THE PARENT IS A REFERENCE AND IS RESOLVED, like every other one this
	// surface takes. `parent` is matched raw against `parent_id`, which
	// holds an ID — so a model passing the key it read answered an empty
	// list and no error, which is the failure `references` carries its own
	// comment about two lines from where this one is parsed.
	if ref := strings.TrimSpace(argString(args, "parent")); ref != "" {
		id, refusal := t.deps.resolveRef(ctx, ListWorkItemsTool, "`parent`", ref)
		if refusal != "" {
			return failed(refusal), nil
		}
		params["parent"] = id
	}
	if raw, held := args["field_filters"].(map[string]any); held {
		for slug, value := range raw {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			params[tracker.FieldKeyPrefix+slug] = value
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
		// AND IT IS THE ACTOR'S OWN PARTY, exactly as `my_work` asks:
		// for a seat its one handle, and for a bound operator the person
		// their credential names plus the credential itself. Asked about
		// the bare author, `preset=my_queue` answered a founder's
		// assistant about the TOKEN — a party no colleague has ever
		// assigned anything to — and `preset=priorities` about a queue
		// nobody wrote. The two fields rather than [Actor.Party],
		// because a viewer is a party plus a container and the grammar
		// derives the party from them ([tracker.Viewer.Party]).
		Handle:     actor.Record(),
		OperatorID: actor.OperatorID,
		Project:    t.deps.defaultProject(actor),
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
	// AND THE CHART THE UNIT FILTERS RESOLVE THROUGH, set here for the
	// reason the level is: it belongs to this surface rather than to the
	// grammar. A model types the team name it remembers, and the rows hold
	// the key the chart chose — see [tracker.Units].
	q.Units = t.deps.Units
	// AND THE ROWS ARE THE ROWS THAT MATCH, which is the one promise this
	// tool makes and the default took away.
	//
	// The grammar's default is [tracker.SubtasksCollapsed], where the filter
	// is a predicate on the ROOT and its whole subtree rides along
	// UNFILTERED. That is right for a board, which draws a tree. It is wrong
	// for an answer a model reads as a list: measured against a running
	// engine, `assignee=agent-cto` came back with nine rows of which six were
	// assigned to backend-engineer — they were subtasks of an epic the CTO
	// owns — and nothing on the answer says which rows matched and which rode
	// along, so a model counting its own queue counted somebody else's work.
	//
	// OVERRULED RATHER THAN DEFAULTED, for the reason the level above is: one
	// grammar serves the board, the socket, the REST route and this tool, and
	// a saved `view` carries an answer shape chosen for a board. A tree IS
	// board furniture, which this tool already declines along with `group_by`
	// and `totals` — see the passthrough list above. A model that wants a
	// subtree asks with `parent`, which the mode does not apply to at all.
	q.Subtasks = tracker.SubtasksSeparate
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
	// duplicate. AND HAVING COLUMNS IS NOT HAVING WORK: a closed axis
	// carries every column the query admits whether or not anything is in
	// it (see internal/tracker's grouping doc), so the question is whether
	// any column COUNTS anything.
	if len(answer.Rows) == 0 && boardEmpty(answer.Groups) && answer.Complete {
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
		// AND WHETHER ANYBODY ACTUALLY COUNTED THAT FAR. The count
		// stops at a ceiling, so a bare `total` at it reads as an exact
		// number a model will quote back — "there are 10000 open items"
		// — when what happened is that nobody counted past ten
		// thousand.
		if answer.TotalCapped {
			result["total_is_at_least"] = true
		}
	}
	// THE NEXT PAGE'S CURSOR, which is what makes the `cursor` argument
	// reachable at all: a caller cannot page without one, and this tool was
	// the only reader of this query grammar that dropped it — the REST
	// route beside it has always passed it on.
	if answer.NextCursor != "" {
		result["next_cursor"] = answer.NextCursor
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
	return "Read one work item: its description, status, assignee, labels, " +
		"links in both directions, the most recent comments and its recent " +
		"history. Comment bodies in the thread are EXCERPTS, ending in `…` " +
		"where one was cut — pass `comment` with that comment's id to read " +
		"it whole. Take `task.version` from the result and pass it back as " +
		"`if_match` on update_work_item to make your edit conditional."
}

func (t *getWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item key (ENG-42) or its id.",
			},
			"include": map[string]any{
				"type": "array",
				"description": "Which parts to read beside the item itself: " +
					"`comments`, `history`, `links`, `fields`. All four by " +
					"default — name fewer when you only need one, and the " +
					"answer is smaller. `fields` are the custom-field values " +
					"with the slug, name and type that explain each, which " +
					"is what you write back with.",
				"items": map[string]any{
					"type": "string",
					"enum": []any{"comments", "history", "links", "fields"},
				},
			},
			"comments_cursor": map[string]any{
				"type": "string",
				"description": "The `comments_cursor` from a previous read, " +
					"to see the comments before that page.",
			},
			"comment": map[string]any{
				"type": "string",
				"description": "One comment's id, to read that comment ALONE " +
					"with its body exactly as it was written. The thread " +
					"page carries excerpts; this is how you open the one you " +
					"need after seeing it end in `…`. On its own it answers " +
					"the item and that comment and nothing else — name " +
					"`include` as well if you also want the history, the " +
					"links or the fields.",
			},
			"body": map[string]any{
				"type": "boolean",
				"description": "Read the item's description in full. Every " +
					"other read carries the opening of it; this is how you " +
					"get the rest after seeing one end in `…`. On its own it " +
					"answers the item and its whole description and nothing " +
					"else.",
			},
		},
		"required": []any{"item"},
	}
}

// detailWants reads the `include` argument, defaulting to all four.
//
// ALL FOUR BY DEFAULT, because that is what this tool answered before the
// argument existed and a model that never learned to pass it must keep getting
// a whole item. What the argument buys is the caller who knows they want one
// part: the answer is then smaller by the parts they did not ask for, rather
// than by a cap the engine chose for them.
//
// `comment` AND `body` ARE THEIR OWN DEFAULTS, and that exception is what
// keeps either escape hatch usable. Naming one says what the call is for, and
// both arguments are new enough to have no back-compatible default to honour
// — where a whole item at its maximum is refused by [ToolAnswerBytes], one
// whole comment plus fifty history rows plus sixty-four links would be too,
// so the read that exists to recover a value would meet a refusal telling it
// to narrow. An explicit `include` still wins: a caller that asks for the
// comment AND the fields means it.
//
// They do not COMPOSE, and the second return is what says which won: a whole
// body and a whole comment together are 96 KiB before escaping, against a 64
// KiB ceiling, so a call naming both would be refused for asking for exactly
// the two things this pair exists to make reachable. `comment` takes
// precedence because it is the narrower ask — one value out of a thread,
// against the item's own description, which the next call gets by dropping it.
func detailWants(args map[string]any) (tracker.DetailWants, bool, string) {
	want := tracker.DetailWants{
		CommentCursor: strings.TrimSpace(argString(args, "comments_cursor")),
		Comment:       strings.TrimSpace(argString(args, "comment")),
	}
	wholeBody := want.Comment == "" && argBool(args, "body")
	raw, held := args["include"]
	if !held || raw == nil {
		if want.Comment != "" || wholeBody {
			return want, wholeBody, ""
		}
		want.Comments, want.History = true, true
		want.Links, want.Fields = true, true
		return want, wholeBody, ""
	}
	for _, part := range refList(raw) {
		switch strings.ToLower(part) {
		case "comments":
			want.Comments = true
		case "history":
			want.History = true
		case "links":
			want.Links = true
		case "fields":
			want.Fields = true
		default:
			return want, wholeBody, fmt.Sprintf("get_work_item has no %q to "+
				"include. The parts are: comments, history, links, fields.",
				clip(part))
		}
	}
	return want, wholeBody, ""
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
	want, wholeBody, refusal := detailWants(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	// THE CHART, so the answer carries the team's NAME beside the key the
	// row holds. A model acts on the key — it is what a `unit=` filter
	// takes, and that filter takes the name too — but a model also writes
	// PROSE about the item it just read, into a comment, a chat message
	// or a hand-off, and "filed into eng" is a sentence about a slug
	// nobody outside the config file has seen. It is also the only way
	// this surface can say a team has left the chart, which is the
	// difference between a stale unit and a typo.
	//
	// It is passed WHEREVER A CHART IS HELD rather than gated on a caller
	// asking, because an unresolved reference is a FINDING: a surface
	// that could resolve and did not would report every task as orphaned.
	want.Units = t.deps.Units
	detail, err := t.deps.Reader.Task(ctx, id, want, seatRead)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q. Check the key, or "+
			"use list_work_items to find it.", clip(id))), nil
	case errors.Is(err, tracker.ErrNoComment):
		return failed(fmt.Sprintf("Work item %q has no comment %q. Comment ids "+
			"come from the `comments` in a read of the item itself — drop "+
			"`comment` to see the thread.", clip(id), clip(want.Comment))), nil
	case err != nil:
		return failed(readFailure(GetWorkItemTool, err)), nil
	}
	// THE CUT IS TAKEN HERE, where the budget is. The tracker answers the
	// body whole because its other reader — the dashboard — renders a task
	// page and has no ceiling at all; this is the caller that has to fit
	// [ToolAnswerBytes], so it is the caller that decides what to carry.
	// The same division the config diff settled on: the differ reports
	// everything it found and the surface answering over a wire is the one
	// that bounds it.
	detail.Task.Body = shownBody(detail.Task.Body, wholeBody)
	narrow := "Ask for less with `include`: the parts are comments, history, " +
		"links and fields."
	if !detail.Complete {
		return jsonAnswer(map[string]any{
			"task": detail, "incomplete": incompleteNote(detail.Incomplete),
		}, narrow)
	}
	return jsonAnswer(detail, narrow)
}

// TaskBodyShown is how much of a task's description ONE tool answer carries
// when the caller did not ask for the whole of it.
//
// 4 KiB, against [tracker.MaxBody]'s 64 KiB, and the gap is the point: a body
// at its cap is 64 KiB before JSON escaping, which on its own is already past
// [ToolAnswerBytes] — so a maximal item was refused by weight with no argument
// that would narrow it, because `include` governs the collections beside the
// task and never the task itself. Every part of a detail read is bounded now:
// the thread is paged, the history is capped, the relation sets are capped,
// and this was the one value that was not.
//
// 4 KiB is roughly a thousand tokens — enough that an ordinary description
// arrives whole and is never marked at all, while the outliers that would
// spend a turn's budget become a pointer to `body: true`. It is the same
// bargain [tracker.CommentBodyShown] strikes one field over, at twice the
// size because an item has ONE description and a page carries twenty
// comments.
const TaskBodyShown = 4 << 10

// shownBody is the description as one tool answer carries it.
//
// MARKED when it is cut, which is the half a plain slice leaves out: a body
// cut at exactly the cap and handed over unmarked reads as a description that
// ENDED there, and the reader has no way to know there is a `body: true` call
// worth making. [textcut.Ellipsis] is the tree's one rune-safe cut, so a
// multi-byte character on the boundary does not reach a model as a
// replacement character.
func shownBody(body string, whole bool) string {
	if whole {
		return body
	}
	return textcut.Ellipsis(body, TaskBodyShown)
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
	return scheduleInto(map[string]any{
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
			"fields": map[string]any{
				"type": "object",
				"description": "Custom fields, keyed by SLUG — read them with " +
					"get_work_catalogue or describe_project. A project may " +
					"REQUIRE some for this type, and the create is refused " +
					"naming any that are missing.",
			},
			"unit": map[string]any{
				"type": "string",
				"description": "The team this work belongs to, by its id or " +
					"its name. Defaults to the team that owns `project`, " +
					"which is almost always right — name another only when " +
					"the work belongs to a different team than the project " +
					"it sits in.",
			},
			"waiting_on": map[string]any{
				"type": "array",
				"description": "Items this one is blocked BY, as keys or ids. " +
					"A plain list here, unlike update_work_item: a new item " +
					"has no dependencies to replace.",
				"items": map[string]any{"type": "string"},
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
	}, false)
}

// scheduleInto merges the scheduling parameters into a tool's own schema.
//
// MERGED rather than repeated, because the two tools have to accept exactly
// the same spellings: a create that takes `due` and an update that takes
// `due_at` is a pair a model gets wrong once and then avoids.
func scheduleInto(schema map[string]any, update bool) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	for key, value := range scheduleSchema(update) {
		props[key] = value
	}
	return schema
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
	if raw, held := args["fields"]; held {
		fields, refusal := fieldMap(raw)
		if refusal != "" {
			return failed(refusal), nil
		}
		task.Fields = fields
	}
	// WHEN IT IS DUE AND HOW BIG IT IS — see workschedule.go for why these
	// were filterable and unwritable.
	plan, refusal := readSchedule(args, now, t.deps.zone(), CreateWorkItemTool)
	if refusal != "" {
		return failed(refusal), nil
	}
	plan.applyToTask(&task)
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
		//nolint:govet // shadow: `x, refusal := f()` declares x too; see .golangci.yml
		parent, refusal := t.deps.resolveRef(ctx, CreateWorkItemTool, "`parent`", ref)
		if refusal != "" {
			return failed(refusal), nil
		}
		task.Parent = &parent
	}
	if task.Project == "" {
		task.Project = t.deps.defaultProject(actor)
		if task.Project == "" {
			return failed("create_work_item needs a `project`: your team owns " +
				"none, so there is no default. Ask which project this belongs " +
				"in rather than guessing."), nil
		}
	}
	// THE OPERATION, AND THE TASK'S ID FROM IT — see [createdTaskID]. The
	// project is the operation's object rather than the id, because the id
	// is what is being derived; by here it is settled, default included.
	opID := opIDFor(actor, "create", task.Project, args)
	task.ID = createdTaskID(opID)
	// A UNIT THE CALLER NAMED, checked against the chart and open to every
	// seat — deliberately unlike the re-route above. `FiledUnit` is the
	// immutable record of which team the work belongs to; `RoutingUnit`
	// is the mutable half, whose lead hears about it now.
	//
	// NAMING NONE IS THE ORDINARY CASE, and the tracker fills both from
	// the project's own chart-owned unit at the write — one derivation,
	// inside the create's own snapshot, so every writer gets the same
	// answer. This stamped the FILING SEAT'S team instead,
	// which was right only for a seat filing into its own team's project:
	// an operator holds no seat and a root-level seat holds no unit, so
	// both filed work into no unit at all, whatever the project said the
	// work belonged to.
	//
	// WHAT IS STORED IS THE CHART'S OWN KEY, not the string the model
	// typed: a unit is named by its id or by its name, in whatever case
	// the model remembered, and `filed_unit` is written once and never
	// rewritten — so storing the argument verbatim would leave one team's
	// work under as many spellings as its colleagues have ways of writing
	// it, each of them a filter the others miss.
	if unit := strings.TrimSpace(argString(args, "unit")); unit != "" {
		if t.deps.Units != nil {
			resolved, found := t.deps.Units.ResolveUnit(unit)
			if !found {
				return failed(fmt.Sprintf("This company has no team %q.",
					clip(unit))), nil
			}
			unit = resolved.Key
		}
		task.FiledUnit, task.RoutingUnit = unit, unit
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
	//
	// AND THE WATCH IS THE PERSON'S, which is [Actor.Record] and not the
	// author beside it: `reporter` is attribution and stays the token,
	// but a watcher is an ADDRESS — the party registry resolves SEATS, so
	// a token id in the set is dropped at every later wake and renders as
	// a colleague on the item. A founder whose assistant filed the work
	// heard nothing about it again.
	task.Watchers = handles(actor.Record(), task.Assignee)

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
	// THE BLOCKERS ARE RESOLVED BEFORE THE CREATE, so a dependency on a
	// task that does not exist refuses the whole call rather than leaving
	// a new item filed with an edge nobody asked to drop.
	var blockers []string
	for _, ref := range argStrings(args, "waiting_on") {
		id, refusal := t.deps.resolveRef(ctx, CreateWorkItemTool, "`waiting_on`", ref)
		if refusal != "" {
			return failed(refusal), nil
		}
		blockers = append(blockers, id)
	}
	if len(blockers) > 0 && t.deps.Dependencies == nil {
		return failed(unconfiguredText(CreateWorkItemTool)), nil
	}
	got, err := writer.CreateTask(ctx, opID, task, notify)
	if err != nil {
		return failed(writeFailure(CreateWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	answer := map[string]any{
		"key": got.Key, "id": task.ID, "status": task.Status,
		"assignee": task.Assignee, "outcome": string(got.Outcome), "position": positionOf(got.Position),
		"labels_created": declared, "version": got.Version,
	}
	if len(got.Warnings) > 0 {
		answer["warnings"] = got.Warnings
	}
	// AND THE DEPENDENCIES AFTER IT, because a dependency is an edge
	// between two items that exist: the mirror commit names this task,
	// and writing it before the create would name a task no node holds.
	if len(blockers) > 0 {
		result, err := t.deps.Dependencies(actor).Depend(ctx,
			opIDFor(actor, "depend", task.ID, args), tracker.DependencyChange{
				Task: task.ID, Project: task.Project, WaitingOnAdd: blockers,
			}, t.deps.Leads)
		if err != nil {
			// THE ITEM EXISTS AND IS REPORTED. Failing the call would
			// tell a model its item was not filed, and the next
			// attempt would file a second one.
			answer["dependencies_failed"] = writeFailure(CreateWorkItemTool, err)
			return jsonResult(answer)
		}
		t.deps.settle(ctx, result.Position)
		if len(result.OneSided) > 0 {
			answer["dependencies_pending_mirror"] = len(result.OneSided)
		}
	}
	return jsonResult(answer)
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
	got, err := d.Reader.Task(ctx, ref, tracker.DetailWants{}, seatRead)
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
// metric. The write answers `outcome: applied` and the person it named never
// hears anything, so a misspelling is indistinguishable from a colleague who
// is simply quiet.
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
//
// OUTSIDE A TURN IT IS FRESH PER CALL, and that is the whole of the second
// branch: there is nothing to be idempotent against, because nothing is going
// to redeliver an operator's tool call. It used to return `verb + "-" + object`
// here on the reasoning that "the object's own id is enough to make it unique"
// — which is true of two DIFFERENT objects and false of the same one twice, so
// the id was stable for the life of the deployment and the operation ledger
// collapsed every write after the first as a redelivery.
//
// Measured through `/operator/mcp`, which is the ONLY write path the dashboard
// offers and what an operator's own assistant connects to: a work item could
// be updated exactly once. The second update, and every one after it, wrote
// nothing and answered `outcome: "applied"` with the FIRST write's position —
// the worst shape a write surface has, because the caller is told it worked.
// The same held for a dependency change, a re-removal after a restore, a
// merge, a priority list, a pin set and an inbox mark.
//
// [commentID] three hundred lines below has always had this right, and is
// where the shape comes from: the turn's key where there is one, a fresh id
// where there is not.
//
// # And it carries the instant the unit of work began
//
// Both branches mint through [statelog], whose ids carry their own mint
// instant: a fresh id the instant of this call, which is the instant it was
// minted, and a derived one [Actor.OperationSince] — the start of the work it
// is derived from, which is what a re-run reproduces. The state log refuses to
// decide a second time an operation its ledger cannot vouch for, and "cannot
// vouch" is "minted before this node adopted a donated snapshot"; an id that
// carried this CALL's instant instead read every re-run after an adoption as
// minted after it, and published the operation again.
//
// # And what the call ASKS FOR is part of it
//
// The unit of work, the verb and the object say which write this is — and
// not which of two writes. A turn that moved a task to `in_progress` and later
// to `done`, commented on it twice, or set a queue and then corrected it,
// derived ONE id for both calls, and the second was the operation's
// retry as far as anything downstream could tell: inside the log's duplicate
// window the broker acknowledged it as the first record, the ledger answered
// `applied` at the first position, and the change the model had just asked
// for was dropped with a success. So the id also covers a digest of the
// call's own arguments ([callDigest]): the same call repeated — a re-run, or
// an executor that asks twice — is still one operation, and two different
// calls are two.
func opIDFor(actor Actor, verb, object string, args map[string]any) string {
	seed := actor.OperationSeed()
	if seed == "" {
		return statelog.NewOpID(time.Now(), verb+"-"+object)
	}
	return statelog.DeriveOpID(actor.OperationSince(), verb+"-"+object,
		opIDNamespace, seed, verb, object, callDigest(args))
}

// callDigest is what one tool call asks for, as a digest of its arguments.
//
// CANONICAL, because a re-run must reproduce it: the arguments are JSON a
// model wrote and a decoder read, and encoding/json writes a map's keys in
// sorted order at every depth, so two decodes of the same call encode to the
// same bytes whatever order the model emitted them in.
//
// A FRESH VALUE WHERE THE ARGUMENTS CANNOT BE ENCODED, which a decoded JSON
// object never is — and the direction matters: a fresh digest makes a retry
// write twice, which is visible and bounded, where a constant one would make
// two different calls one operation and drop the second with a success.
func callDigest(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return uuid.NewString()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// opIDNamespace keeps a derived operation id from colliding with one another
// package derives from the same turn — a page comment is seeded from the same
// work key. FIXED for the life of the format: changing it makes every re-run
// straddling the change write twice.
const opIDNamespace = "crewlet.builtin.work"

// createdTaskID is the id of the task a create operation files: a UUIDv5 over
// the operation id.
//
// A FUNCTION OF THE OPERATION, because a task's id is the subject its create
// arbitrates on and the row it is guarded by. A fresh id per call made every
// re-run of one `create_work_item` — a turn redelivered after a crash, the
// same call repeated after an `unknown` — a different operation filing a
// different task, so one request left two items with two keys, and the retry
// identity [opIDFor] derives was never reached: the id was part of the
// operation's name. Derived from the operation, a re-run addresses the task
// the first run filed, and the tracker answers it with that task.
//
// Where the operation is fresh — a call with no turn identity to re-derive —
// so is the id, which is the same promise: one operation, one task.
func createdTaskID(opID string) string {
	return uuid.NewSHA1(createdTaskNamespace, []byte(opID)).String()
}

// createdTaskNamespace is the uuid namespace a created task's id is derived
// under. Fixed for the life of the format: it is durable in every task row.
var createdTaskNamespace = uuid.MustParse("8677bb1c-20e0-4fbe-ab44-846868b79b37")

// ---- update_work_item -------------------------------------------------- //

type updateWorkItem struct {
	deps WorkDeps

	// leads answers whether this seat leads the project a task is filed
	// in, which is the gate on `routing_unit`.
	//
	// A RE-ROUTE IS A LEAD'S, unlike the unit a create stamps: filing your
	// own work into your own team is what every seat does, and pointing
	// somebody ELSE's work at a different team is a decision about who
	// owns it.
	//
	// OR A PERSON'S OWN, which is the second arm at the gate itself and
	// not a fact about this seam: a lead relation is between two people in
	// the chart, and an operator's credential is in no chart — so this
	// lookup alone would have locked the company's own token out of the
	// verb entirely.
	leads LeadsProject
}

var _ tools.SeatCallable = (*updateWorkItem)(nil)

func (t *updateWorkItem) Name() string { return UpdateWorkItemTool }

func (t *updateWorkItem) Description() string {
	return "Change a work item: its status, assignee, priority, title, " +
		"description, labels, or whether you watch it. Only the fields you " +
		"pass are changed. Say WHY you closed something with " +
		"comment_on_work_item — an item that went to `cancelled` with no " +
		"word is one somebody has to reconstruct. Closing as a duplicate " +
		"also names the item that survives, in `duplicate_of`."
}

func (t *updateWorkItem) Parameters() map[string]any {
	return scheduleInto(map[string]any{
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
				"description": "The `task.version` from get_work_item. Given, the " +
					"edit is REFUSED if anybody changed the item since you " +
					"read it. Omitted, your fields are merged onto the " +
					"current item — which is usually what you want.",
			},
			"routing_unit": map[string]any{
				"type": "string",
				"description": "Point this item at a different team, by its " +
					"id or its name: that team's lead hears that work routes " +
					"to them now. The project lead's to set, or a person's " +
					"own — filing your own work into your own team is what " +
					"`unit` on create_work_item does.",
			},
			"waiting_on":   setArgSchema("The items this one is blocked BY. Each is a key or an id."),
			"blocking":     setArgSchema("The items blocked BY this one. Each is a key or an id."),
			"linked":       setArgSchema("Related items, with no blocking meaning. Each is a key or an id."),
			"linked_pages": setArgSchema("Knowledge-base pages this item references, by page id."),
			"fields": map[string]any{
				"type": "object",
				"description": "Custom fields, keyed by SLUG — read them with " +
					"get_work_catalogue or describe_project. A value is " +
					"checked against the field's own declaration and refused " +
					"naming the rule, never rounded or coerced to fit; null " +
					"clears a field.",
			},
			"dependency_note": map[string]any{
				"type": "string",
				"description": "One line saying WHY, recorded on every " +
					"dependency this call adds. Skipped on removals.",
			},
		},
		"required": []any{"item"},
	}, true)
}

// setArgSchema is the shape every set-valued argument takes.
//
// SPELLED OUT IN THE SCHEMA rather than left to the refusal, because the
// refusal only arrives after a model has already decided what it meant — and
// the reading it would otherwise pick, a bare list, is the one that silently
// drops every entry it did not repeat.
func setArgSchema(what string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": what + " Pass {\"add\": [...], \"remove\": [...]} to change part of the set, or {\"set\": [...]} to replace it entirely. Never a bare list.",
		"properties": map[string]any{
			"add":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"remove": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"set": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "REPLACES the whole set. Everything not listed is removed.",
			},
		},
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
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(UpdateWorkItemTool, err)), nil
	}

	patch, kind, refusal := patchFromArgs(args, actor, t.deps.now(), t.deps.zone())
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
		//
		// The outer refusal was checked empty above and is next WRITTEN
		// by declareLabels, so nothing reads a stale one.
		//nolint:govet // shadow: `x, refusal := f()` declares x too; see .golangci.yml
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
	// THE RE-ROUTE IS ITS OWN KIND AND ITS OWN GATE. `routed` carries
	// exactly one delta, and the new unit's lead hears it as an ORDINARY
	// candidate rather than a fallback — which is what makes the promise
	// "the new unit's lead learns work routes to them now" keepable on a
	// task that still has an assignee.
	if raw, held := args["routing_unit"]; held {
		unit := strings.TrimSpace(argString(map[string]any{"v": raw}, "v"))
		// EITHER AUTHORITY IS ENOUGH, and the operator half is why the
		// lookup is not the whole answer: it resolves a lead from the ORG
		// CHART by handle, and an operator's actor carries the TOKEN's own
		// name. Asked about [Actor.Handle] it therefore answered false for
		// every operator, a bound founder included, and the one surface a
		// person re-routes from could not use the verb at all. Asked about
		// [Actor.Record] it is the person behind the credential, and
		// [tracker.AuthorKind.Person] is the arm for a human at the
		// dashboard and for the token nobody bound — the same pair
		// `write_project` resolves, for the same reason.
		lead := t.leads != nil && t.leads(ctx, actor.Record(), before.Task.Project)
		if !lead && !actor.Kind.Person() {
			return failed(fmt.Sprintf("Pointing %s at a different team is the "+
				"lead of %s's decision, not yours. Ask them, or say in a "+
				"comment why it belongs elsewhere.",
				before.Task.Key, before.Task.Project)), nil
		}
		// AND THE CHART'S OWN KEY IS WHAT LANDS, for the reason the
		// filed unit above takes it: the wake resolves the stored value
		// back to a lead, and a spelling the chart did not choose is one
		// a rename walks away from.
		if unit != "" && t.deps.Units != nil {
			resolved, found := t.deps.Units.ResolveUnit(unit)
			if !found {
				return failed(fmt.Sprintf("This company has no team %q. A task "+
					"routed at a team nobody has reaches nobody at all.",
					clip(unit))), nil
			}
			unit = resolved.Key
		}
		patch.RoutingUnit = &unit
		if kind == tracker.ChangeFields {
			kind = tracker.ChangeRouted
		}
	}
	// THE INERT EDGES RIDE THE PATCH and the DEPENDENCIES DO NOT, because
	// a dependency has two ends: a `linked` edge is one collection on one
	// item, while `waiting_on` is an authored edge on one item and a
	// mirrored entry on another, which is a sequence rather than a field.
	inert, refusal := t.deps.inertRelations(ctx, UpdateWorkItemTool, args,
		before.Task)
	if refusal != "" {
		return failed(refusal), nil
	}
	if inert != nil {
		patch.Relate = inert
		if kind == tracker.ChangeFields {
			kind = tracker.ChangeRelations
		}
	}
	change, refusal := t.deps.dependencyChange(ctx, UpdateWorkItemTool, args, before.Task)
	if refusal != "" {
		return failed(refusal), nil
	}

	answer := map[string]any{"key": before.Task.Key, "labels_created": declared}
	// THE PATCH IS SKIPPED WHEN THIS CALL IS ONLY A DEPENDENCY CHANGE.
	// An empty patch is a real write — it stamps a version and writes a
	// history row — and spending one on a call that changed no field of
	// this item would put a `fields` commit in the feed that changed no
	// fields.
	if !patch.Empty() {
		got, err := writer.UpdateTask(ctx,
			opIDFor(actor, "update", before.Task.ID, args), before.Task.ID,
			before.Task.Project, ifMatch, patch, kind,
			tracker.Wake{
				Kind:   kind,
				Before: before.Task,
				After:  patched(before.Task, patch),
				Parent: t.deps.parentParty(ctx, before.Task, patch),
			}.Notify(t.deps.Leads))
		if err != nil {
			return failed(writeFailure(UpdateWorkItemTool, err)), nil
		}
		t.deps.settle(ctx, got.Position)
		answer["outcome"], answer["version"] = string(got.Outcome), got.Version
		answer["position"] = positionOf(got.Position)
		// THE WARNINGS THE WRITE PRODUCED, which today is the one the
		// coercion table can raise: a timestamp truncated to its date on
		// a field that holds no time. A change the engine made to a
		// value somebody typed is one they have to be told about, or the
		// board shows something they did not write with nothing saying
		// why.
		if len(got.Warnings) > 0 {
			answer["warnings"] = got.Warnings
		}
	}
	if !change.Empty() {
		if t.deps.Dependencies == nil {
			return failed(unconfiguredText(UpdateWorkItemTool)), nil
		}
		result, err := t.deps.Dependencies(actor).Depend(ctx,
			opIDFor(actor, "depend", before.Task.ID, args), change, t.deps.Leads)
		if err != nil {
			return failed(writeFailure(UpdateWorkItemTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		if _, held := answer["outcome"]; !held {
			answer["outcome"], answer["version"] = string(result.Outcome), result.Version
			answer["position"] = positionOf(result.Position)
		}
		// THE HALF-WRITTEN EDGES ARE REPORTED, never swallowed. A
		// dependency whose mirror lost its race is durable on the
		// authoring side and repaired by the tracker duty — so the
		// honest answer names it rather than either failing the call or
		// claiming it landed whole.
		if len(result.OneSided) > 0 {
			answer["dependencies_pending_mirror"] = len(result.OneSided)
		}
	}
	return jsonResult(answer)
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
		statelog.NewOpID(time.Now(), "tags-"+project), project, labels)
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
func patchFromArgs(args map[string]any, actor Actor, now time.Time,
	loc *time.Location) (tracker.TaskPatch, tracker.ChangeKind, string) {

	var patch tracker.TaskPatch
	kind := tracker.ChangeFields

	// THE SCHEDULING HALF FIRST, so a refusal about a date a model typed
	// arrives before anything else is decided — and so the kind below can
	// still be overridden by a status or an assignee, which is what a
	// change with both in it is actually about.
	plan, refusal := readSchedule(args, now, loc, UpdateWorkItemTool)
	if refusal != "" {
		return patch, kind, refusal
	}
	// THE DATES AND THE SIZING ARE `fields`, which is what they are.
	plan.applyToPatch(&patch)

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
		//
		// AND IT IS THE PERSON'S OWN WATCH — [Actor.Record], not the
		// author. A bound operator's `watch: true` wrote the TOKEN's name
		// into the watcher set, where the `watchers` history delta
		// rendered it as a colleague, the person's own seat was not
		// following the item, and every later wake dropped it against the
		// roster. Attribution is the other question and is unchanged: the
		// commit still names the credential.
		patch.Watch = &tracker.WatchIntent{Handle: actor.Record(), Watch: watch}
		if kind == tracker.ChangeFields {
			kind = tracker.ChangeWatchers
		}
	}
	if raw, held := args["fields"]; held {
		fields, refusal := fieldMap(raw)
		if refusal != "" {
			return patch, kind, refusal
		}
		// AND THE KIND STAYS `fields`, which is what a custom field
		// moving IS — the default this function already starts from, so
		// there is nothing to set. It is stated here because every other
		// arm in this function changes the kind, and a reader checking
		// why this one does not should find the answer rather than a
		// gap.
		patch.Fields = &fields
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
	// THE WRITER'S OWN MERGE, not a copy of it. This was a field-by-field
	// reimplementation, so every field added to [tracker.TaskPatch] had to
	// be remembered here too — and when the schedule fields arrived the
	// durable row took them and this snapshot did not. [tracker.TaskDeltas]
	// then compared a task against itself on exactly those fields, so a due
	// date, an estimate or a size a seat moved reached its notification as
	// a change that changed nothing.
	task = tracker.Patched(task, patch)

	if patch.Watch != nil {
		// THE GESTURE APPLIED TO THE SNAPSHOT THIS TOOL READ, which is
		// the one thing genuinely this caller's rather than the merge's.
		// The durable sets are the WRITER's, settled inside its own
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
			"ask": map[string]any{
				"type": "string",
				"description": "A colleague's handle: this comment is a " +
					"QUESTION they owe an answer to. They are woken asking " +
					"for one and start following the item. It does not hand " +
					"the item over, and it does not stop anybody closing it.",
			},
			"answers": map[string]any{
				"type": "string",
				"description": "The comment id of the question this answers, " +
					"which closes it. Omitted, it is inferred when exactly " +
					"one open question on the item is addressed to you.",
			},
			"reply_to": map[string]any{
				"type": "string",
				"description": "The id of the comment you are replying to. " +
					"This threads the conversation and wakes everybody " +
					"already in that thread; it does not close a question — " +
					"`answers` is what does.",
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
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatRead)
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
		ID:         commentID(actor, before.Task.ID, args),
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
	// THE ASK IS RESOLVED LIKE ANY OTHER HANDLE, because an ask that
	// names nobody the company has wakes nobody and leaves a question
	// open on the board for ever, addressed to a spelling.
	ask := strings.TrimSpace(argString(args, "ask"))
	if ask != "" {
		resolved, refusal := t.deps.resolveHandle(CommentOnWorkTool, "`ask`", ask)
		if refusal != "" {
			return failed(refusal), nil
		}
		ask = resolved
	}
	thread, refusal := t.deps.resolveThread(ctx, tracker.ThreadQuery{
		Task:    before.Task.ID,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
		Ask:     ask,
		Answers: strings.TrimSpace(argString(args, "answers")),
		// THE PERSON, because this is what the inference is asked about:
		// an ask is answered by the one it was ADDRESSED to, and an ask
		// names a seat. Under the token's own name a bound operator
		// answering the question put to their seat stamped nothing, and
		// the ask stayed open on the board.
		Author: actor.Record(),
	})
	if refusal != "" {
		return failed(refusal), nil
	}
	comment.Ask = thread.Asked
	if thread.Answers != "" {
		comment.Answers = &thread.Answers
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
			// THE PERSON'S, NOT THE CREDENTIAL'S — the same
			// [Actor.Record] the explicit gesture takes.
			Handle: actor.Record(), Watch: true, Auto: true,
		},
	}
	got, err := writer.UpdateTask(ctx,
		opIDFor(actor, "comment", comment.ID, args), before.Task.ID, before.Task.Project,
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
			Thread: thread.ThreadParties,
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(CommentOnWorkTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	answer := map[string]any{
		"comment_id": comment.ID, "item": before.Task.Key,
		"mentioned": comment.Mentions, "outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version": got.Version,
	}
	if comment.Ask != "" {
		answer["asked"] = comment.Ask
	}
	if comment.Answers != nil {
		answer["answered"] = *comment.Answers
	}
	// THE WARNING THE CALLER CANNOT SEE FOR THEMSELVES. A comment from
	// somebody who is not the assignee, naming nobody, still WAKES the
	// assignee — unaddressed, which a turn is entitled to absorb without
	// replying. A commenter expecting an answer therefore gets silence,
	// and nothing about the call says so.
	if warning := unansweredWarning(before.Task, actor, comment); warning != "" {
		answer["warnings"] = []string{warning}
	}
	return jsonResult(answer)
}

// unansweredWarning says when a comment will wake somebody who is not being
// asked anything.
func unansweredWarning(task tracker.Task, actor Actor, comment *tracker.Comment) string {
	switch {
	// AGAINST THE PERSON RATHER THAN THE AUTHOR: an assignee is a seat,
	// and a bound operator commenting on their own task matched nothing —
	// so the warning told them they were waking themselves.
	case task.Assignee == "" || task.Assignee == actor.Record():
		return ""
	case comment.Ask != "" || len(comment.Mentions) > 0:
		return ""
	}
	return fmt.Sprintf("%s is woken by this but is not asked to answer it. "+
		"Use ask: %s, or @-mention them, if you are expecting a reply.",
		task.Assignee, task.Assignee)
}

// commentID is the comment's own id, derived so a re-run turn posts once.
//
// A UUIDv5 over the operation and the task, because the id is a PRIMARY KEY on
// every node: two nodes applying one record must write one row, so an id
// generated at apply time would produce two.
//
// AND OVER THE CALL'S OWN ARGUMENTS, for the reason [opIDFor] gives: without
// them a seat's second remark on a task in one turn was the first one's id,
// so it upserted the first comment's row — or, inside the log's duplicate
// window, never landed at all while the tool answered that it had.
func commentID(actor Actor, taskID string, args map[string]any) string {
	seed := actor.OperationSeed()
	if seed == "" {
		seed = uuid.NewString()
	}
	return uuid.NewSHA1(commentNamespace, []byte(seed+"\x00"+taskID+"\x00"+
		actor.Handle+"\x00"+callDigest(args))).String()
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
// jsonResult is [jsonAnswer] for the answers whose size is bounded by their
// own shape — a write's receipt, a refusal, a handful of fields.
//
// THE CEILING STILL APPLIES, because "bounded by its own shape" is a claim
// about today's shape: a field added to a receipt is exactly how one of these
// stops being small, and the guard is what says so rather than a reader
// noticing a turn ran out of context.
func jsonResult(v any) (tools.Result, error) {
	return jsonAnswer(v, "Narrow what you asked for.")
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

// defaultProject is the caller's own, or empty where their unit owns none.
//
// ONE SPELLING, because five tools fall back to it and a second copy would
// be the one that stopped matching — a create filing into the seat's project
// while a describe answered about another is the shape that teaches a model
// the wrong vocabulary for the container it is writing to.
//
// IT TAKES THE ACTOR RATHER THAN A HANDLE, so that WHICH of an actor's names
// the chart is asked about is decided once here rather than at each call. It
// is [Actor.Record]: a home project is a fact about the PERSON, and a token
// is in no chart at all — so a bound founder's assistant was refused its own
// team's project by five tools that each spelled the identity separately.
func (d WorkDeps) defaultProject(a Actor) string {
	if d.DefaultProject == nil {
		return ""
	}
	return d.DefaultProject(a.Record())
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

// positionOf is the `position` a write's answer carries: where its record
// landed, in the form every read grammar takes back as `min_position`.
//
// THE ANSWER NAMES THE POSITION because that is what read-your-writes costs
// over a wire: a caller that reads through the dashboard, the REST route or
// its own client holds nothing else it could wait for. A seat's own reads are
// `linearizable` and need it for nothing — but the same tool answers are what
// the operator MCP returns to a person's assistant, and what a turn's trace
// shows a person redrawing a board, and "created at tracker@1:4711" is the one
// fact that lets either of them ask for an answer that includes it.
//
// NIL FOR AN UNKNOWN OUTCOME rather than a zero position: `unknown` means the
// broker never answered where the record went, and "@0:0" is a position that
// parses, which a client would hand straight back as a floor nothing waits
// for.
func positionOf(at statelog.Position) any {
	if at.IsZero() {
		return nil
	}
	return at.String()
}

// seatReadLevel is what every read on this surface uses, and [seatRead] is
// the same decision in the shape the point readers take.
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

// seatRead is [seatReadLevel] for the readers that take a whole freshness.
//
// NO BOUND AND NO FLOOR, deliberately: a seat reads `linearizable`, which
// establishes the log's end itself and takes no staleness bound, and the floor
// a wake carried was waited for before the turn opened (read-your-trigger),
// so there is nothing left for a tool call to name.
var seatRead = statelog.Freshness{Level: seatReadLevel}

// boardEmpty reports whether a grouped answer holds no task at all — which,
// on a closed axis, is a board of columns every one of which counts zero, and
// on a flat answer is no groups at all.
func boardEmpty(groups []tracker.Group) bool {
	for _, group := range groups {
		if group.Count > 0 {
			return false
		}
	}
	return true
}
