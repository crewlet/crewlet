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

// THE EXECUTOR LOG'S SUBJECTS: what a record arbitrates over.
//
//   - A CONVERSATION's subject orders every record about it: each turn's
//     captures, its end and its retention delete. The rules over a whole
//     conversation (its head, its absorption, its work-key dedupe) are then
//     decided from one snapshot of it and ordered by the broker, whichever node
//     decides them.
//   - A SEAT's subject carries the one record that removes everything the seat
//     holds here, after the company removed the seat.
//
// THE CONVERSATION'S ID IS THE SEAT'S HANDLE, A DOT AND A DIGEST of the
// conversation's key ([ConversationID]). A key is a surface's own identity for
// a thread, an issue or a channel, opaque here and free to hold anything a
// subject or a scope path may not (a dot, a slash, a space), so it never
// appears in either; the row keeps it whole. A turn with no conversation is a
// conversation of its own, digested from its turn id, so its records are
// ordered alone. A seat's handle holds no dot ([iam.ValidSeatHandle]), so the
// first dot of the id is always where the seat ends.

// ObjectKind is what an object on the executor log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of, and the literal is RETAINED either way: a newer peer publishes a
// kind this build does not know, and the deferral this build files it under
// forms a scope term out of that literal.
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
	// for the whole domain. The only kind that writes no row on any node.
	KindBarrier ObjectKind = "barrier"
)

// ObjectKinds are the five, and THE ORDER IS LOAD-BEARING.
//
// The framework's conformance suite publishes the FIRST THREE a domain
// declares, twice each, in order, so the declaration decides what the suite
// certifies. These three are a real sequence: a turn's capture (which writes a
// row), its seat's removal (which deletes it and tombstones the live turn) and
// a node's eviction.
var ObjectKinds = []ObjectKind{
	KindConversation, KindSeat, KindEviction, KindGeneration, KindBarrier,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// EVERY KIND BUT THE BARRIER DOES: every record here decides from what the
// conversation or the seat held when it was decided, so two writers deciding
// from one state contend and one re-decides. The barrier shares one subject
// across the whole domain, so an expectation there would serialise every
// linearizable read behind every other.
func (k ObjectKind) Arbitrated() bool { return k.Valid() && k != KindBarrier }

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
// A DIGEST, so a key holding what a subject or a path may not is still one
// token; 128 BITS, so two conversations of one seat share one by chance with a
// probability no fleet reaches; and the two inputs are DOMAIN-SEPARATED by a
// tag byte, so no key spells the digest of a turn's own conversation. Two that
// did collide would only arbitrate together and share a scope: every row keeps
// its whole key, and every rule over a conversation reads the key.
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
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN ([Subject.writable]) and accepted where one is READ, which
// is the asymmetry the two-pass decode exists for.
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
