package sandboxtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a call that OUTLIVES ITS JOB: one the bridge answers after its
// job's harness died, gave up on it or was replaced, by which time a relaunch
// may have opened the run's log to the next job. Where that relaunch carried
// the round on, the call is a late call of the round ([sandbox.BridgeCall.Late]);
// where it started a new round, the call is dropped
// (testABridgedCallOutlivingItsRoundIsNotTheNextRounds).
var lateCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ALateCallOfTheRoundIsListedMarkedAndPaidByNoJob", testALateCallOfTheRoundIsListedMarkedAndPaidByNoJob},
	{"ALateCallCarriesWhatItsSessionOwed", testALateCallCarriesWhatItsSessionOwed},
	{"ALateCallAskedAgainIsListedOnce", testALateCallAskedAgainIsListedOnce},
	{"ALateCallKeepsItsMarkThroughEveryWriteOfItsRound", testALateCallKeepsItsMarkThroughEveryWriteOfItsRound},
	{"ALateSubmissionNeverTakesTheRoundsWholeShare", testALateSubmissionNeverTakesTheRoundsWholeShare},
	{"OfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole", testOfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole},
	{"TheCallAPersonApprovedIsNeverLate", testTheCallAPersonApprovedIsNeverLate},
	{"ACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob", testACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob},
}

// lateStagedCases are the late-call cases whose subject is what the store asks
// of the run's record, which only the records it is built on can show
// ([stagedRuns]).
var lateStagedCases = []struct {
	name string
	fn   func(*testing.T, *stagedRuns, sandbox.PendingStore)
}{
	{"ALateCallTouchesNoRecord", testALateCallTouchesNoRecord},
}

// carryRoundOn relaunches turnID's run carrying its round on — as a loss's
// relaunch, an answered question's and a replaced sign-in's do — and answers
// the new job, after checking it is of the round the row held before.
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
// question's, a replaced sign-in's — finishes after the log was opened to the
// next job. It is a call of the round, and its effect happened: the log lists
// it as it lists every call, at the place the bridge gave it and numbered past
// every call the round's log took before it, and MARKS it, so no save point of
// the round holds it. Its spend is nobody's: the next job's record keeps its
// own total, which that job's resume pays, and the job it was made in is over.
// The next job's own call beside it is listed unmarked, its spend written, as
// every job's own call is. And an append that names no round — what served
// its job knew none — is dropped once its job is replaced, as every late
// append once was.
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

// A LATE CALL CARRIES WHAT ITS SESSION OWED. A session whose earlier append did
// not land owes it whole, and its next append carries it. Where that append is
// a late one — its job replaced meanwhile by a relaunch that carried the round
// on — or the carry its session makes as it ends, the owed calls are calls of
// the round as much as the call carrying them: each listed, in the order it was
// made and before the call that carries it, marked late, with what its own
// work absorbed counted once.
//
// Mutation: drop a late append's owed calls, or list them unmarked, and this
// goes red.
func testALateCallCarriesWhatItsSessionOwed(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-late-owed"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	carryRoundOn(t, s, turn)

	worker := []sandbox.ElidedCall{{Name: "worker_search", Digest: "d1", Calls: 1}}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Owed: []sandbox.OwedAppend{{Call: madeAt("jira_create", 1, 1), Absorbed: worker}},
		Call: madeAt("slack_post", 2, 2)}) {
		t.Fatal("a late call carrying what its session owed was not recorded")
	}
	// AND THE CARRY ITS SESSION MAKES AS IT ENDS, which names no call of its
	// own.
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Owed: []sandbox.OwedAppend{{Call: madeAt("read_file", 3, 3)}}}) {
		t.Fatal("the late carry of the session's end was not recorded")
	}

	got := mustLogged(t, s, turn)
	listsExactly(t, got.Log.Calls, []wantCall{
		{"jira_create", 1, 1, true},
		{"slack_post", 2, 2, true},
		{"read_file", 3, 3, true},
	})
	absorbed := got.Log.Absorbed()
	if len(absorbed) != 1 || absorbed[0].Name != "worker_search" || absorbed[0].Calls != 1 {
		t.Errorf("the owed call's own work is counted %+v, want worker_search once", absorbed)
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

// A LATE CALL TOUCHES NO RECORD. An append reads and writes the run's log alone,
// and the run's record only to write its job's spend, where the total moved. A
// late call of the round has no job to pay it — the one the row holds would pay
// it as its own, and its own is over — so the store decides as it takes the
// call that its spend is nobody's, and asks the record nothing for it, its
// calls in the log all the same. The next job's own call beside it still
// writes its spend.
//
// Mutation: write a late call's spend as a job's own call's is, guarded by the
// row's job alone, and the record is asked for it.
func testALateCallTouchesNoRecord(t *testing.T, runs *stagedRuns, s sandbox.PendingStore) {
	const turn = "t-late-no-record"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	next := carryRoundOn(t, s, turn)

	before := runs.rowAsks.Load()
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1), Spent: bridged(3)}) {
		t.Fatal("a late call of the round was not recorded")
	}
	if asked := runs.rowAsks.Load() - before; asked != 0 {
		t.Errorf("a late call asked the run's record %d times, want none: its spend is no job's", asked)
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
// The mark is the bridge's, and the log keeps it as handed: on a call of the
// log's own job, whose spend is written to the job's record as any of its
// calls' is; on one its session owed and a later append carried; and on a call
// of the job that lands after a relaunch carried the round on, which the log
// marks late beside it. A call the bridge did not mark is listed unmarked.
//
// Mutation: drop the mark as the log takes a call, and this goes red.
func testACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-abandoned-kept"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	gaveUp, owed, outlived := madeAt("delegate", 1, 1), madeAt("slack_post", 2, 2), madeAt("delegate", 4, 4)
	gaveUp.Abandoned, owed.Abandoned, outlived.Abandoned = true, true, true
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: gaveUp,
		Spent: bridged(1)}) {
		t.Fatal("the job's call nobody waited for was not recorded")
	}
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Owed: []sandbox.OwedAppend{{Call: sandbox.CarriedCall(owed)}}, Call: madeAt("read_file", 3, 3),
		Spent: bridged(2)}) {
		t.Fatal("the job's call carrying what its session owed was not recorded")
	}
	if got := mustGet(t, s, turn).Launch.Bridged; got != bridged(2) {
		t.Errorf("the job's bridged spend = %+v, want its newest total %+v: a call nobody waited for is "+
			"its job's", got, bridged(2))
	}
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
}
