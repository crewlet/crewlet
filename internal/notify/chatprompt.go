package notify

import (
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// ChatPrompt is the prompt every chat backend shares.
//
// The triage (was this addressed to this agent? else stay silent) is
// backend-neutral, so it exists once; a backend supplies how a mention is
// written, its collective addresses and how a direct conversation is spotted.
//
// THE ENGINE STATES ONLY WHAT THE MODEL CANNOT KNOW: who is speaking, how this
// seat is addressed here, where a reply goes and why the message reached it.
// A declined direct ask is the executor's contract, enforced at decode time.
//
// NOTHING HERE NAMES A TOOL: the engine cannot know the MCP server's tool
// names (docs/concepts/tool-capabilities.md).
type ChatPrompt struct {
	// Backend is the transport key this prompt answers for.
	Backend string

	// Label is the backend's name as it appears in prose.
	Label string

	// Address is how this backend decides a message was addressed to the
	// seat — the SAME value its transport hands the working indicator,
	// because the prompt telling an agent it was asked and the indicator
	// telling a person it is working must not be two opinions.
	Address AddressRule

	// Collectives is this backend's everyone-in-the-room addresses, as
	// rendered prose: "`@channel` / `@here`".
	Collectives string

	// SelfReference is how this agent is addressed here — a user id in
	// markup, a literal username. Rendered into the triage block so the
	// agent recognises a mention of itself, and into the thread block so
	// it recognises its own earlier replies. Empty is valid: an identity
	// that has not resolved yet.
	SelfReference func(metadata map[string]string) string

	// MentionHint is one line telling the agent how to WRITE a mention on
	// this backend, which is not always how it reads one.
	MentionHint func(metadata map[string]string) string

	// IdentityNote optionally names the agent's own id here.
	IdentityNote func(metadata map[string]string) string

	// Author is who wrote the message, as its fence names them. Nil resolves
	// the sender's id on this backend through the party registry, which is a
	// vendor's whole answer: an id no seat holds is an outside party. A
	// backend whose record states the author's kind (the engine's own chat)
	// answers from it for a sender the registry no longer names.
	Author func(metadata map[string]string, parties Parties) provenance.Author

	// Hosted says this backend is one the engine hosts itself, where every
	// message's sender authenticated as a company principal: a message whose
	// sender nobody can name reads as a colleague's
	// ([provenance.TrustForHosted]). A vendor's backend leaves it false, and
	// there such a message is an outside party's.
	Hosted bool
}

// Source implements [Prompt].
func (p ChatPrompt) Source() string { return p.Backend }

// WakesActor implements [Prompt]: never.
//
// A chat backend echoes an agent's own message back to it, and the parser
// suppresses that at the door. Nothing a chat backend emits is an OUTCOME of
// the actor's own action that the actor does not already know — which is the
// only thing the exception is for.
func (ChatPrompt) WakesActor(Inbound) bool { return false }

// DigestBody implements [Prompt]: every constituent is kept.
//
// A chat backend has no supersede rule, because every event it emits IS a
// message. The rule exists for a third-party app that re-emits its whole current
// state on every field change; a person typing four times has said four
// things.
func (ChatPrompt) DigestBody(_, body string) string { return body }

// RequiresRecon implements [Prompt]: a thread reply is a POINTER.
//
// The trigger is usually thin ("+1"); a top-level or direct message is not.
// IT STAYS TRUE though the engine hands the thread over at turn start
// (internal/agent/prefetch): the flag describes the trigger BODY and gates the
// prefetches that query on trigger TEXT, which "+1" spoils with or without the
// thread. It is also stamped at parse, merged across a coalesced trigger,
// STORED as types.InboundInteraction.RequiresRecon and shown on
// prefetch_summary, so changing it rewrites what past turns claim.
// CHATLET ANSWERS FALSE with a thread and overrides this: its record carries
// the message itself, so the prefetches have a real query.
func (ChatPrompt) RequiresRecon(n Inbound) bool { return n.Metadata[ThreadField] != "" }

// SubjectIsLabel implements [Prompt]: always. A chat message has no subject
// of its own — the parser names the surface ("Slack message", "Mattermost
// message"), which is the same on every message and says nothing of what was
// said, so the turn's ask is the message alone.
func (ChatPrompt) SubjectIsLabel(Inbound) bool { return true }

// Addressed implements [Prompt] through the same rule the working-status
// indicator uses — the backend's own [AddressRule], read here rather than
// re-derived, so the two cannot disagree by construction. [Build]
// reads the same rule, through [addressOf], to tell the seat which it is.
func (p ChatPrompt) Addressed(n Inbound) bool { return p.Address.Addressed(n.Metadata) }

// PartitionKey implements [Prompt].
//
// In a direct conversation a person's consecutive TOP-LEVEL messages key on
// the channel alone, so a typing burst coalesces into one turn.
//
// A direct THREAD REPLY keeps its thread key: merged with unrelated top-level
// pings, the merged metadata's thread pointer would name only one of two
// reply targets and misdirect the digest's reply.
//
// In a shared channel two unrelated top-level asks must NOT merge, so the key
// stays thread-grained: a reply carries the root's id, and a top-level
// message keys on its OWN id so its later replies join the partition.
func (p ChatPrompt) PartitionKey(metadata map[string]string, _ string) string {
	channel := metadata[ChannelField]
	if channel == "" {
		// No channel, no conversation identity — and a key that was
		// just the thread would collide across channels.
		return ""
	}
	if metadata[ThreadField] == "" && p.Address.IsDirect(metadata) {
		return channel
	}
	anchor := Anchor(metadata)
	if anchor == "" {
		return ""
	}
	return channel + ":" + anchor
}

// ConversationIdentity implements [Prompt]; with Worklet's, one of the two
// where it differs from the partition key. A DIRECT CONVERSATION IS ONE
// CONVERSATION, thread or no thread: the DM channel. The identity must be
// constant across a partition, and a top-level DM burst partitions on the
// channel while its events carry different ts values, so "channel:anchor"
// would file its executor-log rows under whichever event sorted first.
//
// ELSEWHERE IT IS THE PARTITION KEY: a thread IS the conversation, and its
// anchor is the one [ConversationOf] and [ChatPrompt.Build] use (thread_ts,
// else the message's ts), or spinner, reply and next turn name different
// threads. THE DIRECT BRANCH USES NO ANCHOR: the identity is about the LINE.
func (p ChatPrompt) ConversationIdentity(metadata map[string]string, subject string) string {
	channel := metadata[ChannelField]
	if channel == "" {
		return ""
	}
	// THROUGH THE BACKEND'S OWN RULE, not a predicate of this prompt's:
	// what counts as direct is a fact about the surface the message came
	// from, and [AddressRule] is the one value that states it. A second
	// answer here is how the delivery obligation and the partition key
	// came to disagree about the same message.
	if p.Address.IsDirect(metadata) {
		return channel
	}
	return p.PartitionKey(metadata, subject)
}

// Build implements [Prompt].
func (p ChatPrompt) Build(n Inbound, parties Parties) string {
	meta := n.Metadata
	// An OR CHAIN rather than a lookup default: a transport always writes
	// the sender key, so an absent sender arrives as an empty string and
	// a default would never fire — leaving the prompt to say "posted by
	// ****" and render a blank From line.
	who := n.Sender
	if who == "" {
		who = meta[UserField]
	}
	if who == "" {
		who = "unknown"
	}
	// Annotate a known colleague, and a HUMAN one especially: the agent
	// then treats the counterparty as a person — who replies on their own
	// time and cannot be reached by an agent-to-agent ask — rather than as
	// an opaque platform id.
	if parties != nil {
		if party, ok := parties.ByExternalID(p.Backend, meta[UserField]); ok {
			if label := party.Label(); label != "" {
				who = label
				// THE ID BESIDE THE NAME, which is what this surface's
				// chat tools take to mention them — unless the id is the
				// handle, as it is on the engine's own chat, which the
				// label already carries.
				if id := meta[UserField]; id != party.Handle {
					who = label + " — `" + id + "`"
				}
			}
		}
	}

	handle := meta[RecipientField]
	if handle == "" {
		handle = "your handle"
	}

	var b strings.Builder
	label := p.Label
	if label == "" {
		label = "chat"
	}
	b.WriteString("A " + label + " message was posted by **" + who + "**.")
	b.WriteString(" Your handle is `" + handle + "`.")
	if p.IdentityNote != nil {
		if note := p.IdentityNote(meta); note != "" {
			b.WriteString("\n" + note)
		}
	}

	var self string
	if p.SelfReference != nil {
		self = p.SelfReference(meta)
	}
	// THE ENGINE'S VERDICT WHERE IT HAS ONE, AND THE TRIAGE ONLY WHERE IT
	// DOES NOT. An addressed message owes an answer — the delivery check
	// refuses a silent end to it — so on a direct message, a mention or a
	// followed thread the "were you asked?" question is already settled,
	// and asking it anyway told a seat it might stay silent on exactly the
	// messages it may not. The triage is for what is left: a message in an
	// open channel, which the seat may rightly leave alone.
	switch addressOf(meta, p.Address) {
	case addressDirect:
		b.WriteString("\n**Addressed to you:** this is a direct message.")
	case addressMention:
		b.WriteString("\n**Addressed to you:** the message names you.")
	case addressThread:
		b.WriteString("\n**Addressed to you:** you follow this thread.")
	case addressRouted:
		b.WriteString("\n**Addressed to you:** the message was routed to you to answer.")
	default:
		marker := "`" + handle + "`"
		if self != "" {
			marker = self + " or " + marker
		}
		b.WriteString(p.triage(marker))
	}

	thread := meta[ThreadField]
	if thread != "" {
		b.WriteString(threadBlock(self))
	}

	if n.Body == "" {
		b.WriteString("\n\n**Message:** (empty)")
	} else {
		// ALREADY FENCED as what somebody said ([Render]), under this
		// prompt's own [ChatPrompt.Provenance].
		b.WriteString("\n\n**Message:**\n" + n.Body)
	}
	b.WriteString("\n**From:** " + who)
	if channel := meta[ChannelField]; channel != "" {
		// The NAME beside the id where the backend supplies one: "eng"
		// is what a person would say and what the agent has to
		// recognise in the conversation, and the id is what its chat
		// tools take. Rendering only the id makes the trigger name a
		// room nobody can place.
		if name := meta[ChannelNameField]; name != "" {
			channel = name + " (`" + channel + "`)"
		}
		b.WriteString("\n**Channel:** " + channel)
	}
	// Through [Anchor], which is the same value the working indicator
	// raises on: the reply and the spinner must land in one place.
	switch anchor := Anchor(meta); {
	case anchor == "":
	case thread != "":
		b.WriteString("\n**Thread:** " + anchor + " (existing thread)")
	default:
		b.WriteString("\n**Thread:** " + anchor +
			" (top-level message — reply as a thread)")
	}
	if ts := meta[MessageIDField]; ts != "" {
		b.WriteString("\n**Message id:** " + ts + " — for acting on this" +
			" message itself (reacting to it) rather than replying.")
	}
	if meta[ReplayedField] != "" {
		// A message re-read over the backend's API across a reconnect
		// gap. How stale the conversation is changes what the seat
		// should do about it — somebody may have answered already —
		// and the parser stamping the fact is worth nothing if the
		// trigger never says it.
		b.WriteString("\n**Note:** this message was re-read after a connection" +
			" gap rather than delivered live, so the conversation may have" +
			" moved on since. Check the thread before replying.")
	}
	if p.MentionHint != nil {
		if hint := p.MentionHint(meta); hint != "" {
			b.WriteString("\n" + hint)
		}
	}
	return b.String()
}

// Provenance implements [Prompt]: a message in a room or a thread, written by
// its sender — every chat event IS a message, and its author is who sent it —
// on a surface the engine hosts or a vendor's ([ChatPrompt.Hosted]).
func (p ChatPrompt) Provenance(n Inbound, parties Parties) Provenance {
	return Provenance{
		Source:  provenance.SourceChatThread,
		Authors: []provenance.Author{p.author(n.Metadata, parties)},
		Hosted:  p.Hosted,
	}
}

// author is [ChatPrompt.Author], or the registry's reading of the sender's id.
func (p ChatPrompt) author(meta map[string]string, parties Parties) provenance.Author {
	if p.Author != nil {
		return p.Author(meta, parties)
	}
	if parties != nil {
		if party, ok := parties.ByExternalID(p.Backend, meta[UserField]); ok {
			return PartyAuthor(party)
		}
	}
	return provenance.Author{}
}

// triage is the addressee guidance, identical on every backend, for a message
// the engine has not already read as addressed to the seat.
//
// A PRINCIPLE, NOT A METHOD. It was a numbered procedure with worked examples
// ("Engineers, welcome @newhire" → …), and a current model tells who a message
// is for as well without them — while the examples read to the turn-start
// relevance filters as people and roles the task involved. What the model
// cannot know is how THIS seat is addressed here (its marker, the backend's
// collectives), and that silence is the expected answer to most of what an
// open channel carries: a company where every agent answers every message is
// unusable within an hour, and nothing but this text keeps an unaddressed
// seat quiet.
func (p ChatPrompt) triage(marker string) string {
	collectives := p.Collectives
	if collectives == "" {
		collectives = "`@channel` / `@here`"
	}
	return "\n## Triage — decide BEFORE replying" +
		"\nRead whom the message asks to act — the verb, not just the names" +
		" in it." +
		"\n- **You:** it names you — " + marker + ", or your role in plain" +
		" text as the one to act → answer." +
		"\n- **A group you belong to:** " + collectives + `, "everyone",` +
		` "team", or a role group such as "engineers" → answer only with a` +
		" specific contribution of your own." +
		"\n- **Someone else, or nobody:** an FYI, an announcement, your role" +
		" or team named only as its subject → silence beats noise: stay" +
		" silent unless you hold information that changes the outcome."
}

// threadBlock marks a thread reply, identical on every backend.
//
// IT DOES NOT SEND THE AGENT TO READ THE THREAD: the thread is in the system
// prompt under `## The thread so far`, a DIFFERENT heading from this
// section's (which lands in the user message), because two sections with one
// name saying different things make a model read whichever it saw last.
//
// What stays is the one fact the model cannot work out: which thread messages
// are its own, so a seat does not answer itself. Holding up its end of a
// conversation is the model's own; telling a seat to stay silent on an
// acknowledgement would contradict the engine, since a reply in a followed
// thread is addressed and an addressed turn may not end in silence.
func threadBlock(self string) string {
	reference := self
	if reference == "" {
		reference = "your own account"
	}
	return "\n## Thread context" +
		"\nThis message is a reply in a thread. Messages from " + reference +
		" in this thread are YOUR previous replies."
}
