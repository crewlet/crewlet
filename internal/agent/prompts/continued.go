package prompts

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// WHAT A RUN THAT CONTINUES A SESSION IS TOLD: its NEXT MESSAGE, not a whole
// brief (the session holds the task, internal/sandbox restore.go), and only
// what it lacks: what changed, whether it was cut off and what it may not show
// ([InterruptedSession], [InterruptedCopy], [MayBeInterruptedCopy],
// [SavedSession]), the calls and notes it may lack ([LeadsToldAgain],
// [CalledAfter], [NotesReadAgain]), and what a new box changes. A run starting
// a session gets [BuildPhaseUserMessage]'s brief ([CalledBefore] for a round
// carried on). A CONVERSATION'S NEXT TURN continues a session holding another
// turn, so its run is told where the turn boundary is and where the session
// ends ([BuildNextTurn]), then the turn's whole fresh brief.

// sentBackHeader opens a reviewer's send-back to the session that did the round.
// A send-back told again says it in [sentBackAgainHeader]'s words instead.
const sentBackHeader = "## Your work was reviewed and sent back" +
	"\nThis is the same turn, and the same session: everything above — what you read, tried," +
	" decided and called — is this turn's work so far, and you carry it on from where it" +
	" stands rather than starting again."

// ContinuedRoundRules is [PriorWorkHeader]'s rules for a run that continues the
// session its earlier rounds of the turn worked in: those rounds' calls are in
// the session above, so it is told the rules without the list. The session
// remembers each call, but not that an earlier write must not be repeated nor
// that its delivery is citable: without that a model submitted with no citation.
//
// A FAILED CALL WHOSE OUTCOME IS UNKNOWN IS EXCEPTED, as [PriorWorkHeader]
// excepts it: a call in flight when an earlier round was read is on its record
// failed, and the late calls' section told the session its outcome is unknown.
// Told beside it that a failed call did not take effect, the run would retry it.
const ContinuedRoundRules = "## Already done earlier in this turn" +
	"\nYour earlier rounds of this turn are in this session above, and every call they made" +
	" that succeeded ALREADY RAN:" + continuedRoundRules

// sentBackAgainHeader is [sentBackHeader] for a send-back told again
// ([ContinuedRound.Again]): the turn's work was reviewed and sent back, and it
// is carried on from where it stands, whatever the session above holds of it.
const sentBackAgainHeader = "## Your work was reviewed and sent back" +
	"\nA review of this turn's work sent it back. This is the same turn: what its earlier rounds read, tried," +
	" decided and called is this turn's work so far, whether or not the session above shows it, and you carry" +
	" it on from where it stands rather than starting again."

// continuedRoundRulesAgain is [ContinuedRoundRules] for a send-back told again
// ([ContinuedRound.Again]): the same rules, said of every call the turn's
// earlier rounds made rather than of the calls in the session above, which may
// hold none of them.
const continuedRoundRulesAgain = "## Already done earlier in this turn" +
	"\nEvery call your earlier rounds of this turn made that succeeded ALREADY RAN, whether or not the" +
	" session above shows it:" + continuedRoundRules

// continuedRoundRules is the rules [ContinuedRoundRules] and
// [continuedRoundRulesAgain] read a turn's earlier calls by, after the line
// that says which calls those are.
const continuedRoundRules = "\n- **A read** fetched data only: re-run it if you need the data now, never invent it." +
	"\n- **Everything else** changed something outside the engine: do NOT issue it again — a" +
	" person sees the second one as a duplicate. If it was the delivery, it still counts: name" +
	" it in `" + toolname.SubmitWork + "`'s `deliveries`." +
	"\n- **A call that failed** did not take effect, unless it says its outcome is unknown: that one may have," +
	" so check whether it did before making it again. Retry any other, fixed." +
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

	// Again says the send-back is told again, as a lead a later brief tells
	// a session of the work that may lack it (internal/agent/runner's
	// leadAgain), rather than sent to the session that did the round: told
	// in words true of any session of the work ([sentBackAgainHeader],
	// [continuedRoundRulesAgain]). A continuation that may be a summary is
	// told every lead the run keeps, a send-back that continued another
	// session among them, and hands them on in turn, so "the same session"
	// and "in this session above" are no longer known of the session a
	// send-back told again reaches.
	Again bool
}

// BuildContinuedRound renders a send-back's next message to the session, or,
// told again ([ContinuedRound.Again]), the send-back as any session of the
// work may read it.
func BuildContinuedRound(m ContinuedRound) Prompt {
	task := m.Task
	if task == "" {
		task = "(no description)"
	}
	sentBack, rules := sentBackHeader, ContinuedRoundRules
	if m.Again {
		sentBack, rules = sentBackAgainHeader, continuedRoundRulesAgain
	}
	b := NewBuilder("\n\n")
	b.Heading("sent_back", sentBack)
	b.Heading("task", taskHeader+"\n"+task)
	b.Heading("prior_work", rules)
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
// first that its harness had been stopped when it never was. A capture its job
// went on past, never that job's final one, is told [SavedSession] in its
// place, which says this and that the job went on.
const InterruptedSession = "## The session above was cut off" +
	"\nIt was saved while its harness was still working, or as or after it was stopped, so it may" +
	" end in the middle of a call — a tool call with no result, which may or may not have run." +
	" Check what such a call was meant to do before doing it again."

// SavedSession tells a run that continues a session saved while the run that
// worked in it still worked (internal/agent/runner's SessionBasis.WentOn) that
// it was saved then, and when (at), and that the run went on: the session ends
// where it was saved, not where its run stopped, and may end mid-call as
// [InterruptedSession] says. Told only that, the run would take the session's
// end for where its work stopped, while its job went on changing files no
// record keeps; so it is told to read the working tree first. "Any call ... is
// told below", never "what it did": a run that made no call after the save
// gets no list ([CalledAfter]), and the words stay true. With at zero the
// instant is left out, as [BuildNextTurn] leaves out a zero CapturedAt.
func SavedSession(at time.Time) string {
	var w strings.Builder
	w.WriteString("## The session above was saved while its run still worked\nIt was saved")
	if !at.IsZero() {
		w.WriteString(" at " + at.UTC().Format(time.RFC3339) + ",")
	}
	w.WriteString(" while the run that worked in it was still working, and that run went on after it: the" +
		" session above ends where it was saved, not where the run stopped. So it may end in the middle of a" +
		" call: a tool call with no result, which may or may not have run. Any call the run made through your" +
		" tools after it is told below. What else it did after it, the commands it ran and the files it read" +
		" or changed, is in no record: check the working tree, and what any such call was meant to do, before" +
		" doing any of it again.")
	return w.String()
}

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

// MayCompactSession tells a run continuing a session its harness may hand it as a
// summary (internal/agent/runner's firstRequest) why what carried the work on,
// its calls and its notes are told again below: Claude Code compacts a session
// near its window's size before a request, its resumed run's first among them,
// which leaves the model a summary of everything before its last reply, and a
// session may have compacted, or started over, as its job worked, after its
// brief. NEVER "CUT OFF": the session may be whole and may still show all of
// it, so each told again is said to be one message, call or note, not two.
// Where the job's record also says it was cut off, the cut-off words are told
// in its place ([InterruptedCopy], [MayBeInterruptedCopy],
// [InterruptedSession], [SavedSession]), which say so and more.
const MayCompactSession = "## The session above may reach you as a summary" +
	"\nIts harness may have compacted it as this work went on, or may compact it before going on: a" +
	" session near its window's size is compacted, which replaces everything before its last reply" +
	" with a summary of it. So what carried this work on, every call of this round and every note" +
	" sent while it worked are told again below. The session above may still show some or all of" +
	" them, in full or in its summary: each is ONE message, call or note, made or sent once, not two."

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

// readHeader introduces the calls after the newest point a session cut off is
// known to hold where a reading of its archive found it holding some of them,
// which the list leaves out ([AfterSave.Read]): in place of [calledSinceHeader],
// whose "they are every call ... since the last point" is false of a thinned list.
// TRUE ON EVERY PATH A READING TAKES: the session may still show some listed
// calls (one the reading could not hold whole, was not asked about or could not
// place), so a call shown in both places is still one call. "Less any it found
// the session holds or lists below" is true of the calls it held, which no list
// tells, and of the late and subagent calls, which sections below list. Never
// "any" of what it left out, which is true of none. NO LONGER THAN THE HEADER IT
// REPLACES (471 bytes each).
const readHeader = "## Calls this run made that the session above may not show" +
	"\nThe session above may show some of these: the run's calls through your tools since the last point" +
	" the engine knows it reached, less any it found the session holds or lists below. A call shown both" +
	" above and here is ONE call, made once, not two. They happened, and their outcomes stand: a call" +
	" marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked `→ error` did" +
	" not take effect."

// subagentsHeader heads the calls a reading of a session cut off found in the
// threads of subagents whose report the session does not hold
// ([AfterSave.Subagents]), each under a heading naming its subagent
// ([subagentsSection]). Claude Code hands a resumed session's model the main
// thread alone, which holds a subagent's work only as its report, and the
// reading found none for these: so the session surely lacks what each call
// returned, and the words claim nothing of it, never that a call is shown
// twice.
const subagentsHeader = "## Calls subagents of this run made" +
	"\nThe run working in the session above started each subagent named below, and the session may not hold" +
	" its report of what it did. These are the calls each made through your tools. They happened, and their" +
	" outcomes stand: a call marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one" +
	" marked `→ error` did not take effect."

// SubagentCalls is the calls a reading told under one subagent's name
// ([AfterSave.Subagents]): its agent id (Agent) and name (Name, "" for none),
// each defused by the caller, the name the box's words cut to a value's
// length; its calls (Calls), rendered as the prior-work ledger renders a
// round's calls, under the one read cap; and whether a successful
// `submit_work` is among them (Submission), said not to stand right after
// them ([earlierSubmission]).
type SubagentCalls struct {
	Agent, Name, Calls string
	Submission         bool
}

// subagentsSection is [CalledAfter]'s section of the calls told under
// subagents' names: [subagentsHeader], then per subagent with a call listed a
// `### Subagent <agent>: <name>` line (`### Subagent <agent>` where it has no
// name), its calls, and that a submission among them does not stand where one
// is ([SubagentCalls.Submission]). "" where none lists a call.
func subagentsSection(subs []SubagentCalls) string {
	var parts []string
	for _, s := range subs {
		calls := strings.TrimSpace(s.Calls)
		if calls == "" {
			continue
		}
		heading := "### Subagent " + s.Agent
		if s.Name != "" {
			heading += ": " + s.Name
		}
		part := heading + "\n" + calls
		if s.Submission {
			part += "\n" + earlierSubmission
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	return subagentsHeader + "\n\n" + strings.Join(parts, "\n\n")
}

// shownAgainHeader introduces the calls the session above may show that a
// brief lists again ([AfterSave.Shown]), each one a call in another list could
// be taken for. It claims only that the session MAY show each: a point's claim
// is not what the session reads back (a Claude Code subagent's bridge calls are
// held where its session was saved past them, while the resumed main thread
// shows only its report; runner's closedShown, heldAt). It says only "could be
// taken for", the one reason true on every path (runner's readAlike): arguments
// reading alike is one of several (in flight, sealed for approval, kept less
// than whole). It names neither list, which differ by path; "another list of
// this message" is true of both.
const shownAgainHeader = "## Calls the session above may show, listed again" +
	"\nThe session above may show each of these calls. Each is listed again because a call in another list" +
	" of this message names the same tool and could be taken for it: that one is a call of its own, made as" +
	" well as this one, never this one shown twice."

// EarlierRoundNote is the note a call listed again under [shownAgainHeader]
// ends its line with where it is a call of one of the turn's earlier rounds,
// closed before the round whose calls the brief tells (internal/agent/runner's
// closedShown): it says where in the session above the call may be, which
// anchors it to the rounds the session holds before this one. fire says the
// conversation is a schedule's, whose turns are its fires ([NextTurn.Fire]),
// so the words name a fire where they would name a turn.
//
// NEVER ON A CALL OF THE ROUND ITSELF, which a brief lists again as well: on a
// send-back that round is the one just sent back, so "this round" would be
// false of it, and "an earlier round" is true of none of its calls.
func EarlierRoundNote(fire bool) string {
	return "(made in an earlier round of this " + nextTurnWordsFor(NextTurn{Fire: fire}).turn + ")"
}

// ToldApartNote is the note a told call's line ends with where another call
// the same message tells could be taken for it (internal/agent/runner's
// notes): a line of another list, a line or tally of the work already done, a
// held call a lead tells, or the call a person approved. Without it, the words
// saying a call shown twice is ONE call would let a model read two as one and
// lose one. TRUE WHEREVER PRINTED: every partner is a call this message tells,
// never the marked call itself. So it says "could be taken for", as
// [shownAgainHeader] does, and never which call: the partner may be a tally's
// count or a lead's words, which no line names. The prior-work line carries
// none: its render is shared with the reviewer.
const ToldApartNote = "(a call of its own: another call this message tells could be taken for it)"

// otherCallsHeader heads the calls the session above may show that no list of
// the brief below names ([AfterSave.Other]); otherCallsOpening opens them and
// otherCallsClosing follows their parts. WHY WORDS, NOT LINES: the engine
// cannot list these calls (an earlier turn's, an earlier round's held only in a
// summary, ones kept only as a tool and a count, a worker's, which reaches the
// session only as its report), yet each could be taken for a told call and
// lose it. TRUE OF EVERY PART: "may" of each (runner's otherCalls), and "no
// list below holds one by one": the record lists a call or counts it, never
// both. NEVER "NO LIST BELOW NAMES": the tally (AfterSave.Tally) names the same
// calls by tool, so such words would have a model count them twice.
const (
	otherCallsHeader  = "## Other calls the session above may show"
	otherCallsOpening = "The session above may also show calls no list below holds one by one:"
	otherCallsClosing = "However alike one of them reads to a call listed below, it is a call of its own, never that" +
		" one shown twice."
)

// OtherCalls is what the session above may show that no list of a brief below
// names ([AfterSave.Other]), each part told only where it holds: its calls by
// the tools the told calls name, never by a count, since the count the record
// keeps is no figure of what the session shows.
type OtherCalls struct {
	// EarlierTurns says the session may hold the conversation's earlier turns:
	// the turn continued the session its conversation's last turn left. Fire
	// says the conversation is a schedule's, whose turns are its fires
	// ([NextTurn.Fire]), so every part names a fire where it would a turn.
	EarlierTurns, Fire bool
	// EarlierRounds is the told tools of the turn's earlier rounds where the
	// session may hold those rounds only in a summary's words, less the calls
	// listed again; Unlisted the told tools of the calls the record no longer
	// lists one by one; Workers the told tools of the calls workers made under
	// a delegation.
	EarlierRounds, Unlisted, Workers ToolsNamed
}

// ToolsNamed is the tools a part of [OtherCalls] names, defused by the
// caller, in the order the told calls name them, and whether a count it speaks
// of names no tool. A part is told where either holds.
type ToolsNamed struct {
	Tools   []string
	Unnamed bool
}

// told reports whether the part is told.
func (n ToolsNamed) told() bool { return len(n.Tools) > 0 || n.Unnamed }

// madeTo is the part's tools as its bullet ends with them: ", made to " and
// the tools, each in backticks, then "and to tools <namer> does not name"
// where a count names no tool, or that alone. namer is "the record", or "it"
// where the bullet has named the record already.
func (n ToolsNamed) madeTo(namer string) string {
	quoted := make([]words.Said, len(n.Tools))
	for i, tool := range n.Tools {
		quoted[i] = words.Said("`" + tool + "`")
	}
	switch {
	case !n.Unnamed:
		return ", made to " + words.And(quoted)
	case len(quoted) == 0:
		return ", made to tools " + namer + " does not name"
	}
	return ", made to " + words.And(quoted) + " and to tools " + namer + " does not name"
}

// otherCalls is the section [OtherCalls] is told in, "" where no part is:
// the header, the opening, a bullet per part in the order the session would
// hold them (the earlier turns, the earlier rounds, the calls the record no
// longer lists, the workers'), and the closing.
func otherCalls(o OtherCalls) string {
	said := nextTurnWordsFor(NextTurn{Fire: o.Fire})
	var parts []string
	if o.EarlierTurns {
		parts = append(parts, "- calls of this "+said.whole+"'s earlier "+said.turn+"s, made before this "+
			said.turn+" began")
	}
	if o.EarlierRounds.told() {
		parts = append(parts, "- calls of this "+said.turn+"'s earlier rounds, in full or only in a summary's words"+
			o.EarlierRounds.madeTo("the record"))
	}
	if o.Unlisted.told() {
		parts = append(parts, "- calls of this "+said.turn+" the record no longer lists one by one"+
			o.Unlisted.madeTo("it"))
	}
	if o.Workers.told() {
		parts = append(parts, "- calls of workers this "+said.turn+" delegated to, in their reports' words"+
			o.Workers.madeTo("the record"))
	}
	if len(parts) == 0 {
		return ""
	}
	return otherCallsHeader + "\n" + otherCallsOpening + "\n" + strings.Join(parts, "\n") + "\n" + otherCallsClosing
}

// lateCallsHeader introduces the calls of the round that finished only after
// the run that made them stopped waiting for them, and the ones still in flight
// as the round was read ([AfterSave.Late]), and ones whose outcome is unknown
// (internal/agent/runner's UnfinishedNote). TOLD UNDER WORDS OF THEIR OWN,
// WHATEVER THE SESSION'S END: the session may show such a call with no outcome,
// as running, interrupted or failed, so it is said to be ONE call whose outcome
// as told here stands. NEVER THAT THE CALLS FINISHED, NOR THAT EVERY ERROR
// FAILED: a call in flight is failed on the record, its line saying its outcome
// is unknown. Words saying `→ error` did not take effect contradicted that
// line, and a run trusting them made the call again (a second post or
// delegation). So the words say it did not take effect unless its line says its
// outcome is unknown: that one may have, so check before making it again.
const lateCallsHeader = "## Calls the run that made them stopped waiting for, or whose outcome is unknown" +
	"\nA run of this work made these calls through your tools, and each is one that run stopped waiting for" +
	" before it finished (it gave up on it, or it was cut off or replaced) or one whose outcome is unknown." +
	" So the session above may show one with no outcome, as still running, as interrupted or as failed, and an" +
	" earlier message may have told it. Each is ONE call, made once, not two, and the outcome told here stands:" +
	" a call marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked `→ error` did" +
	" not take effect, unless it says its outcome is unknown: that one may have, so check whether it did before" +
	" making it again."

// lateSubmission tells a run that a successful `submit_work` among the calls
// under [lateCallsHeader] or [lateBeforeHeader] is no submission of the work
// it carries on: the run that made it had stopped waiting for it before it
// returned — replaced, the run carrying the work on handed it only once under
// way, or told it failed where it gave up on it — and the work's submission is
// the one a run carrying it on makes (internal/agent/runner's sinceAnswer,
// which never takes a late one, and replays the newest). Read as the work's, a
// late one would leave a run that took it for done ending without its own, and
// the work collected as one that submitted nothing.
const lateSubmission = "A `" + toolname.SubmitWork + "` among them does NOT stand as this work's submission:" +
	" the run that made it had stopped waiting for it before it returned. When your work is done, call `" +
	toolname.SubmitWork + "` yourself."

// LateCallHead is the heading [LateCallArrived] opens with, a line of its own:
// the session reader tells such an account from a person's note and from a
// brief by it (internal/sandbox/executor's lateCallHead, held equal by a test).
const LateCallHead = "## A call the run that made it stopped waiting for"

// LateCallArrived is one call of the work its run stopped waiting for, which
// finished since, as the engine hands it to the run working on the work
// (internal/engine's lateItemMessage): call its line's `name(args)` and outcome
// `success` or `error: …`, each one line, the outcome first. submission says it
// is a successful `submit_work`.
//
// EVERY FACT IS [lateCallsHeader]'s OR [lateSubmission]'s, as STATEMENTS, for
// the reason [SteerMessage] states its own: a command in context a run did not
// ask for reads as steering. The outcome-unknown clause is left out, since an
// account is of a finished call; added are that it reached the run while the
// work was under way and is no new request, and what a run told it as it
// finishes answers next.
func LateCallArrived(outcome, call string, submission bool) string {
	text := LateCallHead +
		"\nA run of this work made this call through your tools and stopped waiting for it before it" +
		" finished (it gave up on it, or it was cut off or replaced), and it has finished since. This reached" +
		" you while the work was under way, and it is not a new request. The session above may show the call" +
		" with no outcome, as still running, as interrupted or as failed, and an earlier message may have told" +
		" it. It is one call, made once, and the outcome told here stands: a call that succeeded has already" +
		" run, so making it again would make it twice; a call that failed did not take effect. If this" +
		" reached you as you were finishing, the answer you give next is this run's report, in place of the" +
		" one before it." +
		"\n\nOutcome: " + outcome + "\nCall: " + call
	if submission {
		text += "\n\nIt is not this work's submission: the run that made it had stopped waiting for it before it" +
			" returned, and this work's submission is the one this run makes when its work is done."
	}
	return carried(text)
}

// earlierSubmission tells a run that a successful `submit_work` among the calls
// listed before it, under [calledAfterHeader], [calledSinceHeader] or
// [calledBeforeHeader], is no submission of the work it carries on. Every call
// a brief tells was made before the relaunch whose brief tells it, and none is
// the work's submission (internal/agent/runner's sinceAnswer and carriedRound):
// the collection takes a submission only from calls after the round's last
// answer; a lost or refused round whose calls since that answer hold one is
// collected on it, never carried on; and a send-back's continuation can tell the
// submission its review sent back. Told as `→ success` alone, it read as the
// work's: a run taking it for done could end without its own, and the work be
// rescued as incomplete.
const earlierSubmission = "A `" + toolname.SubmitWork + "` among them does NOT stand as this work's" +
	" submission: the work was carried on after it. When your work is done, call `" + toolname.SubmitWork +
	"` yourself."

// heldSubmission tells a run that a successful `submit_work` of its round that
// the session above shows, and that no list of the brief tells, is no
// submission of the work it carries on ([AfterSave.HeldSubmission]). It stands
// no more than a listed one ([earlierSubmission]), and for the same reason: it
// was made before the relaunch whose brief continues that session (internal/
// agent/runner's heldReading). The commonest is a run that submitted and then
// asked, which its gate allows: the cut its answer's opening takes falls past
// the call (runner's sinceAnswer). SAID FIRST, WHETHER OR NOT A CALL IS LISTED,
// in words true of the session, not of a list: that copy is told no call, so a
// run reading its transcript's submission as done ended without its own. Under
// a list it would name a call that list does not hold.
const heldSubmission = "A `" + toolname.SubmitWork + "` the session above shows does NOT stand as this" +
	" work's submission: the work was carried on after it. When your work is done, call `" +
	toolname.SubmitWork + "` yourself."

// AfterSave is what a session's run did through the seat's tools after the
// session was saved, as a run that continues the session is told it
// ([CalledAfter]).
type AfterSave struct {
	// HeldSubmission says the session holds a successful `submit_work` of its
	// round that no list tells: one its save point holds, one a reading of its
	// archive, cut off, found it holding (Read), or any but a late one or one
	// nobody waited for in the copy its job's own box holds where that job's
	// harness ended its own run. It is said first, whether or not a call is
	// listed, as no submission of the work ([heldSubmission]). Set only by the
	// round's own calls, so never by one the session holds from an earlier
	// turn, as the session its conversation's last turn left holds that turn's;
	// and never for one among Calls, which EarlierSubmission says, among Late,
	// which LateSubmission says, nor among a subagent's calls, which
	// [SubagentCalls.Submission] says.
	HeldSubmission bool

	// Owed are the reads of the round an earlier message to the session
	// counted without listing them, its read cap spent, which the session holds
	// only as that count (internal/agent/runner's heldAt, Resume.Owed and
	// SessionBasis.Owed): the reads its job's brief left out, as the prior-work
	// ledger renders them within its budgets, under one read cap with Calls',
	// Late's, Subagents' and Shown's, spent on these after Late, Subagents' and
	// Calls and before Shown, whose calls the session holds
	// (ledger.FormatCallGroups), "" for none.
	// Told first, in a section of their own ([owedHeader]): they are the
	// oldest calls the brief tells. Reads only, since only a read is ever
	// counted rather than listed, and settled before that message was built,
	// so none is a submission or a call whose outcome is unknown.
	Owed string

	// OwedDropped is how many of those reads the round's record has since
	// dropped from the middle of a long run's record, which no list can tell
	// one by one: counted in Owed's section ([owedDropped]), with Tally where
	// no later count of the brief needs it ([tallyBesideLast]).
	OwedDropped int

	// Calls are the calls its round's record still lists that came after the
	// save, as the prior-work ledger renders them within its budgets, under
	// one read cap with Late's, Subagents', Owed's and Shown's, spent on Late
	// first, then on Subagents', then on these, then on Owed and Shown
	// (ledger.FormatCallGroups), "" for none: but, where Read, those a reading
	// of the session's archive found it holding, and those it found in an
	// unreported subagent's thread, which Subagents lists. For a run that
	// starts a session of its own ([CalledBefore]), every call of the round
	// but Late's.
	Calls string

	// EarlierSubmission says a successful `submit_work` is among Calls, which
	// is told after them as no submission of the work ([earlierSubmission]):
	// every call a brief tells was made before the relaunch whose brief tells
	// it, so none is the submission of the work that relaunch carries on.
	// Never set for one among Late, which LateSubmission says in Late's own
	// words, nor for one among a subagent's calls, which
	// [SubagentCalls.Submission] says under that subagent's heading.
	EarlierSubmission bool

	// Dropped is how many more calls the run made after the save that its
	// record did not keep one by one, dropped from the middle of a long run's
	// record. EXACT for a record only this version of the engine numbered: it
	// numbers every call it takes, so a count of the numbers past the save it
	// does not list is no estimate. A call an older version appended in a
	// fleet mid-upgrade carries no number, so where such a call is listed it
	// may be counted here as well as listed — an over-count, held to the
	// calls the record dropped at all, never an under-count. So may a late
	// call the record dropped past the save, counted here by its number and
	// again under LateDropped.
	Dropped int

	// Tally is the record's line for every call it dropped
	// (ledger.FormatElided), those made before the save among them: what
	// Dropped's calls were, by tool, beside calls the session holds. Said
	// only beside a count it accounts for (OwedDropped, Dropped or
	// LateDropped), and once: beside the LAST of them the brief gives, since
	// every count says the tally is below it ([tallyBesideLast]).
	Tally string

	// Interrupted says the session was, or may have been, cut off
	// ([InterruptedSession], [InterruptedCopy], [MayBeInterruptedCopy]), so
	// the point its calls are told from is the newest one its launch is known
	// to hold, not where it ends (internal/sandbox's savepoint.go) — or, for
	// the copy a job's own box holds where its harness may have been cut off,
	// the round's start (internal/agent/runner's calledInPlace): some of the
	// calls may be in it as well. They are told under [calledSinceHeader], or
	// [readHeader] where Read, never as calls it lacks, and the dropped ones
	// counted since that point.
	Interrupted bool

	// Read says a reading of the archive of the session, cut off, told Calls
	// (internal/agent/runner's heldReading): the calls it found the session
	// holding are in no list, so Calls are headed by [readHeader], whose
	// words are true of a list a reading thinned, in place of
	// [calledSinceHeader]'s, which say the list holds every call since the
	// point. Set only beside Interrupted.
	Read bool

	// Subagents are the calls a reading found in the threads of subagents
	// whose report the session does not hold, one group per subagent in the
	// order of its first told call, each under its subagent's name
	// ([subagentsSection]), rendered under the one read cap after Late's and
	// before Calls'. Told after Calls and before Shown, under
	// [subagentsHeader]. Nil where no reading told the brief.
	Subagents []SubagentCalls

	// Shown are calls the session may show that a call told in Calls, Late or
	// Subagents could be taken for (internal/agent/runner's readAlike: the same
	// tool, and arguments neither line tells apart, or not comparable): first
	// earlier rounds' calls the brief tells nowhere else, each line ending with
	// [EarlierRoundNote], then the round's own calls the session holds, rendered
	// as the prior-work ledger does under the one read cap, spent on these after
	// every other (ledger.FormatCallGroups). "" for none, and where the cap left
	// no call, never a count alone. Told after Calls and before Late in a section
	// of their own ([shownAgainHeader]): the words over Calls and Late say a call
	// shown in both places is ONE call, so a told call whose like the session
	// shows could be read as that one and lost. Never set for a run that starts a
	// session of its own ([CalledBefore]), which holds no call of the round.
	Shown string

	// Other is what the session may show that no list of the brief names
	// ([OtherCalls]): told first, after the held submission and before the
	// owed reads, so "below" is every list, in a section of its own
	// ([otherCallsHeader]). The zero value tells nothing. Never set for a run
	// that starts a session of its own ([CalledBefore]), which has none above.
	Other OtherCalls

	// Late are the calls of the round that finished only after the run that
	// made them stopped waiting for them (sandbox.BridgeCall.Late), or that the
	// run gave up on or whose session ended before they returned
	// (sandbox.BridgeCall.Abandoned), rendered as the prior-work ledger does
	// under the one read cap, spent on these first, "" for none; none is among
	// Calls. Told in a section of their own, under [lateCallsHeader], whatever
	// Interrupted says ([lateBeforeHeader] for a run that starts a session).
	//
	// AND THE CALLS STILL IN FLIGHT AS THE ROUND WAS READ, rendered and capped
	// with them: each one whose outcome is unknown (ledger.Call.Unfinished),
	// failed, its line saying it may have taken effect. None finished on the
	// record, so the section's words never say its calls finished.
	Late string

	// LateSubmission says a successful `submit_work` is among Late, which is
	// told as no submission of the work ([lateSubmission]).
	LateSubmission bool

	// LateDropped is how many such calls the record dropped from the middle of
	// a long run's record (sandbox.BridgeLog.LateElided), counted in Late's
	// section ([droppedLate]) whatever the session's save point, beside Tally,
	// the last count a brief gives ([tallyBesideLast]): a dropped call keeps
	// no mark and no number, so no point can be read against it. Said under
	// the words of calls after a save, they would be calls the session lacks,
	// made by its own run after it was saved, which a late call is not, and
	// one its run gave up on may be shown in it. One past the save point is
	// counted among Dropped as well, by its number: an over-count, never an
	// under-count.
	LateDropped int
}

// CalledAfter is the section telling a run that continues a session what its
// run did through the seat's tools after the session was saved ([AfterSave]),
// "" where there is none and the session holds no submission of its round.
//
// THE TALLY ONCE, beside the LAST count that needs it in section order (reads
// dropped, calls dropped after the save, late calls dropped), since every count
// says the tally is below it ([tallyBesideLast]).
//
// A LINE MAY END WITH [ToldApartNote] (internal/agent/runner's notes): the
// caller's words, already in the lines it hands, as with [EarlierRoundNote].
// Nothing here reads them, and no header needs them.
func CalledAfter(after AfterSave) string {
	var sections []string
	// THE ORDER: the held submission first, where none lists a call; the other
	// calls before every list, since their words speak of the lists below and
	// list no call; the owed reads, in words true of reads the session was told
	// were made, never as calls after its save; the calls since the save; the
	// subagents' and listed-again sections after the list most of their likes
	// are in; the late section, whose words also say a call shown twice is
	// one, last on every path. A successful submission in a list is said to be
	// none of the work's right after that list, so a listed-again one says nothing.
	if after.HeldSubmission {
		sections = append(sections, heldSubmission)
	}
	if other := otherCalls(after.Other); other != "" {
		sections = append(sections, other)
	}
	tally := tallyBesideLast(after.Tally, after.OwedDropped, after.Dropped, after.LateDropped)
	if owed := toldList(owedHeader, after.Owed, "", owedDropped(after.OwedDropped), tally[0]); owed != "" {
		sections = append(sections, owed)
	}
	if since := calledSince(after, tally[1]); since != "" {
		sections = append(sections, since)
	}
	if subs := subagentsSection(after.Subagents); subs != "" {
		sections = append(sections, subs)
	}
	if shown := toldList(shownAgainHeader, after.Shown, "", "", ""); shown != "" {
		sections = append(sections, shown)
	}
	if late := lateSection(lateCallsHeader, after, tally[2]); late != "" {
		sections = append(sections, late)
	}
	return strings.Join(sections, "\n\n")
}

// tallyBesideLast is where a brief says tally, the record's line for every
// call it dropped from its middle, among counts, the counts of dropped calls it
// gives in the order it gives them: beside the last of them above zero, and ""
// beside every other, so the tally is said once and only where a count needs
// it.
//
// THE LAST, NEVER THE FIRST: every count says the tally is below it
// ([owedDropped], [droppedAfter], [droppedLate], [droppedBefore]), which only
// the last place makes true of each. Said beside an earlier count, a later one
// would send the run looking below it for a line that is above it, and the run
// would find nothing there to tell it what a dropped call was.
func tallyBesideLast(tally string, counts ...int) []string {
	at := make([]string, len(counts))
	for i := len(counts) - 1; i >= 0; i-- {
		if counts[i] > 0 {
			at[i] = tally
			break
		}
	}
	return at
}

// calledSince is [CalledAfter]'s section of the calls after the save — or,
// of a session cut off, since the newest point it is known to hold, under
// [readHeader] where a reading thinned it — "" where the record lists none and
// dropped none; tally is the record's tally where
// this section's count is the last the brief gives, "" where a later one says
// it ([tallyBesideLast]).
//
// A SUCCESSFUL SUBMISSION AMONG THE CALLS IS SAID NOT TO STAND right after
// them ([AfterSave.EarlierSubmission]), before the count of calls dropped
// after the save and its tally, which list none: the sentence speaks of the
// calls listed above it, as [lateSubmission] follows the late calls.
func calledSince(after AfterSave, tally string) string {
	header := calledAfterHeader
	switch {
	case after.Read:
		header = readHeader
	case after.Interrupted:
		header = calledSinceHeader
	}
	dropped := ""
	if after.Dropped > 0 {
		dropped = droppedAfter(after.Dropped, after.Interrupted)
	}
	return toldList(header, after.Calls, saidIf(after.EarlierSubmission, earlierSubmission), dropped, tally)
}

// lateSection is the section of the late calls and the ones whose outcome is
// unknown ([AfterSave.Late]) under header, a submission among them said not to
// stand right after them ([AfterSave.LateSubmission]), and how many more the
// record dropped ([AfterSave.LateDropped]) with tally beside that count, the
// last a brief gives ([tallyBesideLast]): "" where none is listed and none
// dropped.
func lateSection(header string, after AfterSave, tally string) string {
	dropped := ""
	if after.LateDropped > 0 {
		dropped = droppedLate(after.LateDropped)
	}
	return toldList(header, after.Late, saidIf(after.LateSubmission, lateSubmission), dropped, tally)
}

// toldList is one told list's section: header, then the calls listed under it
// and the line said of them (of, "" for none), then the count of the ones the
// record dropped (dropped, "" for none) and the record's tally beside it (tally,
// "" where the brief says it elsewhere). "" where nothing is listed and nothing
// dropped: a header over no call names nothing.
func toldList(header, calls, of, dropped, tally string) string {
	calls = strings.TrimSpace(calls)
	if calls == "" && dropped == "" {
		return ""
	}
	parts := []string{header}
	if calls != "" {
		parts = append(parts, calls)
		if of != "" {
			parts = append(parts, of)
		}
	}
	if dropped != "" {
		parts = append(parts, dropped)
		if tally = strings.TrimSpace(tally); tally != "" {
			parts = append(parts, tally)
		}
	}
	return strings.Join(parts, "\n")
}

// saidIf is line where set says to say it, "" where it does not.
func saidIf(set bool, line string) string {
	if !set {
		return ""
	}
	return line
}

// owedHeader introduces the reads of the round an earlier message to the
// session counted without listing them, its read cap spent ([AfterSave.Owed]).
//
// AS READS THE SESSION WAS TOLD WERE MADE, never as calls after its save: the
// message that counted them is in the session, so it holds that they were
// made, but not what any of them returned, which is what this list adds. A
// read fetched data only, so it may be run again for its data, and a read
// whose outcome is unknown is never among them: one still in flight as its
// round was read is told among the late calls, under their own words.
const owedHeader = "## Reads an earlier message counted but did not list" +
	"\nAn earlier message to the session above counted these reads of this work without listing them, to keep" +
	" its list short. Each was made, and its outcome stands: one marked `→ success` fetched data only, so re-run" +
	" it if you need its data now, never invent it; one marked `→ error` did not take effect."

// owedDropped says that n of the reads an earlier message counted without
// listing them ([AfterSave.OwedDropped]) are no longer kept one by one, and
// what that asks of the run: a read fetched data only, so it runs one again
// for data it needs, rather than check whether it took effect. "" for none.
func owedDropped(n int) string {
	if n <= 0 {
		return ""
	}
	reads := "reads"
	if n == 1 {
		reads = "read"
	}
	return "(+" + strconv.Itoa(n) + " further such " + reads + ", which the record no longer keeps one by one:" +
		" past two hundred calls a run's record keeps its first and last and only a tally of those between," +
		" and the tally below counts other calls as well. Re-run a read if you need its data now.)"
}

// KeyCarriedOn and KeyRoundSoFar key the sections a fresh brief carrying a
// round on is appended under (internal/agent/runner's continueAgentRun): what
// became of the run before it (the reseed), and the round's calls so far
// ([CalledBefore]). Appended with [Prompt.Then], so the prompt the phase
// publishes as its opening frame keeps its outline where the box starts a
// session of its own.
const (
	KeyCarriedOn  = "carried_on"
	KeyRoundSoFar = "round_so_far"
)

// calledBeforeHeader introduces the calls of the round a run that starts a
// session of its own carries on: the calls the runs before it made, which its
// session holds none of.
//
// NEVER "THE SESSION ABOVE": a run that starts a session of its own has none
// above, and a session it started remembers none of these calls, so they are
// told as calls the runs before it made, in the order they made them.
const calledBeforeHeader = "## Calls the runs before you made on this task" +
	"\nThe runs before you on this task made these calls through your tools, in the order they made them." +
	" They happened, and their outcomes stand: a call marked `→ success` ALREADY RAN, so do NOT issue a" +
	" write among them again; one marked `→ error` did not take effect."

// lateBeforeHeader is [lateCallsHeader] for a run that starts a session of its
// own: no session above shows anything of the calls, so the words say nothing
// of one, and keep the rest. Each is a call its run stopped waiting for before
// it finished, or one whose outcome is unknown, ONE call made once, and a call
// marked `→ error` did not take effect unless it says its outcome is unknown,
// which may have, to be checked before it is made again (internal/agent/runner's
// UnfinishedNote).
const lateBeforeHeader = "## Calls the runs before you stopped waiting for, or whose outcome is unknown" +
	"\nRuns before you on this task made these calls through your tools, and each is one the run that made" +
	" it stopped waiting for before it finished (it gave up on it, or it was cut off or replaced) or one whose" +
	" outcome is unknown. Each is ONE call, made once, and the outcome told here stands: a call marked" +
	" `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked `→ error` did not take" +
	" effect, unless it says its outcome is unknown: that one may have, so check whether it did before" +
	" making it again."

// CalledBefore is the section telling a run that starts a session of its own,
// carrying a round on (internal/agent/runner's continueAgentRun), what the runs
// before it did through the seat's tools: every call of the round, listed
// then tallied once beside the last count ([tallyBesideLast]), with a
// successful `submit_work` said not to stand ([AfterSave.EarlierSubmission]),
// and the late calls in a section of their own ([AfterSave.Late]). "" where
// the round made no call. IN WORDS OF ITS OWN, never [CalledAfter]'s: a run of
// its own has no session above, so told calls made after a save it would look
// for them in a transcript it does not have; told nothing, it could make them
// again (a second post or delegation). A LINE MAY END WITH [ToldApartNote]
// (runner's notes), which the caller supplies: [lateBeforeHeader] says each
// late call is ONE call, which a model could read as one with an alike line.
func CalledBefore(told AfterSave) string {
	var sections []string
	dropped := ""
	if told.Dropped > 0 {
		dropped = droppedBefore(told.Dropped)
	}
	tally := tallyBesideLast(told.Tally, told.Dropped, told.LateDropped)
	if before := toldList(calledBeforeHeader, told.Calls, saidIf(told.EarlierSubmission, earlierSubmission),
		dropped, tally[0]); before != "" {
		sections = append(sections, before)
	}
	if late := lateSection(lateBeforeHeader, told, tally[1]); late != "" {
		sections = append(sections, late)
	}
	return strings.Join(sections, "\n\n")
}

// droppedBefore says that n of the calls the runs before a run of its own made
// are counted only in their record's tally, which names them, and what a call
// counted only there asks of the run: it has no outcome to go by.
func droppedBefore(n int) string {
	calls := "calls"
	if n == 1 {
		calls = "call"
	}
	return "(+" + strconv.Itoa(n) + " further " + calls + " the runs before you made, which their record did" +
		" not keep one by one: past two hundred calls a run's record keeps its first and last and only a tally" +
		" of those between, and the tally below names them. A call counted only in the tally has no outcome" +
		" here: check whether its tool already did what it was for before doing it again.)"
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

// notesAgainHeader opens the notes a session is told again that it may not show
// ([NotesReadAgain]).
const notesAgainHeader = "## Notes this turn read that the session above may not show"

// NotesReadAgain is the section that tells a session again notes the turn has
// read that it may not show, each already rendered as the model reads a note
// ([SteerMessage]), "" for none: the notes the turn read that the session may
// not show (every note to a session cut off; every note from a read whose
// answer never reached its harness; a note the bridge served its harness
// unconfirmed), told as notes it MAY ALREADY SHOW, ONE NOTE WHEREVER SHOWN, AND
// COUNTED: each says it stands for the rest of the turn, so a model reading one
// twice could do what it asks twice. NEVER "EVERY NOTE": earlier ones are the
// session's. The notes after them are ones the turn had NOT READ, never new to
// the session: a harness can be handed a note before the record says it was
// read (OpenCode's is recorded only once its plugin confirms it); a note the
// bridge recorded serving is told among these (runner's toldNotes).
func NotesReadAgain(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	which := "the note below"
	if len(notes) > 1 {
		which = "the " + strconv.Itoa(len(notes)) + " notes below"
	}
	return notesAgainHeader + "\nThe session above may not show " + which + ": notes a person sent" +
		" this turn, in the order the turn read them. A note shown both above and" +
		" here is ONE note, sent once, not two: do what it asks once. A note after them is one the turn" +
		" had not read before this message." +
		"\n\n" + strings.Join(notes, "\n\n")
}

// BoxReplaced is what a run that continues its session in a box other than the
// one the session was last worked in is told beside its continuation: a fresh
// checkout, which branch to check out, the uncommitted work gone, and that the
// session's absolute paths were another machine's. branch (the session's pushed
// branch) and home (the earlier box's home) are "" where unknown, defused.
// checkout is the tool that checks a repository of Gitlet out (`code_checkout`,
// ADR-0093), "" where the surface offers none. WHERE IT OFFERS ONE, "a FRESH
// checkout" is true only of a repository the box's setup clones: nothing is
// cloned at launch, so a new box holds none the session checked out of Gitlet.
// The run is told to check each out again, the same branch and access and no
// base, so it is cloned at the branch's head with everything pushed there,
// rather than told a checkout is in front of it that is not.
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
// conversation's last turn left; `read_session` reads it under turn's id (the
// engine's record, defused by the caller). THIS TURN'S IS KEPT ONLY UNTIL THIS
// RUN FIRST SAVES ITS OWN, so the run is told to read it before then; no time
// is given, which pacing or a failed save would make false. A SCHEDULE'S FIRE
// IS TOLD IN ITS WORDS (fire), as [BuildNextTurn] does. ONE SENTENCE, ONLY
// HERE: a standing sentence would ride every request of every run. NEVER WHY IT
// IS NOT CONTINUED: the log's words quote what said no (a store's error, a
// box's output) and told in the engine's voice would put them outside any
// fence; what the run can do is the same whatever the reason.
func KeptSession(turn string, thisTurn, fire bool) string {
	said := nextTurnWordsFor(NextTurn{Fire: fire})
	whose, read := "This "+said.whole+"'s last "+said.turn, ". Read what it did"
	if thisTurn {
		whose = "This " + said.turn + "'s earlier launches"
		read = ", kept until this run first saves its own. Until then, read what they did"
	}
	return keptSessionHeader + "\n" + whose + " worked in a session this run does not continue" + read +
		" with `" + toolname.ReadSession + "`, turn_id `" + turn + "`; `match` finds a word in it."
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
// that its box is a new one (internal/sandbox, restore.go; [BoxReplaced]) and,
// where its harness was cut off, that it may end mid-call. A schedule's fire is
// told in its words ([NextTurn.Fire]). NOT THE SAME-TURN LEAD
// ([BuildContinuedRound]): the session above is another turn's. NEVER "THROUGH" A TURN THAT WENT ON PAST ITS
// SESSION ([NextTurn.WentOn]): the session ends where it was kept, so the lead
// says what the turn did after it is not above, or the run would take the
// turn's last state for the session's and redo or contradict it. A turn lost
// ([NextTurn.Lost]) without that is told through, as far as the engine knows the
// session runs, and that it may not hold the last of what it did.
func BuildNextTurn(m NextTurn) string {
	said := nextTurnWordsFor(m)
	var w strings.Builder
	w.WriteString("## A new " + said.turn + " of this " + said.whole + " begins here" +
		"\nEverything above is this " + said.whole + " as your " + said.turn + "s left it")
	span, ended := nextTurnSpan(m, said)
	w.WriteString(span)
	w.WriteString(". " + ended + " " +
		said.woke + ", and the task below is this " + said.turn + "'s." +
		"\n- **What you read above may be stale:** read it again before relying on it." +
		"\n- **Tools you activated and skills you loaded above were set up anew for this " + said.turn +
		":** activate or load them again before relying on them.")
	w.WriteString("\n\n## This " + said.turn + " is in a new box\nYou are in a new box")
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

// nextTurnSpan is how far the session a conversation's last turn left runs, as
// the clause that follows "as your turns left it", and where how that turn
// ended is told: [BuildNextTurn]'s span, and both builders' ending.
// [NextTurnAgain] names the turn in [turnAgainSpan]'s words instead, which say
// nothing of the session.
func nextTurnSpan(m NextTurn, said nextTurnWords) (span, ended string) {
	var w strings.Builder
	ended = "How the " + said.whole + "'s earlier " + said.turn + "s ended is told below."
	switch {
	case m.WentOn:
		w.WriteString(", up to a session kept while " + said.named(m.BaseTurn) + " still worked")
		if !m.CapturedAt.IsZero() {
			w.WriteString(", at " + m.CapturedAt.UTC().Format(time.RFC3339))
		}
		w.WriteString(": what that " + said.turn + " did after it is not above")
		if m.Lost {
			w.WriteString(", and that " + said.turn + " was lost")
		}
		ended = "How " + said.named(m.BaseTurn) + " ended, among the " + said.whole + "'s earlier " +
			said.turn + "s, is told below."
	case m.Lost:
		w.WriteString(", through " + said.named(m.BaseTurn) + ", which was lost")
		if !m.EndedAt.IsZero() {
			w.WriteString(" and ended at " + m.EndedAt.UTC().Format(time.RFC3339))
		}
		w.WriteString(": the session above may not hold the last of what that " + said.turn + " did")
		ended = "How " + said.named(m.BaseTurn) + " ended, among the " + said.whole + "'s earlier " +
			said.turn + "s, is told below."
	default:
		if m.BaseTurn != "" {
			w.WriteString(", through " + said.named(m.BaseTurn))
		}
		if !m.EndedAt.IsZero() {
			w.WriteString(", which ended at " + m.EndedAt.UTC().Format(time.RFC3339))
		}
	}
	return w.String(), ended
}

// NextTurnAgain is the lead of a run that continued the session its
// conversation's last turn left, as a later brief to a session of the work that
// may lack it tells it again (runner's leadsAgain): the turn boundary
// [BuildNextTurn] said, in the past tense, what it makes stale, and, of a session that began as the one the earlier turns left, that
// its files are the earlier box's, that it ends where it was kept
// ([NextTurn.WentOn]) or may not hold the last of a lost turn ([NextTurn.Lost]),
// and any cut-off call. NEVER THAT THE SESSION ABOVE BEGAN THERE: the lead
// reaches sessions other than the one its brief went to (leadsAgain hands on
// every lead the run keeps), so a session begun with a fresh brief would read
// its own opening as another turn's. NEVER THE NEW BOX: stale or false of the
// box a later job continues in; the brief says its own box. NEVER "EVERYTHING
// ABOVE": the session may hold this turn's own work after the boundary.
func NextTurnAgain(m NextTurn) string {
	said := nextTurnWordsFor(m)
	_, ended := nextTurnSpan(m, said)
	var w strings.Builder
	w.WriteString("### A new " + said.turn + " of this " + said.whole + " began here" +
		"\nThis " + said.turn + " began after your earlier " + said.turn + "s of this " + said.whole +
		turnAgainSpan(m, said) + ", and the task below is this " + said.turn + "'s. " + ended +
		"\n- **What was read before this " + said.turn + " began may be stale:** read it again before " +
		"relying on it." +
		"\n- **Tools activated and skills loaded before this " + said.turn + " began were set up anew for " +
		"it:** activate or load them again before relying on them." +
		"\n\nWhere the session above began as the one your earlier " + said.turn + "s left, in full or in a " +
		"summary:")
	switch {
	case m.WentOn:
		w.WriteString("\n- **It was kept while " + said.named(m.BaseTurn) + " still worked")
		if !m.CapturedAt.IsZero() {
			w.WriteString(", at " + m.CapturedAt.UTC().Format(time.RFC3339))
		}
		w.WriteString(":** what that " + said.turn + " did after it is not in it.")
	case m.Lost:
		w.WriteString("\n- **It may not hold the last of what " + said.named(m.BaseTurn) + " did,** which was " +
			"lost.")
	}
	w.WriteString("\n- **What it says about files on disk from before this " + said.turn + " began** is what " +
		"the box of the " + said.turn + " it came from held.")
	if m.Interrupted {
		w.WriteString("\n- " + baseCutOffAgain)
	}
	return w.String()
}

// FreshTaskAgain is the lead of a fresh brief, which opened a session of the
// run's own with the turn's task, as a later brief to a session of the work
// that may be a summary tells it again (internal/agent/runner's leadsAgain):
// the task it opened with follows it, rendered again by its teller. NO TURN
// BOUNDARY AND NO BOX: that brief said neither, and the session it reaches may
// have begun as another, so it says only that the work was started on it.
const FreshTaskAgain = "### The task this work was started on" +
	"\nA run of this work that began a session of its own was first told the task below, and the session " +
	"above may show it only in a summary."

// turnAgainSpan is the turn a conversation's last turn left this one after, as
// [NextTurnAgain] names it, the clause that follows "your earlier turns of this
// conversation": that turn, and of how it ended only what is the turn's own,
// when it ended and that it was lost. Nothing of the session it left, which
// [NextTurnAgain] says only of a session that began as that one.
func turnAgainSpan(m NextTurn, said nextTurnWords) string {
	if m.BaseTurn == "" && !m.WentOn && !m.Lost {
		if m.EndedAt.IsZero() {
			return ""
		}
		return ", which ended at " + m.EndedAt.UTC().Format(time.RFC3339)
	}
	span := ", through " + said.named(m.BaseTurn)
	switch {
	case m.Lost && !m.WentOn && !m.EndedAt.IsZero():
		return span + ", which was lost and ended at " + m.EndedAt.UTC().Format(time.RFC3339)
	case m.Lost:
		return span + ", which was lost"
	case !m.WentOn && !m.EndedAt.IsZero():
		return span + ", which ended at " + m.EndedAt.UTC().Format(time.RFC3339)
	}
	return span
}

// baseCutOffAgain is [NextTurnAgain]'s word that the session a conversation's
// last turn left was cut off when it was kept ([InterruptedSession], as
// [BuildNextTurn] tells it), said of a session that began as that one: of
// where it began, never of where it ends, which the brief that tells it again
// says in its own cut-off section.
const baseCutOffAgain = "**It was cut off when it was kept,** so before this turn began it may hold a call with no " +
	"result, which may or may not have run. Check what such a call was meant to do before doing it again."

// leadsAgainHeader opens the messages a session is told again that carried its
// work on and that it may not show ([LeadsToldAgain]).
const leadsAgainHeader = "## Messages that carried this work on that the session above may not show"

// LeadsToldAgain is the section that tells a session again the messages that
// carried its work on that it may not show, each rendered under its own heading
// (runner's leadsAgain), "" for none: those the job's brief opened with
// where the session is not known to hold it, and every one the run keeps where
// its harness may summarize, in order sent (an unreadable one or a review no
// longer kept said in its place, [LeadUnread], [ReviewNotKept]); and how many
// earlier ones are not kept to tell (cut), said as not kept, never as no longer
// kept: the store may keep them still. TOLD AS MESSAGES IT MAY ALREADY SHOW,
// ONE MESSAGE WHEREVER SHOWN, as [NotesReadAgain] does: an answer read twice
// could be acted on twice. NEVER WHY IT MAY NOT SHOW THEM: the section before
// says that ([InterruptedSession], [InterruptedCopy], [MayBeInterruptedCopy],
// [MayCompactSession]), and here it would say what nothing bears out.
func LeadsToldAgain(leads []string, cut int) string {
	if len(leads) == 0 && cut <= 0 {
		return ""
	}
	var w strings.Builder
	w.WriteString(leadsAgainHeader + "\n")
	switch len(leads) {
	case 0:
		w.WriteString("The session above may not show every message that carried this work on.")
	case 1:
		w.WriteString("The session above may not show the message below, which carried this work on. A message " +
			"shown both above and here is ONE message, sent once, not two: do what it asks once.")
	default:
		w.WriteString("The session above may not show the " + strconv.Itoa(len(leads)) + " messages below, each of " +
			"which carried this work on. They are in the order they were sent. A message shown both above and " +
			"here is ONE message, sent once, not two: do what it asks once, and where two of them differ, the " +
			"later one stands.")
	}
	if cut > 0 {
		w.WriteString("\n\n(+" + strconv.Itoa(cut) + " earlier such " + plural(cut, "message", "messages") +
			", which " + plural(cut, "is", "are") + " not kept to tell again.)")
	}
	for _, lead := range leads {
		w.WriteString("\n\n" + lead)
	}
	return w.String()
}

// plural is one where n is 1, many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// LeadUnread is a message that carried the work on that the brief telling it
// again cannot read on the node that builds it ([LeadsToldAgain]): its seal under
// a key that node does not hold, of a kind its build does not know, or one that
// does not open. Told rather than left out, so the run knows a message is
// missing; never a word of what it said.
const LeadUnread = "### A message that carried this work on cannot be read here" +
	"\nThe engine's copy of it does not open on the node carrying this run on, so it is not told again here."

// ReviewNotKept is a review that sent the work back, said in its place among the
// messages a session is told again ([LeadsToldAgain]) where the turn's record
// no longer keeps the round it closed whole (internal/agent/runner's
// reviewOf): a parked turn past its share folds whole closed rounds into one
// entry keeping their calls by tool and none of their reviews
// (ledger's Iteration.Through). Never a word of what the review said.
//
// IN ITS PLACE, NEVER AMONG THE EARLIER MESSAGES NOT KEPT, which the section
// says before every message it tells: a review can follow a message the section
// tells, and the later of two that differ stands, so said as earlier, the run
// would take the messages before it as standing over a review that may have
// redirected the work. Nor as [LeadUnread], which says the engine holds a copy.
const ReviewNotKept = "### A review that sent this work back, which is not kept" +
	"\nThe round it reviewed is not kept on the parked turn, and what its reviewer said is gone with it, so it " +
	"is not told again here."

// KeyAnswersBefore keys the section [EarlierAnswers] is appended to a fresh
// brief as (internal/agent/runner's continueAgentRun and executeAsAgentRun):
// after the turn's own brief, and before what carries a round on now
// ([KeyCarriedOn]) where one does, so the answers read in the order they came.
const KeyAnswersBefore = "answers_before"

// answersBeforeHeader opens the answers that carried the turn's work on before
// the brief that tells them ([EarlierAnswers]); messagesBeforeHeader opens them
// where one is no answer: an account of what a run waited for.
const (
	answersBeforeHeader  = "## Earlier answers that carried this work on"
	messagesBeforeHeader = "## Earlier messages that carried this work on"
)

// EarlierKind is what an earlier lead told by [EarlierAnswers] was.
type EarlierKind int

const (
	// EarlierAnswer answered a question the work asked: a person's answer, a
	// withdrawal, a refused reply or a held call's outcome.
	EarlierAnswer EarlierKind = iota
	// EarlierCalls is the engine's account of calls a run parked on.
	EarlierCalls
	// EarlierWait is the engine's account of the run a run waited for.
	EarlierWait
)

// EarlierLead is one lead [EarlierAnswers] tells: Text under its own heading,
// and its Kind, which the section's intro names.
type EarlierLead struct {
	Text string
	Kind EarlierKind
}

// EarlierAnswers is the section that tells the leads that carried the turn's
// work on before the brief that tells them, each under a heading naming its
// round ([AnswerRound]; runner's earlierAnswers), oldest first, and how many
// earlier messages are not kept to tell (cut), said as not kept, never as no
// longer kept (the store may keep them still). AN ANSWER IS AN INSTRUCTION TO
// THE TURN, NOT TO ONE ROUND: a run starting a session of its own was told only
// the newest, so told every answer in order, the later of two that differ
// standing, it can tell which came before a later answer or a send-back.
// fresh is such a run; not fresh is the section told again inside the task
// (runner's taskAgain), where [LeadsToldAgain] says the cut, so it is zero.
// BUILT WITH AN OUTLINE, so the task nests each answer ([Prompt.Nested]).
func EarlierAnswers(leads []EarlierLead, cut int, fresh bool) Prompt {
	if len(leads) == 0 && (cut <= 0 || !fresh) {
		return Prompt{}
	}
	header, intro := answersBeforeHeader, earlierIntro(leads, cut, fresh)
	if slices.ContainsFunc(leads, func(l EarlierLead) bool { return l.Kind != EarlierAnswer }) {
		header = messagesBeforeHeader
	}
	if cut > 0 && len(leads) > 0 && fresh {
		intro += "\n\n(+" + strconv.Itoa(cut) + " earlier " + plural(cut, "message", "messages") +
			" that carried this work on, which " + plural(cut, "is", "are") + " not kept to tell: any answer " +
			"among them is not told here.)"
	}
	b := NewBuilder("\n\n")
	b.Heading(KeyAnswersBefore, header+"\n"+intro)
	for i, lead := range leads {
		// A key per lead: a section's key is unique within its prompt, and
		// each lead is its own heading.
		b.Heading("answer_"+strconv.Itoa(i+1), lead.Text)
	}
	return b.Build()
}

// earlierIntro is [EarlierAnswers]' intro to leads, but for the cut beside
// them. AN ACCOUNT OF WHAT A RUN WAITED FOR IS NO ANSWER ([EarlierKind]): it
// says a run asked only where every lead answered a question, says what was
// waited for where none did, and else names both kinds, "the later stands"
// said of answers alone. Said of an account as an answer, a run would read
// the engine's words as a person's instruction to a question nobody asked.
func earlierIntro(leads []EarlierLead, cut int, fresh bool) string {
	if len(leads) == 0 {
		return "Runs before you on this task may have been carried on by answers that are not told here: the " +
			strconv.Itoa(cut) + " earliest " + plural(cut, "message", "messages") + " that carried this work on " +
			plural(cut, "is", "are") + " not kept to tell, so any answer among them is not told."
	}
	if len(leads) == 1 {
		run, held := "A run of this work", ""
		if fresh {
			run, held = "A run before you on this task", " Your session does not hold it."
		}
		switch leads[0].Kind {
		case EarlierCalls:
			return run + " waited for calls of its own, and was carried on by the engine's account of them " +
				"below, in the round its heading names." + held
		case EarlierWait:
			return run + " waited for another run, and was carried on by the engine's account below of how " +
				"that run left its seat, in the round its heading names." + held
		}
		return run + " stopped to ask, and was carried on by the answer below, in the round its heading names." +
			held
	}
	// The later of two answers stands: said after "none of them" to a fresh
	// run, else after the heading's round.
	n, runs, held, stands := strconv.Itoa(len(leads)), "Runs of this work", "", ": where two of them differ, "+
		"the later one stands."
	if fresh {
		runs, held, stands = "Runs before you on this task", " Your session holds none of them.",
			". Your session holds none of them: where two of them differ, the later one stands."
	}
	kinds := map[EarlierKind]int{}
	for _, l := range leads {
		kinds[l.Kind]++
	}
	switch {
	case kinds[EarlierAnswer] == len(leads):
		return runs + " stopped to ask, and were carried on by the " + n + " answers below, in the order they " +
			"came, each in the round its heading names" + stands
	case kinds[EarlierAnswer] == 0:
		waited := "calls of their own or other runs"
		switch {
		case kinds[EarlierWait] == 0:
			waited = "calls of their own"
		case kinds[EarlierCalls] == 0:
			waited = "other runs"
		}
		return runs + " waited for " + waited + ", and were carried on by the engine's " + n + " accounts of " +
			"them below, in the order they came, each in the round its heading names." + held
	}
	return runs + " were carried on by the " + n + " messages below, in the order they came, each in the round " +
		"its heading names: answers to questions a run asked, and the engine's accounts of what a run waited " +
		"for, as each heading says." + held + " Where two answers differ, the later one stands."
}

// AnswerRound is how an answer told to a run names the round of the turn it
// came in (round), to a run working in round current, as a suffix of its
// heading: this round; an earlier one, which a review sent back, since a later
// round of a turn is begun by nothing else; or a later one, which no record of
// this build makes and which is named as it is; "" for a round no record names.
func AnswerRound(round, current int) string {
	switch {
	case round <= 0:
		return ""
	case round == current:
		return " (in this round)"
	case round < current:
		return " (in round " + strconv.Itoa(round) + ", which a review sent back)"
	}
	return " (in round " + strconv.Itoa(round) + ")"
}

// HeldCallListed is what an answer told to a run says of the call it answered
// that was made, where the brief that tells it lists the calls of round, the
// round the answer came in, as work already done ([PriorWorkHeader], which
// heads each round "Iteration" and its number): that the call it says is one of
// those, made once, and that the list says how it ended. Never its outcome a
// second time, which would tell one call twice, the second time as a call of its
// own, as no list of a round's calls tells the one a lead tells.
func HeldCallListed(round int) string {
	n := strconv.Itoa(round)
	return "The call was made once: it is among round " + n + "'s calls, under Iteration " + n + " of the work " +
		"already done earlier in this turn, which says how it ended."
}

// HeldCallCounted is what an answer told to a run says of the call it answered
// that was made, before what became of the call, where the brief that tells it
// holds round, the round the answer came in, under an entry of the work already
// done ([PriorWorkHeader]) that may count the call rather than list it with how
// it ended (internal/agent/runner's heldIn): a span of rounds a park dropped,
// headed by its first round and its last (internal/agent/ledger's
// Iteration.Through), or a round alone, last not past first, whose record
// counts calls of its tool by tool, or its reads past the list's read cap.
// Said so the run reads the call as one of that entry's, never as a call
// beside the ones the entry counts, and is told how it ended, which the entry
// may not say. A span's rounds are written with "to", never the dash its
// heading has, which a run matches all the same.
func HeldCallCounted(round, first, last int) string {
	heading := "Iteration " + strconv.Itoa(first)
	if last > first {
		heading = "Iterations " + strconv.Itoa(first) + " to " + strconv.Itoa(last)
	}
	return "The call is one of round " + strconv.Itoa(round) + "'s calls, which " + heading + " of the work " +
		"already done earlier in this turn may count rather than list, so what became of it is told here. "
}

// EarlierUnread is a lead of kind told to a run that the brief cannot read on
// the node that builds it ([EarlierAnswers]), its heading naming its round as
// [AnswerRound] says it (named): its seal under a key that node does not hold,
// or one that does not open. Told rather than left out, as [LeadUnread] is, so
// the run knows a lead is missing; never a word of what it said, and an
// account of what a run waited for never as an answer.
func EarlierUnread(kind EarlierKind, named string) string {
	what := "An answer"
	if kind != EarlierAnswer {
		what = "A message"
	}
	return "### " + what + " that carried this work on, which cannot be read here" + named +
		"\nThe engine's copy of it does not open on the node carrying this run on, so it is not told here."
}
