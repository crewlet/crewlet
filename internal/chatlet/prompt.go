package chatlet

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is what a message asks of the seat it reached.
//
// It REUSES the shared chat prompt rather than forking it: whether a message
// addressed this agent is settled once in [notify.ChatPrompt] under this
// domain's [AddressRule], which this embeds; a copy would drift invisibly. The
// sender is resolved by handle ([notify.Parties.ByHandle]): it IS a seat.
//
// IT NAMES ITS OWN TOOLS: the rule against naming a tool concerns a deployed
// MCP server, whose names the engine cannot know. These are this build's, on
// every seat that can read this notification, the exception
// [github.com/crewlet/crewlet/internal/worklet.Prompt] and
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
		// WHO WROTE THE MESSAGE, as its fence names them: the roster's
		// seat for the handle the record carries, and otherwise what the
		// record says they were — see [messageAuthor].
		Author: messageAuthor,
		// THE ENGINE'S OWN SURFACE: every sender here authenticated as a
		// seat, a person or the engine, so a message whose sender the
		// record no longer names is still a colleague's.
		Hosted: true,
	}}
}

// messageAuthor is who wrote a Chatlet message, as its fence names them: the
// first-party rule every engine-hosted surface names its writers by
// ([notify.FirstPartyAuthor]) — the roster's seat for the handle the record
// carries, and where it no longer names them the kind the record stated.
func messageAuthor(meta map[string]string, parties notify.Parties) provenance.Author {
	return notify.FirstPartyAuthor(meta[notify.ActorField],
		iam.ActorKind(meta[MetaAuthorKind]), parties)
}

// Source implements [notify.Prompt].
//
// DECLARED HERE although the embedded prompt has one, and the reason is the
// zero value: a `chatlet.Prompt{}` would answer with the embedded Backend, which
// is empty — and [notify.NewPrompts] SKIPS a prompt whose source is empty, so
// a company would render every chat wake through the generic fallback with no
// symptom anywhere. The package constant cannot be empty.
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: never, the inverse of the vendor
// rule and deliberate. A vendor's thread reply is a pointer (a fragment like
// "yes"), so the seat must fetch the conversation first. A CHATLET WAKE carries
// the message WHOLE (not the card's [Notify.Excerpt]), author, room, thread
// and routing reason, copied at write time from the routing's snapshot.
//
// The flag ALSO suppresses the turn-start personal-memory filtering and episode
// recall, which a chat turn needs most (what this colleague asked last week).
//
// THE THREAD IS READ, NOT CARRIED: the turn-start prefetch hands it over
// through [ThreadReader], linearizably so it includes the waking message; a
// copy on the record would cost double every round and could disagree.
func (Prompt) RequiresRecon(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: the verdict the routing stamped.
//
// READ RATHER THAN RE-DERIVED. Whether a wake obliges an answer is a property
// of the (message, recipient) PAIR, which only the parser holds. It evaluated
// [AddressRule] there and stamped the answer; the working indicator evaluates
// the same rule over the same metadata, so gate, prompt and indicator read ONE
// evaluation rather than agreeing by coincidence.
//
// AN ABSENT STAMP IS "NOT ADDRESSED", the conservative half: a seat wrongly
// told nobody is waiting may stay silent, while one wrongly told somebody is
// must post on every broadcast it observes.
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
	// RESOLVED BEFORE DELEGATING, for the author the roster does NOT
	// name. The shared prompt annotates a sender it resolves through
	// [notify.Parties.ByExternalID] — which answers this source by the
	// handle its ids are — and leaves any other as the id it arrived
	// with: here a bare handle, which an agent would answer as a
	// colleague. What such an author IS is the record's kind, which only
	// this domain reads, so the sender the shared prompt falls back to is
	// rendered here ([AuthorLabel]).
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

// AuthorLabel renders a Chatlet author as a colleague, BY HANDLE: the one
// rendering for the woken sender and every [ThreadReader] line, so one person
// is never two labels in one prompt.
//
// The author kind decides the fallback for a handle the roster does not
// resolve. The engine's own narration never resolves (it is nobody), and a bare
// handle invites a reply to a colleague who does not exist. A human writes as
// the seat they hold, so the roster ordinarily names them; the person label is
// what is left once the seat is removed from the chart, and it keeps the one
// thing an agent must not get wrong: nobody answers an agent-to-agent ask
// there. A credential's label is the same promise for a record naming one,
// which no write of this build makes.
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
// THE REASON IS RENDERED, not left on the metadata: it separates an ask from an
// FYI in one read. The shared triage finds an addressee in the TEXT; this says
// what the ROUTING concluded, which the text may not show.
//
// TWO REASONS ARE THE VERDICT'S OWN and get no line: a direct conversation and
// a mention are what the shared "**Addressed to you:**" line already says.
// Every other reason says what the verdict cannot: the thread is the seat's
// own, the whole room was asked, the seat answers for a room nobody else did,
// or what kind of news an unaddressed wake is.
func whyYou(meta map[string]string) string {
	var line string
	switch Reason(meta[notify.FollowReasonField]) {
	case ReasonDM, ReasonMention:
		// The shared verdict's: see above.
	case ReasonReplyToOwnRoot:
		line = "This is a reply in a thread YOU started."
	case ReasonCollectiveRequest:
		line = collectiveRequestLine(meta[MetaAsked])
	case ReasonAnswerer:
		line = answererLine(Kind(meta[notify.ChannelTypeField]))
	case ReasonAnswer:
		line = "This answers a question you asked here. Nothing more is " +
			"asked of you unless it says so."
	case ReasonNamed:
		line = "An agent named you while telling the room something; it " +
			"asks you nothing. Answer only if you have something to add."
	case ReasonOwing:
		line = "You were asked something in this thread and have not " +
			"answered yet; this adds to what you were asked."
	case ReasonAwaiting:
		line = "You asked something in this thread that has not been " +
			"answered yet; this is a reply in it."
	case ReasonReply:
		line = "You have spoken in this thread, so you are hearing the rest " +
			"of it. That is not an ask."
	case ReasonCollective:
		line = "An agent addressed the whole room (`@channel`) to tell it " +
			"something, not to ask you anything."
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

// collectiveRequestLine is why a person's `@channel` reached this seat, and
// what answering it means when the question went to many seats at once.
//
// THE COUNT IS SAID because it changes the answer: one of three seats asked
// owes the room a full reply, while one of thirty owes it the part that is
// its own and nothing that restates its colleagues. It is the parser's count
// of the seats the message obliged ([MetaAsked]), and a wake that lost it
// still reads as a request to many.
func collectiveRequestLine(asked string) string {
	who := "one of the seats it asked"
	if n, err := strconv.Atoi(asked); err == nil && n > 1 {
		who = "one of " + strconv.Itoa(n) + " seats it asked"
	} else if err == nil && n == 1 {
		who = "the only seat it asked"
	}
	return "A person addressed the whole room (`@channel`), so this is a " +
		"request to every seat in it, and you are " + who + " to answer. " +
		"Answer for your own role only: what you know, what you will do, or " +
		"that you have nothing to add. Keep it short, and leave the parts " +
		"that belong to other seats to them."
}

// answererLine is why a seat was woken as the room's answerer, in the terms
// of the room it answers for.
//
// TWO ROOMS, TWO REASONS TO BE THE ONE: a unit's room is answered by its
// lead, any other named room by its most senior agent member. Saying which is
// what tells the seat whether the room is its own team's or one it was
// placed in, and so whether to answer itself or bring in whoever should.
func answererLine(kind Kind) string {
	why := "you are the most senior agent in this room"
	if kind == KindUnit {
		why = "you lead the unit this room belongs to"
	}
	return "A person spoke in this room without naming a colleague, and it " +
		"reached no other agent, so it comes to you: " + why + ". Answer it, " +
		"or bring in the colleague who should; a room where this resolves to " +
		"silence is a person talking to an empty screen."
}

// handling is what to do about it, with the tools this build ships.
//
// TWO BRANCHES, on the verdict the routing already reached rather than on the
// reason again: being asked and being told are what a recipient does
// differently, and the thirteen reasons collapse to those two.
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
			"are doing, say so: reply with `" + ReplyInThreadTool + "` where it was " +
			"said, rather than starting a new topic about it.\n"
	}
	b := strings.Builder{}
	b.WriteString("\n## How to handle this" +
		"\n1. **Answer where you were asked.** `" + ReplyInThreadTool + "` puts your " +
		"answer under the thread named above, beside the question, where the " +
		"next person to read it will find it. `" + PostMessageTool + "` starts a NEW " +
		"top-level message in the room, which is the wrong place for an " +
		"answer and reads as a second conversation.")
	if meta[notify.ThreadField] == "" {
		b.WriteString("\n   This message is top-level and has no thread yet: " +
			"replying under it is what creates one.")
	}
	b.WriteString(readFirst(meta))
	b.WriteString("\n3. **Take it out of the room only when it belongs out of it.** " +
		"`" + SendDMTool + "` reaches one person privately; use it for something that " +
		"is genuinely between the two of you, never to answer a question " +
		"that was asked in the open.\n")
	return b.String()
}

// readFirst is what the seat already holds of the conversation, and what it
// has to go and read.
//
// BRANCHED ON THE THREAD, because the two wakes hold different things. A THREAD
// REPLY'S THREAD IS ALREADY IN THE PROMPT: the turn-start prefetch read it
// through [ThreadReader] under `## The thread so far`, so sending the seat to
// read it spends a round on a second copy. What is left to read is what that
// block says it could not give: an unreadable thread, a left-out message, the
// exact words of a condensed middle, or the room outside the thread. A
// TOP-LEVEL MESSAGE opens no thread, so nothing before it was handed over and
// reading the room is the only way to it.
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
