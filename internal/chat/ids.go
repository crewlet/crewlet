package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE OPERATION IDS, and the two properties every one of them buys: a gesture
// repeated is the SAME gesture rather than a second message, and the state log
// can always tell when it was minted.
//
// # Why every id carries the instant it was minted
//
// The publisher asks, before it decides and again when it resolves an append
// whose answer was lost, whether this node's operation ledger can vouch for
// the operation ([statelog.OpMintedAt]). It cannot for one minted before the
// instant its retention sweep may have deleted rows from, whose row it does
// not hold — a re-run of a turn whose trigger predates the ledger's thirty-day
// sweep — and such a write is answered `unknown` and never published. An id
// that carries no instant at all is read as minted before every loss, so a
// chat whose ids were digests of their inputs, as this domain's once were,
// would answer EVERY post `unknown` from the first sweep that deleted
// anything, on every node, for ever. So every id this package mints is in the
// state log's own grammar: [statelog.NewOpID] for an operation minted now,
// [statelog.DeriveOpID] for one a repetition must reproduce, whose instant is
// when the unit of work behind it BEGAN — never the call's, which a re-run
// moves past the loss it is judged against.
//
// An id a caller hands over WHOLE — an erase's or a prune's, the node gate's —
// is the caller's to mint in that grammar, and the edge it arrives through
// holds it to [statelog.CheckCallerOpID]; the engine's own callers mint
// through [statelog.NewOpID] or step one with [statelog.StepOpID]. This
// package refuses only an empty one, as the wiki's node gate does: a store
// that held a caller-supplied id to a second rule would be a rule two edges
// could disagree with, and the one rule already has a home.
//
// # Why a message's id is derived rather than minted
//
// A post is ADDITIVE. It carries no expectation and arbitrates nothing — see
// [KindMessage] — so there is nothing at the broker that can tell a retry from
// a second remark, and a freshly minted id would make the two
// indistinguishable everywhere else too. What makes a retried post idempotent
// is that it lands under the same OPERATION ID, which the broker's duplicate
// window collapses and the operation ledger collapses after it, and that it
// names the same MESSAGE ID, which the applier's own insert refuses to write
// twice. Two layers, and both of them need a value that survives the retry —
// which is a [statelog.CallKey].
//
// # Why the message id and the operation id are ONE value
//
// On the precedent the wiki's comments set (`pages.Store.commentOpID`): one
// message is one operation, and two identifiers for one thing are two places
// for a retry to disagree with itself. A post whose operation id was stable
// and whose message id was fresh would be collapsed by the ledger where it
// still held the first attempt's row and written as a second message anywhere
// it did not. Derived from one input, the two answers cannot come apart — and
// the id is UNNAMED for that reason, since a reader addresses the message by
// it.
//
// A DIRECT CONVERSATION'S id is [DirectChannelID] and is derived from its
// participants for the same family of reason, one level up — see that
// function. It is not repeated here.

// THE KEY A KEYED GESTURE IS DERIVED FROM is [statelog.CallKey], shared with
// the wiki, and its doc carries the reasons for each field. What is chat's own
// is how it fills two of them.
//
// # Why chat's key names the tool, which the wiki's does not
//
// Because two of this domain's tools make ONE gesture. post_message and
// send_dm both post into a room, so a seat that posted "on it" into a direct
// conversation by its id and later sent "on it" to the same colleague would
// derive one identity for the two, and [statelog.CallKey.Repeat] could not
// tell them apart: it counts calls to ONE tool, so each call is its own tool's
// first. Collapsing them is the unsafe direction — the second is answered as
// the first's retry and dropped with a success — where naming the tool can
// only make a repetition a second message, which is what was asked for. Every
// key a chat tool builds names its tool ([statelog.CallKey.Tool]); the wiki has
// no such pair and leaves it empty.
//
// # And why two tools are sometimes counted as one
//
// Naming the tool keeps two tools' calls APART, which is safe for two tools
// that both add. It is not enough for two tools that UNDO each other, and
// this domain has a pair: join_channel and leave_channel write one membership.
// A count kept per tool cannot see the leave between two joins, so the second
// join would be its tool's first call after nothing different, derive the
// first join's operation, and be answered with the first join's success while
// the seat stayed outside the room. So a turn counts such a pair together
// ([CallFamily]): the leave raises the second join's repeat count, and it is a
// new operation.
//
// # Why the count needs the arguments, in chat's own terms
//
// A seat that left a room by its id, asked that again, joined, left by the
// room's NAME, joined and left by the id once more gave its last leave a count
// of three — the join, the leave by name and the join before it — which is
// exactly what the leave by name had counted: the two leaves by id and the
// first join. Without the digest of the arguments in the identity
// ([statelog.CallKey.Args]) the last leave derived the leave by name's id, was
// answered with that leave's success, and the seat stayed in the room the join
// before it had put it in.

// callOpID is the operation one keyed gesture appends under.
//
// OVER THE KEY, THE VERB AND WHAT THE CALLER ASKED FOR — never over anything
// read from the rows. A retry must derive the same id, and a retry arrives
// AFTER its first attempt may have moved the room: a revision read at call
// time would differ between a request and its own retry and collapse nothing.
// The parts are the gesture's object and its content — the room, the message,
// a digest of the words — so two different gestures under one key are two
// operations.
//
// UNNAMED, like a fresh one ([freshOpID]): for a message the id IS the
// message's, which a reader addresses; for every other gesture the subject and
// the op the ledger reads are on the record.
func (s *Store) callOpID(key statelog.CallKey, verb string, parts ...string) string {
	if key.Empty() {
		return s.newOpID()
	}
	return key.DeriveOpID(operationNamespace, append([]string{verb}, parts...)...)
}

// roomIDFor is a NAMED room's id: derived from the key where there is one, so
// a create the broker collapsed into its first attempt answers with the room
// that attempt made rather than an id nothing holds.
//
// OVER THE NORMALISED NAME, which is the address the create arbitrates on: two
// creates of one address under one key are a repetition, and two different
// addresses are two rooms. Two DIFFERENT keys derive two different ids for one
// address, which is the property a create that lost its race depends on — it
// must not have written a room under the id the winner chose.
func (s *Store) roomIDFor(key statelog.CallKey, name string) string {
	if key.Empty() {
		return s.newID()
	}
	joined := strings.Join(key.Identity(roomIDNamespace, NormalizeName(name)), "\x00")
	return uuid.NewSHA1(roomNamespace, []byte(joined)).String()
}

// freshOpID mints an operation id for a gesture nothing will repeat.
//
// THROUGH [statelog.NewOpID] rather than a UUIDv7 minted here, because the
// instant is what the state log reads to decide whether its ledger can vouch
// for a retry of this operation — and the fallback this had, a v4 when a v7
// could not be minted, is an id carrying no instant at all, which a node whose
// ledger ever lost a row answers `unknown` for ever.
//
// THE WALL CLOCK and not the store's own [Options.Now], which is the clock the
// AUTHORED instants are stamped from and which a test pins: the mint instant is
// compared with the ledger's watermark, which a sweep and a join set off the
// wall.
//
// UNNAMED, because a message's id is its operation's (see the file head) and
// one spelling serves every gesture.
func freshOpID() string { return statelog.NewOpID(time.Now(), "") }

// requireOpID refuses a gesture whose caller handed over no operation id — the
// erase, the prune and the node gate, whose operations are the caller's
// whole rather than derived here. An empty one is refused rather than minted
// in its place: the caller is the only one that could bring a minted id back,
// and a retry under a fresh one would publish the gesture twice.
func requireOpID(opID, gesture string) error {
	if strings.TrimSpace(opID) == "" {
		return invalid("op_id", "the %s names no operation id — it is the "+
			"caller's to mint (statelog.NewOpID) and to send again unchanged "+
			"on a retry, and a retry under a fresh one would write it twice",
			gesture)
	}
	return nil
}

// EraseOpName is the name an erase's operation id carries: the room, and a
// digest of the messages it destroys taken as a SET.
//
// # Why the gesture is in the name
//
// An erase and a prune arbitrate on the ROOM's subject, and the publisher
// collapses a retry onto the record its operation already holds there — which
// is the whole of how an `unknown` is finished. That collapse compares the
// operation and the subject, never what the record says, so an id carried to
// the same room with other messages was answered `applied` from the first
// record while nothing it named was destroyed: a compliance erasure reported
// done that did not happen. The tracker's purge cannot meet this, because its
// subject is the task and the task is the whole gesture; a room is not. So the
// name says which gesture the id is, the route that takes one brought back
// holds it there ([statelog.OpNamed]), and every minter — the route, `crewlet
// chat erase` — mints through this function rather than spelling it.
//
// AS A SET — trimmed and deduplicated as [Store.Erase] reads the list, then
// sorted — so the same messages named in another order are the same erase.
// Sixteen hex digits, [digestOf]'s, tell two sets apart with certainty, which
// is all the name is for: a caller can mint any id it likes, so this is not a
// credential. The longest name it makes keeps the id inside
// [statelog.MaxCallerOpIDBytes].
func EraseOpName(channelID string, messageIDs []string) string {
	ids := cleanHandles(messageIDs)
	slices.Sort(ids)
	return "chat-erase-" + strings.TrimSpace(channelID) + "-" + digestOf(ids...)
}

// PruneOpName is the name a prune's operation id carries: the room and the
// cutoff, for [EraseOpName]'s reason — the same id carried to a later cutoff
// in the same room was answered from the first prune, and nothing after the
// first cutoff went.
//
// THE CUTOFF AS UNIX NANOSECONDS, in UTC: two spellings of one instant — an
// offset, a `Z` — are one prune, and a name with no `.` in it never reads as
// one of the grammar's steps.
func PruneOpName(channelID string, cutoff time.Time) string {
	return "chat-prune-" + strings.TrimSpace(channelID) + "-" +
		strconv.FormatInt(cutoff.UTC().UnixNano(), 10)
}

// freshRoomID mints a NAMED room's id where no key derives one. It is not an
// operation and carries no instant anybody reads, so it is a plain uuid — the
// wiki's choice for a page id.
func freshRoomID() string { return uuid.NewString() }

// contentKey is a digest of what a gesture asked to SAY: its words and the
// links beside them. The mentions and the collective address are derived from
// the words and are not part of it, so a roster that moved between a call and
// its retry cannot make the retry a second message.
func contentKey(body string, links []string) string {
	return digestOf(append([]string{strings.TrimSpace(body)}, links...)...)
}

// valueKey is a digest of a structured value a gesture asked for — a room's
// patch, a membership set — through its JSON encoding, which writes a struct's
// fields in one order and a map's keys sorted.
func valueKey(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		// Every value passed here encodes; formatting is the
		// deterministic fallback rather than a reason to refuse a write.
		raw = []byte(fmt.Sprintf("%#v", value))
	}
	return digestOf(string(raw))
}

// digestOf is sixteen hex digits of a SHA-256 over the parts, NUL-joined so
// ("ab", "c") and ("a", "bc") are two values — enough to tell two different
// requests apart with certainty, which is all an identity part is for.
func digestOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// operationNamespace and roomIDNamespace keep a derived id from colliding with
// one another package derives from the same seed — a turn's tracker and wiki
// writes are derived from the same work key — and roomNamespace is the uuid
// namespace a derived room id lives under. FIXED for the life of the format: a
// room id is durable on every node, and a new namespace would make every
// repeated gesture straddling the change a second record.
const (
	operationNamespace = "crewlet.chat.operation"
	roomIDNamespace    = "crewlet.chat.room"
)

var roomNamespace = uuid.MustParse("f6cc3dc1-2efe-48bf-b10c-ea5942a6fb2e")
