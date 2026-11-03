package prompts

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/toolname"
)

// WHAT A RUN THAT CONTINUES A SESSION IS TOLD.
//
// A relaunch of a turn continues the session its earlier launches worked in
// (internal/sandbox, restore.go), so its brief is the session's NEXT MESSAGE,
// not a whole brief: the session already holds the task as it read it, every
// call it made and every note it read. What it is told is what it lacks —
// what changed since its last message — and, where the box it runs in is not
// the one it was last worked in, what that changes. The fresh brief a run that
// starts a session of its own is handed is [BuildPhaseUserMessage]'s.
//
// A CONVERSATION'S NEXT TURN continues the session its last turn left, in a box
// of its own, and that session holds another turn: none of what this turn was
// asked, and a turn that ended. So its run is told where the turn boundary is
// and what a new box changes ([BuildNextTurn]), and then the turn's whole fresh
// brief, the account of how the conversation's earlier turns ended among it.

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
//
// checkout is the tool the run's surface checks a repository of this
// company's own code host out with (`code_checkout`, ADR-0093), "" where it
// offers none: a company that hosts no code. WHERE IT OFFERS ONE, "a FRESH
// checkout" is true only of a repository the box's setup clones. One the
// session checked out of the code host is in no box until a checkout
// gives it, since nothing is cloned at launch, so a new box holds none of
// them: the run is told to check each out again — the same branch and
// access, and no base, so it is cloned at the branch's head with everything
// pushed there (a round carried on is answered its held grant from that head;
// any other launch resolves the branch's head anew) — rather than told a
// checkout is in front of it that is not.
func BoxReplaced(branch, home, checkout string) string {
	var w strings.Builder
	w.WriteString("## This run is in a new box" +
		"\nThe session above was last worked in a box that is gone: you are in a new one")
	if checkout == "" {
		w.WriteString(", on a FRESH checkout of the repository.")
	} else {
		w.WriteString(".")
	}
	w.WriteString(" Anything it had not committed and pushed went with" +
		" that box, and what the session above says about files on disk is what that box held," +
		" not what this one holds.")
	if checkout != "" {
		w.WriteString("\n\n" + checkoutsAgain(checkout))
	}
	if branch != "" {
		w.WriteString("\n\nCheck out the branch `" + branch + "` it pushed, re-read the code" +
			" there, and redo what is missing from it.")
	} else {
		w.WriteString("\n\nCheck out the work-in-progress branch the session pushed, if it" +
			" pushed one, re-read the code there, and redo what is missing from it.")
	}
	w.WriteString("\n\n" + boxPaths(home))
	return w.String()
}

// checkoutsAgain is what a run continuing a session in a new box is told of
// the repositories that session checked out of this company's own code host,
// where its surface offers checkout, the tool that checks one out: that none
// is in the box until it is checked out again, and how to check it out at its
// branch's head. [BoxReplaced] and [BuildNextTurn] tell it in the same words.
func checkoutsAgain(checkout string) string {
	return "A repository the session checked out of this company's code host is not in this box" +
		" until you check it out again with `" + checkout + "`, naming the same branch and access" +
		" and no `base`: it is cloned at that branch's head, with everything pushed to it. A" +
		" repository the box's setup clones is a FRESH checkout."
}

// boxPaths is what a run continuing a session in a new box is told of the
// session's absolute paths: that they were the box it was last worked in's —
// under home, where the engine knows it, defused by the caller.
func boxPaths(home string) string {
	if home != "" {
		return "Absolute paths in the session above under `" + home + "` were that box's: this" +
			" box's home is `$HOME`, and `pwd` says where its checkout is."
	}
	return "Absolute paths in the session above were that box's: `pwd` and `$HOME` say where" +
		" this box keeps its checkout and its home."
}

// NextTurn is where the session a conversation's last turn left ends and the
// turn that continues it begins, as [BuildNextTurn] tells the run: the ended
// turn whose session it continues and when it ended, where that session was
// last worked, and whether its harness was cut off when it was saved. Each is
// the engine's record of that session, defused by the caller.
//
// Checkout is not: it is the tool the run's surface checks a repository of
// this company's own code host out with, "" where it offers none, which
// decides what the run is told its new box holds ([BoxReplaced]).
type NextTurn struct {
	BaseTurn    string
	EndedAt     time.Time
	Home        string
	Interrupted bool
	Checkout    string
}

// BuildNextTurn is the lead of a run that continues the session its
// conversation's last turn left: the turn boundary the transcript cannot know,
// that what the session read may be stale and what it set up is set up anew,
// that its box is a new one — a session the conversation's last turn left is
// continued only in a box of its own (internal/sandbox, restore.go) — on a
// fresh checkout, or, where its surface offers the code host's checkout,
// holding none of the repositories the session checked out of that host, as
// [BoxReplaced] tells a run — and, where its harness was cut off, that it may
// end mid-call.
// The turn's fresh brief follows it, which tells how the conversation's earlier
// turns ended and what this turn is asked.
//
// NOT THE SAME-TURN LEAD ([BuildContinuedRound]), which says "the same turn,
// and the same session": the session above is another turn's, which ended, and
// this turn's task is none of what it read.
func BuildNextTurn(m NextTurn) string {
	var w strings.Builder
	w.WriteString("## A new turn of this conversation begins here" +
		"\nEverything above is this conversation as your turns left it")
	if m.BaseTurn != "" {
		w.WriteString(", through turn `" + m.BaseTurn + "`")
	}
	if !m.EndedAt.IsZero() {
		w.WriteString(", which ended at " + m.EndedAt.UTC().Format(time.RFC3339))
	}
	w.WriteString(". How the conversation's earlier turns ended is told below. A new message has" +
		" woken you, and the task below is this turn's." +
		"\n- **What you read above may be stale:** read it again before relying on it." +
		"\n- **Tools you activated and skills you loaded above were set up anew for this turn:**" +
		" activate or load them again before relying on them.")
	w.WriteString("\n\n## This turn is in a new box\nYou are in a new box")
	if m.Checkout == "" {
		w.WriteString(", on a FRESH checkout of the repository")
	}
	w.WriteString(": anything the session above had not committed and pushed went with the box it" +
		" was worked in, and what it says about files on disk is what that box held, not what this" +
		" one holds.")
	if m.Checkout != "" {
		w.WriteString("\n\n" + checkoutsAgain(m.Checkout))
	}
	w.WriteString("\n\n" + boxPaths(m.Home))
	if m.Interrupted {
		w.WriteString("\n\n" + InterruptedSession)
	}
	return w.String()
}
