package engine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// appendingStore is a pending store that records each bridged append and
// answers it as a row holding job "launch-1" would.
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
// session to before its box existed, so a call that outlives its job is refused
// by the store rather than landing on the next job's record. And every append
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
	l.bind("launch-1")
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
// ANSWERED AND WHAT IT WAS — everything the bridge measured, in the row's own
// shape, so a resumed executor's record can place its calls on a timeline, in
// the order the run made them, and name the server each came from — read off
// the row, which is the only place a collection, often another process, can
// read them; and the identity the bridge admitted it under, which a later
// segment's call log counts it by whatever the row keeps of its arguments.
//
// Mutation: drop any of the six from the append, and this goes red.
func TestABridgedCallsRowCarriesItsTimingAndWhoAnswered(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	l.bind("launch-1")
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

// WHAT A ROW KEPT OF A CALL IS WHAT THE RESUMED TURN READS: its identity on the
// turn's record of the call, and the size of the whole arguments and output
// where the row kept less, which the collection's replay decides by — and the
// calls the row dropped, by identity, in the turn's own shape.
//
// Mutation: drop the digest or either size on the way to the runner, or the
// tally, and a resumed segment seeds a cut call as another and replays a cut
// submission as the run's.
func TestWhatARowKeptOfACallIsWhatTheResumedTurnReads(t *testing.T) {
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
	// the tool called with none.
	for _, call := range bridgedCalls([]sandbox.BridgeCall{{Name: "close_issue", Held: true, Failed: true},
		{Name: "close_issue", Approved: true}}) {
		if !call.Withheld || call.Digest != "" || len(call.Args) != 0 {
			t.Errorf("a held or approved call reads %+v, want it withheld", call.Call)
		}
	}
}

// A SESSION WITH NO METER SENDS NO SPEND, and a call its job no longer takes
// is not an error to the box: the store's "not recorded" is the ordinary end
// of a job.
func TestABridgeSessionWithNoMeterSendsNoSpend(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	l.bind("launch-0")
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

// A RUN'S SUBMISSION, ONCE ON ITS ROW, TELLS THE WAITER THAT DRIVES IT TO LOOK
// NOW: its harness is about to exit, and its completion is found within seconds of
// that rather than up to a poll interval later. Only the submission, only one
// the gate let through, and only one recorded on the run's own job — a
// refused submission is no end, and one its job no longer takes names a job
// that is over.
//
// Mutation: nudge on every recorded call, on a refused submission or ask, or
// on one the row did not take, and this goes red.
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
			l.bind(tc.launch)
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

// A BRIDGED CALL'S RECORD THAT DID NOT LAND IS TRIED AGAIN, AND THEN OWED. The
// call has run and its answer goes back to the box whatever the row says, but a
// call missing from the row is one a resumed segment's call log never counts —
// and a later call asking for something else then derives its operation id.
// So a failed append is tried again before the answer goes back, which rides a
// store's blip; and a call whose every attempt failed is owed by identity, its
// own work's calls with it, and carried to the row by the session's next
// append, counted there as a call of the round the row does not list.
//
// Mutation: append once and give up, and a blip loses the call; drop a call
// whose attempts all failed, and the next append carries nothing of it.
func TestABridgedCallsRecordThatDidNotLandIsTriedAgainAndThenOwed(t *testing.T) {
	t.Parallel()
	blip := &flakyStore{fail: func(attempt int, _ sandbox.BridgeAppend) bool { return attempt == 1 }}
	l := newBridgeLedger(blip, nil)
	l.bind("launch-1")
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "post_message"},
		Seq: 1, Digest: "d1"}); err != nil {
		t.Fatalf("an append the store refused once = %v; want it landed on its retry", err)
	}
	if len(blip.landed) != 1 || blip.landed[0].Call.Digest != "d1" {
		t.Fatalf("the store took %+v, want the call once", blip.landed)
	}

	down := &flakyStore{fail: func(_ int, a sandbox.BridgeAppend) bool { return a.Call.Seq == 1 }}
	l = newBridgeLedger(down, nil)
	l.bind("launch-1")
	worker := []toolloop.Called{{Tool: "leave_channel", Digest: "w1"}}
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "delegate", Failed: true},
		Seq: 1, Digest: "d1", Absorbed: worker}); err == nil {
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
	wantOwed := []sandbox.ElidedCall{{Name: "delegate", Digest: "d1", Calls: 1, Failed: 1}}
	wantAbsorbed := []sandbox.ElidedCall{{Name: "leave_channel", Digest: "w1", Calls: 1}}
	if !reflect.DeepEqual(carried.Unrecorded, wantOwed) || !reflect.DeepEqual(carried.Absorbed, wantAbsorbed) {
		t.Errorf("the next append carried %+v owed and %+v absorbed, want %+v and %+v", carried.Unrecorded,
			carried.Absorbed, wantOwed, wantAbsorbed)
	}
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{Call: tools.Call{Name: "read_page"},
		Seq: 3, Digest: "d3"}); err != nil {
		t.Fatalf("a third append: %v", err)
	}
	if again := down.landed[1]; len(again.Unrecorded) != 0 || len(again.Absorbed) != 0 {
		t.Errorf("the append after the one that carried the owed call carried %+v and %+v again",
			again.Unrecorded, again.Absorbed)
	}
}
