// Package doclet is Doclet, the engine's own knowledge base: containers,
// pages, their revision history, and the change record a wake is derived from.
//
// # What it is for
//
// A company running Crewlet needs somewhere to write things down, and until
// now that somewhere had to be Confluence. This is the first-party
// alternative, on exactly the terms [internal/worklet] is Worklet's: every
// change is ONE RECORD on an ordered stream, arbitrated at the broker on the
// subject of the object it changes and applied into N identical SQL copies
// with the checkpoint in the same transaction as the rows. It is the state
// log's THIRD domain, its second STRICT one (which tells a per-domain
// checkpoint from a per-node one), and it is refused by the config beside an
// `integrations.confluence` block — pages in two places with nothing keeping
// them in step is the cache-with-no-invalidation the whole design is against.
//
// That shape is ADR-0002 — the stream is the write-ahead log, these SQL tables
// are derived from it — and [internal/statelog] is the record's authority. What
// is particular to a wiki is below.
//
// # The three things a wiki has that a tracker does not
//
//   - A TITLE IS AN ADDRESS. People link to pages by name, so a title is
//     unique within its container, and it is what a create ARBITRATES ON:
//     the record's SUBJECT is the address, not the new page's uuid, because
//     two writers must contend for a name and two uuids never would. The
//     title travels as a bounded TOKEN, since a subject is a broker path
//     and a title is prose carrying spaces, dots and wildcards; the applier
//     recomputes it from the payload and REFUSES a record that took one
//     address and claimed another.
//   - A BODY HAS A HISTORY. Every save writes an immutable revision, and the
//     last [RevisionsKept] survive. The head carries a monotonic version, and
//     a save must state the version it edited — the same rule Confluence's
//     version+1 and this repo's own /config 409 enforce, because a wiki's
//     worst failure is silently overwriting somebody's paragraph.
//   - A PAGE IS SEARCHED, not filtered. The applied rows feed the lexical
//     index in [internal/search], and a published page is what an agent's
//     "what do we already know about this" reads.
//
// # The reserved containers
//
// Two, and they are reserved for different reasons. The SKILLS container
// holds tool-skill pages: machinery, excluded from search and from routing,
// and a seat told to read one would follow an instruction written for a
// different phase of a different turn — so an AGENT writes nothing there. The
// ROOT container holds the organisation's own pages, starting with the
// Onboarding page every seat reads first; it is searched and routed like any
// other, and what an agent may not do there is ADD a page, because a new page
// at the top of the company is a person's decision. [Reserved] states both,
// and the store enforces them on every write path. Both are refused as a
// unit's own space by the config loader, and both are named there rather than
// here so an operator can move either.
//
// # The declared spaces
//
// A page is CREATED only in a container the company declares — a unit's or a
// seat's `space:`, the root or the skills container ([Reserved.Declared]) —
// and that, unlike the two rules above, binds a person exactly as it binds an
// agent. A key nothing declares is no space at all: no lead tidies what is
// filed there, no routing reaches it, and a typo of a real key splits a team's
// knowledge in two with nothing to say so. It is the rule a task's create
// holds an undeclared project to. Only a create asks, because every other
// write is to a page already inside a space that was declared when it was
// made; and a store that declares nothing creates nothing, so a surface wired
// without the seam cannot file pages under keys nobody named.
//
// # The two-key sequences this no longer has
//
// Under the coordination bucket this domain grew up on, a page's identity was
// TWO keys — its title claim and the page itself — and no write spanned two
// keys. So a create, a save and a rename were each a SEQUENCE with a window
// between the halves, and each carried its own account of what a crash in that
// window left behind: an orphan claim, an orphan revision above the page's own
// version, an old claim still held. Stepping over that debris needed a GRACE
// RULE — a refusal older than an hour is an orphan, overwrite it — which is a
// rule about time rather than about ordering, and the one shape a reader can
// neither derive nor check.
//
// Adopting the log removed all three, and that is the clearest thing the
// adoption bought. A create is ONE record whose apply writes the title claim,
// the head, the first revision and the history entry in ONE TRANSACTION; so is
// a save, and so is a rename, which takes the new claim and releases the old
// inside the same transaction. There is no half-applied state to name, no
// orphan to step over, and no grace rule anywhere in this package. A crash
// mid-apply rolls the transaction back and the record is re-applied from the
// checkpoint, which is the framework's own guarantee rather than this domain's.
//
// A RENAME IS STILL ITS OWN OPERATION rather than a field of a save, and for a
// reason the transaction does not remove: one record has one subject, and a
// record carrying both an address change and a content change could arbitrate
// only one of them. It stamps the head's `scoped_through` and never its
// `version`, so the page's own expectation is never poisoned.
//
// # Revisions, comments and wakes
//
// REVISION N IS THE BODY AT VERSION N, the newest included: the other reading,
// that a revision holds what was replaced, collides with itself on the first
// save and leaves the current body reachable only through the head. A save
// must state the version it edited, unlike a work item's optional If-Match,
// because no per-field merge makes overwriting prose safe.
//
// A COMMENT DOES NOT SUBSCRIBE ITS COMMENTER, the opposite of the tracker's
// participants rule and deliberate: a page a hundred people remarked on would
// wake a hundred seats when somebody fixes a heading. Only a mention
// subscribes. A remark is its author's: an edit is refused to anybody else,
// and a removal takes the author or a moderator the caller passes as
// [CommentAuthority], since this package holds no chart and no grants.
//
// The parser routes by [internal/routing] as its degenerate case: `mention`
// over `watcher`, and a one-link chain to the container's lead offered only
// for a change [LeadWorthy], so a departed watcher, a lead who is no longer a
// seat and the page's own author are dropped by the tracker's steps rather
// than a hand-kept copy. The applier is what notices a tool-skill page moving:
// there is no page webhook and the change feed drops those changes, so the
// apply, which derives the skill flag, is the only place that sees both an
// arrival and a departure. A record that may wake an agent other than its
// writer says whom the writer worked for ([MutationRecord.WorkingFor],
// ADR-0109).
//
// # Who may write where
//
// The reserved-container rules are the store's, checked on every write path
// (create, save, rename, every comment path) and reading the keys live because
// they are Tier B; as a tool's check alone, a seat refused a create in the
// skills container could still save, rename or comment there. They concern the
// ACTOR, not capability: a person publishing there on purpose is what those
// containers are for. The capability half is the tool layer's, because the
// store exempts every person and holds no permissions: a person's write to a
// skills page is asked `pages.skill.write`, since a tool skill is injected
// into every seat's turn.
//
// # Reads
//
// A listing is read in the order its index holds: heads by container, title
// and id, resumed through `keyset`'s arms, so a late window costs what the
// first does; a history is read newest first in log order. A read reports the
// POSITION it answered at, not a hydration boolean. The newest tool
// skills are the other order ([OrderNewest]), served beside `skills=true`
// alone ([ErrUnservedOrder]) from a partial index, read across a CROSS JOIN
// because joined plainly the planner drove on the heads and sorted. An index no
// reader's plan reaches is a write on every apply on every data node, carried
// in every snapshot, so none is kept (the trash's stays: the reaper's listing
// seeks it, on trash instant and page id): every statement that reads a page table
// is named and planned (`TestEveryIndexServesARegisteredQuery`), which also
// refuses an unregistered reading of a covered table and an index no plan
// reaches. [ContainerKey] is the one canonical form of a container key.
// The tool names are this domain's (`tools.go`), aliased by `agent/builtin`,
// and the read types declare their somebody-else's-words fields
// (`provenance.FieldTag`).
package doclet

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("doclet")

// RevisionsKept is how many revisions of one page survive.
//
// A HUNDRED. It is what a person actually uses a history for — finding when a
// paragraph changed and who changed it, over weeks rather than years — and
// the cost is the bound: a 512 KiB body times a hundred is 50 MB for one
// page, so the number is a storage decision as much as a product one. A page
// edited by an auto-refiner after every turn would otherwise grow one full
// copy per turn, for ever.
const RevisionsKept = 100

// The content caps, in bytes. Refused at the edge naming the field, never
// silently cut: a page truncated mid-sentence is a procedure somebody will
// follow the first half of.
const (
	// MaxTitle bounds a page title.
	//
	// Two hundred and fifty-six. A title is an ADDRESS here — people link
	// by it — so it has to fit on a line, in a breadcrumb and in a link.
	MaxTitle = 256

	// MaxBody bounds a page.
	//
	// Five hundred and twelve kibibytes, sixteen times a work item's, because
	// a runbook or a design document genuinely is that long. A record
	// carries a page's body once, and JSON escapes a byte into at most six,
	// so a page at this size is at most 3 MiB on the wire — inside this
	// log's declared largest record ([PagesMaxRecordBytes]), and so inside
	// the transport's 8 MiB message ceiling. Nothing here checks the broker:
	// every node, knowledge base or not, refuses at connect an external NATS
	// server configured below that ceiling (nats-server's own default is
	// 1 MiB; internal/queue/jetstream).
	MaxBody = 512 << 10

	// MaxComment bounds one comment on a page.
	MaxComment = 32 << 10

	// MaxOpenAsks bounds the open asks a comment reads, and MaxAwaiting the
	// askers it wakes.
	MaxOpenAsks = 64
	MaxAwaiting = 16

	// MaxExcerpt bounds the excerpt a change record carries.
	MaxExcerpt = 600

	// MaxLabels bounds a page's labels.
	MaxLabels = 32

	// MaxLabelLength bounds one label.
	MaxLabelLength = 64

	// MaxWatchers bounds a page's watcher set, for the reason an item's is
	// bounded: every watcher is a wake per change.
	MaxWatchers = 256

	// MaxMessage bounds a save's edit message.
	//
	// Two hundred and fifty-six: a commit-message line, which is what it
	// is for. A save whose message needs more than that is describing the
	// page, and the page is right there.
	MaxMessage = 256
)

// Status is where a page sits.
type Status string

// The statuses. Three, and each is a different answer to "should a reader see
// this": published is the page, draft is somebody's unfinished thought, and
// trashed is deleted as far as any reader is concerned while still being
// recoverable for thirty days. After that the trash reaper purges it
// (internal/trashreap, through [Store.PurgeTrashed]): a trash nothing empties
// keeps every page anybody ever deleted, for ever, in every data node's copy
// and every backup.
const (
	StatusPublished Status = "published"
	StatusDraft     Status = "draft"
	StatusTrashed   Status = "trashed"
)

// Statuses is every status.
func Statuses() []Status { return []Status{StatusPublished, StatusDraft, StatusTrashed} }

// Valid reports whether s is a status this build serves.
func (s Status) Valid() bool { return slices.Contains(Statuses(), s) }

// Readable reports whether a page in this status is one a search may return.
//
// PUBLISHED ONLY. A draft is unfinished and a trashed page is deleted, and
// surfacing either puts content in front of an agent that no person considers
// current — which is worse than returning nothing, because the agent acts on
// it.
func (s Status) Readable() bool { return s == StatusPublished }

// AuthorKind is who wrote something, on the same three values the tracker
// uses and for the same reasons.
type AuthorKind string

// The author kinds.
const (
	AuthorAgent    AuthorKind = "agent"
	AuthorHuman    AuthorKind = "human"
	AuthorOperator AuthorKind = "operator"
)

// AuthorKinds is every kind.
func AuthorKinds() []AuthorKind { return []AuthorKind{AuthorAgent, AuthorHuman, AuthorOperator} }

// Valid reports whether k is a kind this build serves.
func (k AuthorKind) Valid() bool { return slices.Contains(AuthorKinds(), k) }

// ChangeKind is what happened to a page.
type ChangeKind string

// The change kinds.
const (
	ChangeCreated       ChangeKind = "created"
	ChangeSaved         ChangeKind = "saved"
	ChangeRenamed       ChangeKind = "renamed"
	ChangeMoved         ChangeKind = "moved"
	ChangeStatus        ChangeKind = "status"
	ChangeComment       ChangeKind = "comment"
	ChangeCommentEdited ChangeKind = "comment_edited"
	ChangeLabels        ChangeKind = "labels"
	ChangeWatchers      ChangeKind = "watchers"
	ChangeRemoved       ChangeKind = "removed"
)

// ChangeKinds is every kind.
func ChangeKinds() []ChangeKind {
	return []ChangeKind{ChangeCreated, ChangeSaved, ChangeRenamed, ChangeMoved,
		ChangeStatus, ChangeComment, ChangeCommentEdited, ChangeLabels,
		ChangeWatchers, ChangeRemoved}
}

// Valid reports whether k is a kind this build serves.
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds(), k) }

// Moves reports whether a change of kind k moved work: anything but a
// comment, its edit, or who watches.
func (k ChangeKind) Moves() bool {
	return k != ChangeComment && k != ChangeCommentEdited && k != ChangeWatchers
}

// OnboardingTitle is the page every seat's reading chain starts at, in each
// container it appears in.
//
// A TITLE RATHER THAN A LABEL OR A FLAG, because it is a convention a person
// follows when they write the page — nobody has to remember to tick a box —
// and because the chain is walked from the org root down through a unit's own
// container, where a page called anything else is an ordinary page.
const OnboardingTitle = "Onboarding"

// NormalizeTitle is the canonical form of a title, for comparison and for the
// claim key.
//
// WHITESPACE-FOLDED AND CASE-INSENSITIVE, because a title is an ADDRESS: a
// person linking to "Deploy Runbook" and one linking to "deploy  runbook"
// mean the same page, and a container holding both is one where every link is
// a coin flip. The DISPLAYED title keeps whatever the author typed.
func NormalizeTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

// ContainerKey is the canonical form of a container key, for comparison, for
// every query argument and for the subject a record arbitrates on.
//
// UPPER-CASE AND TRIMMED, because a container key is an ADDRESS a person types
// (a unit's `space:`, a tool argument, a URL), and `ENG` and `eng` as two
// containers would file pages by whichever the author typed. ONE FUNCTION:
// copies that upper-cased without trimming and copies that trimmed first
// drifted, so a filter built from a key with a trailing space matched nothing
// while the lookup beside it matched the container.
func ContainerKey(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

// nowUTC is the default clock.
func nowUTC() time.Time { return time.Now().UTC() }

// ErrInvalid reports a value this knowledge base refuses.
var ErrInvalid = fmt.Errorf("doclet: invalid")

// InvalidError is a refusal naming the field and what to do, marked
// [ErrInvalid]. The field and the sentence are held apart so [Sentence] can
// say them without the package prefix (a page tool tells a model this class
// in those words alone; the act route and the human write surface answer it
// in a person's words or in its code's own, ADR-0060).
//
// EXPORTED, FIELDS AND ALL: a refusal crosses the estate router's wire
// (internal/estate), which rebuilds an error TYPE from its exported fields; an
// unexported one arrived as its message alone, so [Sentence] found no refusal.
// Only this package builds one.
type InvalidError struct {
	// Field is the argument refused, as the caller named it.
	Field string
	// Why is what is wrong with it and what to do, without the package
	// prefix.
	Why string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("%v: %s: %s", ErrInvalid, e.Field, e.Why)
}
func (e *InvalidError) Unwrap() error { return ErrInvalid }

// invalid builds a refusal naming the field and what to do.
func invalid(field, why string, args ...any) error {
	return &InvalidError{Field: field, Why: fmt.Sprintf(why, args...)}
}

// Sentence is a refusal this package wrote, as a person reads it: the
// argument it names, as a value, and what to do, without the "doclet:
// invalid:" a Go error opens with, which says where the error came from and
// nothing about what to change. Any other error this package wrote loses the
// "doclet:" it opens with and keeps every word after it; one a caller wrapped
// in context of its own is its whole message. THE FRAMEWORK'S OWN THREE — a
// lost race ([ErrConflict] wraps it), a create of what is there and a write a
// deletion keeps deleted — are told in the state log's words for a person
// ([statelog.Sentence]), as every surface that asks the state log first tells
// them, so a caller asking only this one never prints the framework's words
// after the mark.
func Sentence(err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	var r *InvalidError
	if errors.As(err, &r) && r.Error() == err.Error() {
		return "`" + r.Field + "`: " + r.Why
	}
	return strings.TrimPrefix(err.Error(), "doclet: ")
}

// badParent builds the refusal of a page's `parent_id`, which is both a field
// refusal and the one fact [ErrParent] names.
func badParent(why string, args ...any) error {
	return fmt.Errorf("%w (%w)", invalid("parent_id", why, args...), ErrParent)
}
