// Package iamdomain is the company's own identity estate — people, the
// addresses and logins they are known by, the credentials they hold, the
// invitations that enrolled them and the sessions they are signed in with — as
// the state-log framework's FOURTH domain and its third STRICT one.
//
// internal/iam stays the VALUES LEAF it was built as: a principal, its kind
// and the grants it carries, importable from config, the tool layer and the
// query registry without any of them pulling in a store. This package is the
// durable half, and it imports that leaf rather than restating it.
//
// # ONE SUBJECT FOR THE DIRECTORY, and why that is the whole design
//
// The replicated estate forbids a UNIQUE index outside a primary key, because
// a constraint violation inside an apply transaction aborts it
// DETERMINISTICALLY on every node at once — a rare cosmetic anomaly becomes a
// fleet-wide stalled log with no partial-failure arm to recover through. So
// there is no uniqueness constraint anywhere in this estate, and there is
// nothing to add one to.
//
// What keeps an address, a login and a seat binding to one holder instead is
// that EVERY WRITE THAT SETS OR FREES ONE IS A RECORD ON ONE SUBJECT — the
// directory ([KindDirectory]): an enrolment, a redemption, an invitation, an
// identity change and a removal. Its decide reads the whole directory in its
// own snapshot, refuses a value somebody else holds by naming them
// ([ErrTaken]), and publishes at that one subject's arbitration anchor — so two
// directory writes contend at the broker and the loser decides again from rows
// that hold the winner. It is the org chart's structure rule applied to the
// directory: directory writes are an administrator's or an invitation's, so
// serialising them costs nothing, and a uniqueness question answered by one
// read in one snapshot needs no sequence, no reservation and no release.
//
// EVERYTHING THAT SETS NOTHING UNIQUE STAYS OFF IT. A person's own content —
// their name, grants, credentials, stage and revocation epoch — arbitrates on
// their own subject, and a session on its lineage, so two administrators
// editing two people never contend and a thousand people signing in at nine
// o'clock do not either.
//
// # Two subjects meet on one row, and never on one column
//
// The directory CREATES a person's row, sets and clears its three unique
// columns, and DELETES it; the person's own subject owns the document and the
// columns derived from it. A directory record after the enrolment never
// touches the document, and a person record never touches a unique column and
// never creates a row — so the two halves are disjoint, and since every record
// about one person is applied in log order, one `version` is the whole guard.
//
// # What a record states that it is not the subject of
//
// Every record declares a SCOPE — the set of identity BUCKETS its apply may
// write — because that is what a node which cannot decode it files the
// deferral under. The buckets are this domain's own partition and `scope.go`
// argues them; what matters here is that a directory record's subject names
// nobody and a session's names a lineage, and the person each is about is
// inside a payload the deferring node cannot read. The scope is the only thing
// that can connect the two, which is why it is on the envelope and readable at
// every version.
//
// # A seat is bound by its HANDLE (ADR-0013)
//
// A seat binding — the row's `seat_id`, the seat a removal's tombstone
// records, the successor's stamp on that tombstone, and every reading of them
// ([Reader.SeatHolders],
// [Reader.SeatBindings], the notify registry's standing, the request path's
// seat table, the dangling-binding rule) — names the seat by its handle, which
// is immutable in the company document: a document that changes a handle has
// removed one seat and created another, and the bind checks the seat against
// the running organisation ([Writer.seatOf]).
package iamdomain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// ObjectKind is what a subject on the iam log addresses.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of — and the literal is RETAINED either way: a newer peer publishes a
// kind this build does not know, and the deferral this build files it under is
// reported to an operator with that literal in it.
type ObjectKind string

// The eight kinds.
//
// EXPORTED AND ENUMERATED because four readers that cannot see each other all
// compare against them: the publisher builds the subject, the wake feed's
// filter includes or excludes the kind, the applier's dispatch switches on it,
// and the domain's own Tables declaration must classify every one.
const (
	// KindDirectory is the company's whole directory, on ONE subject with
	// no id: every record that sets or frees a unique identity value — a
	// person's login, their address and the seat they hold — and every
	// record that creates or removes a person row. See the package doc.
	//
	// ONE OBJECT, DELIBERATELY. Two administrators enrolling one address,
	// a redemption racing an administrator's create, a rename racing an
	// enrolment that takes the same login: each is a decide that reads
	// the whole directory, and only one subject makes the snapshot it
	// read the one the broker arbitrates against. Directory writes are an
	// administrator's or an invitation's, so the serialisation costs
	// nothing a person would notice.
	//
	// NOT ROOT-SCOPED, although it is about everybody's values: a record
	// here declares the buckets of the rows its apply writes, so a node
	// that cannot decode one holds back the people it is about rather
	// than every sign-in in the company. What a decide here adds — that
	// the WHOLE directory it read is complete — it asks of the deferral
	// index itself ([wholeDirectory]).
	KindDirectory ObjectKind = "directory"

	// KindPerson is one person, machine or human, by the uuid7 that was
	// minted for them and that nothing ever renames.
	//
	// EVERYTHING ABOUT A PERSON THAT IS NOT UNIQUE arbitrates here: their
	// name, their status, their grants, their credentials, and the
	// REVOCATION EPOCH that ends every session they hold. Two
	// administrators editing one person contend; two editing two people
	// never do. A person's row is created and removed on the directory,
	// never here.
	KindPerson ObjectKind = "person"

	// KindSession is one signed-in session's whole life, by its LINEAGE:
	// the uuid7 the session was opened with, which never changes while it
	// lives.
	//
	// ONE SUBJECT PER SESSION, so two requests closing one session contend
	// and a thousand people signing in at nine o'clock do not contend at
	// all. A re-issue is NOT a record: the idle deadline it moves is in the
	// bearer's signed payload, so the busiest thing a session does writes
	// nothing at all.
	KindSession ObjectKind = "session"

	// KindInvalidation ends EVERY session in the company at once, on ONE
	// subject for the whole domain, with no id.
	//
	// IT IS NOT CALLED A GENERATION, although the counter it moves is the
	// one a bearer carries under that name and the row it writes is
	// `iam_session_generation`. [KindGeneration] already means the LOG's
	// own reanchor here and in every sibling domain, and two closed sets
	// under one word is how a later change reaches for the wrong one while
	// each side stays self-consistent — the same reason [iam.Stage] is not
	// called a posture. So the kind is named for what it DOES, and the
	// counter keeps the name the bearer already spells it by.
	//
	// ONE OBJECT, DELIBERATELY: two operators invalidating the company's
	// sessions must contend, because the whole value of the gesture is that
	// nothing issued before it survives, and two concurrent bumps that did
	// not contend would each read the same current value and write the same
	// new one — leaving every cookie minted between them valid.
	//
	// IT INSTALLS A GATE ([OpInvalidate]), which is what buys it the root
	// scope: a node that could not decode it would go on honouring every
	// bearer the company had just ended, with no later record that repairs
	// that, and there is no bucket to file it under because it is about
	// nobody in particular.
	KindInvalidation ObjectKind = "invalidation"

	// KindSweep is the retention sweep for ONE bucket, by the bucket
	// number.
	//
	// A RECORD AND NOT A LOCAL DELETE, because the rows it removes are
	// identity-claimed: a node that swept on its own clock would hold
	// different bytes from its peers, and the claim that N copies are
	// byte-identical would become a claim about how synchronised their
	// clocks were. The record names a POSITION RANGE the publisher
	// resolved once, so two nodes with skewed clocks delete identical
	// rows.
	//
	// PER BUCKET rather than one record for the estate, because a sweep is
	// bounded by the applier's row budget and a company-wide delete is
	// not: sixty-four records spread one horizon's worth of deletions
	// across sixty-four transactions instead of holding this store's only
	// writer for the length of the largest one.
	KindSweep ObjectKind = "sweep"

	// KindEviction is a node's removal from THIS log, or its readmission.
	//
	// ONE SUBJECT PER NODE, so two operators evicting two nodes never
	// contend and two evicting one do. It is the domain's own copy of the
	// gate every state log installs: the record states a POSITION, and
	// every node then reaches the same verdict about every record with no
	// clock, no coordination read and no agreement beyond the order they
	// already share.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's own record, create-only at an
	// expectation of zero on a fresh stream, by the generation number.
	//
	// THE LOG'S GENERATION, never a session's. What ends every session in
	// the company is [KindInvalidation], which moves a different counter
	// for a different reason — the distinction is stated at both constants
	// because the word alone cannot carry it.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain.
	//
	// The only kind that writes no row on any node, which is why its table
	// declaration is the EMPTY set stated explicitly rather than left out.
	KindBarrier ObjectKind = "barrier"
)

// ObjectKinds are the eight, and THE ORDER IS LOAD-BEARING.
//
// [statelogtest] publishes the FIRST THREE a domain declares, twice each, in
// order — so the declaration decides what the framework's own suite certifies,
// and reordering this list silently stops certifying whatever falls past the
// third place.
//
// These three are a real sequence rather than three unrelated records: a
// person is enrolled (the directory), their content changes (their own
// subject), and they sign in (a session). A content record for a person
// nobody enrolled is a record whose apply has nothing to change, so any other
// order would certify a failure rather than a domain.
var ObjectKinds = []ObjectKind{
	KindDirectory, KindPerson, KindSession,
	KindInvalidation, KindSweep, KindEviction, KindGeneration, KindBarrier,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// SEVEN OF EIGHT DO. The barrier shares one subject across the whole domain, so
// an expectation there would serialise every linearizable read behind every
// other one and write an anchor row per read into the transaction holding this
// store's only writer.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier }

// Identified reports whether this kind's subject carries an id.
//
// FIVE OF EIGHT DO. The directory, the invalidation and the barrier are the
// kinds with exactly one object in the whole domain, and each is a singleton
// for a reason stated at its constant rather than because an id was hard to
// choose.
func (k ObjectKind) Identified() bool {
	switch k {
	case KindDirectory, KindInvalidation, KindBarrier:
		return false
	}
	return true
}

// RootScoped reports whether a record on this kind may state the whole estate
// as its scope.
//
// FOUR OF EIGHT MAY, and it is the tightest rule in this package because the
// cost of the root term here is the highest in the tree: a deferred record at
// the root blocks every read whose closure it covers, which is every read in
// the domain — so one record a node cannot decode would freeze every
// suspension, every revocation and every login in the company at once, during
// an ordinary rolling upgrade.
//
// An EVICTION may and pays nothing for it, because it INSTALLS A GATE: a
// version this build cannot read STOPS the applier rather than being filed at
// a path every read queues behind. A GENERATION may and does pay it, correctly
// — a node that cannot decode a record saying this log was reanchored cannot
// certify any read over it either. A BARRIER may because its scope is the
// framework's own [statelog.BarrierScope], which intersects nothing, and what
// it declares is never read. An INVALIDATION may for the eviction's reason and
// must: it installs a gate too, so it is never deferred, and it is about
// everybody — a bucket would be a claim that it ends one sixty-fourth of the
// company's sessions.
func (k ObjectKind) RootScoped() bool {
	switch k {
	case KindInvalidation, KindEviction, KindGeneration, KindBarrier:
		return true
	}
	return false
}

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// PersonSubject names one person by the id nothing renames.
//
// There is a constructor per kind rather than a Subject{Kind, ID} literal at
// every call site, because several of these ids are DERIVED — a bucket number,
// a generation — and a derivation written twice is a subject two writers
// disagree about.
func PersonSubject(personID string) Subject {
	return Subject{Kind: KindPerson, ID: personID}
}

// DirectorySubject is the company's one directory: see [KindDirectory].
func DirectorySubject() Subject { return Subject{Kind: KindDirectory} }

// SessionSubject names one session's whole life by its lineage.
func SessionSubject(lineage string) Subject {
	return Subject{Kind: KindSession, ID: lineage}
}

// InvalidationSubject is the company's one session-invalidation counter.
func InvalidationSubject() Subject { return Subject{Kind: KindInvalidation} }

// SweepSubject names one bucket's retention sweep.
//
// THE BUCKET IS RENDERED ZERO-PADDED, so the subject an operator reads sorts
// the way the buckets do and a listing of sixty-four of them is in order
// rather than in ASCII order.
func SweepSubject(b Bucket) Subject {
	return Subject{Kind: KindSweep, ID: b.String()}
}

// EvictionSubject names one node's gate on this log.
//
// The id is NOT folded: a node id is not an address a person types, it is what
// that node published as its own writer, and folding it here would make the
// gate miss every record the node wrote.
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}

// GenerationSubject names one reanchor.
//
// The id is the generation number, which is what makes the record create-only:
// a second reanchor to one generation is the same subject at an expectation
// that is no longer zero, and the broker refuses it.
func GenerationSubject(generation uint32) Subject {
	return Subject{Kind: KindGeneration,
		ID: strconv.FormatUint(uint64(generation), 10)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// String renders the subject's own path — what the framework appends to the
// domain's subject prefix.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string {
	return topics.IamLogSubject(string(s.Kind), s.ID)
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN and accepted where one is READ, which is the asymmetry the
// whole two-pass decode exists for: this build must be able to hold a newer
// peer's record under its own subject without being able to act on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("iamdomain: a subject with no kind addresses the " +
			"log's own prefix, which is a real subject inside the stream's " +
			"wildcard that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("iamdomain: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again",
			s.Kind)
	}
	if s.ID == "" && s.Kind.Identified() {
		return fmt.Errorf("iamdomain: a %s subject needs an id — the "+
			"directory, the invalidation and the barrier are the only kinds "+
			"with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("iamdomain: subject id %q carries whitespace or a "+
			"wildcard, which the broker would read as a subject pattern", s.ID)
	}
	// THE SCOPE SEPARATOR IS NOT REFUSED HERE, and the difference from the
	// chart is worth stating rather than leaving as an omission: there, a
	// unit key is a SEGMENT of every scope path its records are filed
	// under, so a key carrying the separator would file the record where no
	// probe reaches. Here a scope path is built from a BUCKET NUMBER and
	// never from a subject id, so no id of any kind reaches a path — which
	// is one of the things the bucketed scope buys.
	//
	// A DOT IS NOT REFUSED EITHER: a node id may carry one, and
	// topics.IamLogPath splits only the FIRST dot, so an id keeps every
	// dot it was published with.
	return nil
}

// ParseSubject recovers a subject from a wire subject on the iam log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.IamLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
