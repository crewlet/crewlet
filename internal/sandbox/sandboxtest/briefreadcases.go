package sandboxtest

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of the read a relaunch's briefs were built from
// ([sandbox.PendingRun.BriefRead]), past which the opening of a job carrying the
// round on marks the round's calls late.
var briefReadCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ACallTakenAfterTheBriefsReadIsMarkedLateByTheOpening", testACallTakenAfterTheBriefsReadIsMarkedLateByTheOpening},
}

// A CALL TAKEN AFTER THE BRIEFS' READ IS MARKED LATE BY THE OPENING. A relaunch
// that carries the round on builds its briefs from one read of the round's log,
// then readies its launch — a probe of the kept session, a rewrite of long calls
// by the seat's auxiliary model, the call a person approved — before its opening
// moves the log on. A call of the job it replaces that lands in between is taken
// as that job's own: none of the briefs told it, and a later capture of the new
// job, placed by its own read, would count it among the calls its session holds.
// So the opening marks every call of the round the log took after the read late,
// as it marks one taken after it — but the call a person approved, which the
// relaunch's lead tells — and leaves the calls the read saw as they were. A
// launch that knows no read marks none.
//
// Mutation: open the log without the briefs' read, or mark the approved call
// with the rest, and this goes red.
func testACallTakenAfterTheBriefsReadIsMarkedLateByTheOpening(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-late-before-opening"
	asking := heldParked(t, s, turn)
	round := mustGet(t, s, turn).Round
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: asking, Round: round,
		Call: madeAt("read_page", 1, 1)}) {
		t.Fatal("the asking job's own call was not recorded")
	}
	if _, ok, err := s.RecordAnswer(ctx, turn, asking, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, turn, sandbox.RecordedAnswerTail(asking), sandbox.Fence{}); err != nil ||
		!ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	// THE READ THE RELAUNCH'S BRIEFS ARE BUILT FROM.
	read := mustLogged(t, s, turn).Log.Appends()

	// WHILE THE RELAUNCH IS READIED: the asking job's worker lands, as that
	// job's own, and the call the person approved is made and recorded.
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: asking, Round: round,
		Call: madeAt("delegate", 2, 2)}) {
		t.Fatal("the asking job's worker was not recorded")
	}
	if _, first, err := s.MarkHeldCall(ctx, turn, asking, sandbox.Fence{}); err != nil || !first {
		t.Fatalf("MarkHeldCall = %v, %v", first, err)
	}
	if ok, err := s.RecordHeldResult(ctx, turn, asking, heldMade("sealed-result"), sandbox.Fence{}); err != nil ||
		!ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}

	carried := run(turn)
	carried.Continues, carried.BriefRead = true, &read
	next := mustLaunched(t, s, carried)
	listsExactly(t, mustLogged(t, s, turn).Log.Calls, []wantCall{
		{"read_page", 1, 1, false},
		{"delegate", 2, 2, true},
		{"tr_close_issue", 3, 3, false},
	})

	// AND A LAUNCH THAT KNOWS NO READ MARKS NONE: the carried-on job's own
	// call stays its own as the round is carried on again.
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: next, Round: round,
		Call: madeAt("post_message", 4, 4)}) {
		t.Fatal("the carried-on job's own call was not recorded")
	}
	again := run(turn)
	again.Continues = true
	mustLaunched(t, s, again)
	listsExactly(t, mustLogged(t, s, turn).Log.Calls, []wantCall{
		{"read_page", 1, 1, false},
		{"delegate", 2, 2, true},
		{"tr_close_issue", 3, 3, false},
		{"post_message", 4, 4, false},
	})
}
