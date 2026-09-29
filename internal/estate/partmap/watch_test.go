package partmap

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// A CONDITION IS MEASURED FROM WHEN THIS NODE FIRST SAW IT, and only for as
// long as it has gone on seeing it: a sighting where it does not hold ends the
// run, and the next is counted from again — so an alarm never fires on a
// shortfall nobody saw hold for its whole grace.
func TestAWatchMeasuresAShortfallOnlyWhileItHolds(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(3)...)
	s.settle(40)
	var w Watch
	if f := w.Observe(s.state.Map, s.live(), base); len(f.Short) != 0 || len(f.Unserved) != 0 {
		t.Fatalf("a settled map reads %+v", f)
	}

	s.nodes["data-00"].down = true
	first := w.Observe(s.state.Map, s.live(), base)
	if len(first.Short) == 0 || first.Short[0].For != 0 {
		t.Fatalf("the first sighting of a shortfall reads %+v, want it seen for nothing yet", first.Short)
	}
	later := w.Observe(s.state.Map, s.live(), base.Add(11*time.Minute))
	if later.Short[0].For != 11*time.Minute {
		t.Errorf("a shortfall seen for eleven minutes reads %v", later.Short[0].For)
	}

	// BACK FOR ONE SIGHTING, and the run is over.
	s.nodes["data-00"].down = false
	if f := w.Observe(s.state.Map, s.live(), base.Add(12*time.Minute)); len(f.Short) != 0 {
		t.Fatalf("a whole map still reads short: %+v", f.Short)
	}
	s.nodes["data-00"].down = true
	again := w.Observe(s.state.Map, s.live(), base.Add(13*time.Minute))
	if again.Short[0].For != 0 {
		t.Errorf("a shortfall that broke and began again reads %v, want counted from again",
			again.Short[0].For)
	}
}

// A JOIN IS COUNTED FROM ITS OWN START: one withdrawn and named again at a
// later epoch is a new join, and a map written again from nothing is another
// map, nothing of the old one holding.
func TestAWatchCountsEachJoinFromItsOwnStart(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(2)...)
	s.settle(40)
	s.add("data-02", 1, nil)
	s.tick()
	var w Watch
	w.Observe(s.state.Map, s.live(), base)
	f := w.Observe(s.state.Map, s.live(), base.Add(40*time.Minute))
	if len(f.Joining) == 0 || f.Joining[0].For != 40*time.Minute || f.Joining[0].Node != "data-02" {
		t.Fatalf("a join seen for forty minutes reads %+v", f.Joining)
	}

	renamed := s.state.Map.Clone()
	for g := range renamed.Partitions {
		for i := range renamed.Partitions[g].Holders {
			if h := &renamed.Partitions[g].Holders[i]; h.State == Joining {
				h.Since++
			}
		}
	}
	renamed.Epoch++
	if f := w.Observe(renamed, s.live(), base.Add(41*time.Minute)); f.Joining[0].For != 0 {
		t.Errorf("a join named again at a later epoch reads %v, want counted from its own start",
			f.Joining[0].For)
	}

	w.Observe(renamed, s.live(), base.Add(50*time.Minute))
	rewritten := renamed.Clone()
	rewritten.Generation = uuid.New()
	if f := w.Observe(rewritten, s.live(), base.Add(51*time.Minute)); f.Joining[0].For != 0 {
		t.Errorf("a map written again from nothing kept the old one's join: %v", f.Joining[0].For)
	}
}

// A BREAK IN WHAT THE NODE COULD SEE IS A BREAK IN THE CONDITION: forgotten, the
// watch counts a shortfall it sees again from again.
func TestAWatchThatForgotCountsFromAgain(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 1, nodeIDs(2)...)
	s.settle(40)
	s.nodes["data-00"].down = true
	var w Watch
	w.Observe(s.state.Map, s.live(), base)
	w.Forget()
	f := w.Observe(s.state.Map, s.live(), base.Add(time.Hour))
	if len(f.Unserved) == 0 || f.Unserved[0].For != 0 {
		t.Errorf("after forgetting, an unserved partition reads %+v, want seen for nothing yet",
			f.Unserved)
	}
}

// THE LONGEST HELD COMES FIRST, which is the one an alarm's detail names.
func TestAWatchNamesTheLongestHeldFirst(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(4)...)
	s.settle(80)
	var w Watch
	s.nodes["data-00"].down = true
	w.Observe(s.state.Map, s.live(), base)
	s.nodes["data-01"].down = true
	f := w.Observe(s.state.Map, s.live(), base.Add(time.Minute))
	if len(f.Short) < 2 {
		t.Fatalf("two nodes down left %d partitions short", len(f.Short))
	}
	for i := 1; i < len(f.Short); i++ {
		if f.Short[i].For > f.Short[i-1].For {
			t.Fatalf("the shortfalls are not longest first: %+v", f.Short)
		}
	}
	if f.Short[0].For != time.Minute {
		t.Errorf("the longest shortfall reads %v, want the minute since data-00 went", f.Short[0].For)
	}
}
