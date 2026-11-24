package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN OPENING MARKS LATE WHAT THE LOG TOOK AFTER ITS BRIEFS' READ, THROUGH THE
// LAUNCH. A relaunch builds its briefs from one read of the round's log and
// hands its launch that read (runner.AgentRunRequest.BriefRead); the store's
// opening of the job's log marks late every call taken after it. So a call of
// the asking job landing after the read and before the opening is listed late,
// and the call the read saw is not.
//
// Mutation: drop the read from the request the launch hands the store, and the
// call no brief told is listed as the asking job's own.
func TestAnOpeningMarksLateWhatTheLogTookAfterItsBriefsRead(t *testing.T) {
	t.Parallel()
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, "t-brief-read", read)
	asking.call(t, "read_page")
	briefRead := asking.calls(t, "t-brief-read").Read()

	// THE ASKING JOB'S CALL LANDING WHILE THE RELAUNCH IS READIED.
	asking.call(t, "read_page")
	carried := asking.launchFrom(t, &runner.Continuation{SessionID: "sess-ask", Brief: "use main"}, &briefRead)

	logged := carried.calls(t, "t-brief-read")
	if len(logged.Calls) != 2 || logged.Calls[0].Late || !logged.Calls[1].Late {
		t.Fatalf("the round's log lists %+v, want the call the briefs' read saw as it was and the one "+
			"taken after it late", logged.Calls)
	}
}

// A RESUME HANDS THE RUNNER THE READ ITS COLLECTION MADE: the job the log was
// open to, the calls the round's log had taken at it, before the coordinator adds the
// call a person approved that the row alone holds, and what the log had
// absorbed (sandbox.ResumeRequest.BridgedRead) — never how many calls the
// resume lists, since a relaunch's opening marks late, or carries into a new
// round, every call after that read, while the log is open to that job.
//
// Mutation: hand the runner the read with no job, and a send-back's opening
// carries nothing.
func TestAResumeHandsTheRunnerTheCountItsReadSaw(t *testing.T) {
	t.Parallel()
	// THE READ SAW TWO CALLS; the resume lists a third, the call a person
	// approved that the row alone held, added after the read.
	logged := sandbox.BridgeLog{Round: "round-1", Calls: []sandbox.BridgeCall{
		{Name: "read_page", Seq: 1, Appended: 1},
		{Name: "post_message", Seq: 2, Appended: 2},
		{Name: "add_issue_comment", Seq: 3, Appended: 3, Approved: true},
	}}
	read := sandbox.LogRead{Launch: "job-1", Took: 2,
		Absorbed: []sandbox.ElidedCall{{Name: "read_file", Digest: "d1", Calls: 2}}}
	got := runnerResume(resumeInput{Bridged: logged, BridgedRead: read}, nil, nil, nil).BridgedRead
	if got.Launch != read.Launch || got.Took != read.Took || len(got.Absorbed) != 1 ||
		got.Absorbed[0].Calls != 2 {
		t.Errorf("the runner is handed the read %+v, want the collection's %+v, never the 3 calls the resume "+
			"lists", got, read)
	}
}

// A CALL LANDING WHILE A RELAUNCH IS READIED IS TOLD TO THE NEXT CONTINUATION,
// AND THE CALL A PERSON APPROVED NEVER AS LATE, through the real launcher and
// coordination store. The worker's call and then the approved call land after
// the resume's read and before the relaunch's opening, which marks only the
// first late. The next relaunch, in the carried-on job's box, is told the
// worker's call once under the late calls' words and the approved call never
// there: once where its harness was cut off, not at all where it ended its own
// run (the copy holds the brief that told it; [collectedFarBelow]).
//
// Mutation: drop the read from the resume or the launch, mark the approved call
// late, drop the late mark a resume hands the runner, read a late call as one a
// copy holds, or tell an own run's copy nothing, and a case goes red.
func TestACallLandingWhileARelaunchIsReadiedIsToldToTheNextContinuation(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]sandbox.HarnessEnd{
		"its harness ended its own run": sandbox.HarnessEndOwnRun,
		"its harness was cut off":       sandbox.HarnessEndCutOff,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			const turnID = "t-landed-while-readied"
			read := newHeldTool("read_page")
			close(read.release)
			worker := newHeldTool("delegate")
			asking := bridgedRig(t, turnID, read, worker).launch(t, nil)
			asking.call(t, "read_page")
			// THE ASKING JOB'S WORKER, still running as the answer is claimed.
			landed := make(chan struct{})
			go func() {
				defer close(landed)
				asking.call(t, "delegate")
			}()
			<-worker.entered
			launch := asking.row(t, turnID).LaunchID
			parkedOnAHeldCall(t, asking.pending, turnID, launch)
			if _, ok, err := asking.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(launch),
				sandbox.Fence{}); err != nil || !ok {
				t.Fatalf("ClaimForResume = %v, %v", ok, err)
			}
			// THE RESUME'S READ, which every brief of the relaunch is built from,
			// as the runner is handed it.
			logged := asking.calls(t, turnID)
			resumed := runnerResume(resumeInput{Run: asking.row(t, turnID), Bridged: logged,
				BridgedRead: logged.Read()}, nil, nil, nil)

			// WHILE THE RELAUNCH IS READIED: the worker lands as the asking
			// job's own, and the call the person approved is made.
			close(worker.release)
			<-landed
			if _, first, err := asking.pending.MarkHeldCall(ctx, turnID, launch, sandbox.Fence{}); err != nil ||
				!first {
				t.Fatalf("MarkHeldCall = %v, %v", first, err)
			}
			if ok, err := asking.pending.RecordHeldResult(ctx, turnID, launch, sandbox.HeldMade{Result: "posted",
				Call: sandbox.BridgeCall{Name: "vendor_post", Origin: "mcp:vendor", Server: "vendor",
					Source: "mcp:vendor"}}, sandbox.Fence{}); err != nil || !ok {
				t.Fatalf("RecordHeldResult = %v, %v", ok, err)
			}

			// THE RELAUNCH, handed the read as the runner hands it on
			// (runner.AgentRunRequest.BriefRead).
			carried := asking.launchFrom(t, &runner.Continuation{SessionID: "sess-ask", Brief: "use main"},
				&resumed.BridgedRead)
			next := carried.calls(t, turnID)
			listed := map[string]sandbox.BridgeCall{}
			for _, call := range next.Calls {
				listed[call.Name] = call
			}
			if len(next.Calls) != 3 || listed["read_page"].Late || !listed["delegate"].Late ||
				listed["vendor_post"].Late || !listed["vendor_post"].Approved {
				t.Fatalf("the round's log lists %+v, want the call the read saw as it was, the worker's call "+
					"late and the approved call not", next.Calls)
			}

			// THE NEXT RELAUNCH, a person's answer carrying the round on in the
			// carried-on job's own box.
			row := carried.row(t, turnID)
			row.Launch.HarnessEnd = end
			collectedFarBelow(&row.Launch)
			brief := relaunchedInPlace(t, carried, runnerResume(resumeInput{Run: row, Bridged: next,
				BridgedRead: next.Read(), State: execstate.State{Version: execstate.Version, Round: 1},
				Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "sess-carried", BoxKept: true}}, nil, nil, nil))
			const lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
			at := strings.Index(brief, lateHeading)
			if at < 0 || !strings.Contains(brief[at:], "- delegate(") || strings.Count(brief, "- delegate(") != 1 {
				t.Errorf("the next continuation is not told the worker's call once, under the late calls' "+
					"words:\n%s", brief)
			}
			approved := strings.Count(brief, "- vendor_post(")
			if (at >= 0 && strings.Contains(brief[at:], "- vendor_post(")) || approved > 1 ||
				(approved == 1) == end.EndedOwnRun() {
				t.Errorf("the next continuation is told the approved call %d time(s), want it never under the "+
					"late calls' words, and once only where its copy may lack the brief that told it:\n%s",
					approved, brief)
			}
		})
	}
}

// parkedOnAHeldCall parks job launch of run turnID, on store, on a call to
// `vendor_post` held for approval, its arguments sealed under a keyring of the
// case's own, with a person's answer allowing it once recorded: [heldParked]
// for a job a launch has already opened.
func parkedOnAHeldCall(t *testing.T, store sandbox.PendingStore, turnID, launch string) {
	t.Helper()
	ctx := t.Context()
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000003", LaunchID: launch,
		Server: "vendor", Tool: "vendor_post", Raw: "post", Digest: "sha256:d1", HeldAt: time.Now().UTC()}
	var err error
	if held.Arguments, err = sandbox.NewCallSeal(valueRing(t, "k1")).SealArguments(turnID, held,
		map[string]any{"body": "done"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe call vendor_post on the vendor MCP server?"
	if got, err := store.RecordAsk(ctx, turnID, launch, sandbox.Ask{Question: question,
		Decision: sandbox.HeldDecision(question, nil), To: sandbox.AskToManager, Held: &held}); err != nil ||
		got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %v, %v", got, err)
	}
	if err := store.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if _, ok, err := store.RecordAnswer(ctx, turnID, launch, sandbox.RecordedAnswer{
		Sealed: []byte("sealed: once"), Via: types.AnswerViaOperator, EventIDs: []string{"r1"}},
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
}

// relaunchedInPlace is the brief a relaunch built from resume hands the session
// copy its job's box holds ([runner.Continuation.Brief]), through the runner b's
// company builds and a launcher that launches nothing. Its probe answers with a
// claude-sonnet-4-5 member's window, one this build knows (the rig's model is
// not), so a copy recorded far below its threshold is judged by its harness's
// end alone ([collectedFarBelow]).
func relaunchedInPlace(t *testing.T, b bridgedRun, resume *runner.Resume) string {
	t.Helper()
	return relaunchedContinuation(t, b, resume).Brief
}

// relaunchedContinuation is [relaunchedInPlace]'s whole continuation, for a
// case that launches it.
func relaunchedContinuation(t *testing.T, b bridgedRun, resume *runner.Resume) *runner.Continuation {
	t.Helper()
	recorder := &probingRecorder{basis: runner.SessionBasis{
		Window: claudeWindowOf("claude-sonnet-4-5")}}
	r, err := b.launcher.company.RunnerFor(t.Context(), b.handle, tools.NewRegistry(), RunnerInput{
		Task: "fix the failing test", Reply: turn.ToolReply(""), Launcher: recorder,
		Sessions: testSessions(t), Resume: resume,
		Turn: runner.Turn{RunID: b.launcher.turn.RunID, Context: b.launcher.turn},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	// REFUSED BY THE RECORDER, which launches nothing: what it was handed is
	// the case's.
	_, _, _ = r.Resume(t.Context(), nil)
	handed := recorder.handed()
	if handed == nil || handed.Continue == nil || handed.Continue.SessionID == "" {
		t.Fatalf("the relaunch continues no copy in its job's box: %+v", handed)
	}
	return handed.Continue
}

// collectedFarBelow records on l the count a collection writes of a Claude Code
// copy far below its threshold under [relaunchedInPlace]'s window, its thread
// restarting nowhere (sandbox.LaunchRecord.ResumedCount, BasisRestart). Without
// a count a copy is told as a cut-off one whatever its harness's end
// (internal/agent/runner's sessionHolds), so a case about an own run's copy
// records it.
func collectedFarBelow(l *sandbox.LaunchRecord) {
	l.BasisRestart = new("")
	l.ResumedCount = &sandbox.ResumedCount{Tokens: 10_000, Restart: new("")}
}

// relaunchRecorder is an executor launcher that keeps the request it is handed
// and launches nothing.
type relaunchRecorder struct {
	mu  sync.Mutex
	req *runner.AgentRunRequest
}

func (r *relaunchRecorder) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) (runner.Launched, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.req = &req
	return runner.Launched{}, errors.New("the recorder launches nothing")
}

// handed is the request the recorder was handed, nil for none.
func (r *relaunchRecorder) handed() *runner.AgentRunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.req
}
