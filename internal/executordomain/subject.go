package executordomain

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

// THE EXECUTOR LOG'S SUBJECTS (package doc, "Subjects, scopes and the five
// records"): a CONVERSATION's subject orders every record about it, so the rules
// over a whole conversation are decided from one snapshot and ordered by the
// broker; a SEAT's carries the one record that removes all the seat holds here
// once the company removed the seat.
//
// A conversation's id is the seat's handle, a dot and [ConversationID] of its
// key. The key, a surface's opaque identity for a thread, issue or channel, may
// hold what a subject or scope path may not (a dot, a slash, a space), so only
// the row keeps it. A seat's handle holds no dot ([iam.ValidSeatHandle]), so
// the id's first dot ends the seat.

// ObjectKind is what an object on the executor log is. Valid is false for a
// kind this build has never heard of, but the literal is RETAINED: a newer
// peer's kind is deferred under a scope term formed from it.
type ObjectKind string

const (
	// KindConversation is one conversation of one seat: every record about
	// a turn of it.
	KindConversation ObjectKind = "conversation"

	// KindSeat is one seat: the subject of its removal.
	KindSeat ObjectKind = "seat"

	// KindEviction is a node's eviction from THIS log, or its readmission.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. It writes no row on any node.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds ([statelog.TrimmedKind], ADR-0080), on one
	// subject per floor, which the id names. The framework removes the anchors as
	// it applies it; this domain's case for it writes nothing.
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are the six, and THE ORDER IS LOAD-BEARING: the framework's
// conformance suite publishes a domain's FIRST THREE, twice each, in order, so
// they are a real sequence: a turn's capture (writes a row), its seat's removal
// (deletes it, tombstones the live turn) and a node's eviction.
var ObjectKinds = []ObjectKind{
	KindConversation, KindSeat, KindEviction, KindGeneration, KindBarrier,
	KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// Every kind but the two framework records does: each record decides from what
// its conversation or seat held, so two writers deciding from one state contend
// and one re-decides (which is why every conversation keeps an anchor until a
// trimmed record removes it). On the barrier's one domain-wide subject an
// expectation would serialise every linearizable read behind every other; the
// trimmed record's identical copies are each meant to land, so one would refuse
// all but the first.
func (k ObjectKind) Arbitrated() bool {
	return k.Valid() && k != KindBarrier && k != KindTrimmed
}

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later: the eviction alone (see [Domain.InstallsGate]).
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// ConversationID is the digest a conversation's subject and scope name it by:
// the first 128 bits of SHA-256 over its key, or over the turn's own id for a
// turn with no conversation, in lowercase hex.
//
// A DIGEST, so a key holding what a subject or a path may not is one token; 128
// BITS, so no fleet sees two of one seat's conversations collide; and a TAG BYTE
// separates the two inputs, so no key spells a turn's own digest. A collision
// would only arbitrate two together and share a scope: every row keeps its
// whole key, and every rule over a conversation reads the key.
func ConversationID(conversationKey, turn string) string {
	h := sha256.New()
	if conversationKey == "" {
		_, _ = h.Write([]byte{'t', 0})
		_, _ = h.Write([]byte(turn))
	} else {
		_, _ = h.Write([]byte{'c', 0})
		_, _ = h.Write([]byte(conversationKey))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// ConversationSubject names the conversation a turn of handle belongs to: its
// key's, or the turn's own for a turn with no conversation.
func ConversationSubject(handle, conversationKey, turn string) Subject {
	return Subject{Kind: KindConversation, ID: handle + "." + ConversationID(conversationKey, turn)}
}

// SeatSubject names one seat's hold on everything it has here.
func SeatSubject(handle string) Subject { return Subject{Kind: KindSeat, ID: handle} }

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
func (s Subject) Wire() string { return topics.ExecutorLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object.
//
// A kind this build does not know is not refused here: it is refused where a
// record is WRITTEN ([Subject.writable]) and accepted where one is READ, the
// asymmetry the two-pass decode exists for.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("executordomain: a subject with no kind addresses the " +
			"log's own prefix, which no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("executordomain: subject kind %q carries a separator "+
			"or a wildcard, so the kind and the id could not be told apart", s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("executordomain: a %s subject needs an id: only the "+
			"barrier has exactly one object on this log", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\r\n*>/") {
		return fmt.Errorf("executordomain: subject id %q carries whitespace, a "+
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
		return fmt.Errorf("executordomain: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the executor log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.ExecutorLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}

// splitConversation takes a conversation subject's id apart into the seat's
// handle and the conversation's digest, at the first dot.
func splitConversation(id string) (handle, conversation string, ok bool) {
	handle, conversation, ok = strings.Cut(id, ".")
	return handle, conversation, ok && handle != "" && conversation != ""
}
