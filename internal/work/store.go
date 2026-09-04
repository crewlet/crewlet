package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Documents is the coordination surface this package writes through.
//
// A NARROWED [coord.Documents]: the tracker reads a key, creates one, updates
// one at a version and lists a prefix. Declaring what it uses keeps the fake
// in this package's tests to five methods rather than nine, and makes the
// dependency legible — nothing here watches, and nothing here purges outside
// the removal path.
type Documents interface {
	Document(ctx context.Context, family coord.Family, key string) (coord.Record, bool, error)
	Documents(ctx context.Context, family coord.Family, prefix string) ([]coord.Record, error)
	CreateDocument(ctx context.Context, family coord.Family, key string, value []byte) (bool, error)
	UpdateDocument(ctx context.Context, family coord.Family, key string, value []byte, version uint64) (bool, error)
	PurgeDocument(ctx context.Context, family coord.Family, key string, version uint64) (bool, error)
}

// Store is the tracker's write path.
//
// EVERY WRITE GOES TO COORDINATION, never to the projection. A row written
// here directly would be erased by the next boot reconcile, and it would look
// exactly like the engine losing somebody's work.
type Store struct {
	docs Documents
	now  func() time.Time

	// newID mints a document id, and newSeqID a time-ordered one.
	// Injectable so a test can make a sequence deterministic; production
	// takes UUIDv4 and UUIDv7.
	newID    func() string
	newSeqID func() string
}

// Options configure a store.
type Options struct {
	Documents Documents

	// Now is the clock. Nil takes the wall clock in UTC.
	Now func() time.Time
}

// NewStore builds the tracker over a coordination backend.
func NewStore(opts Options) (*Store, error) {
	if opts.Documents == nil {
		return nil, errors.New("work: a document store is required")
	}
	s := &Store{
		docs:     opts.Documents,
		now:      opts.Now,
		newID:    uuid.NewString,
		newSeqID: newTimeOrderedID,
	}
	if s.now == nil {
		s.now = nowUTC
	}
	return s, nil
}

// newTimeOrderedID mints a change id that sorts by creation time.
//
// UUIDv7, not a ULID: the property wanted is a lexicographically sortable
// id, which v7 has (48 bits of Unix milliseconds first), and the module is
// already a dependency. Adding a ULID library would buy a shorter string and
// a fourth id format in one tree.
//
// A FAILING RANDOM SOURCE FALLS BACK TO v4 rather than failing the write.
// The ordering is a convenience for listing a history; losing a change record
// because the kernel's entropy pool hiccuped is not a trade worth making.
func newTimeOrderedID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// casRounds bounds a read-decide-write retry.
//
// SIXTEEN, the shape [sandbox]'s run store uses and for the same reason: each
// round is a lost race against another writer, and sixteen consecutive losses
// on one item means something is rewriting it in a loop rather than that this
// caller is unlucky. Reporting "it kept changing" then is the honest answer;
// retrying for ever would hold a request open against a write storm.
const casRounds = 16

// ErrConflict reports a write that lost its race too many times.
var ErrConflict = errors.New("work: the item kept changing under this write")

// ErrNotFound reports an item, comment or project that does not exist.
//
// A SENTINEL, because "no such item" is a fact a caller acts on — it files a
// new one, it tells a person their link is dead — and it must never be
// produced by a store that simply could not be reached. Every path that could
// confuse the two raises instead.
var ErrNotFound = errors.New("work: no such record")

// ErrStaleVersion reports a write conditioned on a version that has moved.
var ErrStaleVersion = errors.New("work: the record has changed since it was read")

// ErrReassignmentBudget reports a hand-off past [ReassignmentBudget].
var ErrReassignmentBudget = errors.New("work: this item has been reassigned too many times")

// ErrInvalid reports a value this tracker refuses.
var ErrInvalid = errors.New("work: invalid")

// invalid builds a refusal naming the field and what to do.
func invalid(field, why string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(why, args...))
}

// Actor is who is making a write.
//
// IDENTITY IS NEVER A MODEL ARGUMENT. A builtin receives it from the turn
// context the tool surface bound; the REST edge takes it from the
// authenticated token and the seat it is bound to. A caller that could name
// its own actor could file work as anybody.
type Actor struct {
	// Handle is the seat this write acts as, empty for an operator whose
	// token is bound to no seat.
	Handle string

	Kind AuthorKind

	// OperatorID is the API token's own label, recorded beside the seat for
	// audit. A person and the credential they used are two facts.
	OperatorID string

	// TurnID and Chain are provenance for an agent write. They bound
	// nothing — see the package doc on hand-off loops.
	TurnID string
	Chain  []string
}

// Name is how this actor is recorded and rendered.
//
// A TOKEN BOUND TO NO SEAT IS `operator:<id>`, rendered by the label its
// operator chose. Never "the engine": the engine does not act as itself, and
// a label a person picked is the honest name for what they did.
func (a Actor) Name() string {
	if a.Handle != "" {
		return a.Handle
	}
	if a.OperatorID != "" {
		return "operator:" + a.OperatorID
	}
	return "operator:" + coordAnonymous
}

// coordAnonymous mirrors config.ReservedOperatorID without importing config:
// this package is below it, and the one string both need is the attribution
// stamped when the auth guard is off.
const coordAnonymous = "anonymous"

// IsAgent reports whether this write came from a turn.
func (a Actor) IsAgent() bool { return a.Kind == AuthorAgent }

// IsHuman reports whether a person made this write, through either surface.
//
// AN OPERATOR COUNTS, and that is what resets the reassignment budget: a
// person acting through the dashboard with a token bound to no seat is still
// a person looking at the item, which is exactly the event the budget is
// waiting for.
func (a Actor) IsHuman() bool { return a.Kind == AuthorHuman || a.Kind == AuthorOperator }

// validate checks an actor is one this store may attribute a write to.
func (a Actor) validate() error {
	if !a.Kind.Valid() {
		return invalid("actor.kind", "%q is not one of %v", a.Kind, AuthorKinds())
	}
	if a.Kind != AuthorOperator && a.Handle == "" {
		return invalid("actor.handle",
			"an %s write must name the seat it acts as", a.Kind)
	}
	return nil
}

// excerpt cuts text for a change record, rune-safely and with a marker.
func excerpt(text string) string {
	return textcut.Ellipsis(strings.Join(strings.Fields(text), " "), MaxExcerpt)
}

// cleanList trims, drops empties and deduplicates, preserving order.
func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// addWatcher adds a handle unless it is muted.
//
// THE MUTE IS WHAT MAKES AN UNWATCH STICK. Every automatic re-add — reporter,
// assignee, commenter — goes through here, so a person who unwatched an item
// stays unwatched however many times the rules would otherwise put them back.
// A DIRECTED MENTION IS THE EXCEPTION and does not come through this path:
// somebody typing a handle is addressing that person, and a mute must not
// swallow that.
func addWatcher(item *Item, handle string) {
	handle = strings.TrimSpace(handle)
	if handle == "" || slices.Contains(item.Muted, handle) {
		return
	}
	if !slices.Contains(item.Watchers, handle) {
		item.Watchers = append(item.Watchers, handle)
	}
}

// mention adds a handle even when muted, for a directed mention.
func mention(item *Item, handle string) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return
	}
	// A mention UNMUTES, because a mute is "stop telling me about this
	// item" and a mention is "I am telling you specifically" — leaving the
	// mute would silently drop every subsequent message in a thread the
	// person was pulled into.
	item.Muted = slices.DeleteFunc(item.Muted, func(h string) bool { return h == handle })
	if !slices.Contains(item.Watchers, handle) {
		item.Watchers = append(item.Watchers, handle)
	}
}

// snapshotOf is the routing state a change carries.
//
// WATCHERS MINUS MUTED, computed here so the feed never has to subtract and
// can never forget to.
func snapshotOf(item Item) Snapshot {
	watchers := make([]string, 0, len(item.Watchers))
	for _, w := range item.Watchers {
		if !slices.Contains(item.Muted, w) {
			watchers = append(watchers, w)
		}
	}
	if len(watchers) == 0 {
		watchers = nil
	}
	return Snapshot{
		Key: item.Key, Project: item.Project, Title: item.Title,
		Status: item.Status, Assignee: item.Assignee, Reporter: item.Reporter,
		Watchers: watchers,
	}
}
