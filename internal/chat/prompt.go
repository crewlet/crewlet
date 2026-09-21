package chat

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
// addressed to this agent, and staying silent when it was not. That triage is
// the bulk of a chat trigger and it is backend-neutral, so it exists once in
// [notify.ChatPrompt] and this embeds it. A copy tuned "for native chat"
// would be the same guidance drifting in two places, and the drift would be
// invisible: both halves would keep rendering.
//
// # What is genuinely this domain's
//
// THE SENDER IS RESOLVED BY HANDLE. A native sender IS a seat's own identity,
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
// the same exception [github.com/crewlet/crewlet/internal/tracker.Prompt] and
// [github.com/crewlet/crewlet/internal/pages.Prompt] state.
type Prompt struct {
	notify.ChatPrompt
}

var _ notify.Prompt = Prompt{}

// NewPrompt builds the native chat prompt.
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
		// of a native wake is a seat, and a seat is woken whether or
		// not anybody is looking at a screen.
		Collectives:   "`@channel`",
		SelfReference: selfReference,
		MentionHint:   mentionHint,
	}}
}

// Source implements [notify.Prompt].
//
// DECLARED HERE although the embedded prompt has one, and the reason is the
// zero value: a `chat.Prompt{}` would answer with the embedded Backend, which
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
// A NATIVE WAKE IS NOT. It carries what was said ([Notify.Excerpt]), who said
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
// The thread is still READ rather than carried, and the prompt says so: see
// the thread block [notify.ChatPrompt] renders and the handling block below.
// Being told where to look is not the same as being told only that.
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
// The shared prompt renders the triage, the thread self-check and the
// message's own facts. This adds the two things only this domain can say: WHY
// the seat was woken, and what to do about it with the tools it has.
func (p Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	// RESOLVED BEFORE DELEGATING, because the shared prompt annotates a
	// sender through [notify.Parties.ByExternalID] — the vendor rule,
	// which misses here by construction: a native seat holds no external
	// id in any namespace, and asking for one would mean registering
	// every handle as its own external id in a namespace that exists
	// only to satisfy the call. The label it would have produced is
	// produced here instead, from the handle the record carries.
	n.Sender = senderLabel(n, parties)

	var b strings.Builder
	b.WriteString(p.ChatPrompt.Build(n, parties))
	b.WriteString(earlier(n.Metadata))
	b.WriteString(whyYou(n.Metadata))
	b.WriteString(handling(n.Metadata))
	return b.String()
}

// earlier renders the thread this message is part of, when there is one.
//
// BEFORE the routing reason and the handling block, because it is what the
// newest message MEANS: a reply read without the exchange behind it is a
// sentence the seat has to guess the subject of, and a model that guesses
// answers the wrong question confidently.
//
// IT IS ALREADY BOUNDED when it arrives — the record carries at most the
// company's `chat.native.thread_context_messages` lines within
// [MaxThreadContextBytes], taken newest-first at write time — so nothing here
// trims, and nothing here reads a clock or a row. This frame renders what the
// record decided.
//
// NOTHING AT ALL FOR A ROOT POST, rather than an empty heading: a block that
// announces a conversation and then shows none reads as a thread whose
// history was lost.
func earlier(meta map[string]string) string {
	thread := meta[MetaThread]
	if thread == "" {
		return ""
	}
	return "\n## Earlier in this thread\n\n" +
		"What was said before the message above, oldest first. It is context " +
		"for answering, not something to reply to line by line.\n\n" +
		thread
}

// senderLabel renders the author as a colleague, BY HANDLE.
//
// The author kind is what decides the fallback, and the three that are not
// seats are the whole reason it is stamped: an operator acting through a
// token and the engine's own narration resolve to nobody in the roster, and
// rendering either as a bare handle invites the agent to reply to a colleague
// who does not exist.
func senderLabel(n notify.Inbound, parties notify.Parties) string {
	handle := n.Metadata[notify.ActorField]
	if handle == "" {
		handle = n.Sender
	}
	if parties != nil && handle != "" {
		if party, ok := parties.ByHandle(handle); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	switch AuthorKind(n.Metadata[MetaAuthorKind]) {
	case AuthorOperator:
		return handle + " (an operator, through the API — not a seat you can " +
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

// whyYou states the routing reason in the recipient's own terms.
//
// THE REASON IS RENDERED rather than left on the metadata, because it is the
// difference between an ask and an FYI and the seat has to make that
// distinction in one read. The shared triage block teaches how to find an
// addressee in the TEXT; this says what the ROUTING already concluded, which
// the text cannot always show — nothing in "we should ship on Friday" says it
// reached you because nobody else in your unit was listening.
func whyYou(meta map[string]string) string {
	var line string
	switch Reason(meta[notify.FollowReasonField]) {
	case ReasonDM:
		line = "This is a direct conversation: the message was addressed to " +
			"you and to nobody else."
	case ReasonMention:
		line = "You were named in this message."
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
	out := "\n**Why you:** " + line
	if meta[MetaTruncated] == "true" {
		// A PARTIAL BROADCAST READS EXACTLY LIKE A COMPLETE ONE unless
		// it is said: the seats that were woken otherwise assume the
		// rest of the room heard it too, and nobody repeats it.
		out += "\n**Note:** this address reached fewer seats than the room " +
			"has — it exceeded the limit on how many one `@channel` may wake. " +
			"Do not assume everybody in the room has seen it."
	}
	return out + "\n"
}

// handling is what to do about it, with the tools this build ships.
//
// TWO BRANCHES, on the verdict the routing already reached rather than on the
// reason again: being asked and being told are what a recipient does
// differently, and the eight reasons collapse to those two.
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
	b.WriteString("\n2. **Read before you answer.** The triggering message is " +
		"here in full, but the conversation around it is not — read the room " +
		"or the thread with your chat tools when the answer depends on what " +
		"came before." +
		"\n3. **Take it out of the room only when it belongs out of it.** " +
		"`send_dm` reaches one person privately; use it for something that " +
		"is genuinely between the two of you, never to answer a question " +
		"that was asked in the open." +
		"\n4. **If you have decided not to act** — out of scope, already " +
		"handled, somebody else owns it — say so in one sentence rather than " +
		"going silent. Somebody is waiting on this message, and to them " +
		"silence is indistinguishable from a message that was lost.\n")
	return b.String()
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
