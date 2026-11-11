package types_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// EVERY COMMITTED CHANGE KIND HAS AN OUTCOME CLASS, words or moved, and a
// turn's outcome is the highest its changes reach: nothing, then words, then
// moved. A new kind with no class fails here.
func TestEveryCommittedChangeKindHasAnOutcomeClass(t *testing.T) {
	t.Parallel()
	for _, c := range types.CommittedChanges {
		if o := c.Outcome(); o != types.OutcomeMoved && o != types.OutcomeWords {
			t.Errorf("%q has outcome %q, want moved or words", c, o)
		}
	}
	for _, tc := range []struct {
		changes []types.CommittedChange
		want    types.TurnOutcome
	}{
		{nil, types.OutcomeNothing},
		{[]types.CommittedChange{types.CommittedChat, types.CommittedComment}, types.OutcomeWords},
		{[]types.CommittedChange{types.CommittedComment, types.CommittedItem}, types.OutcomeMoved},
		{[]types.CommittedChange{types.CommittedAnswer}, types.OutcomeWords},
		{[]types.CommittedChange{types.CommittedAnswer, types.CommittedChat}, types.OutcomeWords},
		{[]types.CommittedChange{types.CommittedCode}, types.OutcomeMoved},
		{[]types.CommittedChange{types.CommittedPage}, types.OutcomeMoved},
	} {
		if got := types.OutcomeOf(tc.changes); got != tc.want {
			t.Errorf("OutcomeOf(%v) = %q, want %q", tc.changes, got, tc.want)
		}
	}
}

// A TURN'S WAKE IS THE OBJECT'S KIND AND WHY IT REACHED THE SEAT, a schedule by
// its name, else the waking event's type.
func TestATurnsWakeIsWhatWokeIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		trigger types.Trigger
		want    string
	}{
		{types.Trigger{Type: "external_notification",
			Origin: types.TriggerOrigin{Kind: types.OriginWorkItem, ID: "W-1", Reason: "mention"}}, "work_item:mention"},
		{types.Trigger{Type: "schedule_fired",
			Origin: types.TriggerOrigin{Kind: types.OriginSchedule, ID: "standup"}}, "schedule:standup"},
		{types.Trigger{Type: "a2a_request", Origin: types.TriggerOrigin{Kind: types.OriginColleagueAsk, ID: "c-1"}},
			"colleague_ask"},
		{types.Trigger{Type: "work_pickup"}, "work_pickup"},
	} {
		if got := types.WakeOf(tc.trigger); got != tc.want {
			t.Errorf("WakeOf(%+v) = %q, want %q", tc.trigger, got, tc.want)
		}
	}
}

// A PICKUP ANSWERED TYPED MOVED ITS ITEM (ADR-0122): an ask is a comment by
// kind, words on any other turn, and on a pickup the delivery check's own
// reading counts it a move; a pickup nothing typed answered keeps what its
// writes committed, an item that left the seat included.
//
// Mutation: read a pickup's outcome from its committed changes alone, and the
// advisor counts every pickup answered with a question as words.
func TestAPickupAnsweredTypedMovedItsItem(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		committed types.TurnOutcome
		answer    types.PickupAnswer
		want      types.TurnOutcome
	}{
		{types.OutcomeWords, types.PickupTyped, types.OutcomeMoved},
		{types.OutcomeMoved, types.PickupTyped, types.OutcomeMoved},
		{types.OutcomeWords, types.PickupUnanswered, types.OutcomeWords},
		{types.OutcomeNothing, types.PickupRemoved, types.OutcomeNothing},
		{types.OutcomeWords, types.PickupUnread, types.OutcomeWords},
		{types.OutcomeWords, "", types.OutcomeWords},
	} {
		if got := types.OutcomeOfPickup(c.committed, c.answer); got != c.want {
			t.Errorf("a turn committing %s and answering %q is %s, want %s", c.committed, c.answer, got, c.want)
		}
	}
	for _, a := range []types.PickupAnswer{types.PickupUnanswered, types.PickupTyped, types.PickupRemoved,
		types.PickupFinished, types.PickupReassigned, types.PickupUnread} {
		if !a.Valid() || a.Accepted() == (a == types.PickupUnanswered) ||
			a.Moot() != (a == types.PickupRemoved || a == types.PickupFinished || a == types.PickupReassigned) {
			t.Errorf("%q: valid %v accepted %v moot %v", a, a.Valid(), a.Accepted(), a.Moot())
		}
	}
	if types.PickupAnswer("pondered").Accepted() {
		t.Error("an answer this build does not know is accepted")
	}
}
