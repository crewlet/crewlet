package chatlet

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// THE DOCUMENTS: the blob a channel's row and a message's row each carry. THE
// COLUMNS ARE WHAT A QUERY DRIVES ON; THE DOCUMENT IS WHAT THE OBJECT IS. It
// holds the full encoded state INCLUDING fields this build has never heard of
// ([Channel.Extra], [Message.Extra]), so a rolling upgrade hands a newer peer's
// field back instead of dropping it. A field nothing filters on gets no
// column; it gains one the day a predicate reads it.
//
// NOT IN IT: the values the applier mints from log order (`channel_seq`,
// `message_seq`, `version`: a copy would be a second answer every edit must
// carry, and the room counter moves on every message), and membership
// (`chat_members`, [MemberSet], [MaxMembers]: a blob copy would rewrite a
// thousand handles per topic change and answer who may read a room twice).

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
// it: the account of what the room IS. What happened in it is its transcript,
// and the two gestures that destroy part of that leave their account in the
// audit record of whoever made them, not in the room.
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

	Topic   string `json:"topic,omitempty" untrusted:"fence"`
	Purpose string `json:"purpose,omitempty" untrusted:"fence"`

	// Unit is the unit whose room this is, empty for every other kind. It
	// is IDENTITY (what the room's answerer resolves through) and never a
	// read scope and never a credential. It is the org chart's, like the
	// room's membership: recorded at the create and moved only by the
	// chart's own write, to the unit that declares the room now
	// ([MemberSet.Unit]).
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

	// ChartEpoch is the configuration activation a UNIT room's membership
	// was last written from ([configplane.ActivationStamp]), and zero for
	// every other kind, whose membership is somebody's decision rather than
	// the chart's. It is what [Store.SetUnitMembers] compares a write
	// against, so a node that applies an OLDER activation — booting on a
	// revision the fleet has since replaced — cannot walk the room back to
	// its own chart.
	ChartEpoch int64 `json:"chart_epoch,omitempty"`

	// AutoJoin puts every seat that joins the company from now on in this
	// room ([Store.AutoJoin]), with a [EventJoined] line authored by the
	// engine. An administrator's to set ([ChannelPatch.AutoJoin]), and never
	// on a unit's room or a direct conversation: the first's membership is
	// the org chart's, and the second's is its identity.
	//
	// WHO WAS ALREADY OFFERED THE ROOM is not here. It is `chat_autojoin`,
	// a row per seat, because it grows with the company and a room's
	// document is read on every rail.
	AutoJoin bool `json:"auto_join,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// called is how a sentence names the room to whoever reads it, a model in a
// tool's answer above all: `#launch` where it has a name, which is how anybody
// refers to it, and what it is where it has none.
//
// NEVER ITS ID. A named room's id was minted when it was created, and a direct
// conversation's is derived from its participants; nobody chose either, and a
// refusal that named a room by one left the reader to work out which room it
// meant ([roomLabel] says the same of a notification's subject line).
func (c Channel) called() string {
	switch {
	case c.Name != "":
		return "#" + c.Name
	case c.Kind.Direct():
		return "this conversation"
	}
	return "this room"
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

	Body string `json:"body,omitempty" untrusted:"fence"`

	// Links are what the message points at, at most [MaxLinks]. There are
	// no attachments — see the package doc — so this is the whole of what
	// a message carries besides its own prose.
	Links []string `json:"links,omitempty" untrusted:"name"`

	// Mentions are the RESOLVED handles the body named, IN THE ORDER IT
	// named them. Also exploded into `chat_mentions`, which is a person's
	// @-mention feed and the index that feed ranges over — a SET, keyed
	// on (message, handle), which cannot hold an order and does not need
	// one. Rendering the message needs the order, so it is here.
	Mentions []string `json:"mentions,omitempty"`

	// Collective says the message addressed the whole room (`@channel`).
	// Also the row's `collective` column, which the rail's mention count
	// filters on: a message that named the whole room is one that named
	// its reader, and counting it from the document would decode a room's
	// newest hundred messages on every poll.
	Collective bool `json:"collective,omitempty"`

	// Asks is an agent author's statement that the message asks something
	// of the seats it reaches ([Routing.Asks]), kept so an edit routes the
	// way the post did.
	Asks bool `json:"asks,omitempty"`

	// Reach is who a PERSON'S post reached and why: the seats its record
	// routed it to ([Notify.Recipients]), in the order the routing reached
	// them, written by the applier from that record. Nil on everything
	// else, an agent's post and a room event included, so a reader can tell
	// a post that reached nobody (a Reach with no recipients) from one
	// nobody recorded the reach of.
	//
	// ON THE DOCUMENT because it is a fact about the post, read with it,
	// pruned and erased with it. Whether each seat has ANSWERED changes
	// with later messages, so that is `chat_receipts` ([MessageView.Answered]).
	Reach *Reach `json:"reach,omitempty"`

	// WorkingFor is whom an AGENT'S post was written for (ADR-0109): the
	// carrier its record rode ([MutationRecord.WorkingFor]), written by the
	// applier ([workingForOf]), nil on every other message (no carrier, a
	// record below [WorkingForVersion], a person's message, a room event).
	// An edit keeps it, as it keeps [Message.Reach].
	//
	// ON THE DOCUMENT FROM THE CARRIER'S FIRST BUILD ON: the applier writes a
	// message's document once, so a field a later build began to fill would
	// differ between nodes that applied the post before and after it. Only the
	// turn-start read takes it ([Reader.TurnThread]). NEVER HANDED TO A READER:
	// every other read and write answer strips it ([Message.forReaders]).
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`

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

	// Event marks a ROOM EVENT rather than words somebody wrote: the room's
	// own change, written by the applier from the record that made it
	// ([RoomEvent]). Nil on every message somebody said.
	Event *RoomEvent `json:"event,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Reach is who a person's post reached.
type Reach struct {
	// Recipients are the seats the post woke, each under the strongest
	// reason it reached them for, in routing order: at most
	// [MaxRecipients]. Empty is a post that reached no agent.
	Recipients []Reached `json:"recipients,omitempty"`

	// Truncated says a routing cap left seats out ([Notify.WakesTruncated]).
	Truncated bool `json:"truncated,omitempty"`
}

// Reached is one seat a post reached, and why.
//
// NOT [Recipient], whose tags are the record's compact wire form: this is
// what a reader of the message renders, so it spells its fields out. The two
// hold the same fields, so a record's recipient converts to one directly, and
// a field added to either stops that conversion compiling until the other
// follows.
type Reached struct {
	Handle string `json:"handle"`
	Reason Reason `json:"reason"`
}

// reachOf is the reach a post's record carries, for the applier to write onto
// the message: the routed set of a PERSON'S post, or nil.
//
// NIL WHERE NOTHING WAS ROUTED FOR A PERSON: an agent's post, which a receipt
// is not shown on, and a record with no routing snapshot (a system line).
func reachOf(author AuthorKind, n *Notify) *Reach {
	if author != AuthorHuman || n == nil {
		return nil
	}
	out := &Reach{Truncated: n.WakesTruncated}
	for _, r := range n.Recipients {
		out.Recipients = append(out.Recipients, Reached(r))
	}
	return out
}

// workingForOf is whom a post was written for as its message keeps it: the
// carrier of a record at [WorkingForVersion] or above, as a copy of its own,
// and nil for every other, so every node writes the same document.
func workingForOf(record MutationRecord) *types.WorkingFor {
	if record.V < WorkingForVersion {
		return nil
	}
	return record.WorkingFor.Clone()
}

// forReaders is m as a read or a write's answer hands it to whoever asked:
// without whom an agent's post was written for ([Message.WorkingFor]), which
// the document keeps for the engine's own reads alone. A room's reader needs
// no requester's seat, nor a credential's login and apps, to read what was
// said, so neither leaves the package on a message: the transcript, a thread,
// the agents' read tools and the answer of a post, an edit or a removal.
func (m Message) forReaders() Message {
	m.WorkingFor = nil
	return m
}

// speaker is the seat that said m, "" for the engine's own line
// ([AuthorSystem]): no seat said it, whatever seat shares the name it is stored
// under, so it starts no thread a seat is woken by, joins none and answers
// nobody's receipt.
func (m Message) speaker() string {
	if m.AuthorKind == AuthorSystem {
		return ""
	}
	return m.Author
}

// widen adds the seats an EDIT newly reached to a post's reach, each once and
// after the seats it already reached, keeping the reason a seat was first
// reached for; and reports the seats it added.
//
// An edit wakes only whom it newly names ([ResolveMentions]), so its record's
// recipients are new mentions, and a seat already reached for another reason
// (it follows the room in full) was told about the message when it was posted.
func (r *Reach) widen(n *Notify) []string {
	if r == nil || n == nil {
		return nil
	}
	held := make(map[string]bool, len(r.Recipients))
	for _, seat := range r.Recipients {
		held[seat.Handle] = true
	}
	r.Truncated = r.Truncated || n.WakesTruncated
	var added []string
	for _, rec := range n.Recipients {
		if held[rec.Handle] {
			continue
		}
		if len(r.Recipients) >= MaxRecipients {
			r.Truncated = true
			break
		}
		held[rec.Handle] = true
		r.Recipients = append(r.Recipients, Reached(rec))
		added = append(added, rec.Handle)
	}
	return added
}

// EventKind is what a room event says happened.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// written, so a line a newer peer wrote is a value rather than a panic, and a
// reader draws it as a plain room event.
type EventKind string

// The seven kinds, each written by the applier from the record that made the
// change, never by a writer: who joined, was added, left or was taken out,
// and what the room's words were set to.
const (
	// EventJoined is seats coming into the room: a room's founding members,
	// a seat joining by itself, and the org chart or an auto-join room
	// putting a seat in (authored by the engine).
	EventJoined EventKind = "joined"

	// EventAdded is the author putting other seats in the room.
	EventAdded EventKind = "added"

	// EventLeft is seats going: a seat leaving by itself, and the org chart
	// taking a seat out of its unit's room (authored by the engine).
	EventLeft EventKind = "left"

	// EventRemoved is the author taking other seats out of the room.
	EventRemoved EventKind = "removed"

	// EventDescription and EventTopic are the room's purpose and topic set
	// to [RoomEvent.Text], an empty text being the value cleared.
	EventDescription EventKind = "description"
	EventTopic       EventKind = "topic"

	// EventTemplate is the room set up from the template named
	// [RoomEvent.Text] ([ChannelPatch.Template]): ONE line in place of the
	// description and topic lines the same patch would otherwise write,
	// because the person did one thing.
	EventTemplate EventKind = "template"
)

// EventKinds is every kind.
func EventKinds() []EventKind {
	return []EventKind{EventJoined, EventAdded, EventLeft, EventRemoved,
		EventDescription, EventTopic, EventTemplate}
}

// Valid reports whether k is a kind this build writes.
func (k EventKind) Valid() bool { return slices.Contains(EventKinds(), k) }

// RoomEvent is a room's own change, as the transcript row that records it
// carries it.
//
// A ROW OF THE TRANSCRIPT, not a table: a person reads it WHERE IT HAPPENED, so
// it takes the next number in the room's sequence and pages, cursors, live
// contiguity, the prune and the erase carry it. The `event` column every
// conversational read filters on keeps it from being CONVERSATION (never an
// unread, mention, preview or search hit; no reply, reaction, edit or delete).
//
// THE APPLIER WRITES IT, derived from the record that made the change in that
// record's own transaction: a second record could land one and lose the other.
type RoomEvent struct {
	Kind EventKind `json:"kind"`

	// Handles are the seats the event is about, sorted: who joined, was
	// added, left or was taken out. Empty for the three kinds about the
	// room's words.
	Handles []string `json:"handles,omitempty"`

	// Text is the new description or topic, empty when it was cleared, or
	// the template's name. Words somebody wrote, so a reader fences them.
	Text string `json:"text,omitempty" untrusted:"fence"`
}

// ErrUnknownVersion reports a document a newer build wrote.
type ErrUnknownVersion struct {
	Got  int
	Want int
}

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("chatlet: document version %d was written by a newer build "+
		"(this one writes %d) — it is left alone rather than rewritten, because "+
		"a rewrite from here would drop whatever the new shape added",
		e.Got, e.Want)
}

// ---- encoding ---------------------------------------------------------- //
//
// THROUGH [jsoncarry], the one implementation of the carried-field rule every
// state-log domain shares: a merge of its own here would be a second answer to
// whether a carried field loses to a known one, and the two would disagree the
// first time one of them changed.

// The known field names per document, READ OFF EACH STRUCT'S OWN TAGS — see
// [jsoncarry.Names] for what a hand-kept list of the omitempty ones cost: a
// name missing from it is decoded into the struct AND carried as unknown, so
// the next encode writes the stale carried copy back over what the caller set.
var (
	channelFields = jsoncarry.Names(Channel{})
	messageFields = jsoncarry.Names(Message{})
)

// EncodeChannel renders a room.
func EncodeChannel(c Channel) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

// DecodeChannel reads a room.
func DecodeChannel(data []byte) (Channel, error) {
	var c Channel
	extra, err := jsoncarry.Decode(data, &c, channelFields)
	if err != nil {
		return Channel{}, fmt.Errorf("chatlet: decode channel: %w", err)
	}
	if err := checkDocumentVersion(c.V); err != nil {
		return Channel{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeMessage renders a message.
func EncodeMessage(m Message) ([]byte, error) { return jsoncarry.Encode(m, m.Extra) }

// DecodeMessage reads a message.
func DecodeMessage(data []byte) (Message, error) {
	var m Message
	extra, err := jsoncarry.Decode(data, &m, messageFields)
	if err != nil {
		return Message{}, fmt.Errorf("chatlet: decode message: %w", err)
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
