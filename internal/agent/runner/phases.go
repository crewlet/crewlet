package runner

import (
	"context"
	"fmt"

	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracing"
)

var log = logging.Get("agent.runner")

// Caps are the executor's round budget and its extension ceiling.
//
// The reviewer has no knob of its own: it holds one submission tool, so its
// budget is one round plus the tool loop's own corrective retries. That is a
// structural fact rather than an operator preference, and it used to be
// silently borrowed from the executor's cap — twenty rounds for a phase that
// calls one tool.
type Caps struct {
	ExecutorRounds  int
	ExecutorCeiling int
	ExtensionStep   int
	ExtensionOn     bool
}

// reviewRounds is the reviewer's whole budget: one submission, the tool loop's
// two corrective re-prompts when a model answers without calling it, and one
// spare.
const reviewRounds = 4

// Config is everything a runner needs that does not change between rounds.
type Config struct {
	Seat     prompts.Seat
	Registry *tools.Registry
	Models   *phase.Registry
	Caps     Caps

	// Judge decides round-cap extensions. Nil means every exhaustion goes
	// straight to the rescue path.
	Judge extension.Judge

	// Subagent is what this turn needs to spawn sub-agents: the company's
	// caps and a way to read the seat's remaining allowance. Nil leaves
	// spawn_subagent off every surface, which is the honest shape for a
	// build with no budget source — and was the shape of every build,
	// because nothing imported internal/agent/subagent at all.
	Subagent *SubagentConfig

	// Budget is the shared token counter a turn charges. Nil disables the
	// per-round charge, which is the embedded single-node case where no
	// counter is shared with anyone.
	Budget toolloop.BudgetMeter

	// Task is the ask, and Conversation is the prior-turns block. Both are
	// fixed for the turn.
	Task         string
	Conversation string

	// Reply says who is waiting for this turn, so the executor's own
	// submission can be checked against a fact the model does not control.
	// See [turn.Reply].
	Reply turn.Reply

	// Context is what this seat remembers, what its company has written
	// down, what it has done before, and who it is talking to — rendered
	// by the caller BEFORE the turn.
	//
	// Strings rather than a seam the runner could pull on, and that is the
	// freeze: the executor runs again on a self_iterate loop, and a runner
	// able to re-fetch would produce a different system prompt on each
	// pass. A provider caches on an exact prefix, so a prompt that moves
	// costs the whole prompt again every iteration — and the executor would
	// see its own context change underneath work it is mid-way through.
	// There is nowhere here for a second fetch to happen; a turn that wants
	// fresher knowledge asks for it with search_knowledge.
	Context prefetch.Blocks

	// Skills is the company's tool-skill registry. Nil is a company that
	// has published none, and it disarms the required-skill guard too —
	// which is right: with no skills there is nothing to enforce.
	Skills *skills.Registry

	// SkipNames are the meta-tools the ledger filters out.
	SkipNames []string

	// Publisher receives the phase telemetry — see telemetry.go. Nil
	// publishes nothing, which is the right answer for a runner driven
	// directly by a test and for a sub-agent, whose host phase is already
	// the visible one.
	//
	// This is the ONLY source of every agent_* event in the company: no
	// publisher means a seat that renders as idle for the whole of a turn
	// and leaves no durable record it ran.
	Publisher queue.Publisher

	// Turn identifies the turn these events belong to.
	Turn Turn

	// Onboarding wires the dedicated first-turn pass. Zero disables it,
	// which is what a node with no marker store has — a pass that could
	// never be marked would run every turn forever.
	Onboarding Onboarding

	// Resume re-enters a suspended executor phase. Non-nil makes this
	// runner's turn a RESUME: the executor continues the saved
	// conversation rather than starting one. See [Runner.Resume].
	Resume *Resume
}

// Resume is a suspended Execute conversation plus the answer that unblocks it.
type Resume struct {
	State execstate.State

	// Answer is what the pending run_sandbox call is answered with: the
	// coding agent's findings, or a person's reply to its question.
	Answer string
}

// Runner implements [turn.Phases] against real models and real tools.
//
// One per turn — see [Company.RunnerFor] on the engine side. That is what
// makes the spend tally below a per-turn fact rather than state to reset.
type Runner struct {
	cfg   Config
	spend Spend

	// mu guards suspension, which the Execute phase writes and the engine
	// reads once the turn returns, onboardedThisTurn, which the onboarding
	// pass writes and the executor reads, and guard, which surfaceWith
	// writes and a suspend reads back.
	mu         sync.Mutex
	suspension *Suspension

	// guard is the required-skill gate of the phase currently being built
	// or run, or nil where this turn arms none.
	//
	// A field rather than a return value because only ONE caller wants it —
	// a suspend, which persists the keys the session has loaded so the
	// resumed half of the same conversation is not asked to load them
	// again. Threading it through every phase's signature to serve that one
	// caller would put it in four places that ignore it. One phase runs at
	// a time on a runner, so the field is that phase's.
	guard *skills.Guard

	// onboardedThisTurn suppresses the executor prompt's onboarding hint for a
	// seat that has just been through the pass. See [Runner.Onboard].
	onboardedThisTurn bool
}

var _ turn.Phases = (*Runner)(nil)

// onboardingHint is the prefetched hint, unless this turn's own onboarding
// pass just ran.
//
// The prefetch is frozen at turn start and the pass runs after it, so the
// hint is rendered against a seat that had not onboarded YET. Repeating it
// afterwards tells a seat that has just read its onboarding pages to go and
// read them.
func (r *Runner) onboardingHint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.onboardedThisTurn {
		return ""
	}
	return r.cfg.Context.OnboardingHint
}

// New builds a runner.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Registry == nil:
		return nil, fmt.Errorf("runner: no tool registry")
	case cfg.Models == nil:
		return nil, fmt.Errorf("runner: no provider registry")
	}
	return &Runner{cfg: cfg}, nil
}

// Execute is the executor: one agentic pass that decides what to do and does
// it.
//
// It is given the whole first-party surface and the SLIM catalogue — builtin
// names and MCP server names — rather than every tool definition a real
// server publishes, which on a large stack is a wall of text in every prompt.
// Discovery is a tool call, which also keeps the prompt prefix stable while a
// server's catalogue changes underneath.
//
// One phase rather than two is the whole redesign. Planning in one
// conversation and acting in another cost the actor everything the planner
// learned, and made the planner NAME its tools in advance against a catalogue
// it was never shown — so it guessed, and the engine spent a whole subsystem
// reconciling those guesses with reality.
func (r *Runner) Execute(ctx context.Context, round int, notes string, history []ledger.Iteration) (turn.Work, turn.Surface, error) {
	snapshot := r.cfg.Registry.Snapshot()

	// The submission is validated against THIS surface's live record, so
	// the tool cannot exist before the surface and the surface cannot
	// resolve the tool before it exists. The closure breaks that the same
	// way the discovery pair's does, and is read only at call time — by
	// which point both exist.
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call { return calls(surface) },
			func() turn.Surface { return describe(surface) }))

	built, err := r.surfaceWith(ctx, phase.Execute, round, snapshot, submit, r.executorActive(snapshot))
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	system := prompts.BuildExecutor(r.cfg.Seat, prompts.ExecutorInput{
		ToolCatalogue:  r.cfg.Registry.Catalogue(),
		AvailableTools: snapshot.Names(),

		PersonalMemory:      r.cfg.Context.PersonalMemory,
		RelevantKnowledge:   r.cfg.Context.RelevantKnowledge,
		EpisodeRecall:       r.cfg.Context.EpisodeRecall,
		CounterpartyProfile: r.cfg.Context.CounterpartyProfile,
		SynthesizedSkills:   r.cfg.Context.SynthesizedSkills,
		OnboardingHint:      r.onboardingHint(),
		Workers:             r.workerCatalogue(),
		// The tool-skill catalogue, offered against the surface this
		// phase actually has — and nil where a company has published
		// none, which keeps the prompt free of skill scaffolding rather
		// than rendering an empty section.
		Skills: r.catalogue(),
	})
	user := prompts.BuildPhaseUserMessage(prompts.UserMessage{
		TaskDescription:     r.taskFor(notes),
		PriorWork:           ledger.RenderIterations(history, r.cfg.SkipNames),
		ConversationHistory: r.cfg.Conversation,
	})

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Execute, surface: surface, system: system, user: user,
		rounds: r.cfg.Caps.ExecutorRounds, ceiling: r.cfg.Caps.ExecutorCeiling,
		iteration: round, terminateAfter: []string{SubmitWorkTool},
		allowSuspend: true,
	})
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}

	if res.Suspended {
		r.recordSuspension(round, surface, res.Result, history)
		// A suspended phase submitted nothing and is not finished. It
		// returns with the ledger intact; the resumed turn comes back
		// through Resume and submits then.
		return turn.Work{
			Text: res.Text, Calls: calls(surface), Suspended: true,
		}, describe(surface), nil
	}

	return r.finishWork(phaseCtx, round, work{
		submit: submit, res: res, surface: surface, snapshot: snapshot,
		system: system, user: user,
	})
}

// work is one executor pass, assembled for reporting.
type work struct {
	submit   *structured.Tool[workPayload]
	res      phaseResult
	surface  *tools.Surface
	snapshot tools.Snapshot
	system   string
	user     string
}

// finishWork turns a finished executor pass into the turn's Work, publishing
// the phase record on the way.
//
// Shared with the resume path, which reaches the same point by a different
// road: a resumed executor is the same phase continuing, so it must report
// the same shape and rescue the same way.
func (r *Runner) finishWork(phaseCtx context.Context, round int, w work) (turn.Work, turn.Surface, error) {
	payload, submitted := w.submit.Value()
	if !submitted {
		// THE RESCUE PATH. An executor that ran out of rounds, or simply
		// stopped, has produced text and no account of itself. Discarding
		// the turn wastes everything it did; calling it delivered puts
		// words in its mouth on the one question that matters.
		//
		// So the outcome is INCOMPLETE and the work is marked rescued.
		// Both are load-bearing: the engine wrote this word, so no fast
		// path may act on it and the reviewer judges the record instead.
		log.WarnContext(phaseCtx, "work_never_submitted", "round", round, "rounds_used", w.res.Rounds)
		payload = workPayload{Outcome: string(turn.OutcomeIncomplete), Summary: w.res.Text}
	}

	missing := missingTools(w.surface)
	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Execute, Iteration: round, System: w.system, User: w.user,
		Result: w.res.Result, Exhausted: w.res.Exhausted,
		Decision: payload.Outcome, Rescued: !submitted,
		Notes:     missingNote(missing),
		Available: w.surface.Active(),
		// The names the executor was shown as prose, with no schemas.
		// Sending every MCP server's tool definitions is what made a turn
		// expensive, and this is what replaced it — which is why
		// Available (the schemas actually passed) is the short list and
		// the catalogue is the long one.
		Catalogue: w.snapshot.Names(),
	})

	return turn.Work{
		Outcome:         turn.Outcome(payload.Outcome),
		Summary:         payload.Summary,
		Deliveries:      payload.Deliveries,
		Evidence:        payload.Evidence,
		OpenQuestions:   payload.OpenQuestions,
		Text:            w.res.Text,
		Calls:           calls(w.surface),
		MissingTools:    missing,
		ExhaustedRounds: w.res.Exhausted,
		Rescued:         !submitted,
	}, describe(w.surface), nil
}

// Review judges the round.
//
// It is given the tool LOG as evidence and told the narration is not. A
// reviewer shown only what the executor said about itself grades the prose.
func (r *Runner) Review(ctx context.Context, round int, w turn.Work, history []ledger.Iteration) (turn.Review, error) {
	snapshot := r.cfg.Registry.Snapshot()
	submit := structured.New(SubmitReviewTool, submitReviewDescription, reviewSchema, decodeReview)
	surface, err := r.surfaceWith(ctx, phase.Review, round, snapshot, submit, nil)
	if err != nil {
		return turn.Review{}, err
	}

	// VERBATIM. Review's evidence log takes the zero FormatOptions: the
	// budgets belong to the cross-round ledger, and a reviewer judging an
	// elided log is judging a summary and calling it evidence.
	system := prompts.BuildReview(r.cfg.Seat, prompts.ReviewInput{
		Intent:            w.Summary,
		Outcome:           string(w.Outcome),
		Rescued:           w.Rescued,
		Evidence:          w.Evidence,
		OpenQuestions:     w.OpenQuestions,
		Produced:          reviewArtifact(w),
		ToolLog:           ledger.FormatCalls(w.Calls, ledger.FormatOptions{Skip: r.cfg.SkipNames}),
		EarlierIterations: ledger.RenderIterations(history, r.cfg.SkipNames),
	})

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Review, surface: surface, system: system, user: r.cfg.Task,
		rounds: reviewRounds, iteration: round,
		terminateAfter: []string{SubmitReviewTool}, intent: w.Summary,
	})
	if err != nil {
		return turn.Review{}, err
	}

	payload, submitted := submit.Value()
	if !submitted {
		// A reviewer that never decided must not silently pass the turn.
		// Defaulting to `done` here is the difference between "the work
		// was judged good" and "nothing judged it", and those look
		// identical downstream. Loop back instead — the delivery gate and
		// the stall guard bound how long that can go on.
		log.WarnContext(ctx, "review_never_submitted", "round", round, "rounds_used", res.Rounds)
		rescue := turn.Review{
			Decision: phase.SelfIterate,
			Notes: "The review phase produced no decision. Re-check what the " +
				"executor set out to do against what the tool log says it did, " +
				"and call " + SubmitReviewTool + ".",
		}
		r.emitter().completed(phaseCtx, reviewRecord(round, system, r.cfg.Task, res,
			string(rescue.Decision), rescue.Notes, true, surface))
		return rescue, nil
	}
	r.emitter().completed(phaseCtx, reviewRecord(round, system, r.cfg.Task, res,
		payload.Decision, payload.Notes, false, surface))
	return turn.Review{
		Decision:      phase.Decision(payload.Decision),
		Notes:         payload.Notes,
		CompletedWork: payload.CompletedWork,
		FinalArtifact: payload.FinalArtifact,
	}, nil
}

// reviewRecord builds Review's completed record.
//
// A function because Review reports from TWO places — its decoded payload and
// its rescue — and the two must describe the same phase. Written out twice,
// the rescue path is the one that quietly loses a field.
func reviewRecord(round int, system, user string, res phaseResult,
	decision, notes string, rescued bool, surface *tools.Surface,
) phaseRecord {
	return phaseRecord{
		Phase: phase.Review, Iteration: round, System: system, User: user,
		Result: res.Result, Exhausted: res.Exhausted,
		Decision: decision, Notes: notes, Rescued: rescued,
		Available: surface.Active(),
	}
}

// missingNote names the tools an Execute phase called that its surface did not
// have, for the phase event's short free-text field.
//
// Worth carrying: a model that guessed an MCP tool's name wrong produces a
// phase that looks like it simply chose not to deliver, and the note is the
// difference between reading that as a model problem and as a config one.
func missingNote(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return "missing tools: " + strings.Join(missing, ", ")
}

// runPhase drives one phase's loop, extending it when the judge allows.
//
// The extension loop lives here rather than in the extension package because
// only the caller knows what FINISHING looks like for its phase — the executor
// and the reviewer exit through their submission tools, a sub-agent by
// returning text with no calls — and a generic loop would have to be told,
// which is the same thing said twice.
// phaseRun is one phase's inputs.
//
// A struct rather than nine positional parameters, which is what it was: three
// adjacent ints (rounds, ceiling, iteration) and two adjacent strings (system,
// user) is a signature where a transposition compiles and produces a phase
// that silently runs with the wrong budget.
type phaseRun struct {
	phase   phase.Phase
	surface *tools.Surface

	// system and user open the conversation. Both are ignored when Seed is
	// set: a resumed loop already has its opening in the saved messages.
	system string
	user   string

	rounds    int
	ceiling   int
	iteration int

	// terminateAfter names tools that end the loop once they have run.
	terminateAfter []string

	// seed is the conversation a RESUMED loop starts from: the suspended
	// messages plus the answer to their dangling call. Nil for an ordinary
	// phase.
	seed []llm.Message

	// spent is what the pre-suspend rounds already cost, so a resumed
	// phase's record is the turn's total rather than only its second half.
	spent toolloop.Result

	// allowSuspend permits a tool to stop this loop with its call
	// unanswered. ONLY EXECUTE sets it: a phase that never persists a
	// partial conversation cannot resume one, so a suspend elsewhere would
	// silently abandon a turn.
	allowSuspend bool

	// intent is what the turn set out to do, for the extension judge.
	//
	// Empty for the executor, which is the phase that decides it as it
	// goes. The judge's question is whether a phase is progressing, and
	// progress is only meaningful against an intention: a tool log with
	// nothing beside it makes "reading the same page twice" and
	// "re-reading a page in order to compare" the same evidence.
	intent string
}

// runPhase returns the PHASE'S CONTEXT as well as its result.
//
// The caller needs it to publish agent_phase_completed under this phase's span
// rather than the turn's, which is what turns the dashboard's trace tree from
// a flat list under one turn node into trigger -> turn -> {execute, review}.
// The span has ENDED by then, and that is fine: an event recording a
// phase belongs to that phase's span, and a span id is a label rather than a
// lifetime.
//
// It is returned under its own name rather than shadowing the caller's ctx on
// purpose — work the caller does AFTER the phase must not be attributed to a
// span that is closed.
func (r *Runner) runPhase(ctx context.Context, in phaseRun) (context.Context, phaseResult, error) {
	ph, surface := in.phase, in.surface
	// ONE SPAN PER PHASE, wrapping the WHOLE extension loop below rather
	// than each toolloop.Run inside it: an extended Execute is one phase
	// that ran longer, and a span per invocation would report it as two
	// phases and split its rounds across them.
	//
	// The attributes are deliberately thin. Everything a reader wants about
	// what a phase DID — prompts and response verbatim, every tool call's
	// arguments and result, tokens, the decision — is already on
	// agent_phase_completed, and duplicating it onto a span would
	// send whole prompts to a collector that is not the event store. What
	// no event records is DURATION, per phase and per round, and that is
	// exactly what a span adds.
	ctx, span := tracing.Start(ctx, "agent.runner", "agent.turn."+string(ph),
		attribute.String("crewlet.phase", string(ph)),
		attribute.Int("crewlet.iteration", in.iteration))
	defer span.End()
	system, user := in.system, in.user
	iteration, ceiling := in.iteration, in.ceiling
	terminateAfter := in.terminateAfter
	// The phase's own record of what it managed, so the FAILURE path has
	// something to report. A phase that dies returns no Result at all, and
	// without this the only trace of it is the started event — a dashboard
	// left showing an in-flight call with no response and no reason.
	emit := r.emitter()
	progress := &toolloop.Progress{}
	// Returns the phase context too, so `return fail(err)` stays a single
	// line now that runPhase hands its context back.
	fail := func(err error) (context.Context, phaseResult, error) {
		// The span carries the failure too. The event below is the record
		// of WHAT broke; the span is what makes the broken phase findable
		// in a trace beside the calls that led to it.
		tracing.Fail(span, err)
		emit.completed(ctx, phaseRecord{
			Phase: ph, Iteration: iteration, System: system, User: user,
			Result: progress.Snapshot(), Available: surface.Active(),
			Failed: true, Err: err,
		})
		return ctx, phaseResult{}, err
	}

	members, err := r.cfg.Models.Chain(r.cfg.Seat.Role, ph)
	if err != nil {
		return fail(err)
	}
	// A chain even for one member. The wrapper is a pass-through there, and
	// uniform behaviour is worth more than the allocation: a one-member seat
	// and a three-member one then fail, log and report identically.
	provider, err := chain.New(members, chain.Options{})
	if err != nil {
		return fail(fmt.Errorf("runner: %s: %w", ph, err))
	}

	// Published BEFORE the first provider call, so a seat that is thinking
	// says which phase it is thinking in. The completed event is the durable
	// record and may be minutes away.
	emit.started(ctx, ph, iteration, system, user)

	messages := in.seed
	if messages == nil {
		messages = []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		}
	}
	policy := extension.Policy{
		Enabled: r.cfg.Caps.ExtensionOn, RoundStep: r.cfg.Caps.ExtensionStep, Ceiling: ceiling,
	}

	var out phaseResult
	budget := in.rounds
	for {
		// The loop numbers its rounds per INVOCATION, from 1. An extended
		// phase runs it again, so without this offset the second invocation
		// restarts at 1 and every consumer that orders on the round number
		// sees the phase run backwards: the live projection's stale-round
		// guard drops the whole extension, and the ledger merges extension
		// round 1 into original round 1. Captured before the call because
		// out.Rounds only grows after it returns.
		prior := out.Rounds
		res, err := toolloop.Run(ctx, toolloop.Config{
			Provider: provider, Messages: messages, Surface: surface,
			MaxRounds: budget, Budget: r.cfg.Budget,
			AllowSuspend: in.allowSuspend,
			// A phase that has SUBMITTED is finished. Without this the
			// loop asks again, the model submits again, and the phase
			// spends its whole round budget re-deciding — measured at
			// four identical submissions before the cap stopped it.
			TerminateAfter: terminateAfter,
			// Progress feeds the failure path above; OnProgress feeds the
			// live view. Both, because they answer different questions:
			// one is read after the loop returns nothing, the other while
			// it is still running.
			Progress: progress,
			// The live view is the only reason to stream, so it is on
			// exactly when something is listening.
			StreamPartials: true,
			OnProgress: func(live toolloop.Result) {
				emit.progress(ctx, ph, iteration, offsetRounds(live, prior))
			},
		})
		if err != nil {
			return fail(fmt.Errorf("runner: %s: %w", ph, err))
		}
		out.Text = res.Text
		out.Suspended = res.Suspended
		out.Exhausted = res.ExhaustedRounds
		// ACCUMULATED, not replaced. The conversation carries across an
		// extension (so res.Text is already the whole phase), but the loop's
		// executions and narration are per-invocation — assigning the last
		// one wholesale dropped every tool call and every round of narration
		// from before the extension, on exactly the long, hard phases that
		// get extended.
		shifted := offsetRounds(*res, out.Rounds)
		shifted.Executions = append(out.Result.Executions, shifted.Executions...)
		shifted.Narration = append(out.Result.Narration, shifted.Narration...)
		out.Result = shifted
		out.Rounds += res.RoundsUsed
		// The loop's own count is per-invocation; an extended phase runs it
		// more than once and the record must carry the phase's total.
		out.Result.RoundsUsed = out.Rounds
		// A resumed phase adds what the pre-suspend rounds spent. The
		// MESSAGES are not re-emitted (they are already recorded, and
		// re-publishing them would redraw a turn the dashboard has and
		// double-count every token) but the COUNTERS are the turn's, and a
		// record showing only the second half understates every resumed
		// turn's cost.
		out.Result.InputTokens += in.spent.InputTokens
		out.Result.OutputTokens += in.spent.OutputTokens
		messages = res.Messages

		if res.Suspended || !res.ExhaustedRounds {
			// Read off the phase TOTALS, never off `res`: an extended
			// phase ran the loop more than once and the last invocation
			// knows only its own slice.
			span.SetAttributes(
				attribute.Int("crewlet.rounds", out.Rounds),
				attribute.Bool("crewlet.suspended", out.Suspended))
			return ctx, out, nil
		}
		granted, decision := extension.Consider(ctx, r.cfg.Judge, policy, extension.Request{
			Phase: ph, Task: r.cfg.Task, PlanSummary: in.intent,
			Calls: calls(surface), LastText: res.Text, RoundsUsed: out.Rounds,
		})
		if granted <= 0 {
			log.InfoContext(ctx, "phase_not_extended", "phase", ph, "iteration", iteration,
				"rounds_used", out.Rounds, "reason", decision.Reason)
			return ctx, out, nil
		}
		log.InfoContext(ctx, "phase_extended", "phase", ph, "iteration", iteration,
			"granted", granted, "rounds_used", out.Rounds, "reason", decision.Reason)
		messages = append(messages, llm.Message{
			Role:    llm.RoleUser,
			Content: extension.Nudge(ph, granted, decision.Reason),
		})
		budget = granted
	}
}

// offsetRounds renumbers one loop invocation's rounds onto the phase's own
// scale, so an extension continues the count instead of restarting it.
//
// Copies rather than mutates: the caller's Result is the loop's live snapshot,
// published from another goroutine, and shifting it in place would renumber a
// slice the loop is still appending to.
func offsetRounds(res toolloop.Result, prior int) toolloop.Result {
	if prior <= 0 {
		return res
	}
	res.RoundsUsed += prior
	execs := make([]toolloop.Execution, len(res.Executions))
	for i, ex := range res.Executions {
		ex.Round += prior
		execs[i] = ex
	}
	res.Executions = execs
	narration := make([]toolloop.Narration, len(res.Narration))
	for i, n := range res.Narration {
		n.Round += prior
		narration[i] = n
	}
	res.Narration = narration
	return res
}

type phaseResult struct {
	Text      string
	Rounds    int
	Exhausted bool
	Suspended bool

	// Result is the loop's own outcome, kept whole so the phase can report
	// what it spent and what it called. The fields above are the ones the
	// loop's CALLER acts on; this is what its telemetry publishes, and
	// re-deriving one from the other is how the two come to disagree.
	Result toolloop.Result
}

// surfaceWith builds a phase surface, registering the phase's submission tool
// on a PRIVATE copy of the snapshot.
//
// Private because the tool is per-phase state: two phases of one turn each get
// their own, and registering into the shared registry would make one phase's
// submission visible to the next.
// The `extra` set is per-TURN rather than per-phase state: today it is the
// sub-agent spawner, which is bound to one parent turn's grant and must reach
// a resumed Execute as well as a fresh one. Injected here because this is the
// single funnel every phase surface goes through, so a phase that lost the
// tool mid-turn is not representable.
func (r *Runner) surfaceWith(ctx context.Context, ph phase.Phase, round int,
	snapshot tools.Snapshot, submit tools.Callable, active []string, loaded ...string,
) (*tools.Surface, error) {
	if submit != nil {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: submit, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append([]string{submit.Name()}, active...)
	}

	// The discovery pair reaches its surface through a closure, because
	// there is a real cycle here: activate must mutate the same Surface the
	// loop reads its tool definitions from, so the tools cannot exist
	// before the surface — and the surface cannot resolve them until they
	// are in its snapshot. The closure is read at call time, by which point
	// the surface exists.
	var surface *tools.Surface
	for _, tool := range DiscoveryTools(func() *tools.Surface { return surface }) {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: tool, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append(active, tool.Name())
	}

	// The sub-agent spawner, on the same closure and for the same reason:
	// the child inherits the parent's LIVE active list, which does not
	// exist until this surface does.
	//
	// Built HERE rather than by each caller because this is the single
	// funnel every phase surface goes through, so a fresh Execute and a
	// RESUMED one get it from one place — and a resumed turn that lost the
	// tool mid-run is not representable.
	if entry := r.spawnEntry(ctx, ph, round, snapshot,
		func() *tools.Surface { return surface }); entry.Tool != nil {
		next, err := snapshot.With(entry)
		if err != nil {
			// A collision here is an operator's MCP server publishing a
			// first-party tool name. LOSING THE SPAWNER is the
			// proportionate answer: failing the surface would kill every
			// Execute phase on that seat for as long as that server is
			// configured, which is a far larger outage than the missing
			// capability. The subagent package makes the same choice for
			// the same collision one level down.
			log.WarnContext(ctx, "spawn_tool_skipped", "phase", ph,
				"tool", entry.Tool.Name(), "error", err.Error())
		} else {
			snapshot = next
			active = append(active, entry.Tool.Name())
		}
	}
	// Bound to the turn, which is what lets a seat-scoped tool know who is
	// calling it without the seat travelling through the model's arguments.
	surface = tools.NewSurface(ph.String(), snapshot, active).ForTurn(r.cfg.Turn.Context)
	// THE GUARD IS BUILT FROM THE FINISHED SURFACE, so what it enforces and
	// what the catalogue showed cannot disagree: both are derived from the
	// same active list, at the same moment, and the catalogue's "required"
	// marker is the promise this keeps.
	guard := r.guardFor(ph, surface, loaded)
	// RECORDED ONLY HERE, which is what keeps it the TURN's guard: this is
	// the funnel every phase of the turn goes through, and a sub-agent's
	// child surface is built elsewhere. Recording inside guardFor would let
	// a child spawned mid-round overwrite the executor's own loaded set, so
	// a suspend moments later would persist the worker's.
	r.mu.Lock()
	r.guard = guard.skills()
	r.mu.Unlock()
	return surface.WithGuard(guard.tools()), nil
}

// armedGuard is one built gate: the concrete guard for the runner's own
// bookkeeping, and the interface value the surface installs.
//
// Two views because A TYPED NIL WOULD NOT BE NIL. Returning the *skills.Guard
// as a tools.Guard would give the surface a non-nil interface wrapping a nil
// pointer, so its `guard != nil` check would pass and every tool call would
// take the guard path to be told yes.
type armedGuard struct {
	guard *skills.Guard
	wrap  tools.Guard
}

func (a armedGuard) skills() *skills.Guard { return a.guard }
func (a armedGuard) tools() tools.Guard    { return a.wrap }

// guardFor arms the required-skill gate for one phase session, or nil.
//
// PER SESSION, because "loaded" means "the body is in this model's context"
// and the executor, the reviewer and each sub-agent are separate message
// histories. A
// round-cap extension continues the same session AND the same surface, so a
// load carries across it; a self_iterate builds a fresh surface and
// therefore a fresh guard, which is correct — its LLM context started over
// too.
// The loaded keys are the RESUME seed: a suspended executor is re-entered as
// the same message history, so the bodies it read before the suspend are still
// in front of the model and a guard rebuilt empty would block the tools that
// session already unlocked. Empty for every other phase.
func (r *Runner) guardFor(ph phase.Phase, surface *tools.Surface, loaded []string) armedGuard {
	if r.cfg.Skills == nil {
		return armedGuard{}
	}
	guard := skills.NewGuard(r.cfg.Skills, promptPhase(ph), catalogueSurface(surface))
	if guard == nil {
		return armedGuard{}
	}
	guard.Restore(loaded)
	return armedGuard{
		guard: guard,
		wrap:  &reportingGuard{guard: guard, emit: r.emitter(), phase: ph},
	}
}

// catalogue is the tool-skill registry as the prompt sees it, or nil.
//
// A TYPED NIL WOULD NOT BE NIL here either: prompts.injectSkillCatalogue
// checks its interface against nil, and a non-nil interface wrapping a nil
// registry would take the catalogue path and render a header over nothing.
func (r *Runner) catalogue() prompts.SkillCatalogue {
	if r.cfg.Skills == nil {
		return nil
	}
	return r.cfg.Skills
}

// promptPhase maps a runner phase onto the prompt package's own.
//
// Two vocabularies because two packages own them, and the mapping is
// explicit rather than a string cast: a phase this package adds and forgets
// to map would silently become the zero value, which matches no skill and
// arms no guard.
func promptPhase(ph phase.Phase) prompts.Phase {
	switch ph {
	case phase.Execute:
		return prompts.PhaseExecute
	case phase.Review:
		return prompts.PhaseReview
	case phase.Subagent:
		return prompts.PhaseSubagent
	default:
		// Onboarding and anything later. Not a phase skills are scoped
		// to, and the empty value matches none of them — which is the
		// honest answer rather than a default that quietly picks one.
		return ""
	}
}

// catalogueSurface is the tool surface as the skill triggers see it.
func catalogueSurface(surface *tools.Surface) prompts.Surface {
	active := surface.Active()
	return prompts.Surface{Tools: active, MCPServers: surface.Universe().MCPServers()}
}

// executorActive is what the executor is offered: every first-party tool bar
// the phase-scoped ones. MCP tools are reachable through discovery.
//
// The whole first-party surface, not a slice of it chosen in advance. Choosing
// in advance is what the planner used to do — against a catalogue it was never
// shown — and every wrong guess became a tool the actor did not have when it
// turned out to need it.
func (r *Runner) executorActive(snapshot tools.Snapshot) []string {
	var out []string
	for _, e := range snapshot.Entries() {
		if _, fromMCP := e.FromMCP(); fromMCP {
			continue
		}
		if phaseScoped[e.Name()] {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// phaseScoped names the first-party tools that belong to ONE phase and must
// not be offered by the others.
//
// mark_onboarded is the whole list, and it earns its place: onboarding is its
// own pass, and a seat that could mark itself from inside the executor would
// permanently skip orientation — the marker suppresses the pass for ever
// after, so a single stray call means an agent that never reads its team's
// conventions and never will. Measured: the surface offered it, and a model
// that called it there produced a phase that never submitted at all.
//
// Everything else stays available on purpose. lookup_colleague, use_skill,
// query_episodes and refresh_memory are recon, which the executor now does
// itself; reflect_and_persist is deliberate too — an agent that learns
// something mid-turn should be able to keep it.
var phaseScoped = map[string]bool{MarkOnboardedTool: true}

// taskFor prefixes the reviewer's correction to the ask.
//
// PREFIXED to the user message rather than appended to the task description
// itself: the task text also feeds knowledge search, the sandbox brief and the
// episode record, all of which want the requester's actual ask and not the
// engine's running commentary on it.
func (r *Runner) taskFor(notes string) string {
	if strings.TrimSpace(notes) == "" {
		return r.cfg.Task
	}
	return r.cfg.Task + "\n\nThe previous round was reviewed. What to do differently:\n" + notes
}

// calls converts a surface's record into ledger calls.
func calls(s *tools.Surface) []ledger.Call {
	recorded := s.Calls()
	out := make([]ledger.Call, 0, len(recorded))
	for _, c := range recorded {
		out = append(out, ledger.Call{Name: c.Name, Args: c.Args, Result: c.Output, Failed: c.Failed})
	}
	return out
}

// describe renders the surface the delivery gate judges against.
func describe(s *tools.Surface) turn.Surface {
	u := s.Universe()
	return turn.Surface{Catalogue: u.Names(), MCPTools: u.MCPNames(), KnownReads: u.KnownReads()}
}

// missingTools are names the phase called that the surface did not have.
//
// Judged against the surface's OWN universe rather than the registry snapshot
// it was built from: the phase's submission tool and the discovery pair are
// added on the way in and exist in neither the registry nor any earlier
// snapshot, so comparing against one would report the executor's own
// terminator as a hallucinated tool on every turn.
//
// Membership is the single source of truth. Matching on the failure TEXT
// instead would flag a false positive the moment a legitimate tool's own
// output began with the same words.
func missingTools(s *tools.Surface) []string {
	universe := s.Universe()
	var out []string
	seen := map[string]bool{}
	for _, name := range s.CalledNames() {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := universe.Lookup(name); !ok {
			out = append(out, name)
		}
	}
	return out
}

// reviewArtifact bounds the draft handed to the reviewer.
//
// The same budget the cross-round ledger gives an artifact, because it is the
// same content answering the same question: enough to judge, and enough for
// the next round to extend rather than rewrite.
func reviewArtifact(w turn.Work) string {
	if w.Text == "" {
		return "(empty)"
	}
	return ledger.Elide(w.Text, ledger.ArtifactLimit)
}

// Caps returns the runner's round budgets, so an assembler can assert on what
// it actually wired rather than on what it meant to.
func (r *Runner) Caps() Caps { return r.cfg.Caps }
