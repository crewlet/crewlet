package ledger

import "strings"

// Session is one completed turn of one conversation.
//
// Built by the turn engine at turn end from data already in hand — no LLM call
// — and stored as the payload of a conversation_sessions row.
//
// Why a ledger and not a transcript. The engine can already round-trip a whole
// LLM conversation: a parked sandbox run persists the full message list,
// signed thinking blocks included, and splices it back into a running loop.
// That is right for a PARKED turn, whose dangling tool call is waiting for one
// answer. It is wrong here: a conversation's next turn arrives against a
// thread that has MOVED, and replaying raw prior context invites acting on
// state that is no longer true. So this carries the same things the iteration
// ledger carries (intent, calls, artifact, verdict) under the same budgets,
// plus the two facts a cross-turn reader needs that a within-turn one does
// not: who said what to trigger it, and what the seat finally replied.
type Session struct {
	TurnID string `json:"turn_id,omitempty"`

	// At is the completion time, rendered so the next turn can tell "ten
	// minutes ago" from "last Tuesday" — the difference between a live
	// exchange and a thread that has moved on.
	At string `json:"at,omitempty"`

	Trigger string `json:"trigger,omitempty"`

	// Intent is what the turn set out to do, in the seat's own words.
	//
	// One field where there were two. The other was the planner's separate
	// reasoning, which no longer exists as a distinct artifact — the
	// executor decides and acts in one conversation — and which nothing
	// ever filled: the engine wrote a Reply and a Decision and left every
	// other field here empty, so a seat's own history read as a list of
	// replies with no account of what produced them.
	Intent string `json:"intent,omitempty"`

	// Calls is PRE-RENDERED tool-call lines. Rendered at WRITE time so the
	// budgets are applied once against the arguments the engine actually
	// saw, and so a reader — the prompt, the dashboard — never needs the raw
	// executions. It also fixes what the arguments were: re-rendering later
	// against a changed surface would silently restate history.
	Calls string `json:"tool_calls,omitempty"`

	// Reply is what the turn said, and it is set ONLY when the turn
	// actually put something where the waiting party can see it.
	//
	// The distinction is the field beside it. Both carry the same value —
	// the turn's final artifact — and which one holds it is the whole
	// record of whether anybody received it.
	Reply string `json:"reply,omitempty"`

	// Unsent is the turn's artifact when NOTHING reached the waiting party.
	//
	// Kept rather than dropped, because the conclusion is real context for
	// the next turn ("last time I decided to file LEAD-1") and losing it
	// would trade one wrong answer for a blank. What it must never do is
	// read as a reply: this was filed as one, so a seat re-reading its own
	// history saw that it had already announced work it had in fact
	// announced to nobody, and the next turn on that thread answered a
	// follow-up against it. A turn can end with real work done and no way
	// to say so — the round budget ran out, the loop broke, the reviewer
	// closed it — and the history has to show that, or the failure repairs
	// itself in the record and nowhere else.
	Unsent string `json:"unsent,omitempty"`

	Decision string `json:"decision,omitempty"`

	// CompletedWork is the reviewer's prose on what already landed —
	// descriptive, so it still reads true a week later.
	//
	// Its sibling, the reviewer's NOTES, is deliberately not carried. That
	// field is a correction scoped to the next round of ONE turn, and is
	// phrased as one ("the next round should retry posting X"). Replayed
	// into a later turn it stops being history and becomes a standing order
	// the reviewer never issued, aimed at a round that already came and
	// went. What actually happened survives without it: the failed calls are
	// in Calls and the outcome is in Decision.
	CompletedWork string `json:"completed_work,omitempty"`

	// BlockedOn is what stopped the turn, from a blocked round's own
	// account of what it tried and what it ran into.
	//
	// TWO AXES, AND [Session.Decision] ONLY CARRIES ONE. That field is the
	// REVIEWER's word for whether the round was good enough; this is the
	// EXECUTOR's for what actually happened. Without it a turn that put a
	// question to somebody and ended there recorded identically to one that
	// finished the work — both `done`, both with whatever prose was in hand
	// — and the seat's next turn on this thread could not tell "I answered
	// this" from "I asked about this and I am waiting".
	//
	// Which is exactly the turn that reads it. The reply to that question is
	// what wakes the seat again, so the entry the next turn opens on is this
	// one, and the message in front of it is very often the answer.
	//
	// THE CALLER DECIDES whether a round was blocked, for the same reason it
	// decides [SessionInput.Delivered]: the outcome vocabulary belongs to
	// [github.com/crewlet/crewlet/internal/agent/turn], and that package
	// imports this one. A string here that is empty on every other outcome
	// keeps the dependency pointing one way instead of spelling `blocked` in
	// a second place for a comparison this package has no business making.
	BlockedOn string `json:"blocked_on,omitempty"`
}

// SessionInput is what the engine has in hand at turn end. BuildSession
// applies every budget once, here, so no later reader has to know them.
type SessionInput struct {
	TurnID        string
	At            string
	Trigger       string
	Intent        string
	Calls         []Call
	Reads         []string
	Skip          []string
	Reply         string
	Decision      string
	CompletedWork string

	// BlockedOn is the blocked round's evidence, and empty on every other
	// outcome. See [Session.BlockedOn] for why the caller and not this
	// package decides that.
	BlockedOn string

	// Delivered says which of [Session.Reply] and [Session.Unsent] the
	// Reply value belongs in. The caller knows; this package cannot.
	Delivered bool
}

// BuildSession assembles one entry.
//
// VERBATIM, apart from the tool ARGUMENTS FormatCalls elides. Every field here
// used to be cut at write time, which made the loss permanent: this row is the
// store's only record of the turn, so a trigger trimmed at 400 runes was not a
// shortened entry, it was the only copy. The turn's own rendering budget is a
// read-side concern (see HistoryOptions), and applying one at write time
// answered a display question by destroying data.
func BuildSession(in SessionInput) Session {
	s := Session{
		TurnID:        in.TurnID,
		At:            in.At,
		Trigger:       in.Trigger,
		Intent:        in.Intent,
		Calls:         FormatCalls(in.Calls, Format(in.Skip, in.Reads)),
		Decision:      in.Decision,
		CompletedWork: in.CompletedWork,
		BlockedOn:     in.BlockedOn,
	}
	// ONE VALUE, TWO FIELDS, and the branch is the record. Writing it to
	// both would make the undelivered case indistinguishable from the
	// delivered one for any reader that checks Reply first.
	if in.Delivered {
		s.Reply = in.Reply
	} else {
		s.Unsent = in.Reply
	}
	return s
}

// InjectedMaxChars bounds the block a TURN is given, by dropping whole entries.
//
// The record is verbatim (see BuildSession); this bounds only the RENDER, and
// it is the one bound this ledger needs. Each entry carries the seat's own
// reply, which is unbounded — a seat whose turn produced a document puts that
// document in the row — so a busy DM's twenty kept entries can be megabytes,
// re-sent on every round of every phase of every later turn in that
// conversation. That is not a prompt-weight preference: past the model's
// context it is a turn that cannot run at all.
//
// WHOLE ENTRIES, oldest dropped first, and the drop is REPORTED — never a cut
// inside an entry, which would leave a half-recorded reply reading as the
// whole of what the seat said. The newest entry always survives however long
// it is: a block trimmed to nothing tells the next turn this conversation has
// no history, which is the one thing it must not conclude.
//
// 24000 bytes is roughly 6k tokens — the order of one iteration of the
// prior-work ledger, and a small fraction of any model this engine targets.
// It bites only on a conversation whose entries are documents rather than
// replies, which is exactly the case an unbounded block cannot serve.
const InjectedMaxChars = 24000

// HistoryOptions bounds a rendered conversation. The zero value is unbounded,
// which is what a reader for DISPLAY — the dashboard — wants.
type HistoryOptions struct {
	// MaxEntries keeps the newest N.
	MaxEntries int
	// MaxChars then drops from the OLDEST end until the block fits. Oldest
	// first because recency is what a follow-up turn needs: the message it
	// is answering is the newest one, and the turn before it is the one most
	// likely to have already answered it.
	MaxChars int
}

// RenderHistory renders prior turns of this conversation as the injected block.
//
// Returns "" when there is nothing to show — the first turn of every
// conversation — so the caller drops the whole section rather than emitting an
// empty heading. Section headings are the caller's, matching RenderIterations.
func RenderHistory(entries []Session, opts HistoryOptions) string {
	if len(entries) == 0 {
		return ""
	}
	selected := entries
	if opts.MaxEntries > 0 && len(selected) > opts.MaxEntries {
		selected = selected[len(selected)-opts.MaxEntries:]
	}
	blocks := make([]string, 0, len(selected))
	for _, entry := range selected {
		blocks = append(blocks, renderSession(entry))
	}
	dropped := len(entries) - len(blocks)
	if opts.MaxChars > 0 {
		// The newest entry always survives, however long it is: a block
		// trimmed to nothing tells the next turn this conversation has no
		// history, which is the one thing it must not conclude.
		for len(blocks) > 1 && len(strings.Join(blocks, "\n\n")) > opts.MaxChars {
			blocks = blocks[1:]
			dropped++
		}
	}
	out := strings.Join(blocks, "\n\n")
	if dropped > 0 {
		// SAID OUT LOUD. A silently shortened history reads as the whole
		// conversation, and a seat that believes it has seen everything it
		// said will not go and look for the rest.
		out = "_(" + itoa(dropped) + " earlier turn(s) in this conversation are not " +
			"shown; re-read the thread itself if you need them.)_\n\n" + out
	}
	return out
}

// renderSession renders one entry as prose.
//
// Second person throughout ("You set out to", "You replied") because the reader
// is the same seat on a later turn: the block is its own past, not a report
// about someone else.
func renderSession(e Session) string {
	head := "### Earlier turn"
	if e.At != "" {
		head = "### " + e.At
	}
	if e.TurnID != "" {
		head += " (turn " + shortID(e.TurnID) + ")"
	}
	lines := []string{head}
	for _, kv := range []struct{ label, value string }{
		{"Triggered by: ", e.Trigger},
		{"You set out to: ", e.Intent},
	} {
		if kv.value != "" {
			lines = append(lines, kv.label+kv.value)
		}
	}
	if e.Calls != "" {
		lines = append(lines, "You called:", e.Calls)
	}
	if e.Reply != "" {
		lines = append(lines, "You replied: "+e.Reply)
	}
	// SPELLED OUT, not implied by the absence of a reply. The reader is a
	// model deciding what this thread still owes, and "no reply line" is
	// something it has to notice; "nobody received this" is something it
	// has to answer. The two readings produce opposite next turns.
	if e.Unsent != "" {
		lines = append(lines, "You did NOT reply — this never reached anyone. "+
			"What the turn concluded: "+e.Unsent)
	}
	if e.CompletedWork != "" {
		lines = append(lines, "Reviewer, on what landed: "+e.CompletedWork)
	}
	// SECOND PERSON and forward-looking, like every other line here. The
	// reader is this seat's own next turn, and where the last one ended on
	// something it could not get past, the thing it most needs to know is
	// that the message now in front of it may well be the answer.
	//
	// Rendered whatever the decision. A blocked round that ended the turn
	// and one the reviewer sent back are different facts, but the reader
	// needs the same thing from both — what the obstacle was — and the
	// decision line below supplies the rest.
	if e.BlockedOn != "" {
		lines = append(lines, "You ended that turn blocked: "+e.BlockedOn)
	}
	// "done" is the unremarkable ending and saying so on every entry trains
	// the reader to skip the line — which is the line that says a turn
	// FAILED.
	if e.Decision != "" && e.Decision != "done" {
		lines = append(lines, "Turn ended: "+e.Decision)
	}
	return strings.Join(lines, "\n")
}

// shortID trims a turn id for display without assuming it is a UUID. Slicing
// [:8] blindly panics on anything shorter, and the id is a string the caller
// supplies.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
