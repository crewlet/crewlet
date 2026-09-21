package chart

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The org chart as a state-log DOMAIN: what a record is, and what the subject
// is for.
//
// Every change is one RECORD on one ordered stream, published to the subject of
// the object it changes and conditioned on that object's last sequence there.
// The subject is the ARBITRATION UNIT rather than a routing label: two writers
// editing one seat contend at the broker and exactly one wins, and two writers
// on different seats never contend at all.
//
// # Why the STRUCTURE has a subject of its own
//
// It is the one place this domain departs from the shape the tracker and the
// knowledge base share, and the package doc argues it in full. In one
// sentence: containment in an org chart is the object rather than a field on
// the thing contained, so two moves that are each locally valid can jointly
// make a cycle — and a cycle is unreachable from any per-object subject,
// because neither writer ever looked at the other's. One subject for the whole
// structure makes the joint state a thing exactly one writer at a time can
// produce.
//
// The cost is stated rather than glossed: every reorganisation in the company
// serialises on [KindTree]. That is the rarest write this company makes — a
// chart changes when somebody is hired, moved or promoted — and the ordinary
// traffic, a seat's own content, does not touch it at all.
//
// What the record must still state is the SCOPE — every object its apply
// touches — because that is what a node which cannot decode it files the
// deferral under.

// ObjectKind is what an object on the chart log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of — and the literal is RETAINED either way, for the reason
// [tracker.ObjectKind] gives: a newer peer publishes a kind this build does not
// know, and the deferral this build files it under forms a scope term out of
// that literal.
type ObjectKind string

// The five kinds.
//
// EXPORTED AND ENUMERATED because four readers that cannot see each other all
// compare against them: the publisher builds the subject, the wake feed's
// filter includes or excludes the kind, the applier's dispatch switches on it,
// and the domain's own Tables declaration must classify every one.
const (
	// KindTree is the chart's STRUCTURE, on ONE subject for the whole
	// domain: who is under whom, who leads what, and what the chart no
	// longer names.
	//
	// IT HAS NO ID, because there is exactly one structure. That is what
	// makes a reparent contend with every other reparent — see the header
	// above for why that serialisation is bought deliberately.
	//
	// IT IS NOT A ROW. The structure lives in `chart_units.parent_key` and
	// `chart_seats.unit_key`, which is why a record on this subject can
	// never use the exactly-the-subject sentinel: "the object my subject
	// names" would name nothing at all. A structural record ENUMERATES the
	// units and seats it touches, and [ScopeSet.Validate] refuses the
	// sentinel here.
	KindTree ObjectKind = "tree"

	// KindUnit is one unit's own CONTENT — its name, its purpose, its
	// goals, its channel, its tracker and knowledge identities, its tool
	// credentials.
	//
	// Its id is the unit's KEY, which is what makes a deferred settings
	// edit block every write inside that unit: the containment the scope
	// alphabet is for.
	KindUnit ObjectKind = "unit"

	// KindSeat is one seat's own CONTENT — its backstory, its goal, its
	// responsibilities, its per-phase model chains, its contact
	// identities.
	//
	// Its id is the seat's HANDLE, which is the identity every other
	// subsystem already addresses a seat by: the mailbox, the derived
	// agent id, the external accounts, and every `lead:` and `manages:`
	// entry in the document.
	KindSeat ObjectKind = "seat"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain.
	//
	// The only kind that writes no row on any node, which is why its table
	// declaration is the EMPTY set stated explicitly rather than left out.
	KindBarrier ObjectKind = "barrier"

	// KindRekey is a claim on one KEY — a unit key or a seat handle — and
	// the maintenance half of this domain.
	//
	// ITS SUBJECT IS THE KEY ITSELF, create-only at an expectation of zero,
	// for the reason a page's create arbitrates on its title: two objects
	// taking one address must contend, and two objects' own subjects never
	// would. A key that moves is not a cosmetic change — it is what every
	// `manages:` entry, every `lead:`, every inbound webhook mapping and
	// every mailbox name resolves through — so the claim is the whole
	// operation.
	//
	// IT IS NOT A ROW EITHER, and it carries no sentinel for the same
	// reason [KindTree] does not: a key is an address, and the object that
	// takes it is what the apply writes. The record enumerates that object.
	KindRekey ObjectKind = "rekey"
)

// ObjectKinds are the five, and THE ORDER IS LOAD-BEARING.
//
// [statelogtest] publishes the FIRST THREE a domain declares, twice each, in
// order — so the declaration decides what the framework's own suite certifies,
// and reordering this list silently stops certifying whatever falls past the
// third place.
//
// These three are a real sequence rather than three unrelated records: the
// structure places a unit, that unit's content is written, and then a seat
// inside it is written. A unit's content record before the structure that
// created it, or a seat before its unit, is a malformed record under a strict
// replay — so any other order would certify a failure rather than a domain.
var ObjectKinds = []ObjectKind{
	KindTree, KindUnit, KindSeat, KindBarrier, KindRekey,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// FOUR OF FIVE DO. The barrier shares one subject across the whole domain, so
// an expectation there would serialise every linearizable read behind every
// other one and write an anchor row per read into the transaction holding this
// store's only writer.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier }

// SentinelScoped reports whether a record on this kind may state the
// exactly-the-subject sentinel as its scope.
//
// THREE OF FIVE MAY, and for two different reasons. A unit and a seat are rows
// the scope alphabet renders, so "exactly the object my subject names" is a
// path. The BARRIER may too, and its sentinel resolves to the framework's own
// [statelog.BarrierScope] rather than to anything in this alphabet — which is
// the whole point of it, since a barrier writes no row and a scope that
// intersected anything would make every linearizable read wait behind every
// other.
//
// The STRUCTURE and a KEY CLAIM may not. Neither names a row, so a sentinel on
// one of them has no narrower path than the whole chart — and a record that
// silently widened to the whole chart is a reorganisation that blocks every
// read in the company while looking, in the row an operator reads, exactly like
// a record that touched one seat. They ENUMERATE instead, which is what
// [ScopeSet.Validate] enforces.
//
// This is a method rather than a list beside the enum for the reason
// [Domain.Stream]'s arbitrated kinds are derived: a rule written twice is a
// kind that is sentinel-scoped in one place and not the other, and the two
// disagree the first time a writer takes the shorter path.
func (k ObjectKind) SentinelScoped() bool {
	return k == KindUnit || k == KindSeat || k == KindBarrier
}

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// TreeSubject is the structure's one subject.
//
// There is a constructor per kind rather than a Subject{Kind, ID} literal at
// every call site, because a key's id is NORMALISED on the way in and a
// normalisation written twice is a subject two writers disagree about.
func TreeSubject() Subject { return Subject{Kind: KindTree} }

// UnitSubject names one unit by its key, which is what every change to that
// unit's own content contends on.
func UnitSubject(key string) Subject {
	return Subject{Kind: KindUnit, ID: NormalizeKey(key)}
}

// SeatSubject names one seat by its handle.
func SeatSubject(handle string) Subject {
	return Subject{Kind: KindSeat, ID: NormalizeKey(handle)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// RekeySubject names one claim on one key.
//
// THE KEY IS NORMALISED HERE, once, so a caller cannot arbitrate on the
// author's own capitalisation: `Platform` and `platform` are one address, and
// two subjects would make them two — which is precisely the duplicate-unit-key
// failure the organisation model refuses a document for.
//
// # Why the key is the key and not a digest
//
// A page's title is arbitrated on a digest because a title is PROSE: it
// carries spaces, dots and wildcards, all of which a broker path forbids, and
// escaping it would mean a second alphabet that has to agree with the first
// for ever. A unit key and a seat handle are not prose — they are slugs a
// person types into `manages:` and `lead:`, bounded at [MaxKey] — so the key
// travels as itself and an operator reading the log sees the address that is
// being claimed rather than sixteen bytes of hex. [Subject.Validate] is what
// keeps that true: a key carrying a separator or a wildcard is refused where it
// is written.
func RekeySubject(key string) Subject {
	return Subject{Kind: KindRekey, ID: NormalizeKey(key)}
}

// String renders the subject's own path — what the framework appends to the
// domain's subject prefix, and what a scope term names.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string {
	return topics.ChartLogSubject(string(s.Kind), s.ID)
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN and accepted where one is READ, which is the asymmetry the
// whole two-pass decode exists for: this build must be able to hold a newer
// peer's record under its own subject without being able to act on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("chart: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("chart: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again",
			s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier && s.Kind != KindTree {
		return fmt.Errorf("chart: a %s subject needs an id — the structure and "+
			"the barrier are the only kinds with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("chart: subject id %q carries whitespace or a "+
			"wildcard, which the broker would read as a subject pattern", s.ID)
	}
	// THE SCOPE SEPARATOR TOO, and only here. A unit key and a seat handle
	// are SEGMENTS of every scope path their records are filed under, so a
	// key carrying the separator would decode as a different path and file
	// the record where no probe for it ever looks. The broker would have
	// taken it happily.
	if strings.Contains(s.ID, scopeSeparator) {
		return fmt.Errorf("chart: subject id %q contains %q, which is the scope "+
			"path separator — the record would be filed under a path no probe "+
			"for this object reaches", s.ID, scopeSeparator)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the chart log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.ChartLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
