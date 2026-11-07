package queries_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- whether a relaunch continues a session ----------------------------- //

// stubRuns is the fleet's run records as the sessions surface reads them, and
// how many times each read was asked: a seat's live runs in one listing, and
// one turn's.
type stubRuns struct {
	mu     sync.Mutex
	runs   []sandbox.PendingRun
	err    error
	listed int
	got    int
}

func (s *stubRuns) ListActiveForSeat(_ context.Context, handle string) ([]sandbox.PendingRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed++
	if s.err != nil {
		return nil, s.err
	}
	var out []sandbox.PendingRun
	for _, run := range s.runs {
		if run.AgentHandle == handle {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s *stubRuns) Get(_ context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got++
	if s.err != nil {
		return sandbox.PendingRun{}, false, s.err
	}
	for _, run := range s.runs {
		if run.TurnID == turnID {
			return run, true, nil
		}
	}
	return sandbox.PendingRun{}, false, nil
}

// reads is how many times each read was asked.
func (s *stubRuns) reads() (listed, got int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed, s.got
}

// stubTargets is the node holding each seat as the sessions surface asks it
// what a relaunch of the seat's turns would be launched on — the seat's one
// pick, or a turn's own where byTurn names one, as a turn whose resumed segment
// runs on the holder is answered from that segment's revision — and every set
// of sessions it was asked about; a seat it does not name has no member.
type stubTargets struct {
	mu      sync.Mutex
	targets map[string]sandbox.RelaunchTarget
	byTurn  map[string]sandbox.RelaunchTarget
	asked   [][]sandbox.RelaunchAsk
}

// targetOf is the holder of ana's seat answering target.
func targetOf(target sandbox.RelaunchTarget) *stubTargets {
	return &stubTargets{targets: map[string]sandbox.RelaunchTarget{"ana": target}}
}

func (s *stubTargets) RelaunchTargets(_ context.Context, _ uuid.UUID, handle string,
	asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slices.Clone(asks))
	out := make([]sandbox.RelaunchTarget, len(asks))
	for i, ask := range asks {
		target, own := s.byTurn[ask.Turn]
		if !own {
			target = s.targets[handle]
		}
		out[i] = target
	}
	return out
}

// asks is every set of sessions the holder was asked about, in order.
func (s *stubTargets) asks() [][]sandbox.RelaunchAsk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// restoring is a runner of harness that restores every session that harness
// wrote, and none another wrote — or, given why, none at all, saying so.
type restoring struct{ harness, why string }

func (r restoring) Restores(archive sandbox.SessionArchive) (bool, string) {
	switch {
	case r.why != "":
		return false, r.why
	case archive.Harness != r.harness:
		return false, "the session was written by " + archive.Harness + ", and this runner drives " + r.harness
	}
	return true, ""
}

// holder is the node holding ana's seat, on whose host the run's direct box
// was launched.
const holder = "node-a"

// claudeTarget is a relaunch of ana's turns launched on a Claude Code member in
// the direct cell, as the node holding the seat picks it, whose runner
// restores what Claude Code wrote.
func claudeTarget() sandbox.RelaunchTarget {
	return sandbox.RelaunchTarget{Harness: executor.ClaudeCodeName, Placement: sandbox.Direct, Node: holder,
		Restorer: restoring{harness: executor.ClaudeCodeName}}
}

// sessionAt is when ana's live turn kept its capture, and the clock the
// continuable cases read is pinned an hour after it: a box ages out of a
// relaunch after [sandbox.KeptBoxMaxAge], so read on the wall clock these
// fixtures stopped being continuable a week after they were written.
var sessionAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// liveSession is ana's live turn whose job, its second, kept its session's
// first capture, as the executor log's row names it and the run's record keeps
// it, written by harness — the job parked on a question, its box held in the
// direct cell on the host of the node holding the seat, as a collection leaves
// a job that asked. Each is a conversation of its own, so ended, it is its
// conversation's head.
func liveSession(harness string) (executordomain.Session, sandbox.PendingRun) {
	turn := uuid.NewString()
	at := sessionAt
	key := objstore.KeyAt(at)
	kept := sandbox.SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: sandbox.CaptureLanded,
		Object: key.String(), At: at, Through: new(0)}
	row := executordomain.Session{Handle: "ana", Turn: turn, ConversationKey: "chat:" + turn, Harness: harness,
		SessionID: "s-1", ArchiveFormat: executor.ClaudeCode{}.ArchiveFormat(), CapturedAt: at,
		Object:    objstore.Object{Key: key, Hash: objstore.Hash(strings.Repeat("a", 64)), Size: 4096},
		LaunchSeq: 2, CaptureSeq: 1}
	return row, sandbox.PendingRun{TurnID: turn, AgentHandle: "ana", Status: sandbox.StatusAwaiting,
		SandboxID: "box-1", Placement: string(sandbox.Direct), LaunchNode: holder, Harness: harness,
		SessionID: "s-1", LaunchSeq: 2, Capture: &kept, Kept: &kept, BoxSince: at.Add(-time.Hour)}
}

// continuableSources answers the sessions questions over rows, deciding each
// row's continuity off runs and a relaunch of ana's turns on a Claude Code
// member ([claudeTarget]).
func continuableSources(t *testing.T, rows []executordomain.Session, runs *stubRuns) queries.Sources {
	t.Helper()
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, &stubSessions{rows: rows}, store)
	if runs != nil {
		s.SessionRuns = runs
	}
	s.RelaunchTargets = targetOf(claudeTarget())
	s.Now = func() time.Time { return sessionAt.Add(time.Hour) }
	return s
}

// rowsByTurn is a page's rows by their turns.
func rowsByTurn(t *testing.T, page any) map[string]queries.SessionRow {
	t.Helper()
	rows := map[string]queries.SessionRow{}
	for _, row := range page.(queries.SessionsAnswer).Sessions {
		rows[row.Turn] = row
	}
	return rows
}

// decided is what a row says of a relaunch, and why, in one line for a failure.
func decided(row queries.SessionRow) string {
	if row.Continuable == nil {
		return "undecided (" + row.NotContinuable + ")"
	}
	if *row.Continuable {
		return "continuable in " + string(row.ContinuesIn) + " (" + row.NotContinuable + ")"
	}
	return "not continuable (" + row.NotContinuable + ")"
}

// A SESSION'S ROW SAYS WHICH LAUNCH WOULD CONTINUE IT, AND WHETHER, AS THAT
// LAUNCH DECIDES IT. Asked of the one decision that launch takes, over the
// turn's row on the log, its run's record, its conversation's head as the page
// named it and the member the launch is launched on: a live turn whose job kept
// the capture the row names, which that member's runner restores, is
// continuable by a relaunch in any box, with no reason; a session another
// harness wrote, which the member's runner does not take up, is not continued
// from its capture — and, its parked box of that other harness, nowhere — saying
// why; an ended turn's session that is its conversation's head is the next
// turn's, continuable in any box where the member restores it; and a live turn
// whose end is owed, whose run's ending is decided, or whose run is over, is the
// next turn's too, undecided until its end is on the log. A whole page reads the
// coordination store ONCE, and once more for the one live row the listing does
// not hold, and asks the node holding the seat ONCE, about the two live rows a
// relaunch decides, each by its turn, and the head the next turn decides, by no
// turn; one session read alone reads its own turn's record and asks about its
// own turn's session, and nothing else.
//
// Mutation: decide a row by anything but sandbox.ReadContinuityOf, read the
// store or ask the holder once per row, ask the runner of the harness that
// wrote the session rather than the launch's member, ask about an ended head
// by its own turn, answer a decided ending as live, or say which launch a row
// is about otherwise than the decision did, and a check goes red.
func TestASessionsRowSaysWhichLaunchWouldContinueIt(t *testing.T) {
	t.Parallel()
	continues, continuesRun := liveSession(executor.ClaudeCodeName)
	continuesRun.Status = sandbox.StatusRunning
	continuesRun.Launch.Collected = true
	ending, endingRun := liveSession(executor.ClaudeCodeName)
	endingRun.OwedEnd = []byte(`{"outcome":"completed"}`)
	stopping, stoppingRun := liveSession(executor.ClaudeCodeName)
	stoppingRun.Ending = &sandbox.RecordedEnding{ID: "ending-1", At: stopping.CapturedAt}
	foreign, foreignRun := liveSession(executor.OpenCodeName)
	over, _ := liveSession(executor.ClaudeCodeName)
	ended, endedRun := liveSession(executor.ClaudeCodeName)
	ended.EndedAt = ended.CapturedAt.Add(time.Minute)
	ended.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	runs := &stubRuns{runs: []sandbox.PendingRun{continuesRun, endingRun, stoppingRun, foreignRun, endedRun}}
	s := continuableSources(t, []executordomain.Session{continues, ending, stopping, foreign, over, ended}, runs)

	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	rows := rowsByTurn(t, page)
	for name, tc := range map[string]struct {
		turn      string
		by        sandbox.ContinuedBy
		decided   bool
		continues bool
		why       string
	}{
		"a live turn's kept session": {turn: continues.Turn, by: sandbox.ContinuedByRelaunch, decided: true,
			continues: true},
		"a session another harness wrote": {turn: foreign.Turn, by: sandbox.ContinuedByRelaunch, decided: true,
			why: "written by " + executor.OpenCodeName + ", and this runner drives " + executor.ClaudeCodeName},
		"an ended turn's session, its conversation's head": {turn: ended.Turn, by: sandbox.ContinuedByNextTurn,
			decided: true, continues: true},
		"a turn whose end is owed": {turn: ending.Turn, by: sandbox.ContinuedByNextTurn,
			why: "is ending, and its end is not on the executor log"},
		"a turn whose run's ending is decided": {turn: stopping.Turn, by: sandbox.ContinuedByNextTurn,
			why: "is ending, and its end is not on the executor log"},
		"a live turn whose run is over": {turn: over.Turn, by: sandbox.ContinuedByNextTurn,
			why: "run is over, and its end is not on the executor log"},
	} {
		row, ok := rows[tc.turn]
		switch {
		case !ok:
			t.Errorf("%s: the page holds no row of its turn", name)
		case row.ContinuedBy != tc.by:
			t.Errorf("%s: %s by %q, want it about %q", name, decided(row), row.ContinuedBy, tc.by)
		case (row.Continuable != nil) != tc.decided:
			t.Errorf("%s: %s, want decided %v", name, decided(row), tc.decided)
		case !strings.Contains(row.NotContinuable, tc.why) || (tc.continues && (row.NotContinuable != "" ||
			row.ContinuesIn != sandbox.ContinuesInAnyBox)) || (!tc.continues && row.ContinuesIn != ""):
			t.Errorf("%s: %s, want continues %v saying %q", name, decided(row), tc.continues, tc.why)
		case tc.decided && *row.Continuable != tc.continues:
			t.Errorf("%s: %s, want continues %v", name, decided(row), tc.continues)
		}
	}
	if listed, got := runs.reads(); listed != 1 || got != 1 {
		t.Errorf("a page of six rows read the seat's runs %d times and a turn's %d, want one listing "+
			"and the one live turn it does not hold", listed, got)
	}
	targets := s.RelaunchTargets.(*stubTargets)
	asks := targets.asks()
	if len(asks) != 1 || len(asks[0]) != 3 {
		t.Fatalf("a page asked the node holding the seat %v, want once, about the two live rows a relaunch "+
			"decides and the head the next turn decides", asks)
	}
	if !slices.Contains(asks[0], sandbox.RelaunchAsk{Turn: continues.Turn,
		Archive: sandbox.ArchiveOf(continues.Kept())}) ||
		!slices.Contains(asks[0], sandbox.RelaunchAsk{Archive: sandbox.ArchiveOf(ended.Kept())}) {
		t.Errorf("a page asked the node holding the seat %v, want a live row by its turn and the head by none",
			asks[0])
	}

	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": continues.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	row := read.(queries.SessionAnswer).Session
	if row.Continuable == nil || !*row.Continuable || row.ContinuesIn != sandbox.ContinuesInAnyBox ||
		row.NotContinuable != "" {
		t.Errorf("the session read alone is %s, want continuable in any box", decided(row))
	}
	if listed, got := runs.reads(); listed != 1 || got != 2 {
		t.Errorf("after a page and a session read alone, the seat's runs were listed %d times and a "+
			"turn's read %d, want the page's one listing and two turns read alone", listed, got)
	}
	if asks := targets.asks(); len(asks) != 2 || len(asks[1]) != 1 ||
		asks[1][0] != (sandbox.RelaunchAsk{Turn: continues.Turn, Archive: sandbox.ArchiveOf(continues.Kept())}) {
		t.Errorf("after a page and a session read alone, the holder was asked %v, want the session read "+
			"alone asking about its own turn's session once", asks)
	}
}

// A LIVE SESSION WHOSE RUN RECORD CANNOT BE READ IS UNDECIDED, NEVER A NO, AND
// AN ENDED ONE NEEDS NO RECORD. A coordination store that does not answer, a
// node wired with none to read, and a node wired with no way to ask the node
// holding the seat what a relaunch is launched on each leave every live turn's
// row with `continuable` null and its reason saying why — a no would tell a
// person a session starts over that may well be continued — while the page is
// still answered, and an ended turn's row of no conversation is decided off the
// log alone. A page of ended turns reads nothing from the store at all.
//
// Mutation: answer an unread record as a run that is over, read a node with no
// run records as one whose runs are all over, read a node that asks no holder
// as one whose relaunch continues nothing, fail the page, or read the store for
// a page of ended turns, and a check goes red.
func TestALiveSessionWhoseRunCannotBeReadIsUndecided(t *testing.T) {
	t.Parallel()
	live, liveRun := liveSession(executor.ClaudeCodeName)
	ended, _ := liveSession(executor.ClaudeCodeName)
	ended.ConversationKey = ""
	ended.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed}
	down := &stubRuns{runs: []sandbox.PendingRun{liveRun}, err: errors.New("the coordination store is down")}
	unwired := continuableSources(t, []executordomain.Session{live, ended}, nil)
	unwired.SessionRuns = nil
	unasked := continuableSources(t, []executordomain.Session{live, ended},
		&stubRuns{runs: []sandbox.PendingRun{liveRun}})
	unasked.RelaunchTargets = nil
	for name, s := range map[string]queries.Sources{
		"a store that does not answer": continuableSources(t, []executordomain.Session{live, ended}, down),
		"a node with no run records":   unwired,
		"a node that asks no holder":   unasked,
	} {
		page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
		if err != nil {
			t.Fatalf("%s: a page whose run records could not be read: %v, want the page answered", name, err)
		}
		for turn, row := range rowsByTurn(t, page) {
			switch turn {
			case live.Turn:
				if row.Continuable != nil || row.NotContinuable == "" {
					t.Errorf("%s: the live turn's row is %s, want undecided, saying why", name, decided(row))
				}
			case ended.Turn:
				if row.Continuable == nil || *row.Continuable ||
					!strings.Contains(row.NotContinuable, "served no conversation") {
					t.Errorf("%s: the ended turn's row is %s, want no, off the log alone", name, decided(row))
				}
			}
		}
		read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": live.Turn})
		if err != nil {
			t.Fatalf("%s: a session whose run record could not be read: %v, want it answered", name, err)
		}
		if row := read.(queries.SessionAnswer).Session; row.Continuable != nil {
			t.Errorf("%s: the session read alone is %s, want undecided", name, decided(row))
		}
	}

	quiet := &stubRuns{err: errors.New("the coordination store is down")}
	if _, err := askAsSeat(t, continuableSources(t, []executordomain.Session{ended}, quiet), "ana",
		"executor_sessions", nil); err != nil {
		t.Fatalf("a page of ended turns: %v", err)
	}
	if listed, got := quiet.reads(); listed != 0 || got != 0 {
		t.Errorf("a page of ended turns read the store %d and %d times, want none", listed, got)
	}
}

// A PARKED RUN'S SESSION IS CONTINUED IN ITS OWN BOX WHERE NO OTHER BOX WOULD.
// A person's answer carries a parked run on in the box it parked in, continuing
// the session that box holds whatever became of its capture — so a run whose
// kept capture no relaunch restores (written by a release no other box
// continues, or not kept at all) is still continuable, only in its parked box,
// with the reason no other box would, its answer recorded or its answer's claim
// under way as much as parked; and not where the relaunch could not continue in
// that box: its box gone (a pause past its time-to-live), its record naming no
// session, the relaunch's member another harness or in another cell, a box on
// an engine host that is not the host of the node holding the seat — the seat
// moved since its job launched — or a box that ran another session of the turn
// than the row's, started in its place, each saying why.
//
// Mutation: decide the row on the kept capture alone, read a box the relaunch
// would not continue in as one it would — skip its host, or the session it
// holds — or read an answer's claim as a run no answer carries on, and a case
// goes red.
func TestAParkedRunsSessionIsContinuedInItsOwnBoxWhereNoOtherBoxWould(t *testing.T) {
	t.Parallel()
	const older = "the session was written by Claude Code 2.1.265, older than 2.1.300"
	refusing := claudeTarget()
	refusing.Restorer = restoring{harness: executor.ClaudeCodeName, why: older}
	elsewhere := refusing
	elsewhere.Placement = sandbox.Container
	other := refusing
	other.Harness = executor.OpenCodeName
	moved := refusing
	moved.Node = "node-b"
	for name, tc := range map[string]struct {
		run       func(*sandbox.PendingRun)
		target    sandbox.RelaunchTarget
		inParked  bool
		continues bool
		why       string
	}{
		"its kept capture, in any box":   {target: claudeTarget(), continues: true},
		"a release no other box resumes": {target: refusing, continues: true, inParked: true},
		"a capture that was not kept": {target: claudeTarget(), continues: true, inParked: true,
			run: func(r *sandbox.PendingRun) {
				refused := *r.Kept
				refused.CaptureSeq, refused.State, refused.Reason = 2, sandbox.CaptureRefused, "past the seat's budget"
				r.Capture = &refused
			}},
		"its answer recorded": {target: refusing, continues: true, inParked: true,
			run: func(r *sandbox.PendingRun) { r.Status = sandbox.StatusAnswered }},
		"its answer's claim under way": {target: refusing, continues: true, inParked: true,
			run: func(r *sandbox.PendingRun) {
				final := *r.Kept
				final.Final = true
				r.Status, r.Capture, r.Kept = sandbox.StatusResumed, &final, &final
				r.Answer = &sandbox.RecordedAnswer{EventIDs: []string{"ev-1"}}
			}},
		"its answer's claim under way, its job having kept nothing": {target: claudeTarget(),
			continues: true, inParked: true, why: "kept no capture",
			run: func(r *sandbox.PendingRun) {
				r.Status, r.Capture, r.Kept = sandbox.StatusResumed, nil, nil
				r.Answer = &sandbox.RecordedAnswer{EventIDs: []string{"ev-1"}}
			}},
		"a box its pause reclaimed": {target: refusing, why: "is gone",
			run: func(r *sandbox.PendingRun) { r.Status, r.SandboxID = sandbox.StatusReseed, "" }},
		"a record naming no session": {target: refusing, why: "names no session",
			run: func(r *sandbox.PendingRun) { r.SessionID = "" }},
		"a member in another cell":                     {target: elsewhere, why: "in the container cell"},
		"a member of another harness":                  {target: other, why: "launched on " + executor.OpenCodeName},
		"a box on another node's host, the seat moved": {target: moved, why: "cannot continue in it"},
		"a box that ran another session of the turn": {target: refusing, why: "another session of the turn",
			run: func(r *sandbox.PendingRun) {
				r.Launch.FreshSessionID, r.Launch.SessionStart = "f-1", sandbox.SessionStartFresh
			}},
		"a box whose word never reached its record": {target: refusing, why: "never said",
			run: func(r *sandbox.PendingRun) { r.Launch.FreshSessionID = "f-1" }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, run := liveSession(executor.ClaudeCodeName)
			if tc.run != nil {
				tc.run(&run)
			}
			s := continuableSources(t, []executordomain.Session{row}, &stubRuns{runs: []sandbox.PendingRun{run}})
			s.RelaunchTargets = targetOf(tc.target)
			page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
			if err != nil {
				t.Fatalf("executor_sessions: %v", err)
			}
			got := rowsByTurn(t, page)[row.Turn]
			want := sandbox.ContinuesInAnyBox
			if tc.inParked {
				want = sandbox.ContinuesInParkedBox
			}
			switch {
			case got.Continuable == nil:
				t.Fatalf("the row is %s, want decided", decided(got))
			case *got.Continuable != tc.continues:
				t.Fatalf("the row is %s, want continues %v", decided(got), tc.continues)
			case tc.continues && got.ContinuesIn != want:
				t.Fatalf("the row is %s, want continuable in %s", decided(got), want)
			case tc.inParked && got.NotContinuable == "":
				t.Errorf("the row is %s, want why no other box would", decided(got))
			case !tc.inParked && tc.continues && got.NotContinuable != "":
				t.Errorf("the row is %s, want no reason", decided(got))
			case !strings.Contains(got.NotContinuable, tc.why):
				t.Errorf("the row is %s, want it saying %q", decided(got), tc.why)
			}
		})
	}
}

// WHETHER A RELAUNCH CONTINUES IS UNDECIDED WHERE A RELAUNCH WOULD NOT DECIDE
// NOW EITHER, never a no. The job a relaunch would follow still working, or
// being collected — its session is taken by the read that collects it — the
// capture of that job still being kept, which a relaunch waits for; what a
// relaunch is launched on not to be said now — every member of the seat's
// chain sitting out its plan, where which of them takes the relaunch decides
// it, or the node holding the seat not answering; a round lost a second time,
// carried on in a session of its own unless the job had submitted its work,
// which its resume decides; and a collected job whose session no other box
// continues, which the resume of its collection carries on in its box only
// where its sign-in was replaced while it ran: each is `continuable` null,
// saying why.
//
// Mutation: read a job still working as one that left no capture, a capture
// still being kept as one not kept, a target that cannot be said as one with no
// runner, a second loss as a round continued from its capture, or a collected
// job's resume as one that carries nothing on in its box, and a case goes red.
func TestWhetherARelaunchContinuesIsUndecidedWhereARelaunchWouldNotDecideEither(t *testing.T) {
	t.Parallel()
	sittingOut := sandbox.RelaunchTarget{Undecided: true,
		Why: "every member of the seat's executor chain is sitting out its plan"}
	silent := sandbox.RelaunchTarget{Undecided: true,
		Why: "node-a, which holds the seat, did not say within 2s what a relaunch is launched on"}
	refusing := claudeTarget()
	refusing.Restorer = restoring{harness: executor.ClaudeCodeName, why: "a release no other box resumes"}
	for name, tc := range map[string]struct {
		run    func(*sandbox.PendingRun, time.Time)
		target sandbox.RelaunchTarget
		why    string
	}{
		"a job still running": {target: claudeTarget(), why: "still working",
			run: func(r *sandbox.PendingRun, _ time.Time) { r.Status, r.LaunchSeq = sandbox.StatusRunning, 3 }},
		"a job being collected": {target: claudeTarget(), why: "being collected",
			run: func(r *sandbox.PendingRun, _ time.Time) { r.Status = sandbox.StatusResumed }},
		"a launch not yet suspended": {target: claudeTarget(), why: "still working",
			run: func(r *sandbox.PendingRun, _ time.Time) { r.Status, r.LaunchSeq = sandbox.StatusLaunching, 3 }},
		"a capture still being kept": {target: claudeTarget(), why: "still being kept",
			run: func(r *sandbox.PendingRun, now time.Time) {
				begun := *r.Kept
				begun.CaptureSeq, begun.State, begun.At = 2, sandbox.CaptureBegun, now.Add(-time.Second)
				r.Capture = &begun
			}},
		"every member sitting out": {target: sittingOut, why: "sitting out its plan"},
		"the holder not answering": {target: silent, why: "did not say"},
		"a round lost a second time": {target: claudeTarget(), why: "lost a second time",
			run: func(r *sandbox.PendingRun, _ time.Time) {
				r.Status, r.Launch.Collected, r.RoundLosses = sandbox.StatusRunning, true, 1
				r.Lost = &sandbox.JobLoss{Cause: sandbox.LossBoxGone}
			}},
		"a collected job's resume": {target: refusing, why: "sign-in was replaced",
			run: func(r *sandbox.PendingRun, _ time.Time) {
				r.Status, r.Launch.Collected = sandbox.StatusRunning, true
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
			row, run := liveSession(executor.ClaudeCodeName)
			if tc.run != nil {
				tc.run(&run, now)
			}
			s := continuableSources(t, []executordomain.Session{row}, &stubRuns{runs: []sandbox.PendingRun{run}})
			s.RelaunchTargets = targetOf(tc.target)
			s.Now = func() time.Time { return now }
			page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
			if err != nil {
				t.Fatalf("executor_sessions: %v", err)
			}
			if got := rowsByTurn(t, page)[row.Turn]; got.Continuable != nil ||
				!strings.Contains(got.NotContinuable, tc.why) || got.ContinuesIn != "" {
				t.Errorf("the row is %s, want undecided saying %q", decided(got), tc.why)
			}
		})
	}
}

// A CLAIM IS DECIDED ONCE ITS READ HAS TAKEN THE JOB'S FINAL CAPTURE, OR ITS
// TURN HAS SAID WHAT IT DOES. The read a claim makes takes the job's session
// first, and the claim's row stays resumed for the whole resumed turn, its review
// included — which is when a person most wants to know what a send-back would
// continue. So once that final capture is taken and kept, the row is decided as
// any other: continued from it in any box. And once the turn has begun, saying
// what it does with the round, so is one whose capture no relaunch's runner
// restores, and one whose read took no capture at all: a turn that collects its
// job is reviewed, and its send-back continues nothing in the job's box; one
// that carries the round on in that box continues it there. Before either, it is
// still undecided.
//
// Mutation: read every resumed row as a job still being collected, read a turn
// that said what it does as one that has not, or read a collecting turn as one
// whose relaunch continues in the job's box, and a case goes red.
func TestAClaimIsDecidedOnceItsReadOrItsTurnHasSaid(t *testing.T) {
	t.Parallel()
	refusing := claudeTarget()
	refusing.Restorer = restoring{harness: executor.ClaudeCodeName, why: "a release no other box resumes"}
	for name, tc := range map[string]struct {
		carry     sandbox.ResumeCarry
		final     bool
		target    sandbox.RelaunchTarget
		undecided bool
		continues bool
		in        sandbox.ContinuesIn
		why       string
	}{
		"its final capture kept, which the relaunch's member restores": {final: true, target: claudeTarget(),
			continues: true, in: sandbox.ContinuesInAnyBox},
		"its turn collecting the job, its capture no other box continues": {final: true, target: refusing,
			carry: sandbox.ResumeCollects, why: "collects its job"},
		"its turn carrying the round on in its box": {final: true, target: refusing,
			carry: sandbox.ResumeCarriesOn, continues: true, in: sandbox.ContinuesInParkedBox,
			why: "no other box resumes"},
		"its turn begun, its read having taken no capture": {target: claudeTarget(),
			carry: sandbox.ResumeCollects, continues: true, in: sandbox.ContinuesInAnyBox},
		"its turn not begun, its capture no other box continues": {final: true, target: refusing,
			undecided: true, why: "as its turn begins"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, run := liveSession(executor.ClaudeCodeName)
			capture := *run.Kept
			capture.Final = tc.final
			run.Status, run.Capture, run.Kept = sandbox.StatusResumed, &capture, &capture
			run.Launch.ResumeCarry = tc.carry
			s := continuableSources(t, []executordomain.Session{row}, &stubRuns{runs: []sandbox.PendingRun{run}})
			s.RelaunchTargets = targetOf(tc.target)
			page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
			if err != nil {
				t.Fatalf("executor_sessions: %v", err)
			}
			got := rowsByTurn(t, page)[row.Turn]
			switch {
			case tc.undecided && got.Continuable != nil:
				t.Errorf("the claim's row is %s, want undecided", decided(got))
			case !tc.undecided && (got.Continuable == nil || *got.Continuable != tc.continues):
				t.Errorf("the claim's row is %s, want continues %v", decided(got), tc.continues)
			case got.ContinuesIn != tc.in:
				t.Errorf("the claim's row is %s, want it continued in %q", decided(got), tc.in)
			case !strings.Contains(got.NotContinuable, tc.why):
				t.Errorf("the claim's row is %s, want it saying %q", decided(got), tc.why)
			}
		})
	}
}

// EACH ROW IS DECIDED BY ITS OWN TURN'S TARGET. The node holding the seat answers
// what a relaunch of each turn would be launched on from the revision that
// turn's relaunch runs on — a turn whose resumed segment is running there, from
// the revision that segment was admitted under, whatever an apply put live since
// — so two turns of one seat on one page can be targeted at two members, and
// each row is decided by its own.
//
// Mutation: decide every row of a page by one target, and one of the two rows
// is decided for a member its relaunch is never launched on.
func TestEachRowIsDecidedByItsOwnTurnsTarget(t *testing.T) {
	t.Parallel()
	open, openRun := liveSession(executor.ClaudeCodeName)
	final := *openRun.Kept
	final.Final = true
	openRun.Status, openRun.Capture, openRun.Kept = sandbox.StatusResumed, &final, &final
	openRun.Launch.ResumeCarry = sandbox.ResumeCollects
	parked, parkedRun := liveSession(executor.ClaudeCodeName)
	s := continuableSources(t, []executordomain.Session{open, parked},
		&stubRuns{runs: []sandbox.PendingRun{openRun, parkedRun}})
	// THE REVISION LIVE ON THE HOLDER NOW launches the seat on OpenCode in the
	// container cell; the open segment relaunches on the one it was admitted
	// under, Claude Code in the direct cell.
	targets := targetOf(sandbox.RelaunchTarget{Harness: executor.OpenCodeName, Placement: sandbox.Container,
		Node: holder, Restorer: restoring{harness: executor.OpenCodeName}})
	targets.byTurn = map[string]sandbox.RelaunchTarget{open.Turn: claudeTarget()}
	s.RelaunchTargets = targets
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	rows := rowsByTurn(t, page)
	if got := rows[open.Turn]; got.Continuable == nil || !*got.Continuable ||
		got.ContinuesIn != sandbox.ContinuesInAnyBox {
		t.Errorf("the open segment's row is %s, want continuable in any box by the member its send-back "+
			"is launched on", decided(got))
	}
	if got := rows[parked.Turn]; got.Continuable == nil || *got.Continuable ||
		!strings.Contains(got.NotContinuable, executor.OpenCodeName) {
		t.Errorf("the parked turn's row is %s, want not continuable by the live revision's member",
			decided(got))
	}
}

// hidingRuns is a fleet's run records whose listing passes over the turns in
// hidden — as the store's listing passes over a record of a status this build
// does not know, and one it could not decode — while a read of one turn finds
// it.
type hidingRuns struct {
	*stubRuns
	hidden map[string]bool
}

func (h hidingRuns) ListActiveForSeat(ctx context.Context, handle string) ([]sandbox.PendingRun, error) {
	listed, err := h.stubRuns.ListActiveForSeat(ctx, handle)
	var out []sandbox.PendingRun
	for _, run := range listed {
		if !h.hidden[run.TurnID] {
			out = append(out, run)
		}
	}
	return out, err
}

// A LIVE ROW THE LISTING DOES NOT HOLD IS READ ALONE, and decided as a session
// read alone decides it: a listing passes over a record of a status this build
// does not know and one it could not decode, and neither is a run that is over —
// read as one, the page said the turn had ended while the session read alone
// said it continued.
//
// Mutation: decide a live row the listing does not hold as a run that is over,
// and the page and the session read alone disagree.
func TestALiveRowTheListingDoesNotHoldIsReadAlone(t *testing.T) {
	t.Parallel()
	row, run := liveSession(executor.ClaudeCodeName)
	run.Status = "handed_over"
	runs := hidingRuns{stubRuns: &stubRuns{runs: []sandbox.PendingRun{run}}, hidden: map[string]bool{row.Turn: true}}
	s := continuableSources(t, []executordomain.Session{row}, nil)
	s.SessionRuns = runs
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	listed := rowsByTurn(t, page)[row.Turn]
	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	alone := read.(queries.SessionAnswer).Session
	if decided(listed) != decided(alone) || listed.Continuable == nil || !*listed.Continuable {
		t.Errorf("the page says %s and the session read alone %s, want both continuable", decided(listed),
			decided(alone))
	}
}

// levelledSessions is the executor log as two copies answer it: the copy a page
// is read from, and the row a read at the relaunch's level is answered with —
// which, where through names it, has applied the turn's newest capture, or its
// end — beside the head of its conversation that read names (heads, by turn);
// and how many reads at that level it was asked for.
type levelledSessions struct {
	*stubSessions
	through map[string]executordomain.Session
	heads   map[string]string

	mu      sync.Mutex
	reReads int
}

func (l *levelledSessions) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {

	if fresh.Level == executordomain.RelaunchRead().Level {
		l.mu.Lock()
		l.reReads++
		l.mu.Unlock()
		if row, ok := l.through[turn]; ok {
			return executordomain.SessionRead{Session: row, Head: l.heads[turn], Complete: true}, nil
		}
	}
	return l.stubSessions.Session(ctx, handle, turn, fresh)
}

// readAgain is how many reads at the relaunch's level the log was asked for.
func (l *levelledSessions) readAgain() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reReads
}

// A LIVE ROW IS DECIDED ON THE LOG AS A RELAUNCH READS IT. A page read from a
// copy of the executor log behind the run's record holds the turn's older
// capture where the record keeps the newer — and a relaunch, which reads the
// row at linearizable, continues the newer. So the row is read again at the
// relaunch's level, once, and decided on that: continuable in any box with no
// reason, where the behind copy alone read it as a log naming another capture.
// The run's job was collected and its box handed back, so nothing else would
// continue its session.
//
// Mutation: decide the row on the page's own read, and it says no.
func TestALiveRowIsDecidedOnTheLogAsARelaunchReadsIt(t *testing.T) {
	t.Parallel()
	behind, run := liveSession(executor.ClaudeCodeName)
	newer := *run.Kept
	newer.CaptureSeq = 2
	run.Capture, run.Kept = &newer, &newer
	run.Status, run.Launch.Collected, run.SandboxID = sandbox.StatusRunning, true, ""
	applied := behind
	applied.CaptureSeq = 2
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, nil, store)
	onLog := &levelledSessions{stubSessions: &stubSessions{rows: []executordomain.Session{behind}},
		through: map[string]executordomain.Session{behind.Turn: applied}}
	s.ExecutorSessions = onLog
	s.SessionRuns = &stubRuns{runs: []sandbox.PendingRun{run}}
	s.RelaunchTargets = targetOf(claudeTarget())
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	got := rowsByTurn(t, page)[behind.Turn]
	if got.Continuable == nil || !*got.Continuable || got.ContinuesIn != sandbox.ContinuesInAnyBox ||
		got.NotContinuable != "" {
		t.Errorf("a row read from a copy behind the run's record is %s, want continuable in any box with "+
			"no reason, as a relaunch reading the log at its level continues it", decided(got))
	}
	if n := onLog.readAgain(); n != 1 {
		t.Errorf("the row was read again at the relaunch's level %d times, want once", n)
	}
}

// A ROW READ AGAIN IS DECIDED ON THE HEAD THAT READ NAMED. A page read from a
// copy of the executor log behind the run's record holds a live turn's older
// capture, and the head of its conversation as that copy saw it: the turn
// before. The row is read again at the relaunch's level, and the turn's end
// landed in between: the row comes back ended, its conversation's head in the
// snapshot that answered it. Decided on that read's head, it is the session
// the conversation's next turn continues, in any box; decided on the page's,
// it was a definite no naming the turn before, whose row that end may have
// absorbed.
//
// Mutation: decide a row read again on the head its page named, and it says no.
func TestARowReadAgainIsDecidedOnTheHeadItsOwnReadNamed(t *testing.T) {
	t.Parallel()
	before := endedOn("chat:room", 1)
	behind, run := liveSession(executor.ClaudeCodeName)
	behind.ConversationKey = "chat:room"
	newer := *run.Kept
	newer.CaptureSeq = 2
	run.Capture, run.Kept = &newer, &newer
	run.Status, run.Launch.Collected, run.SandboxID = sandbox.StatusRunning, true, ""
	ended := behind
	ended.CaptureSeq = 2
	ended.EndedAt = ended.CapturedAt.Add(time.Minute)
	ended.EndedAtPosition = statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1, Seq: 2}
	ended.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, nil, store)
	onLog := &levelledSessions{stubSessions: &stubSessions{rows: []executordomain.Session{behind, before}},
		through: map[string]executordomain.Session{behind.Turn: ended},
		heads:   map[string]string{behind.Turn: behind.Turn}}
	s.ExecutorSessions = onLog
	s.SessionRuns = &stubRuns{runs: []sandbox.PendingRun{run}}
	s.RelaunchTargets = targetOf(claudeTarget())
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	got := rowsByTurn(t, page)[behind.Turn]
	if got.ContinuedBy != sandbox.ContinuedByNextTurn || got.Continuable == nil || !*got.Continuable ||
		got.ContinuesIn != sandbox.ContinuesInAnyBox || got.NotContinuable != "" {
		t.Errorf("a live row read again ended is %s by %q, want continuable by the conversation's next turn "+
			"in any box, as the head its own read named", decided(got), got.ContinuedBy)
	}
	if n := onLog.readAgain(); n != 1 {
		t.Errorf("the row was read again at the relaunch's level %d times, want once", n)
	}
}

// A ROW NAMING ITS RUN'S KEPT CAPTURE IS NOT READ AGAIN, nor is a row the
// decision never reaches. A read at the relaunch's level appends a barrier to
// the executor log, and a row that already names the capture the run's record
// keeps decides as that read would — a newer capture is stamped on the record
// before a byte of it moves — while one whose job still works, or whose turn
// has ended, is decided without its capture at all. So a page of such rows,
// asked every minute, reads the log once, at its own level. And a transcript's
// later page decides nothing again: its first page says it, once, and a later
// one leaves it null, saying so, reading no run record and asking no node.
//
// Mutation: read every live row again at the relaunch's level, or decide a
// later page again, and a check goes red.
func TestARowNamingItsRunsKeptCaptureIsNotReadAgain(t *testing.T) {
	t.Parallel()
	kept, keptRun := liveSession(executor.ClaudeCodeName)
	working, workingRun := liveSession(executor.ClaudeCodeName)
	workingRun.Status, workingRun.LaunchSeq = sandbox.StatusRunning, 3
	ended, _ := liveSession(executor.ClaudeCodeName)
	ended.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, nil, store)
	onLog := &levelledSessions{stubSessions: &stubSessions{rows: []executordomain.Session{kept, working, ended}}}
	s.ExecutorSessions = onLog
	runs := &stubRuns{runs: []sandbox.PendingRun{keptRun, workingRun}}
	s.SessionRuns = runs
	targets := targetOf(claudeTarget())
	s.RelaunchTargets = targets
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	if got := rowsByTurn(t, page)[kept.Turn]; got.Continuable == nil || !*got.Continuable {
		t.Errorf("a row naming its run's kept capture is %s, want continuable", decided(got))
	}
	if n := onLog.readAgain(); n != 0 {
		t.Errorf("a page whose rows name their runs' kept captures read the log again %d times, want none", n)
	}

	_, gotBefore := runs.reads()
	asksBefore := len(targets.asks())
	cursor := kept.Object.Key.String() + "." + strconv.Itoa(executor.TranscriptVersion) + ".5"
	later, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": kept.Turn,
		"cursor": cursor})
	if err != nil {
		t.Fatalf("a transcript's later page: %v", err)
	}
	row := later.(queries.SessionAnswer).Session
	if row.Continuable != nil || !strings.Contains(row.NotContinuable, "first page") {
		t.Errorf("a transcript's later page says %s, want it left to the first page", decided(row))
	}
	if _, got := runs.reads(); got != gotBefore || len(targets.asks()) != asksBefore {
		t.Errorf("a transcript's later page read %d run records and asked the holder %d times, want "+
			"neither", got-gotBefore, len(targets.asks())-asksBefore)
	}
}

// A REASON IS REDACTED BEFORE IT IS SENT. A reason can quote what a box or a
// runner said — a harness's release as it printed it, a refusal's own words —
// and a box holds the seat's credentials, so every reason passes the surface's
// one redaction as every other text of it does.
//
// Mutation: send the decision's reason as it came, and the token is in the
// answer.
func TestAContinuityReasonIsRedacted(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("c", 36)
	row, run := liveSession(executor.ClaudeCodeName)
	run.SandboxID = ""
	target := claudeTarget()
	target.Restorer = restoring{harness: executor.ClaudeCodeName, why: "the box answered " + secret}
	s := continuableSources(t, []executordomain.Session{row}, &stubRuns{runs: []sandbox.PendingRun{run}})
	s.RelaunchTargets = targetOf(target)
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	got := rowsByTurn(t, page)[row.Turn]
	if strings.Contains(got.NotContinuable, secret) || !strings.Contains(got.NotContinuable, "the box answered") {
		t.Errorf("the row says %q, want the reason with the credential taken out", got.NotContinuable)
	}
}

// endedOn is ana's ended turn on conversation, its end at seq on the executor
// log, its session written by Claude Code and kept: each made into the row a
// case needs by what it takes off.
func endedOn(conversation string, seq uint64) executordomain.Session {
	row, _ := liveSession(executor.ClaudeCodeName)
	row.ConversationKey = conversation
	row.EndedAt = row.CapturedAt.Add(time.Minute)
	row.EndedAtPosition = statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1, Seq: seq}
	row.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}
	return row
}

// A SCHEDULE'S ROW SAYS WHETHER ITS NEXT FIRE CONTINUES IT. A schedule is a
// conversation of each seat that runs it, every fire a turn of it, so a fire's
// row is decided as any conversation's is — by the decision its next fire's
// first launch takes — and said as the schedule's next fire (`next_fire`), in
// the schedule's words: the newest fire's session in the schedule's line is
// continuable in any box, and a fire that parked on a question — which read
// the schedule before that fire did and ended after it — names the fire the
// next one is handed instead, on its page and read alone. A chat
// conversation's head on the same page stays its next turn's.
//
// Mutation: decide a schedule's row as `next_turn`, word it as a
// conversation's, or tell a chat conversation's row as a schedule's, and a
// check goes red; order the stub's head by its end alone (headOf), as the
// domain's head statement no longer does, and the parked fire is named the
// head on the page and alone.
func TestAScheduleRowSaysWhetherItsNextFireContinuesIt(t *testing.T) {
	t.Parallel()
	schedule := notify.ScheduleConversation(types.ScheduleScopeRole, "a-1", "health-check")
	readAt := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1, Seq: seq}
	}
	// THE PARKED FIRE read the schedule at 1 and ended last, at 4; THE HEAD
	// read it at 2, once a fire beside the parked one had ended, and ended at
	// 3.
	parked, head, chat := endedOn(schedule, 4), endedOn(schedule, 3), endedOn("chat:room", 5)
	parked.ReadThrough, head.ReadThrough = readAt(1), readAt(2)
	s := continuableSources(t, []executordomain.Session{chat, head, parked}, nil)
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	rows := rowsByTurn(t, page)
	if got := rows[head.Turn]; got.ContinuedBy != sandbox.ContinuedByNextFire || got.Continuable == nil ||
		!*got.Continuable || got.ContinuesIn != sandbox.ContinuesInAnyBox {
		t.Errorf("the schedule's newest session is %s by %q, want its next fire's, in any box", decided(got),
			got.ContinuedBy)
	}
	handed := "the schedule's next fire is handed fire " + head.Turn +
		"'s session, the newest in the schedule's line"
	if got := rows[parked.Turn]; got.ContinuedBy != sandbox.ContinuedByNextFire || got.Continuable == nil ||
		*got.Continuable || !strings.Contains(got.NotContinuable, handed) {
		t.Errorf("the parked fire's session is %s by %q, want its next fire's, naming the fire it is handed",
			decided(got), got.ContinuedBy)
	}
	if got := rows[chat.Turn]; got.ContinuedBy != sandbox.ContinuedByNextTurn || got.Continuable == nil ||
		!*got.Continuable {
		t.Errorf("a chat conversation's head is %s by %q, want its next turn's", decided(got), got.ContinuedBy)
	}

	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": parked.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	if alone := read.(queries.SessionAnswer).Session; alone.ContinuedBy != sandbox.ContinuedByNextFire ||
		alone.Continuable == nil || *alone.Continuable || !strings.Contains(alone.NotContinuable, handed) {
		t.Errorf("the parked fire's session read alone is %s by %q, want its next fire's, naming the fire it "+
			"is handed", decided(alone), alone.ContinuedBy)
	}
}

// A ROW SAYS WHETHER THE NEXT TURN CONTINUES IT, as that turn's first launch
// decides it, on the head the read that answered the row named: the head of a
// conversation is continuable by the next turn in any box, asked of the node
// holding the seat by NO turn, its runner restoring it; an older row names the
// head the next turn is handed instead; a row that names no session says which
// of three reasons — nothing captured, its newest capture not kept (that
// capture's own reason redacted, on the ending as much as in the sentence), or
// the seat's budget's release — and a turn that served no conversation is
// continued by no launch. A live turn whose first job was lost before it kept
// anything says its relaunch continues the session its first launch was
// handed, names it (`base_turn`), and asks the holder about that session's
// archive. A head whose launch cannot be said is undecided, never a no. And a
// session read alone is decided as its page decides it, on its own read's head.
//
// Mutation: decide an ended row as 6b did (a relaunch's no), by its own turn's
// target or by a head the page did not name; read an unknown target as a no;
// drop a reason for a row with no session, or send an unkept capture's reason
// unredacted; or ask the holder about a begun turn's own empty archive, and a
// check goes red.
func TestARowSaysWhetherTheNextTurnContinuesIt(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("d", 36)
	older := endedOn("chat:room", 1)
	unkept := endedOn("chat:room", 2)
	unkept.Object = objstore.Object{}
	unkept.Ending.Unkept = &executorkeep.Unkept{LaunchSeq: 3, CaptureSeq: 1,
		Reason: "refused: the store answered " + secret}
	head := endedOn("chat:room", 3)
	released := endedOn("chat:room", 4)
	released.Object = objstore.Object{}
	blank := endedOn("chat:room", 5)
	blank.Object, blank.CapturedAt, blank.LaunchSeq, blank.CaptureSeq = objstore.Object{}, time.Time{}, 0, 0
	keyless := endedOn("", 6)
	begun := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), ConversationKey: "chat:room",
		BaseTurn: head.Turn}
	base := sandbox.BaseSession{Turn: head.Turn, Object: head.Object.Key.String(),
		ObjectHash: string(head.Object.Hash), ObjectSize: head.Object.Size, Harness: head.Harness,
		SessionID: head.SessionID, ArchiveFormat: head.ArchiveFormat}
	begunRun := sandbox.PendingRun{TurnID: begun.Turn, AgentHandle: "ana", Status: sandbox.StatusRunning,
		LaunchSeq: 1, Launch: sandbox.LaunchRecord{Collected: true},
		Lost: &sandbox.JobLoss{Cause: sandbox.LossBoxGone},
		Base: &sandbox.ConversationBase{ReadThrough: 9, Absorbs: []string{head.Turn}, Session: &base}}
	s := continuableSources(t, []executordomain.Session{begun, keyless, blank, released, head, unkept, older},
		&stubRuns{runs: []sandbox.PendingRun{begunRun}})

	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	rows := rowsByTurn(t, page)
	for name, tc := range map[string]struct {
		turn      string
		by        sandbox.ContinuedBy
		continues bool
		why       string
	}{
		"its conversation's head": {turn: head.Turn, by: sandbox.ContinuedByNextTurn, continues: true},
		"an older ended turn": {turn: older.Turn, by: sandbox.ContinuedByNextTurn,
			why: "is handed turn " + head.Turn + "'s session"},
		"a turn that captured nothing": {turn: blank.Turn, by: sandbox.ContinuedByNextTurn,
			why: "nothing of this turn's session was captured"},
		"a turn whose newest capture was not kept": {turn: unkept.Turn, by: sandbox.ContinuedByNextTurn,
			why: "(launch 3, capture 1) was not kept: refused: the store answered"},
		"a session the seat's budget released": {turn: released.Turn, by: sandbox.ContinuedByNextTurn,
			why: "budget released"},
		"a turn that served no conversation": {turn: keyless.Turn, why: "served no conversation"},
		"a turn whose first job was lost before it kept anything": {turn: begun.Turn, by: sandbox.ContinuedByRelaunch,
			why: "a relaunch of the turn in a box of its own continues turn " + head.Turn + "'s session instead"},
	} {
		row, ok := rows[tc.turn]
		switch {
		case !ok:
			t.Errorf("%s: the page holds no row of its turn", name)
		case row.ContinuedBy != tc.by || row.Continuable == nil:
			t.Errorf("%s: %s by %q, want decided, about %q", name, decided(row), row.ContinuedBy, tc.by)
		case *row.Continuable != tc.continues || !strings.Contains(row.NotContinuable, tc.why) ||
			(tc.continues && (row.NotContinuable != "" || row.ContinuesIn != sandbox.ContinuesInAnyBox)) ||
			(!tc.continues && row.ContinuesIn != ""):
			t.Errorf("%s: %s, want continues %v saying %q", name, decided(row), tc.continues, tc.why)
		}
	}
	if got := rows[begun.Turn]; got.BaseTurn != head.Turn {
		t.Errorf("the begun turn's row names %q as the session its first launch was handed, want %s",
			got.BaseTurn, head.Turn)
	}
	if got := rows[unkept.Turn]; strings.Contains(got.NotContinuable, secret) || got.Ending == nil ||
		got.Ending.Unkept == nil || strings.Contains(got.Ending.Unkept.Reason, secret) ||
		!strings.Contains(got.Ending.Unkept.Reason, redact.Marker) {
		t.Errorf("an unkept capture's reason reaches the reader as %q and %+v, want it redacted in both",
			got.NotContinuable, got.Ending)
	}
	targets := s.RelaunchTargets.(*stubTargets)
	want := []sandbox.RelaunchAsk{{Turn: begun.Turn, Archive: sandbox.SessionArchive{Harness: head.Harness,
		SessionID: head.SessionID, Format: head.ArchiveFormat}}, {Archive: sandbox.ArchiveOf(head.Kept())}}
	if asks := targets.asks(); len(asks) != 1 || !slices.Equal(asks[0], want) {
		t.Errorf("a page asked the node holding the seat %v, want once, about the begun turn's base by its "+
			"turn and the head by none: %v", asks, want)
	}

	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": head.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	alone := read.(queries.SessionAnswer).Session
	if decided(alone) != decided(rows[head.Turn]) || alone.ContinuedBy != sandbox.ContinuedByNextTurn {
		t.Errorf("the head read alone is %s by %q, and its page said %s", decided(alone), alone.ContinuedBy,
			decided(rows[head.Turn]))
	}
	if asks := targets.asks(); len(asks) != 2 || !slices.Equal(asks[1],
		[]sandbox.RelaunchAsk{{Archive: sandbox.ArchiveOf(head.Kept())}}) {
		t.Errorf("the head read alone asked the node holding the seat %v, want its session by no turn", asks)
	}
	none, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": unkept.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	if got := none.(queries.SessionAnswer); got.State != executorread.TranscriptNone ||
		!strings.Contains(got.Why, "was not kept") || strings.Contains(got.Why, secret) {
		t.Errorf("a session whose newest capture was not kept reads %s: %q, want none, saying why, redacted",
			got.State, got.Why)
	}

	sittingOut := continuableSources(t, []executordomain.Session{head}, nil)
	sittingOut.RelaunchTargets = targetOf(sandbox.RelaunchTarget{Undecided: true,
		Why: "every member of the seat's executor chain is sitting out its plan"})
	unsaid, err := askAsSeat(t, sittingOut, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	if got := rowsByTurn(t, unsaid)[head.Turn]; got.Continuable != nil ||
		got.ContinuedBy != sandbox.ContinuedByNextTurn || !strings.Contains(got.NotContinuable, "sitting out") {
		t.Errorf("a head whose next turn's member cannot be said is %s, want undecided, saying why", decided(got))
	}
}
