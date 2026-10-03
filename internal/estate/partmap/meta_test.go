package partmap

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// roundTrip is a lease's Meta as a peer reads it off the KV backend: through
// JSON, so an int comes back a float64 and a map a map[string]any.
func roundTrip(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// THE LEASE READS BACK AS IT WAS WRITTEN, as the twin hands it over and as the
// broker does: every field, whichever form a backend returns it in.
func TestTheEstateLeaseReadsBackAsItWasWritten(t *testing.T) {
	t.Parallel()
	want := Meta{
		Weight: 3, Labels: map[string]string{"zone": "a"}, Layout: layoutNo(1),
		MapGeneration: uuid.MustParse("00000000-0000-4000-8000-000000000009"), MapEpoch: 57,
		Partitions: map[string]PartitionState{"tracker.007": PartServing, "pages.001": PartCatchingUp},
		FreeBytes:  1 << 40, Healthy: yes(), Detail: "ok", Building: []string{"tracker.007"},
	}
	meta, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, form := range map[string]map[string]any{"as written": meta, "through JSON": roundTrip(t, meta)} {
		got, ok := MetaFromLease(form)
		if !ok {
			t.Fatalf("%s: the lease offers no share", name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: read back %+v, wrote %+v", name, got, want)
		}
	}
}

// ABSENT IS NOT ZERO: a lease that leaves out its health or its layout reads
// back WITHOUT them — not as a healthy store, not as layout 0 — and the map
// counts such a node as one that has not said it can hold partitions.
func TestAnUnsaidHealthOrLayoutReadsAsUnsaid(t *testing.T) {
	t.Parallel()
	got, ok := MetaFromLease(roundTrip(t, map[string]any{"weight": 1}))
	if !ok {
		t.Fatal("a lease with a weight offers no share")
	}
	if got.Healthy != nil || got.Layout != nil {
		t.Fatalf("a lease that said nothing reads health %v, layout %v", got.Healthy, got.Layout)
	}
	p := Presence{Node: "data-00", Meta: got}.membership(1)
	if !p.Unhealthy || p.Detail == "" {
		t.Fatalf("a node that did not say its store is healthy counts as %+v", p)
	}
}

// A LEASE FAILS SOFT, FIELD BY FIELD: a peer's malformed report must not take
// a working member out of the map, so one unreadable field is absent and the
// rest still read — except the weight, without which the lease offers
// nothing, and a fraction is not a weight.
func TestAnEstateLeaseFailsSoftFieldByField(t *testing.T) {
	t.Parallel()
	got, ok := MetaFromLease(map[string]any{
		"weight": 2, "layout": "one", "healthy": "yes", "map_epoch": -1,
		"map_generation": "not a uuid", "labels": []any{"zone"},
		"partitions": map[string]any{"tracker.007": "serving", "tracker.7": "serving", "nonsense": "x"},
	})
	if !ok || got.Weight != 2 {
		t.Fatalf("a lease with a readable weight offers %v (%v)", got.Weight, ok)
	}
	if got.Layout != nil || got.Healthy != nil || got.MapEpoch != 0 ||
		got.MapGeneration != uuid.Nil || got.Labels != nil {
		t.Fatalf("an unreadable field read as a value: %+v", got)
	}
	if want := map[string]PartitionState{"tracker.007": PartServing}; !reflect.DeepEqual(got.Partitions, want) {
		t.Fatalf("partitions read as %v, want only the one named as a layout names it", got.Partitions)
	}
	for _, weight := range []any{nil, 0, -1, 1.5, "1"} {
		if _, ok := MetaFromLease(map[string]any{"weight": weight, "healthy": true}); ok {
			t.Errorf("a lease of weight %v offers a share", weight)
		}
	}
}

// A LEASE THAT DOES NOT SAY WHAT THE MAP DECIDES BY IS NOT WRITTEN: its weight,
// its layout, its health and what it holds. A writer that does not know its
// health says unhealthy and why; every writer knows what files it holds, and
// one holding none says so with an empty set. Nor is a lease written that a
// reader would read as another claim: a partition named as no layout names it
// reads as one not held, and a state this build does not know is one no
// reader of this build can act on.
func TestAnIncompleteEstateLeaseIsNotWritten(t *testing.T) {
	t.Parallel()
	holds := func(id string, s PartitionState) map[string]PartitionState {
		return map[string]PartitionState{id: s}
	}
	nothing := map[string]PartitionState{}
	for name, m := range map[string]Meta{
		"no weight":        {Layout: layoutNo(1), Healthy: yes(), Partitions: nothing},
		"too heavy":        {Weight: 65, Layout: layoutNo(1), Healthy: yes(), Partitions: nothing},
		"no layout":        {Weight: 1, Healthy: yes(), Partitions: nothing},
		"health left":      {Weight: 1, Layout: layoutNo(1), Partitions: nothing},
		"holdings left":    {Weight: 1, Layout: layoutNo(1), Healthy: yes()},
		"a malformed name": {Weight: 1, Layout: layoutNo(1), Healthy: yes(), Partitions: holds("tracker.7", PartServing)},
		"an unknown state": {Weight: 1, Layout: layoutNo(1), Healthy: yes(), Partitions: holds("tracker.007", "verifying")},
	} {
		if _, err := m.Encode(); !errors.Is(err, ErrMetaIncomplete) {
			t.Errorf("%s: Encode = %v, want ErrMetaIncomplete", name, err)
		}
	}
	meta, err := Meta{Weight: 1, Layout: layoutNo(1), Healthy: yes(), Partitions: nothing}.Encode()
	if err != nil {
		t.Fatalf("a lease holding nothing: %v", err)
	}
	got, _ := MetaFromLease(roundTrip(t, meta))
	if got.Partitions == nil || len(got.Partitions) != 0 {
		t.Fatalf("a lease holding nothing reads back holding %v", got.Partitions)
	}
}

// ONLY AN ESTATE LEASE IS A PRESENCE: a node's presence and its objects lease
// name the same node and say nothing about what it holds of the estate.
func TestOnlyAnEstateLeaseIsAPresence(t *testing.T) {
	t.Parallel()
	meta, err := Meta{Weight: 1, Layout: layoutNo(1), Healthy: yes(),
		Partitions: map[string]PartitionState{}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := PresenceOf(coord.Lease{Resource: coord.EstateResource("data-00"), Meta: meta}); !ok || p.Node != "data-00" {
		t.Fatalf("an estate lease read as (%+v, %v)", p, ok)
	}
	for _, resource := range []string{coord.NodeResource("data-00"), coord.SeatResource("data-00")} {
		if _, ok := PresenceOf(coord.Lease{Resource: resource, Meta: meta}); ok {
			t.Errorf("%s read as an estate presence", resource)
		}
	}
	if _, ok := PresenceOf(coord.Lease{Resource: coord.EstateResource("data-00"),
		Meta: map[string]any{"healthy": true}}); ok {
		t.Error("an estate lease offering no share read as a presence")
	}
}

// PartitionState is an enum: a state off the wire this build does not know is
// a value, never a panic, and not one it may act on.
func TestAPartitionStateIsAnEnum(t *testing.T) {
	t.Parallel()
	for _, s := range PartitionStates {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	for _, s := range []PartitionState{"", "verifying", "Serving"} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
	for _, s := range []HolderState{Joining, Serving, Leaving} {
		if !s.Valid() {
			t.Errorf("holder state %q is not valid", s)
		}
	}
	if HolderState("resting").Valid() {
		t.Error("an unknown holder state is valid")
	}
}
