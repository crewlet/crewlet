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
// ledger carries — plan, calls, artifact, verdict — under the same budgets,
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

	Reply    string `json:"reply,omitempty"`
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
}

// BuildSession assembles one entry, applying every budget once at write time.
func BuildSession(in SessionInput) Session {
	return Session{
		TurnID:        in.TurnID,
		At:            in.At,
		Trigger:       elide(in.Trigger, TriggerLimit),
		Intent:        elide(in.Intent, IntentLimit),
		Calls:         FormatCalls(in.Calls, Format(in.Skip, in.Reads)),
		Reply:         elide(in.Reply, ReplyLimit),
		Decision:      in.Decision,
		CompletedWork: elide(in.CompletedWork, NoteLimit),
	}
}

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
	if opts.MaxChars > 0 {
		// The newest entry always survives, however long it is: a block
		// trimmed to nothing tells the next turn this conversation has no
		// history, which is the one thing it must not conclude.
		for len(blocks) > 1 && len(strings.Join(blocks, "\n\n")) > opts.MaxChars {
			blocks = blocks[1:]
		}
	}
	return strings.Join(blocks, "\n\n")
}

// renderSession renders one entry as prose.
//
// Second person throughout ("You planned", "You replied") because the reader
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
	if e.CompletedWork != "" {
		lines = append(lines, "Reviewer, on what landed: "+e.CompletedWork)
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
