// Package chat is the company's own conversation: channels, the messages in
// them, and the routing snapshot a wake is derived from.
//
// # What it is for
//
// A company running Crewlet talks to itself, and until now that talking had
// to happen in somebody else's workspace — Mattermost or Slack — which meant
// a company that configures neither has no way for a person to say anything
// to a seat at all. This is the first-party alternative, on exactly the terms
// [github.com/crewlet/crewlet/internal/tracker] is the tracker's and
// [github.com/crewlet/crewlet/internal/pages] is the wiki's: every change is
// ONE RECORD on an ordered stream, arbitrated at the broker on the subject of
// the object it changes and applied into N identical SQL copies with the
// checkpoint in the same transaction as the rows.
//
// That shape is ADR-0002 and [github.com/crewlet/crewlet/internal/statelog] is
// the record's authority. It is the framework's FOURTH domain, and what is
// particular to a conversation is below.
//
// # Why one log carries two arbitration disciplines
//
// This is the first domain whose hot path and whose arbitration path share a
// stream, and the split is the whole design.
//
// CHANNEL STATE ARBITRATES. A topic, a purpose, an archive, a membership set,
// a retention cutoff — each is a write to one room's own state, published on
// that room's subject and conditioned on its last sequence there, so two
// writers changing one room contend at the broker and exactly one wins.
//
// MESSAGES ARE ADDITIVE. A post carries NO expectation at all, on the
// precedent [github.com/crewlet/crewlet/internal/tracker.KindTurn] set for the
// same reason: the records COMMUTE. Two people talking in one room are not
// racing for anything — neither post is deciding against the other's state —
// and paying for arbitration would serialise the single hottest subject in
// the company behind itself at the declared census of [ChatMessagesPerDay].
// A rejection there would also be a message somebody typed and lost, which is
// the one failure a chat system may not have.
//
// # Why a message record is scoped to its CHANNEL and never to itself
//
// ADR-0018 is the record of this decision and of what the obvious alternative
// costs; what follows is the mechanism.
//
// A message's subject is [KindMessage] with the CHANNEL's id, and its declared
// scope is that channel's path. There is no per-message path in the scope
// alphabet at all, and that is a correctness property rather than an economy.
//
// The applier mints a per-channel sequence from LOG ORDER. Under a rolling
// upgrade a node defers a record it cannot decode and goes on applying the
// ones it can — so if a post were scoped to itself, a node holding a deferred
// post would happily apply the NEXT post in that room and hand it the sequence
// the deferred one should have had. The deferral would then be reprocessed
// into a room whose numbering had already moved, and that node's rows would
// disagree with every other node's for the life of the deployment, with no
// inverse that repairs it. Scoping the post to the channel is what makes the
// deferral block the room rather than one message in it.
//
// # Why there are no attachments
//
// A message carries up to [MaxLinks] links of [MaxLinkBytes] and NO FILE
// BYTES. The log is replicated to every node, held for the stream's whole
// retention window and replayed from zero by a node joining the fleet, so a
// file on it is a file every member stores for ever — and the store has no
// blob estate, no garbage collector and no transfer path to give one back.
// A link is a pointer to something that already has all three.
//
// # What this package deliberately does NOT do
//
//   - IT DOES NOT WAKE A HUMAN SEAT. A wake runs a TURN, and a turn is what
//     an agent seat does; routing one to a `kind: human` seat would spend a
//     model call answering on a person's behalf. A person is told through the
//     in-app unread and mention feeds, which are reads rather than wakes.
//   - IT DOES NOT PUBLISH AN EVENT. The wake is DERIVED from the committed
//     record by something that outlives the writer — see
//     [github.com/crewlet/crewlet/internal/changefeed] — rather than published
//     by the writing goroutine as a courtesy. A courtesy publish is swallowed
//     by exactly the failure that most needs the wake to survive.
//   - IT DOES NOT KEEP READ STATE ON THE LOG. Where somebody's eye has
//     reached is per-person, per-device and changes on every glance, and it is
//     not a fact a fleet has to agree on. On an ordered, identity-claiming log
//     it would be one record per screenful of scrolling, replayed by every
//     node from zero, for ever.
package chat

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The content caps, in bytes. Refused at the WRITE naming the field, never
// silently cut: a message truncated mid-sentence is an instruction somebody
// will follow the first half of.
const (
	// MaxBody bounds one message.
	//
	// THIRTY-TWO KIBIBYTES, which is the figure
	// [github.com/crewlet/crewlet/internal/tracker.MaxCommentBody] and
	// [github.com/crewlet/crewlet/internal/pages.MaxComment] already
	// agree on — roughly eight thousand words. Prose longer than that is
	// a DOCUMENT rather than a remark, and the knowledge base carries
	// 512 KiB with a title, a history and a name somebody can find it by.
	MaxBody = 32 << 10

	// MaxTopic bounds a channel's topic.
	//
	// 256 bytes: a line in the room's header, which is what a topic is.
	MaxTopic = 256

	// MaxPurpose bounds a channel's purpose.
	//
	// 600 bytes, the same figure as [MaxExcerpt] and for the same reason
	// — it is a paragraph somebody reads before deciding to join, and a
	// purpose that needs more than a paragraph is describing a project,
	// which has a page.
	MaxPurpose = 600

	// MaxChannelName bounds a channel's name.
	//
	// SIXTY-FOUR BYTES, and it is the same number as the bound inside
	// [namePattern] rather than a second opinion about it: the name is an
	// ADDRESS, so its shape and its length are one rule checked in one
	// place. It has to fit in a sidebar, in a `#mention` inside a
	// sentence, and as the input to [ChannelToken].
	MaxChannelName = 64

	// MaxExcerpt bounds the excerpt a routing snapshot carries.
	//
	// 600 bytes, matching the tracker's and the wiki's, because all three
	// feed the same card and the same wake prompt.
	MaxExcerpt = 600

	// MaxLinks and MaxLinkBytes bound what a message may point at.
	//
	// EIGHT LINKS OF TWO KIBIBYTES. There are no attachments — see the
	// package doc — so a link is the whole of what a message carries
	// besides its own text, and eight of them is a hand-off with its
	// sources rather than a dump. Two kibibytes clears every URL length
	// any of the eight integration surfaces mints, signed query strings
	// included.
	MaxLinks     = 8
	MaxLinkBytes = 2 << 10

	// MaxEmoji bounds one reaction's emoji.
	//
	// SIXTY-FOUR BYTES. A reaction is a shortcode between colons — the
	// longest in the standard set is about thirty characters — and 64
	// leaves room for a skin-tone or variant suffix while keeping a
	// reaction record two orders of magnitude inside [MaxRecordBytes].
	// It is bounded here rather than left to the applier because the
	// value goes onto the log, where an unbounded string is bytes every
	// node stores for the stream's whole retention window.
	MaxEmoji = 64
)

// The collection caps. Every collection a record touches travels WHOLE — a
// delta cannot rebuild a row on a replay from zero — which is affordable only
// because each of these bounds it.
const (
	// MaxMentions bounds the handles one message names.
	//
	// THIRTY-TWO, the tracker's figure for the same collection. It is a
	// routing cap as much as a display one: every mention is a wake.
	MaxMentions = 32

	// MaxThreadParticipants bounds the people a thread carries with it.
	//
	// SIXTEEN, the tracker's, and derived from what a thread IS rather
	// than from what a table holds: past a dozen or so voices a thread is
	// a meeting, and the routing that matters there is the mention.
	//
	// IT BOUNDS A CANDIDATE SET, NOT A FIELD ON ANY RECORD. A reply's
	// participants are resolved into [Notify.Recipients] at write time, so
	// the handles that ride the log are the ones already routed; this is
	// the bound the routing arithmetic reads them under.
	MaxThreadParticipants = 16

	// MaxCollectiveRecipients bounds how many agents one `@channel` wakes.
	//
	// THIRTY-TWO, which is
	// [github.com/crewlet/crewlet/internal/node.DefaultMaxConcurrent]: one
	// collective address must not be able to saturate a node's ENTIRE turn
	// budget, because every other seat on that node then waits behind a
	// broadcast nobody was obliged to answer. Past it the routing keeps
	// the first 32 and sets [Notify.WakesTruncated], so the room can see
	// that it reached fewer people than it named.
	MaxCollectiveRecipients = 32

	// MaxRecipients bounds the whole resolved recipient set on one post.
	//
	// NINETY-SIX: [MaxCollectiveRecipients] plus [MaxThreadParticipants]
	// plus [MaxMentions] plus the one lead fallback, rounded up. It is the
	// sum rather than a number of its own because the arms are disjoint
	// reasons over the same people — a recipient appears once, under its
	// strongest reason — so the sum is a true ceiling and not an estimate.
	MaxRecipients = 96

	// MaxMembers bounds one channel's membership.
	//
	// A THOUSAND, which is a quarter of
	// [github.com/crewlet/crewlet/internal/statelog.ApplyTxRowBudget]. A
	// membership record writes at most one row per member and a RECORD IS
	// NEVER SPLIT across transactions, so the cap is what keeps the widest
	// membership write inside one apply with room for the channel's own
	// rows beside it.
	MaxMembers = 1000

	// MaxChannels bounds how many channels one company has.
	//
	// A THOUSAND, the same figure as [MaxMembers] and for the adjacent
	// reason: a company whose org chart has a room per unit, per project
	// and per pair is nowhere near it, and past it the sidebar is a search
	// problem rather than a list. It binds at the CREATE — nothing in a
	// record can count the company's rooms — so the write path refuses the
	// thousand-and-first naming this constant.
	//
	// IT COUNTS LIVE ROOMS, not every row ever written. There is no channel
	// delete in this vocabulary, so counting archived rooms made this a
	// number a company could reach once and never get back under — with a
	// refusal telling it to archive a room, which did nothing. Archiving is
	// the release, which is what the sidebar argument above was about in the
	// first place.
	MaxChannels = 1000

	// MaxDMParticipants bounds a direct or group conversation.
	//
	// EIGHT, which is
	// [github.com/crewlet/crewlet/internal/tracker.MaxCollaborators], and
	// the argument carries over exactly: past eight people a conversation
	// is a room, and a room has a name, a purpose and a membership somebody
	// can join. A derived id over more than eight handles is also a key
	// nobody can reconstruct by hand when it goes wrong.
	MaxDMParticipants = 8

	// MaxReactionEmoji bounds the DISTINCT emoji on one message.
	//
	// THIRTY-TWO. Past it a message's reactions are longer than the
	// message, and every one is a row replicated to every node.
	//
	// THE APPLIER ENFORCES THIS ONE, not a Validate here, and that is
	// forced by the record's shape: a [Reaction] is a single toggle so
	// that two people reacting never overwrite each other, and a toggle
	// cannot see the set it is joining. The applier can — it holds the
	// message's rows in its own transaction — so it declines the
	// thirty-third distinct emoji deterministically, reaching the same
	// answer on every node.
	MaxReactionEmoji = 32

	// MaxEraseMessages bounds one erase gesture.
	//
	// TWO HUNDRED AND FIFTY. An erased message costs about four rows — the
	// message, its reactions, its mentions, its thread membership — so 250
	// is a thousand rows, a quarter of
	// [github.com/crewlet/crewlet/internal/statelog.ApplyTxRowBudget], and
	// a record is never split across transactions. An operator erasing
	// more than that issues more than one gesture, each of which is its own
	// auditable record.
	MaxEraseMessages = 250

	// MaxThreadContext bounds how many earlier lines of a thread a
	// routing snapshot carries.
	//
	// FIFTY, which is `config.MaxThreadContextMessages` — the largest
	// number a company may ask for. The record carries what the company
	// ASKED for rather than a fixed slice, because the count is founder
	// policy read once at the edge; this is the ceiling that holds
	// whatever it asked, so a config change can never widen a record past
	// what [MaxRecordBytes] was sized for.
	MaxThreadContext = 50

	// MaxThreadContextBytes bounds those lines TOGETHER, and it is the cap
	// that actually decides the record's size.
	//
	// FOUR KIBIBYTES. A line count alone cannot bound bytes — fifty lines
	// at [MaxExcerpt] would be 30 KiB on top of a snapshot already sized
	// at 54 — so the lines are taken NEWEST FIRST and stop when this
	// budget is spent, which is the order that keeps the closest context
	// when a thread is long and verbose. Four is about a sixth of the
	// conversation ledger's own injected budget, which is the other
	// structured block a turn's prompt carries, and it is what keeps a
	// thread from displacing what the seat already knows.
	MaxThreadContextBytes = 4 << 10
)

// MaxRecordBytes is the design maximum for one encoded record, envelope
// included.
//
// SIXTY-FOUR KIBIBYTES, which is sixteen times inside an external NATS
// cluster's 1 MiB `max_payload` default — the broker a fleet dials when it is
// not running the embedded one, and the only ceiling in the path this package
// does not own. The worst record this build can write is a post at every cap
// at once: [MaxBody] plus [MaxLinks] times [MaxLinkBytes] plus a routing
// snapshot at [MaxRecipients] and a thread at [MaxThreadContextBytes], which
// sums to about 58 KiB and leaves the rest for JSON escaping of text that is
// not ASCII.
const MaxRecordBytes = 64 << 10

// ChatMessagesPerDay is the declared census this domain is sized against.
//
// TWENTY THOUSAND: three thousand turns a day, two messages each, times about
// 3.3 for the talk that wakes nobody. It is not a limit anything enforces —
// it is the INPUT every capacity number downstream is derived from, and the
// supported ceiling is 50,000, so a deployment reading this knows which
// figure a stream size or a retention window was computed against.
const ChatMessagesPerDay = 20_000

// The retention settings a channel carries.
//
// A DAY COUNT RATHER THAN A DURATION, because it is a number a person sets in
// a form and reads back in a screen, and because the prune it drives is a
// RECORD carrying a cutoff instant rather than a local sweep: every node must
// delete exactly the same rows at exactly the same position, which a duration
// each node evaluated against its own clock could never guarantee.
const (
	// DefaultRetentionDays is how long a channel keeps its messages when
	// nobody says otherwise.
	//
	// THREE HUNDRED AND SIXTY-FIVE. A year is what a company looks back
	// over — the last planning cycle, the last incident of this kind —
	// and at [ChatMessagesPerDay] it is the figure the log's own ceiling
	// was sized from.
	DefaultRetentionDays = 365

	// RetentionForever is the override that never prunes.
	//
	// ZERO IS A VALID SETTING HERE, which is why every field carrying it
	// is a POINTER: an absent override means "the company default" and a
	// present zero means "keep this room for ever", and a plain int
	// collapses the two into the reading that silently deletes a year of
	// somebody's decisions.
	RetentionForever = 0
)

// Kind is what a channel is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of, so an unknown value off the wire is a VALUE rather than a panic.
type Kind string

// The five kinds. Two axes cross here — who may read a room, and whether the
// room has an address — and the constants are the five combinations that
// exist rather than the four a product of the axes would give.
const (
	// KindPublic is a room anybody in the company may read and join.
	KindPublic Kind = "public"

	// KindPrivate is a room whose membership is the only way in.
	KindPrivate Kind = "private"

	// KindUnit is a unit's own room, and the one kind the ENGINE fills:
	// the unit's agent seats are members with the follow-all flag set, so
	// the room is where work addressed to nobody in particular lands.
	//
	// It is also the only kind a [ReasonLeadFallback] can arise in — a
	// person who posts to a unit and names nobody is addressing whoever
	// leads it, and a room where that resolved to silence would be a
	// person talking to an empty screen.
	KindUnit Kind = "unit"

	// KindDM is a conversation between exactly two parties.
	KindDM Kind = "dm"

	// KindGroup is a conversation between three to [MaxDMParticipants]
	// parties, and it is a separate kind from a private channel although
	// both are closed: a group has NO NAME, so it has no address to
	// arbitrate on and its identity is derived from who is in it. That
	// difference decides which subject its create is published on — see
	// [Kind.Direct].
	KindGroup Kind = "group"
)

// Kinds is every kind.
func Kinds() []Kind { return []Kind{KindPublic, KindPrivate, KindUnit, KindDM, KindGroup} }

// Valid reports whether k is a kind this build serves.
func (k Kind) Valid() bool { return slices.Contains(Kinds(), k) }

// Direct reports a channel whose identity is DERIVED from its participants
// rather than claimed as a name.
//
// TWO, AND IT IS A CLOSED SET. The distinction is not cosmetic: a named
// channel's create arbitrates on its NAME, so two founders typing `#launch`
// contend at the broker and one of them is told it already exists — while a
// direct conversation has no name to contend for, and two seats opening the
// same DM from two nodes must converge on ONE room rather than be told to
// pick another. [DirectChannelID] is what makes that true, and this is the
// predicate that decides which discipline a create takes.
//
// A CLOSED SET RATHER THAN A NEGATIVE ONE, so a kind added later is named
// rather than silently derived: a kind wrongly treated as direct would have
// its create published on a subject nothing arbitrates a name against, and
// two rooms with one name is not a state this alphabet can express.
func (k Kind) Direct() bool {
	switch k {
	case KindDM, KindGroup:
		return true
	}
	return false
}

// Named reports a channel that holds an address — the complement of
// [Kind.Direct] over the kinds this build knows.
//
// AN UNKNOWN KIND IS NEITHER. It is not named and it is not direct, because
// both answers are claims about how its create arbitrates and this build has
// no basis for either.
func (k Kind) Named() bool { return k.Valid() && !k.Direct() }

// AuthorKind is who wrote something.
//
// The tracker's and the wiki's three, plus one this domain needs that neither
// of them does.
type AuthorKind string

// The author kinds.
const (
	AuthorAgent    AuthorKind = "agent"
	AuthorHuman    AuthorKind = "human"
	AuthorOperator AuthorKind = "operator"

	// AuthorSystem is the ENGINE narrating the room: somebody joined, the
	// room was archived, a retention prune removed a year of it.
	//
	// ITS OWN KIND rather than an operator or a seat, because attributing
	// a narration to either makes the engine a PARTICIPANT in a
	// conversation it only described — a seat that never spoke would
	// appear to have spoken, and an operator token that authorised nothing
	// would carry the line. It is also what a reader needs in order to
	// render a system line differently from a remark, and what the routing
	// reads to wake nobody for it.
	AuthorSystem AuthorKind = "system"
)

// AuthorKinds is every kind.
func AuthorKinds() []AuthorKind {
	return []AuthorKind{AuthorAgent, AuthorHuman, AuthorOperator, AuthorSystem}
}

// Valid reports whether k is a kind this build serves.
func (k AuthorKind) Valid() bool { return slices.Contains(AuthorKinds(), k) }

// NormalizeName is the canonical form of a channel name, for comparison and
// for the address it is claimed under.
//
// LOWER-CASED AND TRIMMED, because a name is an ADDRESS: a person typing
// `#Launch` and one typing `#launch ` mean the same room, and a company
// holding both is one where every `#mention` is a coin flip. Unlike a page
// title there is no whitespace folding, because a legal name has no interior
// whitespace at all — see [namePattern].
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// namePattern is the shape of a channel name: lower-case alphanumerics and
// hyphens, starting with an alphanumeric, at most [MaxChannelName] bytes.
//
// # Why this is not github.com/crewlet/crewlet/internal/org.ValidHandle
//
// The two patterns look alike — org's is `^[a-z0-9][a-z0-9-]*$` — and chat
// deliberately does not delegate to it, for two reasons that both bite later:
//
//   - IT CARRIES NO LENGTH BOUND, and it does not need one: a seat handle is
//     slugified from a role name the config already bounds. A channel name is
//     typed by a person, goes into a broker subject token and a claim key, and
//     is bounded here so that the shape and the length are ONE rule checked in
//     ONE place rather than a regex and a forgotten `len` at each caller.
//   - THEY ARE DIFFERENT NAMESPACES WITH DIFFERENT OWNERS. Widening the seat
//     handle to admit an underscore is a decision about identity derivation —
//     it re-derives every agent id — and it must not silently widen what a
//     channel may be called, nor the reverse. Sharing the regex would make
//     each change a change to both.
//
// The bound and [MaxChannelName] must agree; a test holds them together.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName reports whether s is a legal channel name in its NORMALISED form.
//
// It is deliberately strict about what a name may contain rather than
// escaping what it does: the name is a claim key, half of a scope path and
// the input to a broker subject token, and a character that is legal in one of
// those and not the others is the bug an escaping alphabet exists to hide.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// ErrInvalid reports a value this chat refuses.
var ErrInvalid = fmt.Errorf("chat: invalid")

// invalid builds a refusal naming the field and what to do about it.
func invalid(field, why string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(why, args...))
}
