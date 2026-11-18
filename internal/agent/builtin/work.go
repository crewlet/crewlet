package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
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

// WorkTools are Worklet's whole catalogue, so a caller registering them names
// one thing.
func WorkTools() []string { return worklet.Tools() }

// WorkWrites are the five that count as a DELIVERY to the delivery gate, so a
// turn that moved, commented on, filed or merged an item is not looped for
// "doing nothing". Get and list are absent: a turn that only read is the turn
// the gate exists to catch.
func WorkWrites() []string { return worklet.WriteTools() }

// WorkReader is what these tools need from the tracker's read side: a board
// (many rows, what a card renders) and one task (everything about it, from
// many tables) are two questions, so two shapes.
type WorkReader interface {
	Tasks(ctx context.Context, q worklet.Query, now time.Time) (worklet.Answer, error)
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
	// CarriedViews is a container's strip as list_work_views hands it to a
	// model, which save_work_view saves back whole by id: read whole or
	// refused where this build would drop part of it (estate's
	// tracker.views_carried), never shown short.
	CarriedViews(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error)
	// CarriedCatalogue is the catalogue as get_work_catalogue hands it to a
	// model, which write_work_catalogue takes back whole: read whole or
	// refused where this build would drop part of it (estate's
	// tracker.catalogue_carried), never shown short.
	CarriedCatalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error)
	Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (worklet.PersonState, error)
	Thread(ctx context.Context, q worklet.ThreadQuery,
		fresh statelog.Freshness) (worklet.ResolvedThread, error)
}

// WorkWriter is what these tools need from the tracker's write side.
//
// Every write takes an operation id minted once per caller-visible operation
// and stable across rounds and retries: a regenerated id defeats the ledger in
// the lost-acknowledgement case it exists for.
type WorkWriter interface {
	CreateTask(ctx context.Context, opID string, task worklet.Task,
		notify *worklet.Notify) (worklet.WriteResult, error)
	// CreateTaskAsking files a task AS a question: the task and its ask in
	// one record, so neither lands without the other.
	CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
		ask worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error)
	// UpdateTask takes the change KIND beside the notification because they
	// are two facts, what happened and who is told; the kind must not be
	// guessed when nobody is listening.
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch worklet.TaskPatch, kind worklet.ChangeKind,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// WorkDepender writes a dependency, a SEQUENCE rather than a patch.
//
// Not part of [WorkWriter] because only a surface holding the replicated
// estate can read the counterparties a dependency checks before its first
// append. A build without one refuses the dependency arguments by name and
// still serves every other edit.
type WorkDepender interface {
	Depend(ctx context.Context, opID string, change worklet.DependencyChange,
		leads worklet.Leads) (worklet.DependencyResult, error)
}

// WorkMerger folds one item into another, a second SEQUENCE, kept out of
// [WorkWriter] for [WorkDepender]'s reason: it reads a subtree and takes a
// fleet claim before its first append.
type WorkMerger interface {
	MergeDuplicates(ctx context.Context, opID, duplicate, into string,
		reparent bool, notify *worklet.Notify) (worklet.WriteResult, error)
}

// WorkMover moves a top-level item, and its whole subtree, to another
// project — the third SEQUENCE here, and its own seam for [WorkMerger]'s
// reason: it reads the subtree and takes a fleet claim before its first append.
type WorkMover interface {
	MoveTaskToProject(ctx context.Context, opID, taskID, target string,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// WorkDeps are the tracker halves plus what a write needs to attribute itself.
type WorkDeps struct {
	Reader WorkReader

	// ApprovesAct reports whether the person ctx resolves to may approve an
	// act of kind now, at the proof its approval asks (ADR-0125); nil admits
	// nobody, so an answer approving an act is refused by the tracker.
	ApprovesAct func(ctx context.Context, kind delegated.Kind) bool

	// ChangeRef is a pull request's tracker name, where raw is one under the
	// company's GitHub owners, or why one on its host is refused; both empty,
	// or nil, and raw names an item.
	ChangeRef func(raw string) (ref, refused string)

	// Authorize decides the checks a tool can only take AFTER a read, and the
	// verb a write that copies a project's file asks before it reads.
	//
	// The registration gate's own seam, pushed down by [Register] and
	// [OperatorTools] so a caller cannot set the two differently. A few verbs
	// have their object only in the stored row (re-routing is the lead of the
	// task's HOME project's decision), so they ask here against the same
	// table. A write copying a project's file (an artifact's
	// `from_project_file`) asks read_project_file's verb here too
	// ([WorkDeps.copyFrom]), so nil refuses every such copy.
	Authorize Authorizer

	// Writer resolves the tracker's write side FOR ONE ACTOR: a writer acts as
	// exactly one party, since a history row whose author the caller chose is
	// no audit trail. Each surface derives the actor from its own immutable
	// identity: the turn's bound seat, or the operator request's credential.
	Writer func(actor Actor) WorkWriter

	// Dependencies resolves the dependency sequence for one actor, as
	// [WorkDeps.Writer] does. Nil on a build whose writer holds no replicated
	// estate, and the dependency arguments are then refused by name.
	Dependencies func(actor Actor) WorkDepender

	// Merges resolves the duplicate-merge sequence for one actor, as
	// [WorkDeps.Dependencies] does. Nil OMITS the merge tool rather than
	// refusing at the call: a catalogue advertising a tool that always fails
	// teaches a model to distrust all of them.
	Merges func(actor Actor) WorkMerger

	// Moves resolves the cross-project move FOR ONE ACTOR, in the shape
	// and for the reason [WorkDeps.Merges] is; nil omits the tool.
	Moves func(actor Actor) WorkMover

	// Search ranks work items by text (worksearch.go says why it is its own
	// verb). A value rather than a function of the actor: it reads the same
	// corpus for everybody, so there is nothing to attribute.
	Search WorkSearcher

	// ViewWriter resolves the saved-view write side for one actor, as
	// [WorkDeps.Writer] does. Separate from Writer because only the
	// OPERATOR's surface has one: folded in, a seat's registration would
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

	// Inbox reads what the company asked of somebody (the applier's own
	// notification rows). A value, for [WorkDeps.Search]'s reason; whose
	// inbox is the surface's authority, not this seam's. The operator
	// surface's alone, as [WorkDeps.PersonWriter] is.
	Inbox InboxReader

	// ProjectWriter resolves a project's own settings for one actor. Every
	// surface has one, because declaring a tag is open to every seat (else
	// the `labels` argument of create and update would be unusable); the
	// authority for the rest is resolved per call.
	ProjectWriter func(actor Actor) ProjectWriter

	// Files reads a project's files, FileWriter writes them for one actor,
	// and Objects stores and reads their bytes. All three or no file tool:
	// a file is a row naming an object, and a surface holding one half could
	// only record content it cannot store, or store content nothing names.
	Files      FileReader
	FileWriter func(actor Actor) FileWriter
	Objects    ObjectStore

	// TrashWriter resolves the removal and restore side for one actor, and is
	// the operator surface's alone: a removal hides a task from every list,
	// and a seat that could hide work it did not want would leave no trace.
	TrashWriter func(actor Actor) TrashWriter

	// Placer resolves the board-drag side for one actor, the operator
	// surface's alone for [WorkDeps.ViewWriter]'s reason: a board's manual
	// order is a person's arrangement. Its lane half is a status change, which
	// every surface makes through [WorkDeps.Writer]. See workplace.go.
	Placer func(actor Actor) WorkPlacer

	// Seats is the company's roster, for validating a typed handle; nil admits
	// every handle (no chart loaded). A function resolved per call, like
	// [WorkDeps.Units] and [WorkDeps.DefaultProject]: a seat's tools are
	// cloned into its lease and an apply does not rebuild the clone, so a
	// captured roster would validate against an org that has since moved.
	Seats func() []colleague.Seat

	// FleetSeat is an agent seat as the fleet's active revision holds it,
	// which an answer choosing a grant is checked against (ADR-0118). Nil
	// refuses every grant choice: the grant cannot be confirmed.
	FleetSeat func(ctx context.Context, handle string) (*org.Role, error)

	// Grants is the engine's half of an agent's request for one
	// integration permission and of its answer. Nil omits request_access's
	// integration form and refuses every answer choosing such a grant.
	Grants GrantDesk

	// Holders says whose record somebody else's LOGIN names: the identity
	// directory's half of [iam.OwnerOf], which every person verb resolves its
	// name through first. A login is never a seat, so it goes here and never
	// through [WorkDeps.Seats] (where `jane.doe` lands on the seat `jane`).
	//
	// Nil cannot say, and a login somebody else holds is then refused as
	// undecidable, unlike Seats' nil: an unchecked handle is stored where its
	// holder finds it, an unresolved login where they never will. Every
	// surface serving a person verb wires the engine.
	Holders iam.Holders

	// Units resolves a project's chart-owned unit at READ time — the
	// tracker holds no org, because the applier may not read one. Nil
	// renders every unit unresolved, which is honest rather than empty.
	Units worklet.Units

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
	// Nil takes the turn's bound seat, the only correct answer in-engine: a
	// model that could name its own actor could file work as anybody. The
	// OPERATOR surface sets it, having no turn and a person's credential as
	// writer; a seam rather than a second copy of these tools, which would
	// drift on trimming, defaults and refusals. It takes the context too
	// because an operator's credential is on the request's context; anything
	// else would be a package variable shared by every concurrent request.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Channels is where an asker may promise to report a decision: the chat
	// surfaces a seat posts on and the channels the chart declares. Read per
	// call, for [WorkDeps.Seats]'s reason. Nil refuses `decision.inform`: no
	// chart to check it against.
	Channels ChannelDirectory

	// Leads resolves the two fallbacks a wake may need — a project's lead
	// and a unit's. Nil carries neither, which degrades to a change that
	// reaches the people already on the task and nobody else.
	Leads worklet.Leads

	// Now is the clock a query's relative dates resolve against, and Zone the
	// company's zone they resolve in (ADR-0018): "due this week" is a calendar
	// boundary, and the wrong zone names a different week. Zone is read per
	// call, for the chart seams' reason: the operator surface is not rebuilt
	// for a new clock. Nil is UTC.
	Now  func() time.Time
	Zone func() *time.Location

	// Await is handed the position every write here landed at, so the next
	// read sees it: the read-your-writes seam. A write goes to the fleet's log
	// while a read is answered from some copy's rows, and a model that cannot
	// see its own write files it again. Applied after every write, never
	// before a read.
	//
	// The wait happens in the copy that answers next, never here (the router
	// may ask another copy): every surface hands the position, comparable only
	// within its stream and generation, to the node's session floor (the
	// estate router's), which later reads carry to whichever copy answers. Nil
	// skips it. A failure is logged and ignored: the write landed, and
	// reporting a failed create for a task that exists makes a duplicate.
	Await func(ctx context.Context, at statelog.Position) error

	// entries reads the atlas as a call's caller, for `linked_entries`:
	// pushed down by [Register] and [OperatorTools] beside Authorize.
	entries atlasReader

	// ops names a call's operation where neither a turn nor a request's key
	// does ([operations]), set by [OperatorTools] alone; its zero value takes
	// no `op_id`.
	ops operations
}

// Actor is who a write is attributed to: the tool layer's own small type, so
// callers of these tools do not depend on the tracker's record format.
type Actor struct {
	// Handle is the record's AUTHOR and never empty: a history row must name
	// who wrote it. A seat's handle, or an operator token's own name;
	// [Actor.Kind] says which, so a renderer never shows a credential as a
	// colleague.
	Handle string
	Kind   worklet.AuthorKind

	// OperatorID is the credential the write was made under, recorded
	// BESIDE the author rather than instead of it: a person acting through
	// a token is attributed to the person, and the token is how an audit
	// answers "what did this credential do".
	OperatorID string

	// Apps are the apps an OPERATOR (a credential acting as no seat) used when
	// this node resolved it (iam.ActorApps), nil for every other author.
	// Carried on every record a later turn reads (worklet's Writer.carries),
	// so a turn it wakes or hands work to is worked no wider (ADR-0109,
	// ADR-0112). Set by [ActorOf] alone.
	Apps []iam.Grant

	// WrittenFor is whom an AGENT's writes are made for: whoever its turn
	// works for, from the call's bound ceiling ([actorFor], [writtenForTurn]),
	// carried like Apps so the turn a record wakes works for them too
	// (ADR-0109). The tracker refuses an agent's write stating nothing
	// ([types.WrittenFor.For]): a site building an agent's actor without its
	// turn says why ([types.ForNobody]); a person's or credential's carries
	// nobody.
	WrittenFor types.WrittenFor

	// TurnID is the RUN that produced this write: provenance, so an audit can
	// walk from an item back to the execution that wrote it.
	TurnID string

	// WorkKey is the unit of work behind that run, which derived operation and
	// comment ids are seeded from: it survives a re-run, where a redelivered
	// trigger gets a new TurnID. See [Actor.OperationSeed] and ADR-0017.
	WorkKey string

	// WorkSince is when that unit of work BEGAN ([turnctx.Turn.WorkSince]):
	// the instant every id derived from WorkKey carries, so a re-run must
	// reproduce it ([Actor.OperationSince]). Set by the turn, or for a
	// request's key by [Actor.Keyed] alone.
	WorkSince time.Time

	// RebasedTo is the instant a derived operation id carries in place of
	// its identity's own start, zero where it carries that start — see
	// [turnctx.Turn.RebasedTo], which is the only place it comes from.
	RebasedTo time.Time

	// Calls is the run's call log, which a call's key reads its repeat
	// count from — see [Actor.callKey] and [turnctx.CallLog]. Nil outside
	// a turn, where no key is derived from a run.
	Calls *turnctx.CallLog

	// Operation is this CALL's operation id on a surface with no turn to
	// derive one from (the operator's assistant over MCP): the `op_id` the
	// caller brought back, or one minted and answered
	// ([WorkDeps.bindOperation]). Every write the call makes derives its id
	// from it ([opIDFor], [commentID]), so a call brought back repeats the
	// same writes (package doc, "How a write is made once"). Empty in a turn,
	// and on a transport carrying its own key, which arrives as
	// [Actor.WorkKey].
	Operation string

	Chain []string

	// Thread is the chat surface and conversation the turn was woken on,
	// which every task it files states (see [worklet.Origin]). Nil outside
	// a turn and for a turn no chat message woke: a person at the
	// dashboard files from no thread.
	Thread *worklet.Origin

	// Written is the turn's record of the items its writes committed to
	// ([turnctx.Written]), which lets the engine charge a turn dispatch named
	// no item for to the one it wrote. Nil outside a turn.
	Written *turnctx.Written
}

// OperationSeed is what a derived, idempotent id is built from.
//
// The work key where there is one: the identity a redelivery reproduces, so a
// re-run's writes collapse into the first attempt's. Else the run: a turn with
// no ledgerable trigger (a sub-agent's, a resumed run) still has rounds, and a
// call repeated in one run must write once. A scheduled fire is a task
// assignment with its own key. Outside a turn, the request's `Idempotency-Key`
// where its transport carries one ([Actor.WorkKey], internal/api/opkey);
// empty otherwise ([Actor.Operation] names it instead), since an invented seed
// lies about what produced the write and a stable one would collapse every
// later write into the first.
func (a Actor) OperationSeed() string {
	if a.WorkKey != "" {
		return a.WorkKey
	}
	return a.TurnID
}

// Keyed is this actor carrying a request's operation key: the ONE place a
// transport's key goes onto an actor, dated by the key's own instant
// ([statelog.OpMintedAt]), which every derived id carries and the ledger
// vouches for a retry by.
//
// A key carrying no instant is refused ([statelog.CheckCallerOpID]) rather
// than dated at the epoch, where every derived id would read as older than any
// ledger loss and be answered `unknown` on every attempt. An empty key is no
// key: the actor comes back as it was ([Actor.Operation] names the
// assistant's).
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

// keyedRequest reports whether this caller is a REQUEST whose transport
// carries its own operation key ([Actor.WorkKey]) rather than a turn or the
// operator's assistant. Read off the identities the actor holds, not a flag
// every constructor must set alike: a turn's actor always names its run, so a
// work key with no run is a request's. It decides what the caller is told
// ([sameCall], [reusedOperation]) and that the key is used as sent rather than
// as a seed ([Actor.callKey]), as the knowledge base and chat use it.
func (a Actor) keyedRequest() bool { return a.WorkKey != "" && a.TurnID == "" }

// OperationSince is the instant an id derived from [Actor.OperationSeed]
// carries: when the identity it is seeded from BEGAN, chosen in the seed's
// order (the work's start, else the run's, which its time-ordered id carries).
//
// Never the instant of the call: the ledger cannot vouch for an operation
// minted before its watermark, and a re-run's call always follows the loss it
// is judged against, so a call-time id is decided twice ([statelog.OpMintedAt]).
// Zero where neither is known (only a hand-built turn), which reads as older
// than every loss and is answered `unknown` unless its row is held.
//
// A rebase ([Actor.RebasedTo]) outranks both: a start older than what the
// ledger may have swept answers every write `unknown` on every node.
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

// asBegun is this actor with the rebase put aside ([Actor.RebasedTo]), so a
// key derived from it ([Actor.callKey]) carries the work's start on every
// attempt. For a key naming a ROW ([commentID], [createdTaskID],
// [createdViewID]): a rebase concerns the operation ledger alone, and the rows
// remember what the ledger forgot.
func (a Actor) asBegun() Actor {
	a.RebasedTo = time.Time{}
	return a
}

// settle hands a write's position to [WorkDeps.Await], best effort (see
// there). The applier's own budget bounds any wait, so a wedged copy costs a
// tool call seconds rather than the turn.
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
	// Mentions returns the handles this text addresses that are seats
	// here. Anything else is dropped: a comment naming an outsider must
	// not produce a notification nobody can deliver.
	Mentions(text string) []string
}

// ---- the seat identity every write is attributed to -------------------- //

// actorFor builds the write's attribution from the TURN, never from arguments:
// a model that could name its own actor could file work as anybody. The turn
// id and chain are provenance only and bound nothing (a hand-off is charged to
// the item's reassignment counter, not the delegation depth). Whom the turn
// works for comes from the call's bound ceiling ([writtenForTurn], ADR-0109).
// r is the composer of a family asking outside its gate, nil where every call
// passes the gate.
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
// [workingForOf]) and nothing a model writes; nil for a turn working for its
// own seat. The gate binds the ceiling for every call ([bindCeiling]), so r is
// asked only by a family resolving its caller outside a gate
// ([CodeDeps.ResolveRunRepos]). A ceiling that cannot be composed is
// [ErrUndecidable], never a turn nobody woke: a write carrying nobody would
// wake whoever it reaches for this seat, every app.
func writtenForTurn(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (types.WrittenFor,
	error) {

	ceiling, err := ceilingOf(ctx, turn, seat, r)
	if err != nil {
		return types.WrittenFor{}, fmt.Errorf("%w: %w", ErrUndecidable, err)
	}
	return types.ForTurn(workingForOf(ceiling)), nil
}

// unattributed is what a call answers when nobody could be named to write or
// read as ([WorkDeps.actor], [PageDeps.actor]): a call whose turn's requester
// could not be composed is unavailable for now, since what a seat writes
// carries that requester to whoever it wakes; any other is a caller with no
// seat and no principal behind it ([notInATurn]).
func unattributed(name string, err error) tools.Result {
	if errors.Is(err, ErrUndecidable) {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not establish who this "+
			"turn's work is for, which everything this seat writes carries to whoever it wakes. "+
			"Nothing was done. Try again shortly.", name))
	}
	return notInATurn(name)
}

// threadOf is where a turn was woken, as a create states it: the chat surface
// and the conversation on it, or nil for a turn no chat message woke.
func threadOf(turn *turnctx.Turn) *worklet.Origin {
	if turn == nil || strings.TrimSpace(turn.Transport) == "" {
		return nil
	}
	return &worklet.Origin{Surface: turn.Transport, Conversation: turn.ConversationKey}
}

// Provenance is the trail this actor's writes carry, and where they report the
// items they commit to: one construction for every writer a surface derives,
// so no shape drops the turn's write log.
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
// [unattributed] does. The call log is always the turn's: it is not
// attribution, and a resolver that dropped it would hand a repeated call its
// first copy's operation id.
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
// outside a turn, where a caller names its operation instead
// ([Actor.Operation], [Actor.WorkKey]). Through [Actor.OperationSeed], so the
// rule is stated once.
func turnKey(turn *turnctx.Turn) string {
	return turnIdentity(turn).OperationSeed()
}

// turnIdentity is the part of a turn a derived id is built from (its seed,
// instant and call log) as an [Actor], so the seed, its instant and a call's
// key ([Actor.callKey]) follow the actor's one rule.
func turnIdentity(turn *turnctx.Turn) Actor {
	if turn == nil {
		return Actor{}
	}
	return Actor{TurnID: turn.RunID, WorkKey: turn.WorkKey, WorkSince: turn.WorkSince,
		RebasedTo: turn.RebasedTo, Calls: turn.CallLog()}
}

// runIdentity is the turn's RUN, never its unit of work, as the identity a
// derived id is seeded from, for an operation that is one per run whatever
// work it serves ([holdOpID]). An [Actor] so its instant follows
// [Actor.OperationSince] (the rebase, else the run's own start).
func runIdentity(turn *turnctx.Turn) Actor {
	if turn == nil {
		return Actor{}
	}
	return Actor{TurnID: turn.RunID, RebasedTo: turn.RebasedTo}
}

// notInATurn is the refusal every one of these tools gives outside a turn.
//
// [tools.RefusalForbidden]: nothing about the arguments is wrong — this caller,
// with no seat and no bound person behind it, may not write as anybody.
func notInATurn(name string) tools.Result {
	return refused(tools.RefusalForbidden,
		name+" can only be called during a turn, on behalf of a seat.")
}

// unconfigured is the refusal when the company does not run Worklet.
//
// [tools.RefusalUnavailable]: the call is well-formed and the object may well
// exist in the tracker the company DOES run, so neither "invalid" nor "not
// found" is true of it.
func unconfigured(name string) tools.Result {
	return refusedBy(tools.RefusalUnavailable, waitless(), name+" is unavailable: this "+
		"company does not run Worklet. Use the tracker tools your company has "+
		"configured.")
}

// queryAlias is one argument this surface spells differently from the query
// grammar it compiles into.
//
// Written down, not only performed, because a saved view stores the GRAMMAR's
// keys ([worklet.ParseQuery]) while these arguments are the model's
// vocabulary, and a model saving the query it just ran must be told which to
// write. The translation stays in [listWorkItems.CallForTurn]; this table
// builds the sentence, and a test holds both against what the tool sends.
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
		"description, comments and links."
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
	// The identity check is [WorkDeps.actor]'s, never turn.RequireSeat(): the
	// OPERATOR surface has no turn and supplies its own actor, while a seat
	// with no turn is still refused by the nil-Actor fallback.
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(ListWorkItemsTool, err), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(ListWorkItemsTool), nil
	}

	// The arguments translate once into the query grammar's own keys: one
	// grammar serves the board, the socket, the REST route and this tool, and
	// a second parser would be where a filter meant something different.
	params := map[string]any{}
	if v := strings.TrimSpace(argString(args, "project")); v != "" {
		params["container"] = "project:" + strings.ToUpper(v)
	}
	// Keys whose argument and grammar key are the same word forward as-is;
	// those that differ follow. The set is only what a seat cannot ask another
	// way: board furniture (group, subgroup, group_limit, totals) is left
	// out, since a model reads rows and grouping costs it a shape to unpack.
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
	// The parent is a reference and is resolved: `parent` matches
	// `parent_id` raw, so a key passed through answers an empty list and no
	// error.
	if ref := strings.TrimSpace(argString(args, "parent")); ref != "" {
		id, refused := t.deps.resolveRef(ctx, ListWorkItemsTool, "`parent`", ref)
		if refused != nil {
			return *refused, nil
		}
		params["parent"] = id
	}
	// Custom-field filters (`f.<slug>`), the grammar's company-invented keys.
	// An object rather than `slug=value` strings, so a value containing `=`
	// stays writable.
	if raw, held := args["field_filters"].(map[string]any); held {
		for slug, value := range raw {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			params[worklet.FieldKeyPrefix+slug] = value
		}
	}
	// The grammar's text key is `q`; the argument is `text`, the word a model
	// reaches for. Copied through under its own name, it would not filter.
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
		// The two open groups, from the constants: `in_progress` and
		// `blocked` are not status groups, and the parser refuses them.
		params["status_group"] = strings.Join(openGroups(), ",")
	}

	// Through the expansion with the SEAT and its own project as viewer, from
	// the immutable turn context, so `preset=my_queue` is what this seat holds
	// and what is unclaimed in ITS project; a model naming its viewer could
	// read as anybody.
	q, err := t.deps.Reader.ExpandedQuery(ctx, params, worklet.Viewer{
		// The actor's handle: a person bound to a seat writes AS that seat
		// ([PrincipalActor]), so my_queue is theirs, not the credential's.
		Handle: actor.Handle, Project: t.deps.defaultProject(actor),
	},
		t.deps.now(), t.deps.zone())
	if err != nil {
		return failed(fmt.Sprintf("That filter is not one Worklet accepts: %s.",
			worklet.Sentence(err))), nil
	}
	// This surface's own level OVERRULES rather than fills in: a level can
	// arrive from a grammar path that knew nothing of this surface, and a
	// guard that only filled an empty field would enforce nothing. See
	// [statelog.LevelFor].
	q.Level = statelog.LevelFor(statelog.SurfaceSeat, q.Level)
	// The chart the unit filters resolve through is this surface's too: a
	// model types the team name, the rows hold the chart's key
	// ([worklet.Units]).
	q.Units = t.deps.Units
	// The rows are the rows that match. The grammar's default,
	// [worklet.SubtasksCollapsed], filters the ROOT and carries its subtree
	// unfiltered: right for a board's tree, wrong for a list a model counts,
	// where others' subtasks would read as its own queue. Overruled for the
	// level's reason, since a saved `view` carries a board's shape; a model
	// wanting a subtree asks with `parent`.
	q.Subtasks = worklet.SubtasksSeparate
	// And the row stays the plain row: `fields=` and `around` are a board
	// card's and a task page's opt-ins, which a saved `view` may carry and
	// which would grow every row in a model's context. Overruled likewise.
	q.RowFields, q.Around = nil, ""
	answer, err := t.deps.Reader.Tasks(ctx, q, t.deps.now())
	switch {
	case errors.Is(err, worklet.ErrTooBroad):
		// Not [readFailure]'s "try again": this refusal is about the QUERY
		// and repeats forever; its own sentence names what would narrow it.
		return failed(fmt.Sprintf("%s: %s.", ListWorkItemsTool, worklet.Sentence(err))), nil
	case errors.Is(err, worklet.ErrBadCursor):
		// NOT [readFailure]'s "change the arguments" alone: the one
		// argument to change is the cursor, and the remedy is to drop it.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused the "+
			"cursor: %s. Call it again without `cursor` to start from the first "+
			"page.", ListWorkItemsTool, ReadRefusalSentence(err))), nil
	case err != nil:
		return readFailure(ctx, ListWorkItemsTool, err), nil
	}
	// A grouped answer has no flat rows, so emptiness asks the groups too; a
	// closed axis carries every admitted column whether or not anything is
	// in it (internal/worklet's grouping doc), so empty means no column
	// COUNTS anything. A board wrongly reported empty invites a duplicate.
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
		// The count stops at a ceiling, so a `total` at it must not read as
		// an exact number a model quotes back.
		if answer.TotalCapped {
			result["total_is_at_least"] = true
		}
	}
	// The next page's cursor, without which `cursor` is unreachable.
	if answer.NextCursor != "" {
		result["next_cursor"] = answer.NextCursor
	}
	if !answer.Complete {
		// An incomplete answer says so, or a seat concludes the work does
		// not exist and files a duplicate.
		result["incomplete"] = incompleteNote(answer.Incomplete)
	}
	// EVERY TITLE IS SOMEBODY'S WORDS and no row says whose: neutralised,
	// with the answer labelled as the company's writers' (ADR-0040). A
	// group's label is a value somebody gave the rows it gathers.
	read := newForeignRead(turn, actor.Handle)
	rowNames(read, answer.Rows)
	if len(answer.Groups) > 0 {
		namesBy(read)
	}
	return read.answer(ctx, ListWorkItemsTool, result, "Narrow what you asked for.")
}

// incompleteNote is what a model is told when the answer could not account
// for everything: words rather than a flag, because the right response is a
// behaviour (say you could not check), and a boolean reads as metadata.
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
		"the two fence lines are a label, not part of the item."
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

// includeParts are the parts `include` names, as its schema closes them and
// as its reading takes them ([argval.Choice]): one list, both places.
var includeParts = []string{"body", "comments", "history", "links", "fields"}

// detailWants reads the `include` argument; with none, every part, so a model
// that never passes it still gets a whole item.
//
// The description is a part like the others so each stays reachable WHOLE: a
// description and a comment are each capped at 32 KiB, and a read always
// carrying the description could refuse the narrowed call its refusal
// advised. A named `comment` is its own default (the item and that comment
// only); an explicit `include` still wins.
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
		// THE ONE RULE for a closed set ([argval.Choice]), which the
		// check of the call reads `include` by too.
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
	// Who may read it is decided on the item: a tracker holder, or the person
	// an open ask on it names (workasked.go), known only once it is read; so
	// a caller the table does not admit has their asks probed BEFORE any part
	// is read for the answer. After the arguments, whose refusals reveal
	// nothing about the board.
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
	// The chart, so the answer carries the team's NAME beside its key: a
	// model writes prose about the item, and only the chart can say a team
	// has left it. Passed wherever a chart is held, because an unresolved
	// unit is a FINDING: a surface that could resolve and did not would
	// report every task orphaned.
	want.Units = t.deps.Units
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
	// The description is whole or absent, never cut: a model acts on a
	// truncated spec as the spec. Left out, the answer says so and its size.
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
	// THE DESCRIPTION AND THE THREAD ARE SOMEBODY'S WORDS, fenced and
	// labelled for the handle this call reads as.
	read := newForeignRead(turn, actor.Handle)
	if len(answer) == 1 {
		fenceTask(read, nil, detail)
		return read.answer(ctx, GetWorkItemTool, detail, narrow)
	}
	fenceTask(read, fieldPath{}.key("task"), detail)
	return read.answer(ctx, GetWorkItemTool, answer, narrow)
}

// fenceTask fences what a work item read holds that somebody wrote: the
// description under its author, and each comment under its own. The
// description's author is its last writer, so a body past its first version
// is labelled as having unnamed writers too, for [fencePage]'s reason.
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
			// THE ASK'S OPTIONS ARE ITS AUTHOR'S, written with it.
			read.attribute([]provenance.Author{author}, false)
		}
	}
	// THE HISTORY'S EXCERPTS are the changes' own text — a comment, a
	// hand-off's reason — each under who made it, as the activity feed
	// labels the same rows.
	for i, h := range d.History {
		fenceExcerpt(read, at.key("history").index(i).key("excerpt"),
			worklet.ChangeKind(h.Kind), h.Actor, h.ActorKind, h.Excerpt)
	}
	// AND ITS NAMES: the title, the titles and notes of what it links to,
	// its checklists, its custom fields' values and what its history's
	// changes moved, none of which a row records a writer of.
	if d.Task.Title != "" {
		namesBy(read)
	}
}

// ---- create_work_item -------------------------------------------------- //

type createWorkItem struct {
	deps WorkDeps

	// pages checks a decision's page evidence — see [evidenceLookup]. Nil
	// on a company with no Doclet.
	pages PageReader
}

var _ tools.SeatCallable = (*createWorkItem)(nil)

func (t *createWorkItem) Name() string { return CreateWorkItemTool }

func (t *createWorkItem) Description() string {
	return "File a new work item. Search with " + ListWorkItemsTool + " first — a " +
		"duplicate costs somebody a triage turn. Name an assignee and it " +
		"goes straight to their queue; leave it empty and it goes to the " +
		"project's default assignee, or lands in triage — where the " +
		"team's lead is told about it — when the project names none. The " +
		"answer's `assignee` says which. Name " +
		"`ask` to file the item AS a question to that person — the title " +
		"is the question — with `decision` when they have to choose."
}

func (t *createWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(scheduleInto(map[string]any{
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
				"type": "string",
				"description": "The project key. Defaults to the parent's " +
					"for a subtask, and to your team's otherwise.",
			},
			"assignee": map[string]any{
				"type": "string",
				"description": "A seat's handle, from " + LookupColleagueTool + ". Omit " +
					"and the project's default assignee takes it (" + worklet.DescribeProjectTool + " " +
					"names them), else it waits in triage for the lead to route.",
			},
			"priority": map[string]any{
				"type":        "string",
				"description": "One of " + words.Or(worklet.Priorities) + ". Default none.",
			},
			"status": map[string]any{
				"type": "string",
				"description": "Where it starts: one of " + words.Or(worklet.Statuses) +
					". Default todo — name another only when the work is " +
					"already under way or already done as it is filed.",
			},
			"parent": map[string]any{
				"type": "string",
				"description": "The item this belongs under — a subtask " +
					"lives in its parent's project. " + itemRef,
			},
			"fields": map[string]any{
				"type": "object",
				"description": "Custom fields, keyed by SLUG — read them with " +
					worklet.GetWorkCatalogueTool + " or " + worklet.DescribeProjectTool + ". A project may " +
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
				"description": "Items this one is blocked BY. " + itemRefs +
					" A plain list here, unlike " + UpdateWorkItemTool + ": a new item " +
					"has no dependencies to replace.",
				"items": map[string]any{"type": "string"},
			},
			"labels": map[string]any{
				"type": "array",
				"description": "Tags this project declares — read them with " +
					worklet.DescribeProjectTool + ". A label the project has not declared " +
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
			"ask": map[string]any{
				"type": "string",
				"description": "A colleague's handle: the item IS a question " +
					"they owe an answer to, and its title is the question. " +
					"They are woken asking for one and follow the item, and " +
					"their answer wakes you. Filed in one record with the " +
					"item, so the question cannot be lost between two writes. " +
					"An agent always asks a person with a `decision`.",
			},
			"decision": decisionSchema(),
		},
		"required": []any{"title"},
	}, false))
}

// askFor is the question a create is filed as, or nil when it asks nobody.
// The title is the question and the ask's body (the item's body is context it
// already carries); an ask with no text renders as nothing in the thread and
// in `asked_of_me`.
func (t *createWorkItem) askFor(ctx context.Context, actor Actor, task worklet.Task,
	args map[string]any) (*worklet.Comment, *tools.Result) {

	asked := strings.TrimSpace(argString(args, "ask"))
	rawDecision, decided := args["decision"]
	switch {
	case asked == "" && decided:
		return nil, refusalOf(failed(worklet.CreateWorkItemTool + ": `decision` needs `ask` " +
			"— the handle of the person who has to choose. A decision asked " +
			"of nobody wakes nobody."))
	case asked == "":
		return nil, nil
	}
	resolved, refusal := t.deps.resolveHandle(ctx, CreateWorkItemTool, "`ask`", asked)
	if refusal != "" {
		return nil, refusalOf(failed(refusal))
	}
	if refused := t.deps.checkAskOfPerson(CreateWorkItemTool, actor, resolved, decided); refused != nil {
		return nil, refused
	}
	ask := &worklet.Comment{
		// Derived like every comment's, so a re-run or a call brought back
		// files one question.
		ID:         commentID(actor, CreateWorkItemTool, task.ID, args),
		Task:       task.ID,
		Author:     actor.Handle,
		AuthorKind: actor.Kind,
		Body:       task.Title,
		Ask:        resolved,
		CreatedAt:  task.CreatedAt,
	}
	if decided {
		decision, refusal := t.deps.readDecision(ctx, CreateWorkItemTool,
			actor, rawDecision, t.pages)
		if refusal != nil {
			return nil, refusal
		}
		ask.Decision = decision
	}
	return ask, nil
}

// scheduleInto merges the scheduling parameters into a tool's own schema, so
// create and update accept exactly the same spellings.
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
		return unattributed(CreateWorkItemTool, err), nil
	}
	if t.deps.Writer == nil {
		return unconfigured(CreateWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, CreateWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	writer := t.deps.Writer(actor)

	now := t.deps.now()
	// Without the fence a description copied from a read still wears; see
	// [unfence].
	body := unfence(turn, argString(args, "body"))
	task := worklet.Task{
		V:           worklet.DocumentVersion,
		Title:       strings.TrimSpace(argString(args, "title")),
		Body:        body,
		Type:        strings.TrimSpace(argString(args, "type")),
		Project:     strings.ToUpper(strings.TrimSpace(argString(args, "project"))),
		Assignee:    strings.TrimSpace(argString(args, "assignee")),
		Reporter:    actor.Handle,
		Priority:    worklet.Priority(strings.TrimSpace(argString(args, "priority"))),
		Status:      worklet.StatusTodo,
		StatusGroup: worklet.GroupNotStarted,
		Rank:        worklet.RankOrigin,
		Tags:        argStrings(args, "labels"),
		CreatedAt:   now,
		UpdatedAt:   now,
		BodyAuthor:  actor.Handle,
		BodyAt:      now,
	}
	if task.Title == "" {
		return toPerson(failed(worklet.CreateWorkItemTool+" needs a `title`: one line "+
			"saying what the work is."), caveat.TitleMissing()), nil
	}
	if raw, held := args["fields"]; held {
		fields, refusal := fieldMap(raw)
		if refusal != "" {
			return failed(refusal), nil
		}
		task.Fields = fields
	}
	// When it is due and how big it is (workschedule.go).
	plan, refusal := readSchedule(args, now, t.deps.zone(), CreateWorkItemTool)
	if refusal != "" {
		return failed(refusal), nil
	}
	plan.applyToTask(&task)
	if task.Type == "" {
		task.Type = worklet.DefaultType
	}
	if task.Priority == "" {
		task.Priority = worklet.PriorityNone
	}
	if !task.Priority.Valid() {
		return failed(fmt.Sprintf("%q is not a priority. The priorities are: %s.",
			clip(string(task.Priority)), words.And(worklet.Priorities))), nil
	}
	// Where it starts, when the caller says: filing from a board lane is one
	// write, not a create and a move that could fail in between. The group
	// is the status's own, and the applier stamps the finish instants from
	// it, so a task filed `done` is done from its first row.
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := worklet.Status(raw)
		if !status.Valid() {
			return failed(fmt.Sprintf("%q is not a status. The statuses are: %s.",
				clip(raw), words.And(worklet.Statuses))), nil
		}
		task.Status, task.StatusGroup = status, status.Group()
	}
	if ref := strings.TrimSpace(argString(args, "parent")); ref != "" {
		parent, refused := t.deps.resolveTask(ctx, CreateWorkItemTool, "`parent`", ref)
		if refused != nil {
			return *refused, nil
		}
		task.Parent = &parent.ID
		// A subtask files into its parent's project when the call names
		// none; the caller's own team's would be refused by the tracker.
		if task.Project == "" {
			task.Project = parent.Project
		}
	}
	if task.Project == "" {
		task.Project = t.deps.defaultProject(actor)
		if task.Project == "" {
			return failed(org.NoHomeProject(worklet.CreateWorkItemTool)), nil
		}
	}
	// The operation, and the task's id from it ([createdTaskID]). Its object
	// is the project the CALL names, never one a default resolved to: the
	// chart and the parent a default reads can move between attempts, and a
	// second attempt would derive another operation and file a second item.
	// The call's own arguments tell new items apart, as a view's do
	// ([viewObjectKey]).
	opID := opIDFor(actor, t.Name(), "create",
		createObjectKey(argString(args, "project")), args)
	task.ID = createdTaskID(actor, t.Name(), argString(args, "project"), args)
	// A unit the caller named, checked against the chart and open to every
	// seat, unlike update_work_item's re-route (the project lead's).
	// `FiledUnit` is the immutable record of the owning team, `RoutingUnit`
	// the mutable half. Naming none is ordinary: the tracker fills both from
	// the project's chart-owned unit inside the create's snapshot, alike for
	// every writer. The chart's key is stored, not the typed string:
	// `filed_unit` is never rewritten, so verbatim spellings would split one
	// team's work across filters.
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

	// The assignee resolves before the watchers, so the canonical handle is
	// what gets watched.
	assignee, refusal := t.deps.resolveHandle(ctx, CreateWorkItemTool,
		"`assignee`", task.Assignee)
	if refusal != "" {
		return failed(refusal), nil
	}
	task.Assignee = assignee
	// The reporter and the assignee watch it, set at the write: a list built
	// at the wake would read a row that has moved on. The author is the
	// address, since a person bound to a seat writes AS it ([PrincipalActor]).
	task.Watchers = handles(actor.Handle, task.Assignee)

	// The question, checked before anything is published and after the
	// task's id is derived, which the ask's id is derived from.
	ask, askRefusal := t.askFor(ctx, actor, task, args)
	if askRefusal != nil {
		return *askRefusal, nil
	}
	// Blockers resolve before any write, so an unknown dependency refuses
	// the whole call rather than leaving an item or declared labels behind.
	var blockers []string
	for _, ref := range argStrings(args, "waiting_on") {
		id, refused := t.deps.resolveBlocker(ctx, CreateWorkItemTool, "waiting_on", ref)
		if refused != nil {
			return *refused, nil
		}
		blockers = append(blockers, id)
	}
	if len(blockers) > 0 && t.deps.Dependencies == nil {
		return unconfigured(CreateWorkItemTool), nil
	}
	// Labels before the task, which the create refuses undeclared, and after
	// every refusal above: the declare is the call's first PUBLISH, and a
	// later refusal would leave labels nobody used.
	declared, caveats, labelRefusal := t.deps.declareLabels(ctx, actor, labelWrite{
		tool:    t.Name(),
		before:  "filing the work item",
		notMade: "The work item was NOT filed by this call.",
		then:    "files it",
		twice:   "files a second item",
		unvouched: "it answers with the item where an earlier attempt " +
			"filed it",
		look: "the item may exist on a node this one has not caught up " +
			"with: look for it with " + ListWorkItemsTool + " rather than filing it " +
			"any other way",
	}, args, task.Project, task.Tags)
	if labelRefusal != nil {
		return *labelRefusal, nil
	}
	notify := worklet.Wake{
		Kind: worklet.ChangeCreated, After: task, Comment: ask,
	}.Notify(t.deps.Leads)
	var got worklet.WriteResult
	if ask != nil {
		got, err = writer.CreateTaskAsking(ctx, opID, task, *ask, notify)
	} else {
		got, err = writer.CreateTask(ctx, opID, task, notify)
	}
	if err != nil {
		return writeFailure(ctx, actor, CreateWorkItemTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// An item nobody can say was filed is not reported: its key may name
		// a task that never landed.
		return createUnknown(actor, opID, task.ID, got), nil
	}
	t.deps.settle(ctx, got.Position)
	answer := withOperation(receiptItem(map[string]any{
		"id": task.ID, "status": task.Status,
		// The tracker's assignee, not the argument: a create naming nobody
		// goes to the default assignee its own snapshot decides.
		"assignee": got.Assignee, "outcome": string(got.Outcome), "position": positionOf(got.Position),
		"labels_created": declared, "version": got.Version,
	}, task.ID, got.Key, got.KeyCollision), actor)
	if ask != nil {
		// The ask's own id: what an answer names in `answers`.
		answer["asked"], answer["comment_id"] = ask.Ask, ask.ID
		if ask.Decision != nil {
			answer["decision"] = ask.Decision
		}
	}
	caveats = append(caveats, got.Caveats...)
	// Dependencies after it: the mirror commit names this task, which must
	// exist first.
	if len(blockers) > 0 {
		result, err := t.deps.Dependencies(actor).Depend(ctx,
			opIDFor(actor, t.Name(), "depend", task.ID, args), worklet.DependencyChange{
				Task: task.ID, Project: task.Project, WaitingOnAdd: blockers,
			}, t.deps.Leads)
		if err != nil {
			// The item exists and is reported: failing the call would
			// make the next attempt file a second one.
			return ownReceipt(ctx, turn, actor, t.Name(), answer, caveats,
				partial(answer, DependenciesFailedKey, caveat.DependenciesNotRecorded(
					worklet.ItemAddress(task.ID, got.Key, got.KeyCollision),
					dependencyFailure(task.ID, got.Key, got.KeyCollision,
						writeTold(ctx, actor, t.Name(), err)))))
		}
		t.deps.settle(ctx, result.Position)
		// The call answers for both records: the dependency lands after the
		// create, so the create's position is a floor below the edge, and an
		// `unknown` dependency must not read as landed. The version moves
		// too, since the edge is a commit on this item.
		outcome, at, version := joinDepend(got.Outcome, got.Position,
			got.Version, result)
		answer["outcome"], answer["position"] = string(outcome), positionOf(at)
		answer["version"] = version
		caveats = append(caveats, result.Caveats...)
		if len(result.OneSided) > 0 {
			answer["dependencies_pending_mirror"] = len(result.OneSided)
		}
	}
	return ownReceipt(ctx, turn, actor, t.Name(), answer, caveats)
}

// joinDepend folds a dependency sequence that ran AFTER a call's first record
// into that call's one answer: the less certain outcome and the later position
// ([statelog.LessCertain], [statelog.Later]; save_page's rule too), and the
// item's later version. A caller barriers on the position and reads the
// outcome as "may I stop looking", so the first record's alone would be a
// floor below the edge and an unknown dependency reported as applied.
//
// The version is THIS item's ([worklet.DependencyResult]'s, zero where it
// landed no commit here), never the sequence's last commit's, which may be
// another item's; where both wrote here the dependency's is the newer, else
// the caller gets an `if_match` its own second write moved past.
func joinDepend(outcome statelog.Outcome, at statelog.Position, version int64,
	result worklet.DependencyResult) (statelog.Outcome, statelog.Position, int64) {

	if !result.Wrote() {
		return outcome, at, version
	}
	return statelog.LessCertain(outcome, result.Outcome),
		statelog.Later(at, result.Position), max(version, result.Version)
}

// createUnknown explains a create whose outcome is unknown: the item may have
// been filed and it may not, so it is reported as neither: a failed result,
// for [unknownWrite]'s reason (a key beside "unknown" reads as the item's).
// What the caller does next is [unknownNext]'s.
func createUnknown(actor Actor, opID, id string, got worklet.WriteResult) tools.Result {
	filed := ""
	if got.Key != "" {
		filed = fmt.Sprintf(" If this attempt filed it, it is %s.",
			worklet.ItemNamed(id, got.Key, got.KeyCollision))
	}
	return unknownWrite(actor, CreateWorkItemTool, "the work item was filed",
		opID, got.Unvouched, filed+" "+unknownNext(got.Unvouched,
			sameCall(actor, CreateWorkItemTool),
			"Look for it with "+ListWorkItemsTool, "it files a second item"))
}

// unknownWrite explains a tracker write whose outcome is unknown, as the
// failed result every write tool answers one with: whether `what` happened is
// unknown, why, under which operation, and next. Never a receipt carrying
// `outcome: unknown`: a `comment_id` or `version` beside it reads as the change
// made, when an unvouched operation is answered `unknown` without publishing.
//
// It names the caller's `op_id` where there is one ([sameCall]), since a
// step's id handed back would be a new call's operation; otherwise `opID`,
// never a step. An unvouched one says so, since the same call answers alike
// until the write reaches this node. Its cause is [UnknownOutcome], for a
// caller answering in status codes ([ErrOutcomeUnknown]).
func unknownWrite(actor Actor, tool, what, opID string, unvouched bool,
	next string) tools.Result {

	why := "the write's acknowledgement was lost"
	if unvouched {
		why = "this node's operation ledger may have lost the record of this " +
			"operation, so this node cannot tell"
	}
	if actor.Operation != "" {
		opID = actor.Operation
	}
	return failedUnknown(opID, unvouched, fmt.Sprintf("%s: whether %s is "+
		"unknown (%s; operation %s). It may have landed and it may not — do "+
		"not report it as done, and do not report it as failed. %s", tool,
		what, why, opID, strings.TrimSpace(next)))
}

// unknownNext says what a caller told `unknown` does next, from whether this
// node's ledger vouches for the operation and whether the caller has a repeat
// that IS the same operation (`again`, from [sameCall], or ""). A lost
// acknowledgement's same-operation repeat finishes it: the ledger answers what
// landed. An unvouched one's repeat is safe but answers alike until the write
// reaches this node, so looking says sooner. With no such repeat the caller
// looks first: `twice` is what a new operation does if the first landed. A
// reworded call is always the duplicate. `look` is an imperative clause naming
// the read that shows it ("Read ENG-4 with get_work_item").
func unknownNext(unvouched bool, again, look, twice string) string {
	switch {
	case unvouched && again != "":
		return look + " before doing anything else about it. To repeat it, " +
			again + ": that is safe — it writes nothing this node cannot vouch " +
			"for — but answers the same way until the write reaches this node. " +
			"Never make it again under different arguments: if the first call " +
			"landed, " + twice + "."
	case again != "":
		return capitalize(again) + ": the retry is the same operation, answers " +
			"with what landed, and lands once if it did not. Do not reword it: " +
			"a call with different arguments is a new operation, and if the " +
			"first one landed, " + twice + "."
	case unvouched:
		return look + " before making it again: this node cannot tell, and if " +
			"the first call landed, " + twice + "."
	}
	return look + " before making it again: a second call is a new operation, " +
		"and if the first one landed, " + twice + "."
}

// restateNext is [unknownNext] for a write that STATES a value (a catalogue
// list, a project's tags and policy): any repeat is harmless, since it states
// the same thing again, so it is offered, and never promised as "the same
// operation", which an operator's assistant's repeat is not.
func restateNext(look string) string {
	return look + " to see whether it did. Making the same call again is " +
		"harmless: it states the same thing again, so it changes nothing if " +
		"the first one landed."
}

// dependencyFailure explains a create whose ITEM was filed and whose
// dependencies did not all land. Not [writeFailure], whose "NOT made" is false
// of the item and whose "call again" files a second one: the dependencies are
// finished on the item with update_work_item, which records whatever of
// `waiting_on` is still missing under its own operation. The call names the
// item's address ([worklet.ItemAddress]), since one filed on a key another
// task holds is reached by its id. Why it stopped is told as [writeTold] tells
// it.
func dependencyFailure(id, key string, collision bool, why string) string {
	filed := key + " (" + id + ")"
	if collision {
		filed = worklet.ItemNamed(id, key, collision)
	}
	return fmt.Sprintf("The item WAS filed, as %s, but recording what it "+
		"waits on did not finish: %s. Do NOT call "+worklet.CreateWorkItemTool+" again — that "+
		"files a second item. Set its dependencies with "+UpdateWorkItemTool+" on %s, "+
		"giving the same waiting_on: it records whichever of them are still "+
		"missing, and says so if one cannot be.", filed, why,
		worklet.ItemAddress(id, key, collision))
}

// updateDependencyFailure explains an update whose PATCH landed and whose
// dependency change did not all land, for [dependencyFailure]'s reason. It
// prescribes a call carrying only the dependencies, safe on every surface;
// the whole call reworded is a new operation setting every field again, and
// refused as stale if it names `if_match`.
func updateDependencyFailure(item worklet.TaskDetail, why string) string {
	return fmt.Sprintf("The change to %s WAS made, but its dependency change "+
		"did not finish: %s. Do not report the dependencies as set. Set them "+
		"with "+UpdateWorkItemTool+" on %s giving only the same waiting_on and "+
		"blocking: it records whichever of them are still missing, and says so "+
		"if one cannot be.", item.Named(), why, item.Address())
}

// resolveRef turns what a model typed (a key like ENG-7, or an id) into the
// task ID every relation and parent pointer is written with: both are IDs
// ([worklet.Relation.Other], [worklet.Task.Parent]), and a stored KEY is an
// edge resolving to nothing on every node forever, its mirror never written.
//
// It answers a refusal as the tool's RESULT, of two classes: an argument
// naming no item is [tools.RefusalInvalid], and a read this node could not
// serve is [tools.RefusalUnavailable] with its error beneath ([readFailure]),
// so a caller answering in status codes knows to come back.
func (d WorkDeps) resolveRef(ctx context.Context, tool, field,
	ref string) (string, *tools.Result) {

	task, refused := d.resolveTask(ctx, tool, field, ref)
	return task.ID, refused
}

// resolveTask is [WorkDeps.resolveRef] answering the whole task, for a caller
// that needs more of it than the id — a subtask's project is its parent's.
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

// refusalOf is a refusal as the optional second answer of a helper that
// resolves something for a tool: nil when there is nothing to refuse.
func refusalOf(r tools.Result) *tools.Result { return &r }

// resolveHandle turns a handle a caller typed into one the company has.
//
// Validated because an unknown handle fails silently and permanently:
// [worklet.Route] drops it against the live roster with no error or metric,
// and the write answers `applied`. Here rather than in the tracker, which
// holds no chart: an applier reading one would write different rows on nodes
// briefly on different epochs. It resolves through `lookup_colleague`'s
// machinery, so a role's name yields the canonical handle. A nil roster
// admits everything: refusing every handle with no chart loaded would be
// worse than the hole this closes. A refusal lists seats only to a caller who may read the roster
// ([WorkDeps.mayReadRoster]), asked of the table rather than assumed.
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
// write is made once"). A caller's own operation ([Actor.Operation]) makes each
// write a STEP of it, so the call brought back derives the same ids; its
// arguments are bound to it already ([WorkDeps.bindOperation]). A turn's or
// request's key derives the id from the call's key ([Actor.callKey]), the verb,
// the object and the arguments' digest (a request's key names the request, not
// what it asked), at [Actor.OperationSince], under [opIDNamespace] (package doc,
// "What the tracker's tools derive from a call's key"). With neither it is fresh
// per call: a stable id has the ledger collapse every later write as a
// redelivery. The object is one escaped, bounded name segment
// ([statelog.OpNamePart]), since a dot begins a STEP; tool, the calling tool's
// own name, keeps the call's repeat count.
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

// opIDNamespace scopes [opIDFor]'s derivation from a call's key, and is FIXED
// for the life of the format (package doc, "What the tracker's tools derive
// from a call's key").
const opIDNamespace = "crewlet.builtin.work"

// createObjectKey is the object a create's operation is named for: the project
// the call names, as it is stored, or the fixed word `new` for a call that
// names none — see the create's own comment for why never its default.
func createObjectKey(project string) string {
	if key := strings.ToUpper(strings.TrimSpace(project)); key != "" {
		return key
	}
	return "new"
}

// createdTaskID is the id of the task a `create_work_item` call files: a UUIDv5
// over the create's operation id as the work began.
//
// A function of the operation, since a task's id is the subject its create
// arbitrates on: a re-run addresses the task the first run filed and the
// tracker answers with it, where a fresh id per call filed two. As the work
// began ([Actor.asBegun]), for [commentID]'s reason: a rebased attempt
// ([Actor.RebasedTo], ADR-0029) re-mints its operations, but the task's row
// has not forgotten the item ([worklet.Writer.CreateTask]). Where the
// operation is fresh, so is the id: one operation, one task.
func createdTaskID(actor Actor, tool, project string, args map[string]any) string {
	opID := opIDFor(actor.asBegun(), tool, "create", createObjectKey(project), args)
	return uuid.NewSHA1(createdTaskNamespace, []byte(opID)).String()
}

// createdTaskNamespace is the uuid namespace a created task's id is derived
// under. Fixed for the life of the format: it is durable in every task row.
var createdTaskNamespace = uuid.MustParse("8677bb1c-20e0-4fbe-ab44-846868b79b37")

// callOutcome is one tool call's answer over EVERY record it appended: the
// less certain outcome and the later position ([joinDepend]'s rule). For a
// tool whose records are reported facet by facet (a project's tags and
// policy, the catalogue's types and fields): every transport reads the call's
// outcome and position from the top of the answer, and barriers on it.
type callOutcome struct {
	outcome statelog.Outcome
	at      statelog.Position
}

// add folds one record's result in. A result that appended nothing is left
// out ([statelog.Result.Wrote]): it claims no record a caller must wait for.
func (c *callOutcome) add(r statelog.Result) {
	if !r.Wrote() {
		return
	}
	c.outcome = statelog.LessCertain(c.outcome, r.Outcome)
	c.at = statelog.Later(c.at, r.Position)
}

// stamp writes the call's answer onto the top of a tool's result. A call that
// appended nothing is `applied` at no position — nothing is outstanding.
func (c callOutcome) stamp(answer map[string]any) {
	outcome := c.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	answer["outcome"], answer["position"] = string(outcome), positionOf(c.at)
}

// partlyWritten is the refusal of a call whose LATER record was refused after
// an earlier one landed: the later write's cause, and a sentence saying what
// landed and where, as save_page does for a refused rename. The unmarked "the
// change was NOT made" would have a caller redo, or report undone, a change
// that is on every node.
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

// ---- update_work_item -------------------------------------------------- //

type updateWorkItem struct {
	itemTarget
	deps WorkDeps
}

var _ tools.SeatCallable = (*updateWorkItem)(nil)

func (t *updateWorkItem) Name() string { return UpdateWorkItemTool }

func (t *updateWorkItem) Description() string {
	return "Change a work item: its status, assignee, priority, title, " +
		"description, labels, checklists, or whether you watch it. Only the " +
		"fields you pass are changed. An assignment can carry a `reason` the " +
		"new assignee reads. Moving an item you keep into `in_review` names its " +
		"`reviewer`. Say WHY you closed something with " +
		worklet.CommentOnWorkTool + " — an item that went to `cancelled` with no " +
		"word is one somebody has to reconstruct. Closing as a duplicate " +
		"also names the item that survives, in `duplicate_of`."
}

func (t *updateWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(scheduleInto(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to change. " + itemRef,
			},
			"status":   map[string]any{"type": "string", "description": "One of " + words.Or(worklet.Statuses) + "."},
			"assignee": map[string]any{"type": "string", "description": "A seat's handle, or \"\" to unassign."},
			"reviewer": reviewerParam,
			"reason": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Only with `assignee` or `reviewer`: one line "+
					"saying why the work is theirs now, at most %d "+
					"characters. It is what the new assignee is woken with "+
					"and what the item's history shows beside the hand-off.",
					MaxAssignmentReason),
			},
			"checklist": checklistSchema(),
			"priority":  map[string]any{"type": "string", "description": "One of " + words.Or(worklet.Priorities) + "."},
			"title":     map[string]any{"type": "string"},
			"body":      map[string]any{"type": "string", "description": "Replaces the description."},
			"labels": map[string]any{
				"type": "array",
				"description": "REPLACES the item's labels. Each must be a " +
					"tag this project declares — " + worklet.DescribeProjectTool + " lists them.",
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
					"and the status records that it is closed. " + itemRef +
					" null or \"\" takes this item's duplicate link off.",
			},
			"watch": map[string]any{
				"type": "boolean",
				"description": "True to follow this item, false to stop. " +
					"Stopping sticks: you will not be re-subscribed by being " +
					"assigned it, though a direct @-mention still reaches you.",
			},
			"if_match": map[string]any{
				"type": "integer",
				"description": "The `task.version` from " + GetWorkItemTool + ". Given, the " +
					"edit is REFUSED if anybody changed the item since you " +
					"read it. Omitted, your fields are merged onto the " +
					"current item — which is usually what you want.",
			},
			"routing_unit": map[string]any{
				"type": "string",
				"description": "Point this item at a different team, by its " +
					"id or its name: that team's lead hears that work routes " +
					"to them now. The lead's of the project the item is in " +
					"to set — filing your own work into your own team is " +
					"what `unit` on " + worklet.CreateWorkItemTool + " does.",
			},
			"waiting_on": setArgSchema("The items this one is blocked BY. " + itemRefs +
				" A pull request under the company's GitHub owners, by its URL, blocks it " +
				"until it merges or closes, when you are told to update the item. When it " +
				"covers a question you asked a person on this item, withdraw that question " +
				"in the same step with " + WithdrawAskTool + " and `replaced_by`."),
			"blocking":     setArgSchema("The items blocked BY this one. " + itemRefs),
			"linked":       setArgSchema("Related items, with no blocking meaning. " + itemRefs),
			"linked_pages": setArgSchema("Knowledge-base pages this item references, by page id."),
			"linked_entries": setArgSchema("Entries of the company's atlas this item touches (the " +
				"servers, services, databases, domains or accounts the work changes), each by its id " +
				"as " + AtlasSearchTool + " answers it, or by its exact name or key."),
			"fields": map[string]any{
				"type": "object",
				"description": "Custom fields, keyed by SLUG — read them with " +
					worklet.GetWorkCatalogueTool + " or " + worklet.DescribeProjectTool + ". A value is " +
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
	}, true))
}

// setArgSchema is the shape every set-valued argument takes, spelled out in
// the schema because a model's likeliest reading, a bare list, silently drops
// every entry it did not repeat.
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
		return unattributed(UpdateWorkItemTool, err), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(UpdateWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, UpdateWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	writer := t.deps.Writer(actor)

	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed(UpdateWorkItemTool + " needs an `item` — a key like ENG-42, or an id."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, UpdateWorkItemTool, err), nil
	}

	patch, kind, refusal := patchFromArgs(args, actor, t.deps.now(), t.deps.zone())
	if refusal != "" {
		return failed(refusal), nil
	}
	if refused := t.deps.checkWaitOnPerson(ctx, actor, before, patch); refused != nil {
		return *refused, nil
	}
	if patch.Body != nil {
		// The description get_work_item answered arrives fenced, and the
		// likeliest edit sends the fence back with it; see [unfence].
		body := unfence(turn, *patch.Body)
		patch.Body = &body
	}
	// One operation per distinct call, keyed on what was sent
	// ([Actor.callKey]). Derived before the patch is built: a checklist
	// gesture mints the ids of the lists and items it adds from it.
	opID := opIDFor(actor, t.Name(), "update", before.Task.ID, args)
	reason, refusal := assignmentReason(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	checklist, refusal := t.deps.checklistIntent(ctx, args, opID)
	if refusal != "" {
		return failed(refusal), nil
	}
	if checklist != nil {
		patch.Checklist = checklist
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeChecklist
		}
	}
	// if_match is the model's own precondition, passed through: omitted it
	// merges, given it refuses if anybody moved the task since the read.
	ifMatch := uint64(max(argInt(args, "if_match", 0), 0))
	if patch.Assignee != nil {
		// The create's check, here rather than in patchFromArgs, which is
		// free of the deps that hold the roster.
		//nolint:govet // shadow: `x, refusal := f()` declares x too; see .golangci.yml
		assignee, refusal := t.deps.resolveHandle(ctx, UpdateWorkItemTool,
			"`assignee`", *patch.Assignee)
		if refusal != "" {
			return failed(refusal), nil
		}
		patch.Assignee = &assignee
	}
	if refused := t.deps.holdReview(ctx, actor, before, args, &patch, &kind); refused != nil {
		return *refused, nil
	}
	// The re-route is its own kind and gate: `routed` carries one delta, and
	// the new unit's lead hears it as an ORDINARY candidate rather than a
	// fallback, so it reaches them on a task that still has an assignee.
	if raw, held := args["routing_unit"]; held {
		unit := strings.TrimSpace(argString(map[string]any{"v": raw}, "v"))
		// Asked here, not at the gate: the task's project comes from the
		// row just read. [Authorizer]'s error kinds tell a chart that could
		// not answer from one that said no.
		if refusal := t.deps.mayWrite(ctx, authz.ActionWorkRoute,
			authz.Object{
				Kind: authz.KindProject, Container: before.Task.Project,
			}); refusal != nil {

			return *refusal, nil
		}
		// The chart's own key lands, as for the filed unit: the wake
		// resolves it back to a lead, and a rename strands other spellings.
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
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeRouted
		}
	}
	// Inert edges ride the patch; dependencies do not: `waiting_on` is an
	// authored edge here and a mirrored entry on another item, a sequence
	// rather than a field.
	inert, relationRefusal := t.deps.inertRelations(ctx, turn, UpdateWorkItemTool, args,
		before.Task)
	if relationRefusal != nil {
		return *relationRefusal, nil
	}
	if inert != nil {
		patch.Relate = inert
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeRelations
		}
	}
	change, dependencyRefusal := t.deps.dependencyChange(ctx, UpdateWorkItemTool, args, before.Task)
	if dependencyRefusal != nil {
		return *dependencyRefusal, nil
	}
	// Before the patch, so a call that cannot write its dependency change
	// writes nothing.
	if !change.Empty() && t.deps.Dependencies == nil {
		return unconfigured(UpdateWorkItemTool), nil
	}
	// Labels last among the checks: declaring them is the call's first
	// PUBLISH, and a later refusal would leave them declared. Against the
	// task's HOME project, whose set the write is checked against.
	var declared []string
	// What the call tells its caller beside the change, in the order its
	// records landed.
	var caveats []caveat.Caveat
	if patch.Tags != nil {
		var labelRefusal *tools.Result
		if declared, caveats, labelRefusal = t.deps.declareLabels(ctx, actor, labelWrite{
			tool:   t.Name(),
			before: "changing " + before.Named(),
			notMade: fmt.Sprintf("Nothing it asked of %s was written by this "+
				"call.", before.Named()),
			then: "makes the change",
			twice: "applies the change a second time, and is refused as " +
				"stale if it names `if_match`",
			unvouched: "it answers with the change where this node still " +
				"holds its record",
			look: fmt.Sprintf("the change may have landed where this node "+
				"cannot see it: read %s with "+GetWorkItemTool+" to see whether it "+
				"is there rather than making it any other way", before.Address()),
		}, args, before.Task.Project, *patch.Tags); labelRefusal != nil {
			return *labelRefusal, nil
		}
	}

	answer := withOperation(ReceiptOf(map[string]any{
		"labels_created": declared,
	}, before), actor)
	var (
		patchOutcome statelog.Outcome
		patchAt      statelog.Position
		patchVersion int64
	)
	// The patch is skipped for a dependency-only call: an empty patch still
	// stamps a version and writes a `fields` history row that changed nothing.
	if !patch.Empty() {
		got, err := writer.UpdateTask(ctx, opID, before.Task.ID,
			before.Task.Project, ifMatch, patch, kind,
			worklet.Wake{
				Kind:   kind,
				Before: before.Task,
				After:  patched(before.Task, patch),
				// The reason is the change's excerpt, which the history row
				// and the wake already carry: no field of its own.
				Excerpt: reason,
			}.Notify(t.deps.Leads))
		if err != nil {
			return writeFailure(ctx, actor, UpdateWorkItemTool, err), nil
		}
		if got.Outcome == statelog.OutcomeUnknown {
			// A change nobody can say landed is not reported, its `version`
			// least of all (as `if_match` it may name a version the item
			// never had). The dependencies wait: the same call again answers
			// this write first and writes them after, once.
			return updateUnknown(actor, opID, got, before,
				!change.Empty()), nil
		}
		t.deps.settle(ctx, got.Position)
		answer["outcome"], answer["version"] = string(got.Outcome), got.Version
		answer["position"] = positionOf(got.Position)
		patchOutcome, patchAt, patchVersion = got.Outcome, got.Position, got.Version
		// The write's caveats (a timestamp truncated to its date, a body
		// naming unlinked items): a change the engine made to a typed value
		// must be told.
		caveats = append(caveats, got.Caveats...)
	}
	if !change.Empty() {
		result, err := t.deps.Dependencies(actor).Depend(ctx,
			opIDFor(actor, t.Name(), "depend", before.Task.ID, args), change, t.deps.Leads)
		_, patchRan := answer["outcome"]
		switch {
		case err != nil && patchRan:
			// The patch landed and is reported, as a create reports its
			// item: "the change was NOT made" would be false of every field
			// it set.
			return caveated(answer, caveats, partial(answer, DependenciesFailedKey,
				caveat.DependenciesNotRecorded(before.Address(), updateDependencyFailure(
					before, writeTold(ctx, actor, UpdateWorkItemTool, err)))))
		case err != nil:
			return writeFailure(ctx, actor, UpdateWorkItemTool, err), nil
		}
		t.deps.settle(ctx, result.Position)
		// The call answers for both records when the patch appended one
		// ([joinDepend]), else for the sequence alone, where version zero
		// (nothing landed on this item) is no `if_match` condition.
		if patchRan {
			outcome, at, version := joinDepend(patchOutcome, patchAt,
				patchVersion, result)
			answer["outcome"], answer["version"] = string(outcome), version
			answer["position"] = positionOf(at)
		} else {
			answer["outcome"], answer["version"] = string(result.Outcome), result.Version
			answer["position"] = positionOf(result.Position)
		}
		caveats = append(caveats, result.Caveats...)
		// Half-written edges are reported: a dependency whose mirror lost
		// its race is durable on the authoring side and repaired by the
		// Worklet duty.
		if len(result.OneSided) > 0 {
			answer["dependencies_pending_mirror"] = len(result.OneSided)
		}
	}
	return caveated(answer, caveats)
}

// updateUnknown explains an update whose outcome is unknown ([unknownWrite]).
// It points at the version it read: a read past it is what can say a change
// landed, and the history tells whether it was this one.
func updateUnknown(actor Actor, opID string, got worklet.WriteResult,
	before worklet.TaskDetail, dependencies bool) tools.Result {

	look := fmt.Sprintf("Read %s with "+GetWorkItemTool+" (a version past %d means "+
		"something landed, and its history says whether it was this)",
		before.Address(), before.Task.Version)
	next := unknownNext(got.Unvouched, sameCall(actor, UpdateWorkItemTool), look,
		"it is applied a second time, and refused as stale if it names `if_match`")
	if dependencies {
		next += " The dependency changes this call asked for were NOT written: " +
			"it stopped here, before them."
	}
	return unknownWrite(actor, UpdateWorkItemTool,
		fmt.Sprintf("the change to %s landed", before.Named()), opID, got.Unvouched, next)
}

// declareLabels declares the labels a write is about to use that its project
// does not have, only if the caller SAID SO: a meant `regression` and a typo
// `regresion` look identical at the call, and create-on-use would fill the
// filter strip with misspellings. It returns what it created, so a habitual
// flag still shows a typo in the answer. Its write is one of the call's own,
// derived through [opIDFor] so a repeated call declares under the same
// operation; its refusal is [labelFailure]'s, under the CALLING tool.
func (d WorkDeps) declareLabels(ctx context.Context, actor Actor,
	write labelWrite, args map[string]any, project string,
	labels []string) ([]string, []caveat.Caveat, *tools.Result) {

	if len(labels) == 0 || !argBool(args, "labels_create_missing") {
		return nil, nil, nil
	}
	if d.ProjectWriter == nil {
		// The refusal names the other route: a lead can still declare the
		// tag.
		return nil, nil, refusalOf(refusedBy(tools.RefusalUnavailable, waitless(), "This build "+
			"cannot declare labels at a write. Ask the project lead to "+
			"declare it, or file without the label."))
	}
	opID := opIDFor(actor, write.tool, "tags", project, args)
	created, caveats, err := d.ProjectWriter(actor).EnsureTags(ctx, opID,
		project, labels)
	if err != nil {
		return nil, nil, refusalOf(labelFailure(ctx, actor, write, opID, project, labels, err))
	}
	// The caveats are the call's, never items of the created list of slugs,
	// where a sentence would read as a tag.
	return created, caveats, nil
}

// labelWrite is the write a call was about to make when it declared its labels
// first ([WorkDeps.declareLabels]), in the words an answer about that
// declaration needs.
type labelWrite struct {
	// tool is the CALLING tool, which every answer names: the declaration is
	// a step of its call, never a call of its own to write_project.
	tool string
	// before is what the call stopped before doing, as a gerund phrase:
	// "filing the work item", "changing ENG-4".
	before string
	// notMade says, as a sentence, that this call did not make that write.
	notMade string
	// then is what the same call made again does once the declaration is
	// answered: "files it", "makes the change".
	then string
	// twice is what a DIFFERENT call does where an earlier attempt at this
	// one already made the write: "files a second item".
	twice string
	// unvouched is what the same call made again does with its OWN write
	// where this node cannot vouch for the declaration: every step of one
	// call carries the call's mint instant, so it cannot vouch for that
	// write either. A clause: "it answers with the item where …".
	unvouched string
	// look is the read that says whether that write is there, as a clause:
	// "look for it with list_work_items".
	look string
}

// labelFailure explains a create or an update refused, or stopped, at the
// inline declaration of its labels, under the CALLING tool and in terms of the
// write it did NOT make: never [writeFailure] under write_project, a tool the
// caller never called (and the declaration precedes the task's own append).
//
// The same call finishes it, since both writes are steps of it ([opIDFor],
// [sameCall]). An unvouched declaration is the exception: the repeat stops at
// that step until it reaches this node, so the answer prescribes declaring the
// tags with write_project (harmless: it states the same tags), and warns that
// the repeat's own write shares the call's instant ([Actor.OperationSince])
// and may answer `unknown` again.
func labelFailure(ctx context.Context, actor Actor, write labelWrite, opID, project string,
	labels []string, err error) tools.Result {

	var clash *worklet.TagClash
	var full *worklet.TagsFull
	switch {
	case errors.As(err, &clash):
		// A COLLISION ([worklet.ErrTaken]): another tag carries the label.
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s was refused, "+
			"and nothing was written: to use "+
			"the label %s it has to declare it in %s, where the tag %s is "+
			"already labelled %q — two tags nobody could tell apart. If they "+
			"mean the same thing, give `labels` %s instead of %s; if not, "+
			"declare %s in %s yourself under a label of its own ("+worklet.WriteProjectTool+" "+
			"on %s with tags_add: [{\"slug\": %q, \"label\": \"<a label %s "+
			"does not use>\"}]). Then make this call again.", write.tool,
			clash.Slug, clash.Project, clash.Other.Slug, clash.Other.Label,
			clash.Other.Slug, clash.Slug, clash.Slug, clash.Project,
			clash.Project, clash.Slug, clash.Project))
	case errors.As(err, &full):
		// THE TARGET PROJECT'S STATE ([worklet.ErrState]): the label is
		// fine, and the declaration lands once a tag is archived there.
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s was refused, "+
			"and nothing was written: to use "+
			"the label %s it has to declare it in %s, which already keeps %d "+
			"tags, the most a project may. Use a tag %s already has "+
			"("+worklet.DescribeProjectTool+" lists them) or leave %s out of `labels`; or have "+
			"%s's lead archive tags it no longer files under ("+worklet.WriteProjectTool+"'s "+
			"tags_archive), then make this call again.", write.tool, full.Slug,
			full.Project, worklet.MaxTagsPerProject, full.Project, full.Slug,
			full.Project))
	case errors.Is(err, worklet.ErrStepUnresolved), errors.Is(err, estate.ErrOutcomeUnknown):
		// A declaration no data node answered is this case too: it may have
		// landed, and the same operation again settles it.
		unvouched := errors.Is(err, worklet.ErrStepUnvouched)
		why := "the write's acknowledgement was lost"
		switch {
		case unvouched:
			why = "this node's operation ledger may have lost the record of " +
				"this operation, so this node cannot tell"
		case errors.Is(err, estate.ErrOutcomeUnknown):
			// FIXED WORDS, as [Unanswered] is: the router's error names
			// every node it asked.
			why = "a data node it was sent to did not answer"
		}
		if actor.Operation != "" {
			opID = actor.Operation
		}
		stopped := fmt.Sprintf("%s stopped before %s: it first declares "+
			"whichever of its labels (%s) %s does not have yet, and whether "+
			"that declaration landed is unknown (%s; operation %s). %s",
			write.tool, write.before, strings.Join(labels, ", "), project, why,
			opID, write.notMade)
		// THE CAUSE IS THE UNKNOWN STEP ([writeCause]), so a caller
		// answering in status codes reads it as the unknown it is rather
		// than as a refusal of what was asked.
		cause := writeCause(actor, err)
		again := sameCall(actor, write.tool)
		switch {
		case again == "":
			return refusedBy(tools.RefusalUnavailable, cause, stopped+" Making "+
				"the call again is a new operation: it declares the labels again — harmless, since that "+
				"states the same tags — then "+write.then+".")
		case unvouched:
			adds := make([]string, len(labels))
			for i, label := range labels {
				adds[i] = fmt.Sprintf("{\"slug\": %q}", label)
			}
			// AN `op_id` TRAVELS, so another node can be asked.
			elsewhere := ""
			if actor.Operation != "" {
				elsewhere = ", or make the same call with that `op_id` through " +
					"another node's operator MCP, whose ledger may reach back " +
					"that far"
			}
			return refusedBy(tools.RefusalUnavailable, cause, fmt.Sprintf("%s The "+
				"same call made here stops at this step "+
				"until the declaration reaches this node, and never if it did "+
				"not land. Declaring a tag again is harmless, so declare them "+
				"first: "+worklet.WriteProjectTool+" on %s with tags_add: [%s] states the same "+
				"tags as a new operation, and changes nothing if the first "+
				"declaration landed. Then %s. That repeat skips the declaration, "+
				"but its own write dates from the same instant, so this node "+
				"cannot vouch for it either: %s, and otherwise answers `unknown` "+
				"again — which then means %s%s. Do not reword it: if an earlier "+
				"attempt at this call got past this step, a different call %s.",
				stopped, project, strings.Join(adds, ", "), again,
				write.unvouched, write.look, elsewhere, write.twice))
		}
		return refusedBy(tools.RefusalUnavailable, cause, fmt.Sprintf("%s %s: that "+
			"is the same operation — it answers the "+
			"declaration with what landed, then %s, once. Do not reword it: if "+
			"an earlier attempt at this call got past this step, a different call %s. "+
			"Do not report it as done until a call answers without this error.",
			stopped, capitalize(again), write.then, write.twice))
	}
	if reused := reusedOperation(actor, write.tool, err); reused != "" {
		return refusedBy(tools.RefusalInvalid, err, reused)
	}
	// The error's own class as every write reads it ([writeFailure]), under
	// a sentence naming the write not made; a fault is told in fixed words
	// ([toldOf]), its error already logged.
	failure := writeFailure(ctx, actor, write.tool, err)
	return tools.Result{Failed: true, Cause: failure.Cause,
		Output: fmt.Sprintf("%s was refused before %s: declaring its labels in "+
			"%s did not land (%s). Nothing was written — do not report it as "+
			"done.", write.tool, write.before, project, toldOf(failure, err))}
}

// patchFromArgs builds the patch and the change kind, or the refusal to show
// the model. The kind is decided here, not derived from the patch: a change
// with several fields is still about ONE thing (a status move that set an
// assignee is a status change), and the order below is that judgement, most
// specific first.
func patchFromArgs(args map[string]any, actor Actor, now time.Time,
	loc *time.Location) (worklet.TaskPatch, worklet.ChangeKind, string) {

	var patch worklet.TaskPatch
	kind := worklet.ChangeFields

	// Scheduling first, so a refusal about a typed date comes before
	// anything else, and a status or assignee below can still set the kind.
	plan, refusal := readSchedule(args, now, loc, UpdateWorkItemTool)
	if refusal != "" {
		return patch, kind, refusal
	}
	if refusal = readWait(args, now, loc, &plan, UpdateWorkItemTool); refusal != "" {
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
		kind = worklet.ChangeAssignee
	}
	if raw := strings.TrimSpace(argString(args, "priority")); raw != "" {
		priority := worklet.Priority(raw)
		if !priority.Valid() {
			return patch, kind, fmt.Sprintf("%q is not a priority. The "+
				"priorities are: %s.", clip(raw), words.And(worklet.Priorities))
		}
		patch.Priority = &priority
		// The kind stays `fields`: `prioritised` is somebody writing YOUR
		// OWN priority list, routed from Snapshot.Person, and is not in
		// [worklet.ChangeKind.TaskCommit], so on a task it would wake nobody.
	}
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := worklet.Status(raw)
		if !status.Valid() {
			return patch, kind, fmt.Sprintf("%q is not a status. The statuses "+
				"are: %s.", clip(raw), words.And(worklet.Statuses))
		}
		patch.Status = &status
		kind = worklet.ChangeStatus
	}
	if _, held := args["labels"]; held {
		labels := argStrings(args, "labels")
		patch.Tags = &labels
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeTags
		}
	}
	if watch, held := argval.Bool(args["watch"]); held {
		// A gesture about one person, never the set: [worklet.TaskPatch]'s
		// collections are carried whole, so `watchers: [me]` would remove
		// everybody else. The writer resolves it against the current sets
		// inside its own snapshot. It is the author's own watch: a person
		// bound to a seat writes AS that seat, and the credential
		// ([Actor.OperatorID]) is never an address.
		patch.Watch = &worklet.WatchIntent{Handle: actor.Handle, Watch: watch}
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeWatchers
		}
	}
	if raw, held := args["fields"]; held {
		fields, refusal := fieldMap(raw)
		if refusal != "" {
			return patch, kind, refusal
		}
		// The kind stays `fields`, the default: what a custom field moving
		// is.
		patch.Fields = &fields
	}
	return patch, kind, ""
}

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

// patched is the task as the write will leave it, for the wake's snapshot:
// applied here, since the change has not landed and a read after the write
// would race other writers.
func patched(task worklet.Task, patch worklet.TaskPatch) worklet.Task {
	// The writer's own merge, never a copy, which would miss new fields and
	// have [worklet.TaskDeltas] report changes that changed nothing.
	task = worklet.Patched(task, patch)

	if patch.Watch != nil {
		// The gesture applied to the snapshot this tool read: the durable
		// sets are the writer's, settled in its transaction, and this is
		// only the wake's recipients; without it a new watcher would miss
		// the wake announcing it.
		task.Watchers = appendMissing(remove(task.Watchers, patch.Watch.Handle),
			patch.Watch.Handle, patch.Watch.Watch)
		task.Muted = appendMissing(remove(task.Muted, patch.Watch.Handle),
			patch.Watch.Handle, !patch.Watch.Watch)
	}
	if patch.Checklist != nil {
		// Through the writer's own function over the snapshot read, so the
		// wake's delta and recipients follow what the gesture does. One this
		// read cannot resolve leaves the snapshot as read; the decide
		// resolves it as the task lands.
		if lists, err := worklet.ApplyChecklist(task.Checklists, *patch.Checklist); err == nil {
			task.Checklists = lists
		}
	}
	return task
}

// ---- comment_on_work_item ---------------------------------------------- //

type commentOnWorkItem struct {
	itemTarget
	deps WorkDeps

	// pages checks a decision's page evidence — see [evidenceLookup]. Nil
	// on a company with no Doclet.
	pages PageReader
}

var _ tools.SeatCallable = (*commentOnWorkItem)(nil)

func (t *commentOnWorkItem) Name() string { return CommentOnWorkTool }

func (t *commentOnWorkItem) Description() string {
	return commentToolDescription
}

// commentToolDescription is the comment tool's contract as a model reads it,
// the wake rule included: another agent's comment wakes a colleague only when
// it names them ([worklet.CandidateFor]), so an agent relying on the assignee
// hearing an unaddressed note would hand work to nobody. A constant so a test
// can hold the words.
const commentToolDescription = "Post a comment on a work item. Everyone " +
	"following the item finds it in their inbox, but your comment wakes a " +
	"colleague to act on it only when it asks them: an @-mention with " +
	"`asks: true`, a question put to them with `ask`, or a reply (`reply_to`) " +
	"in a thread where they asked or were asked something still open. An " +
	"@-mention with `asks: false` is a reference and wakes nobody, and a " +
	"comment that asks nothing wakes nobody, not even the item's assignee or " +
	"its reporter: it waits until they next open the item. So name whoever " +
	"you need to act, and say what you need from them. Post ONE " +
	"substantive comment when you have something to say: running commentary " +
	"is noise on a surface other people read, and an acknowledgement that " +
	"asks nothing needs no comment at all. `ask` puts a question to somebody, " +
	"and `decision` offers it as options with the one you recommend, which " +
	"every question an agent puts to a person carries; `answers` with `choice` " +
	"answers one."

func (t *commentOnWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to comment on. " + itemRef,
			},
			"body": map[string]any{
				"type": "string",
				"description": "The comment, in markdown. @-mention a " +
					"colleague by handle, with `asks: true`, to wake them to act on it.",
			},
			"asks": asksParam(workAsks),
			"ask": map[string]any{
				"type": "string",
				"description": "A colleague's handle: this comment is a " +
					"QUESTION they owe an answer to. They are woken asking " +
					"for one and start following the item. It does not hand " +
					"the item over, and it does not stop anybody closing it. " +
					"An agent always asks a person with a `decision`.",
			},
			"decision": decisionSchema(),
			"answers": map[string]any{
				"type": "string",
				"description": "The comment id of the question this answers, " +
					"which closes it. Omitted, it is inferred when exactly " +
					"one open question on the item is addressed to you.",
			},
			"choice": map[string]any{
				"type": "string",
				"description": "When the question you answer carries a " +
					"decision: the id of the option you choose. `body` is " +
					"then optional — say why in it. Leave it out to answer " +
					"in prose when none of the options is right.",
			},
			"reply_to": map[string]any{
				"type": "string",
				"description": "The id of the comment you are replying to. " +
					"This threads the conversation and wakes everybody " +
					"already in that thread; it does not close a question — " +
					"`answers` is what does.",
			},
		},
		"required": []any{"item"},
	})
}

func (t *commentOnWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *commentOnWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(CommentOnWorkTool, err), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(CommentOnWorkTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, CommentOnWorkTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	writer := t.deps.Writer(actor)

	ref := strings.TrimSpace(argString(args, "item"))
	body := strings.TrimSpace(argString(args, "body"))
	choice := strings.TrimSpace(argString(args, "choice"))
	rawDecision, decided := args["decision"]
	agent := actor.Kind == worklet.AuthorAgent
	asks, refused := asksOf(CommentOnWorkTool, args, agent)
	switch {
	case refused != nil:
		return *refused, nil
	case ref == "":
		return failed(worklet.CommentOnWorkTool + " needs an `item` — a key like ENG-42, or an id."), nil
	case body == "" && choice == "":
		// A CHOICE IS AN ANSWER ON ITS OWN: the option is what the asker
		// was waiting for, and a reason is welcome but not owed.
		return failed(worklet.CommentOnWorkTool + " needs a `body`. Say the substantive " +
			"thing, once — or, answering a decision, a `choice`."), nil
	case decided && strings.TrimSpace(argString(args, "ask")) == "":
		// Refused before any read: a decision asked of nobody wakes nobody,
		// whatever the item says.
		return failed(worklet.CommentOnWorkTool + ": `decision` needs `ask` — the " +
			"handle of the person who has to choose. A decision asked of " +
			"nobody wakes nobody."), nil
	case decided && choice != "":
		return failed(worklet.CommentOnWorkTool + ": a comment either asks a decision " +
			"(`decision`) or answers one (`choice`), never both — they are two " +
			"comments, by two people."), nil
	}
	// Who may post is decided on the item (workasked.go): a tracker holder as
	// ever, anybody else only as THE ANSWER to an open ask naming them, so
	// what can never be an answer is refused before the item is read.
	holder, refused := t.deps.itemGate(ctx, authz.ActionWorkComment)
	if refused != nil {
		return *refused, nil
	}
	var before worklet.TaskDetail
	if holder {
		before, err = t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
		switch {
		case errors.Is(err, worklet.ErrNoTask):
			return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
				clip(ref))), nil
		case err != nil:
			return readFailure(ctx, CommentOnWorkTool, err), nil
		}
	} else {
		// A QUESTION OF YOUR OWN IS NOT AN ANSWER: `ask` and `decision`
		// put one to somebody, which is Worklet's to do.
		if decided || strings.TrimSpace(argString(args, "ask")) != "" {
			return *t.deps.answerOnly(ctx, "`ask` and `decision` put a new "+
				"question to somebody, which takes Worklet."), nil
		}
		// WITH THE ID THIS COMMENT WILL CARRY, so the answer made again
		// after it landed — which closed the ask that admitted it — is
		// admitted as the answer it is, and answered from its row.
		if before, refused = t.deps.askedOn(ctx, authz.ActionWorkComment, ref,
			actor.Handle, func(task string) string {
				return commentID(actor, t.Name(), task, args)
			}); refused != nil {
			return *refused, nil
		}
	}

	now := t.deps.now()
	comment := &worklet.Comment{
		// Derived from the operation, so a re-run turn posts once.
		ID:         commentID(actor, t.Name(), before.Task.ID, args),
		Task:       before.Task.ID,
		Author:     actor.Handle,
		AuthorKind: actor.Kind,
		Body:       body,
		Choice:     choice,
		CreatedAt:  now,
	}
	if decided {
		decision, refusal := t.deps.readDecision(ctx, CommentOnWorkTool,
			actor, rawDecision, t.pages)
		if refusal != nil {
			return *refusal, nil
		}
		comment.Decision = decision
		// WHAT THE ASK IS FOR, where an engine tool composed it: never
		// read from the caller's arguments (ADR-0125).
		if act, ok := delegated.ActOf(ctx); ok {
			comment.Act = &act
		}
	}
	if reply := strings.TrimSpace(argString(args, "reply_to")); reply != "" {
		comment.ReplyTo = &reply
	}
	if t.deps.Mentions != nil {
		comment.Mentions = t.deps.Mentions.Mentions(body)
	}
	// The ask resolves like any handle: one naming nobody leaves a question
	// open forever, addressed to a spelling.
	ask := strings.TrimSpace(argString(args, "ask"))
	if ask != "" {
		resolved, refusal := t.deps.resolveHandle(ctx, CommentOnWorkTool, "`ask`", ask)
		if refusal != "" {
			return failed(refusal), nil
		}
		if refused := t.deps.checkAskOfPerson(CommentOnWorkTool, actor, resolved, decided); refused != nil {
			return *refused, nil
		}
		ask = resolved
	}
	if refused := t.deps.checkAsksOfNamed(CommentOnWorkTool, actor, asks, ask, comment.Mentions); refused != nil {
		return *refused, nil
	}
	query := worklet.ThreadQuery{
		Task:    before.Task.ID,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
		Ask:     ask,
		Answers: strings.TrimSpace(argString(args, "answers")),
		// The author, the seat for a bound person: an ask names a seat.
		Author: actor.Handle,
		// This comment's own id, so a re-run whose first answer landed is
		// not told that somebody (itself) already answered.
		Comment: comment.ID,
		Choice:  comment.Choice,
	}
	var thread worklet.ResolvedThread
	var threadRefusal *tools.Result
	if holder {
		thread, threadRefusal = t.deps.resolveThread(ctx, query)
	} else {
		// STRICT ON THIS ARM: the answer is the whole of what admits this
		// caller, so an inference this node could not make is a read that
		// failed rather than a remark that answers nothing.
		thread, threadRefusal = t.deps.readThread(ctx, query)
	}
	if threadRefusal != nil {
		return *threadRefusal, nil
	}
	if !holder && thread.Answers == "" {
		return *t.deps.answerOnly(ctx, "this comment answers no open question "+
			"addressed to you. Name it with `answers` (its comment id)."), nil
	}
	comment.Ask = thread.Asked
	if thread.Answers != "" {
		comment.Answers = &thread.Answers
	}
	if comment.Choice != "" && comment.Answers == nil {
		// A CHOICE THAT ANSWERS NOTHING: no `answers` was named and no
		// open question on the item is addressed to this caller, so there
		// is no decision for the option to be one of.
		return toPerson(failed(fmt.Sprintf(worklet.CommentOnWorkTool+": `choice` %q "+
			"answers a decision, and no open question on %s is addressed to "+
			"you. Name the question with `answers` (its comment id), or answer "+
			"in `body` without a choice.", clip(comment.Choice),
			before.Named())), caveat.NoQuestionForChoice()), nil
	}
	if refused := t.deps.grantChosen(ctx, comment.Choice, thread); refused != nil {
		return *refused, nil
	}
	// AN ANSWER THAT APPROVES AN ACT is that act's approval, so whether this
	// person may give one, at its proof, is decided here and carried to the
	// tracker's check of the choice against its ask (ADR-0125).
	if comment.Choice != "" && t.deps.ApprovesAct != nil {
		for _, kind := range delegated.DelegableKinds() {
			if t.deps.ApprovesAct(ctx, kind) {
				ctx = delegated.WithApproval(ctx, kind)
			}
		}
	}

	// A comment rides the task's own write, sharing its arbitration. The
	// commenter's watch is a GESTURE, not a set: a set read outside the
	// writer's decide would be last-write-wins over the watchers and could
	// pass [worklet.MaxWatchers]. It is resolved against the current row
	// inside the decide, and `Auto` since nobody pressed watch: at the cap
	// the comment lands and the watch is skipped.
	patch := worklet.TaskPatch{Comment: comment}
	if holder {
		// NOT FOR SOMEBODY ADMITTED ONLY TO ANSWER: following the item
		// would wake them on every later change to a board they do not
		// use, and the asker hears their answer whether or not they do.
		patch.Watch = &worklet.WatchIntent{
			// THE AUTHOR'S, the same address the explicit gesture
			// takes.
			Handle: actor.Handle, Watch: true, Auto: true,
		}
	}
	opID := opIDFor(actor, t.Name(), "comment", comment.ID, args)
	notify := worklet.Wake{
		Kind:   worklet.ChangeComment,
		Before: before.Task,
		// Through the simulation every write uses ([patched]), so the
		// gesture is applied once, cap included.
		After:   patched(before.Task, patch),
		Comment: comment, Mentions: comment.Mentions,
		Asks:            t.deps.statedAsks(agent, asks || comment.Ask != ""),
		MentionedAgents: t.deps.agentsAmong(agent, comment.Mentions),
		Thread:          thread.ThreadParties,
		AnswersDecision: thread.AnswersDecision,
	}.Notify(t.deps.Leads)
	got, err := writer.UpdateTask(ctx, opID, before.Task.ID, before.Task.Project,
		// A comment never conditions on a version: it replaces nobody's
		// value.
		worklet.NoIfMatch, patch, worklet.ChangeComment, notify)
	if err != nil {
		return writeFailure(ctx, actor, CommentOnWorkTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// A comment nobody can say was posted is not reported: its id and
		// ask beside "unknown" would read as a posted remark.
		return unknownWrite(actor, CommentOnWorkTool,
			fmt.Sprintf("the comment on %s was posted", before.Named()), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, CommentOnWorkTool),
				fmt.Sprintf("Read %s's thread with "+GetWorkItemTool, before.Address()),
				"that is a second comment")), nil
	}
	if got.Posted != nil {
		// This call made again, whose first attempt posted: the writer
		// answered from that row and published nothing, so the row (in the
		// write's own snapshot, later than the thread read) says what it
		// asked and answered.
		comment.Ask = got.Posted.Asked
		comment.Answers = nil
		if got.Posted.Answers != "" {
			answered := got.Posted.Answers
			comment.Answers = &answered
		}
	}
	t.deps.settle(ctx, got.Position)
	t.deps.requestDeclined(ctx, comment.Choice, actor.Handle, thread)
	answer := withOperation(ReceiptOf(map[string]any{
		"comment_id": comment.ID,
		"mentioned":  comment.Mentions, "outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version": got.Version,
	}, before), actor)
	if comment.Ask != "" {
		answer["asked"] = comment.Ask
	}
	if comment.Answers != nil {
		answer["answered"] = *comment.Answers
	}
	if comment.Choice != "" {
		answer["choice"] = comment.Choice
	}
	if comment.Decision != nil {
		// WHAT THE PERSON ASKED WILL CHOOSE BETWEEN, as it was stored — a
		// task named by key in the evidence comes back as its id.
		answer["decision"] = comment.Decision
	}
	// The warning the caller cannot see: a non-assignee's comment naming
	// nobody starts no turn or wakes the assignee unasked, so an expected
	// answer may never come.
	var caveats []caveat.Caveat
	if unaddressed := unaddressedCaveat(before.Task, actor, comment, notify); !unaddressed.IsZero() {
		caveats = append(caveats, unaddressed)
	}
	return ownReceipt(ctx, turn, actor, CommentOnWorkTool, answer, caveats)
}

// ownReceipt answers a write's receipt, which may echo what the caller just
// wrote (an ask's question and options), through a [foreignRead] labelling
// those words as the caller's own (ADR-0040); a receipt echoing nothing is
// answered plainly. Both paths carry the write's caveats cs and partials
// through [caveated] ([caveatedBy]), so none is dropped for echoing an ask.
func ownReceipt(ctx context.Context, turn *turnctx.Turn, actor Actor, tool string,
	answer map[string]any, cs []caveat.Caveat, partials ...caveat.Caveat) (tools.Result, error) {

	declared, err := declaredIn(answer)
	if err != nil || len(declared) == 0 {
		return caveated(answer, cs, partials...)
	}
	read := newForeignRead(turn, actor.Handle)
	read.attribute([]provenance.Author{read.writer(actor.Handle, iam.ActorKind(actor.Kind))}, false)
	return caveatedBy(func(v any) (tools.Result, error) {
		return read.answer(ctx, tool, v, "")
	}, answer, cs, partials...)
}

// unaddressedCaveat says when a comment leaves the item's assignee unasked:
// not woken at all, or woken without being asked anything. notify is the
// record the comment publishes, so what it says is the routing's own answer.
func unaddressedCaveat(task worklet.Task, actor Actor, comment *worklet.Comment,
	notify *worklet.Notify) caveat.Caveat {

	switch {
	// AGAINST THE AUTHOR, which is a bound person's seat: an assignee is
	// a seat, and a person commenting on their own task is not waking
	// themselves.
	case task.Assignee == "" || task.Assignee == actor.Handle:
		return caveat.Caveat{}
	case comment.Ask != "" || (len(comment.Mentions) > 0 && notify.MentionsWake()):
		return caveat.Caveat{}
	// An answer expects no reply: it closes a question and wakes the asker.
	case comment.Answers != nil:
		return caveat.Caveat{}
	}
	// WHETHER IT WAKES THEM, AND ASKS THEM, is the routing's answer for this
	// record: a person's comment on the task is the ask and needs no
	// warning; another agent's reaches the assignee's inbox and starts no
	// turn, unless it is a reply in a thread where they still owe an answer,
	// which wakes them without asking them anything (ADR-0113).
	heard, ok := worklet.CandidateFor(notify, task.Assignee)
	if !ok || heard.Addressed {
		return caveat.Caveat{}
	}
	return caveat.UnaddressedComment(task.Assignee, heard.Wakes)
}

// commentID is the comment's own id, derived so a re-run turn posts once: a
// UUIDv5, since the id is a PRIMARY KEY on every node and an apply-time id
// would make two rows.
//
// From the call's key ([Actor.callKey]) and digest, as [opIDFor], so every
// different call is a different comment (the apply posts only where no row
// holds the id). But the key as the work began ([Actor.asBegun]): a rebase
// re-mints operations for the ledger, while the comment's row remembers, so
// the writer answers a re-post from it and wakes nobody twice. A caller with
// no turn derives it from [Actor.Operation], without the arguments, for
// [opIDFor]'s reason. Why a UUIDv5: package doc, "What the tracker's tools
// derive from a call's key".
func commentID(actor Actor, tool, taskID string, args map[string]any) string {
	if actor.Operation != "" {
		return uuid.NewSHA1(commentNamespace, []byte(actor.Operation+"\x00"+
			taskID+"\x00"+actor.Handle)).String()
	}
	key := actor.asBegun().callKey(tool, nil, args, "")
	if key == "" {
		key = uuid.NewString()
	}
	return uuid.NewSHA1(commentNamespace, []byte(key+"\x00"+taskID+"\x00"+
		actor.Handle+"\x00"+turnctx.ArgsDigest(args))).String()
}

// commentNamespace is the uuid namespace comment ids are derived under, FIXED
// for the life of the format (package doc, "What the tracker's tools derive
// from a call's key").
var commentNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// ---- shared rendering -------------------------------------------------- //

// jsonResult renders a value as the tool's output: JSON, since these results
// are STRUCTURED and a model parsing a rendered table back gets fields wrong.
// It is [jsonAnswer] for answers bounded by their own shape (a receipt, a
// refusal); the ceiling still applies, since a new field is how one stops
// being small.
func jsonResult(v any) (tools.Result, error) {
	return jsonAnswer(v, "Narrow what you asked for.")
}

// ReadRefusalSentence is a read's refusal of what it was asked (an item or
// page that is not there, a query the tracker cannot run, a floor, cursor or
// `since` on another log) in its packages' words, without the package names
// ("worklet: ", "doclet: ", "statelog: ") wherever in the message they sit,
// mid-message included. Exported for internal/api/workapi and
// internal/api/queries.
func ReadRefusalSentence(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"worklet: ", "doclet: ", "statelog: "} {
		msg = strings.ReplaceAll(msg, prefix, "")
	}
	return msg
}

// readFailure explains a read that could not be served, and never says
// "nothing found": a copy behind its log must not tell a seat there is no
// work, which it would duplicate or abandon.
//
// A read refused for what it asked ([worklet.ErrBadQuery],
// [statelog.ErrForeignPosition]) is refused alike on every attempt, so it is
// [tools.RefusalInvalid] and says the arguments must change. Everything else
// is this node's ([Condition]), never not-found: a CONDITION waiting clears is
// [tools.RefusalUnavailable] and says try again; a FAULT is
// [tools.RefusalInternalError], keeping its error (a path, a SQL fragment)
// for the log ([faulted]). The read's error rides beneath the class
// ([tools.Classify]).
func readFailure(ctx context.Context, name string, err error) tools.Result {
	if errors.Is(err, worklet.ErrBadQuery) || errors.Is(err, statelog.ErrForeignPosition) {
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused "+
			"what it was asked: %s. Nothing is wrong with Worklet; change the "+
			"arguments, because the same ones are refused again.", name,
			ReadRefusalSentence(err)))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not "+
			"read Worklet%s (%s). This is NOT an empty result: do not "+
			"conclude the item or the list does not exist. %s", name,
			rightNow(err), why,
			conditionNext(err, "Try again, or say you could not check.", stuckRead)))
	}
	return faulted(ctx, name, err, fmt.Sprintf("%s could not read Worklet: "+
		"%s. This is NOT an empty result: do not conclude the item or the list "+
		"does not exist. Say you could not check.", name, faultSaid))
}

// writeFailure explains a write that did not land, in terms the model can act
// on. Each sentinel has its own class, which a person's surface acts on: a
// stale version is re-read, a lost race retried, a spent hand-off budget never
// retried, a refused append is this node's condition.
//
// A content refusal is MARKED and the unmarked is the node's: the writer wraps
// [worklet.ErrInvalid] (what was asked) or [worklet.ErrState] (what the object
// is now) on every refusal it writes, and types [worklet.TextCapError]. The
// node's is a CONDITION ([tools.RefusalUnavailable], retry invited) or a FAULT
// ([tools.RefusalInternalError], logged by [faulted]), as in [readFailure].
// It takes the actor because the same call again is the caller's ([sameCall]).
func writeFailure(ctx context.Context, actor Actor, name string, err error) tools.Result {
	// The error rides beneath the class on every arm, and a gesture stopped
	// at an unknown step carries that unknown ([writeCause]).
	out := func(class tools.Refusal, msg string) tools.Result {
		return refusedBy(class, writeCause(actor, err), msg)
	}
	if reused := reusedOperation(actor, name, err); reused != "" {
		return out(tools.RefusalInvalid, reused)
	}
	var clash *worklet.TagClash
	var full *worklet.TagsFull
	switch {
	case errors.As(err, &clash) && name == worklet.WriteProjectTool:
		// THE DECLARATION ITSELF, so the remedy is this call with another
		// label, or the tag that already has this one: a COLLISION
		// ([worklet.ErrTaken]), never a malformed label.
		return out(tools.RefusalExists, fmt.Sprintf("%s was refused, and nothing was written: %s already "+
			"has a tag %s labelled %q, and %s (%q) would be a second tag nobody "+
			"could tell apart from it. Use %s if it means the same thing, or "+
			"give %s a label of its own.", name, clash.Project,
			clash.Other.Slug, clash.Other.Label, clash.Slug, clash.Label,
			clash.Other.Slug, clash.Slug))
	case errors.As(err, &clash):
		// A label two projects use for different tags, met declaring tags in
		// a move's target or a write's own project before its first append;
		// named with the two ways past it.
		return out(tools.RefusalExists, fmt.Sprintf("%s was refused, and nothing was written: it has to "+
			"declare the tag %s (%q) in %s, where the tag %s is already labelled "+
			"%q — two tags nobody could tell apart. If they mean the same thing, "+
			"put the items under %s instead ("+UpdateWorkItemTool+"'s `labels`); if "+
			"not, declare %s in %s yourself under a label of its own "+
			"("+worklet.WriteProjectTool+" on %s with tags_add: [{\"slug\": %q, \"label\": "+
			"\"<a label %s does not use>\"}]). Then make this call again.",
			name, clash.Slug, clash.Label, clash.Project, clash.Other.Slug,
			clash.Other.Label, clash.Other.Slug, clash.Slug, clash.Project,
			clash.Project, clash.Slug, clash.Project))
	case errors.As(err, &full):
		// THE PROJECT'S STATE, not the call's: the add is well formed and
		// lands once a tag is archived to make room ([worklet.ErrState]).
		return out(tools.RefusalConflict, fmt.Sprintf("%s was refused, and nothing was written: %s already "+
			"keeps %d tags, the most a project may, and %s is not among them. "+
			"Archive tags %s no longer files under ("+worklet.WriteProjectTool+"'s "+
			"tags_archive — its lead's decision), or take %s off the items with "+
			UpdateWorkItemTool+", then make this call again.", name, full.Project,
			worklet.MaxTagsPerProject, full.Slug, full.Project, full.Slug))
	case errors.Is(err, worklet.ErrNoTask):
		return out(tools.RefusalNotFound, fmt.Sprintf("%s: %s.", name, worklet.Sentence(err)))
	case errors.Is(err, worklet.ErrForbidden):
		// NOT A RACE. The sentence names who may, which is the only part
		// of it a caller can act on; re-reading changes nothing.
		return out(tools.RefusalForbidden, fmt.Sprintf("%s was refused: "+
			"%s. Re-reading will not change this — ask whoever the refusal "+
			"names.", name, worklet.Sentence(err)))
	case errors.Is(err, statelog.ErrConflict):
		// A lost race, in words rather than the error's broker path; other
		// domains tell it in [statelog.LostRace] ([WriterSentence]), a
		// person's surface in its code's sentence (ADR-0060).
		return out(tools.RefusalConflict, fmt.Sprintf("%s could not land: somebody "+
			"else is editing this item, so nothing was written. Read it again with "+
			GetWorkItemTool+" and decide from what it says now.", name))
	case errors.Is(err, statelog.ErrExists):
		// WHAT AN EARLIER ATTEMPT OF THIS WRITE MADE, most often: the same
		// create sent again once its operation was forgotten.
		return out(tools.RefusalExists, fmt.Sprintf("%s was refused: what it creates "+
			"is already there, so nothing was written. Look for it with "+
			ListWorkItemsTool+", or read it with "+GetWorkItemTool+", rather than "+
			"making it again.", name))
	case errors.Is(err, worklet.ErrStaleVersion):
		// THE ONE REFUSAL WHOSE ANSWER IS "READ IT AGAIN". Nothing is
		// wrong with the patch, so a model told only "it failed" would
		// re-send the same one against the same moved item forever.
		return out(tools.RefusalStaleVersion, fmt.Sprintf("%s was "+
			"refused: %s. Nothing is wrong with your edit — somebody "+
			"changed the item after you read it. Call "+GetWorkItemTool+" again "+
			"and decide from what it says now.", name, worklet.Sentence(err)))
	case errors.Is(err, worklet.ErrAlreadyAnswered):
		// An answer that lost a race: the sentence names who answered and
		// when; read that answer, never resend this one.
		return out(tools.RefusalAlreadyAnswered, fmt.Sprintf("%s was "+
			"refused: %s. Nothing was posted.", name, worklet.Sentence(err)))
	case errors.Is(err, inbox.ErrFull):
		// The inbox rules' own sentence, naming `read_through`, the one move
		// that empties these lists.
		return out(tools.RefusalInboxFull, fmt.Sprintf("%s was refused: "+
			"%s.", name, inbox.Sentence(err)))
	case errors.Is(err, estate.ErrOutcomeUnknown):
		// Sent, and no data node answered: any silent one may have run it,
		// so "NOT made" is false. The same OPERATION again settles it
		// ([sameCall]). In fixed words ([Unanswered]), not the router's list
		// of nodes. Ahead of the condition below, which would call a
		// cancelled caller's case "NOT made".
		return tools.Result{Output: fmt.Sprintf("%s: %s. It may have landed and "+
			"it may not — do not report it as done, and do not report it as "+
			"failed. %s", name, Unanswered, unknownNext(false, sameCall(actor, name),
			"Read what it writes with "+GetWorkItemTool+" or "+ListWorkItemsTool,
			"that is a second write")), Failed: true, Cause: writeCause(actor, err)}
	case errors.Is(err, worklet.ErrStepUnvouched):
		// An ErrStepUnresolved, so first: a retry here stops at the same
		// step every time, the ledger row it needs being the one the loss
		// took.
		return out(tools.RefusalUnavailable, fmt.Sprintf("%s stopped part of the way through: %s. Some of "+
			"it may already have landed, and this node cannot tell which — "+
			"calling %s again here stops at the same step. Do not report it as "+
			"done or as failed, and do not redo it by other means: read the "+
			"items it touches with "+GetWorkItemTool+", and say which part is "+
			"unconfirmed.", name, worklet.Sentence(err), name))
	case errors.Is(err, worklet.ErrStepUnresolved):
		// A walking gesture (a move, a merge, a dependency) stopped at an
		// unknown step: neither "NOT made" nor "done" is true. The same
		// OPERATION again finishes it ([sameCall]).
		again := sameCall(actor, name)
		if again == "" {
			return out(tools.RefusalUnavailable, fmt.Sprintf("%s stopped part of the way through: %s. Some "+
				"of it may already have landed. Calling %s again is a new "+
				"operation here and does not finish this one: read the items "+
				"it touches with "+GetWorkItemTool+" and finish what is missing with "+
				"the tool for each. Do not report it as done.", name, worklet.Sentence(err), name))
		}
		return out(tools.RefusalUnavailable, fmt.Sprintf("%s stopped part of the way through: %s. Some of "+
			"it may already have landed. %s — the retry answers what landed "+
			"and finishes the rest. Do not report it as done until a call "+
			"answers without this error.", name, worklet.Sentence(err), capitalize(again)))
	case errors.Is(err, worklet.ErrReassignmentBudget):
		// THE REFUSAL THAT MUST NOT INVITE ANOTHER ATTEMPT: this item is
		// circulating between agents, and a message that reads like a
		// transient failure is exactly what keeps it circulating.
		return out(tools.RefusalReassignmentBudget, fmt.Sprintf("%s was "+
			"refused: %s. Do not reassign it again. Comment on the item "+
			"saying what is blocking it and who you think should own it, "+
			"and leave it where it is.", name, worklet.Sentence(err)))
	}
	var capped *worklet.TextCapError
	if errors.As(err, &capped) {
		return out(tools.RefusalInvalid, textCapRefusal(capped))
	}
	if errors.Is(err, worklet.ErrState) {
		// What the object is now (an archived project, a task in the trash,
		// a set at its cap), in the writer's sentence naming who changes
		// it: re-reading will not help, and nothing about the call is wrong.
		return out(tools.RefusalConflict, fmt.Sprintf("%s refused that: %s",
			name, worklet.Sentence(err)))
	}
	if errors.Is(err, worklet.ErrInvalid) {
		// What was asked, in the writer's sentence alone, which names what
		// to change: the class a caller corrects its call by (ADR-0060 for
		// a person's surface).
		return out(tools.RefusalInvalid, fmt.Sprintf("%s refused that: %s",
			name, worklet.Sentence(err)))
	}
	if class, ok := WriterClass(err); ok {
		// Any other refusal a domain decided, in its class and its writer's
		// sentence ([WriterClass]), never told as the node breaking.
		return out(class, domainRefusal(name, err))
	}
	if why, ok := Condition(err); ok {
		return out(tools.RefusalUnavailable, sentence(fmt.Sprintf("%s did not land (%s). "+
			"The change was NOT made — do not report it as done.", name, why),
			conditionNext(err, "", stuckWrite)))
	}
	// NOT "NOT MADE": a fault after the append was acknowledged — the
	// ledger or the gates unreadable while it resolved — leaves a durable
	// record nobody here can account for, so the sentence claims neither
	// way and forbids the one move that could make a second copy.
	return faulted(ctx, name, err, fmt.Sprintf("%s failed: %s. Do not report "+
		"it as done, and do not make it again by other means — say it could "+
		"not be made.", name, faultSaid))
}

// textCapRefusal says which of a task's texts is too long, by how much, and
// what to do with the rest, for a model and a person alike (the invalid class
// is shown verbatim). Composed from the typed fields, not the writer's Go
// error, which carries a prefix and the id of a task never created.
func textCapRefusal(e *worklet.TextCapError) string {
	argument, holds := "`title`", "a task's title"
	switch e.Field {
	case worklet.TextBody:
		argument, holds = "`body`", "a task's description"
	case worklet.TextComment:
		argument, holds = "`body`", "a comment"
	}
	return fmt.Sprintf("%s is %d bytes and %s holds at most %d, so nothing was "+
		"changed. Shorten it, or put the long form on a page and link it here "+
		"— a value is refused rather than cut.", argument, e.Size, holds, e.Limit)
}

// reusedOperation explains a write refused because the caller's operation
// already names a write to another object, or is "" for any other error.
//
// A brought-back `op_id` carries its call's arguments
// ([WorkDeps.bindOperation]), but the world can move under them (a move
// aliases the item's CURRENT key), and the state log then refuses the step
// ([statelog.ReasonOpReused]). The remedy is an operation of its own: leave
// `op_id` out, or for a request a new key ([sameCall]'s split). A seat has no
// operation to leave out, so it gets "".
func reusedOperation(actor Actor, tool string, err error) string {
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonOpReused {
		return ""
	}
	// THE REFUSAL'S OWN FACTS, without the framework's prefix: its reason
	// and the two objects its detail names.
	where := string(refusal.Reason)
	if refusal.Detail != "" {
		where += ": " + refusal.Detail
	}
	if actor.keyedRequest() {
		return fmt.Sprintf("%s was refused, and this request's change was NOT "+
			"made: the operation its Idempotency-Key names already wrote to "+
			"something else (%s) — what it names has changed since the request "+
			"was first sent — so it cannot finish that request here. Read the "+
			"item again, and send this as a new request, under a new key.",
			tool, where)
	}
	if actor.Operation == "" {
		return ""
	}
	return fmt.Sprintf("%s was refused, and this call's change was NOT made: "+
		"the operation `op_id` %q names already wrote to something else (%s) — "+
		"what this call names has changed since the call that op_id was "+
		"answered for — so it cannot finish that call here. Read the item "+
		"again, and leave `op_id` out to make this as a new write.",
		tool, actor.Operation, where)
}

// sameCall says how this caller makes THIS call again as the same operation,
// as a clause, or "" where no repeat is. One place for four answers: a SEAT
// repeats the same arguments before any different call ([Actor.callKey]); a
// REQUEST resends under its key ([Actor.keyedRequest]); an OPERATOR brings
// back its `op_id`; any other caller mints afresh, so a repeat it is told to
// make is a second write.
func sameCall(actor Actor, tool string) string {
	switch {
	case actor.Operation != "":
		return fmt.Sprintf("call %s again with exactly the same arguments and "+
			"`op_id` %q", tool, actor.Operation)
	case actor.keyedRequest():
		return fmt.Sprintf("send the same %s request again — under the same "+
			"Idempotency-Key, with exactly the same arguments", tool)
	case actor.OperationSeed() != "":
		return fmt.Sprintf("call %s again with exactly the same arguments, "+
			"before calling it with any others", tool)
	}
	return ""
}

// writeTold is how a write's error reads inside a sentence that names what it
// stopped — a walk part of the way through, an item filed whose dependencies
// did not follow — classed the one way every write's error is
// ([writeFailure]), so a fault is told in [faultSaid] and logged there, and
// anything else in its own words ([toldOf]). A stop that carries no error
// says so rather than printing a nil.
func writeTold(ctx context.Context, actor Actor, tool string, err error) string {
	if err == nil {
		return "it named no reason"
	}
	return toldOf(writeFailure(ctx, actor, tool, err), err)
}

// capitalize upper-cases a clause's first letter, for a clause that opens a
// sentence. ASCII only, which is every clause [sameCall] writes.
func capitalize(clause string) string {
	if clause == "" || clause[0] < 'a' || clause[0] > 'z' {
		return clause
	}
	return string(clause[0]-'a'+'A') + clause[1:]
}

// bindOperation settles which operation this call is, or answers the refusal
// ([operations.bindActor]).
func (d WorkDeps) bindOperation(actor Actor, tool string,
	args map[string]any) (Actor, string) {

	return d.ops.bindActor(actor, tool, args)
}

// withOperation puts the call's operation on its answer where it has one: the
// `op_id` a caller brings back ([WorkDeps.bindOperation]). Never for a
// request, which repeats its key and would have an `op_id` refused; such a
// caller holds no [Actor.Operation].
func withOperation(answer map[string]any, actor Actor) map[string]any {
	if actor.Operation != "" {
		answer[opIDArg] = actor.Operation
	}
	return answer
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

// defaultProject is the caller's own, or empty where their unit owns none: one
// spelling for the five tools that fall back to it. It takes the actor and
// asks about its author: a person bound to a seat writes AS it
// ([PrincipalActor]), and an unbound credential is in no chart.
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

// openGroups is the status groups where work has not finished, derived from
// the closed set so a new group cannot leave a stale literal.
func openGroups() []string {
	out := make([]string, 0, len(worklet.StatusGroups))
	for _, group := range worklet.StatusGroups {
		if group.Open() {
			out = append(out, string(group))
		}
	}
	return out
}

// positionOf is the `position` a write's answer carries, in the form every
// read grammar takes back as `min_position`: over a wire (the dashboard, REST,
// the operator MCP) it is all a caller can wait for. Nil for an unknown
// outcome, never "@0:0", which parses and would be handed back as a floor
// nothing waits for.
func positionOf(at statelog.Position) any {
	if at.IsZero() {
		return nil
	}
	return at.String()
}

// positionText is [positionOf] for a typed answer: the position as the string
// every write tool states it in (`<stream>@<generation>:<sequence>`), "" for
// none. The act transport reads it as that string (operator.receiptOf) and
// lifts it as the dashboard's read floor; an object there is unreadable.
func positionText(at statelog.Position) string {
	if at.IsZero() {
		return ""
	}
	return at.String()
}

// seatReadLevel is what every read on this surface uses, and [seatRead] is
// the same decision in the shape the point readers take.
//
// A name, never a per-site literal: `session` at each site looked plausible,
// but with no high-water mark populated it served the committed prefix as a
// stale read wearing a stronger name. A seat's reads DECIDE things and it has
// no screen to notice staleness on. The operator MCP serves these same tools
// at the same level, for the same reason; neither surface lets the caller
// choose ([statelog.SettableLevels]).
var seatReadLevel = statelog.DefaultReadLevel(statelog.SurfaceSeat)

// seatRead is [seatReadLevel] for the readers that take a whole freshness.
//
// No bound and no floor: `linearizable` establishes the log's end itself, and
// a wake's position is already in the node's floors from the turn's start
// (the engine's runTurn), which the router carries to whichever holder
// answers. The ranked searches carry no floor: an index has no log position.
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
