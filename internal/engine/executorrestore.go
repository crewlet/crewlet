package engine

import (
	"context"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN'S KEPT SESSION, CONTINUED BY ITS NEXT LAUNCH, as the engine wires it
// (internal/sandbox's restore.go has the mechanism).
//
// THE TURN'S ROW IS READ THROUGH THE ESTATE ROUTER'S READS, wherever the seat
// runs: a seats-only node holds no log, and the capture it continues may be one
// another node's keep recorded, which a read at `linearizable` waits for or asks
// a data node for. The session's sealed object is fetched and opened through
// the domain's session store, over the Tier A keyring, on whichever node
// launches.
//
// WHICH SESSION A RELAUNCH CONTINUES is asked before its runner builds the run's
// surface and briefs ([agentLauncher.ProbeSession]): the kept capture of the job
// the relaunch follows, where the runner of the member the launch would pick
// restores it. WHETHER THE SEAT'S BUDGET CAN CARRY IT is the launch's to decide,
// once the briefs that continue it exist ([agentLauncher.launch], [launchFloor]):
// the context the session sends first is known here, and the brief beside it
// only once the runner has written it.

// executorRestore is the engine's part of continuing a turn's kept session
// ([sandbox.SessionRestore]).
type executorRestore struct{ engine *Engine }

var _ sandbox.SessionRestore = executorRestore{}

// Session reads one turn's row through the estate router's reads. Where it was
// read is the read surface's to state, not a restore's: a point read never
// answers a row a record it could not decode may be missing from, since such a
// record covering the turn refuses the read instead.
func (r executorRestore) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {

	if r.engine.router == nil {
		return executordomain.Session{}, errors.New("this node has no estate router to read the " +
			"executor log through")
	}
	read, err := r.engine.router.ExecutorReads().Session(ctx, handle, turn, fresh)
	return read.Session, err
}

// Fetch copies the sealed object row names into dst.
func (r executorRestore) Fetch(ctx context.Context, row executordomain.Session, dst io.Writer) error {
	store, err := r.engine.executorSessionStore()
	if err != nil {
		return err
	}
	return store.Fetch(ctx, row, dst)
}

// OpenSpooled opens a spool Fetch wrote.
func (r executorRestore) OpenSpooled(row executordomain.Session, sealed io.Reader) (io.Reader, error) {
	store, err := r.engine.executorSessionStore()
	if err != nil {
		return nil, err
	}
	return store.OpenSpooled(row, sealed)
}

// ProbeSession answers which kept session of the turn a relaunch continues
// ([runner.SessionProber]): the kept capture of the job the relaunch follows,
// as [sandbox.Coordinator.ContinuedCapture] decides for the runner of the
// member the launch would pick — or why it continues none, which is logged —
// and either way what that job's session's last request carried, which the
// launch's floor is made of.
//
// THE MEMBER IS THE ONE THE LAUNCH WOULD PICK, without the hand-offs the pick
// announces: the first of the chain's runner members not sitting out. A member
// that starts sitting out between this and the launch is the launch's to pass
// over, and a session its runner cannot restore is started fresh by the launch.
func (l *agentLauncher) ProbeSession(ctx context.Context) runner.SessionBasis {
	basis := l.probeSession(ctx)
	if !basis.Continues() {
		log.InfoContext(ctx, "executor_session_not_continued", "seat", l.seat.Handle(),
			"turn_id", l.turn.RunID, "detail", basis.Why,
			"outcome", "the relaunch starts a session of its own, or continues the one its box holds")
	}
	return basis
}

// probeSession is [agentLauncher.ProbeSession] before it logs.
func (l *agentLauncher) probeSession(ctx context.Context) runner.SessionBasis {
	rt := l.engine.sandbox.Load()
	manager := l.company.catalogue
	if rt == nil || rt.coordinator == nil || manager == nil {
		return runner.SessionBasis{Why: "this node has no sandbox runtime for this company"}
	}
	target := l.company.relaunchTarget(l.seat, manager)
	if target.Restorer == nil {
		return runner.SessionBasis{Why: target.Why}
	}
	continued, why := rt.coordinator.ContinuedCapture(ctx, l.turn.RunID, target.Restorer)
	if why != "" {
		return runner.SessionBasis{Why: why, ContextTokens: continued.Context}
	}
	captured := continued.Capture
	return runner.SessionBasis{
		LaunchSeq: captured.LaunchSeq, CaptureSeq: captured.CaptureSeq, SessionID: continued.Row.SessionID,
		BridgeSeq: captured.BridgeSeq, BridgeOpen: captured.BridgeOpen, NoteCursor: captured.NoteCursor,
		Interrupted: captured.Interrupted, Home: captured.Home, ContextTokens: continued.Context,
	}
}

// relaunchTarget is what a relaunch of seat's turn would be launched on
// ([sandbox.RelaunchTarget]): the first member of the seat's executor chain
// with an in-box runner that is not sitting out its plan — the member the
// launch's own pick takes ([agentLauncher.member]), without the hand-offs the
// pick announces — the cell its run lands in, and its runner, as it takes up a
// session carried between boxes; or why there is none.
//
// THE ONE PICK, asked by the relaunch's probe of the session it continues
// ([agentLauncher.ProbeSession]) and by every reader of whether a relaunch
// would ([Engine.RelaunchTarget]), so a row of a seat's sessions is decided by
// the runner its relaunch would take it up with — never by the runner of the
// harness that wrote it, which a seat whose chain moved to another harness, or
// whose first member's plan is spent, would not launch on. A member that starts
// sitting out between this and the launch is the launch's to pass over, and a
// session its runner cannot restore is started fresh by the launch.
func (c *Company) relaunchTarget(seat *org.Role, manager *sandbox.Manager) sandbox.RelaunchTarget {
	members, err := executorRunners(c, seat, manager)
	if err != nil {
		return sandbox.RelaunchTarget{Why: err.Error()}
	}
	for _, m := range members {
		agent, ok := m.Provider.(*harness.Provider)
		if !ok || agent.SittingOut() != nil {
			continue
		}
		target := sandbox.RelaunchTarget{Harness: agent.Harness(), Placement: memberCell(c, manager, m.Key)}
		run, err := manager.RunnerFor(agent.Harness())
		if err != nil {
			target.Why = err.Error()
			return target
		}
		restorer, ok := run.(sandbox.SessionRestorer)
		if !ok {
			target.Why = agent.Harness() + "'s runner continues no session carried between boxes"
			return target
		}
		target.Restorer = restorer
		return target
	}
	return sandbox.RelaunchTarget{Waiting: true, Why: "every member of the seat's executor chain is " +
		"sitting out its plan, and which of them takes a relaunch decides what it continues"}
}

// RelaunchTarget is what a relaunch of the seat handle's turn would be launched
// on, as the company this node runs picks it ([Company.relaunchTarget]) — what
// the read surface hands [sandbox.ReadContinuityOf] for each row of a seat's
// sessions (internal/api/queries' `continuable`), so the row is decided by the
// member and the runner a relaunch would take. A node running no company, or
// none that reaches a sandbox cell, and a handle the company holds no seat
// under, have no member to say anything of, which the target says.
func (e *Engine) RelaunchTarget(handle string) sandbox.RelaunchTarget {
	company := e.Company()
	if company == nil || company.catalogue == nil || company.Org == nil {
		return sandbox.RelaunchTarget{Why: "this node runs no company whose seats launch in a box"}
	}
	seat := company.Org.Role(handle)
	if seat == nil {
		return sandbox.RelaunchTarget{Why: "the company this node runs holds no seat " + handle}
	}
	return company.relaunchTarget(seat, company.catalogue)
}

// sandboxSession is a relaunch's kept session in the sandbox layer's terms, nil
// where it continues none: the brief for the box the session was last worked
// in, and the one for any other, each where the sandbox layer hands it.
func sandboxSession(s *runner.SessionContinuation) *sandbox.SessionContinuation {
	if s == nil || !s.Basis.Continues() {
		return nil
	}
	return &sandbox.SessionContinuation{LaunchSeq: s.Basis.LaunchSeq, CaptureSeq: s.Basis.CaptureSeq,
		Brief: s.Brief, Replaced: s.Replaced}
}

// sessionDecision is how the launch decided its run takes up a session, in the
// runner's terms ([runner.SessionDecision]).
func sessionDecision(d sandbox.LaunchDecision) runner.SessionDecision {
	return runner.SessionDecision{Continues: d.Brief.Continues(), Kept: d.Kept,
		NewBox: d.Brief == sandbox.BriefContinuedNewBox}
}
