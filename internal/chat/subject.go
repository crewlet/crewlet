package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/google/uuid"
)

// Chat as a state-log DOMAIN: what a record is, and what the subject is for.
//
// Every change is one RECORD on one ordered stream, published to the subject
// of the object it changes. For channel state the subject is the ARBITRATION
// UNIT — two writers renaming one room contend at the broker and exactly one
// wins — and for a message it is a ROUTING AND ORDERING unit that arbitrates
// nothing at all, because posts commute. The package doc is why; this file is
// the grammar.
//
// What the record must state either way is the SCOPE — every object its apply
// touches — because that is what a node which cannot decode it files the
// deferral under.

// ObjectKind is what an object on the chat log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of — and the literal is RETAINED either way, for the reason
// [github.com/crewlet/crewlet/internal/tracker.ObjectKind] gives: a newer peer
// publishes a kind this build does not know, and the deferral this build files
// it under forms a scope term out of that literal.
type ObjectKind string

// The six kinds.
//
// EXPORTED AND ENUMERATED because four readers that cannot see each other all
// compare against them: the publisher builds the subject, the wake feed's
// filter includes or excludes the kind, the applier's dispatch switches on it,
// and the domain's own Tables declaration must classify every one. A kind
// added to three of the four is a record that publishes, is delivered, wakes
// nobody and writes nothing.
const (
	// KindChannelName is the company's hold on one normalised channel
	// name, and the subject a NAMED channel's create arbitrates on.
	//
	// THE NAME IS THE ADDRESS, so making a room is a create-only append at
	// an expectation of zero on the name itself: two people creating
	// `#launch` contend at the broker and exactly one wins, where two
	// nodes publishing on their own new rooms' ids would not contend at
	// all and would both succeed.
	//
	// A DIRECT channel never comes here — it has no name to claim, and its
	// id is derived from its participants instead. See [Kind.Direct] and
	// [DirectChannelID].
	KindChannelName ObjectKind = "channelname"

	// KindChannel is one room's own state: its topic, its purpose, its
	// membership, its archive, its retention and the erases and prunes
	// that remove what was said in it.
	//
	// ITS ID IS THE CHANNEL ID, which is what makes a deferred membership
	// edit block every write in that room — the containment the scope
	// alphabet is for.
	KindChannel ObjectKind = "channel"

	// KindMessage is everything anybody says, and the overwhelming
	// majority of records: a post, an edit, a deletion and a reaction are
	// all writes under this kind.
	//
	// ITS ID IS THE CHANNEL'S, not the message's, and that is the whole
	// hot-path design. One subject per room gives the log a total order
	// per room — which is what the applier mints the per-channel sequence
	// from — while the broker's per-subject index stays bounded by the
	// number of ROOMS rather than by the number of things anybody ever
	// said. A subject per message would be an index entry per utterance,
	// held for the stream's whole retention window, for an arbitration
	// nothing ever asks for.
	//
	// THE ONE ADDITIVE KIND, on the precedent of
	// [github.com/crewlet/crewlet/internal/tracker.KindTurn]: it carries
	// no expectation, decides against nothing, and its apply is guarded by
	// its own id insert affecting a row.
	KindMessage ObjectKind = "message"

	// KindEviction is a node's eviction from THIS log, or its readmission.
	//
	// Its own record here rather than another domain's, because an
	// eviction fences records above a POSITION and positions on different
	// streams name different number spaces. The kind that installs a gate,
	// which is why its version is pinned for ever.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero: two operators deriving the same number race there and
	// exactly one wins.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain.
	//
	// The only kind that writes no row on any node, which is why its table
	// declaration is the EMPTY set stated explicitly rather than left out:
	// a kind that writes nothing must not be able to slip through the
	// completeness walk by writing nothing.
	KindBarrier ObjectKind = "barrier"
)

// ObjectKinds are the six, and THE ORDER IS LOAD-BEARING.
//
// [github.com/crewlet/crewlet/internal/statelog/statelogtest] publishes the
// FIRST THREE a domain declares, twice each, in order — so the declaration
// decides what the framework's own suite certifies. These three are a real
// sequence rather than three unrelated records: a create on a name whose
// payload names the room, a patch on that room, and then a message posted into
// it. A message in a room no create wrote is a malformed record under a strict
// replay, so putting the message before the name would certify a failure.
var ObjectKinds = []ObjectKind{
	KindChannelName, KindChannel, KindMessage, KindEviction, KindGeneration,
	KindBarrier,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and therefore whether an anchor row is written for it.
//
// TWO OF SIX, AND IT IS A CLOSED SET. A name claim and a room's own state are
// the only objects two writers ever decide against each other over.
//
// EVERYTHING ELSE PAYS NOTHING FOR ARBITRATION IT DOES NOT USE. A message is
// additive — the records commute, so an expectation there would serialise the
// single hottest subject in the company behind itself and turn a lost race
// into a message somebody typed and lost. A barrier shares one subject across
// the whole domain, so an anchor there would put an upsert per linearizable
// read on one hot row inside the transaction holding this store's only writer.
// An eviction and a generation are fleet machinery with a handful of records
// each: their expectation comes from the broker's own last sequence, at the
// cost of one round trip on a write nobody takes twice a day.
//
// A CLOSED SET RATHER THAN A NEGATIVE ONE, so a kind added later arbitrates
// nothing until somebody says it should — the failure of a missing anchor is
// an extra round trip, and the failure of an unintended one is a row on a hot
// subject inside every apply transaction.
func (k ObjectKind) Arbitrated() bool {
	switch k {
	case KindChannelName, KindChannel:
		return true
	}
	return false
}

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later.
//
// ONE, AND IT IS A CLOSED SET. A deferred gate does not postpone one record's
// effect on one node: it silently licenses every record above it, with no
// inverse that repairs it.
//
// It is answered from the KIND ALONE because it must be answerable by a node
// that cannot decode the payload. The DESTRUCTIVE OPS need the same treatment
// and do not have a kind of their own — see [RecordEnvelope.InstallsGate],
// which is the reader that asks both questions.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Routable reports an object kind whose records can wake somebody.
//
// TWO, AND IT IS A CLOSED SET. A message is the obvious one. A channel is
// there because two of its ops are about PEOPLE rather than about a room's
// settings: being added to a private room is how somebody learns it exists,
// and an erase is an operator removing what a person wrote, which that room
// is told about rather than left to notice.
//
// THE OTHER FOUR ARE MACHINERY. A name claim has no audience — the room it
// claims for does, and that is the channel record written in the same
// transaction. An eviction, a generation and a barrier are the fleet talking
// to itself, and a wake per barrier would page the company once per
// linearizable read.
//
// A CLOSED SET RATHER THAN A NEGATIVE ONE, so a kind added later is silently
// unroutable rather than silently routed: the failure of a missing wake is one
// person not hearing something, and the failure of an unintended one is every
// seat in the company woken by a bookkeeping append.
func (k ObjectKind) Routable() bool {
	switch k {
	case KindMessage, KindChannel:
		return true
	}
	return false
}

// RecordsHistory reports a kind whose apply writes a chat history row.
//
// THREE, AND IT IS A CLOSED SET. A history row's kind is what every feed
// filter and every audit window selects on, and here that kind is the
// (subject kind, op) PAIR rather than a vocabulary of its own — which is the
// whole reason this domain has no separate change-kind enum. The op alone
// would not do it: `create` names both a room and the address it was claimed
// under, and a filter that could not tell them apart would report a company's
// rooms twice.
//
// A NAME CLAIM IS IN THE SET ALTHOUGH NOBODY IS WOKEN FOR IT, and that is the
// distinction this set exists to make against [ObjectKind.Routable]: a room's
// address being taken is a durable fact about the company, and the history is
// a complete account of what HAPPENED rather than an account of what was
// ANNOUNCED. An eviction, a generation and a barrier are the fleet's own
// bookkeeping, with no entry in anybody's account of what happened.
func (k ObjectKind) RecordsHistory() bool {
	switch k {
	case KindChannelName, KindChannel, KindMessage:
		return true
	}
	return false
}

// Subject is the object a record is published on.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// The subject constructors, ONE PER KIND rather than a Subject{Kind, ID}
// literal at every call site.
//
// Two of the ids are not the caller's own value — a name claim's is a DIGEST
// and a message's is the CHANNEL's id, not the message's — and an id composed
// or substituted at each call site is a subject two writers disagree about.
// Building every one here is what makes that impossible rather than unlikely.

// ChannelNameSubject names the company's claim on one channel name, which is
// what a NAMED channel's create arbitrates on.
//
// THE NAME IS NORMALISED HERE, once, so a caller cannot arbitrate on the
// author's own capitalisation: `#Launch` and `#launch` are one address, and
// two subjects would make them two rooms.
func ChannelNameSubject(name string) Subject {
	return Subject{Kind: KindChannelName, ID: ChannelToken(name)}
}

// ChannelSubject names one room's own state by its id, which is what every
// change to a room that is not a message contends on: a topic, a membership
// set, an archive, an erase, a prune. It is also what a DIRECT channel's
// create arbitrates on, create-only at an expectation of zero.
func ChannelSubject(channelID string) Subject {
	return Subject{Kind: KindChannel, ID: channelID}
}

// MessageSubject names the message stream of ONE ROOM — its id is the
// CHANNEL's, never the message's.
//
// That is the hot path's whole shape in one function: every post, edit,
// deletion and reaction in a room lands on one subject, which gives the log a
// total order per room for the applier to mint a per-channel sequence from,
// and keeps the broker's per-subject index bounded by the number of rooms.
// See [KindMessage], and see the package doc for why the record's SCOPE is
// that same channel.
func MessageSubject(channelID string) Subject {
	return Subject{Kind: KindMessage, ID: channelID}
}

// EvictionSubject names one node's standing in this log, so an eviction and
// the readmission that inverts it arbitrate against each other and against
// nothing else — a fleet shedding two nodes at once writes two independent
// subjects rather than serialising on one.
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}

// GenerationSubject names one generation of the stream, which a reanchor
// claims create-only: the generation number IS the arbitration unit, so two
// nodes reacting to the same recreated stream cannot both install it.
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: fmt.Sprintf("%d", gen)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// ChannelToken is the address a channel name is arbitrated on.
//
// # Why the name is a TOKEN rather than the name
//
// A subject is a broker path: it may not contain a space, a dot, a `*` or a
// `>`. A channel name is checked against [ValidName] and so contains none of
// them today — and that is exactly why the token is here rather than the name.
// Putting prose in a subject makes the grammar depend on a validation rule
// somewhere else staying as strict as it is now: the day a name is allowed to
// carry a dot, every subject already on the log means something different.
//
// So the address is arbitrated on a fixed-width digest of the normalised name,
// and three things make that the better trade — the same three
// [github.com/crewlet/crewlet/internal/pages.TitleToken] states:
//
//   - ARBITRATION NEEDS ONLY EQUALITY. Two writers claiming one address must
//     land on one subject, and nothing here ever needs to read the name back
//     out of the subject.
//   - THE SUBJECT IS A COST. It is a key in the broker's per-subject index on
//     every member for the life of the deployment, and a 32-byte token is
//     fixed where a name is not.
//   - NOTHING IS LOST. The record's own payload carries the displayed name,
//     the applier writes it, and this function recomputes the token from it —
//     so a writer that arbitrated on one address while claiming another is
//     REFUSED rather than applied.
//
// The first sixteen bytes of SHA-256 over the NORMALISED name, in lower-case
// hex. Truncated because 128 bits is far past what the collision matters: a
// company at [MaxChannels] sits around 1.5e-33, and a collision's whole
// consequence is that two names contend at the broker and one create retries
// — not a wrong row and not a lost write.
func ChannelToken(name string) string {
	sum := sha256.Sum256([]byte(NormalizeName(name)))
	return hex.EncodeToString(sum[:16])
}

// directChannelNamespace is a fixed UUIDv4, chosen once and FROZEN.
//
// Changing it re-derives every direct conversation's id and orphans every row
// written under the old one — the messages, the membership, the read state,
// the thread rows. The conversation does not break; it simply becomes a second
// empty room beside a full one nobody can reach. Treat as load-bearing, on
// exactly the terms
// [github.com/crewlet/crewlet/internal/org.DeriveAgentID]'s namespace is.
var directChannelNamespace = uuid.MustParse("7b1f4a2c-8d63-4e57-9c0a-2f5b6d8e3a41")

// DirectChannelID is the deterministic id of the conversation between these
// participants.
//
// # Why it is derived and not minted
//
// A named room is created ONCE, by somebody who typed a name, and two people
// typing it contend on that name. A direct conversation has nobody who creates
// it: it comes into existence because somebody said something, and the other
// side may say something at the same moment on another node. Minting an id
// there gives two rooms with the same two people in them, each holding half
// the conversation — and no arbitration can fix it afterwards, because neither
// writer was deciding against anything the other wrote.
//
// So the id IS the participant set: a UUIDv5 over the normalised, sorted,
// deduplicated handles joined with NUL. Every node computes the same id for
// the same people with no database and no lookup, the create is a create-only
// append on that id, and the loser of the race is told the room already exists
// — which is the right answer, because it does.
//
// THE JOIN IS NUL rather than a comma or a space, because a handle may not
// contain one: a separator a value can carry makes `[ab, c]` and `[a, bc]` the
// same input, which is an id collision between two different conversations.
// SORTED because the two sides pass their participants in whatever order they
// hold them, and DEDUPLICATED because a caller that lists itself twice must
// not derive a third room.
//
// IT ANSWERS [uuid.Nil] FOR AN EMPTY SET rather than the digest of nothing,
// for the reason [github.com/crewlet/crewlet/internal/org.DeriveAgentID]
// refuses empty inputs: an unnameable conversation must not silently acquire
// an id that every other unnameable conversation would also derive.
// [ChannelCreate.Validate] refuses the nil id, so the zero value is a refusal
// rather than a room. The participant COUNT is bounded there too, against
// [MaxDMParticipants]: a cap belongs where a write can be refused naming the
// field, not in a derivation that has no way to say no.
func DirectChannelID(handles []string) uuid.UUID {
	normalised := make([]string, 0, len(handles))
	for _, h := range handles {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			normalised = append(normalised, h)
		}
	}
	slices.Sort(normalised)
	normalised = slices.Compact(normalised)
	if len(normalised) == 0 {
		return uuid.Nil
	}
	return uuid.NewSHA1(directChannelNamespace, []byte(strings.Join(normalised, "\x00")))
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
	return topics.ChatLogSubject(string(s.Kind), s.ID)
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN and accepted where one is READ, which is the asymmetry the
// whole two-pass decode exists for: this build must be able to hold a newer
// peer's record under its own subject without being able to act on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("chat: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("chat: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again",
			s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("chat: a %s subject needs an id — only the barrier "+
			"is a kind with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, ". \t\n*>") {
		return fmt.Errorf("chat: subject id %q carries a separator, whitespace "+
			"or a wildcard — the broker would read it as a subject pattern, and "+
			"a dot would make the id's first segment look like the kind. Every "+
			"id this domain mints is a uuid, a node id, a number or a hex "+
			"token, and none of them carries one", s.ID)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the chat log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.ChatLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
