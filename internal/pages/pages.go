// Package pages is the engine's own knowledge base: containers, pages, their
// revision history, and the change record a wake is derived from.
//
// # What it is for
//
// A company running Crewlet needs somewhere to write things down, and until
// now that somewhere had to be Confluence. This is the first-party
// alternative, on exactly the terms [internal/tracker] is the tracker's: every
// change is ONE RECORD on an ordered stream, arbitrated at the broker on the
// subject of the object it changes and applied into N identical SQL copies
// with the checkpoint in the same transaction as the rows. It is the state
// log's THIRD domain, and it is refused by the config beside an
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
// Two, and both are excluded from search and from routing. The SKILLS
// container holds tool-skill pages: machinery, and a seat told to read one
// would follow an instruction written for a different phase of a different
// turn. The ROOT container holds the organisation's own pages, starting with
// the Onboarding page every seat reads first. Both are refused as a unit's
// own space by the config loader, and both are named there rather than here
// so an operator can move either.
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
// only one of them.
package pages

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("pages")

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
	// Five hundred and twelve kibibytes, eight times a work item's, because
	// a runbook or a design document genuinely is that long. It is also
	// the number the broker's max_payload has to clear: the embedded
	// server allows 8 MiB, but an external NATS cluster defaults to 1 MiB,
	// so a native knowledge base refuses to start against a broker that
	// could not carry a full page.
	MaxBody = 512 << 10

	// MaxComment bounds one comment on a page.
	MaxComment = 32 << 10

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
// recoverable for thirty days.
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
// UPPER-CASE AND TRIMMED, because a container key is an ADDRESS a person
// types — in a unit's `space:`, in a tool argument, in a URL — and a company
// whose `ENG` and `eng` are two containers is one where every page is filed
// in whichever the author happened to type.
//
// ONE FUNCTION because it was eleven copies and they had ALREADY DRIFTED: six
// upper-cased without trimming and five trimmed first, so a filter built from
// a key with a trailing space matched nothing while the lookup beside it,
// built from the same string, matched the container — a page that was plainly
// there and could not be listed.
func ContainerKey(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

// nowUTC is the default clock.
func nowUTC() time.Time { return time.Now().UTC() }

// ErrInvalid reports a value this knowledge base refuses.
var ErrInvalid = fmt.Errorf("pages: invalid")

// invalid builds a refusal naming the field and what to do.
func invalid(field, why string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(why, args...))
}
