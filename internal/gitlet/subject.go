package gitlet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ObjectKind is what an object on the code log is. Valid is false for a kind
// this build does not know, and the literal is kept either way: a newer peer
// publishes kinds this build lacks, and the deferral it files them under is
// formed from that literal.
type ObjectKind string

// The ten kinds, exported because their readers cannot see each other: the
// publisher builds the subject, the applier's dispatch switches on it, and the
// domain's declaration must classify every one.
const (
	// KindName is a repository NAME's claim, and the subject a create
	// arbitrates on: two reconciles creating "web" must contend, and two
	// fresh repository ids never would. Its id is [NameToken] of the name.
	KindName ObjectKind = "name"

	// KindRepo is one repository, by its id: the subject of every record that
	// moves or governs its refs or pack set. ONE SUBJECT A REPOSITORY, not a
	// ref, because every write updates the repository row too, so a per-ref
	// subject would never narrow contention; two pushes to it meet at the
	// broker, one landing per round. It also carries the key its persons'
	// viewed marks are digested under ([OpViewedKey]).
	KindRepo ObjectKind = "repo"

	// KindEviction is a node's eviction from THIS log, or its readmission:
	// an eviction fences records above a POSITION, and positions on
	// different streams name different number spaces. A gate, by its kind.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero on the generation's own number.
	KindGeneration ObjectKind = "generation"

	// KindCompany is the company's own state on this log; its one object is
	// the ACTIVATION FENCE ([CompanySubject]): the activation code.repos'
	// repositories were last reconciled from, which refuses every older
	// activation's decision about them. A gate by kind ([ObjectKind.InstallsGate]).
	KindCompany ObjectKind = "company"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. It writes no row on any node.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds — [statelog.TrimmedKind], ADR-0080 —
	// on one subject per floor. The framework removes the anchors in the
	// transaction that applies it; this domain writes nothing for it.
	KindTrimmed ObjectKind = statelog.TrimmedKind

	// KindChange is one change (web!42), by its id: the subject of what is said
	// ABOUT it — reviewers, comments and their threads, reviews and the answers
	// to their findings ([ConversationOps]). ITS OWN SUBJECT, NEVER ITS
	// REPOSITORY'S: the lifecycle is on that one, in the committer's queue
	// beside the pushes, and a comment decided there would wait behind a push's
	// batch and contend over rows it never writes. ARBITRATED, published only by the
	// repository's committing data node ([ChangeWriter]), one change's writes at
	// a time. A table is written by ONE subject's records: the repository's keep
	// code_changes, its authors, revisions and body; this one the conversation's.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets a record on a
	// change's subject reads it. It is no gate.
	KindChange ObjectKind = "change"

	// KindChecks is a repository's CHECKS, by its id (checks.<repo>): the
	// subject each check's fate — queued, claimed, ended — is written on
	// (checks.go). ITS OWN SUBJECT, NEVER THE REPOSITORY'S, for [KindChange]'s
	// reason. ADDITIVE: its records carry no expectation and commute, every
	// apply a join ([joinCheck]). Its records alone write code_checks and
	// code_check_logs.
	//
	// BASE FORMAT, as the checks subject is (checks.go). It is no gate.
	KindChecks ObjectKind = "checks"

	// KindPerson is one PERSON'S CODE INBOX, by their human seat (person.<handle>):
	// the subject of the marks over their code notices ([OpMarks], marks.go) and
	// the files of a change they marked viewed ([OpViewed], viewed.go). ITS OWN
	// SUBJECT: a mark spans every change that reached them, and on a change's
	// subject would contend with its conversation over rows it never writes.
	// ARBITRATED, so two screens marking at once both land, each gesture resolved
	// against the record its decide read (internal/inbox's Gesture); published by
	// whichever data node is asked ([Marks]), on THIS log beside the notices, as a
	// position is a coordinate on one stream (internal/inbox's package doc). Its
	// records alone write code_person_marks and code_viewed_files.
	//
	// BASE FORMAT, as [KindChange]; the viewed op is versioned ([ViewedVersion]). No gate.
	KindPerson ObjectKind = "person"
)

// ObjectKinds are the ten, and THE ORDER IS LOAD-BEARING: [statelogtest]
// publishes the FIRST THREE a domain declares, twice each, in order. They are a
// real sequence — a create on a name, which makes the repository; a push on it;
// an eviction — and a push on a repository no create made is malformed under a
// strict replay. A later kind is APPENDED, so the three never move.
var ObjectKinds = []ObjectKind{
	KindName, KindRepo, KindEviction, KindGeneration, KindCompany,
	KindBarrier, KindTrimmed, KindChange, KindChecks, KindPerson,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation. Three do not: the barrier shares one subject across the domain,
// so an expectation would serialise every linearizable read behind every other;
// a trimmed record REMOVES anchors, so an anchor of its own is the row it exists
// not to leave (ADR-0080); and checks are ADDITIVE (checks.go), so an
// expectation would queue every runner's claim and ending for nothing.
func (k ObjectKind) Arbitrated() bool {
	return k != KindBarrier && k != KindTrimmed && k != KindChecks
}

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later: the record installs a rule under which later
// records produce no rows on any node. An eviction drops a node's records; the
// activation fence drops every older activation's decision. A deferred gate
// licenses every record above it — a node that deferred a fence would apply
// settings every peer refused, and differ from them for ever. The purge is the
// third gate, by its OP ([RecordEnvelope.InstallsGate]).
func (k ObjectKind) InstallsGate() bool { return k == KindEviction || k == KindCompany }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// activationID is the company subject's one id: the activation fence.
const activationID = "activation"

// NameToken is the address a repository name is arbitrated on: the first
// sixteen bytes of SHA-256 over the name, in lower-case hex. Arbitration needs
// only equality, and a 32-byte token is a BOUNDED key in the broker's
// per-subject index on every member for the deployment's life, even once a
// grammar admits a name the subject alphabet cannot carry. The payload carries
// the name; the applier recomputes the token and REFUSES a record that
// arbitrated one name and claims another. A name is canonical by its grammar
// ([ValidName]), so nothing is normalised.
func NameToken(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}

// NameSubject is a repository name's claim — what a create arbitrates on.
func NameSubject(name string) Subject { return Subject{Kind: KindName, ID: NameToken(name)} }

// RepoSubject is one repository — what every record about it arbitrates on.
func RepoSubject(id string) Subject { return Subject{Kind: KindRepo, ID: id} }

// ChangeSubject is one change — what every record of its conversation
// arbitrates on.
func ChangeSubject(id string) Subject { return Subject{Kind: KindChange, ID: id} }

// ChecksSubject is one repository's checks — what every record of them is
// published on, by the repository's id.
func ChecksSubject(repoID string) Subject { return Subject{Kind: KindChecks, ID: repoID} }

// PersonSubject is one person's code inbox — what every record of their marks
// arbitrates on, by the human seat they hold.
func PersonSubject(handle string) Subject { return Subject{Kind: KindPerson, ID: handle} }

// EvictionSubject is one node's standing on this log, so an eviction and the
// readmission that inverts it contend with each other and with nothing else.
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}

// GenerationSubject is one generation of the stream, which a reanchor claims
// create-only.
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: strconv.FormatUint(uint64(gen), 10)}
}

// CompanySubject is the activation fence's one subject: every reconcile of
// code.repos publishes its activation here first, and two nodes activating
// the same configuration contend here so exactly one record lands.
func CompanySubject() Subject { return Subject{Kind: KindCompany, ID: activationID} }

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// TrimmedSubject names the trimmed record whose floor is floor, in the
// framework's one spelling of it ([statelog.TrimmedSubject]).
func TrimmedSubject(floor statelog.Position) Subject {
	return Subject{Kind: KindTrimmed, ID: statelog.TrimmedSubject(floor).ID}
}

// String renders the subject's own path — what the framework appends to the
// domain's subject prefix.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string { return topics.CodeLogSubject(string(s.Kind), s.ID) }

// statelog is the subject as the framework names it.
func (s Subject) statelog() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object. An unknown kind
// is NOT refused here: it is refused where a record is WRITTEN and accepted
// where one is READ (the two-pass decode), so this build holds a newer peer's
// record under its own subject without acting on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("gitlet: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("gitlet: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again", s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("gitlet: a %s subject needs an id — only the barrier "+
			"is a kind with exactly one object and no name for it", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("gitlet: subject id %q carries whitespace or a "+
			"wildcard, which the broker would read as a subject pattern", s.ID)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the code log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.CodeLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
