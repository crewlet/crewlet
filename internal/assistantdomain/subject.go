package assistantdomain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ASSISTANT LOG'S SUBJECTS: what a record arbitrates over.
//
// Every change is one RECORD published to the subject of the object it
// contends for. The subject is the ARBITRATION UNIT, so the choice of subject
// per operation is the concurrency design:
//
//   - A conversation's own subject orders its create, its title, its purge
//     and the opening of each turn, so two nodes opening a turn in one
//     conversation at once contend there and exactly one wins: one running
//     or parked turn per conversation is decided by the broker, not by a
//     check two nodes can both pass.
//   - A turn's subject orders its park, resume and close (one runner at a
//     time), and carries its messages, steps and cards as additive records.
//   - A proposal's subject orders its decision, so two approvals on two nodes
//     arbitrate to one.
//   - An owner's subject carries the one record that erases everything they
//     hold.
//
// A TURN'S AND A PROPOSAL'S ID LEAD WITH THEIR CONVERSATION, before a dot, so
// the purge gate reads the conversation from the subject alone: the
// publisher's side of the gate is asked about another node's record with no
// body at all ([Gates.GatedAt]).

// ObjectKind is what an object on the assistant log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of, and the literal is RETAINED either way: a newer peer publishes a
// kind this build does not know, and the deferral this build files it under
// forms a scope term out of that literal.
type ObjectKind string

const (
	// KindConversation is one conversation: its create, its title, its
	// purge, its compliance reads and the opening of each of its turns.
	KindConversation ObjectKind = "conversation"

	// KindTurn is one turn of a conversation, its id the conversation's, a
	// dot and the turn's.
	KindTurn ObjectKind = "turn"

	// KindProposal is one proposal a turn made, its id the conversation's,
	// a dot and the proposal's.
	KindProposal ObjectKind = "proposal"

	// KindOwner is one person, by their identity-estate id: the subject of
	// their erasure.
	KindOwner ObjectKind = "owner"

	// KindEviction is a node's eviction from THIS log, or its readmission.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. It writes no row on any node.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds ([statelog.TrimmedKind], ADR-0073),
	// on one subject per floor, which the subject's id names. The framework
	// removes the anchors in the transaction that applies it; this domain
	// writes nothing for it, and has the case that says so.
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are the eight, and THE ORDER IS LOAD-BEARING.
//
// The framework's conformance suite publishes the FIRST THREE a domain
// declares, twice each, in order, so the declaration decides what the suite
// certifies. These three are a real sequence: a conversation's create, a
// message recorded on a turn of it, and a proposal made in it. A turn's or a
// proposal's record about a conversation no create wrote applies as nothing,
// so any other order would certify a no-op.
var ObjectKinds = []ObjectKind{
	KindConversation, KindTurn, KindProposal, KindOwner, KindEviction, KindGeneration,
	KindBarrier, KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// EVERY KIND BUT THE TWO FRAMEWORK RECORDS DOES. The additive records on a turn
// (a message, a step, a card) carry no expectation of their own, and they move
// the anchor the turn's park and close are arbitrated against, which costs
// those at most one round; it is also why every conversation ever written
// keeps an anchor until a trimmed record removes it. The barrier shares one
// subject across the whole domain, so an expectation there would serialise
// every linearizable read behind every other; the trimmed record REMOVES
// anchors, and its identical copies are each meant to land, so an expectation
// would refuse every one after the first.
func (k ObjectKind) Arbitrated() bool {
	return k.Valid() && k != KindBarrier && k != KindTrimmed
}

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later. A conversation's purge and an owner's
// erasure are this log's other gates, by their OPS ([RecordEnvelope.InstallsGate]).
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// ConversationSubject names one conversation.
func ConversationSubject(conversation uuid.UUID) Subject {
	return Subject{Kind: KindConversation, ID: conversation.String()}
}

// TurnSubject names one turn of one conversation.
func TurnSubject(conversation, turn uuid.UUID) Subject {
	return Subject{Kind: KindTurn, ID: conversation.String() + "." + turn.String()}
}

// ProposalSubject names one proposal made in one conversation.
func ProposalSubject(conversation, proposal uuid.UUID) Subject {
	return Subject{Kind: KindProposal, ID: conversation.String() + "." + proposal.String()}
}

// OwnerSubject names one person's hold on everything they own here.
func OwnerSubject(owner uuid.UUID) Subject {
	return Subject{Kind: KindOwner, ID: owner.String()}
}

// EvictionSubject names one node's standing in this log, so an eviction and
// the readmission that inverts it arbitrate against each other and nothing
// else.
func EvictionSubject(nodeID string) Subject { return Subject{Kind: KindEviction, ID: nodeID} }

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// TrimmedSubject names the trimmed record whose floor is floor, in the
// framework's ONE spelling of it ([statelog.TrimmedSubject]): the framework
// reads the floor back from the id.
func TrimmedSubject(floor statelog.Position) Subject {
	return Subject{Kind: KindTrimmed, ID: statelog.TrimmedSubject(floor).ID}
}

// GenerationSubject names one generation of the stream, which a reanchor
// claims create-only.
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: strconv.FormatUint(uint64(gen), 10)}
}

// String renders the subject's own path: what the framework appends to the
// domain's subject prefix, and what a log line names.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string { return topics.AssistantLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN ([Subject.writable]) and accepted where one is READ, which
// is the asymmetry the two-pass decode exists for.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("assistantdomain: a subject with no kind addresses the " +
			"log's own prefix, which no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("assistantdomain: subject kind %q carries a separator "+
			"or a wildcard, so the kind and the id could not be told apart", s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("assistantdomain: a %s subject needs an id: only the "+
			"barrier has exactly one object on this log", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\r\n*>/") {
		return fmt.Errorf("assistantdomain: subject id %q carries whitespace, a "+
			"wildcard or a slash, which the broker or the scope alphabet would "+
			"read as something else", s.ID)
	}
	return nil
}

// writable refuses a subject this build may not WRITE: an unknown kind among
// them.
func (s Subject) writable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("assistantdomain: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the assistant log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.AssistantLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}

// splitChild takes a turn's or a proposal's subject id apart into the
// conversation and the child, at the first dot: a conversation's id carries
// none. The applier and the gates read ids as the opaque keys the rows hold;
// that each is a uuid is the writer's to hold.
func splitChild(id string) (conversation, child string, ok bool) {
	conversation, child, ok = strings.Cut(id, ".")
	return conversation, child, ok && conversation != "" && child != ""
}

// gatedConversation is the conversation whose deletion marker gates a record
// on (kind, id), and false for a subject no conversation's marker covers.
//
// THE SUBJECT ALONE DECIDES, for the reason [Gates.GatedAt] gives. ONE
// FUNCTION, asked by [Applier.Gated] and [Gates.GatedAt] alike, because the
// framework holds the two to one answer.
func gatedConversation(kind, id string) (string, bool) {
	switch ObjectKind(kind) {
	case KindConversation:
		return id, id != ""
	case KindTurn, KindProposal:
		conversation, _, ok := splitChild(id)
		return conversation, ok
	}
	return "", false
}
