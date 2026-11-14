package sandboxtest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a call's ENTRY IN FLIGHT ([sandbox.BridgeLog.InFlight]): written
// ahead of the call, before its tool runs, and taken out by the call's one update
// as it finishes — so no call runs off its run's record (internal/api/mcpbridge,
// "A call is on its run's record before it runs").
var inFlightCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ACallsEntryIsUpdatedOnceByItsIdentity", testACallsEntryIsUpdatedOnceByItsIdentity},
	{"ALogAtItsCallsInFlightRefusesTheNextBegin", testALogAtItsCallsInFlightRefusesTheNextBegin},
	{"ALaterCallIsNumberedAfterACallStillInFlight", testALaterCallIsNumberedAfterACallStillInFlight},
	{"ACallCarriesItsHarnessIDWholeAndIsNeverFoundByIt", testACallCarriesItsHarnessIDWholeAndIsNeverFoundByIt},
}

// beginCall writes the entry of a's call on turnID's log ahead of it, as its
// session's ledger does ([sandbox.AppendBegin]), and fails t unless the log took
// it.
func beginCall(t *testing.T, s sandbox.PendingStore, turnID string, a sandbox.BridgeAppend) {
	t.Helper()
	a.Step = sandbox.AppendBegin
	if taken, err := s.AppendBridgeCall(t.Context(), turnID, a); err != nil || !taken {
		t.Fatalf("the entry of %s at place %d = %v, %v, want it taken", a.Call.Name, a.Call.Seq, taken, err)
	}
}

// A CALL'S ENTRY IS UPDATED ONCE, BY ITS IDENTITY. The bridge writes a call's
// entry ahead of it — its identity, with who runs it and until when, beside the
// id its harness gave it — and the call's one update, as it finishes, takes the
// entry out and lists the finished call in its place in the same write: the log
// never holds two records of one call. The update finds the entry by the call's
// place, the instant the bridge took it and its tool, never by its digest, which
// an entry leaves out for a tool that may ask a person first and the finished
// call then carries; so another opening's call at the same place, taken at
// another instant, is another entry, left as it is. Asked again — a begin or an
// update whose answer was lost — each writes nothing more. So a call a reader
// took for one whose outcome is unknown, its entry read in flight, has that
// unknown replaced by its own outcome once its update lands, while the run's
// record stands. The finished call is listed with neither who ran it nor its
// bound, which only its entry carries, though its update hands it both. And a
// withdrawal takes the entry of a call that was then not run out, listing
// nothing.
//
// Mutation: list the call without taking its entry out, and the log holds the
// call twice, finished and in flight; keep a finished call's runner and bound
// as handed (kept-call-keeps-its-runner), and the call is listed with both.
func testACallsEntryIsUpdatedOnceByItsIdentity(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-in-flight-once"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	entry := madeAt("post_message", 1, 1)
	entry.Runner, entry.Until = "node-a:incarnation", base.Add(time.Hour)
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: entry})
	other := madeAt("post_message", 1, 2)
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: other})
	if taken, err := s.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Step: sandbox.AppendBegin,
		Launch: job, Round: round, Call: entry}); err != nil || !taken {
		t.Errorf("a begin asked again = %v, %v, want it answered taken, so its call runs", taken, err)
	}
	got := mustLogged(t, s, turn).Log
	if len(got.InFlight) != 2 || len(got.Calls) != 0 || got.InFlight[0].Runner != entry.Runner ||
		!got.InFlight[0].Until.Equal(entry.Until) {
		t.Fatalf("the log holds %+v in flight and %d calls, want both entries, the first with who runs it "+
			"and its bound, and no call", got.InFlight, len(got.Calls))
	}

	finished := madeAt("post_message", 1, 1)
	finished.Digest, finished.Output = digestOf(1), "posted"
	// AND WHO RAN IT UNTIL WHEN, as a writer may hand them: the finished call
	// keeps neither ([sandbox.BridgeCall.Runner]).
	finished.Runner, finished.Until = entry.Runner, entry.Until
	for range 2 {
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: finished}) {
			t.Fatal("the call's update was not recorded")
		}
	}
	got = mustLogged(t, s, turn).Log
	listsExactly(t, got.Calls, []wantCall{{"post_message", 1, 1, false}})
	if c := got.Calls[0]; c.Output != "posted" || c.Digest != digestOf(1) || c.Runner != "" || !c.Until.IsZero() {
		t.Errorf("the finished call is listed %+v, want what it answered and its digest, and neither who ran "+
			"it nor its bound", c)
	}
	if len(got.InFlight) != 1 || !got.InFlight[0].StartedAt.Equal(other.StartedAt) {
		t.Fatalf("after the update the log holds %+v in flight, want the other opening's entry alone",
			got.InFlight)
	}

	if taken, err := s.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Step: sandbox.AppendWithdraw,
		Launch: job, Round: round, Call: other}); err != nil || !taken {
		t.Fatalf("the withdrawal = %v, %v, want it taken", taken, err)
	}
	got = mustLogged(t, s, turn).Log
	if len(got.InFlight) != 0 || len(got.Calls) != 1 {
		t.Errorf("after the withdrawal the log holds %d in flight and %d calls, want none and the one",
			len(got.InFlight), len(got.Calls))
	}
}

// A LOG AT ITS CALLS IN FLIGHT REFUSES THE NEXT BEGIN. The log holds at most
// [sandbox.MaxCallsInFlight] entries, each its share of the log's byte budget;
// the next begin is refused with [sandbox.ErrCallsInFlight] and writes nothing,
// so its call is not run — and once one of the calls in flight has finished,
// the next begin is taken.
//
// Mutation: take a begin past the bound, and the log holds one more entry than
// its bytes are measured for.
func testALogAtItsCallsInFlightRefusesTheNextBegin(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-in-flight-full"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	for i := range sandbox.MaxCallsInFlight {
		beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("read_page", i+1, i+1)})
	}
	next := madeAt("read_page", sandbox.MaxCallsInFlight+1, sandbox.MaxCallsInFlight+1)
	taken, err := s.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Step: sandbox.AppendBegin,
		Launch: job, Round: round, Call: next})
	if taken || !errors.Is(err, sandbox.ErrCallsInFlight) {
		t.Errorf("a begin past the bound = %v, %v, want refused with sandbox.ErrCallsInFlight", taken, err)
	}
	if got := mustLogged(t, s, turn).Log; len(got.InFlight) != sandbox.MaxCallsInFlight {
		t.Errorf("the log holds %d entries in flight, want its bound %d", len(got.InFlight),
			sandbox.MaxCallsInFlight)
	}
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("read_page", 1, 1)}) {
		t.Fatal("the first call's update was not recorded")
	}
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: next})
}

// A LATER CALL IS NUMBERED AFTER A CALL STILL IN FLIGHT. Whatever numbers the
// round's next call reads the log's last place ([sandbox.BridgeLog.LastSeq]) —
// a node taking a run's seat over while a call of the old holder's opening still
// runs, a relaunch carrying the round on, an answered round's cut — and a call
// in flight counts there, so no later call is numbered at a place a call whose
// update has not landed holds. A relaunch that carries the round on keeps the
// entry, and the call's update, landing once its job was replaced, is listed
// late; so is a call the replaced job's box begins while its opening still
// serves it, numbered after both.
//
// Mutation: read the last place off the listed calls alone, and the log's last
// place is the finished call's.
func testALaterCallIsNumberedAfterACallStillInFlight(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-in-flight-numbered"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("read_page", 1, 1)}) {
		t.Fatal("the first call was not recorded")
	}
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("delegate", 2, 2)})
	if last := mustLogged(t, s, turn).Log.LastSeq(); last != 2 {
		t.Errorf("the log's last place is %d, want the call in flight's, 2", last)
	}
	carryRoundOn(t, s, turn)
	if got := mustLogged(t, s, turn).Log; got.LastSeq() != 2 || len(got.InFlight) != 1 {
		t.Errorf("the relaunch's log holds %d in flight and numbers after %d, want the entry kept and 2",
			len(got.InFlight), got.LastSeq())
	}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("delegate", 2, 2)}) {
		t.Fatal("the update of the call in flight, landing once its job was replaced, was not recorded")
	}
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("read_page", 3, 3)})
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("read_page", 3, 3)}) {
		t.Fatal("the update of a call the replaced job began was not recorded")
	}
	got := mustLogged(t, s, turn).Log
	listsExactly(t, got.Calls, []wantCall{{"read_page", 1, 1, false}, {"delegate", 2, 2, true},
		{"read_page", 3, 3, true}})
	if len(got.InFlight) != 0 {
		t.Errorf("the log holds %+v in flight once the call's update landed", got.InFlight)
	}
}

// A CALL CARRIES ITS HARNESS'S ID WHOLE, AND IS NEVER FOUND BY IT. The bridge
// hands a call's entry, and its one update, the id the call's harness gave it
// ([sandbox.BridgeCall.CallID]): the log keeps it on the entry while the call
// runs, and on the finished call its update lists in the entry's place. It is
// the box's word and no part of the call's identity — a harness that makes a
// call again may send the id it sent before — so two calls under one id, at two
// places, are two entries and two finished calls. Held whole or not at all, on
// the entry and the record alike, whatever the caller handed: an id at
// [sandbox.MaxBridgeCallIDBytes] is kept whole, and one past it or outside
// letters, digits, `-` and `_` is kept as none, never cut. And an entry
// withdrawn takes its id with it.
//
// Mutation: keep an entry without its id (entry-keeps-no-id), find an entry by
// its id (entry-found-by-its-id), or keep an id whatever it is
// (log-keeps-any-id), and this goes red.
func testACallCarriesItsHarnessIDWholeAndIsNeverFoundByIt(t *testing.T, s sandbox.PendingStore) {
	const (
		turn  = "t-call-id"
		twice = "toolu_01madeagain"
	)
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	idAt := func(name string, seq int, id string) sandbox.BridgeCall {
		call := madeAt(name, seq, seq)
		call.CallID = id
		return call
	}
	whole := strings.Repeat("a", sandbox.MaxBridgeCallIDBytes)
	calls := []struct {
		call sandbox.BridgeCall
		want string
	}{
		{idAt("post_message", 1, twice), twice},
		{idAt("post_message", 2, twice), twice},
		{idAt("read_page", 3, whole), whole},
		{idAt("read_page", 4, whole+"a"), ""},
		{idAt("read_page", 5, "toolu_a/b"), ""},
	}
	for _, c := range calls {
		beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: c.call})
	}
	got := mustLogged(t, s, turn).Log
	if len(got.InFlight) != len(calls) {
		t.Fatalf("the log holds %d entries in flight, want %d: two calls under one id are two calls",
			len(got.InFlight), len(calls))
	}
	for i, c := range calls {
		if e := got.InFlight[i]; e.Seq != c.call.Seq || e.CallID != c.want {
			t.Errorf("the entry at place %d carries the id %q, want %q", e.Seq, e.CallID, c.want)
		}
	}

	for _, c := range calls {
		finished := c.call
		finished.Output = "done"
		if !recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: finished}) {
			t.Fatalf("the update of the call at place %d was not recorded", c.call.Seq)
		}
	}
	got = mustLogged(t, s, turn).Log
	if len(got.InFlight) != 0 || len(got.Calls) != len(calls) {
		t.Fatalf("after the updates the log holds %d in flight and %d calls, want none and %d",
			len(got.InFlight), len(got.Calls), len(calls))
	}
	for i, c := range calls {
		if call := got.Calls[i]; call.Seq != c.call.Seq || call.CallID != c.want {
			t.Errorf("the call at place %d is listed under the id %q, want %q", call.Seq, call.CallID, c.want)
		}
	}

	withdrawn := idAt("delegate", 6, "toolu_01neverran")
	beginCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: withdrawn})
	if taken, err := s.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Step: sandbox.AppendWithdraw,
		Launch: job, Round: round, Call: madeAt("delegate", 6, 6)}); err != nil || !taken {
		t.Fatalf("the withdrawal = %v, %v, want it taken", taken, err)
	}
	if got = mustLogged(t, s, turn).Log; len(got.InFlight) != 0 || len(got.Calls) != len(calls) {
		t.Errorf("after the withdrawal the log holds %+v in flight and %d calls, want none and %d",
			got.InFlight, len(got.Calls), len(calls))
	}
}
