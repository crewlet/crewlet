package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/search"
)

// THE SEMANTIC INDEX'S DUTY READS THIS NODE AS CURRENT ON THE VECTOR LOG once
// it has applied everything the log holds: the step decides nothing from a
// node behind it, so a node with nothing left to apply read as behind would
// never take one.
func TestTheIndexDutyReadsANodeThatAppliedTheLogAsCurrent(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	duty := &embedDuty{engine: e, log: s.Domain(search.Domain{}.Name())}
	standing, err := duty.standing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !standing.Current {
		t.Fatal("a node with nothing on its vector log to apply is read as behind it")
	}
}

// A TICK KEEPS ITS LEASE WHILE IT RUNS, AND STOPS THE MOMENT IT CANNOT.
//
// A training on a large corpus runs longer than the interval the lease is
// claimed on, so the tick renews it as it goes; a renewal that is refused —
// or that cannot be answered, since a node that cannot say it holds the duty
// must not publish as its holder — cancels the tick, so the singleton never has
// two writers. And the renewals stop with the tick: their goroutine is the
// tick's, and outliving it would renew a lease nobody is using.
func TestATickStopsWhenItsLeaseCannotBeRenewed(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(int) (bool, error){
		"refused":    func(int) (bool, error) { return false, nil },
		"unanswered": func(int) (bool, error) { return false, coord.ErrUnavailable },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			calls := 0
			d := &embedDuty{renewEvery: 5 * time.Millisecond,
				claim: func(context.Context) (bool, error) {
					mu.Lock()
					defer mu.Unlock()
					calls++
					if calls < 3 {
						return true, nil
					}
					return answer(calls)
				}}
			tick, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			stop := d.keepClaimed(tick, cancel)
			<-tick.Done()
			stop()
			if !errors.Is(tick.Err(), context.Canceled) {
				t.Fatalf("the tick ended with %v, want cancelled by the lost lease",
					tick.Err())
			}
			mu.Lock()
			held := calls
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if calls != held || held != 3 {
				t.Fatalf("the lease was claimed %d times, then %d — renewals "+
					"continued after the tick ended", held, calls)
			}
		})
	}
}
