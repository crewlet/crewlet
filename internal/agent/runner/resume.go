package runner

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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

// Resume collects the executor run the turn parked on: what it submitted, the
// calls it made over the bridge, the branches and pull requests it delivered.
//
// THE SAME TURN CONTINUES. The round the run was launched in is collected
// rather than launched again — a resumed turn that started over would pay for
// the run twice — and the surface and skill-guard state are REPLAYED from the
// state rather than rebuilt from the plan, because the run's calls were made
// against the surface it was given, activations included.
func (r *Runner) Resume(ctx context.Context, history []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if r.cfg.Resume == nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: resume with no parked state")
	}
	state := r.cfg.Resume.State
	// THE NOTES THE TURN READ BEFORE IT PARKED, so the phases after this
	// one open with them as the phases before it did.
	r.mu.Lock()
	r.steered = append([]string(nil), state.Steered...)
	r.mu.Unlock()

	// THE RUN'S CALL LOG STARTS FROM WHAT THE TURN ALREADY CALLED. A resume
	// is the same turn in a fresh process, so a count it derives must see
	// every call made before it — the rounds that closed and whatever the
	// run called over the bridge — or a call made again after a different
	// one would take its first copy's operation id. See [turnctx.CallLog].
	bridged := inAdmissionOrder(r.cfg.Resume.Bridged)
	seedCalls(r.cfg.Turn.Context.CallLog(), history, bridgedLedger(bridged))

	// A PERSON'S ANSWER CARRIES THE RUN ON; it is never the run's report.
	// See [Asked].
	if asked := r.cfg.Resume.Asked; asked != nil {
		return r.continueAgentRun(ctx, history, state, asked, bridgedLedger(bridged))
	}
	return r.resumeAgentRun(ctx, history, state, r.cfg.Resume.Answer, bridged)
}

// seedCalls records into a resumed turn's log every call it made before it
// parked. Order does not matter to a count, and a call recorded twice only
// raises one, which is safe.
func seedCalls(log *turnctx.CallLog, history []ledger.Iteration, groups ...[]ledger.Call) {
	for _, iteration := range history {
		groups = append(groups, iteration.Calls)
	}
	for _, group := range groups {
		for _, call := range group {
			log.Seed(turnctx.SeededCall{Tool: call.Name, Args: call.Args})
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
