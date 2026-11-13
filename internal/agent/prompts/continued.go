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
// every call it made — one it gave up on with no outcome or a failed one — and
// every note it read. What it is told is what it lacks
// — what changed since its last message, and of a session cut off, that it was
// or may have been and what it may not show ([InterruptedSession],
// [InterruptedCopy], [MayBeInterruptedCopy]), with the messages that carried
// its work on, the calls and the notes it may lack ([LeadsToldAgain],
// [CalledAfter], [NotesReadAgain]) — and, where the box it runs in is
// not the one it was last worked in, what that changes. The fresh brief a run
// that starts a session of its own is handed is [BuildPhaseUserMessage]'s, and,
// for one carrying a round on, what became of the run before it and every call
// of the round so far ([CalledBefore]), which its session holds none of.
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
//
// AND A FAILED CALL WHOSE OUTCOME IS UNKNOWN EXCEPTED, as [PriorWorkHeader]
// excepts it: a call still in flight as an earlier round was read is on that
// round's record failed, and told to the session, in the late calls' section
// of this brief or of an earlier one, in a line that says its outcome is
// unknown and that what it did may have taken effect. Told beside it that a
// call that failed did not take effect and to retry it, the run would make it
// again.
const ContinuedRoundRules = "## Already done earlier in this turn" +
	"\nYour earlier rounds of this turn are in this session above, and every call they made" +
	" that succeeded ALREADY RAN:" +
	"\n- **A read** fetched data only: re-run it if you need the data now, never invent it." +
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

// lateCallsHeader introduces the calls of the round that finished only after
// the run that made them stopped waiting for them, and the ones still in flight
// as the round was read ([AfterSave.Late]): calls an earlier run of the work
// made that were still running when a relaunch carried the work on, which its
// record took only after that run was replaced; calls a run gave up on, or
// whose session ended under them, before they returned; and calls whose outcome
// is unknown, not recorded as finished when the round's collection stopped
// waiting for them (internal/agent/runner's UnfinishedNote). TOLD UNDER WORDS
// OF THEIR OWN, WHATEVER THE SESSION'S END, since no point tells them and each
// of the other headings is untrue of them: the session above may show such a
// call with no outcome, as still running, as interrupted or as failed, its
// harness having died, given up on it, been answered that it was still running
// as its session on the bridge ended, or been replaced before the call
// returned; and an earlier brief may have told it, since every continuation of
// the work tells it again. So it is said to be ONE call, made once, whose
// outcome as told here stands, never one the session lacks.
//
// NEVER THAT THE CALLS FINISHED, NOR THAT EVERY ERROR FAILED. A call still in
// flight is failed on the record, so that nothing takes it for a delivery or a
// submission, and its line says its outcome is unknown and that what it did may
// have taken effect. Under words saying the calls finished after their run
// stopped waiting for them, and that one marked `→ error` did not take effect,
// the section contradicted that line, and a run that trusted its words made
// the call again: a second post or delegation, the duplicate this section
// exists to prevent. So the words say a call marked `→ error` did not take
// effect unless its line says its outcome is unknown, in the line's own terms,
// and that such a one may have, to be checked before it is made again. And
// they are true of every such call, as the line's are: one still running as the
// turn stopped waiting for it, one whose process stopped, and one that had
// returned, its harness handed its answer, whose update the store had not
// taken. Its run never stopped waiting for that last one, so of each call the
// words say only that it is one its run stopped waiting for before it
// finished, or one whose outcome is unknown.
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
// returned — replaced, and the run carrying the work on never told it, or told
// it failed where it gave up on it — and the work's submission is the one a
// run carrying it on makes (internal/agent/runner's sinceAnswer, which never
// takes a late one, and replays the newest). Read as the work's, a late one
// would leave a run that took it for done ending without its own, and the work
// collected as one that submitted nothing.
const lateSubmission = "A `" + toolname.SubmitWork + "` among them does NOT stand as this work's submission:" +
	" the run that made it had stopped waiting for it before it returned. When your work is done, call `" +
	toolname.SubmitWork + "` yourself."

// earlierSubmission tells a run that a successful `submit_work` among the calls
// listed before it, under [calledAfterHeader], [calledSinceHeader] or
// [calledBeforeHeader], is no submission of the work it carries on. Every call
// a brief tells was made before the relaunch whose brief tells it, and none of
// those is the work's submission (internal/agent/runner's sinceAnswer and
// carriedRound): the collection takes a submission only from the calls made
// after the round's last answer, so one made before a run's question, or
// before an earlier answer, is not the round's; a round lost, or refused on a
// replaced sign-in, whose calls since that answer hold one is collected on it,
// never carried on, so a brief carrying such a round on tells only one made
// before that answer; and a send-back's continuation can tell the submission
// its review sent back. Told as `→ success` with no such word, it read as the
// work's: a run that took it for done could end without its own, and the work
// be rescued as incomplete.
const earlierSubmission = "A `" + toolname.SubmitWork + "` among them does NOT stand as this work's" +
	" submission: the work was carried on after it. When your work is done, call `" + toolname.SubmitWork +
	"` yourself."

// heldSubmission tells a run that a successful `submit_work` of its round that
// the session above shows, and that no list of the brief tells, is no
// submission of the work it carries on ([AfterSave.HeldSubmission]). It stands
// no more than a listed one ([earlierSubmission]), and for the same reason:
// the session holding it was saved past it, or is the copy its job's own box
// holds where that job's harness ended its own run, so it was made before the
// relaunch whose brief continues that session. The commonest is a run that
// submitted and then asked, which its gate allows: the copy in its box shows
// the call answered "submitted", and the cut its answer's opening takes falls
// past it (internal/agent/runner's sinceAnswer).
//
// SAID FIRST, WHETHER OR NOT A CALL IS LISTED, in words true of the session
// rather than of a list. That copy is told no call at all, so its brief was the
// answer's lead alone, which says to finish the task as it asks: a run that
// read its transcript's submission as done ended without its own, and the work
// was rescued as incomplete. Said under a list, as [earlierSubmission] is, it
// would name a call that list does not hold.
const heldSubmission = "A `" + toolname.SubmitWork + "` the session above shows does NOT stand as this" +
	" work's submission: the work was carried on after it. When your work is done, call `" +
	toolname.SubmitWork + "` yourself."

// AfterSave is what a session's run did through the seat's tools after the
// session was saved, as a run that continues the session is told it
// ([CalledAfter]).
type AfterSave struct {
	// HeldSubmission says the session holds a successful `submit_work` of its
	// round that no list tells: one its save point holds, or any but a late one
	// or one nobody waited for in the copy its job's own box holds where that
	// job's harness ended its own run. It is said first, whether or not a call
	// is listed, as no submission of the work ([heldSubmission]). Set only by
	// the round's own calls, so never by one the session holds from an earlier
	// turn, as the session its conversation's last turn left holds that turn's;
	// and never for one among Calls, which EarlierSubmission says, nor among
	// Late, which LateSubmission says.
	HeldSubmission bool

	// Owed are the reads of the round an earlier message to the session
	// counted without listing them, its read cap spent, which the session holds
	// only as that count (internal/agent/runner's heldAt, Resume.Owed and
	// SessionBasis.Owed): the reads its job's brief left out, as the prior-work
	// ledger renders them within its budgets, under one read cap with Calls'
	// and Late's, spent on these LAST (ledger.FormatCallGroups), "" for none.
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
	// one read cap with Late's and Owed's, spent on Late first and on Owed
	// last (ledger.FormatCallGroups), "" for none. For a run that starts a
	// session of its own ([CalledBefore]), every call of the round but Late's.
	Calls string

	// EarlierSubmission says a successful `submit_work` is among Calls, which
	// is told after them as no submission of the work ([earlierSubmission]):
	// every call a brief tells was made before the relaunch whose brief tells
	// it, so none is the submission of the work that relaunch carries on.
	// Never set for one among Late, which LateSubmission says in Late's own
	// words.
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
	// calls may be in it as well. They are told under [calledSinceHeader],
	// never as calls it lacks, and the dropped ones counted since that point.
	Interrupted bool

	// Late are the calls of the round that finished only after the run that
	// made them stopped waiting for them — its record took them after that run
	// was replaced by a relaunch that carried the work on
	// (sandbox.BridgeCall.Late), or the run gave up on them, or its session
	// ended, before they returned (sandbox.BridgeCall.Abandoned) — as the
	// prior-work ledger renders them within its budgets, under one read cap
	// with Calls' and Owed's, spent on these first, "" for none: none of them
	// is among Calls. Told in a section of their own, under
	// [lateCallsHeader], whatever Interrupted says ([lateBeforeHeader] for a
	// run that starts a session of its own).
	//
	// AND THE CALLS STILL IN FLIGHT AS THE ROUND WAS READ, rendered and capped
	// with them: each one whose outcome is unknown (ledger.Call.Unfinished),
	// failed, its line saying that what it did may have taken effect. None of
	// those finished on the record, so the section's words never say its calls
	// finished, and say an error whose line says its outcome is unknown may
	// have taken effect.
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
// run did through the seat's tools after the session was saved: the calls the
// record lists, then how many more it kept only in a tally and that tally, the
// way every other reader of a long run's calls is told what was dropped — of a
// session cut off, as calls it may already show ([AfterSave.Interrupted]) —
// and then, in a section of their own, the calls that finished only after the
// run that made them stopped waiting for them and the ones whose outcome is
// unknown ([AfterSave.Late]), and how many more such calls the record dropped
// ([AfterSave.LateDropped]). "" where there is none of any and the session
// holds no submission of its round.
//
// THE READS AN EARLIER MESSAGE COUNTED WITHOUT LISTING THEM COME FIRST
// ([AfterSave.Owed]), in a section of their own: the session holds them only
// as that count, they are the oldest calls the brief tells, and they are told
// in words true of reads the session was told were made, never as calls after
// its save. Under the words of calls after a save they would be calls its own
// run made after it was saved, which they are not.
//
// THE TALLY ONCE, beside the LAST count that needs it in the order the
// sections come (the reads dropped since the earlier message counted them,
// then the calls dropped after the save, then the late calls dropped), since
// every count says the tally is below it ([tallyBesideLast]).
//
// A SUCCESSFUL SUBMISSION AMONG EITHER LIST is said to be none of the work's,
// right after the list it is in and in words true of that list: the calls
// after the save ([AfterSave.EarlierSubmission]) and the late ones
// ([AfterSave.LateSubmission]). AND ONE THE SESSION HOLDS IS SAID FIRST
// ([AfterSave.HeldSubmission]), before every section and where none lists a
// call: it is in no list, and the session that shows it is the one the brief
// continues.
func CalledAfter(after AfterSave) string {
	var sections []string
	if after.HeldSubmission {
		sections = append(sections, heldSubmission)
	}
	tally := tallyBesideLast(after.Tally, after.OwedDropped, after.Dropped, after.LateDropped)
	if owed := toldList(owedHeader, after.Owed, "", owedDropped(after.OwedDropped), tally[0]); owed != "" {
		sections = append(sections, owed)
	}
	if since := calledSince(after, tally[1]); since != "" {
		sections = append(sections, since)
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
// of a session cut off, since the newest point it is known to hold — "" where
// the record lists none and dropped none; tally is the record's tally where
// this section's count is the last the brief gives, "" where a later one says
// it ([tallyBesideLast]).
//
// A SUCCESSFUL SUBMISSION AMONG THE CALLS IS SAID NOT TO STAND right after
// them ([AfterSave.EarlierSubmission]), before the count of calls dropped
// after the save and its tally, which list none: the sentence speaks of the
// calls listed above it, as [lateSubmission] follows the late calls.
func calledSince(after AfterSave, tally string) string {
	header := calledAfterHeader
	if after.Interrupted {
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
// before it did through the seat's tools: every call of the round, as
// internal/agent/runner's calledAfter tells them from the round's start. First
// the calls its record lists, with a successful `submit_work` among them said
// not to stand ([AfterSave.EarlierSubmission]), then how many more it kept only
// in a tally; and then, in a section of their own, the calls that finished only
// after the run that made them stopped waiting for them and the ones whose
// outcome is unknown ([AfterSave.Late]), with a late submission said not to
// stand and how many such calls the record dropped; and the record's tally
// once, beside the last of those counts, which each says is below it
// ([tallyBesideLast]). Read from Calls, EarlierSubmission, Dropped, Tally,
// Late, LateSubmission and LateDropped; "" where the round made no call.
//
// IN WORDS OF ITS OWN, never [CalledAfter]'s: those speak of a session above,
// and a run of its own has none, and remembers none of the calls. Told them as
// calls made after a save, or ones the session may show, it would look for
// them in a transcript it does not have. Told nothing of them, the reseed
// alone saying only that the run before it did things through the seat's
// tools, it could make them again: a second post, a second delegation.
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
// ([SteerMessage]), "" for none: every note the turn has read, to a session its
// run may have been cut off in; every note from a read whose take's answer
// never reached its harness on, to one whose harness ended its own run; and,
// with them, a note the run's bridge served its harness, which may have
// delivered it unconfirmed, whether the turn read it before this message or
// reads it in it. The session may not show them, and may show some.
//
// NEVER "EVERY NOTE" IN ITS WORDS: the notes before such a read are the
// session's, and are not told again.
//
// SAID AS NOTES IT MAY ALREADY SHOW, ONE NOTE WHEREVER SHOWN, AND COUNTED. Each
// note says it stands for the rest of the turn, so a model reading one both above
// and here could do twice what it asks; and the notes the brief is the first to
// read follow these in the same form, which the count tells apart.
//
// AND THE NOTES AFTER THEM AS ONES THE TURN HAD NOT READ, never as new to the
// session. Which notes the turn has read is the run's record's word, and a
// harness can be handed a note before the record says it was read: OpenCode's
// delivery is recorded read only once its plugin confirms it. A note the run's
// bridge recorded serving is told among these, however it was delivered
// (internal/agent/runner's toldNotes); one a bridge of an older build served is
// not marked, and the record cannot tell it from a note no harness was handed.
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
	span, ended := nextTurnSpan(m, words)
	w.WriteString(span)
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

// nextTurnSpan is how far the session a conversation's last turn left runs, as
// the clause that follows "as your turns left it", and where how that turn
// ended is told: [BuildNextTurn]'s words, and [NextTurnAgain]'s.
func nextTurnSpan(m NextTurn, words nextTurnWords) (span, ended string) {
	var w strings.Builder
	ended = "How the " + words.whole + "'s earlier " + words.turn + "s ended is told below."
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
	return w.String(), ended
}

// NextTurnAgain is the lead of a run that continued the session its
// conversation's last turn left, as a later brief to a session of the work that
// may lack it tells it again (internal/agent/runner's leadsAgain): the turn
// boundary [BuildNextTurn] said, in the past tense and in a schedule's words for
// a schedule's fire, the two things that boundary makes stale, and that what
// the session says about files on disk from before it is what the earlier
// turn's box held; and, where that session was cut off when it was kept, that
// it may hold a call with no result from before the turn began. The task it
// opened with follows it, rendered again by its teller.
//
// NEVER THE NEW BOX. [BuildNextTurn] tells a run it is in a new box, on a fresh
// checkout or holding none of the repositories the session checked out of
// Gitlet, where the earlier box's paths were: true of the box that lead's run
// started in, and stale, or false, of the box a later job continues the
// session in, which may be that very box after its job worked there. The
// brief that tells this again says the box it is in, in its own lead and its
// cut-off section, so nothing here repeats a present-tense claim about a box.
//
// AND NEVER "EVERYTHING ABOVE": the session above may hold this turn's own work
// after the boundary, where its harness wrote any of it, so the boundary is
// told as where the session began rather than where it ends.
func NextTurnAgain(m NextTurn) string {
	words := nextTurnWordsFor(m)
	span, ended := nextTurnSpan(m, words)
	var w strings.Builder
	w.WriteString("### A new " + words.turn + " of this " + words.whole + " began here" +
		"\nThe session above began as this " + words.whole + " as your " + words.turn + "s left it" + span +
		". This " + words.turn + " began after that, and the task below is this " + words.turn + "'s. " + ended +
		"\n- **What was read before this " + words.turn + " began may be stale:** read it again before " +
		"relying on it." +
		"\n- **Tools activated and skills loaded before this " + words.turn + " began were set up anew for " +
		"it:** activate or load them again before relying on them." +
		"\n\nWhat the session above says about files on disk from before this " + words.turn + " began is " +
		"what the box of the " + words.turn + " it came from held.")
	if m.Interrupted {
		w.WriteString("\n\n" + baseCutOffAgain)
	}
	return w.String()
}

// baseCutOffAgain is [NextTurnAgain]'s word that the session a conversation's
// last turn left was cut off when it was kept ([InterruptedSession], as
// [BuildNextTurn] tells it): of where the session began, never of where it
// ends, which the brief that tells it again says in its own cut-off section.
const baseCutOffAgain = "The session this turn began from was cut off when it was kept, so before this turn " +
	"began it may hold a call with no result, which may or may not have run. Check what such a call was " +
	"meant to do before doing it again."

// leadsAgainHeader opens the messages a session is told again that carried its
// work on and that it may not show ([LeadsToldAgain]).
const leadsAgainHeader = "## Messages that carried this work on that the session above may not show"

// LeadsToldAgain is the section that tells a session again the messages that
// carried its work on that it may not show, each already rendered under a
// heading of its own (internal/agent/runner's leadsAgain), "" for none: the
// answers, the review and the turn's task the brief of the job whose session it
// continues opened with, or told again in turn, where that session is not known
// to hold that brief (its harness cut off, or never known to have ended its
// own run), in the order they were sent; and how many earlier such messages the
// engine no longer keeps to tell again (cut), said rather than left out.
//
// SAID AS MESSAGES IT MAY ALREADY SHOW, ONE MESSAGE WHEREVER SHOWN, as notes
// told again are ([NotesReadAgain]): a session cut off may hold any of them, an
// answer read both above and here could be acted on twice, and two of them that
// differ are told apart by their order, the later standing.
func LeadsToldAgain(leads []string, cut int) string {
	if len(leads) == 0 && cut <= 0 {
		return ""
	}
	var w strings.Builder
	w.WriteString(leadsAgainHeader + "\n")
	switch len(leads) {
	case 0:
		w.WriteString("The session above may not show every message that carried this work on: the run working " +
			"in it may have been cut off before its harness wrote one down.")
	case 1:
		w.WriteString("The session above may not show the message below: it carried this work on, and the run " +
			"working in the session may have been cut off before its harness wrote it down. A message shown " +
			"both above and here is ONE message, sent once, not two: do what it asks once.")
	default:
		w.WriteString("The session above may not show the " + strconv.Itoa(len(leads)) + " messages below: each " +
			"carried this work on, and the run working in the session may have been cut off before its " +
			"harness wrote it down. They are in the order they were sent. A message shown both above and here " +
			"is ONE message, sent once, not two: do what it asks once, and where two of them differ, the later " +
			"one stands.")
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
