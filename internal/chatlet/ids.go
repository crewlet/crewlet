package chatlet

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE OPERATION IDS: a gesture repeated is the SAME gesture, and the state log
// can always tell when the id was minted. Every id carries its instant, in the
// state log's grammar ([statelog.NewOpID]; keyed gestures derive from
// [Actor.OpKey] and carry the KEY's instant, never the call's): the ledger
// answers `unknown` for an id older than its retention sweep, and an
// instant-less id reads as older than every loss. Caller-handed ids are held
// by [statelog.CheckCallerOpID] at the edge; this package refuses only empty.
//
// A keyed post is idempotent through ONE value naming both the operation and
// the message ([statelog.KeyedObjectOpID]): two ids could disagree on a retry.
// The id hashes what a gesture ASKS ([askOf]), never what it resolved to
// (roster, thread root), so a roster move cannot make a retry a second message.

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

// askOf is what a post or a reply asks: its words for a seat's, the place
// alone for the engine's line ([AuthorSystem]), whose key names the fact it
// states, so a build that words the fact anew retries the same line rather
// than posting a second.
func askOf(actor Actor, replyTo string, in NewMessage) messageAsk {
	ask := messageAsk{ReplyTo: strings.TrimSpace(replyTo)}
	if actor.Kind != AuthorSystem {
		ask.Body, ask.Links = strings.TrimSpace(in.Body), in.Links
	}
	return ask
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
// Minted per call, a keyed retry of a landed create would be answered from the
// ledger with a room id no row would ever have, while the real room went
// unreported. A fresh operation is still a fresh id ([statelog.NewOpID] carries
// random bits), and two DIFFERENT operations derive two ids, so a create that
// lost its race never writes a room under the winner's id.
func roomIDOf(opID string) string {
	return uuid.NewSHA1(roomNamespace, []byte(opID)).String()
}

// roomNamespace scopes [roomIDOf]. FIXED for the life of the format: a room id
// is durable on every node, and a new namespace would give a retry that
// straddles an upgrade a second room.
var roomNamespace = uuid.MustParse("f6cc3dc1-2efe-48bf-b10c-ea5942a6fb2e")

// freshOpID mints an operation id for a gesture nothing will repeat.
//
// THROUGH [statelog.NewOpID], never a bare UUID: the state log reads the
// instant to decide whether its ledger can vouch for a retry, and an id with no
// instant is answered `unknown` for ever once the ledger lost a row.
//
// THE WALL CLOCK, not [Options.Now] (the authored-instant clock, which tests
// pin): the instant is compared with the ledger's watermark, set off the wall.
//
// UNNAMED, because a message's id is its operation's (see the file head).
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
// EXPORTED because the caller's operation must be bound to what it destroys:
// an erase arbitrates on its ROOM's subject and the publisher collapses a retry
// onto the record its operation already holds, so an id reused with other
// messages would be answered `applied` while nothing it named was destroyed. A
// caller derives the operation over the room and this set, so another order or
// a repeat is one erase and another set is another. One spelling, shared by the
// store and every caller, so they cannot disagree on which list is the same.
func EraseSet(messageIDs []string) []string {
	ids := cleanHandles(messageIDs)
	slices.Sort(ids)
	return ids
}
