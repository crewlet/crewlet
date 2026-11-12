package atlas

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// View is what the atlas says: every entry, folded from every claim, and the
// claims the fold refused with why.
type View struct {
	Entries []Entry `json:"entries"`
	Skipped []Skip  `json:"skipped,omitempty"`
}

// Skip is one claim the fold refused. It names the entry and the rule, never
// the value, which may be what the rule refused.
type Skip struct {
	Subject Ref    `json:"subject"`
	Reason  string `json:"reason"`
}

// State is where an entry stands, read off its claims on every read, so no
// state outlives its condition and two nodes give one answer.
type State string

const (
	// StateConfirmed is an entry the settings, the engine's records or a
	// person stand behind.
	StateConfirmed State = "confirmed"
	// StateDiscovered is an entry only an integration or an agent reports.
	StateDiscovered State = "discovered"
	// StateStale is a discovered entry only agents report, none of them for
	// [StaleAfter].
	StateStale State = "stale"
	// StateRetired is an entry a person retired. It leaves the map and stays
	// in history.
	StateRetired State = "retired"
)

// States is every state.
var States = []State{StateConfirmed, StateDiscovered, StateStale, StateRetired}

// Valid reports whether s is a member of the closed set.
func (s State) Valid() bool { return slices.Contains(States, s) }

// Standing is whether the sources agree on one fact or relation.
type Standing string

const (
	// StandingSettled is a fact nobody contradicts.
	StandingSettled Standing = "settled"
	// StandingContested is a person's value a newer report from an
	// integration, an agent or the engine contradicts. Both are shown.
	StandingContested Standing = "contested"
	// StandingDisputed is two sources the engine trusts as much as each other
	// disagreeing, with no person to settle it. Both are shown.
	StandingDisputed Standing = "disputed"
)

// Standings is every standing.
var Standings = []Standing{StandingSettled, StandingContested, StandingDisputed}

// Valid reports whether s is a member of the closed set.
func (s Standing) Valid() bool { return slices.Contains(Standings, s) }

// Entry is one thing the company runs on, as the atlas reads it.
type Entry struct {
	// ID is the entry's id: its canonical member's. Every member's own id
	// finds it too ([View.Find]).
	ID   uuid.UUID `json:"id"`
	Ref  Ref       `json:"ref"`
	Lane Lane      `json:"lane"`
	// Name is its name fact, or its key where nothing names it: somebody's
	// words, which a tool answers only under their writers' label.
	Name string `json:"name" untrusted:"name"`
	// Members is every entry an exact key merged into this one, the canonical
	// one first; one member for an entry nothing merged.
	Members    []Ref      `json:"members"`
	State      State      `json:"state"`
	Confidence Confidence `json:"confidence"`
	Word       Word       `json:"word"`
	Facts      []FactView `json:"facts"`
	Relations  []Edge     `json:"relations"`
	// Sources is every source with a live claim on it.
	Sources  []Source  `json:"sources"`
	LastSeen time.Time `json:"last_seen"`
	// Review is a fact or a relation on it that is contested or disputed.
	Review bool `json:"review,omitempty"`
	// Use, Holders and Signals are what the engine's records say about it
	// beyond claims, joined on every read ([Join]); NotUsedRecently is an
	// entry whose use is counted and that nothing used for [NotUsedFor].
	Use             []Use    `json:"use,omitempty"`
	Holders         []Holder `json:"holders,omitempty"`
	Signals         []Signal `json:"signals,omitempty"`
	NotUsedRecently bool     `json:"not_used_recently,omitempty"`
}

// Claimed is one source's live claim beside the value the atlas holds.
type Claimed struct {
	Value      Value      `json:"value,omitzero"`
	Denied     bool       `json:"denied,omitempty"`
	Source     Source     `json:"source"`
	Confidence Confidence `json:"confidence"`
	ObservedAt time.Time  `json:"observed_at"`
}

// FactView is one fact the atlas holds, where it came from, and every other
// value a live source holds for it.
type FactView struct {
	Fact       Fact       `json:"fact"`
	Value      Value      `json:"value"`
	Source     Source     `json:"source"`
	Confidence Confidence `json:"confidence"`
	Word       Word       `json:"word"`
	ObservedAt time.Time  `json:"observed_at"`
	Standing   Standing   `json:"standing"`
	// Suggested is a fact of meaning nobody but an integration or an agent
	// has given: shown as a suggestion until a person sets it.
	Suggested bool      `json:"suggested,omitempty"`
	Others    []Claimed `json:"others,omitempty"`
}

// Edge is one relation from an entry: to another entry by its id, or to an
// identity outside the atlas.
type Edge struct {
	Relation   Relation   `json:"relation"`
	To         uuid.UUID  `json:"to,omitzero"`
	External   External   `json:"external,omitempty"`
	Name       string     `json:"name,omitempty"`
	Source     Source     `json:"source"`
	Confidence Confidence `json:"confidence"`
	Word       Word       `json:"word"`
	ObservedAt time.Time  `json:"observed_at"`
	Standing   Standing   `json:"standing"`
	Suggested  bool       `json:"suggested,omitempty"`
	Others     []Claimed  `json:"others,omitempty"`
}

// Find is the entry id names: its own, or one merged into it.
func (v View) Find(id uuid.UUID) (Entry, bool) {
	for _, e := range v.Entries {
		if e.ID == id || slices.ContainsFunc(e.Members, func(r Ref) bool { return r.ID() == id }) {
			return e, true
		}
	}
	return Entry{}, false
}

// Index maps every member's id to its entry's position in Entries, for a
// reader that finds many.
func (v View) Index() map[uuid.UUID]int {
	out := make(map[uuid.UUID]int, len(v.Entries))
	for i, e := range v.Entries {
		for _, m := range e.Members {
			out[m.ID()] = i
		}
	}
	return out
}
