package learning

import (
	"slices"
	"time"
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
// and when the seat was last told its access changed (zero for never, or
// longer ago than the told memory window).
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
