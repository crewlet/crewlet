package runner

import (
	"cmp"
	"context"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// A RELAUNCH THAT CONTINUES ITS TURN'S SESSION (a send-back, a person's answer,
// a loss, a replaced sign-in) continues the session of the job it follows where
// its launcher says it can ([SessionProber]), from that job's kept capture in
// whatever box the relaunch gets (internal/sandbox, restore.go). Its brief is
// the session's NEXT MESSAGE, built from what it lacks: what carries it on
// ([prompts.BuildContinuedRound], [carried.tellsHeld], [prompts.SavedSession]);
// its job's leads where it may lack that brief ([Runner.leadsAgain]); every call
// after the save point or in flight, within the ledger's budgets
// ([prompts.CalledAfter]); late, abandoned and unfinished calls apart
// ([ledger.Call.Late], [BridgedCall.Abandoned], [Resume.Unfinished]); that a held
// submission is none of the round's ([prompts.AfterSave.HeldSubmission]); the
// notes it has not read ([Runner.toldNotes]).

// IN TWO FORMS, because the box is the launch's to learn: for the box the
// session was last worked in, and for any other, adding what a new box changes
// ([prompts.BoxReplaced]). The fresh brief rides beside them for a run whose
// box cannot take the session up. A round carried on in the JOB'S OWN BOX
// (no kept capture, [AgentRunRequest.Continue]) continues the copy that box
// holds: where its harness is not known to have ended its run
// ([Resume.HarnessEnd]) it may lack calls and its job's brief
// ([Runner.copyMayLackBrief]) and is told it was cut off with the leads, calls
// and notes ([Runner.inPlaceAfter], [Runner.inPlaceNotes]); where it did, it is
// told only what it cannot hold (late, abandoned, unfinished, owed reads).

// A SESSION HOLDS WHAT ITS BRIEFS TOLD, NEVER WHAT THEIR BUDGETS LEFT OUT:
// reads past the ledger's cap are counted, not listed ([ledger.ShownCallGroups]),
// so each brief answers the reads it left out ([Runner.calledAfter]), its launch
// records them (sandbox.LaunchRecord.BriefOmitted), and the next brief to that
// copy or capture (sandbox.SessionCapture.Omitted) tells them first, up to the
// cap. A brief telling the round from its start starts the account afresh; a
// send-back's new round ends it.

// A SESSION ITS HARNESS MAY HAND ITS MODEL AS A SUMMARY IS TOLD AS A CUT-OFF
// ONE (firstrequest.go): Claude Code compacts before a request at a threshold,
// so anything counted as held can be gone. Each path decides one value
// ([sessionHolds]), once its tellings and lead exist, which every telling and
// its guard read: where it may be a summary, every call, lead and note is told
// as one it may show, under words that never say it was cut off where its
// record does not ([prompts.MayCompactSession]), and its guard arms no skill.

// THE RUN'S GUARD IS THE SESSION'S IT RUNS IN ([Runner.sessionDecided]): the
// skills the job's launch state held and its own loads through the capture's
// save point ([ownCalls], [heldAt]); for the job's own copy only where its
// harness ended its run; never a late or abandoned call's (a skill loaded by a
// call its harness gave up on never reached the model); none for a fresh
// session. A cut-off session arms nothing loaded after its save point (loaded
// again before a call it covers, the safe direction). The guard is put right if
// the box cannot take the session up ([Runner.sessionStarted]); the job's
// record keeps that ([Resume.Continued]).

// A CONVERSATION'S NEXT TURN continues the session its last turn left (internal/sandbox,
// restore.go): another turn's session in a box of its own, so its run is told
// where this turn begins and what the new box changes ([prompts.BuildNextTurn]),
// then handed the fresh brief whole ([Runner.baseSession]). ITS GUARD IS EMPTY:
// its skills were another turn's.

// sessionBasis asks the turn's launcher which kept session this relaunch
// continues ([SessionProber]). A launcher that cannot say continues none.
func (r *Runner) sessionBasis(ctx context.Context) SessionBasis {
	prober, ok := r.cfg.Launcher.(SessionProber)
	if !ok {
		return SessionBasis{}
	}
	return prober.ProbeSession(ctx)
}

// keptSession is how a relaunch continues basis, nil where basis names no kept
// session: everything but the lead that carries it on (the caller leads it,
// [keptBrief.led]). In the box it was last worked in and, in another, with what
// that changes. The engine's and the seat's words, every foreign word fenced or
// defused under the relaunching segment's token; branch and the basis's home
// are the engine's record of the session, defused too.

// It tells: the calls of round after the save ([Runner.calledAfter]), except the
// call approved for each of leadHeld ([leadsHeld]), with the reads its job's
// brief counted unlisted ([SessionBasis.Owed]); the closed rounds' calls a told
// call could be taken for ([Runner.closedShown]) and, where holds says they may
// sit in a summary ([sessionHolds.closedSummarized]), that it may show calls no
// list names ([Runner.otherCalls]); where the session may lack its job's brief
// ([SessionBasis.MayLackBrief]) the leads again ([Runner.leadsAgain]); then the
// notes ([Runner.toldNotes]). holds ([sessionHolds]) decides the cut-off word,
// a restart, and whether the harness may compact the session. A reading
// ([heldReading]) only thins the calls.

// BUILT BEFORE ITS LEAD, so before the relaunch asks the seat's budget for the
// last time: telling calls rewrites an over-budget payload with the seat's
// auxiliary model, charged to the same counters, and can fill the window; the
// lead of a round a person's answer carries on says what the approved call
// returned, made only once every check that could refuse the relaunch has
// passed ([Runner.continueAgentRun]). Led after the check, the brief costs
// nothing the check did not count.
func (r *Runner) keptSession(ctx context.Context, basis SessionBasis, round roundCalls, current int,
	leadHeld []toldHeld, history []ledger.Iteration, surface *tools.Surface, fresh []steer.Note,
	branch string, holds sessionHolds, read *heldReading) *keptBrief {

	if !basis.Continues() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	var after []string
	switch {
	case basis.WentOn:
		// Saved while its run still worked: told so, when, and that the run
		// went on after it and may end mid-call. Always cut off, so calls, leads
		// and notes are told as a cut-off session's; told only the cut-off
		// words, its run took the session's end for where its work stopped.
		after = append(after, prompts.SavedSession(basis.CapturedAt))
	case holds.cutOff:
		after = append(after, prompts.InterruptedSession)
	case holds.summarized():
		// May be a summary: told why, in words that never say it was cut off.
		after = append(after, prompts.MayCompactSession)
	}
	// The leads its job's brief told, where the session may lack that brief;
	// EVERY LEAD THE RECORD KEEPS where it may be a summary, which can have
	// dropped every lead.
	var again toldAgain
	if holds.lacks() {
		again = r.leadsAgain(ctx, history, current, b, holds.summarized())
		if again.section != "" {
			after = append(after, again.section)
		}
	}
	// A cut-off session's calls are told as calls it may already show: its save
	// point is the newest its launch is known to hold, not where it ends
	// (internal/sandbox's savepoint.go). One that may be a summary is told EVERY
	// CALL FROM THE ROUND'S START (a summary holds earlier calls in its own words
	// alone). Never the call a person approved that a lead tells (leadHeld, and
	// held leads told again), nor a closed round a re-told task renders as work
	// done. The reads its job's brief counted without listing (basis.Owed) are
	// held only as a count. This is the one place these calls are told as a
	// possibly-lacking session's ([sessionHolds.keptCalls]), beside the reading
	// of what it holds (read, [Runner.heldReading]): the telling tells none the
	// reading holds.
	from, owed, mayShow := holds.keptCalls(basis)
	as := toldAs{mayShow: mayShow, leads: withHeld(leadHeld, again), rendered: history[:again.rendered],
		summed: holds.closedSummarized(), read: read}
	// A late call its session took, by the capture's cursor ([heldByTelling]),
	// where the session lacks nothing it was told; and apart, the calls told to a
	// box whose release the resume was not read in, which credit no telling a
	// hook's step recorded ([UnheldTelling]).
	if !holds.lacks() {
		as.point, as.lateOwed = &basis.NoteCursor, basis.LateOwed
	}
	called, omitted, readHeld := r.calledAfter(ctx, round, from, owed, history, as, surface, b)
	calls := prompts.CalledAfter(called)
	unheldCalls, unheldOmitted := calls, omitted
	if as.point != nil && slices.ContainsFunc(r.tellings(), func(t Telling) bool { return t.Hook }) {
		as.unheld = true
		told, left, _ := r.calledAfter(ctx, round, from, owed, history, as, surface, b)
		unheldCalls, unheldOmitted = prompts.CalledAfter(told), left
	}
	before := slices.Clone(after)
	after = append(after, calls)
	// Then the notes it has not read past its cursor and the ones fresh holds,
	// any a harness was served by the run's bridge told as one it may already
	// show ([Runner.toldNotes]). A cut-off session is told EVERY NOTE THE TURN
	// HAS READ (it may not hold its job's brief, which told notes no record says
	// it holds; internal/sandbox's notesHeld), as [Runner.inPlaceNotes] does. One
	// whose notes stop before a read marked unanswered or restarted is told every
	// note past that ([Runner.notesHeldBy]).
	// And the wakes it may not show past the same cursor ([Runner.toldWakes]).
	var unheld []string
	if holds.lacks() {
		after = append(after, r.toldNotes(r.readsSince(0), true, fresh)...)
		after = append(after, r.toldWakes(0, false)...)
	} else {
		held, unsure := r.notesHeldBy(basis.NoteCursor, false)
		wakes := r.toldWakes(held, false)
		// And where the cursor counts a read at a hook's step or a hook's telling
		// is credited, the telling for a box whose release the resume was not
		// read in, which holds no such read ([UnheldTelling]).
		strict, strictUnsure := r.notesHeldBy(basis.NoteCursor, true)
		if strictWakes := r.toldWakes(strict, true); strict < held || unheldCalls != calls ||
			!slices.Equal(strictWakes, wakes) {
			unheld = append(append(before, unheldCalls), r.toldNotes(r.readsSince(strict), strictUnsure, fresh)...)
			unheld = append(unheld, strictWakes...)
		}
		after = append(after, r.toldNotes(r.readsSince(held), unsure, fresh)...)
		after = append(after, wakes...)
	}
	return &keptBrief{basis: basis, after: after, unheld: unheld, omitted: omitted, unheldOmitted: unheldOmitted,
		told: again.refs, toldUnread: again.unread, readHeld: readHeld,
		newBox: prompts.BoxReplaced(provenance.Neutralise(b, branch), provenance.Neutralise(b, basis.Home),
			r.checkoutName())}
}

// inPlaceAfter is what a relaunch carrying the round on in its job's own box
// tells the session there, between what carries it on and the notes
// ([Continuation.Brief]). Where the harness is not known to have ended its own
// run ([Resume.HarnessEnd]): that the session was cut off
// ([prompts.InterruptedCopy]) or may have been ([prompts.MayBeInterruptedCopy]),
// said whether or not a call is listed; every call of the round
// ([Runner.calledInPlace]), the late ones apart under words of their own; but
// not the call a person approved that the answer's lead tells
// ([carried.tellsHeld]). The notes follow ([Runner.inPlaceNotes]).

// WHERE THE HARNESS ENDED ITS OWN RUN, ONLY the late calls ([ledger.Call.Late]),
// nobody-waited-for ones ([BridgedCall.Abandoned]: shown with no outcome or
// failed though their effect happened) and in-flight ones ([Resume.Unfinished]),
// none of whose outcomes the copy is known to hold ([heldAt] at a nil save
// point), and the reads its brief counted unlisted ([Resume.Owed]), held only as
// a count. Every other call was held ([prompts.CalledBefore]). Late calls are
// told under their own words, a submission among them as none of the round's,
// with how many the log dropped ([Runner.calledAfter]). Owed reads come first,
// up to what the read cap leaves after the late calls, and the reads this brief
// counts unlisted (omitted) are recorded, so the copy owes exactly what its
// newest brief left out.

// FIRST, THAT A SUCCESSFUL SUBMISSION AMONG THE CALLS IT HOLDS DOES NOT STAND
// ([prompts.AfterSave.HeldSubmission]): a job may submit then ask, its harness
// ending its own run, and the cut its answer's opening takes falls past every
// call ([sinceAnswer]). Told nothing, the copy, handed only the answer's lead,
// read its transcript's submission as done and ended without its own.
//
// holds ([sessionHolds]) overrides this for a copy that may be cut off, whose
// job restarted it after its brief, or whose harness may compact it: it is told
// as above, under the record's cut-off words or [prompts.MayCompactSession],
// with every lead its record keeps.

// Nil where nothing is left to tell or carry continues no copy in that box (a
// lost job's, a round lost twice, or a gone box, [carried.boxKept]), so its calls
// are not rewritten or charged for a brief nothing sends.
//
// Where the copy may lack the job's brief ([Runner.copyMayLackBrief]) it lacks
// the leads too, so it is told them again ([Runner.leadsAgain]) after the
// cut-off section and before the calls, as in [Runner.keptSession]; told is
// which, for the job's record to mark.

// BUILT BEFORE ITS LEAD, as [Runner.keptSession] is: call rewrites by the
// seat's auxiliary model are charged to counters the relaunch checks once every
// brief's rewrites are spent. A launch continuing the copy sends all of it, so
// its floor grows with it: beside the copy's context where its newest capture or
// record measured it ([Continuation.ContextTokens]), else the static floor
// (internal/engine's continuedFloor).
//
// omitted is the reads the brief counts without listing ([Runner.calledAfter]),
// recorded on the job it opens for the next brief to that copy.
func (r *Runner) inPlaceAfter(ctx context.Context, carry carried, round roundCalls, current int,
	history []ledger.Iteration, surface *tools.Surface, holds sessionHolds) (after []string, omitted omittedReads,
	told toldAgain) {

	if carry.session() == "" || !carry.boxKept() || !carry.keeps() {
		return nil, omittedReads{}, toldAgain{}
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	end, leadHeld := r.cfg.Resume.HarnessEnd, leadsHeld(carry, current)
	var called prompts.AfterSave
	if !holds.lacks() {
		// And every telling on the row, which the copy holds ([heldByTelling]).
		called, omitted, _ = r.calledAfter(ctx, round, nil, r.cfg.Resume.Owed, history, toldAs{leads: leadHeld,
			summed: holds.closedSummarized(), point: new(math.MaxInt), lateOwed: r.cfg.Resume.LateOwed}, surface, b)
		if calls := prompts.CalledAfter(called); calls != "" {
			after = []string{calls}
		}
		return after, omitted, toldAgain{}
	}
	switch {
	case holds.cutOff && end == sandbox.HarnessEndCutOff:
		after = []string{prompts.InterruptedCopy}
	case holds.cutOff:
		after = []string{prompts.MayBeInterruptedCopy}
	default:
		// Harness ended its run; the copy may be a summary: told why.
		after = []string{prompts.MayCompactSession}
	}
	again := r.leadsAgain(ctx, history, current, b, holds.summarized())
	if again.section != "" {
		after = append(after, again.section)
	}
	called, omitted = r.calledInPlace(ctx, round, leadHeld, history, again, holds, surface, b)
	if calls := prompts.CalledAfter(called); calls != "" {
		after = append(after, calls)
	}
	return after, omitted, again
}

// leadsHeld is every held call carry's lead tells what became of
// ([carried.tellsHeld]), and every call it tells its run parked on
// ([carried.tellsCalled]), which no list in a brief it leads tells
// ([Runner.calledAfter]); round is the round the carry carries on, where its
// calls were made.
//
// BY ITS TOOL, WHATEVER ITS ID: a lead whose held call names no id still tells
// a call of its tool, which a told call of that tool could be taken for
// ([partners.couldBeTakenFor]); an empty id picks out no call to leave out of
// the lists, since only an approved call carries one ([BridgedCall.HeldID]).
func leadsHeld(carry carried, round int) []toldHeld {
	var out []toldHeld
	if id, tool := carry.tellsHeld(); tool != "" {
		out = append(out, toldHeld{id: id, tool: tool, round: round})
	}
	return append(out, parkedOn(carry.tellsCalled(), round)...)
}

// parkedOn is called, the calls a lead tells its run parked on in round
// ([Asked.Called]), as the calls no list in its brief tells: the lead tells
// each, with what it returned, so listed too it would be told twice.
func parkedOn(called []types.CallReturned, round int) []toldHeld {
	var out []toldHeld
	for _, c := range called {
		out = append(out, toldHeld{tool: c.Tool, round: round, seq: c.Seq})
	}
	return out
}

// inPlaceNotes is what a relaunch carrying the round on in its job's own box
// tells the session there of the notes a person sent, after
// [Runner.inPlaceAfter] ([Continuation.Brief]): where the harness is not known
// to have ended its run, every note read as ones it may already show (no record
// says which its brief's notes were); where it did, every note from the first
// read no session of its job holds ([Runner.firstUnheld]); then the notes fresh
// holds, those served by the bridge as ones it may show ([Runner.toldNotes]).
//
// THE NOTES FRESH HOLDS ALONE where carry continues no copy in that box, as
// [Runner.inPlaceAfter] tells nothing there; every note read wherever holds says
// the copy may lack them ([sessionHolds.lacks]).
func (r *Runner) inPlaceNotes(carry carried, fresh []steer.Note, holds sessionHolds) []string {
	switch {
	case carry.session() == "" || !carry.boxKept() || !carry.keeps():
		return renderedNotes(fresh)
	case holds.lacks():
		return append(r.toldNotes(r.readsSince(0), true, fresh), r.toldWakes(0, false)...)
	}
	// And the wakes the copy may not show: each served and never shown read,
	// and each read past the first read no session of its job holds.
	if first := r.firstUnheld(false); first > 0 {
		return append(r.toldNotes(r.readsSince(first-1), true, fresh), r.toldWakes(first-1, false)...)
	}
	return append(r.toldNotes(nil, false, fresh), r.toldWakes(math.MaxInt, false)...)
}

// toldWakes is the section telling a continued session again the wakes it may
// not show ([Resume.Wakes], [prompts.WakesReadAgain]): each served and never
// shown read, each read past cursor or at no known place, and each read where
// no session of its job is known to hold it, before its last restart or,
// where hooks, at a hook's step. Nil where none is.
func (r *Runner) toldWakes(cursor int, hooks bool) []string {
	if r.cfg.Resume == nil {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	var told []string
	for _, w := range r.cfg.Resume.Wakes {
		if !w.Read || w.Place == 0 || w.Place > cursor || w.Restarted || (hooks && w.Hook) {
			told = append(told, prompts.WakeMessage(w.Wake, b, r.cfg.Turn.ConversationKey))
		}
	}
	if section := prompts.WakesReadAgain(told); section != "" {
		return []string{section}
	}
	return nil
}

// wakesRead is every wake a step of the turn's run read ([Resume.Wakes]),
// rendered, for a run that starts a session of its own: the turn worked each,
// so its delivery starts no turn of its own, and a session lacking it would
// lose it. One served and never read is left to a turn of its own, untold.
func (r *Runner) wakesRead() []string {
	if r.cfg.Resume == nil {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	var out []string
	for _, w := range r.cfg.Resume.Wakes {
		if w.Read {
			out = append(out, prompts.WakeMessage(w.Wake, b, r.cfg.Turn.ConversationKey))
		}
	}
	return out
}

// keptBrief is a relaunch's continuation of a kept session but for its lead
// ([Runner.keptSession]): the session, what the brief tells after the lead, the
// reads counted without listing ([Runner.calledAfter]), the leads told again or
// that could not be read ([Runner.leadsAgain]), the calls a reading found the
// session holding (readHeld, told nowhere), and what another box changes.
type keptBrief struct {
	basis SessionBasis
	after []string
	// unheld is after with the calls and notes told as [UnheldTelling] tells
	// them, nil where they tell the same; unheldOmitted what it left out.
	unheld        []string
	omitted       omittedReads
	unheldOmitted omittedReads
	told          []sandbox.LeadRef
	toldUnread    []int
	readHeld      []BridgedCall
	newBox        string
}

// led is k's continuation led by lead (what carries the session on), with own,
// the leads the launch opens with ([ownLeads]): the brief for the box last
// worked in and for any other tell the same calls and leads, so leave out the
// same reads. Nil for a nil k.
func (k *keptBrief) led(lead string, own []sandbox.Lead) *SessionContinuation {
	if k == nil {
		return nil
	}
	joined := func(after []string) string {
		parts := append([]string{strings.TrimSpace(lead)}, after...)
		parts = slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" })
		return strings.Join(parts, "\n\n")
	}
	brief := joined(k.after)
	out := &SessionContinuation{Basis: k.basis, Brief: brief, Replaced: brief + "\n\n" + k.newBox,
		Told: sandbox.BriefTold{Omitted: k.omitted.reads, LateOmitted: k.omitted.late, Own: own, Told: k.told,
			ToldUnread: k.toldUnread}}
	if k.unheld != nil {
		unheld, told := joined(k.unheld), out.Told
		told.Omitted, told.LateOmitted = k.unheldOmitted.reads, k.unheldOmitted.late
		out.Unheld = &UnheldTelling{Brief: unheld, Replaced: unheld + "\n\n" + k.newBox, Told: told}
	}
	return out
}

// heldReading is a reading of what a cut-off kept session holds
// ([HoldingsAsker]), as a telling is told by it ([Runner.calledAfter]): the
// key the session's harness names a call by ([callKey]), the reading, and each
// asked call's outcome as the ask carried it, by that key.
type heldReading struct {
	keying   sandbox.CallKeying
	holdings sandbox.SessionHoldings
	asked    map[string]sandbox.LoggedCall
}

// holds is what read says of call ([sandbox.SessionHoldings.Holds]): whether
// the session holds its logged result, else the subagent whose heading tells it
// ("" for the main list). Neither for a nil read or a call it was not asked
// about, told as with no reading: BY THE ONE KEY THE ASK WAS MADE BY
// ([callKey]), held to one call of the round, so no call is answered by
// another's key.
func (read *heldReading) holds(call BridgedCall) (held bool, under string) {
	if read == nil {
		return false, ""
	}
	key, ok := callKey(read.keying, call)
	logged, asked := read.asked[key]
	if !ok || !asked {
		return false, ""
	}
	return read.holdings.Holds(logged)
}

// thins reports whether read holds some asked call, or tells one under a
// subagent: where it does neither, a telling by it tells what the telling
// with no reading tells, and none is built ([keptTellings]).
func (read *heldReading) thins() bool {
	for _, logged := range read.asked {
		if held, under := read.holdings.Holds(logged); held || under != "" {
			return true
		}
	}
	return false
}

// callKey is the key a reading of a session harness names call by, and whether
// call carries one this build asks by: THE ONE PLACE a call's key and validity
// are decided per keying ([sandbox.CallKeying]), read by the ask and the
// telling's lookup ([Runner.holdingsAsk], [heldReading]). By call id: the
// call's id ([BridgedCall.CallID]), valid where the log keeps it
// (sandbox.ValidCallID). By log place: the call's place in its round's log
// ([BridgedCall.Appended]) in decimal, valid beside an admission place
// ([BridgedCall.Seq]) and never for a call a person's approval held, which the
// reader refuses and whose part answers with the held answer. Any other: none.
func callKey(keying sandbox.CallKeying, call BridgedCall) (string, bool) {
	switch keying {
	case sandbox.CallKeyingByCallID:
		return call.CallID, sandbox.ValidCallID(call.CallID)
	case sandbox.CallKeyingByLogPlace:
		return strconv.Itoa(call.Appended), call.Appended > 0 && call.Seq > 0 && !call.Held
	}
	return "", false
}

// holdingsAsk is what a relaunch asks a reading of what the session harness
// wrote holds of round ([sandbox.HoldingsAsk]), told as from and owed say
// ([sessionHolds.keptCalls]), for release, the session's writer; and each asked
// call by its key ([callKey]). Its calls are every listed call but those a
// person approved (made on the engine host, held by no session), by key,
// outcome, place, tool, digest, a held call marked, and for a reading by log
// place the arguments' key ([argsKey]); its Unlisted the in-flight calls and the
// log's dropped tally, never the late dropped count, which that tally holds. A
// call is asked where its telling would list it and the reader can name it:
// none a skipped meta-tool's, held at the point ([heldAt]), or late or
// abandoned ([heldByTelling]); its key valid and carried by no other call of
// round.
func (r *Runner) holdingsAsk(keying sandbox.CallKeying, round roundCalls, from *int, owed []int,
	release string) (sandbox.HoldingsAsk, map[string]sandbox.LoggedCall) {

	carried := map[string]int{}
	for _, call := range slices.Concat(round.bridged, round.unfinished) {
		if key, ok := callKey(keying, call); ok {
			carried[key]++
		}
	}
	ask, asked := sandbox.HoldingsAsk{Resumer: release}, map[string]sandbox.LoggedCall{}
	for _, call := range inAdmissionOrder(round.bridged) {
		if call.Approved {
			continue
		}
		key, valid := callKey(keying, call)
		logged := sandbox.LoggedCall{ID: key, Failed: call.Failed, Output: call.Result,
			OutputBytes: call.OutputBytes, Appended: call.Appended, Seq: call.Seq, Name: call.Name,
			Digest: call.Digest, Held: call.Held}
		if keying == sandbox.CallKeyingByLogPlace {
			logged.Args = argsKey(call)
		}
		logged.Asked = !slices.Contains(r.cfg.SkipNames, call.Name) && !heldAt(call, from, owed) &&
			!call.Late && !call.Abandoned && valid && carried[key] == 1
		if logged.Asked {
			asked[key] = logged
		}
		ask.Calls = append(ask.Calls, logged)
	}
	for _, call := range round.unfinished {
		ask.Unlisted = append(ask.Unlisted, sandbox.CallIdentity{Name: call.Name, Digest: call.Digest,
			InFlight: true})
	}
	for _, e := range round.elided {
		ask.Unlisted = append(ask.Unlisted, sandbox.CallIdentity{Name: e.Name, Digest: e.Digest,
			OnlyFailed: e.Failed == e.Calls && e.Unfinished == 0})
	}
	return ask, asked
}

// argsKey is call's arguments' key from its logged text
// ([BridgedCall.ArgsText]), Whole only where that text states them: kept whole,
// not withheld, read by the bridge, decoding as toolargs.Key does, and not ""
// unless the call carries a digest ("" is the empty object only for an admitted
// call).
func argsKey(call BridgedCall) sandbox.ArgsKey {
	if call.ArgsBytes != 0 || call.Withheld || call.ArgsUnread || call.ArgsText == "" && call.Digest == "" {
		return sandbox.ArgsKey{}
	}
	sum, err := toolargs.Key([]byte(call.ArgsText))
	if err != nil {
		return sandbox.ArgsKey{}
	}
	return sandbox.ArgsKey{Whole: true, Sum: sum}
}

// heldReading asks the turn's launcher which of round's calls the cut-off kept
// session basis names holds ([HoldingsAsker]), told as holds says
// ([sessionHolds.keptCalls]). Nil where the launcher reads none, no call is
// asked (the reader refuses an empty ask, [Runner.holdingsAsk]), or the reading
// moves no asked call ([heldReading.thins]). The launcher logs what it read and
// why it read nothing, never an error: every failure is no reading, and the
// tellings with none tell every call after the save point. Asked once per path
// ([Runner.keptTellings]).
func (r *Runner) heldReading(ctx context.Context, basis SessionBasis, round roundCalls,
	holds sessionHolds) *heldReading {

	asker, ok := r.cfg.Launcher.(HoldingsAsker)
	if !ok {
		return nil
	}
	from, owed, _ := holds.keptCalls(basis)
	ask, asked := r.holdingsAsk(basis.Traits[basis.Harness].CallKeying, round, from, owed, basis.Release)
	if len(asked) == 0 {
		return nil
	}
	read := &heldReading{keying: basis.Traits[basis.Harness].CallKeying, holdings: asker.SessionHoldings(ctx, basis, ask), asked: asked}
	if !read.thins() {
		return nil
	}
	return read
}

// baseSession is how a launch continues the session its conversation's last
// turn left, nil where basis names none: the lead saying where this turn begins
// and what the new box changes ([prompts.BuildNextTurn]; a schedule's words
// where the conversation is one's, [notify.Scheduled]), and where that turn went
// on past the session or was lost, that it ends where it was kept; then fresh,
// the launch's own brief or reseed, whole ([prompts.CalledBefore]); and told,
// what that brief left out, with the leads it opens with of its own: the task,
// which the session lacks until it reads this brief, and what carries a round on
// (leads.go). Only in a box of its own, so the same-box brief is empty. The
// base's turn and home are the engine's record, defused under the launching
// segment's token.
func (r *Runner) baseSession(basis SessionBasis, fresh string, told sandbox.BriefTold) *SessionContinuation {
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
	return &SessionContinuation{Basis: basis, Replaced: lead + "\n\n" + strings.TrimSpace(fresh), Told: told}
}

// namingKeptSession is a run's fresh brief naming the kept session the probe
// read (KeptTurn) that the run does not continue ([prompts.KeptSession]), only
// where the surface carries the tool that reads it (a sentence sending a run to
// a missing tool cannot be followed); else brief itself. The turn is the
// engine's record, defused under the launching segment's token; why the launch
// continues none is the log's, never the brief's.
func (r *Runner) namingKeptSession(brief prompts.Prompt, basis SessionBasis,
	surface *tools.Surface) prompts.Prompt {

	if basis.KeptTurn == "" || !slices.Contains(surface.Universe().Names(), toolname.ReadSession) {
		return brief
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	return brief.Then(prompts.KeyKeptSession, prompts.KeptSession(provenance.Neutralise(b, basis.KeptTurn),
		basis.KeptTurn == r.cfg.Turn.RunID, notify.Scheduled(r.cfg.Turn.ConversationKey)))
}

// checkoutName is the tool the executor's surface offers to check a Gitlet
// repository out, "" if none: a run continuing in a new box is told it must
// again, since the box holds none ([prompts.BoxReplaced], [prompts.BuildNextTurn]).
func (r *Runner) checkoutName() string {
	if r.cfg.Checkout == nil {
		return ""
	}
	return r.cfg.Checkout.Name()
}

// calledAfter is what a session saved at through (how many of round's calls the
// log had taken at the read the capture was placed by, [SessionBasis.Through])
// is not known to have been answered: every call the log still lists that the
// session does not hold ([heldAt]), in bridge admission order, with how many
// more the log took after that read and dropped from its middle and the round's
// tally of every dropped call ([droppedAfterSave]); and, apart, the late
// ([ledger.Call.Late]), abandoned ([BridgedCall.Abandoned]) and still
// in-flight calls (round.unfinished, [Resume.Unfinished]), with how many late
// ones were dropped ([prompts.AfterSave.LateDropped]), never one in flight.

// A NIL through is the job's own copy in its box whose harness ended its own
// run, as [heldAt] reads it: it holds every call its job made or its brief told
// but the late ones and the reads that brief counted without listing (owed) and
// the outcome of each but an abandoned one, so it is told the late ones no
// telling credits ([heldByTelling]), the in-flight ones and the owed reads,
// with the round's tally, and first that a successful submission among the
// rest does not stand. A through of zero tells every call: a fresh run's
// ([prompts.CalledBefore]) and a cut-off copy's ([Runner.calledInPlace]).

// as is how the brief tells them ([toldAs]). as.mayShow says calls after the
// point are told as ones the session may already show
// ([prompts.AfterSave.Interrupted], set only here): the record says it was or
// may have been cut off, or it may be a summary ([sessionHolds.keptCalls]).
// history is the turn's closed rounds, whose calls the session may show too.
//
// A DROPPED LATE CALL IS COUNTED UNDER THE LATE CALLS' WORDS WHATEVER THE
// POINT: it keeps no mark or number to read a save point against, and counted
// as a call after a save it would read as the session's own run's. One past the
// point is also counted in the calls dropped after it ([droppedAfterSave]): an
// over-count, never an under-count.

// A LATE CALL OR ONE NOBODY WAITED FOR IS TOLD APART under words of its own
// whatever the session's end ([prompts.AfterSave.Late]): it finished only after
// its run was replaced, gave up on or ended, so the session may show it with no
// outcome, interrupted or failed, or an earlier message may have told it, and
// neither the words for calls the session lacks nor for a cut-off session are
// true of it. A successful `submit_work` among them is told as no submission of
// the round ([prompts.AfterSave.LateSubmission]): the round's is the one a run
// carrying it on makes (a late one the collection never takes, [sinceAnswer]).

// A SUCCESSFUL `submit_work` AMONG THE OTHER CALLS IS TOLD AS NONE OF THE
// ROUND'S TOO ([prompts.AfterSave.EarlierSubmission]): a brief tells only calls
// made before the relaunch it opens. The collection takes a submission only
// from calls after the round's last answer ([sinceAnswer]); an asking job's own
// calls fall below the cut its answer's opening takes (internal/sandbox's
// BridgeLog.opened), a lost or refused-sign-in round collects on one rather
// than carrying on ([Resume.carriedRound]), and a send-back's continuation can
// tell the submission its review sent back. Listed as `→ success`, a run
// carrying the round on could take it for done and be rescued as incomplete.

// ONE THE SESSION HOLDS IS SAID NOT TO STAND TOO, FIRST AND WHETHER OR NOT A
// CALL IS LISTED ([prompts.AfterSave.HeldSubmission]): a successful
// `submit_work` the point holds ([heldAt]) is in no list yet was made before the
// relaunch, so a session saved past it, or the copy at a nil point, shows it
// answered "submitted" and a run taking that for done would end without its
// own. Only the round's calls set it: an earlier turn's submission, held by
// the session its conversation's last turn left, is told nothing of. A late or
// abandoned call is never held.

// NEVER THE CALL A PERSON APPROVED THAT THE BRIEF'S LEAD TELLS. as.leads is
// every held call the leads tell what became of ([carried.tellsHeld],
// [leadsHeld], [withHeld]), and the call made for each ([BridgedCall.HeldID])
// is in no list. A later attempt at an answer, the first's relaunch refused
// after it made the call, reads it placed after every call the log had taken
// (internal/sandbox's withApproved), so past the save point of every capture
// before it: one placed after holds it, and listed beside the lead it would be
// told twice. A call an earlier answer approved is told as any call is. NOR A
// CALL A LEAD TELLS ITS RUN PARKED ON ([parkedOn]), by its place, late or in
// flight: nobody waited for it, so it would be listed among the late calls
// beside the lead's account of what it returned.

// THE LISTS ARE RENDERED AS THE PRIOR-WORK LEDGER RENDERS A ROUND, HELD TO ITS
// BUDGETS ([ledger.Format]), fitted together: skipped meta-tools left out, an
// over-budget argument value or failed call's error rewritten first by the
// seat's auxiliary model ([Runner.priorWork]), reads past the cap counted, not
// listed (the surface's positively read-only tools, the delivery gate's
// reading). Verbatim, a session saved early in a long round was handed every
// later call whole, up to the 16 KiB each record keeps. Defused under b:
// arguments are a model's, and a failed call's error is whatever the tool said.

// OWED reads (their places in the round's log, [Resume.Owed],
// [SessionBasis.Owed]) are told apart ([prompts.AfterSave.Owed]): an earlier
// brief counted them without listing, so the session holds them only as a
// count, at no point ([heldAt]); without this a copy or a capture placed by its
// own read would be read as holding every read past the ledger's cap and never
// be told one. How many the log has dropped is counted
// ([prompts.AfterSave.OwedDropped]): an owed place no longer listed keeps
// nothing to tell it by.

// ONE READ CAP FOR EVERY LIST ([ledger.FormatCallGroups]): they are one round's
// calls in one brief, and capped apart would tell several times the reads the
// cap allows. SPENT IN THIS ORDER whatever order they are told in: late, each
// subagent's, after the point, owed, listed again. The session surely lacks the
// outcome of a late or subagent's call, so a read left out there loses an
// outcome where one after the save may be shown; late calls are few, so told
// first they leave most of the cap to the calls after the save. Owed reads are
// held already as a count, so they take what is left (what they cannot take is
// owed again), and listed-again calls the remainder. A read's line carries
// nothing it returned, so a listed and a counted read are alike one the session
// may run again. The fit is over all lists together.

// IT ANSWERS WHICH READS IT LEFT OUT (omitted), by their places in the round's
// log: the reads of the calls after the point, of each subagent and of the owed
// ones that the cap counted without listing, read off the render's own walk
// ([ledger.ShownCallGroups], [leftOut]) never a rule of this package's, so what
// the launch records ([sandbox.BriefTold]) is what the brief counted. Never one
// listed again, nor one with no place; a late call apart ([omittedReads],
// [heldByTelling]).

// A CALL THE SESSION MAY SHOW THAT A TOLD CALL COULD BE TAKEN FOR IS LISTED
// AGAIN ([prompts.AfterSave.Shown]), under words saying the told one is a call
// of its own, made as well (the words over the calls after a point the session
// may show, and over late calls, say a call shown in both is ONE call). Where a
// held call and a told call name one tool with arguments neither line tells
// apart ([readAlike]), a model seeing the one could take the told one for it and
// lose it, or read it as a call that never ran. Reachable wherever a point holds
// calls beside a late one (a capture whose harness ended its own run, placed by
// its own read; a cut-off capture another build stamped; the copy whose harness
// ended its own run), and wherever the turn has closed rounds, on every harness.

// Held side, in order: the closed rounds' calls the brief tells nowhere else
// ([Runner.closedShown]), each ending with [prompts.EarlierRoundNote]; then
// every round call held at the point ([heldAt]) or by the reading (as.read),
// never a skipped meta-tool's, a person-approved call a lead tells, or a call
// the log dropped (no arguments to compare). A round call carries no note: on a
// send-back "an earlier round" is untrue. Told side: every late call, every
// call a reading tells under a subagent, and the calls after the point where
// as.mayShow; never the owed reads (their words say an earlier message counted
// them) nor a list whose words say its calls are not in the session. Each list
// renders only where not empty ("(none)" otherwise), and the calls listed again
// only where the cap leaves them a call, never as a count alone.

// A TOLD CALL ANOTHER CALL OF THE BRIEF COULD BE TAKEN FOR IS MARKED AS A CALL
// OF ITS OWN ([Runner.notes], [prompts.ToldApartNote]): a call the same message
// tells is already in front of the model, so listing it again would tell it
// twice. Partners: a shown call of another told list reading alike it (both
// marked); a held call a lead tells what became of, of its tool, never a held
// call of the lead's own round and tool ([partners.couldBeTakenFor]); and the
// work already done the brief renders (as.rendered), each shown line reading
// alike it and each tally naming its tool or none ([Runner.partnersOf]). Marks
// are computed on every path, a run of its own included. Never on two calls of
// one list, on or against the owed reads, against a call the cap leaves out, or
// on a call listed again.

// A READING OF WHAT A CUT-OFF SESSION HOLDS (as.read, [heldReading]), asked of
// the calls this telling would tell ([Runner.heldReading]), TELLS NONE THE
// READING FINDS HELD: such a call is held as one at the point is (a successful
// `submit_work` among them is said not to stand, it joins the held side of the
// calls listed again) and is answered (readHeld) for the guard
// ([Runner.sessionDecided]). A call in the thread of a subagent whose report the
// session lacks is told under that subagent's heading ([prompts.AfterSave.Subagents]),
// one group per subagent in order of first told call, in words claiming nothing
// of the session. NO CAP OF THEIR OWN, deliberately: their calls are the round's
// listed calls under the one read cap, a heading per subagent (at most 1,024,
// descriptions at most 1 KiB). Late, abandoned, in-flight, skipped and approved
// calls are decided before any reading.

// BESIDE WORDS THAT SAY A TOLD CALL MAY BE SHOWN TWICE, WHAT THE SESSION MAY SHOW
// THAT NO LIST NAMES ([Runner.otherCalls], [prompts.AfterSave.Other]): calls of
// the conversation's earlier turns, of earlier rounds where the session may hold
// them only in a summary's words (as.summed), of this turn that the record no
// longer lists one by one, and of workers under a delegation, each by the tools
// the told calls name, in a section before every list saying none is a listed
// call. The engine holds none as a line to list again, so it says they may be
// shown and that a told call alike one is a call of its own. Told only where the
// late section prints or the calls after the point are told as ones the session
// may show, never on a run of its own: no other words say a call may be shown
// twice, and it has no session above.
func (r *Runner) calledAfter(ctx context.Context, round roundCalls, through *int, owed []int,
	history []ledger.Iteration, as toldAs, surface *tools.Surface, b provenance.Boundary) (prompts.AfterSave,
	omittedReads, []BridgedCall) {

	var (
		held, late, after, owe, readHeld []BridgedCall
		subs                             []subagentCalls
		out                              = prompts.AfterSave{Interrupted: as.mayShow, Read: as.read != nil}
	)
	if through != nil {
		out.Dropped = droppedAfterSave(round, *through)
	}
	out.LateDropped = min(round.lateElided, ledger.Count(round.elided))
	for _, call := range inAdmissionOrder(round.bridged) {
		if slices.Contains(r.cfg.SkipNames, call.Name) {
			continue
		}
		// The approved call a lead tells is told once, by the lead, wherever
		// the point places it: never in a list nor on the held side. The engine
		// made it on the host after the harness ended its run, so the session
		// never shows it, and it reads alike every told call of its tool: on the
		// held side it would be told twice, the second under words saying the
		// session shows it.
		toldByLead := as.toldByLead(call, round.of)
		if heldAt(call, through, owed) {
			if !toldByLead {
				held = append(held, call)
			}
			out.HeldSubmission = out.HeldSubmission || (call.Name == SubmitWorkTool && !call.Failed)
			continue
		}
		if toldByLead {
			continue
		}
		// A late call the session took by a credited telling ([heldByTelling]).
		if heldByTelling(call, as.point, as.lateOwed, as.credited(r.tellings())) {
			held = append(held, call)
			out.HeldSubmission = out.HeldSubmission || (call.Name == SubmitWorkTool && !call.Failed)
			continue
		}
		if call.Late || call.Abandoned {
			late = append(late, call)
			out.LateSubmission = out.LateSubmission || (call.Name == SubmitWorkTool && !call.Failed)
			continue
		}
		// Held by the reading, or told under the subagent whose thread holds
		// its records where the session holds no report of it.
		if in, under := as.read.holds(call); in {
			held, readHeld = append(held, call), append(readHeld, call)
			out.HeldSubmission = out.HeldSubmission || (call.Name == SubmitWorkTool && !call.Failed)
			continue
		} else if under != "" {
			subs = underSubagent(subs, under, call)
			continue
		}
		if heldAt(call, through, nil) {
			// Held at the point and owed: counted by an earlier brief unlisted.
			owe = append(owe, call)
			continue
		}
		after = append(after, call)
		out.EarlierSubmission = out.EarlierSubmission || (call.Name == SubmitWorkTool && !call.Failed)
	}
	// Every call whose outcome is unknown ([Resume.Unfinished]) goes under the
	// late calls' words whatever the point: in flight when the round was read, so
	// no save point holds what it did, and the session may show it with no
	// outcome, as running, or with the answer its harness got where the store
	// refused its update. Its line says the outcome is unknown and it may have
	// taken effect ([UnfinishedNote]), never that it finished or that a failed
	// call did not take effect, so a run checks before repeating it. Never a
	// submission: it is failed.
	for _, call := range inAdmissionOrder(round.unfinished) {
		if !slices.Contains(r.cfg.SkipNames, call.Name) && !as.toldByLead(call, round.of) {
			late = append(late, call)
		}
	}
	out.OwedDropped = owedGone(round, owed)
	var (
		omitted omittedReads
		// told names the told calls the render shows, listed the closed
		// rounds' calls listed again: what [Runner.otherCalls] names and keeps
		// apart from.
		told   []string
		listed []ledger.Call
	)
	underCalls := 0
	for _, s := range subs {
		underCalls += len(s.calls)
	}
	if len(late)+underCalls+len(after)+len(owe) > 0 {
		opts := ledger.Format(r.cfg.SkipNames, surface.Universe().KnownReads())
		// A RUN OF ITS OWN IS TOLD WHAT ITS ROUND'S WRITES RETURNED, which it
		// remembers none of: the key of what one created, the id of a message.
		opts.Answers = as.fresh
		// The order the one cap is spent in: late, each subagent's, after the
		// point, owed, listed again. toldGroups are the first of them.
		toldGroups := [][]BridgedCall{late}
		for _, s := range subs {
			toldGroups = append(toldGroups, s.calls)
		}
		toldGroups = append(toldGroups, after)
		afterAt := len(toldGroups) - 1
		oweAt, againAt := afterAt+1, afterAt+2
		// Listed again: calls the session may show that a told call could be
		// taken for. Told calls whose words leave that open are every late one,
		// every subagent's, and the calls after the point only where told as
		// ones it may show. The closed rounds' first.
		toldCalls := slices.Concat(toldGroups[:afterAt]...)
		if as.mayShow {
			toldCalls = slices.Concat(toldCalls, after)
		}
		again, closedN := listedAgain(r.closedShown(history, round.of, as), held, toldCalls, opts)
		groups := make([][]ledger.Call, 0, againAt+1)
		for _, group := range toldGroups {
			groups = append(groups, bridgedLedger(group))
		}
		groups = append(groups, bridgedLedger(owe), again)
		opts.Fitted = ledgerfit.Fit(ctx, r.cfg.Compact, ledger.CallPieces(slices.Concat(groups...), opts))
		// Notes come after the fit and walk, before the render, by the places
		// shown ([ledger.ShownIndices]): a note lands on its call's line, and no
		// call the cap leaves out is marked or marks another.
		at := ledger.ShownIndices(groups, opts)
		opts.Notes = r.notes(toldGroups, at, againAt, closedN, r.partnersOf(round.of, as), opts)
		// Never a subagent's: its words claim nothing of a call the session may
		// show, so none it tells could be lost to one the session shows.
		told = shownNames(told, late, at[0])
		if as.mayShow {
			told = shownNames(told, after, at[afterAt])
		}
		for _, i := range at[againAt] {
			if i < closedN {
				listed = append(listed, again[i])
			}
		}
		rendered := ledger.FormatCallGroups(groups, opts)
		if len(late) > 0 {
			out.Late = provenance.Neutralise(b, rendered[0])
		}
		for i, s := range subs {
			out.Subagents = append(out.Subagents, prompts.SubagentCalls{Agent: provenance.Neutralise(b, s.agent),
				Name:  subagentName(b, as.read.holdings.Subagents[s.agent].Description),
				Calls: provenance.Neutralise(b, rendered[1+i]),
				Submission: slices.ContainsFunc(s.calls, func(call BridgedCall) bool {
					return call.Name == SubmitWorkTool && !call.Failed
				})})
		}
		if len(after) > 0 {
			out.Calls = provenance.Neutralise(b, rendered[afterAt])
		}
		if len(owe) > 0 {
			out.Owed = provenance.Neutralise(b, rendered[oweAt])
		}
		shown := ledger.ShownCallGroups(groups, opts)
		// Rendered only where the cap left it a call to list: a section listing
		// none would head a count, and nothing is lost (each is a read the
		// session may show).
		if len(shown[againAt]) > 0 {
			out.Shown = provenance.Neutralise(b, rendered[againAt])
		}
		// Owed by the next brief: reads left out of each subagent's group, the
		// calls after the point and the owed ones, which the session lacks; never
		// one listed again. Apart, the late calls left out, which the next brief
		// tells again with no telling credited ([omittedReads.late]).
		for g := 0; g <= oweAt; g++ {
			group := owe
			if g < oweAt {
				group = toldGroups[g]
			}
			for _, call := range leftOut(group, shown[g]) {
				switch {
				case call.Appended == 0:
				case g > 0:
					omitted.reads = append(omitted.reads, call.Appended)
				case call.Late || call.Abandoned:
					omitted.late = append(omitted.late, call.Appended)
				}
			}
		}
	}
	if out.Dropped > 0 || out.LateDropped > 0 || out.OwedDropped > 0 {
		out.Tally = provenance.Neutralise(b, ledger.FormatElided(round.elided, r.cfg.SkipNames))
	}
	// Calls the session may show that no list names go only beside a section
	// whose words say a told call may be shown twice (claims): the late section,
	// even one holding only a count of dropped calls, and the calls after the
	// point told as ones it may show. Never on a run of its own (no session
	// above). Outside the lists' guard, so a dropped-only late section carries it.
	claims := len(late) > 0 || out.LateDropped > 0 || (as.mayShow && (len(after) > 0 || out.Dropped > 0))
	if claims && !as.fresh {
		out.Other = r.otherCalls(round, history, as, out, told, listed, b)
	}
	return out, omitted, readHeld
}

// subagentCalls is the calls a reading tells under one subagent's name
// ([Runner.calledAfter]): its agent id and its calls in bridge admission order.
type subagentCalls struct {
	agent string
	calls []BridgedCall
}

// underSubagent is subs with call told under agent, in a new group after the
// others if it has none, so groups follow each one's first told call.
func underSubagent(subs []subagentCalls, agent string, call BridgedCall) []subagentCalls {
	if i := slices.IndexFunc(subs, func(s subagentCalls) bool { return s.agent == agent }); i >= 0 {
		subs[i].calls = append(subs[i].calls, call)
		return subs
	}
	return append(subs, subagentCalls{agent: agent, calls: []BridgedCall{call}})
}

// subagentName is a subagent's description ([sandbox.SubagentThread.Description])
// as a brief names it: the box's words, whitespace runs collapsed, cut to
// [ledger.ValueLimit] runes as a call's value is, and defused under b so no
// newline or fence token of the turn's reaches the brief.
func subagentName(b provenance.Boundary, description string) string {
	name := strings.Join(strings.Fields(description), " ")
	if runes := []rune(name); len(runes) > ledger.ValueLimit {
		name = string(runes[:ledger.ValueLimit])
	}
	return provenance.Neutralise(b, name)
}

// shownNames is names plus the tool of each shown call of group (places at,
// [ledger.ShownIndices]) not yet held, in order first told.
func shownNames(names []string, group []BridgedCall, at []int) []string {
	for _, i := range at {
		if !slices.Contains(names, group[i].Name) {
			names = append(names, group[i].Name)
		}
	}
	return names
}

// otherCalls is what the session above may show that no list of a brief names
// ([prompts.OtherCalls]), beside a section whose words say a told call may be
// shown twice ([Runner.calledAfter]). told is the tools of the told calls the
// render shows, in order first told, and listed the closed rounds' calls listed
// again that it shows. A part names only told tools, defused under b, and
// speaks of a count naming no tool only where told is not empty: a call of a
// tool no told call names is none a told one could be taken for.

//   - THE CONVERSATION'S EARLIER TURNS ([Resume.Base]): the session may hold
//     their calls, which the engine keeps none of. By words alone.
//   - THE TURN'S EARLIER ROUNDS, IN A SUMMARY'S WORDS (as.summed,
//     [sessionHolds.closedSummarized]): the told tools of every closed call the
//     listed-again section does not show and of every closed tally. A line's
//     value cannot be compared with a summary's words, so a call listed again
//     is no reason to leave the rest unsaid.
//   - THE CALLS THE RECORD NO LONGER LISTS: the told tools of the round's
//     dropped calls where some may be shown ([droppedMayShow]) and, where closed
//     rounds are not summary-only, of every closed tally (no arguments, so any
//     of its calls reads alike a told call of its tool).
//   - WORKERS' CALLS UNDER A DELEGATION (roundCalls.absorbed): a prose report.

// The history entry standing for round.of is the round's own, so it is read
// nowhere here.
func (r *Runner) otherCalls(round roundCalls, history []ledger.Iteration, as toldAs,
	out prompts.AfterSave, told []string, listed []ledger.Call, b provenance.Boundary) prompts.OtherCalls {

	named := func(of []string, unnamed bool) prompts.ToolsNamed {
		var n prompts.ToolsNamed
		for _, tool := range told {
			if slices.Contains(of, tool) {
				n.Tools = append(n.Tools, provenance.Neutralise(b, tool))
			}
		}
		n.Unnamed = unnamed && len(told) > 0
		return n
	}
	var closed, tallied, absorbed, dropped []string
	var tallyUnnamed, absorbedUnnamed, droppedUnnamed bool
	// A closed call listed again is named by nothing else: by tool, it stands
	// for one listed, so a tool is named exactly where more of its closed calls
	// are listed nowhere than listed again.
	listedOf := map[string]int{}
	for _, call := range listed {
		listedOf[call.Name]++
	}
	for _, entry := range history {
		if standsFor(entry, round.of) {
			continue
		}
		for _, call := range entry.Calls {
			if listedOf[call.Name] > 0 {
				listedOf[call.Name]--
				continue
			}
			closed = append(closed, call.Name)
		}
		tallied, tallyUnnamed = talliedNames(tallied, tallyUnnamed, entry.Elided)
		absorbed, absorbedUnnamed = talliedNames(absorbed, absorbedUnnamed, entry.Absorbed)
	}
	absorbed, absorbedUnnamed = talliedNames(absorbed, absorbedUnnamed, round.absorbed)
	if droppedMayShow(round, as, out) {
		dropped, droppedUnnamed = talliedNames(dropped, droppedUnnamed, round.elided)
	}
	other := prompts.OtherCalls{EarlierTurns: r.cfg.Resume != nil && r.cfg.Resume.Base != nil,
		Fire: notify.Scheduled(r.cfg.Turn.ConversationKey), Workers: named(absorbed, absorbedUnnamed)}
	if as.summed {
		other.EarlierRounds = named(slices.Concat(closed, tallied), tallyUnnamed)
		other.Unlisted = named(dropped, droppedUnnamed)
	} else {
		other.Unlisted = named(slices.Concat(dropped, tallied), droppedUnnamed || tallyUnnamed)
	}
	return other
}

// talliedNames is names with the tool of each count of elided that counts a
// call, and unnamed set where one names no tool.
func talliedNames(names []string, unnamed bool, elided []ledger.Elided) ([]string, bool) {
	for _, e := range elided {
		switch {
		case e.Calls <= 0:
		case e.Name == "":
			unnamed = true
		default:
			names = append(names, e.Name)
		}
	}
	return names, unnamed
}

// droppedMayShow reports whether the session may show some of the calls the
// round's log dropped from its middle ([Runner.calledAfter], out being what it
// told): never where it dropped none, else wherever the point is not known to
// hold them apart. True when calls are told as ones the session may already
// show (as.mayShow, past a point it may have passed), a late call was dropped
// (out.LateDropped), or the log dropped more than it dropped past the point
// (out.Dropped) or lists a call with no number (an older build's append, which
// [droppedAfterSave] cannot place and may over-count by): some dropped call may
// be at or below it. A NIL point (the copy holding every call but the late
// ones) is the second kind: calledAfter counts nothing past it, so out.Dropped
// is zero. ONLY WHERE NONE HOLDS are they all past a point the session ends at
// ([droppedAfterSave] exact), and the list's words say its calls are not in it.
func droppedMayShow(round roundCalls, as toldAs, out prompts.AfterSave) bool {
	dropped := ledger.Count(round.elided)
	return dropped > 0 && (as.mayShow || out.LateDropped > 0 || dropped > out.Dropped ||
		slices.ContainsFunc(round.bridged, func(call BridgedCall) bool { return call.Appended == 0 }))
}

// standsFor reports whether closed round entry is round of or a span holding it
// ([doneFor]'s reading).
func standsFor(entry ledger.Iteration, of int) bool {
	return entry.Iteration <= of && of <= max(entry.Iteration, entry.Through)
}

// notes is the words each line of [Runner.calledAfter]'s groups ends with
// ([ledger.FormatOptions.Notes]), at being the places each group's render shows
// ([ledger.ShownIndices]), in the order late, each subagent's, after, owe,
// listed again (the last at againAt):

//   - A SHOWN CALL OF A TOLD GROUP (the first groups: late, each subagent's,
//     after the point) THAT ANOTHER CALL OF THE BRIEF COULD BE TAKEN FOR ends
//     with [prompts.ToldApartNote]: where a shown call of another told group
//     reads alike it ([readAlike], symmetric, so both are marked) or a partner
//     could be taken for it ([partners.couldBeTakenFor]). Never two calls of one
//     group (two lines of one list are two calls), the owed reads (a run may
//     run them again), a call the cap left out, or a call listed again (its
//     section says it already).
//   - THE FIRST closedN LISTED-AGAIN CALLS, the closed rounds' ([listedAgain]),
//     each end with [prompts.EarlierRoundNote]: by place, so a closed read the
//     cap leaves out carries its note nowhere.
func (r *Runner) notes(told [][]BridgedCall, at [][]int, againAt, closedN int, p partners,
	opts ledger.FormatOptions) [][]string {

	notes := make([][]string, len(at))
	alikeElsewhere := func(g int, t BridgedCall) bool {
		for h, other := range told {
			if h != g && slices.ContainsFunc(at[h], func(k int) bool { return readAlike(other[k].Call, t.Call, opts) }) {
				return true
			}
		}
		return false
	}
	for g, group := range told {
		for _, j := range at[g] {
			t := group[j]
			if !p.couldBeTakenFor(t, opts) && !alikeElsewhere(g, t) {
				continue
			}
			if notes[g] == nil {
				notes[g] = make([]string, len(group))
			}
			notes[g][j] = prompts.ToldApartNote
		}
	}
	if closedN > 0 {
		earlier := make([]string, closedN)
		for i := range earlier {
			earlier[i] = prompts.EarlierRoundNote(notify.Scheduled(r.cfg.Turn.ConversationKey))
		}
		notes[againAt] = earlier
	}
	return notes
}

// partners is every call of a brief, besides its told groups, that a told call
// could be taken for: the shown calls and the tallies of the closed rounds it
// renders as work already done, and the held calls its leads tell.
type partners struct {
	// calls is the shown calls of the rendered rounds, each round's as the
	// prior-work ledger draws them ([ledger.RenderIterations]).
	calls []ledger.Call
	// tools is the tools their tallies name, and anyTool says a tally of
	// theirs counts calls of no tool it names.
	tools   []string
	anyTool bool
	// leads is every held call a lead of the brief tells what became of
	// ([toldAs.leads]), and of is the round whose calls the brief tells
	// ([roundCalls.of]), whose held calls a lead of it never marks.
	leads []toldHeld
	of    int
}

// partnersOf is the partners of a brief telling round of's calls as as says:
// the closed rounds as.rendered holds, never the entry standing for of; each
// round's shown calls by the walk [ledger.RenderIterations] draws them under
// its own reads (none of a span's, drawn as a tally alone); every tally's tools
// by name, or as no tool where a count names none; and as.leads.
func (r *Runner) partnersOf(of int, as toldAs) partners {
	p := partners{leads: as.leads, of: of}
	for _, entry := range as.rendered {
		if standsFor(entry, of) {
			continue
		}
		if entry.Through == 0 {
			p.calls = append(p.calls,
				ledger.ShownCallGroups([][]ledger.Call{entry.Calls}, ledger.Format(r.cfg.SkipNames, entry.Reads))[0]...)
		}
		for _, e := range entry.Elided {
			switch {
			case e.Calls <= 0:
			case e.Name == "":
				p.anyTool = true
			default:
				p.tools = append(p.tools, e.Name)
			}
		}
	}
	return p
}

// couldBeTakenFor reports whether a partner of the brief could be taken for t,
// a told call: rules in the order cheapest to ask.

//   - A HELD CALL A LEAD TELLS, OF t's TOOL: the lead tells what became of it or
//     of the call a person approved for it, either a call of the tool whose
//     arguments no line shows, so it reads alike every call of that tool. Never
//     where t is itself a held call of the lead's round (BridgedCall.Held): the
//     lead tells t's own fate, and a held call carries no id to tell it from
//     another held call of its tool (sandbox's BridgeCall.HeldID names only the
//     approved one), so every held call of that round and tool is spared. Marked,
//     the note would be false where nothing was approved (a denial, a
//     withdrawal, a call not made). Another rule can still mark it.
//   - A TALLY OF THE RENDERED ROUNDS naming t's tool or counting calls of no
//     tool it names: its line shows no arguments, so any such call reads alike t.
//   - A SHOWN CALL OF THE RENDERED ROUNDS that reads alike t ([readAlike]).
func (p partners) couldBeTakenFor(t BridgedCall, opts ledger.FormatOptions) bool {
	if slices.ContainsFunc(p.leads, func(l toldHeld) bool {
		return l.tool == t.Name && (l.seq != 0 || !t.Held || l.round != p.of)
	}) {
		return true
	}
	if p.anyTool || slices.Contains(p.tools, t.Name) {
		return true
	}
	return slices.ContainsFunc(p.calls, func(c ledger.Call) bool { return readAlike(c, t.Call, opts) })
}

// listedAgain is the calls a brief lists again ([Runner.calledAfter], under
// words saying the session may show them, [prompts.AfterSave.Shown]): the
// closed rounds' calls the session may show ([Runner.closedShown]), then the
// round's held calls, each that a told call could be taken for ([readAlike]),
// each in its own order; and how many from the first are closed's, each with a
// note of its own.
//
// ONE PASS IS THE WHOLE ANSWER: a call listed again is told as one the session
// may show, never one it may lack, so it is no told call another could be taken
// for; matching against told alone, never against what it lists, is a fixed
// point.
func listedAgain(closed []ledger.Call, held, told []BridgedCall, opts ledger.FormatOptions) ([]ledger.Call,
	int) {

	var out []ledger.Call
	alike := func(c ledger.Call) bool {
		return slices.ContainsFunc(told, func(t BridgedCall) bool { return readAlike(c, t.Call, opts) })
	}
	for _, c := range closed {
		if alike(c) {
			out = append(out, c)
		}
	}
	closedN := len(out)
	for _, h := range held {
		if alike(h.Call) {
			out = append(out, h.Call)
		}
	}
	return out, closedN
}

// closedShown is the calls of history, the turn's closed rounds, that a brief
// may set beside a told call to list again ([listedAgain]): calls the session
// may show, having continued through their rounds, as a fresh brief's line or a
// summary's words, which the brief tells nowhere else. Every call of every
// entry in order, but:

//   - NONE ON A RUN OF ITS OWN (as.fresh): no session above;
//   - none of the entry standing for of ([roundCalls.of]; [doneFor]'s reading):
//     a send-back's closed rounds hold the round last collected, listed twice;
//   - none of the entries rendered as work done (as.rendered, a prefix of
//     history): told twice in one message, which a re-told task never does
//     ([taskAgain]);
//   - never a skipped meta-tool's, as no list tells one;
//   - never a submission ([SubmitWorkTool]): a closed round's was sent back, and
//     under words silent on submissions it could read as standing;
//   - never a withheld call of a round and tool a held call a lead tells is of
//     ([toldHeld]): a closed list keeps no held call's id ([heldIn]), so every
//     withheld call of that round and tool is left out.
func (r *Runner) closedShown(history []ledger.Iteration, of int, as toldAs) []ledger.Call {
	if as.fresh {
		return nil
	}
	var out []ledger.Call
	for i, entry := range history {
		if i < len(as.rendered) || standsFor(entry, of) {
			continue
		}
		for _, call := range entry.Calls {
			switch {
			case slices.Contains(r.cfg.SkipNames, call.Name), call.Name == SubmitWorkTool:
				continue
			case call.Withheld && slices.ContainsFunc(as.leads, func(l toldHeld) bool {
				return l.seq == 0 && l.round == entry.Iteration && l.tool == call.Name
			}):
				continue
			}
			out = append(out, call)
		}
	}
	return out
}

// toldHeld is a call a lead tells what became of: a held call, by the approved
// call's id ([BridgedCall.HeldID]), its tool and its round; or, seq set, a call
// the run parked on ([Asked.Called]), by its place in the round's log
// ([BridgedCall.Seq]), its tool and its round. By id or place the round's lists
// leave out the call ([toldAs.toldByLead]); by round and tool the closed
// rounds' listed-again calls leave out a held call's withheld calls
// ([Runner.closedShown]), a closed list keeping no id.
type toldHeld struct {
	id, tool string
	round    int
	seq      int
}

// toldByLead reports whether a lead of the brief tells call, a call of round
// of, so it is told once, by the lead, never in a list ([Runner.calledAfter]):
// the approved call made for a held call a lead tells, or a call a lead tells
// its run parked on, found in flight ([Resume.Unfinished]) or not.
func (as toldAs) toldByLead(call BridgedCall, of int) bool {
	return slices.ContainsFunc(as.leads, func(l toldHeld) bool {
		if l.seq != 0 {
			return l.seq == call.Seq && l.tool == call.Name && l.round == of
		}
		return call.Approved && call.HeldID != "" && l.id == call.HeldID
	})
}

// toldAs is how a brief tells a round's calls, as [Runner.calledAfter]'s rules
// read it.
type toldAs struct {
	// mayShow says the calls after the point are told as ones the session may
	// already show ([prompts.AfterSave.Interrupted]), and so are told calls a
	// call the session may show could be taken for.
	mayShow bool
	// fresh says the brief is a run's of its own ([prompts.CalledBefore]): no
	// session above, so nothing is listed again of the closed rounds.
	fresh bool
	// leads is every held call a lead of the brief tells what became of.
	leads []toldHeld
	// rendered is the closed rounds the brief tells as work already done, a
	// prefix of the turn's closed rounds: a fresh brief's prior work, or the
	// rounds a task told again renders ([toldAgain.rendered]).
	rendered []ledger.Iteration
	// summed says the session may hold the closed rounds only in a summary's
	// words ([sessionHolds.closedSummarized]): their calls are still listed
	// again where a told call could be taken for one, and the rest are named
	// by tool as calls it may show that no list holds one by one
	// ([Runner.otherCalls]).
	summed bool
	// read is the reading of what a cut-off session holds that the brief is
	// told by ([heldReading]), nil for none: a call it holds is told nowhere,
	// and one it finds in an unreported subagent's thread under that
	// subagent's name.
	read *heldReading
	// point is the note cursor a late call's telling is read against
	// ([heldByTelling]), nil where the session is credited none; lateOwed
	// the late calls its brief counted without listing them, which it is
	// credited no telling of.
	point    *int
	lateOwed []int
	// unheld is the telling for a box whose release the resume was not read
	// in, which credits no telling a hook's step recorded ([UnheldTelling]).
	unheld bool
}

// credited is what of tellings as credits: every one, but a hook's where as is
// the telling for a box whose release the resume was not read in.
func (as toldAs) credited(tellings []Telling) []Telling {
	if !as.unheld {
		return tellings
	}
	return slices.DeleteFunc(slices.Clone(tellings), func(t Telling) bool { return t.Hook })
}

// leftOut is the calls of group a render left out, shown being those it shows
// ([ledger.ShownCallGroups]): a call is shown where it is the next shown call,
// walking both in order.
//
// EXACT BY THE WALK'S CONTRACT, never a guess at what the cap counted: shown
// calls are a subsequence of the group, each the very value given; a write is
// never left out and only the group's LAST reads are, so two equal reads either
// side of the cap are told apart by place. No skipped name is walked:
// [Runner.calledAfter] drops them first.
func leftOut(group []BridgedCall, shown []ledger.Call) []BridgedCall {
	var out []BridgedCall
	next := 0
	for _, call := range group {
		if next < len(shown) && reflect.DeepEqual(call.Call, shown[next]) {
			next++
			continue
		}
		out = append(out, call)
	}
	return out
}

// owedGone is how many of owed (places in round's log of reads an earlier brief
// counted unlisted) the log no longer lists, dropped from its middle since and
// kept only in its tally ([prompts.AfterSave.OwedDropped]). EXACT, never a late
// call too: those are recorded apart ([omittedReads]) and counted in the late
// tally alone ([prompts.AfterSave.LateDropped]).
func owedGone(round roundCalls, owed []int) int {
	gone := 0
	for _, place := range owed {
		if !slices.ContainsFunc(round.bridged, func(call BridgedCall) bool { return call.Appended == place }) {
			gone++
		}
	}
	return gone
}

// calledInPlace is what the job's own copy of its session, in its box, may lack
// of round where its harness is not known to have ended its own run
// ([Resume.HarnessEnd]): EVERY CALL OF THE ROUND, and every dropped call
// counted, told as ones the session may already show
// ([prompts.AfterSave.Interrupted]), as a cut-off capture never raised past the
// round's start is (internal/sandbox's savepoint.go), for a copy no capture
// describes.

// NEVER ONLY THE JOB'S OWN CALLS: the copy is not known to hold the job's brief,
// which told the round's earlier calls (an earlier job's, or the call an earlier
// answer approved). No probe has measured whether Claude Code 2.1.310 writes the
// message a continued session is handed before it answers a call, so a copy
// whose job continued a session and lacks it lacks those calls. (2.1.310 was
// measured answering a fresh session's first two calls before its transcript
// existed, but a fresh session cut off then leaves the box nothing to continue.)

// BUT NOT the call a person approved for each of leadHeld (the answer's lead
// alone tells it, and the held leads told again, [Runner.calledAfter]). The
// closed rounds' calls a told call could be taken for are listed again, except
// those of rounds a re-told task renders as work done ([Runner.closedShown]);
// where holds says they may sit in a summary's words
// ([sessionHolds.closedSummarized]), it may show calls no list names
// ([Runner.otherCalls]).
//
// IT TELLS THE ROUND FROM ITS START, so the account of what its brief left out
// starts afresh: it owes no earlier-counted read, tells every read itself, and
// answers the reads it leaves out ([Runner.calledAfter]).
func (r *Runner) calledInPlace(ctx context.Context, round roundCalls, leadHeld []toldHeld,
	history []ledger.Iteration, again toldAgain, holds sessionHolds, surface *tools.Surface,
	b provenance.Boundary) (prompts.AfterSave, omittedReads) {

	called, omitted, _ := r.calledAfter(ctx, round, new(0), nil, history, toldAs{mayShow: true,
		leads: withHeld(leadHeld, again), rendered: history[:again.rendered], summed: holds.closedSummarized()},
		surface, b)
	return called, omitted
}

// omittedReads is the calls a brief counted without listing
// ([Runner.calledAfter]), by place in the round's log, as the launch records
// them (sandbox.BriefTold): the reads it owes, and APART the late and
// nobody-waited-for calls. Every continuation tells such a call again, so none
// is owed, but the session holds only a count, so it is credited no telling
// ([heldByTelling]). Apart because a call the log drops keeps no mark: counted
// among the owed reads, a late one would be counted twice
// ([prompts.AfterSave.OwedDropped] and the late tally).
type omittedReads struct {
	reads, late []int
}

// tellings is the late calls of the round a job's session is known to hold
// ([Resume.Tellings]), none for a run no resume carries.
func (r *Runner) tellings() []Telling {
	if r.cfg.Resume == nil {
		return nil
	}
	return r.cfg.Resume.Tellings
}

// heldByTelling reports whether a session whose note cursor is point holds a
// late or nobody-waited-for call by a telling of it: the row records that a
// job's own session took the call at a place at or below point
// ([Resume.Tellings]) and its brief did not count it unlisted (owed). Nil point
// credits none: a possibly cut-off or summary session, one told the round from
// its start, a fresh run. THE ONE TEST BY A TELLING, beside [heldAt]'s by the
// save point, sound by the place: a capture's cursor is read before any later
// write, so it is past a telling only where read after the write that recorded
// it, by the session that took the call or a later one whose brief credited or
// told it (internal/sandbox's package doc; ADR-0073).
func heldByTelling(call BridgedCall, point *int, owed []int, tellings []Telling) bool {
	if !call.Late && !call.Abandoned || call.Appended == 0 || point == nil || slices.Contains(owed, call.Appended) {
		return false
	}
	return slices.ContainsFunc(tellings, func(t Telling) bool {
		return t.Appended == call.Appended && t.Place <= *point
	})
}

// heldAt reports whether a session saved at through holds call: one the round's
// log took at or before the read that placed the save ([BridgedCall.Appended]
// at most through), whatever its admission place (a call in flight at the read
// is taken once its tool answers, after it). A NIL through is a job's own copy
// in its box whose harness ended its own run, holding every call but a late or
// abandoned one ([Runner.calledAfter] reads a nil point so), of which its guard
// counts the job's own, past [execstate.State.BridgeFrom] ([ownCalls]). THE ONE
// TEST BY THE SAVE POINT, beside [heldByTelling]'s: a continuation is told every
// call it does not hold, a held successful submission is none of the round's
// ([prompts.AfterSave.HeldSubmission]), and the guard counts held calls' loads.
// A reading of a cut-off session finds it holding more, by call key
// ([heldReading], [Runner.sessionDecided]).

// NOR A READ ITS BRIEF COUNTED WITHOUT LISTING IT (owed), whatever the point: a
// brief past the prior-work ledger's read cap counts the reads rather than
// listing them, so its session holds that the read was made, not what it
// returned. Held, it would be told to no continuation. Never one of the job's
// own: it came before the job's session opened, so its guard's count is
// unchanged.
//
// A call with no number (an older build's append in a mid-upgrade fleet) is
// held at no capture's save point: told again, never not at all. The job's own
// copy whose harness ended its run holds it as every call but a late one.

// NOR IS A LATE ONE HELD, whatever its number ([ledger.Call.Late]): the log
// took it after its job was replaced, after the brief of the job carrying the
// round on was built, which never told it; and the session of the job that made
// it may not hold its outcome (harness died, gave up, or replaced before the
// call returned). Yet a save point is a prefix of the log, so a later capture
// placed by its own read would count it held and no continuation would ever tell
// it. Told whatever the point, but to a session a telling credits it to
// ([heldByTelling]), under words saying it may be shown or told already and is
// one call ([Runner.calledAfter]); a skill it loaded is loaded again before a
// call the skill covers, the safe direction, as for one its session owed after
// a failed append and carried only once its job was replaced. So no session a
// guard is rebuilt for is taken to hold a late call, the job's own copy included.

// NOR ONE NOBODY WAITED FOR ([BridgedCall.Abandoned]), whatever its number: its
// harness gave up on it or its session ended before it returned, so the session
// shows it with no outcome or failed (told it timed out) while it ran on and
// took effect. A harness that gave up can still end its own run, so the call can
// land before a capture's read and before a relaunch's read whose briefs count
// it among the copy's calls: held there, no continuation would be told it and a
// run could make it again. Told whatever the point, as a late call is, and its
// skill load is loaded again since its body never reached the session.
func heldAt(call BridgedCall, through *int, owed []int) bool {
	switch {
	case call.Late || call.Abandoned:
		return false
	case call.Appended != 0 && slices.Contains(owed, call.Appended):
		return false
	case through == nil:
		return true
	}
	return call.Appended != 0 && call.Appended <= *through
}

// droppedAfterSave is how many of the calls round's log took after the save
// point through it dropped from its middle, which no list can tell a session
// one by one: the log's appends past the point, less the calls it still lists
// numbered past it. NOT THE LATE ONES APART: a dropped late call keeps no mark,
// so every continuation is told how many the log dropped
// ([roundCalls.lateElided]) under the late calls' words, whatever the point
// ([Runner.calledAfter]); one past the point is counted here too.

// EXACT, because every append is numbered one past the last
// ([BridgedCall.Appended]) and each call is listed or counted, never both, so
// the numbers past the point the log does not list are the dropped ones. A
// listed call with no number is not subtracted (place unknown), so a log an
// older build appended to may over-count, capped at the dropped calls the round
// counts; it never under-counts. A dropped late call past the point, counted
// here and under the late words, over-counts the same way.
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
// as its last read has not, in order read: a read the record placed after
// cursor, and every read of this segment (a review's, the send-back's among
// them) placed after any continued session was saved, which reaches here before
// the record numbers it (Order zero). Zero is every note read.
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

// toldNotes is what a continuation tells a session of the notes a person sent:
// reads, those the turn has read that the session may lack, in order read, then
// fresh, those the brief is the first to read, under the header of notes it may
// already show ([prompts.NotesReadAgain]) wherever the session may hold one,
// the rest after it plainly.

// WHICH IT MAY HOLD: every read where unsure says so (a cut-off session or one
// whose notes stop before a read no session of its job holds) and every note
// the run's bridge served a harness ([Resume.Served]), read since or waiting: a
// note is recorded served before delivery and read only once the harness
// confirms, so a harness cut off between, or whose confirmation the store never
// took, leaves a session that may show a note the record says was unread. A
// fresh run holds none and is told every note.

// IN THE ORDER READ, WHATEVER THE HEADER: a later note wins, so the header takes
// every read and every fresh note up to the last served one, and only fresh
// notes after that follow plainly. A fresh note whose place its record holds
// already (a lost take's, or one a hook's serve placed) is one of reads, at that
// place ([Runner.inReadOrder]).
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

// inReadOrder is reads with every fresh note whose place the run's record holds
// ([steer.Briefing.Orders]: a lost take's, or a hook's serve's), in the
// record's order, then the rest of fresh, which the launch's write reads after
// all of them, in their own order.
//
// BY THE RECORD'S PLACES, never when the runner came to hold a note, which told
// a person's correcting note ahead of the note it corrects. A read the source
// has not placed (the first segment's box keeps no record) goes after every
// placed one. See the package doc's "The order a continuation tells a person's
// notes in".
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
// cursor (sandbox.SessionCapture.NoteCursor) holds, as far as the carried reads
// say, and whether it may hold a read after it: stopped before the first read
// no session of its job holds ([Runner.firstUnheld]), every note past told as
// one it may already show. Re-read although the capture's cursor already stops
// there (internal/sandbox's notesHeld): a mark is made off the take's frame and
// can land after the stamp. Only where the path holds its brief
// ([sessionHolds.lacks]); one that lacks it is told every note.
func (r *Runner) notesHeldBy(cursor int, hooks bool) (held int, unsure bool) {
	first := r.firstUnheld(hooks)
	if first == 0 || first > cursor+1 {
		return cursor, false
	}
	return first - 1, true
}

// firstUnheld is the place of the earliest read the turn carries that no
// session of its job holds: marked unanswered (steer.Reading.Unanswered, its
// take's answer lost) or taken before the session's last restart
// (steer.Reading.Restarted); and, where hooks, any read at a hook's step, which
// a box whose release the resume was not read in holds none of
// ([UnheldTelling]). Zero for none.
func (r *Runner) firstUnheld(hooks bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	first := 0
	for _, read := range r.steered {
		unheld := read.Unanswered || read.Restarted || (hooks && hookStep(read.Boundary))
		if unheld && read.Order > 0 && (first == 0 || read.Order < first) {
			first = read.Order
		}
	}
	return first
}

// hookStep reports whether boundary is a step a Claude Code hook answers: the
// two a hook's serve reads at, which only its job's session confirms.
func hookStep(boundary types.SteerBoundary) bool {
	return boundary == types.SteerBoundaryToolBatch || boundary == types.SteerBoundaryFinish
}

// sendBackLead is what carries a send-back's session on: the review's notes and
// the engine's correction, with the task restated and the rules earlier rounds'
// calls are read by ([prompts.BuildContinuedRound]). A later brief telling the
// review again builds it anew, in words true of any session
// ([prompts.ContinuedRound.Again], [Runner.leadAgain]): these say the session
// above is the one the review sent back, true only of this brief's.
func (r *Runner) sendBackLead(notes string) prompts.Prompt {
	return prompts.BuildContinuedRound(prompts.ContinuedRound{Task: r.cfg.Task, Review: notes})
}

// launchBriefs is what a relaunch's run is handed, and holds, for each way its
// launch may decide it takes up a session ([SessionDecision]).
type launchBriefs struct {
	// fresh is the brief a session of the run's own is handed, and
	// freshReported whether the phase's record already carries it.
	fresh         prompts.Prompt
	freshReported bool

	// kept is the turn's kept session the launch may continue
	// ([AgentRunRequest.Session]), inPlace the brief the previous job's own
	// session gets in its box ([Continuation.Brief]), "" where the launch may
	// not continue that.
	kept    *SessionContinuation
	inPlace string
	// inPlaceSummarized is that brief told as one that may be a summary
	// ([Continuation.Summarized]), "" for none.
	inPlaceSummarized string

	// state is the parked state of the job the relaunch follows and bridged the
	// round's calls: what the continued session's guard is rebuilt from. Zero
	// for a launch that follows no job.
	state   execstate.State
	bridged []BridgedCall

	// inPlaceHolds and keptHolds are what each path's telling counts its session
	// as holding ([sessionHolds]), the one value its guard reads too: a copy that
	// may lack its job's brief holds none of the job's own loads, a possible
	// summary none at all. owed is the reads the copy holds only as its brief's
	// count ([Resume.Owed]), read as [heldAt] reads them.
	inPlaceHolds, keptHolds sessionHolds
	owed                    []int

	// exact is the value the kept continuation's telling by a reading was decided
	// by ([exactHolds], [SessionContinuation.Exact]), with the calls the reading
	// found held, read in place of keptHolds where that brief is handed. Zero
	// where none rides.
	exact exactHolds

	// unheldHolds is the value the kept continuation's telling for a box whose
	// release the resume was not read in was decided by ([keptTellings.led]),
	// read in place of keptHolds where that brief is handed
	// ([SessionDecision.Unheld]).
	unheldHolds sessionHolds
}

// sessionDecided is what a launch tells the runner once it has decided how its
// run takes up a session ([AgentRunRequest.Decided]), before the run starts.

// IT ARMS THE RUN'S GUARD with the skills that session holds
// ([Runner.armSession]), NOTHING AT ALL where the path's value says it may be a
// summary ([sessionHolds.summarized]): the launch state of the followed job and
// its own loads (through the kept capture's save point; all of them where its own
// copy is continued and its harness ended its run; none where cut off,
// [Resume.HarnessEnd]), because the continued model has those bodies; none for a
// fresh session. Never the turn's other jobs' loads. NONE FOR THE SESSION THE
// CONVERSATION'S LAST TURN LEFT: its skills were another turn's, set up anew, so
// a skill-covered call is refused until loaded again rather than admitted on a
// body nobody read this turn. Armed before the run starts.

// WHERE THE LAUNCH HANDED THE BRIEF TOLD BY A READING ([SessionDecision.Exact]),
// THE VALUE THAT TELLING WAS DECIDED BY ([exactHolds]): beside the save point's
// loads, the job's own loads among the calls its reading found held (past
// BridgeFrom, as [ownCalls] judges), a subagent's included; none where the
// session may be a summary. For a box whose release the resume was not read in
// ([SessionDecision.Unheld]), that telling's value.
//
// IT PUBLISHES THE PROMPT HANDED, of whichever telling in the form the box
// takes, or the fresh brief, as the phase's opening frame ([emitter.prompted])
// where the record does not carry it already (the started record carries the
// fresh brief, which a continuing run never reads).

// What it decided is kept in decided, which the box's word on the session it
// started is read against ([Runner.sessionStarted]), BESIDE THE SKILLS IT ARMED,
// read off the guard: the skills the parked state records as its launch's
// ([Runner.recordAgentSuspension]).
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
			holds, readHeld, brief, replaced := b.keptHolds, []BridgedCall(nil), b.kept.Brief, b.kept.Replaced
			switch {
			case d.Exact && b.kept.Exact != nil:
				holds, readHeld = b.exact.holds, b.exact.held
				brief, replaced = b.kept.Exact.Brief, b.kept.Exact.Replaced
			case d.Summarized && b.kept.Summarized != nil:
				// The box's point did not prove the held brief whole: the
				// summarized telling's, which arms nothing.
				holds.compacts = true
				brief, replaced = b.kept.Summarized.Brief, b.kept.Summarized.Replaced
			case d.Unheld && b.kept.Unheld != nil:
				holds = b.unheldHolds
				brief, replaced = b.kept.Unheld.Brief, b.kept.Unheld.Replaced
			}
			// None where it may be a summary, which holds a skill's body in its
			// own words alone: loading again is the safe direction.
			if !holds.summarized() {
				loaded = b.state.LoadedSkills
				calls = slices.Concat(ownCalls(b.bridged, b.state.BridgeFrom, &b.kept.Basis.Through,
					b.kept.Basis.Owed), ownHeld(readHeld, b.state.BridgeFrom))
			}
			handed = prompts.Prompt{Text: brief}
			if d.NewBox {
				handed = prompts.Prompt{Text: replaced}
			}
		case d.Continues && !d.Kept && b.inPlace != "":
			// The job's own copy in its box holds the skills its launch armed, its
			// own loads only where known to hold its job's brief (a cut-off copy
			// may never have written a load; loaded again before a call it
			// covers), none where it may be a summary.
			holds, brief := b.inPlaceHolds, b.inPlace
			if d.Summarized && b.inPlaceSummarized != "" {
				holds.compacts, brief = true, b.inPlaceSummarized
			}
			if !holds.summarized() {
				loaded = b.state.LoadedSkills
				if !holds.lacksBrief() {
					calls = ownCalls(b.bridged, b.state.BridgeFrom, nil, b.owed)
				}
			}
			handed = prompts.Prompt{Text: brief}
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

// launchDecided is how a launch decided its run takes up a session, kept
// between the launch's word ([AgentRunRequest.Decided], before the run starts)
// and the box's on the session it started ([AgentRunRequest.Started], as its
// harness first reaches the seat's tools, on another goroutine); and the skills
// the guard was armed with, which the parked state records
// ([launchDecided.skills]).
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

// skills is the skills the guard was armed with as the launch decided
// ([Runner.sessionDecided]), nil before it decides (the guard is empty until
// then). NEVER THE GUARD AS IT STANDS ONCE THE LAUNCH RETURNS: the run starts
// inside the launch, and a load its harness reported before it returned is a
// call of the job's own.
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
// run, by the token its harness dials, which its model cannot forge.
//
// A BOX THAT STARTED THE SESSION ITS LAUNCH CHOSE changes nothing. One that
// started a session of its own instead (an archive that did not unpack, an
// import OpenCode refused) runs a model that has read no skill, so its guard is
// put right before the first call: EMPTY, never the continued session's, which
// would admit a call on a skill the model never read. The prompt handed was the
// fresh brief, which the phase's record then states ([emitter.prompted]).

// The parked state the launch records keeps the chosen session's skills; later
// rebuilds of the guard also read what the box said ([Resume.Continued]) and
// take the skills only where it says the session was continued.
//
// THE GUARD ON THE PATH OF THE REQUEST THAT SAID IT, THE FRAME OFF IT. The guard
// is put right before that request is served, so no call is admitted against a
// session the run is not in. The frame is a publish the broker answers
// (acknowledgement waited for, listeners inline), and that request is the run's
// first to its bridge, likely its harness's MCP handshake, held to a short
// connect timeout: a slow broker would drop the run's tools for the sake of a
// record. So it goes out on its own, within [startedFrameBudget], as the job's
// record of the same word is written by the engine's bridge ledger.
func (r *Runner) sessionStarted(ctx context.Context, emit emitter, round int, system prompts.Prompt,
	surface *tools.Surface, caps roundCaps, b launchBriefs, decided *launchDecided) func(bool) {

	// The turn's values, never its cancellation: the box speaks after the
	// launching segment parked, and the frame is owed to the record anyway.
	base := context.WithoutCancel(ctx)
	return func(continued bool) {
		if d := decided.get(); !d.Continues || continued {
			return
		}
		r.armSession(surface, nil, nil)
		// The runner's own goroutine ([Runner.Sent]), ended by its budget.
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
// broker's acknowledgement (milliseconds when healthy); one not answered then is
// an outage, which this record does not wait out. A missed frame leaves the
// continuation as the round's opening frame; the collection still says the run
// started its own session (`harness_session_continued`,
// `session_not_continued`).
const startedFrameBudget = 5 * time.Second

// armSession re-arms the run's guard with exactly the skills in loaded and those
// calls loaded ([skills.Guard.Replace], [Runner.observeLoads]): the bodies the
// session holds.
func (r *Runner) armSession(surface *tools.Surface, loaded []string, calls []ledger.Call) {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	guard.Replace(loaded)
	r.observeLoads(surface, calls)
}

// ownCalls is the calls of bridged one job made itself that the session it is
// rebuilt for holds, in bridge admission order: past from, the last call the
// round held when the job's session opened ([execstate.State.BridgeFrom]; a
// place in that order is the right one for which job made a call), and held at
// through ([heldAt]): where set, a capture's save point ([SessionBasis.Through]);
// nil, for the job's own copy whose harness ended its run, every call it made but
// a late or abandoned one. NEVER A LATE CALL, whose place can fall past from (an
// earlier job's call still running when this one opened) and which no brief had
// told; NOR ONE NOBODY WAITED FOR, whose answer never reached its session. A call
// with no place is no job's. owed is the reads held only as an earlier brief's
// count ([heldAt]): none is the job's own, each preceding its brief's read.
func ownCalls(bridged []BridgedCall, from int, through *int, owed []int) []ledger.Call {
	var out []ledger.Call
	for _, call := range inAdmissionOrder(bridged) {
		if call.Seq <= from || !heldAt(call, through, owed) {
			continue
		}
		out = append(out, call.Call)
	}
	return out
}

// ownHeld is the calls a reading found the session holding ([exactHolds]) that
// one job made itself, past from as [ownCalls] judges. None is one ownCalls
// counts: a reading is asked only of calls the save point does not hold
// ([Runner.holdingsAsk]).
func ownHeld(held []BridgedCall, from int) []ledger.Call {
	var out []ledger.Call
	for _, call := range held {
		if call.Seq > from {
			out = append(out, call.Call)
		}
	}
	return out
}
