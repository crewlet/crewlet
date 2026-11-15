package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A RESUME READS WHAT THE JOB'S COPY CARRIED, off the job's own record, whatever
// that record says the figure came from: the job's own request as its
// collection read it, or a figure carried along the copy's lineage where the
// job made none (a capture's, a source a later build names), since a relaunch
// that continues the copy in the job's box sends that copy's whole context
// again before its brief either way. A record that holds none hands none.
//
// Mutation: leave the record's measure out of the resume (runnerResume not
// reading it), and every row but the last goes red.
func TestAResumeReadsWhatTheJobsCopyCarried(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		measure *sandbox.SessionMeasure
		want    int64
	}{
		"the job's own request": {measure: &sandbox.SessionMeasure{Tokens: 4320, Source: sandbox.MeasureRequest,
			Launch: "job-1"}, want: 4320},
		"a capture's, carried": {measure: &sandbox.SessionMeasure{Tokens: 61_000, Source: sandbox.MeasureCapture,
			Launch: "job-0"}, want: 61_000},
		"a source a later build names": {measure: &sandbox.SessionMeasure{Tokens: 70_000,
			Source: "a-later-builds-source"}, want: 70_000},
		"none": {},
	} {
		in := resumeInput{Run: sandbox.PendingRun{Launch: sandbox.LaunchRecord{Context: tc.measure}}}
		if got := runnerResume(in, nil, nil, nil).ContextTokens; got != tc.want {
			t.Errorf("%s: the resume hands the runner %d tokens of the copy, want %d", name, got, tc.want)
		}
	}
}

// AN IN-BOX CONTINUATION IS HELD TO ITS COPY'S MEASURE, end to end, in a company
// that keeps no session, where no capture measures the copy: the job's record
// holds what its collection found its newest request carried
// (sandbox.LaunchRecord.Context), the resume an answer drives reads it
// ([runnerResume]), the relaunch carrying the round on in the job's box hands
// its launch that copy's context (runner.Continuation.ContextTokens), and the
// launch's floor for the copy is that context with the brief that continues it,
// never the static floor beside the brief alone, which the copy's first request
// would overrun by its whole context.
//
// Mutation: leave the record's measure out of the resume, and the copy is held
// to the static floor with its brief.
func TestAnInBoxContinuationIsHeldToItsCopysMeasure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const turnID = "t-copy-measured"
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, turnID, read)
	asking.call(t, "read_page")
	row := asking.row(t, turnID)
	parkedOnAHeldCall(t, asking.pending, turnID, row.LaunchID)
	if _, ok, err := asking.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(row.LaunchID),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	logged := asking.calls(t, turnID)
	resumed := asking.row(t, turnID)
	// THE JOB'S RECORD AS ITS COLLECTION WROTE IT: a harness that ended its own
	// run, whose newest request carried a context near a window's whole.
	const copied = 150_000
	resumed.Launch.HarnessEnd = sandbox.HarnessEndOwnRun
	resumed.Launch.Context = &sandbox.SessionMeasure{Tokens: copied, Source: sandbox.MeasureRequest,
		Launch: row.LaunchID}
	handed := relaunchHanded(t, asking.launcher.company, asking.handle, asking.launcher.turn,
		runnerResume(resumeInput{Run: resumed, Bridged: logged,
			BridgedRead: logged.Appends(), State: execstate.State{Version: execstate.Version, Round: 1},
			Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
				SessionID: "sess-ask", BoxKept: true}}, nil, nil, nil))
	if handed.Continue.ContextTokens != copied {
		t.Errorf("the relaunch hands its launch %d tokens of the copy's context, want the record's %d",
			handed.Continue.ContextTokens, copied)
	}
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-haiku-4-5"})
	_, floor := asking.launcher.continuedFloor(ctx, *handed, member, true)
	if want := staticFloor(asking.launcher.company).continuing(copied,
		harness.EstimateTokens(handed.Continue.Brief)); floor != want {
		t.Errorf("the launch is held to %+v, want %+v: the copy's context with the brief that continues it", floor,
			want)
	}
}
