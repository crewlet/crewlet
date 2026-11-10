package prompts

import (
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/toolname"
)

// WHAT A RUN THAT CONTINUES A SESSION IS TOLD.
//
// A relaunch of a turn continues the session its earlier launches worked in
// (internal/sandbox, restore.go), so its brief is the session's NEXT MESSAGE,
// not a whole brief: the session holds the task as it read it, where it holds
// the message that brief was in, and, where its harness ended its own run,
// every call it made and every note it read. What it is told is what it lacks
// — what changed since its last message, and of a session cut off, that it was
// or may have been and what it may not show ([InterruptedSession],
// [InterruptedCopy], [MayBeInterruptedCopy]), with the calls and the notes it
// may lack ([CalledAfter], [NotesReadAgain]) — and, where the box it runs in is
// not the one it was last worked in, what that changes. The fresh brief a run
// that starts a session of its own is handed is [BuildPhaseUserMessage]'s.
//
// A CONVERSATION'S NEXT TURN continues the session its last turn left, in a box
// of its own, and that session holds another turn: none of what this turn was
// asked, and a turn that ended — or, where that turn went on past the session,
// what it did up to where the session was kept. So its run is told where the
// turn boundary is, where the session ends where that is not the boundary, and
// what a new box changes ([BuildNextTurn]), and then the turn's whole fresh
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
//
// TRUE OF EVERY CUT-OFF CAPTURE, whatever packed it: one taken while its
// harness still worked — a job its box would not end, packed as it stood — and
// one taken as or after its harness was stopped, by a stop, a loss, its turn's
// cap or its own death. Said as only the second, it told a run continuing the
// first that its harness had been stopped when it never was.
const InterruptedSession = "## The session above was cut off" +
	"\nIt was saved while its harness was still working, or as or after it was stopped, so it may" +
	" end in the middle of a call — a tool call with no result, which may or may not have run." +
	" Check what such a call was meant to do before doing it again."

// InterruptedCopy is [InterruptedSession] for the copy of a session its job's
// own box holds, continued in place where the job's record says its harness
// was cut off: no save took that copy, so it is told it was cut off in words
// that say nothing of one — and that it may not show the last of what its run
// did, which the calls told after it, every call of the round, are the seat's
// share of. TOLD WHETHER OR NOT A CALL IS LISTED AFTER IT: the call a harness
// was cut off in may be none the round's log holds — one of its own tools,
// which never reach the seat's.
const InterruptedCopy = "## The session above was cut off" +
	"\nThe run that worked in it did not finish cleanly by itself: it was stopped, killed or ended on" +
	" an error before it finished, so the session above may end in the middle of a call — a tool call" +
	" with no result, which may or may not have run — and may not show the last of what it did." +
	" Check what such a call was meant to do before doing it again."

// MayBeInterruptedCopy is [InterruptedCopy] for a copy whose job's record says
// nothing of how its harness ended, or says it in a word this build does not
// know — a job a build before this one collected, a record the coordination
// store did not take: the engine has no word that its run finished cleanly by
// itself and none that it did not, so the copy is told only what is known, and
// what it may then lack. Told where [InterruptedCopy] would be, whether or not
// a call is listed after it.
const MayBeInterruptedCopy = "## The session above may have been cut off" +
	"\nThe engine has no record that the run that worked in it finished cleanly by itself, so the" +
	" session above may end in the middle of a call — a tool call with no result, which may or may" +
	" not have run — and may not show the last of what it did. Check what such a call was meant to" +
	" do before doing it again."

// calledAfterHeader introduces the calls a session's run made through the seat's
// tools after the point the session was saved at.
const calledAfterHeader = "## Calls made after the session above was saved" +
	"\nThe run working in this session made these calls through your tools after the point the" +
	" session above ends, so they are not in it. They happened, and their outcomes stand: a" +
	" call marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked" +
	" `→ error` did not take effect."

// calledSinceHeader introduces the calls a session CUT OFF had its run make
// through the seat's tools after the newest point it is known to hold
// ([AfterSave.Interrupted]) — a kept capture's save point, or, for the copy a
// job's own box holds, the round's start — which is not where it ends: the
// session may show some of them, so they are never said to be missing from it,
// and a call shown in both places is said to be one call, so a model reading it
// twice does not take it for a second one. It says nothing of why the session
// may show only some of them, which the section before it says: that it was
// cut off, or, of a copy whose job's record says nothing, that it may have been
// ([MayBeInterruptedCopy]).
const calledSinceHeader = "## Calls this run made that the session above may not show" +
	"\nThe session above may show some of these calls and not others: they are" +
	" every call the run working in it made through your tools since the last point the engine" +
	" knows it reached. A call shown both above and here is ONE call, made once, not two. They" +
	" happened, and their outcomes stand: a call marked `→ success` ALREADY RAN, so do NOT issue a" +
	" write among them again; one marked `→ error` did not take effect."

// lateCallsHeader introduces the calls of the round its record took only after
// the run that made them was replaced ([AfterSave.Late]): calls an earlier run
// of the work made that were still running when a relaunch carried the work on,
// and that finished after. TOLD UNDER WORDS OF THEIR OWN, WHATEVER THE SESSION'S
// END, since no point tells them and each of the other headings is untrue of
// them: the session above may show such a call with no outcome, or with an
// interrupted one, its harness having died, given up on it or been replaced
// before the call returned; and an earlier brief may have told it, since every
// continuation of the work tells it again. So it is said to be ONE call, made
// once, whose outcome as told here stands — never one the session lacks.
const lateCallsHeader = "## Calls that finished after the run that made them was replaced" +
	"\nAn earlier run of this work made these calls through your tools, and they finished only after that run was" +
	" cut off or replaced, so the session above may show one with no outcome or as interrupted, and an earlier" +
	" message may have told it. Each is ONE call, made once, not two, and the outcome told here stands: a call" +
	" marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked `→ error` did not take" +
	" effect."

// lateSubmission tells a run that a successful `submit_work` among the calls
// under [lateCallsHeader] is no submission of the work it carries on: the run
// that made it was replaced before the call returned, the run carrying the work
// on was never told it, and the work's submission is the one a run carrying it
// on makes (internal/agent/runner's sinceAnswer). Read as the work's, it would
// leave a run that took it for done ending without its own, and the work
// collected as one that submitted nothing.
const lateSubmission = "A `" + toolname.SubmitWork + "` among them does NOT stand as this work's submission:" +
	" the run that made it was replaced before it returned. When your work is done, call `" +
	toolname.SubmitWork + "` yourself."

// AfterSave is what a session's run did through the seat's tools after the
// session was saved, as a run that continues the session is told it
// ([CalledAfter]).
type AfterSave struct {
	// Calls are the calls its round's record still lists that came after the
	// save, as the prior-work ledger renders them within its budgets
	// (ledger.FormatCalls), "" for none.
	Calls string

	// Dropped is how many more calls the run made after the save that its
	// record did not keep one by one, dropped from the middle of a long run's
	// record. EXACT for a record only this version of the engine numbered: it
	// numbers every call it takes, so a count of the numbers past the save it
	// does not list is no estimate. A call an older version appended in a
	// fleet mid-upgrade carries no number, so where such a call is listed it
	// may be counted here as well as listed — an over-count, held to the
	// calls the record dropped at all, never an under-count.
	Dropped int

	// Tally is the record's line for every call it dropped
	// (ledger.FormatElided), those made before the save among them: what
	// Dropped's calls were, by tool, beside calls the session holds. Said
	// only beside a Dropped or a LateDropped it accounts for, and once.
	Tally string

	// Interrupted says the session was, or may have been, cut off
	// ([InterruptedSession], [InterruptedCopy], [MayBeInterruptedCopy]), so
	// the point its calls are told from is the newest one its launch is known
	// to hold, not where it ends (internal/sandbox's savepoint.go) — or, for
	// the copy a job's own box holds where its harness may have been cut off,
	// the round's start (internal/agent/runner's calledInPlace): some of the
	// calls may be in it as well. They are told under [calledSinceHeader],
	// never as calls it lacks, and the dropped ones counted since that point.
	Interrupted bool

	// Late are the calls of the round its record took only after the run that
	// made them was replaced by a relaunch that carried the work on
	// (sandbox.BridgeCall.Late), as the prior-work ledger renders them within
	// its budgets, "" for none: none of them is among Calls. Told in a section
	// of their own, under [lateCallsHeader], whatever Interrupted says.
	Late string

	// LateSubmission says a successful `submit_work` is among Late, which is
	// told as no submission of the work ([lateSubmission]).
	LateSubmission bool

	// LateDropped is how many calls the record took late and then dropped
	// from the middle of a long run's record (sandbox.BridgeLog.LateElided),
	// counted in Late's section ([droppedLate]), beside Tally, where they are
	// the only dropped calls the session lacks: the copy of a session its
	// job's own box holds where its harness ended its own run, which holds
	// every other call its record dropped (internal/agent/runner's
	// inPlaceAfter). Said under the words of calls after a save, they would
	// be calls the session's own run made after it was saved, which neither
	// is true of. A session told the calls after a point counts the late
	// ones it dropped among Dropped instead, beside the other calls dropped
	// past that point.
	LateDropped int
}

// CalledAfter is the section telling a run that continues a session what its
// run did through the seat's tools after the session was saved: the calls the
// record lists, then how many more it kept only in a tally and that tally, the
// way every other reader of a long run's calls is told what was dropped — of a
// session cut off, as calls it may already show ([AfterSave.Interrupted]) —
// and then, in a section of its own, the calls that finished only after the run
// that made them was replaced ([AfterSave.Late]), and how many more such calls
// the record dropped where it counts them there ([AfterSave.LateDropped]). ""
// where there is none of either.
func CalledAfter(after AfterSave) string {
	var sections []string
	if since := calledSince(after); since != "" {
		sections = append(sections, since)
	}
	late := strings.TrimSpace(after.Late)
	if late != "" || after.LateDropped > 0 {
		parts := []string{lateCallsHeader}
		if late != "" {
			parts = append(parts, late)
		}
		if after.LateSubmission {
			parts = append(parts, lateSubmission)
		}
		if after.LateDropped > 0 {
			parts = append(parts, droppedLate(after.LateDropped))
			// THE TALLY ONCE: beside the calls after the save where the record
			// dropped any of those, and here where it dropped only late ones.
			if tally := strings.TrimSpace(after.Tally); tally != "" && after.Dropped <= 0 {
				parts = append(parts, tally)
			}
		}
		sections = append(sections, strings.Join(parts, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// calledSince is [CalledAfter]'s section of the calls after the save — or,
// of a session cut off, since the newest point it is known to hold — "" where
// the record lists none and dropped none.
func calledSince(after AfterSave) string {
	calls := strings.TrimSpace(after.Calls)
	if calls == "" && after.Dropped <= 0 {
		return ""
	}
	header := calledAfterHeader
	if after.Interrupted {
		header = calledSinceHeader
	}
	parts := []string{header}
	if calls != "" {
		parts = append(parts, calls)
	}
	if after.Dropped > 0 {
		parts = append(parts, droppedAfter(after.Dropped, after.Interrupted))
		if tally := strings.TrimSpace(after.Tally); tally != "" {
			parts = append(parts, tally)
		}
	}
	return strings.Join(parts, "\n")
}

// droppedAfter says that n of the calls a session's run made after it was
// saved are counted only in its record's tally, which says what they were
// beside calls the session holds, and what a call counted only there asks of
// the run: it has no outcome to go by. Of a session cut off (interrupted),
// they are the calls since the point its section says, not since a save.
func droppedAfter(n int, interrupted bool) string {
	calls := "calls"
	if n == 1 {
		calls = "call"
	}
	since, before := "after the session above was saved", "before the save"
	if interrupted {
		since, before = "since that point", "before that point"
	}
	return "(+" + strconv.Itoa(n) + " further " + calls + " this run made " + since + "," +
		" which its record did not keep one by one: past two hundred calls a run's record keeps its first and" +
		" last and only a tally of those between. The tally below counts calls made " + before + " as well." +
		" A call counted only in the tally has no outcome here: check whether its tool already did what it" +
		" was for before doing it again.)"
}

// droppedLate says that n more calls like the ones under [lateCallsHeader] are
// counted only in the record's tally ([AfterSave.LateDropped]), and what a call
// counted only there asks of the run, as [droppedAfter] says of the calls after
// a save: it has no outcome to go by. Said as calls the record took late — made
// by an earlier run, never this session's run after a save — and as counted
// beside other calls, which the tally names too.
func droppedLate(n int) string {
	calls := "calls"
	if n == 1 {
		calls = "call"
	}
	return "(+" + strconv.Itoa(n) + " further such " + calls + ", which the record did not keep one by one:" +
		" past two hundred calls a run's record keeps its first and last and only a tally of those between." +
		" The tally below counts other calls as well. A call counted only in the tally has no outcome here:" +
		" check whether its tool already did what it was for before doing it again.)"
}

// notesAgainHeader opens the notes a session that may have been cut off is told
// again ([NotesReadAgain]).
const notesAgainHeader = "## Notes this turn read that the session above may not show"

// NotesReadAgain is the section that tells a session its run may have been cut
// off in the notes the turn has read again, each already rendered as the model
// reads a note ([SteerMessage]), "" for none: the session may not show them, and
// may show some.
//
// SAID AS NOTES IT MAY ALREADY SHOW, ONE NOTE WHEREVER SHOWN, AND COUNTED. Each
// note says it stands for the rest of the turn, so a model reading one both above
// and here could do twice what it asks; and the notes the brief is the first to
// read follow these in the same form, which the count tells apart.
//
// AND THE NOTES AFTER THEM AS ONES THE TURN HAD NOT READ, never as new to the
// session. Which notes the turn has read is the run's record's word, and a
// harness can be handed a note before the record says it was read: OpenCode's
// delivery is recorded read only once its plugin confirms it, so a harness gone
// before its confirmation landed leaves a session that may show a note the
// brief takes as waiting.
func NotesReadAgain(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	which := "the note below"
	if len(notes) > 1 {
		which = "the " + strconv.Itoa(len(notes)) + " notes below"
	}
	return notesAgainHeader + "\nThe session above may not show " + which + ": every note a person sent" +
		" this turn that the turn has read so far, in the order it read them. A note shown both above and" +
		" here is ONE note, sent once, not two: do what it asks once. A note after them is one the turn" +
		" had not read before this message." +
		"\n\n" + strings.Join(notes, "\n\n")
}

// BoxReplaced is what a run that continues its session in a box other than the
// one the session was last worked in is told beside its continuation: the
// facts a fresh run's reseed carries — a fresh checkout, which branch to check
// out, the uncommitted work gone — and that the session's absolute paths were
// another machine's. branch is the work-in-progress branch the session pushed,
// where the engine knows it, and home the earlier box's home, where it knows
// it; each is the engine's record of the session, defused by the caller.
//
// checkout is the tool the run's surface checks a repository of
// Gitlet out with (`code_checkout`, ADR-0093), "" where it
// offers none: a company that hosts no code. WHERE IT OFFERS ONE, "a FRESH
// checkout" is true only of a repository the box's setup clones. One the
// session checked out of Gitlet is in no box until a checkout
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
// the repositories that session checked out of Gitlet,
// where its surface offers checkout, the tool that checks one out: that none
// is in the box until it is checked out again, and how to check it out at its
// branch's head. [BoxReplaced] and [BuildNextTurn] tell it in the same words.
func checkoutsAgain(checkout string) string {
	return "A repository the session checked out of Gitlet is not in this box" +
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

// keptSessionHeader titles what a fresh brief says of a kept session its run
// does not continue.
const keptSessionHeader = "## A kept session this run does not continue"

// KeyKeptSession keys the section [KeptSession] is appended to a brief as.
const KeyKeptSession = "kept_session"

// KeptSession is what a run that starts a session of its own is told of a kept
// session it does not continue, which its model cannot otherwise know is there
// to read: this turn's earlier launches' (thisTurn), or the session its
// conversation's last turn left; and that `read_session` reads it under turn's
// id, `match` finding a word in it. turn is the engine's record, defused by the
// caller.
//
// A SCHEDULE'S FIRE IS TOLD IN THE SCHEDULE'S WORDS (fire), as [BuildNextTurn]
// and the endings block tell it: the session its schedule's last fire left, or
// this fire's earlier launches'. Everywhere else in its brief a run its
// schedule woke is told of fires, and "this conversation's last turn" beside
// them names a turn nothing else in the brief speaks of.
//
// ONE SENTENCE, AND ONLY HERE: a standing sentence about the session tools
// would ride every request of every run and say what their descriptions say,
// and this is the one place a model cannot know a session it could read
// exists — a launch that could not continue the turn's own, or the one its
// conversation's last turn left.
//
// NEVER WHY IT IS NOT CONTINUED. Why a launch continues no session is said in
// words for the log, and they quote what said no: a store's or a router's
// error, a refused capture's reason, which carries what a box printed. Told
// to the run in the engine's own voice, they put what a box wrote outside any
// fence and an error meant for an operator into a vendor model's prompt; and
// what the run can do about it is the same whatever the reason — read what
// that session did.
func KeptSession(turn string, thisTurn, fire bool) string {
	words := nextTurnWordsFor(NextTurn{Fire: fire})
	whose, what := "This "+words.whole+"'s last "+words.turn, "it did"
	if thisTurn {
		whose, what = "This "+words.turn+"'s earlier launches", "they did"
	}
	return keptSessionHeader + "\n" + whose + " worked in a session this run does not continue. Read what " +
		what + " with `" + toolname.ReadSession + "`, turn_id `" + turn + "`; `match` finds a word in it."
}

// NextTurn is where the session a conversation's last turn left ends and the
// turn that continues it begins, as [BuildNextTurn] tells the run: the ended
// turn whose session it continues and when it ended, where that session was
// last worked, and whether its harness was cut off when it was saved. Each is
// the engine's record of that session, defused by the caller.
//
// Checkout is not: it is the tool the run's surface checks a repository of
// Gitlet out with, "" where it offers none, which
// decides what the run is told its new box holds ([BoxReplaced]).
type NextTurn struct {
	BaseTurn    string
	EndedAt     time.Time
	Home        string
	Interrupted bool
	Checkout    string

	// WentOn says the session's turn went on past it, as that turn's end said:
	// the session was kept while the turn still worked, at CapturedAt (zero
	// where its row recorded no instant), so it ends there and not where the
	// turn did. Lost says the turn ended lost — the engine, not the turn,
	// ended it, or its end was never recorded (internal/executorkeep's
	// OutcomeLost) — so, where its end did not say WentOn, the session may
	// still not hold the last of what it did, which the engine cannot say more
	// of (internal/executorkeep's TurnRow.WentOn).
	WentOn     bool
	Lost       bool
	CapturedAt time.Time

	// Fire says the conversation is a schedule's, each of its turns a fire
	// (internal/notify's Scheduled): the run is told that the schedule has
	// fired again and that the session above is its last fire's.
	Fire bool
}

// nextTurnWords are the words a lead tells the turn boundary in: what the
// conversation and its turns are called, and what woke the run.
type nextTurnWords struct {
	turn, whole, woke string
}

// nextTurnWordsFor is the lead's words for m: a schedule's fire, or a
// conversation's turn.
func nextTurnWordsFor(m NextTurn) nextTurnWords {
	if m.Fire {
		return nextTurnWords{turn: "fire", whole: "schedule", woke: "The schedule has fired again"}
	}
	return nextTurnWords{turn: "turn", whole: "conversation", woke: "A new message has woken you"}
}

// named is how the lead names the turn whose session it continues: by its id,
// where the engine's record of the session names one.
func (w nextTurnWords) named(turn string) string {
	if turn == "" {
		return "your last " + w.turn
	}
	return w.turn + " `" + turn + "`"
}

// BuildNextTurn is the lead of a run that continues the session its
// conversation's last turn left: the turn boundary the transcript cannot know,
// that what the session read may be stale and what it set up is set up anew,
// that its box is a new one — a session the conversation's last turn left is
// continued only in a box of its own (internal/sandbox, restore.go) — on a
// fresh checkout, or, where its surface offers Gitlet's checkout,
// holding none of the repositories the session checked out of that host, as
// [BoxReplaced] tells a run — and, where its harness was cut off, that it may
// end mid-call.
// The turn's fresh brief follows it, which tells how the conversation's earlier
// turns ended and what this turn is asked. A schedule's fire is told the same
// in the schedule's words ([NextTurn.Fire]): the schedule's last fire left the
// session, and the schedule firing again, not a message, is what woke the run.
//
// NOT THE SAME-TURN LEAD ([BuildContinuedRound]), which says "the same turn,
// and the same session": the session above is another turn's, which ended, and
// this turn's task is none of what it read.
//
// AND NEVER "THROUGH" A TURN THAT WENT ON PAST ITS SESSION ([NextTurn.WentOn]):
// that session was kept while the turn still worked, so the lead says it ends
// where it was kept, that what the turn did after it is not above, and that how
// the turn ended is told below — saying too, of a turn that was then lost, that
// it was. Told it ran through the turn's end, the run would take the turn's
// last state for the session's, and redo or contradict what the turn did after
// it. A turn that was lost ([NextTurn.Lost]) and whose end did not say it went
// on is told through, which is as far as the engine knows the session runs,
// and that the session may not hold the last of what it did: no frame of that
// turn saw it end.
func BuildNextTurn(m NextTurn) string {
	words := nextTurnWordsFor(m)
	var w strings.Builder
	w.WriteString("## A new " + words.turn + " of this " + words.whole + " begins here" +
		"\nEverything above is this " + words.whole + " as your " + words.turn + "s left it")
	ended := "How the " + words.whole + "'s earlier " + words.turn + "s ended is told below."
	switch {
	case m.WentOn:
		w.WriteString(", up to a session kept while " + words.named(m.BaseTurn) + " still worked")
		if !m.CapturedAt.IsZero() {
			w.WriteString(", at " + m.CapturedAt.UTC().Format(time.RFC3339))
		}
		w.WriteString(": what that " + words.turn + " did after it is not above")
		if m.Lost {
			w.WriteString(", and that " + words.turn + " was lost")
		}
		ended = "How " + words.named(m.BaseTurn) + " ended, among the " + words.whole + "'s earlier " +
			words.turn + "s, is told below."
	case m.Lost:
		w.WriteString(", through " + words.named(m.BaseTurn) + ", which was lost")
		if !m.EndedAt.IsZero() {
			w.WriteString(" and ended at " + m.EndedAt.UTC().Format(time.RFC3339))
		}
		w.WriteString(": the session above may not hold the last of what that " + words.turn + " did")
		ended = "How " + words.named(m.BaseTurn) + " ended, among the " + words.whole + "'s earlier " +
			words.turn + "s, is told below."
	default:
		if m.BaseTurn != "" {
			w.WriteString(", through " + words.named(m.BaseTurn))
		}
		if !m.EndedAt.IsZero() {
			w.WriteString(", which ended at " + m.EndedAt.UTC().Format(time.RFC3339))
		}
	}
	w.WriteString(". " + ended + " " +
		words.woke + ", and the task below is this " + words.turn + "'s." +
		"\n- **What you read above may be stale:** read it again before relying on it." +
		"\n- **Tools you activated and skills you loaded above were set up anew for this " + words.turn +
		":** activate or load them again before relying on them.")
	w.WriteString("\n\n## This " + words.turn + " is in a new box\nYou are in a new box")
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
