package queries_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
)

// objectMaps is a stored placement map a test sets directly.
type objectMaps struct {
	raw   []byte
	found bool
	err   error
}

func (m objectMaps) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	return coord.ObjectMapRecord{Value: m.raw, Version: 7}, m.found, m.err
}

// placedFleet is a map of five members spread across three zones, two copies of
// each chunk — fewer than the three placeable members, so a weight shows in the
// shares: data-a at twice the weight, data-b gone for twelve ticks and back for three, data-c
// up with a failed store, data-d taken out for a decommission, and data-f
// removed for absence and back on probation for twelve ticks — plus data-e,
// removed for absence, remembered, and not seen since.
func placedFleet(t *testing.T) objstore.MapState {
	t.Helper()
	since := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	member := func(node string, weight int, zone string) placement.Member {
		return placement.Member{Node: node, Weight: weight,
			Share: placement.DefaultShare(weight), Domain: zone}
	}
	out := member("data-d", 1, "eu-3")
	out.Out = true
	probation := member("data-f", 1, "eu-2")
	probation.Probation = true
	return objstore.MapState{
		Map: objplacement.Map{
			Generation: uuid.MustParse("5b0c1f7e-9c1d-4f5e-8a3b-2d7c6e1f0a9b"),
			Epoch:      7, Replicas: 2, PGBits: objplacement.MinPGBits,
			FailureDomain: "zone",
			Members: []placement.Member{
				member("data-a", 2, "eu-1"), member("data-b", 1, "eu-2"),
				member("data-c", 1, "eu-3"), out, probation,
			},
		},
		Absence: map[string]membership.Absence{"data-b": {Ticks: 12, Present: 3,
			Since: since, Reason: membership.ReasonAbsent}},
		TakenOut: map[string]membership.Gesture{"data-d": {By: "alice",
			Reason: "decommission", At: since}},
		Removed: map[string]membership.Removal{
			"data-e": {Gone: 6, At: since, Reason: membership.ReasonUnhealthy,
				Detail: "probe: read-only filesystem"},
			"data-f": {Present: 12, At: since.Add(-time.Hour), Reason: membership.ReasonAbsent},
		},
	}
}

// objectsLeases claims the objects leases the fleet in [placedFleet] holds:
// data-b holds none, data-c's store has failed, and data-a reports all it can.
func objectsLeases(t *testing.T) *coordmemory.Backend {
	t.Helper()
	backend := coordmemory.New()
	strays, none := 14, 0
	for node, meta := range map[string]objstore.ObjectsMeta{
		"data-a": {Weight: 2, Labels: map[string]string{"zone": "eu-1"},
			Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 41.5},
			Repair: &objstore.ObjectsRepair{Epoch: 7, Completed: true, Placed: 900,
				Held: 900, At: time.Date(2026, 9, 1, 12, 4, 0, 0, time.UTC)},
			Scrub: &objstore.ObjectsScrub{Progress: 0.25, Verified: 300, Rotten: 1,
				Unreadable: 2, Error: "walk slots [16384, 16640): permission denied"},
			Strays: &none},
		"data-c": {Weight: 1, Health: &objstore.ObjectsHealth{State: "failed",
			Detail: "probe: input/output error"}},
		"data-d": {Weight: 1, Strays: &strays},
		"data-f": {Weight: 1, Labels: map[string]string{"zone": "eu-2"},
			Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 20}},
	} {
		if _, err := backend.TryAcquire(t.Context(), coord.ObjectsResource(node),
			coord.AcquireOptions{Owner: node + ":1", TTL: time.Minute, Meta: meta.Encode(),
				Ungated: true}); err != nil {
			t.Fatal(err)
		}
	}
	return backend
}

// fleetObjects asks the fleet question and returns its objects block as a
// client reads it.
func fleetObjects(t *testing.T, backend coord.Backend, maps objectMaps) map[string]any {
	t.Helper()
	body := asMap(t, answer(t, queries.Sources{Coord: backend, NodeID: "data-a",
		Objects: maps}, "fleet", nil))
	objects, _ := body["objects"].(map[string]any)
	if objects == nil {
		t.Fatalf("no objects in %v", body)
	}
	return objects
}

func encodeState(t *testing.T, state objstore.MapState) []byte {
	t.Helper()
	raw, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// THE FLEET VIEW SHOWS WHERE THE COMPANY'S FILES ARE PLACED — the map's own
// facts, each member's measured share of the copies, and what each member's
// objects lease says about it — so an operator can read off it whether the
// next data node may be stopped.
func TestTheFleetShowsWhereTheCompanysFilesArePlaced(t *testing.T) {
	t.Parallel()
	state := placedFleet(t)
	got := fleetObjects(t, objectsLeases(t),
		objectMaps{raw: encodeState(t, state), found: true})

	for key, want := range map[string]any{
		"state": "placed", "generation": state.Map.Generation.String(),
		"epoch": float64(7), "replicas": float64(2), "copies": float64(2),
		"pgs": float64(256), "failure_domain": "zone",
		"distinct_domains": float64(3), "domain_limited": false,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	if _, held := got["hold"]; held {
		t.Errorf("a map nobody held renders a hold: %v", got["hold"])
	}

	members, _ := got["members"].([]any)
	if len(members) != 5 {
		t.Fatalf("members = %v", got["members"])
	}
	row := func(i int) map[string]any {
		r, _ := members[i].(map[string]any)
		return r
	}

	// THE SHARES ARE THE COPIES THE STORED MAP PLACES, summing to the
	// whole, and a member taken out or on probation holds none of them.
	sum := 0.0
	for i := range members {
		share, _ := row(i)["share_percent"].(float64)
		sum += share
	}
	if math.Abs(sum-100) > 1e-9 {
		t.Errorf("the shares sum to %v, want 100", sum)
	}
	if row(3)["share_percent"] != float64(0) {
		t.Errorf("an out member holds %v of the copies", row(3)["share_percent"])
	}
	if row(4)["share_percent"] != float64(0) {
		t.Errorf("a member on probation holds %v of the copies", row(4)["share_percent"])
	}
	if a, b := row(0)["share_percent"].(float64), row(1)["share_percent"].(float64); a <= b {
		t.Errorf("data-a at twice data-b's weight holds %v%% against %v%%", a, b)
	}

	// A MEMBER TAKEN OUT SAYS WHO TOOK IT OUT AND WHY, beside the flag.
	if d := row(3); d["out"] != true || d["out_by"] != "alice" || d["out_reason"] != "decommission" {
		t.Errorf("the out member reads %v", d)
	}

	// AN ABSENCE IS COUNTED IN TICKS, with what removes it and what clears
	// it beside the count — a member back for three ticks keeps its twelve.
	absence, _ := row(1)["absence"].(map[string]any)
	for key, want := range map[string]any{
		"ticks": float64(12), "out_after_ticks": float64(membership.OutTicks),
		"present": float64(3), "clear_after_ticks": float64(membership.StableTicks),
		"reason": "absent",
	} {
		if absence[key] != want {
			t.Errorf("data-b's absence %s = %v, want %v", key, absence[key], want)
		}
	}

	// LIVE IS THE LEASE, and a member that holds none reports nothing it
	// could only have said on one — absent, never zero.
	if row(1)["live"] != false {
		t.Errorf("data-b holds no objects lease and reads live")
	}
	for _, key := range []string{"health", "repair", "scrub", "strays"} {
		if _, said := row(1)[key]; said {
			t.Errorf("data-b holds no lease and reports %s %v", key, row(1)[key])
		}
		if _, said := row(0)[key]; !said {
			t.Errorf("data-a reported %s and it was dropped", key)
		}
	}
	if repair, _ := row(0)["repair"].(map[string]any); repair["pending"] != float64(0) ||
		repair["completed"] != true || repair["epoch"] != float64(7) {
		t.Errorf("data-a's repair reads %v", repair)
	}
	// A report the member did not make is absent even while its lease is
	// live: data-d said nothing about its health.
	if _, said := row(3)["health"]; said || row(3)["strays"] != float64(14) {
		t.Errorf("data-d reads health %v, strays %v", row(3)["health"], row(3)["strays"])
	}

	// DEGRADED IS EVERY GROUP WITH A COPY ON A MEMBER THAT IS GONE OR
	// FAILED — counted here the long way, off the ranking rather than the
	// layout the answer used, so the two paths have to agree.
	down := map[string]bool{"data-b": true, "data-c": true}
	want := 0
	for pg := range state.Map.Groups() {
		for _, node := range state.Map.Ranked(pg)[:state.Map.Size()] {
			if down[node] {
				want++
				break
			}
		}
	}
	if want == 0 || got["degraded_groups"] != float64(want) {
		t.Errorf("degraded_groups = %v, want %d", got["degraded_groups"], want)
	}

	// A SCRUB SAYS WHAT IT COULD NOT READ AND WHAT STOPPED IT, beside what
	// it verified: an unreadable chunk is stepped past, so the count is
	// the only place a disk failing chunk by chunk shows.
	if scrub, _ := row(0)["scrub"].(map[string]any); scrub["unreadable"] != float64(2) ||
		scrub["rotten"] != float64(1) || scrub["error"] == nil {
		t.Errorf("data-a's scrub reads %v", scrub)
	}

	// A MEMBER ON PROBATION IS A MEMBER — the reader and the repair find
	// the copies it held when it went through the map's ranking — with how
	// close it is to being placed on again and the absence that removed it.
	probation, _ := row(4)["probation"].(map[string]any)
	for key, want := range map[string]any{
		"present": float64(12), "placed_after_ticks": float64(membership.StableTicks),
		"reason": "absent",
	} {
		if probation[key] != want {
			t.Errorf("data-f's probation %s = %v, want %v", key, probation[key], want)
		}
	}
	if row(4)["live"] != true || row(4)["out"] != false {
		t.Errorf("data-f reads %v", row(4))
	}
	// AND ONLY THERE: no other member carries one, never a zero one.
	for i := range 4 {
		if _, said := row(i)["probation"]; said {
			t.Errorf("%v is not on probation and renders one", row(i)["node"])
		}
	}

	// A NODE REMOVED, REMEMBERED AND NOT SEEN SINCE is listed apart, with
	// how close it is to being forgotten — and a node on probation is not
	// listed again beside its member row.
	removed, _ := got["removed"].([]any)
	if len(removed) != 1 {
		t.Fatalf("removed = %v", got["removed"])
	}
	if e, _ := removed[0].(map[string]any); e["node"] != "data-e" || e["gone"] != float64(6) ||
		e["forget_after_ticks"] != float64(membership.OutTicks) ||
		e["placed_after_ticks"] != float64(membership.StableTicks) || e["reason"] != "unhealthy" {
		t.Errorf("the removed node reads %v", e)
	}
}

// A HOLD IS SHOWN WHILE IT HOLDS, and not a moment after: an expired hold the
// maintainer has not cleared yet removes members again on its next tick, so
// rendering it as holding would tell an operator a gone member is safe.
func TestTheFleetShowsAHoldOnlyWhileItHolds(t *testing.T) {
	t.Parallel()
	state := placedFleet(t)
	state.Hold = &membership.Hold{Until: pinned.Add(time.Hour), By: "alice",
		Reason: "kernel upgrade", At: pinned.Add(-time.Minute)}
	got := fleetObjects(t, objectsLeases(t), objectMaps{raw: encodeState(t, state), found: true})
	hold, _ := got["hold"].(map[string]any)
	if hold["by"] != "alice" || hold["reason"] != "kernel upgrade" ||
		hold["until"] != pinned.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("hold = %v", got["hold"])
	}

	state.Hold.Until = pinned.Add(-time.Second)
	got = fleetObjects(t, objectsLeases(t), objectMaps{raw: encodeState(t, state), found: true})
	if _, held := got["hold"]; held {
		t.Errorf("an expired hold still renders: %v", got["hold"])
	}
}

// HOW EVENLY THE MAP SPREADS ITS COPIES IS ON THE FLEET VIEW, as the map
// recorded it. A balance that cannot reach its tolerance still writes the best
// map it measured, so a fleet whose weights are only intents — a crowded
// failure domain, members too light to be counted in whole copies — said so in
// a log line on whichever node wrote the change, and nowhere an operator reads.
// Absent for a map nothing has measured, never a zero that reads as perfectly
// even; and rounds zero — a split's placement measured and left as it was — is
// written, since it is the reading that says no balance ran.
func TestTheFleetShowsHowEvenlyTheMapSpreadsItsCopies(t *testing.T) {
	t.Parallel()
	state := placedFleet(t)
	got := fleetObjects(t, objectsLeases(t), objectMaps{raw: encodeState(t, state), found: true})
	if b, present := got["balance"]; present {
		t.Errorf("a map nothing has measured renders a balance: %v", b)
	}

	for name, tc := range map[string]struct {
		balance objstore.Balance
		want    map[string]any
	}{
		"a balance that ran out of rounds": {
			balance: objstore.Balance{Epoch: 7, BalanceReport: placement.BalanceReport{
				Rounds: placement.DefaultMaxRounds, Deviation: 0.051}},
			want: map[string]any{"epoch": 7.0, "deviation_percent": 5.1,
				"tolerance_percent": 2.0, "converged": false,
				"rounds": float64(placement.DefaultMaxRounds)},
		},
		"a split measured and left as it was": {
			balance: objstore.Balance{Epoch: 6, BalanceReport: placement.BalanceReport{
				Deviation: 0.012, Converged: true}},
			want: map[string]any{"epoch": 6.0, "deviation_percent": 1.2,
				"tolerance_percent": 2.0, "converged": true, "rounds": 0.0},
		},
	} {
		state.Balance = tc.balance
		got := fleetObjects(t, objectsLeases(t), objectMaps{raw: encodeState(t, state), found: true})
		b, _ := got["balance"].(map[string]any)
		if len(b) != len(tc.want) {
			t.Errorf("%s: balance = %v, want %v", name, b, tc.want)
			continue
		}
		for key, want := range tc.want {
			if f, ok := want.(float64); ok {
				if g, _ := b[key].(float64); math.Abs(g-f) > 1e-9 {
					t.Errorf("%s: %s = %v, want %v", name, key, b[key], want)
				}
				continue
			}
			if b[key] != want {
				t.Errorf("%s: %s = %v, want %v", name, key, b[key], want)
			}
		}
	}
}

// A MEMBER WHOSE LEASE OFFERS NO READABLE SHARE READS AS NOT LIVE, because
// that is how the maintainer counts it: a view that called it live would show
// a member healthy on the screen while the map was counting it gone.
func TestAMemberWhoseLeaseOffersNoShareIsNotLive(t *testing.T) {
	t.Parallel()
	state := placedFleet(t)
	backend := objectsLeases(t)
	if _, err := backend.TryAcquire(t.Context(), coord.ObjectsResource("data-b"),
		coord.AcquireOptions{Owner: "data-b:1", TTL: time.Minute, Ungated: true,
			Meta: map[string]any{"weight": 0.5}}); err != nil {
		t.Fatal(err)
	}
	got := fleetObjects(t, backend, objectMaps{raw: encodeState(t, state), found: true})
	members, _ := got["members"].([]any)
	if b, _ := members[1].(map[string]any); b["live"] != false {
		t.Errorf("a lease offering half a share reads live: %v", b)
	}
}

// THE FOUR STATES ARE NAMED APART, never folded into an empty member list — a
// store that would not answer, a fleet with no map yet, a map a newer build
// wrote — because each sends an operator somewhere different. And none of them
// carries a placed map's counts, whose zeros would read as readings.
func TestTheFleetNamesEachStateOfTheMapApart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		maps objectMaps
		want string
	}{
		"a store that would not answer": {objectMaps{err: errors.New("down")}, "unavailable"},
		"a fleet with no map yet":       {objectMaps{}, "no_map"},
		"a map a newer build wrote": {objectMaps{raw: []byte("not a map"), found: true},
			"unreadable"},
	} {
		got := fleetObjects(t, coordmemory.New(), tc.maps)
		if got["state"] != tc.want || len(got) != 1 {
			t.Errorf("%s: objects = %v, want only state %q", name, got, tc.want)
		}
		if !queries.ObjectMapState(tc.want).Valid() {
			t.Errorf("%s: %q is not a state this build declares", name, tc.want)
		}
	}
	if queries.ObjectMapState("gone").Valid() {
		t.Error("an undeclared state reads valid")
	}
}

// AND NO NODE ROW CARRIES AN OBJECT SHARE: it left presence for the object
// store's own lease, and a row reading it off presence would report every
// data node as offering nothing.
func TestNoNodeRowCarriesAnObjectShare(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("data-a"), coord.AcquireOptions{
		Owner: "data-a:1", TTL: time.Minute,
		Meta: map[string]any{"roles": []any{"data"}, "object_weight": 4},
	}); err != nil {
		t.Fatal(err)
	}
	body := asMap(t, answer(t, queries.Sources{Coord: backend, NodeID: "data-a"}, "fleet", nil))
	nodes, _ := body["nodes"].([]any)
	if node, _ := nodes[0].(map[string]any); node["object_weight"] != nil {
		t.Errorf("a node row still carries object_weight %v", node["object_weight"])
	}
	if _, present := body["objects"]; present {
		t.Error("a surface with no map reader reported a placement anyway")
	}
}
