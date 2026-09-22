package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op).
//
// A small whole-document object — a membership set, a gate — travels as FULL
// POST-STATE: one document field, one upsert, and no patch semantics to get
// wrong. A ROOM'S SETTINGS travel as a TYPED PATCH, because a topic change
// must not have to restate a thousand members.
//
// The patch's rule is the tracker's and the wiki's, and it is the right one: a
// nil field is unchanged, and the POINTER is what tells "set this to empty"
// from "leave it alone" — which a plain string cannot. Two rules travel with
// it:
//
//  1. A named scalar present in the patch carries its COMPLETE new value,
//     never an excerpt and never a diff. The [MaxExcerpt] previews live in the
//     notification, where a wake prompt is what they are for.
//  2. A collection the write TOUCHES is carried whole; one it does not touch
//     is absent. That is what makes a record able to rebuild the row on a
//     replay from zero, and it is affordable because every collection here is
//     capped.

// OpKind is what a record does.
type OpKind string

const (
	// OpCreate makes a channel. It is the ONE op whose subject depends on
	// what it is making — see [SubjectKind].
	OpCreate OpKind = "create"

	// OpPatch changes a room's settings: its topic, its purpose, whether
	// it is archived, how long it keeps what is said in it.
	OpPatch OpKind = "patch"

	// OpMembers replaces a room's membership, WHOLE.
	//
	// ITS OWN OP rather than a collection on [OpPatch], because it is the
	// one channel write that is about PEOPLE: it wakes whoever was added,
	// it is what a private room's existence is revealed by, and its
	// history row is the record of who let somebody in. Folding it into a
	// patch would file all of that under "settings changed".
	OpMembers OpKind = "members"

	// OpPost is somebody saying something, and the overwhelming majority
	// of records on this log.
	OpPost OpKind = "post"

	// OpEdit rewrites a message's body in place.
	OpEdit OpKind = "edit"

	// OpDelete tombstones a message: it clears the body and keeps the row,
	// so a thread hung off it still has its root and a reply does not
	// become unreachable. Removing the row is [OpErase], which is a
	// different gesture with a different author.
	OpDelete OpKind = "delete"

	// OpReact adds or removes one reaction. A TOGGLE rather than a set,
	// because the message kind is additive: a whole-set record would let
	// the later of two simultaneous reactors erase the earlier one.
	OpReact OpKind = "react"

	// OpErase REMOVES message rows permanently — an operator redacting
	// what should never have been said. Only an operator may publish one
	// (see [MessageErase]), and it INSTALLS A GATE by its op rather than
	// by its kind, on an ordinary channel subject.
	OpErase OpKind = "erase"

	// OpPrune removes everything in a room older than a stated instant,
	// which is how retention is enforced.
	//
	// A RECORD RATHER THAN A LOCAL SWEEP, and that is the whole reason it
	// exists: a sweep evaluating "older than 365 days" against each node's
	// own clock deletes a different set on every node, for ever. A cutoff
	// on the log is one instant every applier reads identically. It
	// installs a gate for [OpErase]'s reason — it deletes rows.
	OpPrune OpKind = "prune"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"
)

// OpKinds are the twelve, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPatch, OpMembers, OpPost, OpEdit, OpDelete, OpReact, OpErase,
	OpPrune, OpEviction, OpGeneration, OpBarrier,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// opRidesOn is the op → object kind table for every op but [OpCreate].
//
// A TABLE RATHER THAN A SWITCH AT EACH WRITER, because it is the half of the
// dispatch that decides where a record is PUBLISHED, and the applier's
// (kind, op) switch is the half that decides what it MEANS. Written twice,
// the two stop agreeing the first time an op moves: the record publishes to a
// subject whose kind the applier has no case for, and the domain faults on a
// pair nobody wrote.
var opRidesOn = map[OpKind]ObjectKind{
	OpPatch:      KindChannel,
	OpMembers:    KindChannel,
	OpErase:      KindChannel,
	OpPrune:      KindChannel,
	OpPost:       KindMessage,
	OpEdit:       KindMessage,
	OpDelete:     KindMessage,
	OpReact:      KindMessage,
	OpEviction:   KindEviction,
	OpGeneration: KindGeneration,
	OpBarrier:    KindBarrier,
}

// SubjectKind names the object kind whose subject an op is published on,
// reporting false for a pair this build does not write.
//
// `of` is the channel's own kind and is read for [OpCreate] ALONE, because a
// create is the one op with two answers:
//
//   - A NAMED room's create arbitrates on its NAME ([KindChannelName]), so two
//     people typing `#launch` contend at the broker and exactly one wins.
//   - A DIRECT conversation has no name to contend for, so its create
//     arbitrates on its own DERIVED id ([KindChannel]), create-only at an
//     expectation of zero — and the two sides opening it from two nodes derive
//     the SAME id, so the loser is told the room already exists, which is the
//     right answer because it does. See [DirectChannelID].
//
// An unknown channel kind answers false rather than guessing, because the two
// answers are different arbitration disciplines and picking the wrong one for
// a named room is two rooms with one name.
func SubjectKind(op OpKind, of Kind) (ObjectKind, bool) {
	if op == OpCreate {
		switch {
		case of.Direct():
			return KindChannel, true
		case of.Named():
			return KindChannelName, true
		default:
			return "", false
		}
	}
	k, ok := opRidesOn[op]
	return k, ok
}

// Payload is what every typed payload here satisfies.
//
// ONE METHOD, and it is the one every write path must call: a payload states
// its own caps, and a cap enforced at each caller is a cap missing from
// whichever caller is written next. [DecodeMutation] answers this interface so
// a node reading somebody else's record can hold it to the same bar the writer
// was held to.
type Payload interface {
	// Validate refuses a payload this build will not write, NAMING THE
	// FIELD it refused and what to do about it.
	Validate() error
}

// Member is one seat's or one person's standing in a room.
type Member struct {
	// Handle is the seat handle. It is the identity every caller that
	// reaches this record already holds — a human seat included, since a
	// person IS a seat here and a token resolves to one.
	Handle string `json:"h"`

	// FollowAll makes every message in the room reach this member, under
	// [ReasonFollowAll], whether or not it named them.
	//
	// AN EXPLICIT FLAG rather than a property derived from the room's
	// kind, because the engine sets it for a unit's own agent seats in
	// that unit's room AND a person may turn it on for a room they care
	// about. Derived from the kind, the second case would have no way to
	// exist and the first would silently change whenever somebody moved
	// between units.
	FollowAll bool `json:"fa,omitempty"`
}

// ChannelCreate makes a room. Its subject is the NAME for a named room and the
// room's own DERIVED id for a direct one — see [SubjectKind].
//
// The whole create is ONE record: the applier writes the name claim (where
// there is one), the room, its membership and its first history entry in ONE
// TRANSACTION. That is what removes the two-key sequence a coordination bucket
// would have forced — there is no window in which a name is held by a room
// that was never written, so there is no orphan to step over and no grace rule
// anywhere in this package.
type ChannelCreate struct {
	V int `json:"v"`

	// ChannelID is the id the applier files the room under. MINTED BY THE
	// WRITER for a named room and carried, never derived, so a create that
	// lost its race has not written a room under an id the winner also
	// chose. For a DIRECT room it is [DirectChannelID] over exactly
	// [ChannelCreate.Members] — Validate recomputes it and REFUSES a
	// record that derived one id and claimed another, which is the same
	// guard the wiki puts on a title token.
	ChannelID string `json:"channel_id"`

	Kind Kind `json:"kind"`

	// Name is the DISPLAYED address, already normalised: unlike a page
	// title there is no capitalisation to preserve, because [ValidName]
	// admits none. Empty for a direct room, which has no address.
	Name string `json:"name,omitempty"`

	// Unit is the unit a [KindUnit] room belongs to, and empty for every
	// other kind. It is what the lead fallback resolves through, so a unit
	// room that did not name its unit would route a person's question to
	// nobody.
	Unit string `json:"unit,omitempty"`

	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`

	// Members is the founding membership, WHOLE.
	Members []Member `json:"members,omitempty"`

	// RetentionDays overrides the company default for this room. A
	// POINTER because [RetentionForever] is zero and is a real setting:
	// absent means "take the company's", and a present 0 means "keep this
	// room for ever".
	RetentionDays *int `json:"retention_days,omitempty"`

	CreatedBy     string     `json:"created_by"`
	CreatedByKind AuthorKind `json:"created_by_kind"`
}

// Validate refuses a create this build will not write.
func (c ChannelCreate) Validate() error {
	if c.ChannelID == "" {
		return invalid("channel_id", "a create mints the room's id and carries "+
			"it; a record without one writes a room nothing can address")
	}
	if !c.Kind.Valid() {
		return invalid("kind", "%q is not a channel kind this build serves — "+
			"the kind decides whether the create arbitrates on a name or on a "+
			"derived id, so an unknown one has no discipline at all", c.Kind)
	}
	if err := checkMembers(c.Members, c.memberCap()); err != nil {
		return err
	}
	if err := c.checkAddress(); err != nil {
		return err
	}
	if len(c.Topic) > MaxTopic {
		return invalid("topic", "a topic is %d bytes against a %d cap",
			len(c.Topic), MaxTopic)
	}
	if len(c.Purpose) > MaxPurpose {
		return invalid("purpose", "a purpose is %d bytes against a %d cap — "+
			"anything longer is describing a project, and a project has a page",
			len(c.Purpose), MaxPurpose)
	}
	if c.RetentionDays != nil && *c.RetentionDays < 0 {
		return invalid("retention_days", "retention is %d days — a negative "+
			"window has no cutoff instant, and %d is how a room says it keeps "+
			"everything for ever", *c.RetentionDays, RetentionForever)
	}
	if c.CreatedBy == "" {
		return invalid("created_by", "a room records who made it, and a create "+
			"with no author is a room nobody is accountable for")
	}
	if !c.CreatedByKind.Valid() {
		return invalid("created_by_kind", "%q is not an author kind this build "+
			"serves", c.CreatedByKind)
	}
	return nil
}

// memberCap is the bound on this room's founding membership.
//
// A DIRECT room's is far tighter than a named one's, and it is the cap that
// makes the derived id meaningful: past [MaxDMParticipants] a conversation is
// a room, and a room has a name somebody can join by.
func (c ChannelCreate) memberCap() int {
	if c.Kind.Direct() {
		return MaxDMParticipants
	}
	return MaxMembers
}

// checkAddress holds a create to the discipline its kind implies: a named room
// claims a legal name, a direct room claims none and derives its id from
// exactly the people in it.
func (c ChannelCreate) checkAddress() error {
	if c.Kind.Direct() {
		if c.Name != "" {
			return invalid("name", "a %s conversation is named %q — a direct "+
				"conversation has no address, and a name here would be claimed "+
				"by nothing and resolvable by nobody", c.Kind, c.Name)
		}
		if len(c.Members) < 2 {
			return invalid("members", "a %s conversation names %d participant(s) "+
				"— its id is derived from who is in it, and a set of one derives "+
				"a room nobody else can reach", c.Kind, len(c.Members))
		}
		derived := DirectChannelID(memberHandles(c.Members)).String()
		if c.ChannelID != derived {
			return invalid("channel_id", "a %s conversation between these "+
				"participants is %s and this record claims %s — the id IS the "+
				"participant set, so a record that derived one and claimed "+
				"another would make a second room with the same people in it",
				c.Kind, derived, c.ChannelID)
		}
		return nil
	}
	if c.Name == "" || !ValidName(c.Name) {
		return invalid("name", "%q is not a channel name — it is the address "+
			"the create arbitrates on, so it is lower-case alphanumerics and "+
			"hyphens, starting with an alphanumeric, at most %d bytes",
			c.Name, MaxChannelName)
	}
	if c.Kind == KindUnit && c.Unit == "" {
		return invalid("unit", "a %s room names no unit — the lead fallback "+
			"resolves through it, so a room without one routes a person's "+
			"question to nobody", c.Kind)
	}
	if c.Kind != KindUnit && c.Unit != "" {
		return invalid("unit", "a %s room names unit %q — only a %s room "+
			"belongs to one, and a unit stated here would be read by the "+
			"fallback in a room that has no lead", c.Kind, c.Unit, KindUnit)
	}
	return nil
}

// ChannelPatch changes a room's settings. Every field is a pointer, and absent
// means unchanged.
//
// # Why there is no name here
//
// A channel's NAME IS IMMUTABLE. It is the address the create arbitrated on,
// and moving it is a second claim plus a release of the first, inside one
// transaction, on a record whose subject is the NEW name — a rename op, with
// its own payload and its own arbitration, which this build does not have.
// What a room's people actually change is its topic and its purpose, and both
// are here. A `Name *string` on this patch would arbitrate on the room's own
// subject, which nobody else contends for, and hand two rooms the same
// address.
type ChannelPatch struct {
	V int `json:"v"`

	Topic   *string `json:"topic,omitempty"`
	Purpose *string `json:"purpose,omitempty"`

	// Archived closes the room to new messages while keeping every word
	// of it readable.
	Archived *bool `json:"archived,omitempty"`

	// RetentionDays is the room's override. A POINTER for
	// [RetentionForever]'s reason — see [ChannelCreate.RetentionDays].
	RetentionDays *int `json:"retention_days,omitempty"`
}

// Validate refuses a patch this build will not write.
func (p ChannelPatch) Validate() error {
	if p.Topic == nil && p.Purpose == nil && p.Archived == nil && p.RetentionDays == nil {
		return invalid("patch", "a patch sets no field — an empty patch is a "+
			"record on the log, a history row and a wake for a change nobody "+
			"made")
	}
	if p.Topic != nil && len(*p.Topic) > MaxTopic {
		return invalid("topic", "a topic is %d bytes against a %d cap",
			len(*p.Topic), MaxTopic)
	}
	if p.Purpose != nil && len(*p.Purpose) > MaxPurpose {
		return invalid("purpose", "a purpose is %d bytes against a %d cap",
			len(*p.Purpose), MaxPurpose)
	}
	if p.RetentionDays != nil && *p.RetentionDays < 0 {
		return invalid("retention_days", "retention is %d days — a negative "+
			"window has no cutoff instant, and %d is how a room says it keeps "+
			"everything for ever", *p.RetentionDays, RetentionForever)
	}
	return nil
}

// MemberSet replaces a room's membership, WHOLE.
//
// A DELTA CANNOT REBUILD A ROW ON A REPLAY FROM ZERO, and this is the
// collection where that matters most: membership is what a private room's
// readability IS, so a node that replayed a room's adds and missed one of its
// removals would serve a conversation to somebody who was taken out of it.
type MemberSet struct {
	V int `json:"v"`

	// Members is the complete membership AFTER this write.
	Members []Member `json:"members"`
}

// Validate refuses a membership this build will not write.
func (m MemberSet) Validate() error {
	// AN EMPTY SET IS LEGAL AND IS NOT THE ZERO VALUE'S MEANING: emptying
	// a room is a real gesture, and it is spelled by a record that carries
	// the empty list rather than by one that carries nothing.
	return checkMembers(m.Members, MaxMembers)
}

// MessagePost is somebody saying something. Its subject is the ROOM's message
// stream, additively — see [KindMessage].
type MessagePost struct {
	V int `json:"v"`

	// MessageID is the uuid the applier files the message under, MINTED BY
	// THE WRITER. Additive records race nobody, so there is no lost claim
	// to worry about — what the writer's id buys is that the caller can
	// name what it just said without waiting for the apply.
	MessageID string `json:"message_id"`

	Body string `json:"body,omitempty"`

	// ThreadRoot is the message this one replies to, empty on a room post.
	// It is the ROOT rather than the parent, because a thread here is one
	// level deep: a reply to a reply is a reply to the thread, which is
	// what keeps routing a property of the thread rather than a walk.
	ThreadRoot string `json:"thread_root,omitempty"`

	// Mentions are the RESOLVED handles the body named, carried whole.
	// Resolved at write time because a mention is prose — `@sarah` — and
	// resolving it on each node would make a wake depend on which node
	// read it.
	Mentions []string `json:"mentions,omitempty"`

	// Collective says the message addressed the whole room (`@channel`).
	// A FLAG rather than a mention of everybody, because the set it names
	// is the membership at APPLY time on each node and the routing cap
	// [MaxCollectiveRecipients] is what bounds it.
	Collective bool `json:"collective,omitempty"`

	// Links are what the message points at. THERE ARE NO ATTACHMENTS —
	// see the package doc — so this is the whole of what a message carries
	// besides its text.
	Links []string `json:"links,omitempty"`

	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`
}

// Validate refuses a post this build will not write.
func (m MessagePost) Validate() error {
	if m.MessageID == "" {
		return invalid("message_id", "a post mints the message's id and carries "+
			"it; a record without one writes a message nothing can reply to")
	}
	if m.Body == "" && len(m.Links) == 0 {
		return invalid("body", "a post carries neither text nor a link — an "+
			"empty message is a wake for nothing")
	}
	if err := checkBody(m.Body); err != nil {
		return err
	}
	if err := checkMentions(m.Mentions); err != nil {
		return err
	}
	if err := checkLinks(m.Links); err != nil {
		return err
	}
	if m.Author == "" {
		return invalid("author", "a message names no author, and a room where "+
			"anybody could have said anything is not a conversation")
	}
	if !m.AuthorKind.Valid() {
		return invalid("author_kind", "%q is not an author kind this build "+
			"serves", m.AuthorKind)
	}
	return nil
}

// MessageEdit rewrites a message's body in place.
//
// THE DERIVED COLLECTIONS ARE RE-CARRIED, both of them, because the body is
// what they were derived from: an edit that added a mention and did not carry
// the new set would leave the row naming whoever the first draft named.
type MessageEdit struct {
	V int `json:"v"`

	MessageID string `json:"message_id"`

	Body     string   `json:"body"`
	Mentions []string `json:"mentions,omitempty"`
	Links    []string `json:"links,omitempty"`

	EditedBy     string     `json:"edited_by"`
	EditedByKind AuthorKind `json:"edited_by_kind"`
}

// Validate refuses an edit this build will not write.
func (m MessageEdit) Validate() error {
	if m.MessageID == "" {
		return invalid("message_id", "an edit names no message")
	}
	if m.Body == "" && len(m.Links) == 0 {
		return invalid("body", "an edit leaves the message with neither text "+
			"nor a link — emptying a message is a delete, which keeps the row "+
			"so the thread hung off it still has its root")
	}
	if err := checkBody(m.Body); err != nil {
		return err
	}
	if err := checkMentions(m.Mentions); err != nil {
		return err
	}
	if err := checkLinks(m.Links); err != nil {
		return err
	}
	if m.EditedBy == "" {
		return invalid("edited_by", "an edit names no editor, and a message "+
			"whose text changed with nobody accountable is not an audit trail")
	}
	if !m.EditedByKind.Valid() {
		return invalid("edited_by_kind", "%q is not an author kind this build "+
			"serves", m.EditedByKind)
	}
	return nil
}

// MessageDelete tombstones a message: the body goes, the row stays.
//
// THE ROW STAYS so a thread hung off this message still has its root and its
// replies stay reachable. Removing the row is [MessageErase], which is a
// different gesture with a different author and a gate.
type MessageDelete struct {
	V int `json:"v"`

	MessageID string `json:"message_id"`

	DeletedBy     string     `json:"deleted_by"`
	DeletedByKind AuthorKind `json:"deleted_by_kind"`
}

// Validate refuses a deletion this build will not write.
func (m MessageDelete) Validate() error {
	if m.MessageID == "" {
		return invalid("message_id", "a deletion names no message")
	}
	if m.DeletedBy == "" {
		return invalid("deleted_by", "a deletion names nobody, and who removed "+
			"somebody's words is the whole of what the tombstone records")
	}
	if !m.DeletedByKind.Valid() {
		return invalid("deleted_by_kind", "%q is not an author kind this build "+
			"serves", m.DeletedByKind)
	}
	return nil
}

// MessageErase removes message rows permanently. Its subject is the CHANNEL.
//
// # Why it is a channel record and why only an operator may write one
//
// It deletes up to [MaxEraseMessages] rows across one room, so the room is its
// blast radius and the channel subject is what serialises it against the
// room's own state — an erase racing an archive has a defined order rather
// than two. And it is the one gesture that destroys what somebody wrote, so it
// is an OPERATOR's: an agent that could erase its own messages could erase the
// evidence of what it did, and the author kind is checked HERE, in the decide,
// rather than only at whichever route happened to be the way in.
type MessageErase struct {
	V int `json:"v"`

	// ChannelID is ECHOED although it is the subject, because the history
	// row and the notification this produces are read without decoding the
	// id list, and a gesture that removed a year of a room should say
	// which room in the one field a reader sees.
	ChannelID string `json:"channel_id"`

	// MessageIDs is exactly what this apply removes, stated rather than
	// selected: a predicate each node evaluated would delete a different
	// set on a node whose rows were behind.
	MessageIDs []string `json:"message_ids"`

	// Count is the number the history row and the wake render. It must
	// equal the length of the list, and a mismatch is REFUSED rather than
	// corrected, because the two are read by different readers and a
	// record whose summary disagrees with its effect is one nobody can
	// audit.
	Count int `json:"count"`

	Reason string `json:"reason,omitempty"`

	By     string     `json:"by"`
	ByKind AuthorKind `json:"by_kind"`
}

// Validate refuses an erase this build will not write.
func (m MessageErase) Validate() error {
	if m.ChannelID == "" {
		return invalid("channel_id", "an erase echoes the room it emptied, and "+
			"one that does not is a history row nobody can place")
	}
	if len(m.MessageIDs) == 0 {
		return invalid("message_ids", "an erase names no messages, which is a "+
			"gate-installing record that removes nothing")
	}
	if len(m.MessageIDs) > MaxEraseMessages {
		return invalid("message_ids", "an erase names %d messages and the "+
			"maximum is %d — each costs about four rows, and a record is never "+
			"split across transactions, so more than that is one apply over "+
			"the row budget. Issue more than one gesture; each is its own "+
			"auditable record", len(m.MessageIDs), MaxEraseMessages)
	}
	for _, id := range m.MessageIDs {
		if id == "" {
			return invalid("message_ids", "an erase names an empty message id, "+
				"which removes nothing while counting against the cap")
		}
	}
	if m.Count != len(m.MessageIDs) {
		return invalid("count", "an erase says it removed %d messages and names "+
			"%d — the count is what the history row and the wake render, and a "+
			"summary that disagrees with the effect is a record nobody can "+
			"audit", m.Count, len(m.MessageIDs))
	}
	if m.By == "" {
		return invalid("by", "an erase names nobody")
	}
	if m.ByKind != AuthorOperator {
		return invalid("by_kind", "an erase was written as %q and only %q may "+
			"publish one — an author that could erase its own messages could "+
			"erase the evidence of what it did, so the rule is here in the "+
			"decide and not only at whichever route was the way in",
			m.ByKind, AuthorOperator)
	}
	return nil
}

// Reaction adds or removes one reaction. Its subject is the room's message
// stream, additively.
//
// A SINGLE TOGGLE rather than a set, and the shape is forced: the message kind
// carries no expectation, so a record holding the whole reaction set would let
// the later of two simultaneous reactors overwrite the earlier one's mark with
// a set that never contained it. A toggle commutes, which is the property the
// additive kind rests on.
//
// [MaxReactionEmoji] is therefore the APPLIER's to enforce — a toggle cannot
// see the set it joins, and the applier holds the message's rows in its own
// transaction — which is stated at the constant so nobody looks for it here.
type Reaction struct {
	V int `json:"v"`

	MessageID string `json:"message_id"`

	// Emoji is the shortcode, at most [MaxEmoji] bytes.
	Emoji string `json:"emoji"`

	// Removed is what makes this a toggle rather than two ops: the pair
	// would otherwise be two words for one row's presence, and an
	// operator reading the log would have to know both.
	Removed bool `json:"removed,omitempty"`

	By string `json:"by"`
}

// Validate refuses a reaction this build will not write.
func (r Reaction) Validate() error {
	if r.MessageID == "" {
		return invalid("message_id", "a reaction names no message")
	}
	if r.Emoji == "" {
		return invalid("emoji", "a reaction carries no emoji, so there is "+
			"nothing to add or remove")
	}
	if len(r.Emoji) > MaxEmoji {
		return invalid("emoji", "an emoji is %d bytes against a %d cap — a "+
			"reaction is a shortcode, and an unbounded one is bytes every node "+
			"stores for the stream's whole retention window", len(r.Emoji), MaxEmoji)
	}
	if strings.ContainsAny(r.Emoji, " \t\n") {
		return invalid("emoji", "emoji %q carries whitespace, so two spellings "+
			"of one reaction would count as two", r.Emoji)
	}
	if r.By == "" {
		return invalid("by", "a reaction names nobody, and who reacted is the "+
			"whole of what the row holds")
	}
	return nil
}

// Prune removes everything in a room older than a stated instant. Its subject
// is the CHANNEL.
//
// A RECORD RATHER THAN A LOCAL SWEEP, which is the whole of why retention is
// expressible at all: "older than a year" evaluated against each node's own
// clock deletes a different set on every node, for ever, and nothing would
// ever report it. A CUTOFF INSTANT on the log is one number every applier
// compares identically against the broker's own stored timestamps.
//
// IT CARRIES NO CHANNEL ID, unlike [MessageErase], and the difference is that
// an erase is READ — its count and its room are rendered in a history row —
// while a prune is machinery nobody opens. The subject is the room, and a
// second copy of it here could only ever disagree with the first.
type Prune struct {
	V int `json:"v"`

	// Cutoff is the instant: everything stored STRICTLY BEFORE it goes.
	//
	// AN INSTANT RATHER THAN THE DAY COUNT IT CAME FROM, because the day
	// count is a setting that changes and the cutoff is a decision that
	// already happened — a node replaying this record a month later must
	// delete what the duty decided then, not what the same arithmetic
	// would decide now.
	Cutoff time.Time `json:"cutoff"`
}

// Validate refuses a prune this build will not write.
func (p Prune) Validate() error {
	if p.Cutoff.IsZero() {
		return invalid("cutoff", "a prune states no cutoff instant — the zero "+
			"time is before every message ever stored, so the record would be "+
			"a gate-installing append that deletes nothing")
	}
	return nil
}

// Eviction is a node's eviction from this log, or its readmission.
//
// PINNED AT [GateRecordVersion] FOR EVER. See the constant.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the INVERSE COMMIT rather than a delete, so an
	// eviction's whole history survives a replay — and a node that was
	// evicted, readmitted and evicted again reads correctly rather than as
	// one long absence.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Validate refuses an eviction this build will not write.
func (e Eviction) Validate() error {
	if e.NodeID == "" {
		return invalid("node_id", "an eviction names no node, so it fences "+
			"nobody while still stopping every applier that cannot read it")
	}
	if e.V != GateRecordVersion {
		return invalid("v", "an eviction carries version %d and every "+
			"gate-installing record carries %d, for ever — a gate a node cannot "+
			"read licenses every record above it, with no inverse that repairs "+
			"it", e.V, GateRecordVersion)
	}
	return nil
}

// Generation is a reanchor's record: this log was recreated, and every
// position below this number is comparable and safely stale.
type Generation struct {
	V          int    `json:"v"`
	Generation uint32 `json:"generation"`

	By string `json:"by,omitempty"`

	// PrevHighest is the highest sequence the fleet had seen on the OLD
	// stream, recorded so an operator can see what the transition stepped
	// over. Provenance only.
	PrevHighest uint64 `json:"prev_highest,omitempty"`

	// StreamCreatedAt is the new stream's own creation instant, which is
	// what detected the recreation.
	StreamCreatedAt time.Time `json:"stream_created_at"`
}

// Validate refuses a generation this build will not write.
func (g Generation) Validate() error {
	if g.Generation == 0 {
		return invalid("generation", "a reanchor claims generation 0, which is "+
			"the generation every record on a stream that was never recreated "+
			"already carries — nothing would be comparable against it")
	}
	if g.StreamCreatedAt.IsZero() {
		return invalid("stream_created_at", "a reanchor states no stream "+
			"creation instant, which is the evidence the recreation was "+
			"detected from")
	}
	return nil
}

// DecodeMutation reads the typed payload for one record.
//
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, so a record whose pair this
// build does not know is an error naming both rather than a nil payload the
// applier would treat as an empty patch.
func DecodeMutation(rec MutationRecord) (Payload, error) {
	switch rec.Subject.Kind {
	case KindChannelName:
		if rec.Op == OpCreate {
			return decodePayload[ChannelCreate](rec)
		}
	case KindChannel:
		switch rec.Op {
		case OpCreate:
			return decodePayload[ChannelCreate](rec)
		case OpPatch:
			return decodePayload[ChannelPatch](rec)
		case OpMembers:
			return decodePayload[MemberSet](rec)
		case OpErase:
			return decodePayload[MessageErase](rec)
		case OpPrune:
			return decodePayload[Prune](rec)
		}
	case KindMessage:
		switch rec.Op {
		case OpPost:
			return decodePayload[MessagePost](rec)
		case OpEdit:
			return decodePayload[MessageEdit](rec)
		case OpDelete:
			return decodePayload[MessageDelete](rec)
		case OpReact:
			return decodePayload[Reaction](rec)
		}
	case KindEviction:
		if rec.Op == OpEviction {
			return decodePayload[Eviction](rec)
		}
	case KindGeneration:
		if rec.Op == OpGeneration {
			return decodePayload[Generation](rec)
		}
	case KindBarrier:
		if rec.Op == OpBarrier {
			// THE ONE PAYLOAD-FREE RECORD, and nil is its value
			// rather than an absence: the applier's switch has a
			// case for it that writes nothing, which is what keeps
			// "this kind wrote nothing" distinguishable from
			// "nobody classified this kind".
			return nil, nil
		}
	}
	return nil, fmt.Errorf("chat: no payload shape for (%s, %s) — a record's "+
		"kind and op together name its payload, and a pair this build does not "+
		"know is a newer peer's record rather than an empty one",
		rec.Subject.Kind, rec.Op)
}

// decodePayload reads one typed payload.
//
// IT DOES NOT VALIDATE. The write path calls [Payload.Validate] before it
// publishes; an applier reading a record off the log must APPLY what the fleet
// accepted, and re-refusing it here would make one node's caps a reason to
// diverge from every other node's rows.
func decodePayload[T Payload](rec MutationRecord) (Payload, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return nil, fmt.Errorf("chat: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return nil, fmt.Errorf("chat: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append.
//
// # Why the framework cannot write this itself
//
// The read index owns WHEN a barrier goes out and what its acknowledgement
// proves — a quorum-committed position. The DOMAIN owns what a record on its
// log looks like: this one's envelope, its version gate, its subject grammar
// and the scope alphabet its applier files deferrals under. Neither can write
// the other's half, and this function is where they meet.
//
// AN OP ID IS REFUSED, and that is a correctness rule rather than tidiness. An
// op id becomes the Nats-Msg-Id; a repeat inside the duplicate window is
// answered from the dedupe cache with no quorum round trip at all, and the
// sequence it returns is then a position nothing confirmed — which is exactly
// the claim a barrier exists to make and the one it must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("chat: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("chat: a barrier carries no op id and this one " +
			"has one — an op id becomes a message id, and a duplicate ack is " +
			"served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V:       RecordVersion,
			Subject: BarrierSubject(),
			Op:      OpBarrier,
			Scope:   ScopeSet{Subject: true},
			Gen:     env.Gen,
		},
	})
}

// ---- the shared field checks ------------------------------------------- //
//
// ONE SPELLING EACH, because the same collection appears on more than one
// payload — a body on a post and on an edit, mentions and links on both — and
// two copies of a cap are two caps that stop agreeing the first time one
// moves.

// memberHandles is the handles of a membership set, in the order given.
func memberHandles(ms []Member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Handle
	}
	return out
}

// checkMembers bounds a membership set and refuses one that names anybody
// twice.
//
// A DUPLICATE IS REFUSED RATHER THAN FOLDED, because folding makes the record
// say something other than what it carries: the stated set is what counts
// against the cap, and — for a direct conversation — it is what the room's own
// id was derived from, where [DirectChannelID] deduplicates. A record naming
// somebody twice would derive an id for a smaller set than the one it claims.
func checkMembers(ms []Member, max int) error {
	if len(ms) > max {
		return invalid("members", "a membership names %d people and the maximum "+
			"is %d — a membership record writes one row per member and a record "+
			"is never split across transactions", len(ms), max)
	}
	seen := make(map[string]bool, len(ms))
	for _, m := range ms {
		if m.Handle == "" {
			return invalid("members", "a membership names an empty handle, "+
				"which reaches nobody while counting against the cap")
		}
		if seen[m.Handle] {
			return invalid("members", "a membership names %q twice — the stated "+
				"set is what counts against the cap and what a direct "+
				"conversation's id is derived from, so a duplicate makes the "+
				"record claim a set it did not derive", m.Handle)
		}
		seen[m.Handle] = true
	}
	return nil
}

// checkBody bounds a message body.
func checkBody(body string) error {
	if len(body) > MaxBody {
		return invalid("body", "a message is %d bytes against a %d cap — prose "+
			"longer than that is a document rather than a remark, and the "+
			"knowledge base carries 512 KiB with a title, a history and a name "+
			"somebody can find it by", len(body), MaxBody)
	}
	return nil
}

// checkMentions bounds the handles one message names.
func checkMentions(mentions []string) error {
	if len(mentions) > MaxMentions {
		return invalid("mentions", "a message names %d mentions and the maximum "+
			"is %d — every mention is a wake, so the cap is on the routing as "+
			"much as on the record", len(mentions), MaxMentions)
	}
	for _, h := range mentions {
		if h == "" {
			return invalid("mentions", "a message names an empty handle, which "+
				"wakes nobody while counting against the cap")
		}
	}
	return nil
}

// checkLinks bounds what a message points at.
func checkLinks(links []string) error {
	if len(links) > MaxLinks {
		return invalid("links", "a message carries %d links and the maximum is "+
			"%d — there are no attachments here, so a message with more than "+
			"that is a page's worth of references", len(links), MaxLinks)
	}
	for _, l := range links {
		if l == "" {
			return invalid("links", "a message carries an empty link, which "+
				"points nowhere while counting against the cap")
		}
		if len(l) > MaxLinkBytes {
			return invalid("links", "a link is %d bytes against a %d cap, which "+
				"already clears every url any integration surface mints, signed "+
				"query strings included", len(l), MaxLinkBytes)
		}
	}
	return nil
}
