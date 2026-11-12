package atlas

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ActivityDays is how many days of the log an activity read counts, today
// included: two weeks, so a weekly rhythm shows twice and a quiet weekend
// reads as a pattern rather than a gap, and a sparkline has a point a day.
const ActivityDays = 14

// Change is what one line of the atlas's log did.
type Change string

// The changes a line records.
const (
	// ChangeRecorded is an entry a source said is there: added, confirmed,
	// restored or seen.
	ChangeRecorded Change = "recorded"
	// ChangeRetired is an entry a person retired, or one an integration's
	// reads stopped listing.
	ChangeRetired Change = "retired"
	// ChangeFactSet is a value recorded for a fact.
	ChangeFactSet Change = "fact_set"
	// ChangeFactDenied is a value a person said is wrong.
	ChangeFactDenied Change = "fact_denied"
	// ChangeRelated is a relation, or an owner, recorded.
	ChangeRelated Change = "related"
	// ChangeUnrelated is a relation, or an owner, a person said is not there.
	ChangeUnrelated Change = "unrelated"
	// ChangeJoined is two entries a person said are the same thing.
	ChangeJoined Change = "joined"
	// ChangeKeptApart is two entries a person said are not the same thing.
	ChangeKeptApart Change = "kept_apart"
	// ChangeWithdrawn is a claim its source stopped making: what an
	// integration's read no longer holds, or a "the same" a person took back.
	ChangeWithdrawn Change = "withdrawn"
)

// Changes is every change.
var Changes = []Change{
	ChangeRecorded, ChangeRetired, ChangeFactSet, ChangeFactDenied, ChangeRelated, ChangeUnrelated,
	ChangeJoined, ChangeKeptApart, ChangeWithdrawn,
}

// Valid reports whether c is a member of the closed set.
func (c Change) Valid() bool { return slices.Contains(Changes, c) }

// LineTarget is what a line's relation, or its "the same", names: another
// entry by its id and name, or a seat, a unit or a page by its own.
type LineTarget struct {
	ID       uuid.UUID `json:"id,omitzero"`
	External External  `json:"external,omitempty"`
	Name     string    `json:"name" untrusted:"name"`
}

// Line is one line of the atlas's log as a reader is shown it: what changed
// on which entry, when, and in whose voice. Text is the engine's own sentence
// ([Entry.Reason]'s rule); a value and a name ride beside it, typed.
type Line struct {
	// ID is unique to the line: a claim's id, and for the line a withdrawal
	// left, the claim's id marked as withdrawn.
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Entry, Name and Kind are the entry the line is about, as the reader's
	// atlas names it.
	Entry    uuid.UUID   `json:"entry"`
	Name     string      `json:"name" untrusted:"name"`
	Kind     Kind        `json:"kind"`
	Change   Change      `json:"change"`
	Text     string      `json:"text"`
	Fact     Fact        `json:"fact,omitempty"`
	Relation Relation    `json:"relation,omitempty"`
	Value    Value       `json:"value,omitzero"`
	Target   *LineTarget `json:"target,omitempty"`
	Source   Source      `json:"source"`
}

// spaced is a vocabulary word as prose reads it.
func spaced[T ~string](word T) string { return strings.ReplaceAll(string(word), "_", " ") }

// changeOf is what a claim did when it was recorded, or with withdrawn, when
// its source stopped making it.
func changeOf(c Claim, withdrawn bool) Change {
	switch {
	case withdrawn:
		return ChangeWithdrawn
	case c.Facet.Kind == FacetExists && c.Denied:
		return ChangeRetired
	case c.Facet.Kind == FacetExists:
		return ChangeRecorded
	case c.Facet.Kind == FacetFact && c.Denied:
		return ChangeFactDenied
	case c.Facet.Kind == FacetFact:
		return ChangeFactSet
	case c.Facet.Kind == FacetRelation && c.Denied:
		return ChangeUnrelated
	case c.Facet.Kind == FacetRelation:
		return ChangeRelated
	case c.Denied:
		// A "the same" or a "not the same" a person took back.
		return ChangeWithdrawn
	case c.Facet.Kind == FacetSame:
		return ChangeJoined
	}
	return ChangeKeptApart
}

// lineText is the one sentence a line reads as: its change and the closed
// word of the fact or the relation it is about, and nothing anybody wrote.
func lineText(c Claim, change Change) string {
	owner := c.Facet.Relation == RelationOwnedBy
	switch change {
	case ChangeRecorded:
		if c.Source.Kind == SourcePerson {
			return "A person recorded it as part of the atlas"
		}
		return "Seen by its source"
	case ChangeRetired:
		if c.Source.Kind == SourcePerson {
			return "Retired by a person"
		}
		return "Retired: its source stopped listing it"
	case ChangeFactSet:
		return "Recorded its " + spaced(c.Facet.Fact)
	case ChangeFactDenied:
		return "A value of its " + spaced(c.Facet.Fact) + " was marked as wrong"
	case ChangeRelated:
		if owner {
			return "Recorded its owner"
		}
		return "Recorded a relation: " + spaced(c.Facet.Relation)
	case ChangeUnrelated:
		if owner {
			return "Its owner was removed"
		}
		return "A relation was marked as not there: " + spaced(c.Facet.Relation)
	case ChangeJoined:
		return "Recorded as the same thing as another entry"
	case ChangeKeptApart:
		return "Recorded as not the same thing as another entry"
	}
	switch c.Facet.Kind {
	case FacetFact:
		return "No longer reported: its " + spaced(c.Facet.Fact)
	case FacetRelation:
		if owner {
			return "No longer reported: its owner"
		}
		return "No longer reported: a relation, " + spaced(c.Facet.Relation)
	case FacetSame, FacetDistinct:
		return "A person took back what they said of it and another entry"
	}
	return "No longer reported by its source"
}

// Logged is the entry of v that r names, where a reader of v is shown its
// lines: not an unconfirmed entry unless unconfirmed says the read shows them,
// and not a repository or a check Gitlet names unless Gitlet's own records
// listed it to this reader, so the log never tells of a repository its reader
// may not read, whoever recorded something about it. index is v's
// ([View.Index]).
func (v View) Logged(index map[uuid.UUID]int, r Ref, unconfirmed bool) (*Entry, bool) {
	i, ok := index[r.ID()]
	if !ok {
		return nil, false
	}
	e := &v.Entries[i]
	switch {
	case !unconfirmed && e.Word == WordUnconfirmed:
		return nil, false
	case r.Kind != KindAccount && r.OnGitlet() && !slices.Contains(e.Sources, RecordSource(RecordGitlet)):
		return nil, false
	}
	return e, true
}

// Line is the log line a claim left as a reader of v is shown it: id names
// the line, at is when it was recorded, and withdrawn says it is the line its
// source left by no longer making the claim. It reports false for a line the
// reader is not shown ([View.Logged]): one on an entry they are not shown,
// or naming one.
func (v View) Line(index map[uuid.UUID]int, c Claim, id string, at time.Time, withdrawn, unconfirmed bool) (Line, bool) {
	e, ok := v.Logged(index, c.Subject, unconfirmed)
	if !ok {
		return Line{}, false
	}
	change := changeOf(c, withdrawn)
	out := Line{ID: id, At: at, Entry: e.ID, Name: e.Name, Kind: e.Ref.Kind, Change: change,
		Text: lineText(c, change), Fact: c.Facet.Fact, Relation: c.Facet.Relation, Value: c.Value, Source: c.Source}
	if withdrawn {
		out.ID += ":withdrawn"
	}
	switch t := c.Facet.Target; {
	case t.Entry != (Ref{}):
		other, ok := v.Logged(index, t.Entry, unconfirmed)
		if !ok {
			return Line{}, false
		}
		out.Target = &LineTarget{ID: other.ID, Name: other.Name}
	case t.External != "":
		out.Target = &LineTarget{External: t.External, Name: t.Name}
	}
	return out, true
}
