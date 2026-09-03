package runner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/structured"
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
// THE SAME TURN CONTINUES. The executor is re-entered rather than restarted —
// a resumed turn that started over would re-derive work already half done —
// and the tool surface and skill-guard state are REPLAYED from the state
// rather than rebuilt, because a phase that rebuilt its surface would lose
// every activation the pre-suspend rounds made.
//
// THE RECORD IT PUBLISHES COVERS THE WHOLE PHASE, pre-suspend rounds
// included. This used to carry only the post-resume slice, on the argument
// that the earlier rounds were already recorded and re-emitting them would
// redraw a turn the dashboard already had. That premise was false: a
// suspending phase returns before `emit.completed` runs, so it publishes no
// completed event at all, and its `agent_turn_progress` frames are stream-only
// and refused by the event store. Nothing durable held those rounds. What the
// argument protected against — a double record — could not happen, and what it
// cost was every round before the suspend, the `run_sandbox` call that caused
// it included, with the resumed half renumbered from 1 beside token counters
// covering both halves.
func (r *Runner) Resume(ctx context.Context, history []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if r.cfg.Resume == nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: resume with no suspended state")
	}
	state := r.cfg.Resume.State
	answer := r.cfg.Resume.Answer

	if state.AgentRun {
		// THE STATE DECIDES, NOT THE CONFIG. A run launched as an agentic
		// one is collected as one however the company's providers have
		// been applied in the days it was parked — see
		// [execstate.State.AgentRun].
		return r.resumeAgentRun(ctx, state, answer, r.cfg.Resume.Bridged)
	}

	snapshot := r.cfg.Registry.Snapshot()

	// The submission tool is rebuilt, not replayed: it is where the phase
	// ENDS, and the re-entered conversation has not ended yet. Its checks
	// read the resumed surface's own record, which resumedCalls widens to
	// the whole phase — so a delivery made before the suspend is citable
	// after it, and is never demanded twice.
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return resumedCalls(surface, state) },
			func() turn.Surface { return describe(surface) }))

	built, err := r.surfaceWith(ctx, phase.Execute, state.Round, snapshot, submit,
		state.ActiveTools, state.LoadedSkills...)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Execute, surface: surface,
		rounds: r.cfg.Caps.ExecutorRounds, ceiling: r.cfg.Caps.ExecutorCeiling,
		iteration:      state.Round,
		seed:           state.Answer(answer),
		terminateAfter: []string{SubmitWorkTool},
		// A resumed executor can suspend AGAIN: it may call run_sandbox a
		// second time to continue in the same box.
		allowSuspend: true,
		prior:        priorRounds(state),
	})
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}

	if res.Suspended {
		r.recordSuspension(state.Round, surface, res.Result, history)
		return turn.Work{
			Text: res.Text, Calls: resumedCalls(surface, state), Suspended: true,
		}, describe(surface), nil
	}

	// The same finish as a fresh pass, including the rescue: an executor
	// that came back from a coding run and then stopped without reporting
	// is exactly as unjudged as one that never started.
	//
	// No system or user prompt on the record: this phase did not open a
	// conversation, it re-entered one, and publishing the original opening
	// again would show a reader a prompt that was not sent this time.
	work, described, err := r.finishWork(phaseCtx, state.Round, work{
		submit: submit, res: res, surface: surface, snapshot: snapshot,
	})
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	// The WHOLE phase's calls, pre-suspend rounds included — see
	// resumedCalls.
	work.Calls = resumedCalls(surface, state)
	return work, described, nil
}

// resumedCalls is what the WHOLE executor phase called, pre-suspend rounds
// included.
//
// The delivery check, the submission's own citations and the iteration ledger
// all read this list, and all three are about the turn rather than about one
// re-entry: a resumed turn that saw only the post-resume calls would read a
// delivery made before the suspend as never having happened, and re-fire it.
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

// priorRounds is what the phase did before it suspended, back in the loop's
// own shape so the resumed phase can continue it.
//
// The wire rows are loose maps ([types.ToolExecution] and
// [types.RoundNarration] are both `map[string]any`), because the event
// envelope evolves additive-only and a producer this build predates may have
// written fields it does not know. A row missing the one field that matters —
// a call with no name, a round with no number — is dropped rather than
// defaulted: a call numbered 0 would land in a round no other row shares and
// render as a round of its own.
func priorRounds(state execstate.State) toolloop.Result {
	out := toolloop.Result{
		RoundsUsed:   state.RoundsUsed,
		InputTokens:  state.InputTokens,
		OutputTokens: state.OutputTokens,
	}
	for _, exec := range state.ToolExecutions {
		name, _ := exec["name"].(string)
		round, ok := intField(exec["round"])
		if name == "" || !ok {
			continue
		}
		ex := toolloop.Execution{Round: round, Name: name}
		if args, ok := exec["arguments"].(string); ok {
			ex.Args = decodeArgs(args)
		}
		if out, ok := exec["result"].(string); ok {
			ex.Output = out
		}
		if ok, present := exec["success"].(bool); present {
			ex.Failed = !ok
		}
		out.Executions = append(out.Executions, ex)
	}
	for _, narr := range state.RoundNarration {
		round, ok := intField(narr["round"])
		if !ok {
			continue
		}
		reasoning, _ := narr["reasoning"].(string)
		content, _ := narr["content"].(string)
		out.Narration = append(out.Narration,
			toolloop.Narration{Round: round, Reasoning: reasoning, Content: content})
	}
	return out
}

// intField reads a number that has been through JSON, where every one of them
// is a float64 — and out of a Go map that never was, where it is still an int.
// Both shapes reach here: a state serialized to the pending-run row and back,
// and one handed straight over in the process that wrote it.
func intField(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
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
		// The rounds themselves, so the resumed phase continues the count
		// instead of restarting it — and so they reach the store at all.
		// Nothing else records them: this phase publishes no completed event
		// (it returns suspended, before the record is written) and its
		// progress frames are stream-only.
		RoundsUsed:     res.RoundsUsed,
		RoundNarration: roundNarration(res.Narration),
		Iterations:     history,
		Task:           r.cfg.Task,
		// THE SKILL-GUARD STATE, which the field declared and nothing ever
		// wrote. With it empty, a resumed executor was told to load the
		// skills it had already loaded — the bodies were in the very
		// transcript it was re-entering — and every required tool it had
		// unlocked before the suspend was refused again.
		LoadedSkills: r.loadedSkills(),
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

// loadedSkills is what the running phase's guard has unlocked, or nil where
// this turn arms none.
func (r *Runner) loadedSkills() []string {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	return guard.LoadedKeys()
}
