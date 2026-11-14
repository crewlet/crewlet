package runner

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// THE CALLS DROPPED AFTER A SAVE ARE NEVER UNDER-COUNTED. A continued session is
// told how many of the calls its run made after the save its round's log
// dropped from the middle: the log's appends past the save point — the calls it
// lists and the ones it tallies — less the listed calls numbered past it, which
// is exact where every call is numbered. A call an older build appended in a
// fleet mid-upgrade has no number, so its place is unknown and it is never
// taken off: the count may take a call the brief lists for a dropped one, never
// leave a dropped one out, and it is held to the dropped calls the round counts
// at all. A save point past every call the resume hands over — a capture placed
// off a copy of the log that counted a call no resume reads — counts none.
//
// Mutation: take a call with no number off the count, drop the hold to the
// round's dropped calls, or let a save point past the round's calls count
// below none, and a row goes red.
func TestTheDroppedCallsAfterASaveAreNeverUnderCounted(t *testing.T) {
	t.Parallel()
	// numbered is the calls the log took from..to, each at its own number.
	numbered := func(from, to int) []BridgedCall {
		var out []BridgedCall
		for n := from; n <= to; n++ {
			out = append(out, BridgedCall{Seq: n, Appended: n})
		}
		return out
	}
	// older is n calls an older build appended, which it numbered nowhere.
	older := func(n int) []BridgedCall { return make([]BridgedCall, n) }
	join := func(parts ...[]BridgedCall) []BridgedCall {
		var out []BridgedCall
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	// A ROUND OF THREE HUNDRED whose log lists its first and last hundred
	// and dropped the hundred between.
	listed := join(numbered(1, 100), numbered(201, 300))
	for name, tc := range map[string]struct {
		bridged []BridgedCall
		dropped int
		through int
		want    int
	}{
		"saved part way through the dropped middle": {bridged: listed, dropped: 100, through: 150, want: 50},
		"saved before any call":                     {bridged: listed, dropped: 100, through: 0, want: 100},
		"saved after the dropped middle":            {bridged: listed, dropped: 100, through: 250, want: 0},
		// THE LAST CALL AN OLDER BUILD'S, after the save: listed, and
		// counted as dropped too, its place unknown.
		"a listed call an older build appended after the save": {
			bridged: join(numbered(1, 100), numbered(201, 299), older(1)), dropped: 100, through: 250,
			want: 1},
		// MOST OF THE LIST AN OLDER BUILD'S: every one of them may be after
		// the save, but no more calls were dropped than the round counts.
		"more calls an older build appended than the round dropped": {
			bridged: join(numbered(1, 50), older(150)), dropped: 100, through: 40, want: 100},
		"a save point past every call the resume hands over": {through: 5, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			round := roundCalls{bridged: tc.bridged}
			if tc.dropped > 0 {
				round.elided = []ledger.Elided{{Name: "slack_post", Calls: tc.dropped}}
			}
			if got := droppedAfterSave(round, tc.through); got != tc.want {
				t.Fatalf("a session saved at %d is told %d calls were dropped after its save, want %d",
					tc.through, got, tc.want)
			}
		})
	}
}

// THE CALLS A BRIEF LEFT OUT ARE THE ONES ITS RENDER COUNTED. The reads a brief
// counts without listing them are what its launch records it left out, for the
// next brief to tell, so they are read off the render's own walk
// ([ledger.ShownCallGroups]), never off a rule of the runner's: two identical
// reads side by side either side of the read cap are told apart by their
// places, the earlier shown and the later left out, a write is never named, and
// as many are named as the render's omitted line counts.
//
// Mutation: match a shown call to the last equal call of the group rather than
// the next, walking it from its end, and the earlier read is named in the later
// one's place.
func TestTheCallsABriefLeftOutAreTheOnesItsRenderCounted(t *testing.T) {
	t.Parallel()
	read := func(channel string, appended int) BridgedCall {
		return BridgedCall{Call: ledger.Call{Name: "slack_history", Args: map[string]any{"channel": channel}},
			Seq: appended, Appended: appended}
	}
	var group []BridgedCall
	for n := 1; n < ledger.MaxReadCalls; n++ {
		group = append(group, read(fmt.Sprintf("C%d", n), n))
	}
	// THE CAP'S LAST READ AND THE FIRST PAST IT, side by side and alike in
	// everything but their places; then a write, and a read after.
	group = append(group, read("same", ledger.MaxReadCalls), read("same", 13),
		BridgedCall{Call: ledger.Call{Name: "slack_post", Args: map[string]any{"text": "a write"}},
			Seq: 14, Appended: 14},
		read("C15", 15))
	opts := ledger.Format(nil, []string{"slack_history"})
	calls := bridgedLedger(group)
	shown := ledger.ShownCallGroups([][]ledger.Call{calls}, opts)[0]
	var places []int
	for _, call := range leftOut(group, shown) {
		places = append(places, call.Appended)
	}
	if !slices.Equal(places, []int{13, 15}) {
		t.Errorf("the brief left out the calls at %v, want the later of the two alike and the read after the "+
			"write, [13 15]", places)
	}
	told := ledger.FormatCalls(calls, opts)
	if omitted := fmt.Sprintf("(+%d further read call(s) omitted)", len(places)); !strings.Contains(told,
		omitted) {
		t.Errorf("the render counts other than the %d calls named left out:\n%s", len(places), told)
	}
}
