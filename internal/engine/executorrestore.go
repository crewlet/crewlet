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
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A turn's next launch continues a session (internal/sandbox's restore.go has
// the mechanism): the turn's own kept one on a relaunch, else the one its
// conversation's last turn left.
//
// Rows are read through the estate router wherever the seat runs: a seats-only
// node holds no log, and the capture may be another node's keep, which a
// `linearizable` read waits for. The sealed object opens through the domain's
// session store over the Tier A keyring. Which session a launch continues is
// asked before the runner builds briefs ([agentLauncher.ProbeSession]); which
// member takes it, and whether the budget can carry it, once they exist
// ([agentLauncher.launch], [launchFloor]).

// What a cut-off kept session holds is read before the briefs are written
// ([agentLauncher.SessionHoldings]), through the runner of the harness that
// wrote it, for its release ([runner.SessionBasis.Release]). The briefs told by
// it go to a box of that release only ([sandboxSession]); the floor drops them
// alone where the budget cannot carry them ([agentLauncher.continuedFloor]).
// A reader's answer to what a relaunch would do is [Engine.RelaunchTargets].

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

var _ sandbox.SessionHolders = executorRestore{}

// SessionHeld reads whether any turn's row names the session id through the
// estate router's reads, at the level a bind asks ([executordomain.BindRead]):
// what a minted session's bind asks of a fresh id (internal/sandbox, minted.go).
func (r executorRestore) SessionHeld(ctx context.Context, sessionID string) (bool, error) {
	if r.engine.router == nil {
		return false, errors.New("this node has no estate router to read the executor log through")
	}
	return r.engine.router.ExecutorReads().SessionHeld(ctx, sessionID, executordomain.BindRead())
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

// Open streams the object row names out of the session store, opened against
// the row as it streams ([executordomain.SessionStore.Open]): what a restore
// and a reading of what the session holds read, spooling nothing on this
// node's disk.
func (r executorRestore) Open(ctx context.Context, row executorkeep.TurnRow) (io.ReadCloser, error) {
	store, err := r.engine.executorSessionStore()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, storedSession(row))
}

// storedSession is the row the session store fetches and opens by: whose turn
// it is, which names the seal's associated data, the object it names, the
// media its frames splice in and the archive they restore to, which its read
// is budgeted for. The store reads nothing else of a row.
func storedSession(row executorkeep.TurnRow) executordomain.Session {
	return executordomain.Session{Handle: row.Handle, Turn: row.Turn, Object: row.Object, Media: row.Media,
		ArchiveBytes: row.ArchiveBytes}
}

// ProbeSession answers which session a launch of the turn continues
// ([runner.SessionProber]): on the FIRST launch, the head its dispatch read
// ([conversationRead.basis]); on a relaunch, as
// [sandbox.Coordinator.ContinuedCapture] decides for the seat's chain, or why
// none (logged, but for a conversation's first turn). EACH SESSION'S CONTEXT
// IS ITS OWN: a kept capture's own measure and next-request estimate
// ([runner.SessionBasis.ContextTokens], NextRequest) for the floor in any box,
// its job's newest capture's, either way, for its copy in place (CopyContext,
// CopyNextRequest), since one figure would hold a session to what another
// carried. ANY MEMBER OF THE CHAIN, never one picked here: the launch picks
// once as it launches, and hands the briefs on only where its member runs the
// session's harness ([agentLauncher.continuedFloor]).
func (l *agentLauncher) ProbeSession(ctx context.Context) runner.SessionBasis {
	basis, own := l.probeSession(ctx)
	// AND THE WINDOW EVERY PATH'S FIRST REQUEST IS JUDGED UNDER, on every basis
	// whatever branch answered it: the in-place path is judged too, where no
	// kept session is continued.
	basis.Window = l.continuationWindow()
	basis.Traits = sessionTraits()
	// AND THE SUMMARY OF THE SESSION IT DOES NOT CONTINUE, within what its brief
	// has room for under that window ([agentLauncher.handOver]).
	basis = l.handOver(ctx, basis, own)
	if n := basis.NotContinued; n != nil {
		sayNotContinued(ctx, l.seat.Handle(), l.turn.RunID, *n, "decided", "probe")
	}
	return basis
}

// probeSession is [agentLauncher.ProbeSession] before it reads the lost
// session's summary and logs, with the summary the turn's record names, nil
// where the probe read none.
func (l *agentLauncher) probeSession(ctx context.Context) (runner.SessionBasis, *executorkeep.Summary) {
	rt := l.engine.sandbox.Load()
	manager := l.company.catalogue
	// NO EVENT WHERE NOTHING CAN BE CONTINUED: a node with no runtime, or a
	// chain none of whose harnesses keeps a session.
	if rt == nil || rt.coordinator == nil || manager == nil {
		return runner.SessionBasis{}, nil
	}
	restores, err := l.company.chainRestores(l.seat, manager)
	switch {
	case err != nil:
		return runner.SessionBasis{NotContinued: executorkeep.Lost(executorkeep.LossRestoreFailed,
			executorkeep.InsteadFresh, err.Error())}, nil
	case len(restores) == 0:
		// NOTHING TO WAIT FOR: no member could take a session up, whatever
		// the job's capture comes to.
		return runner.SessionBasis{}, nil
	}
	if l.conversation != nil {
		// THE TURN'S FIRST LAUNCH: the head its dispatch read, which no
		// record or row of the turn exists yet to say anything more of.
		return l.conversation.basis(restores), nil
	}
	continued, why := rt.coordinator.ContinuedCapture(ctx, l.turn.RunID, restores)
	if base := continued.Base; base != nil {
		// A TURN THAT HAS KEPT NOTHING, relaunched: the base its first launch
		// froze, which its row still holds.
		basis := baseBasis(*base, continued.CopyContext)
		basis.KeptTurn, basis.CopyNextRequest = continued.KeptTurn, continued.CopyNextRequest
		basis.NotContinued = continued.NotContinued
		return basis, continued.Summary
	}
	if why != "" {
		// THE SESSION THE TURN KEEPS, or else the base it froze, where the
		// relaunch continues neither: a run that starts a session of its own
		// beside it is told it may read it.
		return runner.SessionBasis{NotContinued: continued.NotContinued, CopyContext: continued.CopyContext,
			CopyNextRequest: continued.CopyNextRequest, KeptTurn: continued.KeptTurn}, continued.Summary
	}
	captured := continued.Capture
	// ITS SAVE POINT, which every capture a relaunch continues states: one
	// that states none is not continued ([sandbox.ContinuityOf]). Read as none
	// of the round's calls where it did not — every call told again, and no
	// load armed — the safe direction for both readers of it.
	through, _ := captured.SavePoint()
	return runner.SessionBasis{
		LaunchSeq: captured.LaunchSeq, CaptureSeq: captured.CaptureSeq, SessionID: continued.Row.SessionID,
		Harness: continued.Row.Harness, Entry: continued.Row.Entry,
		// AND THE RELEASE THAT WROTE IT, which a reading of what the session
		// holds is read for: the relaunch's best word for the release that
		// resumes it, its own box not yet acquired.
		Release: continued.Row.HarnessVersion,
		Through: through, NoteCursor: captured.NoteCursor,
		Interrupted: captured.Interrupted, Home: captured.Home,
		// AND WHETHER ITS JOB WENT ON PAST IT, and when it was taken: a
		// capture that is not final was taken while its job still worked, so
		// the run continuing it is told the session ends where it was saved,
		// not where its job stopped.
		WentOn: !captured.Final, CapturedAt: captured.At,
		// WHAT THE CAPTURE ITSELF CARRIED AND ITS HARNESS PUT ITS NEXT REQUEST
		// AT, the kept path's floor, and the same of the job's copy in its
		// box, the in-place path's.
		ContextTokens: captured.ContextTokens, NextRequest: captured.NextRequest(),
		CopyContext: continued.CopyContext, CopyNextRequest: continued.CopyNextRequest,
		KeptTurn: continued.KeptTurn,
		// AND WHAT ITS HARNESS COUNTS THE CAPTURE'S SESSION AT, as its take
		// read its archive whole, with the provider an OpenCode count's reply
		// was answered by: what the kept path's first request is judged by.
		Count: captured.ResumedCount, CountProvider: captured.CountProvider,
		// AND THE READS AT OR BELOW IT THE SESSION HOLDS ONLY AS ITS JOB'S
		// BRIEF'S COUNT, which the brief continuing it tells.
		Owed: slices.Clone(captured.Omitted),
		// AND THE LATE CALLS IT HOLDS ONLY AS THAT BRIEF'S COUNT, which it is
		// credited no telling of.
		LateOwed: slices.Clone(captured.LateOmitted),
		// AND A NEWER CAPTURE OF THE JOB IT PASSED OVER, where it did.
		NotContinued: continued.NotContinued,
	}, continued.Summary
}

// sayNotContinued is the engine's one `executor_session_not_continued` line: a
// launch's verdict, in its sentence, with the engine's detail.
func sayNotContinued(ctx context.Context, seat, turn string, n executorkeep.NotContinued, attrs ...any) {
	log.InfoContext(ctx, "executor_session_not_continued", append([]any{"seat", seat, "turn_id", turn,
		"loss", string(n.Loss), "instead", string(n.Instead), "said", n.Sentence(),
		"detail", redact.Secrets(n.Detail)}, attrs...)...)
}

var _ runner.HoldingsAsker = (*agentLauncher)(nil)

// SessionHoldings answers which of the round's asked calls the cut-off kept
// session basis names holds ([runner.HoldingsAsker]), as
// [sandbox.Coordinator.HoldingsOf] reads it off the turn's capture through the
// runner of the chain member driving the harness that wrote it
// ([chainRestorers.reader]); another harness's runner refuses the session. It
// logs `executor_session_holdings_read` (with one
// `executor_session_subagent_unreported` per subagent under a told call) or
// `executor_session_holdings_unread` with why. Never an error to the runner:
// every failure is no reading, and the relaunch is told every call after the
// save point, which is never wrong.
func (l *agentLauncher) SessionHoldings(ctx context.Context, basis runner.SessionBasis,
	ask sandbox.HoldingsAsk) sandbox.SessionHoldings {

	holdings, err := l.sessionHoldings(ctx, basis, ask)
	if err != nil {
		log.InfoContext(ctx, "executor_session_holdings_unread", "seat", l.seat.Handle(),
			"turn_id", l.turn.RunID, "detail", err.Error(),
			"outcome", "the relaunch is told every call after its save point")
		return sandbox.SessionHoldings{}
	}
	asked, held := 0, 0
	var unreported []string
	for _, c := range ask.Calls {
		if !c.Asked {
			continue
		}
		asked++
		in, under := holdings.Holds(c)
		switch {
		case in:
			held++
		case under != "" && !slices.Contains(unreported, under):
			unreported = append(unreported, under)
		}
	}
	log.InfoContext(ctx, "executor_session_holdings_read", "seat", l.seat.Handle(), "turn_id", l.turn.RunID,
		"asked", asked, "held", held, "told", asked-held, "unreported", len(unreported), "writer", ask.Resumer,
		"compacted", holdings.Compacted, "outcome", readOutcome(held, len(unreported)))
	// ONE LINE PER SUBAGENT, never per call: its calls are told under one
	// heading, and the line says why the session does not hold its report.
	for _, agent := range unreported {
		log.InfoContext(ctx, "executor_session_subagent_unreported", "seat", l.seat.Handle(),
			"turn_id", l.turn.RunID, "agent", agent, "detail", holdings.Subagents[agent].Why)
	}
	return holdings
}

// readOutcome is what follows a reading that held held of the asked calls and
// told some under unreported subagents, as `executor_session_holdings_read`
// says it.
//
// BY WHAT FOLLOWS, NEVER ONE PROMISE FOR EVERY READING: one that holds no
// asked call and moves none under a subagent tells what the telling with no
// reading tells, so the runner builds no brief told by it (its heldReading),
// and no line says it was not handed. Every other reading is followed by the
// brief told by it, by `executor_session_holdings_unused` saying why that brief
// was not handed, or by `executor_session_not_continued` where the launch does
// not continue the session at all, the brief told by the reading with it.
func readOutcome(held, unreported int) string {
	if held == 0 && unreported == 0 {
		return "the reading holds no asked call and moves none, so the relaunch is told every call after its " +
			"save point"
	}
	return "the launch hands the brief told by it where its box's harness answers writer and its first request " +
		"sends the session whole; otherwise executor_session_holdings_unused says why not, or " +
		"executor_session_not_continued where the session itself is not continued"
}

// sessionHoldings is [agentLauncher.SessionHoldings] before it logs: the
// reading, or why there is none. A reading the reader answered without
// having read the archive whole, or of a kind this build does not read, is
// none: it holds no call ([sandbox.SessionHoldings.Holds]), and the line saying
// it was read would claim what it did not.
func (l *agentLauncher) sessionHoldings(ctx context.Context, basis runner.SessionBasis,
	ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	rt := l.engine.sandbox.Load()
	manager := l.company.catalogue
	if rt == nil || rt.coordinator == nil || manager == nil {
		return sandbox.SessionHoldings{}, errors.New("this node has no sandbox runtime for this company")
	}
	restores, err := l.company.chainRestores(l.seat, manager)
	if err != nil {
		return sandbox.SessionHoldings{}, err
	}
	holdings, err := rt.coordinator.HoldingsOf(ctx, l.turn.RunID, basis.LaunchSeq, basis.CaptureSeq, ask,
		restores.reader(basis.Harness))
	switch {
	case err != nil:
		return sandbox.SessionHoldings{}, err
	case !holdings.Read:
		return sandbox.SessionHoldings{}, errors.New("the reading did not read the session's archive whole")
	case !holdings.Kind.Valid():
		return sandbox.SessionHoldings{}, fmt.Errorf("the reading is of a kind this build does not read (%q)",
			holdings.Kind)
	}
	return holdings, nil
}

// continuationWindow is what the seat's executor chain may hold a continued
// run to ([runner.SessionBasis.Window]): the least window over every member
// whose harness compacts by its window, each on its entry's coarse route
// ([executor.WindowAsk.Exact] unset), since the launch picks its member only as
// it launches; Known only where every one is known
// ([sandbox.ExecutorWindow.Least]). Not Known where the node has no sandbox
// runtime, the chain cannot be read, or it holds no such member: a
// continuation is then told as one that may compact, never held whole on a
// window nobody read.
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
		if !ok {
			continue
		}
		w, compacts := executor.Window(agent.Harness(), windowAsk(agent, false))
		switch {
		case !compacts:
		case !seen:
			least, seen = w, true
		default:
			least = least.Least(w)
		}
	}
	return least
}

// sessionTraits is what a relaunch reads of every harness this build drives
// ([runner.SessionBasis.Traits]), by name.
func sessionTraits() map[string]sandbox.SessionTraits {
	traits := make(map[string]sandbox.SessionTraits, len(executor.Names()))
	for _, name := range executor.Names() {
		traits[name] = executor.SessionTraits(name)
	}
	return traits
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
		restorer, ok := run.(sandbox.SessionRestorer)
		if !ok {
			continue
		}
		reader, _ := run.(sandbox.HoldingsReader)
		out = append(out, harnessRestorer{harness: agent.Harness(),
			restorer: sandbox.RestorerUnder(restorer, agent.EntryID()), reader: reader})
	}
	return out, nil
}

// harnessRestorer is one member's runner as a launch under that member's
// entry restores a session ([sandbox.RestorerUnder]), the harness it drives,
// and the runner as it reads what a session holds, nil where it reads none.
type harnessRestorer struct {
	harness  string
	restorer sandbox.ArchiveRestorer
	reader   sandbox.HoldingsReader
}

// chainRestorers is the runners of a seat's chain as one [sandbox.ArchiveRestorer]:
// an archive is restored where the runner of a member driving the harness that
// wrote it restores it under that member's entry — a session is continued only
// by its own harness, and only under the entry it ran under unless that
// harness resumes on its launch's route — and the reason is that runner's
// where none does.
type chainRestorers []harnessRestorer

var _ sandbox.ArchiveRestorer = chainRestorers(nil)

// Restores answers [sandbox.ArchiveRestorer] for the chain.
func (c chainRestorers) Restores(archive sandbox.SessionArchive) sandbox.SessionContinuity {
	var refused sandbox.SessionContinuity
	for _, r := range c {
		if r.harness != archive.Harness {
			continue
		}
		if refused = r.restorer.Restores(archive); refused.Continues {
			return refused
		}
	}
	if refused.Why == "" {
		refused = sandbox.SessionContinuity{Loss: executorkeep.LossHarnessChanged, Why: fmt.Sprintf("no member "+
			"of the seat's executor chain runs %s, which wrote the session: a session is continued only by the "+
			"harness that wrote it", archive.Harness)}
	}
	return refused
}

// reader is the runner of the chain's member driving harness as it reads
// what a session holds ([sandbox.HoldingsReader]), nil where no member drives
// harness or that member's runner reads none: a session's holdings are read
// only by its own harness's runner, which is the one that restores it
// ([chainRestorers.Restores]), and one runner drives each harness, so the
// first member driving it names it.
func (c chainRestorers) reader(harness string) sandbox.HoldingsReader {
	for _, r := range c {
		if r.harness != harness {
			continue
		}
		return r.reader
	}
	return nil
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
			Node: node, Entry: agent.EntryID(), LaunchRouted: executor.ResumesOnLaunchRoute(agent.Harness())}
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
		target.Restorer = sandbox.RestorerUnder(restorer, agent.EntryID())
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
// terms, nil where it continues none: a relaunch's kept capture, with a brief
// for the box it was last worked in and one for any other; or the session the
// conversation's last turn left (a base), continued only in a box of its own
// with the one brief. Either carries what its briefs left out of the round
// ([sandbox.BriefTold]). A kept capture also carries the two briefs told by a
// reading ([runner.SessionContinuation.Exact]), handed only to a box of their
// release, and the two for any other release or the mark that the floor dropped
// them ([runner.SessionContinuation.Unheld]); never beside a base.
func sandboxSession(s *runner.SessionContinuation) *sandbox.SessionContinuation {
	switch {
	case s == nil:
		return nil
	case s.Basis.ContinuesBase():
		return &sandbox.SessionContinuation{Base: true, Replaced: s.Replaced, Told: s.Told, Handover: s.Handover}
	case !s.Basis.Continues():
		return nil
	}
	out := &sandbox.SessionContinuation{LaunchSeq: s.Basis.LaunchSeq, CaptureSeq: s.Basis.CaptureSeq,
		Brief: s.Brief, Replaced: s.Replaced, Told: s.Told, UnheldDropped: s.UnheldDropped, Handover: s.Handover}
	if x := s.Exact; x != nil {
		out.Exact = &sandbox.ExactTelling{Resumer: x.Resumer, Brief: x.Brief, Replaced: x.Replaced, Told: x.Told}
	}
	if x := s.Summarized; x != nil {
		out.Summarized = &sandbox.SummarizedTelling{Brief: x.Brief, Replaced: x.Replaced, Told: x.Told}
		out.Count, out.CountProvider = s.Count, s.CountProvider
	}
	if u := s.Unheld; u != nil {
		out.Unheld = &sandbox.UnheldTelling{Brief: u.Brief, Replaced: u.Replaced, Told: u.Told}
	}
	return out
}

// sessionDecision is how the launch decided its run takes up a session, in the
// runner's terms ([runner.SessionDecision]): a session its conversation's last
// turn left is always continued in a new box; and whether the brief handed is
// the one told by a reading of what the session holds, or the one for a box
// whose release the resume was not read in, whose own telling the run's guard
// and its published prompt then follow.
func sessionDecision(d sandbox.LaunchDecision) runner.SessionDecision {
	return runner.SessionDecision{Continues: d.Brief.Continues(), Kept: d.Kept, Base: d.Base,
		NewBox: d.Brief == sandbox.BriefContinuedNewBox || d.Brief == sandbox.BriefContinuedConversation,
		Exact:  d.Exact, Summarized: d.Summarized, Unheld: d.Unheld}
}
