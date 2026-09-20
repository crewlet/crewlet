package notify

import (
	"slices"
	"strings"
)

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

	// DirectKinds are the channel_type values meaning a direct
	// conversation.
	DirectKinds []string

	// DMPrefix marks a direct message unambiguously by channel id, or is
	// empty where a backend's ids are opaque. See [Addressed] for why it
	// must stay empty in that case.
	DMPrefix string

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
func (ChatPrompt) RequiresRecon(n Inbound) bool { return n.Metadata["thread_ts"] != "" }

// Addressed implements [Prompt] through the same rule the working-status
// indicator uses.
//
// ONE IMPLEMENTATION, deliberately: the indicator says "this agent is working
// on your message" and the delivery check says "this agent owes your message
// an answer", and the two disagreeing would raise a spinner on a turn allowed
// to end in silence, or end one in silence after raising a spinner.
func (p ChatPrompt) Addressed(n Inbound) bool { return Addressed(n.Metadata, p.DMPrefix) }

// IsDirect reports whether an event happened in a direct conversation.
func (p ChatPrompt) IsDirect(metadata map[string]string) bool {
	if slices.Contains(p.DirectKinds, metadata["channel_type"]) {
		return true
	}
	return p.DMPrefix != "" && strings.HasPrefix(metadata["channel"], p.DMPrefix)
}

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
	channel := metadata["channel"]
	if channel == "" {
		// No channel, no conversation identity — and a key that was
		// just the thread would collide across channels.
		return ""
	}
	thread := metadata["thread_ts"]
	if thread == "" && p.IsDirect(metadata) {
		return channel
	}
	anchor := thread
	if anchor == "" {
		anchor = metadata["ts"]
	}
	if anchor == "" {
		return ""
	}
	return channel + ":" + anchor
}

// ConversationIdentity implements [Prompt]. THIS IS THE ONE SOURCE WHERE THE
// TWO ANSWERS DIFFER, and the invariant is what decides how.
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
	channel := metadata["channel"]
	if channel == "" {
		return ""
	}
	if p.IsDirect(metadata) {
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
		who = meta["user"]
	}
	if who == "" {
		who = "unknown"
	}
	// Annotate a known colleague, and a HUMAN one especially: the agent
	// then treats the counterparty as a person — who replies on their own
	// time and cannot be reached by an agent-to-agent ask — rather than as
	// an opaque platform id.
	if parties != nil {
		if party, ok := parties.ByExternalID(p.Backend, meta["user"]); ok {
			if label := party.Label(); label != "" {
				who = label + " — `" + meta["user"] + "`"
			}
		}
	}

	handle := meta["recipient_handle"]
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

	thread := meta["thread_ts"]
	if thread != "" {
		b.WriteString(threadBlock(self))
	}

	body := n.Body
	if body == "" {
		body = "(empty)"
	}
	b.WriteString("\n\n**Message:** " + body)
	b.WriteString("\n**From:** " + who)
	if channel := meta["channel"]; channel != "" {
		b.WriteString("\n**Channel:** " + channel)
	}
	switch {
	case thread != "":
		b.WriteString("\n**Thread:** " + thread + " (existing thread)")
	case meta["ts"] != "":
		b.WriteString("\n**Thread:** " + meta["ts"] +
			" (top-level message — reply as a thread)")
	}
	if ts := meta["ts"]; ts != "" {
		b.WriteString("\n**Message id:** " + ts + " — the reference for acting on" +
			" *this* message rather than replying to it (for example reacting" +
			" to it) with your chat tools.")
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
// Everything else here stays: recognising the seat's own replies and not
// repeating a take it already gave are properties of the CONVERSATION rather
// than of where the thread came from, and they are what stop a seat answering
// itself.
func threadBlock(self string) string {
	reference := self
	if reference == "" {
		reference = "your own account"
	}
	return "\n## Thread context" +
		"\nThis is a thread reply. Focus on the triggering message and" +
		" treat the rest of the thread as background." +
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
