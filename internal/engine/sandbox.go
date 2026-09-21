package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The sandbox's engine-side wiring: which concrete thing satisfies which seam.
//
// The manager and its provider are built from providers.sandbox and swapped on
// an apply, like the LLM providers beside them. The coordinator and the waiter
// are NOT: they hold the busy set and the poll loop, which are facts about
// this PROCESS rather than about a config revision — rebuilding them on an
// apply would forget which seats are mid-run and start a second poll loop
// against the same rows.

// buildSandbox constructs the manager for a company, or nil when no seat can
// run code.
//
// Nil is an ordinary outcome, not a failure: providers.sandbox is absent in
// most deployments, and a build with none simply never offers run_sandbox.
// What is NOT ordinary is a configured provider that cannot be constructed —
// that fails the apply, because the alternative publishes a company whose
// sandbox-enabled seats plan around a box they will never get.
func buildSandbox(c *config.Company, env *config.Resolver, otel *sandbox.OtelReceiver) (*sandbox.Manager, error) {
	spec := c.Providers.Sandbox
	if spec == nil || !spec.Enabled() {
		return nil, nil
	}
	// ONLY THE CELLS THE COMPANY ACTUALLY REACHES, from the same computation
	// validation reads. Building the whole catalogue eagerly constructed a
	// container backend for a company whose seats all run direct, and failed
	// the apply demanding an image the validator had just refused as a field
	// nothing would read.
	providers, err := buildSandboxProviders(spec, env, c.SandboxPlacements())
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, nil
	}
	return sandbox.NewManager(sandbox.ManagerOptions{
		Providers:          providers,
		DefaultPlacement:   sandbox.Placement(spec.RunIn()),
		Runners:            sandboxRunners(),
		DefaultCodingAgent: string(spec.DefaultCodingAgent),
		DefaultTimeout:     seconds(spec.Timeout()),
		DefaultPauseTTL:    secondsPtr(spec.PauseTTL()),
		DefaultMaxTurns:    spec.DefaultMaxTurns,
		DefaultSetup:       setupSteps(spec.Setup),
		Telemetry:          otel,
	})
}

// sandboxRunners is every coding agent this build can drive.
//
// A FUNCTION rather than a literal at the one call site, because a second
// reader needs the same list: `crewlet llm doctor` refuses an agent-mode entry
// whose CLI has no runner, and it checks against codingagent.Names(). A test
// holds the two together — a drift would have the doctor pass an entry the
// engine then refuses at the seat's first turn.
func sandboxRunners() map[string]sandbox.Runner {
	return map[string]sandbox.Runner{
		codingagent.ClaudeCodeName: codingagent.NewClaudeCode(),
		codingagent.OpenCodeName:   codingagent.NewOpenCode(),
	}
}

// buildSandboxProviders turns the catalogue into one backend per placement it
// configures.
//
// THE CLOSED SET AND THIS FUNCTION ARE THE SAME LIST, asserted by a test,
// because nothing else connects them: the last time they disagreed the
// config's default named a backend with no case here, so a company validated
// cleanly, reported a configured sandbox on the dashboard, and failed at its
// first coding run.
//
// ONE LOCAL BACKEND PER LOCAL PLACEMENT, not one shared between them: the two
// differ in exactly one option, and a single instance would have to be told
// which cell it was serving on every call — which is the block-wide mode this
// whole reshape removed, reintroduced one layer down.
func buildSandboxProviders(spec *config.SandboxProvider, env *config.Resolver, reached map[config.Placement]string) (map[sandbox.Placement]sandbox.Provider, error) {
	built := make(map[sandbox.Placement]sandbox.Provider, len(reached))
	// WALKED IN THE CLOSED SET'S ORDER, not the map's: a map iterates
	// randomly, and an error naming whichever backend happened to come
	// first would differ between two runs of the same broken config.
	for _, placement := range config.BackendPlacements() {
		if _, want := reached[placement]; !want {
			continue
		}
		if !spec.Configured(placement) {
			// Refused by validation, so this is the belt to that brace: an
			// embedder building a company by hand reaches it, and a
			// silently missing backend would offer run_sandbox to a seat
			// whose runs can never start.
			return nil, fmt.Errorf("providers.sandbox: %q is reached by %s and "+
				"has no backend configured", placement, reached[placement])
		}
		provider, err := buildSandboxProvider(spec, env, placement)
		if err != nil {
			return nil, err
		}
		built[sandbox.Placement(placement)] = provider
	}
	return built, nil
}

func buildSandboxProvider(spec *config.SandboxProvider, env *config.Resolver, placement config.Placement) (sandbox.Provider, error) {
	if spec.Fake {
		// The in-process double, for a deployment demonstrating the flow
		// without a real box. Named in config rather than inferred, so
		// nobody runs one by accident — and it answers every placement,
		// so a demonstration config differs from a real one in exactly
		// one line.
		return sandbox.NewFakeProvider(), nil
	}
	switch placement {
	case config.PlacementE2B:
		e2b := spec.E2B
		// RESOLVED HERE, at the moment the provider is built, which is
		// the only place the key's value exists in this process. Tier B
		// stores its references verbatim — that is what keeps an exported
		// revision free of resolved secrets — so a backend handed
		// e2b.APIKey directly would authenticate with the literal
		// "${E2B_API_KEY}" and get a 401 naming the vendor rather than
		// the misconfiguration.
		//
		// The DOMAIN is resolved on the same terms and for a reason of its
		// own: a staging cluster and a production one are the same config
		// with a different variable, and passing the reference through
		// would point every box at a host called "${E2B_DOMAIN}".
		return sandbox.NewE2B(sandbox.E2BOptions{
			APIKey:   resolvedOr(env, e2b.APIKey),
			Domain:   resolvedOr(env, e2b.Domain),
			Template: e2b.Template,
		})
	case config.PlacementDirect, config.PlacementContainer:
		local := spec.Local
		return sandbox.NewLocal(sandbox.LocalOptions{
			Placement: sandbox.Placement(placement),
			StateDir:  local.StateDir,
			Image:     local.Image,
			Runtime:   string(local.Runtime),
			Network:   local.Network,
			RunArgs:   local.RunArgs,
		})
	default:
		// UNREACHABLE THROUGH A PARSED CONFIG, because the closed set and
		// this switch are the same list.
		//
		// Answered rather than panicked: an embedder building a spec by
		// hand is a caller, not an operator to be crashed at.
		return nil, fmt.Errorf("providers.sandbox: %q is not one of %v",
			placement, config.Placements)
	}
}

// setupSteps maps the config shape onto the sandbox package's own.
//
// A translation rather than a shared type, so the sandbox package does not
// import the config package: a setup step is a runtime instruction, and
// keeping the two apart is what lets the sandbox layer be tested with a step
// built in a test rather than a YAML document parsed into one.
// resolvedOr reads a config value through this node's chain, falling back to
// the literal when there is no resolver.
//
// A NIL RESOLVER IS A CALLER INSIDE THE PROCESS — a test, an embedder — and
// handing it the literal is right: it wrote the literal. A running engine
// always has one.
func resolvedOr(env *config.Resolver, value string) string {
	if env == nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(env.Value(value))
}

// setupSteps converts the PROVIDER-WIDE steps a `providers.sandbox` block
// declares, which are settings and therefore config's.
func setupSteps(steps []config.SandboxSetupStep) []sandbox.SetupStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]sandbox.SetupStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, sandbox.SetupStep{
			Name: s.Name, Files: s.Files, Commands: s.Commands,
			Env: s.Env, Brief: s.Brief, TimeoutSeconds: s.Timeout(),
		})
	}
	return out
}

// seatSetupSteps converts a SEAT's own steps, which ride the org chart.
//
// TWO CONVERTERS BECAUSE THERE ARE TWO SOURCES, and only one thing about them
// could drift — what an unset timeout means — which is why that rule is
// [org.SetupTimeout] and neither of these states it. Everything else here is
// field-for-field and the compiler checks it.
func seatSetupSteps(steps []org.SandboxSetupStep) []sandbox.SetupStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]sandbox.SetupStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, sandbox.SetupStep{
			Name: s.Name, Files: s.Files, Commands: s.Commands,
			Env: s.Env, Brief: s.Brief,
			TimeoutSeconds: org.SetupTimeout(s.TimeoutSeconds),
		})
	}
	return out
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// secondsPtr carries an OPTIONAL number of seconds through as a duration,
// keeping nil distinct from zero.
//
// The distinction is the whole point at the two call sites that need it: a
// pause TTL of 0 means "never pause" and an absent one means "take the
// engine default", and every layer that collapsed the two re-applied a
// default over a setting an operator had deliberately chosen.
func secondsPtr(v *float64) *time.Duration {
	if v == nil {
		return nil
	}
	d := seconds(*v)
	return &d
}

// sandboxAccountant post-charges a collected coding run against the shared
// counter.
//
// The charge happens AFTER the spend, which is why it cannot refuse: a refusal
// cannot un-spend a run that already ran, and recording it anyway is the only
// way the meter stays true when the cap is binding. So it goes through
// [coord.Budgets.PostCharge], never the gate: charged through
// [coord.Budgets.Charge], a run that did not fit was recorded not at all,
// which under-stated the company's spend by the whole run at exactly the
// moment the cap bound, and stamped a refusal on a seat that would still
// admit its next round.
//
// The caps only decide whether to SAY the run went over. They are read live,
// and a limit of 0 is unlimited, matching the config.
//
// Charging it once per launch, however often its completion is retried, is the
// coordinator's side: see [sandbox.PendingRun.Charged].
type sandboxAccountant struct {
	budgets coord.Budgets
	caps    func(agentID string) (org, seat int)
}

func (a sandboxAccountant) Charge(ctx context.Context, agentID, _ string, tokens int) (bool, error) {
	if a.budgets == nil || tokens <= 0 {
		return false, nil
	}
	spend, err := a.budgets.PostCharge(ctx, coord.AgentScope(agentID), tokens)
	if err != nil {
		return false, err
	}
	orgLimit, seatLimit := a.caps(agentID)
	over := (orgLimit > 0 && spend.OrgUsed > orgLimit) || (seatLimit > 0 && spend.AgentUsed > seatLimit)
	return over, nil
}

// resumer re-enters a suspended turn on this node.
//
// It is the coordinator's one seam into the agent layer, and the direction
// matters: the sandbox package holds the state as an OPAQUE blob and this
// decodes it, so the wire format's shape stays in the agent layer that
// understands it.
type resumer struct{ engine *Engine }

var _ sandbox.Resumer = (*resumer)(nil)

// Resume re-enters the suspended turn a completion or an answer names.
//
// NO PANIC LEAVES THIS FRAME, for the reason [Dispatcher.Dispatch] gives on the
// other path. A phase that panics is recovered inside the turn loop; anything
// else that panics while re-entering a turn is recovered here and abandoned.
// Unrecovered, it skipped the coordinator's revert as well as its settle, so
// the run row was stranded in resumed while the queue redelivered a completion
// the claim then refused.
func (r *resumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	return r.engine.guardResume(ctx, req.Run, func() error { return r.resume(ctx, req) })
}

// resume is [resumer.Resume]'s body, separated so the recovery around it is
// one deferred call.
func (r *resumer) resume(ctx context.Context, req sandbox.ResumeRequest) error {
	state, ok, err := execstate.Decode(req.Run.ExecuteState)
	if err != nil {
		// A state this build cannot read is a ROUTING failure, not a run
		// failure: a peer on the version that wrote it can resume this, so
		// the completion must go back rather than be settled here.
		return fmt.Errorf("%w: %w", sandbox.ErrResumeUnavailable, err)
	}
	if !ok {
		return fmt.Errorf("%w: run %s has no suspended conversation",
			sandbox.ErrResumeUnavailable, req.Run.TurnID)
	}
	// ONE EPOCH for the whole resume: the seat checked here, the organization
	// it belongs to, and the company the resumed turn then runs in (see
	// [resumeInput.Company]). A second read of the engine's company is a
	// different epoch once an apply lands in between, and a seat this check
	// found could be gone from it, which fails the run with "not an agent seat"
	// instead of routing the completion to a node that has the seat.
	company := r.engine.Company()
	if company == nil {
		// A node with no applied revision has no seat to resume into, and
		// a peer that has one can. Read through a nil company this was a
		// nil dereference.
		return fmt.Errorf("%w: this node has no applied company to resume run %s into",
			sandbox.ErrResumeUnavailable, req.Run.TurnID)
	}
	seat := company.Org.AgentSeatByHandle(req.Run.AgentHandle)
	if seat == nil {
		// The seat is gone from this epoch — decommissioned, or this node
		// is on a revision that never had it. Either way the resume belongs
		// somewhere else.
		return fmt.Errorf("%w: seat %q is not in this node's company",
			sandbox.ErrResumeUnavailable, req.Run.AgentHandle)
	}
	return r.engine.resumeTurn(ctx, resumeInput{
		Company: company,
		Run:     req.Run,
		State:   state,
		Turn: &turnctx.Turn{
			// THE SAME RUN AND THE SAME UNIT OF WORK the suspended
			// turn had, both read off the row: the resume re-enters
			// that run, and its writes stay idempotent against the
			// trigger the run was dispatched for.
			RunID: req.Run.TurnID, WorkKey: req.Run.UnitOfWork(),
			Seat: seat, Org: company.Org,
			Depth: req.Run.DelegationDepth, Chain: req.Run.DelegationChain,
		},
		Answer:        req.Answer,
		Success:       req.Success,
		Trigger:       req.Trigger,
		CostUSD:       req.CostUSD,
		DeliveredRefs: req.DeliveredRefs,
	})
}

// guardResume runs one resume, abandoning it if it panics.
//
// It publishes the unhandled-exception guard itself because the frame that
// would have, the resumed turn's own telemetry, is what did not run: without
// it the seat renders as whatever it was last doing rather than AFK. The run's
// own row names the seat, and the live epoch is preferred where it still does,
// so a seat renamed since the run detached is addressed as it is now.
func (e *Engine) guardResume(ctx context.Context, run sandbox.PendingRun, resume func() error) (err error) {
	defer func() {
		if panicked := turn.Recovered(recover()); panicked != nil {
			err = e.resumePanicked(ctx, run, panicked)
		}
	}()
	return resume()
}

// resumePanicked is [Engine.guardResume]'s recovery.
func (e *Engine) resumePanicked(ctx context.Context, run sandbox.PendingRun, panicked *turn.PanicError) error {
	log.ErrorContext(ctx, "sandbox_resume_panicked", "turn_id", run.TurnID,
		"seat", run.AgentHandle, "panic", panicked.Value, "stack", panicked.Stack)
	role, agentID := run.Role, run.AgentID
	if live, id := seatIdentity(e.Company(), run.AgentHandle); live != "" {
		role = live
		if id != "" {
			agentID = id
		}
	}
	trace := events.TraceContext{TraceID: run.TraceID, SpanID: run.SpanID}
	if breach := panicBreach(role, agentID, run.TurnID, trace, panicked); breach != nil {
		e.observe(ctx, breach)
	}
	return fmt.Errorf("%w (%s): %w", sandbox.ErrResumeAbandoned, turn.AbandonedPanicked, panicked)
}

// resumeInput is one re-entry, assembled.
type resumeInput struct {
	// Company is the epoch the resume was admitted under: the one whose
	// organization holds Turn's seat. The resumed turn runs in it rather than
	// reading the engine's company again, because that read is the NEXT epoch
	// once an apply lands between the two, and the seat the admission found
	// may not be in it.
	Company *Company
	Run     sandbox.PendingRun
	State   execstate.State
	Turn    *turnctx.Turn
	Answer  string
	Success bool
	Trigger *events.Event

	// CostUSD and DeliveredRefs are what the collected run reported, for the
	// resumed phase's own event. Zero when a person's answer resumed a
	// parked clarification: nothing was collected.
	CostUSD       float64
	DeliveredRefs []string
}

// resumeTurn re-enters a suspended turn.
//
// THE SAME TURN, not a new one: the same id, the same trigger, and the ledger
// the suspended turn had. What is rebuilt rather than restored is everything
// LIVE — the budget meter, the phase recorder, the config pin — because those
// belong to the node doing the resuming, not to the one that suspended.
//
// The config pin is the interesting one: this turn pins the epoch live AT
// RESUME, not the one it suspended under. A run can be parked for days waiting
// on a person, and re-entering under a revision that has since been deleted
// would resume a turn into a company that no longer exists. The cost — a turn
// observing a config change across its own suspension — is real and accepted,
// and it is why suspension is a phase boundary rather than a mid-round pause.
func (e *Engine) resumeTurn(ctx context.Context, in resumeInput) error {
	// A SUSPEND IS A RETURN, AND A SPAN CANNOT SURVIVE IT. The suspending
	// phase's span ended when that phase returned; the process may have
	// exited, the seat may have moved node, and days may have passed. So the
	// resume does not continue a span — it RECONSTRUCTS the suspended one as
	// a remote parent from the ids on the run's own row, and opens a new
	// span beneath it. That is the honest shape: two spans in one trace,
	// with the wait between them visible as the gap it actually is.
	//
	// A run written by a build before those ids were stored carries none,
	// and WithRemote turns that into a fresh root rather than refusing to
	// resume — a rolling upgrade guarantees some of those exist.
	ctx = tracing.WithRemote(ctx, events.TraceContext{
		TraceID: in.Run.TraceID, SpanID: in.Run.SpanID,
	})
	ctx, span := tracing.Start(ctx, "engine", "agent.turn.resume",
		attribute.String("crewlet.seat", in.Turn.Handle()),
		attribute.String("crewlet.turn_id", in.Run.TurnID),
		// The unit of work beside the run, so the two halves of a
		// suspended turn answer the same trace query as the dispatch
		// span that started it.
		attribute.String("crewlet.work_key", in.Run.UnitOfWork()))
	defer span.End()

	// THE INDICATOR A RESUMED TURN SHOWS, which comes from one of two places
	// and never from both. Ended below however the resumed turn goes.
	//
	// REJOIN FIRST, because a resume a BOX'S COMPLETION drove has nothing to
	// raise from: the suspended half ended with keepAlive, so the hold is
	// already up under this same turn id and the box's minutes are visible to
	// whoever is waiting — and a parked run's row carries no chat metadata by
	// design, the message that woke the suspended turn being days gone and
	// never this node's to keep, while the conversation keys the row does
	// carry are partition keys rather than addresses in a channel. A fresh
	// raise there would assert a conversation this turn cannot prove it is
	// in, over an indicator that is already up.
	//
	// BEGIN FROM THE TRIGGER WHERE NO HOLD IS LEFT, because that is a resume a
	// PERSON'S ANSWER drove: the park released the hold, and the answer is an
	// ordinary chat message carrying the conversation to raise in. Nil where
	// neither holds — a completion whose session is not on this node, the seat
	// having moved while the box ran or this process having restarted: the
	// indicator that node raised lapses on the backend's own expiry, and
	// nothing here invents a thread for it.
	//
	// See [Engine.resumeWorkingStatus], which owns that order and reports
	// which of the two answered.
	status, rejoined := e.resumeWorkingStatus(ctx, in.Turn.Handle(), in.Run.TurnID, in.Trigger)
	// TRUE UNTIL THE TURN ACTUALLY RUNS, BUT ONLY ON ONE OF THE TWO ROUTES.
	// Every early return below is a RETRY rather than an ending — a reply
	// this build cannot read and a runner that could not be built both leave
	// the coordinator to revert its claim — but the claim reverts to exactly
	// the status it was taken FROM, and the two routes came from opposite
	// facts:
	//
	//   - A BOX'S COMPLETION was claimed from a live run, so the revert puts
	//     it back and the completion comes round again. Nothing else could
	//     re-raise this indicator — the run's row carries no chat metadata,
	//     so a later resume has only the hold to take back — where on the
	//     dispatch path a redelivered trigger simply raises a fresh one. So
	//     it is KEPT.
	//   - A PERSON'S ANSWER was claimed from a question still open, so the
	//     revert puts the run back to awaiting THEM. Nothing is working, and
	//     an indicator over that wait tells the one person who could move it
	//     that nobody needs them — the same lie the park exists to stop. So
	//     it is CLEARED, and this is the only place that clear happens: the
	//     coordinator's revert reports no stop, deliberately, because the
	//     same revert on the completion route puts a run back to a box that
	//     is still working. See [sandbox.Coordinator.unclaim].
	//
	//     THE MESSAGE ITSELF IS HANDED BACK, not spent. The offer reports
	//     [sandbox.AnswerDeferred] — the run is awaiting THIS answer again
	//     — so the dispatcher NAKs the delivery instead of letting it be
	//     run as the ordinary chat message it looks like, and the
	//     redelivery, once the queue's backoff has passed, raises its own
	//     indicator off its own trigger. It used to fall through, which
	//     answered the person with a turn rather than with the coding run
	//     they were replying to and left that run waiting for a further
	//     message. See [Dispatcher.answered].
	working := rejoined
	defer func() { endWorkingStatus(ctx, status, working) }()

	company := in.Company
	if company == nil {
		// Not a second read of the epoch, which is the one thing this
		// must not do (see [resumeInput.Company]). Handed back rather
		// than settled: a caller that assembled a resume without its
		// epoch is a defect here, and a peer can still resume the run.
		return fmt.Errorf("%w: run %s was handed to resumeTurn without the "+
			"company its seat was resolved in", sandbox.ErrResumeUnavailable, in.Run.TurnID)
	}
	resumedReply, err := resumeReply(in.Run)
	if err != nil {
		return err
	}
	tel := e.describeResume(ctx, company, in)
	turnIdentity := tel.runnerTurn(company, in.Run.DelegationDepth,
		in.Run.DelegationChain, resumeTask(in), resumedReply)
	r, err := company.RunnerFor(in.Turn.Handle(),
		e.seatRegistry(company, in.Turn.Handle()), RunnerInput{
			Task: resumeTask(in),
			// THE RUNNER NEEDS IT TOO, not just the loop below. This
			// field reaches runner.Config.Reply, which is what
			// submit_work's own citation check reads — so a resumed turn
			// with this unset accepted `no_action` on a turn somebody was
			// waiting on, and accepted a citation naming any surface at
			// all, before the loop's later checks ever ran.
			Reply:     resumedReply,
			Publisher: e.backends.Queue,
			Turn:      turnIdentity,
			// THE SAME RUNTIME THE TURN SUSPENDED UNDER — supplied here for
			// the turn's LATER rounds, not for this one: whether the phase
			// being re-entered was agentic is the state's own answer (see
			// [execstate.State.AgentRun]), but a turn that loops to another
			// iteration must run that one the same way it ran the first.
			AgentRun: e.agentRunFor(company, in.Turn.Handle(), turnIdentity.Context),
			Budget:   e.meterFor(company, in.Turn.Handle()),
			// A resumed Execute loop can exhaust its rounds like any other,
			// and it is the phase most likely to: it comes back mid-task with
			// its budget already partly spent.
			Judge:     e.judgeFor(company, in.Turn.Handle()),
			Remaining: e.remainingFor(company, in.Turn.Handle()),
			// THE SAME FENCE THE DISPATCH PATH GETS, and a resume needs it
			// more than a fresh turn does: this loop was parked across a
			// coding run that may have taken an hour, and the node that
			// resumes it is not always the node that suspended it. The
			// grant it closes on is the one THIS node holds now — which
			// is the correct anchor, since the resume is what this node is
			// admitted for.
			Fence: e.seatFence(in.Turn.Handle()),
			// The working indicator's phase updates, on the same terms as
			// the dispatch path: the resumed Execute loop opens a phase
			// like any other, and a reader watching the thread should see
			// it move when the box's answer lands.
			OnPhase: func(ph phase.Phase) { status.Phase(ph.String()) },
			// THE SKILL REGISTRY, which this call site omitted. With nil
			// Skills the runner's guardFor returns nil, so the load-before-use
			// gate was disarmed for every resumed turn: a seat could call a
			// tool whose required skill it had never loaded, on the one path
			// where nobody would notice — and the two RunnerFor sites must
			// agree or a turn changes shape by being resumed.
			Skills: e.skills,
			// NO onboarding on a resume. The pass is a seat's FIRST turn, and
			// this turn already ran its first phase — running it here would
			// spend the resumed turn's opening on orientation for a seat that
			// is mid-task.
			Resume: &runner.Resume{
				State: in.State, Answer: in.Answer,
				// WHICH BOX DID THE WORK. Off the run's own row, because
				// this process may not be the one that launched it — and
				// it is the only thing that can say a phase ran in a
				// sandbox rather than in this process's own tool loop.
				Run: runner.RunRecord{
					CodingAgent:   in.Run.CodingAgent,
					SandboxID:     in.Run.SandboxID,
					CostUSD:       in.CostUSD,
					DeliveredRefs: in.DeliveredRefs,
				},
				// THE RUN'S OWN TOOL CALLS, off its durable row. An
				// agent-mode executor called them over the bridge, possibly
				// in another process, so this list is the only record of
				// what the phase did — its submission included.
				Bridged: bridgedCalls(in.Run.BridgeCalls),
			},
		})
	if err != nil {
		return err
	}
	// NO WALL-CLOCK CAP ON A RESUME. The cap bounds the turn a fire started;
	// a detached sandbox run can legitimately outlive it, and the resumed
	// half is finishing work the box already did rather than starting more.
	res, err := turn.Run(ctx, r, company.TurnSettings(0),
		resumeInputFor(in, resumedReply))
	// THE TURN RAN, so from here the indicator follows what it concluded
	// rather than the retry rule above: a resumed turn that suspended AGAIN
	// keeps it, because the same box is still working.
	working = res.Suspended
	e.publishTurnCompleted(ctx, tel, r.Spend(), res, err)
	if err != nil {
		if reason, abandon := turn.Abandon(res, err); abandon {
			// The same decision the dispatcher makes on the other path
			// (see (*Dispatcher).abandon), from the same rule, taken here
			// in this subsystem's own vocabulary: the coordinator reads
			// the sentinel and leaves the claim taken, so the completion
			// is not redelivered into a conversation a retry must not
			// re-enter.
			//
			// A resumed turn is the one most likely to qualify. It
			// re-enters the executor's suspended loop with the whole
			// pre-suspend conversation, and the round that called
			// run_sandbox was never closed, so its writes are in no
			// ledger and a replay would repeat every one of them.
			//
			// The indicator comes down with it: the run is settled,
			// the box reclaimed and the record deleted, so nothing is
			// coming back for this turn.
			return fmt.Errorf("%w (%s): %w", sandbox.ErrResumeAbandoned, reason, err)
		}
		// Reverted, so the turn is not over — and BOTH ROUTES BRING THIS
		// SAME DELIVERY BACK rather than wait for a further one.
		//
		// A BOX'S COMPLETION is NAK'd by the seat's control-topic handler
		// and redelivered on the broker's own backoff, to this node once
		// it recovers or to the seat's next owner, until its delivery
		// budget is spent.
		//
		// A PERSON'S ANSWER takes the same return for the same reason:
		// the offer reports [sandbox.AnswerDeferred] — the run is awaiting
		// THIS reply again — so the dispatcher hands the delivery back
		// with a NAK rather than letting it be the ordinary chat message
		// it looks like, bounded by the deliveries the message itself has
		// left ([sandbox.AnswerDeliveryReserve], the only clause a seat
		// handoff does not reset), by [sandbox.MaxAnswerAttempts] within
		// this process, and by the run's own pause_ttl_seconds — and let
		// go to the ordinary route past any of them. This comment used to say the resume went back to
		// awaiting the person "for the conversation's next message rather
		// than for a redelivery of this one", which described the defect
		// rather than the design: the reply that carried the answer was
		// spent on an unrelated turn while the run that asked waited out
		// its pause TTL for a message that may never come.
		//
		// ON THE ROUTE'S OWN TERMS, exactly as the seed above: this is the
		// same retry rule reached one step later, over the same revert.
		working = rejoined
		return err
	}
	// A resumed turn that suspended AGAIN persists its new conversation the
	// same way the first one did — the coordinator sees the row back in
	// running and leaves the box for the next completion — and keeps its
	// indicator on the same terms, off the ROW rather than off the intent.
	if res.Suspended {
		working = stillWorking(e.persistSuspension(ctx, r, in.Run.TurnID))
	}
	e.recordResume(ctx, in, res)
	return nil
}

// recordResume files a finished resumed turn against the conversation it was
// serving when it detached.
//
// ONLY THE DISPATCHER WROTE THIS, so a turn that ended here — which is every
// turn that did code work — left the conversation ledger untouched. The
// thread's history then stopped at the moment the run detached, and the
// seat's next turn on it read a conversation in which the coding work had
// never happened: no record of what was built, and nothing to stop it being
// planned again.
//
// A turn that suspended again records nothing — [Dispatcher.RecordSession]
// declines it, for both paths and for the same reason: it has not finished,
// so there is no reply to file. The completion that eventually lands comes
// back through this same frame and records then.
func (e *Engine) recordResume(ctx context.Context, in resumeInput, res turn.Result) {
	// THE RUN THE ROW NAMES, and the work key it rode in on: a resume
	// re-enters the run that suspended, so the entry names that one rather
	// than a fresh id, and it dedupes against the same trigger the dispatch
	// that launched it did.
	//
	// AND UNDER THE CONVERSATION IT REPORTS BACK TO, which is the
	// conversation its answer was matched on and never the partition beside
	// it: for a direct message those are different values, and filing here
	// under the batch would put the coding work in a row the seat's next
	// turn on that DM never looks up — the same silence this frame exists
	// to end.
	e.dispatch.RecordSession(ctx, in.Turn.Handle(), in.Run.Conversation(),
		in.Run.TurnID, in.Run.UnitOfWork(), resumeTask(in), res, e.dispatch.now())
}

// resumeTask is the brief the resumed turn re-enters with.
//
// Taken from the STATE rather than rebuilt from the trigger: the trigger may
// no longer be readable days later, and the task the turn was working on is a
// fact the suspension already captured.
func resumeTask(in resumeInput) string {
	if in.State.Task != "" {
		return in.State.Task
	}
	return in.Run.TaskDescription
}

// resumeInputFor renders a parked run coming back as the loop's own
// [turn.Input].
//
// A NAMED MAPPING for the reason [turnInputFor] is one on the dispatch path,
// and this literal proved it twice over: every field here is one the resumed
// loop cannot derive for itself, and each degrades SILENTLY without. A missing
// History forgets the rounds that closed before the box and re-fires their
// deliveries; a missing Reply skips every delivery gate; a missing Round hands
// the turn a whole fresh `max_iterations` and re-numbers rounds the suspended
// half had already published phase records under. Three of the five come off
// the parked row rather than off anything in this process, which is precisely
// why nothing else can notice one going missing.
func resumeInputFor(in resumeInput, reply turn.Reply) turn.Input {
	return turn.Input{
		RunID:   in.Run.TurnID,
		Depth:   in.Run.DelegationDepth,
		Reply:   reply,
		History: in.State.Iterations,
		Resume:  true,
		Round:   in.State.Round,
	}
}

// resumeReply is the delivery obligation a parked run comes back with.
//
// OFF THE ROW, and it cannot come from anywhere else. The resumed turn never
// sees the trigger that raised the obligation, and the event that carries the
// completion here is the run FINISHING rather than the ask, so [ReplyFor] over
// it would answer "nobody is waiting" for every turn somebody is waiting on.
//
// AN ABSENT VALUE IS [turn.NoReply], which is the reading [sandbox.PendingRun]
// states for it: the column is `reply,omitempty`, nothing ever rewrites a
// parked row, and a run launched before the field existed therefore carries
// none. Read as [turn.ReplyUnset] instead, those rows were refused by
// [Company.RunnerFor] and could never be resumed at all, so a box that had
// already done the work was collected and its answer dropped.
//
// A value that is PRESENT and unrecognised is refused rather than defaulted: it
// was written by a build that knows a kind this one does not, and guessing at
// who is waiting is the half of the delivery question this engine exists to get
// right. The refusal is a ROUTING failure, like a state this build cannot
// decode, so the completion goes back for a peer that can read it.
func resumeReply(run sandbox.PendingRun) (turn.Reply, error) {
	if run.Reply == "" {
		return turn.NoReply(), nil
	}
	reply := turn.ParseReply(run.Reply)
	if !reply.Valid() {
		return turn.Reply{}, fmt.Errorf(
			"%w: run %s carries reply %q, which is not one of %q, %q or %q",
			sandbox.ErrResumeUnavailable, run.TurnID, run.Reply,
			turn.ReplyNone, turn.ReplyTool, turn.ReplyEngine)
	}
	return reply, nil
}

// persistSuspension writes a turn's suspended conversation to its row, which
// is also what OPENS the run to the completion poll, and reports whether the
// turn is actually coming back.
//
// Called the moment the turn returns Suspended, because the runner holds the
// conversation only until its frame unwinds. Until this lands the run sits in
// [sandbox.StatusLaunching] and nothing polls or claims it — the job is
// already executing, and a job that finishes first would otherwise be
// collected against a row with nothing to resume into.
//
// EVERY WAY THIS CAN FAIL FAILS THE RUN rather than dropping the suspension: a
// row with no state is one nothing can resume, and failing here, while the
// box is still in the engine's hands and the seat's owner is still this
// process, is far better than leaving a launching row to hold a box until its
// seat happens to move.
//
// # Why it answers, and why the answer is three-valued
//
// A caller that has to know whether anything is still working cannot read that
// off [turn.Result] — every path below leaves it Suspended while settling the
// run, so the turn's own intent says "coming back" on exactly the endings
// where nothing is. The working indicator is that caller: driven off the
// intent it stayed up for the life of the process on every failure here.
//
// The three answers are the ordinary three, so the middle one is not collapsed
// into the loss: (true, nil) the row is open and the poll will resume it;
// (false, nil) the run is settled and definitively will not; and a non-nil
// error for the one case nothing can establish — a write that failed MAY have
// landed, and [sandbox.Coordinator.FailRun] settles only a run still
// launching, so a row this reports as unwritten can be one the completion poll
// resumes. The error is for deciding, not for logging: every failure path here
// has already said what it did and why (see [Engine.failSuspension]), and no
// caller fails a turn over it — the run is settled either way.
func (e *Engine) persistSuspension(ctx context.Context, r *runner.Runner, turnID string) (bool, error) {
	if e.sandboxPending == nil || e.sandboxCoordinator == nil {
		// No store and no coordinator: nothing recorded the run, nothing
		// polls it, and nothing will ever resume this turn.
		return false, nil
	}
	suspension, ok := r.Suspended()
	if !ok {
		e.failSuspension(ctx, turnID, "sandbox_suspension_missing",
			"the turn suspended but recorded no conversation", nil)
		return false, nil
	}
	blob, err := execstate.Encode(suspension.State)
	if err != nil {
		e.failSuspension(ctx, turnID, "sandbox_suspension_unserializable",
			"the suspended conversation could not be serialized", err)
		return false, nil
	}
	suspended, err := e.sandboxPending.MarkSuspended(ctx, turnID, blob)
	if err != nil {
		e.failSuspension(ctx, turnID, "sandbox_suspension_unwritable",
			"the suspended conversation could not be written", err)
		// UNKNOWN, not lost: the write may have landed, and the settle
		// that follows it declines a row that is no longer launching.
		return false, fmt.Errorf("engine: recording the suspension of turn %s: %w", turnID, err)
	}
	if !suspended {
		// The row is not launching, so this suspension has nowhere to go:
		// either the launch never recorded it, or its tail has already
		// been claimed and settled by somebody else. Overwriting either
		// one is worse than failing.
		e.failSuspension(ctx, turnID, "sandbox_suspension_not_launching",
			"the run was no longer launching when its conversation was written", nil)
		return false, nil
	}
	return true, nil
}

// failSuspension settles a run whose suspension has nowhere to go, and says
// why, in the one voice all four failure paths share.
//
// SETTLED, NOT MARKED. The job is already executing in its box, and writing a
// failed status onto the record stranded that box: a record that is not
// active is read by no recovery pass and polled by no waiter, so the box ran
// to its provider's TTL, billed, with nothing left to reclaim it. The
// coordinator settles it like every other lost turn, while this node still
// owns the seat, and the loss is announced rather than left as silence. It
// settles only a run still launching (see [sandbox.Coordinator.FailRun]): a
// write reported as failed can have landed, and a run it moved to running is
// one the completion poll resumes.
func (e *Engine) failSuspension(ctx context.Context, turnID, event, detail string, cause error) {
	args := []any{"turn_id", turnID,
		"detail", detail + "; a run still launching cannot be resumed, so its box is reclaimed and the run ended"}
	if cause != nil {
		args = append(args, "error", cause)
	}
	log.ErrorContext(ctx, event, args...)
	if err := e.sandboxCoordinator.FailRun(ctx, turnID,
		types.SandboxFailureSuspensionUnrecorded, detail); err != nil {
		log.WarnContext(ctx, "sandbox_suspension_settle_failed", "turn_id", turnID, "error", err,
			"detail", "the run's record could not be read, so its box was not reclaimed; the "+
				"seat's next recovery pass reaps a run left launching")
	}
}

// launcher is the run_sandbox tool's seam into the sandbox layer.
//
// It exists to assemble a launch from things only the engine holds — the
// seat's config block, the run environment, the box spec — so the tool itself
// stays a thin adapter that reads a brief and reports a suspension.
type launcher struct{ engine *Engine }

var _ builtin.SandboxLauncher = (*launcher)(nil)

func (l *launcher) Launch(ctx context.Context, t *turnctx.Turn, brief string) (sandbox.LaunchResult, error) {
	e := l.engine
	manager, pending := e.sandboxManager(), e.sandboxPending
	if manager == nil || pending == nil {
		return sandbox.LaunchResult{}, fmt.Errorf("this engine has no sandbox backend configured")
	}
	seat, err := t.RequireSeat()
	if err != nil {
		return sandbox.LaunchResult{}, err
	}
	company := e.Company()
	gate := seatSandbox(company, seat.Handle())
	if gate == nil || !gate.Enabled {
		// Belt and braces: the tool is only on a sandbox-enabled seat's
		// surface, but the surface is built from a snapshot and a seat can
		// lose its gate across an apply mid-turn.
		return sandbox.LaunchResult{}, fmt.Errorf("this seat's sandbox is not enabled")
	}
	if config.Placement(gate.RunIn) == config.PlacementSelf {
		// `self` means this seat's code work rides its own executor run,
		// which is a coding CLI in agent mode and already holds a shell,
		// an editor and a checkout. A second box beside it would give the
		// seat two filesystems with the work in the one the turn cannot
		// see — so the tool refuses and says which shell to use, rather
		// than provisioning a box whose output is invisible.
		return sandbox.LaunchResult{}, fmt.Errorf(
			"this seat runs its code work in its own agent session (role.sandbox.run_in: " +
				"self) — use the shell and editor you already have rather than " +
				"starting a second box")
	}

	// THE PRE-FLIGHT BUDGET FLOOR. turn_engine.sandbox_min_budget_tokens
	// was validated, schema'd and documented and read by nothing, so a
	// company that set it got a new revision and no behaviour.
	//
	// It is checked HERE rather than in the tool, because this is the frame
	// that holds the seat's counter — and refused as a launch error, which
	// the tool reports back to the model as a failed call. That is the
	// point of a floor: a coding run costs a box, a clone and a toolchain
	// install before it produces a token, so a seat with no headroom must
	// learn that now and fall back to its own tools rather than after the
	// job has died mid-run having delivered nothing.
	if err := sandboxHeadroom(ctx, e.remainingFor(company, seat.Handle()),
		company.Config.TurnEngine.SandboxMinBudgetTokens); err != nil {
		return sandbox.LaunchResult{}, err
	}

	// A box this turn already has, paused from an earlier call. An EMPTY id
	// on an existing row means that box is gone — reaped past its pause TTL,
	// or torn down under a zero TTL — so this call provisions a fresh one
	// and the work re-seeds from the pushed branch.
	reuse := ""
	if existing, found, err := pending.Get(ctx, t.RunID); err == nil && found {
		reuse = existing.SandboxID
	}

	setup := append(manager.DefaultSetup(), seatSetupSteps(gate.Setup)...)
	servers := sandboxMCP(l.engine.resolver(), company, seat, gate)
	// The seat's own model and login, resolved from llm_sandbox — which
	// falls back to `llm`, because sandboxed work IS this seat's own work
	// running somewhere else, and `llm` is what that work runs on.
	agentLLM, credentials, credentialEnv := sandboxLLM(company, seat)
	env := underlay(e.sandboxEnv(seat, gate, setup), credentialEnv)
	spec := manager.BuildSpec(sandbox.SpecInput{
		// The seat's cell, empty inheriting providers.sandbox's default.
		// Resolved at LAUNCH rather than at config time, for the same
		// reason coding_agent is: a catalogue change reaches every seat
		// that named nothing without rewriting their blocks.
		Placement:       sandbox.Placement(gate.RunIn),
		CodingAgent:     string(gate.CodingAgent),
		PauseTTL:        pauseTTL(gate),
		MaxTurns:        gate.MaxTurns,
		Env:             env,
		CredentialFiles: credentials,
	})
	if err := sandboxCredentials(company, seat, phase.Sandbox, spec.Placement, env); err != nil {
		return sandbox.LaunchResult{}, err
	}

	return sandbox.Launch(ctx, manager, pending, e.backends.Queue, sandbox.LaunchRequest{
		Turn:       sandboxTurnRef(ctx, t, seat.Name),
		Brief:      brief,
		Task:       t.Task,
		Setup:      setup,
		Spec:       spec,
		LLM:        agentLLM,
		MCPServers: servers,
		ReuseBox:   reuse,
	})
}

// sandboxTurnRef is what a detached run's durable row records about the turn
// that launched it.
//
// ONE BUILDER FOR BOTH LAUNCH PATHS — the run_sandbox tool and a cli-agent
// executor handed its whole turn — because it is one row, the same facts and
// the same reasons: the resume happens in another process, days later, and can
// recover none of this from a trigger that is long gone. Written out twice it
// drifted inside a single commit: one copy put the partition in both
// conversation fields, which compiles, because both are strings.
//
// EVERY FACT OFF THE TURN'S OWN EPOCH, never the engine's current company. An
// apply landing mid-turn is the next epoch, and a company renamed there
// derives a DIFFERENT agent id for the same seat — so a row taking its id from
// the live company named an agent none of that turn's other events did, and
// its announcements split the seat in two on every surface that groups by id.
// One path already took the id from the turn and said why; the other did not,
// which is the whole hazard of writing the rule twice.
//
// The ROLE NAME is the caller's, because each already holds the seat it
// resolved and the turn's copy is the same pointer — nothing is gained by
// deriving it a second time here.
//
// The TRACE comes from the ACTIVE span, so the box's spans nest under the call
// that started them rather than appearing as unrelated work minutes later.
// TurnRef carried these two since the OTLP receiver was written and the
// run_sandbox path left both empty: PendingRun.TraceID was always "", RunEnv
// minted no telemetry token for any launch (it refuses to scope one to an
// empty trace, which is the one property that scoping has), and describeResume
// filed every resumed turn under no trace at all.
func sandboxTurnRef(ctx context.Context, t *turnctx.Turn, role string) sandbox.TurnRef {
	runTrace := tracing.TraceOf(ctx)
	return sandbox.TurnRef{
		// The id is derived from the turn's PINNED organization, like every
		// other fact about the seat here. The engine's current company is
		// the next epoch once an apply lands mid-turn, and a renamed
		// company derives a different id for the same seat.
		TurnID: t.RunID, WorkKey: t.WorkKey,
		AgentID: t.AgentID(), AgentHandle: t.Handle(), Role: role,
		Depth: t.Depth, Chain: t.Chain,
		TraceID: runTrace.TraceID, SpanID: runTrace.SpanID,
		// BOTH CONVERSATION VALUES, field for field with [turnctx.Turn] so
		// a swapped pair reads as one. The row has carried a conversation
		// since it was written and nothing set it: with it empty a resumed
		// turn had no way to say where its answer belonged, so it left no
		// conversation entry at all and the next turn on that thread read
		// history that stopped when the run detached.
		//
		// The conversation is where the resume reports and what admits a
		// person's answer; the partition states the batch this run was
		// launched from, which tells two runs parked on one direct message
		// apart and is all a peer predating the conversation can match on.
		PartitionKey:    t.PartitionKey,
		ConversationKey: t.ConversationKey,
		// The delivery obligation, so the resumed turn knows whether
		// anybody is waiting: it sees neither its trigger nor this frame,
		// and the row is the only place this can reach it from.
		Reply: t.Reply,
	}
}

// sandboxHeadroom refuses a launch below turn_engine.sandbox_min_budget_tokens.
//
// THREE-VALUED, like every other budget read in this engine: a seat with no
// counter is uncapped and passes, a seat below the floor is refused, and a
// read that FAILED is refused too — launching a box on an unknown budget is
// how a company discovers its ceiling by spending past it.
func sandboxHeadroom(ctx context.Context, remaining runner.Remaining, floor int) error {
	if floor <= 0 {
		return nil
	}
	if remaining == nil {
		// No counter anywhere in the epoch, which is a company with no
		// token budget rather than a read that could not be made.
		return nil
	}
	left, err := remaining.Remaining(ctx)
	if err != nil {
		return fmt.Errorf("this seat's remaining token budget could not be read, "+
			"and a coding run costs a box before it produces anything: %w", err)
	}
	if left < floor {
		return fmt.Errorf("this seat has %d tokens left and a coding run needs at "+
			"least %d (turn_engine.sandbox_min_budget_tokens): do the work with "+
			"your own tools, or report that the budget is exhausted", left, floor)
	}
	return nil
}

// sandboxMCP renders the seat's SCOPED coding-agent MCP surface.
//
// Only the servers role.sandbox.mcp.servers names — never the seat's whole MCP
// surface by default. A coding agent inside a box reaches whatever it is given
// with no per-tool control left, so what it is given is the decision, and it
// is made here rather than inherited.
//
// The credentials are the seat's OWN, inherited down the org chart at build
// time, so a seat gets the tokens it is entitled to and no others.
func sandboxMCP(env *config.Resolver, c *Company, seat *org.Role, gate *org.RoleSandbox) map[string]sandbox.MCPServer {
	if len(gate.MCP.Servers) == 0 {
		return nil
	}
	servers := make([]sandbox.MCPServer, 0, len(c.Config.MCPServers))
	for _, s := range c.Config.MCPServers {
		servers = append(servers, sandbox.MCPServer{
			Name: s.Name, Transport: sandbox.Transport(s.Kind()),
			Command: s.Command, Args: s.Args, Env: s.Env,
			URL: s.URL, Headers: s.Headers,
		})
	}
	// RESOLVED HERE, like the run env and for the same reason: an in-box
	// server has to authenticate, and Tier B stores its references
	// verbatim.
	credentials := make(map[string]map[string]string, len(seat.MCPEnv))
	for name, values := range seat.MCPEnv {
		resolved, missing := env.Map("mcp_env."+name, values)
		if len(missing) > 0 {
			log.Warn("sandbox_mcp_env_unresolved", "seat", seat.Handle(), "server", name,
				"hint", "the in-box MCP server will not authenticate")
		}
		credentials[name] = resolved
	}
	return sandbox.RenderMCP(servers, gate.MCP.Servers, credentials)
}

// pauseTTL reads the seat's override, distinguishing "inherit" from "never".
//
// UNSET MEANS INHERIT and an explicit zero means never pause — two genuinely
// different instructions, which is why both the config field and the manager's
// input carry a pointer rather than a sentinel number. A negative value is the
// field's earlier spelling of "inherit" and is read as one.
//
// NIL-SAFE, like [maxTurnsFor] and for the same seat: an agent-mode executor
// is placed by its own providers.llm entry and runs in a box whether or not
// role.sandbox was ever written, and reading the override off a block that
// does not exist panicked that seat's first launch — after the bridge session
// was opened and before anything would have closed it.
func pauseTTL(gate *org.RoleSandbox) *time.Duration {
	if gate == nil || gate.PauseTTLSeconds == nil || *gate.PauseTTLSeconds < 0 {
		return nil
	}
	d := seconds(*gate.PauseTTLSeconds)
	return &d
}

// seatSandbox is a seat's sandbox gate, or nil.
//
// KEYED ON THE HANDLE, which is what makes the answer single-valued. Two seats
// may carry one display NAME — it is an admission rule, so a revision stored
// before that rule runs with duplicates and a write merely cannot add one —
// and a walk matching on it returned whichever came first. A seat with no
// sandbox block would then be handed its namesake's, which is a coding run on
// another seat's setup steps, its credentials and its box.
//
// A handle is unique by a RUNNABLE rule instead: a company carrying two is
// refused outright, so there is no document on which this can be ambiguous.
// ONE LOOKUP, NOT A WALK OF ITS OWN, because the seat is what carries the
// block now.
//
// It used to walk the company DOCUMENT — first only the top-level `roles:`,
// which answered nil for every seat in a unit and refused each of them with
// "this seat's sandbox is not enabled" on a seat whose block said otherwise;
// then every seat at any depth, breaking at the first match. Both were
// walking the wrong thing: a stored revision carries no seats at all, so the
// walk returned nil for EVERY seat and code work became silently unavailable
// to the whole company.
//
// The seat's own [org.Role] holds it, so this asks the org the company is
// running, through the one lookup every other layer resolves a handle with —
// aliases and all. The "two seats with one handle" hazard the walk had to
// reason about cannot arise there, because that lookup answers a live handle
// before any retired one and a company carrying two live ones is refused.
func seatSandbox(c *Company, handle string) *org.RoleSandbox {
	if c == nil || c.Org == nil {
		return nil
	}
	seat := c.Org.Role(handle)
	if seat == nil {
		return nil
	}
	return seat.Sandbox
}

// sandboxEnv assembles the run environment.
//
// THE ENGINE CONTRIBUTES ONLY TOOL-AGNOSTIC FACTS: the agent's identity, as
// CREWLET_AGENT_HANDLE and CREWLET_AGENT_EMAIL — per-launch values static
// config cannot know, which a setup recipe maps into whatever shape its tool
// needs. Every tool-specific variable comes from config: an external token is
// DECLARED in role.sandbox.env or a setup step's env, and the engine never
// names one of its own.
//
// Precedence, later winning: identity, then the setup steps' contributions,
// then the seat's own env — so an operator's explicit value always beats a
// step's default.
func (e *Engine) sandboxEnv(seat *org.Role, gate *org.RoleSandbox, setup []sandbox.SetupStep) map[string]string {
	env := map[string]string{}
	if handle := seat.Handle(); handle != "" {
		env["CREWLET_AGENT_HANDLE"] = handle
	}
	if email := seat.Email; email != "" {
		env["CREWLET_AGENT_EMAIL"] = email
	}
	for key, value := range sandbox.SetupEnv(setup) {
		env[key] = value
	}
	// NIL-SAFE, because a seat legitimately has no block: an agent-mode
	// executor is placed by its own providers.llm entry, so it runs in a
	// box whether or not role.sandbox was ever written. Ranging over a nil
	// pointer's field panicked the seat's first launch.
	if gate != nil {
		for key, value := range gate.Env {
			env[key] = value
		}
	}

	// RESOLVED EXACTLY ONCE, here, at launch — not at load. These values
	// are where an operator declares a code-host token, and Tier B stores
	// its references verbatim so an exported revision carries no resolved
	// secret. Resolving at load as well would double-resolve and mangle any
	// secret whose real value contains a literal ${...}.
	resolved, missing := e.resolver().Map("role.sandbox.env", env)
	if len(missing) > 0 {
		// KEYS ONLY, never values, which may embed a partial secret. Flagged
		// per REFERENCE rather than per final value: an embedded form like
		// "Bearer ${TOKEN}" resolves to a truthy-but-broken "Bearer " when
		// the variable is unset, so testing the result would miss exactly
		// the composite shapes this field documents.
		names := make([]string, 0, len(missing))
		for _, m := range missing {
			names = append(names, m.Path)
		}
		log.Warn("sandbox_env_unresolved", "seat", seat.Handle(), "keys", names,
			"hint", "the coding agent will see these as empty; export the "+
				"variables or put them in the secret store")
	}
	return resolved
}

// sandboxManager is this node's manager, or nil.
func (e *Engine) sandboxManager() *sandbox.Manager {
	if e.sandboxCoordinator == nil {
		return nil
	}
	return e.sandboxCoordinator.Manager()
}

// buildSandboxRuntime builds this node's code-work machinery, or leaves it off.
//
// Called once at boot rather than on every apply: the coordinator holds the
// busy set and the waiter holds the poll loop, both facts about this PROCESS.
// The manager under them IS swapped on an apply, through SetManager, so a
// provider change reaches a running node without forgetting which seats are
// mid-run.
//
// SPLIT FROM startSandboxWaiter because the two need different things to
// exist. The coordinator must exist before equip, which registers run_sandbox
// only on a node that has one; the waiter needs the node, whose incarnation is
// what its fleet-singleton duty is claimed under. Doing both at once meant one
// of the two ran against a nil.
func (e *Engine) buildSandboxRuntime(company *Company) error {
	manager, err := buildSandbox(company.Config, e.resolver(), e.sandboxOtel)
	if err != nil {
		return err
	}
	if manager == nil {
		return nil
	}
	if e.backends == nil || e.backends.Fleet == nil {
		// A detached run's RECORD is what survives the turn that starts
		// it, so a node with no coordination store cannot offer code work
		// at all. Refused loudly rather than degraded: a sandbox whose
		// runs vanish is worse than no sandbox, because the box keeps
		// running and billing with nobody to collect it.
		//
		// The FLEET's store, not this node's: a run is recovered by
		// whichever node owns its seat next, and that node is not
		// reliably the one that launched it.
		return fmt.Errorf("providers.sandbox needs a coordination store: a detached " +
			"run's state is a fleet record, and without one a seat handoff orphans every box")
	}
	e.sandboxPending = sandbox.NewCoordStore(e.backends.Fleet)

	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: e.backends.Queue, Pending: e.sandboxPending, Manager: manager,
		Resume:  &resumer{engine: e},
		Account: e.sandboxAccountant(),
		// The per-run tool bridge dies with the run — see
		// [sandbox.CoordinatorOptions.Ended]. Idempotent, and reached
		// from every settle path, so a run that failed before it ever
		// had a box closes its session too.
		Ended: e.bridge.Close,
		// A run that stops with a turn still suspended into it takes the
		// working indicator down with it, whichever way it stopped: the
		// agent is waiting on a person, or is never coming back at all,
		// and either way the turn does not return to say so. ONE
		// FUNCTION FOR EVERY REASON, because to the indicator they are
		// one fact — the frame that raised it has already returned. See
		// [sandbox.CoordinatorOptions.Stopped].
		Stopped: e.releaseWorkingStatus,
	})
	if err != nil {
		return err
	}
	e.sandboxCoordinator = coordinator
	log.Info("sandbox_enabled", "placements", manager.Placements(),
		"default_run_in", string(manager.DefaultPlacement()),
		"coding_agent", manager.DefaultCodingAgent())
	return nil
}

// startSandboxWaiter starts the completion poll, once the node exists.
func (e *Engine) startSandboxWaiter(ctx context.Context, interval time.Duration) error {
	if e.sandboxCoordinator == nil {
		return nil
	}
	duty, err := e.waiterDuty(interval)
	if err != nil {
		return err
	}
	waiter, err := sandbox.NewWaiter(sandbox.WaiterOptions{
		Queue: e.backends.Queue, Pending: e.sandboxPending,
		Manager:  e.sandboxCoordinator.Manager(),
		Interval: interval,
		// The duty is claimed per tick: the waiter polls EVERY active run
		// in the company, not just this node's seats, so N nodes running it
		// unclaimed means N reconnects per box per tick and N racing
		// reapers.
		ClaimDuty: sandbox.DutyFunc(duty),
	})
	if err != nil {
		return err
	}
	e.sandboxWaiter = waiter
	waiter.Start(context.WithoutCancel(ctx))
	return nil
}

// stopSandbox halts the poll loop. The rows and the boxes are untouched: a
// detached run belongs to its row, and the next owner of its seat recovers it.
func (e *Engine) stopSandbox() {
	if e.sandboxWaiter != nil {
		e.sandboxWaiter.Stop()
	}
}

// sandboxAccountant charges collected runs, or nil where nothing counts them.
func (e *Engine) sandboxAccountant() sandbox.Accountant {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return sandboxAccountant{
		budgets: e.backends.Fleet,
		// The ORG cap and the seat's own, read off the epoch LIVE at charge
		// time rather than pinned to the turn: this charge lands after a run
		// that may have taken hours, and the cap the company is running
		// under now is the one it should be measured against.
		caps: func(agentID string) (int, int) {
			c := e.Company()
			id, err := uuid.Parse(agentID)
			if err != nil {
				return c.Config.TokenBudget, 0
			}
			return c.Config.TokenBudget, seatBudget(c.Org, c.Org.AgentSeatByID(id))
		},
	}
}

// waiterDutyName is the fleet singleton the sandbox waiter claims.
const waiterDutyName = "sandbox-waiter"

// waiterDuty gates the poll tick on holding the fleet's waiter duty.
//
// Nil where there is no coordination backend, which is the single-node case:
// that node always holds it, and a wrapper that always said yes would make a
// single node report itself as a fleet singleton.
//
// An interval whose duty no backend will grant is refused HERE, at start,
// rather than on every tick: the waiter fails closed on a claim error, so a
// refused TTL would leave it never ticking, every detached run hanging and
// every box losing its keepalive, with one warning per tick as the only sign.
func (e *Engine) waiterDuty(interval time.Duration) (schedule.DutyFunc, error) {
	if err := coord.CheckDutyTTL(coord.WorkerResource(waiterDutyName), waiterDutyTTL(interval)); err != nil {
		return nil, fmt.Errorf("engine: a sandbox poll interval of %v needs a %v waiter duty; "+
			"lower Options.SandboxPollInterval: %w", interval, waiterDutyTTL(interval), err)
	}
	if e.backends == nil || e.backends.Coord == nil {
		return nil, nil
	}
	// The TTL expression is spelled as the duty-TTL guard test expects it;
	// see TestEveryDutyTTLFitsTheDutyCeiling.
	return e.workerDuty(waiterDutyName, waiterDutyTTL(interval)), nil
}

// dutyTTLTicks is how many poll intervals the waiter duty survives without a
// re-claim, and dutyTTLFloor the shortest it may ever be.
//
// Three intervals is the same "do not flap on a blip" rule the scheduler duty
// and the seat heartbeat follow: the holder re-claims every tick, so this
// rides out two consecutive slow or failed claims without the duty moving. The
// invariant is the RATIO, not any duration — a duty that cannot outlive three
// of its own ticks moves on ordinary jitter, and one that outlives many is
// time the fleet has no waiter after a holder dies.
//
// The floor is for the other end: this code is driven at a sub-second cadence
// in tests, and three of those is a lease that lapses inside its own claim.
const (
	dutyTTLTicks = 3
	dutyTTLFloor = 30 * time.Second
)

// waiterDutyTTL is how long the waiter duty survives without a re-claim.
//
// DERIVED FROM THE INTERVAL IT GUARDS, and from nothing else. It used to be
// `3 * sandbox.DefaultPollInterval`, a compile-time constant that ignored the
// configured interval entirely, and then it was capped at the seat lease TTL,
// because duties shared the seat lease bucket and the KV refused a lease
// longer than that bucket's age. The cap was wrong in its own right: a poll
// interval longer than the seat lease TTL got a duty that lapsed between two
// of its own ticks, so the waiter moved to whichever peer ticked first on
// every tick. Duties now have a bucket of their own whose ceiling is
// [coord.MaxDutyTTL], so the ratio holds for every interval that ceiling
// admits, and [Engine.waiterDuty] refuses the ones it does not.
func waiterDutyTTL(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = sandbox.DefaultPollInterval
	}
	return max(dutyTTLTicks*interval, dutyTTLFloor)
}

// prepareSeat is the node's SeatReady hook: recover this seat's in-flight runs
// and start listening for their completions, BEFORE its mailbox opens.
func (e *Engine) prepareSeat(ctx context.Context, handle string, epoch int64, owner string) error {
	// THE SEAT'S OWN TOOLS FIRST, because OnAcquire's whole contract is
	// that everything the first turn needs is ready before the mailbox
	// opens. A seat that started consuming before its per-role children
	// were up would run its first turn with a surface missing exactly the
	// tools it acts through.
	company := e.Company()
	// startSeatServers files the registry alongside the bridge, so the two
	// cannot disagree about which children this seat has.
	e.startSeatServers(ctx, company, handle)

	// THE SEAT'S MEMORY, before it can take a turn. Its diary, episodes,
	// counterparties and skills were written on whichever nodes ran it
	// before, and placement moves seats — so without this the seat opens
	// its mailbox against a store that has never heard of it and runs its
	// first turn having forgotten everything it learned elsewhere.
	//
	// A failure REFUSES the seat, like every other step here. A peer that
	// takes it instead may hydrate cleanly, and a seat serving with
	// amnesia produces work its own history contradicts — which is worse
	// than the seat waiting for the next placement sweep.
	if e.memory != nil {
		if _, err := e.memory.Hydrate(ctx, handle); err != nil {
			return fmt.Errorf("carrying the seat's memory: %w", err)
		}
	}

	if e.sandboxCoordinator == nil {
		return nil
	}
	control, group := topics.AgentControl(handle), topics.AgentControlGroup(handle)
	if control == "" {
		return nil
	}
	// The subscription is attached BEFORE recovery, so a completion
	// published in the window between the two is held rather than dropped.
	// A detached run outlives its node, so that window is not hypothetical.
	if err := e.backends.Queue.Subscribe(ctx, control, group,
		func(ctx context.Context, ev *events.Event) queue.Result {
			if err := e.sandboxCoordinator.OnEvent(ctx, ev); err != nil {
				// NAK, so a completion this node could not settle comes
				// back — to this node once its store recovers, or to the
				// seat's next owner. Acking it would lose the turn.
				return queue.Nak(err)
			}
			return queue.Ack()
		}); err != nil {
		return fmt.Errorf("attaching the sandbox control topic: %w", err)
	}
	if err := e.sandboxCoordinator.RecoverSeat(ctx, handle, owner, epoch); err != nil {
		return err
	}
	// THE SEAT IS LIVE HERE, which is the fact the live projection has
	// always had a branch for and nothing ever published: an
	// `agent_spawned` is what clears a stale `terminated`, `offline` or
	// `afk` from a seat that has moved to this node, so without it a seat
	// whose last owner went away renders as broken until it happens to do
	// some work.
	e.publishSeatLifecycle(ctx, handle, types.AgentSpawned{})
	return nil
}

// publishSeatLifecycle announces a seat arriving on or leaving this node.
//
// LIVE-ONLY, and deliberately: placement moves seats between nodes on every
// rebalance, so a durable row per claim would fill the audit log with a fact
// about scheduling rather than about the company — which is why neither type
// is in [events.Category]'s map. What reads them is the live seat state, where
// "this seat is running here now" is exactly the question.
//
// THE ROLE, NOT THE HANDLE, because every other event the projection keys on
// carries the role name and a seat under two spellings is two rows.
func (e *Engine) publishSeatLifecycle(ctx context.Context, handle string,
	payload events.Payload) {

	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	role := e.seatRole(handle)
	if role == nil {
		// A HANDLE THIS EPOCH NO LONGER HAS, which is the ordinary way a
		// seat is released: the revision that removed it is already
		// current. There is no role to key the row on, and inventing one
		// from the handle would make a second row for the same seat.
		return
	}
	var ev *events.Event
	switch p := payload.(type) {
	case types.AgentSpawned:
		p.RoleName, p.Agent = role.Name, handle
		ev = events.New(p, tracing.TraceOf(ctx))
	case types.AgentTerminated:
		p.RoleName, p.Agent = role.Name, handle
		ev = events.New(p, tracing.TraceOf(ctx))
	default:
		return
	}
	ev.Source = role.Name
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_lifecycle_not_published", "type", ev.Type,
			"seat", handle, "error", err.Error(),
			"detail", "the live seat state keeps whatever this seat last showed")
	}
}

// releaseSeat is the node's SeatDone hook.
//
// The control subscription is DETACHED, never deleted: a completion published
// while the seat is between owners must be held for its successor, and the box
// it refers to is still real.
func (e *Engine) releaseSeat(ctx context.Context, handle string) {
	// FIRST, while the queue is still reachable and before anything this
	// release tears down: a seat that went away with no event left its
	// last state standing on every dashboard — stuck "working" in a phase
	// that ended, because `terminated` was a state nothing could reach.
	e.publishSeatLifecycle(ctx, handle,
		types.AgentTerminated{Reason: "the seat was released by this node"})
	// The seat's children die with its lease. The credentials in one ARE
	// that seat's identity, so a child left running would let this node go
	// on acting as a seat a peer has taken over — and the surface goes
	// with them, so nothing here can serve a turn through a dead client.
	// stopSeatServers drops the registry with the bridge, in one step.
	e.stopSeatServers(ctx, handle)

	// AND THE SEAT'S WORKING INDICATORS, for the reason the event above
	// exists: a seat that went away with no signal leaves its last state
	// standing, and on a chat surface that state is a live re-assertion
	// rather than a stale row. A turn that suspended into a detached coding
	// run deliberately leaves its indicator up, and the resume lands on
	// whichever node holds the seat when the box reports — so a node that has
	// handed the seat on would otherwise go on saying "is thinking…" every
	// refresh interval, for a turn it is not running, until the process died.
	//
	// DETACHED AND BOUNDED, the same shape the memory flush below takes: the
	// clear has to go out even when the release is a cancelled drain, and a
	// chat instance that has stopped answering must cost the drain seconds
	// rather than a client timeout per seat. See [statusTeardown].
	clearCtx, stopClear := statusTeardown(ctx)
	e.Status().ClearFor(clearCtx, handle)
	stopClear()

	// A LAST PUBLISH, then forget the seat. The publish is what makes a
	// graceful handoff lossless: whatever this node learned since its last
	// cycle reaches the changelog before the successor hydrates. Forgetting
	// the watermarks is what makes a LATER re-acquisition correct — resuming
	// from the old marks would skip exactly the memory the other node made
	// in between.
	//
	// Best effort, and it must be: this runs on the release path, where the
	// seat is already gone. A failure here costs the successor whatever was
	// written since the last cycle, which is the same bounded loss a crash
	// costs — and refusing to release would strand the seat for a full TTL.
	if e.memory != nil {
		// BOUNDED, because this runs on the release path — including a
		// drain, where every seat passes through here in turn. A broker
		// that has stopped answering must cost the drain a few seconds
		// per seat, not the whole shutdown.
		flushCtx, stopFlush := context.WithTimeout(
			context.WithoutCancel(ctx), memoryFlushTimeout)
		if _, err := e.memory.Publish(flushCtx, handle); err != nil {
			log.WarnContext(ctx, "seat_memory_not_flushed", "seat", handle, "error", err)
		}
		stopFlush()
		e.memory.Forget(handle)
	}

	if e.sandboxCoordinator == nil {
		return
	}
	e.sandboxCoordinator.ReleaseSeat(handle)
	control, group := topics.AgentControl(handle), topics.AgentControlGroup(handle)
	if control == "" {
		return
	}
	if _, err := e.backends.Queue.Detach(ctx, control, group); err != nil {
		log.WarnContext(ctx, "sandbox_control_detach_failed", "seat", handle, "error", err)
	}
}

// SeatHeldBySandbox reports whether a detached coding run HOLDS a seat, so it
// starts no new turn until the run settles.
//
// Exported for the operator surfaces and for a test: the inbox screening reads
// it internally through the dispatcher's conditions, but "is this seat busy on
// code work" is also a question a dashboard asks, and answering it from a
// second place would eventually answer it differently.
//
// It is NOT "does this seat have a run waiting for an answer" — a parked run
// frees its seat by design. See [sandbox.Coordinator.SeatRuns].
func (e *Engine) SeatHeldBySandbox(handle string) bool {
	if e.sandboxCoordinator == nil {
		return false
	}
	return e.sandboxCoordinator.SeatHeldBySandbox(handle)
}

// sandboxLLM resolves the model a coding run works under, and the credential
// that lets it reach one.
//
// THREE things travel, and they travel differently on purpose:
//
//   - The MODEL and its endpoint, so an agent that resolves
//     "<family>/<model>" against a catalogue addresses the right vendor at
//     the right host rather than the catalogue's default.
//   - A TOKEN, in the run environment, which reaches any box including a
//     remote one: it is one scoped, revocable variable.
//   - The credential FILES, only as a host-path map the LOCAL backend seeds
//     and writes back. They carry a refresh token whose rotation is shared
//     fleet state, so pushing them onto somebody else's VM is a materially
//     larger trust step than the token — which is why the map is offered and
//     each backend decides, rather than being exported like the rest.
//
// A seat with no resolvable sandbox model is not an error here: a company with
// no models takes no turn, so launches no run (see nomodels.go), and a run
// whose agent reads its credential from the environment needs none of this.
func sandboxLLM(c *Company, seat *org.Role) (*sandbox.AgentLLM, map[string]string, map[string]string) {
	return runLLM(c, seat, phase.Sandbox)
}

// runLLM is [sandboxLLM] over an explicit phase.
//
// TWO CALLERS, TWO PHASES, and the difference is load-bearing. A run_sandbox
// call is CODE WORK the executor delegated, so it runs on llm_sandbox — a seat
// legitimately points that at a cheaper or more code-shaped model than the one
// it thinks with. An AGENT-MODE run is the executor ITSELF, so it runs on the
// executor's own entry: sending it to llm_sandbox would run a seat's whole turn
// on the model it chose for a subordinate job, silently, on any seat that set
// both.
func runLLM(c *Company, seat *org.Role, ph phase.Phase) (*sandbox.AgentLLM, map[string]string, map[string]string) {
	if c == nil || c.Models == nil {
		return nil, nil, nil
	}
	member, err := c.Models.Head(seat, ph)
	if err != nil {
		log.Warn("sandbox_llm_unresolved", "seat", seat.Handle(),
			"phase", ph.String(), "error", err)
		return nil, nil, nil
	}
	spec, ok := c.Config.Providers.LLM[member.Key]
	if !ok {
		return nil, nil, nil
	}

	out := &sandbox.AgentLLM{
		Model:        spec.Model,
		ProviderType: string(spec.Type),
		BaseURL:      spec.BaseURL,
	}
	agent, isCLI := member.Provider.(*cliagent.Provider)
	if !isCLI {
		return out, nil, nil
	}
	// Every subscription entry shares one providers.llm type, so the type
	// does not name the family. The profile's vendor does.
	if vendor := agent.Vendor(); vendor != "" {
		out.ProviderType = vendor
	}
	// A cli-agent entry has no base URL of its own: the CLI talks to its
	// vendor, and declaring a custom provider for it would point a coding
	// agent at an endpoint nothing is serving.
	out.BaseURL = ""
	return out, agent.SandboxCredentials(), agent.SandboxEnv()
}

// SandboxCredentialError reports a coding run whose box could never
// authenticate, refused before the box is minted.
//
// A distinct type because the two remedies are different config edits and an
// operator has to be told which one is theirs — and because this is the one
// launch failure that is a CONFIGURATION mistake rather than a provider
// outage, so it must not be retried as if the vendor were down.
type SandboxCredentialError struct{ msg string }

func (e *SandboxCredentialError) Error() string { return e.msg }

// sandboxCredentials refuses a run whose coding agent has nothing to
// authenticate with inside its box.
//
// THE FAILURE THIS CLOSES IS SILENT AND EXPENSIVE. A subscription CLI that
// mints no headless token (Codex, Gemini CLI) authenticates from credential
// FILES, and those deliberately never leave the engine host: they carry a
// refresh token whose rotation is shared fleet state. So a seat on such a
// provider running in a remote cell provisions a box, installs the agent,
// applies every setup step, starts the job — and the agent fails at its first
// model call with the vendor's own "not authenticated", minutes in, naming
// nothing an operator could act on. The documented error existed in the docs
// and in no code at all.
//
// IT ASKS THE PROFILE WHICH NAMES COUNT rather than recognising a credential
// by inspection, because the engine names no tool-specific variable of its own
// and an operator legitimately declares their own key in role.sandbox.env.
// That value is in the merged run environment this is handed, so a seat that
// brought its own credential passes — which is why the check can be a refusal
// rather than a warning.
//
// THE PHASE IS THE CALLER'S, and it is the same phase the caller resolved the
// run's model with — see [runLLM]. A run_sandbox launch runs on llm_sandbox
// and an agent-mode run on the executor's own entry, and a guard that always
// asked llm_sandbox inspected the wrong provider for every agent-mode seat
// that set both: it waved through a remote run whose CLI had no token, and
// refused one whose executor entry would have minted one.
func sandboxCredentials(c *Company, seat *org.Role, ph phase.Phase, placement sandbox.Placement, env map[string]string) error {
	if placement.OnEngineHost() {
		// A local box seeds the credential files and writes a refreshed
		// one back, so files alone are a complete answer there.
		return nil
	}
	if c == nil || c.Models == nil {
		return nil
	}
	member, err := c.Models.Head(seat, ph)
	if err != nil {
		//nolint:nilerr // Deliberate: a seat with no resolvable model for
		// this phase is not this guard's question. A company with no
		// models takes no turn and so launches no run, and [runLLM],
		// which resolved the same seat and phase to pick the run's model
		// moments ago, has already logged it. Returning the error here
		// would refuse a run over a question this guard does not ask.
		return nil
	}
	agent, isCLI := member.Provider.(*cliagent.Provider)
	if !isCLI {
		// An api-key entry exports its key into the run environment on
		// every backend; there is no host-bound half to strand.
		return nil
	}
	for _, name := range agent.CredentialEnvNames() {
		if strings.TrimSpace(env[name]) != "" {
			return nil
		}
	}
	remedy := fmt.Sprintf("give this seat a local cell — role.sandbox.run_in: %q or %q",
		sandbox.Direct, sandbox.Container)
	if agent.MintsHeadlessToken() {
		remedy = fmt.Sprintf("mint a token that travels with `crewlet llm login %s "+
			"-capture-token`, or give this seat a local cell (role.sandbox.run_in: %q or %q)",
			member.Key, sandbox.Direct, sandbox.Container)
	}
	return &SandboxCredentialError{msg: fmt.Sprintf(
		"seat %q runs code in %q, where the %q subscription login cannot follow it: "+
			"the credential files stay on the engine host because they carry a refresh "+
			"token whose rotation is fleet state, and nothing in this run's environment "+
			"authenticates. %s",
		seat.Handle(), placement, member.Key, remedy)}
}

// underlay adds defaults to env WITHOUT overwriting what is already there.
//
// The direction is the decision: an operator who named a variable in
// role.sandbox.env meant that value, and the engine silently replacing it
// with a resolved subscription token would override a deliberate choice —
// including the deliberate choice to point one seat's coding runs at a
// different account.
func underlay(env, defaults map[string]string) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	for name, value := range defaults {
		if _, declared := env[name]; !declared {
			env[name] = value
		}
	}
	return env
}
