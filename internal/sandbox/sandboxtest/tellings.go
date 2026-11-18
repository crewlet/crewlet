package sandboxtest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a TELLING ([sandbox.RunSteer.Tellings]): a late call a job's own
// session is known to hold, recorded by the write that confirmed the item that
// handed it over, at that write's own place among the reads.
var tellingCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AConfirmationOfAnItemRecordsItsTellingAtItsOwnPlace", testAConfirmationOfAnItemRecordsItsTellingAtItsOwnPlace},
	{"AHooksTakeRecordsNoTelling", testAHooksTakeRecordsNoTelling},
	{"AHeldVerdictRecordsATellingAtItsOwnPlaceAndARestartedOneNone",
		testAHeldVerdictRecordsATellingAtItsOwnPlaceAndARestartedOneNone},
	{"ATellingOutlivesItsItem", testATellingOutlivesItsItem},
	{"ANewRoundClearsTheTellings", testANewRoundClearsTheTellings},
	{"TellingsAreOnePerCallAndBounded", testTellingsAreOnePerCallAndBounded},
}

// confirmTake is the take an OpenCode job's plugin makes once OpenCode reports
// the notes ids delivered into the job's session ([sandbox.NoteTake.Confirms]).
func confirmTake(launch string, ids ...string) sandbox.NoteTake {
	take := bridgeTake(launch, ids...)
	take.Read.Boundary, take.Confirms = "step", true
	return take
}

// mustTake takes ids by take and fails the case unless it took exactly them.
func mustTake(t *testing.T, s sandbox.PendingStore, turnID string, take sandbox.NoteTake) {
	t.Helper()
	taken, err := s.TakeNotes(t.Context(), turnID, take)
	if err != nil || !slices.Equal(idsOf(taken), take.IDs) {
		t.Fatalf("take of %v = %v, %v, want every one taken", take.IDs, idsOf(taken), err)
	}
}

// tellingsOf is turnID's tellings, each as "round#appended by job at place".
func tellingsOf(t *testing.T, s sandbox.PendingStore, turnID string) []string {
	t.Helper()
	out := []string{}
	for _, c := range mustGet(t, s, turnID).Steer.Tellings {
		out = append(out, telling(c.Round, c.Appended, c.Launch, c.Place))
	}
	return out
}

// telling is one telling as [tellingsOf] writes it.
func telling(round string, appended int, job string, place int) string {
	return fmt.Sprintf("%s#%d by %s at %d", round, appended, job, place)
}

// lateRound is turnID's run carried on to a second job while the first still
// works: the first job, the round and the second job.
func lateRound(t *testing.T, s sandbox.PendingStore, turnID string) (first, round, next string) {
	t.Helper()
	first = mustLaunched(t, s, run(turnID))
	round = mustGet(t, s, turnID).Round
	return first, round, carryRoundOn(t, s, turnID)
}

// mustLate records the replaced job first's call name, the seq'th, late on
// turnID's log, and answers the item it filed for the job working now.
func mustLate(t *testing.T, s sandbox.PendingStore, turnID, first, round, name string, seq int) sandbox.RunNote {
	t.Helper()
	if !recordLate(t, s, turnID, sandbox.BridgeAppend{Launch: first, Round: round, Call: madeAt(name, seq, seq)}) {
		t.Fatalf("the late call %s was not recorded", name)
	}
	items := lateItems(t, s, turnID)
	if len(items) == 0 || items[len(items)-1].LateCall.Call.Name != name {
		t.Fatalf("the premise: the record holds items %v, want the late call %s last", itemCalls(items), name)
	}
	return items[len(items)-1]
}

// A CONFIRMATION OF AN ITEM RECORDS ITS TELLING AT ITS OWN PLACE. An OpenCode
// job's plugin confirms a late-call item once OpenCode reports it delivered into
// the job's session: the write that reads it records a telling of its call (the
// round, its place in the log, the job) at the write's own new place among the
// reads, and the record's count moves there with the item's read. No hook's
// step read it ([sandbox.CallTelling.Hook]).
//
// Mutation: record no telling at a confirmation (telling-never-confirmed), or
// mark it a hook's (telling-confirmed-as-hook), and a check goes red.
func testAConfirmationOfAnItemRecordsItsTellingAtItsOwnPlace(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-telling-confirmed"
	first, round, next := lateRound(t, s, turn)
	item := mustLate(t, s, turn, first, round, "delegate", 1)
	reads := mustGet(t, s, turn).Steer.Reads
	mustTake(t, s, turn, confirmTake(next, item.ID))
	if got, want := tellingsOf(t, s, turn), []string{telling(round, 1, next, reads+1)}; !slices.Equal(got, want) {
		t.Errorf("the record holds tellings %v, want %v: the confirmed call, at the confirmation's own place", got,
			want)
	}
	if told := mustGet(t, s, turn).Steer.Tellings; len(told) == 1 && told[0].Hook {
		t.Error("a confirmation's telling is marked as read at a hook's step")
	}
	if got := mustGet(t, s, turn).Steer.Reads; got != reads+1 {
		t.Errorf("the record counts %d places, want the confirmation's %d", got, reads+1)
	}
	if read := notesByID(t, s, turn)[item.ID].Read; read == nil || read.Order != reads+1 || read.Launch != next {
		t.Errorf("the confirmed item is read %+v, want it read by job %s at place %d", read, next, reads+1)
	}
}

// A HOOK'S TAKE RECORDS NO TELLING. A take that hands notes over at a step is
// recorded before its answer reaches the harness, which may never take it: the
// item is read, and the record says nothing of the job's session holding its
// call, so every continuation tells it.
//
// Mutation: record a telling at every take of an item (telling-at-any-take),
// and the record holds one.
func testAHooksTakeRecordsNoTelling(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-telling-take"
	first, round, next := lateRound(t, s, turn)
	item := mustLate(t, s, turn, first, round, "delegate", 1)
	mustTake(t, s, turn, bridgeTake(next, item.ID))
	if got := tellingsOf(t, s, turn); len(got) != 0 {
		t.Errorf("a hook's take recorded tellings %v, want none", got)
	}
	if notesByID(t, s, turn)[item.ID].Waiting() {
		t.Error("the take left the item waiting")
	}
}

// A HELD VERDICT RECORDS A TELLING AT ITS OWN PLACE, AND A RESTARTED ONE NONE.
// The job's session shows its hook took one item after its last restart and
// another only before it: the first is read at its own place, its first
// serve's, and its call is told at the verdict's own new place, which the
// record's count moves to; a capture whose cursor was read off the record
// before the verdict is below it, and it is a hook's ([sandbox.CallTelling.Hook]).
// The second is read marked restarted, and a resume of the session is not
// known to hold it, so nothing is told of it.
//
// Mutation: record the telling at the item's own place (telling-at-item-place),
// on a restarted take too (telling-on-restarted), or not as a hook's
// (telling-hook-unmarked), and a check goes red.
func testAHeldVerdictRecordsATellingAtItsOwnPlaceAndARestartedOneNone(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-telling-verdict"
	first, round, next := lateRound(t, s, turn)
	held := mustLate(t, s, turn, first, round, "delegate", 1)
	restarted := mustLate(t, s, turn, first, round, "post_message", 2)
	mustServe(t, s, turn, hookServe(next, "take-1", callNo(1), held.ID), held.ID)
	mustServe(t, s, turn, hookServe(next, "take-2", callNo(2), restarted.ID), restarted.ID)
	place := notesByID(t, s, turn)[held.ID].Place
	reads := mustGet(t, s, turn).Steer.Reads
	if place == 0 || place >= reads {
		t.Fatalf("the premise: the held item's serve took place %d of %d, want one before the second's", place,
			reads)
	}
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: next, Held: []string{"take-1"},
		Restarted: []string{"take-2"}})
	if got, want := tellingsOf(t, s, turn), []string{telling(round, 1, next, reads+1)}; !slices.Equal(got, want) {
		t.Errorf("the record holds tellings %v, want %v: the held call alone, at the verdict's own place", got,
			want)
	}
	if told := mustGet(t, s, turn).Steer.Tellings; len(told) == 1 && !told[0].Hook {
		t.Error("a hook's telling is not marked as read at a hook's step")
	}
	if got := mustGet(t, s, turn).Steer.Reads; got != reads+1 {
		t.Errorf("the record counts %d places, want the verdict's %d", got, reads+1)
	}
	notes := notesByID(t, s, turn)
	if read := notes[held.ID].Read; read == nil || read.Order != place || read.Restarted {
		t.Errorf("the held item is read %+v, want it read at its own place %d", read, place)
	}
	if read := notes[restarted.ID].Read; read == nil || !read.Restarted {
		t.Errorf("the restarted item is read %+v, want it read marked restarted", read)
	}
}

// A TELLING OUTLIVES ITS ITEM. A relaunch that carries the round on removes
// every item of the job before ([sandbox.RunNote.For]) and keeps the tellings:
// the session that took the call holds it whichever job continues it.
//
// Mutation: clear the tellings with the items (telling-cleared-by-launch), and
// the record holds none after the relaunch.
func testATellingOutlivesItsItem(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-telling-outlives"
	first, round, next := lateRound(t, s, turn)
	item := mustLate(t, s, turn, first, round, "delegate", 1)
	mustTake(t, s, turn, confirmTake(next, item.ID))
	want := tellingsOf(t, s, turn)
	carryRoundOn(t, s, turn)
	if items := lateItems(t, s, turn); len(items) != 0 {
		t.Fatalf("the premise: the relaunch left items %v", itemCalls(items))
	}
	if got := tellingsOf(t, s, turn); len(want) != 1 || !slices.Equal(got, want) {
		t.Errorf("after a relaunch carrying the round on the record holds tellings %v, want %v", got, want)
	}
}

// A NEW ROUND CLEARS THE TELLINGS. A send-back's relaunch opens a new round,
// whose log numbers its calls afresh: a telling of the round before names a
// place no call of the new log holds.
//
// Mutation: keep the tellings across a new round (telling-kept-by-new-round),
// and the record still holds one.
func testANewRoundClearsTheTellings(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-telling-new-round"
	first, round, next := lateRound(t, s, turn)
	item := mustLate(t, s, turn, first, round, "delegate", 1)
	mustTake(t, s, turn, confirmTake(next, item.ID))
	if len(tellingsOf(t, s, turn)) != 1 {
		t.Fatal("the premise: the confirmation recorded no telling")
	}
	mustLaunched(t, s, run(turn))
	if got := mustGet(t, s, turn).Round; got == round {
		t.Fatalf("the premise: the send-back's relaunch works in round %q still", got)
	}
	if got := tellingsOf(t, s, turn); len(got) != 0 {
		t.Errorf("the new round's record holds tellings %v, want none", got)
	}
}

// TELLINGS ARE ONE PER CALL, AND BOUNDED. A call told to two jobs (the second
// handed it again by its opening's walk, which a brief read before the call
// forces here) is told once, at the higher place, the second job's: that job's
// session holds it from there alone; and past [sandbox.MaxCallTellings] the
// lowest places go first, the newest kept.
//
// Mutation: keep a telling per job (telling-per-job), keep the lower place
// (telling-keeps-lower), or drop the newest past the bound
// (telling-drop-newest), and a check goes red.
func testTellingsAreOnePerCallAndBounded(t *testing.T, s sandbox.PendingStore) {
	t.Run("one per call", func(t *testing.T) {
		const turn = "t-telling-once"
		first, round, next := lateRound(t, s, turn)
		item := mustLate(t, s, turn, first, round, "delegate", 1)
		mustTake(t, s, turn, confirmTake(next, item.ID))
		if len(tellingsOf(t, s, turn)) != 1 {
			t.Fatal("the premise: the first job's confirmation recorded no telling")
		}
		walked := run(turn)
		walked.Continues, walked.BriefRead = true, &sandbox.LogRead{Round: round}
		third := mustLaunched(t, s, walked)
		items := lateItems(t, s, turn)
		if len(items) != 1 || items[0].For != third {
			t.Fatalf("the premise: the record holds items %v, want the call handed again to %s", itemCalls(items),
				third)
		}
		mustTake(t, s, turn, confirmTake(third, items[0].ID))
		want := []string{telling(round, 1, third, mustGet(t, s, turn).Steer.Reads)}
		if got := tellingsOf(t, s, turn); !slices.Equal(got, want) {
			t.Errorf("the call told to two jobs is told %v, want once, at the second's place: %v", got, want)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		const turn = "t-telling-bounded"
		first, round, job := lateRound(t, s, turn)
		calls := sandbox.MaxCallTellings + 1
		for seq := 1; seq <= calls; seq++ {
			if seq > 1 && (seq-1)%sandbox.MaxJobItems == 0 {
				job = carryRoundOn(t, s, turn)
			}
			item := mustLate(t, s, turn, first, round, fmt.Sprintf("delegate_%d", seq), seq)
			mustTake(t, s, turn, confirmTake(job, item.ID))
		}
		var got []int
		for _, c := range mustGet(t, s, turn).Steer.Tellings {
			got = append(got, c.Appended)
		}
		var want []int
		for appended := 2; appended <= calls; appended++ {
			want = append(want, appended)
		}
		if !slices.Equal(got, want) {
			t.Errorf("past the bound the record tells calls %v, want the newest %d: %v", got,
				sandbox.MaxCallTellings, want)
		}
	})
}
