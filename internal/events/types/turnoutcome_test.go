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
