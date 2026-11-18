package sandboxtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a call that OUTLIVES ITS JOB: one the bridge answers after its
// job's harness died, gave up on it or was replaced, by which time a relaunch
// may have opened the run's log to the next job. Where that relaunch carried
// the round on, the call is a late call of the round ([sandbox.BridgeCall.Late]);
// where it started a new round, its record is a late call of that round, with
// no place (testARecordOfAnEarlierRoundIsListedLateInTheRoundAfterIt), or, where
// it landed before that round's opening, carried into it by the opening
// (testANewRoundCarriesTheCallsItsCollectionDidNotRead).
var lateCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ALateCallOfTheRoundIsListedMarkedAndPaidByNoJob", testALateCallOfTheRoundIsListedMarkedAndPaidByNoJob},
	{"ALateCallAskedAgainIsListedOnce", testALateCallAskedAgainIsListedOnce},
	{"ALateCallKeepsItsMarkThroughEveryWriteOfItsRound", testALateCallKeepsItsMarkThroughEveryWriteOfItsRound},
	{"ALateSubmissionNeverTakesTheRoundsWholeShare", testALateSubmissionNeverTakesTheRoundsWholeShare},
	{"OfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole", testOfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole},
	{"TheCallAPersonApprovedIsNeverLate", testTheCallAPersonApprovedIsNeverLate},
	{"ACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob", testACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob},
	{"WhatALateCallOrOneNobodyWaitedForCommittedIsTheRounds",
		testWhatALateCallOrOneNobodyWaitedForCommittedIsTheRounds},
	{"ARecordOfAnEarlierRoundIsListedLateInTheRoundAfterIt", testARecordOfAnEarlierRoundIsListedLateInTheRoundAfterIt},
	{"ABeginOrWithdrawalOfAnEarlierRoundIsNeverTaken", testABeginOrWithdrawalOfAnEarlierRoundIsNeverTaken},
	{"ARecordOfAnEarlierRoundAskedAgainIsListedOnce", testARecordOfAnEarlierRoundAskedAgainIsListedOnce},
	{"TwoCallsOfAnEarlierRoundAnsweredWithoutRunningAreBothListed",
		testTwoCallsOfAnEarlierRoundAnsweredWithoutRunningAreBothListed},
	{"AnEarlierRoundsCallMovesNoPlaceOfTheRound", testAnEarlierRoundsCallMovesNoPlaceOfTheRound},
	{"WhatAnEarlierRoundsCallCommittedIsTheNextRounds", testWhatAnEarlierRoundsCallCommittedIsTheNextRounds},
	{"ANewRoundCarriesTheCallsItsCollectionDidNotRead", testANewRoundCarriesTheCallsItsCollectionDidNotRead},
	{"ANewRoundCarriesTheAbsorbedCallsItsCollectionDidNotRead",
		testANewRoundCarriesTheAbsorbedCallsItsCollectionDidNotRead},
	{"ANewRoundStartsWithTheTallyOfTheRoundBefore", testANewRoundStartsWithTheTallyOfTheRoundBefore},
	{"ACarriedCallAskedAgainIsListedOnce", testACarriedCallAskedAgainIsListedOnce},
}

// lateStagedCases are the late-call cases whose subject is what the store asks
// of the run's record, which only the records it is built on can show
// ([stagedRuns]).
var lateStagedCases = []struct {
	name string
	fn   func(*testing.T, *stagedRuns, sandbox.PendingStore)
}{
	{"ALateCallWritesNoJobsSpend", testALateCallWritesNoJobsSpend},
	{"AReadIsAppliedOnlyToTheLogOfTheJobItRead", testAReadIsAppliedOnlyToTheLogOfTheJobItRead},
}

// carryRoundOn relaunches turnID's run carrying its round on — as a loss's
// relaunch, an answered question's, a replaced sign-in's and a refused
// session's do — and answers the new job, after checking it is of the round
// the row held before.
func carryRoundOn(t *testing.T, s sandbox.PendingStore, turnID string) string {
	t.Helper()
	round := mustGet(t, s, turnID).Round
	next := run(turnID)
	next.Continues = true
	job := mustLaunched(t, s, next)
	if got := mustGet(t, s, turnID).Round; got != round || round == "" {
		t.Fatalf("the relaunch works in round %q, want the round it carried on, %q", got, round)
	}
	return job
}

// sendBack relaunches turnID's run in a new round, as a send-back does, and
// answers the new job, after checking its round is not the one the row held.
func sendBack(t *testing.T, s sandbox.PendingStore, turnID string) string {
	t.Helper()
	return sendBackFrom(t, s, turnID, nil)
}

// sendBackFrom is [sendBack] handing the opening read, the read of the log the
// send-back's briefs were built from ([sandbox.PendingRun.BriefRead]).
func sendBackFrom(t *testing.T, s sandbox.PendingStore, turnID string, read *sandbox.LogRead) string {
	t.Helper()
	round := mustGet(t, s, turnID).Round
	next := run(turnID)
	next.BriefRead = read
	job := mustLaunched(t, s, next)
	if got := mustGet(t, s, turnID).Round; got == round || got == "" {
		t.Fatalf("the send-back works in round %q, want a new one, not %q", got, round)
	}
	return job
}

// madeAt is a bridged call as its session hands it to the log: the seq'th the
// bridge admitted, handed to its tool at second sec of the case.
func madeAt(name string, seq, sec int) sandbox.BridgeCall {
	return sandbox.BridgeCall{Name: name, Seq: seq, StartedAt: base.Add(time.Duration(sec) * time.Second)}
}

// recordCall appends a to turnID's log as its session's ledger does, answering
// whether the store recorded it, and fails t on an error: an append of the
// log's own job, or one the log drops.
func recordCall(t *testing.T, s sandbox.PendingStore, turnID string, a sandbox.BridgeAppend) bool {
	t.Helper()
	recorded, err := s.AppendBridgeCall(t.Context(), turnID, a)
	if err != nil {
		t.Fatalf("AppendBridgeCall(%s): %v", a.Call.Name, err)
	}
	return recorded
}

// recordLate appends a, an append of a job a relaunch replaced while carrying
// its round on, to turnID's log as its session's ledger does, answering whether
// the store recorded it — and fails t unless a recorded one is answered beside
// [sandbox.ErrJobReplaced], which says its spend can reach no record, so the
// ledger stops asking for it.
func recordLate(t *testing.T, s sandbox.PendingStore, turnID string, a sandbox.BridgeAppend) bool {
	t.Helper()
	recorded, err := s.AppendBridgeCall(t.Context(), turnID, a)
	switch {
	case recorded && !errors.Is(err, sandbox.ErrJobReplaced):
		t.Fatalf("a late append of %s was answered recorded beside %v, want sandbox.ErrJobReplaced",
			a.Call.Name, err)
	case !recorded && err != nil:
		t.Fatalf("AppendBridgeCall(%s): %v", a.Call.Name, err)
	}
	return recorded
}

// wantCall is a call as a case expects the log to list it.
type wantCall struct {
	name          string
	seq, appended int
	late          bool
}

// listsExactly fails t unless calls are want, in order, each at its place, its
// number and its mark.
func listsExactly(t *testing.T, calls []sandbox.BridgeCall, want []wantCall) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("the log lists %d calls, want %d: %+v", len(calls), len(want), calls)
	}
	for i, w := range want {
		c := calls[i]
		if c.Name != w.name || c.Seq != w.seq || c.Appended != w.appended || c.Late != w.late {
			t.Errorf("call %d is %s at place %d, taken %d'th, late %v; want %s at place %d, taken %d'th, "+
				"late %v", i+1, c.Name, c.Seq, c.Appended, c.Late, w.name, w.seq, w.appended, w.late)
		}
	}
}

// A LATE CALL OF THE ROUND IS LISTED, MARKED, AND PAID BY NO JOB. A call still
// running when a relaunch carried its round on — a loss's relaunch, an answered
// question's, a replaced sign-in's, a refused session's — finishes after the
// log was opened to the next job. It is a call of the round, and its effect
// happened: the log lists it as it lists every call, at the place the bridge
// gave it and numbered past every call the round's log took before it, and
// MARKS it, so no save point of the round holds it. Its spend is nobody's: the
// next job's record keeps its own total, which that job's resume pays, and the
// job it was made in is over. The next job's own call beside it is listed
// unmarked, its spend written, as every job's own call is. And an append that
// names no round — what served its job knew none — is dropped once its job is
// replaced, as every late append once was.
//
// Mutation: take a late append by the job alone, as before, and it is not
// listed; list it without its mark, and this goes red.
func testALateCallOfTheRoundIsListedMarkedAndPaidByNoJob(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-late-listed"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("read_page", 1, 1), Spent: bridged(1)}) {
		t.Fatal("the first job's own call was not recorded")
	}
	next := carryRoundOn(t, s, turn)

	// THE FIRST JOB'S CALL STILL IN FLIGHT AS THE ROUND WAS CARRIED ON, its
	// session's running total past the next job's.
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 2, 2), Spent: bridged(5)}) {
		t.Error("a late call of the round was not recorded")
	}
	if recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first,
		Call: madeAt("read_file", 3, 3), Spent: bridged(6)}) {
		t.Error("a late call naming no round was recorded")
	}
	// AND THE NEXT JOB'S OWN, numbered after what the log held at its opening.
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: next, Round: round,
		Call: madeAt("post_message", 2, 4), Spent: bridged(1)}) {
		t.Fatal("the next job's own call was not recorded")
	}

	got := mustLogged(t, s, turn)
	listsExactly(t, got.Log.Calls, []wantCall{
		{"read_page", 1, 1, false},
		{"delegate", 2, 2, true},
		{"post_message", 2, 3, false},
	})
	if got.Launch.Bridged != bridged(1) {
		t.Errorf("the next job's record holds a bridged spend of %+v, want its own %+v: a late call's "+
			"spend is no job's", got.Launch.Bridged, bridged(1))
	}
}

// A LATE CALL ASKED AGAIN IS LISTED ONCE. A session that cannot tell an append
// that did not land from one whose answer was lost after it landed asks it
// again, and a late append is asked again as any is: the log knows a call it
// holds by its place, its instant, its name and its identity, and lists it
// once — answered recorded each time, its number unchanged. So is one that
// landed as its job's own just before the relaunch moved the log on, which the
// ask after finds there: listed once, as it was taken, unmarked.
//
// Mutation: list a late append's call without asking whether the log holds
// it, and this goes red.
func testALateCallAskedAgainIsListedOnce(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-late-again"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	landed := sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("read_page", 1, 1)}
	if !recordCall(t, s, turn, landed) {
		t.Fatal("the first job's own call was not recorded")
	}
	carryRoundOn(t, s, turn)

	late := sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("delegate", 2, 2)}
	for ask := 1; ask <= 2; ask++ {
		if !recordLate(t, s, turn, late) {
			t.Errorf("ask %d of a late call was not answered recorded", ask)
		}
	}
	if !recordLate(t, s, turn, landed) {
		t.Error("an append that landed before the relaunch, asked again, was not answered recorded")
	}

	got := mustLogged(t, s, turn)
	listsExactly(t, got.Log.Calls, []wantCall{
		{"read_page", 1, 1, false},
		{"delegate", 2, 2, true},
	})
	if n := got.Log.Appends(); n != 2 {
		t.Errorf("the log counts %d appends, want the 2 calls it took", n)
	}
}

// A LATE CALL KEEPS ITS MARK THROUGH EVERY WRITE OF ITS ROUND. Every append
// keeps every call the log lists again, and every opening of the log for a
// later job of the same round carries the round's calls on: a mark either of
// them lost would hand the call back to the save point, which holds it once a
// capture's read is past its number. So the mark stands through the next job's
// own calls and a further relaunch that carries the round on, and so does the
// call's number.
//
// Mutation: keep a call again without its mark, and this goes red.
func testALateCallKeepsItsMarkThroughEveryWriteOfItsRound(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-late-kept"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	second := carryRoundOn(t, s, turn)
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1)}) {
		t.Fatal("a late call of the round was not recorded")
	}
	for n := 1; n <= 3; n++ {
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: second, Round: round,
			Call: madeAt("read_file", n, 1+n)}) {
			t.Fatalf("the second job's call %d was not recorded", n)
		}
	}
	third := carryRoundOn(t, s, turn)
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: third, Round: round,
		Call: madeAt("post_message", 4, 5)}) {
		t.Fatal("the third job's call was not recorded")
	}

	got := mustLogged(t, s, turn)
	listsExactly(t, got.Log.Calls, []wantCall{
		{"delegate", 1, 1, true},
		{"read_file", 1, 2, false},
		{"read_file", 2, 3, false},
		{"read_file", 3, 4, false},
		{"post_message", 4, 5, false},
	})
}

// A LATE CALL WRITES NO JOB'S SPEND. An append reads and writes the run's log,
// and the run's record to write its job's spend, where the total moved. A late
// call of the round has no job to pay it — the one the row holds would pay it
// as its own, and its own is over — so the store decides as it takes the call
// that its spend is nobody's, and asks the record for it only ONCE, to hand
// the call to the job working on the round as an item
// ([sandbox.RunNote.LateCall]): never to write its spend. The next job's own
// call beside it still writes its spend.
//
// Mutation: write a late call's spend as a job's own call's is, guarded by the
// row's job alone, and the record is asked for it a second time.
func testALateCallWritesNoJobsSpend(t *testing.T, runs *stagedRuns, s sandbox.PendingStore) {
	const turn = "t-late-no-record"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	next := carryRoundOn(t, s, turn)

	before := runs.rowAsks.Load()
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1), Spent: bridged(3)}) {
		t.Fatal("a late call of the round was not recorded")
	}
	if asked := runs.rowAsks.Load() - before; asked != 1 {
		t.Errorf("a late call asked the run's record %d times, want once, for its item: its spend is no job's",
			asked)
	}
	if got := mustGet(t, s, turn).Launch.Bridged; got != (sandbox.EngineSpend{}) {
		t.Errorf("the next job's record holds %+v after the late call, want no spend", got)
	}
	before = runs.rowAsks.Load()
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: next, Round: round,
		Call: madeAt("read_file", 1, 2), Spent: bridged(1)}) {
		t.Fatal("the next job's own call was not recorded")
	}
	if runs.rowAsks.Load() == before {
		t.Error("the next job's own call asked the record nothing: the case does not see the record")
	}
	if got := mustGet(t, s, turn).Launch.Bridged; got != bridged(1) {
		t.Errorf("the next job's record holds %+v, want its own spend %+v", got, bridged(1))
	}
}

// bigSubmission is the arguments of a successful `submit_work` past every call's
// ordinary share of the log ([sandbox.MaxBridgeArgsBytes]) and within what the
// live tool takes ([sandbox.MaxSubmissionBytes]), told apart by what.
func bigSubmission(t *testing.T, what string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"outcome": "delivered",
		"summary": what + ": " + strings.Repeat("the build passed. ", 20<<10/18)})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(raw); n <= sandbox.MaxBridgeArgsBytes || n > sandbox.MaxSubmissionBytes {
		t.Fatalf("the submission weighs %d bytes, want past %d and within %d", n, sandbox.MaxBridgeArgsBytes,
			sandbox.MaxSubmissionBytes)
	}
	return string(raw)
}

// A LATE SUBMISSION NEVER TAKES THE ROUND'S WHOLE SHARE. The log keeps one
// submission's arguments whole, past every other call's share, because the
// collection replays it as the round's outcome — and refuses one the log cut,
// reading the round as one that submitted nothing. The collection never
// replays a late one: its job was replaced, and the round's submission is the
// one the job carrying the round on makes. So the whole share is never a late
// submission's, though its place — which its own job's session gave it — sorts
// after the carried-on job's own submission, or ties with it, the late one
// taken last: the round's own is kept whole, and the late one held to the
// ordinary share.
//
// Mutation: let a late submission take the whole share, as any submission it
// sorts after, and the round's own is cut.
func testALateSubmissionNeverTakesTheRoundsWholeShare(t *testing.T, s sandbox.PendingStore) {
	for _, tc := range []struct {
		name    string
		lateSeq int
	}{
		{"placed after the round's own", 3},
		{"at the round's own place, taken after it", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := fmt.Sprintf("t-late-share-%d", tc.lateSeq)
			first := mustLaunched(t, s, run(turn))
			round := mustGet(t, s, turn).Round
			next := carryRoundOn(t, s, turn)
			own := madeAt(sandbox.SubmitWorkTool, 2, 2)
			own.Args = bigSubmission(t, "the round's own")
			if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: next, Round: round, Call: own}) {
				t.Fatal("the carried-on job's own submission was not recorded")
			}
			late := madeAt(sandbox.SubmitWorkTool, tc.lateSeq, 3)
			late.Args = bigSubmission(t, "the replaced job's")
			if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: late}) {
				t.Fatal("the late submission was not recorded")
			}
			calls := mustLogged(t, s, turn).Log.Calls
			if len(calls) != 2 {
				t.Fatalf("the log lists %d calls, want the two submissions: %+v", len(calls), calls)
			}
			if got := calls[0]; got.Late || got.ArgsBytes != 0 || got.Args != own.Args {
				t.Errorf("the round's own submission is kept cut to %d of its bytes (late %v): the collection "+
					"replays it, and refuses a cut one", len(got.Args), got.Late)
			}
			if got := calls[1]; !got.Late || got.ArgsBytes == 0 {
				t.Errorf("the late submission is kept whole (late %v, %d bytes said cut), the round's share",
					got.Late, got.ArgsBytes)
			}
		})
	}
}

// OF TWO SUBMISSIONS AT ONE PLACE, THE ONE TAKEN LAST IS KEPT WHOLE. A session
// opened again on its job — a node that took the seat over serves it — numbers
// its calls after the last one the log holds, so a call of the session it
// replaced, still in flight then, can share a place with one of its own. The
// collection reads a round's submissions in the order the bridge admitted
// them, a tie in the order the log took them, and replays the last: so that is
// the one the log keeps whole, and the one taken first is held to the ordinary
// share.
//
// Mutation: keep the whole share on the first submission at the place, and
// the one the collection replays is cut.
func testOfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-share-tie"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	var made []sandbox.BridgeCall
	for i, what := range []string{"taken first", "taken last"} {
		call := madeAt(sandbox.SubmitWorkTool, 1, i+1)
		call.Args = bigSubmission(t, what)
		made = append(made, call)
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: call}) {
			t.Fatalf("the submission %s was not recorded", what)
		}
	}
	calls := mustLogged(t, s, turn).Log.Calls
	if len(calls) != 2 {
		t.Fatalf("the log lists %d calls, want the two submissions: %+v", len(calls), calls)
	}
	if calls[0].ArgsBytes == 0 || calls[1].ArgsBytes != 0 || calls[1].Args != made[1].Args {
		t.Errorf("of two submissions at one place, the first taken is said cut %v and the last %v; want only "+
			"the first cut, the last kept whole for the collection's replay", calls[0].ArgsBytes != 0,
			calls[1].ArgsBytes != 0)
	}
}

// THE CALL A PERSON APPROVED IS NEVER LATE. The store appends it for its own
// round — after its result is recorded, and again in the opening of the job
// that carries the round on where that append never landed — and the
// relaunch's lead tells it, so no mark is its: the mark is the log's to give,
// to a call of a job a relaunch replaced, and never a maker's. One handed in
// marked is listed unmarked, and stays so as the round is carried on.
//
// Mutation: keep the mark a maker handed in on the call a person approved, and
// it is listed late.
func testTheCallAPersonApprovedIsNeverLate(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-approved-not-late"
	launch := heldParked(t, s, turn)
	if _, ok, err := s.RecordAnswer(ctx, turn, launch, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, turn, sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil ||
		!ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if _, first, err := s.MarkHeldCall(ctx, turn, launch, sandbox.Fence{}); err != nil || !first {
		t.Fatalf("MarkHeldCall = %v, %v", first, err)
	}
	made := heldMade("sealed-result")
	made.Call.Late = true
	if ok, err := s.RecordHeldResult(ctx, turn, launch, made, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}
	carryRoundOn(t, s, turn)
	approved := 0
	for _, call := range mustLogged(t, s, turn).Log.Calls {
		if !call.Approved {
			continue
		}
		approved++
		if call.Late {
			t.Errorf("the call a person approved is listed late: %+v", call)
		}
	}
	if approved != 1 {
		t.Fatalf("the round's log lists the approved call %d times, want once", approved)
	}
}

// A CALL NOBODY WAITED FOR KEEPS ITS MARK, AND IS PAID BY ITS JOB. The bridge
// marks a call whose harness had stopped waiting for it by the time it returned
// — given up on, or still running as its opening ended
// ([sandbox.BridgeCall.Abandoned]) — whose effect happened while the session of
// the job that made it holds no outcome of it, so no save point may hold it.
// The mark is the bridge's, and the log keeps it as handed, in the call's ONE
// UPDATE of the entry written ahead of it: on a call of the log's own job, whose
// spend is written to the job's record as any of its calls' is; and on a call
// of the job that lands after a relaunch carried the round on, which the log
// marks late beside it in the same write. A call the bridge did not mark is
// listed unmarked, and no entry is left in flight.
//
// Mutation: drop the mark as the log takes a call, and this goes red.
func testACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-abandoned-kept"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	gaveUp, ended, outlived := madeAt("delegate", 1, 1), madeAt("slack_post", 2, 2), madeAt("delegate", 4, 4)
	gaveUp.Abandoned, ended.Abandoned, outlived.Abandoned = true, true, true
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: gaveUp,
		Spent: bridged(1)}) {
		t.Fatal("the job's call nobody waited for was not recorded")
	}
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("slack_post", 2, 2)})
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: ended,
		Spent: bridged(2)}) {
		t.Fatal("the update of the job's call its opening's end answered was not recorded")
	}
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("read_file", 3, 3),
		Spent: bridged(2)}) {
		t.Fatal("the job's ordinary call was not recorded")
	}
	if got := mustGet(t, s, turn).Launch.Bridged; got != bridged(2) {
		t.Errorf("the job's bridged spend = %+v, want its newest total %+v: a call nobody waited for is "+
			"its job's", got, bridged(2))
	}
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("delegate", 4, 4)})
	carryRoundOn(t, s, turn)
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: outlived}) {
		t.Fatal("the job's call that outlived its opening was not recorded")
	}

	got := mustLogged(t, s, turn)
	listsExactly(t, got.Log.Calls, []wantCall{
		{"delegate", 1, 1, false},
		{"slack_post", 2, 2, false},
		{"read_file", 3, 3, false},
		{"delegate", 4, 4, true},
	})
	for i, want := range []bool{true, true, false, true} {
		if c := got.Log.Calls[i]; c.Abandoned != want {
			t.Errorf("call %d, %s, is listed abandoned %v, want %v, as its bridge handed it", i+1, c.Name,
				c.Abandoned, want)
		}
	}
	if len(got.Log.InFlight) != 0 {
		t.Errorf("the log holds %d entries in flight once every call's update landed: %+v",
			len(got.Log.InFlight), got.Log.InFlight)
	}
}

// WHAT A LATE CALL AND ONE NOBODY WAITED FOR COMMITTED IS THE ROUND'S, listed
// or dropped. Each ran and its effect happened — a message posted, an item
// filed — so the kinds of change it committed to its turn
// ([sandbox.BridgeCall.Committed]) are what the round's collection settles the
// turn's outcome by, as any call's are ([sandbox.BridgeLog.Changes]): a call
// nobody waited for, which is its job's own, and a late one, which the log
// takes after its job was replaced and answers beside [sandbox.ErrJobReplaced]
// on its first try. Read while the log lists both, and again once the log's
// bound has dropped both from its middle, which keeps no field of a dropped
// call: their kinds are in the log's own union of the round's
// ([sandbox.BridgeLog.Committed]), each once.
//
// Mutation: drop a late call's kinds where the log takes it, and they are
// missing while it is listed; fold into the log's union only the kinds of a
// call a save point holds, and both calls' are missing once the bound drops
// them.
func testWhatALateCallOrOneNobodyWaitedForCommittedIsTheRounds(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-late-committed"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	// THE HEAD the log's bound keeps whole ([sandbox.MaxBridgeCalls]), so the
	// calls after it are ones its middle can drop.
	head := sandbox.MaxBridgeCalls / 2
	for seq := 1; seq <= head; seq++ {
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
			Call: madeAt(fmt.Sprintf("read_%03d", seq), seq, seq)}) {
			t.Fatalf("the job's call %d was not recorded", seq)
		}
	}
	gaveUp := madeAt("slack_post", head+1, head+1)
	gaveUp.Abandoned, gaveUp.Committed = true, []string{string(types.CommittedChat)}
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: gaveUp}) {
		t.Fatal("the job's call nobody waited for was not recorded")
	}
	carryRoundOn(t, s, turn)
	late := madeAt("create_work_item", head+2, head+2)
	late.Committed = []string{string(types.CommittedItem), string(types.CommittedChat)}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: late}) {
		t.Fatal("the late call was not recorded")
	}
	// IN THE ORDER FIRST COMMITTED, each once: the chat post the call nobody
	// waited for made, then the item the late call filed beside a post of its
	// own.
	want := []string{string(types.CommittedChat), string(types.CommittedItem)}
	lists := func(log sandbox.BridgeLog, name string) bool {
		return slices.ContainsFunc(log.Calls, func(c sandbox.BridgeCall) bool { return c.Name == name })
	}
	listed := mustLogged(t, s, turn).Log
	if !lists(listed, gaveUp.Name) || !lists(listed, late.Name) {
		t.Fatal("the log does not list both committing calls, so this case proves nothing about a listed one")
	}
	if got := listed.Changes(); !slices.Equal(got, want) {
		t.Errorf("while both are listed, the round committed %q, want %q", strings.Join(got, ", "),
			strings.Join(want, ", "))
	}

	// THEN DROPPED. Every later call of the replaced job is late too, so no
	// ordinary call is left in the middle, and the bound gives up the oldest of
	// the calls no save point holds: the two above first.
	for seq := head + 3; seq <= sandbox.MaxBridgeCalls+2; seq++ {
		if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
			Call: madeAt(fmt.Sprintf("read_%03d", seq), seq, seq)}) {
			t.Fatalf("the late call %d was not recorded", seq)
		}
	}
	dropped := mustLogged(t, s, turn).Log
	if lists(dropped, gaveUp.Name) || lists(dropped, late.Name) {
		t.Fatal("the bound kept a committing call, so this case proves nothing about a dropped one")
	}
	if got := dropped.Changes(); !slices.Equal(got, want) {
		t.Errorf("once the bound dropped both, the round committed %q, want %q", strings.Join(got, ", "),
			strings.Join(want, ", "))
	}
}

// A RECORD OF AN EARLIER ROUND IS LISTED LATE IN THE ROUND AFTER IT. A call
// still in flight as its round's collection read the log lands once a
// send-back's opening moved the log on to a new round, or two: listed late in
// the round the log holds, with NO PLACE (its place was its own round's),
// numbered as that round's next append, the bridge's mark that nobody waited
// for it kept, what its own work called folded into the round's absorbed
// tally, its spend nobody's and answered so. The new job's own call keeps the
// round's order: the log's last place is its.
//
// Mutation: answer an earlier round's record as taken for nothing, fold none
// of what it absorbed, or clear its Abandoned mark, and this goes red.
func testARecordOfAnEarlierRoundIsListedLateInTheRoundAfterIt(t *testing.T, s sandbox.PendingStore) {
	for _, sendBacks := range []int{1, 2} {
		t.Run(fmt.Sprintf("after %d send-backs", sendBacks), func(t *testing.T) {
			turn := fmt.Sprintf("t-earlier-listed-%d", sendBacks)
			first := mustLaunched(t, s, run(turn))
			round := mustGet(t, s, turn).Round
			if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
				Call: madeAt("read_page", 1, 1), Spent: bridged(1)}) {
				t.Fatal("the first job's own call was not recorded")
			}
			var next string
			for range sendBacks {
				next = sendBack(t, s, turn)
			}
			if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: next, Round: mustGet(t, s, turn).Round,
				Call: madeAt("post_message", 1, 3), Spent: bridged(2)}) {
				t.Fatal("the sent-back job's own call was not recorded")
			}
			outlived := madeAt("delegate", 2, 2)
			outlived.Abandoned = true
			if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: outlived,
				Absorbed: []sandbox.ElidedCall{{Name: "read_file", Digest: "d1", Calls: 2}}, Spent: bridged(7)}) {
				t.Fatal("the record of the earlier round was not recorded")
			}

			got := mustLogged(t, s, turn)
			listsExactly(t, got.Log.Calls, []wantCall{
				{"post_message", 1, 1, false},
				{"delegate", 0, 2, true},
			})
			if !got.Log.Calls[1].Abandoned {
				t.Error("the record of the earlier round lost the bridge's mark that nobody waited for it")
			}
			if a := got.Log.Absorbed(); len(a) != 1 || a[0].Name != "read_file" || a[0].Digest != "d1" ||
				a[0].Calls != 2 {
				t.Errorf("the round's absorbed tally is %+v, want the record's read_file d1 x2", a)
			}
			if last := got.Log.LastSeq(); last != 1 {
				t.Errorf("the round's last place is %d, want 1, the sent-back job's own", last)
			}
			if got.Launch.Bridged != bridged(2) {
				t.Errorf("the sent-back job's record holds a bridged spend of %+v, want its own %+v",
					got.Launch.Bridged, bridged(2))
			}
		})
	}
}

// A BEGIN OR WITHDRAWAL OF AN EARLIER ROUND IS NEVER TAKEN. No call of a closed
// round starts: its begin is answered not taken, so the bridge does not run it,
// and a withdrawal, whose entry went with its round's log, takes nothing. The
// log the send-back opened is left as it was.
//
// Mutation: take an earlier round's begin as its record is, and it is listed.
func testABeginOrWithdrawalOfAnEarlierRoundIsNeverTaken(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-earlier-begin"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	sendBack(t, s, turn)
	for _, step := range []sandbox.AppendStep{sandbox.AppendBegin, sandbox.AppendWithdraw} {
		taken, err := s.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Step: step, Launch: first,
			Round: round, Call: madeAt("delegate", 2, 2)})
		if err != nil || taken {
			t.Errorf("the earlier round's %q step = %v, %v; want it not taken, and no error", step, taken, err)
		}
	}
	if got := mustLogged(t, s, turn).Log; len(got.Calls) != 0 || len(got.InFlight) != 0 || got.Appends() != 0 {
		t.Errorf("the sent-back round's log holds calls %+v, entries %+v and %d appends, want none",
			got.Calls, got.InFlight, got.Appends())
	}
}

// A RECORD OF AN EARLIER ROUND ASKED AGAIN IS LISTED ONCE. A session that
// cannot tell a record that did not land from one whose answer was lost asks
// again; with no place to match, the log finds the call by its instant, name
// and identity, lists it once, its number unchanged, and counts what it
// absorbed once.
//
// Mutation: list an earlier round's record without asking whether the log
// lists it already, and it is listed twice.
func testARecordOfAnEarlierRoundAskedAgainIsListedOnce(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-earlier-again"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	sendBack(t, s, turn)
	late := sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("delegate", 4, 4),
		Absorbed: []sandbox.ElidedCall{{Name: "read_file", Digest: "d1", Calls: 1}}}
	for ask := 1; ask <= 2; ask++ {
		if !recordLate(t, s, turn, late) {
			t.Errorf("ask %d of the earlier round's record was not answered recorded", ask)
		}
	}
	got := mustLogged(t, s, turn).Log
	listsExactly(t, got.Calls, []wantCall{{"delegate", 0, 1, true}})
	if n := got.Appends(); n != 1 {
		t.Errorf("the log counts %d appends, want the 1 call it took", n)
	}
	if a := got.Absorbed(); len(a) != 1 || a[0].Calls != 1 {
		t.Errorf("the round's absorbed tally is %+v, want the record's one call, counted once", a)
	}
}

// TWO CALLS OF AN EARLIER ROUND ANSWERED WITHOUT RUNNING ARE BOTH LISTED. A
// call answered without running carries no instant, and an earlier round's
// record has no place to match, so two such calls of one tool with the same
// arguments are told apart by nothing the log keeps. Each is listed, and what
// each absorbed counted: a record of one asked again is listed twice, an
// over-count, where matching them would list one call for two, an under-count.
//
// Mutation: match an earlier round's record with no instant to a listed one,
// and the second call is lost.
func testTwoCallsOfAnEarlierRoundAnsweredWithoutRunningAreBothListed(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-earlier-no-instant"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	sendBack(t, s, turn)
	for seq := 4; seq <= 5; seq++ {
		if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
			Call:     sandbox.BridgeCall{Name: "delegate", Seq: seq, Failed: true},
			Absorbed: []sandbox.ElidedCall{{Name: "read_file", Digest: "d1", Calls: 1}}}) {
			t.Fatalf("the earlier round's call %d was not recorded", seq)
		}
	}
	got := mustLogged(t, s, turn).Log
	listsExactly(t, got.Calls, []wantCall{{"delegate", 0, 1, true}, {"delegate", 0, 2, true}})
	if a := got.Absorbed(); len(a) != 1 || a[0].Calls != 2 {
		t.Errorf("the round's absorbed tally is %+v, want each call's one, counted twice", a)
	}
}

// AN EARLIER ROUND'S CALL MOVES NO PLACE OF THE ROUND. The record lands with
// the place its own round gave it, 40, past the sent-back job's last, 3. The
// round's places are what an answer's cut, the next job's numbering and the
// call a person approved are taken from ([sandbox.BridgeLog.LastSeq]), and
// with no place the earlier round's call moves none of them: an answer cuts
// the round at 3 and its relaunch numbers from 4, and an approved call is 4.
//
// Mutation: keep the record's own place, and each is taken past 40.
func testAnEarlierRoundsCallMovesNoPlaceOfTheRound(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	// sentBack makes turn's run a sent-back round whose job made three calls
	// before the first round's call at place 40 landed, answering that job.
	sentBack := func(t *testing.T, turn string) string {
		t.Helper()
		first := mustLaunched(t, s, run(turn))
		round := mustGet(t, s, turn).Round
		job := sendBack(t, s, turn)
		nextRound := mustGet(t, s, turn).Round
		for seq := 1; seq <= 3; seq++ {
			if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: nextRound,
				Call: madeAt(fmt.Sprintf("read_%d", seq), seq, seq)}) {
				t.Fatalf("the sent-back job's call %d was not recorded", seq)
			}
		}
		if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
			Call: madeAt("delegate", 40, 4)}) {
			t.Fatal("the record of the earlier round was not recorded")
		}
		return job
	}
	t.Run("an answer's relaunch", func(t *testing.T) {
		const turn = "t-earlier-answer"
		job := sentBack(t, turn)
		if got, err := s.RecordAsk(ctx, turn, job, branchAsk()); err != nil || got != sandbox.AskRecorded {
			t.Fatalf("RecordAsk = %q, %v; want it recorded", got, err)
		}
		next := run(turn)
		next.Continues = true
		opened, err := s.BeginLaunch(ctx, next, sandbox.Fence{})
		if err != nil {
			t.Fatalf("the answer's relaunch: %v", err)
		}
		if opened.OpenedAfter != 3 {
			t.Errorf("the answer's relaunch numbers its calls after %d, want 3", opened.OpenedAfter)
		}
		if cut := mustLogged(t, s, turn).Log.AnsweredThrough; cut != 3 {
			t.Errorf("the answer cut the round at %d, want 3, the asking job's last place", cut)
		}
	})
	t.Run("a held call a person approved", func(t *testing.T) {
		const turn = "t-earlier-approved"
		job := sentBack(t, turn)
		if got, err := s.RecordAsk(ctx, turn, job, heldAsk(t, turn, job)); err != nil ||
			got != sandbox.AskRecorded {
			t.Fatalf("RecordAsk of a held call = %v, %v", got, err)
		}
		if err := s.MarkAwaiting(ctx, turn, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("MarkAwaiting: %v", err)
		}
		if _, ok, err := s.RecordAnswer(ctx, turn, job, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("RecordAnswer = %v, %v", ok, err)
		}
		if _, ok, err := s.ClaimForResume(ctx, turn, sandbox.RecordedAnswerTail(job), sandbox.Fence{}); err != nil ||
			!ok {
			t.Fatalf("ClaimForResume = %v, %v", ok, err)
		}
		if _, first, err := s.MarkHeldCall(ctx, turn, job, sandbox.Fence{}); err != nil || !first {
			t.Fatalf("MarkHeldCall = %v, %v", first, err)
		}
		if ok, err := s.RecordHeldResult(ctx, turn, job, heldMade("sealed-result"), sandbox.Fence{}); err != nil ||
			!ok {
			t.Fatalf("RecordHeldResult = %v, %v", ok, err)
		}
		i := slices.IndexFunc(mustLogged(t, s, turn).Log.Calls, func(c sandbox.BridgeCall) bool { return c.Approved })
		if i < 0 {
			t.Fatal("the round's log does not list the approved call")
		}
		if seq := mustLogged(t, s, turn).Log.Calls[i].Seq; seq != 4 {
			t.Errorf("the approved call is at place %d, want 4, after the sent-back job's last", seq)
		}
	})
}

// WHAT AN EARLIER ROUND'S CALL COMMITTED IS THE NEXT ROUND'S. Its effect
// happened in the turn and no earlier collection read it, so the kinds it
// committed join the union the round after settles the turn's outcome by
// ([sandbox.BridgeLog.Changes]).
//
// Mutation: drop the kinds of a record of an earlier round, and the round
// after committed nothing.
func testWhatAnEarlierRoundsCallCommittedIsTheNextRounds(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-earlier-committed"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	sendBack(t, s, turn)
	late := madeAt("create_work_item", 2, 2)
	late.Committed = []string{string(types.CommittedItem)}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: late}) {
		t.Fatal("the record of the earlier round was not recorded")
	}
	if got, want := mustLogged(t, s, turn).Log.Changes(), late.Committed; !slices.Equal(got, want) {
		t.Errorf("the round after committed %q, want %q", strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

// collectedAt records each of calls as job's own in turnID's log, at round, and
// fails t on one not recorded.
func collectedAt(t *testing.T, s sandbox.PendingStore, turnID, job, round string, calls ...sandbox.BridgeCall) {
	t.Helper()
	for _, call := range calls {
		if !recordCall(t, s, turnID, sandbox.BridgeAppend{Launch: job, Round: round, Call: call}) {
			t.Fatalf("the job's own %s was not recorded", call.Name)
		}
	}
}

// A NEW ROUND CARRIES THE CALLS ITS COLLECTION DID NOT READ. A call still in
// flight as the collection read the log can land before the send-back's
// opening moves the log on: as its job's own, in its own round, after the read
// the send-back's briefs were built from. The opening carries every such call
// into the new round, late, with no place, numbered as its appends and the
// bridge's mark that nobody waited for one kept; never a call the read saw,
// nor the call a person approved, which the opening appends to the old round
// and the collection read with it. The old round's calls are all folded by
// name; the new job's own calls are numbered from the start.
//
// Mutation: carry nothing, carry the read's last call too, carry the approved
// call, or clear the carried call's Abandoned mark, and this goes red.
func testANewRoundCarriesTheCallsItsCollectionDidNotRead(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-carry-calls"
	asking := heldParked(t, s, turn)
	round := mustGet(t, s, turn).Round
	collectedAt(t, s, turn, asking, round, madeAt("read_1", 1, 1), madeAt("read_2", 2, 2), madeAt("read_3", 3, 3))
	if _, ok, err := s.RecordAnswer(ctx, turn, asking, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, turn, sandbox.RecordedAnswerTail(asking), sandbox.Fence{}); err != nil ||
		!ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	read := mustLogged(t, s, turn).Log.Read()
	abandoned := madeAt("delegate", 4, 4)
	abandoned.Abandoned = true
	collectedAt(t, s, turn, asking, round, abandoned, madeAt("post_message", 5, 5))
	if _, first, err := s.MarkHeldCall(ctx, turn, asking, sandbox.Fence{}); err != nil || !first {
		t.Fatalf("MarkHeldCall = %v, %v", first, err)
	}
	if ok, err := s.RecordHeldResult(ctx, turn, asking, heldMade("sealed-result"), sandbox.Fence{}); err != nil ||
		!ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}

	sendBackFrom(t, s, turn, &read)
	got := mustLogged(t, s, turn).Log
	listsExactly(t, got.Calls, []wantCall{
		{"delegate", 0, 1, true},
		{"post_message", 0, 2, true},
	})
	if len(got.Calls) == 2 && (!got.Calls[0].Abandoned || got.Calls[1].Abandoned) {
		t.Errorf("the carried calls are marked nobody-waited %v and %v, want the delegate's mark kept alone",
			got.Calls[0].Abandoned, got.Calls[1].Abandoned)
	}
	for _, name := range []string{"read_1", "read_3", "post_message", "tr_close_issue"} {
		if !slices.Contains(got.PriorCalls, name) {
			t.Errorf("the turn's earlier calls %v do not name %s", got.PriorCalls, name)
		}
	}
	if last := got.LastSeq(); last != 0 {
		t.Errorf("the new round's last place is %d, want none: its job numbers its calls from 1", last)
	}
}

// A NEW ROUND CARRIES THE ABSORBED CALLS ITS COLLECTION DID NOT READ. The
// collection seeded the turn's call log with what the log had absorbed at its
// read; the new round's tally starts with only what was absorbed after it, by
// identity, so nothing is seeded twice and nothing is lost. An identity the
// read counted more of than the log now holds under it (the bound folded the
// rest away since) carries none, never a negative count.
//
// Mutation: carry no absorbed calls, carry the whole tally, or carry an
// identity's count below zero, and this goes red.
func testANewRoundCarriesTheAbsorbedCallsItsCollectionDidNotRead(t *testing.T, s sandbox.PendingStore) {
	a := func(digest string, calls int) sandbox.ElidedCall {
		return sandbox.ElidedCall{Name: "read_file", Digest: digest, Calls: calls}
	}
	for _, tc := range []struct {
		name  string
		extra []sandbox.ElidedCall
		want  []sandbox.ElidedCall
	}{
		{"what was absorbed after the read", nil, []sandbox.ElidedCall{a("d1", 1), a("d2", 1)}},
		{"an identity the read counted more of", []sandbox.ElidedCall{a("d1", 5)},
			[]sandbox.ElidedCall{a("d2", 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := "t-carry-absorbed-" + strings.ReplaceAll(tc.name, " ", "-")
			job := mustLaunched(t, s, run(turn))
			round := mustGet(t, s, turn).Round
			if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round,
				Call: madeAt("delegate", 1, 1), Absorbed: []sandbox.ElidedCall{a("d1", 2)}}) {
				t.Fatal("the job's first worker was not recorded")
			}
			read := mustLogged(t, s, turn).Log.Read()
			if tc.extra != nil {
				read.Absorbed = tc.extra
			}
			if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round,
				Call: madeAt("delegate", 2, 2), Absorbed: []sandbox.ElidedCall{a("d1", 1), a("d2", 1)}}) {
				t.Fatal("the job's second worker was not recorded")
			}
			sendBackFrom(t, s, turn, &read)
			got := mustLogged(t, s, turn).Log.Absorbed()
			if !slices.EqualFunc(got, tc.want, func(x, y sandbox.ElidedCall) bool {
				return x.Name == y.Name && x.Digest == y.Digest && x.Calls == y.Calls
			}) {
				t.Errorf("the new round's absorbed tally is %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A READ IS APPLIED ONLY TO THE LOG OF THE JOB IT READ. A relaunch carrying
// the round on whose opening failed leaves the row on its job and the log on
// the job before, in the same round; the store answers that row's log as empty
// under the row's job, and a relaunch after it holds that read of nothing.
// Applied to the log, a read through no call would carry every call of the
// round, which an earlier read told, into a new round as late, or mark each
// late where the round is carried on. An opening applies a read only while the
// log is still open to the job it read: nothing carried, nothing marked, no
// absorbed calls carried.
//
// Mutation: apply a read of the log's round whatever job it read, and this
// goes red.
func testAReadIsAppliedOnlyToTheLogOfTheJobItRead(t *testing.T, runs *stagedRuns, s sandbox.PendingStore) {
	// unopened leaves turn's row on a job carrying the round on whose log was
	// never opened, the log on the job before with its one call, and answers
	// the read the store gives of that row's log.
	unopened := func(t *testing.T, turn string) sandbox.LogRead {
		t.Helper()
		job := mustLaunched(t, s, run(turn))
		round := mustGet(t, s, turn).Round
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("delegate", 1, 1),
			Absorbed: []sandbox.ElidedCall{{Name: "read_file", Digest: "d1", Calls: 1}}}) {
			t.Fatal("the job's own call was not recorded")
		}
		carried := run(turn)
		carried.Continues = true
		down := errors.New("the coordination store is unreachable")
		runs.stage(nil, down)
		if _, err := s.BeginLaunch(t.Context(), carried, sandbox.Fence{}); !errors.Is(err, down) {
			t.Fatalf("a launch whose log could not be opened = %v, want the store's error", err)
		}
		logged := mustLogged(t, s, turn)
		read := logged.Log.Read()
		if logged.LaunchID == job || logged.Round != round || read.Launch != logged.LaunchID || read.Took != 0 {
			t.Fatalf("the premise: the row is on %q of round %q and its log reads %+v, want a job after %q in "+
				"round %q, its log read as empty under that job", logged.LaunchID, logged.Round, read, job, round)
		}
		return read
	}
	t.Run("a new round", func(t *testing.T) {
		const turn = "t-read-unopened-new"
		read := unopened(t, turn)
		sendBackFrom(t, s, turn, &read)
		if got := mustLogged(t, s, turn).Log; len(got.Calls) != 0 || len(got.Absorbed()) != 0 {
			t.Errorf("the new round carried calls %+v and absorbed %+v from a read of another job's log, "+
				"want none", got.Calls, got.Absorbed())
		}
	})
	t.Run("a round carried on", func(t *testing.T) {
		const turn = "t-read-unopened-carried"
		read := unopened(t, turn)
		next := run(turn)
		next.Continues, next.BriefRead = true, &read
		mustLaunched(t, s, next)
		listsExactly(t, mustLogged(t, s, turn).Log.Calls, []wantCall{{"delegate", 1, 1, false}})
	})
}

// A NEW ROUND STARTS WITH THE TALLY OF THE ROUND BEFORE. What the turn's calls
// committed is the turn's: the collection between two rounds folded only what
// its read saw, and a kind a call committed after it would otherwise be in no
// outcome. Kinds are a set, so the whole tally is carried, read or not, and
// with no read at all.
//
// Mutation: start a new round's tally empty, and this goes red.
func testANewRoundStartsWithTheTallyOfTheRoundBefore(t *testing.T, s sandbox.PendingStore) {
	item, comment := madeAt("create_work_item", 1, 1), madeAt("post_comment", 2, 2)
	item.Committed = []string{string(types.CommittedItem)}
	comment.Committed = []string{string(types.CommittedComment)}
	want := []string{string(types.CommittedItem), string(types.CommittedComment)}
	for _, withRead := range []bool{true, false} {
		t.Run(fmt.Sprintf("handed a read %v", withRead), func(t *testing.T) {
			turn := fmt.Sprintf("t-carry-tally-%v", withRead)
			job := mustLaunched(t, s, run(turn))
			round := mustGet(t, s, turn).Round
			collectedAt(t, s, turn, job, round, item)
			var read *sandbox.LogRead
			if withRead {
				r := mustLogged(t, s, turn).Log.Read()
				read = &r
			}
			collectedAt(t, s, turn, job, round, comment)
			sendBackFrom(t, s, turn, read)
			if got := mustLogged(t, s, turn).Log.Changes(); !slices.Equal(got, want) {
				t.Errorf("the new round committed %q, want the turn's %q", strings.Join(got, ", "),
					strings.Join(want, ", "))
			}
		})
	}
}

// A CARRIED CALL ASKED AGAIN IS LISTED ONCE. A record that landed in its own
// round after the collection's read, whose answer was lost, is asked again
// once the send-back's opening carried it: the log finds it by its instant,
// name and identity among the calls with no place, as it finds a record of an
// earlier round, and lists it once.
//
// Mutation: carry a call with its own place, and it is listed twice.
func testACarriedCallAskedAgainIsListedOnce(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-carry-again"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	read := mustLogged(t, s, turn).Log.Read()
	outlived := sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("delegate", 1, 1)}
	collectedAt(t, s, turn, job, round, outlived.Call)
	sendBackFrom(t, s, turn, &read)
	if !recordLate(t, s, turn, outlived) {
		t.Error("the carried call asked again was not answered recorded")
	}
	listsExactly(t, mustLogged(t, s, turn).Log.Calls, []wantCall{{"delegate", 0, 1, true}})
}
