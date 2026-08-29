package runner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/tools"
)

// Suspension is what an Execute phase produced when it parked on a detached
// sandbox run: everything the resume needs, ready to persist.
type Suspension struct {
	State execstate.State
}

// Suspended reports the state of a suspended Execute phase, if the last one
// suspended.
//
// Read by the run_sandbox tool's caller — the engine — immediately after the
// turn returns Suspended, so the conversation is persisted before the turn's
// stack unwinds. It is a Runner field rather than a return value because the
// suspension surfaces through turn.Result, which is the loop's vocabulary and
// must not grow a wire format in it.
func (r *Runner) Suspended() (Suspension, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suspension == nil {
		return Suspension{}, false
	}
	return *r.suspension, true
}

// Resume re-enters the suspended Execute phase with the sandbox result spliced
// in as the pending call's reply.
//
// THE SAME TURN CONTINUES. Plan is skipped — a resumed turn that re-planned
// would re-derive a plan for work already half-done — and the tool surface and
// skill-guard state are REPLAYED from the state rather than rebuilt, because a
// phase that rebuilt its surface from the plan would lose every activation the
// pre-suspend rounds made.
//
// The phase record it publishes covers only the POST-resume slice of the
// conversation: the earlier rounds are already recorded, and re-emitting them
// would redraw a turn the dashboard already has. Their token counters do carry
// forward, because those are the turn's total.
func (r *Runner) Resume(ctx context.Context, history []ledger.Iteration) (turn.Execution, turn.Surface, error) {
	if r.cfg.Resume == nil {
		return turn.Execution{}, turn.Surface{}, fmt.Errorf("runner: resume with no suspended state")
	}
	state := r.cfg.Resume.State
	answer := r.cfg.Resume.Answer

	snapshot := r.cfg.Registry.Snapshot()
	surface, err := r.surfaceWith(phase.Execute, snapshot, nil, state.ActiveTools)
	if err != nil {
		return turn.Execution{}, turn.Surface{}, err
	}

	res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Execute, surface: surface,
		rounds: r.cfg.Caps.ExecuteRounds, ceiling: r.cfg.Caps.ExecuteCeiling,
		iteration: state.Round,
		seed:      state.Answer(answer),
		// A resumed Execute can suspend AGAIN: the executor may call
		// run_sandbox a second time to continue in the same box.
		allowSuspend: true,
		spent: toolloop.Result{
			InputTokens: state.InputTokens, OutputTokens: state.OutputTokens,
		},
	})
	if err != nil {
		return turn.Execution{}, turn.Surface{}, err
	}

	missing := missingTools(surface, snapshot)
	r.emitter().completed(ctx, phaseRecord{
		Phase: phase.Execute, Iteration: state.Round,
		// No system or user prompt: this phase did not open a conversation,
		// it re-entered one. Publishing the original opening again would
		// show the reader a prompt that was not sent this time.
		Result: res.Result, Exhausted: res.Exhausted,
		Notes:     missingNote(missing),
		Available: surface.Active(),
	})

	return turn.Execution{
		Text:  res.Text,
		Calls: resumedCalls(surface, state),
		// A resumed phase can suspend AGAIN: the executor may call
		// run_sandbox a second time to continue in the same box.
		Suspended:       res.Suspended,
		MissingTools:    missing,
		ExhaustedRounds: res.Exhausted,
	}, describe(surface), nil
}

// resumedCalls is what the WHOLE Execute phase called, pre-suspend rounds
// included.
//
// The delivery gate and the iteration ledger both read this list, and both are
// about the turn rather than about one re-entry: a resumed turn whose gate saw
// only the post-resume calls would read a delivery made before the suspend as
// never having happened, and re-fire it.
func resumedCalls(s *tools.Surface, state execstate.State) []ledger.Call {
	prior := make([]ledger.Call, 0, len(state.ToolExecutions))
	for _, exec := range state.ToolExecutions {
		call := ledger.Call{}
		if name, ok := exec["name"].(string); ok {
			call.Name = name
		}
		if call.Name == "" {
			continue
		}
		if args, ok := exec["arguments"].(string); ok {
			call.Args = decodeArgs(args)
		}
		if ok, present := exec["success"].(bool); present {
			call.Failed = !ok
		}
		if out, ok := exec["result"].(string); ok {
			call.Result = out
		}
		prior = append(prior, call)
	}
	return append(prior, calls(s)...)
}

// recordSuspension captures the conversation an Execute phase parked on, for
// the engine to persist the moment the turn returns.
//
// Built HERE rather than by the engine because this is the only frame that
// holds the loop's messages and the phase's live surface. The engine reads it
// back through [Runner.Suspended] and writes it to the pending-run row.
//
// A state that fails its invariants is NOT recorded, and the absence is what
// the engine reports: a run whose conversation could not be serialized is one
// nothing can resume, and it must fail while the box is still in the engine's
// hands rather than at a resume days later.
func (r *Runner) recordSuspension(round int, surface *tools.Surface,
	res toolloop.Result, history []ledger.Iteration,
) {
	state := execstate.State{
		Version:         execstate.Version,
		Messages:        res.Messages,
		PendingCallID:   res.PendingToolCallID,
		PendingCallName: res.PendingToolName,
		ActiveTools:     surface.Active(),
		Round:           round,
		InputTokens:     res.InputTokens,
		OutputTokens:    res.OutputTokens,
		ToolExecutions:  toolExecutions(res.Executions),
		Iterations:      history,
		Task:            r.cfg.Task,
	}
	if err := state.Validate(); err != nil {
		log.Error("execute_suspension_invalid", "round", round, "error", err.Error())
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspension = &Suspension{State: state}
}

// decodeArgs turns the wire's JSON argument string back into the map the
// ledger renders from.
//
// The ledger elides PER VALUE, so it needs the map; the execution row carries
// the string because that is what the dashboard reads. Unparseable input
// yields nil rather than an error: a resumed turn's prior-work ledger losing
// one call's arguments is a worse-rendered line, while failing the resume over
// it loses the whole conversation.
func decodeArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil
	}
	return args
}
