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

// threadStoppedShortPreamble replaces it when the backend could not reach the
// end of the thread.
//
// THE ORDINARY PREAMBLE IS A LIE ON THIS PATH, and the worst available one:
// it tells the model the newest messages are what woke it, and a read that
// stopped short is missing precisely the newest — the triggering message
// included, because a chat backend pages a thread from the OLDEST end (see
// [notify.Transcript.StoppedShort]). A seat told otherwise answers the last
// message it happens to have and reports the work delivered, which is the
// exact failure this whole block exists to stop.
//
// So it keeps the two facts that are still true — which thread this is, and
// what the **you** lines are — drops the one that is not, and spends the
// difference on the instruction that IS right here: go and read the rest.
const threadStoppedShortPreamble = "Part of the chat thread this turn was " +
	"woken in, oldest first. IT STOPS SHORT: this thread is longer than " +
	"could be read from here, so the newest messages — including the one " +
	"that woke you — are NOT below. Read the rest of the thread with your " +
	"chat tools before answering anything that depends on it. Lines marked " +
	"**you** are your own earlier replies — do not answer them or repeat " +
	"what they already said."

// threadBlock is what the block resolved to.
//
// FOUR FACTS, because the prose carries none of them legibly. Every path here
// renders non-empty text — two of them a hint rather than a thread — so a
// reader outside this package cannot tell "handed the conversation" from
// "told to go and find it" from "there was nothing to find" by looking at it,
// and a count alone collapses the last two into the first's opposite.
type threadBlock struct {
	// text is what lands in the prompt.
	text string
	// posts is how many messages were rendered.
	posts int
	// read says a backend answered, whatever it had to say.
	read bool
	// stoppedShort says the answer stops short of the thread's newest
	// message. See [notify.Transcript.StoppedShort].
	stoppedShort bool
}

// threadContext renders the block.
//
// The three states a caller has to keep apart are decided HERE, where the
// difference is still visible, rather than inferred later from a number: a
// trigger with no thread renders nothing, a read that failed renders the
// unreadable hint, and a read that succeeded renders the thread or the empty
// hint — and only the last two read anything at all.
func (f *Fetcher) threadContext(ctx context.Context, r Request) threadBlock {
	if r.Thread.Root == "" {
		// Not a chat thread trigger at all: a top-level message, a
		// webhook, a scheduled fire. No block, not a hint — there is no
		// thread to be missing, and nothing read one.
		return threadBlock{}
	}
	if f.src.Threads == nil {
		return threadBlock{text: UnreadableThreadHint}
	}
	// ITS OWN DEADLINE, layered under the turn's: see [ThreadTimeout].
	read, cancel := context.WithTimeout(ctx, ThreadTimeout)
	defer cancel()
	transcript, ok := f.src.Threads.ReadThread(read, r.Seat.Handle(), r.Thread)
	if !ok {
		return threadBlock{text: UnreadableThreadHint}
	}
	return f.renderThread(transcript, r.Thread.Backend)
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
func (f *Fetcher) renderThread(read notify.Transcript, backend string) threadBlock {
	lines := make([]string, 0, len(read.Messages))
	for _, m := range read.Messages {
		if line := f.renderPost(m, backend); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 && !read.StoppedShort {
		// READ, and it had nothing in it. Reported as a read rather than
		// as a zero, because the same zero from a failed read sends the
		// seat to the opposite place and an operator after the wrong
		// fault.
		return threadBlock{text: EmptyThreadHint, read: true}
	}

	// THE BACKEND'S OWN DROPS COUNT TOO. A paged read holds a window and
	// says how many older messages it let go of; counting only this
	// renderer's own would print a number that is true of the slice in
	// hand and false of the thread, which is worse than no number at all —
	// a seat reads it as the whole of what it is missing.
	dropped := read.Older
	if len(lines) > threadPosts {
		// The ROOT plus the newest threadPosts-1: the oldest reply is the
		// first thing worth losing, and the root is never in that range.
		dropped += len(lines) - threadPosts
		lines = append(lines[:1], lines[len(lines)-(threadPosts-1):]...)
	}
	// The byte ceiling then eats from the same end, and stops at two: the
	// root and the newest both survive however long they are.
	for len(lines) > 2 && len(strings.Join(lines, "\n")) > threadMaxChars {
		lines = append(lines[:1], lines[2:]...)
		dropped++
	}

	var b strings.Builder
	if read.StoppedShort {
		b.WriteString(threadStoppedShortPreamble)
	} else {
		b.WriteString(threadPreamble)
	}
	// Each part is written only if it exists, because a read that stopped
	// short before anything renderable leaves the preamble standing alone
	// — and that preamble is itself the instruction the seat needs.
	if len(lines) > 0 {
		b.WriteString("\n" + lines[0])
	}
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
	return threadBlock{
		text: b.String(), posts: len(lines),
		read: true, stoppedShort: read.StoppedShort,
	}
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
