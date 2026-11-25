package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// reopenAgentRun serves a recovered run's tool bridge again on this node, which
// took the run over while its box works ([sandbox.CoordinatorOptions.Recovered],
// before the seat's mailbox opens); else every bridged call is refused.
//
// It opens the JOB's session under the launch id on the run's row, so the token
// the box holds resolves here (internal/api/mcpbridge), its surface rebuilt by
// [runner.Runner.ReopenAgentRun] on a runner built as the resume builds one
// ([Engine.parkedTurnRunner]) and fenced on the seat as THIS node holds it.
// Not reopened: a run with an ending decided ([sandbox.PendingRun.Ending]) or
// on a seat whose pause asks its work to stop ([Engine.prepareSeatSandbox]),
// whose steps close the session (an unread pause is no stop); nor a lost job
// ([sandbox.PendingRun.Lost]). NEVER AN ERROR: what failed to open is said.
func (e *Engine) reopenAgentRun(ctx context.Context, run sandbox.PendingRun) {
	if run.Ending != nil || run.Lost != nil {
		return
	}
	if seat, err := e.seatID(run.AgentHandle); err == nil && e.stopFor(seat, run.AgentHandle) != nil {
		log.InfoContext(ctx, "bridge_not_reopened", "run_id", run.TurnID, "launch_id", run.LaunchID,
			"seat", run.AgentHandle, "detail", "a person paused the seat and asked for its running "+
				"work to stop, so the run is ended rather than served its tools again")
		return
	}
	state, ok, err := execstate.Decode(run.ExecuteState)
	if err != nil {
		log.WarnContext(ctx, "bridge_not_reopened", "run_id", run.TurnID, "seat", run.AgentHandle,
			"error", err.Error(), "detail", "the run's parked state is one this build cannot "+
				"read, so its tool surface cannot be rebuilt here; a node on the build "+
				"that wrote it serves it when it holds the seat")
		return
	}
	if !ok {
		// No park on the row: the launch's turn was lost before it parked,
		// and the recovery that fails the run answers it.
		return
	}
	refuse := func(detail string, args ...any) {
		log.WarnContext(ctx, "bridge_not_reopened", append([]any{
			"run_id", run.TurnID, "launch_id", run.LaunchID, "seat", run.AgentHandle,
			"detail", detail + "; the box's tool calls are refused until the run ends, " +
				"and the run is collected when it does"}, args...)...)
	}
	switch {
	case e.bridge == nil:
		refuse("this node serves no tool bridge: " + mcpbridge.BaseURLVar + " is unset here, " +
			"and this node serves no bridge route of its own")
		return
	case !e.bridge.Reachable():
		refuse("nothing reaches this node's tool bridge: " + mcpbridge.BaseURLVar + " is set but " +
			"this node serves the API with api.port 0, so it binds no listener for the bridge's route")
		return
	}
	rt := e.sandbox.Load()
	company := e.Company()
	if rt == nil || company == nil {
		refuse("this node has no sandbox runtime or no applied company to serve the run in")
		return
	}
	handle := run.AgentHandle
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		refuse("the run's seat is not in this node's company")
		return
	}

	// THE RUN'S CONTEXT, as its launch had it: its trace, and the seat a
	// model call made inside one of its calls — a delegated worker's — runs
	// its harness as.
	ctx = parkedRunContext(ctx, run)

	input, _, err := e.parkedTurnRunner(ctx, company, resumeInput{
		Company: company, Run: run, State: state,
		Turn: resumedTurn(run, seat, company.Org, e.rowHolds(run)),
	}, nil)
	if err != nil {
		refuse("the run's turn could not be rebuilt here", "error", err.Error())
		return
	}
	r, err := company.RunnerFor(ctx, handle, e.seatRegistry(company, handle), input)
	if err != nil {
		refuse("the run's turn could not be rebuilt here", "error", err.Error())
		return
	}
	// THE RUN'S CALLS SO FAR, off the log beside its row: what the reopened
	// surface has executed, and where the session numbers the next call. A
	// log that cannot be read refuses the opening rather than reopening a
	// run that seems to have called nothing.
	calls, err := rt.pending.BridgeLog(ctx, run)
	if err != nil {
		refuse("the run's bridged tool log could not be read", "error", err.Error())
		return
	}
	// ITS GUARD THE SESSION'S ITS BOX STARTED, as the job's record says the box
	// said it ([sandbox.LaunchRecord.SessionStart]): a box that has not said
	// yet says it on its first request here, which the reopened run is told.
	//
	// AND THE CALLS STILL IN FLIGHT, each by the identity its entry carries
	// ([sandbox.BridgeLog.InFlight]): a call the node this run is taken from
	// still runs, whose update has not landed, is counted by the reopened
	// call log, so no call made here derives its operation id.
	reopened, err := r.ReopenAgentRun(ctx, state, bridgedCalls(calls.Calls), unfinishedCalls(calls.InFlight),
		identitiesOf(calls.Unlisted()), identitiesOf(calls.Absorbed()),
		bridgedOf(run.Launch.Bridged), run.Launch.SessionStart == sandbox.SessionStartContinued)
	if err != nil {
		refuse("the run's tool surface could not be rebuilt", "error", err.Error())
		return
	}
	ledger := newBridgeLedger(rt.pending, reopened.Meter)
	ledger.nudge = e.nudgeRun
	// THE RUN'S CALLS GO ON IN ITS OWN ORDER: the session numbers its calls
	// after the last one the log records, a call still in flight there among
	// them ([sandbox.BridgeLog.LastSeq]), and a resume reads them by it. And
	// IN ITS OWN ROUND, as the row names the job: a call still running when a
	// relaunch carries the round on is listed in the round's log, marked late.
	// AND EACH CALL'S ID READ UNDER THE KEY THIS NODE'S RUNNER OF THE JOB'S
	// HARNESS DECLARES ([sandbox.OpenedJob.CallIDMeta]), each answered before
	// the ceiling it states ([sandbox.OpenedJob.CallCeiling]): the bridge
	// reading the box's calls from here on is this build's, whatever the
	// launch's was, and the row records neither.
	manager := rt.coordinator.Manager()
	job := run.NamedJob(calls.LastSeq(), manager.CallIDMeta(run.Harness), manager.MCPCallCeiling(run.Harness))
	session := &mcpbridge.Session{
		RunID: run.TurnID, Handle: handle, Role: seat.Name,
		Surface: reopened.Surface, Ledger: ledger,
		// THE MODEL THE LAUNCH POINTED THE RUN'S CLI AT, as its row records
		// it: the session the launch opened carried it ([mcpbridge.Session.Model]),
		// and a session opened again without it would stamp every call the
		// box makes from here on with no model, so a code write the run pushes
		// after its seat moved named nobody's model.
		Model: run.Launch.Model,
		// AS THE LAUNCH OPENED IT, so the opening is refused or made on the
		// same terms; the box already holds its endpoint and is never handed
		// the one this opening mints.
		OnHost: sandbox.Placement(run.Placement) == sandbox.Direct,
		// THE SEAT'S FENCE AS THIS NODE HOLDS IT NOW: the grant the launch
		// was fenced on is another node's, or this one's before a restart.
		Fence: e.seatFence(handle),
		// THE SKILLS ITS HARNESS WAS HANDED AS ITS OWN, by the names its
		// launch handed them under, which its box still lists: a load it
		// reports here is read as the launch's opening read one.
		Skills: state.NativeSkills,
		// AND THE SESSION ITS BOX STARTED, said again by the token it dials
		// on its first request here — the same token it dialled the node it
		// was taken from with ([reopenedStarted]).
		Started: reopenedStarted(ledger, run, reopened.Started),
		// AND THAT ITS HARNESS CONNECTED ITS BRIDGE, where its first list
		// reaches this node: once per job, so one recorded already stands.
		Connected: func(ctx context.Context) { ledger.connected(ctx, run.TurnID) },
	}
	// AND THE RUN'S NOTE INBOX, built from its record alone — its turn, its
	// job and its round — so a note to the turn reaches the run between its
	// steps here as it did on the node that launched it (runsteer.go). BOUND
	// TO THE JOB BEFORE THE BRIDGE FILES THE SESSION ([Engine.openBridge]): the
	// box reaches the opening the moment it is filed, with the token it holds,
	// and never finds the inbox serving no job.
	inbox := e.runSteers.inbox(identityOfRun(run), state.Round, run.Harness)
	if inbox != nil {
		session.Steer = inbox
	}
	if _, err := e.openBridge(ctx, session, ledger, inbox, job); err != nil {
		// CLOSED, which stops the view its bind started.
		inbox.Close()
		refuse("the bridge opened no session for the run's job", "error", err.Error())
		return
	}
	log.InfoContext(ctx, "bridge_reopened", "run_id", run.TurnID, "launch_id", run.LaunchID,
		"seat", handle, "calls_before", len(calls.Calls))
}

// reopenedStarted is what the bridge session of run, taken over while it works,
// does once the run's box says which session it started there ([boxStarted]):
// it holds the reopened run's guard to that session through started
// ([runner.ReopenedRun.Started]) and records it on the job's record — for a job
// whose launch continued a session, AS THE JOB'S RECORD NAMES ITS BRIEF
// ([sandbox.LaunchRecord.Brief]), the one word on it this node has of what the
// launch on another node, or before a restart, decided.
func reopenedStarted(ledger *bridgeLedger, run sandbox.PendingRun,
	started func(continued bool)) func(context.Context, bool) {

	continues := run.Launch.Brief.Continues()
	return boxStarted(ledger, run.TurnID, func() bool { return continues }, started)
}
