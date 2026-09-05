// Package pages is the engine's own knowledge base: containers, pages, their
// revision history, and the change record a wake is derived from.
//
// # What it is for
//
// A company running Crewlet needs somewhere to write things down, and until
// now that somewhere had to be Confluence. This is the first-party
// alternative, on exactly the terms [internal/work] is the tracker's: the
// record here is the only copy, held in [coord.FamilyPages], projected into
// every node for reading, and refused by the config beside an
// `integrations.confluence` block — pages in two places with nothing keeping
// them in step is the cache-with-no-invalidation the whole design is against.
//
// # The three things a wiki has that a tracker does not
//
//   - A TITLE IS AN ADDRESS. People link to pages by name, so a title is
//     unique within its container and claimed first-writer-wins on its own
//     key. A rename releases the old claim and takes a new one, in that
//     order, so two pages can never share a name.
//   - A BODY HAS A HISTORY. Every save writes an immutable revision, and the
//     last [RevisionsKept] survive. The head carries a monotonic version, and
//     a save must state the version it edited — the same rule Confluence's
//     version+1 and this repo's own /config 409 enforce, because a wiki's
//     worst failure is silently overwriting somebody's paragraph.
//   - A PAGE IS SEARCHED, not filtered. The projection feeds
//     [projection.Indexer], and a published page is what an agent's
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
// # The two-key sequences
//
// As in the tracker, and for the same reason — coordination has no multi-key
// transaction — each states its order and what a crash between the halves
// leaves:
//
//	Create a page   title claim Create, page Create, change Create
//	                a crash leaves an ORPHAN CLAIM, overwritten by the grace
//	                rule below and swept after an hour
//	Save a body     revision Create, head CAS, change Create
//	                a crash leaves an ORPHAN REVISION above the page's
//	                version; the next writer treats a refusal older than the
//	                grace as an orphan and overwrites it, so a crash never
//	                locks a page until the sweep
//	Rename          new claim Create, head CAS, old claim Purge, change Create
//	                a crash leaves the old claim held, which blocks only a
//	                third page taking that name until the sweep
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

// OrphanGrace is how long a half-finished write is left alone before another
// writer may step over it.
//
// THIRTY SECONDS, and the number is doing real work. Within it, an ordinary
// in-flight write and a crashed one look identical, so stepping over either
// would destroy a live save; past it, a revision above the page's version can
// only be a writer that died between its two keys. Shorter races a slow but
// healthy writer; longer leaves a page locked for a person who is waiting.
//
// A page is never locked PERMANENTLY by a crash as a result — which is the
// alternative this exists to avoid, where an hourly sweep is the only thing
// that frees an edit somebody is trying to make now.
const OrphanGrace = 30 * time.Second

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

// ConversationKey is the key a wake about this page carries, so every message
// about one page lands in one conversation ledger.
//
// THE PAGE ID rather than its title, unlike a work item's human key: a title
// changes, and a conversation keyed on one would split in half at a rename,
// silently — each half looking like a perfectly ordinary conversation.
func ConversationKey(pageID string) string { return "page:" + pageID }

// nowUTC is the default clock.
func nowUTC() time.Time { return time.Now().UTC() }

// ErrInvalid reports a value this knowledge base refuses.
var ErrInvalid = fmt.Errorf("pages: invalid")

// invalid builds a refusal naming the field and what to do.
func invalid(field, why string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(why, args...))
}
