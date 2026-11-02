package notify

import (
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// ChatPrompt is the prompt every chat backend shares.
//
// Chat backends deliver the same KIND of event — a person said something in
// a channel, a direct message or a thread — and the hard part is the same
// for all of them: deciding whether the message was actually addressed to
// this agent, and staying silent when it was not. That triage guidance is
// backend-neutral, so it exists once.
//
// What genuinely differs is small and mechanical: how a mention is written,
// which collective addresses exist, and how a direct conversation is
// recognised. A backend supplies those as fields.
//
// THE ENGINE STATES ONLY WHAT THE MODEL CANNOT KNOW: who is speaking, how this
// seat is addressed here, where a reply goes and why the message reached it.
// How to hold a conversation is the model's own, and what a seat must do when
// it declines a direct ask is the executor's contract, enforced at decode
// time — so neither is restated here.
//
// NOTHING HERE NAMES A TOOL. The prompt describes the capability and lets
// the model pick the matching tool out of its own catalogue, because the
// deployed MCP server's tool names are not knowable by the engine — see
// docs/concepts/tool-capabilities.md.
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
// The triggering message is usually thin — "yes", "+1", "what about the other
// one" — and the thread is the context. A top-level message or a direct
// message carries its own body and is no such pointer.
//
// IT STAYS TRUE NOW THAT THE ENGINE HANDS THE THREAD OVER at turn start (see
// internal/agent/prefetch's thread block), and the two reasons are worth
// having written down because the opposite reads as the obvious tidy-up.
//
// First, this flag describes the trigger BODY, which the thread block does
// not change. What it gates are the three prefetches that judge relevance
// against the trigger TEXT — the memory filter, the knowledge query, the
// episode vector search — and "+1" is exactly as useless a query with the
// thread in the system prompt as it was without it. Flipping the flag would
// turn two auxiliary LLM calls back on for every chat thread reply in the
// company, to search on a string that has not improved.
//
// Second, the flag is not only read here. It is stamped at parse time, merged
// across a coalesced trigger, STORED on every event as
// types.InboundInteraction.RequiresRecon and reported on prefetch_summary,
// where the dashboard reads it to say "gated" rather than "broken". Changing
// what it means for chat would silently rewrite what every past turn in the
// store claims about itself.
//
// CHATLET IS THE ONE SOURCE THAT ANSWERS FALSE with a thread, and it
// overrides this rather than weakening it: its trigger is never a pointer at a
// message somewhere else. The record carries the message itself — what was
// said, who said it, in which room and why it reached this seat, copied at
// write time — so there is nothing to reconcile before the seat can reason,
// and the prefetches this gates have a real query. The THREAD around it is not
// on the record: it reaches the seat the way a vendor thread does, through the
// prefetch's thread block over Chatlet's own thread reader, so one
// mechanism serves every chat surface and a thread is never in the prompt
// twice. See internal/chatlet's prompt.
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
// In a direct conversation a person's consecutive TOP-LEVEL messages are one
// partition, so they key on the channel alone and a typing burst coalesces
// into one turn — the headline case for coalescing at all.
//
// A direct THREAD REPLY keeps its thread key: merging it with unrelated
// top-level pings would hand the turn one merged metadata whose thread
// pointer names only one of two reply targets, steering the digest's reply
// to the wrong place.
//
// In a shared channel two unrelated top-level asks must NOT merge, so the
// key stays thread-grained throughout — a reply carries the root's id, and a
// top-level message keys on its OWN id so its later replies land in the same
// partition.
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

// ConversationIdentity implements [Prompt]. THIS IS ONE OF THE TWO SOURCES
// WHERE THE TWO ANSWERS DIFFER (Worklet's is the other), and the
// invariant is what decides how.
//
// A DIRECT CONVERSATION IS ONE CONVERSATION, thread or no thread: the whole
// DM channel, regardless of thread_ts. What forces that is
// [Prompt.ConversationIdentity]'s invariant — the identity must be constant
// across every event in a partition. A top-level DM burst partitions on the
// bare channel while its constituents carry different ts values, so an
// identity of "channel:anchor" would give that ONE partition three different
// identities and the ledger key would fall to whichever event sorted first.
// The channel is the only value every constituent of that partition agrees
// on.
//
// It is also what a person means. A DM is a 1:1 line that runs for months;
// the thread a reply hangs off is a formatting detail of the transport, and
// keying history on it filed the answer to "what did we say yesterday" under
// an address the next turn never looked up.
//
// ELSEWHERE THE ANSWER IS THE PARTITION KEY: in a shared channel a thread IS
// the conversation, and the partition is already thread-grained, so the two
// coincide and the invariant holds trivially.
//
// WHERE AN ANCHOR IS USED AT ALL — the non-direct branch, which is
// [ChatPrompt.PartitionKey] — IT IS THE SAME ONE: thread_ts falling back to
// the message's own ts, which is what [ConversationOf] resolves for the
// working indicator and what [ChatPrompt.Build] prints as the thread to
// "reply as a thread" in. Three derivations of one anchor that agree only by
// inspection is how the spinner a person is watching, the thread the reply
// lands in and the ledger the seat reads come to name three different
// threads.
//
// THE DIRECT BRANCH USES NO ANCHOR, deliberately, and that is not a fourth
// derivation drifting from the other three — it is the answer to a different
// question. The indicator and the reply target are about ONE MESSAGE: which
// thread to raise a spinner in, which thread to post into. Both are still the
// anchor on a direct message, and still that one. The identity is about the
// LINE the message is on, and on a direct message the line is the channel. So
// here the three must NOT agree: an identity carrying the anchor would give
// one top-level DM partition as many identities as it has constituents, and
// would file the seat's own history under a thread its next turn never looks
// up — which is the failure this branch exists to remove.
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
// IT NO LONGER SENDS THE AGENT TO GO AND READ THE THREAD. That instruction
// cost three rounds on a company whose chat tools come from a per-role MCP
// server (list the server's tools, activate one, then call it, with the schema
// arriving on the next message), and an agent that skipped it answered eleven
// words of trigger text having no idea what the thread was about. The thread
// is now in the system prompt, under `## The thread so far` — a DIFFERENT
// heading from this section's, because this one lands in the user message and
// two sections with one name saying different things is a model reading
// whichever it saw last.
//
// What stays is the one fact the model cannot work out for itself: which of
// the thread's messages are its own, which is what stops a seat answering
// itself. How to hold up its end of a conversation — not repeating a take it
// already gave, letting a colleague finish — is the model's own, and telling
// a seat to stay silent on an acknowledgement was telling it to do what the
// engine refuses: a reply in a followed thread is addressed, and an addressed
// turn may not end in silence.
func threadBlock(self string) string {
	reference := self
	if reference == "" {
		reference = "your own account"
	}
	return "\n## Thread context" +
		"\nThis message is a reply in a thread. Messages from " + reference +
		" in this thread are YOUR previous replies."
}
