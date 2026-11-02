package sandboxtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of the log beside a run's record whose subject is the gap between
// the record's write and the log's: a launch writes the row first and opens
// the log to its job second, and what the store promises has to hold where the
// second write fails, or where the row changes between the two. Each runs
// against a store built over a coordination store that stages that gap
// ([stagedRuns]), which only the store's own two writes can reach.
var stagedLogCases = []struct {
	name string
	fn   func(*testing.T, *stagedRuns, sandbox.PendingStore)
}{
	{"ACarriedOnRoundKeepsItsCallsPastAnOpeningThatFailed",
		testACarriedOnRoundKeepsItsCallsPastAnOpeningThatFailed},
	{"ALaunchWhoseOwnRowEndsUnderItsOpeningIsRefused",
		testALaunchWhoseOwnRowEndsUnderItsOpeningIsRefused},
}

// stagedRuns is a coordination store's run records with the next write of a
// log staged: before it reaches the store, before runs once, and fail, if
// set, is what it answers instead of landing.
type stagedRuns struct {
	records

	mu     sync.Mutex
	before func(ctx context.Context)
	fail   error
}

// records is the store's run records, embedded under a name of their own:
// embedded under coord.SandboxRuns, the field would take the name of the
// listing method and hide it.
type records = coord.SandboxRuns

// stage arms the next log write: before runs first, then the write answers
// fail where it is set, and goes through otherwise.
func (r *stagedRuns) stage(before func(ctx context.Context), fail error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before, r.fail = before, fail
}

func (r *stagedRuns) MutateSandboxRunLog(ctx context.Context, turnID string,
	decide func(coord.Record, bool) ([]byte, error)) (coord.Record, bool, error) {

	r.mu.Lock()
	before, fail := r.before, r.fail
	r.before, r.fail = nil, nil
	r.mu.Unlock()
	if before != nil {
		before(ctx)
	}
	if fail != nil {
		return coord.Record{}, false, fail
	}
	return r.records.MutateSandboxRunLog(ctx, turnID, decide)
}

// A CARRIED-ON ROUND KEEPS ITS CALLS PAST AN OPENING THAT FAILED. A relaunch
// writes the row, naming its job, and then opens the log to it; where that
// second write fails — or its node stops before it — the row names a job the
// log was never opened to, and the log still holds the round's calls under the
// job before. The launch is an error and is retried, continuing the same
// round: the retry keeps those calls, and takes the cut the person's answer
// owed them, because what the opening keeps is decided by the round the row
// names ([sandbox.PendingRun.Round]), never by the job the row held just
// before it. And the row the failed launch left reads no calls of the job
// before it ([sandbox.PendingStore.BridgeLog]).
//
// Mutation: judge what the opening keeps by the job the row held before the
// reset (carry the round as the replaced job), and the retry starts the round
// afresh, losing the submission made before the question; clear the answer a
// relaunch carries on, and the retry takes no cut; drop BridgeLog's job
// filter, and the failed launch's row reads the asking job's calls as its own.
func testACarriedOnRoundKeepsItsCallsPastAnOpeningThatFailed(t *testing.T, runs *stagedRuns,
	s sandbox.PendingStore,
) {
	ctx := t.Context()
	const turn = "t-opening-failed"
	asking := mustLaunched(t, s, run(turn))
	for seq, name := range []string{"submit_work", "ask_and_wait"} {
		if ok, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: asking,
			Call: sandbox.BridgeCall{Name: name, Seq: seq + 1, At: base}}); err != nil || !ok {
			t.Fatalf("AppendBridgeCall(%s) = %v, %v", name, ok, err)
		}
	}
	if got, err := s.RecordAsk(ctx, turn, asking, branchAsk()); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %q, %v; want it recorded", got, err)
	}
	carried := run(turn)
	carried.Continues = true

	down := errors.New("the coordination store is unreachable")
	runs.stage(nil, down)
	if _, err := s.BeginLaunch(ctx, carried, sandbox.Fence{}); !errors.Is(err, down) {
		t.Fatalf("a launch whose log could not be opened = %v, want the store's error", err)
	}
	failed := mustLogged(t, s, turn)
	if failed.LaunchID == asking {
		t.Fatal("the failed launch left the row on the asking job, so this asserts nothing about the gap")
	}
	if len(failed.Log.Calls) != 0 || failed.Log.Launch != failed.LaunchID {
		t.Errorf("the failed launch's row read %d calls of the job before it (log of %q), want none "+
			"under its own job %q", len(failed.Log.Calls), failed.Log.Launch, failed.LaunchID)
	}

	retried, err := s.BeginLaunch(ctx, carried, sandbox.Fence{})
	if err != nil {
		t.Fatalf("the retried launch: %v", err)
	}
	got := mustLogged(t, s, turn)
	if len(got.Log.Calls) != 2 || retried.OpenedAfter != 2 {
		t.Fatalf("the retried launch kept %d of the round's calls and numbers after %d, want both "+
			"and after 2", len(got.Log.Calls), retried.OpenedAfter)
	}
	if got.Log.AnsweredThrough != 2 {
		t.Errorf("the retried launch cut the round at %d, want 2: the answer the failed opening "+
			"owed the round", got.Log.AnsweredThrough)
	}
}

// A LAUNCH WHOSE OWN ROW ENDS UNDER ITS OPENING IS REFUSED, never created
// again. Once a launch's create or reset has landed, the row names its job and
// carries no decided ending (a reset refuses one), so what removes it before
// the log is opened is an ending decided for that very job — a person's stop —
// and its finish. Created again, the row would carry no ending and the launch
// would start a box for a turn that was stopped; the launch is refused
// [sandbox.ErrRunEnding] instead, and the row stays gone.
//
// Mutation: go round BeginLaunch's loop again on an opening that found no row,
// after the create or after the reset, and the stopped run's row is back.
func testALaunchWhoseOwnRowEndsUnderItsOpeningIsRefused(t *testing.T, runs *stagedRuns,
	s sandbox.PendingStore,
) {
	ctx := t.Context()
	stop := func(turn string) func(context.Context) {
		return func(ctx context.Context) {
			decided, ok, err := s.DecideEnding(ctx, turn, sandbox.Decision{
				License: everyJob(sandbox.Fence{}, []string{sandbox.StatusLaunching})})
			if err != nil || !ok {
				t.Errorf("DecideEnding under the opening = %v, %v", ok, err)
				return
			}
			if _, gone, err := s.Finish(ctx, turn, decided.Ending.ID); err != nil || !gone {
				t.Errorf("Finish under the opening = %v, %v", gone, err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		first bool // the run has a row before this launch
	}{
		{"created", false},
		{"reset", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := "t-ended-under-" + tc.name
			if tc.first {
				mustLaunched(t, s, run(turn))
			}
			runs.stage(stop(turn), nil)
			if _, err := s.BeginLaunch(ctx, run(turn), sandbox.Fence{}); !errors.Is(err, sandbox.ErrRunEnding) {
				t.Fatalf("a launch whose row was stopped under its opening = %v, want ErrRunEnding", err)
			}
			if row, found, err := s.Get(ctx, turn); err != nil || found {
				t.Fatalf("the stopped run's row after its launch: found %v (ending %+v), %v; want it "+
					"gone", found, row.Ending, err)
			}
		})
	}
}
