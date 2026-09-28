package membership

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/placement"
)

var stamp = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// full is a state with every field set, beside the members it agrees with.
func full() (State, []placement.Member) {
	s := State{
		Absence: map[string]Absence{"data-a": {Ticks: 3, Present: 1, Since: stamp,
			Reason: ReasonUnhealthy, Detail: "fsync failed"}},
		Removed: map[string]Removal{"data-c": {Present: 2, At: stamp, Reason: ReasonAbsent}},
		Config:  ConfigSource{Epoch: 1_790_000_000_000},
		Hold:    &Hold{Until: stamp.Add(time.Hour), By: "ops", Reason: "upgrade", At: stamp},
		TakenOut: map[string]Gesture{"data-b": {By: "ops", Reason: "decommission",
			At: stamp}},
	}
	members := []placement.Member{
		{Node: "data-a", Weight: 2, Share: 2 << 16},
		{Node: "data-b", Weight: 1, Share: 1 << 16, Out: true},
		{Node: "data-c", Weight: 1, Share: 1 << 16, Probation: true},
	}
	return s, members
}

// A CLONE SHARES NOTHING: a maintainer builds the next state from a clone of
// one its node may still be placing by.
func TestACloneSharesNothing(t *testing.T) {
	t.Parallel()
	orig, _ := full()
	c := orig.Clone()
	c.Absence["data-a"] = Absence{}
	c.Removed["data-z"] = Removal{}
	c.TakenOut["data-a"] = Gesture{}
	c.Hold.Reason = "changed"
	if want, _ := full(); !reflect.DeepEqual(orig, want) {
		t.Fatalf("writing the clone changed the original: %+v", orig)
	}
}

// A MEMBER IS ON PROBATION EXACTLY WHILE THE STATE REMEMBERS IT AS REMOVED,
// and a state saying otherwise is refused for a writer: a probation nothing
// counts would never end, leaving the member placed on nothing for ever, and
// a remembered removal beside a placeable member would still be counting a
// probation the map had already given up.
func TestProbationIsExactlyWhatTheStateRemembers(t *testing.T) {
	t.Parallel()
	if s, members := full(); s.Validate(members) != nil {
		t.Fatalf("a consistent state is refused: %v", s.Validate(members))
	}
	for name, mutate := range map[string]func(*State, []placement.Member){
		"a probation nothing remembers": func(s *State, _ []placement.Member) { delete(s.Removed, "data-c") },
		"a remembered member not on probation": func(_ *State, m []placement.Member) {
			m[2].Probation = false
		},
		"a member remembered as removed": func(s *State, _ []placement.Member) {
			s.Removed["data-a"] = Removal{Gone: 1, At: stamp, Reason: ReasonAbsent}
		},
	} {
		s, members := full()
		mutate(&s, members)
		if err := s.Validate(members); !errors.Is(err, placement.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

// A HOLD IS ACTIVE UNTIL IT ENDS, and none at all when there is none.
func TestAHoldIsActiveUntilItEnds(t *testing.T) {
	t.Parallel()
	var none *Hold
	h := &Hold{Until: stamp}
	switch {
	case none.Active(stamp.Add(-time.Hour)):
		t.Error("no hold is active")
	case !h.Active(stamp.Add(-time.Nanosecond)):
		t.Error("a hold is not active before it ends")
	case h.Active(stamp):
		t.Error("a hold is still active when it ends")
	}
}

func TestAbsenceReasons(t *testing.T) {
	t.Parallel()
	for _, r := range []AbsenceReason{ReasonAbsent, ReasonUnhealthy} {
		if !r.Valid() {
			t.Errorf("%q is not valid", r)
		}
	}
	for _, r := range []AbsenceReason{"", "gone", "unhealthy: disk"} {
		if r.Valid() {
			t.Errorf("%q is valid", r)
		}
	}
}
