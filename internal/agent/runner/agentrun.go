package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/tools"
)

// The executor, run by somebody else's agent loop.
//
// # What agent mode changes, and what it deliberately does not
//
// In text mode the engine's tool loop is the agency: it prompts, the model
// answers with tool calls, the engine runs them, and everything a reader knows
// about the turn comes from that loop. In agent mode a coding CLI runs the
// executor itself — its own loop, its own shell — and the seat's tools reach
// it over the MCP bridge, which dispatches through the SAME tools.Surface a
// native loop would call.
//
// So what changes here is only how the executor's rounds happen. Everything
// around them is untouched, and each of those is a deliberate refusal to fork:
//
//   - The PROMPT is the same one. An agent-mode executor is the same seat with
//     the same catalogue, memory, skills and workers; a second prompt builder
//     would drift on the half nobody re-reads.
//   - The SURFACE is the same one, built the same way, with the same skill
//     guard. The bridge executes against it, so a tool denied natively is
//     denied there.
//   - The SUBMISSION is the same tool. The CLI ends its run by calling
//     submit_work over the bridge, exactly as a native loop ends by calling it
//     locally, so the outcome vocabulary and the rescue path are shared.
//   - The REVIEWER is unchanged and still native. The point of a reviewer is
//     that it is not the thing being reviewed.
//
// # Why it suspends
//
// An agentic run outlives its turn — that is what makes it worth doing — so it
// takes the detached shape the sandbox layer already has: the phase returns
// suspended, the row carries what a resume needs, and a completion re-enters
// the same turn, possibly in another process on another node. What it does NOT
// carry is a conversation: there is no engine loop to re-enter, which is what
// [execstate.State.AgentRun] marks and what [Runner.resumeAgentRun] reads.

// AgentLauncher starts the executor as somebody else's agentic run.
//
// Declared by the consumer, and narrow on purpose: the runner knows what the
// executor should be asked to do and which tools it may use; the engine knows
// what a detached run is, where it goes and how to reach it. Neither has to
// learn the other's half.
type AgentLauncher interface {
	// LaunchExecutor starts the run and returns once it is going. It does
	// NOT wait for it: the phase suspends and a completion re-enters the
	// turn later.
	//
	// An error fails the phase rather than falling back to a native loop.
	// A seat configured for agent mode whose run cannot start has a
	// configuration problem, and quietly running the executor a different
	// way would hide it behind a turn that merely looks expensive.
	LaunchExecutor(ctx context.Context, req AgentRunRequest) error
}

// AgentRunRequest is one executor handed to an outside agent loop.
type AgentRunRequest struct {
	// Brief is the whole executor prompt — the system message and the
	// turn's own ask, rendered as one document, because a CLI takes one
	// prompt and has no system-message channel of its own.
	Brief string

	// Surface is what the run may call, bridged. The SAME object a native
	// loop would execute against; see the package rule above.
	Surface *tools.Surface

	// Round is the turn iteration this executor is, so the run's record and
	// the resumed phase agree about which round they are in.
	Round int
}

// executeAsAgentRun runs the executor as a detached agentic run.
//
// It builds the same surface and the same prompt a native pass would, hands
// them to the launcher, and suspends. The suspension it records carries no
// conversation — see [execstate.State.AgentRun] — but everything else a resume
// needs: the round, the ledger, the surface it may still use, and the skills
// it had loaded.
func (r *Runner) executeAsAgentRun(ctx context.Context, round int, notes string,
	history []ledger.Iteration,
) (turn.Work, turn.Surface, error) {
	snapshot := r.cfg.Registry.Snapshot()

	// The submission tool is on the surface for the same reason it is in a
	// native pass: it is how the executor says what it did. The CLI calls
	// it over the bridge, and the value it captures here is read only when
	// the resume happens in THIS process — otherwise the resume rebuilds it
	// from the run's durable bridged-call log.
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return calls(surface) },
			func() turn.Surface { return describe(surface) }))

	built, err := r.surfaceWith(ctx, phase.Execute, round, snapshot, submit,
		r.executorActive(snapshot))
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	system, user := r.executorPrompt(round, notes, history, snapshot)
	// THE PROMPT REACHES THE RECORD, published here because nothing else
	// will: a native pass publishes it from inside runPhase, which agent
	// mode does not enter, and the resume's own record deliberately carries
	// no prompt (it re-entered a phase rather than opening one). Without
	// this an operator reading an agent-mode turn sees a run that finished
	// and no sign of what it was asked.
	r.emitter().started(ctx, phase.Execute, round, system, user)

	if err := r.cfg.AgentRun.LaunchExecutor(ctx, AgentRunRequest{
		Brief:   system + "\n\n" + user,
		Surface: surface,
		Round:   round,
	}); err != nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("launching the agent-mode executor: %w", err)
	}

	r.recordAgentSuspension(round, surface, history)
	return turn.Work{Calls: calls(surface), Suspended: true}, describe(surface), nil
}

// recordAgentSuspension captures what a resumed agent run needs.
//
// The mirror of [Runner.recordSuspension], and separate rather than a branch
// inside it because the two states are shaped differently: that one is a
// conversation with a dangling call, this one is deliberately not a
// conversation at all. A single function taking a flag would have every field
// mean something in one mode and nothing in the other.
func (r *Runner) recordAgentSuspension(round int, surface *tools.Surface, history []ledger.Iteration) {
	state := execstate.State{
		Version:      execstate.Version,
		AgentRun:     true,
		ActiveTools:  surface.Active(),
		LoadedSkills: r.loadedSkills(),
		Round:        round,
		Iterations:   history,
		Task:         r.cfg.Task,
	}
	if err := state.Validate(); err != nil {
		// Not recorded, and the absence is what the engine reports: a run
		// whose state cannot be written is one nothing can resume, and it
		// must fail while the launch is still in this process's hands.
		log.Error("agent_run_suspension_invalid", "round", round, "error", err.Error())
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspension = &Suspension{State: state}
}

// resumeAgentRun turns a finished agentic run into the turn's Work.
//
// THE RUN'S OWN TOOL CALLS ARE THE RECORD, replayed from the durable log the
// bridge wrote rather than from the surface: the process that resumes may not
// be the one that launched, so the surface here is fresh and remembers
// nothing. That log is also where the submission is: the CLI ended its run by
// calling submit_work over the bridge, and replaying that call through a fresh
// submission tool is what recovers the outcome it declared.
//
// An absent submission is NOT a value — the same rule a native pass follows.
// A run that stopped without saying what it did is rescued as incomplete and
// judged on its record, rather than having a delivery inferred from the prose
// the CLI happened to end with.
func (r *Runner) resumeAgentRun(ctx context.Context, state execstate.State,
	answer string, bridged []ledger.Call,
) (turn.Work, turn.Surface, error) {
	snapshot := r.cfg.Registry.Snapshot()

	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return bridged },
			func() turn.Surface { return describe(surface) }))

	built, err := r.surfaceWith(ctx, phase.Execute, state.Round, snapshot, submit,
		state.ActiveTools, state.LoadedSkills...)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	replaySubmission(ctx, submit, bridged)

	work, described, err := r.finishWork(ctx, state.Round, work{
		submit:   submit,
		res:      agentRunResult(answer, bridged),
		surface:  surface,
		snapshot: snapshot,
		run:      r.cfg.Resume.Run,
	})
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	// The RUN's calls, not this process's surface's — see the doc above.
	work.Calls = bridged
	return work, described, nil
}

// agentRunResult turns what the run actually did into the phase's record.
//
// This used to hand `finishWork` a zero [toolloop.Result] while holding the
// whole record in `bridged`, so the published event carried no response, no
// tool calls, no tokens, no rounds and no model. The card rendered EMPTY: with
// nothing in the ledger and nothing in the joined response there is no
// transcript to fall back on, and the "composing its first round" placeholder
// is gated on a phase being live, which a completed one is not. An operator
// saw a decision word and a blank card for a run that made real tool calls.
//
// ONE ROUND, and that is not a stand-in for a number the engine failed to
// count — it is the honest one. A round is an iteration of the ENGINE's tool
// loop: one request, one answer. In agent mode the engine made a single
// request — the launch — and got a single answer back, however many turns the
// CLI took inside its own loop to produce it. The bridge's call log carries no
// round of its own and inventing one per call would claim a structure nobody
// observed, splitting one run into thirty rounds of one call each.
func agentRunResult(answer string, bridged []ledger.Call) phaseResult {
	res := toolloop.Result{Text: answer, RoundsUsed: 1}
	for _, call := range bridged {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: 1, Name: call.Name, Args: call.Args,
			Output: call.Result, Failed: call.Failed,
		})
	}
	if strings.TrimSpace(answer) != "" {
		// The run's own last word, on the round its calls are on, so the
		// ledger reads as one block rather than as prose beside orphaned
		// tool rows. There is no separate reasoning to report: a CLI's
		// thinking stays inside its own loop.
		res.Narration = []toolloop.Narration{{Round: 1, Content: answer}}
	}
	return phaseResult{Text: answer, Rounds: 1, Result: res}
}

// replaySubmission feeds the run's own submit_work call back through a fresh
// submission tool, so the outcome the CLI declared is the one the turn reports.
//
// LAST WRITE WINS, which is the submission contract everywhere else: a second
// call is a correction, and replaying in order is what makes that true here
// too. A call the decoder rejects is skipped rather than failing the resume —
// the run happened, its other calls happened, and a malformed submission is
// exactly the "said nothing" case the rescue path exists for.
func replaySubmission(ctx context.Context, submit *structured.Tool[workPayload], bridged []ledger.Call) {
	for _, call := range bridged {
		if call.Name != SubmitWorkTool || call.Failed {
			continue
		}
		if _, err := submit.Call(ctx, call.Args); err != nil {
			log.WarnContext(ctx, "agent_run_submission_replay_failed", "error", err.Error())
		}
	}
}
