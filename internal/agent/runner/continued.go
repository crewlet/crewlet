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
//   - every call its run made through the seat's tools that the session had not
//     been answered when it was saved — every call the round's log took after
//     the read its save point is ([SessionBasis.Through]), so after the point it
//     was saved at or in flight then, whatever the log dropped below it — with
//     the call's outcome, held to the prior-work ledger's budgets; and how many
//     more the log took after it and dropped from its middle, which it keeps
//     only as a tally, with that tally ([prompts.CalledAfter]);
//   - every note a person sent the turn that the session has not read — read
//     after it was saved, by the run that went on in it or by a review, which
//     is never the session's, or taken at the last moment before a harness that
//     was cut off could have it — and then the ones this brief is the first to
//     read.
//
// AND IN TWO FORMS, because which box the launch gets is the launch's to learn:
// one for the box the session was last worked in, and one for any other, which
// adds what a new box changes — a fresh checkout, and a repository of
// Gitlet to check out again ([prompts.BoxReplaced]). The fresh
// brief, for a run whose box cannot take the session up after all, rides
// beside them.
//
// THE RUN'S GUARD IS THE SESSION'S IT RUNS IN ([Runner.sessionDecided]): the
// skills the launch state of the job it follows held and that job's own loads —
// those the log took through the point its capture was saved at, by the same
// test the calls after it are told by ([ownCalls]), where the launch restores
// it — because the continued model has those bodies in front of it; none for a
// run that starts a session of its own. Armed once the launch has decided,
// before the run starts, and put right where the run's box finds as it starts
// that it cannot take the session up after all and starts one of its own: its
// box says so before the run's first call ([Runner.sessionStarted]), and its
// job's record keeps what it said for every later rebuild of the guard
// ([Resume.Continued]).
//
// A CONVERSATION'S NEXT TURN continues the session its last turn left: its first
// launch the head its dispatch read, and a relaunch of a turn that has kept no
// session of its own yet the one its first launch froze (internal/sandbox,
// restore.go). That session is another turn's, in a box of its own, so its run
// is told where this turn begins and what the new box changes
// ([prompts.BuildNextTurn]), then handed the launch's own fresh brief whole —
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
// ([Runner.calledAfter]), then every note it has not read and the ones fresh
// holds, in the box it was last worked in, and in another with what that
// changes. The engine's own words and the seat's, every foreign word in them
// fenced or defused under the relaunching segment's token; branch and the
// basis's home are the engine's record of the session, defused too.
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
	after = append(after, prompts.CalledAfter(r.calledAfter(ctx, round, basis.Through, surface, b)))
	after = append(after, r.unreadSince(basis.NoteCursor)...)
	after = append(after, renderedNotes(fresh)...)
	return &keptBrief{basis: basis, after: after, newBox: prompts.BoxReplaced(provenance.Neutralise(b, branch),
		provenance.Neutralise(b, basis.Home), r.checkoutName())}
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
// its last fire's ([notify.Scheduled]) — then fresh, the launch's own fresh
// brief or reseed, whole. Only in a box of its own, so its same-box brief is
// empty. The base's turn and home are the engine's record of that session,
// defused under the launching segment's token.
func (r *Runner) baseSession(basis SessionBasis, fresh string) *SessionContinuation {
	if !basis.ContinuesBase() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	base := basis.Base
	lead := prompts.BuildNextTurn(prompts.NextTurn{
		BaseTurn: provenance.Neutralise(b, base.Turn), EndedAt: base.EndedAt,
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
// round's calls its run's log had taken when the capture read it
// ([SessionBasis.Through]) — was not answered of round: every call the log
// still lists that it took after that read ([afterSave]), in the order the
// bridge admitted them, and how many more it took after it and dropped from
// its middle, with the round's tally of every call it dropped
// ([droppedAfterSave]).
//
// THE LIST AS THE PRIOR-WORK LEDGER RENDERS A ROUND, AND HELD TO ITS BUDGETS
// ([ledger.Format]): the meta-tools it skips left out, an argument value or a
// failed call's error past its budget rewritten by the seat's auxiliary model
// first, as [Runner.priorWork] fits the ledger, and the reads past the ledger's
// cap counted rather than listed — read as the surface's positively read-only
// tools, the delivery gate's reading. Verbatim, a session saved early in a long
// round was handed every call after it whole, each up to the 16 KiB a call's
// record keeps. And defused under b: its arguments are a model's, and a failed
// call's error is whatever the tool said.
func (r *Runner) calledAfter(ctx context.Context, round roundCalls, through int, surface *tools.Surface,
	b provenance.Boundary) prompts.AfterSave {

	var after []ledger.Call
	for _, call := range inAdmissionOrder(round.bridged) {
		if afterSave(call, through) && !slices.Contains(r.cfg.SkipNames, call.Name) {
			after = append(after, call.Call)
		}
	}
	out := prompts.AfterSave{Dropped: droppedAfterSave(round, through)}
	if len(after) > 0 {
		opts := ledger.Format(r.cfg.SkipNames, surface.Universe().KnownReads())
		opts.Fitted = ledgerfit.Fit(ctx, r.cfg.Compact, ledger.CallPieces(after, opts))
		out.Calls = provenance.Neutralise(b, ledger.FormatCalls(after, opts))
	}
	if out.Dropped > 0 {
		out.Tally = provenance.Neutralise(b, ledger.FormatElided(round.elided, r.cfg.SkipNames))
	}
	return out
}

// afterSave reports whether call came after the save point through: the round's
// log took it after the read that placed the save ([BridgedCall.Appended] past
// it), whatever its place in the order the bridge admitted the calls — a call
// in flight at the read is taken once its tool answers. A call the log took
// with no number, an older build's append, is after every save point: told
// again, never not at all.
func afterSave(call BridgedCall, through int) bool {
	return call.Appended == 0 || call.Appended > through
}

// droppedAfterSave is how many of the calls round's log took after the save
// point through it dropped from its middle, which no list can tell a session
// one by one: the log's appends past the point, less the calls it still lists
// numbered past it.
//
// EXACT, because every append is numbered one past the last
// ([BridgedCall.Appended]) and each call the log took is listed or counted
// without listing, never both — so the round's appends are the two together,
// and the numbers past the point the log does not list are the dropped ones.
// A listed call with no number is not subtracted, its place unknown, so a log
// an older build appended to may over-count, held to the dropped calls the
// round counts at all; it never under-counts.
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
}

// sessionDecided is what a launch tells the runner once it has decided how its
// run takes up a session ([AgentRunRequest.Decided]), before the run starts.
//
// IT ARMS THE RUN'S GUARD with the skills that session holds, and nothing else
// ([Runner.armSession]): the launch state of the job the relaunch follows and
// that job's own loads — through the point the turn's kept capture of it was
// saved at, where the launch restores that, and all of them where the job's own
// copy in its box is continued — because the continued model has those bodies
// in front of it; none for a session of the run's own, whose model has read no
// skill. Never the turn's other jobs' loads: a session another job worked in is
// not this one. AND NONE FOR THE SESSION THE CONVERSATION'S LAST TURN LEFT,
// whatever the parked state holds: the skills that session loaded were another
// turn's, which this turn's guard never armed and its lead says were set up
// anew — the safe direction, a skill-covered call refused until it is loaded
// again rather than admitted on a body nobody read this turn. Armed before the
// run starts, so no call of the run is admitted against a guard that holds what
// its model never read.
//
// AND IT PUBLISHES THE PROMPT THE RUN WAS HANDED as the phase's opening frame
// ([emitter.prompted]) — the continuation in the form its box takes, or the
// fresh brief — where the phase's record does not carry it already: the
// started record carries the fresh brief, which a run that continues its
// session never reads.
//
// What it decided is kept in decided, which the box's word on the session it
// started is read against ([Runner.sessionStarted]).
func (r *Runner) sessionDecided(ctx context.Context, emit emitter, round int, system prompts.Prompt,
	surface *tools.Surface, caps roundCaps, b launchBriefs, decided *launchDecided) func(SessionDecision) {

	return func(d SessionDecision) {
		decided.set(d)
		handed := b.fresh
		switch {
		case d.Continues && d.Base && b.kept != nil:
			r.armSession(surface, nil, nil)
			handed = prompts.Prompt{Text: b.kept.Replaced}
		case d.Continues && d.Kept && b.kept != nil:
			r.armSession(surface, b.state.LoadedSkills,
				ownCalls(b.bridged, b.state.BridgeFrom, &b.kept.Basis.Through))
			handed = prompts.Prompt{Text: b.kept.Brief}
			if d.NewBox {
				handed = prompts.Prompt{Text: b.kept.Replaced}
			}
		case d.Continues && !d.Kept && b.inPlace != "":
			r.armSession(surface, b.state.LoadedSkills, ownCalls(b.bridged, b.state.BridgeFrom, nil))
			handed = prompts.Prompt{Text: b.inPlace}
		default:
			r.armSession(surface, nil, nil)
			if b.freshReported {
				return
			}
		}
		emit.prompted(ctx, phase.Execute, round, system, handed, caps)
	}
}

// launchDecided is how a launch decided its run takes up a session, as the
// runner keeps it between the launch's word ([AgentRunRequest.Decided]) and the
// word of the run's box on the session it started ([AgentRunRequest.Started]):
// the first is told before the run starts and the second as its harness first
// reaches the seat's tools, on another goroutine.
type launchDecided struct {
	mu sync.Mutex
	d  SessionDecision
}

func (l *launchDecided) set(d SessionDecision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.d = d
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

// ownCalls is the calls of bridged one job made itself, in the order the bridge
// admitted them: past from, the last call the round held when the job's session
// opened ([execstate.State.BridgeFrom]) — a place in that order, which is the
// right one for which job made a call — and, where through is set, the save
// point of a capture of the job's session ([SessionBasis.Through]), none the
// log took after it ([afterSave]): the calls the capture had been answered,
// whose loads the restored session holds, whatever their place. nil through
// bounds nothing, for the job's own copy of its session in its box, which
// holds every call it made. A call with no place is no job's.
func ownCalls(bridged []BridgedCall, from int, through *int) []ledger.Call {
	var out []ledger.Call
	for _, call := range inAdmissionOrder(bridged) {
		if call.Seq <= from || (through != nil && afterSave(call, *through)) {
			continue
		}
		out = append(out, call.Call)
	}
	return out
}
