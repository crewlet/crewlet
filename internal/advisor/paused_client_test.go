package advisor_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD KNOWS EVERY CLASS A PAUSED SEAT RAISES: the Inbox says a seat
// the engine paused by its failure's finding, which names the seat and the
// fix, and draws no second row for the same pause. A class it did not know
// would draw the pause twice; one it knew that the engine no longer raises
// would hide a seat's row behind a finding that never comes.
func TestTheDashboardKnowsEveryClassAPausedSeatRaises(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "PAUSED_SEAT_CLASSES")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	want := make([]string, 0, len(advisor.PausedClasses()))
	for _, c := range advisor.PausedClasses() {
		want = append(want, string(c))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's paused classes are %q, the engine's %q", got, want)
	}
}
