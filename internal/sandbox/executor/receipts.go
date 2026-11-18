package executor

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// chainReading is the resume's model ([threadChain]) as a reading of the one
// walk ([walkThread]): every whole line to See, every refused one to Unread,
// and the handle of the line last seen kept for [hookReading], composed after
// it. End is its caller's, at the walk's end, under the read's context.
type chainReading struct {
	c    *threadChain
	last chainHandle
}

var (
	_ threadReading = (*chainReading)(nil)
	_ recordKeeper  = (*chainReading)(nil)
)

func (r *chainReading) line(l threadLine) { r.last, _, _ = r.c.See(l) }

func (r *chainReading) refused(fault *threadLineError) {
	r.last = noChainRecord
	r.c.Unread(fault)
}

func (r *chainReading) end(bool, error) {}

// records is how many items the chain holds ([recordKeeper]), its call ids
// among them: a few records may carry a thread's calls.
func (r *chainReading) records() int { return r.c.items() }

// hookEvent is the event of a hook's firing a take is asked at: the zero
// value is neither, which no take is held at.
type hookEvent uint8

const (
	hookEventNone hookEvent = iota
	hookEventBatch
	hookEventStop
)

// hookEventOf is the event a hook record or an ask names.
func hookEventOf(event string) hookEvent {
	switch event {
	case claudeEventToolBatch:
		return hookEventBatch
	case claudeEventFinish:
		return hookEventStop
	}
	return hookEventNone
}

// hookTake is what one walk keeps of one asked take: its pair's firing, by the
// digest of its toolUseID and its event, the two records' handles and the
// system message's line; and the firing of the first success whose stdout
// names it. Fixed in size, whatever the thread holds (the package doc's "A
// hook's note, confirmed off its job's main thread").
type hookTake struct {
	firing, stdout        [32]byte
	event, stdoutEvent    hookEvent
	system, context       chainHandle
	systemLine            int
	opened, closed        bool
	named, namedElsewhere bool
}

// hookReading reads, in the one walk beside [chainReading], which of the
// asked takes the job's main thread shows a hook handed its model, and which
// it proves were not ([hookReading.receipts]). The rules and why each holds
// are the package doc's "A hook's note, confirmed off its job's main thread".
type hookReading struct {
	chain  *chainReading
	tokens []string
	takes  []hookTake

	restarts                 threadRestarts
	lastRestart, lastCleared int
	stray, malformed, broken bool
	torn                     bool
	err, askErr              error
}

var _ takeAsker = (*hookReading)(nil)

// unreadTakes is why no walk is made for ask's takes, "" where one is: none
// can be held off a release whose resume was not read, and so none absent;
// past [maxAskedTakes], a hook record's masks cannot name them, so a later
// build's longer list is refused before the thread is opened.
func unreadTakes(ask sandbox.MainThreadAsk) string {
	switch {
	case len(ask.Takes) == 0:
		return ""
	case !claudeResumeRead(ask.Release):
		return fmt.Sprintf("the release that ran the job, %q, is none whose resume was read", ask.Release)
	case len(ask.Takes) > maxAskedTakes:
		return fmt.Sprintf("%v: %d asked, %d held", errAskedPastMasks, len(ask.Takes), maxAskedTakes)
	}
	return ""
}

// newHookReading reads takes off the handles chain files; past
// [maxAskedTakes] of them, which [unreadTakes] refuses first, it decides none
// ([threadReader.Asking]).
func newHookReading(chain *chainReading, takes []sandbox.AskedTake) *hookReading {
	r := &hookReading{chain: chain, tokens: make([]string, len(takes)), takes: make([]hookTake, len(takes))}
	for i, take := range takes {
		r.tokens[i] = claudeTakeToken(take.Take)
	}
	return r
}

// ask asks reader for the takes' tokens; a refusal decides none of them.
func (r *hookReading) ask(reader *threadReader) { r.askErr = reader.Asking(r.tokens) }

func (r *hookReading) line(l threadLine) {
	if r.restarts.See(l.Head) {
		r.lastRestart = l.N
	}
	if !l.Head.Main() || !l.Head.Chained() {
		return
	}
	switch l.Head.Type {
	case recordAssistant:
		// A REPLY WHOSE REQUEST CLEARED TOOL RESULTS may have cleared a batch's
		// note with its result, so every batch pair before it is read as
		// before a restart; a reply that does not read, as one that did.
		if cleared, err := l.ToolResultsCleared(); cleared || err != nil {
			r.lastCleared = l.N
		}
	case recordAttachment:
		r.attachment(l, r.chain.last)
	}
}

// attachment reads l, a main-thread chained attachment record whose handle
// in the chain is h.
func (r *hookReading) attachment(l threadLine, h chainHandle) {
	kind, err := l.AttachmentType()
	if _, formErr := l.AttachmentWellFormed(); err != nil || formErr != nil {
		r.malformed = true
		return
	}
	if kind != hookSuccess && kind != hookSystemMessage && kind != hookAdditionalContext {
		return
	}
	firing, isString := l.AttachmentToolUseID()
	name, err := l.HookEvent()
	if !isString || err != nil {
		r.malformed = true
		return
	}
	event := hookEventOf(name)
	content, stdout := l.HookTakes()
	switch kind {
	case hookSystemMessage:
		r.open(content, firing, event, h, l.N)
		return
	case hookSuccess:
		r.said(stdout, firing, event)
	case hookAdditionalContext:
		r.close(firing, event, h)
	}
	if content != 0 {
		r.stray = true
	}
}

// open is a system message of firing at event, record h on line n, naming the
// takes bits holds: each opens its take's pair, unless another firing did, or
// the event is none a take is asked at, either a stray name.
func (r *hookReading) open(bits uint64, firing [32]byte, event hookEvent, h chainHandle, n int) {
	for i := range r.takes {
		if bits&(1<<i) == 0 {
			continue
		}
		t := &r.takes[i]
		if event == hookEventNone || t.opened {
			r.stray = true
			continue
		}
		t.opened, t.firing, t.event, t.system, t.systemLine = true, firing, event, h, n
	}
}

// close is an additional context of firing at event, record h: it closes
// every pair that firing's system message opened before it.
func (r *hookReading) close(firing [32]byte, event hookEvent, h chainHandle) {
	for i := range r.takes {
		t := &r.takes[i]
		if t.opened && !t.closed && t.firing == firing && t.event == event {
			t.closed, t.context = true, h
		}
	}
}

// said is a success of firing at event whose stdout names the takes bits
// holds, kept to be held to each take's own pair at the end.
func (r *hookReading) said(bits uint64, firing [32]byte, event hookEvent) {
	for i := range r.takes {
		if bits&(1<<i) == 0 {
			continue
		}
		t := &r.takes[i]
		switch {
		case !t.named:
			t.named, t.stdout, t.stdoutEvent = true, firing, event
		case t.stdout != firing || t.stdoutEvent != event:
			t.namedElsewhere = true
		}
	}
}

func (r *hookReading) refused(*threadLineError) { r.broken = true }

func (r *hookReading) end(torn bool, err error) { r.torn, r.err = torn, err }

// receipts decides each take ask asked once the walk has ended, Ending the
// chain under ctx: Held, Restarted, Absent, or none of these with Why (the
// package doc's "A hook's note, confirmed off its job's main thread"). Its
// caller walks for no take [unreadTakes] refuses.
func (r *hookReading) receipts(ctx context.Context, ask sandbox.MainThreadAsk) sandbox.HookReceipts {
	if r.askErr != nil {
		return sandbox.HookReceipts{Why: fmt.Sprintf("the takes were not asked of the thread: %v", r.askErr)}
	}
	if r.err != nil {
		return sandbox.HookReceipts{Why: fmt.Sprintf("the thread was not read to its end: %v", r.err)}
	}
	chain := r.chain.c
	if err := chain.End(ctx); err != nil {
		return sandbox.HookReceipts{Why: fmt.Sprintf("the resume's model of the thread was not decided: %v", err)}
	}
	var out sandbox.HookReceipts
	why := func(take, reason string) {
		if out.Why == "" {
			out.Why = "take " + take + ": " + reason
		}
	}
	unplaced, unknown := chain.Unknown()
	anySize := chain.AnySize()
	stray := r.stray
	held := make([]bool, len(ask.Takes))
	var heldAt [hookEventStop + 1]bool
	for i, take := range ask.Takes {
		t, event := r.takes[i], hookEventOf(take.Event)
		if t.opened && (!t.closed || t.event != event) {
			stray = true
		}
		if t.named && (!t.opened || t.stdout != t.firing || t.stdoutEvent != t.event || t.namedElsewhere) {
			stray = true
		}
		if !t.opened || !t.closed || t.event != event || event == hookEventNone {
			continue
		}
		switch {
		case !anySize:
			why(take.Take, "the thread is not one every size path of the resume reads alike")
		case chain.Handed(t.system) != handedYes || chain.Handed(t.context) != handedYes:
			why(take.Take, "the resume does not hand its pair: "+string(chain.Why(t.system))+", "+
				string(chain.Why(t.context)))
		case r.lastRestart > t.systemLine || (event == hookEventBatch && r.lastCleared > t.systemLine):
			out.Restarted = append(out.Restarted, take.Take)
		default:
			held[i], heldAt[event] = true, true
			out.Held = append(out.Held, take.Take)
		}
	}
	unproven := ""
	switch {
	case !ask.Ended:
		unproven = "the job's harness did not end its own run"
	case r.broken:
		unproven = "a line of the thread is no record"
	case r.torn:
		unproven = "the thread's last piece is torn"
	case r.lastRestart > 0:
		unproven = "the thread restarts"
	case r.malformed:
		unproven = "a hook record's fields are not of their type"
	case stray:
		unproven = "an asked take is named outside its own pair"
	case unknown:
		unproven = "the resume's model cannot place the thread: " + string(unplaced)
	}
	for i, take := range ask.Takes {
		t, event := r.takes[i], hookEventOf(take.Event)
		switch {
		case held[i], t.opened && t.closed && t.event == event && event != hookEventNone:
			continue
		case t.opened:
			why(take.Take, "its pair is split")
		case unproven != "":
			why(take.Take, unproven)
		case event == hookEventNone || !heldAt[event]:
			why(take.Take, "no other asked take at its event is held")
		default:
			out.Absent = append(out.Absent, take.Take)
		}
	}
	return out
}
