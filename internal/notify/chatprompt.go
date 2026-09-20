package notify

import "strings"

// ChatPrompt is the prompt every chat backend shares.
//
// Chat backends deliver the same KIND of event — a person said something in
// a channel, a direct message or a thread — and the hard part is the same
// for all of them: deciding whether the message was actually addressed to
// this agent, and staying silent when it was not. That triage guidance is
// the bulk of the prompt and is backend-neutral, so it exists once.
//
// What genuinely differs is small and mechanical: how a mention is written,
// which collective addresses exist, and how a direct conversation is
// recognised. A backend supplies those as fields.
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
}

// Source implements [Prompt].
func (p ChatPrompt) Source() string { return p.Backend }

// WakesActor implements [Prompt]: never.
//
// A chat backend echoes an agent's own message back to it, and the parser
// suppresses that at the door. Nothing a chat backend emits is an OUTCOME of
// the actor's own action that the actor does not already know — which is the
// only thing the exception is for.
func (ChatPrompt) WakesActor(string) bool { return false }

// DigestBody implements [Prompt]: every constituent is kept.
//
// A chat backend has no supersede rule, because every event it emits IS a
// message. The rule exists for a third-party app that re-emits its whole current
// state on every field change; a person typing four times has said four
// things.
func (ChatPrompt) DigestBody(_, body string) string { return body }

// RequiresRecon implements [Prompt]: a thread reply is a POINTER.
//
// The prompt tells the agent to read the thread before responding, because
// the triggering message is usually thin — "yes", "+1", "what about the
// other one" — and the thread is the context. A top-level message or a
// direct message carries its own body and needs no such trip.
func (ChatPrompt) RequiresRecon(n Inbound) bool { return n.Metadata[ThreadField] != "" }

// Addressed implements [Prompt] through the same rule the working-status
// indicator uses — the backend's own [AddressRule], read here rather than
// re-derived, so the two cannot disagree by construction.
func (p ChatPrompt) Addressed(n Inbound) bool { return p.Address.Addressed(n.Metadata) }

// ConversationKey implements [Prompt].
//
// In a direct conversation a person's consecutive TOP-LEVEL messages are one
// conversation, so they key on the channel alone and a typing burst
// coalesces into one turn — the headline case for coalescing at all.
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
func (p ChatPrompt) ConversationKey(metadata map[string]string, _ string) string {
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
				who = label + " — `" + meta[UserField] + "`"
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
	marker := "`" + handle + "`"
	if self != "" {
		marker = self + " or " + marker
	}
	b.WriteString(p.triage(marker))

	thread := meta[ThreadField]
	if thread != "" {
		b.WriteString(threadBlock(self))
	}

	body := n.Body
	if body == "" {
		body = "(empty)"
	}
	b.WriteString("\n\n**Message:** " + body)
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
		b.WriteString("\n**Message id:** " + ts + " — the reference for acting on" +
			" *this* message rather than replying to it (for example reacting" +
			" to it) with your chat tools.")
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

// triage is the addressee guidance, identical on every backend.
//
// It is the bulk of the prompt because it is the hard part: a bot in a busy
// channel is woken by everything, and a company where every agent answers
// every message is unusable within an hour.
func (p ChatPrompt) triage(marker string) string {
	collectives := p.Collectives
	if collectives == "" {
		collectives = "`@channel` / `@here`"
	}
	return "\n## Triage — decide BEFORE replying" +
		"\n" +
		"\n**1. Find the addressee — who is the message asking to act?**" +
		"\n- **Personal:** the message names you — " + marker +
		`, or your role-name (e.g. "PM", "the CTO") in plain text.` +
		"\n- **A group you belong to:** " + collectives +
		` / 'everyone' / 'team', or a role-group like "engineers",` +
		` "leadership", "PMs".` +
		"\n- **Someone else:** a specific other person or role the message" +
		" is directing." +
		"\n- **No one (informational):** announcements, FYIs, status" +
		" updates, welcomes or kudos *about* someone." +
		"\n" +
		"\n**Distinguish addressee from subject.** A mention can be either" +
		" — read the verb, not just the `@`." +
		"\n- \"Engineers, welcome @newhire\" → addressee = engineers;" +
		" @newhire is the subject (the new hire)." +
		"\n- \"@PM open a ticket for @SWE\" → addressee = PM;" +
		" @SWE is the subject (assignee mentioned for context)." +
		"\n- \"Thanks @SWE for the fix!\" → no addressee; @SWE is the" +
		" subject of kudos. Don't reply." +
		"\n" +
		"\n**2. Decide.**" +
		"\n- Personal addressee → respond." +
		"\n- In an addressed group → respond only if you have a specific," +
		" substantive contribution. For *action requests* the" +
		" narrowest-matching role should answer (\"engineers\" + an auth" +
		" question → the engineer who owns auth, not every engineer). For" +
		" *social* messages (welcomes, kudos) a brief reply from anyone in" +
		" the group is fine." +
		"\n- Addressee is someone else, or the message is informational →" +
		" stay silent unless you hold decision-changing information they" +
		" cannot see." +
		"\n" +
		"\n**3. Default for non-addressed messages: silence beats noise.**" +
		" When you weren't being asked — informational, a passing" +
		" reference, the addressee is clearly someone else — stay silent." +
		"\n" +
		"\n**4. When the message names you SPECIFICALLY** (not just as part" +
		" of an addressed group) **and you have decided not to act** — out" +
		" of scope, already handled, deferring to someone else → do NOT" +
		" skip silently. Post a brief reply in the same thread or channel;" +
		" one sentence is enough (\"Not my area — try whoever owns auth.\"," +
		" \"Already handled in <link>.\"). Group pings still allow silence" +
		" when you have nothing substantive; this covers personal pings" +
		" only — leaving a direct 1:1 ping unanswered looks like the" +
		" message was lost."
}

// threadBlock is the thread-reply guidance, identical on every backend.
func threadBlock(self string) string {
	reference := self
	if reference == "" {
		reference = "your own account"
	}
	return "\n## Thread context" +
		"\nThis is a thread reply. Read the thread with your chat tools" +
		" before responding; focus on the triggering message and treat the" +
		" rest as background." +
		"\n" +
		"\n**Self-check before replying.** Messages from " + reference +
		" in this thread are YOUR previous replies." +
		"\n- If you already gave your take on this question, do not repeat it." +
		"\n- If the new message is acknowledgement, status or agreement and" +
		" asks you nothing new, stay silent." +
		"\n- If another agent is updating their own progress rather than" +
		" asking you, stay silent — let them finish." +
		"\nEach fresh reply must answer a NEW question or make a NEW decision."
}
