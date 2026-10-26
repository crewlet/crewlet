package runner

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
	seedCalls(r.cfg.Turn.Context.CallLog(), history, ledgerCalls(r.cfg.Resume.Bridged))

	// A PERSON'S ANSWER CARRIES THE RUN ON; it is never the run's report.
	// See [Asked].
	if asked := r.cfg.Resume.Asked; asked != nil {
		return r.continueAgentRun(ctx, history, state, asked, ledgerCalls(r.cfg.Resume.Bridged))
	}
	return r.resumeAgentRun(ctx, history, state, r.cfg.Resume.Answer, r.cfg.Resume.Bridged)
}

// BridgeSurface is the executor's surface rebuilt from the turn's park, for a
// node that lost the bridge session its run's box calls — a restart — while
// the run goes on: the surface the launch built, with the tools the run had
// active and the skills the turn had loaded, on its own fresh meter
// ([BridgedSpend]), which the caller adds to what the row already holds.
//
// THE TURN'S CALL LOG STARTS FROM WHAT THE TURN ALREADY CALLED, as a resume's
// does — the rounds that closed and the run's bridged calls so far — because
// the box's next write derives its operation id from that count, and a count
// restarted at the rebuild would hand a new write an earlier one's id and
// answer it as a retry of work already done.
//
// THE SUBMISSION IT CAPTURES IS NEVER READ, exactly as the launch's is not
// when the run is collected in another process: the collection replays
// submit_work from the run's durable log ([Runner.resumeAgentRun]).
func (r *Runner) BridgeSurface(ctx context.Context) (*tools.Surface, *BridgedSpend, error) {
	if r.cfg.Resume == nil {
		return nil, nil, fmt.Errorf("runner: a bridge surface is rebuilt from a park, " +
			"and this runner was given none")
	}
	state := r.cfg.Resume.State
	logged := ledgerCalls(r.cfg.Resume.Bridged)
	seedCalls(r.cfg.Turn.Context.CallLog(), state.Iterations, logged)

	snapshot := r.executorUniverse(r.cfg.Registry.Snapshot())
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return turn.Record(state.Iterations, calls(surface)) },
			func() turn.Surface { return describe(surface) }))
	bridged := NewBridgedSpend()
	built, err := r.surfaceWith(ctx, r.meterFor(bridged), phase.Execute, state.Round, snapshot,
		submit, state.ActiveTools, state.LoadedSkills...)
	if err != nil {
		return nil, nil, err
	}
	surface = built
	return built, bridged, nil
}

// ledgerCalls is a run's bridged calls in the shape the turn's ledger, its
// delivery check and its submission's citations read: what each call did,
// without the measurement the run's own record carries.
func ledgerCalls(execs []toolloop.Execution) []ledger.Call {
	out := make([]ledger.Call, 0, len(execs))
	for _, ex := range execs {
		out = append(out, ledger.Call{Name: ex.Name, Args: ex.Args, Result: ex.Output, Failed: ex.Failed})
	}
	return out
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
