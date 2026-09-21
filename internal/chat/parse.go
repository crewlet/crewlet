package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// THE METADATA THIS DOMAIN ADDS, on top of the shared chat vocabulary.
//
// The shared keys are [github.com/crewlet/crewlet/internal/notify.ChatKeys] —
// which backend, which surface, which message, which conversation, who, and
// why it reached this seat — and they are spelled through notify's own
// constants, never as literals here, so a rename is a compile error rather
// than a silence.
//
// The three below are facts no vendor has and therefore facts that vocabulary
// cannot hold. EVERY ONE HAS A PRODUCER AND A READER: the parser stamps them
// and [Prompt] reads them back. A key nobody reads is work that looks done,
// which is exactly how `routed_via` was going to end up here before the
// address rule learned to carry this domain's own reasons.
const (
	// MetaAddressed is the ROUTING'S OWN VERDICT on whether this wake
	// obliges an answer.
	//
	// STAMPED ONCE, HERE, by the only party that holds the record and the
	// recipient in one hand — a wake is per-recipient, and one message
	// legitimately addresses one seat and merely informs another. The
	// delivery gate reads it through [Prompt.Addressed], the working
	// indicator evaluates [AddressRule] over this same metadata, and the
	// two are therefore one evaluation read twice rather than two
	// evaluations of one rule. A prompt that re-derived it from the
	// reason would be a third opinion about a question the routing
	// already answered.
	MetaAddressed = "chat_addressed"

	// MetaAuthorKind is [AuthorKind]: who wrote the message.
	//
	// Read by the prompt, because the answer changes what the seat can do
	// next. An operator and the engine's own narration are not seats —
	// neither resolves through [notify.Parties] at all — and a human
	// replies on their own time and cannot be reached by an
	// agent-to-agent ask.
	MetaAuthorKind = "chat_author_kind"

	// MetaTruncated marks a collective address that reached fewer seats
	// than the room has, because it exceeded [MaxCollectiveRecipients].
	//
	// It is on the RECORD ([Notify.WakesTruncated]) and it is rendered,
	// because a room that cannot see it reads a partial broadcast as a
	// complete one: the seats that WERE woken then assume the rest heard
	// it too, and nobody says the thing again.
	MetaTruncated = "chat_wakes_truncated"

	// MetaThread is the earlier lines of this message's thread, already
	// rendered, one line per message and oldest first.
	//
	// RENDERED HERE rather than carried as structure, for two reasons.
	// Metadata is a string map, so a list would have to be encoded
	// anyway; and this is the frame that decoded the record, so it is the
	// last one that can tell a thread that was EMPTY from one this build
	// could not read. A prompt handed a blank key cannot.
	//
	// It is present only where there is a thread: a root post carries no
	// key at all, rather than an empty one.
	MetaThread = "chat_thread"
)

// AddressRule is this domain's answer to "was this message addressed to this
// seat?", and it is ONE DECLARED VALUE for the reason
// [notify.AddressRule] exists: the prompt that tells an agent it was asked,
// the delivery gate that refuses to let an addressed turn end in silence, and
// the working indicator that tells a person the agent is working must not be
// three opinions. A spinner over a turn that is allowed to end in silence,
// and a silence after a spinner, are the same bug.
//
// BOTH HALVES ARE DERIVED FROM THE VOCABULARY rather than typed again:
//
//   - The direct kinds are [Kind.Direct]'s, which is what a conversation with
//     no room around it means here. They are read by the conversation key
//     ([notify.ChatPrompt.ConversationKey]), so a person's consecutive
//     top-level messages in a DM coalesce into one turn.
//   - The follows are the reasons [Reason.Addressed] returns true for. The
//     rule carries them rather than inheriting notify's own four precisely
//     because this domain routes on reasons that package has never heard of —
//     `reply_to_own_root` and `lead_fallback` are not follows anywhere else —
//     and a rule that fell back to the vendor set would read every one of
//     them as "does not address".
//
// NO DMPrefix. A channel id here is a uuid, so a prefix test would mark
// arbitrary rooms as direct messages and oblige every seat in them to answer
// traffic nobody addressed to any of them.
//
// A FUNCTION RATHER THAN A PACKAGE VAR, because the value holds two slices
// and a var would hand every caller the same backing arrays to append to.
//
// SPELLED AS THE VENDORS SPELL IT — `slack.AddressRule`,
// `mattermost.AddressRule` — because the working indicator reaches for it
// through a backend's own StatusPoster: a third name for one concept is how
// a native poster ends up declaring a second rule of its own.
func AddressRule() notify.AddressRule {
	direct := make([]string, 0, len(Kinds()))
	for _, k := range Kinds() {
		if k.Direct() {
			direct = append(direct, string(k))
		}
	}
	follows := make([]notify.FollowReason, 0, len(Reasons))
	for _, r := range Reasons {
		if r.Addressed() {
			follows = append(follows, notify.FollowReason(r))
		}
	}
	return notify.AddressRule{DirectKinds: direct, Follows: follows}
}

// Parser turns one committed chat record into the wakes it implies.
//
// # It holds nothing, and that is the whole of its correctness
//
// Not the roster — that arrives per call, because a parser outlives the epoch
// a wake was written under. Not the leads: the lead a [ReasonLeadFallback]
// resolves to RIDES THE RECORD, resolved at write time inside the decide,
// because who leads a unit is a fact about the epoch and a parser resolving it
// later would route a message posted under one org chart to whoever leads
// under the next one. And not a thread, a membership or a room: the record's
// [Notify] is the snapshot the routing was decided from, which is what lets
// this run on a node whose applier has not reached the message.
type Parser struct{}

// NewParser builds native chat's inbound parser.
//
// A CONSTRUCTOR although there is nothing to configure, so the wiring reads
// like every other source's and so a later field is not a signature change
// across the engine.
func NewParser() *Parser { return &Parser{} }

var _ notify.Parser = (*Parser)(nil)

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Source }

// Parse reports which seats a committed chat record wakes.
//
// # It re-runs the routing rather than reading the record's recipient list
//
// [Candidates] is PURE over the record and [Route] is the impure half that
// needs the two facts no record can carry — who the company employs RIGHT NOW
// and who wrote the message. They are the SAME two functions the write path's
// decide called, which is the point: the applier and the parser cannot
// disagree about who a message concerned, because there is one arithmetic and
// neither of them has a copy of it.
//
// Running them again here is not redundant. The set on the record was routed
// against the roster of the instant it was written; this one is routed against
// the roster the company has now, so a seat that has since been removed is
// dropped and the lead the record named is offered again. And a record from a
// NEWER BUILD may name more seats than this build's caps allow — a wake set is
// a TURN BUDGET rather than a row, and spending this node's whole concurrency
// budget on a record that named two hundred seats is not something an older
// build has to agree to.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) (
	[]notify.Routed, error) {

	record, err := recordFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	n := record.Notify
	if n == nil {
		// A RECORD NOBODY ANNOUNCED reaching the parser at all means the
		// feed's own decision was bypassed — a replayed body, a test, a
		// second producer. Answering "nobody" rather than erroring keeps
		// the two layers independent: the feed decides what to relay and
		// this decides who it is for.
		return nil, nil
	}
	if !record.Subject.Kind.Routable() {
		// A ROUTING SNAPSHOT ON A KIND NOBODY WATCHES. A newer build
		// could write one; this build has no card to render for it and
		// no rule for what it asks of a seat, so it wakes nobody rather
		// than waking everybody the snapshot named.
		return nil, nil
	}
	if n.MessageID == "" || n.ChannelID == "" {
		// [Notify.Validate] refuses both at the publish boundary, so a
		// record without them was written by something that did not go
		// through it. There is nothing for a recipient to open and
		// nothing that says where it was said.
		log.DebugContext(ctx, "chat_wake_names_no_message", "record", record.OpID)
		return nil, nil
	}

	// THE AUTHOR OFF THE SNAPSHOT, not the record's writer field: the
	// decide formed both from one [Actor], and routing from the same
	// snapshot the recipients were resolved in is what keeps this a
	// function of one value.
	routed := Route(Candidates(n), rosterOf(reg), n.Author)
	if len(routed) == 0 {
		return nil, nil
	}

	base := p.inbound(record)
	// THE RULE IS BUILT ONCE for the whole record: every recipient's
	// verdict is the same rule read against that recipient's own reason,
	// and rebuilding it per copy would allocate the vocabulary's two
	// derived sets on the hottest path this domain has.
	rule := AddressRule()
	out := make([]notify.Routed, 0, len(routed))
	for _, c := range routed {
		out = append(out, notify.Routed{
			Inbound: withReason(base, rule, c.Reason, n.ThreadRoot),
			// THE HANDLE DIRECTLY, which is what a first-party source
			// has and a vendor does not: a native sender IS a seat's
			// own identity, so there is no external id to scope and
			// no namespace to resolve it through.
			To: notify.Recipient{Handle: c.Handle},
			// DERIVED PER RECIPIENT, so a redelivery is recognisable
			// as one. The feed's claim is the first dedupe layer and
			// it FAILS OPEN — a coordination store that cannot be
			// reached must not silently stop notifications — so this
			// is what catches what slips through. Per recipient
			// because one message legitimately makes several wakes,
			// and one id for all of them would deliver the first and
			// deduplicate the rest away.
			WakeID: changefeed.WakeID(record.OpID, c.Handle),
		})
	}
	return out, nil
}

// rosterOf narrows the registry to the two facts [Route] needs.
//
// A NIL REGISTRY YIELDS A NIL PREDICATE, and [Route] routes nobody for one.
// That is the honest answer rather than a missing filter: without the registry
// this cannot tell a seat the company still has from one it does not, nor a
// person from an agent, and both answers are required — waking everybody the
// record named would run a turn on behalf of a human.
func rosterOf(reg *notify.Registry) func(string) (bool, bool) {
	if reg == nil {
		return nil
	}
	return func(handle string) (exists, human bool) {
		party, ok := reg.ByHandle(handle)
		return ok, party.Human
	}
}

// inbound is the delivery every recipient of this record shares, before the
// per-recipient reason is stamped on it.
func (p *Parser) inbound(record MutationRecord) notify.Inbound {
	n := record.Notify
	meta := map[string]string{
		notify.TransportField: Source,
		notify.ChannelField:   n.ChannelID,
		// THIS DOMAIN'S OWN WORD for the surface, read only against the
		// rule this domain declared ([AddressRule]) — the key is shared,
		// the value is never read by anybody else's rule.
		notify.ChannelTypeField: string(n.ChannelKind),
		// The canonical shape beside it, for the consumers that must
		// compare across backends. The mapping belongs here, in the
		// only code that knows what a unit room is.
		notify.ChannelKindField: string(canonicalKind(n.ChannelKind)),
		notify.MessageIDField:   n.MessageID,
		// EMPTY ON A TOP-LEVEL MESSAGE, and stamped anyway: an absent
		// key and an empty one are the same to every reader, and the
		// whole follow model turns on that emptiness meaning "this
		// started nothing yet".
		notify.ThreadField: n.ThreadRoot,
		// A NATIVE SENDER IS A HANDLE. Both keys carry it: the learning
		// subsystem resolves a counterparty from the sender key and the
		// self-action guard reads the actor key, and here they are one
		// value rather than an id and a name for it.
		notify.UserField:  n.Author,
		notify.ActorField: n.Author,
		MetaAuthorKind:    string(n.AuthorKind),
		// Per recipient, filled by withReason. Present here so a
		// delivery that somehow reaches a reader before the reason is
		// stamped reads as "this message triggered nothing" rather
		// than as a key nobody wrote.
		notify.FollowReasonField: "",
	}
	// Where a reply GOES, which is not the same question as which thread
	// a message is IN — [notify.Anchor] is the one derivation and the
	// producer states its answer, because a top-level message BECOMES the
	// thread the moment anybody answers under it.
	meta[notify.ThreadAnchorField] = notify.Anchor(meta)
	if n.ChannelName != "" {
		// A direct conversation has no name, which is why this is
		// conditional: "eng" is what a person would say and the uuid is
		// not something an agent can place, but an empty name rendered
		// beside an id is worse than the id alone.
		meta[notify.ChannelNameField] = n.ChannelName
	}
	if n.WakesTruncated {
		meta[MetaTruncated] = strconv.FormatBool(true)
	}
	if rendered := renderThread(n.ThreadContext); rendered != "" {
		meta[MetaThread] = rendered
	}
	return notify.Inbound{
		Source: Source,
		// THE RECORD'S OWN WORD for what happened — `post` or `edit` —
		// rather than a change vocabulary of this parser's. It is the
		// value the event store's source-event-type column carries, and
		// a second spelling would make a dashboard filter over this
		// domain's own log disagree with the log.
		EventType: string(record.Op),
		Sender:    n.Author,
		Subject:   roomLabel(n),
		Body:      n.Excerpt,
		Metadata:  meta,
	}
}

// withReason is one recipient's copy: the shared delivery plus why it reached
// THEM, and the verdict that follows from it.
//
// A COPY OF THE MAP, never the shared one mutated in place: the deliveries are
// built in a loop and handed to the spine, which clones and stamps its own
// keys onto each — a shared map would give every recipient the last one's
// reason.
func withReason(base notify.Inbound, rule notify.AddressRule, reason Reason,
	threadRoot string) notify.Inbound {

	meta := maps.Clone(base.Metadata)
	meta[notify.FollowReasonField] = string(reason)
	if threadRoot != "" && standing(reason) {
		// WHY THE SEAT IS IN THIS THREAD AT ALL, which is the only
		// answer a later reply has once the message that named it has
		// scrolled away. The REASON, never a bare "true": a follow is
		// not an ask, and which follow is the whole question.
		meta[notify.FollowingField] = string(reason)
	}
	// THE VERDICT, evaluated here and read everywhere else. See
	// [MetaAddressed].
	meta[MetaAddressed] = strconv.FormatBool(rule.Addressed(meta))
	base.Metadata = meta
	return base
}

// standing reports a reason that describes the seat's relationship to the
// THREAD rather than something this message did to it.
//
// FOUR, and the split is [notify.FollowingField]'s: being named, being
// `@channel`-ed and being the lead nobody else answered for are all facts
// about THIS message, and stamping one of them as a standing follow would
// make it address every later reply in the thread for ever.
func standing(reason Reason) bool {
	switch reason {
	case ReasonReplyToOwnRoot, ReasonReply, ReasonFollow, ReasonFollowAll:
		return true
	}
	return false
}

// canonicalKind maps a room's kind onto the coarse shape every backend shares.
//
// A PRIVATE ROOM IS A GROUP, which is the same answer Mattermost's `P` takes:
// the canonical set distinguishes a closed conversation from an open one, and
// a private channel is closed. A UNIT ROOM IS PUBLIC, because it is not
// private ([Visible] serves it to everybody) — it is where work addressed to
// nobody in particular lands, and a company that could not see what its own
// units were deciding would be one where the org chart hid the work.
//
// AN UNKNOWN KIND IS UNKNOWN rather than being guessed into one of the four:
// the value is a closed set a consumer switches on, and a kind a newer peer
// wrote has no place in it.
func canonicalKind(kind Kind) types.ChannelKind {
	switch kind {
	case KindDM:
		return types.ChannelDM
	case KindGroup, KindPrivate:
		return types.ChannelGroup
	case KindPublic, KindUnit:
		return types.ChannelPublic
	}
	return types.ChannelUnknown
}

// roomLabel is how a room is named in a notification's subject line.
//
// THE NAME WITH ITS SIGIL where there is one, because `#launch` is what a
// person would write and what the agent has to recognise in the conversation.
// A direct conversation has no name at all, and its id is not one — so it is
// described by what it IS rather than rendered as a uuid nobody can place.
func roomLabel(n *Notify) string {
	if n.ChannelName != "" {
		return "#" + n.ChannelName
	}
	if n.ChannelKind.Direct() {
		return "a direct conversation"
	}
	return "a chat room"
}

// recordFromBody recovers the record the feed relayed.
//
// THROUGH THE DOMAIN'S OWN DECODER, so a record a newer build wrote reaches
// this parser with everything it carried: the body is exactly the record,
// unknown fields included.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	if len(body) == 0 {
		return MutationRecord{}, fmt.Errorf("chat: the delivery carries no " +
			"record, so there is nothing to route — a change feed delivery " +
			"carries the whole record in its body")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("chat: read the delivery: %w", err)
	}
	return Decode(data)
}

// renderThread turns the record's thread lines into the block a prompt shows.
//
// ONE LINE PER MESSAGE, oldest first, in the order a person reads a
// conversation. The author is named on every line because the whole value of
// the block is knowing WHO said what — a thread rendered as anonymous prose
// is a paragraph the model attributes to whoever woke it.
//
// AN OPERATOR AND THE ENGINE ARE LABELLED, for [senderLabel]'s reason one
// level down: a seat that reads a system line as a colleague speaking will
// try to answer it.
func renderThread(lines []ThreadLine) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, line := range lines {
		who := line.Author
		switch line.AuthorKind {
		case AuthorOperator:
			who += " (operator)"
		case AuthorSystem:
			who = "the engine"
		}
		b.WriteString(who)
		b.WriteString(": ")
		b.WriteString(line.Excerpt)
		b.WriteString("\n")
	}
	return b.String()
}
