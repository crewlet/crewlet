package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/executordomain"
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
// surface and briefs ([agentLauncher.ProbeSession]): the turn's newest kept
// capture, where the runner of the member the launch would pick restores it and
// the seat's budget can ever carry it; and the launch's floor is made of what it
// continues ([launchFloor]).

// executorRestore is the engine's part of continuing a turn's kept session
// ([sandbox.SessionRestore]).
type executorRestore struct{ engine *Engine }

var _ sandbox.SessionRestore = executorRestore{}

// Session reads one turn's row through the estate router's reads.
func (r executorRestore) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {

	if r.engine.router == nil {
		return executordomain.Session{}, errors.New("this node has no estate router to read the " +
			"executor log through")
	}
	return r.engine.router.ExecutorReads().Session(ctx, handle, turn, fresh)
}

// Fetch copies the sealed object row names into dst.
func (r executorRestore) Fetch(ctx context.Context, row executordomain.Session, dst io.Writer) error {
	store, err := executorKeep{engine: r.engine}.sessionStore()
	if err != nil {
		return err
	}
	return store.Fetch(ctx, row, dst)
}

// OpenSpooled opens a spool Fetch wrote.
func (r executorRestore) OpenSpooled(row executordomain.Session, sealed io.Reader) (io.Reader, error) {
	store, err := executorKeep{engine: r.engine}.sessionStore()
	if err != nil {
		return nil, err
	}
	return store.OpenSpooled(row, sealed)
}

// ProbeSession answers which kept session of the turn a relaunch continues
// ([runner.SessionProber]): the turn's newest kept capture, and for a relaunch
// that carries its round on one its round took
// ([sandbox.Coordinator.ContinuedCapture]), held to what this launch can take
// up — the runner of the member it would pick restores it, its context is
// within the window a session is carried at ([sandbox.ExecutorContextWindow]),
// and its floor fits a window the seat's budget can ever leave ([launchFloor.fits])
// — or why it continues none, which is logged.
//
// THE MEMBER IS THE ONE THE LAUNCH WOULD PICK, without the hand-offs the pick
// announces: the first of the chain's runner members not sitting out. A member
// that starts sitting out between this and the launch is the launch's to pass
// over, and a session its runner cannot restore is started fresh by the launch.
func (l *agentLauncher) ProbeSession(ctx context.Context, briefTokens int, carries bool) runner.SessionBasis {
	basis := l.probeSession(ctx, briefTokens, carries)
	if !basis.Continues() {
		log.InfoContext(ctx, "executor_session_not_continued", "seat", l.seat.Handle(),
			"turn_id", l.turn.RunID, "detail", basis.Why,
			"outcome", "the relaunch starts a session of its own, or continues the one its box holds")
	}
	return basis
}

// probeSession is [agentLauncher.ProbeSession] before it logs.
func (l *agentLauncher) probeSession(ctx context.Context, briefTokens int, carries bool) runner.SessionBasis {
	e, company, handle := l.engine, l.company, l.seat.Handle()
	rt := e.sandbox.Load()
	manager := company.catalogue
	if rt == nil || rt.coordinator == nil || manager == nil {
		return runner.SessionBasis{Why: "this node has no sandbox runtime for this company"}
	}
	restorer, why := l.restorer(manager)
	if why != "" {
		return runner.SessionBasis{Why: why}
	}
	continued, why := rt.coordinator.ContinuedCapture(ctx, l.turn.RunID, restorer, carries)
	if why != "" {
		return runner.SessionBasis{Why: why}
	}
	captured := continued.Capture
	if captured.ContextTokens > sandbox.ExecutorContextWindow {
		return runner.SessionBasis{Oversized: true, Why: fmt.Sprintf("the session's last request carried "+
			"%d tokens, past the %d a session is carried at", captured.ContextTokens, sandbox.ExecutorContextWindow)}
	}
	floor := staticFloor(company).continuing(captured.ContextTokens, briefTokens)
	if m := e.meterFor(company, handle); m != nil {
		if why := floor.fits(m.basis); why != "" {
			return runner.SessionBasis{Oversized: true, Why: why}
		}
	}
	return runner.SessionBasis{
		LaunchSeq: captured.LaunchSeq, CaptureSeq: captured.CaptureSeq, SessionID: continued.Row.SessionID,
		BridgeSeq: captured.BridgeSeq, NoteCursor: captured.NoteCursor, Interrupted: captured.Interrupted,
		Home: captured.Home, ContextTokens: captured.ContextTokens,
	}
}

// restorer is the runner the launch would take the turn's session up with: the
// in-box runner of the first member of the seat's executor chain that is not
// sitting out its plan — or why there is none.
func (l *agentLauncher) restorer(manager *sandbox.Manager) (sandbox.SessionRestorer, string) {
	members, err := executorRunners(l.company, l.seat, manager)
	if err != nil {
		return nil, err.Error()
	}
	for _, m := range members {
		agent, ok := m.Provider.(*harness.Provider)
		if !ok || agent.SittingOut() != nil {
			continue
		}
		run, err := manager.RunnerFor(agent.Harness())
		if err != nil {
			return nil, err.Error()
		}
		restorer, ok := run.(sandbox.SessionRestorer)
		if !ok {
			return nil, agent.Harness() + "'s runner continues no session carried between boxes"
		}
		return restorer, ""
	}
	return nil, "every member of the seat's executor chain is sitting out its plan"
}

// sandboxSession is a relaunch's kept session in the sandbox layer's terms, nil
// where it continues none.
func sandboxSession(s *runner.SessionContinuation) *sandbox.SessionContinuation {
	if s == nil || !s.Basis.Continues() {
		return nil
	}
	return &sandbox.SessionContinuation{LaunchSeq: s.Basis.LaunchSeq, CaptureSeq: s.Basis.CaptureSeq,
		Brief: s.Brief, Replaced: s.Replaced}
}
