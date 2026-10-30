package chatlet

import (
	"fmt"
	"slices"
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
// state log's own grammar: [statelog.NewOpID] for an operation minted now, and
// one derived from the caller's own key ([Actor.OpKey]) for one a repetition
// must reproduce — which carries the KEY's instant, the instant the unit of
// work behind it began, never the call's, which a re-run moves past the loss
// it is judged against.
//
// An id a caller hands over WHOLE — an erase's or a prune's, the node gate's —
// is the caller's to mint in that grammar, and the edge it arrives through
// holds it to [statelog.CheckCallerOpID]; the engine's own callers mint
// through [statelog.NewOpID] or step one with [statelog.StepOpID]. This
// package refuses only an empty one, as the wiki's node gate does: a store
// that held a caller-supplied id to a second rule would be a rule two edges
// could disagree with, and the one rule already has a home.
//
// # Why a keyed gesture's id is derived rather than minted
//
// A post is SEQUENCED. It carries no expectation and arbitrates nothing — see
// [KindMessage] — so there is nothing at the broker that can tell a retry from
// a second remark, and a freshly minted id would make the two
// indistinguishable everywhere else too. What makes a retried post idempotent
// is that it lands under the same OPERATION ID, which the broker's duplicate
// window collapses and the operation ledger collapses after it, and that it
// names the same MESSAGE ID, which the applier's own insert refuses to write
// twice. Two layers, and both of them need a value that survives the retry —
// which is the caller's key, carried on the [Actor] exactly as the wiki's
// writes take theirs (`doclet.Actor.OpKey`), and derived from by the one rule
// every keyed domain shares ([statelog.KeyedOpID]): the key, the verb, the
// object the gesture is about and a digest of what it ASKS.
//
// WHAT IT ASKS, NEVER WHAT IT RESOLVED TO. The mentions a post names and its
// collective address are derived from its words against the live roster, and
// a roster that moved between a call and its retry must not make the retry a
// second message; a thread root is resolved inside the decide from the message
// answered, and a retry could find it moved. So each gesture's content below
// is the request's own input, the shape the wiki's comment binds
// (`pages.commentAsk`).
//
// # Why the message id and the operation id are ONE value
//
// On the precedent the wiki's comments set: one message is one operation, and
// two identifiers for one thing are two places for a retry to disagree with
// itself. A post whose operation id was stable and whose message id was fresh
// would be collapsed by the ledger where it still held the first attempt's row
// and written as a second message anywhere it did not. Derived from one input,
// the two answers cannot come apart — and the id is UNNAMED for that reason
// ([statelog.KeyedObjectOpID]), since a reader addresses the message by it.
//
// A DIRECT CONVERSATION'S id is [DirectChannelID] and is derived from its
// participants for the same family of reason, one level up — see that
// function. It is not repeated here. A NAMED room's id is its create's
// operation's ([roomIDOf]), so a create the ledger or the broker answered as
// an earlier copy names the room that copy made.

// operation is the id one named gesture appends under: derived from the
// actor's [Actor.OpKey] when there is one, by [statelog.KeyedOpID] — which
// states why the verb, the object, what the gesture asks and the key's own
// instant are each in it — and freshly minted when there is not.
//
// THE VERBS ARE HYPHENATED WORDS, never dotted: the verb is the id's name, and
// in the grammar a dot begins a gesture's STEP ([statelog.StepOpID]).
func (s *Store) operation(actor Actor, verb, object string, content any) (string, error) {
	key := strings.TrimSpace(actor.OpKey)
	if key == "" {
		return s.newOpID(), nil
	}
	id, err := statelog.KeyedOpID(key, verb, object, content)
	if err != nil {
		return "", fmt.Errorf("chatlet: %w", err)
	}
	return id, nil
}

// messageOperation is a message's id, which is its operation's: unnamed
// ([statelog.KeyedObjectOpID]), because a reader addresses the message by it.
func (s *Store) messageOperation(actor Actor, verb, channelID string, ask messageAsk) (
	string, error) {

	key := strings.TrimSpace(actor.OpKey)
	if key == "" {
		return s.newOpID(), nil
	}
	id, err := statelog.KeyedObjectOpID(key, verb, channelID, ask)
	if err != nil {
		return "", fmt.Errorf("chatlet: %w", err)
	}
	return id, nil
}

// The gestures' own content, each what its caller ASKED for — see the head of
// this file. Field names are part of the digest, so they are FIXED for the
// life of the format: a renamed one would make every retry that straddles the
// change a second record.
type (
	// messageAsk is what a post or a reply asks to say, in the room the
	// operation's object names: the message it answers as the caller named
	// it, the words and the links beside them.
	messageAsk struct {
		ReplyTo string   `json:"reply_to,omitempty"`
		Body    string   `json:"body"`
		Links   []string `json:"links,omitempty"`
	}

	// createAsk is what a room's create asks for under its name — the kind
	// as the caller stated it, unresolved, so a retry after the company's
	// default visibility moved is still the same create.
	createAsk struct {
		Kind        Kind      `json:"kind,omitempty"`
		Topic       string    `json:"topic,omitempty"`
		Purpose     string    `json:"purpose,omitempty"`
		Unit        string    `json:"unit,omitempty"`
		Members     []Member  `json:"members,omitempty"`
		ActivatedAt time.Time `json:"activated_at,omitzero"`
	}

	// editAsk is what an edit asks of a message: the new words, and the
	// links — nil when the edit keeps the message's own, which is a
	// different call from one that names its links, an empty list included.
	editAsk struct {
		Channel string    `json:"channel"`
		Body    string    `json:"body"`
		Links   *[]string `json:"links"`
	}

	// reactAsk is one reaction's toggle on a message.
	reactAsk struct {
		Channel string `json:"channel"`
		Emoji   string `json:"emoji"`
	}

	// retentionAsk is the room's retention override a gesture states, nil
	// for the company's default.
	retentionAsk struct {
		Days *int `json:"days"`
	}
)

// roomIDOf is the id a named room's create gives it: a FUNCTION OF THE
// OPERATION, so the retry an `unknown` asks for names the room its first
// attempt created — the wiki's `pageIDOf`, for the same reason.
//
// Minted per call instead, a keyed retry of a create that had landed was
// answered from the ledger with a room id this call had just made up — one no
// row, no listing and no later read would ever find — while the room the
// operation did create went unreported. A fresh operation is a fresh id either
// way, since [statelog.NewOpID] carries random bits, so nothing changes for a
// caller that brings no key. Two DIFFERENT operations derive two ids for one
// address, which is the property a create that lost its race depends on — it
// must not have written a room under the id the winner chose.
func roomIDOf(opID string) string {
	return uuid.NewSHA1(roomNamespace, []byte(opID)).String()
}

// roomNamespace scopes [roomIDOf]. FIXED for the life of the format: a room id
// is durable on every node, and a new namespace would give a retry that
// straddles an upgrade a second room.
var roomNamespace = uuid.MustParse("f6cc3dc1-2efe-48bf-b10c-ea5942a6fb2e")

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

// EraseSet is the messages an erase names, AS A SET: trimmed, deduplicated
// and sorted — the list [Store.Erase] reads and the record carries.
//
// EXPORTED because the operation is the caller's ([Store.Erase]) and has to be
// bound to what it destroys: an erase arbitrates on its ROOM's subject, and the
// publisher collapses a retry onto the record its operation already holds
// there whatever that record says — so an id carried to the same room with
// other messages would be answered `applied` from the first record while
// nothing it named was destroyed. A caller derives the operation over the room
// and this set, so the same messages named in another order, or with one
// repeated, are one erase, and another set is another. One spelling here, read
// by the store and by every caller that binds an erase, so the two cannot come
// to disagree about which list is the same.
func EraseSet(messageIDs []string) []string {
	ids := cleanHandles(messageIDs)
	slices.Sort(ids)
	return ids
}
