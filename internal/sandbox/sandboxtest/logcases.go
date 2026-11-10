package sandboxtest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
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
	{"AnApprovedCallReachesItsRoundsLogPastAnAppendThatFailed",
		testAnApprovedCallReachesItsRoundsLogPastAnAppendThatFailed},
	{"AnEndingReadsTheTurnsCallsPastAnOpeningThatFailed",
		testAnEndingReadsTheTurnsCallsPastAnOpeningThatFailed},
}

// stagedRuns is a coordination store's run records with the next write of a
// log staged: before it reaches the store, before runs once, and fail, if
// set, is what it answers instead of landing. And rowAsks counts every
// read-decide-write of a run's record itself, for a case whose subject is a
// write that must leave the record alone.
type stagedRuns struct {
	records

	mu     sync.Mutex
	before func(ctx context.Context)
	fail   error

	rowAsks atomic.Int64
}

// MutateSandboxRun counts each read-decide-write of a run's record
// ([stagedRuns.rowAsks]) and makes it as the records would.
func (r *stagedRuns) MutateSandboxRun(ctx context.Context, turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool, error) {

	r.rowAsks.Add(1)
	return r.records.MutateSandboxRun(ctx, turnID, decide)
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

// MutateSandboxRunPart stages only the LOG's writes: the attach that starts a
// job writes its handed set beside the record too, and a stage it consumed
// would never reach the opening it was armed for.
func (r *stagedRuns) MutateSandboxRunPart(ctx context.Context, turnID string, part coord.RunPart,
	decide func(coord.Record, bool) ([]byte, error)) (coord.Record, bool, error) {

	if part != coord.RunLog {
		return r.records.MutateSandboxRunPart(ctx, turnID, part, decide)
	}
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
	return r.records.MutateSandboxRunPart(ctx, turnID, part, decide)
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
			ctx := t.Context()
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

// AN APPROVED CALL REACHES ITS ROUND'S LOG PAST AN APPEND THAT FAILED. The
// result of a call a person approved is recorded on the row, and the call
// appended to the log after it: two keys, written in no one step. Where the
// append fails — or its node stops before it — the call is still the round's:
// an ending reads it off the row ([sandbox.PendingStore.TurnLog]), the reset
// that opens the next job carries it to the opening ([sandbox.OwedCall]), and
// where that opening fails too the retried launch still appends it, once.
// Read nowhere, the reply a person approved reads as never sent, which every
// reader of whether the turn replied would report.
//
// Mutation: drop the owed call from resetLaunch's opening, and the job that
// carries the work on keeps no approved call; drop it from the row once the
// reset has carried it, and the retried launch past a failed opening keeps
// none; read the row's held call nowhere in TurnLog, and the ending before the
// relaunch reads no reply.
func testAnApprovedCallReachesItsRoundsLogPastAnAppendThatFailed(t *testing.T, runs *stagedRuns,
	s sandbox.PendingStore,
) {
	ctx := t.Context()
	const turn = "t-approved-unlogged"
	launch := heldParked(t, s, turn)
	if _, ok, err := s.RecordAnswer(ctx, turn, launch, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, turn, sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if _, first, err := s.MarkHeldCall(ctx, turn, launch, sandbox.Fence{}); err != nil || !first {
		t.Fatalf("MarkHeldCall = %v, %v", first, err)
	}
	down := errors.New("the coordination store is unreachable")
	runs.stage(nil, down)
	if _, err := s.RecordHeldResult(ctx, turn, launch, heldMade("sealed-result"), sandbox.Fence{}); !errors.Is(err, down) {
		t.Fatalf("a result whose call could not be appended = %v, want the store's error", err)
	}
	approved := func(log sandbox.BridgeLog) int {
		n := 0
		for _, call := range log.Calls {
			if call.Approved && call.Name == "tr_close_issue" {
				n++
			}
		}
		return n
	}
	turnLog := func(what string) sandbox.BridgeLog {
		t.Helper()
		// The row is read here rather than through mustGet, which takes the
		// test's own context: one reader reads a row and its log under one.
		row, found, err := s.Get(ctx, turn)
		if err != nil || !found {
			t.Fatalf("the row of %s %s: found %v, %v", turn, what, found, err)
		}
		log, err := s.TurnLog(ctx, row)
		if err != nil {
			t.Fatalf("TurnLog %s: %v", what, err)
		}
		return log
	}
	row := mustLogged(t, s, turn)
	if row.HeldCall == nil || !row.HeldCall.Made() || approved(row.Log) != 0 {
		t.Fatalf("the failed append left the row's held call %+v and %d approved calls in the log, "+
			"want the result recorded and the log without the call, or this asserts nothing",
			row.HeldCall, approved(row.Log))
	}
	if got := approved(turnLog("before the relaunch")); got != 1 {
		t.Fatalf("an ending before the relaunch reads %d approved calls, want the one on the row", got)
	}

	carried := run(turn)
	carried.Continues = true
	runs.stage(nil, down)
	if _, err := s.BeginLaunch(ctx, carried, sandbox.Fence{}); !errors.Is(err, down) {
		t.Fatalf("a relaunch whose log could not be opened = %v, want the store's error", err)
	}
	if got := approved(turnLog("past the failed opening")); got != 1 {
		t.Fatalf("an ending past the failed opening reads %d approved calls, want the one the reset "+
			"carried", got)
	}
	if _, err := s.BeginLaunch(ctx, carried, sandbox.Fence{}); err != nil {
		t.Fatalf("the retried relaunch: %v", err)
	}
	if got := approved(mustLogged(t, s, turn).Log); got != 1 {
		t.Fatalf("the job carrying the work on keeps %d approved calls, want the call once", got)
	}
	mustBeginLaunch(t, s, run(turn))
	if prior := mustLogged(t, s, turn).Log.PriorCalls; !slices.Contains(prior, "tr_close_issue") {
		t.Fatalf("a later round finds %v of the turn's earlier calls, want the approved call", prior)
	}
}

// AN ENDING READS THE TURN'S CALLS PAST AN OPENING THAT FAILED. A relaunch
// writes the row, naming its job, and opens the log to it in a second write;
// where that second write fails, or its node stops first, the row names a job
// the log was never opened to, and such a row is reaped as lost or stopped by
// a person as it stands. Its ending reads the turn's calls as that opening
// would have left them ([sandbox.PendingStore.TurnLog]): a new round's earlier
// rounds folded in, and a carried-on round's calls kept. Read as no calls, a
// turn whose first round posted its reply is recorded as one that never did,
// while a resume's read of the same row stays empty
// ([sandbox.PendingStore.BridgeLog]), never replaying another job's calls.
//
// Mutation: answer TurnLog as BridgeLog does for a log on another job, and
// both endings read no reply.
func testAnEndingReadsTheTurnsCallsPastAnOpeningThatFailed(t *testing.T, runs *stagedRuns,
	s sandbox.PendingStore,
) {
	for _, tc := range []struct {
		name      string
		continues bool
	}{
		{"a new round", false},
		{"the round carried on", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			turn := "t-ending-unopened-" + map[bool]string{false: "new", true: "carried"}[tc.continues]
			first := mustLaunched(t, s, run(turn))
			if ok, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: first,
				Call: sandbox.BridgeCall{Name: "vendor_post", Seq: 1, At: base}}); err != nil || !ok {
				t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
			}
			next := run(turn)
			next.Continues = tc.continues
			down := errors.New("the coordination store is unreachable")
			runs.stage(nil, down)
			if _, err := s.BeginLaunch(ctx, next, sandbox.Fence{}); !errors.Is(err, down) {
				t.Fatalf("a relaunch whose log could not be opened = %v, want the store's error", err)
			}
			row := mustLogged(t, s, turn)
			if row.LaunchID == first || len(row.Log.Calls) != 0 || len(row.Log.PriorCalls) != 0 {
				t.Fatalf("the failed relaunch's row (job %s) reads %+v for a resume, want no call of "+
					"another job", row.LaunchID, row.Log)
			}
			log, err := s.TurnLog(ctx, row.PendingRun)
			if err != nil {
				t.Fatalf("TurnLog: %v", err)
			}
			kept := slices.ContainsFunc(log.Calls, func(c sandbox.BridgeCall) bool { return c.Name == "vendor_post" })
			folded := slices.Contains(log.PriorCalls, "vendor_post")
			if log.Launch != row.LaunchID || (tc.continues && !kept) || (!tc.continues && (!folded || kept)) {
				t.Fatalf("the ending reads %+v, want vendor_post among the round's calls (carried on) "+
					"or the turn's earlier ones (a new round)", log)
			}
		})
	}
}
