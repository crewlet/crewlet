package sandboxtest

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a LATE-CALL ITEM ([sandbox.RunNote.LateCall]): a call whose
// outcome the session of the job working on its round lacks, filed for that
// job by the store's own writes that decide the call is late or nobody waited
// for it, the append that lists it and the opening of a job's log.
var lateItemCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ALateCallIsHandedToTheJobItsRoundRunsOn", testALateCallIsHandedToTheJobItsRoundRunsOn},
	{"ACallItsOwnJobGaveUpOnIsHandedToThatJob", testACallItsOwnJobGaveUpOnIsHandedToThatJob},
	{"EveryCallTheOpeningWalksIsHandedToTheJobItOpens", testEveryCallTheOpeningWalksIsHandedToTheJobItOpens},
	{"ALateCallAskedAgainAfterItsAnswerWasLostIsHandedOnce",
		testALateCallAskedAgainAfterItsAnswerWasLostIsHandedOnce},
	{"NoItemIsFiledForAJobThatCannotTakeIt", testNoItemIsFiledForAJobThatCannotTakeIt},
	{"AJobsLateItemsStopAtMaxJobItems", testAJobsLateItemsStopAtMaxJobItems},
	{"AnItemKeepsTheOrdinaryShareOfItsCall", testAnItemKeepsTheOrdinaryShareOfItsCall},
	{"AnEarlierRoundsCallIsHandedToTheJobTheNextRoundRunsOn", testAnEarlierRoundsCallIsHandedToTheJobTheNextRoundRunsOn},
	{"EveryCallANewRoundCarriesIsHandedToItsFirstJob", testEveryCallANewRoundCarriesIsHandedToItsFirstJob},
}

// lateItemStagedCases are the item cases whose subject is a write of the
// record that fails or finds the record moved on, which only the records the
// store is built on can stage ([stagedRuns]).
var lateItemStagedCases = []struct {
	name string
	fn   func(*testing.T, *stagedRuns, sandbox.PendingStore)
}{
	{"AnItemTheStoreDidNotTakeLeavesTheAppendAsItWas", testAnItemTheStoreDidNotTakeLeavesTheAppendAsItWas},
	{"NoItemIsFiledForAJobTheRowNoLongerNames", testNoItemIsFiledForAJobTheRowNoLongerNames},
}

// lateItems is every late-call item on turnID's record, in the record's order.
func lateItems(t *testing.T, s sandbox.PendingStore, turnID string) []sandbox.RunNote {
	t.Helper()
	var out []sandbox.RunNote
	for _, n := range mustGet(t, s, turnID).Steer.Notes {
		if n.LateCall != nil {
			out = append(out, n)
		}
	}
	return out
}

// itemCalls is each of items as its job and the call it hands it:
// "job/name#appended".
func itemCalls(items []sandbox.RunNote) []string {
	out := []string{}
	for _, n := range items {
		out = append(out, fmt.Sprintf("%s/%s#%d", n.For, n.LateCall.Call.Name, n.LateCall.Call.Appended))
	}
	return out
}

// A LATE CALL IS HANDED TO THE JOB ITS ROUND RUNS ON. A call the replaced job
// made, still running as a relaunch carried the round on, lands after the log
// was opened to the next job: the append that lists it marked late files one
// item for the job the log is open to, the round's working job, never the
// job that made it, whose session is gone. The item is unsealed, waiting and
// served to nobody, holds the call as the log listed it (its number, its mark)
// with the round that numbered it, and is the job's alone.
//
// Mutation: file the item for the append's job (item-for-the-caller), or leave
// the append filing none, and this goes red.
func testALateCallIsHandedToTheJobItsRoundRunsOn(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-late"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	next := carryRoundOn(t, s, turn)
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1)}) {
		t.Fatal("the late call was not recorded")
	}
	items := lateItems(t, s, turn)
	if got, want := itemCalls(items), []string{next + "/delegate#1"}; !slices.Equal(got, want) {
		t.Fatalf("the record holds items %v, want %v: the late call, for the job its round runs on", got, want)
	}
	item := items[0]
	if len(item.Sealed) != 0 || !item.Waiting() || item.Served != "" || item.LateCall.Round != round ||
		!item.LateCall.Call.Late || item.IsFor(first) || !item.IsFor(next) {
		t.Errorf("the item is %+v, want it unsealed, waiting, served to nobody, of round %s, its call marked "+
			"late, and the working job's alone", item, round)
	}
}

// A CALL ITS OWN JOB GAVE UP ON IS HANDED TO THAT JOB. A call the job's harness
// stopped waiting for ([sandbox.BridgeCall.Abandoned]) is the job's own, paid
// as its own, and its session holds no outcome of it: the append that lists it
// files an item for that same job, once its spend is written. A call it waited
// for files none.
//
// Mutation: file items for late calls alone (item-on-late-only), and the call
// given up on is handed to nobody.
func testACallItsOwnJobGaveUpOnIsHandedToThatJob(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-own"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	gaveUp := madeAt("slack_post", 1, 1)
	gaveUp.Abandoned, gaveUp.Failed, gaveUp.Output = true, true, "timed out"
	for _, call := range []sandbox.BridgeCall{gaveUp, madeAt("read_page", 2, 2)} {
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: call,
			Spent: bridged(call.Seq)}) {
			t.Fatalf("the job's call %s was not recorded", call.Name)
		}
	}
	if got, want := itemCalls(lateItems(t, s, turn)), []string{job + "/slack_post#1"}; !slices.Equal(got, want) {
		t.Errorf("the record holds items %v, want %v: the call its harness gave up on, for its own job", got, want)
	}
	if got := mustGet(t, s, turn).Launch.Bridged; got != bridged(2) {
		t.Errorf("the job's spend = %+v, want its newest %+v: the item never moved it", got, bridged(2))
	}
}

// EVERY CALL THE OPENING WALKS IS HANDED TO THE JOB IT OPENS. A relaunch that
// carries the round on opens the log past the read its briefs were built from
// ([sandbox.PendingRun.BriefRead]): every call the log took after that read,
// one already late among them, is one no brief told, and the opening marks
// each. Each is handed to the job the opening opens, after the launch removed
// the items of the job before, which are that job's alone.
//
// Mutation: answer no walked calls from the opening (opening-files-nothing),
// or walk the calls not yet late alone (opening-skips-late), and this goes red.
func testEveryCallTheOpeningWalksIsHandedToTheJobItOpens(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-opening"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	second := carryRoundOn(t, s, turn)
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1)}) {
		t.Fatal("the first job's late call was not recorded")
	}
	for i, name := range []string{"read_page", "post_message"} {
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: second, Round: round,
			Call: madeAt(name, i+1, i+2)}) {
			t.Fatalf("the second job's call %s was not recorded", name)
		}
	}
	if got := itemCalls(lateItems(t, s, turn)); !slices.Equal(got, []string{second + "/delegate#1"}) {
		t.Fatalf("the premise: the record holds items %v, want the late call for the second job", got)
	}
	// THE BRIEFS' READ BEFORE ANY OF THE THREE: the opening walks them all.
	carried := run(turn)
	carried.Continues, carried.BriefRead = true, &sandbox.LogRead{Round: round}
	third := mustLaunched(t, s, carried)
	want := []string{third + "/delegate#1", third + "/read_page#2", third + "/post_message#3"}
	got := lateItems(t, s, turn)
	if !slices.Equal(itemCalls(got), want) {
		t.Fatalf("the record holds items %v, want %v: every call the opening walked, for the job it opens, "+
			"none of the job before", itemCalls(got), want)
	}
	for _, n := range got {
		if !n.LateCall.Call.Late {
			t.Errorf("item %s hands its call unmarked; the opening marked every call it walked late", n.ID)
		}
	}
}

// A LATE CALL ASKED AGAIN AFTER ITS ANSWER WAS LOST IS HANDED ONCE. A session
// that cannot tell an append that did not land from one whose answer was lost
// asks it again; the log lists the call once, and the item is one an id the
// job and the call's number make: every ask, on any node, names the same item.
//
// Mutation: name an item by anything an ask makes afresh (an attempt, a clock),
// and the call is handed twice.
func testALateCallAskedAgainAfterItsAnswerWasLostIsHandedOnce(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-again"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	next := carryRoundOn(t, s, turn)
	late := sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt("delegate", 1, 1)}
	for ask := 1; ask <= 3; ask++ {
		if !recordLate(t, s, turn, late) {
			t.Fatalf("ask %d of the late call was not answered recorded", ask)
		}
	}
	if got := itemCalls(lateItems(t, s, turn)); !slices.Equal(got, []string{next + "/delegate#1"}) {
		t.Errorf("the record holds items %v, want the late call handed once", got)
	}
}

// NO ITEM IS FILED FOR A JOB THAT CANNOT TAKE IT. A job takes its notes while
// it is launching or running ([sandbox.LateCallStatuses]) and its run's ending
// is undecided: a job parked on a question, or whose run's ending is decided,
// is handed nothing more, its harness reading nothing until a later launch,
// whose continuation tells the call. The call is listed late all the same.
//
// Mutation: file in a status the harness takes no notes in
// (item-files-while-awaiting), or past a decided ending (item-files-past-the-ending),
// and an item lands.
func testNoItemIsFiledForAJobThatCannotTakeIt(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	for _, tc := range []struct {
		name, turn string
		after      func(t *testing.T, turn string)
	}{
		{"parked on a question", "t-item-awaiting", func(t *testing.T, turn string) {
			if err := s.MarkAwaiting(ctx, turn, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("MarkAwaiting: %v", err)
			}
		}},
		{"its ending decided", "t-item-ended", func(t *testing.T, turn string) {
			if _, ok, err := s.DecideEnding(ctx, turn,
				sandbox.Decision{License: everyJob(sandbox.Fence{}, sandbox.Active)}); err != nil || !ok {
				t.Fatalf("DecideEnding = %v, %v", ok, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := tc.turn
			first := mustLaunched(t, s, run(turn))
			round := mustGet(t, s, turn).Round
			carryRoundOn(t, s, turn)
			tc.after(t, turn)
			if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
				Call: madeAt("delegate", 1, 1)}) {
				t.Fatal("the late call was not recorded")
			}
			if got := itemCalls(lateItems(t, s, turn)); len(got) != 0 {
				t.Errorf("a job %s was handed %v, want nothing", tc.name, got)
			}
			if calls := mustLogged(t, s, turn).Log.Calls; len(calls) != 1 || !calls[0].Late {
				t.Errorf("the log lists %+v, want the call marked late all the same", calls)
			}
		})
	}
}

// A JOB'S LATE ITEMS STOP AT [sandbox.MaxJobItems]. Past it a call is listed,
// marked late, and handed nothing: every continuation of the round tells it.
//
// Mutation: hold the job to one more item than the bound (item-bound-off-by-one),
// and an eleventh lands.
func testAJobsLateItemsStopAtMaxJobItems(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-bound-late"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	carryRoundOn(t, s, turn)
	for seq := 1; seq <= sandbox.MaxJobItems+1; seq++ {
		if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
			Call: madeAt(fmt.Sprintf("delegate_%02d", seq), seq, seq)}) {
			t.Fatalf("late call %d was not recorded", seq)
		}
	}
	if got := lateItems(t, s, turn); len(got) != sandbox.MaxJobItems ||
		got[len(got)-1].LateCall.Call.Appended != sandbox.MaxJobItems {
		t.Errorf("the record holds %d items, the last of call %v; want the first %d", len(got), itemCalls(got),
			sandbox.MaxJobItems)
	}
	calls := mustLogged(t, s, turn).Log.Calls
	if last := calls[len(calls)-1]; last.Appended != sandbox.MaxJobItems+1 || !last.Late {
		t.Errorf("the call past the bound is listed %+v, want it marked late, for every continuation to tell", last)
	}
}

// AN ITEM KEEPS THE ORDINARY SHARE OF ITS CALL. The log keeps the job's newest
// successful submission whole, eight times an ordinary call's arguments
// ([sandbox.MaxSubmissionArgsBytes]), and one its harness gave up on is that
// submission: the item that hands it to the job copies it at the ordinary
// share, its cut said ([sandbox.BridgeCall.ArgsBytes]), with no output, since
// it succeeded.
//
// Mutation: copy the call as the log listed it (item-copies-the-listed-call),
// and the item holds the whole submission.
func testAnItemKeepsTheOrdinaryShareOfItsCall(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-share"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	submission := madeAt(sandbox.SubmitWorkTool, 1, 1)
	submission.Args, submission.Output, submission.Abandoned = bigSubmission(t, "given up on"), "accepted", true
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: submission}) {
		t.Fatal("the submission was not recorded")
	}
	if listed := mustLogged(t, s, turn).Log.Calls[0]; listed.ArgsBytes != 0 {
		t.Fatalf("the premise: the log keeps the submission cut (%d bytes said), want it whole", listed.ArgsBytes)
	}
	items := lateItems(t, s, turn)
	if len(items) != 1 {
		t.Fatalf("the record holds items %v, want the submission's", itemCalls(items))
	}
	call := items[0].LateCall.Call
	if call.ArgsBytes == 0 || len(call.Args) > sandbox.MaxBridgeArgsBytes || call.Output != "" {
		t.Errorf("the item keeps %d bytes of arguments (%d said whole) and output %q; want them within %d, "+
			"their cut said, and no output of a success", len(call.Args), call.ArgsBytes, call.Output,
			sandbox.MaxBridgeArgsBytes)
	}
}

// AN ITEM THE STORE DID NOT TAKE LEAVES THE APPEND AS IT WAS. The item is filed
// in a write of the record after the log's own, and a filing the store refuses
// fails nothing: the append is answered as it would be, the call listed and
// marked, and the record holds no item, the call left to every continuation.
//
// Mutation: answer the append the filing's error (item-fails-the-append).
func testAnItemTheStoreDidNotTakeLeavesTheAppendAsItWas(t *testing.T, runs *stagedRuns, s sandbox.PendingStore) {
	const turn = "t-item-refused"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	carryRoundOn(t, s, turn)
	runs.failNext(nil, errors.New("the coordination store did not answer"))
	recorded, err := s.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1)})
	if !recorded || !errors.Is(err, sandbox.ErrJobReplaced) {
		t.Fatalf("the late append whose item failed = %v, %v; want it recorded beside ErrJobReplaced, as ever",
			recorded, err)
	}
	if got := itemCalls(lateItems(t, s, turn)); len(got) != 0 {
		t.Errorf("the record holds items %v, want none: the filing failed", got)
	}
	if calls := mustLogged(t, s, turn).Log.Calls; len(calls) != 1 || !calls[0].Late {
		t.Errorf("the log lists %+v, want the call marked late", calls)
	}
}

// NO ITEM IS FILED FOR A JOB THE ROW NO LONGER NAMES. A relaunch writes the row
// first and opens the log to its job second; where the opening failed, the log
// is still on the job the row replaced, and a late call of the round is taken
// for that job. The row names another job, which the filing reads in its own
// write: it files nothing, and the relaunch that opens the log after it tells
// the call.
//
// Mutation: file for the log's job whatever the row names (item-ignores-the-row),
// and the replaced job is handed an item no reader of it will take.
func testNoItemIsFiledForAJobTheRowNoLongerNames(t *testing.T, runs *stagedRuns, s sandbox.PendingStore) {
	const turn = "t-item-moved"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	second := carryRoundOn(t, s, turn)
	runs.stage(nil, errors.New("the opening did not land"))
	carried := run(turn)
	carried.Continues = true
	if _, err := s.BeginLaunch(t.Context(), carried, sandbox.Fence{}); err == nil {
		t.Fatal("the premise: the relaunch's opening was staged to fail, and the launch went ahead")
	}
	if row := mustGet(t, s, turn); row.LaunchID == second {
		t.Fatal("the premise: the failed relaunch left the row on the job before it")
	}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 1, 1)}) {
		t.Fatal("the late call was not recorded")
	}
	if got := itemCalls(lateItems(t, s, turn)); len(got) != 0 {
		t.Errorf("the record holds items %v, want none: the row names no job the log is open to", got)
	}
}

// AN EARLIER ROUND'S CALL IS HANDED TO THE JOB THE NEXT ROUND RUNS ON. The
// append that lists a record of an earlier round late, in the round a
// send-back opened, files one item for that round's job, never the job that
// made it: unsealed, of the round that listed it, the call marked late with no
// place.
//
// Mutation: file items for a late call of the log's own round alone, and the
// earlier round's call is handed to nobody.
func testAnEarlierRoundsCallIsHandedToTheJobTheNextRoundRunsOn(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-earlier"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	next := sendBack(t, s, turn)
	nextRound := mustGet(t, s, turn).Round
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("delegate", 3, 3)}) {
		t.Fatal("the record of the earlier round was not recorded")
	}
	items := lateItems(t, s, turn)
	if got, want := itemCalls(items), []string{next + "/delegate#1"}; !slices.Equal(got, want) {
		t.Fatalf("the record holds items %v, want %v: the call, for the job the next round runs on", got, want)
	}
	item := items[0]
	if len(item.Sealed) != 0 || !item.Waiting() || item.LateCall.Round != nextRound || !item.LateCall.Call.Late ||
		item.LateCall.Call.Seq != 0 || item.IsFor(first) || !item.IsFor(next) {
		t.Errorf("the item is %+v, want it unsealed, waiting, of round %s, its call late with no place, and the "+
			"next round's job's alone", item, nextRound)
	}
}

// EVERY CALL A NEW ROUND CARRIES IS HANDED TO ITS FIRST JOB. The opening of a
// send-back's round carries the calls the old round's log took after its
// collection's read, which no brief told and no job of the new round made: it
// files one item for each, for the job it opens, of the new round, the call as
// the new log lists it.
//
// Mutation: answer no walked calls from a new round's opening, and the carried
// calls are handed to nobody.
func testEveryCallANewRoundCarriesIsHandedToItsFirstJob(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-item-carried"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	collectedAt(t, s, turn, job, round, madeAt("read_page", 1, 1))
	read := mustLogged(t, s, turn).Log.Read()
	collectedAt(t, s, turn, job, round, madeAt("delegate", 2, 2), madeAt("post_message", 3, 3))
	next := sendBackFrom(t, s, turn, &read)
	items := lateItems(t, s, turn)
	if got, want := itemCalls(items), []string{next + "/delegate#1", next + "/post_message#2"}; !slices.Equal(got,
		want) {
		t.Fatalf("the record holds items %v, want %v: each carried call, for the new round's job", got, want)
	}
	if nextRound := mustGet(t, s, turn).Round; items[0].LateCall.Round != nextRound || !items[0].LateCall.Call.Late {
		t.Errorf("the item is %+v, want it of round %s, its call late", items[0], nextRound)
	}
}
