package queries_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
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

// stubTargets is the member a relaunch of each seat's turn would be launched
// on; a seat it does not name has none.
type stubTargets map[string]sandbox.RelaunchTarget

func (s stubTargets) RelaunchTarget(handle string) sandbox.RelaunchTarget { return s[handle] }

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

func (r restoring) CheckCarry(_ context.Context, _ sandbox.Sandbox, archive sandbox.SessionArchive) (sandbox.CarryCheck, error) {
	ok, why := r.Restores(archive)
	return sandbox.CarryCheck{Continues: ok, Why: why}, nil
}

// claudeTarget is a relaunch of ana's turns launched on a Claude Code member in
// the direct cell, whose runner restores what Claude Code wrote.
func claudeTarget() sandbox.RelaunchTarget {
	return sandbox.RelaunchTarget{Harness: executor.ClaudeCodeName, Placement: sandbox.Direct,
		Restorer: restoring{harness: executor.ClaudeCodeName}}
}

// liveSession is ana's live turn whose job, its second, kept its session's
// first capture, as the executor log's row names it and the run's record keeps
// it, written by harness — the job parked on a question, its box held in the
// direct cell, as a collection leaves a job that asked.
func liveSession(harness string) (executordomain.Session, sandbox.PendingRun) {
	turn := uuid.NewString()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	key := objstore.KeyAt(at)
	kept := sandbox.SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: sandbox.CaptureLanded,
		Object: key.String(), At: at}
	row := executordomain.Session{Handle: "ana", Turn: turn, Harness: harness, SessionID: "s-1",
		ArchiveFormat: executor.ClaudeCode{}.ArchiveFormat(), CapturedAt: at,
		Object:    objstore.Object{Key: key, Hash: objstore.Hash(strings.Repeat("a", 64)), Size: 4096},
		LaunchSeq: 2, CaptureSeq: 1}
	return row, sandbox.PendingRun{TurnID: turn, AgentHandle: "ana", Status: sandbox.StatusAwaiting,
		SandboxID: "box-1", Placement: string(sandbox.Direct), Harness: harness, SessionID: "s-1",
		LaunchSeq: 2, Capture: &kept, Kept: &kept}
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
	s.RelaunchTargets = stubTargets{"ana": claudeTarget()}
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

// A SESSION'S ROW SAYS WHETHER A RELAUNCH WOULD CONTINUE IT, AS A RELAUNCH
// DECIDES IT. Asked of the one decision a relaunch takes, over the turn's row
// on the log, its run's record and the member a relaunch of the seat's turn is
// launched on: a live turn whose job kept the capture the row names, which that
// member's runner restores, is continuable in any box, with no reason; a turn
// whose end is owed, and one whose run's ending is decided and its end not yet
// owed, are ending; a session another harness wrote, which the member's runner
// does not take up, is not continued from its capture — and, its parked box of
// that other harness, nowhere; a live turn whose run is over continues nothing,
// and an ended turn's session no later turn continues yet — each not
// continuable, saying why. A whole page reads the coordination store ONCE, and
// once more for the one live row the listing does not hold, and one session read
// alone reads its own turn's record and nothing else.
//
// Mutation: decide a row by anything but sandbox.ReadContinuityOf, read the
// store once per row, ask the runner of the harness that wrote the session
// rather than the relaunch's member, or answer a decided ending as live, and a
// check goes red.
func TestASessionsRowSaysWhetherARelaunchWouldContinueIt(t *testing.T) {
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
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}
	runs := &stubRuns{runs: []sandbox.PendingRun{continuesRun, endingRun, stoppingRun, foreignRun, endedRun}}
	s := continuableSources(t, []executordomain.Session{continues, ending, stopping, foreign, over, ended}, runs)

	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	rows := rowsByTurn(t, page)
	for name, tc := range map[string]struct {
		turn      string
		continues bool
		why       string
	}{
		"a live turn's kept session":           {turn: continues.Turn, continues: true},
		"a turn whose end is owed":             {turn: ending.Turn, why: "is ending"},
		"a turn whose run's ending is decided": {turn: stopping.Turn, why: "is ending"},
		"a session another harness wrote": {turn: foreign.Turn,
			why: "written by " + executor.OpenCodeName + ", and this runner drives " + executor.ClaudeCodeName},
		"a live turn whose run is over": {turn: over.Turn, why: "has ended"},
		"an ended turn's session":       {turn: ended.Turn, why: "no later turn continues its session yet"},
	} {
		row, ok := rows[tc.turn]
		switch {
		case !ok:
			t.Errorf("%s: the page holds no row of its turn", name)
		case row.Continuable == nil:
			t.Errorf("%s: %s, want decided", name, decided(row))
		case *row.Continuable != tc.continues || !strings.Contains(row.NotContinuable, tc.why) ||
			(tc.continues && (row.NotContinuable != "" || row.ContinuesIn != sandbox.ContinuesInAnyBox)) ||
			(!tc.continues && row.ContinuesIn != ""):
			t.Errorf("%s: %s, want continues %v saying %q", name, decided(row), tc.continues, tc.why)
		}
	}
	if listed, got := runs.reads(); listed != 1 || got != 1 {
		t.Errorf("a page of six rows read the seat's runs %d times and a turn's %d, want one listing "+
			"and the one live turn it does not hold", listed, got)
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
}

// A LIVE SESSION WHOSE RUN RECORD CANNOT BE READ IS UNDECIDED, NEVER A NO, AND
// AN ENDED ONE NEEDS NO RECORD. A coordination store that does not answer —
// or a node wired with none to read — leaves every live turn's row with
// `continuable` null and its reason saying the record could not be read — a no
// would tell a person a session starts over that may well be continued — while
// the page is still answered, and an ended turn's row is decided off the log
// alone. A page of ended turns reads nothing from the store at all.
//
// Mutation: answer an unread record as a run that is over, read a node with no
// run records as one whose runs are all over, fail the page, or read the store
// for a page of ended turns, and a check goes red.
func TestALiveSessionWhoseRunCannotBeReadIsUndecided(t *testing.T) {
	t.Parallel()
	live, liveRun := liveSession(executor.ClaudeCodeName)
	ended, _ := liveSession(executor.ClaudeCodeName)
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeFailed}
	down := &stubRuns{runs: []sandbox.PendingRun{liveRun}, err: errors.New("the coordination store is down")}
	unwired := continuableSources(t, []executordomain.Session{live, ended}, nil)
	unwired.SessionRuns = nil
	for name, s := range map[string]queries.Sources{
		"a store that does not answer": continuableSources(t, []executordomain.Session{live, ended}, down),
		"a node with no run records":   unwired,
	} {
		page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
		if err != nil {
			t.Fatalf("%s: a page whose run records could not be read: %v, want the page answered", name, err)
		}
		for turn, row := range rowsByTurn(t, page) {
			switch turn {
			case live.Turn:
				if row.Continuable != nil || !strings.Contains(row.NotContinuable, "could not be read") {
					t.Errorf("%s: the live turn's row is %s, want undecided, saying the record could not "+
						"be read", name, decided(row))
				}
			case ended.Turn:
				if row.Continuable == nil || *row.Continuable ||
					!strings.Contains(row.NotContinuable, "no later turn continues") {
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
// with the reason no other box would; and not once its box is gone (a pause
// past its time-to-live), its record names no session, or the relaunch's member
// is another harness or in another cell, none of which continues in that box.
//
// Mutation: decide the row on the kept capture alone, or read a box the
// relaunch would not continue in as one it would, and a case goes red.
func TestAParkedRunsSessionIsContinuedInItsOwnBoxWhereNoOtherBoxWould(t *testing.T) {
	t.Parallel()
	const older = "the session was written by Claude Code 2.1.265, older than 2.1.300"
	refusing := claudeTarget()
	refusing.Restorer = restoring{harness: executor.ClaudeCodeName, why: older}
	elsewhere := refusing
	elsewhere.Placement = sandbox.Container
	other := refusing
	other.Harness = executor.OpenCodeName
	for name, tc := range map[string]struct {
		run       func(*sandbox.PendingRun)
		target    sandbox.RelaunchTarget
		inParked  bool
		continues bool
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
		"a box its pause reclaimed": {target: refusing,
			run: func(r *sandbox.PendingRun) { r.Status, r.SandboxID = sandbox.StatusReseed, "" }},
		"a record naming no session":  {target: refusing, run: func(r *sandbox.PendingRun) { r.SessionID = "" }},
		"a member in another cell":    {target: elsewhere},
		"a member of another harness": {target: other},
		"a run that is not parked": {target: refusing,
			run: func(r *sandbox.PendingRun) { r.Status, r.Launch.Collected = sandbox.StatusRunning, true }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, run := liveSession(executor.ClaudeCodeName)
			if tc.run != nil {
				tc.run(&run)
			}
			s := continuableSources(t, []executordomain.Session{row}, &stubRuns{runs: []sandbox.PendingRun{run}})
			s.RelaunchTargets = stubTargets{"ana": tc.target}
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
			}
		})
	}
}

// WHETHER A RELAUNCH CONTINUES IS UNDECIDED WHERE A RELAUNCH WOULD NOT DECIDE
// NOW EITHER, never a no. The job a relaunch would follow still working, or
// being collected — its session is taken by the read that collects it — the
// capture of that job still being kept, which a relaunch waits for, and every
// member of the seat's chain sitting out its plan, where which of them takes the
// relaunch decides it: each is `continuable` null, saying why.
//
// Mutation: read a job still working as one that left no capture, a capture
// still being kept as one not kept, or a chain sitting out as one with no
// runner, and a case goes red.
func TestWhetherARelaunchContinuesIsUndecidedWhereARelaunchWouldNotDecideEither(t *testing.T) {
	t.Parallel()
	sittingOut := sandbox.RelaunchTarget{Waiting: true,
		Why: "every member of the seat's executor chain is sitting out its plan"}
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
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
			row, run := liveSession(executor.ClaudeCodeName)
			if tc.run != nil {
				tc.run(&run, now)
			}
			s := continuableSources(t, []executordomain.Session{row}, &stubRuns{runs: []sandbox.PendingRun{run}})
			s.RelaunchTargets = stubTargets{"ana": tc.target}
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
// is read from, behind, and the row a read at the relaunch's level is answered
// with, which has applied the turn's newest capture.
type levelledSessions struct {
	*stubSessions
	through map[string]executordomain.Session
}

func (l levelledSessions) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {

	if row, ok := l.through[turn]; ok && fresh.Level == sandbox.RelaunchRead().Level {
		return executordomain.SessionRead{Session: row, Complete: true}, nil
	}
	return l.stubSessions.Session(ctx, handle, turn, fresh)
}

// A LIVE ROW IS DECIDED ON THE LOG AS A RELAUNCH READS IT. A page read from a
// copy of the executor log behind the run's record holds the turn's older
// capture where the record keeps the newer — and a relaunch, which reads the
// row at linearizable, continues the newer. So the row is decided on the log
// read again at the relaunch's level: continuable, where the behind copy alone
// read it as a log naming another capture.
//
// Mutation: decide the row on the page's own read, and it says no.
func TestALiveRowIsDecidedOnTheLogAsARelaunchReadsIt(t *testing.T) {
	t.Parallel()
	behind, run := liveSession(executor.ClaudeCodeName)
	newer := *run.Kept
	newer.CaptureSeq = 2
	run.Capture, run.Kept = &newer, &newer
	applied := behind
	applied.CaptureSeq = 2
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, nil, store)
	s.ExecutorSessions = levelledSessions{stubSessions: &stubSessions{rows: []executordomain.Session{behind}},
		through: map[string]executordomain.Session{behind.Turn: applied}}
	s.SessionRuns = &stubRuns{runs: []sandbox.PendingRun{run}}
	s.RelaunchTargets = stubTargets{"ana": claudeTarget()}
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	if got := rowsByTurn(t, page)[behind.Turn]; got.Continuable == nil || !*got.Continuable {
		t.Errorf("a row read from a copy behind the run's record is %s, want continuable, as a relaunch "+
			"reading the log at its level continues it", decided(got))
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
	s.RelaunchTargets = stubTargets{"ana": target}
	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	got := rowsByTurn(t, page)[row.Turn]
	if strings.Contains(got.NotContinuable, secret) || !strings.Contains(got.NotContinuable, "the box answered") {
		t.Errorf("the row says %q, want the reason with the credential taken out", got.NotContinuable)
	}
}
