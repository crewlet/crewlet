package runner_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// noted is a status write with a megabyte note: the log keeps its arguments
// only in part.
var noted = map[string]any{"item": "GP-12", "status": "draft", "note": strings.Repeat("<x>", 1<<20/3)}

// published is the different call between the two noted ones.
var published = map[string]any{"item": "GP-12", "status": "published"}

// loggedCall is what a run's log holds for a call made with args: the
// arguments as the log keeps them (a marker for a left-out note), read back as
// the engine reads them, the bridge's digest, and the whole arguments' size.
func loggedCall(t *testing.T, seq int, args map[string]any) runner.BridgedCall {
	t.Helper()
	text, err := tools.RecordArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	keptText, whole := kept.Record("the run's record").Args(text, sandbox.MaxBridgeArgsBytes)
	return runner.BridgedCall{Call: ledger.Call{Name: "set_status", Args: tools.ReadArgs(keptText),
		Result: "applied", Digest: turnctx.ArgsDigest(args)}, Seq: seq, ArgsBytes: whole}
}

// A RESUME PAST A DROPPED CALL AND A CUT ONE DERIVES EVERY ID A RUN THAT NEVER
// PARKED DERIVES. A run writes noted, then published, then (resumed) noted
// again: a second operation, one different call after the first. The log cut
// the first call's note and dropped the second to its identity, so the seed
// must go by digest, not by what the log kept, on every read-back path:
// collection, continuation after an answer, and reopening on another node.
//
// Mutation: seed a kept call from the log's arguments rather than its digest,
// and the count is two; leave the dropped identities out of the seed, and it
// is zero — the first call's operation, answered `applied` without landing.
func TestAResumePastADroppedCallAndACutOneDerivesTheIdsOfARunThatNeverParked(t *testing.T) {
	t.Parallel()
	never := turnctx.NewCallLog()
	register := func(t *testing.T, reg *tools.Registry) {
		if err := reg.RegisterWith(&statusWriter{}, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
			t.Fatal(err)
		}
	}
	reg := tools.NewRegistry()
	register(t, reg)
	surface := tools.NewSurface("execute", reg.Snapshot(), nil)
	for _, args := range []map[string]any{noted, published} {
		never.Record("set_status", surface.ArgsOf("set_status", args))
	}
	want := never.Ordinal("set_status", noted)
	if want != 1 {
		t.Fatalf("the run that never parked counts %d different calls before its third, want 1", want)
	}

	cut := loggedCall(t, 1, noted)
	if cut.ArgsBytes == 0 {
		t.Fatal("the noted call's arguments fit the log whole; the case needs them cut")
	}
	bridged := []runner.BridgedCall{cut}
	dropped := []ledger.Elided{{Name: "set_status", Digest: turnctx.ArgsDigest(published), Calls: 1}}
	state := execstate.State{Version: execstate.Version, Round: 1, ActiveTools: []string{"set_status"}}
	check := func(t *testing.T, calls *turnctx.CallLog) {
		t.Helper()
		if got := calls.Ordinal("set_status", noted); got != want {
			t.Errorf("the resumed run counts %d different calls before the repeat, the run that never "+
				"parked %d", got, want)
		}
	}

	t.Run("collected", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register,
			resume: &runner.Resume{State: state, Answer: "done", Bridged: bridged, Elided: dropped},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		check(t, calls)
	})
	t.Run("continued", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register, launcher: &playLauncher{},
			resume: &runner.Resume{State: state, Bridged: bridged, Elided: dropped, Asked: &runner.Asked{
				Question: "which item?", Answer: "GP-12", SessionID: "s-1", BoxKept: true}},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		check(t, calls)
	})
	t.Run("reopened", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register})
		if _, err := r.ReopenAgentRun(context.Background(), state, bridged, nil, dropped, nil, runner.Bridged{},
			false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		check(t, calls)
	})
	t.Run("a closed round", func(t *testing.T) {
		t.Parallel()
		// The same two calls a round earlier, in the closed round's record.
		calls := turnctx.NewCallLog()
		closed := state
		closed.Iterations = []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{cut.Call}, Elided: dropped}}
		closed.Round = 2
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register})
		if _, err := r.ReopenAgentRun(context.Background(), closed, nil, nil, nil, nil, runner.Bridged{},
			false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		check(t, calls)
	})
}

// reopenedLog is the call log a reopened run seeds: bridged, the calls the
// log lists; elided and absorbed, identities counted without listing; closed,
// the turn's closed rounds.
func reopenedLog(t *testing.T, bridged []runner.BridgedCall, elided, absorbed []ledger.Elided,
	closed []ledger.Iteration) *turnctx.CallLog {

	t.Helper()
	calls := turnctx.NewCallLog()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{calls: calls, register: func(t *testing.T, reg *tools.Registry) {
			if err := reg.RegisterWith(&statusWriter{}, tools.OriginBuiltin,
				tools.Annotations{ReadOnly: mcp.No}); err != nil {
				t.Fatal(err)
			}
		}})
	state := execstate.State{Version: execstate.Version, Round: len(closed) + 1,
		ActiveTools: []string{"set_status"}, Iterations: closed}
	if _, err := r.ReopenAgentRun(context.Background(), state, bridged, nil, elided, absorbed, runner.Bridged{},
		false); err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	return calls
}

// A TALLY'S UNNAMED COUNT IS SEEDED AGAINST EVERY LATER CALL, and a named
// identity BY ITS DIGEST. A count with no tool (a tally folded past its
// capacity) may have asked for anything, so it counts against every later
// call of every tool; a kept identity is THAT call, so a later call repeating
// it with nothing between is its retry.
//
// Mutation: skip an entry that names no tool, and a later call counts none of
// those calls; seed every tallied call as one of its tool with no identity,
// and a dropped repeat of the later call counts as another call.
func TestATallysCountsAreSeededAsTheCallsTheyWere(t *testing.T) {
	t.Parallel()
	unnamed := reopenedLog(t, nil, []ledger.Elided{{Calls: 2}}, nil, nil)
	if got := unnamed.Ordinal("set_status", published); got != 2 {
		t.Errorf("two dropped calls whose tool is not known count %d against a later call, want 2", got)
	}
	repeat := reopenedLog(t, nil,
		[]ledger.Elided{{Name: "set_status", Digest: turnctx.ArgsDigest(published), Calls: 1}}, nil, nil)
	if got := repeat.Ordinal("set_status", published); got != 0 {
		t.Errorf("a dropped call repeated with nothing between counts %d, want 0: it is the same call", got)
	}
	if got := repeat.Ordinal("set_status", noted); got != 1 {
		t.Errorf("a dropped call counts %d against a later call that asks for something else, want 1", got)
	}
}

// A WORKER'S CALLS ARE SEEDED FROM THE ROUND'S RECORD. A bridged `delegate`'s
// workers run on forks of the call log, absorbed once the wave is done, so the
// record keeps them as identities (open round and closed rounds alike), and a
// worker's set_status between two of the run's own makes the second a
// different operation, never the first one's retry.
//
// Mutation: leave the absorbed identities out of the seed — the open round's
// or a closed round's — and the repeat counts 0: the first call's operation,
// answered `applied` without landing.
func TestAWorkersCallsAreSeededFromTheRoundsRecord(t *testing.T) {
	t.Parallel()
	first := loggedCall(t, 1, noted)
	worker := []ledger.Elided{{Name: "set_status", Digest: turnctx.ArgsDigest(published), Calls: 1}}
	open := reopenedLog(t, []runner.BridgedCall{first}, nil, worker, nil)
	if got := open.Ordinal("set_status", noted); got != 1 {
		t.Errorf("the open round's worker call counts %d against the run's repeat, want 1", got)
	}
	closed := reopenedLog(t, nil, nil, nil, []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{first.Call}, Absorbed: worker}})
	if got := closed.Ordinal("set_status", noted); got != 1 {
		t.Errorf("a closed round's worker call counts %d against the run's repeat, want 1", got)
	}
}

// A CALL WHOSE ARGUMENTS ARE WITHHELD IS SEEDED AS ONE NO LATER CALL REPEATS. A
// held call and its approved one keep no arguments or digest on any record
// (the arguments ride the row sealed), so each counts as a different call of its tool:
// an over-count, the safe direction, never the tool called with no arguments.
//
// Mutation: seed a withheld call by what its record holds, and a later call
// with no arguments counts it as the same call.
func TestACallWhoseArgumentsAreWithheldIsSeededAsOneNoLaterCallRepeats(t *testing.T) {
	t.Parallel()
	withheld := []runner.BridgedCall{
		{Call: ledger.Call{Name: "set_status", Result: "asked a person", Failed: true, Withheld: true}, Seq: 1},
		{Call: ledger.Call{Name: "set_status", Withheld: true}, Seq: 2},
	}
	log := reopenedLog(t, withheld, nil, nil, nil)
	if got := log.Ordinal("set_status", map[string]any{}); got != 2 {
		t.Errorf("a held call and its approval count %d against a later call with no arguments, want 2", got)
	}
}

// A CALL WHOSE ARGUMENTS DID NOT READ BACK IS SEEDED AS ONE NO LATER CALL
// REPEATS. A call with no digest whose arguments do not read back
// (ledger.Call.ArgsUnread) counts as a different call of its tool, as a
// withheld one does, never as the tool called with no arguments.
//
// Mutation: seed such a call by the arguments it reads as, and a later call
// with no arguments counts it as the same call.
func TestACallWhoseArgumentsDidNotReadBackIsSeededAsOneNoLaterCallRepeats(t *testing.T) {
	t.Parallel()
	unread := []runner.BridgedCall{{Call: ledger.Call{Name: "set_status", Result: "applied", ArgsUnread: true},
		Seq: 1}}
	open := reopenedLog(t, unread, nil, nil, nil)
	if got := open.Ordinal("set_status", map[string]any{}); got != 1 {
		t.Errorf("the open round's call whose arguments did not read back counts %d against a later call with "+
			"no arguments, want 1", got)
	}
	closed := reopenedLog(t, nil, nil, nil, []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{unread[0].Call}}})
	if got := closed.Ordinal("set_status", map[string]any{}); got != 1 {
		t.Errorf("a closed round's call whose arguments did not read back counts %d against a later call with "+
			"no arguments, want 1", got)
	}
}

// submissionOf is a blocked submit_work (which every turn may report) with an
// n-byte summary.
func submissionOf(n int) map[string]any {
	return map[string]any{"outcome": "blocked", "summary": strings.Repeat("s", n),
		"evidence": "the build is red"}
}

// A COLLECTED RUN WHOSE NEWEST SUBMISSION ITS LOG DID NOT KEEP WHOLE IS RESCUED,
// never reported with the log's marker as its summary, nor with the earlier
// submission as its outcome. The live tool refuses a submission the log cannot
// keep whole, so only the run's own spelling takes one past it: what it
// submitted is not on the record, so the round said nothing.
//
// Mutation: replay a submission whatever its record kept, and the round's
// summary is the marker; skip only the cut one, and the earlier submission is
// reported as the round's.
func TestACollectedRunWhoseNewestSubmissionWasNotKeptIsRescued(t *testing.T) {
	t.Parallel()
	earlier := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: submissionOf(64),
		Result: "recorded"}, Seq: 1}
	newest := runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
		Args: map[string]any{"outcome": "blocked", "evidence": "the build is red",
			"summary": "(70 KiB not kept on the run's record, digest 1a2b3c4d)"},
		Result: "recorded"}, Seq: 2, ArgsBytes: 70 << 10}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		resume: &runner.Resume{State: execstate.State{Version: execstate.Version, Round: 1},
			Answer: "done", Bridged: []runner.BridgedCall{earlier, newest}},
	})
	work, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !work.Rescued || strings.Contains(work.Summary, "not kept") || work.Summary == strings.Repeat("s", 64) {
		t.Errorf("the round reads as %+v; want it rescued, reporting neither submission", work)
	}
}

// A LIVE SUBMISSION PAST WHAT THE LOG KEEPS WHOLE IS REFUSED WHILE THE RUN CAN
// SHORTEN IT; one within it is taken. Collection replays the submission off
// the log, so one not kept whole would be collected as having said nothing.
//
// Mutation: take a submission whatever its size, and the oversized one is
// recorded as the run's outcome.
func TestALiveSubmissionPastWhatTheLogKeepsIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{})
	reopened, err := r.ReopenAgentRun(context.Background(), execstate.State{Version: execstate.Version, Round: 1},
		nil, nil, nil, nil, runner.Bridged{}, false)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	for name, tc := range map[string]struct {
		summary int
		refused bool
	}{
		"past what the log keeps whole": {sandbox.MaxSubmissionBytes, true},
		"within it":                     {4 << 10, false},
	} {
		raw, err := json.Marshal(submissionOf(tc.summary))
		if err != nil {
			t.Fatal(err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatal(err)
		}
		res, err := reopened.Surface.Execute(context.Background(), llm.ToolCall{Name: runner.SubmitWorkTool,
			Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Failed != tc.refused || (tc.refused && !strings.Contains(res.Output, "keeps a submission whole")) {
			t.Errorf("%s: answered %+v, want refused %v", name, res, tc.refused)
		}
	}
}
