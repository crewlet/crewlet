package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
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
		if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "query_episodes"}}); err != nil {
			t.Fatalf("Append: %v", err)
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

// A SESSION NO JOB WAS BOUND TO FILES NOTHING. Through the launch that cannot
// happen — the job is named before the box that makes calls exists — so a call
// arriving on an unbound session is a wiring mistake, and the one thing it must
// not do is reach the store naming no job, which a store that read an empty
// name as "whichever job the row holds" filed on the next job's record.
//
// Mutation: send the append whatever the ledger's binding, and this goes red.
func TestAnUnboundBridgeSessionFilesNothing(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"}}); err != nil {
		t.Fatalf("Append: %v", err)
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
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{
		Call: tools.Call{Name: "list_issues", Args: map[string]any{"repo": "core"}, Output: "[]"},
		Seq:  4, StartedAt: started, Duration: 1500 * time.Millisecond,
		Origin: "mcp", Server: "github", Digest: "d1",
	}); err != nil {
		t.Fatalf("Append: %v", err)
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
// calls the log dropped, by identity, in the turn's own shape.
//
// Mutation: drop the digest or either size on the way to the runner, or the
// tally, and a resumed segment seeds a cut call as another and replays a cut
// submission as the run's.
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
		{Name: "close_issue", Approved: true}})
	for _, call := range withheld {
		if !call.Withheld || call.Digest != "" || len(call.Args) != 0 {
			t.Errorf("a held or approved call reads %+v, want it withheld", call.Call)
		}
	}
	if !withheld[0].Held || withheld[0].Approved || withheld[1].Held || !withheld[1].Approved {
		t.Errorf("the held and approved calls read %+v and %+v, want each marked as the one it is",
			withheld[0], withheld[1])
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
	resumed := runnerResume(resumeInput{Run: opened, Bridged: logged}, nil, nil)
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
		if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"}}); err != nil {
			t.Fatalf("Append on a job the row no longer holds: %v", err)
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
			if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tc.call}); err != nil {
				t.Fatalf("Append: %v", err)
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

// flakyStore is a pending store whose appends fail while fail says so, and
// records every append it was asked for, landed or not.
type flakyStore struct {
	sandbox.PendingStore
	mu     sync.Mutex
	fail   func(attempt int, a sandbox.BridgeAppend) bool
	asked  []sandbox.BridgeAppend
	landed []sandbox.BridgeAppend
}

func (s *flakyStore) AppendBridgeCall(_ context.Context, _ string, a sandbox.BridgeAppend) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, a)
	if s.fail(len(s.asked), a) {
		return false, errors.New("the coordination store is electing a leader")
	}
	s.landed = append(s.landed, a)
	return true, nil
}

// errSpendRefused is what a store answers beside an append its log took and
// whose spend its record refused ([sandbox.PendingStore.AppendBridgeCall]
// answers both).
var errSpendRefused = errors.New("the record took the call's log but not its spend")

// spendlessStore is a pending store whose log takes the first attempt of the
// first append asked of it and whose record refuses the spend beside it — the
// calls are in the log, and the write of what they cost failed after it — and
// which answers every attempt after that one as then does: the spend refused
// again, the store out of reach, or the job replaced — by a new round, or by a
// later job of its own round, which lists the calls there.
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

// A CALL THE LOG TOOK IS NEVER OWED, WHATEVER BECAME OF ITS SPEND. An append is
// one write of the run's log and then, where its calls moved the job's spend,
// one of its record; a store that took the first and refused the second
// answers the calls recorded beside the error. They are in the log, and
// nothing an attempt after that one answers takes them out of it: the attempts
// after it are for the spend alone, whether they meet the spend refused again,
// the store out of reach, or the job replaced — by a new round, or by a later
// job of its own round, whose log lists the calls. So neither the call nor what
// the append carried is owed — owed, the session's next append would carry
// calls the log already lists, and its end would name them on no record — nor
// said to be on no record of the run, a carry of them is answered carried, and
// the run's waiter is still nudged for a submission that landed; the spend is
// cumulative, so the next append that lands carries it. And the error answered
// is the spend's, the one write that failed, WHICHEVER RELAUNCH REPLACED THE
// JOB: never the store's word that its job was replaced, which no write
// failed over. On a bubble's clock, so the retries cost the suite nothing.
//
// Mutation: answer an append as its last attempt alone, and a call the first
// attempt put in the log is owed again where the store was then out of reach,
// and said to be on no record where the log then moved on, its submission's
// waiter never told; owe a call on any error the append answers, and the next
// append carries the submission the log already holds; nudge only on an append
// that answered no error, and the waiter is not told the run's work is in;
// take the store's word that the job was replaced in its round for another
// write's failure, and the spend's own error is lost.
func TestACallTheLogTookIsNeverOwedWhateverBecameOfItsSpend(t *testing.T) {
	t.Parallel()
	carried := sandbox.OwedAppend{Call: sandbox.BridgeCall{Name: "post_message", Seq: 1,
		Digest: strings.Repeat("a1", 32)}}
	unrecorded := []string{"bridge_call_owed", "bridge_owed_calls_unrecorded", "bridge_owed_calls_after_their_job"}
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
				// A CALL'S OWN APPEND, carrying one the session owed.
				store := &spendlessStore{then: tc.then}
				l, logs := readingLedger(store)
				l.owed = []sandbox.OwedAppend{carried}
				var nudged []sandbox.Nudge
				l.nudge = func(_ context.Context, n sandbox.Nudge) { nudged = append(nudged, n) }
				if err := l.Append(t.Context(), "run-1", mcpbridge.Call{
					Call: tools.Call{Name: sandbox.SubmitWorkTool}, Seq: 2, Digest: strings.Repeat("d2", 32)}); !errors.Is(err, errSpendRefused) {
					t.Errorf("an append whose spend never reached the record answered %v, want the spend's error", err)
				}
				if first := store.asked[0]; len(first.Owed) != 1 || first.Owed[0].Call.Seq != 1 {
					t.Fatalf("the first attempt carried %+v, want the call the session owed", first.Owed)
				}
				if n := l.owing(); n != 0 {
					t.Errorf("the ledger owes %d appends of calls the log took, want none", n)
				}
				if len(nudged) != 1 || nudged[0].Why != sandbox.NudgeSubmitted {
					t.Errorf("the waiter was nudged %+v, want once for the submission that landed", nudged)
				}
				for _, msg := range unrecorded {
					if line := logLine(t, logs, msg); line != nil {
						t.Errorf("calls the log took were said as %s: %v", msg, line)
					}
				}
				_ = l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"},
					Seq: 3, Digest: strings.Repeat("d3", 32)})
				if last := store.asked[len(store.asked)-1]; len(last.Owed) != 0 {
					t.Errorf("the next append carried %+v, calls the log already held", last.Owed)
				}

				// AND A CARRY OF WHAT THE SESSION OWED, as the run's
				// collection asks for one.
				store = &spendlessStore{then: tc.then}
				l, logs = readingLedger(store)
				l.owed = []sandbox.OwedAppend{carried}
				if !l.Settle(t.Context(), "run-1") {
					t.Error("a carry the log took answered not carried")
				}
				if n := l.owing(); n != 0 {
					t.Errorf("the ledger owes %d appends after a carry the log took, want none", n)
				}
				if line := logLine(t, logs, "bridge_owed_calls_carried"); line == nil || line["spend_error"] == nil {
					t.Errorf("the carry was said as %v, want carried with its spend's error\n%s", line, logs)
				}
				for _, msg := range unrecorded {
					if line := logLine(t, logs, msg); line != nil {
						t.Errorf("calls a carry put in the log were said as %s: %v", msg, line)
					}
				}
			})
		})
	}
}

// A BRIDGED CALL'S RECORD THAT DID NOT LAND IS TRIED AGAIN, AND THEN OWED,
// WHOLE. The call has run and its answer goes back to the box whatever the log
// says, but a call missing from the log is one a resumed segment's call log
// never counts — and a later call asking for something else then derives its
// operation id. So a failed append is tried again before the answer goes back,
// which rides a store's blip; and an append whose every attempt failed is owed
// as the append it was — the call, kept as the log would keep it, and what its
// own work absorbed — and carried by the session's next append, which the log
// lists it from once whether or not one of those attempts landed after all.
//
// Mutation: append once and give up, and a blip loses the call; drop a call
// whose attempts all failed, or owe it by identity alone, and the next append
// carries nothing the log can list.
func TestABridgedCallsRecordThatDidNotLandIsTriedAgainAndThenOwed(t *testing.T) {
	t.Parallel()
	blip := &flakyStore{fail: func(attempt int, _ sandbox.BridgeAppend) bool { return attempt == 1 }}
	l := newBridgeLedger(blip, nil)
	l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
		Seq: 1, Digest: "d1"}); err != nil {
		t.Fatalf("an append the store refused once = %v; want it landed on its retry", err)
	}
	if len(blip.landed) != 1 || blip.landed[0].Call.Digest != "d1" {
		t.Fatalf("the store took %+v, want the call once", blip.landed)
	}

	down := &flakyStore{fail: func(_ int, a sandbox.BridgeAppend) bool { return a.Call.Seq == 1 }}
	l = newBridgeLedger(down, nil)
	l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
	worker := []toolloop.Called{{Tool: "leave_channel", Digest: "w1"}}
	started := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "delegate", Failed: true},
		Seq: 1, StartedAt: started, Digest: "d1", Absorbed: worker}); err == nil {
		t.Fatal("an append the store refused every time answered no error")
	}
	if len(down.asked) != appendAttempts {
		t.Errorf("the append was tried %d times, want %d", len(down.asked), appendAttempts)
	}
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"},
		Seq: 2, Digest: "d2"}); err != nil {
		t.Fatalf("the next append: %v", err)
	}
	if len(down.landed) != 1 {
		t.Fatalf("the store took %d appends, want the second call's", len(down.landed))
	}
	carried := down.landed[0]
	want := []sandbox.OwedAppend{{
		Call:     sandbox.BridgeCall{Name: "delegate", Failed: true, Seq: 1, StartedAt: started, Digest: "d1"},
		Absorbed: []sandbox.ElidedCall{{Name: "leave_channel", Digest: "w1", Calls: 1}},
	}}
	if !reflect.DeepEqual(carried.Owed, want) || len(carried.Absorbed) != 0 {
		t.Errorf("the next append carried %+v owed and %+v absorbed of its own, want %+v and none",
			carried.Owed, carried.Absorbed, want)
	}
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"},
		Seq: 3, Digest: "d3"}); err != nil {
		t.Fatalf("a third append: %v", err)
	}
	if again := down.landed[1]; len(again.Owed) != 0 {
		t.Errorf("the append after the one that carried the owed call carried %+v again", again.Owed)
	}
}

// electingStore fails every bridged append until a re-election has passed
// since its first, as a coordination store does while a stream's leader steps
// down and its successor is elected, and takes every append after that.
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

// A BRIDGED CALL'S RECORD OUTLASTS A LEADER'S RE-ELECTION. A call made as the
// coordination store's stream leader steps down meets a store that refuses
// every write until a successor is elected, about a second later. The retry is
// there to outlast exactly that, so the call's own append lands before its
// answer goes back to the box, rather than being owed to a later call the job
// may never make. On a bubble's clock, so the second is exact and costs the
// suite nothing.
//
// Mutation: space the attempts 250 ms and then 500 ms, as they were, and the
// last one comes before the successor is elected: the call is owed.
func TestABridgedCallsRecordOutlastsALeadersReElection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &electingStore{}
		l := newBridgeLedger(store, nil)
		l.bind(sandbox.OpenedJob{LaunchID: "launch-1"})
		err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
			Seq: 1, Digest: strings.Repeat("e1", 32)})
		if err != nil || l.owing() != 0 {
			t.Errorf("a call made as the stream's leader stepped down was owed after %d attempts (%v); "+
				"want its own append to land once a successor was elected", store.asked, err)
		}
	})
}

// outageStore is a pending store whose bridged appends fail while it is down,
// as a coordination store does while its stream elects a leader, and are the
// store's own otherwise.
type outageStore struct {
	sandbox.PendingStore
	down atomic.Bool
}

func (s *outageStore) AppendBridgeCall(ctx context.Context, turnID string, a sandbox.BridgeAppend) (bool, error) {
	if s.down.Load() {
		return false, errors.New("the coordination store is electing a leader")
	}
	return s.PendingStore.AppendBridgeCall(ctx, turnID, a)
}

// bridgeEndRig is a run's job open on a real run store, with its bridge
// session open on a real bridge, the session's ledger the engine's own.
type bridgeEndRig struct {
	store  *outageStore
	bridge *mcpbridge.Bridge
	ledger *bridgeLedger
	turnID string
	launch string
}

func newBridgeEndRig(t *testing.T) *bridgeEndRig {
	t.Helper()
	ctx := t.Context()
	r := &bridgeEndRig{store: &outageStore{PendingStore: sandbox.NewCoordStore(memory.NewFleet())},
		turnID: "run-1"}
	seatID, _ := org.DeriveAgentID("Nimbus", "dev")
	launch, err := r.store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: r.turnID, AgentID: seatID.String(),
		AgentHandle: "dev", Role: "Engineer", Harness: "claude-code", CreatedAt: time.Now().UTC()},
		sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	r.launch = launch.LaunchID
	if _, err := r.store.MarkSuspended(ctx, r.turnID,
		sandbox.Suspension{State: json.RawMessage(`{"version":4}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	r.bridge, err = mcpbridge.New(mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
		BaseURL: "https://engine.example.com"})
	if err != nil {
		t.Fatalf("mcpbridge.New: %v", err)
	}
	r.bridge.Handler()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
	turn := &turnctx.Turn{RunID: r.turnID, Seat: seat,
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}}
	r.ledger = newBridgeLedger(r.store, nil)
	r.ledger.bind(sandbox.OpenedJob{LaunchID: r.launch})
	if url := r.bridge.Open(ctx, &mcpbridge.Session{RunID: r.turnID, Launch: r.launch, Handle: "dev",
		Role: "Engineer", Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).ForTurn(turn),
		Ledger: r.ledger}); url == "" {
		t.Fatal("the bridge opened no session for the run's job")
	}
	return r
}

// submission is the run's last call as the bridge records it: its submission,
// made while the store was electing a leader.
func (r *bridgeEndRig) submission(t *testing.T) {
	t.Helper()
	r.store.down.Store(true)
	defer r.store.down.Store(false)
	if err := r.ledger.Append(t.Context(), r.turnID, mcpbridge.Call{
		Call: tools.Call{Name: sandbox.SubmitWorkTool, Args: map[string]any{"outcome": "delivered"},
			Output: "submitted"},
		Seq: 1, StartedAt: time.Now().UTC(), Digest: strings.Repeat("d", 64)}); err == nil {
		t.Fatal("the submission's append landed while the store was down")
	}
}

// logged is the job's log as the store holds it beside the job's row.
func (r *bridgeEndRig) logged(t *testing.T) sandbox.BridgeLog {
	t.Helper()
	run, found, err := r.store.Get(t.Context(), r.turnID)
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	logged, err := r.store.BridgeLog(t.Context(), run)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	return logged
}

// A SESSION THAT ENDS OWING CALLS CARRIES THEM FIRST. A job's last call — its
// submission — has no later call of its own to carry it, so when its append
// meets a store's blip, the session's end carries it: the bridge's close of the
// run waits on the ledger's carry of what it still owes, and the log lists the
// call once the close returns, though no call came after it.
//
// Mutation: end the session without the ledger's carry, and the submission the
// run made is on no record of it.
func TestASessionThatEndsOwingCallsCarriesThemFirst(t *testing.T) {
	t.Parallel()
	r := newBridgeEndRig(t)
	r.submission(t)
	if n := len(r.logged(t).Calls); n != 0 {
		t.Fatalf("the log lists %d calls before the session ended, want the submission still owed", n)
	}
	r.bridge.Close(r.turnID)
	logged := r.logged(t)
	if len(logged.Calls) != 1 || logged.Calls[0].Name != sandbox.SubmitWorkTool ||
		logged.Calls[0].Seq != 1 || logged.Elided != 0 {
		t.Errorf("once the session ended the log lists %+v (%d unlisted), want the submission it owed",
			logged.Calls, logged.Elided)
	}
	if n := r.ledger.owing(); n != 0 {
		t.Errorf("the ledger still owes %d appends once its session ended", n)
	}
}

// AND THE RUN'S COLLECTION HAS IT CARRIED BEFORE ITS RESUME READS THE JOB'S
// LOG. A job's session lives until its run settles or its next job replaces it,
// both after the collection has read what the job did; so the collection asks
// the job's own session to carry what it owes, and the log its resume reads
// holds the submission whose append met the blip. A job the session does not serve —
// the run's next one, or another run's — is owed nothing here.
//
// Mutation: answer the collection's ask without the ledger's carry, or carry
// for a job the session does not serve, and this goes red.
func TestARunsCollectionHasItsJobsOwedCallsCarriedFirst(t *testing.T) {
	t.Parallel()
	r := newBridgeEndRig(t)
	r.submission(t)
	if r.bridge.Settle(t.Context(), r.turnID, "another-job") {
		t.Error("a job the session does not serve was answered as carried")
	}
	if n := len(r.logged(t).Calls); n != 0 {
		t.Fatalf("the log lists %d calls after an ask for another job", n)
	}
	if !r.bridge.Settle(t.Context(), r.turnID, r.launch) {
		t.Fatal("the job's own session carried nothing it owed")
	}
	if logged := r.logged(t); len(logged.Calls) != 1 || logged.Calls[0].Name != sandbox.SubmitWorkTool {
		t.Errorf("the log the collection's resume reads lists %+v, want the submission", logged.Calls)
	}
	if r.bridge.Live() != 1 {
		t.Error("the collection's ask ended the job's session, which lives until its run settles")
	}
}

// A CARRY THAT CANNOT LAND IS SAID, AND KEPT WHILE IT MIGHT STILL. One the store
// does not take within its bound is still owed, for the session's next append
// or its next carry; one the store answers for a log that has moved on — a
// relaunch started a new round, or the run ended — is forgotten, since no record
// of the run will ever take it.
//
// Mutation: forget the calls of a carry the store did not take, and a blip that
// outlasts the collection's ask loses them before the session's end can try.
func TestACarryThatCannotLandIsKeptWhileItMightStill(t *testing.T) {
	t.Parallel()
	r := newBridgeEndRig(t)
	r.submission(t)
	r.store.down.Store(true)
	if r.ledger.Settle(t.Context(), r.turnID) {
		t.Fatal("a carry the store refused answered carried")
	}
	r.store.down.Store(false)
	if n := r.ledger.owing(); n != 1 {
		t.Fatalf("the ledger owes %d appends after a carry the store refused, want the submission still", n)
	}
	r.ledger.bind(sandbox.OpenedJob{LaunchID: "another-job"})
	if r.ledger.Settle(t.Context(), r.turnID) || r.ledger.owing() != 0 {
		t.Errorf("a carry for a job the row no longer holds left %d owed, want it forgotten", r.ledger.owing())
	}
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

// named is the calls a line names, as it names them.
func named(rec map[string]any) []string {
	listed, _ := rec["owed"].([]any)
	out := make([]string, 0, len(listed))
	for _, c := range listed {
		s, _ := c.(string)
		out = append(out, s)
	}
	return out
}

// AN OWED CALL NO RECORD WILL TAKE IS NAMED, CALL BY CALL, AT WARNING. Calls
// whose records did not land are owed to the session's next append or carry,
// and the log may have moved on before either lands — a relaunch started a new
// round while one of its calls was still in flight, or the run ended. Whichever
// append then takes them to the log — a carry of the session's, or a later call
// of the old job, whose own drop is the ordinary end of a round — the store
// answers it for a round the log no longer takes, and the calls it carried are on
// no record of the run: each is named, by its tool, its place and the opening
// of its identity, so the line says WHICH calls a resumed segment cannot
// count, and the session forgets them.
//
// Mutation: drop the calls a later call of the old job carried with only that
// call's own debug line, and the calls no record holds are named nowhere.
func TestAnOwedCallNoRecordWillTakeIsNamedCallByCall(t *testing.T) {
	t.Parallel()
	for name, carry := range map[string]func(t *testing.T, l *bridgeLedger){
		"carried by a later call of the old job": func(t *testing.T, l *bridgeLedger) {
			if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"},
				Seq: 3, Digest: strings.Repeat("c", 64)}); err != nil {
				t.Fatalf("the late call's append: %v", err)
			}
		},
		"carried by the session's carry": func(t *testing.T, l *bridgeLedger) {
			if l.Settle(t.Context(), "run-1") {
				t.Fatal("a carry to a log that moved on answered carried")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &movingStore{}
			l, logs := readingLedger(store)
			store.down.Store(true)
			for i, digest := range []string{strings.Repeat("a1", 32), strings.Repeat("b2", 32)} {
				if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
					Seq: i + 1, Digest: digest}); err == nil {
					t.Fatal("an append landed while the store was down")
				}
			}
			store.down.Store(false)
			store.moved.Store(true)
			carry(t, l)

			line := logLine(t, logs, "bridge_owed_calls_after_their_job")
			want := []string{"post_message at place 1, digest a1a1a1a1", "post_message at place 2, digest b2b2b2b2"}
			if line == nil || line["level"] != "WARN" || !reflect.DeepEqual(named(line), want) {
				t.Errorf("the calls no record will take were said as %v, want each named at warning: %v\n%s",
					line, want, logs)
			}
			if n := l.owing(); n != 0 {
				t.Errorf("the session still owes %d appends no record will take", n)
			}
		})
	}
}

// A CALL WHOSE RECORD FAILS ONCE ITS SESSION HAS ENDED IS SAID TO BE ON NO
// RECORD. The session's end makes its last carry of its own; a call still in
// flight as the opening ended records itself after that, and where its record
// does not land nothing of the session's will carry it but another call still
// in flight. So the ledger says so, naming it — never the line that promises
// the carry its session's end has already made.
//
// Mutation: end the session without marking the ledger ended, and the failed
// record is promised a carry that never comes.
func TestACallWhoseRecordFailsOnceItsSessionEndedIsSaidToBeOnNoRecord(t *testing.T) {
	t.Parallel()
	store := &movingStore{}
	l, logs := readingLedger(store)
	if l.End(t.Context(), "run-1") {
		t.Fatal("the end of a session that owed nothing answered carried")
	}
	store.down.Store(true)
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
		Seq: 4, Digest: strings.Repeat("d4", 32)}); err == nil {
		t.Fatal("an append landed while the store was down")
	}
	if promised := logLine(t, logs, "bridge_call_owed"); promised != nil {
		t.Errorf("a call whose record failed after its session ended was promised a carry: %v", promised)
	}
	line := logLine(t, logs, "bridge_owed_calls_unrecorded")
	if line == nil || line["level"] != "WARN" || line["session_ended"] != true ||
		!reflect.DeepEqual(named(line), []string{"post_message at place 4, digest d4d4d4d4"}) {
		t.Errorf("the call was said as %v, want it named at warning as on no record of an ended session\n%s",
			line, logs)
	}
}

// heldStore fails every bridged append while down, as a coordination store
// does while its stream elects a leader, and holds the first append asked once
// hold is set until release closes or that append's own bound ends: a carry
// caught out by the outage. It keeps every append it was asked for.
type heldStore struct {
	sandbox.PendingStore
	down, hold atomic.Bool
	entered    chan struct{}
	release    chan struct{}

	mu    sync.Mutex
	asked []sandbox.BridgeAppend
}

func (s *heldStore) AppendBridgeCall(ctx context.Context, _ string, a sandbox.BridgeAppend) (bool, error) {
	s.mu.Lock()
	s.asked = append(s.asked, a)
	s.mu.Unlock()
	if s.hold.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	}
	if s.down.Load() {
		return false, errors.New("the coordination store is electing a leader")
	}
	return true, nil
}

// carries is how many of the appends asked of s were carries of the call at
// place seq: appends naming no call of their own, owing it.
func (s *heldStore) carries(seq int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.asked {
		if a.Call.Name == "" && slices.ContainsFunc(a.Owed, func(o sandbox.OwedAppend) bool {
			return o.Call.Seq == seq
		}) {
			n++
		}
	}
	return n
}

// A SESSION'S END WAITS FOR THE CARRY STILL OUT, AND TAKES UP WHAT IT HANDS
// BACK. The run's collection asks the job's session to carry what it owes; a
// stop, or the seat leaving this node, ends the same session while that carry
// is still out, in the outage that made the call owed. The end's carry is the
// session's last of its own: so it waits for the collection's, carries the
// call that one hands back, and returns only once it has — the paths that end
// a session read or delete the run's record next. And each line is true as it
// is said: the collection's refused carry leaves the call owed to the end's,
// which comes; the end's, refused as well, says the session has ended and the
// call is on no record.
//
// Mutation: let a carry run beside the one still out (no carrying lock), and
// the session's end finds nothing owed and returns while the collection's
// carry is out, and no carry of the session's own takes up the call it hands
// back.
func TestASessionsEndWaitsForTheCarryStillOutAndTakesUpWhatItHandsBack(t *testing.T) {
	t.Parallel()
	store := &heldStore{entered: make(chan struct{}), release: make(chan struct{})}
	l, logs := readingLedger(store)
	store.down.Store(true)
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
		Seq: 1, Digest: strings.Repeat("a1", 32)}); err == nil {
		t.Fatal("an append landed while the store was down")
	}
	store.hold.Store(true)
	settled := make(chan bool, 1)
	go func() { settled <- l.Settle(context.Background(), "run-1") }()
	<-store.entered
	type end struct {
		carried bool
		// carries is how many carries of the call the store had been asked
		// for by the time the end returned.
		carries int
	}
	ended := make(chan end, 1)
	go func() {
		carried := l.End(context.Background(), "run-1")
		ended <- end{carried: carried, carries: store.carries(1)}
	}()
	close(store.release)
	if <-settled {
		t.Fatal("the collection's carry landed while the store was down")
	}
	got := <-ended
	if got.carried {
		t.Fatal("the session's end's carry landed while the store was down")
	}
	// MORE THAN THE COLLECTION'S OWN ATTEMPTS by the time the end returned:
	// every one of those, and then the end's own carry of what they handed
	// back.
	if got.carries <= appendAttempts {
		t.Errorf("the session's end returned once the store had been asked for %d carries of the owed call, "+
			"no more than the collection's %d attempts: it returned while that carry was out, and no carry "+
			"of its own took up what that one handed back", got.carries, appendAttempts)
	}
	lines := ledgerLines(logs, "bridge_owed_calls_unrecorded")
	want := []string{"post_message at place 1, digest a1a1a1a1"}
	if len(lines) != 2 || lines[0]["session_ended"] != false || lines[1]["session_ended"] != true ||
		!reflect.DeepEqual(named(lines[0]), want) || !reflect.DeepEqual(named(lines[1]), want) {
		t.Errorf("the refused carries were said as %v; want the collection's leaving the call owed to the "+
			"end's carry, and then the end's saying the session ended with the call on no record\n%s",
			lines, logs)
	}
	if n := l.owing(); n != 1 {
		t.Errorf("the ended session owes %d appends, want the one call, owed to a call in flight alone", n)
	}
}
