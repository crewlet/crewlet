// Package work is the engine's own work tracker: items, their threads, and
// the change record a wake is derived from.
//
// # What it is for
//
// A company running Crewlet needs somewhere to file work, and until now that
// somewhere had to be Jira. The engine mirrored nothing — task state lived in
// the PM tool by design, because a mirror is a cache with no invalidation
// story — and that decision stands: this is not a mirror. It is a FIRST COPY.
// The record here is the only one, so there is nothing to keep in step, and
// the config refuses a native tracker beside an `integrations.jira` block for
// exactly that reason.
//
// # Where the record lives, and why it is not here
//
// Every item is a document in [coord.FamilyWork], because every node has to
// agree on it: a seat's next owner reads the item the previous owner was
// working, and a board is the same board on every node. What this package
// owns is the RULES — which status transitions are legal, who ends up
// watching, what a change record says — and coordination holds the bytes,
// exactly as it holds a sandbox run.
//
// Reads come from [projection], not from coordination: a board filters and
// sorts thousands of items, and a listing over a bucket is O(keys) message
// deliveries. Writes go the other way, compare-and-set against the bucket,
// and never touch the projection — a row this package wrote directly would be
// erased by the next reconcile and look exactly like the engine losing
// somebody's work.
//
// # The write shape, and the two-key sequences
//
// There is no multi-key transaction in the coordination store, so every write
// that touches two keys states its order and what a crash between them
// leaves. This is the same contract [coord.Budgets] states for its charge,
// and it is in the package doc rather than at each call because the
// compensation is what a reader has to know:
//
//	Mint a key      counter CAS, then item Create        a numbering gap, which Jira has too
//	Comment         comment Create, head CAS, change Create   a durable comment whose change key any
//	                                                     projector repairs from the comment's own record
//	Any head write  head CAS, then change Create         a missing change key, repaired the same way
//
// The change key is created LAST and never rewritten, which is what makes it
// safe for the feed to consume: a bucket with history 1 terminates an
// un-acked message when its key is rewritten, so a feed over the head would
// silently lose wakes.
//
// # Bounding hand-off loops
//
// An agent reassigning an item to a colleague is an ownership transfer down a
// chart of known height, not a nested ask — so it does NOT charge against the
// delegation depth cap, which exists to bound recursion and would conflate
// two different things. It is bounded where the truth is instead: on the item
// itself, by [Item.Reassignments], which counts agent-initiated assignee
// changes since the last human touch and resets on any human write. Past
// [ReassignmentBudget] the write is refused naming the field, and the item is
// left for a person.
package work

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("work")

// ReassignmentBudget is how many times agents may hand one item on before a
// person has to look at it.
//
// EIGHT, and the number is the org chart plus slack: a hand-off that walks
// from an individual contributor to the founder passes five tiers in the
// shipped example, and three more allows for a bounce between two seats that
// each think the other owns it. Past that the pattern is not delegation, it
// is two agents disagreeing, and every further hop costs a turn each.
//
// A CONSTANT RATHER THAN CONFIG, deliberately. It is not a knob an operator
// can set from evidence they have — the failure it prevents is invisible
// until the token bill arrives — and a company that genuinely needs a ninth
// hop has an org chart problem this setting would hide. Any human write
// resets the count, so a person unblocking an item hands the agents a fresh
// budget without touching a setting.
const ReassignmentBudget = 8

// The content caps, in bytes. Refused at the edge naming the field, never
// silently cut: an item body truncated mid-sentence is a specification an
// agent will act on the wrong half of.
const (
	// MaxTitle bounds an item's title.
	//
	// Two hundred and fifty-six. A title is a line in a list and a subject
	// in a notification card; past this it is a body somebody pasted into
	// the wrong field, and it wraps to three lines on every board.
	MaxTitle = 256

	// MaxBody bounds an item's description.
	//
	// Sixty-four kibibytes. Big enough for a specification with examples;
	// small enough that a hundred items on a board is megabytes rather
	// than a hundred megabytes, and that the whole body fits comfortably
	// inside a turn's context beside everything else it carries.
	MaxBody = 64 << 10

	// MaxComment bounds one comment.
	//
	// Thirty-two kibibytes: half a body, because a comment is a remark on
	// the item rather than a second specification — and a thread is read
	// whole, so the cap that matters is the thread's, which is this times
	// the number of comments.
	MaxComment = 32 << 10

	// MaxExcerpt bounds the excerpt a change record carries.
	//
	// Six hundred bytes, cut rune-safely. The excerpt is what a
	// notification card renders without reading the item, so it has to
	// carry the gist of a comment; it is NOT the comment, and a change
	// record that duplicated a 32 KiB body would put that body in the
	// change bucket for ever.
	MaxExcerpt = 600

	// MaxLabels bounds how many labels one item carries.
	//
	// Thirty-two. Labels are a filter, and past a handful per item they
	// stop discriminating; the cap is here to stop a loop writing labels
	// rather than to express a taxonomy.
	MaxLabels = 32

	// MaxLabelLength bounds one label.
	MaxLabelLength = 64

	// MaxWatchers bounds an item's watcher set.
	//
	// Two hundred and fifty-six, which is far past any real company's seat
	// count: the cap exists because every watcher is a wake per change,
	// so an unbounded set is an unbounded fan-out from a single comment.
	MaxWatchers = 256

	// MaxLinks bounds an item's outgoing links.
	MaxLinks = 64
)

// Type is what kind of work an item is.
//
// A CLOSED SET, and deliberately the smallest one that carries a hierarchy:
// epic over story over task, with bug beside story and subtask beneath task.
// Custom types were excluded because every one of them is a name for a
// workflow this engine does not have — and an agent reading a type it has no
// rule for treats it as a task anyway.
type Type string

// The item types.
const (
	TypeEpic    Type = "epic"
	TypeStory   Type = "story"
	TypeTask    Type = "task"
	TypeBug     Type = "bug"
	TypeSubtask Type = "subtask"
)

// Types is every type, in hierarchy order.
func Types() []Type { return []Type{TypeEpic, TypeStory, TypeTask, TypeBug, TypeSubtask} }

// Valid reports whether t is a type this build serves.
func (t Type) Valid() bool { return slices.Contains(Types(), t) }

// depth is a type's level in the hierarchy, where a smaller number is higher.
// Bug sits with story, and subtask beneath task.
func (t Type) depth() int {
	switch t {
	case TypeEpic:
		return 0
	case TypeStory, TypeBug:
		return 1
	case TypeTask:
		return 2
	case TypeSubtask:
		return 3
	}
	return -1
}

// CanParent reports whether an item of type t may hold a child of type child.
//
// STRICTLY DOWNWARD, so the hierarchy is a tree of bounded depth rather than
// a graph: a cycle would make "the epic this belongs to" a walk that never
// ends, and every board that rolls children up would hang on it.
func (t Type) CanParent(child Type) bool {
	pd, cd := t.depth(), child.depth()
	return pd >= 0 && cd >= 0 && cd > pd
}

// Status is where an item sits.
//
// A FIXED SET FOR THE WHOLE COMPANY, and this is the decision most likely to
// be questioned. Per-unit named statuses were excluded because a status is
// read by agents in prompts and by the engine in routing rules, and both need
// one vocabulary: a seat told an item is "in Review" on one team and
// "Awaiting QA" on another cannot generalise, and the engine would need a
// per-project mapping to answer "is this done" at all. A team that wants its
// own stages expresses them with labels, which cost nothing to ignore.
type Status string

// The statuses.
const (
	// StatusTriage is where an item created with no assignee lands, and
	// the unit lead is woken for it. It is the state that makes an
	// unassigned item somebody's problem rather than nobody's.
	StatusTriage Status = "triage"

	StatusBacklog    Status = "backlog"
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusInReview   Status = "in_review"

	// StatusBlocked is waiting on something outside the item. Not
	// terminal: blocked work is open work, and a board that hid it would
	// hide the thing most in need of a person.
	StatusBlocked Status = "blocked"

	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
)

// Statuses is every status, in board order.
func Statuses() []Status {
	return []Status{StatusTriage, StatusBacklog, StatusTodo, StatusInProgress,
		StatusInReview, StatusBlocked, StatusDone, StatusCancelled}
}

// Valid reports whether s is a status this build serves.
func (s Status) Valid() bool { return slices.Contains(Statuses(), s) }

// Terminal reports whether an item in this status is closed.
func (s Status) Terminal() bool { return s == StatusDone || s == StatusCancelled }

// Open reports whether an item in this status is still work.
func (s Status) Open() bool { return s.Valid() && !s.Terminal() }

// CloseReason says why a terminal item ended that way.
type CloseReason string

// The close reasons.
const (
	// CloseCompleted is the work was done.
	CloseCompleted CloseReason = "completed"

	// CloseNotPlanned is a decision not to do it. Distinct from completed
	// because a report that counted both as delivery would be a lie, and
	// distinct from cancelled-the-status because the status says the item
	// is closed while this says what a reader should conclude.
	CloseNotPlanned CloseReason = "not_planned"

	// CloseDuplicate names another item in DuplicateOf. It is how a moved
	// item is expressed too: moving between projects is not supported,
	// because the key is immutable and a key that changed project would
	// break every link a person has pasted — so the gesture is a new item
	// in the new project and this one closed pointing at it.
	CloseDuplicate CloseReason = "duplicate"
)

// CloseReasons is every reason.
func CloseReasons() []CloseReason {
	return []CloseReason{CloseCompleted, CloseNotPlanned, CloseDuplicate}
}

// Valid reports whether r is a reason this build serves.
func (r CloseReason) Valid() bool { return slices.Contains(CloseReasons(), r) }

// Priority is how urgent an item is.
type Priority string

// The priorities. None is the ZERO VALUE and a real setting: an item nobody
// has triaged has no priority, which is different from a deliberate "low" —
// and a scheme whose zero value was "medium" would have every item arrive
// claiming a judgement nobody made.
const (
	PriorityNone   Priority = "none"
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// Priorities is every priority, lowest first.
func Priorities() []Priority {
	return []Priority{PriorityNone, PriorityLow, PriorityMedium, PriorityHigh, PriorityUrgent}
}

// Valid reports whether p is a priority this build serves.
func (p Priority) Valid() bool { return slices.Contains(Priorities(), p) }

// Rank orders priorities for a board, urgent first.
func (p Priority) Rank() int {
	at := slices.Index(Priorities(), p)
	if at < 0 {
		return 0
	}
	return at
}

// LinkKind is how two items relate.
//
// THREE KINDS, each earning its place: blocks is a dependency a board can act
// on, duplicates is the close gesture above, and relates_to is the honest
// catch-all that stops people encoding everything else as a comment. Every
// other relationship anyone asked for was one of these three with a different
// name.
type LinkKind string

// The link kinds.
const (
	LinkBlocks     LinkKind = "blocks"
	LinkRelatesTo  LinkKind = "relates_to"
	LinkDuplicates LinkKind = "duplicates"
)

// LinkKinds is every kind.
func LinkKinds() []LinkKind { return []LinkKind{LinkBlocks, LinkRelatesTo, LinkDuplicates} }

// Valid reports whether k is a kind this build serves.
func (k LinkKind) Valid() bool { return slices.Contains(LinkKinds(), k) }

// Inverse is how the other end of this link reads.
//
// DERIVED, NEVER STORED. One end is authored and a projector writes both
// directions, so the pair cannot disagree about which item owns the link —
// which is what happens the moment an edit updates one row and not the other.
func (k LinkKind) Inverse() LinkKind {
	switch k {
	case LinkBlocks:
		return "blocked_by"
	case LinkDuplicates:
		return "duplicated_by"
	}
	return LinkRelatesTo
}

// Link is one typed relationship, from the item that owns it.
type Link struct {
	Kind LinkKind `json:"kind"`
	To   string   `json:"to"`
}

// AuthorKind is who wrote a comment or made a change.
type AuthorKind string

// The author kinds. An OPERATOR is a person acting through the API or the
// dashboard with a token bound to no seat — a pipeline, an automation, an
// operator outside the org chart — and it is a real category rather than a
// fallback: attributing their write to "the engine" would be a lie, and
// refusing it would make the API unusable for the people who run it.
const (
	AuthorAgent    AuthorKind = "agent"
	AuthorHuman    AuthorKind = "human"
	AuthorOperator AuthorKind = "operator"
)

// AuthorKinds is every kind.
func AuthorKinds() []AuthorKind { return []AuthorKind{AuthorAgent, AuthorHuman, AuthorOperator} }

// Valid reports whether k is a kind this build serves.
func (k AuthorKind) Valid() bool { return slices.Contains(AuthorKinds(), k) }

// ChangeKind is what happened, as the change record names it.
type ChangeKind string

// The change kinds. Each is a separate wake with its own card, so the split
// is by what a RECIPIENT needs to be told rather than by which field moved.
const (
	ChangeCreated        ChangeKind = "created"
	ChangeFields         ChangeKind = "fields"
	ChangeStatus         ChangeKind = "status"
	ChangeAssignee       ChangeKind = "assignee"
	ChangeWatchers       ChangeKind = "watchers"
	ChangeLinks          ChangeKind = "links"
	ChangeComment        ChangeKind = "comment"
	ChangeCommentEdited  ChangeKind = "comment_edited"
	ChangeCommentRemoved ChangeKind = "comment_removed"
	ChangeRemoved        ChangeKind = "removed"
)

// ChangeKinds is every kind.
func ChangeKinds() []ChangeKind {
	return []ChangeKind{ChangeCreated, ChangeFields, ChangeStatus, ChangeAssignee,
		ChangeWatchers, ChangeLinks, ChangeComment, ChangeCommentEdited,
		ChangeCommentRemoved, ChangeRemoved}
}

// Valid reports whether k is a kind this build serves.
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds(), k) }

// ValidKey reports whether s is a well-formed item key: a container key, a
// hyphen and a positive number.
//
// The CANONICAL FORM, checked rather than parsed leniently, because this
// string is pasted into chat, typed into a tool call and used as a
// conversation key — and a lenient reader would resolve "eng 42" to ENG-42 on
// one path and to nothing on another.
func ValidKey(s string) bool {
	project, number, ok := SplitKey(s)
	return ok && project != "" && number > 0
}

// SplitKey takes an item key apart.
func SplitKey(s string) (project string, number int, ok bool) {
	at := strings.LastIndexByte(s, '-')
	if at <= 0 || at == len(s)-1 {
		return "", 0, false
	}
	project, digits := s[:at], s[at+1:]
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return "", 0, false
		}
	}
	if len(digits) > 1 && digits[0] == '0' {
		// A leading zero would make ENG-1 and ENG-01 two spellings of one
		// item, and the projection's unique index would accept both.
		return "", 0, false
	}
	n := 0
	for i := range len(digits) {
		n = n*10 + int(digits[i]-'0')
	}
	return project, n, true
}

// FormatKey renders an item key.
func FormatKey(project string, number int) string {
	return fmt.Sprintf("%s-%d", project, number)
}

// ConversationKey is the key a wake about this item carries, so every message
// about one item lands in one conversation ledger.
func ConversationKey(itemKey string) string { return "work:" + itemKey }

// nowUTC is the clock every record is stamped from, so a test can freeze it
// without a package-level variable a parallel test would race on. Callers
// pass a time; this is the default for the ones that do not.
func nowUTC() time.Time { return time.Now().UTC() }
