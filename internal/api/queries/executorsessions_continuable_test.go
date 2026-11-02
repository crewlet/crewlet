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

// stubRestorers is this build's runner for each harness it names; a harness it
// does not name has none here that restores its sessions.
type stubRestorers map[string]sandbox.SessionRestorer

func (s stubRestorers) SessionRestorer(harness string) sandbox.SessionRestorer { return s[harness] }

// restoring is a runner that restores every session it is asked about, or none,
// saying why.
type restoring struct{ why string }

func (r restoring) Restores(sandbox.SessionArchive) (bool, string) { return r.why == "", r.why }

func (r restoring) CheckCarry(context.Context, sandbox.Sandbox, sandbox.SessionArchive) (sandbox.CarryCheck, error) {
	return sandbox.CarryCheck{Continues: r.why == "", Why: r.why}, nil
}

// liveSession is ana's live turn whose job, its second, kept its session's
// first capture, as the executor log's row names it and the run's record keeps
// it, written by harness.
func liveSession(harness string) (executordomain.Session, sandbox.PendingRun) {
	turn := uuid.NewString()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	key := objstore.KeyAt(at)
	kept := sandbox.SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: sandbox.CaptureLanded,
		Object: key.String()}
	row := executordomain.Session{Handle: "ana", Turn: turn, Harness: harness, SessionID: "s-1",
		ArchiveFormat: executor.ClaudeCode{}.ArchiveFormat(), CapturedAt: at,
		Object:    objstore.Object{Key: key, Hash: objstore.Hash(strings.Repeat("a", 64)), Size: 4096},
		LaunchSeq: 2, CaptureSeq: 1}
	return row, sandbox.PendingRun{TurnID: turn, AgentHandle: "ana", LaunchSeq: 2, Capture: &kept, Kept: &kept}
}

// continuableSources answers the sessions questions over rows, deciding each
// row's continuity off runs and a runner that restores Claude Code's sessions.
func continuableSources(t *testing.T, rows []executordomain.Session, runs *stubRuns) queries.Sources {
	t.Helper()
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, &stubSessions{rows: rows}, store)
	s.SessionRuns = runs
	s.SessionRestorers = stubRestorers{executor.ClaudeCodeName: restoring{}}
	return s
}

// A SESSION'S ROW SAYS WHETHER A RELAUNCH WOULD CONTINUE IT, AS A RELAUNCH
// DECIDES IT. Asked of the one decision a relaunch takes, over the turn's row
// on the log, its run's record and this build's runner for its harness: a live
// turn whose job kept the capture the row names, of a harness a runner here
// restores, is continuable, with no reason; a turn whose end is owed is ending,
// a harness no runner here restores is never taken up, a live turn whose run
// is over continues nothing, and an ended turn's session no later turn
// continues yet — each not continuable, saying why. A whole page reads the
// coordination store ONCE, and one session read alone reads its own turn's
// record and nothing else.
//
// Mutation: decide a row by anything but sandbox.ContinuityOf, read the store
// once per row, or answer the ending as live, and a check goes red.
func TestASessionsRowSaysWhetherARelaunchWouldContinueIt(t *testing.T) {
	t.Parallel()
	continues, continuesRun := liveSession(executor.ClaudeCodeName)
	ending, endingRun := liveSession(executor.ClaudeCodeName)
	endingRun.OwedEnd = []byte(`{"outcome":"completed"}`)
	foreign, foreignRun := liveSession("codex")
	over, _ := liveSession(executor.ClaudeCodeName)
	ended, endedRun := liveSession(executor.ClaudeCodeName)
	ended.EndedAt = ended.CapturedAt.Add(time.Minute)
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}
	runs := &stubRuns{runs: []sandbox.PendingRun{continuesRun, endingRun, foreignRun, endedRun}}
	s := continuableSources(t, []executordomain.Session{continues, ending, foreign, over, ended}, runs)

	page, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	rows := map[string]queries.SessionRow{}
	for _, row := range page.(queries.SessionsAnswer).Sessions {
		rows[row.Turn] = row
	}
	for name, tc := range map[string]struct {
		turn      string
		continues bool
		why       string
	}{
		"a live turn's kept session":    {turn: continues.Turn, continues: true},
		"a turn whose end is owed":      {turn: ending.Turn, why: "is ending"},
		"a harness no runner restores":  {turn: foreign.Turn, why: "no runner of this build restores"},
		"a live turn whose run is over": {turn: over.Turn, why: "has ended"},
		"an ended turn's session":       {turn: ended.Turn, why: "no later turn continues its session yet"},
	} {
		row, ok := rows[tc.turn]
		switch {
		case !ok:
			t.Errorf("%s: the page holds no row of its turn", name)
		case row.Continuable == nil:
			t.Errorf("%s: undecided (%q), want decided", name, row.NotContinuable)
		case *row.Continuable != tc.continues || !strings.Contains(row.NotContinuable, tc.why) ||
			(tc.continues && row.NotContinuable != ""):
			t.Errorf("%s: continuable %v saying %q, want %v saying %q", name, *row.Continuable,
				row.NotContinuable, tc.continues, tc.why)
		}
	}
	if listed, got := runs.reads(); listed != 1 || got != 0 {
		t.Errorf("a page of five rows read the seat's runs %d times and a turn's %d, want one listing",
			listed, got)
	}

	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": continues.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	row := read.(queries.SessionAnswer).Session
	if row.Continuable == nil || !*row.Continuable || row.NotContinuable != "" {
		t.Errorf("the session read alone is continuable %v (%q), want continuable", row.Continuable,
			row.NotContinuable)
	}
	if listed, got := runs.reads(); listed != 1 || got != 1 {
		t.Errorf("after a page and a session read alone, the seat's runs were listed %d times and a "+
			"turn's read %d, want the page's one listing and the session's own turn once", listed, got)
	}
}

// A LIVE SESSION WHOSE RUN RECORD CANNOT BE READ IS UNDECIDED, NEVER A NO, AND
// AN ENDED ONE NEEDS NO RECORD. A coordination store that does not answer
// leaves every live turn's row with `continuable` null and its reason saying
// the record could not be read — a no would tell a person a session starts
// over that may well be continued — while the page is still answered, and an
// ended turn's row is decided off the log alone. A page of ended turns reads
// nothing from the store at all.
//
// Mutation: answer an unread record as a run that is over, fail the page, or
// read the store for a page of ended turns, and a check goes red.
func TestALiveSessionWhoseRunCannotBeReadIsUndecided(t *testing.T) {
	t.Parallel()
	live, liveRun := liveSession(executor.ClaudeCodeName)
	ended, _ := liveSession(executor.ClaudeCodeName)
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeFailed}
	down := &stubRuns{runs: []sandbox.PendingRun{liveRun}, err: errors.New("the coordination store is down")}
	page, err := askAsSeat(t, continuableSources(t, []executordomain.Session{live, ended}, down), "ana",
		"executor_sessions", nil)
	if err != nil {
		t.Fatalf("a page whose run records could not be read: %v, want the page answered", err)
	}
	for _, row := range page.(queries.SessionsAnswer).Sessions {
		switch row.Turn {
		case live.Turn:
			if row.Continuable != nil || !strings.Contains(row.NotContinuable, "could not be read") {
				t.Errorf("the live turn's row is continuable %v (%q), want undecided, saying the "+
					"record could not be read", row.Continuable, row.NotContinuable)
			}
		case ended.Turn:
			if row.Continuable == nil || *row.Continuable ||
				!strings.Contains(row.NotContinuable, "no later turn continues") {
				t.Errorf("the ended turn's row is continuable %v (%q), want no, off the log alone",
					row.Continuable, row.NotContinuable)
			}
		}
	}
	read, err := askAsSeat(t, continuableSources(t, []executordomain.Session{live}, down), "ana",
		"executor_session", map[string]any{"turn_id": live.Turn})
	if err != nil {
		t.Fatalf("a session whose run record could not be read: %v, want it answered", err)
	}
	if row := read.(queries.SessionAnswer).Session; row.Continuable != nil {
		t.Errorf("the session read alone is continuable %v, want undecided", *row.Continuable)
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
