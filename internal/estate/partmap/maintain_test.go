package partmap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// layoutZero is the single-file layout as the engine describes it: one space,
// one partition, every domain.
var layoutZero = statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
	{Space: statelog.SpaceEstate, Partitions: 1, Domains: []string{"tracker", "pages", "vectors"}},
}}

// layoutZeroLease is a data node's estate lease under layout 0: it holds the
// one partition whole, and serves it.
func layoutZeroLease(node string) Presence {
	return Presence{Node: node, Meta: Meta{Weight: 1, Layout: layoutNo(0), Healthy: yes(),
		Partitions: map[string]PartitionState{"estate.000": PartServing}}}
}

// maintainerOver is a maintainer over a store, with these leases and this
// company, at this layout, provisioning with provision.
func maintainerOver(t *testing.T, store MapStore, layout statelog.Layout,
	live func() ([]Presence, error), co membership.Company, provision Provisioner) *Maintainer {

	t.Helper()
	m, err := NewMaintainer(MaintainerOptions{
		Store:     store,
		Live:      func(context.Context) ([]Presence, error) { return live() },
		Company:   func() (membership.Company, bool) { return co, co != (membership.Company{}) },
		Layout:    layout,
		Provision: provision,
		Now:       func() time.Time { return base },
	})
	if err != nil {
		t.Fatalf("NewMaintainer: %v", err)
	}
	return m
}

// UNDER LAYOUT 0 THE MAINTAINER WRITES NOTHING. Every data node holds the one
// partition whole, so there is no map to maintain and none may be created —
// whatever the leases and the company say, and however many ticks run. This
// is the whole of what the estate map's duty does on a fleet this build runs:
// it reads, and it leaves the store as it found it.
func TestUnderLayoutZeroTheMaintainerWritesNothing(t *testing.T) {
	t.Parallel()
	store := coordmemory.NewFleet()
	live := []Presence{layoutZeroLease("a"), layoutZeroLease("b"), layoutZeroLease("c")}
	provisioned := 0
	m := maintainerOver(t, store, layoutZero,
		func() ([]Presence, error) { return live, nil }, company(3, "zone", 7),
		func(context.Context, statelog.Layout) error { provisioned++; return nil })
	for range 3 * membership.OutTicks {
		res, err := m.Tick(t.Context())
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if res.Mapped || res.Awaited {
			t.Fatalf("a tick at layout 0 reported %+v: there is no map, and none is wanted", res)
		}
	}
	if _, found, err := store.EstateMap(t.Context()); err != nil || found {
		t.Fatalf("the store holds an estate map after layout-0 ticks (found %v, %v)", found, err)
	}
	if provisioned != 0 {
		t.Errorf("a layout-0 maintainer provisioned %d times", provisioned)
	}
}

// A FIRST MAP IS WRITTEN ONLY AFTER ITS LAYOUT'S LOGS ARE CREATED. A node the
// map names opens its partition's logs to join it, so the logs come first —
// and a provisioner that failed leaves no map behind that names holders of logs
// nobody created. A tick that finds no map at a partitioned layout says one is
// awaited, so its duty ticks again soon.
func TestAFirstMapFollowsItsLayoutsLogs(t *testing.T) {
	t.Parallel()
	store := coordmemory.NewFleet()
	live := []Presence{}
	for _, node := range []string{"a", "b", "c"} {
		live = append(live, Presence{Node: node, Meta: Meta{Weight: 1,
			Layout: layoutNo(smallLayout.Number), Healthy: yes(),
			Partitions: map[string]PartitionState{}}})
	}
	failing := true
	var provisioned []int
	m := maintainerOver(t, store, smallLayout,
		func() ([]Presence, error) { return live, nil }, company(3, "", 1),
		func(_ context.Context, l statelog.Layout) error {
			provisioned = append(provisioned, l.Number)
			if failing {
				return errors.New("the broker refused a stream")
			}
			return nil
		})

	res, err := m.Tick(t.Context())
	if err == nil || !strings.Contains(err.Error(), "first map") {
		t.Fatalf("a failed provisioning answered %v, want an error naming the first map", err)
	}
	if !res.Awaited || res.Mapped {
		t.Errorf("the tick reported %+v, want a map awaited", res)
	}
	if _, found, _ := store.EstateMap(t.Context()); found {
		t.Fatal("a map was written although its layout's logs were not created")
	}

	failing = false
	if _, err := m.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rec, found, err := store.EstateMap(t.Context())
	if err != nil || !found {
		t.Fatalf("no first map once the logs exist (%v, %v)", found, err)
	}
	state, err := DecodeMapState(rec.Value)
	if err != nil {
		t.Fatal(err)
	}
	if state.Map.Layout.Number != smallLayout.Number || len(state.Map.Members) != 3 {
		t.Errorf("the first map is layout %d with %d members", state.Map.Layout.Number,
			len(state.Map.Members))
	}
	if len(provisioned) != 2 || provisioned[1] != smallLayout.Number {
		t.Errorf("provisioned %v, want layout %d twice", provisioned, smallLayout.Number)
	}
	res, err = m.Tick(t.Context())
	if err != nil || !res.Mapped || res.Awaited {
		t.Fatalf("the tick after the first map = %+v, %v", res, err)
	}
	if len(provisioned) != 2 {
		t.Errorf("a tick that found a map provisioned again: %v", provisioned)
	}
}

// A MAINTAINER THAT MAY WRITE A FIRST MAP MUST BE ABLE TO CREATE ITS LOGS. At a
// partitioned layout one without a provisioner is refused when it is built,
// rather than writing a map whose holders find no logs to join.
func TestAPartitionedMaintainerNeedsAProvisioner(t *testing.T) {
	t.Parallel()
	_, err := NewMaintainer(MaintainerOptions{
		Store:   coordmemory.NewFleet(),
		Live:    func(context.Context) ([]Presence, error) { return nil, nil },
		Company: func() (membership.Company, bool) { return membership.Company{}, false },
		Layout:  smallLayout,
	})
	if err == nil || !strings.Contains(err.Error(), "provisioner") {
		t.Fatalf("a partitioned maintainer with no provisioner was built: %v", err)
	}
	if _, err := NewMaintainer(MaintainerOptions{
		Store:   coordmemory.NewFleet(),
		Live:    func(context.Context) ([]Presence, error) { return nil, nil },
		Company: func() (membership.Company, bool) { return membership.Company{}, false },
		Layout:  layoutZero,
	}); err != nil {
		t.Fatalf("a layout-0 maintainer, which writes no first map, was refused: %v", err)
	}
}

// losingStore answers every compare-and-set as lost, having let another writer
// in first.
type losingStore struct {
	MapStore
	lost int
}

func (s *losingStore) UpdateEstateMap(context.Context, []byte, uint64) (coord.EstateMapRecord, bool, error) {
	s.lost++
	return coord.EstateMapRecord{}, false, nil
}

// AN INPUT THE TICK COULD NOT READ CHANGES NOTHING, AND A LOST RACE WRITES
// NOTHING. A lease listing that failed, a map a newer build wrote and a write
// another holder won each leave the stored map exactly as it was: nothing is
// released or removed on an unknown, and a tick's counts are lost with its race
// rather than written over its successor's.
func TestATickChangesNothingOnAnUnknownOrALostRace(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 3, "a", "b", "c")
	s.tick()
	store := coordmemory.NewFleet()
	raw, err := s.state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.CreateEstateMap(t.Context(), raw); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	unchanged := func(what string) {
		t.Helper()
		rec, _, err := store.EstateMap(t.Context())
		if err != nil || string(rec.Value) != string(raw) {
			t.Fatalf("%s changed the stored map", what)
		}
	}
	// Two nodes gone: a tick that could read them would open two absences.
	gone := []Presence{s.live()[0]}

	unread := maintainerOver(t, store, smallLayout,
		func() ([]Presence, error) { return nil, errors.New("coordination timed out") },
		company(3, "", 1), noProvision)
	if _, err := unread.Tick(t.Context()); err == nil {
		t.Error("a tick whose lease listing failed answered no error")
	}
	unchanged("a tick that could not list the leases")

	lost := &losingStore{MapStore: store}
	racing := maintainerOver(t, lost, smallLayout,
		func() ([]Presence, error) { return gone, nil }, company(3, "", 1), noProvision)
	res, err := racing.Tick(t.Context())
	if err != nil || !res.Mapped || lost.lost != 1 {
		t.Fatalf("a lost race = (%+v, %v) after %d writes", res, err, lost.lost)
	}
	unchanged("a tick that lost its race")

	newer := coordmemory.NewFleet()
	withField := strings.Replace(string(raw), `{"map":`, `{"from_a_newer_build":1,"map":`, 1)
	if _, ok, err := newer.CreateEstateMap(t.Context(), []byte(withField)); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	future := maintainerOver(t, newer, smallLayout,
		func() ([]Presence, error) { return gone, nil }, company(3, "", 1), noProvision)
	if _, err := future.Tick(t.Context()); !errors.Is(err, errNewerMap) {
		t.Errorf("a map a newer build wrote answered %v, want errNewerMap", err)
	}
	if rec, _, _ := newer.EstateMap(t.Context()); string(rec.Value) != withField {
		t.Error("a map a newer build wrote was rewritten in this build's shape")
	}

	// And the control: the same tick over a store that answers moves the
	// map, so the three above changed nothing because of what they read.
	moving := maintainerOver(t, store, smallLayout,
		func() ([]Presence, error) { return gone, nil }, company(3, "", 1), noProvision)
	if _, err := moving.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rec, _, _ := store.EstateMap(t.Context()); string(rec.Value) == string(raw) {
		t.Fatal("the control tick changed nothing either: the cases above prove nothing")
	}
}

// noProvision is a provisioner for a maintainer over a map that already exists.
func noProvision(context.Context, statelog.Layout) error { return nil }

// WHAT A WRITTEN TICK DID TO THE HOLDER TABLE IS COUNTED, one line rather than a
// line per holder, since a first map names hundreds of joins.
func TestAWrittenTickCountsWhatItDidToTheHolders(t *testing.T) {
	t.Parallel()
	before := Map{Partitions: []Partition{
		{ID: "tracker.000", Holders: []Holder{{Node: "a", State: Joining}, {Node: "b", State: Serving}}},
		{ID: "tracker.001", Holders: []Holder{{Node: "a", State: Serving}, {Node: "c", State: Leaving}}},
		{ID: "tracker.002", Holders: []Holder{{Node: "b", State: Leaving}, {Node: "c", State: Joining}}},
	}}
	after := Map{Partitions: []Partition{
		{ID: "tracker.000", Holders: []Holder{{Node: "a", State: Serving}, {Node: "b", State: Leaving},
			{Node: "c", State: Joining}}},
		{ID: "tracker.001", Holders: []Holder{{Node: "a", State: Serving}, {Node: "d", State: Serving}}},
		{ID: "tracker.002", Holders: []Holder{{Node: "b", State: Serving}, {Node: "c", State: Leaving}}},
	}}
	got := diffHolders(before, after)
	want := holderChanges{joined: 1, adopted: 1, promoted: 1, retired: 1, withdrawn: 1,
		restored: 1, removed: 1}
	if got != want {
		t.Fatalf("counted %+v, want %+v", got, want)
	}
	if first := diffHolders(Map{}, Map{Partitions: []Partition{{ID: "tracker.000"}}}); first.unserved != 1 {
		t.Errorf("a partition nobody serves counted %+v", first)
	}
}
