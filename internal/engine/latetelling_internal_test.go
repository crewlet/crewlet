package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// The tellings a resume hands its runner ([runner.Resume.Tellings]): the late
// calls of the round's log a job's own session is known to hold
// ([sandbox.RunSteer.Tellings]).

// THE RESUME HANDS ONLY THE TELLINGS OF ITS LOG'S ROUND. A telling names a place
// in its round's log, and a row an older build's new-round reset wrote can still
// hold one of the round before, which names no call of this log; and a log that
// names no round names no telling's.
//
// And a telling a hook's step recorded reaches the runner as one
// ([runner.Telling.Hook]).
//
// Mutation: hand every telling on the row (tellings-any-round), or drop the
// hook's mark (telling-hook-dropped), and the runner is handed another.
func TestTheResumeHandsOnlyTheTellingsOfItsLogsRound(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{Steer: sandbox.RunSteer{Tellings: []sandbox.CallTelling{
		{Round: "job-0", Appended: 3, Launch: "job-0", Place: 1},
		{Round: "job-1", Appended: 2, Launch: "job-2", Place: 4, Hook: true},
	}}}
	got := runnerResume(resumeInput{Run: run, Bridged: sandbox.BridgeLog{Round: "job-1"}}, nil, nil, nil).Tellings
	if want := []runner.Telling{{Appended: 2, Place: 4, Hook: true}}; !slices.Equal(got, want) {
		t.Errorf("the resume hands tellings %+v, want the log's round's alone, a hook's as one, %+v", got, want)
	}
	if got := runnerResume(resumeInput{Run: run}, nil, nil, nil).Tellings; len(got) != 0 {
		t.Errorf("a resume of a log naming no round hands tellings %+v, want none", got)
	}
}

// AN OPENCODE JOB THAT CONFIRMED A LATE CALL IS NOT TOLD IT BY ITS COPY. A call
// the job's harness gave up on lands while the job works: the store hands it to
// the job as an item, the job's inbox serves it at a poll and reads it on the
// plugin's confirmation, which records the job's session as holding the call.
// The job then asks a person and its harness ends its own run; the relaunch
// carrying the round on in its box, through the real resume, tells that copy
// nothing of the call. A copy cut off may lack what it was handed, and is told.
//
// Mutation: drop the tellings from the resume (resume-without-tellings), and
// the copy that took the call is told it again.
func TestAnOpenCodeJobThatConfirmedALateCallIsNotToldItByItsCopy(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]sandbox.HarnessEnd{
		"its harness having ended its own run": sandbox.HarnessEndOwnRun,
		"its harness cut off":                  sandbox.HarnessEndCutOff,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			const turnID = "t-telling-in-box"
			read := newHeldTool("read_page")
			close(read.release)
			asking := launchBridged(t, turnID, read)
			asking.call(t, "read_page")
			row := asking.row(t, turnID)
			l := &bridgeLedger{store: asking.pending}
			l.bind(row.NamedJob(0, "", 0))
			if err := l.Record(ctx, turnID, mcpbridge.Call{Call: tools.Call{Name: "delegate",
				Output: "the workers are done"}, Seq: 2, StartedAt: time.Now().UTC(),
				Abandoned: true}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			var item sandbox.RunNote
			for _, n := range asking.row(t, turnID).Steer.Notes {
				if n.LateCall != nil {
					item = n
				}
			}
			if item.LateCall == nil || item.For != row.LaunchID {
				t.Fatalf("the premise: the call its harness gave up on is handed to job %s as %+v", row.LaunchID,
					item)
			}
			// SERVED AT A POLL, AND READ ON THE PLUGIN'S CONFIRMATION, by an
			// inbox over the run's store (the rig's engine binds none).
			runs, _ := steersOver(t, steerRing, asking.pending)
			inbox := boundInbox(t, runs, asking.pending, turnID, row.LaunchID)
			viewHolds(t, inbox, 1)
			if notes, _ := inbox.Wait(ctx, 0); len(notes) != 1 || notes[0].ID != item.ID {
				t.Fatalf("the poll was answered %+v, want the item", notes)
			}
			if err := inbox.Read(ctx, []string{item.ID}); err != nil {
				t.Fatalf("Read: %v", err)
			}
			parkedOnAHeldCall(t, asking.pending, turnID, row.LaunchID)
			if _, ok, err := asking.pending.ClaimForResume(ctx, turnID,
				sandbox.RecordedAnswerTail(row.LaunchID), sandbox.Fence{}); err != nil || !ok {
				t.Fatalf("ClaimForResume = %v, %v", ok, err)
			}
			logged := asking.calls(t, turnID)
			resumed := asking.row(t, turnID)
			if len(resumed.Steer.Tellings) != 1 {
				t.Fatalf("the premise: the row records tellings %+v, want the confirmed call's",
					resumed.Steer.Tellings)
			}
			resumed.Launch.HarnessEnd = end
			collectedFarBelow(&resumed.Launch)
			brief := relaunchedInPlace(t, asking, runnerResume(resumeInput{Run: resumed, Bridged: logged,
				BridgedRead: logged.Read(), State: execstate.State{Version: execstate.Version, Round: 1},
				Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "sess-ask", BoxKept: true}}, nil, nil, nil))
			if told := strings.Contains(brief, "- delegate("); told != (end == sandbox.HarnessEndCutOff) {
				t.Errorf("the copy in its box is told the call its session took: %v, want %v:\n%s", told,
					end == sandbox.HarnessEndCutOff, brief)
			}
		})
	}
}
