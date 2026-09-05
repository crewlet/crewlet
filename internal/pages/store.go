package pages

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
type Documents interface {
	Document(ctx context.Context, family coord.Family, key string) (coord.Record, bool, error)
	Documents(ctx context.Context, family coord.Family, prefix string) ([]coord.Record, error)
	CreateDocument(ctx context.Context, family coord.Family, key string, value []byte) (bool, error)
	UpdateDocument(ctx context.Context, family coord.Family, key string, value []byte, version uint64) (bool, error)
	PurgeDocument(ctx context.Context, family coord.Family, key string, version uint64) (bool, error)
}

// Store is the knowledge base's write path. Every write goes to coordination,
// never to the projection.
type Store struct {
	docs Documents
	now  func() time.Time

	newID    func() string
	newSeqID func() string
}

// Options configure a store.
type Options struct {
	Documents Documents

	// Now is the clock. Nil takes the wall clock in UTC.
	Now func() time.Time
}

// NewStore builds the knowledge base over a coordination backend.
func NewStore(opts Options) (*Store, error) {
	if opts.Documents == nil {
		return nil, errors.New("pages: a document store is required")
	}
	s := &Store{docs: opts.Documents, now: opts.Now,
		newID: uuid.NewString, newSeqID: newTimeOrderedID}
	if s.now == nil {
		s.now = nowUTC
	}
	return s, nil
}

// newTimeOrderedID mints a change id that sorts by creation time. UUIDv7, for
// the reason [work] gives, falling back to v4 rather than failing a write.
func newTimeOrderedID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// casRounds bounds a read-decide-write retry, on [work]'s reasoning.
const casRounds = 16

// The sentinels. Each is a fact a caller acts on differently, which is why
// they are separate: a title somebody else holds is a rename to negotiate, a
// stale version is an edit to re-base, and an unreachable store is neither.
var (
	// ErrNotFound reports a page, container or revision that does not exist.
	ErrNotFound = errors.New("pages: no such record")

	// ErrTitleTaken reports a title another page in the container holds.
	ErrTitleTaken = errors.New("pages: that title is taken in this container")

	// ErrStaleVersion reports a save against a version that has moved.
	ErrStaleVersion = errors.New("pages: the page has changed since it was read")

	// ErrConflict reports a write that lost its race too many times.
	ErrConflict = errors.New("pages: the page kept changing under this write")

	// ErrReserved reports a container the engine holds for itself.
	ErrReserved = errors.New("pages: that container is reserved")
)

// Actor is who is making a write, on [work.Actor]'s terms.
type Actor struct {
	Handle     string
	Kind       AuthorKind
	OperatorID string
	TurnID     string
	Chain      []string
}

// Name is how this actor is recorded and rendered.
func (a Actor) Name() string {
	if a.Handle != "" {
		return a.Handle
	}
	if a.OperatorID != "" {
		return "operator:" + a.OperatorID
	}
	return "operator:anonymous"
}

// IsHuman reports whether a person made this write, through either surface.
func (a Actor) IsHuman() bool { return a.Kind == AuthorHuman || a.Kind == AuthorOperator }

func (a Actor) validate() error {
	if !a.Kind.Valid() {
		return invalid("actor.kind", "%q is not one of %v", a.Kind, AuthorKinds())
	}
	if a.Kind != AuthorOperator && a.Handle == "" {
		return invalid("actor.handle", "an %s write must name the seat it acts as", a.Kind)
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

// addWatcher adds a handle unless it is muted — the same rule the tracker's
// mute enforces, and for the same reason.
func addWatcher(page *Page, handle string) {
	handle = strings.TrimSpace(handle)
	if handle == "" || slices.Contains(page.Muted, handle) {
		return
	}
	if !slices.Contains(page.Watchers, handle) {
		page.Watchers = append(page.Watchers, handle)
	}
}

// mention adds a handle even when muted, and clears the mute.
func mention(page *Page, handle string) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return
	}
	page.Muted = slices.DeleteFunc(page.Muted, func(h string) bool { return h == handle })
	if !slices.Contains(page.Watchers, handle) {
		page.Watchers = append(page.Watchers, handle)
	}
}

// snapshotOf is the routing state a change carries, watchers minus muted.
func snapshotOf(page Page) Snapshot {
	watchers := make([]string, 0, len(page.Watchers))
	for _, w := range page.Watchers {
		if !slices.Contains(page.Muted, w) {
			watchers = append(watchers, w)
		}
	}
	if len(watchers) == 0 {
		watchers = nil
	}
	return Snapshot{
		Container: page.Container, Title: page.Title, Status: page.Status,
		Author: page.Author, Version: page.Version, Watchers: watchers,
	}
}

// change builds a change record for a page's current state.
func (s *Store) change(actor Actor, page Page, kind ChangeKind, at time.Time) Change {
	return Change{
		V: DocumentVersion, ID: s.newSeqID(), PageID: page.ID, Kind: kind,
		Actor: actor.Name(), ActorKind: actor.Kind, OperatorID: actor.OperatorID,
		TurnID: actor.TurnID, Chain: slices.Clone(actor.Chain),
		Snapshot: snapshotOf(page), CreatedAt: at,
	}
}

// writeChange creates the change key, last in every sequence and create-only.
func (s *Store) writeChange(ctx context.Context, change Change) error {
	data, err := EncodeChange(change)
	if err != nil {
		return err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyPages,
		ChangeKey(change.PageID, change.ID), data)
	if err != nil {
		return fmt.Errorf("pages: record change %s: %w", change.ID, err)
	}
	if !created {
		log.DebugContext(ctx, "pages_change_already_recorded",
			"page", change.PageID, "change", change.ID,
			"detail", "a peer repaired this change key from the head first")
	}
	return nil
}

// revisionOf reads a key's current version, after a write this process made.
func (s *Store) revisionOf(ctx context.Context, key string) (uint64, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, key)
	if err != nil {
		return 0, fmt.Errorf("pages: read back %s: %w", key, err)
	}
	if !found {
		return 0, fmt.Errorf("pages: %s vanished immediately after it was written", key)
	}
	return rec.Version, nil
}

// checkTitle refuses a title this knowledge base will not hold.
func (s *Store) checkTitle(title string) error {
	switch {
	case strings.TrimSpace(title) == "":
		return invalid("title", "a page needs a title — it is how people link to it")
	case len(title) > MaxTitle:
		return invalid("title", "%d bytes, past the %d-byte cap", len(title), MaxTitle)
	}
	// NOTHING ELSE IS REFUSED. A title of punctuation, emoji or a single
	// character is odd but it is a legal address: it normalises
	// consistently, it claims like any other, and every byte survives the
	// key grammar's per-segment escaping. A validity rule here would refuse
	// titles in scripts nobody writing it had in mind.
	return nil
}

func (s *Store) checkBody(body string) error {
	if len(body) > MaxBody {
		return invalid("body",
			"%d bytes, past the %d-byte cap — a page is refused rather than "+
				"cut, because a procedure truncated mid-sentence is one somebody "+
				"will follow the first half of", len(body), MaxBody)
	}
	return nil
}

func (s *Store) checkLabels(labels []string) error {
	if len(labels) > MaxLabels {
		return invalid("labels", "%d labels, past the cap of %d", len(labels), MaxLabels)
	}
	for _, l := range labels {
		if len(l) > MaxLabelLength {
			return invalid("labels", "%q is %d bytes, past the %d-byte cap",
				l, len(l), MaxLabelLength)
		}
	}
	return nil
}
