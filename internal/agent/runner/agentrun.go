package runner

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// The executor, run detached in a sandbox box.
//
// # What box mode changes, and what it deliberately does not
//
// In both modes a coding CLI runs the executor's loop — it asks its model,
// calls the seat's tools, asks again — and every call is dispatched through the
// SAME tools.Surface. What differs is where the loop runs and who governs it.
// In host mode the CLI runs on this host, its tools served over the phase's
// tool session by the engine's own tool loop, which holds every call to the
// fence, the budget, the round cap and the person steering the turn, run by
// run (internal/agent/toolloop). In box mode the CLI runs in a sandbox box —
// with its own shell, editor and checkout — reaching the seat's tools over the
// MCP bridge, and the engine launches it, lets it go, and collects what it did.
//
// So what changes here is only where the executor's rounds happen and what the
// engine sees of them. Everything around them is untouched, and each of those
// is a deliberate refusal to fork:
//
//   - The PROMPT is the same one. A box-mode executor is the same seat with
//     the same catalogue, memory, skills and workers; a second prompt builder
//     would drift on the half nobody re-reads.
//   - The SURFACE is the same one, built the same way, with the same skill
//     guard. The bridge executes against it, so a tool denied on the host is
//     denied there.
//   - The SUBMISSION is the same tool. The CLI ends its run by calling
//     submit_work over the bridge, exactly as a host-mode run ends by calling
//     it over its session, so the outcome vocabulary and the rescue path are
//     shared.
//   - The REVIEWER is unchanged and runs on the host. The point of a reviewer
//     is that it is not the thing being reviewed.
//
// # Why it suspends
//
// A box run outlives its turn — that is what makes it worth doing — so it
// takes the detached shape the sandbox layer already has: the phase returns
// suspended, the row carries what a resume needs, and a completion re-enters
// the same turn, possibly in another process on another node. What it does NOT
// carry is a conversation: the engine's loop never drove this one, so there is
// nothing to re-enter, which is what [execstate.State.AgentRun] marks and what
// [Runner.resumeAgentRun] reads.

// AgentLauncher starts the executor as a box-mode run: its coding CLI's own
// loop, detached in a sandbox box.
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
	// An error fails the phase rather than falling back to a run on the
	// host. A seat configured for box mode whose run cannot start has a
	// configuration problem, and quietly running the executor a different
	// way would hide it behind a turn that merely looks expensive.
	LaunchExecutor(ctx context.Context, req AgentRunRequest) error
}

// AgentRunRequest is one executor handed to a box-mode run.
//
// THE PROMPT ARRIVES IN THE TWO HALVES a host-mode pass sends it in, because
// the CLIs that run in a box take it in two: Claude Code appends a file to
// its own system prompt, OpenCode adds an instruction file to its default
// agent's. Both keep the vendor's harness prompt — the part that teaches its
// shell, editor and loop, which a box is FOR — and put the seat's standing
// instructions on the system side of the conversation beneath it.
// Joined into one document, as they once were, the seat's identity, policies
// and memory reached the model as the first user message, which a model weighs
// as a request rather than as who it is.
type AgentRunRequest struct {
	// System is the executor's system prompt: the seat's standing
	// instructions, exactly as a host-mode pass sends them as its system
	// message.
	System string

	// Brief is the turn's own ask — the user message a host-mode pass sends
	// beside System — and the whole of what the CLI is prompted with.
	Brief string

	// Surface is what the run may call, bridged. The SAME object a host-mode
	// run's session would execute against; see the package rule above.
	Surface *tools.Surface

	// Spend is what the calls Surface serves cost the engine, counted
	// apart from this segment because they happen after it — see
	// [BridgedSpend]. The launcher records its running total with every
	// bridged call, for the segment that resumes from the run to pay.
	Spend *BridgedSpend

	// Round is the turn iteration this executor is, so the run's record and
	// the resumed phase agree about which round they are in.
	Round int
}

// executeAsAgentRun runs the executor as a detached box-mode run.
//
// It builds the same surface and the same prompt a host-mode pass would, hands
// them to the launcher, and suspends. The suspension it records carries no
// conversation — see [execstate.State.AgentRun] — but everything else a resume
// needs: the round, the ledger, the surface it may still use, and the skills
// it had loaded.
func (r *Runner) executeAsAgentRun(ctx context.Context, round int, notes string,
	history []ledger.Iteration,
) (turn.Work, turn.Surface, error) {
	snapshot := r.executorUniverse(r.cfg.Registry.Snapshot(), true)

	// The submission tool is on the surface for the same reason it is in a
	// host-mode pass: it is how the executor says what it did. The CLI calls
	// it over the bridge, which records the call in the run's durable log,
	// and the resume reads the outcome from THERE — [Runner.resumeAgentRun]
	// replays the logged calls through a fresh submission tool every time,
	// in whichever process collects the run, this one included. So the
	// value this tool captures is never read: what it does is VALIDATE each
	// submission as it is made, so the CLI's model is told about one it can
	// correct while it is still running.
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return turn.Record(history, calls(surface)) },
			func() turn.Surface { return describe(surface) }))

	// ITS OWN METER: every call this surface serves is the CLI's, made
	// over the bridge after this segment has suspended and charged what it
	// spent — see [BridgedSpend].
	bridged := NewBridgedSpend()
	built, err := r.surfaceWith(ctx, r.meterFor(bridged), phase.Execute, round, snapshot, submit,
		executorActive(snapshot))
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	system, user := r.executorPrompt(ctx, round, notes, history, snapshot, built)
	// AND ASKED AGAIN once the brief exists, before anything is on the
	// record: assembling it rewrites the prior rounds' payloads past their
	// budgets with the seat's auxiliary model, charged to the same counters,
	// and the rewrite can be what fills the window. Asked only at the top of
	// the phase, that run was launched and paid for whole past the ceiling.
	if err := toolloop.Refusal(ctx, r.cfg.Budget); err != nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, err)
	}
	// THE PROMPT REACHES THE RECORD, published here because nothing else
	// will: a host-mode pass publishes it from inside runPhase, which box
	// mode does not enter, and the resume's own record deliberately carries
	// no prompt (it re-entered a phase rather than opening one). Without
	// this an operator reading a box-mode turn sees a run that finished and
	// no sign of what it was asked.
	//
	// NO SEED — this opens a phase rather than re-entering one — and the
	// surface, whose definitions are NOT in the prompt below: they reach the
	// CLI over the MCP bridge, where the CLI's own model is billed for
	// every one of them. See [emitter.promptSize].
	//
	// NO ROUND CAPS: the rounds are the CLI's, inside its own loop, and
	// the engine enforces no cap on them to state.
	r.emitter().started(ctx, phase.Execute, round, system, user, nil, surface, roundCaps{})

	if err := r.cfg.AgentRun.LaunchExecutor(ctx, AgentRunRequest{
		System:  system.Text,
		Brief:   user.Text,
		Surface: surface,
		Spend:   bridged,
		Round:   round,
	}); err != nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("launching the box-mode executor: %w", err)
	}

	r.recordAgentSuspension(round, surface, history)
	return turn.Work{Calls: calls(surface), Suspended: true}, describe(surface), nil
}

// recordAgentSuspension captures what a resumed box run needs.
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

// ReopenAgentRun rebuilds the tool surface a box-mode run's box is still
// calling, for a node that has taken the run over while it works — this
// process after a restart, or the seat's next holder — so the run's bridge
// session can be opened again (internal/api/mcpbridge). It returns the
// surface, and the meter the session's calls are counted on.
//
// FROM THE RUN'S OWN STATE, as [Runner.Resume] rebuilds a parked executor:
// the surface its launch recorded — its active tools and the skills it had
// loaded — and then what the run did to it since, read off the calls it made
// over the bridge, every tool it activated and every skill it loaded. Rebuilt
// from the launch's record alone, the box would find tools it had activated
// gone and the tools a loaded skill unlocked refused again.
//
// AND ITS PLACE IN THE RUN. The turn's call log is seeded with its closed
// rounds and every call the run has made over the bridge, so a call made from
// here counts its repeats from the whole run and derives the operation id the
// run's own process would have; a submission cites those calls as well as the
// ones made from here; and the meter starts from what the row says the run's
// bridged calls have cost so far ([NewBridgedSpendFrom]). spent is that total
// and bridged the row's calls, in any order.
//
// It launches nothing and suspends nothing: the run is going already, and its
// collection resumes the turn as it would have. A state that is not a box-mode
// run's is refused — a host-mode executor parked on a detached run has no box
// calling it.
func (r *Runner) ReopenAgentRun(ctx context.Context, state execstate.State,
	bridged []BridgedCall, spent Bridged,
) (*tools.Surface, *BridgedSpend, error) {
	if !state.AgentRun {
		return nil, nil, fmt.Errorf("runner: reopening a box-mode run: the parked " +
			"state is a host-mode executor's conversation, which no box is calling")
	}
	history := state.Iterations
	called := bridgedLedger(inAdmissionOrder(bridged))

	snapshot := r.executorUniverse(r.cfg.Registry.Snapshot(), true)
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(called), calls(surface)...))
			},
			func() turn.Surface { return describe(surface) }))

	active := slices.Clone(state.ActiveTools)
	for _, call := range called {
		if call.Name == toolname.Activate && !call.Failed {
			active = append(active, activationTarget(call.Args))
		}
	}
	meter := NewBridgedSpendFrom(spent)
	built, err := r.surfaceWith(ctx, r.meterFor(meter), phase.Execute, state.Round, snapshot, submit,
		active, state.LoadedSkills...)
	if err != nil {
		return nil, nil, err
	}
	surface = built
	// THE RUN'S CALL LOG STARTS FROM WHAT IT ALREADY CALLED, through the
	// surface it is served ([seedCalls]).
	seedCalls(r.cfg.Turn.Context.CallLog(), surface, history, called)

	// THE SKILLS IT LOADED, told to the guard the surface was just armed
	// with, by the guard's own reading of a call — the one rule for which
	// call unlocks what. After the surface is built, because whether a guard
	// is armed at all depends on the loader being active, which an
	// activation over the bridge may be what made it.
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	for _, call := range called {
		if !call.Failed {
			guard.Observe(call.Name, surface.ArgsOf(call.Name, call.Args))
		}
	}
	return surface, meter, nil
}

// resumeAgentRun turns a finished box run into the turn's Work.
//
// THE RUN'S OWN TOOL CALLS ARE THE RECORD, replayed from the durable log the
// bridge wrote rather than from the surface: the process that resumes may not
// be the one that launched, so the surface here is fresh and remembers
// nothing. That log is also where the submission is: the CLI ended its run by
// calling submit_work over the bridge, and replaying that call through a fresh
// submission tool is what recovers the outcome it declared.
//
// An absent submission is NOT a value — the same rule a host-mode pass follows.
// A run that stopped without saying what it did is rescued as incomplete and
// judged on its record, rather than having a delivery inferred from the prose
// the CLI happened to end with.
//
// `history` is the turn's closed rounds, which its submission's citations read
// beside the run's own calls — see [turn.Record].
func (r *Runner) resumeAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, answer string, run []BridgedCall,
) (turn.Work, turn.Surface, error) {
	snapshot := r.executorUniverse(r.cfg.Registry.Snapshot(), true)
	run = inAdmissionOrder(run)
	bridged := bridgedLedger(run)

	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return turn.Record(history, bridged) },
			func() turn.Surface { return describe(surface) }))

	built, err := r.surfaceWith(ctx, r.ownMeter(), phase.Execute, state.Round, snapshot, submit,
		state.ActiveTools, state.LoadedSkills...)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built
	// THE RUN'S CALL LOG STARTS FROM WHAT THE RUN ALREADY CALLED, through
	// the surface it resumes on ([seedCalls]); see [Runner.Resume].
	seedCalls(r.cfg.Turn.Context.CallLog(), surface, history, parkedCalls(state), bridged)

	replaySubmission(ctx, submit, bridged)

	work, described, err := r.finishWork(ctx, state.Round, work{
		submit:   submit,
		res:      agentRunResult(answer, run),
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
// tool calls, no tokens, no rounds and no model. The card rendered EMPTY: it
// draws a phase from its rounds, a record with nothing in its ledger has none
// to draw, and the "composing its first round" placeholder is gated on a phase
// being live, which a completed one is not. An operator
// saw a decision word and a blank card for a run that made real tool calls.
//
// ONE ROUND, and that is not a stand-in for a number the engine failed to
// count — it is the honest one. A round is a model request the engine
// observed: a host-mode run's CLI reports each one as it makes it, and one
// that reports none is counted a round per run. A box run is the second kind,
// seen from further off still: the engine launched it and got a single answer
// back, however many requests the CLI made inside its box to produce it. The
// bridge's call log carries no round of its own and inventing one per call
// would claim a structure nobody observed, splitting one run into thirty
// rounds of one call each.
//
// EACH CALL WITH ITS TIMING AND WHO ANSWERED IT, as the bridge recorded them,
// in the order the run made them: a box-mode run's calls were the only ones a
// phase card could not place on a timeline or name a server for.
func agentRunResult(answer string, bridged []BridgedCall) phaseResult {
	res := toolloop.Result{Text: answer, RoundsUsed: 1}
	for _, call := range bridged {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: 1, Name: call.Name, Args: call.Args,
			Output: call.Result, Failed: call.Failed,
			StartedAt: call.StartedAt, Duration: call.Duration,
			Origin: call.Origin, Server: call.Server,
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

// inAdmissionOrder is a run's bridged calls in the order the bridge admitted
// them — the order the CLI made them in — where the row holds them in the
// order they FINISHED: each is appended once it returns, and a CLI that makes
// several calls at once finishes them in whatever order their tools take. The
// record states the order the run acted in, and the replay below reads a
// submission made after another as the correction, whichever returned first.
// Stable, so calls the row carries no place for keep the row's order.
func inAdmissionOrder(bridged []BridgedCall) []BridgedCall {
	out := slices.Clone(bridged)
	slices.SortStableFunc(out, func(a, b BridgedCall) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// replaySubmission feeds the run's own submit_work call back through a fresh
// submission tool, so the outcome the CLI declared is the one the turn reports.
//
// LAST WRITE WINS, which is the submission contract everywhere else: a second
// call is a correction, and replaying in the order the run made its calls is
// what makes that true here too. A call the decoder rejects is skipped rather than failing the resume —
// the run happened, its other calls happened, and a malformed submission is
// exactly the "said nothing" case the rescue path exists for.
func replaySubmission(ctx context.Context, submit *structured.Tool[workPayload], bridged []ledger.Call) {
	for _, call := range bridged {
		if call.Name != SubmitWorkTool || call.Failed {
			continue
		}
		if _, _, err := tools.Invoke(ctx, tools.Invocation{Name: SubmitWorkTool, Tool: submit,
			Args: tools.ArgsOf(submit, false, call.Args), Where: "replay"}); err != nil {
			log.WarnContext(ctx, "agent_run_submission_replay_failed", "error", err.Error())
		}
	}
}
