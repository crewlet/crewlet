package worklet

import (
	"testing"
	"time"
)

// AN ITEM THAT LEFT PROGRESS STOPS COUNTING THEN: its last state stands until
// it left, not until the window ends. Mutation: ignore when it left, and the
// finished item's due hours run on.
func TestAnItemThatLeftProgressStopsCountingThen(t *testing.T) {
	t.Parallel()
	from := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	got := sumFlowHours([]flowSpan{
		{task: "t-1", seat: "ada", state: FlowDue, start: from.Add(time.Hour), left: from.Add(2 * time.Hour)},
		{task: "t-2", seat: "ada", state: FlowDue, start: from.Add(-time.Hour), left: from.Add(-time.Minute)},
	}, from, from.Add(4*time.Hour))
	if len(got) != 1 || got[0].Stood != time.Hour || got[0].Items != 1 {
		t.Errorf("flow hours %+v, want one hour of t-1 alone", got)
	}
}
