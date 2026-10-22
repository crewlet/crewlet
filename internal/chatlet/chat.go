// Package chatlet is Chatlet, the company's own conversation: channels, the
// messages in them, and the routing snapshot a wake is derived from.
//
// # What it is for
//
// A company running Crewlet talks to itself, and until now that talking had
// to happen in somebody else's workspace — Mattermost or Slack — which meant
// a company that configures neither has no way for a person to say anything
// to a seat at all. This is the first-party alternative, on exactly the terms
// [github.com/crewlet/crewlet/internal/worklet] is Worklet's and
// [github.com/crewlet/crewlet/internal/doclet] is Doclet's: every change is
// ONE RECORD on an ordered stream, arbitrated at the broker on the subject of
// the object it changes and applied into N identical SQL copies with the
// checkpoint in the same transaction as the rows.
//
// That shape is ADR-0002 and [github.com/crewlet/crewlet/internal/statelog] is
// the record's authority. It is the framework's NINTH domain, and what is
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
// MESSAGES ARE SEQUENCED ([statelog.PatternSequenced]). A post carries NO
// expectation at all: two people talking in one room are not racing for
// anything, and paying for arbitration would serialise the single hottest
// subject in the company behind itself at the declared census of
// [ChatMessagesPerDay] — and a rejection there would be a message somebody
// typed and lost, which is the one failure a chat system may not have. But
// the records do NOT all commute: a post into a room a record ahead of it
// archived, an edit of a message a delete ahead of it took back, a reaction
// that would be the thirty-third emoji — each depends on what came before it,
// which the writer's snapshot cannot hold. So the log orders them and the
// APPLIER decides each at its position, writing a [Verdict] the writer reads
// back before it answers (verdict.go).
//
// # Why a message record is scoped to its CHANNEL and never to itself
//
// ADR-0046 is the record of this decision and of what the obvious alternative
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
// A message carries up to [MaxLinks] links of [MaxLinkBytes], NO FILE BYTES
// and NO OBJECT. Bytes on the log would be replicated to every node, held for
// the stream's whole retention window and replayed from zero by a node joining
// the fleet — a file every member stores for ever. The company's files have a
// home that is built for them instead: the object store (ADR-0026), where a
// file has a path somebody can find it by, an owner, and a collector that
// deletes its bytes once no row names them. A message LINKS to one.
//
// A message that named an object itself would be a second owner of that
// file's lifetime: a reference the collector must honour, which the retention
// prune and the compliance erase would then have to release — on every node,
// deterministically, inside the apply — or leave a file nobody can reach
// pinned for ever. This build has none of that, so a message names no object
// and no chat table is among the ones `internal/objstore/references` declares.
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
package chatlet

import (
	"errors"
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
	// [github.com/crewlet/crewlet/internal/worklet.MaxCommentBody] and
	// [github.com/crewlet/crewlet/internal/doclet.MaxComment] already
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
	// feed the same card. Not the wake prompt: a woken seat reads the
	// message whole from the record (see the parser's wakeBody).
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

	// MaxEraseReason bounds the reason an erase records.
	//
	// 600 BYTES, the same figure as [MaxPurpose] and for its reason: why
	// the company destroyed something is a sentence or two — "a credential
	// was pasted here" — and an account longer than a paragraph belongs to
	// the incident it is part of. It is bounded at all because the reason
	// rides a gate-installing record every node holds for the log's whole
	// retention window, and an unbounded one is the one field that could
	// carry an erase past the record's own bound ([MaxRecordBytes]).
	MaxEraseReason = 600
)

// The collection caps. Every collection a record touches travels WHOLE — a
// delta cannot rebuild a row on a replay from zero — which is affordable only
// because each of these bounds it.
const (
	// MaxMentions bounds the handles one message names.
	//
	// THIRTY-TWO, the tracker's figure for the same collection. It is a
	// routing cap as much as a display one: every mention is a wake, and
	// [MaxCollectiveRecipients] is this same figure, so moving it moves how
	// many agents one `@channel` wakes too.
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
	// [MaxMentions], THIRTY-TWO: a collective address wakes no more agents
	// than one message may name, because a name and `@channel` are the
	// same wake. More would let the gesture nobody is obliged to answer
	// start more runs than naming each seat could; fewer would only make
	// the writer name the seats it left out, to wake them all the same.
	// ONE CONSTANT, not a second figure, so the two cannot drift apart.
	//
	// A WAKE IS ITS SEAT'S CODING RUN IN A BOX (ADR-0066): its first request
	// carries the CLI's own harness (about 15k input tokens for Claude
	// Code) even where the seat decides the message was not for it, and
	// its box is a 200 to 400 MB process on the engine host for a `direct`
	// cell, or one of the sandboxes an E2B account may run at once. So this
	// is also how many boxes one broadcast starts, and nothing but this cap
	// and the seats it reaches, each running one at a time, bounds them:
	// NOT a node's turn budget
	// ([github.com/crewlet/crewlet/internal/node.DefaultMaxConcurrent],
	// the same figure today), which a wake holds only while it launches
	// and while its run is collected and reviewed. Past the cap the
	// routing keeps the first 32 and sets [Notify.WakesTruncated], so the
	// room can see that it reached fewer people than it named.
	MaxCollectiveRecipients = MaxMentions

	// MaxRecipients bounds the whole resolved recipient set on one post.
	//
	// IT IS A CAP THAT CUTS, NOT A CEILING DERIVED FROM THE ARMS. Three arms
	// carry limits of their own — [MaxMentions], [MaxThreadParticipants]
	// and [MaxCollectiveRecipients] — and together with the one lead
	// fallback they take at most 81. The others carry none: a direct
	// conversation's participants (at most [MaxDMParticipants], by the
	// room's own cap), a thread's followers and a room's followers of
	// everything. Those are cut HERE, and the cut sets
	// [Notify.WakesTruncated], so a room can see that a message reached
	// fewer people than follow it.
	//
	// NINETY-SIX, the smallest multiple of [MaxCollectiveRecipients] above
	// the 81 the capped arms can fill: a post that fills every capped arm
	// at once is never cut by the total, so the total bites only on the
	// arms nothing else bounds. Every recipient is a wake, a coding run in
	// a box, so this is also the most boxes one post starts, and never
	// more than the seats it reaches would run at once for any other
	// cause, since each runs one at a time. It is also a line of the
	// routing snapshot each, which at this figure still fits
	// [MaxRecordBytes] beside a body at every cap.
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
	// [github.com/crewlet/crewlet/internal/worklet.MaxCollaborators], and
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
	// answer on every node. FROM THE RECORD'S STAMP ([Reaction.Cap]),
	// which the writer sets to this, never from the applying node's own
	// copy of it: a later build that moves the number would otherwise
	// decide one record two ways.
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
)

// MaxRecordBytes is the largest record this domain publishes, envelope
// included ([statelog.StreamSpec.MaxRecordBytes]) — the bound the framework
// refuses a larger one at, `record_too_large`, before anything is sent, and
// what the stream's own message limit and its gate reserve are sized from.
//
// HALF A MEBIBYTE, sized from the worst record the caps admit rather than
// from a typical one. A record is JSON, and every `<`, `>`, `&`, control
// character or invalid byte in a value becomes a six-byte escape, so a post at
// every cap at once — [MaxBody] of such text, [MaxLinks] of [MaxLinkBytes]
// full of `&`, and a routing snapshot at [MaxRecipients] of the widest handles
// a seat may have — encodes to about 305 KiB, an edit to the same, and a
// unit room's create or membership at [MaxMembers] to about 87 KiB. A bound
// any of those crosses refuses a message somebody typed — a pasted block of
// markup is ordinary — or a membership the org chart can never write, and the
// record tests hold every op at every cap under this one. The next power of
// two above the worst is the bound: half of an external NATS cluster's 1 MiB
// `max_payload` default, the one ceiling in the path this package does not
// own, and a gate reserve whose fleet term — seven peers' appends of it and a
// mebibyte — is under five mebibytes, far inside a sixteenth of the
// gibibyte the field's floor allows ([statelog.Reservation]). The thread a
// message is part of is not on it: a woken seat reads that at turn start
// ([ThreadReader]).
const MaxRecordBytes = 512 << 10

// ChatMessagesPerDay is the declared census this domain is sized against.
//
// TWENTY THOUSAND: three thousand turns a day, two messages each, times about
// 3.3 for the talk that wakes nobody. It is not a limit anything enforces, and
// NOTHING COMPUTES FROM IT either: it is the figure every capacity statement
// about this domain is MADE AGAINST — the log's growth a day, how long a
// blocked trim takes to fill [ChatLogMaxBytes], how many rows a prune removes,
// how many barriers a seat's chat reads append — so that a deployment reading
// one of those numbers knows which load it was stated for, and that the
// supported ceiling is 50,000. Each of those figures carries its own
// arithmetic beside it; this is the input that arithmetic names.
const ChatMessagesPerDay = 20_000

// The retention settings a channel carries.
//
// A DAY COUNT RATHER THAN A DURATION, because it is a number a person sets in
// a form and reads back in a screen, and because the prune it drives is a
// RECORD carrying a cutoff instant rather than a local sweep: every node must
// delete exactly the same rows at exactly the same position, which a duration
// each node evaluated against its own clock could never guarantee.
//
// THE COMPANY'S DEFAULT IS NOT HERE: it is
// [github.com/crewlet/crewlet/internal/config.DefaultMessageRetentionDays],
// the field a company document reads, and a second spelling in this package
// would be a number the two could disagree on.
const (
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
// FOUR, THE IDENTITY DIRECTORY'S OWN — the words `iam.ActorFor` records every
// principal under, so one party is one kind in this domain's records and in
// the tracker's and the wiki's, which the audit feed reads side by side. A
// person writes as the human seat they hold (ADR-0032); a credential acting as
// itself is an operator, which speaks in no room ([Actor.speaks]) but erases,
// prunes and sets a room's retention under its own name; and the engine's own
// narration is the kind neither the tracker's people nor the wiki's need.
type AuthorKind string

// The author kinds.
const (
	// AuthorAgent is a seat's own turn.
	AuthorAgent AuthorKind = "agent"

	// AuthorHuman is a PERSON, writing as the `kind: human` seat they hold,
	// with the credential they acted through recorded beside it.
	AuthorHuman AuthorKind = "human"

	// AuthorOperator is a credential acting as ITSELF — a pipeline, an
	// automation, a token bound to no seat — under its own login. It says
	// nothing in a room: a room renders a speaker by seat. What it may do
	// is what is not said in a room's voice: a compliance erase, a prune, a
	// room's retention, and a node's eviction from this log.
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

// namePattern is the shape of a channel name: lower-case alphanumerics,
// hyphens and underscores, starting with an alphanumeric, at most
// [MaxChannelName] bytes.
//
// THE UNDERSCORE IS ADMITTED because a unit's channel is one name for every
// chat backend ([github.com/crewlet/crewlet/internal/config]'s unit channel
// grammar is this one), and Slack and Mattermost both admit it — a vendor
// company whose real channel is `dev_ops` must be able to name it. Nothing
// here is put at risk by it: a name reaches a broker subject only as a digest
// ([ChannelToken]), never as itself, so `_` is no more a token character than
// `-` is.
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
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether s is a legal channel name in its NORMALISED form.
//
// It is deliberately strict about what a name may contain rather than
// escaping what it does: the name is a claim key, half of a scope path and
// the input to a broker subject token, and a character that is legal in one of
// those and not the others is the bug an escaping alphabet exists to hide.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// ErrInvalid reports a value this chat refuses.
var ErrInvalid = fmt.Errorf("chatlet: invalid")

// InvalidError is a refusal of one field, keeping the field and the reason
// apart so [Sentence] can say them without the Go prefix.
//
// EXPORTED, FIELDS AND ALL, for [doclet.InvalidError]'s reason: a refusal
// crosses the estate router's wire whenever the node that decided it is not
// the node whose caller is told, the wire rebuilds an error type from its
// exported fields, and an unexported one arrived as its message alone. Only
// this package builds one.
//
// ITS MESSAGE IS THE ONE A FORMATTED REFUSAL HAD, byte for byte: every log
// line and every test that reads one reads the same words, and only a caller
// that asks for [Sentence] gets the other shape.
type InvalidError struct {
	// Field is the argument refused, as the caller named it.
	Field string
	// Why is what is wrong with it and what to do, without the package
	// prefix.
	Why string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("%v: %s: %s", ErrInvalid, e.Field, e.Why)
}

func (e *InvalidError) Unwrap() error { return ErrInvalid }

// invalid builds a refusal naming the field and what to do about it.
func invalid(field, why string, args ...any) error {
	return &InvalidError{Field: field, Why: fmt.Sprintf(why, args...)}
}

// Sentence is a refusal this package wrote, as its reader is told it: the field
// it names and what to do, or the rule that forbade it — without the "chatlet:
// invalid:" or "chatlet: that is not this actor's to do:" a Go error opens with,
// which says where the error came from and nothing about what to change. Any
// other error, or a refusal a caller wrapped in context of its own, is its
// whole message, because context is never dropped to tidy a prefix away.
//
// # Who reads these
//
// A model, in the chat tools' answers: a seat's, a person's own assistant's
// over the operator's MCP surface, and the Dashboard Assistant's, told in
// process. Chat's refusals reached it as "chatlet: invalid: body: …", a
// package path where an instruction belongs. And a person whose chat QUESTION
// was refused for what it asked: the read answers its `invalid` as the detail
// the screen draws (internal/api/queries). A person whose chat gesture the
// dashboard sends is answered the refusal's class in its code's own words,
// never a tool's sentence, until the refusal catalog words it for them
// (ADR-0060).
//
// THE EIGHT IT RECOGNISES ARE THE EIGHT WRITTEN AS SENTENCES: a field refused
// ([ErrInvalid]), a gesture this actor may not make ([ErrForbidden]), a room
// that takes nothing more ([ErrArchived]) and a message that carries all the
// emoji it may ([ErrReactionsFull]), each a state the sentence says how to
// change, a room this node cannot serve until it is upgraded
// ([ErrUnknownKind]), a room, a message or a thread that is not there
// ([ErrNotFound]), a name another room holds ([ErrNameTaken]) and a room that
// changed under a decision ([ErrConflict]). Each names a room as a person
// knows it ([Channel.called]) and never by its id. A lost race carries the
// framework's own error after the mark, and is told to a person in the state
// log's words before this is asked (internal/agent/builtin's WriterSentence).
func Sentence(err error) string {
	var refusal *InvalidError
	if errors.As(err, &refusal) && refusal.Error() == err.Error() {
		return "`" + refusal.Field + "`: " + refusal.Why
	}
	for _, mark := range []error{ErrForbidden, ErrArchived, ErrReactionsFull, ErrUnknownKind,
		ErrNotFound, ErrNameTaken, ErrConflict} {
		if !errors.Is(err, mark) {
			continue
		}
		// THE PREFIX IS THE MARK'S OWN TEXT, which is exactly what a
		// refusal built as "%w: <sentence>" opens with — and exactly what
		// it does not open with once a caller wrapped it in context.
		if rest, ok := strings.CutPrefix(err.Error(), mark.Error()+": "); ok {
			return rest
		}
	}
	// ANY OTHER ERROR THIS PACKAGE WROTE loses the "chatlet:" it opens with and
	// keeps every word after it; one a caller wrapped in context of its own
	// is its whole message.
	return strings.TrimPrefix(err.Error(), "chatlet: ")
}
