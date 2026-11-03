package engine

import (
	"context"
	"errors"
	"fmt"
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
// the relaunch follows, where the runner of a member of the seat's chain restores
// it. WHICH MEMBER TAKES IT, AND WHETHER THE SEAT'S BUDGET CAN CARRY IT, are the
// launch's to decide, once the briefs that continue it exist
// ([agentLauncher.launch], [launchFloor]): the member is picked once, as it
// launches, and the context the session sends first is known here, the brief
// beside it only once the runner has written it.
//
// WHAT A READER OF A SESSION IS TOLD A RELAUNCH WOULD DO is asked of the node
// holding the seat ([Engine.RelaunchTarget]): the member that node would launch
// a relaunch on now, as its own bench of spent plans has it.

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
// as [sandbox.Coordinator.ContinuedCapture] decides it for the seat's chain —
// where the runner of a member that could take the launch restores it — or why
// it continues none, which is logged; and either way what that job's session's
// last request carried, which the launch's floor is made of.
//
// ANY MEMBER OF THE CHAIN, never one picked here: the launch picks its member
// once, as it launches ([agentLauncher.member]), and a member's plan can be
// spent or come back between this and that pick. Asked for one member, the
// probe answered for a member the launch then passed over — holding the launch
// to the floor of a session it would start fresh, or writing no continuation
// for a session its member could have taken up. So the briefs that continue the
// session are written wherever some member restores it, and the launch hands
// them on only where the member it picked runs the harness that wrote the
// session ([SessionBasis.Harness], [agentLauncher.continuedFloor]).
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
	restores, err := l.company.chainRestores(l.seat, manager)
	switch {
	case err != nil:
		return runner.SessionBasis{Why: err.Error()}
	case len(restores) == 0:
		// NOTHING TO WAIT FOR: no member could take a session up, whatever
		// the job's capture comes to.
		return runner.SessionBasis{Why: "no member of the seat's executor chain continues a session " +
			"carried between boxes"}
	}
	continued, why := rt.coordinator.ContinuedCapture(ctx, l.turn.RunID, restores)
	if why != "" {
		return runner.SessionBasis{Why: why, ContextTokens: continued.Context}
	}
	captured := continued.Capture
	return runner.SessionBasis{
		LaunchSeq: captured.LaunchSeq, CaptureSeq: captured.CaptureSeq, SessionID: continued.Row.SessionID,
		Harness:   continued.Row.Harness,
		BridgeSeq: captured.BridgeSeq, BridgeOpen: captured.BridgeOpen, NoteCursor: captured.NoteCursor,
		Interrupted: captured.Interrupted, Home: captured.Home, ContextTokens: continued.Context,
	}
}

// chainRestores is whether some member of seat's executor chain that a launch
// can take restores a session ([sandbox.ArchiveRestorer]): the runner of each
// member with an in-box runner, sitting out its plan or not — which one takes
// the launch is the launch's pick, made as it launches.
func (c *Company) chainRestores(seat *org.Role, manager *sandbox.Manager) (chainRestorers, error) {
	members, err := executorRunners(c, seat, manager)
	if err != nil {
		return nil, err
	}
	var out chainRestorers
	for _, m := range members {
		agent, ok := m.Provider.(*harness.Provider)
		if !ok {
			continue
		}
		run, err := manager.RunnerFor(agent.Harness())
		if err != nil {
			continue
		}
		restorer, ok := run.(sandbox.ArchiveRestorer)
		if !ok {
			continue
		}
		out = append(out, harnessRestorer{harness: agent.Harness(), restorer: restorer})
	}
	return out, nil
}

// harnessRestorer is one member's runner as it restores a session, and the
// harness it drives.
type harnessRestorer struct {
	harness  string
	restorer sandbox.ArchiveRestorer
}

// chainRestorers is the runners of a seat's chain as one [sandbox.ArchiveRestorer]:
// an archive is restored where the runner of a member driving the harness that
// wrote it restores it — a session is continued only by its own harness — and
// the reason is that runner's where none does.
type chainRestorers []harnessRestorer

var _ sandbox.ArchiveRestorer = chainRestorers(nil)

// Restores answers [sandbox.ArchiveRestorer] for the chain.
func (c chainRestorers) Restores(archive sandbox.SessionArchive) (bool, string) {
	why := ""
	for _, r := range c {
		if r.harness != archive.Harness {
			continue
		}
		ok, refused := r.restorer.Restores(archive)
		if ok {
			return true, ""
		}
		why = refused
	}
	if why == "" {
		why = fmt.Sprintf("no member of the seat's executor chain runs %s, which wrote the session: a "+
			"session is continued only by the harness that wrote it", archive.Harness)
	}
	return false, why
}

// relaunchTarget is what a relaunch of seat's turn would be launched on now,
// as this node picks it ([sandbox.RelaunchTarget]): the first member of the
// seat's executor chain with an in-box runner that is not sitting out its plan
// on this node — the member the launch's own pick takes
// ([agentLauncher.member]), without the hand-offs the pick announces — the cell
// its run lands in, this node, and its runner as it restores a session carried
// between boxes; or why there is none.
//
// THIS NODE'S PICK, and so the one a reader of a relaunch is handed only where
// this node holds the seat: which members sit out is this node's own bench, and
// the relaunch is decided on the node holding the seat ([Engine.RelaunchTarget]
// asks that node).
func (c *Company) relaunchTarget(seat *org.Role, manager *sandbox.Manager, node string) sandbox.RelaunchTarget {
	members, err := executorRunners(c, seat, manager)
	if err != nil {
		return sandbox.RelaunchTarget{Node: node, Why: err.Error()}
	}
	for _, m := range members {
		agent, ok := m.Provider.(*harness.Provider)
		if !ok || agent.SittingOut() != nil {
			continue
		}
		target := sandbox.RelaunchTarget{Harness: agent.Harness(), Placement: memberCell(c, manager, m.Key),
			Node: node}
		run, err := manager.RunnerFor(agent.Harness())
		if err != nil {
			target.Why = err.Error()
			return target
		}
		restorer, ok := run.(sandbox.ArchiveRestorer)
		if !ok {
			target.Why = agent.Harness() + "'s runner continues no session carried between boxes"
			return target
		}
		target.Restorer = restorer
		return target
	}
	return sandbox.RelaunchTarget{Node: node, Undecided: true, Why: "every member of the seat's executor " +
		"chain is sitting out its plan on the node holding the seat, and which of them takes a relaunch " +
		"decides what it continues"}
}

// localRelaunchTarget is what a relaunch of the seat handle's turn would be
// launched on, as the company this node runs picks it now ([Company.relaunchTarget])
// — and, where the company keeps no session, that a relaunch continues no kept
// one, by the keep the coordinator's launch asks ([executorKeep.Keeps]). A node
// running no company, or none that reaches a sandbox cell, and a handle the
// company holds no seat under, have no member to say anything of, which the
// target says.
func (e *Engine) localRelaunchTarget(handle string) sandbox.RelaunchTarget {
	company := e.Company()
	if company == nil || company.catalogue == nil || company.Org == nil {
		return sandbox.RelaunchTarget{Node: e.id, Why: "the node holding the seat runs no company whose " +
			"seats launch in a box"}
	}
	seat := company.Org.Role(handle)
	if seat == nil {
		return sandbox.RelaunchTarget{Node: e.id, Why: "the company the node holding the seat runs holds " +
			"no seat " + handle}
	}
	target := company.relaunchTarget(seat, company.catalogue, e.id)
	if !(executorKeep{engine: e}).Keeps() {
		target.NoKept = sandbox.NoKeptSession
	}
	return target
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
