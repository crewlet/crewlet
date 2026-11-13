package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN LOST WITH ITS HOST CARRIES ITS ROUND ON IN A FRESH RUN TOLD ITS CALLS, end
// to end: the round's job made a call over the bridge, which the run's log took,
// and was lost when the engine's restart killed it; the seat's holder claims the
// loss and resumes the turn, which relaunches the round in a session of its own.
// That run remembers nothing the round did, so the brief its box starts it on
// tells, after how the run before it was cut off, every call of the round.
//
// Mutation: build the reseed without the round's calls, as before, and the
// second job's brief names none of them.
func TestARunLostWithItsHostCarriesItsRoundOnInAFreshRunToldItsCalls(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-lost-told", RunID: "run-lost-told", WorkSince: time.Now().UTC(),
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	first := rig.row(t, "run-lost-told")
	runner := rig.runners[first.Harness]
	pending := e.sandbox.Load().pending
	// THE CALL THE JOB MADE before its host took it down, as its bridge's
	// ledger appends one.
	if ok, err := pending.AppendBridgeCall(t.Context(), "run-lost-told", sandbox.BridgeAppend{
		Launch: first.LaunchID, Round: first.Round,
		Call: sandbox.BridgeCall{Name: "post_comment", Seq: 1, Args: `{"text":"halfway through the fix"}`,
			Output: "posted", StartedAt: time.Now().UTC()},
	}); err != nil || !ok {
		t.Fatalf("the job's call = %v, %v", ok, err)
	}

	// THE ENGINE'S RESTART TOOK THE JOB, and its host's waiter said so.
	runner.Lose(sandbox.Result{Text: "half way through the fix", InputTokens: 300, OutputTokens: 30})
	if marked, err := pending.MarkLost(t.Context(), "run-lost-told", first.LaunchID,
		sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: first.LaunchNode}); err != nil || !marked {
		t.Fatalf("MarkLost = %v, %v", marked, err)
	}
	completion := types.ExecutorFinished{
		Agent: first.AgentID, AgentHandle: first.AgentHandle, RoleName: first.Role,
		TurnID: first.TurnID, WorkKey: first.WorkKey, LaunchID: first.LaunchID,
		SandboxID: first.SandboxID, Harness: first.Harness,
	}
	ev := events.New(completion, events.TraceContext{TraceID: first.TraceID, ParentSpanID: first.SpanID})
	if err := e.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev); err != nil {
		t.Fatalf("claiming the lost job: %v", err)
	}

	started := runner.Started()
	if len(started) != 2 {
		t.Fatalf("%d jobs were started, want the lost one and the one carrying its round on", len(started))
	}
	again := started[1]
	cut := strings.Index(again.Brief, "was cut off")
	called := strings.Index(again.Brief, "## Calls the runs before you made on this task")
	if again.Resume != "" || cut < 0 || called < cut ||
		!strings.Contains(again.Brief[called:], `- post_comment({"text":"halfway through the fix"}) → success`) {
		t.Fatalf("the second job continues session %q with a brief that does not tell, after how the run "+
			"before it was cut off, the call that run made:\n%s", again.Resume, again.Brief)
	}
}

// A FRESH JOB'S LEFT-OUT READS REACH ITS COPY'S NEXT BRIEF, end to end: a job
// whose launch handed its run a fresh brief that counted two of the round's
// reads without listing them, its read cap spent, says so on its own record
// (sandbox.LaunchRecord.BriefOmitted), which the resume an answer drives reads
// into the runner's terms ([runnerResume]); the relaunch carrying the round on
// in the job's own box, its harness having ended its own run, tells that copy
// those two reads, which it holds only as a count, and no other call it holds.
//
// Mutation: leave the record's set out of the resume (runnerResume not reading
// it), and the copy is told none of them.
func TestAFreshJobsLeftOutReadsReachItsCopysNextBrief(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const turnID = "t-left-out-in-box"
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, turnID, read)
	for range 3 {
		asking.call(t, "read_page")
	}
	row := asking.row(t, turnID)
	parkedOnAHeldCall(t, asking.pending, turnID, row.LaunchID)
	if _, ok, err := asking.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(row.LaunchID),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	logged := asking.calls(t, turnID)
	if len(logged.Calls) != 3 {
		t.Fatalf("the round's log lists %+v, want the job's three reads", logged.Calls)
	}
	resumed := asking.row(t, turnID)
	// THE JOB'S RECORD AS ITS LAUNCH'S ATTACH AND ITS COLLECTION WROTE IT: a
	// fresh brief that left out the round's last two reads, and a harness that
	// ended its own run.
	resumed.Launch.Brief, resumed.Launch.BriefOmitted = sandbox.BriefFresh, []int{2, 3}
	resumed.Launch.HarnessEnd = sandbox.HarnessEndOwnRun
	brief := relaunchedInPlace(t, asking, runnerResume(resumeInput{Run: resumed, Bridged: logged,
		BridgedRead: logged.Appends(), State: execstate.State{Version: execstate.Version, Round: 1},
		Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
			SessionID: "sess-ask", BoxKept: true}}, nil, nil))
	owed := strings.Index(brief, "## Reads an earlier message counted but did not list")
	if owed < 0 || strings.Count(brief, "- read_page(") != 2 || strings.Index(brief, "- read_page(") < owed {
		t.Errorf("the copy in its box is not told the two reads its job's brief counted, and only those, under "+
			"their own words:\n%s", brief)
	}
}

// A LAUNCH CARRIES WHAT EACH BRIEF LEFT OUT TO THE JOB'S RECORD. The runner hands
// its launch what each brief it may hand its run counted without listing of
// the round it carries on: the kept capture's continuation, and the fresh brief
// its box falls back to. The engine carries both to the sandbox layer
// ([sandboxSession], [sandboxContinuation]), whose launch records, on the job it
// opens, the set of the brief it handed and the fresh brief's beside it.
//
// Mutation: leave either set out of what the engine hands the sandbox layer,
// and the job's record names none of it.
func TestALaunchCarriesWhatEachBriefLeftOutToTheJobsRecord(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 0, 0)
	req := launchRequest(k.l, nil)
	req.Brief = "the fresh brief"
	req.Session = &runner.SessionContinuation{Basis: k.l.ProbeSession(k.ctx),
		Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours",
		Told: sandbox.BriefTold{Omitted: []int{1}}}
	req.Continue = &runner.Continuation{Fresh: sandbox.BriefTold{Omitted: []int{2}}}
	if _, err := k.l.LaunchExecutor(k.ctx, req); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	row, _, err := k.e.sandbox.Load().pending.Get(k.ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !row.Launch.Brief.Continues() || !slices.Equal(row.Launch.BriefOmitted, []int{1}) ||
		!slices.Equal(row.Launch.FreshOmitted, []int{2}) {
		t.Errorf("the job's record says brief %q left out %v, and the fresh one %v; want the kept session's "+
			"continuation, [1], beside the fresh brief's [2]", row.Launch.Brief, row.Launch.BriefOmitted,
			row.Launch.FreshOmitted)
	}
}

// WHAT EVERY BRIEF LEFT OUT REACHES THE SANDBOX LAYER. The runner hands its launch
// the reads each brief it may hand its run counted without listing them: the
// copy's in its job's box and the fresh brief's beside it (runner.Continuation),
// and the kept capture's, or the base's where the turn continues the session its
// conversation's last turn left (runner.SessionContinuation). The engine carries
// every one of them into the sandbox layer's terms ([sandboxContinuation],
// [sandboxSession]), whose launch records the set of the brief it hands: a set
// dropped here leaves its job's record naming none of the reads that brief
// counted past the cap, so the next brief to its session counts them held and
// tells them to nobody.
//
// Mutation: leave out the copy's set, the fresh brief's, the kept capture's or
// the base's, and a case goes red.
func TestWhatEveryBriefLeftOutReachesTheSandboxLayer(t *testing.T) {
	t.Parallel()
	c := sandboxContinuation(&runner.Continuation{SessionID: "sess-1", Brief: "carry on in your box",
		Told: sandbox.BriefTold{Omitted: []int{1}}, Fresh: sandbox.BriefTold{Omitted: []int{2}}})
	if c == nil || !slices.Equal(c.Told.Omitted, []int{1}) || !slices.Equal(c.Fresh.Omitted, []int{2}) {
		t.Errorf("the carry-on in the job's box reaches the sandbox layer as %+v, want the copy's set, [1], "+
			"beside the fresh brief's, [2]", c)
	}
	for name, basis := range map[string]runner.SessionBasis{
		"a kept capture": {LaunchSeq: 1, CaptureSeq: 2, SessionID: "s-kept"},
		"the base":       {SessionID: "s-base", Base: &runner.BaseBasis{Turn: "t-before"}},
	} {
		s := sandboxSession(&runner.SessionContinuation{Basis: basis, Brief: "carry on",
			Replaced: "carry on in a new box", Told: sandbox.BriefTold{Omitted: []int{3}}})
		if s == nil || s.Base != (name == "the base") || !slices.Equal(s.Told.Omitted, []int{3}) {
			t.Errorf("%s's continuation reaches the sandbox layer as %+v, want its own kind with its set, [3]",
				name, s)
		}
	}
}
