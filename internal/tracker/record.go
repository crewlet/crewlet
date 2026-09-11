package tracker

import (
	"encoding/json"
	"time"
)

// The objects, and the two shapes a record carries them in.
//
// A small whole-document object — a view, a goal, a sprint, a tag set, a
// person, a project, a generation, an eviction — travels as FULL POST-STATE:
// one document field, one upsert, and no patch semantics to get wrong. A task
// and the field catalogue travel as a TYPED PATCH, because full post-state
// would put a 64 KiB body on the wire for a status flip, and the catalogue is
// 200 KiB edited one field at a time.
//
// The patch's rule is the one the engine's earlier edit struct already
// carried, and it is the right one: a nil field is unchanged, and the POINTER
// is what tells "set this to empty" from "leave it alone" — which a plain
// string cannot. Two rules travel with it:
//
//  1. A named scalar present in the patch carries its COMPLETE new value,
//     never an excerpt and never a diff. The 600-byte excerpts live in the
//     notification, where a wake prompt is what they are for.
//  2. A collection the write TOUCHES is carried whole; one it does not touch
//     is absent. That is what makes a record able to rebuild the row — a
//     32-entry delta map could not represent a write touching 128 field
//     values at all — and it is affordable because every collection is
//     capped.
//
// And no cap on the patch is tighter than the record's own: the display cap on
// a notification must never govern what a mutation may carry.

// The collection caps. Each is refused at WRITE naming the field, never cut:
// a value silently truncated is a value a person will look for later.
const (
	// MaxTitle and MaxBody bound a task's own text.
	MaxTitle = 256
	MaxBody  = 64 << 10

	// MaxCommentBody is a comment's.
	MaxCommentBody = 32 << 10

	// MaxWatchers is the routing cap and the fan-out cap at once: an
	// explicit sixty-fifth watch is refused, and an automatic one is
	// skipped with a warning.
	MaxWatchers = 64

	// MaxCollaborators is deliberately far smaller than the watcher cap:
	// a collaborator is somebody doing the work, and a task with more than
	// eight is a task nobody owns.
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
	// MaxDependents is the OTHER end, and it is the cap that bounds a
	// status write's declared scope: a group change on a blocker rewrites
	// two columns on every dependent, so the record enumerates at most
	// this many object terms and never needs a covering one.
	MaxDependents = 64

	// MaxChecklists, MaxChecklistItems and MaxChecklistItemsTotal bound
	// the checklist tree.
	MaxChecklists          = 16
	MaxChecklistItems      = 64
	MaxChecklistItemsTotal = 256

	// MaxSprintStays bounds a task's sprint history; past it the OLDEST
	// closed stay is dropped and the count kept, so a report can say how
	// many it is not showing.
	MaxSprintStays = 16

	// MaxFormerKeys bounds what a task DISPLAYS. Resolution is unbounded:
	// every former key also has an alias row, and the applier never
	// deletes one on a task apply.
	MaxFormerKeys = 16

	// MaxMentions bounds a comment's resolved mentions.
	MaxMentions = 32

	// MaxDepth is the subtask depth, and MaxDescendants the subtree size
	// under one root.
	MaxDepth       = 7
	MaxDescendants = 1000

	// BodyRevisionsKept is how many past bodies a task keeps.
	//
	// THE PRUNE RIDES THE COMMIT as an explicit list of retired versions,
	// so every node deletes exactly the same rows at exactly the same
	// position and no node deletes a durable row on its own authority.
	BodyRevisionsKept = 100

	// MaxReferencesPerBody bounds the key scan the applier runs on every
	// body and comment, so a pathological body is 64 lookups on every node
	// rather than five thousand.
	MaxReferencesPerBody = 64
)

// Spend is a task's running totals, for ever.
//
// DERIVED BY THE APPLIER from the turn commits themselves rather than
// transmitted: the insert of the turn row is what gates the addition, in the
// same transaction, so the total is a function of the applied records and
// cannot disagree with them — and a redelivery cannot double-count.
type Spend struct {
	Turns      int `json:"turns,omitempty"`
	Rounds     int `json:"rounds,omitempty"`
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	WallMs     int `json:"wall_ms,omitempty"`
	Tokens     int `json:"tokens,omitempty"`
}

// Tombstone is an operator's removal.
//
// A TOMBSTONED TASK IS FROZEN — no comment, body, field or relation of it can
// change — which is what makes a removal an entirely local decision with no
// walk behind it. There is no expiry and no purge horizon: a restore works at
// any age.
type Tombstone struct {
	By          string     `json:"by"`
	Kind        AuthorKind `json:"kind"`
	At          time.Time  `json:"at"`
	RemovedWith *string    `json:"removed_with,omitempty"`
}

// SprintStay is one interval a task spent in one sprint.
//
// EVERY SPRINT REPORT IS DERIVED FROM THESE — committed, added, removed,
// remaining and velocity — which is why they travel in the record rather than
// being recomputed from a status history that does not carry them.
type SprintStay struct {
	Sprint   int        `json:"sprint"`
	From     time.Time  `json:"from"`
	To       *time.Time `json:"to,omitempty"`
	RolledTo *int       `json:"rolled_to,omitempty"`
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
	// RelationPage names a knowledge-base page, with no write to the
	// pages family at all.
	RelationPage RelationKind = "page"
)

// Relation is one edge, authored on ONE end.
type Relation struct {
	Kind      RelationKind `json:"kind"`
	Other     string       `json:"other"`
	Note      string       `json:"note,omitempty"`
	CreatedBy string       `json:"created_by,omitempty"`
	CreatedAt time.Time    `json:"created_at,omitzero"`

	// OneSided marks an edge whose mirror was never written, and
	// OneSidedFinal one whose mirror was refused permanently — a cap, a
	// tombstone, or a blocker that is not there. The first is a repair the
	// duty retries; the second is an attention flag a person resolves.
	OneSided      bool `json:"one_sided,omitempty"`
	OneSidedFinal bool `json:"one_sided_final,omitempty"`
}

// ChecklistItem is one line of a checklist.
type ChecklistItem struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Done     bool    `json:"done,omitempty"`
	Assignee string  `json:"assignee,omitempty"`
	Parent   *string `json:"parent,omitempty"`
	Order    int     `json:"order,omitempty"`

	// PromotedTo names the subtask this item became, which is what renders
	// it struck through with the new key rather than deleting it.
	PromotedTo *string `json:"promoted_to,omitempty"`
}

// Checklist is a group of items. It mints no object and appears on no board.
type Checklist struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Items []ChecklistItem `json:"items,omitempty"`
}

// Task is the object almost every record is about.
//
// Its VERSION is the composed (generation << 40) | stream sequence of the last
// arbitrated commit on its own subject — the same integer space as the
// domain's checkpoint — and ScopedThrough is the highest record that wrote
// this row FROM ANOTHER SUBJECT. The two are separate because a record may
// bump the version of objects on its own subject and no others: stamping a
// neighbour's version from another subject would make that neighbour's next
// write form an expectation the broker refuses, for ever.
type Task struct {
	V             int    `json:"v"`
	ID            string `json:"id"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`

	Key        string   `json:"key"`
	FormerKeys []string `json:"former_keys,omitempty"`
	Project    string   `json:"project"`

	// FiledUnit is IMMUTABLE and is a RECORD OF WHAT WAS TRUE: nothing
	// rewrites it, and it may legitimately name a unit the chart no longer
	// has, or a spelling it no longer uses.
	FiledUnit string `json:"filed_unit,omitempty"`
	// RoutingUnit is the mutable half — whose lead hears about this task
	// NOW — and is the only unit field any write touches.
	RoutingUnit string `json:"routing_unit,omitempty"`

	Sprint             *int         `json:"sprint,omitempty"`
	SprintHistory      []SprintStay `json:"sprint_history,omitempty"`
	SprintStaysDropped int          `json:"sprint_stays_dropped,omitempty"`

	// Parent and Depth: Depth is a HINT. The applier derives the real
	// depth from the closure and enforces the cap against the subtree's
	// height, because two concurrent re-parents on two nodes can form a
	// tree no single write could see.
	Parent *string `json:"parent,omitempty"`
	Depth  int     `json:"depth,omitempty"`

	Type  string `json:"type"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`

	// BodyVersion, BodyAuthor and BodyAt say who wrote the CURRENT body,
	// and are copied into the revision the next write replaces — so every
	// revision's bytes are a pure function of the state it replaced.
	BodyVersion int       `json:"body_version,omitempty"`
	BodyAuthor  string    `json:"body_author,omitempty"`
	BodyAt      time.Time `json:"body_at,omitzero"`

	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group"`

	// StatusEnteredAt is the EFFECTIVE instant — the closed form over
	// every row about this task at or below this position — rather than
	// the writer's clock, so two nodes at one checkpoint compute the same
	// value and a duration never depends on whose clock ran fast.
	StatusEnteredAt time.Time `json:"status_entered_at,omitzero"`

	Priority Priority `json:"priority"`
	Rank     Rank     `json:"rank,omitempty"`

	Reporter      string   `json:"reporter,omitempty"`
	Assignee      string   `json:"assignee,omitempty"`
	Collaborators []string `json:"collaborators,omitempty"`

	// Watchers is the set and Muted the subtraction, and BOTH travel:
	// carrying only the difference would make "not a watcher" and
	// "watching but muted" the same row, and a replay would silently
	// re-add every unwatched person on the next mention.
	Watchers []string `json:"watchers,omitempty"`
	Muted    []string `json:"muted,omitempty"`

	Tags []string `json:"tags,omitempty"`

	// Fields are keyed by field ID ONLY — agents type the slug, and the
	// write path resolves it — so a rename or a re-declaration never
	// re-points a stored value.
	Fields map[string]json.RawMessage `json:"fields,omitempty"`

	Relations  []Relation  `json:"relations,omitempty"`
	Dependents []string    `json:"dependents,omitempty"`
	Checklists []Checklist `json:"checklists,omitempty"`

	StartAt   *time.Time `json:"start_at,omitempty"`
	DueAt     *time.Time `json:"due_at,omitempty"`
	DueAllDay bool       `json:"due_all_day,omitempty"`

	EstimateMinutes int     `json:"estimate_minutes,omitempty"`
	Points          float64 `json:"points,omitempty"`
	Spend           Spend   `json:"spend,omitzero"`

	// DoneAt and ClosedAt are stamped BY GROUP rather than by slug: the
	// first entry into the done group — done OR cancelled — sets the
	// first, and the first entry into closed sets the second. Both are
	// cleared on a reopen, so every "recent" filter agrees with the task's
	// actual state.
	DoneAt   *time.Time `json:"done_at,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`

	Archived bool `json:"archived,omitempty"`

	// Merging is true while this task's merge walk is running. See
	// [TaskPatch.Merging].
	Merging    bool       `json:"merging,omitempty"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	ArchivedBy string     `json:"archived_by,omitempty"`

	Removed *Tombstone `json:"removed,omitempty"`

	// ChangeSeq counts commits; Reassignments is the budget counter,
	// charged only when an AGENT changes the assignee and reset by any
	// human or operator write — because a hand-off is an ownership
	// transfer down a chart of known height, not a nested ask.
	ChangeSeq     int `json:"change_seq,omitempty"`
	Reassignments int `json:"reassignments,omitempty"`

	// PolicyStamp records which policy version the last write validated
	// against.
	PolicyStamp string `json:"policy_stamp,omitempty"`

	// CreatedAt and UpdatedAt are the AUTHORED instants. Their skew
	// against the broker's own stored value is published as a number
	// rather than corrected, because differencing a CLAMPED instant
	// against a clock reports a large skew on whichever node wrote second
	// and names it for its neighbour's error.
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
// IT HAS NO VERSION AND NO SUBJECT: a comment is a mutation OF the task, so it
// is arbitrated by the task's own version. Its id is derived from the task,
// the turn and the body when it is written from a turn, so a retried turn
// completes rather than duplicating.
type Comment struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`
	Body       string     `json:"body"`

	// Mentions are resolved at WRITE — a unit to its effective lead, and
	// the assignee and watcher shorthands expanded — because a mention
	// resolved at read time would name whoever holds the role later.
	Mentions []string `json:"mentions,omitempty"`

	ReplyTo *string `json:"reply_to,omitempty"`

	// Ask is set only at creation: a comment becomes a question when it is
	// written, and turning an old remark into one retroactively would wake
	// somebody for a conversation that has moved on.
	Ask     string  `json:"ask,omitempty"`
	Answers *string `json:"answers,omitempty"`

	Resolved   bool       `json:"resolved,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`

	// Removed blanks the body and KEEPS the row, so replies still resolve
	// against something.
	Removed bool `json:"removed,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// BodyRevision is the body a task held at one version, carried in the same
// commit that replaces it.
//
// NOT AN OBJECT OF ITS OWN: there is no separate subject, no create-only race
// and nothing to repair, because the revision and the new body land in one
// transaction on every node.
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
// NEVER DELETED BY A TASK APPLY and never lowered from current by a later one:
// a former key must go on resolving for the life of the deployment, because it
// is pasted into chat and typed into tool calls.
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
// THE OBJECT A RANK MOVE MUTATES IS THE PROJECT'S ORDER — a total order over
// its tasks that no single task owns and no single task's version can protect
// — which is why it has a subject of its own rather than riding one of the
// tasks it moves.
type RankOrder struct {
	V             int    `json:"v"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`
	Project       string `json:"project"`

	// Placements is one to sixty-four moves in one record.
	Placements []Placement `json:"placements"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Placement is one task's new position.
type Placement struct {
	Task string `json:"task"`
	Rank Rank   `json:"rank"`
}

// Eviction is a node's eviction or its readmission.
//
// ITS VERSION IS PINNED AT ONE FROM THE FIRST RELEASE AND FOR EVER, because
// this record INSTALLS A GATE: a build that cannot decode a gate must not
// apply anything above it. The shape evolves additively — an old reader drops
// an unknown key — and a semantic change takes a new record kind rather than a
// version bump.
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
// Two operators deriving the same number race there and exactly one wins,
// which is first-writer-wins used for the one thing it is perfectly suited to.
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
// THE POINTER IS WHAT TELLS "set this to empty" FROM "leave it alone", which a
// plain string cannot — and every collection here is a POINTER TO A SLICE for
// the same reason: an absent tag list means the write did not touch tags, and
// an empty one means it cleared them.
type TaskPatch struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`

	// BodyRevision and Retires ride a body write: the revision the write
	// replaced, and the versions its prune removed. THE PRUNE IS ON THE
	// COMMIT so every node deletes exactly the same rows at exactly the
	// same position, rather than each deciding for itself.
	BodyRevision *BodyRevision `json:"body_revision,omitempty"`
	Retires      []int         `json:"retires,omitempty"`

	Status      *Status   `json:"status,omitempty"`
	Priority    *Priority `json:"priority,omitempty"`
	Type        *string   `json:"type,omitempty"`
	Assignee    *string   `json:"assignee,omitempty"`
	RoutingUnit *string   `json:"routing_unit,omitempty"`
	Parent      *string   `json:"parent,omitempty"`
	Sprint      *int      `json:"sprint,omitempty"`
	Project     *string   `json:"project,omitempty"`

	// Mint is the counter value this record took, and THE ONLY WAY A
	// PATCH MAY WRITE A KEY OR A RANK.
	//
	// # Why neither is a plain field
	//
	// A key is what people paste into chat and a rank is a position in a
	// total order no single task owns, so both have exactly two producers
	// (D144, as D156 restates it): a record carrying a counter value it
	// minted in that project, and a record on that project's rankorder
	// subject. A `Key *string` on this struct is a third — an ordinary
	// edit that can name any key it likes, including one another task
	// holds — and a `Rank *Rank` is a fourth that races every drag.
	//
	// Stated as the MINT, the rule is in the type: the applier derives
	// both from the value, so a patch that did not mint one cannot write
	// either, and a patch that did writes exactly what the counter's own
	// arbitration gave it.
	Mint *KeyMint `json:"mint,omitempty"`

	StartAt   *time.Time `json:"start_at,omitempty"`
	DueAt     *time.Time `json:"due_at,omitempty"`
	DueAllDay *bool      `json:"due_all_day,omitempty"`

	EstimateMinutes *int     `json:"estimate_minutes,omitempty"`
	Points          *float64 `json:"points,omitempty"`

	Archived *bool      `json:"archived,omitempty"`
	Removed  *Tombstone `json:"removed,omitempty"`

	// Merging is set while a merge's child walk runs and cleared by its
	// last append. It is what makes a duplicate visibly MID-MERGE rather
	// than silently half-merged, and what the duty selects on to finish a
	// walk whose holder died.
	Merging *bool `json:"merging,omitempty"`

	// Reassignments is the hand-off counter this write leaves behind,
	// decided by the WRITER inside its own snapshot — see
	// [Writer.chargeHandOff]. Carried as a value rather than derived at
	// apply time because a counter two nodes derive independently is a
	// counter two nodes can disagree about.
	Reassignments *int `json:"reassignments,omitempty"`

	// The collections, carried WHOLE when touched.
	Collaborators *[]string                   `json:"collaborators,omitempty"`
	Watchers      *[]string                   `json:"watchers,omitempty"`
	Muted         *[]string                   `json:"muted,omitempty"`
	Tags          *[]string                   `json:"tags,omitempty"`
	Fields        *map[string]json.RawMessage `json:"fields,omitempty"`
	Relations     *[]Relation                 `json:"relations,omitempty"`
	Dependents    *[]string                   `json:"dependents,omitempty"`
	Checklists    *[]Checklist                `json:"checklists,omitempty"`
	SprintHistory *[]SprintStay               `json:"sprint_history,omitempty"`
	FormerKeys    *[]string                   `json:"former_keys,omitempty"`

	// Comment rides a task write, because a comment is a mutation of the
	// task and shares its arbitration.
	Comment *Comment `json:"comment,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// FieldType is a custom field's type.
type FieldType string

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
	// MaxFieldsPerDocument is the cap on ONE declaring document. Fields
	// are declared at the workspace and on a project, so a task's
	// effective union is at most twice this BY CONSTRUCTION — which is
	// what removes the declaration-time union check a deeper hierarchy
	// would have needed.
	MaxFieldsPerDocument = 64

	// MaxTypes is the workspace type catalogue's cap.
	MaxTypes = 64

	// MaxOptions is one field's options and MaxOptionsPerDocument the
	// whole document's, because a hundred fields at the per-field cap
	// would be a document nothing can encode.
	MaxOptions            = 128
	MaxOptionsPerDocument = 2048

	// MaxTagsPerProject is the tag set's cap.
	MaxTagsPerProject = 512

	// MaxGoalOwners, MaxGoalMembers, MaxGoalTargets, MaxGoalUpdates and
	// MaxTasksPerTarget bound a goal.
	MaxGoalOwners     = 8
	MaxGoalMembers    = 32
	MaxGoalTargets    = 32
	MaxGoalUpdates    = 100
	MaxTasksPerTarget = 64

	// MaxViewParamsBytes and MaxViewParamKeys bound a saved view's query;
	// MaxViewName bounds its tab label at half a task's title, because a
	// name that does not fit its strip is one nobody can tell from its
	// neighbour.
	MaxViewParamsBytes = 32 << 10
	MaxViewParamKeys   = 32
	MaxViewName        = 128

	// MaxInboxEntries bounds each of a person's three lists. Entries at or
	// below the seen-through position are pruned on every write, which is
	// what keeps the object small rather than a cap that discards.
	MaxInboxEntries = 256

	// MaxPriorities and MaxPinnedViews bound a person's own ordering.
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
type FieldConfig struct {
	Options   []Option `json:"options,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Precision int      `json:"precision,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Time      bool     `json:"time,omitempty"`
	Progress  string   `json:"progress,omitempty"`
	Tracking  []string `json:"tracking,omitempty"`
	Project   string   `json:"project,omitempty"`
	Multi     bool     `json:"multi,omitempty"`
	Rollup    *Rollup  `json:"rollup,omitempty"`
}

// FieldDef is one custom field's declaration.
//
// Values are keyed by ID and agents type the SLUG, which is what lets a field
// MOVE between the workspace and a project keeping its id — so every stored
// value survives a move that changes where the field is declared.
type FieldDef struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Type        FieldType `json:"type"`
	AppliesTo   []string  `json:"applies_to,omitempty"`
	Required    bool      `json:"required,omitempty"`

	// RequiredInSubtasks decides INSTEAD of Required for a task with a
	// parent, defaulting to false — ClickUp's two separate toggles, and
	// the default that keeps a required field from blocking every
	// checklist item promoted into a subtask.
	RequiredInSubtasks bool `json:"required_in_subtasks,omitempty"`

	Default json.RawMessage `json:"default,omitempty"`
	Config  FieldConfig     `json:"config,omitzero"`

	// Archived is ONE-WAY: values stay on their tasks, leave the value
	// table, and a restored field is a NEW declaration — because a field
	// that came back with its old id would silently re-admit values
	// validated against a definition nobody has seen for a year.
	Archived       bool   `json:"archived,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
	HideFromAgents bool   `json:"hide_from_agents,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`

	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Ident and Label make a field resolvable by the shared three-tier rule.
func (f FieldDef) Ident() string { return f.Slug }

// Label is what a person reads.
func (f FieldDef) Label() string { return f.Name }

// TaskType is a classification, and nothing more.
//
// It changes no statuses — there is one set — and scopes fields only through
// AppliesTo. Each carries a description an agent chooses BY, because a type is
// chosen by meaning exactly as a status is.
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
	Label       string    `json:"label"`
	Color       string    `json:"color,omitempty"`
	Description string    `json:"description,omitempty"`
	Archived    bool      `json:"archived,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
}

// Ident is the tag's slug.
func (t Tag) Ident() string { return t.Slug }

// LabelText is what a person reads. NOT named Label, because the field is.
func (t Tag) LabelText() string { return t.Label }

// TypeCatalogue and FieldCatalogue are the two workspace catalogues, on two
// subjects — so a type edit never contends with a field edit.
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

// SprintPolicy is how a project runs sprints, and nil means it runs none.
type SprintPolicy struct {
	LengthDays   int          `json:"length_days"`
	StartWeekday time.Weekday `json:"start_weekday"`

	// StartMinutes is minutes after midnight in the company's ONE
	// timezone. A number rather than a time, because the value is a
	// calendar boundary and not an instant.
	StartMinutes int  `json:"start_minutes,omitempty"`
	Ahead        int  `json:"ahead"`
	AutoStart    bool `json:"auto_start,omitempty"`
	AutoRoll     bool `json:"auto_roll"`

	// ArchiveAfter is zero for off. The CLOSE is not a policy value at
	// all: every sprint closes when its end arrives.
	ArchiveAfter int       `json:"archive_after,omitempty"`
	Next         int       `json:"next"`
	NameFormat   string    `json:"name_format,omitempty"`
	Measure      string    `json:"measure,omitempty"`
	PointScale   []float64 `json:"point_scale,omitempty"`

	Capacity map[string]Capacity `json:"capacity,omitempty"`
}

// Capacity is one person's sprint capacity.
type Capacity struct {
	Points      float64 `json:"points,omitempty"`
	EstimateMin int     `json:"estimate_min,omitempty"`
}

// Project is a container's identity, policy and active-sprint pointer.
type Project struct {
	V             int    `json:"v"`
	Version       uint64 `json:"version"`
	ScopedThrough uint64 `json:"scoped_through,omitempty"`

	Key string `json:"key"`

	// Name, Purpose and Unit are CHART-OWNED: written and rewritten only
	// by the chart apply, under the epoch guard below. A project genuinely
	// can move between units, so Unit is not immutable — but it is not a
	// field a tool writes either.
	Name       string `json:"name"`
	Purpose    string `json:"purpose,omitempty"`
	Unit       string `json:"unit,omitempty"`
	ChartEpoch int64  `json:"chart_epoch,omitempty"`

	Fields          []FieldDef `json:"fields,omitempty"`
	DefaultAssignee string     `json:"default_assignee,omitempty"`

	Sprints *SprintPolicy `json:"sprints,omitempty"`

	// ActiveSprint is THE pointer, arbitrated by this object's own
	// version, and the authority for "is another sprint running". Writing
	// it alone leaves UpdatedAt untouched and carries no notification.
	ActiveSprint *int `json:"active_sprint,omitempty"`

	// PolicyVersion moves on a fields or sprint-policy edit — NOT on
	// tags, and NOT on the pointer.
	PolicyVersion int `json:"policy_version,omitempty"`

	Archived  bool      `json:"archived,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// SprintState is where a sprint is in its one-way life.
type SprintState string

const (
	SprintFuture SprintState = "future"
	SprintActive SprintState = "active"
	SprintClosed SprintState = "closed"
)

// Sprint is a per-project record whose SUBJECT IS ITS IDENTITY.
//
// A mint is therefore a create-only append at an expectation of zero, and two
// nodes minting number seven collide harmlessly at the broker — there is
// nothing to repair and no claim class at all.
type Sprint struct {
	V       int    `json:"v"`
	Version uint64 `json:"version"`

	Project string `json:"project"`
	Number  int    `json:"number"`
	Name    string `json:"name"`
	Goal    string `json:"goal,omitempty"`

	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`

	// State is ONE-WAY and the record enforces it: a start moves future to
	// active and refuses any other state, so a closed sprint can never be
	// restarted whatever a project's pointer says.
	State    SprintState `json:"state"`
	ClosedAt *time.Time  `json:"closed_at,omitempty"`
	ClosedBy string      `json:"closed_by,omitempty"`

	OpenAtClose int `json:"open_at_close,omitempty"`

	// RolloverTo is EMPTY while the spillover is pending, which is what
	// makes "pending" a state rather than an absence somebody has to
	// interpret.
	RolloverTo   string `json:"rollover_to,omitempty"`
	RolloverDone bool   `json:"rollover_done,omitempty"`

	Archived  bool      `json:"archived,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
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

// ViewType is the three renderings, and there are no others.
type ViewType string

const (
	ViewList     ViewType = "list"
	ViewBoard    ViewType = "board"
	ViewCalendar ViewType = "calendar"
)

// Container names where an object lives.
type Container struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// The container kinds a view or a goal may belong to.
//
// A CLOSED SET, named so a fourth cannot appear by typo — which is the same
// reason [CatalogueTypes] and [CatalogueFields] are named. The workspace is
// the top of the company and carries an empty id, exactly as
// [WorkspaceContainer] does in the scope grammar.
const (
	ContainerWorkspace = "workspace"
	ContainerProject   = "project"
	ContainerUnit      = "unit"
	ContainerPerson    = "person"
)

// ValidContainerKind reports whether a view or goal names a real container.
func ValidContainerKind(kind string) bool {
	switch kind {
	case ContainerWorkspace, ContainerProject, ContainerUnit, ContainerPerson:
		return true
	}
	return false
}

// View is a saved query.
//
// Pinning is deliberately NOT on it: a pin is personal, so it lives on the
// person — otherwise one person's pin would be everybody's.
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

	Extra map[string]json.RawMessage `json:"-"`
}

// GoalUpdate is one progress note on a goal.
type GoalUpdate struct {
	At     time.Time `json:"at"`
	Author string    `json:"author"`
	Health string    `json:"health,omitempty"`
	Text   string    `json:"text,omitempty"`
}

// GoalTarget is one measurable outcome.
type GoalTarget struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Start    float64  `json:"start,omitempty"`
	Goal     float64  `json:"goal,omitempty"`
	Current  float64  `json:"current,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Tasks    []string `json:"tasks,omitempty"`
	Projects []string `json:"projects,omitempty"`
	Done     bool     `json:"done,omitempty"`
}

// Goal is an outcome with targets, carried as full post-state.
type Goal struct {
	V       int    `json:"v"`
	Version uint64 `json:"version"`

	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Owners      []string `json:"owners"`
	Members     []string `json:"members,omitempty"`
	Group       string   `json:"group,omitempty"`

	StartAt *time.Time `json:"start_at,omitempty"`
	DueAt   *time.Time `json:"due_at,omitempty"`

	Health  string       `json:"health,omitempty"`
	Updates []GoalUpdate `json:"updates,omitempty"`
	Targets []GoalTarget `json:"targets,omitempty"`

	Archived  bool      `json:"archived,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// InboxEntry is one item in a person's inbox, with the position it was at.
type InboxEntry struct {
	RecordID string     `json:"record_id"`
	Position uint64     `json:"position"`
	Until    *time.Time `json:"until,omitempty"`
}

// Favorite is one thing a person starred.
type Favorite struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Person is one human's own state: their inbox, their ordering and their pins.
//
// The inbox fields and the pins are written ONLY on behalf of the person whose
// they are; the priorities may ALSO be written by a lead for somebody in their
// line, which is the one authority here that reaches across people. See
// person.go for why that is three verbs rather than one.
//
// A LEAD'S PRIORITY WRITE IS STAMPED rather than notified. Every [Notify] this
// domain carries is task-shaped — its [Snapshot] is a key, a project and a
// title — so a person record has no card to render and a notification attached
// to one would reach nobody. What makes the authority visible instead is
// [Person.PrioritiesSetBy]: a person who starts the day on work they did not
// choose can see who chose it.
type Person struct {
	V       int    `json:"v"`
	Version uint64 `json:"version"`

	Handle string `json:"handle"`

	// Generation is anchored to the stream's own identity, and it is the
	// THIRD belt behind the identity refusal and the wrong-stream read
	// refusal. It stays because the failure it was written for — a
	// founder's inbox silently reading empty for ever — is one nobody
	// would report as a bug.
	Generation string `json:"generation,omitempty"`

	// SeenThrough is a POSITION TRIPLE rather than a number, because a
	// number from a dead sequence space compares as if it were current.
	SeenThrough Position `json:"seen_through,omitzero"`

	Read    []InboxEntry `json:"read,omitempty"`
	Unread  []InboxEntry `json:"unread,omitempty"`
	Snoozed []InboxEntry `json:"snoozed,omitempty"`

	PrimaryReasons []Reason   `json:"primary_reasons,omitempty"`
	Priorities     []string   `json:"priorities,omitempty"`
	PinnedViews    []string   `json:"pinned_views,omitempty"`
	Favorites      []Favorite `json:"favorites,omitempty"`

	// PrioritiesSetBy is WHO last set this list when it was not the person
	// themselves, and empty when it was. It is how a lead's authority is
	// made visible — see the type doc — and it is cleared by the person's
	// own next write, because taking your queue back is the gesture that
	// says you have seen it.
	PrioritiesSetBy string    `json:"priorities_set_by,omitempty"`
	PrioritiesSetAt time.Time `json:"priorities_set_at,omitzero"`

	UpdatedAt time.Time `json:"updated_at"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Position is where a person's inbox has been read to.
//
// THE TRIPLE, not a bare sequence: a recreated stream restarts sequences at
// one, so a stored number from before it compares as current and an inbox
// reads "nothing unread" for ever.
type Position struct {
	Stream     string `json:"stream"`
	Generation uint32 `json:"generation"`
	Seq        uint64 `json:"seq"`
}

// MaxCommitBytes is the design maximum for one record, envelope included.
//
// DERIVED FROM THE MAXIMAL TASK PATCH rather than from a body edit, and the
// difference is 3×: rule 2 carries a touched collection WHOLE and no cap on
// the patch is tighter than the record's own, so the worst commit is a patch
// that touches every collection at every cap — a 64 KiB body at six-fold JSON
// escaping (384 KiB) plus 128 field values at 4 KiB (512 KiB) plus four
// textareas at 16 KiB (64 KiB) plus 256 checklist items (100 KiB) plus 192
// relations (22.5 KiB) plus watchers, tags and sprint stays, ≈ 1.06 MiB, plus
// a maximal notification at 120 KiB.
//
// It is 6.6× inside the broker's own payload ceiling, so an oversized publish
// is refused client-side rather than closing the connection, and inside the
// store's own encoded-record refusal.
const MaxCommitBytes = 1_279_262

// KeyMint is a counter value a record took, carried on the record that uses it.
//
// # Why the RANGE rides the record and is not recomputed
//
// A cross-project move re-keys a whole subtree from one range mint, and the
// base is NOT recoverable afterwards: by the time a duty completes an
// abandoned walk, other creates have advanced the counter, so a duty that
// recomputed the base would assign a different key to the same descendant on a
// different node. The ordering by (depth, id) fixes the ORDER; only the base on
// the record fixes the ORIGIN — which is what makes the walk's completion a
// pure function of the record rather than of when it runs.
type KeyMint struct {
	// N is the counter value this task took. Its key is "<PROJECT>-<n>"
	// and its rank is the n-th key of the create lattice.
	N uint64 `json:"n"`

	// Base and Length describe the whole range, on the ROOT record of a
	// moving subtree alone. Zero on every other mint.
	Base   uint64 `json:"base,omitempty"`
	Length int    `json:"length,omitempty"`
}
