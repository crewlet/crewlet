package chatlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/named"
	"github.com/crewlet/crewlet/internal/notify"
)

// THE METADATA THIS DOMAIN ADDS, on top of the shared chat vocabulary.
//
// The shared keys are [github.com/crewlet/crewlet/internal/notify.ChatKeys],
// spelled through notify's own constants, never as literals here, so a rename
// is a compile error rather than a silence.
//
// The four below are facts no vendor has, which that vocabulary cannot hold.
// EVERY ONE HAS A PRODUCER AND A READER: the parser stamps them and [Prompt]
// reads them back. A key nobody reads is work that looks done.
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
	// next. The engine's own narration is not a seat and resolves through
	// [notify.Parties] as nobody. A human author is a PERSON, writing as
	// the `kind: human` seat they hold — so the roster ordinarily names
	// them as that person, and the kind is what is left to say when it no
	// longer does. Either way a person replies on their own time and
	// cannot be reached by an agent-to-agent ask.
	MetaAuthorKind = "chat_author_kind"

	// MetaTruncated marks a collective address that reached fewer seats
	// than the room has, because it exceeded [MaxCollectiveRecipients].
	//
	// It is on the RECORD ([Notify.WakesTruncated]) and it is rendered,
	// because a room that cannot see it reads a partial broadcast as a
	// complete one: the seats that WERE woken then assume the rest heard
	// it too, and nobody says the thing again.
	MetaTruncated = "chat_wakes_truncated"

	// MetaAsked is how many seats a person's `@channel` obliged to answer:
	// every recipient of the message routed under an addressed reason,
	// stamped only on the wakes of a message that raised a
	// [ReasonCollectiveRequest].
	//
	// Read by the prompt, because the answer to a question put to thirty
	// seats is not the answer to one put to three: each seat is told how
	// many share it, so it answers for its own role rather than for the
	// room. Counted HERE, from the set this parser routed, because that
	// is the set that is actually woken; the record's own list was routed
	// against the roster of an earlier instant.
	MetaAsked = "chat_asked"
)

// AddressRule is this domain's answer to "was this message addressed to this
// seat?", ONE DECLARED VALUE for [notify.AddressRule]'s reason: the prompt, the
// delivery gate that refuses to let an addressed turn end in silence, and the
// working indicator must not be three opinions (a spinner over a turn allowed
// to end in silence is the same bug as a silence after one).
//
// NO DMPrefix: a channel id is a uuid, so a prefix test would mark arbitrary
// rooms as DMs and oblige every seat in them to answer unaddressed traffic.
//
// A FUNCTION, not a package var: the value holds two slices a var would share.
// It is spelled as the vendors' (`slack.AddressRule`) because the working
// indicator reaches it through a backend's StatusPoster.
func AddressRule() notify.AddressRule {
	// The direct kinds are [Kind.Direct]'s. The partition key
	// ([notify.ChatPrompt.PartitionKey]) coalesces a person's consecutive
	// top-level DM messages into one turn, and the conversation identity
	// ([notify.ChatPrompt.ConversationIdentity]) files a DM's history under
	// its line rather than one thread of it.
	direct := make([]string, 0, len(Kinds()))
	for _, k := range Kinds() {
		if k.Direct() {
			direct = append(direct, string(k))
		}
	}
	// The follows are the reasons [Reason.Addressed] accepts: notify's own four
	// would read this domain's (`reply_to_own_root`, `collective_request`,
	// `answerer`) as "does not address".
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
// It holds nothing, which is the whole of its correctness. Not the roster: it
// arrives per call, since a parser outlives the epoch a wake was written under.
// Not the answerers: a [ReasonAnswerer]'s seat RIDES THE RECORD, resolved at
// write time inside the decide, because resolving it later would route a
// message posted under one org chart to whoever answers under the next. Not a
// thread, membership or room: the record's [Notify] is the snapshot the routing
// was decided from, so this runs on a node whose applier has not reached it.
type Parser struct{}

// NewParser builds Chatlet's inbound parser.
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
// It re-runs the routing rather than reading the record's recipient list.
// [Candidates] is PURE over the record; [Route] needs who the company employs
// RIGHT NOW and who wrote the message. They are the SAME two functions the
// write path's decide called, so applier and parser cannot disagree about who
// a message concerned.
//
// The record's set was routed against the roster of its write instant; this one
// against the current roster, so a removed seat is dropped and the named
// answerer is offered again. A NEWER BUILD's record may name more seats than
// this build's caps allow, and a wake set is SPENT, not kept like a row.
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
	if len(n.Held) > 0 {
		// WHAT THE QUIET RULE KEPT FROM WAKING (ADR-0113), once per
		// record: the seats still read the message in the room.
		log.InfoContext(ctx, "chat_wakes_held", "message", n.MessageID,
			"channel", n.ChannelID, "author", n.Author,
			"author_kind", string(n.AuthorKind), "asks", n.Asking(),
			"held", len(n.Held), "woken", len(routed))
	}
	held := heldSeats(n, rosterOf(reg))
	if len(routed) == 0 && len(held) == 0 {
		return nil, nil
	}

	base := p.inbound(record)
	if asked := askedBy(routed); asked > 0 {
		base.Metadata[MetaAsked] = strconv.Itoa(asked)
	}
	// THE RULE IS BUILT ONCE for the whole record: every recipient's
	// verdict is the same rule read against that recipient's own reason,
	// and rebuilding it per copy would allocate the vocabulary's two
	// derived sets on the hottest path this domain has.
	rule := AddressRule()
	out := make([]notify.Routed, 0, len(routed)+len(held))
	for _, c := range held {
		// TOLD AND NOT WOKEN: the held wake's record, under the reason the
		// quiet rule held the seat at, never a delivery.
		out = append(out, notify.Routed{Inbound: notify.Inbound{Source: base.Source,
			Origin: types.TriggerOrigin{Kind: types.OriginChatMessage, ID: n.MessageID,
				Reason: string(c.Reason)}},
			To: notify.Recipient{Handle: c.Handle}, Held: true,
			WakeID: changefeed.WakeID(record.OpID, c.Handle), WakeAt: record.CreatedAt})
	}
	for _, c := range routed {
		out = append(out, notify.Routed{
			Inbound: withReason(base, rule, c.Reason, n.ThreadRoot),
			// THE HANDLE DIRECTLY, which is what a first-party source
			// has and a vendor does not: a Chatlet sender IS a seat's
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
			// AND STAMPED WITH THE RECORD'S OWN INSTANT, which every copy
			// of this wake carries — see [notify.Routed.WakeAt]. The
			// woken turn's unit of work starts at it, and every operation
			// id the turn derives carries that start: stamped with the
			// delivery's own clock instead, a producer retry would hand a
			// re-run turn a later start, and its posts would derive ids
			// the first run's never collapse onto — the same remark,
			// written twice.
			WakeAt: record.CreatedAt,
		})
	}
	return out, nil
}

// askedBy is how many of the routed seats a person's `@channel` obliged to
// answer ([MetaAsked]), or 0 for a message that raised no such request.
//
// EVERY ADDRESSED RECIPIENT COUNTS, not only those routed as the request: a
// seat the same message also named, or whose thread it answered, was claimed
// under that stronger reason and is asked all the same, so leaving it out
// would tell the rest that fewer seats share the question than do.
func askedBy(routed []Candidate) int {
	requested, asked := false, 0
	for _, c := range routed {
		requested = requested || c.Reason == ReasonCollectiveRequest
		if c.Reason.Addressed() {
			asked++
		}
	}
	if !requested {
		return 0
	}
	return asked
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

// heldSeats is the agent seats the quiet rule held a message from waking that
// the company still employs, never its author.
func heldSeats(n *Notify, known func(string) (exists, human bool)) []Recipient {
	if known == nil {
		return nil
	}
	var out []Recipient
	for _, c := range n.Held {
		if c.Handle == "" || c.Handle == n.Author {
			continue
		}
		if exists, human := known(c.Handle); exists && !human {
			out = append(out, c)
		}
	}
	return out
}

// MessageConversation is the conversation message m in room c is in, as every
// wake about it, a reply in its thread included, is stamped: the identity the
// keys [Parser.inbound] stamps give it ([notify.ChatPrompt.ConversationIdentity]
// under [AddressRule]) namespaced by this source. A seat's post records it
// (internal/agent/builtin), and a running run's router finds a reply in the
// thread by it, exactly (ADR-0167): the thread in a room, the line in a direct
// conversation. "" for a room or message with no id.
func MessageConversation(c Channel, m Message) string {
	if c.ID == "" || m.ID == "" {
		return ""
	}
	meta := map[string]string{notify.ChannelField: c.ID, notify.ChannelTypeField: string(c.Kind),
		notify.MessageIDField: m.ID, notify.ThreadField: m.ThreadRoot}
	meta[notify.ThreadAnchorField] = notify.Anchor(meta)
	return notify.Namespaced(Source, NewPrompt().ConversationIdentity(meta, ""))
}

// ConversationPlace is where conversation key, as [MessageConversation] and
// every wake stamp it, is said: its room, and the message whose thread it is,
// "" for a direct conversation, which is its whole room. False for a key of
// another source or none.
func ConversationPlace(key string) (room, anchor string, ok bool) {
	local, found := strings.CutPrefix(key, Source+":")
	if !found {
		return "", "", false
	}
	room, anchor, _ = strings.Cut(local, ":")
	return room, anchor, room != ""
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
		// A CHATLET SENDER IS A HANDLE. Both keys carry it: the learning
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
	room := roomLine(n)
	return notify.Inbound{
		Source: Source,
		// THE RECORD'S OWN WORD for what happened — `post` or `edit` —
		// rather than a change vocabulary of this parser's. It is the
		// value the event store's source-event-type column carries, and
		// a second spelling would make a dashboard filter over this
		// domain's own log disagree with the log.
		EventType:   string(record.Op),
		Sender:      n.Author,
		Subject:     room.Text,
		SubjectRefs: room.Refs,
		Body:        wakeBody(record),
		Metadata:    meta,
		// AND WHO WROTE IT, as the other first-party sources state theirs
		// (notify.FirstPartyWriter): a message is said as a seat, an agent's
		// or the human seat a person holds, so the writer is that seat (and
		// the partition keys on it, `seat:`, as it does theirs), and whom an
		// agent wrote it for, which the turn this wakes works for (ADR-0109).
		Writer: notify.FirstPartyWriter(iam.ActorKind(n.AuthorKind), n.Author, nil, record.WorkingFor),
	}
}

// wakeBody is what a woken seat reads of a message: the WHOLE body, from the
// record that carries it, where the card's excerpt was cut from it.
//
// The card excerpt is cut at [MaxExcerpt]; the wake prompt is the one message
// a seat ACTS on, up to [MaxBody]. Handed the excerpt, a seat answered the
// first six hundred bytes of a two-kilobyte direct message as if they were all
// of it, and a top-level message has no thread to prefetch the rest from. The
// tracker's and knowledge base's wakes follow the same rule.
//
// RECOGNISED BY DERIVATION, never guessed: the body is used exactly when the
// record's excerpt is what [Excerpt] makes of it. Anything else (an op this
// build does not decode, an unparseable payload) keeps the excerpt.
func wakeBody(record MutationRecord) string {
	n := record.Notify
	if n == nil || n.Excerpt == "" {
		return ""
	}
	var body string
	switch record.Op {
	case OpPost:
		var post MessagePost
		if json.Unmarshal(record.Mutation, &post) != nil {
			return n.Excerpt
		}
		body = post.Body
	case OpEdit:
		var edit MessageEdit
		if json.Unmarshal(record.Mutation, &edit) != nil {
			return n.Excerpt
		}
		body = edit.Body
	default:
		return n.Excerpt
	}
	if Excerpt(body) == n.Excerpt {
		return strings.TrimSpace(body)
	}
	return n.Excerpt
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
	// WHAT STARTS THE TURN THIS WAKES: the message, under this recipient's
	// own reason, so the turn is found by the message a person wrote.
	base.Origin = types.TriggerOrigin{Kind: types.OriginChatMessage,
		ID: meta[notify.MessageIDField], Reason: string(reason)}
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
// `@channel`-ed and being the answerer nobody else spoke up for are all facts
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

// roomLine is [roomLabel] naming the room (package named), so a line about
// the wake links to it.
func roomLine(n *Notify) named.Line {
	return named.Words("").Name(named.RoomRef(n.ChannelID, roomLabel(n)))
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
// THROUGH THE DOMAIN'S OWN DECODER, so a newer build's record keeps every
// field it carried, unknown ones included.
//
// A RECORD AT A VERSION A NEWER BUILD WRITES IS DEFERRED to a peer on that
// build ([notify.Defer]), never refused as unreadable: notify acks what a
// parser refuses, on the ONE group every node takes inbound mail from, so an
// older node winning the delivery during a roll lost the wake for the whole
// fleet. It is the feed's future-version arm ([Translator]) met on this side.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	if len(body) == 0 {
		return MutationRecord{}, fmt.Errorf("chatlet: the delivery carries no " +
			"record, so there is nothing to route — a change feed delivery " +
			"carries the whole record in its body")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("chatlet: read the delivery: %w", err)
	}
	record, err := Decode(data)
	var future *ErrFutureVersion
	if errors.As(err, &future) {
		return MutationRecord{}, notify.Defer(notify.DeferNewerRecord, err)
	}
	return record, err
}
