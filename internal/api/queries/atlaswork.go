package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE WORK THAT TOUCHES AN ENTRY. A task names the atlas entries it touches on
// its own record (ADR-0140), so these questions join two apps, and each half
// is decided by its own (ADR-0127): the atlas permission opens the question,
// and the tracker is read only for an asker who uses Worklet. Anybody else is
// told no work was read and why, and is handed none.

// Why no work was read, in the words the answer carries.
const (
	workUnreadNoApp  = "you do not use Worklet, so no work was read"
	workUnreadAbsent = "this company's work is not kept in Worklet, so there is none to read"
)

// AtlasWorkAnswer is how many open tasks name each entry, by the entry's id.
// Unread says why no work was read, and is empty where it was.
type AtlasWorkAnswer struct {
	Unread string         `json:"unread,omitempty"`
	Open   map[string]int `json:"open"`
	Capped bool           `json:"capped,omitempty"`
}

// AtlasEntryWorkAnswer is the tasks naming one entry: the open ones, then the
// ones finished lately, each with its total.
type AtlasEntryWorkAnswer struct {
	Unread string `json:"unread,omitempty"`
	worklet.EntryWork
}

// AtlasNamed is one asked id and the entry it names, as a row: the entry the
// id merged into, so two ids of one entry name the same row.
type AtlasNamed struct {
	ID    string        `json:"id"`
	Entry atlas.Summary `json:"entry"`
}

// AtlasNamedAnswer is what a list of ids names, in the order asked, and the
// ids that name no entry the asker reads.
type AtlasNamedAnswer struct {
	Named   []AtlasNamed `json:"named"`
	Missing []string     `json:"missing"`
}

// registerAtlasWork registers the questions about the work on entries.
func registerAtlasWork(r *Registry, s Sources) {
	app := Requires(iam.GrantAtlas)
	r.Register("atlas_work", app, s.atlasWork)
	r.Register("atlas_entry_work", app, s.atlasEntryWork)
	r.Register("atlas_named", app, s.atlasNamed)
}

// workUnread is why principal is read no work, or "" where the tracker is
// read for them.
func (s Sources) workUnread(principal iam.Principal) string {
	switch {
	case !principal.Can(iam.GrantWorklet):
		return workUnreadNoApp
	case s.Work == nil:
		return workUnreadAbsent
	}
	return ""
}

// workFailure reads a tracker this company does not keep as no work to read,
// and anything else as the failure it is.
func workFailure(err error) (string, error) {
	if errors.Is(err, ErrUnknown) {
		return workUnreadAbsent, nil
	}
	return "", err
}

func (s Sources) atlasWork(ctx context.Context, _ Params) (any, error) {
	principal, err := caller(ctx, "atlas_work")
	if err != nil {
		return nil, err
	}
	out := AtlasWorkAnswer{Open: map[string]int{}}
	if out.Unread = s.workUnread(principal); out.Unread != "" {
		return out, nil
	}
	view, _, err := s.atlasView(ctx, "atlas_work")
	if err != nil {
		return nil, err
	}
	marks, err := s.Work.EntryMarks(ctx, atlasFresh.Level)
	if err != nil {
		out.Unread, err = workFailure(err)
		return out, err
	}
	// THROUGH THE ENTRY EACH ID MERGED INTO, so a task that named one member
	// marks the entry the map draws; an id the asker's atlas does not hold
	// marks nothing.
	index := view.Index()
	for named, open := range marks.Open {
		id, parseErr := uuid.Parse(named)
		if parseErr != nil {
			continue
		}
		if at, held := index[id]; held {
			out.Open[view.Entries[at].ID.String()] += open
		}
	}
	out.Capped = marks.Capped
	return out, nil
}

func (s Sources) atlasEntryWork(ctx context.Context, p Params) (any, error) {
	id, err := atlasID(p)
	if err != nil {
		return nil, err
	}
	principal, err := caller(ctx, "atlas_entry_work")
	if err != nil {
		return nil, err
	}
	view, _, err := s.atlasView(ctx, "atlas_entry_work")
	if err != nil {
		return nil, err
	}
	e, found := view.Find(id)
	if !found {
		return nil, noAtlasEntry()
	}
	out := AtlasEntryWorkAnswer{EntryWork: worklet.EntryWork{Open: []worklet.TaskRow{}, Recent: []worklet.TaskRow{}}}
	if out.Unread = s.workUnread(principal); out.Unread != "" {
		return out, nil
	}
	// EVERY MEMBER'S ID, the newest first to merge left out past the read's
	// bound: a task named whichever member its writer read.
	ids := make([]string, 0, len(e.Members))
	for _, m := range e.Members[:min(len(e.Members), worklet.MaxEntryWorkEntries)] {
		ids = append(ids, m.ID().String())
	}
	work, err := s.Work.EntryWork(ctx, worklet.EntryWorkQuery{Entries: ids, Level: atlasFresh.Level}, s.clock())
	if err != nil {
		out.Unread, err = workFailure(err)
		return out, err
	}
	out.EntryWork = work
	return out, nil
}

func (s Sources) atlasNamed(ctx context.Context, p Params) (any, error) {
	var ids []uuid.UUID
	for _, raw := range strings.Split(p.String("ids"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: `ids` is atlas entry ids, separated by commas", ErrBadParams)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > worklet.MaxOtherRelations {
		return nil, fmt.Errorf("%w: `ids` names 1 to %d atlas entries", ErrBadParams, worklet.MaxOtherRelations)
	}
	view, _, err := s.atlasView(ctx, "atlas_named")
	if err != nil {
		return nil, err
	}
	out := AtlasNamedAnswer{Named: []AtlasNamed{}, Missing: []string{}}
	summaries, index, asked := view.Summaries(true), view.Index(), map[uuid.UUID]bool{}
	for _, id := range ids {
		if asked[id] {
			continue
		}
		asked[id] = true
		if at, held := index[id]; held {
			out.Named = append(out.Named, AtlasNamed{ID: id.String(), Entry: summaries.Of(view.Entries[at])})
			continue
		}
		out.Missing = append(out.Missing, id.String())
	}
	return out, nil
}
