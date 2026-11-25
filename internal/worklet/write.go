package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The write paths all follow one sentence: take ONE snapshot of your own rows,
// decide and form the expectation inside it, publish, let the broker
// arbitrate, never guess. The framework owns the loop (the snapshot, the
// expectation, the rounds, resolving an ambiguous append); a write path
// contributes a DECISION FUNCTION: given the rows in this transaction, the
// record to publish or what to tell the caller instead.
//
// A write is `applied`, `pending` (committed on the log, not yet consumed
// here) or `unknown` (the acknowledgement was lost), never a bool: folding
// unknown into "failed" retries a write that landed, and into "succeeded"
// reports work never recorded.

// ErrStaleVersion reports an update conditioned on a version that has moved —
// a task's, or a file's.
//
// ITS OWN SENTINEL because nothing is wrong with the request: somebody got
// there first, and the caller re-reads and decides again rather than retrying
// the same patch.
var ErrStaleVersion = errors.New("worklet: it has changed since it was read")

// ErrReassignmentBudget reports a hand-off past [ReassignmentBudget].
var ErrReassignmentBudget = errors.New("worklet: this task has been handed on too many times")

// ErrForbidden reports a write this ACTOR may not make, whatever it carries:
// somebody else's inbox, pins or priority list, a view protected for its owner,
// a project's policy or tag vocabulary without the lead's authority or an
// admin's, which overrides it.
//
// ITS OWN SENTINEL, not [statelog.ErrConflict]: a conflict is a lost race a
// re-read may land, while a refusal of authority lands the same however often
// it is re-read, so the caller is told whom to ask, never to retry. The
// sentence names who may.
var ErrForbidden = errors.New("worklet: not this actor's to write")

// ErrInvalid marks a write the tracker refuses on its CONTENT: a missing or
// malformed argument, a value a field does not take or past a limit, or an
// argument naming ANOTHER object that cannot take part (a parent or blocker
// in the trash, a blocker at its dependents' cap, an archived type or tag, a
// destination that takes nothing). A different request can land. What the
// object the write is ABOUT refuses by its state is [ErrState].
//
// A MARK RATHER THAN A DEFAULT: a node's failure arrives unwrapped from places
// a writer does not own, and read as "invalid" would tell a person to fix their
// input over a disk error. Unmarked means the node failed, as for
// [doclet.ErrInvalid]. The mark leaves the sentence, prompt text a model was
// tuned against, unchanged ([invalid]).
var ErrInvalid = errors.New("worklet: invalid")

// ErrState marks a write the OBJECT IT IS ABOUT refuses by its state, until
// somebody changes it: a project archived before work is filed into it, a task
// frozen in the trash that a write would place, move or merge, a project's
// tags, a task's watchers or checklists, or a person's pins and stars at their
// cap. The same request lands once whoever the sentence names reopens,
// restores or takes something off the set, and not before.
//
// ITS OWN MARK beside [ErrInvalid] because the two send a caller to opposite
// places (internal/mcp's Refusal): told `invalid`, a seat sends another
// argument an object will refuse, and a screen shows the sentence where it
// should redraw the object. The sentence names the state and who changes it.
var ErrState = errors.New("worklet: its state refuses this")

// ErrTaken marks a write the tracker refuses because what it would give an
// object — a tag's label, a checklist's or a line's id — is ANOTHER object's
// already: a COLLISION. The same request lands once that object gives it up,
// and a different value lands now; the sentence names the holder, so a caller
// uses it if it means the same thing or chooses its own.
//
// ITS OWN MARK beside [ErrInvalid] and [ErrState] for internal/mcp's Refusal
// classes: a collision is `exists`, as a page title, a room name or an
// artifact's label another object holds is, never `invalid` as if the value
// were malformed.
var ErrTaken = errors.New("worklet: another object already holds that")

// ErrAlreadyAnswered reports an `answers` naming a question somebody already
// answered. Its own sentinel because nothing about the comment is wrong and
// nothing a caller could change makes it land: the answer it meant to give
// exists, and the useful move is to read it.
var ErrAlreadyAnswered = errors.New("worklet: that question is already answered")

// Refusal is a write this package refuses on what was asked or on who asked
// it: the writer's own SENTENCE ([Refusal.Err]), and the sentinel that classes
// it ([Refusal.Mark]) beside that sentence's own %w chain.
//
// THE SENTENCE IS HELD WITHOUT THE PACKAGE PREFIX, which [Refusal.Error] adds
// for a log; [Sentence] gives a model or a person the words alone (ADR-0060).
//
// EXPORTED, FIELDS AND ALL: the estate router's wire (internal/estate)
// rebuilds an error type from its exported fields alone, and [Sentence] on the
// asking node must still find the refusal. Only this package builds one
// ([invalid], [refusedByState], [collision], [forbidden], [forbiddenAs],
// [invalidSaying], [forbiddenSaying]).
type Refusal struct {
	// Mark is the sentinel that classes the refusal: [ErrInvalid],
	// [ErrState], [ErrTaken] or [ErrForbidden].
	Mark error
	// Also is a sentinel of the refusal's own beside its class, or nil —
	// see [forbiddenAs].
	Also error
	// Err is the sentence, without the package prefix, and its %w chain.
	Err error
	// said is the refusal in a person's words, or nil where nobody has
	// worded it for one yet ([invalidSaying], ADR-0060).
	//
	// UNEXPORTED, and the wire loses nothing: the words cross as the
	// [caveat.Said] Unwrap yields, which the estate router carries itself and
	// rebuilds through [caveat.SaidFromPeer]. Exported, it would be rebuilt as
	// plain JSON, which a phrasing never decodes from.
	said *caveat.Said
}

func (e *Refusal) Error() string { return "worklet: " + e.Err.Error() }
func (e *Refusal) Unwrap() []error {
	out := []error{e.Mark}
	if e.Also != nil {
		out = append(out, e.Also)
	}
	out = append(out, e.Err)
	if e.said != nil {
		out = append(out, e.said)
	}
	return out
}
func (e *Refusal) sentence() string { return e.Err.Error() }

// sentenced is a refusal this package wrote, whatever its type: [Refusal]
// itself, and the typed refusals a caller several steps away has to take
// apart ([TagClash], [TagsFull]). Each holds its sentence without the prefix
// its Error adds.
type sentenced interface {
	error
	sentence() string
}

// invalid is fmt.Errorf for a content refusal: the sentence — written WITHOUT
// the package prefix, which [Refusal.Error] adds — and its %w chain, marked
// [ErrInvalid].
func invalid(format string, args ...any) error {
	return &Refusal{Mark: ErrInvalid, Err: fmt.Errorf(format, args...)}
}

// collision is [invalid] for a collision, marked [ErrTaken]. The sentence names
// the object that holds the value.
func collision(format string, args ...any) error {
	return &Refusal{Mark: ErrTaken, Err: fmt.Errorf(format, args...)}
}

// refusedByState is [invalid] for a write the object it is about refuses by
// its state, marked [ErrState]. The sentence names the state and who changes
// it.
func refusedByState(format string, args ...any) error {
	return &Refusal{Mark: ErrState, Err: fmt.Errorf(format, args...)}
}

// inTheTrash is the refusal of a gesture on a task in the trash — the task's
// state, which a restore changes — naming who put it there and what to do
// before the gesture ("moving it"), or before anything where before is empty.
// Written for a person beside the task, so it names neither its id nor an
// instant.
func inTheTrash(task Task, before string) error {
	restore := "Restore it first"
	if before != "" {
		restore = "Restore it before " + before
	}
	if task.Removed == nil || task.Removed.By == "" {
		return refusedByState("this task is in the trash. %s", restore)
	}
	return refusedByState("this task is in the trash, put there by %s. %s",
		task.Removed.By, restore)
}

// unarchiveProject is who changes an archived project, said wherever a gesture
// meets one: the archive's own relation (authz's work.project.archive), since
// most people reading the refusal may not unarchive it themselves.
const unarchiveProject = "Its lead or an administrator can unarchive it"

// nameOf is how a refusal names a task other than the one it is about: its
// key, or its id where it has none yet.
func nameOf(task Task) string {
	if task.Key != "" {
		return task.Key
	}
	return task.ID
}

// forbidden is [invalid] for a write that is not this actor's to make, marked
// [ErrForbidden]. The sentence names who may.
func forbidden(format string, args ...any) error {
	return &Refusal{Mark: ErrForbidden, Err: fmt.Errorf(format, args...)}
}

// invalidSaying is [invalid] with the refusal in a person's words beside the
// model's sentence: the tool answering it lifts the phrasing onto its result,
// and the act route answers a person with it (ADR-0060).
func invalidSaying(person caveat.Refusal, format string, args ...any) error {
	return &Refusal{Mark: ErrInvalid, Err: fmt.Errorf(format, args...),
		said: &caveat.Said{Words: person}}
}

// forbiddenSaying is [forbidden] with the refusal in a person's words, as
// [invalidSaying] is [invalid].
func forbiddenSaying(person caveat.Refusal, format string, args ...any) error {
	return &Refusal{Mark: ErrForbidden, Err: fmt.Errorf(format, args...),
		said: &caveat.Said{Words: person}}
}

// forbiddenAs is [forbidden] carrying a sentinel of its own BESIDE the class,
// for a caller that acts on this refusal in particular. Wrapped into the
// sentence with %w instead, the sentinel's words would be read to a person
// after a sentence that already said them.
func forbiddenAs(sentinel error, format string, args ...any) error {
	return &Refusal{Mark: ErrForbidden, Also: sentinel, Err: fmt.Errorf(format, args...)}
}

// Sentence is a refusal this package wrote, as a person reads it: the
// writer's own sentence, without the package prefix a Go error carries.
//
// Any other error this package wrote ("worklet: no such task: ENG-42") loses
// the prefix and keeps every word after it; one a caller wrapped in context of
// its own is its whole message, because context is never dropped to tidy a
// prefix away. The framework's own three (a lost race, a create of what is
// there, a write a deletion keeps deleted) are told in [statelog.Sentence]'s
// words, so no caller prints the subject the framework met.
func Sentence(err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	var r sentenced
	if errors.As(err, &r) && r.Error() == err.Error() {
		return r.sentence()
	}
	return strings.TrimPrefix(err.Error(), "worklet: ")
}

// NoIfMatch omits an update's version precondition, which MERGES the patch
// onto whatever the task currently is. Named because a literal 0 in a
// seven-argument call says nothing about which behaviour it selects.
const NoIfMatch uint64 = 0

// ReassignmentBudget is how many times AGENTS may hand one task on before the
// engine refuses and the item needs a person.
//
// ON THE ITEM, not the delegation depth: an assignment is one hop on a chart a
// founder authored, and the failure is two seats each sure the other owns it,
// a loop that is a property of the item. Eight, because the deepest chart
// designed for is four tiers, so a legitimate escalation up and back down is
// seven hops at worst, plus one so the honest worst case is not the refusal.
// A human or operator touch resets it ([Writer.chargeHandOff]).
const ReassignmentBudget = 8

// Writer is the tracker's write authority.
//
// Every write takes the OPERATION ID it publishes under from its caller,
// minted with [statelog.NewOpID] or derived with [statelog.DeriveOpID] when a
// retry must reproduce it. No instant is stamped beside it: the state log
// reads the id's mint instant to decide whether this node's ledger can vouch
// for a retry ([statelog.OpMintedAt]), and the writer's clock at the call is
// always later than the mint on exactly that retry.
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read by the sequences that need to see a
	// subtree BEFORE their first append. Never the write path's own snapshot
	// (the framework's, per append), so nothing decided from it is paired with
	// an expectation.
	db store.ReplicatedReader

	// claims is where a walking sequence takes its claim, and nodeID the prefix
	// of every claim's owner, never the owner ([Writer.claimOwner]). Either may
	// be unset on a writer making only single-append writes; a sequence that
	// needs one refuses by name.
	claims Claims
	nodeID string

	// Actor and ActorKind are who this writer acts as, and OperatorID, TurnID
	// and Chain the provenance that travels with it. ON THE WRITER, never per
	// call: a tracker whose author is an argument is no audit trail. A surface
	// serving many parties takes one writer each through [Writer.As].
	Actor      string
	ActorKind  AuthorKind
	OperatorID string
	TurnID     string
	Chain      []string

	// ActorApps are the apps a credential acting as no seat used when this
	// node resolved it, for a writer acting as one ([Provenance.Apps]), and
	// nil for every other actor.
	ActorApps []iam.Grant

	// writtenFor is whom this writer's records are written for
	// ([Provenance.WrittenFor]), set by [Writer.As] alone and asked of the
	// actor's kind before every record is built ([Writer.decide]): a writer
	// acting as an agent that states nobody writes nothing.
	writtenFor types.WrittenFor

	// Origin is where the turn behind this writer was woken — see
	// [Provenance.Origin] — and what every task it creates states.
	Origin *Origin

	// Leads resolves a project's or a unit's lead, the ONE thing this writer
	// needs that the rows cannot give: a lead is chart-owned, and the applier
	// may not read an org, since two nodes on different epochs would write
	// different rows. ON THE WRITER, unlike [Wake.Notify], because its caller is
	// a fleet singleton on a tick with no arguments to carry a seam. Nil
	// resolves to no lead, costing a wake its fallback recipient, never its
	// delivery.
	Leads Leads

	// World is the two lookups the custom-field coercion table cannot do
	// itself: a relationship's task and a people field's seat. On the writer
	// for [Writer.Leads]'s reason. Nil refuses those two field types BY NAME,
	// because a handle nothing checked filters against nobody.
	World FieldWorld

	// Now is the clock the AUTHORED instants are stamped from: a field, so a
	// test can pin it and nothing on the write path reads a clock the applier
	// is forbidden.
	Now func() time.Time

	// Zone is the company's clock (ADR-0018), which a value that holds a DAY is
	// cut in: an instant given for a date field or a project's target date is
	// stored as the date it falls on HERE ([coerceDay]). READ PER CALL, because
	// the zone is the epoch's and one captured at boot would cut every later day
	// on an old clock. Nil is UTC.
	Zone func() *time.Location

	// after is one of this writer's OWN earlier writes, which the framework
	// waits for this node's applier to reach before the next snapshot. Zero,
	// every write a surface makes on its own, waits for nothing ([Writer.After]).
	after statelog.Position

	// written is [Provenance.Written], carried per party like the rest of
	// the provenance. See [Writer.publish] for what reaches it.
	written WriteLog

	// refusal is set by [Writer.As] when its identity cannot author a record,
	// and checked at the one funnel every write passes through ([Writer.publish]).
	refusal error
}

// WriterDeps is everything a writer needs that it does not own.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader
	Claims    Claims
	NodeID    string

	// World is the chart seam the custom-field coercion needs for the one
	// field type whose value is a colleague — see [Writer.World].
	World FieldWorld

	Actor     string
	ActorKind AuthorKind
	Leads     Leads
	Now       func() time.Time

	// Zone is the company's clock — see [Writer.Zone].
	Zone func() *time.Location
}

// As is this writer acting as somebody else, and it is the ONLY way the actor
// ever changes.
//
// A COPY RATHER THAN AN ARGUMENT, because a writer acts as exactly one party: a
// history row whose author the caller chose is no audit trail. A surface
// serving many parties (a seat's tool loop, an operator's MCP session) takes
// one writer per party, derived from its own IMMUTABLE identity, the seat
// bound into the turn context or the request's credential, which no model or
// request body can set. The shallow copy shares the publisher, store and
// claims, so a per-call writer costs nothing.
func (w *Writer) As(actor string, kind AuthorKind, provenance Provenance) *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	if actor == "" || !kind.Valid() {
		// THE SAME REFUSAL [NewWriter] MAKES, for the other door into the same
		// state. CARRIED rather than returned: the caller has no other identity to
		// fall back to, and the one funnel every write passes through surfaces it
		// at the call that would have written.
		clone.refusal = fmt.Errorf("worklet: a writer cannot act as %q of "+
			"kind %q — every record carries who wrote it", actor, kind)
	}
	clone.Actor = actor
	clone.ActorKind = kind
	// AN OPERATOR'S APPS ALONE: a seat's or a person's record names the seat,
	// and apps on it would stamp the record at a version older builds retain
	// for a field nothing reads.
	clone.ActorApps = nil
	if kind == AuthorOperator {
		clone.ActorApps = slices.Clone(provenance.Apps)
	}
	// AND WHOM THE PARTY WRITES FOR, as stated: refused at the write
	// ([Writer.decide]) where an agent states nobody, never mended here into
	// a carrier nobody composed.
	clone.writtenFor = provenance.WrittenFor
	clone.OperatorID = provenance.OperatorID
	clone.TurnID = provenance.TurnID
	clone.Chain = provenance.Chain
	clone.written = provenance.Written
	clone.Origin = provenance.Origin
	return &clone
}

// recordedApps are the apps a record carries beside its actor
// ([MutationRecord.ActorApps]): the writer's own on a record a later turn reads
// ([Writer.carries]), nothing on any other, which stays at the version every
// build reads. Asked by [Writer.recordedFor]'s rule.
func (w *Writer) recordedApps(carries bool) []iam.Grant {
	if !carries {
		return nil
	}
	return slices.Clone(w.ActorApps)
}

// recordedFor is whom a record was written for ([MutationRecord.WorkingFor]),
// or why this writer may write none: an agent's carrier on a record a later
// turn reads ([Writer.carries]), nobody on any other, and a refusal for an
// agent's writer that states nobody or any other writer carrying somebody
// ([types.WrittenFor.For]).
//
// ASKED OF EVERY RECORD, carrying or not: the rule is the actor's, and refused
// only where a record carried it would pass every test that wakes nobody.
func (w *Writer) recordedFor(carries bool) (*types.WorkingFor, error) {
	carrier, err := w.writtenFor.For(iam.ActorKind(w.ActorKind))
	if err != nil {
		return nil, fmt.Errorf("worklet: a writer acting as %q: %w", w.Actor, err)
	}
	if !carries {
		return nil, nil
	}
	return carrier, nil
}

// carries reports whether a record is one a later turn reads to learn whom it
// works for (ADR-0109), so it carries the writer's carrier and apps:
//
//   - AN ITEM'S CREATION ([ChangeCreated], never a move's alias for its old
//     key) AND EVERY MOVE OF ITS ASSIGNEE, whatever they wake now: the work
//     pickup reads those rows (ADR-0112). A move is a patch whose assignee
//     differs from the current row, whatever its kind (assigns, which only the
//     decide holding that row can tell); a reassertion is none;
//   - ANY OTHER RECORD only where its Notify may wake an agent other than the
//     actor ([mayWakeAnAgent]).
//
// NEVER A RECORD EVERY BUILD MUST READ ([RecordEnvelope.readByEveryBuild]).
func (w *Writer) carries(subject Subject, op OpKind, kind ChangeKind, assigns bool,
	notify *Notify) bool {

	switch {
	case RecordEnvelope{Subject: subject, Op: op}.readByEveryBuild():
		return false
	case op == OpCreate && kind == ChangeCreated, assigns:
		return true
	}
	return mayWakeAnAgent(notify, w.Actor)
}

// origin is the origin a create states: the writer's own, and none where the
// writer names no surface — a conversation with no surface names nothing a
// reader can render.
func (w *Writer) origin() *Origin {
	if w.Origin == nil || strings.TrimSpace(w.Origin.Surface) == "" {
		return nil
	}
	origin := *w.Origin
	return &origin
}

// After is this writer carrying one of its own earlier writes, so a GESTURE
// that touches one subject twice waits once instead of wasting a round: the
// later append would decide from a snapshot below the earlier one, be refused
// by the broker, and only then wait for this position. Its
// [statelog.Request.Session] moves the wait ahead: free when the applier is
// caught up, and an expiry names the caller's own write (the session-wait
// histogram), not a colleague's edit.
//
// A COPY, never accumulated nor turn-wide: the mark is one gesture's, as a
// remembered position would stall every later write and a turn-wide one is
// state shared across goroutines that makes unrelated appends wait. Between
// tool calls the tool layer's settle covers it. Zero is a no-op.
func (w *Writer) After(at statelog.Position) *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	clone.after = at
	return &clone
}

// Provenance is what an audit walks from a record back to what produced it.
//
// It BOUNDS NOTHING. A hand-off is charged to the task's own reassignment
// counter and not to the delegation depth, so the chain here is a trail rather
// than a budget.
type Provenance struct {
	// OperatorID is the credential a person's own write was made under,
	// recorded beside the actor rather than instead of it.
	OperatorID string

	// TurnID is the turn that produced this write, and Chain the
	// delegation path that reached it.
	TurnID string
	Chain  []string

	// Origin is the chat surface and conversation the turn behind this
	// writer was woken on, stated on every task it creates — see [Origin].
	// Nil for a writer no chat message caused.
	Origin *Origin

	// Apps are the apps the credential behind a writer acting as no seat
	// ([AuthorOperator]) used when this node resolved it (iam.ActorApps),
	// carried on every record a later turn reads ([MutationRecord.ActorApps],
	// [Writer.carries]) so that turn uses no app the credential did not
	// (ADR-0109). An operator's alone ([Writer.As]). Sent only where set,
	// inside the estate's signed envelope (internal/estate's Actor).
	Apps []iam.Grant `json:",omitempty"`

	// WrittenFor is whom the writer's records are written for (ADR-0109): for
	// an [AuthorAgent] writer, whoever its turn works for, carried on every
	// record a later turn reads ([MutationRecord.WorkingFor], [Writer.carries]),
	// never the writing seat. AN AGENT'S WRITER THAT STATES NOTHING WRITES
	// NOTHING ([Writer.recordedFor]); every other author carries nobody
	// ([types.WrittenFor]).
	//
	// internal/estate refuses such a write before asking any node, and states
	// it on arrival from the actor the provenance crosses inside, WHATEVER
	// ARRIVED: an older build sends an empty one as no member, which a decoder
	// of the provenance alone would never run for.
	WrittenFor types.WrittenFor `json:",omitzero"`

	// Written is where the tasks this writer's records COMMIT to are reported,
	// so the engine can charge a turn nothing at dispatch named an item for to
	// the one item it wrote. Nil reports nothing. IN-PROCESS ONLY: a write
	// crossing nodes reports what it committed in its answer, and the asking
	// node adds it here (internal/estate's reply.Written).
	Written WriteLog `json:"-"`
}

// WriteLog is what a turn's writer reports the tasks it committed to into,
// declared by its one caller and kept to what it calls; the set is the
// turn's (internal/agent/turnctx).
type WriteLog interface {
	Add(types.WorkItem)
	// Commit records the kind of change a committed write was, which the
	// turn's outcome is read from.
	Commit(types.CommittedChange)
}

// NewWriter builds the tracker's write authority.
func NewWriter(d WriterDeps) (*Writer, error) {
	switch {
	case d.Publisher == nil:
		return nil, fmt.Errorf("worklet: a writer has no publisher")
	case d.Actor == "":
		return nil, fmt.Errorf("worklet: a writer has no actor; every record " +
			"carries who wrote it, and one that does not is a history row " +
			"nobody can attribute")
	case !d.ActorKind.Valid():
		return nil, fmt.Errorf("worklet: %q is not an author kind", d.ActorKind)
	}
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Writer{
		publisher: d.Publisher, db: d.DB, claims: d.Claims, nodeID: d.NodeID,
		Actor: d.Actor, ActorKind: d.ActorKind, Leads: d.Leads, World: d.World,
		Now: now, Zone: d.Zone,
	}, nil
}

// zone is the company's clock as this call reads it, or UTC.
func (w *Writer) zone() *time.Location {
	if w.Zone == nil {
		return time.UTC
	}
	if loc := w.Zone(); loc != nil {
		return loc
	}
	return time.UTC
}

// UpdateTask changes one, ARBITRATED AGAINST THE TASK'S OWN LAST RECORD: two
// writers on one task contend at the broker, two on different tasks never do.
//
// ifMatch is the caller's precondition (zero MERGES), a PARAMETER and not a
// [TaskPatch] field because a patch is the record every applier replays, where
// a precondition the broker already arbitrated could only diverge; it is
// checked inside the decide snapshot. [ReassignmentBudget] is charged there
// too, travelling as a value: a counter each node derived would disagree.
//
// A comment under an edit, resolve or removal kind is refused here: those are
// formed from the stored row inside the decide, under the author rule, by
// [Writer.EditComment], [Writer.ResolveComment] and [Writer.RemoveComment].
func (w *Writer) UpdateTask(ctx context.Context, opID, id, project string,
	ifMatch uint64, patch TaskPatch, kind ChangeKind,
	notify *Notify) (WriteResult, error) {

	if patch.Comment != nil && kind == ChangeCommentRemoved {
		return WriteResult{}, invalid("a removal of comment %s on task %s is "+
			"not a patch: a remark is taken down through RemoveComment, which "+
			"forms the removal from the stored row and refuses anybody but its "+
			"author or a moderator", patch.Comment.ID, id)
	}
	if patch.Comment != nil && kind == ChangeCommentEdited {
		return WriteResult{}, invalid("an edit of comment %s on task %s is not "+
			"a patch: a remark is rewritten through EditComment, which forms "+
			"the edit from the stored row and refuses anybody but its author",
			patch.Comment.ID, id)
	}
	if patch.Comment != nil && kind == ChangeCommentResolved {
		return WriteResult{}, invalid("a resolve of comment %s on task %s is not "+
			"a patch: a remark is resolved through ResolveComment, which forms "+
			"the resolve from the stored row and refuses anybody but its author",
			patch.Comment.ID, id)
	}
	if err := patch.CheckDates(); err != nil {
		return WriteResult{}, err
	}
	return w.updateTask(ctx, opID, id, project, ifMatch, patch, kind, notify, nil)
}

// amendment settles a patch against the task as the decide snapshot holds it,
// or refuses the write: the one seam into [Writer.updateTask]'s decide, for a
// patch that cannot be formed until the CURRENT row is in hand (a comment edit
// is the stored comment with one field changed, and its author is a fact of
// that row).
type amendment func(ctx context.Context, tx *sql.Tx, current Task,
	patch TaskPatch) (TaskPatch, error)

// ErrNotAuthor reports an edit or a resolve of a remark by somebody who did not
// write it, or a removal by somebody who neither wrote it nor moderates.
var ErrNotAuthor = errors.New("worklet: only its author may edit, resolve or remove a comment")

// EditComment rewrites one comment's body, AS ITS AUTHOR.
//
// ONLY THE AUTHOR, whatever else the caller holds: the authority table admits
// an admin too, since it answers removal and edit alike, but a remark somebody
// else rewrote is attributed to a person who never made it, on a record that
// outlives the thread (as internal/doclet rules). Taking it down is visible;
// rewriting it is not.
//
// DECIDED INSIDE THE SNAPSHOT: the author and whether the remark is still there
// are read where the expectation is formed, so a racing edit or removal
// contends at the broker on the task's subject.
func (w *Writer) EditComment(ctx context.Context, opID, taskID, project,
	commentID, body string, notify *Notify) (WriteResult, error) {

	body = strings.TrimSpace(body)
	switch {
	case strings.TrimSpace(commentID) == "":
		return WriteResult{}, invalid("an edit names no comment")
	case body == "":
		return WriteResult{}, invalid("an edit leaves comment %s "+
			"with no body — remove it instead", commentID)
	}
	at := w.Now()
	return w.updateTask(ctx, opID, taskID, project, NoIfMatch,
		TaskPatch{Comment: &Comment{ID: commentID, Body: body}},
		ChangeCommentEdited, notify,
		func(ctx context.Context, tx *sql.Tx, _ Task,
			patch TaskPatch) (TaskPatch, error) {

			held, err := readComment(ctx, tx, taskID, commentID)
			if err != nil {
				return TaskPatch{}, err
			}
			stored := held[0]
			switch {
			case stored.Removed:
				return TaskPatch{}, fmt.Errorf("%w: comment %s was removed, "+
					"and a removed remark has no text left to edit",
					ErrNoComment, commentID)
			case stored.Author != w.Actor:
				return TaskPatch{}, fmt.Errorf("%w: comment %s was written by "+
					"%s — reply to it instead of rewriting it", ErrNotAuthor,
					commentID, stored.Author)
			}
			// THE STORED COMMENT WITH ITS BODY REPLACED, whole, so the record (the
			// history row and the wake quote it) states the remark as it now reads.
			// The apply rewrites only the words ([editComment]).
			edited := stored
			edited.Body, edited.UpdatedAt = patch.Comment.Body, at
			// An answer that chose still says what it chose once rewritten.
			if edited.Choice != "" && edited.Answers != nil {
				ask, err := readAsk(ctx, tx, taskID, *edited.Answers)
				if err != nil {
					return TaskPatch{}, err
				}
				if err := checkStatesChoice(taskID, ask.id, ask.decision, &edited); err != nil {
					return TaskPatch{}, err
				}
			}
			patch.Comment = &edited
			return patch, nil
		})
}

// ResolveComment marks one comment resolved, AS ITS AUTHOR: who resolved it is
// this writer, and when is its clock.
//
// ONLY THE AUTHOR, whatever grants the caller holds: a resolution says the
// remark was dealt with, which only whoever made it can say. A question's
// recipient answers it instead, which closes it too and says what was decided.
// Decided inside the snapshot, as [Writer.EditComment] is. A remark already
// resolved is resolved again, the row holding the newest ([resolveComment]).
// A non-nil withdrawal resolves an open question as its asker withdrawing it,
// with the reason posted in its thread ([Withdrawal]), checked in the same
// snapshot ([checkWithdrawal]).
func (w *Writer) ResolveComment(ctx context.Context, opID, taskID, project,
	commentID string, withdrawal *Withdrawal, notify *Notify) (WriteResult, error) {

	if strings.TrimSpace(commentID) == "" {
		return WriteResult{}, invalid("a resolve names no comment")
	}
	at := w.Now()
	return w.updateTask(ctx, opID, taskID, project, NoIfMatch,
		TaskPatch{Comment: &Comment{ID: commentID}},
		ChangeCommentResolved, notify,
		func(ctx context.Context, tx *sql.Tx, _ Task,
			patch TaskPatch) (TaskPatch, error) {

			held, err := readComment(ctx, tx, taskID, commentID)
			if err != nil {
				return TaskPatch{}, err
			}
			stored := held[0]
			switch {
			case stored.Removed:
				return TaskPatch{}, fmt.Errorf("%w: comment %s was removed, "+
					"and a removed remark has nothing left to resolve",
					ErrNoComment, commentID)
			case stored.Author != w.Actor:
				return TaskPatch{}, fmt.Errorf("%w: comment %s was written by "+
					"%s — reply to it instead of resolving it", ErrNotAuthor,
					commentID, stored.Author)
			}
			// THE STORED COMMENT WITH ITS RESOLUTION SET, whole, for an edit's
			// reason; the apply writes the resolution alone ([resolveComment]).
			if withdrawal != nil {
				if err := checkWithdrawal(ctx, tx, taskID, stored, withdrawal); err != nil {
					return TaskPatch{}, err
				}
			}
			resolved := stored
			resolved.Resolved, resolved.ResolvedBy, resolved.ResolvedAt = true, w.Actor, &at
			if withdrawal != nil {
				resolved.Withdrawal = withdrawal
			}
			patch.Comment = &resolved
			return patch, nil
		})
}

// CommentAuthority is what a caller may do to a remark somebody else wrote.
//
// A VALUE RATHER THAN A BOOL for [PersonAuthority]'s reason, stating the
// DECISION the caller got from the authority table: this package holds no
// chart and no grants.
type CommentAuthority struct {
	// Moderate reports a caller admitted to take down a remark they did not
	// write (admin alone). There is no edit equivalent: a removal is visible,
	// and an edit would put words in somebody's mouth.
	Moderate bool
}

// RemoveComment takes one remark down: its row stays, flagged and with its
// words blanked, so the replies under it still resolve against something.
//
// Its author, or an admin moderating ([CommentAuthority]), read against the
// row inside the snapshot as [Writer.EditComment] reads it. NEVER A REMARK
// THAT ASKS OR ANSWERS: a question closes by its asker withdrawing it or its
// recipient answering, and an answer taken down leaves its question closed
// with none, so each is refused naming the gesture meant (withdraw, or edit).
//
// QUIET: a wake would quote what was just taken down. The removal's history
// row names who took it down, and every surface that quoted the remark stops
// ([removeComment]).
func (w *Writer) RemoveComment(ctx context.Context, opID, taskID, project,
	commentID string, authority CommentAuthority) (WriteResult, error) {

	if strings.TrimSpace(commentID) == "" {
		return WriteResult{}, invalid("a removal names no comment")
	}
	at := w.Now()
	return w.updateTask(ctx, opID, taskID, project, NoIfMatch,
		TaskPatch{Comment: &Comment{ID: commentID, Removed: true}},
		ChangeCommentRemoved, nil,
		func(ctx context.Context, tx *sql.Tx, _ Task,
			patch TaskPatch) (TaskPatch, error) {

			held, err := readComment(ctx, tx, taskID, commentID)
			if err != nil {
				return TaskPatch{}, err
			}
			stored := held[0]
			switch {
			case stored.Removed:
				return TaskPatch{}, fmt.Errorf("%w: comment %s was already removed",
					ErrNoComment, commentID)
			case !authority.Moderate && stored.Author != w.Actor:
				return TaskPatch{}, fmt.Errorf("%w: comment %s was written by "+
					"%s, and a remark is taken down by whoever made it and by "+
					"nobody else but an admin", ErrNotAuthor, commentID, stored.Author)
			case stored.Ask != "":
				return TaskPatch{}, invalid("comment %s asks %s a question, and a "+
					"question is not taken down: its asker withdraws it, saying why, "+
					"or %s answers it", commentID, stored.Ask, stored.Ask)
			case stored.Answers != nil && *stored.Answers != "":
				return TaskPatch{}, invalid("comment %s answers a question, and an "+
					"answer is not taken down: the question it closed would be left "+
					"with no answer. Edit its words instead", commentID)
			}
			// THE STORED COMMENT WITH ITS WORDS TAKEN DOWN, whole, for an edit's
			// reason; the apply blanks and flags the row ([removeComment]).
			removed := stored
			removed.Body, removed.Removed, removed.UpdatedAt = "", true, at
			patch.Comment = &removed
			return patch, nil
		})
}

// updateTask is [Writer.UpdateTask] with an optional [amendment].
func (w *Writer) updateTask(ctx context.Context, opID, id, project string,
	ifMatch uint64, patch TaskPatch, kind ChangeKind,
	notify *Notify, amend amendment) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, invalid("an update names no task")
	case project == "":
		return WriteResult{}, invalid("an update on task %s "+
			"names no project — the caller resolved a key to reach this task "+
			"and therefore holds one", id)
	}
	// THE COMMENT'S BODY IS CHECKED HERE: a comment rides a task write, so this
	// is the one place every comment passes through.
	var commentBody *string
	if patch.Comment != nil {
		commentBody = &patch.Comment.Body
	}
	if err := checkTextCaps(patch.Title, patch.Body, commentBody); err != nil {
		return WriteResult{}, err
	}
	if patch.Comment != nil {
		if err := checkCommentShape(id, patch.Comment); err != nil {
			return WriteResult{}, err
		}
		// A NEW POST SAYS SOMETHING: an edit, a resolve and a removal are formed
		// from the stored row in their decide, and only a removal blanks it.
		if postsComment(patch, kind) && strings.TrimSpace(patch.Comment.Body) == "" {
			return WriteResult{}, invalid("comment %s on task %s says nothing: a "+
				"comment carries words, and an answer that chooses states its "+
				"choice in them (ChoiceWords)", patch.Comment.ID, id)
		}
	}
	if patch.FormerKeys != nil {
		// THE DECIDE FORMS THEM from its own snapshot on a re-key
		// ([formerKeysAfterRekey]): carried whole, a caller's list would replace
		// former keys the task gained since.
		return WriteResult{}, invalid("a patch on task %s names its former "+
			"keys, and nothing writes those but the move that re-keys it — "+
			"the key a task leaves is read where the move decides", id)
	}
	if patch.Tags != nil {
		// NORMALISED BEFORE THE PUBLISH, so the record carries the rows' spelling
		// and no data node re-derives the slug, a second implementation of the
		// grammar.
		tags, err := normaliseTags(project, *patch.Tags)
		if err != nil {
			return WriteResult{}, err
		}
		patch.Tags = &tags
	}
	subject := TaskSubject(id)
	// A PROJECT MOVE STATES BOTH CONTAINERS: the task's rows leave one project's
	// closure for another's, and naming only the destination would let a write
	// into the one it left slip past a deferral covering it.
	scope := ScopeSet{Subject: true, Container: project}
	if patch.Project != nil && *patch.Project != project {
		scope = ScopeSet{Terms: []ScopeTerm{
			{Kind: TermObject, Container: project, ID: id},
			{Kind: TermObject, Container: *patch.Project, ID: id},
		}}
	}
	// EVERY PATCH, not only a status one: every task apply rewrites the rows
	// naming this task as a blocker (`maintainDeps` from `explodeTask`), so any
	// patch's scope names the dependents. Gated on status, an edit to a task
	// somebody waits on failed [ScopeSet.covers] on every re-run.
	var err error
	if scope, err = w.scopeForDependents(ctx, id, scope); err != nil {
		return WriteResult{}, err
	}
	// A RE-KEY (a patch carrying a mint) names every key the task held and the
	// one it takes: its apply claims the new key and turns the old one former,
	// which changes who a purge hands it to ([keyTerm]).
	if patch.Mint != nil {
		if scope, err = w.scopeForRekey(ctx, id, project, patch, scope); err != nil {
			return WriteResult{}, err
		}
	}
	// AND THE SUM STAYS UNDER THE CAP, for [ScopeSet.capped]'s reason.
	scope = scope.capped()
	at := w.Now()

	// CAVEATS COME FROM INSIDE THE DECIDE, which reruns each round: the LAST
	// run's record was published, so the slice is replaced, not appended to.
	var fieldCaveats []caveat.Caveat
	// AND SO IS THE REMARK A POST FOUND ALREADY POSTED, which the result
	// answers for ([WriteResult.Posted]): the run that found it is the one
	// that published nothing.
	var posted *PostedRemark
	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			posted = nil
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			current, held, err := readTask(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				// TWO ABSENCES, and only one is worth coming back for —
				// see [missingTask].
				return statelog.Decision{}, missingTask(ctx, tx, id,
					"no change can be made to it")
			}
			if postsComment(patch, kind) {
				// A REMARK ALREADY POSTED IS ANSWERED FROM ITS ROW ([postedComment]),
				// before anything about the item now: the post landed, so what became
				// of the item since is no reason to refuse it.
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				found, err := postedComment(ctx, tx, id, patch.Comment.ID,
					patch.Comment.Author)
				if err != nil {
					return statelog.Decision{}, err
				}
				if found != nil {
					posted = found
					return statelog.Decision{Version: int64(current.Version)}, nil
				}
			}
			// THE PROJECT THE CALLER DECIDED ON IS STILL THIS TASK'S ([stillIn]):
			// the tags, scope and lead checks upstream were formed against it, so a
			// task moved in between is another project's write.
			if moved := stillIn(current, project); moved != nil {
				return statelog.Decision{}, moved
			}
			if current.Removed != nil {
				// A TOMBSTONED TASK IS FROZEN, which keeps a removal a local decision
				// with no walk; asked before any new parent, because it is the answer
				// whatever the parent is.
				return statelog.Decision{}, inTheTrash(current, "")
			}
			if patch.Parent != nil {
				// A TASK A MOVE HAS NOT CARRIED KEEPS ITS PLACE until it has: the walk
				// carries every task it read, so one moved out from under it would
				// still be carried ([refuseMidMove]).
				if err = refuseMidMove(ctx, tx, current, "task",
					"filed elsewhere now it would still be carried"); err != nil {
					return statelog.Decision{}, err
				}
			}
			if patch.Parent != nil && *patch.Parent != "" {
				// A RE-PARENT IS HELD TO THE CREATE'S RULE ([refuseParent]), in the
				// snapshot the expectation is formed in.
				if err = refuseParent(ctx, tx, current, *patch.Parent); err != nil {
					return statelog.Decision{}, err
				}
			}
			if ifMatch != 0 && current.Version != ifMatch {
				return statelog.Decision{}, fmt.Errorf("%w: task %s is at "+
					"version %d and the edit was conditioned on %d — re-read "+
					"it and decide again rather than re-sending this patch",
					ErrStaleVersion, id, current.Version, ifMatch)
			}
			if markOnly(patch) && *patch.Moving == current.Moving {
				// THE MARK ALREADY SAYS IT: a walk's last append whose mark somebody
				// else took down, or a re-run of one that landed. An empty decision
				// succeeds without a history row.
				return statelog.Decision{Version: int64(current.Version)}, nil
			}
			if laneOnly(patch) && *patch.Status == statusOf(current) {
				// THE LANE IT IS ALREADY IN, on a card unchanged since its caller read
				// it: a no-op status commit would still stamp a version, write history
				// and wake the task's people. A drag retried under its own operation
				// lands here safely, since the caller sends the lane it dropped into
				// ([Writer.PlaceTask]).
				return statelog.Decision{Version: int64(current.Version)}, nil
			}
			// A COPY, for [settleWatch]'s reason: Decide runs again on a
			// retry, and an amendment folded into the captured patch would
			// compound across attempts.
			patch := patch
			if amend != nil {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				amended, err := amend(ctx, tx, current, patch)
				if err != nil {
					return statelog.Decision{}, err
				}
				patch = amended
			}
			if patch.Mint != nil {
				// THE KEY IT LEAVES BECOMES A FORMER ONE, read here, not where the
				// caller read the task ([formerKeysAfterRekey]).
				former := formerKeysAfterRekey(current)
				patch.FormerKeys = &former
			}
			if patch.Tags != nil {
				// AGAINST THE TASK'S OWN PROJECT, not the argument's: a move carries
				// the tags into the destination before it re-homes the task, and this
				// read sees both.
				home := current.Project
				if patch.Project != nil {
					home = *patch.Project
				}
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := declaredTags(ctx, tx, home, *patch.Tags); err != nil {
					return statelog.Decision{}, err
				}
			}
			asks := ""
			if postsComment(patch, kind) {
				// A NEW POST ([postedComment] answered every other), so its question
				// is asked now. An edit or a resolve asks nobody: its amendment is its
				// check.
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := settleAnswer(ctx, tx, id, patch.Comment); err != nil {
					return statelog.Decision{}, err
				}
				asks = patch.Comment.Ask
			}
			charged, err := w.chargeHandOff(current, patch)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = settleWatch(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			if asks != "" {
				charged = settleAskWatch(current, charged, asks)
			}
			charged, err = endAskWatch(ctx, tx, id, current, charged, kind)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = settleWaitReason(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = w.settleRelations(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = settleDependents(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			// A CHECKLIST GESTURE is one change to the checklists, which
			// are carried whole — so it is resolved against this snapshot,
			// and refused beside a whole set inside [settleChecklist].
			charged, err = settleChecklist(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			if charged.Fields != nil {
				// THE COERCION TABLE and the required-field check, as on a create: an
				// update must not store prose in a number field or empty a required
				// one.
				//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
				coerced, warned, err := settleFields(ctx, tx, current.Project,
					current.Type, *charged.Fields, w.World, w.zone())
				if err != nil {
					return statelog.Decision{}, err
				}
				if err := refuseUnsetRequired(ctx, tx, current, coerced); err != nil {
					return statelog.Decision{}, err
				}
				charged.Fields = &coerced
				fieldCaveats = warned
			}
			// THE SCOPE THE REQUEST CLAIMED STILL COVERS THIS WRITE. Every task
			// apply rewrites `blocker_open` and `cleared_at` on rows of OTHER tasks
			// naming this one as a blocker, so the scope names each dependent
			// ([Writer.scopeForDependents]), read BEFORE the request because the
			// publisher probes deferrals with the request's scope and the applier
			// files under the envelope's. A dependent arriving since is refused
			// here; the re-run enumerates it.
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			filed, err := dependentsFiled(ctx, tx, current.Dependents)
			if err != nil {
				return statelog.Decision{}, err
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := scope.covers(current.Dependents, filed); err != nil {
				return statelog.Decision{}, err
			}
			// AND EVERY KEY THIS WRITE MOVES, paired the same way: asked with the
			// applier's own question ([keysMoved]) of the stored task and the one
			// this record leaves.
			next := applyPatch(current, charged)
			if keysMoved(current, next) {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := scope.coversKeys(append(keysOf(current),
					keysOf(next)...)); err != nil {
					return statelog.Decision{}, err
				}
			}
			// THE WAKE SAYS WHAT THIS WRITE MOVED, from this snapshot's two states,
			// not the caller's read ([Notify.restated]): the router picks who hears
			// it by these deltas.
			notify, err = w.restatedFor(ctx, tx, notify, current, next)
			if err != nil {
				return statelog.Decision{}, err
			}
			// AN ASSIGNMENT WHATEVER ITS KIND: a status move that takes the
			// item is recorded under `status`, which outranks `assignee`, and
			// it is still the row the pickup reads for whom the work is for.
			assigns := charged.Assignee != nil && *charged.Assignee != current.Assignee
			decision, err := w.decideAssigning(ctx, tx, stamp, subject, OpPatch, kind, scope, opID,
				charged, notify, at, assigns)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	if patch.Body != nil {
		result.Caveats = bodyCaveats(*patch.Body)
	}
	result.Caveats = append(result.Caveats, fieldCaveats...)
	result.Posted = posted
	return result, err
}

// postsComment reports whether a patch POSTS its comment: it carries one,
// under any kind but the three a remark's own gestures form from its stored
// row, an edit ([Writer.EditComment]), a resolve ([Writer.ResolveComment]) and
// a removal ([Writer.RemoveComment]).
func postsComment(patch TaskPatch, kind ChangeKind) bool {
	return patch.Comment != nil && kind != ChangeCommentEdited &&
		kind != ChangeCommentResolved && kind != ChangeCommentRemoved
}

// PostedRemark is what a remark already on its task asked and answered, as its
// row holds them: a post naming it is the call that posted it made again,
// answered from that row ([postedComment]), so this — not what the call
// carried this time — is what the post did.
type PostedRemark struct {
	// Asked is the handle the remark put a question to, empty for none.
	Asked string
	// Answers is the comment id of the question the remark answered, empty
	// for none.
	Answers string
}

// postedComment is the remark already on this task under its id, as its author
// posted it, or nil when no row holds that id.
//
// A POST IS WRITTEN ONCE. A re-run of the call that posted it names the same
// id (derived as the work began: [commentID] in internal/agent/builtin), even
// on a rebased attempt the ledger does not hold, and the decide answers it
// from the row and publishes nothing: no history row, second wake or watch
// gesture. ONE READ for this decide and [Reader.Thread], for [readAsk]'s
// reason. An id another task's or another author's remark holds is refused:
// answered from it, the call would be told a remark it never made was its own.
func postedComment(ctx context.Context, tx *sql.Tx, task, comment,
	author string) (*PostedRemark, error) {

	var holder, wrote, asked string
	var answers sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT task_id, author, ask, answers FROM tracker_comments WHERE id = ?`,
		comment).Scan(&holder, &wrote, &asked, &answers)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("worklet: read comment %s: %w", comment, err)
	case holder != task:
		return nil, invalid("comment %s is a remark on task %s, so it "+
			"cannot be posted on task %s — a new remark takes an id of its own",
			comment, holder, task)
	case wrote != author:
		return nil, invalid("comment %s is a remark by %s, so it cannot be "+
			"posted as %s's — a new remark takes an id of its own",
			comment, wrote, author)
	}
	return &PostedRemark{Asked: asked, Answers: answers.String}, nil
}

// settleAnswer is a new remark's check against the rows it lands among: the
// question it answers, if it answers one.
//
// INSIDE THE DECIDE SNAPSHOT, because whether the ask is still open moves: two
// answers decided at once would both pass the pre-publish thread read, and the
// second land claiming a question it did not close. Here it loses at the
// broker and its re-decide refuses it by name. The
// ask's decision is immutable, so the choice checked here is the one every
// node reads. Only a new remark reaches it ([postedComment]; an edit carries
// the stored decision whole).
func settleAnswer(ctx context.Context, tx *sql.Tx, task string, comment *Comment) error {
	if comment.Answers == nil || *comment.Answers == "" {
		return nil
	}
	ask, err := readAsk(ctx, tx, task, *comment.Answers)
	if err != nil {
		return err
	}
	if err = ask.answerableBy(""); err != nil {
		return err
	}
	if err = checkChoice(task, ask.id, ask.decision, comment.Choice); err != nil {
		return err
	}
	if err = checkStatesChoice(task, ask.id, ask.decision, comment); err != nil {
		return err
	}
	// AN ACT'S APPROVING CHOICE IS ITS KIND'S APPROVAL, decided as one by
	// whoever posts the answer before it reaches here (ADR-0125).
	if a := ask.act; a != nil && comment.Choice == a.Approve && !delegated.Approves(ctx, a.Kind) {
		return invalid("the ask %s on task %s approves one exact %s request, and only an administrator "+
			"or a person the company names to approve it, signed in again recently, may choose %q",
			ask.id, task, a.Kind, a.Approve)
	}
	return nil
}

// settleWaitReason holds a wait's reason to its start: a reason needs a start
// set in the same patch, and a start set or cleared without one clears the
// reason it had, which said why the item waited until the old instant.
func settleWaitReason(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.WaitReason != nil && *patch.WaitReason != "" {
		reason := *patch.WaitReason
		switch {
		case patch.StartAt == nil || patch.StartAt.IsZero():
			return patch, invalid("a wait reason on task %s needs the start it waits until, "+
				"set in the same change", current.ID)
		case len(reason) > MaxWaitReason || strings.ContainsAny(reason, "\r\n"):
			return patch, invalid("a wait reason on task %s is one line of at most %d bytes",
				current.ID, MaxWaitReason)
		}
		return patch, nil
	}
	// A CLEARED START IS ITS FLAG ([TaskPatch.ClearStart]), with no StartAt:
	// the reason of a wait whose start is taken off goes with it.
	if (patch.StartAt != nil || patch.ClearStart) && patch.WaitReason == nil && current.WaitReason != "" {
		cleared := ""
		patch.WaitReason = &cleared
	}
	return patch, nil
}

// settleAskWatch puts the person a new question asks on the task's watchers:
// an ask is an obligation, and its holder has to hear what happens to the
// item they owe an answer about. Inside the decide, over the commenter's own
// watch, for [settleWatch]'s reason.
func settleAskWatch(current Task, patch TaskPatch, asked string) TaskPatch {
	watchers, muted := current.Watchers, current.Muted
	if patch.Watchers != nil {
		watchers = *patch.Watchers
	}
	if patch.Muted != nil {
		muted = *patch.Muted
	}
	if next, ok := followAuto(watchers, muted, asked); ok {
		patch.Watchers = &next
		held := append(slices.Clone(askWatchOf(current, patch)), asked)
		patch.AskWatch = &held
	}
	return patch
}

// askWatchOf is the task's ask-added watchers as the patch leaves them.
func askWatchOf(current Task, patch TaskPatch) []string {
	if patch.AskWatch != nil {
		return *patch.AskWatch
	}
	return current.AskWatch
}

// openAskToStatement is whether a task still holds an open question to a
// person other than one comment, named for the plan test.
const openAskToStatement = `SELECT 1 FROM tracker_comments
	WHERE task_id = ? AND ask = ? AND id <> ? AND resolved = 0 AND answered_by IS NULL AND removed = 0
	  AND overtaken = ''
	LIMIT 1`

// endAskWatch ends the watch a question added once it has nothing left to
// follow (ADR-0023): the person an answered or resolved question was put to,
// when no other open question here is theirs, and everybody a question added
// when the task finishes. A watch somebody chose is never ended.
func endAskWatch(ctx context.Context, tx *sql.Tx, task string, current Task, patch TaskPatch,
	kind ChangeKind) (TaskPatch, error) {

	held := askWatchOf(current, patch)
	if len(held) == 0 {
		return patch, nil
	}
	var ends []string
	switch {
	case patch.Status != nil && patch.Status.Group().Finished():
		ends = held
	case patch.Comment != nil && (kind == ChangeCommentResolved || patch.Comment.Answers != nil):
		closed := patch.Comment.ID
		if kind != ChangeCommentResolved {
			closed = *patch.Comment.Answers
		}
		ask, err := readAsk(ctx, tx, task, closed)
		if err != nil {
			return patch, err
		}
		if ask.asked == "" || !slices.Contains(held, ask.asked) {
			return patch, nil
		}
		var open int
		err = tx.QueryRowContext(ctx, openAskToStatement, task, ask.asked, closed).Scan(&open)
		switch {
		case err == nil:
			return patch, nil
		case !errors.Is(err, sql.ErrNoRows):
			return patch, fmt.Errorf("worklet: read whether %s still owes an answer on %s: %w",
				ask.asked, task, err)
		}
		ends = []string{ask.asked}
	default:
		return patch, nil
	}
	watchers := current.Watchers
	if patch.Watchers != nil {
		watchers = *patch.Watchers
	}
	watchers, rest := without(watchers, ends), without(held, ends)
	patch.Watchers, patch.AskWatch = &watchers, &rest
	return patch, nil
}

// followAuto is a watcher set with somebody the write made a party to the
// task added — the person a question is put to, or the default assignee a
// create is filed to — and false when it does not change.
//
// LEFT ALONE when already watching; when MUTED, since the mute is their choice
// not to follow and `asked` delivers regardless; and AT THE CAP, where the
// watch is skipped, not the write refused ([WatchIntent.Auto]).
func followAuto(watchers, muted []string, handle string) ([]string, bool) {
	if handle == "" || slices.Contains(watchers, handle) ||
		slices.Contains(muted, handle) || len(watchers) >= MaxWatchers {
		return watchers, false
	}
	return append(slices.Clone(watchers), handle), true
}

// markOnly reports a patch whose one change is the cross-project move's mark.
func markOnly(patch TaskPatch) bool {
	if patch.Moving == nil {
		return false
	}
	patch.Moving = nil
	return patch.Empty()
}

// laneOnly reports a patch whose one change is the task's status — a board
// drop's lane step, or any caller's status-only edit.
func laneOnly(patch TaskPatch) bool {
	if patch.Status == nil {
		return false
	}
	patch.Status = nil
	return patch.Empty()
}

// settleWatch resolves a membership gesture into the whole watcher sets.
//
// INSIDE THE DECIDE SNAPSHOT, as [Writer.chargeHandOff] is: "add me" is about
// the CURRENT set, and one read in another transaction drops whoever arrived
// in between. BOTH SETS MOVE TOGETHER, since watching is "in [Task.Watchers],
// not in [Task.Muted]"; writing one alone loses the difference from somebody
// who never watched. It returns a COPY: Decide reruns on a retry, and a
// resolution folded into the captured patch would compound.
func settleWatch(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Watch == nil {
		return patch, nil
	}
	if patch.Watchers != nil || patch.Muted != nil {
		// BOTH SPELLINGS AT ONCE IS A PROGRAMMING ERROR, refused rather than
		// ordered: whichever won would silently discard the other.
		return patch, invalid("this patch carries both a watch "+
			"gesture for %s and a whole watcher set — a caller states one or "+
			"the other", patch.Watch.Handle)
	}
	handle := patch.Watch.Handle
	if handle == "" {
		return patch, invalid("a watch gesture names no handle")
	}
	// A GESTURE IS A CHOICE, so a watch a question added stops being one.
	if held := askWatchOf(current, patch); slices.Contains(held, handle) {
		rest := without(held, []string{handle})
		patch.AskWatch = &rest
	}
	watchers := without(current.Watchers, []string{handle})
	muted := without(current.Muted, []string{handle})
	if !patch.Watch.Watch {
		// THE MUTE IS THE WHOLE POINT OF AN UNWATCH: it says they CHOSE not to
		// watch, so the next mention does not put them back.
		muted = append(muted, handle)
		// AND AN UNWATCH IS NEVER REFUSED, so it returns before the cap: it only
		// shrinks the set, and refusing it would leave a task past the cap one
		// nobody could leave.
		patch.Watch = nil
		patch.Watchers, patch.Muted = &watchers, &muted
		return patch, nil
	}
	watchers = append(watchers, handle)
	if len(watchers) > MaxWatchers {
		// THE ROUTING CAP, at the one place a watcher set grows: past
		// [MaxWatchers] an item is a broadcast, its wake the whole inbox. AN
		// AUTOMATIC WATCH IS SKIPPED, NOT REFUSED ([WatchIntent.Auto]): the
		// comment lands, the watch does not.
		if patch.Watch.Auto {
			patch.Watch = nil
			return patch, nil
		}
		// AN EXPLICIT ONE IS THE TASK'S STATE ([ErrState]): it lands once
		// somebody stops watching.
		return patch, refusedByState("this task already has %d watchers, "+
			"the most a task can. Somebody has to stop watching it first",
			len(current.Watchers))
	}
	patch.Watch = nil
	patch.Watchers, patch.Muted = &watchers, &muted
	return patch, nil
}

// chargeHandOff settles the reassignment counter this patch leaves behind, or
// refuses the hand-off:
//
//   - an AGENT moving the assignee to somebody else spends one, refused past
//     [ReassignmentBudget]: the task is circulating, and the next move is a
//     person's;
//   - a HUMAN or OPERATOR touching it at all resets it: somebody looked;
//   - anything else (the engine's own repairs) neither spends nor forgives.
//
// Re-asserting the current assignee spends nothing: it is what an idempotent
// retry looks like, and charging it would let a redelivery exhaust the budget.
func (w *Writer) chargeHandOff(current Task, patch TaskPatch) (TaskPatch, error) {
	switch w.ActorKind {
	case AuthorHuman, AuthorOperator:
		if current.Reassignments != 0 {
			reset := 0
			patch.Reassignments = &reset
		}
		return patch, nil
	case AuthorAgent:
		if patch.Assignee == nil || *patch.Assignee == current.Assignee {
			return patch, nil
		}
		if current.Reassignments >= ReassignmentBudget {
			return patch, fmt.Errorf("%w: %s has been handed on %d times "+
				"without a person touching it, and the budget is %d — say "+
				"what is blocking it on the item instead, and let somebody "+
				"reassign it", ErrReassignmentBudget, current.Key,
				current.Reassignments, ReassignmentBudget)
		}
		spent := current.Reassignments + 1
		patch.Reassignments = &spent
		return patch, nil
	default:
		return patch, nil
	}
}

// MoveTasks repositions tasks in a project's manual order.
//
// THE SUBJECT IS THE ORDER, which no single task owns: two people dragging two
// cards in one project edit one object, and arbitrating on either card would
// land both and leave an order neither intended. The scope is the project
// CONTAINER, not an enumeration, because the set is the tasks named plus a
// re-spread's neighbours, which could exceed the term cap.
func (w *Writer) MoveTasks(ctx context.Context, opID, project string,
	placements []Placement) (WriteResult, error) {

	switch {
	case project == "":
		return WriteResult{}, invalid("a move names no project")
	case len(placements) == 0:
		return WriteResult{}, invalid("a move places no task")
	case len(placements) > MaxTasksNamed+RankRespreadInline:
		// THE CALLER'S OWN MOVES PLUS A RE-SPREAD'S WORTH OF NEIGHBOURS; the
		// container scope is what lets one drag rewrite hundreds of keys.
		return WriteResult{}, invalid("a move carries %d "+
			"placements and one record carries at most %d — %d moves plus "+
			"a re-spread's %d neighbours", len(placements),
			MaxTasksNamed+RankRespreadInline, MaxTasksNamed, RankRespreadInline)
	}
	for _, placement := range placements {
		if !placement.Rank.Valid() {
			return WriteResult{}, invalid("%q is not a well-formed "+
				"rank key", placement.Rank)
		}
	}
	return w.publishOrder(ctx, opID, project,
		func(*sql.Tx) ([]Placement, error) { return placements, nil })
}

// publishOrder appends one record on a project's order, its placements
// decided inside the write's own snapshot: one path for a given list and a
// decided one, since a drag's keys are honest only when minted from the
// neighbours the broker arbitrates.
func (w *Writer) publishOrder(ctx context.Context, opID, project string,
	place func(*sql.Tx) ([]Placement, error)) (WriteResult, error) {

	subject := RankOrderSubject(project)
	scope := ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: project}}}
	at := w.Now()

	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			placements, err := place(tx)
			if err != nil || len(placements) == 0 {
				// NOTHING TO MOVE IS A SUCCESS THAT APPENDS NOTHING
				// — a card dropped where it already sits.
				return statelog.Decision{}, err
			}
			// A RANK MOVE CARRIES NO NOTIFICATION: a reposition changes where a
			// card sits, not what the work is, and must not make a board's drag a
			// source of inbox traffic.
			return w.decide(ctx, tx, stamp, subject, OpPatch, "", scope, opID, RankOrder{
				V: DocumentVersion, Project: project, Placements: placements,
			}, nil, at)
		},
	})
}

// WriteDocument publishes a whole-document object: one path for every kind
// [documentTable] lists, because full post-state is one upsert with no patch
// semantics, affordable for exactly those small kinds.
func (w *Writer) WriteDocument(ctx context.Context, opID string, subject Subject,
	container string, document any, kind ChangeKind,
	notify *Notify) (WriteResult, error) {

	if _, _, err := documentTable(subject); err != nil {
		return WriteResult{}, err
	}
	// THE CONTAINER IS CHECKED, NOT ASSUMED: most kinds carry their home in
	// their subject and a view chooses its own, so a container elsewhere could
	// file a person's record under a project.
	if container != "" && !subject.Kind.HomedInAProject() {
		return WriteResult{}, invalid("a %s names container %q, "+
			"and its own path is derived from its subject — a container here "+
			"would file its deferral where no probe for it looks",
			subject.Kind, container)
	}
	scope := ScopeSet{Subject: true, Container: container}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decide(ctx, tx, stamp, subject, OpPatch, kind, scope, opID, document, notify, at)
		},
	})
}

// TurnRecord is one completed turn segment charged to a task: the payload a
// turn record carries. ONE SHAPE FOR BOTH ENDS: the writer encodes it and
// [Applier.applyTurn] decodes it, so no key grows on one side alone.
type TurnRecord struct {
	// Task is the task id the turn is charged to, never its key: a key
	// moves with the task and an id does not.
	Task string `json:"task"`
	// Seat is the seat whose turn it was, by its handle.
	Seat string `json:"seat"`
	// TurnID is the run the segment belongs to (ADR-0017), which a task's
	// turn list groups segments by. REQUIRED: [Writer.RecordTurn] refuses a
	// segment that names none, since it would belong to no turn.
	TurnID string `json:"turn_id"`
	// Trigger is what woke the turn — the trigger type a reader filters by.
	Trigger string `json:"trigger"`
	// Outcome is how the segment ended: the turn's own decision, `failed`,
	// `stopped` for one a person's pause ended, or `suspended` for a segment
	// that parked on its executor.
	Outcome string `json:"outcome"`
	// Phases are the phases that ran in the segment, in first-run order.
	Phases []string `json:"phases"`
	// Spend is what the segment cost.
	Spend TurnSpend `json:"spend"`

	// Summary is what the segment did, in the agent's own words (the
	// reviewer's account, or the executor's artifact), cut to
	// [MaxTurnSummary]; empty for a segment that failed or parked first. ON
	// THE RECORD, not read from event history at query time: events are per
	// node and kept thirty days, and a task's turn list must outlive both.
	Summary string `json:"summary,omitempty"`

	// Review is the notes of the newest review in the segment that sent the
	// work back for another pass, cut to [MaxTurnSummary]. Empty when no
	// review sent it back, the ordinary case.
	Review string `json:"review,omitempty"`

	// Tools is what the segment's executor called, one entry per tool in
	// first-call order with its call count, at most [MaxTurnTools]. A COUNT,
	// because the arguments and results belong on the trace.
	Tools []TurnTool `json:"tools,omitempty"`

	// FailedIn is the phase that failed, for a segment whose [Outcome] is
	// `failed` because one of its phases did; empty otherwise, and for a
	// failure outside every phase. The phase list says what ran; this says
	// which step broke.
	FailedIn string `json:"failed_in,omitempty"`
}

// TurnTool is one tool a turn segment called, and how often.
type TurnTool struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
}

// MaxTurnSummary bounds [TurnRecord.Summary] and [TurnRecord.Review], in
// bytes: the three or four sentences a reviewer's account runs to, keeping a
// task's fifty-turn page near 60 KiB with both filled. The whole text stays on
// the turn's trace.
const MaxTurnSummary = 600

// MaxTurnTools bounds [TurnRecord.Tools]. Sixteen distinct tools is more than
// any turn the engine's prompts shape calls in one segment; past it the card
// would be a wall of chips, and the trace is where the rest are.
const MaxTurnTools = 16

// CountTurnTools folds a segment's tool calls, one name per call in call
// order, into [TurnRecord.Tools]: first-call order, a count each, at most
// [MaxTurnTools] names. A name past the cap is dropped, not folded into an
// "other" bucket, which names nothing a reader can look for.
func CountTurnTools(calls []string) []TurnTool {
	if len(calls) == 0 {
		return nil
	}
	var out []TurnTool
	at := map[string]int{}
	for _, name := range calls {
		if name == "" {
			continue
		}
		if i, seen := at[name]; seen {
			out[i].Calls++
			continue
		}
		if len(out) == MaxTurnTools {
			continue
		}
		at[name] = len(out)
		out = append(out, TurnTool{Name: name, Calls: 1})
	}
	return out
}

// recordTurnAttempts bounds how often [Writer.RecordTurn] re-reads a task's
// project that moved between the read and the decide. Each attempt absorbs
// one move in a milliseconds-wide window; three in a row is a task moved
// continuously, refused by name rather than chased.
const recordTurnAttempts = 3

// errTurnTaskMoved is the decide's report that the task left the project its
// scope was formed from — the one refusal [Writer.RecordTurn] retries.
var errTurnTaskMoved = errors.New("worklet: the task moved project while its turn was recorded")

// RecordTurn charges one completed turn segment to a task.
//
// THE ONE ADDITIVE WRITE: it records what already happened, so it races
// nobody, and the op id IS the turn row's id, so a segment recorded twice
// counts once. A task this node lacks, or a PURGED one, is refused; a
// TOMBSTONED one is charged, since a removal destroys nothing; a purge after
// the decide is the applier's deletion gate's ([Applier.Gated]).
//
// THE PROJECT IS READ, NEVER TAKEN FROM THE CALLER: a task moved since the turn
// woke would put the scope where its rows no longer are. It is read off the row
// ([Writer.projectOfTask]), confirmed in the decide ([chargeable]), and a move
// in between is read again ([Writer.recordTurn]).
func (w *Writer) RecordTurn(ctx context.Context, opID string, turn TurnRecord) (WriteResult, error) {
	switch {
	case opID == "":
		return WriteResult{}, invalid("a turn on task %s names no "+
			"operation id — the id is the turn row's own, and it is what "+
			"makes a segment recorded twice count once", turn.Task)
	case turn.Task == "":
		return WriteResult{}, invalid("a turn names no task")
	case turn.TurnID == "":
		return WriteResult{}, invalid("a turn on task %s names no run — the "+
			"run is what its task's turn list groups segments by", turn.Task)
	case len(turn.Summary) > MaxTurnSummary, len(turn.Review) > MaxTurnSummary:
		// REFUSED, NOT CUT: the engine cuts its own prose before it writes, so
		// an overlong one is a writer that skipped the cut.
		return WriteResult{}, invalid("a turn on task %s carries a "+
			"summary of %d bytes and a review of %d; each is at most %d",
			turn.Task, len(turn.Summary), len(turn.Review), MaxTurnSummary)
	case len(turn.Tools) > MaxTurnTools:
		return WriteResult{}, invalid("a turn on task %s names %d "+
			"tools; at most %d", turn.Task, len(turn.Tools), MaxTurnTools)
	case w.db.IsZero():
		return WriteResult{}, invalid("this writer holds no " +
			"replicated estate, and a turn's scope is its task's project, " +
			"which only the task's own row can say")
	}
	return w.recordTurn(ctx, opID, turn, w.projectOfTask)
}

// recordTurn is [Writer.RecordTurn]'s attempts: read the task's project, form
// the scope, decide, and read again up to [recordTurnAttempts] times while the
// decide finds the task moved. The read is a parameter because only a test's
// read can produce the milliseconds-wide window it guards.
func (w *Writer) recordTurn(ctx context.Context, opID string, turn TurnRecord,
	read func(context.Context, string) (string, error)) (WriteResult, error) {

	for attempt := 1; ; attempt++ {
		project, err := read(ctx, turn.Task)
		if err != nil {
			return WriteResult{}, err
		}
		result, err := w.recordTurnIn(ctx, opID, project, turn)
		if !errors.Is(err, errTurnTaskMoved) || attempt == recordTurnAttempts {
			return result, err
		}
	}
}

// taskProjectStatement is the project one task's row names, read by
// [Writer.projectOfTask] and [chargeable] and named for the plan test. Shared
// by both reads, a wrong column would agree with itself, so
// TestATurnIsScopedToTheProjectItsTaskIsIn holds the record's scope to the
// task's project.
const taskProjectStatement = `SELECT project_key FROM tracker_tasks WHERE id = ?`

// projectOfTask is the project a task's own row names, read ahead of the
// decide so the record's scope can be formed. Empty for a task this node does
// not hold, which the decide then refuses with the message that says why.
func (w *Writer) projectOfTask(ctx context.Context, id string) (string, error) {
	var project string
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, taskProjectStatement, id).Scan(&project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("worklet: read the project of task %s to "+
			"scope its turn: %w", id, err)
	}
	return project, nil
}

// recordTurnIn is one attempt at [Writer.RecordTurn], scoped to the project
// the task was read in.
func (w *Writer) recordTurnIn(ctx context.Context, opID, project string,
	turn TurnRecord) (WriteResult, error) {

	subject := TurnSubject(turn.Task)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternAdditive,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if err := chargeable(ctx, tx, turn.Task, project, "a turn cannot be "+
				"charged to rows that no longer exist; the spend is still on the "+
				"seat's own counters"); err != nil {
				return statelog.Decision{}, err
			}
			return w.decide(ctx, tx, stamp, subject, OpTurn, "", scope, opID, turn, nil, at)
		},
	})
}

// chargeable is the decide's own read of the task a turn or a flow record is
// filed under: a task with no row is refused through [missingTask] with
// refused, which says whether that is a purge (final) or a create this node
// has not applied yet.
func chargeable(ctx context.Context, tx *sql.Tx, id, project, refused string) error {
	var home string
	switch err := tx.QueryRowContext(ctx, taskProjectStatement, id).Scan(&home); {
	case errors.Is(err, sql.ErrNoRows):
		return missingTask(ctx, tx, id, refused)
	case err != nil:
		return fmt.Errorf("worklet: read task %s for the record filed under it: %w", id, err)
	case home != project:
		return fmt.Errorf("%w: task %s is in %s and the record was scoped to %s",
			errTurnTaskMoved, id, home, project)
	}
	return nil
}

// missingTask is the refusal of a task this node holds no row for —
// THREE-VALUED: two kinds of absence, and the read failing.
//
// A PURGE LEAVES A MARKER that outlives the row ([taskGuards]), so such a task
// is gone everywhere for good: [ErrNoTask], final, naming its key and what the
// gesture cannot do (refused completes "task KEY (id) was purged, and …").
// Only no row AND no marker is a create not yet applied here:
// [statelog.ErrUnavailable], the one absence worth coming back for. EVERY
// WRITE MEETING A MISSING TASK ANSWERS HERE, because "not on this node" is
// retried by the caller, and a purged task must not be retried for ever.
func missingTask(ctx context.Context, tx *sql.Tx, id, refused string) error {
	var key string
	err := tx.QueryRowContext(ctx,
		`SELECT task_key FROM tracker_deletions WHERE task_id = ?`, id).Scan(&key)
	switch {
	case err == nil:
		return fmt.Errorf("%w: task %s (%s) was purged, and %s", ErrNoTask,
			key, id, refused)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("worklet: read the deletion marker of %s: %w", id, err)
	}
	return fmt.Errorf("worklet: task %s is not on this node: %w", id,
		statelog.ErrUnavailable)
}

// checkChangeKind is the rule that a record which writes a history row states
// what it did, and one that writes none states nothing.
//
// THE WRITER STATES IT because the applier cannot derive it: a catalogue patch
// and a view save, or a tombstone and a purge, are one operation on different
// subjects, and the operation is all the applier has; it stores the kind
// verbatim. A kind and a notification on one record must AGREE, or the feed
// and the card would silently name different things.
func checkChangeKind(subject Subject, op OpKind, kind ChangeKind, notify *Notify) error {
	switch {
	case !subject.Kind.RecordsHistory():
		if kind != "" {
			return invalid("a %s record names change kind %q, and "+
				"an apply of it writes no history row for that kind to "+
				"describe — see ObjectKind.RecordsHistory", subject.Kind, kind)
		}
		switch {
		case notify == nil:
		case !subject.Kind.NoticesItself():
			return invalid("a %s record carries a notification, "+
				"and an apply of it writes no history row a wake could be "+
				"derived from", subject.Kind)
		case notify.Kind != ChangeFlow:
			// A flow record's notice is the flow's own, and its apply writes
			// the inbox row the history row would have.
			return invalid("a %s record carries a %q notification; it tells "+
				"only of its item's flow", subject.Kind, notify.Kind)
		}
		return nil
	case kind == "":
		return invalid("a %s %s record names no change kind — its "+
			"apply writes a history row, and the kind is what every feed "+
			"filter, report window and repair scan selects on", subject.Kind, op)
	case !kind.Valid():
		return invalid("%q is not a change kind this build writes "+
			"— every kind has exactly one writer, so an unknown one is a "+
			"history row no filter can name", kind)
	case notify != nil && notify.Kind != kind:
		return invalid("this %s record says it is a %q and its "+
			"notification says %q — one fact with two carriers is one fact "+
			"that can disagree with itself, and a reader would see the feed "+
			"and the card name different things", subject.Kind, kind, notify.Kind)
	}
	return nil
}

// decide builds the record a write path publishes.
//
// ONE BUILDER for every path, so the envelope's eight reserved keys are filled
// in one place: a record missing one could not be filed by the deferral index,
// which would only show on a rolling upgrade. The generation and the writer
// arrive as [statelog.Stamp], which the publisher checks on the encoded record.
func (w *Writer) decide(ctx context.Context, tx *sql.Tx, stamp statelog.Stamp,
	subject Subject, op OpKind, kind ChangeKind, scope ScopeSet, opID string,
	payload any, notify *Notify, at time.Time) (statelog.Decision, error) {

	return w.decideAssigning(ctx, tx, stamp, subject, op, kind, scope, opID, payload, notify, at, false)
}

// decideAssigning is [Writer.decide] for a record that may move an item's
// assignee whatever its kind, saying whether it does (assigns): a task patch,
// whose decide alone holds the row the patch is compared against.
func (w *Writer) decideAssigning(ctx context.Context, tx *sql.Tx, stamp statelog.Stamp,
	subject Subject, op OpKind, kind ChangeKind, scope ScopeSet, opID string,
	payload any, notify *Notify, at time.Time, assigns bool) (statelog.Decision, error) {

	if err := checkChangeKind(subject, op, kind, notify); err != nil {
		return statelog.Decision{}, err
	}
	// EVERY WAKE SAYS WHETHER ITS KEY OPENS ITS TASK, here for the same reason:
	// the caller built it from a read outside this snapshot, or before its key
	// existed ([Snapshot.KeyCollision]).
	notify, err := collided(ctx, tx, subject, notify)
	if err != nil {
		return statelog.Decision{}, err
	}
	// A CREDENTIAL IS NOBODY'S INBOX: the record offers none as a recipient.
	// Then whether the owner made it, which only this writer knows.
	notify = notify.withoutCredentials().ownership(w.Actor)
	if err = notify.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if err = scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if scope.Subject && scope.Container == "" && subject.Kind.RequiresAProject() {
		return statelog.Decision{}, invalid("a %s record for %s "+
			"states no container, and there is no %s outside a project — an "+
			"empty one resolves to the workspace and files its deferral where "+
			"no project-scoped probe looks", op, subject, subject.Kind)
	}
	carries := w.carries(subject, op, kind, assigns, notify)
	writtenFor, err := w.recordedFor(carries)
	if err != nil {
		return statelog.Decision{}, err
	}
	apps := w.recordedApps(carries)
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("worklet: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// NO VERSION: the encoder stamps the lowest one that reads
			// what this record carries — see [RecordVersion].
			OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Kind:      kind,
		Mutation:  body,
		Actor:     w.Actor,
		ActorKind: w.ActorKind,
		ActorApps: apps,
		// THE HISTORY ROW KEEPS THEM from this version on (ADR-0112).
		ActorAppsKept: len(apps) > 0,
		// THE NOTICE ROWS KEEP THE STATEMENT from this version on.
		NoticeAsksKept: notify != nil && notify.Snapshot.CommentAsks != nil,
		// AND WHETHER IT WOKE EACH RECIPIENT, from this version on, where it
		// tells somebody it does not wake.
		NoticeWokeKept: tellsWithoutWaking(notify, w.Actor),
		WorkingFor:     writtenFor,
		OperatorID:     w.OperatorID,
		TurnID:         w.TurnID,
		Chain:          w.Chain,
		Notify:         notify,
	}
	encoded, err := record.Encode()
	if err != nil {
		return statelog.Decision{}, err
	}
	if len(encoded) > MaxCommitBytes {
		// REFUSED NAMING THE SIZE, never trimmed: a trimmed record cannot rebuild
		// its row, which is the record format's whole point.
		return statelog.Decision{}, invalid("the %s record for %s "+
			"is %d bytes and the design maximum is %d — a record is refused "+
			"rather than trimmed, because a trimmed one cannot rebuild its row",
			op, subject, len(encoded), MaxCommitBytes)
	}
	return statelog.Decision{Payload: encoded}, nil
}

// restatedFor is [Notify.restated] in an update's decide, with two facts read
// in its snapshot: the project's catalogue where either side holds a custom
// value, and the parent's assignee where the write crosses the finished edge.
// THE PARENT HERE, NEVER AT THE CALLER: several callers closing a task never
// name its parent, and every update passes through this decide. Only that
// transition reads it.
func (w *Writer) restatedFor(ctx context.Context, tx *sql.Tx, notify *Notify,
	current, next Task) (*Notify, error) {

	if notify == nil {
		return nil, nil
	}
	var declared map[string]FieldDef
	if len(current.Fields) > 0 || len(next.Fields) > 0 {
		var err error
		if declared, err = declaredFields(ctx, tx, next.Project); err != nil {
			return nil, fmt.Errorf("worklet: read project %s's fields for the wake "+
				"on task %s: %w", next.Project, next.ID, err)
		}
	}
	parent := ""
	if crossesFinished(current, next) && next.Parent != nil && *next.Parent != "" {
		held, found, err := readTask(ctx, tx, *next.Parent)
		if err != nil {
			return nil, fmt.Errorf("worklet: read task %s's parent %s for the "+
				"wake: %w", next.ID, *next.Parent, err)
		}
		if found {
			parent = held.Assignee
		}
	}
	return notify.restated(current, next, declared, w.Leads, parent), nil
}

// collided is a wake with [Snapshot.KeyCollision] derived from the key
// directory in the decide's snapshot ([keyHeldByAnother]), or nil for a record
// that wakes nobody. A COPY: a decide reruns each rejected round, and a wake
// written in place would be read as the caller's by the next.
func collided(ctx context.Context, tx *sql.Tx, subject Subject, notify *Notify) (*Notify, error) {
	if notify == nil {
		return nil, nil
	}
	task := notify.Snapshot.TaskID(subject)
	held, err := keyHeldByAnother(ctx, tx, notify.Snapshot.Key, task)
	if err != nil {
		return nil, fmt.Errorf("worklet: read whether key %s opens task %s "+
			"for the wake on %s: %w", notify.Snapshot.Key, task, subject, err)
	}
	out := *notify
	out.Snapshot.KeyCollision = held
	return &out, nil
}

// wire is the framework's subject for one of this domain's.
func wire(s Subject) statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// published is [Writer.publish] with the domain's own result around it, for
// the paths whose only extra fact is the body warning — which is every
// single-append path.
func (w *Writer) published(ctx context.Context, req statelog.Request) (WriteResult, error) {
	result, err := w.publish(ctx, req)
	return WriteResult{Result: result}, err
}

// publish runs one request, its three outcomes unchanged, translating the
// framework's refusals a caller can act on into its own vocabulary
// ([publishRefusal]).
func (w *Writer) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	if w.refusal != nil {
		// THE GUARD IS HERE, not in [Writer.published]: two sequences publish
		// through this directly.
		return statelog.Result{}, w.refusal
	}
	// THE CALLER'S OWN HIGH-WATER MARK, set only here and handed on by
	// [Writer.After]; zero waits for nothing.
	req.Session = w.after
	wrote := w.recording(ctx, &req)
	result, err := w.publisher.Publish(ctx, req)
	if err != nil {
		return result, publishRefusal(err)
	}
	w.report(wrote, result)
	return result, nil
}

// publishRefusal is the framework's refusal of a publish in this package's
// terms: a create found already made is this package's to say, any other
// travels unchanged. THE SENTINEL, NOT THE FRAMEWORK'S ERROR, whose text names
// a broker subject and an id a person never chose.
func publishRefusal(err error) error {
	if errors.Is(err, statelog.ErrExists) {
		return fmt.Errorf("worklet: that already exists: %w", statelog.ErrExists)
	}
	return err
}

// recording arranges for a task record's item to be reported to the turn's
// [WriteLog] once it commits, and answers where the item will be.
//
// READ INSIDE THE DECIDE'S SNAPSHOT, the only place a key and project are
// certain: a patch carries neither, and a later read races the applier on a
// `pending` write; a create's record carries the whole task. The LAST round's
// answer is kept, being the record the broker took. ONLY A TASK SUBJECT, as
// only a task is a work item. BEST EFFORT: a failed read leaves the item
// unreported rather than failing the write over spend attribution.
func (w *Writer) recording(ctx context.Context, req *statelog.Request) *committing {
	wrote := &committing{}
	if w.written == nil || req.Subject.Kind != string(KindTask) || req.Decide == nil {
		return wrote
	}
	id, decide := req.Subject.ID, req.Decide
	req.Decide = func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
		decision, err := decide(tx, stamp)
		if err != nil || len(decision.Payload) == 0 {
			// A round that decided nothing appends nothing, and names no
			// item for the turn to be charged to.
			wrote.item = nil
			return decision, err
		}
		named := writtenItem(ctx, tx, id, decision.Payload)
		wrote.item, wrote.change = &named, committedChange(decision.Payload)
		return decision, nil
	}
	return wrote
}

// committing is the item one write will report once it commits, set by the
// last round of its decide.
type committing struct {
	item   *types.WorkItem
	change types.CommittedChange
}

// report hands the turn's [WriteLog] the item a write named, once the write is
// known to have committed: `applied` or `pending`, read off the OUTCOME, never
// the position. `unknown` is left out, since the record may not exist and
// charging a turn to it is the guess this set replaces.
func (w *Writer) report(wrote *committing, result statelog.Result) {
	if wrote.item == nil || w.written == nil {
		return
	}
	switch result.Outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		w.written.Add(*wrote.item)
		if wrote.change != "" {
			w.written.Commit(wrote.change)
		}
	case statelog.OutcomeUnknown:
	}
}

// committedChange is the kind of change one record is, for its turn's outcome:
// a comment, an answer to an ask, or a move of the item; "" for a change that
// moves no work: who watches it, and nothing else ([Notify.watchOnly]).
//
// BY WHAT THE WRITE MOVED, as [Reader.TurnsMoved] reads it, never by its kind
// alone: `update_work_item` files a watch written with other fields under
// `watchers`. A quiet commit carries no deltas and is read by its kind.
func committedChange(payload []byte) types.CommittedChange {
	record, err := Decode(payload)
	if err != nil {
		return types.CommittedItem
	}
	switch {
	case record.Kind.Conversation():
		var patch TaskPatch
		if json.Unmarshal(record.Mutation, &patch) == nil && patch.Comment != nil &&
			patch.Comment.Answers != nil {
			return types.CommittedAnswer
		}
		return types.CommittedComment
	case record.Notify != nil && record.Notify.watchOnly(),
		record.Notify == nil && record.Kind == ChangeWatchers:
		return ""
	}
	return types.CommittedItem
}

// writtenItem is the task one record commits to, named as a work item.
//
// The id is the subject's and always known; the key and project are labels,
// and a record whose labels could not be read still names its item by id.
func writtenItem(ctx context.Context, tx *sql.Tx, id string, payload []byte) types.WorkItem {
	named := types.WorkItem{Backend: types.WorkWorklet, ID: id}
	if task, found, err := readTask(ctx, tx, id); err == nil && found {
		named.Key, named.Project = task.Key, task.Project
		return named
	}
	record, err := Decode(payload)
	if err != nil {
		return named
	}
	if record.Notify != nil && record.Notify.Snapshot.Key != "" {
		named.Key, named.Project = record.Notify.Snapshot.Key, record.Notify.Snapshot.Project
		return named
	}
	if record.Op == OpCreate {
		var task Task
		if json.Unmarshal(record.Mutation, &task) == nil {
			named.Key, named.Project = task.Key, task.Project
		}
	}
	return named
}

// SettleChange records a change (a pull request) task waits on as merged or
// closed, as the engine learned it, and tells whoever holds the task to update
// it. The task's own status is never moved: what follows is the holder's call.
// Keyed by the caller on (task, change, state), so a webhook and a poll that
// both saw it write once.
func (w *Writer) SettleChange(ctx context.Context, opID string, task Task, s ChangeSettlement) (WriteResult, error) {
	switch {
	case s.Ref == "" || s.At.IsZero():
		return WriteResult{}, invalid("a settled change on task %s names no change or no instant", task.ID)
	case s.State != ChangeMerged && s.State != ChangeClosed:
		return WriteResult{}, invalid("a change on task %s settles merged or closed, not %q", task.ID, s.State)
	case !waitsOn(task, s.Ref):
		return WriteResult{}, invalid("task %s does not wait on %s", task.ID, s.Ref)
	}
	after := task
	after.Changes = maps.Clone(task.Changes)
	if after.Changes == nil {
		after.Changes = map[string]ChangeSettlement{}
	}
	after.Changes[s.Ref] = s
	notify := Wake{Kind: ChangeLinkSettled, Before: task, After: after}.Notify(w.Leads)
	return w.UpdateTask(ctx, opID, task.ID, task.Project, NoIfMatch,
		TaskPatch{ChangeSettled: &s}, ChangeLinkSettled, notify)
}

// RecordDeadWait records that nothing moves a change task waits on at d.Head,
// as the engine read it on its host (ADR-0128). It notifies nobody: the work
// pickup hands the item back to its holder, once per head.
func (w *Writer) RecordDeadWait(ctx context.Context, opID string, task Task, d DeadWait) (WriteResult, error) {
	switch {
	case d.Ref == "" || d.Head == "" || d.At.IsZero():
		return WriteResult{}, invalid("a dead wait on task %s names no change, no head or no instant", task.ID)
	case !waitsOn(task, d.Ref):
		return WriteResult{}, invalid("task %s does not wait on %s", task.ID, d.Ref)
	}
	return w.UpdateTask(ctx, opID, task.ID, task.Project, NoIfMatch,
		TaskPatch{DeadWait: &d}, ChangeDeadWait, nil)
}

// EndDeadWait drops task's dead wait on ref: something moves the change again.
func (w *Writer) EndDeadWait(ctx context.Context, opID string, task Task, ref string) (WriteResult, error) {
	if _, dead := task.DeadWaits[ref]; !dead {
		return WriteResult{}, invalid("task %s holds no dead wait on %s", task.ID, ref)
	}
	return w.UpdateTask(ctx, opID, task.ID, task.Project, NoIfMatch,
		TaskPatch{DeadWaitEnded: &ref}, ChangeDeadWaitEnded, nil)
}

// waitsOn reports whether task waits on ref.
func waitsOn(task Task, ref string) bool {
	return slices.ContainsFunc(task.Relations, func(r Relation) bool {
		return r.Kind == RelationWaitingOn && r.Other == ref
	})
}
