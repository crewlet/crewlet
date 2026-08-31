package runner

import (
	"context"
	"fmt"
	"slices"
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

// Caps are the per-phase round budgets and their extension ceilings.
type Caps struct {
	PlanRounds     int
	ExecuteRounds  int
	ReviewRounds   int
	PlanCeiling    int
	ExecuteCeiling int
	ExtensionStep  int
	ExtensionOn    bool
}

// Config is everything a runner needs that does not change between rounds.
type Config struct {
	Seat     prompts.Seat
	Registry *tools.Registry
	Models   *phase.Registry
	Caps     Caps

	// Judge decides round-cap extensions. Nil means every exhaustion goes
	// straight to the rescue path.
	Judge extension.Judge

	// Budget is the shared token counter a turn charges. Nil disables the
	// per-round charge, which is the embedded single-node case where no
	// counter is shared with anyone.
	Budget toolloop.BudgetMeter

	// Task is the ask, and Conversation is the prior-turns block. Both are
	// fixed for the turn.
	Task         string
	Conversation string

	// Recon recovers the knowledge block a thin trigger's gate skipped,
	// keyed on the plan summary. Nil means no recovery, which is the right
	// answer for a company with no knowledge backend and for a runner a
	// test drives directly.
	//
	// THE ONE MID-TURN FETCH, and structurally so: its input does not
	// exist until Plan has run. It happens once, between the phases, so
	// the Execute prompt is still fixed for the whole of Execute.
	Recon func(ctx context.Context, planSummary string) string

	// Context is what this seat remembers, what its company has written
	// down, what it has done before, and who it is talking to — rendered
	// by the caller BEFORE the turn.
	//
	// Strings rather than a seam the runner could pull on, and that is the
	// freeze: Plan runs again on a self_iterate loop, and a runner able to
	// re-fetch would produce a different system prompt on each pass. A
	// provider caches on an exact prefix, so a prompt that moves costs the
	// whole prompt again every iteration — and the planner would see its
	// own context change underneath a decision it is mid-way through
	// making. There is nowhere here for a second fetch to happen.
	Context prefetch.Blocks

	// AlwaysOn are tools Execute has whatever the plan named.
	AlwaysOn []string

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

	// Resume re-enters a suspended Execute phase. Non-nil makes this
	// runner's turn a RESUME: Plan is skipped and Execute continues the
	// saved conversation. See [Runner.Resume].
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
	// reads once the turn returns, and onboardedThisTurn, which the
	// onboarding pass writes and Plan reads.
	mu         sync.Mutex
	suspension *Suspension

	// onboardedThisTurn suppresses the Plan prompt's onboarding hint for a
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

// recon recovers the knowledge block a thin trigger skipped, or answers
// empty.
//
// Wrapped rather than called inline so the nil case lives in one place: the
// Execute prompt is assembled in a struct literal, and a nil check inside
// one would be the kind of thing a later edit drops.
func (r *Runner) recon(ctx context.Context, planSummary string) string {
	if r.cfg.Recon == nil {
		return ""
	}
	return r.cfg.Recon(ctx, planSummary)
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

// Plan runs the planning pass.
//
// The planner is given the META-TOOLS and the slim catalogue, not the whole of
// every MCP server: a real server publishes dozens of tools and a planner
// shown all of them plans against a wall of text. Discovery is a tool call,
// which also keeps the prompt prefix stable while a server's catalogue changes
// underneath.
func (r *Runner) Plan(ctx context.Context, round int, notes string, history []ledger.Iteration) (turn.Plan, turn.Surface, error) {
	snapshot := r.cfg.Registry.Snapshot()
	submit := &submitted[planPayload]{
		name: SubmitPlanTool, desc: submitPlanDescription, schema: planSchema, decode: decodePlan,
	}
	surface, err := r.surfaceWith(phase.Plan, snapshot, submit, r.planActive(snapshot))
	if err != nil {
		return turn.Plan{}, turn.Surface{}, err
	}

	system := prompts.BuildPlan(r.cfg.Seat, prompts.PlanInput{
		ToolCatalogue:  r.cfg.Registry.Catalogue(),
		AvailableTools: snapshot.Names(),

		PersonalMemory:      r.cfg.Context.PersonalMemory,
		RelevantKnowledge:   r.cfg.Context.RelevantKnowledge,
		EpisodeRecall:       r.cfg.Context.EpisodeRecall,
		CounterpartyProfile: r.cfg.Context.CounterpartyProfile,
		SynthesizedSkills:   r.cfg.Context.SynthesizedSkills,
		OnboardingHint:      r.onboardingHint(),
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
		phase: phase.Plan, surface: surface, system: system, user: user,
		rounds: r.cfg.Caps.PlanRounds, ceiling: r.cfg.Caps.PlanCeiling, iteration: round,
		terminateAfter: []string{SubmitPlanTool},
	})
	if err != nil {
		return turn.Plan{}, turn.Surface{}, err
	}

	payload, submitted := submit.Value()
	if !submitted {
		// THE RESCUE PATH. A planner that ran out of rounds, or simply
		// stopped, has produced reasoning and no decision. Discarding the
		// turn wastes everything it did; inventing a full plan puts words
		// in its mouth. A `direct` plan with its own text as the reasoning
		// is the honest middle: Execute improvises against the full
		// surface.
		//
		// The plan is marked RESCUED, and that mark is load-bearing. A
		// rescue names no tools, so the delivery gate reads it as a turn
		// that intended nothing and does not force Review — while
		// `direct` on its own skips Review outright. Both together let a
		// rescued turn deliver nothing and report done, so the turn loop
		// keys on the mark rather than on the synthesised word.
		log.WarnContext(ctx, "plan_never_submitted", "round", round, "rounds_used", res.Rounds)
		payload = planPayload{Decision: string(turn.PlanDirect), Reasoning: res.Text}
	}

	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Plan, Iteration: round, System: system, User: user,
		Result: res.Result, Exhausted: res.Exhausted,
		Decision: payload.Decision, Rescued: !submitted,
		Available: surface.Active(),
		// Plan alone offers a catalogue: the names it was shown as prose,
		// with no schemas. Sending every MCP server's tool definitions is
		// what made planning expensive, and this is what replaced it —
		// which is why Available (the schemas actually passed) is the
		// short meta-tool list here and the catalogue is the long one.
		Catalogue: snapshot.Names(),
	})

	return turn.Plan{
		Decision:        turn.PlanDecision(payload.Decision),
		Reasoning:       payload.Reasoning,
		Summary:         payload.Summary(),
		ToolsNeeded:     payload.ToolsNeeded,
		SuccessCriteria: payload.SuccessCriteria,
		Calls:           calls(surface),
		Rescued:         !submitted,
	}, describe(surface), nil
}

// Execute runs the plan.
//
// Its surface is what the plan NAMED plus the always-on set — not the whole
// catalogue — because a plan that named its delivery tool should be executing
// it, not re-deciding. A `direct` plan is the exception: it committed to one
// shot against everything, so it gets everything.
func (r *Runner) Execute(ctx context.Context, round int, p turn.Plan, history []ledger.Iteration) (turn.Execution, turn.Surface, error) {
	snapshot := r.cfg.Registry.Snapshot()
	active := r.executeActive(snapshot, p)
	surface, err := r.surfaceWith(phase.Execute, snapshot, nil, active)
	if err != nil {
		return turn.Execution{}, turn.Surface{}, err
	}

	_, phantom := phase.ResolvePlanned(p.ToolsNeeded, snapshot.Names())
	system := prompts.BuildExecute(r.cfg.Seat, prompts.ExecuteInput{
		PlanSummary:    p.Summary,
		AvailableTools: surface.Active(),
		ToolCatalogue:  r.cfg.Registry.Catalogue(),
		// FORWARDED from the Plan-phase prefetch. The executor needs the
		// requester's observed traits even where the plan describes the
		// action abstractly — "reply in the counterparty's preferred
		// register" is a plan step that cannot be carried out by
		// somebody who cannot see what that register is.
		CounterpartyProfile: r.cfg.Context.CounterpartyProfile,
		RelevantKnowledge:   r.recon(ctx, p.Summary),
		Skills:              r.catalogue(),
		// Named explicitly, because a planner that guessed an MCP tool's
		// name wrong and is not told so assumes the tool exists, fails to
		// call it, and settles for a text reply that delivers nothing.
		PhantomTools: phantom,
	})
	user := prompts.BuildPhaseUserMessage(prompts.UserMessage{
		TaskDescription:     r.cfg.Task,
		PriorWork:           ledger.RenderIterations(history, r.cfg.SkipNames),
		ConversationHistory: r.cfg.Conversation,
	})

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Execute, surface: surface, system: system, user: user,
		rounds: r.cfg.Caps.ExecuteRounds, ceiling: r.cfg.Caps.ExecuteCeiling, iteration: round,
		allowSuspend: true, planSummary: p.Summary,
	})
	if err != nil {
		return turn.Execution{}, turn.Surface{}, err
	}

	if res.Suspended {
		r.recordSuspension(round, surface, res.Result, history)
	}

	missing := missingTools(surface, snapshot)
	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Execute, Iteration: round, System: system, User: user,
		Result: res.Result, Exhausted: res.Exhausted,
		// Execute reaches no structured verdict and never rescues — it has
		// no submit tool to miss.
		Notes:     missingNote(missing),
		Available: surface.Active(),
	})

	return turn.Execution{
		Text:            res.Text,
		Calls:           calls(surface),
		MissingTools:    missing,
		ExhaustedRounds: res.Exhausted,
		Suspended:       res.Suspended,
	}, describe(surface), nil
}

// Review judges the round.
//
// It is given the tool LOGS as evidence and told the narration is not. A
// reviewer shown only what Execute said about itself grades the prose.
func (r *Runner) Review(ctx context.Context, round int, p turn.Plan, e turn.Execution, history []ledger.Iteration) (turn.Review, error) {
	snapshot := r.cfg.Registry.Snapshot()
	submit := &submitted[reviewPayload]{
		name: SubmitReviewTool, desc: submitReviewDescription, schema: reviewSchema, decode: decodeReview,
	}
	surface, err := r.surfaceWith(phase.Review, snapshot, submit, nil)
	if err != nil {
		return turn.Review{}, err
	}

	// VERBATIM. Review's evidence log takes the zero FormatOptions: the
	// budgets belong to the cross-round ledger, and a reviewer judging an
	// elided log is judging a summary and calling it evidence.
	system := prompts.BuildReview(r.cfg.Seat, prompts.ReviewInput{
		PlanSummary:       p.Summary,
		ExecuteSummary:    reviewArtifact(e),
		PlanToolLog:       ledger.FormatCalls(p.Calls, ledger.FormatOptions{Skip: r.cfg.SkipNames}),
		ExecuteToolLog:    ledger.FormatCalls(e.Calls, ledger.FormatOptions{Skip: r.cfg.SkipNames}),
		EarlierIterations: ledger.RenderIterations(history, r.cfg.SkipNames),
	})

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Review, surface: surface, system: system, user: r.cfg.Task,
		rounds: r.cfg.Caps.ReviewRounds, iteration: round,
		terminateAfter: []string{SubmitReviewTool}, planSummary: p.Summary,
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
			Notes: "The review phase produced no decision. Re-check the plan's " +
				"success criteria against what Execute actually did, and call " +
				SubmitReviewTool + ".",
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
// only the caller knows what FINISHING looks like for its phase — Plan exits
// through its submission tool, Execute by returning text with no calls — and a
// generic loop would have to be told, which is the same thing said twice.
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

	// planSummary is what the turn set out to do, for the extension judge.
	//
	// Empty for Plan, which is the phase that produces it. The judge's
	// question is whether a phase is progressing, and progress is only
	// meaningful against an intention: a tool log with no plan beside it
	// makes "reading the same page twice" and "re-reading a page the plan
	// says to compare" the same evidence. Declared on extension.Request
	// since that type existed and populated by nobody.
	planSummary string
}

// runPhase returns the PHASE'S CONTEXT as well as its result.
//
// The caller needs it to publish agent_phase_completed under this phase's span
// rather than the turn's, which is what turns the dashboard's trace tree from
// a flat list under one turn node into trigger -> turn -> {plan, execute,
// review}. The span has ENDED by then, and that is fine: an event recording a
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
			OnProgress: func(live toolloop.Result) {
				emit.progress(ctx, ph, iteration, live)
			},
		})
		if err != nil {
			return fail(fmt.Errorf("runner: %s: %w", ph, err))
		}
		out.Rounds += res.RoundsUsed
		out.Text = res.Text
		out.Suspended = res.Suspended
		out.Exhausted = res.ExhaustedRounds
		out.Result = *res
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
			Phase: ph, Task: r.cfg.Task, PlanSummary: in.planSummary,
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
func (r *Runner) surfaceWith(ph phase.Phase, snapshot tools.Snapshot, submit tools.Callable, active []string) (*tools.Surface, error) {
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
	for _, tool := range discoveryTools(func() *tools.Surface { return surface }) {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: tool, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append(active, tool.Name())
	}
	// Bound to the turn, which is what lets a seat-scoped tool know who is
	// calling it without the seat travelling through the model's arguments.
	surface = tools.NewSurface(ph.String(), snapshot, active).ForTurn(r.cfg.Turn.Context)
	// THE GUARD IS BUILT FROM THE FINISHED SURFACE, so what it enforces and
	// what the catalogue showed cannot disagree: both are derived from the
	// same active list, at the same moment, and the catalogue's "required"
	// marker is the promise this keeps.
	surface = surface.WithGuard(r.guardFor(ph, surface))
	return surface, nil
}

// guardFor arms the required-skill gate for one phase session, or nil.
//
// PER SESSION, because "loaded" means "the body is in this model's context"
// and Plan, Execute and each sub-agent are separate message histories. A
// round-cap extension continues the same session AND the same surface, so a
// load carries across it; a self_iterate builds a fresh surface and
// therefore a fresh guard, which is correct — its LLM context started over
// too.
func (r *Runner) guardFor(ph phase.Phase, surface *tools.Surface) tools.Guard {
	if r.cfg.Skills == nil {
		return nil
	}
	guard := skills.NewGuard(r.cfg.Skills, promptPhase(ph), catalogueSurface(surface))
	if guard == nil {
		// A TYPED NIL WOULD NOT BE NIL. Returning the *skills.Guard
		// directly would give the surface a non-nil interface wrapping a
		// nil pointer, so its `guard != nil` check would pass and every
		// tool call would take the guard path to be told yes.
		return nil
	}
	return &reportingGuard{guard: guard, emit: r.emitter(), phase: ph}
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
	case phase.Plan:
		return prompts.PhasePlan
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

// planActive is what the planner is offered: its submission tool plus the
// first-party tools. MCP tools are reachable through discovery.
func (r *Runner) planActive(snapshot tools.Snapshot) []string {
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
// own pass now, and a seat that could mark itself from inside Plan would
// permanently skip orientation — the marker suppresses the pass for ever
// after, so a single stray call means an agent that never reads its team's
// conventions and never will. Measured: the Plan surface offered it, and a
// model that called it there produced a Plan that never submitted, rescued to
// `direct`, and skipped Review.
//
// Everything else stays available to Plan on purpose. lookup_colleague,
// use_skill, query_episodes and refresh_memory are recon, which is what Plan
// is for; reflect_and_persist is deliberate too — an agent that learns
// something while planning should be able to keep it, and the reflect engine's
// no-action gate reads the plan tool sequence for exactly that.
var phaseScoped = map[string]bool{MarkOnboardedTool: true}

// executeActive is what the plan named plus the always-on set — or everything,
// for a `direct` plan that committed to one shot.
//
// No filtering and no dedupe. [tools.Surface.Activate] is the gate: it admits
// nothing the snapshot resolves and is idempotent, so a name the planner
// guessed wrong is dropped there and a repeat costs nothing. Filtering here as
// well was written, mutated away, and nothing noticed — two guards for one
// property, with the doc comment attached to the one that was not doing the
// work.
//
// Dropping a phantom rather than refusing it is deliberate: the planner
// guessed at an MCP surface it could not see, the delivery gate already
// reports that, and failing the phase would turn a recoverable mis-guess into
// a lost turn.
func (r *Runner) executeActive(snapshot tools.Snapshot, p turn.Plan) []string {
	if p.Decision == turn.PlanDirect {
		return snapshot.Names()
	}
	return append(slices.Clone(p.ToolsNeeded), r.cfg.AlwaysOn...)
}

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
// Membership in the snapshot is the single source of truth. Matching on the
// failure TEXT instead would flag a false positive the moment a legitimate
// tool's own output began with the same words.
func missingTools(s *tools.Surface, snapshot tools.Snapshot) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range s.CalledNames() {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := snapshot.Lookup(name); !ok {
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
func reviewArtifact(e turn.Execution) string {
	if e.Text == "" {
		return "(empty)"
	}
	return ledger.Elide(e.Text, ledger.ArtifactLimit)
}

// Caps returns the runner's round budgets, so an assembler can assert on what
// it actually wired rather than on what it meant to.
func (r *Runner) Caps() Caps { return r.cfg.Caps }
