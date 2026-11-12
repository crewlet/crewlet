package runner

import (
	"cmp"
	"context"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// A RELAUNCH THAT CONTINUES ITS TURN'S SESSION, and what its run is told.
//
// Every relaunch of a turn — a reviewer's send-back, a round a person's answer,
// a loss or a replaced sign-in carries on — continues the session of the job it
// follows where its launcher says it can ([SessionProber]): that job's kept
// capture, in whatever box the relaunch gets (internal/sandbox, restore.go).
// So its brief is the session's NEXT MESSAGE, built from what the session
// lacks:
//
//   - what carries it on: the review and the engine's correction with the task
//     restated and the rules its earlier rounds' calls are read by
//     ([prompts.BuildContinuedRound]), the answer to its question — and, of a
//     held call, what became of it, which tells the call a person approved
//     once: no list below tells it again ([carried.tellsHeld]) — or what cut it
//     off;
//   - that it may end mid-call, where its harness was cut off when it was saved;
//   - every call its run made through the seat's tools that the session may not
//     have been answered when it was saved — every call the round's log took
//     after the read its save point is ([SessionBasis.Through]), so after the
//     point it was saved at or in flight then, whatever the log dropped below
//     it — with the call's outcome, held to the prior-work ledger's budgets;
//     and how many more the log took after it and dropped from its middle,
//     which it keeps only as a tally, with that tally ([prompts.CalledAfter]) —
//     of a session cut off, whose save point is the newest its launch was known
//     to hold rather than where it ends, as calls it may already show, each
//     shown both there and in the session one call;
//   - every call the log took after its job was replaced, whatever its number
//     ([BridgedCall.Late]), which no brief of the round had told when the log
//     took it and the session of the job that made it may not hold ([heldAt]),
//     and every call nobody waited for by the time it returned
//     ([BridgedCall.Abandoned]) — given up on by its harness, or outliving its
//     job's session — whose outcome that session holds none of, told to every
//     continuation built after it landed: apart, under words of their own
//     whatever the session's end — one may be shown, as interrupted, failed or
//     with no outcome, or told by an earlier brief, since every continuation
//     of the round tells it again, and it is one call — and a submission
//     among them told as none of the round's;
//   - every note a person sent the turn that the session has not read — read
//     after it was saved, by the run that went on in it or by a review, which
//     is never the session's — or, of a session cut off, every note the turn
//     has read, as notes it may already show ([prompts.NotesReadAgain]): its
//     job's last take of them was recorded read before its harness had them,
//     and it is not known to hold its job's brief, which told the notes the
//     session it continued lacked; or, of one whose notes stop before a read
//     the record marks unanswered — a take whose answer never reached its
//     harness — every note past that, as notes it may already show
//     ([Runner.notesHeldBy]); and then the ones this brief is the first to
//     read — under the same header, up to the last of them the run's bridge
//     served a harness, which may have delivered it unconfirmed, and any read
//     one so served likewise ([Runner.toldNotes]).
//
// AND IN TWO FORMS, because which box the launch gets is the launch's to learn:
// one for the box the session was last worked in, and one for any other, which
// adds what a new box changes — a fresh checkout, and a repository of
// Gitlet to check out again ([prompts.BoxReplaced]). The fresh
// brief, for a run whose box cannot take the session up after all, rides
// beside them.
//
// A ROUND CARRIED ON IN THE JOB'S OWN BOX — a person's answer, a replaced
// sign-in, where the launch takes up no kept capture of the job
// ([AgentRunRequest.Continue]) — continues the copy of the session that box
// holds, which no capture describes: its brief is what carries it on and then
// the notes it has not read. Where the job's harness is not known to have ended
// its own run ([Resume.HarnessEnd]), that copy may lack calls the harness made
// and had answered without writing them, and is not known to hold the job's
// brief: so between the two it is told that it was cut off, or may have been,
// every call of the round, as a session cut off is told its calls
// ([Runner.inPlaceAfter]), and every note the turn has read, as it is told its
// notes ([Runner.inPlaceNotes]).
// Where it did, the copy holds every call its job made and every call its
// job's brief told — every call of the round but the ones the log took late,
// where that brief told the round's calls (a fresh run's brief tells none) —
// and is not known to hold a late one, nor the outcome of one its harness gave
// up on ([heldAt]), which it shows with no outcome or a failed one: it is told
// those, as every continuation of the round built after they landed is told
// them — and every note from the first read the record marks unanswered on, as
// notes it may already show, since it lacks that one.
//
// THE RUN'S GUARD IS THE SESSION'S IT RUNS IN ([Runner.sessionDecided]): the
// skills the launch state of the job it follows held and that job's own loads —
// those the log took through the point its capture was saved at, by the same
// test the calls after it are told by ([ownCalls], [heldAt]), where the launch
// restores it, and all of them for the job's own copy in its box only where its
// harness ended its own run, and never a late one nor one nobody waited for —
// because the continued model has those bodies in front of it, and a skill a
// call its harness gave up on loaded never reached it; none for a run that
// starts a session of its
// own. A session cut off is saved at the newest point its launch was known to
// hold, so a skill it loaded after that point is not armed, nor is one its job
// loaded where its own copy in its box is continued after its harness was cut
// off: each is loaded again before a call it covers, the safe direction, since
// its body may never have reached the session. Armed once the launch has
// decided, before the run starts, and put right where the run's box finds as it
// starts that it cannot take the session up after all and starts one of its
// own: its box says so before the run's first call ([Runner.sessionStarted]),
// and its job's record keeps what it said for every later rebuild of the guard
// ([Resume.Continued]).
//
// A CONVERSATION'S NEXT TURN continues the session its last turn left: its first
// launch the head its dispatch read, and a relaunch of a turn that has kept no
// session of its own yet the one its first launch froze (internal/sandbox,
// restore.go). That session is another turn's, in a box of its own, so its run
// is told where this turn begins and what the new box changes — and, of a
// session that turn went on past, that it was kept while the turn still worked
// and what the turn did after is not in it, or of a lost turn's, that it may not
// hold the last of what the turn did ([prompts.BuildNextTurn]) — then handed
// the launch's own fresh brief whole —
// the task, the turn's earlier rounds and notes, and how the conversation's
// earlier turns ended — none of which it read ([Runner.baseSession]). ITS GUARD
// IS EMPTY: the skills that session loaded were another turn's, set up anew for
// this one, which the lead says.

// sessionBasis asks the turn's launcher which kept session this relaunch
// continues ([SessionProber]). A launcher that cannot say continues none.
func (r *Runner) sessionBasis(ctx context.Context) SessionBasis {
	prober, ok := r.cfg.Launcher.(SessionProber)
	if !ok {
		return SessionBasis{Why: "the turn's launcher continues no kept session"}
	}
	return prober.ProbeSession(ctx)
}

// keptSession is how a relaunch continues basis, nil where basis names no kept
// session — all of it but what carries the session on, which the caller leads
// it with ([keptBrief.led]): what came after the session was saved of round,
// the calls of the round its job worked in, as surface reads them
// ([Runner.calledAfter]) — but the call a person approved for leadHeld, the
// held call that lead tells what became of ([carried.tellsHeld]), "" for none
// — then every note it has not read — of a session cut off, every note the
// turn has read, as notes it may already show — and the ones fresh holds, the
// served among them told as notes it may already show too ([Runner.toldNotes]),
// in the box it was last worked in, and in another with what that changes. The
// engine's own words and the seat's, every foreign word in them fenced or
// defused under the relaunching segment's token; branch and the basis's home
// are the engine's record of the session, defused too.
//
// NOT WHAT CARRIED ITS JOB ON: a session cut off is no more known to hold its
// job's brief than the copy a job's own box holds ([Runner.inPlaceAfter]), and
// one that lacks it lacks the lead it opened with too — the turn's task where
// the job continued the session its conversation's last turn left, a review
// that sent the round back, an earlier answer — which the engine keeps no copy
// of to tell again.
//
// BUILT BEFORE ITS LEAD, so before the relaunch asks the seat's budget for the
// last time: telling the calls after the save rewrites a payload past its
// budget with the seat's auxiliary model, charged to the same counters, and
// that rewrite can be what fills the window — while the lead of a round a
// person's answer carries on says what the call they approved returned, and
// that call is made only once every check that could refuse the relaunch has
// passed ([Runner.continueAgentRun]). Led after the check, the brief costs
// nothing the check did not count.
func (r *Runner) keptSession(ctx context.Context, basis SessionBasis, round roundCalls, leadHeld string,
	surface *tools.Surface, fresh []steer.Note, branch string) *keptBrief {

	if !basis.Continues() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	var after []string
	if basis.Interrupted {
		after = append(after, prompts.InterruptedSession)
	}
	// OF A SESSION CUT OFF, AS CALLS IT MAY ALREADY SHOW: its save point is the
	// newest its launch is known to hold rather than where it ends
	// (internal/sandbox's savepoint.go), so the calls after it may be in it.
	// AND NEVER THE CALL A PERSON APPROVED THAT ITS LEAD TELLS (leadHeld).
	called := r.calledAfter(ctx, round, &basis.Through, leadHeld, surface, b)
	called.Interrupted = basis.Interrupted
	after = append(after, prompts.CalledAfter(called))
	// THEN THE NOTES IT HAS NOT READ, past its cursor, and the ones fresh holds,
	// a note the run's bridge served a harness among either told as one it may
	// already show ([Runner.toldNotes]); of a session cut off,
	// EVERY NOTE THE TURN HAS READ, as notes it may already show, as the copy a
	// job's own box holds is told them ([Runner.inPlaceNotes]): its job's last
	// take of them was recorded read before its harness had them, and it is not
	// known to hold its job's brief, which told the notes the session it
	// continued lacked, and no record says which those were (internal/sandbox's
	// notesHeld states none it holds). AND OF ONE WHOSE NOTES STOP BEFORE A READ
	// MARKED UNANSWERED, every note past that, as notes it may already show
	// ([Runner.notesHeldBy]): the session lacks that note, and holds the
	// answered ones after it.
	if basis.Interrupted {
		after = append(after, r.toldNotes(r.readsSince(0), true, fresh)...)
	} else {
		held, unsure := r.notesHeldBy(basis.NoteCursor)
		after = append(after, r.toldNotes(r.readsSince(held), unsure, fresh)...)
	}
	return &keptBrief{basis: basis, after: after, newBox: prompts.BoxReplaced(provenance.Neutralise(b, branch),
		provenance.Neutralise(b, basis.Home), r.checkoutName())}
}

// inPlaceAfter is what a relaunch carrying the round on in its job's own box
// tells the session there between what carries it on and the notes the brief
// is the first to read ([Continuation.Brief]), where the job's harness is not
// known to have ended its own run ([Resume.HarnessEnd]): that the session was
// cut off ([prompts.InterruptedCopy]) — that it may have been, where the job's
// record says nothing of how its harness ended ([prompts.MayBeInterruptedCopy])
// — said whether or not a call is listed; every call of the round
// ([Runner.calledInPlace]), the ones the log took late apart under words of
// their own — but the call a person approved that the answer carrying the
// round on tells what became of ([carried.tellsHeld]), which that lead alone
// tells. The notes follow it ([Runner.inPlaceNotes]).
//
// WHERE THE HARNESS ENDED ITS OWN RUN, ONLY THE CALLS THE LOG TOOK LATE
// ([BridgedCall.Late]) AND THE ONES NOBODY WAITED FOR ([BridgedCall.Abandoned]),
// whose outcomes that copy is not known to hold ([heldAt] at a nil save point),
// while it holds every other call its job made and every call its job's brief
// told: every other call of the round, where that brief told the round's calls
// (a fresh run's brief tells none). A call nobody waited for is one the job
// made whose harness gave up on it — a harness ends its own run once every
// call it made is answered or given up on — or whose session ended under it:
// the copy shows it with no outcome or a failed one, its harness having been
// told it timed out, though it ran on and its effect happened, and it is told
// under the late calls' words, which hold of it. A late call is one the log took after
// the job that made it was replaced — after this job's opening, or after the
// read this job's brief was built from, which its opening marks — so no brief
// had told it when the log took it, and that job's session may not hold its
// outcome. This copy may show one with no outcome or as interrupted, where
// it continued that job's session; it may hold one as told, where its job's
// own brief told it, or as made, where it is its job's own call, carried only
// once the job was replaced after its append failed. Told under their own
// words all the same, a submission among them as none of the round's, with how
// many the log dropped from its middle ([Runner.calledAfter]), so a copy whose
// harness ended its own run is told a late call as every other continuation of
// the round is, rather than holding it at no save point and never being told
// it. The rest is told it nowhere: so the copy of a job whose brief told none
// of the round's calls — a fresh run carrying the round on — is told none of
// the calls before it either.
//
// Nil where nothing is left to tell — and where carry continues no copy in
// that box at all: a lost job's, never continued in place; a round lost a
// second time, which continues nothing; and one whose box is gone
// ([carried.boxKept]), which no launch continues in, so its calls are neither
// rewritten nor charged for a brief nothing would send.
//
// NOT WHAT CARRIED THE JOB ON: a copy that lacks the job's brief lacks the
// lead it opened with too — the turn's task where the job continued the
// session its conversation's last turn left, a review that sent the round
// back, an earlier answer — which the engine keeps no copy of to tell again,
// as a kept session cut off may lack it ([Runner.keptSession]).
//
// BUILT BEFORE ITS LEAD, as the kept session's brief is ([Runner.keptSession]):
// telling the calls rewrites a payload past its budget with the seat's
// auxiliary model, charged to the same counters, which the relaunch asks the
// seat's budget for once every brief's rewrites are spent. A launch that
// continues the copy sends all of it, so where a capture measured the copy's
// context the floor its launch is held to grows with it; a copy no capture
// measured — every copy in a company that keeps no session — holds its launch
// to the static floor whatever its brief (internal/engine's continuedFloor).
func (r *Runner) inPlaceAfter(ctx context.Context, carry carried, round roundCalls,
	surface *tools.Surface) []string {

	if carry.session() == "" || !carry.boxKept() || !carry.keeps() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	end, leadHeld := r.cfg.Resume.HarnessEnd, carry.tellsHeld()
	if end.EndedOwnRun() {
		if late := prompts.CalledAfter(r.calledAfter(ctx, round, nil, leadHeld, surface, b)); late != "" {
			return []string{late}
		}
		return nil
	}
	after := []string{prompts.MayBeInterruptedCopy}
	if end == sandbox.HarnessEndCutOff {
		after[0] = prompts.InterruptedCopy
	}
	if called := prompts.CalledAfter(r.calledInPlace(ctx, round, leadHeld, surface, b)); called != "" {
		after = append(after, called)
	}
	return after
}

// inPlaceNotes is what a relaunch carrying the round on in its job's own box
// tells the session there of the notes a person sent the turn, after
// [Runner.inPlaceAfter] ([Continuation.Brief]): where the job's harness is not
// known to have ended its own run, every note the turn has read, as notes it
// may already show — a copy not known to hold its job's brief lacks the notes
// that brief told, and no record says which those were; where it did, every
// note from the first read the record marks unanswered on, the same way — that
// take's answer never reached the harness, so the copy lacks its notes, and
// holds the answered ones after it and every one before ([Runner.notesHeldBy]);
// and then the notes fresh holds, the ones the brief is the first to read, the
// served among them told as notes it may already show ([Runner.toldNotes]).
//
// THE NOTES FRESH HOLDS ALONE where carry continues no copy in that box, as
// [Runner.inPlaceAfter] tells nothing there.
func (r *Runner) inPlaceNotes(carry carried, fresh []steer.Note) []string {
	switch {
	case carry.session() == "" || !carry.boxKept() || !carry.keeps():
		return renderedNotes(fresh)
	case !r.cfg.Resume.HarnessEnd.EndedOwnRun():
		return r.toldNotes(r.readsSince(0), true, fresh)
	}
	if first := r.firstUnanswered(); first > 0 {
		return r.toldNotes(r.readsSince(first-1), true, fresh)
	}
	return r.toldNotes(nil, false, fresh)
}

// keptBrief is a relaunch's continuation of a kept session but for its lead
// ([Runner.keptSession]): the session it continues, what the brief tells it
// after the lead, and what a box other than the one it was last worked in
// changes.
type keptBrief struct {
	basis  SessionBasis
	after  []string
	newBox string
}

// led is k's continuation led by lead, what carries the session on: the brief
// for the box the session was last worked in, and the one for any other. Nil
// for a nil k, a basis that names no kept session.
func (k *keptBrief) led(lead string) *SessionContinuation {
	if k == nil {
		return nil
	}
	parts := append([]string{strings.TrimSpace(lead)}, k.after...)
	parts = slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" })
	brief := strings.Join(parts, "\n\n")
	return &SessionContinuation{Basis: k.basis, Brief: brief, Replaced: brief + "\n\n" + k.newBox}
}

// baseSession is how a launch continues the session its conversation's last
// turn left, nil where basis names none: the lead that says where this turn
// begins and what the new box changes ([prompts.BuildNextTurn]) — in a
// schedule's words where the turn's conversation is a schedule's, the session
// its last fire's ([notify.Scheduled]) — and, where that turn went on past the
// session or was lost, that the session ends where it was kept rather than
// where the turn did; then fresh, the launch's own fresh brief or reseed,
// whole. Only in a box of its own, so its same-box brief is empty. The base's
// turn and home are the engine's record of that session, defused under the
// launching segment's token.
func (r *Runner) baseSession(basis SessionBasis, fresh string) *SessionContinuation {
	if !basis.ContinuesBase() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	base := basis.Base
	lead := prompts.BuildNextTurn(prompts.NextTurn{
		BaseTurn: provenance.Neutralise(b, base.Turn), EndedAt: base.EndedAt,
		CapturedAt: base.CapturedAt, WentOn: base.WentOn, Lost: base.Lost,
		Home: provenance.Neutralise(b, base.Home), Interrupted: base.Interrupted,
		Checkout: r.checkoutName(),
		Fire:     notify.Scheduled(r.cfg.Turn.ConversationKey),
	})
	return &SessionContinuation{Basis: basis, Replaced: lead + "\n\n" + strings.TrimSpace(fresh)}
}

// namingKeptSession is brief, a run's fresh brief, naming the kept session
// basis read that the run does not continue where it starts a session of its
// own ([prompts.KeptSession]): where the probe read one (KeptTurn) and the
// run's surface carries the tool that reads it — a sentence sending a run to a
// tool its surface lacks is one it cannot follow — and brief itself elsewhere.
// The turn is the engine's record, defused under the launching segment's
// token; why the launch continues none is the log's, never the brief's
// ([prompts.KeptSession] says why).
func (r *Runner) namingKeptSession(brief prompts.Prompt, basis SessionBasis,
	surface *tools.Surface) prompts.Prompt {

	if basis.KeptTurn == "" || !slices.Contains(surface.Universe().Names(), toolname.ReadSession) {
		return brief
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	return brief.Then(prompts.KeyKeptSession, prompts.KeptSession(provenance.Neutralise(b, basis.KeptTurn),
		basis.KeptTurn == r.cfg.Turn.RunID, notify.Scheduled(r.cfg.Turn.ConversationKey)))
}

// checkoutName is the checkout the executor's surface offers, the tool that
// checks a repository of Gitlet out, "" where it offers
// none: a run continuing its session in a new box that can check those
// repositories out is told it must again, since the box holds none of them
// ([prompts.BoxReplaced], [prompts.BuildNextTurn]).
func (r *Runner) checkoutName() string {
	if r.cfg.Checkout == nil {
		return ""
	}
	return r.cfg.Checkout.Name()
}

// calledAfter is what a session saved at through — its save point, how many of
// round's calls its run's log had taken at the read the capture was placed by
// ([SessionBasis.Through]) — is not known to have been answered of round:
// every call the log still lists that the session does not hold ([heldAt]),
// in the order the bridge admitted them — the ones it took after that read,
// with how many more it took after the read and dropped from its middle and
// the round's tally of every call it dropped ([droppedAfterSave]); and, apart,
// the ones it took late ([BridgedCall.Late]) and the ones nobody waited for
// ([BridgedCall.Abandoned]), with how many such it dropped from its middle
// ([prompts.AfterSave.LateDropped]).
//
// A NIL through is the job's own copy of its session in its box whose harness
// ended its own run, as [heldAt] reads it: it holds every call its job made and
// every one its job's brief told — every call of the round but the late ones,
// listed or dropped, where that brief told the round's calls — and the outcome
// of each but one nobody waited for, so it is told the late ones and those
// alone, listed and dropped, as every continuation is told them, with the
// round's tally.
//
// A DROPPED LATE CALL IS COUNTED UNDER THE LATE CALLS' WORDS, WHATEVER THE
// POINT: it keeps no mark and no number, so no save point can be read against
// it, and counted under the words of calls after a save it would read as one
// the session's own run made after it was saved, which no late call is. One
// past the point is counted among the calls dropped after it as well, by its
// number ([droppedAfterSave]): an over-count, never an under-count.
//
// A LATE CALL IS TOLD APART, under words of its own whatever the session's end
// ([prompts.AfterSave.Late]): it finished only after the run that made it was
// replaced, so the session may show it with no outcome or an interrupted one,
// and every continuation of the round tells it again, an earlier brief among
// them — so neither the words for calls the session lacks nor those for a
// session cut off are true of it. SO IS A CALL NOBODY WAITED FOR, under the
// same words, which hold of it: it finished only after its harness gave up on
// it or its session ended, so the session shows it with no outcome or a failed
// one, and every continuation tells it again. And a successful `submit_work`
// among them is told as no submission of the round
// ([prompts.AfterSave.LateSubmission]): the round's is the one a run carrying
// it on makes — a late one the collection never takes ([sinceAnswer]), and
// one nobody waited for its own run was told failed, which the run's own,
// replayed after it, stands over.
//
// NEVER THE CALL A PERSON APPROVED THAT THE BRIEF'S LEAD TELLS. leadHeld is the
// held call the continuation leading the brief tells what became of
// ([carried.tellsHeld]), "" where it tells none, and the call made for it
// ([BridgedCall.HeldID]) is in neither list. A later attempt at an answer, the
// first's relaunch refused after it made the call, reads that call in the
// round's log, placed after every call the log had taken (internal/sandbox's
// withApproved), so past the save point of every capture taken before it was
// made — one placed after it holds it, and lists it nowhere: listed beside the
// lead that tells what it returned, it was told twice, the second time as a
// call of its own. A call an earlier answer of the round approved is told as
// any call is, since no lead of this relaunch tells it.
//
// THE LISTS AS THE PRIOR-WORK LEDGER RENDERS A ROUND, AND HELD TO ITS BUDGETS
// ([ledger.Format]), fitted together: the meta-tools it skips left out, an
// argument value or a failed call's error past its budget rewritten by the
// seat's auxiliary model first, as [Runner.priorWork] fits the ledger, and the
// reads past the ledger's cap counted rather than listed — read as the
// surface's positively read-only tools, the delivery gate's reading. Verbatim,
// a session saved early in a long round was handed every call after it whole,
// each up to the 16 KiB a call's record keeps. And defused under b: its
// arguments are a model's, and a failed call's error is whatever the tool
// said.
//
// ONE READ CAP FOR BOTH LISTS ([ledger.FormatCallGroups]), never one each: they
// are one round's calls, told in one brief, and capped apart they would be
// told twice the reads the cap allows a round. It is spent in the order the
// brief tells them, the calls after the save first: where the session's harness
// ended its own run they are calls it lacks outright, and where it was or may
// have been cut off they include calls it may lack, an earlier job's among
// them, while a late call is one the session may already show. On every path a
// read's line carries nothing the read returned, so a read listed and a read
// counted are alike one the session may run again. The fit is over the two
// lists together, which names exactly what the one cap shows.
func (r *Runner) calledAfter(ctx context.Context, round roundCalls, through *int, leadHeld string,
	surface *tools.Surface, b provenance.Boundary) prompts.AfterSave {

	var (
		after, late []ledger.Call
		out         prompts.AfterSave
	)
	if through != nil {
		out.Dropped = droppedAfterSave(round, *through)
	}
	out.LateDropped = min(round.lateElided, ledger.Count(round.elided))
	for _, call := range inAdmissionOrder(round.bridged) {
		if heldAt(call, through) || slices.Contains(r.cfg.SkipNames, call.Name) {
			continue
		}
		if leadHeld != "" && call.Approved && call.HeldID == leadHeld {
			continue
		}
		if call.Late || call.Abandoned {
			late = append(late, call.Call)
			out.LateSubmission = out.LateSubmission || (call.Name == SubmitWorkTool && !call.Failed)
			continue
		}
		after = append(after, call.Call)
	}
	// AND EVERY CALL WHOSE OUTCOME IS UNKNOWN ([Resume.Unfinished]), under the
	// late calls' words, whatever the point: the log held it in flight when the
	// round was read, so no save point holds what it did, and the session that
	// made it shows it with no outcome or as still running — which those words
	// say — while its effects may stand. Never a submission: it is failed.
	for _, call := range inAdmissionOrder(round.unfinished) {
		if !slices.Contains(r.cfg.SkipNames, call.Name) {
			late = append(late, call.Call)
		}
	}
	if len(after)+len(late) > 0 {
		opts := ledger.Format(r.cfg.SkipNames, surface.Universe().KnownReads())
		opts.Fitted = ledgerfit.Fit(ctx, r.cfg.Compact, ledger.CallPieces(slices.Concat(after, late), opts))
		told := ledger.FormatCallGroups([][]ledger.Call{after, late}, opts)
		if len(after) > 0 {
			out.Calls = provenance.Neutralise(b, told[0])
		}
		if len(late) > 0 {
			out.Late = provenance.Neutralise(b, told[1])
		}
	}
	if out.Dropped > 0 || out.LateDropped > 0 {
		out.Tally = provenance.Neutralise(b, ledger.FormatElided(round.elided, r.cfg.SkipNames))
	}
	return out
}

// calledInPlace is what the job's own copy of its session, in its box, may lack
// of round where its harness is not known to have ended its own run
// ([Resume.HarnessEnd]): EVERY CALL OF THE ROUND, and every call its log
// dropped from its middle counted, told as calls the session may already show
// ([prompts.AfterSave.Interrupted]) — what a capture cut off is told on a fleet
// of this version, its save point the round's start (internal/sandbox's
// savepoint.go), for a copy no capture describes.
//
// NEVER ONLY THE JOB'S OWN CALLS. The copy is not known to hold the job's
// brief, which told the round's calls before the job's own — an earlier job's,
// or the call an earlier answer approved, made before the job opened: no
// probe has measured whether Claude Code 2.1.310 writes the message a
// continued session is handed before it answers a call, and a copy whose job
// continued a session and lacks it lacks those calls whatever the job made
// itself. (2.1.310 was measured answering a fresh session's first two calls
// before its transcript file existed, but a fresh session cut off then leaves
// its box nothing to continue: the relaunch starts a session of its own.)
//
// BUT THE CALL A PERSON APPROVED FOR leadHeld, the held call the answer
// carrying the round on tells what became of, which that lead alone tells
// ([Runner.calledAfter]).
func (r *Runner) calledInPlace(ctx context.Context, round roundCalls, leadHeld string,
	surface *tools.Surface, b provenance.Boundary) prompts.AfterSave {

	start := 0
	called := r.calledAfter(ctx, round, &start, leadHeld, surface, b)
	called.Interrupted = true
	return called
}

// heldAt reports whether a session saved at through holds call: one the round's
// log took at or before the read that placed the save ([BridgedCall.Appended]
// at most through), whatever its place in the order the bridge admitted the
// calls — a call in flight at the read is taken once its tool answers, after
// it. A NIL through is a job's own copy of its session in its box whose harness
// ended its own run, which holds every call its job made and every call its
// job's brief told: every call of the round but a late one or one nobody waited
// for, where that brief told the round's calls (a fresh run's brief tells
// none) — what the copy's
// telling reads a nil point as ([Runner.calledAfter]) — of which its
// guard counts the job's own, past [execstate.State.BridgeFrom] ([ownCalls]).
// THE ONE TEST of what a session holds: a continuation is told every call it
// does not ([Runner.calledAfter]), and its guard counts the loads of the calls
// it does ([ownCalls]).
//
// A call the log took with no number, an older build's append in a fleet
// mid-upgrade, is held at no save point a capture states: told again, never not
// at all. The job's own copy in its box, where its harness ended its own run,
// holds it as it holds every call but a late one, and is not told it. NOR IS A
// LATE ONE HELD, whatever its number ([BridgedCall.Late]): the log took it
// after its job was replaced, so after the brief of the job carrying the round
// on was built, which never told it; and the session of the job that made it
// may not hold its outcome, its harness having died, given up on the call or
// been replaced before the call returned. Yet a save point is a prefix of the
// log, so a later capture placed by its own read counts it among the calls its
// session holds, and every continuation after would never be told it. Told
// whatever the point instead — again by every later continuation of the round,
// under words that say it may be shown or told already and is one call
// ([Runner.calledAfter]) — and a skill it loaded is loaded again before a call
// the skill covers, the safe direction. One its session owed after a failed
// append and carried only once its job was replaced is told again although
// that job's session holds its answer, and its load is not counted although
// that session holds the skill: the same safe direction. So no session a guard
// is rebuilt for is taken to hold a late call, a job's own copy in its box
// among them.
//
// NOR ONE NOBODY WAITED FOR ([BridgedCall.Abandoned]), whatever its number: its
// harness gave up on it, or its session ended, before it returned, so the
// session of the job that made it shows it with no outcome or a failed one —
// its harness told it timed out — while it ran on and its effect happened. A
// harness that gave up on a call can still end its own run, since it waits for
// no answer it gave up on, so the call can land before the read a capture of
// that session is placed by, and before the read of a relaunch whose briefs
// count it among the calls the job's own copy in its box holds: held there, it
// was told to no continuation, and a run could make it again. Told whatever the
// point instead, as a late call is, and a skill it loaded is loaded again,
// since its body never reached the session.
func heldAt(call BridgedCall, through *int) bool {
	switch {
	case call.Late || call.Abandoned:
		return false
	case through == nil:
		return true
	}
	return call.Appended != 0 && call.Appended <= *through
}

// droppedAfterSave is how many of the calls round's log took after the save
// point through it dropped from its middle, which no list can tell a session
// one by one: the log's appends past the point, less the calls it still lists
// numbered past it. NOT THE LATE ONES APART: a dropped late call keeps no mark
// to be told by, so every continuation is told how many the log dropped
// ([roundCalls.lateElided]) under the late calls' own words, whatever its point
// ([Runner.calledAfter]) — and one past the point is counted here as well.
//
// EXACT, because every append is numbered one past the last
// ([BridgedCall.Appended]) and each call the log took is listed or counted
// without listing, never both — so the round's appends are the two together,
// and the numbers past the point the log does not list are the dropped ones.
// A listed call with no number is not subtracted, its place unknown, so a log
// an older build appended to may over-count, held to the dropped calls the
// round counts at all; it never under-counts. So may a dropped late call past
// the point, counted here and again under the late calls' words: the same
// over-count, under the same hold.
func droppedAfterSave(round roundCalls, through int) int {
	dropped := ledger.Count(round.elided)
	pastPoint := len(round.bridged) + dropped - through
	for _, call := range round.bridged {
		if call.Appended > through {
			pastPoint--
		}
	}
	return min(max(pastPoint, 0), dropped)
}

// readsSince is every note the turn has read that a session saved with cursor
// as its last read has not, in the order read: a read the record placed after
// cursor, and every read of this segment — a review's, the one that sent the
// round back among them — which the record has placed after any session its
// launch continues was saved, and which reaches here before the record numbers
// it (Order zero). Zero is every note the turn has read.
func (r *Runner) readsSince(cursor int) []steer.Read {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []steer.Read
	for _, read := range r.steered {
		if read.Order == 0 || read.Order > cursor {
			out = append(out, read)
		}
	}
	return out
}

// toldNotes is what a continuation tells a session of the notes a person sent
// the turn: reads, the notes the turn has read that the session may lack, in
// the order read, then fresh, the ones the brief is the first to read — under
// the header of notes it may already show ([prompts.NotesReadAgain]) wherever
// the session may hold one of them, and the rest after it, plainly.
//
// WHICH IT MAY HOLD: every one of reads, where unsure says so — a session cut
// off, or one whose notes stop before a read the record marks unanswered — and
// every note the run's bridge served a harness ([Resume.Served]), read since or
// waiting still: a note is recorded served before its harness can deliver it,
// and read only once the harness confirms it did, so a harness cut off between
// the two, or whose confirmation the store never took, leaves a session that
// may show a note the record says it had not read. A fresh run holds none, and
// is told every note as it always is.
//
// IN THE ORDER READ, WHATEVER THE HEADER: a later note is the one that wins, so
// the header takes every read and every fresh note up to the last served one,
// and only the fresh notes after that follow it plainly — each one the turn had
// not read before this brief, which is what the header says of them. A note of
// fresh its record holds read already — a lost take's, which this brief hands
// over — is one of reads, at the place the record read it
// ([Runner.inReadOrder]).
func (r *Runner) toldNotes(reads []steer.Read, unsure bool, fresh []steer.Note) []string {
	reads, fresh = r.inReadOrder(reads, fresh)
	var served []string
	if r.cfg.Resume != nil {
		served = r.cfg.Resume.Served
	}
	upTo := 0
	for i, n := range fresh {
		if slices.Contains(served, n.ID) {
			upTo = i + 1
		}
	}
	for _, read := range reads {
		unsure = unsure || slices.Contains(served, read.ID)
	}
	told := make([]string, 0, len(reads)+len(fresh))
	for _, read := range reads {
		told = append(told, prompts.SteerMessage(read.By, read.Text))
	}
	if !unsure && upTo == 0 {
		return append(told, renderedNotes(fresh)...)
	}
	told = append(told, renderedNotes(fresh[:upTo])...)
	var out []string
	if again := prompts.NotesReadAgain(told); again != "" {
		out = append(out, again)
	}
	return append(out, renderedNotes(fresh[upTo:])...)
}

// inReadOrder is reads together with every note of fresh the run's record holds
// read already — a take recorded it and its answer was lost, and the take's
// settle handed it to this brief ([steer.Briefing.Orders]) — in the order the
// record numbered their reads, and the rest of fresh, the notes the brief is
// the first to read, in their own order.
//
// BY THE RECORD'S PLACES, never by when the runner came to hold a note: a read
// this segment made reaches the runner before the record has numbered it (Order
// zero), and the source that recorded it says where it is ([Runner.placed]);
// and a lost take's note is handed over only once its settle is answered — at a
// later round, after a later take's notes, or at a brief — while the record
// numbered it where the take was made. Told in the order the runner came to hold
// them, a person's later note, correcting it, was told ahead of the note it
// corrects. A read the source has not placed — the first segment's box keeps no
// record — keeps its turn, after every placed one.
func (r *Runner) inReadOrder(reads []steer.Read, fresh []steer.Note) ([]steer.Read, []steer.Note) {
	r.mu.Lock()
	placed := r.placed
	r.mu.Unlock()
	if len(placed) == 0 {
		return reads, fresh
	}
	merged := slices.Clone(reads)
	var rest []steer.Note
	for _, n := range fresh {
		if order, ok := placed[n.ID]; ok {
			merged = append(merged, steer.Read{Note: n, Reading: steer.Reading{Order: order}})
			continue
		}
		rest = append(rest, n)
	}
	place := func(read steer.Read) int {
		if read.Order > 0 {
			return read.Order
		}
		if order, ok := placed[read.ID]; ok {
			return order
		}
		return math.MaxInt
	}
	slices.SortStableFunc(merged, func(a, b steer.Read) int { return cmp.Compare(place(a), place(b)) })
	return merged, rest
}

// notesHeldBy is the last read a session saved holding the notes read through
// cursor (sandbox.SessionCapture.NoteCursor) holds as far as the turn's carried
// reads say, and whether it may hold a read after that one too.
//
// STOPPED BEFORE THE FIRST READ MARKED UNANSWERED (steer.Reading.Unanswered)
// that the cursor reaches: its take's answer never reached the harness, so the
// session lacks that note, while it holds the answered reads after it — so
// every note past the stop is told, as notes it may already show. The capture's
// own cursor already stops there wherever the mark landed before the capture
// was stamped (internal/sandbox's notesHeld); read again here, because a mark is
// made off the take's frame and can land after the stamp, and the carried reads
// are the record as this segment found it. A read marked unanswered that the
// cursor neither counts nor stopped right before was made after the session was
// saved, and is told as every read after it is.
func (r *Runner) notesHeldBy(cursor int) (held int, unsure bool) {
	first := r.firstUnanswered()
	if first == 0 || first > cursor+1 {
		return cursor, false
	}
	return first - 1, true
}

// firstUnanswered is the place among the record's reads of the earliest read
// the turn carries that the record marks unanswered (steer.Reading.Unanswered),
// zero for none.
func (r *Runner) firstUnanswered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	first := 0
	for _, read := range r.steered {
		if read.Unanswered && read.Order > 0 && (first == 0 || read.Order < first) {
			first = read.Order
		}
	}
	return first
}

// sendBackLead is what carries a send-back's session on: the review's notes and
// the engine's correction, with the task restated and the rules the earlier
// rounds' calls are read by ([prompts.BuildContinuedRound]).
func (r *Runner) sendBackLead(notes string) string {
	return prompts.BuildContinuedRound(prompts.ContinuedRound{Task: r.cfg.Task, Review: notes}).Text
}

// launchBriefs is what a relaunch's run is handed, and holds, for each way its
// launch may decide it takes up a session ([SessionDecision]).
type launchBriefs struct {
	// fresh is the brief a session of the run's own is handed, and
	// freshReported whether the phase's record already carries it.
	fresh         prompts.Prompt
	freshReported bool

	// kept is the turn's kept session the launch may continue
	// ([AgentRunRequest.Session]), and inPlace the brief the job before's own
	// session is handed in its box ([Continuation.Brief]), "" where the
	// launch may not continue that.
	kept    *SessionContinuation
	inPlace string

	// state is the parked state of the job the relaunch follows and bridged
	// the round's calls: what the guard of the session it continues is
	// rebuilt from. Zero for a launch that follows no job.
	state   execstate.State
	bridged []BridgedCall

	// endedOwnRun says that job's harness ended its own run
	// ([Resume.HarnessEnd]), so its own copy of its session in its box holds
	// every load it made; false arms none of them where that copy is
	// continued.
	endedOwnRun bool
}

// sessionDecided is what a launch tells the runner once it has decided how its
// run takes up a session ([AgentRunRequest.Decided]), before the run starts.
//
// IT ARMS THE RUN'S GUARD with the skills that session holds, and nothing else
// ([Runner.armSession]): the launch state of the job the relaunch follows and
// that job's own loads — through the point the turn's kept capture of it was
// saved at, where the launch restores that, and all of them where the job's own
// copy in its box is continued and its harness ended its own run, none where it
// may have been cut off ([Resume.HarnessEnd]) — because the continued model
// has those bodies in front of it; none for a session of the run's own, whose
// model has read no skill. Never the turn's other jobs' loads: a session
// another job worked in is not this one. AND NONE FOR THE SESSION THE
// CONVERSATION'S LAST TURN LEFT, whatever the parked state holds: the skills
// that session loaded were another turn's, which this turn's guard never armed
// and its lead says were set up anew — the safe direction, a skill-covered call
// refused until it is loaded again rather than admitted on a body nobody read
// this turn. Armed before the run starts, so no call of the run is admitted
// against a guard that holds what its model never read.
//
// AND IT PUBLISHES THE PROMPT THE RUN WAS HANDED as the phase's opening frame
// ([emitter.prompted]) — the continuation in the form its box takes, or the
// fresh brief — where the phase's record does not carry it already: the
// started record carries the fresh brief, which a run that continues its
// session never reads.
//
// What it decided is kept in decided, which the box's word on the session it
// started is read against ([Runner.sessionStarted]), BESIDE THE SKILLS IT ARMED,
// read off the guard as it armed them: the skills the job's parked state records
// as its launch's ([Runner.recordAgentSuspension]).
func (r *Runner) sessionDecided(ctx context.Context, emit emitter, round int, system prompts.Prompt,
	surface *tools.Surface, caps roundCaps, b launchBriefs, decided *launchDecided) func(SessionDecision) {

	return func(d SessionDecision) {
		handed, publish := b.fresh, true
		var (
			loaded []string
			calls  []ledger.Call
		)
		switch {
		case d.Continues && d.Base && b.kept != nil:
			handed = prompts.Prompt{Text: b.kept.Replaced}
		case d.Continues && d.Kept && b.kept != nil:
			loaded, calls = b.state.LoadedSkills, ownCalls(b.bridged, b.state.BridgeFrom, &b.kept.Basis.Through)
			handed = prompts.Prompt{Text: b.kept.Brief}
			if d.NewBox {
				handed = prompts.Prompt{Text: b.kept.Replaced}
			}
		case d.Continues && !d.Kept && b.inPlace != "":
			// THE JOB'S OWN COPY IN ITS BOX holds the skills its launch armed,
			// and its own loads only where its harness ended its own run: one
			// cut off may never have written a load it made, which is told as
			// a call it may lack, and is loaded again before a call it covers.
			loaded = b.state.LoadedSkills
			if b.endedOwnRun {
				calls = ownCalls(b.bridged, b.state.BridgeFrom, nil)
			}
			handed = prompts.Prompt{Text: b.inPlace}
		default:
			publish = !b.freshReported
		}
		r.armSession(surface, loaded, calls)
		decided.set(d, r.loadedSkills())
		if publish {
			emit.prompted(ctx, phase.Execute, round, system, handed, caps)
		}
	}
}

// launchDecided is how a launch decided its run takes up a session, as the
// runner keeps it between the launch's word ([AgentRunRequest.Decided]) and the
// word of the run's box on the session it started ([AgentRunRequest.Started]):
// the first is told before the run starts and the second as its harness first
// reaches the seat's tools, on another goroutine. And the skills the launch
// armed the run's guard with as it decided, which the launch's parked state
// records ([launchDecided.skills]).
type launchDecided struct {
	mu    sync.Mutex
	d     SessionDecision
	armed []string
}

// set keeps d, and armed, the skills the guard holds once armed for it.
func (l *launchDecided) set(d SessionDecision, armed []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.d, l.armed = d, armed
}

// skills is the skills the launch armed the run's guard with as it decided
// ([Runner.sessionDecided]), nil where it has not decided: the guard is empty
// from the relaunch's last step before its launch until the launch decides.
// NEVER THE GUARD AS IT STANDS ONCE THE LAUNCH RETURNS: the run starts inside the
// launch, and a load its harness reported before the launch returned is a call
// of the job's own, made on the same guard.
func (l *launchDecided) skills() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.armed
}

func (l *launchDecided) get() SessionDecision {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d
}

// sessionStarted is what a launch tells the runner once the run's box has said
// which session it started ([AgentRunRequest.Started]): before any call of the
// run is made, by the token its harness dials, which its model cannot forge.
//
// A BOX THAT STARTED THE SESSION ITS LAUNCH CHOSE changes nothing: the guard was
// armed for that session as the launch decided ([Runner.sessionDecided]). One
// that started a session of its own in place of the one the launch continued —
// an archive that did not unpack, an import OpenCode refused — runs a model that
// has read no skill, so its guard is put right before that first call: EMPTY,
// as a fresh launch's is, and never the continued session's, which would let a
// call through on a skill the model never read. And the prompt the run was
// handed was the fresh brief, in the continuation's place, which the phase's
// record then states ([emitter.prompted]).
//
// The parked state the launch records keeps the chosen session's skills, as the
// launch armed them; every later rebuild of the guard reads, beside it, what the
// box said on the job's record ([Resume.Continued]), and takes the skills only
// where it says the session was continued.
//
// THE GUARD ON THE PATH OF THE REQUEST THAT SAID IT, AND THE FRAME OFF IT. The
// guard is put right before that request is served, as every request of the
// run waits on, so no call is admitted against the guard of a session the run
// is not in. The frame is a publish the broker answers — its acknowledgement
// waited for, its listeners run inline — and that request is the run's first to
// its bridge, its harness's MCP handshake as likely as not, which a harness
// holds to a short connect timeout: a broker having a slow moment would drop
// the run's tools from its harness for the sake of a record. So it goes out on
// its own, within [startedFrameBudget], as the job's record of the same word
// is written by the engine's bridge ledger.
func (r *Runner) sessionStarted(ctx context.Context, emit emitter, round int, system prompts.Prompt,
	surface *tools.Surface, caps roundCaps, b launchBriefs, decided *launchDecided) func(bool) {

	// THE TURN'S VALUES AND NEVER ITS CANCELLATION: the box speaks once the
	// launching segment has parked, whose context ends with it, and the frame
	// is owed to the round's record all the same.
	base := context.WithoutCancel(ctx)
	return func(continued bool) {
		if d := decided.get(); !d.Continues || continued {
			return
		}
		r.armSession(surface, nil, nil)
		// THE RUNNER'S OWN GOROUTINE ([Runner.Sent]), which its budget ends.
		r.sent.Go(func() {
			ctx, cancel := context.WithTimeout(base, startedFrameBudget)
			defer cancel()
			emit.prompted(ctx, phase.Execute, round, system, b.fresh, caps)
		})
	}
}

// startedFrameBudget bounds the publish of the frame stating the fresh brief a
// run was handed where its box started a session of its own
// ([Runner.sessionStarted]): FIVE SECONDS, the publisher's own wait for a
// broker's acknowledgement, which a healthy broker gives in milliseconds — a
// publish not answered in that is one an outage holds, which this record does
// not wait out. A frame that missed it leaves the continuation as the round's
// opening frame; the run's collection still says it started a session of its
// own (`harness_session_continued`, `harness_session_fell_back`).
const startedFrameBudget = 5 * time.Second

// armSession re-arms the run's guard with exactly the skills in loaded and those
// calls loaded ([skills.Guard.Replace], [Runner.observeLoads]): the skills the
// session its run takes up holds the bodies of.
func (r *Runner) armSession(surface *tools.Surface, loaded []string, calls []ledger.Call) {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	guard.Replace(loaded)
	r.observeLoads(surface, calls)
}

// ownCalls is the calls of bridged one job made itself that the session it is
// rebuilt for holds, in the order the bridge admitted them: past from, the last
// call the round held when the job's session opened
// ([execstate.State.BridgeFrom]) — a place in that order, which is the right
// one for which job made a call — and held at through ([heldAt]): where it is
// set, the save point of a capture of the job's session
// ([SessionBasis.Through]), none the log took after it, so the calls the
// capture had been answered, whose loads the restored session holds, whatever
// their place; and nil, for the job's own copy of its session in its box where
// its harness ended its own run, every call it made but a late one or one
// nobody waited for. NEVER A LATE CALL, whose place can fall past from — an
// earlier job's call still running when this one opened, numbered by that
// job's session — and whose answer no brief of the round had told when the log
// took it, and the session of the job that made it may not hold; NOR ONE NOBODY
// WAITED FOR, whose answer never reached its session. A call with no place is
// no job's.
func ownCalls(bridged []BridgedCall, from int, through *int) []ledger.Call {
	var out []ledger.Call
	for _, call := range inAdmissionOrder(bridged) {
		if call.Seq <= from || !heldAt(call, through) {
			continue
		}
		out = append(out, call.Call)
	}
	return out
}
