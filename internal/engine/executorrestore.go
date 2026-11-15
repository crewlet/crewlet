package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A SESSION, CONTINUED BY A TURN'S NEXT LAUNCH, as the engine wires it
// (internal/sandbox's restore.go has the mechanism): the turn's own kept
// session, by its relaunches, and the session its conversation's last turn
// left, by its first launch and by a relaunch of a turn that has kept none.
//
// THE ROWS ARE READ THROUGH THE ESTATE ROUTER'S READS, wherever the seat runs: a
// seats-only node holds no log, and the capture a relaunch continues may be one
// another node's keep recorded, which a read at `linearizable` waits for or asks
// a data node for. The session's sealed object is fetched and opened through
// the domain's session store, over the Tier A keyring, on whichever node
// launches.
//
// WHICH SESSION A LAUNCH CONTINUES is asked before its runner builds the run's
// surface and briefs ([agentLauncher.ProbeSession]): a turn's first launch, the
// head its dispatch read (conversation.go), answered with no read at all; a
// relaunch, the kept capture of the job it follows, else the base the turn's
// first launch froze where the turn has kept nothing — each where the runner of
// a member of the seat's chain restores it. WHICH MEMBER TAKES IT, AND WHETHER
// THE SEAT'S BUDGET CAN CARRY IT, are the launch's to decide, once the briefs
// that continue it exist ([agentLauncher.launch], [launchFloor]): the member is
// picked once, as it launches, and the context the session sends first is known
// here, the brief beside it only once the runner has written it.
//
// WHAT A READER OF A SESSION IS TOLD A RELAUNCH WOULD DO is asked of the node
// holding the seat ([Engine.RelaunchTargets]): the member that node would launch
// a relaunch on now, as its own bench of spent plans has it.

// executorRestore is the engine's part of continuing a turn's kept session
// ([sandbox.SessionRestore]).
type executorRestore struct{ engine *Engine }

var _ sandbox.SessionRestore = executorRestore{}

// Session reads one turn's row through the estate router's reads, at the level
// a relaunch reads it ([executordomain.RelaunchRead]), as the sandbox's view of
// it ([executordomain.Session.Kept]), the log's not-found as
// [sandbox.ErrTurnRowNotFound]. Where it was read is the read surface's
// to state, not a restore's: a point read never answers a row a record it
// could not decode may be missing from, since such a record covering the turn
// refuses the read instead.
func (r executorRestore) Session(ctx context.Context, handle, turn string) (executorkeep.TurnRow, error) {
	if r.engine.router == nil {
		return executorkeep.TurnRow{}, errors.New("this node has no estate router to read the " +
			"executor log through")
	}
	read, err := r.engine.router.ExecutorReads().Session(ctx, handle, turn, executordomain.RelaunchRead())
	return restoreRow(read.Session, err)
}

// restoreRow is a point read of a turn's row as the sandbox's restore takes it:
// the row made its view ([executordomain.Session.Kept]), and the log's
// not-found as [sandbox.ErrTurnRowNotFound] — IN THE SANDBOX'S WORDS, since it
// links no state log to compare against the log's own refusal.
func restoreRow(row executordomain.Session, err error) (executorkeep.TurnRow, error) {
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return executorkeep.TurnRow{}, fmt.Errorf("%w: %w", sandbox.ErrTurnRowNotFound, err)
	case err != nil:
		return executorkeep.TurnRow{}, err
	}
	return row.Kept(), nil
}

// Fetch copies the sealed object row names into dst.
func (r executorRestore) Fetch(ctx context.Context, row executorkeep.TurnRow, dst io.Writer) error {
	store, err := r.engine.executorSessionStore()
	if err != nil {
		return err
	}
	return store.Fetch(ctx, storedSession(row), dst)
}

// OpenSpooled opens a spool Fetch wrote.
func (r executorRestore) OpenSpooled(row executorkeep.TurnRow, sealed io.Reader) (io.Reader, error) {
	store, err := r.engine.executorSessionStore()
	if err != nil {
		return nil, err
	}
	return store.OpenSpooled(storedSession(row), sealed)
}

// storedSession is the row the session store fetches and opens by: whose turn
// it is, which names the seal's associated data, and the object it names. The
// store reads nothing else of a row.
func storedSession(row executorkeep.TurnRow) executordomain.Session {
	return executordomain.Session{Handle: row.Handle, Turn: row.Turn, Object: row.Object}
}

// ProbeSession answers which session a launch of the turn continues
// ([runner.SessionProber]): on the turn's FIRST launch, the head its dispatch
// read and the turn will freeze, answered from that read with no other
// ([conversationRead.basis]); on a relaunch, the kept capture of the job it
// follows, else the base the turn's first launch froze where the turn has kept
// nothing, as [sandbox.Coordinator.ContinuedCapture] decides it for the seat's
// chain — where the runner of a member that could take the launch restores it —
// or why it continues none, which is logged, but for a conversation's first
// turn, which had nothing to continue; and either way what the job before's
// session's last request carried, which the in-place floor is made of.
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
	// AND THE WINDOW EVERY PATH'S FIRST REQUEST IS JUDGED UNDER, on every basis
	// whatever branch answered it: the in-place path is judged too, where no
	// kept session is continued.
	basis.Window = l.continuationWindow()
	if !basis.Continues() && !basis.ContinuesBase() && !l.conversation.leftNone() {
		log.InfoContext(ctx, "executor_session_not_continued", "seat", l.seat.Handle(),
			"turn_id", l.turn.RunID, "detail", basis.Why,
			"outcome", "the launch starts a session of its own, or continues the one its box holds")
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
	if l.conversation != nil {
		// THE TURN'S FIRST LAUNCH: the head its dispatch read, which no
		// record or row of the turn exists yet to say anything more of.
		return l.conversation.basis(restores)
	}
	continued, why := rt.coordinator.ContinuedCapture(ctx, l.turn.RunID, restores)
	if base := continued.Base; base != nil {
		// A TURN THAT HAS KEPT NOTHING, relaunched: the base its first launch
		// froze, which its row still holds.
		basis := baseBasis(*base, continued.Context)
		basis.KeptTurn = continued.KeptTurn
		return basis
	}
	if why != "" {
		// THE SESSION THE TURN KEEPS, or else the base it froze, where the
		// relaunch continues neither: a run that starts a session of its own
		// beside it is told it may read it.
		return runner.SessionBasis{Why: why, ContextTokens: continued.Context, KeptTurn: continued.KeptTurn}
	}
	captured := continued.Capture
	// ITS SAVE POINT, which every capture a relaunch continues states: one
	// that states none is not continued ([sandbox.ContinuityOf]). Read as none
	// of the round's calls where it did not — every call told again, and no
	// load armed — the safe direction for both readers of it.
	through, _ := captured.SavePoint()
	return runner.SessionBasis{
		LaunchSeq: captured.LaunchSeq, CaptureSeq: captured.CaptureSeq, SessionID: continued.Row.SessionID,
		Harness: continued.Row.Harness, Through: through, NoteCursor: captured.NoteCursor,
		Interrupted: captured.Interrupted, Home: captured.Home, ContextTokens: continued.Context,
		KeptTurn: continued.KeptTurn,
		// AND WHAT CLAUDE CODE COUNTS THE CAPTURE'S SESSION AT, as its take
		// read its archive whole: what the kept path's first request is judged
		// by.
		Count: captured.ResumedCount,
		// AND THE READS AT OR BELOW IT THE SESSION HOLDS ONLY AS ITS JOB'S
		// BRIEF'S COUNT, which the brief continuing it tells.
		Owed: slices.Clone(captured.Omitted),
	}
}

// continuationWindow is what the seat's executor chain may hold a continued
// Claude Code run to ([runner.SessionBasis.Window]): the least standard window
// over every Claude Code member, each on its entry's route ([claudeRoute]),
// since the launch picks its member only as it launches; Known only where
// every one is known. Not Known where the node has no sandbox runtime, the
// chain cannot be read, or it holds no Claude Code member: a continuation is
// then told as one that may compact, never held whole on a window nobody
// read.
func (l *agentLauncher) continuationWindow() sandbox.ExecutorWindow {
	rt := l.engine.sandbox.Load()
	manager := l.company.catalogue
	if rt == nil || rt.coordinator == nil || manager == nil {
		return sandbox.ExecutorWindow{}
	}
	members, err := executorRunners(l.company, l.seat, manager)
	if err != nil {
		return sandbox.ExecutorWindow{}
	}
	var (
		least sandbox.ExecutorWindow
		seen  bool
	)
	for _, m := range members {
		agent, ok := m.Provider.(*harness.Provider)
		if !ok || agent.Harness() != executor.ClaudeCodeName {
			continue
		}
		w := sandbox.ClaudeCodeWindowOn(agent.ModelArgument(), claudeRoute(agent.AuthMode()))
		switch {
		case !seen:
			least, seen = w, true
		case !w.Known:
			least.Known = false
		case w.Standard < least.Standard:
			least.Standard = w.Standard
		}
	}
	if !seen {
		return sandbox.ExecutorWindow{}
	}
	return least
}

// claudeRoute is the route a Claude Code member reaches its model by, as far as
// the telling reads it ([sandbox.ClaudeRoute]): Anthropic's own API for an entry
// signed in with a login, a key or the host's own environment, each of which
// hands the harness first-party credentials alone (config refuses a sign-in
// switch in such an entry's env); and a route this build cannot name for an
// entry signed in by its own env, and for a mode this build does not know,
// which is held to the least window off Anthropic's API, the side that tells
// more. The launch floor reads the entry's platform exactly ([claudePlatform]),
// since there a smaller window costs a session rather than words.
func claudeRoute(mode harness.AuthMode) sandbox.ClaudeRoute {
	switch mode {
	case harness.AuthSubscription, harness.AuthAPIKey, harness.AuthInheritEnv:
		return sandbox.ClaudeRouteAnthropic
	}
	return sandbox.ClaudeRouteElsewhere
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

// relaunchTarget is what a launch of seat's turn — a relaunch, or a
// conversation's next turn — would be launched on now, as this node picks it
// ([sandbox.RelaunchTarget]): the first member of the seat's executor chain
// with an in-box runner that is not sitting out its plan on this node — the
// member the launch's own pick takes ([agentLauncher.member]), without the
// hand-offs the pick announces — the cell its run lands in, this node, and its
// runner as it restores a session carried between boxes; or why there is none.
//
// THIS NODE'S PICK, and so the one a reader of a launch is handed only where
// this node holds the seat: which members sit out is this node's own bench, and
// the launch is decided on the node holding the seat ([Engine.RelaunchTargets]
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
		"chain is sitting out its plan on the node holding the seat, and which of them takes a launch " +
		"decides what it continues"}
}

// localRelaunchTarget is what a relaunch of the seat handle's turn would be
// launched on — or, for no turn, the next turn of one of the seat's
// conversations — as the company revision that launch runs on picks it now
// ([Engine.relaunchCompany], [Company.relaunchTarget]) — and, where the company
// keeps no session, that the launch continues no kept one, by the keep the
// coordinator's launch asks ([executorKeep.Keeps]), which reads the revision
// this node runs. A node running no company, or none that reaches a sandbox
// cell, and a handle the company holds no seat under, have no member to say
// anything of, which the target says.
func (e *Engine) localRelaunchTarget(handle, turn string) sandbox.RelaunchTarget {
	company := e.relaunchCompany(turn)
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

// relaunchCompany is the company revision a relaunch of turn would be launched
// under on this node: the one a resumed segment of the turn running here was
// admitted under ([resumeInput.Company]) — its reviewer's send-back and the
// round it carries on are launched from that segment's pinned epoch, which an
// apply landing mid-segment does not move — and otherwise the revision this
// node runs now, which a segment admitted later pins. No turn is a
// conversation's next turn, whose first launch the revision this node runs
// when it comes admits: never a pinned one, whatever segments run here.
func (e *Engine) relaunchCompany(turn string) *Company {
	if turn != "" {
		if pinned, ok := e.resumedSegments.Load(turn); ok {
			return pinned.(*Company)
		}
	}
	return e.Company()
}

// sandboxSession is the session a launch continues in the sandbox layer's
// terms, nil where it continues none: a relaunch's kept capture, with the brief
// for the box the session was last worked in and the one for any other, each
// where the sandbox layer hands it; or the session the turn's conversation's
// last turn left, which the sandbox layer reads off the turn's record as its
// first launch froze it and continues only in a box of its own, with the one
// brief for that. Either with what its briefs left out of the round the launch
// carries on, which the launch records where it hands one ([sandbox.BriefTold]).
func sandboxSession(s *runner.SessionContinuation) *sandbox.SessionContinuation {
	switch {
	case s == nil:
		return nil
	case s.Basis.ContinuesBase():
		return &sandbox.SessionContinuation{Base: true, Replaced: s.Replaced, Told: s.Told}
	case !s.Basis.Continues():
		return nil
	}
	return &sandbox.SessionContinuation{LaunchSeq: s.Basis.LaunchSeq, CaptureSeq: s.Basis.CaptureSeq,
		Brief: s.Brief, Replaced: s.Replaced, Told: s.Told}
}

// sessionDecision is how the launch decided its run takes up a session, in the
// runner's terms ([runner.SessionDecision]): a session its conversation's last
// turn left is always continued in a new box.
func sessionDecision(d sandbox.LaunchDecision) runner.SessionDecision {
	return runner.SessionDecision{Continues: d.Brief.Continues(), Kept: d.Kept, Base: d.Base,
		NewBox: d.Brief == sandbox.BriefContinuedNewBox || d.Brief == sandbox.BriefContinuedConversation}
}
