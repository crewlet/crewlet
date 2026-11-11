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
// parked.
//
// Read by the engine immediately after the turn returns Suspended, so the
// state is persisted before the turn's stack unwinds. It is a Runner field
// rather than a return value because the park surfaces through turn.Result,
// which is the loop's vocabulary and must not grow a wire format in it.
func (r *Runner) Suspended() (Suspension, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suspension == nil {
		return Suspension{}, false
	}
	return *r.suspension, true
}

// Resume re-enters the turn the executor's run parked it on, in one of three
// ways, each read off what drove it.
//
// A COMPLETION COLLECTS THE RUN: what it submitted, the calls it made over the
// bridge, the branches and pull requests it delivered. The round the run was
// launched in is collected rather than launched again — a resumed turn that
// started over would pay for the run twice ([Runner.resumeAgentRun]). Its
// surface and skill-guard state are rebuilt as the run left them: the tools
// and skills the state recorded at the launch, and then every tool the run
// activated and every skill it loaded over the bridge, read off its bridged
// calls ([Runner.rebuiltSurface]). The run's calls were made against that
// surface, and the round's record states it: the tools the run held, and
// apart from them the ones it could still have discovered.
//
// A PERSON'S ANSWER CARRIES THE RUN ON ([Asked]), and so does the LOSS OF ITS
// JOB with the host it ran on ([Lost]): the same round, relaunched with what
// the run did so far, never collected ([Runner.continueAgentRun]). A lost job
// whose round's calls already hold its submission is the one exception: it
// finished its work and was lost on its way out, so the round is collected.
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
	// THE JOB'S PARKED STATE AS THE SESSION ITS BOX STARTED HOLDS IT: every
	// guard rebuilt from it below holds the skills its launch armed only where
	// the box said it continued that session ([Resume.parked]).
	state := r.cfg.Resume.parked()
	// THE NOTES THE TURN HAS READ, off the run's record in the order read —
	// so the phases after this one open with them as the phases before it
	// did. See [Resume.Steered].
	r.mu.Lock()
	r.steered = slices.Clone(r.cfg.Resume.Steered)
	r.mu.Unlock()

	// THE CALLS THE RUN MADE OVER THE BRIDGE, in the order they were
	// admitted. Each branch below seeds the turn's call log from them and
	// from the closed rounds once its own surface exists ([seedCalls]), so
	// the seeding reads the universe every later call is admitted against.
	bridged := inAdmissionOrder(r.cfg.Resume.Bridged)

	// A PERSON'S ANSWER CARRIES THE RUN ON, and so do a job lost with its host
	// and one refused on a sign-in replaced while it ran, each unless its
	// round was already submitted: see [Resume.carriedRound].
	if carry := r.cfg.Resume.carriedRound(); carry != nil {
		return r.continueAgentRun(ctx, history, state, carry, bridged)
	}
	return r.resumeAgentRun(ctx, history, state, r.cfg.Resume.Answer, bridged)
}

// carriedRound is the round this resume carries on rather than collects, nil
// where it collects the run — THE ONE DECISION [Runner.Resume] takes and
// [Resume.CarriesOn] reports, so the engine's record of what a resumed segment
// does with the round is this decision and no copy of it.
//
// A PERSON'S ANSWER CARRIES THE RUN ON; it is never the run's report (see
// [Asked]). And so does a job lost with its host ([Lost]), and one refused on a
// sign-in replaced while it ran ([Rotated]), unless the round's calls already
// hold its submission: such a job had finished its work, and the round is
// collected.
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
	}
	return nil
}

// CarriesOn reports whether the resume relaunches the round of the job it
// resumes from rather than collecting the job ([Resume.carriedRound]): what a
// segment that resumes from a collected job says of it as it begins, for every
// reader of its run's record (sandbox.ResumeCarry).
func (r Resume) CarriesOn() bool { return r.carriedRound() != nil }

// submitted reports whether a round's calls hold a submission its run made:
// a `submit_work` the bridge did not refuse, which the round's collection
// replays as its outcome ([replaySubmission]). Handed only the calls made since
// the answer that carried the round on ([sinceAnswer]), as the replay is.
func submitted(calls []ledger.Call) bool {
	return slices.ContainsFunc(calls, func(call ledger.Call) bool {
		return call.Name == SubmitWorkTool && !call.Failed
	})
}

// roundCalls is the open round's calls as the log beside its run's row holds
// them: the calls it lists ([BridgedCall]), in the order the bridge admitted
// them — a call whose own append reached the log only with a later one among
// them — and the identities of the calls it counts without listing: the round's
// own that it dropped from the middle (elided), how many of those it had taken
// late (lateElided, [Resume.LateElided]), and the ones its calls' own work made
// in the run's call log (absorbed).
type roundCalls struct {
	bridged    []BridgedCall
	elided     []ledger.Elided
	lateElided int
	absorbed   []ledger.Elided
}

// seedCalls records into a resumed run's log every call the turn made before
// this segment: the rounds that closed and whatever its executor run called
// over the bridge — the calls their records kept (the round's, and each closed
// round's [ledger.Iteration.Calls]), the ones those records count without
// listing, by identity (the round's elided, and each closed round's
// [ledger.Iteration.Elided]), and the calls those calls' own work made, by
// identity (the round's absorbed, and each closed round's
// [ledger.Iteration.Absorbed]).
//
// THE RUN'S CALL LOG STARTS FROM WHAT THE TURN ALREADY CALLED. A collection,
// a continuation after a person's answer and a reopen are the same turn in a
// fresh segment, so a count it derives must see every call made before it,
// or a call made again after a different one would take its first copy's
// operation id. See [turnctx.CallLog]. Order does not matter to a count, and
// a call recorded twice only raises one, which is safe.
//
// BY IDENTITY: the digest each call was admitted under ([ledger.Call.Digest]),
// which its record carries whatever it kept of its arguments — a run's log
// keeps a value past its share as a marker, and a parked turn's closed rounds
// are condensed the same way — and a tallied call's identity seeded as many
// times as it was made. Seeded from the arguments a record kept, a cut call
// counted as another call, and a dropped one as none: a later call that asked
// for something else derived an earlier call's operation id and was answered
// `applied` without landing. An identity the record could not keep — a
// tallied call's digest, or its tool too — or keeps withheld — a call held
// for a person's approval and the one they approved
// ([ledger.Call.Withheld]), whose arguments ride sealed — is seeded as a call
// matching no other ([turnctx.SeededCall.Unknown],
// [turnctx.CallLog.SeedUnknown]): an over-count, the safe direction.
//
// A CALL WITH NO DIGEST, AND NONE WITHHELD — one a build before it recorded,
// whose arguments it kept whole — is seeded by its canonical arguments, made
// through the surface the segment serves ([tools.Surface.ArgsOf]), so the
// seeding and every call made after it read one universe by construction, the
// tools the surface adds (submit_work, the discovery pair) included. The
// records hold the model's own spelling, which they keep: the closed rounds
// and the bridge's log of the run's calls are what the model sent, and a count
// read against them as written took a repeat spelt "Draft" after "draft" for
// another call and wrote it twice. A tool the surface does not hold (one
// removed since the run was launched) is seeded by its arguments as sent, and
// logged at debug so the fallback is visible: no call made now can reach it
// to be counted anyway.
func seedCalls(calls *turnctx.CallLog, surface *tools.Surface, history []ledger.Iteration, round roundCalls) {
	groups := [][]ledger.Call{bridgedLedger(round.bridged)}
	tallies := [][]ledger.Elided{round.elided, round.absorbed}
	for _, iteration := range history {
		groups = append(groups, iteration.Calls)
		tallies = append(tallies, iteration.Elided, iteration.Absorbed)
	}
	universe := surface.Universe()
	for _, group := range groups {
		for _, call := range group {
			switch {
			case call.Withheld:
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
