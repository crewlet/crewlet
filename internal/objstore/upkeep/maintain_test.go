package upkeep

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func members(pairs ...any) []placement.Member {
	var out []placement.Member
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, placement.Member{Node: pairs[i].(string), Weight: pairs[i+1].(int)})
	}
	return out
}

func stateOf(epoch uint64, replicas int, ms []placement.Member) objstore.MapState {
	return objstore.MapState{Map: placement.Map{Epoch: epoch, Replicas: replicas, Members: ms}}
}

// THE MAP'S RULES, one case each: who is added, who is taken out and when,
// and which changes move the epoch.
func TestTheNextMap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		state       objstore.MapState
		live        []Presence
		replicas    int
		now         time.Time
		wantMembers []placement.Member
		wantEpoch   uint64
		wantAbsent  []string
		wantChanged bool
	}{
		{
			name: "the first data node makes the first map",
			live: []Presence{{"data-a", 1}}, replicas: 3, now: t0,
			wantMembers: members("data-a", 1), wantEpoch: 1, wantChanged: true,
		},
		{
			name:  "a node that joins is added",
			state: stateOf(1, 3, members("data-a", 1)),
			live:  []Presence{{"data-a", 1}, {"data-b", 2}}, replicas: 3, now: t0,
			wantMembers: members("data-a", 1, "data-b", 2), wantEpoch: 2, wantChanged: true,
		},
		{
			name:  "a changed weight is taken",
			state: stateOf(4, 3, members("data-a", 1)),
			live:  []Presence{{"data-a", 5}}, replicas: 3, now: t0,
			wantMembers: members("data-a", 5), wantEpoch: 5, wantChanged: true,
		},
		{
			name:  "a steady fleet writes nothing",
			state: stateOf(4, 3, members("data-a", 1, "data-b", 1)),
			live:  []Presence{{"data-b", 1}, {"data-a", 1}}, replicas: 3, now: t0,
			wantMembers: members("data-a", 1, "data-b", 1), wantEpoch: 4, wantChanged: false,
		},
		{
			name:  "a member that goes quiet is noted, and still placed on",
			state: stateOf(4, 3, members("data-a", 1, "data-b", 1)),
			live:  []Presence{{"data-a", 1}}, replicas: 3, now: t0,
			wantMembers: members("data-a", 1, "data-b", 1), wantEpoch: 4,
			wantAbsent: []string{"data-b"}, wantChanged: true,
		},
		{
			name: "a member quiet for less than the grace stays",
			state: objstore.MapState{Map: placement.Map{Epoch: 4, Replicas: 3,
				Members: members("data-a", 1, "data-b", 1)},
				Absent: map[string]time.Time{"data-b": t0}},
			live: []Presence{{"data-a", 1}}, replicas: 3, now: t0.Add(OutGrace - time.Second),
			wantMembers: members("data-a", 1, "data-b", 1), wantEpoch: 4,
			wantAbsent: []string{"data-b"}, wantChanged: false,
		},
		{
			name: "a member quiet for the whole grace is taken out",
			state: objstore.MapState{Map: placement.Map{Epoch: 4, Replicas: 3,
				Members: members("data-a", 1, "data-b", 1)},
				Absent: map[string]time.Time{"data-b": t0}},
			live: []Presence{{"data-a", 1}}, replicas: 3, now: t0.Add(OutGrace),
			wantMembers: members("data-a", 1), wantEpoch: 5, wantChanged: true,
		},
		{
			name: "a member back before the grace is no longer absent",
			state: objstore.MapState{Map: placement.Map{Epoch: 4, Replicas: 3,
				Members: members("data-a", 1, "data-b", 1)},
				Absent: map[string]time.Time{"data-b": t0}},
			live: []Presence{{"data-a", 1}, {"data-b", 1}}, replicas: 3, now: t0.Add(time.Minute),
			wantMembers: members("data-a", 1, "data-b", 1), wantEpoch: 4, wantChanged: true,
		},
		{
			name:  "a changed replica count is a placement change",
			state: stateOf(4, 3, members("data-a", 1)),
			live:  []Presence{{"data-a", 1}}, replicas: 1, now: t0,
			wantMembers: members("data-a", 1), wantEpoch: 5, wantChanged: true,
		},
		{
			name:  "a node offering no weight holds nothing",
			state: stateOf(4, 1, members("data-a", 1)),
			live:  []Presence{{"data-a", 1}, {"agent-1", 0}}, replicas: 1, now: t0,
			wantMembers: members("data-a", 1), wantEpoch: 4, wantChanged: false,
		},
		{
			name:  "a weight past the ceiling is held to it",
			state: stateOf(4, 1, nil),
			live:  []Presence{{"data-a", placement.MaxWeight * 2}}, replicas: 1, now: t0,
			wantMembers: members("data-a", placement.MaxWeight), wantEpoch: 5, wantChanged: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			next, changed := Next(c.state, c.live, c.replicas, c.now)
			if changed != c.wantChanged {
				t.Errorf("changed = %v, want %v", changed, c.wantChanged)
			}
			if next.Map.Epoch != c.wantEpoch {
				t.Errorf("epoch = %d, want %d", next.Map.Epoch, c.wantEpoch)
			}
			if len(next.Map.Members) != len(c.wantMembers) {
				t.Fatalf("members = %+v, want %+v", next.Map.Members, c.wantMembers)
			}
			for i := range c.wantMembers {
				if next.Map.Members[i] != c.wantMembers[i] {
					t.Fatalf("members = %+v, want %+v", next.Map.Members, c.wantMembers)
				}
			}
			if len(next.Absent) != len(c.wantAbsent) {
				t.Fatalf("absent = %v, want %v", next.Absent, c.wantAbsent)
			}
			for _, node := range c.wantAbsent {
				if _, ok := next.Absent[node]; !ok {
					t.Fatalf("absent = %v, want %v", next.Absent, c.wantAbsent)
				}
			}
			if err := next.Map.Validate(); err != nil && len(next.Map.Members) > 0 {
				t.Fatalf("the next map does not validate: %v", err)
			}
		})
	}
}

// THE ABSENCE OUTLIVES THE DUTY HOLDER: a grace started by one maintainer is
// finished by the next, because it is written into the stored record rather
// than held in anybody's memory.
func TestAnAbsenceStartedByOneHolderIsFinishedByTheNext(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	live := []Presence{{"data-a", 1}, {"data-b", 1}}
	now := t0
	holder := func() *Maintainer {
		m, err := NewMaintainer(MaintainerOptions{Store: store, Replicas: 2,
			Live: func(context.Context) ([]Presence, error) { return live, nil },
			Now:  func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := holder().Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	live = live[:1]
	if err := holder().Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(OutGrace)
	if err := holder().Tick(t.Context()); err != nil { // a third holder
		t.Fatal(err)
	}
	rec, _, err := store.ObjectMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		t.Fatal(err)
	}
	if state.Map.Holds("data-b") || !state.Map.Holds("data-a") {
		t.Fatalf("after the grace the map holds %+v, want data-a alone", state.Map.Members)
	}
}

// A FLEET WITH NO DATA NODE WRITES NO MAP — an empty one says what none says,
// at a write per tick.
func TestAFleetWithNoMembersWritesNoMap(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	m, err := NewMaintainer(MaintainerOptions{Store: store, Replicas: 3,
		Live: func(context.Context) ([]Presence, error) { return []Presence{{"agent-1", 0}}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ObjectMap(t.Context()); found {
		t.Fatal("a map was written for a fleet with no member")
	}
}

// A MAP THIS BUILD CANNOT READ IS NEVER OVERWRITTEN: a newer build wrote it,
// and rewriting it in this build's shape would drop what that build added.
func TestAMapThisBuildCannotReadIsNotOverwritten(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	unreadable := []byte(`{"map":{"epoch":9,"replicas":0}}`)
	if _, _, err := store.CreateObjectMap(t.Context(), unreadable); err != nil {
		t.Fatal(err)
	}
	m, err := NewMaintainer(MaintainerOptions{Store: store, Replicas: 1,
		Live: func(context.Context) ([]Presence, error) { return []Presence{{"data-a", 1}}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(t.Context()); !errors.Is(err, errNewerMap) {
		t.Fatalf("Tick over an unreadable map = %v, want errNewerMap", err)
	}
	rec, _, _ := store.ObjectMap(t.Context())
	if string(rec.Value) != string(unreadable) {
		t.Fatalf("the unreadable map was overwritten with %s", rec.Value)
	}
}

// A LOST RACE IS NOT AN ERROR AND WRITES NOTHING: the next tick starts from
// what the winner wrote.
func TestALostRaceWritesNothing(t *testing.T) {
	t.Parallel()
	store := &racing{Fleet: memory.NewFleet()}
	observed := &observer{}
	m, err := NewMaintainer(MaintainerOptions{Store: store, Replicas: 1, Observer: observed,
		Live: func(context.Context) ([]Presence, error) { return []Presence{{"data-a", 1}}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatalf("a lost race = %v, want nil", err)
	}
	if observed.n != 0 {
		t.Fatal("a map this holder lost the race to write was installed as though written")
	}
}

type racing struct{ *memory.Fleet }

func (r *racing) CreateObjectMap(context.Context, []byte) (coord.ObjectMapRecord, bool, error) {
	return coord.ObjectMapRecord{}, false, nil
}

type observer struct{ n int }

func (o *observer) Observe(objstore.MapState, uint64) { o.n++ }
