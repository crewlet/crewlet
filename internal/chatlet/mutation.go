package chatlet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
//     notification, where a card is what they are for — and the complete
//     value is what a woken seat reads, so a message is never handed to the
//     seat it addresses cut at a card's length.
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
	// it is archived.
	OpPatch OpKind = "patch"

	// OpRetention sets how long a room keeps what is said in it.
	//
	// ITS OWN OP rather than a field of [OpPatch], because a shorter
	// horizon is a prune on a delay: it is the one room setting that
	// destroys, so it is an admin's ([Store.SetRetention]) where a topic is
	// any member's, and a record that carried both would arbitrate one
	// authority for the two.
	OpRetention OpKind = "retention"

	// OpMembers replaces a room's membership, WHOLE.
	//
	// ITS OWN OP rather than a collection on [OpPatch], because it is the
	// one channel write that is about PEOPLE: it decides who may read a
	// private room, and is what that room's existence is revealed by.
	// Folding it into a patch would file that under "settings changed".
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
	// because the message kind carries no expectation: a whole-set record
	// would let the later of two simultaneous reactors erase the earlier one.
	OpReact OpKind = "react"

	// OpErase REMOVES message rows permanently — an admin redacting what
	// should never have been said. Only a person or a credential may publish
	// one (see [MessageErase]), and it INSTALLS A GATE by its op rather
	// than by its kind, on an ordinary channel subject.
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

// OpKinds are the thirteen, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPatch, OpRetention, OpMembers, OpPost, OpEdit, OpDelete, OpReact,
	OpErase, OpPrune, OpEviction, OpGeneration, OpBarrier,
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
// pair nobody wrote. So the one builder every gesture's record goes through
// ([decide]) CONSULTS it, and refuses a record formed on any other kind.
var opRidesOn = map[OpKind]ObjectKind{
	OpPatch:      KindChannel,
	OpRetention:  KindChannel,
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
	// person IS a seat here and every principal of theirs resolves to one.
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
// there is one), the room and its membership in ONE TRANSACTION. That is what removes the two-key sequence a coordination bucket
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
	// other kind. It is what the room's answerer resolves through, so a unit
	// room that did not name its unit would route a person's question to
	// nobody.
	Unit string `json:"unit,omitempty"`

	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`

	// Members is the founding membership, WHOLE.
	//
	// A ROOM IS MADE WITH NO RETENTION OVERRIDE: it keeps what the company
	// keeps until somebody sets one ([RetentionSet]), which is its own
	// record because a shorter horizon is a destruction.
	Members []Member `json:"members,omitempty"`

	CreatedBy     string     `json:"created_by"`
	CreatedByKind AuthorKind `json:"created_by_kind"`

	// ChartEpoch is the configuration activation a UNIT room is created
	// from ([configplane.ActivationStamp]) — required for that kind and
	// refused for every other, whose founding membership is the author's
	// rather than the chart's. See [Channel.ChartEpoch].
	ChartEpoch int64 `json:"chart_epoch,omitempty"`
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
	if err := checkTopic(c.Topic); err != nil {
		return err
	}
	if err := checkPurpose(c.Purpose); err != nil {
		return err
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
		return invalid("unit", "a %s room names no unit, and its answerer (the "+
			"unit's lead) resolves through it, so a room without one routes a "+
			"person's question to nobody", c.Kind)
	}
	if c.Kind != KindUnit && c.Unit != "" {
		return invalid("unit", "a %s room names unit %q — only a %s room "+
			"belongs to one, and a unit stated here would be read by the "+
			"fallback in a room that has no lead", c.Kind, c.Unit, KindUnit)
	}
	// THE CHART'S ROOM CARRIES THE CHART'S ACTIVATION, and only it does. A
	// unit room without one is a membership no later apply can be compared
	// against; any other room with one claims the chart decided a
	// membership somebody chose.
	if c.Kind == KindUnit && c.ChartEpoch <= 0 {
		return invalid("chart_epoch", "a %s room is created from the org "+
			"chart and names no activation it came from — without one, a node "+
			"applying an older chart could not be told apart from the current "+
			"one", c.Kind)
	}
	if c.Kind != KindUnit && c.ChartEpoch != 0 {
		return invalid("chart_epoch", "a %s room carries chart epoch %d — only "+
			"a %s room's membership is the org chart's", c.Kind, c.ChartEpoch,
			KindUnit)
	}
	return nil
}

// ChannelPatch changes a room's settings. Every field is a pointer, and absent
// means unchanged.
//
// # Why there is no retention here
//
// A room's retention is its own record ([RetentionSet]): a shorter horizon is a
// prune on a delay, so it is the one setting that destroys what a room said,
// and it is an admin's to set where a topic is any member's.
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
	// of it readable — its creator's, its unit's lead's or an admin's to
	// move, never any member's ([ChannelAuthority]).
	Archived *bool `json:"archived,omitempty"`

	// Template is the name of the template this patch sets the room up
	// from, and the room's transcript records it as ONE line
	// ([EventTemplate]) in place of the topic and description lines the
	// same patch would otherwise write.
	//
	// A NAME AND NOTHING ELSE. A template is the client's preset: the words
	// it sets are this patch's own Topic and Purpose, carried and applied
	// like any other, and the server keeps no catalogue of templates to
	// check the name against. So a template patch must set at least one of
	// the two, or it would be a line saying the room was set up when
	// nothing about it changed.
	Template *string `json:"template,omitempty"`

	// AutoJoin turns the room's auto-join on or off ([Channel.AutoJoin]):
	// an administrator's to move, never any member's ([ChannelAuthority]).
	AutoJoin *bool `json:"auto_join,omitempty"`

	// AutoJoinSeats is the company's seats when the setting is turned ON,
	// carried WHOLE and sorted: the seats the room accounts for from the
	// start, so the setting adds the people the company takes on from now
	// and nobody already in it. Decided by the writer from the org chart it
	// holds, because the applier holds no chart, and refused on any other
	// patch.
	AutoJoinSeats []string `json:"auto_join_seats,omitempty"`
}

// Validate refuses a patch this build will not write.
func (p ChannelPatch) Validate() error {
	if p.Topic == nil && p.Purpose == nil && p.Archived == nil && p.AutoJoin == nil {
		return invalid("patch", "a patch sets no field — an empty patch is a "+
			"record on the log and a live frame for a change nobody made")
	}
	if len(p.AutoJoinSeats) > 0 && (p.AutoJoin == nil || !*p.AutoJoin) {
		return invalid("auto_join_seats", "a patch that does not turn auto-join "+
			"on accounts for no seats: the seats are the company as it stood "+
			"when the setting was turned on")
	}
	if err := checkHandles("auto_join_seats", p.AutoJoinSeats, MaxMembers); err != nil {
		return err
	}
	if p.Topic != nil {
		if err := checkTopic(*p.Topic); err != nil {
			return err
		}
	}
	if p.Purpose != nil {
		if err := checkPurpose(*p.Purpose); err != nil {
			return err
		}
	}
	if p.Template != nil {
		name := *p.Template
		switch {
		case p.Topic == nil && p.Purpose == nil:
			return invalid("template", "a template sets a room's topic and "+
				"description, and this patch sets neither. Send the template's "+
				"topic and purpose with its name")
		case strings.TrimSpace(name) == "" || name != strings.TrimSpace(name):
			return invalid("template", "%q is not a template's name: name the "+
				"template the room was set up from, without surrounding space", name)
		case textChars(name) > MaxTemplateName:
			return invalid("template", "a template's name is %d characters "+
				"against a %d cap", textChars(name), MaxTemplateName)
		}
	}
	return nil
}

// RetentionSet states how long a room keeps what is said in it. Its subject is
// the CHANNEL, arbitrated like every other setting of the room.
type RetentionSet struct {
	V int `json:"v"`

	// RetentionDays is the room's override of the company default, and
	// ABSENT CLEARS IT: the room takes the company's horizon again. A
	// POINTER because [RetentionForever] is zero and is a real setting — a
	// present 0 keeps the room for ever — and an int that collapsed the
	// two would silently delete a year of somebody's decisions.
	RetentionDays *int `json:"retention_days,omitempty"`
}

// Validate refuses a retention this build will not write.
func (r RetentionSet) Validate() error {
	if r.RetentionDays != nil && *r.RetentionDays < 0 {
		return invalid("retention_days", "retention is %d days — a negative "+
			"window has no cutoff instant, and %d is how a room says it keeps "+
			"everything for ever", *r.RetentionDays, RetentionForever)
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

	// ChartEpoch is the configuration activation a UNIT room's membership
	// is written from ([Store.SetUnitMembers], [Store.ReleaseUnitRoom]),
	// and zero on every other membership write — the only one that is the
	// chart's. See [Channel.ChartEpoch].
	ChartEpoch int64 `json:"chart_epoch,omitempty"`

	// Unit RE-HOMES a unit room: the key of the unit the chart now gives
	// it to, which the room is recorded under from this record on ([Channel.Unit]).
	// EMPTY ON EVERY RECORD THAT MOVES NO UNIT — every write that is not
	// the chart's, and every chart write to a room already recorded under
	// that key — so the record says exactly what changed.
	//
	// THE CHART'S ALONE, and only beside its activation: a room's unit is
	// what its answerer resolves through, and it moves for the reason
	// its membership does — the org chart gave the channel to another unit
	// (moved in a reorg, or the unit renamed without an `id`) — so it rides
	// the record the chart already arbitrates on the room, stamped with the
	// same activation, and an older activation moves it back no more than
	// it moves the members back.
	Unit string `json:"unit,omitempty"`

	// AutoJoined is the seats this write puts in an auto-join room as
	// newcomers to the company ([Store.AutoJoin]), which the room accounts
	// for from this record on, sorted. Every one of them is in Members.
	// Empty on every other membership write.
	AutoJoined []string `json:"auto_joined,omitempty"`
}

// Validate refuses a membership this build will not write.
func (m MemberSet) Validate() error {
	if err := checkHandles("auto_joined", m.AutoJoined, MaxMembers); err != nil {
		return err
	}
	for _, handle := range m.AutoJoined {
		if !slices.ContainsFunc(m.Members, func(x Member) bool { return x.Handle == handle }) {
			return invalid("auto_joined", "%q is put in the room by auto-join "+
				"and is not in the membership this record states", handle)
		}
	}
	if m.ChartEpoch < 0 {
		return invalid("chart_epoch", "a membership names activation %d — an "+
			"activation is an instant, and zero is how a write says it is not "+
			"the chart's", m.ChartEpoch)
	}
	if m.Unit != "" && m.ChartEpoch == 0 {
		return invalid("unit", "a membership re-homes its room to unit %q and "+
			"names no activation — only the org chart's write moves a room's "+
			"unit, and it always says which configuration it came from", m.Unit)
	}
	if m.Unit != strings.TrimSpace(m.Unit) {
		return invalid("unit", "%q carries surrounding space — a unit key is "+
			"compared as written, so a padded one names no unit", m.Unit)
	}
	// AN EMPTY SET IS LEGAL AND IS NOT THE ZERO VALUE'S MEANING: emptying
	// a room is a real gesture, and it is spelled by a record that carries
	// the empty list rather than by one that carries nothing.
	return checkMembers(m.Members, MaxMembers)
}

// MessagePost is somebody saying something. Its subject is the ROOM's message
// stream, sequenced — see [KindMessage].
type MessagePost struct {
	V int `json:"v"`

	// MessageID is the uuid the applier files the message under, MINTED BY
	// THE WRITER. Sequenced records race nobody, so there is no lost claim
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
	// besides its text. Each is an absolute http, https or mailto address,
	// carried as its author wrote it ([LinkSchemes]).
	Links []string `json:"links,omitempty"`

	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`

	// MembersOnly says the room admits its members only, as the writer's
	// snapshot read the room: the applier refuses the post
	// ([VerdictNotMember]) when a record ahead of it took the author out.
	//
	// THE WRITER'S ANSWER, CARRIED, rather than the applier's reading of
	// the room's kind: which kinds admit members only is a rule a later
	// build may change ([privateRoom]), and two builds deciding it at
	// apply would write different rows from one record. What the applier
	// decides is only the fact the record cannot carry — whether the
	// author is still in the room at this position.
	MembersOnly bool `json:"members_only,omitempty"`
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
// # Why it is a channel record and why no seat may write one
//
// It deletes up to [MaxEraseMessages] rows across one room, so the room is its
// blast radius and the channel subject is what serialises it against the
// room's own state — an erase racing an archive has a defined order rather
// than two. And it is the one gesture that destroys what somebody wrote, so it
// is a PERSON's or a CREDENTIAL's — an admin's, which the authority table
// decides before it is published: an agent that could erase its own messages
// could erase the evidence of what it did, and the author kind is checked
// HERE, in the decide, rather than only at whichever route happened to be the
// way in.
//
// # Why it carries no room and no count
//
// The room is the SUBJECT, and how many is the length of the list. A copy of
// either beside the thing it copies could only ever disagree with it, and
// nothing reads a summary of an erase off the log: the account a person reads
// is the audit record of whoever made it, written where the request was
// served.
type MessageErase struct {
	V int `json:"v"`

	// MessageIDs is exactly what this apply removes, stated rather than
	// selected: a predicate each node evaluated would delete a different
	// set on a node whose rows were behind.
	MessageIDs []string `json:"message_ids"`

	// Reason is why, as whoever erased it stated it. The route requires
	// one; the record carries it for as long as the log holds the record.
	Reason string `json:"reason,omitempty"`

	By     string     `json:"by"`
	ByKind AuthorKind `json:"by_kind"`
}

// Validate refuses an erase this build will not write.
func (m MessageErase) Validate() error {
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
	if len(m.Reason) > MaxEraseReason {
		return invalid("reason", "an erase's reason is %d bytes against a %d cap "+
			"— it is a sentence or two on a record every node keeps, and a "+
			"longer account belongs to the incident it is part of",
			len(m.Reason), MaxEraseReason)
	}
	if m.By == "" {
		return invalid("by", "an erase names nobody")
	}
	if !erases(m.ByKind) {
		return invalid("by_kind", "an erase was written as %q and only %q or %q "+
			"may publish one — an author that could erase its own messages could "+
			"erase the evidence of what it did, so the rule is here in the "+
			"decide and not only at whichever route was the way in",
			m.ByKind, AuthorHuman, AuthorOperator)
	}
	return nil
}

// Reaction adds or removes one reaction. Its subject is the room's message
// stream, sequenced.
//
// A SINGLE TOGGLE rather than a set, and the shape is forced: the message kind
// carries no expectation, so a record holding the whole reaction set would let
// the later of two simultaneous reactors overwrite the earlier one's mark with
// a set that never contained it.
//
// [MaxReactionEmoji] is therefore the APPLIER's to decide — a toggle cannot
// see the set it joins, and the applier holds the message's rows in its own
// transaction — and its verdict ([VerdictReactionsFull]) is how the writer
// hears of it; stated at the constant so nobody looks for it here.
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

	// Cap is the most distinct emoji the WRITER's build lets a message
	// carry — [MaxReactionEmoji], stamped on every reaction added, and
	// absent on one taken back, which no cap bounds.
	//
	// THE WRITER'S CAP, CARRIED, rather than each applier's constant: the
	// cap is a rule a later build may change, and a record decided by the
	// applying node's own constant would be declined on a node built with
	// one cap and applied on a node built with another, so their rows
	// would differ for good. Stamped, every node declines the same record
	// at the same count ([VerdictReactionsFull]), as [MessagePost.MembersOnly]
	// carries the writer's answer about who a room admits.
	Cap int `json:"cap,omitempty"`
}

// Validate refuses a reaction this build will not write.
func (r Reaction) Validate() error {
	if r.MessageID == "" {
		return invalid("message_id", "a reaction names no message")
	}
	if !r.Removed && r.Cap < 1 {
		return invalid("cap", "a reaction added carries no cap, so no applier "+
			"could say whether it is one emoji too many — the writer stamps "+
			"its own (%d)", MaxReactionEmoji)
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
// IT CARRIES NO CHANNEL ID, as [MessageErase] carries none: the subject is
// the room, and a second copy of it here could only ever disagree with the
// first.
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
		case OpRetention:
			return decodePayload[RetentionSet](rec)
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
	return nil, fmt.Errorf("chatlet: no payload shape for (%s, %s) — a record's "+
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
		return nil, fmt.Errorf("chatlet: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return nil, fmt.Errorf("chatlet: decode the %s payload on %s: %w",
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
		return nil, fmt.Errorf("chatlet: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("chatlet: a barrier carries no op id and this one " +
			"has one — an op id becomes a message id, and a duplicate ack is " +
			"served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED, never [RecordVersion] and never left to the
			// table: see [baseRecordVersion]. The encoder refuses a
			// barrier carrying a versioned field.
			V:       baseRecordVersion,
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

// checkHandles holds a list of seat handles to its cap and to one spelling
// of each: sorted, distinct and none empty, so a record's bytes say one thing
// whichever order a writer gathered the set in.
func checkHandles(field string, handles []string, max int) error {
	if len(handles) > max {
		return invalid(field, "%d seats against a %d cap", len(handles), max)
	}
	for i, h := range handles {
		if strings.TrimSpace(h) == "" || h != strings.TrimSpace(h) {
			return invalid(field, "%q is not a seat handle", h)
		}
		if i > 0 && handles[i-1] >= h {
			return invalid(field, "the seats are not sorted and distinct at %q", h)
		}
	}
	return nil
}

// textChars is how long a person's text is as they see it: its Unicode code
// points. It is what [MaxTopic], [MaxPurpose] and [MaxTemplateName] count.
func textChars(s string) int { return utf8.RuneCountInString(s) }

// checkTopic and checkPurpose hold a room's two words to their caps, one
// rule for the create and the patch alike.
func checkTopic(topic string) error {
	if n := textChars(topic); n > MaxTopic {
		return invalid("topic", "a topic is %d characters against a %d cap",
			n, MaxTopic)
	}
	return nil
}

func checkPurpose(purpose string) error {
	if n := textChars(purpose); n > MaxPurpose {
		return invalid("purpose", "a purpose is %d characters against a %d cap. "+
			"Anything longer is describing a project, and a project has a page",
			n, MaxPurpose)
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

// checkLinks bounds what a message points at, and holds each link to what a
// link is ([checkLink]).
//
// EVERY LINK THAT IS NOT ONE IS NAMED IN THE ONE REFUSAL (ADR-0059), each by
// its entry: a caller that wrote several the same wrong way corrects them all
// at once rather than one per attempt. One alone is refused as its own entry
// (`links[1]`), and several as `links`, each entry beside what is wrong with
// it.
func checkLinks(links []string) error {
	if len(links) > MaxLinks {
		return invalid("links", "a message carries %d links and the maximum is "+
			"%d — there are no attachments here, so a message with more than "+
			"that is a page's worth of references", len(links), MaxLinks)
	}
	var faults []string
	var first error
	for i, l := range links {
		err := checkLink(fmt.Sprintf("links[%d]", i), l)
		if err == nil {
			continue
		}
		if first == nil {
			first = err
		}
		faults = append(faults, Sentence(err))
	}
	if len(faults) > 1 {
		return invalid("links", "%d of the links are not ones a message can "+
			"carry: %s", len(faults), strings.Join(faults, "; "))
	}
	return first
}

// LinkSchemes are the schemes a message's link is written in. A link is an
// ABSOLUTE address in one of them, written whole, with no space or control
// character in it, and it is stored exactly as it was sent ([checkLink] holds
// every link to that). Every tool that takes links names them.
//
// # Why a link is an absolute address
//
// A message's links are read wherever the message is: on a person's screen,
// by a seat in the thread it is handed, and by any client that prints them
// for somebody to open. A relative reference (`docs/plan`, `/worklet/ENG-1`),
// a scheme-relative one (`//host/plan`) and a fragment (`#/worklet/ENG-1`,
// one of the dashboard's own routes) resolve against whichever page shows
// them, so each reader would open a different place or none at all. And the
// dashboard's one address check opens http, https and mailto and draws any
// other scheme as words, so a link in any other scheme is not one a reader
// can follow.
//
// HTTP IS TAKEN BESIDE HTTPS, where a decision's evidence takes https alone
// (internal/worklet's Evidence): that address is drawn as the button a person
// presses while weighing a decision, while a message's link is a reference in
// a conversation, often to a company's own host serving plain http, and how
// a reader draws it is that reader's link rules to decide. What the store
// holds is that it is an address a reader can open.
//
// NO SPACE AND NO CONTROL CHARACTER, ANYWHERE: the dashboard's address check
// strips them before it reads an address, a line of text ends a link at its
// first space, and the thread a seat reads joins a message's links with
// spaces, so a link holding one would open a different address for each
// reader. A space inside a path is written %20. url.Parse refuses a control
// character itself, and its error says less than this one does.
//
// THE LINK IS STORED AS SENT. It is checked and never rewritten, so every
// reader opens exactly the address its author wrote.
func LinkSchemes() []string { return []string{"http", "https", "mailto"} }

// checkLink holds one link to what a link is ([LinkSchemes] says what and
// why). A refusal names the entry (`links[1]`), so a caller sending several
// knows which one to change.
func checkLink(field, link string) error {
	if link == "" {
		return invalid(field, "a message carries an empty link, which "+
			"points nowhere while counting against the cap")
	}
	if len(link) > MaxLinkBytes {
		return invalid(field, "a link is %d bytes against a %d cap, which "+
			"already clears every url any integration surface mints, signed "+
			"query strings included", len(link), MaxLinkBytes)
	}
	if strings.ContainsFunc(link, func(r rune) bool { return r == ' ' || unicode.IsControl(r) }) {
		return invalid(field, "%q holds a space or a control character, and a "+
			"link is one unbroken address: write a space inside it as %%20", link)
	}
	address, err := url.Parse(link)
	if err != nil {
		// THE PARSER'S CAUSE ALONE: its whole error quotes the link again.
		cause := err
		var parsed *url.Error
		if errors.As(err, &parsed) {
			cause = parsed.Err
		}
		return invalid(field, "%q is not an address (%v): write it whole, as "+
			"https://example.com/page", link, cause)
	}
	switch {
	case address.Scheme == "":
		return invalid(field, "%q is not an absolute address, and a relative one "+
			"resolves against whichever page shows it: write it whole, with its "+
			"scheme and host, as https://example.com/page", link)
	case !slices.Contains(LinkSchemes(), address.Scheme):
		return invalid(field, "%q uses the %s scheme, and a link is an http, https "+
			"or mailto address, the ones every reader of a message opens",
			link, address.Scheme)
	case address.Scheme == "mailto" && address.Opaque == "":
		return invalid(field, "%q names no address to write to: write it as "+
			"mailto:name@example.com", link)
	case address.Scheme != "mailto" && (address.Opaque != "" || address.Hostname() == ""):
		return invalid(field, "%q names no host: write it whole, as "+
			"https://example.com/page", link)
	}
	return nil
}
