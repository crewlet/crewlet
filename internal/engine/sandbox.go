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
	provider, err := buildSandboxProvider(spec, env)
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
		DefaultMaxTurns:    spec.DefaultMaxTurns,
		DefaultSetup:       setupSteps(spec.Setup),
		Telemetry:          otel,
	})
}

func buildSandboxProvider(spec *config.SandboxProvider, env *config.Resolver) (sandbox.Provider, error) {
	switch spec.Type {
	case config.SandboxE2B:
		// RESOLVED HERE, at the moment the provider is built, which is
		// the only place the key's value exists in this process. Tier B
		// stores its references verbatim — that is what keeps an exported
		// revision free of resolved secrets — so a backend handed
		// spec.APIKey directly would authenticate with the literal
		// "${E2B_API_KEY}" and get a 401 naming the vendor rather than
		// the misconfiguration.
		//
		// The DOMAIN is resolved on the same terms and for a reason of its
		// own: a staging cluster and a production one are the same config
		// with a different variable, and passing the reference through
		// would point every box at a host called "${E2B_DOMAIN}".
		return sandbox.NewE2B(sandbox.E2BOptions{
			APIKey:   resolvedOr(env, spec.APIKey),
			Domain:   resolvedOr(env, spec.Domain),
			Template: spec.Template,
		})
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
	default:
		// UNREACHABLE THROUGH A PARSED CONFIG, because the closed set and
		// this switch are the same list — asserted by a test, because
		// nothing else connects them and the last time they disagreed the
		// config's default named a backend with no case here.
		//
		// Answered rather than panicked: an embedder building a spec by
		// hand is a caller, not an operator to be crashed at.
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
	budgets coord.Budgets
	caps    func(agentID string) (org, seat int)
}

func (a sandboxAccountant) Charge(ctx context.Context, agentID, _ string, tokens int) (bool, error) {
	if a.budgets == nil || tokens <= 0 {
		return false, nil
	}
	orgLimit, seatLimit := a.caps(agentID)
	spend, err := a.budgets.Charge(ctx, coord.AgentScope(agentID), tokens, orgLimit, seatLimit)
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
		return fmt.Errorf("%w: %w", sandbox.ErrResumeUnavailable, err)
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
		attribute.String("crewlet.turn_id", in.Run.TurnID))
	defer span.End()

	company := e.Company()
	tel := e.describeResume(ctx, company, in)
	r, err := company.RunnerFor(in.Turn.Handle(), RunnerInput{
		Task:      resumeTask(in),
		Publisher: e.backends.Queue,
		Turn: tel.runnerTurn(company, in.Run.TurnID, in.Run.DelegationDepth,
			in.Run.DelegationChain, resumeTask(in), turn.Reply(in.Run.Reply)),
		Budget: e.meterFor(company, in.Turn.Handle()),
		// A resumed Execute loop can exhaust its rounds like any other,
		// and it is the phase most likely to: it comes back mid-task with
		// its budget already partly spent.
		Judge:     e.judgeFor(company, in.Turn.Handle()),
		Remaining: e.remainingFor(company, in.Turn.Handle()),
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
		// Who is waiting, carried on the row rather than re-derived: the
		// resumed turn never sees its trigger, so without this a turn
		// somebody asked for would come back from a coding run free to
		// end in silence.
		Reply: turn.Reply(in.Run.Reply),
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
	e.dispatch.RecordSession(ctx, in.Turn.Handle(), in.Run.ConversationKey,
		in.Run.TurnID, resumeTask(in), res, e.dispatch.now())
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

// persistSuspension writes a turn's suspended conversation to its row, which
// is also what OPENS the run to the completion poll.
//
// Called the moment the turn returns Suspended, because the runner holds the
// conversation only until its frame unwinds. Until this lands the run sits in
// [sandbox.StatusLaunching] and nothing polls or claims it — the job is
// already executing, and a job that finishes first would otherwise be
// collected against a row with nothing to resume into.
//
// EVERY WAY THIS CAN FAIL FAILS THE RUN rather than dropping the suspension: a
// row with no state is one nothing can resume, and failing here — while the
// box is still in the engine's hands and the seat's owner is still this
// process — is far better than leaving a launching row to hold a box until its
// seat happens to move.
func (e *Engine) persistSuspension(ctx context.Context, r *runner.Runner, turnID string) {
	if e.sandboxPending == nil {
		return
	}
	suspension, ok := r.Suspended()
	if !ok {
		e.failSuspension(ctx, turnID, "sandbox_suspension_missing",
			"the turn suspended but recorded no conversation", nil)
		return
	}
	blob, err := execstate.Encode(suspension.State)
	if err != nil {
		e.failSuspension(ctx, turnID, "sandbox_suspension_unserializable",
			"the suspended conversation could not be serialized", err)
		return
	}
	suspended, err := e.sandboxPending.MarkSuspended(ctx, turnID, blob)
	if err != nil {
		e.failSuspension(ctx, turnID, "sandbox_suspension_unwritable",
			"the suspended conversation could not be written", err)
		return
	}
	if !suspended {
		// The row is not launching, so this suspension has nowhere to go:
		// either the launch never recorded it, or its tail has already
		// been claimed and settled by somebody else. Overwriting either
		// one is worse than failing.
		e.failSuspension(ctx, turnID, "sandbox_suspension_not_launching",
			"the run was no longer launching when its conversation was written", nil)
	}
}

// failSuspension marks a run unresumable and says why, in the one voice all
// three failure paths share.
func (e *Engine) failSuspension(ctx context.Context, turnID, event, detail string, cause error) {
	args := []any{"turn_id", turnID,
		"detail", detail + "; the run cannot be resumed and is failed"}
	if cause != nil {
		args = append(args, "error", cause)
	}
	log.ErrorContext(ctx, event, args...)
	// Unfenced: this node is the seat's owner by construction — it just ran
	// the turn — and a fence read back from a row this write may not be able
	// to read is a second failure mode for no gain.
	if err := e.sandboxPending.SetStatus(ctx, turnID, sandbox.StatusFailed, sandbox.Fence{}); err != nil {
		log.WarnContext(ctx, "sandbox_suspension_mark_failed", "turn_id", turnID, "error", err)
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
	if existing, found, err := pending.Get(ctx, t.ID); err == nil && found {
		reuse = existing.SandboxID
	}

	setup := append(manager.DefaultSetup(), setupSteps(gate.Setup)...)
	servers := sandboxMCP(l.engine.resolver(), company, seat, gate)
	// The seat's own model and login, resolved from llm_sandbox — which
	// falls back to `llm`, because sandboxed work IS this seat's own work
	// running somewhere else, and `llm` is what that work runs on.
	agentLLM, credentials, credentialEnv := sandboxLLM(company, seat)
	env := underlay(e.sandboxEnv(seat, gate, setup), credentialEnv)
	spec := manager.BuildSpec(sandbox.SpecInput{
		CodingAgent:     string(gate.CodingAgent),
		PauseTTL:        pauseTTL(gate),
		MaxTurns:        gate.MaxTurns,
		Env:             env,
		CredentialFiles: credentials,
	})

	agentID := ""
	if id, ok := company.Org.AgentIDFor(seat); ok {
		agentID = id.String()
	}
	// THE TRACE THE RUN BELONGS TO, which nothing set before.
	//
	// TurnRef has carried TraceID and SpanID since the OTLP receiver was
	// written, and this — its only construction site — left both empty. So
	// PendingRun.TraceID was always "", RunEnv returned nil for every launch
	// (it refuses to mint a token scoped to an empty trace, which is the one
	// property that scoping has), and no coding run has ever exported
	// telemetry through an endpoint the engine goes to some length to offer.
	// The same emptiness broke the resume: describeResume built the resumed
	// turn's events from Run.TraceID, so every resumed turn was filed under
	// no trace at all.
	//
	// Taken from the ACTIVE span, which at this point is the run_sandbox
	// tool call, so the box's spans nest under the call that started them.
	runTrace := tracing.TraceOf(ctx)
	return sandbox.Launch(ctx, manager, pending, e.backends.Queue, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{
			TurnID: t.ID, AgentID: agentID, AgentHandle: t.Handle(), Role: seat.Name,
			Depth: t.Depth, Chain: t.Chain,
			TraceID: runTrace.TraceID, SpanID: runTrace.SpanID,
			// THE CONVERSATION THE WORK CAME FROM, which nothing set
			// either. The row has carried this field since it was
			// written, and with it empty a resumed turn had no way to
			// say where its answer belonged: it left no conversation
			// entry at all, so the next turn on that thread re-read
			// history that stopped at the moment the run detached and
			// planned as though the coding work had never happened.
			ConversationKey: t.ConversationKey,
			// The brief and the delivery obligation, so the resumed turn
			// has both when the trigger is long gone. Neither can be
			// recovered from the row any other way.
			Reply: t.Reply,
		},
		Brief:      brief,
		Task:       t.Task,
		Setup:      setup,
		Spec:       spec,
		LLM:        agentLLM,
		MCPServers: servers,
		ReuseBox:   reuse,
	})
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
func (e *Engine) sandboxEnv(seat *org.Role, gate *config.RoleSandbox, setup []sandbox.SetupStep) map[string]string {
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
		ClaimDuty: sandbox.DutyFunc(e.waiterDuty(interval)),
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
func (e *Engine) waiterDuty(interval time.Duration) schedule.DutyFunc {
	if e.backends == nil || e.backends.Coord == nil {
		return nil
	}
	return e.workerDuty(waiterDutyName, e.waiterDutyTTL(interval))
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
// DERIVED FROM THE INTERVAL IT GUARDS, and capped by the lease bucket's own
// age. It used to be `3 * sandbox.DefaultPollInterval` — a compile-time
// constant that ignored the configured interval entirely, so its own comment
// ("three poll intervals") was true of exactly one deployment. That constant
// was 45 s, which is also precisely the default lease TTL, and the KV refuses
// a lease STRICTLY longer than its bucket's age: the two agreed by one
// comparison. An operator lowering coordination.lease_ttl_seconds below 45
// therefore made every waiter duty claim fail — and mayTick fails closed, so
// the waiter would never tick again. Every detached run would hang forever and
// every box lose its keepalive, with one warning per tick as the only symptom.
//
// Capping rather than erroring, because a duty that is re-claimed every tick
// loses nothing by expiring sooner: the holder renews long before either
// deadline, and a shorter TTL only means a dead holder is replaced faster.
func (e *Engine) waiterDutyTTL(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = sandbox.DefaultPollInterval
	}
	ttl := max(dutyTTLTicks*interval, dutyTTLFloor)
	if e.leaseTTL > 0 && ttl > e.leaseTTL {
		return e.leaseTTL
	}
	return ttl
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
	if reg := e.startSeatServers(ctx, company, handle); reg != nil {
		company.setSeatTools(handle, reg)
	}

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
	return e.sandboxCoordinator.RecoverSeat(ctx, handle, owner, epoch)
}

// releaseSeat is the node's SeatDone hook.
//
// The control subscription is DETACHED, never deleted: a completion published
// while the seat is between owners must be held for its successor, and the box
// it refers to is still real.
func (e *Engine) releaseSeat(ctx context.Context, handle string) {
	// The seat's children die with its lease. The credentials in one ARE
	// that seat's identity, so a child left running would let this node go
	// on acting as a seat a peer has taken over — and the surface goes
	// with them, so nothing here can serve a turn through a dead client.
	company := e.Company()
	e.stopSeatServers(ctx, handle)
	company.dropSeatTools(handle)

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
// A seat with no resolvable sandbox model is not an error here: the phase
// registry already refuses a company with no models at build, and a run whose
// agent reads its credential from the environment needs none of this.
func sandboxLLM(c *Company, seat *org.Role) (*sandbox.AgentLLM, map[string]string, map[string]string) {
	if c == nil || c.Models == nil {
		return nil, nil, nil
	}
	member, err := c.Models.Head(seat, phase.Sandbox)
	if err != nil {
		log.Warn("sandbox_llm_unresolved", "seat", seat.Handle(), "error", err)
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
