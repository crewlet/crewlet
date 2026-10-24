package statelog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE ANSWER OVER SEVERAL RECORDS IS THE LEAST CERTAIN OF THEM, in either
// order, and a record that appended nothing claims nothing.
//
// It is what every tool appending more than one record answers — a save and
// its rename, an item and its dependency — and the promise every transport
// makes on top of it is that an unknown write is never reported applied.
func TestTheLessCertainOutcomeWinsEitherWay(t *testing.T) {
	t.Parallel()
	order := []statelog.Outcome{"", statelog.OutcomeApplied,
		statelog.OutcomePending, statelog.OutcomeUnknown}
	for i, weaker := range order {
		for _, stronger := range order[:i+1] {
			if got := statelog.LessCertain(stronger, weaker); got != weaker {
				t.Errorf("LessCertain(%q, %q) = %q, want %q", stronger, weaker, got, weaker)
			}
			if got := statelog.LessCertain(weaker, stronger); got != weaker {
				t.Errorf("LessCertain(%q, %q) = %q, want %q", weaker, stronger, got, weaker)
			}
		}
	}
}

// THE LATER POSITION IS GENERATION FIRST, and the zero position loses to any
// other — so a record that appended nothing never lowers a call's floor.
func TestTheLaterPositionIsGenerationFirst(t *testing.T) {
	t.Parallel()
	old := statelog.Position{Stream: "S", Generation: 1, Seq: 900}
	next := statelog.Position{Stream: "S", Generation: 2, Seq: 3}
	if got := statelog.Later(old, next); got != next {
		t.Errorf("Later = %s, want the next generation's %s", got, next)
	}
	if got := statelog.Later(next, statelog.Position{}); got != next {
		t.Errorf("Later with the zero position = %s, want %s", got, next)
	}
}

// A POSITION ON ANOTHER LOG IS NEVER LATER, however large its number.
//
// A packed position is a coordinate in one log's number space, and two logs'
// spaces are unrelated: compared bare, the knowledge base's sequence 90 would
// "follow" the tracker's sequence 4 for no reason but the sizes of two
// counters, and a caller would barrier on a log it never reads. The first log
// keeps the answer, in either argument order.
//
// Mutation: compare packed values in Later, and this fails.
func TestAPositionOnAnotherLogIsNeverLater(t *testing.T) {
	t.Parallel()
	tracker := statelog.Position{Stream: "TRACKER", Generation: 1, Seq: 4}
	pages := statelog.Position{Stream: "PAGES", Generation: 3, Seq: 90}
	if got := statelog.Later(tracker, pages); got != tracker {
		t.Errorf("Later(%s, %s) = %s, want the first log's position kept", tracker, pages, got)
	}
	if got := statelog.Later(pages, tracker); got != pages {
		t.Errorf("Later(%s, %s) = %s, want the first log's position kept", pages, tracker, got)
	}
}

// AN UNKNOWN WRITE SPEAKS FOR A RECORD although it names no position, and a
// write that decided it had nothing to append speaks for none.
func TestAnUnknownWriteStillWrote(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		result statelog.Result
		want   bool
	}{
		"unknown, no position": {statelog.Result{Outcome: statelog.OutcomeUnknown}, true},
		"applied at a position": {statelog.Result{Outcome: statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "S", Seq: 4}}, true},
		"applied, nothing appended": {statelog.Result{Outcome: statelog.OutcomeApplied}, false},
		"the zero result":           {statelog.Result{}, false},
	}
	for name, c := range cases {
		if got := c.result.Wrote(); got != c.want {
			t.Errorf("%s: Wrote() = %v, want %v", name, got, c.want)
		}
	}
}
