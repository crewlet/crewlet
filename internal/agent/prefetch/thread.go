package prefetch

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The thread so far: what was already said where this turn was woken.
//
// Handed over, not fetched: a thread reply is thin ("yes", "+1") and the
// thread is the context. Telling the agent to read it with its chat tools
// costs three rounds on a per-role MCP server, and an agent that skipped the
// trip answered with no idea what the thread was about. The channel and
// thread root are on the trigger's metadata and each seat's client is held by
// its transport, so the node can read it.

// Fenced: every message is somebody else's words, frozen into the system
// prompt of every round, so they sit in one [provenance.Fence] carrying the
// turn's token and the speakers as authors. The preamble and the engine's own
// notices stay outside it: they are the engine telling the seat how to read.
// It does not turn the trigger thick: [Request.RequiresRecon] still gates the
// relevance filters, because it describes the trigger BODY, which this block
// does not change. See [notify.ChatPrompt].

const (
	// threadMaxChars is the only bound, in bytes, on the block: past it the
	// root and the newest WHOLE messages fill three quarters and the middle
	// is condensed into the last quarter, never cut inside a message. A
	// message count would bound nothing the bytes do not.
	//
	// 8000 is roughly 2k tokens, about thirty ordinary chat messages, and a
	// third of internal/agent/prompts' EndingsMaxBytes (24 000): both ride
	// every request of the executor's session, and the thread must not crowd
	// out the account of what the conversation's turns already did.
	//
	// Not a config field: a bound belongs where the render is.
	threadMaxChars = 8000

	// threadEarlierShare is the part of threadMaxChars the condensed
	// account of the middle of a long thread is given — one quarter, so
	// the root and the messages the trigger is answering keep three
	// quarters of the room.
	threadEarlierShare = 4

	// ThreadTimeout bounds the read. Not [AuxTimeout]: the prefetch's wall
	// clock is its slowest block, which a person sits through.
	//
	//   - A vendor read is one HTTP call; 5s is under the self-hosted
	//     client's 10s retry budget, so its 429 retry cannot stall every turn.
	//   - The engine's own chat is a linearizable estate read: the serving node
	//     may wait statelog.ReadBudget (2s) for the floor and again for the
	//     level, then runs one query, so a live holder answers inside four
	//     seconds (a test holds the two together). A failover
	//     (estate.ReadAttempt, fifteen seconds) is not covered: the unreadable hint
	//     renders and the seat reads the thread itself.
	ThreadTimeout = 5 * time.Second
)

// EmptyThreadHint is what the block says when the thread was read and had
// nothing in it.
const EmptyThreadHint = "(this thread has no earlier messages — the message " +
	"that woke you is all there is, so answer it on its own terms)"

// UnreadableThreadHint is what the block says when the thread could not be
// read from this node.
//
// A different sentence from [EmptyThreadHint], as [BuildingKnowledgeHint]
// differs from [EmptyKnowledgeHint]: it tells the seat the trigger is a
// fragment of a conversation, so go and read it before concluding anything.
//
// It covers more than a failed request: maintenance mode starts no chat
// transport, a self-hosted instance unreachable at boot leaves no chat
// surface, and a refused token leaves no client. Each renders this rather
// than nothing, because a seat handed no thread and no instruction to find
// one is worse off.
const UnreadableThreadHint = "(the thread this message is part of could not be " +
	"read from this node — you have only the triggering message, so read the " +
	"thread with your chat tools before answering anything that depends on it)"

// UnrenderableRootLine stands in for a thread root there is nothing to render.
//
// The root keeps its slot, and this sits in it. A root with no readable body
// is ordinary (an alert bot's attachments, a file share, a system line, a
// deleted root on the self-hosted backend) and people reply in such threads.
// Dropping it would promote the oldest surviving reply into the root's place,
// kept by every bound and described to the model as what the thread is about.
// So the absence is said: a seat shown a reply labelled as the opening could
// not find out.
const UnrenderableRootLine = "- _(the first message in this thread could not be " +
	"shown here — it carries no readable text. Read the thread itself with your " +
	"chat tools if what it opened with matters.)_"

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
// The ordinary preamble is false on this path: it says the newest messages
// woke the turn, and a read that stopped short is missing precisely the
// newest, the trigger included, because a backend pages a thread from the
// OLDEST end (see [notify.Transcript.StoppedShort]). A seat told otherwise
// answers the last message it has. So this keeps the two true facts (which
// thread, what the **you** lines are) and instead says to read the rest.
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
	// answered are the messages the block showed that were waiting for this
	// turn, each at the version it was shown at. See
	// [Blocks.ThreadContextAnswered].
	answered []Answered
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
	return f.renderThread(ctx, r, transcript, r.Thread.Backend)
}

// renderThread bounds and renders what came back.
//
// Whole messages, never a cut inside one. Past the bound the middle is
// condensed by the auxiliary model into one account marked as a rewrite, or
// left out and counted where none can be had. The ROOT and the NEWEST are
// exempt from every bound. The root is the first message, never "the first
// line that rendered" ([notify.ThreadReader]'s answer): one with nothing
// renderable keeps its slot as [UnrenderableRootLine], else the oldest reply
// would be promoted. It records which waiting messages it showed
// ([Blocks.ThreadContextAnswered]), only those rendered VERBATIM: a condensed
// one was seen as a paraphrase, and a recorded one has later deliveries dropped.
func (f *Fetcher) renderThread(ctx context.Context, r Request, read notify.Transcript, backend string) threadBlock {
	lines := make([]string, 0, len(read.Messages))
	// waiting runs beside lines: the message a line rendered, by its id and
	// the version it was shown at, when that message was waiting for this
	// turn, and the zero Answered for every other line.
	waiting := make([]Answered, 0, len(read.Messages))
	// authors runs beside them too: who said each line, so the fence names
	// the speakers of what it holds and nobody whose message was left out.
	authors := make([]provenance.Author, 0, len(read.Messages))
	waitingFor := waitingMessages(read, r.Message, r.WorkingFor)
	// rootSlot says the read carried a root at all; rootShown says it
	// rendered as a message rather than as its stand-in. The two are apart
	// because the message count reports what was HANDED OVER, and a notice
	// saying the opening is missing is not a message somebody sent.
	rootSlot, rootShown := len(read.Messages) > 0, false
	for i, m := range read.Messages {
		line := f.renderPost(m, backend)
		if i == 0 {
			if line == "" {
				// THE ENGINE'S WORDS in the root's slot, so the engine is
				// their author.
				lines = append(lines, UnrenderableRootLine)
				waiting = append(waiting, Answered{})
				authors = append(authors, provenance.Author{Kind: iam.ActorSystem})
				continue
			}
			rootShown = true
		} else if line == "" {
			continue
		}
		lines = append(lines, line)
		waiting = append(waiting, waitingFor[i])
		authors = append(authors, f.author(r, m, backend))
	}
	posts := len(lines)
	if rootSlot && !rootShown {
		posts--
	}
	if posts == 0 && !read.StoppedShort {
		// READ, and nobody had said anything in it. Reported as a read
		// rather than as a zero, because the same zero from a failed read
		// sends the seat to the opposite place and an operator after the
		// wrong fault.
		//
		// The root's own notice goes with it: it frames a conversation,
		// and there is no conversation here to frame — a thread whose one
		// message is a bookkeeping line has nothing the seat could go and
		// read that the trigger has not already given it.
		return threadBlock{text: EmptyThreadHint, read: true}
	}

	// THE BACKEND'S OWN DROPS COUNT TOO. A paged read holds a window and
	// says how many older messages it let go of; counting only this
	// renderer's own would print a number that is true of the slice in
	// hand and false of the thread, which is worse than no number at all —
	// a seat reads it as the whole of what it is missing.
	//
	// An unrenderable root is NOT in this number: it is reported by the
	// line standing in for it, and counting it here as well would tell the
	// seat one message is missing twice.
	dropped := read.Older
	lines, middle := splitThread(lines)
	// The same split, beside the lines: splitThread keeps the root's slot
	// and the newest, so a middle is lines[1 : 1+len(middle)].
	var waitingMiddle []Answered
	var authorsMiddle []provenance.Author
	if len(middle) > 0 {
		waitingMiddle = waiting[1 : 1+len(middle)]
		waiting = append(append([]Answered{}, waiting[0]), waiting[1+len(middle):]...)
		authorsMiddle = authors[1 : 1+len(middle)]
		authors = append(append([]provenance.Author{}, authors[0]), authors[1+len(middle):]...)
	}
	posts = len(lines)
	if rootSlot && !rootShown {
		posts--
	}
	// THE MIDDLE IS CONDENSED, NOT DROPPED. The messages between the root
	// and the newest that fit used to be left out, counted, and the seat
	// sent to its chat tools for them — which on a long thread is exactly
	// where the decision it is now being asked about was made. They are
	// rewritten by the seat's auxiliary model into one account instead,
	// and only left out, and counted, where no rewrite can be had.
	condensed := ""
	if len(middle) > 0 {
		// THE REWRITE READS THEM AS THEIR WORDS, fenced under who said
		// them, exactly as the seat reads the rest of the thread.
		label := threadLabel(r.Seat.Handle(), authorsMiddle, read.Hosted)
		res, err := f.src.Compact.For(r.Seat, r.Aux).Labelled(label).Fit(ctx, compact.KindThread,
			strings.Join(middle, "\n"), threadMaxChars/threadEarlierShare)
		switch {
		case err == nil && !res.Compacted:
			// THE MIDDLE FITS ITS SHARE AS IT IS — the newest message
			// alone was what pushed the block over — so it renders
			// verbatim, unlabelled. Calling it condensed would be a lie
			// about text nobody rewrote.
			lines = append(append(append([]string{}, lines[0]), middle...), lines[1:]...)
			waiting = append(append(append([]Answered{}, waiting[0]), waitingMiddle...), waiting[1:]...)
			authors = append(append(append([]provenance.Author{}, authors[0]), authorsMiddle...), authors[1:]...)
			posts += len(middle)
			middle = nil
		case err == nil:
			// A REWRITE OF THEIR WORDS is still theirs to answer for: a
			// condensation carries what an instruction in the middle said.
			condensed = res.Text
			authors = append(authors, authorsMiddle...)
		default:
			log.DebugContext(ctx, "prefetch_thread_not_condensed", "seat", r.Seat.Handle(),
				"messages", len(middle), "error", err.Error())
			dropped += len(middle)
		}
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
		var said strings.Builder
		said.WriteString(lines[0])
		if condensed != "" {
			// MARKED AS A REWRITE, and as standing for a stated number of
			// messages: a seat that took it for what somebody wrote would
			// quote a model's paraphrase back at them.
			said.WriteString("\n_(" + strconv.Itoa(len(middle)) + " earlier message(s), condensed by " +
				"a model (identifiers verbatim, wording not):)_\n" + condensed)
		}
		for _, line := range lines[1:] {
			said.WriteString("\n" + line)
		}
		b.WriteString("\n" + threadLabel(r.Seat.Handle(), authors, read.Hosted).Fence(
			r.Boundary, said.String()))
	}
	if dropped > 0 {
		// SAID OUT LOUD, and OUTSIDE THE FENCE, because it is the engine
		// speaking. A silently shortened thread reads as the whole
		// conversation, and a seat that believes it has seen everything
		// will not go and look for the rest.
		b.WriteString("\n_(" + strconv.Itoa(dropped) + " earlier message(s) in this " +
			"thread are not shown; read the thread itself with your chat tools if " +
			"you need them.)_")
	}
	var answered []Answered
	for _, a := range waiting {
		if a.ID != "" {
			answered = append(answered, a)
		}
	}
	return threadBlock{
		text: b.String(), posts: posts,
		read: true, stoppedShort: read.StoppedShort,
		answered: answered,
	}
}

// waitingMessages marks, by position, the messages waiting for the turn the
// trigger woke: the trigger writer's, written for whom the turn works for
// (workingFor), after this seat's last reply and before the trigger. Each is
// named by id and version read ([notify.Message.Version]), so its mark drops
// the wake of the edit shown, never a later one.
//
// Nothing unless the read reached the trigger. Only the trigger writer's
// (ADR-0109): another's is owed a turn of its own and would be dropped when it
// came back. Only those for this turn's carrier ([notify.WorkingForKey]), never
// the trigger document's, which keeps its post's carrier across an edit made
// for somebody else; nothing if the read could not say
// ([notify.Transcript.WorkingForUnread]).
func waitingMessages(read notify.Transcript, trigger string, workingFor *types.WorkingFor) map[int]Answered {
	if trigger == "" || read.StoppedShort || read.WorkingForUnread {
		return nil
	}
	at := -1
	for i, m := range read.Messages {
		if m.ID == trigger {
			at = i
		}
	}
	if at < 0 {
		return nil
	}
	writer := read.Messages[at].SenderID
	if writer == "" {
		return nil
	}
	whom := notify.WorkingForKey(workingFor)
	out := map[int]Answered{}
	for i := at - 1; i >= 0; i-- {
		m := read.Messages[i]
		if m.Own {
			break
		}
		if m.ID != "" && m.ID != trigger && m.SenderID == writer &&
			notify.WorkingForKey(m.WorkingFor) == whom {
			out[i] = Answered{ID: m.ID, Version: m.Version}
		}
	}
	return out
}

// splitThread divides rendered lines into what renders verbatim — the root's
// slot and the newest messages that fit — and the middle that does not.
//
// Everything renders verbatim when the thread is within [threadMaxChars].
// Past it, the newest are kept while they fit the bytes left after the
// condensed account's share; the root's slot and the newest message survive
// however long they are.
func splitThread(lines []string) (kept, middle []string) {
	if len(lines) <= 2 || len(strings.Join(lines, "\n")) <= threadMaxChars {
		return lines, nil
	}
	room := threadMaxChars - threadMaxChars/threadEarlierShare
	from := len(lines) - 1
	for from > 1 && len(lines[0])+len(strings.Join(lines[from-1:], "\n"))+1 <= room {
		from--
	}
	kept = append(append([]string{}, lines[0]), lines[from:]...)
	return kept, lines[1:from]
}

// renderPost renders one message as a bullet.
func (f *Fetcher) renderPost(m notify.Message, backend string) string {
	text := collapse(m.Text)
	if text == "" {
		return ""
	}
	return "- **" + f.speaker(m, backend) + "**: " + text
}

// threadLabel is what a thread's lines are fenced under: who said them, at the
// trust they earn from the seat self. On the engine's own chat it is the rule
// every door to a hosted surface reads by ([provenance.LabelForHosted]), so a
// line whose sender nobody can name is a colleague's there — as the same
// message is when it wakes the seat — and on a vendor's an outside party's.
func threadLabel(self string, authors []provenance.Author, hosted bool) provenance.Label {
	if hosted {
		return provenance.LabelForHosted(self, provenance.SourceChatThread, authors, true)
	}
	return provenance.LabelFor(self, provenance.SourceChatThread, authors)
}

// author is who said it, as the fence names them: the seat itself for its own
// replies, a colleague's seat where the party registry knows the sender, and
// otherwise somebody nothing vouches for. The SAME lookup [Fetcher.speaker]
// makes, so the line and the fence cannot name two different people.
func (f *Fetcher) author(r Request, m notify.Message, backend string) provenance.Author {
	if m.Own {
		return provenance.Author{Kind: iam.ActorAgent, Seat: r.Seat.Handle()}
	}
	if f.src.Parties != nil && backend != "" && m.SenderID != "" {
		if party, ok := f.src.Parties.ByExternalID(backend, m.SenderID); ok &&
			strings.TrimSpace(party.Handle) != "" {
			party.Handle = strings.TrimSpace(party.Handle)
			return notify.PartyAuthor(party)
		}
	}
	// NOBODY THE ANSWER PROVES: on a vendor's backend a person or a bot in a
	// shared channel the company does not know, and on the engine's own chat
	// a sender the roster no longer names, which [threadLabel] reads as a
	// writer the record could not name rather than as an outside party.
	return provenance.Author{}
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
