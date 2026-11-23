package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CALL NOBODY WAITED FOR IS TOLD TO THE COPY IN ITS BOX, end to end: a
// `delegate` the job's harness gave up on (mcpbridge.Call.Abandoned) reaches the
// session's ledger while the job is parked on its question, before the answer's
// resume reads the round's log; it is logged as the job's own call, numbered
// below every read the relaunch makes, and marked. The copy relaunched in its
// box is told it once, under the late calls' words: alone where its harness
// ended its own run (which holds every other call), apart from the round's
// other calls where cut off. Both arms' records carry a collection far below
// threshold ([collectedFarBelow]), so the harness's end alone decides.
//
// Mutation: drop the mark in the ledger or in the resume's hand-over, and a copy
// whose harness ended its own run is told nothing of a call it holds as timed out.
func TestACallNobodyWaitedForIsToldToTheCopyInItsBox(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
	for name, end := range map[string]sandbox.HarnessEnd{
		"its harness having ended its own run": sandbox.HarnessEndOwnRun,
		"its harness cut off":                  sandbox.HarnessEndCutOff,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			const turnID = "t-abandoned-in-box"
			read := newHeldTool("read_page")
			close(read.release)
			asking := launchBridged(t, turnID, read)
			asking.call(t, "read_page")
			row := asking.row(t, turnID)
			// THE JOB'S SESSION'S LEDGER, bound to the job and its round as the
			// launch binds the bridge's own.
			l := &bridgeLedger{store: asking.pending}
			l.bind(row.NamedJob(0, "", 0))
			parkedOnAHeldCall(t, asking.pending, turnID, row.LaunchID)

			// THE CALL ITS HARNESS GAVE UP ON LANDS DURING THE PARK, recorded as
			// the bridge records a call nobody waited for.
			if err := l.Record(ctx, turnID, mcpbridge.Call{Call: tools.Call{Name: "delegate",
				Output: "the workers are done"}, Seq: 2, StartedAt: time.Now().UTC(),
				Abandoned: true}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if _, ok, err := asking.pending.ClaimForResume(ctx, turnID,
				sandbox.RecordedAnswerTail(row.LaunchID), sandbox.Fence{}); err != nil || !ok {
				t.Fatalf("ClaimForResume = %v, %v", ok, err)
			}
			logged := asking.calls(t, turnID)
			if len(logged.Calls) != 2 || !logged.Calls[1].Abandoned || logged.Calls[1].Late {
				t.Fatalf("the round's log lists %+v, want the job's read and the call nobody waited for, "+
					"marked so and not late", logged.Calls)
			}
			resumed := asking.row(t, turnID)
			resumed.Launch.HarnessEnd = end
			collectedFarBelow(&resumed.Launch)
			brief := relaunchedInPlace(t, asking, runnerResume(resumeInput{Run: resumed, Bridged: logged,
				BridgedRead: logged.Read(), State: execstate.State{Version: execstate.Version, Round: 1},
				Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "sess-ask", BoxKept: true}}, nil, nil, nil))
			at := strings.Index(brief, lateHeading)
			if at < 0 || strings.Count(brief, "- delegate(") != 1 || !strings.Contains(brief[at:], "- delegate(") {
				t.Errorf("the copy in its box is not told the call nobody waited for once, under the late "+
					"calls' words:\n%s", brief)
			}
			if told := strings.Contains(brief, "- read_page("); told != (end == sandbox.HarnessEndCutOff) {
				t.Errorf("the copy in its box is told the job's read: %v, want %v:\n%s", told,
					end == sandbox.HarnessEndCutOff, brief)
			}
		})
	}
}
