package prompts

import (
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
)

// WHAT A RUN THAT CONTINUES ITS TURN'S SESSION IS TOLD.
//
// A relaunch of a turn continues the session its earlier launches worked in
// (internal/sandbox, restore.go), so its brief is the session's NEXT MESSAGE,
// not a whole brief: the session already holds the task as it read it, every
// call it made and every note it read. What it is told is what it lacks —
// what changed since its last message — and, where the box it runs in is not
// the one it was last worked in, what that changes. The fresh brief a run that
// starts a session of its own is handed is [BuildPhaseUserMessage]'s.

// sentBackHeader opens a reviewer's send-back to the session that did the round.
const sentBackHeader = "## Your work was reviewed and sent back" +
	"\nThis is the same turn, and the same session: everything above — what you read, tried," +
	" decided and called — is this turn's work so far, and you carry it on from where it" +
	" stands rather than starting again."

// ContinuedRoundRules is [PriorWorkHeader]'s rules for a run that continues the
// session its earlier rounds of the turn worked in: those rounds' calls are in
// the session above, so it is told the rules without the list.
//
// THE RULES AND NOT THE LIST, and the rules still: the session remembers each
// call, but not that a write an earlier round made must not be repeated, nor
// that a delivery an earlier round made is citable — a model forbidden to
// repeat a write it cannot cite submitted with no citation, or went looking
// for some other surface to deliver on.
const ContinuedRoundRules = "## Already done earlier in this turn" +
	"\nYour earlier rounds of this turn are in this session above, and every call they made" +
	" that succeeded ALREADY RAN:" +
	"\n- **A read** fetched data only: re-run it if you need the data now, never invent it." +
	"\n- **Everything else** changed something outside the engine: do NOT issue it again — a" +
	" person sees the second one as a duplicate. If it was the delivery, it still counts: name" +
	" it in `" + toolname.SubmitWork + "`'s `deliveries`." +
	"\n- **A call that failed** did not take effect: retry it, fixed." +
	"\nDeliver only what is still missing. A follow-up to an earlier delivery ADDS to it — reply" +
	" in its thread, edit the item — rather than re-sending it, and is not a duplicate."

// differentlyHeader introduces what the review asked of the round that comes next.
const differentlyHeader = "## What to do differently"

// ContinuedRound is a reviewer's send-back as the session that did the round
// reads it: the task restated, since a compacted session holds only its own
// summary of it; the rules its earlier rounds' calls are read by; and what the
// review and the engine asked of the next round. The fourth part, every note a
// person sent the turn that the session has not read and then the ones this
// brief is the first to read, follows it as the notes follow a fresh brief
// ([UserMessage.Steered]), each rendered by [SteerMessage].
type ContinuedRound struct {
	Task   string
	Review string
}

// BuildContinuedRound renders a send-back's next message to the session.
func BuildContinuedRound(m ContinuedRound) Prompt {
	task := m.Task
	if task == "" {
		task = "(no description)"
	}
	b := NewBuilder("\n\n")
	b.Heading("sent_back", sentBackHeader)
	b.Heading("task", taskHeader+"\n"+task)
	b.Heading("prior_work", ContinuedRoundRules)
	if review := strings.TrimSpace(m.Review); review != "" {
		b.Heading("review", differentlyHeader+"\n"+review)
	}
	return b.Build()
}

// InterruptedSession tells a run that continues a session its harness was cut
// off in that the session above may end in the middle of a call: a tool call
// with no result, which may or may not have run.
const InterruptedSession = "## The session above was cut off" +
	"\nIt was saved while, or after, its harness was stopped, so it may end in the middle of a" +
	" call — a tool call with no result, which may or may not have run. Check what such a call" +
	" was meant to do before doing it again."

// calledAfterHeader introduces the calls a session's run made through the seat's
// tools after the point the session was saved at.
const calledAfterHeader = "## Calls made after the session above was saved" +
	"\nThe run working in this session made these calls through your tools after the point the" +
	" session above ends, so they are not in it. They happened, and their outcomes stand: a" +
	" call marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked" +
	" `→ error` did not take effect."

// CalledAfter is the section listing calls (as the prior-work ledger renders
// them, ledger.FormatCalls) a session's run made after the session was saved,
// or "" where it made none.
func CalledAfter(calls string) string {
	if strings.TrimSpace(calls) == "" {
		return ""
	}
	return calledAfterHeader + "\n" + calls
}

// BoxReplaced is what a run that continues its session in a box other than the
// one the session was last worked in is told beside its continuation: the
// facts a fresh run's reseed carries — a fresh checkout, which branch to check
// out, the uncommitted work gone — and that the session's absolute paths were
// another machine's. branch is the work-in-progress branch the session pushed,
// where the engine knows it, and home the earlier box's home, where it knows
// it; each is the engine's record of the session, defused by the caller.
func BoxReplaced(branch, home string) string {
	var w strings.Builder
	w.WriteString("## This run is in a new box" +
		"\nThe session above was last worked in a box that is gone: you are in a new one, on a" +
		" FRESH checkout of the repository. Anything it had not committed and pushed went with" +
		" that box, and what the session above says about files on disk is what that box held," +
		" not what this one holds.")
	if branch != "" {
		w.WriteString("\n\nCheck out the branch `" + branch + "` it pushed, re-read the code" +
			" there, and redo what is missing from it.")
	} else {
		w.WriteString("\n\nCheck out the work-in-progress branch the session pushed, if it" +
			" pushed one, re-read the code there, and redo what is missing from it.")
	}
	if home != "" {
		w.WriteString("\n\nAbsolute paths in the session above under `" + home + "` were that" +
			" box's: this box's home is `$HOME`, and `pwd` says where its checkout is.")
	} else {
		w.WriteString("\n\nAbsolute paths in the session above were that box's: `pwd` and" +
			" `$HOME` say where this box keeps its checkout and its home.")
	}
	return w.String()
}
