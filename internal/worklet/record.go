package worklet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/inbox"
)

// The objects, and the two shapes a record carries them in.
//
// A small object (a view, a tag set, a person, a project) travels as FULL
// POST-STATE, one upsert. A task and the field catalogue travel as a TYPED
// PATCH, lest a status flip carry a whole body; a nil field is unchanged:
//
//  1. A scalar in the patch carries its COMPLETE new value, never an excerpt
//     (the excerpts are the notification's; a woken seat reads the payload,
//     wakeBody), so a comment never reaches its seat cut at a card's length.
//  2. A collection the write TOUCHES is carried whole, one it does not is
//     absent: that lets a record rebuild the row, and every collection is
//     capped. No cap on the patch is tighter than the record's own.

// The collection caps. Each is refused at WRITE naming the field, never cut:
// a value silently truncated is a value a person will look for later.
const (
	// MaxTitle and MaxBody bound a task's own text.
	//
	// MaxBody IS SIZED AGAINST THE ANSWER THAT RETURNS IT WHOLE: a cap equal
	// to [builtin.ToolAnswerBytes] leaves no room for any envelope, so
	// `get_work_item` could never return a body at it. 32 KiB (≈ 8,000
	// words) leaves that read a 2× margin for JSON escaping; longer prose is
	// a PAGE (internal/doclet), and the refusal says so.
	MaxTitle = 256
	MaxBody  = 32 << 10

	// MaxCommentBody is a comment's, the same figure for the same reason:
	// the single-comment read returns it whole.
	MaxCommentBody = 32 << 10

	// MaxWatchers is the routing cap and the fan-out cap at once: an
	// explicit sixty-fifth watch is refused, and an automatic one is
	// skipped with a warning.
	MaxWatchers = 64

	// MaxWaitReason is a wait's reason: one line a card shows beside the date.
	MaxWaitReason = 200

	// MaxCollaborators is far smaller than the watcher cap: a collaborator
	// does the work, and a task with more than eight is one nobody owns.
	MaxCollaborators = 8

	// MaxTagsPerTask is ClickUp's own per-task limit.
	MaxTagsPerTask = 40

	// MaxFieldValues and MaxFieldValueBytes bound the custom-field map.
	MaxFieldValues     = 128
	MaxFieldValueBytes = 4 << 10
	// MaxTextareaBytes is the one field type with a larger ceiling.
	MaxTextareaBytes = 16 << 10

	// MaxWaitingOn bounds one task's outbound dependency edges, and
	// MaxOtherRelations the rest together.
	MaxWaitingOn      = 64
	MaxOtherRelations = 128

	// MaxEntries bounds the atlas entries one task names, inside
	// MaxOtherRelations: four times what a turn's systems block lists, past
	// which the work is about the company and its project is what to link.
	MaxEntries = 32

	// MaxRelationNote bounds an edge's one free-text field, in bytes: a
	// sentence saying WHY. Longer is the conversation, which has a comment thread;
	// every byte here is re-encoded by every commit touching any relation
	// and counted against MaxCommitBytes 128 edges at a time.
	MaxRelationNote = 256
	// MaxDependents is the OTHER end, and bounds a write's declared scope on
	// a blocker: every apply of a blocker rewrites two columns on every
	// dependent, so the record names each — this many plus the blocker's own
	// term, one past [MaxScopeTerms], so the largest project folds into its
	// container ([ScopeSet.capped]).
	MaxDependents = 64

	// MaxChecklists, MaxChecklistItems and MaxChecklistItemsTotal bound
	// the checklist tree, and every gesture that grows it is refused past
	// them ([ApplyChecklist]).
	MaxChecklists          = 16
	MaxChecklistItems      = 64
	MaxChecklistItemsTotal = 256

	// MaxChecklistName and MaxChecklistItemName bound the tree's text, at
	// the figures [MaxCommitBytes] counts it at (256 items at 256 bytes is
	// its "100 KiB"). A longer line is a task of its own.
	MaxChecklistName     = 128
	MaxChecklistItemName = 256

	// MaxMentions bounds a comment's resolved mentions.
	MaxMentions = 32

	// MaxThreadParticipants bounds the people a REPLY carries, so a thread
	// of a hundred voices does not put a hundred handles on every record.
	// Past a dozen or so a thread is a meeting, routed by mention; a
	// participant beyond the cap still hears as a watcher.
	MaxThreadParticipants = 16

	// MaxDepth is the subtask depth, and MaxDescendants the subtree size
	// under one root.
	MaxDepth       = 7
	MaxDescendants = 1000

	// BodyRevisionsKept is how many past bodies a task keeps. The prune
	// rides the commit ([TaskPatch.Retires]), so no node deletes a durable
	// row on its own authority.
	BodyRevisionsKept = 100

	// MaxReferencesPerBody bounds the key scan the applier runs on every
	// task's description, so a pathological body is 64 lookups on every
	// node rather than five thousand.
	MaxReferencesPerBody = 64
)

// checkTextCaps refuses a write whose own text is past the cap its field
// declares — [MaxTitle], [MaxBody], [MaxCommentBody] — naming the field.
//
// The read side relies on it: a thread page carries comments WHOLE, as many
// as fit [CommentPageBytes] and at least one, which holds only while a comment
// is within [MaxCommentBody]; one stored past it is reachable through no tool.
//
// BYTES rather than runes, because every ceiling downstream is a byte budget.
// Pure over values, for coerce.go's reason.
func checkTextCaps(title, body, comment *string) error {
	for _, c := range []struct {
		field TextField
		value *string
		limit int
	}{
		{TextTitle, title, MaxTitle},
		{TextBody, body, MaxBody},
		{TextComment, comment, MaxCommentBody},
	} {
		if c.value == nil || len(*c.value) <= c.limit {
			continue
		}
		return &TextCapError{Field: c.field, Size: len(*c.value), Limit: c.limit}
	}
	return nil
}

// TextField names which of a task's own texts a [TextCapError] is about.
type TextField string

// The three texts [checkTextCaps] bounds, named as a refusal names them.
const (
	TextTitle   TextField = "title"
	TextBody    TextField = "body"
	TextComment TextField = "comment body"
)

// TextCapError is a task's own text past the cap its field declares, refused
// rather than cut. It is [ErrInvalid] — the same request can never land and a
// shorter one can.
//
// TYPED, so a caller can NAME THE FIELD to a reader who is not reading Go:
// [builtin]'s refusal composes its sentence from the field, size and cap, and
// a person gets the field's own words (ADR-0060). It NAMES NO TASK: a
// create's id is minted before the write and names nothing anybody can look
// up, and an update's caller already named the item.
type TextCapError struct {
	Field TextField
	// Size is what was sent and Limit the most the field holds, in bytes.
	Size, Limit int
}

func (e *TextCapError) Error() string {
	return fmt.Sprintf("worklet: the %s is %d bytes and the maximum is %d; it "+
		"is refused rather than cut, because a value silently truncated is one "+
		"somebody will look for later; shorten it, or put the long form on a "+
		"page and link it here", e.Field, e.Size, e.Limit)
}

// Unwrap marks the refusal as one about CONTENT ([ErrInvalid]), and carries it
// in a person's words for the field it is about (ADR-0060).
func (e *TextCapError) Unwrap() []error {
	out := []error{ErrInvalid}
	if words := e.person(); !words.IsZero() {
		out = append(out, &caveat.Said{Words: words})
	}
	return out
}

// person is the refusal in a person's words, or none for a field nobody has
// worded one for.
func (e *TextCapError) person() caveat.Refusal {
	switch e.Field {
	case TextTitle:
		return caveat.TitleTooLong()
	case TextBody:
		return caveat.DescriptionTooLong()
	case TextComment:
		return caveat.CommentTooLong()
	}
	return caveat.Refusal{}
}

// Spend is a task's running totals, for ever.
//
// DERIVED BY THE APPLIER from the turn commits rather than transmitted: the
// turn row's insert gates the addition in the same transaction, so the total
// cannot disagree with the records and a redelivery cannot double-count.
type Spend struct {
	Turns      int `json:"turns,omitempty"`
	Rounds     int `json:"rounds,omitempty"`
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	WallMs     int `json:"wall_ms,omitempty"`
	Tokens     int `json:"tokens,omitempty"`
	Workers    int `json:"workers,omitempty"`
	SentBack   int `json:"sent_back,omitempty"`
}

// Tombstone is an operator's removal.
//
// A TOMBSTONED TASK IS FROZEN (no comment, body, field or relation of it can
// change), which makes a removal a local decision with no walk behind it. No
// expiry and no purge horizon: a restore works at any age.
type Tombstone struct {
	By          string     `json:"by"`
	Kind        AuthorKind `json:"kind"`
	At          time.Time  `json:"at"`
	RemovedWith *string    `json:"removed_with,omitempty"`
}

// RelationKind is what one task is to another.
type RelationKind string

const (
	// RelationWaitingOn is a dependency, and the ONE kind that is
	// mirrored: the other end carries the dependent's id so a close can
	// name who it unblocks.
	RelationWaitingOn RelationKind = "waiting_on"
	// RelationLinked is an untyped association, authored on whichever end
	// wrote it.
	RelationLinked RelationKind = "linked"
	// RelationDuplicates is authored on the duplicate and derives
	// "duplicated_by" on the canonical.
	RelationDuplicates RelationKind = "duplicates"
	// RelationPage names a knowledge-base page. The edge rides the task's
	// own record; the pages log is never written.
	RelationPage RelationKind = "page"
	// RelationEntry names an entry of the company's atlas by its id, never
	// by its name or key, so no atlas name enters this log. The edge rides
	// the task's own record and the atlas is never written (ADR-0140).
	RelationEntry RelationKind = "atlas_entry"
)

// RelationKinds is the closed set, in the order a refusal lists them.
var RelationKinds = []RelationKind{
	RelationWaitingOn, RelationLinked, RelationDuplicates, RelationPage, RelationEntry,
}

// NamesTask reports whether an edge of this kind names another task: every
// kind but a page's and an atlas entry's, whose other end is another domain's.
func (k RelationKind) NamesTask() bool { return k != RelationPage && k != RelationEntry }

// Valid reports whether this is a kind this build knows, decided by
// [RelationKinds] alone rather than by a switch that could forget a case.
func (k RelationKind) Valid() bool { return slices.Contains(RelationKinds, k) }

// ChangeSettlement is a change (a pull request) a task waits on, merged or
// closed as its host reported it, by webhook or by the poll.
type ChangeSettlement struct {
	Ref    string    `json:"ref"`
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	Source string    `json:"source,omitempty"`
}

// DeadWait is a change (a pull request) a task waits on that nothing is moving
// at its head, as the engine read it on its host: open, with no reviewer asked,
// no review of that head and no merge asked (ADR-0128).
type DeadWait struct {
	Ref  string `json:"ref"`
	Head string `json:"head"`
	// Author is who opened it: a seat's handle where a seat's app did, else
	// the host's login.
	Author string    `json:"author,omitempty"`
	Draft  bool      `json:"draft,omitempty"`
	At     time.Time `json:"at"`
}

// IsChangeRef reports whether a relation names a change by its URL rather
// than a task, which a task id or key never spells.
func IsChangeRef(other string) bool { return strings.Contains(other, "://") }

// The states a change settles in, and where the engine learned it.
const (
	ChangeMerged     = "merged"
	ChangeClosed     = "closed"
	SettledByWebhook = "webhook"
	SettledByPoll    = "poll"
)

// Relation is one edge, authored on ONE end.
type Relation struct {
	Kind      RelationKind `json:"kind"`
	Other     string       `json:"other"`
	Note      string       `json:"note,omitempty" untrusted:"name"`
	CreatedBy string       `json:"created_by,omitempty"`
	CreatedAt time.Time    `json:"created_at,omitzero"`

	// OneSidedFinal marks an edge whose mirror was refused PERMANENTLY (the
	// dependents cap, a tombstone on the blocker, a blocker gone): an
	// attention flag a person resolves, which the duty never retries.
	//
	// "Not mirrored YET" is not here: it is a fact about the blocker's own
	// rows, so the applier derives it (the `one_sided` case in
	// writeChildren); a writer would be stating another subject's state
	// read in another transaction.
	OneSidedFinal bool `json:"one_sided_final,omitempty"`
}

// ChecklistItem is one line of a checklist.
type ChecklistItem struct {
	ID       string  `json:"id"`
	Name     string  `json:"name" untrusted:"name"`
	Done     bool    `json:"done,omitempty"`
	Assignee string  `json:"assignee,omitempty"`
	Parent   *string `json:"parent,omitempty"`
	Order    int     `json:"order,omitempty"`
}

// Checklist is a group of items. It mints no object and appears on no board.
type Checklist struct {
	ID    string          `json:"id"`
	Name  string          `json:"name" untrusted:"name"`
	Items []ChecklistItem `json:"items,omitempty"`
}

// Task is the object almost every record is about.
//
// Its VERSION is the composed (generation << 40) | stream sequence of the last
// arbitrated commit on its own subject (the checkpoint's integer space), and
// ScopedThrough the highest record that wrote this row FROM ANOTHER SUBJECT.
// They are separate because a record may bump versions on its own subject
// only: stamping a neighbour's would make its next write form an expectation
// the broker refuses for ever.
type Task struct {
	V             int    `json:"v"`
	ID            string `json:"id"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`

	Key        string   `json:"key"`
	FormerKeys []string `json:"former_keys,omitempty"`
	Project    string   `json:"project"`

	// FiledUnit is IMMUTABLE, a record of what was true: it may name a unit
	// the chart no longer has. It stores the unit's KEY (`org.Unit.Key`,
	// via [Units]), so a rename does not move the work; a read resolves to
	// the same key ([unitKeys]).
	FiledUnit string `json:"filed_unit,omitempty"`
	// RoutingUnit is the mutable half (whose lead hears about this task
	// NOW) and the only unit field a write touches. It takes the key too.
	RoutingUnit string `json:"routing_unit,omitempty"`

	// Depth is a HINT: the applier derives the real depth from the closure
	// and enforces the cap there, because two concurrent re-parents can
	// form a tree no single write saw.
	Parent *string `json:"parent,omitempty"`
	Depth  int     `json:"depth,omitempty"`

	Type  string `json:"type"`
	Title string `json:"title" untrusted:"name"`
	Body  string `json:"body,omitempty" untrusted:"fence"`

	// BodyVersion, BodyAuthor and BodyAt describe the CURRENT body and are
	// copied into the revision the next write makes, so a revision's bytes
	// are a pure function of the state it replaced.
	BodyVersion int       `json:"body_version,omitempty"`
	BodyAuthor  string    `json:"body_author,omitempty"`
	BodyAt      time.Time `json:"body_at,omitzero"`

	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group"`

	// StatusEnteredAt is the EFFECTIVE instant (the closed form over every
	// row about this task at or below this position), not the writer's
	// clock, so two nodes at one checkpoint compute the same value.
	StatusEnteredAt time.Time `json:"status_entered_at,omitzero"`

	Priority Priority `json:"priority"`
	Rank     Rank     `json:"rank,omitempty"`

	Reporter      string   `json:"reporter,omitempty"`
	Assignee      string   `json:"assignee,omitempty"`
	Collaborators []string `json:"collaborators,omitempty"`

	// Watchers is the set and Muted the subtraction, and BOTH travel: the
	// difference alone makes "not a watcher" and "muted" one row, and a
	// replay would re-add every unwatched person on the next mention.
	Watchers []string `json:"watchers,omitempty"`
	Muted    []string `json:"muted,omitempty"`
	// AskWatch is the watchers a question put to them added, who chose
	// nothing: their watch ends with their last open question here.
	AskWatch []string `json:"ask_watch,omitempty"`

	Tags []string `json:"tags,omitempty"`

	// Fields are keyed by field ID ONLY (the write path resolves the slug
	// an agent types), so a rename never re-points a stored value.
	Fields map[string]json.RawMessage `json:"fields,omitempty" untrusted:"name"`

	Relations  []Relation  `json:"relations,omitempty"`
	Dependents []string    `json:"dependents,omitempty"`
	Checklists []Checklist `json:"checklists,omitempty"`

	StartAt   *time.Time `json:"start_at,omitempty"`
	DueAt     *time.Time `json:"due_at,omitempty"`
	DueAllDay bool       `json:"due_all_day,omitempty"`
	// WaitReason is why the item waits until StartAt, said by whoever set the
	// wait; a start set without one clears it.
	WaitReason string `json:"wait_reason,omitempty" untrusted:"name"`

	// Changes are the changes (pull requests) the task waits on that their
	// host reported merged or closed, by canonical URL.
	Changes map[string]ChangeSettlement `json:"changes,omitempty"`

	// DeadWaits are the open changes the task waits on that nothing is moving,
	// by canonical URL: dropped when one moves, settles or is no longer waited on.
	DeadWaits map[string]DeadWait `json:"dead_waits,omitempty"`

	EstimateMinutes int     `json:"estimate_minutes,omitempty"`
	Points          float64 `json:"points,omitempty"`
	Spend           Spend   `json:"spend,omitzero"`

	// DoneAt and ClosedAt are stamped BY GROUP, not slug: the first entry
	// into done (done OR cancelled) sets one, into closed the other. A
	// reopen clears both, so every "recent" filter agrees with the state.
	DoneAt   *time.Time `json:"done_at,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`

	Archived bool `json:"archived,omitempty"`

	// Merging is true while this task's merge walk runs, and MergeReparent
	// is what the walk INTENDS for the subtree ([TaskPatch.Merging]).
	//
	// The intent is durable because the repair duty cannot otherwise tell a
	// merge that left the children on purpose from one that crashed before
	// moving the first, and guessing overrides a `move_subtasks: false`. It
	// rides the document, not a column: the duty selects on `merging` only.
	Merging       bool `json:"merging,omitempty"`
	MergeReparent bool `json:"merge_reparent,omitempty"`

	// Moving is true on a ROOT whose cross-project move has not yet carried
	// every task beneath it ([TaskPatch.Moving]).
	Moving bool `json:"moving,omitempty"`

	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	ArchivedBy string     `json:"archived_by,omitempty"`

	Removed *Tombstone `json:"removed,omitempty"`

	// ChangeSeq counts commits; Reassignments is the hand-off budget,
	// charged only when an AGENT changes the assignee and reset by any human
	// or operator write: a hand-off moves ownership down a chart of known
	// height, not a nested ask.
	ChangeSeq     int `json:"change_seq,omitempty"`
	Reassignments int `json:"reassignments,omitempty"`

	// PolicyStamp records which policy version the last write validated
	// against.
	PolicyStamp string `json:"policy_stamp,omitempty"`

	// CreatedAt and UpdatedAt are the AUTHORED instants. Their skew against
	// the broker's stored value is published, not corrected: differencing a
	// CLAMPED instant blames whichever node wrote second for its
	// neighbour's error.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// FinishedAt is the earlier of the two stamps, or nil.
func (t Task) FinishedAt() *time.Time {
	switch {
	case t.DoneAt == nil:
		return t.ClosedAt
	case t.ClosedAt == nil:
		return t.DoneAt
	case t.ClosedAt.Before(*t.DoneAt):
		return t.ClosedAt
	}
	return t.DoneAt
}

// Delivered reports work that finished AND was delivered.
func (t Task) Delivered() bool { return Delivered(t.Status) }

// Comment is a ROW a task's commit produces, not an object of its own.
//
// NO VERSION AND NO SUBJECT: it is arbitrated by the task's own version. Its
// id is derived from the task, turn and body when a turn writes it, so a
// retried turn completes rather than duplicating.
type Comment struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`
	Body       string     `json:"body" untrusted:"fence"`

	// Mentions are resolved at WRITE (a unit to its effective lead, the
	// shorthands expanded); at read time they would name a later holder.
	Mentions []string `json:"mentions,omitempty"`

	ReplyTo *string `json:"reply_to,omitempty"`

	// Ask is set only at creation: an old remark turned into a question
	// would wake somebody for a conversation that has moved on.
	Ask     string  `json:"ask,omitempty"`
	Answers *string `json:"answers,omitempty"`

	// Decision is the structure an ask carries when somebody must choose
	// (decision.go). Only on the ASK, never changed after: an answer names
	// an option by id, and edited options would re-point it.
	Decision *decision.Decision `json:"decision,omitempty"`

	// Act is what the ask is for, set only by the engine on a question it
	// composed (ADR-0125); nil otherwise. Immutable, as the decision is.
	Act *delegated.Act `json:"act,omitempty"`

	// Choice is the option an ANSWER chose, by id, checked against the
	// decision of the ask it answers inside the write's own snapshot.
	Choice string `json:"choice,omitempty"`

	Resolved   bool       `json:"resolved,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`

	// Withdrawal is why its asker withdrew this question and what covers it
	// now, set by the resolve that withdrew it; nil otherwise.
	Withdrawal *Withdrawal `json:"withdrawal,omitempty"`

	// Removed blanks the body and KEEPS the row, so replies still resolve
	// against something.
	Removed bool `json:"removed,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// BodyRevision is the body a task held at one version, carried in the same
// commit that replaces it.
//
// NOT AN OBJECT OF ITS OWN: no subject, no create-only race, nothing to
// repair, because the revision and the new body land in one transaction.
type BodyRevision struct {
	Task       string     `json:"task"`
	Version    int        `json:"version"`
	Body       string     `json:"body"`
	Author     string     `json:"author,omitempty"`
	AuthorKind AuthorKind `json:"author_kind,omitempty"`
	At         time.Time  `json:"at"`
}

// KeyAlias is a row created by the commit that mints or moves a key.
//
// NEVER DELETED BY A TASK APPLY nor lowered from current by a later one: a
// former key, pasted into chat and tool calls, resolves for ever.
type KeyAlias struct {
	Key     string `json:"key"`
	TaskID  string `json:"task_id"`
	Current bool   `json:"current,omitempty"`
}

// Counter is a project's key sequence, on its own subject so a mint never
// contends with an edit to that project's settings.
type Counter struct {
	V       int    `json:"v"`
	Version uint64 `json:"version"`
	Project string `json:"project"`
	Last    int    `json:"last"`

	Extra map[string]json.RawMessage `json:"-"`
}

// RankOrder is a project's manual order.
//
// A rank move mutates the project's total order, which no single task's
// version can protect, so it has a subject of its own.
type RankOrder struct {
	V             int    `json:"v"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`
	Project       string `json:"project"`

	// Placements is one record's moves: at least one, and at most
	// [MaxTasksNamed] of a caller's own plus a re-spread's
	// [RankRespreadInline] neighbours — see [Writer.MoveTasks].
	Placements []Placement `json:"placements"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Roster is the org chart's seats in the order of their names: what
// `sort=assignee` orders by, reconciled from the chart ([Writer.ApplyChart]).
//
// THE ORDER IS THE WRITER'S, carried whole: its collation ([seatsByName])
// runs once at decide, so every node applies the same places whatever
// collation tables its build carries.
type Roster struct {
	V             int    `json:"v"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`

	// Seats are the chart's handles, a seat's place its index here.
	Seats []string `json:"seats"`

	// ChartEpoch is the activation the order was taken from
	// ([configplane.ActivationStamp]), so a late older one cannot walk it back.
	ChartEpoch int64 `json:"chart_epoch"`

	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Placement is one task's new position.
type Placement struct {
	Task string `json:"task"`
	Rank Rank   `json:"rank"`
}

// Eviction is a node's eviction or its readmission.
//
// ITS SHAPE VERSION IS PINNED AT ONE FOR EVER, because this record INSTALLS
// A GATE: a build that cannot decode it must apply nothing above it. The
// shape evolves additively (an old reader drops an unknown key), and a
// change to its apply takes a raised RECORD version, never a new kind
// ([GateRecordVersion]).
type Eviction struct {
	V          int       `json:"v"`
	Version    uint64    `json:"version"`
	NodeID     string    `json:"node_id"`
	EvictedBy  string    `json:"evicted_by"`
	EvictedAt  time.Time `json:"evicted_at"`
	Readmitted bool      `json:"readmitted,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Generation is a reanchor's record, create-only at an expectation of zero.
//
// Two operators deriving the same number race there and exactly one wins.
type Generation struct {
	V                   int       `json:"v"`
	Gen                 uint32    `json:"gen"`
	PrevStreamCreatedAt time.Time `json:"prev_stream_created_at,omitzero"`
	NewStreamCreatedAt  time.Time `json:"new_stream_created_at"`
	PrevLastSeqSeen     uint64    `json:"prev_last_seq_seen,omitempty"`
	ReanchoredBy        string    `json:"reanchored_by"`
	Reason              string    `json:"reason,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// TaskPatch is the typed patch: a nil field is unchanged.
//
// THE POINTER tells "set this to empty" from "leave it alone", and every
// collection is a POINTER TO A SLICE for the same reason: absent tags are
// untouched, an empty list clears them.
//
// # The pointer idiom's one exception: the two dates
//
// A date has no empty VALUE: the zero instant reads as the most overdue task
// ever, and internal/estate's router drops a pointer at it (omitzero), so a
// clear would arrive as no date. A clear is its own flag, [TaskPatch.ClearStart]
// and [TaskPatch.ClearDue]; a date set is always real.
type TaskPatch struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`

	// BodyRevision and Retires ride a body write: the revision it replaced
	// and the versions its prune removed. THE PRUNE IS ON THE COMMIT so every
	// node deletes the same rows at the same position.
	BodyRevision *BodyRevision `json:"body_revision,omitempty"`
	Retires      []int         `json:"retires,omitempty"`

	Status      *Status   `json:"status,omitempty"`
	Priority    *Priority `json:"priority,omitempty"`
	Type        *string   `json:"type,omitempty"`
	Assignee    *string   `json:"assignee,omitempty"`
	RoutingUnit *string   `json:"routing_unit,omitempty"`
	Parent      *string   `json:"parent,omitempty"`
	Project     *string   `json:"project,omitempty"`

	// Mint is the counter value this record took, and THE ONLY WAY A
	// PATCH MAY WRITE A KEY OR A RANK.
	//
	// Both have exactly two producers (D144): a record carrying a counter
	// value minted in that project, and a record on its rankorder subject.
	// A `Key *string` field could name a key another task holds, and a
	// `Rank *Rank` would race every drag. The applier derives both from the
	// mint, so a patch writes exactly what the counter's arbitration gave.
	Mint *KeyMint `json:"mint,omitempty"`

	// StartAt and DueAt set a date, always a real instant: the writer
	// refuses the zero instant ([TaskPatch.CheckDates]). ClearStart and
	// ClearDue take one off (the type's doc says why a flag); a patch that
	// both sets and clears one date is refused.
	StartAt    *time.Time `json:"start_at,omitempty"`
	DueAt      *time.Time `json:"due_at,omitempty"`
	ClearStart bool       `json:"clear_start,omitempty"`
	ClearDue   bool       `json:"clear_due,omitempty"`
	DueAllDay  *bool      `json:"due_all_day,omitempty"`
	WaitReason *string    `json:"wait_reason,omitempty"`

	// ChangeSettled is a change the task waits on merging or closing.
	ChangeSettled *ChangeSettlement `json:"change_settled,omitempty"`

	// DeadWait records a change the task waits on as moved by nothing, and
	// DeadWaitEnded drops that record for the change it names.
	DeadWait      *DeadWait `json:"dead_wait,omitempty"`
	DeadWaitEnded *string   `json:"dead_wait_ended,omitempty"`

	EstimateMinutes *int     `json:"estimate_minutes,omitempty"`
	Points          *float64 `json:"points,omitempty"`

	Archived *bool      `json:"archived,omitempty"`
	Removed  *Tombstone `json:"removed,omitempty"`

	// Merging is set while a merge's child walk runs and cleared by its
	// last append: it makes a duplicate visibly MID-MERGE, and the duty
	// selects on it to finish a walk whose holder died.
	//
	// MergeReparent rides the same append; the applier clears it whenever
	// the marker goes down, so the pair never reads "not merging, but
	// re-parenting".
	Merging       *bool `json:"merging,omitempty"`
	MergeReparent *bool `json:"merge_reparent,omitempty"`

	// Moving is the cross-project move's marker, as Merging is a merge's:
	// set by the SAME append that moves the root (the moment the subtree
	// splits, so a separate mark could land without the move or the move
	// without it), cleared by the move's last, and selected on by the duty.
	Moving *bool `json:"moving,omitempty"`

	// Reassignments is the hand-off counter this write leaves, decided by
	// the WRITER in its snapshot ([Writer.chargeHandOff]) and carried as a
	// value, because two nodes deriving it could disagree.
	Reassignments *int `json:"reassignments,omitempty"`

	// Watch is a MEMBERSHIP gesture ("add me", "take me off") the WRITER
	// resolves in its decide snapshot into the whole Watchers and Muted sets
	// below. A caller CANNOT form those sets: they are carried whole, so
	// `watchers: [me]` would replace everybody watching, and a set read
	// outside the decide is written from a stale transaction.
	//
	// NEVER ON THE WIRE: the applier sees the resolved collections, so a
	// replay writes rows rather than re-deriving a set.
	Watch *WatchIntent `json:"-"`

	// Relate and Depend are the same kind of gesture as Watch, for the two
	// halves of a dependency: [TaskPatch.Relations] and
	// [TaskPatch.Dependents] are carried whole, so a set read outside the
	// decide discards every edge that arrived since, and the caps
	// ([MaxWaitingOn], [MaxOtherRelations], [MaxDependents]) must be counted
	// against the set the decide reads. NEVER ON THE WIRE, like Watch.
	Relate *RelationIntent  `json:"-"`
	Depend *DependentIntent `json:"-"`

	// Checklist is the same gesture over [TaskPatch.Checklists], where one
	// person ticks while another adds. Resolved by [settleChecklist] inside
	// the decide; NEVER ON THE WIRE, like Watch.
	Checklist *ChecklistIntent `json:"-"`

	// The collections, carried WHOLE when touched.
	Collaborators *[]string                   `json:"collaborators,omitempty"`
	Watchers      *[]string                   `json:"watchers,omitempty"`
	Muted         *[]string                   `json:"muted,omitempty"`
	AskWatch      *[]string                   `json:"ask_watch,omitempty"`
	Tags          *[]string                   `json:"tags,omitempty"`
	Fields        *map[string]json.RawMessage `json:"fields,omitempty"`
	Relations     *[]Relation                 `json:"relations,omitempty"`
	Dependents    *[]string                   `json:"dependents,omitempty"`
	Checklists    *[]Checklist                `json:"checklists,omitempty"`
	FormerKeys    *[]string                   `json:"former_keys,omitempty"`

	// Comment rides a task write and shares its arbitration.
	Comment *Comment `json:"comment,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Empty reports a patch that would change no field of the item.
//
// REFLECTION rather than a hand-kept list of fields, which fails SILENTLY when
// a field is added: the write reports success and changes nothing. Every field
// is a pointer, slice, map or clear flag whose zero means "not set", so "no
// field is set" is a property of the value and covers a new field the day it
// lands. It runs once per write, against a broker round trip.
func (p TaskPatch) Empty() bool { return reflect.ValueOf(p).IsZero() }

// CheckDates refuses a patch whose dates say what no task can hold: a date set
// to the zero instant (see the type's doc), or both set and cleared — naming
// the field to send instead. A comment it posts is held to
// [Comment.CheckInstants].
//
// THE WRITER'S CHECK ([Writer.UpdateTask]), AND THE ESTATE'S BEFORE IT ASKS
// ANY NODE (tracker.update_task): the estate's wire drops a zero instant, so a
// data node would read a patch naming no date and apply the rest. Refused
// before it is sent, it is refused alike wherever it is asked from.
func (p TaskPatch) CheckDates() error {
	for _, date := range []struct {
		name, clear string
		set         *time.Time
		cleared     bool
	}{{"start_at", "clear_start", p.StartAt, p.ClearStart}, {"due_at", "clear_due", p.DueAt, p.ClearDue}} {
		switch {
		case date.set != nil && date.cleared:
			return invalid("the patch both sets %s and clears it with %s — send one of the two",
				date.name, date.clear)
		case date.set != nil && date.set.IsZero():
			return invalid("the patch sets %s to the zero instant, which is no date — take the date "+
				"off with %s", date.name, date.clear)
		}
	}
	if p.Comment != nil {
		return p.Comment.CheckInstants()
	}
	return nil
}

// CheckInstants refuses a task a create carries holding the zero instant as one
// of its instants (start, due, finish or archive stamp): stored, a zero due
// date is the most overdue task ever. An unset instant is nil.
//
// [Writer.CreateTask]'s check, and the estate's before it asks any node to
// create one (tracker.create_task and tracker.create_task_asking), for
// [TaskPatch.CheckDates]'s reason.
func (t Task) CheckInstants() error {
	for _, at := range []struct {
		name string
		at   *time.Time
	}{{"start_at", t.StartAt}, {"due_at", t.DueAt}, {"done_at", t.DoneAt}, {"closed_at", t.ClosedAt},
		{"archived_at", t.ArchivedAt}} {
		if at.at != nil && at.at.IsZero() {
			return invalid("task %s holds the zero instant as its %s, which is no instant — leave "+
				"it out", t.ID, at.name)
		}
	}
	return nil
}

// CheckInstants refuses a comment a write posts holding the zero instant as the
// instant it was resolved, which is no instant — for [TaskPatch.CheckDates]'s
// reason, on the create that asks it and the patch that posts it.
func (c Comment) CheckInstants() error {
	if c.ResolvedAt != nil && c.ResolvedAt.IsZero() {
		return invalid("comment %s holds the zero instant as its resolved_at, which is no instant — "+
			"leave it out", c.ID)
	}
	return nil
}

// WatchIntent is one person's membership gesture on a task's watcher set.
//
// A HANDLE AND A DIRECTION, never a list: a person says whether THEY watch,
// and [settleWatch] turns it into the set.
type WatchIntent struct {
	Handle string
	Watch  bool

	// Auto marks a watch nobody asked for (a commenter's). It decides what
	// happens at [MaxWatchers]: an explicit watch past it is refused, an
	// automatic one skipped, since refusing it would fail somebody's
	// COMMENT over something they never asked for.
	Auto bool
}

// RelationIntent is a gesture over a task's relation set, resolved inside the
// writer's own decide snapshot by [settleRelations].
//
// TWO SPELLINGS, NEVER BOTH: Set states the whole collection, Add and Remove
// a delta. Even a Set is a gesture: [MaxWaitingOn] and [MaxOtherRelations]
// are counted against the set the decide reads, not the caller's.
type RelationIntent struct {
	// Set replaces the whole collection. Empty and non-nil clears it,
	// which is what `{set: []}` means.
	Set []Relation

	// Add and Remove are the delta. Remove matches on (Kind, Other): a
	// note is not part of an edge's identity.
	Add    []Relation
	Remove []Relation

	// Final marks edges [Relation.OneSidedFinal], matched like Remove. A
	// gesture because its only writer, the repair duty, runs minutes later,
	// and a set from its own scan would discard every edge since.
	Final []Relation
}

// Whole reports whether this gesture states the collection outright.
func (r *RelationIntent) Whole() bool { return r != nil && r.Set != nil }

// DependentIntent is the BLOCKER's half: which dependents this commit adds to
// or removes from [Task.Dependents].
//
// IDS RATHER THAN RELATIONS: the blocker authored no edge; this lists the
// tasks whose `waiting_on` names it, so a close names who it unblocks without
// a reverse scan.
type DependentIntent struct {
	Add    []string
	Remove []string
}

// FieldType is a custom field's type.
type FieldType string

// The thirteen; each constant IS the stored wire value, and coerce.go's table
// (the one place a value is REFUSED) switches on exactly these strings.
const (
	FieldText         FieldType = "text"
	FieldTextarea     FieldType = "textarea"
	FieldNumber       FieldType = "number"
	FieldDate         FieldType = "date"
	FieldDropdown     FieldType = "dropdown"
	FieldLabels       FieldType = "labels"
	FieldCheckbox     FieldType = "checkbox"
	FieldURL          FieldType = "url"
	FieldEmail        FieldType = "email"
	FieldProgress     FieldType = "progress"
	FieldRelationship FieldType = "relationship"
	FieldPeople       FieldType = "people"
	FieldRollup       FieldType = "rollup"
)

// FieldTypes are the thirteen.
var FieldTypes = []FieldType{
	FieldText, FieldTextarea, FieldNumber, FieldDate, FieldDropdown,
	FieldLabels, FieldCheckbox, FieldURL, FieldEmail, FieldProgress,
	FieldRelationship, FieldPeople, FieldRollup,
}

// Valid reports whether a field type off the wire is one this build knows.
func (f FieldType) Valid() bool {
	for _, t := range FieldTypes {
		if t == f {
			return true
		}
	}
	return false
}

// The catalogue caps.
const (
	// MaxFieldsPerDocument caps ONE declaring document. Fields are declared
	// at the workspace and on a project, so a task's union is at most twice
	// this by construction, with no declaration-time union check.
	MaxFieldsPerDocument = 64

	// MaxTypes is the workspace type catalogue's cap.
	MaxTypes = 64

	// MaxOptions is one field's options and MaxOptionsPerDocument the
	// whole document's, because a hundred fields at the per-field cap
	// would be a document nothing can encode.
	MaxOptions            = 128
	MaxOptionsPerDocument = 2048

	// MaxTagsPerProject is the tag set's cap, and MaxTagLabel bounds a
	// tag's label. The SLUG is bounded by its grammar ([ValidTagSlug]), the
	// one place a tag's shape is stated. The label takes [MaxViewName]'s
	// reason: a chip beside a title, like a tab, must fit its strip.
	MaxTagsPerProject = 512
	MaxTagLabel       = MaxViewName

	// MaxViewParamsBytes and MaxViewParamKeys bound a saved view's query;
	// MaxViewName bounds its tab label at half a task's title, because a
	// name that does not fit its strip is one nobody can tell from its
	// neighbour.
	MaxViewParamsBytes = 32 << 10
	MaxViewParamKeys   = 32
	MaxViewName        = 128

	// MaxPriorities and MaxPinnedViews bound a person's own ordering; their
	// inbox lists are bounded by [inbox.MaxEntries].
	MaxPriorities  = 32
	MaxPinnedViews = 32
	MaxFavorites   = 64
)

// Option is one choice of a dropdown or labels field.
type Option struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Color    string `json:"color,omitempty"`
	Order    int    `json:"order,omitempty"`
	Archived bool   `json:"archived,omitempty"`
}

// Ident and Label make an option resolvable by the shared three-tier rule.
func (o Option) Ident() string { return o.Slug }

// Label is what a person reads.
func (o Option) Label() string { return o.Name }

// Rollup is an aggregate over a relationship.
type Rollup struct {
	Source string `json:"source"`
	Field  string `json:"field"`
	Op     string `json:"op"`
}

// FieldConfig is everything a field's type may need.
//
// EVERY KEY HERE IS DECLARABLE: a setting nothing sets or reads is a knob a
// reader will wire to a guess. `Tracking` and `Rollup` stay although the
// tracker refuses both, because a refusal must DECODE what it refuses. A
// field's project is the document declaring it ([declaredFields]), never a key.
type FieldConfig struct {
	Options   []Option `json:"options,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Precision int      `json:"precision,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Time      bool     `json:"time,omitempty"`
	Progress  string   `json:"progress,omitempty"`
	Tracking  []string `json:"tracking,omitempty"`
	Multi     bool     `json:"multi,omitempty"`
	Rollup    *Rollup  `json:"rollup,omitempty"`
}

// FieldDef is one custom field's declaration.
//
// Values are keyed by ID and agents type the SLUG, so a field MOVES between
// the workspace and a project keeping its id and every stored value.
type FieldDef struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Type        FieldType `json:"type"`
	AppliesTo   []string  `json:"applies_to,omitempty"`
	Required    bool      `json:"required,omitempty"`

	// RequiredInSubtasks decides INSTEAD of Required for a task with a
	// parent, default false, so a required field is not a form for every
	// step a task is broken into.
	RequiredInSubtasks bool `json:"required_in_subtasks,omitempty"`

	Config FieldConfig `json:"config,omitzero"`

	// Archived is ONE-WAY: values stay on their tasks but leave the value
	// table, and a restored field is a NEW declaration, so stale values
	// validated against an old definition are never re-admitted.
	Archived       bool `json:"archived,omitempty"`
	Pinned         bool `json:"pinned,omitempty"`
	HideFromAgents bool `json:"hide_from_agents,omitempty"`

	// CreatedBy and CreatedAt are the WRITE's, never the caller's
	// ([stampFields]).
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Ident and Label make a field resolvable by the shared three-tier rule.
func (f FieldDef) Ident() string { return f.Slug }

// Label is what a person reads.
func (f FieldDef) Label() string { return f.Name }

// TaskType is a classification, and nothing more.
//
// It changes no statuses (there is one set) and scopes fields only through
// AppliesTo. Its description is what an agent chooses it BY.
type TaskType struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Plural      string `json:"plural,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
	Builtin     bool   `json:"builtin,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
}

// Ident and Label make a type resolvable by the shared three-tier rule.
func (t TaskType) Ident() string { return t.Slug }

// Label is what a person reads.
func (t TaskType) Label() string { return t.Name }

// Tag is one label in a project's tag set.
type Tag struct {
	Slug        string    `json:"slug"`
	Label       string    `json:"label" untrusted:"name"`
	Color       string    `json:"color,omitempty"`
	Description string    `json:"description,omitempty" untrusted:"name"`
	Archived    bool      `json:"archived,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
}

// Ident is the tag's slug.
func (t Tag) Ident() string { return t.Slug }

// LabelText is what a person reads. NOT named Label, because the field is.
func (t Tag) LabelText() string { return t.Label }

// TypeCatalogue and FieldCatalogue are the two workspace catalogues, on two
// subjects so a type edit never contends with a field edit.
type TypeCatalogue struct {
	V         int        `json:"v"`
	Version   uint64     `json:"version"`
	Types     []TaskType `json:"types"`
	UpdatedAt time.Time  `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// FieldCatalogue is the workspace's field declarations.
type FieldCatalogue struct {
	V       int        `json:"v"`
	Version uint64     `json:"version"`
	Fields  []FieldDef `json:"fields"`

	// PolicyVersion moves on every fields edit, and it is what a task's
	// policy stamp records having validated against.
	PolicyVersion int       `json:"policy_version,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Project is a container's identity and its policy.
type Project struct {
	V             int    `json:"v"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`

	Key string `json:"key"`

	// Name, Purpose and Unit are CHART-OWNED: written only by the chart
	// apply, under the activation guard below; no tool writes Unit.
	//
	// UNIT IS THE UNIT'S KEY (`org.Unit.Key`), not its display name,
	// because a task filed here takes its immutable filed unit from it. A
	// reader renders the key to the current name through [Units].
	Name    string `json:"name"`
	Purpose string `json:"purpose,omitempty"`
	Unit    string `json:"unit,omitempty"`

	// ChartEpoch is the activation the chart-owned fields were last written
	// from ([configplane.ActivationStamp]), so a late older one cannot walk
	// them back.
	ChartEpoch int64 `json:"chart_epoch,omitempty"`

	Fields          []FieldDef `json:"fields,omitempty"`
	DefaultAssignee string     `json:"default_assignee,omitempty"`

	// TargetDate is when the project's LEAD means it to be finished: a day
	// on the company's clock, `YYYY-MM-DD`, empty for no target.
	//
	// A DATE, not an instant, which would invent an hour and a zone.
	// LEAD-OWNED, not chart-owned: set through [Writer.WriteProject], and a
	// chart apply carries it through untouched.
	TargetDate string `json:"target_date,omitempty"`

	// PolicyVersion moves on a fields edit — NOT on tags.
	PolicyVersion int `json:"policy_version,omitempty"`

	Archived  bool      `json:"archived,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// TagSet is a project's tags. Any seat may add; archive and rename follow the
// lead rule.
type TagSet struct {
	V           int       `json:"v"`
	Version     uint64    `json:"version"`
	Project     string    `json:"project"`
	Tags        []Tag     `json:"tags"`
	TagsVersion int       `json:"tags_version,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ViewType is the five renderings, and there are no others.
type ViewType string

// The five; each constant IS the wire value a saved view stores, which the
// dashboard also reads to pick a renderer.
//
// [ViewTimeline] draws the rows against a DATE AXIS, the one arrangement that
// shows a span or a dependency ([TaskRow.Start], [TaskRow.Due],
// [TaskRow.WaitingOn] are on the row for it). [ViewTable] is read by COLUMN,
// one field per column, so values compare and sort at their head. Same rows,
// same query: only the arrangement differs, which is what a view TYPE is.
const (
	ViewList     ViewType = "list"
	ViewBoard    ViewType = "board"
	ViewCalendar ViewType = "calendar"
	ViewTimeline ViewType = "timeline"
	ViewTable    ViewType = "table"
)

// ViewTypes is that closed set as a value, in the order a strip draws them.
//
// ONE LIST that [ViewType.Valid], `save_work_view`'s `type` enum and the gate
// holding them together all read, so a rendering added here is offered,
// accepted and asserted with no second edit.
var ViewTypes = []ViewType{ViewList, ViewBoard, ViewCalendar, ViewTimeline, ViewTable}

// Container names where an object lives.
type Container struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// The container kinds a view may belong to.
//
// A CLOSED SET, named so another cannot appear by typo. The workspace carries
// an empty id, as [WorkspaceContainer] does in the scope grammar.
const (
	ContainerWorkspace = "workspace"
	ContainerProject   = "project"
	ContainerUnit      = "unit"
	ContainerPerson    = "person"
)

// ValidContainerKind reports whether a view names a real container.
func ValidContainerKind(kind string) bool {
	switch kind {
	case ContainerWorkspace, ContainerProject, ContainerUnit, ContainerPerson:
		return true
	}
	return false
}

// View is a saved query.
//
// Pinning is NOT on it: a pin is personal, so it lives on the person.
type View struct {
	V       int    `json:"v"`
	Version uint64 `json:"version"`

	ID        string            `json:"id"`
	Container Container         `json:"container"`
	Name      string            `json:"name"`
	Type      ViewType          `json:"type"`
	Params    map[string]string `json:"params,omitempty"`

	// Owner empty means a SHARED view; a handle makes it personal.
	Owner     string `json:"owner,omitempty"`
	Protected bool   `json:"protected,omitempty"`
	Default   bool   `json:"default,omitempty"`

	Rank      Rank      `json:"rank,omitempty"`
	Icon      string    `json:"icon,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// DeletedAt and DeletedBy mark a deleted view. The row stays
	// ([Writer.DeleteView]) because every view upsert is version-guarded
	// against it; no read lists, runs or replaces a deleted view.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy string     `json:"deleted_by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Deleted reports whether somebody deleted the view.
func (v View) Deleted() bool { return v.DeletedAt != nil }

// Favorite is one thing a person starred.
type Favorite struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Person is one human's own state: their inbox, their ordering and their pins.
//
// The inbox fields and pins are written ONLY on the person's own behalf; the
// priorities may ALSO be written by a lead for somebody in their line (see
// person.go for why that is three verbs).
//
// THE INBOX FIELDS ARE THE PERSON'S [inbox.Marks], EMBEDDED, so the record
// keeps its keys and their order under the rules every domain's inbox shares.
//
// A lead's priority write wakes its person with `prioritised`, naming the task
// now at the TOP ([Writer.prioritisedWake]), because every [Notify] here is
// task-shaped; [Person.PrioritiesSetBy] says who chose the list.
type Person struct {
	V       int    `json:"v"`
	Version uint64 `json:"version"`

	Handle string `json:"handle"`

	inbox.Marks[Reason]

	Priorities  []string   `json:"priorities,omitempty"`
	PinnedViews []string   `json:"pinned_views,omitempty"`
	Favorites   []Favorite `json:"favorites,omitempty"`

	// PrioritiesSetBy is WHO last set this list when it was not the person,
	// else empty: a lead's authority made visible. The person's own next
	// write clears it, since taking the queue back says they have seen it.
	PrioritiesSetBy string    `json:"priorities_set_by,omitempty"`
	PrioritiesSetAt time.Time `json:"priorities_set_at,omitzero"`

	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// MaxCommitBytes is the design maximum for one record, envelope included.
//
// DERIVED FROM THE MAXIMAL TASK PATCH, not a body edit: rule 2 carries a
// touched collection WHOLE, so the worst commit touches every collection at
// every cap — a 32 KiB body at six-fold JSON escaping (192 KiB), 128 field
// values at 4 KiB (512 KiB), four textareas at 16 KiB (64 KiB), 256 checklist
// items (100 KiB), 192 relations (22.5 KiB), watchers and tags, and a maximal
// notification (120 KiB). TestTheMaximalCommitFitsItsDesignMaximum measures it.
//
// It sizes the applier's fetch, ack-pending window and reorder buffer, and is
// 6.6× inside the broker's payload ceiling (an oversized publish is refused
// client-side) and inside the store's encoded-record refusal.
const MaxCommitBytes = 1_279_262

// KeyMint is a counter value a record took, carried on the record that uses it.
//
// ONE NUMBER, even on the root of a moving subtree: a subtree's membership
// moves while a walk is stopped, so no range can be re-derived later. A
// stopped move is finished by its re-run or the Worklet duty, each on a FRESH
// range ([Writer.followRoot]).
type KeyMint struct {
	// N is the counter value this task took. Its key is "<PROJECT>-<n>"
	// and its rank is the n-th key of the create lattice.
	N uint64 `json:"n"`
}
