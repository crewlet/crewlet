package chat

import (
	"encoding/json"
	"fmt"
	"time"
)

// THE DOCUMENTS: the blob a channel's row and a message's row each carry, and
// what it is for beside the columns that already hold most of it.
//
// # Why a row carries a document at all
//
// THE COLUMNS ARE WHAT A QUERY DRIVES ON; THE DOCUMENT IS WHAT THE OBJECT IS.
// Three things fall out of that split, and each one is a failure if the blob
// is missing:
//
//   - A NEWER BUILD'S FIELDS SURVIVE A ROLLING UPGRADE. The document is the
//     object's full encoded state INCLUDING what this build has never heard
//     of ([Channel.Extra], [Message.Extra]), so a node that applies a record
//     from a newer peer, and every node that reads the row afterwards, hands
//     the field back rather than silently dropping it. A row made only of
//     columns is a row whose width is this build's opinion.
//   - A FIELD NOTHING FILTERS ON GETS NO COLUMN. Whether a message addressed
//     the whole room, and who edited or removed one, are read when a message
//     is rendered and by no predicate anywhere — and a column per such field
//     is an index-less column on the largest table in the engine.
//   - ONE READ ANSWERS "WHAT IS THIS". A reader that had to assemble a room
//     or a message from its own columns would be a second definition of the
//     object, in SQL, that nothing holds against this one.
//
// # What the documents deliberately do NOT carry
//
// THE VALUES THE APPLIER MINTS FROM LOG ORDER: a message's `channel_seq`, a
// room's `message_seq`, and the composed `version` on every row. They are the
// applier's arithmetic rather than anything a record authored, the transcript
// is ordered on the column, and a copy in the blob would be a second answer
// that every edit would have to carry forward by hand. The room's counter is
// the sharper case: it moves on EVERY message, so a copy in the document would
// put a blob rewrite on the hot path the column exists to keep off it.
//
// A ROOM'S MEMBERSHIP, which is `chat_members` and not a field of the room. It
// is this domain's largest collection ([MaxMembers]), it carries per-member
// columns of its own — a role, whether the reconcile or a person added it,
// the follow-all flag — and it has its own record ([MemberSet]). Duplicated in
// the blob it would make every topic change rewrite a thousand handles, and it
// would be a second answer to who may read a private room. A replay rebuilds
// the table from the records, which is the only thing that has to be true.

// DocumentVersion is the shape version every document and every typed payload
// here carries.
//
// ONE CONSTANT FOR BOTH, because a payload IS the post-state a document is
// written from: [ChannelCreate] and [Channel] describe one room, and a build
// that could read one shape and write the other would be two versions of one
// object.
//
// Unknown FIELDS round-trip; an unknown VERSION is refused rather than
// downgraded. A channel is read, modified and written back by whichever node
// the request landed on, so an older build that rewrote a newer build's
// document would strip whatever the new shape added.
const DocumentVersion = 1

// Channel is one room's own state, as the blob on its row.
//
// THE ROOM'S SETTINGS AND ITS PROVENANCE, and nothing about what was said in
// it. Who changed a setting is `chat_history`'s — the complete account of what
// happened in a room — where this is the account of what the room IS.
type Channel struct {
	V int `json:"v"`

	ID string `json:"id"`

	// Kind decides how the room is addressed, who may read it and whether
	// a create arbitrated on a name or on a derived id. It is also what
	// the `private` column is computed from at apply, which is why there
	// is no `Private` field here: a second answer to who may read a room
	// is the one disagreement this document may not contain.
	Kind Kind `json:"kind"`

	// Name is the address, already normalised — [ValidName] admits no
	// capitalisation to preserve. Empty for a direct conversation, which
	// has no address at all.
	Name string `json:"name,omitempty"`

	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`

	// Unit is the unit whose room this is, empty for every other kind. It
	// is IDENTITY — what the lead fallback resolves through — and never a
	// read scope and never a credential.
	Unit string `json:"unit,omitempty"`

	// RetentionDays is this room's override of the company default. A
	// POINTER because [RetentionForever] is zero and is a real setting:
	// absent means "take the company's", and a present 0 means "keep this
	// room for ever". A plain int collapses the two into the reading that
	// silently deletes a year of somebody's decisions.
	RetentionDays *int `json:"retention_days,omitempty"`

	// CreatedAt is the BROKER's own instant for the create, not the
	// writer's clock — which is what makes one node's copy of this row
	// byte-identical to another's.
	CreatedAt     time.Time  `json:"created_at"`
	CreatedBy     string     `json:"created_by,omitempty"`
	CreatedByKind AuthorKind `json:"created_by_kind,omitempty"`

	// ArchivedAt closes the room to new messages while keeping every word
	// of it readable. A POINTER rather than a zero time, because "not
	// archived" is an absence and the zero instant is a real point on the
	// same scale as every other timestamp here.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Message is one message's full state, as the blob on its row.
type Message struct {
	V int `json:"v"`

	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`

	// ThreadRoot is the message that started the thread, empty on a room
	// post. It is the ROOT rather than the parent: a thread here is one
	// level deep by construction, which is what makes "this thread" a
	// range scan rather than a recursive walk.
	ThreadRoot string `json:"thread_root,omitempty"`

	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`

	Body string `json:"body,omitempty"`

	// Links are what the message points at, at most [MaxLinks]. There are
	// no attachments — see the package doc — so this is the whole of what
	// a message carries besides its own prose.
	Links []string `json:"links,omitempty"`

	// Mentions are the RESOLVED handles the body named, IN THE ORDER IT
	// named them. Also exploded into `chat_mentions`, which is a person's
	// @-mention feed and the index that feed ranges over — a SET, keyed
	// on (message, handle), which cannot hold an order and does not need
	// one. Rendering the message needs the order, so it is here.
	Mentions []string `json:"mentions,omitempty"`

	// Collective says the message addressed the whole room (`@channel`).
	// THE FIELD WITH NO COLUMN, and the plainest case for this document
	// existing: nothing filters on it, it is read whenever the message is
	// rendered, and a routing snapshot is not durable state.
	Collective bool `json:"collective,omitempty"`

	// CreatedAt is the BROKER's own instant. A message's displayed time
	// is the broker's for the reason the ordering is: two people posting
	// a second apart on nodes whose clocks disagree by a minute would
	// otherwise render differently on every node.
	CreatedAt time.Time `json:"created_at"`

	EditedAt     *time.Time `json:"edited_at,omitempty"`
	EditedBy     string     `json:"edited_by,omitempty"`
	EditedByKind AuthorKind `json:"edited_by_kind,omitempty"`

	// DeletedAt marks the TOMBSTONE: the row survives with its body
	// blanked, so a thread hung off this message still has its root.
	//
	// THE DELETER IS ON THE ROW rather than only in the history, which is
	// the one place this document deliberately duplicates an activity
	// entry: who removed somebody's words is the whole of what a tombstone
	// records ([MessageDelete]), and the history is swept on a horizon
	// while the tombstone is kept for as long as the thread is.
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
	DeletedBy     string     `json:"deleted_by,omitempty"`
	DeletedByKind AuthorKind `json:"deleted_by_kind,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ErrUnknownVersion reports a document a newer build wrote.
type ErrUnknownVersion struct {
	Got  int
	Want int
}

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("chat: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// ---- encoding ---------------------------------------------------------- //
//
// THROUGH THE PACKAGE'S ONE ENCODER, in record.go. A merge of its own here
// would be a second answer to whether a carried field loses to a known one,
// and the two would disagree the first time one of them changed.

// The known field names per document. Explicit rather than reflective, for the
// reason the record's own set is: a name missing here is decoded into the
// struct AND carried as unknown, so the next encode writes the stale carried
// copy back over what the caller set.
var (
	channelFields = fieldSet(Channel{}, "name", "topic", "purpose", "unit",
		"retention_days", "created_by", "created_by_kind", "archived_at")
	messageFields = fieldSet(Message{}, "thread_root", "body", "links",
		"mentions", "collective", "imported", "edited_at", "edited_by",
		"edited_by_kind", "deleted_at", "deleted_by", "deleted_by_kind")
)

// EncodeChannel renders a room.
func EncodeChannel(c Channel) ([]byte, error) { return encode(c, c.Extra) }

// DecodeChannel reads a room.
func DecodeChannel(data []byte) (Channel, error) {
	var c Channel
	extra, err := decodeInto(data, &c, channelFields)
	if err != nil {
		return Channel{}, fmt.Errorf("chat: decode channel: %w", err)
	}
	if err := checkDocumentVersion(c.V); err != nil {
		return Channel{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeMessage renders a message.
func EncodeMessage(m Message) ([]byte, error) { return encode(m, m.Extra) }

// DecodeMessage reads a message.
func DecodeMessage(data []byte) (Message, error) {
	var m Message
	extra, err := decodeInto(data, &m, messageFields)
	if err != nil {
		return Message{}, fmt.Errorf("chat: decode message: %w", err)
	}
	if err := checkDocumentVersion(m.V); err != nil {
		return Message{}, err
	}
	m.Extra = extra
	return m, nil
}

// checkDocumentVersion refuses a document a newer build wrote.
//
// NAMED FOR THE DOCUMENT, because this package has two versions that are not
// the same number and never move together: [RecordVersion] is the shape of
// what is published on the log, and [DocumentVersion] is the shape of what is
// stored in a row. A bare `checkVersion` here would be an invitation to hold
// one against the other.
func checkDocumentVersion(got int) error {
	if got > DocumentVersion {
		return ErrUnknownVersion{Got: got, Want: DocumentVersion}
	}
	return nil
}
