package engine

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
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

// A BRIDGED CALL'S ROW SAYS WHEN IT RAN, FOR HOW LONG, IN WHAT ORDER AND WHO
// ANSWERED — everything the bridge measured, in the row's own shape, so a
// resumed executor's record can place its calls on a timeline, in the order
// the run made them, and name the server each came from — read off the row,
// which is the only place a collection, often another process, can read them.
//
// Mutation: drop any of the five from the append, and this goes red.
func TestABridgedCallsRowCarriesItsTimingAndWhoAnswered(t *testing.T) {
	t.Parallel()
	store := &appendingStore{}
	l := newBridgeLedger(store, nil)
	l.bind("launch-1")
	started := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	if err := l.Append(t.Context(), "run-1", mcpbridge.Call{
		Call: tools.Call{Name: "list_issues", Args: map[string]any{"repo": "core"}, Output: "[]"},
		Seq:  4, StartedAt: started, Duration: 1500 * time.Millisecond,
		Origin: "mcp", Server: "github",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(store.appends) != 1 {
		t.Fatalf("%d appends, want 1", len(store.appends))
	}
	got := store.appends[0].Call
	want := sandbox.BridgeCall{Name: "list_issues", Args: `{"repo":"core"}`, Output: "[]",
		Seq: 4, StartedAt: started, DurationMS: 1500, Origin: "mcp", Server: "github"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the row is %+v, want %+v", got, want)
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
