package chatlet

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a message asks of the seat it reached.
//
// # It REUSES the shared chat prompt rather than forking it
//
// Every chat backend delivers the same KIND of event — somebody said
// something in a room, a direct conversation or a thread — and the hard part
// is the same for all of them: deciding whether the message was actually
// addressed to this agent, and staying silent when it was not. The shared
// prompt settles that under this domain's own [AddressRule], stating the
// engine's verdict on an addressed wake and the triage on any other, so it
// exists once in [notify.ChatPrompt] and this embeds it. A copy tuned "for
// Chatlet" would be the same guidance drifting in two places, and the
// drift would be invisible: both halves would keep rendering.
//
// # What is genuinely this domain's
//
// THE SENDER IS RESOLVED BY HANDLE. A Chatlet sender IS a seat's own identity,
// not an account somewhere else, so there is nothing to scope by transport
// and nothing to look up in a namespace — see [notify.Parties.ByHandle],
// which exists for exactly the first-party sources.
//
// THE VERDICT IS READ, NOT RE-DERIVED. Whether somebody is waiting was
// decided by the routing and stamped on the wake; see [MetaAddressed].
//
// IT NAMES ITS OWN TOOLS. The rule that a prompt must not name a tool is
// about a deployed MCP server, whose tool names the engine cannot know. These
// are shipped by this build, registered by this build under names this build
// chose, and present on every seat that can read this notification at all —
// the same exception [github.com/crewlet/crewlet/internal/worklet.Prompt] and
// [github.com/crewlet/crewlet/internal/doclet.Prompt] state.
type Prompt struct {
	notify.ChatPrompt
}

var _ notify.Prompt = Prompt{}

// NewPrompt builds the Chatlet prompt.
//
// A CONSTRUCTOR, because the embedded value has to be filled: a prompt whose
// [notify.ChatPrompt.Address] is the zero rule reads a direct conversation as
// an ordinary room, and a person's consecutive messages in a DM then land in
// as many turns as they typed.
func NewPrompt() Prompt {
	return Prompt{ChatPrompt: notify.ChatPrompt{
		Backend: Source,
		Label:   "chat",
		// The SAME value the working indicator reads. One rule, so the
		// spinner and the reply obligation cannot be two opinions.
		Address: AddressRule(),
		// ONE collective address here, unlike a vendor's two or three:
		// a message either addressed the room or it did not, and there
		// is no "whoever is online" to narrow it to — every recipient
		// of a Chatlet wake is a seat, and a seat is woken whether or
		// not anybody is looking at a screen.
		Collectives:   "`@channel`",
		SelfReference: selfReference,
		MentionHint:   mentionHint,
	}}
}

// Source implements [notify.Prompt].
//
// DECLARED HERE although the embedded prompt has one, and the reason is the
// zero value: a `chatlet.Prompt{}` would answer with the embedded Backend, which
// is empty — and [notify.NewPrompts] SKIPS a prompt whose source is empty, so
// a company would render every chat wake through the generic fallback with no
// symptom anywhere. The package constant cannot be empty.
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: never, which is the inverse of
// the vendor rule and deliberate.
//
// A VENDOR'S THREAD REPLY IS A POINTER. The webhook carries a fragment — "yes",
// "+1" — and the conversation is somewhere else, so the seat has to go and
// fetch it before it has any context at all.
//
// A CHATLET WAKE IS NOT. It carries what was said — the message WHOLE, read from
// the record's own payload rather than the card's [Notify.Excerpt] — who said
// it, which room, which thread, and why it reached this seat — every one of
// them on the record, copied at write time from the snapshot the routing was
// decided in. The seat can begin reasoning from the trigger.
//
// The flag is not cosmetic: it ALSO suppresses the turn-start personal-memory
// filtering and episode recall. Setting it on a wake that already carries what
// it means costs the seat its own context for nothing — and a chat turn is the
// one kind where that context is most of the value, because what this
// colleague asked last week is exactly what decides how to answer them now.
//
// THE THREAD AROUND IT IS READ, NOT CARRIED: the turn-start prefetch hands the
// seat its thread through [ThreadReader], the way it does every chat
// surface's, read at the seat's own linearizable level so it includes the
// message that woke the turn. One mechanism serves every surface, and a thread
// is never in the prompt twice — a copy on the record beside the prefetch's
// would double its cost on every round of the turn and let the two disagree.
func (Prompt) RequiresRecon(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: the verdict the routing stamped.
//
// READ RATHER THAN RE-DERIVED. Whether this wake obliges an answer is a
// property of the (message, recipient) PAIR — one message addresses one seat
// and merely informs another — and the parser is the only party that holds
// both. It evaluated [AddressRule] there and stamped the answer; the working
// indicator evaluates the same rule over the same metadata. So the gate, the
// prompt and the indicator are readers of ONE evaluation rather than three
// evaluations of one rule, which is the difference between agreeing by
// construction and agreeing by coincidence.
//
// AN ABSENT STAMP IS "NOT ADDRESSED", which is the conservative half: a seat
// wrongly told nobody is waiting keeps the freedom to stay silent, while one
// wrongly told somebody is must post something on every broadcast it observes.
func (Prompt) Addressed(n notify.Inbound) bool {
	return n.Metadata[MetaAddressed] == "true"
}

// Build implements [notify.Prompt].
//
// The shared prompt renders the engine's verdict on an addressed wake and the
// triage on any other, the line that names the seat's own replies in a
// thread, and the message's own facts. This adds the two things only this
// domain can say: the routing reason the verdict has no words for, and what
// to do about it with the tools it has.
func (p Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	// RESOLVED BEFORE DELEGATING, because the shared prompt annotates a
	// sender through [notify.Parties.ByExternalID] — the vendor rule,
	// which misses here by construction: a Chatlet seat holds no external
	// id in any namespace, and asking for one would mean registering
	// every handle as its own external id in a namespace that exists
	// only to satisfy the call. The label it would have produced is
	// produced here instead, from the handle the record carries.
	n.Sender = senderLabel(n, parties)

	var b strings.Builder
	b.WriteString(p.ChatPrompt.Build(n, parties))
	b.WriteString(whyYou(n.Metadata))
	b.WriteString("\n")
	b.WriteString(handling(n.Metadata))
	return b.String()
}

// senderLabel renders the author of the message that woke a seat.
func senderLabel(n notify.Inbound, parties notify.Parties) string {
	handle := n.Metadata[notify.ActorField]
	if handle == "" {
		handle = n.Sender
	}
	return AuthorLabel(handle, AuthorKind(n.Metadata[MetaAuthorKind]), parties)
}

// AuthorLabel renders a Chatlet author as a colleague, BY HANDLE — the
// one rendering both the woken message's sender and every line of the thread
// it is part of ([ThreadReader]) are named by, so one person is never two
// labels in one prompt.
//
// The author kind is what decides the fallback, for a handle the roster does
// not resolve. The engine's own narration never resolves — it is nobody — and
// rendering it as a bare handle invites the agent to reply to a colleague who
// does not exist. A human author is a person, writing as the seat they hold (a
// credential acting as itself says nothing in a room at all), so the roster
// ordinarily names them as the person they are; the person label is what is
// left when it no longer does — the seat removed from the chart since — and it
// still says the one thing an agent must not get wrong: nobody answers an
// agent-to-agent ask there. A credential's label is the same promise for a
// record that names one, which no write of this build makes.
func AuthorLabel(handle string, kind AuthorKind, parties notify.Parties) string {
	if parties != nil && handle != "" {
		if party, ok := parties.ByHandle(handle); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	switch kind {
	case AuthorHuman:
		return handle + " (a person — not a seat you can reach with an " +
			"agent-to-agent ask)"
	case AuthorOperator:
		return handle + " (a credential, through the API — not a seat you can " +
			"reach with an agent-to-agent ask)"
	case AuthorSystem:
		return "the engine itself (a system line describing the room, not " +
			"somebody speaking in it)"
	}
	if handle == "" {
		return "someone"
	}
	return handle
}

// whyYou states the routing reason in the recipient's own terms, where the
// shared verdict does not already state it.
//
// THE REASON IS RENDERED rather than left on the metadata, because it is the
// difference between an ask and an FYI and the seat has to make that
// distinction in one read. The shared triage teaches how to find an addressee
// in the TEXT; this says what the ROUTING already concluded, which the text
// cannot always show — nothing in "we should ship on Friday" says it reached
// you because nobody else in your unit was listening.
//
// TWO REASONS ARE THE VERDICT'S OWN and get no line here. A direct
// conversation and a mention are what the shared prompt's "**Addressed to
// you:**" line says, read from this domain's own rule, and a line here beside
// it told the seat one reason twice. Every other reason says what the verdict
// cannot: that the thread is the seat's own, that it leads the unit nobody
// else answered in, or which kind of news an unaddressed wake is.
func whyYou(meta map[string]string) string {
	var line string
	switch Reason(meta[notify.FollowReasonField]) {
	case ReasonDM, ReasonMention:
		// The shared verdict's: see above.
	case ReasonReplyToOwnRoot:
		line = "This is a reply in a thread YOU started."
	case ReasonLeadFallback:
		line = "Somebody spoke in your unit's room and nobody else here was " +
			"reached. It comes to you as the unit's lead — a room where this " +
			"resolves to silence is a person talking to an empty screen."
	case ReasonReply:
		line = "You have spoken in this thread, so you are hearing the rest " +
			"of it. That is not an ask."
	case ReasonCollective:
		line = "The message addressed the whole room (`@channel`) rather " +
			"than anybody in it."
	case ReasonFollow:
		line = "You follow this thread."
	case ReasonFollowAll:
		line = "You follow everything said in this room."
	default:
		// A REASON THIS BUILD DOES NOT KNOW is still a wake: a newer
		// build routed this seat deliberately, and telling it nothing
		// about why would leave it guessing from the text alone. It is
		// carried unaddressed, so the honest framing is news.
		line = "A newer build routed this to you for a reason this one does " +
			"not know. Read it as news rather than as a question put to you."
	}
	var out string
	if line != "" {
		out = "\n**Why you:** " + line
	}
	if meta[MetaTruncated] == "true" {
		// A PARTIAL BROADCAST READS EXACTLY LIKE A COMPLETE ONE unless
		// it is said: the seats that were woken otherwise assume the
		// rest of the room heard it too, and nobody repeats it.
		out += "\n**Note:** this address reached fewer seats than the room " +
			"has — it exceeded the limit on how many one `@channel` may wake. " +
			"Do not assume everybody in the room has seen it."
	}
	return out
}

// handling is what to do about it, with the tools this build ships.
//
// TWO BRANCHES, on the verdict the routing already reached rather than on the
// reason again: being asked and being told are what a recipient does
// differently, and the eight reasons collapse to those two.
//
// WHAT A SEAT OWES WHEN IT DECLINES IS NOT HERE. The executor's contract
// states it for every surface and the delivery check enforces it, which is
// why the shared prompt does not restate it either ([notify.ChatPrompt]); a
// copy here told an addressed seat the rule twice. What is this domain's is
// where an answer goes, with the tools it ships.
func handling(meta map[string]string) string {
	if meta[MetaAddressed] != "true" {
		return "\n## How to handle this" +
			"\nThis is news, not a request. Read it if it bears on work you " +
			"have in hand, and otherwise do nothing — a message being said " +
			"near you is not an instruction. If it CONTRADICTS something you " +
			"are doing, say so: reply with `reply_in_thread` where it was " +
			"said, rather than starting a new topic about it.\n"
	}
	b := strings.Builder{}
	b.WriteString("\n## How to handle this" +
		"\n1. **Answer where you were asked.** `reply_in_thread` puts your " +
		"answer under the thread named above, beside the question, where the " +
		"next person to read it will find it. `post_message` starts a NEW " +
		"top-level message in the room, which is the wrong place for an " +
		"answer and reads as a second conversation.")
	if meta[notify.ThreadField] == "" {
		b.WriteString("\n   This message is top-level and has no thread yet: " +
			"replying under it is what creates one.")
	}
	b.WriteString(readFirst(meta))
	b.WriteString("\n3. **Take it out of the room only when it belongs out of it.** " +
		"`send_dm` reaches one person privately; use it for something that " +
		"is genuinely between the two of you, never to answer a question " +
		"that was asked in the open.\n")
	return b.String()
}

// readFirst is what the seat already holds of the conversation, and what it
// has to go and read.
//
// BRANCHED ON THE THREAD, because the two wakes arrive holding different
// things. A THREAD REPLY'S THREAD IS ALREADY IN THE PROMPT: the turn-start
// prefetch read it through [ThreadReader] and rendered it under
// `## The thread so far`, so telling the seat to go and read it spends a round
// on a second copy of what it holds — and a second copy of one thread in one
// turn is what retiring the record's own excerpt removed. What is left to read
// is what that block says it could not give: a thread it could not read, a
// message it left out, the exact words of the middle it condensed, or the room
// outside the thread. A TOP-LEVEL MESSAGE opens no thread, so nothing said
// before it was handed over at all, and reading the room is the only way to it.
func readFirst(meta map[string]string) string {
	if meta[notify.ThreadField] != "" {
		return "\n2. **The thread so far is already in front of you.** It was " +
			"read for you at the start of this turn and is in your system " +
			"prompt under `## The thread so far`, with the message that woke " +
			"you. Answer from it: reach for `" + ReadChannelTool + "` only " +
			"where that block says the thread could not be read or left " +
			"messages out, when you need the exact words of a message it " +
			"condensed, or when the answer depends on the room outside the " +
			"thread."
	}
	return "\n2. **Read the room when the answer depends on it.** The " +
		"triggering message is here in full, but it opens no thread, and " +
		"nothing said before it in the room was handed to you — read the " +
		"room with `" + ReadChannelTool + "` when the answer depends on what " +
		"came before."
}

// selfReference is how somebody addresses this agent here: `@handle`, in the
// message text.
//
// THE RECIPIENT'S OWN HANDLE, which the spine stamps after resolution and
// which is therefore the one identity the parser could not have known. There
// is no second identity to reconcile — no bot id beside a username — because
// a seat IS its handle on this surface.
func selfReference(meta map[string]string) string {
	if handle := meta[notify.RecipientField]; handle != "" {
		return "`@" + handle + "`"
	}
	return ""
}

// mentionHint tells the agent how to WRITE a mention, which here is the same
// as how it reads one.
//
// Worth saying explicitly: a model that has seen Slack will reach for
// `<@U123>` markup, which this domain stores as the literal text it is and
// which resolves to nobody.
func mentionHint(map[string]string) string {
	return "**Mentions:** write them literally as `@handle` — a mention is " +
		"resolved from the text as typed, so any other markup renders as " +
		"itself and reaches nobody."
}
