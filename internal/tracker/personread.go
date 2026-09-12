package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading a person's own state.
//
// # Why this is one read and not three
//
// The three verbs write disjoint parts of one document, and a screen renders
// all of them at once: the inbox beside the priority list beside the pins is
// what "my day" IS. Three reads would let the strip show a pinned view the
// inbox write had just removed.
//
// # A person nobody has written is an EMPTY person, not a missing one
//
// Every human starts with no inbox, no pins and no priorities, and there is no
// gesture that creates the record — the first write does. So a read that found
// nothing answers the empty state rather than a not-found, which is what makes
// a fresh company's first screen render instead of erroring.

// PersonState is one human's own state, plus what a screen needs beside it.
type PersonState struct {
	Handle string `json:"handle"`

	// Unread, Read and Snoozed are the inbox. Snoozed entries whose time
	// has come are reported as DUE rather than silently promoted: putting
	// one back is a write, and a read that performed it would be a read
	// that changed the fleet's state.
	Unread  []InboxEntry `json:"unread,omitempty"`
	Read    []InboxEntry `json:"read,omitempty"`
	Snoozed []InboxEntry `json:"snoozed,omitempty"`
	Due     []InboxEntry `json:"due,omitempty"`

	PrimaryReasons []Reason   `json:"primary_reasons,omitempty"`
	Priorities     []string   `json:"priorities,omitempty"`
	PinnedViews    []string   `json:"pinned_views,omitempty"`
	Favorites      []Favorite `json:"favorites,omitempty"`

	// PrioritiesSetBy is who last set the list when it was not this
	// person, and empty when it was theirs. See [Person].
	PrioritiesSetBy string    `json:"priorities_set_by,omitempty"`
	PrioritiesSetAt time.Time `json:"priorities_set_at,omitzero"`

	SeenThrough Position `json:"seen_through,omitzero"`
	Version     uint64   `json:"version"`

	// Held is false for a person nobody has written yet, which is an
	// EMPTY state rather than a missing one — every other field is the
	// zero value and a screen renders it as a clean inbox.
	Held bool `json:"held"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// PersonQuery asks for one.
type PersonQuery struct {
	Handle string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Person answers one human's own state.
func (r *Reader) Person(ctx context.Context, q PersonQuery, now time.Time) (PersonState, error) {
	switch {
	case q.Level == "":
		return PersonState{}, fmt.Errorf("tracker: this person read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	case q.Handle == "":
		return PersonState{}, fmt.Errorf("tracker: a person read names nobody")
	}

	out := PersonState{Handle: q.Handle}
	served, err := r.log.Read(ctx, statelog.Query{
		Level:           q.Level,
		Scope:           personScope(),
		Session:         q.Session,
		MinPosition:     q.MinPosition,
		MaxLag:          q.MaxLag,
		MaxLagPositions: q.MaxLagSeq,
		Set:             true,
	}, func(tx *sql.Tx) error {
		person, held, err := readPerson(ctx, tx, q.Handle)
		if err != nil {
			return err
		}
		out.Held = held
		out.Unread, out.Read = person.Unread, person.Read
		out.PrimaryReasons = person.PrimaryReasons
		out.Priorities = person.Priorities
		out.PinnedViews, out.Favorites = person.PinnedViews, person.Favorites
		out.PrioritiesSetBy = person.PrioritiesSetBy
		out.PrioritiesSetAt = person.PrioritiesSetAt
		out.SeenThrough, out.Version = person.SeenThrough, person.Version
		out.Snoozed, out.Due = splitSnoozes(person.Snoozed, now)
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return PersonState{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// personScope is the person FAMILY, which is where a person subject resolves
// — see [subjectPath].
func personScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermFamily, ID: string(KindPerson)}.Path(),
	}}.Normalised()
}

// splitSnoozes separates what is still asleep from what is due.
//
// REPORTED, NEVER PROMOTED. Putting a due item back in the unread list is a
// WRITE, and a read that performed one would change the fleet's state from a
// path with no operation id, no arbitration and no record — so this says which
// are due and the caller's next inbox write is what moves them.
func splitSnoozes(entries []InboxEntry, now time.Time) (asleep, due []InboxEntry) {
	for _, entry := range entries {
		if entry.Until != nil && !entry.Until.After(now) {
			due = append(due, entry)
			continue
		}
		asleep = append(asleep, entry)
	}
	return asleep, due
}
