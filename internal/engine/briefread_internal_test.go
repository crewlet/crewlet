package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
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
