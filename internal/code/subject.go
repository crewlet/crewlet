package code

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

// ObjectKind is what an object on the code log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of — and the literal is RETAINED either way, for the reason the
// tracker's kind gives: a newer peer publishes a kind this build does not know,
// and the deferral this build files it under is formed from that literal.
type ObjectKind string

// The seven kinds.
//
// EXPORTED AND ENUMERATED because the readers that compare against them cannot
// see each other: the publisher builds the subject, the applier's dispatch
// switches on it, and the domain's own declaration must classify every one.
const (
	// KindName is a repository NAME's claim, and the subject a create
	// arbitrates on: two reconciles creating "web" must contend, and two
	// fresh repository ids never would. Its id is [NameToken] of the name.
	KindName ObjectKind = "name"

	// KindRepo is one repository, by its id — the subject every record that
	// moves its refs or its pack set, or governs them, arbitrates on. ONE
	// SUBJECT A REPOSITORY rather than one a ref, because every write on a
	// repository updates the repository row too, so a per-ref subject would
	// never narrow a write's contention; and two pushes to one repository
	// meet at the broker, where exactly one record lands per round.
	KindRepo ObjectKind = "repo"

	// KindEviction is a node's eviction from THIS log, or its readmission:
	// an eviction fences records above a POSITION, and positions on
	// different streams name different number spaces. A gate, by its kind.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero on the generation's own number.
	KindGeneration ObjectKind = "generation"

	// KindCompany is the company's own state on this log, and its one
	// object is the ACTIVATION FENCE ([CompanySubject]): the configuration
	// activation the repositories code.repos declares were last reconciled
	// from, which refuses every older activation's decision about them. A
	// gate, by its kind — see [ObjectKind.InstallsGate].
	KindCompany ObjectKind = "company"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. It writes no row on any node.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds — [statelog.TrimmedKind], ADR-0046 —
	// on one subject per floor. The framework removes the anchors in the
	// transaction that applies it; this domain writes nothing for it.
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are the seven, and THE ORDER IS LOAD-BEARING.
//
// [statelogtest] publishes the FIRST THREE a domain declares, twice each, in
// order — so the declaration decides what the framework's own suite certifies.
// These three are a real sequence: a create on a name, whose payload makes the
// repository; a push on that repository; and an eviction. A push on a
// repository no create made is a malformed record under a strict replay, so
// the repository before its name would certify a failure.
var ObjectKinds = []ObjectKind{
	KindName, KindRepo, KindEviction, KindGeneration, KindCompany,
	KindBarrier, KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// FIVE OF SEVEN DO. The barrier shares one subject across the whole domain, so
// an expectation there would serialise every linearizable read behind every
// other one; and a trimmed record REMOVES anchors, so an anchor of its own
// would be the one row it exists not to leave behind (ADR-0046).
func (k ObjectKind) Arbitrated() bool {
	return k != KindBarrier && k != KindTrimmed
}

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later, because the record installs a rule under
// which later records produce no rows on any node.
//
// TWO KINDS ARE GATES BY KIND. An eviction drops a node's records; the
// company's activation fence drops every older activation's decision about a
// repository. A deferred gate does not postpone one record's effect on one
// node: it silently licenses every record above it — a node that deferred a
// fence would apply an older activation's settings every peer refused, and
// its copy would differ from theirs for ever. The purge is the third gate, by
// its OP on an ordinary repository ([RecordEnvelope.InstallsGate]).
func (k ObjectKind) InstallsGate() bool { return k == KindEviction || k == KindCompany }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// activationID is the company subject's one id: the activation fence.
const activationID = "activation"

// NameToken is the address a repository name is arbitrated on: the first
// sixteen bytes of SHA-256 over the name, in lower-case hex.
//
// # Why a token rather than the name
//
// ARBITRATION NEEDS ONLY EQUALITY — two writers claiming one name must land on
// one subject, and nothing reads the name back out of a subject. A 32-byte
// token is a BOUNDED key in the broker's per-subject index on every member for
// the life of the deployment, and it stays one when a later grammar admits a
// name the broker's subject alphabet cannot carry. Nothing is lost: the
// record's payload carries the name, and the applier recomputes the token from
// it and REFUSES a record that arbitrated one name and claims another. A name
// is canonical by its grammar ([ValidName]), so there is nothing to normalise.
func NameToken(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}

// NameSubject is a repository name's claim — what a create arbitrates on.
func NameSubject(name string) Subject { return Subject{Kind: KindName, ID: NameToken(name)} }

// RepoSubject is one repository — what every record about it arbitrates on.
func RepoSubject(id string) Subject { return Subject{Kind: KindRepo, ID: id} }

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

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN and accepted where one is READ — the asymmetry the
// two-pass decode exists for: this build must hold a newer peer's record under
// its own subject without being able to act on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("code: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("code: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again", s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("code: a %s subject needs an id — only the barrier "+
			"is a kind with exactly one object and no name for it", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("code: subject id %q carries whitespace or a "+
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
