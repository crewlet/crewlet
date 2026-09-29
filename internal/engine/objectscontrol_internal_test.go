package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
)

var gestureNow = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// fixtureCopies is the copies [storedMap]'s company asks for: ONE, so taking
// out any member but the last leaves a member to rebuild its copies on. These
// tests are the control's — its compare-and-set, its retries, its answers — and
// the membership rule that refuses an out with nowhere to rebuild is
// internal/membership's, certified there; [storedMapAt] asks for more where a
// test is about that refusal reaching the control's caller.
const fixtureCopies = 1

// storedMap writes a first placement map over the named data nodes, as the
// maintainer's first tick would, and answers the store holding it.
func storedMap(t *testing.T, nodes ...string) *coordmemory.Fleet {
	t.Helper()
	return storedMapAt(t, fixtureCopies, nodes...)
}

// storedMapAt is [storedMap] for a company asking for copies copies.
func storedMapAt(t *testing.T, copies int, nodes ...string) *coordmemory.Fleet {
	t.Helper()
	live := make([]upkeep.Presence, 0, len(nodes))
	for _, n := range nodes {
		live = append(live, upkeep.Presence{Node: n, Weight: 1})
	}
	state, changed := upkeep.Next(objstore.MapState{}, live,
		membership.Company{Epoch: 1, Replicas: copies}, gestureNow)
	if !changed {
		t.Fatal("the fixture wrote no first map")
	}
	raw, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	fleet := coordmemory.NewFleet()
	if _, won, err := fleet.CreateObjectMap(t.Context(), raw); err != nil || !won {
		t.Fatalf("create the map: %v", err)
	}
	return fleet
}

// observed records every map a gesture handed its observer.
type observed struct{ states []objstore.MapState }

func (o *observed) Observe(s objstore.MapState, _ uint64) { o.states = append(o.states, s) }

func controlOver(store objectMapStore, obs objectMapObserver) *ObjectsControl {
	return &ObjectsControl{store: store, observer: obs, now: func() time.Time { return gestureNow }}
}

// A GESTURE THAT LANDS IS IN THE STORED MAP, and this node places by it at once
// rather than a refresh later.
func TestAnOutGestureLandsInTheStoredMap(t *testing.T) {
	t.Parallel()
	fleet := storedMap(t, "a", "b", "c")
	obs := &observed{}
	c := controlOver(fleet, obs)

	got, err := c.Out(t.Context(), "b", "ops@example.com", "replacing its disk")
	if err != nil || !got.Landed {
		t.Fatalf("Out = (%+v, %v), want it landed", got.Landed, err)
	}
	state, _, found, err := c.State(t.Context())
	if err != nil || !found {
		t.Fatalf("State = (%v, %v)", found, err)
	}
	if m, _ := state.Map.Member("b"); !m.Out {
		t.Error("the stored map does not have b out")
	}
	if g := state.TakenOut["b"]; g.By != "ops@example.com" || g.Reason != "replacing its disk" {
		t.Errorf("the gesture is recorded as %+v", g)
	}
	if len(obs.states) != 1 {
		t.Errorf("the observer was told %d maps, want the one written", len(obs.states))
	}

	// AND BACK IN, on the same terms.
	back, err := c.In(t.Context(), "b", "ops@example.com")
	if err != nil || !back.Landed {
		t.Fatalf("In = (%+v, %v)", back.Landed, err)
	}
	if m, _ := back.State.Map.Member("b"); m.Out {
		t.Error("b is still out after In")
	}
}

// EVERY REFUSAL IS ONE A SURFACE CAN TELL APART: no map yet, a node that is no
// member, the last member present, a member the copies cannot do without, a
// hold of no length or past a day, a store that would not answer, and a map a
// newer build wrote.
func TestAGestureRefusesByName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if _, err := controlOver(coordmemory.NewFleet(), nil).Out(ctx, "a", "ops", ""); !errors.Is(err, upkeep.ErrNoMap) {
		t.Errorf("a gesture with no map = %v, want ErrNoMap", err)
	}
	c := controlOver(storedMap(t, "a"), nil)
	if _, err := c.Out(ctx, "zz", "ops", ""); !errors.Is(err, membership.ErrUnknownMember) ||
		errors.Is(err, membership.ErrRemovedMember) {
		t.Errorf("taking out a stranger = %v, want ErrUnknownMember alone", err)
	}
	removed := controlOver(removedFrom(t, "c", "a", "b", "c"), nil)
	if _, err := removed.Out(ctx, "c", "ops", ""); !errors.Is(err, membership.ErrRemovedMember) {
		t.Errorf("taking out a node the map removed = %v, want ErrRemovedMember", err)
	}
	if _, err := c.Out(ctx, "a", "ops", ""); !errors.Is(err, membership.ErrNothingPlaceable) {
		t.Errorf("taking out the last member = %v, want ErrNothingPlaceable", err)
	}
	// THREE COPIES ON THREE MEMBERS: an out would drop one of each.
	tight := controlOver(storedMapAt(t, 3, "a", "b", "c"), nil)
	if _, err := tight.Out(ctx, "b", "ops", ""); !errors.Is(err, membership.ErrNowhereToRebuild) {
		t.Errorf("taking out a member the copies cannot do without = %v, want ErrNowhereToRebuild", err)
	}
	for _, d := range []time.Duration{0, membership.MaxHold + time.Minute} {
		if _, err := c.Hold(ctx, d, "ops", ""); !errors.Is(err, membership.ErrHoldRange) {
			t.Errorf("a %v hold = %v, want ErrHoldRange", d, err)
		}
	}
	down := &flakyMaps{readErr: errors.New("nats: timeout")}
	if _, err := controlOver(down, nil).Release(ctx, "ops"); !errors.Is(err, ErrObjectsUnavailable) {
		t.Errorf("a store that did not answer = %v, want ErrObjectsUnavailable", err)
	}
	newer := &flakyMaps{raw: []byte(`{"format":99}`)}
	if _, err := controlOver(newer, nil).Release(ctx, "ops"); !errors.Is(err, ErrObjectsNewerMap) {
		t.Errorf("a map this build cannot rewrite = %v, want ErrObjectsNewerMap", err)
	}
}

// flakyMaps is a map store that answers what it is told to: a read error, a
// fixed record, or a compare-and-set that always loses.
type flakyMaps struct {
	raw     []byte
	readErr error
	writes  int
	reads   int
}

func (f *flakyMaps) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	f.reads++
	if f.readErr != nil {
		return coord.ObjectMapRecord{}, false, f.readErr
	}
	return coord.ObjectMapRecord{Value: f.raw, Version: uint64(f.reads)}, true, nil
}

func (f *flakyMaps) UpdateObjectMap(context.Context, []byte, uint64) (coord.ObjectMapRecord, bool, error) {
	f.writes++
	return coord.ObjectMapRecord{}, false, nil
}

// A GESTURE THAT LOSES EVERY RACE SAYS SO rather than looping: bounded at
// three compare-and-sets, it answers not landed — with no error, because
// nothing failed — and the map as it now stands.
func TestAGestureThatLosesEveryRaceAnswersNotLanded(t *testing.T) {
	t.Parallel()
	fleet := storedMap(t, "a", "b")
	rec, _, err := fleet.ObjectMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	racing := &flakyMaps{raw: rec.Value}
	got, err := controlOver(racing, nil).Out(t.Context(), "b", "ops", "")
	if err != nil {
		t.Fatalf("a lost race answered an error: %v", err)
	}
	if got.Landed {
		t.Fatal("a gesture that never won a compare-and-set reports it landed")
	}
	// THREE, and the number is the invariant rather than the constant: one
	// retry covers a race with the maintainer's tick, a second covers that
	// and another operator at once, and past that the map is moving faster
	// than a gesture can be reasoned about.
	if racing.writes != 3 {
		t.Errorf("it tried %d compare-and-sets, want 3", racing.writes)
	}
	if got.State.Map.Generation != mustDecode(t, rec.Value).Map.Generation {
		t.Error("the answer is not the map as it stands")
	}
}

// A GESTURE THAT CHANGES NOTHING WRITES NOTHING: releasing a map with no hold
// has already landed, and a write of the same bytes would move the record's
// version for nothing — which a racing maintainer would lose its tick to.
func TestAGestureThatChangesNothingWritesNothing(t *testing.T) {
	t.Parallel()
	fleet := storedMap(t, "a", "b")
	rec, _, err := fleet.ObjectMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store := &flakyMaps{raw: rec.Value}
	got, err := controlOver(store, nil).Release(t.Context(), "ops")
	if err != nil || !got.Landed {
		t.Fatalf("Release = (%+v, %v), want landed", got.Landed, err)
	}
	if store.writes != 0 {
		t.Errorf("a gesture that changed nothing wrote %d time(s)", store.writes)
	}
}

// countingMaps is a real map store that counts the compare-and-sets it is
// asked for.
type countingMaps struct {
	objectMapStore
	writes int
}

func (c *countingMaps) UpdateObjectMap(ctx context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error) {
	c.writes++
	return c.objectMapStore.UpdateObjectMap(ctx, value, version)
}

// AN OUT SENT AGAIN WRITES NOTHING AND KEEPS THE FIRST OPERATOR'S RECORD, at
// the level an API request reaches. A lost answer tells an operator to send the
// gesture again, and a second out that wrote would move the record's version
// under a maintainer's tick for nothing and replace who took the member out,
// why and when with a retry's.
func TestAnOutSentAgainWritesNothing(t *testing.T) {
	t.Parallel()
	store := &countingMaps{objectMapStore: storedMap(t, "a", "b", "c")}
	obs := &observed{}
	c := controlOver(store, obs)
	first, err := c.Out(t.Context(), "b", "ops@example.com", "replacing its disk")
	if err != nil || !first.Landed || store.writes != 1 {
		t.Fatalf("the first out = (%v, %v) after %d writes", first.Landed, err, store.writes)
	}

	c.now = func() time.Time { return gestureNow.Add(time.Hour) }
	again, err := c.Out(t.Context(), "b", "someone-else@example.com", "a retry")
	if err != nil || !again.Landed {
		t.Fatalf("the out sent again = (%v, %v), want it landed", again.Landed, err)
	}
	if store.writes != 1 || len(obs.states) != 1 {
		t.Errorf("the out sent again wrote %d more time(s) and told the observer %d more",
			store.writes-1, len(obs.states)-1)
	}
	if again.Version != first.Version {
		t.Errorf("the record moved from version %d to %d", first.Version, again.Version)
	}
	want := membership.Gesture{By: "ops@example.com", Reason: "replacing its disk", At: gestureNow}
	if g := again.State.TakenOut["b"]; g != want {
		t.Errorf("the out is recorded as %+v, want the first operator's %+v", g, want)
	}
}

// A HOLD SENT AGAIN EXTENDS, at the same level: it is the one gesture whose
// repeat writes, because it states now how much longer the maintenance needs —
// so its length runs from the resend, and its who and why are the resend's.
func TestAHoldSentAgainExtends(t *testing.T) {
	t.Parallel()
	store := &countingMaps{objectMapStore: storedMap(t, "a", "b", "c")}
	c := controlOver(store, nil)
	first, err := c.Hold(t.Context(), time.Hour, "ops@example.com", "rack power work")
	if err != nil || !first.Landed || first.State.Hold == nil {
		t.Fatalf("the first hold = (%+v, %v)", first, err)
	}
	if want := gestureNow.Add(time.Hour); !first.State.Hold.Until.Equal(want) {
		t.Fatalf("the first hold ends %v, want %v", first.State.Hold.Until, want)
	}

	resent := gestureNow.Add(30 * time.Minute)
	c.now = func() time.Time { return resent }
	again, err := c.Hold(t.Context(), time.Hour, "oncall@example.com", "it overran")
	if err != nil || !again.Landed || again.State.Hold == nil {
		t.Fatalf("the hold sent again = (%+v, %v)", again, err)
	}
	if store.writes != 2 || again.Version == first.Version {
		t.Errorf("the hold sent again made %d write(s) in all and left the version at %d, "+
			"want a second write", store.writes, again.Version)
	}
	h := again.State.Hold
	if want := resent.Add(time.Hour); !h.Until.Equal(want) {
		t.Errorf("the hold sent again ends %v, want an hour after the resend, %v", h.Until, want)
	}
	if h.By != "oncall@example.com" || h.Reason != "it overran" {
		t.Errorf("the hold sent again is recorded as by %q for %q", h.By, h.Reason)
	}
}

// removedFrom writes a first map over nodes and runs the maintainer's ticks
// with gone absent until the map removes it, answering the store holding the
// result.
func removedFrom(t *testing.T, gone string, nodes ...string) *coordmemory.Fleet {
	t.Helper()
	fleet := storedMap(t, nodes...)
	rec, _, err := fleet.ObjectMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state := mustDecode(t, rec.Value)
	var live []upkeep.Presence
	for _, n := range nodes {
		if n != gone {
			live = append(live, upkeep.Presence{Node: n, Weight: 1})
		}
	}
	for range membership.OutTicks + 1 {
		state, _ = upkeep.Next(state, live, membership.Company{Epoch: 1, Replicas: fixtureCopies}, gestureNow)
	}
	if _, removed := state.Removed[gone]; !removed {
		t.Fatalf("%d ticks without %s did not remove it", membership.OutTicks+1, gone)
	}
	raw, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := fleet.UpdateObjectMap(t.Context(), raw, rec.Version); err != nil || !won {
		t.Fatalf("store the map with %s removed: %v", gone, err)
	}
	return fleet
}

func mustDecode(t *testing.T, raw []byte) objstore.MapState {
	t.Helper()
	s, err := objstore.DecodeMapState(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// AN OUT'S BALANCE IS LOGGED BY THE NODE THAT RAN IT: the gesture balances on
// the node serving it, so its own log line is the only one that can say
// whether the shares converged — and a hold, which places nothing, says
// nothing about a balance it never ran.
func TestAGestureLogsTheBalanceItRan(t *testing.T) {
	t.Parallel()
	fleet := storedMap(t, "a", "b", "c", "d")
	c := controlOver(fleet, nil)
	before, _, _, err := c.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.Out(t.Context(), "b", "ops@example.com", "replacing its disk")
	if err != nil || !out.Landed {
		t.Fatalf("Out = (%+v, %v)", out.Landed, err)
	}
	fields := gestureBalance(before, out.State)
	if len(fields) != 6 || fields[0] != "balance_rounds" || fields[4] != "balance_converged" {
		t.Fatalf("an out logs %v, want its balance's rounds, deviation and convergence", fields)
	}
	if got := fields[5]; got != out.State.Balance.Converged {
		t.Errorf("balance_converged = %v, the map says %v", got, out.State.Balance.Converged)
	}

	held, err := c.Hold(t.Context(), time.Hour, "ops@example.com", "rack work")
	if err != nil || !held.Landed {
		t.Fatalf("Hold = (%+v, %v)", held.Landed, err)
	}
	if fields := gestureBalance(out.State, held.State); fields != nil {
		t.Errorf("a hold logs %v, but it ran no balance", fields)
	}
}
