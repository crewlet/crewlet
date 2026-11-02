package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/provenance"
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
//     been answered when it was saved — after the point it was saved at, or in
//     flight then — with the call's outcome;
//   - every note a person sent the turn that the session has not read — read
//     after it was saved, by the run that went on in it or by a review, which
//     is never the session's, or taken at the last moment before a harness that
//     was cut off could have it — and then the ones this brief is the first to
//     read.
//
// AND IN TWO FORMS, because which box the launch gets is the launch's to learn:
// one for the box the session was last worked in, and one for any other, which
// adds what a fresh checkout changes ([prompts.BoxReplaced]). The fresh brief,
// for a run whose box cannot take the session up after all, rides beside them.
//
// THE RUN'S GUARD IS THE SESSION'S IT RUNS IN ([Runner.sessionDecided]): the
// skills the launch state of the job it follows held and that job's own loads —
// through the point its capture was saved at, where the launch restores it —
// because the continued model has those bodies in front of it; none for a run
// that starts a session of its own. Armed once the launch has decided, before
// the run starts. Within a turn only: the next turn's first launch continues
// nothing yet.

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
// session: lead — what carries the session on — then what came after the
// session was saved, then every note it has not read and the ones fresh holds,
// in the box it was last worked in, and in another with what that changes. The
// engine's own words and the seat's, every foreign word in them fenced or
// defused under the relaunching segment's token; branch and the basis's home
// are the engine's record of the session, defused too.
func (r *Runner) keptSession(basis SessionBasis, lead string, bridged []BridgedCall,
	fresh []steer.Note, branch string) *SessionContinuation {

	if !basis.Continues() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	parts := []string{strings.TrimSpace(lead)}
	if basis.Interrupted {
		parts = append(parts, prompts.InterruptedSession)
	}
	parts = append(parts, prompts.CalledAfter(r.calledAfter(bridged, basis.BridgeSeq, basis.BridgeOpen, b)))
	parts = append(parts, r.unreadSince(basis.NoteCursor)...)
	parts = append(parts, renderedNotes(fresh)...)
	parts = slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" })
	brief := strings.Join(parts, "\n\n")
	replaced := brief + "\n\n" + prompts.BoxReplaced(provenance.Neutralise(b, branch),
		provenance.Neutralise(b, basis.Home))
	return &SessionContinuation{Basis: basis, Brief: brief, Replaced: replaced}
}

// calledAfter is the calls of bridged a session saved with seq as the highest
// call it had been answered, and open as the places below it no call was
// recorded at yet ([SessionBasis.BridgeOpen]), was not answered: every call
// past seq, and every call at an open place — one in flight when the session
// was saved, whose outcome it may never have been handed. As the prior-work
// ledger renders them, the meta-tools it skips left out, and defused under b:
// its arguments are a model's, and a failed call's error is whatever the tool
// said. "" for none. A call with no place (zero) is after nothing.
func (r *Runner) calledAfter(bridged []BridgedCall, seq int, open []int, b provenance.Boundary) string {
	var after []ledger.Call
	for _, call := range inAdmissionOrder(bridged) {
		unanswered := call.Seq > seq || (call.Seq > 0 && slices.Contains(open, call.Seq))
		if unanswered && !slices.Contains(r.cfg.SkipNames, call.Name) {
			after = append(after, call.Call)
		}
	}
	if len(after) == 0 {
		return ""
	}
	return provenance.Neutralise(b, ledger.FormatCalls(after, ledger.FormatOptions{Skip: r.cfg.SkipNames}))
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
// not this one. Armed before the run starts, so no call of the run is admitted
// against a guard that holds what its model never read.
//
// AND IT PUBLISHES THE PROMPT THE RUN WAS HANDED as the phase's opening frame
// ([emitter.prompted]) — the continuation in the form its box takes, or the
// fresh brief — where the phase's record does not carry it already: the
// started record carries the fresh brief, which a run that continues its
// session never reads.
func (r *Runner) sessionDecided(ctx context.Context, emit emitter, round int, system prompts.Prompt,
	surface *tools.Surface, caps roundCaps, b launchBriefs) func(SessionDecision) {

	return func(d SessionDecision) {
		handed := b.fresh
		switch {
		case d.Continues && d.Kept && b.kept != nil:
			r.armSession(surface, b.state.LoadedSkills,
				ownCalls(b.bridged, b.state.BridgeFrom, b.kept.Basis.BridgeSeq, b.kept.Basis.BridgeOpen))
			handed = prompts.Prompt{Text: b.kept.Brief}
			if d.NewBox {
				handed = prompts.Prompt{Text: b.kept.Replaced}
			}
		case d.Continues && !d.Kept && b.inPlace != "":
			r.armSession(surface, b.state.LoadedSkills, ownCalls(b.bridged, b.state.BridgeFrom, 0, nil))
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
// opened ([execstate.State.BridgeFrom]); through through, where it is not zero
// — the highest a capture of the job's session had been answered — and at no
// place open names, which the capture had not been answered yet. A call with
// no place is no job's.
func ownCalls(bridged []BridgedCall, from, through int, open []int) []ledger.Call {
	var out []ledger.Call
	for _, call := range inAdmissionOrder(bridged) {
		if call.Seq <= from || (through > 0 && call.Seq > through) || slices.Contains(open, call.Seq) {
			continue
		}
		out = append(out, call.Call)
	}
	return out
}
