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
// Every node that holds the estate applies it, company or none, so every node
// that validates a session reads its own identity rows; a node without `data`
// asks a data node through internal/estate's identity reads.
//
// # ONE SUBJECT FOR THE DIRECTORY, and why that is the whole design
//
// The replicated estate forbids a UNIQUE index outside a primary key, because
// a constraint violation inside an apply transaction aborts it
// DETERMINISTICALLY on every data node at once — a rare cosmetic anomaly
// becomes a fleet-wide stalled log with no partial-failure arm to recover
// through. So there is no uniqueness constraint anywhere in this estate, and
// there is nothing to add one to.
//
// What keeps an address, a login and a seat binding to one holder instead is
// that EVERY WRITE THAT SETS OR FREES ONE IS A RECORD ON ONE SUBJECT — the
// directory ([KindDirectory]): an administrator's create, a redemption, an
// invitation and its cancellation, an identity change and a removal. An OPEN
// INVITATION holds an address AND a seat as surely as a person does, from its
// issue until it is redeemed, cancelled or ages out. Its decide reads the
// whole directory in its own snapshot, refuses a value somebody else holds —
// a person or an open invitation — by naming them
// ([ErrTaken]), and publishes at that one subject's arbitration anchor — so two
// directory writes contend at the broker and the loser decides again from rows
// that hold the winner. Directory writes are an administrator's or an
// invitation's, so serialising them on one subject costs nothing, and a
// uniqueness question answered by one read in one snapshot needs no sequence,
// no reservation and no release.
//
// EVERYTHING THAT SETS NOTHING UNIQUE STAYS OFF IT. A person's own content —
// their name, grants, credentials, stage and revocation epoch, and a new
// password with the epoch it moves in one record ([OpPassword]) — arbitrates on
// their own subject, and a session on its lineage, so two administrators
// editing two people never contend and a thousand people signing in at nine
// o'clock do not either.
//
// # Two subjects meet on one row, and never on one column
//
// The directory CREATES a person's row, sets its three unique columns, and
// DELETES it; the person's own subject owns the document and the
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
// # A write answers its whole Result
//
// Every write returns the whole [statelog.Result], never a bare position: a
// position cannot tell `applied` or `pending` from `unknown`, and a zero one
// beside a nil error reads as success, which opens a session on a second
// factor whose spend may not have landed.
//
// # Every statement over the directory is planned
//
// Every statement reading `iam_people`, `iam_invites` or `iam_history` is a
// whole literal the plan gate registers and plans
// (`TestTheDirectoryStatementsArePlanned`); a value's statement is picked from
// a table of whole statements ([sightingQuery]), never formed from a column
// name, which the gate could not match.
//
// # A seat is bound by its HANDLE (ADR-0013)
//
// A seat binding — the row's `seat_id`, an invitation's, the seat a removal's
// tombstone records, the successor's stamp on that tombstone, and every
// reading of them ([Reader.SeatHolders], [Reader.SeatClaims], the notify
// registry's standing, the request path's seat table, the dangling-binding
// rule) — names the seat by its handle, which is immutable in the company
// document: a document that changes a handle has removed one seat and created
// another.
//
// # Every person holds a HUMAN seat for as long as they are here (ADR-0032)
//
// A person is created on a human seat ([Writer.Create]) or invited onto one
// ([Writer.Invite], [Writer.Redeem]), and their seat is MOVED, never cleared
// ([Writer.SetIdentity], [ErrSeatRequired]) — freed only by moving them or
// removing them. A person with no seat acted under their bare login, in no
// unit, led by nobody and reached by no contact route, so every rule the org
// chart decides had nothing to say about them. A SERVICE ACCOUNT's seat stays
// optional: it acts as itself and binds one only to act as it. Every binding,
// of either kind, is checked against the running organisation to be a human
// seat ([Writer.humanSeat]); a node running no company has none to offer
// ([ErrNoCompany]). The WRITER's decides are where the rule is enforced and
// never the applier, which writes whatever a record says, because refusing a
// record there stops the log on every data node.
//
// The company's FIRST person has no exemption: the Tier A token's invitation
// is held to its grants ([Writer.MayConfer]) and names a human seat like any
// other, so a company whose chart declares no human seat enrols nobody.
package iamdomain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ObjectKind is what a subject on the iam log addresses. Valid is false for a
// kind this build has never heard of, and the literal is RETAINED: the
// deferral of a newer peer's record is reported to an operator by it.
type ObjectKind string

// The nine kinds, exported because the publisher, the wake feed's filter, the
// applier's dispatch and the domain's Tables declaration all compare against
// them.
const (
	// KindDirectory is the company's whole directory, on ONE subject with
	// no id: every record that sets or frees a login, an address or a
	// seat, and every record that creates or removes a person row (package
	// doc, "ONE SUBJECT FOR THE DIRECTORY").
	//
	// NOT ROOT-SCOPED: a record declares the buckets of the rows it
	// writes, so a node that cannot decode one holds back only the people
	// it is about. That the WHOLE directory a decide read is complete, it
	// asks of the deferral index ([wholeDirectory]).
	KindDirectory ObjectKind = "directory"

	// KindPerson is one person, machine or human, by the uuid7 minted for
	// them, which nothing renames. Everything about them that is not unique
	// arbitrates here, the REVOCATION EPOCH among it; their row is created
	// and removed on the directory, never here.
	KindPerson ObjectKind = "person"

	// KindSession is one signed-in session's whole life, by its LINEAGE:
	// the uuid7 it was opened with, so sign-ins never contend. A re-issue
	// is NOT a record: the idle deadline it moves is in the bearer's signed
	// payload.
	KindSession ObjectKind = "session"

	// KindInvalidation ends EVERY session in the company at once, on ONE
	// subject with no id.
	//
	// NOT CALLED A GENERATION, though it moves the bearer's generation and
	// writes `iam_session_generation`: [KindGeneration] means the LOG's
	// reanchor in every domain, and one word for two closed sets invites a
	// change to reach for the wrong one.
	//
	// ONE OBJECT so two invalidations contend: two that did not would write
	// the same new value, leaving every cookie minted between them valid.
	// It INSTALLS A GATE ([OpInvalidate]), hence the root scope: a node that
	// could not decode it would keep honouring every ended bearer.
	KindInvalidation ObjectKind = "invalidation"

	// KindSweep is the retention sweep for ONE bucket, by bucket number.
	//
	// A RECORD, not a local delete: the rows are identity-claimed, and a
	// node sweeping on its own clock would hold different bytes from its
	// peers. The record names a POSITION RANGE the publisher resolved once.
	// PER BUCKET because a sweep is bounded by the applier's row budget: 64
	// records spread the deletions over 64 transactions instead of holding
	// this store's only writer for one large one.
	KindSweep ObjectKind = "sweep"

	// KindEviction is a node's removal from THIS log, or its readmission,
	// one subject per node, the gate every state log installs. It states a
	// POSITION, so every data node reaches the same verdict on every record
	// with no clock and no coordination read.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's own record, create-only at an
	// expectation of zero on a fresh stream, by the generation number. It
	// is THE LOG'S generation, never a session's ([KindInvalidation]).
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. Like [KindTrimmed] it writes no row of this
	// domain's, so its table declaration is the EMPTY set, stated.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record removing the arbitration anchors
	// below what the log no longer holds ([statelog.TrimmedKind],
	// ADR-0080), one subject per floor. The framework removes them in the
	// applying transaction; this domain writes nothing for it, no row and
	// no trail, and has the case that says so.
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are the nine, and THE ORDER IS LOAD-BEARING: [statelogtest]
// certifies only the FIRST THREE a domain declares, in order. They are a real
// sequence (a person enrolled, their content changed, a sign-in); any other
// order certifies a record whose apply has nothing to change.
var ObjectKinds = []ObjectKind{
	KindDirectory, KindPerson, KindSession,
	KindInvalidation, KindSweep, KindEviction, KindGeneration, KindBarrier,
	KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation. Not the barrier: on its one domain-wide subject an expectation
// would serialise every linearizable read and write an anchor per read. Not a
// trimmed record: identical ones are each meant to land, and an anchor of its
// own is the one row it exists not to leave.
func (k ObjectKind) Arbitrated() bool {
	return k != KindBarrier && k != KindTrimmed
}

// Identified reports whether this kind's subject carries an id. The directory,
// the invalidation and the barrier are singletons, each for the reason at its
// constant.
func (k ObjectKind) Identified() bool {
	switch k {
	case KindDirectory, KindInvalidation, KindBarrier:
		return false
	}
	return true
}

// GatedByRemoval reports whether a record on this kind can be ABOUT a person,
// and so whether a removal's tombstone may drop it ([markedPerson]): a person's
// subject names them, and a directory or session record's payload does. On
// the kind, the applier and the publisher's reader ask one rule instead of
// decoding each body.
func (k ObjectKind) GatedByRemoval() bool {
	switch k {
	case KindPerson, KindDirectory, KindSession:
		return true
	}
	return false
}

// RootScoped reports whether a record on this kind may state the whole estate
// as its scope. The rule is tight: a deferred root-scoped record blocks every
// read in the domain, freezing every login and revocation in the company
// during a rolling upgrade.
//
// An EVICTION and an INVALIDATION install a GATE, so an unreadable version
// stops the applier instead of being deferred; an invalidation is about
// everybody, so no bucket fits it. A GENERATION pays the cost correctly: a node
// that cannot read a reanchor cannot certify any read over it. A BARRIER's and
// a TRIMMED record's scope is [statelog.FrameworkScope], which intersects
// nothing, and every build reads a trimmed record ([statelog.TrimmedVersion]).
func (k ObjectKind) RootScoped() bool {
	switch k {
	case KindInvalidation, KindEviction, KindGeneration, KindBarrier, KindTrimmed:
		return true
	}
	return false
}

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// PersonSubject names one person by the id nothing renames. Each kind has a
// constructor because several ids are DERIVED (a bucket, a generation), and a
// derivation written twice is a subject two writers disagree about.
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

// SweepSubject names one bucket's retention sweep, ZERO-PADDED so subjects
// sort as the buckets do.
func SweepSubject(b Bucket) Subject {
	return Subject{Kind: KindSweep, ID: b.String()}
}

// EvictionSubject names one node's gate on this log. The id is NOT folded: it
// is what the node published as its writer, and folding it would make the gate
// miss every record the node wrote.
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}

// GenerationSubject names one reanchor by its number, which makes the record
// create-only: a second reanchor to one generation meets a nonzero expectation
// and the broker refuses it.
func GenerationSubject(generation uint32) Subject {
	return Subject{Kind: KindGeneration,
		ID: strconv.FormatUint(uint64(generation), 10)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// TrimmedSubject names the trimmed record whose floor is floor, in the
// framework's ONE spelling ([statelog.TrimmedSubject]), which it parses back;
// the dot inside is no separator here ([Subject.Validate]).
func TrimmedSubject(floor statelog.Position) Subject {
	return Subject{Kind: KindTrimmed, ID: statelog.TrimmedSubject(floor).ID}
}

// String renders the subject's own path, which the framework appends to the
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

// Validate refuses a subject that cannot address an object. An unknown kind
// is NOT refused here: it is refused where a record is written and accepted
// where read, so this build can hold a newer peer's record (the two-pass
// decode).
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
	// Unlike the chart's, the scope separator is not refused: a scope path
	// is built from a bucket number, never a subject id. Nor is a dot: a
	// node id may carry one, and topics.IamLogPath splits only the FIRST.
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
