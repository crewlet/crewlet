package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PARKED CALL'S RESULT REACHES ITS SESSION BY THE RELAUNCH, WITH NO NOTE, for
// a runner that states a ceiling and reads no notes (Cursor Agent's), through
// the real launcher, bridge, ledger, store and runner: whether the model ends
// its turn or works on, its job is parked on the call, the call lands with no
// item, and the relaunch tells the session the call returned, with its result,
// once (internal/api/mcpbridge's "A call longer than its harness waits").
//
// Mutation: launch-names-no-ceiling, and nothing answers before the ceiling;
// Park unset, and the row holds no wait; no WaitsOnCall in fileLateItems, and
// the job is handed an item; Called dropped from runnerAsked, and the relaunch
// tells a person's answer.
func TestAParkedCallsResultReachesItsSessionByTheRelaunchWithNoNote(t *testing.T) {
	t.Parallel()
	for name, worksOn := range map[string]bool{"the model ends its turn": false, "the model works on": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const (
				turnID  = "t-call-ceiling"
				ceiling = 6 * time.Second
			)
			read, list := newHeldTool("read_page"), newHeldTool("list_pages")
			close(list.release)
			rig := bridgedRig(t, turnID, read, list)
			rig.jobs.Ceiling, rig.jobs.TakesNotes = ceiling, false
			run := rig.launch(t, nil)
			var freed sync.Once
			free := func() { freed.Do(func() { close(read.release) }) }
			t.Cleanup(free)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			answered := make(chan *mcp.CallToolResult, 1)
			go func() {
				res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page",
					Arguments: map[string]any{}})
				if err != nil {
					res = &mcp.CallToolResult{IsError: true}
				}
				answered <- res
			}()
			select {
			case <-read.entered:
			case <-ctx.Done():
				t.Fatal("the call never reached its tool")
			}
			select {
			case res := <-answered:
				if res.IsError || len(res.Content) != 1 ||
					!strings.Contains(res.Content[0].(*mcp.TextContent).Text, "END YOUR TURN NOW") {
					t.Fatalf("the call was answered %+v while its tool ran, want it still running and the "+
						"turn to end, not as failed", res)
				}
			case <-ctx.Done():
				t.Fatalf("the call was not answered while its tool ran, %v before its harness's ceiling of %v",
					ceiling-toolloop.ParkBefore(ceiling), ceiling)
			}
			row := run.row(t, turnID)
			if !row.WaitsOnOwnCalls() || len(row.ParkedCalls) != 1 || row.ParkedCalls[0].Tool != "read_page" ||
				row.ParkedCalls[0].Seq != 1 || row.Question != sandbox.CallWaitQuestion("read_page") ||
				!row.AskedBy(row.LaunchID) {
				t.Fatalf("the run's row parks on %+v asking %q, want its job parked on its call to read_page",
					row.ParkedCalls, row.Question)
			}
			if worksOn {
				if res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "list_pages",
					Arguments: map[string]any{}}); err != nil || res.IsError {
					t.Fatalf("a call made after the park was answered %+v, %v, want served", res, err)
				}
			}

			// THE CALL LANDS AND THE JOB ENDS: its session on the bridge ends, and
			// the bridge waits for the call it runs to be recorded.
			free()
			run.engine.bridge.Close(turnID)
			run.engine.bridge.AwaitCalls(ctx)
			logged := run.calls(t, turnID)
			i := len(logged.Calls) - 1
			if i < 0 || logged.Calls[i].Name != "read_page" || !logged.Calls[i].Abandoned ||
				logged.Calls[i].Output != "read_page done" {
				t.Fatalf("the run's log holds %+v, want the parked call with what its tool did, as one "+
					"nobody waited for", logged.Calls)
			}
			row = run.row(t, turnID)
			for _, n := range row.Steer.Notes {
				if n.LateCall != nil {
					t.Errorf("the job parked on its call is handed it as an item: %+v", n)
				}
			}
			if !row.WaitsOnCall() {
				t.Fatalf("the run's row no longer waits on the call once it landed: %+v", row.WaitsFor)
			}

			// THE PARK IS ANSWERED, and the relaunch it carries continues the job's
			// own copy, its harness having ended its own run.
			if err := run.pending.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("MarkAwaiting: %v", err)
			}
			if _, ok, err := run.pending.RecordAnswer(ctx, turnID, row.LaunchID, sandbox.RecordedAnswer{
				Sealed: []byte("sealed: returned"), Via: types.AnswerViaOperator, EventIDs: []string{"r1"}},
				sandbox.Fence{}); err != nil || !ok {
				t.Fatalf("RecordAnswer = %v, %v", ok, err)
			}
			if _, ok, err := run.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(row.LaunchID),
				sandbox.Fence{}); err != nil || !ok {
				t.Fatalf("ClaimForResume = %v, %v", ok, err)
			}
			logged = run.calls(t, turnID)
			resumed := run.row(t, turnID)
			resumed.Launch.HarnessEnd = sandbox.HarnessEndOwnRun
			collectedFarBelow(&resumed.Launch)
			brief := relaunchedInPlace(t, run, runnerResume(resumeInput{Run: resumed, Bridged: logged,
				BridgedRead: logged.Read(), State: execstate.State{Version: execstate.Version, Round: 1},
				Asked: &sandbox.AskedRun{Question: row.Question, Answer: "Your call to read_page returned.",
					SessionID: "sess-ask", BoxKept: true,
					Called: []types.CallReturned{{Tool: "read_page", Seq: 1}}, CallsReturned: true}}, nil, nil, nil))
			if !strings.Contains(brief, "## Your call returned") || strings.Count(brief, "read_page done") != 1 ||
				strings.Contains(brief, "answered:") {
				t.Errorf("the relaunch does not tell the session its call returned, with what it returned, "+
					"once:\n%s", brief)
			}
		})
	}
}
