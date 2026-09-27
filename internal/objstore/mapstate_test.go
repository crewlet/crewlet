package objstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/objstore/placement"
)

var stamp = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// full is a record with every field set, so a round trip that dropped one
// would show.
func full() MapState {
	return MapState{
		Map: placement.Map{
			Generation: uuid.MustParse("7b0e0a52-3c43-4d9f-9d6f-2f3d1c1b8e11"),
			Epoch:      7, Replicas: 3, PGBits: 9, FailureDomain: "zone",
			Members: []placement.Member{
				{Node: "data-a", Weight: 2, Share: 2 << 16, Domain: "eu-1"},
				{Node: "data-b", Weight: 1, Share: 1 << 16, Domain: "eu-2", Out: true},
				{Node: "data-c", Weight: 1, Share: 1 << 16, Domain: "eu-3", Probation: true},
			},
		},
		Absence: map[string]Absence{"data-a": {Ticks: 3, Present: 1, Since: stamp,
			Reason: ReasonUnhealthy, Detail: "fsync failed"}},
		Removed: map[string]Removal{"data-c": {Present: 2, At: stamp, Reason: ReasonAbsent}},
		Config:  ConfigSource{Epoch: 1_790_000_000_000},
		Hold:    &Hold{Until: stamp.Add(time.Hour), By: "ops", Reason: "upgrade", At: stamp},
		TakenOut: map[string]Gesture{"data-b": {By: "ops", Reason: "decommission",
			At: stamp}},
		Balance: Balance{Epoch: 7, BalanceReport: placement.BalanceReport{Rounds: 4,
			Deviation: 0.0125, Converged: true}},
	}
}

// WHAT THIS BUILD WRITES, THIS BUILD CAN UPDATE — every field, through the
// strict decoder a writer uses. A field the encoder emitted and the strict
// decoder did not know would wedge the maintainer on its own map.
func TestAStoredMapRoundTrips(t *testing.T) {
	t.Parallel()
	raw, err := full().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, decode := range map[string]func([]byte) (MapState, error){
		"to place by": DecodeMapState, "to update": DecodeMapStateForUpdate,
	} {
		got, err := decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, full()) {
			t.Fatalf("%s: round trip gave\n%+v\nwant\n%+v", name, got, full())
		}
	}
}

// A FIELD THIS BUILD DOES NOT KNOW is read past by a node placing by the map,
// and refused by one that would write it back without it: a newer build added
// it, and a rolling upgrade must neither stop placement nor lose what the
// newer build recorded.
func TestAFieldFromANewerBuildIsReadButNotRewritten(t *testing.T) {
	t.Parallel()
	raw, err := full().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, newer := range map[string][]byte{
		"at the top":    append([]byte(`{"from_a_newer_build":1,`), raw[1:]...),
		"in the map":    bytes.Replace(raw, []byte(`"map":{`), []byte(`"map":{"shards":2,`), 1),
		"in an absence": bytes.Replace(raw, []byte(`"ticks":3`), []byte(`"ticks":3,"why":"x"`), 1),
	} {
		if _, err := DecodeMapState(newer); err != nil {
			t.Errorf("%s: a reader refused it: %v", name, err)
		}
		if _, err := DecodeMapStateForUpdate(newer); err == nil {
			t.Errorf("%s: a writer accepted it", name)
		}
	}
	if _, err := DecodeMapStateForUpdate(append(raw, []byte(` {}`)...)); err == nil {
		t.Error("a writer accepted a record with data after it")
	}
	for name, decode := range map[string]func([]byte) (MapState, error){
		"to place by": DecodeMapState, "to update": DecodeMapStateForUpdate,
	} {
		if _, err := decode([]byte(`{"map":{"epoch":9,"replicas":0}}`)); err == nil {
			t.Errorf("%s: a map that cannot place was accepted", name)
		}
	}
}

// A CLONE SHARES NOTHING: the maintainer builds the next record from a clone
// of one its node may still be placing by.
func TestACloneSharesNothing(t *testing.T) {
	t.Parallel()
	orig := full()
	c := orig.Clone()
	c.Map.Members[0].Weight = 9
	c.Absence["data-a"] = Absence{}
	c.Removed["data-z"] = Removal{}
	c.TakenOut["data-a"] = Gesture{}
	c.Hold.Reason = "changed"
	if !reflect.DeepEqual(orig, full()) {
		t.Fatalf("writing the clone changed the original: %+v", orig)
	}
}

// A MEMBER IS ON PROBATION EXACTLY WHILE THE RECORD REMEMBERS IT AS REMOVED,
// and a record saying otherwise is refused at the write: a probation nothing
// counts would never end, leaving the member placed on nothing for ever, and
// a remembered removal beside a placeable member would still be counting a
// probation the map had already given up. A READER is not refused either
// way — it places by the map, which places correctly whatever the record
// remembers.
func TestProbationIsExactlyWhatTheRecordRemembers(t *testing.T) {
	t.Parallel()
	if err := full().Validate(); err != nil {
		t.Fatalf("a consistent record is refused: %v", err)
	}
	for name, mutate := range map[string]func(*MapState){
		"a probation nothing remembers": func(s *MapState) { delete(s.Removed, "data-c") },
		"a remembered member not on probation": func(s *MapState) {
			s.Map.Members[2].Probation = false
		},
		"a member remembered as removed": func(s *MapState) {
			s.Removed["data-a"] = Removal{Gone: 1, At: stamp, Reason: ReasonAbsent}
		},
	} {
		s := full()
		mutate(&s)
		if err := s.Validate(); !errors.Is(err, placement.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
		if _, err := s.Encode(); err == nil {
			t.Errorf("%s: the record was stored", name)
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeMapState(raw); err != nil {
			t.Errorf("%s: a reader refused it: %v", name, err)
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
