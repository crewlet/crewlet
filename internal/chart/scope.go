package chart

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the WRITER and readable at every version.
//
// It is what a node that cannot decode a record files the deferral under, and
// what a later read or write probes to discover it is behind. The framework
// knows exactly one thing about a scope path — that it is a hierarchy written
// left to right with a separator, from which it computes CONTAINMENT — so
// everything this file is for rests on the paths NESTING: a record deferred on
// a unit must block a write to a seat inside it, and a record deferred on one
// seat must NOT block a write to its neighbour.

// scopeSeparator is the framework's, named locally so [Subject.Validate] can
// refuse a key carrying it without importing the framework into a file that
// otherwise needs nothing from it.
const scopeSeparator = statelog.ScopeSeparator

// The scope path alphabet, and the two decisions inside it.
//
// # FLAT
//
// A unit's path is `g/u/<key>` WHATEVER ITS DEPTH IN THE CHART. The obvious
// alternative — writing the ancestor chain into the path, `g/u/eng/u/backend`,
// so the framework's own containment gives an ancestor's deferral its subtree
// for free — is wrong here in a way it is not wrong for a tracker's project or
// a wiki's space, and the difference is that THIS DOMAIN MOVES ITS OWN
// HIERARCHY. A deferral filed under `g/u/eng/u/backend` is a durable row; the
// move that puts `backend` under `product` changes nothing about that row, and
// from that moment every probe for `backend` searches `g/u/product/u/backend`
// and never finds it. The record goes on being undecodable and stops blocking
// anything — silently, and in exactly the window a reorganisation is in
// flight, which is the window the deferral was most needed in.
//
// So an ancestor's blast radius is ENUMERATED by the writer instead, which is
// what [MaxScopeTerms] and the covering root term are for. A structural record
// states the units it touches; it does not rely on a path shape to imply them.
//
// # ID-KEYED
//
// The segment is the unit's KEY and the seat's HANDLE — the addresses the
// document itself uses — rather than a display name, for the same class of
// reason: a name is prose a founder edits at will, so a path built from one
// changes under a rename while the deferral row keeps the old spelling. A key
// changes too, but only through [KindRekey], which is an arbitrated record
// whose own apply is what rewrites the paths that name it — a change with a
// position on the log rather than one that happens between two probes.
//
// # The domain letter is on EVERY path
//
// `g` is the whole chart, and `g/u/...` sits under it, because the domain term
// is the widest-on-unreadable answer and an answer that covers nothing is not
// one: `g` beside `u/<key>` would make the root a SIBLING of every unit, so a
// record whose scope this build could not parse would block exactly nothing.
// The tracker's `t` and the pages log's `p` carry the same weight for the same
// reason; this domain's letter is `g` for the graph the chart is.
const (
	pathDomain = "g"
	pathUnit   = "u"
	pathSeat   = "s"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermSeat is one seat by its handle, WITHIN its unit.
	TermSeat TermKind = "seat"

	// TermUnit is one unit by its key, and every seat in it.
	TermUnit TermKind = "unit"

	// TermRoot is the whole chart. The fallback for an unreadable scope,
	// what a gate names, and what a batch past [MaxScopeTerms] collapses
	// to.
	TermRoot TermKind = "root"
)

// TermKinds are the three.
var TermKinds = []TermKind{TermSeat, TermUnit, TermRoot}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope.
//
// SIXTY-FOUR, which is this design's universal fan-out batch and the same
// number the tracker's cap is. It is larger than the knowledge base's sixteen
// because the widest record here is genuinely wide: a structural record that
// dissolves a unit names the unit, its parent and every seat it reparents, and
// an IMPORT rewrites whatever the company's document changed in one commit.
//
// A writer whose affected set would exceed it states [TermRoot] instead — see
// [BatchScope] — so the field is bounded by construction rather than by a cap a
// writer can hit and then have to handle. That collapse is the ONLY producer of
// the root term on a record this build wrote, and it is deliberate: a chart
// import past sixty-four objects is a reorganisation of the company, and
// "everything" is the honest blast radius for one.
const MaxScopeTerms = 64

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names a seat, a unit or the whole chart and nothing
// about the operation that produced it.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Unit is the unit key a seat lives in, or [RootUnit]. Required for
	// TermSeat, empty for the other two, and never guessed: see the
	// alphabet above.
	Unit string `json:"u,omitempty"`

	// ID is the seat's handle or the unit's key. Empty for TermRoot, which
	// names everything.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermSeat:
		return join(pathDomain, pathUnit, t.unit(), pathSeat, t.ID)
	case TermUnit:
		// THE UNIT TERM'S OWN NAME IS ITS ID, not its Unit field:
		// reading the field here would resolve every unit term to the
		// org root and make one team's deferral cover the whole chart.
		key := t.ID
		if key == "" {
			key = t.unit()
		}
		return join(pathDomain, pathUnit, key)
	case TermRoot:
		return pathDomain
	}
	// AN UNKNOWN TERM KIND IS THE WHOLE CHART, NOT A SKIPPED TERM. A term
	// this build cannot interpret is a claim about a blast radius it cannot
	// bound, and the only safe reading of an unbounded claim is the widest
	// one. Dropping it would silently narrow a newer peer's scope to
	// whatever this build happened to recognise.
	return pathDomain
}

// unit is the term's unit, defaulting to the org root.
func (t ScopeTerm) unit() string {
	if t.Unit == "" {
		return RootUnit
	}
	return t.Unit
}

// join builds a scope path from its segments, skipping empty ones so a missing
// id can never produce a path with a hole in it.
func join(segments ...string) string {
	kept := make([]string, 0, len(segments))
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, statelog.ScopeSeparator)
}

// ScopeSet is the COMPLETE set of objects a record's apply may write.
//
// NEVER EMPTY. The exactly-the-subject case encodes as the one-byte sentinel,
// so the field can never be absent by accident. An absent or unreadable scope
// on a record this build cannot decode is [TermRoot], never "just the subject":
// assuming a V1 convention holds for a V2 operation is the bug this field
// exists to close.
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	//
	// LEGAL ONLY FOR AN ADDRESSABLE KIND. A structural record, a key claim
	// and a barrier arbitrate on something that is not a row, so "exactly
	// the object my subject names" names nothing — see
	// [ObjectKind.Addressable] and [ScopeSet.Validate].
	Subject bool

	// Unit is the unit the subject lives in, and rides the sentinel.
	//
	// # Why the unit is ON THE RECORD and not derived from the subject
	//
	// A seat's subject is its handle, and which unit it sits in is a fact
	// about the row — one that CHANGES, because moving a seat between units
	// is the whole point of this domain. So the path a record's scope
	// resolves to cannot be computed from the subject alone, and the only
	// party that knows it at the moment the record is written is the
	// writer.
	//
	// Leaving it out is silent rather than wrong-looking: every seat record
	// would file its deferral under the org root while every unit-scoped
	// read probed its own, and the containment probe would simply never
	// match.
	//
	// Empty is [RootUnit], which is what a seat above every unit resolves
	// to.
	Unit string

	// Terms is the enumeration, when the record touches more than its own
	// subject. A unit rides the sentinel and never an enumeration: every
	// term already states its own.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// BatchScope is the scope a record touching many objects states.
//
// IT COLLAPSES PAST [MaxScopeTerms] TO [TermRoot], and that collapse is the one
// place in this package a record this build wrote names the whole chart. The
// alternative is a writer that hits the cap and then has to decide what to do
// about it, at every call site, forever — and the tracker's own rule is that an
// unbounded cascade is expressed in O(1) bytes or not at all.
//
// Two properties make the collapse safe rather than merely cheap. It only ever
// WIDENS: every path the enumeration would have produced is under `g`, so a
// probe that would have matched a term still matches the root. And it is a pure
// function of the term count, so two nodes forming the same batch form the same
// scope — which matters because the scope is what a third node files their
// deferral under.
//
// A caller passing no terms at all gets the root as well, for the reason the
// type's own doc gives: an empty scope claims the record makes nothing stale,
// and that is the one claim a record may not make.
func BatchScope(terms []ScopeTerm) ScopeSet {
	if len(terms) == 0 || len(terms) > MaxScopeTerms {
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermRoot}}}
	}
	return ScopeSet{Terms: slices.Clone(terms)}
}

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case is one byte on the wire.
//
// A unit is appended to the sentinel with the path separator — "s/eng" — which
// keeps the root-seat case at one byte and costs a unit key on the records that
// have one. The separator cannot appear inside a key: [Subject.Validate]
// refuses one that carries it, at the edge where a key is written.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		if s.Unit != "" {
			return json.Marshal(ScopeSentinel + statelog.ScopeSeparator + s.Unit)
		}
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the root.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set. This runs on a node reading a record a newer build wrote: the only
// honest reading of a blast radius it cannot parse is "everything", and
// returning an error here would take the whole two-pass decode down with it.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		head, unit, _ := strings.Cut(sentinel, statelog.ScopeSeparator)
		if head == ScopeSentinel {
			*s = ScopeSet{Subject: true, Unit: unit}
			return nil
		}
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermRoot}}}
		return nil
	}
	var terms []ScopeTerm
	if err := json.Unmarshal(b, &terms); err != nil || len(terms) == 0 {
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermRoot}}}
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT, per the
		// paragraph above: this runs on a node decoding a record a newer
		// build wrote, and returning the decode error here would fail the
		// envelope pass that exists precisely so such a record can be
		// filed, gated and reprocessed rather than dropped.
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Resolve renders the scope as the framework's own paths.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return statelog.ScopeSet{Paths: []string{subjectPath(subject, s.Unit)}}
	}
	paths := make([]string, 0, len(s.Terms))
	for _, t := range s.Terms {
		paths = append(paths, t.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject, unit string) string {
	switch s.Kind {
	case KindUnit:
		// A UNIT'S OWN PATH IS THE UNIT, which is what makes a deferred
		// settings edit block every write inside that team — which is
		// exactly what dissolving one is.
		return ScopeTerm{Kind: TermUnit, ID: s.ID}.Path()
	case KindSeat:
		return ScopeTerm{Kind: TermSeat, Unit: unit, ID: s.ID}.Path()
	case KindBarrier:
		// THE FRAMEWORK'S OWN SCOPE, so a barrier never intersects any
		// read's closure. A barrier writes no row anywhere, so a scope
		// that intersected anything would make every linearizable read
		// wait behind every other.
		return statelog.BarrierScope
	}
	// THE STRUCTURE, A KEY CLAIM, AND ANY KIND THIS BUILD DOES NOT KNOW.
	// None of the three names a row, so there is no narrower path to give:
	// a sentinel on one of them is either a peer whose grammar this build
	// cannot read or a writer that took a shortcut [ScopeSet.Validate]
	// refuses, and the only safe reading of either is the whole chart.
	return pathDomain
}

// Validate refuses a scope a writer could not have meant.
//
// It is the WRITE-SIDE half of the sentinel rule: [subjectPath] widens a
// sentinel on a non-addressable kind to the whole chart, because a record off
// the wire has to resolve to something, and this refuses one being written in
// the first place — so the widening only ever covers a peer's record and never
// this build's own laziness.
func (s ScopeSet) Validate(subject Subject) error {
	if s.Subject {
		if len(s.Terms) != 0 {
			return invalid("scope", "carries the exactly-the-subject sentinel "+
				"and %d enumerated term(s): the two say different things about "+
				"the same record", len(s.Terms))
		}
		if !subject.Kind.SentinelScoped() {
			return invalid("scope", "carries the exactly-the-subject sentinel "+
				"on a %s subject, which names no row — the structure and a key "+
				"claim each arbitrate on something the scope alphabet cannot "+
				"render, so a sentinel there widens silently to the whole "+
				"chart. A record on one of them enumerates the units and seats "+
				"its apply writes", subject.Kind)
		}
		if s.Unit != "" {
			// THE SEPARATOR IS WHAT THE SENTINEL ENCODING RESTS ON, and
			// a blank segment is what the path join drops. A unit
			// carrying either decodes as a DIFFERENT unit and files the
			// record's deferral under a path no probe reaches — so both
			// are refused where the value is accepted rather than
			// trusted where it is split.
			switch {
			case strings.Contains(s.Unit, statelog.ScopeSeparator):
				return invalid("scope.unit", "%q contains %q, which is the path "+
					"separator the sentinel encoding splits on — it would decode "+
					"as %q and file this record where no probe for it looks",
					s.Unit, statelog.ScopeSeparator,
					strings.SplitN(s.Unit, statelog.ScopeSeparator, 2)[0])
			case strings.TrimSpace(s.Unit) == "":
				return invalid("scope.unit", "%q is blank, and a blank segment "+
					"is dropped from the path — the record would resolve to its "+
					"unit's own path rather than to its own", s.Unit)
			}
		}
		return nil
	}
	if s.Unit != "" {
		return invalid("scope", "enumerates %d term(s) and also names unit %q: "+
			"every term states its own unit, so the field says nothing an "+
			"enumeration has not already said", len(s.Terms), s.Unit)
	}
	if len(s.Terms) == 0 {
		return invalid("scope", "is empty — a record that touches nothing "+
			"writes nothing, and an absent scope is read as the whole chart "+
			"rather than as the subject")
	}
	if len(s.Terms) > MaxScopeTerms {
		return invalid("scope", "enumerates %d terms and the cap is %d — a "+
			"writer whose affected set is larger states the covering root term "+
			"instead, which is what BatchScope does, so the field stays bounded "+
			"by construction", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermSeat, TermUnit:
			if t.ID == "" {
				return invalid("scope", "a %s term names nothing", t.Kind)
			}
		case TermRoot:
		default:
			return invalid("scope", "term kind %q is not one this build writes; "+
				"it is READ as the whole chart, which is safe, and WRITING one "+
				"is a writer that cannot say what it touched", t.Kind)
		}
	}
	return nil
}

// RootPath is the scope path that covers the whole chart.
//
// EXPORTED so a test can assert what no record kind produces without restating
// the alphabet — a constant compared against a literal spelled somewhere else
// is a guard that passes when the alphabet moves underneath it.
func RootPath() string { return pathDomain }

// String renders a term for a log line and for a refusal.
func (t ScopeTerm) String() string {
	if t.Kind == TermRoot {
		return string(t.Kind)
	}
	return fmt.Sprintf("%s(%s)", t.Kind, t.Path())
}
