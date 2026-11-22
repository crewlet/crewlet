package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Worklet's tool names, re-exported from internal/worklet, which declares them
// because its notification prompt names them: a constant read from here would
// be an import cycle.
const (
	ListWorkItemsTool   = worklet.ListWorkItemsTool
	GetWorkItemTool     = worklet.GetWorkItemTool
	CreateWorkItemTool  = worklet.CreateWorkItemTool
	UpdateWorkItemTool  = worklet.UpdateWorkItemTool
	CommentOnWorkTool   = worklet.CommentOnWorkTool
	MergeWorkItemTool   = worklet.MergeWorkItemTool
	SearchWorkItemsTool = worklet.SearchWorkItemsTool
)

// WorkTools are Worklet's whole catalogue.
func WorkTools() []string { return worklet.Tools() }

// WorkWrites are the five that count as a DELIVERY to the delivery gate. Get
// and list are absent: a turn that only read is the one the gate catches.
func WorkWrites() []string { return worklet.WriteTools() }

// WorkReader is the tracker's read side: a board (many rows) and one task
// (everything about it) are two questions, so two shapes.
type WorkReader interface {
	Tasks(ctx context.Context, q worklet.Query, now time.Time) (worklet.Answer, error)
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
	// CarriedViews is a container's strip as list_work_views hands it to a
	// model, which save_work_view saves back whole: read whole or refused
	// (estate's tracker.views_carried), never shown short.
	CarriedViews(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error)
	// CarriedCatalogue is the catalogue as get_work_catalogue hands it to a
	// model, which write_work_catalogue takes back whole: read whole or
	// refused (estate's tracker.catalogue_carried), never shown short.
	CarriedCatalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error)
	Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (worklet.PersonState, error)
	Thread(ctx context.Context, q worklet.ThreadQuery,
		fresh statelog.Freshness) (worklet.ResolvedThread, error)
}

// WorkWriter is the tracker's write side.
//
// Every write takes an operation id minted once per caller-visible operation
// and stable across rounds and retries: a regenerated id defeats the ledger
// when an acknowledgement is lost.
type WorkWriter interface {
	CreateTask(ctx context.Context, opID string, task worklet.Task,
		notify *worklet.Notify) (worklet.WriteResult, error)
	// CreateTaskAsking files a task AS a question, task and ask in one record.
	CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
		ask worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error)
	// UpdateTask takes the change KIND beside the notification: what happened
	// and who is told are two facts, and the kind is never guessed.
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch worklet.TaskPatch, kind worklet.ChangeKind,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// WorkDepender writes a dependency, a SEQUENCE rather than a patch.
//
// Not part of [WorkWriter]: only a surface holding the replicated estate can
// read the counterparties a dependency checks before its first append. A build
// without one refuses the dependency arguments by name and serves every other
// edit.
type WorkDepender interface {
	Depend(ctx context.Context, opID string, change worklet.DependencyChange,
		leads worklet.Leads) (worklet.DependencyResult, error)
}

// WorkMerger folds one item into another, a SEQUENCE kept out of [WorkWriter]
// for [WorkDepender]'s reason: it reads a subtree and takes a fleet claim first.
type WorkMerger interface {
	MergeDuplicates(ctx context.Context, opID, duplicate, into string,
		reparent bool, notify *worklet.Notify) (worklet.WriteResult, error)
}

// WorkMover moves a top-level item and its subtree to another project, a
// SEQUENCE kept out of [WorkWriter] for [WorkMerger]'s reason.
type WorkMover interface {
	MoveTaskToProject(ctx context.Context, opID, taskID, target string,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// WorkDeps are the tracker halves plus what a write needs to attribute itself.
type WorkDeps struct {
	Reader WorkReader

	// ApprovesAct reports whether the person ctx resolves to may approve an act
	// of kind now, at the proof its approval asks (ADR-0125); nil admits nobody.
	ApprovesAct func(ctx context.Context, kind delegated.Kind) bool

	// ArtifactLink is the address the artifact tools hand out, so a decision
	// citing one cites the artifact ([ArtifactDeps.Link], ADR-0160); nil or empty
	// recognises none.
	ArtifactLink func(artifacts.ID) string

	// ChangeRef is a pull request's tracker name, where raw is one under the
	// company's GitHub owners, or why one on its host is refused; both empty, or
	// nil, and raw names an item.
	ChangeRef func(raw string) (ref, refused string)

	// Authorize decides the checks a tool can only take AFTER a read, and the
	// verb a write that copies a project's file asks before it reads. The
	// registration gate's own seam, pushed down by [Register] and [OperatorTools]
	// so the two cannot differ. Some verbs have their object only in the stored
	// row (re-routing is the lead of the task's HOME project's decision). Nil
	// refuses every `from_project_file` copy ([WorkDeps.copyFrom]).
	Authorize Authorizer

	// Writer resolves the write side FOR ONE ACTOR: a history row whose author
	// the caller chose is no audit trail. Each surface derives the actor from its
	// own immutable identity: the turn's bound seat, or the operator's credential.
	Writer func(actor Actor) WorkWriter

	// Dependencies resolves the dependency sequence for one actor. Nil where the
	// writer holds no replicated estate: the arguments are refused by name.
	Dependencies func(actor Actor) WorkDepender

	// Merges resolves the duplicate-merge sequence for one actor. Nil OMITS the
	// tool rather than refusing at the call: a catalogue advertising a tool that
	// always fails teaches a model to distrust all of them.
	Merges func(actor Actor) WorkMerger

	// Moves resolves the cross-project move for one actor; nil omits the tool.
	Moves func(actor Actor) WorkMover

	// Search ranks work items by text (worksearch.go says why it is its own
	// verb). A value: one corpus for everybody.
	Search WorkSearcher

	// ViewWriter resolves the saved-view write side for one actor, the
	// OPERATOR's surface alone: a seat's registration would implement a method
	// nothing there may call.
	ViewWriter func(actor Actor) ViewWriter

	// CatalogueWriter resolves the catalogue write side, operator-only as
	// ViewWriter is.
	CatalogueWriter func(actor Actor) CatalogueWriter

	// PersonWriter resolves the person write side for one actor, operator-only: a
	// seat is not a human, with a mailbox rather than an inbox.
	PersonWriter func(actor Actor) PersonWriter

	// Inbox reads what the company asked of somebody (the applier's notification
	// rows). A value, operator-only; whose inbox is the surface's authority.
	Inbox InboxReader

	// ProjectWriter resolves a project's settings for one actor. Every surface
	// has one: declaring a tag is open to every seat (else `labels` would be
	// unusable); other authority is resolved per call.
	ProjectWriter func(actor Actor) ProjectWriter

	// Files reads a project's files, FileWriter writes them for one actor, and
	// Objects stores their bytes. All three or no file tool: a file is a row
	// naming an object, and one half could only record content it cannot store,
	// or store content nothing names.
	Files      FileReader
	FileWriter func(actor Actor) FileWriter
	Objects    ObjectStore

	// TrashWriter resolves removal and restore for one actor, operator-only: a
	// seat hiding work from every list would leave no trace.
	TrashWriter func(actor Actor) TrashWriter

	// Placer resolves the board-drag side for one actor, operator-only: a board's
	// manual order is a person's arrangement. Its lane half is a status change,
	// made through [WorkDeps.Writer]. See workplace.go.
	Placer func(actor Actor) WorkPlacer

	// Seats is the company's roster, for validating a typed handle; nil admits
	// every handle. Resolved per call, like Units and DefaultProject: a seat's
	// tools are cloned into its lease and an apply does not rebuild the clone, so
	// a captured roster would go stale.
	Seats func() []colleague.Seat

	// FleetSeat is an agent seat as the fleet's active revision holds it, which
	// an answer choosing a grant is checked against (ADR-0118). Nil refuses every
	// grant choice.
	FleetSeat func(ctx context.Context, handle string) (*org.Role, error)

	// Grants is the engine's half of an agent's request for one integration
	// permission and its answer. Nil omits request_access's integration form and
	// refuses every answer choosing such a grant.
	Grants GrantDesk

	// Holders says whose record somebody else's LOGIN names ([iam.OwnerOf]'s
	// directory half), which every person verb resolves first. A login is never a
	// seat, so it never goes through [WorkDeps.Seats] (`jane.doe` would land on
	// the seat `jane`).
	//
	// Nil cannot say: a login somebody else holds is refused as undecidable,
	// unlike Seats' nil, since an unchecked handle is stored where its holder
	// finds it, an unresolved login where they never will. Every surface serving
	// a person verb wires the engine.
	Holders iam.Holders

	// Units resolves a project's chart-owned unit at READ time: the tracker holds
	// no org. Nil renders every unit unresolved, honest rather than empty.
	Units worklet.Units

	// Mentions resolves the handles a comment names, so a mention wakes the
	// person meant. Nil: only the ordinary watchers are told.
	Mentions MentionResolver

	// DefaultProject is where a seat files work naming no project: its unit's.
	// Empty makes the project argument required.
	DefaultProject func(handle string) string

	// Actor decides who a write is attributed to. Nil takes the turn's bound
	// seat: a model that could name its own actor could file work as anybody. The
	// OPERATOR surface sets it, having no turn and a person's credential as
	// writer; a seam, not a second copy of these tools, which would drift. It
	// takes the context because an operator's credential is on the request's
	// context; anything else would be a package variable shared by every
	// concurrent request.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Channels is where an asker may promise to report a decision: the chat
	// surfaces a seat posts on and the channels the chart declares. Read per call,
	// for Seats' reason. Nil refuses `decision.inform`.
	Channels ChannelDirectory

	// Leads resolves a wake's two fallbacks: a project's lead and a unit's. Nil
	// carries neither: only people already on the task are reached.
	Leads worklet.Leads

	// Now is the clock a query's relative dates resolve against, Zone the
	// company's zone they resolve in (ADR-0018): "due this week" is a calendar
	// boundary, and the wrong zone names a different week. Zone is read per call:
	// the operator surface is not rebuilt for a new clock. Nil is UTC.
	Now  func() time.Time
	Zone func() *time.Location

	// Await is handed the position every write landed at, so the next read sees
	// it (read-your-writes): a model that cannot see its own write files it
	// again. Applied after every write, never before a read. The wait happens in
	// the copy that answers next, never here (the router may ask another): every
	// surface hands the position, comparable only within its stream and
	// generation, to the node's session floor (the estate router's). Nil skips
	// it. A failure is logged and ignored: reporting a failed create for a task
	// that exists makes a duplicate.
	Await func(ctx context.Context, at statelog.Position) error

	// entries reads the atlas as a call's caller, for `linked_entries`, pushed
	// down beside Authorize.
	entries atlasReader

	// ops names a call's operation where neither a turn nor a request's key does
	// ([operations]), set by [OperatorTools] alone; zero takes no `op_id`.
	ops operations
}

// Actor is who a write is attributed to: the tool layer's own small type, so
// callers do not depend on the tracker's record format.
type Actor struct {
	// Handle is the record's AUTHOR and never empty: a seat's handle, or an
	// operator token's own name; [Actor.Kind] says which, so a renderer never
	// shows a credential as a colleague.
	Handle string
	Kind   worklet.AuthorKind

	// OperatorID is the credential the write was made under, recorded BESIDE
	// the author: a person acting through a token is attributed to the person,
	// and the token answers "what did this credential do".
	OperatorID string

	// Apps are the apps an OPERATOR (a credential acting as no seat) used when
	// this node resolved it (iam.ActorApps), nil for every other author. Carried
	// on every record a later turn reads (worklet's Writer.carries), so a turn it
	// wakes is worked no wider (ADR-0109, ADR-0112). Set by [ActorOf] alone.
	Apps []iam.Grant

	// WrittenFor is whom an AGENT's writes are made for: whoever its turn works
	// for, from the call's bound ceiling ([actorFor], [writtenForTurn]), carried
	// like Apps (ADR-0109). The tracker refuses an agent's write stating nothing
	// ([types.WrittenFor.For]); a site building an agent's actor without its turn
	// says why ([types.ForNobody]).
	WrittenFor types.WrittenFor

	// TurnID is the RUN that produced this write: provenance for an audit.
	TurnID string

	// WorkKey is the unit of work behind that run, which derived operation and
	// comment ids are seeded from: it survives a re-run, where a redelivered
	// trigger gets a new TurnID (ADR-0017).
	WorkKey string

	// WorkSince is when that unit of work BEGAN ([turnctx.Turn.WorkSince]): the
	// instant every id derived from WorkKey carries, so a re-run must reproduce
	// it ([Actor.OperationSince]). Set by the turn, or a request's key by
	// [Actor.Keyed] alone.
	WorkSince time.Time

	// RebasedTo is the instant a derived operation id carries in place of its
	// identity's own start, zero for that start; see [turnctx.Turn.RebasedTo].
	RebasedTo time.Time

	// Calls is the run's call log, which a call's key reads its repeat count
	// from ([Actor.callKey]). Nil outside a turn.
	Calls *turnctx.CallLog

	// Operation is this CALL's operation id on a surface with no turn (the
	// operator's assistant over MCP): the `op_id` the caller brought back, or one
	// minted and answered ([WorkDeps.bindOperation]). Every write derives its id
	// from it ([opIDFor], [commentID]), so a call brought back repeats the same
	// writes (package doc, "How a write is made once"). Empty in a turn and on a
	// transport carrying its own key ([Actor.WorkKey]).
	Operation string

	Chain []string

	// Thread is the chat surface and conversation the turn was woken on, which
	// every task it files states ([worklet.Origin]); nil for none.
	Thread *worklet.Origin

	// Written is the turn's record of the items its writes committed to
	// ([turnctx.Written]), which lets the engine charge a turn dispatch named no
	// item for to the one it wrote. Nil outside a turn.
	Written *turnctx.Written
}

// OperationSeed is what a derived, idempotent id is built from: the work key
// where there is one (the identity a redelivery reproduces, so a re-run's writes
// collapse into the first attempt's), else the run (a call repeated in one run
// must write once). Outside a turn, the request's `Idempotency-Key`
// ([Actor.WorkKey], internal/api/opkey); else empty ([Actor.Operation] names
// it), since an invented seed lies about what produced the write and a stable
// one would collapse every later write into the first.
func (a Actor) OperationSeed() string {
	if a.WorkKey != "" {
		return a.WorkKey
	}
	return a.TurnID
}

// Keyed is this actor carrying a request's operation key: the ONE place a
// transport's key goes onto an actor, dated by the key's own instant
// ([statelog.OpMintedAt]), which every derived id carries.
//
// A key carrying no instant is refused ([statelog.CheckCallerOpID]) rather than
// dated at the epoch, where every derived id would read as older than any ledger
// loss and be answered `unknown` on every attempt. An empty key is no key.
func (a Actor) Keyed(key string) (Actor, error) {
	if key == "" {
		return a, nil
	}
	if err := statelog.CheckCallerOpID(key); err != nil {
		return Actor{}, fmt.Errorf("builtin: a request's operation key is an "+
			"operation id this engine minted, carrying the instant it was "+
			"minted: %w", err)
	}
	at, _ := statelog.OpMintedAt(key)
	a.WorkKey, a.WorkSince = key, at
	return a, nil
}

// keyedRequest reports whether this caller is a REQUEST whose transport carries
// its own operation key ([Actor.WorkKey]), not a turn or the operator's
// assistant. Read off the identities held, not a flag every constructor must set
// alike: a turn's actor always names its run. It decides what the caller is told
// ([sameCall], [reusedOperation]) and that the key is used as sent, not as a
// seed ([Actor.callKey]).
func (a Actor) keyedRequest() bool { return a.WorkKey != "" && a.TurnID == "" }

// OperationSince is the instant an id derived from [Actor.OperationSeed]
// carries: when its identity BEGAN (the work's start, else the run's).
//
// Never the instant of the call: the ledger cannot vouch for an operation minted
// before its watermark, and a re-run's call always follows the loss it is judged
// against ([statelog.OpMintedAt]). Zero where neither is known (a hand-built
// turn): older than every loss, answered `unknown` unless its row is held.
//
// A rebase ([Actor.RebasedTo]) outranks both: a start older than what the ledger
// may have swept answers every write `unknown` on every node.
func (a Actor) OperationSince() time.Time {
	if !a.RebasedTo.IsZero() {
		return a.RebasedTo
	}
	if a.WorkKey != "" {
		return a.WorkSince
	}
	at, _ := statelog.OpMintedAt(a.TurnID)
	return at
}

// asBegun is this actor with the rebase put aside ([Actor.RebasedTo]), so a key
// derived from it ([Actor.callKey]) carries the work's start on every attempt.
// For a key naming a ROW ([commentID], [createdTaskID], [createdViewID]): a
// rebase concerns the operation ledger alone, and the rows remember what the
// ledger forgot.
func (a Actor) asBegun() Actor {
	a.RebasedTo = time.Time{}
	return a
}

// settle hands a write's position to [WorkDeps.Await], best effort. The
// applier's own budget bounds the wait.
func (d WorkDeps) settle(ctx context.Context, at statelog.Position) {
	if d.Await == nil || at.Seq == 0 {
		return
	}
	if err := d.Await(ctx, at); err != nil {
		log.WarnContext(ctx, "work_write_not_applied_yet",
			"position", at.String(), "error", err.Error(),
			"detail", "the write landed on the fleet's log; the copy this "+
				"surface reads has not applied it yet, so a list in this same "+
				"turn may not show it")
	}
}

// MentionResolver turns the handles a comment's text names into seats.
type MentionResolver interface {
	// Mentions returns the handles this text addresses that are seats here;
	// anything else is dropped, since nobody can be sent that notification.
	Mentions(text string) []string
}

// ---- the seat identity every write is attributed to -------------------- //

// actorFor builds the write's attribution from the TURN, never from arguments:
// a model that could name its own actor could file work as anybody. The turn id
// and chain are provenance only (a hand-off is charged to the item's
// reassignment counter, not the delegation depth). Whom the turn works for comes
// from the call's bound ceiling ([writtenForTurn], ADR-0109). r is the composer
// of a family asking outside its gate, nil where every call passes the gate.
func actorFor(ctx context.Context, turn *turnctx.Turn, r Requesters) (Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return Actor{}, err
	}
	writtenFor, err := writtenForTurn(ctx, turn, seat.Handle(), r)
	if err != nil {
		return Actor{}, err
	}
	return Actor{
		Handle:     seat.Handle(),
		Kind:       worklet.AuthorAgent,
		WrittenFor: writtenFor,
		TurnID:     turn.RunID,
		WorkKey:    turn.WorkKey,
		WorkSince:  turn.WorkSince,
		RebasedTo:  turn.RebasedTo,
		Chain:      turn.Chain,
		Thread:     threadOf(turn),
		Written:    turn.Written,
	}, nil
}

// writtenForTurn is whom a seat's write in turn is made for ([types.ForTurn]):
// whoever the turn works for, from the call's bound ceiling ([ceilingOf],
// [workingForOf]), never anything a model writes; nil for a turn working for its
// own seat. The gate binds the ceiling ([bindCeiling]), so r is asked only by a
// family resolving its caller outside a gate ([CodeDeps.ResolveRunRepos]). A
// ceiling that cannot be composed is [ErrUndecidable], never a turn nobody woke:
// a write carrying nobody would wake whoever it reaches for this seat.
func writtenForTurn(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (types.WrittenFor,
	error) {

	ceiling, err := ceilingOf(ctx, turn, seat, r)
	if err != nil {
		return types.WrittenFor{}, fmt.Errorf("%w: %w", ErrUndecidable, err)
	}
	return types.ForTurn(workingForOf(ceiling)), nil
}

// unattributed is what a call answers when nobody could be named to write or
// read as ([WorkDeps.actor], [PageDeps.actor]): a turn whose requester could not
// be composed is unavailable for now (what a seat writes carries that requester
// to whoever it wakes); any other caller has no seat and no principal
// ([notInATurn]).
func unattributed(name string, err error) tools.Result {
	if errors.Is(err, ErrUndecidable) {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not establish who this "+
			"turn's work is for, which everything this seat writes carries to whoever it wakes. "+
			"Nothing was done. Try again shortly.", name))
	}
	return notInATurn(name)
}

// threadOf is where a turn was woken, as a create states it, or nil.
func threadOf(turn *turnctx.Turn) *worklet.Origin {
	if turn == nil || strings.TrimSpace(turn.Transport) == "" {
		return nil
	}
	return &worklet.Origin{Surface: turn.Transport, Conversation: turn.ConversationKey}
}

// Provenance is the trail this actor's writes carry and where they report the
// items they commit to: one construction for every writer, so none drops the
// turn's write log.
func (a Actor) Provenance() worklet.Provenance {
	provenance := worklet.Provenance{
		OperatorID: a.OperatorID, TurnID: a.TurnID, Chain: a.Chain,
		Origin: a.Thread, Apps: slices.Clone(a.Apps), WrittenFor: a.WrittenFor,
	}
	// Nil stays nil: a nil *Written in the interface is a non-nil log that
	// reports into nothing.
	if a.Written != nil {
		provenance.Written = a.Written
	}
	return provenance
}

// actor resolves who this call writes as ([WorkDeps.Actor]) and, for a seat's
// turn, whom it writes for ([actorFor]); [ErrUndecidable] is answered as
// [unattributed] does. The call log is always the turn's: a resolver that
// dropped it would hand a repeated call its first copy's operation id.
func (d WorkDeps) actor(ctx context.Context, turn *turnctx.Turn) (Actor, error) {
	resolve := func(turn *turnctx.Turn) (Actor, error) { return actorFor(ctx, turn, nil) }
	if d.Actor != nil {
		resolve = func(turn *turnctx.Turn) (Actor, error) { return d.Actor(ctx, turn) }
	}
	actor, err := resolve(turn)
	if err != nil {
		return Actor{}, err
	}
	actor.Calls = turn.CallLog()
	return actor, nil
}

// turnKey is the idempotency key a turn's derived ids are seeded from, or ""
// outside a turn ([Actor.Operation], [Actor.WorkKey]).
func turnKey(turn *turnctx.Turn) string {
	return turnIdentity(turn).OperationSeed()
}

// turnIdentity is the part of a turn a derived id is built from (seed, instant
// and call log) as an [Actor], so all follow the actor's one rule.
func turnIdentity(turn *turnctx.Turn) Actor {
	if turn == nil {
		return Actor{}
	}
	return Actor{TurnID: turn.RunID, WorkKey: turn.WorkKey, WorkSince: turn.WorkSince,
		RebasedTo: turn.RebasedTo, Calls: turn.CallLog()}
}

// runIdentity is the turn's RUN, never its unit of work, as the identity for an
// operation that is one per run whatever work it serves ([holdOpID]). An
// [Actor] so its instant follows [Actor.OperationSince].
func runIdentity(turn *turnctx.Turn) Actor {
	if turn == nil {
		return Actor{}
	}
	return Actor{TurnID: turn.RunID, RebasedTo: turn.RebasedTo}
}

// notInATurn is the refusal every one of these tools gives outside a turn.
// [tools.RefusalForbidden]: the arguments are fine; a caller with no seat and no
// bound person may not write as anybody.
func notInATurn(name string) tools.Result {
	return refused(tools.RefusalForbidden,
		name+" can only be called during a turn, on behalf of a seat.")
}

// unconfigured is the refusal when the company does not run Worklet.
// [tools.RefusalUnavailable]: the object may exist in the tracker the company
// DOES run, so neither "invalid" nor "not found" is true.
func unconfigured(name string) tools.Result {
	return refusedBy(tools.RefusalUnavailable, waitless(), name+" is unavailable: this "+
		"company does not run Worklet. Use the tracker tools your company has "+
		"configured.")
}

// resolveRef turns what a model typed (a key or an id) into the task ID every
// relation and parent pointer is written with ([worklet.Relation.Other],
// [worklet.Task.Parent]): a stored KEY is an edge resolving to nothing on every
// node forever. A refusal is the tool's RESULT: no such item is
// [tools.RefusalInvalid]; an unserved read is [tools.RefusalUnavailable]
// ([readFailure]).
func (d WorkDeps) resolveRef(ctx context.Context, tool, field,
	ref string) (string, *tools.Result) {

	task, refused := d.resolveTask(ctx, tool, field, ref)
	return task.ID, refused
}

// resolveTask is [WorkDeps.resolveRef] answering the whole task.
func (d WorkDeps) resolveTask(ctx context.Context, tool, field,
	ref string) (worklet.Task, *tools.Result) {

	if d.Reader == nil {
		return worklet.Task{}, refusalOf(unconfigured(tool))
	}
	got, err := d.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return worklet.Task{}, refusalOf(failed(fmt.Sprintf("%s names %s %q "+
			"and there is no such work item. Check the key with "+ListWorkItemsTool+" "+
			"rather than guessing — a link to an item that does not exist "+
			"renders as a dead reference on everybody's board.", tool, field,
			clip(ref))))
	case err != nil:
		return worklet.Task{}, refusalOf(readFailure(ctx, tool, err))
	}
	return got.Task, nil
}

// refusalOf is a refusal as a resolving helper's optional second answer.
func refusalOf(r tools.Result) *tools.Result { return &r }

// resolveHandle turns a handle a caller typed into one the company has.
//
// An unknown handle fails silently and permanently: [worklet.Route] drops it
// with no error or metric, and the write answers `applied`. Checked here, not in
// the tracker, which holds no chart: an applier reading one would write
// different rows on nodes briefly on different epochs. Resolves through
// `lookup_colleague`'s machinery, so a role's name yields the canonical handle.
// A nil roster admits everything. A refusal lists seats only to a caller who may
// read the roster ([WorkDeps.mayReadRoster]).
func (d WorkDeps) resolveHandle(ctx context.Context, tool, field,
	arg string) (string, string) {

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
		missing := fmt.Sprintf("%s names %s %q and there is nobody here by "+
			"that name — a handle nobody has is stored, and then every "+
			"notification to it is dropped in silence.",
			tool, field, clip(handle))
		if !d.mayReadRoster(ctx) {
			return "", missing
		}
		return "", fmt.Sprintf("%s Look them up with %s rather than guessing. "+
			"The seats are: %s.", missing, LookupColleagueTool,
			words.And(allHandles(seats)))
	}
	if !d.mayReadRoster(ctx) {
		return "", fmt.Sprintf("%s names %s %q and more than one person here "+
			"answers to it. Name them by their exact handle.",
			tool, field, clip(handle))
	}
	return "", fmt.Sprintf("%s names %s %q and it matches %s. Name one of them "+
		"exactly.", tool, field, clip(handle), words.Or(matchHandles(found)))
}

// mayReadRoster reports whether this call's caller may read the company's
// roster, as `lookup_colleague` is decided ([authz.ClassEverybody]).
// Undecidable is a no: withheld names cost a lookup, leaked ones the chart.
func (d WorkDeps) mayReadRoster(ctx context.Context) bool {
	return d.Authorize != nil && d.Authorize(ctx, authz.ActionColleagueRead,
		authz.Object{Kind: authz.KindPerson}) == nil
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

// opIDFor is the operation id one tool call writes under (package doc, "How a
// write is made once"). A caller's [Actor.Operation] makes each write a STEP of
// it ([WorkDeps.bindOperation] binds its arguments). A turn's or request's key
// derives the id from [Actor.callKey], verb, object and the arguments' digest (a
// request's key names the request, not what it asked), at
// [Actor.OperationSince], under [opIDNamespace]. With neither it is fresh per
// call: a stable id has the ledger collapse later writes as redeliveries. The
// object is one escaped, bounded segment ([statelog.OpNamePart]), since a dot
// begins a STEP; tool keeps the call's repeat count.
func opIDFor(actor Actor, tool, verb, object string, args map[string]any) string {
	name := verb + "-" + statelog.OpNamePart(object)
	if actor.Operation != "" {
		return statelog.StepOpID(actor.Operation, name)
	}
	key := actor.callKey(tool, nil, args, "")
	if key == "" {
		return statelog.NewOpID(time.Now(), name)
	}
	return statelog.DeriveOpID(actor.OperationSince(), name, opIDNamespace, key, verb,
		object, turnctx.ArgsDigest(args))
}

// opIDNamespace scopes [opIDFor]'s derivation from a call's key, FIXED for the
// life of the format (package doc, "What the tracker's tools derive from a
// call's key").
const opIDNamespace = "crewlet.builtin.work"

// callOutcome is one tool call's answer over EVERY record it appended ([joinDepend]'s
// rule), for a tool reporting its records facet by facet (a project's tags and
// policy, the catalogue's types and fields): every transport reads the call's
// outcome and position from the top of the answer and barriers on it.
type callOutcome struct {
	outcome statelog.Outcome
	at      statelog.Position
}

// add folds one record's result in; one that appended nothing is left out
// ([statelog.Result.Wrote]).
func (c *callOutcome) add(r statelog.Result) {
	if !r.Wrote() {
		return
	}
	c.outcome = statelog.LessCertain(c.outcome, r.Outcome)
	c.at = statelog.Later(c.at, r.Position)
}

// stamp writes the call's answer onto the top of a tool's result. A call that
// appended nothing is `applied` at no position.
func (c callOutcome) stamp(answer map[string]any) {
	outcome := c.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	answer["outcome"], answer["position"] = string(outcome), positionOf(c.at)
}

// partlyWritten is the refusal of a call whose LATER record was refused after an
// earlier one landed: the later write's cause, and what landed and where, as
// save_page does for a refused rename. "The change was NOT made" would have a
// caller redo, or report undone, a change that is on every node.
func partlyWritten(failure tools.Result, landed, refusedPart string, call callOutcome) tools.Result {
	outcome := call.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	where := "and appended nothing"
	if !call.at.IsZero() {
		where = "at " + call.at.String()
	}
	return tools.Result{Failed: true, Cause: failure.Cause, Output: fmt.Sprintf(
		"%s (%s, %s) and %s were not: %s", landed, outcome, where, refusedPart,
		failure.Output)}
}

// bindOperation settles which operation this call is, or answers the refusal
// ([operations.bindActor]).
func (d WorkDeps) bindOperation(actor Actor, tool string,
	args map[string]any) (Actor, string) {

	return d.ops.bindActor(actor, tool, args)
}

// withOperation puts the `op_id` a caller brings back on its answer
// ([WorkDeps.bindOperation]). Never for a request, which repeats its key and
// would have an `op_id` refused.
func withOperation(answer map[string]any, actor Actor) map[string]any {
	if actor.Operation != "" {
		answer[opIDArg] = actor.Operation
	}
	return answer
}

// now and zone are the clock a query resolves against, defaulting to UTC rather
// than a zero time nothing can compare.
func (d WorkDeps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now()
}

// defaultProject is the caller's own, or empty where their unit owns none. It
// asks about the actor's author: a person bound to a seat writes AS it
// ([PrincipalActor]); an unbound credential is in no chart.
func (d WorkDeps) defaultProject(a Actor) string {
	if d.DefaultProject == nil {
		return ""
	}
	return d.DefaultProject(a.Handle)
}

func (d WorkDeps) zone() *time.Location {
	if d.Zone == nil {
		return time.UTC
	}
	if loc := d.Zone(); loc != nil {
		return loc
	}
	return time.UTC
}

// openGroups is the status groups where work has not finished, derived so a new
// group cannot leave a stale literal.
func openGroups() []string {
	out := make([]string, 0, len(worklet.StatusGroups))
	for _, group := range worklet.StatusGroups {
		if group.Open() {
			out = append(out, string(group))
		}
	}
	return out
}

// positionOf is the `position` a write's answer carries, in the form every read
// grammar takes back as `min_position`: over a wire it is all a caller can wait
// for. Nil for an unknown outcome, never "@0:0", which parses and would be a
// floor nothing waits for.
func positionOf(at statelog.Position) any {
	if at.IsZero() {
		return nil
	}
	return at.String()
}

// positionText is [positionOf] for a typed answer: `<stream>@<generation>:<sequence>`,
// "" for none. The act transport reads it as that string (operator.receiptOf)
// and lifts it as the dashboard's read floor; an object there is unreadable.
func positionText(at statelog.Position) string {
	if at.IsZero() {
		return ""
	}
	return at.String()
}

// seatReadLevel is what every read on this surface uses ([seatRead] is the same
// in the point readers' shape). A name, never a per-site literal: `session` with
// no high-water mark populated served the committed prefix as a stale read
// wearing a stronger name. A seat's reads DECIDE things and it has no screen to
// notice staleness on. The operator MCP serves these tools at the same level;
// neither lets the caller choose ([statelog.SettableLevels]).
var seatReadLevel = statelog.DefaultReadLevel(statelog.SurfaceSeat)

// seatRead is [seatReadLevel] for the readers that take a whole freshness.
//
// No bound and no floor: `linearizable` establishes the log's end itself, and a
// wake's position is already in the node's floors from the turn's start (the
// engine's runTurn). The ranked searches carry no floor: an index has no log
// position.
var seatRead = statelog.Freshness{Level: seatReadLevel}

// boardEmpty reports whether a grouped answer holds no task: on a closed
// axis, every column counts zero; on a flat answer, there are no groups.
func boardEmpty(groups []worklet.Group) bool {
	for _, group := range groups {
		if group.Count > 0 {
			return false
		}
	}
	return true
}
