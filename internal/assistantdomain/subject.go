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

// THE ASSISTANT LOG'S SUBJECTS: the arbitration units, so the choice of
// subject per operation is the concurrency design (package doc, "Subjects").
// One running or parked turn per conversation is decided by the broker on the
// conversation's subject, never by a check two nodes can both pass. A turn's
// and a proposal's id lead with their conversation, before a dot, so a purge
// gate reads the conversation from the subject alone, as [Gates.GatedAt] must
// for another node's record with no body.

// ObjectKind is what an object on the assistant log is. An unknown kind keeps
// its literal: the deferral a newer peer's record is filed under forms a scope
// term out of it.
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
	// below what the log no longer holds ([statelog.TrimmedKind], ADR-0080),
	// on one subject per floor, which the id names. The framework removes the
	// anchors as it applies it; this domain writes nothing for it.
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are the eight, and THE ORDER IS LOAD-BEARING: the conformance
// suite publishes the first three, twice each, in order. They must be a real
// sequence (a conversation's create, then a turn's and a proposal's records),
// since a record about a conversation no create wrote applies as nothing and
// any other order would certify a no-op.
var ObjectKinds = []ObjectKind{
	KindConversation, KindTurn, KindProposal, KindOwner, KindEviction, KindGeneration,
	KindBarrier, KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// Every kind but the two framework records does, so every conversation ever
// written keeps an anchor until a trimmed record removes it. A turn's additive
// records move the anchor its park and close arbitrate against, costing those
// at most one round. The barrier's one domain-wide subject would serialise every
// linearizable read; the trimmed record's identical copies must each land.
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
// its readmission arbitrate against each other and nothing else.
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

// Validate refuses a subject that cannot address an object. An unknown kind
// passes: it is refused only where a record is written ([Subject.writable]),
// the asymmetry the two-pass decode exists for.
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

// writable refuses a subject this build may not write, an unknown kind too.
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
// conversation and the child, at the first dot (a conversation's id has
// none). Ids stay opaque keys here; that each is a uuid is the writer's to
// hold.
func splitChild(id string) (conversation, child string, ok bool) {
	conversation, child, ok = strings.Cut(id, ".")
	return conversation, child, ok && conversation != "" && child != ""
}

// gatedConversation is the conversation whose deletion marker gates a record
// on (kind, id), and false for a subject no conversation's marker covers.
//
// The subject alone decides (a gate may be asked with no body), in one
// function for [Applier.Gated] and [Gates.GatedAt], which must agree.
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
