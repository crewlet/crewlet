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

// noted is a status write carrying a megabyte note: a call whose arguments the
// run's log keeps only in part.
var noted = map[string]any{"item": "GP-12", "status": "draft", "note": strings.Repeat("<x>", 1<<20/3)}

// published is the call between the two noted ones, which asked for something
// else.
var published = map[string]any{"item": "GP-12", "status": "published"}

// loggedCall is the call a run's log holds for a call made with args: its
// arguments kept as the log keeps them (the left-out note's marker in place of
// the note) and read back as the engine reads a log's, its identity the digest
// the bridge admitted it under, and the size of what it was sent with.
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
// PARKED DERIVES (#477). A run wrote a status with a megabyte note, then
// another status, then — after its turn parked and was resumed — the first
// status again: a run that never parked counts the third call one different
// call after the first, so it is a second operation. In the log, the first
// call's note is left out (its arguments kept in part) and the second call was
// dropped from the middle of a long log, kept only as its identity. Seeded by
// what the log KEPT, the first counted as another call and the second as none;
// seeded by identity, the resumed run counts what the run that never parked
// counts, on every path a run's calls are read back on: its collection, its
// continuation after a person's answer, and its reopening on a node that took
// it over.
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
		// THE SAME TWO CALLS, a round earlier: the closed round carries the
		// kept call with its identity and the dropped one in its tally.
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

// reopenedLog is the call log a run reopened from its row and its log seeds:
// bridged, its calls the log lists; elided and absorbed, the identities it
// counts without listing; closed, the turn's closed rounds.
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
// identity BY ITS DIGEST. A long run's record that folded its dropped middle
// past what its tally holds keeps a count with no tool — or an older build
// dropped calls and kept nothing of them, which reads the same — and each of
// those calls asked for something a later call may not: so each counts
// against every later call, of every tool, as one that asked for something
// else. While a dropped call's identity is kept, it is THAT call: a later call
// repeating it with nothing different between is its retry, as the run's own
// process would count it.
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

// A WORKER'S CALLS ARE SEEDED FROM THE ROUND'S RECORD (#477). A bridged
// `delegate` runs its workers on forks of the run's call log, and the log
// absorbs their calls once the wave is done — so a later call of the run counts
// a worker's call between two of its own. No call of the round's record is a
// worker's, so the record keeps them as identities beside it, the open round's
// and each closed round's, and a resumed segment counts them as the run did:
// a set_status the worker made between two of the run's own makes the second
// a different operation, never the first one's retry.
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
// call held for a person's approval, and the one they approved, keep neither
// their arguments nor a digest of them on any record — the arguments ride the
// row sealed — so a resumed segment cannot know what they asked for, and
// counts each as a call of its tool that asked for something else: an
// over-count, the safe direction. Seeded by the arguments the record holds,
// which are none, the call read as the tool called with no arguments.
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

// submissionOf is submit_work's arguments with a summary of n bytes: a round
// blocked, which every turn may report.
func submissionOf(n int) map[string]any {
	return map[string]any{"outcome": "blocked", "summary": strings.Repeat("s", n),
		"evidence": "the build is red"}
}

// A COLLECTED RUN WHOSE NEWEST SUBMISSION ITS LOG DID NOT KEEP WHOLE IS RESCUED,
// never reported with the log's marker as its summary, nor with the submission
// before it as its outcome. The log keeps the newest successful submission
// whole, and the live tool refuses one past what it keeps, so this is a run
// whose own spelling took one past it: what it submitted is not on the record,
// and the round reads as one that said nothing.
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
// SHORTEN IT, naming its size; one within it is taken. The collection replays a
// submission off the run's log, so one the log could not keep whole would be a
// round collected as having said nothing.
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
