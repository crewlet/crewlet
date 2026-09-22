package chat

import (
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// THE DERIVED IDS, and the one property every one of them buys: a gesture
// repeated is the SAME gesture rather than a second message.
//
// # Why a message's id is derived rather than minted
//
// A post is ADDITIVE. It carries no expectation and arbitrates nothing — see
// [KindMessage] — so there is nothing at the broker that can tell a retry from
// a second remark, and a freshly minted uuid would make the two
// indistinguishable everywhere else too. What makes a retried post idempotent
// is that it lands under the same OPERATION ID, which the broker's duplicate
// window collapses and the operation ledger collapses after it, and that it
// names the same MESSAGE ID, which the applier's own insert refuses to write
// twice. Two layers, and both of them need a value that survives the retry.
//
// # Why the message id and the operation id are ONE value
//
// On the precedent the wiki's comments set (`pages.Store.commentOpID`): one
// message is one operation, and two identifiers for one thing are two places
// for a retry to disagree with itself. A post whose op id was stable and whose
// message id was fresh would be collapsed by the ledger on one node and
// written as a second message by any node that had already swept the ledger
// row — [ChatOpsRetention] is seven days, and a scheduled seat can hold an
// operation id across a long weekend. Derived from the same inputs, the two
// answers cannot come apart.
//
// # Why there are two rules rather than one
//
// Because the two callers hold different evidence of "the same gesture".
//
// A PERSON has an idempotency key and nothing else: the surface that took the
// keystroke mints one per submission, and the retry that matters is the same
// submission arriving twice. So a person's message is (channel, author,
// operation id).
//
// A SEAT has a TURN, which is the unit the engine re-runs — and a turn
// legitimately posts more than once. Deriving from the turn alone would make
// the second remark of a turn overwrite the first, everywhere, silently. So a
// seat's message is (turn, channel, ORDINAL), and the ordinal is the caller's
// to supply because only the caller knows which of its own calls this is. It
// is not derived from the body: a seat that says "on it" twice in one turn
// said it twice.
//
// A DIRECT CONVERSATION'S id is [DirectChannelID] and is derived from its
// participants for the same family of reason, one level up — see that
// function. It is not repeated here.

// messageNamespace scopes every derived message id. A fixed UUIDv4, chosen
// once and FROZEN.
//
// Changing it re-derives every message id this company has ever written, so a
// retry of anything in flight lands as a second message and every id anybody
// recorded elsewhere — a link, a wake, an erase gesture's list — names a
// message nothing can find. Treat as load-bearing, exactly as
// [directChannelNamespace] is.
var messageNamespace = uuid.MustParse("3f8c9d21-5b47-4e0a-9d6c-1a2b3c4d5e6f")

// The derivation tags, which are the FIRST segment of every name below.
//
// ONE NAMESPACE AND A TAG rather than a namespace each, because the tag is
// what makes the separation a property of the VALUE instead of a property of
// two constants somebody has to keep distinct. Without it, a person's
// (channel, author, operation id) and a seat's (turn, channel, ordinal) are
// two three-segment names under one namespace, and nothing but their content
// stops one from deriving the other's id. It is the same reason the run-token
// key derivation carries a domain: what stops one endpoint's token validating
// at the other is a segment, not a convention.
const (
	idFromPerson = "p"
	idFromTurn   = "s"
	idFromImport = "i"
)

// PersonMessageID is the id of a message a person submits, and the operation
// id the record carries.
//
// (channel, author, operation id). THE OPERATION ID IS REQUIRED and is the
// caller's own idempotency key — refused empty naming the field, because
// without one there is nothing that makes a resubmitted message the same
// message, and the failure is silent: the person sees their remark twice.
func PersonMessageID(channelID, author, operationID string) (uuid.UUID, error) {
	channelID = strings.TrimSpace(channelID)
	author = strings.TrimSpace(author)
	operationID = strings.TrimSpace(operationID)
	switch {
	case channelID == "":
		return uuid.Nil, invalid("channel_id", "a message names no room, and "+
			"the room is half of what makes its id this message rather than "+
			"the same words said somewhere else")
	case author == "":
		return uuid.Nil, invalid("author", "a message names no author")
	case operationID == "":
		return uuid.Nil, invalid("operation_id", "a person's message needs an "+
			"idempotency key — it is what makes a resubmission the same "+
			"message rather than a second one, and a post arbitrates nothing "+
			"at the broker, so there is nothing else that could tell them apart")
	}
	return derivedID(idFromPerson, channelID, author, operationID), nil
}

// TurnMessageID is the id of a message a seat posts during a turn, and the
// operation id the record carries.
//
// (turn, channel, ordinal). THE ORDINAL IS REQUIRED and is the caller's,
// because a turn may legitimately post twice — a seat that answers a question
// and then reports what it did said two things — and an id derived from the
// turn alone would make the second remark overwrite the first on every node.
// It is a plain call counter within the turn: the first posting call of a turn
// is 0, the next is 1, and a re-run turn that makes the same calls in the same
// order derives the same ids and writes each message once.
func TurnMessageID(turnID, channelID string, ordinal int) (uuid.UUID, error) {
	turnID = strings.TrimSpace(turnID)
	channelID = strings.TrimSpace(channelID)
	switch {
	case turnID == "":
		return uuid.Nil, invalid("turn_id", "a seat's message names no turn, "+
			"which is the unit a re-run repeats — use the person's rule "+
			"instead, which takes an idempotency key")
	case channelID == "":
		return uuid.Nil, invalid("channel_id", "a message names no room")
	case ordinal < 0:
		return uuid.Nil, invalid("ordinal", "a turn's posting call is numbered "+
			"%d — the ordinal is a call counter within the turn, starting at 0, "+
			"and a negative one is a caller that has no counter at all", ordinal)
	}
	return derivedID(idFromTurn, turnID, channelID, strconv.Itoa(ordinal)), nil
}

// derivedID is the one derivation, so no caller can join its segments
// differently from any other.
//
// THE JOIN IS NUL, for [DirectChannelID]'s reason: a separator a value can
// carry makes two different names the same input, which here is two different
// messages with one id. A handle, a channel id, a turn id and an operation id
// carry no NUL, and nothing in this package mints one that could.
func derivedID(segments ...string) uuid.UUID {
	return uuid.NewSHA1(messageNamespace, []byte(strings.Join(segments, "\x00")))
}

// newOperationID mints an operation id for a gesture that has no derivation of
// its own — a room's create, a membership change, an edit, a reaction.
//
// UUIDv7, so the ledger's rows and an operator reading the log sort by when
// the operation was minted rather than by a random digest, falling back to v4
// rather than failing a write: an id that does not sort is worse than an id
// nobody can read, and no id at all is a write with no third answer.
//
// A GESTURE WITH NO NATURAL KEY GETS A FRESH ONE, deliberately. Two identical
// reactions a second apart are two gestures, and the second one's record is
// what carries the removal a toggle needs; collapsing them on their content
// would make a reaction that was added, removed and added again end up
// wherever the ledger's sweep left it.
func newOperationID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
