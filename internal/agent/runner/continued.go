package runner

import (
	"context"
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
//     ([prompts.BuildContinuedRound]), the answer to its question, or what cut
//     it off;
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
//     told to every continuation built after it landed: apart, under words of
//     its own whatever the session's end — it may be shown, as interrupted or
//     with no outcome, or told by an earlier brief, since every continuation of
//     the round tells it again, and it is one call — and a submission among
//     them told as none of the round's;
//   - every note a person sent the turn that the session has not read — read
//     after it was saved, by the run that went on in it or by a review, which
//     is never the session's — or, of a session cut off, every note the turn
//     has read, as notes it may already show ([prompts.NotesReadAgain]): its
//     job's last take of them was recorded read before its harness had them,
//     and it is not known to hold its job's brief, which told the notes the
//     session it continued lacked; and then the ones this brief is the first to
//     read.
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
// every call of the round, as a session cut off is told its calls, and every
// note the turn has read, as it is told its notes ([Runner.inPlaceAfter]).
// Where it did, the copy holds every call its job made and every call its
// job's brief told — every call of the round but the ones the log took late,
// where that brief told the round's calls (a fresh run's brief tells none) —
// and is not known to hold a late one ([heldAt]): it is told those, as every
// continuation of the round built after they landed is told them.
//
// THE RUN'S GUARD IS THE SESSION'S IT RUNS IN ([Runner.sessionDecided]): the
// skills the launch state of the job it follows held and that job's own loads —
// those the log took through the point its capture was saved at, by the same
// test the calls after it are told by ([ownCalls], [heldAt]), where the launch
// restores it, and all of them for the job's own copy in its box only where its
// harness ended its own run, and never a late one — because the continued model
// has those bodies in front of it; none for a run that starts a session of its
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
// ([Runner.calledAfter]), then every note it has not read — of a session cut
// off, every note the turn has read, as notes it may already show — and the
// ones fresh holds, in the box it was last worked in, and in another with what
// that changes. The engine's own words and the seat's, every foreign word in
// them fenced or defused under the relaunching segment's token; branch and the
// basis's home are the engine's record of the session, defused too.
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
func (r *Runner) keptSession(ctx context.Context, basis SessionBasis, round roundCalls,
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
	called := r.calledAfter(ctx, round, &basis.Through, surface, b)
	called.Interrupted = basis.Interrupted
	after = append(after, prompts.CalledAfter(called))
	// THEN THE NOTES IT HAS NOT READ, past its cursor; of a session cut off,
	// EVERY NOTE THE TURN HAS READ, as notes it may already show, as the copy a
	// job's own box holds is told them ([Runner.inPlaceAfter]): its job's last
	// take of them was recorded read before its harness had them, and it is not
	// known to hold its job's brief, which told the notes the session it
	// continued lacked, and no record says which those were (internal/sandbox's
	// notesHeld states none it holds).
	if basis.Interrupted {
		after = append(after, prompts.NotesReadAgain(r.carriedSteers()))
	} else {
		after = append(after, r.unreadSince(basis.NoteCursor)...)
	}
	after = append(after, renderedNotes(fresh)...)
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
// their own; and every note the turn has read ([prompts.NotesReadAgain]).
//
// WHERE THE HARNESS ENDED ITS OWN RUN, ONLY THE CALLS THE LOG TOOK LATE
// ([BridgedCall.Late]), which that copy is not known to hold ([heldAt] at a nil
// save point), while it holds every call its job made and every call its job's
// brief told: every other call of the round, where that brief told the round's
// calls (a fresh run's brief tells none). A late call is one the log took after
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
// EVERY NOTE THE TURN HAS READ, never only the job's: a copy that lacks the
// job's brief lacks the notes that brief told — an earlier job's, which a
// continuation re-tells where its session may not hold them — as much as the
// ones the job read, and no record says which those were, so it is told every
// one a fresh brief would be, as notes it may already show, as a kept session
// cut off is ([Runner.keptSession]).
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
	end := r.cfg.Resume.HarnessEnd
	if end.EndedOwnRun() {
		if late := prompts.CalledAfter(r.calledAfter(ctx, round, nil, surface, b)); late != "" {
			return []string{late}
		}
		return nil
	}
	after := []string{prompts.MayBeInterruptedCopy}
	if end == sandbox.HarnessEndCutOff {
		after[0] = prompts.InterruptedCopy
	}
	if called := prompts.CalledAfter(r.calledInPlace(ctx, round, surface, b)); called != "" {
		after = append(after, called)
	}
	if again := prompts.NotesReadAgain(r.carriedSteers()); again != "" {
		after = append(after, again)
	}
	return after
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
// the ones it took late ([BridgedCall.Late]).
//
// A NIL through is the job's own copy of its session in its box whose harness
// ended its own run, as [heldAt] reads it: it holds every call its job made and
// every one its job's brief told — every call of the round but the late ones,
// listed or dropped, where that brief told the round's calls — so it is told
// the late ones alone: the ones listed, and how many the log dropped from its
// middle ([prompts.AfterSave.LateDropped]), with the round's tally, under the
// late calls' own words, since no save placed that copy.
//
// A LATE CALL IS TOLD APART, under words of its own whatever the session's end
// ([prompts.AfterSave.Late]): it finished only after the run that made it was
// replaced, so the session may show it with no outcome or an interrupted one,
// and every continuation of the round tells it again, an earlier brief among
// them — so neither the words for calls the session lacks nor those for a
// session cut off are true of it. And a successful `submit_work` among them is
// told as no submission of the round ([prompts.AfterSave.LateSubmission]):
// the round's is the one a run carrying it on makes ([sinceAnswer]).
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
func (r *Runner) calledAfter(ctx context.Context, round roundCalls, through *int, surface *tools.Surface,
	b provenance.Boundary) prompts.AfterSave {

	var (
		after, late []ledger.Call
		out         prompts.AfterSave
	)
	if through != nil {
		out.Dropped = droppedAfterSave(round, *through)
	} else {
		out.LateDropped = min(round.lateElided, ledger.Count(round.elided))
	}
	for _, call := range inAdmissionOrder(round.bridged) {
		if heldAt(call, through) || slices.Contains(r.cfg.SkipNames, call.Name) {
			continue
		}
		if call.Late {
			late = append(late, call.Call)
			out.LateSubmission = out.LateSubmission || (call.Name == SubmitWorkTool && !call.Failed)
			continue
		}
		after = append(after, call.Call)
	}
	if len(after)+len(late) > 0 {
		opts := ledger.Format(r.cfg.SkipNames, surface.Universe().KnownReads())
		pieces := ledger.CallPieces(after, opts)
		for _, p := range ledger.CallPieces(late, opts) {
			if !slices.Contains(pieces, p) {
				pieces = append(pieces, p)
			}
		}
		opts.Fitted = ledgerfit.Fit(ctx, r.cfg.Compact, pieces)
		if len(after) > 0 {
			out.Calls = provenance.Neutralise(b, ledger.FormatCalls(after, opts))
		}
		if len(late) > 0 {
			out.Late = provenance.Neutralise(b, ledger.FormatCalls(late, opts))
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
// or the call a person approved, made before the job opened: no probe has
// measured whether Claude Code 2.1.310 writes the message a continued session
// is handed before it answers a call, and a copy whose job continued a session
// and lacks it lacks those calls whatever the job made itself. (2.1.310 was
// measured answering a fresh session's first two calls before its transcript
// file existed, but a fresh session cut off then leaves its box nothing to
// continue: the relaunch starts a session of its own.)
func (r *Runner) calledInPlace(ctx context.Context, round roundCalls, surface *tools.Surface,
	b provenance.Boundary) prompts.AfterSave {

	start := 0
	called := r.calledAfter(ctx, round, &start, surface, b)
	called.Interrupted = true
	return called
}

// heldAt reports whether a session saved at through holds call: one the round's
// log took at or before the read that placed the save ([BridgedCall.Appended]
// at most through), whatever its place in the order the bridge admitted the
// calls — a call in flight at the read is taken once its tool answers, after
// it. A NIL through is a job's own copy of its session in its box whose harness
// ended its own run, which holds every call its job made and every call its
// job's brief told: every call of the round but a late one, where that brief
// told the round's calls (a fresh run's brief tells none) — what the copy's
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
func heldAt(call BridgedCall, through *int) bool {
	switch {
	case call.Late:
		return false
	case through == nil:
		return true
	}
	return call.Appended != 0 && call.Appended <= *through
}

// droppedAfterSave is how many of the calls round's log took after the save
// point through it dropped from its middle, which no list can tell a session
// one by one: the log's appends past the point, less the calls it still lists
// numbered past it — and every LATE call it dropped, whatever its number
// ([roundCalls.lateElided]): no save point holds a late call, and a dropped
// one keeps no mark to be told by.
//
// EXACT, because every append is numbered one past the last
// ([BridgedCall.Appended]) and each call the log took is listed or counted
// without listing, never both — so the round's appends are the two together,
// and the numbers past the point the log does not list are the dropped ones.
// A listed call with no number is not subtracted, its place unknown, so a log
// an older build appended to may over-count, held to the dropped calls the
// round counts at all; it never under-counts. So may a dropped late call past
// the point, counted among those past it and again as late: the same
// over-count, under the same hold.
func droppedAfterSave(round roundCalls, through int) int {
	dropped := ledger.Count(round.elided)
	pastPoint := len(round.bridged) + dropped - through
	for _, call := range round.bridged {
		if call.Appended > through {
			pastPoint--
		}
	}
	return min(max(pastPoint, 0)+round.lateElided, dropped)
}

// unreadSince is every note the turn has read that a session saved with cursor
// as its last read has not, each rendered as the model reads it: a read the
// record placed after cursor, and every read of this segment — a review's, the
// one that sent the round back among them — which the record has placed after
// any session its launch continues was saved, and which reaches here before the
// record numbers it (Order zero).
func (r *Runner) unreadSince(cursor int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, read := range r.steered {
		if read.Order == 0 || read.Order > cursor {
			out = append(out, prompts.SteerMessage(read.By, read.Text))
		}
	}
	return out
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
// its harness ended its own run, every call it made but a late one. NEVER A
// LATE CALL, whose place can fall past from — an earlier job's call still
// running when this one opened, numbered by that job's session — and whose
// answer no brief of the round had told when the log took it, and the session
// of the job that made it may not hold. A call with no place is no job's.
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
