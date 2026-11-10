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
// LAUNCH. A relaunch that carries the round on builds its briefs from one read
// of the round's log and hands its launch how many calls that read saw
// (runner.Continuation.BriefRead); the launch hands it on to the store, whose
// opening of the job's log marks late every call of the round taken after it.
// So a call of the asking job that lands while the relaunch is readied — after
// the read, before the opening, as that job's own — is listed late, and the
// call the read saw is not.
//
// Mutation: drop the read from the continuation the launch hands the store, and
// the call no brief told is listed as the asking job's own.
func TestAnOpeningMarksLateWhatTheLogTookAfterItsBriefsRead(t *testing.T) {
	t.Parallel()
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, "t-brief-read", read)
	asking.call(t, "read_page")
	briefRead := asking.calls(t, "t-brief-read").Appends()

	// THE ASKING JOB'S CALL LANDING WHILE THE RELAUNCH IS READIED.
	asking.call(t, "read_page")
	carried := asking.launch(t, &runner.Continuation{SessionID: "sess-ask", Brief: "use main",
		BriefRead: &briefRead})

	logged := carried.calls(t, "t-brief-read")
	if len(logged.Calls) != 2 || logged.Calls[0].Late || !logged.Calls[1].Late {
		t.Fatalf("the round's log lists %+v, want the call the briefs' read saw as it was and the one "+
			"taken after it late", logged.Calls)
	}
}

// A RESUME HANDS THE RUNNER THE COUNT ITS READ SAW. The coordinator counts the
// calls the round's log had taken at the read a resume is built from, before it
// adds the call a person approved that the row alone holds
// (sandbox.ResumeRequest.BridgedRead): every call a brief built from that read
// can tell. A relaunch that carries the round on hands that count to its
// launch, whose opening marks late every call of the round the log took after
// it. So the runner is handed the read's own count, never how many calls the
// resume lists.
//
// Mutation: hand the runner no count, and a relaunch's opening marks late every
// call the round's log took before it.
func TestAResumeHandsTheRunnerTheCountItsReadSaw(t *testing.T) {
	t.Parallel()
	// THE READ SAW TWO CALLS; the resume lists a third, the call a person
	// approved that the row alone held, added after the read.
	logged := sandbox.BridgeLog{Calls: []sandbox.BridgeCall{
		{Name: "read_page", Seq: 1, Appended: 1},
		{Name: "post_message", Seq: 2, Appended: 2},
		{Name: "add_issue_comment", Seq: 3, Appended: 3, Approved: true},
	}}
	resumed := runnerResume(resumeInput{Bridged: logged, BridgedRead: 2}, nil, nil)
	if resumed.BridgedRead != 2 || len(resumed.Bridged) != 3 {
		t.Errorf("the runner is handed a read of %d calls beside %d listed, want the 2 the read saw beside "+
			"the 3 the resume lists", resumed.BridgedRead, len(resumed.Bridged))
	}
}

// A CALL LANDING WHILE A RELAUNCH IS READIED IS TOLD TO THE NEXT CONTINUATION,
// AND THE CALL A PERSON APPROVED NEVER AS LATE: end to end, through the real
// launcher and the coordination store. The asking job's worker is still running
// as a person's answer is claimed and the resume reads the round's log, so no
// brief of the relaunch is built from it; it lands, as that job's own, while the
// relaunch is readied, and the call the person approved is made after it, both
// before the relaunch's opening moves the log on. The resume hands the runner
// the count its read saw, which the relaunch hands its launch, so the opening
// marks the worker's call late and leaves the approved call, which the
// relaunch's lead tells, as it was. The next relaunch, carrying the round on in
// the carried-on job's own box, is then told the worker's call once, under the
// late calls' words, whatever that job's harness's end; and the approved call
// never under them, and at most once: not at all where the harness ended its
// own run, its copy holding the brief that told it, and once among the round's
// calls where it was cut off.
//
// Mutation: drop the read from the resume the runner is handed or from the
// launch, mark the approved call at the opening with the rest, drop the mark
// from the calls a resume hands the runner, read a late call as one a copy
// holds, or tell an own run's copy nothing in place, and a case goes red.
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
			// and the count of it the runner is handed.
			logged := asking.calls(t, turnID)
			resumed := runnerResume(resumeInput{Run: asking.row(t, turnID), Bridged: logged,
				BridgedRead: logged.Appends()}, nil, nil)

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
			// (runner.Continuation.BriefRead).
			carried := asking.launch(t, &runner.Continuation{SessionID: "sess-ask", Brief: "use main",
				BriefRead: &resumed.BridgedRead})
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
			brief := relaunchedInPlace(t, carried, runnerResume(resumeInput{Run: row, Bridged: next,
				BridgedRead: next.Appends(), State: execstate.State{Version: execstate.Version, Round: 1},
				Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "sess-carried", BoxKept: true}}, nil, nil))
			const lateHeading = "## Calls that finished after the run that made them was replaced"
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

// relaunchedInPlace is the brief a relaunch built from resume hands the copy of
// the session its job's box holds ([runner.Continuation.Brief]): the runner b's
// company builds for its seat, as a resumed segment's is, relaunching through a
// launcher that keeps what it is handed and launches nothing.
func relaunchedInPlace(t *testing.T, b bridgedRun, resume *runner.Resume) string {
	t.Helper()
	recorder := &relaunchRecorder{}
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
	return handed.Continue.Brief
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
