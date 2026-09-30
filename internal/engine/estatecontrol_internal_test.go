package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// testLayoutOne is a partitioned layout small enough to reason about: four
// tracker partitions and the company's one.
var testLayoutOne = statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
	{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{"tracker", "vectors"}},
	{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
}}

// storedEstateMap writes a first estate map over the named data nodes at three
// copies, as the maintainer's first tick would, and answers the store holding
// it.
func storedEstateMap(t *testing.T, nodes ...string) *coordmemory.Fleet {
	t.Helper()
	layout, healthy := testLayoutOne.Number, true
	live := make([]partmap.Presence, 0, len(nodes))
	for _, n := range nodes {
		live = append(live, partmap.Presence{Node: n, Meta: partmap.Meta{Weight: 1,
			Layout: &layout, Healthy: &healthy, Partitions: map[string]partmap.PartitionState{}}})
	}
	state, changed := partmap.Next(partmap.MapState{}, partmap.Input{
		Layout: testLayoutOne, Live: live, Now: gestureNow,
		Company: membership.Company{Epoch: 1, Replicas: 3, Block: "estate"},
	})
	if !changed {
		t.Fatal("the fixture wrote no first map")
	}
	raw, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	fleet := coordmemory.NewFleet()
	if _, won, err := fleet.CreateEstateMap(t.Context(), raw); err != nil || !won {
		t.Fatalf("create the map: %v", err)
	}
	return fleet
}

func estateControlOver(store estateMapStore) *EstateControl {
	return &EstateControl{store: store, running: LayoutZero(), now: func() time.Time { return gestureNow }}
}

// UNDER THE SINGLE-FILE LAYOUT THERE IS NO ESTATE MAP, and every gesture says
// so by name: the state reads as none, and an out, an in, a hold, a release, a
// move and a cancel are each refused with ErrNoMap, saying the estate is held
// whole by every data node — never an empty map answered as though it were one.
func TestUnderLayoutZeroEveryEstateGestureSaysThereIsNoMap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := estateControlOver(coordmemory.NewFleet())
	if _, _, found, err := c.State(ctx); err != nil || found {
		t.Fatalf("State = (found %v, %v), want no map", found, err)
	}
	p := statelog.PartitionID{Space: statelog.SpaceEstate}
	for name, gesture := range map[string]func() (EstateGesture, error){
		"out":     func() (EstateGesture, error) { return c.Out(ctx, "a", "ops", "") },
		"in":      func() (EstateGesture, error) { return c.In(ctx, "a", "ops") },
		"hold":    func() (EstateGesture, error) { return c.Hold(ctx, uuid.NewString(), time.Hour, "ops", "") },
		"release": func() (EstateGesture, error) { return c.Release(ctx, uuid.NewString(), "ops") },
		// WITH NO GENERATION TO REPEAT, which is all an operator at layout
		// 0 has: the refusal is still the fleet's own.
		"unconfirmed hold":    func() (EstateGesture, error) { return c.Hold(ctx, "", time.Hour, "ops", "") },
		"unconfirmed release": func() (EstateGesture, error) { return c.Release(ctx, "whole", "ops") },
		"move":                func() (EstateGesture, error) { return c.Move(ctx, p, "a", "ops", "") },
		"cancel":              func() (EstateGesture, error) { return c.CancelMove(ctx, p, "a", "ops") },
	} {
		got, err := gesture()
		if !errors.Is(err, partmap.ErrNoMap) || got.Landed {
			t.Errorf("%s = (landed %v, %v), want ErrNoMap", name, got.Landed, err)
			continue
		}
		// AND BY THE SENTINEL a surface branches on, which the no-map
		// refusal of a partitioned layout is not: the one is a fact
		// about the fleet, the other a wait.
		if !errors.Is(err, ErrEstateWhole) {
			t.Errorf("%s's refusal = %v, want ErrEstateWhole", name, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "layout 0") || !strings.Contains(msg, "whole") {
			t.Errorf("%s's refusal does not say why there is no map: %v", name, err)
		}
		// NOTHING IN IT READS AS A WAIT: layout 0 never has a map, and a
		// "yet" anywhere in the sentence tells an operator one is coming.
		if strings.Contains(err.Error(), "yet") {
			t.Errorf("%s's refusal reads as a map still to come: %v", name, err)
		}
	}
}

// AT A PARTITIONED LAYOUT A MISSING MAP IS A MAP NOT WRITTEN YET, and the
// refusal says so — never that every data node holds the whole estate, which
// is layout 0's absence and would send an operator the opposite way.
func TestAtAPartitionedLayoutAGestureSaysNoMapIsWrittenYet(t *testing.T) {
	t.Parallel()
	c := estateControlOver(coordmemory.NewFleet())
	c.running = DefaultLayoutOne()
	_, err := c.Out(t.Context(), "a", "ops", "")
	if !errors.Is(err, partmap.ErrNoMap) {
		t.Fatalf("Out = %v, want ErrNoMap", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "layout 1") || !strings.Contains(msg, "written yet") ||
		strings.Contains(msg, "whole") {
		t.Errorf("a partitioned node's refusal reads %q", msg)
	}
	if errors.Is(err, ErrEstateWhole) {
		t.Errorf("a partitioned node's refusal is ErrEstateWhole: %v", err)
	}
}

// THE READ A SURFACE RENDERS IS THE RECORD AS STORED, a newer build's field and
// all: the surface decodes it as every node reading the map does, ignoring what
// it does not know, where a gesture — which REWRITES it — must refuse the same
// record rather than drop the field.
func TestTheEstateMapIsReadAsStoredWhereAGestureRefusesIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := storedEstateMap(t, "a", "b", "c")
	rec, _, err := store.EstateMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newer := append(bytes.TrimSuffix(rec.Value, []byte("}")), []byte(`,"from_a_newer_build":1}`)...)
	if _, ok, err := store.UpdateEstateMap(ctx, newer, rec.Version); err != nil || !ok {
		t.Fatalf("setup: store the newer map: (%v, %v)", ok, err)
	}
	c := estateControlOver(store)
	if _, _, _, err := c.State(ctx); !errors.Is(err, ErrEstateNewerMap) {
		t.Fatalf("State of a newer build's map = %v, want ErrEstateNewerMap", err)
	}
	got, found, err := c.EstateMap(ctx)
	if err != nil || !found || !bytes.Equal(got.Value, newer) {
		t.Fatalf("EstateMap = (%s, %v, %v), want the record as stored", got.Value, found, err)
	}
	if c.Running().Number != 0 {
		t.Errorf("Running = layout %d, want the layout this node runs, 0", c.Running().Number)
	}
}

// A GESTURE THAT LANDS IS IN THE STORED MAP, and one sent again writes nothing:
// the version does not move under a maintainer's tick, and the first operator's
// who and why stand. A move is recorded on its partition and cancelled the same
// way; one with no member to rebuild the copy on is refused by name.
func TestAnEstateGestureLandsInTheStoredMap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := storedEstateMap(t, "a", "b", "c", "d")
	c := estateControlOver(store)

	out, err := c.Out(ctx, "b", "ops@example.com", "replacing its disk")
	if err != nil || !out.Landed {
		t.Fatalf("Out = (%v, %v), want it landed", out.Landed, err)
	}
	state, version, found, err := c.State(ctx)
	if err != nil || !found {
		t.Fatalf("State = (%v, %v)", found, err)
	}
	if g := state.TakenOut["b"]; g.By != "ops@example.com" || g.Reason != "replacing its disk" {
		t.Errorf("the gesture is recorded as %+v", g)
	}
	again, err := c.Out(ctx, "b", "someone-else", "a second reason")
	if err != nil || !again.Landed || again.Version != version {
		t.Fatalf("a resent out = (landed %v, version %d, %v), want landed at %d with nothing written",
			again.Landed, again.Version, err, version)
	}
	if g := again.State.TakenOut["b"]; g.By != "ops@example.com" {
		t.Errorf("a resent out rewrote who took the node out: %+v", g)
	}
	if _, err := c.In(ctx, "b", "ops"); err != nil {
		t.Fatalf("In: %v", err)
	}

	// A BAR LANDS FOR A NODE THE MAP DOES NOT HOLD — where an out has
	// nothing to take out — and In lifts it.
	barred, err := c.Bar(ctx, "gone", "ops@example.com", "evicted")
	if err != nil || !barred.Landed {
		t.Fatalf("Bar of a node the map does not hold = (%v, %v), want it landed", barred.Landed, err)
	}
	if g := barred.State.Barred["gone"]; g.By != "ops@example.com" || g.Reason != "evicted" {
		t.Errorf("the bar is recorded as %+v", g)
	}
	lifted, err := c.In(ctx, "gone", "ops")
	if err != nil || !lifted.Landed || lifted.State.Barred != nil {
		t.Fatalf("In of a barred node = (%v, %v) leaving %v", lifted.Landed, err, lifted.State.Barred)
	}

	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	holder := state.Map.HoldersOf(p)[0].Node
	moved, err := c.Move(ctx, p, holder, "ops", "rebalancing by hand")
	if err != nil || !moved.Landed {
		t.Fatalf("Move = (%v, %v)", moved.Landed, err)
	}
	if _, ok := moved.State.Map.Moves[p.String()][holder]; !ok {
		t.Errorf("the move is not recorded: %+v", moved.State.Map.Moves)
	}
	cancelled, err := c.CancelMove(ctx, p, holder, "ops")
	if err != nil || !cancelled.Landed || len(cancelled.State.Map.Moves) != 0 {
		t.Fatalf("CancelMove = (%v, %v) leaving %v", cancelled.Landed, err, cancelled.State.Map.Moves)
	}

	tight := estateControlOver(storedEstateMap(t, "a", "b", "c"))
	tightState, _, _, _ := tight.State(ctx)
	if _, err := tight.Move(ctx, p, tightState.Map.HoldersOf(p)[0].Node, "ops", ""); !errors.Is(err, partmap.ErrNowhereToMove) {
		t.Errorf("a move with no member to rebuild on = %v, want ErrNowhereToMove", err)
	}
}

// A HOLD AND A RELEASE LAND ONLY ON THE MAP THEY WERE CONFIRMED FOR: confirmed
// for another generation — another fleet's map, or this one's before it was
// written again — each is refused by name and writes nothing, as is one
// confirmed by nothing that is a generation at all; and confirmed for the
// stored one each lands.
func TestAHoldAndAReleaseLandOnlyOnTheMapTheyWereConfirmedFor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := storedEstateMap(t, "a", "b", "c")
	c := estateControlOver(store)
	state, version, _, err := c.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.NewString()
	for name, want := range map[string]struct {
		gesture func() (EstateGesture, error)
		refusal error
	}{
		"hold for another map": {func() (EstateGesture, error) {
			return c.Hold(ctx, other, time.Hour, "ops", "")
		}, ErrEstateOtherMap},
		"release for another map": {func() (EstateGesture, error) {
			return c.Release(ctx, other, "ops")
		}, ErrEstateOtherMap},
		"hold confirmed by nothing": {func() (EstateGesture, error) {
			return c.Hold(ctx, "", time.Hour, "ops", "")
		}, ErrEstateUnconfirmed},
		"release confirmed by a word": {func() (EstateGesture, error) {
			return c.Release(ctx, "yes", "ops")
		}, ErrEstateUnconfirmed},
	} {
		if got, err := want.gesture(); !errors.Is(err, want.refusal) || got.Landed {
			t.Errorf("%s = (landed %v, %v), want %v", name, got.Landed, err, want.refusal)
		}
	}
	if _, after, _, _ := c.State(ctx); after != version {
		t.Fatalf("a refused gesture moved the map's version from %d to %d", version, after)
	}
	held, err := c.Hold(ctx, state.Map.Generation.String(), time.Hour, "ops", "rack work")
	if err != nil || !held.Landed || held.State.Hold == nil {
		t.Fatalf("a hold confirmed for the stored map = (%+v, %v), want it held", held, err)
	}
	released, err := c.Release(ctx, state.Map.Generation.String(), "ops")
	if err != nil || !released.Landed || released.State.Hold != nil {
		t.Fatalf("a release confirmed for the stored map = (%+v, %v), want no hold", released, err)
	}
}

// flakyEstate is an estate map store that fails, answers a raw record, or loses
// every compare-and-set.
type flakyEstate struct {
	readErr error
	raw     []byte
	losing  *coordmemory.Fleet
	writes  int
}

func (f *flakyEstate) EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error) {
	switch {
	case f.readErr != nil:
		return coord.EstateMapRecord{}, false, f.readErr
	case f.losing != nil:
		return f.losing.EstateMap(ctx)
	}
	return coord.EstateMapRecord{Value: f.raw, Version: 1}, true, nil
}

func (f *flakyEstate) UpdateEstateMap(context.Context, []byte, uint64) (coord.EstateMapRecord, bool, error) {
	f.writes++
	return coord.EstateMapRecord{}, false, nil
}

// EVERY OTHER FAILURE IS ONE A SURFACE CAN TELL APART: a store that did not
// answer, a map a newer build wrote — refused rather than rewritten in this
// build's shape — and a gesture that lost every race, which is no error but an
// answer that it did not land, with the map as it stands, after a bounded
// number of attempts.
func TestAnEstateGestureThatCannotLandSaysWhy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	down := &flakyEstate{readErr: errors.New("nats: timeout")}
	if _, err := estateControlOver(down).Out(ctx, "a", "ops", ""); !errors.Is(err, ErrEstateUnavailable) {
		t.Errorf("a store that did not answer = %v, want ErrEstateUnavailable", err)
	}
	// A MAP EVERY READER OF THIS BUILD ACCEPTS, with one field it does not
	// know: routable, and not this build's to rewrite.
	valid, _, err := storedEstateMap(t, "a", "b", "c", "d").EstateMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(string(valid.Value), `{"map":`, `{"from_a_newer_build":1,"map":`, 1)
	if _, err := partmap.DecodeMapState([]byte(raw)); err != nil {
		t.Fatalf("the premise: a reader takes a newer build's map: %v", err)
	}
	newer := &flakyEstate{raw: []byte(raw)}
	if _, err := estateControlOver(newer).Out(ctx, "a", "ops", ""); !errors.Is(err, ErrEstateNewerMap) {
		t.Errorf("a map this build cannot rewrite = %v, want ErrEstateNewerMap", err)
	}
	if newer.writes != 0 {
		t.Errorf("a map a newer build wrote was rewritten %d times", newer.writes)
	}
	losing := &flakyEstate{losing: storedEstateMap(t, "a", "b", "c", "d")}
	got, err := estateControlOver(losing).Out(ctx, "a", "ops", "")
	if err != nil || got.Landed {
		t.Fatalf("a gesture that lost every race = (landed %v, %v), want not landed", got.Landed, err)
	}
	if losing.writes != mapGestureAttempts {
		t.Errorf("it tried %d writes, want %d", losing.writes, mapGestureAttempts)
	}
	if got.State.Map.Generation.String() == "" || got.State.TakenOut["a"].By != "" {
		t.Errorf("it answered %+v rather than the map as it stands", got.State.TakenOut)
	}
}
