package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// reopenAgentRun serves an executor run's tool bridge again on this node, which
// has just taken the run over while it works: the coordinator's
// [sandbox.CoordinatorOptions.Recovered], as this node recovers the run's
// seat and before the seat's mailbox opens.
//
// # Why a recovered run needs it
//
// The box keeps calling the seat's tools for as long as its harness runs, over a
// session that lived in the process that launched it. That process can
// restart, or lose the seat to a peer, while the box is still working — and
// then every bridged call was refused and the rest of the run was done blind:
// no colleague, no channel, no ticket, no submission, and a turn rescued as
// incomplete for a box that may well have finished the work.
//
// # What is opened, and from what
//
// The run's JOB's session, under the job's own name — the launch id on the
// run's durable row — so the token the box already holds, which names the run
// and the job and is signed under the fleet's keyring, resolves here (see
// internal/api/mcpbridge). Its surface is rebuilt from the run's own state by
// the runner ([runner.Runner.ReopenAgentRun]), on a runner built exactly as the
// turn's resume builds one ([Engine.parkedTurnRunner]); its ledger records
// under the same job, from the spend the row already holds; and it is fenced
// on the seat as THIS node holds it now.
//
// # How the box reaches it here
//
// The box dials the fleet's bridge address, whichever node answers it — or,
// for a direct box, the launching node's own listener on loopback — and the
// node it reaches relays the call to this one by the seat's lease
// ([mcpbridge.Bridge.Join]) — so a run this node took over from a peer is
// served here as surely as one it launched.
//
// # Not a run whose ending is decided, nor one a person stopped
//
// A run a person stopped, or one being failed, has its ending recorded on its
// row before its steps run ([sandbox.PendingRun.Ending]), and those steps close
// its session and reclaim its box. Reopened, the box would get its tools back
// for the moments before its reclaim, as a seat whose stop said otherwise.
//
// The same holds a step earlier, for a run on a seat whose pause asks for its
// running work to stop and whose stop has not been decided yet: the seat's
// acquisition ends it right after this recovery ([Engine.prepareSeatSandbox]),
// so a session opened here would serve the seat's tools to a run the person
// asked to end — every call refused by the session's fence, which reads the
// same pause, but the box's handshake answered and its surface rebuilt for
// nothing. A pause this node has not read yet is no stop: the session is
// opened, and the stop that arrives with the pauses closes it.
//
// # Nor one whose job was lost with its host
//
// A run whose job was lost — killed with its engine by a restart, its box gone
// from its host, or its launch node gone from the fleet — carries that verdict
// on its row ([sandbox.PendingRun.Lost]): nothing in a box is left to call
// anything, and the completion announced for it carries its round on in a job
// of its own, which opens a session of its own, the moment the seat's mailbox
// opens. A session opened for the lost job would serve nobody.
//
// NEVER AN ERROR to the recovery that calls it: the seat is this node's
// whatever becomes of one run's bridge, and the run is still collected when it
// ends. What could not be opened is said, with why.
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
		Turn: resumedTurn(run, seat, company.Org),
	})
	if err != nil {
		refuse("the run's turn could not be rebuilt here", "error", err.Error())
		return
	}
	r, err := company.RunnerFor(handle, e.seatRegistry(company, handle), input)
	if err != nil {
		refuse("the run's turn could not be rebuilt here", "error", err.Error())
		return
	}
	surface, meter, err := r.ReopenAgentRun(ctx, state, bridgedCalls(run.BridgeCalls),
		bridgedOf(run.Launch.Bridged))
	if err != nil {
		refuse("the run's tool surface could not be rebuilt", "error", err.Error())
		return
	}
	ledger := newBridgeLedger(rt.pending, meter)
	ledger.nudge = e.nudgeRun
	// THE RUN'S CALLS GO ON IN ITS OWN ORDER: the session numbers its calls
	// after the last one the row records, and a resume reads them by it.
	job := sandbox.OpenedJob{LaunchID: run.LaunchID, LastSeq: run.LastBridgeSeq()}
	session := &mcpbridge.Session{
		RunID: run.TurnID, Handle: handle, Role: seat.Name,
		Surface: surface, Ledger: ledger,
		// AS THE LAUNCH OPENED IT, so the opening is refused or made on the
		// same terms; the box already holds its endpoint and is never handed
		// the one this opening mints.
		OnHost: sandbox.Placement(run.Placement) == sandbox.Direct,
		// THE SEAT'S FENCE AS THIS NODE HOLDS IT NOW: the grant the launch
		// was fenced on is another node's, or this one's before a restart.
		Fence: e.seatFence(handle),
	}
	// AND THE RUN'S NOTE INBOX, built from its record alone — its turn, its
	// job and its round — so a note to the turn reaches the run between its
	// steps here as it did on the node that launched it (runsteer.go).
	inbox := e.runSteers.inbox(identityOfRun(run), state.Round)
	if inbox != nil {
		session.Steer = inbox
	}
	if _, err := e.openBridge(ctx, session, ledger, inbox, job); err != nil {
		inbox.Close()
		refuse("the bridge opened no session for the run's job", "error", err.Error())
		return
	}
	log.InfoContext(ctx, "bridge_reopened", "run_id", run.TurnID, "launch_id", run.LaunchID,
		"seat", handle, "calls_before", len(run.BridgeCalls))
}
