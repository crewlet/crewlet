package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
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
func buildSandbox(c *config.Company) (*sandbox.Manager, error) {
	spec := c.Providers.Sandbox
	if spec == nil || !spec.Enabled() {
		return nil, nil
	}
	provider, err := buildSandboxProvider(spec)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, nil
	}
	return sandbox.NewManager(sandbox.ManagerOptions{
		Provider: provider,
		Runners: map[string]sandbox.Runner{
			codingagent.ClaudeCodeName: codingagent.NewClaudeCode(),
			codingagent.OpenCodeName:   codingagent.NewOpenCode(),
		},
		DefaultCodingAgent: string(spec.DefaultCodingAgent),
		DefaultTemplate:    spec.Template,
		DefaultTimeout:     seconds(spec.Timeout()),
		DefaultPauseTTL:    seconds(spec.PauseTTL()),
		DefaultSetup:       setupSteps(spec.Setup),
	})
}

func buildSandboxProvider(spec *config.SandboxProvider) (sandbox.Provider, error) {
	switch spec.Type {
	case config.SandboxLocal:
		local := spec.Local
		if local == nil {
			local = &config.LocalSandbox{}
		}
		return sandbox.NewLocal(sandbox.LocalOptions{
			Containment: sandbox.Containment(local.Containment),
			StateDir:    local.StateDir,
			Image:       local.Image,
			Runtime:     string(local.Runtime),
			Network:     local.Network,
			RunArgs:     local.RunArgs,
		})
	case config.SandboxFake:
		// The in-process double, for a deployment demonstrating the flow
		// without a real box. Named in config rather than inferred, so
		// nobody runs one by accident.
		return sandbox.NewFakeProvider(), nil
	case config.SandboxE2B:
		// The seam ships in v1; the backend lands when a deployment needs
		// it. Refused rather than silently downgraded to local, which would
		// run the operator's code on the engine host without saying so.
		return nil, fmt.Errorf("providers.sandbox.type %q is not built into this engine yet; "+
			"use %q (containment %q or %q)", config.SandboxE2B,
			config.SandboxLocal, config.ContainmentDirect, config.ContainmentContainer)
	default:
		return nil, fmt.Errorf("providers.sandbox.type %q is not one of %v",
			spec.Type, config.SandboxTypes)
	}
}

// setupSteps maps the config shape onto the sandbox package's own.
//
// A translation rather than a shared type, so the sandbox package does not
// import the config package: a setup step is a runtime instruction, and
// keeping the two apart is what lets the sandbox layer be tested with a step
// built in a test rather than a YAML document parsed into one.
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

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// sandboxAccountant charges a collected coding run against the shared counter.
//
// The charge happens AFTER the spend, which is why it cannot refuse: a
// refusal cannot un-spend a run that already ran, and recording it anyway is
// the only way the meter stays true when the cap is binding.
type sandboxAccountant struct {
	budgets *store.Budgets
	caps    func(agentID string) (org, seat int)
}

func (a sandboxAccountant) Charge(ctx context.Context, agentID, _ string, tokens int) (bool, error) {
	if a.budgets == nil || tokens <= 0 {
		return false, nil
	}
	orgLimit, seatLimit := a.caps(agentID)
	spend, err := a.budgets.Charge(ctx, agentID, tokens, orgLimit, seatLimit)
	if err != nil {
		return false, err
	}
	return !spend.OK, nil
}

// resumer re-enters a suspended turn on this node.
//
// It is the coordinator's one seam into the agent layer, and the direction
// matters: the sandbox package holds the state as an OPAQUE blob and this
// decodes it, so the wire format's shape stays in the agent layer that
// understands it.
type resumer struct{ engine *Engine }

var _ sandbox.Resumer = (*resumer)(nil)

func (r *resumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	state, ok, err := execstate.Decode(req.Run.ExecuteState)
	if err != nil {
		// A state this build cannot read is a ROUTING failure, not a run
		// failure: a peer on the version that wrote it can resume this, so
		// the completion must go back rather than be settled here.
		return fmt.Errorf("%w: %s", sandbox.ErrResumeUnavailable, err)
	}
	if !ok {
		return fmt.Errorf("%w: run %s has no suspended conversation",
			sandbox.ErrResumeUnavailable, req.Run.TurnID)
	}
	seat := r.engine.Company().Org.AgentSeatByHandle(req.Run.AgentHandle)
	if seat == nil {
		// The seat is gone from this epoch — decommissioned, or this node
		// is on a revision that never had it. Either way the resume belongs
		// somewhere else.
		return fmt.Errorf("%w: seat %q is not in this node's company",
			sandbox.ErrResumeUnavailable, req.Run.AgentHandle)
	}
	return r.engine.resumeTurn(ctx, resumeInput{
		Run:   req.Run,
		State: state,
		Turn: &turnctx.Turn{
			ID: req.Run.TurnID, Seat: seat, Org: r.engine.Company().Org,
			Depth: req.Run.DelegationDepth, Chain: req.Run.DelegationChain,
		},
		Answer:  req.Answer,
		Success: req.Success,
		Trigger: req.Trigger,
	})
}

// resumeInput is one re-entry, assembled.
type resumeInput struct {
	Run     sandbox.PendingRun
	State   execstate.State
	Turn    *turnctx.Turn
	Answer  string
	Success bool
	Trigger *events.Event
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
	company := e.Company()
	tel := e.describeResume(company, in)
	r, err := company.RunnerFor(in.Turn.Handle(), RunnerInput{
		Task:      resumeTask(in),
		Publisher: e.backends.Queue,
		Turn:      tel.runnerTurn(company, in.Run.TurnID, in.Run.DelegationDepth),
		Budget:    e.meterFor(company, in.Turn.Handle()),
		// NO onboarding on a resume. The pass is a seat's FIRST turn, and
		// this turn already ran its first phase — running it here would
		// spend the resumed turn's opening on orientation for a seat that
		// is mid-task.
		Resume: &runner.Resume{State: in.State, Answer: in.Answer},
	})
	if err != nil {
		return err
	}
	res, err := turn.Run(ctx, r, company.TurnSettings(), turn.Input{
		TurnID:  in.Run.TurnID,
		Depth:   in.Run.DelegationDepth,
		History: in.State.Iterations,
		Resume:  true,
		// The plan the suspended turn was executing, so the delivery gate
		// judges the round against what it INTENDED. A plan that cannot be
		// recovered downgrades the gate rather than failing the resume.
		ResumePlan: resumePlan(in.Run),
	})
	e.publishTurnCompleted(ctx, tel, in.Run.TurnID, r.Spend(), res, err)
	if err != nil {
		return err
	}
	// A resumed turn that suspended AGAIN persists its new conversation the
	// same way the first one did — the coordinator sees the row back in
	// running and leaves the box for the next completion.
	if res.Suspended {
		e.persistSuspension(ctx, r, in.Run.TurnID)
	}
	return nil
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

// resumePlan rebuilds the plan the suspended turn was executing.
func resumePlan(run sandbox.PendingRun) turn.Plan {
	p := turn.Plan{Summary: run.TaskDescription}
	if len(run.Plan) > 0 {
		if summary, ok := run.Plan["summary"].(string); ok && summary != "" {
			p.Summary = summary
		}
		if tools, ok := run.Plan["tools_needed"].([]any); ok {
			for _, t := range tools {
				if name, ok := t.(string); ok {
					p.ToolsNeeded = append(p.ToolsNeeded, name)
				}
			}
		}
	}
	return p
}

// persistSuspension writes a turn's suspended conversation to its row.
//
// Called the moment the turn returns Suspended, because the runner holds the
// conversation only until its frame unwinds. A suspension that cannot be
// serialized FAILS THE RUN rather than being dropped: a row with no state is
// one nothing can resume, and failing here — while the box is still in the
// engine's hands — is far better than discovering it at a resume days later.
func (e *Engine) persistSuspension(ctx context.Context, r *runner.Runner, turnID string) {
	if e.sandboxPending == nil {
		return
	}
	suspension, ok := r.Suspended()
	if !ok {
		log.ErrorContext(ctx, "sandbox_suspension_missing", "turn_id", turnID,
			"detail", "the turn suspended but recorded no conversation; the run "+
				"cannot be resumed and will be failed")
		if err := e.sandboxPending.SetStatus(ctx, turnID, sandbox.StatusFailed, sandbox.Fence{}); err != nil {
			log.WarnContext(ctx, "sandbox_suspension_mark_failed", "turn_id", turnID, "error", err)
		}
		return
	}
	blob, err := execstate.Encode(suspension.State)
	if err != nil {
		log.ErrorContext(ctx, "sandbox_suspension_unserializable",
			"turn_id", turnID, "error", err)
		if err := e.sandboxPending.SetStatus(ctx, turnID, sandbox.StatusFailed, sandbox.Fence{}); err != nil {
			log.WarnContext(ctx, "sandbox_suspension_mark_failed", "turn_id", turnID, "error", err)
		}
		return
	}
	if err := e.sandboxPending.SaveExecuteState(ctx, turnID, blob); err != nil {
		log.ErrorContext(ctx, "sandbox_suspension_unwritable", "turn_id", turnID, "error", err)
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
	gate := seatSandbox(company, seat.Name)
	if gate == nil || !gate.Enabled {
		// Belt and braces: the tool is only on a sandbox-enabled seat's
		// surface, but the surface is built from a snapshot and a seat can
		// lose its gate across an apply mid-turn.
		return sandbox.LaunchResult{}, fmt.Errorf("this seat's sandbox is not enabled")
	}

	// A box this turn already has, paused from an earlier call. An EMPTY id
	// on an existing row means that box is gone — reaped past its pause TTL,
	// or torn down under a zero TTL — so this call provisions a fresh one
	// and the work re-seeds from the pushed branch.
	reuse := ""
	if existing, found, err := pending.Get(ctx, t.ID); err == nil && found {
		reuse = existing.SandboxID
	}

	setup := append(manager.DefaultSetup(), setupSteps(gate.Setup)...)
	servers := sandboxMCP(l.engine.resolver(), company, seat, gate)
	spec := manager.BuildSpec(sandbox.SpecInput{
		CodingAgent: string(gate.CodingAgent),
		PauseTTL:    pauseTTL(gate),
		Env:         e.sandboxEnv(company, seat, gate, setup),
	})

	agentID := ""
	if id, ok := company.Org.AgentIDFor(seat); ok {
		agentID = id.String()
	}
	return sandbox.Launch(ctx, manager, pending, e.backends.Queue, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{
			TurnID: t.ID, AgentID: agentID, AgentHandle: t.Handle(), Role: seat.Name,
			Depth: t.Depth, Chain: t.Chain,
		},
		Brief:      brief,
		Setup:      setup,
		Spec:       spec,
		MCPServers: servers,
		ReuseBox:   reuse,
	})
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
func sandboxMCP(env *config.Resolver, c *Company, seat *org.Role, gate *config.RoleSandbox) map[string]map[string]any {
	if len(gate.MCP.Servers) == 0 {
		return nil
	}
	servers := make([]sandbox.MCPServer, 0, len(c.Config.MCPServers))
	for _, s := range c.Config.MCPServers {
		servers = append(servers, sandbox.MCPServer{
			Name: s.Name, Transport: string(s.Transport),
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
func pauseTTL(gate *config.RoleSandbox) *time.Duration {
	if gate.PauseTTLSeconds == nil || *gate.PauseTTLSeconds < 0 {
		return nil
	}
	d := seconds(*gate.PauseTTLSeconds)
	return &d
}

// seatSandbox is a seat's sandbox gate, or nil.
func seatSandbox(c *Company, roleName string) *config.RoleSandbox {
	if c == nil || c.Config == nil {
		return nil
	}
	for i := range c.Config.Roles {
		if c.Config.Roles[i].Name == roleName {
			return c.Config.Roles[i].Sandbox
		}
	}
	return nil
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
func (e *Engine) sandboxEnv(c *Company, seat *org.Role, gate *config.RoleSandbox, setup []sandbox.SetupStep) map[string]string {
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
	for key, value := range gate.Env {
		env[key] = value
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
	manager, err := buildSandbox(company.Config)
	if err != nil {
		return err
	}
	if manager == nil {
		return nil
	}
	if e.backends == nil || e.backends.Store == nil {
		// A detached run's row is what survives the turn that starts it, so
		// a node with no store cannot offer code work at all. Refused
		// loudly rather than degraded: a sandbox whose runs vanish on
		// restart is worse than no sandbox, because the box keeps running
		// and billing with nobody to collect it.
		return fmt.Errorf("providers.sandbox needs a database: a detached run's " +
			"state is a row, and without one a restart orphans every box")
	}
	e.sandboxPending = sandbox.NewSQLStore(e.backends.Store)

	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: e.backends.Queue, Pending: e.sandboxPending, Manager: manager,
		Resume:  &resumer{engine: e},
		Account: e.sandboxAccountant(),
	})
	if err != nil {
		return err
	}
	e.sandboxCoordinator = coordinator
	log.Info("sandbox_enabled", "provider", manager.Provider().Kind(),
		"coding_agent", manager.DefaultCodingAgent())
	return nil
}

// startSandboxWaiter starts the completion poll, once the node exists.
func (e *Engine) startSandboxWaiter(ctx context.Context, interval time.Duration) error {
	if e.sandboxCoordinator == nil {
		return nil
	}
	waiter, err := sandbox.NewWaiter(sandbox.WaiterOptions{
		Queue: e.backends.Queue, Pending: e.sandboxPending,
		Manager:  e.sandboxCoordinator.Manager(),
		Interval: interval,
		// The duty is claimed per tick: the waiter polls EVERY active run
		// in the company, not just this node's seats, so N nodes running it
		// unclaimed means N reconnects per box per tick and N racing
		// reapers.
		ClaimDuty: sandbox.DutyFunc(e.waiterDuty()),
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
	if e.backends == nil || e.backends.Store == nil {
		return nil
	}
	return sandboxAccountant{
		budgets: e.backends.Store.Budgets(),
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
func (e *Engine) waiterDuty() schedule.DutyFunc {
	if e.backends == nil || e.backends.Coord == nil {
		return nil
	}
	return e.workerDuty(waiterDutyName, waiterDutyTTL)
}

// waiterDutyTTL is how long the waiter duty survives without a re-claim.
//
// Three poll intervals, the same "do not flap on a blip" rule the scheduler
// duty and the seat heartbeat follow: the holder re-claims every tick, so this
// rides out two consecutive slow or failed claims without the duty moving. At
// the default 15 s poll that is 45 s — under a minute of no polling if the
// holder dies, which costs at most one late completion, and the next tick's
// keepalive is well inside the box TTL.
const waiterDutyTTL = 3 * sandbox.DefaultPollInterval

// prepareSeat is the node's SeatReady hook: recover this seat's in-flight runs
// and start listening for their completions, BEFORE its mailbox opens.
func (e *Engine) prepareSeat(ctx context.Context, handle string, epoch int64, owner string) error {
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
	return e.sandboxCoordinator.RecoverSeat(ctx, handle, owner, epoch)
}

// releaseSeat is the node's SeatDone hook.
//
// The control subscription is DETACHED, never deleted: a completion published
// while the seat is between owners must be held for its successor, and the box
// it refers to is still real.
func (e *Engine) releaseSeat(ctx context.Context, handle string) {
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

// AwaitingSandbox reports whether a seat is parked on a detached coding run.
//
// Exported for the operator surfaces and for a test: the inbox screening reads
// it internally through the dispatcher's conditions, but "is this seat busy on
// code work" is also a question a dashboard asks, and answering it from a
// second place would eventually answer it differently.
func (e *Engine) AwaitingSandbox(handle string) bool {
	if e.sandboxCoordinator == nil {
		return false
	}
	return e.sandboxCoordinator.AwaitingSandbox(handle)
}
