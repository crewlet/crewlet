package learning

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// About is what a memory note is about, a closed class the writer names. It is
// what lets a note that an upgrade or an access change may have made false be
// held back, rather than repeated to people as the truth.
type About string

// The classes of a note.
const (
	// AboutPlatform is how the engine itself works: its tools, its rules.
	AboutPlatform About = "platform"
	// AboutCapability is what the seat can reach or run: its access, its box.
	AboutCapability About = "capability"
	// AboutCompany is how the company works: its conventions, where things live.
	AboutCompany About = "company"
	// AboutPreference is how a person likes to work.
	AboutPreference About = "preference"
)

// Abouts is every class, in the order a writer is offered them.
var Abouts = []About{AboutCompany, AboutPreference, AboutCapability, AboutPlatform}

// Valid reports whether a is a class this build knows.
func (a About) Valid() bool { return slices.Contains(Abouts, a) }

// Currency is what a note is held to at a turn: the engine build running it,
// and when the seat was last told its access (its newest told record, however
// old, or the turn itself where it tells a change); zero for never told.
type Currency struct {
	Build           string
	AccessChangedAt time.Time
}

// HeldBack reports whether e may be false at c: a platform note another build
// wrote, or a capability note written before the seat's access last changed.
// A note of any other class is never held back.
func (e DiaryEntry) HeldBack(c Currency) bool {
	switch e.About {
	case AboutPlatform:
		return c.Build != "" && e.EngineBuild != c.Build
	case AboutCapability:
		return !c.AccessChangedAt.IsZero() && e.CreatedAt.Before(c.AccessChangedAt)
	}
	return false
}

// heldWhere is [DiaryEntry.HeldBack] as a predicate over agent_diary's columns,
// with its arguments: what a read leaves out, so a held note never takes the
// place of one it would offer. TestTheHeldPredicateIsHeldBack holds the two to
// each other.
func heldWhere(c Currency) (string, []any) {
	var changed int64
	if !c.AccessChangedAt.IsZero() {
		changed = store.EncodeTime(c.AccessChangedAt)
	}
	return `((provenance = 'platform' AND ? <> '' AND engine_build <> ?)
		OR (provenance = 'capability' AND ? > 0 AND created_at < ?))`,
		[]any{c.Build, c.Build, changed, changed}
}

// HeldNoteLife is how long a note stays held back before the retention sweep
// retires it: thirty days of turns, each told how many notes it held back and
// how to keep one again. A fact not kept again in that time is one the seat
// does not need, and a held note no read offers is only a row every scan pays.
const HeldNoteLife = 30 * 24 * time.Hour
