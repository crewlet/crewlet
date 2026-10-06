package auxspend_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events/types"
)

// AN UNATTRIBUTED CALL IS REFUSED, not filed as "unknown": the zero attribution
// names no stage and no purpose, and spend filed under neither is a row every
// breakdown shows and nobody can place.
func TestAnUnattributedCallIsRefused(t *testing.T) {
	t.Parallel()
	for name, use := range map[string]auxspend.Use{
		"the zero value":     {},
		"no purpose":         {Stage: types.AuxStageTurn},
		"an unknown purpose": {Stage: types.AuxStageTurn, Purpose: "a_later_worker"},
		"no stage":           {Purpose: types.AuxMemoryFilter},
		// A TALLY OFF THE TURN STAGE would charge a reflection — the
		// seat's learning — to the turn's work item (ADR-0022).
		"a reflection carrying a tally": {Stage: types.AuxStageReflection,
			Purpose: types.AuxPersistDecider, Tally: auxspend.NewTally()},
	} {
		if err := use.Validate(); !errors.Is(err, auxspend.ErrUnattributed) {
			t.Errorf("%s: Validate = %v, want ErrUnattributed", name, err)
		}
	}
	ok := auxspend.Use{Stage: types.AuxStageTurn, TurnID: "t-1", Tally: auxspend.NewTally()}
	if err := ok.For(types.AuxMemoryFilter).Validate(); err != nil {
		t.Errorf("a turn's call with its tally: %v", err)
	}
	if ok.Purpose != "" {
		t.Error("For changed the attribution it was called on")
	}
}

// A TALLY SUMS WHAT IT IS GIVEN, from every goroutine a turn hands it to, and
// a nil one counts nothing.
func TestATallySumsConcurrentCalls(t *testing.T) {
	t.Parallel()
	tally := auxspend.NewTally()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			tally.Add(auxspend.Spent{Calls: 1, Input: 10, Output: 2, CacheRead: 4})
		})
	}
	wg.Wait()
	if got := tally.Total(); got != (auxspend.Spent{Calls: 50, Input: 500, Output: 100, CacheRead: 200}) ||
		got.Tokens() != 600 {
		t.Errorf("total = %+v", got)
	}
	var none *auxspend.Tally
	none.Add(auxspend.Spent{Calls: 1})
	if none.Total() != (auxspend.Spent{}) {
		t.Error("a nil tally counted")
	}
}
