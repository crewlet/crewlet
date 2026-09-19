package prefetch

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// The thread so far: what was already said where this turn was woken.
//
// # Why it is handed over rather than fetched
//
// A chat thread reply is usually thin — "yes", "+1", "what about the other
// one" — and the thread is the context. The engine used to say so in the
// prompt and tell the agent to go and read it with its chat tools. On a
// company whose chat tools come from a per-role MCP server that costs three
// rounds before a word is read: list the server's tools, activate one, and
// only then call it, with the schema arriving on the NEXT message. An agent
// that skipped the trip answered the eleven words of trigger text with no
// idea what the thread was about and reported the work delivered. Everything
// needed to just hand it over was already on the node — the channel and the
// thread root are on the trigger's own metadata, and each seat's
// authenticated client is held by its transport.
//
// # It does not turn the trigger thick
//
// [Request.RequiresRecon] still gates the three relevance filters on a chat
// thread reply, and deliberately: that flag describes the trigger BODY, which
// this block does not change, and the filters judge relevance against the
// trigger TEXT rather than against the thread. See [notify.ChatPrompt] for
// the other half of the argument, which is that the flag is stored on every
// historical event.

const (
	// threadPosts is how many messages the block renders.
	//
	// Thirty is more than the span of a thread a decision is actually made
	// in, and the root is always one of them however far back it is — so
	// the bite is on a long-running thread, where the newest thirty are
	// what the trigger is answering and the rest is history the seat's own
	// conversation ledger already carries.
	threadPosts = 30

	// threadMaxChars is the second bound, in bytes, enforced by dropping
	// WHOLE messages.
	//
	// One third of [ledger.InjectedMaxChars] (24000), and anchored there
	// on purpose: both blocks are frozen into the same system prompt and
	// re-sent on every round of every phase, and this one must not
	// dominate the one carrying the seat's own cross-turn history. A third
	// is roughly 2k tokens — thirty chat messages, which is the item cap
	// above, and the ceiling bites only on a thread whose messages are
	// documents rather than replies.
	//
	// NOT A CONFIG FIELD. config.ConversationSession's doc records what
	// happened the last time a render bound here was exposed: two knobs
	// were validated, defaulted and documented for a truncation that never
	// happened, because neither was threaded to a caller.
	threadMaxChars = 8000

	// ThreadTimeout bounds the read.
	//
	// ITS OWN, and deliberately NOT [AuxTimeout]: thirty seconds is
	// justified for an LLM round trip and is nowhere near justified here.
	// Five seconds is under the self-hosted client's own 10s retry budget,
	// so that client's automatic 429 retry cannot turn a prefetch into a
	// ten-second stall at the start of every chat turn — and the
	// prefetch's wall clock is its SLOWEST block, which is latency a
	// person is sitting through.
	ThreadTimeout = 5 * time.Second
)

// EmptyThreadHint is what the block says when the thread was read and had
// nothing in it.
const EmptyThreadHint = "(this thread has no earlier messages — the message " +
	"that woke you is all there is, so answer it on its own terms)"

// UnreadableThreadHint is what the block says when the thread could not be
// read from this node.
//
// A DIFFERENT SENTENCE from [EmptyThreadHint], for the reason
// [BuildingKnowledgeHint] differs from [EmptyKnowledgeHint]: the difference
// is what it tells the seat to do. "There is nothing earlier" says answer the
// trigger as it stands; this says the trigger is a fragment of a conversation
// nobody handed you, so go and read it before concluding anything — which is
// the one case where the old "go and look" instruction was right.
//
// It covers more than a failed request. A node in maintenance mode starts no
// chat transport, a self-hosted instance unreachable at boot leaves the
// company running without its chat surface, and a seat whose token was
// refused has no client at all. Every one of those renders this rather than
// nothing, because a seat handed no thread AND no instruction to find one is
// strictly worse off than it was before this block existed.
const UnreadableThreadHint = "(the thread this message is part of could not be " +
	"read from this node — you have only the triggering message, so read the " +
	"thread with your chat tools before answering anything that depends on it)"

// threadPreamble frames the block.
//
// Three facts, each of which the model gets wrong without being told: the
// list is the thread the trigger came from (not a search result), it is
// oldest-first (so the last line is what woke the turn), and the lines marked
// **you** are this seat's own earlier replies rather than a colleague's.
const threadPreamble = "The chat thread this turn was woken in, oldest first, " +
	"as it stood when the turn started. The newest messages are what woke " +
	"you. Lines marked **you** are your own earlier replies — do not answer " +
	"them or repeat what they already said."

// threadContext renders the block, and reports how many messages went into
// it.
//
// The COUNT is not derivable from the prose: both hints are non-empty, so
// without it telemetry cannot tell a thread that was handed over from one
// that could not be read — which is the whole question an operator asks when
// a seat answers a thread it evidently had not seen.
func (f *Fetcher) threadContext(ctx context.Context, r Request) (string, int) {
	if r.Thread.Root == "" {
		// Not a chat thread trigger at all: a top-level message, a
		// webhook, a scheduled fire. No block, not a hint — there is no
		// thread to be missing.
		return "", 0
	}
	if f.src.Threads == nil {
		return UnreadableThreadHint, 0
	}
	// ITS OWN DEADLINE, layered under the turn's: see [ThreadTimeout].
	read, cancel := context.WithTimeout(ctx, ThreadTimeout)
	defer cancel()
	messages, ok := f.src.Threads.ReadThread(read, r.Seat.Handle(), r.Thread)
	if !ok {
		return UnreadableThreadHint, 0
	}
	return f.renderThread(messages, r.Thread.Backend)
}

// renderThread bounds and renders what came back.
//
// WHOLE MESSAGES, oldest dropped first, and the drop is REPORTED — never a
// cut inside one, which would leave half of what somebody said reading as the
// whole of it. Two survivors are exempt from every bound: the ROOT, because
// it is what the thread is about and a thread rendered without it reads as a
// conversation starting mid-sentence, and the NEWEST, because it is the
// message that woke the turn.
//
// This is the rule [joinBullets] argues for and [ledger.RenderHistory]
// implements, applied to the one block whose items are somebody else's prose:
// the shared cuts are both wrong here, because [textcut] counts bytes and its
// own doc says content a turn reasons over is passed whole, and ledger.Elide
// says outright that it is not for content.
func (f *Fetcher) renderThread(messages []notify.Message, backend string) (string, int) {
	lines := make([]string, 0, len(messages))
	for _, m := range messages {
		if line := f.renderPost(m, backend); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return EmptyThreadHint, 0
	}

	dropped := 0
	if len(lines) > threadPosts {
		// The ROOT plus the newest threadPosts-1: the oldest reply is the
		// first thing worth losing, and the root is never in that range.
		dropped = len(lines) - threadPosts
		lines = append(lines[:1], lines[len(lines)-(threadPosts-1):]...)
	}
	// The byte ceiling then eats from the same end, and stops at two: the
	// root and the newest both survive however long they are.
	for len(lines) > 2 && len(strings.Join(lines, "\n")) > threadMaxChars {
		lines = append(lines[:1], lines[2:]...)
		dropped++
	}

	var b strings.Builder
	b.WriteString(threadPreamble)
	b.WriteString("\n" + lines[0])
	if dropped > 0 {
		// SAID OUT LOUD. A silently shortened thread reads as the whole
		// conversation, and a seat that believes it has seen everything
		// will not go and look for the rest.
		b.WriteString("\n_(" + strconv.Itoa(dropped) + " earlier message(s) in this " +
			"thread are not shown; read the thread itself with your chat tools if " +
			"you need them.)_")
	}
	for _, line := range lines[1:] {
		b.WriteString("\n" + line)
	}
	return b.String(), len(lines)
}

// renderPost renders one message as a bullet.
func (f *Fetcher) renderPost(m notify.Message, backend string) string {
	text := collapse(m.Text)
	if text == "" {
		return ""
	}
	return "- **" + f.speaker(m, backend) + "**: " + text
}

// speaker names who said it.
//
// THROUGH THE PARTY REGISTRY, the same call [notify.ChatPrompt.Build] makes
// for the triggering message's own sender, so a colleague reads as "Ana Ruiz
// (ana)" in both places rather than as a colleague in one and a 26-character
// opaque id in the other.
//
// A MISS IS ORDINARY — most people in a shared channel are not colleagues —
// and it falls through to whatever the backend volunteered and then to the
// raw platform id. Never blank: an unattributed line in a thread reads as the
// previous speaker continuing, which is how an agent comes to answer a
// question nobody asked it.
func (f *Fetcher) speaker(m notify.Message, backend string) string {
	if m.Own {
		return "you"
	}
	if f.src.Parties != nil && backend != "" && m.SenderID != "" {
		if party, ok := f.src.Parties.ByExternalID(backend, m.SenderID); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	if name := strings.TrimSpace(m.SenderName); name != "" {
		return name
	}
	if id := strings.TrimSpace(m.SenderID); id != "" {
		return id
	}
	return "unknown"
}
