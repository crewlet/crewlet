package auxspend_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
		// A TURN'S METER OFF THE TURN STAGE would hold a reflection — run
		// after the turn, behind its own gate — on a turn that is over,
		// and charge it through a meter nothing asks again.
		"a reflection carrying a turn's meter": {Stage: types.AuxStageReflection,
			Purpose: types.AuxPersistDecider, Budget: openBudget{}},
		"a background pass carrying a turn's meter": {Stage: types.AuxStageBackground,
			Purpose: types.AuxEpisodeCompaction, Budget: openBudget{}},
	} {
		if err := use.Validate(); !errors.Is(err, auxspend.ErrUnattributed) {
			t.Errorf("%s: Validate = %v, want ErrUnattributed", name, err)
		}
	}
	ok := auxspend.Use{Stage: types.AuxStageTurn, TurnID: "t-1", Tally: auxspend.NewTally(),
		Budget: openBudget{}}
	if err := ok.For(types.AuxMemoryFilter).Validate(); err != nil {
		t.Errorf("a turn's call with its tally and its meter: %v", err)
	}
	if ok.Purpose != "" {
		t.Error("For changed the attribution it was called on")
	}
}

// openBudget is a turn's meter that holds nothing and records nothing.
type openBudget struct{}

func (openBudget) Held() error                                  { return nil }
func (openBudget) Record(context.Context, int, time.Time) error { return nil }

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
