package types_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A TOLD CHANGE IS SAID AS CHANGES. A turn told changes too many to keep is
// recorded as a baseline and still told them, so the trail says how many it
// was told, and "for the first time" only of a record that told none.
//
// Mutation: decide the words on Baseline and the overflowing turn reads as a
// seat's first.
func TestAToldEventWithChangesIsNotAFirstTime(t *testing.T) {
	t.Parallel()
	overflow := types.AgentCapabilityTold{AgentHandle: "sre", Baseline: true, Changes: 30}
	if got := overflow.SummaryFor(""); strings.Contains(got, "first time") || !strings.Contains(got, "30 changes") {
		t.Errorf("a baseline that told 30 changes reads %q", got)
	}
	first := types.AgentCapabilityTold{AgentHandle: "sre", Baseline: true}
	if got := first.SummaryFor(""); !strings.Contains(got, "first time") {
		t.Errorf("a seat's first told record reads %q", got)
	}
}
