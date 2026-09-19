package prefetch_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
)

// ── the fakes ──

// threads is a chat backend that hands back a canned thread.
type threads struct {
	messages []notify.Message
	// older and stoppedShort are what a PAGED backend reports about its own
	// bounds: how many earlier messages it dropped to hold its window, and
	// whether it gave up before the newest message in the thread.
	older        int
	stoppedShort bool
	refuse       bool
	asked        []notify.Thread
}

func (s *threads) ReadThread(_ context.Context, _ string, t notify.Thread) (notify.Transcript, bool) {
	s.asked = append(s.asked, t)
	if s.refuse {
		return notify.Transcript{}, false
	}
	return notify.Transcript{
		Messages: s.messages, Older: s.older, StoppedShort: s.stoppedShort,
	}, true
}

// parties answers the registry lookup the renderer makes.
type parties map[string]notify.Party

func (p parties) ByExternalID(transport, externalID string) (notify.Party, bool) {
	party, ok := p[transport+":"+externalID]
	return party, ok
}

// threadRequest is a turn woken in a chat thread.
func threadRequest(t *testing.T) prefetch.Request {
	t.Helper()
	r := request(t)
	r.Thread = notify.Thread{Backend: "slack", Channel: "C0ENG", Root: "1700000001.000100"}
	return r
}

func said(who, what string) notify.Message {
	return notify.Message{SenderID: who, Text: what}
}

// ── the invariants ──

// AN UNREADABLE THREAD SAYS SOMETHING DIFFERENT FROM AN EMPTY ONE, and the
// difference is what it tells the seat to do.
//
// "There is nothing earlier" says answer the trigger as it stands; "it could
// not be read" says the trigger is a fragment of a conversation nobody handed
// over, so go and read it. A seat given the wrong one of those either answers
// a fragment as though it were the whole ask, or spends three MCP rounds
// re-reading a thread that has one message in it.
func TestAnUnreadableThreadIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()
	unreadable := fetch(t, prefetch.Sources{Threads: &threads{refuse: true}}, threadRequest(t))
	if unreadable.ThreadContext != prefetch.UnreadableThreadHint {
		t.Fatalf("a refused read rendered:\n%s", unreadable.ThreadContext)
	}
	if unreadable.ThreadContextPosts != 0 {
		t.Errorf("a refused read reported %d messages", unreadable.ThreadContextPosts)
	}

	empty := fetch(t, prefetch.Sources{Threads: &threads{}}, threadRequest(t))
	if empty.ThreadContext != prefetch.EmptyThreadHint {
		t.Fatalf("an empty thread rendered:\n%s", empty.ThreadContext)
	}
	if prefetch.EmptyThreadHint == prefetch.UnreadableThreadHint {
		t.Fatal("the two hints are the same sentence")
	}

	// AND THE DIFFERENCE IS CARRIED OUT, not left in the prose. Both hints
	// are non-empty and both report zero messages, so everything downstream
	// — the summary event, the dashboard, an operator — sees one pair of
	// numbers for two opposite states unless a field says which.
	if unreadable.ThreadContextRead {
		t.Error("a refused read reported the thread as read")
	}
	if !empty.ThreadContextRead {
		t.Error("a thread that was read and had nothing in it reports as unreadable")
	}
}

// NO READER AT ALL IS AN UNREADABLE THREAD, not an absent one.
//
// A node in maintenance mode starts no chat transport, and a node whose chat
// instance was unreachable at boot runs the company without its chat surface.
// Rendering nothing there would leave a seat with no thread AND no
// instruction to find one, which is strictly worse than before the block
// existed — the prompt used to at least tell it to go and look.
func TestANodeWithNoChatReaderStillTellsTheSeatToLook(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{}, threadRequest(t))
	if blocks.ThreadContext != prefetch.UnreadableThreadHint {
		t.Fatalf("a node with no reader rendered:\n%q", blocks.ThreadContext)
	}
}

// A TRIGGER WITH NO THREAD RENDERS NO BLOCK AT ALL — not a hint.
//
// A webhook, a scheduled fire and a top-level chat message have no earlier
// conversation to be missing, and a hint telling such a seat that its thread
// could not be read would send it looking for one that does not exist.
func TestATriggerWithNoThreadRendersNoBlock(t *testing.T) {
	t.Parallel()
	source := &threads{messages: []notify.Message{said("U1", "hello")}}
	blocks := fetch(t, prefetch.Sources{Threads: source}, request(t))
	if blocks.ThreadContext != "" {
		t.Fatalf("a non-thread trigger rendered:\n%s", blocks.ThreadContext)
	}
	if len(source.asked) != 0 {
		t.Errorf("a non-thread trigger read a thread anyway: %v", source.asked)
	}
	// AND IT IS THE THIRD STATE, not either of the other two: nothing was
	// read, so reporting a read here would put a chat fact on every webhook
	// and scheduled turn in the company.
	if blocks.ThreadContextRead || blocks.ThreadContextPosts != 0 {
		t.Errorf("a non-thread trigger reported a thread: %+v", blocks)
	}
}

// THE BOUND DROPS WHOLE MESSAGES, OLDEST FIRST, AND SAYS HOW MANY.
//
// Never a cut inside one, which would leave half of what somebody said
// reading as the whole of it. The ROOT survives every bound because it is
// what the thread is about, and the NEWEST survives because it is the message
// that woke the turn.
func TestTheBoundDropsWholeMessagesAndReportsThem(t *testing.T) {
	t.Parallel()
	const total = 80
	msgs := []notify.Message{said("U1", "the root question")}
	for i := 1; i < total; i++ {
		msgs = append(msgs, said("U2", "reply "+strconv.Itoa(i)))
	}
	blocks := fetch(t, prefetch.Sources{Threads: &threads{messages: msgs}}, threadRequest(t))

	if blocks.ThreadContextPosts != 30 {
		t.Fatalf("the block rendered %d messages, want the item cap", blocks.ThreadContextPosts)
	}
	if !strings.Contains(blocks.ThreadContext, "the root question") {
		t.Fatalf("the root was dropped:\n%s", blocks.ThreadContext)
	}
	if !strings.Contains(blocks.ThreadContext, "reply "+strconv.Itoa(total-1)) {
		t.Fatalf("the newest message was dropped:\n%s", blocks.ThreadContext)
	}
	// Whole messages: the oldest reply is gone entirely rather than
	// appearing shortened.
	if strings.Contains(blocks.ThreadContext, "reply 1:") ||
		strings.Contains(blocks.ThreadContext, "reply 1\n") {
		t.Fatalf("the oldest reply survived the cap:\n%s", blocks.ThreadContext)
	}
	if !strings.Contains(blocks.ThreadContext, "50 earlier message(s)") {
		t.Fatalf("the drop was not reported:\n%s", blocks.ThreadContext)
	}
	if lines := strings.Count(blocks.ThreadContext, "\n- "); lines != 30 {
		t.Fatalf("the block carries %d bullets, want 30", lines)
	}
}

// THE BYTE CEILING DROPS WHOLE MESSAGES TOO, and stops at two.
//
// A block trimmed to nothing tells the turn this thread has no history, which
// is the one thing it must not conclude — so the root and the newest both
// survive however long they are, exactly as the conversation ledger's newest
// entry does.
func TestTheByteCeilingNeverTrimsAwayTheRootOrTheNewest(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 6000)
	msgs := []notify.Message{
		said("U1", "ROOT "+long),
		said("U2", "MIDDLE "+long),
		said("U3", "NEWEST "+long),
	}
	blocks := fetch(t, prefetch.Sources{Threads: &threads{messages: msgs}}, threadRequest(t))

	if blocks.ThreadContextPosts != 2 {
		t.Fatalf("the ceiling kept %d messages, want the root and the newest",
			blocks.ThreadContextPosts)
	}
	for _, want := range []string{"ROOT", "NEWEST", "1 earlier message(s)"} {
		if !strings.Contains(blocks.ThreadContext, want) {
			t.Errorf("the block lost %q", want)
		}
	}
	if strings.Contains(blocks.ThreadContext, "MIDDLE") {
		t.Error("the middle message survived the byte ceiling")
	}
	// And nothing was cut INSIDE a message: both survivors are whole.
	if strings.Count(blocks.ThreadContext, long) != 2 {
		t.Error("a surviving message was shortened rather than kept whole")
	}
}

// A THREAD INSIDE BOTH BOUNDS IS HANDED OVER WHOLE, with nothing claimed to
// be missing: a drop notice on a complete thread would send a seat looking
// for messages that are already in front of it.
func TestAShortThreadIsHandedOverWhole(t *testing.T) {
	t.Parallel()
	source := &threads{messages: []notify.Message{
		said("U1", "the staging redirect loops"), said("U2", "on it"),
	}}
	blocks := fetch(t, prefetch.Sources{Threads: source}, threadRequest(t))

	if blocks.ThreadContextPosts != 2 {
		t.Fatalf("posts = %d, want 2", blocks.ThreadContextPosts)
	}
	if strings.Contains(blocks.ThreadContext, "not shown") {
		t.Fatalf("a whole thread claimed messages were dropped:\n%s", blocks.ThreadContext)
	}
	// AND IT IS THE TRIGGER'S OWN THREAD, addressed off the request rather
	// than re-derived: a second derivation is free to disagree with the
	// reply target about which thread this is.
	if len(source.asked) != 1 || source.asked[0].Root != "1700000001.000100" ||
		source.asked[0].Channel != "C0ENG" || source.asked[0].Backend != "slack" {
		t.Fatalf("the reader was asked for %+v", source.asked)
	}
}

// THE SEAT'S OWN REPLIES ARE MARKED AS ITS OWN.
//
// An agent that reads its own earlier replies as a colleague's answers itself
// — which is exactly the confusion the thread block's self-check exists to
// prevent, reintroduced one layer down.
func TestTheSeatsOwnRepliesAreMarked(t *testing.T) {
	t.Parallel()
	msgs := []notify.Message{
		said("U1", "can you look at staging"),
		{Text: "on it — reproducing now", Own: true},
	}
	blocks := fetch(t, prefetch.Sources{Threads: &threads{messages: msgs}}, threadRequest(t))
	if !strings.Contains(blocks.ThreadContext, "- **you**: on it — reproducing now") {
		t.Fatalf("the seat's own reply is not marked:\n%s", blocks.ThreadContext)
	}
	if strings.Contains(blocks.ThreadContext, "- **you**: can you look") {
		t.Fatalf("a colleague's message was marked as the seat's own:\n%s", blocks.ThreadContext)
	}
}

// A KNOWN COLLEAGUE RENDERS AS A COLLEAGUE, A STRANGER AS THEIR PLATFORM ID,
// AND NEITHER RENDERS BLANK.
//
// A miss is ordinary — most people in a shared channel are not colleagues —
// and an unattributed line reads as the previous speaker continuing, which is
// how an agent comes to answer a question nobody asked it.
func TestEverySpeakerIsNamedSomehow(t *testing.T) {
	t.Parallel()
	msgs := []notify.Message{
		said("U-lead", "what is the plan"),
		said("U-outsider", "same question here"),
		{SenderID: "B-hook", SenderName: "deploybot", Text: "release 4.2.4 is live"},
		{Text: "no id at all"},
	}
	blocks := fetch(t, prefetch.Sources{
		Threads: &threads{messages: msgs},
		Parties: parties{"slack:U-lead": {Handle: "lead", Name: "Tech Lead"}},
	}, threadRequest(t))

	for _, want := range []string{
		"- **Tech Lead (lead)**: what is the plan",
		"- **U-outsider**: same question here",
		"- **deploybot**: release 4.2.4 is live",
		"- **unknown**: no id at all",
	} {
		if !strings.Contains(blocks.ThreadContext, want) {
			t.Errorf("the block is missing %q:\n%s", want, blocks.ThreadContext)
		}
	}
}

// WITH NO REGISTRY every sender is their raw platform id — legible, and never
// blank.
func TestWithoutARegistryEverySenderIsStillNamed(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{Threads: &threads{
		messages: []notify.Message{said("U-lead", "what is the plan")},
	}}, threadRequest(t))
	if !strings.Contains(blocks.ThreadContext, "- **U-lead**: what is the plan") {
		t.Fatalf("a sender rendered without a registry as:\n%s", blocks.ThreadContext)
	}
}

// THE BLOCK FRAMES ITSELF. The model is told what the list is, which end is
// newest, and what the marked lines are — none of which it can infer from a
// bare list of bullets in a system prompt.
func TestTheBlockSaysWhatItIs(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{Threads: &threads{
		messages: []notify.Message{said("U1", "hello")},
	}}, threadRequest(t))
	for _, want := range []string{"oldest first", "woke", "**you**"} {
		if !strings.Contains(blocks.ThreadContext, want) {
			t.Errorf("the preamble does not mention %q:\n%s", want, blocks.ThreadContext)
		}
	}
}

// A MESSAGE WITH NOTHING IN IT IS NOT A LINE. A blank bullet in a thread
// reads as somebody having said nothing on purpose, and a whole thread of
// them is an empty thread rather than a rendered one.
func TestMessagesWithNoTextAreNotRendered(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{Threads: &threads{messages: []notify.Message{
		said("U1", "the root question"), said("U2", "   "), said("U3", "and the answer"),
	}}}, threadRequest(t))
	if blocks.ThreadContextPosts != 2 {
		t.Fatalf("posts = %d, want the two messages with something in them",
			blocks.ThreadContextPosts)
	}
	if strings.Contains(blocks.ThreadContext, "- **U2**") {
		t.Fatalf("an empty message rendered a bullet:\n%s", blocks.ThreadContext)
	}

	// And a thread of nothing but those is an EMPTY thread, not a rendered
	// one with no lines in it.
	silent := fetch(t, prefetch.Sources{Threads: &threads{messages: []notify.Message{
		said("U2", " "),
	}}}, threadRequest(t))
	if silent.ThreadContext != prefetch.EmptyThreadHint {
		t.Fatalf("a thread of blank messages rendered:\n%s", silent.ThreadContext)
	}
}

// A ROOT THAT RENDERS TO NOTHING IS SAID, NEVER REPLACED BY A REPLY.
//
// The renderer keeps the first message whatever every bound drops, and the
// preamble tells the model that line is what the thread is about. When the
// root carries nothing renderable — an alert bot's attachment-only post, a
// deleted opening the self-hosted backend leaves out — filtering first and
// keeping "line 0" promotes the oldest surviving REPLY into that slot, and
// the seat is then told a mid-thread answer is the question.
func TestAnUnrenderableRootIsSaidRatherThanReplaced(t *testing.T) {
	t.Parallel()
	source := &threads{messages: []notify.Message{
		// The root: present, and with nothing to render.
		{SenderID: "B-alerts"},
		said("U2", "is this the billing job or the payments one"),
		said("U3", "billing"),
	}}
	blocks := fetch(t, prefetch.Sources{Threads: source}, threadRequest(t))

	lines := strings.Split(blocks.ThreadContext, "\n")
	if len(lines) < 2 {
		t.Fatalf("the block rendered as:\n%s", blocks.ThreadContext)
	}
	// The root's own slot, in the root's own place — not the oldest reply.
	if lines[1] != prefetch.UnrenderableRootLine {
		t.Fatalf("the line under the preamble is %q, want the root's stand-in",
			lines[1])
	}
	// AND THE COUNT IS OF MESSAGES, not of lines: the stand-in says a
	// message is missing, so counting it would report the seat as having
	// been handed the very thing the line says it was not.
	if blocks.ThreadContextPosts != 2 {
		t.Fatalf("posts = %d, want the two replies that rendered",
			blocks.ThreadContextPosts)
	}
	// AND IT IS NOT ALSO IN THE DROP NOTICE. The stand-in reports the root
	// once; a drop count that included it would tell the seat a second
	// message is missing that never existed.
	if strings.Contains(blocks.ThreadContext, "not shown") {
		t.Fatalf("a whole thread claimed messages were dropped:\n%s", blocks.ThreadContext)
	}
	if !blocks.ThreadContextRead {
		t.Error("a thread that was read reported itself unread")
	}

	// AND THE BOUNDS KEEP THE SLOT rather than trimming it away with the
	// oldest replies: the whole point of exempting the root is that the
	// block says what the thread is about, and a stand-in that the item
	// cap deleted would leave the newest replies reading as the opening.
	long := &threads{messages: []notify.Message{{SenderID: "B-alerts"}}}
	for i := 1; i < 80; i++ {
		long.messages = append(long.messages, said("U2", "reply "+strconv.Itoa(i)))
	}
	capped := fetch(t, prefetch.Sources{Threads: long}, threadRequest(t))
	cappedLines := strings.Split(capped.ThreadContext, "\n")
	if cappedLines[1] != prefetch.UnrenderableRootLine {
		t.Fatalf("the item cap dropped the root's slot:\n%s", capped.ThreadContext)
	}
	if capped.ThreadContextPosts != 29 {
		t.Fatalf("posts = %d, want the cap less the root's stand-in",
			capped.ThreadContextPosts)
	}
	if !strings.Contains(capped.ThreadContext, "50 earlier message(s)") {
		t.Fatalf("the drop was not reported:\n%s", capped.ThreadContext)
	}
}

// A TURN WHOSE ONLY CONTEXT IS THE THREAD HAS CONTEXT.
//
// [prefetch.Blocks.Empty] enumerates the blocks by hand and is called only by
// tests, so a field missing from it costs no build and fails nothing — it
// quietly converts every assertion that reads through it into one that cannot
// fail, which is worse than having none.
func TestAThreadIsContext(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{Threads: &threads{
		messages: []notify.Message{said("U1", "the staging redirect loops")},
	}}, threadRequest(t))
	if blocks.Empty() {
		t.Fatalf("a turn handed a thread reports no context at all: %+v", blocks)
	}
}

// The blocks run CONCURRENTLY, so an unrecovered panic in this one would take
// the whole PROCESS down rather than the turn — and this renderer indexes
// into a slice whose length came from a third-party app.
func TestAPanickingThreadReaderCostsOnlyItsOwnBlock(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{
		Threads: panickingThreads{},
		Skills: skills{rows: []learning.Skill{
			{Name: "ship-a-fix", Description: "the release checklist"}}},
	}, threadRequest(t))
	if blocks.ThreadContext != "" {
		t.Fatalf("the panicking reader rendered %q", blocks.ThreadContext)
	}
	// THE COUNT GOES WITH THE PROSE. A block that panicked mid-render must
	// never report messages it did not surface.
	if blocks.ThreadContextPosts != 0 {
		t.Fatalf("the panicking reader reported %d messages", blocks.ThreadContextPosts)
	}
	if blocks.ThreadContextRead || blocks.ThreadContextStoppedShort {
		t.Fatalf("the panicking reader reported a read: %+v", blocks)
	}
	if !strings.Contains(blocks.SynthesizedSkills, "ship-a-fix") {
		t.Fatalf("a sibling block was lost: %q", blocks.SynthesizedSkills)
	}
}

type panickingThreads struct{}

func (panickingThreads) ReadThread(context.Context, string, notify.Thread) (notify.Transcript, bool) {
	panic("a malformed thread")
}

// A READ THAT STOPPED SHORT NEVER CLAIMS THE NEWEST MESSAGE IS HERE.
//
// A chat backend pages a thread from the OLDEST end, so a read bounded before
// the end of a long thread is missing the NEWEST messages — the one that woke
// the turn included. The ordinary preamble tells the model those newest
// messages are what woke it, and on this path that sentence is guaranteed
// false: a seat believing it answers whichever message it happens to hold and
// reports the work delivered, which is the exact failure this block exists to
// stop. So the preamble changes, and what replaces it carries the instruction
// that IS right here — go and read the rest.
func TestAReadThatStoppedShortNeverClaimsTheNewestIsHere(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{Threads: &threads{
		messages:     []notify.Message{said("U1", "the root question"), said("U2", "and then")},
		older:        900,
		stoppedShort: true,
	}}, threadRequest(t))

	if strings.Contains(blocks.ThreadContext, "newest messages are what woke") {
		t.Fatalf("a truncated thread still claims its newest message woke the turn:\n%s",
			blocks.ThreadContext)
	}
	for _, want := range []string{
		"IT STOPS SHORT",
		"are NOT below",
		"the rest of the thread with your chat tools",
		// Everything the backend dropped is still counted: a seat told
		// nothing is missing does not go and look.
		"900 earlier message(s)",
	} {
		if !strings.Contains(blocks.ThreadContext, want) {
			t.Errorf("the truncated block does not say %q:\n%s", want, blocks.ThreadContext)
		}
	}

	// AND THE ORDINARY THREAD KEEPS THE CLAIM. The honest sentence has to be
	// the exception: printed on every turn it would teach every seat to
	// re-read a thread it was just handed whole.
	whole := fetch(t, prefetch.Sources{Threads: &threads{
		messages: []notify.Message{said("U1", "the root question"), said("U2", "and then")},
	}}, threadRequest(t))
	if !strings.Contains(whole.ThreadContext, "newest messages are what woke") {
		t.Fatalf("a complete thread lost the preamble's newest-message claim:\n%s",
			whole.ThreadContext)
	}
	if strings.Contains(whole.ThreadContext, "IT STOPS SHORT") {
		t.Fatalf("a complete thread claims it stopped short:\n%s", whole.ThreadContext)
	}

	// AND IT IS REPORTED, not only said in the prompt: the message count
	// reads the same on a truncated thread as on a whole one, so without
	// this field a seat answering a message it never saw is invisible to
	// the operator looking for exactly that turn.
	truncated := fetch(t, prefetch.Sources{Threads: &threads{
		messages:     []notify.Message{said("U1", "the root question")},
		stoppedShort: true,
	}}, threadRequest(t))
	if !truncated.ThreadContextStoppedShort || !truncated.ThreadContextRead {
		t.Errorf("a truncated read reported %+v", truncated)
	}
	if whole.ThreadContextStoppedShort {
		t.Error("a complete thread reported itself truncated")
	}
}

// THE BACKEND'S OWN DROPS ARE COUNTED TOO.
//
// A paged backend holds a window of its own and says how many older messages
// it let go of to keep the newest. A notice built from this renderer's drops
// alone prints a number that is true of the slice in hand and false of the
// thread — and understating it is worse than printing nothing, because a seat
// reads it as the whole of what it is missing.
func TestTheBackendsOwnDropsAreCountedInTheNotice(t *testing.T) {
	t.Parallel()
	const read = 40
	msgs := []notify.Message{said("U1", "the root question")}
	for i := 1; i < read; i++ {
		msgs = append(msgs, said("U2", "reply "+strconv.Itoa(i)))
	}
	blocks := fetch(t, prefetch.Sources{Threads: &threads{messages: msgs, older: 610}},
		threadRequest(t))

	// 610 the backend dropped, plus the 10 this renderer drops to reach its
	// own thirty.
	if !strings.Contains(blocks.ThreadContext, "620 earlier message(s)") {
		t.Fatalf("the notice does not account for the backend's own drops:\n%s",
			blocks.ThreadContext)
	}
	if blocks.ThreadContextPosts != 30 {
		t.Fatalf("the block rendered %d messages, want the item cap",
			blocks.ThreadContextPosts)
	}
}
