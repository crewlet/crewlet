package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// The executor's engine half: turning a seat's executor into a detached run of
// the coding CLI its executor chain picks at the launch.
//
// EVERY MOVING PART HERE ALREADY EXISTED, which is the point of the shape. The
// detachment, the durable row, the completion poll, the claim and the re-entry
// are the sandbox layer's — an executor is a coding run whose brief is the
// turn's ask, with the seat's system prompt as its standing instructions. The
// tool surface is the bridge's. What this file adds is the choice of member,
// the wiring, and the refusals that wiring makes possible.

// agentLauncher runs a seat's executor as its CLI's own agentic run.
//
// Every fact it launches from is the turn's PINNED epoch ([agentLauncher.company])
// — the seat, its executor chain, the entries' cells and logins, its box
// settings, and the sandbox catalogue that turns them into a box
// ([Company.catalogue]) — never the engine's live company or the node's live
// manager: an apply landing mid-turn is the next epoch, and a launch that read
// half of one and half of the other could pick a member from one chain and
// place it by the other's entries, or by a catalogue its revision never had.
type agentLauncher struct {
	engine  *Engine
	turn    *turnctx.Turn
	seat    *org.Role
	company *Company
}

var _ runner.AgentLauncher = (*agentLauncher)(nil)

// LaunchExecutor starts the run and returns.
//
// IN THIS ORDER, and each step before anything the next one opens:
//
//  1. The TURN'S FENCE ([runner.AgentRunRequest.Fence]): a seat this node no
//     longer holds under the grant the turn was admitted under, or one a
//     person stopped, launches nothing. The runner asked the same fence when
//     its phase opened; the prompt it built since can take a model call of
//     its own, and a seat that moved or a stop placed meanwhile must not still
//     buy a box.
//  2. The node can launch at all: a sandbox runtime, the epoch's catalogue, a
//     bridge something reaches, a surface bound to the seat.
//  3. The BUDGET FLOOR ([executorHeadroom]): below it the launch is refused as
//     a budget refusal, which the dispatcher answers by parking the seat
//     rather than by a redelivery the floor would refuse again.
//  4. The MEMBER, picked here and at every launch ([agentLauncher.member]),
//     and the cell it runs in, which a box has to be able to reach the
//     bridge from.
//  5. The job, its bridge session, then the box.
//
// THE SESSION IS OPENED BEFORE THE RUN'S BOX EXISTS, and closed by every path
// that ends one. A run whose box could dial a live session after the run
// finished would hold a working key to a seat's whole tool surface — the
// window this closes is the difference between a credential that expires with
// a job and one that expires on a four-hour clock.
//
// AND UNDER THE JOB'S OWN NAME, which the store mints as the launch opens the
// job — so the session is opened from inside the launch
// ([sandbox.LaunchRequest.Opened]) rather than before it. The endpoint names
// that job, and the job is on the run's durable row: a node that takes the
// run over opens the same job's session again ([Engine.reopenAgentRun]), and
// the token the box already holds reaches it.
func (l *agentLauncher) LaunchExecutor(ctx context.Context, req runner.AgentRunRequest) error {
	e, company, handle := l.engine, l.company, l.seat.Handle()
	// THE TURN'S OWN FENCE, the closure its runner was built with, asked
	// again — never one built here. A fence captures the seat's grant when
	// it is BUILT and compares it when it is CALLED, so one built and called
	// in the same instant compares a grant with itself: it could not see a
	// seat that moved and came back under a new grant while the prompt was
	// being built, and it opened on a seat this node does not hold at all.
	// The turn's was built when the turn was admitted, which is the grant
	// this launch has to still be covered by.
	if req.Fence == nil {
		return fmt.Errorf("seat %q's executor launch carries no fence from its turn; this "+
			"is an assembly fault in the runner, not a setting", handle)
	}
	if err := req.Fence(); err != nil {
		return err
	}
	var (
		coordinator *sandbox.Coordinator
		pending     sandbox.PendingStore
	)
	if rt := e.sandbox.Load(); rt != nil {
		coordinator, pending = rt.coordinator, rt.pending
	}
	// THE EPOCH'S CATALOGUE, not the node's live one — see
	// [Company.catalogue]. Nil where the epoch configures none: a revision
	// that drops providers.sandbox leaves the runtime running, because the
	// runs already in flight still have to be polled and finished through
	// the backends that made them (see [sandbox.Coordinator.SetManager]),
	// and a launch against that runtime would mint a box on a catalogue the
	// company no longer configures. Config refuses an agent seat with an
	// executor and no providers.sandbox, so either half missing is a
	// company built by hand or a runtime that could not start: said by name
	// either way.
	manager := company.catalogue
	if coordinator == nil || manager == nil || pending == nil ||
		company.Config.Providers.Sandbox == nil {
		return fmt.Errorf("seat %q's executor runs in a box, and this node has no sandbox "+
			"runtime for this company: configure providers.sandbox", handle)
	}
	// NO BRIDGE IS A REFUSAL, not a degraded run. A coding agent with none
	// of the seat's tools cannot answer anybody, cannot touch a ticket and
	// cannot submit its work — it would burn a subscription producing prose
	// nothing collects, and the turn would be rescued as incomplete with no
	// sign of why. Refused HERE, before the launch opens a job, as every
	// case Open refuses is: the session is opened from inside the launch,
	// and a refusal there costs a row opened and abandoned.
	if e.bridge == nil {
		return fmt.Errorf("an executor needs a tool bridge its box can dial, and this "+
			"node has none: set %s to the fleet's address a box can reach (any node "+
			"serving the API, or a load balancer in front of them); a direct box on a "+
			"node that serves the API reaches it on loopback without it",
			mcpbridge.BaseURLVar)
	}
	// A BRIDGE NOTHING REACHES IS REFUSED BY NAME, because the settings it
	// takes are different fixes: an unset base URL is one variable, and a
	// node that serves the API with no listener (api.port 0) is a second one
	// the first message would send an operator past.
	if !e.bridge.Reachable() {
		return fmt.Errorf("an executor needs a node serving its tool bridge, and "+
			"nothing here reaches it: this node serves the API with api.port 0, so "+
			"it binds no listener for the bridge's route; set api.port, and point %s "+
			"at the nodes that serve the API as a sandbox reaches them",
			mcpbridge.BaseURLVar)
	}
	// A SURFACE BOUND TO NO SEAT IS REFUSED BY NAME, before the launch, for
	// the reason the two refusals above are: Open refuses it too — every
	// bridged call acts as the surface's seat, and the token names it, so a
	// surface with none could decide nothing — and its empty answer would
	// otherwise be reported as a missing bridge URL, sending an operator to
	// a variable that is set.
	if _, err := mcpbridge.SeatOf(req.Surface); err != nil {
		return fmt.Errorf("the executor's tool surface is bound to no seat, so a " +
			"bridged call would have nobody to act as; this is an assembly fault " +
			"in the runner, not a setting")
	}
	if err := executorHeadroom(ctx, e.remainingFor(company, handle),
		company.Config.TurnEngine.ExecutorFloor()); err != nil {
		return err
	}
	member, agent, err := l.member(ctx, manager, req.OnFallback)
	if err != nil {
		return err
	}

	// THE CELL THE RUN GOES TO, resolved as the manager resolves it — the
	// member's own run_in, else the catalogue's default — because it decides
	// which address its box dials the bridge at ([mcpbridge.Session.OnHost]).
	cell := sandbox.Placement(agentPlacement(company, member.Key))
	if cell == "" {
		cell = manager.DefaultPlacement()
	}
	// A CELL THAT CANNOT DIAL THE BRIDGE IS REFUSED BY NAME, before the launch
	// opens a job, for the reason the refusals above are. A direct box is a
	// process of this host and dials this node's own listener on loopback,
	// where the node serves the bridge's route; a box in any other cell is
	// not on this host's loopback, and reaches the fleet only at the
	// address the variable gives.
	if e.bridge.Reaches(cell == sandbox.Direct) == "" {
		return fmt.Errorf("an executor needs a tool bridge its box can dial, and a box in "+
			"the %s cell cannot reach this node on its loopback: set %s to the fleet's "+
			"address that box can reach", cell, mcpbridge.BaseURLVar)
	}

	// The run's calls, and with each what they have cost the engine so far,
	// for the segment that resumes from the run to pay — bound to the run's
	// job by the launch below, before the box that makes them exists.
	ledger := newBridgeLedger(pending, req.Spend)
	ledger.nudge = e.nudgeRun
	session := &mcpbridge.Session{
		RunID:   l.turn.RunID,
		Handle:  handle,
		Role:    l.seat.Name,
		Surface: req.Surface,
		Ledger:  ledger,
		// WHICH ADDRESS THE BOX DIALS: this node's own listener for a box
		// on its host, the fleet's address for any other. NEVER
		// [mcpbridge.Session.Loopback], which a direct box could otherwise
		// be held to: a seat's session follows the seat, and once a peer
		// takes it over the box's calls reach their holder only relayed
		// from this node — which a loopback-only session refuses.
		OnHost: cell == sandbox.Direct,
		// THE SEAT'S FENCE, built HERE, inside the turn the run is the
		// executor of, so it closes on the grant that turn was admitted
		// under — the same check the turn's own phases run, now asked
		// before and after every call the box makes. The run outlives the
		// turn: the seat can move off this node, or a person stop it,
		// while it runs, and a call after either is refused rather than
		// made as a seat this node no longer speaks for. A plain pause
		// closes nothing: it holds the seat's mailbox, not its running
		// work.
		Fence: e.seatFence(handle),
	}

	settings := seatSandbox(company, handle)
	// A RUN ENVIRONMENT THAT DOES NOT RESOLVE IS SAID AND NOT REFUSED. This
	// run is the turn's executor itself: refused, the phase fails and the
	// turn with it (internal/agent/turn), so every message the seat could
	// answer through its bridge would fail for a code-host token a pass may
	// mint in a minute — the reason a seat's mailbox does not wait for a
	// tool server held back for its credential (toolcredentials.go). The
	// run starts without what is missing, and the line says why it is.
	runEnv := e.runEnvironment(ctx, company, l.seat, settings, manager.DefaultSetup())
	runEnv.logGap(ctx, handle, "the run starts without them")
	runEnv.logFacts(ctx, handle)
	// THE SEAT'S OWN SERVERS, and the bridge beside them once the job has a
	// name (below). Both, because they answer different questions: a scoped
	// MCP server is a credential the box holds, and the bridge is the seat's
	// engine-side surface — its colleagues, its channels, its memory, its
	// submission.
	var servers map[string]sandbox.MCPServer
	if settings != nil {
		servers = e.boxServers(ctx, company, l.seat, settings)
	}

	run := newCodingRun(agent, runEnv.env)
	spec := manager.BuildSpec(sandbox.SpecInput{
		Placement:       cell,
		CodingAgent:     agent.Agent(),
		PauseTTL:        pauseTTL(settings),
		MaxTurns:        maxTurnsFor(settings),
		Env:             run.env,
		CredentialFiles: run.files,
	})
	// THE PICKED MEMBER'S LOGIN, the one the box will actually run under:
	// a fallback launch on a later member is judged on that member's entry,
	// never the chain's head.
	if err = sandboxCredentials(agent, member.Key, handle, spec.Placement, run.env); err != nil {
		return err
	}

	var opened bool
	_, err = coordinator.Launch(ctx, manager, sandbox.LaunchRequest{
		Turn:        l.runTurnRef(ctx),
		Spec:        spec,
		Setup:       runEnv.setup,
		MCPServers:  servers,
		Model:       run.model,
		ProviderKey: member.Key,
		System:      req.System,
		Brief:       req.Brief,
		// HOW TO WAIT ON A PERSON, with the names the chart gives this
		// seat, from the pinned epoch — see [askBrief]. Without it the box
		// carries the ask shim and the collection reads what it writes,
		// but nothing tells the run either exists.
		Ask:   askBrief(company.Org, l.seat),
		Task:  l.turn.Task,
		Label: l.turn.Task,
		// A RUN CARRIED ON WITH A PERSON'S ANSWER, continued in its own
		// session where the launch runs it in the asking run's box — see
		// [runner.Asked].
		Continue: sandboxContinuation(req.Continue),
		Opened: func(job sandbox.OpenedJob) (map[string]sandbox.MCPServer, error) {
			bridge, openErr := e.openBridge(ctx, session, ledger, job)
			opened = openErr == nil
			return bridge, openErr
		},
		// THE SAME FENCE, asked again once the run's row exists — see
		// [sandbox.LaunchRequest.Fence]: a stop that lands while this
		// launch is being built either finds the row or is refused here.
		Fence: req.Fence,
	})
	if err != nil {
		if opened {
			// The launch failed after the job's session opened, so
			// nothing will ever close it on the completion path.
			e.bridge.Close(l.turn.RunID)
		}
		// A LAUNCH A PERSON'S STOP ENDED UNDER IT — its row's ending decided
		// between the box and the job, so the attach was refused — is the
		// stop, and the turn reports it as one rather than as a launch that
		// broke, naming who stopped it: as the coordinator kept the stop's
		// decision for this frame, never as the seat's pause reads by now,
		// which a person may have lifted since ([sandbox.Coordinator.LaunchStopped]).
		// Taken whatever the launch answered, so a stop that came with its own
		// fence's refusal is not kept for nobody.
		decided, stopped := coordinator.LaunchStopped(l.turn.RunID)
		switch {
		case turn.Stopped(err):
			return err
		case stopped:
			log.InfoContext(ctx, "executor_launch_stopped", "seat", handle, "turn_id", l.turn.RunID,
				"error", err, "detail", "a person stopped the seat's running work while its "+
					"executor was launching; the launch is ended and the turn stops")
			return &stopError{pause: pauseOfStop(decided), handle: handle}
		}
		return err
	}
	return nil
}

// openBridge opens a job's bridge session under the job's own name, with its
// ledger bound to the same job, and answers the box's server entry for it:
// [sandbox.LaunchRequest.Opened], for the launch, and the same opening for a
// run taken over while it works ([Engine.reopenAgentRun]), whose box already
// holds the endpoint and is never handed the one minted here.
//
// THE SESSION NUMBERS ITS CALLS AFTER THE ONES THE ROW HOLDS
// ([sandbox.OpenedJob.LastSeq]): a recovery's after its own job's, and a job
// continuing one that asked a person after the asking job's, so the round's
// calls keep the one order its collection reads them in.
func (e *Engine) openBridge(ctx context.Context, s *mcpbridge.Session, ledger *bridgeLedger,
	job sandbox.OpenedJob,
) (map[string]sandbox.MCPServer, error) {
	ledger.bind(job.LaunchID)
	s.Launch, s.LastSeq = job.LaunchID, job.LastSeq
	endpoint := e.bridge.Open(ctx, s)
	if endpoint == "" {
		// UNREACHABLE past the refusals LaunchExecutor makes by name, which
		// are every case Open refuses — said rather than assumed, because a
		// box launched without its endpoint has none of the seat's tools.
		return nil, fmt.Errorf("the tool bridge opened no session for run %s's job %s; "+
			"this node's log says why (mcp_bridge_*)", s.RunID, job.LaunchID)
	}
	return bridgeServer(endpoint), nil
}

// sandboxContinuation is the runner's carry-on of an asking run in the sandbox
// layer's terms, nil for every other launch.
func sandboxContinuation(c *runner.Continuation) *sandbox.Continuation {
	if c == nil {
		return nil
	}
	return &sandbox.Continuation{SessionID: c.SessionID, Brief: c.Brief}
}

// member is the executor-chain member this launch runs on: the first one that
// can take a run here and now.
//
// PICKED AT EVERY LAUNCH, not once for the turn: a turn the reviewer sends back
// launches again, possibly an hour later, and an entry whose plan ran out in
// between must be passed over then rather than relaunched into the same limit.
// From the turn's pinned chain ([phase.Registry.Chain], the same resolution
// config validates the chain by), walked in order:
//
//   - a member that is not a coding CLI with an in-box runner is passed over
//     SILENTLY. It is a fact of the configuration rather than a refusal of the
//     moment — the seat's `llm` is shared with the reviewer and its other
//     phases, where any CLI is legitimate — and config warns about it once,
//     where a hand-off announced on every launch would report a fallback
//     nothing fell from;
//   - a member sitting out its spent plan on this node
//     ([cliagent.Provider.SittingOut]) is handed off like a call's fallback
//     ([chain.Pick]), published against this turn through onFallback;
//   - every runner member sitting out is a [*chain.Error], the same
//     unavailability an exhausted call reports, so the seat reads as stopped
//     on its provider exactly as it does when a reviewer's run finds every
//     member spent.
//
// A run that meets its limit midway is not relaunched on the next member: its
// box did the work it did, and the next launch is where the chain moves on.
func (l *agentLauncher) member(ctx context.Context, manager *sandbox.Manager,
	onFallback func(chain.Fallback)) (chain.Member, *cliagent.Provider, error) {
	runners, err := executorRunners(l.company, l.seat, manager)
	if err != nil {
		return chain.Member{}, nil, err
	}
	picked, err := chain.Pick(ctx, runners, func(m chain.Member) error {
		return m.Provider.(*cliagent.Provider).SittingOut()
	}, onFallback)
	if err != nil {
		return chain.Member{}, nil, err
	}
	return picked, picked.Provider.(*cliagent.Provider), nil
}

// executorRunners is seat's executor chain as a launch can take it: every
// member, in chain order, that is a coding CLI with an in-box runner in
// manager — the members [agentLauncher.member] picks among, and so every
// member whose cell a run of the seat can land in.
//
// Refused when there is none. Config refuses this chain on every write ("needs
// a claude-code or opencode member"), so reaching it is a company built by hand
// or a catalogue built without the runners: either way the operator's to fix,
// and a retry changes nothing.
func executorRunners(c *Company, seat *org.Role, manager *sandbox.Manager) ([]chain.Member, error) {
	if c.Models == nil {
		return nil, fmt.Errorf("seat %q cannot launch its executor: %w",
			seat.Handle(), phase.ErrNoProviders)
	}
	members, err := c.Models.Chain(seat, phase.Execute)
	if err != nil {
		return nil, fmt.Errorf("seat %q's executor chain: %w", seat.Handle(), err)
	}
	runners := make([]chain.Member, 0, len(members))
	for _, m := range members {
		agent, isCLI := m.Provider.(*cliagent.Provider)
		if !isCLI {
			continue
		}
		if _, err := manager.RunnerFor(agent.Agent()); err != nil {
			continue
		}
		runners = append(runners, m)
	}
	if len(runners) == 0 {
		keys := make([]string, len(members))
		for i, m := range members {
			keys[i] = m.Key
		}
		return nil, fmt.Errorf("seat %q's executor chain [%s] has no member whose CLI "+
			"runs in a box (%s): point the seat's llm at an entry on one of them",
			seat.Handle(), strings.Join(keys, ", "), strings.Join(codingagent.Names(), ", "))
	}
	return runners, nil
}

// executorHeadroom refuses a launch below turn_engine.executor_min_budget_tokens.
//
// EVERY LAUNCH, the dispatch's and each relaunch a reviewer sends the turn back
// for: a run's first request alone is its CLI's harness prompt and tool
// definitions, the bridged tool array and the seat's system prompt, so a seat
// with less room than that spends a box, a clone and a setup to be refused at
// its first call.
//
// THREE-VALUED, like every other budget read in this engine: a seat with no
// counter is uncapped and passes, a seat below the floor is refused, and a
// read that FAILED is refused too — launching a box on an unknown budget is how
// a company discovers its ceiling by spending past it.
//
// BELOW THE FLOOR IS A BUDGET REFUSAL ([toolloop.ErrBudgetExhausted]), which is
// what makes it wait rather than fail, on both paths a launch is made from. On
// a dispatch the dispatcher answers that error by asking the budget stage,
// which recognises a seat whose windows leave less than the floor
// ([meter.starved]) — so the seat waits for its window to turn over rather
// than taking its redelivery into the same refusal until the broker
// dead-letters it. In a resumed segment — a reviewer's send-back, or the run
// a person's answer carries on — there is no delivery to park, and the resume
// is HELD on the same window instead ([Engine.budgetHold],
// [sandbox.ErrResumeHeld]): the job stays collected, its seat held, until the
// window turns over. An unreadable counter is not one: it is the store's
// moment, and a retry's backoff is what rides it out.
func executorHeadroom(ctx context.Context, remaining runner.Remaining, floor int) error {
	if floor <= 0 || remaining == nil {
		// No floor, or no counter anywhere in the epoch — a company with
		// no token budget rather than a read that could not be made.
		return nil
	}
	left, err := remaining.Remaining(ctx)
	if err != nil {
		return fmt.Errorf("this seat's remaining token budget could not be read, and an "+
			"executor run costs a box before it produces anything: %w", err)
	}
	if left < floor {
		return fmt.Errorf("this seat has %d tokens left and an executor run needs at least "+
			"%d (turn_engine.executor_min_budget_tokens): %w",
			left, floor, toolloop.ErrBudgetExhausted)
	}
	return nil
}

// bridgeServer is the engine-side surface as one entry of a box's MCP server
// list, which the launch adds beside the seat's own servers in a map of the
// job's own: an endpoint written into the seat's rendered map, whose values
// the epoch owns, would follow the seat into its next run, where the token
// names a job that is over.
func bridgeServer(endpoint string) map[string]sandbox.MCPServer {
	// Under the reserved name, which config refuses to any mcp_servers
	// entry — so this entry can never shadow a server the seat scoped into
	// its box. See [config.ToolServerName] for why it is not the handle.
	return map[string]sandbox.MCPServer{config.ToolServerName: {
		Name: config.ToolServerName, Transport: sandbox.TransportHTTP, URL: endpoint,
	}}
}

// agentRunFor is THE construction path for a turn's executor launcher, used by
// both RunnerFor call sites, over the turn's pinned company.
//
// Nil only where there is nothing to launch for — no turn, no agent seat, or a
// company with no models, which the no-model hold keeps from taking a turn at
// all — and the runner refuses to be built without one: every executor is a
// launched run, so there is no other runtime a nil could stand for.
//
// A typed nil is not a nil interface, so this returns the interface rather
// than the concrete type: assigning a nil *agentLauncher into
// runner.Config.Launcher would hand the runner a launcher that dereferences
// nothing on its first launch.
func (e *Engine) agentRunFor(company *Company, handle string, t *turnctx.Turn) runner.AgentLauncher {
	if t == nil || company == nil || company.Models == nil {
		return nil
	}
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	return &agentLauncher{engine: e, turn: t, seat: seat, company: company}
}

// agentPlacement is the cell an executor entry runs in: its own `run_in`, then
// the catalogue's default.
//
// Empty is a real answer and reaches the manager as one, where it resolves to
// the company default — spelled once. See [sandbox.Manager.BuildSpec].
func agentPlacement(c *Company, key string) config.Placement {
	entry, ok := c.Config.Providers.LLM[key]
	if !ok {
		return ""
	}
	return entry.RunIn
}

// maxTurnsFor is a seat's coding-round cap, nil-safe on a seat with no sandbox
// block at all — which an agent seat legitimately is: its executor runs in a
// box whether or not the seat says anything about that box.
func maxTurnsFor(settings *org.RoleSandbox) *int {
	if settings == nil {
		return nil
	}
	return settings.MaxTurns
}

// runTurnRef is what the run's durable row records about the turn that
// launched it. See [sandboxTurnRef].
func (l *agentLauncher) runTurnRef(ctx context.Context) sandbox.TurnRef {
	return sandboxTurnRef(ctx, l.turn, l.seat.Name)
}
