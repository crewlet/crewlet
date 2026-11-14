package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// appendingStore is a pending store that records each bridged append and
// answers it as a run's log open for job "launch-1" would.
type appendingStore struct {
	sandbox.PendingStore
	mu      sync.Mutex
	appends []sandbox.BridgeAppend
}

func (s *appendingStore) AppendBridgeCall(_ context.Context, _ string, a sandbox.BridgeAppend) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends = append(s.appends, a)
	return a.Launch == "launch-1", nil
}

// steppingMeter is a bridged meter whose total grows by one call each read.
type steppingMeter struct {
	mu sync.Mutex
	n  int
}

func (m *steppingMeter) Total() runner.Bridged {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return runner.Bridged{Aux: auxspend.Spent{Calls: m.n, Input: 100 * m.n, Output: 10 * m.n,
		CacheRead: 30 * m.n, CacheWrite: 2 * m.n}, Workers: m.n, WorkerInput: 500 * m.n,
		WorkerOutput: 40 * m.n}
}

// A SESSION'S CALLS ARE FILED UNDER THE JOB IT WAS BOUND TO, AND CARRY ITS
// METER.
//
// Every append — the first included — names the job the launch bound the
// session to before its box existed, so a call that outlives its job never lands
// on the next job's record as that job's: the store lists it, marked late, in the
// round it was made in, or drops it once a new round has started. And every append
// carries the meter's running total as the row stores it — every figure, the
// cache shares and the workers included — for the segment that resumes from
// the run to pay.
//
// Mutations: leave the first append unnamed (the ledger that learned its job
// from the row), or send an empty total, and this goes red.
func TestABridgeSessionsCallsAreFiledUnderItsBoundJobAndCarryItsSpend(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	meter := &steppingMeter{}
	l := &bridgeLedger{store: store, spend: meter}
	l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
	for range 3 {
		if err := l.Record(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "query_episodes"}}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if len(store.appends) != 3 {
		t.Fatalf("%d appends, want 3", len(store.appends))
	}
	for i, a := range store.appends {
		if a.Launch != "launch-1" {
			t.Errorf("append %d named job %q, want the one the session was bound to", i+1, a.Launch)
		}
	}
	n := 3
	want := sandbox.EngineSpend{
		Aux:     sandbox.AuxTokens{Input: 100 * n, Output: 10 * n, CacheRead: 30 * n, CacheWrite: 2 * n},
		Workers: n, WorkerInput: 500 * n, WorkerOutput: 40 * n,
	}
	if got := store.appends[2].Spent; got != want {
		t.Errorf("the third append carried %+v, want the meter's running total %+v", got, want)
	}
}

// A SESSION NO JOB WAS BOUND TO FILES NOTHING, AND RUNS NOTHING. Through the
// launch that cannot happen — the job is named before the box that makes calls
// exists — so a call arriving on an unbound session is a wiring mistake, and the
// one thing it must not do is reach the store naming no job, which a store that
// read an empty name as "whichever job the row holds" filed on the next job's
// record. Its entry is refused, so the call is not run off the record.
//
// Mutation: send the writes whatever the ledger's binding, and this goes red.
func TestAnUnboundBridgeSessionFilesNothing(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	if err := l.Begin(t.Context(), "run-1", mcpbridge.Begun{Name: "read_page", Seq: 1}); err == nil {
		t.Error("the entry of a call on an unbound session was taken, so the call would run off the record")
	}
	if err := l.Record(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(store.appends) != 0 {
		t.Fatalf("an unbound session sent %d appends to the store: %+v", len(store.appends), store.appends)
	}
}

// A BRIDGED CALL'S ROW SAYS WHEN IT RAN, FOR HOW LONG, IN WHAT ORDER, WHO
// ANSWERED AND WHAT IT WAS — everything the bridge measured, in the log's own
// shape, so a resumed executor's record can place its calls on a timeline, in
// the order the run made them, and name the server each came from — read off
// the log, which is the only place a collection, often another process, can
// read them; and the identity the bridge admitted it under, which a later
// segment's call log counts it by whatever the log keeps of its arguments.
//
// Mutation: drop any of the six from the append, and this goes red.
func TestABridgedCallsRowCarriesItsTimingAndWhoAnswered(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
	started := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	if err := l.Record(t.Context(), "run-1", mcpbridge.Call{
		Call: tools.Call{Name: "list_issues", Args: map[string]any{"repo": "core"}, Output: "[]"},
		Seq:  4, StartedAt: started, Duration: 1500 * time.Millisecond,
		Origin: "mcp", Server: "github", Digest: "d1",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(store.appends) != 1 {
		t.Fatalf("%d appends, want 1", len(store.appends))
	}
	got := store.appends[0].Call
	want := sandbox.BridgeCall{Name: "list_issues", Args: `{"repo":"core"}`, Output: "[]",
		Seq: 4, StartedAt: started, DurationMS: 1500, Origin: "mcp", Server: "github", Digest: "d1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the row is %+v, want %+v", got, want)
	}
}

// WHAT THE LOG KEPT OF A CALL IS WHAT THE RESUMED TURN READS: its identity on
// the turn's record of the call, and the size of the whole arguments and output
// where the log kept less, which the collection's replay decides by — and the
// calls the log dropped, by identity, in the turn's own shape. And of the call
// a person approved, the held call it was made for, which tells the one a
// relaunch's lead tells from one an earlier answer approved.
//
// Mutation: drop the digest or either size on the way to the runner, or the
// tally, and a resumed segment seeds a cut call as another and replays a cut
// submission as the run's; drop the held call's id, and a later attempt at an
// answer lists the call its lead tells among the round's calls again.
func TestWhatTheLogKeptOfACallIsWhatTheResumedTurnReads(t *testing.T) {
	t.Parallel()
	got := bridgedCalls([]sandbox.BridgeCall{{Name: "code_commit_files",
		Args:   `{"content":"(1.0 MiB not kept on the run's record, digest 1a2b3c4d)","path":"a.go"}`,
		Output: "committed", Seq: 3, Digest: "d1", ArgsBytes: 1 << 20, OutputBytes: 70 << 10}})
	if len(got) != 1 || got[0].Digest != "d1" || got[0].ArgsBytes != 1<<20 || got[0].OutputBytes != 70<<10 ||
		got[0].Args["path"] != "a.go" {
		t.Errorf("the resumed turn reads %+v", got)
	}
	tally := identitiesOf([]sandbox.ElidedCall{{Name: "read_file", Digest: "d2", Calls: 7, Failed: 1}})
	if len(tally) != 1 || tally[0].Name != "read_file" || tally[0].Digest != "d2" || tally[0].Calls != 7 ||
		tally[0].Failed != 1 {
		t.Errorf("the dropped calls read %+v", tally)
	}
	// A CALL WHOSE ARGUMENTS ARE WITHHELD reads as one, so a resumed segment
	// counts it as a call of its tool whose arguments are not known, never as
	// the tool called with none — and as WHICH of the two it is, which the
	// execute phase's record says in place of its arguments.
	withheld := bridgedCalls([]sandbox.BridgeCall{{Name: "close_issue", Held: true, Failed: true},
		{Name: "close_issue", Approved: true, HeldID: "held-1"}})
	for _, call := range withheld {
		if !call.Withheld || call.Digest != "" || len(call.Args) != 0 {
			t.Errorf("a held or approved call reads %+v, want it withheld", call.Call)
		}
	}
	if !withheld[0].Held || withheld[0].Approved || withheld[1].Held || !withheld[1].Approved {
		t.Errorf("the held and approved calls read %+v and %+v, want each marked as the one it is",
			withheld[0], withheld[1])
	}
	if withheld[0].HeldID != "" || withheld[1].HeldID != "held-1" {
		t.Errorf("the held and approved calls name held calls %q and %q, want none and the one the approved "+
			"call was made for", withheld[0].HeldID, withheld[1].HeldID)
	}
}

// A RESUME HANDS THE RUNNER THE ROUND'S CALLS AS ITS LOG NUMBERED THEM. A
// relaunch that continues a kept session is told the calls its run made after
// the save by the order the round's log took them in (runner.BridgedCall's
// Appended past runner.SessionBasis' Through), and counts the ones the log
// dropped from its middle after it off the round's appends — the calls the
// resume lists and the ones it hands over as a tally (runner.Resume's Elided).
// So a resume read off a real log carries each call the number the log's own
// append gave it, never its place among the calls the run made, and hands over
// every append the log counts, listed or tallied.
//
// Mutation: drop the number from the conversion, or hand the runner the log's
// dropped calls short of what it counts, and this goes red.
func TestAResumeHandsTheRunnerTheRoundsCallsAsItsLogNumberedThem(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	opened, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe",
		AgentID: "a-1", Role: "SWE", Harness: "claude-code", Round: "r1"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	// PAST THE LOG'S BOUND, and taken in another order than the run made
	// them: the n'th call the log takes is the (taken+1-n)'th the bridge
	// admitted.
	const taken = sandbox.MaxBridgeCalls + 30
	for n := 1; n <= taken; n++ {
		call := sandbox.BridgeCall{Name: fmt.Sprintf("tool_%03d", n), Seq: taken + 1 - n}
		if ok, err := store.AppendBridgeCall(ctx, "run-1", sandbox.BridgeAppend{Launch: opened.LaunchID,
			Call: call}); err != nil || !ok {
			t.Fatalf("appending the %d'th call: %v, %v", n, ok, err)
		}
	}
	logged, err := store.BridgeLog(ctx, opened)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	resumed := runnerResume(resumeInput{Run: opened, Bridged: logged}, nil, nil, nil)
	if got := len(resumed.Bridged) + ledger.Count(resumed.Elided); got != taken || logged.Appends() != taken {
		t.Errorf("the runner is handed %d of the round's calls, listed and tallied, of the %d its log took "+
			"(it counts %d)", got, taken, logged.Appends())
	}
	for _, call := range resumed.Bridged {
		var n int
		if _, err := fmt.Sscanf(call.Name, "tool_%d", &n); err != nil {
			t.Fatalf("a call the case did not make: %q", call.Name)
		}
		if call.Appended != n {
			t.Errorf("%s reaches the runner numbered %d, want %d: the place its log took it at, not the "+
				"%d'th the run made", call.Name, call.Appended, n, call.Seq)
		}
	}
}

// A SESSION WITH NO METER SENDS NO SPEND, and a call its round no longer takes
// is not an error to the box: the store's "not recorded" is the ordinary end
// of a round.
func TestABridgeSessionWithNoMeterSendsNoSpend(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	l.bind(sandbox.OpenedJob{LaunchID: "launch-0"})
	for range 2 {
		if err := l.Record(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"}}); err != nil {
			t.Fatalf("Record on a job the row no longer holds: %v", err)
		}
	}
	for i, a := range store.appends {
		if a.Spent != (sandbox.EngineSpend{}) {
			t.Errorf("append %d carried spend %+v from a session with no meter", i+1, a.Spent)
		}
	}
}

// A RUN'S SUBMISSION, ONCE IN ITS LOG, TELLS THE WAITER THAT DRIVES IT TO LOOK
// NOW: its harness is about to exit, and its completion is found within seconds of
// that rather than up to a poll interval later. Only the submission, only one
// the gate let through, and only one the log recorded — a refused submission
// is no end, and one the log did not take, its round over, names a job that is
// over. (A late one of the round, which the log records, names its own job,
// which the row has moved on from and no waiter polls for it.)
//
// Mutation: nudge on every recorded call, on a refused submission or ask, or
// on one the log did not take, and this goes red.
func TestARecordedSubmissionTellsTheRunsWaiterToLookNow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		launch string
		call   tools.Call
		nudged bool
	}{
		{"a recorded submission", "launch-1", tools.Call{Name: runner.SubmitWorkTool}, true},
		{"a submission the gate refused", "launch-1", tools.Call{Name: runner.SubmitWorkTool, Failed: true}, false},
		{"any other call", "launch-1", tools.Call{Name: "read_page"}, false},
		{"a submission its job no longer takes", "launch-0", tools.Call{Name: runner.SubmitWorkTool}, false},
		// AN ASK ENDS THE RUN AS A SUBMISSION DOES (ADR-0068), and one the
		// tool refused (malformed, its store unreachable, or its job moved
		// on) ends nothing: the run was told so and goes on.
		{"a recorded ask", "launch-1", tools.Call{Name: builtin.AskAndWaitTool}, true},
		{"an ask the tool refused", "launch-1", tools.Call{Name: builtin.AskAndWaitTool, Failed: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []sandbox.Nudge
			l := newBridgeLedger(&appendingStore{}, nil)
			l.nudge = func(_ context.Context, n sandbox.Nudge) { got = append(got, n) }
			l.bind(sandbox.OpenedJob{LaunchID: tc.launch})
			if err := l.Record(t.Context(), "run-1", mcpbridge.Call{Call: tc.call}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			want := sandbox.Nudge{TurnID: "run-1", LaunchID: "launch-1", Why: sandbox.NudgeSubmitted}
			switch {
			case tc.nudged && (len(got) != 1 || got[0] != want):
				t.Errorf("told %+v; want %+v", got, want)
			case !tc.nudged && len(got) != 0:
				t.Errorf("told %+v; want nobody", got)
			}
		})
	}
}

// errSpendRefused is what a store answers beside a record its log took and
// whose spend its record refused ([sandbox.PendingStore.AppendBridgeCall]
// answers both).
var errSpendRefused = errors.New("the record took the call's log but not its spend")

// spendlessStore is a pending store whose log takes the first attempt of the
// first record asked of it and whose record refuses the spend beside it — the
// call is in the log, and the write of what it cost failed after it — and
// which answers every attempt after that one as then does: the spend refused
// again, the store out of reach, or the job replaced — by a new round, or by a
// later job of its own round, which lists the call there.
type spendlessStore struct {
	sandbox.PendingStore
	then  func() (bool, error)
	mu    sync.Mutex
	asked []sandbox.BridgeAppend
}

func (s *spendlessStore) AppendBridgeCall(_ context.Context, _ string, a sandbox.BridgeAppend) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, a)
	if len(s.asked) == 1 {
		return true, errSpendRefused
	}
	return s.then()
}

// A CALL THE LOG TOOK IS NEVER ANSWERED AS AN UPDATE STILL OWED, WHATEVER
// BECAME OF ITS SPEND. A call's update is one write of the run's log and then, where it
// moved the job's spend, one of its record; a store that took the first and
// refused the second answers the call recorded beside the error. The call is in
// the log, and nothing an attempt after that one answers takes it out: the
// attempts after it are for the spend alone, whether they meet the spend
// refused again, the store out of reach, or the job replaced — by a new round,
// or by a later job of its own round, whose log lists the call. So the call is
// never answered as an update its bridge should ask for again
// ([mcpbridge.ErrUnrecorded]), and the run's waiter is still nudged for a
// submission that landed; the spend is cumulative, so the next record that
// lands carries it. And the error answered is the spend's, the one
// write that failed, WHICHEVER RELAUNCH REPLACED THE JOB: never the store's
// word that its job was replaced, which no write failed over. On a bubble's
// clock, so the retries cost the suite nothing.
//
// Mutation: answer a record as its last attempt alone, and a call the first
// attempt put in the log is answered as still owed where the store was then
// out of reach, its submission's waiter never told; nudge only on a record
// that answered no error, and the waiter is not told the run's work is in;
// take the store's word that the job was replaced in its round for another
// write's failure, and the spend's own error is lost.
func TestACallTheLogTookIsNeverSaidToHaveLostItsOutcomeWhateverBecameOfItsSpend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		then func() (bool, error)
	}{
		{"its spend refused at every attempt", func() (bool, error) { return true, errSpendRefused }},
		{"the store out of reach after the attempt the log took", func() (bool, error) {
			return false, errors.New("the coordination store is electing a leader")
		}},
		{"the log moved on after the attempt it took", func() (bool, error) { return false, nil }},
		{"a later job of its round took the log after the attempt it took", func() (bool, error) {
			return true, sandbox.ErrJobReplaced
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := &spendlessStore{then: tc.then}
				l, _ := readingLedger(store)
				var nudged []sandbox.Nudge
				l.nudge = func(_ context.Context, n sandbox.Nudge) { nudged = append(nudged, n) }
				err := l.Record(t.Context(), "run-1", mcpbridge.Call{
					Call: tools.Call{Name: sandbox.SubmitWorkTool}, Seq: 2, Digest: strings.Repeat("d2", 32)})
				if !errors.Is(err, errSpendRefused) {
					t.Errorf("a record whose spend never reached the row answered %v, want the spend's error", err)
				}
				if len(nudged) != 1 || nudged[0].Why != sandbox.NudgeSubmitted {
					t.Errorf("the waiter was nudged %+v, want once for the submission that landed", nudged)
				}
				if errors.Is(err, mcpbridge.ErrUnrecorded) {
					t.Errorf("a call the log took was answered as an update still owed (%v): its bridge "+
						"would ask for it again", err)
				}
			})
		})
	}
}

// recordOutage is a run store whose records fail while it is down, as a
// coordination store does while its stream elects a leader, and are the
// store's own otherwise, as every begin and withdrawal is; it counts the
// records it was asked for.
type recordOutage struct {
	sandbox.PendingStore
	down    atomic.Bool
	records atomic.Int32
}

func (s *recordOutage) AppendBridgeCall(ctx context.Context, turnID string, a sandbox.BridgeAppend) (bool, error) {
	if a.Step == sandbox.AppendRecord {
		s.records.Add(1)
		if s.down.Load() {
			return false, errors.New("the coordination store is electing a leader")
		}
	}
	return s.PendingStore.AppendBridgeCall(ctx, turnID, a)
}

// ledgerOn is the engine's ledger of the one job of run-1 on store, launched
// there, saying what it could not record into the buffer it answers.
func ledgerOn(t *testing.T, store sandbox.PendingStore) (*bridgeLedger, *bytes.Buffer, sandbox.PendingRun) {
	t.Helper()
	launched, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe",
		Harness: "claude-code"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	var logs bytes.Buffer
	l := newBridgeLedger(store, nil)
	l.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l.bind(launched.NamedJob(0, ""))
	return l, &logs, launched
}

// AN UPDATE THE STORE REFUSES LEAVES THE CALL'S IDENTITY ON RECORD, AND IS
// ANSWERED AS ONE STILL OWED. A call's entry is written before it runs, so a
// store that then refuses every attempt at its update holds back what the call
// answered, cost and committed, and nothing else: the entry stands in the run's
// log, the call's identity on the record for every later segment's call log —
// handed to the runner as a call whose outcome is unknown — and the ledger
// answers [mcpbridge.ErrUnrecorded], which its bridge asks for again once the
// call's answer is out. A blip the retries outlast loses nothing: the update
// lands on a later attempt and takes the entry out. Nothing is carried by a
// later call: the next call's update carries only itself.
//
// Mutation: make one attempt at an update, and a blip loses the call's
// outcome; take the entry out of the log where the update did not land, and
// the call is off the record; answer a refused update without
// [mcpbridge.ErrUnrecorded] (refused-update-not-owed), and its bridge never asks
// for it again.
func TestAnUpdateTheStoreRefusesLeavesTheCallsIdentityOnRecord(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		store := &recordOutage{PendingStore: sandbox.NewCoordStore(memory.NewFleet())}
		l, logs, launched := ledgerOn(t, store)
		started := time.Now().UTC()
		for seq, name := range map[int]string{1: "post_message", 2: "read_page"} {
			if err := l.Begin(ctx, "run-1", mcpbridge.Begun{Name: name, Seq: seq,
				StartedAt: started.Add(time.Duration(seq) * time.Second), Digest: strings.Repeat("a", 64)}); err != nil {
				t.Fatalf("the entry of %s: %v", name, err)
			}
		}
		store.down.Store(true)
		if err := l.Record(ctx, "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message", Output: "posted"},
			Seq: 1, StartedAt: started.Add(time.Second), Digest: strings.Repeat("a", 64)}); !errors.Is(err,
			mcpbridge.ErrUnrecorded) {
			t.Fatalf("an update the store refused at every attempt answered %v, want it owed "+
				"(mcpbridge.ErrUnrecorded) for its bridge to ask again", err)
		}
		if n := int(store.records.Load()); n != appendAttempts {
			t.Errorf("the update was tried %d times, want %d", n, appendAttempts)
		}
		line := logLine(t, logs, "bridge_call_update_refused")
		if line == nil || line["call"] != "post_message at place 1, digest aaaaaaaa" {
			t.Errorf("the update that did not land was said as %v, want the call named\n%s", line, logs)
		}

		// THE STORE BACK, the next call's update carries itself alone.
		store.down.Store(false)
		if err := l.Record(ctx, "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page", Output: "read"},
			Seq: 2, StartedAt: started.Add(2 * time.Second), Digest: strings.Repeat("a", 64)}); err != nil {
			t.Fatalf("the next call's update: %v", err)
		}
		logged, err := store.BridgeLog(ctx, launched)
		if err != nil {
			t.Fatalf("BridgeLog: %v", err)
		}
		if len(logged.Calls) != 1 || logged.Calls[0].Name != "read_page" {
			t.Errorf("the log lists %+v, want the call whose update landed alone", logged.Calls)
		}
		if len(logged.InFlight) != 1 || logged.InFlight[0].Name != "post_message" || logged.InFlight[0].Seq != 1 {
			t.Fatalf("the log holds %+v in flight, want the call whose update was refused, by its identity",
				logged.InFlight)
		}
		resumed := runnerResume(resumeInput{Run: launched, Bridged: logged}, nil, nil, nil)
		if len(resumed.Unfinished) != 1 || resumed.Unfinished[0].Name != "post_message" ||
			resumed.Unfinished[0].Digest != strings.Repeat("a", 64) || !resumed.Unfinished[0].Unfinished {
			t.Errorf("the runner is handed %+v of the call whose update was refused, want it by its identity, "+
				"its outcome unknown", resumed.Unfinished)
		}
	})
}

// electingStore fails every bridged write until a re-election has passed since
// its first, as a coordination store does while a stream's leader steps down
// and its successor is elected, and takes every write after that.
type electingStore struct {
	sandbox.PendingStore
	mu    sync.Mutex
	first time.Time
	asked int
}

// reElection is how long a stream's successor takes to be elected once its
// leader steps down: about a second (see [appendRetryWaits]).
const reElection = time.Second

func (s *electingStore) AppendBridgeCall(context.Context, string, sandbox.BridgeAppend) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	if s.first.IsZero() {
		s.first = time.Now()
	}
	if time.Since(s.first) < reElection {
		return false, errors.New("the coordination store is electing a leader")
	}
	return true, nil
}

// A BRIDGED CALL'S WRITES OUTLAST A LEADER'S RE-ELECTION. A call made as the
// coordination store's stream leader steps down meets a store that refuses
// every write until a successor is elected, about a second later. The retries
// are there to outlast exactly that, so the call's entry lands within its
// budget and the call runs, rather than being refused as one that could not be
// recorded — and its update lands before its answer goes back to the box. On a
// bubble's clock, so the second is exact and costs the suite nothing.
//
// Mutation: space the attempts 250 ms and then 500 ms, as they were, and the
// last one comes before the successor is elected: the call is refused.
func TestABridgedCallsWritesOutlastALeadersReElection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for name, write := range map[string]func(l *bridgeLedger) error{
			"its entry": func(l *bridgeLedger) error {
				return l.Begin(t.Context(), "run-1", mcpbridge.Begun{Name: "post_message", Seq: 1,
					StartedAt: time.Now().UTC(), Digest: strings.Repeat("e1", 32)})
			},
			"its update": func(l *bridgeLedger) error {
				return l.Record(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
					Seq: 1, Digest: strings.Repeat("e1", 32)})
			},
		} {
			store := &electingStore{}
			l := newBridgeLedger(store, nil)
			l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
			if err := write(l); err != nil {
				t.Errorf("%s, written as the stream's leader stepped down, failed after %d attempts (%v); "+
					"want it landed once a successor was elected", name, store.asked, err)
			}
		}
	})
}

// fullStore is a pending store whose log holds as many calls in flight as it
// keeps, and counts the writes asked of it.
type fullStore struct {
	sandbox.PendingStore
	asked atomic.Int32
}

func (s *fullStore) AppendBridgeCall(context.Context, string, sandbox.BridgeAppend) (bool, error) {
	s.asked.Add(1)
	return false, sandbox.ErrCallsInFlight
}

// A CALL WHOSE ENTRY THE LOG DOES NOT TAKE IS NAMED, AND REFUSED. The call is not
// run, so the ledger says so, at warning, naming it — its tool, its place and
// the opening of its digest — and answers the bridge why: a store that refused
// every attempt within the entry's budget, an error; a log that takes no more
// calls of the job, [mcpbridge.ErrNotTaken], which the box is told as its run's
// end; and a log holding as many calls in flight as it keeps, asked once — room
// is made by calls finishing, not by asking again.
//
// Mutation: try a begin again past a log's bound, and a full log is asked as
// often as an outage is.
func TestACallWhoseEntryTheLogDoesNotTakeIsNamedAndRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		begun := mcpbridge.Begun{Name: "post_message", Seq: 1, StartedAt: time.Now().UTC(),
			Digest: strings.Repeat("a1", 32)}

		down := &movingStore{}
		down.down.Store(true)
		l, logs := readingLedger(down)
		if err := l.Begin(t.Context(), "run-1", begun); err == nil || errors.Is(err, mcpbridge.ErrNotTaken) {
			t.Errorf("an entry the store refused at every attempt answered %v, want the store's error", err)
		}
		line := logLine(t, logs, "bridge_call_not_recorded")
		if line == nil || line["level"] != "WARN" || line["call"] != "post_message at place 1, digest a1a1a1a1" {
			t.Errorf("the call not run was said as %v, want it named at warning\n%s", line, logs)
		}

		moved := &movingStore{}
		moved.moved.Store(true)
		l, _ = readingLedger(moved)
		if err := l.Begin(t.Context(), "run-1", begun); !errors.Is(err, mcpbridge.ErrNotTaken) {
			t.Errorf("an entry the log no longer takes answered %v, want mcpbridge.ErrNotTaken", err)
		}

		full := &fullStore{}
		l, _ = readingLedger(full)
		if err := l.Begin(t.Context(), "run-1", begun); !errors.Is(err, sandbox.ErrCallsInFlight) {
			t.Errorf("an entry a full log refused answered %v, want sandbox.ErrCallsInFlight", err)
		}
		if n := full.asked.Load(); n != 1 {
			t.Errorf("a full log was asked %d times, want once", n)
		}
	})
}

// movingStore is a pending store whose bridged appends fail while it is down,
// and which answers an append for a job its log no longer takes once the log
// has moved on — a relaunch started a new round, or the run ended — as the store
// does: taken, and landed nowhere.
type movingStore struct {
	sandbox.PendingStore
	down, moved atomic.Bool
}

func (s *movingStore) AppendBridgeCall(context.Context, string, sandbox.BridgeAppend) (bool, error) {
	if s.down.Load() {
		return false, errors.New("the coordination store is electing a leader")
	}
	return !s.moved.Load(), nil
}

// readingLedger is the engine's ledger of one session bound to job
// "launch-1" over store, saying what it could not record into the buffer it
// answers, as JSON lines.
func readingLedger(store sandbox.PendingStore) (*bridgeLedger, *bytes.Buffer) {
	var logs bytes.Buffer
	l := newBridgeLedger(store, nil)
	l.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
	return l, &logs
}

// ledgerLines is every line of logs whose message is msg, in the order said.
func ledgerLines(logs *bytes.Buffer, msg string) []map[string]any {
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if line == "" || json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != msg {
			continue
		}
		found = append(found, rec)
	}
	return found
}

// logLine is the one line of logs whose message is msg, or nil where there is
// none; a second is a case's failure.
func logLine(t *testing.T, logs *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	found := ledgerLines(logs, msg)
	if len(found) > 1 {
		t.Fatalf("the ledger said %q %d times:\n%s", msg, len(found), logs)
	}
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

// A CALL NOBODY WAITED FOR THAT LANDS ON NO RECORD IS NAMED AT WARNING. An
// opening's end answers a call still running that it is still running and
// waits for none, and an ending that does not wait for it goes on to delete the
// run's record: a call whose update finds the record gone ran, and its effect
// happened, while neither the session that made it (which holds the end's
// answer) nor any record of the run holds what it did. So the ledger names it,
// at warning, by its tool, its place among the job's calls and the opening of
// its identity, where a call its harness was handed whose round is over stays
// the ordinary end of a round, said at debug.
//
// Mutation: say every call the log no longer takes at debug (`false &&` before
// the test of its mark), and the call nobody waited for is named only at debug.
func TestACallNobodyWaitedForThatLandsOnNoRecordIsNamedAtWarning(t *testing.T) {
	t.Parallel()
	store := &movingStore{}
	l, logs := readingLedger(store)
	// THE RUN'S ENDING DELETED ITS RECORD while the calls ran.
	store.moved.Store(true)
	for _, call := range []mcpbridge.Call{
		{Call: tools.Call{Name: "delegate"}, Seq: 5, Digest: strings.Repeat("e5", 32), Abandoned: true},
		{Call: tools.Call{Name: "read_page"}, Seq: 6, Digest: strings.Repeat("f6", 32)},
	} {
		if err := l.Record(t.Context(), "run-1", call); err != nil {
			t.Fatalf("the update of %s, which no record takes: %v", call.Name, err)
		}
	}
	line := logLine(t, logs, "bridge_abandoned_call_after_its_job")
	if line == nil || line["level"] != "WARN" || line["call"] != "delegate at place 5, digest e5e5e5e5" {
		t.Errorf("the call nobody waited for was said as %v, want it named at warning\n%s", line, logs)
	}
	ordinary := logLine(t, logs, "bridge_call_after_its_job")
	if ordinary == nil || ordinary["level"] != "DEBUG" || ordinary["tool"] != "read_page" {
		t.Errorf("the call its harness was handed was said as %v, want it at debug alone\n%s", ordinary, logs)
	}
}
