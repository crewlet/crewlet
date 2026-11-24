package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	seatpkg "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/words"
)

// The executor's engine half: turning a seat's executor into a detached run of
// the harness its executor chain picks at the launch.
//
// EVERY MOVING PART HERE ALREADY EXISTED, which is the point of the shape. The
// detachment, the durable row, the completion poll, the claim and the re-entry
// are the sandbox layer's — an executor is its harness's own run, whose
// brief is the turn's ask, with the seat's system prompt as its standing
// instructions. The tool surface is the bridge's. What this file adds is the
// choice of member, the wiring, and the refusals that wiring makes possible.

// agentLauncher runs a seat's executor as its harness's own agentic run.
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
	// ask is the colleague's question the turn answers, nil for none,
	// which the run's record carries to the frame that ends the turn
	// ([sandbox.ColleagueAsk]).
	ask *sandbox.ColleagueAsk

	// conversation is what the turn's dispatch read of its conversation
	// ([Engine.readConversation]): the base its first launch continues and
	// freezes, answered with no further read. Set on a dispatched segment's
	// launcher, whose one launch is the turn's first; nil on a resumed
	// segment's, whose relaunches read the base the first launch froze.
	conversation *conversationRead

	// told is what the turn's dispatch told the seat about its access, which
	// its first launch records; nil on a resumed turn, whose first launch
	// recorded it.
	told *toldTurn

	// notes is the resumed segment's notes on its run's record, nil on a
	// dispatched segment's launcher, whose brief's notes are its box's: a
	// note a relaunch's brief was handed by the settle of a take whose answer
	// was lost is sealed naming that take, whose read the launch's write takes
	// over ([rowSource.lostTakes]).
	notes *rowSource

	// relaunchRefusedIn is, on a resumed segment's launcher, the window the
	// last relaunch a resume of its run's job tried was refused below its
	// floor in, off that job's record
	// ([sandbox.LaunchRecord.RelaunchRefusedIn]): the zero window where none
	// was, or where the record was written by a build that kept none. NIL on a
	// dispatched segment's launcher, whose launch reads what the seat's park
	// recorded on this node for its delivery instead
	// ([agentLauncher.delivery]). Read by [agentLauncher.continuedFloor].
	relaunchRefusedIn *sandbox.BudgetWindow

	// delivery is, on a dispatched segment's launcher, the ids of the events
	// its turn was dispatched for ([deliveryOf]), under which the seat's park
	// on this node recorded the window a launch of that delivery was refused
	// below its floor in ([floorWaits]); nil on a resumed segment's, whose
	// relaunches read their job's record instead. Read by
	// [agentLauncher.refusedIn].
	delivery []uuid.UUID

	// run is the launch past the turn's fence, nil for [agentLauncher.launch]:
	// a case's stand-in for the sandbox.
	run func(context.Context, runner.AgentRunRequest) (runner.Launched, error)
}

var _ runner.AgentLauncher = (*agentLauncher)(nil)

// LaunchExecutor starts the run and returns. In this order:
//
//  1. The turn's fence ([runner.AgentRunRequest.Fence]): a seat this node no
//     longer holds under the admitting grant, or one a person stopped, buys no
//     box. Asked again of every later refusal.
//  2. The node can launch at all: sandbox runtime, catalogue, a reachable bridge.
//  3. The budget floor ([executorHeadroom]): a budget refusal parks the seat
//     rather than redelivering into the same refusal.
//  4. The member ([agentLauncher.member]) and the cell its box reaches the bridge from.
//  5. The job, its bridge session, then the box ([agentLauncher.launch]).
func (l *agentLauncher) LaunchExecutor(ctx context.Context, req runner.AgentRunRequest) (_ runner.Launched, err error) {
	defer origin.Stamp(&err)
	// THE TURN'S OWN FENCE, the closure its runner was built with, asked
	// again — never one built here. A fence captures the seat's grant when
	// it is BUILT and compares it when it is CALLED, so one built and called
	// in the same instant compares a grant with itself: it could not see a
	// seat that moved and came back under a new grant while the prompt was
	// being built, and it opened on a seat this node does not hold at all.
	// The turn's was built when the turn was admitted, which is the grant
	// this launch has to still be covered by.
	if req.Fence == nil {
		return runner.Launched{}, fmt.Errorf("seat %q's executor launch carries no fence from "+
			"its turn; this is an assembly fault in the runner, not a setting", l.seat.Handle())
	}
	if err = req.Fence(); err != nil {
		return runner.Launched{}, err
	}
	run := l.run
	if run == nil {
		run = l.launch
	}
	launched, err := run(ctx, req)
	if err == nil {
		// THE BRIEF REACHED A RUN, so what the dispatch told the seat is now
		// told: recorded once, by the turn's first launch (capability.go).
		l.engine.tellCapability(ctx, l.turn, l.told)
	}
	if err == nil || turn.Stopped(err) || errors.Is(err, seatpkg.ErrSeatMoved) {
		return launched, err
	}
	// Ask the fence again of every refusal, whatever the launch answered: the
	// seat can move during setup, and the next holder ends a run it finds
	// launching ([sandbox.Coordinator.RecoverSeat]), so the attach meets that
	// ending or no record. Returned as is, that is a broken turn to the
	// dispatcher, redelivered behind newer mail; a closed fence is the move
	// that defers to the holder at once. The launch's refusal rides beside it:
	// [sandbox.ErrSeatNotHeld] hands a resumed relaunch on
	// ([sandbox.Coordinator.OnEvent]) and its class is still readable. Where
	// the fence holds, the refusal is the launch's alone.
	if moved := req.Fence(); moved != nil {
		return launched, fmt.Errorf("%w (the launch it was making: %w)", moved, err)
	}
	return launched, err
}

// launch is [agentLauncher.LaunchExecutor] past the turn's fence: steps 2 to 5
// of its order.
//
// The bridge session opens before the box exists and closes on every path that
// ends the run: a box dialling a live session after the run would hold a key
// to the seat's whole tool surface. It opens from inside the launch
// ([sandbox.LaunchRequest.Opened]) under the job's own name, which the store
// mints there and the durable row keeps, so a node taking the run over reopens
// the same session ([Engine.reopenAgentRun]). It carries the run's model
// ([mcpbridge.Session.Model]) and serves the run's note inbox (ADR-0067). A
// launch failing after its record is written names the job
// ([runner.Launched.LaunchID]); the record answers for every note.
func (l *agentLauncher) launch(ctx context.Context, req runner.AgentRunRequest) (_ runner.Launched, err error) {
	defer origin.Stamp(&err)
	e, company, handle := l.engine, l.company, l.seat.Handle()
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
	// either way, and classed as the configuration it is
	// ([sandbox.WhyConfiguration]), which an operator changes and no wait
	// does.
	manager := company.catalogue
	if coordinator == nil || manager == nil || pending == nil ||
		company.Config.Providers.Sandbox == nil {
		return runner.Launched{}, sandbox.Because(sandbox.WhyConfiguration, fmt.Errorf(
			"seat %q's executor runs in a box, and this node has no sandbox runtime for "+
				"this company: configure providers.sandbox", handle))
	}
	// NO BRIDGE IS A REFUSAL, not a degraded run. A harness with none
	// of the seat's tools cannot answer anybody, cannot touch a ticket and
	// cannot submit its work — it would burn a subscription producing prose
	// nothing collects, and the turn would be rescued as incomplete with no
	// sign of why. Refused HERE, before the launch opens a job, as every
	// case Open refuses is: the session is opened from inside the launch,
	// and a refusal there costs a row opened and abandoned.
	//
	// AND CLASSED AS CONFIGURATION ([sandbox.WhyConfiguration]), as each
	// refusal of a bridge below is and a node with no sandbox runtime above
	// is: what clears it is a setting of the node, the variable or api.port,
	// which an operator changes and no wait does.
	if e.bridge == nil {
		return runner.Launched{}, sandbox.Because(sandbox.WhyConfiguration, fmt.Errorf(
			"an executor needs a tool bridge its box can dial, and this node has none: set %s "+
				"to the fleet's address a box can reach (any node serving the API, or a load "+
				"balancer in front of them); a direct box reaches this node's own listener on "+
				"loopback without it wherever the node binds api.port on every interface or "+
				"on loopback",
			mcpbridge.BaseURLVar))
	}
	// A BRIDGE NOTHING REACHES IS REFUSED BY NAME, because the settings it
	// takes are different fixes: an unset base URL is one variable, and a
	// node that serves the API with no listener (api.port 0) is a second one
	// the first message would send an operator past.
	if !e.bridge.Reachable() {
		return runner.Launched{}, sandbox.Because(sandbox.WhyConfiguration, fmt.Errorf(
			"an executor needs a node serving its tool bridge, and nothing here reaches it: "+
				"this node serves the API with api.port 0, so it binds no listener for the "+
				"bridge's route; set api.port, and point %s at the nodes that serve the API "+
				"as a sandbox reaches them",
			mcpbridge.BaseURLVar))
	}
	// A SURFACE BOUND TO NO SEAT IS REFUSED BY NAME, before the launch, for
	// the reason the two refusals above are: Open refuses it too — every
	// bridged call acts as the surface's seat, and the token names it, so a
	// surface with none could decide nothing — and its empty answer would
	// otherwise be reported as a missing bridge URL, sending an operator to
	// a variable that is set.
	if _, err = mcpbridge.SeatOf(req.Surface); err != nil {
		return runner.Launched{}, fmt.Errorf("the executor's tool surface is bound to no " +
			"seat, so a bridged call would have nobody to act as; this is an assembly " +
			"fault in the runner, not a setting")
	}
	// THE MEMBER, PICKED ONCE AND FIRST ([agentLauncher.member]): a session is
	// continued only by the harness that wrote it, so which member the launch
	// takes decides what its run sends first, and the floor below is reckoned
	// for this pick and no other — never for the member the runner's probe
	// asked about, whose plan may have been spent or come back since. Its
	// hand-offs are announced once the seat's budget lets the launch go on.
	var handoffs []chain.Fallback
	member, agent, pick, pickErr := l.member(ctx, manager, func(f chain.Fallback) { handoffs = append(handoffs, f) })
	// THE CELL THE RUN GOES TO ([memberCell]), because it decides which
	// address its box dials the bridge at ([mcpbridge.Session.OnHost]) — and
	// whether the launch can continue the job before's session in that job's
	// box at all, which the floor below is reckoned for.
	var cell sandbox.Placement
	inBox := true
	if pickErr == nil {
		cell = memberCell(company, manager, member.Key)
		inBox = l.continuesInItsBox(ctx, pending, manager, agent, cell, req.Continue)
	}
	// THE FLOOR OF WHAT THIS LAUNCH SENDS FIRST ([launchFloor]): its fresh
	// brief beside the static floor, held at the static floor, or at the room
	// below it, where that is past the most room a window can leave the
	// launch; and, for a launch that may continue a session, that session's
	// context with the brief that continues it where that is larger, since a
	// continued session sends its context before its brief. A session no
	// window of the seat's budget can ever carry is not continued, its run
	// starting a session of its own on its fresh brief
	// ([agentLauncher.continuedFloor]).
	req, floor := l.continuedFloor(ctx, req, agent, inBox)
	floor.beside = e.besideFloor(company, handle, l.turn.RunID)
	if err = executorHeadroom(ctx, e.remainingFor(company, handle), floor); err != nil {
		return runner.Launched{}, err
	}
	if req.OnFallback != nil {
		for _, f := range handoffs {
			req.OnFallback(f)
		}
	}
	if pickErr != nil {
		return runner.Launched{}, pickErr
	}
	if pick != nil && req.OnFleetPick != nil {
		req.OnFleetPick(pick)
	}

	// A CELL THAT CANNOT DIAL THE BRIDGE IS REFUSED BY NAME, before the launch
	// opens a job, for the reason the refusals above are. A direct box is a
	// process of this host and dials this node's own listener on loopback,
	// where the node serves the bridge's route; a box in any other cell is
	// not on this host's loopback, and reaches the fleet only at the
	// address the variable gives.
	if e.bridge.Reaches(cell == sandbox.Direct) == "" {
		return runner.Launched{}, sandbox.Because(sandbox.WhyConfiguration, fmt.Errorf(
			"an executor needs a tool bridge its box can dial, and a box in the %s cell "+
				"cannot reach this node on its loopback: set %s to the fleet's address that "+
				"box can reach", cell, mcpbridge.BaseURLVar))
	}

	// The run's calls, and with each what they have cost the engine so far,
	// for the segment that resumes from the run to pay — bound to the run's
	// job by the launch below, before the box that makes them exists.
	ledger := newBridgeLedger(pending, req.Spend)
	ledger.nudge = e.nudgeRun
	// WHETHER THE LAUNCH CONTINUES A SESSION, decided once it has its box and
	// before the run starts, and read on the run's first request to its
	// bridge, on another goroutine, to read the box's word on the session it
	// started against ([boxStarted]).
	var continuing atomic.Bool
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
		// THE SKILLS ITS HARNESS IS HANDED AS ITS OWN, by the name it lists
		// each under: a load the harness reports is the run's load of the
		// key (internal/api/mcpbridge, skills.go).
		Skills: req.Skills.Listed(),
		// AND THE SESSION ITS BOX STARTED, as the box says it by the token
		// its harness dials, before the run's first call: the run's guard is
		// held to it, and the job's record keeps it (internal/api/mcpbridge,
		// started.go).
		Started: boxStarted(ledger, l.turn.RunID, continuing.Load, req.Started),
	}

	settings := seatSandbox(company, handle)
	// A RUN ENVIRONMENT THAT DOES NOT RESOLVE IS SAID AND NOT REFUSED. This
	// run is the turn's executor itself: refused, the phase fails and the
	// turn with it (internal/agent/turn), so every message the seat could
	// answer through its bridge would fail for a code-host token a pass may
	// mint in a minute — the reason a seat's mailbox does not wait for a
	// tool server held back for its credential (toolcredentials.go). The
	// run starts without what is missing, and the line says why it is.
	runEnv := e.runEnvironment(ctx, company, l.seat, settings, manager.DefaultSetup(),
		e.boxDefaults(company.Config, handle))
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

	run := newExecutorRun(agent, runEnv)
	if len(run.withheld) > 0 {
		log.WarnContext(ctx, "sandbox_credential_withheld", "seat", handle, "variables", run.withheld,
			"detail", "the run's harness signs in by these names, so the agent's own integration "+
				"credentials are not put there: run the seat's executor on an entry that signs in by another name")
	}
	// THE RUN'S MODEL ON ITS SESSION, before the session opens: see
	// [mcpbridge.Session.Model].
	session.Model = run.model
	spec := manager.BuildSpec(sandbox.SpecInput{
		Placement:       cell,
		Harness:         agent.Harness(),
		PauseTTL:        pauseTTL(settings),
		MaxTurns:        maxTurnsFor(settings),
		Env:             run.env,
		SignInEnv:       run.signInNames,
		CredentialFiles: run.files(),
		CredentialRules: run.fileRules(),
	})
	// THE PICKED MEMBER'S LOGIN, the one the box will actually run under:
	// a fallback launch on a later member is judged on that member's entry,
	// never the chain's head.
	if err = sandboxCredentials(agent, member.Key, handle, spec.Placement, run.signIn); err != nil {
		return runner.Launched{}, err
	}

	// THE TURN'S NOTES, sealed for the record the launch opens: the seat a
	// person's authority is decided on, and what the brief carries, read by
	// the job the write names — a note a lost take's settle handed the brief
	// among them, in that take's place — see runsteer.go.
	ref := l.runTurnRef(ctx)
	brief, err := e.runSteers.briefNotes(l.turn.RunID, req.Round, req.Notes, l.notes)
	if err != nil {
		return runner.Launched{}, err
	}
	// AND THE RUN'S NOTE INBOX, which its session serves to its harness between
	// steps, bound to the job with the ledger once the job has a name, and
	// before the bridge files the session ([Engine.openBridge]).
	inbox := e.runSteers.inbox(identityOfRef(ref), req.Round, agent.Harness())
	if inbox != nil {
		session.Steer = inbox
	}
	// THE FIRST SEGMENT'S BOX, HANDED OVER IN THE WRITE THAT CREATES THE
	// RECORD — the last step before the launch, past every refusal above:
	// its intake ends here, and what it holds rides the write, so nothing a
	// person files on the record once it exists lands ahead of it. Settled on
	// every path out: recorded the moment the write lands, and given back to
	// the box where the launch failed before it ([steerDesk.handOver]).
	hand := e.steers.handOver(l.turn.RunID)
	handed, err := e.runSteers.handedNotes(l.turn.RunID, hand.held())
	if err != nil {
		hand.unrecorded(ctx)
		inbox.Close()
		return runner.Launched{}, err
	}

	// The turn's code holds go on the row the launch opens, sealed from the
	// read until that row exists so no hold is taken in between that neither
	// the set nor the row would carry ([launchHolds]). The row's writer attaches
	// when the job is named, before its bridge session opens, so every hold a
	// bridged call takes is on the row first and survives a park or restart.
	// Sealed last, past every refusal above, which would have to unseal it.
	//
	// The run's checkouts: a round carried on keeps its job's
	// ([runner.Continuation]) and reaches only what that job checked out; any
	// other launch starts with none and checks out through `code_checkout`
	// (ADR-0093).
	grants, err := carriedGrants(ctx, pending, l.turn.RunID, req.Continue != nil)
	if err != nil {
		return runner.Launched{}, err
	}
	spec.Repos = grants.Repos
	// THE BOX'S GIT, on every launch on a node serving Gitlet:
	// Gitlet's base, git's configuration through the
	// environment, and the run's credential, minted for the launch the store
	// is about to name, so a job a relaunch supersedes is refused at
	// Gitlet whatever its token's expiry. It reaches nothing until a checkout
	// grants a repository on the row, which Gitlet's box admission
	// reads on every request. Its environment is the launch's own, over the
	// run environment just assembled, never the spec's or the row's.
	boxGit := e.launchBoxGit(l.seat, l.turn.RunID, run.env)

	holds := launchHolds(l.turn.Holding(), pending, l.turn.RunID)
	var (
		opened   bool
		recorded string
		// bridgeFrom is the last call the log held as the job's session
		// opened, which the job's calls are numbered after.
		bridgeFrom int
	)
	result, err := coordinator.Launch(ctx, manager, withRunsOwn(req, pick, sandbox.LaunchRequest{
		Turn:       ref,
		ReadBox:    e.boxUnread(ctx, company, handle, cell),
		Spec:       spec,
		Setup:      runEnv.setup,
		MCPServers: servers,
		// WHAT OF ITS ENVIRONMENT IS A CREDENTIAL, which only this side can
		// tell, for its session's scrub.
		Secrets: launchSecrets(agent, runEnv, run.env),
		// AND THE PATHS ITS BOX FILES ARE NAMED BY, under the box's home,
		// joined onto the box's own once the launch has one.
		HomePaths:   run.homePaths,
		Skills:      req.Skills.Box(),
		Model:       run.model,
		Effort:      run.effort,
		WebDenied:   run.webDenied,
		ProviderKey: member.Key,
		// THE SIGN-IN THE JOB'S HARNESS IS HANDED, on the job's record: what
		// its collection compares the entry's against ([Engine.signInReplaced]).
		SignInID: string(agent.SignInID()),
		// AND THE ENTRY IT RUNS UNDER, which every session the launch would
		// continue is held to ([sandbox.EntryContinues]).
		Entry:  agent.EntryID(),
		System: req.System,
		Brief:  req.Brief,
		// HOW TO WAIT ON A PERSON, with the names the chart gives this
		// seat, from the pinned epoch (see [askBrief]). Without it the run
		// holds `ask_and_wait` and nothing tells it when to park on it.
		Ask: askBrief(company.Org, l.seat),
		// THE RUN AS A PERSON READS IT ([turnctx.Turn.Title]): what the
		// turn was asked, in its trigger's own words, redacted. Never the
		// brief's task, whose fence lines and escapes are a model's, and
		// which a resume reads off the park ([resumeTask]) and never off
		// the row.
		Task:  l.turn.Title,
		Label: l.turn.Title,
		// A RUN CARRIED ON WITH A PERSON'S ANSWER, continued in its own
		// session where the launch runs it in the asking run's box — see
		// [runner.Asked] — or a ROUND CARRIED ON after its job was lost with
		// its host, in a session of its own ([runner.Lost]). Either keeps the
		// round's calls in the log.
		Continue: sandboxContinuation(req.Continue),
		// AND THE SESSION IT CONTINUES in whatever box the launch gets — the
		// turn's kept capture, or the session its conversation's last turn
		// left — where the turn's runner was told it would be
		// ([agentLauncher.ProbeSession]), with the briefs that continue it.
		Session: sandboxSession(req.Session),
		// AND THE TURN'S READ OF ITS CONVERSATION, which its first launch
		// alone freezes on the record it creates and records on the executor
		// log, whatever this launch then takes up: a relaunch of a turn that
		// keeps nothing of its own continues it.
		Base:       l.conversation.frozen(),
		SteerSeat:  e.runSteers.anchor(ref.TurnID, ref.AgentID, ref.AgentHandle),
		BriefNotes: brief,
		// AND THE NOTES ITS BRIEF TELLS AGAIN, whose reads the record marked
		// unanswered: recorded read by the job, at their takes' own places.
		RetoldNotes: retoldNotes(req.Round, req.Retold),
		HandedNotes: handed,
		CodeHolds:   holds.ids,
		CodeFloor:   grants.CodeFloor,
		CodeCeiling: grants.CodeCeiling,
		// AND WHOM ITS TURN WORKS FOR, which a round carried on keeps with
		// the grants: its box's pushes and its change carry it.
		CodeWorkingFor: grants.CodeWorkingFor,
		RunEnv:         boxGit,
		Recorded: func(launch string) {
			// THE RECORD HOLDS THE TURN'S NOTES FROM HERE, the box's
			// included: a note to the turn waits on it, whichever node it
			// reaches, and a launch that fails from here ends it, whose
			// ending reports what no job ran with.
			recorded = launch
			hand.recorded()
		},
		Opened: func(job sandbox.OpenedJob) (sandbox.Opening, error) {
			holds.opened(job.LaunchID)
			opening, openErr := e.openBridge(ctx, session, ledger, inbox, job)
			opened, bridgeFrom = openErr == nil, job.LastSeq
			return opening, openErr
		},
		// HOW THE RUN TAKES UP A SESSION, told to the runner once the launch
		// knows its box and before the run starts there, so the run's guard
		// and its prompt's record are the session's it runs in.
		Decided: func(d sandbox.LaunchDecision) {
			decided := sessionDecision(d)
			continuing.Store(decided.Continues)
			if req.Decided != nil {
				req.Decided(decided)
			}
		},
		// THE SAME FENCE, asked again once the run's row exists — see
		// [sandbox.LaunchRequest.Fence]: a stop that lands while this
		// launch is being built either finds the row or is refused here.
		Fence: req.Fence,
	}))
	holds.settle(err)
	if err != nil {
		if recorded == "" {
			// NO RECORD WAS WRITTEN: the box's notes are the box's again.
			hand.unrecorded(ctx)
		}
		if opened {
			// The launch failed after the job's session opened, so
			// nothing will ever close it on the completion path — and
			// closing it closes its inbox.
			e.bridge.Close(l.turn.RunID)
		} else {
			// NO SESSION OPENED: the inbox's close stops the view a bind
			// made ahead of the bridge's refusal started.
			inbox.Close()
		}
		// A LAUNCH A PERSON'S STOP ENDED UNDER IT — its row's ending decided
		// between the box and the job, so the attach was refused — is the
		// stop, and the turn reports it as one rather than as a launch that
		// broke, naming who stopped it: as the coordinator kept the stop's
		// decision for this frame, never as the seat's pause reads by now,
		// which a person may have lifted since ([sandbox.Coordinator.LaunchStopped]).
		// Taken whatever the launch answered, so a stop that came with its own
		// fence's refusal is not kept for nobody.
		//
		// AND WHETHER THE RECORD WAS WRITTEN, by the job it opened: its
		// brief's notes are the record's then, never the turn's to hand back.
		failed := runner.Launched{LaunchID: recorded}
		decided, stopped := coordinator.LaunchStopped(l.turn.RunID)
		switch {
		case stopped:
			// A STOP DECIDED ON THE RUN, whether or not the turn's own fence
			// refused the launch beside it: the stop's ending answers the
			// turn's colleague and records its end, so the error says the
			// stop is the run's ([stoppedByRun]) and the frame does neither.
			log.InfoContext(ctx, "executor_launch_stopped", "seat", handle, "turn_id", l.turn.RunID,
				"error", err, "detail", "a person stopped the seat's running work while its "+
					"executor was launching; the launch is ended and the turn stops")
			return failed, &stopError{pause: pauseOfStop(decided), handle: handle, ofRun: true}
		case recorded != "":
			// THE LAUNCH ABANDONED THE RECORD IT WROTE — the turn's own fence
			// refused it once the record was written, or the box, the job or
			// its attach failed after ([sandbox.Coordinator.Launch]) — so no
			// ending of the run owes the turn's end, and the frame that ends
			// the turn records it: stopped where its fence was, and failed,
			// saying why, where the launch was.
			e.abandonedLaunches.Store(l.turn.RunID, struct{}{})
		}
		return failed, err
	}
	if result.Read != nil {
		e.keepBoxReading(ctx, company, handle, cell, *result.Read)
	}
	return runner.Launched{LaunchID: result.LaunchID, BridgeFrom: bridgeFrom}, nil
}

// floorUnusedOutcome is what a launch whose floor drops the briefs told by a
// reading alone offers instead ([agentLauncher.continuedFloor]).
//
// AN OFFER, NEVER A HAND: the floor is judged before the box is acquired, and
// the session itself may still not be continued (another member's harness, a
// carry its box refuses, a fetch that fails), which
// `executor_session_not_continued` then says.
const floorUnusedOutcome = "the launch offers the session told without the reading; " +
	"executor_session_not_continued says so where the session itself is not continued"

// continuedFloor is req as the seat's budget lets it launch on member (nil
// where none was picked), and the floor ([launchFloor]) it is held to: the
// largest of the briefs it may hand, capped at the room the windows leave
// ([windowRoom], [launchFloor.fresh]).
// A session survives only on the harness that wrote it
// ([runner.SessionBasis.Harness]) and only if some window and model can carry
// its floor ([launchFloor.fits], [launchFloor.carriedBy]); else it is dropped,
// never waited for. Each session has its own floor, since the box takes the
// first where it can (continueSession): one drops alone, the other stays, at
// the larger floor of those offered ([agentLauncher.droppedAlone], [keptFloor]).
// A try refused below its floor waits one turnover at most
// ([agentLauncher.refusedIn], [launchFloor.after], [launchFloor.stillShort]).
func (l *agentLauncher) continuedFloor(ctx context.Context, req runner.AgentRunRequest,
	member *harness.Provider, inBox bool) (runner.AgentRunRequest, launchFloor) {

	if s := req.Session; s != nil && member != nil {
		why := ""
		if s.Basis.Harness != member.Harness() {
			why = fmt.Sprintf("the session was written by %s, and the member the launch picked runs %s: a "+
				"session is continued only by the harness that wrote it", s.Basis.Harness, member.Harness())
		} else if ok, refused := sandbox.EntryContinues(s.Basis.Entry, member.EntryID(), member.Harness(),
			executor.ResumesOnLaunchRoute(member.Harness())); !ok {
			why = refused
		}
		if why != "" {
			log.InfoContext(ctx, "executor_session_not_continued", "seat", l.seat.Handle(),
				"turn_id", l.turn.RunID, "detail", why, "outcome", "the run starts a session of its own")
			req.Session = nil
		}
	}
	static := staticFloor(l.company)
	// THE MOST ROOM A WINDOW CAN LEAVE THIS LAUNCH: each capped window's
	// ceiling less what the launch's own turn has charged in it, which is in
	// that window before the launch reads it and is charged there again by
	// every turn that tries again ([windowRoom]).
	var room windowRoom
	m := l.engine.meterFor(l.company, l.seat.Handle())
	if m != nil {
		room = m.basis.room(turnCharged(l.turn, m.windows()))
	}
	// THE WINDOW AN EARLIER TRY WAS REFUSED BELOW ITS FLOOR IN, where it has
	// turned over since: that try waited for it, and this one waits for no
	// more than the static floor alone ([launchFloor.after]). A delivery a
	// redelivery merged newer mail into can carry two, each its own event's:
	// one that has turned over is a wait its work has done.
	var waited sandbox.BudgetWindow
	for _, w := range l.refusedIn() {
		if m.turnedOver(w) {
			waited = w
			break
		}
	}
	// THE FRESH BRIEF FIRST, since every launch may hand its run that brief,
	// however many sessions it offers: its box falls back to it wherever it
	// takes none of them up, a restore that fails in the box included.
	fresh := static.fresh(harness.EstimateTokens(req.Brief), room)
	if waited.Valid() {
		fresh = fresh.after(waited)
	}
	floor := fresh
	// WHAT THE SEAT'S WINDOWS LEAVE NOW, read once where the launch has waited
	// and offers a session, which it drops where they are short of its floor
	// again ([launchFloor.stillShort]). A read that fails drops nothing: the
	// launch's own read of the same counters refuses it then
	// ([executorHeadroom]), so no session is given up on a store's blip.
	stillShort := func(launchFloor) string { return "" }
	if waited.Valid() && (req.Session != nil || (req.Continue != nil && req.Continue.SessionID != "" && inBox)) {
		if left, err := m.Remaining(ctx); err == nil {
			stillShort = func(f launchFloor) string { return f.stillShort(left, waited) }
		}
	}
	carried := func(f launchFloor) string {
		if why := uncarried(member, room, f); why != "" {
			return why
		}
		return stillShort(f)
	}
	var keptWhy, inPlaceWhy string
	// THE RATE THE NEXT REQUEST'S MODEL IS COUNTED AT, which the harness's
	// estimate of a continued session's next request is read at
	// ([continuedContext]).
	var older bool
	if member != nil {
		older = memberWindow(member, false).OlderText
	}
	if s := req.Session; s != nil {
		kept := keptFloor(static, s, older)
		keptWhy = carried(kept)
		// THE BRIEFS TOLD BY A READING, AND THE UNHELD ONES, EACH DROPPED
		// ALONE where theirs beside the session's own is a floor that cannot
		// be carried and the session's own briefs' can; on a copy, never the
		// caller's, which the runner still holds. Where the session's own
		// floor fails too, it is dropped by that floor, which its line names.
		if keptWhy != "" && (s.Exact != nil || s.Unheld != nil) {
			own := *s
			own.Exact, own.Unheld = nil, nil
			if whyOwn := carried(keptFloor(static, &own, older)); whyOwn != "" {
				keptWhy = whyOwn
			} else {
				without := l.droppedAlone(ctx, s, own, static, older, carried)
				req.Session, kept, keptWhy = &without, keptFloor(static, &without, older), ""
			}
		}
		if keptWhy == "" {
			floor = floor.atLeast(kept)
		}
	}
	if c := req.Continue; c != nil && c.SessionID != "" && inBox {
		// THE LONGER OF ITS TWO BRIEFS, never the summarized one dropped
		// alone: it is what the launch hands where the box's point does not
		// prove the held one whole ([runner.Continuation.Summarized]).
		inPlace := static.continuing(continuedContext(c.NextRequest, c.ContextTokens, older),
			max(harness.EstimateTokens(c.Brief), harness.EstimateTokens(c.Summarized)))
		if inPlaceWhy = carried(inPlace); inPlaceWhy == "" {
			floor = floor.atLeast(inPlace)
		}
	}
	if keptWhy != "" {
		req.Session = nil
	}
	if inPlaceWhy != "" {
		fresh := *req.Continue
		fresh.SessionID = ""
		req.Continue = &fresh
	}
	// SAID ONCE BOTH ARE DECIDED, so the line names what the launch still
	// offers its run rather than what it offered before the other was judged.
	outcome := "the run starts a session of its own, on its fresh brief's floor"
	if req.Session != nil || (req.Continue != nil && req.Continue.SessionID != "" && inBox) {
		outcome = "the launch offers its run the other session it may continue, which its box takes " +
			"up where it can; otherwise the run starts a session of its own"
	}
	for _, why := range []string{keptWhy, inPlaceWhy} {
		if why != "" {
			log.InfoContext(ctx, "executor_session_not_continued", "seat", l.seat.Handle(),
				"turn_id", l.turn.RunID, "detail", why, "outcome", outcome)
		}
	}
	// THE HOLD, SAID ONCE THE LAUNCH'S FLOOR IS DECIDED, and naming that
	// floor. A held fresh floor is the static floor or the room below it, so a
	// session the launch still offers, its own floor within that room, may
	// hold the launch higher: said before the sessions were judged, the line
	// named a floor the launch was not held to. And it is said wherever the
	// fresh floor is held, whatever floor holds the launch: its box may still
	// start its run on the fresh brief, a restore that fails in the box
	// included, and that run's first request is the one the hold lets past
	// what the window has left.
	if fresh.held() {
		heldOutcome := fmt.Sprintf("the launch is held at %s, and its run's first request may take its "+
			"window past its ceiling, which the run's charge then says (sandbox_spend_over_budget)",
			fresh.heldAt())
		if !floor.held() {
			// A SESSION'S FLOOR HOLDS THE LAUNCH, being the larger: a
			// session's floor carries no room ([launchFloor.room]), so it is
			// never held.
			heldOutcome = fmt.Sprintf("the launch is held at %d, the floor of a session it offers its run, "+
				"which its box takes up where it can; where its run starts a session of its own on the "+
				"fresh brief, that run's first request may take its window past its ceiling, which the "+
				"run's charge then says (sandbox_spend_over_budget)", floor.tokens())
		}
		log.InfoContext(ctx, "executor_floor_held", "seat", l.seat.Handle(),
			"turn_id", l.turn.RunID, "floor", floor.tokens(), "detail", fmt.Sprintf("the run's fresh brief "+
				"is about %d tokens, which beside turn_engine.executor_min_budget_tokens (%d) is %s",
				fresh.brief, fresh.static, fresh.heldFor()),
			"outcome", heldOutcome)
	}
	return req, floor
}

// droppedAlone is s less each telling beside its own briefs whose floor, beside
// own's, carried cannot carry ([agentLauncher.continuedFloor]): the briefs told
// by a reading, logged, and the unheld briefs, marked dropped, so the launch
// continues the session only in a box whose release the resume was read in.
func (l *agentLauncher) droppedAlone(ctx context.Context, s *runner.SessionContinuation,
	own runner.SessionContinuation, static launchFloor, older bool,
	carried func(launchFloor) string) runner.SessionContinuation {

	without := *s
	if s.Exact != nil {
		with := own
		with.Exact = s.Exact
		if why := carried(keptFloor(static, &with, older)); why != "" {
			log.InfoContext(ctx, "executor_session_holdings_unused", "seat", l.seat.Handle(),
				"turn_id", l.turn.RunID, "detail", why, "outcome", floorUnusedOutcome)
			without.Exact = nil
		}
	}
	if s.Unheld != nil {
		with := own
		with.Unheld = s.Unheld
		if carried(keptFloor(static, &with, older)) != "" {
			without.Unheld, without.UnheldDropped = nil, true
		}
	}
	return without
}

// refusedIn is every window an earlier try of this launcher's work was refused
// below its floor in, none where none is known: on a resumed segment, the one
// its job's record names ([agentLauncher.relaunchRefusedIn]); on a dispatched
// one, those the seat's parks recorded for the events its turn was dispatched
// for ([agentLauncher.delivery], [floorWaits]), never another delivery's: a
// park's release hands the seat a person's message before the deferred
// delivery, and that launch, given the wait it never made, would go out below
// its own floor while the delivery that waited was refused again.
//
// Read, never taken: the record lasts as long as its delivery
// ([Dispatcher.Gone]), so a try refused for anything but its floor (a chain at
// its limit included) leaves the next try held as it was.
func (l *agentLauncher) refusedIn() []sandbox.BudgetWindow {
	if l.relaunchRefusedIn != nil {
		return []sandbox.BudgetWindow{*l.relaunchRefusedIn}
	}
	if len(l.delivery) == 0 || l.company == nil || l.company.Org == nil {
		return nil
	}
	if seat, ok := l.company.Org.AgentIDFor(l.seat); ok {
		return l.engine.floorWaits.waited(seat, l.delivery)
	}
	return nil
}

// keptFloor is the floor of a launch that continues s, the session it was asked
// to continue in any box ([runner.AgentRunRequest.Session]): the turn's kept
// capture, in the longest of its briefs (its two, and the two told by a
// reading, [runner.SessionContinuation.Exact], and for a box whose release the
// resume was not read in, [runner.SessionContinuation.Unheld], where it carries
// them, since which box and release the launch gets are its to learn), or the
// session its conversation's last turn left, in its one; static where s names
// neither. And the two told as a session that may be a summary, which the
// launch hands where its box's point does not prove the others whole
// ([runner.SessionContinuation.Summarized]), never dropped alone. The kept
// capture's context is its harness's estimate of its next request at the rate
// older says where the capture holds one ([continuedContext]).
func keptFloor(static launchFloor, s *runner.SessionContinuation, older bool) launchFloor {
	switch {
	case s.Basis.Continues():
		// THE KEPT CAPTURE'S OWN CONTEXT, as it measured it, never its job's
		// copy's ([runner.SessionBasis.CopyContext]): that one sizes the
		// in-place floor, and a restored capture sends what it holds.
		brief := max(harness.EstimateTokens(s.Brief), harness.EstimateTokens(s.Replaced))
		if x := s.Exact; x != nil {
			brief = max(brief, harness.EstimateTokens(x.Brief), harness.EstimateTokens(x.Replaced))
		}
		if x := s.Summarized; x != nil {
			brief = max(brief, harness.EstimateTokens(x.Brief), harness.EstimateTokens(x.Replaced))
		}
		if u := s.Unheld; u != nil {
			brief = max(brief, harness.EstimateTokens(u.Brief), harness.EstimateTokens(u.Replaced))
		}
		return static.continuing(continuedContext(s.Basis.NextRequest, s.Basis.ContextTokens, older), brief)
	case s.Basis.ContinuesBase():
		// THE BASE'S OWN CONTEXT, never the job before's: that one sizes the
		// in-place floor, and a base is continued only in a box of its own,
		// with its one brief.
		return static.continuing(s.Basis.Base.ContextTokens, harness.EstimateTokens(s.Replaced))
	}
	return static
}

// continuedContext is the context a continued session sends again before a
// word of its brief, which its launch's floor is made of: next, its harness's
// own estimate of its next request, at the rate its harness counts the model the
// launch's member hands it at ([sandbox.NextRequest.At]), where it holds
// a figure; else last, its last request's input, which under-counts the next
// request by its reply and all after it, the figure of every record holding no
// estimate (an older build's, a capture settled off the executor log, a thread
// not read whole, a harness whose reader estimates none). Never the larger:
// past a compaction no request has followed, last is the compacted-away
// conversation's and the estimate the nearer, though short of the request by
// its system prompt and tools until a reply anchors it (the residual
// internal/sandbox/executor's package doc states).
func continuedContext(next sandbox.NextRequest, last int64, older bool) int64 {
	if n := next.At(older); n > 0 {
		return n
	}
	return last
}

// uncarried is why no launch on member can carry f, "" where one can: one
// request of the member's model, under the window its harness holds it to on
// the member's platform where it compacts by its window ([floorWindow],
// [launchFloor.carriedBy]), and room, the most room a window of the seat's
// budget can leave the launch as it turns over ([launchFloor.fits]), none to
// judge where nothing is capped.
func uncarried(member *harness.Provider, room windowRoom, f launchFloor) string {
	if member != nil {
		if w, ok := floorWindow(member); ok {
			if why := f.carriedBy(w); why != "" {
				return why
			}
		}
	}
	return f.fits(room)
}

// floorWindow is the window one request of member's model can carry, false
// where its harness compacts by no window: the pin, held to the least window
// the harness holds the model to on the member's own platform, read exactly
// off its entry, where that is less ([sandbox.ExecutorWindow.Standard]). So a
// session kept under a larger window and continued on a member held to
// 200,000 tokens is judged under 200,000, which one request of that model can
// carry, and not under the 1,000,000 its pin names.
func floorWindow(member *harness.Provider) (sandbox.ExecutorWindow, bool) {
	w, ok := executor.Window(member.Harness(), windowAsk(member, true))
	if w.Standard > 0 && w.Standard < w.Tokens {
		w.Tokens = w.Standard
	}
	return w, ok
}

// memberWindow is the window member's harness holds a run of it to, zero where
// it compacts by no window; exact reads the member's platform off its env
// ([executor.WindowAsk.Exact]).
func memberWindow(member *harness.Provider, exact bool) sandbox.ExecutorWindow {
	w, _ := executor.Window(member.Harness(), windowAsk(member, exact))
	return w
}

// windowAsk is what a window of member is asked by.
func windowAsk(member *harness.Provider, exact bool) executor.WindowAsk {
	ask := executor.WindowAsk{Model: member.ModelArgument(), SignIn: signInOf(config.HarnessAuthMode(member.AuthMode())),
		Exact: exact}
	if exact {
		ask.Env = member.SandboxEnv()
	}
	return ask
}

// signInOf is how an entry signed in by mode hands its harness credentials, as
// the executor judges a window's route: first-party alone for a login, a key or
// the host's own environment (config refuses a sign-in switch in such an
// entry's env), its own env for `env`, and none this build knows otherwise.
func signInOf(mode config.HarnessAuthMode) executor.SignIn {
	switch mode {
	case config.AuthSubscription, config.AuthAPIKey, config.AuthInheritEnv:
		return executor.SignInFirstParty
	case config.AuthEnv:
		return executor.SignInOwnEnv
	}
	return ""
}

// turnCharged is what t's turn has charged in each window of w, by period, read
// off the meter it charges through ([turnctx.Turn.Budget], that turn segment's
// [meter] wherever a counter counts the seat), nil where it has none.
//
// THAT METER, never one the launch builds: what reaches a window before the
// launch reads it is the turn's own context assembly, its review and its
// rewrites, every one charged through the turn's meter ([turnTelemetry.aux]),
// and a meter of the launch's own has charged nothing.
func turnCharged(t *turnctx.Turn, w coord.Windows) map[period.Period]int {
	if t == nil {
		return nil
	}
	if m, ok := t.Budget.(*meter); ok && m != nil {
		return m.chargedIn(w)
	}
	return nil
}

// continuesInItsBox reports whether a launch on agent, its run in cell, could
// continue the job before's own session in that job's box (c, the session it
// would carry on there; [runner.Continuation]): the box the turn's run record
// names, of agent's harness in cell and reached from this node — the launch's
// own tests of a box it continues in ([sandbox.PriorBox.ContinuesIn],
// [sandbox.PriorBox.ReachableFrom]), asked of the record as it stands before
// the launch opens its job, as the launch asks them as it does. True where the
// launch carries no session on in a box, and where the record cannot be read,
// which holds it to that session's floor: the safe direction for a floor.
func (l *agentLauncher) continuesInItsBox(ctx context.Context, pending sandbox.PendingStore,
	manager *sandbox.Manager, agent *harness.Provider, cell sandbox.Placement, c *runner.Continuation) bool {

	if c == nil || c.SessionID == "" {
		return true
	}
	run, found, err := pending.Get(ctx, l.turn.RunID)
	if err != nil || !found {
		return true
	}
	// DATED AS THE ROW DATES IT ([sandbox.PendingRun.BoxSince]): a box past
	// [sandbox.KeptBoxMaxAge] is one the launch reclaims, and an undated one
	// it never continues in, so the floor is the fresh session's there too.
	prior := sandbox.PriorBox{SandboxID: run.SandboxID, Placement: run.Placement, Harness: run.Harness,
		Status: run.Status, LaunchNode: run.LaunchNode, Since: run.BoxSince}
	// AND UNDER THE ENTRY THAT COPY RAN UNDER, as the launch holds it
	// ([sandbox.EntryContinues]).
	entry, _ := sandbox.EntryContinues(run.Launch.Entry, agent.EntryID(), agent.Harness(),
		executor.ResumesOnLaunchRoute(agent.Harness()))
	return entry && prior.ReachableFrom(l.engine.id, manager.DefaultPlacement()) &&
		prior.ContinuesIn(sandbox.Spec{Placement: cell, Harness: agent.Harness()}, l.engine.now())
}

// openBridge opens a job's bridge session under the job's own name, with its
// ledger and its note inbox bound to the same job, and answers what the box is
// given for it: its server entry, and where its harness asks for its turn's
// notes ([sandbox.LaunchRequest.Opened]), for the launch and for a run taken
// over while it works ([Engine.reopenAgentRun]), whose box already holds the
// endpoint. The session numbers its calls after the ones the log holds
// ([sandbox.OpenedJob.LastSeq]) and reads each call's id under the key the job
// names ([sandbox.OpenedJob.CallIDMeta]), set here. THE INBOX IS BOUND BEFORE
// THE BRIDGE FILES THE OPENING, which a recovery's box reaches at once: bound
// after Open, it was served unbound until then. The caller closes the inbox of
// a session that does not open. The package doc's "An OpenCode confirmation is
// recorded once, and reported" says why.
func (e *Engine) openBridge(ctx context.Context, s *mcpbridge.Session, ledger *bridgeLedger,
	inbox *runInbox, job sandbox.OpenedJob,
) (_ sandbox.Opening, err error) {
	defer origin.Stamp(&err)
	ledger.bind(job)
	// WHO RUNS THE SESSION'S CALLS, as every entry it writes ahead of a call
	// names it: what a collection on any node reads this process's liveness by.
	ledger.runner = e.callRunner()
	s.Launch, s.LastSeq, s.CallIDMeta = job.LaunchID, job.LastSeq, job.CallIDMeta
	inbox.Bind(ctx, job.LaunchID, sessionFence(s.Surface))
	endpoint := e.bridge.Open(ctx, s)
	if endpoint == "" {
		// UNREACHABLE past the refusals LaunchExecutor makes by name, which
		// are every case Open refuses — said rather than assumed, because a
		// box launched without its endpoint has none of the seat's tools.
		return sandbox.Opening{}, fmt.Errorf("the tool bridge opened no session for run %s's "+
			"job %s; this node's log says why (mcp_bridge_*)", s.RunID, job.LaunchID)
	}
	// AS LONG AS THIS NODE'S BRIDGE CAN TAKE TO ANSWER A CALL is how long the
	// box's harness waits for one, on every server it is handed: the bridge
	// keeps a running call's answer going, so nothing else bounds the wait
	// ([mcpbridge.Bridge.CallTimeout]).
	wait := e.bridge.CallTimeout(s.Surface)
	opening := bridgeOpening(endpoint, s, inbox != nil, wait)
	// AND THE SAME UNDER THE JOB'S OTHER TOKEN, for a session its box starts of
	// its own in place of the one its launch continues: the token its harness
	// dials is the box's word on which session it started
	// (internal/api/mcpbridge, started.go). Handed to the run only where the
	// launch continues one ([sandbox.Opening.Fresh]); minted here because the
	// job is named before its launch knows its box, and a token is worth
	// nothing outside the box that is handed it.
	if fresh := e.bridge.FreshEndpoint(s); fresh != "" {
		other := bridgeOpening(fresh, s, inbox != nil, wait)
		opening.Fresh = &other
	}
	return opening, nil
}

// callRunner is the incarnation every bridged call this process runs names as
// its runner ([sandbox.BridgeCall.Runner]): the one its sandbox coordinator
// holds its taker lease under ([sandbox.CoordinatorOptions.Incarnation]), where
// it holds one — a lease store wired and a runtime built — and "" where it
// holds none, whose calls a collection waits for by their bound alone rather
// than read as stopped the moment it finds no lease.
func (e *Engine) callRunner() string {
	if e.backends == nil || e.backends.Coord == nil || e.sandbox.Load() == nil {
		return ""
	}
	return e.incarnation
}

// bridgeOpening is what a job's box is given at endpoint, one of its bridge
// tokens: the bridge's servers ([bridgeServers]), each waited on for a call
// as long as wait, where its harness asks for its turn's notes where steered,
// and where it reports loading one of its skills where it is handed any —
// under THE SAME NAMES THE BRIDGE SERVES, so the box and the bridge cannot
// disagree about where they are.
func bridgeOpening(endpoint string, s *mcpbridge.Session, steered bool, wait time.Duration) sandbox.Opening {
	opening := sandbox.Opening{Servers: bridgeServers(endpoint, s.Surface, wait)}
	if steered {
		opening.Steer = sandbox.SteerWiring{Server: config.ToolServerName,
			Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)}
	}
	if len(s.Skills) > 0 {
		opening.SkillLoads = sandbox.SkillWiring{Server: config.ToolServerName,
			Tool: mcpbridge.SkillTool, URL: mcpbridge.SkillURL(endpoint)}
	}
	return opening
}

// withRunsOwn is out with what req says of the run itself beside the launch's
// decisions: an LLM fleet's decision record of the member it took, its own
// context sources, which a launch charges an OpenCode
// continuation in the box that holds its copy (internal/sandbox's RunSources),
// what each server the run's bridge serves says it is for and offers; the leads its fresh
// brief opened with, which the launch keeps for its job; and the read its
// briefs were built from ([sandbox.LaunchRequest.BriefRead]). Set beside
// [agentLauncher.launch]'s literal rather than in it, since that literal is the
// launch's decisions (member, cell, reach, environment, login), which
// crewlet-operator mirrors, and none of these is one.
func withRunsOwn(req runner.AgentRunRequest, pick *fleet.Pick, out sandbox.LaunchRequest) sandbox.LaunchRequest {
	if pick != nil {
		out.Fleet = runner.FleetDecision([]*fleet.Pick{pick}, pick.Key)
	}
	out.MCPGuidance = mcpbridge.Guidance(req.Surface)
	out.MCPCatalogs = mcpbridge.Catalogs(req.Surface)
	out.FreshOwn = req.FreshOwn
	out.BriefRead = req.BriefRead
	return out
}

// sandboxContinuation is the runner's carry-on of an asking run in the sandbox
// layer's terms, nil for every other launch.
func sandboxContinuation(c *runner.Continuation) *sandbox.Continuation {
	if c == nil {
		return nil
	}
	out := &sandbox.Continuation{SessionID: c.SessionID, Brief: c.Brief, Told: c.Told, Fresh: c.Fresh}
	if c.Summarized != "" {
		out.Summarized, out.SummarizedTold = c.Summarized, c.SummarizedTold
		out.Count, out.CountProvider = c.Count, c.CountProvider
	}
	switch {
	case c.SignInReplaced:
		out.Reason = sandbox.FollowUpSignInReplaced
	case c.RequestRefused:
		out.Reason = sandbox.FollowUpRequestRefused
	}
	return out
}

// member is the executor-chain member this launch runs on: the first that can
// take a run now, picked at every launch from the seat's executor walk
// ([phase.Registry.Walk]), a pinned chain in order or an LLM fleet's picks, whose
// pick is taken and returned for the launch's decision record. A member that is
// no in-box-runner harness is passed over silently (config warns once); one
// sitting out its spent plan ([harness.Provider.SittingOut]) is handed off like
// a call's fallback ([chain.Pick]) through onFallback; all sitting out is a
// [*chain.Error], a chain at its limit ([chain.Limited]): the dispatcher parks
// the seat and a resume waits ([Dispatcher.Provider], [resumeRefusal];
// ADR-0064). A run that meets its limit midway is not relaunched elsewhere.
func (l *agentLauncher) member(ctx context.Context, manager *sandbox.Manager,
	onFallback func(chain.Fallback)) (_ chain.Member, _ *harness.Provider, _ *fleet.Pick, err error) {
	defer origin.Stamp(&err)
	walk, err := executorWalk(l.company, l.seat, manager)
	if err != nil {
		return chain.Member{}, nil, nil, err
	}
	pickOf := func(key string) *fleet.Pick {
		if i := slices.IndexFunc(walk.Members, func(m chain.Member) bool { return m.Key == key }); i >= 0 &&
			i < len(walk.Picks) {
			return walk.Picks[i]
		}
		return nil
	}
	picked, err := chain.Pick(ctx, walk.Members, func(m chain.Member) error {
		return m.Provider.(*harness.Provider).SittingOut()
	}, func(f chain.Fallback) {
		f.FromFleet, f.ToFleet = pickOf(f.From), pickOf(f.To)
		onFallback(f)
	})
	var exhausted *chain.Error
	if errors.As(err, &exhausted) && len(walk.Picks) > 0 {
		exhausted.Fleet = walk.Picks[0].Fleet
	}
	if err != nil {
		return chain.Member{}, nil, nil, err
	}
	pick := pickOf(picked.Key)
	if pick != nil {
		pick.Take()
	}
	return picked, picked.Provider.(*harness.Provider), pick, nil
}

// executorRunners is seat's executor chain as a launch can take it: every
// member, in chain order, that is a harness with an in-box runner in
// manager — the members [agentLauncher.member] picks among, and so every
// member whose cell a run of the seat can land in.
func executorRunners(c *Company, seat *org.Role, manager *sandbox.Manager) ([]chain.Member, error) {
	walk, err := executorWalk(c, seat, manager)
	return walk.Members, err
}

// executorWalk is [executorRunners] with an LLM fleet's picks of them, index
// for index, nil on a pinned chain.
//
// Refused when there is none. Config refuses this chain on every write (it
// needs a member on a harness with a runner), so reaching it is a company built by hand
// or a catalogue built without the runners: either way the operator's to fix,
// and a retry changes nothing, which is the class it carries
// ([sandbox.WhyConfiguration]). So does a company with no providers.llm entry
// at all, though no launch meets it, since such a company builds no launcher
// ([Engine.agentRunFor]): what clears it is a person's edit too. A fleet with
// no member is its [*phase.EmptyFleetError], which holds the seat.
func executorWalk(c *Company, seat *org.Role, manager *sandbox.Manager) (_ phase.Walk, err error) {
	defer origin.Stamp(&err)
	if c.Models == nil {
		return phase.Walk{}, sandbox.Because(sandbox.WhyConfiguration, fmt.Errorf(
			"seat %q cannot launch its executor: %w", seat.Handle(), phase.ErrNoProviders))
	}
	walk, err := c.Models.Walk(seat, phase.Execute)
	if err != nil {
		return phase.Walk{}, fmt.Errorf("seat %q's executor chain: %w", seat.Handle(), err)
	}
	var out phase.Walk
	for i, m := range walk.Members {
		agent, isHarness := m.Provider.(*harness.Provider)
		if !isHarness {
			continue
		}
		if _, err := manager.RunnerFor(agent.Harness()); err != nil {
			continue
		}
		out.Members = append(out.Members, m)
		if len(walk.Picks) > 0 {
			out.Picks = append(out.Picks, walk.Picks[i])
		}
	}
	if len(out.Members) == 0 {
		keys := make([]string, len(walk.Members))
		for i, m := range walk.Members {
			keys[i] = m.Key
		}
		return phase.Walk{}, sandbox.Because(sandbox.WhyConfiguration, fmt.Errorf("seat %q's executor "+
			"chain (%s) has no member whose harness runs in a box: point the seat's llm at an "+
			"entry on %s", seat.Handle(), words.And(keys), words.Or(executor.Names())))
	}
	return out, nil
}

// executorHeadroom refuses a launch below turn_engine.executor_min_budget_tokens.
//
// Every launch and relaunch: a run's first request alone carries the harness's
// prompt and tool definitions, so a seat with less room spends a box to be
// refused at its first call. Three-valued: no counter is uncapped, below the
// floor is refused, and a failed read is too (an unknown budget is spent past).
// Below the floor is a budget refusal ([toolloop.ErrBudgetExhausted]), so it
// waits: a dispatch parks the seat via the budget stage ([meter.starved])
// rather than redeliver into the same refusal; a resumed segment has no
// delivery to park and is held on the window ([Engine.budgetHold],
// [sandbox.ErrResumeHeld]). An unreadable counter is retried by backoff.
func executorHeadroom(ctx context.Context, remaining runner.Remaining, floor launchFloor) (err error) {
	defer origin.Stamp(&err)
	if floor.needs() <= 0 || remaining == nil {
		// No floor, or no counter anywhere in the epoch — a company with
		// no token budget rather than a read that could not be made.
		return nil
	}
	left, err := remaining.Remaining(ctx)
	if err != nil {
		return fmt.Errorf("this seat's remaining token budget could not be read, and an "+
			"executor run costs a box before it produces anything: %w", err)
	}
	if left < floor.needs() {
		// THE FLOOR RIDES THE REFUSAL, so the hold that waits on it waits
		// for room for this launch ([floorRefusal]).
		return &floorRefusal{floor: floor, left: left}
	}
	return nil
}

// bridgeServers is the engine-side surface as entries of a box's MCP server
// list, a map of the job's own: an endpoint written into the seat's rendered
// map, which the epoch owns, would follow it into its next run, where the
// token names a job that is over.
//
// The engine's tools sit under the reserved name config refuses to any
// mcp_servers entry ([config.ToolServerName]), so they never shadow a seat's
// server, and are always loaded ([sandbox.MCPServer.AlwaysLoaded]). Each of the
// seat's servers the surface offers a tool of follows under its own name at
// its bridge route ([mcpbridge.ServerURL], [mcpbridge.Servers]). Every call
// waits as long as wait ([mcpbridge.Bridge.CallTimeout]).
func bridgeServers(endpoint string, surface mcpbridge.Surface, wait time.Duration) map[string]sandbox.MCPServer {
	out := map[string]sandbox.MCPServer{config.ToolServerName: {
		Name: config.ToolServerName, Transport: sandbox.TransportHTTP, URL: endpoint,
		AlwaysLoaded: true, CallTimeout: wait,
	}}
	for _, name := range mcpbridge.Servers(surface) {
		out[name] = sandbox.MCPServer{Name: name, Transport: sandbox.TransportHTTP,
			URL: mcpbridge.ServerURL(endpoint, name), CallTimeout: wait}
	}
	return out
}

// agentRunFor is THE construction path for a turn's executor launcher, over the
// turn's pinned company.
//
// Nil only where there is nothing to launch for (no turn, agent seat or
// models); the runner refuses to be built without one. It returns the
// interface so a nil is never a typed nil.
//
// conversation and delivery are set for a dispatched segment (its first launch
// continues and freezes the conversation), notes and relaunchRefusedIn for a
// resumed one; nil otherwise. See [agentLauncher.delivery],
// [agentLauncher.notes] and [agentLauncher.relaunchRefusedIn].
func (e *Engine) agentRunFor(company *Company, handle string, t *turnctx.Turn,
	ask *sandbox.ColleagueAsk, conversation *conversationRead, told *toldTurn, delivery []uuid.UUID,
	notes *rowSource, relaunchRefusedIn *sandbox.BudgetWindow,
) runner.AgentLauncher {
	if t == nil || company == nil || company.Models == nil {
		return nil
	}
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	return &agentLauncher{engine: e, turn: t, seat: seat, company: company, ask: ask,
		conversation: conversation, told: told, delivery: delivery, notes: notes,
		relaunchRefusedIn: relaunchRefusedIn}
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

// memberCell is the cell a run of the chain member key lands in, resolved as
// the manager resolves it: the member's own run_in, else the catalogue's
// default. The launch's cell ([agentLauncher.launch]) and the cell a relaunch
// target names ([Company.relaunchTarget]) alike, so a reader's word on whether
// a relaunch continues in the box a run parked in is the launch's.
func memberCell(c *Company, manager *sandbox.Manager, key string) sandbox.Placement {
	if cell := sandbox.Placement(agentPlacement(c, key)); cell != "" {
		return cell
	}
	return manager.DefaultPlacement()
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
	return sandboxTurnRef(ctx, l.turn, l.seat.Name, l.ask)
}
