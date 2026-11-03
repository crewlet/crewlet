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
// run's row keeps only in part.
var noted = map[string]any{"item": "GP-12", "status": "draft", "note": strings.Repeat("<x>", 1<<20/3)}

// published is the call between the two noted ones, which asked for something
// else.
var published = map[string]any{"item": "GP-12", "status": "published"}

// rowCall is the call a run's row holds for a call made with args: its
// arguments kept as the row keeps them (the left-out note's marker in place of
// the note) and read back as the engine reads a row's, its identity the digest
// the bridge admitted it under, and the size of what it was sent with.
func rowCall(t *testing.T, seq int, args map[string]any) runner.BridgedCall {
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
// call after the first, so it is a second operation. On the row, the first
// call's note is left out (its arguments kept in part) and the second call was
// dropped from the middle of a long log, kept only as its identity. Seeded by
// what the row KEPT, the first counted as another call and the second as none;
// seeded by identity, the resumed run counts what the run that never parked
// counts, on every path a run's calls are read back on: its collection, its
// continuation after a person's answer, and its reopening on a node that took
// it over.
//
// Mutation: seed a kept call from the row's arguments rather than its digest,
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

	cut := rowCall(t, 1, noted)
	if cut.ArgsBytes == 0 {
		t.Fatal("the noted call's arguments fit the row whole; the case needs them cut")
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
		if _, err := r.ReopenAgentRun(context.Background(), state, bridged, dropped, runner.Bridged{},
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
		if _, err := r.ReopenAgentRun(context.Background(), closed, nil, nil, runner.Bridged{}, false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		check(t, calls)
	})
}

// submissionOf is submit_work's arguments with a summary of n bytes: a round
// blocked, which every turn may report.
func submissionOf(n int) map[string]any {
	return map[string]any{"outcome": "blocked", "summary": strings.Repeat("s", n),
		"evidence": "the build is red"}
}

// A COLLECTED RUN WHOSE NEWEST SUBMISSION ITS ROW DID NOT KEEP WHOLE IS RESCUED,
// never reported with the row's marker as its summary, nor with the submission
// before it as its outcome. The row keeps the newest successful submission
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

// A LIVE SUBMISSION PAST WHAT THE ROW KEEPS WHOLE IS REFUSED WHILE THE RUN CAN
// SHORTEN IT, naming its size; one within it is taken. The collection replays a
// submission off the run's row, so one the row could not keep whole would be a
// round collected as having said nothing.
//
// Mutation: take a submission whatever its size, and the oversized one is
// recorded as the run's outcome.
func TestALiveSubmissionPastWhatTheRowKeepsIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{})
	reopened, err := r.ReopenAgentRun(context.Background(), execstate.State{Version: execstate.Version, Round: 1},
		nil, nil, runner.Bridged{}, false)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	for name, tc := range map[string]struct {
		summary int
		refused bool
	}{
		"past what the row keeps whole": {sandbox.MaxSubmissionBytes, true},
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
