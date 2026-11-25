package runner

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/tools"
)

// Suspension is what an Execute phase produced when it launched its run and
// parked: everything the run's collection needs, ready to persist.
type Suspension struct {
	State execstate.State
}

// Suspended reports the state of the parked Execute phase, if the last one
// parked. The engine reads it as soon as the turn returns Suspended, to persist
// it before the stack unwinds. A Runner field, not a return value: the park
// surfaces through turn.Result, the loop's vocabulary, which must not grow a
// wire format.
func (r *Runner) Suspended() (Suspension, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suspension == nil {
		return Suspension{}, false
	}
	return *r.suspension, true
}

// Resume re-enters the turn the executor's run parked it on.
//
// A COMPLETION COLLECTS THE RUN ([Runner.resumeAgentRun]): its submission,
// bridged calls and deliveries, never launching the round again, which would
// pay for the run twice. Its surface and skill guard are rebuilt as the run
// left them: the launch's recorded tools and skills plus every tool it
// activated and skill it loaded over the bridge ([Runner.rebuiltSurface]),
// the surface its calls were made against.
//
// A person's answer, a lost job and the other carried cases relaunch the same
// round with what the run did so far ([Runner.continueAgentRun]); which ones,
// and the submitted-round exception, is [Resume.carriedRound].
func (r *Runner) Resume(ctx context.Context, history []ledger.Iteration) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	work, surface, err := r.resume(ctx, history)
	if err == nil && !work.Suspended && r.cfg.Reply.Item != "" {
		work.Answer = r.pickupAnswer(ctx)
	}
	return work, surface, err
}

// resume is [Runner.Resume] before the engine reads the turn's typed answer.
func (r *Runner) resume(ctx context.Context, history []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if r.cfg.Resume == nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: resume with no parked state")
	}
	// The parked state as the box's session holds it: the guards rebuilt from
	// it hold the launch's skills only where the box continued that session
	// ([Resume.parked]).
	state := r.cfg.Resume.parked()
	// The notes already read, so later phases open with them as earlier ones
	// did ([Resume.Steered]).
	r.mu.Lock()
	r.steered = slices.Clone(r.cfg.Resume.Steered)
	r.mu.Unlock()

	// Each branch seeds the call log from these only once its own surface
	// exists ([seedCalls]), so the seeding reads the universe every later call
	// is admitted against.
	bridged := inAdmissionOrder(r.cfg.Resume.Bridged)

	if carry := r.cfg.Resume.carriedRound(); carry != nil {
		return r.continueAgentRun(ctx, history, state, carry, bridged)
	}
	return r.resumeAgentRun(ctx, history, state, r.cfg.Resume.Answer, bridged)
}

// carriedRound is the round this resume carries on rather than collects, nil
// where it collects the run: the one decision [Runner.Resume] takes and
// [Resume.CarriesOn] reports, so the engine's record holds no copy of it.
//
// A person's answer always carries the run on; it is never the run's report
// ([Asked]). A job lost with its host ([Lost]), refused on a replaced sign-in
// ([Rotated]), refused its continued session ([Refused]) or limited before any
// work ([Limited]) carries on unless
// the round's calls already hold its submission: that job had finished, and
// the round is collected.
func (r Resume) carriedRound() carried {
	if r.Asked != nil {
		return r.Asked
	}
	submittedRound := submitted(bridgedLedger(sinceAnswer(inAdmissionOrder(r.Bridged), r.Answered)))
	switch {
	case r.Lost != nil && !submittedRound:
		return r.Lost
	case r.Rotated != nil && !submittedRound:
		return r.Rotated
	case r.Refused != nil && !submittedRound:
		return r.Refused
	case r.Limited != nil && !submittedRound:
		return r.Limited
	}
	return nil
}

// CarriesOn reports whether the resume relaunches the job's round rather than
// collecting the job ([Resume.carriedRound]), as a resumed segment states it
// for every reader of its run's record (sandbox.ResumeCarry).
func (r Resume) CarriesOn() bool { return r.carriedRound() != nil }

// submitted reports whether a round's calls hold a `submit_work` the bridge did
// not refuse, which collection replays as the outcome ([replaySubmission]).
// Handed only the calls since the answer that carried the round on
// ([sinceAnswer]), as the replay is.
func submitted(calls []ledger.Call) bool {
	return slices.ContainsFunc(calls, func(call ledger.Call) bool {
		return call.Name == SubmitWorkTool && !call.Failed
	})
}

// roundCalls is the open round's calls as its run's log holds them: bridged,
// in the bridge's admission order (a late append included); unfinished, begun
// but not finished as the log was read, so of unknown outcome
// ([ledger.Call.Unfinished]); and, by identity only, the round's calls dropped
// from the middle (elided; lateElided of them taken late, [Resume.LateElided])
// and those its calls' own work made (absorbed).
//
// of is the round they belong to: the open round on a relaunch, the collected
// one (also last in the closed rounds) on a send-back, else the round reopened
// or collected elsewhere. A brief relisting a closed round's call reads by it
// which closed entry is this round ([Runner.closedShown]). Set wherever a
// roundCalls is built, so no brief reads its zero.
type roundCalls struct {
	of         int
	bridged    []BridgedCall
	unfinished []BridgedCall
	elided     []ledger.Elided
	lateElided int
	absorbed   []ledger.Elided
}

// UnfinishedNote is what every record of a round keeps as the answer of a call
// its log held in flight when read ([ledger.Call.Unfinished]). Never a plain
// failure, which a run takes for a call that did nothing and makes again (the
// duplicate delegation, post or write this prevents): its effects may stand.
// The words hold for every such call, whether still running, returned but not
// yet stored, or its process stopped.
const UnfinishedNote = "This call's outcome is unknown: its turn stopped waiting for it before " +
	"its result was recorded, so what it did may have taken effect. Check whether it did " +
	"before making it again."

// seedCalls seeds a resumed run's call log with every call the turn made
// before this segment, or a repeat after a different call takes its first
// copy's operation id ([turnctx.CallLog]): the round's bridged and in-flight
// calls (begun is made, even if still running on the node the run left),
// the closed rounds' calls, and the elided and absorbed tallies. Over-counting
// is the safe direction. Each is seeded by digest ([ledger.Call.Digest]), not
// by the args a record kept, which may be cut and let a different call derive
// its operation id and be answered `applied` without landing. A call no record
// identifies (withheld, digestless in flight, args unread) matches no other
// ([turnctx.SeededCall.Unknown]); another digestless one is seeded by its
// canonical args through this surface ([tools.Surface.ArgsOf]), as later
// calls are: records keep the model's spelling ("Draft" after "draft").
func seedCalls(calls *turnctx.CallLog, surface *tools.Surface, history []ledger.Iteration, round roundCalls) {
	groups := [][]ledger.Call{bridgedLedger(round.bridged), bridgedLedger(round.unfinished)}
	tallies := [][]ledger.Elided{round.elided, round.absorbed}
	for _, iteration := range history {
		groups = append(groups, iteration.Calls)
		tallies = append(tallies, iteration.Elided, iteration.Absorbed)
	}
	universe := surface.Universe()
	for _, group := range groups {
		for _, call := range group {
			switch {
			case call.Withheld, (call.Unfinished || call.ArgsUnread) && call.Digest == "":
				// No record holds these arguments.
				calls.Seed(turnctx.SeededCall{Tool: call.Name, Unknown: true})
				continue
			case call.Digest != "":
				calls.Seed(turnctx.SeededCall{Tool: call.Name, Digest: call.Digest})
				continue
			}
			if _, known := universe.Lookup(call.Name); !known {
				log.Debug("agent_resume_seed_unknown_tool", "tool", call.Name)
			}
			calls.Seed(turnctx.SeededCall{Tool: call.Name, Args: surface.ArgsOf(call.Name, call.Args)})
		}
	}
	for _, tally := range tallies {
		for _, e := range tally {
			if e.Name == "" {
				calls.SeedUnknown(e.Calls)
				continue
			}
			for range e.Calls {
				calls.Seed(turnctx.SeededCall{Tool: e.Name, Digest: e.Digest, Unknown: e.Digest == ""})
			}
		}
	}
}

// loadedSkills is what the running phase's guard has unlocked, or nil where
// this turn arms none.
func (r *Runner) loadedSkills() []string {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	return guard.LoadedKeys()
}
