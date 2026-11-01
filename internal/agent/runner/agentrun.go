package runner

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// The executor, run by its coding CLI's own agentic loop.
//
// # What is the CLI's, and what stays the engine's
//
// Every seat's executor is its coding CLI's own run, in a box: its own loop,
// its own shell, its own edits and commits. The seat's tools reach it over the
// MCP bridge, which dispatches through the SAME tools.Surface every
// engine-driven phase calls. So what the CLI owns is how the executor's work
// happens; everything around it stays the engine's, and each of those is a
// deliberate refusal to fork:
//
//   - The PROMPT is the seat's: identity, chart, memory, skills and workers,
//     built once here and handed to the CLI as an appended system prompt and
//     the brief it runs.
//   - The SURFACE is built the same way as every phase's, with the same skill
//     guard. The bridge executes against it, so a tool denied to the seat is
//     denied to its run.
//   - The SUBMISSION is the same tool. The CLI ends its run by calling
//     submit_work over the bridge, so the outcome vocabulary and the rescue
//     path are the engine's.
//   - The REVIEWER is the engine's own loop. The point of a reviewer is that
//     it is not the thing being reviewed.
//
// # Why the turn parks
//
// An agentic run outlives its turn — that is what makes it worth doing — so it
// is detached: the phase returns suspended, the row carries what a resume
// needs, and the run's collection re-enters the same turn, possibly in another
// process on another node. What it does NOT carry is a conversation: there is
// no engine loop to re-enter, which is why [execstate.State] holds the turn's
// bookkeeping and nothing a model said — and the context the turn froze at its
// start, since a turn sent back for another iteration launches its next run
// from the segment that collected this one.

// AgentLauncher starts the executor as its coding CLI's own agentic run.
//
// Declared by the consumer, and narrow on purpose: the runner knows what the
// executor should be asked to do and which tools it may use; the engine knows
// what a detached run is, which member of the seat's executor chain takes it,
// where it goes and how to reach it. Neither has to learn the other's half.
type AgentLauncher interface {
	// LaunchExecutor starts the run and returns once it is going, naming
	// the job it opened. It does NOT wait for it: the phase suspends and the
	// run's collection re-enters the turn later.
	//
	// An error fails the phase: there is no other way to run the executor,
	// and the error says why this run could not start. It still names the
	// job where the launch had opened one before it failed — see
	// [Launched.LaunchID].
	LaunchExecutor(ctx context.Context, req AgentRunRequest) (Launched, error)
}

// Launched is what a launch answers.
type Launched struct {
	// LaunchID is the JOB the launch opened — the name the run's record
	// holds it under, which only the launch learns — so the notes its brief
	// carried are recorded delivered to that job.
	//
	// SET ON AN ERROR TOO, where the launch failed after the write that
	// opened the job: that write recorded the brief's notes read by the job
	// ([AgentRunRequest.Notes]), and the record, whose ending reports every
	// note no job ran with, answers for them from then on — so they are not
	// handed back to the turn's source ([SteerSource.Return]), which would
	// report them a second time. Empty with an error is a launch that wrote
	// nothing, whose notes are the turn's still.
	LaunchID string
}

// AgentRunRequest is one executor handed to its coding CLI's agent loop.
//
// THE PROMPT ARRIVES IN TWO HALVES, because the CLIs take it in two: Claude
// Code appends a file to its own system prompt, OpenCode adds an instruction
// file to its default agent's. Both keep the vendor's harness prompt — the
// part that teaches its shell, editor and loop, which the run is FOR — and put
// the seat's standing instructions on the system side of the conversation
// beneath it. Joined into one document, as they once were, the seat's
// identity, policies and memory reached the model as the first user message,
// which a model weighs as a request rather than as who it is.
type AgentRunRequest struct {
	// System is the executor's system prompt: the seat's standing
	// instructions.
	System string

	// Brief is the turn's own ask — with its earlier rounds, every note a
	// person sent the turn that it has already read, and then Notes — and
	// the whole of what the CLI is prompted with.
	Brief string

	// Notes are the notes this brief is the FIRST to read, in the order the
	// brief renders them ([SteerSource.Brief]): the launch records them read
	// by the job it opens, in the write that opens it, so the job's own CLI
	// never takes them again.
	Notes []steer.Note

	// Surface is what the run may call, bridged; see the package rule
	// above.
	Surface *tools.Surface

	// Spend is what the calls Surface serves cost the engine, counted
	// apart from this segment because they happen after it — see
	// [BridgedSpend]. The launcher records its running total with every
	// bridged call, for the segment that resumes from the run to pay.
	Spend *BridgedSpend

	// Round is the turn iteration this executor is, so the run's record and
	// the resumed phase agree about which round they are in.
	Round int

	// OnFallback publishes a launch-time hand-off against THIS turn: the
	// launch walks the seat's executor chain for a member that can take the
	// run ([chain.Pick]), and each member it skips is a provider_fallback
	// like a call's. The launcher has no publisher of its own and must not
	// grow one — which turn, phase and iteration a hand-off belongs to is
	// the runner's to say — so the runner hands it this, bound to the
	// launch. Never nil.
	OnFallback func(chain.Fallback)

	// Continue is set when this launch CARRIES ON a run that parked on a
	// person's question, now answered, or whose job was lost with its host —
	// the same round, relaunched: the asking run's CLI session, and the
	// prompt that continues it, which the launch uses where it runs in the
	// asking run's box (a lost job's session is never continued, and names
	// none). Brief is then what a fresh run is told instead, the question and
	// the answer, or how the run before it was cut off, included — see
	// [Asked] and [Lost]. Nil for every other launch.
	Continue *Continuation

	// Fence is the turn's own fence ([Config.Fence]), which the launch asks
	// again immediately before it opens anything: the prompt built since
	// the phase opened can take a model call of its own, and a seat that
	// moved or was stopped meanwhile must not still buy a box. THE SAME
	// CLOSURE, never one the launcher builds: a fence compares the grant it
	// was built under with the one held when it is called, so it is the
	// turn's admission that the launch is held to. Nil where the runner was
	// built without one.
	Fence func() error
}

// Continuation is how a launch carries on an asking run — see
// [AgentRunRequest.Continue].
type Continuation struct {
	// SessionID is the asking run's CLI session; Brief its next prompt —
	// the answer, and the notes [AgentRunRequest.Notes] carries: only those,
	// because the session already holds every note it read before it asked.
	SessionID string
	Brief     string

	// SignInReplaced is a launch carrying on a round whose job was refused
	// on a sign-in replaced while it ran ([Rotated]): the launch records it
	// on the new job, so that job is not carried on again for it.
	SignInReplaced bool
}

// executeAsAgentRun launches the executor as a detached agentic run.
//
// It builds the surface and the prompt, hands them to the launcher, and
// parks. The state it records carries no conversation — see [execstate.State]
// — but everything a resume needs: the round, the ledger, the surface it may
// still use, and the skills it had loaded.
//
// A LAUNCH THAT DOES NOT HAPPEN STILL CLOSES ITS PHASE. The started record is
// published before the launch, so the record carries the prompt the run was
// sent; a launch refused after that publishes the phase's completion too —
// failed, or stopped where a person stopped it — or the phase reads as still
// running for ever on every screen that draws it.
func (r *Runner) executeAsAgentRun(ctx context.Context, round int, notes string,
	history []ledger.Iteration,
) (turn.Work, turn.Surface, error) {
	snapshot := r.executorUniverse(r.seatSnapshot())

	// The submission tool is on the surface because it is how the executor
	// says what it did. The CLI calls it over the bridge, and the value it
	// captures here is read only when the resume happens in THIS process —
	// otherwise the resume rebuilds it from the run's durable bridged-call
	// log.
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

	// THE NOTES THIS BRIEF IS THE FIRST TO READ, after every one the turn
	// read before — and the launch's from here: handed back on every way out
	// that reaches no run's record ([Runner.steerReturn]), and recorded
	// delivered only once the launch has opened its job ([Runner.steerBriefed]).
	fresh := r.steerBrief(ctx)
	system, user := r.executorPrompt(ctx, notes, history, snapshot, built, fresh)
	// AND ASKED AGAIN once the brief exists, before anything is on the
	// record: assembling it rewrites the prior rounds' payloads past their
	// budgets with the seat's auxiliary model, charged to the same counters,
	// and the rewrite can be what fills the window. Asked only at the top of
	// the phase, that run was launched and paid for whole past the ceiling.
	if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, refused)
	}
	// THE PROMPT REACHES THE RECORD, published here because nothing else
	// will: the collection's own record deliberately carries no prompt (it
	// collects a run rather than opening a conversation). Without this an
	// operator reading a turn sees a run that finished and no sign of what
	// it was asked.
	//
	// The surface travels too, whose definitions are NOT in the prompt
	// below: they reach the CLI over the MCP bridge, where the CLI's own
	// model is billed for every one of them. See [emitter.promptSize].
	//
	// ONE ROUND OF ONE, the cap the collected record states too: the
	// engine's whole exchange with the run is this launch and its answer,
	// and the CLI's own turns inside it are bounded by the run's turn cap
	// (`max_turns`) rather than by anything the engine states here. See
	// [agentRunResult].
	emit := r.emitter()
	began := time.Now()
	caps := roundCaps{max: runRound, ceiling: runRound}
	emit.started(ctx, phase.Execute, round, system, user, surface, caps)

	launched, err := r.cfg.Launcher.LaunchExecutor(ctx, AgentRunRequest{
		System:     system.Text,
		Brief:      user.Text,
		Notes:      fresh,
		Surface:    surface,
		Spend:      bridged,
		Round:      round,
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, phase.Execute, round, f) },
		Fence:      r.cfg.Fence,
	})
	if err != nil {
		r.steerReturn(ctx, launched, fresh)
		err = fmt.Errorf("runner: %s: launching the executor's run: %w", phase.Execute, err)
		// A PHASE A PERSON STOPPED DID NOT FAIL, exactly as for every phase
		// the engine drives: it carries the stop as its error and is not
		// marked failed, since a turn somebody deliberately ended is not one
		// that broke.
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: round, System: system, User: user,
			Available: surface.Active(),
			Elapsed:   time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: !turn.Stopped(err), Err: err,
		})
		return turn.Work{}, describe(surface), err
	}
	r.steerBriefed(ctx, round, launched.LaunchID, fresh)

	r.recordAgentSuspension(round, surface, history, began, time.Since(began))
	return turn.Work{Calls: calls(surface), Suspended: true}, describe(surface), nil
}

// continueAgentRun carries on a run that parked on a person's question, now
// answered ([Asked]), or whose job was lost with its host ([Lost]): the SAME
// round, relaunched rather than collected — in the run's box, continuing the
// asking run's own conversation, where that box is still there, and as a fresh
// run told what happened where it is not, or where the conversation was cut
// off. The launch decides which, once it knows which box it has.
//
// A ROUND THAT MAY NOT BE CARRIED ON AGAIN ([Lost.Limit]) is not relaunched:
// its phase is closed here as failed, saying why, and the turn ends with it.
//
// THE ROUND'S SURFACE AND CLOCK CARRY ON. The surface is the asking run's as
// it left it — the tools its launch had active, the skills the turn had
// loaded, and what its bridged calls did to it since: every tool it activated
// and every skill it loaded, which a run continuing its own session goes on
// calling — and its calls so far are seeded into the turn's call log through
// that surface before the relaunch, so the relaunched run's writes go on
// counting from them: the one rebuild every segment that re-enters a run
// makes ([Runner.rebuiltSurface]). The round's launch time keeps accumulating
// from the instant the round first launched. No new phase is opened: the
// round's started record was published when it first launched, and its
// completion is the collection that finally closes it — unless this relaunch
// is refused, which closes it here.
func (r *Runner) continueAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, carry carried, logged []ledger.Call,
) (turn.Work, turn.Surface, error) {
	began := time.Now()
	snapshot := r.executorUniverse(r.seatSnapshot())
	var surface *tools.Surface
	// A SUBMISSION CITES THE CARRIED RUN'S CALLS TOO: the relaunched run is
	// the same round, and the collection judges its submission against
	// every call the round made ([Runner.resumeAgentRun]) — so the check it
	// gets as it submits reads them as well, or a delivery made before the
	// question, or before the loss, would be refused here and accepted there.
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(logged), calls(surface)...))
			},
			func() turn.Surface { return describe(surface) }))
	bridged := NewBridgedSpend()
	// THE TURN'S CALL LOG STARTS FROM WHAT THE TURN ALREADY CALLED, the
	// carried run's calls included, as the surface is rebuilt and so BEFORE
	// the launch: the relaunched run's bridged calls are admitted against
	// this log from its first one.
	built, err := r.rebuiltSurface(ctx, r.meterFor(bridged), state, snapshot, submit, history, logged)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	emit := r.emitter()
	caps := roundCaps{max: runRound, ceiling: runRound}
	if refused := carry.refusal(); refused != nil {
		// THE ROUND ENDS HERE, and its phase with it: it was opened when the
		// round first launched, and nothing else will close it now.
		refusedErr := fmt.Errorf("runner: %s: %w", phase.Execute, refused)
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: state.Round,
			Available: surface.Active(),
			Elapsed:   time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: true, Err: refusedErr,
		})
		return turn.Work{Calls: logged}, describe(surface), refusedErr
	}

	// THE NOTES WAITING SINCE THE RUN ASKED, which either brief carries:
	// a fresh run's after every note the turn read, as any brief does; the
	// asking run's own session after the answer alone, since that session
	// already holds every note it read.
	fresh := r.steerBrief(ctx)
	system, user := r.executorPrompt(ctx, "", history, snapshot, built, fresh)
	if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, refused)
	}
	// UNDER THE TURN'S OWN FENCE TOKEN, which the relaunched run's tools fence
	// what they read with: a reply carried on is somebody's words, fenced as
	// the message that woke the turn is ([Asked.Continuation]).
	fence := r.cfg.Turn.Context.FenceBoundary()
	continued := carry.continuation(fence)
	if rendered := renderedNotes(fresh); len(rendered) > 0 {
		continued += "\n\n" + strings.Join(rendered, "\n\n")
	}
	launched, err := r.cfg.Launcher.LaunchExecutor(ctx, AgentRunRequest{
		System: system.Text,
		// A FRESH RUN'S BRIEF is the round's own, and what became of the run
		// before it — what it asked and was told, or how it was cut off: it
		// remembers neither.
		Brief: user.Text + "\n\n" + carry.reseed(fence),
		Notes: fresh,
		Continue: &Continuation{
			SessionID: carry.session(), Brief: continued,
			SignInReplaced: isRotated(carry),
		},
		Surface:    surface,
		Spend:      bridged,
		Round:      state.Round,
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, phase.Execute, state.Round, f) },
		Fence:      r.cfg.Fence,
	})
	if err != nil {
		r.steerReturn(ctx, launched, fresh)
		err = fmt.Errorf("runner: %s: %s: %w", phase.Execute, carry.what(), err)
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: state.Round, System: system, User: user,
			Available: surface.Active(),
			Elapsed:   time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: !turn.Stopped(err), Err: err,
		})
		// The asking run's calls, an earlier attempt's: see below.
		return turn.Work{Calls: logged, Inherited: len(logged)}, describe(surface), err
	}
	r.steerBriefed(ctx, state.Round, launched.LaunchID, fresh)
	launchedAt := state.LaunchedAt
	if launchedAt.IsZero() {
		launchedAt = began
	}
	r.recordAgentSuspension(state.Round, surface, history, launchedAt,
		time.Duration(state.LaunchMS)*time.Millisecond+time.Since(began))
	// THE ROUND'S CALLS SO FAR, the run's before it asked or was cut off:
	// they happened and stay on the round's record, as an EARLIER ATTEMPT's
	// ([turn.Work.Inherited]) — the job before made them, and a retry of this
	// continuation relaunches the run rather than making them again.
	return turn.Work{Calls: logged, Inherited: len(logged), Suspended: true}, describe(surface), nil
}

// recordAgentSuspension captures what the run's collection needs.
func (r *Runner) recordAgentSuspension(round int, surface *tools.Surface, history []ledger.Iteration,
	began time.Time, launched time.Duration,
) {
	state := execstate.State{
		Version:      execstate.Version,
		ActiveTools:  surface.Active(),
		LoadedSkills: r.loadedSkills(),
		Round:        round,
		// THE LAUNCH'S HALF OF THE PHASE, and where it began, for the
		// record the collection publishes. See [execstate.State.LaunchMS]
		// and [execstate.State.LaunchedAt].
		LaunchMS:   int(launched / time.Millisecond),
		LaunchedAt: began.UTC(),
		Iterations: history,
		Task:       r.cfg.Task,
		// AND WHAT THE TURN STARTED WITH, for the launches after it. See
		// [execstate.State.Context].
		Context: frozen(r.cfg.Context, r.cfg.Conversation),
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

// ReopenAgentRun rebuilds the tool surface an executor run's box is still
// calling, for a node that has taken the run over while it works — this
// process after a restart, or the seat's next holder — so the run's bridge
// session can be opened again (internal/api/mcpbridge). It returns the
// surface, and the meter the session's calls are counted on.
//
// FROM THE RUN'S OWN STATE, by the one rebuild every segment that re-enters a
// run makes ([Runner.rebuiltSurface]): the surface its launch recorded — its
// active tools and the skills it had loaded — and then what the run did to it
// since, read off the calls it made over the bridge, every tool it activated
// and every skill it loaded. Rebuilt from the launch's record alone, the box
// would find tools it had activated gone and the tools a loaded skill
// unlocked refused again.
//
// AND ITS PLACE IN THE RUN. The turn's call log is seeded with its closed
// rounds and every call the run has made over the bridge, through the surface
// it is served, by canonical arguments, in that same rebuild, so a call made
// from here counts its repeats from the whole run and derives the operation
// id the run's own process would have; a submission cites those calls as
// well as the ones made from here; and the meter starts from what the row
// says the run's bridged calls have cost so far ([NewBridgedSpendFrom]).
// spent is that total and bridged the row's calls, in any order.
//
// It launches nothing and parks nothing: the run is going already, and its
// collection resumes the turn as it would have. THE SUBMISSION IT CAPTURES IS
// NEVER READ, exactly as the launch's is not when the run is collected in
// another process: the collection replays submit_work from the run's durable
// log ([Runner.resumeAgentRun]).
func (r *Runner) ReopenAgentRun(ctx context.Context, state execstate.State,
	bridged []BridgedCall, spent Bridged,
) (*tools.Surface, *BridgedSpend, error) {
	history := state.Iterations
	called := bridgedLedger(inAdmissionOrder(bridged))

	snapshot := r.executorUniverse(r.seatSnapshot())
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(called), calls(surface)...))
			},
			func() turn.Surface { return describe(surface) }))

	meter := NewBridgedSpendFrom(spent)
	built, err := r.rebuiltSurface(ctx, r.meterFor(meter), state, snapshot, submit, history, called)
	if err != nil {
		return nil, nil, err
	}
	surface = built
	return surface, meter, nil
}

// rebuiltSurface is a run's surface rebuilt by a later segment, as the run
// left it, and the turn's call log seeded with the run's place in the turn —
// the one rebuild every segment that re-enters a run makes: the one that
// collects it ([Runner.resumeAgentRun]), one that carries it on
// ([Runner.continueAgentRun]) and one that reopens it while it works
// ([Runner.ReopenAgentRun]).
//
// AS THE RUN LEFT IT: the tools its launch recorded and the skills its guard
// held then, and what the run did to both since over the bridge, read off
// called — every tool a successful activation named ([replayedActive]) and
// every skill it loaded ([Runner.observeLoads]). AND ITS PLACE IN THE TURN:
// the closed rounds in history and the run's calls, seeded into the call log
// through the rebuilt surface ([seedCalls]), so a count read from here sees
// every call made before it.
//
// ONE REBUILD FOR ALL THREE, because they drifted apart when each made its
// own: the collection rebuilt the launch's record alone, on the reasoning that
// it makes no gated call. That much is true, and it is beside the point,
// because the collected surface is also what the round's record states the
// run held (tools_available) and could still have discovered
// (tool_catalogue). Every tool the run had activated over the bridge was
// published as one it never found, and the record contradicted the run's own
// calls listed beside it. The guard is replayed too, although nothing calls
// through the collection's surface: a rebuild that replays half of what the
// run did is the drift this rule exists to stop.
func (r *Runner) rebuiltSurface(ctx context.Context, m meter, state execstate.State,
	snapshot tools.Snapshot, submit tools.Callable, history []ledger.Iteration, called []ledger.Call,
) (*tools.Surface, error) {
	surface, err := r.surfaceWith(ctx, m, phase.Execute, state.Round, snapshot, submit,
		replayedActive(state, called), state.LoadedSkills...)
	if err != nil {
		return nil, err
	}
	seedCalls(r.cfg.Turn.Context.CallLog(), surface, history, called)
	r.observeLoads(surface, called)
	return surface, nil
}

// replayedActive is the tools a run's surface holds once its bridged calls
// are replayed onto the one its launch recorded: those, and every tool a
// successful activation named.
func replayedActive(state execstate.State, called []ledger.Call) []string {
	active := slices.Clone(state.ActiveTools)
	for _, call := range called {
		if call.Name == toolname.Activate && !call.Failed {
			active = append(active, activationTarget(call.Args))
		}
	}
	return active
}

// observeLoads tells the guard the surface was just armed with every skill a
// run's bridged calls loaded, by the guard's own reading of a call — the one
// rule for which call unlocks what. Called once the surface is built, because
// whether a guard is armed at all depends on the loader being active, which an
// activation over the bridge may be what made it. A call is read by its
// canonical arguments, made through the surface ([tools.Surface.ArgsOf]), as
// the guard reads every call the surface admits.
func (r *Runner) observeLoads(surface *tools.Surface, called []ledger.Call) {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	for _, call := range called {
		if !call.Failed {
			guard.Observe(call.Name, surface.ArgsOf(call.Name, call.Args))
		}
	}
}

// frozen is the turn-start context a runner builds every launch's prompt from,
// as a park carries it: the blocks' rendered text and the prior-turns block —
// what [Runner.executorPrompt] reads of them, and nothing else.
func frozen(blocks prefetch.Blocks, conversation string) execstate.Frozen {
	return execstate.Frozen{
		ThreadContext:       blocks.ThreadContext,
		PersonalMemory:      blocks.PersonalMemory,
		RelevantKnowledge:   blocks.RelevantKnowledge,
		EpisodeRecall:       blocks.EpisodeRecall,
		CounterpartyProfile: blocks.CounterpartyProfile,
		SynthesizedSkills:   blocks.SynthesizedSkills,
		Conversation:        conversation,
	}
}

// thawed is a parked turn's frozen context back in the shape a runner's
// prompts read — the inverse of [frozen].
func thawed(f execstate.Frozen) (prefetch.Blocks, string) {
	return prefetch.Blocks{
		ThreadContext:       f.ThreadContext,
		PersonalMemory:      f.PersonalMemory,
		RelevantKnowledge:   f.RelevantKnowledge,
		EpisodeRecall:       f.EpisodeRecall,
		CounterpartyProfile: f.CounterpartyProfile,
		SynthesizedSkills:   f.SynthesizedSkills,
	}, f.Conversation
}

// resumeAgentRun turns a finished agentic run into the turn's Work.
//
// THE RUN'S OWN TOOL CALLS ARE THE RECORD, replayed from the durable log the
// bridge wrote rather than from the surface: the process that resumes may not
// be the one that launched, so the surface here is rebuilt as the run left it
// ([Runner.rebuiltSurface]) and has served no call of its own. That log is
// also where the submission is: the CLI ended its run by calling submit_work
// over the bridge, and replaying that call through a fresh submission tool is
// what recovers the outcome it declared.
//
// An absent submission is NOT a value. A run that stopped without saying what
// it did is rescued as incomplete and judged on its record, rather than having
// a delivery inferred from the prose the CLI happened to end with.
//
// `history` is the turn's closed rounds, which its submission's citations read
// beside the run's own calls — see [turn.Record] — and `bridged` the run's
// calls in the order the bridge admitted them ([inAdmissionOrder]).
func (r *Runner) resumeAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, answer string, bridged []BridgedCall,
) (turn.Work, turn.Surface, error) {
	// THIS SEGMENT'S START, which the collection's record states as its
	// started_at: a start read off neither segment would put the record at
	// the epoch, a day before the turn existed. Its DURATION is the
	// phase's — both segments, the launch's carried on the park — and
	// never the run between them, which is the run's own fact, on the
	// record of the box it ran in. See types.AgentPhaseCompleted.
	began := time.Now()
	snapshot := r.executorUniverse(r.seatSnapshot())
	logged := bridgedLedger(bridged)

	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return turn.Record(history, logged) },
			func() turn.Surface { return describe(surface) }))

	// THE SURFACE THE RUN LEFT, the tools it activated over the bridge
	// included, which the round's record states as what the run held and
	// what it could still have discovered ([Runner.finishWork]); and the
	// turn's call log, which starts from what the turn already called.
	built, err := r.rebuiltSurface(ctx, r.ownMeter(), state, snapshot, submit, history, logged)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	replaySubmission(ctx, submit, bridgedLedger(sinceAnswer(bridged, r.cfg.Resume.Answered)))

	run := r.cfg.Resume.Run
	res := agentRunResult(answer, bridged, run, state.LaunchedAt, began)
	// THE NOTES THE ROUND'S RUN READ, off the record every read is written
	// on: in its briefs, and at the boundaries of its CLI's own loop — the
	// reads of this round's jobs, at the run's one round, each with where it
	// was read.
	res.RunSteers = runReads(r.cfg.Resume.Steered, state.Round)
	res.StartedAt = began.UTC()
	res.Elapsed = time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began)
	work, described := r.finishWork(ctx, state.Round, work{
		submit:   submit,
		res:      res,
		surface:  surface,
		snapshot: snapshot,
		run:      run,
	})
	// The RUN's calls, not this process's surface's — see the doc above —
	// and every one an EARLIER ATTEMPT's ([turn.Work.Inherited]): the run
	// made them before this segment began, and a retry of the collection
	// collects the same run and makes none of them again. They are on the
	// record, for the delivery check and the ledger; what they are not is
	// a write this attempt would repeat. Counted as one, a collection that
	// met its reviewer's usage limit before writing anything was settled
	// for good as a turn that had written, and the run's result never
	// reached the seat.
	work.Calls = logged
	work.Inherited = len(logged)
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
// count — it is the honest one. A round is an iteration of the ENGINE's
// exchange with a model: one request, one answer. For the executor the engine
// made a single request — the launch — and got a single answer back, however
// many turns the CLI took inside its own loop to produce it. The bridge's call
// log carries no round of its own and inventing one per call would claim a
// structure nobody observed, splitting one run into thirty rounds of one call
// each. So the record's cap is that one round too: the engine allowed the
// run one request, and its turns are bounded by the CLI's own cap
// (`max_turns`), which is the run's fact rather than this phase's.
//
// THE ROUND IS TIMED, from the launch (launchedAt, the phase's first instant)
// to the collection (answered, when the run's answer came back), so a
// timeline draws the run as the span the phase waited on and places each
// bridged call inside it at the instant the bridge measured. Two clocks when
// the launch and the collection ran on different nodes, which is why a skew
// that would make the span negative states none. It carries NO TOKENS: what
// the run spent is the run's own fact, on the `sandbox` record its collection
// publishes, and repeating it here would charge every rollup twice.
//
// EACH CALL WITH ITS TIMING AND WHO ANSWERED IT, as the bridge recorded them,
// in the order the run made them: the bridge is where a run's calls are
// measured, since they ran inside the CLI's own loop and not one of the
// engine's.
//
// THE MEMBER THAT RAN IT is the record's entry and model: the launch picked it
// from the seat's executor chain, so the chain's head is not an answer, and a
// record naming nobody files the run's spend under no provider.
func agentRunResult(answer string, bridged []BridgedCall, run RunRecord,
	launchedAt, answered time.Time,
) phaseResult {
	res := toolloop.Result{
		Text: answer, RoundsUsed: runRound, MaxRounds: runRound,
		Model: run.Model, ProviderKey: run.ProviderKey,
	}
	if !launchedAt.IsZero() {
		res.Rounds = []toolloop.Round{{
			Round: runRound, StartedAt: launchedAt.UTC(),
			Duration: max(answered.Sub(launchedAt), 0),
			Model:    run.Model, ToolCalls: len(bridged),
		}}
	}
	for _, call := range bridged {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: runRound, Name: call.Name, Args: call.Args,
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
		res.Narration = []toolloop.Narration{{Round: runRound, Content: answer}}
	}
	return phaseResult{Text: answer, Rounds: runRound, Result: res,
		Caps: roundCaps{max: runRound, ceiling: runRound}}
}

// runReads is the notes the jobs of a collected round read, in the order read,
// as its execute record lists them: at the run's one round ([runRound]), each
// with the boundary that read it. Nil for a round whose jobs read none.
//
// THE ROUND'S JOBS, NOT THE COLLECTED JOB'S ALONE, because the record is the
// round's: a job carried on with a person's answer continues the round of the
// job that asked, and the record lists the asking job's calls and then its own
// as one order (sandbox.OpenedJob.LastSeq). Keyed on the collected job, the
// record drew the asking job's calls and none of the notes it read between
// them, which were delivered all the same. A read of the round's execute phase
// that names a job is one of those jobs': a reviewer's reads are review's, the
// first segment's name no job, and an earlier round's run has a record of its
// own.
func runReads(reads []steer.Read, iteration int) []types.PhaseSteer {
	var out []types.PhaseSteer
	for _, read := range reads {
		if read.Phase != types.PhaseExecute || read.Iteration != iteration || read.Launch == "" {
			continue
		}
		out = append(out, types.PhaseSteer{Round: runRound, NoteID: read.ID, Boundary: read.Boundary})
	}
	return out
}

// runRound is the one round an executor run's record has, which every call
// the run makes is recorded on — see [agentRunResult] for why there is one.
// The workers a run's delegate call spawns nest under it too, so they sit
// under the call that spawned them.
const runRound = 1

// sinceAnswer is the calls of a round made after a person's answer last
// carried it on: those placed after `answered` ([Resume.Answered]), or every
// call where no answer has.
//
// ONLY WHAT THE RUN DID KNOWING THE ANSWER. A run carried on by a person's
// answer keeps the calls it made before the question (ADR-0068), and a
// `submit_work` among them was made without the answer: replayed, it would
// report the carried-on run as the work submitted before it, whether or not
// the run went on to submit again. The cut is the place the row recorded when
// the continuing job opened, never a search for the round's last ask: the log
// is bounded and drops from the middle (sandbox.MaxBridgeCalls), so the ask
// may be gone from it, and the asking job may have called `submit_work` after
// its question as well as before it. A call with no place (zero) is not after
// any answer.
func sinceAnswer(bridged []BridgedCall, answered int) []BridgedCall {
	if answered <= 0 {
		return bridged
	}
	out := make([]BridgedCall, 0, len(bridged))
	for _, call := range bridged {
		if call.Seq > answered {
			out = append(out, call)
		}
	}
	return out
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
//
// The caller hands it the calls a submission may come from: the round's since
// a person's answer last carried it on ([sinceAnswer]).
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
