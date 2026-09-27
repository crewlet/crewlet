package objstore_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// jsonTrip is what the embedded-NATS lease store does to a Meta: numbers come
// back float64, nested maps map[string]any.
func jsonTrip(t *testing.T, meta map[string]any) map[string]any {
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

// THE LEASE READS BACK WHAT WAS WRITTEN, from either backend's shape. The
// in-memory twin hands back the map it was given and the broker a JSON round
// trip of it, and a reader that knew one shape would read every peer on the
// other as reporting nothing.
func TestTheObjectsLeaseRoundTripsThroughEitherBackend(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 27, 10, 0, 0, 123, time.UTC)
	strays := 4
	want := objstore.ObjectsMeta{
		Weight: 3,
		Labels: map[string]string{"zone": "a", "rack": "r7"},
		Health: &objstore.ObjectsHealth{State: "nearfull", Detail: "the volume is 87% used",
			UsedPercent: 87.25},
		Repair: &objstore.ObjectsRepair{Epoch: 42, Completed: true, Placed: 900, Held: 890,
			Pending: 10, Unreachable: 2, Missing: 1, At: at},
		Scrub: &objstore.ObjectsScrub{CycleStarted: at.Add(-time.Hour), Progress: 0.25,
			Verified: 1200, Rotten: 3, Unreadable: 2,
			Error: "objstore/upkeep: walk slots [0, 256): permission denied"},
		Strays: &strays,
	}
	for name, meta := range map[string]map[string]any{
		"as written":              want.Encode(),
		"after a JSON round trip": jsonTrip(t, want.Encode()),
	} {
		got, ok := objstore.ObjectsFromMeta(meta)
		if !ok {
			t.Fatalf("%s: a lease with a weight read as offering nothing", name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// ABSENT IS NOT ZERO. A member that has not run a repair has no pending count,
// and one whose scrub has not started has found nothing rotten only in the
// sense that it has not looked — so an unreported sub-report reads back nil,
// never as a zero a screen would render as "all held".
func TestAnUnreportedPartOfTheLeaseReadsAsAbsent(t *testing.T) {
	t.Parallel()
	got, ok := objstore.ObjectsFromMeta(jsonTrip(t, objstore.ObjectsMeta{Weight: 1}.Encode()))
	if !ok {
		t.Fatal("a weight-1 lease read as offering nothing")
	}
	if got.Health != nil || got.Repair != nil || got.Scrub != nil || got.Strays != nil ||
		got.Labels != nil {
		t.Errorf("a lease reporting only its weight read back %+v", got)
	}
}

// A LEASE WITH NO READABLE WEIGHT OFFERS NOTHING. A node read as holding
// objects is one writers send chunks to and count toward a quorum, so an
// unreadable weight is no share rather than the default one.
func TestAnUnreadableWeightOffersNoShare(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]any{
		"absent":   nil,
		"zero":     0,
		"negative": -2,
		"fraction": 1.5,
		"string":   "3",
	} {
		meta := map[string]any{}
		if raw != nil {
			meta["weight"] = raw
		}
		if got, ok := objstore.ObjectsFromMeta(meta); ok {
			t.Errorf("%s weight: read as offering %d", name, got.Weight)
		}
	}
}

// A PEER'S UNKNOWN HEALTH STATE IS CARRIED, not dropped: a newer build may
// report a state this one does not know, and what a reader must not do is
// turn it into `failed` — or into nothing, which would hide it from the screen.
func TestAnUnknownHealthStateIsCarriedAsWritten(t *testing.T) {
	t.Parallel()
	meta := map[string]any{"weight": 1, "health": map[string]any{"state": "degraded-v9"}}
	got, ok := objstore.ObjectsFromMeta(meta)
	if !ok || got.Health == nil || got.Health.State != "degraded-v9" {
		t.Fatalf("read %+v", got.Health)
	}
}
