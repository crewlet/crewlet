// Package iamdomain is the company's own identity estate — people, the
// addresses and logins they are known by, the credentials they hold, the
// invitations that enrolled them and the sessions they are signed in with — as
// the state-log framework's FIFTH domain and its fourth STRICT one.
//
// internal/iam stays the VALUES LEAF it was built as: a principal, its kind
// and the grants it carries, importable from config, the tool layer and the
// query registry without any of them pulling in a store. This package is the
// durable half, and it imports that leaf rather than restating it.
//
// # ONE SUBJECT PER CLAIM, and why that is the whole design
//
// The replicated estate forbids a UNIQUE index outside a primary key, because
// a constraint violation inside an apply transaction aborts it
// DETERMINISTICALLY on every node at once — a rare cosmetic anomaly becomes a
// fleet-wide stalled log with no partial-failure arm to recover through. So
// there is no uniqueness check anywhere in this estate, and there is nothing
// to add one to.
//
// What makes an address, a login and a seat binding unique instead is the
// SUBJECT GRAMMAR: each claim arbitrates on ITSELF, create-only at an
// expectation of zero, so two operators enrolling one address contend at the
// broker and exactly one wins. It is the knowledge base's create rule —
// arbitrate on the TITLE, because two writers must contend for a name and two
// uuids never would — applied to every address a person can be known by.
//
// The consequence is that NO TWO CLAIMS MAY SHARE A SUBJECT. A single
// `iam.claim.<something>` carrying both an email and a login would make two
// people taking two different unclaimed addresses contend with each other,
// which is the opposite failure: correct, and serialising every enrolment in
// the company behind one subject. A shared subject in the other direction —
// two distinct claims folded onto one token — is worse and silent: the second
// claim never contends at all, and the duplicate identity nobody refused is
// discovered by a person signing in as somebody else.
//
// # A person is minted, and the claims are taken one at a time
//
// A person's id is a uuid7 that nothing renames, and every claim record names
// it. An enrolment is therefore a SEQUENCE — take the address, take the login,
// write the person — for the reason the tracker's dependency edge is one: each
// end arbitrates on its own subject, and a record has exactly one subject to
// arbitrate on. A sequence that stops halfway leaves a CLAIMED ADDRESS WITH NO
// PERSON, which is a legal named state rather than corruption: the duplicate
// and orphan claim duty reports it, and the sweep collects it.
//
// # What a record states that it is not the subject of
//
// Every record declares a SCOPE — the set of identity BUCKETS its apply may
// write — because that is what a node which cannot decode it files the
// deferral under. The buckets are this domain's own partition and `scope.go`
// argues them; what matters here is that a claim's subject is a BLIND or a
// token, and the person it concerns is inside a payload the deferring node
// cannot read. The scope is the only thing that can connect the two, which is
// why it is on the envelope and readable at every version.
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
// heard of — and the literal is RETAINED either way, for the reason
// [chart.ObjectKind] gives: a newer peer publishes a kind this build does not
// know, and the deferral this build files it under is reported to an operator
// with that literal in it.
type ObjectKind string

// The eleven kinds.
//
// EXPORTED AND ENUMERATED because four readers that cannot see each other all
// compare against them: the publisher builds the subject, the wake feed's
// filter includes or excludes the kind, the applier's dispatch switches on it,
// and the domain's own Tables declaration must classify every one.
const (
	// KindPerson is one person, machine or human, by the uuid7 that was
	// minted for them and that nothing ever renames.
	//
	// EVERYTHING ABOUT A PERSON THAT IS NOT A CLAIM arbitrates here: their
	// name and contacts, their status, their grants, their credentials,
	// and the REVOCATION EPOCH that ends every session they hold. Two
	// administrators editing one person contend; two editing two people
	// never do.
	KindPerson ObjectKind = "person"

	// KindEmail is a claim on one email ADDRESS, and its id is the keyed
	// BLIND rather than the address.
	//
	// A blind because a subject is a broker path: it is carried in
	// cleartext in every delivery, every consumer's filter and every
	// operator's stream listing, and an address is personal data. The
	// blind is a keyed hash, so the broker sees a token, a node holding
	// the key can compute it from an address, and nobody else can go the
	// other way. `blind.go` owns the derivation.
	//
	// It is also what an INVITATION arbitrates on, which is the point: an
	// invitation is a claim on an address by somebody who does not have a
	// person yet, so an invite and an enrolment for one address must
	// contend, and they do because they share this subject.
	KindEmail ObjectKind = "email"

	// KindLogin is a claim on one LOGIN — a person's `jane.doe` or a
	// machine's `ci:release`.
	//
	// THE SHAPE IS THE HOLDER'S KIND, and a claim is refused in its decide
	// when the two disagree ([iam.ValidLoginFor]): `token:<id>` is the
	// login a Tier A token acts under, so a person holding one would make
	// the deployment's credential act as their seat.
	//
	// ITS ID IS THE LOGIN ITSELF, not a blind, and the asymmetry with
	// [KindEmail] is deliberate. A login is a name the company CHOSE, in a
	// grammar internal/iam defines, typed into an audit row and read back
	// by everybody; an email address is a person's own contact detail that
	// reaches this engine from outside it. Blinding a value the dashboard
	// prints beside every change would cost an operator the ability to
	// read their own log for nothing.
	KindLogin ObjectKind = "login"

	// KindSeat is a claim binding one person to one SEAT, by the seat's
	// HANDLE.
	//
	// BY THE HANDLE because there is nothing else to bind to: `chart_seats`
	// is keyed on it and stores no derived id, so a binding to a UUIDv5 the
	// chart never wrote down would resolve to nothing on every node. A
	// rename is survived the way every other written-down reference to a
	// seat survives one — through the chart's `former_keys_json`, whose own
	// doc states the residue: a former handle goes on resolving until
	// something else claims it, and then the claimant wins.
	//
	// The claim is on the SEAT rather than on the person because that is
	// the side that must be exclusive: one seat is held by at most one
	// person, and two administrators binding two people to one seat have
	// to contend. A person holding no seat is ordinary, and a person
	// holding a seat that the chart has since removed is a LEGAL named
	// state the session layer answers with a 403 naming the seat — not a
	// state this domain can prevent, because the chart is a different log
	// and a read of it guarantees nothing.
	KindSeat ObjectKind = "seat"

	// KindSession is one signed-in session's whole life, by its LINEAGE:
	// the uuid7 the session was opened with, which its rotations are
	// derived from and which never changes while it lives.
	//
	// ONE SUBJECT PER SESSION, so two requests closing one session contend
	// and a thousand people signing in at nine o'clock do not contend at
	// all. Rotations are NOT records: a rotation id is an HMAC over the
	// lineage and the session's age in rotate_after units, so the busiest
	// thing a session does writes nothing at all.
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
	// ONE OBJECT, for [KindBootstrap]'s reason turned round: two operators
	// invalidating the company's sessions must contend, because the whole
	// value of the gesture is that nothing issued before it survives, and
	// two concurrent bumps that did not contend would each read the same
	// current value and write the same new one — leaving every cookie
	// minted between them valid.
	//
	// IT INSTALLS A GATE ([OpInvalidate]), which is what buys it the root
	// scope: a node that could not decode it would go on honouring every
	// bearer the company had just ended, with no later record that repairs
	// that, and there is no bucket to file it under because it is about
	// nobody in particular.
	KindInvalidation ObjectKind = "invalidation"

	// KindBootstrap is the company's FIRST-PERSON bootstrap, on ONE
	// subject for the whole domain, with no id.
	//
	// ONE OBJECT, DELIBERATELY: a bootstrap code is what turns an engine
	// that nobody can sign in to into one with an administrator, so two
	// live bootstraps is two ways in. Sharing one subject means two nodes
	// minting a code contend and exactly one wins, which is the whole
	// property — and it costs nothing, because this is the rarest write
	// the company ever makes and it happens once.
	KindBootstrap ObjectKind = "bootstrap"

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

// ObjectKinds are the eleven, and THE ORDER IS LOAD-BEARING.
//
// [statelogtest] publishes the FIRST THREE a domain declares, twice each, in
// order — so the declaration decides what the framework's own suite certifies,
// and reordering this list silently stops certifying whatever falls past the
// third place.
//
// These three are a real sequence rather than three unrelated records: a
// person is minted, their address is claimed for them, and their login is
// claimed for them. A claim naming a person nobody minted is a record whose
// apply has nothing to attach to, so any other order would certify a failure
// rather than a domain.
var ObjectKinds = []ObjectKind{
	KindPerson, KindEmail, KindLogin, KindSeat, KindSession,
	KindInvalidation, KindBootstrap, KindSweep, KindEviction,
	KindGeneration, KindBarrier,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// TEN OF ELEVEN DO. The barrier shares one subject across the whole domain, so
// an expectation there would serialise every linearizable read behind every
// other one and write an anchor row per read into the transaction holding this
// store's only writer.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier }

// Identified reports whether this kind's subject carries an id.
//
// EIGHT OF ELEVEN DO. The bootstrap, the invalidation and the barrier are the
// three kinds with exactly one object in the whole domain, and each is a
// singleton for a reason stated at its constant rather than because an id was
// hard to choose.
func (k ObjectKind) Identified() bool {
	switch k {
	case KindBootstrap, KindInvalidation, KindBarrier:
		return false
	}
	return true
}

// RootScoped reports whether a record on this kind may state the whole estate
// as its scope.
//
// FOUR OF ELEVEN MAY, and it is the tightest rule in this package because the
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
//
// THE BOOTSTRAP MAY NOT, although it is the other kind with no id and would
// be the natural place to reach for "the whole estate". It writes a row like
// any other record and it is deferrable like any other record, so it takes
// [BootstrapBucket] — the bucket of its own subject — and a node that cannot
// decode it blocks a sixty-fourth of the domain rather than all of it.
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
// every call site, because several of these ids are DERIVED — a blind, a
// folded login, a bucket number — and a derivation written twice is a subject
// two writers disagree about.
func PersonSubject(personID string) Subject {
	return Subject{Kind: KindPerson, ID: personID}
}

// EmailSubject names a claim on one address, taking the BLIND rather than the
// address: this package never sees a cleartext address in a subject, so no
// caller can accidentally publish one.
func EmailSubject(blind string) Subject {
	return Subject{Kind: KindEmail, ID: blind}
}

// LoginSubject names a claim on one login.
//
// THE LOGIN IS FOLDED HERE, once, so a caller cannot arbitrate on the
// author's own capitalisation: `Jane.Doe` and `jane.doe` are one address, and
// two subjects would make them two people — which is exactly the duplicate
// nothing in this estate can refuse after the fact.
func LoginSubject(login string) Subject {
	return Subject{Kind: KindLogin, ID: strings.ToLower(strings.TrimSpace(login))}
}

// SeatSubject names a claim on one seat, by its derived id.
func SeatSubject(seatID string) Subject {
	return Subject{Kind: KindSeat, ID: seatID}
}

// SessionSubject names one session's whole life by its lineage.
func SessionSubject(lineage string) Subject {
	return Subject{Kind: KindSession, ID: lineage}
}

// BootstrapSubject is the company's one bootstrap.
func BootstrapSubject() Subject { return Subject{Kind: KindBootstrap} }

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
		return fmt.Errorf("iamdomain: a %s subject needs an id — the bootstrap "+
			"and the barrier are the only kinds with exactly one object", s.Kind)
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
	// A DOT IS NOT REFUSED EITHER, and it must not be: a person's login is
	// `jane.doe` by construction, because internal/iam requires the dot to
	// tell a login apart from a seat handle in an audit row. topics.IamLogPath
	// splits only the FIRST dot for exactly that reason.
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
