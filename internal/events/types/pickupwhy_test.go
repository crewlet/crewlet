package types_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// THE BRIEF SAYS WHERE AN ITEM SITS AS THE ORDER PUTS IT (ADR-0159). An urgent
// item the seat has not started comes before started work, and its brief says
// so; started work and a review come before unstarted work only where that
// work is not urgent, and their briefs say that too.
//
// Mutation: say the old in-progress sentence, and the brief claims started
// work comes before an urgent item the pickup hands over first.
func TestAPickupsBriefSaysWhereUrgentWorkSits(t *testing.T) {
	t.Parallel()
	brief := func(why types.PickupWhy) string {
		return types.WorkPickup{Key: "ENG-1", Status: "todo", Why: why}.Brief()
	}
	if !types.PickupUrgent.Valid() {
		t.Fatal("the urgent reason is not one this build knows")
	}
	if got := brief(types.PickupUrgent); !strings.Contains(got,
		"It is urgent, so it comes before every item that is not, work you already started included.") {
		t.Errorf("urgent brief = %q, want it to say it comes before started work", got)
	}
	for _, why := range []types.PickupWhy{types.PickupInProgress, types.PickupReview} {
		if got := brief(why); !strings.Contains(got, "unless that work is urgent") {
			t.Errorf("%s brief = %q, want it to say urgent work comes first", why, got)
		}
	}
}
