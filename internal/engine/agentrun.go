package engine

import (
	"context"
	"fmt"
	"maps"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Agent mode's engine half: turning a seat's executor into a detached run of
// the coding CLI its provider names.
//
// EVERY MOVING PART HERE ALREADY EXISTED, which is the point of the shape.
// The detachment, the durable row, the completion poll, the claim and the
// re-entry are the sandbox layer's, unchanged — an agent-mode executor is a
// coding run whose brief happens to be a prompt rather than a code task. The
// tool surface is the bridge's, unchanged. What this file adds is the wiring
// and the two refusals that wiring makes possible.

// agentLauncher runs a seat's executor as its CLI's own agentic run.
type agentLauncher struct {
	engine *Engine
	turn   *turnctx.Turn
	seat   *org.Role
	// codingAgent is the runner that drives this seat's CLI, and placement
	// is the cell it runs in — both resolved from the seat's own
	// providers.llm entry before the turn started, so a config applied
	// mid-turn cannot move a run that is already going.
	codingAgent string
	placement   sandbox.Placement
}

var _ runner.AgentLauncher = (*agentLauncher)(nil)

// LaunchExecutor starts the run and returns.
//
// THE SESSION IS OPENED BEFORE THE RUN, and closed by every path that ends
// one. A run whose box could dial a live session after the run finished would
// hold a working key to a seat's whole tool surface — the window this closes
// is the difference between a credential that expires with a job and one that
// expires on a four-hour clock.
func (l *agentLauncher) LaunchExecutor(ctx context.Context, req runner.AgentRunRequest) error {
	e := l.engine
	manager, pending := e.sandboxManager(), e.sandboxPending
	if manager == nil || pending == nil {
		return fmt.Errorf("this seat's executor is a coding CLI in agent mode, which "+
			"runs in a box: configure providers.sandbox, or set `mode: text` on "+
			"providers.llm.%s", l.codingAgent)
	}
	endpoint := e.bridge.Open(&mcpbridge.Session{
		RunID:   l.turn.ID,
		Handle:  l.turn.Handle(),
		Role:    l.seat.Name,
		Surface: req.Surface,
		Ledger:  bridgeLedger{store: pending},
	})
	if endpoint == "" {
		// NO ENDPOINT IS A REFUSAL, not a degraded run. A coding agent
		// with none of the seat's tools cannot answer anybody, cannot
		// touch a ticket and cannot submit its work — it would burn a
		// subscription producing prose nothing collects, and the turn
		// would be rescued as incomplete with no sign of why.
		return fmt.Errorf("agent mode needs a tool bridge the box can dial, and this "+
			"engine has none: set %s to a URL a sandbox can reach", mcpbridge.BaseURLVar)
	}

	company := e.Company()
	gate := seatSandbox(company, l.seat.Name)
	setup := manager.DefaultSetup()
	var servers map[string]sandbox.MCPServer
	if gate != nil {
		setup = append(setup, setupSteps(gate.Setup)...)
		servers = sandboxMCP(e.resolver(), company, l.seat, gate)
	}
	// THE SEAT'S OWN SERVERS PLUS THE BRIDGE. Both, because they answer
	// different questions: a scoped MCP server is a credential the box
	// holds, and the bridge is the seat's engine-side surface — its
	// colleagues, its channels, its memory, its submission.
	servers = withBridge(servers, endpoint)

	agentLLM, credentials, credentialEnv := l.executorLLM(company)
	env := underlay(e.sandboxEnv(l.seat, gate, setup), credentialEnv)
	spec := manager.BuildSpec(sandbox.SpecInput{
		Placement:       l.placement,
		CodingAgent:     l.codingAgent,
		PauseTTL:        pauseTTL(gate),
		MaxTurns:        maxTurnsFor(gate),
		Env:             env,
		CredentialFiles: credentials,
	})
	// THE EXECUTOR'S PHASE, matching executorLLM above: the guard has to
	// inspect the entry whose login the box will actually run under, and
	// asking it about llm_sandbox would answer for a model this run never
	// touches.
	if err := sandboxCredentials(company, l.seat, phase.Execute, spec.Placement, env); err != nil {
		e.bridge.Close(l.turn.ID)
		return err
	}

	_, err := sandbox.Launch(ctx, manager, pending, e.backends.Queue, sandbox.LaunchRequest{
		Turn:       l.runTurnRef(ctx),
		Spec:       spec,
		Setup:      setup,
		MCPServers: servers,
		LLM:        agentLLM,
		Brief:      req.Brief,
		Task:       l.turn.Task,
	})
	if err != nil {
		// The launch failed, so nothing will ever close this session on
		// the completion path.
		e.bridge.Close(l.turn.ID)
		return err
	}
	return nil
}

// executorLLM is the model an agent-mode run works under.
//
// THE EXECUTOR'S OWN ENTRY, not llm_sandbox — this run IS the executor. A
// method rather than an inline call so the choice has a name a test can hold:
// see [runLLM] for what the two phases mean, and why sending an agent-mode run
// to llm_sandbox would silently run a seat's whole turn on the model it chose
// for a subordinate job.
func (l *agentLauncher) executorLLM(c *Company) (*sandbox.AgentLLM, map[string]string, map[string]string) {
	return runLLM(c, l.seat, phase.Execute)
}

// withBridge adds the engine-side surface to a box's MCP server list.
//
// A COPY, because the seat's rendered server map is built per launch but its
// values come from config the epoch owns, and a bridge endpoint written into
// a shared map would follow the seat into its next run — where the token is
// dead and every tool call fails for a reason nothing in the config explains.
func withBridge(servers map[string]sandbox.MCPServer, endpoint string) map[string]sandbox.MCPServer {
	out := make(map[string]sandbox.MCPServer, len(servers)+1)
	maps.Copy(out, servers)
	// Under the reserved name, which config refuses to any mcp_servers
	// entry — so this write can never shadow a server the seat scoped into
	// its box. See [config.BridgeServerName] for why it is not the handle.
	out[config.BridgeServerName] = sandbox.MCPServer{
		Name: config.BridgeServerName, Transport: sandbox.TransportHTTP, URL: endpoint,
	}
	return out
}

// agentRunFor is THE construction path for a turn's executor runtime, used by
// both RunnerFor call sites.
//
// A typed nil is not a nil interface, so this returns the interface rather
// than the concrete type: assigning a nil *agentLauncher into
// runner.Config.AgentRun would make every seat's executor take the agent
// branch and immediately dereference nothing.
func (e *Engine) agentRunFor(company *Company, handle string, t *turnctx.Turn) runner.AgentLauncher {
	if t == nil {
		return nil
	}
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	launcher := company.executorRuntime(e, t, seat)
	if launcher == nil {
		return nil
	}
	return launcher
}

// executorRuntime decides how a seat's executor runs, from the providers.llm
// entry its own chain resolves to.
//
// RESOLVED ONCE, BEFORE THE TURN, and carried on the launcher: the alternative
// is reading the config again at launch, which is a different epoch by then if
// an apply landed in between — and a turn whose executor changes runtime
// halfway is one whose prompt was built for a loop that is not running.
//
// A nil launcher is the native tool loop, which is what every API entry and
// every text-mode CLI entry has.
func (c *Company) executorRuntime(e *Engine, t *turnctx.Turn, seat *org.Role) *agentLauncher {
	if c.Models == nil {
		return nil
	}
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		return nil
	}
	agent, isCLI := member.Provider.(*cliagent.Provider)
	if !isCLI || !agent.AgentMode() {
		return nil
	}
	return &agentLauncher{
		engine:      e,
		turn:        t,
		seat:        seat,
		codingAgent: agent.CodingAgent(),
		placement:   sandbox.Placement(agentPlacement(c, member.Key)),
	}
}

// agentPlacement is the cell an agent-mode entry runs in: its own `run_in`,
// then the catalogue's default.
//
// Empty is a real answer and reaches the manager as one, where it resolves to
// the company default — the same fallback a seat's code work takes, spelled
// once. See [sandbox.Manager.BuildSpec].
func agentPlacement(c *Company, key string) config.Placement {
	entry, ok := c.Config.Providers.LLM[key]
	if !ok || entry.CLI == nil {
		return ""
	}
	return entry.CLI.RunIn
}

// maxTurnsFor is a seat's coding-round cap, nil-safe on a seat with no
// sandbox block at all — which an agent-mode seat legitimately is: its
// executor runs in a box whether or not it was ever offered run_sandbox.
func maxTurnsFor(gate *config.RoleSandbox) *int {
	if gate == nil {
		return nil
	}
	return gate.MaxTurns
}

// runTurnRef is what the run's durable row records about the turn that
// launched it.
//
// THE SAME FIELDS A run_sandbox LAUNCH RECORDS, and for the same reasons: the
// conversation the work came from and who is waiting for it cannot be
// recovered any other way once the trigger is gone, and the trace comes from
// the ACTIVE span so the run's own spans nest under the phase that started
// them rather than appearing as unrelated work minutes later.
func (l *agentLauncher) runTurnRef(ctx context.Context) sandbox.TurnRef {
	agentID := ""
	if id, ok := l.engine.Company().Org.AgentIDFor(l.seat); ok {
		agentID = id.String()
	}
	runTrace := tracing.TraceOf(ctx)
	return sandbox.TurnRef{
		TurnID: l.turn.ID, AgentHandle: l.turn.Handle(), AgentID: agentID,
		Role:            l.seat.Name,
		ConversationKey: l.turn.ConversationKey,
		Reply:           l.turn.Reply,
		TraceID:         runTrace.TraceID, SpanID: runTrace.SpanID,
		Depth: l.turn.Depth, Chain: l.turn.Chain,
	}
}

// AgentModeSeat reports whether a seat's executor runs as its CLI's own
// agentic run, for the surface that must not also offer it run_sandbox.
//
// A seat whose executor IS a coding agent with a shell has no use for a second
// box beside it: two filesystems, and the one doing the work invisible to the
// other. That is what role.sandbox.run_in `self` names — see
// [config.PlacementSelf].
func (c *Company) AgentModeSeat(seat *org.Role) bool {
	if c.Models == nil || seat == nil {
		return false
	}
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		return false
	}
	agent, isCLI := member.Provider.(*cliagent.Provider)
	return isCLI && agent.AgentMode()
}
