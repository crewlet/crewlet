package coord_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A RETIREMENT STANDS ONLY UNTIL ITS NODE WRITES AGAIN: against no row, or a
// row the store took before it, the id stays retired; a row stored after it is
// a node running under the id, which takes part again.
//
// Mutation: compare the row with At (the writer's clock) or let a later row
// leave it standing, and a node back under its id stays retired.
func TestARetirementStandsOnlyUntilItsNodeWritesAgain(t *testing.T) {
	t.Parallel()
	stored := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	r := coord.NodeRetirement{NodeID: "node-9", StoredAt: stored}
	for row, stands := range map[time.Time]bool{
		{}:                          true,
		stored.Add(-time.Minute):    true,
		stored:                      true,
		stored.Add(time.Nanosecond): false,
	} {
		if r.Stands(row) != stands {
			t.Errorf("a retirement stored at %v against a row stored at %v stands %t, want %t", stored, row,
				r.Stands(row), stands)
		}
	}
}
